package trafficlog_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/oxkrypton/Halon/internal/trafficlog"
)

type repositoryStub struct {
	mu      sync.Mutex
	batches [][]trafficlog.Entry
	err     error
	calls   int
}

func (r *repositoryStub) CreateBatch(_ context.Context, entries []trafficlog.Entry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.err != nil {
		return r.err
	}
	batch := append([]trafficlog.Entry(nil), entries...)
	r.batches = append(r.batches, batch)
	return nil
}

func TestBatchWriterFlushesBySizeAndClose(t *testing.T) {
	t.Parallel()

	repository := &repositoryStub{}
	queue := make(chan trafficlog.Entry, 4)
	writer := trafficlog.NewBatchWriter(repository, queue, 2, time.Hour, testLogger())
	done := make(chan struct{})
	go func() {
		writer.Run()
		close(done)
	}()

	queue <- trafficlog.Entry{Base: trafficlog.BaseLog{RequestID: "one"}}
	queue <- trafficlog.Entry{Base: trafficlog.BaseLog{RequestID: "two"}}
	queue <- trafficlog.Entry{Base: trafficlog.BaseLog{RequestID: "three"}}
	close(queue)
	waitForWriter(t, done)

	if len(repository.batches) != 2 {
		t.Fatalf("batch count = %d, want 2", len(repository.batches))
	}
	if len(repository.batches[0]) != 2 || len(repository.batches[1]) != 1 {
		t.Fatalf("batch sizes = %d and %d, want 2 and 1", len(repository.batches[0]), len(repository.batches[1]))
	}
}

func TestBatchWriterFlushesByInterval(t *testing.T) {
	t.Parallel()

	repository := &repositoryStub{}
	queue := make(chan trafficlog.Entry, 1)
	writer := trafficlog.NewBatchWriter(repository, queue, 10, 10*time.Millisecond, testLogger())
	done := make(chan struct{})
	go func() {
		writer.Run()
		close(done)
	}()

	queue <- trafficlog.Entry{Base: trafficlog.BaseLog{RequestID: "timed"}}

	deadline := time.Now().Add(time.Second)
	for {
		repository.mu.Lock()
		calls := repository.calls
		repository.mu.Unlock()
		if calls > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for interval flush")
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(queue)
	waitForWriter(t, done)
}

func TestBatchWriterRetriesFailedBatch(t *testing.T) {
	t.Parallel()

	repository := &repositoryStub{err: errors.New("database unavailable")}
	queue := make(chan trafficlog.Entry, 1)
	writer := trafficlog.NewBatchWriter(repository, queue, 1, time.Hour, testLogger())
	done := make(chan struct{})
	go func() {
		writer.Run()
		close(done)
	}()

	queue <- trafficlog.Entry{Base: trafficlog.BaseLog{RequestID: "retry"}}
	close(queue)
	waitForWriter(t, done)
	if repository.calls != 3 {
		t.Fatalf("CreateBatch calls = %d, want 3", repository.calls)
	}
}

func waitForWriter(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("batch writer did not stop")
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
