#!/usr/bin/env bash
# packages/envoy/scripts/callout-check.test.sh
#
# Proves callout-check.sh's verdicts without a stack: a fake natsprobe and a fake curl on PATH
# answer as the hub would, and an exported stack (DEV_STACK_CALLOUT=1) points the check at them,
# with a sleep standing in for the second broker it kills. callout-check.sh runs each probe under
# env -i, so nothing of this test's environment reaches the fakes; each case instead sets files in
# a control directory the fakes read, baked into them when they are written, and
# CALLOUT_CHECK_GRANT_ALL=1 in a case's environment is the grant-all file: the fixture service
# granting every session's delivery channel.
#
# The fake hub keeps the killed broker's connection in its connection list for 100 ms of its own
# clock after the kill, as a real hub does until it reads that connection's close, and a session
# that connects meanwhile is routed to the killed replica's place in the queue group and refused
# at the hub's authorization timeout (measured on the real stack: a session opened at once after
# the kill was refused at 2 s, and the remaining broker never saw it).
#
# The fakes keep a clock of their own, which only they move (each connection-list read 20 ms, each
# probe 50 ms, a session the dead replica's place takes the hub's 2 s), so the hub's drop comes
# after a count of reads, five, and not after a length of time on this machine. callout-check.sh
# reads no clock: its property is the order of events, the drop and then three admissions, and it
# waits for the drop through scripts/e2e/lib/proof-wait.sh's wait_held, which ends at once where
# the list cannot be read and otherwise at the check's own guard.
#
# It asserts that: (a) a hub that answers as the auth service does passes all six lines, in order,
# ending `all 6 assertions passed`, the second broker is killed, and the check waits until the hub
# drops the killed broker's connection before it opens a session, since a session opened before
# the drop is refused; (b) CALLOUT_CHECK_GRANT_ALL=1, a service that grants every session's
# delivery channel, FAILs the second line naming the channel it was served (the check's negative
# control); (c) a hub whose connection list names nothing reads UNKNOWN on line 5, not a pass;
# (d) a probe that prints nothing reads UNKNOWN; (f) an exported stack missing a variable the check
# needs refuses with exit 2, naming it; (g) a hub that never drops the killed broker's connection
# FAILs line 6 when the check's wait ends, 30 s before its guard; (h) a connection list that
# cannot be read after the kill reads UNKNOWN on line 6; (i) a hub listing one auth service
# connection before the kill reads UNKNOWN on line 6 and kills nothing; and (j) a hub whose list
# is read once after the kill, still naming both connections, and then cannot be read FAILs line 6
# with the count it last read.
set -euo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly check="${project_root}/packages/envoy/scripts/callout-check.sh"
temporary_dir="$(mktemp -d "${TMPDIR:-/tmp}/callout-check-test.XXXXXX")"
readonly temporary_dir
readonly fake_bin_dir="${temporary_dir}/bin"
readonly control_dir="${temporary_dir}/control"
mkdir -p "$fake_bin_dir" "$control_dir"

broker_stand_in=""
cleanup() {
  if [ -n "$broker_stand_in" ] && kill -0 "$broker_stand_in" 2>/dev/null; then
    kill "$broker_stand_in" 2>/dev/null || true
  fi
  rm -rf "$temporary_dir"
}
trap cleanup EXIT

readonly stack_broker="http://127.0.0.1:7001"
readonly session="example-worker-session"

# fake writes the fake command $1 from the body on stdin, after the header every fake shares: the
# control directory it reads. The fakes call one another by name, through the PATH the check runs
# them under, which puts this directory first.
fake() {
  { printf '#!/usr/bin/env bash\ncontrol=%q\n' "$control_dir" && cat; } >"${fake_bin_dir}/$1"
  chmod +x "${fake_bin_dir}/$1"
}

# --- The fakes' clock, in milliseconds: tick moves it on, and the fake hub reads it to know when
# it drops the killed broker's connection. callout-check.sh reads no clock. The fakes are called
# one at a time, so a plain write-and-rename is all tick needs. ---
fake tick <<'EOF'
printf '%s' "$(($(cat "$control/clock") + $1))" >"$control/clock.next"
mv "$control/clock.next" "$control/clock"
EOF

# --- The fake hub's view of the second broker: alive while its stand-in runs (a zombie, which the
# kill leaves until this test's shell reaps it, is dead); once the stand-in is gone, listed while
# the hub still lists its connection (always with no-drop, else for drop-ms of the fake clock from
# the first read that found it gone, which records that time), then dropped. ---
fake hub-state <<'EOF'
broker="$(cat "$control/second-broker")"
state="$(cut -d' ' -f3 "/proc/$broker/stat" 2>/dev/null || true)"
if [ -n "$state" ] && [ "$state" != Z ]; then
  echo alive
  exit 0
