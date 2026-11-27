#!/usr/bin/env bash
# The eval registry's measurement Job (dispatch://INFRA-2916, T15), whole: a Postgres 18 of its own
# with pg_stat_statements preloaded and the statement log in jsonlog, the server, the generator and
# the instrument built from the checkout, the database migrated by the server, the evalbench
# fixture loaded, and run.sh's measurement. job.yaml runs it on the build farm; it runs the same in
# a pod at a small EVALBENCH_SCALE.
#
# Postgres and the server each run under a supervisor (supervise.sh): in a container of their own
# where the pod runs them so (job.yaml, which names their directory in EVALBENCH_CONTROL and holds
# each container to the size it gives it), else beside this script, which then starts both and
# holds their driver lock. Before the measurement and again at its end the run prints its shape as
# a SHAPE line. It reads gating where each reading the pod itself makes holds the architect's
# ruling for a timed run (dispatch://INFRA-2916/comment/59ca49ea-68ec-4b3b-940e-d8a110b91b30): the
# pod runs on the compute class EVALBENCH_GATING_CLASS names and under no runtime class; the
# server's requests are its limits, 9 CPU and 21Gi (EVALBENCH_PODINFO); and at every start of the
# server its own cgroup read cpu.max "900000 100000" and memory.max 22548578304 and its Go runtime
# chose GOMAXPROCS 9 (run.sh's SERVER_START lines and server-starts.tsv; the trace count per run
# is recorded beside them). Otherwise it reads record, with each reading that failed in its
# reasons. The ruling's third reading, the node's machine family, is not the pod's to make: the
# family is the node's label (node.kubernetes.io/instance-type, n4d-* or c3d-* for a run that
# gates), the pod has no API access to read it, and GKE refuses a pod that asks for a family or
# a machine type beside a custom ComputeClass. So the line prints family=unread and the node, and
# its reader reads the node's label beside the log (kubectl get node <node> -L
# node.kubernetes.io/instance-type): a run on any other family is a record, whatever its SHAPE
# word. The line also prints the machine's CPUs, CPU model and memory, each container's requests
# and limits as the API server gave them, and Postgres's own cgroup, outside the server's limit
# (supervise.sh's container file).
#
#   job.sh head    this head's first paint, EVALBENCH_PAINT_RUNS times (8), each after a fresh
#                  start of the server, with the median and the maximum of the runs, and k2, k13
#                  and k16 once (run.sh's header says which run each belongs to); then, with a
#                  failure day added to the history, the Delivery timeline's first paint once more,
#                  as many times as EVALBENCH_PAINT_RUNS names, once where it names none
#                  (run.sh's failure-day)
#   job.sh base    the base's first paint (k8's and k13's negative controls), the base being main at
#                  the commit this head merges: envoy-dispatch built at EVALBENCH_BASE, its
#                  database migrated by it, and the generator's -base history, written by this
#                  head's generator; then, with the same failure day added to the history, the
#                  base's Delivery page once more (run.sh's base-failure-day). Its paint is
#                  measured once unless EVALBENCH_PAINT_RUNS names more, which sets the head's
#                  timeline reads beside the base's the same number of times
#
# The failure day is INFRA's worst day of deploy failures (evalbench/failureday.go says where it
# was read): 36 failed deploy runs on main with 45 failed jobs across them, on the UTC day
# EVALBENCH_FAILURE_DAY days before the history's end, inside both of the timeline's windows, on
# top of the history's own runs of that day. The timeline reads each deploy run's failed jobs, so
# the day it reads the most is that day.
#
# Environment:
#   EVALBENCH_WORK         scratch for everything the run writes (required)
#   EVALBENCH_SOURCE       the checkout (the repository this script is in)
#   EVALBENCH_COMMIT       the commit the head server is stamped with (the checkout's HEAD)
#   EVALBENCH_BASE         the base: main at the commit this head merges, `git merge-base <head>
#                          origin/main`; required by base mode
#   EVALBENCH_BASE_SOURCE  a tree of the base; by default a worktree of the checkout at
#                          EVALBENCH_BASE, made under EVALBENCH_WORK
#   EVALBENCH_SCALE (1), EVALBENCH_SEED (2916), EVALBENCH_TEN_TIMES (0), EVALBENCH_NOW (the current
#                          UTC midnight): the generator's -scale, -seed, -ten-times and -now; the
#                          paint's windows end on EVALBENCH_NOW's day too (run.sh)
#   EVALBENCH_FAILURE_DAY (7; 0 adds none), EVALBENCH_FAILURE_DAY_RUNS (36),
#   EVALBENCH_FAILURE_DAY_JOBS (45): the generator's -failure-day, -failure-day-runs and
#                          -failure-day-jobs, at head and at the base
#   EVALBENCH_PGPORT (55432), EVALBENCH_SHARED_BUFFERS (1GB), PGBIN (the newest
#                          /usr/lib/postgresql/*/bin)
#   EVALBENCH_NODE         the node the run is on (job.yaml sets it from the pod's spec.nodeName),
#                          printed with the machine's CPUs and memory and in the SHAPE line
#   EVALBENCH_CONTROL      the supervisors' directory (supervise.sh) where the pod runs them in
#                          containers of their own and holds their driver lock (job.yaml); unset,
#                          job.sh runs them beside itself, under EVALBENCH_WORK
#   EVALBENCH_PODINFO      a directory of each container's requests and limits, a file a value
#                          (<server|postgres|driver>.<requests|limits>.<cpu|memory>, in millicores and
#                          MiB: job.yaml's downward API), which the SHAPE line prints and judges
#   EVALBENCH_COMPUTE_CLASS, EVALBENCH_GATING_CLASS, EVALBENCH_RUNTIME_CLASS: the compute class the
#                          pod runs on, the one a run is judged on (none: no run is), and the pod's
#                          runtime class (none: the machine's own), which the SHAPE line names and
#                          judges
#   run.sh's EVALBENCH_PORT, EVALBENCH_CATCHUP, EVALBENCH_QUIET, EVALBENCH_CATALOG and
#   EVALBENCH_PAINT_RUNS pass through.
set -euo pipefail

