#!/usr/bin/env bash
# The eval registry's measurement (dispatch://INFRA-2916, T15: k2, k8, k13, k16, and
# dispatch://INFRA-978's k14, T19.9) and the Evals tab's (dispatch://INFRA-2918, T27: k4, k19, k22)
# over a built envoy-dispatch and a database the evalbench fixture is loaded into.
#
#   run.sh migrate <database-url> <envoy-dispatch>
#   run.sh measure <database-url> <envoy-dispatch> <head|failure-day|base|base-failure-day|k14>
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
# a read of events (k13) and judges the query keys of the list route (k16). failure-day is this head
# over the fixture with a failure day added to it (evalbench's -failure-day), whose paint is the
# Delivery timeline's alone; base is the base's paint, and base-failure-day the base's over the same
# history with the failure day added, whose reads are the Delivery page's the head's timeline reads
# are set beside. Each is measured once unless EVALBENCH_PAINT_RUNS names more, in which case the
# runs are tagged and summarised as the head's are. With EVALBENCH_PG_RESTART set it
# measures the first paint once more after a Postgres restart, for information: it judges no
# budget, and what it misses prints as INFO-FAIL lines, which fail no step.
#
# k14 is this head over the k14 load (evalbench's -k14: two more projects at INFRA's size, PAIR
# with two github_workflow sources and MIXED with a github_workflow source and an events source).
# Its server's GitHub App points at a loopback stand-in (`measure github`), which makes the App's
# throwaway key itself and answers the source check the settings route makes before it saves; the
# catch-up also waits for the production signals' loop. Then, with the statement log on from the
# server's start, `measure k14` reads the daily whole failure write of the server's first walking
# pass, opens three pages of each k14 project's Evals tab and makes k14's writes through the routes
# a person's page uses (a settings save moving a source to another repository and back, a move of
# the default repository a source naming none reads and back, and a settings save changing a
# source's production job names and back), each timed from its commit to the last page's update
# and with every transaction it starts, and writes the warm first-paint reads; then each k14
# project's first paint is measured EVALBENCH_PAINT_RUNS times (2 by default, so each of its two
# reads is the first read after a restart once), each after a fresh start of the server, and
# `measure k14-paints` judges them together. R14 binds every k14 bound at both rates, so
# -ten-times records none of them. The information round after a Postgres restart is not run.
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
# The server runs with no NATS (DISPATCH_NATS_DISABLED=1), no GitHub App (mode k14: the stand-in's)
# and, but for tab, the header identity the Go tests use, and is reached with its agent token, which
# every client of it here reads from a file (or, for curl, from standard input), never from its
# command line. It runs under the server's supervisor (supervise.sh, in EVALBENCH_CONTROL), in the
# server's own container where the pod has one, and a stand-in here takes its place: the stand-in
# lives as long as the server, passes it the signals it is sent, and exits with its status. Its
# build must be stamped
# (-ldflags "-X main.buildCommit=<commit>"): an unstamped binary is a new build at every start, and
# its restart would rebuild every family the measurement waits for. The database URL is a
# superuser's: the instrument resets pg_stat_statements and turns the statement log on and off.
#
# Environment:
#   EVALBENCH_WORK        where the server's home, its logs and the measurement's output go (required)
#   EVALBENCH_CONTROL     the supervisors' directory, whose server/ the server runs under (required;
#                         job.sh sets it)
#   EVALBENCH_MEASURE     the built measure command (required by every command but migrate)
#   EVALBENCH_PG_LOG      Postgres's jsonlog file, with log_destination jsonlog (required by measure)
#   EVALBENCH_PORT        the port the server serves on (58111)
#   EVALBENCH_GITHUB_PORT the port the GitHub stand-in serves on in mode k14 (58112)
#   EVALBENCH_SOURCE      the checkout the server was built from, whose read path k13 greps
#   EVALBENCH_PG_RESTART  a command that restarts Postgres, for the information round
#   EVALBENCH_CATCHUP     how long the catch-up may take (1h; 150m in mode k14)
#   EVALBENCH_QUIET, EVALBENCH_CATALOG   measure paint's -quiet and -catalog (30s, 65s)
#   EVALBENCH_PAINT_RUNS  how many times the first paint is measured (8 at this head, 2 for each
#                         project in mode k14; once in every other mode unless named; fewer for a
#                         smoke run, but never under 2 in mode k14, which refuses 1 before it starts)
#   EVALBENCH_NOW         the UTC midnight the loaded history ends at, measure paint's -now (job.sh
#                         sets it; the current UTC midnight where unset); capture-base and pages: the
#                         history's end
#   EVALBENCH_TEN_TIMES   1 where the fixture is at ten times the stated run rate, measure's
#                         -ten-times: a database time over a budget stated at the stated rate, and
#                         k8's base control, print RECORD lines there, not FAIL lines
#   EVALBENCH_SCALE       the share of INFRA's rate the fixture is loaded at, measure's -scale,
#                         which k14's lines name
#   EVALBENCH_NODE        the node the run is on, measure's -node, which its k16 lines name (job.yaml
#                         sets it from the pod's spec.nodeName)
#   EVALBENCH_TAB_QUIET, EVALBENCH_TAB_CATALOG   tab/paint.ts's --quiet and --catalog, seconds (30, 65)
#   EVALBENCH_TAB, EVALBENCH_DISPATCH_DIR, EVALBENCH_WEB_DIST   tab: the evalbench/tab directory,
#                         packages/dispatch, and the built web app the server serves
#   EVALBENCH_RECOMPUTE_ARGS  tab: more flags for tab/recompute.ts (its phases' sizes)
#   EVALBENCH_BASE_TIMELINE   capture-base and pages: the capture's file
#   EVALBENCH_PAGES       pages: the built timelinepages command
#
# Every measurement prints a RESULT line, and every one that misses its budget or its rule a FAIL
# line, or a RECORD line for a budget no check gates at the run's rate; the script exits 1 when any
# step failed.
set -euo pipefail

