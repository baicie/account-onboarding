# PR #23 - Deployment tooling re-review 2 (independent)

Repo: `/Users/liuzhiwei/Desktop/workspace/git-code/account-onboarding`
Branch: `feat/v0.1-compose-deployment`
HEAD reviewed: `129cf2fd524152796f7abc76dae911f2a4fffdc5` (worktree clean; no branch switch, commit or push performed)
PR: #23 (base `main`)
Scope: the two fixes claimed by `129cf2f`, plus regression checks on `scripts/update_account_onboarding.sh`, `tests/deploy/update_script_test.sh`, `.github/workflows/deployment-tooling.yml`, `deploy/compose.yaml` and the runbook/README claims. The earlier reviews `PR23-DEPLOYMENT-TOOLING-REVIEW.md` and `PR23-DEPLOYMENT-TOOLING-REREVIEW.md` were read only and not modified.

Method: static review; `bash -n`; `/opt/homebrew/bin/shellcheck -S warning`; `bash tests/deploy/update_script_test.sh` (35 PASS, 0 FAIL); an independent stub harness under `/tmp/pr23-rereview2` (docker/git/sudo/install/stat/curl stubs, roots under `/tmp/pr23-rereview2/out`); and inspection of CI run `40626214371` (conclusion `success`, SHA `129cf2f`) and its `deployment-e2e-evidence` artifact via `gh`. No Docker daemon was available locally, so the Docker E2E was assessed statically and against the CI logs/artifact.

## Verdict summary

