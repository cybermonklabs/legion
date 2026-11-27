#!/usr/bin/env bash
# packages/envoy/scripts/proxy-chain-check.sh
#
# Runs the proxy chain's checks end to end on the local stack (scripts/dev-airlock.sh) and says
# whether each passed, so the implementer and the tester run the same command and read the same
# evidence. It shares judge-check.sh's stack helpers (lib/check-stack.sh) and copies its result
# block. Three parts:
#
#   k27      (the default) each broker pod signs with an intermediate from the pool, renews it
#            with 2 days left, keeps signing no later than its end while the pool fails, takes
#            the next one when the pool answers again, is not ready while it holds none, and
#            signs for a new host once the pool's policy names it. Its negative control is a
#            broker whose renewal margin is 0 (BROKER_PROXY_TEST_RENEWAL_MARGIN=0s).
#   k21      the local half of k21's control: a broker whose mounted pair is an intermediate and
#            its chain starts and signs; one whose mounted intermediate has no chain is refused.
#   compat   a client of the release before the chain (--old-client-ref, the newest
#            legion-envoy-v* tag by default), given a release whose root field carries two roots
#            - a self-signed authority as today's and the pool's test root - writes both into its
#            bundle and reaches a bound host through a broker signing under either. Its control
#            is a release carrying the pool's root alone, under which the same client cannot
#            reach a broker signing under the other.
#
# How the stack lets it drive days of a broker's life in minutes: every broker and the ca stand
# read one clock file (PROXY_CLOCK_OFFSET_FILE, BROKER_PROXY_TEST_CLOCK_OFFSET_FILE), a Go
# duration they add to the system's clock, and this script moves it by writing that file. A
# client verifying at the system's time would see each certificate dated days ahead, so a call at
# a moved clock is made by openssl s_client, under agent-secrets, through the proxy with the
# grant's own proxy URL, verifying the chain the broker presented against the pool's test root
# alone at the moved time (-attime). The first call of k27 is made by curl as well, at no offset.
#
# A step that needs another broker - the second process, a broker on another profiles file, the
# control's margin - starts one more broker on the stack's own database, stands and identities
# with its environment read out of the stack broker's (lib/exec-env.sh), and points the stack's two
# fronts at it, as judge-check.sh does. The stack runs one broker of its own (--one-broker).
#
# No line it prints is a credential: the stand-in value S records is compared by SHA-256 and
# length, and a certificate by its serial number and its dates.
#
# Usage: packages/envoy/scripts/proxy-chain-check.sh [k27|k21|compat] [--keep-stack]
#                                                     [--old-client-ref <ref>]
# Needs what dev-airlock.sh needs (bash, curl, jq, go, psql and a Postgres server named by
# DEV_BROKER_POSTGRES_URL, or Docker), and openssl 3. compat also builds the old client from git.
# A part takes the stack's start plus a few minutes; k27's wait for the retry after the pool
# answers again can take up to its 5-minute backoff.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENVOY_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
# A broker this script starts gets the stack broker's environment through its own environment,
# never through a process's arguments (exec_with_env, broker_env_of, with_broker_env).
# shellcheck source-path=SCRIPTDIR source=lib/exec-env.sh
. "$SCRIPT_DIR/lib/exec-env.sh"
# The stack helpers this script and judge-check.sh share: await, stack_start, broker_env,
# start_extra_broker, log_records and logged_addr.
CHECK_NAME=proxy-chain-check
# shellcheck source-path=SCRIPTDIR source=lib/check-stack.sh
. "$SCRIPT_DIR/lib/check-stack.sh"

usage() {
	cat <<'EOF'
usage: proxy-chain-check.sh [k27|k21|compat] [--keep-stack] [--old-client-ref <ref>]

Runs one part of the proxy chain's checks on the local stack and prints, for each property, what
it expected beside what it got.

  k27              the intermediate, its renewal, its retries, readiness and a new host (default)
  k21              the local half of k21's control: a mounted intermediate and its chain starts
  compat           a client of the release before the chain against a release of two roots
  --keep-stack     leave the stack and this run's work directory up, to look at a failure with
  --old-client-ref the git ref compat builds the old client from (default: the newest
                   legion-envoy-v* tag)
  --help           print this

It exits 0 only when every property passed, 1 when one did not and 2 on a usage error.
EOF
}

PART=""
KEEP_STACK=false
OLD_CLIENT_REF=""
while [ $# -gt 0 ]; do
	case "$1" in
	--keep-stack) KEEP_STACK=true ;;
	--old-client-ref)
		if [ $# -lt 2 ] || [ -z "${2:-}" ]; then
			echo "proxy-chain-check: --old-client-ref wants a git ref" >&2
			exit 2
		fi
		OLD_CLIENT_REF="$2"
		shift
		;;
	--help | -h)
		usage
		exit 0
		;;
	-*)
		echo "proxy-chain-check: unexpected option $1" >&2
		usage >&2
		exit 2
		;;
	*)
		if [ -n "$PART" ]; then
			echo "proxy-chain-check: one part at a time, not $PART and $1" >&2
			usage >&2
			exit 2
		fi
		PART="$1"
		;;
	esac
	shift
done
PART="${PART:-k27}"
case "$PART" in
k27 | k21 | compat) ;;
*)
	echo "proxy-chain-check: $PART is no part of this check; it runs k27, k21 and compat" >&2
	usage >&2
	exit 2
	;;
esac

WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/proxy-chain-check-$PART.XXXXXX")"
STACK_LOG="$WORK_DIR/stack.log"
STACK_PID=""
EXTRA_BROKER_PIDS=()

