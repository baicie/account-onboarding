-- 0003_session_revocations.sql — durable session revocation.
--
-- Explicit logout must revoke a signed session cookie for the remainder of its
-- TTL, independent of process restarts. Rows are pruned opportunistically after
-- their expiry (see internal/storage/sessions.go), so the table stays bounded.
CREATE TABLE IF NOT EXISTS session_revocations (
    session_id   TEXT PRIMARY KEY,
    revoked_at_ms INTEGER NOT NULL,
    expires_at_ms INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_session_revocations_expires
    ON session_revocations (expires_at_ms);

INSERT OR IGNORE INTO schema_migrations(version,applied_at_ms)
VALUES(3, CAST(strftime('%s','now') AS INTEGER)*1000);
