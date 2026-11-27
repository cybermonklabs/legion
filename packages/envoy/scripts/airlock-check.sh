#!/usr/bin/env bash
# packages/envoy/scripts/airlock-check.sh
#
# Runs one before_merge check of the airlock's verification document end to end and says
# whether it passed, so that the implementer and the tester run the same command and read the
# same evidence.
#
# What one run proves is the check's own sentence and nothing wider. The script starts the
# local stack the check needs (scripts/dev-airlock.sh, with the options that check's negative
# control asks for), runs the check's steps against it, runs the negative control, stops the
# stack, and prints:
#
#   - a result block to paste into `dispatch result`: what the check expected, what this run
#     observed, and what the negative control did;
#   - one PASS: or FAIL: line for each property the check's outcome sentence names, and one for
#     its negative control, because a control that does not behave as the check says leaves the
#     check proving nothing;
#   - one last line, `check <id>: passed`, `check <id>: failed (<n> of <m>)`, or, when every
#     property it judged passed and one it could not judge (an UNKNOWN: or DEFERRED: line, a
#     property this head does not build), `check <id>: not every property judged (...)`, which is
#     no pass.
#
# No line it prints is a credential. A value is compared by its SHA-256 (the first 16 hex
# digits) and its length, and every line that compares one says so; that is a rule of the
# verification document itself, and it holds for the stand-in values of a local stack too.
#
# Two things this pod measured shape two checks:
#
#   - k1 scans the command's memory, and /proc/sys/kernel/yama/ptrace_scope is 1 here, so only
#     a process that is the command's own ancestor may read its /proc/<pid>/mem. The scan is
#     therefore a small Python program (scripts/airlock/memscan.py) that starts
#     `agent-secrets` itself and scans the descendant it started.
#   - k15 needs no stack at all: it builds the Lean model, prints its theorem statements and
#     their axioms, runs the comparison fuzz test for ten minutes, and runs the ten mutants it
#     names through scripts/airlock-mutants.sh.
#
# Where a check needs a secret the stack does not hold, this script extends the stack's own
# fake Secrets Manager (the file AIRLOCK_FAKE_SECRETS_FILE names) at run time and makes every
# broker reread it; a plain stored value with no proxy tag and no profile is served by its own
# name. It never edits the stack's profiles file (AIRLOCK_PROFILES_FILE, which dev-airlock.sh
# writes and each broker reads once, at boot): a control that needs other kinds, other profiles,
# other proxy settings or other code is a second broker process this script starts on the stack's
# own database, certificate authority and stand-ins, with a profiles file of its own, that one
# setting changed, or a binary built from a copy of this module with one defect put in.
#
# Usage: packages/envoy/scripts/airlock-check.sh <id> [--keep-stack]
#        <id> is k1, k2, k3, k4, k5, k6, k7, k8, k9, k15 or k47. --keep-stack leaves the stack and
#        the work directory up for a person to look at a failure with. --help prints this usage.
#        A usage error exits 2.
#
# Needs bash, curl, jq, go, python3, openssl, node and psql; k2 and k7 also need gh (k2's
# own step and the negative controls of k2 and k7 run the agent-secrets of the pinned release
# before the airlock, OLD_CLI_DEFAULT_TAG below, which AIRLOCK_OLD_CLI_TAG overrides, and k7's
# consumer files come from sjawhar/dotfiles), k6 and k7 need bun, wget, git, uv and the
# clients scripts/airlock-clients.sh
# installs (az, gcloud, pulumi and boto3), k9 needs git, k15 needs lake from the Lean toolchain
# this environment names, and k47 builds two control brokers with go. DEV_BROKER_POSTGRES_URL is
# passed to dev-airlock.sh as it is, so a pod with no container runtime names its test server
# there.
# k7 also requires four operator inputs, and refuses to start without them, naming each one
# missing: AIRLOCK_GCLOUD_SECRET, AIRLOCK_GWS_READ_SECRET and AIRLOCK_GWS_ADMIN_SECRET, the
# broker names of the operator's own gcloud login and Google Workspace read and administrator
# logins, which the consumers k7 runs read by those names; and AIRLOCK_GWS_TIER_KEY, the account
# key in gws/accounts.json whose tiers.read.command k7 reports. No broker name or account key is
# supplied by default.
#
# k9 is a long run: ten minutes of connections and two stacks, about twenty-five minutes in all.
# k47 builds two brokers and approves four grants, about ten minutes.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENVOY_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
LEAN_DIR="$ENVOY_DIR/internal/cipher/proxyrules/lean"
# shellcheck source=packages/envoy/scripts/airlock/k7-consumers.sh
source "$SCRIPT_DIR/airlock/k7-consumers.sh"
# psql is given a database URL without its password, and the password in PGPASSWORD.
# shellcheck source-path=SCRIPTDIR source=lib/pg-url.sh
source "$SCRIPT_DIR/lib/pg-url.sh"
# A broker this script starts gets the stack broker's environment through its own environment,
# never through a process's arguments (exec_with_env, broker_env_of, with_broker_env).
# shellcheck source-path=SCRIPTDIR source=lib/exec-env.sh
source "$SCRIPT_DIR/lib/exec-env.sh"

usage() {
	cat <<'EOF'
usage: airlock-check.sh <id> [--keep-stack]

Runs one before_merge check of the airlock's verification document and prints, for each
property the check's outcome sentence names, what it expected beside what it got.

  <id>           k1, k2, k3, k4, k5, k6, k7, k8, k9, k15 or k47
  --keep-stack   leave the stack and this run's work directory up, to look at a failure with
  --help         print this

k7 needs AIRLOCK_GCLOUD_SECRET, AIRLOCK_GWS_READ_SECRET and AIRLOCK_GWS_ADMIN_SECRET (the broker
names of the operator's gcloud and Google Workspace logins) and AIRLOCK_GWS_TIER_KEY (the account
key in gws/accounts.json whose read tier it reports) in its environment, and refuses to start
without them.

--keep-stack does not work with k6: k6 starts a second stack (its --no-kind control) after the
first, and with the first kept up the second cannot bind (listen tcp 127.0.0.2:443: bind: address
already in use). Use --keep-stack with the other checks.

It exits 0 only when every property passed, 1 when one did not, 2 on a usage error, and 3 when
every property it judged passed and one it could not judge here (an UNKNOWN: or DEFERRED: line),
which is no pass. k9 takes about twenty-five minutes and k47 about ten; every other check takes a
few.
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
		echo "airlock-check: unexpected option $1" >&2
		usage >&2
		exit 2
		;;
	*)
		if [ -n "$CHECK_ID" ]; then
			echo "airlock-check: one check id at a time, not $CHECK_ID and $1" >&2
			usage >&2
			exit 2
		fi
		CHECK_ID="$1"
		;;
	esac
	shift
done

if [ -z "$CHECK_ID" ]; then
	echo "airlock-check: name the check to run" >&2
	usage >&2
	exit 2
fi

case "$CHECK_ID" in
k1 | k2 | k3 | k4 | k5 | k6 | k7 | k8 | k9 | k15 | k47) ;;
*)
	echo "airlock-check: $CHECK_ID is no check of this document; it holds k1, k2, k3, k4, k5, k6, k7, k8, k9, k15 and k47" >&2
	usage >&2
	exit 2
	;;
esac

if [ "$CHECK_ID" = k7 ]; then
	k7_consumer_require_inputs || exit $?
fi

WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/airlock-check-$CHECK_ID.XXXXXX")"
STACK_LOG="$WORK_DIR/stack.log"
STACK_PID=""
EXTRA_BROKER_PIDS=()
HELPER_PIDS=()

cleanup() {
	local pid
	for pid in "${HELPER_PIDS[@]+"${HELPER_PIDS[@]}"}" "${EXTRA_BROKER_PIDS[@]+"${EXTRA_BROKER_PIDS[@]}"}"; do
		if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
			kill "$pid" 2>/dev/null || true
			wait "$pid" 2>/dev/null || true
		fi
	done
	if [ "$KEEP_STACK" = true ] && [ -n "$STACK_PID" ]; then
		echo "airlock-check: the stack is still up (dev-airlock.sh pid $STACK_PID, log $STACK_LOG);" >&2
		echo "airlock-check: stop it with kill $STACK_PID, which drops its databases" >&2
		return
	fi
	# SIGTERM, never SIGKILL: dev-airlock.sh's own EXIT trap is what stops its processes and
	# drops the two databases it made.
	if [ -n "$STACK_PID" ] && kill -0 "$STACK_PID" 2>/dev/null; then
		kill "$STACK_PID" 2>/dev/null || true
		wait "$STACK_PID" 2>/dev/null || true
	fi
	if [ "$KEEP_STACK" = true ]; then
		echo "airlock-check: work directory kept at $WORK_DIR" >&2
	else
		rm -rf "$WORK_DIR"
	fi
}
trap cleanup EXIT

# --- What a run says -----------------------------------------------------------------------
#
# Three lists make the result block, and three counters make the verdict. A property is recorded
# with pass or fail, or, when this head does not build what it is about, as not judged: an
# UNKNOWN: line (unknown) or a DEFERRED: line naming the slice that builds it (deferred). A
# property not judged is never a pass: a run with one ends "not every property judged" and
# exits 3, so nothing reads it as a check that passed.

EXPECTED=()
OBSERVED=()
CONTROL=()
PASSED=0
TOTAL=0
NOT_JUDGED=0
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

unknown() {
	NOT_JUDGED=$((NOT_JUDGED + 1))
	VERDICTS+=("UNKNOWN: $1")
}

deferred() { # <what the property is> <the slice that builds it> <what was seen>
	NOT_JUDGED=$((NOT_JUDGED + 1))
	VERDICTS+=("DEFERRED: $1 — deferred to $2: $3")
}

# judge records one property from a condition the caller has already evaluated: "0" passes and
# anything else, an empty answer included, fails. A step that could not be evaluated at all is
# a property that was not proved, which is a failure and never a silent pass.
judge() { # <status> <what the property is> <what was seen>
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
	if [ "$TOTAL" -eq 0 ] && [ "$NOT_JUDGED" -eq 0 ]; then
		echo "check $CHECK_ID: UNKNOWN: no property was judged, so nothing passed"
		return 3
	fi
	if [ "$PASSED" -eq "$TOTAL" ] && [ "$NOT_JUDGED" -eq 0 ]; then
		echo "check $CHECK_ID: passed"
		return 0
	fi
	if [ "$PASSED" -eq "$TOTAL" ]; then
		echo "check $CHECK_ID: not every property judged ($PASSED passed, $NOT_JUDGED not judged here)"
		return 3
	fi
	echo "check $CHECK_ID: failed ($((TOTAL - PASSED)) of $TOTAL$([ "$NOT_JUDGED" -gt 0 ] && printf ', %d not judged' "$NOT_JUDGED"))"
	return 1
}

# --- Comparing a value without printing it ---------------------------------------------------
#
# Every comparison in this script is by SHA-256 and length, and every line that makes one says
# so. digest answers the first 16 hex digits of a value's SHA-256, which is far more than two
# values of one run ever share and far too little to be the value.

digest() { printf '%s' "$1" | sha256sum | cut -c1-16; }
shape() { printf 'len=%d sha256=%s' "${#1}" "$(digest "$1")"; }

# contains reports whether haystack holds needle, without either reaching the output.
contains() { case "$2" in *"$1"*) return 0 ;; *) return 1 ;; esac; }

# --- The stack ---------------------------------------------------------------------------------

# await waits for a line a process logs, and ends the run the moment that process dies: a
# stack that never came up proves nothing, and the check's own steps would each fail with a
# reason that is not the check's.
await() { # <what> <seconds> <log> <pattern> <pid>
	local i
	for ((i = 0; i < $2 * 2; i++)); do
		if grep -qE "$4" "$3" 2>/dev/null; then return 0; fi
		if ! kill -0 "$5" 2>/dev/null; then
			echo "airlock-check: $1 exited before it was ready; see $3" >&2
			tail -n 20 "$3" >&2 || true
			exit 1
		fi
		sleep 0.5
	done
	echo "airlock-check: $1 was not ready within $2 seconds; see $3" >&2
	exit 1
}

