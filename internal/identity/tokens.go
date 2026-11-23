package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

const (
	// RefreshTokenBytes is the entropy of an opaque refresh token. Only its
	// SHA-256 digest is stored in the database.
	RefreshTokenBytes = 32

	// RefreshReuseGracePeriod tolerates a retried request that raced the
	// first successful rotation (e.g. a client with an aggressive timeout).
	// Beyond it, a used token is treated as stolen.
	RefreshReuseGracePeriod = 10 * time.Second
)

var (
	ErrInvalidRefreshToken = errors.New("invalid or expired refresh token")
	ErrRefreshTokenReuse   = errors.New("refresh token reuse detected")
)

// RefreshRotation is the outcome of issuing or rotating a refresh token.
// Token is the plaintext value returned to the caller exactly once; it is
// never persisted or logged.
type RefreshRotation struct {
	Token           string
	UserID          string
	FamilyID        string
	FamilyStartedAt time.Time
	ExpiresAt       time.Time
}

type refreshTokenRow struct {
	UserID          string
	FamilyID        string
	FamilyStartedAt time.Time
	ExpiresAt       time.Time
	UsedAt          *time.Time
	RevokedAt       *time.Time
}

// GenerateRefreshToken returns a base64url-encoded 32-byte random value.
func GenerateRefreshToken() (string, error) {
	buf := make([]byte, RefreshTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate refresh token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// HashRefreshToken returns the SHA-256 digest stored in token_hash.
func HashRefreshToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func newUUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate uuid: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// IssueRefreshToken starts a new token family at login.
func (s *Store) IssueRefreshToken(
	ctx context.Context,
	userID string,
	ttl, maxLifetime time.Duration,
	clientIP, userAgent string,
) (RefreshRotation, error) {
	familyID, err := newUUIDv4()
	if err != nil {
		return RefreshRotation{}, err
	}
	return s.insertRefreshToken(ctx, userID, familyID, time.Now().UTC(), ttl, maxLifetime, clientIP, userAgent)
}

// RotateRefreshToken consumes token and issues one replacement in the same
// family. The UPDATE ... WHERE used_at IS NULL ... RETURNING statement is the
// atomic decision point: across replicas only one caller can consume a token.
// ttl and maxLifetime are injected by the caller (config layer).
func (s *Store) RotateRefreshToken(
	ctx context.Context,
	token string,
	ttl, maxLifetime time.Duration,
	clientIP, userAgent string,
) (RefreshRotation, error) {
	hash := HashRefreshToken(token)
	var consumed refreshTokenRow
	result := s.db.WithContext(ctx).Raw(`
		UPDATE auth_refresh_tokens
		   SET used_at = NOW()
		 WHERE token_hash = $1
		   AND used_at IS NULL
		   AND revoked_at IS NULL
		   AND expires_at > NOW()
		RETURNING user_id, family_id, family_started_at, expires_at`, hash).Scan(&consumed)
	if result.Error == nil && result.RowsAffected > 0 {
		return s.insertRefreshToken(ctx, consumed.UserID, consumed.FamilyID,
			consumed.FamilyStartedAt, ttl, maxLifetime, clientIP, userAgent)
	}
	if result.Error != nil && !errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return RefreshRotation{}, fmt.Errorf("consume refresh token: %w", result.Error)
	}
	return s.inspectUnusableToken(ctx, hash)
}

// inspectUnusableToken distinguishes a harmless retry from theft when the
// atomic consume missed.
func (s *Store) inspectUnusableToken(ctx context.Context, hash []byte) (RefreshRotation, error) {
	var row refreshTokenRow
	result := s.db.WithContext(ctx).Raw(`
		SELECT user_id, family_id, used_at, revoked_at, expires_at
		  FROM auth_refresh_tokens
		 WHERE token_hash = $1`, hash).Scan(&row)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) || result.RowsAffected == 0 {
		return RefreshRotation{}, ErrInvalidRefreshToken
	}
	if result.Error != nil {
		return RefreshRotation{}, fmt.Errorf("inspect refresh token: %w", result.Error)
	}
	if row.RevokedAt != nil || row.UsedAt == nil ||
		time.Since(*row.UsedAt) <= RefreshReuseGracePeriod {
		return RefreshRotation{}, ErrInvalidRefreshToken
	}

	// The token was consumed long ago: revoke every descendant and alert.
	if err := s.RevokeTokenFamily(ctx, row.FamilyID); err != nil {
		return RefreshRotation{}, err
	}
	detail, err := json.Marshal(map[string]string{
		"user_id":   row.UserID,
		"family_id": row.FamilyID,
	})
	if err != nil {
		return RefreshRotation{}, fmt.Errorf("encode reuse detail: %w", err)
	}
	if err := s.RecordAuthEvent(ctx, AuthEvent{
		Action: EventRefreshReuseDetected,
		Result: EventResultFailure,
		Detail: JSONB(detail),
	}); err != nil {
		return RefreshRotation{}, err
	}
	return RefreshRotation{}, ErrRefreshTokenReuse
}

// RevokeRefreshTokenFamilyByToken invalidates the whole family that owns the
// presented token. Unknown tokens are a no-op so logout stays idempotent.
func (s *Store) RevokeRefreshTokenFamilyByToken(ctx context.Context, token string) error {
	var familyID string
	result := s.db.WithContext(ctx).Raw(`
		SELECT family_id FROM auth_refresh_tokens
		 WHERE token_hash = $1`, HashRefreshToken(token)).Scan(&familyID)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) || result.RowsAffected == 0 {
		return nil
	}
	if result.Error != nil {
		return fmt.Errorf("lookup refresh token family: %w", result.Error)
	}
	return s.RevokeTokenFamily(ctx, familyID)
}

// RevokeTokenFamily invalidates every refresh token descended from one login.
// The service layer calls this when it observes a disabled user or a reuse
// alert.
func (s *Store) RevokeTokenFamily(ctx context.Context, familyID string) error {
	result := s.db.WithContext(ctx).Exec(`
		UPDATE auth_refresh_tokens
		   SET revoked_at = NOW()
		 WHERE family_id = $1 AND revoked_at IS NULL`, familyID)
	if result.Error != nil {
		return fmt.Errorf("revoke refresh token family: %w", result.Error)
	}
	return nil
}

func (s *Store) insertRefreshToken(
	ctx context.Context,
	userID, familyID string,
	familyStartedAt time.Time,
	ttl, maxLifetime time.Duration,
	clientIP, userAgent string,
) (RefreshRotation, error) {
	token, err := GenerateRefreshToken()
	if err != nil {
		return RefreshRotation{}, err
	}
	now := time.Now().UTC()
	expiresAt := now.Add(ttl)
	if familyDeadline := familyStartedAt.Add(maxLifetime); familyDeadline.Before(expiresAt) {
		expiresAt = familyDeadline
	}
	if !expiresAt.After(now) {
		return RefreshRotation{}, ErrInvalidRefreshToken
	}
	result := s.db.WithContext(ctx).Exec(`
		INSERT INTO auth_refresh_tokens
		    (user_id, family_id, family_started_at, token_hash, issued_at,
		     expires_at, client_ip, user_agent)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		userID, familyID, familyStartedAt, HashRefreshToken(token),
		now, expiresAt, clientIP, userAgent)
	if result.Error != nil {
		return RefreshRotation{}, fmt.Errorf("issue refresh token: %w", result.Error)
	}
	return RefreshRotation{
		Token:           token,
		UserID:          userID,
		FamilyID:        familyID,
		FamilyStartedAt: familyStartedAt,
		ExpiresAt:       expiresAt,
	}, nil
}
