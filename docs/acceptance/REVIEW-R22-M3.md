# R22 M3 security re-review (secret materialization, Docker, Kind, ACL)

Scope: every M3 artefact that touches secrets - `scripts/materialize_secrets.sh`,
`tests/r22/docker_secret.sh`, `tests/r22/kind_secret.sh`,
`tests/r22/acl_secret.sh`, `tests/r22/materializer_fault.sh`,
`.github/workflows/r22-deployment-matrix.yml`, `deploy/secret-materializer/`.

**This is a self-review**: the same agent wrote the code and reviewed it. No
independent reviewer was available in this session, so it is *not* sufficient on
its own for `PRE_RELEASE_QUALIFIED`. An independent review of the diff range
`788197a..67d4f09` is still required.

## Verdict

| Severity | Found | Fixed | Open |
| --- | --- | --- | --- |
| P0 | 0 | - | - |
| P1 | 5 | 5 | 0 |
| P2 | 4 | 3 | 1 (accepted, documented) |
| P3 | 6 | 0 | 6 |

## P1 findings (all fixed in `0806243`)

### P1-1 - the previous secret could be destroyed (reproduced, not inferred)

The old commit loop moved the live file into a backup directory that lived
*inside* the staging directory, and only appended the name to `committed` after
the install rename:

    if [ -e "$DST/$name" ]; then mv -f -- "$DST/$name" "$BACKUP/$name"; fi
    mv -f -- "$STAGE/$name" "$DST/$name"
    committed+=("$name")

A failure between the two renames left the name out of `committed`, the exit trap
skipped it and then deleted the staging directory that held the only copy.
Reproduced on a real filesystem with an `mv` shim failing the install of `beta`:
`alpha` rolled back to `OLD-A`, `beta` was **gone**, exit 1.

Fix: the backup set lives in `$DST/.materialize.txn/backup` (outside staging), the
journal records `B <name>` *before* the backup rename and `I <name>` *before* the
install rename, and recovery replays the journal to restore every backed-up name.
Verified by `tests/r22/materializer_fault.sh` case 1 and by the same shim that
reproduced the loss.

### P1-2 - crash recovery data was deleted by cleanup

Same root cause as P1-1: because the backup directory was created with
`mktemp -d` under staging, any `kill -9` left recovery material in a directory
the next run simply removed. Fixed by the journal plus a next-run replay, which
is now the *only* recovery mechanism and does not depend on a trap.

### P1-3 - the lock silently degraded

    if command -v flock >/dev/null 2>&1; then ... flock -n 9 ...; fi

Without `flock` the script proceeded unlocked: two materializers could interleave
their renames on one target. Fixed: `flock` is required, otherwise the script fails
closed with `flock is required to serialize the target directory`. Asserted by fault
case 10, which runs the script with a PATH that has every other utility but no
`flock`.

### P1-4 - the lock file was an attacker-selectable directory entry

`exec 9>"$DST/.materialize.lock"` opens (and truncates) whatever that path is. In
an init container running as root, a pre-created symlink there redirects the open
and the following `chmod 0600`. Fixed: the lock is `flock(2)` on the target
directory descriptor - no lock path exists at all, so there is nothing to
pre-create, nothing to leave stale and no PID-named file to collide with.

### P1-5 - validate-then-read race on the source

The old code validated `$SRC/$name` (readlink/stat) and then re-resolved the path
for `cp`, so the object that was checked and the object that was copied could be
different. Fixed: the source is opened once (`exec 7<"$f"`), validated *through that
descriptor* (`stat -L` on `/proc/self/fd/7`, containment resolved from the fd, size
compared after the copy) and copied from the same descriptor. A concurrent swap
of the path can no longer redirect the checks.

## P2 findings

### P2-1 - Kind rotation proved nothing (fixed)

`grep -q 'kind-admin-' /run/secrets/onboard_admin_token` passed for the *old*
value too. Replaced with identity checks: source digest vs in-pod digest before
and after rotation, digest inequality, the master key proven unchanged, and the
running service proven to accept the new admin credential (200) and reject the
previous one (401) through `/v1/ops/metrics`. Only truncated digests are printed.

### P2-2 - cleanup failures could not fail the job (fixed)

`cleanup` recorded `CLEAN_RC=1` and then exited with the test status only, so a
leaked cluster, volume or container was invisible. Now the test status is
preserved, cleanup failure fails the job when the test passed, "not found" is
distinguished from a failure (existence is re-checked instead of trusting the
delete exit code), and the workflow has a separate always-run leftover check.

### P2-3 - fixed resource names (fixed)

`CLUSTER="r22-kind"`, `r22-data` and `r22-candidate:local` collide across
concurrent runs. Now run-scoped names plus a workflow concurrency group.

### P2-4 - the init container runs as root with three capabilities (accepted, documented)

`runAsUser: 0` with `CHOWN, DAC_OVERRIDE, FOWNER`, everything else dropped,
`allowPrivilegeEscalation: false`. Necessity:

- `CHOWN` - the materialized file must be owned by the runtime UID (10001) or the
  strict reader rejects it.
- `DAC_OVERRIDE` - the target is an fsGroup emptyDir (`root:10001 0770`) and the
  sources are root-owned 0444; a non-root materializer cannot traverse them.
- `FOWNER` - permitted ownership change on the file it just created.

Alternatives, both removing root from the init container: pre-create the target
as the runtime UID so `chown` becomes a no-op, or run the materializer as a
dedicated user that owns both target and projection. Both need a stricter mount
contract than the current compose/kind manifests provide, so this review records
root-with-three-capabilities as the accepted minimum *for the current manifests*
and the alternative as the follow-up when the mounts are reworked. No `SYS_ADMIN`,
no `SETUID`/`SETGID`, no host mounts, no privileged mode.

## P3 (open, recorded)

1. A secret name removed from the source set stays live in the target (no
   deletion path). Deliberate - deleting a secret automatically is more
   dangerous than leaving it - but it must be an explicit operator step.
2. The non-procfs host fallback resolves the source by path (developer macOS
   only; every deployment target is Linux and uses the descriptor).
3. `sync` is a whole-filesystem call in some environments; the journal is short,
   but a per-file fsync helper would be cheaper on NFS.
4. POSIX ACLs on the source are still not inspected; the copy path is the
   mitigation (documented).
5. No hot reload: rotation requires a restart, and this is now stated in the
   guarantees table instead of being implicit.
6. `tests/r15/secret_materialize.sh` still mutates a 0444 source by rewriting it;
   the test works but a failure would report a confusing permission error.

## What was verified against real systems in this session

- Fault injection on a real filesystem, in CI with the real `flock`
  (`materializer-faults` job, run 40458563623): copy, chmod, chown, backup and
  install failures, SIGTERM, SIGKILL recovery, concurrent writers, missing
  `flock`, disk full.
- Real containers (`docker-secret`): 0400/10001:10001, unprivileged read denied,
  non-root read-only runtime, restart, rotation identity, interrupted-transaction
  replay, negative sources.
- Real Kubernetes (`kind-projected-secret`): `..data` projection, init container,
  fsGroup target, credential accepted/rejected across rotation, symlink escape
  refused.
- Real POSIX ACLs (`posix-acl`): granted read ACL observed, unauthorized read and
  write denied, rotation leaves no stale ACL.
