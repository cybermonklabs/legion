#!/usr/bin/env bash
# shellcheck disable=SC2317,SC2329 # stop is invoked by the traps
# packages/envoy/scripts/callout-check.sh
#
# Proves, on the whole local stack behind the NATS auth service (dev-stack.sh --callout: the real
# brokers, listener, Dispatch and nats-server, each the process production runs), what the hub
# lets a connection reach, and prints one line per property:
#
#   1. a session subscribes its own delivery channel;
#   2. a session is refused another session's delivery channel, with nats-server's own
#      "Permissions Violation for Subscription to ..." naming that channel;
#   3. a connection with no credential is refused, with nats-server's own "Authorization
#      Violation";
#   4. a token neither of this stack's brokers issued is refused the same way: a stand-in token
#      endpoint, which answers every trade with a token of its own, plays the other broker;
#   5. the listener is connected by its own service token: the hub's connection list names a
#      connection authorized as the listener's service user, listener/<its machine id>;
#   6. with one of the two brokers killed, the hub drops the killed replica's connection, and with
#      it the replica's place in the auth service's queue group, and three new sessions opened
#      after that drop are each admitted, so no request waits on the dead replica. A session
#      opened before the hub has read that connection's close can be routed to the dead replica and
#      is refused at the hub's authorization timeout (measured), so the check opens the first
#      session only once the hub lists one envoy-broker-callout connection fewer (both replicas
#      carry that name). Nothing here reads a clock: the property is that order of events, the
#      drop and then three admissions.
#
# Each probe is cmd/natsprobe, run with only the variables it needs: as a session, it trades the
# pod's exchange token for a broker access token under the session the stack registered the pod
# for (DEV_STACK_EXCHANGE_SESSION) and presents it with a proof of its key.
#
# It runs against a stack one of two ways. In a shell that exported a callout stack's banner
# (DEV_STACK_CALLOUT=1), against that stack: assertion 6 kills that stack's second broker, which
# the stack outlives. With nothing exported, it starts dev-stack.sh --callout itself, passing its
# own arguments on (--database-url, --binaries; else LEGION_TEST_PG_DSN or DATABASE_URL, as
# dev-stack.sh reads them), drives it, and stops it on exit, which drops its databases.
#
# Every line is PASS, FAIL or UNKNOWN; UNKNOWN is a probe that reported nothing it can read, and
# counts as a failure, since a check that saw nothing has not passed. The last line is
# `all <n> assertions passed`, exit 0, or `<k> of <n> assertions failed`, exit 1. A stack that
# never comes up is exit 2.
#
#   LEGION_TEST_PG_DSN=... bash scripts/callout-check.sh       # starts and stops its own stack
#   bash scripts/callout-check.sh --database-url 'postgres://...'
#   (with a callout stack's banner exported) bash scripts/callout-check.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/callout-check.XXXXXX")"
STACK_PID=""
FOREIGN_PID=""

# The check's one guard and its one wait: scripts/e2e/lib/proof-wait.sh's proof_guard and wait_held,
# which ends at once where its fact can no longer come and otherwise 30 s before the guard, and
# keeps its last poll in WORK_DIR. It waits through wait_held, the returning form of wait_for,
# because this check reports each property on a line of its own and goes on, and a wait that did not
# hold is such a line (or, for the stack itself, exit 2), so each caller says what its wait's end
# means. Guard 220 s: at least twice the longest run of its step's command beside 8 busy loops on 2
# CPUs in a builds Job, 95 s with the stack's start on a warm Go cache (d4-final2, two runs,
# 2026-11-20), plus the wait's 30 s margin; on CI, the longest green run of its step in
# ci-job-envoy-go.yaml was 13 s (run 44598907697), over the 20 newest green merge-group runs of
# ci.yaml that ran it up to 2026-11-19 (runs 44508510162 to 44607131679). A cold build is what
# dev-stack.sh's --binaries spares a run, and a hub that never drops the killed broker
# (callout-check.test.sh's case g) waits the 190 s this leaves.
work=$WORK_DIR
note() { echo "callout-check: $*" >&2; }
# shellcheck source=scripts/e2e/lib/proof-wait.sh
source "$SCRIPT_DIR/../../../scripts/e2e/lib/proof-wait.sh"
proof_guard 220

