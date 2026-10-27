# Deployment Runbook

Current R22 is not release-qualified. Preparation only:

1. Verify approved exact digest and trusted preflight evidence.
2. Provision data dir/keyring/admin token with 0400/0600 permissions.
3. Run `onboardd`, verify `/healthz` and authenticated `/readyz`.
4. Apply backups only with service quiesced; verify keyring/checksum.
5. Rollback requires previous image and pre-upgrade backup; schema6 cannot downgrade in place.

## Single-command updates (deployment tooling)

`scripts/update_account_onboarding.sh` wraps the whole lifecycle described above on a
single host. It defaults to `--build-only`, so nothing is recreated unless `--deploy` is
explicit. See `scripts/deploy/README.md` for the full flag list and the enforced safety
rules; the short version:

```bash
./scripts/update_account_onboarding.sh --preflight
./scripts/update_account_onboarding.sh --build-only --expected-sha=<sha>
./scripts/update_account_onboarding.sh --deploy
./scripts/update_account_onboarding.sh --status
./scripts/update_account_onboarding.sh --backup-only
./scripts/update_account_onboarding.sh --rollback
```

The script reuses the shipped backup, verify and materializer tooling; it does not
reimplement snapshotting, and it refuses to deploy when the running container keeps its
database in a different directory than the one being deployed (that would look like a
first install and silently start on an empty database).

The sandbox deployment is exercised end to end by `.github/workflows/deployment-tooling.yml`
on every pull request: first install, verified snapshot, second deploy, wrong-SHA refusal,
hand-written override refusal, rollback, and a database integrity check after each step.
