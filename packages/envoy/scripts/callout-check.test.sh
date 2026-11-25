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
# The fake hub keeps the killed broker's connection in its connection list for 300 ms after the
# kill, as a real hub does until it reads that connection's close, and a session that connects
# meanwhile is routed to the killed replica's place in the queue group and refused at the hub's
# 2 s authorization timeout (measured on the real stack: a session opened at once after the kill
# was refused at 2 s, and the remaining broker never saw it).
#
# Time is the fakes' own: callout-check.sh reads it only as `date +%s%N`, and a fake date answers
# from a clock that only the fakes move (each connection-list read 20 ms, each probe 50 ms, a
# session the dead replica's place takes the hub's 2 s, a slow one 2.5 s). So the verdicts do not
# depend on how fast this machine starts a process: under a load of about 1000 on 288 CPUs a wall
# clock let one run in 20 take 5 s for the fakes alone. For the same reason the check's own pause
# between two reads of the list, 20 ms of wall time by default, is none here
# (CALLOUT_CHECK_POLL_SECONDS=0): the fake read stands for it on the fake clock, and a real pause
# would only add 2 s of waiting to each case that reads the list for the check's whole 2 s.
#
# It asserts that: (a) a hub that answers as the auth service does passes all six lines, in order,
# ending `all 6 assertions passed`, the second broker is killed, and the check waits until the hub
# drops the killed broker's connection before it opens a session; (b) CALLOUT_CHECK_GRANT_ALL=1,
# a service that grants every session's delivery channel, FAILs the second line naming the channel
# it was served (the check's negative control); (c) a hub whose connection list names nothing
# reads UNKNOWN on line 5, not a pass; (d) a probe that prints nothing reads UNKNOWN; (e) an
# admission that takes longer than 2 s after the kill FAILs line 6; (f) an exported stack missing
# a variable the check needs refuses with exit 2, naming it; (g) a hub that never drops the killed
# broker's connection FAILs line 6 at the check's bound, 2000 ms after the kill; (h) a connection
# list that cannot be read after the kill reads UNKNOWN on line 6; (i) a hub listing one auth
# service connection before the kill reads UNKNOWN on line 6 and kills nothing; (j) a hub whose
# list is read once after the kill, still naming both connections, and then cannot be read FAILs
# line 6 with the count it last read, at that bound; (k) a probe that says "not admitted" in words
# that are not the server's FAILs lines 3 and 4; (l) with no stack exported, a --database-url
# reaches the dev-stack.sh the check starts as LEGION_TEST_PG_DSN, never in its arguments; and (m)
# brokers whose connections name their build after envoy-broker-callout pass line 6, as the bare
# name does.
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
# The machine the stack's metadata stand-in answers for (DEV_STACK_MACHINE_ACCOUNT) and the Google
# audience its brokers verify a service's identity token at (DEV_STACK_NATS_GOOGLE_AUDIENCE), as
# dev-stack.sh --callout exports them.
readonly machine_account="example-fleet@example-project.iam.gserviceaccount.com"
readonly nats_google_audience="dev-stack-machine"

# fake writes the fake command $1 from the body on stdin, after the header every fake shares: the
# control directory it reads. The fakes call one another by name, through the PATH the check runs
# them under, which puts this directory first.
fake() {
  { printf '#!/usr/bin/env bash\ncontrol=%q\n' "$control_dir" && cat; } >"${fake_bin_dir}/$1"
  chmod +x "${fake_bin_dir}/$1"
}

# --- The fake clock, in milliseconds: date answers `date +%s%N` from it and refuses any other
# call, since the check reads time in no other form; tick moves it on. The fakes are called one at
# a time, so tick writes the clock in place, and every fake reads its control files with the
# shell's own read: the check reads the hub's list a hundred times in a case that waits out its
# 2 s, and a cat or a cut per read would start that many more processes. ---
fake date <<'EOF'
if [ "$#" -ne 1 ] || [ "$1" != +%s%N ]; then
  echo "fake date: called as date $*; callout-check.sh reads time only as date +%s%N" >&2
  exit 2
