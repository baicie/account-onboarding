# Base image governance

R8 supports digest pinning while keeping reproducible, reviewable upgrades.

| Stage | Default tag | Policy |
|---|---|---|
| build | `golang:1.26.10-bookworm` | Go minor/patch must not change in an unrelated PR; base image bumps are separate PRs with compatibility + vulnerability tests |
| runtime | `debian:bookworm-slim` | same; prefer digest pinning after review |

`Dockerfile` exposes `ARG GOLANG_IMAGE` / `ARG RUNTIME_IMAGE`, so a release build
can pin digests without editing the Dockerfile:

```bash
docker build \
  --build-arg GOLANG_IMAGE='golang:1.26.10-bookworm@sha256:<digest>' \
  --build-arg RUNTIME_IMAGE='debian:bookworm-slim@sha256:<digest>' \
  --build-arg VERSION=0.1.0 --build-arg COMMIT="$(git rev-parse HEAD)" \
  -t account-onboarding:r8 .
```

The current `scripts/supply_chain.sh` records the local base image references and
any available `RepoDigests`. A digest must only be recorded as "pinned" when it
came from a real registry resolution (`docker buildx imagetools inspect` or the
pushed image's manifest); otherwise it is `NOT_RUN`, not a PASS.

Scanner availability: `govulncheck`, `syft`, `trivy`, `shellcheck` are run in
`.github/workflows/supply-chain.yml` with pinned versions. Local absence is
reported as NOT_RUN, never as a scan pass.
