#!/usr/bin/env bash
# packages/envoy/scripts/airlock-check.sh
#
# Runs check k9 of the airlock's verification document end to end: under a ten-minute load of
# generated traffic no call fails, the proxy adds at most 100 ms at the median against a plain
# tunnel, and each broker process passes at most half its wall time in CPU and at most 1 GiB of
# memory, which are the document's rules for the broker. The `broker-multiprocess` job of
# `ci.yaml` runs it in merge groups and on a pull request that carries the `full-ci` label.
#
# The script starts the local stack the check needs (scripts/dev-airlock.sh), runs the check's
# steps against it, runs the negative control on a second stack, stops the stack, and prints:
#
#   - a result block: what the check expected, what this run observed, and what the negative
#     control did;
#   - one `CHECK k9-<property>: PASS` or `CHECK k9-<property>: FAIL: <what was seen>` line for
#     each property the check's outcome sentence names, and one for its negative control,
#     because a control that does not behave as the check says leaves the check proving
#     nothing; a PASS line follows a line saying what was seen;
#   - one last line, `check k9: passed` or `check k9: failed (<n> of <m>)`.
#
# No line it prints is a credential. A value is compared by its SHA-256 (the first 16 hex
# digits) and its length, and every line that compares one says so; that is a rule of the
# verification document itself, and it holds for the stand-in values of a local stack too.
#
# The document's other checks are Go tests: k3, k4, k5 and k8a are
# TestAMethodOrPathTheEntryDoesNotAllowIsRefused and TestTheProxyRefusesTheAddressesItMustNeverDial
# (internal/broker/airlockproxy), TestARereadOfASecretReachesTheAirlocksProxy and
# TestAirlockCheckRefusesAFileTheBrokerWouldNotServe (cmd/broker); k1's memory and file scan is a
# row of TestExecFormRunsACommandForANameBehindTheAirlock (cmd/agent-secrets); the swaps k6 and
# k7 drove real clients through are airlockproxy's Azure, AWS, GitHub App and gRPC tests; k15 is
# the airlock-lean job and scripts/airlock-mutants.sh. k2 was a one-time check of a release from
# before the airlock.
#
# Usage: packages/envoy/scripts/airlock-check.sh k9 [--keep-stack]
#        --keep-stack leaves the stack and the work directory up for a person to look at a
#        failure with. --help prints this usage. A usage error exits 2.
#
# Needs bash, curl, jq, go, python3, openssl, node, psql and git; a missing one is a
# `CHECK setup: FAIL` naming it. DEV_BROKER_POSTGRES_URL is passed to dev-airlock.sh as it is,
# so a machine with no container runtime names its test server there.
#
# k9 is a long run: ten minutes of connections and two stacks, about twenty-five minutes in all.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

usage() {
	cat <<'EOF'
usage: airlock-check.sh k9 [--keep-stack]

Runs check k9 of the airlock's verification document and prints one CHECK line for each
property the check's outcome sentence names, after a line saying what it got.

  k9             the one check this script runs
  --keep-stack   leave the stack and this run's work directory up, to look at a failure with
  --help         print this

It exits 0 only when every property passed, 1 when one did not and 2 on a usage error. It
takes about twenty-five minutes.
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
k9) ;;
*)
	echo "airlock-check: $CHECK_ID is not k9, the one check this script runs" >&2
	usage >&2
	exit 2
	;;
esac

for tool in curl jq go python3 openssl node psql git; do
	if ! command -v "$tool" >/dev/null 2>&1; then
		echo "CHECK setup: FAIL: $tool is not on PATH"
		exit 1
	fi
done

WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/airlock-check-$CHECK_ID.XXXXXX")"
STACK_LOG="$WORK_DIR/stack.log"
STACK_PID=""
HELPER_PIDS=()

