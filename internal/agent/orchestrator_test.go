package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oxkrypton/Halon/internal/incident"
)

type stubReportStore struct {
	mu                sync.Mutex
	claims            int
	saved             []incident.Report
	processing        []string
	failed            map[string]string
	unfinished        []incident.Report
	loadUnfinishedErr error
	claimExists       bool
	claimErr          error
}

func (s *stubReportStore) Claim(_ context.Context, event incident.Event) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claims++
	if s.claimErr != nil {
		return false, s.claimErr
	}
	return !s.claimExists, nil
}

func (s *stubReportStore) claimCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.claims
}

func (s *stubReportStore) MarkProcessing(_ context.Context, eventID string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.processing = append(s.processing, eventID)
	return nil
}

func (s *stubReportStore) SaveResult(_ context.Context, report incident.Report) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saved = append(s.saved, report)
	return nil
}

func (s *stubReportStore) LoadUnfinished(context.Context) ([]incident.Report, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadUnfinishedErr != nil {
		return nil, s.loadUnfinishedErr
	}
	return append([]incident.Report(nil), s.unfinished...), nil
}

func (s *stubReportStore) MarkFailed(_ context.Context, eventID, cause string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed == nil {
		s.failed = make(map[string]string)
	}
	s.failed[eventID] = cause
	return nil
}

func (s *stubReportStore) failedReports() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.failed))
	for id, cause := range s.failed {
		out[id] = cause
	}
	return out
}

func (s *stubReportStore) savedReports() []incident.Report {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]incident.Report(nil), s.saved...)
}

func newTestOrchestrator(model Model, store ReportStore, queueSize int) *Orchestrator {
	return newTestOrchestratorWithOptions(model, store, OrchestratorOptions{
		QueueSize: queueSize,
	})
}

func newTestOrchestratorWithOptions(model Model, store ReportStore, opts OrchestratorOptions) *Orchestrator {
	registry, _ := NewRegistry(LevelReadOnly, Tool{
		Name: "read", Description: "Read", Handler: func(context.Context, json.RawMessage) (any, error) {
			return map[string]bool{"ok": true}, nil
		},
	})
	loop := NewLoop(model, registry, 3, 1024, 4096, testLogger())
	if opts.Level == "" {
		opts.Level = LevelReadOnly
	}
	if opts.Workers == 0 {
		opts.Workers = 1
	}
	if opts.QueueSize == 0 {
		opts.QueueSize = 8
	}
	if opts.RunTimeout == 0 {
		opts.RunTimeout = 5 * time.Second
	}
	opts.Logger = testLogger()
	return NewOrchestrator(loop, store, opts)
}

func finalMessageModel(text string) Model {
	return modelFunc(func(context.Context, []Message, []ToolDefinition) (Message, Usage, error) {
		return NewTextMessage("assistant", text), Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}, nil
	})
}

func TestOrchestratorCompletesInvestigation(t *testing.T) {
	store := &stubReportStore{}
	orchestrator := newTestOrchestrator(finalMessageModel("## 事件摘要\n已完成"), store, 4)
	go orchestrator.Run()
	defer orchestrator.Close()

	result, err := orchestrator.Submit(context.Background(), testEvent())
	if err != nil || result.Outcome != SubmitAccepted {
		t.Fatalf("submit = %+v, err %v", result, err)
	}
	waitReports(t, store, 1)
	report := store.savedReports()[0]
	if report.Status != incident.StatusCompleted || report.FinalMessage == "" || report.TotalTokens != 2 {
		t.Fatalf("unexpected report: %+v", report)
	}
	if report.StartedAt == nil || report.FinishedAt == nil {
		t.Fatalf("report must carry investigation timestamps: %+v", report)
	}
}

