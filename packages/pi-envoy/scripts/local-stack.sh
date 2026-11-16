# shellcheck shell=bash disable=SC2034 # natsd_argv and listener_env are the caller's to read
# local-stack.sh — the loopback Envoy stack the smokes beside it each bring up: natsd
# (packages/envoy/internal/testnats/cmd/natsd) and the listener (packages/envoy/cmd/listener) built
# from a checkout with `go build`, each serving on a loopback port the kernel chooses and names in
# its own log. smoke-role-takeover.sh and smoke-registration-stall.sh (--local-stack) source it, so
# what those two know about the stack is written once; packages/envoy-client/test/envoy-stack.ts is
# the same stack for the TypeScript suite.
#
# Sourced, never run: it defines functions, sets no trap, starts no process and writes no file.
# Each caller keeps its own process bookkeeping — how it isolates the environment (`env -i`, bare
# or under `exec` in a subshell), where each log goes, which pids its trap stops and how long it
# waits for what — so this hands out an argv and a set of assignments rather than starting
# anything, and each script's own messages, exit codes and timeouts stay its own.
#
# No port is picked free and handed over: another process on the machine can bind such a port in
# the moment between the check that found it free and the service's own bind. Each service binds
# port 0 itself, so it holds its port from the kernel's choice on, and reports it (reported_port).

# The lines each service logs once it listens, as extended regular expressions that end where the
# port's digits start (reported_port): natsd's client port and monitoring port, and the address in
# the listener's JSON `envoy-listener listening` line.
readonly natsd_client_line='Listening for client connections on 127\.0\.0\.1:'
readonly natsd_monitor_line='Starting http monitor on 127\.0\.0\.1:'
readonly listener_line='"msg":"envoy-listener listening".*"addr":"127\.0\.0\.1:'

# reported_port LOG PATTERN VAR: scripts/e2e/lib/rig.sh's report_port, which the smokes here do not
# source, assigning VAR instead of printing so poll_until can call it: the port a service that bound
# port 0 itself reported, the digits right after the first match of PATTERN in LOG. It fails while
# LOG holds no such line, so it is also the poll that waits for one.
reported_port() {
  local match
  match=$(grep -m1 -oE -- "$2[0-9]+" "$1" 2>/dev/null) || return 1
  printf -v "$3" '%s' "${match##*[!0-9]}"
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

# set_natsd_argv BIN STORE_DIR [--monitor]: sets the array `natsd_argv` to natsd serving 127.0.0.1
# on a port the kernel chooses (`-p -1`, natsd_client_line), with JetStream stored under STORE_DIR,
# and with --monitor, monitoring on another such port (`-m -1`, natsd_monitor_line). Any other
# third argument is refused with status 2. The caller runs it under its own environment isolation
# and redirection.
set_natsd_argv() {
  natsd_argv=("$1" -p -1)
  case "${3:-}" in
    "") ;;
    --monitor) natsd_argv+=(-m -1) ;;
    *)
      printf 'set_natsd_argv: %s is not --monitor\n' "$3" >&2
      return 2
      ;;
  esac
  natsd_argv+=(-js -store_dir "$2")
}

# set_listener_env NATS_URL MACHINE_ID: sets the array `listener_env` to the assignments the
# listener is run with, to serve 127.0.0.1 on a port the kernel chooses (PORT=0, listener_line)
# against NATS_URL as MACHINE_ID. HOME is the caller's to add: each run gives the listener a scratch
# home of its own.
set_listener_env() {
  listener_env=(PORT=0 ENVOY_LISTEN_HOST=127.0.0.1 ENVOY_MACHINE_ID="$2" NATS_URLS="$1")
}

# natsd_serving LOG: natsd's log says it is serving.
natsd_serving() {
  grep -q 'Server is ready' "$1"
}

# listener_healthy URL: the listener at URL answers /healthz healthy.
listener_healthy() {
  curl -fsS --max-time 2 "${1}/healthz" | jq -e '.status == "healthy"' >/dev/null 2>&1
}
