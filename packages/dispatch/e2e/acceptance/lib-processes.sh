# shellcheck shell=bash
# The processes a driver starts itself over a database of the run's own: the two fakes
# e2e/run-server.sh needs and each server it runs, every one on a port the kernel chooses. Sourced,
# never run. It sources lib-baseline.sh, whose baseline_stop_pid stops each process, and through it
# lib.sh, whose `fail` and `pass` every function here prints with.
#
# It reads the caller's `driver`, `work` and `database_url`. It keeps the pid of each fake in `pids`
# and that of the server boot_server started last in `server_pid`, which stop_processes, called from
# the caller's cleanup trap, stops.

# shellcheck source-path=SCRIPTDIR source=lib-baseline.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib-baseline.sh"

# The directory that holds run-server.sh and the fakes.
e2e_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
readonly e2e_dir
# How long a process gets to print the line that says it listens. A server's first boot migrates a
# whole schema from empty, the longest this wait covers.
readonly listen_limit=180

pids=()
server_pid=""

# require_local_database sets $database_host to the host DATABASE_URL names, and refuses one that is
# not this machine: a server's dev sign-in refuses it, and the driver writes rows of its store.
require_local_database() {
  database_host="${database_url#*://}"
  database_host="${database_host#*@}"
  database_host="${database_host%%[:/?]*}"
  case "$database_host" in
    127.0.0.1 | ::1 | localhost) ;;
    *)
      printf 'DATABASE_URL names the host %s, which is not this machine: a server'"'"'s dev sign-in refuses it, and %s writes rows of its store\n' \
        "$database_host" "$driver" >&2
      exit 2
      ;;
  esac
}

# fail_with_log SECTION WHAT OBSERVED LOG: fail, with the last lines of LOG after OBSERVED.
fail_with_log() {
  fail "$1" "$2" "$3; the last lines of its log: $(tail -n 20 "$4" 2>/dev/null)"
}

# await_line SECTION WHAT PID LOG PATTERN: waits for PATTERN in LOG while PID lives, for up to
# $listen_limit seconds, and sets $line to the first line holding it.
await_line() {
  local waited=0
  until line="$(grep -m 1 -E "$5" "$4" 2>/dev/null)"; do
    kill -0 "$3" 2>/dev/null || fail_with_log "$1" "$2" "the process stopped before it printed /$5/" "$4"
    [[ "$waited" -lt $((listen_limit * 2)) ]] ||
      fail_with_log "$1" "$2" "no line /$5/ after $listen_limit s" "$4"
    sleep 0.5
    waited=$((waited + 1))
  done
}

# start_fake NAME FILE: starts the fake e2e/FILE on a port the OS chooses, which its own line
# reports, and sets $fake_port to it.
start_fake() {
  env -u FAKE_ENVOY_PORT -u FAKE_GITHUB_PORT -u FAKE_BROKER_PORT -u FAKE_EMBEDDER_PORT \
    -u DISPATCH_E2E_STACKS -u DISPATCH_E2E_STACK_COUNT -u DATABASE_URL \
    bun "$e2e_dir/$2" >"$work/$1.log" 2>&1 &
  pids+=("$!")
  await_line setup "start the $1" "$!" "$work/$1.log" "listener on 127\.0\.0\.1:[0-9]+"
  fake_port="${line##*:}"
  [[ "$fake_port" =~ ^[0-9]+$ ]] || fail setup "start the $1" "its line reads: $line"
  printf '%s: the %s listens on 127.0.0.1:%s\n' "$driver" "$1" "$fake_port"
}

# start_fakes: starts the fake Envoy and the fake GitHub, which run-server.sh requires, and sets
# $envoy_port and $github_port. No secrets broker is started: boot_server turns the credential
# feature off, which no driver here reads.
start_fakes() {
  start_fake "fake Envoy" fake-envoy.ts
  envoy_port="$fake_port"
  start_fake "fake GitHub" fake-github.ts
  github_port="$fake_port"
}

# boot_server SECTION NAME BINARY: starts BINARY with e2e/run-server.sh over the run's database, on a
# port the kernel chooses and held-port.py holds until the server serves on it, with no secrets
# broker and with the dev sign-in key baseline_start made where it made one, so every server of a
# run signs cookies with one key. It sets $server_pid and $base, the server's address, which its
# own line names.
boot_server() {
  env -u DISPATCH_E2E_PORT -u DISPATCH_E2E_PROCESSES -u DISPATCH_E2E_SIGNIN -u DISPATCH_E2E_LISTEN_FROM \
    DATABASE_URL="$database_url" FAKE_ENVOY_PORT="$envoy_port" FAKE_GITHUB_PORT="$github_port" \
    DISPATCH_E2E_AGENT_SECRETS_URL= DISPATCH_E2E_SERVER_BIN="$3" \
    DISPATCH_E2E_DEV_SIGNIN_SHARED_KEY="$baseline_signin_key" \
    bash "$e2e_dir/run-server.sh" >"$work/$2.log" 2>&1 &
  server_pid="$!"
  await_line "$1" "start $2" "$server_pid" "$work/$2.log" 'msg="dispatch: listening" addr=127\.0\.0\.1:[0-9]+'
  [[ "$line" =~ addr=(127\.0\.0\.1:[0-9]+) ]] || fail_with_log "$1" "start $2" "its line names no address: $line" "$work/$2.log"
  base="http://${BASH_REMATCH[1]}"
  printf '%s: %s, %s, serves %s (pid %s)\n' "$driver" "$2" "$3" "$base" "$server_pid"
}

# stop_processes: stops the server boot_server started last and every fake, for the caller's
# cleanup trap.
stop_processes() {
  local pid
  baseline_stop_pid "$server_pid"
  server_pid=""
  for pid in "${pids[@]}"; do
    baseline_stop_pid "$pid"
  done
}
