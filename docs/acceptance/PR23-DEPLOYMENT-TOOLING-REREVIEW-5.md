# PR #23 - deployment tooling re-review 5 (independent)

Repo: `/Users/liuzhiwei/Desktop/workspace/git-code/account-onboarding`
Branch: `feat/v0.1-compose-deployment`, HEAD `a643ce6` (no branch switch, no commit, no push; the only change this
review makes to the tree is this new file)
PR: #23 (base `main`)
Scope: the three defects `PR23-DEPLOYMENT-TOOLING-REREVIEW-4.md` left in P1-A - (a) the bare-number
`previous_schema_version` read with the quoted-string helper, (b) the scenario case that was blind to that half, (c)
the retarget keyed on attempt presence instead of the accepted mismatch - plus the requested re-probes of P1-B/P1-C
and a fast regression sweep. The five earlier documents (`-REVIEW.md`, `-REREVIEW.md`, `-REREVIEW-2.md`,
`-REREVIEW-3.md`, `-REREVIEW-4.md`) were read only and are untouched.

Method: static reading plus my own stub harness under `/tmp/pr23-r5` (independent `docker`/`git`/`curl`/`df`/`sudo`/
`stat` stubs, one fresh root per probe, real `python3` + SQLite for the database). The stubs simulate only the
container runtime, image ids, health, the backups `docker run` would produce, and the uid/ownership boundary; every
line of logic under test - state and attempt JSON parsing, all rollback guards, compose file writing, the health
loop - is the repository script itself. CI run `40632102949` was **not** queried (no network access from this
session); every claim below comes from a probe I ran or a line I read. No Docker daemon was available, so the Docker
E2E is assessed statically.

## Verdict summary

| Item | Verdict | One line |
| --- | --- | --- |
| P0 | **0** | no data-safety or fail-open defect found |
| P1-A, destination half | **CLOSED** | the recovery targets B, the release the failed attempt replaced, not the older last-good A - probe p1 |
| P1-A, completion half (schema) | **CLOSED** | the attempt's bare-number `previous_schema_version` is now read numerically; reverting that one read on a copy brings the refusal back - probes p1/p1m |
| P1-A, third half (retarget gating) | **CLOSED** | the override only applies inside the accepted-mismatch branch (`RECOVERING`); a stale `mutation_started` attempt no longer turns the rollback into a no-op - probe p3 |
| P1-A (overall) | **CLOSED** | the advertised recovery now happens in the schema-bump case that was broken |
| P1-B | **CLOSED** | both missing-field paths still exit 3, absent state still "nothing to roll back to" |
| P1-C | **CLOSED** | unrelated running image still exits 3 with its message, with and without an attempt file |
| scenario case (`tests/deploy/update_script_test.sh:218-226`) | **partially effective** | catches a reverted destination override (m2), still blind to a reverted schema reader (m1) and to a deleted `RECOVERING` guard (m3) |

Final line: **REVIEW_ACCEPTED** - no P0, no P1; the remaining findings are P2 (test coverage, one stale message,
docs).

## What `a643ce6` (+ `9f64273`) changes

- `scripts/update_account_onboarding.sh:645-646` - the accepted-mismatch branch sets `RECOVERING=1` and rewords its
  warning to "returning to the release that attempt replaced".
- `scripts/update_account_onboarding.sh:662` - the attempt override is now gated on
  `[ "${RECOVERING:-0}" = "1" ]`, i.e. keyed on the branch that actually accepted the running image, not on the mere
  presence of an attempt file.
- `scripts/update_account_onboarding.sh:669` - the attempt's `previous_schema_version` is read with the numeric sed
  that `:724` already used, instead of the quoted-string helper `attempt_field` (`:513`).
- `tests/deploy/update_script_test.sh:85-87` - a `sudo` stub that always fails, so the fixture behaves the same on a
  developer machine and on a CI runner.
