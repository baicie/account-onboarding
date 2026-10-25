# account-onboarding

Self-hosted, pluggable, crash-recoverable **identity & mailbox onboarding engine**
for the Moonbyte self-hosted stack.

It orchestrates three things against services you already control:

1. **Mailboxes** — the existing Cloudflare Temp Email deployment (`api.moonbyte.top`)
   or a local Mailpit sink for tests.
2. **Blocking-test IdP** — a local Keycloak sandbox (`moonbyte-test` realm) with
   SMTP into Mailpit, used for fully automated 5-identity E2E.
3. **Existing identities** — an already-authorized GitHub account via the official
   `/user/emails` API, and (permission-gated, disabled by default) the existing
   `workbuddy2api-panel` control plane.

It is the durable orchestration layer: Go HTTP service + SQLite (WAL) workflow
engine with leases/fencing + typed provider contracts + minimal operator console.

## What this is not

- Not a new mail server. Cloudflare Temp Email and Mailpit stay untouched.
- Not a bulk third-party account factory. There is **no** account-creation path,
  CAPTCHA/anti-bot bypass, proxy rotation, fingerprint spoofing, or free-quota
  circumvention. Real-account templates only touch an account the operator
  already owns, and a human must complete vendor consent / email confirmation.
- Not a credential authority. OAuth tokens stay with the existing gateway
  (`workbuddy2api-panel`); this service only stores opaque `secret://` references
  and redacted status.
- The real WorkBuddy adapter ships **disabled** until verified written platform
  permission exists (`ONBOARD_WORKBUDDY_PERMISSION_ACK` + explicit enablement).

## Status