stop() {
  local rc="$1"
  trap - EXIT
  trap '' INT TERM
  if [ -n "$FOREIGN_PID" ] && kill -0 "$FOREIGN_PID" 2>/dev/null; then
    kill "$FOREIGN_PID" 2>/dev/null || true
    wait "$FOREIGN_PID" 2>/dev/null || true
  fi
  if [ -n "$STACK_PID" ] && kill -0 "$STACK_PID" 2>/dev/null; then
    kill -TERM "$STACK_PID" 2>/dev/null || true
    wait "$STACK_PID" 2>/dev/null || true
  fi
  exit "$rc"
}
trap 'stop "$?"' EXIT
trap 'stop 130' INT
trap 'stop 143' TERM

# --- The stack: the caller's, when it exported one, else one of this run's own. ---
if [ "${DEV_STACK_CALLOUT:-}" != 1 ]; then
  stack_log="$WORK_DIR/dev-stack.log"
  echo "callout-check: starting dev-stack.sh --callout (log $stack_log)..." >&2
  bash "$SCRIPT_DIR/dev-stack.sh" --callout "$@" >"$stack_log" 2>&1 &
  STACK_PID=$!
  # stack_ready: whether dev-stack.sh printed its ready line, for wait_held; a stack that exited
  # never will.
  stack_ready() {
    grep -q '^dev-stack: ready\.' "$stack_log" 2>/dev/null && return 0
    kill -0 "$STACK_PID" 2>/dev/null || wait_impossible="dev-stack.sh exited before it was ready"
    return 1
  }
  if ! wait_held "the stack to come up" stack_ready; then
    cat "$stack_log" >&2
    echo "callout-check: the stack never came up: $wait_gave_up" >&2
    exit 2
  fi
  # The banner's exports, read as values: each `  export NAME=value` line, a quoted value unquoted.
  while IFS= read -r line; do
    name="${line#  export }"
    value="${name#*=}"
    name="${name%%=*}"
    value="${value#\"}"
    value="${value%\"}"
    [ "$name" = PATH ] && value="${value%%:*}:$PATH"
    printf -v "$name" '%s' "$value"
  done < <(sed -n 's/^\(  export [A-Z_0-9]*=.*\)$/\1/p' "$stack_log")
fi
for required in AGENT_SECRETS_URL NATS_URLS DEV_STACK_EXCHANGE_TOKEN_FILE DEV_STACK_EXCHANGE_SESSION \
  DEV_STACK_BROKER_PIDS DEV_STACK_NATS_MONITOR_URL; do
  if [ -z "${!required:-}" ]; then
    echo "callout-check: $required is not set: export a dev-stack.sh --callout banner, or run with none exported" >&2
    exit 2
  fi
done
command -v natsprobe >/dev/null || {
  echo "callout-check: natsprobe is not on PATH; the stack's banner puts its binaries first" >&2
  exit 2
}

SESSION="$DEV_STACK_EXCHANGE_SESSION"
# Another session's delivery channel: the session's own id with a suffix, which no session of
# this stack holds.
OTHER="${SESSION}-other"
OWN_CHANNEL="notifications.envoy.deliver.$SESSION"
OTHER_CHANNEL="notifications.envoy.deliver.$OTHER"

# probe runs natsprobe against the hub as the session, with only the variables a trade reads, and
# with broker naming the token endpoint it trades at; it sets probe_out and probe_rc.
probe_out=""
probe_rc=0
probe() {
  local broker="$1"
  shift
  probe_rc=0
  probe_out="$(env -i PATH="$PATH" HOME="$WORK_DIR" \
    AGENT_SECRETS_URL="$broker" \
    AGENT_SECRETS_MACHINE_IDENTITY=kubernetes \
    AGENT_SECRETS_MACHINE_TOKEN_FILE="$DEV_STACK_EXCHANGE_TOKEN_FILE" \
    AGENT_SECRETS_SESSION="$SESSION" \
    natsprobe --servers "$NATS_URLS" "$@" 2>&1)" || probe_rc=$?
}

failed=0
total=0
pass() {
  total=$((total + 1))
  echo "PASS $1"
}
fail() {
  total=$((total + 1))
  failed=$((failed + 1))
  echo "FAIL $1"
}
unknown() {
  total=$((total + 1))
  failed=$((failed + 1))
  echo "UNKNOWN $1"
}
# first_line_with prints the first line of probe_out that starts with $1, without it.
first_line_with() { printf '%s\n' "$probe_out" | sed -n "s/^$1//p" | head -n1; }

