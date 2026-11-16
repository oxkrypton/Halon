package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/oxkrypton/Halon/internal/incident"
	"github.com/oxkrypton/Halon/internal/trafficlog"
	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

func TestUpstreamTimeoutEmitsIncidentAndMetrics(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logQueue := make(chan trafficlog.Entry, 1)
	timeoutQueue := make(chan incident.TimeoutEvent, 1)
	rules, _ := NewIPRules(nil)
	limiter := NewRateLimiter(10, 10)
	metrics := NewMetrics()
	router := NewHTTPHandler(Dependencies{
		UserClient:           userClientStub{err: status.Error(codes.DeadlineExceeded, "deadline")},
		HealthClient:         healthClientStub{response: &healthpb.HealthCheckResponse{Status: healthpb.HealthCheckResponse_SERVING}},
		Repository:           pingerStub{},
		LogQueue:             logQueue,
		TimeoutQueue:         timeoutQueue,
		Logger:               testLogger(),
		Rules:                rules,
		Limiter:              limiter,
		Metrics:              metrics,
		RPCTimeout:           time.Second,
		SlowRequestThreshold: time.Second,
		BodyCaptureLimit:     1024,
	})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/users/1", nil)
	request.Header.Set("X-Request-ID", "timeout-request")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", response.Code)
	}
	event := <-timeoutQueue
	if event.RequestID != "timeout-request" || event.Route != "/api/v1/users/:id" || event.StatusCode != 504 {
		t.Fatalf("unexpected timeout event: %+v", event)
	}
	if err := event.Validate(); err != nil {
		t.Fatalf("invalid timeout event: %v", err)
	}

	admin := NewAdminHandler(testAdminToken, rules, limiter, testLogger(), metrics)
	metricsResponse := httptest.NewRecorder()
	admin.ServeHTTP(metricsResponse, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(metricsResponse.Body.String(), `halon_gateway_http_requests_total{method="GET",route="/api/v1/users/:id",status="504"} 1`) {
		t.Fatalf("timeout metric missing: %s", metricsResponse.Body.String())
	}
}

func TestTimeoutNotifierDeliversAuthenticatedEvent(t *testing.T) {
	received := make(chan incident.TimeoutEvent, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-webhook-token" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		var event incident.TimeoutEvent
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			t.Error(err)
		}
		received <- event
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	queue := make(chan incident.TimeoutEvent, 1)
	notifier := NewTimeoutNotifier(server.URL, "test-webhook-token", queue, testLogger())
	done := make(chan struct{})
	go func() {
		notifier.Run()
		close(done)
	}()
	event := incident.TimeoutEvent{
		EventID: "event-1", RequestID: "request-1", OccurredAt: time.Now().UTC(),
		Method: http.MethodGet, Path: "/api/v1/users/1", Route: "/api/v1/users/:id",
		StatusCode: 504, LatencyMS: 3000,
	}
	queue <- event
	close(queue)
	<-done
	if got := <-received; got.EventID != event.EventID || got.RequestID != event.RequestID {
		t.Fatalf("received event = %+v, want %+v", got, event)
	}
}