| Phase | Scope | Status |
|---|---|---|
| R0 | Platform skeleton: config, SQLite schema + WAL, durable engine, leases/fencing, provider contracts, fake providers, API + console, Dockerfile, Makefile, CI | **PASS** (see [docs/acceptance/R0.md](docs/acceptance/R0.md)) |
| R1 | Mailbox providers: Mailpit sink, Cloudflare Temp Email (`x-admin-auth`, address JWT), mail extraction + dedupe, contract tests | **PASS** scoped; live Cloudflare smoke **NOT_RUN** (no credentials) — [docs/acceptance/R1.md](docs/acceptance/R1.md) |
| R2 | Local Keycloak + Mailpit sandbox, 5-identity fully automated email verification + OIDC PKCE, crash/restart, idempotency, cancel | **PASS (LOCAL SANDBOX)** — 5/5 through `/v1/runs`, crash/restart recovered, no duplicate identity; Playwright/Docker/real-platform **NOT_RUN** — [docs/acceptance/R2.md](docs/acceptance/R2.md) |
| R3 | GitHub existing-account email management + permission-gated WorkBuddy adapter with fake contracts | **PASS** contract/fake; live GitHub and WorkBuddy **NOT_RUN** — [docs/acceptance/R3.md](docs/acceptance/R3.md) |
| R4 | Backup/restore (incl. encrypted snapshots), key rotation, rate limiting, viewer RBAC, CSRF/CSP/SSRF audit, redaction scanning, deployment hardening | **PARTIAL** — all executable controls PASS; TLS ingress, Docker build/run, GHCR publish and real-platform live tests **NOT_RUN**; no P0 — [docs/acceptance/R4.md](docs/acceptance/R4.md), [audit](docs/security/R4-audit.md) |
| R5 | Release readiness: durable SQLite session revocation, container audit, Cloudflare live prep, identity/platform boundaries | **LOCAL_RC_READY / DEPLOYMENT_BLOCKED** — R5-A and boundary gates PASS; Docker, TLS, live platforms **NOT_RUN** — [docs/acceptance/R5.md](docs/acceptance/R5.md) |
| R5.1 | Container & TLS reliability: isolated local HTTPS ingress acceptance, single-writer instance lock, Docker readiness | **DEPLOYMENT_BLOCKED** — TLS ingress **PASS (26/26)**; Docker daemon unavailable so container build/run **NOT_RUN**; container-level fault injection **NOT_RUN** — [docs/acceptance/R5.1.md](docs/acceptance/R5.1.md) |
| R5.1-B | Docker runtime closure: container build/run/health/persistence/migration/fault/backup gates | **DEPLOYMENT_BLOCKED** — Docker daemon unavailable (`docker info` exit 1), container runtime and image **NOT_RUN**; readiness fixes (JSON keyring, `/readyz`-based HEALTHCHECK) tested locally; no image digest — [docs/acceptance/R5.1-B.md](docs/acceptance/R5.1-B.md) |
| R6 | Autonomous sprint: offline container audit, DB file hardening, fault/concurrency tests, Cloudflare dry-run, deploy/rollback/supply-chain docs | **DEPLOYMENT_BLOCKED** — Docker daemon still unavailable (container runtime **NOT_RUN**); P1/P2/P3 offline evidence added; no LIVE_READY — [docs/acceptance/AUTONOMOUS-SPRINT-20261025.md](docs/acceptance/AUTONOMOUS-SPRINT-20261025.md) |
| R6.1 | Container runtime closure attempt: real Docker build/run/security/recovery gates | **DEPLOYMENT_BLOCKED** — Docker daemon/VM not running (`docker info` exit 1, socket `_ping` 000); all container gates **NOT_RUN**, no image/ID/digest; no code changes — [docs/acceptance/R6.1.md](docs/acceptance/R6.1.md) |
| R7 | Source-review closure + real container CI: idempotency contract, deadline/cancel race, GitHub Actions Docker acceptance | **CONTAINER_RC_READY** — final Actions run 40161727232, 63 PASS/0 FAIL, image `sha256:352b85ec…9763`; independent review PASS (P0=0/P1=0); no production TLS/GHCR/live platforms — [docs/acceptance/R7.md](docs/acceptance/R7.md) |
| R10 | Live safety: worker-lifecycle/fenced external gate, schema 5, credential audit, isolated OCI dry run, Caddy/rollback, Cloudflare preflight | **LIVE_PREFLIGHT_READY (pending review)** — final CI: staging 27/27, container 63/63, rollback 20/20, Caddy 18/18, OCI dry-run 15/15, candidate PASS (publish skipped); no live platforms/GHCR — [docs/acceptance/R10.md](docs/acceptance/R10.md) |
| R9 | External-write gate, reconciliation pagination/resolve, old-image rollback, real Caddy TLS, release candidate pipeline | **RELEASE_CANDIDATE_READY** — final CI: staging 27/27, container 63/63, rollback 20/20, Caddy 17/17, candidate PASS with real scans (publish skipped); no live platforms/GHCR — [docs/acceptance/R9.md](docs/acceptance/R9.md) |
| R8 | Staging release readiness: isolated TLS staging, R8 container regression, supply-chain SBOM/vuln gates, external-op reconciliation | **STAGING_RC_READY** — final CI: staging 27/27, container 63/63, supply-chain PASS (0 vulns, SBOM); no GHCR/publish/live platforms — [docs/acceptance/R8.md](docs/acceptance/R8.md) |

Legend: **PASS** means the command was actually run with real output. **NOT_RUN**
means the step needs credentials or infrastructure that is not available in this
environment; mocked results are never reported as live success.

## Quickstart (offline, fake providers, no external calls)

```bash
# 1) Scratch state + a local-only admin token (never commit these)
mkdir -p .sandbox/run
export ONBOARD_LISTEN=127.0.0.1:8788
export ONBOARD_DB_PATH="$PWD/.sandbox/run/state.sqlite"
export ONBOARD_FAKE_PROVIDERS=true
export ONBOARD_DEV_EPHEMERAL_KEY=1          # local fake-provider demo only
export ONBOARD_ADMIN_TOKEN="$(openssl rand -hex 24)"
export ONBOARD_DOMAINS=moonbyte.test
export ONBOARD_POLL_INTERVAL=1s

# 2) Build and run
make build
./bin/onboardd

# 3) Create a sandbox run (all six steps complete offline)
curl -s -X POST http://127.0.0.1:8788/v1/runs \
  -H "Authorization: Bearer $ONBOARD_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: demo-run-0001" \
  -d '{"template":"sandbox-email-verify-oidc","mode":"sandbox-e2e",
       "mailbox_provider":"fake-mailbox","identity_provider":"fake-identity",
       "oauth_provider":"fake-oauth","sandbox_provider":"fake-sandbox"}'

# 4) Follow the run and its redacted event timeline
curl -s -H "Authorization: Bearer $ONBOARD_ADMIN_TOKEN" http://127.0.0.1:8788/v1/runs
```

