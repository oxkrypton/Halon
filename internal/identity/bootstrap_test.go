package identity_test

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/oxkrypton/Halon/internal/identity"
	"gorm.io/gorm"
)

func TestBootstrapAdminCreatesFirstUser(t *testing.T) {
	t.Parallel()
	store, mock := newMockStore(t)
	mock.ExpectExec(`INSERT INTO auth_users`).
		WithArgs("admin", sqlmock.AnyArg(), "full").
		WillReturnResult(sqlmock.NewResult(0, 1))

	created, err := store.BootstrapAdmin(context.Background(), "admin",
		"bootstrap-secret-phrase", identity.DefaultPasswordPolicy())
	if err != nil || !created {
		t.Fatalf("bootstrap = %v, %v; want true, nil", created, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestBootstrapAdminSkipsWhenUsersAlreadyExist(t *testing.T) {
	t.Parallel()
	store, mock := newMockStore(t)
	mock.ExpectExec(`INSERT INTO auth_users`).
		WithArgs("admin", sqlmock.AnyArg(), "full").
		WillReturnResult(sqlmock.NewResult(0, 0))

	created, err := store.BootstrapAdmin(context.Background(), "admin",
		"bootstrap-secret-phrase", identity.DefaultPasswordPolicy())
	if err != nil || created {
		t.Fatalf("bootstrap = %v, %v; want false, nil", created, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestBootstrapAdminTreatsUniqueRaceAsNoop(t *testing.T) {
	t.Parallel()
	store, mock := newMockStore(t)
	mock.ExpectExec(`INSERT INTO auth_users`).
		WithArgs("admin", sqlmock.AnyArg(), "full").
		WillReturnError(gorm.ErrDuplicatedKey)

	created, err := store.BootstrapAdmin(context.Background(), "admin",
		"bootstrap-secret-phrase", identity.DefaultPasswordPolicy())
	if err != nil || created {
		t.Fatalf("bootstrap = %v, %v; want false, nil", created, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestBootstrapAdminValidatesInput(t *testing.T) {
	t.Parallel()
	store, _ := newMockStore(t)
	if _, err := store.BootstrapAdmin(context.Background(), "ADMIN",
		"bootstrap-secret-phrase", identity.DefaultPasswordPolicy()); !errors.Is(err, identity.ErrInvalidUsername) {
		t.Fatalf("invalid username: %v", err)
	}
	if _, err := store.BootstrapAdmin(context.Background(), "admin",
		"short", identity.DefaultPasswordPolicy()); !errors.Is(err, identity.ErrWeakPassword) {
		t.Fatalf("weak password: %v", err)
	}
}
