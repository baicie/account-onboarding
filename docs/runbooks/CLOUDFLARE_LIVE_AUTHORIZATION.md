# Cloudflare Live Authorization & Acceptance Runbook (R11)

This is the only approved path from `CONTROLLED_LIVE_TEST_READY` to a limited
Cloudflare mailbox live test. R11 did **not** execute it.

## Authorization required before any request

The operator must approve, in writing, all of:

| Item | Example |
|---|---|
| Environment | staging/private test instance, not production |
| Exact target host | the real Cloudflare Worker host (HTTPS only) |
| Exact test address | one dedicated address, never a user's existing mailbox |
| Operation scope | read-only listing only, or separately approved create/send |
| Credential | address JWT in a protected file/secret; never in argv/chat/logs |
| Window | start/end time and maximum request/time budget |
| Stop authority | who can stop the run and how |
| Cleanup | whether the dedicated test mailbox may be deleted afterwards |

If any item is missing: do not start. Do not treat R11 development approval as
live-write approval.

## Mode selection

```bash
# default: no network, no credential read
scripts/cloudflare_preflight.sh --dry-run --test-address TEST@example.test

# mock: local HTTPS only (CI/dev)
scripts/cloudflare_preflight.sh --mock-live --mock-url https://127.0.0.1:PORT \
  --mock-ca ca.pem --test-address TEST@example.test --token-file jwt.txt

# authorized read-only live (requires R11+ authorization)
ONBOARD_CF_TEST_ADDRESSES='TEST@example.test' \
ONBOARD_CF_LIVE_AUTHORIZATION=/secure/r11-authorization.txt \
scripts/cloudflare_preflight.sh --live --base-url https://REAL_HOST \
  --test-address TEST@example.test --token-file /secure/test-address.jwt \
  --max-requests 3 --max-seconds 60 --report /secure/r11-report.json
```

The authorization file must contain exactly `AUTHORIZED=YES` and
`allowed_host=<hostname>`. The script only GETs the existing provider contract
`/api/parsed_mails`; it cannot create, modify or delete a mailbox, send mail, or
change DNS/Worker/D1/Email Routing.

## Stop conditions

Stop immediately on: redirect, TLS/host mismatch, 401/403, 429, 5xx, timeout,
request budget exhaustion, unexpected response shape, or any sensitive value in
output. A remote-create timeout/cancel must be treated as OUTCOME_UNKNOWN and
reviewed with `scripts/ops_external.sh`/UI; never blindly retry.

## Evidence to archive

`authorized_scope`, `request_budget`, `requests_used`, start/end, `target_host`,
`tls_result`, `auth_result`, `readonly_contract_result`, `redaction_result`,
`stop_reason`, `cleanup_result`, `verification_status`. Without real network
evidence these must remain `NOT_RUN`, never PASS.
