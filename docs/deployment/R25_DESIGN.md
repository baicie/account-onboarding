# R25 Design — Lock, WAL, Rollback, State

## Operation lock

A host-wide lock protects all mutating actions: deploy, rollback, backup-only, and build-only. The default path is `/var/tmp/account-onboarding-operation.lock`, independent of ROOT, so two roots targeting the fixed Compose project cannot race. Linux uses `flock` on a read-only descriptor; macOS test/dev uses an atomic PID directory with stale-owner detection. Status and preflight remain read-only and do not take the lock. The lock is acquired before preflight/backup/build and released on every shell exit, including failures; flock is released by the kernel on SIGKILL.

## Data/snapshot invariants

- The data directory is `0700:10001`; the deployer may inspect it only through a root/sudo read boundary.
- Empty databases, restore markers, symlinked data paths, and database symlinks are refused before any container is changed.
- Snapshot verification is required before deploy/rollback; rollback now takes a fresh protection snapshot before switching images.
- Ops tools build with `-buildvcs=false` because `/src` is read-only.
- WAL snapshot uses SQLite `VACUUM INTO`; existing concurrent-write and encrypted restore tests cover committed WAL rows.

## Remaining gates

Durable attempt/reconcile state, trusted build attestation, and a full Docker fault matrix remain intentionally separate; this round does not claim them.