# start_stack runs dev-airlock.sh with the options its caller names, waits for its banner, and
# exports everything that banner names. dev-broker.sh's own export block is in the same log and
# is read first, so where the two name one variable the airlock's wins, as the banner says.
start_stack() {
	echo "airlock-check: starting the stack (dev-airlock.sh $*)..." >&2
	: >"$STACK_LOG"
	bash "$SCRIPT_DIR/dev-airlock.sh" "$@" >"$STACK_LOG" 2>&1 &
	STACK_PID=$!
	await "the stack" 900 "$STACK_LOG" '^dev-airlock: ready\.$' "$STACK_PID"
	eval "$(sed -n 's/^  \(export [A-Z][A-Z_0-9]*=.*\)$/\1/p' "$STACK_LOG")"
	mapfile -t BROKER_API_ADDRS < <(sed -n 's/^  broker [0-9]*: *api \([^,]*\),.*/\1/p' "$STACK_LOG")
	mapfile -t BROKER_PROXY_ADDRS < <(sed -n 's/^  broker [0-9]*:.*proxy \([^,]*\),.*/\1/p' "$STACK_LOG")
	STACK_DB_NAME="$(sed -n 's/^dev-airlock: created isolated database \([a-z0-9_]*\).*/\1/p' "$STACK_LOG" | head -n 1)"
	# The brokers' database URL, built exactly as dev-airlock.sh builds it, which is the one
	# way to reach the rows a check sets up by hand (an enrollment's withheld names).
	if [ -n "${DEV_BROKER_POSTGRES_URL:-}" ] && [[ "$DEV_BROKER_POSTGRES_URL" =~ ^(postgres(ql)?://[^/?]+)/[^/?]*(\?.*)?$ ]]; then
		STACK_DB_URL="${BASH_REMATCH[1]}/${STACK_DB_NAME}${BASH_REMATCH[3]:-}"
	else
		STACK_DB_URL="postgres://postgres:dispatch@127.0.0.1:55432/${STACK_DB_NAME}?sslmode=disable"
	fi
	BROKER_BIN="$DEV_BROKER_DIR/bin/broker"
	# dev-broker.sh builds this binary with the rest of the stack's binaries, under the work
	# directory its own banner named. Three of this script's steps are subcommands of it.
	STANDS_BIN="$DEV_BROKER_DIR/bin/dev-airlock-stands"
	echo "airlock-check: the stack is up (${#BROKER_API_ADDRS[@]} brokers, work directory $AIRLOCK_DIR)" >&2
}

# stop_stack stops this run's own extra brokers first and then dev-airlock.sh. The order
# matters: a broker of this script's still holding a connection to the stack's database keeps
# dev-airlock.sh's own cleanup from dropping it, and the database is left behind on the server.
stop_stack() {
	if [ "$KEEP_STACK" = true ]; then return 0; fi
	local pid
	for pid in "${EXTRA_BROKER_PIDS[@]+"${EXTRA_BROKER_PIDS[@]}"}"; do
		if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
			kill "$pid" 2>/dev/null || true
			wait "$pid" 2>/dev/null || true
		fi
	done
	EXTRA_BROKER_PIDS=()
	if [ -n "$STACK_PID" ] && kill -0 "$STACK_PID" 2>/dev/null; then
		echo "airlock-check: stopping the stack..." >&2
		kill "$STACK_PID" 2>/dev/null || true
		wait "$STACK_PID" 2>/dev/null || true
	fi
	STACK_PID=""
}

# broker_env answers, in BROKER_ENV, the environment of a broker like the stack's own with the
# caller's overrides on top, one NAME=value entry each (lib/exec-env.sh's broker_env_of). It is
# read out of the first stack broker's /proc/<pid>/environ rather than spelled here, so that a
# broker this script starts can never lack a setting dev-airlock.sh gave the stack's. That
# script's own launcher is the one that has to spell the environment: it starts the first broker
# and has nothing to copy from. A broker is started with it through lib/exec-env.sh's
# with_broker_env, in its own environment and in no process's arguments, since the stack broker's
# environment holds its database URL, password and all.
broker_env() { # <VAR=value>...
	local pid
	pid="$(sed -n 's/^  broker [0-9]*:.*pid \([0-9]*\),.*/\1/p' "$STACK_LOG" | head -n 1)"
	broker_env_of "$pid" "$@"
}

# start_extra_broker starts one more broker process beside the stack's, on its database, profiles
# file, certificate authority and stand-ins, with the environment the caller adds on top, and
# answers its API and proxy addresses in EXTRA_API_ADDR and EXTRA_PROXY_ADDR. It is how a control
# that turns one proxy setting off is run: the stack's own brokers keep the setting, and the
# command's HTTPS_PROXY is pointed at this one for that step alone. EXTRA_BROKER_BIN is the binary
# it runs, the stack's own when the caller sets none: k47's controls are brokers built from a copy
# of this module with one defect put in, which no setting stands in for, and everything else
# about that process - the database, the profiles file, the fake store, the identities - has to
# stay the stack's. judge-check.sh's start_extra_broker is the same device.
start_extra_broker() { # <name> <VAR=value>...
	local name="$1"
	shift
	local log="$WORK_DIR/broker-$name.log"
	: >"$log"
	# Three ports the system picks, so this one never collides with the broker it was copied from.
	broker_env BROKER_LISTEN_ADDR="127.0.0.1:0" BROKER_AIRLOCK_LISTEN_ADDR="127.0.0.1:0" BROKER_METRICS_LISTEN_ADDR="127.0.0.1:0" "$@"
	( with_broker_env "${EXTRA_BROKER_BIN:-$BROKER_BIN}" ) >"$log" 2>&1 &
	local pid=$!
	EXTRA_BROKER_PIDS+=("$pid")
	await "the $name broker" 120 "$log" '"message":"airlock proxy listening"' "$pid"
	EXTRA_API_ADDR="$(logged_addr 'broker listening' "$log")"
	EXTRA_PROXY_ADDR="$(logged_addr 'airlock proxy listening' "$log")"
}

# front_backends points the stack's two fronts at one API address and one proxy address, so a
# command whose AGENT_SECRETS_URL and HTTPS_PROXY are the fronts - every agent-secrets run - is
# served by a broker this script started. The broker's public URL is the front's, so a session's
# proofs, which are signed for that URL, verify at it. front_backends_stack points them back at
# the stack's own brokers. Each front rereads its backends file when it changes; a moment is
# enough for both to have read it.
front_backends() { # <api addr> <proxy addr>
	printf '%s\n' "$1" >"$AIRLOCK_DIR/api-backends"
	printf '%s\n' "$2" >"$AIRLOCK_DIR/proxy-backends"
	sleep 2
}
front_backends_stack() {
	printf '%s\n' "${BROKER_API_ADDRS[@]}" >"$AIRLOCK_DIR/api-backends"
	printf '%s\n' "${BROKER_PROXY_ADDRS[@]}" >"$AIRLOCK_DIR/proxy-backends"
	sleep 2
}

# logged_addr answers the addr field of the first line of a broker's log whose message is the one
# named, or nothing. The broker logs one JSON object per line, and a line still being written
# parses as nothing, so half an address is never read as the whole one.
logged_addr() { # <message> <log>
	jq -Rnr --arg message "$1" '[inputs | fromjson? | select(.message == $message) | .addr][0] // empty' "$2" || true
}

# bound_call_lines prints, one JSON object per line, the lines the named broker logs hold for
# the airlock's proxy port: the call line of a call to a bound host the proxy carried or failed
# (`airlock proxy: a call to a bound host`), and every refusal record of that port (`broker:
# answer refused` with caller_class airlock). Those records are a refused call's to a bound
# host, which the proxy writes in its call line's place, and the port's other refusals too: a
# request that is no proxy request, a 407, a refused address. A line still being written parses
# as nothing and is left out.
bound_call_lines() { # <log>...
	jq -Rc 'fromjson? | select(.message == "airlock proxy: a call to a bound host" or (.message == "broker: answer refused" and .caller_class == "airlock"))' "$@" 2>/dev/null || true
}

# tunnel_lines is the proxy's line for each socket it opens to a host it does not read - a CONNECT
# to an unbound host, tunnelled end to end. k3's one tunnel is its call to T.
tunnel_lines() { # <log>...
	jq -Rc 'fromjson? | select(.message == "airlock proxy: a tunnel")' "$@" 2>/dev/null || true
}

# refusal_lines counts the proxy's refusal lines coded <code> in this stack's brokers' logs, to
# <host> alone when one is given.
refusal_lines() { # <code> [host]
	bound_call_lines "$AIRLOCK_DIR"/broker*.log |
		jq -s --arg c "$1" --arg h "${2:-}" '[.[] | select(.message == "broker: answer refused" and .code == $c and ($h == "" or .host == $h))] | length'
}

# stack_facts reads the few settings a second broker and the fake store need and that the
# banner does not name: the namespace prefix and key the brokers serve (their own /v1/settings
# route), and the resolve and dial-allow lists, which are the stack's bound hosts on their own
# loopback addresses.
stack_facts() {
	local settings
	settings="$(curl -sS "$AGENT_SECRETS_URL/v1/settings")"
	STACK_SECRETS_PREFIX="$(printf '%s' "$settings" | jq -r .secrets_prefix)"
	STACK_KMS_KEY_ARN="$(printf '%s' "$settings" | jq -r '.kms_key_arn // ""')"
	STACK_DIAL_ALLOW=""
	STACK_RESOLVE=""
	local host addr
	while read -r host addr; do
		[ -n "$host" ] || continue
		STACK_DIAL_ALLOW="${STACK_DIAL_ALLOW:+$STACK_DIAL_ALLOW }$host:443"
		STACK_RESOLVE="${STACK_RESOLVE:+$STACK_RESOLVE }$host=$addr"
	done <<-EOF
		s.airlock.test 127.0.0.2
		t.airlock.test 127.0.0.3
		microsoft.airlock.test 127.0.0.4
		azure-resource.airlock.test 127.0.0.4
		aws.airlock.test 127.0.0.5
		api.anthropic.com 127.0.0.6
	EOF
}

# --- Talking to the brokers --------------------------------------------------------------------

# release asks for a grant of one name and releases it as the CLI does, leaving the raw answer
# in RELEASE_JSON and the grant's placeholder, proxy password and id in three variables. The
# placeholder and the password never reach the output.
release() { # <name> <ttl>
	RELEASE_REQUEST="$(agent-secrets request "$1" --ttl "$2" --json 2>"$WORK_DIR/request.err")" || {
		echo "airlock-check: the request for $1 failed:" >&2
		cat "$WORK_DIR/request.err" >&2
		return 1
	}
	GRANT_ID="$(printf '%s' "$RELEASE_REQUEST" | jq -r '.grant_id // ""')"
	[ -n "$GRANT_ID" ] && [ "$GRANT_ID" != null ] || {
		echo "airlock-check: the request for $1 was not granted: $(printf '%s' "$RELEASE_REQUEST" | jq -r .state)" >&2
		return 1
	}
	release_values "$1"
}

# release_values releases GRANT_ID's values as the CLI does (POST /v1/grants/{id}/values, signed by
# the session), leaving the raw answer in RELEASE_JSON and <name>'s placeholder and the proxy
# password in two variables.
release_values() { # <name>
	local url proof
	url="$AGENT_SECRETS_URL/v1/grants/$GRANT_ID/values"
	proof="$(agent-secrets sign --method POST --url "$url")"
	RELEASE_JSON="$(curl -sS -X POST -H "Proof: $proof" "$url")"
	PLACEHOLDER="$(printf '%s' "$RELEASE_JSON" | jq -r --arg n "$1" '.airlock.names[$n].value // ""')"
	PROXY_PASSWORD="$(printf '%s' "$RELEASE_JSON" | jq -r '.airlock.proxy_password // ""')"
}

# release_approved is release for a human-tier name: the request waits for its person, so this
# approves it on the broker's page as the stack's person does (approve_record), waits for its
# grant, and releases it. A live grant of the name and lifetime is handed back with no request
# (the broker's reuse), and is released as it is. It answers 1, with RELEASE_JSON empty, when no
# grant came within a minute.
release_approved() { # <name> <ttl> <reason>
	RELEASE_JSON="" PLACEHOLDER="" PROXY_PASSWORD="" GRANT_ID=""
	# A pending request exits non-zero, which is no failure here: its state is read below.
	RELEASE_REQUEST="$(agent-secrets request "$1" --ttl "$2" --reason "$3" --json 2>"$WORK_DIR/request.err")"
	GRANT_ID="$(printf '%s' "$RELEASE_REQUEST" | jq -r '.grant_id // ""' 2>/dev/null)"
	if [ -z "$GRANT_ID" ]; then
		local request record i
		request="$(printf '%s' "$RELEASE_REQUEST" | jq -r '.request_id // ""' 2>/dev/null)"
		record="$(printf '%s' "$RELEASE_REQUEST" | jq -r '.approve_url // ""' 2>/dev/null | grep -o '[0-9a-f]\{64\}' | head -n 1)"
		[ -n "$request" ] && [ -n "$record" ] || {
			echo "airlock-check: the request for $1 is neither granted nor waiting for a person: $(cat "$WORK_DIR/request.err")" >&2
			return 1
		}
		approve_record "$record" >/dev/null 2>&1 || true
		for ((i = 0; i < 60; i++)); do
			GRANT_ID="$(agent-secrets status "$request" --json 2>/dev/null | jq -r '.grant_id // ""')"
			[ -n "$GRANT_ID" ] && break
			sleep 1
		done
	fi
	[ -n "$GRANT_ID" ] || {
		echo "airlock-check: the request for $1 was not granted within a minute of its approval" >&2
		return 1
	}
	release_values "$1"
}

# proxy_for is the proxy URL of this grant at one address, credential and all. It is never
# printed, and never a command's argument, which every process of the pod reads in
# /proc/<pid>/cmdline: dev-airlock-stands load reads it from --proxy-file and curl from the config
# proxy_config writes, each given as a <(…) path.
proxy_for() { printf 'http://agent-secrets:%s@%s' "$PROXY_PASSWORD" "$1"; }

# proxy_config is a curl config naming proxy_for's URL at one address as the proxy, for
# `curl -K <(proxy_config …)`. The password is a placeholder, of letters, digits and hyphens, so
# the quoted value needs no escape.
proxy_config() { printf 'proxy = "%s"\n' "$(proxy_for "$1")"; }

records() { curl -sS "$AIRLOCK_RECORDS_URL/records/$1"; }
records_count() { records "$1" | jq 'length'; }
# refusals_of is why each call stand-in <stand> refused did not verify, in order (GET <records
# URL>/refusals/<stand>), as one JSON array, and nothing at all when the route answered no array:
# a reader takes nothing as UNKNOWN, never as no refusal.
refusals_of() { curl -sS "$AIRLOCK_RECORDS_URL/refusals/$1" 2>/dev/null | jq -ce 'if type == "array" then . else empty end' 2>/dev/null || true; }

# reread_all makes every broker read one secret's name again, which is what the release of a
# new version of a stored value waits on. Each call is signed by this stack's session, one proof a
# broker (a proof is single-use), so each broker answers once its own reread is done; a call with
# no proof is answered at once, before the store is read, and the next check would race it. Every
# broker must answer 200: a signing failure or any other answer is a failed check (fail), and it
# returns 1 so the step stops rather than read a broker that did not reread.
reread_all() { # <name>
	local addr proof code
	for addr in "${BROKER_API_ADDRS[@]}"; do
		if ! proof="$(agent-secrets sign --method POST --url "$AGENT_SECRETS_URL/v1/secrets/$1/reread")"; then
			fail "the stack's session could not sign the reread of $1"
			return 1
		fi
		code="$(curl -sS -o /dev/null -w '%{http_code}' -X POST -H "Proof: $proof" "http://$addr/v1/secrets/$1/reread")"
		if [ "$code" != 200 ]; then
			fail "the broker at $addr answered the reread of $1 $code, not 200"
			return 1
		fi
	done
}

# add_fake_secret appends one secret to the stack's fake Secrets Manager and makes every broker
# read it. It is how a check gets a name the profiles file does not hold without writing a
# profiles file of its own: a plain stored value no profile names is served by its own name.
add_fake_secret() { # <store name> <tags json> <value>
	local tmp="$WORK_DIR/fake-secrets.json"
	jq --arg name "$1" --argjson tags "$2" --arg value "$3" --arg kms "$STACK_KMS_KEY_ARN" \
		'.secrets += [{name: $name, kms_key_id: $kms, tags: $tags, value: $value}]' \
		"$AIRLOCK_FAKE_SECRETS_FILE" >"$tmp"
	cat "$tmp" >"$AIRLOCK_FAKE_SECRETS_FILE"
	rm -f "$tmp"
}

# set_fake_value replaces one stored secret's value, which is the local stack's way of adding a
# new version and disabling the old: the fake store holds one value for each name.
set_fake_value() { # <store name> <value>
	local tmp="$WORK_DIR/fake-secrets.json"
	jq --arg name "$1" --arg value "$2" \
		'.secrets = [.secrets[] | if .name == $name then .value = $value else . end]' \
		"$AIRLOCK_FAKE_SECRETS_FILE" >"$tmp"
	cat "$tmp" >"$AIRLOCK_FAKE_SECRETS_FILE"
	rm -f "$tmp"
}

# set_fake_tags replaces one stored secret's tags, which is how a check moves a key's judge tag
# at run time; every broker reads it at its next reread of the name.
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

# header_of is one recorded request's header, and body_of its body: the two a check reads a
# result back from. The index is jq's, so -1 is the last record.
header_of() { records "$1" | jq -r --argjson i "$2" --arg h "$3" '.[$i].header[$h][0] // ""'; }
record_field() { records "$1" | jq -r --argjson i "$2" --arg f "$3" '.[$i][$f] // ""'; }

# github_revokes is how many revokes of an installation token GitHub's stand-in has received.
github_revokes() { records github | jq '[(. // [])[] | select(.method == "DELETE" and .path == "/installation/token")] | length'; }

psql_stack() { pg_url_run "$STACK_DB_URL" psql -Atq -v ON_ERROR_STOP=1 "$@"; }

###############################################################################################
# k1
###############################################################################################

# k1_python prints the interpreter python3 names, asked in this script's environment, for k1's
# scanner to run on by that path. The scanner starts agent-secrets by name, and the one that must
# answer is the stack's own build, first on this script's PATH once start_stack has evaluated the
# banner's `export PATH`. The first python3 on PATH can be a version manager's shim that starts the
# interpreter with a PATH of its own: mise 2026.10.11's python3 shim, in a mise-activated
# environment, put that environment's PATH in front of the entries put first, so the scanner would
# run an installed agent-secrets and not this checkout's.
k1_python() {
	local python
	python="$(python3 -I -c 'import sys; print(sys.executable)')" || return 1
	[[ "$python" == /* && -x "$python" ]] || {
		echo "airlock-check: python3 did not report an absolute executable path: $python" >&2
		return 1
	}
	printf '%s\n' "$python"
}

# k1_case runs one of k1's two commands under the scanner and judges the five places.
# bound is true when the name's profile has a proxy block, which is the check itself; false is
# its negative control, where the same run must find the value in env.txt and in the environment.
k1_case() { # <name> <label> <value> <bound true|false>
	local name="$1" label="$2" value="$3" bound="$4" python
	local dir="$WORK_DIR/k1-$label"
	mkdir -p "$dir"
	printf '%s' "$value" | od -An -tx1 -v | tr -d ' \n' >"$dir/needles"
	echo >>"$dir/needles"
	chmod 600 "$dir/needles"
	cat >"$dir/command.sh" <<EOF
printf 'Authorization: Bearer %s' "\$$name" | curl -fsS -H @- $AIRLOCK_S_URL/ok >curl.out
env > env.txt
: > ready
sleep 5
EOF
	python="$(k1_python)"
	"$python" "$SCRIPT_DIR/airlock/memscan.py" "$dir/ready" "$dir/needles" "$dir/scan.json" "$dir" \
		agent-secrets "$name" -- sh "$dir/command.sh" 2>"$dir/cli.err" || true
	K1_EXIT="$(jq -r '.exit' "$dir/scan.json")"
	K1_MEM="$(jq -r '.memory_hits[0]' "$dir/scan.json")"
	K1_ENVIRON="$(jq -r '.environ_hits[0]' "$dir/scan.json")"
	K1_REGIONS="$(jq -r '.regions' "$dir/scan.json")"
	K1_SCANNED_BYTES="$(jq -r '.bytes' "$dir/scan.json")"
	K1_SCAN_ERROR="$(jq -r '.scan_error' "$dir/scan.json")"
	K1_ENVIRON_BYTES="$(jq -r '.environ_bytes' "$dir/scan.json")"
	K1_FILES="$(grep -rlF "$value" "$dir" 2>/dev/null | grep -v '/needles$' | sed "s|$dir/||" | tr '\n' ' ')"
	# What each place searched held, which the check requires above zero: a search of nothing
	# finds nothing and proves nothing. env.txt is read with stat, not `wc -c <file`, because
	# the shell opens the redirect before 2>/dev/null takes effect and a missing env.txt - the
	# case where the CLI never ran the command - would print to this run's stderr and, under
	# pipefail, end the run with no result block at all.
	read -r K1_DIR_FILES K1_DIR_BYTES < <(find "$dir" -type f ! -name needles -printf '%s\n' 2>/dev/null | awk '{n++; t+=$1} END {print n+0, t+0}')
	K1_ENV_FILE_BYTES="$(stat -c %s "$dir/env.txt" 2>/dev/null || echo 0)"
	K1_HEADER="$(header_of s -1 Authorization)"
	K1_PATH="$(record_field s -1 path)"
	# S's record of this call, without the Authorization header the check reads on its own:
	# the value belongs in that header and nowhere else in the record. `// empty` leaves it
	# empty when S recorded nothing, where `// {}` would print two bytes and make the size
	# below a guard that cannot fail.
	K1_RECORD="$(records s | jq -c '.[-1] // empty | del(.header.Authorization)')"
	K1_RECORD_BYTES="${#K1_RECORD}"
	K1_RECORD_HITS=0
	if contains "$value" "$K1_RECORD"; then K1_RECORD_HITS=1; fi
	# The broker's own answer to the CLI for this name, read with a release of its own. The
	# answer is cleared first: `release` leaves RELEASE_JSON as it was when the request fails,
	# which would search the previous case's answer and call its size this case's coverage.
	# GitHub's revokes are counted first too, so check_k1 can wait on the one this release gives
	# back.
	K1_GITHUB_REVOKES="$(github_revokes)"
	RELEASE_JSON=""
	release "$name" 10m >/dev/null 2>&1 || true
	K1_ANSWER_BYTES="${#RELEASE_JSON}"
	K1_ANSWER_HITS=0
	if contains "$value" "$RELEASE_JSON"; then K1_ANSWER_HITS=1; fi
	K1_ENV_FILE_HITS="$(grep -cF "$value" "$dir/env.txt" 2>/dev/null || true)"
	[ -n "$K1_ENV_FILE_HITS" ] || K1_ENV_FILE_HITS=0
	if [ "$bound" = true ]; then
		observe "$label: the command exited $K1_EXIT; S's last record is $K1_PATH and its Authorization is $(shape "$K1_HEADER") (compared by SHA-256 and length)"
		observe "$label: the value is $(shape "$value"); scanned $K1_REGIONS readable regions ($K1_SCANNED_BYTES bytes) of the command's memory${K1_SCAN_ERROR:+ (scan error: $K1_SCAN_ERROR)}"
		observe "$label: hits for the value — memory $K1_MEM, /proc/<pid>/environ $K1_ENVIRON, the broker's answer $K1_ANSWER_HITS, S's record outside Authorization $K1_RECORD_HITS, env.txt $K1_ENV_FILE_HITS, files under the working directory: ${K1_FILES:-none}"
		observe "$label: what each place held — S's record $K1_RECORD_BYTES bytes outside its Authorization header, the broker's answer $K1_ANSWER_BYTES bytes, /proc/<pid>/environ $K1_ENVIRON_BYTES bytes, env.txt $K1_ENV_FILE_BYTES bytes, and $K1_DIR_FILES files of $K1_DIR_BYTES bytes under the working directory"
	else
		# No record of S's is read here: a name with no proxy block has no proxy, so the
		# command's call to a .test name no resolver answers never arrives, and S's last
		# record would be the previous case's.
		control "$label with no proxy block: the command exited $K1_EXIT; the value's hits are $K1_ENV_FILE_HITS in env.txt and $K1_ENVIRON in /proc/<pid>/environ, against $(shape "$value") (compared by SHA-256 and length)"
	fi
}

# k1_judge_places judges the case k1_case has just run: the value is in none of the places the
# check searches, and each of those places held something to find it in, so that a place the run
# never filled cannot read as a place the value is absent from. The two cases differ only in
# what the value is, which is this function's one argument.
#
# The region count and the file count are printed, not judged: each is above zero whenever the bytes are.
k1_judge_places() { # <what the value is>
	judge "$([ "$K1_SCANNED_BYTES" -gt 0 ] && [ "$K1_RECORD_BYTES" -gt 0 ] && [ "$K1_ANSWER_BYTES" -gt 0 ] && [ "$K1_ENVIRON_BYTES" -gt 0 ] && [ "$K1_ENV_FILE_BYTES" -gt 0 ] && [ "$K1_MEM" = 0 ] && [ "$K1_ENVIRON" = 0 ] && [ "$K1_ANSWER_HITS" = 0 ] && [ "$K1_RECORD_HITS" = 0 ] && [ -z "$K1_FILES" ] && echo 0 || echo 1)" \
		"$1 is in none of the five places, each of which held something to find it in" \
		"memory $K1_MEM hits over $K1_SCANNED_BYTES scanned bytes of $K1_REGIONS regions, environ $K1_ENVIRON of $K1_ENVIRON_BYTES bytes, the broker's answer $K1_ANSWER_HITS of $K1_ANSWER_BYTES bytes, S's record $K1_RECORD_HITS of $K1_RECORD_BYTES bytes outside its Authorization header, env.txt $K1_ENV_FILE_HITS of $K1_ENV_FILE_BYTES bytes, files ${K1_FILES:-none} over $K1_DIR_FILES files of $K1_DIR_BYTES bytes"
}

check_k1() {
	expect "S gets the stored secret's real value, and the github-app secret's minted token, in the Authorization header"
	expect "each command exits 0"
	expect "neither value is found in S's record aside: not in the broker's answer to the CLI, not in /proc/<pid>/environ, not in a scan of the command's readable memory, and not in any file under its working directory"
	expect "the negative control, the same runs for names no profile has a proxy block for, finds both in env.txt and in the command's environment"
	expect "the session's own release of the github-app secret gives back to GitHub the token it minted for the slots, and the token the proxy sent S is still live at GitHub afterwards"

	start_stack
	stack_facts

	local demo_value github_token
	demo_value="$(stored_value "${STACK_SECRETS_PREFIX}airlock-demo-token")"
	# The stand-in GitHub App mints ghs_standin_<owner>_<n>, a token of its own for every request
	# as GitHub mints one, and the stack configures acme-org (dev-airlock.sh's GITHUB_OWNER), so
	# every token it mints for this run begins with this string.
	github_token="ghs_standin_acme-org"

	k1_case AIRLOCK_DEMO_TOKEN stored "$demo_value" true
	judge "$([ "$K1_EXIT" = 0 ] && echo 0 || echo 1)" "the stored secret's command exits 0" "it exited $K1_EXIT"
	judge "$(contains "$demo_value" "$K1_HEADER" && echo 0 || echo 1)" \
		"S gets the stored secret's real value in the Authorization header of GET /ok" \
		"the header is $(shape "$K1_HEADER") and the stored value is $(shape "$demo_value"), compared by SHA-256 and length"
	k1_judge_places "the stored secret's value"

	k1_case AIRLOCK_GITHUB_APP github "$github_token" true
	judge "$([ "$K1_EXIT" = 0 ] && echo 0 || echo 1)" "the github-app command exits 0" "it exited $K1_EXIT"
	judge "$(contains "$github_token" "$K1_HEADER" && echo 0 || echo 1)" \
		"S gets the minted installation token in the Authorization header" \
		"the header is $(shape "$K1_HEADER") and the minted token is $(shape "$github_token"), compared by SHA-256 and length"
	k1_judge_places "the minted token"
	# The session's own release of the name, which k1_case's last step made, minted a token only
	# to learn the slots it hands placeholders for, and gives it back to GitHub in the background.
	# The token the proxy sent S stays live: a give-back that ended it, as a stand-in that answers
	# one string for every request would make it, would end the token the proxy holds for the
	# grant the session is still running.
	local proxy_token revokes_before revokes_after proxy_status
	# The profile's proxy block names no kind and its github block takes the default value,
	# installations, so the proxy sent S the whole released value, the JSON object of
	# installations; the token is its one installation's.
	proxy_token="$(printf '%s' "${K1_HEADER#Bearer }" | jq -Rr '(fromjson? | .installations[]?.token) // empty' | head -n 1)"
	revokes_before="$K1_GITHUB_REVOKES"
	revokes_after="$revokes_before"
	for _ in $(seq 30); do
		revokes_after="$(github_revokes)"
		[ "$revokes_after" -gt "$revokes_before" ] && break
		sleep 0.5
	done
	# The token reaches curl on standard input, in no command line.
	proxy_status="$(printf 'Authorization: Bearer %s' "$proxy_token" | curl -sS -o /dev/null -w '%{http_code}' -H @- "$AIRLOCK_GITHUB_URL/repos/acme-org/k1")"
	observe "github: GitHub got $((revokes_after - revokes_before)) revoke(s) after the session's own release, and a read at GitHub with the token the proxy sent S, $(shape "$proxy_token"), answered $proxy_status"
	judge "$([ -n "$proxy_token" ] && [ "$revokes_after" -gt "$revokes_before" ] && [ "$proxy_status" = 200 ] && echo 0 || echo 1)" \
		"the session's release gives back the token it minted for the slots, and the token the proxy sent S is still live at GitHub" \
		"$((revokes_after - revokes_before)) revoke(s) after the session's release; the proxy's token, $(shape "$proxy_token") (empty is a header no token was read from), answered $proxy_status, where 200 is live and anything else (401 a token GitHub no longer knows, 000 no answer) is not"

	# The negative control: the same two runs for names no profile has a proxy block for. A
	# plain stored value no profile names is served by its own name, so the control adds one to
	# the stack's fake store. A github-app secret no profile names is refused
	# (github-app-no-profile), and a broker reads its profiles only at boot, so the github-app
	# half is the stack's own AIRLOCK_PLAIN_GITHUB: a profile of the stack's github-app secret
	# with no proxy block, whose value, the JSON object of installations, is released to the
	# command.
	local plain_value
	plain_value="the-control-airlock-plain-token-$RANDOM$RANDOM"
	add_fake_secret "${STACK_SECRETS_PREFIX}airlock-plain-token" \
		'{"owner":"legion-daemon","tier":"agent"}' "$plain_value"
	reread_all AIRLOCK_PLAIN_TOKEN || return

	k1_case AIRLOCK_PLAIN_TOKEN plain-stored "$plain_value" false
	local plain_env="$K1_ENV_FILE_HITS" plain_environ="$K1_ENVIRON"
	k1_case AIRLOCK_PLAIN_GITHUB plain-github "$github_token" false
	judge "$([ "$plain_env" -gt 0 ] && [ "$plain_environ" -gt 0 ] && [ "$K1_ENV_FILE_HITS" -gt 0 ] && [ "$K1_ENVIRON" -gt 0 ] && echo 0 || echo 1)" \
		"negative control: with no proxy block both values are in env.txt and in the command's environment" \
		"stored: env.txt $plain_env, environ $plain_environ; github-app: env.txt $K1_ENV_FILE_HITS, environ $K1_ENVIRON"

	stop_stack
}

###############################################################################################
# k2
###############################################################################################

# The airlock's own merge, and the release whose agent-secrets the old-CLI steps of k2 and k7
# run: the last legion-envoy-v* release before that merge (legion-envoy-v29.40.0 is the merge's
# own release). Those steps ask what a CLI that knows nothing of the airlock does, so the release
# is pinned rather than read as "the newest": every release from v29.40.0 on carries the airlock,
# and a newest-release CLI answers the airlock's way and proves nothing.
AIRLOCK_MERGE_COMMIT="46c4162bc50a39852918cc016e3198944da1a5e7"
OLD_CLI_DEFAULT_TAG="legion-envoy-v29.39.0"

# fetch_old_cli puts that release's agent-secrets under the work directory, answers its path,
# and proves the two things the steps that run it rest on: that the binary is the release it
# asked for (its --version names the tag), and that the release is one GitHub puts before the
# airlock merge. It answers non-zero when either fails, and OLD_CLI_WHY says which, so a step
# that cannot run its old CLI fails rather than passing on a command that never ran.
# AIRLOCK_OLD_CLI_TAG names another release instead, and is checked the same way.
fetch_old_cli() {
	local tag asset status
	tag="${AIRLOCK_OLD_CLI_TAG:-$OLD_CLI_DEFAULT_TAG}"
	OLD_CLI_TAG="$tag"
	OLD_CLI_VERSION=""
	OLD_CLI_WHY=""
	mkdir -p "$WORK_DIR/old-cli"
	if ! gh release download "$tag" --repo cybermonklabs/legion --pattern 'agent-secrets-amd64.tar.gz' \
		--dir "$WORK_DIR/old-cli" --clobber >"$WORK_DIR/old-cli/download.log" 2>&1; then
		OLD_CLI_WHY="the release's agent-secrets-amd64.tar.gz did not download: $(tail -n 1 "$WORK_DIR/old-cli/download.log")"
		return 1
	fi
	asset="$WORK_DIR/old-cli/agent-secrets-amd64.tar.gz"
	tar -xzf "$asset" -C "$WORK_DIR/old-cli"
	# The release's tarball holds agent-secrets/bin/agent-secrets beside its helper and its
	# notices, so the binary is under that directory and is not the archive's own name.
	OLD_CLI="$WORK_DIR/old-cli/agent-secrets/bin/agent-secrets"
	chmod +x "$OLD_CLI"
	OLD_CLI_VERSION="$("$OLD_CLI" --version 2>&1 | tr -d '\n')"
	contains "$tag" "$OLD_CLI_VERSION" || {
		OLD_CLI_WHY="its --version prints '$OLD_CLI_VERSION', which does not name $tag"
		return 1
	}
	# GitHub's compare of the tag with the airlock merge: `ahead` means the merge is ahead of
	# the tag, so the tag's commit is an ancestor of it and that release has no airlock in it.
	status="$(gh api "repos/cybermonklabs/legion/compare/$tag...$AIRLOCK_MERGE_COMMIT" --jq .status 2>&1 | tr -d '\n')"
	[ "$status" = ahead ] || {
		OLD_CLI_WHY="GitHub compares $tag with the airlock merge ${AIRLOCK_MERGE_COMMIT:0:12} as '$status', not 'ahead', so that release is not one from before the airlock"
		return 1
	}
}

check_k2() {
	expect "POST /v1/grants/{id}/values for a grant of a name whose profile has a proxy block answers the placeholder and the proxy's certificate in their new fields and leaves the name out of values"
	expect "the agent-secrets of the release before the airlock exits without running the command and says that no value was released"
	expect "the negative control, the same call for a grant of a stored secret no profile names, answers the value under the name, and the old CLI runs its command"

	start_stack
	stack_facts
	local old_cli_ok=0
	fetch_old_cli || old_cli_ok=1
	observe "the agent-secrets of the release before the airlock is $OLD_CLI_TAG and prints: ${OLD_CLI_VERSION:-nothing}"
	judge "$old_cli_ok" \
		"the old CLI is the release it was asked for, and GitHub puts that release before the airlock" \
		"${OLD_CLI_WHY:-its --version prints $OLD_CLI_VERSION, and GitHub compares $OLD_CLI_TAG with the airlock merge as ahead}"

	release AIRLOCK_DEMO_TOKEN 30m
	local values_keys airlock_keys ca_len name_value demo_value
	values_keys="$(printf '%s' "$RELEASE_JSON" | jq -c '.values // {} | keys')"
	airlock_keys="$(printf '%s' "$RELEASE_JSON" | jq -c '.airlock | keys')"
	ca_len="$(printf '%s' "$RELEASE_JSON" | jq -r '.airlock.ca_certificate | length')"
	name_value="$(printf '%s' "$RELEASE_JSON" | jq -r '.airlock.names.AIRLOCK_DEMO_TOKEN.value // ""')"
	demo_value="$(stored_value "${STACK_SECRETS_PREFIX}airlock-demo-token")"
	observe "the answer's values keys are $values_keys and its airlock keys are $airlock_keys"
	observe "airlock.names.AIRLOCK_DEMO_TOKEN.value is $(shape "$name_value") and the stored value is $(shape "$demo_value") (compared by SHA-256 and length: a different value, which is what a placeholder is)"
	observe "airlock.ca_certificate is $ca_len bytes and begins $(head -c 27 <<<"$(printf '%s' "$RELEASE_JSON" | jq -r .airlock.ca_certificate)")"

	judge "$([ "$values_keys" = "[]" ] && echo 0 || echo 1)" \
		"the answer leaves the name out of values" "values keys are $values_keys"
	judge "$([ -n "$name_value" ] && contains "agent-secrets-placeholder" "$name_value" && ! contains "$demo_value" "$RELEASE_JSON" && echo 0 || echo 1)" \
		"the answer carries the placeholder in its new field and no real value anywhere" \
		"airlock.names.AIRLOCK_DEMO_TOKEN.value is $(shape "$name_value"); the stored value's bytes are not in the answer"
	judge "$(printf '%s' "$RELEASE_JSON" | jq -e '.airlock.ca_certificate | startswith("-----BEGIN CERTIFICATE-----")' >/dev/null && echo 0 || echo 1)" \
		"the answer carries the proxy's certificate in its new field" "airlock.ca_certificate is a $ca_len byte PEM certificate"

	local old_out old_status
	old_out="$("$OLD_CLI" AIRLOCK_DEMO_TOKEN -- sh -c 'echo THE-COMMAND-RAN' 2>&1)"
	old_status=$?
	observe "the old CLI for AIRLOCK_DEMO_TOKEN exited $old_status and said: $(printf '%s' "$old_out" | tail -n 2 | tr '\n' ' ')"
	judge "$([ "$old_status" -ne 0 ] && ! contains "THE-COMMAND-RAN" "$old_out" && echo 0 || echo 1)" \
		"the old CLI exits without running the command" "exit $old_status, and its output does not hold the command's own line"
	judge "$(contains "no value" "$old_out" && echo 0 || echo 1)" \
		"the old CLI says that no value was released" "it said: $(printf '%s' "$old_out" | tail -n 1)"

	# The negative control: a stored secret no profile names, which is served by its own name.
	local plain_value
	plain_value="the-control-airlock-plain-token-$RANDOM$RANDOM"
	add_fake_secret "${STACK_SECRETS_PREFIX}airlock-plain-token" \
		'{"owner":"legion-daemon","tier":"agent"}' "$plain_value"
	reread_all AIRLOCK_PLAIN_TOKEN || return
	release AIRLOCK_PLAIN_TOKEN 30m
	local plain_values plain_airlock released
	plain_values="$(printf '%s' "$RELEASE_JSON" | jq -c '.values // {} | keys')"
	plain_airlock="$(printf '%s' "$RELEASE_JSON" | jq -r '.airlock // "null"')"
	released="$(printf '%s' "$RELEASE_JSON" | jq -r '.values.AIRLOCK_PLAIN_TOKEN // ""')"
	control "for a stored secret no profile names the answer's values keys are $plain_values, its airlock field is ${plain_airlock:0:4}, and values.AIRLOCK_PLAIN_TOKEN is $(shape "$released") against the stored $(shape "$plain_value") (compared by SHA-256 and length)"
	old_out="$("$OLD_CLI" AIRLOCK_PLAIN_TOKEN -- sh -c 'echo THE-COMMAND-RAN' 2>&1)"
	old_status=$?
	control "the old CLI for that name exited $old_status and its output holds the command's own line: $(contains "THE-COMMAND-RAN" "$old_out" && echo yes || echo no)"
	judge "$([ "$released" = "$plain_value" ] && contains "THE-COMMAND-RAN" "$old_out" && echo 0 || echo 1)" \
		"negative control: the answer carries the value under the name and the old CLI runs its command" \
		"values.AIRLOCK_PLAIN_TOKEN matches the stored value by SHA-256 and length, and the old CLI exited $old_status having run the command"

	stop_stack
}

###############################################################################################
# k3
###############################################################################################

check_k3() {
	expect "S gets the real value only in Authorization and X-Key of GET /read"
	expect "X-Other and the query are refused 403 PLACEHOLDER_NOT_SWAPPED, and S records neither; the body reaches S with the placeholder"
	expect "a GET of /read carrying a minted placeholder's form in X-Other is refused 403 PLACEHOLDER_NOT_SWAPPED, and S records it not"
	expect "POST /read and GET /other get an error that names the rule, and S records neither"
	expect "T records the placeholder"
	expect "the request whose TLS name and Host name T is refused with an error that names the mismatch, and neither S nor T records a value"
	expect "the request that already carries the value is refused with an error that names it, and S records nothing"
	expect "the broker's log has a line for each call to S, allowed or refused with its reason"
	expect "the negative control, with the proxy block's allow removed, lets the POST and the GET of /other reach S with the real value in Authorization"

	start_stack
	stack_facts
	local scoped_value s_before t_before log_before minted
	# A string of a minted placeholder's form: the prefix, a minted kind's word and a hyphen, then
	# 32 letters and digits (placeholder.MintedPrefix). No grant holds it; what it tests is that
	# the leftover scan reads that form, as it reads a release's. It is written in two parts so no
	# file holds it whole.
	minted="agent-secrets-placeholder-""results-k3mintedk3mintedk3mintedk3minted"
	scoped_value="$(stored_value "${STACK_SECRETS_PREFIX}airlock-scoped-token")"
	s_before="$(records_count s)"
	t_before="$(records_count t)"
	log_before="$(bound_call_lines "$AIRLOCK_DIR"/broker*.log | wc -l)"
	tunnel_before="$(tunnel_lines "$AIRLOCK_DIR"/broker*.log | wc -l)"
	# T is reached end-to-end through the CONNECT tunnel the proxy opens to it, and T presents
	# its own certificate, which the stands' authority signs (PROXY_STANDS_CA_CERT_FILE). The
	# command verifies T against its own trust bundle, the system ca-bundle agent-secrets points
	# it at, which holds no stands' authority; AIRLOCK_CA_CERT_FILE, the broker-client bundle
	# dev-airlock.sh writes, holds both the proxy root and the stands' authority, but the command
	# does not read it. So the T call names the stands' authority itself. The proxy front is plain
	# http, so the CONNECT uses no proxy TLS root and --cacert verifies T's leaf alone: the
	# stands' authority is the one root that should sign T, and trusting any other - the proxy's
	# own among them - would let a proxy that MITM'd T pass the one check that exists to catch it.
	# printf %q so a TMPDIR path with a space in it stays one argument in the generated script.
	local stands_ca
	stands_ca="$(printf %q "$PROXY_STANDS_CA_CERT_FILE")"
	cat >"$WORK_DIR/k3-steps.sh" <<EOF
set -u
P="\$AIRLOCK_SCOPED_TOKEN"
out="$WORK_DIR/k3-out"
: >"\$out"
say() { printf '%s\n' "\$1" >>"\$out"; }
say "authorization=\$(printf 'Authorization: Bearer %s' "\$P" | curl -sS -o /dev/null -w %{http_code} -H @- $AIRLOCK_S_URL/read)"
say "xkey=\$(curl -sS -o /dev/null -w %{http_code} -H "X-Key: \$P" $AIRLOCK_S_URL/read)"
say "xother=\$(curl -sS -o /dev/null -w %{http_code} -H "X-Other: \$P" $AIRLOCK_S_URL/read)"
say "xother_error=\$(curl -sS -H "X-Other: \$P" $AIRLOCK_S_URL/read 2>&1 | tr '\n' ' ')"
say "query=\$(curl -sS -o /dev/null -w %{http_code} "$AIRLOCK_S_URL/read?k=\$P")"
say "query_error=\$(curl -sS "$AIRLOCK_S_URL/read?k=\$P" 2>&1 | tr '\n' ' ')"
say "minted=\$(curl -sS -o $WORK_DIR/k3-minted-answer -w %{http_code} -H "X-Other: $minted" $AIRLOCK_S_URL/read)"
say "minted_error=\$(tr '\n' ' ' <$WORK_DIR/k3-minted-answer)"
say "getbody=\$(curl -sS -o /dev/null -w %{http_code} -X GET --data "k=\$P" $AIRLOCK_S_URL/read)"
say "postbody=\$(curl -sS -o /dev/null -w %{http_code} -X POST --data "k=\$P" $AIRLOCK_S_URL/read)"
say "postbody_error=\$(curl -sS -X POST --data "k=\$P" $AIRLOCK_S_URL/read 2>&1 | tr '\n' ' ')"
say "post=\$(printf 'Authorization: Bearer %s' "\$P" | curl -sS -o /dev/null -w %{http_code} -X POST -H @- $AIRLOCK_S_URL/read)"
say "post_error=\$(printf 'Authorization: Bearer %s' "\$P" | curl -sS -X POST -H @- $AIRLOCK_S_URL/read 2>&1 | tr '\n' ' ')"
say "other=\$(printf 'Authorization: Bearer %s' "\$P" | curl -sS -o /dev/null -w %{http_code} -H @- $AIRLOCK_S_URL/other)"
say "other_error=\$(printf 'Authorization: Bearer %s' "\$P" | curl -sS -H @- $AIRLOCK_S_URL/other 2>&1 | tr '\n' ' ')"
say "t=\$(printf 'Authorization: Bearer %s' "\$P" | curl -sS --cacert $stands_ca -o /dev/null -w %{http_code} -H @- $AIRLOCK_T_URL/read)"
say "mismatch_sni=\$(printf 'Authorization: Bearer %s' "\$P" | curl -sS --connect-to t.airlock.test:443:s.airlock.test:443 -H @- $AIRLOCK_T_URL/read 2>&1 | tr '\n' ' ')"
say "mismatch_host=\$(printf 'Authorization: Bearer %s' "\$P" | curl -sS -H "Host: t.airlock.test" -H @- $AIRLOCK_S_URL/read 2>&1 | tr '\n' ' ')"
say "carries=\$(curl -sS -H "X-Key: \$(cat $WORK_DIR/k3-real)" $AIRLOCK_S_URL/read 2>&1 | tr '\n' ' ')"
EOF
	printf '%s' "$scoped_value" >"$WORK_DIR/k3-real"
	chmod 600 "$WORK_DIR/k3-real"
	agent-secrets AIRLOCK_SCOPED_TOKEN --reason "airlock check k3: the scoped placeholder in each header, the query and the body of calls to S" -- \
		bash "$WORK_DIR/k3-steps.sh" >"$WORK_DIR/k3-cli.log" 2>&1 || true
	rm -f "$WORK_DIR/k3-real"
	declare -A K3
	while IFS='=' read -r key value; do K3["$key"]="$value"; done <"$WORK_DIR/k3-out"

	local recs
	recs="$(records s | jq --argjson n "$s_before" '.[$n:]')"
	local auth_hits key_hits other_hits query_hits body_hits post_records other_records
	auth_hits="$(printf '%s' "$recs" | jq --arg v "$scoped_value" '[.[] | select(.path == "/read" and .method == "GET") | .header.Authorization // [] | .[] | select(contains($v))] | length')"
	key_hits="$(printf '%s' "$recs" | jq --arg v "$scoped_value" '[.[] | select(.path == "/read" and .method == "GET") | .header["X-Key"] // [] | .[] | select(contains($v))] | length')"
	other_hits="$(printf '%s' "$recs" | jq --arg v "$scoped_value" '[.[] | .header["X-Other"] // [] | .[] | select(contains($v))] | length')"
	query_hits="$(printf '%s' "$recs" | jq --arg v "$scoped_value" '[.[] | select(.query | contains($v))] | length')"
	body_hits="$(printf '%s' "$recs" | jq --arg v "$scoped_value" '[.[] | select(.body | contains($v))] | length')"
	post_records="$(printf '%s' "$recs" | jq '[.[] | select(.method == "POST")] | length')"
	other_records="$(printf '%s' "$recs" | jq '[.[] | select(.path == "/other")] | length')"
	local xother_records query_records body_ph minted_records
	xother_records="$(printf '%s' "$recs" | jq '[.[] | select(.header["X-Other"] != null)] | length')"
	minted_records="$(printf '%s' "$recs" | jq --arg m "$minted" '[.[] | .header["X-Other"] // [] | .[] | select(contains($m))] | length')"
	query_records="$(printf '%s' "$recs" | jq '[.[] | select(.query != "")] | length')"
	body_ph="$(printf '%s' "$recs" | jq -r '[.[] | select(.body != "") | .body] | .[0] // ""')"
	local t_recs t_ph
	t_recs="$(records t | jq --argjson n "$t_before" '.[$n:]')"
	t_ph="$(printf '%s' "$t_recs" | jq -r '[.[] | .header.Authorization // [] | .[]] | .[0] // ""')"

	observe "status by step: Authorization ${K3[authorization]}, X-Key ${K3[xkey]}, X-Other ${K3[xother]}, query ${K3[query]}, X-Other with a minted placeholder's form ${K3[minted]}, GET /read with a body ${K3[getbody]}, POST /read with a body ${K3[postbody]}, POST /read ${K3[post]}, GET /other ${K3[other]}, T ${K3[t]}"
	observe "S recorded $(printf '%s' "$recs" | jq length) requests: the real value (by SHA-256 and length, $(shape "$scoped_value")) in $auth_hits Authorization and $key_hits X-Key of GET /read, and in $other_hits X-Other, $query_hits queries and $body_hits bodies"
	observe "S recorded $xother_records requests with an X-Other header and $query_records with a query; the body carried $(shape "$body_ph") — the placeholder, not the value"
	observe "S recorded $post_records POST and $other_records /other requests"
	observe "how the body step is read: the proxy block's allow is GET /read alone, and the same check requires POST /read to be refused, so the body carrying the placeholder is read off the GET of /read that carries one; the POST of /read with a placeholder body is refused with the other POST"
	observe "how the mismatch step is read: the proxy answers the CONNECT to S with a leaf for S, so a client whose TLS name is T fails the handshake and nothing is sent; the same request with the Host header alone changed reaches the proxy and is refused 'airlock: host header mismatch'"
	observe "the refusals: X-Other said ${K3[xother_error]}; the query said ${K3[query_error]}; POST /read said ${K3[post_error]}; GET /other said ${K3[other_error]}; the TLS-name and Host mismatch said ${K3[mismatch_sni]}; a Host-only mismatch said ${K3[mismatch_host]}; a request already carrying the value said ${K3[carries]}"
	observe "T recorded $(printf '%s' "$t_recs" | jq length) requests and its Authorization was $(shape "$t_ph")"

	judge "$([ "$auth_hits" = 1 ] && [ "$key_hits" = 1 ] && [ "$other_hits" = 0 ] && [ "$query_hits" = 0 ] && [ "$body_hits" = 0 ] && echo 0 || echo 1)" \
		"S gets the real value only in Authorization and X-Key of GET /read" \
		"Authorization $auth_hits, X-Key $key_hits, X-Other $other_hits, query $query_hits, body $body_hits (compared by SHA-256 and length)"
	# The two refusals' code is on their refusal lines (the answer's words name the rule, and the
	# code beside it is the broker's own record of it).
	local left_lines
	left_lines="$(refusal_lines PLACEHOLDER_NOT_SWAPPED)"
	judge "$([ "${K3[xother]}" = 403 ] && [ "${K3[query]}" = 403 ] && contains "still carries a placeholder after the swap" "${K3[xother_error]}" && contains "still carries a placeholder after the swap" "${K3[query_error]}" && [ "$left_lines" -ge 4 ] && [ "$xother_records" = 0 ] && [ "$query_records" = 0 ] && contains "agent-secrets-placeholder" "$body_ph" && echo 0 || echo 1)" \
		"X-Other and the query are refused 403 PLACEHOLDER_NOT_SWAPPED, and S records neither; the body reaches S with the placeholder" \
		"X-Other answered ${K3[xother]} saying '${K3[xother_error]}' and the query ${K3[query]} saying '${K3[query_error]}' (want 403, each naming 'still carries a placeholder after the swap'); $left_lines refusal lines coded PLACEHOLDER_NOT_SWAPPED for their four requests; S recorded $xother_records with X-Other and $query_records with a query; the body carried $(shape "$body_ph")"
	# The minted form is read as a placeholder too: in a build whose scan reads a release's form
	# alone (the proxy mutant proxy-leftover-stored-form-only) it reaches S, and this fails. It is
	# one request, whose answer is kept to read its words from.
	judge "$([ "${K3[minted]}" = 403 ] && contains "still carries a placeholder after the swap" "${K3[minted_error]}" && [ "$minted_records" = 0 ] && [ "$left_lines" -ge 5 ] && echo 0 || echo 1)" \
		"a GET of /read carrying a minted placeholder's form in X-Other is refused 403 PLACEHOLDER_NOT_SWAPPED, and S records it not" \
		"it answered ${K3[minted]}, saying ${K3[minted_error]}; S recorded $minted_records requests carrying it, and the brokers logged $left_lines refusal lines coded PLACEHOLDER_NOT_SWAPPED for the five requests that carry a placeholder left after the swap"
	judge "$(contains "not allowed by rule" "${K3[post_error]}" && contains "not allowed by rule" "${K3[postbody_error]}" && contains "not allowed by rule" "${K3[other_error]}" && [ "$post_records" = 0 ] && [ "$other_records" = 0 ] && echo 0 || echo 1)" \
		"POST /read with the placeholder in its header and in its body, and GET /other, get an error that names the rule, and S records neither" \
		"the header POST answered ${K3[post]} saying '${K3[post_error]}', the body POST ${K3[postbody]} saying '${K3[postbody_error]}' and GET /other ${K3[other]} saying '${K3[other_error]}'; S recorded $post_records POST and $other_records /other"
	judge "$(contains "agent-secrets-placeholder" "$t_ph" && [ "$(printf '%s' "$t_recs" | jq length)" = 1 ] && echo 0 || echo 1)" \
		"T records the placeholder, and only the one call to T reaches it" \
		"T recorded $(printf '%s' "$t_recs" | jq length) request(s) (want 1); its Authorization was $(shape "$t_ph") (want the grant's placeholder, which opens agent-secrets-placeholder); the two mismatch requests reach T not"
	judge "$( { contains "t.airlock.test" "${K3[mismatch_sni]}" || contains "mismatch" "${K3[mismatch_sni]}"; } && contains "host header mismatch" "${K3[mismatch_host]}" && [ "$(printf '%s' "$recs" | jq --arg v "$scoped_value" '[.[] | select(.host == "t.airlock.test")] | length')" = 0 ] && echo 0 || echo 1)" \
		"the request whose TLS name and Host name T fails the handshake, the Host-only mismatch is refused 'host header mismatch', and neither S nor T records a value for T" \
		"the TLS-name mismatch answered: ${K3[mismatch_sni]}; the Host-only mismatch answered: ${K3[mismatch_host]} (want a refusal naming 'host header mismatch')"
	judge "$(contains "already carries the value" "${K3[carries]}" && echo 0 || echo 1)" \
		"the request that already carries the value is refused with an error that names it, and S records nothing" \
		"the request that already carries the value answered: ${K3[carries]} (want a refusal that names it)"

	# The 17 requests this command makes leave exactly 17 log rows by request: 16 reach the proxy
	# on a CONNECT to a bound host and log a line (bound_call_lines - a carried call logs its call
	# line, a refused one its refusal record in that line's place), the call to T logs one tunnel
	# line (it is no bound host, so it is tunnelled unread), and the TLS-name mismatch logs none,
	# its handshake failing before a request exists. The counts are exact, not a lower bound: a
	# broker that wrote no tunnel line, wrote one request twice, or logged the handshake is caught.
	# Each number comes from one read of the logs, so a line written between two reads cannot make
	# them disagree.
	local calls log_after tunnel_after log_lines allowed refused
	calls="$(bound_call_lines "$AIRLOCK_DIR"/broker*.log)"
	log_after="$(jq -s length <<<"$calls")"
	tunnel_after="$(tunnel_lines "$AIRLOCK_DIR"/broker*.log | wc -l)"
	log_lines="$(printf '%s' "$calls" | tail -n 3 | sed 's/^/    /')"
	allowed="$(jq -s '[.[] | select(.message == "airlock proxy: a call to a bound host" and .allowed == true)] | length' <<<"$calls")"
	refused="$(jq -s '[.[] | select(.message == "broker: answer refused" and (.reason // "") != "")] | length' <<<"$calls")"
	observe "the brokers logged $((log_after - log_before)) bound-call lines and $((tunnel_after - tunnel_before)) tunnel line this run, $allowed of the bound-call lines allowed and $refused refused with a reason; the last three bound-call lines:"
	observe "$log_lines"
	judge "$([ "$((log_after - log_before))" -eq 16 ] && [ "$((tunnel_after - tunnel_before))" -eq 1 ] && [ "$allowed" -gt 0 ] && [ "$refused" -gt 0 ] && echo 0 || echo 1)" \
		"the broker's log has exactly 17 rows by request: one bound-call line for each of the 16 calls to S, one tunnel line for the call to T, and none for the handshake that failed" \
		"$((log_after - log_before)) bound-call lines for the 16 requests that reached the proxy (want 16), $((tunnel_after - tunnel_before)) tunnel line for T (want 1), $allowed allowed and $refused refused with the reason their refusal record names"

	# The negative control. The stack's AIRLOCK_DEMO_TOKEN profile has the scoped profile's proxy
	# block with its allow removed: the same host and the authorization header, and no allow list.
	local demo_value s_mark
	demo_value="$(stored_value "${STACK_SECRETS_PREFIX}airlock-demo-token")"
	s_mark="$(records_count s)"
	cat >"$WORK_DIR/k3-control.sh" <<EOF
set -u
out="$WORK_DIR/k3-control-out"
: >"\$out"
printf 'post=%s\n' "\$(printf 'Authorization: Bearer %s' "\$AIRLOCK_DEMO_TOKEN" | curl -sS -o /dev/null -w %{http_code} -X POST -H @- $AIRLOCK_S_URL/read)" >>"\$out"
printf 'other=%s\n' "\$(printf 'Authorization: Bearer %s' "\$AIRLOCK_DEMO_TOKEN" | curl -sS -o /dev/null -w %{http_code} -H @- $AIRLOCK_S_URL/other)" >>"\$out"
EOF
	agent-secrets AIRLOCK_DEMO_TOKEN --reason "airlock check k3 control: the demo placeholder on POST /read and GET /other, with no allow" -- \
		bash "$WORK_DIR/k3-control.sh" >>"$WORK_DIR/k3-cli.log" 2>&1 || true
	local crecs cpost cother
	crecs="$(records s | jq --argjson n "$s_mark" '.[$n:]')"
	cpost="$(printf '%s' "$crecs" | jq --arg v "$demo_value" '[.[] | select(.method == "POST" and .path == "/read") | .header.Authorization // [] | .[] | select(contains($v))] | length')"
	cother="$(printf '%s' "$crecs" | jq --arg v "$demo_value" '[.[] | select(.path == "/other") | .header.Authorization // [] | .[] | select(contains($v))] | length')"
	control "with a proxy block that has no allow (the stack's AIRLOCK_DEMO_TOKEN, the same host and header as the scoped profile's), $(cat "$WORK_DIR/k3-control-out" | tr '\n' ' ')"
	control "S recorded the real value (by SHA-256 and length) in $cpost POST /read and $cother GET /other"
	judge "$([ "$cpost" = 1 ] && [ "$cother" = 1 ] && echo 0 || echo 1)" \
		"negative control: with the proxy block's allow removed, the POST and the GET of /other reach S with the real value in Authorization" \
		"POST /read $cpost and GET /other $cother records carried the stored value in Authorization by SHA-256 and length (want 1 each)"

	stop_stack
}

###############################################################################################
# k4
###############################################################################################

check_k4() {
	expect "the call to S goes through the proxy port and S's certificate comes from the proxy's authority"
	expect "T presents its own certificate and gets the bytes the command sent"
	expect "the .svc and .ts.net calls and the 10.x calls of curl and of a Go program do not reach the proxy port"
	expect "Node's 10.x call reaches the proxy and is refused with an error that names the private address"
	expect "the connections to 169.254.169.254 and the broker's own loopback port are refused with an error that names the rule"
	expect "both bad proxy passwords, the call after the grant ends and the call after the withholding get 407, and nothing is tunnelled"
	expect "the negative control, with the proxy's refusal of loopback and link-local addresses turned off for it, tunnels the connection to the broker's loopback port"

	start_stack
	stack_facts

	# A stand-in on a .svc name of its own, which the stack does not have. It is this run's
	# own: one Python HTTP server on loopback, reached by the .svc name with curl --resolve,
	# because a Legion pod's /etc/hosts cannot be written.
	local svc_port
	svc_port="$(python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')"
	mkdir -p "$WORK_DIR/svc"
	echo "the .svc stand-in answered" >"$WORK_DIR/svc/index.html"
	(cd "$WORK_DIR/svc" && exec python3 -m http.server "$svc_port" --bind 127.0.0.1 >"$WORK_DIR/svc.log" 2>&1) &
	HELPER_PIDS+=($!)

	local s_before t_before
	s_before="$(records_count s)"
	t_before="$(records_count t)"
	# printf %q so a TMPDIR path with a space in it stays one argument in the generated script.
	local stands_ca
	stands_ca="$(printf %q "$PROXY_STANDS_CA_CERT_FILE")"
	cat >"$WORK_DIR/k4-steps.sh" <<EOF
set -u
out="$WORK_DIR/k4-out"
: >"\$out"
say() { printf '%s\n' "\$1" >>"\$out"; }
say "no_proxy=\$NO_PROXY"
printf 'Authorization: Bearer %s' "\$AIRLOCK_DEMO_TOKEN" | curl -sS -o /dev/null -w "%{certs}" -H @- $AIRLOCK_S_URL/ok >"$WORK_DIR/s-leaf.pem"
say "s_status=\$(printf 'Authorization: Bearer %s' "\$AIRLOCK_DEMO_TOKEN" | curl -sS -o /dev/null -w %{http_code} -H @- $AIRLOCK_S_URL/ok)"
curl -sS --cacert $stands_ca -o /dev/null -w "%{certs}" $AIRLOCK_T_URL/read >"$WORK_DIR/t-leaf.pem"
say "t_body=\$(curl -sS --cacert $stands_ca -X POST --data 'the-bytes-the-command-sent' $AIRLOCK_T_URL/echo | tr '\n' ' ')"
say "svc=\$(curl -sS --resolve stand.default.svc:$svc_port:127.0.0.1 --max-time 10 http://stand.default.svc:$svc_port/ 2>&1 | tr '\n' ' ')"
say "svc_exit=\$?"
say "tsnet=\$(curl -sS --max-time 10 https://host.example-tailnet.ts.net/ 2>&1 | tr '\n' ' ')"
say "tsnet_exit=\$?"
say "curl10=\$(curl -sS --max-time 10 https://10.11.12.13/ 2>&1 | tr '\n' ' ')"
say "curl10_exit=\$?"
say "go10=\$($STANDS_BIN probe-private 2>&1 | tr '\n' ' ')"
node -e '
const t = Date.now();
fetch("https://10.11.12.13/").then(r => console.log("status", r.status, Date.now() - t))
  .catch(e => { const p = []; let c = e.cause; while (c) { p.push((c.code || "") + ":" + (c.message || "")); c = c.cause; } console.log("ms", Date.now() - t, "|", e.message, "|", p.join(" <- ")); });
' >"$WORK_DIR/node10.txt" 2>&1
say "node10=\$(tr '\n' ' ' <"$WORK_DIR/node10.txt")"
say "private=\$(curl -sS --noproxy '' -K <(printf 'proxy = "%s"\n' "\$HTTPS_PROXY") http://10.11.12.13/ 2>&1 | tr '\n' ' ')"
say "metadata=\$(curl -sS --noproxy '' -K <(printf 'proxy = "%s"\n' "\$HTTPS_PROXY") http://169.254.169.254/ 2>&1 | tr '\n' ' ')"
say "broker_port=\$(curl -sS --noproxy '' -K <(printf 'proxy = "%s"\n' "\$HTTPS_PROXY") http://${BROKER_API_ADDRS[0]}/healthz 2>&1 | tr '\n' ' ')"
say "broker_connect=\$(curl -sS --noproxy '' -K <(printf 'proxy = "%s"\n' "\$HTTPS_PROXY") https://${BROKER_API_ADDRS[0]}/healthz 2>&1 | tr '\n' ' ')"
say "no_password=\$(curl -sS --noproxy '' --proxy "http://\${HTTPS_PROXY##*@}" $AIRLOCK_S_URL/ok 2>&1 | tr '\n' ' ')"
say "made_up=\$(curl -sS --noproxy '' -K <(printf 'proxy = "%s-made-up@%s"\n' "\${HTTPS_PROXY%@*}" "\${HTTPS_PROXY##*@}") $AIRLOCK_S_URL/ok 2>&1 | tr '\n' ' ')"
EOF
	agent-secrets AIRLOCK_DEMO_TOKEN -- bash "$WORK_DIR/k4-steps.sh" >"$WORK_DIR/k4-cli.log" 2>&1 || true
	declare -A K4
	while IFS='=' read -r key value; do K4["$key"]="$value"; done <"$WORK_DIR/k4-out"

	local s_leaf t_leaf s_own t_own s_issuer s_chain_ok s_not_stands s_chain_verify
	s_leaf="$(openssl x509 -in "$WORK_DIR/s-leaf.pem" -noout -fingerprint -sha256 | sed 's/.*=//')"
	t_leaf="$(openssl x509 -in "$WORK_DIR/t-leaf.pem" -noout -fingerprint -sha256 | sed 's/.*=//')"
	s_own="$(openssl x509 -in "$AIRLOCK_DIR/stands/s.crt" -noout -fingerprint -sha256 | sed 's/.*=//')"
	t_own="$(openssl x509 -in "$AIRLOCK_DIR/stands/t.crt" -noout -fingerprint -sha256 | sed 's/.*=//')"
	s_issuer="$(openssl x509 -in "$WORK_DIR/s-leaf.pem" -noout -issuer)"
	# Build the chain, do not compare a string: S's leaf must chain through the broker's
	# intermediate to the proxy test root (PROXY_ROOT_FILE). An issuer-string check cannot tell a
	# leaf that chains to the root from one that does not; openssl verify does. The control is the
	# negative - the same served chain must not verify against the stands' authority
	# (PROXY_STANDS_CA_CERT_FILE), which signs a stand's own certificate - so a leaf chaining
	# elsewhere is caught. %{certs} writes the chain with metadata between the PEM blocks, so the
	# blocks are pulled out first.
	sed -n '/-----BEGIN CERTIFICATE-----/,/-----END CERTIFICATE-----/p' "$WORK_DIR/s-leaf.pem" >"$WORK_DIR/s-chain.pem"
	s_chain_ok=0
	openssl verify -CAfile "$PROXY_ROOT_FILE" -untrusted "$WORK_DIR/s-chain.pem" "$WORK_DIR/s-chain.pem" >/dev/null 2>&1 && s_chain_ok=1
	s_not_stands=0
	openssl verify -CAfile "$PROXY_STANDS_CA_CERT_FILE" -untrusted "$WORK_DIR/s-chain.pem" "$WORK_DIR/s-chain.pem" >/dev/null 2>&1 || s_not_stands=1
	s_chain_verify="$(openssl verify -CAfile "$PROXY_ROOT_FILE" -untrusted "$WORK_DIR/s-chain.pem" "$WORK_DIR/s-chain.pem" 2>&1 | tr '\n' ' ')"
	local s_recs t_recs
	s_recs="$(records s | jq --argjson n "$s_before" '.[$n:]')"
	t_recs="$(records t | jq --argjson n "$t_before" '.[$n:]')"

	observe "the command's NO_PROXY is ${K4[no_proxy]}"
	observe "S's certificate as the command saw it is $s_leaf and S's own certificate file is $s_own: a different leaf, issued by $s_issuer; its served chain verifies against the proxy test root (chain_ok=$s_chain_ok: $s_chain_verify) and not against the stands' authority (not_stands=$s_not_stands)"
	observe "T's certificate as the command saw it is $t_leaf and T's own certificate file is $t_own"
	observe "T answered the POST with: ${K4[t_body]}; T's last record's body is $(printf '%s' "$t_recs" | jq -r '[.[] | select(.body != "") | .body] | .[0] // ""')"
	observe ".svc: ${K4[svc]}; .ts.net: ${K4[tsnet]}; curl 10.x: ${K4[curl10]}; Go 10.x: ${K4[go10]}; Node 10.x: ${K4[node10]}"
	observe "169.254.169.254 through the proxy: ${K4[metadata]}"
	observe "the broker's own loopback port through the proxy: ${K4[broker_port]} (and on a CONNECT: ${K4[broker_connect]})"
	observe "the same private address asked of the proxy with curl: ${K4[private]}"
	observe "no proxy password: ${K4[no_password]}; a made-up one: ${K4[made_up]}"

	judge "$([ "${K4[s_status]}" = 200 ] && [ "$s_leaf" != "$s_own" ] && [ "$s_chain_ok" = 1 ] && [ "$s_not_stands" = 1 ] && contains "Cipher proxy intermediate" "$s_issuer" && echo 0 || echo 1)" \
		"the call to S goes through the proxy port and S's certificate chains through the broker's intermediate to the proxy test root" \
		"S answered ${K4[s_status]} (want 200) and presented a leaf $s_leaf against the stand's own $s_own (want a leaf the stand never wrote), issued by $s_issuer (want an issuer naming 'Cipher proxy intermediate'); its chain against the proxy test root verified: '$s_chain_verify' (chain_ok=$s_chain_ok, want 1; not_stands=$s_not_stands, want 1 - it must not verify against the stands' authority)"
	judge "$([ "$t_leaf" = "$t_own" ] && contains "the-bytes-the-command-sent" "$(printf '%s' "$t_recs" | jq -r '[.[] | .body] | join(" ")')" && echo 0 || echo 1)" \
		"T presents its own certificate and gets the bytes the command sent" \
		"T's leaf as the command saw it was $t_leaf and T's own certificate is $t_own; T's recorded body held: '$(printf '%s' "$t_recs" | jq -r '[.[] | .body] | join(" ")')'"
	# 1186 reads the Go probe's error as "did not reach the proxy" only when it names the direct
	# dial (dial tcp 10.11.12.13:443). That text appears only where a direct dial to 10/8 is refused
	# quickly - a no-route namespace, as a Legion pod gives itself with a NET_ADMIN sidecar that adds
	# `unreachable 10.0.0.0/8` (Go then prints "no route to host" in a few ms). On a host that
	# blackholes 10/8 instead, Go times out ("context deadline exceeded"), never writes "dial tcp",
	# and this judge fails though nothing regressed: run k4 where a dial to 10/8 is refused, not dropped.
	judge "$(! contains "CONNECT tunnel failed" "${K4[svc]}" && contains "the .svc stand-in answered" "${K4[svc]}" && contains "curl:" "${K4[tsnet]}" && ! contains "CONNECT tunnel failed" "${K4[tsnet]}" && contains "curl:" "${K4[curl10]}" && ! contains "CONNECT tunnel failed" "${K4[curl10]}" && contains "dial tcp 10.11.12.13:443" "${K4[go10]}" && echo 0 || echo 1)" \
		"the .svc and .ts.net calls and the 10.x calls of curl and of a Go program do not reach the proxy port" \
		".svc reached its own stand-in directly (${K4[svc]}); .ts.net and curl's 10.x each failed with a curl error and no proxy CONNECT; Go's 10.x said '${K4[go10]}' - read as not reaching the proxy only when it names its own direct dial (dial tcp 10.11.12.13:443), which a proxy-reached refusal (the proxy's status) does not carry"
	judge "$(contains "Proxy response (403)" "${K4[node10]}" && contains "private" "${K4[private]}" && echo 0 || echo 1)" \
		"Node's 10.x call reaches the proxy and is refused with an error that names the private address" \
		"undici reported '${K4[node10]}' - read as the proxy's CONNECT 403 only when it holds 'Proxy response (403)', where a 'status 200' is the proxy letting it through; and the same address asked of the proxy with curl answered '${K4[private]}', read as a refusal only when it names 'private'"
	judge "$(contains "link-local" "${K4[metadata]}" && contains "loopback" "${K4[broker_port]}" && echo 0 || echo 1)" \
		"the connections to 169.254.169.254 and the broker's own loopback port are refused with an error that names the rule" \
		"169.254.169.254: ${K4[metadata]}; the broker's port: ${K4[broker_port]}"

	# The three 407s, each driven with a release of its own: the CLI never builds a wrong proxy
	# password, so these are the grant's own release and curl.
	local s_mark short_now short_later withheld_before withheld_after
	s_mark="$(records_count s)"
	release AIRLOCK_DEMO_TOKEN 1m
	short_now="$(printf 'Authorization: Bearer %s' "$PLACEHOLDER" | curl -sS --noproxy '' --cacert "$AIRLOCK_CA_CERT_FILE" -K <(proxy_config "${AIRLOCK_PROXY_URL##*://}") -H @- "$AIRLOCK_S_URL/ok" 2>&1 | tr '\n' ' ')"
	sleep 70
	short_later="$(printf 'Authorization: Bearer %s' "$PLACEHOLDER" | curl -sS --noproxy '' --cacert "$AIRLOCK_CA_CERT_FILE" -K <(proxy_config "${AIRLOCK_PROXY_URL##*://}") -H @- "$AIRLOCK_S_URL/ok" 2>&1 | tr '\n' ' ')"

	release AIRLOCK_DEMO_TOKEN 30m
	withheld_before="$(printf 'Authorization: Bearer %s' "$PLACEHOLDER" | curl -sS --noproxy '' --cacert "$AIRLOCK_CA_CERT_FILE" -K <(proxy_config "${AIRLOCK_PROXY_URL##*://}") -H @- "$AIRLOCK_S_URL/ok" 2>&1 | tr '\n' ' ')"
	# Withholding a name from a session is an operator's act, and a pod session has no
	# operator, so the row the broker reads is written here directly.
	psql_stack -c "insert into withheld_secrets (enrollment_id, name) values ('$AIRLOCK_ENROLLMENT', 'AIRLOCK_DEMO_TOKEN')" >/dev/null
	withheld_after="$(printf 'Authorization: Bearer %s' "$PLACEHOLDER" | curl -sS --noproxy '' --cacert "$AIRLOCK_CA_CERT_FILE" -K <(proxy_config "${AIRLOCK_PROXY_URL##*://}") -H @- "$AIRLOCK_S_URL/ok" 2>&1 | tr '\n' ' ')"
	psql_stack -c "delete from withheld_secrets where enrollment_id = '$AIRLOCK_ENROLLMENT'" >/dev/null
	local tunnelled
	tunnelled="$(($(records_count s) - s_mark))"

	observe "a one-minute grant: at once '$short_now', after 70 seconds '$short_later'"
	observe "withholding the name mid-grant: before '$withheld_before', after '$withheld_after'"
	observe "S recorded $tunnelled requests across the four refused calls and the two that were meant to succeed"
	judge "$(contains "407" "${K4[no_password]}" && contains "407" "${K4[made_up]}" && contains "407" "$short_later" && contains "407" "$withheld_after" && [ "$tunnelled" = 2 ] && echo 0 || echo 1)" \
		"both bad proxy passwords, the call after the grant ends and the call after the withholding get 407, and nothing is tunnelled" \
		"the call with no password answered ${K4[no_password]}, with a made-up one ${K4[made_up]}, after the grant ended '$short_later', after the withholding '$withheld_after' (want 407 each); S recorded $tunnelled of the calls (want the 2 that were meant to succeed)"

	# The negative control: one more broker, on the stack's own database and profiles, whose
	# refusal of loopback is turned off for the first broker's API port alone.
	start_extra_broker loopback-allowed \
		BROKER_AIRLOCK_TEST_DIAL_ALLOW="$STACK_DIAL_ALLOW ${BROKER_API_ADDRS[0]}"
	release AIRLOCK_DEMO_TOKEN 30m
	local control_answer
	# The tunnel must reach the fixture, not merely answer without the word "loopback": a refusal
	# by any other rule also lacks it (the tester's control: with addr.IsLoopback() off, a later
	# localRule rule refuses 127.0.0.1 anyway). The proof the tunnel reached the broker's /healthz
	# is its 200, which a proxy refusal (403) or a failed CONNECT (000) never gives.
	control_answer="$(curl -sS -w ' http_status=%{http_code}' --noproxy '' -K <(proxy_config "$EXTRA_PROXY_ADDR") "http://${BROKER_API_ADDRS[0]}/healthz" 2>&1 | tr '\n' ' ')"
	control "a second broker on the same database and profiles, started with the first broker's address in BROKER_AIRLOCK_TEST_DIAL_ALLOW — the setting that turns the refusal of a loopback address off for one host — answered /healthz: $control_answer"
	judge "$(contains "http_status=200" "$control_answer" && echo 0 || echo 1)" \
		"negative control: with the refusal turned off the connection to the broker's loopback port is tunnelled and its /healthz answers 200" \
		"the same request through that broker answered '$control_answer' - read as the tunnel reaching /healthz only when it holds http_status=200, where a 403 is a refusal and 000 a failed CONNECT - and the stack's own brokers refuse it with '${K4[broker_port]}'"

	stop_stack
}

###############################################################################################
# k5
###############################################################################################

k5_call() { # <placeholder> -> prints the status
	printf 'Authorization: Bearer %s' "$1" | curl -sS -o /dev/null -w '%{http_code}' --noproxy '' --cacert "$AIRLOCK_CA_CERT_FILE" \
		-K <(proxy_config "${AIRLOCK_PROXY_URL##*://}") -H @- "$AIRLOCK_S_URL/ok"
}

# k5_stored drives the stored half of k5 against the stack this run started and answers through
# K5_* variables. keep says whether the stack was started with --keep-values.
k5_stored() {
	local s_mark first second new_value old_value
	old_value="$(stored_value "${STACK_SECRETS_PREFIX}airlock-demo-token")"
	s_mark="$(records_count s)"
	release AIRLOCK_DEMO_TOKEN 30m
	first="$(k5_call "$PLACEHOLDER")"
	new_value="the-second-version-of-the-airlock-demo-token-$RANDOM$RANDOM"
	set_fake_value "${STACK_SECRETS_PREFIX}airlock-demo-token" "$new_value"
	reread_all AIRLOCK_DEMO_TOKEN || return
	sleep 2
	second="$(k5_call "$PLACEHOLDER")"
	local recs got_first got_second
	recs="$(records s | jq --argjson n "$s_mark" '.[$n:]')"
	got_first="$(printf '%s' "$recs" | jq -r '[.[] | .header.Authorization // [] | .[]] | .[0] // ""')"
	got_second="$(printf '%s' "$recs" | jq -r '[.[] | .header.Authorization // [] | .[]] | .[1] // ""')"
	K5_FIRST_STATUS="$first"
	K5_SECOND_STATUS="$second"
	K5_FIRST_VALUE="$got_first"
	K5_SECOND_VALUE="$got_second"
	K5_OLD_VALUE="$old_value"
	K5_NEW_VALUE="$new_value"
}

check_k5() {
	expect "with a grant of a stored secret behind the airlock, the call after a new version of the secret and the broker's reread carries the new value, and every call succeeds"
	expect "with a google-login secret whose stand-in minter issues tokens of 7 minutes, the call 3 minutes after the first carries a freshly minted value"
	expect "the negative control, with the proxy's value kept across the reread and past the 5-minute margin, carries the old value on the second call"

	# One broker, not the stack's two. What this check reads is a pod's own held value: the
	# first call makes the pod hold it, the reread is what ends that, and the second call must
	# read the store again. With two brokers the front gives each connection whichever it
	# pleases, so a second call landing on the pod that never held the value reads the new one
	# whatever the reread did - the check would pass with the drop removed, and its negative
	# control would fail with the drop in place. Neither the check's words nor its control
	# names a second process.
	start_stack --one-broker
	stack_facts
	k5_stored
	observe "the stored secret: the first call answered $K5_FIRST_STATUS carrying $(shape "$K5_FIRST_VALUE") and the second $K5_SECOND_STATUS carrying $(shape "$K5_SECOND_VALUE") (compared by SHA-256 and length)"
	observe "the store held $(shape "$K5_OLD_VALUE") and then $(shape "$K5_NEW_VALUE")"
	# The recorded header is "Bearer <value>", so the comparison is that it carries the value
	# and not that it equals it.
	judge "$(contains "$K5_OLD_VALUE" "$K5_FIRST_VALUE" && contains "$K5_NEW_VALUE" "$K5_SECOND_VALUE" && [ "$K5_FIRST_STATUS" = 200 ] && [ "$K5_SECOND_STATUS" = 200 ] && echo 0 || echo 1)" \
		"the stored secret's second call carries the new value, and every call succeeds" \
		"the first call carried the old value and the second the new one, by SHA-256 and length; the two answered $K5_FIRST_STATUS and $K5_SECOND_STATUS"

	# The google-login half. The stack's Google stand answers a new numbered access token at
	# each refresh, with the 7-minute lifetime dev-airlock.sh gives it, so the proxy's
	# five-minute margin falls due before the second call three minutes on, which must carry a
	# fresh mint.
	local google_before google_after first_status second_status s_mark got_first got_second recs
	google_before="$(records_count google)"
	s_mark="$(records_count s)"
	release AIRLOCK_GOOGLE_LOGIN 30m
	first_status="$(k5_call "$PLACEHOLDER")"
	sleep 180
	second_status="$(k5_call "$PLACEHOLDER")"
	google_after="$(records_count google)"
	recs="$(records s | jq --argjson n "$s_mark" '.[$n:]')"
	got_first="$(printf '%s' "$recs" | jq -r '[.[] | .header.Authorization // [] | .[]] | .[0] // ""')"
	got_second="$(printf '%s' "$recs" | jq -r '[.[] | .header.Authorization // [] | .[]] | .[1] // ""')"
	observe "the google-login secret: the call at once answered $first_status carrying $(shape "$got_first"), and the one three minutes later $second_status carrying $(shape "$got_second"); the stand-in minter was asked $((google_after - google_before)) times"
	if [ "$got_first" = "$got_second" ]; then
		fail "the google-login secret's second call carries the new value — it carried the first call's. The stand answers a new numbered access token at each refresh, with the 7-minute lifetime dev-airlock.sh gives it, so the proxy's five-minute margin falls due two minutes after the first mint and the call three minutes on should have carried a fresh one. Here the minter was asked $((google_after - google_before)) times, both calls answered $first_status and $second_status, and the two carried one value by SHA-256 and length."
	else
		judge "$([ "$first_status" = 200 ] && [ "$second_status" = 200 ] && echo 0 || echo 1)" \
			"the google-login secret's second call carries the new value, and every call succeeds" \
			"the minter was asked $((google_after - google_before)) times and the two calls carried different values by SHA-256 and length"
	fi

	stop_stack

	# The negative control, which is a stack of its own: --keep-values is the switch that keeps
	# a grant's values across a reread and past a minted token's margin.
	start_stack --one-broker --keep-values
	stack_facts
	k5_stored
	control "with --keep-values (BROKER_AIRLOCK_TEST_KEEP_VALUES=true) the first call answered $K5_FIRST_STATUS carrying $(shape "$K5_FIRST_VALUE") and the second $K5_SECOND_STATUS carrying $(shape "$K5_SECOND_VALUE"); the store held $(shape "$K5_OLD_VALUE") and then $(shape "$K5_NEW_VALUE")"
	judge "$(contains "$K5_OLD_VALUE" "$K5_SECOND_VALUE" && echo 0 || echo 1)" \
		"negative control: with the proxy's value kept across the reread, the second call carries the old value" \
		"the second call carried the old value by SHA-256 and length, where the same run without the switch carried the new one"

	stop_stack
}

###############################################################################################
# k6
###############################################################################################
#
# k6 drives real clients: the Azure CLI, signed in as an admin az command signs in under its
# grant (az login --service-principal with the grant's federated token), and the AWS CLI and
# boto3 against the stack's stand-in AWS service. A Legion issue pod has no boto3, and a
# pod on an older worker image has no az, so the check installs both with
# scripts/airlock-clients.sh, which pins a version and checks its SHA-256 before anything of it
# runs, and reads that script's exports, whose PATH puts its az ahead of any az the image has.
#
# The stack's stand-ins play the two clouds whole, so both clients run as they do in anger: the
# Microsoft stand answers the tenant's OpenID configuration document and the token endpoint,
# the same host plays the Resource Manager an access token is spent at, and the AWS stand is
# told the session token the stack's own AWS secret holds, so a signature it verifies is one
# AWS would verify. az is driven through its own private cloud with MSAL's instance discovery
# turned off (AZURE_CORE_INSTANCE_DISCOVERY=false, the setting Azure Stack deployments use):
# that discovery asks login.microsoftonline.com whether a cloud's authority is a Microsoft one,
# which no stand-in authority can ever be, and it is the one az step no stand could answer.

# k6_clients reads the exports of scripts/airlock-clients.sh, which installs az and boto3 at a
# pinned, checked version. Its own lines go to this script's stderr; its stdout is the exports.
k6_clients() {
	echo "airlock-check: installing the pinned clients k6 drives (scripts/airlock-clients.sh)..." >&2
	local exports
	exports="$(bash "$SCRIPT_DIR/airlock-clients.sh")" || {
		echo "airlock-check: scripts/airlock-clients.sh could not install the clients" >&2
		exit 1
	}
	eval "$exports"
	CLIENT_VERSIONS="az $(az version --output json 2>/dev/null | jq -r '."azure-cli"'), boto3 $("$AIRLOCK_BOTO3_PYTHON" -c 'import boto3; print(boto3.__version__)' 2>/dev/null), aws $(aws --version 2>&1 | cut -d' ' -f1)"
}

# approve_record approves one pending request on the broker's own page, as the person does: the
# page sends a browser to the stack's issuer, which signs the approver in with no form to fill,
# and the decision is posted back with an Origin header, which the page requires. curl's own
# cookie jar cannot be used - both cookies are Secure and this broker is on http - so each
# Set-Cookie is read out of the headers and sent back by hand. Every printed and every
# profile_digest field of the form is posted back: the page token is a MAC over every record the
# page printed, so an approval that names fewer is refused 403 PAGE_TOKEN_INVALID, and an approval
# names the fingerprint of each profile the page printed, so one that names fewer is refused as
# "Profile changed". Either way nothing is decided.
approve_record() { # <record id>
	local rec="$1" d="$WORK_DIR/approve"
	mkdir -p "$d"
	curl -sS -D "$d/start" -o /dev/null "$AGENT_SECRETS_URL/approve/$rec"
	local authorize signin callback session
	authorize="$(sed -n 's/^Location: //p' "$d/start" | tr -d '\r')"
	signin="$(sed -n 's/^Set-Cookie: \(__Host-broker-signin=[^;]*\).*/\1/p' "$d/start" | tr -d '\r')"
	[ -n "$authorize" ] || return 1
	curl -sS -D "$d/authorize" -o /dev/null "$authorize"
	callback="$(sed -n 's/^Location: //p' "$d/authorize" | tr -d '\r')"
	curl -sS -D "$d/callback" -o /dev/null -H "Cookie: $signin" "$callback"
	session="$(sed -n 's/^Set-Cookie: \(__Host-broker-session=[^;]*\).*/\1/p' "$d/callback" | tr -d '\r')"
	[ -n "$session" ] || return 1
	curl -sS -H "Cookie: $session" -o "$d/page.html" "$AGENT_SECRETS_URL/approve/$rec"
	# values is every value of the hidden field the page names, in the page's order, read one
	# field at a time: the form prints all of its fields on one line, and a field it prints more
	# than once - one printed field per record, one profile_digest field per profile, each
	# "<record> <name> <digest>" - comes back once each. field is its first value.
	local value
	values() { grep -o "name=\"$1\" value=\"[^\"]*\"" "$d/page.html" | sed 's/.*value="\([^"]*\)"/\1/'; }
	field() { values "$1" | head -n 1; }
	local printed=() digests=()
	while IFS= read -r value; do
		printed+=(--data-urlencode "printed=$value")
	done < <(values printed)
	while IFS= read -r value; do
		digests+=(--data-urlencode "profile_digest=$value")
	done < <(values profile_digest)
	# The page's form token reaches curl on standard input (`--data-urlencode token@-`), in no
	# command line.
	printf '%s' "$(field token)" | curl -sS -o "$d/decided.html" -w '%{http_code}' -H "Cookie: $session" -H "Origin: $AGENT_SECRETS_URL" \
		--data-urlencode "decision=approve" --data-urlencode "record=$(field record)" \
		"${printed[@]+"${printed[@]}"}" --data-urlencode token@- \
		"${digests[@]+"${digests[@]}"}" \
		--data-urlencode "expires=$(field expires)" "$AGENT_SECRETS_URL/approve" >>"$d/decisions"
	echo >>"$d/decisions"
}

# approve_watch is the approver sitting at the page while a command waits: it reads the record
# ids out of the CLI's own stderr, where each pending request prints its page, and approves each
# one once. A pod session has no operator and the broker takes this decision on its page alone,
# so this is the only way a human-tier name is released to a check.
approve_watch() { # <the file the CLI's stderr goes to>
	local log="$1" seen=" " rec
	while :; do
		for rec in $(grep -o '/approve/[0-9a-f]\{64\}' "$log" 2>/dev/null | sed 's|/approve/||' | sort -u); do
			case "$seen" in *" $rec "*) continue ;; esac
			seen="$seen$rec "
			approve_record "$rec" >/dev/null 2>&1 || true
		done
		sleep 1
	done
}

# k6_azure_wrapper writes the `az` this check puts first on PATH. The stand-in Microsoft
# endpoint and the stand-in Resource Manager are .test names of this stack's own, and az reaches
# the endpoints of a cloud, so az has to be told a cloud whose endpoints are those two names.
# The login steps (k6_azure) make their own AZURE_CONFIG_DIR and log in at once, as an admin az
# command's steps do, and they are kept exactly as those run, so the registration is here: this
# wrapper registers the cloud in whatever directory the login steps made and then runs the real
# az with the arguments it was given.
#
# It also turns MSAL's instance discovery off, which is what an Azure Stack deployment does
# (az's own core.instance_discovery, as AZURE_CORE_INSTANCE_DISCOVERY): that discovery asks
# login.microsoftonline.com whether this cloud's authority is a Microsoft one, and the answer
# for a stand-in authority is and must be no. It is the one call of az's a stand cannot answer,
# since it is made to Microsoft's own host and about the stand rather than to it.
#
# It records the az it wraps, REAL_AZ, as `command -v az` names it, and the PATH it was found on,
# K6_AZ_PATH, which k6_azure puts the wrapper's directory in front of. Every call the wrapper makes
# to REAL_AZ runs on K6_AZ_PATH again, not on the PATH the wrapper was handed. On a box whose az
# on PATH is a shim (a version manager's, which looks az up on PATH again once its own directory
# is out of the way), REAL_AZ is that shim, and on the wrapper's PATH the next az the shim finds
# is this wrapper: the two would hand each call to each other for good, and the cloud register
# below would start a new pair at each turn. On K6_AZ_PATH the shim finds the az it stands for.
k6_azure_wrapper() {
	REAL_AZ="$(command -v az)"
	K6_AZ_PATH="$PATH"
	local real_path
	printf -v real_path %q "$K6_AZ_PATH"
	mkdir -p "$WORK_DIR/k6/bin"
	cat >"$WORK_DIR/k6/bin/az" <<EOF
#!/usr/bin/env bash
set -euo pipefail
export AZURE_CORE_INSTANCE_DISCOVERY=false
export PATH=$real_path
if [ -n "\${AZURE_CONFIG_DIR:-}" ] && [ ! -f "\$AZURE_CONFIG_DIR/clouds.config" ]; then
  "$REAL_AZ" cloud register -n airlock --skip-endpoint-discovery \\
    --endpoint-active-directory "$AIRLOCK_MICROSOFT_URL" \\
    --endpoint-active-directory-resource-id "$AIRLOCK_AZURE_RESOURCE_URL/" \\
    --endpoint-active-directory-graph-resource-id "$AIRLOCK_AZURE_RESOURCE_URL/" \\
    --endpoint-resource-manager "$AIRLOCK_AZURE_RESOURCE_URL/" \\
    --endpoint-management "$AIRLOCK_AZURE_RESOURCE_URL/" \\
    --endpoint-gallery "$AIRLOCK_AZURE_RESOURCE_URL/" \\
    --suffix-storage-endpoint core.windows.net >/dev/null
  "$REAL_AZ" cloud set -n airlock >/dev/null
fi
exec "$REAL_AZ" "\$@"
EOF
	chmod +x "$WORK_DIR/k6/bin/az"
}

# k6_azure_login_steps prints the script each k6 run hands `sh -c` under its grant of
# AIRLOCK_AZURE_LOGIN, an azure-login value ({"tenant_id","client_id","federated_token",
# "expires_at"}): the steps an admin az command runs under its grant. The federated token goes
# into a 0600 file in a throwaway directory, beside the AZURE_CONFIG_DIR made in it, and az logs in
# as the service principal with `--federated-token @FILE`, so the token is in no command line;
# then the script runs its arguments. The EXIT trap removes the directory and keeps the command's
# status, and the HUP, INT and TERM traps turn a signal into an exit, so that trap still runs.
k6_azure_login_steps() {
	cat <<'EOF'
d=$(mktemp -d) || exit 1; umask 077
trap "rm -rf \"\$d\"" EXIT; trap "exit 129" HUP; trap "exit 130" INT; trap "exit 143" TERM
printf %s "$AIRLOCK_AZURE_LOGIN" | jq -er .federated_token >"$d/token" &&
tenant=$(printf %s "$AIRLOCK_AZURE_LOGIN" | jq -er .tenant_id) && client=$(printf %s "$AIRLOCK_AZURE_LOGIN" | jq -er .client_id) &&
mkdir "$d/config" && export AZURE_CONFIG_DIR="$d/config" && unset AIRLOCK_AZURE_LOGIN &&
az login --service-principal --username "$client" --tenant "$tenant" --federated-token "@$d/token" --allow-no-subscriptions --output none &&
"$@"
EOF
}

# k6_azure runs the five signed-in az runs and then the exchange and the five Resource Manager
# calls, and leaves what it saw in K6_AZ_*.
k6_azure() {
	local d="$WORK_DIR/k6"
	mkdir -p "$d"
	k6_azure_wrapper

	local microsoft_before
	microsoft_before="$(records_count microsoft)"
	: >"$d/cli.err"
	approve_watch "$d/cli.err" &
	local watcher=$!
	HELPER_PIDS+=("$watcher")

	# The five runs. Each is one grant of AIRLOCK_AZURE_LOGIN, under which az logs in with
	# k6_azure_login_steps.
	#
	# What each run does once it is logged in: `az group list`, the admin command,
	# and then `az account get-access-token`, which prints the access token az itself holds -
	# the one the broker's exchange handed it, which has to be a placeholder and never the real
	# token the Resource Manager is sent. Each run writes its own access file, named by the run's
	# number, so each of the five is judged and not only the last.
	cat >"$d/az-steps.sh" <<EOF
set -u
az group list -o json
az account get-access-token -o json >$d/access-\$1.json
EOF
	local run
	: >"$d/runs.log"
	rm -f "$d"/access-*.json
	for run in 1 2 3 4 5; do
		echo "=== run $run ===" >>"$d/runs.log"
		PATH="$d/bin:$K6_AZ_PATH" agent-secrets AIRLOCK_AZURE_LOGIN --ttl 1h --wait 5m \
			--reason "airlock check k6 run $run: az logs in and calls the Resource Manager" -- \
			sh -c "$(k6_azure_login_steps)" sh bash "$d/az-steps.sh" "$run" >>"$d/runs.log" 2>>"$d/cli.err"
		echo "exit $?" >>"$d/runs.log"
	done
	kill "$watcher" 2>/dev/null || true
	K6_AZ_RUNS="$(grep -c '^exit ' "$d/runs.log")"
	K6_AZ_FAILED="$(grep -c '^exit [^0]' "$d/runs.log" || true)"
	K6_AZ_ERROR="$(grep -m1 '^ERROR' "$d/cli.err" | tr -d '\r')"

	local recs
	recs="$(records microsoft | jq --argjson n "$microsoft_before" '.[$n:]')"
	K6_AZ_REACHED="$(printf '%s' "$recs" | jq -r '[.[] | .method + " " + .path] | unique | join("; ")')"
	K6_AZ_ASSERTION="$(printf '%s' "$recs" | jq -r '[.[] | select(.path | endswith("/token")) | .body | capture("client_assertion=(?<a>[^&]*)") | .a] | .[0] // ""')"
	# The form's own percent-encoding: a JWT holds none of the characters a form escapes but
	# the "=" of its padding, and the comparison is against the stack's own token.
	K6_AZ_ASSERTION="${K6_AZ_ASSERTION//%3D/=}"
	# The access token az held after each run, and how many of the five hold the placeholder
	# prefix anywhere in them (contains): a run whose file is missing or holds no token counts as
	# none.
	K6_AZ_ACCESS_SHAPES="" K6_AZ_ACCESS_PLACEHOLDERS=0
	local token
	for run in 1 2 3 4 5; do
		token="$(jq -r '.accessToken // ""' "$d/access-$run.json" 2>/dev/null)"
		K6_AZ_ACCESS_SHAPES+="run $run: $(shape "$token"); "
		contains agent-secrets-placeholder "$token" && K6_AZ_ACCESS_PLACEHOLDERS=$((K6_AZ_ACCESS_PLACEHOLDERS + 1))
	done
	mapfile -t K6_AZ_ARM_HEADERS < <(printf '%s' "$recs" | jq -r '.[] | select(.path == "/subscriptions") | .header.Authorization // [] | .[]')
	K6_AZ_ARM_STATUS="$(grep '^exit ' "$d/runs.log" | tr '\n' ' ')"
	K6_AZ_EXCHANGES="$(printf '%s' "$recs" | jq '[.[] | select(.path | endswith("/token"))] | length')"
	K6_AZ_SERVED="$(for log in "$AIRLOCK_DIR"/broker*.log; do printf '%s:%s ' "${log##*/}" "$(bound_call_lines "$log" | jq -s '[.[] | select(.message == "airlock proxy: a call to a bound host")] | length')"; done)"
}

# k6_aws runs the AWS CLI's three calls and boto3's two against the stand-in AWS service, and
# reads what it recorded and why it refused each call it refused (refusals_of), before and after:
# the stand-in records a call whose signature it refuses as well as one it accepts, so its
# records alone cannot show a refused signature.
k6_aws() {
	local d="$WORK_DIR/k6"
	mkdir -p "$d"
	head -c 102400 /dev/urandom >"$d/100k.bin"
	# boto3's own two steps are scripts/airlock/aws-boto3.py, run where it lies.
	local aws_before refusals_before
	aws_before="$(records_count aws)"
	refusals_before="$(refusals_of aws)"
	cat >"$d/aws-steps.sh" <<EOF
set -u
out=$d/aws-out
: >"\$out"
export AWS_DEFAULT_REGION=us-east-1
say() { printf '%s\n' "\$1" >>"\$out"; }
say "sts=\$(aws --endpoint-url $AIRLOCK_AWS_URL sts get-caller-identity 2>&1 | tail -n 1 | tr -d '\r')"
ls_out=\$(aws --endpoint-url $AIRLOCK_AWS_URL s3 ls 2>&1); ls_exit=\$?
say "ls=\$(printf '%s' "\$ls_out" | tail -n 1 | tr -d '\r')"
say "ls_exit=\$ls_exit"
say "cp=\$(aws --endpoint-url $AIRLOCK_AWS_URL s3 cp $d/100k.bin s3://airlock-bucket/k6-100k.bin 2>&1 | tail -n 1 | tr -d '\r')"
$AIRLOCK_BOTO3_PYTHON $SCRIPT_DIR/airlock/aws-boto3.py $AIRLOCK_AWS_URL $d/boto3.json >$d/boto3.log 2>&1
echo "boto3 exit \$?" >>$d/boto3.log
EOF
	agent-secrets AIRLOCK_AWS_KEY --ttl 30m --reason "airlock check k6: the AWS CLI and boto3 call the stand-in AWS service" -- \
		bash "$d/aws-steps.sh" >>"$d/runs.log" 2>>"$d/cli.err"

	declare -gA K6AWS
	local key value
	while IFS='=' read -r key value; do K6AWS["$key"]="$value"; done <"$d/aws-out"
	K6_AWS_RECS="$(records aws | jq --argjson n "$aws_before" '(.[$n:] // [])')"
	K6_AWS_CALLS="$(printf '%s' "$K6_AWS_RECS" | jq 'length')"
	K6_AWS_TOKENS="$(printf '%s' "$K6_AWS_RECS" | jq -r '[.[] | .header["X-Amz-Security-Token"] // [] | .[]] | unique | length')"
	K6_AWS_KEYIDS="$(printf '%s' "$K6_AWS_RECS" | jq -r '[.[] | .header.Authorization // [] | .[] | capture("Credential=(?<k>[^/]*)/") | .k] | unique | join(",")')"
	K6_AWS_PLACEHOLDERS="$(printf '%s' "$K6_AWS_RECS" | jq '[.[] | select(((.header | tostring) + .query) | contains("agent-secrets-placeholder"))] | length')"
	K6_AWS_REFUSED="$(printf '%s' "$K6_AWS_RECS" | jq -r '[.[] | .path] | join(" ")')"
	K6_AWS_CHUNK="$(jq -r '.chunk_answer // .chunk_signed // ""' "$d/boto3.json" 2>/dev/null)"
	K6_AWS_PRESIGNED="$(jq -r '.presigned // ""' "$d/boto3.json" 2>/dev/null)"
	K6_AWS_BOTO3="$(jq -c '{boto3, botocore, chunks, decoded_length, presigned_query}' "$d/boto3.json" 2>/dev/null)"
	K6_AWS_BOTO3_LOG="$(tail -n 3 "$d/boto3.log" 2>/dev/null | tr '\n' ' ')"
	# The stand-in's refusals of this run's calls, and UNKNOWN when either read answered no array.
	# The two arrays reach jq on its standard input, never as arguments: a refusal of a call that
	# carries a placeholder holds that whole call, body included, which a 100 kB upload takes past
	# the limit of one argument.
	local refusals_after this_run
	refusals_after="$(refusals_of aws)"
	if [ -n "$refusals_before" ] && [ -n "$refusals_after" ]; then
		this_run="$(printf '%s\n%s\n' "$refusals_before" "$refusals_after" | jq -cs '.[1][(.[0] | length):]')"
		K6_AWS_STAND_REFUSED="$(printf '%s' "$this_run" | jq 'length')"
		K6_AWS_STAND_REFUSALS="$(printf '%s' "$this_run" | jq -r 'map(split("\n")[0]) | join(" | ")')"
	else
		K6_AWS_STAND_REFUSED=UNKNOWN
		K6_AWS_STAND_REFUSALS="the stand-in's refusals route answered no JSON array, so whether it refused a signature is not known"
	fi
}

# k6_second_aws starts a second stand-in AWS service that holds a second fake key pair with no
# session token, which no kind binds and no proxy reaches, and sends it, outside agent-secrets,
# aws sts get-caller-identity signed with that pair and the same call with the pair's key id and
# a wrong secret key. It reads what that service recorded and why it refused (its refusals
# route), so the check can see the stand-in refuse a bad signature after it has passed the key id
# and the session token. It is a second dev-airlock-stands process of its own, killed on return.
k6_second_aws() {
	local d="$WORK_DIR/k6/second-aws"
	mkdir -p "$d"
	local key_id="AKIA""SECONDSTANDIN000" secret="the-second-stand-in-aws-secret-key"
	"$STANDS_BIN" --dir "$d/files" --aws-key "$key_id=$secret" >"$d/report.json" 2>"$d/stands.log" &
	local pid=$!
	HELPER_PIDS+=("$pid")
	local _
	for _ in $(seq 1 100); do
		grep -q 'stands listening' "$d/stands.log" 2>/dev/null && break
		sleep 0.1
	done
	local url ca records_url
	url="$(jq -r '.stands.aws.url' "$d/report.json")"
	ca="$(jq -r '.ca_cert_file' "$d/report.json")"
	records_url="$(jq -r '.records_url' "$d/report.json")"
	declare -gA K6SECOND
	local name secret_used out exit_code
	for name in signed wrong; do
		secret_used="$secret"
		[ "$name" = wrong ] && secret_used="not-the-second-stand-in-secret-key"
		exit_code=0
		# --debug writes the exchange to standard error, where the status line of each answer is
		# read: the CLI's own error for a refusal whose body is no XML names no status.
		out="$(env -u HTTPS_PROXY -u HTTP_PROXY -u https_proxy -u http_proxy -u AWS_SESSION_TOKEN -u AWS_PROFILE \
			AWS_ACCESS_KEY_ID="$key_id" AWS_SECRET_ACCESS_KEY="$secret_used" AWS_DEFAULT_REGION=us-east-1 \
			AWS_CA_BUNDLE="$ca" AWS_CONFIG_FILE=/dev/null AWS_SHARED_CREDENTIALS_FILE=/dev/null AWS_EC2_METADATA_DISABLED=true \
			aws --debug --endpoint-url "$url" sts get-caller-identity --output text 2>"$d/$name.debug")" || exit_code=$?
		K6SECOND["${name}_exit"]="$exit_code"
		K6SECOND["${name}_status"]="$(grep -oE 'HTTP/1\.1" [0-9]{3}' "$d/$name.debug" | tail -n 1 | grep -oE '[0-9]{3}$')"
		K6SECOND["${name}_said"]="$(printf '%s' "$out" | tail -n 1 | tr -d '\r')"
	done
	# What the second stand-in recorded and why it refused, read with the same helpers as the
	# stack's own stand-in, at its own records URL: a refusals route that answers no array is
	# UNKNOWN, which fails the judge.
	local refusals
	K6SECOND[records]="$(AIRLOCK_RECORDS_URL="$records_url" records_count aws)"
	refusals="$(AIRLOCK_RECORDS_URL="$records_url" refusals_of aws)"
	if [ -n "$refusals" ]; then
		K6SECOND[refusal_count]="$(printf '%s' "$refusals" | jq 'length')"
		K6SECOND[refusals]="$(printf '%s' "$refusals" | jq -r 'map(split("\n")[0]) | join(" | ")')"
	else
		K6SECOND[refusal_count]=UNKNOWN
		K6SECOND[refusals]="the second stand-in's refusals route answered no JSON array"
	fi
	kill "$pid" 2>/dev/null || true
	wait "$pid" 2>/dev/null || true
}

check_k6() {
	expect "the stand-in token endpoint gets the real federated token"
	expect "az gets a placeholder access token"
	expect "the stand-in Resource Manager gets a real access token in Authorization on all five runs, whichever process served each call"
	expect "the stand-in AWS service accepts each signature of the STS call, the list and the upload, with the real session token, and finds no placeholder in them"
	expect "the list is answered, not only signed: aws s3 ls exits 0 through the airlock"
	expect "the chunk-signed upload and the presigned URL get an error that names them"
	expect "the negative control, with the exchange and the signing turned off (dev-airlock.sh --no-kind), gives the token endpoint the placeholder, and refuses each AWS call 403 PLACEHOLDER_NOT_SWAPPED, because X-Amz-Security-Token keeps its placeholder, so the stand-in AWS service records nothing"
	expect "the second stand-in AWS service, which holds a second key pair with no session token and which no kind binds, records both of its calls, answers the call signed with its pair 2xx, and refuses the call with the wrong secret key 403 because the signature is not the one its pair makes"

	k6_clients
	start_stack
	stack_facts
	observe "the clients this run drove: $CLIENT_VERSIONS, each installed by scripts/airlock-clients.sh at a pinned version whose SHA-256 it checked"

	k6_azure
	local federated
	federated="$(cat "$AIRLOCK_DIR/azure-identity-token")"
	observe "the five az runs: $K6_AZ_RUNS runs, $K6_AZ_FAILED of them non-zero ($K6_AZ_ARM_STATUS); what they reached on the Microsoft stand-in was $K6_AZ_REACHED, and the first ERROR line az wrote, if any, was: ${K6_AZ_ERROR:-none}"
	observe "the exchange: the token endpoint was asked $K6_AZ_EXCHANGES times and its client_assertion was $(shape "$K6_AZ_ASSERTION") against the stack's own federated token $(shape "$federated") (compared by SHA-256 and length)"
	observe "the access token az itself holds after its login (az account get-access-token), for each run: ${K6_AZ_ACCESS_SHAPES}$K6_AZ_ACCESS_PLACEHOLDERS of the five open 'agent-secrets-placeholder'"
	observe "the Resource Manager calls az made, one per run, and their Authorization headers: $(for h in "${K6_AZ_ARM_HEADERS[@]+"${K6_AZ_ARM_HEADERS[@]}"}"; do printf '%s; ' "$(shape "$h")"; done)"
	observe "the brokers' own logs of calls to a bound host, which is how many each process served: $K6_AZ_SERVED"

	judge "$([ "$K6_AZ_EXCHANGES" -gt 0 ] && [ -n "$federated" ] && contains "$federated" "$K6_AZ_ASSERTION" && echo 0 || echo 1)" \
		"the stand-in token endpoint gets the real federated token" \
		"the token endpoint saw $K6_AZ_EXCHANGES exchange(s); its client_assertion was $(shape "$K6_AZ_ASSERTION") against the stack's own federated token $(shape "$federated"), compared by SHA-256 and length - an empty federated token is no match"

	judge "$([ "$K6_AZ_RUNS" = 5 ] && [ "$K6_AZ_FAILED" = 0 ] && [ "$K6_AZ_ACCESS_PLACEHOLDERS" = 5 ] && echo 0 || echo 1)" \
		"az gets a placeholder access token" \
		"az ran $K6_AZ_RUNS times against the stand-in cloud ($K6_AZ_FAILED exited non-zero; want 5 runs, 0 failed), and the token az held after each run opens 'agent-secrets-placeholder' on $K6_AZ_ACCESS_PLACEHOLDERS of the five (want 5) ($K6_AZ_ACCESS_SHAPES)"

	local arm_calls=0 arm_real=0 header
	for header in "${K6_AZ_ARM_HEADERS[@]+"${K6_AZ_ARM_HEADERS[@]}"}"; do
		arm_calls=$((arm_calls + 1))
		contains "agent-secrets-placeholder" "$header" || arm_real=$((arm_real + 1))
	done
	judge "$([ "$arm_calls" = 5 ] && [ "$arm_real" = 5 ] && [ "$(echo "$K6_AZ_SERVED" | tr ' ' '\n' | grep -c ':[1-9]')" -ge 2 ] && echo 0 || echo 1)" \
		"the stand-in Resource Manager gets a real access token in Authorization on all five runs, whichever process served each call" \
		"$arm_real of $arm_calls calls carried a token that is no placeholder, each answered by whichever broker the front gave its connection to ($K6_AZ_SERVED); the five calls are az's own, one per run, made with the azure-access placeholder the exchange handed it"

	k6_aws
	local aws_secret aws_session
	aws_secret="$(stored_value "${STACK_SECRETS_PREFIX}airlock-aws-key")"
	aws_session="$(printf '%s' "$aws_secret" | jq -r .session_token)"
	observe "the AWS CLI: sts said '${K6AWS[sts]}'; s3 ls said '${K6AWS[ls]}' and exited ${K6AWS[ls_exit]}; s3 cp said '${K6AWS[cp]}'"
	observe "the stand-in AWS service recorded $K6_AWS_CALLS calls on $K6_AWS_REFUSED; the Credential of each named the key id '$K6_AWS_KEYIDS' and the real key id is '$AIRLOCK_AWS_KEY_ID'; $K6_AWS_PLACEHOLDERS of them carried a placeholder in a header or the query"
	observe "the stand-in AWS service refused $K6_AWS_STAND_REFUSED of this run's calls (its own refusals route): ${K6_AWS_STAND_REFUSALS:-none}"
	observe "boto3: $K6_AWS_BOTO3 (its own last lines: $K6_AWS_BOTO3_LOG)"
	observe "the chunk-signed upload got: $K6_AWS_CHUNK"
	observe "the presigned URL got: $K6_AWS_PRESIGNED"

	# A record is a call that reached the stand-in, accepted or refused, so the judge reads the
	# stand-in's own refusals as well: a call it refused is a signature it did not accept, and a
	# refusals route that could not be read is UNKNOWN, which is no pass.
	local accepted
	accepted="$(printf '%s' "$K6_AWS_RECS" | jq '[.[] | select(.path != "")] | length')"
	judge "$([ "$K6_AWS_PLACEHOLDERS" = 0 ] && [ "$accepted" -ge 3 ] && [ "$K6_AWS_STAND_REFUSED" = 0 ] && contains "$AIRLOCK_AWS_KEY_ID" "$K6_AWS_KEYIDS" && ! contains "403" "${K6AWS[sts]}" && echo 0 || echo 1)" \
		"the stand-in AWS service accepts each signature of the STS call, the list and the upload, with the real session token, and finds no placeholder in them" \
		"$accepted calls reached it (want 3 or more), signed with key id(s) $K6_AWS_KEYIDS (want the real $AIRLOCK_AWS_KEY_ID) and the stack's own session token ($(shape "$aws_session")), with $K6_AWS_PLACEHOLDERS placeholders among them (want 0), and it refused $K6_AWS_STAND_REFUSED of them (want 0); the three calls said '${K6AWS[sts]}' / '${K6AWS[ls]}' / '${K6AWS[cp]}'"

	# The list is read for its own answer as well as its signature. A stand-in that verifies a
	# signature and then answers a line of prose makes the AWS CLI report a 500, and a check that
	# reads the signature alone would pass on a call its own client calls broken: the stand-in
	# answers an empty ListAllMyBuckets now, and this is what holds it to that.
	judge "$([ "${K6AWS[ls_exit]}" = 0 ] && ! contains "error" "${K6AWS[ls]}" && echo 0 || echo 1)" \
		"the list is answered, not only signed: aws s3 ls exits 0 through the airlock" \
		"aws s3 ls exited ${K6AWS[ls_exit]} and its last line was '${K6AWS[ls]}'"

	judge "$(contains "refusing a chunk-signed upload" "$K6_AWS_CHUNK" && contains "refusing a presigned URL" "$K6_AWS_PRESIGNED" && echo 0 || echo 1)" \
		"the chunk-signed upload and the presigned URL get an error that names them" \
		"the chunk-signed upload answered '$(printf '%s' "$K6_AWS_CHUNK" | head -c 120)…' and the presigned URL '$(printf '%s' "$K6_AWS_PRESIGNED" | head -c 120)…' (want each refused with an error that names it)"

	stop_stack

	# The negative control: a stack whose every proxy block reads as having no kind, so the azure
	# exchange and the AWS signing are both off.
	start_stack --no-kind
	stack_facts
	k6_azure
	k6_aws
	local control_federated
	control_federated="$(cat "$AIRLOCK_DIR/azure-identity-token")"
	control "with --no-kind (BROKER_AIRLOCK_TEST_NO_KIND=true) the token endpoint's client_assertion was $(shape "$K6_AZ_ASSERTION"), against the real federated token $(shape "$control_federated") and a placeholder: it opens 'agent-secrets-placeholder': $(contains agent-secrets-placeholder "$K6_AZ_ASSERTION" && echo yes || echo no)"
	# Each AWS call keeps the session token's placeholder in X-Amz-Security-Token when nothing
	# signs it again, so the proxy refuses it before it goes upstream: the stand-in records
	# nothing, and the refusal lines of this stack's brokers say why.
	local aws_host aws_left
	aws_host="${AIRLOCK_AWS_URL#https://}"
	aws_left="$(refusal_lines PLACEHOLDER_NOT_SWAPPED "$aws_host")"
	control "with --no-kind the stand-in AWS service recorded $K6_AWS_CALLS calls, and the brokers refused $aws_left calls to $aws_host 403 PLACEHOLDER_NOT_SWAPPED; sts said '${K6AWS[sts]}', s3 ls exited ${K6AWS[ls_exit]}"
	judge "$(contains "agent-secrets-placeholder" "$K6_AZ_ASSERTION" && [ "$K6_AWS_CALLS" = 0 ] && [ "$aws_left" -ge 3 ] && [ "${K6AWS[ls_exit]}" != 0 ] && echo 0 || echo 1)" \
		"negative control: with the exchange and the signing turned off the token endpoint gets the placeholder, and each AWS call is refused 403 PLACEHOLDER_NOT_SWAPPED and reaches no stand-in" \
		"the assertion the endpoint got was $(shape "$K6_AZ_ASSERTION") (want the grant's placeholder, which opens agent-secrets-placeholder); the stand-in AWS service recorded $K6_AWS_CALLS calls (want 0), and $aws_left calls to $aws_host were refused PLACEHOLDER_NOT_SWAPPED (want 3 or more), where the same runs with the kinds on are signed again and accepted"

	# The second stand-in AWS service: the control that the stand-in refuses a bad signature
	# after its key id and session token checks pass, so the acceptance above is the signature's.
	# In a stand-in build whose VerifySigV4 skips its last comparison, the wrong-key call is
	# answered 2xx and this fails.
	k6_second_aws
	control "the second stand-in AWS service recorded ${K6SECOND[records]} calls: the one signed with its pair was answered ${K6SECOND[signed_status]} and exited ${K6SECOND[signed_exit]} saying '${K6SECOND[signed_said]}', and the one with a wrong secret key was answered ${K6SECOND[wrong_status]} and exited ${K6SECOND[wrong_exit]}; its ${K6SECOND[refusal_count]} refusals: ${K6SECOND[refusals]}"
	judge "$([ "${K6SECOND[records]}" = 2 ] && [ "${K6SECOND[signed_status]}" = 200 ] && [ "${K6SECOND[signed_exit]}" = 0 ] && contains "arn:aws:iam" "${K6SECOND[signed_said]}" && [ "${K6SECOND[wrong_status]}" = 403 ] && [ "${K6SECOND[wrong_exit]}" != 0 ] && [ "${K6SECOND[refusal_count]}" = 1 ] && contains "the signature is" "${K6SECOND[refusals]}" && echo 0 || echo 1)" \
		"the second stand-in AWS service, which holds a second key pair with no session token and which no kind binds, records both of its calls, answers the call signed with its pair 2xx, and refuses the call with the wrong secret key 403 because the signature is not the one its pair makes" \
		"it recorded ${K6SECOND[records]} calls; the signed call was answered ${K6SECOND[signed_status]} and the wrong-key call ${K6SECOND[wrong_status]}; it refused ${K6SECOND[refusal_count]}: ${K6SECOND[refusals]}"

	stop_stack
}

###############################################################################################
# k7
###############################################################################################
#
# k7 drives the consumers as they run, against real services, with fake values: the refusal each
# real service answers is the evidence, not a failure. The consumer files come from
# sjawhar/dotfiles at main and nothing of them is rewritten (k7_dotfiles). The GitHub admin,
# gcloud and Google Workspace calls are the commands an agent runs for them, written into the
# steps here: each runs under agent-secrets itself, and gcloud with its login in a throwaway
# CLOUDSDK_CONFIG, as an admin gcloud command runs under its grant.
# One consumer speaks gRPC, which offers h2 alone in its TLS handshake: Pulumi's gcpkms secrets
# manager, whose Cloud KMS call the proxy carries in HTTP/2.
#
# The names are the consumers' own - GITHUB_ADMIN_PAT, and the operator's gcloud and Google
# Workspace logins, which k7's inputs name - and the hosts are the real ones, so this check needs
# a profiles file the stack's does not hold. That is one broker process of this check's own, on
# the stack's own database, store, certificate authority and stand-ins, with its profiles file,
# its own two ports and its own public URL changed - the same device k4's control uses for one
# setting, with the public URL changed too because this broker must serve the release as well as
# the proxy, and a session's proof is verified against the public URL it was signed for. Its
# whole environment is the first stack broker's own, read out of /proc/<pid>/environ, so nothing
# of the stack's settings is left out.
#
# The pod sets NO_PROXY=* and GIT_CONFIG_COUNT=9 with a GitHub credential helper. NO_PROXY=* is
# left exactly as it is: the CLI must drop it, and that it does is part of what this check
# proves. The git credential settings are cleared for the git step alone, which brings its own
# helper - the one that answers the placeholder - and the proxy in git's own configuration.

# k7_dotfiles fetches the consumer files from sjawhar/dotfiles at main and lays them out as a
# dotfiles checkout, which is how shims/gh finds the two scripts it runs (scripts/gh-agent-config
# and scripts/public-text-gate). gws/accounts.json holds the read tier the Google Workspace step
# reports.
k7_dotfiles() {
	local d="$WORK_DIR/k7/dotfiles" path
	mkdir -p "$d/scripts" "$d/shims" "$d/gws"
	for path in scripts/gh-agent-config scripts/public-text-gate shims/gh gws/accounts.json; do
		gh api "repos/sjawhar/dotfiles/contents/$path" -H 'Accept: application/vnd.github.raw' >"$d/$path" || {
			echo "airlock-check: sjawhar/dotfiles $path could not be fetched" >&2
			return 1
		}
	done
	chmod +x "$d/scripts/gh-agent-config" "$d/scripts/public-text-gate" "$d/shims/gh"
	K7_DOTFILES="$d"
	K7_FETCHED="$(cd "$d" && find . -type f | sed 's|^\./||' | sort | tr '\n' ' ')"
}

# k7_broker starts the one broker this check's steps run through: a broker like the stack's,
# whose environment comes from the running one, with the profiles file, the three listening
# addresses and the public URL replaced; the metrics port is one the system picks, since nothing
# here scrapes it and the stack's own brokers hold theirs.
k7_broker() {
	local d="$WORK_DIR/k7" api proxy
	api="$(python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')"
	proxy="$(python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')"
	K7_API_URL="http://127.0.0.1:$api"
	K7_PROXY_URL="http://127.0.0.1:$proxy"
	broker_env \
		BROKER_PROFILES_FILE="$d/profiles.json" \
		BROKER_LISTEN_ADDR="127.0.0.1:$api" \
		BROKER_PUBLIC_URL="$K7_API_URL" \
		BROKER_AIRLOCK_LISTEN_ADDR="127.0.0.1:$proxy" \
		BROKER_AIRLOCK_PROXY_URL="$K7_PROXY_URL" \
		BROKER_METRICS_LISTEN_ADDR="127.0.0.1:0"
	: >"$d/broker.log"
	( with_broker_env "$BROKER_BIN" ) >"$d/broker.log" 2>&1 &
	local started=$!
	EXTRA_BROKER_PIDS+=("$started")
	await "k7's broker" 120 "$d/broker.log" '"message":"airlock proxy listening"' "$started"
}


# k7_steps writes every consumer's own command into one file, as k3's and k4's steps are
# written, so one grant of the seven names covers them all.
k7_steps() {
	local d="$WORK_DIR/k7"
	{
		printf 'set -u\n'
		k7_consumer_preamble "$d"
	} >"$d/steps.sh"
	cat >>"$d/steps.sh" <<EOF
out="\$K7_CONSUMER_WORK_DIR/out"
say() { printf '%s\n' "\$1" >>"\$out"; }
say "no_proxy=\$NO_PROXY"
say "https_proxy=\$(printf '%s' "\$HTTPS_PROXY" | sed 's/:[^:@]*@/:***@/')"

# The slack_post poster (scripts/airlock/slack.ts): bun, the Bearer a Slack app's token goes in,
# and auth.test.
say "slack=\$(bun run $SCRIPT_DIR/airlock/slack.ts 2>&1 | tail -n 1 | tr -d '\r')"

# gh api under agent-secrets for GITHUB_ADMIN_PAT, with the real GitHub CLI first on PATH for this
# one step (k7_consumer_github_step, in the callable boundary).
k7_consumer_github_step

# pulumi preview of a program whose Tailscale provider calls tailscale:getDevices.
say "pulumi=\$(cd $d/pulumi && export PULUMI_CONFIG_PASSPHRASE= PULUMI_BACKEND_URL=file://$d/pulumi/state TAILSCALE_API_KEY="\$TAILSCALE_API_KEY" TAILSCALE_TAILNET=example.com && pulumi stack init k7 >/dev/null 2>&1; pulumi preview --stack k7 --non-interactive 2>&1 | grep -iE 'error|refus|unauthor|401|403' | head -n 1 | tr -d '\r')"

# The callable boundary runs the gRPC, gcloud and Google Workspace consumers after the generated
# setup above has established their work directory, dotfiles path and isolated configuration.
k7_consumer_google_steps

# git push --dry-run through scripts/airlock/credential-helper, which answers the placeholder,
# with the proxy in git's own configuration. It is given in the environment (GIT_CONFIG_*) rather
# than as a -c argument, since the proxy URL holds the proxy password and git's arguments are in
# /proc/<pid>/cmdline. GIT_CONFIG_COUNT=1 makes git read that one pair alone, so the pod's own
# GIT_CONFIG_* settings, which name a credential helper of the pod's that would answer in place
# of this one, are left out for this step.
rm -rf $d/repo
git init -q $d/repo
(cd $d/repo && git -c user.email=k7@airlock.test -c user.name=k7 commit -q --allow-empty -m k7)
say "git=\$(cd $d/repo && GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=http.proxy GIT_CONFIG_VALUE_0="\$HTTPS_PROXY" git -c credential.helper=$SCRIPT_DIR/airlock/credential-helper -c http.sslCAInfo="\$GIT_SSL_CAINFO" push --dry-run https://github.com/cybermonklabs/legion HEAD:refs/heads/k7-airlock-never-pushed 2>&1 | tail -n 2 | tr '\n' ' ')"

# The plain clients to S, each with the fake value in the header it uses, and none with it in its
# command line: curl reads the header on standard input. wget reads a header from no pipe, so it
# gets a startup file of its own, removed when this script exits: the airlock's WGETRC first,
# whose ca_certificate is the one way the proxy's certificate reaches wget
# (cmd/cipher/airlock.go, wgetrcFor), then the header. --config would read that file in
# place of WGETRC and lose the certificate, so the file is named as WGETRC for this one call.
# wget follows a redirect by default and sends a startup file's header on every request, to the
# origin a Location names too (wget 1.21.4, measured 2026-11-17), so --max-redirect=0 holds it to
# the one request to S. -S prints that answer's status, which is what this records, and a 3xx is
# recorded as refused. wget still reads a refused redirect's body to its end, and a chunked body
# that never ends held it past 50 s (measured 2026-11-17), so timeout 30 bounds the call and the
# line says when it ran out. curl follows no redirect here, and -q first keeps a .curlrc from
# making it follow one. Python's urllib opener follows a redirect and keeps the Authorization
# header on it (Python 3.12, measured 2026-11-17), so its line builds an opener whose redirect
# handler refuses, and records a 3xx as refused, as the wget line does.
say "curl=\$(printf 'Authorization: Bearer %s' "\$AIRLOCK_DEMO_TOKEN" | curl -q -sS -o /dev/null -w %{http_code} -H @- $AIRLOCK_S_URL/curl)"
wgetrc=\$(mktemp)
trap 'rm -f "\$wgetrc"' EXIT
{ if [ -n "\${WGETRC:-}" ]; then cat "\$WGETRC"; echo; fi; printf 'header = Authorization: Bearer %s\n' "\$AIRLOCK_DEMO_TOKEN"; } >"\$wgetrc"
say "wget=\$({ WGETRC="\$wgetrc" timeout 30 wget -q -S --max-redirect=0 -O /dev/null $AIRLOCK_S_URL/wget 2>&1; echo "wget-exit=\$?"; } | awk '/^  HTTP\\//{s=\$2} /^wget-exit=/{e=substr(\$0, 11)} END{if (s ~ /^3/) s = s " refused: a redirect, which a request that carries a credential does not follow"; if (s == "") s = "failed"; if (e == 124) s = s " (wget stopped after 30 s)"; print s}')"
say "node=\$(node -e 'fetch("$AIRLOCK_S_URL/node", {headers: {authorization: "Bearer " + process.env.AIRLOCK_DEMO_TOKEN}}).then(r => console.log(r.status)).catch(e => console.log("error", e.message))')"
say "bun=\$(bun -e 'const r = await fetch("$AIRLOCK_S_URL/bun", {headers: {authorization: "Bearer " + process.env.AIRLOCK_DEMO_TOKEN}}); console.log(r.status)' 2>&1 | tail -n 1)"
say "urllib=\$($AIRLOCK_BOTO3_PYTHON -c 'import os, urllib.error, urllib.request
class Refuse(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args):
        return None
request = urllib.request.Request("$AIRLOCK_S_URL/urllib", headers={"Authorization": "Bearer " + os.environ["AIRLOCK_DEMO_TOKEN"]})
try:
    print(urllib.request.build_opener(Refuse).open(request).status)
except urllib.error.HTTPError as error:
    print(f"{error.code} refused: a redirect, which a request that carries a credential does not follow" if 300 <= error.code < 400 else error.code)' 2>&1 | tail -n 1)"
say "requests=\$($AIRLOCK_BOTO3_PYTHON -c 'import os, requests
print(requests.get("$AIRLOCK_S_URL/requests", headers={"Authorization": "Bearer " + os.environ["AIRLOCK_DEMO_TOKEN"]}).status_code)' 2>&1 | tail -n 1)"
say "go=\$($STANDS_BIN probe-bound)"
EOF
}

check_k7() {
	expect "each call went through the proxy port and the broker swapped the fake value into the header that the consumer uses"
	expect "each real service refused the fake value"
	expect "a gRPC client reaches its service through the proxy in HTTP/2, and the service refuses the fake value"
	expect "the gh consumer's own call is refused the fake value by GitHub"
	expect "S got the fake value in each client's call"
	expect "the command's exit status is 3, and the CLI's stderr has the line agent-secrets: NAME from …"
	expect "the negative control, the same runs with a CLI built without the change, shows no call through the proxy port"

	# The real GitHub CLI, which the gh step puts first on PATH: LEGION_GH_PATH names it, and
	# it is the binary Legion's own gh wrapper runs. Without it there is no gh in this pod the
	# consumer's admin line can run - the wrapper refuses a call whose GH_TOKEN is set - and
	# the step is refused rather than run against the wrapper, whose refusal is its own and
	# says nothing about the airlock.
	K7_GH_DIR=""
	if [ -n "${LEGION_GH_PATH:-}" ] && [ -x "${LEGION_GH_PATH:-}" ]; then
		K7_GH_DIR="$(dirname "$LEGION_GH_PATH")"
	fi

	k6_clients
	start_stack
	stack_facts
	k7_dotfiles
	observe "the consumers, fetched from sjawhar/dotfiles at main: $K7_FETCHED"
	K7_CONSUMER_WORK_DIR="$WORK_DIR/k7" \
		k7_consumer_add_fixtures
	k7_broker
	observe "this check's own broker is the stack's first broker's environment with its profiles file, its two ports and its public URL changed: api $K7_API_URL, proxy $K7_PROXY_URL, names $(jq -c '[.profiles[].name]' "$WORK_DIR/k7/profiles.json")"

	K7_CONSUMER_WORK_DIR="$WORK_DIR/k7" \
		k7_consumer_write_pulumi
	k7_steps

	local s_before
	s_before="$(records_count s)"
	# AGENT_SECRETS_URL is this check's own broker, the one whose profiles name the real hosts.
	AGENT_SECRETS_URL="$K7_API_URL" agent-secrets \
		SLACK_BOT_TOKEN GITHUB_ADMIN_PAT TAILSCALE_API_KEY "$AIRLOCK_GCLOUD_SECRET" \
		"$AIRLOCK_GWS_READ_SECRET" "$AIRLOCK_GWS_ADMIN_SECRET" AIRLOCK_DEMO_TOKEN \
		--ttl 30m -- bash "$WORK_DIR/k7/steps.sh" >"$WORK_DIR/k7/cli.out" 2>"$WORK_DIR/k7/cli.err"
	declare -A K7
	local key value
	while IFS='=' read -r key value; do K7["$key"]="$value"; done <"$WORK_DIR/k7/out"

	local recs clients=0 with_value=0 client demo_value
	recs="$(records s | jq --argjson n "$s_before" '.[$n:]')"
	demo_value="$(stored_value "${STACK_SECRETS_PREFIX}airlock-demo-token")"
	for client in curl wget node bun urllib requests go; do
		clients=$((clients + 1))
		if [ "$(printf '%s' "$recs" | jq --arg p "/$client" --arg v "$demo_value" '[.[] | select(.path == $p) | .header.Authorization // [] | .[] | select(contains($v))] | length')" -ge 1 ]; then
			with_value=$((with_value + 1))
		fi
	done

	observe "the command's inherited NO_PROXY was ${K7[no_proxy]-} — the pod's own NO_PROXY is '*', which the CLI had to drop — and its proxy was ${K7[https_proxy]-}"
	observe "slack.com/api/auth.test under bun: ${K7[slack]-}"
	observe "gh api user under agent-secrets for GITHUB_ADMIN_PAT: ${K7[gh]-}"
	observe "pulumi preview with the Tailscale provider calling tailscale:getDevices: ${K7[pulumi]-}"
	observe "pulumi stack init with a gcpkms secrets provider, Cloud KMS over gRPC: ${K7[kms]-}"
	observe "gcloud projects list with the operator's gcloud login as its access token file, in a throwaway CLOUDSDK_CONFIG: ${K7[gcloud]-}"
	observe "the gws operator read tier (${K7[gws_tier]-}) answered: ${K7[gws]-}"
	observe "the google-workspace skill's Bearer call to admin.googleapis.com answered: ${K7[admin]-}"
	observe "git push --dry-run through a credential helper answering the placeholder: ${K7[git]-}"
	observe "the plain clients to S: curl ${K7[curl]-}, wget ${K7[wget]-}, node ${K7[node]-}, bun ${K7[bun]-}, python urllib ${K7[urllib]-}, python requests ${K7[requests]-}, Go ${K7[go]-}"
	observe "S recorded the fake value (by SHA-256 and length, $(shape "$demo_value")) in $with_value of the $clients plain clients' calls"

	# Each real service's own refusal. A fake value is refused with an unauthorized answer, and
	# what each consumer printed of it is what is read here.
	local refused=0 refusals=""
	k7_refused() { # <label> <what it said>
		if printf '%s' "$2" | grep -qiE '401|403|not_authed|invalid_auth|unauthor|unauthenticated|forbid|refus|denied|Bad credentials|permission|authentication failed|invalid username or token|CREDENTIALS_MISSING|ACCESS_TOKEN_TYPE'; then
			refused=$((refused + 1))
			refusals="$refusals; $1 refused it"
		else
			refusals="$refusals; $1 did NOT refuse it: $2"
		fi
	}
	k7_refused slack "${K7[slack]-}"
	k7_refused tailscale "${K7[pulumi]-}"
	k7_refused gcloud "${K7[gcloud]-}"
	k7_refused gws "${K7[gws]-}"
	k7_refused admin "${K7[admin]-}"
	k7_refused git "${K7[git]-}"
	k7_refused kms "${K7[kms]-}"
	observe "what each real service answered the fake value${refusals}"
	observe "the gh consumer: $(printf '%s' "${K7[gh]-}" | head -c 300)"

	# What the broker logged: one line per call to a bound host - its call line, or the refusal
	# record a refused call writes in that line's place (bound_call_lines) - which is how a call
	# is known to have reached the proxy port and been decided there. Each number comes from one
	# read of the log.
	local calls logged swapped
	calls="$(bound_call_lines "$WORK_DIR/k7/broker.log")"
	logged="$(jq -s length <<<"$calls")"
	swapped="$(jq -s '[.[] | select(.message == "airlock proxy: a call to a bound host" and .allowed == true)] | length' <<<"$calls")"
	observe "this check's broker logged $logged calls to a bound host, $swapped of them allowed; the last three:"
	observe "$(printf '%s' "$calls" | tail -n 3 | sed 's/^/    /')"
	judge "$([ "$logged" -ge 10 ] && [ "$swapped" = "$logged" ] && echo 0 || echo 1)" \
		"each call went through the proxy port and the broker swapped the fake value into the header that the consumer uses" \
		"$logged calls to a bound host were logged by the proxy, $swapped of them allowed and so swapped"
	judge "$([ "$refused" = 7 ] && echo 0 || echo 1)" \
		"each real service refused the fake value" "$refused of the 7 consumers that can run in this pod refused it${refusals}"
	# The gRPC consumer on its own: Google's own UNAUTHENTICATED is what says the call crossed the
	# proxy in HTTP/2 and reached Cloud KMS, and a call line naming its host is the proxy's side.
	local kms_calls
	kms_calls="$(jq -s '[.[] | select(.host == "cloudkms.googleapis.com")] | length' <<<"$calls")"
	# The absence of "application protocol" is the property, not a weak anchor: a broker that broke
	# HTTP/2 would make the gRPC client fail ALPN with an "application protocol" error before it
	# ever reached Cloud KMS. Its absence, with the positive "unauthenticated" and a logged call,
	# is how the check reads that HTTP/2 was negotiated end to end.
	judge "$(printf '%s' "${K7[kms]-}" | grep -qi 'unauthenticated' && ! printf '%s' "${K7[kms]-}" | grep -qi 'application protocol' && [ "$kms_calls" -ge 1 ] && echo 0 || echo 1)" \
		"a gRPC client reaches its service through the proxy in HTTP/2, and the service refuses the fake value" \
		"Pulumi's gcpkms secrets manager said '$(printf '%s' "${K7[kms]-}" | head -c 300)', and the proxy logged $kms_calls calls to cloudkms.googleapis.com"
	if [ -z "$K7_GH_DIR" ]; then
		fail "the gh consumer's own call is refused the fake value by GitHub — this step did not run: LEGION_GH_PATH is unset or names no runnable file in this pod, and it names the only real GitHub CLI here. The step's command is \`agent-secrets GITHUB_ADMIN_PAT --reason … -- sh -c 'GH_TOKEN=\$GITHUB_ADMIN_PAT exec gh …'\`, and the other gh on PATH is Legion's wrapper, which refuses a call whose GH_TOKEN is set and refuses to run at all when another gh is ahead of it, so running the step against the wrapper would prove nothing about the airlock. Set LEGION_GH_PATH to the real CLI and run this check again."
	else
		judge "$(printf '%s' "${K7[gh]-}" | grep -qiE '401|403|Bad credentials|unauthor' && echo 0 || echo 1)" \
			"the gh consumer's own call is refused the fake value by GitHub" \
			"the real GitHub CLI ($LEGION_GH_PATH, first on PATH for that one step) sent the fake admin token to api.github.com through the proxy and GitHub answered: $(printf '%s' "${K7[gh]-}" | head -c 200)"
	fi
	judge "$([ "$with_value" = "$clients" ] && echo 0 || echo 1)" \
		"S got the fake value in each client's call" \
		"$with_value of $clients clients' calls carried it, compared by SHA-256 and length"

	# The exit status and the CLI's line: a command that exits 3 under the CLI leaves this run
	# with 3, and the CLI says on its stderr which source each name came from.
	local three_status three_err
	AGENT_SECRETS_URL="$K7_API_URL" agent-secrets GITHUB_ADMIN_PAT --ttl 5m -- sh -c 'exit 3' >/dev/null 2>"$WORK_DIR/k7/three.err"
	three_status=$?
	three_err="$(tr '\n' ' ' <"$WORK_DIR/k7/three.err")"
	observe "a command that exits 3 under the CLI left exit $three_status, and the CLI's stderr said: $three_err"
	judge "$([ "$three_status" = 3 ] && contains "agent-secrets: GITHUB_ADMIN_PAT from " "$three_err" && echo 0 || echo 1)" \
		"the command's exit status is 3, and the CLI's stderr has the line agent-secrets: NAME from …" \
		"it exited $three_status and said '$three_err'"

	# The negative control: the agent-secrets of the release before the airlock, which is the
	# CLI without this change. It is released no value for a name whose profile has a proxy
	# block, so nothing it runs reaches the proxy port. A control that counts zero calls proves
	# that only once the binary it counted them for is the one it asked for: a missing or wrong
	# binary makes no call either, so fetch_old_cli's answer is part of the verdict.
	local old_cli_ok=0
	fetch_old_cli || old_cli_ok=1
	local before_control after_control old_out old_status
	before_control="$(bound_call_lines "$WORK_DIR/k7/broker.log" | wc -l)"
	old_out="$(AGENT_SECRETS_URL="$K7_API_URL" "$OLD_CLI" GITHUB_ADMIN_PAT -- sh -c 'curl -sS -o /dev/null -w %{http_code} https://api.github.com/user' 2>&1)"
	old_status=$?
	after_control="$(bound_call_lines "$WORK_DIR/k7/broker.log" | wc -l)"
	control "the agent-secrets of $OLD_CLI_TAG, the release before the airlock and so the CLI without this change, exited $old_status saying '$(printf '%s' "$old_out" | tail -n 1)'"
	control "the proxy logged $((after_control - before_control)) further calls to a bound host while it ran"
	judge "$([ "$old_cli_ok" = 0 ] && [ "$((after_control - before_control))" = 0 ] && echo 0 || echo 1)" \
		"negative control: with a CLI built without the change no call goes through the proxy port" \
		"${OLD_CLI_WHY:+$OLD_CLI_WHY; }$((after_control - before_control)) calls to a bound host were logged while the agent-secrets of $OLD_CLI_TAG ran, which exited $old_status"

	stop_stack
}

###############################################################################################
# k8
###############################################################################################
#
# The rules test of the head's profiles file and of ten changed copies, each run as the
# deployment's pull-request check runs it (`envoy-broker profiles check <copy> --against <head's
# file>`), the local broker on each failing copy, and the policy load's refusal of a judge tag of
# 1800 seconds. The pull request half of k8 (a draft pull request whose required check goes red)
# is the deployment's and is not run here.

# k8_start_broker starts a broker on one profiles file and answers 0 when it refused to start
# for that file, leaving why in K8_WHY. Its environment is the running stack broker's own, with
# the profiles file and the three addresses replaced, so that what it refuses on is the profiles
# file and not a setting this function left out. It runs in the foreground under timeout,
# because what it is here to see is a broker that never comes up.
k8_start_broker() { # <profiles file> <label>
	local log="$WORK_DIR/k8-broker-$2.log"
	broker_env \
		BROKER_PUBLIC_URL="http://127.0.0.1:0" \
		BROKER_LISTEN_ADDR="127.0.0.1:0" \
		BROKER_PROFILES_FILE="$1" \
		BROKER_AIRLOCK_LISTEN_ADDR="127.0.0.1:0" \
		BROKER_METRICS_LISTEN_ADDR="127.0.0.1:0"
	( with_broker_env timeout 25 "$BROKER_BIN" ) >"$log" 2>&1
	local status=$?
	K8_WHY="$(grep -m1 'fatal' "$log" | tr '\n' ' ')"
	# The refusal has to be about this file: a broker that died of a setting this function did
	# not give it would otherwise read as a refusal of the profiles file.
	[ "$status" -ne 0 ] && [ -n "$K8_WHY" ] && contains "$1" "$K8_WHY"
}

# k8_copies writes the ten copies of <head's file> into <directory>, one file each, named for
# what it changes, and the list of them, one "<label> <what it should do>" a line, where the
# second word is fail or pass: a kind with no host, a kind's new host, header and variable, a
# kind's changed fields, a new row of a kind's allow, a new row inside its kind's allow of a
# profile's allow, a kind removed, a kind naming a secret, and a file that is not JSON. The fields
# copy is listed as unknown: a kind's fields are the named-fields slice's, which this head does
# not build, so the copy is not written.
k8_copies() { # <head's file> <directory>
	local head="$1" d="$2"
	jq '(.kinds[] | select(.name == "stest") | .hosts) = []' "$head" >"$d/no-host.json"
	jq '(.kinds[] | select(.name == "stest") | .hosts) += ["extra.airlock.test"]' "$head" >"$d/new-host.json"
	jq '(.kinds[] | select(.name == "scoped") | .headers) += ["x-extra"]' "$head" >"$d/new-header.json"
	jq '(.kinds[] | select(.name == "stest") | .variables) = ["STEST_TOKEN"]' "$head" >"$d/new-variable.json"
	jq '(.kinds[] | select(.name == "scoped") | .allow) += [{"method": "GET", "pathPrefix": "/other"}]' "$head" >"$d/kind-allow-row.json"
	jq '(.profiles[] | select(.name == "AIRLOCK_ANTHROPIC_NARROWED") | .allow) += [{"method": "GET", "pathPrefix": "/v1/organizations/workspaces"}]' "$head" >"$d/profile-allow-row.json"
	jq '.kinds |= map(select(.name != "aws"))' "$head" >"$d/removed-kind.json"
	jq '(.kinds[] | select(.name == "stest")) += {"secrets": ["AIRLOCK_DEMO_TOKEN"]}' "$head" >"$d/names-secret.json"
	printf '{"kinds": [{"name": "a", "hosts": ["a.airlock.test"], "ways": ["swap"]},], "profiles": []}' >"$d/malformed.json"
	printf '%s\n' "no-host fail" "new-host pass" "new-header pass" "new-variable pass" "fields unknown" \
		"kind-allow-row pass" "profile-allow-row pass" "removed-kind leaves" "names-secret fail" "malformed fail" >"$d/copies"
}

# k8_names is what each copy's output must name, beside its exit status: the kind and the value a
# passing copy adds or removes, and the kind or the field a failing copy is refused for.
k8_names() { # <label>
	case "$1" in
	no-host) printf '%s\n' 'stest' 'no host' ;;
	new-host) printf '%s\n' 'kind "stest"' 'extra.airlock.test' ;;
	new-header) printf '%s\n' 'kind "scoped"' 'x-extra' ;;
	new-variable) printf '%s\n' 'kind "stest"' 'STEST_TOKEN' ;;
	kind-allow-row) printf '%s\n' 'kind "scoped"' 'GET /other' ;;
	profile-allow-row) printf '%s\n' 'profile "AIRLOCK_ANTHROPIC_NARROWED"' 'GET /v1/organizations/workspaces' ;;
	removed-kind) printf '%s\n' 'kind "aws"' 'removed' ;;
	names-secret) printf '%s\n' 'secrets' ;;
	esac
}

