# Update tool: `scripts/update_account_onboarding.sh`

One command surface for a single-host Docker Compose deployment of Account Onboarding.
It never deploys by accident: the default action only builds.

```bash
./scripts/update_account_onboarding.sh --preflight
./scripts/update_account_onboarding.sh --build-only
./scripts/update_account_onboarding.sh --deploy
./scripts/update_account_onboarding.sh --status
./scripts/update_account_onboarding.sh --rollback
./scripts/update_account_onboarding.sh --backup-only
./scripts/update_account_onboarding.sh --offline --expected-sha=<40-char sha> --build-only
./scripts/update_account_onboarding.sh --deploy --dry-run
```

## What each action does

| Action | Effect |
| --- | --- |
| `--preflight` | docker/compose present, worktree clean, git fetch with retry, SHA resolution, mount + schema + secret + disk checks. Changes nothing. |
| `--build-only` (default) | preflight, build `account-onboarding:local-<short-sha>`, generate the override, validate `docker compose config`. **Never** recreates a container. |
| `--backup-only` | encrypted snapshot with the shipped backup CLI and verify with the shipped verifier. |
| `--deploy` | preflight, snapshot, build, config check, materialize secrets, `compose up -d --no-build --no-deps --force-recreate onboarding`, then health checks, then record state. |
| `--rollback` | returns to the last known good image, or to the release an unfinished deployment attempt replaced when the running container is that attempt; refuses when the running image disagrees with the record, when no schema version is recorded, or when the database schema is newer. |
| `--status` | recorded state plus live container state, `/healthz`, `/readyz`, database schema. |

## Safety rules the script enforces

- Dirty worktree, a diverged checkout, a fetch failure or a wrong `--expected-sha` abort before any build.
- Offline mode requires a full 40-character commit and uses exactly that commit for both build and deploy.
- The existing container mounts are inspected: if the running service keeps its database somewhere else, the script refuses instead of starting on an empty directory and calling it a first install.
- A pending `.restore-in-progress` marker blocks the deploy.
- Every state change is preceded by an encrypted, verified snapshot.
- The generated compose override carries an ownership marker; a hand-written file is never overwritten.
- Secrets are materialized into 0400 files owned by 10001 by the shipped materializer; the service checks are never relaxed.
- `/healthz` alone is not accepted: `/readyz` must also answer 200, and a restart count above 5 is treated as a crash loop.
- Rollback never runs the previous binary against a newer schema unless `--allow-schema-forward` is passed; a database restore stays a separate, explicitly authorized operation.
- No `docker compose down -v`, no path deletion, no `git reset --hard`, no force push.

## Layout

The script operates on a `--root` directory (default `/opt/account-onboarding`) laid out as
`app/` (git checkout), `runtime/data` (SQLite + WAL), `runtime/secrets` (materialized 0400),
`runtime/secrets-src` (0400 source projection, directory 0700), `backups/`, `deploy/` (generated override +
state) and `logs/`. Source and data are separate, so a pull, branch switch or deleted
checkout never touches the database, the keyring or the tokens.

## Build arguments

- `--go-proxy` (default `https://mirrors.tencent.com/go/,https://goproxy.cn,direct`) is passed as the `GOPROXY` build arg.
- `--apt-mirror=<host>` is optional and only ever replaces `deb.debian.org` inside the build stage; TLS stays on and the base image digests are never changed.
- `CGO_ENABLED=1` is required: `go-sqlite3` needs cgo, and the image is verified by the health checks above.
- Image tags are `account-onboarding:local-<short-sha>` and the full commit plus image ID are recorded in `deploy/deployment-state.json`.

This tooling prepares and tests a **sandbox** deployment. It does not confer a production
qualification: `release_authorized=false`, `NOT_PRODUCTION_READY`, and no trusted build
signature is established.
