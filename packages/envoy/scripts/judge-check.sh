#!/usr/bin/env bash
# packages/envoy/scripts/judge-check.sh
#
# Runs one before_merge check of the model judge's verification document
# (dispatch://INFRA-1353/artifact/verification-md) end to end and says whether it passed, so
# that the implementer and the tester run the same command and read the same evidence. It is
# scripts/airlock-check.sh for the judge's own six checks - k10, k10b, k10c, k10d, k10e and
# k10f - and copies that script's shape: the same stack, the same helpers, the same result
# block.
#
# What one run proves is the check's own sentence and nothing wider. The script starts the
# local stack the check needs (scripts/dev-airlock.sh), drives every case the check names,
# runs the negative control, stops the stack, and prints:
#
#   - a result block to paste into `dispatch result`: what the check expected, what this run
#     observed, and what the negative control did;
#   - one PASS: or FAIL: line for each property the check's outcome sentence names, and one for
#     its negative control, because a control that does not behave as the check says leaves the
#     check proving nothing;
#   - one last line, `check <id>: passed` or `check <id>: failed (<n> of <m>)`.
#
# No line it prints is a credential. A value is compared by its SHA-256 (the first 16 hex
# digits) and its length, and every line that compares one says so; that is a rule of the
# verification document itself, and it holds for the stand-in values of a local stack too. The
# placeholders this script forges for check k10c are made here and printed nowhere.
#
# One name is deliberately not airlock-check.sh's. That script records a property with a helper
# called `judge`; here the judge is the thing under test, so the helper is called `assert` and
# the word "judge" means the broker's model judge throughout.
#
# Three things this stack measured shape how the checks below are driven, and each is said
# again in the result block of the check it touches:
#
#   - The broker reads BROKER_AIRLOCK_RULES_FILE once, at boot (cmd/broker/main.go: "The file is
#     read once; a change to it rolls the pods"). A check that needs other rules - k10d's judge
#     300, and its control's judge 900 - therefore starts one more broker on the stack's own
#     database, stand-ins and certificate authority with a rules file of its own, and points the
#     stack's two fronts at it by writing their backends files. Pointing the CLI straight at
#     that broker's own port would not do: a request object's audience is the base URL the CLI
#     was given, and the broker verifies it against BROKER_PUBLIC_URL, which is the front.
#   - `agent-secrets grant list|revoke` act under this machine's login, which
#     agent-secrets-helper holds, and the helper pins its peer with SO_PEERPIDFD (Linux 6.5).
#     A Legion issue pod's gVisor kernel has no such option, so k10f drives the two broker
#     routes those two commands call - GET /v1/grants?approver=<login> and POST
#     /v1/grants/{id}/revoke-by-approver, requests.Machine.GrantsForApprover and
#     RevokeByApprover - with the stack's own API account token, under each signed-in login.
#   - A grant the judge decided records the *record's* own approver in grants.approver
#     (requests' decider.grantApprover) and the judge in requests.decided_by. k10e's and k10f's
#     result blocks say which column each of their sentences is about.
#
# Usage: packages/envoy/scripts/judge-check.sh <id> [--keep-stack]
#        <id> is k10, k10b, k10c, k10d, k10e or k10f. --keep-stack leaves the stack and the work
#        directory up for a person to look at a failure with. --help prints this usage.
#        A usage error exits 2.
#
# Needs bash, curl, jq, go and psql. DEV_BROKER_POSTGRES_URL is passed to dev-airlock.sh
# as it is, so a pod with no container runtime names its test server there. k10b needs no stack
# and takes seconds; every other check starts one stack (k10 starts two) and takes a few minutes.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENVOY_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

usage() {
	cat <<'EOF'
usage: judge-check.sh <id> [--keep-stack]

Runs one before_merge check of dispatch://INFRA-1353/artifact/verification-md and prints, for
each property the check's outcome sentence names, what it expected beside what it got.

  <id>           k10, k10b, k10c, k10d, k10e or k10f
  --keep-stack   leave the stack and this run's work directory up, to look at a failure with
  --help         print this

It exits 0 only when every property passed, 1 when one did not and 2 on a usage error.
EOF
}

CHECK_ID=""
KEEP_STACK=false
while [ $# -gt 0 ]; do
	case "$1" in
	--keep-stack) KEEP_STACK=true ;;
	--help | -h)
		usage
		exit 0
		;;
	-*)
		echo "judge-check: unexpected option $1" >&2
		usage >&2
		exit 2
		;;
	*)
		if [ -n "$CHECK_ID" ]; then
			echo "judge-check: one check id at a time, not $CHECK_ID and $1" >&2
			usage >&2
			exit 2
		fi
		CHECK_ID="$1"
		;;
	esac
	shift
done

if [ -z "$CHECK_ID" ]; then
	echo "judge-check: name the check to run" >&2
	usage >&2
	exit 2
fi

case "$CHECK_ID" in
k10 | k10b | k10c | k10d | k10e | k10f) ;;
*)
	echo "judge-check: $CHECK_ID is no check of this document; it holds k10, k10b, k10c, k10d, k10e and k10f" >&2
	usage >&2
	exit 2
	;;
esac

WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/judge-check-$CHECK_ID.XXXXXX")"
STACK_LOG="$WORK_DIR/stack.log"
STACK_PID=""
EXTRA_BROKER_PIDS=()

# kill_extra_brokers stops every broker this run started of its own, the ones start_extra_broker
# left in EXTRA_BROKER_PIDS, and waits for each to go. Both the EXIT trap and stop_stack do it,
# and they must do it the same way: a broker of this script's still holding a connection to the
# stack's database keeps dev-airlock.sh's own cleanup from dropping it.
kill_extra_brokers() {
	local pid
	for pid in "${EXTRA_BROKER_PIDS[@]+"${EXTRA_BROKER_PIDS[@]}"}"; do
		if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
			kill "$pid" 2>/dev/null || true
			wait "$pid" 2>/dev/null || true
		fi
	done
}

cleanup() {
	kill_extra_brokers
	if [ "$KEEP_STACK" = true ] && [ -n "$STACK_PID" ]; then
		echo "judge-check: the stack is still up (dev-airlock.sh pid $STACK_PID, log $STACK_LOG);" >&2
		echo "judge-check: stop it with kill $STACK_PID, which drops its databases" >&2
		return
	fi
	# SIGTERM, never SIGKILL: dev-airlock.sh's own EXIT trap is what stops its processes and
	# drops the two databases it made.
	if [ -n "$STACK_PID" ] && kill -0 "$STACK_PID" 2>/dev/null; then
		kill "$STACK_PID" 2>/dev/null || true
		wait "$STACK_PID" 2>/dev/null || true
	fi
	if [ "$KEEP_STACK" = true ]; then
		echo "judge-check: work directory kept at $WORK_DIR" >&2
	else
		rm -rf "$WORK_DIR"
	fi
}
trap cleanup EXIT

# --- What a run says -----------------------------------------------------------------------
#
# Three lists make the result block, and one pair of counters makes the verdict. A property is
# recorded with pass or fail and nothing else decides the exit status.

EXPECTED=()
OBSERVED=()
CONTROL=()
PASSED=0
TOTAL=0
VERDICTS=()

expect() { EXPECTED+=("$1"); }
observe() { OBSERVED+=("$1"); }
control() { CONTROL+=("$1"); }

pass() {
	TOTAL=$((TOTAL + 1))
	PASSED=$((PASSED + 1))
	VERDICTS+=("PASS: $1")
}

fail() {
	TOTAL=$((TOTAL + 1))
	VERDICTS+=("FAIL: $1")
}

# assert records one property from a condition the caller has already evaluated: "0" passes and
# anything else, an empty answer included, fails. A step that could not be evaluated at all is a
# property that was not proved, which is a failure and never a silent pass. airlock-check.sh
# calls this helper `judge`; here that word is the thing under test.
assert() { # <status> <what the property is> <what was seen>
	if [ "${1:-}" = 0 ]; then
		pass "$2 — $3"
	else
		fail "$2 — $3"
	fi
}

report() {
	echo
	echo "---- result block for check $CHECK_ID ----"
	echo "expected:"
	printf '  %s\n' "${EXPECTED[@]+"${EXPECTED[@]}"}"
	echo "observed:"
	printf '  %s\n' "${OBSERVED[@]+"${OBSERVED[@]}"}"
	echo "negative control:"
	printf '  %s\n' "${CONTROL[@]+"${CONTROL[@]}"}"
	echo "---- end result block ----"
	echo
	printf '%s\n' "${VERDICTS[@]+"${VERDICTS[@]}"}"
	echo
	if [ "$PASSED" -eq "$TOTAL" ]; then
		echo "check $CHECK_ID: passed"
		return 0
	fi
	echo "check $CHECK_ID: failed ($((TOTAL - PASSED)) of $TOTAL)"
	return 1
}

# --- Comparing a value without printing it ---------------------------------------------------
#
# Every comparison of a value in this script is by SHA-256 and length, and every line that makes
# one says so. digest answers the first 16 hex digits of a value's SHA-256, which is far more
# than two values of one run ever share and far too little to be the value.

digest() { printf '%s' "$1" | sha256sum | cut -c1-16; }
shape() { printf 'len=%d sha256=%s' "${#1}" "$(digest "$1")"; }

# contains reports whether haystack holds needle, without either reaching the output.
contains() { case "$2" in *"$1"*) return 0 ;; *) return 1 ;; esac; }

# --- The stack ---------------------------------------------------------------------------------

# await waits for a line a process logs, and ends the run the moment that process dies: a stack
# that never came up proves nothing, and the check's own steps would each fail with a reason
# that is not the check's.
await() { # <what> <seconds> <log> <pattern> <pid>
	local i
	for ((i = 0; i < $2 * 2; i++)); do
		if grep -qE "$4" "$3" 2>/dev/null; then return 0; fi
		if ! kill -0 "$5" 2>/dev/null; then
			echo "judge-check: $1 exited before it was ready; see $3" >&2
			tail -n 20 "$3" >&2 || true
			exit 1
		fi
		sleep 0.5
	done
	echo "judge-check: $1 was not ready within $2 seconds; see $3" >&2
	exit 1
}

