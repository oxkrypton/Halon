package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/oxkrypton/Halon/internal/authz"
	"github.com/oxkrypton/Halon/internal/trafficlog"
)

type stubTrafficLogReader struct {
	overview   trafficlog.OverviewStats
	series     []trafficlog.TimeSeriesPoint
	routes     []trafficlog.RouteStat
	logs       []trafficlog.LogItem
	logsTotal  int64
	detail     *trafficlog.LogDetail
	detailErr  error
	traceLogs  []trafficlog.LogDetail
	topClients []trafficlog.TopClientStat
}

func (s *stubTrafficLogReader) OverviewStats(context.Context, time.Duration, string) (trafficlog.OverviewStats, error) {
	return s.overview, nil
}

func (s *stubTrafficLogReader) TimeSeries(context.Context, time.Duration, time.Duration, string) ([]trafficlog.TimeSeriesPoint, error) {
	return s.series, nil
}

func (s *stubTrafficLogReader) RouteStats(context.Context, time.Duration, string, string, int) ([]trafficlog.RouteStat, error) {
	return s.routes, nil
}

func (s *stubTrafficLogReader) ListLogs(context.Context, trafficlog.LogFilter) ([]trafficlog.LogItem, int64, error) {
	return s.logs, s.logsTotal, nil
}

func (s *stubTrafficLogReader) GetLogByID(context.Context, int64) (*trafficlog.LogDetail, error) {
	if s.detailErr != nil {
		return nil, s.detailErr
	}
	if s.detail == nil {
		return nil, trafficlog.ErrNotFound
	}
	return s.detail, nil
}

func (s *stubTrafficLogReader) GetLogByRequestID(context.Context, string) (*trafficlog.LogDetail, error) {
	if s.detail == nil {
		return nil, trafficlog.ErrNotFound
	}
	return s.detail, nil
}

func (s *stubTrafficLogReader) GetLogsByTraceID(context.Context, string, int) ([]trafficlog.LogDetail, error) {
	return s.traceLogs, nil
}

func (s *stubTrafficLogReader) TopClients(context.Context, time.Duration, string, int) ([]trafficlog.TopClientStat, error) {
	return s.topClients, nil
}

