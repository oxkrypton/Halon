package trafficlog

import "time"

// Entry groups the base request log with its optional detailed error body.
type Entry struct {
	Base   BaseLog
	Detail *ErrorBody
}

type BaseLog struct {
	ID        int64
	RequestID string  `gorm:"column:request_id;size:64;not null"`
	ClientIP  string  `gorm:"column:client_ip;size:45;not null"`
	Method    string  `gorm:"column:method;size:16;not null"`
	Path      string  `gorm:"column:path;type:text;not null"`
	Status    int     `gorm:"column:status_code;not null"`
	LatencyMS float64 `gorm:"column:latency_ms;not null"`
	CreatedAt time.Time
}

func (BaseLog) TableName() string {
	return "base_logs"
}

type ErrorBody struct {
	ID           int64
	BaseLogID    int64  `gorm:"column:base_log_id;not null;uniqueIndex"`
	RequestBody  string `gorm:"column:request_body;type:text;not null"`
	ResponseBody string `gorm:"column:response_body;type:text;not null"`
	CreatedAt    time.Time
}

func (ErrorBody) TableName() string {
	return "error_body"
}
