package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/oxkrypton/Halon/internal/authz"
)

var (
	// ErrInvalidCredentials is the single error returned for an unknown,
	// disabled or locked user and for a bad password. The HTTP layer must
	// map all four to the same 401 response so usernames cannot be probed.
	ErrInvalidCredentials = errors.New("invalid username or password")
	// ErrWeakPassword wraps the password policy rejection.
	ErrWeakPassword = errors.New("password does not satisfy the policy")
	// ErrUsernameTaken marks a create that collided with an existing name.
	ErrUsernameTaken = errors.New("username is already taken")
	// ErrLastAdmin protects the final enabled full-role user from being
	// demoted or disabled, which would lock everyone out of administration.
	ErrLastAdmin = errors.New("cannot demote or disable the last enabled administrator")
	// ErrCannotModifySelf stops an administrator from changing their own
	// role or disabling themselves through the user-management API.
	ErrCannotModifySelf = errors.New("cannot change own role or disable self")
)

// AuthStore is the slice of Store that the authentication service needs.
// Keeping it an interface makes the login/refresh flows testable without a
// live PostgreSQL instance; *Store is the production implementation.
type AuthStore interface {
	GetUserByUsername(ctx context.Context, username string) (*User, error)
	GetUserByID(ctx context.Context, id string) (*User, error)
	ListUsers(ctx context.Context, offset, limit int) ([]User, int64, error)
	CreateUser(ctx context.Context, user *User) error
	UpdateUserGuarded(ctx context.Context, targetID, actorID string, update UserUpdate) error
	RevokeAllUserRefreshTokens(ctx context.Context, userID string) error
	SetPasswordHashOnly(ctx context.Context, username, hash string) error
	SetPasswordHash(ctx context.Context, username, hash string) error
	SetMustChangePassword(ctx context.Context, username string, value bool) error
	RecordFailedLogin(ctx context.Context, username string, failedCount int, lockUntil *time.Time) error
	RecordSuccessfulLogin(ctx context.Context, username string, at time.Time) error
	RecordAuthEvent(ctx context.Context, event AuthEvent) error
	IssueRefreshToken(ctx context.Context, userID string, ttl, maxLifetime time.Duration, clientIP, userAgent string) (RefreshRotation, error)
	RotateRefreshToken(ctx context.Context, token string, ttl, maxLifetime time.Duration, clientIP, userAgent string) (RefreshRotation, error)
	RevokeTokenFamily(ctx context.Context, familyID string) error
	RevokeRefreshTokenFamilyByToken(ctx context.Context, token string) error
}

var _ AuthStore = (*Store)(nil)

// ServiceConfig carries the validated configuration for a Service. The
// gateway config layer is the only production caller; tests may rely on the
// defaults applied by NewService.
type ServiceConfig struct {
	AccessTokenTTL     time.Duration
	RefreshTokenTTL    time.Duration
	RefreshMaxLifetime time.Duration
	// Audiences lists every component that accepts a session access token.
	Audiences []authz.Audience
	// Issuer must match the verifier's issuer; empty uses the signer default.
	Issuer         string
	PasswordPolicy PasswordPolicy
	MaxFailures    int
	LockDuration   time.Duration
}

// Service implements local username/password login and refresh rotation for
// the gateway administration surface. It never logs credentials or token
// material: audit events contain only usernames, IPs, request IDs and a
// short reason string.
type Service struct {
	store  AuthStore
	signer *authz.Signer
	cfg    ServiceConfig
	logger *slog.Logger
}

// SessionUser is the safe subset of a user returned to a browser client.
type SessionUser struct {
	ID       string     `json:"id"`
	Username string     `json:"username"`
	Role     authz.Role `json:"role"`
}

// Session is the response body for login and refresh.
type Session struct {
	AccessToken        string      `json:"access_token"`
	ExpiresIn          int64       `json:"expires_in"`
	RefreshToken       string      `json:"refresh_token"`
	MustChangePassword bool        `json:"must_change_password"`
	User               SessionUser `json:"user"`
}

