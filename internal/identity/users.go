package identity

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/oxkrypton/Halon/internal/authz"
)

// UserView is the safe projection of a user returned by the management API.
// PasswordHash and token-related state are deliberately absent.
type UserView struct {
	ID                 string     `json:"id"`
	Username           string     `json:"username"`
	DisplayName        string     `json:"display_name"`
	Role               authz.Role `json:"role"`
	Disabled           bool       `json:"disabled"`
	MustChangePassword bool       `json:"must_change_password"`
	CreatedAt          time.Time  `json:"created_at"`
}

func toUserView(user *User) UserView {
	return UserView{
		ID:                 user.ID,
		Username:           user.Username,
		DisplayName:        user.DisplayName,
		Role:               user.Role,
		Disabled:           user.Disabled,
		MustChangePassword: user.MustChangePassword,
		CreatedAt:          user.CreatedAt,
	}
}

// UserUpdate carries only PATCH-able fields. Nil means "not present", which
// lets the handler distinguish an omitted field from an explicit false.
type UserUpdate struct {
	Role        *authz.Role
	Disabled    *bool
	DisplayName *string
}

type CreateUserInput struct {
	Username      string
	Role          authz.Role
	DisplayName   string
	Password      string // empty means "generate a temporary password"
	ActorUserID   *string
	ActorUsername string
	ClientIP      string
	RequestID     string
}

type CreateUserResult struct {
	User UserView
	// GeneratedPassword is populated only when the server generated one. It
	// is returned exactly once and never stored in plaintext.
	GeneratedPassword string
}

const defaultTempPasswordLength = 24

// GenerateTemporaryPassword returns 24 URL-safe characters of randomness.
// It comfortably satisfies the default 12-byte policy.
func GenerateTemporaryPassword() (string, error) {
	return GenerateTemporaryPasswordLength(defaultTempPasswordLength)
}

// GenerateTemporaryPasswordLength returns at least minLength URL-safe random
// characters (with a floor of 24), bounded by MaxPasswordLength.
func GenerateTemporaryPasswordLength(minLength int) (string, error) {
	length := max(defaultTempPasswordLength, minLength)
	if length > MaxPasswordLength {
		return "", fmt.Errorf("%w: temporary password length %d exceeds %d", ErrInvalidPasswordPolicy, length, MaxPasswordLength)
	}
	rawBytes := (length*3 + 3) / 4
	buf := make([]byte, rawBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate temporary password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf)[:length], nil
}

// ListUsers returns one page of users plus the total count.
func (s *Service) ListUsers(ctx context.Context, offset, limit int) ([]UserView, int64, error) {
	users, total, err := s.store.ListUsers(ctx, offset, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("list identity users: %w", err)
	}
	views := make([]UserView, 0, len(users))
	for i := range users {
		views = append(views, toUserView(&users[i]))
	}
	return views, total, nil
}

// GetUser loads one user for the management API.
func (s *Service) GetUser(ctx context.Context, id string) (*UserView, error) {
	user, err := s.store.GetUserByID(ctx, id)
	if err != nil {
		return nil, err
	}
	view := toUserView(user)
	return &view, nil
}

