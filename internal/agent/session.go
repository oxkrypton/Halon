package agent

import (
	"errors"
	"time"

	"github.com/oxkrypton/Halon/internal/incident"
)

// SessionState describes the live execution phase of an active investigation
// session in the single-instance agent orchestrator.
type SessionState string

const (
	SessionQueued     SessionState = "queued"
	SessionRunning    SessionState = "running"
	SessionCancelling SessionState = "cancelling"
)

// ErrSessionNotFound is returned when an operator tries to cancel a session
// that is not currently queued or running.
var ErrSessionNotFound = errors.New("active session not found")

// SessionSnapshot is a read-only view of one active investigation session.
type SessionSnapshot struct {
	SessionID   string             `json:"session_id"`
	EventType   incident.EventType `json:"event_type"`
	ScopeKey    string             `json:"scope_key"`
	Route       string             `json:"route,omitempty"`
	Path        string             `json:"path,omitempty"`
	Occurrences int                `json:"occurrences"`
	State       SessionState       `json:"state"`
	CurrentStep int                `json:"current_step"`
	MaxSteps    int                `json:"max_steps"`
	QueuedAt    time.Time          `json:"queued_at"`
	StartedAt   *time.Time         `json:"started_at,omitempty"`
	UpdatedAt   time.Time          `json:"updated_at"`
}
