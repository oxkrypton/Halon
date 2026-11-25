package identity

import (
	"errors"
	"fmt"
	"regexp"
)

// ErrInvalidUsername marks a username that violates the auth_users CHECK
// constraint. Validating at the HTTP/config boundary keeps the error stable
// across PostgreSQL versions.
var ErrInvalidUsername = errors.New("invalid username")

var usernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,63}$`)

// ValidateUsername applies the same 3..64 character lowercase pattern as the
// auth_users table. Callers should wrap this error with their own API error.
func ValidateUsername(username string) error {
	if !usernamePattern.MatchString(username) {
		return fmt.Errorf("%w: %q must be 3..64 lowercase letters, digits, dots, dashes or underscores", ErrInvalidUsername, username)
	}
	return nil
}
