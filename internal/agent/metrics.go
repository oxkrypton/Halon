package agent

import (
	"net/http"

	"github.com/oxkrypton/Halon/internal/incident"
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

	activeSessions     prometheus.Gauge
	incidentAdmissions *prometheus.CounterVec
}

func NewMetrics() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		activeSessions: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "halon_agent_active_sessions",
			Help: "Number of active incident investigation sessions (queued, running, or cancelling).",
		}),
		incidentAdmissions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "halon_agent_incident_admissions_total",
			Help: "Incident events received by the agent, partitioned by bounded event type and admission outcome.",
		}, []string{"type", "outcome"}),
	}
	m.registry.MustRegister(m.activeSessions, m.incidentAdmissions)
	return m
}

func metricEventTypeLabel(eventType incident.EventType) string {
	if incident.IsBuiltinEventType(eventType) {
		return string(eventType)
	}
	return "other"
}

// IncIncidentAdmission increments the admission counter for one event type and outcome label.
func (m *Metrics) IncIncidentAdmission(eventType incident.EventType, outcome string) {
	if m == nil {
		return
	}
	m.incidentAdmissions.WithLabelValues(metricEventTypeLabel(eventType), outcome).Inc()
}

// SetActiveSessions records the current active session count.
func (m *Metrics) SetActiveSessions(count int) {
	if m == nil {
		return
	}
	m.activeSessions.Set(float64(count))
}

func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}
