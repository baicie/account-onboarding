# ADR 0003 — UNRESOLVED is a review, not a closure

Status: accepted (R11)

`external_operations` has terminal `RESOLVED` and pending `OUTCOME_UNKNOWN` /
`RECONCILIATION_REQUIRED` states. A human decision may be:

- RESOURCE_CONFIRMED — verified remote resource exists → close RESOLVED.
- NO_RESOURCE_CREATED — verified nothing exists → close RESOLVED.
- MANUAL_COMPENSATION_COMPLETED — manual compensation done → close RESOLVED.
- UNRESOLVED — reviewed but not yet verified → **do not close**.

R11 keeps the pending status, increments the optimistic version and writes an
atomic `external_operation.review` audit row (actor, reason, evidence, from/to,
versions, requested/reviewed timestamps). The operation remains in the
reconciliation queue/global count and cannot trigger a provider retry. This
prevents an unresolved operation from disappearing while preserving the audit
trail. CONFIRMED may be reviewed but cannot be closed as
NO_RESOURCE_CREATED. No schema change was needed: review history is append-only
audit, current status remains authoritative.
