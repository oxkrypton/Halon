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

// ErrOrchestratorClosed is returned when Submit is called after Close.
var ErrOrchestratorClosed = errors.New("agent orchestrator is closed")

// SubmitOutcome classifies how Submit handled one incoming timeout event.
type SubmitOutcome string

const (
	SubmitAccepted   SubmitOutcome = "accepted"
	SubmitDuplicate  SubmitOutcome = "duplicate"
	SubmitSuppressed SubmitOutcome = "suppressed"
)

// SubmitResult reports the admission decision for one timeout event.
type SubmitResult struct {
	Outcome SubmitOutcome
	// IncidentID is event.EventID for accepted/duplicate events, or the active
	// window owner's event ID when Outcome is SubmitSuppressed.
	IncidentID string
}

// ReportStore is the persistence contract the orchestrator needs.
type ReportStore interface {
	Claim(context.Context, incident.TimeoutEvent) (bool, error)
	MarkProcessing(ctx context.Context, eventID string, startedAt time.Time) error
	SaveResult(context.Context, incident.Report) error
	LoadUnfinished(ctx context.Context) ([]incident.Report, error)
	MarkFailed(ctx context.Context, eventID, cause string, finishedAt time.Time) error
}

type OrchestratorOptions struct {
	Level      OperationLevel
	Workers    int
	QueueSize  int
	RunTimeout time.Duration
	// RecoveryMaxAge bounds how old an unfinished incident may be for
	// startup recovery to re-queue it; older ones are marked failed.
	RecoveryMaxAge time.Duration
	// CoalesceCooldown suppresses subsequent timeouts on the same route after
	// a completed investigation. Non-positive disables route suppression.
	CoalesceCooldown time.Duration
	// RetryBackoff shortens the suppression window after a failed run or
	// full-queue rejection so persistent outages are re-investigated sooner.
	RetryBackoff time.Duration
	// SettleDelay waits before enqueuing a newly accepted incident so gateway
	// traffic logs and Prometheus scrapes have time to land. Non-positive
	// enqueues immediately.
	SettleDelay time.Duration
	// Suppressor arbitrates per-route windows when CoalesceCooldown > 0.
	// Nil selects the single-replica MemoryRouteSuppressor.
	Suppressor RouteSuppressor
	Logger     *slog.Logger
}

type pendingEvent struct {
	event incident.TimeoutEvent
	timer *time.Timer
}