// NewService validates its dependencies and fills in safe defaults.
func NewService(store AuthStore, signer *authz.Signer, cfg ServiceConfig, logger *slog.Logger) (*Service, error) {
	if store == nil {
		return nil, errors.New("identity service requires a store")
	}
	if signer == nil {
		return nil, errors.New("identity service requires a signer")
	}
	if cfg.AccessTokenTTL <= 0 {
		return nil, errors.New("identity service requires a positive access token TTL")
	}
	if cfg.RefreshTokenTTL <= 0 || cfg.RefreshMaxLifetime <= 0 {
		return nil, errors.New("identity service requires positive refresh TTL and max lifetime")
	}
	if len(cfg.Audiences) == 0 {
		cfg.Audiences = []authz.Audience{authz.AudienceGatewayAdmin, authz.AudienceAgent}
	}
	if cfg.PasswordPolicy.MinLength == 0 {
		cfg.PasswordPolicy = DefaultPasswordPolicy()
	}
	if cfg.MaxFailures <= 0 {
		cfg.MaxFailures = 10
	}
	if cfg.LockDuration <= 0 {
		cfg.LockDuration = 5 * time.Minute
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{store: store, signer: signer, cfg: cfg, logger: logger}, nil
}

// Login verifies a password and issues a new token pair. A nonexistent user
// is verified against the fake hash so the response time does not disclose
// whether the username exists.
func (s *Service) Login(
	ctx context.Context,
	username, password, clientIP, userAgent, requestID string,
) (*Session, error) {
	user, err := s.store.GetUserByUsername(ctx, username)
	if errors.Is(err, ErrNotFound) {
		// #nosec G104 -- verification result is deliberately discarded.
		_, _ = VerifyPassword(FakePasswordHash(), password)
		s.auditFailure(ctx, EventLoginFailure, username, username, clientIP, requestID, "unknown_user")
		return nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, fmt.Errorf("load user for login: %w", err)
	}

	// Always run the expensive verification, even for a disabled or locked
	// account, so those states cannot be distinguished by timing.
	matched, verifyErr := VerifyPassword(user.PasswordHash, password)
	now := time.Now().UTC()
	switch {
	case user.Disabled:
		s.auditFailure(ctx, EventLoginFailure, username, username, clientIP, requestID, "disabled_user")
		return nil, ErrInvalidCredentials
	case user.LockedUntil != nil && user.LockedUntil.After(now):
		s.auditFailure(ctx, EventLoginFailure, username, username, clientIP, requestID, "locked_user")
		return nil, ErrInvalidCredentials
	case verifyErr != nil || !matched:
		failed := user.FailedLoginCount + 1
		var lockUntil *time.Time
		if failed >= s.cfg.MaxFailures {
			until := now.Add(s.cfg.LockDuration)
			lockUntil = &until
		}
		if err := s.store.RecordFailedLogin(ctx, username, failed, lockUntil); err != nil {
			return nil, fmt.Errorf("record failed login: %w", err)
		}
		reason := "bad_password"
		if lockUntil != nil {
			reason = "account_locked"
		}
		s.auditFailure(ctx, EventLoginFailure, username, username, clientIP, requestID, reason)
		return nil, ErrInvalidCredentials
	}

	if needs, err := NeedsRehash(user.PasswordHash); err == nil && needs {
		if hash, err := RehashPassword(password); err == nil {
			if err := s.store.SetPasswordHashOnly(ctx, username, hash); err != nil {
				s.logger.Warn("write rehashed password failed", "error", err)
			}
		}
	}
	if err := s.store.RecordSuccessfulLogin(ctx, username, now); err != nil {
		return nil, fmt.Errorf("record successful login: %w", err)
	}
	rotation, err := s.store.IssueRefreshToken(ctx, user.ID,
		s.cfg.RefreshTokenTTL, s.cfg.RefreshMaxLifetime, clientIP, userAgent)
	if err != nil {
		return nil, fmt.Errorf("issue refresh token: %w", err)
	}
	session, err := s.newSession(user, rotation)
	if err != nil {
		return nil, err
	}
	s.auditSuccess(ctx, EventLoginSuccess, username, username, clientIP, requestID)
	return session, nil
}

// Refresh consumes one refresh token and returns a rotated pair. The user is
// re-read from storage on every refresh so a role change or a disabled flag
// takes effect no later than the next refresh (well under the access TTL).
func (s *Service) Refresh(
	ctx context.Context,
	token, clientIP, userAgent, requestID string,
) (*Session, error) {
	rotation, err := s.store.RotateRefreshToken(ctx, token,
		s.cfg.RefreshTokenTTL, s.cfg.RefreshMaxLifetime, clientIP, userAgent)
	if err != nil {
		if errors.Is(err, ErrInvalidRefreshToken) || errors.Is(err, ErrRefreshTokenReuse) {
			return nil, ErrInvalidRefreshToken
		}
		return nil, fmt.Errorf("rotate refresh token: %w", err)
	}
	user, err := s.store.GetUserByID(ctx, rotation.UserID)
	if err != nil {
		s.revokeFamily(ctx, rotation.FamilyID)
		if errors.Is(err, ErrNotFound) {
			return nil, ErrInvalidRefreshToken
		}
		return nil, fmt.Errorf("load user for refresh: %w", err)
	}
	if user.Disabled {
		s.revokeFamily(ctx, rotation.FamilyID)
		s.auditFailure(ctx, EventLoginFailure, user.Username, user.Username, clientIP, requestID, "disabled_user_refresh")
		return nil, ErrInvalidRefreshToken
	}
	session, err := s.newSession(user, rotation)
	if err != nil {
		return nil, err
	}
	return session, nil
}

// Logout revokes the entire token family that owns the presented refresh
// token. Unknown tokens are a no-op, so logout is safe to retry.
func (s *Service) Logout(ctx context.Context, token, requestID string) error {
	if err := s.store.RevokeRefreshTokenFamilyByToken(ctx, token); err != nil {
		return fmt.Errorf("logout: %w", err)
	}
	s.auditSuccess(ctx, EventLogout, "", "", "", requestID)
	return nil
}

// ChangePassword verifies the current password and installs a new one. The
// caller passes the principal's UserID; static credentials have none and are
// rejected by the HTTP layer before reaching this method.
func (s *Service) ChangePassword(
	ctx context.Context,
	userID, currentPassword, newPassword, clientIP, requestID string,
) error {
	user, err := s.store.GetUserByID(ctx, userID)
	if errors.Is(err, ErrNotFound) {
		return ErrInvalidCredentials
	}
	if err != nil {
		return fmt.Errorf("load user for password change: %w", err)
	}
	matched, err := VerifyPassword(user.PasswordHash, currentPassword)
	if err != nil || !matched {
		s.auditFailure(ctx, EventPasswordChanged, user.Username, user.Username, clientIP, requestID, "bad_current_password")
		return ErrInvalidCredentials
	}
	if err := s.cfg.PasswordPolicy.Validate(user.Username, newPassword); err != nil {
		return fmt.Errorf("%w: %w", ErrWeakPassword, err)
	}
	hash, err := HashPassword(newPassword)
	if err != nil {
		return fmt.Errorf("hash new password: %w", err)
	}
	if err := s.store.SetPasswordHash(ctx, user.Username, hash); err != nil {
		return fmt.Errorf("store new password: %w", err)
	}
	if err := s.store.SetMustChangePassword(ctx, user.Username, false); err != nil {
		return fmt.Errorf("clear password change flag: %w", err)
	}
	s.auditSuccess(ctx, EventPasswordChanged, user.Username, user.Username, clientIP, requestID)
	return nil
}

// newSession signs an access token from the freshly loaded user and pairs it
// with the already-rotated refresh token.
func (s *Service) newSession(user *User, rotation RefreshRotation) (*Session, error) {
	access, err := s.signer.Issue(authz.IssueOptions{
		Subject:                user.Username,
		Type:                   authz.TypeHuman,
		Role:                   user.Role,
		Audiences:              s.cfg.Audiences,
		UserID:                 user.ID,
		PasswordChangeRequired: user.MustChangePassword,
		TTL:                    s.cfg.AccessTokenTTL,
		Issuer:                 s.cfg.Issuer,
	})
	if err != nil {
		return nil, fmt.Errorf("issue access token: %w", err)
	}
	return &Session{
		AccessToken:        access,
		ExpiresIn:          int64(s.cfg.AccessTokenTTL.Seconds()),
		RefreshToken:       rotation.Token,
		MustChangePassword: user.MustChangePassword,
		User: SessionUser{
			ID:       user.ID,
			Username: user.Username,
			Role:     user.Role,
		},
	}, nil
}

func (s *Service) revokeFamily(ctx context.Context, familyID string) {
	if err := s.store.RevokeTokenFamily(ctx, familyID); err != nil {
		s.logger.Warn("revoke refresh token family failed", "error", err)
	}
}

func (s *Service) auditSuccess(ctx context.Context, action, actor, target, clientIP, requestID string) {
	s.audit(ctx, action, EventResultSuccess, actor, target, clientIP, requestID, "")
}

func (s *Service) auditFailure(ctx context.Context, action, actor, target, clientIP, requestID, reason string) {
	s.audit(ctx, action, EventResultFailure, actor, target, clientIP, requestID, reason)
}

func (s *Service) audit(ctx context.Context, action, result, actor, target, clientIP, requestID, reason string) {
	event := AuthEvent{
		Action:         action,
		Result:         result,
		ActorUsername:  actor,
		TargetUsername: target,
		ClientIP:       clientIP,
		RequestID:      requestID,
	}
	if reason != "" {
		detail, err := json.Marshal(map[string]string{"reason": reason})
		if err != nil {
			s.logger.Warn("encode auth event detail failed", "error", err)
		} else {
			event.Detail = JSONB(detail)
		}
	}
	if err := s.store.RecordAuthEvent(ctx, event); err != nil {
		s.logger.Warn("write auth event failed", "action", action, "error", err)
	}
}
