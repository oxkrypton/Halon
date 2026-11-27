package gateway

import (
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/oxkrypton/Halon/internal/incident"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Rejection reasons for halon_gateway_rejected_requests_total.
const (
	RejectReasonBlacklist        = "blacklist"
	RejectReasonRateLimit        = "rate_limit"
	RejectReasonConcurrencyLimit = "concurrency_limit"
	RejectReasonBodyTooLarge     = "body_too_large"
)

// Upstream call results for halon_gateway_upstream_requests_total.
const (
	UpstreamResultSuccess = "success"
	UpstreamResultTimeout = "timeout"
	UpstreamResultError   = "error"
)

// Circuit breaker states as reported by halon_gateway_circuit_breaker_state.
const (
	breakerStateClosed   = 0
	breakerStateHalfOpen = 1
	breakerStateOpen     = 2
)

// Metrics holds the gateway's Prometheus collectors. All methods are nil-safe
// so tests and partial wiring can skip instrumentation.
type Metrics struct {
	registry *prometheus.Registry

	requests      *prometheus.CounterVec
	duration      *prometheus.HistogramVec
	inFlight      prometheus.Gauge
	respSize      *prometheus.HistogramVec
	rejected      *prometheus.CounterVec
	adminRequests *prometheus.CounterVec

	upstreamRequests *prometheus.CounterVec
	upstreamDuration *prometheus.HistogramVec
	upstreamInFlight *prometheus.GaugeVec

	retries          *prometheus.CounterVec
	retriesExhausted *prometheus.CounterVec
	breakerState     *prometheus.GaugeVec
	breakerTrips     *prometheus.CounterVec

	trafficlogDropped       prometheus.Counter
	incidentEventsDropped   *prometheus.CounterVec
	incidentEventsCoalesced *prometheus.CounterVec
	incidentsEmitted        *prometheus.CounterVec
	webhookNotifyFailures   prometheus.Counter

	redisUp      prometheus.Gauge
	redisUpState atomic.Bool
}

func NewMetrics() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "halon_gateway_http_requests_total",
			Help: "Total number of HTTP requests handled by the gateway.",
		}, []string{"method", "route", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "halon_gateway_http_request_duration_seconds",
			Help:    "HTTP request duration in seconds.",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}, []string{"method", "route"}),
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "halon_gateway_in_flight_requests",
			Help: "Number of HTTP requests currently being handled by the gateway.",
		}),
		respSize: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "halon_gateway_response_size_bytes",
			Help:    "HTTP response body size in bytes.",
			Buckets: []float64{128, 1024, 8 << 10, 64 << 10, 512 << 10, 4 << 20, 16 << 20},
		}, []string{"route"}),
		rejected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "halon_gateway_rejected_requests_total",
			Help: "Requests rejected before reaching an upstream, by reason.",
		}, []string{"reason"}),
		adminRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "halon_gateway_admin_requests_total",
			Help: "Admin API requests by role, permission and decision.",
		}, []string{"role", "permission", "result"}),
		upstreamRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "halon_gateway_upstream_requests_total",
			Help: "Upstream call attempts by route and result (success/timeout/error).",
		}, []string{"route", "result"}),
		upstreamDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "halon_gateway_upstream_request_duration_seconds",
			Help:    "Upstream call attempt duration in seconds.",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}, []string{"route"}),
		upstreamInFlight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "halon_gateway_upstream_in_flight",
			Help: "Upstream calls currently in flight, per route.",
		}, []string{"route"}),
		retries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "halon_gateway_retries_total",
			Help: "Upstream retry attempts scheduled, per route.",
		}, []string{"route"}),
		retriesExhausted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "halon_gateway_retry_exhausted_total",
			Help: "Requests whose upstream retry budget was exhausted, per route.",
		}, []string{"route"}),
		breakerState: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "halon_gateway_circuit_breaker_state",
			Help: "Circuit breaker state per route: 0=closed 1=half-open 2=open.",
		}, []string{"route"}),
		breakerTrips: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "halon_gateway_circuit_breaker_trips_total",
			Help: "Number of times a circuit breaker transitioned to open, per route.",
		}, []string{"route"}),
		trafficlogDropped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "halon_gateway_trafficlog_dropped_total",
			Help: "Traffic log entries dropped because the bounded queue was full.",
		}),
		incidentEventsDropped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "halon_gateway_incident_events_dropped_total",
			Help: "Incident events dropped because the bounded queue was full, by event type.",
		}, []string{"type"}),
		incidentEventsCoalesced: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "halon_gateway_incident_events_coalesced_total",
			Help: "Raw anomaly events merged into an active coalescing window, by event type and route.",
		}, []string{"type", "route"}),
		incidentsEmitted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "halon_gateway_incidents_emitted_total",
			Help: "Coalesced incident webhooks delivered to the agent, by event type and route.",
		}, []string{"type", "route"}),
		webhookNotifyFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "halon_gateway_webhook_notify_failures_total",
			Help: "Incident webhook deliveries that failed after all attempts.",
		}),
		redisUp: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "halon_gateway_redis_up",
			Help: "Whether Redis is reachable (1) or degraded (0).",
		}),
	}
	m.redisUp.Set(1)
	m.redisUpState.Store(true)
	m.registry.MustRegister(
		m.requests, m.duration, m.inFlight, m.respSize, m.rejected, m.adminRequests,
		m.upstreamRequests, m.upstreamDuration, m.upstreamInFlight,
		m.retries, m.retriesExhausted, m.breakerState, m.breakerTrips,
		m.trafficlogDropped, m.incidentEventsDropped, m.incidentEventsCoalesced, m.incidentsEmitted, m.webhookNotifyFailures,
		m.redisUp,
	)
	return m
}