usage() {
  echo "usage: run.sh migrate <database-url> <envoy-dispatch> | run.sh measure <database-url> <envoy-dispatch> <head|failure-day|base|base-failure-day|k14>" \
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
    case "$mode" in head | failure-day | base | base-failure-day | k14) ;; *) usage ;; esac
    : "${EVALBENCH_MEASURE:?EVALBENCH_MEASURE must name the built measure command}"
    : "${EVALBENCH_PG_LOG:?EVALBENCH_PG_LOG must name Postgres jsonlog file}"
    # The first paint is measured EVALBENCH_PAINT_RUNS times: 8 by default at this head (k8), 2
    # for each project in mode k14, once in every other mode unless the count is named.
    runs=1
    [ "$mode" != head ] || runs=8
    [ "$mode" != k14 ] || runs=2
    runs=${EVALBENCH_PAINT_RUNS:-$runs}
    case "$runs" in '' | *[!0-9]* | 0*)
      echo "run.sh: EVALBENCH_PAINT_RUNS is a count from 1, not '$runs'" >&2
      exit 2
      ;;
    esac
    # Mode k14 judges each project's two reads each as the first read after a restart once
    # (`measure k14-paints`), and a run makes one of them first: one run leaves the other never
    # first, which fails every project.
    if [ "$mode" = k14 ] && [ "$runs" -lt 2 ]; then
      echo "run.sh: mode k14 measures each project's first paint at least twice, so each of its two reads is the first after a restart once; EVALBENCH_PAINT_RUNS is $runs" >&2
      exit 2
    fi
    recorded=0
    if [ "$mode" = head ] || [ "$runs" -gt 1 ]; then
      recorded=1
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
# token_file holds the agent token for every client this script starts, which reads it from there
# (measure's -token-file, recompute.ts's and timelinepages' --token-file), so the token is in no
# command line; curl reads its header from standard input instead (printf is a builtin).
token_file=$work/token
(umask 077 && printf '%s' "$token" >"$token_file")
server_pid=""
github_pid=""
# The server's environment beside the bench's own: the GitHub App's, in mode k14 (start_github).
server_env=()
status=0

# The identity the server reads people by: the Go tests' trusted header, or, for the tab, the
# cookie a deployment uses, signed by dev sign-in (which serves a loopback origin alone).
identity=(DISPATCH_IDENTITY=header:X-Dispatch-User DISPATCH_IDENTITY_HEADER_TRUSTED=1)
web_dist=$work/web
if [ "$command" = tab ]; then
  identity=(DISPATCH_IDENTITY=cookie DISPATCH_INSECURE_COOKIE=1 DISPATCH_DEV_SIGNIN=1 DISPATCH_SERVER_URL="$url")
  web_dist=$EVALBENCH_WEB_DIST
fi