# kill_extra_brokers stops every broker this run started of its own and waits for each to go: a
# broker of this script's still holding a connection to the stack's database keeps dev-airlock.sh's
# own cleanup from dropping it.
kill_extra_brokers() {
	local pid
	for pid in "${EXTRA_BROKER_PIDS[@]+"${EXTRA_BROKER_PIDS[@]}"}"; do
		if [ -n "$pid" ]; then
			kill "$pid" 2>/dev/null || true
			wait "$pid" 2>/dev/null || true
		fi
	done
}

cleanup() {
	kill_extra_brokers
	if [ "$KEEP_STACK" = true ] && [ -n "$STACK_PID" ]; then
		echo "proxy-chain-check: the stack is still up (dev-airlock.sh pid $STACK_PID, log $STACK_LOG);" >&2
		echo "proxy-chain-check: stop it with kill $STACK_PID, which drops its databases" >&2
		return
	fi
	# SIGTERM, never SIGKILL: dev-airlock.sh's own EXIT trap is what stops its processes and
	# drops the two databases it made.
	if [ -n "$STACK_PID" ]; then
		kill "$STACK_PID" 2>/dev/null || true
		wait "$STACK_PID" 2>/dev/null || true
	fi
	if [ "$KEEP_STACK" = true ]; then
		echo "proxy-chain-check: work directory kept at $WORK_DIR" >&2
	else
		rm -rf "$WORK_DIR"
	fi
}
trap cleanup EXIT

# --- What a run says (judge-check.sh's) -----------------------------------------------------

EXPECTED=()
OBSERVED=()
CONTROL=()
PASSED=0
TOTAL=0
VERDICTS=()

expect() { EXPECTED+=("$1"); }
observe() {
	OBSERVED+=("$1")
	echo "proxy-chain-check: $1" >&2
}
control() { CONTROL+=("$1"); }

# assert records one property from a condition the caller has already evaluated: "0" passes and
# anything else, an empty answer included, fails.
assert() { # <status> <what the property is> <what was seen>
	TOTAL=$((TOTAL + 1))
	if [ "${1:-}" = 0 ]; then
		PASSED=$((PASSED + 1))
		VERDICTS+=("PASS: $2 — $3")
	else
		VERDICTS+=("FAIL: $2 — $3")
	fi
}

report() {
	echo
	echo "---- result block for $PART ----"
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
	# A run that asserted nothing proved nothing.
	if [ "$TOTAL" -eq 0 ]; then
		echo "check $PART: UNKNOWN (no property was evaluated)"
		return 1
	fi
	if [ "$PASSED" -eq "$TOTAL" ]; then
		echo "check $PART: passed"
		return 0
	fi
	echo "check $PART: failed ($((TOTAL - PASSED)) of $TOTAL)"
	return 1
}

digest() { printf '%s' "$1" | sha256sum | cut -c1-16; }
shape() { printf 'len=%d sha256=%s' "${#1}" "$(digest "$1")"; }

# --- The stack ---------------------------------------------------------------------------------

# start_stack runs dev-airlock.sh with one broker of its own and reads its banner
# (lib/check-stack.sh's stack_start), then the stored value the demo profile releases.
start_stack() {
	stack_start --one-broker
	DEMO_VALUE="$(jq -r '.secrets[] | select(.name | endswith("airlock-demo-token")) | .value' "$AIRLOCK_FAKE_SECRETS_FILE")"
	echo "proxy-chain-check: the stack is up (broker pid ${BROKER_PIDS[0]}, work directory $AIRLOCK_DIR)" >&2
}

# stop_broker stops one broker this run started.
stop_broker() { # <pid>
	kill "$1" 2>/dev/null || true
	wait "$1" 2>/dev/null || true
}

# front_backends points the stack's two fronts at the brokers the caller names, by writing the
# backends files they reread at each connection they accept, so every call the CLI makes - its
# requests signed for the front's own URL - reaches that broker.
front_backends() { # <api addr> <proxy addr>
	printf '%s\n' "$1" >"$AIRLOCK_DIR/api-backends"
	printf '%s\n' "$2" >"$AIRLOCK_DIR/proxy-backends"
}

front_backends_stack() { front_backends "${BROKER_API_ADDRS[0]}" "${BROKER_PROXY_ADDRS[0]}"; }

healthz() { curl -sS -o "$WORK_DIR/healthz.body" -w '%{http_code}' "http://$1/healthz" 2>/dev/null || true; }

records() { curl -sS "$1"; }
records_count() { records "$1" | jq 'length'; }
# last_authorization is the Authorization header the stand behind a records route last recorded.
last_authorization() { records "$1" | jq -r '.[-1].header.Authorization[0] // ""'; }

# --- The clock and the intermediates ----------------------------------------------------------

# set_clock_at moves the stack's clock so it reads epoch now: it writes the offset from the
# system's clock into the clock file, through a file of its own renamed over it, so a reader never
# reads half a write.
set_clock_at() { # <epoch seconds>
	local offset=$(($1 - $(date +%s)))
	printf '%ss\n' "$offset" >"$PROXY_CLOCK_OFFSET_FILE.next"
	mv "$PROXY_CLOCK_OFFSET_FILE.next" "$PROXY_CLOCK_OFFSET_FILE"
}

# clock_now is the stack's clock, in epoch seconds: the system's plus the file's offset.
clock_now() {
	local offset
	offset="$(tr -d '[:space:]' <"$PROXY_CLOCK_OFFSET_FILE")"
	echo $(($(date +%s) + ${offset%s}))
}

