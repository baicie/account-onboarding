-- account-onboarding schema v0.1.0 / migration 0001
-- SQLite: execute PRAGMA foreign_keys = ON on every new connection.
CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY,
    applied_at_ms INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS runs (
    id TEXT PRIMARY KEY,
    owner_id TEXT NOT NULL,
    template TEXT NOT NULL,
    mode TEXT NOT NULL CHECK(mode IN ('sandbox-e2e','cloudflare-integration','real-account')),
    status TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    provider_snapshot_json TEXT NOT NULL DEFAULT '{}',
    existing_identity_ref TEXT,
    deadline_ms INTEGER NOT NULL,
    next_run_at_ms INTEGER,
    lease_owner TEXT,
    lease_until_ms INTEGER,
    fencing_generation INTEGER NOT NULL DEFAULT 0,
    version INTEGER NOT NULL DEFAULT 1,
    created_at_ms INTEGER NOT NULL,
    updated_at_ms INTEGER NOT NULL,
    finished_at_ms INTEGER,
    UNIQUE(owner_id, template, idempotency_key)
);
CREATE INDEX IF NOT EXISTS idx_runs_schedule ON runs(status,next_run_at_ms,lease_until_ms);
CREATE INDEX IF NOT EXISTS idx_runs_owner_created ON runs(owner_id,created_at_ms DESC);

CREATE TABLE IF NOT EXISTS steps (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    step_name TEXT NOT NULL,
    status TEXT NOT NULL,
    operation_key TEXT NOT NULL,
    attempt_count INTEGER NOT NULL DEFAULT 0,
    error_class TEXT,
    error_summary TEXT,
    next_retry_at_ms INTEGER,
    started_at_ms INTEGER,
    finished_at_ms INTEGER,
    updated_at_ms INTEGER NOT NULL,
    UNIQUE(run_id,step_name),
    UNIQUE(operation_key)
);
CREATE INDEX IF NOT EXISTS idx_steps_run ON steps(run_id);

CREATE TABLE IF NOT EXISTS events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    event_seq INTEGER NOT NULL,
    event_type TEXT NOT NULL,
    step_name TEXT,
    public_payload_json TEXT NOT NULL DEFAULT '{}',
    created_at_ms INTEGER NOT NULL,
    UNIQUE(run_id,event_seq)
);
CREATE INDEX IF NOT EXISTS idx_events_run_seq ON events(run_id,event_seq);

CREATE TABLE IF NOT EXISTS resources (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    resource_kind TEXT NOT NULL CHECK(resource_kind IN ('mailbox','identity','oauth_session','credential_ref')),
    external_id_hmac TEXT NOT NULL,
    display_alias TEXT,
    secret_ref TEXT,
    extra_json TEXT NOT NULL DEFAULT '{}',
    created_at_ms INTEGER NOT NULL,
    UNIQUE(run_id,resource_kind,external_id_hmac)
);

CREATE TABLE IF NOT EXISTS secrets (
    id TEXT PRIMARY KEY,
    secret_kind TEXT NOT NULL,
    ciphertext BLOB NOT NULL,
    nonce BLOB NOT NULL,
    key_version INTEGER NOT NULL,
    created_at_ms INTEGER NOT NULL,
    updated_at_ms INTEGER NOT NULL
);

INSERT OR IGNORE INTO schema_migrations(version,applied_at_ms)
VALUES(1, CAST(strftime('%s','now') AS INTEGER)*1000);
-- ACID note: migration runner must wrap this script in one explicit transaction.