func (m *Metrics) Observe(c *gin.Context, status int, duration time.Duration) {
	if m == nil {
		return
	}
	route := routeLabel(c)
	m.requests.WithLabelValues(c.Request.Method, route, strconv.Itoa(status)).Inc()
	m.duration.WithLabelValues(c.Request.Method, route).Observe(duration.Seconds())
	if size := c.Writer.Size(); size > 0 {
		m.respSize.WithLabelValues(route).Observe(float64(size))
	}
}

func (m *Metrics) IncInFlight() {
	if m != nil {
		m.inFlight.Inc()
	}
}

func (m *Metrics) DecInFlight() {
	if m != nil {
		m.inFlight.Dec()
	}
}

func (m *Metrics) IncRejected(reason string) {
	if m != nil {
		m.rejected.WithLabelValues(reason).Inc()
	}
}

func (m *Metrics) IncAdminRequest(role, permission, result string) {
	if m != nil {
		m.adminRequests.WithLabelValues(role, permission, result).Inc()
	}
}

func (m *Metrics) ObserveUpstream(route, result string, duration time.Duration) {
	if m == nil {
		return
	}
	m.upstreamRequests.WithLabelValues(route, result).Inc()
	m.upstreamDuration.WithLabelValues(route).Observe(duration.Seconds())
}

func (m *Metrics) IncUpstreamInFlight(route string) {
	if m != nil {
		m.upstreamInFlight.WithLabelValues(route).Inc()
	}
}

func (m *Metrics) DecUpstreamInFlight(route string) {
	if m != nil {
		m.upstreamInFlight.WithLabelValues(route).Dec()
	}
}

func (m *Metrics) IncRetry(route string) {
	if m != nil {
		m.retries.WithLabelValues(route).Inc()
	}
}

func (m *Metrics) IncRetryExhausted(route string) {
	if m != nil {
		m.retriesExhausted.WithLabelValues(route).Inc()
	}
}

func (m *Metrics) SetBreakerState(route string, state int) {
	if m != nil {
		m.breakerState.WithLabelValues(route).Set(float64(state))
	}
}

func (m *Metrics) IncBreakerTrips(route string) {
	if m != nil {
		m.breakerTrips.WithLabelValues(route).Inc()
	}
}

func (m *Metrics) IncTrafficlogDropped() {
	if m != nil {
		m.trafficlogDropped.Inc()
	}
}

func metricIncidentTypeLabel(eventType incident.EventType) string {
	if incident.IsBuiltinEventType(eventType) {
		return string(eventType)
	}
	return "other"
}

func metricRouteLabel(route string) string {
	if route == "" {
		return "none"
	}
	return route
}

func (m *Metrics) IncIncidentEventsDropped(eventType incident.EventType) {
	if m != nil {
		m.incidentEventsDropped.WithLabelValues(metricIncidentTypeLabel(eventType)).Inc()
	}
}

func (m *Metrics) IncIncidentEventsCoalesced(eventType incident.EventType, route string) {
	if m != nil {
		m.incidentEventsCoalesced.WithLabelValues(metricIncidentTypeLabel(eventType), metricRouteLabel(route)).Inc()
	}
}

func (m *Metrics) IncIncidentsEmitted(eventType incident.EventType, route string) {
	if m != nil {
		m.incidentsEmitted.WithLabelValues(metricIncidentTypeLabel(eventType), metricRouteLabel(route)).Inc()
	}
}

