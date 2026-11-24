#!/usr/bin/env bash
# packages/envoy/scripts/dev-airlock-lease.test.sh
#
# Proves that the pod session dev-airlock.sh enrols outlives its lease, so a check that holds a
# grant for longer than BROKER_LEASE_SECONDS keeps it. The broker ends a session whose lease
# passes unrenewed, and every grant of that session with it. A worker pod's shim renews its
# session for as long as the pod runs, and dev-airlock.sh runs the same renewer for its own.
#
# It starts the real stack, with one broker and BROKER_LEASE_SECONDS at 91, the shortest lease
# the broker starts with at its default five-second sweep. The run therefore takes minutes,
# where the default lease of 900 seconds would take a quarter of an hour. It asks for a
# 30-minute grant of the stack's stored secret, calls S through the proxy front with the grant's
# placeholder, waits three leases, and calls again. It prints one PASS: or FAIL: line for each
# of these:
#
#   - the broker gave the session the short lease, without which the wait proves nothing;
#   - the first call reaches S with the real value;
#   - the call three leases later reaches S with the real value too, where a session nobody
#     renewed has been ended and the call is refused 407 "grant is expired, revoked, or its
#     session ended";
#   - the session is still the one the stack enrolled, and its lease ends later than its first.
#
# The last line is `dev-airlock-lease: passed` or `dev-airlock-lease: failed (<n> of <m>)`, and
# the exit status is 0 or 1. No line it prints is a credential: a value is compared by its
# SHA-256 (the first 16 hex digits) and its length.
#
# Usage: packages/envoy/scripts/dev-airlock-lease.test.sh
#
# Needs what dev-airlock.sh needs: bash, curl, jq, go and psql, a Postgres server named by
# DEV_BROKER_POSTGRES_URL (or Docker for dev-postgres.sh), and loopback port 443 for the
# stand-in services. It takes the stack's start-up time plus about five minutes.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# The shortest lease config.Load accepts beside the default BROKER_SWEEP_SECONDS of 5
# (leasetime.Production().Shortest, which the broker's refusal names).
LEASE_SECONDS=91
WAIT_SECONDS=$((3 * LEASE_SECONDS))
GRANT_TTL="30m"

WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/dev-airlock-lease.XXXXXX")"
STACK_LOG="$WORK_DIR/stack.log"
STACK_PID=""

cleanup() {
  if [ -n "$STACK_PID" ] && kill -0 "$STACK_PID" 2>/dev/null; then
    kill "$STACK_PID" 2>/dev/null || true
    wait "$STACK_PID" 2>/dev/null || true
  fi
  echo "dev-airlock-lease: work directory kept at $WORK_DIR (rm -rf it once nothing reads it)" >&2
}
trap cleanup EXIT

digest() { printf '%s' "$1" | sha256sum | cut -c1-16; }
shape() { printf 'len=%d sha256=%s' "${#1}" "$(digest "$1")"; }

PASSED=0
FAILED=0
judge() { # <0 for a pass> <property> <what was seen>
  if [ "$1" = 0 ]; then
    PASSED=$((PASSED + 1))
    printf 'PASS: %s — %s\n' "$2" "$3"
  else
    FAILED=$((FAILED + 1))
    printf 'FAIL: %s — %s\n' "$2" "$3"
  fi
}

# --- The stack, with the short lease. dev-airlock.sh starts each broker with its own
# environment and these settings on top, so a BROKER_LEASE_SECONDS set here reaches them. ---
echo "dev-airlock-lease: starting the stack with BROKER_LEASE_SECONDS=$LEASE_SECONDS..." >&2
BROKER_LEASE_SECONDS="$LEASE_SECONDS" bash "$SCRIPT_DIR/dev-airlock.sh" --one-broker >"$STACK_LOG" 2>&1 &
STACK_PID=$!
ready=false
for _ in $(seq 1 1800); do
  if grep -q '^dev-airlock: ready\.$' "$STACK_LOG" 2>/dev/null; then
    ready=true
    break
  fi
  if ! kill -0 "$STACK_PID" 2>/dev/null; then
    break
  fi
  sleep 0.5
done
if [ "$ready" != true ]; then
  echo "dev-airlock-lease: the stack did not come up; see $STACK_LOG" >&2
  tail -n 20 "$STACK_LOG" >&2 || true
  exit 1
fi
eval "$(sed -n 's/^  \(export [A-Z][A-Z_0-9]*=.*\)$/\1/p' "$STACK_LOG")"
echo "dev-airlock-lease: the stack is up (work directory $AIRLOCK_DIR)" >&2

# session_field is one field of the session's own description, which the broker answers from
# its enrollment row: GET /v1/enrollments/self, signed with the session's key.
session_field() { agent-secrets whoami | jq -r --arg f "$1" '.[$f] // ""'; }
seconds_until() { echo $(($(date -u -d "$1" +%s) - $(date -u +%s))); }