fi
read -r now <"$control/clock"
echo "${now}000000"
EOF
fake tick <<'EOF'
read -r now <"$control/clock"
printf '%s' "$((now + $1))" >"$control/clock"
EOF

# --- The fake hub's view of the second broker: alive while its stand-in runs (a zombie, which the
# kill leaves until this test's shell reaps it, is dead); once the stand-in is gone, listed while
# the hub still lists its connection (always with no-drop, else for drop-ms of the fake clock from
# the first read that found it gone, which records that time), then dropped. ---
fake hub-state <<'EOF'
read -r broker <"$control/second-broker"
state=""
{ read -r _ _ state _ <"/proc/$broker/stat"; } 2>/dev/null
if [ -n "$state" ] && [ "$state" != Z ]; then
  echo alive
  exit 0
fi
read -r now <"$control/clock"
if [ ! -e "$control/dead-at.$broker" ]; then
  printf '%s' "$now" >"$control/dead-at.$broker"
fi
read -r dead_at <"$control/dead-at.$broker"
read -r drop_ms <"$control/drop-ms"
if [ -e "$control/no-drop" ] || [ $((now - dead_at)) -lt "$drop_ms" ]; then
  echo listed
else
  echo dropped
fi
EOF

# --- Fake natsprobe: answers as the hub behind the auth service does. A connection with no
# credential, or with a token from any token endpoint but the stack's broker, is not admitted; the
# session's own channel is served; another session's is refused in nats-server's words, unless the
# control directory says the service grants every channel (grant-all). A service naming the
# stack's Google audience (--service-google-audience) is admitted as the machine its metadata
# stand-in answers for, and one naming another audience, or run with no metadata server to read
# its token from, is not: those are the two ways the broker's google entry could be missing.
# While the hub still lists the killed broker's connection, a session is routed to that replica,
# which never answers, and is not admitted at the hub's authorization timeout. A probe takes 50 ms
# of the fake clock; with silent, it prints nothing; with slow, it takes 2.5 s once the second
# broker is gone; with other-words, a connection the hub does not admit exits 4 in words that are
# not the server's, as natsprobe once did for any failure to connect; with google-refused the
# Google service is not admitted, and with google-silent it prints nothing. ---
fake natsprobe <<'EOF'
subject="" none=false google=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --subscribe) subject="$2"; shift 2 ;;
    --no-credential) none=true; shift ;;
    --service-google-audience) google="$2"; shift 2 ;;
    *) shift ;;
  esac
done
if [ -n "$google" ]; then
  [ -e "$control/google-silent" ] && exit 1
  tick 50
  if [ -e "$control/google-refused" ] || [ "$google" != "$(cat "$control/nats-google-audience")" ] ||
    [ -z "${GCE_METADATA_HOST:-}" ]; then
    echo "not admitted: Authorization Violation"
    exit 4
  fi
  echo "connected as service $(cat "$control/machine-account")"
  echo "served $subject"
  exit 0
fi
[ -e "$control/silent" ] && exit 1
state="$(hub-state)"
if [ -e "$control/slow" ] && [ "$state" != alive ]; then
  tick 2500
  state="$(hub-state)"
fi
if [ "$state" = listed ]; then
  tick 2000
  echo "not admitted: Authorization Violation"
  exit 4
fi
tick 50
read -r stack_broker <"$control/stack-broker"
if [ "$none" = true ] || [ "${AGENT_SECRETS_URL:-}" != "$stack_broker" ]; then
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