func TestOrchestratorMarksFailedRuns(t *testing.T) {
	store := &stubReportStore{}
	model := modelFunc(func(context.Context, []Message, []ToolDefinition) (Message, Usage, error) {
		return Message{}, Usage{}, errors.New("model unavailable")
	})
	orchestrator := newTestOrchestrator(model, store, 4)
	go orchestrator.Run()
	defer orchestrator.Close()

	if _, err := orchestrator.Submit(context.Background(), testEvent()); err != nil {
		t.Fatal(err)
	}
	waitReports(t, store, 1)
	reports := store.savedReports()
	if reports[0].Status != incident.StatusFailed || !strings.Contains(reports[0].Error, "model unavailable") {
		t.Fatalf("unexpected report: %+v", reports[0])
	}
}

func TestOrchestratorSkipsDuplicateClaims(t *testing.T) {
	store := &stubReportStore{claimExists: true}
	orchestrator := newTestOrchestrator(finalMessageModel("ok"), store, 4)

	result, err := orchestrator.Submit(context.Background(), testEvent())
	if err != nil || result.Outcome != SubmitDuplicate {
		t.Fatalf("submit = %+v, err %v, want duplicate", result, err)
	}
	if remaining := len(orchestrator.queue); remaining != 0 {
		t.Fatalf("duplicate event must not be queued, queue depth = %d", remaining)
	}
}

func TestOrchestratorRejectsInvalidEvents(t *testing.T) {
	store := &stubReportStore{}
	orchestrator := newTestOrchestrator(finalMessageModel("ok"), store, 4)

	event := testEvent()
	event.EventID = ""
	if _, err := orchestrator.Submit(context.Background(), event); err == nil {
		t.Fatal("invalid event must be rejected before claiming")
	}
}

func TestOrchestratorReportsFullQueue(t *testing.T) {
	store := &stubReportStore{}
	orchestrator := newTestOrchestrator(finalMessageModel("ok"), store, 1)

	first := testEvent()
	first.ScopeKey = "/api/v1/users/"
	first.Route = "/api/v1/users/"
	second := testEvent()
	second.EventID = "event-2"
	second.ScopeKey = "/api/v1/orders/"
	second.Route = "/api/v1/orders/"
	if _, err := orchestrator.Submit(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	// Workers are not running: the second claim overflows the queue.
	_, err := orchestrator.Submit(context.Background(), second)
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("submit error = %v, want ErrQueueFull", err)
	}
	reports := store.savedReports()
	if len(reports) != 1 || reports[0].Status != incident.StatusFailed {
		t.Fatalf("overflowed claim must be persisted as failed: %+v", reports)
	}
}