- `tests/deploy/update_script_test.sh:226` - the new case also asserts the absence of "not a safe rollback".
- No workflow, compose, runbook or README change.

## P1-A, destination half - CLOSED

Probe p1 (fresh root): state `current_image_id=sha256:id-B`, `last_good_image=local-A` with
`previous_schema_version: 5` and `previous_image_id=sha256:id-A`; attempt `status=failed`,
`previous_image=local-B`, `previous_image_id=sha256:id-B`, `previous_commit=bbb`,
`previous_schema_version: 6` written as a bare number; database `schema_migrations = 1,5,6`; container running
`sha256:id-C`, the attempt's `expected_image_id`.

| Evidence | Value |
| --- | --- |
| exit code | **0** |
| `rolling back to` line | `rolling back to account-onboarding:local-B (bbb)` |
| "not a safe rollback" in the log | **absent** |
| generated override | `image: account-onboarding:local-B` |
| final line | `rolled back to account-onboarding:local-B` |

The destination (`prev_image`), the commit (`prev_sha`, `bbb` from the attempt rather than `aaa` from the state) and
the image id (`prev_id`, `id-B` from the attempt rather than `id-A` from the state) all come from the attempt, so
the replaced-tag guard at `:683-687` is satisfied rather than tripped. The pre-run state and the pre-run attempt were
snapshotted before the run, so the fixture is provably the one described above (a successful rollback rewrites the
state at `:700`).

## P1-A, completion half (schema) - CLOSED

The state itself records schema **5** while the database is at **6**, so an exit code of 0 is only possible if the
`6` written into the attempt reaches `recorded_schema`:

| Probe | Fixture difference | rc | `rolling back to` | "not a safe rollback" |
| --- | --- | --- | --- | --- |
| p1 | HEAD `9f64273/a643ce6` | **0** | `local-B` | no |
| p1m | identical fixture, numeric reader on a copy replaced by `attempt_field previous_schema_version` | **3** | `local-B` | **yes** - `database schema 6 is newer than the recorded 5 for the previous release ... is not a safe rollback` |

Mutating exactly one line (`:669` back to the quoted helper, nothing else) restores the refusal that
`PR23-DEPLOYMENT-TOOLING-REREVIEW-4.md` reported, which isolates the fix to that read and proves the `6` in p1 came
from the attempt file and not from the state. The refused run leaves the container on the failed image, as before.

## P1-A, third half (retarget gating) - CLOSED

| Probe | Fixture | rc | Target | Verdict |
| --- | --- | --- | --- | --- |
| p3 | attempt `status=mutation_started` with `previous_image=local-C` and `expected_image_id=sha256:id-D`, container **still** on the recorded `sha256:id-C` (the `compose up` never switched) | **0** | `rolling back to account-onboarding:local-A (aaa)` - the state's last good, *not* the attempt's previous image | the no-op is gone; no "unfinished deployment attempt" warning is printed |
| p3m3 | the same fixture against mutation m3 (the `RECOVERING` guard deleted) | **0** | `rolling back to account-onboarding:local-C (ccc)` → `rolled back to account-onboarding:local-C` - the image that is already running, and `:700` then rewrites the state with `last_good_image: ""`, `previous_image_id: ""`, `previous_schema_version: 0` | confirms the guard is what prevents the silent no-op, and that the guard's repair is real |

Probe p2 confirms the committed side of the same gate: attempt `status=committed` with a decoy
`previous_image=local-B` / `previous_schema_version: 3`, container on the attempt image `sha256:id-D`, state
`last_good_image=local-A` → rc **0**, target `local-A`, override `local-A`: the override is not applied on the
committed path (had it been, the decoy schema 3 would have tripped `:681`).

## Probe results (probes 1-5 as specified)

