package identity_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/oxkrypton/Halon/internal/identity"
)

func rotateRows(familyStartedAt time.Time) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"user_id", "family_id", "family_started_at", "expires_at"}).
		AddRow("user-uuid", "family-uuid", familyStartedAt, familyStartedAt.Add(time.Hour))
}

func emptyConsume(t *testing.T, mock sqlmock.Sqlmock, hash []byte) {
	t.Helper()
	mock.ExpectQuery(`UPDATE auth_refresh_tokens SET used_at = NOW\(\) WHERE token_hash = \$1`).
		WithArgs(hash).
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "family_id", "family_started_at", "expires_at"}))
}

func expectInspect(mock sqlmock.Sqlmock, hash []byte) *sqlmock.ExpectedQuery {
	return mock.ExpectQuery(`SELECT user_id, family_id, used_at, revoked_at, expires_at FROM auth_refresh_tokens WHERE token_hash = \$1`).
		WithArgs(hash)
}

func TestGenerateAndHashRefreshToken(t *testing.T) {
	t.Parallel()

	first, err := identity.GenerateRefreshToken()
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	second, err := identity.GenerateRefreshToken()
	if err != nil {
		t.Fatalf("generate second token: %v", err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(first)
	if err != nil {
		t.Fatalf("decode token: %v", err)
	}
	if len(raw) != identity.RefreshTokenBytes || first == second {
		t.Fatalf("token length = %d, unique = %v", len(raw), first != second)
	}
	digest := identity.HashRefreshToken(first)
	want := sha256.Sum256([]byte(first))
	if len(digest) != sha256.Size || string(digest) != string(want[:]) {
		t.Fatal("token hash must be the SHA-256 digest of the plaintext")
	}
	if string(identity.HashRefreshToken(second)) == string(digest) {
		t.Fatal("different tokens must have different digests")
	}
}

func TestRotateRefreshTokenSuccess(t *testing.T) {
	t.Parallel()

	store, mock := newMockStore(t)
	rawValue := "opaque-refresh-value-for-test"
	hash := identity.HashRefreshToken(rawValue)
	startedAt := time.Now().UTC().Add(-time.Minute)

	mock.ExpectQuery(`UPDATE auth_refresh_tokens SET used_at = NOW\(\) WHERE token_hash = \$1`).
		WithArgs(hash).
		WillReturnRows(rotateRows(startedAt))
	mock.ExpectExec(`INSERT INTO auth_refresh_tokens`).
		WithArgs("user-uuid", "family-uuid", startedAt, sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), "10.0.0.4", "test-agent").
		WillReturnResult(sqlmock.NewResult(0, 1))

	rotation, err := store.RotateRefreshToken(context.Background(), rawValue,
		15*time.Minute, 24*time.Hour, "10.0.0.4", "test-agent")
	if err != nil {
		t.Fatalf("rotate token: %v", err)
	}
	if rotation.UserID != "user-uuid" || rotation.FamilyID != "family-uuid" ||
		!rotation.FamilyStartedAt.Equal(startedAt) || rotation.Token == "" {
		t.Fatalf("unexpected rotation: %+v", rotation)
	}
	if rotation.Token == rawValue {
		t.Fatal("rotation must issue a different token")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRotateRefreshTokenHonorsFamilyLifetime(t *testing.T) {
	t.Parallel()

	store, mock := newMockStore(t)
	rawValue := "another-opaque-refresh-value"
	hash := identity.HashRefreshToken(rawValue)
	startedAt := time.Now().UTC().Add(-13 * time.Minute)

	mock.ExpectQuery(`UPDATE auth_refresh_tokens SET used_at = NOW\(\) WHERE token_hash = \$1`).
		WithArgs(hash).
		WillReturnRows(rotateRows(startedAt))
	mock.ExpectExec(`INSERT INTO auth_refresh_tokens`).
		WithArgs("user-uuid", "family-uuid", startedAt, sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), "", "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	before := time.Now().UTC()
	rotation, err := store.RotateRefreshToken(context.Background(), rawValue,
		15*time.Minute, 20*time.Minute, "", "")
	after := time.Now().UTC().Add(15 * time.Minute)
	if err != nil {
		t.Fatalf("rotate token: %v", err)
	}
	// Started 13 minutes ago, max lifetime 20 minutes: about 7 minutes left,
	// earlier than the 15-minute rolling TTL.
	lower := before.Add(7 * time.Minute).Add(-2 * time.Second)
	if rotation.ExpiresAt.Before(lower) || rotation.ExpiresAt.After(after) {
		t.Fatalf("expires_at = %v, want ~7 minutes from now", rotation.ExpiresAt)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRotateRefreshTokenWithinGracePeriod(t *testing.T) {
	t.Parallel()

	store, mock := newMockStore(t)
	rawValue := "value-retried-within-grace"
	hash := identity.HashRefreshToken(rawValue)
	usedAt := time.Now().UTC().Add(-3 * time.Second)

	emptyConsume(t, mock, hash)
	expectInspect(mock, hash).WillReturnRows(
		sqlmock.NewRows([]string{"user_id", "family_id", "used_at", "revoked_at", "expires_at"}).
			AddRow("user-uuid", "family-uuid", usedAt, nil, time.Now().UTC().Add(time.Hour)))

	_, err := store.RotateRefreshToken(context.Background(), rawValue, 15*time.Minute, 24*time.Hour, "", "")
	if !errors.Is(err, identity.ErrInvalidRefreshToken) {
		t.Fatalf("error = %v, want ErrInvalidRefreshToken", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRotateRefreshTokenDetectsReuseAndRevokesFamily(t *testing.T) {
	t.Parallel()

	store, mock := newMockStore(t)
	rawValue := "stolen-refresh-value"
	hash := identity.HashRefreshToken(rawValue)
	usedAt := time.Now().UTC().Add(-time.Minute)

	emptyConsume(t, mock, hash)
	expectInspect(mock, hash).WillReturnRows(
		sqlmock.NewRows([]string{"user_id", "family_id", "used_at", "revoked_at", "expires_at"}).
			AddRow("user-uuid", "family-uuid", usedAt, nil, time.Now().UTC().Add(time.Hour)))
	mock.ExpectExec(`UPDATE auth_refresh_tokens SET revoked_at = NOW\(\) WHERE family_id = \$1`).
		WithArgs("family-uuid").
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO "auth_events"`).
		WithArgs(identity.EventRefreshReuseDetected, identity.EventResultFailure, "", "", "", "",
			`{"family_id":"family-uuid","user_id":"user-uuid"}`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1))
	mock.ExpectCommit()

	_, err := store.RotateRefreshToken(context.Background(), rawValue, 15*time.Minute, 24*time.Hour, "", "")
	if !errors.Is(err, identity.ErrRefreshTokenReuse) {
		t.Fatalf("error = %v, want ErrRefreshTokenReuse", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRotateRefreshTokenRejectsUnknownExpiredAndRevoked(t *testing.T) {
	t.Parallel()

	t.Run("unknown", func(t *testing.T) {
		store, mock := newMockStore(t)
		rawValue := "never-issued-value"
		hash := identity.HashRefreshToken(rawValue)
		emptyConsume(t, mock, hash)
		expectInspect(mock, hash).WillReturnRows(
			sqlmock.NewRows([]string{"user_id", "family_id", "used_at", "revoked_at", "expires_at"}))
		_, err := store.RotateRefreshToken(context.Background(), rawValue, 15*time.Minute, 24*time.Hour, "", "")
		if !errors.Is(err, identity.ErrInvalidRefreshToken) {
			t.Fatalf("error = %v, want ErrInvalidRefreshToken", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("expired", func(t *testing.T) {
		store, mock := newMockStore(t)
		rawValue := "expired-refresh-value"
		hash := identity.HashRefreshToken(rawValue)
		emptyConsume(t, mock, hash)
		expectInspect(mock, hash).WillReturnRows(
			sqlmock.NewRows([]string{"user_id", "family_id", "used_at", "revoked_at", "expires_at"}).
				AddRow("user-uuid", "family-uuid", nil, nil, time.Now().UTC().Add(-time.Hour)))
		_, err := store.RotateRefreshToken(context.Background(), rawValue, 15*time.Minute, 24*time.Hour, "", "")
		if !errors.Is(err, identity.ErrInvalidRefreshToken) {
			t.Fatalf("error = %v, want ErrInvalidRefreshToken", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("revoked", func(t *testing.T) {
		store, mock := newMockStore(t)
		rawValue := "revoked-refresh-value"
		hash := identity.HashRefreshToken(rawValue)
		revokedAt := time.Now().UTC().Add(-time.Minute)
		emptyConsume(t, mock, hash)
		expectInspect(mock, hash).WillReturnRows(
			sqlmock.NewRows([]string{"user_id", "family_id", "used_at", "revoked_at", "expires_at"}).
				AddRow("user-uuid", "family-uuid", nil, revokedAt, time.Now().UTC().Add(time.Hour)))
		_, err := store.RotateRefreshToken(context.Background(), rawValue, 15*time.Minute, 24*time.Hour, "", "")
		if !errors.Is(err, identity.ErrInvalidRefreshToken) {
			t.Fatalf("error = %v, want ErrInvalidRefreshToken", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestIssueRefreshToken(t *testing.T) {
	t.Parallel()

	store, mock := newMockStore(t)
	mock.ExpectExec(`INSERT INTO auth_refresh_tokens`).
		WithArgs("user-uuid", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), "10.0.0.9", "login-test").
		WillReturnResult(sqlmock.NewResult(0, 1))

	rotation, err := store.IssueRefreshToken(context.Background(), "user-uuid",
		15*time.Minute, 24*time.Hour, "10.0.0.9", "login-test")
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	if rotation.UserID != "user-uuid" || rotation.FamilyID == "" || rotation.Token == "" {
		t.Fatalf("unexpected rotation: %+v", rotation)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRevokeTokenFamily(t *testing.T) {
	t.Parallel()

	store, mock := newMockStore(t)
	mock.ExpectExec(`UPDATE auth_refresh_tokens SET revoked_at = NOW\(\) WHERE family_id = \$1`).
		WithArgs("family-uuid").
		WillReturnResult(sqlmock.NewResult(0, 3))

	if err := store.RevokeTokenFamily(context.Background(), "family-uuid"); err != nil {
		t.Fatalf("revoke family: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
