package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestQueryErrorLogsUsesParametersAndReturnsRows(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	createdAt := time.Now().UTC()
	rows := sqlmock.NewRows([]string{"id", "request_id", "client_ip", "method", "path", "status_code", "latency_ms", "created_at", "request_body", "response_body"}).
		AddRow(1, "request-1", "127.0.0.1", "GET", "/api/v1/users/1", 504, 3000.0, createdAt, "", `{"error":"timeout"}`)
	mock.ExpectQuery("SELECT b.id, b.request_id").WithArgs(30, "request-1", 20).WillReturnRows(rows)
	results, err := queryErrorLogs(context.Background(), database, "request-1", "", 30, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].RequestID != "request-1" || results[0].StatusCode != 504 {
		t.Fatalf("unexpected rows: %+v", results)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestObservabilityToolRejectsInvalidArguments(t *testing.T) {
	database, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	tools := ObservabilityTools(database, "", nil)
	_, err = tools[0].Handler(context.Background(), json.RawMessage(`{"limit":1000}`))
	if err == nil {
		t.Fatal("expected limit validation error")
	}
	_, err = tools[0].Handler(context.Background(), json.RawMessage(`{"unknown":true}`))
	if err == nil {
		t.Fatal("expected unknown field validation error")
	}
}

func TestQueryPrometheus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.Form.Get("query") != "up" || r.Form.Get("time") != "2026-01-01T00:00:00Z" {
			t.Errorf("form = %+v", r.Form)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": []any{}}})
	}))
	defer server.Close()
	result, err := queryPrometheus(context.Background(), server.Client(), server.URL, "up", "2026-01-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if result["status"] != "success" || result["data"] == nil {
		t.Fatalf("result = %+v", result)
	}
}

func TestQueryTrafficStatsReturnsEmptySet(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func(database *sql.DB) { _ = database.Close() }(database)
	rows := sqlmock.NewRows([]string{"path", "status_code", "count", "avg", "max", "p95", "p99", "server_errors"})
	mock.ExpectQuery("SELECT path, status_code").WithArgs(30, "/api/v1/users/1").WillReturnRows(rows)
	results, err := queryTrafficStats(context.Background(), database, "/api/v1/users/1", 30)
	if err != nil || len(results) != 0 {
		t.Fatalf("results = %+v, err = %v", results, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
