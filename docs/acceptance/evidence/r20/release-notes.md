# R20 Release Candidate Notes (DRAFT — NOT AUTHORIZED)

Commit: `14ed83334c7801ebe42103d2ab5d12f44e0ba5bb`
Image ID: `sha256:41c57d98c94991668e224a4d74e7d633e14cd3f02dccdd642747447191c7735f`
TAR SHA-256: `d2ce194e77dfde922867697660ce9864767e8749bc245938bdce11dbe76da33f`
Registry digest: `sha256:068c9e6b7d18424e71ed77a6404955e2f0635dbd8daad8ee3015aad37c2e0bbd`
SBOM: `d630f3df2b9a52a0b99b15ad2999be187f8885421a7a77f2b29471bc7f0ccedb`
Schema: 6

Trusted preflight: PASS via GitHub Actions API (push runs, exact workflow paths). Candidate/same-artifact bound.

## Not verified
- Full operator UI/browser matrix, Docker/Kind/ACL, full ARM64, trusted BuildKit/SLSA, historical old-binary backup, PR-stack merge dry-run.
- Live platforms, GHCR publish, release, merge, production.

Workflow-definition trust remains a P2: a feature-branch commit can modify required workflows; exact path/API success is not protected-baseline content proof. `release_authorized=false`.