Open <http://127.0.0.1:8788/ui/> and sign in with the same admin token for the
minimal console (runs, steps, event timeline, resume/cancel, provider gates).

## Live integration gates

All real providers are **off by default** and require an explicit, signed-off
configuration:

| Provider | Enable | Additional requirements |
|---|---|---|
| Cloudflare Temp Email | `ONBOARD_CLOUDFLARE_ENABLED=true` | `ONBOARD_CLOUDFLARE_BASE_URL=https://api.moonbyte.top`, admin password secret file; optional site password; base URL is allowlisted in the adapter |
| Mailpit (test only) | `ONBOARD_MAILPIT_ENABLED=true` | Loopback base URL only (`http://127.0.0.1:8025`) |
| Keycloak sandbox (test only) | `ONBOARD_KEYCLOAK_ENABLED=true` | Loopback base URL, test realm/admin secret file; wired in R2 |
| GitHub existing account | `ONBOARD_GITHUB_ENABLED=true` | PAT/App token secret file with email read/write; **the account holder** confirms the verification email |
| WorkBuddy existing gateway | `ONBOARD_WORKBUDDY_ENABLED=true` **and** `ONBOARD_WORKBUDDY_PERMISSION_ACK=<review ref>` | Private/loopback gateway URL, gateway token secret file; adapter stays disabled without the acknowledgement |

No live test runs in CI. `go test ./...` never contacts GitHub, Cloudflare,
WorkBuddy or Keycloak.

## Architecture

```
Console UI ── HTTP API (bearer / HttpOnly session + CSRF)
                  │
             Workflow engine (durable steps, leases, fencing)
                  │                 │                │
              SQLite WAL       Provider registry   Encrypted secret store
            (runs/steps/        (enabled gates)     (AES-256-GCM, refs only)
             events/resources)       │
                                     ├── Mailbox:  Mailpit | Cloudflare Temp Email
                                     ├── Identity: Keycloak sandbox (R2)
                                     ├── OAuth:    sandbox OIDC driver (R2) | WorkBuddy (gated)
                                     ├── Email:    GitHub official /user/emails
                                     └── Credential verifier
```

Design invariants (implemented):

- SQLite `WAL`, `foreign_keys=ON`, `busy_timeout=5000`, `synchronous=FULL`,
  `BEGIN IMMEDIATE` on the single write connection.
- A step is claimed in one transaction (lease owner + deadline + fencing
  generation), executed **outside** any transaction, then committed in a new
  transaction that rejects stale workers (`ErrLeaseLost`).
- Step/workflow transitions are validated against an explicit state machine;
  terminal states are final; cancellation wins over an in-flight result.
- Idempotency is enforced by a unique `(owner, template, idempotency_key)` plus a
  request fingerprint; reusing a key with a different body returns `409`.
- Secrets (mailbox JWT, sandbox credentials, OAuth URLs) are encrypted with
  AES-256-GCM, versioned keys and AAD `kind|id|version`; only opaque
  `secret://<kind>/<id>` references are stored in run resources.
- Events pass a strict payload allowlist and a redaction filter; mailbox bodies,
  links, OTPs, OAuth state, tokens and passwords are never persisted or logged.
- URL allowlists and redirect guards in every live adapter; credentials are never
  forwarded on cross-host redirects.
- Durable mail dedupe (`mail_receipts`) so one message is consumed once per run.
- Per-client-IP rate limiting (general control plane + a stricter console-login
  bucket, `Retry-After` on 429) and a read-only viewer role
  (`ONBOARD_READONLY_TOKEN`): GET endpoints are allowed, every mutation is 403.
