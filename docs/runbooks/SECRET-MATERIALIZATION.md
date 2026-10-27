# Secret Materialization Runbook

- Source tokens are never logged; target 0400/0600, non-root ownership.
- Materializer is atomic; partial temp files must be cleaned.
- Docker/Kind/ACL real acceptance not yet run; do not claim those gates.
