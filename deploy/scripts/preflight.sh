#!/usr/bin/env bash
# R4c deploy preflight: validate the files a compose deployment needs BEFORE
# `docker compose up`, without a Docker daemon and without printing secret
# values.
#
# Checks:
#   1. compose file exists and is readable;
#   2. required secret files exist, are non-empty, are owner-only (0400/0600)
#      and are not the shipped *.example placeholders;
#   3. the master key file is one of: 32 raw bytes, 64 hex chars, base64 of 32
#      bytes, or a JSON keyring envelope {"current":N,"keys":{"1":"<base64>"}};
#   4. the admin token is non-empty (warning when shorter than 16 chars);
#   5. the optional readonly/viewer token, when present, is non-empty and
#      differs from the admin token;
#   6. the data directory exists, is writable, and ownership guidance is printed
#      for the container UID 10001.
#
# Usage:
#   deploy/scripts/preflight.sh [--secrets-dir DIR] [--data-dir DIR]
#                               [--master-key FILE] [--admin-token FILE]
#                               [--readonly-token FILE] [--compose FILE]
#
# Environment fallbacks:
#   DEPLOY_SECRETS_DIR, DEPLOY_DATA_DIR, DEPLOY_MASTER_KEY_FILE,
#   DEPLOY_ADMIN_TOKEN_FILE, DEPLOY_READONLY_TOKEN_FILE, DEPLOY_COMPOSE_FILE
#
# Exit codes: 0 all checks pass, 1 at least one check failed, 2 usage.
# Never prints secret material (only paths, formats and sizes).
set -euo pipefail
export LC_ALL=C

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

SECRETS_DIR="${DEPLOY_SECRETS_DIR:-$DEPLOY_DIR/secrets}"
DATA_DIR="${DEPLOY_DATA_DIR:-$DEPLOY_DIR/data}"
COMPOSE_FILE="${DEPLOY_COMPOSE_FILE:-$DEPLOY_DIR/compose.yaml}"
# Empty means "derive from the final SECRETS_DIR after flag parsing".
MASTER_KEY="${DEPLOY_MASTER_KEY_FILE:-}"
ADMIN_TOKEN="${DEPLOY_ADMIN_TOKEN_FILE:-}"
READONLY_TOKEN="${DEPLOY_READONLY_TOKEN_FILE:-}"

ERRORS=0
WARNINGS=0

usage() {
	cat >&2 <<'EOF'
usage: deploy/scripts/preflight.sh [--secrets-dir DIR] [--data-dir DIR]
                                   [--master-key FILE] [--admin-token FILE]
                                   [--readonly-token FILE] [--compose FILE]

Validates compose secret files, master-key format, admin/viewer tokens and the
SQLite data directory before `docker compose up`. Exit 0 = ready, 1 = problems,
2 = usage. Secret values are never printed.
EOF
}

pass() { echo "PASS  $*"; }
warn() {
	echo "WARN  $*" >&2
	WARNINGS=$((WARNINGS + 1))
}
fail() {
	echo "FAIL  $*" >&2
	ERRORS=$((ERRORS + 1))
}

# file_mode / owner_uid print portable metadata (macOS vs GNU coreutils).
file_mode() {
	case "$(uname -s)" in
	Darwin) stat -f '%Lp' "$1" 2>/dev/null || echo "unknown" ;;
	*) stat -c '%a' "$1" 2>/dev/null || echo "unknown" ;;
	esac
}

owner_uid() {
	case "$(uname -s)" in
	Darwin) stat -f '%u' "$1" 2>/dev/null || echo "unknown" ;;
	*) stat -c '%u' "$1" 2>/dev/null || echo "unknown" ;;
	esac
}

# contains_placeholder reports whether a file still looks like a shipped example.
contains_placeholder() {
	grep -Eq 'REPLACE_ME|REPLACE-WITH|EXAMPLE ONLY|NOT A SECRET' "$1" 2>/dev/null
}

