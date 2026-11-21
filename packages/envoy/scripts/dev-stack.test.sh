#!/usr/bin/env bash
# packages/envoy/scripts/dev-stack.test.sh
#
# Proves dev-stack.sh's per-run isolation, its port reads and its stop, without a real Postgres,
# real network binds or a real server binary: it puts a fake `go`, `psql`, `curl` and `python3` on
# PATH — each recording what it was asked for to the instance's own call file and answering it
# without touching anything real — and the fake `go build -o <dir>/ ./cmd/<pkg> ...` installs a
# stub at <dir>/<pkg> instead of compiling. The same fake go makes the prebuilt directory a
# --binaries run is given, as the builds farm makes the real one. dev-stack.sh's own isolation,
# port and stop logic lives entirely in the shell driver and never in the Go binaries it starts,
# so a stub proves the driver's behaviour without depending on cmd/dispatch, cmd/cipherd or
# cmd/listener compiling.
#
# Each stub logs the line the real process logs once it is listening — the issuer's
# "issuer listening url=", the broker's JSON "broker listening", natsd's "Listening for client
# connections on" and "Server is ready", the listener's JSON "envoy-listener listening" and
# Dispatch's msg="dispatch: listening" addr= — with its own PID standing in for the port a real
# Listen would report, since two concurrently running stubs always have distinct PIDs, and then
# stays alive until it is signalled, as the server it stands in for does. So a test that reads a
# port out of dev-stack.sh's banner and finds the stub's PID has proved that dev-stack.sh took
# that port from the process's own report and from nowhere else.
#
# It runs two instances concurrently — the scenario a port and a database of each run's own exist
# to make safe — and asserts that: (a) every process of a run is started and the port the banner
# names for it is the one that process reported; (b) the Dispatch port keeper's held port is what
# the broker's machine-login URL and Dispatch's own server URL name, which is why a keeper is
# there at all, and the broker binds its metrics on a port the kernel picks; (c) "dev-stack:
# ready." is printed exactly once, with the export block after it;
# (d) the two instances mint different database names, different ports and different work
# directories, and neither names the other's; (e) the one trap stops every process it started and
# drops exactly the two databases it created, on a normal exit, on INT, on a SIGTERM to its whole
# process group sent again while the stop runs, on that SIGTERM with the run's output piped
# through tee, which the signal stops too, on a normal exit after its output's reader is gone, and
# on a failure part way through, which leaves nothing running: one before a process reports its
# port, and one after, a listener or Dispatch that reports its address and exits while its keeper
# still holds the port, which the run reports inside 15 s rather than waiting on a request that
# port never answers; a SIGTERM to the run while such a request is outstanding at a server that
# stays alive ends it inside 15 s with 143; a process that ignores SIGTERM is killed 10 s into
# the stop, which says so and still drops both databases; a SIGHUP while the stop runs does not end
# it; and the stop signals only a process the run started that is still running, never another
# process that took the pid of one that exited early; (f) a run given no database URL refuses with
# exit 2, naming all three sources;
# (g) the second box is printed with a path of its own for each of the first box's, every one
# of them under this run's own work directory, the directories it writes into already there, and
# its metadata stand-in taking its port file from the environment rather than from the one baked
# path two stand-ins would fight over; (h) a run given --binaries with the eight binaries the
# header names calls no go at all, copies the prebuilt stubs into its own bin directory, and runs
# them, held to everything (a), (b), (c) and (e) hold a built run to; and (i) a --binaries
# directory short of one of them, or holding one that is not an executable file, is refused with
# exit 2, naming each such file, before anything is created or started; and (j) a --callout run
# starts natsd behind the auth callout with a monitor port, two brokers that each run the auth
# service on it, and the listener and Dispatch each with a NATS service token the issuer writes,
# exports what callout-check.sh reads, and outlives its second broker being killed, while a run
# without --callout does none of it; (k) every run gives each broker its database URL with
# pool_max_conns=10, production's pool size, joined to the server URL's own query string or
# starting one where it has none (the --callout run names a server URL with no query string),
# while Dispatch, which refuses that parameter, gets the URL's query string alone; (l) on the
# --callout run SIGUSR1 restarts natsd on the ports it reported, with the same store and callout
# keys, and SIGUSR2 restarts the listener on its keeper's socket, each old process gone, its log
# kept, and the stack up; and the controls that (l)'s same-port checks can fail, each stopping the
# whole stack and naming both ports: (m) a natsd that comes back on another client port, (n) one
# that comes back on its client port and another monitor port, and (o) a listener that comes back
# on another port than its keeper's; (p) a bash older than 5.1 (4.4, 5.0) is refused with exit 2
# and one line naming the bash it needs and the one it is, while 5.1 and 5.3 are taken, by the
# gate the driver runs at its top; (q) a --project-access run points the broker at its own
# Dispatch's access routes with a token of its own account, has Dispatch admit that account, and
# starts the airlock in a container of its own, while a run without it sets none of it; (r) a
# --project-access run whose airlock proxy bound another port than the one it picked stops,
# naming both ports; (s) only --project-access needs the airlock's stands: it builds
# dev-airlock-stands beside the eight, given --binaries it needs that ninth binary there, and it
# needs docker on PATH, refused before anything is created or started without either, while a
# run without it builds no stands and needs no docker; (t) --callout and --project-access
# together, in either order, are refused with exit 2 and one line naming the pair, before anything
# is created or started and with no psql, curl, go or docker call; and (u) every broker a run
# starts, the --callout run's second among them, lists the run's registrar as the registrar of the
# pod's account and of nothing else (BROKER_POD_REGISTRARS), without which the one registration
# the run makes is refused. (q) and (r) run after (h), and (s) and (t) after (i).
#
# The fake docker is scripts/lib/dev-stack-test-docker.sh's, and assert_project_access and the
# --project-access cases are scripts/lib/dev-stack-test-project-access.sh's; this file sources
# both, and is the one entry point that runs every case.
set -euo pipefail
# Monitor mode, for the two cases that signal an instance: a background command of a shell
# without job control inherits SIGINT ignored, and a shell cannot trap a signal that was ignored
# when it started, so an instance started with & from this script could not run its own INT trap
# at all. With -m each instance is a process group of its own, and the group the SIGTERM case
# signals holds that instance and every process of its stack and nothing of this test's own.
# Monitor mode gives an instance the dispositions this test itself started with, though, and this
# test may be started with SIGINT ignored too (a background job of a shell without job control,
# as podrun.sh's `setsid "$@" &` starts it). So both launchers start each instance with SIGINT put
# back to its default (env --default-signal=INT, in prepare_instance), and each INT row first
# reads the instance's own report that it was not started with SIGINT ignored, so the INT reaches
# dev-stack.sh's trap exactly as a terminal's Ctrl-C does whatever started this test.
set -m

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly project_root
readonly driver="${project_root}/packages/envoy/scripts/dev-stack.sh"
temporary_dir="$(mktemp -d "${TMPDIR:-/tmp}/dev-stack-test.XXXXXX")"
readonly temporary_dir
readonly fake_bin_dir="${temporary_dir}/bin"
mkdir -p "$fake_bin_dir"
# The ports a server stub stopped serving while its keeper still holds the socket, which the fake
# curl below answers as nothing ever answers a request there (DEV_STACK_TEST_UNSERVED_FILE).
readonly unserved_file="${temporary_dir}/unserved-ports"

# The Postgres server every instance below is told to use. Nothing connects to it: the fake psql
# answers every call.
readonly admin_url='postgres://tester:s3cret@127.0.0.1:5999/postgres?sslmode=disable'
# The same server named with no query string, so the URL a run builds for a database of its own
# has to start one: the --callout run (j) is given it. server_url is the server alone, which each
# run's own database URL starts with.
readonly admin_url_no_query=${admin_url%%\?*}
readonly server_url=${admin_url%/postgres*}

declare -a instance_pid=()
cleanup() {
  # Best-effort: if an assertion fails mid-test, leave no dev-stack.sh instance, and no stub it
  # started, running.
  local pid
  for pid in "${instance_pid[@]}"; do
    if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
      kill -TERM "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
    fi
  done
  pkill -f "${temporary_dir}/dev-stack\." 2>/dev/null || true
  rm -rf "$temporary_dir"
}
trap cleanup EXIT

# --- Fake psql: records every call to the instance's own DEV_STACK_TEST_PSQL_CALL_FILE (each
# instance gets its own, so two concurrent instances' calls never interleave in one file), and
# answers `select current_database()` with the database the URL in its first argument connects to,
# as libpq does: a dbname= in the query string wins over the URL's path. ---
cat >"${fake_bin_dir}/psql" <<'EOF'
#!/usr/bin/env bash
printf 'psql %s\n' "$*" >>"${DEV_STACK_TEST_PSQL_CALL_FILE:?}"
# The password psql was handed in its environment, a line a call: dev-stack.sh keeps it out of
# psql's arguments (server_psql), which the line above records.
printf '%s\n' "${PGPASSWORD-}" >>"${DEV_STACK_TEST_PSQL_CALL_FILE}.pgpassword"
if [[ "$*" == *"select current_database()"* ]]; then
  url=$1 query=""
  [[ "$url" == *\?* ]] && query=${url#*\?}
  if [[ "&$query" =~ \&dbname=([^&]+) ]]; then
    printf '%s\n' "${BASH_REMATCH[1]}"
  else
    path=${url%%\?*}
    printf '%s\n' "${path##*/}"
  fi
fi
exit 0
EOF
chmod +x "${fake_bin_dir}/psql"

# --- Fake curl: dev-stack.sh curls its own broker's health and pod-session route, its own
# listener's health and its own Dispatch's route index; record the exact command line (which names
# the port dev-stack.sh read from each process's own log) to the instance's own
# DEV_STACK_TEST_CURL_CALL_FILE and answer success at once, so no readiness loop ever opens a
# socket. The one exception is a port listed in DEV_STACK_TEST_UNSERVED_FILE, which a server stub
# writes when it serves nothing on the socket its keeper holds: the kernel queues a connection
# there and no process ever accepts it, so the real curl waits out its --max-time and exits 28,
# and with no --max-time waits until it is killed, and so does this one. It first names that port
# in <DEV_STACK_TEST_UNSERVED_FILE>.asked, which a stub that exits waits for, so a row knows the
# request was outstanding. ---
cat >"${fake_bin_dir}/curl" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"${DEV_STACK_TEST_CURL_CALL_FILE:?}"
max_time="" port=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --max-time)
      max_time="${2:?--max-time takes a value}"
      shift 2
      ;;
    http://127.0.0.1:*)
      port="${1#http://127.0.0.1:}"
      port="${port%%/*}"
      shift
      ;;
    *) shift ;;
  esac
done
if [[ -n "$port" && -s "${DEV_STACK_TEST_UNSERVED_FILE:-}" ]] && grep -qx "$port" "$DEV_STACK_TEST_UNSERVED_FILE"; then
  echo "$port" >>"${DEV_STACK_TEST_UNSERVED_FILE}.asked"
  # With no bound it waits until it is killed. It stays this process rather than exec the sleep, so
  # a list of a stuck run's processes names this request.
  sleep "${max_time:-999999}"
  exit 28
fi
exit 0
EOF
chmod +x "${fake_bin_dir}/curl"

# --- Fake docker, which dev-stack.sh runs only under --project-access: write_fake_docker is
# lib/dev-stack-test-docker.sh's. ---
# shellcheck source=lib/dev-stack-test-docker.sh
. "${project_root}/packages/envoy/scripts/lib/dev-stack-test-docker.sh"
write_fake_docker "$fake_bin_dir"

# --- Fake env, first on PATH: appends its arguments, one line per call, to the run's own
# DEV_STACK_TEST_ARGV_FILE when that is set, and then runs the real env with them. With the fake
# python3 below, which does the same, it is how the row (u2) reads every argument list a start
# handed a process: the URL's password must be in none of them. ---
real_env="$(command -v env)"
cat >"${fake_bin_dir}/env" <<EOF
#!/usr/bin/env bash
if [[ -n "\${DEV_STACK_TEST_ARGV_FILE:-}" ]]; then printf 'env %s\n' "\$*" >>"\$DEV_STACK_TEST_ARGV_FILE"; fi
exec ${real_env} "\$@"
EOF
chmod +x "${fake_bin_dir}/env"

# --- Fake python3, which dev-stack.sh runs as two things. As scripts/e2e/lib/held-port.py:
# `keep` writes its own PID to the port file as the port it holds (two concurrent keepers have
# distinct PIDs, as two kernel-chosen ports are distinct) and stays alive, writing that file last
# as the real keeper does, which writes it only once it is serving; it writes the same port at
# its --socket path, which stands in for the socket a server's start is handed. `exec` drops its
# own options and executes the command after `--`, as the real one does once it has put the held
# socket on descriptor 3; with --from, it reads the port that socket path holds and puts it for
# each {port} in the command, as the real one puts the held socket's port. And as `python3 -c`,
# which asks the kernel for a free port for the airlock's proxy: this one prints its own PID for
# the same reason, and with DEV_STACK_TEST_PICKED_PORT set prints that instead. The lost port race
# of (r) does not use it: there the broker's stub reports another address
# (DEV_STACK_TEST_AIRLOCK_BOUND). No socket is opened either way. ---
{
  printf '#!/usr/bin/env bash\nreal_python3=%q\n' "$(command -v python3)"
  cat <<'EOF'
if [[ -n "${DEV_STACK_TEST_ARGV_FILE:-}" ]]; then printf 'python3 %s\n' "$*" >>"$DEV_STACK_TEST_ARGV_FILE"; fi
script="${1:-}"
case "$script" in
  */held-port.py) shift ;;
  -c)
    printf '%s\n' "${DEV_STACK_TEST_PICKED_PORT:-$$}"
    exit 0
    ;;
  *)
    echo "fake python3: unexpected script $script" >&2
    exit 2
    ;;
