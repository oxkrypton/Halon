package incident

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"time"

	migrate "github.com/golang-migrate/migrate/v4"
	migratepgx "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/jackc/pgx/v5/stdlib"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	gormlogger "gorm.io/gorm/logger"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// ErrNotFound is returned when no report exists for an event ID.
var ErrNotFound = errors.New("incident report not found")

// Store persists incident investigation reports in PostgreSQL.
type Store struct {
	db *gorm.DB
}

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
		return nil, fmt.Errorf("open postgres: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("access postgres connection pool: %w", err)
	}
	sqlDB.SetMaxOpenConns(10)
	sqlDB.SetMaxIdleConns(3)
	sqlDB.SetConnMaxLifetime(30 * time.Minute)
	sqlDB.SetConnMaxIdleTime(5 * time.Minute)

	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Migrate() error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return fmt.Errorf("access migration connection: %w", err)
	}
	// Use a dedicated migrations table so this module can share a database
	// with trafficlog without their version numbers colliding.
	driver, err := migratepgx.WithInstance(sqlDB, &migratepgx.Config{MigrationsTable: "incident_schema_migrations"})
	if err != nil {
		return fmt.Errorf("create migration driver: %w", err)
	}
	source, err := iofs.New(migrationFiles, "migrations")
	if err != nil {
		return fmt.Errorf("load embedded migrations: %w", err)
	}
	migration, err := migrate.NewWithInstance("iofs", source, "postgres", driver)
	if err != nil {
		return fmt.Errorf("initialize migrations: %w", err)
	}
	if err := migration.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("apply migrations: %w", err)
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

// Claim inserts a queued report for the event. It reports false when a
// report with the same event ID already exists, which makes the ingest
// path idempotent for retried webhook deliveries.
func (s *Store) Claim(ctx context.Context, event TimeoutEvent) (bool, error) {
	report := NewReport(event)
	result := s.db.WithContext(ctx).
		Clauses(clause.OnConflict{DoNothing: true}).
		Create(&report)
	if result.Error != nil {
		return false, fmt.Errorf("claim incident report: %w", result.Error)
	}
	return result.RowsAffected == 1, nil
}

// MarkProcessing records that an investigation has started.
func (s *Store) MarkProcessing(ctx context.Context, eventID string, startedAt time.Time) error {
	result := s.db.WithContext(ctx).Model(&Report{}).
		Where("event_id = ? AND status = ?", eventID, StatusQueued).
		Updates(map[string]any{
			"status":     StatusProcessing,
			"started_at": startedAt,
			"updated_at": time.Now().UTC(),
		})
	if result.Error != nil {
		return fmt.Errorf("mark incident processing: %w", result.Error)
	}
	return nil
}

// LoadUnfinished returns every report still in queued or processing status,
// oldest first. At startup these rows are orphans of a previous process: the
// investigation queue is in-memory, so no worker will ever pick them up.
// Safe to re-queue only in single-replica (memory) mode; in shared mode a
// live replica may legitimately own some of these rows.
func (s *Store) LoadUnfinished(ctx context.Context) ([]Report, error) {
	reports := make([]Report, 0)
	result := s.db.WithContext(ctx).
		Where("status IN ?", []ReportStatus{StatusQueued, StatusProcessing}).
		Order("occurred_at ASC").
		Find(&reports)
	if result.Error != nil {
		return nil, fmt.Errorf("load unfinished incident reports: %w", result.Error)
	}
	return reports, nil
}

// MarkFailed closes an unfinished report without running an investigation,
// recording why it was abandoned. Like MarkProcessing it is a conditional
// update: a row that has since completed is left untouched, and zero
// affected rows is not an error.
func (s *Store) MarkFailed(ctx context.Context, eventID, cause string, finishedAt time.Time) error {
	result := s.db.WithContext(ctx).Model(&Report{}).
		Where("event_id = ? AND status IN ?", eventID, []ReportStatus{StatusQueued, StatusProcessing}).
		Updates(map[string]any{
			"status":      StatusFailed,
			"error":       cause,
			"finished_at": finishedAt,
			"updated_at":  time.Now().UTC(),
		})
	if result.Error != nil {
		return fmt.Errorf("mark incident failed: %w", result.Error)
	}
	return nil
}

// SaveResult writes the finished investigation outcome for a claimed event.
func (s *Store) SaveResult(ctx context.Context, report Report) error {
	updates := map[string]any{
		"status":            report.Status,
		"final_message":     report.FinalMessage,
		"steps":             report.Steps,
		"executions":        report.Executions,
		"prompt_tokens":     report.PromptTokens,
		"completion_tokens": report.CompletionTokens,
		"total_tokens":      report.TotalTokens,
		"error":             report.Error,
		"started_at":        report.StartedAt,
		"finished_at":       report.FinishedAt,
		"updated_at":        time.Now().UTC(),
	}
	result := s.db.WithContext(ctx).Model(&Report{}).
		Where("event_id = ?", report.EventID).
		Updates(updates)
	if result.Error != nil {
		return fmt.Errorf("save incident report: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, report.EventID)
	}
	return nil
}

// Get returns the report for one event ID.
func (s *Store) Get(ctx context.Context, eventID string) (*Report, error) {
	var report Report
	result := s.db.WithContext(ctx).Where("event_id = ?", eventID).Take(&report)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, eventID)
	}
	if result.Error != nil {
		return nil, fmt.Errorf("get incident report: %w", result.Error)
	}
	return &report, nil
}

// List returns the most recent reports ordered by occurrence time.
func (s *Store) List(ctx context.Context, limit int) ([]Report, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	reports := make([]Report, 0, limit)
	result := s.db.WithContext(ctx).
		Order("occurred_at DESC").
		Limit(limit).
		Find(&reports)
	if result.Error != nil {
		return nil, fmt.Errorf("list incident reports: %w", result.Error)
	}
	return reports, nil
}
