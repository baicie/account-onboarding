# R20 Deployment Plan

1. Verify trusted preflight artifact and exact candidate digest.
2. Provision private data dir/keyring/admin secrets; materialize 0400/0600.
3. Start service, verify schema6/readyz; restore backup only after quiesce.
4. Run smoke tests; monitor outbox/staging.
5. Rollback requires previous image + verified pre-upgrade backup; no schema downgrade.

This plan is not executable authorization.
