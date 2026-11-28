#!/usr/bin/env bash
# R8's Job (dispatch://INFRA-5577, check k14; spec Design, "How R8 is measured"), whole, in one pod:
# a Postgres 18 of its own, then `main`'s tip and then this head, one after the other. For each
# build it builds the server stamped with its commit, migrates a database of its own by booting the
# server once, loads INFRA's history at ten times the run volume (legionbench load -ten-times),
# starts the daemon stand-in with 1,000 cards of the loaded issues and a NATS server with JetStream
# of the arm's own, boots the server on the loaded database and that NATS, and drives one `legion`
# view (legionbench drive): this head's delivery-live-probe.ts for both builds, issue events through
# the API at 0.26 a second for 10 minutes. The server needs NATS for the stand-in's cards: its
# Legion state intake, which starts only where the server has NATS, reads the stand-in whole as it
# starts, and no other path reads a daemon. The drive first waits for that read and, at this head,
# for the Legion facts consumer to read INFRA whole, for 20 minutes at the most; then it judges each
# facts change's push and the board's body, and takes the board's facts read, whose plan and median
# time TestFarmBoardFactsRead measures. Last, legionbench compare holds this head's median recompute
# to twice main's. With R8_PROFILE=1 each arm's server also records a CPU profile of the drive's
# window, which shows where its recomputes spend their time. r8.yaml runs it on the build farm.
#
# Every measurement prints a RESULT line and every miss a FAIL line; the script exits 1 when any
# step failed. A CPU profile judges nothing: one that could not be taken is a MARK line, and the
# run's measurements stand without it.
#
# Environment:
#   R8_WORK      scratch for everything the run writes (required)
#   R8_SOURCE    the checkout holding legion/legionbench (the repository this script is in)
#   R8_MAIN      `main`'s commit, the first arm (required)
#   R8_HEAD      this head's commit, the second arm (required); its tree's probe drives both arms
#   R8_SCALE (1), R8_SEED (5577), R8_NOW (the current UTC midnight): the load's -scale, -seed, -now
#   R8_RATE (0.26), R8_DURATION (10m): the drive's -rate and -duration
#   R8_HEAD_FACTS (1): whether this head's server runs the Legion facts consumer; 0 drives this head
#                as main is driven, for a head whose server starts no consumer yet
#   R8_PROFILE (0): 1 profiles each arm's server's CPU over the drive's window, through the
#                profiler the server serves on 127.0.0.1 when DISPATCH_PPROF_PORT is set (main and
#                this head both serve it), into $R8_WORK/out as main.cpu.pprof and head.cpu.pprof;
#                R8_DURATION, the profile's length, is then whole hours, minutes and seconds (10m,
#                1h30m, 90s)
#   R8_PGPORT (55432), R8_SHARED_BUFFERS (2GB), PGBIN (the newest /usr/lib/postgresql/*/bin)
set -euo pipefail

