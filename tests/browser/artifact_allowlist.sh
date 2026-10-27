#!/usr/bin/env bash
# Only sanitized browser-evidence JSON may be uploaded from browser CI.
set -euo pipefail
DIR="${1:?usage: artifact_allowlist.sh DIR}"
[ -d "$DIR" ] || { echo "artifact dir missing" >&2; exit 1; }
bad=0
while IFS= read -r f; do
  case "$(basename "$f")" in browser-evidence.json) ;; *) echo "UNEXPECTED BROWSER ARTIFACT: $f" >&2; bad=1 ;; esac
done < <(find "$DIR" -type f)
if grep -RIlE '"(cookie|authorization|token|jwt|password|code_verifier|oauth_code|email_body|har|trace)"[[:space:]]*:' "$DIR" >/tmp/browser-evidence-hits.txt 2>/dev/null; then
  echo "FORBIDDEN browser evidence fields (contents not printed):" >&2; sed "s|$DIR/||" /tmp/browser-evidence-hits.txt >&2; bad=1
fi
rm -f /tmp/browser-evidence-hits.txt
[ "$bad" = 0 ] || exit 1
echo "browser artifact allowlist PASS"
