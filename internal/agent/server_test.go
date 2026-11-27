package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	mu           sync.Mutex
	reports      map[string]*incident.Report
	pingErr      error
	lastListType incident.EventType
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

func (s *stubReportReader) List(_ context.Context, eventType incident.EventType, limit int) ([]incident.Report, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastListType = eventType
	result := make([]incident.Report, 0, len(s.reports))
	for _, report := range s.reports {
		if eventType != "" && report.EventType != eventType {
			continue
		}
		result = append(result, *report)
	}
	return result, nil
}

func (s *stubReportReader) Ping(context.Context) error {
	return s.pingErr
}

type stubPodLogReader struct {
	namespaces []string
	pods       []PodSummary
	listErr    error
	logs       map[string]PodLogResult
	logsErr    error
}

func (s *stubPodLogReader) AllowedNamespaces() []string {
	out := make([]string, len(s.namespaces))
	copy(out, s.namespaces)
	return out
}

func (s *stubPodLogReader) ListPods(_ context.Context, namespace, _ string, _ int) (string, []PodSummary, error) {
	if s.listErr != nil {
		return "", nil, s.listErr
	}
	resolved := namespace
	if resolved == "" {
		if len(s.namespaces) > 0 && s.namespaces[0] != "*" {
			resolved = s.namespaces[0]
		} else {
			resolved = "default"
		}
	}
	if resolved == "forbidden-ns" {
		return "", nil, fmt.Errorf("namespace %q: %w", resolved, ErrNamespaceForbidden)
	}
	return resolved, s.pods, nil
}

