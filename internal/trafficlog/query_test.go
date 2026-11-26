package trafficlog

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func newMockRepository(t *testing.T) (*Repository, sqlmock.Sqlmock) {
	t.Helper()
	sqlDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	gormDB, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{
		SkipDefaultTransaction: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &Repository{db: gormDB}, mock
}

func TestOverviewStats(t *testing.T) {
	repo, mock := newMockRepository(t)
	rows := sqlmock.NewRows([]string{
		"count", "s2xx", "s4xx", "s5xx", "avg", "p50", "p95", "p99", "max", "clients",
	}).AddRow(120, 100, 14, 6, 45.2, 20.1, 180.4, 920.5, 1400.0, 18)

	mock.ExpectQuery("SELECT\\s+COUNT\\(\\*\\)").WithArgs(int64(3600), "/api/v1/users/").WillReturnRows(rows)

	stats, err := repo.OverviewStats(context.Background(), time.Hour, "/api/v1/users/")
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalRequests != 120 || stats.Status5xx != 6 || stats.P99MS != 920.5 || stats.UniqueClients != 18 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	if stats.ErrorRate <= 0 || stats.QPS <= 0 {
		t.Fatalf("expected derived error rate and qps: %+v", stats)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestOverviewStatsValidation(t *testing.T) {
	repo, _ := newMockRepository(t)
	if _, err := repo.OverviewStats(context.Background(), 0, ""); err == nil {
		t.Fatal("expected error for zero window")
	}
	if _, err := repo.OverviewStats(context.Background(), 8*24*time.Hour, ""); err == nil {
		t.Fatal("expected error for oversized window")
	}
}

func TestTimeSeries(t *testing.T) {
	repo, mock := newMockRepository(t)
	bucket := time.Now().UTC().Truncate(time.Minute)
	rows := sqlmock.NewRows([]string{
		"bucket", "count", "s2xx", "s4xx", "s5xx", "avg", "p50", "p95", "p99",
	}).AddRow(bucket, 25, 22, 2, 1, 32.5, 18.0, 95.0, 210.0)

	mock.ExpectQuery("date_bin").WithArgs(int64(60), int64(3600)).WillReturnRows(rows)

	points, err := repo.TimeSeries(context.Background(), time.Hour, time.Minute, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 || points[0].P99MS != 210.0 || points[0].Status5xx != 1 {
		t.Fatalf("unexpected points: %+v", points)
	}
	if _, err := repo.TimeSeries(context.Background(), time.Hour, 2*time.Hour, ""); err == nil {
		t.Fatal("expected error when step exceeds window")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRouteStats(t *testing.T) {
	repo, mock := newMockRepository(t)
	rows := sqlmock.NewRows([]string{
		"target", "count", "s4xx", "s5xx", "avg", "p50", "p95", "p99", "max",
	}).AddRow("/api/v1/orders/", 50, 2, 5, 110.0, 40.0, 400.0, 1200.0, 3000.0)

	mock.ExpectQuery("FROM base_logs").WithArgs(int64(900), 25).WillReturnRows(rows)

	stats, err := repo.RouteStats(context.Background(), 15*time.Minute, "route", "p99_ms", 25)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 1 || stats[0].Target != "/api/v1/orders/" || stats[0].ErrorRate != 0.1 {
		t.Fatalf("unexpected route stats: %+v", stats)
	}
	if _, err := repo.RouteStats(context.Background(), time.Hour, "invalid", "requests", 10); err == nil {
		t.Fatal("expected invalid group_by error")
	}
	if _, err := repo.RouteStats(context.Background(), time.Hour, "route", "bad_sort", 10); err == nil {
		t.Fatal("expected invalid sort_by error")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestListLogsAndDetails(t *testing.T) {
	repo, mock := newMockRepository(t)
	now := time.Now().UTC()

	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM base_logs").
		WithArgs(int64(3600), "/api/v1/users/", 500, 100.0).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery("FROM base_logs b\\s+LEFT JOIN error_body").
		WithArgs(int64(3600), "/api/v1/users/", 500, 100.0, 20, 0).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "request_id", "trace_id", "client_ip", "method", "path", "route",
			"status_code", "latency_ms", "upstream_latency_ms", "upstream_result",
			"retries", "created_at", "has_body",
		}).AddRow(42, "req-1", "0123456789abcdef0123456789abcdef", "10.0.0.1", "GET", "/api/v1/users/7", "/api/v1/users/", 504, 3001.2, 3000.1, "timeout", 1, now, true))

	items, total, err := repo.ListLogs(context.Background(), LogFilter{
		Window:       time.Hour,
		Route:        "/api/v1/users/",
		StatusMin:    500,
		MinLatencyMS: 100,
		OnlyErrors:   true,
		Limit:        20,
		Offset:       0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(items) != 1 || items[0].ID != 42 || !items[0].HasBody {
		t.Fatalf("unexpected list result: total=%d items=%+v", total, items)
	}

	mock.ExpectQuery("WHERE b.id = \\$1").WithArgs(int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "request_id", "trace_id", "client_ip", "method", "path", "route",
			"status_code", "latency_ms", "upstream_latency_ms", "upstream_result",
			"retries", "created_at", "has_body", "request_body", "response_body",
		}).AddRow(42, "req-1", "0123456789abcdef0123456789abcdef", "10.0.0.1", "GET", "/api/v1/users/7", "/api/v1/users/", 504, 3001.2, 3000.1, "timeout", 1, now, true, `{"id":7}`, `{"error":"timeout"}`))
	detail, err := repo.GetLogByID(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if detail.ResponseBody != `{"error":"timeout"}` {
		t.Fatalf("unexpected detail: %+v", detail)
	}

	mock.ExpectQuery("WHERE b.id = \\$1").WithArgs(int64(999)).WillReturnError(sql.ErrNoRows)
	if _, err := repo.GetLogByID(context.Background(), 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	mock.ExpectQuery("WHERE b.trace_id = \\$1").WithArgs("0123456789abcdef0123456789abcdef", 20).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "request_id", "trace_id", "client_ip", "method", "path", "route",
			"status_code", "latency_ms", "upstream_latency_ms", "upstream_result",
			"retries", "created_at", "has_body", "request_body", "response_body",
		}))
	if byTrace, err := repo.GetLogsByTraceID(context.Background(), "0123456789abcdef0123456789abcdef", 20); err != nil || len(byTrace) != 0 {
		t.Fatalf("unexpected GetLogsByTraceID: %+v %v", byTrace, err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestTopClients(t *testing.T) {
	repo, mock := newMockRepository(t)
	rows := sqlmock.NewRows([]string{"client_ip", "count", "errs", "s5xx", "avg", "p95"}).
		AddRow("192.0.2.5", 80, 12, 3, 25.4, 120.0)
	mock.ExpectQuery("GROUP BY client_ip").WithArgs(int64(3600), 10).WillReturnRows(rows)

	clients, err := repo.TopClients(context.Background(), time.Hour, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(clients) != 1 || clients[0].ClientIP != "192.0.2.5" || clients[0].Status5xx != 3 {
		t.Fatalf("unexpected top clients: %+v", clients)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
