package identity_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/oxkrypton/Halon/internal/authz"
	"github.com/oxkrypton/Halon/internal/identity"
)

// memoryStore implements identity.AuthStore for service-level tests. Token
// plaintext is kept only in this test double; the real Store persists digests.
type memoryStore struct {
	mu     sync.Mutex
	users  map[string]*identity.User
	byID   map[string]*identity.User
	tokens map[string]*memoryToken
	events []identity.AuthEvent
	nextID int
}

type memoryToken struct {
	userID          string
	familyID        string
	familyStartedAt time.Time
	expiresAt       time.Time
	usedAt          *time.Time
	revokedAt       *time.Time
}

func newMemoryStore() *memoryStore {
	return &memoryStore{
		users:  make(map[string]*identity.User),
		byID:   make(map[string]*identity.User),
		tokens: make(map[string]*memoryToken),
	}
}

func (m *memoryStore) addUser(t *testing.T, role authz.Role, mustChange bool) *identity.User {
	t.Helper()
	return m.addNamedUser(t, "alice", role, mustChange)
}

func (m *memoryStore) addNamedUser(t *testing.T, username string, role authz.Role, mustChange bool) *identity.User {
	t.Helper()
	password := testPassword
	hash, err := identity.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	m.nextID++
	user := &identity.User{
		ID:                 fmt.Sprintf("user-%d", m.nextID),
		Username:           username,
		PasswordHash:       hash,
		Role:               role,
		MustChangePassword: mustChange,
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.users[username] = user
	m.byID[user.ID] = user
	return user
}

func (m *memoryStore) GetUserByUsername(_ context.Context, username string) (*identity.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	user, ok := m.users[username]
	if !ok {
		return nil, identity.ErrNotFound
	}
	return user, nil
}

func (m *memoryStore) GetUserByID(_ context.Context, id string) (*identity.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	user, ok := m.byID[id]
	if !ok {
		return nil, identity.ErrNotFound
	}
	return user, nil
}

func (m *memoryStore) ListUsers(_ context.Context, offset, limit int) ([]identity.User, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	all := make([]identity.User, 0, len(m.users))
	for _, user := range m.users {
		all = append(all, *user)
	}
	if offset > len(all) {
		offset = len(all)
	}
	end := offset + limit
	if end > len(all) {
		end = len(all)
	}
	return all[offset:end], int64(len(all)), nil
}

func (m *memoryStore) CreateUser(_ context.Context, user *identity.User) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, taken := m.users[user.Username]; taken {
		return identity.ErrUsernameTaken
	}
	if user.ID == "" {
		m.nextID++
		user.ID = fmt.Sprintf("user-%d", m.nextID)
	}
	m.users[user.Username] = user
	m.byID[user.ID] = user
	return nil
}

func (m *memoryStore) UpdateUserGuarded(_ context.Context, targetID, actorID string, update identity.UserUpdate) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	target, ok := m.byID[targetID]
	if !ok {
		return identity.ErrNotFound
	}
	enabledFull := 0
	for _, user := range m.users {
		if user.Role == authz.RoleFull && !user.Disabled {
			enabledFull++
		}
	}
	if targetID == actorID && (update.Role != nil || (update.Disabled != nil && *update.Disabled)) {
		return identity.ErrCannotModifySelf
	}
	losesFull := target.Role == authz.RoleFull && !target.Disabled &&
		((update.Role != nil && *update.Role != authz.RoleFull) || (update.Disabled != nil && *update.Disabled))
	if losesFull && enabledFull <= 1 {
		return identity.ErrLastAdmin
	}
	if update.Role != nil {
		target.Role = *update.Role
	}
	if update.Disabled != nil {
		target.Disabled = *update.Disabled
		if *update.Disabled {
			now := time.Now().UTC()
			for _, token := range m.tokens {
				if token.userID == targetID && token.revokedAt == nil {
					token.revokedAt = &now
				}
			}
		}
	}
	if update.DisplayName != nil {
		target.DisplayName = *update.DisplayName
	}
	return nil
}