func (s *stubPodLogReader) GetPodLogs(_ context.Context, opts PodLogOptions) (PodLogResult, error) {
	if s.logsErr != nil {
		return PodLogResult{}, s.logsErr
	}
	if opts.Namespace == "forbidden-ns" {
		return PodLogResult{}, fmt.Errorf("namespace %q: %w", opts.Namespace, ErrNamespaceForbidden)
	}
	if opts.Pod == "missing" {
		return PodLogResult{}, ErrPodNotFound
	}
	if opts.Previous && opts.Pod == "no-previous" {
		return PodLogResult{}, fmt.Errorf("previous terminated container not found: %w", ErrInvalidPodRequest)
	}
	if res, ok := s.logs[opts.Pod]; ok {
		res.Previous = opts.Previous
		return res, nil
	}
	ns := opts.Namespace
	if ns == "" {
		ns = "default"
	}
	return PodLogResult{
		Namespace: ns,
		Pod:       opts.Pod,
		Container: opts.Container,
		Previous:  opts.Previous,
		Logs:      "2026-11-27T12:00:00Z {\"level\":\"INFO\",\"msg\":\"ok\"}\n",
	}, nil
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

func postEventAt(t *testing.T, router *gin.Engine, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func postEvent(t *testing.T, router *gin.Engine, token, body string) *httptest.ResponseRecorder {
	return postEventAt(t, router, "/api/v1/incidents", token, body)
}

func validEventBody() string {
	return `{"event_id":"evt-100","type":"upstream_timeout","request_id":"req-100","occurred_at":"2026-11-16T12:00:00Z",` +
		`"method":"GET","path":"/api/v1/users/1","route":"/api/v1/users/:id","client_ip":"10.0.0.8",` +
		`"status_code":504,"latency_ms":3001}`
}

func TestIngestAcceptsValidEventAndLegacyTimeoutEndpoint(t *testing.T) {
	t.Parallel()

	router := newTestRouter(&stubReportStore{}, &stubReportReader{}, incident.NewDeduper(time.Minute))
	response := postEvent(t, router, testIngestToken, validEventBody())
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"accepted":true`) || !strings.Contains(response.Body.String(), `"event_type":"upstream_timeout"`) {
		t.Fatalf("body = %s", response.Body.String())
	}

	// Non-HTTP event (breaker_open) on /api/v1/incidents without request fields.
	breakerBody := `{"event_id":"evt-brk-1","type":"breaker_open","scope_key":"/api/v1/orders/","route":"/api/v1/orders/","summary":"circuit breaker opened","occurrences":3}`
	brkResp := postEvent(t, router, testIngestToken, breakerBody)
	if brkResp.Code != http.StatusAccepted || !strings.Contains(brkResp.Body.String(), `"event_type":"breaker_open"`) {
		t.Fatalf("breaker ingest = %d %s", brkResp.Code, brkResp.Body.String())
	}

	// Legacy /api/v1/incidents/timeout defaults missing type to upstream_timeout.
	legacyBody := `{"event_id":"evt-legacy-1","request_id":"req-legacy","occurred_at":"2026-11-16T12:00:00Z","method":"GET","path":"/api/v1/legacy/1","route":"/api/v1/legacy/","client_ip":"10.0.0.8","status_code":504,"latency_ms":3001}`
	legacyResp := postEventAt(t, router, "/api/v1/incidents/timeout", testIngestToken, legacyBody)
	if legacyResp.Code != http.StatusAccepted || !strings.Contains(legacyResp.Body.String(), `"event_type":"upstream_timeout"`) {
		t.Fatalf("legacy timeout ingest = %d %s", legacyResp.Code, legacyResp.Body.String())
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

	report := incident.NewReport(incident.Event{
		EventID: "evt-9", Type: incident.TypeUpstreamTimeout, ScopeKey: "/p",
		RequestID: "req-9", OccurredAt: time.Now().UTC(),
		Method: "GET", Path: "/p", Route: "/p", ClientIP: "10.0.0.1", StatusCode: 504,
	})
	report.Status = incident.StatusCompleted
	errReport := incident.NewReport(incident.Event{
		EventID: "evt-10", Type: incident.TypeUpstreamError, ScopeKey: "/err",
		RequestID: "req-10", OccurredAt: time.Now().UTC(),
		Method: "GET", Path: "/err", Route: "/err", ClientIP: "10.0.0.2", StatusCode: 502,
	})
	errReport.Status = incident.StatusCompleted
	reader := &stubReportReader{reports: map[string]*incident.Report{"evt-9": &report, "evt-10": &errReport}}
	router := newTestRouter(&stubReportStore{}, reader, incident.NewDeduper(time.Minute))

	request := httptest.NewRequest(http.MethodGet, "/api/v1/incidents", nil)
	request.Header.Set("Authorization", "Bearer "+testIngestToken)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "evt-9") {
		t.Fatalf("list: status = %d, body = %s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/api/v1/incidents?type=upstream_error", nil)
	request.Header.Set("Authorization", "Bearer "+testIngestToken)
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "evt-10") || strings.Contains(response.Body.String(), "evt-9") {
		t.Fatalf("filtered list: status = %d, body = %s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/api/v1/incidents?type=Invalid!", nil)
	request.Header.Set("Authorization", "Bearer "+testIngestToken)
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid type query = %d, want 400", response.Code)
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

func TestIngestSuppressesActiveRouteAndRecordsMetrics(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	store := &stubReportStore{}
	metrics := NewMetrics()
	orchestrator := newTestOrchestratorWithOptions(finalMessageModel("ok"), store, OrchestratorOptions{
		Metrics: metrics,
	})
	router := NewHTTPHandler(ServerDeps{
		Orchestrator: orchestrator,
		Deduper:      incident.NewDeduper(time.Minute),
		Reports:      &stubReportReader{},
		IngestToken:  testIngestToken,
		Logger:       testLogger(),
		Metrics:      metrics,
	})

	// 1) First event on route -> 202 accepted.
	first := postEvent(t, router, testIngestToken, validEventBody())
	if first.Code != http.StatusAccepted || !strings.Contains(first.Body.String(), `"suppressed":false`) {
		t.Fatalf("first = %d %s", first.Code, first.Body.String())
	}

	// 2) Same event_id retried -> 200 duplicate.
	dup := postEvent(t, router, testIngestToken, validEventBody())
	if dup.Code != http.StatusOK || !strings.Contains(dup.Body.String(), `"duplicate":true`) {
		t.Fatalf("dup = %d %s", dup.Code, dup.Body.String())
	}

	// 3) Different event_id while route session is active -> 200 suppressed, pointing to evt-100.
	secondBody := strings.Replace(validEventBody(), "evt-100", "evt-101", 1)
	sup := postEvent(t, router, testIngestToken, secondBody)
	if sup.Code != http.StatusOK || !strings.Contains(sup.Body.String(), `"suppressed":true`) || !strings.Contains(sup.Body.String(), `"incident_id":"evt-100"`) {
		t.Fatalf("suppressed = %d %s", sup.Code, sup.Body.String())
	}

	// 4) Sessions endpoint exposes the active queued session.
	sessReq := httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil)
	sessReq.Header.Set("Authorization", "Bearer "+testIngestToken)
	sessRec := httptest.NewRecorder()
	router.ServeHTTP(sessRec, sessReq)
	if sessRec.Code != http.StatusOK || !strings.Contains(sessRec.Body.String(), `"session_id":"evt-100"`) {
		t.Fatalf("sessions = %d %s", sessRec.Code, sessRec.Body.String())
	}

	// 5) Prometheus /metrics reports admission outcomes and active sessions.
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	body := rec.Body.String()
	for _, want := range []string{
		`halon_agent_incident_admissions_total{outcome="accepted",type="upstream_timeout"} 1`,
		`halon_agent_incident_admissions_total{outcome="duplicate",type="upstream_timeout"} 1`,
		`halon_agent_incident_admissions_total{outcome="suppressed",type="upstream_timeout"} 1`,
		`halon_agent_active_sessions 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics missing %q:\n%s", want, body)
		}
	}
}

