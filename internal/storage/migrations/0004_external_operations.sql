-- 0004_external_operations.sql — durable write-ahead gate for remote side effects.
--
-- The event log is audit history, not a mutual-exclusion primitive. This table
-- is the authoritative operation permit/state machine: a run/step can acquire
-- an IN_FLIGHT permit only once; unknown/ambiguous outcomes must be resolved by
-- an operator before any automatic retry is allowed.
CREATE TABLE IF NOT EXISTS external_operations (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    step_name TEXT NOT NULL,
    operation_key TEXT NOT NULL UNIQUE,
    provider TEXT NOT NULL,
    operation_type TEXT NOT NULL,
    request_fingerprint TEXT NOT NULL,
    status TEXT NOT NULL CHECK(status IN (
        'PREPARED','IN_FLIGHT','CONFIRMED','OUTCOME_UNKNOWN',
        'RECONCILIATION_REQUIRED','FAILED_DEFINITE','RESOLVED')),
    attempt_count INTEGER NOT NULL DEFAULT 0,
    version INTEGER NOT NULL DEFAULT 1,
    last_error_class TEXT,
    resolution TEXT,
    resolution_reason TEXT,
    evidence_reference TEXT,
    prepared_at_ms INTEGER NOT NULL,
    started_at_ms INTEGER,
    resolved_at_ms INTEGER,
    created_at_ms INTEGER NOT NULL,
    updated_at_ms INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_external_ops_status_created
    ON external_operations(status, created_at_ms DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_external_ops_run
    ON external_operations(run_id, created_at_ms DESC);
CREATE INDEX IF NOT EXISTS idx_external_ops_provider
    ON external_operations(provider, created_at_ms DESC);

INSERT OR IGNORE INTO schema_migrations(version,applied_at_ms)
VALUES(4, CAST(strftime('%s','now') AS INTEGER)*1000);