func TestOrchestratorSuppressesConcurrentActiveRouteSession(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	model := modelFunc(func(ctx context.Context, _ []Message, _ []ToolDefinition) (Message, Usage, error) {
		select {
		case <-started:
		default:
			close(started)
		}
		<-release
		return NewTextMessage("assistant", "ok"), Usage{}, nil
	})

	store := &stubReportStore{}
	orchestrator := newTestOrchestratorWithOptions(model, store, OrchestratorOptions{
		Workers:   2,
		QueueSize: 8,
	})
	go orchestrator.Run()
	defer orchestrator.Close()

	evt1 := testEvent()
	evt1.EventID = "ord-0"
	evt1.Route = "/api/v1/orders/"
	res1, err := orchestrator.Submit(context.Background(), evt1)
	if err != nil || res1.Outcome != SubmitAccepted {
		t.Fatalf("first submit = (%+v, %v)", res1, err)
	}
	<-started

	// Same (event_type, scope_key) while session is active -> suppressed pointing to ord-0.
	evt2 := evt1
	evt2.EventID = "ord-1"
	res2, err := orchestrator.Submit(context.Background(), evt2)
	if err != nil || res2.Outcome != SubmitSuppressed || res2.IncidentID != "ord-0" {
		t.Fatalf("concurrent same (type, scope_key) = (%+v, %v), want suppressed -> ord-0", res2, err)
	}

	// Same route but DIFFERENT event_type is admitted independently.
	diffType := evt1
	diffType.EventID = "ord-err-0"
	diffType.Type = incident.TypeUpstreamError
	diffType.StatusCode = 502
	resDiffType, err := orchestrator.Submit(context.Background(), diffType)
	if err != nil || resDiffType.Outcome != SubmitAccepted {
		t.Fatalf("different event type on same route = (%+v, %v), want accepted", resDiffType, err)
	}

	// Different route is admitted independently.
	other := evt1
	other.EventID = "usr-0"
	other.Route = "/api/v1/users/"
	other.ScopeKey = "/api/v1/users/"
	resOther, err := orchestrator.Submit(context.Background(), other)
	if err != nil || resOther.Outcome != SubmitAccepted {
		t.Fatalf("other route = (%+v, %v), want accepted", resOther, err)
	}
	if claims := store.claimCount(); claims != 3 {
		t.Fatalf("Claim calls = %d, want 3 (suppressed events must not hit DB)", claims)
	}

	close(release)
	waitReports(t, store, 3)

	// Once ord-0 completes, a new session on /api/v1/orders/ is accepted.
	deadline := time.Now().Add(time.Second)
	for len(orchestrator.ListSessions()) > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	evt3 := evt1
	evt3.EventID = "ord-2"
	res3, err := orchestrator.Submit(context.Background(), evt3)
	if err != nil || res3.Outcome != SubmitAccepted {
		t.Fatalf("after completion = (%+v, %v), want accepted", res3, err)
	}
	waitReports(t, store, 4)
}

func TestOrchestratorListsAndCancelsSessions(t *testing.T) {
	runningCh := make(chan struct{})
	model := modelFunc(func(ctx context.Context, _ []Message, _ []ToolDefinition) (Message, Usage, error) {
		select {
		case <-runningCh:
		default:
			close(runningCh)
		}
		<-ctx.Done()
		return Message{}, Usage{}, ctx.Err()
	})

	store := &stubReportStore{}
	orchestrator := newTestOrchestratorWithOptions(model, store, OrchestratorOptions{
		Workers:   1,
		QueueSize: 4,
	})
	go orchestrator.Run()
	defer orchestrator.Close()

	evtRun := testEvent()
	evtRun.EventID = "sess-run"
	evtRun.ScopeKey = "/api/v1/orders/"
	evtRun.Route = "/api/v1/orders/"
	evtRun.Occurrences = 4
	if _, err := orchestrator.Submit(context.Background(), evtRun); err != nil {
		t.Fatal(err)
	}
	<-runningCh

	evtQueue := testEvent()
	evtQueue.EventID = "sess-queue"
	evtQueue.ScopeKey = "/api/v1/users/"
	evtQueue.Route = "/api/v1/users/"
	if _, err := orchestrator.Submit(context.Background(), evtQueue); err != nil {
		t.Fatal(err)
	}

	sessions := orchestrator.ListSessions()
	if len(sessions) != 2 || sessions[0].SessionID != "sess-run" || sessions[0].EventType != incident.TypeUpstreamTimeout || sessions[0].State != SessionRunning || sessions[0].Occurrences != 4 || sessions[1].SessionID != "sess-queue" || sessions[1].State != SessionQueued {
		t.Fatalf("unexpected ListSessions: %+v", sessions)
	}

	// Cancel queued session first.
	if err := orchestrator.CancelSession(context.Background(), "sess-queue"); err != nil {
		t.Fatalf("cancel queued session: %v", err)
	}
	if cause := store.failedReports()["sess-queue"]; !strings.Contains(cause, "cancelled by operator") {
		t.Fatalf("queued cancel failure cause = %q", cause)
	}

	// Cancel running session.
	if err := orchestrator.CancelSession(context.Background(), "sess-run"); err != nil {
		t.Fatalf("cancel running session: %v", err)
	}
	waitReports(t, store, 1)
	if rep := store.savedReports()[0]; rep.Status != incident.StatusFailed || !strings.Contains(rep.Error, "cancelled by operator") {
		t.Fatalf("running cancel report = %+v", rep)
	}

	if err := orchestrator.CancelSession(context.Background(), "missing"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("cancel missing = %v, want ErrSessionNotFound", err)
	}
}