func (m *memoryStore) RevokeAllUserRefreshTokens(_ context.Context, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, token := range m.tokens {
		if token.userID == userID {
			now := time.Now().UTC()
			token.revokedAt = &now
		}
	}
	return nil
}

func (m *memoryStore) SetPasswordHashOnly(_ context.Context, username, hash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.users[username].PasswordHash = hash
	return nil
}

func (m *memoryStore) SetPasswordHash(_ context.Context, username, hash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	user := m.users[username]
	user.PasswordHash = hash
	user.FailedLoginCount = 0
	user.LockedUntil = nil
	return nil
}

func (m *memoryStore) SetMustChangePassword(_ context.Context, username string, value bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.users[username].MustChangePassword = value
	return nil
}

func (m *memoryStore) RecordFailedLogin(_ context.Context, username string, maxFailures int, lockDuration time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	user, ok := m.users[username]
	if !ok {
		return false, identity.ErrNotFound
	}
	now := time.Now().UTC()
	if user.LockedUntil != nil && !user.LockedUntil.After(now) {
		user.FailedLoginCount = 1
	} else {
		user.FailedLoginCount++
	}
	if user.FailedLoginCount >= maxFailures {
		until := now.Add(lockDuration)
		user.LockedUntil = &until
		return true, nil
	}
	user.LockedUntil = nil
	return false, nil
}

func (m *memoryStore) RecordSuccessfulLogin(_ context.Context, username string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	user := m.users[username]
	user.FailedLoginCount = 0
	user.LockedUntil = nil
	user.LastLoginAt = &at
	return nil
}

func (m *memoryStore) RecordAuthEvent(_ context.Context, event identity.AuthEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, event)
	return nil
}

func (m *memoryStore) IssueRefreshToken(_ context.Context, userID string, ttl, maxLifetime time.Duration, _, _ string) (identity.RefreshRotation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	familyID, err := newTestUUID()
	if err != nil {
		return identity.RefreshRotation{}, err
	}
	return m.insertToken(userID, familyID, time.Now().UTC(), ttl, maxLifetime), nil
}

func (m *memoryStore) RotateRefreshToken(_ context.Context, token string, ttl, maxLifetime time.Duration, _, _ string) (identity.RefreshRotation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.tokens[token]
	if !ok || current.revokedAt != nil || current.expiresAt.Before(time.Now().UTC()) {
		return identity.RefreshRotation{}, identity.ErrInvalidRefreshToken
	}
	if current.usedAt != nil {
		if time.Since(*current.usedAt) <= identity.RefreshReuseGracePeriod {
			return identity.RefreshRotation{}, identity.ErrInvalidRefreshToken
		}
		m.revokeFamily(current.familyID)
		return identity.RefreshRotation{}, identity.ErrRefreshTokenReuse
	}
	now := time.Now().UTC()
	current.usedAt = &now
	return m.insertToken(current.userID, current.familyID, current.familyStartedAt, ttl, maxLifetime), nil
}

func (m *memoryStore) insertToken(userID, familyID string, startedAt time.Time, ttl, maxLifetime time.Duration) identity.RefreshRotation {
	token, err := identity.GenerateRefreshToken()
	if err != nil {
		panic(err)
	}
	expiresAt := time.Now().UTC().Add(ttl)
	if deadline := startedAt.Add(maxLifetime); deadline.Before(expiresAt) {
		expiresAt = deadline
	}
	m.tokens[token] = &memoryToken{
		userID: userID, familyID: familyID,
		familyStartedAt: startedAt, expiresAt: expiresAt,
	}
	return identity.RefreshRotation{
		Token: token, UserID: userID, FamilyID: familyID,
		FamilyStartedAt: startedAt, ExpiresAt: expiresAt,
	}
}

func (m *memoryStore) revokeFamily(familyID string) {
	for _, token := range m.tokens {
		if token.familyID == familyID {
			now := time.Now().UTC()
			token.revokedAt = &now
		}
	}
}

