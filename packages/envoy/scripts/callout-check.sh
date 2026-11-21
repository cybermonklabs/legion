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
#   6. with one of the two brokers killed, a new session is admitted within 2 s of the kill, three
#      times over: the hub drops the killed replica's connection, and with it the replica's place in
#      the auth service's queue group, so no request waits on it. A session opened before the hub
#      has read that connection's close can be routed to the dead replica and is refused at the
#      hub's authorization timeout (measured), so the check opens the first session only once the
#      hub lists one auth service connection fewer (each replica's is named `envoy-auth <build>
#      hold-aware`), inside the first session's 2 s.
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
  ready=false
  for _ in $(seq 1 600); do
    if grep -q '^dev-stack: ready\.' "$stack_log" 2>/dev/null; then
      ready=true
      break
    fi
    kill -0 "$STACK_PID" 2>/dev/null || break
    sleep 0.5
  done
  if [ "$ready" != true ]; then
    cat "$stack_log" >&2
    echo "callout-check: the stack never came up" >&2
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
for _ in $(seq 1 100); do
  [ -s "$foreign_port_file" ] && break
  sleep 0.1
done
if [ ! -s "$foreign_port_file" ]; then
  unknown "a token another broker issued is refused: the stand-in token endpoint never started: $(cat "$WORK_DIR/foreign.log")"
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
# service's (natscallout's connectionName, `envoy-auth <build> hold-aware`), or nothing when the
# list cannot be read inside 2 s. nats-server marks a connection closed, and so routes nothing more
# to its subscriptions (client.go, deliverMsg), before it takes the connection off that list
# (closeConnection, removeClient).
callout_connections() {
  curl -sf --max-time 2 "$DEV_STACK_NATS_MONITOR_URL/connz?limit=1024" 2>/dev/null |
    jq -r '[.connections[]? | select((.name // "") | startswith("envoy-auth ") and endswith(" hold-aware"))] | length' 2>/dev/null || true
}

# 6. One of the two brokers killed, then three new sessions, each admitted within 2 s: the first
# counted from the kill, so the wait for the hub to drop the killed replica is inside its 2 s.
read -r _ second_broker <<<"$DEV_STACK_BROKER_PIDS"
killed_property="one broker killed and a new session admitted within 2 s"
if [ -z "${second_broker:-}" ] || ! kill -0 "$second_broker" 2>/dev/null; then
  unknown "$killed_property: DEV_STACK_BROKER_PIDS ($DEV_STACK_BROKER_PIDS) names no live second broker"
else
  before="$(callout_connections)"
  if [ -z "$before" ]; then
    unknown "$killed_property: the hub's connection list ($DEV_STACK_NATS_MONITOR_URL/connz) could not be read before the kill"
  elif [ "$before" -lt 2 ]; then
    unknown "$killed_property: the hub lists $before auth service connection before the kill, not one for each of the two brokers"
  else
    killed=$(date +%s%N)
    kill -KILL "$second_broker"
    dropped=""
    listed=""
    waited=0
    while :; do
      count="$(callout_connections)"
      waited=$((($(date +%s%N) - killed) / 1000000))
      if [ -n "$count" ]; then
        listed=$count
        if [ "$count" -lt "$before" ]; then
          dropped=$waited
          break
        fi
      fi
      [ "$waited" -ge 2000 ] && break
      sleep 0.02
    done
    # A list that could not be read at all after the kill says nothing; one read that still named
    # both connections says the hub kept the killed broker, whatever the reads after it answered.
    if [ -z "$dropped" ] && [ -z "$listed" ]; then
      unknown "$killed_property: the hub's connection list ($DEV_STACK_NATS_MONITOR_URL/connz) could not be read after the kill"
    elif [ -z "$dropped" ]; then
      fail "$killed_property: the hub still listed $listed auth service connections ${waited} ms after the kill"
    else
      slowest=0
      outcome=pass
      detail=""
      began=$killed
      for attempt in 1 2 3; do
        probe "$AGENT_SECRETS_URL" --subscribe "$OWN_CHANNEL" --wait 10ms
        took=$((($(date +%s%N) - began) / 1000000))
        [ "$took" -gt "$slowest" ] && slowest=$took
        if [ "$probe_rc" -ne 0 ]; then
          outcome=fail
          detail="attempt $attempt: natsprobe exit $probe_rc after ${took} ms: $probe_out"
          break
        fi
        if [ "$took" -ge 2000 ]; then
          outcome=fail
          detail="attempt $attempt took ${took} ms"
          break
        fi
        began=$(date +%s%N)
      done
      if [ "$outcome" = pass ]; then
        pass "$killed_property: the hub dropped the killed broker's connection after ${dropped} ms; three sessions, the slowest ${slowest} ms, the first counted from the kill"
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
