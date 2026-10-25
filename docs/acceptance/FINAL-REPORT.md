# Final delivery report — `account-onboarding` v0.1.0

- **Date**: 2026-10-24 (Asia/Shanghai)
- **Repository**: `/Users/liuzhiwei/Desktop/workspace/git-code/account-onboarding`
- **Git state**: local commits through R6: `e557387` (v0.1.0), `3719026` (R5-A
  durable session revocation), R5.1/R5.1-B commits, R6 commits `43f7f1a`,
  `55e6555`, `7bd33a6`, `ca834ee`, `38b17df`, `661e960` and the final evidence/docs commits
  through R6.1 (`20d0cb0`, `d8438f6`, `e812d41`). At the user's explicit request at the
  end of R6.1, local `main` was pushed to `origin/main` (new branch; source/docs only).
  No tag, release, image, GHCR push, merge or deployment was made; production/live
  platforms remain untouched.
- **Design pack**: `account-onboarding-design-v0.1.0/` (v0.1.0, read-only input)
- **Verdict**: **`STAGING_RC_READY`** (R8). Real Docker build/runtime/health,
  SQLite persistence+migration, SIGTERM/SIGKILL, single-writer, secret
  permissions, encrypted backup/restore and compose parity are PASS in GitHub
  Actions run 40161727232 (63/63 checks); independent R7 review is PASS with
  P0=0/P1=0. This is **not `LIVE_READY`**: production TLS ingress, GHCR publish
  and Cloudflare/GitHub/WorkBuddy live acceptance remain NOT_RUN; production
  deployment still requires those gates. Remaining gaps are infrastructure or
  live-platform items explicitly marked **NOT_RUN**, not hidden PARTIALs.

## 1. Executive summary

This delivery is a self-hosted Go service (`onboardd`) that orchestrates
mailboxes, a controlled identity provider, existing-account email verification
and permission-gated OAuth, backed by a durable SQLite WAL workflow engine. It
reuses the existing Cloudflare Temp Email API and the existing
`workbuddy2api-panel` control plane; it does **not** re-implement a mail server,
create third-party accounts, automate consent, or bypass platform controls.