- Cookie-authenticated mutations require a same-origin `Origin` or `Referer`
  (beyond the CSRF token); session cookies are `Secure` under TLS or behind a
  trusted TLS proxy (`ONBOARD_TRUST_PROXY_HEADERS=1`).

See [docs/baseline-audit.md](docs/baseline-audit.md) for the environment probe and
the deviations from the design pack.

## API

The authoritative contract is [contracts/openapi.yaml](contracts/openapi.yaml).
Implemented endpoints:

| Method | Path | Notes |
|---|---|---|
| GET | `/healthz` | unauthenticated liveness |
| GET | `/readyz` | private readiness: DB writable, WAL, schema version, keyring |
| GET | `/v1/meta` | version, mode, templates |
| GET | `/v1/providers` | provider catalog + activation gates |
| POST | `/v1/runs` | idempotent create (`Idempotency-Key` required) |
| GET | `/v1/runs` | cursor pagination, status/template filters |
| GET | `/v1/runs/{id}` | run + steps + action flags |
| GET | `/v1/runs/{id}/events` | redacted append-only events |
| POST | `/v1/runs/{id}/resume` | only from wait states |
| POST | `/v1/runs/{id}/cancel` | terminal runs return 409 |
| GET | `/v1/audit` | operator audit log (redacted) |

Authentication: `Authorization: Bearer <admin token>` (full access) or the
optional read-only token (GET only). The console uses an HttpOnly +
SameSite=Strict session cookie with CSRF. Rate limits:
`ONBOARD_RATE_LIMIT_PER_MINUTE` (default 300) and
`ONBOARD_LOGIN_RATE_LIMIT_PER_MINUTE` (default 10); 0 disables a bucket.

## Testing and evidence

```bash
make verify           # gofmt + go vet + go test ./... -race -count=1 + build
make contract         # mailbox/identity provider contract tests (offline)
make security         # API auth/CSRF/redaction tests
make compose-config   # validates deploy/compose.yaml without a Docker daemon
```

Evidence reports live under [`docs/acceptance/`](docs/acceptance/): environment,
exact commands, exit codes, PASS/PARTIAL/FAIL/NOT_RUN per acceptance item, and
explicit statements about what was *not* executed (live platforms, Docker build,
real email delivery). Start with [`docs/acceptance/index.md`](docs/acceptance/index.md)
and the consolidated [`docs/acceptance/FINAL-REPORT.md`](docs/acceptance/FINAL-REPORT.md).
An independent adversarial review is recorded in
[`docs/acceptance/REVIEW.md`](docs/acceptance/REVIEW.md).

## Repository layout

```
cmd/onboardd/                 service entrypoint + healthcheck/migrate subcommands
internal/api/                 HTTP API, auth/CSRF/Origin, minimal console UI
internal/workflow/            durable engine, templates, steps, state machine
internal/storage/             SQLite schema, migrations, repositories, leases
internal/security/            AES-256-GCM keyring, sessions, redaction
internal/secrets/             opaque secret reference service
internal/provider/            contracts, registry, fake + live adapters
internal/mailx/               message matching + link/OTP extraction (transient)
internal/logging/             structured JSON logging with redaction
contracts/openapi.yaml        control-plane contract
db/migrations/                design schema reference (embedded copy in storage)
tests/sandbox/                local Keycloak + Mailpit fixture (R2)
deploy/                       compose manifest, env example, secrets placeholders
docs/acceptance/              per-phase evidence reports
```

## Compliance

- Only official APIs for accounts the operator already holds; no account
  creation, no CAPTCHA/anti-bot bypass, no quota circumvention, no credential
  sharing/pooling.
- Sandbox automation targets only a self-controlled Keycloak realm and Mailpit;
  it is never pointed at a real third-party platform.
- WorkBuddy real calls require verified written permission and stay disabled
  otherwise; the Fake adapter is used for contracts and CI.
- No secrets in the repository. `.env` examples contain no values.

## License / ownership

Private self-hosted engineering project. No license is granted for third-party
code referenced by the design pack; no third-party source was copied.
