# R24 Deployment Reliability Progress

- Actual start HEAD: `af892a9` (the named `9aada068` is an ancestor)
- Current verified PR head before this doc: `e1154ccd`
- PR: #23, branch `feat/v0.1-compose-deployment`, Draft
- Release: `NOT_PRODUCTION_READY`, `release_authorized=false`

## M1 — CI and real Docker

- ShellCheck PASS at the actual start HEAD; the previously reported `SC2034` items were already fixed by `af892a9`.
- Root cause of readiness failure: `/readyz` is authenticated, but the deployer could not read the materialized `0400` secret in the `0700:10001` secret directory, so `api_probe` sent no Authorization header and received `401`. It now prefers the deployer-readable `runtime/secrets-src/onboard_admin_token`, falls back to the materialized copy, and never puts the token in argv.
- `docker logs --no-color` removed for older Docker CLI compatibility.
- Real Docker E2E now passes first install, health/ready, second deploy with snapshot, rollback, wrong-SHA refusal, override protection, and database persistence.

## M2 — Data safety

- Empty database refusal moved before the no-container early return; symlinked data directory/database refused.
- Valid database without a container is still reusable.
- First install data directory is `0700:10001`; the app creates SQLite/WAL/SHM as UID 10001.
- Host verification of private data uses the host root/sudo boundary, never weakening runtime permissions.

## M3 — Backup/rollback

- Backup tools are built with `-buildvcs=false` inside the read-only source mount.
- Real E2E now proves encrypted snapshot creation, manifest verification, rollback to the previous image, and database survival.
- Concurrency lock and full WAL checkpoint fault matrix are **NOT_STARTED**.

## M4/M5/M6

- Stub scenario suite: 27 PASS, 0 FAIL; ShellCheck, Go tests, Compose, secret, and real Docker workflows PASS on `e1154ccd`.
- Security hardening and operational runbook are **PARTIAL/EXISTING**; no production host was touched.
- Review bundle generated from `af892a9..e1154ccd` and PR base.

## Commits

- `778b61c` readiness authentication
- `3423c5d`, `d316e3a`, `8de5e01`, `0920204`, `c545653` CI harness corrections
- `f941ecd` private runtime DB reads
- `113c1bc` ops tools VCS stamping
- `e1154ccd` empty DB/symlink safety
