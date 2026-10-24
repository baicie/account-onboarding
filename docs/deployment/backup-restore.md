# Backup / restore runbook

The only durable state is the SQLite database (`state.sqlite` + WAL/SHM) plus the
master keyring. Raw email bodies are transient and are not stored.

## What to back up

| Artifact | Location | Notes |
|---|---|---|
| SQLite snapshot | `scripts/backup.sh --db <db> --out-dir <dir> [--encrypt --keyring <keyring>]` | `VACUUM INTO` is WAL-safe while the service runs; snapshot + manifest, 0600 |
| Master keyring | operator-managed path (e.g. `deploy/secrets/onboarding_master_key`) | **Separate media** from snapshots; never in the same directory or backup bucket |
| Compose/secrets | `deploy/compose.yaml` + secret files | Secret files are not in Git; keep a recovery procedure, not a copy next to snapshots |

RPO/RTO targets are operator-defined; for a single-host service a nightly
encrypted snapshot plus a pre-upgrade snapshot is the minimum.

## Backup

```bash
# Preferred: encrypted snapshot + verification (keyring current key version)
ONBOARD_BACKUP_ENCRYPT=1 ONBOARD_KEYRING=/secure/keyring.json \
  scripts/backup.sh --db /srv/account-onboarding/state.sqlite \
  --out-dir /srv/backups --encrypt --keyring /secure/keyring.json
scripts/verify_backup.sh /srv/backups/state-<UTC>.sqlite.enc --keyring /secure/keyring.json
```

- Snapshots are `0600`; manifests record sha256, schema version, counts,
  encryption/key_version/plaintext_sha256.
- Keep keyring versions needed to decrypt retained snapshots; rotate the keyring
  only with a verified restore plan.

## Restore (requires downtime)

The restore tool refuses to overwrite a live database without `--force`, and the
single-writer flock will refuse a second writer. Always stop the service first.

```bash
# 1. Stop the service (compose: docker compose stop onboarding)
# 2. Dry verification (no target write)
scripts/verify_backup.sh /srv/backups/state-<UTC>.sqlite.enc --keyring /secure/keyring.json
# 3. Restore to a fresh path (or --force to replace after evidence)
CONFIRM_RESTORE=1 scripts/restore.sh \
  --snapshot /srv/backups/state-<UTC>.sqlite.enc \
  --target /srv/account-onboarding/state.sqlite \
  --keyring /secure/keyring.json --force
# 4. Start the service and verify
docker compose up -d
curl -fsS http://127.0.0.1:8788/healthz
curl -fsS -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8788/readyz
```

Restore verifies: integrity_check, foreign keys, schema version, row counts,
per-secret decryption with the supplied keyring, and manifest checksum. A wrong
keyring or tampered snapshot fails closed and leaves the target untouched.

## Post-restore checks

1. `/readyz` returns 200 (schema 3 + `session_revocations` table + master key).
2. List a known run; its status/events match the snapshot.
3. A session revoked before the snapshot is still rejected after restore (the
   `session_revocations` table is part of the snapshot; verified process-level in
   `tests/r6/restore_revocation.sh`).
4. New runs complete (sandbox profile) and mail receipt rows behave.
5. Record restore time/operator/snapshot name in the audit log.

## Failure handling

| Symptom | Action |
|---|---|
| `encrypted snapshot requires a keyring` | Pass the keyring that was current when the snapshot was taken |
| `secret decryption failed` | Wrong keyring/version; do not force; restore from another snapshot |
| `integrity_check`/FK failures | Snapshot corrupt; use previous snapshot; investigate disk |
| target untouched after failure | expected fail-closed behavior; verify target hash before retry |
