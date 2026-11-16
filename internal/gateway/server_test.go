package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/oxkrypton/Halon/internal/trafficlog"
)

const testAdminToken = "test-admin-token"

type pingerStub struct {
	err error
}

func (s pingerStub) Ping(context.Context) error { return s.err }

type fixture struct {
	router   *gin.Engine
	rules    *IPRules
	limiter  *RateLimiter
	queue    chan trafficlog.Entry
	upstream *httptest.Server
}

// echoUpstream records the inbound request and echoes it back, so tests can
// assert exactly what the proxy forwarded.
func echoUpstream(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func newFixture(t *testing.T, upstream *httptest.Server) *fixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	queue := make(chan trafficlog.Entry, 32)
	rules, _ := NewIPRules(nil)
	limiter := NewRateLimiter(100, 100)
	routes, err := ParseRoutes("/api/v1/=" + upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{
		router: NewHTTPHandler(Dependencies{
			Routes:               routes,
			Repository:           pingerStub{},
			LogQueue:             queue,
			Logger:               testLogger(),
			Rules:                rules,
			Limiter:              limiter,
			Proxy:                ProxyOptions{Timeout: time.Second},
			SlowRequestThreshold: time.Second,
			BodyCaptureLimit:     1024,
		}),
		rules:    rules,
		limiter:  limiter,
		queue:    queue,
		upstream: upstream,
	}
}

func TestProxyForwardsRequestAndPreservesPath(t *testing.T) {
	upstream := echoUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/users/1" {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		if r.Header.Get("X-Request-ID") != "trace-123" {
			t.Errorf("request ID was not forwarded: %q", r.Header.Get("X-Request-ID"))
		}
		if r.Header.Get("X-Forwarded-For") == "" {
			t.Error("X-Forwarded-For was not set")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user":{"id":"1","name":"Alice"}}`))
	})
	fixture := newFixture(t, upstream)

	request := httptest.NewRequest(http.MethodGet, "/api/v1/users/1", nil)
	request.Header.Set("X-Request-ID", "trace-123")
	response := httptest.NewRecorder()
	fixture.router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"name":"Alice"`) {
		t.Fatalf("unexpected response: %s", response.Body.String())
	}
	if response.Header().Get("X-Request-ID") != "trace-123" {
		t.Fatalf("request ID header = %q", response.Header().Get("X-Request-ID"))
	}
	entry := <-fixture.queue
	if entry.Base.RequestID != "trace-123" || entry.Base.Status != http.StatusOK || entry.Detail != nil {
		t.Fatalf("unexpected log entry: %+v", entry)
	}
}

func TestProxyPassesThroughUpstreamErrors(t *testing.T) {
	upstream := echoUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"code":"NOT_FOUND","message":"user missing"}}`, http.StatusNotFound)
	})
	fixture := newFixture(t, upstream)

	response := httptest.NewRecorder()
	fixture.router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/users/404", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want the upstream 404 to pass through", response.Code)
	}
	if !strings.Contains(response.Body.String(), "NOT_FOUND") {
		t.Fatalf("upstream body must pass through: %s", response.Body.String())
	}
	entry := <-fixture.queue
	if entry.Detail == nil || !strings.Contains(entry.Detail.ResponseBody, "NOT_FOUND") {
		t.Fatalf("error response was not captured: %+v", entry)
	}
}

func TestProxyTimeoutReturns504(t *testing.T) {
	upstream := echoUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	})
	fixture := newFixture(t, upstream)

	response := httptest.NewRecorder()
	fixture.router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/users/1", nil))
	if response.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", response.Code)
	}
	var body errorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "UPSTREAM_TIMEOUT" {
		t.Fatalf("error code = %q", body.Error.Code)
	}
}

func TestProxyUnreachableUpstreamReturns502(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	address := server.URL
	server.Close() // nothing is listening anymore
	routes, err := ParseRoutes("/api/v1/=" + address)
	if err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	queue := make(chan trafficlog.Entry, 1)
	rules, _ := NewIPRules(nil)
	router := NewHTTPHandler(Dependencies{
		Routes: routes, Repository: pingerStub{}, LogQueue: queue,
		Logger: testLogger(), Rules: rules, Limiter: NewRateLimiter(100, 100),
		Proxy: ProxyOptions{Timeout: time.Second}, SlowRequestThreshold: time.Second, BodyCaptureLimit: 1024,
	})

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/users/1", nil))
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", response.Code)
	}
}

func TestProxyUnmatchedPathReturns404(t *testing.T) {
	fixture := newFixture(t, echoUpstream(t, http.NotFoundHandler().ServeHTTP))
	response := httptest.NewRecorder()
	fixture.router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/unrouted/path", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.Code)
	}
}

