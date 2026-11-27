package gateway

import (
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/oxkrypton/Halon/internal/incident"
)

// IncidentSink is a nil-safe, non-blocking producer that filters and enqueues
// gateway anomaly events for IncidentNotifier.
type IncidentSink struct {
	queue    chan<- incident.Event
	policies IncidentPolicySet
	metrics  *Metrics
	logger   *slog.Logger
	dropped  atomic.Uint64
}

// NewIncidentSink builds an IncidentSink backed by queue. Returning nil when
// queue is nil keeps call sites branch-free.
func NewIncidentSink(queue chan<- incident.Event, policies IncidentPolicySet, metrics *Metrics, logger *slog.Logger) *IncidentSink {
	if queue == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &IncidentSink{
		queue:    queue,
		policies: policies,
		metrics:  metrics,
		logger:   logger,
	}
}

// Emit fills default identity/timestamp/scope fields, respects per-type
// enablement, and enqueues event without blocking.
func (s *IncidentSink) Emit(event incident.Event) {
	if s == nil || s.queue == nil {
		return
	}
	if event.Type == "" {
		return
	}
	if !s.policies.Enabled(event.Type) {
		return
	}
	if event.EventID == "" {
		event.EventID = newRequestID()
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now().UTC()
	}
	event.ScopeKey = event.EffectiveScopeKey()
	if event.ScopeKey == "" {
		return
	}
	select {
	case s.queue <- event:
	default:
		s.metrics.IncIncidentEventsDropped(event.Type)
		count := s.dropped.Add(1)
		if count == 1 || count%100 == 0 {
			s.logger.Warn("incident event queue full; dropping event",
				"event_type", event.Type, "scope_key", event.ScopeKey, "dropped_total", count)
		}
	}
}
