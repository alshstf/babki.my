#!/usr/bin/env bash
# End-to-end API smoke test against a babki instance.
# Usage: scripts/smoke-api.sh http://localhost:8080
#
# Works against two kinds of instance, auto-detected via
# GET /setup/status:
#   - fresh (setup_needed=true): exercises the onboarding flow itself
#     (POST /setup) under a throwaway "Smoke" space. Additionally proves
#     partial fx conversion (GET /summary's unconverted + total_in_base_minor
#     contract) purely through the public API, by adding a second account in
#     a currency (USD) a fresh instance has no rate for.
#   - pre-seeded via `babki seed` (setup_needed=false): logs in as the demo
#     user instead, and additionally verifies the market-data-backed
#     features seed populates — GET /summary's multi-currency
#     total_in_base_minor (RUB+USD via the seeded fx rate) and GET
#     .../positions' market_value_minor (via the seeded SBER quote) — which
#     a fresh instance has no data to exercise.
#
# SAFETY: a seeded instance is a real, populated one — possibly someone's
# live demo. Running against it logs in as "demo" and creates accounts,
# instruments and operations under that space, which mutates it. That path
# is opt-in only: set SMOKE_ALLOW_SEEDED=1 to allow it. Without that,
# smoke-api.sh refuses to run against anything but a fresh instance.
set -euo pipefail
BASE="${1:-http://localhost:8080}"
JAR="$(mktemp)"
STATUS_FILE="$(mktemp)"
# BODY_FILE is a mktemp under the same trap as the other two, and not the fixed
# /tmp/smoke-body it used to be: a fixed path in a world-writable directory is
# one another user (or a second copy of this script) can own, replace with a
# symlink, or read the session's responses out of.
BODY_FILE="$(mktemp)"
trap 'rm -f "$JAR" "$STATUS_FILE" "$BODY_FILE"' EXIT

req() { # method path [body] -> body (stdout); status code written to $STATUS_FILE
	# NB: status is written to a file, not a global var, because req() is
	# routinely invoked as `out=$(req ...)` — command substitution runs in a
	# subshell, so a plain `STATUS=...` assignment inside req() would be lost
	# the moment that subshell exits.
	local method="$1" path="$2" body="${3:-}"
	local args=(-s -o "$BODY_FILE" -w '%{http_code}' -X "$method" -b "$JAR" -c "$JAR" "$BASE$path")
	[ -n "$body" ] && args+=(-H 'Content-Type: application/json' -d "$body")
	curl "${args[@]}" >"$STATUS_FILE"
	cat "$BODY_FILE"
}

status() { cat "$STATUS_FILE"; }

expect() { # expected actual message
	if [ "$1" != "$2" ]; then echo "FAIL: $3 (want $1, got $2)"; exit 1; fi
	echo "OK: $3"
}

out=$(req GET /api/v1/setup/status)
expect 200 "$(status)" "setup/status"
SETUP_NEEDED="$(echo "$out" | jq -r .setup_needed)"

# Fail closed: anything other than a literal "true" is treated as "not fresh",
# so a malformed body can never let the mutating branch run unguarded.
if [ "$SETUP_NEEDED" != "true" ] && [ "${SMOKE_ALLOW_SEEDED:-}" != "1" ]; then
	echo "FAIL: instance is not fresh; set SMOKE_ALLOW_SEEDED=1 to run against seeded data"
	exit 1
fi

if [ "$SETUP_NEEDED" = "true" ]; then
	req POST /api/v1/setup '{"space_name":"Smoke","username":"smoke","display_name":"Smoke","password":"smoke1234"}' >/dev/null
	expect 201 "$(status)" "setup"
else
	# instance already seeded (`babki seed`) — log in as the demo owner
	# instead of registering a second space (this app is single-tenant: one
	# space per instance, so /setup only ever succeeds once).
	req POST /api/v1/auth/login '{"username":"demo","password":"demo1234"}' >/dev/null
	expect 200 "$(status)" "login (pre-seeded instance)"
fi

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
if [ "$SETUP_NEEDED" = "true" ]; then
	# fresh space: the account just created is the only money in it.
	expect 100000000 "$(echo "$out" | jq -r '.totals[0].net_minor')" "summary.net"
fi
expect RUB "$(echo "$out" | jq -r .base_currency)" "summary.base_currency"
TOTAL_BASE=$(echo "$out" | jq -r '.total_in_base_minor')
if ! [[ "$TOTAL_BASE" =~ ^-?[0-9]+$ ]]; then
	echo "FAIL: summary.total_in_base_minor not numeric (got $TOTAL_BASE)"; exit 1
