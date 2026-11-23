package identity

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	migrate "github.com/golang-migrate/migrate/v4"
	migratepgx "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/oxkrypton/Halon/internal/authz"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// ErrNotFound is returned when no identity user exists for the given key.
var ErrNotFound = errors.New("identity user not found")

// Audit actions written to auth_events. Details never contain passwords or
// token material.
const (
	EventLoginSuccess         = "login_success"
	EventLoginFailure         = "login_failure"
	EventUserCreated          = "user_created"
	EventUserDisabled         = "user_disabled"
	EventRoleChanged          = "role_changed"
	EventPasswordReset        = "password_reset"
	EventRefreshReuseDetected = "refresh_reuse_detected"

	EventResultSuccess = "success"
	EventResultFailure = "failure"
)

// Store persists identity users, refresh tokens and audit events in
// PostgreSQL. Table names and the migration version table are prefixed with
// auth_ so this module can share the database with incident and trafficlog.
type Store struct {
	db *gorm.DB
}

// User is one row of auth_users.
type User struct {
	ID                 string     `gorm:"column:id;primaryKey;default:gen_random_uuid()"`
	Username           string     `gorm:"column:username"`
	DisplayName        string     `gorm:"column:display_name"`
	PasswordHash       string     `gorm:"column:password_hash"`
	Role               authz.Role `gorm:"column:role"`
	Disabled           bool       `gorm:"column:disabled"`
	MustChangePassword bool       `gorm:"column:must_change_password"`
	FailedLoginCount   int        `gorm:"column:failed_login_count"`
	LockedUntil        *time.Time `gorm:"column:locked_until"`
	PasswordChangedAt  time.Time  `gorm:"column:password_changed_at"`
	LastLoginAt        *time.Time `gorm:"column:last_login_at"`
	CreatedBy          *string    `gorm:"column:created_by"`
	CreatedAt          time.Time  `gorm:"column:created_at"`
	UpdatedAt          time.Time  `gorm:"column:updated_at"`
}

func (User) TableName() string { return "auth_users" }

// JSONB persists optional structured detail in the auth_events table.
type JSONB json.RawMessage

func (j JSONB) Value() (driver.Value, error) {
	if len(j) == 0 {
		return nil, nil
	}
	if !json.Valid(j) {
		return nil, fmt.Errorf("refuse to store invalid JSON in jsonb column")
	}
	return string(j), nil
}

func (j *JSONB) Scan(value any) error {
	switch v := value.(type) {
	case nil:
		*j = nil
		return nil
	case []byte:
		*j = append((*j)[:0], v...)
		return nil
	case string:
		*j = append((*j)[:0], v...)
		return nil
	default:
		return fmt.Errorf("cannot scan %T into JSONB", value)
	}
}

// AuthEvent is one row of auth_events.
type AuthEvent struct {
	ID             int64     `gorm:"column:id;primaryKey;autoIncrement"`
	At             time.Time `gorm:"column:at;default:NOW()"`
	Action         string    `gorm:"column:action"`
	Result         string    `gorm:"column:result"`
	ActorUsername  string    `gorm:"column:actor_username"`
	TargetUsername string    `gorm:"column:target_username"`
	ClientIP       string    `gorm:"column:client_ip"`
	RequestID      string    `gorm:"column:request_id"`
	Detail         JSONB     `gorm:"column:detail"`
}

func (AuthEvent) TableName() string { return "auth_events" }

// NewStore wraps an existing gorm database handle, primarily for tests.
func NewStore(db *gorm.DB) *Store {
	return &Store{db: db}
}

// OpenStore connects to PostgreSQL with a bounded pool and verifies it.
func OpenStore(ctx context.Context, dsn string) (*Store, error) {
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		TranslateError: true,
		Logger:         gormlogger.Default.LogMode(gormlogger.Warn),
	})
	if err != nil {
		return nil, fmt.Errorf("open postgres for identity: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("access identity postgres pool: %w", err)
	}
	sqlDB.SetMaxOpenConns(10)
	sqlDB.SetMaxIdleConns(3)
	sqlDB.SetConnMaxLifetime(30 * time.Minute)
	sqlDB.SetConnMaxIdleTime(5 * time.Minute)

	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("ping identity postgres: %w", err)
	}
	return &Store{db: db}, nil
}

