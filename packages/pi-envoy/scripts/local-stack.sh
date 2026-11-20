# shellcheck shell=bash disable=SC2154,SC2034 # the variables named below are the caller's to set or read
# local-stack.sh — the loopback Envoy stack the smokes beside it each bring up: natsd
# (packages/envoy/internal/testnats/cmd/natsd) and the listener (packages/envoy/cmd/listener) built
# from a checkout with `go build`, each on a port no other program can take between its choice and
# its server's bind. natsd takes no socket, so it binds ports the kernel chooses and logs them; the
# listener serves on a socket held from the choice of its port until it serves
# (scripts/e2e/lib/held-port.py, ENVOY_LISTEN_FD). A port found free and bound later can be taken in
# between, and its server then exits with `bind: address already in use`.
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
# The caller sets
#   repo_root   the checkout whose scripts/e2e/lib/held-port.py and port-file.sh this uses

# shellcheck source=scripts/e2e/lib/port-file.sh
source "${repo_root}/scripts/e2e/lib/port-file.sh"

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

# set_natsd_argv BIN STORE_DIR [--monitor]: sets the array `natsd_argv` to natsd serving a loopback
# port the kernel chooses, with JetStream stored under STORE_DIR, and with --monitor a monitoring
# port the kernel chooses too: -1 is nats-server's RANDOM_PORT, where a -p of 0 would be the default
# 4222. natsd_ports reads the ports back from natsd's log. The caller runs it under its own
# environment isolation and redirection.
set_natsd_argv() {
  natsd_argv=("$1" -p -1)
  [[ "${3:-}" != --monitor ]] || natsd_argv+=(-m -1)
  natsd_argv+=(-js -store_dir "$2")
}

# natsd_ports LOG: sets `nats_port`, and `monitor_port` when natsd serves monitoring, to the ports
# natsd bound, from the lines it logs once it has bound them ("Starting http monitor on
# 127.0.0.1:<port>", "Listening for client connections on 127.0.0.1:<port>"). Non-zero while the
# log names no client port.
natsd_ports() {
  local line
  line="$(grep -m1 -oE 'Listening for client connections on [^ ]+:[0-9]+' "$1" 2>/dev/null)" || return 1
  nats_port="${line##*:}"
  monitor_port=""
  if line="$(grep -m1 -oE 'Starting http monitor on [^ ]+:[0-9]+' "$1" 2>/dev/null)"; then
    monitor_port="${line##*:}"
  fi
}

# set_listener_env NATS_URL MACHINE_ID: sets the array `listener_env` to the assignments the
# listener is run with, to serve against NATS_URL as MACHINE_ID on the socket listener_held_argv
# hands it as descriptor 3: ENVOY_LISTEN_FD names it, since the caller's `env -i` drops the one
# held-port.py sets, and ENVOY_LISTEN_HOST is the held socket's own address, which the listener
# checks. HOME is the caller's to add: each run gives the listener a scratch home of its own.
set_listener_env() {
  listener_env=(ENVOY_LISTEN_FD=3 ENVOY_LISTEN_HOST=127.0.0.1 ENVOY_MACHINE_ID="$2" NATS_URLS="$1")
}

# set_listener_held_argv PORT_FILE: sets the array `listener_held_argv` to the command the caller
# puts before its own `env -i … LISTENER`: held-port.py exec opens a listening socket on a loopback
# port the kernel chooses, writes that port to PORT_FILE and executes the rest in its place with the
# socket as descriptor 3, which the listener serves on. A listener built before ENVOY_LISTEN_FD
# (legion-envoy-v18.4.0 and earlier) binds a port of its own and exits with
# `address already in use`. read_port_file (port-file.sh) reads PORT_FILE back.
set_listener_held_argv() {
  listener_held_argv=(python3 "${repo_root}/scripts/e2e/lib/held-port.py" exec --port-file "$1"
    --fd-env ENVOY_LISTEN_FD --)
}

# natsd_serving LOG: natsd's log says it is serving.
natsd_serving() {
  grep -q 'Server is ready' "$1"
}

# listener_healthy URL: the listener at URL answers /healthz healthy.
listener_healthy() {
  curl -fsS --max-time 2 "${1}/healthz" | jq -e '.status == "healthy"' >/dev/null 2>&1
}
