#!/usr/bin/env bash
# The eval registry's measurement (dispatch://INFRA-2916, T15: k2, k8, k13, k16) and the Evals
# tab's (dispatch://INFRA-2918, T27: k4, k19, k22) over a built envoy-dispatch and a database the
# evalbench fixture is loaded into.
#
#   run.sh migrate <database-url> <envoy-dispatch>
#   run.sh measure <database-url> <envoy-dispatch> <head|failure-day|base>
#   run.sh tab <database-url> <envoy-dispatch> <stated|ten>
#   run.sh capture-base <database-url> <envoy-dispatch>
#   run.sh pages <database-url> <envoy-dispatch>
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
# a read of events (k13) and of the code survival results (k17) and judges the query keys of the
# list route (k16). Where the fixture holds the code survival results, the catch-up also waits for
# the regroup of INFRA's code day rows, each run's paint holds both code evals and the statements of
# its reads name none of the results' tables, and, with EVALBENCH_GITHUB_STUB=1 below, no request to
# GitHub falls in a paint (k17). The base is measured by the same list reads, with no budget, where
# it holds the eval registry (job.sh's EVALBENCH_BASE_FLAGS empty, a base before the code evals),
# whose answer holds neither code eval. failure-day is this head
# over the fixture with a failure day added to it (evalbench's -failure-day), whose paint is the
# Delivery timeline's alone, measured once, as the base's is. With EVALBENCH_PG_RESTART set it
# measures the first paint once more after a Postgres restart, for information: it judges no
# budget, and what it misses prints as INFO-FAIL lines, which fail no step.
#
# tab is the Evals tab in Chromium (evalbench/tab, through the Playwright packages/dispatch locks),
# at the run rate its last argument names, which its lines carry: once the consumers have caught
# up, for each of the paint's three cases (84 days with a compare range, 28 days with none, and 84
# days from an address that sets the author facet) the server starts again and the tab's first
# reader is timed (tab/paint.ts); then the server starts once more and INFRA-2916's write drive,
# phases A to D, runs with the tab open on two pages and the merge timeline open on each
# (tab/recompute.ts). The server serves the built web app (EVALBENCH_WEB_DIST) and signs its
# readers in through dev sign-in, with the cookie identity a deployment uses.
#
# capture-base is k22's capture at the base head (legion/INFRA-978 82380570f8) over the
# one-project fixture: once the consumers have caught up, GET /api/v1/delivery/timeline for the 84
# days before EVALBENCH_NOW, written to EVALBENCH_BASE_TIMELINE. pages is k22's read at this head
# over the same fixture: once the consumers have caught up, evalbench/timelinepages reads the
# project's timeline page by page and compares the union with that capture.
#
# The server runs with no NATS (DISPATCH_NATS_DISABLED=1), no GitHub App and, but for tab, the
# header identity the Go tests use, and is reached with its agent token. Its build must be stamped
# (-ldflags "-X main.buildCommit=<commit>"): an unstamped binary is a new build at every start, and
# its restart would rebuild every family the measurement waits for. The database URL is a
# superuser's: the instrument resets pg_stat_statements and turns the statement log on and off.
#
# With EVALBENCH_GITHUB_STUB=1, in measure alone, the server runs with a GitHub App instead, so that
# the requests it makes to GitHub are on record (check k17, dispatch://INFRA-3206, R15: a page read
# costs no request to GitHub, and a server with no App could make none): `measure ghstub` listens on
# a loopback port, answers the App's installation and token calls and 404 to every other route, and
# writes each request it is sent, with its time, method, path and Accept, as a JSON line to
# $EVALBENCH_WORK/github.jsonl; the server gets a throwaway App key made here (openssl genrsa 2048)
# as DISPATCH_APP_PEM_B64, DISPATCH_APP_CLIENT_ID=evalbench and the stand-in as
# DISPATCH_GITHUB_API_BASE; and `measure paint -github-log` prints a FAIL line for each request the
# log holds that came between the paint's first request and the answer of its last, and a RESULT
# line with how many came outside it. The server's delivery reconcile, which would make a request at
# a random moment of its first five minutes, is put off for a year (DISPATCH_TEST_HOOKS=1 and
# DISPATCH_TEST_DELIVERY_RECONCILE_INTERVAL=8760h): it is a loop of the server's own and no read of
# the page, and one request of it in a paint's few seconds would fail a paint no read caused.
# Without the variable the server runs as it did and paint reads no log.
#
# Environment:
#   EVALBENCH_WORK        where the server's home, its logs and the measurement's output go (required)
#   EVALBENCH_MEASURE     the built measure command (required by every command but migrate)
#   EVALBENCH_PG_LOG      Postgres's jsonlog file, with log_destination jsonlog (required by measure)
#   EVALBENCH_PORT        the port the server serves on (58111)
#   EVALBENCH_SOURCE      the checkout the server was built from, whose read path k13 greps
#   EVALBENCH_PG_RESTART  a command that restarts Postgres, for the information round
#   EVALBENCH_CATCHUP     how long the catch-up may take (1h)
#   EVALBENCH_QUIET, EVALBENCH_CATALOG   measure paint's -quiet and -catalog (30s, 65s)
#   EVALBENCH_PAINT_RUNS  how many times this head's first paint is measured (8; fewer for a smoke
#                         run)
#   EVALBENCH_TAB_QUIET, EVALBENCH_TAB_CATALOG   tab/paint.ts's --quiet and --catalog, seconds (30, 65)
#   EVALBENCH_TAB, EVALBENCH_DISPATCH_DIR, EVALBENCH_WEB_DIST   tab: the evalbench/tab directory,
#                         packages/dispatch, and the built web app the server serves
#   EVALBENCH_RECOMPUTE_ARGS  tab: more flags for tab/recompute.ts (its phases' sizes)
#   EVALBENCH_NOW         the UTC midnight the loaded history ends at: measure paint's -now (job.sh
#                         sets it; the current UTC midnight where unset), and the history's end of
#                         capture-base and pages
#   EVALBENCH_BASE_TIMELINE   capture-base and pages: the capture's file
#   EVALBENCH_PAGES       pages: the built timelinepages command
#   EVALBENCH_NODE        the node the run is on, which measure's k16 lines name (job.yaml sets it
#                         from the pod's spec.nodeName); run.sh passes it to measure as -node
#   EVALBENCH_GITHUB_STUB  1 starts the GitHub stand-in above in measure (0)
#
# Every measurement prints a RESULT line, and every one that misses its budget or its rule a FAIL
# line; the script exits 1 when any step failed.
set -euo pipefail

