#!/usr/bin/env bash
# The local-stack proof that Dispatch's live view shows an Oh My Pi session's current model and
# thinking level and switches them, on a real session. Oh My Pi runs in `--mode rpc`, as a Legion
# pane runs it, with this checkout's Envoy plugin in an isolated agent directory under the run's own
# HOME; it registers with a real Envoy listener and NATS, and a real Dispatch built from the
# checkout, with NATS on and its trusted identity header, serves the SPA this checkout builds. One
# held-open Chromium page, signed in by that header, reads and drives the header. Every value the
# page shows is compared with oracles that do not read the page: the session's own RPC answers
# (get_state, get_available_models, the command output of an `/effort` prompt), its session file,
# and Dispatch's stored messages.
#
# This script is setup and cleanup only, and its setup is one check, `setup`. The checks are
# lib/dispatch-session-settings.ts's: each prints `== <name>`, what it observed and
# `CHECK <name>: PASS`, and the first that fails prints `CHECK <name>: FAIL: <why>` and ends the run
# with exit 1. The agent-harness job's session-settings part runs it as a step
# (.github/workflows/ci-job-agent-harness.yaml). Run it from a checkout on a Linux host with go,
# bun, jq, curl, psql, tar and python3, no docker, tmux or root needed, as
#
#   LEGION_TEST_OMP=<omp> LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/dispatch-session-settings.sh
#
# Inputs, the two required ones refused when missing, naming them:
#   LEGION_TEST_PG_DSN          a loopback PostgreSQL with pgvector; the run creates a database of
#                               its own in it and drops it on any exit
#   LEGION_TEST_OMP             the Oh My Pi binary (the build .omp-pin pins, which
#                               .github/actions/install-omp installs)
#   SESSION_SETTINGS_EVIDENCE_DIR   default a fresh directory under TMPDIR; kept and printed: the
#                               transcript, every service's log, the model stand-in's requests,
#                               each Oh My Pi's stderr and log, the session files, and one
#                               screenshot per check
# Nothing else comes from outside the runner:
#   - NATS is natsd (packages/envoy/internal/testnats/cmd/natsd), built from this checkout, with
#     JetStream on a store of the run's own;
#   - every model is lib/session-settings-model.ts, a stand-in on loopback serving an
#     Anthropic-shaped provider and an OpenAI-shaped one, keyed by a placeholder of the run's own,
#     which the profile the driver writes (writeStandinProfile) names;
#   - the page is Playwright's own Chromium, `bunx playwright install chromium` in packages/dispatch,
#     which the job runs before this step.
# The page's notice for a session that sends no settings frame has no check here:
# lib/dispatch-session-settings.ts's `checks` says why no session this stack serves can show it.
#
# Everything the run starts is its own and goes on any exit, by the exact pid it was started as: the
# stand-in, natsd, the listener, Dispatch, the process holding the broken provider's port closed,
# and every Oh My Pi the driver started (it records each pid in $work/omp.pids). Its database is
# dropped and its work directory, under TMPDIR, removed. No DISPATCH_*, ENVOY_*, NATS_*, OMP*, PI_*,
# PGHOSTADDR, PGSERVICE or PGSERVICEFILE variable of the caller's shell reaches any process of
# the run: the rig talks only to its own loopback stack.
set -euo pipefail
# PGHOSTADDR, PGSERVICE and PGSERVICEFILE go with the rest: require_loopback_dsn in
# scripts/e2e/lib/loopback-dsn.sh says why a caller of the loopback guard clears the three.
for variable in $(compgen -e); do
  case $variable in
    DISPATCH_* | ENVOY_* | NATS_* | OMP* | PI_* | PGHOSTADDR | PGSERVICE | PGSERVICEFILE) unset "$variable" ;;
  esac
done

