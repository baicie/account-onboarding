-- 0006_alert_outbox.sql
-- Durable at-least-once alert delivery outbox. Stable alert IDs, delivery
-- attempt IDs, lease fencing, retry scheduling and dead-letter state all live
-- in SQLite so a restart does not forget pending/recovery notifications.
CREATE TABLE IF NOT EXISTS alert_outbox (
    id TEXT PRIMARY KEY,
    fingerprint TEXT NOT NULL,
    rule TEXT NOT NULL,
    severity TEXT NOT NULL,
    message TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'PENDING',
    attempts INTEGER NOT NULL DEFAULT 0,
    next_attempt_ms INTEGER NOT NULL,
    lease_owner TEXT NOT NULL DEFAULT '',
    lease_expires_ms INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT '',
    delivery_id TEXT NOT NULL DEFAULT '',
    created_at_ms INTEGER NOT NULL,
    updated_at_ms INTEGER NOT NULL,
    delivered_at_ms INTEGER
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_alert_outbox_active_fingerprint
    ON alert_outbox(fingerprint)
    WHERE status IN ('PENDING','IN_FLIGHT','RETRY_PENDING');
CREATE INDEX IF NOT EXISTS idx_alert_outbox_status_due
    ON alert_outbox(status, next_attempt_ms, id);
CREATE TABLE IF NOT EXISTS alert_deliveries (
    id TEXT PRIMARY KEY,
    alert_id TEXT NOT NULL,
    attempt INTEGER NOT NULL,
    status TEXT NOT NULL DEFAULT 'IN_FLIGHT',
    lease_owner TEXT NOT NULL,
    started_at_ms INTEGER NOT NULL,
    finished_at_ms INTEGER,
    http_status INTEGER,
    error TEXT NOT NULL DEFAULT '',
    FOREIGN KEY(alert_id) REFERENCES alert_outbox(id)
);
CREATE INDEX IF NOT EXISTS idx_alert_deliveries_alert
    ON alert_deliveries(alert_id, started_at_ms DESC);
INSERT OR IGNORE INTO schema_migrations(version,applied_at_ms)
VALUES(6, CAST(strftime('%s','now') AS INTEGER)*1000);