| Gate | Verdict | Summary |
|---|---|---|
| **R0** platform skeleton | **PASS** | config/storage/engine/contracts/fake providers/API/UI/build; idempotency, lease fencing, transition matrix, redaction, restart durability all tested |
| **R1** mailbox providers | **PASS (scoped)** | Mailpit + Cloudflare adapters, mail matching/extraction, contract tests; live Cloudflare smoke **NOT_RUN** (no credentials) |
| **R2** sandbox E2E | **PASS (LOCAL SANDBOX)** | **5/5** identities through `/v1/runs` on real local Keycloak 26.8.1 + Mailpit; SIGKILL restart recovery, idempotency, cancel; Playwright/Docker/real-platform **NOT_RUN** |
| **R3** real existing identities | **PASS (contract/fake)** | GitHub official `/user/emails` adapter; permission-gated WorkBuddy adapter default-disabled with 410 → `REAUTHORIZE_REQUIRED`; live **NOT_RUN** |
| **R4** reliability/security closure | **PARTIAL** | backup/restore (incl. encrypted snapshots), key rotation, rate limiting, viewer RBAC, CSRF/CSP/SSRF audit, redaction scan, load evidence **PASS**; TLS ingress, Docker build/run, GHCR and real live tests **NOT_RUN**; no P0 |
| **R5** release readiness | **LOCAL_RC_READY** (2026-10-24) | R5-A durable SQLite session revocation (restart/并发/过期/迁移) **PASS**; R5-B Docker **NOT_RUN** (no daemon); R5-C Cloudflare live **NOT_RUN** (no authorization/credentials, read-only audit PASS); R5-D identity/platform boundaries **PASS**, live **NOT_RUN**; see [`R5.md`](R5.md) |
| **R5.1-B** Docker runtime closure | **NOT_RUN** (2026-10-25) | Docker daemon still unavailable (`docker info`/`docker build`/`docker images`/`docker ps` exit 1): no image, no digest, no container ever started; readiness fixes found by the audit — documented JSON keyring now actually accepted, Docker `HEALTHCHECK` switched to authenticated `/readyz` (DB/schema/revocation table/master key) — tested locally 6/6; hardened isolated `tests/r51/docker_acceptance.sh` (migration v2→v3, SIGTERM/SIGKILL, same-volume single-writer, backup/restore, degraded DB, fail-closed configs) remains **NOT_RUN** — [`R5.1-B.md`](R5.1-B.md) |
| **R5.1** container & TLS reliability | **DEPLOYMENT_BLOCKED** (2026-10-24) | isolated local HTTPS ingress **PASS 26/26** (test CA, hostname/TLS floor, HTTP→HTTPS, Secure/HttpOnly/SameSite, CSRF/Origin, SIGKILL+restart revocation, forged-header defense, loopback-only); single-writer instance lock added+tested; Docker build/run and container-level fault injection **NOT_RUN** (daemon unavailable) — [`R5.1.md`](R5.1.md) |
| **R6** autonomous sprint | **PARTIAL / DEPLOYMENT_BLOCKED** (2026-10-25) | P0-B offline container audit complete (Docker runtime **NOT_RUN**); P1 fixed SQLite/WAL/SHM 0644→0600 and added fault/concurrency/goroutine/restore-revocation tests; P2 Cloudflare dry-run + refusal gate; P3 reproducible-build provenance + deployment/TLS/backup/rollback/checklist docs; P4 review pending — see [`AUTONOMOUS-SPRINT-20261025.md`](AUTONOMOUS-SPRINT-20261025.md) |
| **R9** external consistency + release hardening | **RELEASE_CANDIDATE_READY** (2026-10-25) | Durable migration-0004 external-write gate (PREPARED/IN_FLIGHT/CONFIRMED/OUTCOME_UNKNOWN/RECONCILIATION_REQUIRED/FAILED_DEFINITE/RESOLVED) with lease-fenced CAS, sticky no-retry after unknown, paginated global reconciliation counts, audited admin resolve API/CLI, old-image schema-3→4 rollback **20/20**, real Caddy TLS proxy **17/17**, candidate release pipeline (publish skipped). Final CI 40176187395/40176187340/40176187307/40176187349; no GHCR push/live platform — [`R9.md`](R9.md) |
| **R8** staging release readiness | **STAGING_RC_READY** (2026-10-25) | Real CI: isolated test-CA HTTPS staging **27/27**, R8 container regression **63/63**, supply chain **PASS** (govulncheck 0, Trivy CRITICAL/HIGH 0, ShellCheck errors 0, CycloneDX+SPDX SBOM); external-operation reconciliation audit/API/ADR; future-schema rollback guard. No GHCR push/digest, no production TLS/live platforms — [`R8.md`](R8.md) |
| **R7** source-review closure + container CI | **CONTAINER_RC_READY** (2026-10-25) | All container gates executed on GitHub Actions run 40161727232 (Docker Engine 28.0.4, linux/amd64): image build + injected commit, Image ID `sha256:352b85ec…9763`, non-root/read-only/cap_drop, health/readiness, schema 3, v2 upgrade, WAL/SHM 0600, secret 0400 + UID 10002 denial, idempotency contract, revocation, SIGTERM/SIGKILL, single-writer, encrypted backup/restore + restored instance, fail-closed configs, compose parity: **63 PASS / 0 FAIL**. P0 fixes: idempotency test contract, deadline/cancel sweep race. `REVIEW-R7.md` PASS (P0=0/P1=0; P2 image-inspect evidence fixed and verified) — see [`R7.md`](R7.md) |
| **R6.1** container runtime closure | **NOT_RUN / DEPLOYMENT_BLOCKED** (2026-10-25) | Docker client 29.7.2 present but daemon/VM not running (`docker info`/`version` exit 1, socket `_ping` 000, `docker desktop status` exit 1); no authorized alternate Docker env. Build/run/health/persistence/migration/SIGTERM/SIGKILL/single-writer/backup/security gates **NOT_RUN**; **no image, no Image ID/digest**; no code changes — [`R6.1.md`](R6.1.md), evidence `R6.1-docker-probe.log` |
| **Independent review** | **PASS (scoped)** after fixes | first pass PARTIAL (P0=0, P1=3, P2=11); two code-level P1s fixed and independently re-verified, TLS ingress reclassified as deployment `NOT_RUN`; see [`REVIEW.md`](REVIEW.md) §8 |

**Machine-checkable gates (actually run):**

- `make verify` (gofmt + `go vet ./...` + `go test ./... -race -count=1` + build) → **exit 0**
- `go test ./... -count=1` → **29 packages (14 with test files), 353 top-level tests (348 PASS + 5 opt-in SKIP) / 665 passes including subtests, 0 FAIL** (after R8 sprint fixes)
- `ONBOARD_SANDBOX_E2E=1 go test ./tests/e2e -v -count=1` → **exit 0, 4/4 PASS**
  (5/5 identities, crash/restart, idempotency, cancel) — independent Lead reruns in
  [`evidence/R2-lead-rerun.log`](evidence/R2-lead-rerun.log) (pre-review) and
  [`evidence/R2-lead-rerun3.log`](evidence/R2-lead-rerun3.log) (after the review P1 fixes)
- `docker compose -f deploy/compose.yaml config` → **exit 0**;
  `docker info` → **exit 1** (no daemon ⇒ Docker build/run **NOT_RUN**)

## 2. What was built