func TestObservabilityAndTraceAdminEndpoints(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	const traceID = "0123456789abcdef0123456789abcdef"
	now := time.Now().UTC()

	tempoServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/search":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"traces": []map[string]any{
					{
						"traceID":           traceID,
						"rootServiceName":   "halon-gateway",
						"rootTraceName":     "GET /api/v1/users/1",
						"startTimeUnixNano": "1764000000000000000",
						"durationMs":        42,
						"spanSet":           map[string]any{"matched": 2},
					},
				},
			})
		case "/api/traces/" + traceID:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"batches": []map[string]any{
					{
						"resource": map[string]any{
							"attributes": []map[string]any{
								{"key": "service.name", "value": map[string]any{"stringValue": "halon-gateway"}},
							},
						},
						"scopeSpans": []map[string]any{
							{
								"spans": []map[string]any{
									{
										"traceId":           traceID,
										"spanId":            "0102030405060708",
										"name":              "halon-gateway",
										"kind":              2,
										"startTimeUnixNano": "1764000000000000000",
										"endTimeUnixNano":   "1764000000042000000",
										"status":            map[string]any{"code": 1},
									},
								},
							},
						},
					},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer tempoServer.Close()

	routes, err := ParseRoutes("/api/v1/users/=http://user-service:8090")
	if err != nil {
		t.Fatal(err)
	}
	signer, keys := newAdminSigner(t)
	authenticator, err := authz.NewAuthenticator(authz.Config{
		Mode: authz.ModeJWT, PublicKeys: keys, Issuer: authz.DefaultIssuer,
	}, authz.AudienceGatewayAdmin, "")
	if err != nil {
		t.Fatal(err)
	}
	rules, limiter := newTestRedisStores(t, 100, 100)
	metrics := NewMetrics()
	stub := &stubTrafficLogReader{
		overview: trafficlog.OverviewStats{TotalRequests: 100, P99MS: 250.5, P95MS: 120, P50MS: 25},
		series:   []trafficlog.TimeSeriesPoint{{TS: now, Requests: 10, P99MS: 250.5}},
		routes:   []trafficlog.RouteStat{{Target: "/api/v1/users/", Requests: 100, P99MS: 250.5}},
		logs: []trafficlog.LogItem{{
			ID: 7, RequestID: "req-7", TraceID: traceID, ClientIP: "10.0.0.1",
			Method: "GET", Path: "/api/v1/users/1", Route: "/api/v1/users/",
			StatusCode: 504, LatencyMS: 3001, UpstreamLatencyMS: 3000, UpstreamResult: "timeout",
			CreatedAt: now, HasBody: true,
		}},
		logsTotal: 1,
		detail: &trafficlog.LogDetail{
			LogItem: trafficlog.LogItem{
				ID: 7, RequestID: "req-7", TraceID: traceID, ClientIP: "10.0.0.1",
				Method: "GET", Path: "/api/v1/users/1", Route: "/api/v1/users/",
				StatusCode: 504, LatencyMS: 3001, UpstreamLatencyMS: 3000, UpstreamResult: "timeout",
				CreatedAt: now, HasBody: true,
			},
			ResponseBody: `{"error":"timeout"}`,
		},
		traceLogs: []trafficlog.LogDetail{{
			LogItem: trafficlog.LogItem{
				ID: 7, RequestID: "req-7", TraceID: traceID, ClientIP: "10.0.0.1",
				Method: "GET", Path: "/api/v1/users/1", Route: "/api/v1/users/",
				StatusCode: 504, LatencyMS: 3001, UpstreamLatencyMS: 3000, UpstreamResult: "timeout",
				CreatedAt: now, HasBody: true,
			},
		}},
		topClients: []trafficlog.TopClientStat{{ClientIP: "10.0.0.1", Requests: 40, ErrorCount: 2, Status5xx: 1}},
	}

	router := NewAdminHandler("", rules, limiter, testLogger(), metrics, AdminOptions{
		Resilience:  &ResilienceSet{},
		Auth:        authenticator,
		TrafficLogs: stub,
		Routes:      routes,
		Tempo:       NewTempoClient(tempoServer.URL, 2*time.Second),
		SystemInfo: SystemInfo{
			AuthMode: "jwt", LoginEnabled: true, OTELEnabled: true, TempoEnabled: true, RedisUp: metrics.RedisUp,
		},
	})
	token := adminTokenFor(t, signer, authz.RoleReadOnly)

	endpoints := []struct {
		path string
		want string
	}{
		{"/admin/system", `"tempo_enabled":true`},
		{"/admin/observability/overview?window=1h", `"p99_ms":250.5`},
		{"/admin/observability/timeseries?window=15m", `"step_seconds":30`},
		{"/admin/observability/routes?window=1h&group_by=route&sort_by=p99_ms", `"/api/v1/users/"`},
		{"/admin/observability/logs?window=1h&only_errors=true", `"req-7"`},
		{"/admin/observability/logs/7", `"response_body"`},
		{"/admin/observability/top-ips?window=1h", `"10.0.0.1"`},
		{"/admin/traces?window=1h", `"source":"tempo"`},
		{"/admin/traces?window=1h&source=postgres", `"source":"postgres"`},
		{"/admin/traces/" + traceID, `"source":"tempo"`},
		{"/admin/traces/req-7", `"source":"postgres"`},
	}
	for _, tc := range endpoints {
		resp := adminRequest(router, http.MethodGet, tc.path, token, "")
		if resp.Code != http.StatusOK {
			t.Fatalf("%s -> status %d: %s", tc.path, resp.Code, resp.Body.String())
		}
		if !strings.Contains(resp.Body.String(), tc.want) {
			t.Fatalf("%s -> body %s missing %s", tc.path, resp.Body.String(), tc.want)
		}
	}

	// Bad parameters return 400.
	if resp := adminRequest(router, http.MethodGet, "/admin/observability/overview?window=99d", token, ""); resp.Code != http.StatusBadRequest {
		t.Fatalf("bad window status = %d", resp.Code)
	}
	if resp := adminRequest(router, http.MethodGet, "/admin/observability/logs/not-an-id", token, ""); resp.Code != http.StatusBadRequest {
		t.Fatalf("bad log id status = %d", resp.Code)
	}

	// Password change required tokens cannot access observability endpoints.
	pcrToken, err := signer.Issue(authz.IssueOptions{
		Subject:                "alice",
		Type:                   authz.TypeHuman,
		Role:                   authz.RoleFull,
		PasswordChangeRequired: true,
		Audiences:              []authz.Audience{authz.AudienceGatewayAdmin},
		TTL:                    time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp := adminRequest(router, http.MethodGet, "/admin/system", pcrToken, ""); resp.Code != http.StatusForbidden {
		t.Fatalf("pcr token status = %d, want 403", resp.Code)
	}
}