fi
now="$(cat "$control/clock")"
if [ ! -e "$control/dead-at.$broker" ]; then
  printf '%s' "$now" >"$control/dead-at.$broker.next"
  mv "$control/dead-at.$broker.next" "$control/dead-at.$broker"
fi
if [ -e "$control/no-drop" ] || [ $((now - $(cat "$control/dead-at.$broker"))) -lt "$(cat "$control/drop-ms")" ]; then
  echo listed
else
  echo dropped
fi
EOF

# --- Fake natsprobe: answers as the hub behind the auth service does. A connection with no
# credential, or with a token from any token endpoint but the stack's broker, is not admitted; the
# session's own channel is served; another session's is refused in nats-server's words, unless the
# control directory says the service grants every channel (grant-all). While the hub still lists
# the killed broker's connection, a session is routed to that replica, which never answers, and
# is not admitted at the hub's authorization timeout. A probe takes 50 ms of the fake clock; with
# silent, it prints nothing; with other-words, a connection the hub does not admit exits 4 in
# words that are not the server's, as natsprobe once did for any failure to connect. ---
fake natsprobe <<'EOF'
subject="" none=false
while [ "$#" -gt 0 ]; do
  case "$1" in
    --subscribe) subject="$2"; shift 2 ;;
    --no-credential) none=true; shift ;;
    *) shift ;;
  esac
done
[ -e "$control/silent" ] && exit 1
state="$(hub-state)"
if [ "$state" = listed ]; then
  tick 2000
  echo "not admitted: Authorization Violation"
  exit 4
fi
tick 50
if [ "$none" = true ] || [ "${AGENT_SECRETS_URL:-}" != "$(cat "$control/stack-broker")" ]; then
  if [ -e "$control/other-words" ]; then
    echo "not admitted: nats: no servers available for connection"
  else
    echo "not admitted: Authorization Violation"
  fi
  exit 4
fi
echo "connected as session/${AGENT_SECRETS_SESSION:-}"
if [ "$subject" = "notifications.envoy.deliver.${AGENT_SECRETS_SESSION:-}" ] || [ -e "$control/grant-all" ]; then
  echo "served $subject"
  exit 0
fi
echo "refused: Permissions Violation for Subscription to \"$subject\""
exit 3
EOF

# --- Fake curl: the hub's connection list, naming Dispatch's and the listener's service users and
# one envoy-broker-callout connection for each broker the hub lists (two, or one with one-broker,
# and one fewer once the killed broker's is dropped); nothing with no-connections; no answer at all
# once the second broker is gone with connz-down-after-kill, or after the first read since then
# with connz-down-late. Each read takes 20 ms of the fake clock, answered or not, so the hub drops
# the killed broker's connection five reads after the first that found it gone. ---
fake curl <<'EOF'
tick 20
if [ -e "$control/no-connections" ]; then
  echo '{"connections": []}'
  exit 0
fi
state="$(hub-state)"
if [ "$state" != alive ]; then
  [ -e "$control/connz-down-after-kill" ] && exit 7
  if [ -e "$control/connz-down-late" ]; then
    [ -e "$control/read-after-kill" ] && exit 7
    : >"$control/read-after-kill"
  fi
fi
callouts=2
[ -e "$control/one-broker" ] && callouts=1
[ "$state" = dropped ] && callouts=$((callouts - 1))
entries='{"name": "dispatch", "authorized_user": "dispatch/dev-stack-dispatch"}, {"name": "listener", "authorized_user": "listener/dev-stack-test"}'
for _ in $(seq 1 "$callouts"); do
  entries="$entries"', {"name": "envoy-broker-callout", "authorized_user": "UAUTHSERVICE"}'
done
echo "{\"connections\": [$entries]}"
EOF
printf '%s' "$stack_broker" >"${control_dir}/stack-broker"
printf '100' >"${control_dir}/drop-ms"
: >"${temporary_dir}/exchange-token"

# fail ends the test, printing the last check's whole output after the reason, its stderr after
# its lines, so a red run in CI says which line failed and how.
fail() {
  echo "FAIL: $*" >&2
  if [ -n "${output:-}" ]; then
    printf 'the check printed:\n%s\n' "$output" >&2
  fi
  if [ -s "${temporary_dir}/check-stderr" ]; then
    printf 'and on stderr:\n%s\n' "$(cat "${temporary_dir}/check-stderr")" >&2
  fi
  exit 1
}

