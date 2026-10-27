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
| ARM64 full iterations (real CI) | 1: image+arch ok, service could not write /app/data (bind mount owned by the runner) - fixed. 2: 5 checks, startup still not ready in 78s - wait raised. 3: 18 checks (aarch64, non-root, read-only rootfs, SQLite/WAL, run 202, outbox, audit, SIGKILL recovery, persistence), backup EROFS because /app/backups was not mounted - fixed. 4: 27 checks, four harness bugs (nested manifest field, wrong-key fixture in a 0700 target, rollback exec into a removed container) - fixed in cd3cabc |
| ARM64 full (emulated) | **ARM64_FULL_EMULATED_PASS** - run 40462295245, 31/31 checks, real aarch64 runtime; evidence docs/acceptance/evidence/r22/arm64-full.txt |
| ARM64 full iterations (CI history) | 1: service could not write /app/data (bind mount owned by the runner). 2: startup slower than 78s. 3: 18 checks, backup EROFS (/app/backups not mounted). 4: 27 checks, nested manifest field/dir ownership/exec into a removed container. 5: 28 checks, snap_of read the wrong backup and verified the deliberately corrupted file. 6: pass |
| ARM64 native | **BLOCKED** - no native arm64 runner for this repository; never claimed |
| Historical schema5 replay | `tests/r22/historical_backup.sh`: real schema5 source (commit `3796047`) built and run, its own backup tool, current restore + migration |

| Running restore (Phase 3) | **implemented and locally verified**: maintenance window (workers paused, writes 503 + Retry-After, reads served, readyz 503, in-flight drain, audit, process-local), restore protocol with -keep-old, restart; 3 API tests + tests/r22/running_restore.sh 32/32 checks (wrong key, tampered snapshot, disk-full fail closed) |
| Backup retention (Phase 3) | **implemented and tested**: newest valid + -min-valid floor always survive, delete only when it verifies, pair-consistent delete, atomic JSON audit report; 9 unit tests + E2E retention checks |
| BuildKit SLSA provenance (Phase 4) | **implemented, CI verification pending**: scripts/verify_buildkit_provenance.py reads the registry (index -> attestation manifests -> in-toto statements) and checks subject/builder/invocation/source/platform/SPDX; 15/15 offline negatives; tests/r22/buildkit_provenance.sh builds once with --provenance=mode=max --sbom=true into a throwaway registry, verifies, pulls by digest, then attacks the claim (wrong commit/repo/platform/subject, forged statement, unsigned build, rebuild). TRUSTED_BUILD_VERIFIED is explicitly NOT claimed (no signature issuer verification) |

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
