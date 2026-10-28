# PR #23 - Deployment tooling re-review (independent)

Repo: `/Users/liuzhiwei/Desktop/workspace/git-code/account-onboarding`
Branch: `feat/v0.1-compose-deployment`
HEAD at review: `21ebd11e36703928043de95ac7565358bed9318b` (worktree clean; no commit, push, or branch switch performed)
Assignment SHA: `542d37a`. While this re-review was running, two commits landed on the branch: `3aa4ad6` (verify the recorded previous image id before rollback) and `21ebd11` (R26 design/progress). All findings below refer to the current `21ebd11` worktree. The earlier `542d37a` snapshot did not contain the `previous_image_id` guard; that difference is called out where it matters.
PR: #23 (base `main`)
Scope: `scripts/update_account_onboarding.sh`, `tests/deploy/update_script_test.sh`, `.github/workflows/deployment-tooling.yml`, `scripts/deploy/README.md`, the two runbook additions, and the port parameterisation in `deploy/compose.yaml`.

Method: static review, `bash -n`, `shellcheck -S warning` (clean), `bash tests/deploy/update_script_test.sh` (33 PASS, 0 FAIL), an independent stub harness under `/tmp/pr23-probe` (docker/git/sudo/stat stubs), and static `docker compose config` validation (Compose v5.5.0, no daemon). The Docker E2E was **not** executed (no daemon); it is assessed statically for false positives.

Note on the original review: the file `docs/acceptance/PR23-DEPLOYMENT-TOOLING-REVIEW.md` actually contains **2 P0, 3 P1 and 12 P2** bullets (17 findings), not 10 P2. I reviewed all 12 P2 bullets; the status table below covers all 17.

## Verdict summary

- P0: **2 / 2 CLOSED**.
- P1: **2 / 3 CLOSED**; the remaining state-protocol item is **PARTIAL**. There are **2 current P1 blockers** (one is the residual of that item, one is the moved-up schema guard).
- P2: **2 CLOSED, 5 PARTIAL, 5 OPEN** (of the 12 original bullets), plus several new P2 observations below.
- No new P0 found.
- Final line: **REVIEW_BLOCKED**, because the failed-deploy recovery path is unusable and the rollback identity/schema guards fail open for legacy or incomplete state files.

## Original findings - CLOSED / PARTIAL / OPEN

