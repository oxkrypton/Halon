package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/oxkrypton/Halon/internal/incident"
)

type stubReportStore struct {
	mu          sync.Mutex
	claims      int
	saved       []incident.Report
	processing  []string
	failed      map[string]string
	claimExists bool
	claimErr    error
}

func (s *stubReportStore) Claim(_ context.Context, event incident.TimeoutEvent) (bool, error) {
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
	registry, _ := NewRegistry(LevelReadOnly, Tool{
		Name: "read", Description: "Read", Handler: func(context.Context, json.RawMessage) (any, error) {
			return map[string]bool{"ok": true}, nil
		},
	})
	loop := NewLoop(model, registry, 3, 1024, 4096, testLogger())
	return NewOrchestrator(loop, store, OrchestratorOptions{
		Level: LevelReadOnly, Workers: 1, QueueSize: queueSize,
		RunTimeout: 5 * time.Second, Logger: testLogger(),
	})
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

	deadline := time.Now().Add(2 * time.Second)
	for {
		reports := store.savedReports()
		if len(reports) == 1 {
			report := reports[0]
			if report.Status != incident.StatusCompleted || report.FinalMessage == "" || report.TotalTokens != 2 {
				t.Fatalf("unexpected report: %+v", report)
			}
			if report.StartedAt == nil || report.FinishedAt == nil {
				t.Fatalf("report must carry investigation timestamps: %+v", report)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("investigation did not finish in time")
		}
		time.Sleep(5 * time.Millisecond)
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
	deadline := time.Now().Add(2 * time.Second)
	for {
		reports := store.savedReports()
		if len(reports) == 1 {
			if reports[0].Status != incident.StatusFailed || !strings.Contains(reports[0].Error, "model unavailable") {
				t.Fatalf("unexpected report: %+v", reports[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("failed run was not persisted in time")
		}
		time.Sleep(5 * time.Millisecond)
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
	second := testEvent()
	second.EventID = "event-2"
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

func TestOrchestratorSuppressesRouteStorm(t *testing.T) {
	mr := miniredis.RunT(t)
	store := &stubReportStore{}
	orchestrator := newTestOrchestratorWithOptions(finalMessageModel("ok"), store, OrchestratorOptions{
		CoalesceCooldown: 5 * time.Minute,
		RetryBackoff:     time.Minute,
		Suppressor:       newTestRedisRouteSuppressor(t, mr, nil),
	})
	go orchestrator.Run()
	defer orchestrator.Close()

	var accepted, suppressed int
	for i := 0; i < 50; i++ {
		evt := testEvent()
		evt.EventID = fmt.Sprintf("ord-%d", i)
		evt.Route = "/api/v1/orders/"
		evt.Path = fmt.Sprintf("/api/v1/orders/%d", i)
		res, err := orchestrator.Submit(context.Background(), evt)
		if err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
		switch res.Outcome {
		case SubmitAccepted:
			accepted++
		case SubmitSuppressed:
			suppressed++
			if res.IncidentID != "ord-0" {
				t.Fatalf("suppressed event %d pointed to %q, want ord-0", i, res.IncidentID)
			}
		default:
			t.Fatalf("unexpected outcome %s", res.Outcome)
		}
	}

	// A different route is admitted independently.
	other := testEvent()
	other.EventID = "usr-0"
	other.Route = "/api/v1/users/"
	res, err := orchestrator.Submit(context.Background(), other)
	if err != nil || res.Outcome != SubmitAccepted {
		t.Fatalf("other route submit = (%+v, %v), want accepted", res, err)
	}

	if accepted != 1 || suppressed != 49 {
		t.Fatalf("orders storm: accepted=%d suppressed=%d, want 1/49", accepted, suppressed)
	}
	if claims := store.claimCount(); claims != 2 {
		t.Fatalf("Claim calls = %d, want 2 (suppressed events must not hit DB)", claims)
	}

	deadline := time.Now().Add(2 * time.Second)
	for len(store.savedReports()) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("expected 2 saved reports, got %d", len(store.savedReports()))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestOrchestratorCooldownAndRetryBackoff(t *testing.T) {
	mr := miniredis.RunT(t)
	suppressor := newTestRedisRouteSuppressor(t, mr, nil)

	var failNext atomic.Bool
	failNext.Store(true)
	model := modelFunc(func(context.Context, []Message, []ToolDefinition) (Message, Usage, error) {
		if failNext.Load() {
			return Message{}, Usage{}, errors.New("llm timeout")
		}
		return NewTextMessage("assistant", "ok"), Usage{}, nil
	})

	store := &stubReportStore{}
	orchestrator := newTestOrchestratorWithOptions(model, store, OrchestratorOptions{
		CoalesceCooldown: 5 * time.Minute,
		RetryBackoff:     time.Minute,
		Suppressor:       suppressor,
	})
	go orchestrator.Run()
	defer orchestrator.Close()

	// 1) First run fails -> window shrinks to RetryBackoff (1m).
	evt1 := testEvent()
	evt1.EventID = "evt-fail"
	if res, err := orchestrator.Submit(context.Background(), evt1); err != nil || res.Outcome != SubmitAccepted {
		t.Fatalf("evt1 = (%+v, %v)", res, err)
	}
	waitReports(t, store, 1)
	waitRouteTTL(t, mr, evt1.Route, func(ttl time.Duration) bool { return ttl > 0 && ttl <= time.Minute })

	// Within 1m backoff, another event is suppressed.
	mr.FastForward(30 * time.Second)
	evtBlocked := testEvent()
	evtBlocked.EventID = "evt-blocked-backoff"
	if res, err := orchestrator.Submit(context.Background(), evtBlocked); err != nil || res.Outcome != SubmitSuppressed {
		t.Fatalf("within backoff: (%+v, %v), want suppressed", res, err)
	}

	// Past 1m backoff (still < 5m cooldown), next event is accepted and succeeds.
	failNext.Store(false)
	mr.FastForward(40 * time.Second)
	evt2 := testEvent()
	evt2.EventID = "evt-ok"
	if res, err := orchestrator.Submit(context.Background(), evt2); err != nil || res.Outcome != SubmitAccepted {
		t.Fatalf("after backoff: (%+v, %v), want accepted", res, err)
	}
	waitReports(t, store, 2)
	waitRouteTTL(t, mr, evt2.Route, func(ttl time.Duration) bool { return ttl > time.Minute && ttl <= 5*time.Minute })

	// After success, window stays closed for full 5m cooldown.
	mr.FastForward(2 * time.Minute)
	evtBlockedCooldown := testEvent()
	evtBlockedCooldown.EventID = "evt-blocked-cooldown"
	if res, err := orchestrator.Submit(context.Background(), evtBlockedCooldown); err != nil || res.Outcome != SubmitSuppressed {
		t.Fatalf("within cooldown: (%+v, %v), want suppressed", res, err)
	}

	// Past 5m cooldown, a fresh investigation opens.
	mr.FastForward(4 * time.Minute)
	evt3 := testEvent()
	evt3.EventID = "evt-reopen"
	if res, err := orchestrator.Submit(context.Background(), evt3); err != nil || res.Outcome != SubmitAccepted {
		t.Fatalf("after cooldown: (%+v, %v), want accepted", res, err)
	}
	waitReports(t, store, 3)
}

func TestOrchestratorReleasesWindowOnClaimFailureOrDuplicate(t *testing.T) {
	mr := miniredis.RunT(t)
	store := &stubReportStore{claimErr: errors.New("db hiccup")}
	orchestrator := newTestOrchestratorWithOptions(finalMessageModel("ok"), store, OrchestratorOptions{
		CoalesceCooldown: 5 * time.Minute,
		Suppressor:       newTestRedisRouteSuppressor(t, mr, nil),
	})

	evt1 := testEvent()
	evt1.EventID = "evt-1"
	if _, err := orchestrator.Submit(context.Background(), evt1); err == nil {
		t.Fatal("expected claim error")
	}

	// DB recovers, but next event is an old duplicate (claimed=false).
	store.mu.Lock()
	store.claimErr = nil
	store.claimExists = true
	store.mu.Unlock()
	evt2 := testEvent()
	evt2.EventID = "evt-2"
	if res, err := orchestrator.Submit(context.Background(), evt2); err != nil || res.Outcome != SubmitDuplicate {
		t.Fatalf("duplicate submit = (%+v, %v), want duplicate", res, err)
	}

	// Neither failure nor duplicate should leave the route suppressed.
	store.mu.Lock()
	store.claimExists = false
	store.mu.Unlock()
	evt3 := testEvent()
	evt3.EventID = "evt-3"
	if res, err := orchestrator.Submit(context.Background(), evt3); err != nil || res.Outcome != SubmitAccepted {
		t.Fatalf("subsequent submit = (%+v, %v), want accepted", res, err)
	}
}

func TestOrchestratorSettleDelay(t *testing.T) {
	mr := miniredis.RunT(t)
	store := &stubReportStore{}
	orchestrator := newTestOrchestratorWithOptions(finalMessageModel("ok"), store, OrchestratorOptions{
		CoalesceCooldown: 5 * time.Minute,
		SettleDelay:      60 * time.Millisecond,
		Suppressor:       newTestRedisRouteSuppressor(t, mr, nil),
	})
	go orchestrator.Run()
	defer orchestrator.Close()

	started := time.Now()
	evt := testEvent()
	if res, err := orchestrator.Submit(context.Background(), evt); err != nil || res.Outcome != SubmitAccepted {
		t.Fatalf("submit = (%+v, %v)", res, err)
	}
	// Another event during settle delay is suppressed.
	evt2 := testEvent()
	evt2.EventID = "evt-during-settle"
	if res, err := orchestrator.Submit(context.Background(), evt2); err != nil || res.Outcome != SubmitSuppressed {
		t.Fatalf("during settle = (%+v, %v), want suppressed", res, err)
	}
	// Investigation must not complete before settle delay elapses.
	waitReports(t, store, 1)
	if elapsed := time.Since(started); elapsed < 50*time.Millisecond {
		t.Fatalf("investigation finished too early: %v < 50ms", elapsed)
	}
}

func TestOrchestratorCloseCancelsPendingSettleAndReleasesWindow(t *testing.T) {
	mr := miniredis.RunT(t)
	store := &stubReportStore{}
	suppressor := newTestRedisRouteSuppressor(t, mr, nil)
	orchestrator := newTestOrchestratorWithOptions(finalMessageModel("ok"), store, OrchestratorOptions{
		CoalesceCooldown: 5 * time.Minute,
		SettleDelay:      10 * time.Second,
		Suppressor:       suppressor,
	})
	go orchestrator.Run()

	evt := testEvent()
	if res, err := orchestrator.Submit(context.Background(), evt); err != nil || res.Outcome != SubmitAccepted {
		t.Fatalf("submit = (%+v, %v)", res, err)
	}

	closeStart := time.Now()
	orchestrator.Close()
	if elapsed := time.Since(closeStart); elapsed > time.Second {
		t.Fatalf("Close waited for settle timer (%v)", elapsed)
	}

	if cause := store.failedReports()[evt.EventID]; !strings.Contains(cause, "shut down") {
		t.Fatalf("abandoned pending event was not marked failed: %v", store.failedReports())
	}
	if acquired, _, err := suppressor.TryAcquire(context.Background(), evt.Route, "evt-next", time.Minute); err != nil || !acquired {
		t.Fatalf("route window was not released on Close: acquired=%v err=%v", acquired, err)
	}
}

func TestOrchestratorSettlePendingCountsTowardQueueCap(t *testing.T) {
	store := &stubReportStore{}
	orchestrator := newTestOrchestratorWithOptions(finalMessageModel("ok"), store, OrchestratorOptions{
		QueueSize:   1,
		SettleDelay: 10 * time.Second,
	})
	go orchestrator.Run()
	defer orchestrator.Close()

	evt1 := testEvent()
	evt1.EventID = "evt-1"
	if res, err := orchestrator.Submit(context.Background(), evt1); err != nil || res.Outcome != SubmitAccepted {
		t.Fatalf("first submit = (%+v, %v)", res, err)
	}
	evt2 := testEvent()
	evt2.EventID = "evt-2"
	if _, err := orchestrator.Submit(context.Background(), evt2); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("second submit err = %v, want ErrQueueFull", err)
	}
}

func TestOrchestratorMultiReplicaSharedSuppression(t *testing.T) {
	mr := miniredis.RunT(t)
	storeA, storeB := &stubReportStore{}, &stubReportStore{}
	orchA := newTestOrchestratorWithOptions(finalMessageModel("ok"), storeA, OrchestratorOptions{
		CoalesceCooldown: 5 * time.Minute,
		Suppressor:       newTestRedisRouteSuppressor(t, mr, nil),
	})
	orchB := newTestOrchestratorWithOptions(finalMessageModel("ok"), storeB, OrchestratorOptions{
		CoalesceCooldown: 5 * time.Minute,
		Suppressor:       newTestRedisRouteSuppressor(t, mr, nil),
	})
	go orchA.Run()
	go orchB.Run()
	defer orchA.Close()
	defer orchB.Close()

	accepted, suppressed := 0, 0
	for i := 0; i < 20; i++ {
		evt := testEvent()
		evt.EventID = fmt.Sprintf("multi-%d", i)
		evt.Route = "/api/v1/orders/"
		target := orchA
		if i%2 == 1 {
			target = orchB
		}
		res, err := target.Submit(context.Background(), evt)
		if err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
		switch res.Outcome {
		case SubmitAccepted:
			accepted++
		case SubmitSuppressed:
			suppressed++
		}
	}
	if accepted != 1 || suppressed != 19 {
		t.Fatalf("multi-replica storm: accepted=%d suppressed=%d, want 1/19", accepted, suppressed)
	}
	if got := storeA.claimCount() + storeB.claimCount(); got != 1 {
		t.Fatalf("total DB claims across replicas = %d, want 1", got)
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

func waitRouteTTL(t *testing.T, mr *miniredis.Miniredis, route string, check func(time.Duration) bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	key := routeSuppressKeyPrefix + route
	for {
		ttl := mr.TTL(key)
		if check(ttl) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for route TTL on %s (last=%v)", key, ttl)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
