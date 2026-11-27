package incident_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/oxkrypton/Halon/internal/incident"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func newMockStore(t *testing.T) (*incident.Store, sqlmock.Sqlmock) {
	t.Helper()
	sqlDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	gormDB, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{
		// incident timestamps are set explicitly; keep the mock surface minimal.
		DisableAutomaticPing: true,
	})
	if err != nil {
		t.Fatalf("open gorm over sqlmock: %v", err)
	}
	return incident.NewStore(gormDB), mock
}

func testEvent() incident.Event {
	return incident.Event{
		EventID:     "evt-1",
		Type:        incident.TypeUpstreamTimeout,
		ScopeKey:    "/api/v1/users/:id",
		RequestID:   "req-1",
		OccurredAt:  time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC),
		Method:      "GET",
		Path:        "/api/v1/users/42",
		Route:       "/api/v1/users/:id",
		ClientIP:    "10.0.0.8",
		StatusCode:  504,
		LatencyMS:   3001.5,
		Summary:     "upstream timeout on /api/v1/users/:id",
		Occurrences: 1,
	}
}

func TestClaimInsertsQueuedReport(t *testing.T) {
	t.Parallel()

	store, mock := newMockStore(t)
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO "incident_reports"`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	claimed, err := store.Claim(context.Background(), testEvent())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !claimed {
		t.Fatal("claim must report true for a new event")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestClaimIsIdempotentOnConflict(t *testing.T) {
	t.Parallel()

	store, mock := newMockStore(t)
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO "incident_reports"`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()

	claimed, err := store.Claim(context.Background(), testEvent())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed {
		t.Fatal("claim must report false when the event ID already exists")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSaveResultUpdatesClaimedReport(t *testing.T) {
	t.Parallel()

	store, mock := newMockStore(t)
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE "incident_reports" SET`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	report := incident.NewReport(testEvent())
	report.Status = incident.StatusCompleted
	report.FinalMessage = "# 事件摘要\n……"
	report.Steps = 3
	report.Executions = incident.JSONB(`[{"name":"read_k8s"}]`)
	if err := store.SaveResult(context.Background(), report); err != nil {
		t.Fatalf("save result: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSaveResultFailsForUnknownEvent(t *testing.T) {
	t.Parallel()

	store, mock := newMockStore(t)
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE "incident_reports" SET`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()

	report := incident.NewReport(testEvent())
	report.Status = incident.StatusFailed
	err := store.SaveResult(context.Background(), report)
	if !errors.Is(err, incident.ErrNotFound) {
		t.Fatalf("save result error = %v, want ErrNotFound", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadUnfinishedReturnsOrphans(t *testing.T) {
	t.Parallel()

	store, mock := newMockStore(t)
	rows := sqlmock.NewRows([]string{
		"event_id", "event_type", "scope_key", "request_id", "occurred_at", "method", "path", "route",
		"client_ip", "status_code", "latency_ms", "status",
	}).
		AddRow("evt-old", "upstream_timeout", "/a", "req-1", time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC), "GET", "/a", "/a", "10.0.0.1", 504, 3000.0, "queued").
		AddRow("evt-new", "breaker_open", "/b", "", time.Date(2026, 11, 16, 11, 0, 0, 0, time.UTC), "", "", "/b", "", 0, 0.0, "processing")
	mock.ExpectQuery(`SELECT \* FROM "incident_reports" WHERE status IN \(\$1,\$2\) ORDER BY occurred_at ASC`).
		WithArgs("queued", "processing").
		WillReturnRows(rows)

	reports, err := store.LoadUnfinished(context.Background())
	if err != nil {
		t.Fatalf("load unfinished: %v", err)
	}
	if len(reports) != 2 || reports[0].EventID != "evt-old" || reports[1].EventID != "evt-new" {
		t.Fatalf("unexpected unfinished reports: %+v", reports)
	}

	event := reports[1].Event()
	if event.EventID != "evt-new" || event.Type != incident.TypeBreakerOpen ||
		event.ScopeKey != "/b" || event.Route != "/b" || event.StatusCode != 0 {
		t.Fatalf("reconstructed event lost fields: %+v", event)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadUnfinishedPropagatesErrors(t *testing.T) {
	t.Parallel()

	store, mock := newMockStore(t)
	mock.ExpectQuery(`SELECT \* FROM "incident_reports" WHERE status IN`).
		WillReturnError(errors.New("connection reset"))

	if _, err := store.LoadUnfinished(context.Background()); err == nil {
		t.Fatal("database errors must surface to the recovery caller")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMarkFailedClosesUnfinishedRow(t *testing.T) {
	t.Parallel()

	store, mock := newMockStore(t)
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE "incident_reports" SET`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	finished := time.Date(2026, 11, 16, 12, 30, 0, 0, time.UTC)
	if err := store.MarkFailed(context.Background(), "evt-1", "incident expired while the agent was down", finished); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestReportEventRoundTripAndValidation(t *testing.T) {
	t.Parallel()

	want := testEvent()
	want.Occurrences = 7
	want.Details = map[string]any{"attempt": float64(3), "reason": "dial timeout"}
	got := incident.NewReport(want).Event()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("report -> event round trip mismatch:\n got %+v\nwant %+v", got, want)
	}

	nonHTTP := incident.Event{
		EventID:     "evt-breaker",
		Type:        incident.TypeBreakerOpen,
		ScopeKey:    "/api/v1/orders/",
		OccurredAt:  time.Date(2026, 11, 16, 12, 5, 0, 0, time.UTC),
		Route:       "/api/v1/orders/",
		Summary:     "circuit breaker opened for /api/v1/orders/",
		Occurrences: 2,
		Details:     map[string]any{"threshold": float64(5)},
	}
	if err := nonHTTP.Validate(); err != nil {
		t.Fatalf("non-HTTP event Validate(): %v", err)
	}
	if roundTrip := incident.NewReport(nonHTTP).Event(); !reflect.DeepEqual(roundTrip, nonHTTP) {
		t.Fatalf("non-HTTP round trip mismatch:\n got %+v\nwant %+v", roundTrip, nonHTTP)
	}

	custom := incident.Event{
		EventID:  "evt-custom",
		Type:     "custom_probe_failed",
		ScopeKey: "health-check-1",
	}
	if err := custom.Validate(); err != nil {
		t.Fatalf("custom event type should be accepted cleanly: %v", err)
	}

	zeroOccurrences := testEvent()
	zeroOccurrences.Occurrences = 0
	if report := incident.NewReport(zeroOccurrences); report.Occurrences != 1 {
		t.Fatalf("NewReport defaulted occurrences = %d, want 1", report.Occurrences)
	}

	badCases := []func(e *incident.Event){
		func(e *incident.Event) { e.EventID = "" },
		func(e *incident.Event) { e.Type = "Bad Type!" },
		func(e *incident.Event) { e.ScopeKey = ""; e.Route = ""; e.ClientIP = "" },
		func(e *incident.Event) { e.StatusCode = 999 },
		func(e *incident.Event) { e.LatencyMS = -1 },
		func(e *incident.Event) { e.Occurrences = -1 },
		func(e *incident.Event) { e.WindowSeconds = -5 },
	}
	for i, mutate := range badCases {
		bad := testEvent()
		mutate(&bad)
		if err := bad.Validate(); err == nil {
			t.Fatalf("case %d: Validate() must reject invalid event", i)
		}
	}
}

func TestGetReturnsNotFound(t *testing.T) {
	t.Parallel()

	store, mock := newMockStore(t)
	mock.ExpectQuery(`SELECT \* FROM "incident_reports" WHERE event_id = \$1 LIMIT \$2`).
		WithArgs("missing", 1).
		WillReturnError(gorm.ErrRecordNotFound)

	_, err := store.Get(context.Background(), "missing")
	if !errors.Is(err, incident.ErrNotFound) {
		t.Fatalf("get error = %v, want ErrNotFound", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestListClampsLimitAndFiltersType(t *testing.T) {
	t.Parallel()

	store, mock := newMockStore(t)
	mock.ExpectQuery(`SELECT \* FROM "incident_reports" ORDER BY occurred_at DESC LIMIT \$1`).
		WithArgs(50).
		WillReturnRows(sqlmock.NewRows([]string{"event_id"}))

	reports, err := store.List(context.Background(), "", 9999)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(reports) != 0 {
		t.Fatalf("list returned %d reports, want 0", len(reports))
	}

	mock.ExpectQuery(`SELECT \* FROM "incident_reports" WHERE event_type = \$1 ORDER BY occurred_at DESC LIMIT \$2`).
		WithArgs("upstream_error", 10).
		WillReturnRows(sqlmock.NewRows([]string{"event_id"}))

	if _, err := store.List(context.Background(), incident.TypeUpstreamError, 10); err != nil {
		t.Fatalf("list filtered: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestJSONBValueRejectsInvalidJSON(t *testing.T) {
	t.Parallel()

	if _, err := incident.JSONB(`{broken`).Value(); err == nil {
		t.Fatal("invalid JSON must be rejected before reaching the database")
	}
	value, err := incident.JSONB(`{"ok":true}`).Value()
	if err != nil {
		t.Fatalf("valid JSON rejected: %v", err)
	}
	if value != `{"ok":true}` {
		t.Fatalf("driver value = %v", value)
	}
}
