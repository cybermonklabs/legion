#!/usr/bin/env bash
# The eval registry's measurement (dispatch://INFRA-2916, T15: k2, k8, k13, k16) over a built
# envoy-dispatch and a database the evalbench fixture is loaded into.
#
#   run.sh migrate <database-url> <envoy-dispatch>
#   run.sh measure <database-url> <envoy-dispatch> <head|failure-day|base|base-failure-day>
#
# migrate boots the server once on an empty database, which migrates it, and stops it.
#
# measure boots the server on the loaded database, waits until its consumers have caught up
# (`measure wait`: the Legion facts read whole and, at this head, every family's day rows served
# under the running build with every walk done and the stored figures built, and the keyword fill's
# cursor at the newest event, which the load leaves at its last event, with a RESULT line of how
# long the fill took to get there; at the base, those of these consumers the base runs), once, and
# stops it. It then starts it again and measures the first paint as the first requests after that
# start (`measure paint`), and stops it. At this head it does so EVALBENCH_PAINT_RUNS times, 8 by
# default (k8), each run a fresh start of the server, with each run's RESULT and FAIL lines tagged
# run=<n>, and then prints one line per read and per group of reads with the median and the
# maximum of the runs, failing any read over its budget in any run (`measure runs`). The rest is
# measured once. Run 1's paint is followed by k2, k13's named-jobs read and P0 list, k16's day
# rows and folds, the probe, k16's fallback, whose reads after a stale cut's first paint are held
# to the first paint's budgets, and k16's move of the default repository away and back, timed to
# the end of each refold (measure's own checks); after the runs this script greps the read path for
# a read of events (k13) and judges the query keys of the list route (k16). failure-day is this head
# over the fixture with a failure day added to it (evalbench's -failure-day), whose paint is the
# Delivery timeline's alone; base is the base's paint, and base-failure-day the base's over the same
# history with the failure day added, whose reads are the Delivery page's the head's timeline reads
# are set beside. Each is measured once unless EVALBENCH_PAINT_RUNS names more, in which case the
# runs are tagged and summarised as the head's are. With EVALBENCH_PG_RESTART set it
# measures the first paint once more after a Postgres restart, for information: it judges no
# budget, and what it misses prints as INFO-FAIL lines, which fail no step.
#
# The server runs with no NATS (DISPATCH_NATS_DISABLED=1), no GitHub App and the header identity
# the Go tests use, and is reached with its agent token. It runs under the server's supervisor
# (supervise.sh, in EVALBENCH_CONTROL), in the server's own container where the pod has one, and a
# stand-in here takes its place: the stand-in lives as long as the server, passes it the signals it
# is sent, and exits with its status. Its build must be stamped
# (-ldflags "-X main.buildCommit=<commit>"): an unstamped binary is a new build at every start, and
# its restart would rebuild every family the measurement waits for. The database URL is a
# superuser's: the instrument resets pg_stat_statements and turns the statement log on and off.
#
# Environment:
#   EVALBENCH_WORK        where the server's home, its logs and the measurement's output go (required)
#   EVALBENCH_CONTROL     the supervisors' directory, whose server/ the server runs under (required;
#                         job.sh sets it)
#   EVALBENCH_MEASURE     the built measure command (required by measure)
#   EVALBENCH_PG_LOG      Postgres's jsonlog file, with log_destination jsonlog (required by measure)
#   EVALBENCH_PORT        the port the server serves on (58111)
#   EVALBENCH_SOURCE      the checkout the server was built from, whose read path k13 greps
#   EVALBENCH_PG_RESTART  a command that restarts Postgres, for the information round
#   EVALBENCH_CATCHUP     how long the catch-up may take (1h)
#   EVALBENCH_QUIET, EVALBENCH_CATALOG   measure paint's -quiet and -catalog (30s, 65s)
#   EVALBENCH_PAINT_RUNS  how many times the first paint is measured (8 at this head; once in every
#                         other mode unless named; fewer for a smoke run)
#   EVALBENCH_NOW         the UTC midnight the loaded history ends at, measure paint's -now (job.sh
#                         sets it; the current UTC midnight where unset)
#   EVALBENCH_TEN_TIMES   1 where the fixture is at ten times the stated run rate, measure's
#                         -ten-times: a database time over a budget stated at the stated rate, and
#                         k8's base control, print RECORD lines there, not FAIL lines
#   EVALBENCH_NODE        the node the run is on, measure's -node, which its k16 lines name (job.yaml
#                         sets it from the pod's spec.nodeName)
#
# Every measurement prints a RESULT line, and every one that misses its budget or its rule a FAIL
# line, or a RECORD line for a budget no check gates at the run's rate; the script exits 1 when any
# step failed.
set -euo pipefail

