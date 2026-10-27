# Backup / Restore Runbook

- Backup must be encrypted and manifest/checksum verified.
- Restore is not an in-place file overwrite; use maintenance gate/quiesce.
- Wrong master key or corrupt snapshot must fail closed.
- Historical old-binary replay and running restore remain unverified in R22.
