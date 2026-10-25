# Release Provenance & Signing (R11)

- Candidate artifacts bind commit, image ID, image tar SHA-256, SBOM SHA-256,
  base image refs/digests and platform.
- The isolated dry run uses a throwaway local registry and test cosign key:
  manifest digest, registry `Docker-Content-Digest`, content-addressing
  immutability (valid-but-different manifest PUT refused), digest pull, cosign
  sign/verify, attestation push/verify.
- Attestation validation is structural, not string search: exactly one subject
  digest, exact full commit and registry digest inside `predicate.Data`,
  expected predicate type; signatures are verified by cosign first.
- No GHCR image was published; no Production Release/Tag exists. The Publish job
  remains disabled by default and requires a protected Environment plus explicit
  authorization; cosign is pinned and checksum-verified.
- Do not treat local Image ID as a registry digest. Only an actual push yields a
  manifest digest; the dry run proves the mechanics on a throwaway registry.
