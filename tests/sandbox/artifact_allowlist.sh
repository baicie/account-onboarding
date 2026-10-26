#!/usr/bin/env bash
# Fail if a CI artifact tree contains credentials or non-evidence files.
set -euo pipefail
DIR="${1:?usage: artifact_allowlist.sh DIR}"
[ -d "$DIR" ] || { echo "artifact dir missing" >&2; exit 1; }
bad=0
for pattern in 'admin-token' 'master-key' 'keycloak-admin-password' 'client-secret' 'state.sqlite' '*.lock' '*.env'; do
  matches="$(find "$DIR" -name "$pattern" -print 2>/dev/null || true)"
  if [ -n "$matches" ]; then echo "FORBIDDEN FILE: $matches" >&2; bad=1; fi
done
if find "$DIR" -type f | grep -qE '\.(db|sqlite|sqlite3|pem|key)$'; then
  echo "FORBIDDEN database/key extension" >&2; bad=1
fi
if grep -RInE 'BEGIN (RSA |EC |OPENSSH )?PRIVATE KEY|admin-token|master-key|client_secret|keycloak-admin-password' "$DIR" >/tmp/artifact-secret-hits.txt 2>/dev/null; then
  echo "FORBIDDEN secret-looking content:" >&2; cat /tmp/artifact-secret-hits.txt >&2; bad=1
fi
# Only sanitized evidence JSON plus a generated summary are allowed.
while IFS= read -r f; do
  case "$(basename "$f")" in
    evidence-*.json|summary.txt) ;;
    *) echo "UNEXPECTED ARTIFACT FILE: $f" >&2; bad=1 ;;
  esac
done < <(find "$DIR" -type f)
[ "$bad" = 0 ] || exit 1
echo "artifact allowlist PASS ($(find "$DIR" -type f | wc -l | tr -d ' ') files)"
