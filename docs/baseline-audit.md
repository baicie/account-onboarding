# Baseline audit (read-only) — 2026-10-24

Scope: verify the environment, the existing self-hosted systems, and the gap
between the v0.1.0 design pack and the empty repository before writing code.
No production state was modified. No third-party credentials were used.

## 1. Environment facts (probed, not assumed)

| Item | Result |
|---|---|
| Workspace | `/Users/liuzhiwei/Desktop/workspace/git-code/account-onboarding` (empty git repo, no commits) |
| Design pack | extracted from `account-onboarding-design-v0.1.0.zip`; 25 files, SHA256SUMS.txt included |
| Go | 1.25.3 darwin/arm64; `CGO_ENABLED=1`; Apple clang 17 |
| SQLite driver | `github.com/mattn/go-sqlite3 v1.14.32` (cgo). Chosen over `modernc.org/sqlite` because the pure-Go module download stalled for >20 minutes in this environment; the cgo driver downloaded in <60 s and was verified with WAL + `_txlock=immediate` in a scratch probe. |
| Node | v22.12.0, npx 10.9.0 (Playwright cache absent) |
| Docker CLI | 29.7.2; compose v5.5.0 |
| Docker daemon | **NOT RUNNING** — `Cannot connect to the Docker daemon`; Docker Desktop backend could not be started headlessly; `open -a Docker` retried, socket never appeared. All Docker build/run evidence is therefore `NOT_RUN`; `docker compose config` remains usable without a daemon. |
| Java | system 25 (Temurin), Homebrew OpenJDK 21 present at `/opt/homebrew/opt/openjdk@21/...` — needed for Keycloak 26.x |
| Network | `proxy.golang.org` 200, `api.moonbyte.top` 200 (unauthenticated root probe only), GitHub releases reachable |
| Loopback ports | 8788 free; 8080/8081 belong to an unrelated local Java project (left untouched); sandbox uses 8180/1025/8025 |
| Credentials in env | none for Cloudflare/GitHub/Keycloak/WorkBuddy (`env` scan found no matching secret names) |

## 2. Existing systems (read-only code audit)

### Cloudflare Temp Email (`cloudflare_temp_email` checkout present locally)

Confirmed from `vitepress-docs/docs/zh/guide/feature/new-address-api.md`:

- `POST /admin/new_address` with `x-admin-auth`, JSON `{enablePrefix,name,domain}`,
  optional `x-custom-auth` when the Worker private-site password is enabled.
- Response contains `jwt`, `address`, `address_id` (address JWT is per-address and
  must not be confused with the user JWT / `x-user-token`).
- Address JWT is used as `Authorization: Bearer <jwt>` for `/api/*` message reads.

The live deployment (`api.moonbyte.top`) was **not** called beyond an
unauthenticated root probe. Its Worker version, D1 schema, admin password and
catch-all binding are unverified (`NOT_CHECKED`).

### `workbuddy2api-panel` (checkout present locally, HEAD `14045bc`)

The design pack's evidence still holds but the code has moved: the OAuth flow now
lives in `internal/panel/relogin.go` (shared with add-account), while
`internal/panel/login.go` retains the handlers:

- `POST /panel/api/login/start` (`p.withAuth`) → `{ok,url,state,realm}`;
  `realm` defaults to `cn`, `{"realm":"global"}` selects international.
- `GET /panel/api/login/poll?state=` (`p.withAuth`) → `{done:false,...}` while
  pending, `{done:true,...}` after credentials are written and hot-loaded,
  **410** for unknown/expired state.
- `POST /panel/api/login/cancel` also exists (explicit operator cancel).
- `loginTTL = 15 * time.Minute` in `internal/panel/panel.go`; sessions are
  in-process memory, so a gateway restart invalidates outstanding state.
- `state` is single-use and the success branch has side effects (`SaveAtomic`
  credential write). The orchestrator must never replay an old state.

These facts freeze the R3 adapter contract (410 → `AUTH_EXPIRED` →
`REAUTHORIZE_REQUIRED`; no token copying; control-plane only).

### Reference repositories