usage() {
  echo "usage: run.sh migrate <database-url> <envoy-dispatch> | run.sh measure <database-url> <envoy-dispatch> <head|failure-day|base>" \
    "| run.sh tab <database-url> <envoy-dispatch> <stated|ten> | run.sh capture-base|pages <database-url> <envoy-dispatch>" >&2
  exit 2
}
[ $# -ge 3 ] || usage
command=$1 database_url=$2 server_bin=$3
mode=${4:-}
case "$command" in
  migrate) [ $# -eq 3 ] || usage ;;
  measure)
    [ $# -eq 4 ] || usage
    case "$mode" in head | failure-day | base) ;; *) usage ;; esac
    : "${EVALBENCH_MEASURE:?EVALBENCH_MEASURE must name the built measure command}"
    : "${EVALBENCH_PG_LOG:?EVALBENCH_PG_LOG must name Postgres jsonlog file}"
    runs=1
    if [ "$mode" = head ]; then
      runs=${EVALBENCH_PAINT_RUNS:-8}
      case "$runs" in '' | *[!0-9]* | 0*)
        echo "run.sh: EVALBENCH_PAINT_RUNS is a count from 1, not '$runs'" >&2
        exit 2
        ;;
      esac
    fi
    ;;
  tab)
    [ $# -eq 4 ] || usage
    case "$mode" in stated | ten) ;; *) usage ;; esac
    : "${EVALBENCH_MEASURE:?EVALBENCH_MEASURE must name the built measure command}"
    : "${EVALBENCH_TAB:?EVALBENCH_TAB must name the evalbench/tab directory}"
    : "${EVALBENCH_WEB_DIST:?EVALBENCH_WEB_DIST must name the built web app}"
    ;;
  capture-base | pages)
    [ $# -eq 3 ] || usage
    : "${EVALBENCH_MEASURE:?EVALBENCH_MEASURE must name the built measure command}"
    : "${EVALBENCH_NOW:?EVALBENCH_NOW must name the history's end}"
    : "${EVALBENCH_BASE_TIMELINE:?EVALBENCH_BASE_TIMELINE must name the base capture's file}"
    if [ "$command" = pages ]; then : "${EVALBENCH_PAGES:?EVALBENCH_PAGES must name the built timelinepages command}"; fi
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
stub_pid=""
# The settings the server and the paint take when the GitHub stand-in runs (start_github_stub).
server_github=()
paint_github=()
# The identity the server reads people by: the Go tests' trusted header, or, for the tab, the
# cookie a deployment uses, signed by dev sign-in (which serves a loopback origin alone).
identity=(DISPATCH_IDENTITY=header:X-Dispatch-User DISPATCH_IDENTITY_HEADER_TRUSTED=1)
web_dist=$work/web
if [ "$command" = tab ]; then
  identity=(DISPATCH_IDENTITY=cookie DISPATCH_INSECURE_COOKIE=1 DISPATCH_DEV_SIGNIN=1 DISPATCH_SERVER_URL="$url")
  web_dist=$EVALBENCH_WEB_DIST
fi

# start_server boots the server with its stderr in $1 and waits until /healthz answers.
start_server() {
  local log=$1
  (cd "$work/home" && exec env -i PATH="$PATH" HOME="$work/home" XDG_CONFIG_HOME="$work/home" XDG_DATA_HOME="$work/home" \
    DATABASE_URL="$database_url" DISPATCH_AGENT_TOKEN="$token" "${identity[@]}" DISPATCH_LISTEN_HOST=127.0.0.1 \
    DISPATCH_PORT="$port" DISPATCH_WEB_DIST="$web_dist" DISPATCH_NATS_DISABLED=1 "${server_github[@]}" "$server_bin" >>"$log" 2>&1) &
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

# start_github_stub starts the GitHub stand-in (measure ghstub) and waits until it has published
# its port, and sets what the server and the paint are given for it: the throwaway App, whose key is
# made here and kept in no file, and the log paint reads.
start_github_stub() {
  command -v openssl >/dev/null 2>&1 || {
    echo "FAIL run.sh: EVALBENCH_GITHUB_STUB=1 needs openssl, which makes the throwaway App key"
    exit 1
  }
  local pem port_file=$work/github.port
  pem=$(openssl genrsa 2048 2>/dev/null | base64 -w0) || pem=""
  [ -n "$pem" ] || {
    echo "FAIL run.sh: openssl made no App key"
    exit 1
  }
  rm -f "$port_file"
  "$EVALBENCH_MEASURE" ghstub -log "$work/github.jsonl" -port-file "$port_file" 2>>"$work/github-stub.log" &
  stub_pid=$!
  local started=$SECONDS
  until [ -s "$port_file" ]; do
    if ! kill -0 "$stub_pid" 2>/dev/null; then
      echo "FAIL run.sh: the GitHub stand-in exited before it listened; the end of $work/github-stub.log:"
      tail -n 20 "$work/github-stub.log"
      exit 1
    fi
    if [ $((SECONDS - started)) -gt 30 ]; then
      echo "FAIL run.sh: the GitHub stand-in did not listen within 30 s; the end of $work/github-stub.log:"
      tail -n 20 "$work/github-stub.log"
      exit 1
    fi
    sleep 0.2
  done
  local stub_port
  stub_port=$(tr -d '\n' <"$port_file")
  server_github=(DISPATCH_APP_CLIENT_ID=evalbench "DISPATCH_APP_PEM_B64=$pem" "DISPATCH_GITHUB_API_BASE=http://127.0.0.1:$stub_port"
    DISPATCH_TEST_HOOKS=1 DISPATCH_TEST_DELIVERY_RECONCILE_INTERVAL=8760h)
  paint_github=(-github-log "$work/github.jsonl")
  echo "MARK $(date -u +%FT%TZ) GitHub stand-in $stub_pid answering the App's calls on port $stub_port, logging to $work/github.jsonl"
}

# stop_github_stub stops the stand-in with SIGTERM, which it answers with its log whole and exit 0,
# and prints how many requests the log holds; a stand-in that had died is a FAIL line.
stop_github_stub() {
  [ -n "$stub_pid" ] || return 0
  kill -TERM "$stub_pid" 2>/dev/null || true
  local code=0
  wait "$stub_pid" || code=$?
  stub_pid=""
  if [ "$code" -ne 0 ]; then
    echo "FAIL run.sh: the GitHub stand-in exited $code"
    status=1
  fi
  echo "RESULT k17 github_stub requests_logged=$(wc -l <"$work/github.jsonl") log=$work/github.jsonl"
}
# Every exit stops a server still running and waits for it, so the caller's next step (job.sh's
# stop of Postgres, the next measure) never runs beside it, and the stand-in the same.
trap 'if [ -n "$server_pid" ]; then kill -TERM "$server_pid" 2>/dev/null || true; wait "$server_pid" 2>/dev/null || true; fi
  if [ -n "$stub_pid" ]; then kill -TERM "$stub_pid" 2>/dev/null || true; wait "$stub_pid" 2>/dev/null || true; fi' EXIT

if [ "$command" = migrate ]; then
  start_server "$work/server-migrate.log"
  stop_server
  exit "$status"
fi

# measure runs one subcommand of the measure command against the running server, in the mode of
# the head the server is: measure's own, this head's for tab and pages, the base's for
# capture-base, whose consumers hold no day rows of this head's to wait for; and with the day the
# loaded history ends on where job.sh names it.
case "$command" in
  measure) measure_mode=$mode ;;
  capture-base) measure_mode=base ;;
  *) measure_mode=head ;;