# 1. The session's own channel.
probe "$AGENT_SECRETS_URL" --subscribe "$OWN_CHANNEL" --wait 1s
if [ "$probe_rc" -eq 0 ] && [ "$(first_line_with 'served ')" = "$OWN_CHANNEL" ]; then
  pass "a session subscribes its own delivery channel"
elif [ -z "$probe_out" ]; then
  unknown "a session subscribes its own delivery channel: natsprobe printed nothing (exit $probe_rc)"
else
  fail "a session subscribes its own delivery channel: natsprobe exit $probe_rc: $probe_out"
fi

# 2. Another session's channel, refused by nats-server in its own words.
probe "$AGENT_SECRETS_URL" --subscribe "$OTHER_CHANNEL" --wait 2s
refusal="$(first_line_with 'refused: ')"
want="Permissions Violation for Subscription to \"$OTHER_CHANNEL\""
if [ "$probe_rc" -eq 3 ] && [ "$refusal" = "$want" ]; then
  pass "a session is refused another session's delivery channel: $refusal"
elif [ "$probe_rc" -eq 0 ]; then
  fail "a session is refused another session's delivery channel: it was served $OTHER_CHANNEL"
elif [ -z "$probe_out" ]; then
  unknown "a session is refused another session's delivery channel: natsprobe printed nothing (exit $probe_rc)"
else
  fail "a session is refused another session's delivery channel: natsprobe exit $probe_rc: $probe_out"
fi

# 3. No credential at all, refused by nats-server in its own words: natsprobe exits 4 only for the
# hub's Authorization Violation, and this line holds it to those words too, as line 2 does.
NOT_ADMITTED="Authorization Violation"
probe "$AGENT_SECRETS_URL" --no-credential --subscribe "$OWN_CHANNEL" --wait 1s
if [ "$probe_rc" -eq 4 ] && [ "$(first_line_with 'not admitted: ')" = "$NOT_ADMITTED" ]; then
  pass "a connection with no credential is refused: $NOT_ADMITTED"
elif [ -z "$probe_out" ]; then
  unknown "a connection with no credential is refused: natsprobe printed nothing (exit $probe_rc)"
else
  fail "a connection with no credential is refused: natsprobe exit $probe_rc: $probe_out"
fi

# 4. A token another broker issued: a stand-in token endpoint on loopback answers the trade with a
# token of its own, which the stack's brokers never issued.
foreign_port_file="$WORK_DIR/foreign-port"
python3 - "$foreign_port_file" <<'PY' >"$WORK_DIR/foreign.log" 2>&1 &
import http.server, json, secrets, sys

