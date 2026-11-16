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
	userv1 "github.com/oxkrypton/Halon/api/gen/user/v1"
	"github.com/oxkrypton/Halon/internal/trafficlog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

const testAdminToken = "test-admin-token"

type userClientStub struct {
	response *userv1.GetUserResponse
	err      error
}

func (s userClientStub) GetUser(context.Context, *userv1.GetUserRequest, ...grpc.CallOption) (*userv1.GetUserResponse, error) {
	return s.response, s.err
}

type healthClientStub struct {
	response *healthpb.HealthCheckResponse
	err      error
}

func (s healthClientStub) Check(context.Context, *healthpb.HealthCheckRequest, ...grpc.CallOption) (*healthpb.HealthCheckResponse, error) {
	return s.response, s.err
}

type pingerStub struct {
	err error
}

func (s pingerStub) Ping(context.Context) error {
	return s.err
}

type fixture struct {
	router  *gin.Engine
	rules   *IPRules
	limiter *RateLimiter
	queue   chan trafficlog.Entry
}

func newFixture(client userv1.UserServiceClient) *fixture {
	gin.SetMode(gin.TestMode)
	queue := make(chan trafficlog.Entry, 32)
	rules, _ := NewIPRules(nil)
	limiter := NewRateLimiter(100, 100)
	return &fixture{
		router: NewHTTPHandler(Dependencies{
			UserClient:           client,
			HealthClient:         healthClientStub{response: &healthpb.HealthCheckResponse{Status: healthpb.HealthCheckResponse_SERVING}},
			Repository:           pingerStub{},
			LogQueue:             queue,
			Logger:               testLogger(),
			Rules:                rules,
			Limiter:              limiter,
			RPCTimeout:           time.Second,
			SlowRequestThreshold: time.Second,
			BodyCaptureLimit:     1024,
		}),
		rules:   rules,
		limiter: limiter,
		queue:   queue,
	}
}

func TestGetUserSuccess(t *testing.T) {
	fixture := newFixture(userClientStub{response: &userv1.GetUserResponse{
		User: &userv1.User{Id: "1", Name: "Alice", Email: "alice@example.com"},
	}})
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

func TestGetUserErrors(t *testing.T) {
	tests := []struct {
		name       string
		rpcError   error
		httpStatus int
		code       string
	}{
		{name: "invalid argument", rpcError: status.Error(codes.InvalidArgument, "bad id"), httpStatus: 400, code: "INVALID_ARGUMENT"},
		{name: "not found", rpcError: status.Error(codes.NotFound, "missing user"), httpStatus: 404, code: "NOT_FOUND"},
		{name: "timeout", rpcError: status.Error(codes.DeadlineExceeded, "timeout"), httpStatus: 504, code: "UPSTREAM_TIMEOUT"},
		{name: "unavailable", rpcError: status.Error(codes.Unavailable, "offline"), httpStatus: 503, code: "UPSTREAM_UNAVAILABLE"},
		{name: "internal", rpcError: status.Error(codes.Internal, "secret detail"), httpStatus: 502, code: "UPSTREAM_ERROR"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newFixture(userClientStub{err: tt.rpcError})
			response := httptest.NewRecorder()
			fixture.router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/users/1", nil))

			if response.Code != tt.httpStatus {
				t.Fatalf("status = %d, want %d", response.Code, tt.httpStatus)
			}
			var body errorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error.Code != tt.code || body.Error.RequestID == "" {
				t.Fatalf("unexpected error response: %+v", body)
			}
			entry := <-fixture.queue
			if entry.Detail == nil || !strings.Contains(entry.Detail.ResponseBody, tt.code) {
				t.Fatalf("error response was not captured: %+v", entry)
			}
		})
	}
}

func TestReadinessChecksDependencies(t *testing.T) {
	fixture := newFixture(userClientStub{})
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
		UserClient:           userClientStub{},
		HealthClient:         healthClientStub{err: errors.New("offline")},
		Repository:           pingerStub{err: errors.New("offline")},
		LogQueue:             queue,
		Logger:               testLogger(),
		Rules:                rules,
		Limiter:              NewRateLimiter(1, 1),
		RPCTimeout:           time.Second,
		SlowRequestThreshold: time.Second,
		BodyCaptureLimit:     1024,
	})
	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("not ready status = %d, want 503", response.Code)
	}
}

func TestAdminAuthenticationAndRules(t *testing.T) {
	fixture := newFixture(userClientStub{response: &userv1.GetUserResponse{User: &userv1.User{Id: "1", Name: "Alice"}}})
	admin := NewAdminHandler(testAdminToken, fixture.rules, fixture.limiter, testLogger(), nil)

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
	fixture := newFixture(userClientStub{response: &userv1.GetUserResponse{User: &userv1.User{Id: "1"}}})
	admin := NewAdminHandler(testAdminToken, fixture.rules, fixture.limiter, testLogger(), nil)
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