esac
measure() {
  local subcommand=$1 now=()
  shift
  if [ -n "${EVALBENCH_NOW:-}" ]; then
    now=(-now "$EVALBENCH_NOW")
  fi
  "$EVALBENCH_MEASURE" "$subcommand" -mode "$measure_mode" -database "$database_url" -url "$url" -node "${EVALBENCH_NODE:-}" "${now[@]}" "$@" || status=1
}

if [ "$command" = measure ] && [ "${EVALBENCH_GITHUB_STUB:-0}" = 1 ]; then
  start_github_stub
fi
start_server "$work/server-catchup.log"
measure wait -timeout "${EVALBENCH_CATCHUP:-1h}"
# status is 0 until the wait, so here it is the wait's own; stop_server sets it on a bad stop.
waited=$status
stop_server
if [ "$waited" -ne 0 ]; then
  echo "FAIL run.sh: the consumers did not catch up, so nothing is measured"
  exit 1
fi
if [ "$status" -ne 0 ]; then
  echo "FAIL run.sh: the server did not stop cleanly after the catch-up, so nothing is measured"
  exit 1
fi

if [ "$command" = tab ]; then
  for paint_case in 84d-compare 28d 84d-author; do
    start_server "$work/server-paint-$paint_case.log"
    bun "$EVALBENCH_TAB/paint.ts" --url "$url" --database "$database_url" --project INFRA --case "$paint_case" --rate "$mode" \
      --quiet "${EVALBENCH_TAB_QUIET:-30}" --catalog "${EVALBENCH_TAB_CATALOG:-65}" || status=1
    stop_server
  done
  start_server "$work/server-recompute.log"
  # shellcheck disable=SC2086 # EVALBENCH_RECOMPUTE_ARGS is flags, split on purpose.
  bun "$EVALBENCH_TAB/recompute.ts" --url "$url" --database "$database_url" --project INFRA --rate "$mode" --token "$token" \
    ${EVALBENCH_RECOMPUTE_ARGS:-} || status=1
  stop_server
  echo "MARK $(date -u +%FT%TZ) run.sh tab $mode exits $status"
  exit "$status"
