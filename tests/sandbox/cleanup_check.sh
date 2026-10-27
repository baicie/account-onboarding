#!/usr/bin/env bash
# Verify the job left no sandbox listeners/processes/temp state behind.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BAD=0
for port in 8180 9000 1025 8025; do
  if command -v lsof >/dev/null 2>&1 && lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then
    echo "LEFTOVER LISTENER on $port" >&2; BAD=1
  fi
done
if [ -d "$ROOT/.sandbox/run" ] && find "$ROOT/.sandbox/run" -type f -name '*.pid' | grep -q .; then
  echo "LEFTOVER PID FILE" >&2; BAD=1
fi
[ "$BAD" = 0 ] || exit 1
echo "sandbox cleanup verified"
