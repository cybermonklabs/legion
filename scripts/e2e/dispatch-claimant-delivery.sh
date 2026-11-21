#!/usr/bin/env bash
# The live proof that Dispatch's outbox publishes a person's answer, comment or untargeted message
# on an issue a session has claimed to that session's own topic, `notifications.agent.<session>`
# (addClaimantRoute, packages/envoy/internal/dispatch/outbox/publisher.go). It runs the real
# envoy-dispatch binary against a PostgreSQL the caller names, a NATS server of the run's own
# (internal/testnats/cmd/natsd) and the e2e harness's fake Envoy listener
# (packages/dispatch/e2e/fake-envoy.ts), drives it over HTTP as three sessions and one person, and
# reads which `notifications.agent.*` topics carried each event's envelope, `dispatch-<event id>`
# (dispatch-claimant-delivery.ts beside this script). The mention cases also prove that a person's
# new comment mentioning a session skips that session's own copy only when the mention's send
# landed (attempt.leftToMention): the fake listener refuses the send to make it fail. Each case
# prints `== <case>`, what it read, and `ok <case>` or `FAIL <case>`; every case runs, and any FAIL
# exits 1.
#
#   DATABASE_URL=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     [DISPATCH_BIN=<envoy-dispatch>] bash scripts/e2e/dispatch-claimant-delivery.sh
#
# DATABASE_URL is required and must name a loopback host: the run makes a database of its own on
# that server, which the server migrates, and hands Dispatch that database alone, never the one
# DATABASE_URL names, as the URI lib/loopback-dsn.sh's exec_run_database sets in Dispatch's own
# environment. A run that passes drops it; one that fails keeps it for inspection and names it.
# DISPATCH_BIN names a prebuilt envoy-dispatch (the negative control runs one built from the base
# commit); unset, the script builds ./cmd/dispatch from this checkout. The ports are
# CLAIMANT_E2E_DISPATCH_PORT (19051) and CLAIMANT_E2E_ENVOY_PORT (19053), each refused when
# something already answers on it; NATS listens on a port the kernel picks. The work directory, under
# TMPDIR, holds the binaries and every process's log: removed when the run passes, kept and printed
# when it fails.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
started=$SECONDS
check=setup

begin() { check=$1; echo "== $check"; }
note() { echo "   $*"; }
pass() { echo "ok $check"; }
fail() { echo "FAIL $check: $*" >&2; exit 1; }
# What lib/rig.sh reads of its caller: start_process and wait_for log under $evidence/logs and
# $work, the work directory's once the build makes it.
evidence=''
timeout_hook=''
# shellcheck source-path=SCRIPTDIR source=lib/rig.sh
. "$root/scripts/e2e/lib/rig.sh"
# Twice the longest run on record, rounded up to the minute. 27 green e2e.yaml jobs (7 of them in
# green runs) up to 2026-11-18 took at most 190 s (run 44096585513), before every case read its
# topics off a span; with that, the two mention cases that need a topic hold 35 s each where they
# stopped at the topic, and a builds Job at e976062e with that change (d2-claimant, no busy loops,
# its envoy-dispatch and natsd built cold) passed in 221 s.
proof_guard 480

# DISPATCH_BIN is this script's input, read before the sweep below drops every DISPATCH_* variable.
dispatch_bin=${DISPATCH_BIN:-}
database_url=${DATABASE_URL:-}
dispatch_port=${CLAIMANT_E2E_DISPATCH_PORT:-19051}
envoy_port=${CLAIMANT_E2E_ENVOY_PORT:-19053}

# A Legion pane exports the real listener (ENVOY_URL), the real Dispatch (DISPATCH_URL,
# DISPATCH_TOKEN_FILE) and an nkey for the real NATS; nothing this run starts inherits any of it.
# PGHOSTADDR, PGSERVICE and PGSERVICEFILE go with them: require_loopback_dsn in
# scripts/e2e/lib/loopback-dsn.sh says why a caller of the loopback guard clears the three.
mapfile -t inherited < <(compgen -e)
for name in "${inherited[@]}"; do
  case "$name" in
    DISPATCH_* | ENVOY_* | NATS_* | PGHOSTADDR | PGSERVICE | PGSERVICEFILE) unset "$name" ;;
  esac
done

# The work directory is made once the prerequisites hold, so a refused run leaves nothing behind.
work=''
# The run's own database on DATABASE_URL's server, once made.
database=''
nats_pid=''
fake_envoy_pid=''
dispatch_pid=''