func TestParseRoutes(t *testing.T) {
	t.Parallel()

	table, err := ParseRoutes("/api/v1/=http://a:8090, /api/v1/users/=http://b:8090")
	if err != nil {
		t.Fatal(err)
	}
	routes := table.Routes()
	if len(routes) != 2 || routes[0].Prefix != "/api/v1/users/" {
		t.Fatalf("routes must be longest-prefix first: %+v", routes)
	}
	if _, ok := table.match("/api/v1/users/1"); !ok {
		t.Fatal("user path must match")
	}
	route, ok := table.match("/api/v1/orders/1")
	if !ok || route.Prefix != "/api/v1/" {
		t.Fatalf("order path must match the broader prefix, got %+v", route)
	}
	if _, ok := table.match("/healthz"); ok {
		t.Fatal("probe path must not match any route")
	}

	empty, err := ParseRoutes("")
	if err != nil || len(empty.Routes()) != 0 {
		t.Fatalf("empty spec: %+v, %v", empty, err)
	}

	for _, spec := range []string{
		"no-equals-sign",
		"/x/=not-a-url",
		"x/=http://a",
		"/x/=http://a,/x/=http://b",
		"/x/=ftp://a",
	} {
		if _, err := ParseRoutes(spec); err == nil {
			t.Fatalf("spec %q must be rejected", spec)
		}
	}
}

func TestReadinessChecksDependencies(t *testing.T) {
	fixture := newFixture(t, echoUpstream(t, http.NotFoundHandler().ServeHTTP))
	response := httptest.NewRecorder()
	fixture.router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("ready status = %d, body = %s", response.Code, response.Body.String())
	}
	<-fixture.queue

	gin.SetMode(gin.TestMode)
	queue := make(chan trafficlog.Entry, 1)
	rules, _ := NewIPRules(nil)
	router := NewHTTPHandler(Dependencies{
		Repository: pingerStub{err: errors.New("offline")},
		LogQueue:   queue,
		Logger:     testLogger(),
		Rules:      rules,
		Limiter:    NewRateLimiter(1, 1),
	})
	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("not ready status = %d, want 503", response.Code)
	}
}

func TestAdminAuthenticationAndRules(t *testing.T) {
	fixture := newFixture(t, echoUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	admin := NewAdminHandler(testAdminToken, fixture.rules, fixture.limiter, testLogger(), nil, AdminOptions{})

	unauthorized := httptest.NewRecorder()
	admin.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/admin/rules/blacklist", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want 401", unauthorized.Code)
	}

	request := httptest.NewRequest(http.MethodPut, "/admin/rules/blacklist/192.0.2.10", nil)
	request.Header.Set("Authorization", "Bearer "+testAdminToken)
	response := httptest.NewRecorder()
	admin.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("add rule status = %d, body = %s", response.Code, response.Body.String())
	}

	blockedRequest := httptest.NewRequest(http.MethodGet, "/api/v1/users/1", nil)
	blockedRequest.RemoteAddr = "192.0.2.10:12345"
	blocked := httptest.NewRecorder()
	fixture.router.ServeHTTP(blocked, blockedRequest)
	if blocked.Code != http.StatusForbidden {
		t.Fatalf("blocked status = %d, want 403", blocked.Code)
	}

	request = httptest.NewRequest(http.MethodDelete, "/admin/rules/blacklist/192.0.2.10", nil)
	request.Header.Set("Authorization", "Bearer "+testAdminToken)
	response = httptest.NewRecorder()
	admin.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("delete rule status = %d, want 204", response.Code)
	}
}

func TestDynamicRateLimit(t *testing.T) {
	fixture := newFixture(t, echoUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	admin := NewAdminHandler(testAdminToken, fixture.rules, fixture.limiter, testLogger(), nil, AdminOptions{})
	request := httptest.NewRequest(http.MethodPut, "/admin/rules/rate-limit", strings.NewReader(`{"requests_per_second":1,"burst":1}`))
	request.Header.Set("Authorization", "Bearer "+testAdminToken)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	admin.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("update rate limit status = %d, body = %s", response.Code, response.Body.String())
	}

	first := httptest.NewRecorder()
	fixture.router.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/api/v1/users/1", nil))
	second := httptest.NewRecorder()
	fixture.router.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/api/v1/users/1", nil))
	if first.Code != http.StatusOK || second.Code != http.StatusTooManyRequests {
		t.Fatalf("rate limit statuses = %d then %d, want 200 then 429", first.Code, second.Code)
	}
}

func TestAccessLogCapturesRequestAndResponseBodies(t *testing.T) {
	gin.SetMode(gin.TestMode)
	queue := make(chan trafficlog.Entry, 1)
	router := gin.New()
	router.Use(RequestID(), AccessLog(AccessLogOptions{Queue: queue, SlowThreshold: time.Second, CaptureLimit: 8, Logger: testLogger()}), Recovery(testLogger()))
	router.POST("/error", func(c *gin.Context) {
		body, _ := io.ReadAll(c.Request.Body)
		c.Data(http.StatusBadRequest, "text/plain", bytes.Repeat(body, 2))
	})
	request := httptest.NewRequest(http.MethodPost, "/error", strings.NewReader("0123456789abcdef"))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	entry := <-queue
	if entry.Detail == nil || entry.Detail.RequestBody != "01234567" || entry.Detail.ResponseBody != "01234567" {
		t.Fatalf("unexpected captured bodies: %+v", entry.Detail)
	}
}

func TestAccessLogQueueFullDoesNotBlockRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	queue := make(chan trafficlog.Entry, 1)
	queue <- trafficlog.Entry{}
	router := gin.New()
	router.Use(RequestID(), AccessLog(AccessLogOptions{Queue: queue, SlowThreshold: time.Second, CaptureLimit: 8, Logger: testLogger()}))
	router.GET("/healthz", func(c *gin.Context) { c.Status(http.StatusOK) })
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
