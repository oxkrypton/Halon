package agent

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Admission outcomes for halon_agent_incident_admissions_total.
const (
	AdmissionAccepted   = "accepted"
	AdmissionDuplicate  = "duplicate"
	AdmissionSuppressed = "suppressed"
)

// Metrics holds the agent's Prometheus collectors. All methods are nil-safe
// so tests and partial wiring can skip instrumentation.
type Metrics struct {
	registry *prometheus.Registry

	redisUp            prometheus.Gauge
	incidentAdmissions *prometheus.CounterVec
}

func NewMetrics() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		redisUp: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "halon_agent_redis_up",
			Help: "Whether the shared-state Redis is reachable (1) or degraded (0). Always 1 in memory mode.",
		}),
		incidentAdmissions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "halon_agent_incident_admissions_total",
			Help: "Timeout events received by the agent, partitioned by admission outcome (accepted/duplicate/suppressed).",
		}, []string{"outcome"}),
	}
	m.redisUp.Set(1)
	m.registry.MustRegister(m.redisUp, m.incidentAdmissions)
	return m
}

// IncIncidentAdmission increments the admission counter for one outcome label.
func (m *Metrics) IncIncidentAdmission(outcome string) {
	if m == nil {
		return
	}
	m.incidentAdmissions.WithLabelValues(outcome).Inc()
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
