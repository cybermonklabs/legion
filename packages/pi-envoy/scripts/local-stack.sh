# shellcheck shell=bash disable=SC2154,SC2034 # the variables named below are the caller's to set or read
# local-stack.sh — the loopback Envoy stack the smokes beside it each bring up: natsd
# (packages/envoy/internal/testnats/cmd/natsd) and the listener (packages/envoy/cmd/listener),
# built from a checkout with `go build`, each on loopback ports the kernel chooses when the service
# binds them, read back from what the service logs.
# smoke-role-takeover.sh and smoke-registration-stall.sh (--local-stack) source it, so what those
# two know about the stack is written once; packages/envoy-client/test/envoy-stack.ts is the same
# stack for the TypeScript suite.
#
# Sourced, never run: it defines functions, sets no trap, starts no process and writes no file.
# Each caller keeps its own process bookkeeping — how it isolates the environment (`env -i`, bare
# or under `exec` in a subshell), where each log goes, which pids its trap stops and how long it
# waits for what — so this hands out an argv and a set of assignments rather than starting
# anything, and each script's own messages, exit codes and timeouts stay its own.
#
# No port is picked here and handed to a service: a port found free and bound later is any other
# process's to take in between, and the service then exits with "address already in use". Each
# service binds port 0 and names the port it got, and a caller reads it from the service's log
# once the service is up (natsd_port, listener_addr).

# build_natsd and build_listener print go's own lines, a module proxy's reply among them, inside a
# .github/scripts/ci-log-guard.sh block, where they cannot become workflow commands in CI's log
# (ci.yaml's envoy-session-stall job runs smoke-role-takeover.sh). The library defines functions
# alone, as this file does.
# shellcheck source=.github/scripts/ci-log-guard.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)/.github/scripts/ci-log-guard.sh"

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
  (cd -- "$1" && ci_log_guard go build -o "$2" ./internal/testnats/cmd/natsd)
}

# build_listener SOURCE OUT: the listener built from SOURCE, the same way.
build_listener() {
  (cd -- "$1" && ci_log_guard go build -o "$2" ./cmd/listener)
}

# set_natsd_argv BIN STORE_DIR [monitor]: sets the array `natsd_argv` to natsd serving a client
# port of 127.0.0.1 the kernel chooses, with JetStream stored under STORE_DIR, and with `monitor`
# a monitoring port the kernel chooses too. -1 is nats-server's RANDOM_PORT: a -p of 0 would be
# its default 4222, and an -m of 0 no monitoring at all. The caller runs it under its own
# environment isolation and redirection, and reads the ports with natsd_port once it serves.
set_natsd_argv() {
  natsd_argv=("$1" -p -1)
  [[ "${3:-}" != monitor ]] || natsd_argv+=(-m -1)
  natsd_argv+=(-js -store_dir "$2")
}

# set_listener_env NATS_URL MACHINE_ID: sets the array `listener_env` to the assignments the
# listener is run with, to serve a port of 127.0.0.1 the kernel chooses (PORT=0) against NATS_URL
# as MACHINE_ID. HOME is the caller's to add: each run gives the listener a scratch home of its
# own. The caller reads the address with listener_addr once the listener logs it.
set_listener_env() {
  listener_env=(PORT=0 ENVOY_LISTEN_HOST=127.0.0.1 ENVOY_MACHINE_ID="$2" NATS_URLS="$1")
}

# natsd_serving LOG: natsd's log says it is serving, which it logs after the addresses it bound.
natsd_serving() {
  grep -q 'Server is ready' "$1"
}

# natsd_port LOG client|monitor VAR: assigns VAR the client or monitoring port natsd reports in LOG
# that it bound ("Listening for client connections on 127.0.0.1:<port>", "Starting http monitor
# on 127.0.0.1:<port>"). Non-zero while LOG holds no such line.
natsd_port() {
  local pattern match
  case "$2" in
    client) pattern='Listening for client connections on [^ ]*:[0-9]+' ;;
    monitor) pattern='Starting http monitor on [^ ]*:[0-9]+' ;;
    *) return 2 ;;
  esac
  match=$(grep -m1 -oE -- "$pattern" "$1" 2>/dev/null) || return 1
  printf -v "$3" '%s' "${match##*:}"
}

# listener_addr LOG: prints the address the listener reports in LOG that it serves on, the `addr`
# of its JSON "envoy-listener listening" line, which names the bound address, not the configured
# one. Non-zero, printing nothing, while LOG holds no such line or names port 0.
listener_addr() {
  local addr
  addr=$(jq -Rnr '[inputs | fromjson? | select(.msg == "envoy-listener listening") | .addr][0] // empty' "$1" 2>/dev/null)
  [[ "$addr" =~ ^127\.0\.0\.1:[1-9][0-9]*$ ]] || return 1
  printf '%s\n' "$addr"
}

# listener_healthy URL: the listener at URL answers /healthz healthy.
listener_healthy() {
  curl -fsS --max-time 2 "${1}/healthz" | jq -e '.status == "healthy"' >/dev/null 2>&1
}
