-- 0005_external_operation_lifecycle.sql
-- Immutable migration policy: 0004 was used during the R9 release-candidate
-- cycle and is not edited here. New lifecycle/fencing/audit columns and the
-- deterministic sort index are introduced only in this migration; SQLite's
-- IF NOT EXISTS makes it safe for databases that already received the index
-- from an R9 development build.
ALTER TABLE external_operations ADD COLUMN worker_id TEXT NOT NULL DEFAULT '';
ALTER TABLE external_operations ADD COLUMN lease_generation INTEGER NOT NULL DEFAULT 0;
ALTER TABLE external_operations ADD COLUMN resolved_by TEXT NOT NULL DEFAULT '';
ALTER TABLE external_operations ADD COLUMN resolved_from_status TEXT NOT NULL DEFAULT '';
ALTER TABLE external_operations ADD COLUMN resolution_version INTEGER NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS idx_external_ops_created
    ON external_operations(created_at_ms DESC, id DESC);
INSERT OR IGNORE INTO schema_migrations(version,applied_at_ms)
VALUES(5, CAST(strftime('%s','now') AS INTEGER)*1000);