# server_psql ARGS… runs psql on DATABASE_URL's server, connected to its `postgres` database, to
# make and drop the run's own database. lib/loopback-dsn.sh's exec_run_database hands psql the
# connection in libpq's own variables (--libpq-env), PGPASSWORD among them, so no argument list
# holds the password, as stage1-skeleton.sh's server_psql does. It runs in a subshell of its own,
# which the exec replaces.
server_psql() {
  (exec_run_database DATABASE_URL "$database_url" postgres \
    "this run creates and drops a database there" --libpq-env -- psql -X "$@")
}

# cleanup stops what the run started and exits with rc: the script's own status for EXIT, 128 plus
# the signal's number for INT and TERM, so an interrupted run never reports success.
cleanup() {
  local rc=$1
  trap - EXIT INT TERM
  set +e
  stop_pid "$dispatch_pid"
  stop_pid "$fake_envoy_pid"
  stop_pid "$nats_pid"
  if [ -n "$database" ] && [ "$rc" -eq 0 ]; then
    server_psql -q -c "drop database if exists \"$database\" with (force)" >/dev/null 2>&1 ||
      echo "could not drop the run's database $database; drop it by hand" >&2
  elif [ -n "$database" ]; then
    echo "the run's database $database is kept on DATABASE_URL's server for inspection" >&2
  fi
  if [ -z "$work" ]; then
    echo "dispatch claimant delivery e2e: FAIL at $check after $((SECONDS - started))s" >&2
  elif [ "$rc" -eq 0 ]; then
    rm -rf "$work"
    echo "dispatch claimant delivery e2e: PASS in $((SECONDS - started))s"
  else
    rm -rf "${work:?}/bin" "${work:?}/nats-store"
    echo "dispatch claimant delivery e2e: FAIL at $check after $((SECONDS - started))s (logs: $work)" >&2
  fi
  exit "$rc"
}
trap 'cleanup "$?"' EXIT
trap 'cleanup 130' INT
trap 'cleanup 143' TERM

begin prerequisites
[ -n "$database_url" ] ||
  fail "DATABASE_URL is required: it names a loopback PostgreSQL server this run makes a database of its own on (postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable)"
for tool in go bun curl psql openssl python3; do
  command -v "$tool" >/dev/null || fail "$tool is not on PATH"
done
case "$database_url" in
  postgres://* | postgresql://*) ;;
  *) fail "DATABASE_URL must be a postgres:// or postgresql:// URL" ;;
esac
require_loopback_dsn db_host DATABASE_URL "$database_url" "this run creates a database there" || fail "$db_host"
if [ -n "$dispatch_bin" ]; then
  [ -x "$dispatch_bin" ] || fail "DISPATCH_BIN=$dispatch_bin is not an executable file"
fi
for port in "$dispatch_port" "$envoy_port"; do
  if ! [[ "$port" =~ ^[1-9][0-9]{0,4}$ ]] || [ "$port" -gt 65535 ]; then
    fail "'$port' is not a port"
  fi
  port_answers "$port" && fail "something already answers on 127.0.0.1:$port"
done
[ "$dispatch_port" != "$envoy_port" ] || fail "the Dispatch and fake Envoy ports are both $dispatch_port"
note "database host $db_host (URL not printed)"
pass

begin build
work=$(mktemp -d "${TMPDIR:-/tmp}/dispatch-claimant-delivery.XXXXXXXX")
evidence=$work
note "work directory $work"
mkdir -p "$work/bin" "$work/web-dist" "$work/logs"
if [ -z "$dispatch_bin" ]; then
  dispatch_bin=$work/bin/envoy-dispatch
  go -C "$root/packages/envoy" build -o "$dispatch_bin" ./cmd/dispatch || fail "./cmd/dispatch did not build"
  note "built envoy-dispatch from $root/packages/envoy"
else
  note "envoy-dispatch is DISPATCH_BIN=$dispatch_bin"
fi
go -C "$root/packages/envoy" build -o "$work/bin/natsd" ./internal/testnats/cmd/natsd ||
  fail "./internal/testnats/cmd/natsd did not build"
pass

