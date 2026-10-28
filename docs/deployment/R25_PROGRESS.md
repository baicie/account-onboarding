# R25 Progress

Start HEAD: `ebda458` (R24 final `bad803f` plus follow-up)
Current verified HEAD before docs: `3d91883`
PR: #23 Draft, all checks SUCCESS/CLEAN

| Gate | Status | Test |
| --- | --- | --- |
| Operation Lock | IMPLEMENTED / PARTIAL | two-process stub race PASS; real Docker E2E serial operations PASS |
| WAL Backup/Restore | VERIFIED（核心） | WAL concurrent snapshot + encrypted restore test PASS |
| Rollback Safety | PARTIAL | fresh protection snapshot added; A→B→A E2E PASS |
| Durable State | NOT_STARTED | atomic rename existing; no reconcile protocol |
| Trusted Build | NOT_VERIFIED | no attestation/provenance; tags not treated as proof |
| Docker Fault Matrix | PARTIAL | real E2E covers first install, upgrade, wrong SHA, override, DB survival |
| Secret/Permission | VERIFIED（现有） | 0400 secrets, 0700 data, loopback, hardening inspect |
| Operator Diagnostics | PARTIAL | health failure diagnostics and status present |
| CI/Test Harness | VERIFIED | 28 stub scenarios PASS; all PR checks green |

## Commits added

- `f7e207d` crash-safe host operation lock
- `33b16c1` encrypted WAL snapshot restore test
- `13bfb17` verified protection snapshot before rollback
- `3d91883` follow-up port/secret hardening

## Residual

- Lock has not been exercised against two real Docker deployers; only two-process stub and sequential real E2E.
- Full WAL fault matrix, state reconcile, attestation, and production runbook remain open.
