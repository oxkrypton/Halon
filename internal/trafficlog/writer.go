package trafficlog

import (
	"context"
	"log/slog"
	"time"
)

const (
	flushTimeout = 5 * time.Second
	flushRetries = 3
)

type BatchRepository interface {
	CreateBatch(context.Context, []Entry) error
}

// BatchWriter converts the bounded logging queue into timed PostgreSQL batches.
// Logging is best-effort: failed batches are retried and then discarded so a
// database outage cannot indefinitely block request processing or shutdown.
type BatchWriter struct {
	repository    BatchRepository
	queue         <-chan Entry
	batchSize     int
	flushInterval time.Duration
	logger        *slog.Logger
}

func NewBatchWriter(repository BatchRepository, queue <-chan Entry, batchSize int, flushInterval time.Duration, logger *slog.Logger) *BatchWriter {
	if logger == nil {
		logger = slog.Default()
	}
	return &BatchWriter{
		repository:    repository,
		queue:         queue,
		batchSize:     batchSize,
		flushInterval: flushInterval,
		logger:        logger,
	}
}

// Run consumes entries until the queue is closed and drained.
func (w *BatchWriter) Run() {
	ticker := time.NewTicker(w.flushInterval)
	defer ticker.Stop()

	batch := make([]Entry, 0, w.batchSize)
	for {
		select {
		case entry, ok := <-w.queue:
			if !ok {
				w.flush(batch)
				return
			}
			batch = append(batch, entry)
			if len(batch) >= w.batchSize {
				w.flush(batch)
				batch = make([]Entry, 0, w.batchSize)
			}
		case <-ticker.C:
			if len(batch) > 0 {
				w.flush(batch)
				batch = make([]Entry, 0, w.batchSize)
			}
		}
	}
}

func (w *BatchWriter) flush(batch []Entry) {
	if len(batch) == 0 {
		return
	}

	var err error
	for attempt := 0; attempt < flushRetries; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), flushTimeout)
		err = w.repository.CreateBatch(ctx, batch)
		cancel()
		if err == nil {
			return
		}
		if attempt < flushRetries-1 {
			time.Sleep(time.Duration(1<<attempt) * 100 * time.Millisecond)
		}
	}
	w.logger.Error("discarding traffic log batch after retries", "entries", len(batch), "error", err)
}
