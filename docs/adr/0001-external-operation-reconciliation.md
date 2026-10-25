# ADR 0001 — External operation audit and reconciliation (event-log reuse)

- Status: accepted (R8)
- Date: 2026-10-25
- Context: Cloudflare mailbox creation is a remote write. The HTTP response and
  the local SQLite secret/step transaction cannot participate in one atomic
  transaction. A crash or lost response therefore leaves a real ambiguity:
  the remote resource may exist even though the local step did not commit.

## Decision

Reuse the existing append-only `events` table instead of adding a second mutable
`external_operations` table. Each remote mailbox create writes:

| Event | When | Meaning |
|---|---|---|
| `EXTERNAL_OPERATION_PREPARED` | before the provider call, in its own durable event | intent recorded; calling the provider is allowed only after this succeeds |
| `EXTERNAL_OPERATION_CONFIRMED` | as an event committed atomically with the successful step (`Outcome.Events`) | provider returned success and the local step commit succeeded |
| `EXTERNAL_OPERATION_OUTCOME_UNKNOWN` | timeout / 5xx / transport error after the write started | remote side effect may or may not exist; operator reconciliation required |
| `EXTERNAL_OPERATION_RECONCILIATION_REQUIRED` | duplicate/conflict or other ambiguous provider outcome | do not create a second address; reconcile manually |
| `EXTERNAL_OPERATION_FAILED` | deterministic pre-write/validation/auth failures | no remote side effect expected (a cancel after PREPARED is treated as OUTCOME_UNKNOWN, not FAILED) |

Each event payload contains the non-secret fields `operation_key`
(`run:step:v1`), `provider`, `operation_type`, a truncated SHA-256 request
fingerprint, `status`, `attempt` and (on failure) `error_class`. The event payload
allowlist in `internal/security/redact.go` permits exactly these fields; values
are redacted and length-bounded. No tokens, JWTs, passwords, addresses or full
provider responses are stored.

The latest event per operation key is the displayed status, but review is
**sticky**: any historical OUTCOME_UNKNOWN/RECONCILIATION_REQUIRED keeps
`needs_reconciliation=true` until an operator resolves it, even if a later retry
ends in FAILED. A later CONFIRMED (successful step commit) resolves the
operation. A cancellation after the pre-call PREPARED audit is mapped to
OUTCOME_UNKNOWN because the POST may have reached the provider.

The latest event per `operation_key` is the reconciliation state
(`Store.ListExternalOperations`). `GET /v1/ops/external` is admin-only and shows
the items plus counts (`needs_reconciliation`). Diagnostics never include secret
material.

## Why not a new table

- The event log is already durable, append-only, transactionally coupled to the
  run/step, retained with the run and redacted by a strict allowlist.
- A mutable table would add a second source of truth and a second write path
  around the existing step commit; keeping it correct under crash/restart would
  require additional recovery logic.
- Reusing events keeps the state machine unchanged and avoids a schema change
  (and its rollback/upgrade burden) for audit-only data.

## Consequences and limits

- There is **no exactly-once guarantee** for remote creates. The service refuses
  to create a second address on a duplicate response (fail-closed) and requires
  operator reconciliation for unknown outcomes.
- `PREPARED` without a later event after a crash/restart is a stale intent and is
  surfaced as needing review; it is not automatically converted to success or
  deletion.
- No automatic deletion of remote resources is performed; only an operator may
  reconcile/delete after confirming ownership.
- The provider adapter must remain idempotency-aware where possible; the event
  log is audit and decision support, not a distributed transaction.
- The engine refuses to call the provider if the pre-call audit write fails
  (fail-closed).

## Tests

- `TestExternalOperationConfirmedOnSuccess`
- `TestExternalOperationTimeoutRequiresReconciliation`
- `TestExternalOperationDuplicateNeedsOperatorReconciliation`
- `TestExternalOperationReviewIsSticky` (ambiguous history survives later FAILED)
- `TestExternalOperationCancellationIsUnknown` (cancel after PREPARED)
- `TestExternalOperationDiagnosticsAdminOnly`
- Provider duplicate fail-closed tests remain in the Cloudflare package.
