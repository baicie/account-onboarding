# R27 Baseline

- Base main: `7a61546` (PR #23 merged as `f69b67e`; follow-up nginx commit `7a61546`)
- New branch: `feat/v0.1.1-production-readiness`
- New Draft PR: #24
- Existing merged deploy tool already contains: host operation lock, durable attempt/pending state, reconcile, rollback image checks, WAL encrypted backup tests.
- This branch starts from that code; it does not recreate PR #23.
- R27 first slice: strict JSON validation for `deployment-state.json` and `deployment-attempt.json` before preflight/rollback/reconcile.
- Local stub suite: 40 PASS, 0 FAIL; ShellCheck/bash -n PASS.
