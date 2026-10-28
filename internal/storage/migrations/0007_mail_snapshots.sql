-- Mail snapshots record what a run actually received, for the operator console.
-- The body is never stored: only the sender, subject, the verification link and the
-- provider-reported receive time, and only when ONBOARD_MAIL_SNAPSHOT is enabled.
CREATE TABLE IF NOT EXISTS mail_snapshots (
  id            TEXT PRIMARY KEY,
  run_id        TEXT NOT NULL,
  step          TEXT NOT NULL,
  mailbox_alias TEXT NOT NULL,
  sender        TEXT NOT NULL,
  subject       TEXT NOT NULL,
  link          TEXT NOT NULL,
  received_ms   INTEGER NOT NULL,
  created_ms    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_mail_snapshots_run ON mail_snapshots (run_id, created_ms);