// Migrate applies the embedded SQL migrations using a dedicated version
// table so incident_schema_migrations is left untouched.
func (s *Store) Migrate() error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return fmt.Errorf("access identity migration connection: %w", err)
	}
	driver, err := migratepgx.WithInstance(sqlDB, &migratepgx.Config{MigrationsTable: "auth_schema_migrations"})
	if err != nil {
		return fmt.Errorf("create identity migration driver: %w", err)
	}
	source, err := iofs.New(migrationFiles, "migrations")
	if err != nil {
		return fmt.Errorf("load embedded identity migrations: %w", err)
	}
	migration, err := migrate.NewWithInstance("iofs", source, "postgres", driver)
	if err != nil {
		return fmt.Errorf("initialize identity migrations: %w", err)
	}
	if err := migration.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("apply identity migrations: %w", err)
	}
	return nil
}

func (s *Store) Ping(ctx context.Context) error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.PingContext(ctx)
}

func (s *Store) Close() error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// SQLDB exposes the underlying pool for integration tests and diagnostics.
func (s *Store) SQLDB() (*sql.DB, error) {
	return s.db.DB()
}

// GetUserByUsername loads the user used by the login flow.
func (s *Store) GetUserByUsername(ctx context.Context, username string) (*User, error) {
	return s.takeUser(ctx, "username = ?", username)
}

// GetUserByID loads a user by primary key.
func (s *Store) GetUserByID(ctx context.Context, id string) (*User, error) {
	return s.takeUser(ctx, "id = ?", id)
}

func (s *Store) takeUser(ctx context.Context, query, value string) (*User, error) {
	var user User
	result := s.db.WithContext(ctx).Where(query, value).Take(&user)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, value)
	}
	if result.Error != nil {
		return nil, fmt.Errorf("get identity user: %w", result.Error)
	}
	return &user, nil
}

// CreateUser inserts a new user. The primary key is generated by PostgreSQL
// and written back to user.ID.
func (s *Store) CreateUser(ctx context.Context, user *User) error {
	if err := s.db.WithContext(ctx).Create(user).Error; err != nil {
		return fmt.Errorf("create identity user: %w", err)
	}
	return nil
}

// CountEnabledFullUsers reports how many active users hold the full role. It
// backs the "do not lock out the last administrator" check.
func (s *Store) CountEnabledFullUsers(ctx context.Context) (int64, error) {
	var count int64
	result := s.db.WithContext(ctx).Model(&User{}).
		Where("role = ? AND disabled = FALSE", string(authz.RoleFull)).
		Count(&count)
	if result.Error != nil {
		return 0, fmt.Errorf("count enabled full users: %w", result.Error)
	}
	return count, nil
}

// SetUserDisabled disables or re-enables a user. Deletion is not supported.
func (s *Store) SetUserDisabled(ctx context.Context, username string, disabled bool) error {
	return s.updateUser(ctx, username, map[string]any{"disabled": disabled})
}

// SetUserRole changes a user's role.
func (s *Store) SetUserRole(ctx context.Context, username string, role authz.Role) error {
	return s.updateUser(ctx, username, map[string]any{"role": string(role)})
}

// SetMustChangePassword marks whether the user must change their password at
// the next login.
func (s *Store) SetMustChangePassword(ctx context.Context, username string, value bool) error {
	return s.updateUser(ctx, username, map[string]any{"must_change_password": value})
}

// SetPasswordHash stores a new password hash and clears failed-login state.
func (s *Store) SetPasswordHash(ctx context.Context, username, hash string) error {
	return s.updateUser(ctx, username, map[string]any{
		"password_hash":       hash,
		"password_changed_at": time.Now().UTC(),
		"failed_login_count":  0,
		"locked_until":        nil,
	})
}

// RecordFailedLogin stores the incremented counter and any backoff deadline.
func (s *Store) RecordFailedLogin(ctx context.Context, username string, failedCount int, lockUntil *time.Time) error {
	return s.updateUser(ctx, username, map[string]any{
		"failed_login_count": failedCount,
		"locked_until":       lockUntil,
	})
}

// RecordSuccessfulLogin clears failed-login state and stamps last_login_at.
func (s *Store) RecordSuccessfulLogin(ctx context.Context, username string, at time.Time) error {
	return s.updateUser(ctx, username, map[string]any{
		"last_login_at":      at,
		"failed_login_count": 0,
		"locked_until":       nil,
	})
}

func (s *Store) updateUser(ctx context.Context, username string, changes map[string]any) error {
	changes["updated_at"] = time.Now().UTC()
	result := s.db.WithContext(ctx).Model(&User{}).Where("username = ?", username).Updates(changes)
	if result.Error != nil {
		return fmt.Errorf("update identity user: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, username)
	}
	return nil
}

// RecordAuthEvent appends one audit row. Callers must never pass passwords or
// refresh tokens in Detail.
func (s *Store) RecordAuthEvent(ctx context.Context, event AuthEvent) error {
	if err := s.db.WithContext(ctx).Create(&event).Error; err != nil {
		return fmt.Errorf("record auth event: %w", err)
	}
	return nil
}