# validate_secret_file label path require_owner_only
validate_secret_file() {
	local label="$1" path="$2" strict="$3" mode
	if [ ! -e "$path" ]; then
		fail "$label missing: $path (copy the matching *.example and replace the placeholder)"
		return 1
	fi
	if [ ! -f "$path" ]; then
		fail "$label is not a regular file: $path"
		return 1
	fi
	if [ ! -s "$path" ]; then
		fail "$label is empty: $path"
		return 1
	fi
	if [ ! -r "$path" ]; then
		fail "$label is not readable by the current user: $path"
		return 1
	fi
	if contains_placeholder "$path"; then
		fail "$label still contains a placeholder: $path (replace it with a real random value)"
		return 1
	fi
	if [ "$strict" = "1" ]; then
		mode="$(file_mode "$path")"
		case "$mode" in
		400 | 600) pass "$label present and owner-only (0$mode): $path" ;;
		*)
			fail "$label permissions are 0$mode, want 0400/0600 (group/other access is not allowed): chmod 600 '$path'"
			;;
		esac
		# The container runs as UID 10001. A host secret owned by another user
		# with mode 0400/0600 is unreadable inside the container unless the
		# runtime/compose maps ownership, so surface it as an actionable warning.
		local owner
		owner="$(owner_uid "$path")"
		if [ "$owner" != "10001" ] && [ "$owner" != "unknown" ]; then
			warn "$label owner UID is $owner; the container runs as 10001 and cannot read a 0400/0600 file owned by $owner (root included): sudo chown 10001:10001 '$path' (Linux), or verify secret ownership mapping in Docker Compose"
		fi
	fi
	return 0
}

# validate_master_key accepts raw 32 bytes, hex, base64 or a JSON keyring.
validate_master_key() {
	local path="$1" size trimmed current keys_line version encoded found_current=0 key_count=0
	size="$(wc -c <"$path" | tr -d '[:space:]')"
	if [ "$size" = "32" ]; then
		pass "master key: 32 raw bytes"
		return 0
	fi
	trimmed="$(sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' "$path" | tr -d '\r')"
	if [ "${#trimmed}" = "32" ]; then
		pass "master key: 32-byte raw text"
		return 0
	fi
	if [ "${#trimmed}" = "64" ] && printf '%s' "$trimmed" | grep -Eq '^[0-9a-fA-F]{64}$'; then
		pass "master key: 64 hex characters (32 bytes)"
		return 0
	fi
	if [ "${#trimmed}" = "44" ] && printf '%s' "$trimmed" | grep -Eq '^[A-Za-z0-9+/]{43}=$'; then
		pass "master key: base64 (32 bytes)"
		return 0
	fi
	if [ "${#trimmed}" = "43" ] && printf '%s' "$trimmed" | grep -Eq '^[A-Za-z0-9+/]{43}$'; then
		pass "master key: base64 raw (32 bytes)"
		return 0
	fi
	if [ "${trimmed:0:1}" = "{" ]; then
		current="$(printf '%s' "$trimmed" | grep -Eo '"current"[[:space:]]*:[[:space:]]*[0-9]+' | grep -Eo '[0-9]+$' | head -n 1 || true)"
		if [ -z "$current" ] || [ "$current" -lt 1 ]; then
			fail "master key: JSON keyring has no positive \"current\" version"
			return 1
		fi
		while IFS= read -r keys_line; do
			[ -n "$keys_line" ] || continue
			version="$(printf '%s' "$keys_line" | grep -Eo '"[0-9]+"' | tr -d '"' | head -n 1)"
			encoded="$(printf '%s' "$keys_line" | sed -E 's/.*:[[:space:]]*"([^"]+)".*/\1/')"
			key_count=$((key_count + 1))
			if [ "$version" = "$current" ]; then
				found_current=1
			fi
			case "${#encoded}" in
			43)
				printf '%s' "$encoded" | grep -Eq '^[A-Za-z0-9+/]{43}$' || {
					fail "master key: JSON keyring version $version is not base64 of 32 bytes"
					return 1
				}
				;;
			44)
				printf '%s' "$encoded" | grep -Eq '^[A-Za-z0-9+/]{43}=$' || {
					fail "master key: JSON keyring version $version is not base64 of 32 bytes"
					return 1
				}
				;;
			*)
				fail "master key: JSON keyring version $version is not base64 of 32 bytes"
				return 1
				;;
			esac
		done < <(printf '%s' "$trimmed" | grep -Eo '"[0-9]+"[[:space:]]*:[[:space:]]*"[A-Za-z0-9+/=_-]+"' || true)
		if [ "$key_count" -lt 1 ]; then
			fail "master key: JSON keyring has no readable keys"
			return 1
		fi
		if [ "$found_current" != "1" ]; then
			fail "master key: JSON keyring current version $current has no key"
			return 1
		fi
		pass "master key: JSON keyring with $key_count version(s), current=$current"
		return 0
	fi
	fail "master key format is invalid: $path (want 32 raw bytes, 64 hex, base64 or JSON keyring)"
	return 1
}