```
cmd/onboardd/                 serve / migrate / healthcheck / version, graceful shutdown
internal/api/                 control-plane API, bearer + HttpOnly session auth, CSRF/Origin,
                              rate limiting, minimal server-rendered console
internal/workflow/            durable engine, templates, steps, retry/backoff, state machine
internal/storage/             SQLite WAL schema (0001+0002), repositories, lease fencing,
                              events/resources/mail receipts/audit/secrets, backup helpers
internal/security/            AES-256-GCM keyring (versions, AAD), sessions/CSRF, redaction
internal/secrets/             opaque secret:// service (fail-closed)
internal/provider/            contracts + gated registry + fake world + boot wiring
internal/provider/mailbox/    Mailpit (local sink) and Cloudflare Temp Email adapters
internal/provider/keycloak/   sandbox identity + OIDC PKCE + credential verifier
internal/provider/github/     existing-account official email API adapter
internal/provider/workbuddy/  permission-gated gateway control-plane adapter
internal/mailx/               message matching + link/OTP extraction (transient only)
internal/ops/                 backup/verify/restore/rotate/redact-scan/load/http-audit tooling
tests/sandbox/                real local Keycloak + Mailpit fixture (no Docker)
tests/e2e/                    opt-in 5-identity / crash / idempotency / cancel gates
tests/r0/                     R0 end-to-end smoke with fake providers
deploy/                       hardened compose manifest, env example, secret placeholders
docs/                         baseline audit, ADRs, security audit, per-phase evidence
```

Key invariants implemented (details in `README.md` and the phase reports):

- One write connection with `BEGIN IMMEDIATE`; external HTTP never runs inside a
  transaction; claims are fenced by `(lease_owner, fencing_generation)`.
- Idempotency `(owner, template, idempotency_key)` + request fingerprint.
- Append-only redacted events with a strict payload allowlist.
- Secrets sealed with AES-256-GCM (nonce + key version + AAD `kind|id|version`);
  only opaque `secret://` references are persisted.
- Provider allowlists, cross-host redirect refusal, loopback-only sandbox fixture.
- Rate limiting (general + login buckets) and a viewer read-only role; cookie
  mutations require Origin/Referer; `Secure` cookies under TLS/trusted proxy.
- Real WorkBuddy adapter is **disabled by default** and additionally requires
  `ONBOARD_WORKBUDDY_PERMISSION_ACK`; the real adapter only talks to a
  loopback/RFC1918 gateway and never to the public internet.

## 3. Functional requirements (design `docs/01`)

| ID | Requirement | Status | Evidence |
|---|---|---|---|
| F01 | Create/query/cancel/resume workflows; API idempotent | **PASS** | API + storage tests; R0 smoke |
| F02 | Durable status/events; no false success after restart | **PASS** | R0 restart durability; R2 SIGKILL crash test |
| F03 | Provider layering + contract tests | **PASS** | R1/R2/R3 contract suites |
| F04 | Self-owned mailbox create/dedupe read/cleanup | **PASS** (Mailpit/Fake); Cloudflare live **NOT_RUN** | R1 |
| F05 | Test IdP real email verification + OIDC PKCE | **PASS** (LOCAL SANDBOX 5/5) | R2 |
| F06 | GitHub only for already-authorized accounts, no account creation | **PASS (contract)**; live **NOT_RUN** | R3 |
| F07 | OAuth pause for human consent; failure recovery; no implicit consent | **PASS** | engine wait states + R3 410 mapping; no consent automation exists |
| F08 | Secrets only via secret injection; never in API/logs | **PASS** | security tests, redaction scan, leak scans |
| F09 | Minimal UI: runs, timeline, action required, retry, cancel, summary | **PASS** | API/UI tests; R0 smoke |
| F10 | 5 sandbox users unattended + idempotency/fault gates | **PASS (LOCAL SANDBOX)**; multi-process lease E2E **NOT_RUN** | R2 |

## 4. Acceptance matrix (`tests/acceptance-matrix.md`)

| Case | Status | Note |
|---|---|---|
| Idempotent run creation | **PASS** | storage/API tests + R2 idempotency gate |
| Durable retry after crash | **PASS** | R2 crash gate (`request_verification` not re-run, one identity) |
| Five sandbox identities | **PASS (LOCAL SANDBOX)** | R2 5/5 through `/v1/runs` |
| Forbidden provider | **PASS (unit/API)** | disabled provider → `NOT_AUTHORIZED` before network; R2 §9 E2E-012 only PARTIAL (no API-driven forbidden run in the live fixture) |
| OAuth state expiration | **PASS (fake/contract)** | R3 fake 410 → `REAUTHORIZE_REQUIRED`; real gateway **NOT_RUN** |
| Secret redaction | **PASS** | redaction tests + event allowlist + R2/R4 scans |
| 429/Retry-After, redirect credential leak, malformed JSON, OTP reuse, lease fencing, backup/WAL, CSRF | **PASS** | R4 audit + provider/engine/storage tests |

## 5. Deviations from the design pack

Full table in [`docs/baseline-audit.md`](../baseline-audit.md) §4. Summary:

| ID | Deviation | Why |
|---|---|---|
| D1 | Additive migration `0002` instead of editing `0001` | safe-migration rule; 0001 stays byte-identical |
| D2 | Extra state edges (`StepWaitingEmail`, `VALIDATING→OAUTH_PENDING`, wait→`CANCELLED`) | the design's step vocabulary lacked an email-wait status; denial needs a legal terminal edge |
| D3 | Optional `email_provider` / `credential_provider` / `sandbox_provider` + `KindEmail` | three required provider fields cannot express the GitHub official adapter or a sandbox OIDC driver |
| D4 | Extra API fields/events (`deadline_at`, steps, audit, `RUN_RESUMED`) | operator observability of documented resume/cancel transitions |
| D5 | Go-native Keycloak/OIDC driver instead of Playwright | no browser engine available; ADR-006; Playwright recorded **NOT_RUN** |
| D6 | Docker build/run/GHCR **NOT_RUN** | Docker daemon unavailable in this environment |
| D7 | cgo `mattn/go-sqlite3` driver | ADR-007 (pure-Go module download stalled); contained in `internal/storage` |
| D8 | Cursor pagination `(created_at_ms,id)`; events by `after_seq` | deterministic and drift-free |

Integration defects found only by the real sandbox (documented, fixed, tested):
Mailpit detail endpoint/`Date` field, engine email-body hydration, Keycloak
Secure-cookie jar on http loopback (see `R1.md` §14 and `R2.md` §8).

## 6. Security posture and residual risk

Implemented controls (audited in [`docs/security/R4-audit.md`](../security/R4-audit.md)):

- SSRF: every live adapter has a construction-time host allowlist; redirects that
  change scheme/host/port are refused; credentials are never forwarded.
- Secret handling: AES-256-GCM at rest, fail-closed on missing/wrong key version,
  no plaintext fallback, secret values never rendered by the console.
- CSRF/Origin: cookie mutations require `Origin` or same-host `Referer` **and** a
  CSRF token; session cookie is HttpOnly + SameSite=Strict (+Secure under TLS).
- Rate limiting: per-IP general and login buckets, `429` + `Retry-After`.
- RBAC: admin vs viewer (read-only) tokens; single-operator model (no per-user
  accounts/2FA — explicitly out of v0.1.0 scope).
- Backups: WAL-safe `VACUUM INTO`, checksum/schema manifest, optional AES-256-GCM
  encryption with fail-closed restore; key rotation is transactional and
  idempotent.
- Platform boundaries: no account creation, CAPTCHA/anti-bot bypass, fingerprint
  spoofing, quota cycling, or credential extraction; WorkBuddy real calls require
  an explicit permission acknowledgement and a private gateway.

Residual/open items:

| ID | Risk | Status |
|---|---|---|
| P1-2 | TLS ingress not deployed in-repo (loopback-only compose) | **NOT_RUN**; cookie `Secure` works under TLS/trusted proxy; deployment contract documented |
| P1-5 | Rotation/restore require a stopped service; tooling cannot prove it | documented operator procedure with `CONFIRM_*`; rollback-safe scripts |
| P2 | WorkBuddy authorization URL host not pinned; in-memory rate limiter is single-instance; trusted-proxy headers can be spoofed if clients can bypass the proxy; base images not digest-pinned | documented; accepted for a single-instance self-hosted v0.1.0 |
| R2 | E2E-004 (120 s mail delay), E2E-007 (multi-process lease), E2E-008 (exact external-success/pre-commit crash window) are PARTIAL/NOT_RUN at E2E level; covered by unit/adapter tests where possible | see `R2.md` §9 |
| R2 | Mailbox creation is the only non-idempotent external write (Cloudflare has no lookup-by-key API); a crash in that narrow window can leave a fresh self-owned address | documented; no automatic deletion of real addresses by design |

## 7. Explicit NOT_RUN list

- Live Cloudflare Temp Email calls (no admin/address credentials in this environment).
- Live GitHub API calls on a real account (requires operator authorization; the
  account holder must confirm the verification email).
- Live WorkBuddy gateway calls (adapter disabled; written platform permission absent).
- Playwright/browser automation for R2 (deviation D5; Go-native OIDC driver used).
- Docker image build/run, container healthcheck in a container, GHCR publish.
- TLS ingress / HSTS validation (no ingress component executed).
- GitHub Actions CI execution on a remote runner (workflow file provided, not run).
- Production restore drill; 30×/5× production reliability campaign (offline
  5-concurrent + 30-sequential load smoke was executed by R4).

## 8. How to reproduce

```bash
# Offline gates
make verify
make contract
make security
bash tests/r0/smoke.sh

# Real local sandbox (Keycloak + Mailpit, loopback, no Docker)
bash tests/sandbox/up.sh
bash tests/sandbox/verify.sh --count 5
source tests/sandbox/env.sh && ONBOARD_SANDBOX_E2E=1 go test ./tests/e2e -v -count=1

# Reliability tooling
make ops-test
ONBOARD_BACKUP_ENCRYPT=1 ONBOARD_KEYRING=<keyring.json> scripts/backup.sh --encrypt <out.sqlite>
scripts/verify_backup.sh <out.sqlite.enc> --keyring <keyring.json>
scripts/load_smoke.sh
docker compose -f deploy/compose.yaml config
```