# --- Fake curl: the hub's connection list, naming Dispatch's and the listener's service users, a
# connection with no name (which nats-server lists with no name field), and one
# envoy-broker-callout connection for each broker the hub lists (two, or one with one-broker,
# and one fewer once the killed broker's is dropped), each with a stamped broker's build after its
# name with built-names, as a broker built from a clean checkout names it; nothing with
# no-connections; no answer at all once the second broker is gone with connz-down-after-kill, or
# after the first read since then with connz-down-late. Each read takes 20 ms of the fake clock,
# the interval callout-check.sh polls the list at, answered or not. ---
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
entries='{"name": "dispatch", "authorized_user": "dispatch/dev-stack-dispatch"}, {"name": "listener", "authorized_user": "listener/dev-stack-test"}, {"authorized_user": "UNNAMED"}'
callout_name="envoy-broker-callout"
[ -e "$control/built-names" ] && callout_name="envoy-broker-callout legion@0123456789abcdef0123456789abcdef01234567"
for _ in $(seq 1 "$callouts"); do
  entries="$entries"', {"name": "'"$callout_name"'", "authorized_user": "UAUTHSERVICE"}'
done
echo "{\"connections\": [$entries]}"
EOF
printf '%s' "$stack_broker" >"${control_dir}/stack-broker"
printf '300' >"${control_dir}/drop-ms"
: >"${temporary_dir}/exchange-token"
printf '%s' "$machine_account" >"${control_dir}/machine-account"
printf '%s' "$nats_google_audience" >"${control_dir}/nats-google-audience"

# --- The metadata stand-in the check starts for the Google service line: dev-stack.sh writes a
# real one (DEV_STACK_METADATA_SCRIPT), which mints the machine's identity token; the check only
# starts it, waits for its port file and hands its address to natsprobe, so this one writes a port
# nothing listens on and then waits to be stopped. A script that never wrote the file would leave
# the check waiting, which is the case the UNKNOWN below is for. ---
readonly metadata_script="${temporary_dir}/dev-metadata.py"
cat >"$metadata_script" <<'PY'
import os, socket, sys, time

holder = socket.socket()
holder.bind(("127.0.0.1", 0))
with open(os.environ["DEV_STACK_METADATA_PORT_FILE"], "w") as handle:
    handle.write(str(holder.getsockname()[1]))
sys.stdout.flush()
while True:
    time.sleep(3600)
PY

# fail ends the test, printing the last check's whole output after the reason, so a red run in CI
# says which line failed and how.
fail() {
  echo "FAIL: $*" >&2
  if [ -n "${output:-}" ]; then
    printf 'the check printed:\n%s\n' "$output" >&2
  fi
  exit 1
}

# run_check runs the check against the fakes, with the control files named and the fake clock at a
# time of day, after a fresh second broker stand-in (the last one ended first, since a case can
# leave it running); it sets output and status.
output=""
status=0
run_check() {
  local name
  for name in grant-all no-connections silent slow other-words no-drop connz-down-after-kill connz-down-late read-after-kill one-broker built-names google-refused google-silent; do
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
    CALLOUT_CHECK_POLL_SECONDS=0 \
    DEV_STACK_CALLOUT=1 \
    AGENT_SECRETS_URL="$stack_broker" \
    NATS_URLS="nats://127.0.0.1:7002" \
    DEV_STACK_EXCHANGE_TOKEN_FILE="${temporary_dir}/exchange-token" \
    DEV_STACK_EXCHANGE_SESSION="$session" \
    DEV_STACK_BROKER_PIDS="1 $broker_stand_in" \
    DEV_STACK_NATS_MONITOR_URL="http://127.0.0.1:7003" \
    DEV_STACK_MACHINE_ACCOUNT="$machine_account" \
    DEV_STACK_METADATA_SCRIPT="$metadata_script" \
    DEV_STACK_NATS_GOOGLE_AUDIENCE="$nats_google_audience" \
    bash "$check" 2>&1)" || status=$?
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
  "PASS a service connects by its Google identity token: connected as service ${machine_account}"
)
for index in "${!want[@]}"; do
  [ "$(line $((index + 1)))" = "${want[$index]}" ] || fail "line $((index + 1)) is '$(line $((index + 1)))', want '${want[$index]}'"
