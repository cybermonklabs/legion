#!/usr/bin/env bash
# R8's Job (dispatch://INFRA-5577, check k14; spec Design, "How R8 is measured"), whole, in one pod:
# a Postgres 18 of its own, then `main`'s tip and then this head, one after the other. For each
# build it builds the server stamped with its commit, migrates a database of its own by booting the
# server once, loads INFRA's history at ten times the run volume (legionbench load -ten-times),
# starts the daemon stand-in with 1,000 cards of the loaded issues, boots the server on the loaded
# database, and drives one `legion` view (legionbench drive): this head's delivery-live-probe.ts for
# both builds, issue events through the API at 0.26 a second for 10 minutes. At this head the drive
# first waits for the Legion facts consumer to read INFRA whole, then judges each facts change's
# push and the board's body, and takes the board's facts read, whose plan and median time
# TestFarmBoardFactsRead measures. Last, legionbench compare holds this head's median recompute to
# twice main's. r8.yaml runs it on the build farm.
#
# Every measurement prints a RESULT line and every miss a FAIL line; the script exits 1 when any
# step failed.
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
#   R8_PGPORT (55432), R8_SHARED_BUFFERS (2GB), PGBIN (the newest /usr/lib/postgresql/*/bin)
#   R8_PROFILE (unset): seconds of CPU profile to take of each arm's server during its drive,
#                R8_PROFILE_AFTER (60) seconds into it; the two profiles are compared at the end
#                (go tool pprof), the hub's recompute (deliveryHub.recompute) on its own and whole
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
# Each server is stamped with its full commit, which /healthz answers and the facts consumer records.
R8_MAIN=$(git -C "$source_dir" rev-parse "$R8_MAIN^{commit}")
R8_HEAD=$(git -C "$source_dir" rev-parse "$R8_HEAD^{commit}")
server_port=58111
pprof_port=58112
standin_port=13370
# The agent token of both servers, which the probe reads from $work/token: drawn for this run.
token=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
if [ -z "${PGBIN:-}" ]; then
  PGBIN=$(ls -d /usr/lib/postgresql/*/bin | sort -V | tail -n 1)
fi
mkdir -p "$work/bin" "$work/pglog" "$work/sock" "$work/out" "$work/web"
printf '%s\n' "$token" >"$work/token"
mark() { echo "MARK $(date -u +%FT%TZ) $*"; }
status=0
mark "r8.sh: source $source_dir at $(git -C "$source_dir" rev-parse HEAD), main $R8_MAIN, head $R8_HEAD, scale ${R8_SCALE:-1}, now $now, head facts $head_facts, node ${NODE_NAME:-unknown}"
echo "RESULT r8 main=$R8_MAIN head=$R8_HEAD node=${NODE_NAME:-unknown} scale=${R8_SCALE:-1} rate=${R8_RATE:-0.26} duration=${R8_DURATION:-10m} cards=1000 ten_times=1"

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

# The tools, built from the checkout this script is in: the loader, stand-in and drive, and the
# test binary of the facts read's measurement.
cd "$source_dir/packages/envoy"
go build -o "$work/bin/legionbench" ./internal/dispatch/legion/legionbench/cmd
go test -c -tags farm -o "$work/bin/legionbench.test" ./internal/dispatch/legion/legionbench
mark "built legionbench"

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
server_pid=""
start_server() {
  local bin=$1 url=$2 log=$3 home=$work/home
  mkdir -p "$home"
  (cd "$home" && exec env -i PATH="$PATH" HOME="$home" XDG_CONFIG_HOME="$home" XDG_DATA_HOME="$home" \
    DATABASE_URL="$url" DISPATCH_AGENT_TOKEN="$token" DISPATCH_IDENTITY=header:X-Dispatch-User \
    DISPATCH_IDENTITY_HEADER_TRUSTED=1 DISPATCH_LISTEN_HOST=127.0.0.1 DISPATCH_PORT="$server_port" \
    DISPATCH_WEB_DIST="$work/web" DISPATCH_NATS_DISABLED=1 ${R8_PROFILE:+DISPATCH_PPROF_PORT=$pprof_port} "$bin" >>"$log" 2>&1) &
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

# stop_pid stops a process with SIGTERM and waits for it.
stop_pid() {
  local pid=$1
  kill -TERM "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
}

# arm runs one build's arm: build, migrate, load, stand-in, serve, drive, and at this head the facts
# read's measurement. It runs where its caller tests it (`arm … || status=1`), which turns errexit off
# inside it, so every step that must stop the arm says so (`|| return 1`).
arm() {
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
  mark "migrated $db by $name's server"
  "$work/bin/legionbench" load -database "$url" -state-url "http://127.0.0.1:$standin_port" -ten-times \
    -scale "${R8_SCALE:-1}" -seed "${R8_SEED:-5577}" -now "$now" || return 1
  psql "$url" -Atc "select 'RESULT r8 arm=$name size database=' || pg_size_pretty(pg_database_size(current_database()))"
  "$work/bin/legionbench" standin -database "$url" -listen "127.0.0.1:$standin_port" >"$work/standin-$name.log" 2>&1 &
  local standin_pid=$!
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
  start_server "$bin" "$url" "$work/server-$name.log" || return 1
  local facts_flags=()
  if [ "$facts" = 1 ]; then
    facts_flags=(-facts -pg-log "$pg_log")
  fi
  local rc=0 profile_pid=""
  if [ -n "${R8_PROFILE:-}" ]; then
    (sleep "${R8_PROFILE_AFTER:-60}" && curl -fsS -o "$work/out/$name.cpu.pprof" \
      "http://127.0.0.1:$pprof_port/debug/pprof/profile?seconds=$R8_PROFILE" && mark "took $name's profile") &
    profile_pid=$!
  fi
  "$work/bin/legionbench" drive -arm "$name" -url "http://127.0.0.1:$server_port" -token-file "$work/token" \
    -database "$url" -probe-dir "$head_tree/packages/dispatch" -server-log "$work/server-$name.log" \
    -out "$work/out" -rate "${R8_RATE:-0.26}" -duration "${R8_DURATION:-10m}" "${facts_flags[@]}" || rc=$?
  if [ -n "$profile_pid" ]; then
    wait "$profile_pid" || { echo "FAIL r8 arm=$name the CPU profile was not taken"; rc=1; }
  fi
  if [ "$facts" = 1 ] && [ -s "$work/out/facts-read.json" ]; then
    LEGIONBENCH_DATABASE_URL="$url" LEGIONBENCH_FACTS_READ="$work/out/facts-read.json" \
      "$work/bin/legionbench.test" -test.run '^TestFarmBoardFactsRead$' -test.v -test.count=1 || rc=1
  fi
  stop_pid "$server_pid"
  stop_pid "$standin_pid"
  mark "arm $name exits $rc"
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
# The profiles: each arm's top functions inside the hub's recompute, by their own time and with
# what they call, and this head's against main's (positive is this head's extra time). An empty
# profile prints UNKNOWN rather than a clean comparison.
if [ -n "${R8_PROFILE:-}" ]; then
  focus='deliveryHub..recompute$'
  for name in main head; do
    profile=$work/out/$name.cpu.pprof
    if [ ! -s "$profile" ]; then
      echo "FAIL r8 PROFILE UNKNOWN: $name's profile is empty or missing"
      status=1
      continue
    fi
    echo "PROFILE $name whole server, flat:"
    go tool pprof -top -nodecount=25 "$work/bin/server-$name" "$profile" 2>&1 | sed 's/^/PROFILE '"$name"' all /'
    echo "PROFILE $name inside deliveryHub.recompute, cumulative:"
    go tool pprof -top -cum -nodecount=45 -focus="$focus" "$work/bin/server-$name" "$profile" 2>&1 | sed 's/^/PROFILE '"$name"' recompute /'
  done
  if [ -s "$work/out/main.cpu.pprof" ] && [ -s "$work/out/head.cpu.pprof" ]; then
    echo "PROFILE diff head - main inside deliveryHub.recompute, cumulative:"
    go tool pprof -top -cum -nodecount=45 -focus="$focus" -diff_base="$work/out/main.cpu.pprof" \
      "$work/bin/server-head" "$work/out/head.cpu.pprof" 2>&1 | sed 's/^/PROFILE diff /'
  fi
fi
mark "r8.sh exits $status"
exit "$status"