# start_stack runs dev-airlock.sh with the options its caller names, waits for its banner, and
# exports everything that banner names. dev-broker.sh's own export block is in the same log and
# is read first, so where the two name one variable the airlock's wins, as the banner says.
start_stack() {
	echo "judge-check: starting the stack (dev-airlock.sh $*)..." >&2
	: >"$STACK_LOG"
	bash "$SCRIPT_DIR/dev-airlock.sh" "$@" >"$STACK_LOG" 2>&1 &
	STACK_PID=$!
	await "the stack" 900 "$STACK_LOG" '^dev-airlock: ready\.$' "$STACK_PID"
	eval "$(sed -n 's/^  \(export [A-Z][A-Z_0-9]*=.*\)$/\1/p' "$STACK_LOG")"
	mapfile -t BROKER_API_ADDRS < <(sed -n 's/^  broker [0-9]*: *api \([^,]*\),.*/\1/p' "$STACK_LOG")
	mapfile -t BROKER_PROXY_ADDRS < <(sed -n 's/^  broker [0-9]*:.*proxy \([^,]*\),.*/\1/p' "$STACK_LOG")
	mapfile -t BROKER_PIDS < <(sed -n 's/^  broker [0-9]*:.*pid \([0-9]*\),.*/\1/p' "$STACK_LOG")
	mapfile -t BROKER_LOGS < <(sed -n 's/^  broker [0-9]*:.*log \([^ ]*\) *$/\1/p' "$STACK_LOG")
	STACK_DB_NAME="$(sed -n 's/^dev-airlock: created isolated database \([a-z0-9_]*\).*/\1/p' "$STACK_LOG" | head -n 1)"
	# The brokers' database URL, built exactly as dev-airlock.sh builds it, which is the one way
	# to reach the rows a check writes by hand (a withheld name, a forged judged approval).
	if [ -n "${DEV_BROKER_POSTGRES_URL:-}" ] && [[ "$DEV_BROKER_POSTGRES_URL" =~ ^(postgres(ql)?://[^/?]+)/[^/?]*(\?.*)?$ ]]; then
		STACK_DB_URL="${BASH_REMATCH[1]}/${STACK_DB_NAME}${BASH_REMATCH[3]:-}"
	else
		STACK_DB_URL="postgres://postgres:dispatch@127.0.0.1:55432/${STACK_DB_NAME}?sslmode=disable"
	fi
	BROKER_BIN="$DEV_BROKER_DIR/bin/broker"
	JUDGE_ACTOR="judge:$AIRLOCK_JUDGE_MODEL"
	PROXY_ADDR="${AIRLOCK_PROXY_URL##*://}"
	echo "judge-check: the stack is up (${#BROKER_API_ADDRS[@]} brokers, work directory $AIRLOCK_DIR)" >&2
}

# stop_stack stops this run's own extra brokers first and then dev-airlock.sh. The order matters:
# a broker of this script's still holding a connection to the stack's database keeps
# dev-airlock.sh's own cleanup from dropping it, and the database is left behind on the server.
stop_stack() {
	if [ "$KEEP_STACK" = true ]; then return 0; fi
	kill_extra_brokers
	EXTRA_BROKER_PIDS=()
	if [ -n "$STACK_PID" ] && kill -0 "$STACK_PID" 2>/dev/null; then
		echo "judge-check: stopping the stack..." >&2
		kill "$STACK_PID" 2>/dev/null || true
		wait "$STACK_PID" 2>/dev/null || true
	fi
	STACK_PID=""
}

# broker_env answers, in BROKER_ENV, the command prefix that runs a program with the environment
# of a broker like the stack's own and the caller's overrides on top. It is read out of the first
# stack broker's /proc/<pid>/environ rather than spelled here, so that a broker this script starts
# can never lack a setting dev-airlock.sh gave the stack's. The entries are NUL-separated, and
# each becomes one argument of env, so a value holding a space or a quote reaches the broker
# unchanged; env applies its assignments in order, so an override the caller names replaces the
# copy.
broker_env() { # <VAR=value>...
	local entries=()
	mapfile -d '' -t entries <"/proc/${BROKER_PIDS[0]}/environ"
	BROKER_ENV=(env -i "${entries[@]}" "$@")
}

# start_extra_broker starts one more broker process beside the stack's, on its database, rules
# file, certificate authority and stand-ins, with the environment the caller adds on top, and
# answers its API and proxy addresses in EXTRA_API_ADDR and EXTRA_PROXY_ADDR. It is how a control
# that changes one setting - or one rules entry, through BROKER_AIRLOCK_RULES_FILE - is run.
start_extra_broker() { # <name> <VAR=value>...
	local name="$1"
	shift
	local log="$WORK_DIR/broker-$name.log"
	: >"$log"
	# Two ports the system picks, so this one never collides with the broker it was copied from.
	broker_env BROKER_LISTEN_ADDR="127.0.0.1:0" BROKER_AIRLOCK_LISTEN_ADDR="127.0.0.1:0" "$@"
	"${BROKER_ENV[@]}" "$BROKER_BIN" >"$log" 2>&1 &
	local pid=$!
	EXTRA_BROKER_PIDS+=("$pid")
	await "the $name broker" 120 "$log" 'airlock proxy listening addr=' "$pid"
	EXTRA_API_ADDR="$(sed -n 's/.*broker listening addr=\([^ ]*\).*/\1/p' "$log" | head -n 1)"
	EXTRA_PROXY_ADDR="$(sed -n 's/.*airlock proxy listening addr=\([^ ]*\).*/\1/p' "$log" | head -n 1)"
}

# front_backends points the stack's two fronts at the brokers the caller names, by writing the
# backends files they reread. It is how a check sends every call the CLI makes - whose request
# objects are signed for the front's own URL - at a broker this script started.
front_backends() { # <api addr> <proxy addr>
	printf '%s\n' "$1" >"$AIRLOCK_DIR/api-backends"
	printf '%s\n' "$2" >"$AIRLOCK_DIR/proxy-backends"
	# The fronts poll their file; a moment is enough for both to have read it.
	sleep 2
}

# front_backends_stack points the two fronts back at the stack's own brokers.
front_backends_stack() {
	printf '%s\n' "${BROKER_API_ADDRS[@]}" >"$AIRLOCK_DIR/api-backends"
	printf '%s\n' "${BROKER_PROXY_ADDRS[@]}" >"$AIRLOCK_DIR/proxy-backends"
	sleep 2
}

# stack_facts reads the few settings the fake store needs and that the banner does not name: the
# namespace prefix and key the brokers serve, read from their own /v1/settings route.
stack_facts() {
	local settings
	settings="$(curl -sS "$AGENT_SECRETS_URL/v1/settings")"
	STACK_SECRETS_PREFIX="$(printf '%s' "$settings" | jq -r .secrets_prefix)"
	STACK_KMS_KEY_ARN="$(printf '%s' "$settings" | jq -r '.kms_key_arn // ""')"
}

psql_stack() { psql "$STACK_DB_URL" -Atq -v ON_ERROR_STOP=1 "$@"; }

records() { curl -sS "$AIRLOCK_RECORDS_URL/records/$1"; }
records_count() { records "$1" | jq 'length'; }

# reread_all makes every broker read one secret's name again, which is what a change to the fake
# store waits on.
reread_all() { # <name>
	local addr
	for addr in "${BROKER_API_ADDRS[@]}"; do
		# Quiet, and never fatal: a check that stopped one broker on purpose (k10) still
		# rereads on the one that is up, and curl's own line about the other would land in
		# the middle of a result block.
		curl -sS -o /dev/null -X POST "http://$addr/v1/secrets/$1/reread" 2>/dev/null || true
	done
}

# add_fake_secret appends one secret to the stack's fake Secrets Manager and set_fake_tags
# replaces one secret's tags. They are how a check gets a name the stack does not hold, or moves
# the policy version, without writing a rules file of its own.
add_fake_secret() { # <store name> <tags json> <value>
	local tmp="$WORK_DIR/fake-secrets.json"
	jq --arg name "$1" --argjson tags "$2" --arg value "$3" --arg kms "$STACK_KMS_KEY_ARN" \
		'.secrets += [{name: $name, kms_key_id: $kms, tags: $tags, value: $value}]' \
		"$AIRLOCK_FAKE_SECRETS_FILE" >"$tmp"
	cat "$tmp" >"$AIRLOCK_FAKE_SECRETS_FILE"
	rm -f "$tmp"
}

set_fake_tags() { # <store name> <tags json>
	local tmp="$WORK_DIR/fake-secrets.json"
	jq --arg name "$1" --argjson tags "$2" \
		'.secrets = [.secrets[] | if .name == $name then .tags = $tags else . end]' \
		"$AIRLOCK_FAKE_SECRETS_FILE" >"$tmp"
	cat "$tmp" >"$AIRLOCK_FAKE_SECRETS_FILE"
	rm -f "$tmp"
}

# stored_value is what the fake store holds for one name, read straight out of the file so a
# comparison is against the store and not against a string this script carries.
stored_value() { jq -r --arg n "$1" '.secrets[] | select(.name == $n) | .value' "$AIRLOCK_FAKE_SECRETS_FILE"; }

# --- Talking to the brokers --------------------------------------------------------------------

# model_answers sets what the stand-in model answers from here on: {"verdict":…,"reason":…} for a
# structured reply, {"text":…} for one that is no such object, {"delay":"30s"} for an answer held
# back past the judge's own 20-second bound.
model_answers() { # <json body>
	curl -sS -o /dev/null -X POST -H 'Content-Type: application/json' --data "$1" "$AIRLOCK_MODEL_ANSWER_URL"
}

# ask asks for one grant as the CLI does, leaving the raw answer in ASK_JSON and the request's
# id, state and grant in three variables. It never fails the run: a refused request is an
# ordinary answer a case reads.
ask() { # <ttl, or "" for none> <reason> <name>...
	local ttl="$1" reason="$2"
	shift 2
	local args=(request "$@" --reason "$reason" --json)
	if [ -n "$ttl" ]; then args+=(--ttl "$ttl"); fi
	ASK_JSON="$(agent-secrets "${args[@]}" 2>"$WORK_DIR/ask.err")"
	ASK_STATUS=$?
	ASK_ERR="$(tr '\n' ' ' <"$WORK_DIR/ask.err")"
	ASK_ID="$(printf '%s' "$ASK_JSON" | jq -r '.request_id // ""')"
	ASK_STATE="$(printf '%s' "$ASK_JSON" | jq -r '.state // ""')"
	ASK_RECORD="$(printf '%s' "$ASK_JSON" | jq -r '.record_id // ""')"
	ASK_LIFETIME="$(printf '%s' "$ASK_JSON" | jq -r '.lifetime_seconds // ""')"
}

# await_granted polls a request's state as a person would, and answers the seconds it took. It
# stops at the first state that is not pending, so a request the judge referred is read at once.
await_granted() { # <request id> <seconds>
	local id="$1" bound="$2" start now
	start="$(date +%s)"
	while :; do
		AWAIT_STATE="$(agent-secrets status "$id" --json 2>/dev/null | jq -r '.state // ""')"
		now="$(date +%s)"
		AWAIT_SECONDS=$((now - start))
		if [ "$AWAIT_STATE" != pending ] || [ "$AWAIT_SECONDS" -ge "$bound" ]; then return 0; fi
		sleep 1
	done
}

# await_judged waits for one request's judged row the way await_granted waits for its state:
# it polls judged_rows once a second until there is one or the bound passes, and then sleeps a
# two-second settle window before it answers. The window is not padding - k10 expects exactly
# one such row per decision, and returning the instant the first row appeared would stop
# looking before a second one could be written, which would thin the check.
await_judged() { # <request id> <seconds>
	local id="$1" bound="$2" waited=0
	while [ "$waited" -lt "$bound" ]; do
		if [ "$(judged_rows "$id")" != 0 ]; then break; fi
		sleep 1
		waited=$((waited + 1))
	done
	sleep 2
}

# request_state, request_decider and request_grant are one request's state, the login or actor
# the broker recorded as having decided it, and the grant its approval made, as
# `agent-secrets status` prints them.
request_state() { agent-secrets status "$1" --json 2>/dev/null | jq -r '.state // ""'; }
request_decider() { agent-secrets status "$1" --json 2>/dev/null | jq -r '.decision.by // ""'; }
request_grant() { agent-secrets status "$1" --json 2>/dev/null | jq -r '.grant_id // ""'; }

# cancel_pending cancels every request of this session that is still pending, so one case's
# leftover card cannot refuse the next case's request for the same name (ErrRequestPending).
cancel_pending() {
	local id
	for id in $(psql_stack -c "select id from requests where state='pending'"); do
		agent-secrets cancel "$id" >/dev/null 2>&1 || true
	done
}

# release releases a grant's values as the CLI does, leaving the grant's proxy password in
# PROXY_PASSWORD and its placeholder in PLACEHOLDER. Neither ever reaches the output.
release() { # <grant id> <name>
	local url proof RELEASE_JSON
	url="$AGENT_SECRETS_URL/v1/grants/$1/values"
	proof="$(agent-secrets sign --method POST --url "$url")"
	RELEASE_JSON="$(curl -sS -X POST -H "Proof: $proof" "$url")"
	PLACEHOLDER="$(printf '%s' "$RELEASE_JSON" | jq -r --arg n "$2" '.airlock.names[$n].value // ""')"
	PROXY_PASSWORD="$(printf '%s' "$RELEASE_JSON" | jq -r '.airlock.proxy_password // ""')"
}

# proxy_for is the proxy URL of one proxy password at one address, credential and all. It is
# passed to curl and never printed.
proxy_for() { printf 'http://agent-secrets:%s@%s' "$1" "$2"; }

# call_s makes one call to S through the proxy with the password and header value the caller
# names, leaving the status in CALL_STATUS and the answer's body in CALL_BODY.
#
# The status has to be read two ways, because the proxy refuses a call at two moments. A grant
# the connection read refuses (revoked, expired, holding no name this session may have) is
# refused at the CONNECT, and curl answers no HTTP code at all for that - it reports the
# proxy's code in its own words and gives up - while a grant the full release checks refuse
# (the approval chain, the policy re-read) is refused inside the tunnel, as an ordinary
# response with a body. So both are read, and a status this cannot name is reported whole.
call_s() { # <proxy password> <header value>
	local CALL_OUT
	CALL_OUT="$(curl -sS -o "$WORK_DIR/call.out" -w 'http_code=%{http_code}' --noproxy '' \
		--cacert "$AIRLOCK_CA_CERT_FILE" --proxy "$(proxy_for "$1" "$PROXY_ADDR")" \
		-H "Authorization: Bearer $2" "$AIRLOCK_S_URL/ok" 2>&1 | tr '\n' ' ')"
	CALL_BODY="$(tr -d '\n' <"$WORK_DIR/call.out")"
	case "$CALL_OUT" in
	*"http_code=200"*) CALL_STATUS=200 ;;
	*"http_code=403"* | *"response 403"* | *"code 403 from proxy"*) CALL_STATUS=403 ;;
	*"http_code=407"* | *"response 407"* | *"code 407 from proxy"*) CALL_STATUS=407 ;;
	*) CALL_STATUS="$CALL_OUT" ;;
	esac
}

