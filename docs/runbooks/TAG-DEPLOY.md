# Tag-driven deployment (v0.1 sandbox)

Pushing a `v*` tag deploys the image to the sandbox host. Branch pushes never
deploy. The workflow is `.github/workflows/deploy-release.yml`; the server side is
`deploy/release/docker-compose.deploy.yml` plus `deploy/release/remote-deploy.sh`.

## What a tag run does

1. Resolves the tag, builds the image (`linux/amd64`) and pushes
   `ghcr.io/<owner>/<repo>:<tag>` and `:latest`, recording the OCI digest.
2. Verifies the pushed tag resolves, then SSHes to the host and uploads the compose
   file into `/opt/account-onboarding`.
3. Pulls the image, then runs the shipped `materialize_secrets.sh` as root with only
   `CHOWN`, `DAC_OVERRIDE` and `FOWNER` to turn the 0444 projection into the 0400
   files the service accepts.
4. `docker compose up -d`, then polls `http://127.0.0.1:18788/healthz` for up to 60s.
5. On a failed health check it rolls back to the tag recorded in
   `/opt/account-onboarding/.current-tag` and fails the job. On success it records the
   new tag and prunes images older than a week.

It does not create tags, does not touch other containers on the host, and does not
configure TLS or DNS.

## What the deploy needs on the host

Nothing has to be prepared by hand. On the first deploy the remote script creates
`/opt/account-onboarding/{data,secrets,secrets-src}`, generates the master keyring and
the admin token **on the host** (`openssl rand`, root-owned 0444 projection) and
materializes them into the 0400 files the service accepts. They never leave the host and
the job never prints them.

Requirements:

- `docker` with the compose v2 plugin, and either the deploy user in the `docker` group
  or passwordless sudo (the script checks both and fails with a clear message).
- `openssl` and `curl` on the host.
- The GHCR package must be readable from the host: either make it public, or store a
  `read:packages` token in `/opt/account-onboarding/.ghcr-token` (mode 0600) so the
  script can log in before pulling.

To read the generated admin token later:

```bash
sudo cat /opt/account-onboarding/secrets/onboard_admin_token
```

Optional: to pin the values instead of generating them, write
`onboarding_master_key` (32 raw bytes) and `onboard_admin_token` into
`/opt/account-onboarding/secrets-src/` with mode 0444 before the first deploy. The
script keeps existing files and only generates what is missing, so re-deploys never
rotate a key by accident.

## GitHub configuration

| Kind | Name | Notes |
| --- | --- | --- |
| secret | `DEPLOY_SSH_KEY` | private key for the deploy user. **Preferred.** |
| secret | `DEPLOY_SSH_PASSWORD` | fallback only; requires `sshpass` (installed by the job) |
| var | `DEPLOY_HOST` | default `82.156.234.84` |
| var | `DEPLOY_USER` | default `ubuntu` |
| var | `DEPLOY_PORT` | default `22` |
| var | `ONBOARD_DOMAINS` | default `moonbyte.test` |
| environment | `production-sandbox` | add required reviewers here to gate deploys |

The base image and every run block are pinned in the workflow; the SSH host key is
accepted on first use (`accept-new`). If you need pinning, add the host key to
`known_hosts` in a step and switch the option to `StrictHostKeyChecking=yes`.

## Releasing

```bash
git checkout main && git pull
git tag -a v0.1.0 -m "v0.1.0 sandbox"
git push origin v0.1.0        # this is the deploy trigger
gh run watch                   # or open the Actions tab
```

To redeploy an existing tag (for example after a host change) without creating a new
one, run the workflow manually with that tag as input.

Verification after a successful run:

```bash
ssh ubuntu@82.156.234.84 'curl -fsS http://127.0.0.1:18788/healthz; echo; \
  docker ps --filter name=account-onboarding --format "{{.Status}}"; \
  sudo stat -c "%a %U:%G" /opt/account-onboarding/secrets/*'
```

Expect `200`, an `Up ... (healthy)` container, and `400 10001:10001` for both
materialized secrets.

## Rollback

- Automatic: a failed health check rolls the container back to the previous tag and
  fails the job, so a bad tag never leaves the service down.
- Manual: dispatch the workflow with the previous tag.
- Database: the container keeps `/opt/account-onboarding/data`; a schema-6 database is
  not readable by a pre-schema6 build, so roll back with the snapshot procedure in
  `docs/runbooks/BACKUP-RESTORE.md`, not by downgrading in place.

## Security notes

- Prefer `DEPLOY_SSH_KEY`. Rotate any password that was ever stored in plain text
  (for example the export in `~/Downloads/docker_targets_82.156.234.84.csv`) and
  delete that file: it contains a live credential.
- Secret material never goes to GitHub and is never printed by the job; the
  materializer logs no values and the job only prints modes and digests.
- The container runs as `10001:10001`, read-only root filesystem, `cap_drop: ALL`,
  `no-new-privileges`; only the data directory and the materialized secrets are
  mounted.
- `ONBOARD_FAKE_PROVIDERS=true` and every live provider disabled: this is the v0.1
  sandbox. Nothing here is a production release and no live platform is contacted.

## Limits

- `linux/amd64` only. Add `linux/arm64` to `--platform` if the host is ARM.
- Single node, single SQLite file, no HA.
- TLS, DNS and the reverse proxy are the operator side; this workflow stops at the
  loopback port.