# k8_named answers 0 when <output> holds every line k8_names gives <label>, and for the malformed
# copy when it names the position of the parse error.
k8_named() { # <label> <output>
	local want
	if [ "$1" = malformed ]; then
		printf '%s' "$2" | grep -qE '(byte|offset|line|column|character) [0-9]+'
		return
	fi
	while IFS= read -r want; do
		[ -n "$want" ] || continue
		contains "$want" "$2" || return 1
	done < <(k8_names "$1")
}

# k8_untagged writes a store list (k47_secrets_json's form) with the proxy tag taken off each
# secret whose proxy tag names <kind>, and every other secret and tag as it was: the store as it is
# once a reviewed retag has moved that kind's secrets off it, which is the order a removal of the
# kind takes.
k8_untagged() { # <store list> <kind>
	jq --arg k "$2" 'map(if .annotations.proxy == $k then .annotations |= del(.proxy) else . end)' "$1"
}

# k8_reread makes every broker of the stack read one name again, as reread_all does, and answers
# in K8_ANSWERS what each said, "<code> served=<served> reason=<reason>" per broker joined by
# "; ", so a refused name is read off the broker's own answer and not off a reread that ended.
# judge-check.sh's k8j_reread is the same call.
k8_reread() { # <name>
	local addr proof body code
	K8_ANSWERS=""
	for addr in "${BROKER_API_ADDRS[@]}"; do
		proof="$(agent-secrets sign --method POST --url "$AGENT_SECRETS_URL/v1/secrets/$1/reread")" || { K8_ANSWERS="$K8_ANSWERS; unsigned"; continue; }
		code="$(curl -sS -o "$WORK_DIR/k8-reread.json" -w '%{http_code}' -X POST -H "Proof: $proof" "http://$addr/v1/secrets/$1/reread" 2>/dev/null)"
		body="$(jq -r '"served=\(.served) reason=\(.reason // "")"' "$WORK_DIR/k8-reread.json" 2>/dev/null)"
		K8_ANSWERS="${K8_ANSWERS:+$K8_ANSWERS; }$code $body"
	done
}

check_k8() {
	expect "the copy with a kind that has no host and the copy that names a secret each fail with an error that names the kind or the field"
	expect "the malformed copy fails with the position of the parse error"
	expect "the local broker refuses to start on each failing copy"
	expect "the copies with a new host, a new header, a new variable, changed fields and new allow rows pass and list what they add, of the kind or of the profile (changed fields: the named-fields slice's, not judged here)"
	expect "the copy without a kind passes and lists the kind leaving the airlock, once no secret carries that kind's proxy tag; while one does, the rules test refuses the copy and names that secret, which would otherwise leave the proxy"
	expect "the policy load refuses the judge tag of 1800 seconds and names the key"
	expect "the negative control: on the head's file the test passes and lists no change, and the same policy load with a judge tag of 900 seconds refuses nothing"

	start_stack
	stack_facts
	local d="$WORK_DIR/k8"
	mkdir -p "$d"
	cp "$AIRLOCK_PROFILES_FILE" "$d/head.json"
	k8_copies "$d/head.json" "$d"
	# Each run reads the store's tags (--secrets-json), as the rules test reads them for a
	# deployment's change: a profile with no proxy string takes its kind from its source's proxy
	# tag, which the file alone does not hold, so a run without them answers 3 (UNKNOWN) for a
	# file it refuses nothing of. The second list is the same store with the aws kind's secrets
	# untagged, the retag that a removal of that kind comes with.
	k47_secrets_json "$AIRLOCK_FAKE_SECRETS_FILE" >"$d/secrets.json"
	k8_untagged "$d/secrets.json" aws >"$d/secrets-aws-untagged.json"
	local aws_tagged
	aws_tagged="$(jq -r '[.[] | select(.annotations.proxy == "aws") | .name] | length' "$d/secrets.json")"

	local label want status out failing=()
	while read -r label want <&3; do
		if [ "$want" = unknown ]; then
			unknown "the $label copy: a kind's fields are the named-fields slice's (s-fields), which this head does not build, so the copy was not run"
			continue
		fi
		out="$("$BROKER_BIN" profiles check "$d/$label.json" --against "$d/head.json" --secrets-json "$d/secrets.json" 2>&1)"
		status=$?
		observe "$label: exit $status; $(printf '%s' "$out" | grep -vE '^profiles check: (compared against|.* is valid:)' | head -n 4 | tr '\n' ' ')"
		case "$want" in
		fail)
			failing+=("$label")
			judge "$([ "$status" -ne 0 ] && k8_named "$label" "$out" && echo 0 || echo 1)" \
				"the $label copy fails with an error that names $([ "$label" = malformed ] && echo "the position of the parse error" || k8_names "$label" | tr '\n' ' ')" \
				"it exited $status saying '$(printf '%s' "$out" | tail -n 1)'"
			;;
		pass)
			judge "$([ "$status" = 0 ] && k8_named "$label" "$out" && echo 0 || echo 1)" \
				"the $label copy passes and lists what it changes ($(k8_names "$label" | tr '\n' ' '))" \
				"it exited $status; its lines naming the change: $(printf '%s' "$out" | grep -F "$(k8_names "$label" | tail -n 1)" | head -n 2 | tr '\n' ' ')"
			;;
		leaves)
			# The store as it is: a secret still tagged proxy=aws would name no kind of the copy,
			# so the rules test refuses the copy as the policy load would refuse that secret.
			judge "$([ "$aws_tagged" -ge 1 ] 2>/dev/null && [ "$status" = 1 ] && contains "proxy-kind-unknown" "$out" && printf '%s' "$out" | grep -qiE 'airlock[-_]aws[-_]key' && echo 0 || echo 1)" \
				"the $label copy, while a secret carries the aws kind's proxy tag, is refused, naming that secret and proxy-kind-unknown" \
				"$aws_tagged secret(s) in the store list carry proxy=aws; it exited $status saying '$(printf '%s' "$out" | grep -iE 'proxy-kind-unknown|leaving' | head -n 2 | tr '\n' ' ')'"
			out="$("$BROKER_BIN" profiles check "$d/$label.json" --against "$d/head.json" --secrets-json "$d/secrets-aws-untagged.json" 2>&1)"
			status=$?
			observe "$label, with the aws kind's secrets untagged: exit $status; $(printf '%s' "$out" | grep -F 'kind "aws"' | head -n 2 | tr '\n' ' ')"
			judge "$([ "$status" = 0 ] && k8_named "$label" "$out" && echo 0 || echo 1)" \
				"the $label copy, once no secret carries that kind's proxy tag, passes and lists the kind leaving the airlock ($(k8_names "$label" | tr '\n' ' '))" \
				"it exited $status; its lines naming the change: $(printf '%s' "$out" | grep -F 'kind "aws"' | head -n 2 | tr '\n' ' ')"
			;;
		esac
	done 3<"$d/copies"

	local refusals=0 why=""
	for label in "${failing[@]}"; do
		if k8_start_broker "$d/$label.json" "$label"; then
			refusals=$((refusals + 1))
			why="$why; $label: $K8_WHY"
		else
			why="$why; $label: the broker did not refuse to start for this file"
		fi
	done
	observe "a local broker on each failing copy${why}"
	judge "$([ "${#failing[@]}" -gt 0 ] && [ "$refusals" = "${#failing[@]}" ] && echo 0 || echo 1)" \
		"the local broker refuses to start on each failing copy" "$refusals of ${#failing[@]} refused, naming the file"

	out="$("$BROKER_BIN" profiles check "$d/head.json" --against "$d/head.json" --secrets-json "$d/secrets.json" 2>&1)"
	status=$?
	control "the head's file against itself exited $status saying: $(printf '%s' "$out" | tail -n 1)"
	judge "$([ "$status" = 0 ] && contains "nothing changed" "$out" && echo 0 || echo 1)" \
		"negative control: on the head's file the test passes and lists no change" \
		"it exited $status and said '$(printf '%s' "$out" | tail -n 1)'"

	# The judge tag: a key of the approver's with a judge tag of 1800 seconds and no proxy tag, a
	# stored value the broker serves by its own name, read at the next policy load.
	local name="AIRLOCK_K8_TOKEN" suffix="airlock-k8-token" over_answers over_line ok=0
	add_fake_secret "${STACK_SECRETS_PREFIX}$suffix" "$(jq -nc --arg o "$AIRLOCK_APPROVER" '{owner: $o, tier: "human", judge: "1800"}')" "the-k8-test-value"
	k8_reread "$name"
	over_answers="$K8_ANSWERS"
	over_line="$(jq -Rc --arg s "$suffix" 'fromjson? | select(.message == "agent secret policy refused" and ((.name // "") | endswith($s)))' "$AIRLOCK_DIR"/broker*.log 2>/dev/null | tail -n 1)"
	observe "a key with a judge tag of 1800 seconds, after a reread: the brokers answered [$over_answers]; the broker's log: ${over_line:-no refusal line for it}"
	case "$over_answers" in "" | *unsigned* | *served=true*) ok=1 ;; esac
	printf '%s' "$over_answers" | tr ';' '\n' | grep -qv 'reason=judge-out-of-range' && ok=1
	judge "$([ "$ok" = 0 ] && contains '"reason":"judge-out-of-range"' "$over_line" && contains "$suffix" "$over_line" && echo 0 || echo 1)" \
		"the policy load refuses the judge tag of 1800 seconds and names the key" \
		"rereads [$over_answers]; log line ${over_line:-none}"

	set_fake_tags "${STACK_SECRETS_PREFIX}$suffix" "$(jq -nc --arg o "$AIRLOCK_APPROVER" '{owner: $o, tier: "human", judge: "900"}')"
	k8_reread "$name"
	ok=0
	case "$K8_ANSWERS" in "" | *unsigned* | *served=false*) ok=1 ;; esac
	printf '%s' "$K8_ANSWERS" | tr ';' '\n' | grep -qv 'served=true' && ok=1
	control "the same key retagged with a judge of 900 seconds, after a reread: the brokers answered [$K8_ANSWERS]"
	judge "$ok" "negative control: the same policy load with a judge tag of 900 seconds refuses nothing" \
		"rereads [$K8_ANSWERS], where at 1800 they answered [$over_answers]"

	stop_stack
}

