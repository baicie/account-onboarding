# Upgrade / rollback runbook

## Upgrade

1. Read the release notes and `docs/acceptance/` evidence for the target version.
2. Take an **encrypted snapshot** and verify it; record the keyring version.
3. Stop the old instance (single-writer lock prevents two writers; migrations run
   at startup and are additive).
4. Build/pull the new image, record its Image ID/digest, start it.
5. Verify `/readyz` (200), a known run, session login/logout, and one sandbox run.
6. Keep the previous image and snapshot for the rollback window.

```bash
scripts/backup.sh --db /srv/account-onboarding/state.sqlite --out-dir /srv/backups --encrypt --keyring /secure/keyring.json
scripts/verify_backup.sh /srv/backups/state-*.sqlite.enc --keyring /secure/keyring.json
docker compose stop onboarding
docker compose up -d
curl -fsS http://127.0.0.1:8788/healthz
```

## Rollback

```bash
# 1. Stop the NEW instance.
docker compose stop onboarding
# 2. Restore the pre-upgrade snapshot if the new schema/data must be reverted.
CONFIRM_RESTORE=1 scripts/restore.sh --snapshot <pre-upgrade snapshot> \
  --target /srv/account-onboarding/state.sqlite --keyring /secure/keyring.json --force
# 3. Start the previous image/tag (recorded before upgrade).
# 4. Verify /readyz and a known run; record the rollback.
```

Important compatibility notes:

- Migrations are **additive** (0001→0003); rolling back the schema is only safe
  by restoring the pre-upgrade snapshot. Do not hand-edit `schema_migrations`.
- The durable session revocation table was added in R5. Rolling back to a
  pre-R5 binary loses durable logout semantics: rotate the master key (forcing
  all sessions to re-login) or accept that previously logged-out cookies may be
  valid again until their TTL.
- A binary that predates the single-writer lock does **not** take the lock. Never
  run old and new binaries against the same database concurrently; stop the old
  instance before starting the new one.
- The JSON keyring envelope is accepted since R5.1-B; older binaries accept only
  raw/hex/base64, so pin the keyring format to what the rolled-back binary
  supports.

## Restore drill

At least once per release, perform a restore into a scratch directory, start a
dry-run instance on it, and verify `/readyz`, a run, and a revoked session. This
is currently **NOT_RUN** in production form; the repository has process-level
evidence in `tests/r6/restore_revocation.sh`.