// Orchestrator accepts validated timeout events, optionally suppresses storms
// per route and waits a settle delay, claims them in the report store, and
// investigates them sequentially per worker.
type Orchestrator struct {
	loop             *Loop
	store            ReportStore
	level            OperationLevel
	runTimeout       time.Duration
	recoveryMaxAge   time.Duration
	coalesceCooldown time.Duration
	retryBackoff     time.Duration
	settleDelay      time.Duration
	suppressor       RouteSuppressor
	queue            chan incident.TimeoutEvent
	workers          int
	logger           *slog.Logger

	mu        sync.Mutex
	closed    bool
	pending   map[string]*pendingEvent
	wg        sync.WaitGroup
	closeOnce sync.Once
	done      chan struct{}
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
	if options.RecoveryMaxAge <= 0 {
		options.RecoveryMaxAge = 30 * time.Minute
	}
	if options.CoalesceCooldown < 0 {
		options.CoalesceCooldown = 0
	}
	if options.RetryBackoff < 0 {
		options.RetryBackoff = 0
	}
	if options.SettleDelay < 0 {
		options.SettleDelay = 0
	}
	if options.Level == "" {
		options.Level = LevelReadOnly
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	var suppressor RouteSuppressor
	if options.CoalesceCooldown > 0 {
		suppressor = options.Suppressor
		if suppressor == nil {
			suppressor = NewMemoryRouteSuppressor()
		}
	}
	return &Orchestrator{
		loop:             loop,
		store:            store,
		level:            options.Level,
		runTimeout:       options.RunTimeout,
		recoveryMaxAge:   options.RecoveryMaxAge,
		coalesceCooldown: options.CoalesceCooldown,
		retryBackoff:     options.RetryBackoff,
		settleDelay:      options.SettleDelay,
		suppressor:       suppressor,
		queue:            make(chan incident.TimeoutEvent, options.QueueSize),
		workers:          options.Workers,
		logger:           options.Logger,
		pending:          make(map[string]*pendingEvent),
		done:             make(chan struct{}),
	}
}

// Submit validates, optionally checks the per-route suppression window, claims
// the event in the report store, and enqueues (or schedules after SettleDelay)
// an investigation.
func (o *Orchestrator) Submit(ctx context.Context, event incident.TimeoutEvent) (SubmitResult, error) {
	if err := event.Validate(); err != nil {
		return SubmitResult{}, fmt.Errorf("invalid timeout event: %w", err)
	}

	acquiredWindow := false
	if o.suppressor != nil {
		acquired, owner, err := o.suppressor.TryAcquire(ctx, event.Route, event.EventID, o.initialWindowTTL())
		if err != nil {
			o.logger.Warn("route suppression check failed; proceeding without suppression",
				"route", event.Route, "event_id", event.EventID, "error", err)
		} else if !acquired {
			if owner == event.EventID {
				return SubmitResult{Outcome: SubmitDuplicate, IncidentID: event.EventID}, nil
			}
			o.logger.Info("timeout event suppressed by active route window",
				"event_id", event.EventID, "incident_id", owner, "route", event.Route)
			return SubmitResult{Outcome: SubmitSuppressed, IncidentID: owner}, nil
		} else {
			acquiredWindow = true
		}
	}

	claimed, err := o.store.Claim(ctx, event)
	if err != nil {
		if acquiredWindow {
			o.releaseSuppression(event.Route, event.EventID)
		}
		return SubmitResult{}, err
	}
	if !claimed {
		if acquiredWindow {
			o.releaseSuppression(event.Route, event.EventID)
		}
		return SubmitResult{Outcome: SubmitDuplicate, IncidentID: event.EventID}, nil
	}

	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		_ = o.failUnqueued(ctx, event, ErrOrchestratorClosed.Error())
		if acquiredWindow {
			o.releaseSuppression(event.Route, event.EventID)
		}
		return SubmitResult{}, ErrOrchestratorClosed
	}
	if len(o.pending)+len(o.queue) >= cap(o.queue) {
		o.mu.Unlock()
		saveErr := o.failUnqueued(ctx, event, ErrQueueFull.Error())
		if acquiredWindow {
			o.setSuppressionTTL(event.Route, event.EventID, o.retryBackoff)
		}
		if saveErr != nil {
			return SubmitResult{}, errors.Join(ErrQueueFull, saveErr)
		}
		return SubmitResult{}, ErrQueueFull
	}
	if o.settleDelay > 0 {
		p := &pendingEvent{event: event}
		o.pending[event.EventID] = p
		p.timer = time.AfterFunc(o.settleDelay, func() {
			o.flushPending(event.EventID)
		})
		o.mu.Unlock()
		o.logger.Info("incident scheduled after settle delay",
			"event_id", event.EventID, "route", event.Route, "settle_delay", o.settleDelay)
		return SubmitResult{Outcome: SubmitAccepted, IncidentID: event.EventID}, nil
	}
	select {
	case o.queue <- event:
		o.mu.Unlock()
		return SubmitResult{Outcome: SubmitAccepted, IncidentID: event.EventID}, nil
	default:
		o.mu.Unlock()
		saveErr := o.failUnqueued(ctx, event, ErrQueueFull.Error())
		if acquiredWindow {
			o.setSuppressionTTL(event.Route, event.EventID, o.retryBackoff)
		}
		if saveErr != nil {
			return SubmitResult{}, errors.Join(ErrQueueFull, saveErr)
		}
		return SubmitResult{}, ErrQueueFull
	}
}

func (o *Orchestrator) flushPending(eventID string) {
	o.mu.Lock()
	p, ok := o.pending[eventID]
	if !ok || o.closed {
		o.mu.Unlock()
		return
	}
	delete(o.pending, eventID)
	select {
	case o.queue <- p.event:
		o.mu.Unlock()
		return
	default:
		o.mu.Unlock()
	}

	// Queue filled up before the settle timer elapsed: close the report so it
	// is never left stuck in queued and shorten the suppression window.
	saveCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := o.failUnqueued(saveCtx, p.event, ErrQueueFull.Error()); err != nil {
		o.logger.Error("persist settled queue-full report", "event_id", p.event.EventID, "error", err)
	}
	o.setSuppressionTTL(p.event.Route, p.event.EventID, o.retryBackoff)
	o.logger.Warn("settled incident dropped because queue is full", "event_id", p.event.EventID, "route", p.event.Route)
}

