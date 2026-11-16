package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/oxkrypton/Halon/internal/incident"
)

type stubReportReader struct {
	mu      sync.Mutex
	reports map[string]*incident.Report
	pingErr error
}

func (s *stubReportReader) Get(_ context.Context, eventID string) (*incident.Report, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	report, ok := s.reports[eventID]
	if !ok {
		return nil, incident.ErrNotFound
	}
	return report, nil
}

func (s *stubReportReader) List(_ context.Context, limit int) ([]incident.Report, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]incident.Report, 0, len(s.reports))
	for _, report := range s.reports {
		result = append(result, *report)
	}
	return result, nil
}

func (s *stubReportReader) Ping(context.Context) error {
	return s.pingErr
}

const testIngestToken = "test-ingest-token-0123456789"

func newTestRouter(store *stubReportStore, reader ReportReader, deduper *incident.Deduper) *gin.Engine {
	gin.SetMode(gin.TestMode)
	orchestrator := newTestOrchestrator(finalMessageModel("ok"), store, 8)
	return NewHTTPHandler(ServerDeps{
		Orchestrator: orchestrator,
		Deduper:      deduper,
		Reports:      reader,
		IngestToken:  testIngestToken,
		Logger:       testLogger(),
	})
}

func postEvent(t *testing.T, router *gin.Engine, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/incidents/timeout", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func validEventBody() string {
	return `{"event_id":"evt-100","request_id":"req-100","occurred_at":"2026-11-16T12:00:00Z",` +
		`"method":"GET","path":"/api/v1/users/1","route":"/api/v1/users/:id","client_ip":"10.0.0.8",` +
		`"status_code":504,"latency_ms":3001}`
}

func TestIngestAcceptsValidEvent(t *testing.T) {
	t.Parallel()

	router := newTestRouter(&stubReportStore{}, &stubReportReader{}, incident.NewDeduper(time.Minute))
	response := postEvent(t, router, testIngestToken, validEventBody())
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"accepted":true`) {
		t.Fatalf("body = %s", response.Body.String())
	}
}

func TestIngestDeduplicatesRetriedDeliveries(t *testing.T) {
	t.Parallel()

	router := newTestRouter(&stubReportStore{}, &stubReportReader{}, incident.NewDeduper(time.Minute))
	first := postEvent(t, router, testIngestToken, validEventBody())
	second := postEvent(t, router, testIngestToken, validEventBody())
	if first.Code != http.StatusAccepted || second.Code != http.StatusOK {
		t.Fatalf("statuses = %d then %d, want 202 then 200", first.Code, second.Code)
	}
	if !strings.Contains(second.Body.String(), `"duplicate":true`) {
		t.Fatalf("duplicate body = %s", second.Body.String())
	}
}

func TestIngestRejectsInvalidEvent(t *testing.T) {
	t.Parallel()

	router := newTestRouter(&stubReportStore{}, &stubReportReader{}, incident.NewDeduper(time.Minute))
	response := postEvent(t, router, testIngestToken, `{"event_id":"evt-1"}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.Code)
	}
}

func TestIngestRequiresBearerToken(t *testing.T) {
	t.Parallel()

	router := newTestRouter(&stubReportStore{}, &stubReportReader{}, incident.NewDeduper(time.Minute))
	for _, token := range []string{"", "wrong-token"} {
		response := postEvent(t, router, token, validEventBody())
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("token %q: status = %d, want 401", token, response.Code)
		}
	}
}

func TestReportQueries(t *testing.T) {
	t.Parallel()

	report := incident.NewReport(incident.TimeoutEvent{
		EventID: "evt-9", RequestID: "req-9", OccurredAt: time.Now().UTC(),
		Method: "GET", Path: "/p", Route: "/p", ClientIP: "10.0.0.1", StatusCode: 504,
	})
	report.Status = incident.StatusCompleted
	reader := &stubReportReader{reports: map[string]*incident.Report{"evt-9": &report}}
	router := newTestRouter(&stubReportStore{}, reader, incident.NewDeduper(time.Minute))

	request := httptest.NewRequest(http.MethodGet, "/api/v1/incidents", nil)
	request.Header.Set("Authorization", "Bearer "+testIngestToken)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "evt-9") {
		t.Fatalf("list: status = %d, body = %s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/api/v1/incidents/evt-9", nil)
	request.Header.Set("Authorization", "Bearer "+testIngestToken)
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"status":"completed"`) {
		t.Fatalf("get: status = %d, body = %s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/api/v1/incidents/missing", nil)
	request.Header.Set("Authorization", "Bearer "+testIngestToken)
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("get missing: status = %d, want 404", response.Code)
	}
}

func TestHealthAndReadiness(t *testing.T) {
	t.Parallel()

	router := newTestRouter(&stubReportStore{}, &stubReportReader{}, incident.NewDeduper(time.Minute))
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("healthz status = %d", response.Code)
	}

	request = httptest.NewRequest(http.MethodGet, "/readyz", nil)
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("readyz status = %d", response.Code)
	}

	unhealthy := newTestRouter(&stubReportStore{}, &stubReportReader{pingErr: errors.New("db down")}, incident.NewDeduper(time.Minute))
	request = httptest.NewRequest(http.MethodGet, "/readyz", nil)
	response = httptest.NewRecorder()
	unhealthy.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz with db down: status = %d, want 503", response.Code)
	}
}
