package incident

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
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

// Report is the persisted record of one timeout incident investigation.
type Report struct {
	EventID          string       `gorm:"column:event_id;primaryKey;size:64" json:"event_id"`
	RequestID        string       `gorm:"column:request_id;size:64;not null" json:"request_id"`
	OccurredAt       time.Time    `gorm:"column:occurred_at;not null" json:"occurred_at"`
	Method           string       `gorm:"column:method;size:16;not null" json:"method"`
	Path             string       `gorm:"column:path;type:text;not null" json:"path"`
	Route            string       `gorm:"column:route;type:text;not null" json:"route"`
	ClientIP         string       `gorm:"column:client_ip;size:45;not null" json:"client_ip"`
	StatusCode       int          `gorm:"column:status_code;not null" json:"status_code"`
	LatencyMS        float64      `gorm:"column:latency_ms;not null" json:"latency_ms"`
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

// NewReport creates a queued report row for an accepted event.
func NewReport(event TimeoutEvent) Report {
	return Report{
		EventID:    event.EventID,
		RequestID:  event.RequestID,
		OccurredAt: event.OccurredAt,
		Method:     event.Method,
		Path:       event.Path,
		Route:      event.Route,
		ClientIP:   event.ClientIP,
		StatusCode: event.StatusCode,
		LatencyMS:  event.LatencyMS,
		Status:     StatusQueued,
	}
}