func (m *memoryStore) RevokeTokenFamily(_ context.Context, familyID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.revokeFamily(familyID)
	return nil
}

func (m *memoryStore) RevokeRefreshTokenFamilyByToken(_ context.Context, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if current, ok := m.tokens[token]; ok {
		m.revokeFamily(current.familyID)
	}
	return nil
}

func (m *memoryStore) ageToken(token string, age time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	used := time.Now().UTC().Add(-age)
	m.tokens[token].usedAt = &used
}

func (m *memoryStore) eventReasons() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	reasons := make([]string, 0, len(m.events))
	for _, event := range m.events {
		reasons = append(reasons, event.Action)
	}
	return reasons
}

func newTestUUID() (string, error) {
	token, err := identity.GenerateRefreshToken()
	return token, err
}

func newTestService(t *testing.T, store identity.AuthStore, maxFailures int) (*identity.Service, *authz.Verifier) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	kid := authz.KeyID(public)
	signer, err := authz.NewSigner(private, kid, authz.DefaultIssuer)
	if err != nil {
		t.Fatal(err)
	}
	service, err := identity.NewService(store, signer, identity.ServiceConfig{
		AccessTokenTTL:     15 * time.Minute,
		RefreshTokenTTL:    24 * time.Hour,
		RefreshMaxLifetime: 72 * time.Hour,
		Issuer:             authz.DefaultIssuer,
		MaxFailures:        maxFailures,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := authz.NewVerifier(map[string]ed25519.PublicKey{kid: public}, authz.DefaultIssuer, nil)
	if err != nil {
		t.Fatal(err)
	}
	return service, verifier
}

const testPassword = "correct-horse-battery-staple"

func TestLoginIssuesVerifiableSession(t *testing.T) {
	t.Parallel()
	store := newMemoryStore()
	user := store.addUser(t, authz.RoleOperator, false)
	service, verifier := newTestService(t, store, 10)

	session, err := service.Login(context.Background(), "alice", testPassword, "10.0.0.1", "agent", "req-1")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if session.User.ID != user.ID || session.User.Role != authz.RoleOperator || session.MustChangePassword {
		t.Fatalf("unexpected session user: %+v", session.User)
	}
	if session.ExpiresIn != 900 || session.RefreshToken == "" {
		t.Fatalf("unexpected session: %+v", session)
	}
	principal, err := verifier.Verify(session.AccessToken, authz.AudienceGatewayAdmin, authz.AudienceAgent)
	if err != nil {
		t.Fatalf("verify access token: %v", err)
	}
	if principal.UserID != user.ID || principal.Subject != "alice" || principal.Role != authz.RoleOperator ||
		principal.PasswordChangeRequired || principal.Type != authz.TypeHuman {
		t.Fatalf("unexpected principal: %+v", principal)
	}
	if user.LastLoginAt == nil || user.FailedLoginCount != 0 {
		t.Fatalf("successful login did not clear state: %+v", user)
	}
}

func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	t.Parallel()
	const phrase = "invalid username or password"
	cases := []struct {
		name     string
		prepare  func(*memoryStore)
		username string
		password string
	}{
		{"unknown user", func(*memoryStore) {}, "nobody", testPassword},
		{"wrong password", func(*memoryStore) {}, "alice", "not-the-password"},
		{"disabled user", func(m *memoryStore) { m.users["alice"].Disabled = true }, "alice", testPassword},
		{"locked user", func(m *memoryStore) {
			until := time.Now().UTC().Add(time.Hour)
			m.users["alice"].LockedUntil = &until
		}, "alice", testPassword},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			store := newMemoryStore()
			store.addUser(t, authz.RoleFull, false)
			test.prepare(store)
			service, _ := newTestService(t, store, 10)
			session, err := service.Login(context.Background(), test.username, test.password, "", "", "")
			if session != nil || !errors.Is(err, identity.ErrInvalidCredentials) || err.Error() != phrase {
				t.Fatalf("session = %+v, err = %v; want %q", session, err, phrase)
			}
			if len(store.eventReasons()) != 1 || store.events[0].Result != identity.EventResultFailure {
				t.Fatalf("missing failure audit: %+v", store.events)
			}
		})
	}
}

