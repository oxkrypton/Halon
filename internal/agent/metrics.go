package agent

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds the agent's Prometheus collectors. All methods are nil-safe
// so tests and partial wiring can skip instrumentation.
type Metrics struct {
	registry *prometheus.Registry

	redisUp prometheus.Gauge
}

func NewMetrics() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		redisUp: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "halon_agent_redis_up",
			Help: "Whether the shared-state Redis is reachable (1) or degraded (0). Always 1 in memory mode.",
		}),
	}
	m.redisUp.Set(1)
	m.registry.MustRegister(m.redisUp)
	return m
}

// SetRedisUp records shared-state Redis reachability. In memory mode it is
// simply never called and the gauge stays at its initial 1.
func (m *Metrics) SetRedisUp(up bool) {
	if m == nil {
		return
	}
	if up {
		m.redisUp.Set(1)
		return
	}
	m.redisUp.Set(0)
}

func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}
