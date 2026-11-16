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
	mu          sync.Mutex
	saved       []incident.Report
	processing  []string
	unfinished  []incident.Report
	failed      map[string]string
	claimExists bool
	claimErr    error
	loadErr     error
}

func (s *stubReportStore) Claim(_ context.Context, event incident.TimeoutEvent) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claimErr != nil {
		return false, s.claimErr
	}
	return !s.claimExists, nil
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
	if s.loadErr != nil {
		return nil, s.loadErr
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

	duplicate, err := orchestrator.Submit(context.Background(), testEvent())
	if err != nil || duplicate {
		t.Fatalf("submit = duplicate %v, err %v", duplicate, err)
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

	duplicate, err := orchestrator.Submit(context.Background(), testEvent())
	if err != nil || !duplicate {
		t.Fatalf("submit = duplicate %v, err %v, want duplicate", duplicate, err)
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

func TestOrchestratorRecoversUnfinishedIncidents(t *testing.T) {
	fresh := incident.NewReport(testEvent())
	fresh.OccurredAt = time.Now().Add(-5 * time.Minute)
	stale := incident.NewReport(testEvent())
	stale.EventID = "evt-stale"
	stale.OccurredAt = time.Now().Add(-2 * time.Hour)
	store := &stubReportStore{unfinished: []incident.Report{fresh, stale}}

	orchestrator := newTestOrchestrator(finalMessageModel("## 事件摘要\n已恢复"), store, 4)
	go orchestrator.Run()
	defer orchestrator.Close()

	requeued, expired, err := orchestrator.RecoverUnfinished(context.Background())
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if requeued != 1 || expired != 1 {
		t.Fatalf("recover = requeued %d, expired %d, want 1/1", requeued, expired)
	}
	if cause := store.failedReports()["evt-stale"]; cause == "" {
		t.Fatalf("stale incident must be marked failed, got %v", store.failedReports())
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		reports := store.savedReports()
		if len(reports) == 1 {
			report := reports[0]
			if report.EventID != fresh.EventID || report.Status != incident.StatusCompleted {
				t.Fatalf("recovered investigation report: %+v", report)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("recovered incident was not investigated in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestOrchestratorRecoveryPropagatesStoreError(t *testing.T) {
	store := &stubReportStore{loadErr: errors.New("database unreachable")}
	orchestrator := newTestOrchestrator(finalMessageModel("ok"), store, 4)

	requeued, expired, err := orchestrator.RecoverUnfinished(context.Background())
	if err == nil {
		t.Fatal("load failure must surface")
	}
	if requeued != 0 || expired != 0 {
		t.Fatalf("recover = requeued %d, expired %d, want 0/0", requeued, expired)
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