root=$(cd "$(dirname "$0")/../.." && pwd)
started=$SECONDS
evidence=${SESSION_SETTINGS_EVIDENCE_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-session-settings-evidence.XXXXXXXX")}
mkdir -p "$evidence/logs" "$evidence/checks"
# shellcheck source-path=SCRIPTDIR source=lib/transcript.sh
. "$root/scripts/e2e/lib/transcript.sh"
transcript_to "$evidence/transcript.log"

work=$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-session-settings.$$.XXXXXXXX")
# Every program the run execs from here on carries its mark (run_marks, lib/process-tree.sh): the
# driver hands it to each Oh My Pi it starts, and the listener's and Dispatch's `env -i` starts name
# it. A fork of this shell that execs nothing carries none and is stopped by its pid (run_marks'
# comment there). The transcript's tee, started above, carries none.
export LEGION_E2E_RUN=$work
driver=$root/scripts/e2e/lib/dispatch-session-settings.ts
check=setup
timeout_hook=
standin_pid=
natsd_pid=
listener_pid=
dispatch_pid=
holder_pid=
# The run has its database's name from `setup`, before it creates it, so the drop below reads
# database_created, which the `create database` sets, and never names a database that is not there.
database=
database_created=
ok=

note() { echo "   $*"; }
fail() {
  echo "CHECK $check: FAIL: $*"
  exit 1
}
# shellcheck source-path=SCRIPTDIR source=lib/rig.sh
. "$root/scripts/e2e/lib/rig.sh"
# shellcheck source-path=SCRIPTDIR source=lib/dispatch-identity.sh
. "$root/scripts/e2e/lib/dispatch-identity.sh"

# stop_omp PID ends one Oh My Pi the driver started, and only while it still carries the run's mark
# (names_dir and run_marks, lib/process-tree.sh), so a pid the kernel gave to another process since
# is never signalled: a shell that works under this run's work directory names it on its command
# line and carries no mark. It is continued first: a check stops one with SIGSTOP, and a stopped
# process keeps a TERM pending.
stop_omp() {
  names_dir "$work" "${run_marks[@]}" "$1" || return 0
  signalable "$1" || return 0
  kill -CONT "$1" 2>/dev/null || true
  stop_pid "$1"
}

cleanup() {
  local status=$? pid start
  set +e
  # Each line is a pid and its start, which the driver read as it started it (session-settings-omp.ts).
  if [ -f "$work/omp.pids" ]; then
    while read -r pid start; do
      [ -z "$start" ] || started_at[$pid]=$start
      stop_omp "$pid"
    done <"$work/omp.pids"
  fi
  stop_pid "$dispatch_pid"
  stop_pid "$listener_pid"
  stop_pid "$dispatch_identity_issuer_pid"
  stop_pid "$holder_pid"
  stop_pid "$natsd_pid"
  stop_pid "$standin_pid"
  if [ -n "$database_created" ]; then
    psql "$LEGION_TEST_PG_DSN" -X -q -v ON_ERROR_STOP=1 -c "drop database if exists \"$database\" with (force)" >/dev/null ||
      echo "could not drop database $database; drop it by hand" >&2
  fi
  rm -rf "$work"
  if [ "$status" -eq 0 ] && [ -n "$ok" ]; then
    echo "dispatch session settings e2e: PASS in $((SECONDS - started))s (evidence: $evidence)"
  else
    echo "dispatch session settings e2e: FAIL at $check after $((SECONDS - started))s (evidence: $evidence)"
  fi
}
trap cleanup EXIT
trap 'echo "CHECK $check: FAIL: line $LINENO exited $?: $BASH_COMMAND"' ERR
trap 'exit 130' INT
trap 'exit 143' TERM

# ---- inputs ------------------------------------------------------------------------------------------
echo "== setup"
[ -n "${LEGION_TEST_PG_DSN:-}" ] ||
  fail "LEGION_TEST_PG_DSN is required: a loopback PostgreSQL with pgvector (postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable) the run creates its own database in (the agent-harness job's service in CI)"
for tool in go bun jq curl psql tar python3; do
  command -v "$tool" >/dev/null || fail "$tool is required and is not on PATH"
done
# The database's name is only this shell's pid and random bytes, so the run has it here, and the
# one read of the DSN runs in this step, before the builds, the services and the `create
# database` further down: the rewrite refuses every DSN the loopback guard refuses, and a DSN
# whose values the connection string cannot carry as well. Its answer is the connection string
# the run keeps, and its refusal is the line this script fails with.
database=legion_e2e_session_settings_$$_$(od -An -N3 -tx1 /dev/urandom | tr -d ' \n')
run_database_url database_url LEGION_TEST_PG_DSN "$LEGION_TEST_PG_DSN" "$database" "the run creates and drops a database there" || fail "$database_url"
omp=${LEGION_TEST_OMP:-}
[ -n "$omp" ] ||
  fail "LEGION_TEST_OMP is required: the Oh My Pi every session runs (.github/actions/install-omp exports it in CI)"
[ -x "$omp" ] || fail "LEGION_TEST_OMP names $omp, which is not an executable file"
(cd "$root" && bun install --frozen-lockfile >"$evidence/logs/bun-install.log" 2>&1) || fail "bun install failed; see $evidence/logs/bun-install.log"
chromium=$(bun "$driver" chromium) || fail "Playwright could not name its Chromium (bun $driver chromium)"
[ -x "$chromium" ] ||
  fail "Playwright's Chromium is not installed at $chromium: run \`bunx playwright install chromium\` in packages/dispatch"
note "Oh My Pi $omp ($("$omp" --version 2>/dev/null | head -1)); Chromium $chromium"

# ---- builds ------------------------------------------------------------------------------------------
(cd "$root/packages/contracts" && bun run build >"$evidence/logs/build-contracts.log" 2>&1) || fail "@legion/contracts did not build; see $evidence/logs/build-contracts.log"
go -C "$root/packages/envoy" build -o "$work/envoy-listener" ./cmd/listener
go -C "$root/packages/envoy" build -o "$work/dispatch" ./cmd/dispatch
go -C "$root/packages/envoy" build -o "$work/natsd" ./internal/testnats/cmd/natsd
(cd "$root/packages/dispatch" && bun run build:web >"$evidence/logs/build-web.log" 2>&1) || fail "the SPA did not build; see $evidence/logs/build-web.log"
# This checkout's pi-envoy, packed as the release packs it (lib/pack-plugin.sh builds its dist) and
# unpacked as the worker image unpacks it, so `--extension` loads what a Legion pane loads.
tarball=$(bash "$root/scripts/e2e/lib/pack-plugin.sh" pi-envoy "$work/pack" 2>"$evidence/logs/pack-pi-envoy.log") ||
  fail "pi-envoy did not pack; see $evidence/logs/pack-pi-envoy.log"
mkdir -p "$work/pi-envoy"
tar xzf "$tarball" -C "$work/pi-envoy" --strip-components=1
note "pi-envoy $(jq -r .version "$work/pi-envoy/package.json") from this checkout"

# ---- services ----------------------------------------------------------------------------------------
# The model: every request must carry the run's placeholder key, which the profile names, so a turn
# the stand-in served is one the session's own provider route sent.
(umask 077 && printf 'session-settings-standin-%s\n' "$(od -An -N12 -tx1 /dev/urandom | tr -d ' \n')" >"$work/model-key")
start_process standin bun "$root/scripts/e2e/lib/session-settings-model.ts" "$work/standin.port" "$work/model-key" \
  "$evidence/logs/standin-requests.jsonl"
standin_port=
await_port_file standin "$work/standin.port" standin_port

mkdir -p "$work/nats-store"
start_process natsd "$work/natsd" -p -1 -store_dir "$work/nats-store"
await_start natsd "$natsd_pid" 60 "natsd to be ready" grep -q 'Server is ready' "$evidence/logs/natsd.log"
nats_port=$(report_port "$evidence/logs/natsd.log" 'Listening for client connections on [^ ]*:') ||
  fail "natsd logged no client port; see $evidence/logs/natsd.log"
nats_url=nats://127.0.0.1:$nats_port

psql "$LEGION_TEST_PG_DSN" -X -q -v ON_ERROR_STOP=1 -c "create database \"$database\"" >/dev/null || fail "could not create database $database in LEGION_TEST_PG_DSN"
database_created=1

(umask 077 &&
  od -An -N24 -tx1 /dev/urandom | tr -d ' \n' >"$work/envoy-token" &&
  od -An -N24 -tx1 /dev/urandom | tr -d ' \n' >"$work/dispatch-token" &&
  printf 'Authorization: Bearer %s\n' "$(cat "$work/envoy-token")" >"$work/envoy-auth-header")
listener() { curl -fsS -H "@$work/envoy-auth-header" "$@"; }
envoy_port=$(bun "$driver" free-port)
dispatch_port=$(bun "$driver" free-port)
# Neither service inherits this shell (dispatch-user-turns.sh's reasoning): a HOME or XDG directory
# holding legion/envoy.json would hand Dispatch a production configuration. Dispatch has an identity
# of its own at the listener (lib/dispatch-identity.sh), as a deployment gives it.
dispatch_identity_start
env -i PATH="$PATH" HOME=/nonexistent LEGION_E2E_RUN="$work" ENVOY_API_TOKEN="$(cat "$work/envoy-token")" PORT="$envoy_port" \
  ENVOY_LISTEN_HOST=127.0.0.1 ENVOY_MACHINE_ID="legion-e2e-session-settings-$$" NATS_URLS="$nats_url" \
  "${dispatch_identity_listener_env[@]}" "$work/envoy-listener" >"$evidence/logs/listener.log" 2>&1 &
listener_pid=$!
record_start "$listener_pid" || true
until_true 60 "the Envoy listener" listener "http://127.0.0.1:$envoy_port/v1/sessions"
env -i PATH="$PATH" HOME=/nonexistent LEGION_E2E_RUN="$work" XDG_CONFIG_HOME=/nonexistent XDG_DATA_HOME=/nonexistent \
  DATABASE_URL="$database_url" DISPATCH_AGENT_TOKEN="$(cat "$work/dispatch-token")" \
  DISPATCH_IDENTITY=header:X-Dispatch-User DISPATCH_IDENTITY_HEADER_TRUSTED=1 DISPATCH_LISTEN_HOST=127.0.0.1 DISPATCH_PORT="$dispatch_port" \
  DISPATCH_SERVER_URL="http://127.0.0.1:$dispatch_port" DISPATCH_SIGNING_KEY="$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')" \
  DISPATCH_WEB_DIST="$root/packages/dispatch/web/dist" ENVOY_URL="http://127.0.0.1:$envoy_port" \
  ENVOY_TOKEN="$(cat "$work/envoy-token")" NATS_URLS="$nats_url" "${dispatch_identity_dispatch_env[@]}" \
  "$work/dispatch" >"$evidence/logs/dispatch.log" 2>&1 &
dispatch_pid=$!
record_start "$dispatch_pid" || true
until_true 120 "Dispatch to serve, with NATS" sh -c "curl -fsS http://127.0.0.1:$dispatch_port/healthz | jq -e '.ok and .nats'"

# The broken provider's port: bound and never listened on, by a process of the run's own, so every
# connection to it is refused for the whole run and nothing else can take it.
python3 -c 'import socket, sys, time
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1], flush=True)
sys.stdout.close()
time.sleep(1e9)' >"$work/broken-port" &
holder_pid=$!
record_start "$holder_pid" || true
until_true 30 "the broken provider's port to be held" test -s "$work/broken-port"
broken_port=$(cat "$work/broken-port")

