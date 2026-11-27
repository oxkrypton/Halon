package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
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
	if len(tools) != 1 || tools[0].Name != "read_observability" {
		t.Fatalf("unexpected tools: %+v", tools)
	}
	handler := tools[0].Handler

	for _, raw := range []string{
		`{}`,
		`{"queries":[]}`,
		`{"queries":[{"source":"error_logs"},{"source":"error_logs"},{"source":"error_logs"},{"source":"error_logs"},{"source":"error_logs"},{"source":"error_logs"},{"source":"error_logs"},{"source":"error_logs"},{"source":"error_logs"}]}`,
		`{"unknown":true,"queries":[{"source":"error_logs"}]}`,
	} {
		if _, err := handler(context.Background(), json.RawMessage(raw)); err == nil {
			t.Fatalf("expected error for %s", raw)
		}
	}

	if _, err := handler(context.Background(), json.RawMessage(`{"queries":[{"source":"error_logs","limit":1000}]}`)); err == nil {
		t.Fatal("expected limit validation error")
	}
	if _, err := handler(context.Background(), json.RawMessage(`{"queries":[{"source":"traffic_stats","limit":10}]}`)); err == nil || !strings.Contains(err.Error(), "allowed fields:") {
		t.Fatalf("expected allowed fields hint, got %v", err)
	}
	if _, err := handler(context.Background(), json.RawMessage(`{"queries":[{"source":"unknown_source"}]}`)); err == nil || !strings.Contains(err.Error(), "allowed sources:") {
		t.Fatalf("expected allowed sources hint, got %v", err)
	}
}

func TestReadObservabilityBatchAndPartialFailure(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()

	statsRows := sqlmock.NewRows([]string{"path", "status_code", "count", "avg", "max", "p95", "p99", "server_errors"}).
		AddRow("/api/v1/orders/1", 504, 12, 3005.0, 3100.0, 3090.0, 3099.0, 12)
	topIPRows := sqlmock.NewRows([]string{"client_ip", "requests", "error_count", "average_ms"}).
		AddRow("192.0.2.10", 50, 40, 120.5)
	mock.ExpectQuery("SELECT path, status_code").WithArgs(30).WillReturnRows(statsRows)
	mock.ExpectQuery("SELECT client_ip, COUNT").WithArgs(30, 10).WillReturnRows(topIPRows)

	tools := ObservabilityTools(database, "", nil)
	res, err := tools[0].Handler(context.Background(), json.RawMessage(`{
		"queries": [
			{"source": "traffic_stats"},
			{"source": "top_ips"},
			{"source": "traffic_stats", "limit": 5}
		]
	}`))
	if err != nil {
		t.Fatalf("partial success should not fail whole batch: %v", err)
	}
	results := res.(map[string]any)["results"].([]batchItemResult)
	if len(results) != 3 {
		t.Fatalf("len(results) = %d, want 3", len(results))
	}
	if results[0].Source != "traffic_stats" || results[0].Error != "" || len(results[0].Data.([]TrafficStatRow)) != 1 {
		t.Fatalf("unexpected first result: %+v", results[0])
	}
	if results[1].Source != "top_ips" || results[1].Error != "" || len(results[1].Data.([]TopIPRow)) != 1 {
		t.Fatalf("unexpected second result: %+v", results[1])
	}
	if results[2].Source != "traffic_stats" || !strings.Contains(results[2].Error, "allowed fields:") {
		t.Fatalf("unexpected third result: %+v", results[2])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestReadObservabilityPrometheusConditional(t *testing.T) {
	database, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()

	noPromTool := ObservabilityTools(database, "", nil)[0]
	noPromProps := noPromTool.Parameters["properties"].(map[string]any)["queries"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	noPromSources := noPromProps["source"].(map[string]any)["enum"].([]string)
	if !reflect.DeepEqual(noPromSources, []string{"error_logs", "traffic_stats", "top_ips"}) {
		t.Fatalf("sources without prometheus = %v", noPromSources)
	}
	if _, exists := noPromProps["query"]; exists {
		t.Fatal("query field should be omitted when PROMETHEUS_URL is empty")
	}
	if _, exists := noPromProps["time"]; exists {
		t.Fatal("time field should be omitted when PROMETHEUS_URL is empty")
	}
	if _, err := noPromTool.Handler(context.Background(), json.RawMessage(`{"queries":[{"source":"prometheus","query":"up"}]}`)); err == nil || !strings.Contains(err.Error(), "unsupported source") {
		t.Fatalf("expected unsupported source when PROMETHEUS_URL empty, got %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": []any{}}})
	}))
	defer server.Close()

	withPromTool := ObservabilityTools(database, server.URL, server.Client())[0]
	withPromProps := withPromTool.Parameters["properties"].(map[string]any)["queries"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	withPromSources := withPromProps["source"].(map[string]any)["enum"].([]string)
	if !reflect.DeepEqual(withPromSources, []string{"error_logs", "traffic_stats", "top_ips", "prometheus"}) {
		t.Fatalf("sources with prometheus = %v", withPromSources)
	}
	if _, exists := withPromProps["query"]; !exists {
		t.Fatal("query field should be present when PROMETHEUS_URL is configured")
	}
	res, err := withPromTool.Handler(context.Background(), json.RawMessage(`{"queries":[{"source":"prometheus","query":"up"}]}`))
	if err != nil {
		t.Fatalf("prometheus query failed: %v", err)
	}
	results := res.(map[string]any)["results"].([]batchItemResult)
	if len(results) != 1 || results[0].Data.(map[string]any)["status"] != "success" {
		t.Fatalf("unexpected prometheus batch result: %+v", results)
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