begin database
# Empty, migrations included: the server migrates the new database at boot, so a base binary and
# this checkout's each start from a database of their own migrations, and no run writes the
# database DATABASE_URL names.
run_db=dispatch_claimant_delivery_$$_$(od -An -N3 -tx1 /dev/urandom | tr -d ' \n')
refusal='' # require_run_database assigns it with printf -v: the refusal line, or nothing
require_run_database refusal DATABASE_URL "$database_url" "$run_db" \
  "this run creates a database there" --uri || fail "$refusal"
require_run_database refusal DATABASE_URL "$database_url" postgres \
  "this run creates and drops a database there" --libpq-env || fail "$refusal"
server_psql -q -v ON_ERROR_STOP=1 -c "create database \"$run_db\"" >/dev/null ||
  fail "could not create database $run_db on DATABASE_URL's server"
database=$run_db
note "the run's database is $database"
pass

begin nats
# The port is the kernel's pick (`-p -1`), read back from the line the server logs once it listens.
start_process nats "$work/bin/natsd" -p -1 -store_dir "$work/nats-store"
wait_for "natsd to be ready" serving nats "$nats_pid" grep -q 'Server is ready' "$work/logs/nats.log"
nats_port=$(sed -n 's/.*Listening for client connections on .*:\([0-9][0-9]*\)$/\1/p' "$work/logs/nats.log" | sed -n 1p)
[ -n "$nats_port" ] || fail "natsd logged no client port; see $work/logs/nats.log"
nats_url=nats://127.0.0.1:$nats_port
note "JetStream on $nats_url"
pass

begin fake-envoy
FAKE_ENVOY_PORT=$envoy_port start_process fake_envoy bun "$root/packages/dispatch/e2e/fake-envoy.ts"
wait_for "the fake Envoy listener to answer /healthz" serving fake_envoy "$fake_envoy_pid" \
  curl -fsS --max-time 2 "http://127.0.0.1:$envoy_port/healthz"
note "fake Envoy listener on http://127.0.0.1:$envoy_port"
pass

begin dispatch
# The server gets this environment and nothing else. Identity is the production cookie, minted at
# the dev sign-in route (DISPATCH_DEV_SIGNIN=1) with a signing key the process generates for itself;
# HOME and the XDG directories name nothing, so no envoy.json, data directory or AWS profile of the
# caller's is read. Sessions authenticate with a token minted for this run. DATABASE_URL is the
# run's own database as a URI, which exec_run_database sets as the one variable of the server's
# environment (--env-only, the `env -i` this start had), and `env` names the rest.
agent_token=$(openssl rand -hex 24)
dispatch_url=http://127.0.0.1:$dispatch_port
start_process dispatch exec_run_database DATABASE_URL "$database_url" "$run_db" \
  "this run creates a database there" --uri --env-only DATABASE_URL -- env \
  PATH="$PATH" \
  TMPDIR="$work" \
  HOME=/nonexistent \
  XDG_CACHE_HOME=/nonexistent \
  XDG_CONFIG_HOME=/nonexistent \
  XDG_DATA_HOME=/nonexistent \
  DISPATCH_AGENT_TOKEN="$agent_token" \
  DISPATCH_DEV_SIGNIN=1 \
  DISPATCH_IDENTITY=cookie \
  DISPATCH_INSECURE_COOKIE=1 \
  DISPATCH_LISTEN_HOST=127.0.0.1 \
  DISPATCH_PORT="$dispatch_port" \
  DISPATCH_SERVER_URL="$dispatch_url" \
  DISPATCH_WEB_DIST="$work/web-dist" \
  ENVOY_URL="http://127.0.0.1:$envoy_port" \
  NATS_URLS="$nats_url" \
  "$dispatch_bin"
wait_for "envoy-dispatch to answer /healthz" serving dispatch "$dispatch_pid" \
  curl -fsS --max-time 2 "$dispatch_url/healthz"
note "envoy-dispatch on $dispatch_url, NATS on, outbox publishing to $nats_url"
pass

check=cases
status=0
CLAIMANT_E2E_AGENT_TOKEN=$agent_token \
  bun "$root/scripts/e2e/dispatch-claimant-delivery.ts" "$dispatch_url" "$nats_url" "http://127.0.0.1:$envoy_port" ||
  status=$?
kill -0 "$dispatch_pid" 2>/dev/null || fail "envoy-dispatch exited during the run; see $work/logs/dispatch.log"
case "$status" in
  0) ;;
  1) fail "a case failed (above)" ;;
  *) fail "the driver stopped before its cases (status $status)" ;;
esac