func TestOrchestratorCancelActiveAbortsRunningSession(t *testing.T) {
	runningCh := make(chan struct{})
	model := modelFunc(func(ctx context.Context, _ []Message, _ []ToolDefinition) (Message, Usage, error) {
		close(runningCh)
		<-ctx.Done()
		return Message{}, Usage{}, ctx.Err()
	})
	store := &stubReportStore{}
	orchestrator := newTestOrchestrator(model, store, 2)
	done := make(chan struct{})
	go func() {
		orchestrator.Run()
		close(done)
	}()

	if _, err := orchestrator.Submit(context.Background(), testEvent()); err != nil {
		t.Fatal(err)
	}
	<-runningCh

	go orchestrator.Close()
	orchestrator.CancelActive()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("CancelActive did not abort running session in time")
	}
	waitReports(t, store, 1)
	if rep := store.savedReports()[0]; rep.Status != incident.StatusFailed || !strings.Contains(rep.Error, "shut down") {
		t.Fatalf("unexpected shutdown report: %+v", rep)
	}
}

func TestOrchestratorRecoverUnfinished(t *testing.T) {
	now := time.Now().UTC()
	base := testEvent()
	mkReport := func(id, route string, status incident.ReportStatus, occurred time.Time) incident.Report {
		e := base
		e.EventID = id
		e.ScopeKey = route
		e.Route = route
		e.OccurredAt = occurred
		r := incident.NewReport(e)
		r.Status = status
		return r
	}

	store := &stubReportStore{
		unfinished: []incident.Report{
			mkReport("proc-1", "/api/v1/a/", incident.StatusProcessing, now.Add(-2*time.Minute)),
			mkReport("expired-1", "/api/v1/b/", incident.StatusQueued, now.Add(-2*time.Hour)),
			mkReport("queued-fresh-1", "/api/v1/c/", incident.StatusQueued, now.Add(-5*time.Minute)),
			mkReport("queued-dup-route", "/api/v1/c/", incident.StatusQueued, now.Add(-3*time.Minute)),
		},
	}

	orchestrator := newTestOrchestratorWithOptions(finalMessageModel("recovered"), store, OrchestratorOptions{
		Workers:        1,
		QueueSize:      8,
		RecoveryMaxAge: 30 * time.Minute,
	})
	go orchestrator.Run()
	defer orchestrator.Close()

	requeued, failed, err := orchestrator.RecoverUnfinished(context.Background())
	if err != nil {
		t.Fatalf("RecoverUnfinished: %v", err)
	}
	if requeued != 1 || failed != 3 {
		t.Fatalf("requeued=%d failed=%d, want 1/3", requeued, failed)
	}

	failedMap := store.failedReports()
	if !strings.Contains(failedMap["proc-1"], "interrupted") {
		t.Fatalf("processing row cause = %q", failedMap["proc-1"])
	}
	if !strings.Contains(failedMap["expired-1"], "expired") {
		t.Fatalf("expired row cause = %q", failedMap["expired-1"])
	}
	if !strings.Contains(failedMap["queued-dup-route"], "superseded") {
		t.Fatalf("duplicate-route row cause = %q", failedMap["queued-dup-route"])
	}

	waitReports(t, store, 1)
	if rep := store.savedReports()[0]; rep.EventID != "queued-fresh-1" || rep.Status != incident.StatusCompleted {
		t.Fatalf("recovered report = %+v", rep)
	}
}

func waitReports(t *testing.T, store *stubReportStore, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for len(store.savedReports()) < want {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d saved reports (got %d)", want, len(store.savedReports()))
		}
		time.Sleep(5 * time.Millisecond)
	}
}
