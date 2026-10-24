# First-deploy checklist (private / self-hosted)

Nothing in this checklist has been executed against production. It is the
operator procedure for a private deployment after the container and TLS gates
are actually run.

## 0. Release identity

- [ ] Git commit recorded (`git rev-parse HEAD`); binary reports it (`onboardd version`).
- [ ] Image built with `--build-arg VERSION=... --build-arg COMMIT=...`; Image ID
      recorded; digest recorded **only if pushed** to a registry (never invent one).
- [ ] `docs/acceptance/FINAL-REPORT.md` verdict is understood; container/TLS
      gates are PASS, not NOT_RUN.
- [ ] `make verify` and `go test ./... -race -count=1` on the release commit.

## 1. Host and files

- [ ] Docker/Compose available; loopback-only port `127.0.0.1:8788` free.
- [ ] `deploy/data` exists and is writable by container UID **10001**
      (`sudo install -d -o 10001 -g 10001 -m 0750 deploy/data`).
- [ ] Secret files exist, non-empty, `0400/0600`, **readable by UID 10001**
      (Linux: `sudo chown 10001:10001`; verify the compose/secret ownership
      mapping). `.example` placeholders are not used.
- [ ] Master key: raw 32B / hex / base64 / JSON keyring; a JSON keyring is
      required for rotation. Keyring stored separately from snapshots.
- [ ] `deploy/scripts/preflight.sh` exits 0 (run it before every `up`).
- [ ] Snapshot directory exists and is on a separate volume with retention.

## 2. Configuration

- [ ] `ONBOARD_MODE=production` (or documented sandbox for a test host).
- [ ] `ONBOARD_TRUST_PROXY_HEADERS=1` **only** behind the TLS proxy; the app port
      is reachable only from the proxy/loopback.
- [ ] `ONBOARD_ALLOW_PUBLIC_LISTEN=1` inside the container only because the host
      publish is `127.0.0.1`; never change the publish without a reviewed TLS proxy.
- [ ] Admin token strong/unique; viewer token set only if a read-only dashboard is
      needed and differs from admin.
- [ ] Live providers stay disabled until their authorization/live gates are
      completed (Cloudflare/GitHub/WorkBuddy default false).
- [ ] Rate limits appropriate for the deployment (defaults 300/min general, 10/min login).

## 3. Start and verify

- [ ] `docker compose -f deploy/compose.yaml config` exit 0.
- [ ] `docker compose -f deploy/compose.yaml up -d`; container runs as 10001,
      read-only rootfs, cap_drop ALL, healthcheck **healthy**.
- [ ] `/healthz` 200 via loopback; `/readyz` 200 with token.
- [ ] `docker inspect` shows Image ID/digest recorded; `docker ps` shows exactly
      one writer instance for the DB volume.
- [ ] Snapshot + verify immediately after the first successful boot; record
      schema version (expect 3).

## 4. TLS and exposure

- [ ] Reverse proxy terminates TLS 1.2+ with a valid certificate; HTTP redirects
      to HTTPS; HSTS enabled after verification.
- [ ] Cookie on login is `Secure`, `HttpOnly`, `SameSite=Strict`.
- [ ] Proxy strips client `Forwarded`/`X-Forwarded-*`; app sees only proxy values.
- [ ] Direct access to `127.0.0.1:8788` is possible only on the host/private net;
      no public port on the app container.
- [ ] `curl` from an external host reaches only the proxy; `/readyz` is not exposed publicly.

## 5. Operations

- [ ] Backup schedule + restore drill scheduled; runbook owner named.
- [ ] Log aggregation redacts Authorization/Cookie; no secrets in logs.
- [ ] Audit log retention reviewed (90 days default).
- [ ] Upgrade/rollback runbook rehearsed; previous image tag and snapshot retained.
- [ ] Monitoring: container health, `/readyz`, disk space for data+backups, cert expiry.
- [ ] Security gate (`docs/deployment/security-gate.md`) signed off.