func TestLoginBadPasswordEventuallyLocksAccount(t *testing.T) {
	t.Parallel()
	store := newMemoryStore()
	user := store.addUser(t, authz.RoleFull, false)
	service, _ := newTestService(t, store, 2)

	for i := 0; i < 2; i++ {
		if _, err := service.Login(context.Background(), "alice", "wrong", "", "", ""); !errors.Is(err, identity.ErrInvalidCredentials) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if user.FailedLoginCount != 2 || user.LockedUntil == nil {
		t.Fatalf("account was not locked: count=%d until=%v", user.FailedLoginCount, user.LockedUntil)
	}
	if _, err := service.Login(context.Background(), "alice", testPassword, "", "", ""); !errors.Is(err, identity.ErrInvalidCredentials) {
		t.Fatalf("correct password while locked: %v", err)
	}
}

func TestLoginWritesForcedPasswordChangeFlag(t *testing.T) {
	t.Parallel()
	store := newMemoryStore()
	store.addUser(t, authz.RoleReadOnly, true)
	service, verifier := newTestService(t, store, 10)

	session, err := service.Login(context.Background(), "alice", testPassword, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !session.MustChangePassword {
		t.Fatal("session must carry must_change_password")
	}
	principal, err := verifier.Verify(session.AccessToken, authz.AudienceGatewayAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if !principal.PasswordChangeRequired {
		t.Fatal("access token must carry pcr=true")
	}
}

func TestRefreshRotatesAndPicksUpCurrentRole(t *testing.T) {
	t.Parallel()
	store := newMemoryStore()
	store.addUser(t, authz.RoleReadOnly, false)
	service, verifier := newTestService(t, store, 10)
	ctx := context.Background()

	first, err := service.Login(ctx, "alice", testPassword, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Refresh(ctx, first.RefreshToken, "", "", "")
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if second.RefreshToken == first.RefreshToken || second.AccessToken == first.AccessToken {
		t.Fatal("refresh must issue a new pair")
	}
	if _, err := service.Refresh(ctx, first.RefreshToken, "", "", ""); !errors.Is(err, identity.ErrInvalidRefreshToken) {
		t.Fatalf("replay of consumed token: %v", err)
	}
	store.users["alice"].Role = authz.RoleFull
	third, err := service.Refresh(ctx, second.RefreshToken, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := verifier.Verify(third.AccessToken, authz.AudienceGatewayAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if principal.Role != authz.RoleFull {
		t.Fatalf("refresh did not use current role: %s", principal.Role)
	}
}

func TestRefreshReuseWindowAndFamilyRevocation(t *testing.T) {
	t.Parallel()
	store := newMemoryStore()
	store.addUser(t, authz.RoleFull, false)
	service, _ := newTestService(t, store, 10)
	ctx := context.Background()

	first, err := service.Login(ctx, "alice", testPassword, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Refresh(ctx, first.RefreshToken, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Refresh(ctx, first.RefreshToken, "", "", ""); !errors.Is(err, identity.ErrInvalidRefreshToken) {
		t.Fatalf("retry within grace period: %v", err)
	}
	store.ageToken(first.RefreshToken, identity.RefreshReuseGracePeriod+time.Second)
	// The service collapses reuse and invalid tokens into one 401-worthy
	// error; the store has already revoked the family and filed the alert.
	if _, err := service.Refresh(ctx, first.RefreshToken, "", "", ""); !errors.Is(err, identity.ErrInvalidRefreshToken) {
		t.Fatalf("reuse beyond grace period: %v", err)
	}
	if _, err := service.Refresh(ctx, second.RefreshToken, "", "", ""); !errors.Is(err, identity.ErrInvalidRefreshToken) {
		t.Fatalf("family was not revoked: %v", err)
	}
}

func TestRefreshRejectsDisabledUser(t *testing.T) {
	t.Parallel()
	store := newMemoryStore()
	store.addUser(t, authz.RoleFull, false)
	service, _ := newTestService(t, store, 10)
	ctx := context.Background()

	first, err := service.Login(ctx, "alice", testPassword, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	store.users["alice"].Disabled = true
	second, err := service.Refresh(ctx, first.RefreshToken, "", "", "")
	if second != nil || !errors.Is(err, identity.ErrInvalidRefreshToken) {
		t.Fatalf("refresh of disabled user: %+v, %v", second, err)
	}
	if _, err := service.Refresh(ctx, first.RefreshToken, "", "", ""); !errors.Is(err, identity.ErrInvalidRefreshToken) {
		t.Fatal("refresh family of disabled user was not revoked")
	}
}

func TestLogoutRevokesFamily(t *testing.T) {
	t.Parallel()
	store := newMemoryStore()
	store.addUser(t, authz.RoleFull, false)
	service, _ := newTestService(t, store, 10)
	ctx := context.Background()

	first, err := service.Login(ctx, "alice", testPassword, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Logout(ctx, first.RefreshToken, "req-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Refresh(ctx, first.RefreshToken, "", "", ""); !errors.Is(err, identity.ErrInvalidRefreshToken) {
		t.Fatalf("refresh after logout: %v", err)
	}
}

func TestChangePasswordClearsForcedFlag(t *testing.T) {
	t.Parallel()
	store := newMemoryStore()
	user := store.addUser(t, authz.RoleFull, true)
	service, _ := newTestService(t, store, 10)
	ctx := context.Background()

	if err := service.ChangePassword(ctx, user.ID, "wrong", "a-brand-new-secret", "", ""); !errors.Is(err, identity.ErrInvalidCredentials) {
		t.Fatalf("wrong current password: %v", err)
	}
	if err := service.ChangePassword(ctx, user.ID, testPassword, "short", "", ""); !errors.Is(err, identity.ErrWeakPassword) {
		t.Fatalf("weak password: %v", err)
	}
	oldHash := user.PasswordHash
	if err := service.ChangePassword(ctx, user.ID, testPassword, "a-brand-new-secret", "", ""); err != nil {
		t.Fatalf("change password: %v", err)
	}
	if user.MustChangePassword || user.PasswordHash == oldHash {
		t.Fatal("password change did not clear the flag and update the hash")
	}
	if _, err := service.Login(ctx, "alice", "a-brand-new-secret", "", "", ""); err != nil {
		t.Fatalf("login with new password: %v", err)
	}
}

func TestExpiredLockoutResetsFailureCounter(t *testing.T) {
	t.Parallel()
	store := newMemoryStore()
	user := store.addUser(t, authz.RoleFull, false)
	expired := time.Now().UTC().Add(-time.Minute)
	user.FailedLoginCount = 2
	user.LockedUntil = &expired
	service, _ := newTestService(t, store, 2)

	if _, err := service.Login(context.Background(), "alice", "wrong", "", "", ""); !errors.Is(err, identity.ErrInvalidCredentials) {
		t.Fatalf("err = %v, want ErrInvalidCredentials", err)
	}
	if user.FailedLoginCount != 1 || user.LockedUntil != nil {
		t.Fatalf("expired lockout did not reset counter: count=%d locked=%v", user.FailedLoginCount, user.LockedUntil)
	}
}

func TestChangePasswordRevokesExistingRefreshTokens(t *testing.T) {
	t.Parallel()
	store := newMemoryStore()
	user := store.addUser(t, authz.RoleFull, false)
	service, _ := newTestService(t, store, 10)
	ctx := context.Background()

	session, err := service.Login(ctx, "alice", testPassword, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ChangePassword(ctx, user.ID, testPassword, "a-brand-new-secret", "10.0.0.1", "req-2"); err != nil {
		t.Fatalf("change password: %v", err)
	}
	if _, err := service.Refresh(ctx, session.RefreshToken, "", "", ""); !errors.Is(err, identity.ErrInvalidRefreshToken) {
		t.Fatalf("refresh token survived password change: %v", err)
	}
}
