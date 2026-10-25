# Cloudflare Live Authorization & Acceptance Runbook (R12)

This runbook is the only approved path from `CONTROLLED_LIVE_TEST_READY` to a
limited Cloudflare mailbox read. R12 has **not** executed Authorized Read-Only
Live. A Mock Live result is not evidence that the real Worker is healthy.

## Authorization required before any request

An operator must approve in writing:

- Environment and exact HTTPS Worker host (port 443 only).
- One dedicated test address; never an existing personal or production mailbox.
- Operation: `READ_ONLY_LIST` only.
- Credential source and protected-file handling.
- UTC start/end window, request limit, time budget, and stop authority.
- Incident contact and cleanup decision for any separately authorized future write.

A development PR or a `CONTROLLED_LIVE_TEST_READY` label is not Live authorization.
If any field is missing, mismatched, expired, or outside the approved window, do not
start.

## Required authorization file

Create the file outside the repository and restrict it to the operator:

```text
AUTHORIZED=YES
allowed_host=REAL_WORKER_HOST
allowed_address=TEST@example.test
allowed_operation=READ_ONLY_LIST
window_start_utc=2026-10-26T02:00:00Z
window_end_utc=2026-10-26T02:02:00Z
max_requests=1
max_seconds=60
```

Use real values only after the operator has explicitly approved them. Do not copy
the example window or address as an implicit authorization. Set file permissions
to `0600`. The script rejects symlinks, group/world-readable files, duplicate or
unknown fields, non-UTC timestamps, an expired/not-yet-open window, mismatched
host/address/operation, and requested budgets exceeding the approved values.
The complete request time budget must fit inside the approved window.

The token file must also be a regular, non-symlink file with mode `0600`, bounded
size, and no embedded line breaks. The script passes the Bearer credential to curl
through a temporary mode-`0600` header file, not through process arguments, and
removes the temporary directory on exit/signals.

## Modes

### A — Dry Run

```bash
ONBOARD_CF_TEST_ADDRESSES='TEST@example.test' \
  scripts/cloudflare_preflight.sh --dry-run \
  --test-address TEST@example.test --max-requests 1 --max-seconds 60 \
  --report /secure/r12-dry-run.json
```

No network calls and no credential reads.

### B — Mock Live

```bash
scripts/cloudflare_preflight.sh --mock-live \
  --mock-url https://127.0.0.1:PORT --mock-ca /secure/mock-ca.pem \
  --test-address TEST@example.test --token-file /secure/mock-jwt.txt \
  --max-requests 1 --max-seconds 30 --report /secure/r12-mock-report.json
```

The target must be loopback. Use a local HTTPS mock and a test credential only.
A successful report uses `verification_status=MOCK_PASS`, never Live `PASS`.

### C — Authorized Read-Only Live

Only after the complete authorization file and token file exist with mode `0600`:

```bash
ONBOARD_CF_TEST_ADDRESSES='TEST@example.test' \
ONBOARD_CF_LIVE_AUTHORIZATION=/secure/r12-authorization.txt \
  scripts/cloudflare_preflight.sh --live --base-url https://REAL_WORKER_HOST \
  --test-address TEST@example.test --token-file /secure/test-address.jwt \
  --max-requests 1 --max-seconds 60 --report /secure/r12-live-report.json
```

The script issues one request only:

`GET /api/parsed_mails?limit=1&offset=0`

It does not create, modify or delete a mailbox, send email, or change DNS, Worker,
D1, or Email Routing. It requires HTTPS with normal certificate verification,
disallows redirects, limits the response to 1 MiB, validates the JSON Content-Type
and response envelope/item types, and reports only metadata rather than mail body
content. A 2xx status alone is insufficient for success.

## Stop conditions

Stop on redirect, TLS/host mismatch, 401/403, 429, any non-2xx response, timeout,
request-budget exhaustion, oversized/empty response, wrong Content-Type, malformed
JSON, unexpected schema/item type, or any sensitive value in output. Do not retry
a failed request automatically.

A response can prove that the request was accepted; one read-only request does not
independently prove the provider enforces authorization against invalid credentials.
Do not overstate the `auth_result` field as an authorization-policy penetration
test.

## Evidence fields

The JSON report records request method/path, approved scope, request/time budgets,
host/port, UTC start/end, duration, HTTP status, Content-Type, response byte count,
TLS result, request acceptance, schema validation, redaction status, stop reason,
temporary-file cleanup, verification status, and a hash of non-sensitive evidence
metadata. It does not store the Bearer token or response body.

- Dry Run fields remain `NOT_RUN`.
- Mock Live success is `MOCK_PASS`.
- Live `verification_status=PASS` is only possible when the actual HTTPS request,
  TLS verification, HTTP status, and response contract all pass.
- `redaction_result=NOT_RUN` remains explicit because the response body is never
  emitted to logs/reports and no separate live redaction audit has been performed.
- Any failure must be recorded as `FAIL` with a concrete stop reason.

## Forbidden without separate authorization

Creating the dedicated test mailbox, sending a test message, deleting a test
resource, any provider write, GHCR publishing, release/tag creation, and production
deployment each require separate explicit approval. An `OUTCOME_UNKNOWN` external
operation must stop for manual reconciliation; never blindly retry it.
