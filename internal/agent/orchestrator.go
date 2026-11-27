package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/oxkrypton/Halon/internal/incident"
)

// ErrQueueFull is returned when too many incidents wait for investigation.
var ErrQueueFull = errors.New("agent orchestrator queue is full")

// ErrOrchestratorClosed is returned when Submit is called after Close.
var ErrOrchestratorClosed = errors.New("agent orchestrator is closed")

// SubmitOutcome classifies how Submit handled one incoming incident event.
type SubmitOutcome string

const (
	SubmitAccepted   SubmitOutcome = "accepted"
	SubmitDuplicate  SubmitOutcome = "duplicate"
	SubmitSuppressed SubmitOutcome = "suppressed"
)

// SubmitResult reports the admission decision for one incident event.
type SubmitResult struct {
	Outcome SubmitOutcome
	// IncidentID is event.EventID for accepted/duplicate events, or the active
	// session's event ID when Outcome is SubmitSuppressed.
	IncidentID string
}

// ReportStore is the persistence contract the orchestrator needs.
type ReportStore interface {
	Claim(context.Context, incident.Event) (bool, error)
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
	// RecoveryMaxAge bounds how old a queued incident may be for startup
	// recovery to re-queue it; older ones are marked failed.
	RecoveryMaxAge time.Duration
	Metrics        *Metrics
	Logger         *slog.Logger
}

type sessionKey struct {
	eventType incident.EventType
	scopeKey  string
}

func sessionKeyFor(event incident.Event) sessionKey {
	eventType := event.Type
	if eventType == "" {
		eventType = incident.TypeUpstreamTimeout
	}
	return sessionKey{
		eventType: eventType,
		scopeKey:  event.EffectiveScopeKey(),
	}
}

type activeSession struct {
	key          sessionKey
	snapshot     SessionSnapshot
	cancel       context.CancelFunc
	cancelled    bool
	cancelReason string
}

// Orchestrator manages single-instance investigation sessions: it admits
// validated incident events, enforces at most one active session per
// (event_type, scope_key), executes bounded worker sessions with cancellation
// support, and recovers unfinished rows safely on startup.
type Orchestrator struct {
	loop           *Loop
	store          ReportStore
	level          OperationLevel
	runTimeout     time.Duration
	recoveryMaxAge time.Duration
	metrics        *Metrics
	queue          chan incident.Event
	workers        int
	logger         *slog.Logger

	mu          sync.Mutex
	closed      bool
	sessions    map[string]*activeSession
	activeByKey map[sessionKey]string
	wg          sync.WaitGroup
	closeOnce   sync.Once
	done        chan struct{}
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
	if options.Level == "" {
		options.Level = LevelReadOnly
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	return &Orchestrator{
		loop:           loop,
		store:          store,
		level:          options.Level,
		runTimeout:     options.RunTimeout,
		recoveryMaxAge: options.RecoveryMaxAge,
		metrics:        options.Metrics,
		queue:          make(chan incident.Event, options.QueueSize),
		workers:        options.Workers,
		logger:         options.Logger,
		sessions:       make(map[string]*activeSession),
		activeByKey:    make(map[sessionKey]string),
		done:           make(chan struct{}),
	}
}

// Submit validates an incoming incident, enforces one active session per
// (event_type, scope_key), claims the event in the report store, and enqueues
// an investigation session.
func (o *Orchestrator) Submit(ctx context.Context, event incident.Event) (SubmitResult, error) {
	if err := event.Validate(); err != nil {
		return SubmitResult{}, fmt.Errorf("invalid incident event: %w", err)
	}
	event.ScopeKey = event.EffectiveScopeKey()
	key := sessionKeyFor(event)

	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return SubmitResult{}, ErrOrchestratorClosed
	}
	if activeID, active := o.activeByKey[key]; active {
		o.mu.Unlock()
		if activeID == event.EventID {
			return SubmitResult{Outcome: SubmitDuplicate, IncidentID: event.EventID}, nil
		}
		o.logger.Info("incident suppressed by active session",
			"event_id", event.EventID, "incident_id", activeID, "event_type", key.eventType, "scope_key", key.scopeKey)
		return SubmitResult{Outcome: SubmitSuppressed, IncidentID: activeID}, nil
	}
	// Reserve the (event_type, scope_key) slot across the database claim to
	// serialize concurrent submissions on the same fault scope.
	o.activeByKey[key] = event.EventID
	o.mu.Unlock()

	claimed, err := o.store.Claim(ctx, event)
	if err != nil {
		o.releaseSlot(key, event.EventID)
		return SubmitResult{}, err
	}
	if !claimed {
		o.releaseSlot(key, event.EventID)
		return SubmitResult{Outcome: SubmitDuplicate, IncidentID: event.EventID}, nil
	}

	o.mu.Lock()
	if o.closed {
		if o.activeByKey[key] == event.EventID {
			delete(o.activeByKey, key)
		}
		o.mu.Unlock()
		_ = o.failUnqueued(ctx, event, ErrOrchestratorClosed.Error())
		return SubmitResult{}, ErrOrchestratorClosed
	}
	select {
	case o.queue <- event:
		o.registerSessionLocked(key, event)
		o.mu.Unlock()
		return SubmitResult{Outcome: SubmitAccepted, IncidentID: event.EventID}, nil
	default:
		if o.activeByKey[key] == event.EventID {
			delete(o.activeByKey, key)
		}
		o.mu.Unlock()
		saveErr := o.failUnqueued(ctx, event, ErrQueueFull.Error())
		if saveErr != nil {
			return SubmitResult{}, errors.Join(ErrQueueFull, saveErr)
		}
		return SubmitResult{}, ErrQueueFull
	}
}

