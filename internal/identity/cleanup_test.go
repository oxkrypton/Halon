package identity

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func newCleanerTestStore(t *testing.T) (*Cleaner, sqlmock.Sqlmock) {
	t.Helper()
	sqlDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	gormDB, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{
		DisableAutomaticPing: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cleaner := NewCleaner(NewStore(gormDB), time.Hour, logger)
	cleaner.batchSize = 2
	return cleaner, mock
}

func TestSweepDeletesExpiredDataInBatches(t *testing.T) {
	t.Parallel()
	cleaner, mock := newCleanerTestStore(t)
	// A full batch triggers another round; a short batch ends it.
	mock.ExpectExec(`DELETE FROM auth_refresh_tokens WHERE ctid IN`).
		WithArgs(2).WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(`DELETE FROM auth_refresh_tokens WHERE ctid IN`).
		WithArgs(2).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`DELETE FROM auth_events WHERE ctid IN`).
		WithArgs(sqlmock.AnyArg(), 2).WillReturnResult(sqlmock.NewResult(0, 0))

	if err := cleaner.Sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSweepStopsAtFirstError(t *testing.T) {
	t.Parallel()
	cleaner, mock := newCleanerTestStore(t)
	mock.ExpectExec(`DELETE FROM auth_refresh_tokens WHERE ctid IN`).
		WithArgs(2).WillReturnError(context.DeadlineExceeded)

	err := cleaner.Sweep(context.Background())
	if err == nil || !strings.Contains(err.Error(), "delete expired refresh tokens") {
		t.Fatalf("sweep error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCleanerRunCloseDoesNotBlock(t *testing.T) {
	t.Parallel()
	cleaner, _ := newCleanerTestStore(t)
	done := make(chan struct{})
	go func() {
		cleaner.Run(context.Background())
		close(done)
	}()

	start := time.Now()
	cleaner.Close()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Close() blocked instead of stopping Run()")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("Close() took %v", elapsed)
	}
	// Close is safe to repeat and never deadlocks.
	cleaner.Close()
}

func TestCleanerStopsWhenContextCancelled(t *testing.T) {
	t.Parallel()
	cleaner, _ := newCleanerTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		cleaner.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run() did not stop after context cancellation")
	}
}

func TestCleanerCloseBeforeRun(t *testing.T) {
	t.Parallel()
	cleaner, _ := newCleanerTestStore(t)
	cleaner.Close()
	done := make(chan struct{})
	go func() {
		cleaner.Run(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run() ignored a prior Close()")
	}
}
