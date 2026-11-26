package gateway

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/oxkrypton/Halon/internal/trafficlog"
)

// newResilientGateway wires a gateway whose upstream transport is replaced by
// a stub, so resilience policies can be tested deterministically.
func newResilientGateway(t *testing.T, cfg ResilienceConfig, base http.RoundTripper) (*gin.Engine, *Metrics, *ResilienceSet) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	metrics := NewMetrics()
	queue := make(chan trafficlog.Entry, 32)
	rules, limiter := newTestRedisStores(t, 1000, 1000)
	routes, err := ParseRoutes("/api/v1/=http://upstream.test")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Metrics = metrics
	set := NewResilienceSet(routes.Prefixes(), cfg, func(string) http.RoundTripper { return base })
	router := NewHTTPHandler(Dependencies{
		Routes: routes, Repository: pingerStub{}, LogQueue: queue,
		Logger: testLogger(), Rules: rules, Limiter: limiter, Metrics: metrics,
		Proxy: ProxyOptions{
			Timeout:              time.Second,
			RetryBodyBufferBytes: 1024,
			Resilience:           cfg,
		},
		Resilience:           set,
		SlowRequestThreshold: time.Second,
		BodyCaptureLimit:     1024,
	})
	return router, metrics, set
}

func stubResponse(req *http.Request, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

func metricsBody(t *testing.T, metrics *Metrics) string {
	t.Helper()
	response := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return response.Body.String()
}

func TestRetrySucceedsAfterTransientErrors(t *testing.T) {
	var attempts atomic.Int32
	base := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if attempts.Add(1) < 3 {
			return nil, errors.New("connection refused")
		}
		return stubResponse(req, "ok"), nil
	})
	router, metrics, _ := newResilientGateway(t, ResilienceConfig{RetryMaxAttempts: 3}, base)

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/users/1", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
	if body := metricsBody(t, metrics); !strings.Contains(body, `halon_gateway_retries_total{route="/api/v1/"} 2`) {
		t.Fatalf("retry metric missing: %s", body)
	}
}

func TestRetrySkippedForNonIdempotentMethod(t *testing.T) {
	var attempts atomic.Int32
	base := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		attempts.Add(1)
		return nil, errors.New("connection refused")
	})
	router, _, _ := newResilientGateway(t, ResilienceConfig{RetryMaxAttempts: 3}, base)

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/users", strings.NewReader(`{"name":"a"}`))
	router.ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", response.Code)
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1 (POST without Idempotency-Key must not retry)", got)
	}
}

func TestRetryReplaysBodyWithIdempotencyKey(t *testing.T) {
	var attempts atomic.Int32
	bodies := make(chan string, 4)
	base := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		data, _ := io.ReadAll(req.Body)
		bodies <- string(data)
		if attempts.Add(1) < 2 {
			return nil, errors.New("connection reset")
		}
		return stubResponse(req, "created"), nil
	})
	router, _, _ := newResilientGateway(t, ResilienceConfig{RetryMaxAttempts: 3}, base)

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/users", strings.NewReader(`{"name":"a"}`))
	request.Header.Set("Idempotency-Key", "key-123")
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
	first, second := <-bodies, <-bodies
	if first != `{"name":"a"}` || second != first {
		t.Fatalf("replayed bodies = %q, %q; want identical payload", first, second)
	}
}

func TestCircuitBreakerOpensAfterFailures(t *testing.T) {
	var attempts atomic.Int32
	base := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		attempts.Add(1)
		return nil, errors.New("connection refused")
	})
	router, metrics, set := newResilientGateway(t, ResilienceConfig{
		RetryMaxAttempts:         1,
		BreakerEnabled:           true,
		BreakerFailureThreshold:  2,
		BreakerOpenDuration:      time.Minute,
		BreakerHalfOpenSuccesses: 1,
	}, base)

	for i := 0; i < 2; i++ {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/users/1", nil))
		if response.Code != http.StatusBadGateway {
			t.Fatalf("attempt %d: status = %d, want 502", i+1, response.Code)
		}
	}

	// The breaker is open now: the next request must fail fast with 503 and
	// never reach the upstream transport.
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/users/1", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.Code)
	}
	if response.Header().Get("Retry-After") == "" {
		t.Fatal("Retry-After header missing on open-breaker response")
	}
	if !strings.Contains(response.Body.String(), "UPSTREAM_UNAVAILABLE") {
		t.Fatalf("body = %s, want UPSTREAM_UNAVAILABLE error", response.Body.String())
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2 (open breaker must short-circuit)", got)
	}

	statuses := set.BreakerStatuses()
	if len(statuses) != 1 || statuses[0].State != "open" || statuses[0].RemainingDelaySeconds <= 0 {
		t.Fatalf("breaker statuses = %+v, want one open breaker with remaining delay", statuses)
	}
	body := metricsBody(t, metrics)
	if !strings.Contains(body, `halon_gateway_circuit_breaker_state{route="/api/v1/"} 2`) {
		t.Fatalf("breaker state metric missing: %s", body)
	}
	if !strings.Contains(body, `halon_gateway_circuit_breaker_trips_total{route="/api/v1/"} 1`) {
		t.Fatalf("breaker trips metric missing: %s", body)
	}
}