fi

if [ "$command" = capture-base ] || [ "$command" = pages ]; then
  start_server "$work/server-$command.log"
  if [ "$command" = capture-base ]; then
    window=$(python3 -c 'import datetime, sys; end = datetime.datetime.fromisoformat(sys.argv[1].replace("Z", "+00:00")); print((end - datetime.timedelta(days=84)).strftime("%Y-%m-%dT%H:%M:%SZ"), end.strftime("%Y-%m-%dT%H:%M:%SZ"))' "$EVALBENCH_NOW")
    read -r from to <<<"$window"
    if curl -fsS -H "Authorization: Bearer $token" "$url/api/v1/delivery/timeline?from=$from&to=$to" -o "$EVALBENCH_BASE_TIMELINE"; then
      echo "RESULT k22 capture from=$from to=$to file=$EVALBENCH_BASE_TIMELINE bytes=$(wc -c <"$EVALBENCH_BASE_TIMELINE") $(python3 -c 'import json, sys; body = json.load(open(sys.argv[1])); print("prs=%d runs=%d waiting=%d" % (len(body.get("prs") or []), len(body.get("runs") or []), len(body.get("waiting") or [])))' "$EVALBENCH_BASE_TIMELINE")"
    else
      echo "FAIL k22 capture: GET /api/v1/delivery/timeline for $from..$to failed at the base head"
      status=1
    fi
  else
    token_file=$work/token
    (umask 077 && printf '%s' "$token" >"$token_file")
    "$EVALBENCH_PAGES" --url "$url" --token-file "$token_file" --project INFRA --window 84d --limit 200 \
      --compare "$EVALBENCH_BASE_TIMELINE" || status=1
  fi
  stop_server
  echo "MARK $(date -u +%FT%TZ) run.sh $command exits $status"
  exit "$status"