// CreateUser validates the input and stores a new user. A missing password
// generates a temporary one that the caller must return to the administrator
// exactly once; the new user is forced to change it at first login.
func (s *Service) CreateUser(ctx context.Context, input CreateUserInput) (*CreateUserResult, error) {
	if err := ValidateUsername(input.Username); err != nil {
		return nil, err
	}
	role, err := authz.ParseRole(string(input.Role))
	if err != nil {
		return nil, fmt.Errorf("invalid role: %w", err)
	}
	if _, err := s.store.GetUserByUsername(ctx, input.Username); err == nil {
		return nil, ErrUsernameTaken
	} else if !errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("check username availability: %w", err)
	}

	password := input.Password
	generated := password == ""
	if generated {
		password, err = GenerateTemporaryPasswordLength(s.cfg.PasswordPolicy.MinLength)
		if err != nil {
			return nil, err
		}
	}
	if err := s.cfg.PasswordPolicy.Validate(input.Username, password); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrWeakPassword, err)
	}
	hash, err := HashPassword(password)
	if err != nil {
		return nil, fmt.Errorf("hash user password: %w", err)
	}
	user := &User{
		Username:           input.Username,
		DisplayName:        input.DisplayName,
		PasswordHash:       hash,
		Role:               role,
		MustChangePassword: generated,
		CreatedBy:          input.ActorUserID,
	}
	if err := s.store.CreateUser(ctx, user); err != nil {
		// The database's UNIQUE constraint is the final arbiter when two
		// administrators create the same username concurrently.
		if errors.Is(err, ErrUsernameTaken) {
			return nil, err
		}
		return nil, fmt.Errorf("create user: %w", err)
	}
	s.auditSuccess(ctx, EventUserCreated, input.ActorUsername, input.Username, input.ClientIP, input.RequestID)

	result := &CreateUserResult{User: toUserView(user)}
	if generated {
		result.GeneratedPassword = password
	}
	return result, nil
}

// UpdateUser applies a PATCH in a single transaction. The store locks every
// enabled full user before deciding, so the last administrator cannot be
// demoted or disabled and an administrator cannot edit their own privileges.
func (s *Service) UpdateUser(
	ctx context.Context,
	targetID, actorID, actorUsername string,
	update UserUpdate,
	clientIP, requestID string,
) (*UserView, error) {
	if update.Role == nil && update.Disabled == nil && update.DisplayName == nil {
		return nil, errors.New("no fields to update")
	}
	if err := s.store.UpdateUserGuarded(ctx, targetID, actorID, update); err != nil {
		return nil, err
	}
	user, err := s.store.GetUserByID(ctx, targetID)
	if err != nil {
		return nil, fmt.Errorf("reload updated user: %w", err)
	}
	audited := false
	if update.Role != nil {
		s.auditSuccess(ctx, EventRoleChanged, actorUsername, user.Username, clientIP, requestID)
		audited = true
	}
	if update.Disabled != nil {
		action := EventUserEnabled
		if *update.Disabled {
			action = EventUserDisabled
		}
		s.auditSuccess(ctx, action, actorUsername, user.Username, clientIP, requestID)
		audited = true
	}
	if !audited {
		s.auditSuccess(ctx, EventUserUpdated, actorUsername, user.Username, clientIP, requestID)
	}
	view := toUserView(user)
	return &view, nil
}

// ResetPassword installs a generated password, forces a change at next login
// and revokes every refresh-token family belonging to the target user.
func (s *Service) ResetPassword(
	ctx context.Context,
	targetID, actorUsername, clientIP, requestID string,
) (string, error) {
	user, err := s.store.GetUserByID(ctx, targetID)
	if err != nil {
		return "", err
	}
	password, err := GenerateTemporaryPasswordLength(s.cfg.PasswordPolicy.MinLength)
	if err != nil {
		return "", err
	}
	if err := s.cfg.PasswordPolicy.Validate(user.Username, password); err != nil {
		return "", fmt.Errorf("%w: %w", ErrWeakPassword, err)
	}
	hash, err := HashPassword(password)
	if err != nil {
		return "", fmt.Errorf("hash temporary password: %w", err)
	}
	if err := s.store.SetPasswordHash(ctx, user.Username, hash); err != nil {
		return "", fmt.Errorf("store temporary password: %w", err)
	}
	if err := s.store.SetMustChangePassword(ctx, user.Username, true); err != nil {
		return "", fmt.Errorf("set password change flag: %w", err)
	}
	if err := s.store.RevokeAllUserRefreshTokens(ctx, user.ID); err != nil {
		return "", fmt.Errorf("revoke user refresh tokens: %w", err)
	}
	s.auditSuccess(ctx, EventPasswordReset, actorUsername, user.Username, clientIP, requestID)
	return password, nil
}
