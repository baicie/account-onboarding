# R22 execution state

Checkpoint file for context recovery. It is **not** an acceptance report and it
never claims a gate that has no evidence. Update it in place.

## Baseline

| Item | Value |
| --- | --- |
| Branch | `feat/r22-product-first-final-acceptance` |
| PR | #16 (Draft, base `feat/r21-autonomous-final-qualification`) |
| Session start HEAD | `788197a` |
| HEAD after Phase 0 | `0806243` |
| HEAD after ARM64 work | `c39105f` |
| Base of stack | main `9726fc1` |
| `release_authorized` | **false** |

## Done in this session (with evidence)

| Work | Evidence |
| --- | --- |
| Reproduced the materializer secret-loss bug | `beta` disappeared and exit was 1 when the install rename failed after the backup move (old script, real filesystem) |
| Rewrote the materializer around a journal | `scripts/materialize_secrets.sh`: single failure/signal/crash path, flock on the directory fd, fd-based source validation |
| Real fault injection | `tests/r22/materializer_fault.sh` - CI job `materializer-faults` **success** (run 40458563623): copy/chmod/chown/backup/install faults, SIGTERM, SIGKILL recovery, concurrent writers, missing flock, disk full |
| Docker secret identity | `tests/r22/docker_secret.sh`: digest identity, rotation, interrupted-transaction replay (20/20 assertions passed; cleanup fix in `c39105f`) |
| Kind rotation identity | `tests/r22/kind_secret.sh`: digest before/after, credential accepted/rejected, master key unchanged, symlink-escape negative (4 assertion fixes in `c39105f`) |
| ACL regression | `posix-acl` job success (script unchanged) |
| Merge dry run | `scripts/r22_merge_dryrun.sh`: 16/16 merges, no conflicts, merged tree `9c854ce` identical to the accepted tip tree, `go build` / `go vet` / `go test ./...` exit 0; evidence `docs/acceptance/evidence/r22/merge-dryrun.txt` |
| ARM64 full acceptance (iteration 3, run 40460938065) | 18 checks green: aarch64 runtime, non-root, read-only rootfs, SQLite+WAL in-container, run create (202), external ops, outbox, audit, SIGKILL recovery, run persistence. Open: /app/backups was not mounted into the read-only container, so the in-container backup failed with EROFS (fix pushed) |
| ARM64 full acceptance | `tests/r22/arm64_full.sh` + `tests/r22/Dockerfile.arm64test` + `r22-arm64-full` workflow: in-container migration, WAL, runs, outbox, backup, verify, restore, wrong key, corrupt snapshot, SIGKILL, rollback, cleanup |
| Historical schema5 replay | `tests/r22/historical_backup.sh`: real schema5 source (commit `3796047`) built and run, its own backup tool, current restore + migration |

## Known failing / open items

| Item | State | Next action |
| --- | --- | --- |
| `docker-secret` | run 40458563623 failed at cleanup (20/20 assertions passed; the 0700 target owned by the runtime UID could not be removed by the runner) | fixed in `c39105f`: root-in-container fixture + cleanup with the materializer's privilege; needs a green run |
| `kind-projected-secret` | run 40458563623 failed 16/4 (strict listing equality, no port-forward wait, negative pod without `set -e`) | fixed in `c39105f`; needs a green run |
| `r22-arm64-full` | first run after `c39105f` | read the run, fix the root cause, never lower the bar |
| Running restore / retention | not started | maintenance gate design + retention implementation |
| Trusted BuildKit / SLSA | not started | buildx attestation plus verification against an isolated registry |
| Workflow definition trust | **BLOCKED** | needs a protected ruleset / baseline: repository administrator action |
| Browser advanced matrix | not started this session | session expiry/revocation, read-only role, real-sandbox browser |

## Invariants that must not regress

- `release_authorized=false`; no merge, tag, release, GHCR publish or deploy.
- The service reader stays strict: regular 0400/0600 file, no symlink
  (`internal/config/config.go`). The materializer must not weaken it.
- No credentials in logs or artifacts; only digests.
- A gate is never marked PASS from a job conclusion alone: it needs the asserted scope.