## 9. Authorization boundary (observed)

No `git push`, no PR, no release, no deployment, no modification of the existing
Cloudflare Worker/D1/DNS or the WorkBuddy gateway, no real third-party API writes,
and no secrets committed. The local Keycloak/Mailpit fixture and all generated
test credentials live under the gitignored `.sandbox/` directory. The only
external probes were unauthenticated reachability of `api.moonbyte.top` and public
documentation/release downloads for the local test fixture.

## 10. Independent adversarial review and P1 closure

See [`REVIEW.md`](REVIEW.md) for the reviewer's P0/P1/P2 table, reproduction
commands and verdict. The first pass returned **PARTIAL** (P0=0, P1=3, P2=11).
Two code-level P1 findings were fixed and covered by new regression tests; the
third (TLS ingress) is an infrastructure `NOT_RUN`, as before.

| Finding | Fix | Regression evidence |
|---|---|---|
| P1-1 state-machine/commit gap: AUTH_EXPIRED after OAuth produced an illegal `CREDENTIAL_VERIFIED → REAUTHORIZE_REQUIRED` edge, so the commit was rejected, the lease was retained, and the step re-ran without failure budget until the deadline | added the missing edge; added `safeNextState` so any future illegal mapping fails closed as `PROVIDER_BUG/FAILED_TERMINAL` with a counted failure; added missing success edges `WAITING_USER_ACTION → CREDENTIAL_VERIFIED` and `WAITING_EMAIL → COMPLETED` (the latter was latent for the Cloudflare smoke template) | `TestEveryStepOutcomeIsCommitLegal` (every template step × every error class × success edge), `TestSafeNextStateFailsClosedOnIllegalEdge`, `TestCredentialExpiryAfterOAuthRequiresReauthorization` (durable `REAUTHORIZE_REQUIRED`, `action_required=true`, no deadline spin) |
| P1-2a crash window: the verification message was consumed before the step commit, so a crash could permanently lose the link | mail receipts are now inserted in the same transaction as the successful step commit; `wait_email` checks remote verification first, confirms, then attaches the receipt; pending confirmation never consumes the message | `TestMailReceiptsAreAtomicWithStepCommit`; full live E2E rerun after the change (`R2-lead-rerun3.log`) |
| P1-2b Cloudflare duplicate created a second address (`enablePrefix=true` retry) | duplicate now returns `CONFLICT` and never creates a second address; operator reconciliation required | `TestCreateAddress_DuplicateFailsClosed`, `TestCreateAddress_DuplicateMarkerIn400BodyFailsClosed`, `TestCreateAddress_DuplicateTwiceIsConflict` |

Additional post-review P2 fixes: raw 32-byte master keys accepted verbatim;
mail-smoke `From` parsed as address/domain (no substring match); UNIQUE-constraint
detection narrowed; template filter applied in SQL before pagination; the viewer
role cannot see `authorization_url` (single-use OAuth state); explicit logout
revokes the session server-side.

Remaining open items after closure (all disclosed in `REVIEW.md` §8.6):

| ID | Item | Status |
|---|---|---|
| OPS-1 | TLS ingress not deployed in-repo; **any non-loopback exposure must terminate TLS at a reverse proxy first** (default config refuses non-loopback listen, compose publishes 127.0.0.1 only, proxy headers are opt-in) | deployment `NOT_RUN` (reclassified from code P1 with rationale) |
| P2-3 | Logout revocation is a **process-memory** map: after a restart, a pre-logout cookie remains valid until its TTL; multi-instance does not share it | documented limitation; no per-user session store in v0.1.0 |
| P2-8 | No in-service resource compensation/cleanup (`MailboxCleaner` has no caller); real addresses are operator-managed | documented |
| P2 | WorkBuddy authorization-URL host not pinned, in-memory rate limiter (single instance), trusted-proxy header caveat, image digest pinning, rotation/restore downtime orchestration | documented P2/deployment items |
| P2 | Admin-token rotation does not revoke existing browser sessions; rollback to the pre-R5 binary loses durable revocation semantics | rotate the master key (invalidates all sessions) before rollback; documented in `R5.md` §6 |
| P2 | Deleting the `session_revocations` table is not auto-healed (by design); `/readyz` reports 503 | restore from backup or recreate per migration 0003 |

## 11. R5 release readiness (2026-10-24)

R5 executes the release-preparation round on top of `e557387`; details and
commands are in [`R5.md`](R5.md).

