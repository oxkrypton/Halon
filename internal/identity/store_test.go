package identity_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/oxkrypton/Halon/internal/authz"
	"github.com/oxkrypton/Halon/internal/identity"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

const sampleHash = "$argon2id$hash"

func newMockStore(t *testing.T) (*identity.Store, sqlmock.Sqlmock) {
	t.Helper()
	sqlDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	gormDB, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{
		DisableAutomaticPing: true,
	})
	if err != nil {
		t.Fatalf("open gorm over sqlmock: %v", err)
	}
	return identity.NewStore(gormDB), mock
}

func userRows() *sqlmock.Rows {
	now := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	return sqlmock.NewRows([]string{
		"id", "username", "display_name", "password_hash", "role",
		"disabled", "must_change_password", "failed_login_count", "locked_until",
		"password_changed_at", "last_login_at", "created_by", "created_at", "updated_at",
	}).AddRow("user-uuid", "alice", "Alice", sampleHash, "operator",
		false, false, 0, nil, now, nil, nil, now, now)
}

func TestGetUserByUsername(t *testing.T) {
	t.Parallel()

	store, mock := newMockStore(t)
	mock.ExpectQuery(`SELECT \* FROM "auth_users" WHERE username = \$1 LIMIT \$2`).
		WithArgs("alice", 1).
		WillReturnRows(userRows())

	user, err := store.GetUserByUsername(context.Background(), "alice")
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if user.ID != "user-uuid" || user.Username != "alice" || user.DisplayName != "Alice" ||
		user.PasswordHash != sampleHash || user.Role != authz.RoleOperator || user.Disabled {
		t.Fatalf("unexpected user: %+v", user)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetUserByUsernameNotFound(t *testing.T) {
	t.Parallel()

	store, mock := newMockStore(t)
	mock.ExpectQuery(`SELECT \* FROM "auth_users" WHERE username = \$1 LIMIT \$2`).
		WithArgs("missing", 1).
		WillReturnError(gorm.ErrRecordNotFound)

	_, err := store.GetUserByUsername(context.Background(), "missing")
	if !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetUserByID(t *testing.T) {
	t.Parallel()

	store, mock := newMockStore(t)
	mock.ExpectQuery(`SELECT \* FROM "auth_users" WHERE id = \$1 LIMIT \$2`).
		WithArgs("user-uuid", 1).
		WillReturnRows(userRows())

	user, err := store.GetUserByID(context.Background(), "user-uuid")
	if err != nil || user.Username != "alice" {
		t.Fatalf("get user by id = %+v, %v", user, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCreateUserWritesBackGeneratedID(t *testing.T) {
	t.Parallel()

	store, mock := newMockStore(t)
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO "auth_users"`).
		WithArgs("bob", "Bob", sampleHash, "readonly", false, false, 0, nil,
			sqlmock.AnyArg(), nil, nil, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("new-uuid"))
	mock.ExpectCommit()

	user := &identity.User{
		Username:     "bob",
		DisplayName:  "Bob",
		PasswordHash: sampleHash,
		Role:         authz.RoleReadOnly,
	}
	if err := store.CreateUser(context.Background(), user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if user.ID != "new-uuid" {
		t.Fatalf("generated id = %q, want new-uuid", user.ID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCountEnabledFullUsers(t *testing.T) {
	t.Parallel()

	store, mock := newMockStore(t)
	mock.ExpectQuery(`SELECT count\(\*\) FROM "auth_users" WHERE role = \$1 AND disabled = FALSE`).
		WithArgs("full").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))

	count, err := store.CountEnabledFullUsers(context.Background())
	if err != nil || count != 2 {
		t.Fatalf("count = %d, %v; want 2, nil", count, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSetUserDisabled(t *testing.T) {
	t.Parallel()

	store, mock := newMockStore(t)
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE "auth_users" SET`).
		WithArgs(true, sqlmock.AnyArg(), "alice").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if err := store.SetUserDisabled(context.Background(), "alice", true); err != nil {
		t.Fatalf("disable user: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSetUserRole(t *testing.T) {
	t.Parallel()

	store, mock := newMockStore(t)
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE "auth_users" SET`).
		WithArgs("full", sqlmock.AnyArg(), "alice").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if err := store.SetUserRole(context.Background(), "alice", authz.RoleFull); err != nil {
		t.Fatalf("set role: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSetPasswordHashClearsLockout(t *testing.T) {
	t.Parallel()

	store, mock := newMockStore(t)
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE "auth_users" SET`).
		WithArgs(0, nil, sqlmock.AnyArg(), "$argon2id$new", sqlmock.AnyArg(), "alice").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if err := store.SetPasswordHash(context.Background(), "alice", "$argon2id$new"); err != nil {
		t.Fatalf("set password hash: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRecordFailedAndSuccessfulLogin(t *testing.T) {
	t.Parallel()

	store, mock := newMockStore(t)
	lockUntil := time.Date(2026, 11, 23, 10, 15, 0, 0, time.UTC)
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE "auth_users" SET`).
		WithArgs(3, lockUntil, sqlmock.AnyArg(), "alice").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE "auth_users" SET`).
		WithArgs(0, sqlmock.AnyArg(), nil, sqlmock.AnyArg(), "alice").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	ctx := context.Background()
	if err := store.RecordFailedLogin(ctx, "alice", 3, &lockUntil); err != nil {
		t.Fatalf("record failed login: %v", err)
	}
	if err := store.RecordSuccessfulLogin(ctx, "alice", time.Now().UTC()); err != nil {
		t.Fatalf("record successful login: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSetMustChangePassword(t *testing.T) {
	t.Parallel()

	store, mock := newMockStore(t)
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE "auth_users" SET`).
		WithArgs(true, sqlmock.AnyArg(), "alice").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if err := store.SetMustChangePassword(context.Background(), "alice", true); err != nil {
		t.Fatalf("set must change password: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateUserReportsMissingUser(t *testing.T) {
	t.Parallel()

	store, mock := newMockStore(t)
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE "auth_users" SET`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()

	err := store.SetUserDisabled(context.Background(), "missing", true)
	if !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRecordAuthEvent(t *testing.T) {
	t.Parallel()

	store, mock := newMockStore(t)
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO "auth_events"`).
		WithArgs(identity.EventLoginFailure, identity.EventResultFailure, "", "alice", "10.0.0.1", "req-1", `{"reason":"bad_password"}`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(42))
	mock.ExpectCommit()

	event := identity.AuthEvent{
		Action:         identity.EventLoginFailure,
		Result:         identity.EventResultFailure,
		TargetUsername: "alice",
		ClientIP:       "10.0.0.1",
		RequestID:      "req-1",
		Detail:         identity.JSONB(`{"reason":"bad_password"}`),
	}
	if err := store.RecordAuthEvent(context.Background(), event); err != nil {
		t.Fatalf("record auth event: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestJSONBRejectsInvalidJSON(t *testing.T) {
	t.Parallel()

	if _, err := identity.JSONB(`{broken`).Value(); err == nil {
		t.Fatal("invalid JSON must be rejected before reaching the database")
	}
	value, err := identity.JSONB(nil).Value()
	if err != nil || value != nil {
		t.Fatalf("empty JSONB value = %v, %v; want nil, nil", value, err)
	}
}

func fullUserRows() *sqlmock.Rows {
	now := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	return sqlmock.NewRows([]string{
		"id", "username", "display_name", "password_hash", "role",
		"disabled", "must_change_password", "failed_login_count", "locked_until",
		"password_changed_at", "last_login_at", "created_by", "created_at", "updated_at",
	}).AddRow("user-uuid", "alice", "Alice", sampleHash, "full",
		false, false, 0, nil, now, nil, nil, now, now)
}

func TestListUsers(t *testing.T) {
	t.Parallel()
	store, mock := newMockStore(t)
	mock.ExpectQuery(`SELECT count\(\*\) FROM "auth_users"`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`SELECT \* FROM "auth_users" ORDER BY created_at DESC LIMIT \$1 OFFSET \$2`).
		WithArgs(10, 5).
		WillReturnRows(userRows())

	users, total, err := store.ListUsers(context.Background(), 5, 10)
	if err != nil || total != 1 || len(users) != 1 || users[0].Username != "alice" {
		t.Fatalf("list users = %+v, %d, %v", users, total, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRevokeAllUserRefreshTokens(t *testing.T) {
	t.Parallel()
	store, mock := newMockStore(t)
	mock.ExpectExec(`UPDATE auth_refresh_tokens SET revoked_at = NOW\(\) WHERE user_id = \$1 AND revoked_at IS NULL`).
		WithArgs("user-uuid").
		WillReturnResult(sqlmock.NewResult(0, 3))
	if err := store.RevokeAllUserRefreshTokens(context.Background(), "user-uuid"); err != nil {
		t.Fatalf("revoke user tokens: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateUserGuardedDisplayName(t *testing.T) {
	t.Parallel()
	store, mock := newMockStore(t)
	name := "Alice Updated"
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT "id" FROM "auth_users" WHERE role = \$1 AND disabled = FALSE FOR UPDATE`).
		WithArgs("full").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("user-1").AddRow("user-uuid"))
	mock.ExpectQuery(`SELECT \* FROM "auth_users" WHERE id = \$1 LIMIT \$2 FOR UPDATE`).
		WithArgs("user-uuid", 1).
		WillReturnRows(userRows())
	mock.ExpectExec(`UPDATE "auth_users" SET`).
		WithArgs(name, sqlmock.AnyArg(), "user-uuid").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if err := store.UpdateUserGuarded(context.Background(), "user-uuid", "actor-id",
		identity.UserUpdate{DisplayName: &name}); err != nil {
		t.Fatalf("update user: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateUserGuardedRejectsLastAdmin(t *testing.T) {
	t.Parallel()
	store, mock := newMockStore(t)
	disabled := true
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT "id" FROM "auth_users" WHERE role = \$1 AND disabled = FALSE FOR UPDATE`).
		WithArgs("full").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("user-uuid"))
	mock.ExpectQuery(`SELECT \* FROM "auth_users" WHERE id = \$1 LIMIT \$2 FOR UPDATE`).
		WithArgs("user-uuid", 1).
		WillReturnRows(fullUserRows())
	mock.ExpectRollback()

	err := store.UpdateUserGuarded(context.Background(), "user-uuid", "actor-id",
		identity.UserUpdate{Disabled: &disabled})
	if !errors.Is(err, identity.ErrLastAdmin) {
		t.Fatalf("error = %v, want ErrLastAdmin", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
