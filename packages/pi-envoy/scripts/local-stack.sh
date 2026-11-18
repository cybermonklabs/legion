# shellcheck shell=bash disable=SC2154,SC2034 # the variables named below are the caller's to set or read
# local-stack.sh — the loopback Envoy stack the smokes beside it each bring up: free ports below
# the kernel's ephemeral range, and natsd (packages/envoy/internal/testnats/cmd/natsd) and the
# listener (packages/envoy/cmd/listener) built from a checkout with `go build`.
# smoke-role-takeover.sh and smoke-registration-stall.sh (--local-stack) source it, so what those
# two know about the stack is written once; packages/envoy-client/test/envoy-stack.ts is the same
# stack for the TypeScript suite.
#
# Sourced, never run: it defines functions, sets no trap and starts no process. Each caller keeps
# its own process bookkeeping — how it isolates the environment (`env -i`, bare or under `exec` in
# a subshell), where each log goes and which pids its trap stops — so this hands out an argv and
# a set of assignments rather than starting anything, and each script's own messages and exit
# codes stay its own.
#
# A smoke a workflow runs (smoke-role-takeover.sh, ci.yaml's envoy-session-stall job) sets its
# guard once, at its head, with proof_guard and the reading beside it, and waits through wait_for
# alone. poll_until is the bounded wait of smoke-registration-stall.sh, which a person runs and
# which sets no guard; no smoke that sets one calls it.
#
# The caller defines
#   refuse MESSAGE    prints MESSAGE prefixed with the script's name and exits 2; port_range
#                     reports a kernel with no usable range below its ephemeral one through it
# and sets
#   local_stack_fail  the function pick_port reports an exhausted range through: `refuse` in both
#                     callers, each of which prints with the script's name and exits 2
#   work              the run's scratch directory, where wait_for keeps its last poll's output
# and reads, after port_range, ephemeral_low and port_floor.

# proof_margin_seconds is how long before the guard's deadline every wait gives up, so the wait's
# own failure and the teardown after it print before anything stops the run.
proof_margin_seconds=30
proof_guard_seconds=''
# wait_gave_up is the line the last wait_for that gave up printed, empty after one that held, so
# a caller that hides a wait's output (expect_true) can still say why it failed.
wait_gave_up=''

# proof_now_ms: the wall clock in epoch milliseconds, the unit LEGION_PROOF_DEADLINE_MS carries.
proof_now_ms() {
  local now=${EPOCHREALTIME/[.,]/}
  printf '%s\n' "$((10#$now / 1000))"
}

# proof_guard SECONDS gives the smoke SECONDS from here, and exports LEGION_PROOF_DEADLINE_MS, the
# moment in epoch milliseconds every wait of the run gives up by: the deadline less
# proof_margin_seconds, which the run's Bun drivers read too (@legion/test-fixtures/wait). It arms
# nothing: the CI job's timeout-minutes stops a run that hangs outside a wait.
proof_guard() {
  [[ -n "${EPOCHREALTIME:-}" ]] || "$local_stack_fail" "bash 5 or later is required (EPOCHREALTIME)"
  proof_guard_seconds=$1
  export LEGION_PROOF_DEADLINE_MS=$(($(proof_now_ms) + ($1 - proof_margin_seconds) * 1000))
}

# wait_for WHAT COMMAND...: runs COMMAND in this shell every 0.2 s until it succeeds, and returns 0;
# at LEGION_PROOF_DEADLINE_MS it gives up and returns 1, with a line that names WHAT and the last
# output of COMMAND printed on stderr and kept in wait_gave_up, so every caller decides how its
# own run ends. A wait that starts past that moment runs COMMAND once. COMMAND runs in this shell,
# so it can drive a client (send) and set variables, and its output goes to ${work}/wait-for.out.
# It takes no bound and no pace: a step that is only slow never fails the run. A poll that never
# returns would hold the wait past its deadline, so every remote call a COMMAND makes carries its
# own timeout.
wait_for() {
  local what=$1 polls=0 output="${work}/wait-for.out"
  shift
  wait_gave_up=''
  [[ -n "${LEGION_PROOF_DEADLINE_MS:-}" ]] ||
    "$local_stack_fail" "wait_for: the run set no guard (proof_guard), so the wait for ${what} has no deadline"
  while :; do
    polls=$((polls + 1))
    if "$@" >"$output" 2>&1; then return 0; fi
    (($(proof_now_ms) < LEGION_PROOF_DEADLINE_MS)) || break
    sleep 0.2
  done
  wait_gave_up="gave up waiting for ${what} after ${polls} polls, ${proof_margin_seconds}s before the run's guard of ${proof_guard_seconds}s; its last output: $(tail -n 3 "$output" | tr '\n' ' ')"
  printf '%s\n' "$wait_gave_up" >&2
  return 1
}

