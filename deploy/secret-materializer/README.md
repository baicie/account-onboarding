# Secret materialization

Docker and Kubernetes project secrets as world-readable (0444) files or, for
Kubernetes projected volumes, as a `..data` symlink tree. The service accepts
only a regular, non-symlink file with exactly **0400/0600** owned by the runtime
UID (`internal/config/config.go`). Do not weaken that policy; copy the projection
with `scripts/materialize_secrets.sh` in an init container or startup hook:

1. mount the read-only projection at `/run/secrets-source`;
2. give the materializer a private target directory (0700, e.g. a tmpfs);
3. the script validates, stages, journals and renames every file to 0400 with
   the runtime UID/GID;
4. point the `*_FILE` settings at the materialized files.

The service reader is unchanged: the materialized path must stay a real file, not
a symlink. A versioned directory behind a `current` symlink would make the secret
path itself a symlink, and the service rejects that on purpose.

## Transaction protocol

    lock target dir (flock on the directory fd, fail closed)
    replay any interrupted transaction, drop orphaned staging dirs
    validate + copy each source through one open descriptor into a private staging dir
    journal "B <name>"  -> rename live file into the transaction backup dir
    journal "I <name>"   -> rename staged file onto the live path
    drop the journal (commit) and the backups

The journal lives in `<target>/.materialize.txn/journal`. Each record carries a
checksum of its own text, so a torn final line is dropped instead of being
misread; that is safe because a record is written *before* the mutation it
announces.

## Guarantees

| Property | Behaviour |
| --- | --- |
| Single file | Atomic. A reader sees the old bytes or the new bytes, never a partial file: files are staged privately and installed with `rename(2)`. |
| Whole set | Not one filesystem transaction. Each name is versioned on its own and the service validates each file independently; a keyring rotation is expressed inside the master-key payload, so a mixed set is never a corrupt state. Any failure, signal or crash rolls the whole set back. |
| Crash (SIGKILL) | Recovered by the filesystem protocol, not by a trap: the journal survives and the next invocation replays it. |
| Signal (TERM/INT/HUP) | The exit trap replays the journal before exiting with 128+signal. |
| Reader | Sees the previous version of a name until that name is renamed, then the new one. Restart the service to complete a rotation for a running process. |
| Lock | `flock` on the target directory itself. No lock file exists, so there is no lock path to pre-create, symlink or leave stale, and no PID-named file to collide. **Without `flock` the script fails closed.** |
| Concurrency | One writer per target directory. A second materializer refuses immediately instead of interleaving. A killed writer's still-running child may hold an inherited copy of the lock for its remaining lifetime; the next run refuses until then (bounded, never corrupting). |
| Residue | Staging directories are removed on exit and any orphan from a killed run is dropped by the next run (only while holding the lock). The target ends up holding exactly the secret files. |

## Source validation

Every candidate source is opened once and validated **through that descriptor**
(`/proc/self/fd/N` on Linux), so the checks describe the object that is
actually copied and a concurrent replacement of the path cannot redirect them:

- final object must be a regular file;
- no writable mode bits (0222) on the source;
- no hard links (`st_nlink > 1`, an alias is not a trustworthy projection);
- the resolved path must stay inside the source root;
- the copied size must equal the fstat size;
- the file name must match `[A-Za-z0-9_.-]` (no dot files, no separators).

## Kubernetes projected volumes

`--allow-k8s-projection` permits following a source symlink, which is how a
projected volume exposes keys. It is not a general symlink allowance: the link
must resolve to a regular file that stays inside the mounted source root, and the
hard-link, mode and size checks still apply. Everything outside the root is
refused (`refusing source outside the projected root`).

## Privileges

The init container needs `runAsUser: 0` with `CHOWN` (and `DAC_OVERRIDE` /
`FOWNER` when the target is an fsGroup-owned emptyDir) plus
`allowPrivilegeEscalation: false` and `capabilities.drop: [ALL]`. That is the
minimum that can materialize a 0400 file owned by UID 10001 into a root-owned
target:

- `CHOWN` - set the target owner to the runtime UID.
- `FOWNER` - allowed non-root ownership changes on the file it created.
- `DAC_OVERRIDE` - reach a target directory already owned by `root:10001 0770`
  (fsGroup) and read root-owned 0444 sources when the process is not root.
- `SETUID`/`SETGID`/`NET_*`/`SYS_*` are not needed and must stay dropped.

Alternative without root: pre-create the target directory as the runtime UID so
`chown` becomes a no-op and only read access to the source is required, or run
the materializer as a dedicated user that owns both the target and the source
projection. Both remove `CHOWN`/`DAC_OVERRIDE` at the cost of a stricter mount
contract.

## What this script does not do

- It does not inspect POSIX ACLs; the copy path is the mitigation when the
  source ACLs cannot be guaranteed.
- It does not rotate a keyring. Master-key rotation follows the keyring contract
  inside the key file; the admin token is a single credential and rotates by
  replacing the file and restarting the service.
- It does not hot-reload a running process; a rotation is completed by a restart.

## Tests

- `tests/r15/secret_materialize.sh` - unit-level behaviour (modes, rejects, residue).
- `tests/r22/materializer_fault.sh` - real fault injection: copy/chmod/chown/
  backup/install failures, SIGTERM, SIGKILL recovery, concurrent writers, missing
  `flock`, disk full.
- `tests/r22/docker_secret.sh` - real container: 0400 target, non-root reader,
  restart, rotation identity, interrupted-transaction replay.
- `tests/r22/kind_secret.sh` - real cluster: `..data` projection, init container,
  rotation proven by digest plus credential acceptance/rejection, symlink escape
  refusal.
- `tests/r22/acl_secret.sh` - real `getfacl/setfacl` access checks.