| ID | Original location | Status | Current evidence |
| --- | --- | --- | --- |
| P0-1 | `scripts/update_account_onboarding.sh:548-572` | **CLOSED** | `ensure_data_owner()` now exists at `:739-749` and is called from `prepare_deploy()` at `:788` (also `:786-787` run `harden_secret_source` and `require_readable_data` first). On a fresh root the function runs `install -d -o 10001 -g 10001 -m 0700 "$DATA"` as root, or `sudo -n install ...`, and otherwise dies with "need root or passwordless sudo ... refusing to start" (`:746-748`). My stubbed baseline deploy reached "data directory ready for uid 10001 (mode 700)"; the no-sudo path is a refusal, not a silent 0755 deploy. |
| P0-2 | `scripts/update_account_onboarding.sh:202-203` | **CLOSED** | `require_readable_data()` at `:751-764` checks the data directory execute bit and each of `state.sqlite`, `-wal`, `-shm`, `restore-in-progress` readability, using `as_root` only as an explicit privileged fallback; it is called by `do_preflight` (`:561`), `do_backup` (`:589`) and `prepare_deploy` (`:787`). Probe 9: a 000 data directory with an existing DB makes `--preflight` exit 3 with "not readable by the current user ... refusing to treat the existing database as absent". The helper `db_file_exists` still conflates unreadable/absent internally, but no safety path reaches it before `require_readable_data` now. |
| P1-1 | `scripts/update_account_onboarding.sh:178` (old) | **CLOSED** | The base now publishes `127.0.0.1:${ONBOARD_HOST_PORT:-8788}:8788` (`deploy/compose.yaml:65`); `write_overrides` no longer emits a `ports:` block (`:222-245`), and `compose()` exports `ONBOARD_HOST_PORT="$HOST_PORT"` (`:247`). `do_status` and `do_rollback` read `host_port` from the state file (`:630-631`, `:709-710`). Static `docker compose config` with `ONBOARD_HOST_PORT=18788` produced exactly one published port: `127.0.0.1:18788 -> 8788`; no 8788 host binding remained. |
| P1-2 | `scripts/deploy/README.md:45` | **CLOSED** | README now documents `runtime/secrets-src` as "0400 source projection, directory 0700" (`:45`); the workflow enforces `chmod 0400` plus a `sudo -u nobody test -r` check (`.github/workflows/deployment-tooling.yml:60-61`); `harden_secret_source()` sets the source dir to 0700 and upgrades 0444/0644 files to 0400, refusing other modes (`:766-781`). Probe 4: a 0444 source was changed to 0400 before the rollback continued (it then failed later on my stub backup, as expected). |
| P1-3 | `scripts/update_account_onboarding.sh:495-502` | **PARTIAL** | The state protocol is materially better: `write_state` records `current_image`, `current_image_id`, `last_good_image/commit/schema` and `previous_*` (`:457-486`); `write_attempt` persists `mutation_started` before `compose up` and `failed` after a health failure (`:610-620`). But state is still written only after health (`:622-624`), so a failed deploy leaves it describing the previous image while the container runs the attempted image. `do_rollback` then refuses because the new running-image guard sees `attempted != current_image_id` (`:636-638`), and `--reconcile` treats a `failed` attempt as "no reconcile needed" (`:683`). The original failure mode is therefore still present in a new form; see P1-A. |
| P2-1 | `scripts/update_account_onboarding.sh:317-320` | **OPEN** | `materialize_secrets()` still verifies only that `/run/secrets/$f` exists via `/bin/ls -l` and that `stat -c %a` is 400 or 600 (`:374-379`). It does not verify regular-file type, non-symlink, owner 10001, or reject 0600; README:36 claims 0400 owned by 10001. |
| P2-2 | `scripts/update_account_onboarding.sh:168,523-524` | **PARTIAL** | `--deploy --dry-run` no longer materializes secrets or writes state/override (the whole block is under `if DRY_RUN != 1` at `:604-608`, and `write_overrides` returns before writing the override at `:227`), but it still does `cp "$APP/deploy/compose.yaml" "$BASE_COMPOSE"` at `:226` **before** the dry-run check. Probe 7a changed `deploy/compose.base.yml` from "OLD BASE" to the copied compose file while state/secrets/override were untouched. `--rollback --dry-run` still calls `compose_valid` and `materialize_secrets` (`:661-668`), which executes `docker compose config` plus four `docker run` visibility/stat probes (probe 7b). `--backup-only --dry-run` still exits 3 at `:592` after printing "DRY-RUN would snapshot" (probe 8). |
| P2-3 | `scripts/update_account_onboarding.sh:333` (old) | **CLOSED** | `ops_run` now invokes `--entrypoint /bin/sh` with a single-quoted `-c` script and `"$tool" "$@"` positionally (`:392-395`). Probe 5 captured the exact argv passed to `dk run`: arguments with spaces, single/double quotes, `$(...)`, semicolons, an empty argument and a newline all arrived as separate argv entries; no `PWNED` file was created. The same argv under `/bin/dash -c ...` preserved `$0=backup` and every positional argument. |
| P2-4 | `scripts/update_account_onboarding.sh:87,256-260` | **PARTIAL** | The script still checks only `$ROOT` itself for symlink (`:94`); `check_data_safety` now rejects `$DATA` and `$DATA/state.sqlite` symlinks (`:314-318`), but not `$APP`, `$SECRETS_SRC`, `$SECRETS`, `$BACKUPS`, `$DEPLOY`, `$LOGS`, `state.sqlite-wal`, or `state.sqlite-shm`. `do_backup` (`:588-593`) and `do_rollback` (`:628-675`) still do not call `check_data_safety`, so a restore marker or empty DB is not checked on those paths; `prepare_deploy` only checks readability/ownership. |
| P2-5 | `scripts/update_account_onboarding.sh:517` | **OPEN (now P1-B)** | The rollback schema guard is still conditional on `-n "$recorded_schema"` (`:646-653`), and `state_num` cannot distinguish an absent key from zero (`:517`). Probe 6: a state file with `last_good_image`, `previous_image_id` and no `previous_schema_version` logs "rolling back to account-onboarding:local-old" and completes `--dry-run` with rc 0. A missing key therefore skips the schema-downgrade guard instead of failing closed. Elevated to P1-B because an old binary can run against a newer schema. |
| P2-6 | `scripts/update_account_onboarding.sh:178,393` | **PARTIAL** | `--host-port` is now validated as digits and 1..65535 (`:82-85`); probe confirmed `abc`, `0`, `70000` all exit 1. But paths are still `printf`'d unescaped into JSON/YAML: `"root": "%s"` (`:469`), `volumes: - %s:/app/data` (`:235`), and secret `file: %s/...` (`:240-241`). Probe 14: `--root='/tmp/.../p14"q'` produced `"root": "/tmp/.../p14"q"` and invalid JSON after a successful `--reconcile`; future `state_field` parsing can then misread state. |
| P2-7 | `scripts/update_account_onboarding.sh:167` (old) | **OPEN** | `write_overrides` still does `cp "$APP/deploy/compose.yaml" "$BASE_COMPOSE"` (`:226`); the base's `build.context: ..` (`deploy/compose.yaml:25`) now resolves to `$ROOT`, not `$APP`. Static `docker compose config` on the generated base+override showed `context: /tmp/pr23-compose-check` and `dockerfile: Dockerfile`, which does not exist under `$ROOT`. `--no-build` on deploy (`:614`) hides this, but a manual `docker compose ... build` against the generated base resolves the wrong context. |
| P2-8 | `scripts/update_account_onboarding.sh:523` (old) | **CLOSED** | `do_rollback` now runs `prepare_deploy`, `take_backup`, and `backup_is_usable \|\| die` before `write_overrides`/`compose up` (`:661-664`). Probe 13 (A -> B -> rollback to A) logged "taking an encrypted snapshot", "verifying snap.enc", "backup ok", and only then switched; the rollback succeeded and cleared `$STATE.pending`. |
| P2-9 | `.github/workflows/deployment-tooling.yml:178` (old) | **OPEN** | Cleanup is still `docker compose -p account-onboarding down --remove-orphans` without `-f` (`:179`), from the repository root, which has no `compose.yaml`/`docker-compose.yaml`; I reproduced `docker compose -p account-onboarding config` failing with "no configuration file provided: not found". The following cleanup assertion (`:180`) is therefore not reached, and the job fails in the cleanup step. Evidence still runs `docker logs account-onboarding` (`:167`), but the Compose container is `account-onboarding-onboarding-1` (no `container_name` in `deploy/compose.yaml`), so the app log is not collected; the `\|\| true` group hides it. |
| P2-10 | `tests/deploy/update_script_test.sh:87,128` (old) | **PARTIAL** | New tests cover pending status, reconcile, last-good rollback and replaced-tag refusal (`:176-194`), but `mkroot` still creates `$ROOT/runtime/data` as the invoking user with 0755 (`:89`); the only non-dry deploy is absent, and line 130 still tests `--deploy --dry-run`. The "rollback targets last_good_image" test at `:187` checks only a grep of the log, not `$RC`, and `do_rollback` logs "rolling back to ..." before `prepare_deploy`/`take_backup`; on a host without passwordless sudo it then exits at `ensure_data_owner`, and with the repo's docker stub it then exits at `take_backup` because the stub creates no `.enc` file. The test passes without proving a successful rollback. The docker stub also has a duplicate, earlier `*"{{.Image}}"*` case at `tests/deploy/update_script_test.sh:25` that returns `sha256:stubimage` unconditionally, making the later `STUB_CONTAINER_IMAGE` case at line 27 unreachable, so no test can simulate a running-image mismatch. |
| P2-11 | `scripts/deploy/README.md:34,36` | **PARTIAL** | README:45 is fixed, but README:36 still claims materialized secrets are 0400 owned by 10001 while the verifier accepts 0600 and does not check owner (`:374-379`); README:34's "every state change is preceded by an encrypted, verified snapshot" is true for deploy/rollback with a DB, but not for first install (no DB) or `--reconcile`. The new runbook claims at `docs/runbooks/DEPLOYMENT.md:32-34` and `docs/runbooks/RELEASE_UPGRADE_ROLLBACK.md:114-116` say the E2E covers a "verified snapshot" and a database integrity check "after each step"; the E2E only checks `*.enc`/`*.manifest.json` existence (`.github/workflows/deployment-tooling.yml:97-102`) and SQLite headers at three points (`:77-83`, `:153-160`), not after each step. The workflow also passes `--offline` with a network `--go-proxy` (`:66-68`), so "offline" still depends on GOPROXY for Docker/ops builds. |
| P2-12 | `scripts/update_account_onboarding.sh:105,539` (old) | **OPEN** | `detect_docker` still validates the compose plugin with plain `docker compose version` (`:164`), even when access was established through `DOCKER=(sudo -n docker)`; a sudo-only host is told the plugin is missing. `do_status` still calls `detect_docker 2>/dev/null \|\| { warn ...; return 0; }` (`:717`), but `detect_docker` calls `die` (which exits the script), so the fallback is unreachable. Probe 10: `--status` with no docker exits 2 after printing "no deployment state recorded"; it does not return 0 with "docker unavailable". |

## Current P1 findings (blocking)

### P1-A - A failed deploy leaves a state that the advertised recovery cannot use

Evidence:
- `scripts/update_account_onboarding.sh:613-620`: `write_attempt mutation_started`, `compose up`, then on `wait_healthy` failure `write_attempt failed` and `exit 3`, **before** `write_state` at `:622-624`. The state file still describes the previous successful image; the container is running the failed attempt's image.
- `scripts/update_account_onboarding.sh:636-638`: `--rollback` compares the running container image with `current_image_id` and dies when they differ. After the failed deploy they always differ (running = attempted image, state = previous image).
- `scripts/update_account_onboarding.sh:683`: `--reconcile` requires `status = mutation_started`; a `failed` attempt takes the "no reconcile needed" branch and returns 0 without changing state.
- `scripts/update_account_onboarding.sh:726-734`: `--status` prints `attempt: ... status=failed` but only warns for `mutation_started`; a failed attempt gets no `RECOVERY_REQUIRED`/`INCOMPLETE_ATTEMPT` warning.
- `scripts/update_account_onboarding.sh:619` explicitly tells the operator: "to go back: $PROG --rollback".

Probe 11 (full stubbed A -> B -> C-health-failure sequence):
- State before C: `current_image = B`, `last_good_image = A`.
- C deploy: `compose up` set container image to C, every health probe returned 503; attempt status = `failed`, state still B.
- `--rollback`: rc 3, "the running image (C) does not match the recorded current image (B); refusing to roll back blindly - run --status and reconcile first".
- `--reconcile`: rc 0, "attempt is failed; no reconcile needed"; state remains B.
- `--status`: no recovery warning; only the raw attempt line.

Impact: the one command the script advertises after a failed health check is guaranteed to refuse in exactly that scenario. Even if the guard were bypassed, `last_good_image` is A, one generation before the failed attempt's previous image B, so a rollback would skip the last known good B. There is no scripted path back to B and no state transition for the failed attempt.

Suggested fix: persist a pending/attempt state that names the attempted image and the previous image, and let `--rollback` target the attempt's `previous_image` (B) when the attempt is failed; or make `--reconcile failed` transition the state to the previous image after explicit confirmation. At minimum, do not print `--rollback` as the recovery command unless it can run, and have `--status` emit `RECOVERY_REQUIRED` for `failed`.

### P1-B - Rollback identity/schema guards fail open for legacy or incomplete state

Evidence:
- `scripts/update_account_onboarding.sh:635-638`: the running-image check is skipped when `current_image_id` is empty. Probe 15: a state with `state_version: 1`, no `current_image_id`, container image `sha256:image-DIFFERENT` vs. state `image_id: sha256:image-current`; `--rollback --dry-run` logs "rolling back to account-onboarding:local-old" and returns 0. This is exactly the pre-R26 state format an existing deployment has after upgrading the script.
- `scripts/update_account_onboarding.sh:646-653`: the schema guard is skipped when `previous_schema_version` is absent. Probe 6: same pattern without that key; rollback proceeds (dry-run rc 0) instead of refusing.
- `scripts/update_account_onboarding.sh:517`: `state_num` returns empty for a missing key and also for a malformed value; there is no `state_version` validation before rollback.

Impact: on a host with an older state file (no `current_image_id`/`previous_schema_version`), the two new rollback guards are silently bypassed. The old binary can be started against a newer schema, and the running image can be rolled back without the identity check the new code claims. This should fail closed (or require an explicit `--allow-legacy-state` / `--allow-schema-forward`).

## Current P2 findings (not blocking on their own)

1. `scripts/update_account_onboarding.sh:374-379` - materialized-secret verification still lacks regular-file/non-symlink/owner checks and accepts 0600 despite README:36's 0400-owned-by-10001 claim.
2. `scripts/update_account_onboarding.sh:508-510` - `write_attempt` writes `$ATTEMPT.tmp` and immediately `mv`s it, then `sync`s; unlike `write_state` (`:481-485`) and `write_pending` (`:452-454`) it has no pre-rename `sync "$ATTEMPT.tmp"`. R26 design claims the attempt is written "tmp + rename + sync"; a crash in that window can lose the intent record. None of the three writes fsyncs the parent directory.
3. `scripts/update_account_onboarding.sh:614-625` - `do_deploy` records `IMAGE_ID` from the build, not the running container's `.Image`; it never inspects the container after `compose up`. If a tag is replaced between build and up, state says the built ID while the container runs a different image; the next `--rollback` then refuses. R26 design line 9 says "compose up -> inspect image -> health", but there is no inspect step.
4. `scripts/update_account_onboarding.sh:226-227` - dry-run still rewrites `deploy/compose.base.yml` via the unconditional `cp` before the dry-run check; `--rollback --dry-run` additionally executes `docker compose config` and four `docker run` probes at `:661-668`; `--backup-only --dry-run` exits 3 at `:592`. Probe 7/8.
5. `scripts/update_account_onboarding.sh:94,314-318,588-593,628-675` - symlink/restore-marker coverage remains partial; rollback and backup skip `check_data_safety` entirely. `$SECRETS`, `$BACKUPS`, `$DEPLOY` and WAL/SHM paths are unchecked; a restore marker does not block `--rollback`.
6. `scripts/update_account_onboarding.sh:235,240-241,469` - paths are interpolated unquoted into YAML/JSON; `--root` with a double quote produces invalid `deployment-state.json` (probe 14). Host-port validation at `:82-85` is fixed.
7. `scripts/update_account_onboarding.sh:226` + `deploy/compose.yaml:25` - copying the base changes `build.context` from `$APP` to `$ROOT`; `docker compose config` confirms the generated base resolves `context: $ROOT` with a missing Dockerfile there.
8. `.github/workflows/deployment-tooling.yml:167,179-180` - cleanup has no `-f` and runs from the repo root where `docker compose config` fails with "no configuration file provided"; the cleanup assertion is never reached. Evidence uses the wrong container name `account-onboarding` instead of `account-onboarding-onboarding-1`.
9. `tests/deploy/update_script_test.sh:89,130,187` - fixture still creates a user-owned 0755 data dir, only dry-run deploy is tested, and the last-good rollback test checks a log substring rather than the exit code or a switched container; `STUB_CONTAINER_IMAGE` is unreachable due to the duplicate `{{.Image}}` case at lines 25/27.
10. `scripts/deploy/README.md:34,36` and `docs/runbooks/DEPLOYMENT.md:32-34` / `docs/runbooks/RELEASE_UPGRADE_ROLLBACK.md:114-116` - docs still overclaim the E2E ("verified snapshot", "database integrity check after each step") and the 0400/owner guarantee; E2E only asserts `*.enc`/manifest existence and three SQLite header checks.
11. `scripts/update_account_onboarding.sh:164,717` - `detect_docker` uses plain `docker compose version` even for a sudo-only `DOCKER` array, and the `do_status` fallback is unreachable because `detect_docker` calls `die`; `--status` exits 2 when docker is unavailable (probe 10).
12. `scripts/update_account_onboarding.sh:673` - if `dk image inspect "$prev_image"` fails after a healthy rollback, `write_state` records `current_image_id = "unknown"`; every later `--rollback` then sees a permanent mismatch and refuses. It should abort or use the actual container image.
13. `scripts/update_account_onboarding.sh:776-778` - the 0444/0644 source fix silently `chmod`s to 0400 with no log/audit line; the operator cannot tell that a permission change happened. The behavior is safe (tightened, not accepted) but not observable.
14. `scripts/update_account_onboarding.sh:236-241` - `$SECRETS` is not checked for being a symlink before Docker bind-mounts it; `harden_secret_source` only checks the two files and `SECRETS_SRC`. A symlinked `$SECRETS` can redirect the materializer's 0400 writes.

## Positive changes verified

- `ops_run` is now positional and dash-compatible (`:392-395`); probe 5 preserved spaces, quotes, empty args and command substitutions, and `/bin/dash -c` showed the intended `$0`/`$@` split.
- The base compose port is parameterised and the override no longer appends a second published port; `docker compose config` showed exactly one `127.0.0.1:18788 -> 8788` binding (P1-1).
- 0444/0644 secret sources are tightened to 0400 (probe 4), and the workflow checks that `nobody` cannot read the source.
- `--status` prints a pending record before any docker/container branch (`:707`); probe 2a shows it with docker, probe 2b shows the pending JSON printed even when docker is absent (the script then exits 2 at `:717`).
- `--rollback` refuses a running-image mismatch (probe 1) and refuses without a last-good image (probe 3).
- A successful A -> B -> A rollback takes and verifies a protection snapshot, switches the image, writes state and removes the pending marker (probe 13).
- State/pending writes leave no `.tmp`/pending temp files in my successful probes; `write_state` uses tmp + `sync` + `mv` + `sync` and removes `$STATE.pending`.

## Probe results

Harness: `/tmp/pr23-probe` (stub docker/git/sudo/stat/sleep under `/tmp/pr23-probe/bin`, roots under `/tmp/pr23-probe/out`). The stubs are independent of the repo test stubs.

| # | Probe | Result |
| --- | --- | --- |
| 1 | State `current_image_id` disagrees with `docker inspect .Image` | **PASS** - `--rollback` rc 3, "does not match the recorded current image"; no switch. |
| 2a | Pending marker + `--status` | **PASS** - prints "pending deployment (unfinished switch)" and the pending JSON before state/container output. |
| 2b | Pending marker + no docker in PATH | **PASS (record order), P2-12 confirmed** - pending JSON is printed first; script then exits 2 at `detect_docker` instead of returning 0. |
| 3 | No `last_good_image`/`previous_image` | **PASS** - rc 1, "no previous image recorded". |
| 4 | 0444 secret source | **PASS** - source mode became 0400 during `prepare_deploy`; no silent acceptance. |
| 5 | Spaces/quotes/metacharacters into `ops_run` | **PASS** - exact argv preserved (indices 24-30 in the captured vector); no command substitution executed; `/bin/dash` `$0`/`$@` semantics correct. |
| 6 | Missing `previous_schema_version` | **FAIL** - `--rollback --dry-run` rc 0 and "rolling back to account-onboarding:local-old"; the schema guard is skipped (P1-B). |
| 7a | `--deploy --dry-run` side effects | **FAIL** - state, secrets and override unchanged, but `deploy/compose.base.yml` was rewritten from "OLD BASE" to the copied compose file; `runtime/secrets` created. |
| 7b | `--rollback --dry-run` side effects | **FAIL** - same base rewrite; `docker compose config` plus four `docker run` probes executed (ls/stat per secret). |
| 8 | `--backup-only --dry-run` with a DB | **FAIL** - rc 3 "no usable snapshot after the backup step" after printing "DRY-RUN would snapshot". |
| 9 | Unreadable 000 data directory with a DB | **PASS** - `--preflight` rc 3, "not readable by the current user ... refusing to treat the existing database as absent". |
| 10 | `--status` with docker unavailable | **FAIL** - rc 2, not the documented informational rc 0; the `\|\| { warn; return 0; }` fallback is dead (P2-12). |
| 11 | Failed C deploy after A -> B, then `--rollback`/`--reconcile`/`--status` | **FAIL** - state B, attempt `failed` expecting C, container C; `--rollback` rc 3 mismatch, `--reconcile` rc 0 "no reconcile needed", `--status` no recovery warning (P1-A). |
| 12 | (covered by 11) status/reconcile after failed deploy | **FAIL** - see 11. |
| 13 | Successful A -> B -> A rollback | **PASS** - snapshot+verify before switch; rc 0; state now A; pending removed; container image A. |
| 14 | `--root` containing a double quote, then `--reconcile` | **FAIL** - state written with `"root": "/tmp/.../p14"q"`; `python3 -m json.tool` rejects it. |
| 15 | Legacy state without `current_image_id` + running image mismatch | **FAIL** - `--rollback --dry-run` rc 0 and "rolling back to ..."; running-image guard skipped (P1-B). |

