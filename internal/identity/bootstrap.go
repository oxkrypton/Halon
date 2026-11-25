package identity

import (
	"context"
	"errors"
	"fmt"

	"github.com/oxkrypton/Halon/internal/authz"
	"gorm.io/gorm"
)

// BootstrapAdmin creates the first full-role administrator when auth_users is
// completely empty. The whole decision lives in one INSERT ... WHERE NOT
// EXISTS statement: several replicas may start simultaneously, but only the
// one that actually inserts a row reports success. The username UNIQUE
// constraint covers the rare race where both replicas pass the NOT EXISTS
// check; the loser is treated as a no-op rather than a startup failure.
//
// The caller logs at most one line and must never log the password.
func (s *Store) BootstrapAdmin(ctx context.Context, username, password string, policy PasswordPolicy) (bool, error) {
	if err := ValidateUsername(username); err != nil {
		return false, err
	}
	if err := policy.Validate(username, password); err != nil {
		return false, fmt.Errorf("%w: %w", ErrWeakPassword, err)
	}
	hash, err := HashPassword(password)
	if err != nil {
		return false, fmt.Errorf("hash bootstrap password: %w", err)
	}
	result := s.db.WithContext(ctx).Exec(`
		INSERT INTO auth_users (username, password_hash, role, must_change_password)
		SELECT $1, $2, $3, TRUE
		 WHERE NOT EXISTS (SELECT 1 FROM auth_users)`,
		username, hash, string(authz.RoleFull))
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrDuplicatedKey) {
			return false, nil
		}
		return false, fmt.Errorf("bootstrap administrator: %w", result.Error)
	}
	return result.RowsAffected == 1, nil
}