done
line7="$(line 7)"
[[ "$line7" =~ ^"PASS one broker killed and a new session admitted within 2 s: the hub dropped the killed broker's connection after "([0-9]+)" ms; three sessions, the slowest "([0-9]+)" ms, the first counted from the kill"$ ]] ||
  fail "line 7 is '$line7'"
[ "${BASH_REMATCH[1]}" -ge 300 ] && [ "${BASH_REMATCH[2]}" -ge "${BASH_REMATCH[1]}" ] ||
  fail "line 7 has the hub drop the killed broker's connection after ${BASH_REMATCH[1]} ms and its slowest session at ${BASH_REMATCH[2]} ms: the fake hub keeps that connection for 300 ms, so the check did not wait for the drop, or did not count the first session from the kill"
[ "$(line 8)" = "all 7 assertions passed" ] && [ "$status" -eq 0 ] || fail "want 'all 7 assertions passed' and exit 0, got exit $status"
[ "$("${fake_bin_dir}/hub-state")" != alive ] || fail "the check did not kill the second broker $broker_stand_in"
echo "PASS: a hub that answers as the auth service does passes all seven lines, the second broker is killed, and no session is opened before the hub drops its connection"

# (b) The negative control: a service that grants every session's delivery channel.
CALLOUT_CHECK_GRANT_ALL=1 run_check
[ "$(line 2)" = "FAIL a session is refused another session's delivery channel: it was served notifications.envoy.deliver.${session}-other" ] ||
  fail "with CALLOUT_CHECK_GRANT_ALL=1 line 2 is '$(line 2)'"
[ "$status" -eq 1 ] && [ "$(line 8)" = "1 of 7 assertions failed" ] || fail "with CALLOUT_CHECK_GRANT_ALL=1: exit $status, last line '$(line 8)'"
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

# (e) An admission slower than 2 s once a broker is killed.
run_check slow
[[ "$(line 7)" == "FAIL one broker killed and a new session admitted within 2 s: attempt 1 took "*" ms" ]] || fail "a slow admission: line 7 is '$(line 7)'"
[ "$status" -eq 1 ] || fail "a slow admission: exit $status"
echo "PASS: an admission slower than 2 s after the kill FAILs line 7"

# (f) An exported stack missing a variable.
status=0
output="$(env PATH="${fake_bin_dir}:${PATH}" TMPDIR="$temporary_dir" DEV_STACK_CALLOUT=1 \
  AGENT_SECRETS_URL="$stack_broker" NATS_URLS="nats://127.0.0.1:7002" \
  DEV_STACK_EXCHANGE_TOKEN_FILE="${temporary_dir}/exchange-token" DEV_STACK_EXCHANGE_SESSION="$session" \
  DEV_STACK_BROKER_PIDS="1 2" bash "$check" 2>&1)" || status=$?
[ "$status" -eq 2 ] && [[ "$output" == *"DEV_STACK_NATS_MONITOR_URL is not set"* ]] ||
  fail "a stack without DEV_STACK_NATS_MONITOR_URL: exit $status"
echo "PASS: an exported stack missing DEV_STACK_NATS_MONITOR_URL is refused with exit 2, naming it"

# (g) A hub that never drops the killed broker's connection. Each read of the list takes 20 ms of
# the fake clock and nothing else moves it, so the check gives up at exactly its 2 s bound.
run_check no-drop
[[ "$(line 7)" == "FAIL one broker killed and a new session admitted within 2 s: the hub still listed 2 connections named envoy-broker-callout "*" ms after the kill" ]] ||
  fail "a hub that never drops the killed broker's connection: line 7 is '$(line 7)'"
[ "$status" -eq 1 ] || fail "a hub that never drops the killed broker's connection: exit $status"
echo "PASS: a hub that never drops the killed broker's connection FAILs line 7"

