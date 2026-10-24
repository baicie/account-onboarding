# Supply-chain and image identity policy

## Image identity

- Build locally with an explicit tag: `account-onboarding:<version>-<commit>`.
- Record the **local Image ID** (`docker image inspect --format '{{.Id}}'`).
- A **registry digest** exists only after a push to a registry. This repository
  has not pushed any image (no GHCR, no public registry); do not write a digest
  in reports/notes unless a real `docker push` + `docker inspect --format
  '{{index .RepoDigests 0}}'` produced it.
- Never use `:latest` as the deploy reference; use the immutable tag or digest.
- Base images are currently tag-pinned (`golang:1.25-bookworm`,
  `debian:bookworm-slim`), not digest-pinned (documented P2-6). Before a
  production release, pin `@sha256:...` for both stages and record the digests.

## Build provenance

- `-trimpath` is enabled; build flags inject version/commit
  (`-X .../internal/api.Version`, `.../Commit`).
- `onboardd version` prints version + commit; `go version -m <binary>` lists the
  module build info and VCS revision (when built from a Git checkout).
- `scripts/release_info.sh` builds twice with the same flags and asserts
  identical sha256 (reproducible evidence) and records module metadata.
- SBOM: the Go module list (`go list -m all`, captured in
  `docs/acceptance/evidence/R6-P3-go-modules.json`) is the minimum SBOM input.
  A full SPDX/CycloneDX SBOM requires `syft`/`trivy`, which are not installed
  here → **NOT_RUN**. When available: `syft dir:. -o spdx-json > sbom.spdx.json`.

## Vulnerability scanning

- `govulncheck` is **not installed** in this environment → **NOT_RUN**
  (no installs allowed). Recommended gate before release:
  `go run golang.org/x/vuln/cmd/govulncheck@latest ./...` in a controlled CI.
- Image scanning (`trivy image <ref>`) is **NOT_RUN** until the image is built in
  an environment with the scanner available.

## Signing / verification plan (not yet implemented)

1. Build in CI with a locked toolchain and pinned base-image digests.
2. Generate SBOM (syft) and attach as artifact.
3. Sign the image with cosign keyless or an offline key; publish signature and
   SBOM alongside the digest.
4. Verify signature + SBOM before deploy; record the digest in the release notes.
5. Keep the previous signed digest for rollback.

None of these publishing/signing steps were executed in this repository. Until
they are, the release is a **local** image only.
