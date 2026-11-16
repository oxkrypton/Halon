package gateway

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Metrics struct {
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

func NewMetrics() *Metrics {
	return &Metrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "halon_gateway_http_requests_total",
			Help: "Total number of HTTP requests handled by the gateway.",
		}, []string{"method", "route", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "halon_gateway_http_request_duration_seconds",
			Help:    "HTTP request duration in seconds.",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}, []string{"method", "route"}),
	}
}

func (m *Metrics) Observe(c *gin.Context, status int, duration time.Duration) {
	if m == nil {
		return
	}
	route := routeLabel(c)
	m.requests.WithLabelValues(c.Request.Method, route, strconv.Itoa(status)).Inc()
	m.duration.WithLabelValues(c.Request.Method, route).Observe(duration.Seconds())
}

func (m *Metrics) Handler() http.Handler {
	registry := prometheus.NewRegistry()
	registry.MustRegister(m.requests, m.duration)
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
}
