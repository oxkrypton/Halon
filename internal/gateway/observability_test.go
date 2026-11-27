package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/oxkrypton/Halon/internal/incident"
	"github.com/oxkrypton/Halon/internal/trafficlog"
)

func allEnabledIncidentPolicies() IncidentPolicySet {
	enabled := make(map[incident.EventType]bool)
	for _, t := range incident.BuiltinEventTypes() {
		enabled[t] = true
	}
	return Config{
		IncidentEnabledTypes:         enabled,
		IncidentCoalesceCooldown:     0,
		IncidentSettleDelay:          0,
		IncidentBreakerSettleDelay:   0,
		Incident5xxThreshold:         1,
		Incident5xxWindow:            30 * time.Second,
		IncidentConcurrencyThreshold: 1,
		IncidentConcurrencyWindow:    30 * time.Second,
		IncidentRateLimitThreshold:   1,
		IncidentRateLimitWindow:      60 * time.Second,
		IncidentRateLimitCooldown:    0,
	}.IncidentPolicies()
}

func TestGatewayTriggersIncidentsForAllAnomalyTypes(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream500 := echoUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/timeout":
			select {
			case <-r.Context().Done():
			case <-time.After(3 * time.Second):
			}
		case "/api/v1/err500":
			w.WriteHeader(http.StatusInternalServerError)
		case "/api/v1/slow-concurrency":
			select {
			case <-r.Context().Done():
			case <-time.After(400 * time.Millisecond):
			}
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	})
	// Closed listener produces deterministic 502 connection refused for upstream_error.
	deadServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := deadServer.URL
	deadServer.Close()

	rules, limiter := newTestRedisStores(t, 2, 1)
	metrics := NewMetrics()
	routes, err := ParseRoutes("/api/v1/=" + upstream500.URL + ",/api/dead/=" + deadURL)
	if err != nil {
		t.Fatal(err)
	}
	incidentQueue := make(chan incident.Event, 32)
	sink := NewIncidentSink(incidentQueue, allEnabledIncidentPolicies(), metrics, testLogger())

	proxyOpts := ProxyOptions{
		Timeout: 200 * time.Millisecond,
		Resilience: ResilienceConfig{
			BreakerEnabled:           true,
			BreakerFailureThreshold:  2,
			BreakerOpenDuration:      time.Minute,
			BreakerHalfOpenSuccesses: 1,
			BreakerCount5xx:          true,
			ConcurrencyPerRoute:      1,
			Logger:                   testLogger(),
			Incidents:                sink,
		},
		Logger: testLogger(),
	}
	resilience := proxyOpts.BuildResilienceSet(routes.Prefixes(), metrics)
	router := NewHTTPHandler(Dependencies{
		Routes:               routes,
		Repository:           pingerStub{},
		LogQueue:             make(chan trafficlog.Entry, 32),
		Incidents:            sink,
		Logger:               testLogger(),
		Rules:                rules,
		Limiter:              limiter,
		Metrics:              metrics,
		Proxy:                proxyOpts,
		Resilience:           resilience,
		SlowRequestThreshold: time.Second,
		BodyCaptureLimit:     1024,
	})

	drainTypes := func() []incident.Event {
		var out []incident.Event
		for {
			select {
			case e := <-incidentQueue:
				out = append(out, e)
			default:
				return out
			}
		}
	}

	// 1) upstream_timeout -> 504
	req := httptest.NewRequest(http.MethodGet, "/api/v1/timeout", nil)
	req.RemoteAddr = "10.0.0.10:1234"
	req.Header.Set("X-Request-ID", "req-timeout")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("timeout status = %d, want 504", rec.Code)
	}
	evts := drainTypes()
	if len(evts) != 1 || evts[0].Type != incident.TypeUpstreamTimeout || evts[0].ScopeKey != "/api/v1/" || evts[0].RequestID != "req-timeout" {
		t.Fatalf("unexpected timeout events: %+v", evts)
	}

	// 2) upstream_error (502) + breaker_open on second consecutive failure on /api/dead/
	for i := 0; i < 2; i++ {
		r502 := httptest.NewRequest(http.MethodGet, "/api/dead/item", nil)
		r502.RemoteAddr = fmtIP(i + 20)
		rec502 := httptest.NewRecorder()
		router.ServeHTTP(rec502, r502)
		if rec502.Code != http.StatusBadGateway {
			t.Fatalf("502 attempt %d = %d, want 502", i, rec502.Code)
		}
	}
	evts = drainTypes()
	hasErr, hasBreaker := false, false
	for _, e := range evts {
		if e.Type == incident.TypeUpstreamError && e.ScopeKey == "/api/dead/" && e.StatusCode == 502 {
			hasErr = true
		}
		if e.Type == incident.TypeBreakerOpen && e.ScopeKey == "/api/dead/" {
			hasBreaker = true
		}
	}
	if !hasErr || !hasBreaker {
		t.Fatalf("expected upstream_error and breaker_open events, got %+v", evts)
	}

	// 3) Client cancellation on /api/v1/ must NOT emit upstream_error.
	cancelCtx, cancelReq := context.WithCancel(context.Background())
	cancelReq()
	rCanceled := httptest.NewRequest(http.MethodGet, "/api/v1/ok", nil).WithContext(cancelCtx)
	rCanceled.RemoteAddr = "10.0.0.30:1234"
	router.ServeHTTP(httptest.NewRecorder(), rCanceled)
	if cancelledEvts := drainTypes(); len(cancelledEvts) != 0 {
		t.Fatalf("client cancel must not emit incidents, got %+v", cancelledEvts)
	}

	// 4) upstream_5xx (proxy returned upstream 500 directly)
	r500 := httptest.NewRequest(http.MethodGet, "/api/v1/err500", nil)
	r500.RemoteAddr = "10.0.0.40:1234"
	rec500 := httptest.NewRecorder()
	router.ServeHTTP(rec500, r500)
	if rec500.Code != http.StatusInternalServerError {
		t.Fatalf("500 status = %d, want 500", rec500.Code)
	}
	evts = drainTypes()
	has5xx := false
	for _, e := range evts {
		if e.Type == incident.TypeUpstream5xx && e.StatusCode == 500 && e.ScopeKey == "/api/v1/" {
			has5xx = true
		}
	}
	if !has5xx {
		t.Fatalf("expected upstream_5xx event, got %+v", evts)
	}

	// 5) rate_limit_abuse (burst=1 exhausted on second immediate request from same IP)
	rRL1 := httptest.NewRequest(http.MethodGet, "/api/v1/ok", nil)
	rRL1.RemoteAddr = "198.51.100.77:5555"
	router.ServeHTTP(httptest.NewRecorder(), rRL1)
	drainTypes()
	rRL2 := httptest.NewRequest(http.MethodGet, "/api/v1/ok", nil)
	rRL2.RemoteAddr = "198.51.100.77:5555"
	recRL2 := httptest.NewRecorder()
	router.ServeHTTP(recRL2, rRL2)
	if recRL2.Code != http.StatusTooManyRequests {
		t.Fatalf("rate limit status = %d, want 429", recRL2.Code)
	}
	evts = drainTypes()
	if len(evts) != 1 || evts[0].Type != incident.TypeRateLimitAbuse || evts[0].ScopeKey != "198.51.100.77" {
		t.Fatalf("expected rate_limit_abuse event scoped to IP, got %+v", evts)
	}

	// 6) concurrency_limit (bulkhead=1: first in-flight request holds slot, second rejects 429)
	rulesBulk, limiterBulk := newTestRedisStores(t, 100, 100)
	bulkQueue := make(chan incident.Event, 8)
	bulkSink := NewIncidentSink(bulkQueue, allEnabledIncidentPolicies(), metrics, testLogger())
	bulkOpts := ProxyOptions{
		Timeout: time.Second,
		Resilience: ResilienceConfig{
			ConcurrencyPerRoute: 1,
			Logger:              testLogger(),
			Incidents:           bulkSink,
		},
	}
	bulkRoutes, _ := ParseRoutes("/api/v1/=" + upstream500.URL)
	bulkHandler := NewHTTPHandler(Dependencies{
		Routes:     bulkRoutes,
		Repository: pingerStub{},
		LogQueue:   make(chan trafficlog.Entry, 8),
		Incidents:  bulkSink,
		Rules:      rulesBulk,
		Limiter:    limiterBulk,
		Metrics:    metrics,
		Proxy:      bulkOpts,
		Resilience: bulkOpts.BuildResilienceSet(bulkRoutes.Prefixes(), metrics),
	})
	inFlight := make(chan struct{})
	go func() {
		slowReq := httptest.NewRequest(http.MethodGet, "/api/v1/slow-concurrency", nil)
		slowReq.RemoteAddr = "10.0.0.50:1111"
		close(inFlight)
		bulkHandler.ServeHTTP(httptest.NewRecorder(), slowReq)
	}()
	<-inFlight
	time.Sleep(50 * time.Millisecond)
	overflowReq := httptest.NewRequest(http.MethodGet, "/api/v1/ok", nil)
	overflowReq.RemoteAddr = "10.0.0.51:2222"
	overflowRec := httptest.NewRecorder()
	bulkHandler.ServeHTTP(overflowRec, overflowReq)
	if overflowRec.Code != http.StatusTooManyRequests {
		t.Fatalf("bulkhead overflow = %d, want 429", overflowRec.Code)
	}
	select {
	case bulkEvt := <-bulkQueue:
		if bulkEvt.Type != incident.TypeConcurrencyLimit || bulkEvt.ScopeKey != "/api/v1/" || bulkEvt.StatusCode != 429 {
			t.Fatalf("unexpected concurrency_limit event: %+v", bulkEvt)
		}
	case <-time.After(time.Second):
		t.Fatal("expected concurrency_limit event")
	}

	admin := NewAdminHandler(testAdminToken, rules, limiter, testLogger(), metrics, AdminOptions{})
	metricsResponse := httptest.NewRecorder()
	admin.ServeHTTP(metricsResponse, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(metricsResponse.Body.String(), `halon_gateway_http_requests_total{method="GET",route="/api/v1/",status="504"} 1`) {
		t.Fatalf("timeout metric missing: %s", metricsResponse.Body.String())
	}
}

func fmtIP(lastOctet int) string {
	return "10.0.0." + strconv.Itoa(lastOctet) + ":1234"
}

func TestIncidentNotifierDeliversAuthenticatedEvent(t *testing.T) {
	received := make(chan incident.Event, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-webhook-token" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		var event incident.Event
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			t.Error(err)
		}
		received <- event
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	queue := make(chan incident.Event, 1)
	notifier := NewIncidentNotifier(server.URL, "test-webhook-token", queue, testLogger(), nil)
	done := make(chan struct{})
	go func() {
		notifier.Run()
		close(done)
	}()
	event := incident.Event{
		EventID: "event-1", Type: incident.TypeUpstreamError, ScopeKey: "/api/v1/users/:id",
		RequestID: "request-1", OccurredAt: time.Now().UTC(),
		Method: http.MethodGet, Path: "/api/v1/users/1", Route: "/api/v1/users/:id",
		StatusCode: 502, LatencyMS: 12,
	}
	queue <- event
	close(queue)
	<-done
	if got := <-received; got.EventID != event.EventID || got.Type != event.Type || got.RequestID != event.RequestID {
		t.Fatalf("received event = %+v, want %+v", got, event)
	}
}
