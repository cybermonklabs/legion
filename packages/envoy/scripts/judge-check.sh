#!/usr/bin/env bash
# packages/envoy/scripts/judge-check.sh
#
# Runs one before_merge check of the model judge's verification document
# (dispatch://INFRA-1353/artifact/verification-md) end to end and says whether it passed, so
# that the implementer and the tester run the same command and read the same evidence. It is
# scripts/airlock-check.sh for the judge's own seven checks - k10, k10b, k10c, k10d, k10e,
# k10f and k10g - and copies that script's shape: the same stack, the same helpers, the same
# result block.
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
# Four things this stack measured shape how the checks below are driven, and each is said
# again in the result block of the check it touches:
#
#   - The broker reads BROKER_AIRLOCK_RULES_FILE once, at boot (cmd/cipherd/main.go: "The file is
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
#   - A refusal the broker makes in its own code, with no setting that turns it off, can be
#     controlled for only by a broker built from a source tree with that code taken out. k10g's
#     admin refusal is one: it lives in the judge's sixth check and in the guard
#     currentPolicyAdmits makes for the judge, and BROKER_JUDGE_TEST_SKIP_CHECKS reaches
#     neither honestly - it would leave the transaction guard standing, so the request would be
#     put to the model and the approval then refused, which leaves it pending and proves
#     nothing, exactly as k10d's control note says of the lifetime comparison. So k10g copies
#     this module's source to its own work directory, takes the refusal out of all three places,
#     builds a broker from it and runs the same request at that broker, as k10b's control is a
#     scratch copy of one package with one import added.
#
# Usage: packages/envoy/scripts/judge-check.sh <id> [--keep-stack]
#        <id> is k10, k10b, k10c, k10d, k10e, k10f or k10g. --keep-stack leaves the stack and
#        the work directory up for a person to look at a failure with. --help prints this
#        usage. A usage error exits 2.
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

  <id>           k10, k10b, k10c, k10d, k10e, k10f or k10g
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
k10 | k10b | k10c | k10d | k10e | k10f | k10g) ;;
*)
	echo "judge-check: $CHECK_ID is no check of this document; it holds k10, k10b, k10c, k10d, k10e, k10f and k10g" >&2
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
#
# EXTRA_BROKER_BIN is the broker binary it runs, and the stack's own when the caller sets none.
# k10g sets it: its control is a broker built from a source tree with the admin refusal taken
# out, which no setting could stand in for, and everything else about that process - the
# database, the rules file, the fake store, the identities - has to stay the stack's.
start_extra_broker() { # <name> <VAR=value>...
	local name="$1"
	shift
	local log="$WORK_DIR/broker-$name.log"
	: >"$log"
	# Three ports the system picks, so this one never collides with the broker it was copied from.
	broker_env BROKER_LISTEN_ADDR="127.0.0.1:0" BROKER_AIRLOCK_LISTEN_ADDR="127.0.0.1:0" BROKER_METRICS_LISTEN_ADDR="127.0.0.1:0" "$@"
	"${BROKER_ENV[@]}" "${EXTRA_BROKER_BIN:-$BROKER_BIN}" >"$log" 2>&1 &
	local pid=$!
	EXTRA_BROKER_PIDS+=("$pid")
	await "the $name broker" 120 "$log" '"message":"airlock proxy listening"' "$pid"
	EXTRA_API_ADDR="$(logged_addr 'broker listening' "$log")"
	EXTRA_PROXY_ADDR="$(logged_addr 'airlock proxy listening' "$log")"
}

# logged_addr answers the addr field of the first line of a broker's log whose message is the one
# named, or nothing. The broker logs one JSON object per line, and a line still being written
# parses as nothing, so half an address is never read as the whole one.
logged_addr() { # <message> <log>
	jq -Rnr --arg message "$1" '[inputs | fromjson? | select(.message == $message) | .addr][0] // empty' "$2" || true
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
# store waits on. Each call is signed by this stack's session, one proof a broker (a proof is
# single-use), so each broker answers once its own reread is done; a call with no proof is
# answered at once, before the store is read, and the next check would race it. A broker that is
# down is passed over quietly (curl's 000): a check that stopped one on purpose (k10) still rereads
# on the one that is up, and curl's own line would land in the middle of a result block. Any other
# answer but 200, a signing failure, or no broker answering 200 at all is a failed check (fail),
# and it returns 1 so the step stops rather than read a broker that did not reread.
reread_all() { # <name>
	local addr proof code reread=0
	for addr in "${BROKER_API_ADDRS[@]}"; do
		if ! proof="$(agent-secrets sign --method POST --url "$AGENT_SECRETS_URL/v1/secrets/$1/reread")"; then
			fail "the stack's session could not sign the reread of $1"
			return 1
		fi
		code="$(curl -sS -o /dev/null -w '%{http_code}' -X POST -H "Proof: $proof" "http://$addr/v1/secrets/$1/reread" 2>/dev/null)"
		case "$code" in
		200) reread=$((reread + 1)) ;;
		000) ;;
		*)
			fail "the broker at $addr answered the reread of $1 $code, not 200"
			return 1
			;;
		esac
	done
	if [ "$reread" = 0 ]; then
		fail "no broker answered the reread of $1"
		return 1
	fi
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

# run_reason is the reason a request for a judge key signs here: the work, which the stack's
# guidance asks to be one of its own check runs, naming the open issue the run is for. No reason
# has to cite anything for the judge to decide it; the issue it names is data the judge shows the
# model.
run_reason() {
	printf 'the stack check run k10 reaches s.airlock.test for %s' "$AIRLOCK_DISPATCH_OPEN_ISSUE"
}

# deciding answers "true" when the create answer in ASK_JSON says the model judge holds the
# request, and "false" when it does not.
deciding() { printf '%s' "$ASK_JSON" | jq -r '.judge_deciding // false' 2>/dev/null; }

# page_url is the approve_url the create answer in ASK_JSON carries, "" for none.
page_url() { printf '%s' "$ASK_JSON" | jq -r '.approve_url // ""' 2>/dev/null; }

# status_page is the approve_url GET /v1/requests/{id} answers for a request now, "" for none.
status_page() { agent-secrets status "$1" --json 2>/dev/null | jq -r '.approve_url // ""'; }

# model_saw reports whether the model stand's last recorded request carried the text.
model_saw() { records model | jq -r --arg text "$1" 'if (last // {} | .body // "" | contains($text)) then "yes" else "no" end'; }

# await_listed polls GET /v1/pending for a record once a second until it is listed or the bound
# passes, and answers the seconds it took in AWAIT_SECONDS and "yes" or "no" in AWAIT_LISTED.
await_listed() { # <record id> <seconds>
	local start now
	start="$(date +%s)"
	while :; do
		AWAIT_LISTED="$(listed "$1")"
		now="$(date +%s)"
		AWAIT_SECONDS=$((now - start))
		if [ "$AWAIT_LISTED" = yes ] || [ "$AWAIT_SECONDS" -ge "$2" ]; then return 0; fi
		sleep 1
	done
}

# await_granted polls a request's state as a person would, and answers the seconds it took. It
# stops at the first state that is not pending, so a request the judge refused is read at once.
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
request_detail() { agent-secrets status "$1" --json 2>/dev/null | jq -r '.detail // ""'; }