## Docker E2E static assessment (no daemon)

- **Image comparison is meaningful, not self-comparison.** `A_ID` is captured from the running container before the second deploy (`.github/workflows/deployment-tooling.yml:88-90`), `B_ID` after the second deploy (`:94-98`), and after rollback the step compares `C_ID` to `IMAGE_A` and `!= IMAGE_B` (`:108-112`). Those are three distinct builds because the second deploy changes `ONBOARD_BUILD_VERSION` to `0.1.1-e2e` (`:91-93`); the assertion is not circular. Good.
- **Status/rollback port:** the steps do not pass `--host-port=18788` (`:73`, `:107`, `:113`), but both actions read `host_port` from the state file (`scripts/update_account_onboarding.sh:630-631` and `:709-710`), and the first deploy recorded 18788. The port is therefore consistent; this is not a false positive. The earlier review's concern about probing 8788 is fixed by the state-carried port, not by the workflow flags.
- **Cleanup is not reliable.** `docker compose -p account-onboarding down --remove-orphans` (`:179`) has no `-f`; from the repository root `docker compose config` fails with "no configuration file provided: not found", so the cleanup step fails before the assertion at `:180`. The assertion itself is fine, but it is not reached. (P2-9)
- **Evidence uses the wrong container name.** `docker logs --tail 40 account-onboarding` (`:167`) cannot find the Compose container `account-onboarding-onboarding-1`; the group's `|| true` hides the failure, and the artifact contains a Docker error instead of application logs. (P2-9)
- **Snapshot verification is not asserted.** The E2E only does `ls -1 .../*.enc` and `ls .../*.manifest.json` (`:99-101`); it never greps for "backup ok"/"verifying", so removing the `ops_run verify` call would not fail the E2E even though the runbook claims a "verified snapshot". (P2-11)
- **First install is pre-created outside the script.** `sudo install -d -o 10001 ... "$ROOT/runtime/data"` at `:57` masks the fresh-host ownership path; the new `ensure_data_owner` is never exercised by the E2E. (P2-10)
- **No failed-health deploy is tested.** The failure injections are a wrong SHA (`:117-128`) and a hand-written override (`:130-142`), both before a Compose switch. The `mutation_started`/`failed`/`--reconcile`/rollback-after-failure path (P1-A) has no E2E coverage.
- **Database integrity is not checked after every step.** The SQLite header checks are at `:77-83`, `:153-160` (and indirectly after rollback by health/status), not after each step as the runbook says.
- **Cleanup assertion:** `test -z "$(docker ps -aq --filter name=account-onboarding)"` (`:180`) is a good assertion, but because the preceding `down` fails it does not protect the runner from leaked containers.

