DROP INDEX IF EXISTS idx_base_logs_route_created_at;
DROP INDEX IF EXISTS idx_base_logs_trace_id;

ALTER TABLE base_logs
    DROP COLUMN IF EXISTS retries,
    DROP COLUMN IF EXISTS upstream_result,
    DROP COLUMN IF EXISTS upstream_latency_ms,
    DROP COLUMN IF EXISTS route,
    DROP COLUMN IF EXISTS trace_id;
