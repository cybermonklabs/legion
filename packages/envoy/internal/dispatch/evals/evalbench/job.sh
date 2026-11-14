#!/usr/bin/env bash
# The eval registry's measurement Job (dispatch://INFRA-2916, T15), whole: a Postgres 18 of its own
# with pg_stat_statements preloaded and the statement log in jsonlog, the server, the generator and
# the instrument built from the checkout, the database migrated by the server, the evalbench
# fixture loaded, and run.sh's measurement. job.yaml runs it on the build farm; it runs the same in
# a pod at a small EVALBENCH_SCALE.
#
#   job.sh head    this head's first paint, k2, k13 and k16
#   job.sh base    the base head's first paint (k8's and k13's negative controls): envoy-dispatch
#                  built at EVALBENCH_BASE, its database migrated by it, and the generator's -base
#                  history, written by this head's generator
#
# Environment:
#   EVALBENCH_WORK         scratch for everything the run writes (required)
#   EVALBENCH_SOURCE       the checkout (the repository this script is in)
#   EVALBENCH_COMMIT       the commit the head server is stamped with (the checkout's HEAD)
#   EVALBENCH_BASE         the base head (02a7b3c67eefaa5c3e7d724c794423cc6b89da64)
#   EVALBENCH_BASE_SOURCE  a tree of the base head; by default a worktree of the checkout at
#                          EVALBENCH_BASE, made under EVALBENCH_WORK
#   EVALBENCH_SCALE (1), EVALBENCH_SEED (2916), EVALBENCH_TEN_TIMES (0), EVALBENCH_NOW (the current
#                          UTC midnight): the generator's -scale, -seed, -ten-times and -now
#   EVALBENCH_PGPORT (55432), EVALBENCH_SHARED_BUFFERS (1GB), PGBIN (the newest
#                          /usr/lib/postgresql/*/bin)
#   run.sh's EVALBENCH_PORT, EVALBENCH_CATCHUP, EVALBENCH_QUIET and EVALBENCH_CATALOG pass through.
set -euo pipefail

mode=${1:-}
case "$mode" in head | base) ;; *)
  echo "usage: job.sh head|base" >&2
  exit 2
  ;;
esac
: "${EVALBENCH_WORK:?EVALBENCH_WORK must name a scratch directory for the run}"
here=$(cd "$(dirname "$0")" && pwd)
source_dir=${EVALBENCH_SOURCE:-$(cd "$here/../../../../../.." && pwd)}
work=$EVALBENCH_WORK
base=${EVALBENCH_BASE:-02a7b3c67eefaa5c3e7d724c794423cc6b89da64}
commit=${EVALBENCH_COMMIT:-$(git -C "$source_dir" rev-parse HEAD)}
port=${EVALBENCH_PGPORT:-55432}
now=${EVALBENCH_NOW:-$(date -u +%Y-%m-%dT00:00:00Z)}
if [ -z "${PGBIN:-}" ]; then
  PGBIN=$(ls -d /usr/lib/postgresql/*/bin | sort -V | tail -n 1)
fi
mkdir -p "$work/bin" "$work/pglog" "$work/sock"
mark() { echo "MARK $(date -u +%FT%TZ) $*"; }
mark "job.sh $mode: source $source_dir at $commit, scale ${EVALBENCH_SCALE:-1}, ten times ${EVALBENCH_TEN_TIMES:-0}, now $now"

# Postgres, its data on the run's own volume. pg_stat_statements is the instrument (track_utility
# off, so a transaction's begin and commit are not statements of their own), and the jsonlog file
# holds each statement the instrument turns the log on for, with its query id and parameters. Its
# timestamps are in UTC (log_timezone), the zone the instrument reads them in: Go reads a zone
# abbreviation it does not know at offset 0, so a log in another zone would shift each statement
# away from the read it ran in (capture.go's readStatementLog). The durability settings only speed
# the load; nothing the measurement reads depends on them. The trap is set before Postgres starts,
# so every exit from here on stops it.
stop_postgres() { "$PGBIN/pg_ctl" --pgdata "$work/pg" --wait --mode fast stop >/dev/null 2>&1 || true; }
trap stop_postgres EXIT
"$PGBIN/initdb" --pgdata "$work/pg" --username postgres --auth=trust --encoding=UTF8 --no-sync >"$work/initdb.log"
"$PGBIN/pg_ctl" --pgdata "$work/pg" --log "$work/pg-server.log" --wait --timeout 300 --options "-p $port -k $work/sock \
  -c listen_addresses=127.0.0.1 -c max_connections=200 -c shared_buffers=${EVALBENCH_SHARED_BUFFERS:-1GB} \
  -c shared_preload_libraries=pg_stat_statements -c pg_stat_statements.track=top -c pg_stat_statements.track_utility=off \
  -c compute_query_id=on -c logging_collector=on -c log_destination=jsonlog -c log_directory=$work/pglog \
  -c log_filename=postgres.log -c log_rotation_age=0 -c log_rotation_size=0 -c log_timezone=UTC \
  -c fsync=off -c full_page_writes=off -c synchronous_commit=off -c wal_level=minimal -c max_wal_senders=0 \
  -c max_wal_size=4GB -c huge_pages=off -c dynamic_shared_memory_type=mmap -c jit=off" start
database_url="postgres://postgres@127.0.0.1:$port/evalbench?sslmode=disable"
psql "postgres://postgres@127.0.0.1:$port/postgres" -Atc 'select version()'
psql "postgres://postgres@127.0.0.1:$port/postgres" -Atc 'create database evalbench'

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
export EVALBENCH_SOURCE=$server_source
export EVALBENCH_PG_RESTART="'$PGBIN/pg_ctl' --pgdata '$work/pg' --log '$work/pg-server.log' --wait --timeout 300 --mode fast restart"
mkdir -p "$EVALBENCH_WORK"
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
    select 'RESULT size when=$1 table=' || relname || ' rows_estimate=' || reltuples::bigint || ' total=' || pg_size_pretty(pg_total_relation_size(oid))
    from pg_class where relkind = 'r' and relname in ('events', 'issues', 'delivery_run_jobs', 'eval_readings', 'eval_days', 'issue_facts', 'session_titles')
    order by relname"
}
sizes loaded
mark "loaded"
status=0
"$here/run.sh" measure "$database_url" "$work/bin/envoy-dispatch" "$mode" || status=$?
sizes measured
mark "job.sh $mode exits $status"
exit "$status"
