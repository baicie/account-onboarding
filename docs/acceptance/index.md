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
| [R5.md](R5.md) | R5 | Release readiness: durable session revocation, Docker/Compose audit, Cloudflare live prep, identity/platform boundaries | **LOCAL_RC_READY / DEPLOYMENT_BLOCKED**; R5-B/R5-C/live **NOT_RUN** |
| [REVIEW-R5.md](REVIEW-R5.md) | R5 | Independent adversarial review of R5 (PASS, P0=0/P1=0; 8 P2 closed in `a488740`) | see report |
| [R5.1.md](R5.1.md) | R5.1 | Container & TLS reliability: local HTTPS ingress PASS, Docker runtime NOT_RUN, single-writer lock | **DEPLOYMENT_BLOCKED**; TLS **PASS**, Docker/容器级 **NOT_RUN** |
| [R5.1-B.md](R5.1-B.md) | R5.1-B | Docker runtime closure attempt: daemon unavailable, container/image **NOT_RUN**, readiness fixes (JSON keyring, readyz healthcheck) | **DEPLOYMENT_BLOCKED**; `CONTAINER_RC_READY` not claimed |
| [AUTONOMOUS-SPRINT-20261025.md](AUTONOMOUS-SPRINT-20261025.md) | R6 | Autonomous sprint progress: P0-B offline container audit, P1 hardening/tests, P2 Cloudflare dry-run, P3 deploy/supply-chain docs | 进行中/最终证据见文件 |
| [REVIEW-R6.md](REVIEW-R6.md) | R6 | 独立审查：PASS（范围限定），P0=0/P1=0，P2 已修复 | see report |
| [R6.1.md](R6.1.md) | R6.1 | Container runtime closure attempt: Docker daemon unavailable, all runtime gates **NOT_RUN** | **DEPLOYMENT_BLOCKED**; no image/ID/digest |
| [R7.md](R7.md) | R7 | Source-review closure + real container CI (Actions 40161727232, 63/63 PASS, image inspect verified) | **CONTAINER_RC_READY**; image ID only, no registry digest |
| [REVIEW-R7.md](REVIEW-R7.md) | R7 | Independent review: PASS, P0=0/P1=0, P2 fixed | see report |
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
