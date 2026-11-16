package incident

import (
	"errors"
	"time"
)

// TimeoutEvent is emitted by the gateway after an upstream gRPC deadline.
type TimeoutEvent struct {
	EventID    string    `json:"event_id"`
	RequestID  string    `json:"request_id"`
	OccurredAt time.Time `json:"occurred_at"`
	Method     string    `json:"method"`
	Path       string    `json:"path"`
	Route      string    `json:"route"`
	ClientIP   string    `json:"client_ip"`
	StatusCode int       `json:"status_code"`
	LatencyMS  float64   `json:"latency_ms"`
}

func (e TimeoutEvent) Validate() error {
	var validationErrors []error
	if e.EventID == "" || e.RequestID == "" {
		validationErrors = append(validationErrors, errors.New("event_id and request_id are required"))
	}
	if e.Method == "" || e.Path == "" || e.Route == "" {
		validationErrors = append(validationErrors, errors.New("method, path, and route are required"))
	}
	if e.StatusCode < 100 || e.StatusCode > 599 {
		validationErrors = append(validationErrors, errors.New("status_code must be a valid HTTP status"))
	}
	if e.LatencyMS < 0 {
		validationErrors = append(validationErrors, errors.New("latency_ms cannot be negative"))
	}
	return errors.Join(validationErrors...)
}