# s_authorization is the Authorization header S recorded on the last request it was sent, which
# is how a check reads whether the proxy put the real value there.
s_authorization() { records s | jq -r '.[-1].header.Authorization[0] // ""'; }

# --- The judge's own audit row -------------------------------------------------------------

# judged_rows is how many request.judged rows one request has, and judged_detail the detail of
# its last one: the checks the run made, the model, the verdict and the reason.
judged_rows() { psql_stack -c "select count(*) from audit where kind='request.judged' and request_id='$1'"; }
judged_detail() { psql_stack -c "select detail::text from audit where kind='request.judged' and request_id='$1' order by id desc limit 1"; }
judged_actor() { psql_stack -c "select actor from audit where kind='request.judged' and request_id='$1' order by id desc limit 1"; }

# judged_summary is one judged row read back as a line a result block can carry: the checks in
# order with the first that failed named, the model, whether it was asked, the verdict and the
# reason.
judged_summary() { # <request id>
	local detail
	detail="$(judged_detail "$1")"
	if [ -z "$detail" ]; then
		echo "no request.judged row"
		return
	fi
	printf '%s' "$detail" | jq -r '
		"checks=" + ([.checks[] | .check + "=" + (.passed|tostring)] | join(",")) +
		" failed=" + (([.checks[] | select(.passed | not) | .check] | first) // "none") +
		" detail=" + (([.checks[] | select(.passed | not) | .detail] | first) // "") +
		" model=" + (.model // "") + " asked=" + ((.asked // false)|tostring) +
		" verdict=" + (.verdict // "") + " reason=" + (.reason // "") +
		(if has("applied") then " applied=" + (.applied|tostring) else "" end) +
		(if has("not_applied") then " not_applied=" + .not_applied else "" end) +
		(if has("error") then " error=" + .error else "" end) +
		(if has("raw_answer") then " raw_answer=" + .raw_answer else "" end)'
}

# judged_failed_check is the name of the first check a judged row recorded as failed, and "none"
# for a row whose checks all passed.
judged_failed_check() { # <request id>
	local detail
	detail="$(judged_detail "$1")"
	[ -n "$detail" ] || { echo "no-row"; return; }
	printf '%s' "$detail" | jq -r '([.checks[] | select(.passed | not) | .check] | first) // "none"'
}

judged_field() { # <request id> <jq path>
	local detail
	detail="$(judged_detail "$1")"
	[ -n "$detail" ] || { echo ""; return; }
	printf '%s' "$detail" | jq -r "$2 // \"\""
}

# --- The two person-facing broker routes k10 and k10f drive ----------------------------------

# account_get and account_post call the routes a listed service calls with its own ServiceAccount
# token, relaying a signed-in person: the grant list and the human revoke. They are what
# `agent-secrets grant list|revoke` reach through this machine's login, which this pod's kernel
# cannot pin (the header comment).
account_get() { # <path>
	curl -sS -H "Authorization: Bearer $(cat "$AGENT_SECRETS_ACCOUNT_TOKEN_FILE")" "$AGENT_SECRETS_URL$1"
}

account_post() { # <path> <body>
	curl -sS -o "$WORK_DIR/account.out" -w '%{http_code}' -X POST \
		-H "Authorization: Bearer $(cat "$AGENT_SECRETS_ACCOUNT_TOKEN_FILE")" \
		-H 'Content-Type: application/json' --data "$2" "$AGENT_SECRETS_URL$1"
}

# sign_in signs one login in at the broker through the stack's own issuer, the way the approval
# page signs its person in, and answers whether the broker made a session for it. The issuer
# signs in whoever is queued at its loopback control route, so a second person needs no second
# issuer.
sign_in() { # <email> <label for this run's own directory>
	local who="$1" d="$WORK_DIR/signin-$2"
	mkdir -p "$d"
	curl -sS -o /dev/null -X POST -H 'Content-Type: application/json' \
		--data "$(jq -nc --arg e "$who" --arg d "${who#*@}" \
			'{sub: $e, email: $e, email_verified: true, hd: $d, "cognito:username": ("DevIdP_" + $e)}')" \
		"$AIRLOCK_SIGNIN_URL/_dev/sign-in-as"
	curl -sS -D "$d/start" -o /dev/null "$AGENT_SECRETS_URL/approve/0000000000000000000000000000000000000000000000000000000000000000"
	local authorize signin callback
	authorize="$(sed -n 's/^Location: //p' "$d/start" | tr -d '\r')"
	signin="$(sed -n 's/^Set-Cookie: \(__Host-broker-signin=[^;]*\).*/\1/p' "$d/start" | tr -d '\r')"
	[ -n "$authorize" ] || return 1
	curl -sS -D "$d/authorize" -o /dev/null "$authorize"
	callback="$(sed -n 's/^Location: //p' "$d/authorize" | tr -d '\r')"
	curl -sS -D "$d/callback" -o /dev/null -H "Cookie: $signin" "$callback"
	SIGNED_IN_COOKIE="$(sed -n 's/^Set-Cookie: \(__Host-broker-session=[^;]*\).*/\1/p' "$d/callback" | tr -d '\r')"
	[ -n "$SIGNED_IN_COOKIE" ]
}

# --- Forging a judged approval (k10c) ----------------------------------------------------------

# new_uuid is a version-4 UUID made here rather than in Postgres, so this script needs no
# function of a particular server version.
new_uuid() {
	local hex
	hex="$(od -An -tx1 -N16 /dev/urandom | tr -d ' \n')"
	printf '%s-%s-4%s-a%s-%s\n' "${hex:0:8}" "${hex:8:4}" "${hex:13:3}" "${hex:17:3}" "${hex:20:12}"
}

# forge_judged_approval writes, straight into the stack's Postgres, the rows a judge's approval
# of a pending record would have written: the record's approved decision event under the judge's
# own actor with no login, the request moved to granted and decided by that actor, a grant
# recording the record's own approver, and one placeholder for the name the rules bind to S. The
# record and its signed request object are the broker's own - this forges the decision, which is
# what "a record of your own that names judge:<model id> as the decider" is.
#
# It leaves the grant's id in FORGED_GRANT and its placeholder in FORGED_PLACEHOLDER, which is
# made here and printed nowhere.
forge_judged_approval() { # <request id> <record id> <name bound to S>
	local request="$1" record="$2" name="$3"
	FORGED_GRANT="$(new_uuid)"
	FORGED_PLACEHOLDER="agent-secrets-placeholder-$(od -An -tx1 -N24 /dev/urandom | tr -d ' \n')"
	local hash
	hash="$(printf '%s' "$FORGED_PLACEHOLDER" | sha256sum | cut -d' ' -f1)"
	psql_stack >/dev/null <<-SQL
		begin;
		insert into credential_request_events (record_id, event, login, actor)
		  values ('$record', 'approved', null, '$JUDGE_ACTOR');
		update requests set state='granted', decided_at=now(), decided_by='$JUDGE_ACTOR',
		  pending_expires_at=null where id='$request';
		insert into grants (id, request_id, enrollment_id, approver, expires_at)
		  select '$FORGED_GRANT', r.id, r.enrollment_id, cr.approver,
		         now() + make_interval(secs => r.lifetime_seconds)
		  from requests r join credential_requests cr on cr.id = r.record_id
		  where r.id = '$request';
		insert into airlock_placeholders (placeholder_sha256, placeholder, grant_id, name, slot, expires_at)
		  select '$hash', '$FORGED_PLACEHOLDER', '$FORGED_GRANT', '$name', 'value', g.expires_at
		  from grants g where g.id = '$FORGED_GRANT';
		commit;
	SQL
}

# broker_log_holds reports whether any broker of this stack logged a line holding the text, and
# leaves the last such line in BROKER_LOG_LINE.
broker_log_holds() { # <text>
	BROKER_LOG_LINE="$(grep -hF "$1" "${BROKER_LOGS[@]}" 2>/dev/null | tail -n 1 | tr -d '\n' || true)"
	[ -n "$BROKER_LOG_LINE" ]
}

###############################################################################################
# k10
###############################################################################################

# k10_case drives one request that breaks one of the judge's checks: it marks the model stand,
# asks, gives the run its budget, reads the state and the judged row back, and cancels whatever
# is still pending so the next case's names are free. The caller reads K10 for the answers.
declare -A K10=()

k10_case() { # <label> <ttl> <reason> <name>...
	local label="$1" ttl="$2" reason="$3"
	shift 3
	local mark
	mark="$(records_count model)"
	ask "$ttl" "$reason" "$@"
	K10[${label}_exit]="$ASK_STATUS"
	K10[${label}_state0]="$ASK_STATE"
	K10[${label}_lifetime]="$ASK_LIFETIME"
	if [ -n "$ASK_ID" ]; then
		# One run has 30 seconds of budget and this stack answers in milliseconds, so the row
		# is there in a second or two; 15 is the bound the wait is given, past which the case
		# reads whatever is there and the result block says so.
		await_judged "$ASK_ID" 15
		K10[${label}_state]="$(request_state "$ASK_ID")"
		K10[${label}_rows]="$(judged_rows "$ASK_ID")"
		K10[${label}_failed]="$(judged_failed_check "$ASK_ID")"
		K10[${label}_summary]="$(judged_summary "$ASK_ID")"
	else
		K10[${label}_state]="(no request)"
		K10[${label}_rows]=0
		K10[${label}_failed]="(no request)"
		K10[${label}_summary]="the broker refused the request: ${ASK_ERR}"
	fi
	K10[${label}_asked]="$(($(records_count model) - mark))"
	cancel_pending
}

check_k10() {
	expect "only the in-class request with the stand-in answering approve is granted, within 60 seconds, for 15 minutes, and agent-secrets status names judge:<model id>"
	expect "every other request stays pending, or is refused where the tag policy refuses it, and the stand-in is not asked about a request that fails a check"
	expect "each decision has a request.judged row with its checks, the model, the verdict and the reason, and exactly one such row per decision"
	expect "the owner's list shows the judged grant, the owner ends it, and the next call gets 407"
	expect "the negative control, with the checks before the model turned off (dev-airlock.sh --judge-skip-checks), grants the request that names a done issue"

	start_stack
	stack_facts
	local open="$AIRLOCK_DISPATCH_OPEN_ISSUE" done_issue="$AIRLOCK_DISPATCH_DONE_ISSUE"

	# --- The request in the class, with the stand answering approve. ---
	model_answers '{"verdict":"approve","reason":"the issue is open and the ask is inside the rules"}'
	local mark grant_lifetime
	mark="$(records_count model)"
	ask 15m "reach s.airlock.test for $open" "$AIRLOCK_JUDGED_SECRET"
	local in_id="$ASK_ID"
	await_granted "$in_id" 60
	local in_state="$AWAIT_STATE" in_seconds="$AWAIT_SECONDS"
	local in_decider in_grant
	in_decider="$(request_decider "$in_id")"
	in_grant="$(request_grant "$in_id")"
	grant_lifetime="$(psql_stack -c "select round(extract(epoch from (expires_at - created_at)))::int from grants where id='$in_grant'")"
	local in_rows in_summary in_asked
	in_rows="$(judged_rows "$in_id")"
	in_summary="$(judged_summary "$in_id")"
	in_asked="$(($(records_count model) - mark))"
	observe "the in-class request (one secret with judge $AIRLOCK_JUDGE_SECONDS, --ttl 15m, a reason naming the open issue $open): $in_state after ${in_seconds}s, decided by '$in_decider', grant lifetime ${grant_lifetime}s, the stand asked $in_asked time(s)"
	observe "its request.judged row ($in_rows row(s)): $in_summary"
	assert "$([ "$in_state" = granted ] && [ "$in_seconds" -le 60 ] && [ "$grant_lifetime" = 900 ] && [ "$in_decider" = "$JUDGE_ACTOR" ] && echo 0 || echo 1)" \
		"the in-class request is granted within 60 seconds, for 15 minutes, and agent-secrets status names judge:<model id>" \
		"it was $in_state after ${in_seconds}s with a ${grant_lifetime}-second grant, and agent-secrets status says decided_by '$in_decider' against the expected '$JUDGE_ACTOR'"

	# That grant is ended before the cases below: a request for the same names and the same
	# lifetime is handed a live grant back (requests' reuseLiveGrant) and never reaches the
	# judge at all, so each case below has to ask with none of its own standing.
	agent-secrets revoke "$in_grant" >/dev/null 2>&1 || true

	# --- The three answers the same in-class request gets from a model that is not approving. ---
	local answer label
	for answer in refer text delay stopped; do
		if [ "$answer" = stopped ]; then
			# Which broker the front hands this request to decides which process has to
			# be stopped, so the front is pointed at one of them first - the second,
			# because dev-airlock.sh waits on the first.
			front_backends "${BROKER_API_ADDRS[1]}" "${BROKER_PROXY_ADDRS[1]}"
		fi
		label="in_$answer"
		case "$answer" in
		refer) model_answers '{"verdict":"refer","reason":"this one is for the person who decides it today"}' ;;
		text) model_answers '{"text":"I think this is probably fine, but I am not going to say so in JSON."}' ;;
		delay) model_answers '{"delay":"30s"}' ;;
		stopped) model_answers '{"delay":"30s"}' ;;
		esac
		mark="$(records_count model)"
		ask 15m "reach s.airlock.test for $open" "$AIRLOCK_JUDGED_SECRET"
		if [ "$answer" = stopped ]; then
			# The broker stopped during the model call: the judge's run hangs off the
			# broker's own serving lifetime, so what has to stop is the process that
			# took the request. Which broker that is has been settled before the ask -
			# the API front was pointed at the second broker alone - and the second is
			# the one to stop, because dev-airlock.sh itself waits on the first and
			# would tear the whole stack down with it.
			sleep 2
			kill "${BROKER_PIDS[1]}" 2>/dev/null || true
			sleep 35
			front_backends "${BROKER_API_ADDRS[0]}" "${BROKER_PROXY_ADDRS[0]}"
			K10[${label}_state]="$(psql_stack -c "select state from requests where id='$ASK_ID'")"
			K10[${label}_rows]="$(judged_rows "$ASK_ID")"
			K10[${label}_asked]="$(($(records_count model) - mark))"
			K10[${label}_summary]="broker ${BROKER_PIDS[1]}, the one the front sent this request to, was stopped two seconds into the model call; the request's row reads '${K10[${label}_state]}' and it has ${K10[${label}_rows]} request.judged row(s)"
			cancel_pending
			continue
		fi
		await_judged "$ASK_ID" 35
		K10[${label}_state]="$(request_state "$ASK_ID")"
		K10[${label}_rows]="$(judged_rows "$ASK_ID")"
		K10[${label}_asked]="$(($(records_count model) - mark))"
		K10[${label}_summary]="$(judged_summary "$ASK_ID")"
		cancel_pending
	done

	observe "the same in-class request with the stand answering refer: ${K10[in_refer_state]}, ${K10[in_refer_rows]} judged row(s) — ${K10[in_refer_summary]}"
	observe "with the stand answering text that is not JSON: ${K10[in_text_state]}, ${K10[in_text_rows]} judged row(s) — ${K10[in_text_summary]}"
	observe "with the stand not answering for 30 seconds (past the judge's 20-second bound): ${K10[in_delay_state]}, ${K10[in_delay_rows]} judged row(s) — ${K10[in_delay_summary]}"
	observe "with the broker stopped during the model call: ${K10[in_stopped_summary]}"
	assert "$([ "${K10[in_refer_state]}" = pending ] && [ "${K10[in_text_state]}" = pending ] && [ "${K10[in_delay_state]}" = pending ] && [ "${K10[in_stopped_state]}" = pending ] && [ "${K10[in_stopped_rows]}" = 0 ] && echo 0 || echo 1)" \
		"refer, an answer that is not JSON, an answer that never comes and a broker stopped mid-call each leave the request pending, and the stopped broker writes nothing" \
		"refer '${K10[in_refer_state]}', text '${K10[in_text_state]}', 30-second delay '${K10[in_delay_state]}', broker stopped '${K10[in_stopped_state]}' with ${K10[in_stopped_rows]} rows written"

	# The second broker is gone and the fronts are back on the first, which serves every call
	# below: one broker answers any of this stack's calls, both reading one database.
	model_answers '{"verdict":"approve","reason":"the issue is open and the ask is inside the rules"}'

	# --- One request for each check a request can fail. ---
	k10_case two_secrets 10m "reach s.airlock.test for $open" "$AIRLOCK_JUDGED_SECRET" "$AIRLOCK_UNJUDGED_SECRET"
	k10_case no_ttl "" "reach s.airlock.test for $open" "$AIRLOCK_JUDGED_SECRET"
	k10_case long_ttl 30m "reach s.airlock.test for $open" "$AIRLOCK_JUDGED_SECRET"
	k10_case no_issue 10m "reach s.airlock.test, no issue named" "$AIRLOCK_JUDGED_SECRET"
	k10_case done_issue 10m "reach s.airlock.test for $done_issue" "$AIRLOCK_JUDGED_SECRET"
	k10_case no_judge 10m "reach s.airlock.test for $open" "$AIRLOCK_UNJUDGED_SECRET"
	psql_stack -c "insert into withheld_secrets (enrollment_id, name) values ('$AIRLOCK_ENROLLMENT', '$AIRLOCK_JUDGED_SECRET')" >/dev/null
	k10_case withheld 10m "reach s.airlock.test for $open" "$AIRLOCK_JUDGED_SECRET"
	psql_stack -c "delete from withheld_secrets where enrollment_id='$AIRLOCK_ENROLLMENT'" >/dev/null
	# The name the tag policy refuses: a secret whose tier tag is neither agent nor human, which
	# policy.Load refuses to serve at all (ReasonTierTagMalformed), so the broker denies the
	# request at creation and the judge never sees it.
	add_fake_secret "${STACK_SECRETS_PREFIX}judge-bad-tier" '{"owner":"shared","tier":"neither"}' 'the-stand-in-bad-tier-value'
	reread_all JUDGE_BAD_TIER
	k10_case bad_tier 10m "reach s.airlock.test for $open" JUDGE_BAD_TIER

	observe "two secrets: exit ${K10[two_secrets_exit]}, ${K10[two_secrets_state]}, stand asked ${K10[two_secrets_asked]} time(s), ${K10[two_secrets_rows]} judged row(s), first failed check '${K10[two_secrets_failed]}' — ${K10[two_secrets_summary]}"
	observe "no --ttl: exit ${K10[no_ttl_exit]}, ${K10[no_ttl_state]}, lifetime ${K10[no_ttl_lifetime]}s, stand asked ${K10[no_ttl_asked]} time(s), ${K10[no_ttl_rows]} judged row(s), first failed check '${K10[no_ttl_failed]}' — ${K10[no_ttl_summary]}"
	observe "no --ttl is read against the lifetime the broker actually gave it, not against the check's parenthesis: $AIRLOCK_JUDGED_SECRET is human tier, so a request that signs no lifetime gets requests.DefaultHumanGrantLifetime (15 minutes), not the 12-hour default of a name that is not human tier — and 900 seconds is inside both the class's 15 minutes and this entry's $AIRLOCK_JUDGE_SECONDS, so that request is in the class. The lifetime check's own refusal is the --ttl 30m case below."
	observe "--ttl 30m, past the $AIRLOCK_JUDGE_SECONDS seconds the rules give the judge: exit ${K10[long_ttl_exit]}, ${K10[long_ttl_state]}, stand asked ${K10[long_ttl_asked]} time(s), ${K10[long_ttl_rows]} judged row(s), first failed check '${K10[long_ttl_failed]}' — ${K10[long_ttl_summary]}"
	observe "a reason with no issue key: exit ${K10[no_issue_exit]}, ${K10[no_issue_state]}, stand asked ${K10[no_issue_asked]} time(s), ${K10[no_issue_rows]} judged row(s), first failed check '${K10[no_issue_failed]}' — ${K10[no_issue_summary]}"
	observe "a done issue ($done_issue): exit ${K10[done_issue_exit]}, ${K10[done_issue_state]}, stand asked ${K10[done_issue_asked]} time(s), ${K10[done_issue_rows]} judged row(s), first failed check '${K10[done_issue_failed]}' — ${K10[done_issue_summary]}"
	observe "a secret whose entry has no judge ($AIRLOCK_UNJUDGED_SECRET): exit ${K10[no_judge_exit]}, ${K10[no_judge_state]}, stand asked ${K10[no_judge_asked]} time(s), ${K10[no_judge_rows]} judged row(s), first failed check '${K10[no_judge_failed]}' — ${K10[no_judge_summary]}"
	observe "a name withheld for the session: exit ${K10[withheld_exit]}, ${K10[withheld_state]}, stand asked ${K10[withheld_asked]} time(s), ${K10[withheld_rows]} judged row(s), first failed check '${K10[withheld_failed]}' — ${K10[withheld_summary]}"
	observe "a name the tag policy refuses (tier 'neither'): exit ${K10[bad_tier_exit]}, state '${K10[bad_tier_state0]}', stand asked ${K10[bad_tier_asked]} time(s) — ${K10[bad_tier_summary]}"

	local still_pending=0 not_asked=0 case_label
	for case_label in two_secrets long_ttl no_issue done_issue no_judge withheld; do
		[ "${K10[${case_label}_state]}" = pending ] || still_pending=1
		[ "${K10[${case_label}_asked]}" = 0 ] || not_asked=1
	done
	assert "$([ "$still_pending" = 0 ] && [ "${K10[bad_tier_exit]}" != 0 ] && echo 0 || echo 1)" \
		"every request that fails one of the judge's checks stays pending, and the one the tag policy refuses is refused at creation" \
		"two secrets '${K10[two_secrets_state]}', --ttl 30m '${K10[long_ttl_state]}', no issue key '${K10[no_issue_state]}', a done issue '${K10[done_issue_state]}', no judge entry '${K10[no_judge_state]}', withheld '${K10[withheld_state]}'; the refused tag exited ${K10[bad_tier_exit]}"
	assert "$([ "$not_asked" = 0 ] && [ "${K10[bad_tier_asked]}" = 0 ] && [ "${K10[no_ttl_state]}" = granted ] && echo 0 || echo 1)" \
		"the stand-in is not asked about a request that fails a check, and the no---ttl request, whose default lifetime is inside the class, is granted" \
		"the stand was asked 0 times for each of the six that failed a check and for the refused tag; the no---ttl request, lifetime ${K10[no_ttl_lifetime]}s, was ${K10[no_ttl_state]}"

	local one_row=0 named_checks=0
	for case_label in two_secrets long_ttl no_issue done_issue no_judge withheld; do
		[ "${K10[${case_label}_rows]}" = 1 ] || one_row=1
		[ "${K10[${case_label}_failed]}" != none ] && [ "${K10[${case_label}_failed]}" != no-row ] || named_checks=1
	done
	local in_checks in_model in_verdict in_reason
	in_checks="$(printf '%s' "$(judged_detail "$in_id")" | jq -r '[.checks[].check] | join(",")')"
	in_model="$(judged_field "$in_id" .model)"
	in_verdict="$(judged_field "$in_id" .verdict)"
	in_reason="$(judged_field "$in_id" .reason)"
	observe "the in-class row's checks in order: $in_checks; model '$in_model', verdict '$in_verdict', reason '$in_reason'; its actor is '$(judged_actor "$in_id")'"
	assert "$([ "$one_row" = 0 ] && [ "$named_checks" = 0 ] && [ "$in_rows" = 1 ] && [ "$in_checks" = "rules-judge,lifetime,policy,withheld,issue" ] && [ "$in_model" = "$AIRLOCK_JUDGE_MODEL" ] && [ "$in_verdict" = approve ] && [ -n "$in_reason" ] && echo 0 || echo 1)" \
		"each decision has exactly one request.judged row, carrying its checks, the model, the verdict and the reason" \
		"the in-class decision has $in_rows row with checks $in_checks, model '$in_model', verdict '$in_verdict' and a reason of $(shape "$in_reason") (compared by SHA-256 and length); each of the six refused requests has one row naming the check it stopped at"

	# --- The owner lists the grant, ends it, and the next call is refused. ---
	# The judged grant was decided on the stack this run replaced, so one is made again here.
	mark="$(records_count model)"
	ask 15m "reach s.airlock.test for $open" "$AIRLOCK_JUDGED_SECRET"
	local owned_id="$ASK_ID"
	await_granted "$owned_id" 60
	local owned_grant
	owned_grant="$(request_grant "$owned_id")"
	release "$owned_grant" "$AIRLOCK_JUDGED_SECRET"
	local before_pw="$PROXY_PASSWORD" before_ph="$PLACEHOLDER"
	call_s "$before_pw" "$before_ph"
	local before_status="$CALL_STATUS"
	local listed listed_names revoke_status
	listed="$(account_get "/v1/grants?approver=$AIRLOCK_APPROVER")"
	listed_names="$(printf '%s' "$listed" | jq -r --arg g "$owned_grant" '[.grants[] | select(.grant_id == $g) | .names[]] | join(",")')"
	revoke_status="$(account_post "/v1/grants/$owned_grant/revoke-by-approver" "$(jq -nc --arg a "$AIRLOCK_APPROVER" '{approver: $a}')")"
	call_s "$before_pw" "$before_ph"
	local after_status="$CALL_STATUS"
	observe "the owner's list (GET /v1/grants?approver=$AIRLOCK_APPROVER, the route agent-secrets grant list reaches) holds grant $owned_grant over [$listed_names] with approver '$(printf '%s' "$listed" | jq -r --arg g "$owned_grant" '[.grants[] | select(.grant_id == $g) | .approver] | first // ""')'; the revoke answered $revoke_status; the call through the grant answered $before_status before it and $after_status after it"
	assert "$([ "$listed_names" = "$AIRLOCK_JUDGED_SECRET" ] && [ "$revoke_status" = 200 ] && [ "$before_status" = 200 ] && [ "$after_status" = 407 ] && echo 0 || echo 1)" \
		"the owner's list shows the judged grant, the owner ends it, and the next call gets 407" \
		"the list named [$listed_names], the revoke answered $revoke_status, and the call went $before_status then $after_status"

	stop_stack

	# --- The negative control: the checks before the model turned off. ---
	start_stack --judge-skip-checks
	stack_facts
	model_answers '{"verdict":"approve","reason":"asked about a request a check would have stopped"}'
	ask 10m "reach s.airlock.test for $done_issue" "$AIRLOCK_JUDGED_SECRET"
	local control_id="$ASK_ID"
	await_granted "$control_id" 60
	local control_state="$AWAIT_STATE" control_decider
	control_decider="$(request_decider "$control_id")"
	control "with BROKER_JUDGE_TEST_SKIP_CHECKS=true (dev-airlock.sh --judge-skip-checks), the request naming the done issue $done_issue was $control_state after ${AWAIT_SECONDS}s, decided by '$control_decider'"
	control "its judged row: $(judged_summary "$control_id")"
	assert "$([ "$control_state" = granted ] && [ "$control_decider" = "$JUDGE_ACTOR" ] && echo 0 || echo 1)" \
		"negative control: with the checks before the model turned off, the request that names a done issue is granted" \
		"it was $control_state, decided by '$control_decider'; on the stack with the checks on the same request stayed '${K10[done_issue_state]}'"

	stop_stack
}

