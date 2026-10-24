# ADR-007 — cgo SQLite driver (`mattn/go-sqlite3`) for v0.1.0

- **Status**: accepted (R0, 2026-10-24)
- **Context**: the design requires a single-instance SQLite (WAL) durable store
  with `BEGIN IMMEDIATE`, `busy_timeout`, `synchronous=FULL` and online backup
  (`VACUUM INTO`). Two practical candidates exist: the pure-Go
  `modernc.org/sqlite` or the cgo `github.com/mattn/go-sqlite3`.
- **Environment finding**: `modernc.org/sqlite` pulls ~100 MB of transitive
  modules; the download stalled (<40 MB after ~20 minutes) on this network. The
  cgo driver downloaded in under a minute and a scratch probe verified WAL,
  `_txlock=immediate`, and transactions on Go 1.25.3/darwin-arm64.
- **Decision**: use `mattn/go-sqlite3` behind the `internal/storage` API. All
  SQLite access is confined to `internal/storage`; the rest of the code only
  sees `database/sql`-shaped repository methods.
- **Consequences**:
  - The container image needs a C toolchain in the build stage (`gcc`,
    `libc6-dev`) and a glibc runtime (Debian slim); both are encoded in the
    root `Dockerfile`. `CGO_ENABLED=1` is exported by the Makefile.
  - Cross-compiling to a different OS/arch requires a matching C toolchain.
  - Swapping to a pure-Go driver later is a local change in
    `internal/storage/store.go` (DSN parameters and `sql.Open` driver name).
  - `VACUUM INTO`, `PRAGMA integrity_check` and WAL semantics are verified in
    storage tests and used by the R4 backup tooling.
