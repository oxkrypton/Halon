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
