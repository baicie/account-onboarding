# R27 Progress

| Gate | Status | Evidence |
| --- | --- | --- |
| Baseline audit | VERIFIED | main 7a61546; PR #23 merged |
| Docker SIGKILL | NOT_STARTED | needs real Docker fault harness |
| Dual real Deployer | PARTIAL | R26 host flock + stub race; real Docker dual-deployer not run |
| Strict State/Attempt | IMPLEMENTED | JSON schema validation before preflight/rollback/reconcile; corrupt fixtures PASS |
| Rollback fault recovery | PARTIAL | R26 image ID/snapshot checks + existing tests; SIGKILL matrix not run |
| WAL/backup faults | PARTIAL | encrypted WAL concurrent test; full crash matrix not run |
| Trusted build | NOT_VERIFIED | no attestation/digest verification |
| Host security | PARTIAL | lock/secret/path checks; cross-user and child-FD tests not run |
| Staging rehearsal | NOT_STARTED | no isolated pre-production rehearsal |
| Final CI | IN_PROGRESS | Draft PR #24 |

## Commits

- `38325b7` strict state/attempt JSON validation
- baseline/docs pending

## Residual

No real Docker SIGKILL or dual-deployer test. No trusted build attestation. No staging rehearsal. No production authorization.