## Notes on the newest change set

- `write_state` has the expected fields and write order: `state_version: 2`, `image/image_id/commit/schema_version`, `previous_image`, `last_good_image/commit/schema_version`, `current_image/current_image_id`, `previous_image_id/commit/schema_version` (`:457-486`). Successful rollback probe 13 showed `current_image`/ID switched to the old image and `last_good_image` emptied, so a second rollback refuses (expected).
- `write_pending` is only called from `do_rollback` (`:669`); `do_deploy` uses `deployment-attempt.json` instead (`:613`). `--status` prints `$STATE.pending` first (`:707`), before `detect_docker` (`:717`), so a pending rollback survives a docker outage in the output.
- Atomicity is good for state/pending in the normal path, but the attempt write lacks the pre-rename sync (current P2 item 2) and no directory fsync is performed. `sync` with a file argument is not portable to macOS (the script suppresses the error with `2>/dev/null || true`), so on macOS the pre-sync is a no-op; the final whole-system `sync` after `write_state` is the only durable barrier.
- `--rollback` compares the running container `.Image` (sha256 ID) with `current_image_id` from state (`:633-638`) and separately checks `previous_image_id` against the current tag resolution (`:655-659`). Both checks are correct when the fields exist; the fail-open cases are P1-B.

## Files checked

- `scripts/update_account_onboarding.sh` (826 lines at this HEAD)
- `tests/deploy/update_script_test.sh` (33 scenarios; 33 PASS)
- `.github/workflows/deployment-tooling.yml`
- `scripts/deploy/README.md`
- `docs/runbooks/DEPLOYMENT.md`, `docs/runbooks/RELEASE_UPGRADE_ROLLBACK.md`
- `deploy/compose.yaml` (port line 65)
- `docs/deployment/R25_DESIGN.md`, `docs/deployment/R26_DESIGN.md`, `docs/deployment/R26_PROGRESS.md` (claims cross-checked)
- `docs/acceptance/PR23-DEPLOYMENT-TOOLING-REVIEW.md` (read-only; not modified)

REVIEW_BLOCKED
