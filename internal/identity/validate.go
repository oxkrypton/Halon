package identity

import (
	"errors"
	"fmt"
	"regexp"
)

var (
	// ErrInvalidUsername marks a username that violates the auth_users CHECK
	// constraint. Validating at the HTTP/config boundary keeps the error stable
	// across PostgreSQL versions.
	ErrInvalidUsername = errors.New("invalid username")
	// ErrInvalidUserID marks a user identifier that is not a valid UUID.
	ErrInvalidUserID = errors.New("invalid user id")
)

var (
	usernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,63}$`)
	uuidPattern     = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// ValidateUsername applies the same 3..64 character lowercase pattern as the
// auth_users table. Callers should wrap this error with their own API error.
func ValidateUsername(username string) error {
	if !usernamePattern.MatchString(username) {
		return fmt.Errorf("%w: %q must be 3..64 lowercase letters, digits, dots, dashes or underscores", ErrInvalidUsername, username)
	}
	return nil
}

// ValidateUUID rejects identifiers that are not canonical 36-character UUIDs.
func ValidateUUID(value string) error {
	if !uuidPattern.MatchString(value) {
		return fmt.Errorf("%w: must be a valid UUID", ErrInvalidUserID)
	}
	return nil
}
