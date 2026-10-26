#!/usr/bin/env bash
# Start a real onboardd with fake providers (admin UI only; not a provider E2E)
# and run Playwright against the actual browser.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/r17-browser.XXXXXX")"
PID=""
cleanup(){ [ -z "${PID:-}" ] || kill "$PID" 2>/dev/null || true; rm -rf "$WORK"; }
trap cleanup EXIT
PORT="${ONBOARD_BROWSER_PORT:-8799}"
python3 - "$WORK/master.key" <<'PY'
import base64,os,sys
open(sys.argv[1],'wb').write(base64.b64encode(os.urandom(32)))
PY
chmod 600 "$WORK/master.key"
printf 'browser-e2e-admin-token-%s\n' "$(openssl rand -hex 8)" > "$WORK/admin.token"
chmod 600 "$WORK/admin.token"
export ONBOARD_LISTEN="127.0.0.1:$PORT"
export ONBOARD_DB_PATH="$WORK/state.sqlite"
export ONBOARD_MASTER_KEY_FILE="$WORK/master.key"
export ONBOARD_ADMIN_TOKEN_FILE="$WORK/admin.token"
export ONBOARD_FAKE_PROVIDERS=true
export ONBOARD_DOMAINS=moonbyte.test
export ONBOARD_POLL_INTERVAL=200ms
export ONBOARD_TICK_INTERVAL=50ms
export ONBOARD_LOG_LEVEL=warn
export PLAYWRIGHT_CHANNEL="${PLAYWRIGHT_CHANNEL-chrome}"
export ONBOARD_BROWSER_BASE_URL="http://127.0.0.1:$PORT"
export ONBOARD_TEST_ADMIN_TOKEN="$(cat "$WORK/admin.token")"
(cd "$ROOT" && go build -trimpath -o "$WORK/onboardd" ./cmd/onboardd)
"$WORK/onboardd" >"$WORK/onboardd.log" 2>&1 &
PID=$!
for _ in $(seq 1 100); do curl -sf -m 2 "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1 && break; sleep 0.2; done
curl -sf -m 3 "http://127.0.0.1:$PORT/healthz" >/dev/null
cd "$ROOT/tests/browser"
if [ ! -d node_modules ]; then npm install --no-audit --no-fund >/dev/null; fi
npx playwright test --reporter=line
