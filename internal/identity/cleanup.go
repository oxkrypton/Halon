package identity

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

const (
	// CleanupInterval is how often one cleaner wakes up to prune expired
	// auth data. Multi-replica cleaners running together are harmless: the
	// batched DELETEs are idempotent.
	CleanupInterval = time.Hour
	// CleanupBatchSize bounds both the lock time and transaction size of one
	// DELETE round.
	CleanupBatchSize = 1000

	cleanupStopTimeout = time.Second
)

// Cleaner prunes expired refresh tokens and audit events in bounded batches.
// It has a bounded lifecycle: Run returns when its context ends or Close is
// called, and Close waits at most cleanupStopTimeout, so it never holds up
// gateway shutdown.
type Cleaner struct {
	store     *Store
	retention time.Duration
	interval  time.Duration
	batchSize int
	logger    *slog.Logger

	mu        sync.Mutex
	cancel    context.CancelFunc
	done      chan struct{}
	started   bool
	closed    bool
	closeOnce sync.Once
}

func NewCleaner(store *Store, retention time.Duration, logger *slog.Logger) *Cleaner {
	if retention <= 0 {
		retention = 2160 * time.Hour
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Cleaner{
		store: store, retention: retention,
		interval: CleanupInterval, batchSize: CleanupBatchSize,
		logger: logger,
	}
}

// Run sweeps once per interval until the caller's context ends or Close is
// called. Callers start it in a goroutine; Run itself never returns early to
// signal a problem, it logs and waits for the next round instead.
func (c *Cleaner) Run(ctx context.Context) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	c.cancel, c.done, c.started = cancel, done, true
	c.mu.Unlock()
	defer func() {
		cancel()
		close(done)
	}()

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-runCtx.Done():
			return
		case <-ticker.C:
			if err := c.Sweep(runCtx); err != nil && runCtx.Err() == nil {
				c.logger.Warn("auth data cleanup failed", "error", err)
			}
		}
	}
}

// Close stops Run and waits briefly for it to exit. It is safe to call more
// than once and safe to call before Run.
func (c *Cleaner) Close() {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		cancel, done, started := c.cancel, c.done, c.started
		c.mu.Unlock()
		if !started {
			return
		}
		cancel()
		select {
		case <-done:
		case <-time.After(cleanupStopTimeout):
			c.logger.Warn("auth data cleaner shutdown timed out")
		}
	})
}

// Sweep removes expired rows from both auth tables in bounded batches. It is
// also exported so operators can trigger a one-off round from diagnostics.
func (c *Cleaner) Sweep(ctx context.Context) error {
	if err := c.deleteExpiredTokens(ctx); err != nil {
		return err
	}
	return c.deleteExpiredEvents(ctx)
}

func (c *Cleaner) deleteExpiredTokens(ctx context.Context) error {
	for {
		result := c.store.db.WithContext(ctx).Exec(`
			DELETE FROM auth_refresh_tokens
			 WHERE ctid IN (
			       SELECT ctid FROM auth_refresh_tokens
			        WHERE expires_at < NOW()
			        LIMIT $1)`, c.batchSize)
		if result.Error != nil {
			return fmt.Errorf("delete expired refresh tokens: %w", result.Error)
		}
		if result.RowsAffected < int64(c.batchSize) {
			return nil
		}
	}
}

func (c *Cleaner) deleteExpiredEvents(ctx context.Context) error {
	cutoff := time.Now().UTC().Add(-c.retention)
	for {
		result := c.store.db.WithContext(ctx).Exec(`
			DELETE FROM auth_events
			 WHERE ctid IN (
			       SELECT ctid FROM auth_events
			        WHERE at < $1
			        LIMIT $2)`, cutoff, c.batchSize)
		if result.Error != nil {
			return fmt.Errorf("delete expired audit events: %w", result.Error)
		}
		if result.RowsAffected < int64(c.batchSize) {
			return nil
		}
	}
}
