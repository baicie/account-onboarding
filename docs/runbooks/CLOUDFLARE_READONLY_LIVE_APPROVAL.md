# Cloudflare Read-Only Live Approval Form

Status values are exact and must not be inferred from tooling:

- `NOT_AUTHORIZED` — default; no real request may be made.
- `AUTHORIZED_READONLY` — operator signed this form with the exact scope below.
- `EXECUTED_READONLY` — the approved request actually ran and evidence was archived.

A tool existing, an authorization file existing, or a Mock PASS is **not**
authorization and is not execution. The agent must never create this approval on
the operator's behalf.

## Required fields

| Field | Value |
|---|---|
| Environment | staging/private test instance only |
| Exact Host | `________________` (no wildcard) |
| Test mailbox reference | `________________` (reference only; no full address in evidence) |
| Address token source | protected file/secret, 0400/0600, never argv/chat |
| Allowed method/path | `GET /api/parsed_mails` only |
| Allowed query | `limit=1&offset=0` only |
| Token permissions | read-only mailbox parse/list; no create/send/delete/DNS/Worker/D1 |
| Window (UTC) | start `__` / end `__` |
| Request budget | `__` (1..3 recommended) |
| Time budget | `__` seconds (<=60 recommended) |
| TLS | verify certificate and hostname; no `--insecure` |
| Redirects | forbidden |
| Stop conditions | 3xx/401/403/429/5xx/TLS/timeout/oversize/malformed/schema |
| Evidence redaction | no token, full address, JWT, OAuth state, message bodies |
| Review method | operator verifies report fields and artifact digest |
| Remote side effects | none expected; read-only GET |
| Approver / stop authority | `________________` |

## Execution evidence to return

`authorized_scope`, `request_method`, `request_path`, `request_budget`,
`requests_used`, `target_host`, `start_utc`, `end_utc`, `tls_result`,
`auth_result`, `readonly_contract_result`, `response_schema_valid`,
`redaction_result`, `stop_reason`, `cleanup_result`, `verification_status`,
`evidence_hash`.

`redaction_result` remains `NOT_RUN` unless an actual redaction check was
performed on collected evidence. `verification_status=PASS` is allowed only for
`EXECUTED_READONLY`.

## Stop flow

Stop the run and archive evidence; do not retry blindly if the remote outcome is
unknown. Any write-like operation or unexpected mailbox data must be reported,
not cleaned up by the agent. No GHCR/Tag/Release/Production action is part of
this approval.
