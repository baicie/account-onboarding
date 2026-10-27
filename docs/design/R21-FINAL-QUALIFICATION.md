# R21 Final Qualification Design

## Trust model

1. Candidate source SHA and artifact bytes are bound by same-artifact identity.
2. Gate evidence is fetched from GitHub Actions API for exact SHA, exact workflow path, event=push, run attempt, conclusion.
3. Publish re-downloads trusted preflight evidence and revalidates commit, gate records, image.tar/image-id/SBOM before any registry login.
4. Workflow **content** trust cannot be derived from the candidate branch itself: a feature commit can modify the workflow file. This requires a protected baseline/ruleset/required-check policy or an operator-signed workflow digest; current stacked branch is **BLOCKED** on that administrative trust root.
5. `release_authorized=false` always; preflight is not release approval.

## Remaining gates

Full browser matrix, Docker/Kind/ACL, ARM64 full, trusted BuildKit/SLSA, historical backup, PR merge dry-run, and protected workflow trust.
