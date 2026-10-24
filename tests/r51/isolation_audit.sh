#!/usr/bin/env bash
# Static isolation audit for tests/r51/docker_acceptance.sh (no daemon needed).
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SCRIPT="$ROOT/tests/r51/docker_acceptance.sh"
echo "# R5.1-B isolation audit — $(date -u +%Y-%m-%dT%H:%M:%SZ)"
echo "# script: tests/r51/docker_acceptance.sh"
echo
echo "## 1. destructive/global commands (comments excluded; must be none)"
HITS="$(grep -vE '^[[:space:]]*#' "$SCRIPT" | grep -nE \
  'docker[[:space:]]+(system|container[[:space:]]+prune|volume[[:space:]]+(rm|prune)|network[[:space:]]+(rm|prune))|docker[[:space:]]+rm[[:space:]]+-f[[:space:]]+\$\(' || true)"
if [ -z "$HITS" ]; then echo "(none)"; else echo "$HITS"; exit 1; fi
echo
echo "## 2. all docker invocations (isolated by \$PROJECT/\$IMAGE)"
grep -nE '^[[:space:]]*(docker|[A-Z0-9_]+=.*docker)[^#]*' "$SCRIPT" | grep -vE '^[0-9]+:[[:space:]]*#' \
  | sed -E 's/^([0-9]+:)[[:space:]]*[A-Za-z_][A-Za-z0-9_]*=/\1<var>=/' || true
echo
echo "## 3. isolation identifiers"
grep -nE '^(ID|PROJECT|IMAGE|WORK|PORT|BASE)=|--name|127\.0\.0\.1|\.sandbox/r51' "$SCRIPT" | head -25
echo
echo "## 4. cleanup trap (only owns names from this run / its compose project)"
sed -n '/^cleanup()/,/^}/p' "$SCRIPT"
echo
echo "## 5. compose override (static config, isolated project)"
R51_IMAGE=account-onboarding:audit R51_DATA=/tmp/r51-audit-data \
  R51_MASTER_KEY=/tmp/r51-audit-master.key R51_ADMIN_TOKEN=/tmp/r51-audit-admin.token \
  docker compose -p r51-audit -f "$ROOT/deploy/compose.yaml" -f "$ROOT/tests/r51/compose.override.yaml" config >/dev/null \
  && echo "compose override config: exit 0"
echo "## 6. daemon presence (informational; absence is NOT_RUN, not failure)"
docker info >/dev/null 2>&1 && echo "daemon: available" || echo "daemon: not available (docker info exit 1)"
