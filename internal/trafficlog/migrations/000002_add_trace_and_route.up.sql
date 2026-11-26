ALTER TABLE base_logs
    ADD COLUMN trace_id VARCHAR(32) NOT NULL DEFAULT '',
    ADD COLUMN route TEXT NOT NULL DEFAULT '',
    ADD COLUMN upstream_latency_ms DOUBLE PRECISION NOT NULL DEFAULT 0 CHECK (upstream_latency_ms >= 0),
    ADD COLUMN upstream_result VARCHAR(16) NOT NULL DEFAULT '',
    ADD COLUMN retries SMALLINT NOT NULL DEFAULT 0 CHECK (retries >= 0);

CREATE INDEX idx_base_logs_trace_id ON base_logs (trace_id) WHERE trace_id <> '';
CREATE INDEX idx_base_logs_route_created_at ON base_logs (route, created_at DESC);
