# R20 Final Closure Plan / Gap Audit

Status: R20_PARTIAL. No further offline sprint; remaining gaps are explicit.

| # | Gap | Type | Severity | Evidence | Plan | Verification |
|---|---|---|---|---|---|---|
| 1 | Browser admin core was treated as full browser gate | INTEGRATION | P2 | only 4 Playwright tests | full external-ops/alerts/two-context matrix | BROWSER_FULL_MATRIX_PASS |
| 2 | ARM64 service smoke treated as full ARM64 | INFRA | P2 | RUNTIME_SERVICE_PASS only | full QEMU build/migrate/backup/restore/rollback | ARM64_FULL_EMULATED_PASS |
| 3 | Docker/K8s/ACL absent from trusted gates | INFRA | P2 | no CI jobs | Docker/Kind/ACL jobs | three gate markers |
| 4 | Custom candidate provenance not trusted SLSA | SECURITY | P2 | provenance JSON self-built | BuildKit provenance + issuer/identity verification | trusted provenance gate |
| 5 | Trusted API pagination missing | DEFECT | P2 | R19 `per_page=100` | Link pagination, loop/page-failure tests | tests + CI |
| 6 | Publish did not re-verify trusted evidence | SECURITY | P2 | job dependency only | artifact download/digest/gate verification | negative publish test |
| 7 | Workflow event policy inconsistent | DEFECT | P2 | trusted accepted dispatch, wait loop push | push-only trusted gate | tests + workflow |
| 8 | PR stack not integrated | INTEGRATION | P2 | main old, stacked PRs | merge dry-run + runbook | tree/diff evidence |
| 9 | Product UI incomplete | PRODUCT | P2 | no alerts UI/full filters | alerts/runs/ops pages + browser matrix | Playwright |
| 10 | Historical old-binary backup not replayed | RECOVERY | P2 | schema5 fixture reconstructed | old commit backup/replay | historical migration gate |
| 11 | Live providers not authorized | EXTERNAL_AUTH | P1 | Stage A not signed | operator approval package | NOT_RUN |

## Design constraints

- Reuse Go HTTP/SQLite/HTML UI, no new distributed dependencies.
- Trust is derived from GitHub API + exact SHA + workflow path/event; workflow *content* trust still requires a protected baseline/operator approval.
- Release authorization is a separate operator action; `release_authorized=false` always in this round.
- No production credentials, no GHCR push, no tag/release, no merge.

## Execution priority

Trusted gate correctness → release preflight binding → UI/browser → Docker/Kind/ACL → ARM64 → provenance → merge dry-run → review.
