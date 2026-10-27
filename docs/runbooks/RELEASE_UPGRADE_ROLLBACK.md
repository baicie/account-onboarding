# Release / Upgrade / Rollback Runbook (R9)

Scope: controlled release candidate, staging/production-shaped upgrade, binary
rollback, database restore, and external-operation reconciliation. R9 has NOT
published an image or touched production.

## 1. Release candidate

1. Candidate workflow `release-image` runs `make verify`, full `go test ./... -race`,
   targeted external-operation tests, Docker build, SBOM/govulncheck/Trivy/
   ShellCheck, image/commit consistency, and uploads `candidate/` with
   `image.tar`, `candidate-provenance.json`, `supply/metadata.json`.
2. Treat the candidate artifact as immutable input to approval. The Publish job
   is separate and requires:
   - explicit `publish=true`, a semantic-version tag (`vX.Y.Z`),
   - an allowed ref (protected tag or `main`; never an unreviewed feature branch),
   - the protected `release` Environment approval,
   - candidate commit/image hash verification.
3. Only the Publish job holds `packages: write`. It pushes by digest, signs and
   attests the pushed digest, and emits `publish-evidence/rollback.txt` containing
   the immutable digest. That digest (not a mutable tag) is the rollback target.
4. No R9 evidence claims GHCR publish; publish was skipped by design.

## 2. Pre-upgrade

- Record current deployed commit/digest and schema version (`/v1/meta`, `/readyz`).
- Take an encrypted backup and **verify** it (`scripts/backup.sh` + restore verify).
- List external operations:
  `ONBOARD_ADMIN_TOKEN_FILE=... scripts/ops_external.sh list --status OUTCOME_UNKNOWN`
  Resolve or explicitly accept unresolved items before upgrading. Never let an
  unknown remote write retry automatically.
- Confirm the new image's schema max is >= current schema. Start with the new
  image; it migrates and `/readyz` requires schema 5 and the external gate table.

## 3. Upgrade verification

- `/readyz` 200, `/v1/meta` schema 5, image-internal commit matches deployed SHA.
- Existing runs/events/session revocations still readable.
- External operations: no operation left in IN_FLIGHT without an operator plan;
  CONFIRMED operations are not re-executed.

## 4. Rollback decision

Two distinct operations:

| Rollback kind | Allowed when | Command / evidence |
|---|---|---|
| Binary rollback to previous image | Previous binary's max schema >= current DB schema; otherwise it MUST refuse | start old image; `/v1/meta`; R9 `old_image_rollback.sh` proves schema-4 refusal |
| Database restore | Any failure recovery; restore pre-upgrade backup into a fresh volume | `scripts/restore.sh --snapshot ... --target ... --keyring ...`; verify before start |

**Never** run an older binary on a newer schema. R9/R10 uses the durable
`schema_migrations` guard: the old binary exits with a schema-too-new error.
A failed/wrong-key restore must not overwrite the existing valid DB (verified).

A live worker may still be inside an external call even after its lease expires.
The single-process lifecycle barrier marks leftover IN_FLIGHT as
RECONCILIATION_REQUIRED at startup/after graceful Stop; operators must never
resolve IN_FLIGHT directly. The audited resolve path records the credential
subject, operation ID, run ID and before/after versions.

After restoring an older DB, external operations whose audit rows lived only in
the newer DB must be reconstructed from the provider console/audit artifacts;
never assume the restored DB contains the last in-flight attempt.

## 5. External-operation reconciliation

```
# list with filters and stable cursor
ONBOARD_ADMIN_TOKEN_FILE=/secure/admin.token \
  scripts/ops_external.sh list --status OUTCOME_UNKNOWN --limit 50

# resolve one operation (optimistic version; reason mandatory)
ONBOARD_ADMIN_TOKEN_FILE=/secure/admin.token \
  scripts/ops_external.sh resolve --id OP_ID --version 3 \
  --resolution RESOURCE_CONFIRMED --reason "verified mailbox exists" \
  --evidence ticket-123
```

Resolution values: `RESOURCE_CONFIRMED`, `NO_RESOURCE_CREATED`,
`MANUAL_COMPENSATION_COMPLETED`, `UNRESOLVED`. Resolution records an audited
decision only; it does not delete remote resources and does not trigger an
automatic retry. Resolve with the current `version`; repeated/stale resolutions
return 409.

## 6. Post-rollback

- Verify session revocation replay, run/event readability, Provider diagnostics,
  and `/v1/ops/metrics` (admin-only).
- Record the actual image digest/schema and any unresolved external operations in
  the incident log.
- Do not merge/release/deploy as part of a rollback drill.

## Single-command updates (deployment tooling)

`scripts/update_account_onboarding.sh` wraps the whole lifecycle described above on a
single host. It defaults to `--build-only`, so nothing is recreated unless `--deploy` is
explicit. See `scripts/deploy/README.md` for the full flag list and the enforced safety
rules; the short version:

```bash
./scripts/update_account_onboarding.sh --preflight
./scripts/update_account_onboarding.sh --build-only --expected-sha=<sha>
./scripts/update_account_onboarding.sh --deploy
./scripts/update_account_onboarding.sh --status
./scripts/update_account_onboarding.sh --backup-only
./scripts/update_account_onboarding.sh --rollback
```

The script reuses the shipped backup, verify and materializer tooling; it does not
reimplement snapshotting, and it refuses to deploy when the running container keeps its
database in a different directory than the one being deployed (that would look like a
first install and silently start on an empty database).

The sandbox deployment is exercised end to end by `.github/workflows/deployment-tooling.yml`
on every pull request: first install, verified snapshot, second deploy, wrong-SHA refusal,
hand-written override refusal, rollback, and a database integrity check after each step.