# intermediates prints each "proxy intermediate" line a broker's log holds, one per line:
# its serial, its start and end in epoch seconds, and its names, comma-separated.
intermediates() { # <log>
	log_records 'proxy intermediate' "$1" | jq -r '.[] |
		"\(.serial) \(.not_before | fromdateiso8601) \(.not_after | fromdateiso8601) \(.names | split(" ") | join(","))"' 2>/dev/null || true
}

intermediate_count() { intermediates "$1" | grep -c . || true; }

# await_count waits until the counter function, run on log, reads n or more, and answers whether
# that happened within the seconds given. AWAITED is the whole seconds it waited, rounded down, or
# the seconds given when the count never came.
await_count() { # <counter> <log> <n> <seconds>
	local counter="$1" i
	AWAITED=0
	for ((i = 0; i < $4 * 2; i++)); do
		if [ "$("$counter" "$2")" -ge "$3" ]; then
			AWAITED=$((i / 2))
			return 0
		fi
		sleep 0.5
	done
	AWAITED="$4"
	return 1
}

# await_intermediates waits for a broker's log to hold n intermediate lines.
await_intermediates() { # <log> <n> <seconds>
	await_count intermediate_count "$@"
}

# failed_requests is how many "proxy intermediate request failed" lines a broker's log holds, and
# last_failure the error of the last one.
failed_requests() { log_records 'proxy intermediate request failed' "$1" | jq 'length'; }
last_failure() { log_records 'proxy intermediate request failed' "$1" | jq -r '.[-1].error // ""'; }

await_failed_requests() { # <log> <n> <seconds>
	await_count failed_requests "$@"
}

# The serials the broker logs are lower-case hex with no leading zero; openssl prints upper case,
# with one when it pads to whole bytes.
serial_norm() {
	local s="${1,,}"
	s="${s#serial=}"
	while [[ "$s" == 0?* ]]; do s="${s#0}"; done
	printf '%s' "${s:-0}"
}

ca_post() { # <path> [body]
	curl -sS -f -o /dev/null -X POST -H 'Content-Type: application/json' --data "${2:-}" "$PROXY_CA_URL$1"
}

# policy_of is the ca stand's policy a profiles file gives: the names of its hosts.
policy_of() { jq -c -f "$SCRIPT_DIR/lib/ca-policy.jq" "$1"; }

# --- One call through the proxy, verified at a moment ----------------------------------------

