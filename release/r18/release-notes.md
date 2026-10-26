# R18 Release Candidate Notes (DRAFT — NOT AUTHORIZED)

Commit: `73fc6aa`
Candidate image ID: `sha256:06fcfc79dcedafa3cd86be17d271f20f078aa6229aab1e1ca1593e15c510e9db`
Candidate tar SHA-256: `be53eae5814f27b424fdb7ce200dc8c581fa8e19ae6de25023a367e10f515772`
Registry test digest: `sha256:2172ff8263b95666d300671462afe984c20909d0fc4fa27b1651fdea64d2bdd9`
Schema: 6

## Verified
- Real Keycloak/Mailpit sandbox 5/5 and real onboardd E2E 5/5, crash/restart, idempotency, cancel.
- Playwright Chromium admin core 4/4.
- Same-artifact 14/14; SBOM/sign/attest; deploy 16/16; ARM64 service smoke.
- Release preflight PASS on this frozen candidate (not a publish authorization).

## Not verified
- Full browser matrix, Docker/K8s/ACL secret projection, full ARM64, native ARM64, trusted BuildKit/SLSA provenance.
- Live Cloudflare/GitHub/WorkBuddy, GHCR publish, release, merge, production.

No operator approval has been generated or assumed. Do not publish.
