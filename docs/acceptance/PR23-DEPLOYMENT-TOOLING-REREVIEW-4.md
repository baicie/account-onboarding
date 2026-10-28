# PR #23 - deployment tooling re-review 4 (independent)

Repo: `/Users/liuzhiwei/Desktop/workspace/git-code/account-onboarding`
Branch: `feat/v0.1-compose-deployment`, HEAD `993b8e3` (no branch switch, no commit, no push; working tree left clean)
PR: #23 (base `main`)
Scope: the fix claimed by `993b8e3` ("recover to the release the failed attempt replaced") against the two halves of
P1-A that `PR23-DEPLOYMENT-TOOLING-REREVIEW-3.md` left open, plus a re-probe of P1-B/P1-C and a fast regression
sweep. The four earlier documents (`-REVIEW.md`, `-REREVIEW.md`, `-REREVIEW-2.md`, `-REREVIEW-3.md`) were read
only and are untouched.

Method: static reading plus my own stub harness under `/tmp/pr23-r4` (independent `docker`/`git`/`curl`/`stat`/
`sudo`/`df` stubs, one fresh root per probe, real `python3`+SQLite for the database). The stubs simulate only the
container runtime, image ids, health, and the uid/ownership boundary; every line of logic under test - state and
attempt JSON parsing, all rollback guards, the compose file writing, the health loop - is the repository script
itself. CI run `40630162411` was **not** queried (no network access from this session); every claim below comes from
a probe I ran or a line I read. No Docker daemon was available, so the Docker E2E is assessed statically.

## Verdict summary

| Item | Verdict | One line |
| --- | --- | --- |
| P0 | **0** | no data-safety or fail-open defect found |
| P1-A, destination half | **CLOSED** | the recovery now targets the release the failed attempt replaced (B), not the older last-good (A) - probes P-A1/A1q/P-A1q-file |
| P1-A, completion half (schema) | **OPEN** | the attempt's `previous_schema_version` is never read: the reader is string-only and the writer emits a bare number, so the exact state the commit claims to fix still exits 3 |
| P1-A (overall) | **OPEN** | the operator-facing recovery advertised by `do_deploy` still does not happen in the common schema-bump case |
| P1-B | **CLOSED** | both missing-field paths still exit 3, absent state still "nothing to roll back" |
| P1-C | **CLOSED** | with no attempt file the guard still exits 3 with its message |
| new scenario case | **half-effective** | catches a reverted destination override and a reverted log move, is blind to the missing schema half (mutation m3) |

Final line: **REVIEW_BLOCKED** (P1-A is not fully closed).

## What `993b8e3` changes

15 lines in `do_rollback` (`scripts/update_account_onboarding.sh:658-671`): after the target has been read from the
state, an attempt with `status != committed` overrides `prev_image`, `prev_id`, `prev_sha` and `recorded_schema`;
the `log "rolling back to ..."` line moves from before the state reads to after the override (`:672`). 12 lines of
test (`tests/deploy/update_script_test.sh:214-223`). It also adds the previous re-review document. No workflow,
compose or runbook change.

## P1-A, destination half - CLOSED

Claim: a failed deploy replaced B; the rollback must return to B, not to the generation before it.

| Probe | Fixture | Result |
| --- | --- | --- |
| P-A1 | state `current=B`, `last_good=A` (schema 5), attempt `status=failed`, `previous_image=B`, `previous_image_id=id-B`, `previous_schema_version=6` (bare), db schema 6, container runs the attempt image C | target line and warning name **B**; the run still exits **3** for the schema reason below |
| P-A1q | identical, with the attempt's `previous_schema_version` written as `"6"` | **rc 0**, `COMPOSE_UP`, `rolled back to account-onboarding:local-B`, generated override file carries `image: account-onboarding:local-B` |
| P-COMMIT | attempt `status=committed`, `previous_image=B` (deliberately divergent), state `last_good=A`, container runs C | **rc 0**, target **A**, override file `local-A`, no "unfinished attempt" warning - the override is **not** applied on the committed path |
| P-TAG | attempt `previous_image=B` but `previous_image_id=id-WRONG` | **rc 3** "the rollback image tag now resolves to sha256:id-B, but the recorded previous image id is sha256:id-WRONG" - the id check now uses the attempt's id and still fails closed |
| P-PLAIN | no attempt file | **rc 0**, target A (unchanged behaviour) |

