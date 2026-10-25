# R12 CI Root Cause Register

All failures below were observed on R12 branch heads before the final merge-ready
head. Old-SHA failures are diagnostic only; final evidence is the current R12
head (`e0c1944` code, `d9e3ee9` docs-only) and its successful runs.

| Workflow | Job / step | Run | Head | Error | Root cause | Impact | Reproduction | Fix commit | Retest |
|---|---|---|---|---|---|---|---|---|---|
| ci | build/vet/race | [40201087917](https://github.com/baicie/account-onboarding/actions/runs/40201087917) | `881bef7` | Cloudflare tests all fail (`exit status 2`) | `scripts/cloudflare_preflight.sh` command substitution missing closing `"`/`)`; bash parser failed before any test | All Cloudflare negative tests, candidate/staging/supply shellcheck | `bash -n scripts/cloudflare_preflight.sh` → syntax error | `a871d9e` | ci 40208953777 success |
| r12-integration | deploy-recovery | [40201083775](https://github.com/baicie/account-onboarding/actions/runs/40201083775) | `881bef7` | read-only DB directory not fail-closed | Fixture only made the directory read-only; existing SQLite/WAL files owned by UID 10001 remained writable | Test did not inject a real write failure | `go test` irrelevant; CI deploy log | `a871d9e` | deploy 12/12, then R13 16/16 |
| supply-chain | shellcheck | [40201083699](https://github.com/baicie/account-onboarding/actions/runs/40201083699) | `881bef7` | SC1072/SC1073; later `trivy: command not found` | Same unmatched quote; ShellCheck failure skipped Trivy install, causing secondary error | Supply gate failed and scan evidence missing | shellcheck step | `a871d9e` | supply success on e0c1944 |
| release-image | candidate | [40201083705](https://github.com/baicie/account-onboarding/actions/runs/40201083705) | `881bef7` | candidate tests fail | Same Cloudflare script parse failure | Candidate never valid | `go test ./...` | `a871d9e` | candidate success |
| staging-acceptance | staging | [40201083717](https://github.com/baicie/account-onboarding/actions/runs/40201083717) | `881bef7` | offline gate fail | Same script parse failure | staging gate red | targeted test | `a871d9e` | 27/27 + container 63/63 |
| ci | gofmt | [40201087917](https://github.com/baicie/account-onboarding/actions/runs/40201087917) | `881bef7` | gofmt diff | R12 test formatting | gofmt job | `gofmt -l` | `a871d9e` | success |
| R12 review | P2-1 find | n/a | `a871d9e` | direct runbook script exit 126 | Git tree stored scripts as 100644 although runbook executes directly | runbook unusable outside `bash` | `./scripts/cloudflare_preflight.sh --dry-run` | `e0c1944` | direct dry-run 0; executable test |
| R12 review | P2-2 find | n/a | `a871d9e` | claimed negative coverage missing 401/oversize/timeout/authz | Test matrix incomplete | false confidence | review probes | `e0c1944` | Cloudflare test suite success |

Deliberate batching: all same-root-cause fixes were validated locally (`go test`, `bash -n`, targeted Cloudflare) and pushed in one fix commit (`a871d9e`), not one push per workflow.
