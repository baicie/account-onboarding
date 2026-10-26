# R15 Final Engineering Design

Status: implemented incrementally; external phases remain bounded by authorization.

## 1. Modules and dependencies

`cmd/onboardd` -> `internal/config` (strict secret files) -> `internal/storage`
(SQLite/WAL, migrations) -> `internal/workflow` (run/step state machine, external
operation barrier) -> `internal/provider` (Cloudflare/Mailpit/Keycloak/GitHub/
WorkBuddy adapters; fakes for offline tests) -> `internal/api` (auth/CSRF/UI/API)
and `internal/ops` (backup/restore/rotation). `internal/alerts` evaluates local
rules, owns an in-memory queue, a durable SQLite outbox and an opt-in webhook.

## 2. Database migration and recovery compatibility

Schema 6 adds `alert_outbox` and `alert_deliveries` (migration 0006). 0001-0005
remain immutable. Backup/restore includes the new tables; old binaries must
refuse schema 6. New binaries refuse schema >6. The deploy drill now asserts
schema 6 and a schema-7 rejection.

## 3. Alert reliability semantics

- At-least-once delivery with stable `alert_outbox.id`, `delivery.id`, attempt
  number and idempotency key. Exactly-once is not claimed.
- `PENDING -> IN_FLIGHT -> DELIVERED | RETRY_PENDING | DEAD_LETTER`; `RESOLVED`
  can win while a delivery is in flight and suppresses resurrection.
- Lease/fencing on completion; expired leases are reclaimed; retries are
  bounded and backoff scheduled; panic/context cancel become retryable failures.
- `alert_deliveries` is append-only attempt history. In-memory Queue remains the
  fast API-local view; SQLite outbox is durable and survives restart.
- GET endpoints are side-effect free; explicit admin POSTs mutate.

## 4. External-operation idempotency

PREPARED/IN_FLIGHT write-ahead rows, fencing generation/lease/version CAS,
`OUTCOME_UNKNOWN`/`RECONCILIATION_REQUIRED` never auto-retry, only three verified
resolutions close, `UNRESOLVED` remains pending with audit. Recovery barriers
run on start/stop. No distributed exactly-once claim.

## 5. Security and threat model

Threats: leaked credentials, SSRF/webhook exfiltration, CSRF/Origin bypass,
session theft/revocation bypass, IDOR, HTML injection, SQL injection, TOCTOU
secret replacement, replay/duplicate remote writes, supply-chain substitution,
CI write escalation. Controls: owner-only 0400/0600 secret files with
Lstat/Open/fstat SameFile, single secret reader, admin-only APIs, CSRF+Origin,
cookie HMAC revocation, parameterized SQL, html/template escaping, deny redirects,
exact host allowlists, HMAC webhook signing, least-privilege CI job permissions,
same-artifact candidate/publish dependency.

## 6. Secret deployment adaptation

Docker/K8s projections must not weaken the 0400/0600 rule. `scripts/materialize_
secrets.sh` copies read-only projections atomically into a private 0700 target,
chmods 0400, rejects symlinks/writable sources and never logs values. K8s
fixture is illustrative and not executed in this round; ACLs are not inspected.

## 7. Multi-admin session isolation

Each UI admin gets an independent signed cookie/session ID and CSRF token.
Resolution remains a single SQLite transaction with version CAS; the UI race test
uses two independent cookie jars. Logout/revocation is checked before auth.
GET must not mutate state.

## 8. ARM64 test plan

QEMU emulated `linux/arm64` build + real service start, schema init, secret read,
readyz, run create/read, SIGTERM, restart/persistence. Native ARM64 runners are
preferred when available; QEMU is labelled emulated. Full staging/backup on
ARM64 is NOT_RUN.

## 9. Supply-chain provenance

Build once, freeze image.tar; identity gate compares source commit, tar SHA,
loaded image ID, binary commit, SBOM hash and exact Dockerfile base refs/digests.
Same-artifact job pushes the frozen tar to a throwaway registry, pulls by digest,
signs/verifies/attests with strict subject/commit matching. BuildKit/SLSA
provenance is a future control; Dockerfile metadata is not proof of resolver
output. GHCR publish remains disabled/skipped.

## 10. Release and rollback

Candidate is non-publishing by default; publish requires protected environment,
explicit input, green candidate/same-artifact evidence and exact digest linkage.
Rollback uses the pre-upgrade backup and old image; old binaries must fail closed
on schema 6. No production tags/releases in this round.

## 11. Cloudflare live contract blockers

The real Worker contract is not authorized. Shared valid/invalid fixtures are
compatibility assumptions. Live read-only requires a signed approval form with
exact host/path/query/window/budget/token scope; mail creation/sending/deletion
requires a separate Stage B authorization. No agent-generated representative
approval.

## 12. Acceptance evidence mapping

`docs/acceptance/R15.md`, `docs/acceptance/REVIEW-R15.md`,
`docs/acceptance/evidence/r15-*`, CI runs bound to the final SHA. Historical
NOT_RUN evidence is never rewritten as PASS.

## R14.1 P3 disposition

| # | R14.1 P3 | R15 disposition |
|---|---|---|
| 1 | docs/PR draft | docs added; PR stays Draft |
| 2 | Authorized Live NOT_RUN | unchanged, explicit blocker |
| 3 | queue local/notifier | durable outbox + opt-in webhook; production transport remains disabled/NOT_RUN |
| 4 | expired revocation created post-restore | fixed: created before backup and asserted expired after restore |
| 5 | two-admin same cookie | fixed: two independent cookie jars/CSRF sessions |
| 6 | secret exact/all-provider/ACL | exact reader tests retained; materializer + fixtures added; Docker/ACL/K8s NOT_RUN |
| 7 | base digest metadata vs layer provenance | exact Dockerfile/provenance comparison + tamper probes; SLSA resolver provenance NOT_RUN |
| 8 | candidate/general dry-run duplicate, publish not run | same-artifact job exists; publish remains NOT_RUN; arm64 full staging NOT_RUN |