| Gate | Result | Evidence |
|---|---|---|
| R5-A durable session revocation | **PASS** | migration 0003 → schema 3; logout revocation in SQLite bound to the cookie's own expiry (`max(now+TTL, cookie exp)`); concurrent/double logout, restart Server, expiry/prune, fail-closed-on-store-error and v2→v3 migration tests; real `onboardd` SIGTERM restart script **11/11 PASS**; `/readyz` additionally probes the revocation table |
| R5-A existing mechanisms re-check | **PASS** | atomic mail receipts, lease fencing, key rotation, encrypted backup/restore targeted tests + full race suite |
| R5-B container acceptance | **NOT_RUN** | `docker info` / `docker build` exit 1 (no daemon); `docker compose config` exit 0 (static audit only) |
| R5-C Cloudflare live | **NOT_RUN** (read-only audit PASS) | no network request, no credentials/authorization; contract/retention/redaction audit and reproducible live plan in `R5.md` §4 |
| R5-D identity/platform boundaries | **PASS (bounds)**; live **NOT_RUN** | provider catalog all live providers disabled; WorkBuddy without permission ack refused; GitHub without token refused; non-loopback listen refused (archived in `evidence/R5-provider-boundaries.log`, 5/5); TLS ingress still `NOT_RUN` |
| Independent R5 review | **PASS** (P0=0, P1=0; 8 P2 closed) | [`REVIEW-R5.md`](REVIEW-R5.md); reviewed/verified HEAD `a488740` (this report's commit adds only documentation); tree manifest `sha256= f4614dd803cf61157d6c70220f0ef3215c75a9572b7e5d3e4a793c0a371935c5` |
| Final verdict | **LOCAL_RC_READY / DEPLOYMENT_BLOCKED / not LIVE_READY** | `R5.md` §6 |

Final sprint HEAD: `a35c978` (provenance evidence was generated at code HEAD
`38b17df`; later commits add evidence/docs only, no runtime code). Release
environment is local sandbox only; Docker build/run, TLS ingress, GHCR/publish,
Cloudflare/GitHub/WorkBuddy live remain **NOT_RUN**.

## 12. R5.1 / R5.1-B container & TLS reliability (2026-10-24..25)

| Gate | Result | Evidence |
|---|---|---|
| R5.1-C local HTTPS ingress | **PASS** | `tests/r51/tls_acceptance.sh` exit 0, **26/26**: test CA + hostname verification, TLS 1.3 negotiated / TLS 1.1 refused, HTTP 308→HTTPS, Secure+HttpOnly+SameSite=Strict, CSRF/Origin 403, logout+replay rejection, revocation survives SIGKILL+restart, forged Forwarded/X-Forwarded-* cannot downgrade or spoof, forged Host on the HTTP redirect rejected 400, loopback-only listeners, non-loopback listen refused; [`R5.1.md`](R5.1.md) §2 |
| R5.1-B Docker runtime | **NOT_RUN** | daemon unavailable (`docker info`/`docker build`/`docker images`/`docker ps` exit 1); no image/digest/container; `docker compose config` exit 0 (static); hardened isolated script exit 3 = NOT_RUN; JSON-keyring + `/readyz`-based HEALTHCHECK readiness fixes locally tested (6/6); static audit also exposed a secret-readability risk for UID 10001 (preflight warning + acceptance assertions); post-review P2s (backup args, redirect Host allowlist, lock ops semantics, reproducible override) closed; **CONTAINER_RC_READY not claimed** — [`R5.1-B.md`](R5.1-B.md) |
| R5.1-D reliability | container **NOT_RUN**; process-level **PASS** | unconditional single-writer `flock` lock (second instance refused, released on SIGKILL; rolling-upgrade/migrate stop requirements documented), missing revocation table `/readyz` 503 + cookie fail-closed, missing master key fail-closed, sandbox E2E 4/4 PASS (crash/idempotency/cancel) |
| Verdict at R5.1-B | **DEPLOYMENT_BLOCKED** (historical) | TLS passed, but Docker/container acceptance was then the remaining deployment blocker; superseded by R7 `CONTAINER_RC_READY` |

## 13. Post-review verification

The independent reviewer re-verified the fixed tree and updated
[`REVIEW.md`](REVIEW.md) with the post-review evidence and final verdict:
**PASS (scoped)**, P0=0, code-level P1=0; the remaining items are deployment
prerequisites (`OPS-1` TLS ingress), documented limitations (`P2-3` in-memory
session revocation, `P2-8` no in-service compensation) and the NOT_RUN list.

Delivery-tree integrity (reviewer-computed over 126 implementation files):

- manifest `sha256(concat(sorted("<path> <sha256>\n")))` =
  `1a65ea11f2859c03ae18a28d11c2e5581288930d6376b3533a96366c9296186e`
- `state.go` = `ed1d689506ca275a32d07f89511cc305794cb97ba17e7ac63be5ec6ae2242188`
- gate run before/after: `TREE_STABLE: True`

Lead's final gates on that tree: `make verify` (gofmt + vet + `go test ./...
-race` + build) **exit 0**; `ONBOARD_SANDBOX_E2E=1 go test ./tests/e2e -v`
**exit 0, 4/4 PASS** (5/5 identities, crash/restart, idempotency, cancel;
evidence [`evidence/R2-lead-rerun-final.log`](evidence/R2-lead-rerun-final.log)).
Docker build/run, GHCR, TLS ingress and real-platform live tests remain
`NOT_RUN`; this PASS must not be read as "internet-ready" or "live platforms
verified".

## 14. R6 autonomous sprint (2026-10-25)

Full progress log and commands: [`AUTONOMOUS-SPRINT-20261025.md`](AUTONOMOUS-SPRINT-20261025.md).

| Workstream | Result | Evidence |
|---|---|---|
| P0 Docker runtime | **NOT_RUN** (daemon unavailable; no image/container/digest) | `docker info/build/images/ps` exit 1; `docker_acceptance.sh` exit 3; no host changes |
| P0-B offline container prep | **PASS** | Dockerfile audit 22 PASS (found+fixed `.dockerignore` leaking `deploy/secrets`/`deploy/data`), compose matrix 6/6, 24 scripts `bash -n` 0, manual CI workflow (not executed) |
| P1 reliability/security | **PASS** (process-level; container-level NOT_RUN) | SQLite/WAL/SHM hardened to 0600 + test; SQLite busy-wait test; goroutine-leak test; HTTP concurrency test; encrypted backup→restore keeps run + revoked session (11/11); sandbox E2E 4/4 on the sprint tree |
| P2 Cloudflare live prep | **PARTIAL / NOT_RUN live** | dry-run gate + contract tests PASS; `--live` refuses without explicit written authorization (exit 2); no request to `api.moonbyte.top`; no delete/DNS/Worker/D1 changes |
| P3 deploy/supply chain | **PASS (offline)** | version/commit injection; reproducible build (same sha256 twice); module metadata/SBOM input; TLS ingress Caddy/nginx reference configs; backup/restore/rollback/first-deploy/security-gate/supply-chain runbooks; scanners (`govulncheck/syft/trivy/shellcheck`) NOT_INSTALLED → NOT_RUN |
| P4 independent review | **PASS (scoped)**, P0=0/P1=0, 4 P2 fixed + 11 P3 addressed | [`REVIEW-R6.md`](REVIEW-R6.md): CI pipefail fixed, Cloudflare dry-run now loads real config with negative case, provenance regenerated at final HEAD, DB-perm test strict, goroutine tolerance tightened, scanner false positives fixed, compose build args added |

New issues fixed in this sprint: `.dockerignore` secret/data context leak; SQLite
database/WAL/SHM files were created 0644 (now 0600); missing build provenance
(version/commit now injected and printable); CI container workflow could
mask a failing gate through a pipe (now `shell: bash` + pipefail + SUMMARY
assertion); Cloudflare dry-run's config assertion now uses a real
`onboardd configcheck` with a negative case; redact_scan no longer false-positives
on Go import paths/env-var names. Remaining deployment blockers:
Docker runtime evidence, production TLS ingress, and live-platform
authorizations. `CONTAINER_RC_READY` is **not** claimed.

## 15. R6.1 container runtime closure attempt (2026-10-25)

This round's only executable scope was the Docker gate; the daemon is still
unavailable, so it stopped early instead of repeating offline tests.

| Item | Result |
|---|---|
| Docker client / compose | 29.7.2 / v5.5.0 (client only) |
| Docker engine | **unavailable**: `docker version`/`docker info`/`ServerVersion` exit 1; socket stale (`_ping` 000); `docker desktop status` exit 1 |
| Authorized alternate Docker env | none (`DOCKER_HOST` unset; only local contexts; no remote authorization) |
| Build / Image ID / Digest | **NOT_RUN / N/A** (no image built, nothing pushed) |
| Container run, health, non-root, read-only, cap_drop, SQLite init/upgrade/WAL perms, volume persistence, SIGTERM/SIGKILL, lease/lock, single-writer, revocation persistence, encrypted backup/restore, fail-closed configs | **NOT_RUN** |
| Code changes | none (no runtime failure observed) |
| Resources created | none; nothing to clean up |
| Final | **DEPLOYMENT_BLOCKED**; `CONTAINER_RC_READY` not met; `LIVE_READY` out of scope |

Exact operator unblock steps are in [`R6.1.md`](R6.1.md) §6: start Docker
Desktop/daemon, verify `docker info` exit 0, then rerun `make verify`,
`docker compose config`, `tests/r51/docker_acceptance.sh` and record the image
ID/platform for review.

## 16. R7 source-review closure and real container CI (2026-10-25)

The container gate was executed for real on a GitHub-hosted Ubuntu runner
(final Actions run 40161727232, commit 50f47fb, Docker Engine 28.0.4):

| Item | Evidence |
|---|---|
| Image | `sha256:352b85ec6360e2ff9c1f3d22b6e7bdb7cd567c4a4daba1a0b4dc369ddfe69763`, linux/amd64, 97,417,603 bytes, user 10001:10001; local Image ID only (no GHCR push/manifest digest) |
| Container gate | `tests/r51/docker_acceptance.sh` **63 PASS / 0 FAIL** |
| Covered | build+commit injection, non-root, read-only rootfs, cap_drop ALL, NNP, loopback-only, authenticated health/readiness, schema 3, v2 upgrade, SQLite/WAL/SHM 0600, secret 0400 + UID 10002 denial, idempotency contract, revocation, SIGTERM/SIGKILL, single-writer, encrypted backup/restore (+ restored instance), fail-closed configs, compose parity/strict secrets |
| Evidence | `docs/acceptance/evidence/r7-container-ci/` |
| Reports | [`R7.md`](R7.md), [`REVIEW-R7.md`](REVIEW-R7.md) (independent verdict) |

P0 review fixes are closed: the Docker acceptance script now honors the real
Idempotency-Key contract (with a no-Docker self-test), and the deadline sweeper
re-checks CANCEL_REQUESTED/deadline in-transaction with conditional updates and
single terminal-event emission (5 deterministic race tests).

`CONTAINER_RC_READY` is claimed after the independent R7 review confirmed
P0=0/P1=0 and the review P2 (image-inspect artifact after cleanup) was fixed and
verified by the final CI run. `LIVE_READY` remains out of scope (no production
TLS, no GHCR/push, no live Cloudflare/GitHub/WorkBuddy acceptance).

## 17. R8 staging release readiness (2026-10-25)

Branch `feat/r8-staging-readiness` (code/evidence HEAD `74d2c4e`), base
`9726fc1`. Real GitHub Actions evidence (final rerun):

| Gate | Result | Evidence |
|---|---|---|
| Isolated staging + test-CA TLS | **27/27 PASS** (staging app/ingress, internal network, loopback ingress only) | actions run 40168648183; `evidence/r8-staging/` |
| R8 container regression | **63/63 PASS** (build/runtime/health/schema v2→v3/SIGTERM/SIGKILL/single-writer/encrypted backup/restore/restored instance) | same run, container-regression job |
| Supply chain | **PASS**: govulncheck no vulnerabilities; Trivy CRITICAL=0/HIGH=0; ShellCheck errors=0; Syft SPDX-2.3 (97 pkgs) + CycloneDX; Go 1.26.10 | actions run 40168648195; `evidence/r8-supply/` |
| External operation reconciliation | event-log audit PREPARED/CONFIRMED/OUTCOME_UNKNOWN/RECONCILIATION_REQUIRED/FAILED + admin diagnostics; ADR 0001 | `internal/workflow`, `internal/storage`, `REVIEW-R8.md` |
| Rollback safety | future-schema `ErrSchemaTooNew` refuses old binary; backup restore to new volume; old-image rollback itself **NOT_RUN** | storage test + container regression |
| Local gates | `make verify` 0; race 0; 353 top-level (348 PASS+5 SKIP)/665; Docker unavailable locally | — |

Images are local IDs only (staging app `sha256:924ddb…`, container regression
`sha256:49f3e0…`, supply image `sha256:c4e0b8…`); **no registry image digest**
(base-image digests were resolved separately via buildx and are recorded in the
supply metadata).
`REVIEW-R8.md` is PASS (P0=0/P1=0; P2 closed) and the P2 fixes were re-verified by
the final CI rerun; therefore `STAGING_RC_READY` is claimed. `LIVE_READY` remains out of scope (no production TLS, GHCR publish or live
Cloudflare/GitHub/WorkBuddy acceptance).

## 18. R9 external consistency and release hardening (2026-10-25)

Branch `feat/r9-release-hardening` (code/CI HEAD `5f13d78`, base R8
`de89878`; main still `9726fc1`). All final CI jobs PASS:

| Gate | Result | Evidence |
|---|---|---|
| staging + container regression | **27/27** + **63/63** | run 40176187395 |
| supply chain | govulncheck 0, Trivy CRITICAL/HIGH 0, ShellCheck 0, SPDX+CycloneDX | run 40176187340 |
| old-image upgrade/rollback/restore | **20/20 PASS** | run 40176187307, `evidence/r9-rollback/` |
| real Caddy reverse proxy + TLS | **17/17 PASS** (1 WARN oversize body) | run 40176187307, `evidence/r9-caddy/` |
| release candidate | Candidate PASS (real scans), Publish **skipped** | run 40176187349 |
| local | make verify 0; race 0; **350 top-level (345 PASS+5 SKIP)/662**, 0 FAIL | — |

Key changes: migration 0004 durable external_operations gate (the event log is
audit, not a mutex); no automatic provider call after unknown/confirmed/resolved;
only FAILED_DEFINITE can retry; worker lease/fencing revalidated at permit;
pagination uses global counts + stable cursor; admin resolve is CSRF/RBAC/version-
CAS/audit-atomic; CLI `scripts/ops_external.sh`; old binary refuses schema 4 and
future schemas; pre-upgrade backup restore works with the old binary; wrong-key
restore cannot overwrite valid data; real Caddy with test CA and no app published
port. Release Publish requires protected `release` Environment + explicit
publish/tag and never ran. `REVIEW-R9.md` final verdict: PASS, P0=0/P1=0/P2=0/P3=9; therefore
`RELEASE_CANDIDATE_READY` is claimed for the reviewed candidate. `LIVE_READY`
remains out of scope; real Cloudflare/GitHub/WorkBuddy and production TLS/GHCR
are NOT_RUN.