first_enrollment="$(session_field enrollment_id)"
first_lease="$(session_field lease_expires_at)"
first_left="$(seconds_until "$first_lease")"
judge "$([ -n "$first_lease" ] && [ "$first_left" -le "$LEASE_SECONDS" ] && echo 0 || echo 1)" \
  "the broker gave the session the short lease" \
  "session $first_enrollment's lease ends at $first_lease, $first_left seconds from now, against BROKER_LEASE_SECONDS=$LEASE_SECONDS"

# --- One grant, released as the CLI releases it, held for the rest of the run. ---
request="$(agent-secrets request AIRLOCK_DEMO_TOKEN --ttl "$GRANT_TTL" --json)"
grant_id="$(printf '%s' "$request" | jq -r '.grant_id // ""')"
if [ -z "$grant_id" ]; then
  echo "dev-airlock-lease: the request was not granted: $(printf '%s' "$request" | jq -r .state)" >&2
  exit 1
fi
values_url="$AGENT_SECRETS_URL/v1/grants/$grant_id/values"
release="$(curl -sS -X POST -H "Proof: $(agent-secrets sign --method POST --url "$values_url")" "$values_url")"
placeholder="$(printf '%s' "$release" | jq -r '.airlock.names.AIRLOCK_DEMO_TOKEN.value // ""')"
proxy_password="$(printf '%s' "$release" | jq -r '.airlock.proxy_password // ""')"
if [ -z "$placeholder" ] || [ -z "$proxy_password" ]; then
  echo "dev-airlock-lease: the release of grant $grant_id carried no placeholder or no proxy password" >&2
  exit 1
fi
demo_value="$(jq -r '.secrets[] | select(.name | endswith("airlock-demo-token")) | .value' "$AIRLOCK_MEMORY_SECRETS_FILE")"

# call_s calls S's /ok through the proxy front with the grant's placeholder, and answers the
# status, or curl's own words when there was none, followed by the Authorization S recorded for
# the call, shaped. The proxy's password and the placeholder reach curl in a config on its
# standard input (-K -), never as arguments, which every process of the machine can read.
call_s() {
  local before status recorded
  before="$(curl -sS "$AIRLOCK_S_RECORDS" | jq 'length')"
  status="$(printf 'proxy = "http://agent-secrets:%s@%s"\nheader = "Authorization: Bearer %s"\n' \
      "$proxy_password" "${AIRLOCK_PROXY_URL#*://}" "$placeholder" |
    curl -sS -K - -o /dev/null -w '%{http_code}' --noproxy '' --cacert "$AIRLOCK_CA_CERT_FILE" \
      "$AIRLOCK_S_URL/ok" 2>&1 | tr '\n' ' ')" || true
  recorded="$(curl -sS "$AIRLOCK_S_RECORDS" | jq -r --argjson n "$before" '.[$n:] | map(.header.Authorization // [] | .[0]) | .[0] // ""')"
  CALL_STATUS="$status"
  CALL_AUTHORIZATION="$recorded"
}

call_s
judge "$([ "$CALL_STATUS" = 200 ] && [ "$CALL_AUTHORIZATION" = "Bearer $demo_value" ] && echo 0 || echo 1)" \
  "the first call reaches S with the real value" \
  "it answered $CALL_STATUS, and S recorded an Authorization of $(shape "$CALL_AUTHORIZATION") against Bearer and the stored value, $(shape "Bearer $demo_value")"

echo "dev-airlock-lease: holding grant $grant_id for $WAIT_SECONDS seconds, three leases..." >&2
sleep "$WAIT_SECONDS"

call_s
judge "$([ "$CALL_STATUS" = 200 ] && [ "$CALL_AUTHORIZATION" = "Bearer $demo_value" ] && echo 0 || echo 1)" \
  "the call three leases later reaches S with the real value" \
  "it answered $CALL_STATUS, and S recorded an Authorization of $(shape "$CALL_AUTHORIZATION") against Bearer and the stored value, $(shape "Bearer $demo_value")"

last_enrollment="$(session_field enrollment_id 2>/dev/null || true)"
last_lease="$(session_field lease_expires_at 2>/dev/null || true)"
judge "$([ -n "$last_lease" ] && [ "$last_enrollment" = "$first_enrollment" ] && [ "$(date -u -d "$last_lease" +%s)" -gt "$(date -u -d "$first_lease" +%s)" ] && echo 0 || echo 1)" \
  "the session is still the one the stack enrolled, and its lease ends later than its first" \
  "the session is ${last_enrollment:-none} against $first_enrollment, and its lease ends at ${last_lease:-no time, the broker answered no session} against $first_lease"

if [ "$FAILED" -ne 0 ]; then
  echo "dev-airlock-lease: failed ($FAILED of $((PASSED + FAILED)))"
  exit 1
fi
echo "dev-airlock-lease: passed"