usage() {
  echo "usage: run.sh migrate <database-url> <envoy-dispatch> | run.sh measure <database-url> <envoy-dispatch> <head|failure-day|base|base-failure-day>" >&2
  exit 2
}
[ $# -ge 3 ] || usage
command=$1 database_url=$2 server_bin=$3
mode=${4:-}
case "$command" in
  migrate) [ $# -eq 3 ] || usage ;;
  measure)
    [ $# -eq 4 ] || usage
    case "$mode" in head | failure-day | base | base-failure-day) ;; *) usage ;; esac
    : "${EVALBENCH_MEASURE:?EVALBENCH_MEASURE must name the built measure command}"
    : "${EVALBENCH_PG_LOG:?EVALBENCH_PG_LOG must name Postgres jsonlog file}"
    # The first paint is measured EVALBENCH_PAINT_RUNS times: 8 by default at this head (k8), once
    # in every other mode unless the count is named.
    runs=1
    [ "$mode" != head ] || runs=8
    runs=${EVALBENCH_PAINT_RUNS:-$runs}
    case "$runs" in '' | *[!0-9]* | 0*)
      echo "run.sh: EVALBENCH_PAINT_RUNS is a count from 1, not '$runs'" >&2
      exit 2
      ;;
    esac
    recorded=0
    if [ "$mode" = head ] || [ "$runs" -gt 1 ]; then
      recorded=1
    fi
    ;;
  *) usage ;;
esac
: "${EVALBENCH_WORK:?EVALBENCH_WORK must name a directory for the server home and the logs}"
: "${EVALBENCH_CONTROL:?EVALBENCH_CONTROL must name the supervisors directory (job.sh sets it)}"
[ -x "$server_bin" ] || {
  echo "run.sh: $server_bin is not an executable envoy-dispatch" >&2
  exit 2
}
here=$(cd "$(dirname "$0")" && pwd)
work=$EVALBENCH_WORK
port=${EVALBENCH_PORT:-58111}
url="http://127.0.0.1:$port"
token=evalbench-token
mkdir -p "$work/home" "$work/web"
server_pid=""
status=0

# start_server boots the server under its supervisor with its stderr in $1 and waits until
# /healthz answers. server_pid is its stand-in's (the header), which kill and wait treat as the
# server's.
start_server() {
  local log=$1
  "$here/supervise.sh" exec "$EVALBENCH_CONTROL/server" "$log" env -i -C "$work/home" PATH="$PATH" HOME="$work/home" \
    XDG_CONFIG_HOME="$work/home" XDG_DATA_HOME="$work/home" \
    DATABASE_URL="$database_url" DISPATCH_AGENT_TOKEN="$token" DISPATCH_IDENTITY=header:X-Dispatch-User \
    DISPATCH_IDENTITY_HEADER_TRUSTED=1 DISPATCH_LISTEN_HOST=127.0.0.1 DISPATCH_PORT="$port" \
    DISPATCH_WEB_DIST="$work/web" DISPATCH_NATS_DISABLED=1 "$server_bin" &
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
  echo "MARK $(date -u +%FT%TZ) server $server_pid serving after $((SECONDS - started)) s (pid $(cat "$EVALBENCH_CONTROL/server/pid" 2>/dev/null || echo gone) under its supervisor), logging to $log"
}

# stop_server stops the server with SIGTERM and waits for its exit, which is 0 or 143 when it
# shut down cleanly. A server that had exited already is a FAIL line too, not an exit of the
# script under set -e with none.
stop_server() {
  [ -n "$server_pid" ] || return 0
  if ! kill -TERM "$server_pid" 2>/dev/null; then
    echo "FAIL run.sh: the server $server_pid had exited before it was stopped"
    status=1
  fi
  local code=0
  wait "$server_pid" || code=$?
  if [ "$code" -ne 0 ] && [ "$code" -ne 143 ]; then
    echo "FAIL run.sh: the server exited $code when stopped"
    status=1
  fi
  echo "MARK $(date -u +%FT%TZ) server $server_pid stopped ($code)"
  server_pid=""
}
# Every exit stops a server still running and waits for it, so the caller's next step (job.sh's
# stop of Postgres, the next measure) never runs beside it.
trap 'if [ -n "$server_pid" ]; then kill -TERM "$server_pid" 2>/dev/null || true; wait "$server_pid" 2>/dev/null || true; fi' EXIT

if [ "$command" = migrate ]; then
  start_server "$work/server-migrate.log"
  stop_server
  exit "$status"
fi

