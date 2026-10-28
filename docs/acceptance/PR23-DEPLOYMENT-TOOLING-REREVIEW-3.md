# PR #23 - deployment tooling re-review 3 (independent)

Repo: `/Users/liuzhiwei/Desktop/workspace/git-code/account-onboarding`
Branch: `feat/v0.1-compose-deployment` (HEAD `7c6bd2e`; no branch switch, commit or push; working tree left clean)
PR: #23 (base `main`)
Scope: the two fixes claimed by `7c6bd2e` (P1-A, P1-C) plus a P1-B regression check and a fast regression sweep over `scripts/update_account_onboarding.sh`, `tests/deploy/update_script_test.sh`, `.github/workflows/deployment-tooling.yml` and `deploy/compose.yaml`. The three earlier documents (`PR23-DEPLOYMENT-TOOLING-REVIEW.md`, `...-REREVIEW.md`, `...-REREVIEW-2.md`) were read only and are untouched.

Method: static reading plus my own stub harness under `/tmp/pr23-r3` (independent docker/git/curl/stat/sudo/install/df stubs, a fresh root per probe). The harness simulates only the container runtime, the git remote, HTTP health and the uid-10001 ownership of `runtime/data`; the code under test (state JSON parsing, every rollback/refusal guard, the real SQLite read through `python3`, the compose file writing, the health logic) is the repository script itself. CI run `40628428018` was **not** trusted for any claim; every statement below comes from a probe I ran or a line I read. No Docker daemon was available, so the Docker E2E was assessed statically.

## Verdict summary

| Item | Verdict | One line |
| --- | --- | --- |
| P0 | **0** | no data-safety or fail-open defect found |
| P1-A | **OPEN** | the guard now recognises a failed deploy (probes a/b pass as specified), but the recovery still restores `last_good_image` - one release *before* the image the failed deploy replaced - and is refused outright after a schema-bumping release |
| P1-B | **CLOSED** | both missing-field paths exit 3, an absent state still reports "nothing to roll back" |
| P1-C | **CLOSED** | with no attempt file the guard now exits 3 with a clear message on BSD **and** GNU sed semantics |
| new P1 | none | the remaining P1 is the destination half of P1-A, not a new defect |

Final line: **REVIEW_BLOCKED** (P1-A remains open).

## What 7c6bd2e actually changes (5 script lines)

+ the mismatch branch reads `expected_image_id` from `deployment-attempt.json` instead of `image_id` from `deployment-state.json.pending` (`scripts/update_account_onboarding.sh:642`), + appends `|| true` to that command substitution, + splits the `head -1` into its own assignment (`:643`), and the warning text changed (`:645`). Tests: the failed-switch fixture moved to the attempt format and a missing-attempt case was added (`tests/deploy/update_script_test.sh:196-212`).

## P1-A - OPEN (recognition fixed, recovery target still wrong)

Claim under test: a deploy writes `deployment-attempt.json` before switching (`scripts/update_account_onboarding.sh:610-613`, `488-511`), a failed health check records `failed` and leaves the container on the attempt image (`:615-620`); `--rollback` must accept exactly that mismatch, warn, and return to the last known good image.

**Part 1 - recognition: FIXED.** The branch now reads a file `do_deploy` really writes (`:613`), and both probes behave as specified.

- Probe P-A1 (attempt `expected_image_id` == running image): **PASS** - warning printed, rc 0, snapshot taken, container switched.
- Probe P-A3 (running image is anything else): **PASS** - rc 3, "the running image (...) does not match the recorded current image (...)".

**Part 2 - destination: still wrong.** In the same probe the container ended on `account-onboarding:local-A`, while the failed deploy had replaced `account-onboarding:local-B`:

| Probe | State before | Container before | Result after `--rollback` |
| --- | --- | --- | --- |
| P-A1 | `current_image=B`, `current_image_id=...id-B`, `last_good_image=A`, db schema 6 | C | rc 0, warning, container = **A**, `state.current_image=A` |
| P-A2 | same, but the last good deploy bumped the schema (`previous_schema_version=5`, db schema 6) | C | rc 3 "database schema 6 is newer than the recorded 5 ... not a safe rollback" |

`scripts/update_account_onboarding.sh:652-655` takes `last_good_image`/`last_good_commit` (falling back to `previous_image`/`previous_commit`) and `:657-658` takes `previous_image_id`/`previous_schema_version` from the **state**, i.e. the release *before* the deployment that was still committed. The failed attempt's own file already carries the correct target - `previous_image=B`, `previous_image_id=...id-B`, `previous_schema_version=6` - and `do_reconcile` already reads exactly those fields for the successful case (`:707-713`). Two consequences:

