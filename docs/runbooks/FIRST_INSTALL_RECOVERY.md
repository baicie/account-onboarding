# First Install & Recovery Runbook (R11)

## First install

1. Prepare data directory owned by UID 10001, writable; secrets volume with
   `onboarding_master_key` and `onboard_admin_token` mode 0400, owner 10001.
2. Start with `ONBOARD_FAKE_PROVIDERS=true` for a staging smoke; production must
   use the real provider only after the Cloudflare live authorization.
3. Expect schema 5, `/readyz` 200, `/v1/meta` schema 5 and the image-internal
   commit to match the deployed SHA. R11 CI proves this in an isolated runner.

## Failure modes (R11 drill evidence)

| Condition | Behavior |
|---|---|
| Empty data dir | migrate to schema 5, ready |
| SIGKILL / container recreate | SQLite volume persists; ready; run retained |
| Missing secret | container fails closed, never serves ready |
| DB directory read-only | startup/write probe fails closed |
| Corrupt DB file | startup/migration fails closed; do not delete blindly |
| Partial schema (1–3) | migrates to 5 or fails atomically; never serves a half-schema |

## Recovery

- Restore from an encrypted, **verified** backup into a fresh volume; do not
  restore over a live DB. Wrong key or existing valid target must not overwrite.
- After restore: verify schema/readyz, run visibility, session revocation replay,
  and `external_operations` state. IN_FLIGHT from a crashed process must become
  RECONCILIATION_REQUIRED via the lifecycle barrier; never infer success.
- Keep audit logs/evidence. If schema is newer than the old binary supports,
  rollback requires restoring the pre-upgrade schema-appropriate backup; old
  binaries must refuse newer schemas.
- Master key rotation: use the documented keyring envelope; never place keys in
  evidence or command history.
