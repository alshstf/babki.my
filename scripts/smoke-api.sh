#!/usr/bin/env bash
# End-to-end API smoke test against a FRESH babki instance.
# Usage: scripts/smoke-api.sh http://localhost:8080
set -euo pipefail
BASE="${1:-http://localhost:8080}"
JAR="$(mktemp)"
STATUS_FILE="$(mktemp)"
trap 'rm -f "$JAR" "$STATUS_FILE"' EXIT

req() { # method path [body] -> body (stdout); status code written to $STATUS_FILE
	# NB: status is written to a file, not a global var, because req() is
	# routinely invoked as `out=$(req ...)` — command substitution runs in a
	# subshell, so a plain `STATUS=...` assignment inside req() would be lost
	# the moment that subshell exits.
	local method="$1" path="$2" body="${3:-}"
	local args=(-s -o /tmp/smoke-body -w '%{http_code}' -X "$method" -b "$JAR" -c "$JAR" "$BASE$path")
	[ -n "$body" ] && args+=(-H 'Content-Type: application/json' -d "$body")
	curl "${args[@]}" >"$STATUS_FILE"
	cat /tmp/smoke-body
}

status() { cat "$STATUS_FILE"; }

expect() { # expected actual message
	if [ "$1" != "$2" ]; then echo "FAIL: $3 (want $1, got $2)"; exit 1; fi
	echo "OK: $3"
}

out=$(req GET /api/v1/setup/status)
expect 200 "$(status)" "setup/status"
if [ "$(echo "$out" | jq -r .setup_needed)" != "true" ]; then
	echo "FAIL: instance is not fresh (setup_needed=false); smoke needs a clean DB"; exit 1
fi

req POST /api/v1/setup '{"space_name":"Smoke","username":"smoke","display_name":"Smoke","password":"smoke1234"}' >/dev/null
expect 201 "$(status)" "setup"

out=$(req GET /api/v1/auth/me)
expect 200 "$(status)" "me"
expect owner "$(echo "$out" | jq -r .role)" "me.role"

out=$(req POST /api/v1/accounts '{"name":"Брокер","type":"brokerage","currency":"RUB","institution":"Т-Банк"}')
expect 201 "$(status)" "create account"
ACC=$(echo "$out" | jq -r .id)

req PUT "/api/v1/accounts/$ACC/balance" '{"as_of":"2026-07-20","amount_minor":100000000}' >/dev/null
expect 200 "$(status)" "set balance"

out=$(req GET /api/v1/summary)
expect 200 "$(status)" "summary"
expect 100000000 "$(echo "$out" | jq -r '.totals[0].net_minor')" "summary.net"

req POST /api/v1/auth/logout >/dev/null
expect 204 "$(status)" "logout"
req GET /api/v1/auth/me >/dev/null
expect 401 "$(status)" "me after logout"

echo "SMOKE PASSED"