# start_server boots the server under its supervisor with its stderr in $1 and waits until
# /healthz answers. server_pid is its stand-in's (the header), which kill and wait treat as the
# server's.
start_server() {
  local log=$1
  "$here/supervise.sh" exec "$EVALBENCH_CONTROL/server" "$log" env -i -C "$work/home" PATH="$PATH" HOME="$work/home" \
    XDG_CONFIG_HOME="$work/home" XDG_DATA_HOME="$work/home" \
    DATABASE_URL="$database_url" DISPATCH_AGENT_TOKEN="$token" "${identity[@]}" DISPATCH_LISTEN_HOST=127.0.0.1 \
    DISPATCH_PORT="$port" DISPATCH_WEB_DIST="$web_dist" DISPATCH_NATS_DISABLED=1 "${server_env[@]}" "$server_bin" &
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
# stop of Postgres, the next measure) never runs beside it, and stops the GitHub stand-in.
trap 'if [ -n "$server_pid" ]; then kill -TERM "$server_pid" 2>/dev/null || true; wait "$server_pid" 2>/dev/null || true; fi
  if [ -n "$github_pid" ]; then kill -TERM "$github_pid" 2>/dev/null || true; wait "$github_pid" 2>/dev/null || true; fi' EXIT

if [ "$command" = migrate ]; then
  start_server "$work/server-migrate.log"
  stop_server
  exit "$status"
fi

# measure runs one subcommand of the measure command against the running server, in the mode of
# the head the server is: measure's own, this head's for tab and pages, the base's for
# capture-base, whose consumers hold no day rows of this head's to wait for; with the day the
# loaded history ends on where job.sh names it, the node where the Job names it, the scale where
# job.sh names it, and -ten-times where the fixture is at ten times the stated run rate.
case "$command" in
  measure) measure_mode=$mode ;;
  capture-base) measure_mode=base ;;
  *) measure_mode=head ;;
esac
measure() {
  local subcommand=$1 flags=()
  shift
  if [ -n "${EVALBENCH_NOW:-}" ]; then
    flags+=(-now "$EVALBENCH_NOW")
  fi
  if [ -n "${EVALBENCH_NODE:-}" ]; then
    flags+=(-node "$EVALBENCH_NODE")
  fi
  if [ -n "${EVALBENCH_SCALE:-}" ]; then
    flags+=(-scale "$EVALBENCH_SCALE")
  fi
  if [ "${EVALBENCH_TEN_TIMES:-0}" = 1 ]; then
    flags+=(-ten-times)
  fi
  "$EVALBENCH_MEASURE" "$subcommand" -mode "$measure_mode" -database "$database_url" -url "$url" "${flags[@]}" "$@" || status=1
}

# start_github starts, in mode k14, the GitHub stand-in the server's App calls (`measure github`,
# github.go), on loopback, installed on the fixture's owner, and waits until it serves and has
# written the App's throwaway key, which the server's environment then carries. No key GitHub knows
# is loaded, and nothing the App signs leaves the pod.
github_port=${EVALBENCH_GITHUB_PORT:-58112}
github_log=$work/github.log
start_github() {
  local key=$work/github-app.pem client=Iv1.evalbench started=$SECONDS
  rm -f "$key"
  "$EVALBENCH_MEASURE" github -listen "127.0.0.1:$github_port" -key-out "$key" -client-id "$client" -owners acme \
    -log "$github_log" 2>>"$work/github-stderr.log" &
  github_pid=$!
  until [ -s "$key" ] && curl -s -o /dev/null "http://127.0.0.1:$github_port/"; do
    if ! kill -0 "$github_pid" 2>/dev/null || [ $((SECONDS - started)) -gt 60 ]; then
      echo "FAIL run.sh: the GitHub stand-in did not serve; the end of $work/github-stderr.log:"
      tail -n 20 "$work/github-stderr.log" || true
      exit 1
    fi
    sleep 0.2
  done
  server_env=(DISPATCH_APP_CLIENT_ID="$client" DISPATCH_APP_PEM_B64="$(<"$key")" DISPATCH_GITHUB_API_BASE="http://127.0.0.1:$github_port")
  echo "MARK $(date -u +%FT%TZ) GitHub stand-in $github_pid serving 127.0.0.1:$github_port, logging to $github_log"
}

catchup=${EVALBENCH_CATCHUP:-1h}
if [ "$mode" = k14 ]; then
  catchup=${EVALBENCH_CATCHUP:-150m}
  start_github
fi
start_server "$work/server-catchup.log"
measure wait -timeout "$catchup"
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
  bun "$EVALBENCH_TAB/recompute.ts" --url "$url" --database "$database_url" --project INFRA --rate "$mode" \
    --token-file "$token_file" ${EVALBENCH_RECOMPUTE_ARGS:-} || status=1
  stop_server
  echo "MARK $(date -u +%FT%TZ) run.sh tab $mode exits $status"
  exit "$status"
fi

