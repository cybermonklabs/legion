#!/usr/bin/env bash
# The eval registry's measurement (dispatch://INFRA-2916, T15: k2, k8, k13, k16) over a built
# envoy-dispatch and a database the evalbench fixture is loaded into.
#
#   run.sh migrate <database-url> <envoy-dispatch>
#   run.sh measure <database-url> <envoy-dispatch> <head|base>
#
# migrate boots the server once on an empty database, which migrates it, and stops it.
#
# measure boots the server on the loaded database, waits until its consumers have caught up
# (`measure wait`: the Legion facts read whole and, at this head, every family's day rows written
# through today by the running build), stops it, starts it again and measures the first paint as
# the first requests after that restart (`measure paint`). At this head it then greps the read path
# for a read of events (k13) and prints the query keys of the list route (k16). With
# EVALBENCH_PG_RESTART set it measures the first paint once more after a Postgres restart, for
# information, judging no budget.
#
# The server runs with no NATS (DISPATCH_NATS_DISABLED=1), no GitHub App and the header identity
# the Go tests use, and is reached with its agent token. Its build must be stamped
# (-ldflags "-X main.buildCommit=<commit>"): an unstamped binary is a new build at every start, and
# its restart would rebuild every family the measurement waits for. The database URL is a
# superuser's: the instrument resets pg_stat_statements and turns the statement log on and off.
#
# Environment:
#   EVALBENCH_WORK        where the server's home, its logs and the measurement's output go (required)
#   EVALBENCH_MEASURE     the built measure command (required by measure)
#   EVALBENCH_PG_LOG      Postgres's jsonlog file, with log_destination jsonlog (required by measure)
#   EVALBENCH_PORT        the port the server serves on (58111)
#   EVALBENCH_SOURCE      the checkout the server was built from, whose read path k13 greps
#   EVALBENCH_PG_RESTART  a command that restarts Postgres, for the information round
#   EVALBENCH_CATCHUP     how long the catch-up may take (1h)
#   EVALBENCH_QUIET, EVALBENCH_CATALOG   measure paint's -quiet and -catalog (30s, 65s)
#
# Every measurement prints a RESULT line, and every one that misses its budget or its rule a FAIL
# line; the script exits 1 when any step failed.
set -euo pipefail

usage() {
  echo "usage: run.sh migrate <database-url> <envoy-dispatch> | run.sh measure <database-url> <envoy-dispatch> <head|base>" >&2
  exit 2
}
[ $# -ge 3 ] || usage
command=$1 database_url=$2 server_bin=$3
mode=${4:-}
case "$command" in
  migrate) [ $# -eq 3 ] || usage ;;
  measure)
    [ $# -eq 4 ] || usage
    case "$mode" in head | base) ;; *) usage ;; esac
    : "${EVALBENCH_MEASURE:?EVALBENCH_MEASURE must name the built measure command}"
    : "${EVALBENCH_PG_LOG:?EVALBENCH_PG_LOG must name Postgres jsonlog file}"
    ;;
  *) usage ;;
esac
: "${EVALBENCH_WORK:?EVALBENCH_WORK must name a directory for the server home and the logs}"
[ -x "$server_bin" ] || {
  echo "run.sh: $server_bin is not an executable envoy-dispatch" >&2
  exit 2
}
work=$EVALBENCH_WORK
port=${EVALBENCH_PORT:-58111}
url="http://127.0.0.1:$port"
token=evalbench-token
mkdir -p "$work/home" "$work/web"
server_pid=""
status=0

# start_server boots the server with its stderr in $1 and waits until /healthz answers.
start_server() {
  local log=$1
  (cd "$work/home" && exec env -i PATH="$PATH" HOME="$work/home" XDG_CONFIG_HOME="$work/home" XDG_DATA_HOME="$work/home" \
    DATABASE_URL="$database_url" DISPATCH_AGENT_TOKEN="$token" DISPATCH_IDENTITY=header:X-Dispatch-User \
    DISPATCH_IDENTITY_HEADER_TRUSTED=1 DISPATCH_LISTEN_HOST=127.0.0.1 DISPATCH_PORT="$port" \
    DISPATCH_WEB_DIST="$work/web" DISPATCH_NATS_DISABLED=1 "$server_bin" >>"$log" 2>&1) &
  server_pid=$!
  local started=$SECONDS
  until curl -fsS "$url/healthz" >/dev/null 2>&1; do
    if ! kill -0 "$server_pid" 2>/dev/null; then
      echo "FAIL run.sh: the server exited before it served; the end of $log:"
      tail -n 40 "$log"
      exit 1
    fi
    if [ $((SECONDS - started)) -gt 900 ]; then
      echo "FAIL run.sh: the server did not serve within 900 s; the end of $log:"
      tail -n 40 "$log"
      exit 1
    fi
    sleep 1
  done
  echo "MARK $(date -u +%FT%TZ) server $server_pid serving after $((SECONDS - started)) s, logging to $log"
}