fi

# The first paint: at this head $runs times (EVALBENCH_PAINT_RUNS), each as the first requests
# after a fresh start of the server, each run's lines tagged run=<n> and its reads appended to
# paint-runs.jsonl, which `measure runs` reads once the runs are done; in the other modes once.
runs_file=$work/paint-runs.jsonl
if [ "$mode" = head ]; then
  : >"$runs_file"
fi
for run in $(seq 1 "$runs"); do
  paint_log=$work/server-paint.log
  tagged=()
  if [ "$mode" = head ]; then
    paint_log=$work/server-paint-$run.log
    tagged=(-run "$run" -runs-file "$runs_file")
  fi
  start_server "$paint_log"
  measure paint -token "$token" -server-log "$paint_log" -pg-log "$EVALBENCH_PG_LOG" \
    -quiet "${EVALBENCH_QUIET:-30s}" -catalog "${EVALBENCH_CATALOG:-65s}" "${tagged[@]}" "${paint_github[@]}"
  stop_server
done
if [ "$mode" = head ]; then
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
    # k17: nor does it name the code survival results (dispatch://INFRA-3206, R15): the day rows
    # and the stored figures hold what the page shows, so no file of the read path reads
    # code_changes, code_pairs or code_days. This is the files' half of the rule, and `measure
    # paint` holds the statements the paint ran to it, in their text and in their plans.
    searched=0
    results=$(grep -H -o -z -i -E '\b(from|join)[[:space:]]+code_(changes|pairs|days)\b' "${read_path[@]}" | tr '\0\n' '\n ') || searched=$?
    if [ "$searched" -gt 1 ]; then
      echo "FAIL k17 grep exited $searched, so a file of the read path was not read: ${read_path[*]}"
      status=1
    fi
    echo "RESULT k17 read_path_files=${#read_path[@]} reads_of_code_results=$(printf '%s' "$results" | grep -c . || true)"
    if [ -n "$results" ]; then
      echo "FAIL k17 the read path names the code survival results:"
      printf '%s\n' "$results"
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
    -quiet "${EVALBENCH_QUIET:-30s}" -catalog "${EVALBENCH_CATALOG:-65s}" "${paint_github[@]}"
fi
stop_server
stop_github_stub
echo "MARK $(date -u +%FT%TZ) run.sh measure $mode exits $status"
exit "$status"
