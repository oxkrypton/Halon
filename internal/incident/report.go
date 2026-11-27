package incident

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ReportStatus describes the lifecycle of one investigated incident.
type ReportStatus string

const (
	StatusQueued     ReportStatus = "queued"
	StatusProcessing ReportStatus = "processing"
	StatusCompleted  ReportStatus = "completed"
	StatusFailed     ReportStatus = "failed"
)

// JSONB persists arbitrary JSON payloads in a PostgreSQL jsonb column.
type JSONB json.RawMessage

func (j JSONB) Value() (driver.Value, error) {
	if len(j) == 0 {
		return nil, nil
	}
	if !json.Valid(j) {
		return nil, fmt.Errorf("refuse to store invalid JSON in jsonb column")
	}
	return string(j), nil
}

func (j *JSONB) Scan(value any) error {
	switch v := value.(type) {
	case nil:
		*j = nil
		return nil
	case []byte:
		*j = append((*j)[:0], v...)
		return nil
	case string:
		*j = append((*j)[:0], v...)
		return nil
	default:
		return fmt.Errorf("cannot scan %T into JSONB", value)
	}
}

// Report is the persisted record of one incident investigation.
type Report struct {
	EventID          string       `gorm:"column:event_id;primaryKey;size:64" json:"event_id"`
	EventType        EventType    `gorm:"column:event_type;size:64;not null;default:'upstream_timeout'" json:"event_type"`
	ScopeKey         string       `gorm:"column:scope_key;type:text;not null;default:''" json:"scope_key"`
	RequestID        string       `gorm:"column:request_id;size:64;not null;default:''" json:"request_id,omitempty"`
	OccurredAt       time.Time    `gorm:"column:occurred_at;not null" json:"occurred_at"`
	Method           string       `gorm:"column:method;size:16;not null;default:''" json:"method,omitempty"`
	Path             string       `gorm:"column:path;type:text;not null;default:''" json:"path,omitempty"`
	Route            string       `gorm:"column:route;type:text;not null;default:''" json:"route,omitempty"`
	ClientIP         string       `gorm:"column:client_ip;size:45;not null;default:''" json:"client_ip,omitempty"`
	StatusCode       int          `gorm:"column:status_code;not null" json:"status_code,omitempty"`
	LatencyMS        float64      `gorm:"column:latency_ms;not null;default:0" json:"latency_ms,omitempty"`
	Summary          string       `gorm:"column:summary;type:text;not null;default:''" json:"summary,omitempty"`
	Occurrences      int          `gorm:"column:occurrences;not null;default:1" json:"occurrences"`
	Details          JSONB        `gorm:"column:details;type:jsonb" json:"details,omitempty"`
	Status           ReportStatus `gorm:"column:status;size:16;not null" json:"status"`
	FinalMessage     string       `gorm:"column:final_message;type:text;not null;default:''" json:"final_message"`
	Steps            int          `gorm:"column:steps;not null;default:0" json:"steps"`
	Executions       JSONB        `gorm:"column:executions;type:jsonb" json:"executions,omitempty"`
	PromptTokens     int          `gorm:"column:prompt_tokens;not null;default:0" json:"prompt_tokens"`
	CompletionTokens int          `gorm:"column:completion_tokens;not null;default:0" json:"completion_tokens"`
	TotalTokens      int          `gorm:"column:total_tokens;not null;default:0" json:"total_tokens"`
	Error            string       `gorm:"column:error;type:text;not null;default:''" json:"error,omitempty"`
	StartedAt        *time.Time   `gorm:"column:started_at" json:"started_at,omitempty"`
	FinishedAt       *time.Time   `gorm:"column:finished_at" json:"finished_at,omitempty"`
	CreatedAt        time.Time    `gorm:"column:created_at" json:"created_at"`
	UpdatedAt        time.Time    `gorm:"column:updated_at" json:"updated_at"`
}

func (Report) TableName() string {
	return "incident_reports"
}

// Event reconstructs the original incident event from the persisted row.
// Startup recovery uses it to re-queue investigations a previous process
// claimed but never finished.
func (r Report) Event() Event {
	eventType := r.EventType
	if eventType == "" {
		eventType = TypeUpstreamTimeout
	}
	scopeKey := strings.TrimSpace(r.ScopeKey)
	if scopeKey == "" {
		if route := strings.TrimSpace(r.Route); route != "" {
			scopeKey = route
		} else {
			scopeKey = strings.TrimSpace(r.ClientIP)
		}
	}
	occurrences := r.Occurrences
	if occurrences <= 0 {
		occurrences = 1
	}
	var details map[string]any
	if len(r.Details) > 0 {
		_ = json.Unmarshal(r.Details, &details)
	}
	return Event{
		EventID:     r.EventID,
		Type:        eventType,
		ScopeKey:    scopeKey,
		RequestID:   r.RequestID,
		OccurredAt:  r.OccurredAt,
		Method:      r.Method,
		Path:        r.Path,
		Route:       r.Route,
		ClientIP:    r.ClientIP,
		StatusCode:  r.StatusCode,
		LatencyMS:   r.LatencyMS,
		Summary:     r.Summary,
		Occurrences: occurrences,
		Details:     details,
	}
}

// NewReport creates a queued report row for an accepted event.
func NewReport(event Event) Report {
	eventType := event.Type
	if eventType == "" {
		eventType = TypeUpstreamTimeout
	}
	occurredAt := event.OccurredAt
	if occurredAt.IsZero() {
		occurredAt = time.Now().UTC()
	}
	occurrences := event.Occurrences
	if occurrences <= 0 {
		occurrences = 1
	}
	var details JSONB
	if len(event.Details) > 0 {
		if encoded, err := json.Marshal(event.Details); err == nil {
			details = JSONB(encoded)
		}
	}
	return Report{
		EventID:     event.EventID,
		EventType:   eventType,
		ScopeKey:    event.EffectiveScopeKey(),
		RequestID:   event.RequestID,
		OccurredAt:  occurredAt,
		Method:      event.Method,
		Path:        event.Path,
		Route:       event.Route,
		ClientIP:    event.ClientIP,
		StatusCode:  event.StatusCode,
		LatencyMS:   event.LatencyMS,
		Summary:     event.Summary,
		Occurrences: occurrences,
		Details:     details,
		Status:      StatusQueued,
	}
}
