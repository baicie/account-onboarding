# Cloudflare Live Preflight (最小权限验收)

R9 deliberately did **not** call `api.moonbyte.top` or change any real DNS,
Worker, D1 or Email Routing resource. Everything below requires explicit
operator authorization and a change window.

## Authorization scopes (must be approved separately)

| Scope | Example | R9 status |
|---|---|---|
| Read-only verification | list zones/routing rules, read Worker logs, verify existing test mailbox | NOT_RUN |
| Create dedicated test mailbox | one clearly named R9 test address, no wildcard | NOT_RUN |
| Send one test email | from an approved sender to the test address | NOT_RUN |
| Delete test resources | delete only the exact R9 test mailbox/messages created by this window | NOT_RUN |
| Modify existing DNS/Worker/D1/routing | **not authorized** | FORBIDDEN |

Do not reuse or delete existing mailboxes; do not change existing routing.

## Dry-run tool

`scripts/cloudflare_preflight.sh` defaults to a no-network dry run. `--live`
requires a dedicated test-address allowlist (`ONBOARD_CF_TEST_ADDRESSES_FILE`),
HTTPS base URL and a readable admin token file; it still refuses create/send/
delete. R10 only ran the dry run and fail-closed self test, never a real call.

## Preflight

1. Confirm the candidate digest/schema and that the external-operation gate is
   enabled (`/readyz` includes `external_operations_ready=true`).
2. Confirm secrets are provided by the deployment secret store, never command
   history; no JWT/OAuth token in logs, tickets or evidence.
3. Record the exact account/zone IDs (redacted in evidence) and the single test
   mailbox name.
4. Take a backup and confirm no unresolved external operations from earlier runs.

## Authorization and execution

1. Start the app with real provider enabled only after written approval for the
   read-only scope; run `/v1/ops/diagnostics` and confirm no secret leakage.
2. Obtain separate approval for creating the dedicated test mailbox. Expect
   `PREPARED -> IN_FLIGHT -> CONFIRMED`; if the response is lost/timeout, stop:
   the operation becomes `OUTCOME_UNKNOWN` and no retry is allowed until an
   operator checks the Cloudflare console.
3. Send a test email only after mailbox creation is CONFIRMED. Verify delivery,
   duplicate/out-of-order handling, pagination and sender/recipient checks.
4. If cancellation occurs after the gate acquired IN_FLIGHT, treat it as
   `OUTCOME_UNKNOWN`; do not recreate.
5. Cleanup only the exact test mailbox/messages after separate approval. Record
   the resolution/audit entries.

## Stop conditions

- Any unknown external outcome: stop, list with `scripts/ops_external.sh`, inspect
  the provider console, resolve with evidence; never blind-create another mailbox.
- Any credential/JWT/address appearing in logs/evidence: stop and redact.
- Any request to modify existing DNS/Worker/D1/routing: refuse (out of scope).

## Rollback

If the live window must be aborted: stop the app, restore the verified
pre-window backup if local data changed, and use the recorded external-operation
IDs for manual cleanup. A binary rollback follows
`RELEASE_UPGRADE_ROLLBACK.md`; do not run an old binary on schema 4.
