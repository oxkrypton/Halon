DROP INDEX IF EXISTS idx_incident_reports_type_occurred_at;

DELETE FROM incident_reports WHERE status_code = 0;

ALTER TABLE incident_reports
    DROP CONSTRAINT IF EXISTS incident_reports_status_code_check;

ALTER TABLE incident_reports
    ADD CONSTRAINT incident_reports_status_code_check
    CHECK (status_code BETWEEN 100 AND 599);

ALTER TABLE incident_reports
    ALTER COLUMN request_id DROP DEFAULT,
    ALTER COLUMN method DROP DEFAULT,
    ALTER COLUMN path DROP DEFAULT,
    ALTER COLUMN route DROP DEFAULT,
    ALTER COLUMN client_ip DROP DEFAULT,
    ALTER COLUMN latency_ms DROP DEFAULT;

ALTER TABLE incident_reports
    DROP COLUMN IF EXISTS occurrences,
    DROP COLUMN IF EXISTS details,
    DROP COLUMN IF EXISTS summary,
    DROP COLUMN IF EXISTS scope_key,
    DROP COLUMN IF EXISTS event_type;