func (o *Orchestrator) failUnqueued(ctx context.Context, event incident.TimeoutEvent, cause string) error {
	// Never leave a claimed row stuck in queued when the event is not enqueued.
	report := incident.NewReport(event)
	report.Status = incident.StatusFailed
	report.Error = cause
	finished := time.Now().UTC()
	report.FinishedAt = &finished
	return o.store.SaveResult(ctx, report)
}

// RecoverUnfinished re-queues incidents that a previous process claimed but
// never finished: at startup the in-memory queue is empty, so every row
// still in queued/processing status is an orphan no worker will pick up.
// Rows younger than RecoveryMaxAge are re-queued for investigation; older
// ones are marked failed so they do not look perpetually in-flight.
//
// Single-replica (memory) mode only: in shared mode a live replica may
// legitimately own some of these rows, so callers must skip recovery until
// lease-based claiming exists.
//
// Workers must be running (Run) before calling this, because re-queueing
// blocks when the queue is full. Partial progress is reported on error; the
// next startup retries the rest.
func (o *Orchestrator) RecoverUnfinished(ctx context.Context) (requeued, expired int, err error) {
	reports, err := o.store.LoadUnfinished(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("load unfinished incidents: %w", err)
	}
	cutoff := time.Now().Add(-o.recoveryMaxAge)
	for _, report := range reports {
		if report.OccurredAt.Before(cutoff) {
			finished := time.Now().UTC()
			if failErr := o.store.MarkFailed(ctx, report.EventID, "incident expired while the agent was down", finished); failErr != nil {
				return requeued, expired, fmt.Errorf("expire incident %s: %w", report.EventID, failErr)
			}
			expired++
			continue
		}
		event := report.Event()
		if o.suppressor != nil {
			// Best-effort: reserve the route window so new timeouts arriving
			// right after startup do not kick off a parallel investigation.
			_, _, _ = o.suppressor.TryAcquire(ctx, event.Route, event.EventID, o.initialWindowTTL())
		}
		select {
		case o.queue <- event:
			requeued++
			o.logger.Info("re-queued unfinished incident", "event_id", report.EventID, "status", report.Status)
		case <-ctx.Done():
			return requeued, expired, ctx.Err()
		}
	}
	return requeued, expired, nil
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

// Close stops accepting work, cancels pending settle-delay timers (marking
// their reports failed and releasing their suppression windows), and waits
// for the queue to drain.
func (o *Orchestrator) Close() {
	o.closeOnce.Do(func() {
		o.mu.Lock()
		o.closed = true
		abandoned := make([]incident.TimeoutEvent, 0, len(o.pending))
		for id, p := range o.pending {
			p.timer.Stop()
			abandoned = append(abandoned, p.event)
			delete(o.pending, id)
		}
		close(o.queue)
		o.mu.Unlock()

		if len(abandoned) > 0 {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			finished := time.Now().UTC()
			for _, event := range abandoned {
				if err := o.store.MarkFailed(ctx, event.EventID, "agent shut down before investigation started", finished); err != nil {
					o.logger.Error("mark abandoned settled incident failed", "event_id", event.EventID, "error", err)
				}
				o.releaseSuppression(event.Route, event.EventID)
			}
		}
	})
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
	o.setSuppressionTTL(event.Route, event.EventID, o.runTimeout+o.coalesceCooldown)

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
	if runErr != nil {
		o.setSuppressionTTL(event.Route, event.EventID, o.retryBackoff)
	} else {
		o.setSuppressionTTL(event.Route, event.EventID, o.coalesceCooldown)
	}
	o.logger.Info("incident investigation finished",
		"event_id", event.EventID,
		"status", report.Status,
		"steps", report.Steps,
		"duration_ms", finished.Sub(started).Milliseconds(),
		"error", report.Error,
	)
}

func (o *Orchestrator) initialWindowTTL() time.Duration {
	return o.settleDelay + o.runTimeout + o.coalesceCooldown
}

func (o *Orchestrator) setSuppressionTTL(route, eventID string, ttl time.Duration) {
	if o.suppressor == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := o.suppressor.SetTTL(ctx, route, eventID, ttl); err != nil {
		o.logger.Warn("update route suppression ttl", "route", route, "event_id", eventID, "error", err)
	}
}

func (o *Orchestrator) releaseSuppression(route, eventID string) {
	if o.suppressor == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := o.suppressor.Release(ctx, route, eventID); err != nil {
		o.logger.Warn("release route suppression window", "route", route, "event_id", eventID, "error", err)
	}
}