while [ $# -gt 0 ]; do
	case "$1" in
	-h | --help) usage; exit 0 ;;
	--secrets-dir) SECRETS_DIR="$2"; shift 2 ;;
	--data-dir) DATA_DIR="$2"; shift 2 ;;
	--master-key) MASTER_KEY="$2"; shift 2 ;;
	--admin-token) ADMIN_TOKEN="$2"; shift 2 ;;
	--readonly-token) READONLY_TOKEN="$2"; shift 2 ;;
	--compose) COMPOSE_FILE="$2"; shift 2 ;;
	*) echo "preflight: unknown argument: $1" >&2; usage; exit 2 ;;
	esac
done

# Derive per-secret defaults from the final secrets directory unless the caller
# passed an explicit file (or environment override).
if [ -z "$MASTER_KEY" ]; then MASTER_KEY="$SECRETS_DIR/onboarding_master_key"; fi
if [ -z "$ADMIN_TOKEN" ]; then ADMIN_TOKEN="$SECRETS_DIR/onboard_admin_token"; fi
if [ -z "$READONLY_TOKEN" ]; then READONLY_TOKEN="$SECRETS_DIR/onboard_readonly_token"; fi

echo "preflight: deploy dir: $DEPLOY_DIR"
echo "preflight: secrets dir: $SECRETS_DIR"
echo "preflight: data dir:    $DATA_DIR"
echo "preflight: compose:     $COMPOSE_FILE"
echo

# 1) compose file
if [ -f "$COMPOSE_FILE" ] && [ -r "$COMPOSE_FILE" ]; then
	pass "compose file readable: $COMPOSE_FILE"
else
	fail "compose file missing or unreadable: $COMPOSE_FILE"
fi

# 2) secrets dir + master key + admin token
if [ -d "$SECRETS_DIR" ]; then
	pass "secrets directory exists: $SECRETS_DIR"
	validate_secret_file "master key secret" "$MASTER_KEY" 1 || true
	if [ -e "$MASTER_KEY" ] && [ -s "$MASTER_KEY" ] && [ -r "$MASTER_KEY" ] && ! contains_placeholder "$MASTER_KEY"; then
		validate_master_key "$MASTER_KEY" || true
	fi
	if validate_secret_file "admin token secret" "$ADMIN_TOKEN" 1; then
		admin_trimmed="$(tr -d '\r\n' <"$ADMIN_TOKEN")"
		if [ "${#admin_trimmed}" -lt 16 ]; then
			warn "admin token is shorter than 16 characters; use a random token"
		fi
	fi
	# optional readonly/viewer token
	if [ -e "$READONLY_TOKEN" ]; then
		if validate_secret_file "readonly token secret" "$READONLY_TOKEN" 1; then
			admin_value="$(tr -d '\r\n' <"$ADMIN_TOKEN" 2>/dev/null || true)"
			readonly_value="$(tr -d '\r\n' <"$READONLY_TOKEN" 2>/dev/null || true)"
			if [ -n "$admin_value" ] && [ "$admin_value" = "$readonly_value" ]; then
				fail "readonly token must differ from the admin token (ONBOARD_READONLY_TOKEN)"
			else
				pass "readonly token present and differs from admin token"
			fi
		fi
	else
		echo "INFO  optional readonly token not present: $READONLY_TOKEN (viewer role disabled)"
	fi
else
	fail "secrets directory missing: $SECRETS_DIR (mkdir -p '$SECRETS_DIR' and copy deploy/secrets/*.example)"
fi

# 3) data directory
if [ ! -d "$DATA_DIR" ]; then
	fail "data directory missing: $DATA_DIR (mkdir -p '$DATA_DIR' && sudo chown 10001:10001 '$DATA_DIR')"
else
	if [ ! -w "$DATA_DIR" ]; then
		fail "data directory is not writable by the current user: $DATA_DIR (sudo chown 10001:10001 '$DATA_DIR' or run as the deployment owner)"
	else
		probe="$DATA_DIR/.preflight-write-test.$$"
		if (umask 077 && : >"$probe") 2>/dev/null; then
			rm -f "$probe"
			pass "data directory writable: $DATA_DIR"
		else
			fail "data directory is not writable (SQLite needs -wal/-shm there): $DATA_DIR"
		fi
	fi
	dir_owner="$(owner_uid "$DATA_DIR")"
	if [ "$dir_owner" != "10001" ] && [ "$dir_owner" != "unknown" ]; then
		warn "data dir owner UID is $dir_owner; container runs as 10001:10001 (sudo chown 10001:10001 '$DATA_DIR')"
	fi
fi

echo
if [ "$ERRORS" -gt 0 ]; then
	echo "preflight: FAILED with $ERRORS error(s), $WARNINGS warning(s)" >&2
	exit 1
fi
echo "preflight: OK ($WARNINGS warning(s)); safe to run 'docker compose -f '$COMPOSE_FILE' up -d'"