# ---- the Oh My Pi home -------------------------------------------------------------------------------
# HOME is the run's own, with .omp/natives linked to the caller's native-module cache as
# lib/omp-home.sh's make_omp_home links it, so nothing is downloaded and nothing of the caller's
# ~/.omp is written. Its agent directory holds the profile every session of the run shares, which
# the driver writes before its first check.
natives=$HOME/.omp/natives
if [ -n "${XDG_DATA_HOME:-}" ] && [ -d "$XDG_DATA_HOME/omp" ]; then natives=$XDG_DATA_HOME/omp/natives; fi
mkdir -p "$natives" "$work/home/.omp/agent" "$work/session" "$work/tmp"
ln -sfT "$natives" "$work/home/.omp/natives"

jq -n --arg work "$work" --arg evidence "$evidence" --arg omp "$omp" \
  --arg dispatch "http://127.0.0.1:$dispatch_port" --arg envoy "http://127.0.0.1:$envoy_port" --arg nats "$nats_url" \
  --arg standin "http://127.0.0.1:$standin_port" --argjson broken "$broken_port" --arg plugin "$work/pi-envoy" '{
    work: $work, evidence: $evidence, omp: $omp, dispatchUrl: $dispatch, envoyUrl: $envoy,
    natsUrl: $nats, standinUrl: $standin, standinKeyFile: ($work + "/model-key"), brokenPort: $broken,
    plugin: $plugin, home: ($work + "/home"),
    agentDir: ($work + "/home/.omp/agent"), sessionCwd: ($work + "/session"), tmp: ($work + "/tmp"),
    envoyTokenFile: ($work + "/envoy-token"), dispatchTokenFile: ($work + "/dispatch-token"),
    pidFile: ($work + "/omp.pids")
  }' >"$work/rig.json"
note "model stand-in on port $standin_port, NATS $nats_url, listener port $envoy_port, Dispatch port $dispatch_port, database $database, broken provider port $broken_port"
note "setup took $((SECONDS - started))s"
echo "CHECK setup: PASS"

# ---- the checks --------------------------------------------------------------------------------------
# The driver prints a CHECK line for each of its checks, the failing one included; a driver that
# ends without one prints none, and this line names it.
check=driver
driver_status=0
bun "$driver" run "$work/rig.json" || driver_status=$?
cp "$work/home/.omp/agent/models.yml" "$work/home/.omp/agent/config.yml" "$evidence/logs/" 2>/dev/null || true
[ "$driver_status" -eq 0 ] || fail "the driver exited $driver_status (the check it failed in is named above)"
ok=1