# measure runs one subcommand of the measure command against the running server, with the day the
# loaded history ends on where job.sh names it, the node where the Job names it, and -ten-times
# where the fixture is at ten times the stated run rate.
measure() {
  local subcommand=$1 flags=()
  shift
  if [ -n "${EVALBENCH_NOW:-}" ]; then
    flags+=(-now "$EVALBENCH_NOW")
  fi
  if [ -n "${EVALBENCH_NODE:-}" ]; then
    flags+=(-node "$EVALBENCH_NODE")
  fi
  if [ "${EVALBENCH_TEN_TIMES:-0}" = 1 ]; then
    flags+=(-ten-times)
  fi
  "$EVALBENCH_MEASURE" "$subcommand" -mode "$mode" -database "$database_url" -url "$url" "${flags[@]}" "$@" || status=1
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

# The first paint: $runs times (EVALBENCH_PAINT_RUNS), each as the first requests after a fresh
# start of the server. Where the paint is one of several, or at this head, each run's lines are
# tagged run=<n> and its reads appended to paint-runs.jsonl, which `measure runs` reads once the
# runs are done; a paint measured once in another mode is not tagged.
runs_file=$work/paint-runs.jsonl
if [ "$recorded" = 1 ]; then
  : >"$runs_file"
fi
for run in $(seq 1 "$runs"); do
  paint_log=$work/server-paint.log
  tagged=()
  if [ "$recorded" = 1 ]; then
    paint_log=$work/server-paint-$run.log
    tagged=(-run "$run" -runs-file "$runs_file")
  fi
  start_server "$paint_log"
  measure paint -token "$token" -server-log "$paint_log" -pg-log "$EVALBENCH_PG_LOG" \
    -quiet "${EVALBENCH_QUIET:-30s}" -catalog "${EVALBENCH_CATALOG:-65s}" "${tagged[@]}"
  stop_server
done
if [ "$recorded" = 1 ]; then
  measure runs -runs "$runs" -runs-file "$runs_file"
fi

if [ "$mode" = head ]; then
  # k13: no read of the first paint's path names events. The read path is the list read's
  # handler, every statement it runs in its one snapshot, and the registry's folds of what they
  # read: api/evals.go (readEvalList, evalProjectsIn, readEvalValues); loadProject of
  # api/owner.go; evals.ProjectsIn, CurrentIn, FirstDaysIn and DayWindowIn of reads.go, with the
  # column lists and scans of store.go; DeployRepoIn and deployRepoCase of deploy_repos.go;
  # viaAsRead of owners.go; RefresherAgeIn of refresher.go; ReadStoredFiguresIn of figures_read.go,
  # figuresThrough of figures_refresher.go and the stored figures' keys, decode and statement cuts
  # of figures.go; the folds of statistics.go; VerdictOf of delivery_targets.go; and the
  # definition's writer of evals.go.
  if [ -n "${EVALBENCH_SOURCE:-}" ]; then
    dispatch_src="$EVALBENCH_SOURCE/packages/envoy/internal/dispatch"
    read_path=("$dispatch_src/api/evals.go" "$dispatch_src/api/owner.go" "$dispatch_src/evals/reads.go"
      "$dispatch_src/evals/store.go" "$dispatch_src/evals/deploy_repos.go" "$dispatch_src/evals/owners.go"
      "$dispatch_src/evals/refresher.go" "$dispatch_src/evals/figures_read.go"
      "$dispatch_src/evals/figures_refresher.go" "$dispatch_src/evals/figures.go"
      "$dispatch_src/evals/statistics.go" "$dispatch_src/evals/delivery_targets.go" "$dispatch_src/evals/evals.go")
    # grep exits 1 when no line matches and above 1 when it could not read a file, which would
    # leave a file of the read path unread; pipefail hands that status on through tr. Each file is
    # read as one record (-z), so a statement that breaks its line between `from` and `events` is
    # found too, and each match is printed with its file on one line.
    searched=0
    hits=$(grep -H -o -z -i -E '\b(from|join)[[:space:]]+events\b' "${read_path[@]}" | tr '\0\n' '\n ') || searched=$?
    if [ "$searched" -gt 1 ]; then
      echo "FAIL k13 grep exited $searched, so a file of the read path was not read: ${read_path[*]}"
      status=1
    fi
    echo "RESULT k13 read_path_files=${#read_path[@]} reads_of_events=$(printf '%s' "$hits" | grep -c . || true)"
    if [ -n "$hits" ]; then
      echo "FAIL k13 the read path names events:"
      printf '%s\n' "$hits"
      status=1
    fi
  else
    echo "FAIL k13 EVALBENCH_SOURCE names no checkout, so the read path was not read"
    status=1
  fi
  # k16: the list route's query keys, from the route table's own OpenAPI document. The route takes
  # the header's filters, the window, the compare range and the repository, and no other facet
  # (R16): beside them it takes only the page (limit, offset), the kind of eval the list holds (the
  # section of the Evals tab, which selects evals and filters no row) and the session a credential
  # acts for, which every route takes. Any other key fails.
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
    facets=""
    for key in $keys; do
      case " from to compare_from compare_to repo limit offset kind session " in
        *" $key "*) ;;
        *) facets="$facets $key" ;;
      esac
    done
    if [ -n "$facets" ]; then
      echo "FAIL k16 the list route takes a facet beside the window, the compare range and the repository:$facets"
      status=1
    fi
  else
    echo "FAIL k16 the list route's query keys could not be read from $server_bin openapi"
    status=1
  fi
fi

if [ -n "${EVALBENCH_PG_RESTART:-}" ]; then
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
