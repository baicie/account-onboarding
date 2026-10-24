# tests/e2e — LOCAL SANDBOX end-to-end gate (R2)

These Go tests drive the **real `onboardd` binary** against the **real local
Keycloak + Mailpit fixture** (`tests/sandbox/`). They are the executable
evidence for the R2 acceptance items: five independent identities through
`/v1/runs`, crash/restart durability, idempotent run creation and cancellation.

Everything is loopback-only (`127.0.0.1`). No Docker, no public IdP, no live
third-party service and no production credential is used. The tests are skipped
by default so `go test ./...` stays green without the fixture.

## 1. Start the fixture (once)

```bash
bash tests/sandbox/up.sh          # idempotent; Keycloak 127.0.0.1:8180, Mailpit 127.0.0.1:8025/1025
bash tests/sandbox/status.sh      # expect exit 0 and "LOCAL SANDBOX healthy"
```

## 2. Run the gate

```bash
cd <repo-root>
set -a; source tests/sandbox/env.sh; set +a
ONBOARD_SANDBOX_E2E=1 go test ./tests/e2e -v -count=1 -timeout 900s
```

Expected: `ok  github.com/baicie/account-onboarding/tests/e2e` (exit 0), with a
5/5 table, P50/P95 and one `evidence-*.json` per test.

Individual gates:

```bash
ONBOARD_SANDBOX_E2E=1 go test ./tests/e2e -run TestFiveSandboxIdentities -v -count=1
ONBOARD_SANDBOX_E2E=1 go test ./tests/e2e -run TestCrashRecoveryRestartsInWaitingEmail -v -count=1
ONBOARD_SANDBOX_E2E=1 go test ./tests/e2e -run TestIdempotencyKeyCreatesOneRun -v -count=1
ONBOARD_SANDBOX_E2E=1 go test ./tests/e2e -run TestCancelWhileWaitingEmail -v -count=1
```

Offline (no fixture): `go test ./tests/e2e -count=1` → every test `SKIP`, exit 0.

## 3. Environment consumed

`tests/sandbox/env.sh` exports these; the harness falls back to
`.sandbox/secrets/sandbox.env` (fixture-generated, gitignored) when an admin
secret is not in the shell:

| Variable | Used for |
|---|---|
| `KEYCLOAK_URL`, `KEYCLOAK_REALM`, `KEYCLOAK_CLIENT_ID` | adapter config + Admin REST assertions |
| `ONBOARDING_OIDC_REDIRECT_URI` | OIDC `redirect_uri` (must be `http://127.0.0.1:8788/oidc/callback`) |
| `MAILPIT_HTTP_URL` | verification-email assertions (read-only list) |
| `SANDBOX_ADMIN_USER`, `SANDBOX_ADMIN_PASSWORD` | password-grant fallback for the Admin API |
| `SANDBOX_ADMIN_CLIENT_ID`, `SANDBOX_ADMIN_CLIENT_SECRET` | service-account `client_credentials` (preferred) |
| `ONBOARD_SANDBOX_E2E=1` | explicit opt-in gate |

The subprocess environment is built hermetically: inherited `ONBOARD_*`
variables are dropped, then the fixture values are injected (listen, DB path,
master-key/admin-token **files** chmod 600, Keycloak/Mailpit URLs, sender
allowlist `moonbyte.test,idp.fake.test`, `ONBOARD_RUN_DEADLINE=5m`,
`ONBOARD_POLL_INTERVAL=1s`, `ONBOARD_MAX_CONCURRENCY=2`,
`ONBOARD_RATE_LIMIT_PER_MINUTE=6000`). `ONBOARD_ALLOW_PUBLIC_LISTEN` and
`ONBOARD_DEV_EPHEMERAL_KEY` are never set.

Ports (one `onboardd` subprocess per test, all bound to `127.0.0.1`):

| Test | Port |
|---|---|
| 5 identities | 18788 |
| crash/restart | 18789 |
| idempotency | 18790 |
| cancel | 18791 |

## 4. What is asserted

### TestFiveSandboxIdentities (E2E-001)
- registered catalog exposes `identity/sandbox/credential = keycloak-test` and
  `mailbox/mailpit`, enabled, `live=false`;