esac
mode="${1:-}"
shift || true
case "$mode" in
  keep)
    port_file="" socket=""
    while [[ $# -gt 0 ]]; do
      case "$1" in
        --port-file)
          port_file="$2"
          shift 2
          ;;
        --socket)
          socket="$2"
          shift 2
          ;;
        --host)
          shift 2
          ;;
        *)
          echo "fake held-port keep: unexpected argument $1" >&2
          exit 2
          ;;
      esac
    done
    [[ -n "$port_file" && -n "$socket" ]] || exit 2
    sleeper=""
    trap 'kill "$sleeper" 2>/dev/null; exit 0' TERM INT
    printf '%s\n' "$$" >"$socket"
    printf '%s\n' "$$" >"$port_file"
    sleep 999999 &
    sleeper=$!
    wait "$sleeper"
    ;;
  exec)
    held="" passed=()
    while [[ $# -gt 0 ]]; do
      case "$1" in
        --)
          shift
          break
          ;;
        --from)
          held="$(cat "$2")"
          shift 2
          ;;
        --host | --port-file | --fd-env | --env)
          shift 2
          ;;
        --pass)
          passed+=("$2")
          shift 2
          ;;
        *)
          echo "fake held-port exec: unexpected argument $1" >&2
          exit 2
          ;;
      esac
    done
    [[ $# -gt 0 ]] || exit 2
    if [[ -n "$held" ]]; then
      command=()
      for argument in "$@"; do
        command+=("${argument//\{port\}/$held}")
      done
      set -- "${command[@]}"
    fi
    # With --pass, the command's environment is exactly the variables named, as the real
    # held-port.py gives it (DISPATCH_LISTEN_FD among them here, as the real one sets it): the
    # real python3 execs it with those values, read from its own environment and in no process's
    # arguments. A bash exec of its own would add SHLVL.
    if [[ ${#passed[@]} -gt 0 ]]; then
      exec "$real_python3" -c 'import os, sys
env = {name: os.environ[name] for name in sys.argv[1].split()}
os.execve(sys.argv[2], sys.argv[2:], env)' "${passed[*]}" "$@"
    fi
    exec "$@"
    ;;
  *)
    echo "fake held-port: unexpected mode $mode" >&2
    exit 2
    ;;
esac
EOF
} >"${fake_bin_dir}/python3"
chmod +x "${fake_bin_dir}/python3"

# --- Fake go: "go build -o <dir>/ ./cmd/<pkg> ..." installs one stub per package instead of
# compiling. Every server stub reports its own PID as its port and stays alive until it is
# signalled, exiting 0 on SIGTERM as a server stopped by its supervisor does, so a stub the test
# stops drives dev-stack.sh's normal exit. Two report a port they were given instead, as the
# server binds it: natsd one named by -p or -m, and the listener the one held-port.py put in its
# ENVOY_PUBLIC_URL, which is its keeper's. With DEV_STACK_TEST_BROKER_FAILS set at build time the
# broker stub instead exits 1 without reporting an address, which is a failure part way through a
# start; with DEV_STACK_TEST_UNANSWERED set (unanswered_before, below) the listener or Dispatch stub
# serves nothing on the port it reports, and exits once a request is outstanding or stays alive,
# which is a failure after a process reported its port; and with
# DEV_STACK_TEST_NATSD_MOVES set the natsd stub reports its own PID as its client
# port whatever -p names, which is a server that came back on another port; each is baked in
# rather than read from the environment because dev-stack.sh starts every process with env -i,
# so nothing of this test's environment reaches one. The client stubs (agent-secrets,
# agent-secrets-helper) are never run by dev-stack.sh, which only puts their directory on PATH.
# With DEV_STACK_TEST_GO_CALL_FILE set, every call is also recorded there, which is how a
# --binaries run shows it called no go at all. ---
cat >"${fake_bin_dir}/go" <<'EOF'
#!/usr/bin/env bash
# Every call, recorded where the caller asks, so a --binaries run can show it made none.
[[ -z "${DEV_STACK_TEST_GO_CALL_FILE:-}" ]] || printf 'go %s\n' "$*" >>"$DEV_STACK_TEST_GO_CALL_FILE"
out=""
prev=""
packages=()
for arg in "$@"; do
  if [[ "$prev" == "-o" ]]; then
    out="$arg"
  fi
  case "$arg" in ./*) packages+=("$arg") ;; esac
  prev="$arg"
done
if [[ -z "$out" || ${#packages[@]} -eq 0 ]]; then
  echo "fake go: want build -o <dir>/ ./pkg..." >&2
  exit 1
fi
mkdir -p "$out"
# unanswered_before NAME and unanswered_after NAME append to the stub of server NAME (the listener
# or Dispatch, the two that serve on a socket a keeper holds) what DEV_STACK_TEST_UNANSWERED gives
# it. NAME:exits or NAME:stays: before it reports its address it names its port in
# DEV_STACK_TEST_UNSERVED_FILE, so the fake curl answers no request there, as nothing answers one on
# a kept socket that nothing serves. After it, NAME:exits waits up to 10 s for the first such
# request and exits 1, as a server whose start fails once it serves does (a listener refused its
# notification stream), so the request is outstanding when it goes; NAME:stays stays alive and
# serves nothing, as a stuck server does. The file is baked in for the reason the switches are.
unanswered_before() {
  [[ "${DEV_STACK_TEST_UNANSWERED:-}" == "$1":* ]] || return 0
  printf 'unserved=%q\n' "${DEV_STACK_TEST_UNSERVED_FILE:?}" >>"$target"
  cat >>"$target" <<'UNSERVED'
echo "$port" >>"$unserved"
UNSERVED
}
unanswered_after() {
  [[ "${DEV_STACK_TEST_UNANSWERED:-}" == "$1:exits" ]] || return 0
  cat >>"$target" <<'EXITS'
for _ in $(seq 1 100); do
  grep -qx "$port" "$unserved.asked" 2>/dev/null && break
  sleep 0.1
done
EXITS
  printf 'echo "fake %s: its start fails once it serves; exiting" >&2\nexit 1\n' "$1" >>"$target"
}
for package in "${packages[@]}"; do
  name="${package##*/}"
  target="${out%/}/$name"
  case "$name" in
    dev-oidc-issuer)
      cat >"$target" <<'ISSUER'
#!/usr/bin/env bash
code_clients="" device_clients="" sign_in_as=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --token)
      account_and_file="${2#*=}"
      printf 'fake-token-for-%s\n' "${account_and_file%%=*}" >"${account_and_file#*=}"
      ;;
    --code-flow) code_clients="$code_clients ${2%%=*}:$(stat -c %a "${2#*=}")" ;;
    --device-flow) device_clients="$device_clients ${2%%=*}:$(stat -c %a "${2#*=}")" ;;
    --sign-in-as) sign_in_as="$2" ;;
    *)
      echo "fake dev-oidc-issuer: unexpected argument $1" >&2
      exit 2
      ;;
  esac
  shift 2 || exit 2
done
echo "fake issuer: code-flow$code_clients device-flow$device_clients sign-in-as=$sign_in_as" >&2
echo "dev-oidc-issuer: issuer listening url=http://127.0.0.1:$$" >&2
ISSUER
      ;;
    cipherd)
      cat >"$target" <<'BROKER'
#!/usr/bin/env bash
echo "fake broker: database=${BROKER_DATABASE_URL:-} issuer=${BROKER_K8S_OIDC_ISSUER:-} callers=${BROKER_API_ACCOUNTS:-}" >&2
echo "fake broker: environ=$(tr '\0' '\n' </proc/$$/environ | sed 's/=.*//' | sort | tr '\n' ' ')" >&2
echo "fake broker: cmdline=$(tr '\0' ' ' </proc/$$/cmdline)" >&2
echo "fake broker: machine-login=${BROKER_MACHINE_LOGIN_URL:-} sign-in-client=${BROKER_SIGNIN_CLIENT_ID:-} exchange=${BROKER_EXCHANGE_ACCOUNTS:-} metrics=${BROKER_METRICS_LISTEN_ADDR:-}" >&2
echo "fake broker: service-logins=${BROKER_SERVICE_LOGINS:-} project-access=${BROKER_PROJECT_ACCESS_URL:-} project-access-token=${BROKER_PROJECT_ACCESS_TOKEN_FILE:-}" >&2
echo "fake broker: airlock-rules=${BROKER_AIRLOCK_RULES_FILE:-} airlock-ca=${BROKER_AIRLOCK_CA_CERT_FILE:-} airlock-proxy=${BROKER_AIRLOCK_PROXY_URL:-} airlock-resolve=${BROKER_AIRLOCK_TEST_RESOLVE:-} airlock-dial=${BROKER_AIRLOCK_TEST_DIAL_ALLOW:-}" >&2
echo "fake broker: nats=${BROKER_NATS_URLS:-} seeds=${BROKER_NATS_SEED_FILE:-},${BROKER_NATS_ISSUER_SEED_FILE:-},${BROKER_NATS_XKEY_SEED_FILE:-} nats-audience=${BROKER_NATS_K8S_AUDIENCE:-} services=${BROKER_NATS_SERVICES:-}" >&2
echo "fake broker: pod-registrars=${BROKER_POD_REGISTRARS:-}" >&2
# The proxy binds the address it was given, as the real one does. dev-stack.sh starts every
# process with env -i, so nothing of this test's environment reaches the stub: the control for a
# lost port race bakes the other address in at build time, as DEV_STACK_TEST_BROKER_FAILS does.
if [ -n "${BROKER_AIRLOCK_LISTEN_ADDR:-}" ]; then
  printf '{"time":"2026-01-01T00:00:00Z","severity":"INFO","message":"airlock proxy listening","addr":"%s"}\n' \
    "${AIRLOCK_BOUND_ADDR:-$BROKER_AIRLOCK_LISTEN_ADDR}" >&2
fi
BROKER
      if [[ -n "${DEV_STACK_TEST_AIRLOCK_BOUND:-}" ]]; then
        # Written above the stub's own lines, so the proxy line it prints names this address.
        sed -i "1a AIRLOCK_BOUND_ADDR=${DEV_STACK_TEST_AIRLOCK_BOUND}" "$target"
      fi
      if [[ -n "${DEV_STACK_TEST_BROKER_FAILS:-}" ]]; then
        cat >>"$target" <<'REFUSES'
echo "fake broker: refusing to start" >&2
exit 1
REFUSES
      else
        cat >>"$target" <<'LISTENS'
printf '{"time":"2026-01-01T00:00:00Z","severity":"INFO","message":"broker listening","addr":"127.0.0.1:%s"}\n' "$$" >&2
if [[ -n "${BROKER_NATS_URLS:-}" ]]; then
  printf '{"time":"2026-01-01T00:00:00Z","severity":"INFO","message":"broker: the NATS auth service is answering"}\n' >&2
fi
LISTENS
      fi
      ;;
    natsd)
      cat >"$target" <<'NATSD'
#!/usr/bin/env bash
echo "[INF] Starting nats-server" >&2
echo "fake natsd: pid=$$" >&2
echo "fake natsd: args=$*" >&2
# The ports it was given, bound as nats-server binds them: -1 is its own choice, which this stub's
# PID stands for, and any other port is that port.
client=$$ monitor=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    -p)
      [[ "$2" == -1 ]] || client="$2"
      shift 2
      ;;
    -m)
      monitor="$2"
      [[ "$monitor" != -1 ]] || monitor=$$
      shift 2
      ;;
    *) shift ;;
  esac
done
NATSD
      if [[ -n "${DEV_STACK_TEST_NATSD_MOVES:-}" ]]; then
        cat >>"$target" <<'MOVES'
client=$$
MOVES
      fi
      if [[ -n "${DEV_STACK_TEST_NATSD_MONITOR_MOVES:-}" ]]; then
        # A restart, given the monitor port it served (not -1), comes back on one of its own.
        cat >>"$target" <<'MOVES'
[[ -z "$monitor" || "$monitor" == "$$" ]] || monitor=$$
MOVES
      fi
      cat >>"$target" <<'NATSD'
echo "[INF] Listening for client connections on 127.0.0.1:$client" >&2
[[ -z "$monitor" ]] || echo "[INF] Starting http monitor on 127.0.0.1:$monitor" >&2
echo "[INF] Server is ready" >&2
NATSD
      ;;
    listener)
      cat >"$target" <<'LISTENER'
#!/usr/bin/env bash
printf '{"level":"INFO","msg":"listener API auth: none","nats":"%s"}\n' "${NATS_URLS:-}" >&2
echo "fake listener: machine=${ENVOY_MACHINE_ID:-} nats-token=${NATS_SERVICE_TOKEN_FILE:-}" >&2
echo "fake listener: pid=$$ public=${ENVOY_PUBLIC_URL:-} listen-fd=${ENVOY_LISTEN_FD:-}" >&2
# It serves the socket it was handed, whose port held-port.py put in ENVOY_PUBLIC_URL; its own PID
# stands for a port only where it was handed none.
port="${ENVOY_PUBLIC_URL##*:}"
[[ "$port" =~ ^[0-9]+$ ]] || port=$$
LISTENER
      if [[ -n "${DEV_STACK_TEST_LISTENER_MOVES:-}" ]]; then
        # Every start after the first, which a restart is, reports a port of its own instead of its
        # keeper's. The count lives beside the stub, in this run's own bin directory.
        cat >>"$target" <<'MOVES'
starts_file="${0%/*}/.listener-starts"
starts=$(($(cat "$starts_file" 2>/dev/null || echo 0) + 1))
echo "$starts" >"$starts_file"
((starts == 1)) || port=$$
MOVES
      fi
      unanswered_before listener
      cat >>"$target" <<'LISTENER'
printf '{"level":"INFO","msg":"envoy-listener listening","addr":"127.0.0.1:%s"}\n' "$port" >&2
LISTENER
      unanswered_after listener
      ;;
    dispatch)
      cat >"$target" <<'DISPATCH'