| # | Fixture | Result |
| --- | --- | --- |
| 1 | state current=B, last_good=A (schema 5); attempt failed, previous=B, schema 6 (bare); db schema 6; container on the attempt image | **PASS** rc 0, target **B**, no "not a safe rollback", override `local-B` |
| 1m | the same fixture with `:669` reverted to the quoted helper on a copy | **PASS (defect reproduces)** rc 3, "database schema 6 is newer than the recorded 5" |
| 2 | attempt `status=committed`, decoy previous=B, container on the attempt image | **PASS** rc 0, target = state `last_good` (**A**), override `local-A`, no retarget |
| 3 | attempt `status=mutation_started`, container still on the recorded image | **PASS** rc 0, target = state `last_good` (**A**), no override, no no-op |
| 4a | running image unrelated to the state, **no attempt file** | **PASS** rc 3 `does not match the recorded current image (sha256:id-B); refusing to roll back blindly - run --status and reconcile first` (P1-C) |
| 4b | attempt present, running image matches neither the recorded nor the expected image | **PASS** rc 3, same message |
| 5a | state without `current_image_id` | **PASS** rc 3 `the recorded state has no current image id; refusing to roll back blindly - reconcile the state first` (P1-B) |
| 5b | state with no recorded schema for the previous release (only `last_good_schema_version`), no attempt | **PASS** rc 3 `the recorded state has no schema version for the previous release; refusing to roll back blindly` (P1-B) |
| 5c | no state file | **PASS** rc 1 `no deployment state; nothing to roll back to` |

## Probe 6 - do mutations of the fix fail the scenario suite?

Copies of `scripts/`, `tests/deploy/` and `deploy/` under `/tmp/pr23-r5/copies/<m>`; the repository's own suite
(`bash tests/deploy/update_script_test.sh`) is run against each copy; the affected fixture is replayed against the
mutated script.

| Mutation | Suite | Replay / meaning |
| --- | --- | --- |
| **m1** `:669` numeric reader → the quoted-string helper (the pre-fix read) | **37/37 PASS** | replay of the shipped fixture shape: rc **3** "the recorded state has no schema version for the previous release" → **the suite is blind to the schema half** |
| **m2** the whole override block deleted (`:662-672`, 11 lines) | **FAIL** 36/1 "recovery targets the wrong release or the schema guard refuses it" | destination retarget **is** guarded |
| **m3** the `RECOVERING` guard deleted (`:662` keyed on attempt presence again) | **37/37 PASS** | replay (p3m3): rc 0 "rolled back to local-C" = the running image, state wiped → **the suite is blind to the gating half** |
| **m1b** m1 + the case's `! grep -q "not a safe rollback"` half removed | 37/37 PASS | moot: m1 alone is already green |
| **m1x** m1 + the case's state fixture also carrying `previous_schema_version: 5` | **FAIL** 36/1, log shows "database schema 6 is newer than the recorded 5 ... not a safe rollback" | the blindness is the *fixture shape*: with a recorded state schema the refusal takes the branch the case greps for |
| **m1y** m1 + one extra grep `! grep -q "no schema version for the previous release"` | **FAIL** 36/1 | one-line repair; catches m1 as shipped |
| **ok_y** the shipped script + that same extra grep | 37/37 PASS | the repair does not over-fit |
| **m4** HEAD + an `rc=0` assertion on the shipped fixture | **FAIL**, rc=2 | an exit-code assertion cannot be added to the shipped fixture as-is: `prepare_deploy` dies at the uid boundary (`:822`), which is why the case only greps |
| **dry-run** (`--dry-run --rollback` skips `prepare_deploy` at `:688-692`) | HEAD + P1-A fixture **rc 0** "DRY-RUN would roll back"; m1 + the same fixture **rc 3**; HEAD + the shipped fixture **rc 0**; m1 + the shipped fixture **rc 3** | a case run under `--dry-run` with an `rc=0` assertion reaches the guards without the uid boundary and **would** catch m1 |

Conclusion: the destination and the schema repair are real and the shipped case now proves the destination and the
"newer schema" refusal (m2, m1x). It still cannot fail when the schema *read* is reverted (m1), and no case at all
covers the `RECOVERING` gate (m3) - both are P2-1/P2-2 below, with the two verified one-line repairs.