func (m *Metrics) IncWebhookNotifyFailures() {
	if m != nil {
		m.webhookNotifyFailures.Inc()
	}
}

// SetRedisUp records Redis reachability.
func (m *Metrics) SetRedisUp(up bool) {
	if m == nil {
		return
	}
	m.redisUpState.Store(up)
	if up {
		m.redisUp.Set(1)
		return
	}
	m.redisUp.Set(0)
}

// RedisUp reports the latest recorded Redis reachability state.
func (m *Metrics) RedisUp() bool {
	if m == nil {
		return true
	}
	return m.redisUpState.Load()
}

// RejectedCounts breaks down requests rejected before reaching an upstream.
type RejectedCounts struct {
	Total            int64 `json:"total"`
	Blacklist        int64 `json:"blacklist"`
	RateLimit        int64 `json:"rate_limit"`
	ConcurrencyLimit int64 `json:"concurrency_limit"`
	BodyTooLarge     int64 `json:"body_too_large"`
}

// RuntimeMetricsSnapshot aggregates per-replica runtime counters and gauges
// for the console overview page.
type RuntimeMetricsSnapshot struct {
	InFlightRequests      int64          `json:"in_flight_requests"`
	UpstreamInFlight      int64          `json:"upstream_in_flight"`
	Rejected              RejectedCounts `json:"rejected"`
	RetriesTotal          int64          `json:"retries_total"`
	RetryExhaustedTotal   int64          `json:"retry_exhausted_total"`
	BreakerTripsTotal     int64          `json:"breaker_trips_total"`
	TrafficlogDropped     int64          `json:"trafficlog_dropped"`
	IncidentEventsDropped int64          `json:"incident_events_dropped"`
	WebhookNotifyFailures int64          `json:"webhook_notify_failures"`
}

// RuntimeSnapshot reads current counter and gauge totals from the registry.
func (m *Metrics) RuntimeSnapshot() RuntimeMetricsSnapshot {
	var snapshot RuntimeMetricsSnapshot
	if m == nil || m.registry == nil {
		return snapshot
	}
	families, err := m.registry.Gather()
	if err != nil {
		return snapshot
	}
	for _, family := range families {
		switch family.GetName() {
		case "halon_gateway_in_flight_requests":
			for _, metric := range family.GetMetric() {
				snapshot.InFlightRequests += int64(metric.GetGauge().GetValue())
			}
		case "halon_gateway_upstream_in_flight":
			for _, metric := range family.GetMetric() {
				snapshot.UpstreamInFlight += int64(metric.GetGauge().GetValue())
			}
		case "halon_gateway_rejected_requests_total":
			for _, metric := range family.GetMetric() {
				val := int64(metric.GetCounter().GetValue())
				snapshot.Rejected.Total += val
				for _, label := range metric.GetLabel() {
					if label.GetName() != "reason" {
						continue
					}
					switch label.GetValue() {
					case RejectReasonBlacklist:
						snapshot.Rejected.Blacklist += val
					case RejectReasonRateLimit:
						snapshot.Rejected.RateLimit += val
					case RejectReasonConcurrencyLimit:
						snapshot.Rejected.ConcurrencyLimit += val
					case RejectReasonBodyTooLarge:
						snapshot.Rejected.BodyTooLarge += val
					}
				}
			}
		case "halon_gateway_retries_total":
			for _, metric := range family.GetMetric() {
				snapshot.RetriesTotal += int64(metric.GetCounter().GetValue())
			}
		case "halon_gateway_retry_exhausted_total":
			for _, metric := range family.GetMetric() {
				snapshot.RetryExhaustedTotal += int64(metric.GetCounter().GetValue())
			}
		case "halon_gateway_circuit_breaker_trips_total":
			for _, metric := range family.GetMetric() {
				snapshot.BreakerTripsTotal += int64(metric.GetCounter().GetValue())
			}
		case "halon_gateway_trafficlog_dropped_total":
			for _, metric := range family.GetMetric() {
				snapshot.TrafficlogDropped += int64(metric.GetCounter().GetValue())
			}
		case "halon_gateway_incident_events_dropped_total":
			for _, metric := range family.GetMetric() {
				snapshot.IncidentEventsDropped += int64(metric.GetCounter().GetValue())
			}
		case "halon_gateway_webhook_notify_failures_total":
			for _, metric := range family.GetMetric() {
				snapshot.WebhookNotifyFailures += int64(metric.GetCounter().GetValue())
			}
		}
	}
	return snapshot
}

func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}
