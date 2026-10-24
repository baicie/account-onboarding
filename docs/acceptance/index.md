# Acceptance evidence index

Every report below records: environment, the exact commands that were run, exit
codes, PASS/PARTIAL/FAIL/NOT_RUN per acceptance item, and explicit statements
about what was **not** executed. Mocked/offline results are never presented as
live-platform success.

| Report | Phase | Scope | Verdict |
|---|---|---|---|
| [R0.md](R0.md) | R0 | Platform skeleton: config, SQLite/WAL, durable engine, leases/fencing, provider contracts, fake providers, API + console, build/CI scaffolding | **PASS** (Docker/CI execution NOT_RUN) |
| [R1.md](R1.md) | R1 | Mailpit + Cloudflare Temp Email mailbox adapters, mail matching/extraction, contract tests | **PASS** (scoped); live Cloudflare smoke **NOT_RUN** |
| [R2-infra.md](R2-infra.md) | R2 infra | Real local Keycloak 26.8.1 + Mailpit fixture without Docker; verified full email + OIDC PKCE flow | **PASS** (LOCAL SANDBOX) |
| [R2.md](R2.md) | R2 | 5-identity fully automated email verification + OIDC E2E, crash/restart, idempotency, cancel | **PASS (LOCAL SANDBOX)** 5/5; Playwright/Docker/real-platform **NOT_RUN** |
| [R3.md](R3.md) | R3 | GitHub existing-account official email API adapter; permission-gated WorkBuddy control-plane adapter | **PASS** contract/fake; live **NOT_RUN** |
| [R4.md](R4.md) | R4 | Backup/restore (encrypted option), key rotation, rate limiting, viewer RBAC, redaction scanning, HTTP hardening audit, deployment manifests | **PARTIAL** — executable controls PASS; TLS/Docker/GHCR/real-platform **NOT_RUN**; no P0 |
| [REVIEW.md](REVIEW.md) | — | Independent adversarial P0/P1 review run against the real tree | see report |
| [FINAL-REPORT.md](FINAL-REPORT.md) | — | Consolidated delivery report: phase verdicts, acceptance matrix, deviations, residual risks | see report |

Supporting artifacts:

- [`baseline-audit.md`](../baseline-audit.md) — read-only environment and
  existing-system audit plus the design-deviation table.
- [`docs/adr/`](../adr/) — accepted deviations (Go OIDC driver, cgo SQLite driver).
- [`evidence/`](evidence/) — raw transcripts (R0 smoke log; per-phase logs where
  the reports reference them).
- [`docs/security/R4-audit.md`](../security/R4-audit.md) — control-by-control
  security audit produced during R4.

## How to reproduce

```bash
# Offline gates (no external services)
make verify                 # gofmt + vet + go test ./... -race + build
make contract               # provider contract tests
make security               # API auth/CSRF/redaction tests
bash tests/r0/smoke.sh      # R0 end-to-end smoke with fake providers

# Real local sandbox (Keycloak + Mailpit, loopback only, no Docker)
bash tests/sandbox/up.sh
bash tests/sandbox/verify.sh
# then the R2 E2E, as documented in docs/acceptance/R2.md
```

## Live-platform status

| Platform | Status |
|---|---|
| Cloudflare Temp Email (`api.moonbyte.top`) | **NOT_RUN** — no admin/address credentials in this environment |
| GitHub official API (existing account) | **NOT_RUN** — requires an operator-authorized token and human confirmation |
| WorkBuddy gateway (real) | **NOT_RUN** and adapter disabled by default — requires verified written platform permission |
| Docker build / GHCR publish | **NOT_RUN** — Docker daemon unavailable |
| Keycloak + Mailpit (LOCAL SANDBOX) | executed locally; see R2 reports |