1. When schemas are equal the tool silently goes back two releases instead of one: the operator asked to undo the failed deploy and instead lost the last healthy release (P-A1).
2. When the last healthy release bumped the schema (`previous_schema_version=5`, db 6) the advertised recovery is refused outright and points at a destructive snapshot restore, even though rolling back to `B` would satisfy the guard (P-A2). `do_deploy` prints "to go back: ... --rollback" (`:619`) for exactly this state.

I validated the fix on a **copy** (`/tmp/pr23-r3/patched.sh`; the repository was not modified): when the accepted branch overrides the target with the attempt's `previous_image`/`previous_image_id`/`previous_schema_version`, P-A1 ends on `B` (rc 0), P-A2 becomes rc 0, and the unrelated-image probe still exits 3. The data is present and the corrected behaviour does not loosen the guard.

**Part 3 - the shipped test still cannot fail here.** The case named "a failed switch is recoverable by rollback" (`tests/deploy/update_script_test.sh:196-204`) only greps the warning, which is printed before the refusal. Reproducing that fixture verbatim (probe P-D1) gives **rc 3** with "the recorded state has no schema version for the previous release" - the test prints PASS while the rollback does not happen. The suite is 36/36 green in that state (I ran it: `SUMMARY pass=36 fail=0` / `DEPLOY_TOOLING_TESTS_PASS`).

Verdict **OPEN**, stated precisely so the scope is unambiguous: probes (a) and (b) *as written in the brief* pass, and the reason re-review 2 kept P1-A open (no marker written by `do_deploy`) is fixed. What remains is the recovery destination - and, as a consequence, the schema-guard interaction - which was the other half of the same P1-A finding in re-review 2 and was not touched by this commit.

## P1-C - CLOSED