#!/usr/bin/env bash
echo "fake dispatch: database=${DATABASE_URL:-} server-url=${DISPATCH_SERVER_URL:-} listen-fd=${DISPATCH_LISTEN_FD:-} envoy=${ENVOY_URL:-} broker=${DISPATCH_AGENT_SECRETS_URL:-} nats=${NATS_URLS:-} home=${HOME:-} nats-token=${NATS_SERVICE_TOKEN_FILE:-}" >&2
echo "fake dispatch: environ=$(tr '\0' '\n' </proc/$$/environ | sed 's/=.*//' | sort | tr '\n' ' ')" >&2
echo "fake dispatch: cmdline=$(tr '\0' ' ' </proc/$$/cmdline)" >&2
echo "fake dispatch: oidc-issuer=${DISPATCH_OIDC_ISSUER:-} oidc-audience=${DISPATCH_OIDC_AUDIENCE:-} oidc-accounts=${DISPATCH_OIDC_ACCOUNTS:-} access-accounts=${DISPATCH_ACCESS_ACCOUNTS:-}" >&2
port=$$
DISPATCH
      unanswered_before dispatch
      cat >>"$target" <<'DISPATCH'
echo "time=2026-01-01T00:00:00.000Z level=INFO msg=\"dispatch: listening\" addr=127.0.0.1:$port" >&2
DISPATCH
      unanswered_after dispatch
      ;;
    cipher | cipher-helper | natsprobe | dev-proxy-stands)
      cat >"$target" <<'CLIENT'
#!/usr/bin/env bash
exit 0
CLIENT
      chmod +x "$target"
      continue
      ;;
    *)
      echo "fake go: unhandled build target $package" >&2
      exit 1
      ;;
  esac
  # Every server stub ends the same way: stay alive as the backgrounded process dev-stack.sh waits
  # on, and end at 0 when it is stopped, killing the sleep so nothing of the stub outlives it. The
  # one named by DEV_STACK_TEST_IGNORES_TERM instead ignores SIGTERM and INT, as a server stuck in
  # its shutdown does, and ends only at SIGKILL: it becomes the sleep (exec keeps both its pid, the
  # port it reported, and the ignored signals), so nothing of it outlives that SIGKILL either.
  if [[ "${DEV_STACK_TEST_IGNORES_TERM:-}" == "$name" ]]; then
    cat >>"$target" <<'IGNORES'
trap '' TERM INT
exec sleep 999999
IGNORES
  else
    cat >>"$target" <<'IDLE'
sleeper=""
trap 'kill "$sleeper" 2>/dev/null; exit 0' TERM INT
sleep 999999 &
sleeper=$!
wait "$sleeper"
IDLE
  fi
  chmod +x "$target"
done
EOF
chmod +x "${fake_bin_dir}/go"

# Sets the global instance_pid array (never wrapped in a subshell: the EXIT trap's safety-net kill
# must see the same variable this assignment updates). The log and the call files are emptied
# here, before the fork, not only by the background job's own redirect: a reused log still holds
# the previous run's "dev-stack: ready." line until that job runs, and wait_for_ready would read
# it. TMPDIR is this test's own directory, so each run's work directory lives under it and this
# test's cleanup removes both. The run is given no LEGION_TEST_PG_DSN and no DATABASE_URL, so
# nothing of the machine it runs on decides which server it is pointed at. Arguments after the
# fourth are dev-stack.sh's own, after its --database-url. DEV_STACK_TEST_PATH, when set, is the
# run's whole PATH in place of the fakes before this test's own.
run_instance() {
  local which="$1" log_file="$2"
  prepare_instance "$@"
  shift 4
  "${instance_command[@]}" "$@" >"$log_file" 2>&1 &
  instance_pid[which]=$!
}

# run_piped_instance takes run_instance's arguments and starts the same run with its output piped
# through tee into the log, as `dev-stack.sh ... 2>&1 | tee <log>` does: one job, so one process
# group under set -m, holding tee as well, which a signal to the whole group reaches as it reaches
# a terminal's or a supervisor's pipeline. instance_pid gets dev-stack.sh's pid, the group's id
# (the first command of a job leads its group), and tee_pid tee's. The job is disowned, so this
# shell prints no "Terminated" notice when a row's signal stops tee, which would read as a failure
# in a log of this test; this shell still reaps both processes.
tee_pid=""
run_piped_instance() {
  local which="$1" log_file="$2"
  prepare_instance "$@"
  shift 4
  "${instance_command[@]}" "$@" 2>&1 | tee "$log_file" >/dev/null &
  tee_pid=$!
  instance_pid[which]="$(jobs -p %+)"
  disown %+
}

# prepare_instance empties an instance's log and call files and sets instance_command to the run
# both launchers above start, up to its --database-url.
declare -a instance_command=()
prepare_instance() {
  local log_file="$2" psql_call_file="$3" curl_call_file="$4"
  : >"$log_file"
  : >"$psql_call_file"
  : >"${psql_call_file}.pgpassword"
  : >"$curl_call_file"
  if [[ -n "${DEV_STACK_TEST_GO_CALL_FILE:-}" ]]; then
    : >"$DEV_STACK_TEST_GO_CALL_FILE"
  fi
  instance_command=(env --default-signal=INT -u LEGION_TEST_PG_DSN -u DATABASE_URL
    PATH="${DEV_STACK_TEST_PATH:-${fake_bin_dir}:${PATH}}" TMPDIR="$temporary_dir"
    DEV_STACK_TEST_PSQL_CALL_FILE="$psql_call_file" DEV_STACK_TEST_CURL_CALL_FILE="$curl_call_file"
    DEV_STACK_TEST_BROKER_FAILS="${DEV_STACK_TEST_BROKER_FAILS:-}"
    DEV_STACK_TEST_UNANSWERED="${DEV_STACK_TEST_UNANSWERED:-}"
    DEV_STACK_TEST_IGNORES_TERM="${DEV_STACK_TEST_IGNORES_TERM:-}"
    DEV_STACK_TEST_UNSERVED_FILE="$unserved_file"
    DEV_STACK_TEST_NATSD_MOVES="${DEV_STACK_TEST_NATSD_MOVES:-}"
    DEV_STACK_TEST_NATSD_MONITOR_MOVES="${DEV_STACK_TEST_NATSD_MONITOR_MOVES:-}"
    DEV_STACK_TEST_LISTENER_MOVES="${DEV_STACK_TEST_LISTENER_MOVES:-}"
    DEV_STACK_TEST_GO_CALL_FILE="${DEV_STACK_TEST_GO_CALL_FILE:-}"
    DEV_STACK_TEST_DOCKER_CALL_FILE="${DEV_STACK_TEST_DOCKER_CALL_FILE:-}"
    DEV_STACK_TEST_DOCKER_STATE="${DEV_STACK_TEST_DOCKER_STATE:-}"
    DEV_STACK_TEST_PICKED_PORT="${DEV_STACK_TEST_PICKED_PORT:-}"
    DEV_STACK_TEST_AIRLOCK_BOUND="${DEV_STACK_TEST_AIRLOCK_BOUND:-}"
    DEV_STACK_TEST_ARGV_FILE="${DEV_STACK_TEST_ARGV_FILE:-}"
    bash "$driver" --database-url "$admin_url")
}

wait_for_ready() {
  local log_file="$1"
  for _ in $(seq 1 150); do
    grep -q 'dev-stack: ready\.' "$log_file" 2>/dev/null && return 0
    sleep 0.1
  done
  return 1
}

# wait_for_exit N [BOUND [LABEL]] waits up to BOUND seconds (15 by default) for instance N to exit
# by itself and sets the global "exited_status" to its exit status. A run meant to stop that is
# still running then went on serving, or is stuck, and that is a failure of the row whatever status
# the row expects: its whole process group is stopped and the row fails, naming LABEL (instance N
# by default) and every process the run left running. Before, a timeout left "exited_status" at 0,
# which a row that expects a clean exit (e1, and e3's 0) read as a pass. The group, not the run
# alone: a run stuck in a command takes its own signal only once that command returns, so only a
# signal to the command ends it; and SIGKILL after 10 s more, since a run whose stop ignores TERM,
# as dev-stack.sh's own does, would hold this wait for good too.
# A piped run (run_piped_instance) has no status here: its job is disowned, so its wait fails and
# its rows read none.
exited_status=0
wait_for_exit() {
  local n="$1" bound="${2:-15}" label="${3:-instance $1}"
  local pid="${instance_pid[$n]}" left_running=""
  exited_status=0
  for _ in $(seq 1 $((bound * 10))); do
    kill -0 "$pid" 2>/dev/null || break
    sleep 0.1
  done
  if kill -0 "$pid" 2>/dev/null; then
    left_running="$(pgrep -a -g "$pid" | cut -c1-200 || true)"
    kill -TERM -"$pid" 2>/dev/null || true
    for _ in $(seq 1 100); do
      kill -0 -"$pid" 2>/dev/null || break
      sleep 0.1
    done
    kill -KILL -"$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
    instance_pid[n]=""
    fail "$label was still running ${bound} s after it was meant to stop: ${left_running:-no process listed}"
  fi
  wait "$pid" 2>/dev/null || exited_status=$?
  instance_pid[n]=""
}

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

# interrupt <n> <label> <log file> sends instance <n> the INT a terminal's Ctrl-C sends, once the
# instance's log shows its INT trap took: dev-stack.sh reports a run that started with SIGINT
# ignored, which no INT can stop, before it creates anything, so the row fails there by name
# rather than waiting out wait_for_exit's bound and reading the timeout as an exit status of 0.
interrupt() {
  if grep -q 'dev-stack: SIGINT was ignored when this run started' "$3"; then
    fail "$2 reports that it started with SIGINT ignored, so the INT this row sends cannot stop it"
  fi
  kill -INT "${instance_pid[$1]}"
}

# Each reader below ends in `|| true`: under this script's own pipefail a grep that matches
# nothing — the exact regression each caller checks for — exits 1, and head, awk and sed on empty
# input exit 0, so that 1 would become the pipeline's status and trip errexit before the caller's
# own emptiness check could print its diagnostic.
work_dir_of() { grep -o 'dev-stack: workdir .*' "$1" | head -n1 | awk '{print $3}' || true; }
# The suffix a run's database names are made from: the random part of its work directory's name,
# lowercased as dev-stack.sh lowercases it.
suffix_of() { printf '%s' "${1##*.}" | tr '[:upper:]' '[:lower:]'; }
created_databases() { grep -o 'create database dev_stack_[a-z0-9_]*' "$1" | awk '{print $3}' | sort | tr '\n' ' ' || true; }
dropped_databases() { grep -o 'drop database if exists dev_stack_[a-z0-9_]*' "$1" | awk '{print $5}' | sort | tr '\n' ' ' || true; }
# The port dev-stack.sh printed for one process, from its own "dev-stack: <name> <url>" line.
banner_port_of() { grep -o "dev-stack: $2 [a-z]*://127\.0\.0\.1:[0-9]*" "$1" | head -n1 | sed -E 's/.*:([0-9]+)$/\1/' || true; }
# The port a process itself reported, from the log file dev-stack.sh gave that process alone.
issuer_log_port() { grep -o 'issuer listening url=http://127\.0\.0\.1:[0-9]*' "$1" | head -n1 | sed -E 's/.*:([0-9]+)$/\1/' || true; }
broker_log_port() { grep -o '"message":"broker listening","addr":"127\.0\.0\.1:[0-9]*"' "$1" | head -n1 | sed -E 's/.*:([0-9]+)"$/\1/' || true; }
nats_log_port() { grep -o 'Listening for client connections on 127\.0\.0\.1:[0-9]*' "$1" | head -n1 | sed -E 's/.*:([0-9]+)$/\1/' || true; }
listener_log_port() { grep -o '"addr":"127\.0\.0\.1:[0-9]*"' "$1" | head -n1 | sed -E 's/.*:([0-9]+)"$/\1/' || true; }
dispatch_log_port() { grep -o 'msg="dispatch: listening" addr=127\.0\.0\.1:[0-9]*' "$1" | head -n1 | sed -E 's/.*:([0-9]+)$/\1/' || true; }
# The value of one "export NAME=..." line of the banner.
exported() { grep -o "export $2=.*" "$1" | head -n1 | cut -d= -f2- || true; }