# (h) A connection list that cannot be read once the broker is killed.
run_check connz-down-after-kill
[ "$(line 7)" = "UNKNOWN one broker killed and a new session admitted within 2 s: the hub's connection list (http://127.0.0.1:7003/connz) could not be read after the kill" ] ||
  fail "a connection list unreadable after the kill: line 7 is '$(line 7)'"
[ "$status" -eq 1 ] || fail "a connection list unreadable after the kill: exit $status"
echo "PASS: a connection list that cannot be read after the kill reads UNKNOWN on line 7 and fails the run"

# (i) One auth service connection before the kill: the check cannot see a drop, so it kills nothing.
run_check one-broker
[ "$(line 7)" = "UNKNOWN one broker killed and a new session admitted within 2 s: the hub lists 1 connection named envoy-broker-callout before the kill, not one for each of the two brokers" ] ||
  fail "one auth service connection before the kill: line 7 is '$(line 7)'"
[ "$status" -eq 1 ] || fail "one auth service connection before the kill: exit $status"
[ "$("${fake_bin_dir}/hub-state")" = alive ] || fail "with one auth service connection listed the check killed the second broker anyway"
echo "PASS: a hub listing one auth service connection before the kill reads UNKNOWN on line 7, and nothing is killed"

# (j) A list read once after the kill, naming both connections still, that then cannot be read:
# the check saw the hub keep the killed broker, so it is a FAIL with that count, not an UNKNOWN.
run_check no-drop connz-down-late
[[ "$(line 7)" == "FAIL one broker killed and a new session admitted within 2 s: the hub still listed 2 connections named envoy-broker-callout "*" ms after the kill" ]] ||
  fail "a list read once after the kill and then unreadable: line 7 is '$(line 7)'"
[ "$status" -eq 1 ] || fail "a list read once after the kill and then unreadable: exit $status"
echo "PASS: a list read once after the kill and then unreadable FAILs line 7 with the count it last read"

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

# (l) With no stack exported, a --database-url reaches the dev-stack.sh the check starts as
# LEGION_TEST_PG_DSN, set for that one command, in either spelling, and no argument of that child
# holds the URL or its password; every other argument is passed on as it was. The check runs from
# a copy beside a dev-stack.sh stand-in that records its arguments and its LEGION_TEST_PG_DSN and
# exits, so the stack never comes up and the check exits 2.
url_copy="${temporary_dir}/url-check"
mkdir -p "$url_copy"
cp "$check" "$url_copy/callout-check.sh"
cat >"$url_copy/dev-stack.sh" <<'EOF'
printf '%s\n' "$*" >"$(dirname "$0")/args"
printf '%s\n' "${LEGION_TEST_PG_DSN-unset}" >"$(dirname "$0")/dsn"
exit 1
EOF
readonly secret_url='postgres://tester:not-a-secret%40canary@127.0.0.1:5999/postgres?sslmode=disable'
url_case() { # <label> <callout-check.sh argument>...
  local label="$1" got_args got_dsn
  shift
  rm -f "$url_copy/args" "$url_copy/dsn"
  output=""
  status=0
  DEV_STACK_CALLOUT='' TMPDIR="$temporary_dir" bash "$url_copy/callout-check.sh" "$@" >/dev/null 2>&1 || status=$?
  got_args="$(cat "$url_copy/args" 2>/dev/null || true)"
  got_dsn="$(cat "$url_copy/dsn" 2>/dev/null || true)"
  if [ "$status" -ne 2 ] || [ "$got_args" != "--callout --binaries /nowhere" ] || [ "$got_dsn" != "$secret_url" ]; then
    fail "${label}: dev-stack.sh was started with arguments '${got_args}' and LEGION_TEST_PG_DSN '${got_dsn}' (exit ${status}); want '--callout --binaries /nowhere' and the URL"
  fi
}
url_case "--database-url <url>" --database-url "$secret_url" --binaries /nowhere
url_case "--database-url=<url>" "--database-url=$secret_url" --binaries /nowhere
echo "PASS: a --database-url reaches dev-stack.sh as LEGION_TEST_PG_DSN in either spelling, and no argument of dev-stack.sh holds it"