`VentourF/WorkBuddy2api` remains a background reference only: no LICENSE in the
design-pack evidence, so **no source was copied**. `any-auto-register` (AGPL-3.0)
was not used.

## 3. R0 gap list (before implementation)

- No Go module, no service, no database schema, no provider contracts.
- No secret boundary, no auth, no idempotency/lease/fencing implementation.
- No tests, Makefile, Dockerfile, CI, OpenAPI implementation mapping.
- Design-pack OpenAPI has only a draft subset (no `/v1/meta`, audit, events
  pagination details, step detail payloads, error classes).
- Design-pack `0001_initial.sql` lacks operator-facing fields (current step,
  resume state, action_required, request fingerprint), a failure counter for
  retry budgets, durable mail receipts and an audit log.
- Design pack's StepStatus vocabulary has no `WAITING_EMAIL` even though the
  workflow state machine has that wait state.
- Design pack assumes Playwright for R2; no browser engine is installed here.

## 4. Deviations from the design pack (all documented, additive)

| # | Deviation | Reason | Where |
|---|---|---|---|
| D1 | Added migration `0002_operator_fields.sql` (additive columns + `mail_receipts` + `audit_log`) instead of editing `0001_initial.sql` | Design requires safe migrations, no DROP as upgrade path; 0001 stays byte-identical to the pack | `internal/storage/migrations/` |
| D2 | Added `StepWaitingEmail`, `VALIDATING→OAUTH_PENDING`, `API→OAuthPending`-style wait edges and `WAITING_USER_ACTION/OAUTH_PENDING→CANCELLED` | The state machine needs a step status for email waits and a legal terminal edge when the account holder denies authorization; the design text allows "cancelled/failed" for denial | `internal/workflow/state/state.go` |
| D3 | Added optional `email_provider`, `credential_provider`, `sandbox_provider` request fields and `KindEmail` provider slot | The design's three provider fields cannot express the GitHub official-API adapter or a sandbox OIDC driver; optional fields keep the required trio backward-compatible | `contracts/openapi.yaml`, `internal/provider/contracts.go` |
| D4 | Extra run states kept internal but API exposes `deadline_at`, `authorization_url`, steps, audit; added events `RUN_RESUMED`, `RUN_CANCEL_REQUESTED` | Operator needs the documented "resume" and "cancel requested" transitions visible without duplicating state names | `internal/api`, `internal/workflow` |
| D5 | R2 E2E driver: Go-native OIDC/Keycloak HTTP automation instead of Playwright | No browser/Playwright installed; the acceptance objective is *no human interaction* against a self-controlled IdP. A real Keycloak + real Mailpit flow is still exercised. Browser-driven UI automation is recorded as `NOT_RUN` in R2 evidence. | `docs/acceptance/R2.md` |
| D6 | Docker build/run and GHCR publishing `NOT_RUN` | Docker daemon unavailable in this environment | all evidence reports |
| D7 | SQLite via cgo `mattn/go-sqlite3` | pragmatic, see environment table; the storage layer only uses the `database/sql` API, so a driver swap is contained in `internal/storage/store.go` | `go.mod` |
| D8 | Cursor pagination encoded as base64 `{created_at_ms,id}`; events paged by `after_seq` | Simple, deterministic, no offset drift under writes | `internal/storage/runs.go` |

## 5. Live-platform status at baseline

| Platform | Action taken | Status |
|---|---|---|
| Cloudflare Temp Email | docs/code audit only | `NOT_CHECKED` (no credentials) |
| GitHub official API | none | `NOT_RUN` |
| WorkBuddy gateway | source audit only; adapter default-disabled | `NOT_RUN` (permission gate) |
| Keycloak + Mailpit | local fixture (R2) | see R2 evidence |
| Docker/GHCR | none | `NOT_RUN` |

## 6. Authorization boundaries observed

- No git push, no PR, no release, no deployment to any server.
- No changes to the Cloudflare Worker, D1, DNS, or the existing gateway.
- Only loopback-bound local processes; generated test secrets stay out of git
  (`.sandbox/` is gitignored).