## Fast re-probe sweep (unchanged areas)

| Area | Result | Evidence |
| --- | --- | --- |
| Secret source 0700/0400 | **PASS** | a 0755 source file is refused with rc 2 `has mode 755; expected 0400 or 0600` (`:806-811`); with 0644 the file is tightened and the rollback succeeds, host modes after the run `400/400` (`:808-810`) |
| Single published port | **PASS** | `deploy/compose.yaml:62-65` holds the one mapping `127.0.0.1:ONBOARD_HOST_PORT:8788`; the generated override contains no `ports:` |
| `ops_run` positionals | **PASS** | function body extracted verbatim and run with a capturing `dk`: `ARGV_COUNT=29`, the `-c` script constant, then `backup`, `a b`, `it's`, `$(touch ...)`, `a;b` and an empty string as separate argv entries; no `PWNED` file (`:385-396`) |
| Unreadable data fails closed | **PASS** | data dir mode 000, no usable sudo → `--preflight` rc 3 `is not readable by the current user` (`:787-788`) |
| Restore marker | **PASS** | marker present → `--preflight` rc 3 `a restore marker is present` (`:320-322`) |
| Schema via `schema_migrations` | **PASS** | db versions 1,5,6 with recorded 5 → rollback rc 3 `database schema 6 is newer than the recorded 5 ... not a safe rollback` (`:680-681`); `--status` on the same root prints `database schema: 6` (`:768`) |

Tooling: `bash -n` clean on both scripts; `/opt/homebrew/bin/shellcheck -S warning scripts/update_account_onboarding.sh
tests/deploy/update_script_test.sh` → rc 0; `bash tests/deploy/update_script_test.sh` → `SUMMARY pass=37 fail=0` /
`DEPLOY_TOOLING_TESTS_PASS` (matches CI). The suite prints one line of noise before its first case: see P2-3.

## Docker E2E - static assessment

`.github/workflows/deployment-tooling.yml` is untouched by these commits. The rollback step (`:104-115`) runs
`--rollback` once (`:107`) and asserts that the container image id is back to `IMAGE_A` and stays healthy; at that
point no attempt file exists, `RECOVERING` stays 0, so that step exercises the *plain* rollback only. The file
contains no reference to `attempt` or `mutation` at all, so no step ever creates `deployment-attempt.json`: neither
the retarget nor the `RECOVERING` gate can be covered by a green docker E2E. The two failure injections are a wrong
SHA before anything changes (`:117-128`) and a hand-written override before a build-only run (`:130-142`);
`--reconcile` is never run. The green E2E on `a643ce6` therefore corroborates the unchanged paths, nothing more.

## P0 (blocking)

None.

## P1 (blocking)

None. P1-A is closed on all three halves (probes p1/p1m, p2, p3), and the P1-B/P1-C regressions still hold
(probes 4a/4b/5a/5b/5c).

## P2 (probed or read this round; not blocking on their own)

1. `tests/deploy/update_script_test.sh:218-226` - the new case still cannot regress-guard the schema read. Reverting
   `scripts/update_account_onboarding.sh:669` to the quoted helper keeps the suite at 37/37 (m1); the replay exits 3
   with "the recorded state has no schema version for the previous release". Two causes: the fixture's state carries
   only `last_good_schema_version`, so the refusal takes `:675` - a message the case does not grep for - and the line
   it does grep (`"rolling back to"`, printed at `:673`) precedes both schema guards. Verified one-line repairs:
   add `! grep -q "no schema version for the previous release"` (m1y: FAILs on m1, green on HEAD), or give the state
   fixture a `previous_schema_version` (m1x: FAILs), or add a `--dry-run` twin of the case asserting `rc=0` (the
   dry-run path skips `prepare_deploy` and reaches the guards; HEAD rc 0, m1 rc 3).
