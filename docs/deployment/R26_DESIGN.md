# R26 Design — Durable Attempt and Reconcile

## Persisted state

`deployment-state.json` records the last committed image, image ID, commit, schema, and the previous/last-known-good identity. `deployment-attempt.json` records a deploy intent before any Compose mutation: attempt id, status, requested commit, expected image ID, previous identity, snapshot reference, and timestamps. The attempt is written atomically (tmp + rename + sync) while the host operation lock is held.

## Deploy protocol

Lock → preflight/data checks → snapshot + verify → build → override/materialize → persist `mutation_started` → compose up → inspect image → health/ready → write LastKnownGood → write attempt `committed`. A failure after mutation leaves `mutation_started`/`failed` evidence instead of a false success.

## Reconcile

`--reconcile` is a separate explicit action. It requires a persisted `mutation_started` attempt and a running container whose actual image ID equals `expected_image_id`. It refuses on mismatch, requires health/ready and a readable schema, then commits state and marks the attempt committed. `--status` is read-only and reports `INCOMPLETE_ATTEMPT` or `RECOVERY_REQUIRED` when the attempt and live image disagree.

## Rollback

Rollback checks the running image against `current_image_id` and checks the recorded `previous_image_id` against the local image before switching. It takes and verifies a fresh protection snapshot; it never restores a database automatically.

## Boundaries

The macOS PID-directory lock remains a development fallback; Linux `flock` is the production crash-safe lock. Trusted build attestation is still not implemented.