// ListSessions returns snapshots of all queued, running, and cancelling
// investigation sessions sorted by queue time.
func (o *Orchestrator) ListSessions() []SessionSnapshot {
	o.mu.Lock()
	snapshots := make([]SessionSnapshot, 0, len(o.sessions))
	for _, sess := range o.sessions {
		snapshots = append(snapshots, sess.snapshot)
	}
	o.mu.Unlock()

	sort.Slice(snapshots, func(i, j int) bool {
		if !snapshots[i].QueuedAt.Equal(snapshots[j].QueuedAt) {
			return snapshots[i].QueuedAt.Before(snapshots[j].QueuedAt)
		}
		return snapshots[i].SessionID < snapshots[j].SessionID
	})
	return snapshots
}

// CancelSession cancels one active queued or running investigation session.
func (o *Orchestrator) CancelSession(ctx context.Context, eventID string) error {
	const cause = "investigation cancelled by operator"
	o.mu.Lock()
	sess, ok := o.sessions[eventID]
	if !ok {
		o.mu.Unlock()
		return ErrSessionNotFound
	}
	if sess.snapshot.State == SessionQueued {
		key := sess.key
		delete(o.sessions, eventID)
		if o.activeByKey[key] == eventID {
			delete(o.activeByKey, key)
		}
		o.metrics.SetActiveSessions(len(o.sessions))
		o.mu.Unlock()
		return o.store.MarkFailed(ctx, eventID, cause, time.Now().UTC())
	}
	sess.cancelled = true
	sess.cancelReason = cause
	sess.snapshot.State = SessionCancelling
	sess.snapshot.UpdatedAt = time.Now().UTC()
	cancelFn := sess.cancel
	o.mu.Unlock()
	if cancelFn != nil {
		cancelFn()
	}
	return nil
}

// CancelActive aborts all in-flight and queued sessions during shutdown drain
// timeout.
func (o *Orchestrator) CancelActive() {
	const cause = "agent shut down before investigation completed"
	o.mu.Lock()
	var cancels []context.CancelFunc
	for _, sess := range o.sessions {
		sess.cancelled = true
		sess.cancelReason = cause
		if sess.snapshot.State == SessionRunning {
			sess.snapshot.State = SessionCancelling
			sess.snapshot.UpdatedAt = time.Now().UTC()
			if sess.cancel != nil {
				cancels = append(cancels, sess.cancel)
			}
		}
	}
	o.mu.Unlock()
	for _, cancelFn := range cancels {
		cancelFn()
	}
}