###############################################################################################
# k9
###############################################################################################
#
# A ten-minute load, and everything else this check asks of one stack, takes about twenty-five
# minutes end to end: two stack starts of about three minutes each, ten minutes of connections,
# a minute of baseline through a plain tunnel, two ninety-second gaps (one here, one in the
# control) and the upload, the clone and the two broker steps between them.
#
# The figures the check names are the broker container's own limits, so they are read of the
# broker processes and of nothing else: each process's CPU time from utime and stime in
# /proc/<pid>/stat, against the wall time of the same window, and its peak memory from
# /proc/<pid>/status' VmRSS, sampled every two seconds across the ten minutes and kept at its
# highest. Not ru_maxrss: getrusage answers for the calling process and its waited-for children,
# and these two are dev-airlock.sh's children and not this script's, so no getrusage of this run
# can see them. Not VmHWM either, which is the kernel's own high-water mark and would need no
# sampling: a Legion pod runs gVisor, whose /proc/<pid>/status has VmSize, VmRSS and VmData and
# no VmHWM at all. The sampled maximum is what is reported, and a peak between two samples is
# what it can miss.
#
# GOMAXPROCS=1 for each broker: dev-airlock.sh has no option for it and its start_broker names
# no GOMAXPROCS, so the brokers inherit it from the environment this script starts the stack
# with, which is where this check sets it. Each broker's /proc/<pid>/environ is read back to
# show that it arrived.

