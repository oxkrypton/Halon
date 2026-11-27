ALTER TABLE incident_reports
    ADD COLUMN event_type VARCHAR(64) NOT NULL DEFAULT 'upstream_timeout',
    ADD COLUMN scope_key TEXT NOT NULL DEFAULT '',
    ADD COLUMN summary TEXT NOT NULL DEFAULT '',
    ADD COLUMN details JSONB,
    ADD COLUMN occurrences INTEGER NOT NULL DEFAULT 1 CHECK (occurrences >= 1);

ALTER TABLE incident_reports
    ALTER COLUMN request_id SET DEFAULT '',
    ALTER COLUMN method SET DEFAULT '',
    ALTER COLUMN path SET DEFAULT '',
    ALTER COLUMN route SET DEFAULT '',
    ALTER COLUMN client_ip SET DEFAULT '',
    ALTER COLUMN latency_ms SET DEFAULT 0;

UPDATE incident_reports
   SET scope_key = route
 WHERE scope_key = '';

ALTER TABLE incident_reports
    DROP CONSTRAINT IF EXISTS incident_reports_status_code_check;

ALTER TABLE incident_reports
    ADD CONSTRAINT incident_reports_status_code_check
    CHECK (status_code = 0 OR status_code BETWEEN 100 AND 599);

CREATE INDEX idx_incident_reports_type_occurred_at
    ON incident_reports (event_type, occurred_at DESC);
