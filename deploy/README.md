# deploy/ — local compose deployment

This directory is the deployment contract for the self-hosted service. It builds
the image from the repository root `Dockerfile`; nothing here pulls a published
image by default.

## Before first start

1. Create the secret files (never commit them; `deploy/.gitignore` excludes them):

   ```bash
   mkdir -p deploy/secrets deploy/data
   chmod 700 deploy/secrets deploy/data
   # master key: JSON keyring (preferred; supports rotation) or 32 raw/hex/base64 bytes
   go run ./internal/ops/cmd/keyring init -out deploy/secrets/onboarding_master_key
   # operator token (random, >= 16 chars)
   head -c 32 /dev/urandom | base64 > deploy/secrets/onboard_admin_token
   # optional read-only viewer token (must differ from the admin token)
   # head -c 32 /dev/urandom | base64 > deploy/secrets/onboard_readonly_token
   chmod 600 deploy/secrets/*
   # the container runs as 10001:10001 and needs to write SQLite/WAL here
   sudo chown 10001:10001 deploy/data
   ```

2. Validate the deployment inputs (no Docker daemon required, no secrets printed):

   ```bash
   deploy/scripts/preflight.sh
   ```

   It checks: compose file readable; secret files exist, are non-empty, owner-only
   (0400/0600) and are not the shipped `*.example` placeholders; master key is
   32 raw bytes / 64 hex / base64 / JSON keyring with a valid current version;
   admin token non-empty (warns when < 16 chars); optional viewer token differs
   from the admin token; data dir exists and is writable (warns about UID 10001).
   Exit codes: 0 ready, 1 failures, 2 usage.

3. Statically validate compose, then start only when a Docker daemon and an
   explicit deployment decision exist:

   ```bash
   docker compose -f deploy/compose.yaml config
   # docker compose -f deploy/compose.yaml up -d   # NOT_RUN in this workspace
   ```

## Backups

Backups are operator tooling, not a container service. Encrypted at rest:

```bash
ONBOARD_BACKUP_ENCRYPT=1 ONBOARD_KEYRING=deploy/secrets/onboarding_master_key \
  scripts/backup.sh --db deploy/data/state.sqlite --out-dir ./backups --encrypt
scripts/verify_backup.sh ./backups/state-<UTC>.sqlite.enc --keyring deploy/secrets/onboarding_master_key
```

Keep the keyring on separate media from the snapshots; see
`docs/acceptance/R4.md` and `docs/security/R4-audit.md`.
