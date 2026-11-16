package trafficlog

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
	gormlogger "gorm.io/gorm/logger"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Repository stores request logs in PostgreSQL.
type Repository struct {
	db *gorm.DB
}

func OpenPostgres(ctx context.Context, dsn string) (*Repository, error) {
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
	sqlDB.SetMaxOpenConns(20)
	sqlDB.SetMaxIdleConns(5)
	sqlDB.SetConnMaxLifetime(30 * time.Minute)
	sqlDB.SetConnMaxIdleTime(5 * time.Minute)

	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return &Repository{db: db}, nil
}

func (r *Repository) Migrate() error {
	sqlDB, err := r.db.DB()
	if err != nil {
		return fmt.Errorf("access migration connection: %w", err)
	}
	driver, err := migratepgx.WithInstance(sqlDB, &migratepgx.Config{})
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

func (r *Repository) Ping(ctx context.Context) error {
	sqlDB, err := r.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.PingContext(ctx)
}

func (r *Repository) Close() error {
	sqlDB, err := r.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// CreateBatch writes base rows and their optional details atomically.
func (r *Repository) CreateBatch(ctx context.Context, entries []Entry) error {
	if len(entries) == 0 {
		return nil
	}

	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		baseRows := make([]BaseLog, len(entries))
		for i := range entries {
			baseRows[i] = entries[i].Base
		}
		if err := tx.Create(&baseRows).Error; err != nil {
			return err
		}

		details := make([]ErrorBody, 0)
		for i := range entries {
			if entries[i].Detail == nil {
				continue
			}
			detail := *entries[i].Detail
			detail.BaseLogID = baseRows[i].ID
			details = append(details, detail)
		}
		if len(details) > 0 {
			if err := tx.Create(&details).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// SQLDB exposes the underlying pool for integration tests and diagnostics.
func (r *Repository) SQLDB() (*sql.DB, error) {
	return r.db.DB()
}
