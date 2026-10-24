# Release security gate

A release may be called `CONTAINER_RC_READY` only when every executable gate is
PASS on the release commit. Static checks cannot substitute for runtime evidence.

## Required evidence

| Gate | Command | Requirement |
|---|---|---|
| Format/vet/build | `make verify` | exit 0 |
| Full race tests | `go test ./... -race -count=1` | exit 0, 0 FAIL |
| Sandbox E2E | `ONBOARD_SANDBOX_E2E=1 go test ./tests/e2e -v` | 4/4 PASS (local fixture) |
| Container runtime | `tests/r51/docker_acceptance.sh` | exit 0, image ID/platform/health/persistence/migration/fault/backup evidence |
| TLS ingress | `tests/r51/tls_acceptance.sh` | 26/26, test CA only; production ingress separately verified |
| Isolation audit | `tests/r51/isolation_audit.sh` | exit 0, no global destructive commands |
| Compose matrix | `tests/r51/compose_matrix.sh` | exit 0 |
| Secret scan | `scripts/redact_scan.sh --file <evidence...>` | exit 0, 0 findings |
| Backup/restore | `scripts/backup.sh` + `verify_backup.sh` + `restore.sh` | exit 0, restored counts/revocations verified |
| Cloudflare dry-run | `tests/cloudflare/live_preflight.sh` | exit 0 (offline); live **NOT_RUN** without authorization |
| Live platforms | — | Cloudflare/GitHub/WorkBuddy **NOT_RUN** until authorized |

## P0 / P1 policy

- Any P0 (credential leak, arbitrary code/SSRF, cross-host credential forward,
  data loss/corruption, container escape class) blocks the release.
- Any P1 (state-machine inconsistency, lost/duplicated external side effect,
  auth bypass, unrecoverable crash) must be fixed with a regression test and
  re-run the relevant gates.
- P2 requires impact assessment + documented decision; P3 is recorded.

## Static supply-chain checks (current tooling limits)

- `govulncheck`, `syft`, `trivy`, `shellcheck` are **not installed** in this
  environment: those steps are `NOT_RUN` (no installs allowed). When available,
  run them and attach output; do not claim them as done otherwise.
- Reproducible build evidence: `scripts/release_info.sh` (two builds, same sha256).
- Build metadata: version/commit injected via `-ldflags`; `onboardd version` and
  `/v1/meta` (version/mode/schema) expose provenance.

## Sign-off record

Record in the release notes: commit, image ID/digest (or "not pushed"), gate
results with exit codes/evidence paths, open P1/P2 items with owners, and the
final verdict (`CONTAINER_RC_READY` / `LOCAL_RC_READY` / `DEPLOYMENT_BLOCKED`).
`LIVE_READY` requires real-platform evidence and is out of scope until those
authorizations exist.
