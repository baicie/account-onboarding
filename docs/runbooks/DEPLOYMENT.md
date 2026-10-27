# Deployment Runbook

Current R22 is not release-qualified. Preparation only:

1. Verify approved exact digest and trusted preflight evidence.
2. Provision data dir/keyring/admin token with 0400/0600 permissions.
3. Run `onboardd`, verify `/healthz` and authenticated `/readyz`.
4. Apply backups only with service quiesced; verify keyring/checksum.
5. Rollback requires previous image and pre-upgrade backup; schema6 cannot downgrade in place.