func TestBulkheadRejectsWhenFull(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	base := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		entered <- struct{}{}
		<-release
		return stubResponse(req, "ok"), nil
	})
	router, metrics, _ := newResilientGateway(t, ResilienceConfig{ConcurrencyPerRoute: 1}, base)

	first := make(chan int, 1)
	go func() {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/users/1", nil))
		first <- response.Code
	}()
	<-entered

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/users/2", nil))
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", response.Code)
	}
	if response.Header().Get("Retry-After") != "1" {
		t.Fatalf("Retry-After = %q, want 1", response.Header().Get("Retry-After"))
	}
	if !strings.Contains(response.Body.String(), "CONCURRENCY_LIMIT") {
		t.Fatalf("body = %s, want CONCURRENCY_LIMIT error", response.Body.String())
	}
	close(release)
	if code := <-first; code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", code)
	}
	if body := metricsBody(t, metrics); !strings.Contains(body, `halon_gateway_rejected_requests_total{reason="concurrency_limit"} 1`) {
		t.Fatalf("rejection metric missing: %s", body)
	}
}

func TestRequestBodyLimitReturns413(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := echoUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	})
	queue := make(chan trafficlog.Entry, 32)
	rules, limiter := newTestRedisStores(t, 1000, 1000)
	routes, err := ParseRoutes("/api/v1/=" + upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	metrics := NewMetrics()
	router := NewHTTPHandler(Dependencies{
		Routes: routes, Repository: pingerStub{}, LogQueue: queue,
		Logger: testLogger(), Rules: rules, Limiter: limiter, Metrics: metrics,
		Proxy:                ProxyOptions{Timeout: time.Second, MaxRequestBodyBytes: 8},
		SlowRequestThreshold: time.Second, BodyCaptureLimit: 1024,
	})

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/users", strings.NewReader(strings.Repeat("x", 100)))
	router.ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", response.Code)
	}
	if !strings.Contains(response.Body.String(), "REQUEST_TOO_LARGE") {
		t.Fatalf("body = %s, want REQUEST_TOO_LARGE error", response.Body.String())
	}
	if body := metricsBody(t, metrics); !strings.Contains(body, `halon_gateway_rejected_requests_total{reason="body_too_large"} 1`) {
		t.Fatalf("rejection metric missing: %s", body)
	}
}

func TestResponseHeaderTimeoutClassifiedAs504(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := echoUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	queue := make(chan trafficlog.Entry, 32)
	rules, limiter := newTestRedisStores(t, 1000, 1000)
	routes, err := ParseRoutes("/api/v1/=" + upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	router := NewHTTPHandler(Dependencies{
		Routes: routes, Repository: pingerStub{}, LogQueue: queue,
		Logger: testLogger(), Rules: rules, Limiter: limiter,
		Proxy: ProxyOptions{
			Timeout:               5 * time.Second,
			ResponseHeaderTimeout: 50 * time.Millisecond,
		},
		SlowRequestThreshold: time.Second, BodyCaptureLimit: 1024,
	})

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/users/1", nil))
	if response.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504 (response header timeout must keep timeout semantics)", response.Code)
	}
}

func TestMetricsEndpointHonorsAuthOption(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rules, limiter := newTestRedisStores(t, 1, 1)
	metrics := NewMetrics()
	admin := NewAdminHandler(testAdminToken, rules, limiter, testLogger(), metrics, AdminOptions{MetricsRequireAuth: true})

	response := httptest.NewRecorder()
	admin.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 without token", response.Code)
	}

	response = httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	request.Header.Set("Authorization", "Bearer "+testAdminToken)
	admin.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with token", response.Code)
	}
}

func TestAdminBreakersEndpoint(t *testing.T) {
	base := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return stubResponse(req, "ok"), nil
	})
	_, metrics, set := newResilientGateway(t, ResilienceConfig{
		BreakerEnabled:           true,
		BreakerFailureThreshold:  5,
		BreakerOpenDuration:      time.Minute,
		BreakerHalfOpenSuccesses: 1,
	}, base)
	rules, limiter := newTestRedisStores(t, 1, 1)
	admin := NewAdminHandler(testAdminToken, rules, limiter, testLogger(), metrics, AdminOptions{Resilience: set})

	response := httptest.NewRecorder()
	admin.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/admin/breakers", nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 without token", response.Code)
	}

	response = httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/admin/breakers", nil)
	request.Header.Set("Authorization", "Bearer "+testAdminToken)
	admin.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with token", response.Code)
	}
	if !strings.Contains(response.Body.String(), `"route":"/api/v1/"`) || !strings.Contains(response.Body.String(), `"state":"closed"`) {
		t.Fatalf("body = %s, want closed breaker for /api/v1/", response.Body.String())
	}
}