# The client a call at a moved clock runs under agent-secrets: openssl s_client through the proxy
# URL the release names, its password read from the environment and not from an argument, the
# placeholder agent-secrets put in AIRLOCK_DEMO_TOKEN in the Authorization header, and the chain
# the broker presented verified against the one root the caller names at the moment it names.
write_client() {
	cat >"$WORK_DIR/s-client.sh" <<'CLIENT'
#!/usr/bin/env bash
# s-client.sh <host> <path> <attime> <root file> <out dir>
set -u
host=$1 path=$2 at=$3 root=$4 out=$5
mkdir -p "$out"
proxy=${HTTPS_PROXY#*://}
proxy=${proxy%/}
userinfo=${proxy%@*}
export S_CLIENT_PROXY_PASSWORD=${userinfo#*:}
# The call trusts the caller's root alone, not the release's trust bundle, so a chain to any
# other root cannot verify.
printf 'GET %s HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nConnection: close\r\n\r\n' "$path" "$host" "$AIRLOCK_DEMO_TOKEN" |
	openssl s_client -connect "$host:443" -servername "$host" -proxy "${proxy##*@}" \
		-proxy_user "${userinfo%%:*}" -proxy_pass env:S_CLIENT_PROXY_PASSWORD \
		-no-CApath -no-CAstore -CAfile "$root" -verify_return_error -attime "$at" \
		-showcerts -ign_eof >"$out/transcript" 2>"$out/stderr"
echo "$?" >"$out/status"
CLIENT
	chmod +x "$WORK_DIR/s-client.sh"
}

# call reads one call: the client's exit status, the HTTP status, how many certificates the
# broker presented, the leaf's subject, start and end, and the serial and issuer of the
# certificate after it, into CALL_*. A call that never ran reads as status "none".
call() { # <name> <host> <path> <attime> <root file>
	local out="$WORK_DIR/calls/$1"
	rm -rf "$out"
	agent-secrets AIRLOCK_DEMO_TOKEN -- "$WORK_DIR/s-client.sh" "$2" "$3" "$4" "$5" "$out" >"$WORK_DIR/calls-$1.out" 2>&1 || true
	CALL_STATUS="$(cat "$out/status" 2>/dev/null || echo none)"
	CALL_HTTP="$(grep -m1 -oE '^HTTP/1\.1 [0-9]{3}' "$out/transcript" 2>/dev/null | cut -d' ' -f2 || true)"
	awk -v dir="$out" '/-----BEGIN CERTIFICATE-----/ { n++; f = dir "/cert" n ".pem" } f { print > f } /-----END CERTIFICATE-----/ { f = "" }' "$out/transcript" 2>/dev/null || true
	CALL_CERTS="$(find "$out" -name 'cert*.pem' 2>/dev/null | grep -c . || true)"
	CALL_LEAF_SUBJECT="" CALL_LEAF_START=0 CALL_LEAF_END=0 CALL_SIGNER_SERIAL="" CALL_SIGNER_ISSUER=""
	if [ -s "$out/cert1.pem" ]; then
		CALL_LEAF_SUBJECT="$(openssl x509 -in "$out/cert1.pem" -noout -subject -nameopt RFC2253 | sed 's/^subject=//')"
		CALL_LEAF_START="$(date -d "$(openssl x509 -in "$out/cert1.pem" -noout -startdate | cut -d= -f2)" +%s)"
		CALL_LEAF_END="$(date -d "$(openssl x509 -in "$out/cert1.pem" -noout -enddate | cut -d= -f2)" +%s)"
	fi
	if [ -s "$out/cert2.pem" ]; then
		CALL_SIGNER_SERIAL="$(serial_norm "$(openssl x509 -in "$out/cert2.pem" -noout -serial)")"
		CALL_SIGNER_ISSUER="$(openssl x509 -in "$out/cert2.pem" -noout -issuer -nameopt RFC2253 | sed 's/^issuer=//')"
	fi
	CALL_SUMMARY="client exit $CALL_STATUS, HTTP ${CALL_HTTP:-none}, $CALL_CERTS certificate(s) presented, leaf $CALL_LEAF_SUBJECT from $(date -u -d "@$CALL_LEAF_START" +%FT%TZ) to $(date -u -d "@$CALL_LEAF_END" +%FT%TZ) ($(((CALL_LEAF_END - CALL_LEAF_START) / 60)) minutes), signed by serial ${CALL_SIGNER_SERIAL:-none} of $CALL_SIGNER_ISSUER"
	if [ "$CALL_STATUS" != 0 ]; then
		CALL_SUMMARY="$CALL_SUMMARY; the client said: $(grep -m2 -iE 'error|verify' "$out/stderr" "$out/transcript" 2>/dev/null | tr '\n' ' ' | cut -c1-300)"
	fi
}

day=86400

###############################################################################################

check_k27() {
	expect "the first call's leaf chains through an intermediate to the test root, and lasts at most 25 hours"
	expect "at 5 days and 1 minute, the broker logs a new intermediate and the call succeeds, with no person's step"
	expect "while the stand-in fails (from 4 days on), the broker gets no new intermediate, and the leaf at 6 days and 12 hours ends no later than the old intermediate's end at 7 days"
	expect "when the stand-in answers again, the broker logs a new intermediate"
	expect "a second broker process started while the stand-in fails is not ready"
	expect "after adding a host and applying the stand-in's policy first, the call to the new host succeeds after the roll"
	expect "negative control: with the renewal margin at 0, the call at 5 days and 1 minute still uses the first intermediate, and no renewal line is logged"

	start_stack
	write_client
	local log="${BROKER_LOGS[0]}" root="$PROXY_ROOT_FILE" first serial0 start0 end0 names0
	first="$(intermediates "$log" | head -n1)"
	read -r serial0 start0 end0 names0 <<<"$first"
	observe "step 1: the stack's broker (pid ${BROKER_PIDS[0]}) logged its first intermediate: serial ${serial0:-none}, $(date -u -d "@${start0:-0}" +%FT%TZ) to $(date -u -d "@${end0:-0}" +%FT%TZ), names $names0"

	# Step 1: the first call, by curl at no offset and by s_client against the root alone.
	local curl_status s_mark
	s_mark="$(records_count "$AIRLOCK_S_RECORDS")"
	# The command writes curl's status to a file of its own: agent-secrets prints lines of its own
	# on the same streams. Single quotes, since each variable is the command's, not this script's.
	# shellcheck disable=SC2016
	agent-secrets AIRLOCK_DEMO_TOKEN -- sh -c 'printf "Authorization: Bearer %s" "$AIRLOCK_DEMO_TOKEN" | curl -sS -o /dev/null -w "%{http_code}" -H @- "$AIRLOCK_S_URL/ok" >"$1"' \
		first-curl "$WORK_DIR/curl-status" >"$WORK_DIR/curl.log" 2>&1 || true
	curl_status="$(cat "$WORK_DIR/curl-status" 2>/dev/null || echo none)"
	local s_curl
	s_curl="$(last_authorization "$AIRLOCK_S_RECORDS")"
	call first s.airlock.test /ok "$(clock_now)" "$root"
	observe "step 1: curl under agent-secrets answered $curl_status, and S recorded Authorization $(shape "$s_curl") against the stored value's $(shape "Bearer $DEMO_VALUE") ($(($(records_count "$AIRLOCK_S_RECORDS") - s_mark)) new records); s_client at no offset: $CALL_SUMMARY"
	assert "$([ "$curl_status" = 200 ] && [ "$s_curl" = "Bearer $DEMO_VALUE" ] && echo 0 || echo 1)" \
		"the first call by curl succeeds and S gets the real value" "curl $curl_status, S's Authorization $(shape "$s_curl")"
	assert "$([ "$CALL_STATUS" = 0 ] && [ "$CALL_HTTP" = 200 ] && [ "$CALL_CERTS" = 2 ] && [ "$CALL_SIGNER_SERIAL" = "$(serial_norm "$serial0")" ] && echo 0 || echo 1)" \
		"the first call's leaf chains through the broker's intermediate to the test root" "$CALL_SUMMARY; the broker's intermediate is serial $serial0"
	assert "$([ "$CALL_CERTS" -ge 1 ] && [ "$CALL_LEAF_END" -gt 0 ] && [ $((CALL_LEAF_END - CALL_LEAF_START)) -le $((25 * 3600)) ] && [ "$CALL_LEAF_END" -le "${end0:-0}" ] && echo 0 || echo 1)" \
		"the first call's leaf lasts at most 25 hours and no later than its intermediate" "$CALL_CERTS certificate(s), $(((CALL_LEAF_END - CALL_LEAF_START) / 60)) minutes"

	# Step 2: 5 days and 1 minute after the intermediate's start.
	local set_at renewal_seconds second serial1
	set_clock_at $((start0 + 5 * day + 60))
	set_at="$(date +%s)"
	await_intermediates "$log" 2 120 || true
	renewal_seconds=$(($(date +%s) - set_at))
	second="$(intermediates "$log" | sed -n 2p)"
	serial1="${second%% *}"
	call renewed s.airlock.test /ok "$(clock_now)" "$root"
	observe "step 2: clock at 5 days and 1 minute after $serial0's start; the same broker (pid ${BROKER_PIDS[0]}, $(kill -0 "${BROKER_PIDS[0]}" 2>/dev/null && echo running || echo gone)) logged ${second:+its next intermediate: }${second:-no new intermediate} ${renewal_seconds}s after the clock moved; the call: $CALL_SUMMARY"
	assert "$([ -n "$serial1" ] && [ "$serial1" != "$serial0" ] && kill -0 "${BROKER_PIDS[0]}" 2>/dev/null && echo 0 || echo 1)" \
		"at 5 days and 1 minute the broker logs a new intermediate, with no restart and no person's step" "first $serial0, then ${serial1:-none}"
	assert "$([ "$CALL_STATUS" = 0 ] && [ "$CALL_HTTP" = 200 ] && [ "$CALL_SIGNER_SERIAL" = "$(serial_norm "${serial1:-x}")" ] && echo 0 || echo 1)" \
		"the call at 5 days and 1 minute succeeds, signed under the new intermediate" "$CALL_SUMMARY"

	# Negative control: the renewal margin at 0, on a broker of its own.
	local control_line control_serial control_start wait_for
	start_extra_broker margin-zero BROKER_PROXY_TEST_RENEWAL_MARGIN=0s
	local control_pid="$EXTRA_PID" control_log="$EXTRA_LOG"
	await_intermediates "$control_log" 1 60 || true
	control_line="$(intermediates "$control_log" | head -n1)"
	read -r control_serial control_start _ <<<"$control_line"
	set_clock_at $((control_start + 5 * day + 60))
	wait_for=$((renewal_seconds * 3 > 20 ? renewal_seconds * 3 : 20))
	sleep "$wait_for"
	front_backends "$EXTRA_API_ADDR" "$EXTRA_PROXY_ADDR"
	call margin-zero s.airlock.test /ok "$(clock_now)" "$root"
	front_backends_stack
	local control_count margin
	control_count="$(intermediate_count "$control_log")"
	margin="$(log_records 'proxy certificate authority' "$control_log" | jq -r '.[0].renew_before // ""')"
	control "a broker started with BROKER_PROXY_TEST_RENEWAL_MARGIN=0s (its log: renew_before=$margin) took its first intermediate, serial $control_serial; with the clock at 5 days and 1 minute after its start, ${wait_for}s later (three times the ${renewal_seconds}s the stack's broker took to renew, 20 at least) it had logged $control_count intermediate line(s); the call through it: $CALL_SUMMARY"
	assert "$([ "$control_count" = 1 ] && [ "$CALL_STATUS" = 0 ] && [ "$CALL_SIGNER_SERIAL" = "$(serial_norm "${control_serial:-x}")" ] && echo 0 || echo 1)" \
		"negative control: with the renewal margin at 0, the call at 5 days and 1 minute still uses the first intermediate, and no renewal line is logged" \
		"$control_count line(s), the call signed by ${CALL_SIGNER_SERIAL:-none} against the first $control_serial"
	stop_broker "$control_pid"

	# Step 3: start again; the stand-in fails from 4 days on; the leaf at 6 days and 12 hours.
	start_extra_broker again
	local again_pid="$EXTRA_PID" again_log="$EXTRA_LOG" again_api="$EXTRA_API_ADDR" again_proxy="$EXTRA_PROXY_ADDR"
	await_intermediates "$again_log" 1 60 || true
	local again_line again_serial again_start again_end
	again_line="$(intermediates "$again_log" | head -n1)"
	read -r again_serial again_start again_end _ <<<"$again_line"
	set_clock_at $((again_start + 4 * day))
	ca_post /fail || true
	set_clock_at $((again_start + 6 * day + 12 * 3600))
	await_failed_requests "$again_log" 2 60 || true
	local failures_while
	failures_while="$(failed_requests "$again_log")"
	front_backends "$again_api" "$again_proxy"
	call failing s.airlock.test /ok "$(clock_now)" "$root"
	observe "step 3: a broker started again (pid $again_pid) took serial $again_serial, $(date -u -d "@${again_start:-0}" +%FT%TZ) to $(date -u -d "@${again_end:-0}" +%FT%TZ); the stand-in failed from 4 days on, and at 6 days and 12 hours the broker had logged $failures_while failed request(s) (last: $(last_failure "$again_log" | cut -c1-160)) and $(intermediate_count "$again_log") intermediate line(s); the call: $CALL_SUMMARY"
	assert "$([ "$(intermediate_count "$again_log")" = 1 ] && [ "$failures_while" -ge 1 ] && echo 0 || echo 1)" \
		"while the stand-in fails, the broker gets no new intermediate and retries" "$failures_while failed request(s), $(intermediate_count "$again_log") intermediate(s)"
	assert "$([ "$CALL_STATUS" = 0 ] && [ "$CALL_HTTP" = 200 ] && [ "$CALL_SIGNER_SERIAL" = "$(serial_norm "${again_serial:-x}")" ] && [ "$CALL_LEAF_END" -le "${again_end:-0}" ] && echo 0 || echo 1)" \
		"the leaf at 6 days and 12 hours ends no later than the old intermediate's end at 7 days" \
		"leaf end $(date -u -d "@$CALL_LEAF_END" +%FT%TZ), the intermediate's $(date -u -d "@${again_end:-0}" +%FT%TZ)"

	# Step 4: the stand-in answers again.
	ca_post /answer || true
	local answered=failed
	await_intermediates "$again_log" 2 360 && answered=ok
	observe "step 4: the stand-in answers again; the broker logged $(intermediate_count "$again_log") intermediate line(s) within ${AWAITED}s (the next: $(intermediates "$again_log" | sed -n 2p))"
	assert "$([ "$answered" = ok ] && echo 0 || echo 1)" \
		"when the stand-in answers again, the broker logs a new intermediate" "$(intermediate_count "$again_log") line(s) after ${AWAITED}s"
	front_backends_stack

	# Step 5: a second process started while the stand-in fails.
	ca_post /fail || true
	start_extra_broker second
	local second_pid="$EXTRA_PID" second_log="$EXTRA_LOG" second_api="$EXTRA_API_ADDR"
	await_failed_requests "$second_log" 1 60 || true
	local second_health
	second_health="$(healthz "$second_api")"
	observe "step 5: a second broker process (pid $second_pid) started while the stand-in fails: $(failed_requests "$second_log") failed request(s), $(intermediate_count "$second_log") intermediate(s), GET /healthz $second_health $(cut -c1-200 "$WORK_DIR/healthz.body" 2>/dev/null)"
	assert "$([ "$second_health" = 503 ] && grep -q '"code":"NO_INTERMEDIATE"' "$WORK_DIR/healthz.body" && [ "$(intermediate_count "$second_log")" = 0 ] && echo 0 || echo 1)" \
		"the second process is not ready while the stand-in fails" "GET /healthz $second_health"
	ca_post /answer || true
	stop_broker "$second_pid"
	stop_broker "$again_pid"

	# Step 6: add a host, apply the stand-in's policy first, roll, call the new host.
	local wider="$WORK_DIR/profiles-with-t.json"
	jq '(.kinds[] | select(.name == "stest") | .hosts) += ["t.airlock.test"]' "$AIRLOCK_PROFILES_FILE" >"$wider"
	# The order matters: a broker rolled onto the wider file before the policy names the new host
	# is refused its intermediate and is not ready.
	start_extra_broker early BROKER_PROFILES_FILE="$wider"
	local early_pid="$EXTRA_PID" early_log="$EXTRA_LOG" early_api="$EXTRA_API_ADDR"
	await_failed_requests "$early_log" 1 60 || true
	local early_health early_code early_failure
	early_health="$(healthz "$early_api")"
	early_code="$(jq -r '.code // ""' "$WORK_DIR/healthz.body" 2>/dev/null || true)"
	early_failure="$(last_failure "$early_log")"
	observe "step 6: a broker rolled onto the wider file before the policy was applied: GET /healthz $early_health $early_code, its refusal: $(cut -c1-220 <<<"$early_failure")"
	stop_broker "$early_pid"
	ca_post /policy "$(policy_of "$wider")" || true
	start_extra_broker rolled BROKER_PROFILES_FILE="$wider"
	local rolled_pid="$EXTRA_PID" rolled_log="$EXTRA_LOG"
	await_intermediates "$rolled_log" 1 60 || true
	front_backends "$EXTRA_API_ADDR" "$EXTRA_PROXY_ADDR"
	local t_mark t_auth
	t_mark="$(records_count "$AIRLOCK_T_RECORDS")"
	call new-host t.airlock.test /ok "$(clock_now)" "$root"
	t_auth="$(last_authorization "$AIRLOCK_T_RECORDS")"
	observe "step 6: the policy applied ($(policy_of "$wider")), then a broker rolled onto the wider file (pid $rolled_pid) logged $(intermediates "$rolled_log" | head -n1); the call to t.airlock.test: $CALL_SUMMARY; T recorded $(($(records_count "$AIRLOCK_T_RECORDS") - t_mark)) request(s), its Authorization $(shape "$t_auth") against the stored value's $(shape "Bearer $DEMO_VALUE")"
	assert "$([ "$early_health" = 503 ] && [ "$early_code" = NO_INTERMEDIATE ] && grep -qF 't.airlock.test' <<<"$early_failure" && echo 0 || echo 1)" \
		"a broker rolled before the policy names the new host is not ready, refused for that host" "GET /healthz $early_health $early_code"
	assert "$([ "$CALL_STATUS" = 0 ] && [ "$CALL_HTTP" = 200 ] && [ "$t_auth" = "Bearer $DEMO_VALUE" ] && echo 0 || echo 1)" \
		"after adding a host and applying the stand-in's policy first, the call to the new host succeeds after the roll" "$CALL_SUMMARY"
	front_backends_stack
	stop_broker "$rolled_pid"
}

# mounted_intermediate asks the ca stand for an intermediate for a key made here, as a mounted
# pair would be made, and writes the pair: the certificate file holding the intermediate and, with
# chain, the root after it.
mounted_intermediate() { # <dir> <with chain: yes|no>
	mkdir -p "$1"
	openssl ecparam -name prime256v1 -genkey -noout -out "$1/key.pem" 2>/dev/null
	openssl req -new -key "$1/key.pem" -subj '/CN=mounted proxy intermediate' -out "$1/csr.pem" 2>/dev/null
	jq -n --rawfile csr "$1/csr.pem" '{pemCsr: $csr, lifetime: "604800s"}' |
		curl -sS -f -X POST -H 'Content-Type: application/json' --data @- \
			"$PROXY_CA_URL/v1/$PROXY_CA_POOL/certificates?certificateId=mounted-$RANDOM" >"$1/answer.json"
	jq -r .pemCertificate "$1/answer.json" >"$1/chain.pem"
	if [ "$2" = yes ]; then cat "$PROXY_ROOT_FILE" >>"$1/chain.pem"; fi
}

check_k21() {
	expect "the broker started from the head with BROKER_AIRLOCK_CA_CERT_FILE and BROKER_AIRLOCK_CA_KEY_FILE naming a mounted intermediate and its chain starts, and signs a leaf a client verifies against the root alone"
	expect "control: the same start with the intermediate alone, no chain to its root, is refused naming that"

	start_stack
	write_client
	mounted_intermediate "$WORK_DIR/mounted" yes
	local serial
	serial="$(serial_norm "$(openssl x509 -in "$WORK_DIR/mounted/chain.pem" -noout -serial)")"
	start_extra_broker mounted BROKER_PROXY_CA_POOL= BROKER_PROXY_TEST_CA_URL= \
		BROKER_AIRLOCK_CA_CERT_FILE="$WORK_DIR/mounted/chain.pem" BROKER_AIRLOCK_CA_KEY_FILE="$WORK_DIR/mounted/key.pem"
	local mounted_pid="$EXTRA_PID" mounted_log="$EXTRA_LOG" health line
	await_intermediates "$mounted_log" 1 30 || true
	line="$(log_records 'proxy intermediate' "$mounted_log" | jq -c '.[0] // {}')"
	health="$(healthz "$EXTRA_API_ADDR")"
	front_backends "$EXTRA_API_ADDR" "$EXTRA_PROXY_ADDR"
	call mounted s.airlock.test /ok "$(clock_now)" "$PROXY_ROOT_FILE"
	front_backends_stack
	observe "a broker on a mounted intermediate (serial $serial, the ca stand's answer for a key made here) and its root started (pid $mounted_pid): its line $line, GET /healthz $health; the call: $CALL_SUMMARY"
	assert "$([ "$health" = 200 ] && [ "$(jq -r .self_signed <<<"$line")" = false ] && [ "$(jq -r .serial <<<"$line")" = "$serial" ] && echo 0 || echo 1)" \
		"a mounted intermediate and its chain starts, ready, logging that intermediate" "GET /healthz $health, line $line"
	assert "$([ "$CALL_STATUS" = 0 ] && [ "$CALL_HTTP" = 200 ] && [ "$CALL_SIGNER_SERIAL" = "$serial" ] && echo 0 || echo 1)" \
		"a leaf it signs verifies against the root alone, through the mounted intermediate" "$CALL_SUMMARY"
	stop_broker "$mounted_pid"

	mounted_intermediate "$WORK_DIR/unchained" no
	local refused="$WORK_DIR/broker-unchained.log" status=0
	broker_env BROKER_LISTEN_ADDR="127.0.0.1:0" BROKER_AIRLOCK_LISTEN_ADDR="127.0.0.1:0" BROKER_METRICS_LISTEN_ADDR="127.0.0.1:0" \
		BROKER_PROXY_CA_POOL= BROKER_PROXY_TEST_CA_URL= \
		BROKER_AIRLOCK_CA_CERT_FILE="$WORK_DIR/unchained/chain.pem" BROKER_AIRLOCK_CA_KEY_FILE="$WORK_DIR/unchained/key.pem"
	# Bounded, so a broker that wrongly started is a failed control rather than a run that hangs.
	(with_broker_env "$(command -v timeout)" 60 "$BROKER_BIN") >"$refused" 2>&1 || status=$?
	control "the same start with the intermediate alone and no root after it exited $status: $(grep -m1 -o 'no chain to its root[^"]*' "$refused" | cut -c1-160)"
	assert "$([ "$status" != 0 ] && grep -q 'no chain to its root' "$refused" && ! grep -q '"message":"broker listening"' "$refused" && echo 0 || echo 1)" \
		"control: a mounted intermediate with no chain is refused at start, naming that" "exit $status"
}

check_compat() {
	local ref="${OLD_CLIENT_REF:-}"
	if [ -z "$ref" ]; then
		ref="$(git -C "$ENVOY_DIR" tag -l 'legion-envoy-v*' --sort=-v:refname | head -n1)"
	fi
	expect "a client of the release before the chain ($ref), given a release whose root field carries two PEM blocks - a self-signed authority as today's and the pool's test root - writes both into its bundle"
	expect "and reaches a bound host through a broker signing under either root"
	expect "control: given a release carrying the pool's root alone, the same client cannot verify a broker signing under the other"

	start_stack
	# The old client, built from the tag's own tree.
	local old="$WORK_DIR/old"
	mkdir -p "$old/src"
	# From the repository's root, where the tag's packages/envoy is that path.
	if ! git -C "$ENVOY_DIR/../.." archive "$ref" packages/envoy | tar -x -C "$old/src"; then
		assert 1 "the old client is built from $ref" "git archive of $ref failed"
		return
	fi
	echo "proxy-chain-check: building agent-secrets from $ref..." >&2
	if ! (cd "$old/src/packages/envoy" && GOWORK=off go build -o "$old/agent-secrets" ./cmd/cipher) >"$old/build.log" 2>&1; then
		assert 1 "the old client is built from $ref" "$(tail -n 5 "$old/build.log" | tr '\n' ' ')"
		return
	fi

	local today_root="$PROXY_STANDS_CA_CERT_FILE" pool_root="$PROXY_ROOT_FILE"
	# Today's form: the mounted self-signed authority, with the pool's root among the release
	# roots; and the pool's form, with today's authority among them, as rollout steps 2 and 4 have.
	start_extra_broker today BROKER_PROXY_CA_POOL= BROKER_PROXY_TEST_CA_URL= \
		BROKER_AIRLOCK_CA_CERT_FILE="$PROXY_STANDS_CA_CERT_FILE" BROKER_AIRLOCK_CA_KEY_FILE="$PROXY_STANDS_CA_KEY_FILE" \
		BROKER_PROXY_RELEASE_ROOTS_FILE="$pool_root"
	local today_api="$EXTRA_API_ADDR" today_proxy="$EXTRA_PROXY_ADDR"
	start_extra_broker pool BROKER_PROXY_RELEASE_ROOTS_FILE="$today_root"
	local pool_api="$EXTRA_API_ADDR" pool_proxy="$EXTRA_PROXY_ADDR"
	await_intermediates "$EXTRA_LOG" 1 60 || true

	# A certificate's base64 body, one line: the same body is the same DER, so a bundle holds a
	# root exactly when its own one-line text holds the root's.
	body_of() { sed '/^-----/d' "$1" | tr -d '\r\n'; }
	local today_body pool_body
	today_body="$(body_of "$today_root")"
	pool_body="$(body_of "$pool_root")"
	# old_call runs the old client for one name through the brokers the fronts point at: it reads
	# whether the bundle the client wrote holds each root, and curl's answer from S at the path
	# given. Each step asks for a name no step before it asked for, so its grant, and the release
	# the client writes its bundle from, are the broker's under test and no earlier one.
	old_call() { # <step> <NAME> <path>
		local out="$WORK_DIR/old-$1" bundle
		mkdir -p "$out"
		# shellcheck disable=SC2016 # each variable is the command's, not this script's
		"$old/agent-secrets" "$2" -- sh -c '
			cp "$SSL_CERT_FILE" "$1/bundle.pem"
			printf "Authorization: Bearer %s" "$(printenv "$2")" | curl -sS -o /dev/null -w "%{http_code}" -H @- "$AIRLOCK_S_URL$3" >"$1/status" 2>"$1/curl.err"' \
			old-call "$out" "$2" "$3" >"$out/run.log" 2>&1 || true
		bundle="$(tr -d '\r\n' <"$out/bundle.pem" 2>/dev/null || true)"
		OLD_STATUS="$(cat "$out/status" 2>/dev/null || echo none)"
		OLD_ERROR="$(head -c 200 "$out/curl.err" 2>/dev/null | tr '\n' ' ')"
		OLD_HAS_TODAY=no OLD_HAS_POOL=no
		if [[ -n "$today_body" && "$bundle" == *"$today_body"* ]]; then OLD_HAS_TODAY=yes; fi
		if [[ -n "$pool_body" && "$bundle" == *"$pool_body"* ]]; then OLD_HAS_POOL=yes; fi
	}

	front_backends "$today_api" "$today_proxy"
	old_call today AIRLOCK_DEMO_TOKEN /ok
	observe "the $ref client through a broker signing with a self-signed authority (today's form) whose release carries both roots: bundle holds today's authority $OLD_HAS_TODAY, the pool's root $OLD_HAS_POOL; curl to S answered $OLD_STATUS $OLD_ERROR"
	assert "$([ "$OLD_HAS_TODAY" = yes ] && [ "$OLD_HAS_POOL" = yes ] && [ "$OLD_STATUS" = 200 ] && echo 0 || echo 1)" \
		"the old client writes both roots into its bundle and reaches S through a broker signing under today's authority" "bundle $OLD_HAS_TODAY/$OLD_HAS_POOL, curl $OLD_STATUS"

	front_backends "$pool_api" "$pool_proxy"
	old_call pool AIRLOCK_GOOGLE_LOGIN /ok
	observe "the $ref client through a broker signing with the pool's intermediate whose release carries both roots: bundle holds today's authority $OLD_HAS_TODAY, the pool's root $OLD_HAS_POOL; curl to S answered $OLD_STATUS $OLD_ERROR"
	assert "$([ "$OLD_HAS_TODAY" = yes ] && [ "$OLD_HAS_POOL" = yes ] && [ "$OLD_STATUS" = 200 ] && echo 0 || echo 1)" \
		"the old client writes both roots into its bundle and reaches S through a broker signing under the pool's root" "bundle $OLD_HAS_TODAY/$OLD_HAS_POOL, curl $OLD_STATUS"

	# Control: the release from the stack's own broker, which carries the pool's root alone, and
	# the proxy of the broker signing under today's authority.
	# The control's name allows GET /read alone, which is the path it calls.
	front_backends "${BROKER_API_ADDRS[0]}" "$today_proxy"
	old_call control AIRLOCK_SCOPED_TOKEN /read
	control "the $ref client given the release of a broker with no release roots (the pool's root alone), through the proxy of the broker signing under today's authority: bundle holds today's authority $OLD_HAS_TODAY, the pool's root $OLD_HAS_POOL; curl to S answered $OLD_STATUS $OLD_ERROR"
	assert "$([ "$OLD_HAS_TODAY" = no ] && [ "$OLD_HAS_POOL" = yes ] && [ "$OLD_STATUS" != 200 ] && grep -qi certificate <<<"$OLD_ERROR" && echo 0 || echo 1)" \
		"control: with one root in the release the old client cannot verify a broker signing under the other" "bundle $OLD_HAS_TODAY/$OLD_HAS_POOL, curl $OLD_STATUS $OLD_ERROR"
	front_backends_stack
}

###############################################################################################

# From here on a step that answers non-zero is a property that did not pass, not the end of the
# run (judge-check.sh's rule).
set +e

case "$PART" in
k27) check_k27 ;;
k21) check_k21 ;;
compat) check_compat ;;
esac

report
