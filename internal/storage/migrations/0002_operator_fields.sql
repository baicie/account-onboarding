-- account-onboarding schema v0.1.0 / migration 0002
-- Safe additive migration only. No DROP TABLE is ever used as an upgrade path.
-- Adds operator-facing derived fields, mail dedupe receipts and the audit log.

ALTER TABLE runs ADD COLUMN current_step TEXT;
ALTER TABLE runs ADD COLUMN resume_state TEXT;
ALTER TABLE runs ADD COLUMN last_error_class TEXT;
ALTER TABLE runs ADD COLUMN action_required INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runs ADD COLUMN request_fingerprint TEXT;

ALTER TABLE steps ADD COLUMN result_json TEXT NOT NULL DEFAULT '{}';
ALTER TABLE steps ADD COLUMN waiting_reason TEXT;
ALTER TABLE steps ADD COLUMN failure_count INTEGER NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS mail_receipts (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    provider TEXT NOT NULL,
    message_key_hmac TEXT NOT NULL,
    consumed_at_ms INTEGER NOT NULL,
    UNIQUE(run_id, provider, message_key_hmac)
);
CREATE INDEX IF NOT EXISTS idx_mail_receipts_run ON mail_receipts(run_id);

CREATE TABLE IF NOT EXISTS audit_log (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    actor TEXT NOT NULL,
    action TEXT NOT NULL,
    run_id TEXT,
    detail_json TEXT NOT NULL DEFAULT '{}',
    created_at_ms INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_audit_created ON audit_log(created_at_ms DESC);

INSERT OR IGNORE INTO schema_migrations(version,applied_at_ms)
VALUES(2, CAST(strftime('%s','now') AS INTEGER)*1000);
