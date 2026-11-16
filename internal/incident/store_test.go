package incident_test

import (
	"context"
	"errors"
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

func testEvent() incident.TimeoutEvent {
	return incident.TimeoutEvent{
		EventID:    "evt-1",
		RequestID:  "req-1",
		OccurredAt: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC),
		Method:     "GET",
		Path:       "/api/v1/users/42",
		Route:      "/api/v1/users/:id",
		ClientIP:   "10.0.0.8",
		StatusCode: 504,
		LatencyMS:  3001.5,
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
	report.Executions = incident.JSONB(`[{"name":"k8s_list_pods"}]`)
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
		"event_id", "request_id", "occurred_at", "method", "path", "route",
		"client_ip", "status_code", "latency_ms", "status",
	}).
		AddRow("evt-old", "req-1", time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC), "GET", "/a", "/a", "10.0.0.1", 504, 3000.0, "queued").
		AddRow("evt-new", "req-2", time.Date(2026, 11, 16, 11, 0, 0, 0, time.UTC), "POST", "/b", "/b", "10.0.0.2", 504, 3100.5, "processing")
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
	if event.EventID != "evt-new" || event.RequestID != "req-2" || event.Method != "POST" ||
		event.Path != "/b" || event.Route != "/b" || event.ClientIP != "10.0.0.2" ||
		event.StatusCode != 504 || event.LatencyMS != 3100.5 {
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

func TestReportEventRoundTrip(t *testing.T) {
	t.Parallel()

	want := testEvent()
	got := incident.NewReport(want).Event()
	if got != want {
		t.Fatalf("report -> event round trip mismatch:\n got %+v\nwant %+v", got, want)
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

func TestListClampsLimit(t *testing.T) {
	t.Parallel()

	store, mock := newMockStore(t)
	mock.ExpectQuery(`SELECT \* FROM "incident_reports" ORDER BY occurred_at DESC LIMIT \$1`).
		WithArgs(50).
		WillReturnRows(sqlmock.NewRows([]string{"event_id"}))

	reports, err := store.List(context.Background(), 9999)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(reports) != 0 {
		t.Fatalf("list returned %d reports, want 0", len(reports))
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
