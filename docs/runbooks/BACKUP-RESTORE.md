# Backup / Restore Runbook

Verified in R22 by `tests/r22/running_restore.sh` (32 checks) and
`tests/r22/historical_backup.sh` (34 checks). The first runs in the
`running-restore` CI job; the second runs locally or in CI.

## Backup

    backup -db /app/data/state.sqlite -out-dir /app/backups \
           -encrypt -keyring /run/secrets/onboarding_master_key

- The snapshot is a consistent SQLite copy taken over a read-only connection, so
  it sees committed WAL frames and never mutates the service database.
- `-encrypt` seals it with AES-256-GCM under the keyring current version; the
  file ends in `.sqlite.enc` and the manifest records `encryption`,
  `key_version` and `plaintext_sha256`.
- Every snapshot has `<snapshot>.manifest.json` next to it. A snapshot without
  its manifest is treated as invalid by verify, restore and retention.

Verify before trusting a snapshot (checksums, size, schema version,
`PRAGMA integrity_check`, foreign keys, row census, secret decryption):

    verify -snapshot /app/backups/state-...Z.sqlite.enc \
           -keyring /run/secrets/onboarding_master_key

## Retention

    retention -dir /app/backups -max-age 720h -max-count 30 -min-valid 3 \
              -keyring /run/secrets/onboarding_master_key [-purge-invalid] [-dry-run]

Guarantees:

- the **newest valid** snapshot is never deleted;
- at least `-min-valid` valid recovery points always survive (a value below one
  is treated as one: retention never removes the last recovery point);
- a snapshot is deleted only when it verifies and the limits allow it; an invalid
  snapshot is reported and kept unless `-purge-invalid` is given;
- snapshot and manifest are deleted together; if the snapshot removal fails the
  pair is left consistent and the failure is reported with a non-zero exit;
- each run writes `retention-<UTC>.json` into the snapshot directory
  (temp file + rename), which is the retention audit record.

`-dry-run` prints the same plan with `would-delete` instead of deleting.

## Restore (maintenance window protocol)

Restore never overwrites a database that is open by the service. The protocol is
maintenance gate -> stop -> restore -> start:

1. **Enter maintenance** (admin bearer token):

       curl -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
            -d '{"reason":"restore 2026-01-01"}' https://host/v1/maintenance/enter

   Response: `active=true`, `workers_paused=true`, `drained=true`,
   `writes_in_flight=0`. From this point:
   - mutations return `503` with `Retry-After: 30` and class `MAINTENANCE`;
   - reads keep working;
   - `/readyz` returns `503` with `maintenance: true` so a load balancer
     stops routing new traffic;
   - workflow workers stop claiming steps (in-flight steps finish; leases are
     reclaimed after resume if they expire);
   - `maintenance.enter` is written to the audit log.

   Entering again is idempotent and does not rewrite the open window.

2. **Stop the service** so the database file is not open by anyone:

       docker stop onboardd

3. **Restore over the live path**, keeping the replaced file:

       restore -snapshot /app/backups/state-...Z.sqlite.enc \
               -target /app/data/state.sqlite \
               -live-db /app/data/state.sqlite \
               -keyring /run/secrets/onboarding_master_key -force -keep-old

   The tool verifies the snapshot (checksum, manifest, schema, decryption) and
   refuses to touch the live path without `-force`. The previous database stays
   as `state.sqlite.pre-restore-<timestamp>`.

4. **Start the service** and confirm:

       curl -f https://host/readyz          # 200, ready=true, schema_version=6
       curl -f -H "Authorization: Bearer $TOKEN" https://host/v1/runs

   The window is process local, so a restart always comes back in normal
   operation: a crash during a restore can never leave the service "still in
   maintenance" against a half-replaced database. If the window must be closed
   without a restart, call `/v1/maintenance/exit`: it resumes the workers and
   re-checks database health.

## Failure modes

| Situation | Behaviour | Operator action |
| --- | --- | --- |
| Wrong master key | verify/restore exit non-zero, live database untouched | supply the keyring matching `key_version` |
| Corrupt snapshot | checksum mismatch, exit non-zero, nothing promoted | pick another snapshot (retention keeps the newest valid one) |
| Missing manifest | restore refuses the snapshot | restore the manifest, or use another snapshot |
| Full filesystem | restore fails closed, live database untouched | free space, re-run |
| Interrupted restore | the live path is replaced only after a verified copy; a killed restore leaves the previous database and the `pre-restore` copy intact | re-run the restore |
| Maintenance entered by mistake | writes 503, reads fine | `POST /v1/maintenance/exit` or restart |
| Long-running write during drain | reported in `writes_in_flight`, never killed | wait, then enter again |

## What is still unverified

- SIGKILL in the middle of `restore` has no deterministic injection point (the
  tool has no fault seam), so that window is reasoned about, not tested.
- Disk-full is covered only where the test can mount a small tmpfs (Linux CI as
  root), not on a developer laptop.