// RecoverUnfinished reconciles orphaned rows from a previous single-instance
// process at startup. Rows already in processing status may have executed
// partial mutations and are closed as failed; queued rows younger than
// RecoveryMaxAge are re-queued, while older ones are marked failed.
func (o *Orchestrator) RecoverUnfinished(ctx context.Context) (requeued, failed int, err error) {
	reports, err := o.store.LoadUnfinished(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("load unfinished incidents: %w", err)
	}
	cutoff := time.Now().Add(-o.recoveryMaxAge)
	for _, report := range reports {
		now := time.Now().UTC()
		if report.Status == incident.StatusProcessing {
			if failErr := o.store.MarkFailed(ctx, report.EventID, "agent interrupted during investigation", now); failErr != nil {
				return requeued, failed, fmt.Errorf("close interrupted incident %s: %w", report.EventID, failErr)
			}
			failed++
			continue
		}
		if report.OccurredAt.Before(cutoff) {
			if failErr := o.store.MarkFailed(ctx, report.EventID, "incident expired while the agent was down", now); failErr != nil {
				return requeued, failed, fmt.Errorf("expire incident %s: %w", report.EventID, failErr)
			}
			failed++
			continue
		}

		event := report.Event()
		key := sessionKeyFor(event)
		o.mu.Lock()
		if o.closed {
			o.mu.Unlock()
			return requeued, failed, ErrOrchestratorClosed
		}
		if _, active := o.activeByKey[key]; active {
			o.mu.Unlock()
			if failErr := o.store.MarkFailed(ctx, report.EventID, "superseded by an active session on the same event type and scope during startup recovery", now); failErr != nil {
				return requeued, failed, fmt.Errorf("supersede incident %s: %w", report.EventID, failErr)
			}
			failed++
			continue
		}
		select {
		case o.queue <- event:
			o.activeByKey[key] = event.EventID
			o.registerSessionLocked(key, event)
			o.mu.Unlock()
			requeued++
			o.logger.Info("re-queued unfinished incident", "event_id", report.EventID, "event_type", key.eventType, "scope_key", key.scopeKey)
		default:
			o.mu.Unlock()
			if failErr := o.store.MarkFailed(ctx, report.EventID, ErrQueueFull.Error(), now); failErr != nil {
				return requeued, failed, fmt.Errorf("fail overflowed recovery incident %s: %w", report.EventID, failErr)
			}
			failed++
		}
	}
	return requeued, failed, nil
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

// Close stops accepting work, closes the queue, and waits for workers to drain.
func (o *Orchestrator) Close() {
	o.closeOnce.Do(func() {
		o.mu.Lock()
		o.closed = true
		close(o.queue)
		o.mu.Unlock()
	})
	<-o.done
}

func (o *Orchestrator) work() {
	defer o.wg.Done()
	for event := range o.queue {
		o.investigate(event)
	}
}

func (o *Orchestrator) investigate(event incident.Event) {
	started := time.Now().UTC()
	runCtx, cancel := context.WithTimeout(context.Background(), o.runTimeout)
	key := sessionKeyFor(event)

	o.mu.Lock()
	sess, active := o.sessions[event.EventID]
	if !active {
		o.mu.Unlock()
		cancel()
		return
	}
	if sess.cancelled {
		reason := sess.cancelReason
		delete(o.sessions, event.EventID)
		if o.activeByKey[key] == event.EventID {
			delete(o.activeByKey, key)
		}
		o.metrics.SetActiveSessions(len(o.sessions))
		o.mu.Unlock()
		cancel()
		saveCtx, saveCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer saveCancel()
		_ = o.store.MarkFailed(saveCtx, event.EventID, reason, time.Now().UTC())
		return
	}
	sess.snapshot.State = SessionRunning
	sess.snapshot.StartedAt = &started
	sess.snapshot.UpdatedAt = started
	sess.cancel = cancel
	o.mu.Unlock()

	o.logger.Info("incident investigation started",
		"event_id", event.EventID, "event_type", key.eventType, "scope_key", key.scopeKey,
		"route", event.Route, "path", event.Path, "occurrences", max(1, event.Occurrences), "level", o.level)
	if err := o.store.MarkProcessing(context.Background(), event.EventID, started); err != nil {
		o.logger.Error("mark incident processing", "event_id", event.EventID, "error", err)
	}

	result, runErr := o.loop.RunWithProgress(runCtx, event, o.level, func(step int) {
		o.mu.Lock()
		if current, ok := o.sessions[event.EventID]; ok {
			current.snapshot.CurrentStep = step
			current.snapshot.UpdatedAt = time.Now().UTC()
		}
		o.mu.Unlock()
	})
	cancel()

	o.mu.Lock()
	wasCancelled := false
	cancelReason := ""
	if current, ok := o.sessions[event.EventID]; ok {
		wasCancelled = current.cancelled
		cancelReason = current.cancelReason
		delete(o.sessions, event.EventID)
	}
	if o.activeByKey[key] == event.EventID {
		delete(o.activeByKey, key)
	}
	o.metrics.SetActiveSessions(len(o.sessions))
	o.mu.Unlock()

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
	switch {
	case wasCancelled:
		report.Status = incident.StatusFailed
		report.Error = cancelReason
	case runErr != nil:
		report.Status = incident.StatusFailed
		report.Error = runErr.Error()
	default:
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
		"event_type", key.eventType,
		"scope_key", key.scopeKey,
		"status", report.Status,
		"steps", report.Steps,
		"duration_ms", finished.Sub(started).Milliseconds(),
		"error", report.Error,
	)
}

func (o *Orchestrator) registerSessionLocked(key sessionKey, event incident.Event) {
	now := time.Now().UTC()
	occurrences := event.Occurrences
	if occurrences <= 0 {
		occurrences = 1
	}
	maxSteps := 0
	if o.loop != nil {
		maxSteps = o.loop.maxSteps
	}
	o.sessions[event.EventID] = &activeSession{
		key: key,
		snapshot: SessionSnapshot{
			SessionID:   event.EventID,
			EventType:   key.eventType,
			ScopeKey:    key.scopeKey,
			Route:       event.Route,
			Path:        event.Path,
			Occurrences: occurrences,
			State:       SessionQueued,
			MaxSteps:    maxSteps,
			QueuedAt:    now,
			UpdatedAt:   now,
		},
	}
	o.metrics.SetActiveSessions(len(o.sessions))
}

func (o *Orchestrator) releaseSlot(key sessionKey, eventID string) {
	o.mu.Lock()
	if o.activeByKey[key] == eventID {
		delete(o.activeByKey, key)
	}
	o.mu.Unlock()
}

func (o *Orchestrator) failUnqueued(ctx context.Context, event incident.Event, cause string) error {
	// Never leave a claimed row stuck in queued when the event is not enqueued.
	report := incident.NewReport(event)
	report.Status = incident.StatusFailed
	report.Error = cause
	finished := time.Now().UTC()
	report.FinishedAt = &finished
	return o.store.SaveResult(ctx, report)
}