- P0: **0**.
- P1-A (failed switch recoverable): **OPEN**. The narrow `pending.image_id == running image` predicate works, but a failed **deploy** never writes `deployment-state.json.pending`; the advertised recovery still fails. The accepted branch also targets `last_good_image` (one release before the state's current image), and a new `set -e`/`pipefail` bug makes the no-pending mismatch exit non-3 silently instead of 3 (1 on BSD sed, 2 on GNU sed).
- P1-B (guards fail closed): **CLOSED**. Both missing-field paths now exit 3; an absent state still reports "nothing to roll back" (exit 1).
- New P1: **P1-C** - rollback with a running-image mismatch and no pending marker exits non-3 with no diagnostic (1 on BSD sed, 2 on GNU sed) because the `sed` on the missing `$STATE.pending` aborts under `set -euo pipefail` (`scripts/update_account_onboarding.sh:642`). This is the real failed-deploy path.
- Final line: **REVIEW_BLOCKED**, because P1-A remains open and P1-C is a new silent-failure regression.

## P1-A - failed deploy is still not recoverable

Claim in `129cf2f`: when a health check fails the container runs the candidate while state records the last known good image; `--rollback` now accepts exactly that mismatch when the running image equals the `image_id` in `deployment-state.json.pending`, warns, and returns to `last_good_image`; `--status` prints a recovery instruction.

What the code actually does:

- `scripts/update_account_onboarding.sh:613-620` (`do_deploy`): on a failed health check it writes `deployment-attempt.json` with status `failed` and exits 3. It never writes `$STATE.pending`.
- `scripts/update_account_onboarding.sh:680` is the **only** caller of `write_pending`; it is inside `do_rollback`. `write_pending` was introduced only for the rollback candidate (see also `git show ba43f70` and `git show 542d37a`). Therefore after a failed `--deploy` there is no pending marker for the new guard to match.
- `scripts/update_account_onboarding.sh:640-648`: the mismatch branch reads `$STATE.pending` with `sed`. If that file is absent, `sed` exits 1 and, because the script starts with `set -euo pipefail` (`:13`), the command substitution aborts the whole script before the `die ... 3` at `:646`. Probe B2/C below shows rc 1 and no `ERROR` line.
- `scripts/update_account_onboarding.sh:651-655`: even when the pending predicate matches, the rollback target is `last_good_image`, which `write_state` sets to `prev_image` at `:472`. After a successful A -> B deploy, state is `current_image=B`, `last_good_image=A`. A failed C deploy leaves exactly that state plus a C container. The correct recovery target is B (the last committed, healthy image); the code targets A and skips B. The warning at `:644` says "returning to the last known good image" but it is one generation too far back.
- `scripts/update_account_onboarding.sh:718-723`: `--status` prints a recovery instruction only when `$STATE.pending` exists. A failed deploy has no pending file, so the operator gets no recovery instruction. `:739-750` also only warns for `mutation_started`; a `failed` attempt gets only the raw `attempt: ... status=failed` line.

Probe evidence (full A -> B -> C-health-failure sequence, independent harness):

- A and B deployed successfully; B state: `current_image=...-0-1-1-e2e`, `current_image_id=sha256:image-...0_1_1_e2e`, `last_good_image=...-0-1-0`.
- C deploy used `STUB_HTTP=503`; `do_deploy` exited 3 and left the container on the C image, `deployment-state.json.pending` absent, attempt status `failed`.
- `--status` exited 0 but printed no `recovery:` line and no `RECOVERY_REQUIRED`/`INCOMPLETE_ATTEMPT` warning.
- `--rollback` exited **1** with only the action banner, no `ERROR` line. It did not reach the intended mismatch refusal (exit 3).

The narrow predicate does work when a pending file is hand-created:

- Probe A: state `current_image=B`/`current_image_id=sha256:img-b`, `last_good_image=A`, pending `image_id=sha256:img-candidate`, running `sha256:img-candidate`. `--rollback` exited 0, warned "the running container is the pending candidate", took a snapshot and rolled back. But the final state was `current_image=A`, not B. This is a retry of a failed rollback to A, not recovery from a failed deploy.
- Probe B: same state, pending present, running `sha256:img-something-else` -> rc 3, "the running image ... does not match the recorded current image". The narrow (b) case passes.
- Probe B2: same state, no pending file, running `sha256:img-other` -> rc 1, zero `ERROR` lines (P1-C).
- Probe D (the shipped test fixture at `tests/deploy/update_script_test.sh:197-204`): the test's grep for "pending candidate" passes, but the actual rollback exits 3 at `:659` because the fixture provides `last_good_schema_version` and no `previous_schema_version`. This is a false-positive test: it only greps a warning that is printed before the later refusal; it never asserts rc 0 or a switched container.

Conclusion: the commit fixes a failed-rollback retry, not the P1-A failed-deploy scenario. **P1-A: OPEN.** Suggested fix: write a pending record from `do_deploy` before `compose up` (or use `deployment-attempt.json`), target the attempt's `previous_image`/state `current_image` (B) on a failed deploy, not `last_good_image` (A); make `--status` warn on `status=failed`; guard the pending read with `[ -f "$STATE.pending" ]` or `|| true`; and make the test assert rc 0 plus the container image.

## P1-B - rollback guards now fail closed

Verified in code and with probes:

| Probe | State | Command | Result |
| --- | --- | --- | --- |
| E | no `current_image_id`, running `sha256:img-different` | `--rollback --dry-run` | **PASS** - rc 3, "the recorded state has no current image id; refusing to roll back blindly" (`scripts/update_account_onboarding.sh:637-639`) |
| F | `current_image_id` matches, no `previous_schema_version` | `--rollback --dry-run` | **PASS** - rc 3, "the recorded state has no schema version for the previous release" (`:657-659`) |
| G | state file absent | `--rollback` | **PASS** - rc 1, "no deployment state; nothing to roll back to" (`:636`) |

The new state-existence check at `:636` and the two emptiness checks at `:637-639`/`:659` are fail-closed. Note two residual P2s: `detect_docker` runs at `:632` before the state check, so a host without Docker reports a Docker error (rc 2) rather than "nothing to roll back"; and the `[ -f "$STATE" ]` check at `:649` is a duplicate/dead check.

## P1-C (new) - missing pending file turns a refusal into a silent rc 1

`scripts/update_account_onboarding.sh:642`:

```sh
pending_id="$(sed -n 's/.*"image_id": *"\([^"]*\)".*/\1/p' "$STATE.pending" 2>/dev/null | head -1)"
```

The script runs with `set -euo pipefail` (`:13`). When `$STATE.pending` does not exist, `sed` exits 1 on macOS BSD sed (2 on GNU sed); `pipefail` makes the pipeline fail; the assignment fails and `set -e` terminates the script before the `die ... 3` at `:646`. Probe B2 and probe C both show rc 1 with no `ERROR` line locally (GNU sed would return 2). This is the exact failed-deploy state (no pending marker), so the operator sees neither a refusal nor a diagnostic. It also violates the task's probe (b) requirement of exit 3 for a running image that is "something else entirely" whenever there is no pending file. Fix: `if [ -f "$STATE.pending" ]; then pending_id=...; else pending_id=""; fi`, or append `|| true` to the command substitution.

## Regression checks

| Area | Result | Evidence |
| --- | --- | --- |
| Secret source permissions | **PASS** - no regression | `scripts/update_account_onboarding.sh:782-798` sets the source dir 0700 and tightens 0444/0644 to 0400. Probe A started with 0444/0444 and ended with 0400/0400; source dir 0700. `prepare_deploy` calls `harden_secret_source` at `:802`. |
| Port parameterisation | **PASS** - no regression | `deploy/compose.yaml:65` is `127.0.0.1:${ONBOARD_HOST_PORT:-8788}:8788`; `write_overrides` (`:222-245`) emits no `ports:` block; `compose()` exports `ONBOARD_HOST_PORT` at `:247`. Probe H4: `--status` with state `host_port=18788` probed `127.0.0.1:18788/healthz` and `/readyz`. Probe A: rollback health checks also hit 18788. |
| `ops_run` positional arguments | **PASS** - unchanged | `:392-395` still passes `"$tool" "$@"` positionally inside a single-quoted `-c` script. A focused argv capture preserved spaces, quotes and semicolons as separate argv entries. `129cf2f` does not touch this block. |
| Unreadable data fail-closed | **PASS** - no regression | Probe H1: 000 data dir with a DB -> `--preflight` rc 3, "not readable by the current user ... refusing to treat the existing database as absent" (`:767-780`). |
| Restore marker | **PASS** - no regression | Probe H2: `state.sqlite.restore-in-progress` -> `--preflight` rc 3, "restore marker is present" (`:320-322`); scenario tests at `tests/deploy/update_script_test.sh:110,121,122` pass. |
| Schema read via `schema_migrations` | **PASS** - no regression | Probe H3: DB with `schema_migrations` versions 1,5,6 and state `previous_schema_version=5` -> `--rollback --dry-run` rc 3, "database schema 6 is newer than the recorded 5" (`:660-666`); `sqlite_user_version` reads `MAX(version)` at `:265-296`. |

## Docker E2E assessment (static plus CI run 40626214371)

- **Rollback asserts a real image-ID change back to A: yes.** `.github/workflows/deployment-tooling.yml:88-98` captures `IMAGE_A` before the second deploy and `IMAGE_B` after it, and asserts `A != B`; `:104-115` asserts `C_ID == IMAGE_A` and `C_ID != IMAGE_B`. The CI log shows `IMAGE_A=sha256:cae2327e...` and `IMAGE_B=sha256:43af7631...`, and the post-rollback inspect matched A. This is meaningful, not self-comparison.
- **Status/rollback use the configured port: yes.** The workflow omits `--host-port` on the status/rollback steps (`:73`, `:107`, `:113`), but both actions read `host_port` from the state file (`scripts/update_account_onboarding.sh:630-631` and `:725-726`), and the first deploy recorded 18788. CI's status step saw healthz/readyz 200; with 8788 unbound on the compose file that would fail. Probe H4 confirms the URL directly.
- **Cleanup is asserted: yes, and the actual run reached it.** `:180` asserts `test -z "$(docker ps -aq --filter name=account-onboarding)"`. Contrary to the earlier re-review's P2-9, the CI log shows `docker compose -p account-onboarding down --remove-orphans` at `:179` removing `account-onboarding-onboarding-1` and the network, then the assertion passed. The command still has no `-f`, so it is fragile (it relies on project-label discovery and the repo root has no default compose file), but it is not a current failure.
- **Evidence log is wrong.** `:162-167` runs `docker logs --tail 40 account-onboarding`; the actual container is `account-onboarding-onboarding-1`. The downloaded CI artifact contains `Error response from daemon: No such container: account-onboarding` instead of application logs. This is P2.
- **The E2E does not exercise either P1 fix.** The two failure injections are a wrong SHA before any switch (`:117-128`) and a hand-written override before a build-only (`:130-142`). Neither fails a health check after `compose up`, and neither creates a pending marker or a legacy/incomplete state. A green E2E therefore says nothing about P1-A or P1-B. The only new evidence is the scenario suite's two grep-only cases (`tests/deploy/update_script_test.sh:197-207`), which pass while the rollback actually exits 3.
- **Snapshot verification is not asserted.** The E2E only checks `*.enc` and `*.manifest.json` existence (`:99-101`, `:160`), not a "verified snapshot" log; this remains the pre-existing P2 against `scripts/deploy/README.md:34` and the runbooks.
- **First install bypasses the ownership path.** `:57` pre-creates `runtime/data` with `sudo install -d -o 10001`, so `ensure_data_owner` (`scripts/update_account_onboarding.sh:755-765`) is not exercised by the E2E; pre-existing P2.
- **Database integrity claim is overbroad.** SQLite header checks are at `:77-83` and `:153-160`, not after each step as the runbooks claim; pre-existing P2.

## P2 findings (current)

1. `tests/deploy/update_script_test.sh:187` and `:197-204` - both rollback-success tests only grep a log line; they do not assert rc, state, or the container image. Probe D/I show both scenarios actually exit 3 at the schema guard. The `:197` fixture also uses `last_good_schema_version` while `do_rollback` reads `previous_schema_version`.
2. `tests/deploy/update_script_test.sh:25-27` - the first `*"{{.Image}}"*` case returns `sha256:stubimage` unconditionally, making the later `STUB_CONTAINER_IMAGE` case unreachable; the suite cannot simulate a running-image mismatch.
3. `scripts/update_account_onboarding.sh:649` - duplicate `[ -f "$STATE" ]` check; dead code after the check at `:636`.
4. `scripts/update_account_onboarding.sh:632` - `detect_docker` before the state-existence check; an absent state on a host without Docker exits 2 instead of reporting "nothing to roll back".
5. `scripts/update_account_onboarding.sh:684` - if `dk image inspect` fails after a healthy rollback, state records `current_image_id="unknown"`; every later `--rollback` then hits the P1-C silent rc 1.
6. `scripts/update_account_onboarding.sh:613-620` + `docs/deployment/R26_DESIGN.md:9` - the design says "compose up -> inspect image -> health", but `do_deploy` never inspects the running container; state records the built image ID, not the container's actual image.
7. `scripts/update_account_onboarding.sh:374-379` - materialized-secret verification still accepts 0600 and does not check regular-file type, symlink, or owner 10001, while `scripts/deploy/README.md:36` claims 0400 owned by 10001.
8. `scripts/update_account_onboarding.sh:226-227`, `:661-668`, `:592` - dry-run still rewrites `deploy/compose.base.yml`, `--rollback --dry-run` still runs `docker compose config` plus four `docker run` probes, and `--backup-only --dry-run` still exits 3 after printing "DRY-RUN would snapshot".
9. `scripts/update_account_onboarding.sh:235,240-241,469` - paths are still interpolated unescaped into YAML/JSON; a `--root` containing a double quote produces invalid state JSON.
10. `scripts/update_account_onboarding.sh:94,314-318,588-593,628-675` - symlink/restore-marker coverage is still partial; `do_backup` and `do_rollback` do not call `check_data_safety`.
11. `scripts/update_account_onboarding.sh:164,717` - `detect_docker` validates the compose plugin with plain `docker compose version` even for a sudo-only `DOCKER` array; the `do_status` `|| { warn; return 0; }` fallback is unreachable because `detect_docker` calls `die`.
12. `.github/workflows/deployment-tooling.yml:162-167` - evidence uses the wrong container name `account-onboarding`; the downloaded artifact contains a Docker error instead of app logs.
13. `.github/workflows/deployment-tooling.yml:179-180` - cleanup `down` has no `-f`; the actual CI run happened to work via project labels, but it is fragile and the assertion is only meaningful if `down` succeeds.
14. `tests/deploy/update_script_test.sh:89` and `.github/workflows/deployment-tooling.yml:57` - the fixture/E2E pre-create the data directory; the fresh-host ownership path (`ensure_data_owner`) is never exercised.
15. `scripts/update_account_onboarding.sh:508-514` - `write_attempt` lacks the pre-rename `sync` that `write_state`/`write_pending` have; none of the writes fsyncs the parent directory.
16. `scripts/deploy/README.md:34,36` and `docs/runbooks/DEPLOYMENT.md:32-34` / `docs/runbooks/RELEASE_UPGRADE_ROLLBACK.md:114-116` - docs still overclaim a verified snapshot for every state change, 0400/owner guarantees, and database integrity "after each step".

## Probe results

Harness: `/tmp/pr23-rereview2` (stubs under `/tmp/pr23-rereview2/bin`, roots under `/tmp/pr23-rereview2/out`; independent of the repo test stubs). Each probe used a fresh root and a real SQLite DB with `schema_migrations`.

| # | Probe | Result |
| --- | --- | --- |
| A | Pending `image_id` == running image; realistic state `current_image=B`, `last_good_image=A` | **PASS (narrow guard only)** - rc 0, warning printed, snapshot+rollback completed, but final state is A, not B; pending removed; secret source 0444 -> 0400; health checks on 18788 |
| B | Pending present, running image unrelated | **PASS** - rc 3, "does not match the recorded current image"; no switch |
| B2 | No pending file, running image unrelated | **FAIL** - rc 1 locally (GNU sed 2), zero `ERROR` lines (P1-C) |
| C | Full A -> B deploy, C deploy fails health | **FAIL** - state still B, container C, pending absent, attempt `failed`; `--status` rc 0 with no recovery warning; `--rollback` rc 1 locally (GNU sed 2) with no error (P1-A + P1-C) |
| D | Shipped failed-switch fixture (`:197-204`) | **FAIL (test false positive)** - grep "pending candidate" passes, actual rollback rc 3, "no schema version for the previous release" |
| E | State without `current_image_id` | **PASS** - rc 3, "no current image id" |
| F | State without `previous_schema_version` | **PASS** - rc 3, "no schema version for the previous release" |
| G | State file absent | **PASS** - rc 1, "nothing to roll back to" |
| H1 | 000 data dir with DB, `--preflight` | **PASS** - rc 3, "not readable by the current user" |
| H2 | Restore marker, `--preflight` | **PASS** - rc 3, "restore marker is present" |
| H3 | DB schema 6, state previous schema 5 | **PASS** - rc 3, "not a safe rollback" |
| H4 | `--status` with state `host_port=18788` | **PASS** - healthz/readyz requested on 18788 |
| I | Shipped `last_good_image` fixture (`:179-187`) | **FAIL (test false positive)** - grep passes, actual rollback rc 3 at the schema guard |

Tooling results: `bash -n` clean; `shellcheck -S warning` clean (rc 0); `bash tests/deploy/update_script_test.sh` -> 35 PASS / 0 FAIL / `DEPLOY_TOOLING_TESTS_PASS`. CI run `40626214371` -> conclusion `success` on `129cf2f`.

## Bottom line

The fail-closed state guards (P1-B) are genuinely fixed. The failed-switch recovery claim (P1-A) is not: `do_deploy` never creates the pending marker the new branch reads, the accepted branch would roll back one release too far, and the missing-marker path now aborts with a silent rc 1. Because the scenario suite's new tests only grep a line that is printed before the real failure, all 35 tests and the real Docker E2E can be green without proving either fix; the E2E does not perform a health-failing deploy at all.

REVIEW_BLOCKED