func TestPodAndLogEndpoints(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	doGet := func(router *gin.Engine, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+testIngestToken)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	// Nil Pods dependency returns 503 K8S_UNAVAILABLE.
	unconfigured := newTestRouter(&stubReportStore{}, &stubReportReader{}, incident.NewDeduper(time.Minute))
	for _, path := range []string{"/api/v1/k8s/pods", "/api/v1/k8s/pods/gateway-1/logs"} {
		rec := doGet(unconfigured, path)
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"K8S_UNAVAILABLE"`) {
			t.Fatalf("%s with nil Pods = %d %s", path, rec.Code, rec.Body.String())
		}
	}

	podsReader := &stubPodLogReader{
		namespaces: []string{"default", "staging"},
		pods: []PodSummary{{
			Name:     "gateway-1",
			Phase:    "Running",
			Ready:    true,
			Restarts: 1,
			Node:     "node-a",
			Containers: []ContainerState{{
				Name:         "gateway",
				Ready:        true,
				RestartCount: 1,
				State:        "running",
			}},
		}},
		logs: map[string]PodLogResult{
			"gateway-1": {
				Namespace: "default",
				Pod:       "gateway-1",
				Container: "gateway",
				Logs:      "line-1\nline-2\n",
			},
		},
	}
	router := NewHTTPHandler(ServerDeps{
		Orchestrator: newTestOrchestrator(finalMessageModel("ok"), &stubReportStore{}, 8),
		Deduper:      incident.NewDeduper(time.Minute),
		Reports:      &stubReportReader{},
		Pods:         podsReader,
		IngestToken:  testIngestToken,
		Logger:       testLogger(),
	})

	// Happy paths.
	podsResp := doGet(router, "/api/v1/k8s/pods?namespace=default&label_selector=app%3Dgateway&limit=50")
	if podsResp.Code != http.StatusOK {
		t.Fatalf("list pods = %d %s", podsResp.Code, podsResp.Body.String())
	}
	var listBody struct {
		Namespace  string       `json:"namespace"`
		Namespaces []string     `json:"namespaces"`
		Pods       []PodSummary `json:"pods"`
	}
	if err := json.Unmarshal(podsResp.Body.Bytes(), &listBody); err != nil {
		t.Fatal(err)
	}
	if listBody.Namespace != "default" || len(listBody.Namespaces) != 2 || len(listBody.Pods) != 1 || listBody.Pods[0].Name != "gateway-1" {
		t.Fatalf("unexpected list response: %+v", listBody)
	}

	logsResp := doGet(router, "/api/v1/k8s/pods/gateway-1/logs?namespace=default&container=gateway&tail_lines=100&since_seconds=300&previous=true")
	if logsResp.Code != http.StatusOK {
		t.Fatalf("get logs = %d %s", logsResp.Code, logsResp.Body.String())
	}
	var logsBody PodLogResult
	if err := json.Unmarshal(logsResp.Body.Bytes(), &logsBody); err != nil {
		t.Fatal(err)
	}
	if logsBody.Pod != "gateway-1" || logsBody.Container != "gateway" || !logsBody.Previous || !strings.Contains(logsBody.Logs, "line-1") {
		t.Fatalf("unexpected logs response: %+v", logsBody)
	}

	// Error mapping and parameter validation.
	cases := []struct {
		path   string
		status int
		code   string
	}{
		{"/api/v1/k8s/pods?namespace=forbidden-ns", http.StatusForbidden, "NAMESPACE_FORBIDDEN"},
		{"/api/v1/k8s/pods?limit=0", http.StatusBadRequest, "INVALID_REQUEST"},
		{"/api/v1/k8s/pods?limit=101", http.StatusBadRequest, "INVALID_REQUEST"},
		{"/api/v1/k8s/pods/missing/logs?namespace=default", http.StatusNotFound, "POD_NOT_FOUND"},
		{"/api/v1/k8s/pods/gateway-1/logs?namespace=forbidden-ns", http.StatusForbidden, "NAMESPACE_FORBIDDEN"},
		{"/api/v1/k8s/pods/no-previous/logs?previous=true", http.StatusBadRequest, "INVALID_REQUEST"},
		{"/api/v1/k8s/pods/gateway-1/logs?tail_lines=0", http.StatusBadRequest, "INVALID_REQUEST"},
		{"/api/v1/k8s/pods/gateway-1/logs?tail_lines=1001", http.StatusBadRequest, "INVALID_REQUEST"},
		{"/api/v1/k8s/pods/gateway-1/logs?since_seconds=0", http.StatusBadRequest, "INVALID_REQUEST"},
		{"/api/v1/k8s/pods/gateway-1/logs?since_seconds=90000", http.StatusBadRequest, "INVALID_REQUEST"},
		{"/api/v1/k8s/pods/gateway-1/logs?previous=notabool", http.StatusBadRequest, "INVALID_REQUEST"},
	}
	for _, tc := range cases {
		rec := doGet(router, tc.path)
		if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.code) {
			t.Fatalf("%s = %d %s, want %d %s", tc.path, rec.Code, rec.Body.String(), tc.status, tc.code)
		}
	}
}