fi
echo "OK: summary.total_in_base_minor numeric ($TOTAL_BASE)"

if [ "$SETUP_NEEDED" = "true" ]; then
	# Fresh instance, no fx rates at all: a second account in a currency with
	# no rate must (a) show up in summary.unconverted and (b) leave
	# total_in_base_minor exactly where it was with the RUB-only account —
	# proving ConvertMany's partial-conversion contract end to end through
	# the public API alone, no seed data required.
	RUB_ONLY_TOTAL="$TOTAL_BASE"

	out=$(req POST /api/v1/accounts '{"name":"US cash","type":"cash","currency":"USD"}')
	expect 201 "$(status)" "create USD account (fresh, no rate)"
	USD_ACC=$(echo "$out" | jq -r .id)

	req PUT "/api/v1/accounts/$USD_ACC/balance" '{"as_of":"2026-07-20","amount_minor":5000}' >/dev/null
	expect 200 "$(status)" "set USD balance"

	out=$(req GET /api/v1/summary)
	expect 200 "$(status)" "summary after USD account"

	HAS_USD=$(echo "$out" | jq -r '.unconverted | index("USD") != null')
	if [ "$HAS_USD" != "true" ]; then
		echo "FAIL: summary.unconverted does not contain USD (got $(echo "$out" | jq -c .unconverted))"
		exit 1
	fi
	echo "OK: summary.unconverted contains USD (no fx rate on a fresh instance)"

	NEW_TOTAL=$(echo "$out" | jq -r '.total_in_base_minor')
	expect "$RUB_ONLY_TOTAL" "$NEW_TOTAL" "summary.total_in_base_minor unchanged (USD leg excluded, RUB leg only)"
fi

if [ "$SETUP_NEEDED" = "false" ]; then
	# demo's Т-Банк account holds a seeded SBER position with a seeded
	# quote, so it prices with a nonzero market_value_minor — the
	# marketdata.LatestQuotes + marketValue path GET .../positions uses.
	out=$(req GET /api/v1/accounts)
	expect 200 "$(status)" "list accounts (seeded)"
	TBANK=$(echo "$out" | jq -r '.[] | select(.name=="Брокерский Т-Банк") | .id')
	if [ -z "$TBANK" ]; then
		echo "FAIL: seeded account 'Брокерский Т-Банк' not found"; exit 1
	fi

	out=$(req GET "/api/v1/accounts/$TBANK/positions")
	expect 200 "$(status)" "seeded positions"
	MV=$(echo "$out" | jq -r '[.positions[].market_value_minor // 0] | map(select(. != 0)) | .[0] // empty')
	if [ -z "$MV" ]; then
		echo "FAIL: no seeded position with a nonzero market_value_minor"; exit 1
	fi
	echo "OK: seeded position market_value_minor = $MV"
fi

# instrument -> buy -> positions -> delete -> positions empty
out=$(req POST /api/v1/instruments '{"name":"Smoke Instrument","type":"share","currency":"RUB","ticker":"SMOK"}')
expect 201 "$(status)" "create instrument"
INST=$(echo "$out" | jq -r .id)

out=$(req POST /api/v1/operations "{\"account_id\":\"$ACC\",\"instrument_id\":\"$INST\",\"type\":\"buy\",\"occurred_on\":\"2026-07-20\",\"quantity\":\"10\",\"price\":\"100\",\"amount_minor\":-100000,\"currency\":\"RUB\"}")
expect 201 "$(status)" "buy operation"
OP=$(echo "$out" | jq -r .id)

out=$(req GET "/api/v1/accounts/$ACC/positions")
expect 200 "$(status)" "positions after buy"
expect 10 "$(echo "$out" | jq -r '.positions[0].quantity')" "positions.quantity"

req DELETE "/api/v1/operations/$OP" >/dev/null
expect 204 "$(status)" "delete operation"

out=$(req GET "/api/v1/accounts/$ACC/positions")
expect 200 "$(status)" "positions after delete"
expect 0 "$(echo "$out" | jq -r '.positions | length')" "positions.empty"

req POST /api/v1/auth/logout >/dev/null
expect 204 "$(status)" "logout"
req GET /api/v1/auth/me >/dev/null
expect 401 "$(status)" "me after logout"

echo "SMOKE PASSED"