# The globals assert_instance_ready sets for the caller: the run's work directory, the suffix its
# names are made from, the ports every process of it reported, the port its Dispatch keeper held,
# and the pids of the servers whose reported port is not their pid (the listener, whose port is its
# keeper's). A restart adds the pids of the servers it started to restarted_pids.
work_dir=""
run_suffix=""
kept_port=""
declare -A process_port=()
declare -A server_pid=()
restarted_pids=()
# The fifth argument is the query string of the server URL the run was given (with its "?"), by
# default admin_url's.
assert_instance_ready() {
  local label="$1" log_file="$2" psql_call_file="$3" curl_call_file="$4" query="${5-?sslmode=disable}"
  if ! wait_for_ready "$log_file"; then
    cat "$log_file" >&2
    fail "$label never reached ready"
  fi
  work_dir="$(work_dir_of "$log_file")"
  [[ -n "$work_dir" && -d "$work_dir" ]] || fail "$label named no work directory"
  run_suffix="$(suffix_of "$work_dir")"
  process_port=()
  server_pid=()
  restarted_pids=()

  # (a) Every process is started, and the port dev-stack.sh printed for it is the port that
  # process itself reported in its own log, never one the driver chose.
  local entry name reader port banner
  for entry in "dev-oidc-issuer:issuer_log_port:dev-oidc-issuer.log" \
    "broker:broker_log_port:broker.log" \
    "natsd:nats_log_port:natsd.log" \
    "listener:listener_log_port:listener.log" \
    "dispatch:dispatch_log_port:dispatch.log"; do
    name="${entry%%:*}"
    reader="${entry#*:}"
    reader="${reader%%:*}"
    port="$("$reader" "${work_dir}/logs/${entry##*:}")"
    [[ -n "$port" ]] || fail "$label: $name reported no port in its own log"
    kill -0 "$port" 2>/dev/null || fail "$label: $name's reported port $port is no live process of this run"
    banner="$(banner_port_of "$log_file" "$name")"
    [[ "$banner" == "$port" ]] || fail "$label: the banner names port '$banner' for $name, which reported $port"
    process_port["$name"]="$port"
  done

  # (b) The keeper's held port is the one the broker was told to send a machine login to and the
  # one Dispatch serves under: the reason a keeper holds Dispatch's port before either starts.
  kept_port="$(cat "${work_dir}/dispatch-port")"
  [[ -n "$kept_port" ]] || fail "$label: the Dispatch port keeper wrote no port"
  grep -q "machine-login=http://127\.0\.0\.1:${kept_port}/credentials/machine" "${work_dir}/logs/broker.log" ||
    fail "$label: the broker was not told Dispatch's machine-login page at the kept port $kept_port"
  grep -q "server-url=http://127\.0\.0\.1:${kept_port} " "${work_dir}/logs/dispatch.log" ||
    fail "$label: Dispatch's server URL does not name the kept port $kept_port"
  grep -q 'listen-fd=3' "${work_dir}/logs/dispatch.log" ||
    fail "$label: Dispatch was not given the keeper's socket (DISPATCH_LISTEN_FD)"
  # The broker's metrics port is one the kernel picks, since the fixed default is a port only one
  # of two stacks on a machine could hold.
  grep -q 'metrics=127\.0\.0\.1:0$' "${work_dir}/logs/broker.log" ||
    fail "$label: the broker was not started with BROKER_METRICS_LISTEN_ADDR=127.0.0.1:0"
  # The listener serves on its keeper's socket: the port it reported is the one its keeper holds,
  # which held-port.py put in its ENVOY_PUBLIC_URL, so a restart (l) serves that same port.
  local listener_kept_port
  listener_kept_port="$(cat "${work_dir}/listener-port" 2>/dev/null || true)"
  [[ -n "$listener_kept_port" ]] || fail "$label: the listener port keeper wrote no port"
  [[ "${process_port[listener]}" == "$listener_kept_port" ]] ||
    fail "$label: the listener reported port ${process_port[listener]}, not $listener_kept_port its keeper holds"
  grep -q "public=http://127\.0\.0\.1:${listener_kept_port} listen-fd=3$" "${work_dir}/logs/listener.log" ||
    fail "$label: the listener was not handed its keeper's socket (ENVOY_LISTEN_FD) and port (ENVOY_PUBLIC_URL)"
  server_pid[listener]="$(sed -n 's/^fake listener: pid=\([0-9]*\) .*/\1/p' "${work_dir}/logs/listener.log" | head -n1 || true)"
  [[ -n "${server_pid[listener]}" ]] || fail "$label: the listener stub logged no pid"

  # (c) One "ready." line, and the exports after it, each naming the port its own process reported.
  local ready_lines
  ready_lines="$(grep -c 'dev-stack: ready\.' "$log_file" || true)"
  [[ "$ready_lines" -eq 1 ]] || fail "$label printed 'dev-stack: ready.' $ready_lines times"
  awk '/dev-stack: ready\./ { seen = 1 } /^  export AGENT_SECRETS_URL=/ { if (!seen) exit 1 }' "$log_file" ||
    fail "$label printed its exports before it was ready"
  local exported_value
  exported_value="$(exported "$log_file" AGENT_SECRETS_URL)"
  [[ "$exported_value" == "http://127.0.0.1:${process_port[broker]}" ]] ||
    fail "$label exported AGENT_SECRETS_URL=$exported_value, not the broker's own port"
  exported_value="$(exported "$log_file" DISPATCH_URL)"
  [[ "$exported_value" == "http://127.0.0.1:${process_port[dispatch]}" ]] ||
    fail "$label exported DISPATCH_URL=$exported_value, not Dispatch's own port"
  exported_value="$(exported "$log_file" ENVOY_URL)"
  [[ "$exported_value" == "http://127.0.0.1:${process_port[listener]}" ]] ||
    fail "$label exported ENVOY_URL=$exported_value, not the listener's own port"
  exported_value="$(exported "$log_file" NATS_URLS)"
  [[ "$exported_value" == "nats://127.0.0.1:${process_port[natsd]}" ]] ||
    fail "$label exported NATS_URLS=$exported_value, not natsd's own port"
  exported_value="$(exported "$log_file" PATH)"
  [[ "$exported_value" == "${work_dir}/bin:\$PATH" ]] ||
    fail "$label exported PATH=$exported_value, not this run's binaries first"
  exported_value="$(exported "$log_file" AGENT_SECRETS_ACCOUNT_TOKEN_FILE)"
  [[ -s "$exported_value" ]] ||
    fail "$label exported AGENT_SECRETS_ACCOUNT_TOKEN_FILE=$exported_value, which its issuer never wrote"
  # The records file this run's helper keeps its sessions in, under this run's own work
  # directory: a named session's record holds that session's signing key, and the helper keeps
  # its records beside its machine login by default, so a run that exported only a socket would
  # have its helper overwrite the records of the helper installed under the caller's own HOME
  # (dispatch://INFRA-1167).
  exported_value="$(exported "$log_file" AGENT_SECRETS_STATE_FILE)"
  [[ "$exported_value" == "${work_dir}/sessions.json" ]] ||
    fail "$label exported AGENT_SECRETS_STATE_FILE=$exported_value, not a records file of its own under its work directory"

  # (g) The second box, which a session moving from one box to another is driven on: a path of
  # its own for each of the first box's, all under this run's work directory, so two boxes of one
  # stack share no state. A box at the broker is a key — launcher_credentials_live_key allows one
  # live launcher credential per key — so a second box holding one path of the first box's would
  # be the first box.
  local first_box_paths=" ${work_dir} ${work_dir}/keys ${work_dir}/machine.json ${work_dir}/sessions.json ${work_dir}/operator ${work_dir}/helper.sock ${work_dir}/metadata-port "
  local box2_paths=" " variable value
  for variable in DIR HOME XDG_RUNTIME_DIR XDG_STATE_HOME AGENT_SECRETS_KEY_DIR \
    AGENT_SECRETS_MACHINE_FILE AGENT_SECRETS_STATE_FILE AGENT_SECRETS_SIGNIN_DIR \
    AGENT_SECRETS_OPERATOR_FILE AGENT_SECRETS_HELPER_SOCK METADATA_PORT_FILE; do
    value="$(exported "$log_file" "DEV_STACK_BOX2_${variable}")"
    [[ -n "$value" ]] || fail "$label printed no export DEV_STACK_BOX2_${variable} for its second box"
    [[ "$value" == "${work_dir}/"* ]] ||
      fail "$label put the second box's $variable at $value, outside this run's own work directory"
    [[ "$first_box_paths" != *" $value "* ]] ||
      fail "$label gave the second box $value as its $variable, which is the first box's own path"
    [[ "$box2_paths" != *" $value "* ]] ||
      fail "$label gave the second box one path, $value, as two of its own"
    box2_paths="${box2_paths}${value} "
  done
  [[ -n "$(exported "$log_file" DEV_STACK_BOX2_SESSION)" ]] ||
    fail "$label printed no export DEV_STACK_BOX2_SESSION, the session name the second box's own trades write under"

  # The directories its helper and its client write into are there, and its operator file names
  # the person the first box's does, since one person approves on both boxes.
  for variable in DIR HOME XDG_RUNTIME_DIR XDG_STATE_HOME AGENT_SECRETS_KEY_DIR AGENT_SECRETS_SIGNIN_DIR; do
    value="$(exported "$log_file" "DEV_STACK_BOX2_${variable}")"
    [[ -d "$value" ]] || fail "$label named the second box's $variable $value, a directory it never created"
  done
  value="$(exported "$log_file" DEV_STACK_BOX2_AGENT_SECRETS_OPERATOR_FILE)"
  [[ "$(cat "$value" 2>/dev/null)" == "$(cat "$(exported "$log_file" AGENT_SECRETS_OPERATOR_FILE)" 2>/dev/null)" ]] ||
    fail "$label wrote no operator file for the second box at $value naming the person the first box's names"
  # The helper writes the machine file and the stand-in writes the port file, so neither is there
  # yet: the line the banner prints waits for the port file to hold a port.
  for variable in AGENT_SECRETS_MACHINE_FILE AGENT_SECRETS_STATE_FILE AGENT_SECRETS_HELPER_SOCK METADATA_PORT_FILE; do
    value="$(exported "$log_file" "DEV_STACK_BOX2_${variable}")"
    [[ ! -e "$value" ]] ||
      fail "$label created the second box's $variable at $value, which only its helper or its stand-in writes"
  done

  # The stand-in takes its port file from its environment and falls back to the first box's, so
  # the second box's stand-in writes a port file of its own and the first box's keeps working as
  # it did; with the path baked in, the second to start would overwrite the first box's port.
  grep -qxF "PORT_FILE = os.environ.get(\"DEV_STACK_METADATA_PORT_FILE\") or \"${work_dir}/metadata-port\"" \
    "${work_dir}/dev-metadata.py" ||
    fail "$label's metadata stand-in does not read DEV_STACK_METADATA_PORT_FILE, falling back to ${work_dir}/metadata-port"
  # The lines that start this box's stand-in and its helper, each naming the second box's own
  # variable, so a caller pastes them with the block above exported and starts nothing of the
  # first box's.
  # shellcheck disable=SC2016 # the banner prints these dollar signs; they are not this shell's
  for value in 'DEV_STACK_METADATA_PORT_FILE=$DEV_STACK_BOX2_METADATA_PORT_FILE' \
    'AGENT_SECRETS_HELPER_SOCK=$DEV_STACK_BOX2_AGENT_SECRETS_HELPER_SOCK' \
    'AGENT_SECRETS_STATE_FILE=$DEV_STACK_BOX2_AGENT_SECRETS_STATE_FILE'; do
    grep -qF "$value" "$log_file" ||
      fail "$label printed no line that starts the second box with $value"
  done
  grep -qF "${work_dir}/bin/agent-secrets-helper serve" "$log_file" ||
    fail "$label printed no line that starts the second box's helper from this run's own binaries"

  # Each readiness check this run made named its own process's port, so no check of it could have
  # been answered by another run's server, and gave up after 5 s, so none could wait on a port
  # nothing serves any longer (e7).
  grep -qxF -e "-sf --max-time 5 http://127.0.0.1:${process_port[broker]}/healthz" "$curl_call_file" ||
    fail "$label did not check its own broker's health at port ${process_port[broker]} with a 5 s bound"
  grep -qxF -e "-sf --max-time 5 http://127.0.0.1:${process_port[listener]}/healthz" "$curl_call_file" ||
    fail "$label did not check its own listener's health at port ${process_port[listener]} with a 5 s bound"
  grep -qxF -e "-sf --max-time 5 http://127.0.0.1:${process_port[dispatch]}/api/v1" "$curl_call_file" ||
    fail "$label did not read its own Dispatch's route index at port ${process_port[dispatch]} with a 5 s bound"

  # Two databases of this run's own, named for it, and no other; each server pointed at its own,
  # the broker with production's pool size (k), whole URL to whole URL.
  local created broker_query="?pool_max_conns=10"
  [[ -z "$query" ]] || broker_query="${query}&pool_max_conns=10"
  created="$(created_databases "$psql_call_file")"
  [[ "$created" == "dev_stack_broker_${run_suffix} dev_stack_dispatch_${run_suffix} " ]] ||
    fail "$label created '$created', not one broker and one Dispatch database of its own"
  # No psql call holds the server's password in its arguments, where `ps -ww` shows it; each was
  # handed it in PGPASSWORD alone.
  ! grep -qF 's3cret' "$psql_call_file" ||
    fail "$label handed psql the server's password in its arguments: $(grep -F 's3cret' "$psql_call_file" | head -n 1)"
  [[ "$(sort -u "${psql_call_file}.pgpassword")" == "s3cret" ]] ||
    fail "$label did not hand every psql call the server's password in PGPASSWORD: $(sort -u "${psql_call_file}.pgpassword" | tr '\n' ' ')"
  grep -qF "fake broker: database=${server_url}/dev_stack_broker_${run_suffix}${broker_query} issuer=" "${work_dir}/logs/broker.log" ||
    fail "$label did not point its broker at its own database with pool_max_conns=10: $(grep 'fake broker: database' "${work_dir}/logs/broker.log")"
  grep -qF "fake dispatch: database=${server_url}/dev_stack_dispatch_${run_suffix}${query} server-url=" "${work_dir}/logs/dispatch.log" ||
    fail "$label did not point Dispatch at its own database with the URL's query string alone: $(grep 'fake dispatch: database' "${work_dir}/logs/dispatch.log")"
  assert_pod_registrars "$label" "${work_dir}/logs/broker.log"
}

# Every process of the run is gone, by the PID each reported as its port, the keeper with them,
# the servers whose port is not their pid, and every server a restart started.
assert_everything_stopped() {
  local label="$1" pid
  for pid in "${process_port[@]}" "$kept_port" "${server_pid[@]}" "${restarted_pids[@]}"; do
    [[ -n "$pid" ]] || continue
    if kill -0 "$pid" 2>/dev/null; then
      fail "$label left process $pid running"
    fi
  done
}

# assert_project_access, which a run with --project-access and a run without it are each held to,
# and the cases (q), (r) and (s) below call: lib/dev-stack-test-project-access.sh's.
# shellcheck source=lib/dev-stack-test-project-access.sh
. "${project_root}/packages/envoy/scripts/lib/dev-stack-test-project-access.sh"

assert_databases_dropped() {
  local label="$1" psql_call_file="$2" suffix="$3" dropped
  dropped="$(dropped_databases "$psql_call_file")"
  [[ "$dropped" == "dev_stack_broker_${suffix} dev_stack_dispatch_${suffix} " ]] ||
    fail "$label dropped '$dropped', not exactly the two databases it created"
}

# (u) Every broker of a run lists the account the run's registrar token is for as the registrar
# of the pod's account, and lists nothing else (BROKER_POD_REGISTRARS). A real broker admits a
# registration from no registrar its list does not pair with the pod's account, and with the list
# unset from none, so one started without it refuses the one registration the run makes 403
# POD_REGISTRAR_NOT_LISTED and the run exits before it is ready. The line is matched whole, so a
# list holding any other entry fails too.
readonly want_pod_registrars="system:serviceaccount:example-ns:example-daemon=system:serviceaccount:example-ns:example-worker"
assert_pod_registrars() {
  local label="$1" broker_log="$2"
  grep -qxF "fake broker: pod-registrars=${want_pod_registrars}" "$broker_log" ||
    fail "$label started the broker of ${broker_log##*/} without BROKER_POD_REGISTRARS=${want_pod_registrars}, the run's registrar for its pod's account alone: $(grep 'fake broker: pod-registrars' "$broker_log")"
}

log1="${temporary_dir}/instance1.log"
log2="${temporary_dir}/instance2.log"
psql_calls1="${temporary_dir}/psql-calls-1"
psql_calls2="${temporary_dir}/psql-calls-2"
curl_calls1="${temporary_dir}/curl-calls-1"
curl_calls2="${temporary_dir}/curl-calls-2"
go_calls1="${temporary_dir}/go-calls-1"
argv1="${temporary_dir}/argv-1"

# Both instances start before either is waited on: this is the scenario a port and a database of
# each run's own exist to make safe, not two sequential runs that never contend for anything.
# Instance 1 records its go calls, the control for (h): a run that builds is seen to call go; and
# the argument lists its fake env and fake python3 were handed, which (u2) reads.
: >"$argv1"
DEV_STACK_TEST_GO_CALL_FILE="$go_calls1" DEV_STACK_TEST_ARGV_FILE="$argv1" run_instance 1 "$log1" "$psql_calls1" "$curl_calls1"
run_instance 2 "$log2" "$psql_calls2" "$curl_calls2"

assert_instance_ready 'instance 1' "$log1" "$psql_calls1" "$curl_calls1"
assert_project_access 'instance 1' "$log1" false
work_dir1="$work_dir"
suffix1="$run_suffix"
kept_port1="$kept_port"
listener_pid1="${server_pid[listener]}"
grep -q '^go build -o ' "$go_calls1" ||
  fail "instance 1 built nothing through the fake go, so a run that records no go call proves nothing"
# (s) Only --project-access runs the airlock's stands, so a run without it builds none.
if grep -q 'dev-proxy-stands' "$go_calls1"; then
  fail "instance 1, run without --project-access, built the stands (cmd/dev-proxy-stands): $(cat "$go_calls1")"
fi
declare -A port1=()
for name in "${!process_port[@]}"; do port1["$name"]="${process_port[$name]}"; done
printf "PASS: instance 1 started every process and took each port from that process's own report (issuer %s, broker %s, natsd %s, listener %s, dispatch %s; Dispatch keeper %s)\n" \
  "${port1[dev-oidc-issuer]}" "${port1[broker]}" "${port1[natsd]}" "${port1[listener]}" "${port1[dispatch]}" "$kept_port1"
# The control for (j): a run without --callout runs natsd with no callout and one broker that runs
# no auth service, gives the listener and Dispatch no NATS token, and exports nothing of a callout.
grep -q 'fake broker: nats= seeds=,, nats-audience= services=$' "${work_dir1}/logs/broker.log" ||
  fail "instance 1, run without --callout, gave its broker NATS auth service settings"
[[ ! -e "${work_dir1}/logs/broker-2.log" ]] || fail "instance 1, run without --callout, started a second broker"
grep -q -- '-callout-keys' "${work_dir1}/logs/natsd.log" && fail "instance 1, run without --callout, ran natsd with -callout-keys"
grep -q 'nats-token=$' "${work_dir1}/logs/listener.log" || fail "instance 1, run without --callout, gave its listener a NATS token"
[[ -z "$(exported "$log1" DEV_STACK_CALLOUT)" ]] || fail "instance 1, run without --callout, exported DEV_STACK_CALLOUT"
printf 'PASS: a run without --callout runs no auth service, no second broker and no NATS token\n'
# (u2) The broker and Dispatch are started with exactly the settings env -i gave them, and the
# database URL, whose password is s3cret, in no process's arguments: not the broker's or
# Dispatch's own (each stub reads its /proc/<pid>/cmdline), and not those of any env or python3 the
# run started along the way (the fakes of both record theirs), where env -i put every setting.
declare -A want_environ=(
  [broker]="BROKER_ACCESS_TOKEN_SECONDS BROKER_API_ACCOUNTS BROKER_DATABASE_URL BROKER_DRAIN_SECONDS BROKER_EXCHANGE_ACCOUNTS BROKER_EXCHANGE_K8S_AUDIENCE BROKER_FAKE_SECRETS_FILE BROKER_GOOGLE_OIDC_AUDIENCE BROKER_GOOGLE_OIDC_ISSUER BROKER_K8S_OIDC_AUDIENCE BROKER_K8S_OIDC_ISSUER BROKER_LISTEN_ADDR BROKER_MACHINE_LOGIN_URL BROKER_METRICS_LISTEN_ADDR BROKER_POD_REGISTRARS BROKER_PUBLIC_URL BROKER_SECRETS_KMS_KEY_ARN BROKER_SECRETS_PREFIX BROKER_SERVICE_LOGINS BROKER_SIGNIN_CLIENT_ID BROKER_SIGNIN_CLIENT_SECRET_FILE BROKER_SIGNIN_DEVICE_AUDIENCE BROKER_SIGNIN_DOMAIN BROKER_SIGNIN_ISSUER BROKER_VM_EXCHANGE_ACCOUNTS BROKER_VM_SERVICES PATH "
  [dispatch]="DATABASE_URL DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE DISPATCH_AGENT_SECRETS_URL DISPATCH_AGENT_TOKEN DISPATCH_DEV_SIGNIN DISPATCH_IDENTITY DISPATCH_INSECURE_COOKIE DISPATCH_LISTEN_FD DISPATCH_LISTEN_HOST DISPATCH_SERVER_URL DISPATCH_WEB_DIST ENVOY_ACCOUNT_TOKEN_FILE ENVOY_TOKEN ENVOY_URL HOME NATS_URLS PATH XDG_CACHE_HOME XDG_CONFIG_HOME XDG_DATA_HOME "
)
for server in broker dispatch; do
  got="$(sed -n "s/^fake ${server}: environ=//p" "${work_dir1}/logs/${server}.log" | head -n1 || true)"
  [[ "$got" == "${want_environ[$server]}" ]] ||
    fail "instance 1 started its ${server} with the environment '${got}', not exactly '${want_environ[$server]}'"
  cmdline="$(sed -n "s/^fake ${server}: cmdline=//p" "${work_dir1}/logs/${server}.log" | head -n1 || true)"
  [[ -n "$cmdline" && "$cmdline" != *s3cret* ]] ||
    fail "instance 1's ${server} read no arguments of its own, or holds the database URL's password in them: '${cmdline}'"
done
[[ -s "$argv1" ]] || fail "instance 1 recorded no argument list of its env or python3, so (u2) reads nothing"
if grep -n s3cret "$argv1" >&2; then
  fail "instance 1 handed the database URL's password to a process's arguments (above, from its fake env and python3)"
fi
printf 'PASS: instance 1 started its broker and Dispatch with exactly the settings env -i gave them, and the database URL'"'"'s password in no process'"'"'s arguments\n'

assert_instance_ready 'instance 2' "$log2" "$psql_calls2" "$curl_calls2"
work_dir2="$work_dir"
suffix2="$run_suffix"
kept_port2="$kept_port"
listener_pid2="${server_pid[listener]}"
declare -A port2=()
for name in "${!process_port[@]}"; do port2["$name"]="${process_port[$name]}"; done
printf "PASS: instance 2 started every process and took each port from that process's own report (issuer %s, broker %s, natsd %s, listener %s, dispatch %s; Dispatch keeper %s)\n" \
  "${port2[dev-oidc-issuer]}" "${port2[broker]}" "${port2[natsd]}" "${port2[listener]}" "${port2[dispatch]}" "$kept_port2"

# (d) Two concurrent runs share nothing: not a work directory, not a port, not a database.
[[ "$work_dir1" != "$work_dir2" ]] || fail "both instances used the work directory $work_dir1"
if grep -q "$work_dir2" "$log1"; then
  fail "instance 1 named instance 2's work directory"
fi
if grep -q "$work_dir1" "$log2"; then
  fail "instance 2 named instance 1's work directory"
fi
for name in dev-oidc-issuer broker natsd listener dispatch; do
  [[ "${port1[$name]}" != "${port2[$name]}" ]] ||
    fail "both instances' $name reported port ${port1[$name]}"
done
[[ "$kept_port1" != "$kept_port2" ]] || fail "both instances held the Dispatch port $kept_port1"
[[ "$suffix1" != "$suffix2" ]] || fail "both instances named their databases dev_stack_*_${suffix1}"
box2_dir1="$(exported "$log1" DEV_STACK_BOX2_DIR)"
box2_dir2="$(exported "$log2" DEV_STACK_BOX2_DIR)"
[[ "$box2_dir1" != "$box2_dir2" ]] || fail "both instances gave their second box the directory $box2_dir1"
printf 'PASS: two concurrent instances used two work directories, two sets of ports and two sets of databases (%s| %s)\n' \
  "$(created_databases "$psql_calls1")" "$(created_databases "$psql_calls2")"
printf 'PASS: each instance laid out a second box of its own, with its own HOME, XDG directories, key directory, machine file, records file, sign-in store, operator file, helper socket and metadata port file (%s | %s)\n' \
  "$box2_dir1" "$box2_dir2"

# (e1) A normal exit: one process of the stack stops by itself, so the run stops the rest, drops
# both of its databases and exits with that process's status.
kill -TERM "${port1[natsd]}"
wait_for_exit 1
[[ "$exited_status" -eq 0 ]] || fail "instance 1 exited $exited_status after a process of it stopped cleanly"
grep -q 'dev-stack: a process of this stack stopped (status 0)' "$log1" ||
  fail "instance 1 did not report the process that stopped"
process_port=()
for name in "${!port1[@]}"; do process_port["$name"]="${port1[$name]}"; done
kept_port="$kept_port1"
server_pid=([listener]="$listener_pid1")
assert_everything_stopped 'instance 1'
assert_databases_dropped 'instance 1' "$psql_calls1" "$suffix1"
printf 'PASS: a normal exit stopped every process instance 1 started and dropped both its databases\n'

# (e2) An interrupt: the same stop, with 130, the status a run interrupted by Ctrl-C reports.
interrupt 2 'instance 2' "$log2"
wait_for_exit 2
[[ "$exited_status" -eq 130 ]] || fail "instance 2 exited $exited_status on INT, not 130"
process_port=()
for name in "${!port2[@]}"; do process_port["$name"]="${port2[$name]}"; done
kept_port="$kept_port2"
server_pid=([listener]="$listener_pid2")
assert_everything_stopped 'instance 2'
assert_databases_dropped 'instance 2' "$psql_calls2" "$suffix2"
printf 'PASS: INT stopped every process instance 2 started, dropped both its databases and exited 130\n'

# (e3) A stop by signal, as a supervisor sends one: SIGTERM to the whole process group, which
# reaches dev-stack.sh as it reaches every process of its stack, and sent again while the stop
# is running. Both databases are dropped, exactly as on a normal exit. The run that found this
# left them on the server: cleanup disarmed its own TERM trap, so a signal bash had taken and
# not yet handled was re-raised with the shell back on the default disposition, which killed the
# run between the kills and the drops.
run_instance 1 "$log1" "$psql_calls1" "$curl_calls1"
assert_instance_ready 'the SIGTERM stop' "$log1" "$psql_calls1" "$curl_calls1"
term_suffix="$run_suffix"
term_kept_port="$kept_port"
term_listener_pid="${server_pid[listener]}"
declare -A term_port=()
for name in "${!process_port[@]}"; do term_port["$name"]="${process_port[$name]}"; done
# -PID is the instance's own process group, which set -m gave it. The signal is sent again, as
# fast as this shell can send it, for as long as that group holds a process: one of the repeats
# then certainly arrives while the stop is already running, which is the case this is here for
# and what the group's own signal does to dev-stack.sh by chance whenever a process of the stack
# takes it first. The loop ends the moment there is no process left to signal.
for _ in $(seq 1 200); do
  kill -TERM -"${instance_pid[1]}" 2>/dev/null || break
done
wait_for_exit 1 15 'the SIGTERM stop'
# 143 when dev-stack.sh's own TERM trap ran the stop, 0 when a process of the stack took the
# same signal first and the stop ran from the EXIT trap of that clean exit: a group's signal
# reaches both, and which of the two gets there first is the kernel's to decide.
[[ "$exited_status" -eq 143 || "$exited_status" -eq 0 ]] ||
  fail "the SIGTERM stop exited $exited_status, neither 143 nor the status of a process of its own"
grep -q 'dev-stack: stopped; logs kept in' "$log1" ||
  fail "the SIGTERM stop did not reach the end of its cleanup"
process_port=()
for name in "${!term_port[@]}"; do process_port["$name"]="${term_port[$name]}"; done
kept_port="$term_kept_port"
server_pid=([listener]="$term_listener_pid")
assert_everything_stopped 'the SIGTERM stop'
assert_databases_dropped 'the SIGTERM stop' "$psql_calls1" "$term_suffix"
printf 'PASS: SIGTERM to the whole process group, sent again while the stop ran, stopped every process of the run and dropped both its databases\n'

# (e4) A failure part way through, before ready: the broker refuses to start, so the run fails,
# and the processes it had already started are stopped and its databases dropped all the same.
DEV_STACK_TEST_BROKER_FAILS=1 run_instance 1 "$log1" "$psql_calls1" "$curl_calls1"
wait_for_exit 1 15 'a run whose broker refused to start'
[[ "$exited_status" -ne 0 ]] || fail "a run whose broker refused to start reported success"
if grep -q 'dev-stack: ready\.' "$log1"; then
  fail "a run whose broker refused to start reported ready"
fi
failed_work_dir="$(work_dir_of "$log1")"
[[ -n "$failed_work_dir" ]] || fail "the failed run named no work directory"
# natsd starts before the brokers, which with --callout read the seeds it makes, so it is running
# when the broker refuses, and is stopped with the rest.
process_port=()
server_pid=()
process_port[dev-oidc-issuer]="$(issuer_log_port "${failed_work_dir}/logs/dev-oidc-issuer.log")"
[[ -n "${process_port[dev-oidc-issuer]}" ]] || fail "the failed run started no issuer to stop"
process_port[natsd]="$(nats_log_port "${failed_work_dir}/logs/natsd.log")"
[[ -n "${process_port[natsd]}" ]] || fail "the failed run started no natsd to stop"
kept_port="$(cat "${failed_work_dir}/dispatch-port")"
assert_everything_stopped 'the failed run'
assert_databases_dropped 'the failed run' "$psql_calls1" "$(suffix_of "$failed_work_dir")"
for name in listener dispatch; do
  [[ ! -s "${failed_work_dir}/logs/${name}.log" ]] ||
    fail "the failed run started $name after its broker had refused"
done
printf 'PASS: a broker that refuses to start leaves nothing running: the issuer, natsd and the keeper are stopped and both databases dropped\n'

# (e5) The stop of (e3) with the run's output piped through tee, as a terminal or a supervisor's log
# pipes it: the group's SIGTERM stops tee too, before the run has finished its own stop, so every
# line that stop writes goes to a pipe nobody reads. Both databases are dropped all the same. The
# run that found this left its Dispatch database on the server: the first line its stop wrote after
# the broker's drop raised SIGPIPE, whose default action killed it between the two drops.
run_piped_instance 1 "$log1" "$psql_calls1" "$curl_calls1"
assert_instance_ready 'the SIGTERM stop through tee' "$log1" "$psql_calls1" "$curl_calls1"
for _ in $(seq 1 200); do
  kill -TERM -"${instance_pid[1]}" 2>/dev/null || break
done
wait_for_exit 1 15 'the SIGTERM stop through tee'
kill -0 "$tee_pid" 2>/dev/null && fail "the SIGTERM stop through tee left tee (pid $tee_pid) running"
assert_everything_stopped 'the SIGTERM stop through tee'
assert_databases_dropped 'the SIGTERM stop through tee' "$psql_calls1" "$run_suffix"
printf 'PASS: SIGTERM to the whole process group of a run piped through tee, which the signal stops too, stopped every process of the run and dropped both its databases\n'

# (e6) A run whose output reader is gone and which then stops by itself: tee stopped alone, then a
# process of the stack. The run reports that process's stop on a line nobody reads, then stops the
# rest and drops both databases, as (e1) does. A run stopped by its group's SIGTERM comes this way
# whenever a process of the stack takes the signal before this shell does (the 0 of (e3)). Bash
# runs its EXIT trap on the SIGPIPE that first line raises, so the stop begins; before the stop
# ignored PIPE, its own first line after the broker's drop raised a second SIGPIPE, on which bash,
# already ending on one, killed itself, and the Dispatch database outlived the run.
run_piped_instance 1 "$log1" "$psql_calls1" "$curl_calls1"
assert_instance_ready 'the run whose output reader is gone' "$log1" "$psql_calls1" "$curl_calls1"
kill -TERM "$tee_pid"
for _ in $(seq 1 50); do
  kill -0 "$tee_pid" 2>/dev/null || break
  sleep 0.1
done
kill -0 "$tee_pid" 2>/dev/null && fail "tee (pid $tee_pid) outlived its SIGTERM"
kill -TERM "${process_port[natsd]}"
wait_for_exit 1 15 'the run whose output reader is gone'
assert_everything_stopped 'the run whose output reader is gone'
assert_databases_dropped 'the run whose output reader is gone' "$psql_calls1" "$run_suffix"
printf 'PASS: a run whose output reader was gone stopped every process and dropped both its databases when a process of it stopped\n'

# collect_started DIR sets process_port, server_pid and kept_port to every process the run with work
# directory DIR started, from the logs and port files it got far enough to write, for
# assert_everything_stopped after a run that ended before it was ready.
collect_started() {
  local dir="$1" pid
  process_port=()
  server_pid=()
  restarted_pids=()
  [[ ! -f "${dir}/logs/dev-oidc-issuer.log" ]] || process_port[dev-oidc-issuer]="$(issuer_log_port "${dir}/logs/dev-oidc-issuer.log")"
  [[ ! -f "${dir}/logs/natsd.log" ]] || process_port[natsd]="$(nats_log_port "${dir}/logs/natsd.log")"
  [[ ! -f "${dir}/logs/broker.log" ]] || process_port[broker]="$(broker_log_port "${dir}/logs/broker.log")"
  [[ ! -f "${dir}/logs/dispatch.log" ]] || process_port[dispatch]="$(dispatch_log_port "${dir}/logs/dispatch.log")"
  if [[ -f "${dir}/logs/listener.log" ]]; then
    pid="$(sed -n 's/^fake listener: pid=\([0-9]*\) .*/\1/p' "${dir}/logs/listener.log" | head -n1 || true)"
    [[ -z "$pid" ]] || server_pid[listener]="$pid"
  fi
  [[ ! -s "${dir}/listener-port" ]] || server_pid[listener-keeper]="$(cat "${dir}/listener-port")"
  kept_port="$(cat "${dir}/dispatch-port" 2>/dev/null || true)"
}

# (e7) A process that fails after it reported its port, while its keeper still holds that port: the
# listener, then Dispatch, reports its address and exits 1, as a listener refused its notification
# stream does once it serves. The kernel queues a request to that port and no process ever answers
# it, so the run must not wait on one: inside wait_for_exit's 15 s it reports that the server
# exited, with the server's own log, stops the rest and drops both databases. The run that found
# this sent its health check with no time bound, which waited there for good, and a SIGTERM to the
# run with it, since bash runs a trap only once the command in the foreground has returned:
# callout-check.sh's own stop then waited on that run until its caller's 40-minute timeout. Each
# stub exits only once the run's request is outstanding at its port, so a run that never asked (its
# reader saw the server gone first) cannot pass the row without testing it.
for server in listener dispatch; do
  asked_path=/healthz
  [[ "$server" == listener ]] || asked_path=/api/v1
  label="the run whose $server exited after it reported its address"
  : >"$unserved_file"
  : >"${unserved_file}.asked"
  DEV_STACK_TEST_UNANSWERED="${server}:exits" run_instance 1 "$log1" "$psql_calls1" "$curl_calls1"
  wait_for_exit 1 15 "$label"
  [[ "$exited_status" -ne 0 ]] || fail "$label reported success"
  grep -q 'dev-stack: ready\.' "$log1" && fail "$label reported ready"
  grep -qx "dev-stack: $server exited during startup" "$log1" || fail "$label did not say so: $(cat "$log1")"
  grep -qx "fake $server: its start fails once it serves; exiting" "$log1" ||
    fail "$label did not print the ${server}'s own log"
  asked_port="$(head -n1 "${unserved_file}.asked" 2>/dev/null || true)"
  [[ -n "$asked_port" ]] || fail "$label never sent it a readiness request, so the row tested nothing"
  grep -qxF -e "-sf --max-time 5 http://127.0.0.1:${asked_port}${asked_path}" "$curl_calls1" ||
    fail "$label asked port $asked_port without the 5 s bound"
  exited_work_dir="$(work_dir_of "$log1")"
  [[ -n "$exited_work_dir" ]] || fail "$label named no work directory"
  collect_started "$exited_work_dir"
  for name in dev-oidc-issuer natsd broker; do
    [[ -n "${process_port[$name]:-}" ]] || fail "$label started no $name to stop"
  done
  assert_everything_stopped "$label"
  assert_databases_dropped "$label" "$psql_calls1" "$(suffix_of "$exited_work_dir")"
  printf 'PASS: a %s that exited after it reported its address, its keeper still holding the port, was reported with its log, and the run stopped the rest and dropped both databases inside 15 s\n' "$server"
done

# (e8) A SIGTERM to the run alone, as callout-check.sh's stop sends it, while a readiness request is
# outstanding at a server that stays alive and answers nothing: the run takes the signal once that
# request gives up, inside 15 s, and stops with 143. Before, the request waited for good and the
# signal with it.
label='the run whose listener answers nothing'
: >"$unserved_file"
: >"${unserved_file}.asked"
DEV_STACK_TEST_UNANSWERED=listener:stays run_instance 1 "$log1" "$psql_calls1" "$curl_calls1"
for _ in $(seq 1 150); do
  [[ -s "${unserved_file}.asked" ]] && break
  sleep 0.1
done
[[ -s "${unserved_file}.asked" ]] || fail "$label never sent it a health check, so the row tested nothing"
silent_work_dir="$(work_dir_of "$log1")"
kill -TERM "${instance_pid[1]}"
wait_for_exit 1 15 "$label"
[[ "$exited_status" -eq 143 ]] || fail "$label exited $exited_status on SIGTERM, not 143"
grep -q 'dev-stack: ready\.' "$log1" && fail "$label reported ready"
collect_started "$silent_work_dir"
[[ -n "${server_pid[listener]:-}" ]] || fail "$label started no listener to stop"
assert_everything_stopped "$label"
assert_databases_dropped "$label" "$psql_calls1" "$(suffix_of "$silent_work_dir")"
: >"$unserved_file"
: >"${unserved_file}.asked"
printf 'PASS: SIGTERM to a run whose health check was outstanding at a listener that answers nothing stopped it with 143 inside 15 s, every process stopped and both databases dropped\n'

# (e9) A process of the stack that ignores SIGTERM, as a server stuck in its shutdown does: the stop
# gives each process 10 s after its SIGTERM, then SIGKILL, saying so, and still drops both
# databases, inside 20 s here. Before, the stop waited on that process for good, with its
# databases, and ignored every further INT and TERM while it waited. The same row takes the
# SIGHUP a terminal sends the group as it closes, here to the run alone, while the stop runs: it
# is sent once natsd is gone and before either drop, which the row checks, so it lands inside the
# stop's 10 s on the issuer, and the stop goes on. Before, HUP kept its default action there and
# ended the run between the kills and the drops, with 129.
label='the run whose issuer ignores SIGTERM'
DEV_STACK_TEST_IGNORES_TERM=dev-oidc-issuer run_instance 1 "$log1" "$psql_calls1" "$curl_calls1"
assert_instance_ready "$label" "$log1" "$psql_calls1" "$curl_calls1"
stubborn_issuer="${process_port[dev-oidc-issuer]}"
stopped_natsd="${process_port[natsd]}"
interrupt 1 "$label" "$log1"
for _ in $(seq 1 50); do
  kill -0 "$stopped_natsd" 2>/dev/null || break
  sleep 0.1
done
kill -0 "$stopped_natsd" 2>/dev/null && fail "$label had not stopped natsd 5 s after its INT"
[[ -z "$(dropped_databases "$psql_calls1")" ]] ||
  fail "$label had dropped its databases before the SIGHUP, so the row tested nothing"
kill -HUP "${instance_pid[1]}"
wait_for_exit 1 20 "$label"
[[ "$exited_status" -eq 130 ]] || fail "$label exited $exited_status, not the 130 of the INT that began its stop"
grep -qxF "dev-stack: dev-oidc-issuer (pid ${stubborn_issuer}) did not stop within 10 s of its SIGTERM; killing it" "$log1" ||
  fail "$label did not say it killed the issuer: $(grep 'did not stop' "$log1" || true)"
assert_everything_stopped "$label"
assert_databases_dropped "$label" "$psql_calls1" "$run_suffix"
printf 'PASS: an issuer that ignores SIGTERM was killed 10 s into the stop, which said so, went on through a SIGHUP and dropped both databases\n'

# (e10) The stop signals only a process this run started that is still running. A process of the
# stack that exited early (a crash, or the second broker callout-check.sh kills) is reaped, and the
# kernel can give its pid to another process within minutes on a busy box; a stop that went by
# kill -0 sent that process SIGTERM, and 10 s later SIGKILL. No test can make the kernel reuse a
# pid, so the stop's own helpers are read out of the driver, as (p) reads its bash gate, and run in
# a shell of their own: one handed the pid of a child of that shell stops it; one handed the pid of
# a child that has exited and been reaped signals nothing and prints nothing; and one handed, in
# place of that child, the pid of a process that shell never started leaves it running, signals it
# nothing and returns at once.
stop_helpers="$(sed -n -e '/^running_child() {$/,/^}$/p' -e '/^stop_bounded() {$/,/^}$/p' \
  -e '/^stop_process() {$/,/^}$/p' "$driver")"
for helper in running_child stop_bounded stop_process; do
  grep -q "^${helper}() {\$" <<<"$stop_helpers" || fail "dev-stack.sh defines no ${helper}() at the start of a line"
done
stop_out="${temporary_dir}/stop-helpers.out"
# A process this test's shells did not start: its own shell exits at once, so it belongs to no one
# here, as a process that took a reused pid belongs to no one in the run.
stranger="$(bash -c 'sleep 30 >/dev/null 2>&1 & echo $!')"
stop_status=0
# shellcheck disable=SC2016 # the helpers' shell expands these, not this one
timeout 15 bash -c "$stop_helpers"$'\n''
sleep 30 &
child=$!
stop_bounded child "$child"
kill -0 "$child" 2>/dev/null && echo "a running child outlived its stop"
sleep 0.1 &
early=$!
sleep 0.5
stop_bounded "a process that exited early" "$early"
stop_process "$early"
reused="$1"
stop_bounded "a process that exited early" "$reused"
stop_process "$reused"
echo returned' _ "$stranger" >"$stop_out" 2>&1 || stop_status=$?
stranger_alive=no
kill -0 "$stranger" 2>/dev/null && stranger_alive=yes
kill "$stranger" 2>/dev/null || true
[[ "$stop_status" -eq 0 ]] || fail "the stop's helpers did not return within 15 s (status $stop_status): $(cat "$stop_out")"
[[ "$stranger_alive" == yes ]] ||
  fail "the stop signalled pid $stranger, a process the run never started, in place of one that exited: $(cat "$stop_out")"
[[ "$(cat "$stop_out")" == returned ]] || fail "the stop's helpers printed more than they should: $(cat "$stop_out")"
printf 'PASS: the stop stopped a running child and left alone pid %s, which a process it never started held in place of one that exited\n' "$stranger"

# (f) No database URL at all: the refusal names all three sources and exits 2, before anything is
# created.
refusal="${temporary_dir}/refusal.log"
refusal_status=0
env -u LEGION_TEST_PG_DSN -u DATABASE_URL PATH="${fake_bin_dir}:${PATH}" TMPDIR="$temporary_dir" \
  DEV_STACK_TEST_PSQL_CALL_FILE="${temporary_dir}/psql-calls-refusal" \
  DEV_STACK_TEST_CURL_CALL_FILE="${temporary_dir}/curl-calls-refusal" \
  bash "$driver" >"$refusal" 2>&1 || refusal_status=$?
[[ "$refusal_status" -eq 2 ]] || fail "a run with no database URL exited $refusal_status, not 2"
for source in --database-url LEGION_TEST_PG_DSN DATABASE_URL scripts/dev-postgres.sh; do
  grep -q -- "$source" "$refusal" || fail "the refusal does not name $source"
done
[[ ! -e "${temporary_dir}/psql-calls-refusal" ]] || fail "a run with no database URL still called psql"
printf 'PASS: a run with no database URL names --database-url, LEGION_TEST_PG_DSN, DATABASE_URL and dev-postgres.sh, and exits 2\n'

# (h) Prebuilt binaries, as the builds farm makes them for a pod that compiles no Go: the fake go
# makes a stub of each binary in a directory of its own, and a run given that directory with
# --binaries calls no go at all, puts a copy of each in its own bin directory, and runs those.
# Every check a built run passes, (a) to (c), holds for it, and so does the stop of (e). The
# directory holds the eight binaries the header and the usage name, as one built to them does:
# dev-airlock-stands is --project-access's alone, (s) below.
prebuilt="${temporary_dir}/prebuilt"
# One list of the stack's binaries and their packages; the binary names the copy check reads come
# from it. The builds farm names each binary as dev-stack.sh does, so a package whose directory is
# not its binary's name is renamed after the fake build.
prebuilt_entries=(broker=./cmd/cipherd dev-oidc-issuer=./cmd/dev-oidc-issuer dispatch=./cmd/dispatch
  listener=./cmd/listener agent-secrets=./cmd/cipher agent-secrets-helper=./cmd/cipher-helper
  natsd=./internal/testnats/cmd/natsd natsprobe=./cmd/natsprobe)
prebuilt_packages=("${prebuilt_entries[@]#*=}")
prebuilt_binaries=("${prebuilt_entries[@]%%=*}")
env -u DEV_STACK_TEST_BROKER_FAILS -u DEV_STACK_TEST_GO_CALL_FILE "${fake_bin_dir}/go" build -o "${prebuilt}/" \
  "${prebuilt_packages[@]}"
for entry in "${prebuilt_entries[@]}"; do
  built="${entry#*=}"
  [[ "${built##*/}" == "${entry%%=*}" ]] || mv "${prebuilt}/${built##*/}" "${prebuilt}/${entry%%=*}"
done
go_calls_prebuilt="${temporary_dir}/go-calls-prebuilt"
DEV_STACK_TEST_GO_CALL_FILE="$go_calls_prebuilt" run_instance 1 "$log1" "$psql_calls1" "$curl_calls1" --binaries "$prebuilt"
assert_instance_ready 'the --binaries run' "$log1" "$psql_calls1" "$curl_calls1"
[[ ! -s "$go_calls_prebuilt" ]] || fail "the --binaries run called go: $(cat "$go_calls_prebuilt")"
for binary in "${prebuilt_binaries[@]}"; do
  [[ -f "${work_dir}/bin/${binary}" && -x "${work_dir}/bin/${binary}" ]] ||
    fail "the --binaries run put no executable $binary in its own bin directory"
  cmp -s "${prebuilt}/${binary}" "${work_dir}/bin/${binary}" ||
    fail "the --binaries run's own $binary is not the prebuilt one it was given"
done
interrupt 1 'the --binaries run' "$log1"
wait_for_exit 1 15 'the --binaries run'
[[ "$exited_status" -eq 130 ]] || fail "the --binaries run exited $exited_status on INT, not 130"
assert_everything_stopped 'the --binaries run'
assert_databases_dropped 'the --binaries run' "$psql_calls1" "$run_suffix"
printf 'PASS: --binaries called no go, copied the %d prebuilt stubs into its own bin directory (%s/bin) and ran them\n' "${#prebuilt_packages[@]}" "$work_dir"

# (q) --project-access, and (r) the port race it takes: lib/dev-stack-test-project-access.sh's.
project_access_run_cases

# (i) A --binaries directory that does not hold each of those executable files is refused with
# exit 2, naming each file that is missing or not executable and no other, before anything is
# created or started: no work directory, and no psql, curl or go call. refuse_run is that check
# for any refusal: the status it wants, and dev-stack.sh's arguments after its --database-url;
# DEV_STACK_TEST_PATH, when set, is the run's whole PATH, as for run_instance.
work_dirs_made() { compgen -G "${temporary_dir}/dev-stack.*" | wc -l || true; }
refuse_run() { # <label> <status> <dev-stack.sh arguments...>
  local label="$1" want="$2" made_before tool
  shift 2
  made_before="$(work_dirs_made)"
  for tool in psql curl go; do
    rm -f "${temporary_dir}/${tool}-calls-binaries"
  done
  refusal_status=0
  env -u LEGION_TEST_PG_DSN -u DATABASE_URL PATH="${DEV_STACK_TEST_PATH:-${fake_bin_dir}:${PATH}}" TMPDIR="$temporary_dir" \
    DEV_STACK_TEST_PSQL_CALL_FILE="${temporary_dir}/psql-calls-binaries" \
    DEV_STACK_TEST_CURL_CALL_FILE="${temporary_dir}/curl-calls-binaries" \
    DEV_STACK_TEST_GO_CALL_FILE="${temporary_dir}/go-calls-binaries" \
    bash "$driver" --database-url "$admin_url" "$@" >"$refusal" 2>&1 || refusal_status=$?
  [[ "$refusal_status" -eq "$want" ]] || fail "$label exited $refusal_status, not $want: $(cat "$refusal")"
  [[ "$(work_dirs_made)" -eq "$made_before" ]] || fail "$label made a work directory before it refused"
  for tool in psql curl go; do
    [[ ! -e "${temporary_dir}/${tool}-calls-binaries" ]] || fail "$label still called $tool"
  done
}
refuse_binaries() { # <label> <dir> [more dev-stack.sh arguments...]
  refuse_run "$1" 2 --binaries "$2" "${@:3}"
}
# The refusal's lines that name a file of dir are exactly the lines given, in the order given.
refused_files_are() {
  local label="$1" dir="$2" named want
  shift 2
  named="$(grep -F "  ${dir}/" "$refusal" || true)"
  want="$(printf '%s\n' "$@")"
  [[ "$named" == "$want" ]] || fail "$label named '$named', not '$want'"
}

no_natsd="${temporary_dir}/prebuilt-no-natsd"
cp -R "$prebuilt" "$no_natsd"
rm "${no_natsd}/natsd"
refuse_binaries 'a --binaries directory with no natsd' "$no_natsd"
refused_files_are 'a --binaries directory with no natsd' "$no_natsd" "  ${no_natsd}/natsd: missing"
printf 'PASS: a --binaries directory with no natsd is refused with exit 2, naming %s/natsd, before anything is created or started\n' "$no_natsd"

listener_unrunnable="${temporary_dir}/prebuilt-listener-not-executable"
cp -R "$prebuilt" "$listener_unrunnable"
chmod -x "${listener_unrunnable}/listener"
refuse_binaries 'a --binaries directory whose listener is not executable' "$listener_unrunnable"
refused_files_are 'a --binaries directory whose listener is not executable' "$listener_unrunnable" \
  "  ${listener_unrunnable}/listener: not an executable file"
printf 'PASS: a --binaries directory whose listener is not executable is refused with exit 2, naming %s/listener\n' "$listener_unrunnable"

two_unusable="${temporary_dir}/prebuilt-two-unusable"
cp -R "$prebuilt" "$two_unusable"
chmod -x "${two_unusable}/listener"
rm "${two_unusable}/natsd"
refuse_binaries 'a --binaries directory with two unusable binaries' "$two_unusable"
refused_files_are 'a --binaries directory with two unusable binaries' "$two_unusable" \
  "  ${two_unusable}/listener: not an executable file" "  ${two_unusable}/natsd: missing"
printf 'PASS: a --binaries directory with two unusable binaries names each of them\n'

# (s) --project-access given --binaries, and given no docker on PATH:
# lib/dev-stack-test-project-access.sh's.
project_access_binaries_cases

# (t) --callout and --project-access together: both of --callout's brokers would take the airlock
# proxy's one port, so the pair is refused with exit 2 and one line naming it, before anything is
# created or started. refuse_run holds it to no work directory and no psql, curl or go call, so no
# stub is built or run; refuse_pair adds no docker call and nothing printed but that line. Both
# orders, since the driver refuses the pair once it has read every option. The controls are (q)
# and (j): each flag alone runs to ready.
pair_line="dev-stack: --callout and --project-access cannot run together: both of --callout's brokers would take the airlock proxy's one port"
pair_docker_calls="${temporary_dir}/docker-calls-pair"
refuse_pair() { # <dev-stack.sh arguments...>
  local label="a run given $*"
  rm -f "$pair_docker_calls"
  DEV_STACK_TEST_DOCKER_CALL_FILE="$pair_docker_calls" refuse_run "$label" 2 "$@"
  [[ "$(<"$refusal")" == "$pair_line" ]] ||
    fail "$label printed '$(<"$refusal")', not the one line naming the pair"
  [[ ! -e "$pair_docker_calls" ]] || fail "$label still called docker: $(cat "$pair_docker_calls")"
}
refuse_pair --callout --project-access
refuse_pair --project-access --callout
printf 'PASS: --callout and --project-access together, in either order, are refused with exit 2 and one line naming the pair, before anything is created or started\n'

# (j) --callout: natsd behind the auth callout with a monitor port, two brokers each running the
# auth service on it with the listener and Dispatch listed under their machine ids, the issuer
# writing each of those two a NATS-audience token, which each is started with, the exports
# callout-check.sh reads, and a stack that outlives its second broker. Its server URL has no query
# string, so the broker's pool_max_conns starts one (k).
run_instance 1 "$log1" "$psql_calls1" "$curl_calls1" --callout --database-url "$admin_url_no_query"
assert_instance_ready 'the --callout run' "$log1" "$psql_calls1" "$curl_calls1" ''
callout_dir="${work_dir}/nats-callout"
grep -q -- "fake natsd: args=-p -1 -store_dir ${work_dir}/nats-store -callout-keys ${callout_dir} -m -1$" "${work_dir}/logs/natsd.log" ||
  fail "the --callout run did not run natsd behind the callout with a monitor port: $(grep 'fake natsd' "${work_dir}/logs/natsd.log")"
second_broker_port="$(broker_log_port "${work_dir}/logs/broker-2.log")"
if [[ -z "$second_broker_port" ]] || ! kill -0 "$second_broker_port" 2>/dev/null; then
  fail "the --callout run started no second broker"
fi
want_services="listener/dev-stack-${run_suffix}/*=system:serviceaccount:example-ns:example-listener dispatch/dev-stack-dispatch/*=system:serviceaccount:example-ns:example-dispatch"
for broker_log in broker.log broker-2.log; do
  grep -qF "fake broker: nats=nats://127.0.0.1:${process_port[natsd]} seeds=${callout_dir}/service.seed,${callout_dir}/issuer.seed,${callout_dir}/xkey.seed nats-audience=envoy-nats services=${want_services}" \
    "${work_dir}/logs/${broker_log}" || fail "the --callout run's ${broker_log} was not given the auth service's settings: $(grep 'fake broker: nats' "${work_dir}/logs/${broker_log}")"
  grep -qF "fake broker: database=${server_url}/dev_stack_broker_${run_suffix}?pool_max_conns=10 issuer=" "${work_dir}/logs/${broker_log}" ||
    fail "the --callout run's ${broker_log} was not given its database URL with pool_max_conns=10: $(grep 'fake broker: database' "${work_dir}/logs/${broker_log}")"
  assert_pod_registrars 'the --callout run' "${work_dir}/logs/${broker_log}"
done
for token in listener-nats-token dispatch-nats-token; do
  [[ -s "${work_dir}/${token}" ]] || fail "the --callout run's issuer wrote no ${token}"
done
grep -q "fake listener: machine=dev-stack-${run_suffix} nats-token=${work_dir}/listener-nats-token$" "${work_dir}/logs/listener.log" ||
  fail "the --callout run's listener was not started with its NATS token"
grep -q "nats-token=${work_dir}/dispatch-nats-token$" "${work_dir}/logs/dispatch.log" ||
  fail "the --callout run's Dispatch was not started with its NATS token"
for pair in "DEV_STACK_CALLOUT=1" "NATS_AUTH=broker" "DEV_STACK_NATS_SERVICE_TOKEN_FILE=${work_dir}/listener-nats-token" \
  "DEV_STACK_EXCHANGE_SESSION=example-worker-session" \
  "DEV_STACK_BROKER_PIDS=\"${process_port[broker]} ${second_broker_port}\"" \
  "DEV_STACK_NATS_MONITOR_URL=http://127.0.0.1:${process_port[natsd]}"; do
  [[ "$(exported "$log1" "${pair%%=*}")" == "${pair#*=}" ]] ||
    fail "the --callout run exported ${pair%%=*}=$(exported "$log1" "${pair%%=*}"), not ${pair#*=}"
done
kill -TERM "$second_broker_port"
sleep 1
kill -0 "${instance_pid[1]}" 2>/dev/null || fail "the --callout run stopped when its second broker did"
if grep -q 'a process of this stack stopped' "$log1"; then
  fail "the --callout run stopped when its second broker did"
fi

# (l) In-place restarts, on that same stack: SIGUSR1 restarts natsd on the client and monitor ports
# it reported, with the same store and callout keys; SIGUSR2 restarts the listener on its keeper's
# socket. The old process of each is gone and the new one runs, serving the port the old one did,
# its predecessor's log is kept beside its own, the stack stays up, and each restart says so
# naming the URL every client was told.
# wait_for_line FILE TEXT waits up to 15 s for a line of FILE holding TEXT.
wait_for_line() {
  for _ in $(seq 1 150); do
    grep -qF -- "$2" "$1" 2>/dev/null && return 0
    sleep 0.1
  done
  return 1
}
# The pid a restart line names as the new process: "(pid <old> -> <new>)".
restarted_pid_in() { sed -n "s/^dev-stack: $2 restarted (pid [0-9]* -> \([0-9]*\)) .*/\1/p" "$1" | head -n1 || true; }
old_natsd="$(sed -n 's/^fake natsd: pid=\([0-9]*\)$/\1/p' "${work_dir}/logs/natsd.log" | head -n1 || true)"
[[ -n "$old_natsd" ]] || fail "the --callout run's natsd stub logged no pid"
nats_port="${process_port[natsd]}"
kill -USR1 "${instance_pid[1]}"
wait_for_line "$log1" 'dev-stack: natsd restarted' || {
  cat "$log1" >&2
  fail "the --callout run did not restart natsd on SIGUSR1"
}
new_natsd="$(restarted_pid_in "$log1" natsd)"
grep -qxF "dev-stack: natsd restarted (pid ${old_natsd} -> ${new_natsd}) nats://127.0.0.1:${nats_port}" "$log1" ||
  fail "the natsd restart did not name the old pid ${old_natsd}, the new one and the URL nats://127.0.0.1:${nats_port}: $(grep 'natsd restarted' "$log1")"
restarted_pids+=("$new_natsd")
kill -0 "$old_natsd" 2>/dev/null && fail "the natsd restart left the old natsd (pid $old_natsd) running"
kill -0 "$new_natsd" 2>/dev/null || fail "the restarted natsd (pid $new_natsd) is not running"
grep -q -- "fake natsd: args=-p ${nats_port} -store_dir ${work_dir}/nats-store -callout-keys ${callout_dir} -m ${nats_port}$" "${work_dir}/logs/natsd.log" ||
  fail "the restarted natsd was not given the old ports, store and callout keys: $(grep 'fake natsd: args' "${work_dir}/logs/natsd.log")"
grep -q -- "fake natsd: args=-p -1 " "${work_dir}/logs/natsd.log.1" ||
  fail "the natsd restart did not keep the first natsd's log as natsd.log.1"
listener_url="$(exported "$log1" ENVOY_URL)"
old_listener="${server_pid[listener]}"
kill -USR2 "${instance_pid[1]}"
wait_for_line "$log1" 'dev-stack: listener restarted' || {
  cat "$log1" >&2
  fail "the --callout run did not restart the listener on SIGUSR2"
}
new_listener="$(restarted_pid_in "$log1" listener)"
grep -qxF "dev-stack: listener restarted (pid ${old_listener} -> ${new_listener}) ${listener_url}" "$log1" ||
  fail "the listener restart did not name the old pid ${old_listener}, the new one and the URL ${listener_url}: $(grep 'listener restarted' "$log1")"
restarted_pids+=("$new_listener")
kill -0 "$old_listener" 2>/dev/null && fail "the listener restart left the old listener (pid $old_listener) running"
kill -0 "$new_listener" 2>/dev/null || fail "the restarted listener (pid $new_listener) is not running"
grep -q "fake listener: pid=${new_listener} public=http://127\.0\.0\.1:${process_port[listener]} listen-fd=3$" "${work_dir}/logs/listener.log" ||
  fail "the restarted listener was not handed its keeper's socket and port: $(grep 'fake listener: pid' "${work_dir}/logs/listener.log")"
grep -q "fake listener: pid=${old_listener} " "${work_dir}/logs/listener.log.1" ||
  fail "the listener restart did not keep the first listener's log as listener.log.1"
kill -0 "${instance_pid[1]}" 2>/dev/null || fail "the --callout run stopped across its restarts"
if grep -q 'a process of this stack stopped' "$log1"; then
  fail "the --callout run stopped across its restarts"
fi
interrupt 1 'the --callout run' "$log1"
wait_for_exit 1 15 'the --callout run'
[[ "$exited_status" -eq 130 ]] || fail "the --callout run exited $exited_status on INT, not 130"
assert_everything_stopped 'the --callout run'
assert_databases_dropped 'the --callout run' "$psql_calls1" "$run_suffix"
printf 'PASS: --callout ran natsd behind the callout, two brokers running the auth service, the listener and Dispatch with NATS tokens, exported what callout-check.sh reads, and outlived its second broker\n'
printf 'PASS: SIGUSR1 restarted natsd (pid %s -> %s) on port %s with its store and callout keys, and SIGUSR2 the listener (pid %s -> %s) on its kept port, the stack up throughout\n' \
  "$old_natsd" "$new_natsd" "$nats_port" "$old_listener" "$new_listener"

# (m) The control for (l): a natsd that comes back on another port than it served is refused,
# naming both, and the whole stack stops and drops its databases, since every client was told the
# old port. The stub baked with DEV_STACK_TEST_NATSD_MOVES reports a port of its own choice.
DEV_STACK_TEST_NATSD_MOVES=1 run_instance 1 "$log1" "$psql_calls1" "$curl_calls1"
assert_instance_ready 'the run whose natsd moves' "$log1" "$psql_calls1" "$curl_calls1"
nats_port="${process_port[natsd]}"
kill -USR1 "${instance_pid[1]}"
wait_for_exit 1 15 'a run whose natsd came back on another port'
[[ "$exited_status" -ne 0 ]] || fail "a run whose natsd came back on another port reported success"
grep -Eq "^dev-stack: natsd came back on port [0-9]+ \(monitor none\), not port ${nats_port} \(monitor none\) it served before the restart; stopping$" "$log1" || {
  cat "$log1" >&2
  fail "a run whose natsd came back on another port did not name both ports"
}
if grep -q 'dev-stack: natsd restarted' "$log1"; then
  fail "a run whose natsd came back on another port reported the restart done"
fi
restarted_pids+=("$(sed -n 's/^fake natsd: pid=\([0-9]*\)$/\1/p' "${work_dir}/logs/natsd.log" | head -n1 || true)")
assert_everything_stopped 'the run whose natsd moves'
assert_databases_dropped 'the run whose natsd moves' "$psql_calls1" "$run_suffix"
printf 'PASS: a natsd that came back on another port than %s stopped the stack, naming both ports\n' "$nats_port"

# (n) The same for the monitor port alone, under --callout: natsd comes back on its client port and
# another monitor port, which callout-check.sh's connection list is read at.
DEV_STACK_TEST_NATSD_MONITOR_MOVES=1 run_instance 1 "$log1" "$psql_calls1" "$curl_calls1" --callout
assert_instance_ready 'the run whose natsd monitor moves' "$log1" "$psql_calls1" "$curl_calls1"
nats_port="${process_port[natsd]}"
kill -USR1 "${instance_pid[1]}"
wait_for_exit 1 15 'a run whose natsd monitor came back on another port'
[[ "$exited_status" -ne 0 ]] || fail "a run whose natsd monitor came back on another port reported success"
grep -Eq "^dev-stack: natsd came back on port ${nats_port} \(monitor [0-9]+\), not port ${nats_port} \(monitor ${nats_port}\) it served before the restart; stopping$" "$log1" || {
  cat "$log1" >&2
  fail "a run whose natsd monitor came back on another port did not name both monitor ports"
}
grep -q 'dev-stack: natsd restarted' "$log1" && fail "a run whose natsd monitor moved reported the restart done"
restarted_pids+=("$(sed -n 's/^fake natsd: pid=\([0-9]*\)$/\1/p' "${work_dir}/logs/natsd.log" | head -n1 || true)")
assert_everything_stopped 'the run whose natsd monitor moves'
assert_databases_dropped 'the run whose natsd monitor moves' "$psql_calls1" "$run_suffix"
printf 'PASS: a natsd whose monitor came back on another port than %s stopped the stack, naming both\n' "$nats_port"

# (o) A listener that comes back on another port than its keeper's is refused, naming both, and
# the stack stops: every session was told the kept one.
DEV_STACK_TEST_LISTENER_MOVES=1 run_instance 1 "$log1" "$psql_calls1" "$curl_calls1"
assert_instance_ready 'the run whose listener moves' "$log1" "$psql_calls1" "$curl_calls1"
listener_url="$(exported "$log1" ENVOY_URL)"
kill -USR2 "${instance_pid[1]}"
wait_for_exit 1 15 'a run whose listener came back on another port'
[[ "$exited_status" -ne 0 ]] || fail "a run whose listener came back on another port reported success"
grep -Eq "^dev-stack: the listener came back at http://127\.0\.0\.1:[0-9]+, not ${listener_url} it served before the restart; stopping$" "$log1" || {
  cat "$log1" >&2
  fail "a run whose listener came back on another port did not name both addresses"
}
grep -q 'dev-stack: listener restarted' "$log1" && fail "a run whose listener moved reported the restart done"
restarted_pids+=("$(sed -n 's/^fake listener: pid=\([0-9]*\) .*/\1/p' "${work_dir}/logs/listener.log" | head -n1 || true)")
assert_everything_stopped 'the run whose listener moves'
assert_databases_dropped 'the run whose listener moves' "$psql_calls1" "$run_suffix"
printf 'PASS: a listener that came back at another address than %s stopped the stack, naming both\n' "$listener_url"

# (p) The bash the driver needs: its last wait (`wait -n -p`) is bash 5.1's, and under an older
# bash the loop around it spins forever, so the driver's first command refuses one
# (`refuse_old_bash`, handed this bash's own BASH_VERSINFO). No older bash is on a runner, so the
# gate is read out of the driver and run under this test's bash with each version's numbers: 4.4
# and 5.0 are refused with exit 2 and the one line, 5.1 and 5.3 taken with nothing printed. And
# the call stands before the driver's first other command and its one `wait -n -p`.
gate="$(sed -n '/^refuse_old_bash() {$/,/^}$/p' "$driver")"
[[ -n "$gate" ]] || fail "dev-stack.sh defines no refuse_old_bash() at the start of a line"
gate_out="${temporary_dir}/bash-gate.out"
for gate_case in "4 4 4.4.23(1)-release refused" "5 0 5.0.18(1)-release refused" \
  "5 1 5.1.16(1)-release taken" "5 3 5.3.0(1)-release taken"; do
  read -r major minor version verdict <<<"$gate_case"
  gate_status=0
  (eval "$gate" && refuse_old_bash "$major" "$minor" "$version") >"$gate_out" 2>&1 || gate_status=$?
  if [[ "$verdict" == refused ]]; then
    [[ "$gate_status" -eq 2 ]] || fail "bash $version: the gate exited $gate_status, not 2"
    [[ "$(<"$gate_out")" == "dev-stack: needs bash 5.1 or newer (for wait -n -p), and this is bash $version" ]] ||
      fail "bash $version: the gate printed $(<"$gate_out"), not the one line naming 5.1 and $version"
  else
    [[ "$gate_status" -eq 0 && ! -s "$gate_out" ]] ||
      fail "bash $version: the gate exited $gate_status and printed '$(<"$gate_out")', not 0 and nothing"
  fi
done
# shellcheck disable=SC2016 # the call as the driver writes it, its expansions unexpanded
gate_call='refuse_old_bash "${BASH_VERSINFO[0]}" "${BASH_VERSINFO[1]}" "$BASH_VERSION"'
call_line="$(grep -n -x -F -- "$gate_call" "$driver" | head -n1 | cut -d: -f1 || true)"
first_command_line="$(grep -n -E '^SCRIPT_DIR=' "$driver" | head -n1 | cut -d: -f1 || true)"
wait_line="$(grep -n -E '^[[:space:]]*wait -n -p ' "$driver" | head -n1 | cut -d: -f1 || true)"
[[ -n "$call_line" && -n "$first_command_line" && -n "$wait_line" ]] ||
  fail "dev-stack.sh lacks the gate's call ($call_line), SCRIPT_DIR= ($first_command_line) or its wait -n -p ($wait_line)"
((call_line < first_command_line && call_line < wait_line)) ||
  fail "dev-stack.sh calls its bash gate at line $call_line, after its first command ($first_command_line) or its wait -n -p ($wait_line)"
printf 'PASS: bash 4.4 and 5.0 are refused with exit 2 and one line naming 5.1, 5.1 and 5.3 are taken, and the gate runs before the first command\n'

printf 'ALL PASS: dev-stack.test.sh\n'