: "${R8_WORK:?R8_WORK must name a scratch directory for the run}"
: "${R8_MAIN:?R8_MAIN must name main's commit}"
: "${R8_HEAD:?R8_HEAD must name this head's commit}"
here=$(cd "$(dirname "$0")" && pwd)
source_dir=${R8_SOURCE:-$(cd "$here/../../../../../.." && pwd)}
work=$R8_WORK
port=${R8_PGPORT:-55432}
now=${R8_NOW:-$(date -u +%Y-%m-%dT00:00:00Z)}
head_facts=${R8_HEAD_FACTS:-1}
profile=${R8_PROFILE:-0}
# Each server is stamped with its full commit, which /healthz answers and the facts consumer records.
R8_MAIN=$(git -C "$source_dir" rev-parse "$R8_MAIN^{commit}")
R8_HEAD=$(git -C "$source_dir" rev-parse "$R8_HEAD^{commit}")
server_port=58111
standin_port=13370
# The port of the profiler of the server that runs, on 127.0.0.1, with R8_PROFILE=1.
pprof_port=58112
# The client port and the monitoring port of the arm's NATS server, on 127.0.0.1. One arm's NATS
# runs at a time, each on a store of its own (start_nats).
nats_port=58113
nats_monitor_port=58114
# The agent token of both servers, which the probe reads from $work/token: drawn for this run.
token=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
if [ -z "${PGBIN:-}" ]; then
  PGBIN=$(ls -d /usr/lib/postgresql/*/bin | sort -V | tail -n 1)
fi
mkdir -p "$work/bin" "$work/pglog" "$work/sock" "$work/out" "$work/web"
printf '%s\n' "$token" >"$work/token"
mark() { echo "MARK $(date -u +%FT%TZ) $*"; }

# duration_seconds prints the Go duration $1, made of whole hours, minutes and seconds (10m,
# 1h30m, 90s), in seconds, the unit of the profiler's `seconds`; it fails for any other form.
duration_seconds() {
  local rest=$1 total=0
  [[ $rest =~ ^([0-9]+[hms])+$ ]] || return 1
  while [[ $rest =~ ^([0-9]+)([hms])(.*)$ ]]; do
    case ${BASH_REMATCH[2]} in
      h) total=$((total + 10#${BASH_REMATCH[1]} * 3600)) ;;
      m) total=$((total + 10#${BASH_REMATCH[1]} * 60)) ;;
      s) total=$((total + 10#${BASH_REMATCH[1]})) ;;
    esac
    rest=${BASH_REMATCH[3]}
  done
  [ "$total" -gt 0 ] || return 1
  echo "$total"
}

# With R8_PROFILE=1 every server this script starts serves its profiler, and each arm's profile
# lasts as long as the drive appends events.
pprof_env=()
profile_seconds=0
case "$profile" in
  0) ;;
  1)
    profile_seconds=$(duration_seconds "${R8_DURATION:-10m}") || {
      echo "FAIL r8 R8_PROFILE=1 profiles for R8_DURATION, which must be whole hours, minutes and seconds, not ${R8_DURATION:-10m}"
      exit 1
    }
    pprof_env=(DISPATCH_PPROF_PORT="$pprof_port")
    ;;
  *)
    echo "FAIL r8 R8_PROFILE is 0 or 1, not $profile"
    exit 1
    ;;
esac
status=0
mark "r8.sh: source $source_dir at $(git -C "$source_dir" rev-parse HEAD), main $R8_MAIN, head $R8_HEAD, scale ${R8_SCALE:-1}, now $now, head facts $head_facts, profile $profile, node ${NODE_NAME:-unknown}"
echo "RESULT r8 main=$R8_MAIN head=$R8_HEAD node=${NODE_NAME:-unknown} scale=${R8_SCALE:-1} rate=${R8_RATE:-0.26} duration=${R8_DURATION:-10m} cards=1000 ten_times=1 profile=$profile"

# Postgres, its data on the run's own volume, its log in jsonlog for the statement the facts read is
# taken from (the drive turns log_min_duration_statement on around one board read). The durability
# settings only speed the load; nothing measured depends on them. The trap is set before Postgres
# starts, so every exit from here on stops it and every process this script started.
pids=()
cleanup() {
  for pid in "${pids[@]}"; do kill -TERM "$pid" 2>/dev/null || true; done
  "$PGBIN/pg_ctl" --pgdata "$work/pg" --wait --mode fast stop >/dev/null 2>&1 || true
}
trap cleanup EXIT
"$PGBIN/initdb" --pgdata "$work/pg" --username postgres --auth=trust --encoding=UTF8 --no-sync >"$work/initdb.log"
"$PGBIN/pg_ctl" --pgdata "$work/pg" --log "$work/pg-server.log" --wait --timeout 300 --options "-p $port -k $work/sock \
  -c listen_addresses=127.0.0.1 -c max_connections=300 -c shared_buffers=${R8_SHARED_BUFFERS:-2GB} \
  -c logging_collector=on -c log_destination=jsonlog -c log_directory=$work/pglog \
  -c log_filename=postgres.log -c log_rotation_age=0 -c log_rotation_size=0 -c log_timezone=UTC \
  -c fsync=off -c full_page_writes=off -c synchronous_commit=off -c wal_level=minimal -c max_wal_senders=0 \
  -c max_wal_size=8GB -c huge_pages=off -c dynamic_shared_memory_type=mmap -c jit=off" start
admin_url="postgres://postgres@127.0.0.1:$port/postgres?sslmode=disable"
psql "$admin_url" -Atc 'select version()'
pg_log=$work/pglog/postgres.json

# The tools, built from the checkout this script is in: the loader, stand-in and drive, the test
# binary of the facts read's measurement, and the NATS server of each arm at the release go.mod
# requires of github.com/nats-io/nats-server/v2, the server the Go tests run in process.
cd "$source_dir/packages/envoy"
go build -o "$work/bin/legionbench" ./internal/dispatch/legion/legionbench/cmd
go test -c -tags farm -o "$work/bin/legionbench.test" ./internal/dispatch/legion/legionbench
nats_version=$(awk '$1 == "github.com/nats-io/nats-server/v2" { print $2; exit }' go.mod)
if [ -z "$nats_version" ]; then
  echo "FAIL r8 packages/envoy/go.mod requires no github.com/nats-io/nats-server/v2, so no NATS server is built"
  exit 1
fi
GOBIN="$work/bin" go install "github.com/nats-io/nats-server/v2@$nats_version"
mark "built legionbench and nats-server $("$work/bin/nats-server" --version)"

# The two trees. The probe is this head's (main's refuses a legion view), with its dependencies.
tree_of() {
  local commit=$1 name=$2
  if [ "$(git -C "$source_dir" rev-parse HEAD)" = "$(git -C "$source_dir" rev-parse "$commit^{commit}")" ]; then
    echo "$source_dir"
    return
  fi
  if [ ! -d "$work/$name-src" ]; then
    git -C "$source_dir" worktree add --detach "$work/$name-src" "$commit" >&2
  fi
  echo "$work/$name-src"
}
main_tree=$(tree_of "$R8_MAIN" main)
head_tree=$(tree_of "$R8_HEAD" head)
(cd "$head_tree" && bun install --frozen-lockfile --ignore-scripts >"$work/bun-install.log" 2>&1) || {
  echo "FAIL r8 bun install at this head failed; the end of its log:"
  tail -n 40 "$work/bun-install.log"
  exit 1
}
mark "installed the probe's dependencies at $R8_HEAD"

# start_server boots arm's server on database url with its output in log, and waits for /healthz.
# With a fourth argument, the URL of the arm's NATS server (start_nats), the server runs on it, as
# the boot the drive measures does; without one it runs with NATS off, as the boot that only
# migrates the database does, since no daemon is read before the load writes INFRA's setting.
server_pid=""
start_server() {
  local bin=$1 url=$2 log=$3 nats=${4:-} home=$work/home
  local nats_env=(DISPATCH_NATS_DISABLED=1)
  if [ -n "$nats" ]; then
    nats_env=(NATS_URLS="$nats")
  fi
  mkdir -p "$home"
  (cd "$home" && exec env -i PATH="$PATH" HOME="$home" XDG_CONFIG_HOME="$home" XDG_DATA_HOME="$home" \
    DATABASE_URL="$url" DISPATCH_AGENT_TOKEN="$token" DISPATCH_IDENTITY=header:X-Dispatch-User \
    DISPATCH_IDENTITY_HEADER_TRUSTED=1 DISPATCH_LISTEN_HOST=127.0.0.1 DISPATCH_PORT="$server_port" \
    DISPATCH_WEB_DIST="$work/web" "${nats_env[@]}" "${pprof_env[@]}" "$bin" >>"$log" 2>&1) &
  server_pid=$!
  pids+=("$server_pid")
  local started=$SECONDS
  until curl -fsS "http://127.0.0.1:$server_port/healthz" >/dev/null 2>&1; do
    if ! kill -0 "$server_pid" 2>/dev/null; then
      echo "FAIL r8 the server exited before it served; the end of $log:"
      tail -n 40 "$log"
      return 1
    fi
    if [ $((SECONDS - started)) -gt 900 ]; then
      echo "FAIL r8 the server did not serve within 900 s; the end of $log:"
      tail -n 40 "$log"
      return 1
    fi
    sleep 1
  done
  mark "server $server_pid serving after $((SECONDS - started)) s, logging to $log"
}

# start_nats starts arm name's NATS server with JetStream on 127.0.0.1, its store in a directory
# of the arm's own ($work/nats-<name>), so the stream and the durables the head's server binds are
# its own and none of main's, and waits for its monitoring port's /healthz, which answers once
# JetStream is ready. Its output goes to $work/nats-<name>.log.
nats_pid=""
start_nats() {
  local name=$1 log=$work/nats-$1.log
  "$work/bin/nats-server" -js -a 127.0.0.1 -p "$nats_port" -m "$nats_monitor_port" -sd "$work/nats-$name" >"$log" 2>&1 &
  nats_pid=$!
  pids+=("$nats_pid")
  local started=$SECONDS
  until curl -fsS "http://127.0.0.1:$nats_monitor_port/healthz" >/dev/null 2>&1; do
    if ! kill -0 "$nats_pid" 2>/dev/null || [ $((SECONDS - started)) -gt 60 ]; then
      echo "FAIL r8 arm=$name the NATS server did not serve JetStream within 60 s; the end of $log:"
      tail -n 40 "$log"
      return 1
    fi
    sleep 1
  done
  mark "nats-server $nats_pid serving JetStream on nats://127.0.0.1:$nats_port after $((SECONDS - started)) s, its store $work/nats-$name, logging to $log"
}

# stop_pid stops a process with SIGTERM and waits for it.
stop_pid() {
  local pid=$1
  kill -TERM "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
}

# stop_arm stops the arm's server, then its stand-in and its NATS server, each that runs, and
# forgets them, so the next arm binds their ports. arm calls it on every exit of arm_steps, an early
# `return 1` included.
standin_pid=""
stop_arm() {
  local pid
  for pid in "$server_pid" "$standin_pid" "$nats_pid"; do
    if [ -n "$pid" ]; then
      stop_pid "$pid"
    fi
  done
  server_pid="" standin_pid="" nats_pid=""
}

# profile_window profiles the CPU of arm name's server over the drive's window into
# $work/out/<name>.cpu.pprof, in the background beside the drive, whose output it reads in
# drive_log. The drive prints its `board cards=` line as its window opens, after the catch-up (at
# this head the facts consumer's whole read, which the profile so leaves out); the profile then
# lasts as long as the window's events and ends within the drive's 5 s of grace after them.
profile_window() {
  local name=$1 drive_log=$2
  until grep -q "^RESULT r8 arm=$name board cards=" "$drive_log"; do
    sleep 1
  done
  curl -fsS -o "$work/out/$name.cpu.pprof" "http://127.0.0.1:$pprof_port/debug/pprof/profile?seconds=$profile_seconds"
}

# arm runs one build's arm (arm_steps), then stops every process the arm started (stop_arm),
# whichever step the arm ended at, and answers the steps' exit status.
arm() {
  local rc=0
  arm_steps "$@" || rc=$?
  stop_arm
  mark "arm $1 exits $rc"
  return "$rc"
}

# arm_steps are one build's arm: build, migrate, load, stand-in, NATS, serve, drive, and at this
# head the facts read's measurement. It runs where its caller tests it (`arm_steps … || rc=$?`),
# which turns errexit off inside it, so every step that must stop the arm says so (`|| return 1`),
# and leaves the processes it started running for arm to stop.
arm_steps() {
  local name=$1 commit=$2 tree=$3 facts=$4
  local bin=$work/bin/server-$name db=r8_$name
  local url="postgres://postgres@127.0.0.1:$port/$db?sslmode=disable"
  (cd "$tree/packages/envoy" && go build -ldflags "-X main.buildCommit=$commit" -o "$bin" ./cmd/dispatch) || {
    echo "FAIL r8 arm=$name the server did not build at $commit"
    return 1
  }
  mark "built $name's server at $commit"
  psql "$admin_url" -Atc "create database $db" || return 1
  start_server "$bin" "$url" "$work/server-$name-migrate.log" || return 1
  stop_pid "$server_pid"
  server_pid=""
  mark "migrated $db by $name's server"
  "$work/bin/legionbench" load -database "$url" -state-url "http://127.0.0.1:$standin_port" -ten-times \
    -scale "${R8_SCALE:-1}" -seed "${R8_SEED:-5577}" -now "$now" || return 1
  psql "$url" -Atc "select 'RESULT r8 arm=$name size database=' || pg_size_pretty(pg_database_size(current_database()))"
  "$work/bin/legionbench" standin -database "$url" -listen "127.0.0.1:$standin_port" >"$work/standin-$name.log" 2>&1 &
  standin_pid=$!
  pids+=("$standin_pid")
  local waited=0
  until grep -q '^STANDIN serving' "$work/standin-$name.log"; do
    if ! kill -0 "$standin_pid" 2>/dev/null || [ "$waited" -gt 300 ]; then
      echo "FAIL r8 arm=$name the stand-in did not serve; its log:"
      cat "$work/standin-$name.log"
      return 1
    fi
    sleep 1
    waited=$((waited + 1))
  done
  sed 's/^/RESULT r8 arm='"$name"' /' "$work/standin-$name.log"
  start_nats "$name" || return 1
  start_server "$bin" "$url" "$work/server-$name.log" "nats://127.0.0.1:$nats_port" || return 1
  local facts_flags=()
  if [ "$facts" = 1 ]; then
    facts_flags=(-facts -pg-log "$pg_log")
  fi
  # The drive's output goes to drive_log as well as to this script's: profile_window watches it for
  # the window, and the record keeps it with the servers' logs.
  local rc=0 drive_log=$work/drive-$name.log profiler=""
  : >"$drive_log"
  if [ "$profile" = 1 ]; then
    profile_window "$name" "$drive_log" &
    profiler=$!
    pids+=("$profiler")
  fi
  "$work/bin/legionbench" drive -arm "$name" -url "http://127.0.0.1:$server_port" -token-file "$work/token" \
    -database "$url" -probe-dir "$head_tree/packages/dispatch" -server-log "$work/server-$name.log" \
    -out "$work/out" -rate "${R8_RATE:-0.26}" -duration "${R8_DURATION:-10m}" "${facts_flags[@]}" | tee "$drive_log" ||
    rc=${PIPESTATUS[0]}
  if [ -n "$profiler" ]; then
    if ! grep -q "^RESULT r8 arm=$name board cards=" "$drive_log"; then
      stop_pid "$profiler"
      mark "arm $name: the drive opened no window, so its server's CPU was not profiled"
    elif wait "$profiler" && [ -s "$work/out/$name.cpu.pprof" ]; then
      echo "RESULT r8 arm=$name cpu_profile=out/$name.cpu.pprof seconds=$profile_seconds bytes=$(wc -c <"$work/out/$name.cpu.pprof")"
    else
      mark "arm $name: the profile of its server's CPU over the window was not taken"
    fi
  fi
  if [ "$facts" = 1 ] && [ -s "$work/out/facts-read.json" ]; then
    LEGIONBENCH_DATABASE_URL="$url" LEGIONBENCH_FACTS_READ="$work/out/facts-read.json" \
      "$work/bin/legionbench.test" -test.run '^TestFarmBoardFactsRead$' -test.v -test.count=1 || rc=1
  fi
  return "$rc"
}

arm main "$R8_MAIN" "$main_tree" 0 || status=1
# main's database is read no more; its space goes to this head's load.
psql "$admin_url" -Atc 'drop database if exists r8_main' || true
arm head "$R8_HEAD" "$head_tree" "$head_facts" || status=1
if [ -s "$work/out/main-summary.json" ] && [ -s "$work/out/head-summary.json" ]; then
  "$work/bin/legionbench" compare -main "$work/out/main-summary.json" -head "$work/out/head-summary.json" || status=1
else
  echo "FAIL r8 an arm wrote no summary, so the two medians are not compared"
  status=1
fi
mark "r8.sh exits $status"
exit "$status"