`scripts/update_account_onboarding.sh:642-643`:

    pending_id="$(sed -n 's/.*"expected_image_id": *"\([^"]*\)".*/\1/p' "$ATTEMPT" 2>/dev/null || true)"
    pending_id="$(printf %s "$pending_id" | head -1)"

The `|| true` binds to the whole `sed | head` pipeline, so `pipefail` can no longer abort the script and the `die ... 3` at `:647` is always reachable.

- Probe P-A4 (mismatch, **no attempt file at all**, macOS/BSD sed which exits 1 on a missing input): **PASS** - rc 3, "the running image (...) does not match the recorded current image (...)".
- Probe P-A4g (same case with a sed wrapper that emulates GNU sed's exit 2 on a missing file, i.e. the CI image): **PASS** - identical rc 3 and message. No silent abort, no stray `set -e` exit.
- The new scenario case (`tests/deploy/update_script_test.sh:210-212`) asserts both the exit code and the message, so it is a real regression guard: the old code aborted with rc 1/2 and no message, which `want` would have rejected.

## P1-B - CLOSED (regression check)

| Probe | State | Result |
| --- | --- | --- |
| P-B1 | state without `current_image_id`, running image different | **PASS** - rc 3, "the recorded state has no current image id; refusing to roll back blindly" (`:637-639`) |
| P-B2 | `current_image_id` present, no `previous_schema_version`, running image == recorded | **PASS** - rc 3, "the recorded state has no schema version for the previous release" (`:660`) |
| P-B3 | no state file | **PASS** - rc 1, "no deployment state; nothing to roll back to" (`:636`) |

Both fail-closed checks survive the new attempt-file read; nothing in the 5-line diff weakens them.

## Regression sweep (fresh probes, no reliance on the repository tests)

| Area | Result | Evidence |
| --- | --- | --- |
| Secret source permissions 0700/0400 | **PASS** | P-R1: a 0755 source file is refused with rc 2 "has mode 755; expected 0400 or 0600" (`:796`). P-R2/P-A1: source dir 0755 -> 0700, 0644 and 0444 files -> 0400, materialized copies 0400 (`:783-799`) |
| Port parameterisation, one published host port | **PASS** | `deploy/compose.yaml:62-65` holds exactly one mapping (`127.0.0.1:${ONBOARD_HOST_PORT:-8788}:8788`); the generated override has no `ports:` block (`:228-244`); `compose()` exports `ONBOARD_HOST_PORT` (`:247`). P-A1: every health probe went to `127.0.0.1:18788` taken from the state (`:630-631`), never to 8788 |
| `ops_run` positional arguments (no `sh -c "$*"`) | **PASS** | Extracted `:385-396` verbatim and captured the host argv: 36 entries, the `-c` script is a constant, 13 positional arguments follow as separate argv entries (spaces, a single quote, `$(touch PWNED)`, `a;b` and an empty string all preserved; no `PWNED` file). Executing that `-c` script under `/bin/sh` with the same positionals reproduced every argument as `ARG[...]` |
| Unreadable data fails closed | **PASS** | P-R3: data dir 000 with a DB and no usable sudo -> `--preflight` rc 3 "is not readable by the current user ... refusing to treat the existing database as absent" (`:772-780`) |
| Restore marker | **PASS** | P-R4: `state.sqlite.restore-in-progress` -> `--preflight` rc 3 "a restore marker is present" (`:320-322`) |
| Schema read via `schema_migrations` | **PASS** | P-R5: DB versions 1,5,6 with recorded 5 -> `--rollback` rc 3 "database schema 6 is newer than the recorded 5" (`:661-667`); `--status` on the same root prints "database schema: 6" (`:265-296`, `:753`) |

## Docker E2E - static assessment

- **Does it assert a real image-id change back to A?** Yes. `:88-98` captures `IMAGE_A` before the second deploy and `IMAGE_B` after it and asserts `A != B`; `:104-115` runs the rollback and asserts `C_ID == IMAGE_A` **and** `C_ID != IMAGE_B`, plus healthz/readyz 200 afterwards. That is a genuine destination assertion, not a self-comparison.
- **Is cleanup asserted?** Yes, `:175-180`: `docker compose -p account-onboarding down --remove-orphans` followed by `test -z "$(docker ps -aq --filter name=account-onboarding)"`. Caveats: the `down` has no `-f` and relies on project-label discovery (it works in compose v2 while the project containers exist, but it is fragile and a `down` failure aborts the step before the assertion), and the filter covers neither images nor networks.
- **Do the status/rollback steps use the configured port?** Yes. The steps at `:73`, `:107`, `:113`, `:147` omit `--host-port`, and `do_rollback`/`do_status` read `host_port` from the state file (`:630-631`, `:726-727`); the first deploy recorded 18788 and the compose file publishes only that port, so a regression to 8788 would fail the `healthz: 200` greps.

**Can the E2E still pass without proving its claim? Yes.** Every rollback in the E2E is the equal-image path: the container runs exactly the image the state records, so the mismatch branch (`:640-648`) is never entered and the missing-attempt path is never exercised. The two failure injections are a wrong SHA before anything changes (`:117-128`) and a hand-written override before a build-only run (`:130-142`); neither fails a health check after a switch, neither creates or removes `deployment-attempt.json`, and `--reconcile` is never run. A green E2E therefore says nothing about P1-A (either half) or P1-C; the only real coverage of the new guard is the scenario case at `tests/...:210-212` (P1-C) plus one grep-only case (P1-A).

Additional static gaps in the same job: the evidence step logs the wrong container name (`:162-167` runs `docker logs account-onboarding`, while the compose container is `account-onboarding-onboarding-1`, and `|| true` hides the error); snapshot verification is asserted only as file existence (`:99-101`, `:160`); the data directory is pre-created with `install -d -o 10001` (`:57`), so `ensure_data_owner` is never exercised; the SQLite header is checked twice (`:77-83`, `:153-160`), not "after each step" as the runbooks claim.

## P0 (blocking)

None.

## P1 (blocking)

1. **P1-A.2 - a failed deploy cannot be recovered to the last healthy release.** `scripts/update_account_onboarding.sh:652-658`: the accepted-mismatch branch still targets the state's `last_good_image`/`previous_*` (the release *before* the deployment that was committed) instead of the attempt's `previous_image`/`previous_image_id`/`previous_schema_version` (which `:707-713` already uses for reconcile). Probes P-A1 (silent two-release downgrade) and P-A2 (recovery refused after a schema-bumping release, although the attempt records a compatible target). The validated patch (use the attempt fields when the mismatch branch was taken) makes P-A1 land on B, makes P-A2 succeed, and keeps P-A3 at rc 3.

## P2 (read or probed this round, not blocking on their own)

1. `tests/deploy/update_script_test.sh:196-204` - "a failed switch is recoverable by rollback" only greps a warning printed before the refusal; the identical fixture actually exits 3 (probe P-D1). The case at `:187` is grep-only as well.
2. `tests/deploy/update_script_test.sh:25-27` - the first `{{.Image}}` case returns `sha256:stubimage` unconditionally, so the later `STUB_CONTAINER_IMAGE` case is unreachable and no scenario can choose a running image independent of the recorded one.
3. `scripts/update_account_onboarding.sh:650` - duplicate `[ -f "$STATE" ]` (dead after the check at `:636`).
4. `scripts/update_account_onboarding.sh:632` - `detect_docker` runs before the state check: with no docker and no state file the result is rc 2 "docker not found" (probe P-X1) instead of "nothing to roll back".
5. `scripts/update_account_onboarding.sh:685` - after a healthy rollback `write_state` is called with empty previous arguments, so `last_good_image`, `previous_image_id` and `previous_schema_version` are blank/0 and a second `--rollback` dies at `:659` (probes P-X2/P-X2b: rc 1 "no previous image recorded").
6. `scripts/update_account_onboarding.sh:640-648` - the guard is status-blind: a **committed** (stale) attempt whose `expected_image_id` equals the running image is accepted as an "unfinished deployment attempt" with a misleading warning (probe P-E8) and then dies later with rc 1. No `status`, `attempt_id` or `operation` cross-check.
7. `scripts/update_account_onboarding.sh:719-724` and `:740-752` - `--status` emits the "recovery: run --rollback" hint only when `deployment-state.json.pending` exists (a file `do_deploy` never writes) and warns only for `mutation_started`; a failed deploy gets a plain "attempt: ... status=failed" line and zero recovery hints (probe P-S1), while `:695` answers "attempt is failed; no reconcile needed" (probe P-S1r) with the candidate still running.
8. `scripts/update_account_onboarding.sh:642` vs `:513` - the guard re-implements `attempt_field()` inline; the helper still lacks the `|| true` guard the inline copy now needs, so the next caller that forgets a `-f` check reintroduces P1-C.
9. `scripts/update_account_onboarding.sh:374-379` - materialized-secret verification still accepts 0600 and checks neither file type, symlink nor owner 10001, while `scripts/deploy/README.md:36` claims 0400 owned by 10001.
10. `scripts/update_account_onboarding.sh:226-227`, `:661-681`, `:592` - dry-run still copies the base compose file before the dry-run return, `--rollback --dry-run` still runs `compose config` plus the four `docker run` visibility/stat probes (only the materializer sits behind `run`), and `--backup-only --dry-run` exits 3 after printing "DRY-RUN would snapshot".
11. `scripts/update_account_onboarding.sh:235`, `:240-241`, `:469` - paths are still interpolated unescaped into YAML/JSON; a `--root` containing a double quote produces invalid state JSON.
12. `scripts/update_account_onboarding.sh:94`, `:314-318` - symlink checks cover only `$ROOT`, `$DATA` and `$DATA/state.sqlite`; `do_backup` (`:588-594`) and `do_rollback` (`:628-687`) never call `check_data_safety`, so a restore marker or an empty DB is not checked on those paths.
13. `scripts/update_account_onboarding.sh:164`, `:734` - the compose plugin is validated with plain `docker compose version` even for a sudo-only `DOCKER` array, and the `do_status` fallback `|| { warn; return 0; }` is dead because `detect_docker` calls `die`.
14. `scripts/update_account_onboarding.sh:508-510` - `write_attempt` still lacks the pre-rename `sync` that `write_state`/`write_pending` have, and no write fsyncs the parent directory.
15. `.github/workflows/deployment-tooling.yml:162-167` - evidence collects `docker logs account-onboarding`; the container is `account-onboarding-onboarding-1`, so the artifact holds a Docker error instead of application logs (hidden by `|| true`).
16. `.github/workflows/deployment-tooling.yml:175-180` - the cleanup `down` has no `-f`; the assertion is only meaningful when `down` succeeds under `set -euo pipefail`.
17. `.github/workflows/deployment-tooling.yml:57` and `tests/deploy/update_script_test.sh:89` - the data directory is pre-created as 10001:0700, so `ensure_data_owner` (`:756-766`) is exercised by neither the fixture nor the E2E.
18. `.github/workflows/deployment-tooling.yml:99-101`, `:153-160` and `:66-68` - the E2E asserts snapshot file existence rather than a verified snapshot, checks the SQLite header at two points only, and passes `--offline` together with a network `--go-proxy`.
19. `scripts/deploy/README.md:34, 36`, `docs/runbooks/DEPLOYMENT.md:32-34`, `docs/runbooks/RELEASE_UPGRADE_ROLLBACK.md:114-116` - the docs still overclaim a verified snapshot for every state change, 0400/owner guarantees, and integrity "after each step".

## Probe results

Harness `/tmp/pr23-r3` (stubs in `bin/`, roots in `out/`; one fresh root per probe; real `python3`/SQLite for the database).

| # | Probe | Result |
| --- | --- | --- |
| P-A1 | state B / last_good A, attempt failed expecting C, container C, equal schemas | **P1-A destination defect** - rc 0, warning, container switched to **A** (attempt recorded previous_image=B) |
| P-A2 | same, but the last good release bumped the schema (state previous_schema_version=5, db 6) | **FAIL for the recovery claim** - rc 3 "database schema 6 is newer than the recorded 5 ... not a safe rollback"; container unchanged on C |
| P-A3 | attempt expecting X, container C | **PASS** - rc 3 "does not match the recorded current image" |
| P-A4 | no attempt file at all, container C (BSD sed) | **PASS (P1-C)** - rc 3 with the clear message |
| P-A4g | same with GNU-sed semantics (missing file exits 2) | **PASS (P1-C)** - rc 3, same message |
| P-B1 | state without current_image_id | **PASS** - rc 3 "no current image id" |
| P-B2 | state without previous_schema_version | **PASS** - rc 3 "no schema version for the previous release" |
| P-B3 | no state file | **PASS** - rc 1 "nothing to roll back to" |
| P-D1 | shipped failed-switch fixture (`tests/...:197-204`) verbatim | **FALSE POSITIVE** - warning printed, run exits 3; the grep-only assertion passes |
| P-E8 | committed (stale) attempt, running image == attempt image | **status-blind guard** - warning printed, then rc 1 "no previous image recorded" |
| P-S1 | `--status` / `--reconcile` on a failed attempt | rc 0 with "attempt: ... status=failed", **0** recovery hints, 0 RECOVERY_REQUIRED warnings; `--reconcile` rc 0 "attempt is failed; no reconcile needed" |
| P-R1 | secret source file 0755 | **PASS** - rc 2 "has mode 755; expected 0400 or 0600" |
| P-R2 | source dir 0755, files 0644/0444, normal rollback | **PASS** - dir 0700, files 0400, materialized 0400, rc 0 |
| P-R3 | data dir 000 + DB, no usable sudo, `--preflight` | **PASS** - rc 3 "not readable by the current user" |
| P-R4 | restore marker, `--preflight` | **PASS** - rc 3 "restore marker is present" |
| P-R5 | schema_migrations 1,5,6 vs recorded 5 | **PASS** - rc 3 "not a safe rollback"; `--status` prints "database schema: 6" |
| P-R6 | `ops_run` argv and container-side execution | **PASS** - 13 positionals preserved exactly, no injection |
| P-R7 | publish/health port parameterisation | **PASS** - one mapping, health probed 18788 from the state, override has no `ports:` |
| P-X1 | `--rollback` with no docker and no state | rc 2 "docker not found" (P2-4) |
| P-X2 | state written by a successful rollback, then `--rollback` again | rc 1 "no previous image recorded" (P2-5) |
| P-A1p | /tmp copy patched to target the attempt's previous image | rc 0, container **B** |
| P-A2p | same patch, schema-bumping case | rc 0, container **B** (guard passes with the attempt's previous_schema_version=6) |
| P-A3p | same patch, unrelated running image | rc 3 (no fail-open introduced) |

Tooling: `bash -n` clean on both scripts; `/opt/homebrew/bin/shellcheck -S warning scripts/update_account_onboarding.sh tests/deploy/update_script_test.sh` -> rc 0; `bash tests/deploy/update_script_test.sh` -> 36 PASS / 0 FAIL / `DEPLOY_TOOLING_TESTS_PASS`. HEAD confirmed `7c6bd2e` and `git status --porcelain` empty after the review.

## Files checked

`scripts/update_account_onboarding.sh` (whole file, 843 lines), `tests/deploy/update_script_test.sh`, `.github/workflows/deployment-tooling.yml`, `deploy/compose.yaml`, `scripts/deploy/README.md`, `docs/runbooks/DEPLOYMENT.md`, `docs/runbooks/RELEASE_UPGRADE_ROLLBACK.md`, and the three earlier review documents (read only).

REVIEW_BLOCKED