# run_check runs the check against the fakes, with the control files named and the fake clock at a
# time of day, after a fresh second broker stand-in (the last one ended first, since a case can
# leave it running); it sets output to the lines the check prints and status. The check's stderr,
# where its wait says every 60 s that it is still waiting, goes to a file fail prints.
output=""
status=0
run_check() {
  local name
  for name in grant-all no-connections silent other-words no-drop connz-down-after-kill connz-down-late read-after-kill one-broker; do
    rm -f "${control_dir}/${name}"
  done
  if [ "${CALLOUT_CHECK_GRANT_ALL:-}" = 1 ]; then
    : >"${control_dir}/grant-all"
  fi
  for name in "$@"; do
    : >"${control_dir}/${name}"
  done
  printf '1700000000000' >"${control_dir}/clock"
  if [ -n "$broker_stand_in" ] && kill -0 "$broker_stand_in" 2>/dev/null; then
    kill "$broker_stand_in" 2>/dev/null || true
  fi
  sleep 600 &
  broker_stand_in=$!
  disown "$broker_stand_in"
  printf '%s' "$broker_stand_in" >"${control_dir}/second-broker"
  status=0
  output="$(env PATH="${fake_bin_dir}:${PATH}" TMPDIR="$temporary_dir" \
    DEV_STACK_CALLOUT=1 \
    AGENT_SECRETS_URL="$stack_broker" \
    NATS_URLS="nats://127.0.0.1:7002" \
    DEV_STACK_EXCHANGE_TOKEN_FILE="${temporary_dir}/exchange-token" \
    DEV_STACK_EXCHANGE_SESSION="$session" \
    DEV_STACK_BROKER_PIDS="1 $broker_stand_in" \
    DEV_STACK_NATS_MONITOR_URL="http://127.0.0.1:7003" \
    bash "$check" 2>"${temporary_dir}/check-stderr")" || status=$?
}
line() { printf '%s\n' "$output" | sed -n "${1}p"; }

# (a) A hub that answers as the auth service does.
run_check
want=(
  "PASS a session subscribes its own delivery channel"
  "PASS a session is refused another session's delivery channel: Permissions Violation for Subscription to \"notifications.envoy.deliver.${session}-other\""
  "PASS a connection with no credential is refused: Authorization Violation"
  "PASS a token another broker issued is refused: Authorization Violation"
  "PASS the listener is connected by its service token: the hub lists a connection authorized as listener/dev-stack-test"
)
for index in "${!want[@]}"; do
  [ "$(line $((index + 1)))" = "${want[$index]}" ] || fail "line $((index + 1)) is '$(line $((index + 1)))', want '${want[$index]}'"
done
# The fake hub refuses every session opened while it still lists the killed broker's connection,
# so line 6 passes only where the check waited for the drop before it opened a session.
[ "$(line 6)" = "PASS one broker killed, its connection dropped and three new sessions admitted after it: the hub listed 1 envoy-broker-callout connection after the kill, and three sessions opened after that were admitted" ] ||
  fail "line 6 is '$(line 6)'"
[ "$(line 7)" = "all 6 assertions passed" ] && [ "$status" -eq 0 ] || fail "want 'all 6 assertions passed' and exit 0, got exit $status"
[ "$("${fake_bin_dir}/hub-state")" != alive ] || fail "the check did not kill the second broker $broker_stand_in"
echo "PASS: a hub that answers as the auth service does passes all six lines, the second broker is killed, and no session is opened before the hub drops its connection"

# (b) The negative control: a service that grants every session's delivery channel.
CALLOUT_CHECK_GRANT_ALL=1 run_check
[ "$(line 2)" = "FAIL a session is refused another session's delivery channel: it was served notifications.envoy.deliver.${session}-other" ] ||
  fail "with CALLOUT_CHECK_GRANT_ALL=1 line 2 is '$(line 2)'"
[ "$status" -eq 1 ] && [ "$(line 7)" = "1 of 6 assertions failed" ] || fail "with CALLOUT_CHECK_GRANT_ALL=1: exit $status, last line '$(line 7)'"
echo "PASS: CALLOUT_CHECK_GRANT_ALL=1 FAILs line 2, naming the channel it was served, and the run exits 1"

# (c) A connection list that names nothing.
run_check no-connections
[[ "$(line 5)" == "UNKNOWN the listener is connected by its service token: "*"named no connection" ]] || fail "an empty connection list: line 5 is '$(line 5)'"
[ "$status" -eq 1 ] || fail "an empty connection list: exit $status"
echo "PASS: a hub whose connection list names nothing reads UNKNOWN on line 5 and fails the run"

# (d) A probe that prints nothing.
run_check silent
[[ "$(line 1)" == "UNKNOWN a session subscribes its own delivery channel: natsprobe printed nothing"* ]] || fail "a silent probe: line 1 is '$(line 1)'"
[ "$status" -eq 1 ] || fail "a silent probe: exit $status"
echo "PASS: a probe that prints nothing reads UNKNOWN and fails the run"

