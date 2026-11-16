CREATE TABLE incident_reports (
    event_id VARCHAR(64) PRIMARY KEY,
    request_id VARCHAR(64) NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL,
    method VARCHAR(16) NOT NULL,
    path TEXT NOT NULL,
    route TEXT NOT NULL,
    client_ip VARCHAR(45) NOT NULL,
    status_code SMALLINT NOT NULL CHECK (status_code BETWEEN 100 AND 599),
    latency_ms DOUBLE PRECISION NOT NULL CHECK (latency_ms >= 0),
    status VARCHAR(16) NOT NULL CHECK (status IN ('queued', 'processing', 'completed', 'failed')),
    final_message TEXT NOT NULL DEFAULT '',
    steps INTEGER NOT NULL DEFAULT 0 CHECK (steps >= 0),
    executions JSONB,
    prompt_tokens INTEGER NOT NULL DEFAULT 0 CHECK (prompt_tokens >= 0),
    completion_tokens INTEGER NOT NULL DEFAULT 0 CHECK (completion_tokens >= 0),
    total_tokens INTEGER NOT NULL DEFAULT 0 CHECK (total_tokens >= 0),
    error TEXT NOT NULL DEFAULT '',
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_incident_reports_occurred_at ON incident_reports (occurred_at DESC);
CREATE INDEX idx_incident_reports_status_occurred_at ON incident_reports (status, occurred_at DESC);
