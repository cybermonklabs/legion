# shellcheck shell=bash
# packages/envoy/scripts/lib/check-stack.sh
#
# The stack helpers judge-check.sh and proxy-chain-check.sh share: waiting for a process's logged
# readiness, starting dev-airlock.sh and reading its banner, starting one more broker beside the
# stack's, and reading a broker's log records and the address it logged. Each check keeps its own
# scenario setup, its own state past what these set, its own cleanup and its own front_backends;
# this file holds only what the two did the same way.
#
# Sourced, never run, after lib/exec-env.sh (broker_env_of, with_broker_env). The caller sets
# CHECK_NAME, the name its messages start with (the script's own file name when unset), SCRIPT_DIR,
# WORK_DIR, STACK_LOG and EXTRA_BROKER_PIDS=(). Six functions:
#
#   await <what> <seconds> <log> <pattern> <pid>
#       Waits for a line matching pattern in log, and ends the run the moment pid dies or the
#       seconds pass: a stack that never came up proves nothing, and the check's own steps would
#       each fail with a reason that is not the check's.
#   stack_start [dev-airlock.sh option...]
#       Runs dev-airlock.sh with the options into STACK_LOG, sets STACK_PID, waits for its banner,
#       evaluates every export the banner names (dev-broker.sh's export block is in the same log
#       and is read first, so where the two name one variable the airlock's wins, as the banner
#       says), and sets BROKER_API_ADDRS, BROKER_PROXY_ADDRS, BROKER_PIDS and BROKER_LOGS, one
#       entry per stack broker, and BROKER_BIN.
#   broker_env [NAME=value...]
#       broker_env_of on the first stack broker: BROKER_ENV is its environment with the entries
#       named on top. It is read out of /proc/<pid>/environ rather than spelled here, so that a
#       broker a check starts can never lack a setting dev-airlock.sh gave the stack's.
#   start_extra_broker <name> [NAME=value...]
#       Starts one more broker beside the stack's, on its database, profiles file, certificate
#       authority and stand-ins, with the entries named on top and three ports the system picks,
#       through with_broker_env, so its database URL and password are in no process's arguments.
#       It waits for its proxy port, not for an intermediate: a broker that holds none still binds
#       and answers, which is what a readiness read is about. It sets EXTRA_LOG, EXTRA_PID,
#       EXTRA_API_ADDR and EXTRA_PROXY_ADDR, and adds the pid to EXTRA_BROKER_PIDS, which the
#       caller's cleanup stops. EXTRA_BROKER_BIN, when set, is the binary it runs in place of
#       BROKER_BIN: judge-check.sh's k10g control is a broker built from another source tree.
#   log_records <message> <log>
#       Every complete JSON object in log whose message is the named one, as one JSON array. A
#       line being written or otherwise malformed is omitted.
#   logged_addr <message> <log>
#       The addr field of the first matching log record, or nothing.

await() { # <what> <seconds> <log> <pattern> <pid>
	local i who="${CHECK_NAME:-${0##*/}}"
	for ((i = 0; i < $2 * 2; i++)); do
		if grep -qE "$4" "$3" 2>/dev/null; then return 0; fi
		if ! kill -0 "$5" 2>/dev/null; then
			echo "$who: $1 exited before it was ready; see $3" >&2
			tail -n 20 "$3" >&2 || true
			exit 1
		fi
		sleep 0.5
	done
	echo "$who: $1 was not ready within $2 seconds; see $3" >&2
	exit 1
}

# shellcheck disable=SC2034 # each array it sets is the caller's to read
stack_start() { # [dev-airlock.sh option...]
	echo "${CHECK_NAME:-${0##*/}}: starting the stack (dev-airlock.sh $*)..." >&2
	: >"$STACK_LOG"
	bash "$SCRIPT_DIR/dev-airlock.sh" "$@" >"$STACK_LOG" 2>&1 &
	STACK_PID=$!
	await "the stack" 900 "$STACK_LOG" '^dev-airlock: ready\.$' "$STACK_PID"
	eval "$(sed -n 's/^  \(export [A-Z][A-Z_0-9]*=.*\)$/\1/p' "$STACK_LOG")"
	mapfile -t BROKER_API_ADDRS < <(sed -n 's/^  broker [0-9]*: *api \([^,]*\),.*/\1/p' "$STACK_LOG")
	mapfile -t BROKER_PROXY_ADDRS < <(sed -n 's/^  broker [0-9]*:.*proxy \([^,]*\),.*/\1/p' "$STACK_LOG")
	mapfile -t BROKER_PIDS < <(sed -n 's/^  broker [0-9]*:.*pid \([0-9]*\),.*/\1/p' "$STACK_LOG")
	mapfile -t BROKER_LOGS < <(sed -n 's/^  broker [0-9]*:.*, pid [0-9]*, log \(.*\)$/\1/p' "$STACK_LOG")
	BROKER_BIN="$DEV_BROKER_DIR/bin/broker"
}

broker_env() { # [NAME=value...]
	broker_env_of "${BROKER_PIDS[0]}" "$@"
}

# shellcheck disable=SC2034 # EXTRA_API_ADDR and EXTRA_PROXY_ADDR are the caller's to read
start_extra_broker() { # <name> [NAME=value...]
	local name="$1"
	shift
	EXTRA_LOG="$WORK_DIR/broker-$name.log"
	: >"$EXTRA_LOG"
	broker_env BROKER_LISTEN_ADDR="127.0.0.1:0" BROKER_AIRLOCK_LISTEN_ADDR="127.0.0.1:0" BROKER_METRICS_LISTEN_ADDR="127.0.0.1:0" "$@"
	(with_broker_env "${EXTRA_BROKER_BIN:-$BROKER_BIN}") >"$EXTRA_LOG" 2>&1 &
	EXTRA_PID=$!
	EXTRA_BROKER_PIDS+=("$EXTRA_PID")
	await "the $name broker" 120 "$EXTRA_LOG" '"message":"airlock proxy listening"' "$EXTRA_PID"
	EXTRA_API_ADDR="$(logged_addr 'broker listening' "$EXTRA_LOG")"
	EXTRA_PROXY_ADDR="$(logged_addr 'airlock proxy listening' "$EXTRA_LOG")"
}

log_records() { # <message> <log>
	local records
	records="$(jq -Rnc --arg message "$1" '[inputs | fromjson? | select(.message == $message)]' "$2" 2>/dev/null)" || true
	printf '%s\n' "${records:-[]}"
}

logged_addr() { # <message> <log>
	log_records "$1" "$2" | jq -r '.[0].addr // empty'
}
