package incident

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// EventType identifies the kind of gateway anomaly that triggered an incident.
type EventType string

const (
	TypeUpstreamTimeout  EventType = "upstream_timeout"
	TypeUpstreamError    EventType = "upstream_error"
	TypeBreakerOpen      EventType = "breaker_open"
	TypeUpstream5xx      EventType = "upstream_5xx"
	TypeConcurrencyLimit EventType = "concurrency_limit"
	TypeRateLimitAbuse   EventType = "rate_limit_abuse"
)

const (
	maxEventIDLength   = 64
	maxEventTypeLength = 64
	maxScopeKeyLength  = 256
)

var builtinEventTypes = []EventType{
	TypeUpstreamTimeout,
	TypeUpstreamError,
	TypeBreakerOpen,
	TypeUpstream5xx,
	TypeConcurrencyLimit,
	TypeRateLimitAbuse,
}

// BuiltinEventTypes returns the built-in gateway incident event types in stable order.
func BuiltinEventTypes() []EventType {
	out := make([]EventType, len(builtinEventTypes))
	copy(out, builtinEventTypes)
	return out
}

// IsBuiltinEventType reports whether t is one of the built-in event types.
func IsBuiltinEventType(t EventType) bool {
	for _, candidate := range builtinEventTypes {
		if t == candidate {
			return true
		}
	}
	return false
}

// IsValidEventType reports whether t follows the canonical lowercase identifier
// format (`[a-z][a-z0-9_-]{0,63}`), allowing new trigger sources to register
// without altering the incident package.
func IsValidEventType(t EventType) bool {
	s := string(t)
	if len(s) == 0 || len(s) > maxEventTypeLength {
		return false
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if i == 0 {
			if ch < 'a' || ch > 'z' {
				return false
			}
			continue
		}
		isLower := ch >= 'a' && ch <= 'z'
		isDigit := ch >= '0' && ch <= '9'
		if !isLower && !isDigit && ch != '_' && ch != '-' {
			return false
		}
	}
	return true
}

// ParseEventType trims and normalizes raw into a validated EventType.
func ParseEventType(raw string) (EventType, error) {
	t := EventType(strings.ToLower(strings.TrimSpace(raw)))
	if !IsValidEventType(t) {
		return "", fmt.Errorf("invalid event type %q: must match ^[a-z][a-z0-9_-]{0,63}$", raw)
	}
	return t, nil
}

// Event is the contract emitted by the gateway when an investigable anomaly
// occurs. Gateway replicas coalesce repeated anomalies on the same (Type,
// ScopeKey) into one Event carrying occurrence counts, bounded samples, and
// optional type-specific details.
type Event struct {
	EventID         string         `json:"event_id"`
	Type            EventType      `json:"type"`
	ScopeKey        string         `json:"scope_key,omitempty"`
	RequestID       string         `json:"request_id,omitempty"`
	OccurredAt      time.Time      `json:"occurred_at"`
	Method          string         `json:"method,omitempty"`
	Path            string         `json:"path,omitempty"`
	Route           string         `json:"route,omitempty"`
	ClientIP        string         `json:"client_ip,omitempty"`
	StatusCode      int            `json:"status_code,omitempty"`
	LatencyMS       float64        `json:"latency_ms,omitempty"`
	Summary         string         `json:"summary,omitempty"`
	Occurrences     int            `json:"occurrences,omitempty"`
	WindowSeconds   int            `json:"window_seconds,omitempty"`
	SamplePaths     []string       `json:"sample_paths,omitempty"`
	SampleClientIPs []string       `json:"sample_client_ips,omitempty"`
	Details         map[string]any `json:"details,omitempty"`
}

// EffectiveScopeKey returns the trimmed ScopeKey, falling back to Route and then
// ClientIP when ScopeKey is omitted.
func (e Event) EffectiveScopeKey() string {
	if key := strings.TrimSpace(e.ScopeKey); key != "" {
		return key
	}
	if route := strings.TrimSpace(e.Route); route != "" {
		return route
	}
	return strings.TrimSpace(e.ClientIP)
}

// Validate checks structural invariants for an incoming incident event.
func (e Event) Validate() error {
	var validationErrors []error
	if strings.TrimSpace(e.EventID) == "" || len(e.EventID) > maxEventIDLength {
		validationErrors = append(validationErrors, errors.New("event_id is required and must be at most 64 characters"))
	}
	if !IsValidEventType(e.Type) {
		validationErrors = append(validationErrors, fmt.Errorf("type %q must match ^[a-z][a-z0-9_-]{0,63}$", e.Type))
	}
	scopeKey := e.EffectiveScopeKey()
	if scopeKey == "" || len(scopeKey) > maxScopeKeyLength {
		validationErrors = append(validationErrors, errors.New("scope_key (or route/client_ip fallback) is required and must be at most 256 characters"))
	}
	if e.StatusCode != 0 && (e.StatusCode < 100 || e.StatusCode > 599) {
		validationErrors = append(validationErrors, errors.New("status_code must be 0 or a valid HTTP status"))
	}
	if e.LatencyMS < 0 {
		validationErrors = append(validationErrors, errors.New("latency_ms cannot be negative"))
	}
	if e.Occurrences < 0 {
		validationErrors = append(validationErrors, errors.New("occurrences cannot be negative"))
	}
	if e.WindowSeconds < 0 {
		validationErrors = append(validationErrors, errors.New("window_seconds cannot be negative"))
	}
	return errors.Join(validationErrors...)
}