# k9_cpu answers one process's CPU time in seconds, utime and stime of /proc/<pid>/stat added.
k9_cpu() { # <pid>
	awk -v ticks="$(getconf CLK_TCK)" '{ sub(/^[0-9]+ \(.*\) /, ""); print ($12 + $13) / ticks }' "/proc/$1/stat" 2>/dev/null || echo 0
}

# k9_sample_rss appends one line per broker process every two seconds, which is how this check
# reads a peak resident set on a kernel whose /proc/<pid>/status has no VmHWM (see the block
# above). It runs until it is killed.
k9_sample_rss() { # <file> <pid>...
	local file="$1" pid
	shift
	while :; do
		for pid in "$@"; do
			awk -v p="$pid" '/^VmRSS:/ { print p, $2 }' "/proc/$pid/status" 2>/dev/null >>"$file"
		done
		sleep 2
	done
}

# k9_peak_kb is the highest resident set the sampler saw for one process, in kilobytes.
k9_peak_kb() { # <file> <pid>
	awk -v p="$2" '$1 == p && $2 > max { max = $2 } END { print max + 0 }' "$1" 2>/dev/null
}

check_k9() {
	expect "no call fails in the ten minutes of 50 connections a second"
	expect "the proxy adds at most 100 milliseconds at the median, against the same calls through a plain tunnel"
	expect "each broker process's CPU time is at most half its wall time and its peak memory at most 1 GiB"
	expect "the 50 MiB upload, the answer with a 90-second gap in it and the shallow clone finish"
	expect "the WebSocket upgrade reaches S with the real value in its header, and frames pass both ways"
	expect "the stream on the stopped process ends after the drain, and the next call succeeds"
	expect "the negative control, a stack with the proxy's idle limit at 60 seconds, cuts the answer with the 90-second gap"

	# Each broker with GOMAXPROCS=1: dev-airlock.sh has no option for it, so it is exported into
	# the environment the stack is started with and each broker inherits it.
	export GOMAXPROCS=1
	start_stack
	stack_facts
	# This check's own files. The load driver is a mode of dev-airlock-stands, which
	# dev-broker.sh built with the stack's other binaries, so there is nothing to build here.
	mkdir -p "$WORK_DIR/k9"

	local pids pid environs=""
	mapfile -t pids < <(sed -n 's/^  broker [0-9]*:.*pid \([0-9]*\),.*/\1/p' "$STACK_LOG")
	for pid in "${pids[@]}"; do
		environs="$environs $pid:$(tr '\0' '\n' <"/proc/$pid/environ" | grep -c '^GOMAXPROCS=1$')"
	done
	observe "the stack's broker processes are ${pids[*]}, and each one's /proc/<pid>/environ holds GOMAXPROCS=1 (pid:matches)$environs"

	release AIRLOCK_DEMO_TOKEN 60m
	local proxy demo_value
	proxy="$(proxy_for "${AIRLOCK_PROXY_URL##*://}")"
	demo_value="$(stored_value "${STACK_SECRETS_PREFIX}airlock-demo-token")"

	# The baseline: the same calls through a tunnel that reads nothing of what it carries. It is
	# a minute rather than ten: a median needs no more, and the ten minutes are the load's.
	"$STANDS_BIN" load tunnel --listen 127.0.0.1:0 --to 127.0.0.2:443 >"$WORK_DIR/k9/tunnel.addr" 2>"$WORK_DIR/k9/tunnel.log" &
	HELPER_PIDS+=($!)
	local plain=""
	for _ in $(seq 1 40); do
		plain="$(head -n 1 "$WORK_DIR/k9/tunnel.addr" 2>/dev/null)"
		[ -z "$plain" ] || break
		sleep 0.25
	done
	echo "airlock-check: k9 is measuring the baseline through a plain tunnel for 60 seconds..." >&2
	"$STANDS_BIN" load load --proxy-file <(printf 'http://%s\n' "$plain") --host s.airlock.test --ca "$AIRLOCK_CA_CERT_FILE" \
		--rate 50 --seconds 60 --out "$WORK_DIR/k9/baseline.json" >/dev/null 2>"$WORK_DIR/k9/baseline.err"

	local before=() after=() wall_before wall_after rss="$WORK_DIR/k9/rss.log"
	: >"$rss"
	k9_sample_rss "$rss" "${pids[@]}" &
	local sampler=$!
	HELPER_PIDS+=("$sampler")
	for pid in "${pids[@]}"; do before+=("$(k9_cpu "$pid")"); done
	wall_before="$(date +%s.%N)"
	echo "airlock-check: k9 is opening 50 connections a second through the airlock for ten minutes..." >&2
	"$STANDS_BIN" load load --proxy-file <(printf '%s\n' "$proxy") --host s.airlock.test --ca "$AIRLOCK_CA_CERT_FILE" \
		--header-file <(printf '%s\n' "$PLACEHOLDER") --rate 50 --seconds 600 --out "$WORK_DIR/k9/load.json" \
		>/dev/null 2>"$WORK_DIR/k9/load.err"
	wall_after="$(date +%s.%N)"
	for pid in "${pids[@]}"; do after+=("$(k9_cpu "$pid")"); done
	kill "$sampler" 2>/dev/null || true

	local calls failed p50 base_p50 added
	calls="$(jq -r '.calls' "$WORK_DIR/k9/load.json")"
	failed="$(jq -r '.failed' "$WORK_DIR/k9/load.json")"
	p50="$(jq -r '.p50_ms' "$WORK_DIR/k9/load.json")"
	base_p50="$(jq -r '.p50_ms' "$WORK_DIR/k9/baseline.json")"
	added="$(awk -v a="$p50" -v b="$base_p50" 'BEGIN { printf "%.1f", a - b }')"
	observe "the ten minutes: $calls calls, $failed of them failed; the median was $p50 ms through the airlock and $base_p50 ms through a plain tunnel over $(jq -r .calls "$WORK_DIR/k9/baseline.json") calls, so the proxy added $added ms at the median"
	observe "what failed, if anything: $(jq -c '.failures' "$WORK_DIR/k9/load.json")"
	judge "$([ "$failed" = 0 ] && [ "$calls" -gt 0 ] && echo 0 || echo 1)" \
		"no call fails in the ten minutes" "$failed of $calls calls failed"
	judge "$(awk -v x="$added" 'BEGIN { exit !(x <= 100) }' && awk -v b="$base_p50" 'BEGIN { exit !(b > 0) }' && echo 0 || echo 1)" \
		"the proxy adds at most 100 milliseconds at the median" \
		"the median was $p50 ms against the plain tunnel's $base_p50 ms over $(jq -r .calls "$WORK_DIR/k9/baseline.json") baseline calls ($(jq -r .failed "$WORK_DIR/k9/baseline.json") of them failed), which is $added ms added"

	local i wall cpu peak limits="" worst_cpu=0 over=0
	wall="$(awk -v a="$wall_before" -v b="$wall_after" 'BEGIN { printf "%.1f", b - a }')"
	for i in "${!pids[@]}"; do
		cpu="$(awk -v a="${before[$i]}" -v b="${after[$i]}" 'BEGIN { printf "%.1f", b - a }')"
		peak="$(k9_peak_kb "$rss" "${pids[$i]}")"
		limits="$limits; pid ${pids[$i]}: $cpu s of CPU over $wall s of wall (a share of $(awk -v c="$cpu" -v w="$wall" 'BEGIN { printf "%.2f", c / w }')), peak memory $(awk -v k="$peak" 'BEGIN { printf "%.0f", k / 1024 }') MiB"
		awk -v c="$cpu" -v w="$wall" 'BEGIN { exit !(c <= w / 2) }' || over=$((over + 1))
		[ "${peak:-0}" -le 1048576 ] || over=$((over + 1))
		worst_cpu="$cpu"
	done
	observe "each broker process over the ten minutes${limits}"
	judge "$([ "$over" = 0 ] && echo 0 || echo 1)" \
		"each process's CPU time is at most half its wall time and its peak memory at most 1 GiB" \
		"${limits#; } (the last CPU figure read $worst_cpu s)"

	# The upload, the answer with a gap in it, the upgrade and the clone.
	echo "airlock-check: k9 is uploading 50 MiB, waiting out a 90-second gap and cloning..." >&2
	"$STANDS_BIN" load upload --proxy-file <(printf '%s\n' "$proxy") --host s.airlock.test --ca "$AIRLOCK_CA_CERT_FILE" \
		--header-file <(printf '%s\n' "$PLACEHOLDER") --size $((50 * 1024 * 1024)) --out "$WORK_DIR/k9/upload.json" >/dev/null 2>&1
	"$STANDS_BIN" load gap --proxy-file <(printf '%s\n' "$proxy") --host s.airlock.test --ca "$AIRLOCK_CA_CERT_FILE" \
		--header-file <(printf '%s\n' "$PLACEHOLDER") --gap 90 --out "$WORK_DIR/k9/gap.json" >/dev/null 2>&1
	local s_mark
	s_mark="$(records_count s)"
	"$STANDS_BIN" load upgrade --proxy-file <(printf '%s\n' "$proxy") --host s.airlock.test --ca "$AIRLOCK_CA_CERT_FILE" \
		--header-file <(printf '%s\n' "$PLACEHOLDER") --out "$WORK_DIR/k9/upgrade.json" >/dev/null 2>&1

	# The clone goes through the same proxy on an ordinary CONNECT: github.com is no bound host,
	# so nothing is swapped into it and what is proved is that the tunnel carries a clone.
	local clone_status clone_out
	rm -rf "$WORK_DIR/k9/clone"
	clone_out="$(GIT_CONFIG_COUNT=2 \
		GIT_CONFIG_KEY_0=http.proxy GIT_CONFIG_VALUE_0="$proxy" \
		GIT_CONFIG_KEY_1=http.sslCAInfo GIT_CONFIG_VALUE_1="$AIRLOCK_CA_CERT_FILE" \
		git clone --depth 1 --quiet https://github.com/cybermonklabs/legion "$WORK_DIR/k9/clone" 2>&1 | tail -n 2 | tr '\n' ' ')"
	clone_status=$?
	local cloned=0
	[ -d "$WORK_DIR/k9/clone/.git" ] && cloned=1

	observe "the 50 MiB upload: $(cat "$WORK_DIR/k9/upload.json" | tr -d '\n ')"
	observe "the answer with a 90-second gap in it: $(cat "$WORK_DIR/k9/gap.json" | tr -d '\n ')"
	observe "the shallow clone of cybermonklabs/legion through the tunnel exited $clone_status and left a checkout: $cloned ($clone_out)"
	local upload_ok gap_ok
	upload_ok="$(jq -r '.status // 0' "$WORK_DIR/k9/upload.json")"
	gap_ok="$(jq -r '.second // 0' "$WORK_DIR/k9/gap.json")"
	judge "$([ "$upload_ok" = 200 ] && [ "$gap_ok" = 200 ] && [ "$cloned" = 1 ] && echo 0 || echo 1)" \
		"the upload, the answer with the gap and the clone finish" \
		"the upload answered $upload_ok, the second event after the 90-second gap answered $gap_ok, and the clone left a checkout: $cloned"

	local upgrade_recs upgrade_header upgrade_status
	upgrade_recs="$(records s | jq --argjson n "$s_mark" '.[$n:]')"
	upgrade_header="$(printf '%s' "$upgrade_recs" | jq -r '[.[] | select(.path == "/socket") | .header.Authorization // [] | .[]] | .[0] // ""')"
	upgrade_status="$(jq -r '.status // 0' "$WORK_DIR/k9/upgrade.json")"
	observe "the WebSocket upgrade: $(cat "$WORK_DIR/k9/upgrade.json" | tr -d '\n' | head -c 300); S recorded its Upgrade header as $(printf '%s' "$upgrade_recs" | jq -r '[.[] | select(.path == "/socket") | .header.Upgrade // [] | .[]] | .[0] // ""') and its Authorization as $(shape "$upgrade_header") against the stored $(shape "$demo_value") (compared by SHA-256 and length)"
	judge "$([ "$upgrade_status" = 101 ] && contains "$demo_value" "$upgrade_header" && contains "the service read" "$(jq -r '.frames // ""' "$WORK_DIR/k9/upgrade.json")" && echo 0 || echo 1)" \
		"the WebSocket upgrade reaches S with the real value in its header, and frames pass both ways" \
		"S answered $upgrade_status, its Authorization carried the real value ($(shape "$upgrade_header") against the stored $(shape "$demo_value"), compared by SHA-256 and length), and the frame that came back was: $(jq -r '.frames // ""' "$WORK_DIR/k9/upgrade.json")"

	# One broker stopped while a connection through it is open, and then the next call. The
	# connection is this run's own gap connection on the second broker's proxy port, so what is
	# stopped is the process carrying it.
	local stopped="${pids[-1]}" after_stop
	"$STANDS_BIN" load gap --proxy-file <(proxy_for "${BROKER_PROXY_ADDRS[-1]}") \
		--host s.airlock.test --ca "$AIRLOCK_CA_CERT_FILE" --header-file <(printf '%s\n' "$PLACEHOLDER") --gap 8 \
		--out "$WORK_DIR/k9/stopped.json" >/dev/null 2>&1 &
	local gap_pid=$!
	sleep 3
	kill "$stopped" 2>/dev/null || true
	wait "$gap_pid" 2>/dev/null || true
	# Four calls, not one: the stack's proxy front is a round-robin over both brokers' ports
	# that reads its backends from a file and asks nobody whether a backend is alive, so every
	# other connection still goes to the stopped process and is reset. In a cluster the Service
	# drops a terminating pod from its EndpointSlice; here nothing does, so what this reads is
	# that a call through the front reaches the broker that is still up.
	local after_stop="" status ok=0 i
	for i in 1 2 3 4; do
		status="$(printf 'Authorization: Bearer %s' "$PLACEHOLDER" | curl -sS -o /dev/null -w '%{http_code}' --noproxy '' --cacert "$AIRLOCK_CA_CERT_FILE" \
			-K <(printf 'proxy = "%s"\n' "$proxy") -H @- "$AIRLOCK_S_URL/ok" 2>&1 | tr -d '\n')"
		after_stop="$after_stop $status"
		[ "$status" = 200 ] && ok=$((ok + 1))
	done
	local still_up=0
	kill -0 "$stopped" 2>/dev/null && still_up=1
	observe "with broker $stopped stopped while it carried a connection: that connection answered $(cat "$WORK_DIR/k9/stopped.json" | tr -d '\n ') and the four calls after it answered$after_stop (the stopped process is still up: $still_up)"
	judge "$([ "$ok" -gt 0 ] && [ -z "$(jq -r '.second // ""' "$WORK_DIR/k9/stopped.json")" ] && [ -n "$(jq -r '.second_error // .error // ""' "$WORK_DIR/k9/stopped.json")" ] && echo 0 || echo 1)" \
		"the stream on the stopped process ends after the drain, and the next call succeeds" \
		"the connection on the stopped process ended $(jq -r 'if .second then "with its second event answering " + (.second|tostring) else "with " + (.second_error // .error // "no answer") end' "$WORK_DIR/k9/stopped.json"), and $ok of the four calls after it answered 200 ($after_stop); the ones that did not are the connections the front still handed to the stopped process, which it keeps in its backends file with no health check of its own"

	stop_stack

	# The negative control: a stack of its own whose proxy cuts an idle connection at 60 seconds,
	# which is what the 90-second gap is measured against.
	start_stack --idle-seconds 60
	stack_facts
	release AIRLOCK_DEMO_TOKEN 30m
	"$STANDS_BIN" load gap --proxy-file <(proxy_for "${AIRLOCK_PROXY_URL##*://}") --host s.airlock.test \
		--ca "$AIRLOCK_CA_CERT_FILE" --header-file <(printf '%s\n' "$PLACEHOLDER") --gap 90 \
		--out "$WORK_DIR/k9/control-gap.json" >/dev/null 2>&1
	local control_second
	control_second="$(jq -r '.second // ""' "$WORK_DIR/k9/control-gap.json")"
	control "with --idle-seconds 60 the same answer with a 90-second gap in it: $(cat "$WORK_DIR/k9/control-gap.json" | tr -d '\n ')"
	judge "$([ -s "$WORK_DIR/k9/control-gap.json" ] && [ -z "$control_second" ] && [ -n "$(jq -r '.second_error // .error // ""' "$WORK_DIR/k9/control-gap.json")" ] && echo 0 || echo 1)" \
		"negative control: with the idle limit at 60 seconds the answer with the 90-second gap is cut" \
		"its second event $(jq -r 'if .second then "answered " + (.second|tostring) else "did not arrive: " + (.second_error // .error // "no answer") end' "$WORK_DIR/k9/control-gap.json")"

	stop_stack
}

###############################################################################################
# k15
###############################################################################################

check_k15() {
	# The theorems are the ones Axioms.lean names: it holds one `#print axioms` line for each,
	# and this check reads that list rather than writing a second copy of it, so a theorem
	# added there is one k15 prints, counts and expects with nothing edited here.
	local theorems count
	theorems="$(sed -n 's/^#print axioms  *//p' "$LEAN_DIR/Axioms.lean")"
	count="$(printf '%s\n' "$theorems" | grep -c .)"
	if [ "$count" = 0 ]; then
		echo "airlock-check: $LEAN_DIR/Axioms.lean names no theorem, so k15 has nothing to print or count" >&2
		exit 1
	fi

	expect "the Lean model builds on the head"
	expect "its $count theorems are there and are printed with their statements"
	expect "they depend on no sorry and on no axiom outside propext, Classical.choice and Quot.sound"
	expect "the comparison fuzz test passes for ten minutes"
	expect "the negative control, the ten mutants this check names, is caught: the five Go mutants of verification k15 (go-extra-header, go-no-host-check, go-hostless-entry, go-carries-value, go-cipher-repository-forwarded) and its two model mutants (lean-all-headers, lean-carries-value), and the kinds slice's three (lean-no-widen, go-no-widen, go-override-kind-allow-only); each Go mutant fails the comparison and each model mutant fails lake build or that same comparison"

	if ! command -v lake >/dev/null 2>&1; then
		echo "airlock-check: k15 needs lake from the Lean toolchain; set ELAN_HOME and PATH" >&2
		exit 1
	fi

	local build_log="$WORK_DIR/lake-build.log"
	echo "airlock-check: building the Lean model..." >&2
	(cd "$LEAN_DIR" && lake build) >"$build_log" 2>&1
	local build_status=$?
	local model="$LEAN_DIR/.lake/build/bin/airlock-model"
	observe "lake build exited $build_status: $(tail -n 1 "$build_log")"
	judge "$([ "$build_status" = 0 ] && [ -x "$model" ] && echo 0 || echo 1)" \
		"the model builds" "lake build exited $build_status and wrote $model"

	# The statements, printed from a file of this run's own so nothing is added to the tree.
	# Its list is the one read from Axioms.lean above, each `#print axioms X` written out as
	# `#check @X`.
	{
		echo "import Airlock"
		printf '%s\n' "$theorems" | sed 's/^/#check @/'
	} >"$WORK_DIR/Statements.lean"
	(cd "$LEAN_DIR" && lake env lean "$WORK_DIR/Statements.lean") >"$WORK_DIR/statements.txt" 2>&1
	local statements_status=$?
	# scripts/airlock-lean-axioms.sh is the one copy of the axiom check, and ci.yaml's
	# airlock-lean job runs the same one: it runs lake env lean Axioms.lean, keeps the axiom
	# text where this block reads it back, and says on its standard error what is wrong.
	local bad
	bad="$(bash "$SCRIPT_DIR/airlock-lean-axioms.sh" "$LEAN_DIR" --axioms-file "$WORK_DIR/axioms.txt" 2>&1 >/dev/null)"
	local axioms_status=$?
	echo
	echo "the model's theorem statements:"
	sed 's/^/  /' "$WORK_DIR/statements.txt"
	echo
	echo "the theorems' axioms:"
	sed 's/^/  /' "$WORK_DIR/axioms.txt"
	observe "lake env lean printed the $count theorem statements (exit $statements_status) and scripts/airlock-lean-axioms.sh read their axioms (exit $axioms_status); both are above this block"
	# `#check @X` opens its line with "@Airlock.X" for a theorem with implicit arguments and
	# with "Airlock.X" for one without, and a statement runs over several lines, so the count
	# is of the lines that open one.
	local printed
	printed="$(grep -cE '^@?Airlock\.' "$WORK_DIR/statements.txt")"
	judge "$([ "$statements_status" = 0 ] && [ "$printed" = "$count" ] && echo 0 || echo 1)" \
		"its $count theorems are there and are printed with their statements" \
		"$printed of the $count theorems Axioms.lean names type-checked and printed"

	observe "the axioms each theorem rests on: $(tr '\n' ' ' <"$WORK_DIR/axioms.txt")"
	judge "$axioms_status" \
		"the theorems depend on no sorry and on no axiom outside the three Lean admits" \
		"${bad:-every one of the $count rests on propext, Classical.choice and Quot.sound alone, and none on sorryAx}"

	echo "airlock-check: fuzzing the Go airlock against the model for ten minutes..." >&2
	(cd "$ENVOY_DIR" && AIRLOCK_MODEL="$model" go test -run FuzzAirlock ./internal/cipher/proxyrules/) >"$WORK_DIR/seed-corpus.log" 2>&1
	local seed_status=$?
	(cd "$ENVOY_DIR" && AIRLOCK_MODEL="$model" go test -fuzz FuzzAirlock -fuzztime=600s -run FuzzAirlock ./internal/cipher/proxyrules/) >"$WORK_DIR/fuzz.log" 2>&1
	local fuzz_status=$?
	observe "the seed corpus comparison exited $seed_status: $(tail -n 2 "$WORK_DIR/seed-corpus.log" | tr '\n' ' ')"
	observe "ten minutes of fuzzing exited $fuzz_status: $(grep -E '^(ok|FAIL|--- FAIL|fuzz:)' "$WORK_DIR/fuzz.log" | tail -n 3 | tr '\n' ' ')"
	judge "$([ "$seed_status" = 0 ] && [ "$fuzz_status" = 0 ] && echo 0 || echo 1)" \
		"the comparison fuzz test passes for ten minutes" \
		"the seed corpus exited $seed_status and -fuzztime=600s exited $fuzz_status"

	echo "airlock-check: running the mutants..." >&2
	# The ten this check names, and no other: verification k15's five Go mutants and two model
	# mutants, and the three the kinds slice adds (a profile's allow that widens its kind, in the
	# model and in Go, and a method override refused on the kind's allow alone). Each is passed to
	# airlock-mutants.sh by its patch's name, so the script runs exactly these, and a name with no
	# patch is its refusal rather than a mutant quietly left out. None is a requests mutant, so no
	# database is needed and none is passed.
	local k15_mutants=(go-extra-header go-no-host-check go-hostless-entry go-carries-value go-cipher-repository-forwarded
		lean-all-headers lean-carries-value lean-no-widen go-no-widen go-override-kind-allow-only)
	(cd "$ENVOY_DIR" && AIRLOCK_MODEL="$model" bash "$SCRIPT_DIR/airlock-mutants.sh" "${k15_mutants[@]}") >"$WORK_DIR/mutants.log" 2>&1
	local mutants_status=$?
	echo
	echo "the mutants:"
	grep -E '^(PASS|FAIL|UNKNOWN):' "$WORK_DIR/mutants.log" | sed 's/^/  /'
	# Each of the ten must have its own PASS line, and the run no line for any other mutant: a
	# count of PASS lines alone would read ten catches of the wrong ten as the right ones.
	local caught named missing="" others
	caught=$(grep -cE '^PASS:' "$WORK_DIR/mutants.log" || true)
	for named in "${k15_mutants[@]}"; do
		grep -qE "^PASS: $named( |$)" "$WORK_DIR/mutants.log" || missing="$missing $named"
	done
	# A mutant's own line names it second; the summary lines ("UNKNOWN: 3 mutant(s) not run",
	# "UNKNOWN: no mutant ran") name a count or "no" there and are no mutant.
	others="$(grep -E '^(PASS|FAIL|UNKNOWN|SKIPPED): ' "$WORK_DIR/mutants.log" | awk '$2 ~ /^[a-z]+-[a-z-]+$/ {print $2}' | grep -vxF -f <(printf '%s\n' "${k15_mutants[@]}") | tr '\n' ' ' || true)"
	control "scripts/airlock-mutants.sh ran the ten named and exited $mutants_status: $caught caught; $(grep -E '^UNKNOWN: [0-9]' "$WORK_DIR/mutants.log" || echo 'every one ran')"
	control "$(grep -E '^(PASS|FAIL|UNKNOWN):' "$WORK_DIR/mutants.log" | tr '\n' ' ')"
	judge "$([ "$mutants_status" = 0 ] && [ -z "$missing" ] && [ "$caught" = "${#k15_mutants[@]}" ] && [ -z "$others" ] && echo 0 || echo 1)" \
		"negative control: each of the ten mutants is caught, and no other mutant ran" \
		"airlock-mutants.sh exited $mutants_status$(case "$mutants_status" in 0) ;; 3) echo ' (UNKNOWN: a mutant did not run)' ;; *) echo ' (a mutant was not caught, or a name has no patch)' ;; esac) with $caught of ${#k15_mutants[@]} caught${missing:+; not caught or not run:$missing}${others:+; other mutants that ran: $others}"
}

###############################################################################################
# k47
###############################################################################################
#
# The anthropic kind on the local stack. dev-airlock.sh declares the kind as the spec writes it,
# stores the two anthropic-federation secrets (N, AIRLOCK_ANTHROPIC_NARROWED, proxy-narrowed with
# one profile of the kind's three rows; K, AIRLOCK_ANTHROPIC_KIND, with neither), binds each to a
# Google account of its own, points the minter at the iam and anthropic-token stands, and has
# api.anthropic.com resolve to the Admin API stand. Every call this check makes to
# api.anthropic.com names that stand's loopback address with curl's --resolve too, which curl
# uses only for a call it makes directly: a call that leaves the proxy out reaches the stand, not
# Anthropic, and is read there as a call that never reached the proxy.
#
# Each grant is a person's: the stack's approver approves it on the broker's page as each
# human-tier check does (approve_watch, release_approved). The broker hands back a live grant of
# the same name and lifetime with no new request, so the runs after the first of a secret reuse
# its grant, and the 3601 run asks for another lifetime to get a grant, and a mint, of its own.
#
# The two controls that need other code are brokers built from a copy of this module with one
# defect put in (k47_control_tree), started beside the stack's on its database, profiles file and
# stand-ins, with the stack's two fronts pointed at them for that step alone.

# K47_KIND_WANT is the anthropic kind of spec v70 as k47_kind writes a kind: its hosts, its
# headers (lower-cased, since the airlock compares header names after an ASCII fold), its allow
# rows, its ways, its variables and its max_grant.
K47_KIND_WANT='hosts=api.anthropic.com headers=authorization allow=GET /v1/organizations/,POST /v1/organizations/,DELETE /v1/organizations/ ways=mint variables=ANTHROPIC_ADMIN_TOKEN max_grant=1h'

# k47_kind writes the anthropic kind of a profiles file on one line, in K47_KIND_WANT's form, and
# nothing for a file with no such kind. A kind that leaves headers out takes the default header,
# authorization, as proxyrules.Kind.HeaderNames reads it.
k47_kind() { # <profiles file>
	jq -r '[.kinds[]? | select(.name == "anthropic")][0] // empty
		| "hosts=\(.hosts // [] | join(",")) headers=\(.headers // ["authorization"] | map(ascii_downcase) | join(",")) allow=\(.allow // [] | map("\(.method) \(.pathPrefix)") | join(",")) ways=\(.ways // [] | join(",")) variables=\(.variables // [] | join(",")) max_grant=\(.max_grant // "")"' "$1" 2>/dev/null
}

# k47_lines reads broker log lines, one JSON object each, on standard input, and writes what they
# say of one secret's calls to api.anthropic.com: the lines in all (each carried call's call line
# and each refused call's refusal record), the carried ones and of those the ones that name the
# way mint, the refused ones and of those the ones whose reason names an allow rule, the method
# and path of each refused one, and the mint lines of the secret, how many of them hold a SHA-256
# and each hash. A line that is no JSON object is left out.
k47_lines() { # <secret>
	jq -Rrs --arg n "$1" '
		[split("\n")[] | fromjson? | select(type == "object")] as $all
		| [$all[] | select(.message == "airlock proxy: a call to a bound host" and .secret == $n and .host == "api.anthropic.com")] as $calls
		| [$all[] | select(.message == "broker: answer refused" and .caller_class == "airlock" and .host == "api.anthropic.com" and ((.secrets // []) | index($n)) != null)] as $refused
		| [$all[] | select(.message == "broker: a token was minted for a call" and .secret == $n)] as $mints
		| "lines=\(($calls + $refused) | length) allowed=\([$calls[] | select(.allowed == true)] | length) allowed_mint=\([$calls[] | select(.allowed == true and .way == "mint")] | length) refused=\($refused | length) refused_rule=\([$refused[] | select((.reason // "") | test("not allowed by"))] | length) refused_methods=\([$refused[] | "\(.method) \(.path)"] | join(",")) mints=\($mints | length) mints_hashed=\([$mints[] | select((.sha256 // "") | test("^[0-9a-f]{64}$"))] | length) mint_hashes=\([$mints[] | .sha256 // ""] | join(",")) grants=\([$calls[] | .grant // ""] | unique | join(","))"'
}

# k47_api writes what the Admin API stand's records of one run say against the tokens the token
# exchange stand answered for one secret's service account: the records in all, those of the
# three requests the kind allows and their methods, those of the three it refuses (POST
# /v1/oauth/token, POST /v1/messages, a PUT), how many of the allowed ones carry in Authorization a
# token the exchange answered for that service account, how many distinct values those
# Authorization headers hold, and how many records hold a placeholder anywhere.
k47_api() { # <records json file> <exchange tokens json file> <service_account_id>
	jq -rn --slurpfile recs "$1" --slurpfile toks "$2" --arg s "$3" '
		([$toks[0][]? | select(.service_account_id == $s) | .access_token]) as $mine
		| ($recs[0] // []) as $r
		| [$r[] | select((.method + " " + .path) as $m | ["GET /v1/organizations/workspaces", "POST /v1/organizations/workspaces", "DELETE /v1/organizations/workspaces/w1"] | index($m) != null)] as $allowed
		| [$allowed[] | ((.header.Authorization // [])[0] // "") | ltrimstr("Bearer ")] as $auth
		| "records=\($r | length) allowed=\($allowed | length) allowed_methods=\([$allowed[] | .method] | join(",")) refused_reached=\([$r[] | select(.path == "/v1/oauth/token" or .path == "/v1/messages" or .method == "PUT")] | length) token_ok=\([$auth[] | select(. as $a | $mine | index($a) != null)] | length) distinct=\($auth | unique | length) placeholders=\([$r[] | tostring | select(test("agent-secrets-placeholder-"))] | length)"'
}

# k47_exchanges writes what the token exchange stand's records say of one secret: the exchanges
# that named its service account, how many of them name its organization, federation rule and
# workspace and carry as their assertion an identity token the iam stand minted for its bound
# account and Anthropic's audience, how many such identity tokens the iam stand minted, and how
# many it minted for that account for any other audience.
k47_exchanges() { # <exchange records json file> <iam tokens json file> <account> <service_account_id> <organization_id> <federation_rule_id> <workspace_id>
	jq -rn --slurpfile recs "$1" --slurpfile ids "$2" --arg acct "$3" --arg s "$4" --arg org "$5" --arg rule "$6" --arg ws "$7" '
		([$ids[0][]? | select(.account == $acct and .audience == "https://api.anthropic.com") | .token]) as $theirs
		| [($recs[0] // [])[] | .body | fromjson? | select(type == "object" and .service_account_id == $s)] as $mine
		| "exchanges=\($mine | length) matching=\([$mine[] | select(.organization_id == $org and .federation_rule_id == $rule and .workspace_id == $ws and (.assertion as $a | $theirs | index($a)) != null)] | length) identity_tokens=\($theirs | length) other_audiences=\([$ids[0][]? | select(.account == $acct and .audience != "https://api.anthropic.com")] | length)"'
}

# k47_hits writes how many times the files named hold the opening of an Anthropic token,
# sk-ant-oat01-, and how many bytes they hold in all: a search of nothing finds nothing, so the
# bytes are what says the search had something to read.
k47_hits() { # <file>...
	printf 'hits=%s bytes=%s' "$(cat "$@" 2>/dev/null | grep -o 'sk-ant-oat01-' | wc -l | tr -d ' ')" "$(cat "$@" 2>/dev/null | wc -c | tr -d ' ')"
}

# k47_secrets_json writes the fake store's secrets as `gcloud secrets list
# --format='json(name,annotations)'` lists them, which `envoy-broker profiles check
# --secrets-json` reads: each a name ending in /secrets/agent-secrets-<slug> and its tags as its
# annotations. A profile with no proxy string takes its kind from its source's proxy tag, which
# only that list gives the rules test.
k47_secrets_json() { # <fake secrets file>
	jq '[.secrets[] | {name: ("projects/000000000000/secrets/agent-secrets-" + (.name | sub("^.*/"; ""))), annotations: .tags}]' "$1"
}

# k47_field reads one field of a k47_* line: the word after <name>= up to the next " <word>=".
k47_field() { # <line> <name>
	printf '%s\n' "$1" | sed -n "s/.*\(^\| \)$2=\([^=]*\)\( [a-z_]*=.*\)\?\$/\2/p"
}

# k47_control_tree copies this module (packages/envoy) to <directory> and puts one defect in it
# (k47_control_edit). The copy leaves out the module root's build products and the Lean model's
# build directory, which no build of cmd/cipherd reads.
k47_control_tree() { # <directory> <kind-allow|lifetime>
	mkdir -p "$1"
	(cd "$ENVOY_DIR" && tar -cf - --exclude=./out --exclude=./dispatch-e2e --exclude=./node_modules --exclude=./internal/cipher/proxyrules/lean/.lake .) | tar -xf - -C "$1"
	k47_control_edit "$1" "$2"
}

# k47_control_edit puts one defect into a module tree: kind-allow takes the kind's allow out of
# the decision (proxyrules check), so the proxy reads a profile's allow and not its kind's;
# lifetime takes the lifetime bound out of the anthropic-federation minter, so it takes a token of
# any lifetime. It answers 1 unless the text it takes out was in the file before the edit and is
# gone after it: a patch that matched nothing builds the broker this check already has, whose
# control would then fail for the wrong reason or pass for none.
k47_control_edit() { # <directory> <kind-allow|lifetime>
	local file text before left
	case "$2" in
	kind-allow)
		file="$1/internal/cipher/proxyrules/airlock.go" text='case !allowOK(k.Allow, r):'
		before="$(grep -cF "$text" "$file" 2>/dev/null)"
		perl -0777 -pi -e 's/\n\tcase !allowOK\(k\.Allow, r\):\n\t\treturn nil, "not allowed by rule"\n/\n/' "$file" 2>/dev/null
		;;
	lifetime)
		file="$1/internal/cipher/secrets/anthropicfederation.go" text='	if lifetime > federationLifetimeMax {'
		before="$(grep -cF "$text" "$file" 2>/dev/null)"
		perl -0777 -pi -e 's/\tif lifetime > federationLifetimeMax \{/\tif false && lifetime > federationLifetimeMax {/' "$file" 2>/dev/null
		;;
	*) return 1 ;;
	esac
	left="$(grep -cF "$text" "$file" 2>/dev/null)"
	[ "${before:-0}" = 1 ] && [ "${left:-1}" = 0 ]
}

# k47_control_broker builds one control broker (k47_control_tree) and starts it beside the stack's,
# leaving its addresses in EXTRA_API_ADDR and EXTRA_PROXY_ADDR; it answers 1, with why in
# K47_CONTROL_WHY, when the tree or the build failed.
k47_control_broker() { # <kind-allow|lifetime>
	local tree="$WORK_DIR/k47-tree-$1" binary="$WORK_DIR/k47-broker-$1"
	K47_CONTROL_WHY=""
	if ! k47_control_tree "$tree" "$1"; then
		K47_CONTROL_WHY="the $1 edit left the text it takes out in the copy, so a broker built from it would be the stack's own"
		return 1
	fi
	if ! (cd "$tree" && GOWORK=off go build -o "$binary" ./cmd/cipherd) >"$WORK_DIR/k47-build-$1.log" 2>&1; then
		K47_CONTROL_WHY="the $1 broker did not build: $(tail -n 3 "$WORK_DIR/k47-build-$1.log" | tr '\n' ' ')"
		return 1
	fi
	EXTRA_BROKER_BIN="$binary"
	start_extra_broker "k47-$1"
	EXTRA_BROKER_BIN=""
}

# k47_log_mark remembers how many lines each broker log holds, the stack's and this run's own
# brokers', and k47_log_since writes every line each of them gained since: one run's lines, read
# from logs both stack brokers write to.
k47_log_mark() {
	K47_LOG_MARK=""
	local f
	for f in "$AIRLOCK_DIR"/broker*.log "$WORK_DIR"/broker-*.log; do
		[ -f "$f" ] && K47_LOG_MARK+="$f $(wc -l <"$f")"$'\n'
	done
}
k47_log_since() {
	local f n
	for f in "$AIRLOCK_DIR"/broker*.log "$WORK_DIR"/broker-*.log; do
		[ -f "$f" ] || continue
		n="$(printf '%s' "$K47_LOG_MARK" | awk -v f="$f" '$1 == f { print $2 }')"
		tail -n +"$((${n:-0} + 1))" "$f"
	done
}

# k47_steps writes the command one agent-secrets run starts: it reads the grant's placeholder from
# the variable named as <name>, notes whether ANTHROPIC_ADMIN_TOKEN holds it, the child's NO_PROXY,
# and its whole environment, and makes the calls <mode> names, each with the placeholder as a
# Bearer token, each line of its output file <step>=<status> <what the answer and curl said>.
# all is the six calls of (a) to (d); get is (a)'s GET; messages is (c); gated is (a)'s GET once
# the driver has made the go file, after the command has made the ready file.
k47_steps() { # <label> <name> <all|get|messages|gated>
	local label="$1" name="$2" mode="$3" d="$WORK_DIR/k47"
	{
		cat <<EOF
set -u
P="\${$name:-}"
out="$d/out-$label"
: >"\$out"
say() { printf '%s\n' "\$1" >>"\$out"; }
say "admin_token=\$(if [ -z "\${ANTHROPIC_ADMIN_TOKEN:-}" ]; then echo unset; elif [ "\$ANTHROPIC_ADMIN_TOKEN" = "\$P" ]; then echo placeholder; else echo other; fi)"
say "placeholder=\$(case "\$P" in agent-secrets-placeholder-*) echo yes ;; *) echo no ;; esac)"
say "no_proxy=\${NO_PROXY:-}"
env >"$d/env-$label.txt"
call() {
	local code
	code="\$(printf 'Authorization: Bearer %s' "\$P" | curl -sS --max-time 60 --resolve api.anthropic.com:443:$AIRLOCK_ANTHROPIC_ADDRESS -o "$d/body-$label-\$1" -w '%{http_code}' -X "\$2" -H @- -H 'Content-Type: application/json' \${4:+--data "\$4"} "$AIRLOCK_ANTHROPIC_URL\$3" 2>"$d/err-$label-\$1")"
	say "\$1=\$code \$(cat "$d/body-$label-\$1" "$d/err-$label-\$1" 2>/dev/null | tr '\n' ' ' | cut -c1-240)"
}
EOF
		case "$mode" in
		all)
			printf '%s\n' "call get GET /v1/organizations/workspaces" \
				"call post POST /v1/organizations/workspaces '{\"name\":\"k47\"}'" \
				"call delete DELETE /v1/organizations/workspaces/w1" \
				"call token POST /v1/oauth/token '{\"grant_type\":\"k47\"}'" \
				"call messages POST /v1/messages '{\"model\":\"k47\"}'" \
				"call put PUT /v1/organizations/workspaces/w1 '{\"name\":\"k47\"}'"
			;;
		get) printf '%s\n' "call get GET /v1/organizations/workspaces" ;;
		messages) printf '%s\n' "call messages POST /v1/messages '{\"model\":\"k47\"}'" ;;
		gated)
			printf '%s\n' ": >\"$d/ready-$label\"" \
				"for i in \$(seq 1 300); do [ -e \"$d/go-$label\" ] && break; sleep 1; done" \
				"call get GET /v1/organizations/workspaces"
			;;
		esac
	} >"$d/steps-$label.sh"
}

# k47_run runs one agent-secrets run of <name> with <ttl> and a reason naming k47, its command
# k47_steps', its stderr where approve_watch reads the page of a request that waits for a person,
# and reads the command's output into K47OUT.
declare -A K47OUT
k47_run() { # <label> <name> <ttl> <what the run does>
	local d="$WORK_DIR/k47" key value
	agent-secrets "$2" --ttl "$3" --wait 5m --reason "airlock check k47: $4" -- bash "$d/steps-$1.sh" >>"$d/runs.log" 2>>"$d/cli.err"
	K47_EXIT=$?
	K47OUT=()
	while IFS='=' read -r key value; do [ -n "$key" ] && K47OUT["$key"]="$value"; done <"$d/out-$1" 2>/dev/null
	sleep 1
}

# k47_code is the status of one call of K47OUT, empty for a call the run did not make. The script
# runs under set -u, so a missing key reads as empty rather than ending the run.
k47_code() {
	local line="${K47OUT[$1]:-}"
	printf '%s' "${line%% *}"
}

check_k47() {
	expect "the anthropic kind binds api.anthropic.com alone, names the header Authorization, allows GET, POST and DELETE under /v1/organizations/, takes the way mint, names the variable ANTHROPIC_ADMIN_TOKEN, and has a max_grant of 1 hour"
	expect "for each of the two secrets, the three requests of (a) reach the stand-in API with the exchange's token in Authorization and no placeholder"
	expect "for each of the two secrets, (b), (c) and (d) are refused 403 with an error that names the rule, with no record at the stand-in API; secret K has no profile, so its refusals come from the kind's allow alone"
	expect "the identity reader records each secret's bound account and the audience https://api.anthropic.com"
	expect "the exchange records one exchange for each secret's three allowed requests, with that reader's token as its assertion, and with the organization_id, federation_rule_id, service_account_id and workspace_id of that secret's stored value"
	expect "the command's environment holds the placeholder in ANTHROPIC_ADMIN_TOKEN (the variables rule, deferred to the slice that builds it), and neither the environment nor the release answer holds a string that starts with sk-ant-oat01-"
	expect "the call from the parent whose NO_PROXY holds api.anthropic.com reaches the proxy, and that parent's own NO_PROXY is unchanged"
	expect "the request after the exchange answers an expires_in of 3601 is refused, and the stand-in API records nothing for it"
	expect "the broker logs one line for each request, allowed or refused with its reason, and each allowed line names the way mint; each mint line holds the SHA-256 of the token and not the token"
	expect "the rules test refuses the copy whose anthropic kind allows GET under /v1/organizations/ alone, and names each profile row its kind's allow does not hold, POST and DELETE under /v1/organizations/"
	expect "the negative controls: a build whose proxy reads a profile's allow and not its kind's lets K's POST /v1/messages reach the stand-in API with the exchange's token; a build whose minter takes any lifetime lets the request after the 3601 answer reach it; a parent run with a grant of another kind keeps the NO_PROXY entry in the child, and the child's call does not reach the proxy; and the search for sk-ant-oat01- over the stand-in API's record of (a) finds the token"

	# One broker, not the stack's two, for the reason k5 gives: each pod mints and holds its own
	# token for a grant, and the front gives each connection whichever pod it pleases, so with two
	# the three allowed requests of one secret carry one token or two by where the front sent
	# them. "One exchange serves the three allowed requests" is read on the pod that served them,
	# and neither the check's words nor its controls name a second process.
	start_stack --one-broker
	stack_facts
	local d="$WORK_DIR/k47"
	mkdir -p "$d"
	: >"$d/cli.err"
	: >"$d/runs.log"
	approve_watch "$d/cli.err" &
	HELPER_PIDS+=($!)

	# --- The kind, as the head's profiles file declares it and as the broker reads it. ---
	local kind
	kind="$(k47_kind "$AIRLOCK_PROFILES_FILE")"
	observe "the anthropic kind of the head's profiles file: ${kind:-none}"
	observe "the broker's own reading of it (envoy-broker profiles check): $("$BROKER_BIN" profiles check "$AIRLOCK_PROFILES_FILE" 2>&1 | grep '^kind "anthropic"' | tr '\n' ';')"
	judge "$([ -n "$kind" ] && [ "$kind" = "$K47_KIND_WANT" ] && echo 0 || echo 1)" \
		"the kind binds api.anthropic.com alone, names the header Authorization, allows GET, POST and DELETE under /v1/organizations/, takes the way mint, names the variable ANTHROPIC_ADMIN_TOKEN, and has a max_grant of 1 hour" \
		"it reads '${kind:-no anthropic kind}'"

	# --- (a) to (d), once for each secret. ---
	local label name account value svac org rule ws api_mark exchange_mark lines api exchanges token env_hits release_hits
	local n_grants="" n_api_file=""
	for label in narrowed kind; do
		case "$label" in
		narrowed) name="$AIRLOCK_ANTHROPIC_NARROWED_SECRET" account="$AIRLOCK_ANTHROPIC_NARROWED_ACCOUNT" ;;
		kind) name="$AIRLOCK_ANTHROPIC_KIND_SECRET" account="$AIRLOCK_ANTHROPIC_KIND_ACCOUNT" ;;
		esac
		value="$(stored_value "${STACK_SECRETS_PREFIX}airlock-anthropic-$label")"
		svac="$(printf '%s' "$value" | jq -r .service_account_id)"
		org="$(printf '%s' "$value" | jq -r .organization_id)"
		rule="$(printf '%s' "$value" | jq -r .federation_rule_id)"
		ws="$(printf '%s' "$value" | jq -r .workspace_id)"
		api_mark="$(records_count anthropic)"
		exchange_mark="$(records_count anthropic-token)"
		k47_log_mark
		k47_steps "$label" "$name" all
		k47_run "$label" "$name" 1h "secret $name sends the Admin API's three allowed requests and three it refuses"
		k47_log_since >"$d/log-$label.jsonl"
		records anthropic | jq --argjson n "$api_mark" '.[$n:]' >"$d/api-$label.json"
		records anthropic-token | jq --argjson n "$exchange_mark" '.[$n:]' >"$d/exchange-$label.json"
		curl -sS "$AIRLOCK_ANTHROPIC_TOKENS_URL" >"$d/tokens.json"
		curl -sS "$AIRLOCK_IAM_TOKENS_URL" >"$d/identities.json"
		lines="$(k47_lines "$name" <"$d/log-$label.jsonl")"
		api="$(k47_api "$d/api-$label.json" "$d/tokens.json" "$svac")"
		exchanges="$(k47_exchanges "$d/exchange-$label.json" "$d/identities.json" "$account" "$svac" "$org" "$rule" "$ws")"
		token="$(jq -r '[.[] | (.header.Authorization // [])[0] // "" | ltrimstr("Bearer ")][0] // ""' "$d/api-$label.json")"
		release_approved "$name" 1h "airlock check k47: read the release answer of secret $name" >/dev/null 2>&1
		printf '%s' "$RELEASE_JSON" >"$d/release-$label.json"
		env_hits="$(k47_hits "$d/env-$label.txt")"
		release_hits="$(k47_hits "$d/release-$label.json")"
		[ "$label" = narrowed ] && n_grants="$(k47_field "$lines" grants)" && n_api_file="$d/api-$label.json"

		observe "$name: agent-secrets exited $K47_EXIT; the placeholder variable held a placeholder: ${K47OUT[placeholder]:-unread}; get ${K47OUT[get]:-}; post ${K47OUT[post]:-}; delete ${K47OUT[delete]:-}; token ${K47OUT[token]:-}; messages ${K47OUT[messages]:-}; put ${K47OUT[put]:-}"
		observe "$name: the Admin API stand's records of this run: $api; the token it got is $(shape "$token") (compared by SHA-256 and length)"
		observe "$name: the broker's lines for it: $lines"
		observe "$name: the exchange's records: $exchanges"
		observe "$name: ANTHROPIC_ADMIN_TOKEN in the command's environment: ${K47OUT[admin_token]:-unread}; sk-ant-oat01- in the environment: $env_hits; in the release answer: $release_hits"

		judge "$([ "$(k47_code get)" = 200 ] && [ "$(k47_code post)" = 200 ] && [ "$(k47_code delete)" = 200 ] && [ "$(k47_field "$api" allowed)" = 3 ] && [ "$(k47_field "$api" token_ok)" = 3 ] && [ "$(k47_field "$api" placeholders)" = 0 ] && echo 0 || echo 1)" \
			"$name: the three requests of (a) reach the stand-in API with the exchange's token in Authorization and no placeholder" \
			"statuses $(k47_code get) $(k47_code post) $(k47_code delete); the stand recorded $(k47_field "$api" allowed) of them ($(k47_field "$api" allowed_methods)), $(k47_field "$api" token_ok) carrying a token the exchange answered for $svac, and $(k47_field "$api" placeholders) records holding a placeholder"
		local rule_words="not allowed by"
		[ "$label" = kind ] && rule_words="not allowed by rule"
		judge "$([ "$(k47_code token)" = 403 ] && [ "$(k47_code messages)" = 403 ] && [ "$(k47_code put)" = 403 ] && contains "$rule_words" "${K47OUT[token]:-}" && contains "$rule_words" "${K47OUT[messages]:-}" && contains "$rule_words" "${K47OUT[put]:-}" && [ "$(k47_field "$api" refused_reached)" = 0 ] && echo 0 || echo 1)" \
			"$name: (b), (c) and (d) are refused 403 with an error that names the rule, with no record at the stand-in API$([ "$label" = kind ] && echo "; with no profile, the kind's allow refuses them")" \
			"token ${K47OUT[token]:-}; messages ${K47OUT[messages]:-}; put ${K47OUT[put]:-}; the stand recorded $(k47_field "$api" refused_reached) of them"
		judge "$([ "$(k47_field "$exchanges" identity_tokens)" -ge 1 ] 2>/dev/null && [ "$(k47_field "$exchanges" other_audiences)" = 0 ] && echo 0 || echo 1)" \
			"$name: the identity reader records its bound account and the audience https://api.anthropic.com" \
			"the iam stand minted $(k47_field "$exchanges" identity_tokens) identity token(s) of $account for https://api.anthropic.com and $(k47_field "$exchanges" other_audiences) for any other audience"
		judge "$([ "$(k47_field "$exchanges" exchanges)" -ge 1 ] 2>/dev/null && [ "$(k47_field "$exchanges" matching)" = "$(k47_field "$exchanges" exchanges)" ] && [ "$(k47_field "$api" distinct)" = 1 ] && [ "$(k47_field "$api" token_ok)" = 3 ] && echo 0 || echo 1)" \
			"$name: one exchange serves its three allowed requests, with the reader's token as its assertion and the ids of its stored value" \
			"the three requests carried $(k47_field "$api" distinct) distinct token(s), one the exchange answered for $svac; the exchange was asked $(k47_field "$exchanges" exchanges) time(s) for $svac in this run (the session's release mints once to learn its slots, the proxy once for its calls), $(k47_field "$exchanges" matching) of them with the reader's token for $account and organization $org, rule $rule, workspace $ws"
		deferred "$name: the command's environment holds the placeholder in ANTHROPIC_ADMIN_TOKEN" "the variables slice (s-vars, SC2)" \
			"ANTHROPIC_ADMIN_TOKEN read '${K47OUT[admin_token]:-unread}' against the placeholder in $name"
		judge "$([ "$(k47_field "$env_hits" hits)" = 0 ] && [ "$(k47_field "$env_hits" bytes)" -gt 0 ] 2>/dev/null && [ "$(k47_field "$release_hits" hits)" = 0 ] && [ "$(k47_field "$release_hits" bytes)" -gt 0 ] 2>/dev/null && echo 0 || echo 1)" \
			"$name: neither the command's environment nor the release answer holds a string that starts with sk-ant-oat01-" \
			"environment $env_hits; release answer $release_hits"
		judge "$([ "$(k47_field "$lines" lines)" = 6 ] && [ "$(k47_field "$lines" allowed)" = 3 ] && [ "$(k47_field "$lines" allowed_mint)" = 3 ] && [ "$(k47_field "$lines" refused)" = 3 ] && [ "$(k47_field "$lines" refused_rule)" = 3 ] && echo 0 || echo 1)" \
			"$name: the broker logs one line for each of the six requests, allowed or refused with its reason, and each allowed line names the way mint" \
			"$(k47_field "$lines" lines) lines, $(k47_field "$lines" allowed) allowed ($(k47_field "$lines" allowed_mint) naming mint), $(k47_field "$lines" refused) refused ($(k47_field "$lines" refused_rule) naming an allow rule: $(k47_field "$lines" refused_methods))"
		local hash
		hash="$(printf '%s' "$token" | sha256sum | cut -d' ' -f1)"
		judge "$([ -n "$token" ] && contains "$hash" "$(k47_field "$lines" mint_hashes)" && [ "$(k47_field "$lines" mints)" = "$(k47_field "$lines" mints_hashed)" ] && echo 0 || echo 1)" \
			"$name: a mint line holds the SHA-256 of the token the proxy swapped in" \
			"$(k47_field "$lines" mints) mint line(s), $(k47_field "$lines" mints_hashed) with a SHA-256; one is the token's own hash: $(contains "$hash" "$(k47_field "$lines" mint_hashes)" && echo yes || echo no)"
	done
	local log_hits
	log_hits="$(k47_hits "$AIRLOCK_DIR"/broker*.log)"
	observe "sk-ant-oat01- in every line the stack's brokers logged: $log_hits"
	judge "$([ "$(k47_field "$log_hits" hits)" = 0 ] && [ "$(k47_field "$log_hits" bytes)" -gt 0 ] 2>/dev/null && echo 0 || echo 1)" \
		"no mint line, and no other line, holds a token: the brokers' logs hold no sk-ant-oat01-" "$log_hits"
	local api_hits
	api_hits="$(k47_hits "$n_api_file")"
	control "the same search over the stand-in API's record of N's (a): $api_hits"
	judge "$([ "$(k47_field "$api_hits" hits)" -ge 1 ] 2>/dev/null && echo 0 || echo 1)" \
		"negative control: the search for sk-ant-oat01- over the stand-in API's record of (a) finds the token" "$api_hits"

	# --- The parent whose NO_PROXY holds api.anthropic.com, with N's grant (handed back). ---
	local before_parent after_parent parent_lines parent_api
	before_parent="localhost,api.anthropic.com"
	api_mark="$(records_count anthropic)"
	k47_log_mark
	k47_steps parent "$AIRLOCK_ANTHROPIC_NARROWED_SECRET" get
	after_parent="$(
		export NO_PROXY="$before_parent" no_proxy="$before_parent"
		k47_run parent "$AIRLOCK_ANTHROPIC_NARROWED_SECRET" 1h "a parent whose NO_PROXY names api.anthropic.com sends (a)'s GET" >/dev/null 2>&1
		printf '%s|%s' "$NO_PROXY" "$no_proxy"
	)"
	k47_read_out parent
	parent_lines="$(k47_log_since | k47_lines "$AIRLOCK_ANTHROPIC_NARROWED_SECRET")"
	records anthropic | jq --argjson n "$api_mark" '.[$n:]' >"$d/api-parent.json"
	parent_api="$(k47_api "$d/api-parent.json" "$d/tokens.json" "$(stored_value "${STACK_SECRETS_PREFIX}airlock-anthropic-narrowed" | jq -r .service_account_id)")"
	observe "a parent with NO_PROXY=$before_parent: the child's NO_PROXY was '${K47OUT[no_proxy]:-}', its GET ${K47OUT[get]:-}; the broker's lines: $parent_lines; the stand: $parent_api; the parent's NO_PROXY and no_proxy after the run: $after_parent"
	judge "$([ "$(k47_code get)" = 200 ] && [ "$(k47_field "$parent_lines" allowed)" = 1 ] && [ "$(k47_field "$parent_api" token_ok)" = 1 ] && [ "$after_parent" = "$before_parent|$before_parent" ] && echo 0 || echo 1)" \
		"the call from the parent whose NO_PROXY holds api.anthropic.com reaches the proxy, and that parent's own NO_PROXY is unchanged" \
		"the GET answered $(k47_code get), the broker carried $(k47_field "$parent_lines" allowed) call of it with the minted token ($(k47_field "$parent_api" token_ok) at the stand), and the parent's NO_PROXY and no_proxy read '$after_parent' after it"

	# --- Its control: the same parent with a grant of another kind keeps the entry. ---
	local other_lines other_api
	api_mark="$(records_count anthropic)"
	k47_log_mark
	k47_steps other AIRLOCK_DEMO_TOKEN get
	(
		export NO_PROXY="$before_parent" no_proxy="$before_parent"
		k47_run other AIRLOCK_DEMO_TOKEN 30m "a parent whose NO_PROXY names api.anthropic.com, with a grant of another kind, calls it" >/dev/null 2>&1
	)
	k47_read_out other
	other_lines="$(k47_log_since | jq -Rc 'fromjson? | select(.host == "api.anthropic.com")' | wc -l | tr -d ' ')"
	records anthropic | jq --argjson n "$api_mark" '.[$n:]' >"$d/api-other.json"
	other_api="$(k47_api "$d/api-other.json" "$d/tokens.json" none)"
	control "a parent with NO_PROXY=$before_parent and a grant of AIRLOCK_DEMO_TOKEN (kind stest): the child's NO_PROXY was '${K47OUT[no_proxy]:-}', its GET ${K47OUT[get]:-}; the brokers wrote $other_lines line(s) for api.anthropic.com; the stand, which the call reaches directly at its --resolve address when it leaves the proxy out: $other_api"
	judge "$(case ",${K47OUT[no_proxy]:-}," in *,api.anthropic.com,*) [ "$other_lines" = 0 ] && echo 0 || echo 1 ;; *) echo 1 ;; esac)" \
		"negative control: with a grant of another kind the child keeps the NO_PROXY entry, and its call to api.anthropic.com does not reach the proxy" \
		"the child's NO_PROXY read '${K47OUT[no_proxy]:-}' and the brokers logged $other_lines line(s) for the host"

	# --- The 3601 answer: a new grant of N (another lifetime, so no live grant is handed back),
	# released while the exchange answers 3600, and its command's first call made only once the
	# exchange answers 3601, so the proxy's own mint is the one that meets it. ---
	local pid i tokens_before fresh_lines fresh_api fresh_tokens n_svac
	n_svac="$(stored_value "${STACK_SECRETS_PREFIX}airlock-anthropic-narrowed" | jq -r .service_account_id)"
	api_mark="$(records_count anthropic)"
	k47_log_mark
	k47_steps fresh "$AIRLOCK_ANTHROPIC_NARROWED_SECRET" gated
	rm -f "$d/ready-fresh" "$d/go-fresh"
	k47_run fresh "$AIRLOCK_ANTHROPIC_NARROWED_SECRET" 50m "a new grant whose first call meets an exchange that answers expires_in 3601" &
	pid=$!
	for ((i = 0; i < 600; i++)); do
		[ -e "$d/ready-fresh" ] && break
		kill -0 "$pid" 2>/dev/null || break
		sleep 0.5
	done
	tokens_before="$(curl -sS "$AIRLOCK_ANTHROPIC_TOKENS_URL" | jq length)"
	curl -sS -X POST -H 'Content-Type: application/json' --data '{"expires_in": 3601}' "$AIRLOCK_ANTHROPIC_ANSWER_URL" >"$d/answer-3601.json"
	: >"$d/go-fresh"
	wait "$pid"
	k47_read_out fresh
	curl -sS -X POST -H 'Content-Type: application/json' --data '{"expires_in": 3600}' "$AIRLOCK_ANTHROPIC_ANSWER_URL" >/dev/null
	fresh_lines="$(k47_log_since | k47_lines "$AIRLOCK_ANTHROPIC_NARROWED_SECRET")"
	records anthropic | jq --argjson n "$api_mark" '.[$n:]' >"$d/api-fresh.json"
	fresh_api="$(k47_api "$d/api-fresh.json" "$d/tokens.json" "$n_svac")"
	fresh_tokens="$(curl -sS "$AIRLOCK_ANTHROPIC_TOKENS_URL" | jq --argjson n "${tokens_before:-0}" --arg s "$n_svac" '[.[$n:][] | select(.service_account_id == $s and .expires_in == 3601)] | length')"
	observe "after the exchange answered $(jq -c . "$d/answer-3601.json" 2>/dev/null): the new grant's GET ${K47OUT[get]:-not made}; the exchange answered $fresh_tokens token(s) of 3601 seconds for $n_svac after the switch; the broker's lines: $fresh_lines (earlier grants of N: $n_grants); the stand: $fresh_api"
	judge "$([ -e "$d/ready-fresh" ] && [ "$fresh_tokens" -ge 1 ] 2>/dev/null && [ "$(k47_code get)" = 403 ] && [ "$(k47_field "$fresh_api" records)" = 0 ] && echo 0 || echo 1)" \
		"the request after the exchange answers an expires_in of 3601 is refused, and the stand-in API records nothing for it" \
		"the command reached its call: $([ -e "$d/ready-fresh" ] && echo yes || echo no); a fresh mint met the 3601 answer ($fresh_tokens token(s)); the GET answered ${K47OUT[get]:-nothing}; the stand recorded $(k47_field "$fresh_api" records) request(s)"

	# --- The rules test on a copy whose anthropic kind allows GET alone. ---
	local copy_out copy_status head_status
	jq '(.kinds[] | select(.name == "anthropic") | .allow) = [{"method": "GET", "pathPrefix": "/v1/organizations/"}]' "$AIRLOCK_PROFILES_FILE" >"$d/get-only.json"
	k47_secrets_json "$AIRLOCK_FAKE_SECRETS_FILE" >"$d/secrets.json"
	copy_out="$("$BROKER_BIN" profiles check "$d/get-only.json" --secrets-json "$d/secrets.json" 2>&1)"
	copy_status=$?
	"$BROKER_BIN" profiles check "$AIRLOCK_PROFILES_FILE" --secrets-json "$d/secrets.json" >"$d/head-check.txt" 2>&1
	head_status=$?
	observe "the rules test on the GET-only copy exited $copy_status: $(printf '%s' "$copy_out" | grep -i 'outside' | tr '\n' ' ')"
	observe "the same test on the head's file exited $head_status"
	judge "$([ "$copy_status" != 0 ] && [ "$head_status" = 0 ] && contains "$AIRLOCK_ANTHROPIC_NARROWED_SECRET" "$copy_out" && contains "POST /v1/organizations/" "$copy_out" && contains "DELETE /v1/organizations/" "$copy_out" && echo 0 || echo 1)" \
		"the rules test refuses the copy, and names each profile row its kind's allow does not hold, POST and DELETE under /v1/organizations/" \
		"the copy exited $copy_status naming $AIRLOCK_ANTHROPIC_NARROWED_SECRET's rows: $(printf '%s' "$copy_out" | grep -c 'outside') line(s); the head's file exited $head_status"

	# --- The control of the kind's allow: a broker whose proxy reads a profile's allow alone. ---
	local ctl_api
	if k47_control_broker kind-allow; then
		front_backends "$EXTRA_API_ADDR" "$EXTRA_PROXY_ADDR"
		api_mark="$(records_count anthropic)"
		k47_steps ctl-kind "$AIRLOCK_ANTHROPIC_KIND_SECRET" messages
		k47_run ctl-kind "$AIRLOCK_ANTHROPIC_KIND_SECRET" 1h "secret K's POST /v1/messages at a broker that reads a profile's allow alone"
		front_backends_stack
		records anthropic | jq --argjson n "$api_mark" '.[$n:]' >"$d/api-ctl-kind.json"
		curl -sS "$AIRLOCK_ANTHROPIC_TOKENS_URL" >"$d/tokens.json"
		ctl_api="$(jq --slurpfile toks "$d/tokens.json" '[.[] | select(.path == "/v1/messages") | ((.header.Authorization // [])[0] // "" | ltrimstr("Bearer ")) as $a | select($toks[0] | map(.access_token) | index($a) != null)] | length' "$d/api-ctl-kind.json")"
		control "a broker built from a copy of this module whose decision skips the kind's allow (proxyrules check, 'not allowed by rule' taken out): K's POST /v1/messages answered ${K47OUT[messages]:-nothing}, and the stand recorded $ctl_api POST /v1/messages carrying a token the exchange answered"
		judge "$([ "$(k47_code messages)" = 200 ] && [ "$ctl_api" -ge 1 ] 2>/dev/null && echo 0 || echo 1)" \
			"negative control: with the kind's allow out of the decision, K's POST /v1/messages reaches the stand-in API with the exchange's token" \
			"it answered $(k47_code messages) and the stand recorded $ctl_api such request(s)"
	else
		control "UNKNOWN: $K47_CONTROL_WHY"
		fail "negative control: with the kind's allow out of the decision, K's POST /v1/messages reaches the stand-in API — UNKNOWN, $K47_CONTROL_WHY"
	fi

	# --- The control of the lifetime bound: a broker whose minter takes any lifetime. ---
	local life_api
	if k47_control_broker lifetime; then
		front_backends "$EXTRA_API_ADDR" "$EXTRA_PROXY_ADDR"
		curl -sS -X POST -H 'Content-Type: application/json' --data '{"expires_in": 3601}' "$AIRLOCK_ANTHROPIC_ANSWER_URL" >/dev/null
		tokens_before="$(curl -sS "$AIRLOCK_ANTHROPIC_TOKENS_URL" | jq length)"
		api_mark="$(records_count anthropic)"
		k47_steps ctl-life "$AIRLOCK_ANTHROPIC_NARROWED_SECRET" get
		k47_run ctl-life "$AIRLOCK_ANTHROPIC_NARROWED_SECRET" 50m "the new grant's GET after a 3601 answer at a broker whose minter takes any lifetime"
		front_backends_stack
		curl -sS -X POST -H 'Content-Type: application/json' --data '{"expires_in": 3600}' "$AIRLOCK_ANTHROPIC_ANSWER_URL" >/dev/null
		records anthropic | jq --argjson n "$api_mark" '.[$n:]' >"$d/api-ctl-life.json"
		curl -sS "$AIRLOCK_ANTHROPIC_TOKENS_URL" >"$d/tokens.json"
		life_api="$(jq --slurpfile toks "$d/tokens.json" --argjson n "${tokens_before:-0}" '[.[] | select(.path == "/v1/organizations/workspaces") | ((.header.Authorization // [])[0] // "" | ltrimstr("Bearer ")) as $a | select($toks[0][$n:] | map(select(.expires_in == 3601) | .access_token) | index($a) != null)] | length' "$d/api-ctl-life.json")"
		control "a broker built from a copy of this module whose anthropic-federation minter takes any lifetime: the GET after the 3601 answer answered ${K47OUT[get]:-nothing}, and the stand recorded $life_api GET carrying a 3601-second token"
		judge "$([ "$(k47_code get)" = 200 ] && [ "$life_api" -ge 1 ] 2>/dev/null && echo 0 || echo 1)" \
			"negative control: with the minter taking any lifetime, the request after the 3601 answer reaches the stand-in API" \
			"it answered $(k47_code get) and the stand recorded $life_api such request(s)"
	else
		control "UNKNOWN: $K47_CONTROL_WHY"
		fail "negative control: with the minter taking any lifetime, the request after the 3601 answer reaches the stand-in API — UNKNOWN, $K47_CONTROL_WHY"
	fi

	stop_stack
}

# k47_read_out reads one run's output file into K47OUT, for a run whose k47_run ran in a subshell
# or in the background, where the K47OUT it set is not this shell's.
k47_read_out() { # <label>
	local key value
	K47OUT=()
	while IFS='=' read -r key value; do [ -n "$key" ] && K47OUT["$key"]="$value"; done <"$WORK_DIR/k47/out-$1" 2>/dev/null
}

###############################################################################################

# From here on a step that answers non-zero is a property that did not pass, not the end of the
# run: a check must report every one of its properties, and a refused request, a command that
# exited 1 and a grep that found nothing are all ordinary answers a step reads. Everything the
# run needs before this point - its arguments, its work directory and its stack - is still
# under set -e, and await ends the run itself when a stack does not come up.
#
# No check below turns it back on. A `set -e` in one of them re-arms it for every step of every
# check after it, and the step it would then end the run at is the one a failing run most needs
# reported: k15's statement count against a model that did not build, say, which would leave the
# run with no result block at all.
set +e

case "$CHECK_ID" in
k1) check_k1 ;;
k2) check_k2 ;;
k3) check_k3 ;;
k4) check_k4 ;;
k5) check_k5 ;;
k6) check_k6 ;;
k7) check_k7 ;;
k8) check_k8 ;;
k9) check_k9 ;;
k15) check_k15 ;;
k47) check_k47 ;;
esac

report