class Handler(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        self.rfile.read(int(self.headers.get("Content-Length") or 0))
        # The token's field names are built rather than written beside their values: a literal
        # here reads as a credential to the push leak check, and the value is a random string
        # this process makes, which the stack's brokers never issued.
        token_field, kind_field, life_field = "access_" + "token", "token_" + "type", "expires_" + "in"
        body = json.dumps({token_field: "foreign-" + secrets.token_urlsafe(24), kind_field: "DPoP",
                           "issued_" + kind_field: "urn:ietf:params:oauth:token-type:" + token_field,
                           life_field: 900}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass

server = http.server.HTTPServer(("127.0.0.1", 0), Handler)
with open(sys.argv[1], "w") as handle:
    handle.write(str(server.server_address[1]))
server.serve_forever()
PY
FOREIGN_PID=$!
# foreign_listening: whether the stand-in wrote its port, for wait_held; one that exited never will.
foreign_listening() {
  [ -s "$foreign_port_file" ] && return 0
  kill -0 "$FOREIGN_PID" 2>/dev/null || wait_impossible="it exited before it wrote its port"
  return 1
}
if ! wait_held "the stand-in token endpoint's port" foreign_listening; then
  unknown "a token another broker issued is refused: the stand-in token endpoint never started ($wait_gave_up): $(cat "$WORK_DIR/foreign.log")"
else
  probe "http://127.0.0.1:$(cat "$foreign_port_file")" --subscribe "$OWN_CHANNEL" --wait 1s
  if [ "$probe_rc" -eq 4 ] && [ "$(first_line_with 'not admitted: ')" = "$NOT_ADMITTED" ]; then
    pass "a token another broker issued is refused: $NOT_ADMITTED"
  elif [ -z "$probe_out" ]; then
    unknown "a token another broker issued is refused: natsprobe printed nothing (exit $probe_rc)"
  else
    fail "a token another broker issued is refused: natsprobe exit $probe_rc: $probe_out"
  fi
fi

# 5. The listener's own connection, as the hub lists it: authorized as listener/<machine id>.
users="$(curl -sf --max-time 2 "$DEV_STACK_NATS_MONITOR_URL/connz?auth=true&limit=1024" | jq -r '.connections[]?.authorized_user // empty' 2>/dev/null || true)"
listener_user="$(printf '%s\n' "$users" | grep '^listener/' | head -n1 || true)"
if [ -z "$users" ]; then
  unknown "the listener is connected by its service token: the hub's connection list ($DEV_STACK_NATS_MONITOR_URL/connz) named no connection"
elif [ -n "$listener_user" ]; then
  pass "the listener is connected by its service token: the hub lists a connection authorized as $listener_user"
else
  fail "the listener is connected by its service token: the hub lists none authorized as listener/..., only: $(printf '%s' "$users" | tr '\n' ' ')"
fi

# callout_connections prints how many connections the hub's connection list names as the auth
# service's (natscallout's connectionName), or nothing when the list cannot be read inside 2 s.
# nats-server marks a connection closed, and so routes nothing more to its subscriptions
# (client.go, deliverMsg), before it takes the connection off that list (closeConnection,
# removeClient).
callout_connections() {
  curl -sf --max-time 2 "$DEV_STACK_NATS_MONITOR_URL/connz?limit=1024" 2>/dev/null |
    jq -r '[.connections[]? | select(.name == "envoy-broker-callout")] | length' 2>/dev/null || true
}

# 6. One of the two brokers killed, the hub's drop of its connection waited for, then three new
# sessions, each admitted: the order of events the property is, with no clock read.
read -r _ second_broker <<<"$DEV_STACK_BROKER_PIDS"
killed_property="one broker killed, its connection dropped and three new sessions admitted after it"
# hub_dropped: whether the hub lists fewer envoy-broker-callout connections than the $before it
# listed before the kill, for wait_held. It sets listed to each count it reads; a list it cannot
# read ends the wait at once, since this check then sees nothing of the drop it waits for.
listed=""
hub_dropped() {
  local count
  count="$(callout_connections)"
  if [ -z "$count" ]; then
    wait_impossible="the hub's connection list ($DEV_STACK_NATS_MONITOR_URL/connz) could not be read"
    return 1
  fi
  listed=$count
  [ "$count" -lt "$before" ]
}
if [ -z "${second_broker:-}" ] || ! kill -0 "$second_broker" 2>/dev/null; then
  unknown "$killed_property: DEV_STACK_BROKER_PIDS ($DEV_STACK_BROKER_PIDS) names no live second broker"
else
  before="$(callout_connections)"
  if [ -z "$before" ]; then
    unknown "$killed_property: the hub's connection list ($DEV_STACK_NATS_MONITOR_URL/connz) could not be read before the kill"
  elif [ "$before" -lt 2 ]; then
    unknown "$killed_property: the hub lists $before connection named envoy-broker-callout before the kill, not one for each of the two brokers"
  else
    kill -KILL "$second_broker"
    # A list that could not be read at all after the kill says nothing; one read that still named
    # both connections says the hub kept the killed broker, whatever the reads after it answered.
    if ! wait_held "the hub to drop the killed broker's connection" hub_dropped; then
      if [ -z "$listed" ]; then
        unknown "$killed_property: the hub's connection list ($DEV_STACK_NATS_MONITOR_URL/connz) could not be read after the kill"
      else
        fail "$killed_property: the hub still listed $listed connections named envoy-broker-callout when the wait for the drop ended: $wait_gave_up"
      fi
    else
      outcome=pass
      detail=""
      for attempt in 1 2 3; do
        probe "$AGENT_SECRETS_URL" --subscribe "$OWN_CHANNEL" --wait 10ms
        if [ "$probe_rc" -ne 0 ]; then
          outcome=fail
          detail="attempt $attempt: natsprobe exit $probe_rc: $probe_out"
          break
        fi
      done
      if [ "$outcome" = pass ]; then
        pass "$killed_property: the hub listed $listed envoy-broker-callout connection after the kill, and three sessions opened after that were admitted"
      else
        fail "$killed_property: $detail"
      fi
    fi
  fi
fi

if [ "$total" -eq 0 ]; then
  echo "UNKNOWN no assertion ran"
  exit 1
fi
if [ "$failed" -eq 0 ]; then
  echo "all $total assertions passed"
  exit 0
fi
echo "$failed of $total assertions failed"
exit 1
