package identity_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oxkrypton/Halon/internal/authz"
	"github.com/oxkrypton/Halon/internal/identity"
)

// These tests need a real PostgreSQL instance. Point HALON_TEST_DSN at a
// dedicated database: the migrations are applied and temporary users are
// created there. By default both tests skip.
func openIntegrationStore(t *testing.T) *identity.Store {
	t.Helper()
	dsn := os.Getenv("HALON_TEST_DSN")
	if dsn == "" {
		t.Skip("set HALON_TEST_DSN to run identity integration tests")
	}
	store, err := identity.OpenStore(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open integration store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(); err != nil {
		t.Fatalf("apply identity migrations: %v", err)
	}
	return store
}

func createIntegrationUser(t *testing.T, store *identity.Store, role authz.Role) *identity.User {
	t.Helper()
	plain := "integration-test-phrase-long-enough"
	hash, err := identity.HashPassword(plain)
	if err != nil {
		t.Fatalf("hash test password: %v", err)
	}
	user := &identity.User{
		Username:     fmt.Sprintf("itest-%d", time.Now().UnixNano()),
		DisplayName:  "Integration Test",
		PasswordHash: hash,
		Role:         role,
	}
	if err := store.CreateUser(context.Background(), user); err != nil {
		t.Fatalf("create test user: %v", err)
	}
	t.Cleanup(func() {
		sqlDB, err := store.SQLDB()
		if err != nil {
			return
		}
		_, _ = sqlDB.Exec(`DELETE FROM auth_users WHERE id = $1`, user.ID)
	})
	return user
}

func TestIntegrationConcurrentRefreshHasSingleWinner(t *testing.T) {
	store := openIntegrationStore(t)
	user := createIntegrationUser(t, store, authz.RoleReadOnly)
	ctx := context.Background()

	rotation, err := store.IssueRefreshToken(ctx, user.ID, 15*time.Minute, 24*time.Hour, "", "")
	if err != nil {
		t.Fatalf("issue refresh token: %v", err)
	}

	const contenders = 8
	var winners atomic.Int64
	var wg sync.WaitGroup
	errorsCh := make(chan error, contenders)
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.RotateRefreshToken(ctx, rotation.Token, 15*time.Minute, 24*time.Hour, "", "")
			if err == nil {
				winners.Add(1)
				return
			}
			errorsCh <- err
		}()
	}
	wg.Wait()
	close(errorsCh)

	if got := winners.Load(); got != 1 {
		t.Fatalf("successful rotations = %d, want exactly 1", got)
	}
	for err := range errorsCh {
		if !errors.Is(err, identity.ErrInvalidRefreshToken) && !errors.Is(err, identity.ErrRefreshTokenReuse) {
			t.Fatalf("unexpected loser error: %v", err)
		}
	}
}

func TestIntegrationCountEnabledFullUsers(t *testing.T) {
	store := openIntegrationStore(t)
	ctx := context.Background()

	baseline, err := store.CountEnabledFullUsers(ctx)
	if err != nil {
		t.Fatalf("baseline count: %v", err)
	}
	user := createIntegrationUser(t, store, authz.RoleFull)

	afterCreate, err := store.CountEnabledFullUsers(ctx)
	if err != nil || afterCreate != baseline+1 {
		t.Fatalf("count after creating full user = %d, %v; want %d", afterCreate, err, baseline+1)
	}
	if err := store.SetUserDisabled(ctx, user.Username, true); err != nil {
		t.Fatalf("disable full user: %v", err)
	}
	afterDisable, err := store.CountEnabledFullUsers(ctx)
	if err != nil || afterDisable != baseline {
		t.Fatalf("count after disabling full user = %d, %v; want %d", afterDisable, err, baseline)
	}
}
