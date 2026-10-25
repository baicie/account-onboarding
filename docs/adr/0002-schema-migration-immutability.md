# ADR 0002 — Immutable migrations and worker-lifecycle external fencing

Status: accepted (R10)

## Context

R9 changed migration 0004 during its development cycle to add the created_at
index. A database that already applied 0004 would not re-run the altered file.
That violates the migration contract: only append new versions.

R9 also used lease expiry as a proxy for "worker stopped". A lease can expire
while the provider HTTP call is still blocked; expiry does not prove the worker
or remote outcome stopped.

## Decision

1. **Migrations are immutable once applied.** New schema changes append a new
   numbered file. R10 adds `0005_external_operation_lifecycle.sql` with the
   lifecycle/fencing/audit columns and `CREATE INDEX IF NOT EXISTS` for the
   deterministic sort index. Schema 5 is the readiness floor. Migration failure
   leaves the previous version (single transaction); a future-schema database is
   rejected by the old binary.
2. **Worker lifecycle is the fencing barrier, not lease expiry.** The
   single-instance lock establishes that the previous process is dead. On
   startup, PREPARED (the provider call was never started because Mark precedes
   it) becomes FAILED_DEFINITE; IN_FLIGHT becomes RECONCILIATION_REQUIRED.
   After graceful Stop (workers drained), any leftover IN_FLIGHT is likewise
   marked RECONCILIATION_REQUIRED. An operator may only resolve non-IN_FLIGHT
   operations; lease expiry never implies the worker stopped.
3. **Completion is fenced.** CompleteExternalOperation checks run lease owner,
   fencing generation, operation version and current status. A stale/lost lease
   cannot overwrite state: it moves a potentially-sent operation to
   RECONCILIATION_REQUIRED and returns ErrLeaseLost. A late completion after an
   operator resolution is refused.
4. **Resolution is audited identity.** The API passes the authenticated
   credential subject (bearer:admin:sha256:<fingerprint> or cookie:operator) and
   request time to storage. State change and audit row commit atomically; audit
   run_id is the operation's run and detail carries operation_id, from/to status,
   before/after versions and evidence. Reasons/evidence are length/control-char
   validated and redacted.

## Consequences

- A crash/state transition may require an operator instead of automatic retry;
  this is intentional because a remote create may have succeeded.
- Old binaries cannot run schema 5; rollback requires restoring the pre-upgrade
  schema-3 backup or a binary with a max schema >= 5. R10 rollback evidence uses
  the R8 old image and schema 5.
- Early R9 schema-4 fixtures may already contain the index; 0005's IF NOT EXISTS
  is compatible and tests cover empty/3/4 -> 5 plus failed-migration rollback.