# port_range: the kernel's ephemeral range (ephemeral_low), which every outbound connection
# autobinds from, and the floor a service binds above (port_floor).
port_range() {
  read -r ephemeral_low _ </proc/sys/net/ipv4/ip_local_port_range
  port_floor=10000
  ((ephemeral_low > port_floor + 1000)) ||
    refuse "the kernel's ephemeral range starts at ${ephemeral_low}, leaving no ports below it above ${port_floor}"
}

# port_open PORT: something answers on 127.0.0.1:PORT.
port_open() {
  (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null
}

# pick_port VAR: a loopback port nothing answers on, below the kernel's ephemeral range and
# distinct from this run's earlier picks, which is what keeps one run from handing two services
# one port.
picked_ports=" "
pick_port() {
  local candidate i
  for ((i = 0; i < 200; i++)); do
    candidate=$((port_floor + RANDOM % (ephemeral_low - port_floor)))
    [[ "$picked_ports" != *" ${candidate} "* ]] || continue
    port_open "$candidate" && continue
    picked_ports+="${candidate} "
    printf -v "$1" '%s' "$candidate"
    return 0
  done
  "$local_stack_fail" "no free loopback port below ${ephemeral_low}"
}

# poll_until SECONDS INTERVAL COMMAND...: polls COMMAND every INTERVAL seconds, quietly; its last
# run's own status once SECONDS have passed.
poll_until() {
  local deadline=$((SECONDS + $1)) interval=$2
  shift 2
  while ((SECONDS < deadline)); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep "$interval"
  done
  "$@" >/dev/null 2>&1
}

# build_natsd SOURCE OUT: natsd built from SOURCE, a packages/envoy directory — the checkout's, or
# an older revision's extracted beside it (smoke-role-takeover.sh's --base-rev). Non-zero when the
# build fails, having let `go build` say why, so the caller refuses in its own words.
build_natsd() {
  (cd -- "$1" && go build -o "$2" ./internal/testnats/cmd/natsd)
}

# build_listener SOURCE OUT: the listener built from SOURCE, the same way.
build_listener() {
  (cd -- "$1" && go build -o "$2" ./cmd/listener)
}

# set_natsd_argv BIN PORT MONITOR_PORT STORE_DIR: sets the array `natsd_argv` to natsd serving
# 127.0.0.1:PORT with JetStream stored under STORE_DIR, monitoring on MONITOR_PORT unless it is
# empty. The caller runs it under its own environment isolation and redirection.
set_natsd_argv() {
  natsd_argv=("$1" -p "$2")
  [[ -z "$3" ]] || natsd_argv+=(-m "$3")
  natsd_argv+=(-js -store_dir "$4")
}

# set_listener_env PORT NATS_URL MACHINE_ID: sets the array `listener_env` to the assignments the
# listener is run with, to serve 127.0.0.1:PORT against NATS_URL as MACHINE_ID. HOME is the
# caller's to add: each run gives the listener a scratch home of its own.
set_listener_env() {
  listener_env=(PORT="$1" ENVOY_LISTEN_HOST=127.0.0.1 ENVOY_MACHINE_ID="$3" NATS_URLS="$2")
}

# natsd_serving LOG: natsd's log says it is serving.
natsd_serving() {
  grep -q 'Server is ready' "$1"
}

# listener_healthy URL: the listener at URL answers /healthz healthy.
listener_healthy() {
  curl -fsS --max-time 2 "${1}/healthz" | jq -e '.status == "healthy"' >/dev/null 2>&1
}