# (f) An exported stack missing a variable.
status=0
output="$(env PATH="${fake_bin_dir}:${PATH}" TMPDIR="$temporary_dir" DEV_STACK_CALLOUT=1 \
  AGENT_SECRETS_URL="$stack_broker" NATS_URLS="nats://127.0.0.1:7002" \
  DEV_STACK_EXCHANGE_TOKEN_FILE="${temporary_dir}/exchange-token" DEV_STACK_EXCHANGE_SESSION="$session" \
  DEV_STACK_BROKER_PIDS="1 2" bash "$check" 2>&1)" || status=$?
[ "$status" -eq 2 ] && [[ "$output" == *"DEV_STACK_NATS_MONITOR_URL is not set"* ]] ||
  fail "a stack without DEV_STACK_NATS_MONITOR_URL: exit $status"
echo "PASS: an exported stack missing DEV_STACK_NATS_MONITOR_URL is refused with exit 2, naming it"

# (g) A hub that never drops the killed broker's connection: the check's wait ends 30 s before its
# guard, and the line names the count the hub still listed.
run_check no-drop
[[ "$(line 6)" == "FAIL one broker killed, its connection dropped and three new sessions admitted after it: the hub still listed 2 connections named envoy-broker-callout when the wait for the drop ended: gave up waiting for the hub to drop the killed broker's connection after "* ]] ||
  fail "a hub that never drops the killed broker's connection: line 6 is '$(line 6)'"
[ "$status" -eq 1 ] || fail "a hub that never drops the killed broker's connection: exit $status"
echo "PASS: a hub that never drops the killed broker's connection FAILs line 6"

# (h) A connection list that cannot be read once the broker is killed.
run_check connz-down-after-kill
[ "$(line 6)" = "UNKNOWN one broker killed, its connection dropped and three new sessions admitted after it: the hub's connection list (http://127.0.0.1:7003/connz) could not be read after the kill" ] ||
  fail "a connection list unreadable after the kill: line 6 is '$(line 6)'"
[ "$status" -eq 1 ] || fail "a connection list unreadable after the kill: exit $status"
echo "PASS: a connection list that cannot be read after the kill reads UNKNOWN on line 6 and fails the run"

# (i) One auth service connection before the kill: the check cannot see a drop, so it kills nothing.
run_check one-broker
[ "$(line 6)" = "UNKNOWN one broker killed, its connection dropped and three new sessions admitted after it: the hub lists 1 connection named envoy-broker-callout before the kill, not one for each of the two brokers" ] ||
  fail "one auth service connection before the kill: line 6 is '$(line 6)'"
[ "$status" -eq 1 ] || fail "one auth service connection before the kill: exit $status"
[ "$("${fake_bin_dir}/hub-state")" = alive ] || fail "with one auth service connection listed the check killed the second broker anyway"
echo "PASS: a hub listing one auth service connection before the kill reads UNKNOWN on line 6, and nothing is killed"

# (j) A list read once after the kill, naming both connections still, that then cannot be read:
# the check saw the hub keep the killed broker, so it is a FAIL with that count, not an UNKNOWN.
run_check no-drop connz-down-late
[ "$(line 6)" = "FAIL one broker killed, its connection dropped and three new sessions admitted after it: the hub still listed 2 connections named envoy-broker-callout when the wait for the drop ended: the hub to drop the killed broker's connection: the hub's connection list (http://127.0.0.1:7003/connz) could not be read" ] ||
  fail "a list read once after the kill and then unreadable: line 6 is '$(line 6)'"
[ "$status" -eq 1 ] || fail "a list read once after the kill and then unreadable: exit $status"
echo "PASS: a list read once after the kill and then unreadable FAILs line 6 with the count it last read"

# (k) A probe that says "not admitted" in words that are not the server's: lines 3 and 4 hold the
# refusal to nats-server's own "Authorization Violation", as line 2 holds its refusal to the
# server's "Permissions Violation", so a failure to connect at all cannot pass them.
run_check other-words
[[ "$(line 3)" == "FAIL a connection with no credential is refused: natsprobe exit 4: not admitted: nats: no servers available for connection" ]] ||
  fail "a probe not admitted in other words: line 3 is '$(line 3)'"
[[ "$(line 4)" == "FAIL a token another broker issued is refused: natsprobe exit 4: not admitted: nats: no servers available for connection" ]] ||
  fail "a probe not admitted in other words: line 4 is '$(line 4)'"
[ "$status" -eq 1 ] || fail "a probe not admitted in other words: exit $status"
echo "PASS: a probe not admitted in words that are not the server's FAILs lines 3 and 4"