###############################################################################################
# k10b
###############################################################################################

check_k10b() {
	expect "the judge package imports agentloop and no provider SDK directly, and makes no HTTP call to a model provider of its own"
	expect "the negative control, a scratch copy of the package that also imports github.com/anthropics/anthropic-sdk-go, lists it among its direct imports, which the check reports as a failure"

	local imports
	imports="$(cd "$ENVOY_DIR" && go list -f '{{join .Imports "\n"}}' ./internal/broker/judge)"
	printf '%s\n' "$imports" >"$WORK_DIR/imports.txt"
	# Every provider SDK a model call could be made with directly, so "no provider SDK" is a
	# list and not one name: a package that swapped anthropic-sdk-go for one of the others
	# would still be making its own model calls.
	local sdks="anthropic-sdk-go openai-go sashabaranov/go-openai google.golang.org/genai generative-ai-go cloud.google.com/go/aiplatform aws-sdk-go-v2/service/bedrock"
	local sdk found=""
	for sdk in $sdks; do
		grep -qF "$sdk" "$WORK_DIR/imports.txt" && found="$found $sdk"
	done
	local has_agentloop=1
	grep -qxF "github.com/sjawhar/envoy/agentloop" "$WORK_DIR/imports.txt" && has_agentloop=0
	# The one net/http in the package is the Dispatch read. The file that calls the model
	# (model.go) is checked for itself, since a provider call made with the standard library
	# would import no SDK at all.
	local http_files model_http=""
	http_files="$(cd "$ENVOY_DIR/internal/broker/judge" && grep -l '"net/http"' ./*.go | grep -v '_test\.go$' | tr '\n' ' ')"
	contains "model.go" "$http_files" && model_http="model.go"

	observe "go list -f '{{join .Imports \"\\n\"}}' ./internal/broker/judge: $(tr '\n' ' ' <"$WORK_DIR/imports.txt")"
	observe "its net/http is in [$http_files] alone, the judge's Dispatch read; model.go, the file that calls the model, imports $(cd "$ENVOY_DIR/internal/broker/judge" && sed -n '/^import (/,/^)/p' model.go | tr -d '\t"' | tr '\n' ' ')"
	assert "$([ "$has_agentloop" = 0 ] && [ -z "$found" ] && [ -z "$model_http" ] && echo 0 || echo 1)" \
		"the judge package imports agentloop and no provider SDK directly, and makes no HTTP call to a model provider of its own" \
		"its direct imports hold github.com/sjawhar/envoy/agentloop and none of [$sdks]${found:+ (found:$found)}; the file that asks the model, model.go, imports no net/http, so the one HTTP client in the package is the Dispatch read in [$http_files]"

	# --- The negative control: the same package with one import added. ---
	local scratch="$WORK_DIR/scratch"
	mkdir -p "$scratch/judge"
	cp "$ENVOY_DIR"/internal/broker/judge/*.go "$scratch/judge/"
	rm -f "$scratch/judge"/*_test.go
	printf 'module github.com/sjawhar/envoy\n\ngo 1.25.0\n' >"$scratch/go.mod"
	printf 'package judge\n\nimport _ "github.com/anthropics/anthropic-sdk-go"\n' >"$scratch/judge/scratch_provider.go"
	local control_imports control_found=""
	control_imports="$(cd "$scratch" && GOWORK=off GOFLAGS=-mod=mod GOPROXY=off go list -e -f '{{join .Imports "\n"}}' ./judge 2>"$WORK_DIR/scratch.err")"
	printf '%s\n' "$control_imports" >"$WORK_DIR/control-imports.txt"
	for sdk in $sdks; do
		grep -qF "$sdk" "$WORK_DIR/control-imports.txt" && control_found="$control_found $sdk"
	done
	control "a scratch copy of the package under \$TMPDIR with one file added, 'import _ \"github.com/anthropics/anthropic-sdk-go\"', lists these direct imports: $(tr '\n' ' ' <"$WORK_DIR/control-imports.txt")"
	control "the same reading of that list finds the provider SDK[$control_found], which is the failure this check reports"
	assert "$([ -n "$control_found" ] && echo 0 || echo 1)" \
		"negative control: the scratch copy's direct imports list anthropic-sdk-go, which the check reports as a failure" \
		"the scratch copy's list holds[$control_found], where the package's own holds none"
}

###############################################################################################
# k10c
###############################################################################################

# k10c_forge makes one record the stack's own way - a real request from the enrolled session,
# which the judge refers so the broker leaves it pending - and then writes the judged approval
# into Postgres by hand. It answers the grant in FORGED_GRANT and its placeholder in
# FORGED_PLACEHOLDER.
k10c_forge() { # <ttl> <reason> <name bound to S> <name>...
	local ttl="$1" reason="$2" bound_name="$3"
	shift 3
	ask "$ttl" "$reason" "$@"
	if [ "$ASK_STATE" != pending ]; then
		echo "judge-check: the record for [$*] was $ASK_STATE, not pending; the stand must refer before a record is forged" >&2
		return 1
	fi
	sleep 10
	forge_judged_approval "$ASK_ID" "$ASK_RECORD" "$bound_name"
}

declare -A K10C=()

# curl_through is the command `agent-secrets NAME -- …` starts: curl to S carrying the grant's
# placeholder in its Authorization header, which the proxy swaps the real value into on its way
# out. The CLI puts the placeholder in the environment under the secret's own name and starts
# the command with no shell, so the command is a shell of its own: the two variables are the
# child's to expand, never this script's.
curl_through() { # <the secret whose placeholder goes in the header>
	# shellcheck disable=SC2016 # $<NAME> and $AIRLOCK_S_URL are the child shell's to expand, not this one's
	printf '%s' 'curl -sS -o /dev/null -w "%{http_code}" -H "Authorization: Bearer $'"$1"'" "$AIRLOCK_S_URL/ok"'
}

# k10c_call runs the CLI as the check names it - agent-secrets NAME -- curl through the proxy
# port to S - and then makes the same call with the forged grant's own placeholder, which is the
# one way to put that grant in front of the proxy once the broker has refused to hand it back.
k10c_call() { # <label> <the secret the header carries> <ttl> <reason> <name>...
	local label="$1" header="$2" ttl="$3" reason="$4"
	shift 4
	local mark
	mark="$(records_count s)"
	# No `set -e` toggling here: this script turns errexit off once, before the first check
	# runs, and a check that turned it back on would end the run at the next command that
	# answered non-zero - which, for a judge check, is every request the judge leaves pending
	# (agent-secrets exits 75 for one).
	agent-secrets "$@" --ttl "$ttl" --reason "$reason" --wait 0 -- \
		sh -c "$(curl_through "$header")" >"$WORK_DIR/$label.out" 2>"$WORK_DIR/$label.err"
	K10C[${label}_exit]=$?
	K10C[${label}_cli]="$(tr '\n' ' ' <"$WORK_DIR/$label.err" | cut -c1-240)"
	K10C[${label}_out]="$(tr -d '\n' <"$WORK_DIR/$label.out")"
	call_s "$FORGED_PLACEHOLDER" "a-placeholder-the-proxy-never-swaps"
	K10C[${label}_proxy]="$CALL_STATUS"
	K10C[${label}_body]="$(printf '%s' "$CALL_BODY" | tr -d '\n' | cut -c1-200)"
	K10C[${label}_recorded]="$(($(records_count s) - mark))"
	cancel_pending
}

check_k10c() {
	expect "the judge's own grant releases and S gets the real value"
	expect "each of the three forged records is refused by the chain check: the CLI exits non-zero, S records nothing, and the broker's log names the chain refusal"
	expect "the negative control, a fourth record written the same way but inside every rule, releases"

	start_stack
	stack_facts
	local open="$AIRLOCK_DISPATCH_OPEN_ISSUE"
	local judged_value
	judged_value="$(stored_value "${STACK_SECRETS_PREFIX}airlock-judged-token")"

	# --- The judge's own grant. ---
	model_answers '{"verdict":"approve","reason":"the issue is open and the ask is inside the rules"}'
	ask 15m "reach s.airlock.test for $open" "$AIRLOCK_JUDGED_SECRET"
	await_granted "$ASK_ID" 60
	local judged_state="$AWAIT_STATE" judged_grant
	judged_grant="$(request_grant "$ASK_ID")"
	release "$judged_grant" "$AIRLOCK_JUDGED_SECRET"
	call_s "$PROXY_PASSWORD" "$PLACEHOLDER"
	local own_status="$CALL_STATUS" own_header
	own_header="$(s_authorization)"
	observe "the judge's own grant: the request was $judged_state, decided by '$(request_decider "$ASK_ID")'; the call through its proxy port answered $own_status and S was sent $(shape "$own_header") against the stored value's $(shape "Bearer $judged_value") (compared by SHA-256 and length)"
	assert "$([ "$judged_state" = granted ] && [ "$own_status" = 200 ] && [ "$own_header" = "Bearer $judged_value" ] && echo 0 || echo 1)" \
		"the judge's own grant releases and S gets the real value" \
		"the call answered $own_status and the header S recorded matches the stored value by SHA-256 and length"

	# --- Three records of this script's own, each outside one rule, and a fourth inside them
	#     all. Each is a real record the broker made, with the judged approval written by hand.
	model_answers '{"verdict":"refer","reason":"left pending on purpose, so the approval below is written by hand"}'

	k10c_forge 10m "reach s.airlock.test for $open" "$AIRLOCK_JUDGED_SECRET" "$AIRLOCK_JUDGED_SECRET" "$AIRLOCK_UNJUDGED_SECRET"
	local two_grant="$FORGED_GRANT"
	k10c_call two_secrets "$AIRLOCK_JUDGED_SECRET" 10m "reach s.airlock.test for $open" "$AIRLOCK_JUDGED_SECRET" "$AIRLOCK_UNJUDGED_SECRET"

	k10c_forge 30m "reach s.airlock.test for $open" "$AIRLOCK_JUDGED_SECRET" "$AIRLOCK_JUDGED_SECRET"
	local long_grant="$FORGED_GRANT"
	k10c_call long_ttl "$AIRLOCK_JUDGED_SECRET" 30m "reach s.airlock.test for $open" "$AIRLOCK_JUDGED_SECRET"

	k10c_forge 10m "reach s.airlock.test, no issue named" "$AIRLOCK_SHARED_SECRET" "$AIRLOCK_SHARED_SECRET"
	local no_issue_grant="$FORGED_GRANT"
	k10c_call no_issue "$AIRLOCK_SHARED_SECRET" 10m "reach s.airlock.test, no issue named" "$AIRLOCK_SHARED_SECRET"

	local refusal="this grant's approval chain no longer verifies"
	local logged=0
	broker_log_holds "$refusal" || logged=1
	observe "the forged record naming two secrets (grant $two_grant): the CLI exited ${K10C[two_secrets_exit]} — '${K10C[two_secrets_cli]}'; the call through its own placeholder answered ${K10C[two_secrets_proxy]} '${K10C[two_secrets_body]}'; S recorded ${K10C[two_secrets_recorded]} request(s)"
	observe "the forged record carrying 1800 seconds, past the $AIRLOCK_JUDGE_SECONDS the entry allows the judge (grant $long_grant): the CLI exited ${K10C[long_ttl_exit]} — '${K10C[long_ttl_cli]}'; the call through its own placeholder answered ${K10C[long_ttl_proxy]} '${K10C[long_ttl_body]}'; S recorded ${K10C[long_ttl_recorded]} request(s)"
	observe "the forged record whose reason names no issue (grant $no_issue_grant): the CLI exited ${K10C[no_issue_exit]} — '${K10C[no_issue_cli]}'; the call through its own placeholder answered ${K10C[no_issue_proxy]} '${K10C[no_issue_body]}'; S recorded ${K10C[no_issue_recorded]} request(s)"
	observe "the broker's log: $(broker_log_holds "$refusal" && printf '%s' "$BROKER_LOG_LINE" | cut -c1-300 || echo 'no line naming the chain refusal')"
	assert "$([ "${K10C[two_secrets_exit]}" != 0 ] && [ "${K10C[long_ttl_exit]}" != 0 ] && [ "${K10C[no_issue_exit]}" != 0 ] &&
		[ "${K10C[two_secrets_recorded]}" = 0 ] && [ "${K10C[long_ttl_recorded]}" = 0 ] && [ "${K10C[no_issue_recorded]}" = 0 ] &&
		[ "${K10C[two_secrets_proxy]}" = 403 ] && [ "${K10C[long_ttl_proxy]}" = 403 ] && [ "${K10C[no_issue_proxy]}" = 403 ] &&
		contains "$refusal" "${K10C[two_secrets_body]}" && contains "$refusal" "${K10C[long_ttl_body]}" && contains "$refusal" "${K10C[no_issue_body]}" &&
		[ "$logged" = 0 ] && echo 0 || echo 1)" \
		"each of the three forged records is refused by the chain check: the CLI exits non-zero, S records nothing, and the broker's log names the chain refusal" \
		"the three CLI runs exited ${K10C[two_secrets_exit]}, ${K10C[long_ttl_exit]} and ${K10C[no_issue_exit]}; the three calls through the forged placeholders answered ${K10C[two_secrets_proxy]}, ${K10C[long_ttl_proxy]} and ${K10C[no_issue_proxy]}, each in the words '$refusal'; S recorded ${K10C[two_secrets_recorded]}, ${K10C[long_ttl_recorded]} and ${K10C[no_issue_recorded]} requests, and the broker logged the refusal"

	# --- The negative control: a fourth record written the same way, inside every rule. ---
	local mark
	mark="$(records_count s)"
	k10c_forge 10m "reach s.airlock.test for $open" "$AIRLOCK_JUDGED_SECRET" "$AIRLOCK_JUDGED_SECRET"
	local good_grant="$FORGED_GRANT"
	agent-secrets "$AIRLOCK_JUDGED_SECRET" --ttl 10m --reason "reach s.airlock.test for $open" --wait 0 -- \
		sh -c "$(curl_through "$AIRLOCK_JUDGED_SECRET")" >"$WORK_DIR/control.out" 2>"$WORK_DIR/control.err"
	local control_exit=$?
	local control_header control_recorded
	control_header="$(s_authorization)"
	control_recorded="$(($(records_count s) - mark))"
	control "a fourth record written the same way but inside every rule (one secret, 600 seconds against the entry's $AIRLOCK_JUDGE_SECONDS, a reason naming the open issue $open), grant $good_grant: agent-secrets $AIRLOCK_JUDGED_SECRET -- curl exited $control_exit and curl answered '$(tr -d '\n' <"$WORK_DIR/control.out")'; S recorded $control_recorded request(s) and was sent $(shape "$control_header") against the stored value's $(shape "Bearer $judged_value") (compared by SHA-256 and length)"
	control "its CLI line: $(tr '\n' ' ' <"$WORK_DIR/control.err" | cut -c1-200)"
	assert "$([ "$control_exit" = 0 ] && [ "$(tr -d '\n' <"$WORK_DIR/control.out")" = 200 ] && [ "$control_header" = "Bearer $judged_value" ] && echo 0 || echo 1)" \
		"negative control: a fourth record written the same way but inside every rule releases, so the refusals come from the chain check and not from the forgery itself" \
		"it exited $control_exit, S answered 200 and the value it was sent matches the store by SHA-256 and length"

	stop_stack
}

###############################################################################################
# k10d
###############################################################################################

# k10d_rules writes a copy of the stack's rules file with one entry's judge seconds changed,
# which is the one way to give a broker other rules: cmd/broker/main.go reads
# BROKER_AIRLOCK_RULES_FILE once, at boot.
k10d_rules() { # <path> <seconds>
	jq --arg n "$AIRLOCK_JUDGED_SECRET" --argjson s "$2" \
		'.secrets = [.secrets[] | if .name == $n then .judge = $s else . end]' \
		"$AIRLOCK_RULES_FILE" >"$1"
}

check_k10d() {
	expect "the --ttl 15m request stays pending, the stand-in is not asked about it, and its request.judged row names the failed lifetime check"
	expect "the --ttl 5m request is granted by judge:<model id>"
	expect "the negative control, with the judge's lifetime comparison taken out, grants the --ttl 15m request"

	start_stack
	stack_facts
	local open="$AIRLOCK_DISPATCH_OPEN_ISSUE"
	model_answers '{"verdict":"approve","reason":"the issue is open and the ask is inside the rules"}'

	# The test secret's entry given judge 300, on a broker of this stack's own database,
	# stand-ins and certificate authority, with the stack's two fronts pointed at it.
	k10d_rules "$WORK_DIR/rules-300.json" 300
	start_extra_broker judge-300 BROKER_AIRLOCK_RULES_FILE="$WORK_DIR/rules-300.json"
	front_backends "$EXTRA_API_ADDR" "$EXTRA_PROXY_ADDR"

	local mark
	mark="$(records_count model)"
	ask 15m "reach s.airlock.test for $open" "$AIRLOCK_JUDGED_SECRET"
	local long_id="$ASK_ID"
	await_judged "$long_id" 15
	local long_state long_asked long_failed long_rows long_summary
	long_state="$(request_state "$long_id")"
	long_asked="$(($(records_count model) - mark))"
	long_failed="$(judged_failed_check "$long_id")"
	long_rows="$(judged_rows "$long_id")"
	long_summary="$(judged_summary "$long_id")"
	cancel_pending

	mark="$(records_count model)"
	ask 5m "reach s.airlock.test for $open" "$AIRLOCK_JUDGED_SECRET"
	local short_id="$ASK_ID"
	await_granted "$short_id" 60
	local short_state="$AWAIT_STATE" short_decider short_asked
	short_decider="$(request_decider "$short_id")"
	short_asked="$(($(records_count model) - mark))"
	cancel_pending

	observe "with the entry's judge at 300 seconds, --ttl 15m: $long_state, the stand asked $long_asked time(s), $long_rows judged row(s), first failed check '$long_failed' — $long_summary"
	observe "with the same entry, --ttl 5m: $short_state after ${AWAIT_SECONDS}s, decided by '$short_decider', the stand asked $short_asked time(s) — $(judged_summary "$short_id")"
	assert "$([ "$long_state" = pending ] && [ "$long_asked" = 0 ] && [ "$long_failed" = lifetime ] && [ "$long_rows" = 1 ] && echo 0 || echo 1)" \
		"the --ttl 15m request stays pending, the stand is not asked about it, and its request.judged row names the failed lifetime check" \
		"it was $long_state with the stand asked $long_asked times and one row whose first failed check is '$long_failed'"
	assert "$([ "$short_state" = granted ] && [ "$short_decider" = "$JUDGE_ACTOR" ] && echo 0 || echo 1)" \
		"the --ttl 5m request is granted by judge:<model id>" \
		"it was $short_state, decided by '$short_decider' against the expected '$JUDGE_ACTOR'"

	# --- The negative control. ---
	#
	# BROKER_JUDGE_TEST_SKIP_CHECKS is not an honest control for this check, and this run shows
	# why rather than asserting it: it turns off the judge's own five checks as a bar, and
	# leaves record.MayJudge inside requests.Machine.ApplyJudgeDecision, which holds a judged
	# approval to the same entry's seconds. The request is put to the model and the approval is
	# then refused, so it stays pending and the control would prove nothing.
	#
	# What takes the lifetime comparison out is the entry's seconds at the class maximum
	# (airlock's maxJudgeSeconds, 900): a judged request is already bounded at 15 minutes by the
	# class itself (judge's maxJudgedLifetime), so at 900 neither the judge's comparison nor
	# record.MayJudge's can refuse a --ttl 15m request, and nothing else about the run changes.
	front_backends_stack
	start_extra_broker judge-300-skip-checks \
		BROKER_AIRLOCK_RULES_FILE="$WORK_DIR/rules-300.json" BROKER_JUDGE_TEST_SKIP_CHECKS=true
	front_backends "$EXTRA_API_ADDR" "$EXTRA_PROXY_ADDR"
	mark="$(records_count model)"
	ask 15m "reach s.airlock.test for $open" "$AIRLOCK_JUDGED_SECRET"
	local skip_id="$ASK_ID"
	await_judged "$skip_id" 15
	local skip_state skip_asked
	skip_state="$(request_state "$skip_id")"
	skip_asked="$(($(records_count model) - mark))"
	cancel_pending

	front_backends_stack
	k10d_rules "$WORK_DIR/rules-900.json" 900
	start_extra_broker judge-900 BROKER_AIRLOCK_RULES_FILE="$WORK_DIR/rules-900.json"
	front_backends "$EXTRA_API_ADDR" "$EXTRA_PROXY_ADDR"
	mark="$(records_count model)"
	ask 15m "reach s.airlock.test for $open" "$AIRLOCK_JUDGED_SECRET"
	local control_id="$ASK_ID"
	await_granted "$control_id" 60
	local control_state="$AWAIT_STATE" control_decider control_asked
	control_decider="$(request_decider "$control_id")"
	control_asked="$(($(records_count model) - mark))"

	control "the control is a second broker on the stack's own database, stand-ins and authority whose only difference is that entry's judge seconds: 900, the class maximum. A judged request is bounded at 15 minutes by the class itself, so at 900 neither the judge's own lifetime comparison nor record.MayJudge's can refuse a --ttl 15m request — the comparison is taken out of the decision and nothing else moves. The same --ttl 15m request there was $control_state after ${AWAIT_SECONDS}s, decided by '$control_decider', with the stand asked $control_asked time(s)."
	control "BROKER_JUDGE_TEST_SKIP_CHECKS was not used as this check's control, and this run says why: a third broker with the judge 300 rules and BROKER_JUDGE_TEST_SKIP_CHECKS=true put the --ttl 15m request to the model ($skip_asked call(s) to the stand) and the request was still '$skip_state' — record.MayJudge, inside ApplyJudgeDecision, refuses the approval on the same entry's seconds, so that setting leaves the lifetime comparison in."
	control "its judged row under skip-checks: $(judged_summary "$skip_id")"
	assert "$([ "$control_state" = granted ] && [ "$control_decider" = "$JUDGE_ACTOR" ] && [ "$control_asked" -ge 1 ] && echo 0 || echo 1)" \
		"negative control: with the judge's lifetime comparison taken out, the --ttl 15m request is granted" \
		"with the entry at the class maximum the same request was $control_state, decided by '$control_decider', where at judge 300 it stayed '$long_state'"

	stop_stack
}

###############################################################################################
# k10e
###############################################################################################

check_k10e() {
	expect "both calls answer 200 with the real value at S, and the grant outlives the policy-version move"
	expect "the negative control, a second judged grant whose stored decider is no longer the record's own approver, gets 407 after the same move"

	start_stack
	stack_facts
	local open="$AIRLOCK_DISPATCH_OPEN_ISSUE"
	local judged_value
	judged_value="$(stored_value "${STACK_SECRETS_PREFIX}airlock-judged-token")"
	model_answers '{"verdict":"approve","reason":"the issue is open and the ask is inside the rules"}'

	# Two judged grants of the owned human-tier test secret, both made before the move, so the
	# release re-check runs over each: the lifetimes differ, since a request for the same names
	# and the same lifetime is handed the first grant back (reuseLiveGrant).
	ask 15m "reach s.airlock.test for $open" "$AIRLOCK_JUDGED_SECRET"
	local main_id="$ASK_ID"
	await_granted "$main_id" 60
	local main_state="$AWAIT_STATE" main_grant
	main_grant="$(request_grant "$main_id")"
	release "$main_grant" "$AIRLOCK_JUDGED_SECRET"
	local main_pw="$PROXY_PASSWORD" main_ph="$PLACEHOLDER"
	call_s "$main_pw" "$main_ph"
	local before_status="$CALL_STATUS" before_header
	before_header="$(s_authorization)"

	ask 10m "reach s.airlock.test for $open" "$AIRLOCK_JUDGED_SECRET"
	local control_req="$ASK_ID"
	await_granted "$control_req" 60
	local control_grant
	control_grant="$(request_grant "$control_req")"
	release "$control_grant" "$AIRLOCK_JUDGED_SECRET"
	local control_pw="$PROXY_PASSWORD" control_ph="$PLACEHOLDER"

	local version_before version_after
	version_before="$(psql_stack -c "select rules_version from requests where id='$main_id'")"

	# The policy version moves. The rules file is not what moves it: cmd/broker/main.go reads
	# that file once at boot, and no running broker rereads it, so an unrelated entry of the
	# *store* is what a live broker rereads - here one unrelated secret's tags, which
	# policy.Set's version digest folds in.
	set_fake_tags "${STACK_SECRETS_PREFIX}airlock-demo-token" '{"owner":"legion-daemon","tier":"agent","max-grant":"6h"}'
	reread_all AIRLOCK_DEMO_TOKEN
	sleep 2
	ask 7m "reach s.airlock.test for $open" "$AIRLOCK_JUDGED_SECRET"
	local probe_id="$ASK_ID"
	await_granted "$probe_id" 60
	version_after="$(psql_stack -c "select rules_version from requests where id='$probe_id'")"

	call_s "$main_pw" "$main_ph"
	local after_status="$CALL_STATUS" after_header
	after_header="$(s_authorization)"

	observe "the judged grant of the owned human-tier secret ($main_state, decided by '$(request_decider "$main_id")', grant $main_grant): the call before the move answered $before_status and S was sent $(shape "$before_header"); the call after it answered $after_status and S was sent $(shape "$after_header"); the stored value is $(shape "Bearer $judged_value") (all compared by SHA-256 and length)"
	observe "the policy version moved from '$version_before' to '$version_after' when one unrelated secret's tags changed and the brokers reread it"
	assert "$([ "$before_status" = 200 ] && [ "$after_status" = 200 ] && [ "$before_header" = "Bearer $judged_value" ] && [ "$after_header" = "Bearer $judged_value" ] && [ "$version_before" != "$version_after" ] && echo 0 || echo 1)" \
		"both calls answer 200 with the real value at S, and the grant outlives the policy-version move" \
		"the two calls answered $before_status and $after_status, each carrying the stored value by SHA-256 and length, across a version that moved from '$version_before' to '$version_after'"

	# --- The negative control. ---
	#
	# The document names grants.approver. That column is where requests' decider.grantApprover
	# puts the record's own approver for a judged grant, and it is what the grant list and the
	# human revoke read (k10f); no release check reads it. So this run makes the document's own
	# write first and reports what the call then answers, and then makes the write the release
	# re-check does read - requests.decided_by, where the judge's approval records its actor -
	# leaving it a login that is not the record's approver, which is the state the check's
	# sentence is about.
	psql_stack -c "update grants set approver='$JUDGE_ACTOR' where id='$control_grant'" >/dev/null
	call_s "$control_pw" "$control_ph"
	local approver_status="$CALL_STATUS"
	psql_stack -c "update requests set decided_by='someone-else@${AIRLOCK_APPROVER#*@}' where id='$control_req'" >/dev/null
	call_s "$control_pw" "$control_ph"
	local decider_status="$CALL_STATUS" decider_body
	decider_body="$(printf '%s' "$CALL_BODY" | tr -d '\n' | cut -c1-200)"

	control "on a second judged grant ($control_grant) made before the same move, grants.approver set to '$JUDGE_ACTOR' rather than the record's own approver: the call after the move answered $approver_status. That column is the grant list's and the revoke rule's (requests.GrantsForApprover, mayRevoke); the release re-check reads requests.decided_by, so this write alone does not move the release."
	control "the same grant with requests.decided_by left a login that is not the record's approver: the call after the move answered $decider_status '$decider_body'"
	assert "$([ "$decider_status" = 407 ] && echo 0 || echo 1)" \
		"negative control: a judged grant whose stored decider is no longer the record's own approver gets 407 after the same move" \
		"it answered $decider_status, where the grant whose decider the judge itself recorded answered $after_status across the same move"

	stop_stack
}

###############################################################################################
# k10f
###############################################################################################

check_k10f() {
	expect "both logins list the judged grant, the second login ends it, and the call after the revoke gets 407"
	expect "the negative control, a judged grant of an owned secret, is not in the list of a login other than its owner's"

	start_stack
	stack_facts
	local open="$AIRLOCK_DISPATCH_OPEN_ISSUE"
	local first="$AIRLOCK_APPROVER" second="someone-else@${AIRLOCK_APPROVER#*@}"
	model_answers '{"verdict":"approve","reason":"the issue is open and the ask is inside the rules"}'

	# A judged grant of the shared human-tier test secret, which nobody owns and anyone signed
	# in approves (record.AnyoneApprover).
	ask 15m "reach s.airlock.test for $open" "$AIRLOCK_SHARED_SECRET"
	local shared_id="$ASK_ID"
	await_granted "$shared_id" 60
	local shared_state="$AWAIT_STATE" shared_grant shared_decider
	shared_grant="$(request_grant "$shared_id")"
	shared_decider="$(request_decider "$shared_id")"
	release "$shared_grant" "$AIRLOCK_SHARED_SECRET"
	local shared_pw="$PROXY_PASSWORD" shared_ph="$PLACEHOLDER"

	# And one of the owned human-tier secret, for the control.
	ask 10m "reach s.airlock.test for $open" "$AIRLOCK_JUDGED_SECRET"
	local owned_id="$ASK_ID"
	await_granted "$owned_id" 60
	local owned_grant
	owned_grant="$(request_grant "$owned_id")"

	local first_in second_in
	sign_in "$first" first && first_in=yes || first_in=no
	sign_in "$second" second && second_in=yes || second_in=no

	local first_list second_list
	first_list="$(account_get "/v1/grants?approver=$first")"
	second_list="$(account_get "/v1/grants?approver=$second")"
	local first_has second_has first_approver second_approver
	first_has="$(printf '%s' "$first_list" | jq -r --arg g "$shared_grant" '[.grants[] | select(.grant_id == $g)] | length')"
	second_has="$(printf '%s' "$second_list" | jq -r --arg g "$shared_grant" '[.grants[] | select(.grant_id == $g)] | length')"
	first_approver="$(printf '%s' "$first_list" | jq -r --arg g "$shared_grant" '[.grants[] | select(.grant_id == $g) | .approver] | first // ""')"
	second_approver="$(printf '%s' "$second_list" | jq -r --arg g "$shared_grant" '[.grants[] | select(.grant_id == $g) | .approver] | first // ""')"
	local owned_in_second
	owned_in_second="$(printf '%s' "$second_list" | jq -r --arg g "$owned_grant" '[.grants[] | select(.grant_id == $g)] | length')"

	call_s "$shared_pw" "$shared_ph"
	local before_status="$CALL_STATUS"
	local revoke_status
	revoke_status="$(account_post "/v1/grants/$shared_grant/revoke-by-approver" "$(jq -nc --arg a "$second" '{approver: $a}')")"
	call_s "$shared_pw" "$shared_ph"
	local after_status="$CALL_STATUS"
	local revoked_by
	revoked_by="$(psql_stack -c "select coalesce(revoked_by,'') from grants where id='$shared_grant'")"

	observe "the judged grant of the shared secret ($shared_state, grant $shared_grant): the broker recorded '$shared_decider' as its decider on the request row and the record's own approver, '$first_approver', on the grant"
	observe "both logins are signed in at the broker ($first: $first_in, $second: $second_in); each lists the grant ($first: $first_has row(s), $second: $second_has row(s)), and the list names its approver '$first_approver' and '$second_approver'"
	observe "the grant list (GET /v1/grants?approver=<login>, the route agent-secrets grant list reaches) names the record's own approver and not the decider: for a judged grant the broker records the decider in requests.decided_by and the request.judged row, which for this grant read '$shared_decider' and '$(judged_actor "$shared_id")'"
	observe "the second login's revoke answered $revoke_status and the grants row reads revoked_by '$revoked_by'; the call through the grant answered $before_status before it and $after_status after it"
	assert "$([ "$first_in" = yes ] && [ "$second_in" = yes ] && [ "$first_has" = 1 ] && [ "$second_has" = 1 ] &&
		[ "$shared_decider" = "$JUDGE_ACTOR" ] && [ "$(judged_actor "$shared_id")" = "$JUDGE_ACTOR" ] &&
		[ "$revoke_status" = 200 ] && [ "$before_status" = 200 ] && [ "$after_status" = 407 ] && echo 0 || echo 1)" \
		"both logins list the judged grant whose decider is judge:<model id>, the second login ends it, and the call after the revoke gets 407" \
		"both logins signed in and each listed the grant once; its decider is '$shared_decider' on the request row and '$(judged_actor "$shared_id")' on its request.judged row, while the list's own approver field carries the record's approver '$first_approver'; the revoke answered $revoke_status and the call went $before_status then $after_status"

	control "a judged grant of the owned human-tier secret ($owned_grant, approver $first) is in the list of a login other than its owner's $owned_in_second time(s); the shared grant is in that same list $second_has time(s)"
	assert "$([ "$owned_in_second" = 0 ] && echo 0 || echo 1)" \
		"negative control: a judged grant of an owned secret is not in the list of a login other than its owner's" \
		"$second's list holds the shared grant $second_has time(s) and the owned one $owned_in_second time(s)"

	stop_stack
}

###############################################################################################

# From here on a step that answers non-zero is a property that did not pass, not the end of the
# run: a check must report every one of its properties, and a refused request, a command that
# exited 1 and a grep that found nothing are all ordinary answers a step reads. Everything the
# run needs before this point - its arguments, its work directory and its stack - is still under
# set -e, and await ends the run itself when a stack does not come up.
set +e

case "$CHECK_ID" in
k10) check_k10 ;;
k10b) check_k10b ;;
k10c) check_k10c ;;
k10d) check_k10d ;;
k10e) check_k10e ;;
k10f) check_k10f ;;
esac

report