# (m) Brokers whose connections name their build after envoy-broker-callout, as internal/bus names
# every connection of a binary built from a clean checkout (the stack's own go build): the check
# counts both before the kill and sees the drop, as for the bare name.
run_check built-names
[[ "$(line 7)" =~ ^"PASS one broker killed and a new session admitted within 2 s: the hub dropped the killed broker's connection after "[0-9]+" ms" ]] ||
  fail "brokers that name their build: line 7 is '$(line 7)'"
[ "$(line 8)" = "all 7 assertions passed" ] && [ "$status" -eq 0 ] || fail "brokers that name their build: exit $status, last line '$(line 8)'"
echo "PASS: brokers whose connections name their build after envoy-broker-callout pass line 7"

# (n) A hub that does not admit the Google service: the broker's BROKER_NATS_SERVICES names no
# entry for the machine, or its BROKER_NATS_GOOGLE_AUDIENCE is another audience, so the service's
# identity token proves nothing to the auth service and the hub refuses it in its own words.
run_check google-refused
[ "$(line 6)" = "FAIL a service connects by its Google identity token: natsprobe exit 4: not admitted: Authorization Violation" ] ||
  fail "a Google service the hub refuses: line 6 is '$(line 6)'"
[ "$status" -eq 1 ] || fail "a Google service the hub refuses: exit $status"
echo "PASS: a Google service the hub does not admit FAILs line 6"

# (o) A probe that prints nothing for the Google service: UNKNOWN, never a pass, as a silent probe
# is on every other line.
run_check google-silent
[[ "$(line 6)" == "UNKNOWN a service connects by its Google identity token: natsprobe printed nothing"* ]] ||
  fail "a silent Google probe: line 6 is '$(line 6)'"
[ "$status" -eq 1 ] || fail "a silent Google probe: exit $status"
echo "PASS: a Google service probe that prints nothing reads UNKNOWN"

# (p) A stack from before this line, whose banner exports no DEV_STACK_NATS_GOOGLE_AUDIENCE: the
# check has no audience to name, so line 6 is UNKNOWN naming the variable, never a pass, and the
# run fails.
status=0
sleep 600 &
broker_stand_in=$!
disown "$broker_stand_in"
printf '%s' "$broker_stand_in" >"${control_dir}/second-broker"
printf '1700000000000' >"${control_dir}/clock"
output="$(env PATH="${fake_bin_dir}:${PATH}" TMPDIR="$temporary_dir" DEV_STACK_CALLOUT=1 \
  AGENT_SECRETS_URL="$stack_broker" NATS_URLS="nats://127.0.0.1:7002" \
  DEV_STACK_EXCHANGE_TOKEN_FILE="${temporary_dir}/exchange-token" \
  DEV_STACK_EXCHANGE_SESSION="$session" \
  DEV_STACK_BROKER_PIDS="1 $broker_stand_in" \
  DEV_STACK_NATS_MONITOR_URL="http://127.0.0.1:7003" \
  DEV_STACK_MACHINE_ACCOUNT="$machine_account" \
  DEV_STACK_METADATA_SCRIPT="$metadata_script" \
  bash "$check" 2>&1)" || status=$?
[ "$(printf '%s\n' "$output" | sed -n 6p)" = "UNKNOWN a service connects by its Google identity token: the stack's banner exports no DEV_STACK_NATS_GOOGLE_AUDIENCE" ] ||
  fail "a banner with no DEV_STACK_NATS_GOOGLE_AUDIENCE: line 6 is '$(printf '%s\n' "$output" | sed -n 6p)'"
[ "$status" -eq 1 ] || fail "a banner with no DEV_STACK_NATS_GOOGLE_AUDIENCE: exit $status"
echo "PASS: a stack whose banner names no Google audience reads UNKNOWN on line 6"
