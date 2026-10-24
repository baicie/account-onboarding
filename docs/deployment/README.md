# Deployment runbooks — account-onboarding v0.1.0

Status of these documents: **design/runbook only**. No production deployment has
been performed in this repository; Docker runtime acceptance and real-platform
live tests remain `NOT_RUN` (see `docs/acceptance/FINAL-REPORT.md` and
`R5.1-B.md`).

| Document | Purpose |
|---|---|
| [`tls-ingress.md`](tls-ingress.md) | Production TLS termination (Caddy/nginx examples, ACME, HSTS, proxy boundary) |
| [`backup-restore.md`](backup-restore.md) | Encrypted snapshot backup/verify/restore runbook, RPO/RTO, key handling |
| [`rollback.md`](rollback.md) | Upgrade/rollback procedure, migration and session-revocation caveats |
| [`first-deploy-checklist.md`](first-deploy-checklist.md) | Pre-flight and go-live checklist (secrets, data dir, health, backup, monitoring) |
| [`security-gate.md`](security-gate.md) | Release security gate: tests, scans, container/TLS gates, evidence requirements |
| [`supply-chain.md`](supply-chain.md) | Image identity/tag/digest policy, SBOM/signing plan, reproducibility |
| [`../deploy/examples/Caddyfile`](../deploy/examples/Caddyfile) | Reference Caddy TLS ingress (not applied) |
| [`../deploy/examples/nginx.conf`](../deploy/examples/nginx.conf) | Reference nginx TLS ingress (not applied) |

Hard boundaries for any operator:

- Never expose the app container directly; only the TLS reverse proxy may be
  reachable from non-loopback networks.
- Keep `deploy/compose.yaml` publishing `127.0.0.1:8788`; do not change to
  `0.0.0.0` without the reviewed ingress design and the
  `ONBOARD_TRUST_PROXY_HEADERS=1` boundary.
- Never print or commit secret values; keep the keyring on separate media from
  snapshots.
- `scripts/restore.sh` / `rotate_key.sh` require the service to be stopped
  (`CONFIRM_*` flags); the single-writer lock will also refuse a second writer.
- Do not push images or publish releases unless explicitly authorized.
