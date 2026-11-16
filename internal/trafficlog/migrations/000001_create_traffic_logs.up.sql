CREATE TABLE base_logs (
    id BIGSERIAL PRIMARY KEY,
    request_id VARCHAR(64) NOT NULL,
    client_ip VARCHAR(45) NOT NULL,
    method VARCHAR(16) NOT NULL,
    path TEXT NOT NULL,
    status_code SMALLINT NOT NULL CHECK (status_code BETWEEN 100 AND 599),
    latency_ms DOUBLE PRECISION NOT NULL CHECK (latency_ms >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_base_logs_created_at ON base_logs (created_at DESC);
CREATE INDEX idx_base_logs_ip_created_at ON base_logs (client_ip, created_at DESC);
CREATE INDEX idx_base_logs_path_status_created_at ON base_logs (path, status_code, created_at DESC);

CREATE TABLE error_body (
    id BIGSERIAL PRIMARY KEY,
    base_log_id BIGINT NOT NULL UNIQUE REFERENCES base_logs(id) ON DELETE CASCADE,
    request_body TEXT NOT NULL DEFAULT '',
    response_body TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_error_body_created_at ON error_body (created_at DESC);