# stop_server stops the server with SIGTERM and waits for its exit, which is 0 or 143 when it
# shut down cleanly.
stop_server() {
  [ -n "$server_pid" ] || return 0
  kill -TERM "$server_pid"
  local code=0
  wait "$server_pid" || code=$?
  if [ "$code" -ne 0 ] && [ "$code" -ne 143 ]; then
    echo "FAIL run.sh: the server exited $code when stopped"
    status=1
  fi
  echo "MARK $(date -u +%FT%TZ) server $server_pid stopped ($code)"
  server_pid=""
}
trap 'if [ -n "$server_pid" ]; then kill -TERM "$server_pid" 2>/dev/null || true; fi' EXIT

if [ "$command" = migrate ]; then
  start_server "$work/server-migrate.log"
  stop_server
  exit "$status"
fi

# measure runs one subcommand of the measure command against the running server.
measure() {
  local subcommand=$1
  shift
  "$EVALBENCH_MEASURE" "$subcommand" -mode "$mode" -database "$database_url" -url "$url" "$@" || status=1
}

start_server "$work/server-catchup.log"
measure wait -timeout "${EVALBENCH_CATCHUP:-1h}"
# status is 0 until the wait, so here it is the wait's own; stop_server sets it on a bad stop.
waited=$status
stop_server
if [ "$waited" -ne 0 ]; then
  echo "FAIL run.sh: the consumers did not catch up, so no first paint is measured"
  exit 1
fi
if [ "$status" -ne 0 ]; then
  echo "FAIL run.sh: the server did not stop cleanly after the catch-up, so no first paint is measured"
  exit 1
fi

start_server "$work/server-paint.log"
measure paint -token "$token" -server-log "$work/server-paint.log" -pg-log "$EVALBENCH_PG_LOG" \
  -quiet "${EVALBENCH_QUIET:-30s}" -catalog "${EVALBENCH_CATALOG:-65s}"

if [ "$mode" = head ]; then
  # k13: no read of the first paint's path names events. The read path is the list read's
  # handler and the registry reads it calls (api/evals.go readEvalList, evals.Store.Current,
  # DayWindow, FirstDays and RefresherAge, and the folds of statistics.go).
  if [ -n "${EVALBENCH_SOURCE:-}" ]; then
    dispatch_src="$EVALBENCH_SOURCE/packages/envoy/internal/dispatch"
    read_path=("$dispatch_src/api/evals.go" "$dispatch_src/evals/reads.go" "$dispatch_src/evals/store.go"
      "$dispatch_src/evals/statistics.go" "$dispatch_src/evals/refresher.go")
    # grep exits 1 when no line matches and above 1 when it could not read a file, which would
    # leave a file of the read path unread.
    searched=0
    hits=$(grep -n -i -E '\b(from|join)[[:space:]]+events\b' "${read_path[@]}") || searched=$?
    if [ "$searched" -gt 1 ]; then
      echo "FAIL k13 grep exited $searched, so a file of the read path was not read: ${read_path[*]}"
      status=1
    fi
    echo "RESULT k13 read_path_files=${#read_path[@]} lines_reading_events=$(printf '%s' "$hits" | grep -c . || true)"
    if [ -n "$hits" ]; then
      echo "FAIL k13 the read path names events:"
      printf '%s\n' "$hits"
      status=1
    fi
  else
    echo "FAIL k13 EVALBENCH_SOURCE names no checkout, so the read path was not read"
    status=1
  fi
  # k16: the list route's query keys, from the route table's own OpenAPI document.
  if keys=$("$server_bin" openapi | python3 -c '
import json, sys
operation = json.load(sys.stdin)["paths"]["/api/v1/projects/{key}/evals"]["get"]
print(" ".join(sorted(p["name"] for p in operation.get("parameters", []) if p.get("in") == "query")))'); then
    echo "RESULT k16 list_route_query_keys=[$keys]"
    case " $keys " in
      *" repo "*) ;;
      *)
        echo "FAIL k16 the list route takes no repo"
        status=1
        ;;
    esac
  else
    echo "FAIL k16 the list route's query keys could not be read from $server_bin openapi"
    status=1
  fi
fi

if [ -n "${EVALBENCH_PG_RESTART:-}" ]; then
  stop_server
  echo "MARK $(date -u +%FT%TZ) restarting Postgres for the information round"
  bash -c "$EVALBENCH_PG_RESTART" || {
    echo "FAIL run.sh: the Postgres restart failed"
    exit 1
  }
  start_server "$work/server-cold.log"
  measure paint -info -token "$token" -server-log "$work/server-cold.log" -pg-log "$EVALBENCH_PG_LOG" \
    -quiet "${EVALBENCH_QUIET:-30s}" -catalog "${EVALBENCH_CATALOG:-65s}"
fi
stop_server
echo "MARK $(date -u +%FT%TZ) run.sh measure $mode exits $status"
exit "$status"