- 5 × `POST /v1/runs` (`sandbox-email-verify-oidc`, mode `sandbox-e2e`,
  distinct `Idempotency-Key`) → 5 distinct run ids;
- all 5 reach `COMPLETED`; each run's events contain `OAUTH_STARTED` and
  `RUN_COMPLETED`;
- Keycloak Admin REST: 5 distinct subjects/usernames/emails in the test window,
  every `emailVerified=true`, no duplicate identity;
- Mailpit: exactly one verification email per mailbox address (5/5);
- the orchestrator database is opened with the same master key and the stored
  `secret://keycloak_oidc/...` credential is decrypted **only** to call
  `GET /userinfo`; 5/5 return `200`, `sub` matches the stored subject and
  `email_verified=true`;
- prints the 5/5 table and per-run duration P50/P95; writes
  `evidence-five-identities.json`.

### TestCrashRecoveryRestartsInWaitingEmail (E2E-003/E2E-008)
- a run is held deterministically in `WAITING_EMAIL` (the engine's mailx sender
  allowlist deliberately omits `moonbyte.test`, so the delivered verification
  email cannot match yet — this is test configuration, not a code change);
- the Keycloak identity is created and `emailVerified=false`; then the process
  receives **SIGKILL**;
- the same SQLite data dir + master key show status `WAITING_EMAIL` and
  `request_verification.attempt_count = 1`;
- restart on the same data dir (normal allowlist restored) → `COMPLETED`;
  `request_verification` was **never** re-attempted (still 1), `wait_email`
  attempt count > 1 (same step resumed), exactly one Keycloak user exists and is
  verified, and the stored credential passes userinfo;
- writes `evidence-crash-recovery.json` (statuses, subject before/after,
  attempt counts, crash/restart timestamps, log names).

### TestIdempotencyKeyCreatesOneRun (E2E-002)
- same body + same `Idempotency-Key` twice → both HTTP 202, same run id, exactly
  one `RUN_CREATED` event;
- one Keycloak identity, one mailbox, exactly one delivered verification email,
  credential valid via userinfo; writes `evidence-idempotency.json`.

### TestCancelWhileWaitingEmail (E2E-006 surrogate)
- the run is held in `WAITING_EMAIL` as above, then `POST /v1/runs/{id}/cancel`;
- final status `CANCELLED`; `RUN_CANCELLED` event exactly once; **no**
  `OAUTH_STARTED` event and **no** `STEP_STARTED` for `oauth`/`verify_credential`;
- zero `credential_ref` resources, `oauth`/`verify_credential` attempts = 0,
  `request_verification` attempts = 1; writes `evidence-cancel.json`.

## 5. Teardown and safety

- Deleted at teardown: the sandbox Keycloak users created during the test window
  (`onboard-*` usernames, created within ±10s of the test start) and the Mailpit
  messages addressed to the run's own mailboxes (Mailpit has no per-message
  DELETE route; the harness uses the documented batch body
  `DELETE /api/v1/messages {"IDs":[...]}` and never purges the shared sink).
- Never deleted: the realm, the clients, other users' messages, or any
  non-test data.
- The realm/client and the `tests/sandbox/**` fixture are owned by another agent
  and are not modified here.
- Evidence and logs stay under `.sandbox/e2e/run-<timestamp>-<rand>/` (gitignored):
  `state.sqlite`, `master-key`, `admin-token`, `onboardd-N.log`,
  `evidence-*.json`.

## 6. Known limitations (honest scope)

- **No browser automation** (Playwright): the OIDC authorization-code + PKCE
  flow is driven by a Go-native HTTP driver inside
  `internal/provider/keycloak` (cookie jar, login-form POST, callback
  interception). Browser UI automation is `NOT_RUN`; see `docs/acceptance/R2.md`.
- **Docker compose execution is NOT_RUN** (no Docker daemon on this host); the
  manifest is only validated statically.
- The cancel/crash tests hold the run in `WAITING_EMAIL` by withholding the
  sender-allowlist entry for the Keycloak domain. This makes the window
  deterministic; the restart test restores the normal allowlist before resuming.
  It does not change production code or the fixture.
- Single host, single process; multi-instance lease fencing is covered by the
  storage/engine unit tests, not by this harness.