cleanup() {
	local pid
	for pid in "${HELPER_PIDS[@]+"${HELPER_PIDS[@]}"}"; do
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
# Three lists make the result block, and one pair of counters makes the verdict. A property is
# recorded with judge and nothing else decides the exit status.

EXPECTED=()
OBSERVED=()
CONTROL=()
PASSED=0
TOTAL=0
VERDICTS=()

expect() { EXPECTED+=("$1"); }
observe() { OBSERVED+=("$1"); }
control() { CONTROL+=("$1"); }

# judge records one property from a condition the caller has already evaluated: "0" passes and
# anything else, an empty answer included, fails. A step that could not be evaluated at all is
# a property that was not proved, which is a failure and never a silent pass.
judge() { # <status> <name> <what the property is> <what was seen>
	TOTAL=$((TOTAL + 1))
	if [ "${1:-}" = 0 ]; then
		PASSED=$((PASSED + 1))
		VERDICTS+=("   $3 — $4" "CHECK k9-$2: PASS")
	else
		VERDICTS+=("CHECK k9-$2: FAIL: $3 — $4")
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
	if [ "$TOTAL" -gt 0 ] && [ "$PASSED" -eq "$TOTAL" ]; then
		echo "check $CHECK_ID: passed"
		return 0
	fi
	echo "check $CHECK_ID: failed ($((TOTAL - PASSED)) of $TOTAL)"
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
			echo "CHECK setup: FAIL: $1 exited before it was ready; see $3"
			tail -n 20 "$3" >&2 || true
			exit 1
		fi
		sleep 0.5
	done
	echo "CHECK setup: FAIL: $1 was not ready within $2 seconds; see $3"
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
	# dev-broker.sh builds this binary with the rest of the stack's binaries, under the work
	# directory its own banner named. k9's load driver is a subcommand of it.
	STANDS_BIN="$DEV_BROKER_DIR/bin/dev-airlock-stands"
	echo "airlock-check: the stack is up (${#BROKER_API_ADDRS[@]} brokers, work directory $AIRLOCK_DIR)" >&2
}

# stop_stack stops dev-airlock.sh, whose own EXIT trap stops its processes and drops the
# databases it made.
stop_stack() {
	if [ "$KEEP_STACK" = true ]; then return 0; fi
	if [ -n "$STACK_PID" ] && kill -0 "$STACK_PID" 2>/dev/null; then
		echo "airlock-check: stopping the stack..." >&2
		kill "$STACK_PID" 2>/dev/null || true
		wait "$STACK_PID" 2>/dev/null || true
	fi
	STACK_PID=""
}

# stack_facts reads the namespace prefix the brokers serve (their own /v1/settings route), which
# the name of the stored value k9 compares against carries.
stack_facts() {
	STACK_SECRETS_PREFIX="$(curl -sS "$AGENT_SECRETS_URL/v1/settings" | jq -r .secrets_prefix)"
}

# --- Talking to the brokers --------------------------------------------------------------------

# release asks for a grant of one name and releases it as the CLI does, leaving the raw answer
# in RELEASE_JSON and the grant's placeholder, proxy password and id in three variables. The
# placeholder and the password never reach the output.
release() { # <name> <ttl>
	local url proof
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
	url="$AGENT_SECRETS_URL/v1/grants/$GRANT_ID/values"
	proof="$(agent-secrets sign --method POST --url "$url")"
	RELEASE_JSON="$(curl -sS -X POST -H "Proof: $proof" "$url")"
	PLACEHOLDER="$(printf '%s' "$RELEASE_JSON" | jq -r --arg n "$1" '.airlock.names[$n].value // ""')"
	PROXY_PASSWORD="$(printf '%s' "$RELEASE_JSON" | jq -r '.airlock.proxy_password // ""')"
}

# proxy_for is the proxy URL of this grant at one address, credential and all. It is never
# printed, and never a command's argument, which every process of the pod reads in
# /proc/<pid>/cmdline: dev-airlock-stands load reads it from --proxy-file and curl from a config
# (-K), each given as a <(…) path.
proxy_for() { printf 'http://agent-secrets:%s@%s' "$PROXY_PASSWORD" "$1"; }

records() { curl -sS "$AIRLOCK_RECORDS_URL/records/$1"; }
records_count() { records "$1" | jq 'length'; }

# stored_value is what the fake store holds for one name, read straight out of the file so a
# comparison is against the store and not against a string this script carries.
stored_value() { jq -r --arg n "$1" '.secrets[] | select(.name == $n) | .value' "$AIRLOCK_FAKE_SECRETS_FILE"; }

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
	judge "$([ "$failed" = 0 ] && [ "$calls" -gt 0 ] && echo 0 || echo 1)" no-call-fails \
		"no call fails in the ten minutes" "$failed of $calls calls failed"
	judge "$(awk -v x="$added" 'BEGIN { exit !(x <= 100) }' && awk -v b="$base_p50" 'BEGIN { exit !(b > 0) }' && echo 0 || echo 1)" median-added-latency \
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
	judge "$([ "$over" = 0 ] && echo 0 || echo 1)" broker-cpu-and-memory \
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
	judge "$([ "$upload_ok" = 200 ] && [ "$gap_ok" = 200 ] && [ "$cloned" = 1 ] && echo 0 || echo 1)" upload-gap-and-clone \
		"the upload, the answer with the gap and the clone finish" \
		"the upload answered $upload_ok, the second event after the 90-second gap answered $gap_ok, and the clone left a checkout: $cloned"

	local upgrade_recs upgrade_header upgrade_status
	upgrade_recs="$(records s | jq --argjson n "$s_mark" '.[$n:]')"
	upgrade_header="$(printf '%s' "$upgrade_recs" | jq -r '[.[] | select(.path == "/socket") | .header.Authorization // [] | .[]] | .[0] // ""')"
	upgrade_status="$(jq -r '.status // 0' "$WORK_DIR/k9/upgrade.json")"
	observe "the WebSocket upgrade: $(cat "$WORK_DIR/k9/upgrade.json" | tr -d '\n' | head -c 300); S recorded its Upgrade header as $(printf '%s' "$upgrade_recs" | jq -r '[.[] | select(.path == "/socket") | .header.Upgrade // [] | .[]] | .[0] // ""') and its Authorization as $(shape "$upgrade_header") against the stored $(shape "$demo_value") (compared by SHA-256 and length)"
	judge "$([ "$upgrade_status" = 101 ] && contains "$demo_value" "$upgrade_header" && contains "the service read" "$(jq -r '.frames // ""' "$WORK_DIR/k9/upgrade.json")" && echo 0 || echo 1)" websocket-upgrade \
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
	judge "$([ "$ok" -gt 0 ] && echo 0 || echo 1)" call-after-a-stopped-broker \
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
	judge "$([ -z "$control_second" ] && echo 0 || echo 1)" negative-control-idle-limit \
		"negative control: with the idle limit at 60 seconds the answer with the 90-second gap is cut" \
		"its second event $(jq -r 'if .second then "answered " + (.second|tostring) else "did not arrive: " + (.second_error // .error // "no answer") end' "$WORK_DIR/k9/control-gap.json")"

	stop_stack
}

###############################################################################################

# From here on a step that answers non-zero is a property that did not pass, not the end of the
# run: the check must report every one of its properties, and a refused request, a command that
# exited 1 and a grep that found nothing are all ordinary answers a step reads. Everything the
# run needs before this point - its arguments, its work directory and its tools - is still
# under set -e, and await ends the run itself when a stack does not come up.
#
# check_k9 does not turn it back on: a `set -e` there would end the run at the first property
# that did not pass, which would leave the run with no result block at all.
set +e

check_k9
report