if [ "$command" = capture-base ] || [ "$command" = pages ]; then
  start_server "$work/server-$command.log"
  if [ "$command" = capture-base ]; then
    window=$(python3 -c 'import datetime, sys; end = datetime.datetime.fromisoformat(sys.argv[1].replace("Z", "+00:00")); print((end - datetime.timedelta(days=84)).strftime("%Y-%m-%dT%H:%M:%SZ"), end.strftime("%Y-%m-%dT%H:%M:%SZ"))' "$EVALBENCH_NOW")
    read -r from to <<<"$window"
    if printf 'Authorization: Bearer %s\n' "$token" |
      curl -fsS -H @- "$url/api/v1/delivery/timeline?from=$from&to=$to" -o "$EVALBENCH_BASE_TIMELINE"; then
      echo "RESULT k22 capture from=$from to=$to file=$EVALBENCH_BASE_TIMELINE bytes=$(wc -c <"$EVALBENCH_BASE_TIMELINE") $(python3 -c 'import json, sys; body = json.load(open(sys.argv[1])); print("prs=%d runs=%d waiting=%d" % (len(body.get("prs") or []), len(body.get("runs") or []), len(body.get("waiting") or [])))' "$EVALBENCH_BASE_TIMELINE")"
    else
      echo "FAIL k22 capture: GET /api/v1/delivery/timeline for $from..$to failed at the base head"
      status=1
    fi
  else
    "$EVALBENCH_PAGES" --url "$url" --token-file "$token_file" --project INFRA --window 84d --limit 200 \
      --compare "$EVALBENCH_BASE_TIMELINE" || status=1
  fi
  stop_server
  echo "MARK $(date -u +%FT%TZ) run.sh $command exits $status"
  exit "$status"
fi

if [ "$mode" = k14 ]; then
  # The writes, on a fresh start of the server with the statement log on from its start, so the
  # daily whole failure write of its first walking pass is in the log `measure k14` reads from
  # pg_from on. measure k14 turns the log off again as it ends.
  psql "$database_url" -Atq -c "alter system set log_min_duration_statement = 0" -c "select pg_reload_conf()" >/dev/null
  pg_from=$(stat -c %s "$EVALBENCH_PG_LOG")
  expect_file=$work/k14-expect.jsonl
  rm -f "$expect_file"
  start_server "$work/server-k14.log"
  measure k14 -token-file "$token_file" -server-log "$work/server-k14.log" -pg-log "$EVALBENCH_PG_LOG" -pg-log-from "$pg_from" \
    -github-log "$github_log" -expect-file "$expect_file"
  stop_server
  # The first paints of the two k14 projects (evalbench's K14Projects), each run after a fresh
  # start of the server, each judged against the warm reads the writes left in $expect_file.
  paints_file=$work/k14-paints.jsonl
  : >"$paints_file"
  for project in PAIR MIXED; do
    for run in $(seq 1 "$runs"); do
      paint_log=$work/server-k14-paint-$project-$run.log
      start_server "$paint_log"
      measure paint -project "$project" -run "$run" -runs-file "$paints_file" -expect-file "$expect_file" -token-file "$token_file" \
        -server-log "$paint_log" -pg-log "$EVALBENCH_PG_LOG" -quiet "${EVALBENCH_QUIET:-30s}" -catalog "${EVALBENCH_CATALOG:-65s}"
      stop_server
    done
  done
  measure k14-paints -runs-file "$paints_file"
  echo "MARK $(date -u +%FT%TZ) run.sh measure $mode exits $status"
  exit "$status"
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
  measure paint -token-file "$token_file" -server-log "$paint_log" -pg-log "$EVALBENCH_PG_LOG" \
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
  # acts for, which every route takes. The header's filters are the facets and the search of the
  # Delivery timeline's route, which the list takes as the timeline does (dispatch://INFRA-2918, R21),
  # read from the same document, so a key the list takes that the timeline does not still fails. Any
  # other key fails.
  if keys=$("$server_bin" openapi | python3 -c '
import json, sys
paths = json.load(sys.stdin)["paths"]
def query(path):
    operation = paths[path]["get"]
    return " ".join(sorted(p["name"] for p in operation.get("parameters", []) if p.get("in") == "query"))
print(query("/api/v1/projects/{key}/evals"))
print(query("/api/v1/delivery/timeline"))'); then
    header=$(sed -n 2p <<<"$keys")
    keys=$(sed -n 1p <<<"$keys")
    echo "RESULT k16 list_route_query_keys=[$keys] header_filters=[$header]"
    case " $keys " in
      *" repo "*) ;;
      *)
        echo "FAIL k16 the list route takes no repo"
        status=1
        ;;
    esac
    facets=""
    for key in $keys; do
      case " from to compare_from compare_to repo limit offset kind session $header " in
        *" $key "*) ;;
        *) facets="$facets $key" ;;
      esac
    done
    if [ -n "$facets" ]; then
      echo "FAIL k16 the list route takes a key beside the header's filters, the window, the compare range and the repository:$facets"
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
  measure paint -info -token-file "$token_file" -server-log "$work/server-cold.log" -pg-log "$EVALBENCH_PG_LOG" \
    -quiet "${EVALBENCH_QUIET:-30s}" -catalog "${EVALBENCH_CATALOG:-65s}"
fi
stop_server
echo "MARK $(date -u +%FT%TZ) run.sh measure $mode exits $status"
exit "$status"