The destination and the log/action agreement are genuinely fixed, and the fix does not loosen the replaced-tag guard:
the state's `previous_image_id` (id-A) is overwritten by the attempt's (id-B), which is what makes P-A1q pass the
check at `:682-685` instead of dying on it. P-A1q's evidence is the override file on disk, not only the log line.

## P1-A, completion half - OPEN

The override reads four fields; three are strings, one is a number, and the reader only understands strings.

```
scripts/update_account_onboarding.sh:503   printf "  \"previous_schema_version\": %s,\n" ...   # bare number
scripts/update_account_onboarding.sh:513   attempt_field() { sed -n "s/.*\"$1\": *\"\([^\"]*\)\".*/\1/p" ... }   # quoted only
scripts/update_account_onboarding.sh:668   ai="$(attempt_field previous_schema_version || true)"   # always empty
scripts/update_account_onboarding.sh:723   ATTEMPT_PREV_SCHEMA="$(sed -n 's/.*"previous_schema_version": *\([0-9][0-9]*\).*/\1/p' ...)"   # the correct reader, already in the same file
```

Isolated on a file with the exact bytes `write_attempt` produces:

| reader | result |
| --- | --- |
| `attempt_field status` | `failed` |
| `attempt_field previous_image` | `account-onboarding:local-B` |
| `attempt_field previous_image_id` | `sha256:id-B` |
| `attempt_field previous_commit` | `bbbb` |
| `attempt_field previous_schema_version` | **(empty)** |
| the numeric sed used at `:723` | `6` |

Consequence, in the state the previous review specified and the commit claims to fix:

| Probe | Fixture | Result |
| --- | --- | --- |
| P-A1 | state `last_good=A`, `previous_schema_version=5`; attempt `previous_schema_version=6`; db schema 6 | **rc 3** `database schema 6 is newer than the recorded 5 for the previous release ... is not a safe rollback` (`:679-680`). The container stays on the failed image C. |
| P-A1t | same but the state has only `last_good_schema_version: 5` (the shipped test's fixture shape) | **rc 3** `the recorded state has no schema version for the previous release` (`:674`) |
| P-A1q | same as P-A1 but the attempt's value is quoted `"6"` | **rc 0**, container B - the only difference is the quoting |
| P-DRYRUN | P-A1 with `--dry-run` | **rc 3**, same schema refusal |

So `do_deploy`'s advice "to go back: ... --rollback" (`:619`) still cannot be followed whenever the last healthy
release carries a lower recorded schema than the database, which is exactly the "schema bump" case the previous
review raised. The recovery works only when the schemas happen to be equal *and* the state already carries a
`previous_schema_version` >= the database schema.

The defect is one field read; the fix is the reader the same file already uses for reconcile (`:723`). I validated
it on a copy (`/tmp/pr23-r4/copies/m4`, repository not modified): P-A1 becomes **rc 0** with the override file and
state both on **B**, the same repair also makes the shipped fixture shape succeed, the unrelated-running-image probe
stays **rc 3**, and the committed path still targets the state's A.

## Probe 5 - does the new scenario case actually fail when the fix is mutated?

Copies of `scripts/`, `tests/deploy/` and `deploy/` under `/tmp/pr23-r4/copies/<m>`; the repository's own suite is
run against each copy (`bash tests/deploy/update_script_test.sh`), and the P-A1 fixture is replayed against each
mutated script.

| Mutation | Suite | P-A1 replay |
| --- | --- | --- |
| `m1` - the whole attempt override deleted (revert of `993b8e3`) | **FAIL** "recovery targets the wrong release", 36/1, `DEPLOY_TOOLING_TESTS_FAIL` | rc 3, `rolling back to account-onboarding:local-A` |
| `m2` - only the log move reverted (log before the override, action still B) | **FAIL** "recovery targets the wrong release", 36/1 | rc 3, log says `local-A` while the action would be B |
| `m3` - only the schema override deleted (`:668-669`), everything else intact | **37/37 PASS** | rc 3 - indistinguishable from HEAD |
| `m4` - the schema field read with the numeric sed of `:723` | **37/37 PASS** | **rc 0**, `rolled back to account-onboarding:local-B`; the committed path still targets A; an unrelated running image still exits 3 |
| `strict` - the shipped case plus an `rc` assertion (`want ... 0 "rolling back to ... local-B"`), nothing else changed | **FAIL** "recovery returns to the replaced release (B) with exit 0 (rc=3 want=0)", 36/1, log tail: `the recorded state has no schema version for the previous release` | - |

Conclusion: the destination and the log/action agreement are genuinely guarded by the new case, and m1/m2 prove it.
The schema half is not: deleting it leaves the suite green (m3), and the very fixture the new case ships exits 3 once
its exit code is asserted (`strict`). `tests/deploy/update_script_test.sh:222-223` never looks at `RC` and greps a
line printed at `:672`, i.e. **before** the schema guards at `:674` and `:679` - the same false-positive shape this
review series flagged at `:204`, now reproduced one case lower. That is why CI is green while the behaviour is not
fixed.

## Re-probere results (fresh fixtures, no reliance on the repository tests)

| # | Probe | Result |
| --- | --- | --- |
| P-BASE | healthy deployment, equal images, no attempt | **PASS** rc 0, container switched, state rewritten |
| P-COMMIT | committed attempt, divergent `previous_image` | **PASS** target = state `last_good` (A), override file A |
| P-REG-C | state `current_image_id=id-B`, container on C, **no attempt file** | **PASS** rc 3 "does not match the recorded current image" (P1-C regression) |
| P-REG-C2 | attempt present, container on an image that is neither recorded nor expected | **PASS** rc 3, same message |
| P-REG-B1 | state without `current_image_id` | **PASS** rc 3 "no current image id; refusing to roll back blindly" (P1-B) |
| P-REG-B2 | state with no recorded schema for the previous release, no attempt | **PASS** rc 3 "no schema version for the previous release" |
| P-REG-B3 | no state file | **PASS** rc 1 "nothing to roll back to" |
| P-TAG | replaced rollback tag | **PASS** rc 3 (see above) |
| P-STALE | attempt `mutation_started` written before a failing `compose up`, container still on B, state `last_good=A` | **DEFECT (P2-2)** rc 0, "rolled back to B" - the image that is already running - and the state is rewritten with `last_good_image: ""`, `previous_image_id: ""`, `previous_schema_version: 0` |
| P-STALE-prefix | same fixture against `m1` (pre-fix) | rc 0, target **A** - the new code changes a real rollback into a no-op here |
| P-ATTEMPT-EMPTY-STATUS | attempt file with no `status` field | rc 3, but only because the replaced-tag check fires; the target was taken from the unattributable file |
| P-A1 / P-A1t / P-A1q / P-A2 | see the P1-A section | destination B, completion missing |

## Regression sweep (unchanged areas, re-probed)

| Area | Result | Evidence |
| --- | --- | --- |
| Secret source 0700/0400 | **PASS** | a 0755 source file is refused with rc 2 "has mode 755; expected 0400 or 0600" (`:804-811`); directory 0755 -> 0700, files 0644/0444 -> 0400, then a successful rollback (`:797-812`) |
| Single published port | **PASS** | `deploy/compose.yaml:65` holds the one mapping `127.0.0.1:ONBOARD_HOST_PORT:8788`; the generated override has no `ports:`; health probes used the port recorded in the state (18788, `curl ... http://127.0.0.1:18788/healthz`) |
| `ops_run` positionals | **PASS** | the function body extracted verbatim and run with a capturing `dk`: the `-c` script is constant, 8 separate argv entries follow (`a b`, `it's`, `$(touch ...)`, `a;b` and an empty string all intact), no `PWNED` file |
| Unreadable data fails closed | **PASS** | data dir 000, no usable sudo, `--preflight` rc 3 "is not readable by the current user" (`:786-788`) |
| Restore marker | **PASS** | marker present, `--preflight` rc 3 "a restore marker is present" (`:320-322`) |
| Schema via `schema_migrations` | **PASS** | db versions 1,5,6 with recorded 5 -> rc 3 "not a safe rollback"; `--status` on the same root prints "database schema: 6" (`:265-296`, `:767`) |

Tooling: `bash -n` clean on both scripts; `/opt/homebrew/bin/shellcheck -S warning scripts/update_account_onboarding.sh tests/deploy/update_script_test.sh` -> rc 0; `bash tests/deploy/update_script_test.sh` -> `SUMMARY pass=37 fail=0` / `DEPLOY_TOOLING_TESTS_PASS` (matches the commit message). HEAD `993b8e3`, `git status --porcelain` empty after the review.

## Docker E2E - static assessment (unchanged by this commit)

The workflow is not part of `993b8e3`, so the E2E is exactly what re-review 3 described: `:104-115` asserts a real
image-id change back to A, `:175-180` asserts cleanup, and every status/rollback step uses the configured port. It
still never creates `deployment-attempt.json` and never enters the mismatch branch (`:640-648`): the two failure
injections are a wrong SHA before anything changes (`:117-128`) and a hand-written override before a build-only run
(`:130-142`), and `--reconcile` is never run. A green docker-e2e therefore says nothing about either half of P1-A;
the only coverage of the new code is the one grep-only scenario case.

## P0 (blocking)

None.

## P1 (blocking)

1. **P1-A is still open on its completion half.** `scripts/update_account_onboarding.sh:668` reads
   `previous_schema_version` with the string-only helper `:513`, while `write_attempt` emits the value as a bare
   number (`:503`), so the attempt's schema never reaches `recorded_schema` and the guards at `:674`/`:679-680`
   still judge the target by the *state's* older value. In the state the commit claims to fix (attempt replacing a
   schema-6 release on a schema-6 database, state last good at schema 5) the recovery exits 3 and leaves the
   container on the failed image: probe P-A1. Quoting the same fixture's value (P-A1q) or applying the numeric
   reader already present at `:723` (copy `m4`) makes it exit 0 and land on B, which isolates the defect to that
   one read.