mode=${1:-}
case "$mode" in head | base) ;; *)
  echo "usage: job.sh head|base" >&2
  exit 2
  ;;
esac
: "${EVALBENCH_WORK:?EVALBENCH_WORK must name a scratch directory for the run}"
if [ "$mode" = base ] && [ -z "${EVALBENCH_BASE:-}" ]; then
  echo "job.sh: base mode needs EVALBENCH_BASE, the base: main at the commit this head merges (git merge-base <head> origin/main)" >&2
  exit 2
fi
here=$(cd "$(dirname "$0")" && pwd)
source_dir=${EVALBENCH_SOURCE:-$(cd "$here/../../../../../.." && pwd)}
# Absolute, since the supervisors run what job.sh and run.sh name from directories of their own.
mkdir -p "$EVALBENCH_WORK"
work=$(cd "$EVALBENCH_WORK" && pwd)
base=${EVALBENCH_BASE:-}
commit=${EVALBENCH_COMMIT:-$(git -C "$source_dir" rev-parse HEAD)}
port=${EVALBENCH_PGPORT:-55432}
now=${EVALBENCH_NOW:-$(date -u +%Y-%m-%dT00:00:00Z)}
if [ -z "${PGBIN:-}" ]; then
  PGBIN=$(ls -d /usr/lib/postgresql/*/bin | sort -V | tail -n 1)
fi
mkdir -p "$work/bin" "$work/pglog" "$work/sock"
mark() { echo "MARK $(date -u +%FT%TZ) $*"; }
mark "job.sh $mode: source $source_dir at $commit, scale ${EVALBENCH_SCALE:-1}, ten times ${EVALBENCH_TEN_TIMES:-0}, now $now"
# The machine the times are measured on: k8 and k16 hold the reads to their budgets on the node the
# Job runs on, so the log names it.
mark "machine: node ${EVALBENCH_NODE:-unnamed}, $(nproc) CPUs, $(awk -F': *' '/^model name/ { print $2; exit }' /proc/cpuinfo), $(awk '/^MemTotal/ { printf "%.1f GiB", $2 / 1048576 }' /proc/meminfo) of memory"

# The supervisors of Postgres and of the server: the pod's, or two started here, which serve this
# script for as long as it holds their driver lock, so they end once it exits (supervise.sh). Each
# is started without the lock's descriptor, which would hold the lock for as long as it runs.
control=${EVALBENCH_CONTROL:-}
supervisors=()
if [ -z "$control" ]; then
  control=$work/control
  mkdir -p "$control"
  exec {held}>"$control/driver.lock.new"
  flock "$held"
  mv "$control/driver.lock.new" "$control/driver.lock"
  for name in postgres server; do
    "$here/supervise.sh" serve "$control/$name" {held}>&- &
    supervisors+=("$!")
  done
fi
export EVALBENCH_CONTROL=$control

# The server a timed run measures (the header's ruling): production's 9 CPU and 21Gi, as its
# requests and limits read in millicores and MiB, as its own cgroup's cpu.max and memory.max, and
# as the GOMAXPROCS a Go runtime takes from that CPU limit.
ruled_cpu_m=9000 ruled_memory_mi=21504 ruled_cpu_max="900000 100000" ruled_memory_max=22548578304 ruled_gomaxprocs=9

# tally prints the associative array named $1, each value it counts with its count, as
# [value:count,...] in the values' order.
tally() {
  local -n counts=$1
  local value out=""
  while IFS= read -r value; do
    [ -z "$value" ] || out+=",$value:${counts[$value]}"
  done < <(printf '%s\n' "${!counts[@]}" | sort)
  echo "[${out#,}]"
}

# shape prints the run's SHAPE line (the header says what it holds and when it reads gating), from
# the readings each start and stop of the server appended to server-starts.tsv (run.sh).
shape() {
  local reasons=() resources="" container field value kind first second third word=gating starts=0 stops=0
  local -A got=() gomaxprocs=() cpu_max=() memory_max=() traces=()
  for container in server postgres driver; do
    for field in requests.cpu limits.cpu requests.memory limits.memory; do
      value=none
      if [ -n "${EVALBENCH_PODINFO:-}" ] && [ -r "$EVALBENCH_PODINFO/$container.$field" ]; then
        value=$(<"$EVALBENCH_PODINFO/$container.$field")
      fi
      got[$container.$field]=$value
    done
    resources+=" ${container}=cpu:${got[$container.requests.cpu]}m/${got[$container.limits.cpu]}m,memory:${got[$container.requests.memory]}Mi/${got[$container.limits.memory]}Mi"
  done
  if [ -r "$EVALBENCH_WORK/server-starts.tsv" ]; then
    # start <log> <GOMAXPROCS> <cpu.max> <memory.max>, or stop <log> <scheduler traces>.
    while IFS=$'\t' read -r kind _ first second third; do
      case "$kind" in
        start)
          starts=$((starts + 1))
          gomaxprocs[${first:-unread}]=$((${gomaxprocs[${first:-unread}]:-0} + 1))
          cpu_max[${second:-unread}]=$((${cpu_max[${second:-unread}]:-0} + 1))
          memory_max[${third:-unread}]=$((${memory_max[${third:-unread}]:-0} + 1))
          ;;
        stop)
          stops=$((stops + 1))
          traces[${first:-unread}]=$((${traces[${first:-unread}]:-0} + 1))
          ;;
      esac
    done <"$EVALBENCH_WORK/server-starts.tsv"
  fi
  if [ -z "${EVALBENCH_GATING_CLASS:-}" ]; then
    reasons+=("no gating class is named")
  elif [ "${EVALBENCH_COMPUTE_CLASS:-}" != "$EVALBENCH_GATING_CLASS" ]; then
    reasons+=("the pod's compute class is not the gating class")
  fi
  if [ "${EVALBENCH_RUNTIME_CLASS:-none}" != none ]; then
    reasons+=("the pod runs under the runtime class $EVALBENCH_RUNTIME_CLASS")
  fi
  case " ${got[server.requests.cpu]} ${got[server.limits.cpu]} ${got[server.requests.memory]} ${got[server.limits.memory]} " in
    *" none "*) reasons+=("the server's requests and limits were not read (EVALBENCH_PODINFO)") ;;
    *)
      if [ "${got[server.requests.cpu]}" != "${got[server.limits.cpu]}" ] || [ "${got[server.requests.memory]}" != "${got[server.limits.memory]}" ]; then
        reasons+=("the server's requests are not its limits")
      fi
      if [ "${got[server.limits.cpu]}" != "$ruled_cpu_m" ] || [ "${got[server.limits.memory]}" != "$ruled_memory_mi" ]; then
        reasons+=("the server's limits are not ${ruled_cpu_m}m and ${ruled_memory_mi}Mi")
      fi
      ;;
  esac
  [ "$starts" -gt 0 ] || reasons+=("no start of the server was read (server-starts.tsv)")
  for value in "${!gomaxprocs[@]}"; do
    [ "$value" = "$ruled_gomaxprocs" ] || reasons+=("${gomaxprocs[$value]} of $starts server starts read GOMAXPROCS $value, not $ruled_gomaxprocs")
  done
  for value in "${!cpu_max[@]}"; do
    [ "$value" = "$ruled_cpu_max" ] || reasons+=("${cpu_max[$value]} of $starts server starts read cpu.max [$value], not [$ruled_cpu_max]")
  done
  for value in "${!memory_max[@]}"; do
    [ "$value" = "$ruled_memory_max" ] || reasons+=("${memory_max[$value]} of $starts server starts read memory.max $value, not $ruled_memory_max")
  done
  for value in "${!traces[@]}"; do
    [ "$value" -ge 1 ] 2>/dev/null || reasons+=("${traces[$value]} of $stops runs of the server printed $value scheduler traces, want at least 1")
  done
  [ "$stops" -eq "$starts" ] || reasons+=("$((starts - stops)) of $starts server starts were read to no stop")
  [ "${#reasons[@]}" -eq 0 ] || word=record
  echo "SHAPE $word family=unread node=${EVALBENCH_NODE:-unnamed} compute_class=${EVALBENCH_COMPUTE_CLASS:-none}" \
    "gating_class=${EVALBENCH_GATING_CLASS:-none} runtime_class=${EVALBENCH_RUNTIME_CLASS:-none} server_starts=$starts" \
    "server_gomaxprocs=$(tally gomaxprocs) server_cpu_max=$(tally cpu_max) server_memory_max=$(tally memory_max)" \
    "server_schedtraces=$(tally traces) machine_cpus=$(grep -c '^processor' /proc/cpuinfo)" \
    "machine_memory_gib=$(awk '/^MemTotal/ { printf "%.1f", $2 / 1048576 }' /proc/meminfo)" \
    "cpu_model=\"$(awk -F': *' '/^model name/ { print $2; exit }' /proc/cpuinfo)\"$resources" \
    "postgres_cgroup=[$(cat "$control/postgres/container" 2>/dev/null || echo unread)]" \
    "reasons=\"$(IFS=';'; echo "${reasons[*]:-none}")\""
}

# Postgres, its data on the run's own volume. pg_stat_statements is the instrument (track_utility
# off, so a transaction's begin and commit are not statements of their own, and track_planning on,
# so a statement's planning counts in its database time), and the jsonlog file holds each
# statement the instrument turns the log on for, with its query id and parameters. Its timestamps
# are in UTC (log_timezone), the zone the instrument reads them in: Go reads a zone abbreviation it
# does not know at offset 0, so a log in another zone would shift each statement away from the read
# it ran in (capture.go's readStatementLog). The migration and the load run with fsync, full-page
# writes and synchronous commit off (postgresql.auto.conf, which a reload reads), which only speeds
# them; the measurement runs with all three back on, as a served database runs, since the writes it
# times (a stale cut's first paint, a move's refold) wait on them. wal_level stays minimal, which
# a restart alone changes and which spares only the WAL of a table written in the transaction that
# made it. Each pg_ctl runs under Postgres's supervisor, so the postmaster it starts runs in
# Postgres's container. The trap is set before Postgres starts, so every exit from here on stops it,
# and then the supervisors started here.
pg_ctl() {
  local code=0
  "$here/supervise.sh" exec "$control/postgres" "$work/pg-ctl.log" "$PGBIN/pg_ctl" --pgdata "$work/pg" "$@" || code=$?
  if [ "$code" -ne 0 ]; then
    echo "job.sh: pg_ctl ${*: -1} exited $code; the end of $work/pg-ctl.log:"
    tail -n 20 "$work/pg-ctl.log" || true
  fi
  return "$code"
}
stop_postgres() {
  pg_ctl --wait --mode fast stop >/dev/null 2>&1 || true
  if [ "${#supervisors[@]}" -gt 0 ]; then
    exec {held}>&-
    wait "${supervisors[@]}" || true
  fi
}
trap stop_postgres EXIT
"$PGBIN/initdb" --pgdata "$work/pg" --username postgres --auth=trust --encoding=UTF8 --no-sync >"$work/initdb.log"
pg_ctl --log "$work/pg-server.log" --wait --timeout 300 --options "-p $port -k $work/sock \
  -c listen_addresses=127.0.0.1 -c max_connections=200 -c shared_buffers=${EVALBENCH_SHARED_BUFFERS:-1GB} \
  -c shared_preload_libraries=pg_stat_statements -c pg_stat_statements.track=top -c pg_stat_statements.track_utility=off \
  -c pg_stat_statements.track_planning=on \
  -c compute_query_id=on -c logging_collector=on -c log_destination=jsonlog -c log_directory=$work/pglog \
  -c log_filename=postgres.log -c log_rotation_age=0 -c log_rotation_size=0 -c log_timezone=UTC \
  -c wal_level=minimal -c max_wal_senders=0 \
  -c max_wal_size=4GB -c huge_pages=off -c dynamic_shared_memory_type=mmap -c jit=off" start
database_url="postgres://postgres@127.0.0.1:$port/evalbench?sslmode=disable"
psql "postgres://postgres@127.0.0.1:$port/postgres" -Atc 'select version()'
psql "postgres://postgres@127.0.0.1:$port/postgres" -Atc 'create database evalbench'
# durability sets fsync, full_page_writes and synchronous_commit on or off and prints them, each
# ALTER SYSTEM a statement of its own, since none runs in a transaction block.
durability() {
  local change=(-c "alter system set fsync = off" -c "alter system set full_page_writes = off" -c "alter system set synchronous_commit = off")
  if [ "$1" = on ]; then
    change=(-c "alter system reset fsync" -c "alter system reset full_page_writes" -c "alter system reset synchronous_commit")
  fi
  psql "postgres://postgres@127.0.0.1:$port/postgres" -Atq "${change[@]}" -c "select pg_reload_conf()"
  if [ "$1" = on ]; then
    psql "postgres://postgres@127.0.0.1:$port/postgres" -Atq -c "checkpoint"
  fi
  mark "durability $1: $(psql "postgres://postgres@127.0.0.1:$port/postgres" -At -c "select string_agg(name || '=' || setting, ' ' order by name) from pg_settings where name in ('fsync', 'full_page_writes', 'synchronous_commit')")"
}
durability off

# The generator and the instrument are this head's in both modes; the server is the mode's.
cd "$source_dir/packages/envoy"
go build -o "$work/bin/evalbench" ./internal/dispatch/evals/evalbench/cmd
go build -o "$work/bin/measure" ./internal/dispatch/evals/evalbench/measure
server_source=$source_dir
stamp=$commit
load_flags=()
if [ "$mode" = base ]; then
  server_source=${EVALBENCH_BASE_SOURCE:-$work/base-src}
  if [ -z "${EVALBENCH_BASE_SOURCE:-}" ]; then
    git -C "$source_dir" worktree add --detach "$server_source" "$base"
  fi
  stamp=$base
  load_flags=(-base)
fi
(cd "$server_source/packages/envoy" && go build -ldflags "-X main.buildCommit=$stamp" -o "$work/bin/envoy-dispatch" ./cmd/dispatch)
mark "built the generator, the instrument and envoy-dispatch at $stamp"

export EVALBENCH_WORK=$work/run EVALBENCH_MEASURE=$work/bin/measure EVALBENCH_PG_LOG=$work/pglog/postgres.json
export EVALBENCH_SOURCE=$server_source EVALBENCH_NOW=$now
EVALBENCH_PG_RESTART=$(printf '%q ' "$here/supervise.sh" exec "$control/postgres" "$work/pg-ctl.log" "$PGBIN/pg_ctl" \
  --pgdata "$work/pg" --log "$work/pg-server.log" --wait --timeout 300 --mode fast restart)
export EVALBENCH_PG_RESTART
mkdir -p "$EVALBENCH_WORK"
# Each start and stop of the server appends its readings here (run.sh), which shape judges.
: >"$EVALBENCH_WORK/server-starts.tsv"
"$here/run.sh" migrate "$database_url" "$work/bin/envoy-dispatch"
mark "migrated by the server"
if [ "${EVALBENCH_TEN_TIMES:-0}" = 1 ]; then
  load_flags+=(-ten-times)
fi
"$work/bin/evalbench" -database "$database_url" -built-by "$stamp" -seed "${EVALBENCH_SEED:-2916}" \
  -scale "${EVALBENCH_SCALE:-1}" -now "$now" "${load_flags[@]}"
# sizes prints the database's size and its largest tables', $1 naming when.
sizes() {
  psql "$database_url" -Atc "
    select 'RESULT size when=$1 database=' || pg_size_pretty(pg_database_size(current_database()));
    select 'RESULT size when=$1 ' || case relkind when 'i' then 'index' else 'table' end || '=' || relname || ' rows_estimate=' || reltuples::bigint || ' total=' || pg_size_pretty(pg_total_relation_size(oid))
    from pg_class where (relkind = 'r' and relname in ('events', 'issues', 'delivery_run_jobs', 'eval_readings', 'eval_days', 'issue_facts', 'session_titles'))
      or (relkind = 'i' and relname in ('delivery_run_jobs_failed', 'delivery_run_jobs_named'))
    order by relname"
}
sizes loaded
durability on
mark "loaded"
shape
status=0
"$here/run.sh" measure "$database_url" "$work/bin/envoy-dispatch" "$mode" || status=$?
sizes measured
failure_day=${EVALBENCH_FAILURE_DAY:-7}
if [ "$failure_day" != 0 ]; then
  # The failure day on top of the history just measured, at its scale and from its seed, and the
  # Delivery page's first paint over it: the same windows, one database, before and after the day.
  failure_mode=failure-day
  if [ "$mode" = base ]; then
    failure_mode=base-failure-day
  fi
  if "$work/bin/evalbench" -database "$database_url" -seed "${EVALBENCH_SEED:-2916}" -scale "${EVALBENCH_SCALE:-1}" \
    -now "$now" -failure-day "$failure_day" -failure-day-runs "${EVALBENCH_FAILURE_DAY_RUNS:-36}" \
    -failure-day-jobs "${EVALBENCH_FAILURE_DAY_JOBS:-45}"; then
    mark "added the failure day"
    "$here/run.sh" measure "$database_url" "$work/bin/envoy-dispatch" "$failure_mode" || status=$?
    sizes failure-day
  else
    echo "FAIL job.sh: the failure day was not added, so the timeline over it is not measured"
    status=1
  fi
fi
shape
mark "job.sh $mode exits $status"
exit "$status"