2. `tests/deploy/update_script_test.sh:217-226` - no case exercises the guard this commit added. Deleting the
   `RECOVERING` condition (`:662`) keeps the suite at 37/37 (m3) while the stale-attempt fixture silently re-targets
   the image that is already running and wipes the state (p3m3). A case is cheap: attempt `mutation_started`,
   container on the state's `current_image_id`, `last_good_image` different - the target must be the state's last
   good and no "unfinished deployment attempt" warning may appear.
3. `tests/deploy/update_script_test.sh:86` - the continuation of the comment on `:85` is missing its `#`:
   `on a developer machine and on a CI runner.` is executed as a command on every suite run (`bash: line 86: on:
   command not found`). Harmless only because the file sets `set -uo pipefail` without `-e`; shellcheck does not
   flag it and the suite still reports 37/37.
4. `scripts/update_account_onboarding.sh:646` - the warning now promises "returning to the release that attempt
   replaced" before the status gate at `:662`. With a committed attempt whose state is stale (probe p2) the run
   prints that WARN and then rolls back to the state's `last_good` (A), not the attempt's previous image (B); the
   message and the action disagree. Gating the WARN on the same condition as `:662` (or moving it after the gate)
   fixes it. It is a narrow shape - a committed attempt normally means `write_state` already ran at `:623`.
5. `scripts/update_account_onboarding.sh:669` - the numeric reader is inlined a second time instead of living next to
   `attempt_field` (`:513`); there are now three copies of that sed (`:669`, `:724`, and `state_num` `:517`). A single
   `attempt_num()` helper would have removed the whole class of defect P1-A was, and `attempt_field` still has no
   `|| true` of its own, so the next caller that forgets one reintroduces P1-C.
6. `scripts/update_account_onboarding.sh:651` - duplicate `[ -f "$STATE" ]` (dead after `:636`), unchanged.
7. `scripts/update_account_onboarding.sh:700` - a successful rollback still writes `last_good_image: ""`,
   `previous_image_id: ""` and `previous_schema_version: 0` (p1 post-run state), so a second `--rollback` has nothing
   recorded; it now fails closed at `:675`/`:681` instead of silently re-targeting, which is the right direction, but
   the recovery chain is still only one hop.
8. `scripts/deploy/README.md:25` - still says `--rollback` "restores the previous image recorded in the state file";
   with an attempt on record it now returns to the release that attempt replaced.
9. `tests/deploy/update_script_test.sh:25-27` - the first `{{.Image}}` case still shadows `STUB_CONTAINER_IMAGE`
   (`:27` is unreachable), so no case can choose a running image independent of the stub default; the new case relies
   on the fixed `sha256:stubimage` id.
10. `.github/workflows/deployment-tooling.yml:104-115` - as above: the docker E2E never creates
    `deployment-attempt.json`, so a green `docker-e2e` says nothing about the code this PR series changed twice.

## Probe harness

`/tmp/pr23-r5`: `bin/` = `docker`/`git`/`curl`/`df`/`sudo`/`stat` stubs (the `stat`/`sudo` pair simulates the
uid-10001 boundary so a developer machine can reach rc 0); `out/<probe>/` = one fixture root per probe with the
pre-run `fixture/` snapshot, the full log and the generated state/override; `copies/<m>` = the mutation copies
(m1, m1b, m1x, m1y, m2, m3, ok_y, m4, m5) and their suite logs. The repository was not modified: `git status
--porcelain` is empty apart from this document and HEAD is still `a643ce6`.

## Files checked

`scripts/update_account_onboarding.sh` (whole file, 858 lines, focused on `:488-517`, `:596-731`, `:771-831`),
`tests/deploy/update_script_test.sh` (whole file), `.github/workflows/deployment-tooling.yml`, `deploy/compose.yaml`,
`scripts/deploy/README.md`, and the five earlier review documents (read only).

REVIEW_ACCEPTED