## P2 (probed or read this round; not blocking on their own)

1. `tests/deploy/update_script_test.sh:222-223` - the new case asserts no exit code and greps the line printed at
   `scripts/update_account_onboarding.sh:672`, before the schema guards; the shipped fixture itself exits 3
   (mutation `strict`). Deleting the schema override keeps the suite at 37/37 (mutation `m3`), so the suite cannot
   regress-guard the half that is broken.
2. `scripts/update_account_onboarding.sh:661` - the override is keyed on "attempt exists and status != committed"
   rather than on the accepted-mismatch branch. With a `mutation_started` attempt left by a failing `compose up`
   (written at `:613`, before the switch) and the container still on the recorded image, `--rollback` now exits 0
   reporting "rolled back to B" while B is what is already running, and `:699` then rewrites the state with
   `last_good_image`/`previous_image_id` empty - the state's last good (A) becomes unreachable through the tool
   (probe P-STALE; the same fixture against the pre-fix code targets A). An attempt file whose `status` field is
   missing also takes the override, with the target taken from an unattributable file (P-ATTEMPT-EMPTY-STATUS).
3. `scripts/update_account_onboarding.sh:645` - the warning still promises "returning to the last known good image"
   while the accepted path now returns to the attempt's previous image; in P-A1 the state's last known good is A and
   the target is B, so two contradictory WARNs are printed (`:645`, `:670`).
4. `scripts/update_account_onboarding.sh:513` - `attempt_field` still has no internal `|| true` and no numeric
   variant, so every caller must remember both; `:660` relies on its own `2>/dev/null || true`. The next caller
   that forgets reintroduces P1-C, and `:668` shows the numeric variant is already missing.
5. `scripts/update_account_onboarding.sh:699` - unchanged: a successful rollback still writes `last_good_image: ""`,
   `previous_image_id: ""` and `previous_schema_version: 0`, so a second `--rollback` has nothing recorded; with the
   new override it silently re-targets the attempt's previous image instead of refusing (P-A1q /
   P-STALE states after the run).
6. `scripts/deploy/README.md:25` - still says `--rollback` "restores the previous image recorded in the state file",
   which is no longer what happens when an attempt is on record.
7. `tests/deploy/update_script_test.sh:25` - the first `{{.Image}}` case still shadows `STUB_CONTAINER_IMAGE`
   (`:27` is unreachable), so no case can choose a running image independent of the recorded one; the new case has
   to rely on the stub's fixed id.
8. `scripts/update_account_onboarding.sh:650` - duplicate `[ -f "$STATE" ]` (dead after `:636`).

## Probe harness

`/tmp/pr23-r4`: `bin/` = docker/git/curl/stat/sudo/df stubs; `out/` = one fixture root per probe with the full run
log and the generated state/override; `copies/{m1,m2,m3,m4,strict}` = the mutation copies used above. The repository
was not modified: `git status --porcelain` is empty and HEAD is still `993b8e3`.

## Files checked

`scripts/update_account_onboarding.sh` (whole file, 857 lines, with focused reading of `:488-517`, `:596-730`,
`:770-830`), `tests/deploy/update_script_test.sh` (whole file), `.github/workflows/deployment-tooling.yml`,
`deploy/compose.yaml`, `scripts/deploy/README.md`, `docs/runbooks/DEPLOYMENT.md`,
`docs/runbooks/RELEASE_UPGRADE_ROLLBACK.md`, and the four earlier review documents (read only).

REVIEW_BLOCKED
