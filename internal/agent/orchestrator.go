package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/oxkrypton/Halon/internal/incident"
)

// ErrQueueFull is returned when too many incidents wait for investigation.
var ErrQueueFull = errors.New("agent orchestrator queue is full")

// ReportStore is the persistence contract the orchestrator needs.
type ReportStore interface {
	Claim(context.Context, incident.TimeoutEvent) (bool, error)
	MarkProcessing(ctx context.Context, eventID string, startedAt time.Time) error
	SaveResult(context.Context, incident.Report) error
}

type OrchestratorOptions struct {
	Level      OperationLevel
	Workers    int
	QueueSize  int
	RunTimeout time.Duration
	Logger     *slog.Logger
}

// Orchestrator accepts validated timeout events, claims them in the
// report store, and investigates them sequentially per worker.
type Orchestrator struct {
	loop       *Loop
	store      ReportStore
	level      OperationLevel
	runTimeout time.Duration
	queue      chan incident.TimeoutEvent
	workers    int
	logger     *slog.Logger
	wg         sync.WaitGroup
	closeOnce  sync.Once
	done       chan struct{}
}

func NewOrchestrator(loop *Loop, store ReportStore, options OrchestratorOptions) *Orchestrator {
	if options.Workers <= 0 {
		options.Workers = 2
	}
	if options.QueueSize <= 0 {
		options.QueueSize = 64
	}
	if options.RunTimeout <= 0 {
		options.RunTimeout = 5 * time.Minute
	}
	if options.Level == "" {
		options.Level = LevelReadOnly
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	return &Orchestrator{
		loop:       loop,
		store:      store,
		level:      options.Level,
		runTimeout: options.RunTimeout,
		queue:      make(chan incident.TimeoutEvent, options.QueueSize),
		workers:    options.Workers,
		logger:     options.Logger,
		done:       make(chan struct{}),
	}
}

// Submit validates and claims the event, then queues an investigation.
// It reports duplicate=true when the event was already claimed.
func (o *Orchestrator) Submit(ctx context.Context, event incident.TimeoutEvent) (duplicate bool, err error) {
	if err := event.Validate(); err != nil {
		return false, fmt.Errorf("invalid timeout event: %w", err)
	}
	claimed, err := o.store.Claim(ctx, event)
	if err != nil {
		return false, err
	}
	if !claimed {
		return true, nil
	}
	select {
	case o.queue <- event:
		return false, nil
	default:
		// Never leave a claimed row stuck in queued when the queue is full.
		report := incident.NewReport(event)
		report.Status = incident.StatusFailed
		report.Error = ErrQueueFull.Error()
		finished := time.Now().UTC()
		report.FinishedAt = &finished
		if saveErr := o.store.SaveResult(ctx, report); saveErr != nil {
			return false, errors.Join(ErrQueueFull, saveErr)
		}
		return false, ErrQueueFull
	}
}

// Run starts the worker pool and blocks until Close is called and all
// queued investigations have finished.
func (o *Orchestrator) Run() {
	o.wg.Add(o.workers)
	for i := 0; i < o.workers; i++ {
		go o.work()
	}
	o.wg.Wait()
	close(o.done)
}

// Close stops accepting work and waits for the queue to drain.
func (o *Orchestrator) Close() {
	o.closeOnce.Do(func() { close(o.queue) })
	<-o.done
}

func (o *Orchestrator) work() {
	defer o.wg.Done()
	for event := range o.queue {
		o.investigate(event)
	}
}

func (o *Orchestrator) investigate(event incident.TimeoutEvent) {
	started := time.Now().UTC()
	o.logger.Info("incident investigation started", "event_id", event.EventID, "path", event.Path, "level", o.level)
	if err := o.store.MarkProcessing(context.Background(), event.EventID, started); err != nil {
		o.logger.Error("mark incident processing", "event_id", event.EventID, "error", err)
	}

	runCtx, cancel := context.WithTimeout(context.Background(), o.runTimeout)
	result, runErr := o.loop.Run(runCtx, event, o.level)
	cancel()

	finished := time.Now().UTC()
	report := incident.NewReport(event)
	report.StartedAt = &started
	report.FinishedAt = &finished
	report.Steps = result.Steps
	report.PromptTokens = result.Usage.PromptTokens
	report.CompletionTokens = result.Usage.CompletionTokens
	report.TotalTokens = result.Usage.TotalTokens
	if len(result.Executions) > 0 {
		if encoded, err := json.Marshal(result.Executions); err == nil {
			report.Executions = incident.JSONB(encoded)
		}
	}
	if runErr != nil {
		report.Status = incident.StatusFailed
		report.Error = runErr.Error()
	} else {
		report.Status = incident.StatusCompleted
		report.FinalMessage = result.FinalMessage
	}

	// Persist on a fresh context: the run context may already be cancelled.
	saveCtx, saveCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer saveCancel()
	if err := o.store.SaveResult(saveCtx, report); err != nil {
		o.logger.Error("persist incident report", "event_id", event.EventID, "error", err)
	}
	o.logger.Info("incident investigation finished",
		"event_id", event.EventID,
		"status", report.Status,
		"steps", report.Steps,
		"duration_ms", finished.Sub(started).Milliseconds(),
		"error", report.Error,
	)
}