# listed answers "yes" when GET /v1/pending — the broker's page and every Dispatch Inbox, which
# relays it — lists a record for the stack's approver, and "no" when it does not. A record the
# judge holds (judge_until ahead, a request for judge keys alone) is held off it.
listed() { # <record id>
	account_get "/v1/pending?approver=$AIRLOCK_APPROVER" |
		jq -r --arg r "$1" 'if ([.pending[]? | select(.record_id == $r)] | length) > 0 then "yes" else "no" end'
}

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
	CALL_OUT="$(printf 'Authorization: Bearer %s' "$2" | curl -sS -o "$WORK_DIR/call.out" -w 'http_code=%{http_code}' --noproxy '' \
		--cacert "$AIRLOCK_CA_CERT_FILE" --proxy "$(proxy_for "$1" "$PROXY_ADDR")" \
		-H @- "$AIRLOCK_S_URL/ok" 2>&1 | tr '\n' ' ')"
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
# order with the one the run ended at named (the first that failed for good, else the first that
# failed, as the judge's decidingFailure reads them), the model, whether it was asked, the
# verdict and the reason. not_pending is the run's own last gate rather than one of the checks:
# the request's state, read again immediately before the model call, showed it no longer
# pending, so the run asked nothing (judge's Runner.stillPending).
judged_summary() { # <request id>
	local detail
	detail="$(judged_detail "$1")"
	if [ -z "$detail" ]; then
		echo "no request.judged row"
		return
	fi
	printf '%s' "$detail" | jq -r '
		"checks=" + ([.checks[] | .check + "=" + (.passed|tostring)] | join(",")) +
		" failed=" + (([.checks[] | select((.passed | not) and (.final // false)) | .check] + [.checks[] | select(.passed | not) | .check] | first) // "none") +
		" detail=" + (([.checks[] | select((.passed | not) and (.final // false)) | .detail] + [.checks[] | select(.passed | not) | .detail] | first) // "") +
		" model=" + (.model // "") + " asked=" + ((.asked // false)|tostring) +
		" verdict=" + (.verdict // "") + " reason=" + (.reason // "") +
		(if has("not_pending") then " not_pending=" + .not_pending else "" end) +
		(if has("applied") then " applied=" + (.applied|tostring) else "" end) +
		(if has("not_applied") then " not_applied=" + .not_applied else "" end) +
		(if has("error") then " error=" + .error else "" end) +
		(if has("raw_answer") then " raw_answer=" + .raw_answer else "" end)'
}

# judged_failed_check is the name of the check a judged row's run ended at: the first it recorded
# as failed for good, else the first it recorded as failed, and "none" for a row whose checks all
# passed.
judged_failed_check() { # <request id>
	local detail
	detail="$(judged_detail "$1")"
	[ -n "$detail" ] || { echo "no-row"; return; }
	printf '%s' "$detail" | jq -r '([.checks[] | select((.passed | not) and (.final // false)) | .check] + [.checks[] | select(.passed | not) | .check] | first) // "none"'
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
# cannot pin (the header comment). The token reaches curl on standard input, in no command line.
account_get() { # <path>
	printf 'Authorization: Bearer %s' "$(cat "$AGENT_SECRETS_ACCOUNT_TOKEN_FILE")" | curl -sS -H @- "$AGENT_SECRETS_URL$1"
}

account_post() { # <path> <body>
	printf 'Authorization: Bearer %s' "$(cat "$AGENT_SECRETS_ACCOUNT_TOKEN_FILE")" |
		curl -sS -o "$WORK_DIR/account.out" -w '%{http_code}' -X POST \
			-H @- -H 'Content-Type: application/json' --data "$2" "$AGENT_SECRETS_URL$1"
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

# page_of reads a record's approval page as the person SIGNED_IN_COOKIE signs in, and answers in
# PAGE_FORM whether it offers a decision form for that record ("yes" or "no"), in PAGE_NOTE whether
# it says the model judge decides it, and in PAGE_TOKEN_FIELDS the page's hidden token fields as a
# urlencoded string, for a POST that reuses them.
page_of() { # <record id>
	local body="$WORK_DIR/page-$1.html"
	curl -sS -o "$body" -H "Cookie: $SIGNED_IN_COOKIE" "$AGENT_SECRETS_URL/approve/$1"
	PAGE_FORM=no
	if grep -q "name=\"record\" value=\"$1\"" "$body"; then PAGE_FORM=yes; fi
	PAGE_NOTE=no
	if grep -q 'The model judge is deciding this request, so nobody approves or denies it here' "$body"; then PAGE_NOTE=yes; fi
	PAGE_TOKEN_FIELDS="$(grep -o 'name="\(printed\|token\|expires\)" value="[^"]*"' "$body" |
		sed 's/name="\([a-z]*\)" value="\([^"]*\)"/\1=\2/' | paste -sd '&' -)"
}

# post_decision POSTs one approve decision of a record from the broker's own origin, with the token
# fields of a page the person was shown, and answers the HTTP status.
post_decision() { # <record id> <token fields>
	curl -sS -o "$WORK_DIR/decide.out" -w '%{http_code}' -X POST -H "Cookie: $SIGNED_IN_COOKIE" \
		-H "Origin: $AGENT_SECRETS_URL" -H 'Content-Type: application/x-www-form-urlencoded' \
		--data "$2&record=$1&decision=approve" "$AGENT_SECRETS_URL/approve"
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

# airlock_refusal_holds reports whether any broker of this stack wrote a refusal record of the
# airlock's proxy port (`broker: answer refused` with caller_class airlock) whose reason field
# holds the text, and leaves the last such record in BROKER_LOG_LINE. A line still being written
# parses as nothing and is left out.
airlock_refusal_holds() { # <text>
	BROKER_LOG_LINE="$(jq -Rc --arg text "$1" 'fromjson? | select(.message == "broker: answer refused" and .caller_class == "airlock" and ((.reason // "") | contains($text)))' "${BROKER_LOGS[@]}" 2>/dev/null | tail -n 1 | tr -d '\n' || true)"
	[ -n "$BROKER_LOG_LINE" ]
}

###############################################################################################
# k10
###############################################################################################

# k10_case drives one request and reads it back: what its create answer said, whether it was
# listed when written, its state, row and listing once its run had its budget, and what the
# stand-in model was asked. It cancels whatever is still pending so the next case's names are
# free. The caller reads K10 for the answers.
declare -A K10=()

k10_case() { # <label> <ttl> <reason> <name>...
	local label="$1" ttl="$2" reason="$3"
	shift 3
	local mark
	mark="$(records_count model)"
	ask "$ttl" "$reason" "$@"
	K10[${label}_exit]="$ASK_STATUS"
	K10[${label}_err]="$ASK_ERR"
	K10[${label}_state0]="$ASK_STATE"
	K10[${label}_lifetime]="$ASK_LIFETIME"
	K10[${label}_url]="$(page_url)"
	K10[${label}_deciding]="$(deciding)"
	if [ -n "$ASK_ID" ]; then
		K10[${label}_listed0]="$(listed "$ASK_RECORD")"
		# One run has 35 seconds of budget and this stack answers in milliseconds, so the row
		# is there in a second or two; 15 is the bound the wait is given. A request no judge
		# holds has no run to wait for.
		if [ "$(deciding)" = true ]; then await_judged "$ASK_ID" 15; fi
		K10[${label}_state]="$(request_state "$ASK_ID")"
		K10[${label}_rows]="$(judged_rows "$ASK_ID")"
		K10[${label}_failed]="$(judged_failed_check "$ASK_ID")"
		K10[${label}_summary]="$(judged_summary "$ASK_ID")"
		K10[${label}_listed]="$(listed "$ASK_RECORD")"
		K10[${label}_detail]="$(request_detail "$ASK_ID")"
		K10[${label}_page]="$(status_page "$ASK_ID")"
		K10[${label}_decider]="$(request_decider "$ASK_ID")"
	else
		K10[${label}_state]="(no request)"
		K10[${label}_rows]=0
		K10[${label}_failed]="(no request)"
		K10[${label}_summary]="the broker refused the request: ${ASK_ERR}"
		K10[${label}_listed0]="no"
		K10[${label}_listed]="no"
		K10[${label}_detail]=""
		K10[${label}_page]=""
		K10[${label}_decider]=""
	fi
	K10[${label}_asked]="$(($(records_count model) - mark))"
	K10[${label}_saw_guidance]="$(model_saw "$AIRLOCK_JUDGE_GUIDANCE")"
	local grant
	grant="$(request_grant "${ASK_ID:-none}")"
	if [ -n "$grant" ]; then agent-secrets revoke "$grant" >/dev/null 2>&1 || true; fi
	cancel_pending
}

# k10_final is the final refusal's sentence as the judge writes it: the reason, then that the
# request will not be granted, then whom to tell.
k10_final() { # <detail>
	contains "the model judge refused this request: " "$1" && contains ". This request will not be granted. Tell whoever gave you this work." "$1" &&
		! contains "again" "$1" && ! contains "retry" "$1" && ! contains "broker page" "$1"
}

check_k10() {
	expect "a request for a judge key (a rules entry with a judge, no admin tag) is held for the judge: its create answer says judge_deciding and carries no approve_url, it is listed for nobody, and with the stand-in answering grant it is granted by judge:<model id>, for the key's judge seconds when it named no lifetime"
	expect "the stand-in model is shown the key's guidance"
	expect "the stand-in answering deny ends the request denied with the final words, exit 77, and nothing that says to try again; a name withheld from the session is refused the same way without the model being asked"
	expect "a model answer that is not JSON, a model that never answers, a broker with no judge, and a broker stopped during the model call each give the request to its person once: an ordinary card with its approve_url, on the approver's list"
	expect "while the judge holds the request, its page offers no decision, a POST naming it decides nothing while the same POST decides a card, and the decision route answers 403 JUDGE_DECIDES while it names a card's page"
	expect "a lifetime past the key's judge seconds is refused when the request is written, and the model is not asked"
	expect "a request for two judge keys is decided by the judge, which is shown both keys' guidance; a judge key beside a key a person decides, an administrator's key with a judge entry, and a key with no entry are each the person's card from the start, and the model is not asked"
	expect "a request for a judge key beside AIRLOCK_DEMO_TOKEN, an agent-tier key the session gets at once, is held for the judge and granted by judge:<model id>, the model shown the judge key alone, and a release of that grant hands over both keys"
	expect "each judge decision has exactly one request.judged row, carrying the five checks in order, the model, the verdict and the reason"
	expect "the owner's list shows the judged grant, the owner ends it, and the next call gets 407"
	expect "the negative control, with the checks before the model turned off (dev-airlock.sh --judge-skip-checks), asks the model about the withheld request the normal stack refuses without asking"

	start_stack
	stack_facts
	local mark

	# --- A judge key's request with the stand answering grant, naming no lifetime. ---
	model_answers '{"verdict":"grant","reason":"the reason names a check run, as the guidance asks"}'
	k10_case grant "" "$(run_reason)" "$AIRLOCK_JUDGED_SECRET"
	observe "a request for $AIRLOCK_JUDGED_SECRET naming no lifetime: create answer judge_deciding '${K10[grant_deciding]}', approve_url '${K10[grant_url]}', lifetime ${K10[grant_lifetime]}s; listed '${K10[grant_listed0]}' when written; ${K10[grant_state]}, decided by '${K10[grant_decider]}'; the stand asked ${K10[grant_asked]} time(s) and shown the guidance: ${K10[grant_saw_guidance]} — ${K10[grant_summary]}"
	assert "$([ "${K10[grant_deciding]}" = true ] && [ -z "${K10[grant_url]}" ] && [ "${K10[grant_listed0]}" = no ] && [ "${K10[grant_state]}" = granted ] &&
		[ "${K10[grant_decider]}" = "$JUDGE_ACTOR" ] && [ "${K10[grant_lifetime]}" = "$AIRLOCK_JUDGE_SECONDS" ] && echo 0 || echo 1)" \
		"a judge key's request is held for the judge — judge_deciding, no approve_url, listed for nobody — and granted by judge:<model id> for the key's judge seconds when it named no lifetime" \
		"judge_deciding '${K10[grant_deciding]}', approve_url '${K10[grant_url]}', listed '${K10[grant_listed0]}', state '${K10[grant_state]}', decided by '${K10[grant_decider]}', lifetime ${K10[grant_lifetime]}s against ${AIRLOCK_JUDGE_SECONDS}s"
	assert "$([ "${K10[grant_saw_guidance]}" = yes ] && echo 0 || echo 1)" \
		"the stand-in model is shown the key's guidance" \
		"the stand's last call carried '$AIRLOCK_JUDGE_GUIDANCE': ${K10[grant_saw_guidance]}"
	local grant_checks grant_model grant_verdict grant_reason grant_request
	grant_request="$(psql_stack -c "select id from requests where record_id is not null order by created_at desc limit 1")"
	grant_checks="$(printf '%s' "$(judged_detail "$grant_request")" | jq -r '[.checks[].check] | join(",")')"
	grant_model="$(judged_field "$grant_request" .model)"
	grant_verdict="$(judged_field "$grant_request" .verdict)"
	grant_reason="$(judged_field "$grant_request" .reason)"

	# --- The stand answering deny: the final refusal. ---
	model_answers '{"verdict":"deny","reason":"the reason names no check run of the stack"}'
	k10_case deny 10m "$(run_reason)" "$AIRLOCK_JUDGED_SECRET"
	agent-secrets "$AIRLOCK_JUDGED_SECRET" --ttl 10m --reason "$(run_reason)" --wait 10s -- sh -c 'echo the-child-ran' >"$WORK_DIR/deny.out" 2>"$WORK_DIR/deny.err"
	local deny_exec_exit=$?
	local deny_exec_text
	deny_exec_text="$(cat "$WORK_DIR/deny.out" "$WORK_DIR/deny.err" | tr '\n' ' ')"
	cancel_pending
	observe "the stand answering deny: ${K10[deny_state]}, listed '${K10[deny_listed0]}'/'${K10[deny_listed]}', ${K10[deny_rows]} row(s), the detail its agent reads: '${K10[deny_detail]}'; the exec form exited $deny_exec_exit with '$deny_exec_text'"
	assert "$([ "${K10[deny_state]}" = denied ] && [ "${K10[deny_rows]}" = 1 ] && [ "${K10[deny_listed0]}" = no ] && [ "${K10[deny_listed]}" = no ] &&
		k10_final "${K10[deny_detail]}" && contains "the reason names no check run" "${K10[deny_detail]}" &&
		[ "$deny_exec_exit" = 77 ] && ! contains "the-child-ran" "$deny_exec_text" && ! contains "again" "$deny_exec_text" && echo 0 || echo 1)" \
		"the stand answering deny ends the request denied with the final words, exit 77, and nothing that says to try again" \
		"state '${K10[deny_state]}', detail '${K10[deny_detail]}', exec exit $deny_exec_exit"

	# --- A name withheld from the session: refused by the judge's check, no model asked. ---
	model_answers '{"verdict":"grant","reason":"a check would have stopped this"}'
	psql_stack -c "insert into withheld_secrets (enrollment_id, name) values ('$AIRLOCK_ENROLLMENT', '$AIRLOCK_JUDGED_SECRET')" >/dev/null
	k10_case withheld 10m "$(run_reason)" "$AIRLOCK_JUDGED_SECRET"
	psql_stack -c "delete from withheld_secrets where enrollment_id='$AIRLOCK_ENROLLMENT'" >/dev/null
	observe "a name withheld from the session: ${K10[withheld_state]}, stand asked ${K10[withheld_asked]} time(s), first failed check '${K10[withheld_failed]}', detail '${K10[withheld_detail]}'"
	assert "$([ "${K10[withheld_state]}" = denied ] && [ "${K10[withheld_asked]}" = 0 ] && [ "${K10[withheld_failed]}" = withheld ] && k10_final "${K10[withheld_detail]}" && echo 0 || echo 1)" \
		"a name withheld from the session is refused for good by the judge's withheld check, without the model being asked" \
		"state '${K10[withheld_state]}', asked ${K10[withheld_asked]}, failed '${K10[withheld_failed]}', detail '${K10[withheld_detail]}'"

	# --- An answer that is not JSON: no verdict, so the request goes to its person. The stand
	#     holds the answer back five seconds, so the read "when written" is made while the judge
	#     still holds the request, not after a reply that came back at once. ---
	model_answers '{"text":"I think this is probably fine, but I am not going to say so in JSON.","delay":"5s"}'
	ask 10m "$(run_reason)" "$AIRLOCK_JUDGED_SECRET"
	local text_id="$ASK_ID" text_record="$ASK_RECORD"
	K10[text_listed0]="$(listed "$text_record")"
	await_judged "$text_id" 15
	await_listed "$text_record" 10
	K10[text_listed]="$AWAIT_LISTED"
	K10[text_state]="$(request_state "$text_id")"
	K10[text_page]="$(status_page "$text_id")"
	K10[text_rows]="$(judged_rows "$text_id")"
	K10[text_error]="$(judged_field "$text_id" .error)"
	cancel_pending
	observe "an answer that is not JSON: listed '${K10[text_listed0]}' when written and '${K10[text_listed]}' after the run, ${K10[text_state]}, approve_url '${K10[text_page]}', ${K10[text_rows]} row(s), error '${K10[text_error]}'"

	# --- A model that never answers: while the judge holds it, nobody decides it; then the
	#     person does. ---
	model_answers '{"delay":"30s"}'
	ask 10m "$(run_reason)" "$AIRLOCK_JUDGED_SECRET"
	local held_id="$ASK_ID" held_record="$ASK_RECORD"
	K10[delay_listed0]="$(listed "$held_record")"
	sleep 5
	K10[delay_listed_mid]="$(listed "$held_record")"
	sign_in "$AIRLOCK_APPROVER" page || true
	page_of "$held_record"
	K10[page_form]="$PAGE_FORM"
	K10[page_note]="$PAGE_NOTE"
	# A card the approver does decide, beside it: a key with no judge entry.
	ask 10m "$(run_reason)" "$AIRLOCK_UNJUDGED_SECRET"
	local card_id="$ASK_ID" card_record="$ASK_RECORD"
	page_of "$card_record"
	K10[card_form]="$PAGE_FORM"
	K10[post_held]="$(post_decision "$held_record" "$PAGE_TOKEN_FIELDS")"
	K10[post_held_state]="$(psql_stack -c "select state from requests where id='$held_id'")"
	K10[post_held_events]="$(psql_stack -c "select count(*) from credential_request_events where record_id='$held_record' and event in ('approved','denied')")"
	local approve_body
	approve_body="$(jq -nc --arg a "$AIRLOCK_APPROVER" '{approver: $a}')"
	K10[api_held_status]="$(account_post "/v1/credential-requests/$held_record/approve" "$approve_body")"
	K10[api_held_code]="$(jq -r '.code // ""' "$WORK_DIR/account.out" 2>/dev/null)"
	K10[api_held_page]="$(jq -r '.approve_url // ""' "$WORK_DIR/account.out" 2>/dev/null)"
	K10[api_card_status]="$(account_post "/v1/credential-requests/$card_record/approve" "$approve_body")"
	K10[api_card_code]="$(jq -r '.code // ""' "$WORK_DIR/account.out" 2>/dev/null)"
	K10[api_card_page]="$(jq -r '.approve_url // ""' "$WORK_DIR/account.out" 2>/dev/null)"
	K10[post_card]="$(post_decision "$card_record" "$PAGE_TOKEN_FIELDS")"
	K10[post_card_state]="$(psql_stack -c "select state from requests where id='$card_id'")"
	local card_grant
	card_grant="$(request_grant "$card_id")"
	if [ -n "$card_grant" ]; then agent-secrets revoke "$card_grant" >/dev/null 2>&1 || true; fi
	await_judged "$held_id" 35
	await_listed "$held_record" 10
	K10[delay_listed]="$AWAIT_LISTED"
	K10[delay_state]="$(request_state "$held_id")"
	K10[delay_page]="$(status_page "$held_id")"
	K10[delay_rows]="$(judged_rows "$held_id")"
	cancel_pending
	observe "a model that never answers: listed '${K10[delay_listed0]}' when written, '${K10[delay_listed_mid]}' five seconds into the call, '${K10[delay_listed]}' after the run; ${K10[delay_state]}, approve_url '${K10[delay_page]}', ${K10[delay_rows]} row(s)"
	observe "while the judge held it, signed in as $AIRLOCK_APPROVER: its page offered a form '${K10[page_form]}' and the judge note '${K10[page_note]}'; a POST naming it with a card's token answered ${K10[post_held]} and left it '${K10[post_held_state]}' with ${K10[post_held_events]} event(s); the card's own page offered a form '${K10[card_form]}' and the same POST answered ${K10[post_card]}, leaving the card '${K10[post_card_state]}'; the decision route answered ${K10[api_held_status]} ${K10[api_held_code]} (approve_url '${K10[api_held_page]}') for it and ${K10[api_card_status]} ${K10[api_card_code]} (approve_url '${K10[api_card_page]}') for the card"
	assert "$([ "${K10[page_form]}" = no ] && [ "${K10[page_note]}" = yes ] && [ "${K10[post_held_state]}" = pending ] && [ "${K10[post_held_events]}" = 0 ] &&
		[ "${K10[card_form]}" = yes ] && [ "${K10[post_card_state]}" = granted ] &&
		[ "${K10[api_held_status]}" = 403 ] && [ "${K10[api_held_code]}" = JUDGE_DECIDES ] && [ -z "${K10[api_held_page]}" ] &&
		[ "${K10[api_card_status]}" = 403 ] && [ "${K10[api_card_code]}" = DECIDE_ON_BROKER_PAGE ] && [ -n "${K10[api_card_page]}" ] && echo 0 || echo 1)" \
		"while the judge holds a request its page offers no decision, a POST naming it decides nothing while the same POST decides a card, and the decision route answers 403 JUDGE_DECIDES while it names a card's page" \
		"page form '${K10[page_form]}', note '${K10[page_note]}', POST left it '${K10[post_held_state]}'; card form '${K10[card_form]}', card '${K10[post_card_state]}'; route ${K10[api_held_code]} / ${K10[api_card_code]}"

	# --- A broker stopped during the model call, and a broker with no judge in its place. ---
	front_backends "${BROKER_API_ADDRS[1]}" "${BROKER_PROXY_ADDRS[1]}"
	model_answers '{"delay":"30s"}'
	ask 10m "$(run_reason)" "$AIRLOCK_JUDGED_SECRET"
	local lost_id="$ASK_ID" lost_record="$ASK_RECORD"
	K10[lost_listed0]="$(listed "$lost_record")"
	sleep 2
	kill "${BROKER_PIDS[1]}" 2>/dev/null || true
	start_extra_broker restarted BROKER_JUDGE_MODEL_URL= BROKER_JUDGE_MODEL= BROKER_JUDGE_MODEL_AUDIENCE= \
		BROKER_JUDGE_DISPATCH_URL= BROKER_JUDGE_DISPATCH_TOKEN_FILE= BROKER_JUDGE_MODEL_PROTOCOL=
	front_backends "$EXTRA_API_ADDR" "$EXTRA_PROXY_ADDR"
	K10[lost_listed_restart]="$(listed "$lost_record")"
	# The hold is two minutes from the write; the list reads it at once when it ends. 150 is
	# the bound the wait is given.
	await_listed "$lost_record" 150
	K10[lost_listed]="$AWAIT_LISTED"
	K10[lost_after]="$(psql_stack -c "select round(extract(epoch from (now() - created_at)))::int from requests where id='$lost_id'")"
	K10[lost_hold]="$(psql_stack -c "select round(extract(epoch from (judge_until - created_at)))::int from requests where id='$lost_id'")"
	K10[lost_state]="$(request_state "$lost_id")"
	K10[lost_page]="$(status_page "$lost_id")"
	K10[lost_rows]="$(judged_rows "$lost_id")"
	K10[lost_grants]="$(psql_stack -c "select count(*) from grants where request_id='$lost_id'")"
	cancel_pending
	# The same broker, which runs no judge, asked for the judge key again: the person's card.
	model_answers '{"verdict":"grant","reason":"the restarted broker has no judge to ask this"}'
	k10_case nojudge 10m "$(run_reason)" "$AIRLOCK_JUDGED_SECRET"
	front_backends "${BROKER_API_ADDRS[0]}" "${BROKER_PROXY_ADDRS[0]}"
	kill_extra_brokers
	EXTRA_BROKER_PIDS=()
	observe "a broker stopped two seconds into the model call and a broker with no judge in its place: listed '${K10[lost_listed0]}' when written, '${K10[lost_listed_restart]}' on the restarted broker, '${K10[lost_listed]}' at ${K10[lost_after]}s (its hold ${K10[lost_hold]}s); ${K10[lost_state]}, approve_url '${K10[lost_page]}', ${K10[lost_rows]} row(s), ${K10[lost_grants]} grant(s)"
	observe "a request for $AIRLOCK_JUDGED_SECRET on the broker with no judge: judge_deciding '${K10[nojudge_deciding]}', approve_url '${K10[nojudge_url]}', listed '${K10[nojudge_listed0]}', ${K10[nojudge_state]}, stand asked ${K10[nojudge_asked]} time(s)"
	assert "$([ "${K10[text_listed0]}" = no ] && [ "${K10[text_listed]}" = yes ] && [ "${K10[text_state]}" = pending ] && [ -n "${K10[text_page]}" ] && [ "${K10[text_rows]}" = 1 ] &&
		[ "${K10[delay_listed0]}" = no ] && [ "${K10[delay_listed_mid]}" = no ] && [ "${K10[delay_listed]}" = yes ] && [ "${K10[delay_state]}" = pending ] && [ -n "${K10[delay_page]}" ] &&
		[ "${K10[lost_listed0]}" = no ] && [ "${K10[lost_listed_restart]}" = no ] && [ "${K10[lost_listed]}" = yes ] && [ "${K10[lost_state]}" = pending ] &&
		[ "${K10[lost_rows]}" = 0 ] && [ "${K10[lost_grants]}" = 0 ] && [ "${K10[lost_after]}" -le 140 ] &&
		[ "${K10[nojudge_deciding]}" = false ] && [ -n "${K10[nojudge_url]}" ] && [ "${K10[nojudge_listed0]}" = yes ] && [ "${K10[nojudge_asked]}" = 0 ] && echo 0 || echo 1)" \
		"an answer that is not JSON, a model that never answers, a broker stopped mid-call and a broker with no judge each give the request to its person once, as an ordinary card with its approve_url on the approver's list" \
		"text: ${K10[text_listed0]}/${K10[text_listed]} '${K10[text_state]}'; never answers: ${K10[delay_listed0]}/${K10[delay_listed_mid]}/${K10[delay_listed]} '${K10[delay_state]}'; stopped: ${K10[lost_listed0]}/${K10[lost_listed_restart]}/${K10[lost_listed]} at ${K10[lost_after]}s; no judge: deciding '${K10[nojudge_deciding]}', listed '${K10[nojudge_listed0]}'"

	# --- A lifetime past the key's judge seconds: refused when written. ---
	k10_case long 30m "$(run_reason)" "$AIRLOCK_JUDGED_SECRET"
	observe "--ttl 30m of a key whose judge grants ${AIRLOCK_JUDGE_SECONDS}s: exit ${K10[long_exit]}, '${K10[long_err]}', request '${K10[long_state]}', stand asked ${K10[long_asked]} time(s)"
	assert "$([ "${K10[long_exit]}" != 0 ] && [ "${K10[long_state]}" = "(no request)" ] && contains "--ttl 15m" "${K10[long_err]}" && [ "${K10[long_asked]}" = 0 ] && echo 0 || echo 1)" \
		"a lifetime past the key's judge seconds is refused when the request is written, naming the --ttl that would do, and the model is not asked" \
		"exit ${K10[long_exit]}, '${K10[long_err]}', state '${K10[long_state]}', asked ${K10[long_asked]}"

	# --- Several keys, and keys a person decides. ---
	model_answers '{"verdict":"grant","reason":"both keys meet their guidance"}'
	k10_case two_judged 10m "$(run_reason)" "$AIRLOCK_JUDGED_SECRET" "$AIRLOCK_HOSTLESS_SECRET"
	local two_saw_hostless
	two_saw_hostless="$(records model | jq -r --arg a "$AIRLOCK_JUDGE_GUIDANCE" --arg b "$AIRLOCK_HOSTLESS_GUIDANCE" 'if (last // {} | .body // "" | (contains($a) and contains($b))) then "yes" else "no" end')"
	# A judge key beside a key the session gets at once: the judge's, decided on the judge key
	# alone, and its grant a grant of both. k10_case ends the grant it made, so the exec form
	# below asks again, waits for the judge's grant, and runs only once the release — whose
	# chain check holds the judge's approval to the rules again — has handed over both keys.
	k10_case with_agent 10m "$(run_reason)" "$AIRLOCK_JUDGED_SECRET" AIRLOCK_DEMO_TOKEN
	local agent_shown agent_exit agent_out agent_grant
	agent_shown="$(records model | jq -r 'if (last // {} | .body // "" | contains("AIRLOCK_DEMO_TOKEN")) then "yes" else "no" end')"
	agent-secrets "$AIRLOCK_JUDGED_SECRET" AIRLOCK_DEMO_TOKEN --ttl 10m --reason "$(run_reason)" --wait 30s -- sh -c 'echo the-release-ran' >"$WORK_DIR/with-agent.out" 2>"$WORK_DIR/with-agent.err"
	agent_exit=$?
	agent_out="$(cat "$WORK_DIR/with-agent.out" "$WORK_DIR/with-agent.err" | tr '\n' ' ')"
	agent_grant="$(psql_stack -c "select id from grants where enrollment_id='$AIRLOCK_ENROLLMENT' and revoked_at is null order by created_at desc limit 1")"
	if [ -n "$agent_grant" ]; then agent-secrets revoke "$agent_grant" >/dev/null 2>&1 || true; fi
	cancel_pending
	k10_case mixed 10m "$(run_reason)" "$AIRLOCK_JUDGED_SECRET" "$AIRLOCK_UNJUDGED_SECRET"
	k10_case admin 10m "$(run_reason)" "$AIRLOCK_ADMIN_SECRET"
	k10_case unjudged 10m "$(run_reason)" "$AIRLOCK_UNJUDGED_SECRET"
	observe "two judge keys ($AIRLOCK_JUDGED_SECRET, $AIRLOCK_HOSTLESS_SECRET, whose entry binds no host): judge_deciding '${K10[two_judged_deciding]}', ${K10[two_judged_state]}, decided by '${K10[two_judged_decider]}', stand asked ${K10[two_judged_asked]} time(s), shown both guidances: $two_saw_hostless"
	observe "a judge key beside a key with no entry: judge_deciding '${K10[mixed_deciding]}', approve_url '${K10[mixed_url]}', listed '${K10[mixed_listed0]}', ${K10[mixed_state]}, asked ${K10[mixed_asked]}"
	observe "an administrator's key with a judge entry: judge_deciding '${K10[admin_deciding]}', approve_url '${K10[admin_url]}', listed '${K10[admin_listed0]}', ${K10[admin_state]}, asked ${K10[admin_asked]}"
	observe "a key with no judge entry: judge_deciding '${K10[unjudged_deciding]}', approve_url '${K10[unjudged_url]}', listed '${K10[unjudged_listed0]}', ${K10[unjudged_state]}, asked ${K10[unjudged_asked]}"
	local person_cards=0 case_label
	for case_label in mixed admin unjudged; do
		[ "${K10[${case_label}_deciding]}" = false ] && [ -n "${K10[${case_label}_url]}" ] && [ "${K10[${case_label}_listed0]}" = yes ] &&
			[ "${K10[${case_label}_state]}" = pending ] && [ "${K10[${case_label}_asked]}" = 0 ] || person_cards=1
	done
	assert "$([ "${K10[two_judged_deciding]}" = true ] && [ "${K10[two_judged_state]}" = granted ] && [ "${K10[two_judged_decider]}" = "$JUDGE_ACTOR" ] && [ "$two_saw_hostless" = yes ] &&
		[ "$person_cards" = 0 ] && echo 0 || echo 1)" \
		"a request for two judge keys is decided by the judge, which is shown both keys' guidance; a judge key beside a key a person decides, an administrator's key with a judge entry, and a key with no entry are each the person's card from the start, and the model is not asked" \
		"two judge keys '${K10[two_judged_state]}' by '${K10[two_judged_decider]}', both guidances $two_saw_hostless; mixed '${K10[mixed_state]}' listed ${K10[mixed_listed0]}; admin '${K10[admin_state]}' listed ${K10[admin_listed0]}; unjudged '${K10[unjudged_state]}' listed ${K10[unjudged_listed0]}"
	observe "a judge key beside AIRLOCK_DEMO_TOKEN, which the pod session gets at once: judge_deciding '${K10[with_agent_deciding]}', approve_url '${K10[with_agent_url]}', listed '${K10[with_agent_listed0]}', ${K10[with_agent_state]}, decided by '${K10[with_agent_decider]}', stand asked ${K10[with_agent_asked]} time(s), the agent-tier key in what the model was shown: $agent_shown; the release exited $agent_exit with '$agent_out' — ${K10[with_agent_summary]}"
	assert "$([ "${K10[with_agent_deciding]}" = true ] && [ -z "${K10[with_agent_url]}" ] && [ "${K10[with_agent_listed0]}" = no ] && [ "${K10[with_agent_state]}" = granted ] &&
		[ "${K10[with_agent_decider]}" = "$JUDGE_ACTOR" ] && [ "${K10[with_agent_asked]}" = 1 ] && [ "$agent_shown" = no ] &&
		[ "$agent_exit" = 0 ] && contains "the-release-ran" "$agent_out" && echo 0 || echo 1)" \
		"a request for a judge key beside an agent-tier key the session gets at once is held for the judge and granted by judge:<model id>, the model shown the judge key alone, and a release of that grant hands over both keys" \
		"judge_deciding '${K10[with_agent_deciding]}', listed '${K10[with_agent_listed0]}', '${K10[with_agent_state]}' by '${K10[with_agent_decider]}', asked ${K10[with_agent_asked]}, agent key shown: $agent_shown; the release exited $agent_exit"

	# --- One row per decision, carrying the five checks. ---
	local one_row=0
	for case_label in grant deny withheld two_judged with_agent; do
		[ "${K10[${case_label}_rows]}" = 1 ] || one_row=1
	done
	for case_label in mixed admin unjudged nojudge; do
		[ "${K10[${case_label}_rows]}" = 0 ] || one_row=1
	done
	observe "the grant's row: checks $grant_checks; model '$grant_model', verdict '$grant_verdict', reason '$grant_reason'"
	assert "$([ "$one_row" = 0 ] && [ "$grant_checks" = "rules-judge,lifetime,policy,admin,withheld" ] && [ "$grant_model" = "$AIRLOCK_JUDGE_MODEL" ] && [ "$grant_verdict" = grant ] && [ -n "$grant_reason" ] && echo 0 || echo 1)" \
		"each judge decision has exactly one request.judged row, carrying the five checks in order, the model, the verdict and the reason; a request a person decides has none" \
		"grant rows ${K10[grant_rows]}, deny ${K10[deny_rows]}, withheld ${K10[withheld_rows]}, two keys ${K10[two_judged_rows]}; person cards ${K10[mixed_rows]}/${K10[admin_rows]}/${K10[unjudged_rows]}/${K10[nojudge_rows]}; checks $grant_checks, verdict '$grant_verdict'"

	# --- The owner lists the grant, ends it, and the next call is refused. ---
	model_answers '{"verdict":"grant","reason":"the reason names a check run, as the guidance asks"}'
	ask 10m "$(run_reason)" "$AIRLOCK_JUDGED_SECRET"
	local owned_id="$ASK_ID"
	await_granted "$owned_id" 60
	local owned_grant
	owned_grant="$(request_grant "$owned_id")"
	release "$owned_grant" "$AIRLOCK_JUDGED_SECRET"
	local before_pw="$PROXY_PASSWORD" before_ph="$PLACEHOLDER"
	call_s "$before_pw" "$before_ph"
	local before_status="$CALL_STATUS"
	local owner_list listed_names revoke_status
	owner_list="$(account_get "/v1/grants?approver=$AIRLOCK_APPROVER")"
	listed_names="$(printf '%s' "$owner_list" | jq -r --arg g "$owned_grant" '[.grants[] | select(.grant_id == $g) | .names[]] | join(",")')"
	revoke_status="$(account_post "/v1/grants/$owned_grant/revoke-by-approver" "$(jq -nc --arg a "$AIRLOCK_APPROVER" '{approver: $a}')")"
	call_s "$before_pw" "$before_ph"
	local after_status="$CALL_STATUS"
	observe "the owner's list (GET /v1/grants?approver=$AIRLOCK_APPROVER) holds grant $owned_grant over [$listed_names]; the revoke answered $revoke_status; the call through the grant answered $before_status before it and $after_status after it"
	assert "$([ "$listed_names" = "$AIRLOCK_JUDGED_SECRET" ] && [ "$revoke_status" = 200 ] && [ "$before_status" = 200 ] && [ "$after_status" = 407 ] && echo 0 || echo 1)" \
		"the owner's list shows the judged grant, the owner ends it, and the next call gets 407" \
		"the list named [$listed_names], the revoke answered $revoke_status, and the call went $before_status then $after_status"

	stop_stack

	# --- The negative control: the checks before the model turned off. The withheld request
	#     the stack above refused without asking the model is asked about here. ---
	start_stack --judge-skip-checks
	stack_facts
	model_answers '{"verdict":"grant","reason":"asked about a request a check would have stopped"}'
	psql_stack -c "insert into withheld_secrets (enrollment_id, name) values ('$AIRLOCK_ENROLLMENT', '$AIRLOCK_JUDGED_SECRET')" >/dev/null
	k10_case control 10m "$(run_reason)" "$AIRLOCK_JUDGED_SECRET"
	control "with BROKER_JUDGE_TEST_SKIP_CHECKS=true (dev-airlock.sh --judge-skip-checks), the withheld request: the stand asked ${K10[control_asked]} time(s), ${K10[control_state]}, first failed check '${K10[control_failed]}' recorded all the same — ${K10[control_summary]}"
	assert "$([ "${K10[control_asked]}" -ge 1 ] && [ "${K10[control_failed]}" = withheld ] && [ "${K10[control_state]}" != denied ] && echo 0 || echo 1)" \
		"negative control: with the checks before the model turned off, the model is asked about the withheld request the normal stack refused without asking" \
		"asked ${K10[control_asked]} time(s) and '${K10[control_state]}', where the normal stack asked ${K10[withheld_asked]} time(s) and ended it '${K10[withheld_state]}'"

	stop_stack
}

###############################################################################################
# k10b
###############################################################################################

check_k10b() {
	expect "the judge package imports agentloop and no provider SDK directly, and makes no HTTP call to a model provider of its own"
	expect "the negative control, a scratch copy of the package that also imports github.com/anthropics/anthropic-sdk-go, lists it among its direct imports, which the check reports as a failure"

	local imports
	imports="$(cd "$ENVOY_DIR" && go list -f '{{join .Imports "\n"}}' ./internal/cipher/judge)"
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
	http_files="$(cd "$ENVOY_DIR/internal/cipher/judge" && grep -l '"net/http"' ./*.go | grep -v '_test\.go$' | tr '\n' ' ')"
	contains "model.go" "$http_files" && model_http="model.go"

	observe "go list -f '{{join .Imports \"\\n\"}}' ./internal/cipher/judge: $(tr '\n' ' ' <"$WORK_DIR/imports.txt")"
	observe "its net/http is in [$http_files] alone, the judge's Dispatch read; model.go, the file that calls the model, imports $(cd "$ENVOY_DIR/internal/cipher/judge" && sed -n '/^import (/,/^)/p' model.go | tr -d '\t"' | tr '\n' ' ')"
	assert "$([ "$has_agentloop" = 0 ] && [ -z "$found" ] && [ -z "$model_http" ] && echo 0 || echo 1)" \
		"the judge package imports agentloop and no provider SDK directly, and makes no HTTP call to a model provider of its own" \
		"its direct imports hold github.com/sjawhar/envoy/agentloop and none of [$sdks]${found:+ (found:$found)}; the file that asks the model, model.go, imports no net/http, so the one HTTP client in the package is the Dispatch read in [$http_files]"

	# --- The negative control: the same package with one import added. ---
	local scratch="$WORK_DIR/scratch"
	mkdir -p "$scratch/judge"
	cp "$ENVOY_DIR"/internal/cipher/judge/*.go "$scratch/judge/"
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
# which the judge hands to its person (the stand gives it no verdict), so it stays pending - and
# then writes the judged approval into Postgres by hand. It answers the grant in FORGED_GRANT and
# its placeholder in FORGED_PLACEHOLDER.
k10c_forge() { # <ttl> <reason> <name bound to S> <name>...
	local ttl="$1" reason="$2" bound_name="$3"
	shift 3
	ask "$ttl" "$reason" "$@"
	if [ "$ASK_STATE" != pending ]; then
		echo "judge-check: the record for [$*] was $ASK_STATE, not pending; the stand must hand it over before a record is forged" >&2
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
# child's to expand, never this script's, and curl reads the header on standard input.
curl_through() { # <the secret whose placeholder goes in the header>
	# shellcheck disable=SC2016 # $<NAME> and $AIRLOCK_S_URL are the child shell's to expand, not this one's
	printf '%s' 'printf "Authorization: Bearer %s" "$'"$1"'" | curl -sS -o /dev/null -w "%{http_code}" -H @- "$AIRLOCK_S_URL/ok"'
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
	expect "each of the two forged records the rules never admit is refused by the chain check: the CLI exits non-zero, S records nothing, and the broker's log names the chain refusal"
	expect "the negative controls, a record whose reason names no issue and one inside every rule, each written the same way, release"

	start_stack
	stack_facts
	local open="$AIRLOCK_DISPATCH_OPEN_ISSUE"
	local judged_value
	judged_value="$(stored_value "${STACK_SECRETS_PREFIX}airlock-judged-token")"

	# --- The judge's own grant. ---
	model_answers '{"verdict":"grant","reason":"the reason names a check run, as the guidance asks"}'
	ask 15m "$(run_reason)" "$AIRLOCK_JUDGED_SECRET"
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

	# --- Records of this script's own, two outside one rule and two inside them all. Each is a
	#     real record the broker made, with the judged approval written by hand. A model answer
	#     that is no verdict hands each request to its person, so it stays pending for that. ---
	model_answers '{"text":"no verdict here, so the approval below is written by hand"}'

	# The two records the rules never admit: a judge key beside a key the rules give the judge
	# nothing of, and that key alone. A record whose lifetime passes the entry's judge seconds
	# can no longer be written at all (Create refuses it), so the chain's lifetime arm is the Go
	# suite's (api's TestAForgedJudgedRecordReleasesNothing).
	k10c_forge 10m "reach s.airlock.test for $open" "$AIRLOCK_JUDGED_SECRET" "$AIRLOCK_JUDGED_SECRET" "$AIRLOCK_UNJUDGED_SECRET"
	local two_grant="$FORGED_GRANT"
	k10c_call two_secrets "$AIRLOCK_JUDGED_SECRET" 10m "reach s.airlock.test for $open" "$AIRLOCK_JUDGED_SECRET" "$AIRLOCK_UNJUDGED_SECRET"

	k10c_forge 10m "reach s.airlock.test for $open" "$AIRLOCK_UNJUDGED_SECRET" "$AIRLOCK_UNJUDGED_SECRET"
	local unjudged_grant="$FORGED_GRANT"
	k10c_call unjudged "$AIRLOCK_UNJUDGED_SECRET" 10m "reach s.airlock.test for $open" "$AIRLOCK_UNJUDGED_SECRET"

	local refusal="this grant's approval chain no longer verifies"
	local logged=0
	airlock_refusal_holds "$refusal" || logged=1
	observe "the forged record naming two secrets (grant $two_grant): the CLI exited ${K10C[two_secrets_exit]} — '${K10C[two_secrets_cli]}'; the call through its own placeholder answered ${K10C[two_secrets_proxy]} '${K10C[two_secrets_body]}'; S recorded ${K10C[two_secrets_recorded]} request(s)"
	observe "the forged record of $AIRLOCK_UNJUDGED_SECRET, which the rules give the judge nothing of (grant $unjudged_grant): the CLI exited ${K10C[unjudged_exit]} — '${K10C[unjudged_cli]}'; the call through its own placeholder answered ${K10C[unjudged_proxy]} '${K10C[unjudged_body]}'; S recorded ${K10C[unjudged_recorded]} request(s)"
	observe "the broker's log: $([ "$logged" = 0 ] && printf '%s' "$BROKER_LOG_LINE" | cut -c1-300 || echo 'no refusal record whose reason names the chain refusal')"
	assert "$([ "${K10C[two_secrets_exit]}" != 0 ] && [ "${K10C[unjudged_exit]}" != 0 ] &&
		[ "${K10C[two_secrets_recorded]}" = 0 ] && [ "${K10C[unjudged_recorded]}" = 0 ] &&
		[ "${K10C[two_secrets_proxy]}" = 403 ] && [ "${K10C[unjudged_proxy]}" = 403 ] &&
		contains "$refusal" "${K10C[two_secrets_body]}" && contains "$refusal" "${K10C[unjudged_body]}" &&
		[ "$logged" = 0 ] && echo 0 || echo 1)" \
		"each of the two forged records the rules never admit is refused by the chain check: the CLI exits non-zero, S records nothing, and the broker's log names the chain refusal" \
		"the two CLI runs exited ${K10C[two_secrets_exit]} and ${K10C[unjudged_exit]}; the two calls through the forged placeholders answered ${K10C[two_secrets_proxy]} and ${K10C[unjudged_proxy]}, each in the words '$refusal'; S recorded ${K10C[two_secrets_recorded]} and ${K10C[unjudged_recorded]} requests, and the broker logged the refusal"

	# --- The first negative control: a record whose reason names no issue, which the rules
	#     admit, since what a reason says is the judge's to read and no condition of the chain. ---
	local issue_mark
	issue_mark="$(records_count s)"
	k10c_forge 10m "reach s.airlock.test, no issue named" "$AIRLOCK_SHARED_SECRET" "$AIRLOCK_SHARED_SECRET"
	agent-secrets "$AIRLOCK_SHARED_SECRET" --ttl 10m --reason "reach s.airlock.test, no issue named" --wait 0 -- \
		sh -c "$(curl_through "$AIRLOCK_SHARED_SECRET")" >"$WORK_DIR/no-issue.out" 2>"$WORK_DIR/no-issue.err"
	local no_issue_exit=$?
	local no_issue_recorded="$(($(records_count s) - issue_mark))"
	control "a forged record whose reason names no issue (one shared judge key, 600 seconds): agent-secrets -- curl exited $no_issue_exit and curl answered '$(tr -d '\n' <"$WORK_DIR/no-issue.out")'; S recorded $no_issue_recorded request(s)"
	assert "$([ "$no_issue_exit" = 0 ] && [ "$(tr -d '\n' <"$WORK_DIR/no-issue.out")" = 200 ] && [ "$no_issue_recorded" -ge 1 ] && echo 0 || echo 1)" \
		"negative control: a forged record whose reason names no issue releases, since no issue rule is part of the chain" \
		"it exited $no_issue_exit and S recorded $no_issue_recorded request(s)"

	# --- The second negative control: a fourth record written the same way, inside every rule. ---
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
# which is the one way to give a broker other rules: cmd/cipherd/main.go reads
# BROKER_AIRLOCK_RULES_FILE once, at boot.
k10d_rules() { # <path> <seconds>
	jq --arg n "$AIRLOCK_JUDGED_SECRET" --argjson s "$2" \
		'.secrets = [.secrets[] | if .name == $n then .judge = $s else . end]' \
		"$AIRLOCK_RULES_FILE" >"$1"
}

check_k10d() {
	expect "the --ttl 15m request, past the entry's 300 judge seconds, is refused when it is written, naming --ttl 5m, and the stand-in is not asked"
	expect "the --ttl 5m request is granted by judge:<model id>"
	expect "the negative control, the same request on a broker whose entry gives the judge the class maximum of 900 seconds, is granted"

	start_stack
	stack_facts
	local open="$AIRLOCK_DISPATCH_OPEN_ISSUE"
	model_answers '{"verdict":"grant","reason":"the reason names a check run, as the guidance asks"}'

	# The test secret's entry given judge 300, on a broker of this stack's own database,
	# stand-ins and certificate authority, with the stack's two fronts pointed at it.
	k10d_rules "$WORK_DIR/rules-300.json" 300
	start_extra_broker judge-300 BROKER_AIRLOCK_RULES_FILE="$WORK_DIR/rules-300.json"
	front_backends "$EXTRA_API_ADDR" "$EXTRA_PROXY_ADDR"

	local mark
	mark="$(records_count model)"
	ask 15m "$(run_reason)" "$AIRLOCK_JUDGED_SECRET"
	local long_exit="$ASK_STATUS" long_err="$ASK_ERR"
	local long_written
	long_written="$(psql_stack -c "select count(*) from requests where state='pending' and created_at > now() - interval '30 seconds'")"
	local long_asked="$(($(records_count model) - mark))"
	cancel_pending

	mark="$(records_count model)"
	ask 5m "$(run_reason)" "$AIRLOCK_JUDGED_SECRET"
	local short_id="$ASK_ID"
	await_granted "$short_id" 60
	local short_state="$AWAIT_STATE" short_decider short_asked
	short_decider="$(request_decider "$short_id")"
	short_asked="$(($(records_count model) - mark))"
	cancel_pending

	observe "with the entry's judge at 300 seconds, --ttl 15m: exit $long_exit, '$long_err', $long_written pending request(s) written in the last 30s, the stand asked $long_asked time(s)"
	observe "with the same entry, --ttl 5m: $short_state after ${AWAIT_SECONDS}s, decided by '$short_decider', the stand asked $short_asked time(s) — $(judged_summary "$short_id")"
	assert "$([ "$long_exit" != 0 ] && contains "--ttl 5m" "$long_err" && [ "$long_asked" = 0 ] && echo 0 || echo 1)" \
		"the --ttl 15m request, past the entry's 300 judge seconds, is refused when it is written, naming --ttl 5m, and the stand-in is not asked" \
		"it exited $long_exit with '$long_err' and the stand was asked $long_asked time(s)"
	assert "$([ "$short_state" = granted ] && [ "$short_decider" = "$JUDGE_ACTOR" ] && echo 0 || echo 1)" \
		"the --ttl 5m request is granted by judge:<model id>" \
		"it was $short_state, decided by '$short_decider' against the expected '$JUDGE_ACTOR'"

	# --- The negative control: the same request on a broker whose entry gives the judge 900
	#     seconds, the class maximum, where no lifetime bound can refuse a --ttl 15m request and
	#     nothing else about the run changes. ---
	front_backends_stack
	k10d_rules "$WORK_DIR/rules-900.json" 900
	start_extra_broker judge-900 BROKER_AIRLOCK_RULES_FILE="$WORK_DIR/rules-900.json"
	front_backends "$EXTRA_API_ADDR" "$EXTRA_PROXY_ADDR"
	mark="$(records_count model)"
	ask 15m "$(run_reason)" "$AIRLOCK_JUDGED_SECRET"
	local control_id="$ASK_ID"
	await_granted "$control_id" 60
	local control_state="$AWAIT_STATE" control_decider control_asked
	control_decider="$(request_decider "$control_id")"
	control_asked="$(($(records_count model) - mark))"

	control "the control is a second broker on the stack's own database, stand-ins and authority whose only difference is that entry's judge seconds: 900, the class maximum, so no lifetime bound can refuse a --ttl 15m request and nothing else moves. The same --ttl 15m request there was $control_state after ${AWAIT_SECONDS}s, decided by '$control_decider', with the stand asked $control_asked time(s)."
	assert "$([ "$control_state" = granted ] && [ "$control_decider" = "$JUDGE_ACTOR" ] && [ "$control_asked" -ge 1 ] && echo 0 || echo 1)" \
		"negative control: the same --ttl 15m request on a broker whose entry gives the judge the class maximum is granted" \
		"with the entry at the class maximum the same request was $control_state, decided by '$control_decider', where at judge 300 it was refused when written (exit $long_exit)"

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
	model_answers '{"verdict":"grant","reason":"the reason names a check run, as the guidance asks"}'

	# Two judged grants of the owned human-tier test secret, both made before the move, so the
	# release re-check runs over each: the lifetimes differ, since a request for the same names
	# and the same lifetime is handed the first grant back (reuseLiveGrant).
	ask 15m "$(run_reason)" "$AIRLOCK_JUDGED_SECRET"
	local main_id="$ASK_ID"
	await_granted "$main_id" 60
	local main_state="$AWAIT_STATE" main_grant
	main_grant="$(request_grant "$main_id")"
	release "$main_grant" "$AIRLOCK_JUDGED_SECRET"
	local main_pw="$PROXY_PASSWORD" main_ph="$PLACEHOLDER"
	call_s "$main_pw" "$main_ph"
	local before_status="$CALL_STATUS" before_header
	before_header="$(s_authorization)"

	ask 10m "$(run_reason)" "$AIRLOCK_JUDGED_SECRET"
	local control_req="$ASK_ID"
	await_granted "$control_req" 60
	local control_grant
	control_grant="$(request_grant "$control_req")"
	release "$control_grant" "$AIRLOCK_JUDGED_SECRET"
	local control_pw="$PROXY_PASSWORD" control_ph="$PLACEHOLDER"

	local version_before version_after
	version_before="$(psql_stack -c "select rules_version from requests where id='$main_id'")"

	# The policy version moves. The rules file is not what moves it: cmd/cipherd/main.go reads
	# that file once at boot, and no running broker rereads it, so an unrelated entry of the
	# *store* is what a live broker rereads - here one unrelated secret's tags, which
	# policy.Set's version digest folds in.
	set_fake_tags "${STACK_SECRETS_PREFIX}airlock-demo-token" '{"owner":"legion-daemon","tier":"agent","max-grant":"6h"}'
	reread_all AIRLOCK_DEMO_TOKEN || return
	sleep 2
	ask 7m "$(run_reason)" "$AIRLOCK_JUDGED_SECRET"
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
	model_answers '{"verdict":"grant","reason":"the reason names a check run, as the guidance asks"}'

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
	ask 10m "$(run_reason)" "$AIRLOCK_JUDGED_SECRET"
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
# k10g
###############################################################################################

# k10g_control_tree copies this module's source into a tree of its own under the run's work
# directory and takes the admin refusal out of the three places it lives: Create's choice of who
# decides, which gives an administrator's key to a person whatever the rules say
# (Machine.judgeKey), the judge's own admin check, which stops a run before any model is asked
# (Runner.checks), and the guard currentPolicyAdmits makes for the judge inside the decision
# transaction. The copy leaves out
# the two large build products at the module root (out/ and the dispatch-e2e binary), which no
# build of cmd/cipherd reads, and GOWORK=off below builds it as the module it is rather than
# through the repository's workspace, so nothing of this run touches go.work.sum.
#
# Each edit is checked after it is made. A patch that silently matched nothing would build the
# very broker this check already has, and the control would then "pass" for the wrong reason:
# a control is only worth running when it is certain that what it took out was there.
k10g_control_tree() { # <directory>
	local dir="$1"
	mkdir -p "$dir"
	(cd "$ENVOY_DIR" && tar -cf - --exclude=./out --exclude=./dispatch-e2e --exclude=./node_modules .) | tar -xf - -C "$dir"
	# The judge's check: the tag the policy read answers is read no longer, so the admin check
	# always passes and the model is asked about an administrator's secret.
	perl -0777 -pi -e 's/\tnow, withheld, admin, err :=/\tnow, withheld, _, err :=/;
		s/\n\t\tif admin && adminOK \{\n\t\t\tadminOK, adminDetail = false, decidedByAPerson\(name\)\n\t\t\}\n/\n/' \
		"$dir/internal/cipher/judge/judge.go"
	# The transaction's guard, beside the withheld one at the head of currentPolicyAdmits.
	perl -0777 -pi -e 's/\n\t\t\tif set\.Secrets\[g\.name\]\.Admin \{\n\t\t\t\treturn fmt\.Errorf\("%w: %s is an administrator.s secret, and a person approves every release of one", record\.ErrNotApprover, g\.name\)\n\t\t\t\}\n/\n/' \
		"$dir/internal/cipher/requests/machine_state.go"
	# Create's choice of who decides (Machine.judgeKey): without the tag's test an
	# administrator's key with a judge entry is a judge key, written pending for the judge.
	perl -0777 -pi -e 's/ \|\| set\.Secrets\[name\]\.Admin \{/ {/' "$dir/internal/cipher/requests/machine_state.go"
	local in_judge in_transaction in_create
	in_judge="$(grep -c 'if admin && adminOK' "$dir/internal/cipher/judge/judge.go")"
	in_transaction="$(grep -c 'set.Secrets\[g.name\].Admin' "$dir/internal/cipher/requests/machine_state.go")"
	in_create="$(grep -c 'set.Secrets\[name\].Admin' "$dir/internal/cipher/requests/machine_state.go")"
	if [ "$in_judge" != 0 ] || [ "$in_transaction" != 0 ] || [ "$in_create" != 0 ]; then
		echo "judge-check: the control patch left the admin refusal in the tree ($in_judge in the judge's checks, $in_transaction in the decision transaction, $in_create in Create); a control built from it would prove nothing" >&2
		exit 1
	fi
}

# k10g_ask asks for the admin secret and reads back everything the check's sentence is about:
# the request's state, whether the stand was asked, the judged row, and whether any grant was
# made for that request. The grant is counted in the database rather than read off the status
# route alone, so "no grant is made" is the rows and not the answer one route gives.
k10g_ask() { # <ttl> <reason>
	local mark
	mark="$(records_count model)"
	ask "$1" "$2" "$AIRLOCK_ADMIN_SECRET"
	K10G_ID="$ASK_ID"
	if [ -z "$K10G_ID" ]; then
		K10G_STATE="(no request)"
		K10G_ASKED=0
		K10G_ROWS=0
		K10G_GRANTS=0
		return
	fi
	if [ "$ASK_STATE" = pending ]; then await_judged "$K10G_ID" 15; fi
	K10G_STATE="$(request_state "$K10G_ID")"
	K10G_ASKED="$(($(records_count model) - mark))"
	K10G_ROWS="$(judged_rows "$K10G_ID")"
	K10G_GRANTS="$(psql_stack -c "select count(*) from grants where request_id='$K10G_ID'")"
}

check_k10g() {
	expect "the admin-tagged request, whose rules entry gives the judge seconds, is its person's card from the start: not held for the judge, listed with its approve_url, with the stand-in not asked, no request.judged row and no grant"
	expect "the negative control, a broker built from a source tree with the admin refusal taken out of Create, the judge's checks and the decision transaction, grants the same request by judge:<model id>"

	start_stack
	stack_facts
	local open="$AIRLOCK_DISPATCH_OPEN_ISSUE"
	# The stand answers grant to everything from here on, so nothing but the broker's own
	# refusal can keep this request from a grant.
	model_answers '{"verdict":"grant","reason":"grant"}'

	k10g_ask 15m "$(run_reason)"
	local id="$K10G_ID" state="$K10G_STATE" asked="$K10G_ASKED" rows="$K10G_ROWS"
	local grants="$K10G_GRANTS" admin_deciding admin_url admin_listed
	admin_deciding="$(deciding)"
	admin_url="$(page_url)"
	admin_listed="$(listed "$ASK_RECORD")"
	cancel_pending

	observe "the admin-tagged secret $AIRLOCK_ADMIN_SECRET (human tier, admin: \"true\", max-grant $AIRLOCK_ADMIN_MAX_GRANT, and a rules entry of judge $AIRLOCK_JUDGE_SECONDS like the judged secret's own), asked for with --ttl 15m: judge_deciding '$admin_deciding', approve_url '$admin_url', listed '$admin_listed', $state, the stand asked $asked time(s), $rows judged row(s), $grants grant(s)"
	assert "$([ "$state" = pending ] && [ "$admin_deciding" = false ] && [ -n "$admin_url" ] && [ "$admin_listed" = yes ] && [ "$asked" = 0 ] && [ "$rows" = 0 ] && [ "$grants" = 0 ] && echo 0 || echo 1)" \
		"the admin-tagged request is its person's card from the start: not held, listed with its approve_url, with the stand-in not asked, no request.judged row and no grant" \
		"it was $state, judge_deciding '$admin_deciding', listed '$admin_listed', with the stand asked $asked time(s), $rows row(s) and $grants grant(s), where the stand answers grant to everything"

	# --- The negative control: the same request at a broker with the refusal taken out. ---
	local tree="$WORK_DIR/control-src" binary="$WORK_DIR/broker-no-admin-refusal"
	k10g_control_tree "$tree"
	local built=0
	(cd "$tree" && GOWORK=off go build -o "$binary" ./cmd/cipherd) >"$WORK_DIR/control-build.log" 2>&1 || built=1
	if [ "$built" != 0 ]; then
		control "the control broker could not be built from $tree: $(tail -n 3 "$WORK_DIR/control-build.log" | tr '\n' ' ')"
		fail "negative control: with the admin refusal taken out of all three places, the same request is granted by judge:<model id> — the control broker did not build, so the control was not run"
		stop_stack
		return
	fi
	EXTRA_BROKER_BIN="$binary"
	start_extra_broker no-admin-refusal
	EXTRA_BROKER_BIN=""
	front_backends "$EXTRA_API_ADDR" "$EXTRA_PROXY_ADDR"

	local mark
	mark="$(records_count model)"
	ask 15m "$(run_reason)" "$AIRLOCK_ADMIN_SECRET"
	local control_id="$ASK_ID"
	await_granted "$control_id" 60
	local control_state="$AWAIT_STATE" control_decider control_asked control_grants
	control_decider="$(request_decider "$control_id")"
	control_asked="$(($(records_count model) - mark))"
	control_grants="$(psql_stack -c "select count(*) from grants where request_id='$control_id'")"
	front_backends_stack

	control "the control is a broker built from a copy of this module's source under $tree with three deletions and nothing else: Create's choice of a person for an administrator's key (machine_state.go, judgeKey), the judge's admin check (judge.go, Runner.checks) and the guard currentPolicyAdmits makes for the judge (machine_state.go). It runs on the stack's own database, rules file, fake store and identities, and the stack's two fronts were pointed at it, so the only difference between the two runs is the refusal itself. BROKER_JUDGE_TEST_SKIP_CHECKS is no control for this check and was not used: it leaves Create's choice and the transaction guard standing, so the request would still be its person's or its approval refused."
	control "the same --ttl 15m request for $AIRLOCK_ADMIN_SECRET at that broker was $control_state after ${AWAIT_SECONDS}s, decided by '$control_decider', with the stand asked $control_asked time(s) and $control_grants grant(s) made; its judged row: $(judged_summary "$control_id")"
	assert "$([ "$control_state" = granted ] && [ "$control_decider" = "$JUDGE_ACTOR" ] && [ "$control_asked" -ge 1 ] && echo 0 || echo 1)" \
		"negative control: with the admin refusal taken out of Create, the judge's checks and the decision transaction, the same request is granted by judge:<model id>" \
		"it was $control_state, decided by '$control_decider', where at the stack's own brokers the same request stayed '$state'"

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
k10g) check_k10g ;;
esac

report
