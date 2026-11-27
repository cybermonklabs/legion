#!/usr/bin/env bash
# The eval registry's measurement Job (dispatch://INFRA-2916, T15), the Evals tab's
# (dispatch://INFRA-2918, T27) and dispatch://INFRA-978's k14 (T19.9), whole: a Postgres 18 of its
# own with pg_stat_statements preloaded and the statement log in jsonlog, the server, the
# generator and the instrument built from the checkout, the database migrated by the server, the
# evalbench fixture loaded, and run.sh's measurement. job.yaml runs it on the build farm; it runs
# the same in a pod at a small EVALBENCH_SCALE.
#
# Postgres and the server each run under a supervisor (supervise.sh): in a container of their own
# where the pod runs them so (job.yaml, which names their directory in EVALBENCH_CONTROL and holds
# each container to the size it gives it), else beside this script, which then starts both and
# holds their driver lock. Before the measurement and again at its end the run prints its shape as
# a SHAPE line: gating where the pod runs on the compute class EVALBENCH_GATING_CLASS names, the
# server's requests are its limits, the kernel holds the server's container to those limits, and
# the machine's CPUs beside the pod's CPU request are fewer than the server's, so no pod of the
# server's size runs beside it; record otherwise, with the reasons. Beside that word it prints the
# node, the machine's CPUs, CPU model and memory, each container's requests and limits as the API
# server gave them (EVALBENCH_PODINFO) and the limits the kernel holds the server's container to
# (supervise.sh's container file). The machine type is a label of the node, which a pod with no
# API access cannot read: the CPUs, the CPU model and the memory are what the run itself sees.
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
#                  timeline reads beside the base's the same number of times. With
#                  EVALBENCH_BASE_FLAGS empty and EVALBENCH_BASE a commit of this branch that
#                  holds every table of the eval registry the load checks
#                  (evalfixture.DerivedTables) but not the code evals, it is k17's control: the
#                  generator loads this head's history with its readings and none of the code
#                  survival results (-code=false), writing the delivery settings without code_groups
#                  where the base's project_delivery_settings predates that column (migration
#                  20261126133047), and the paint is the evals list, which must hold neither code eval
#   job.sh tab     the Evals tab's paint and recompute in Chromium (k4 at the stated rate, k19 at
#                  ten times; run.sh's tab): this head's server serving the web app built from the
#                  checkout, over the full fixture, with track_commit_timestamp on, so the drive
#                  reads each write's commit
#   job.sh k14     k14 (run.sh's k14): this head over the fixture with the two k14 projects added
#                  (the generator's -k14), k14's three writes and their moves back with pages open,
#                  and each k14 project's first paint after a restart; no failure day is added
#
# With EVALBENCH_ONE_PROJECT=1, head and base load the one-project fixture of k22 and k24
# (evalbench's -one-project: INFRA alone, its pull requests in two repositories) and run k22's
# halves in place of their measurement: base, at EVALBENCH_BASE=82380570f8 (legion/INFRA-978, the
# base the checks name), captures GET /api/v1/delivery/timeline for the 84 days before the
# history's end into EVALBENCH_BASE_TIMELINE (run.sh's capture-base), outside the repository; head
# reads the project's timeline page by page and compares the union with that capture (run.sh's
# pages, evalbench/timelinepages). One EVALBENCH_NOW and one EVALBENCH_SEED for the two runs give
# both databases one history.
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
#   EVALBENCH_ONE_PROJECT (0), EVALBENCH_BASE_TIMELINE (EVALBENCH_WORK/base-timeline.json): the
#                          one-project fixture and k22's capture, at head and base
#   EVALBENCH_FAILURE_DAY (7; 0 adds none), EVALBENCH_FAILURE_DAY_RUNS (36),
#   EVALBENCH_FAILURE_DAY_JOBS (45): the generator's -failure-day, -failure-day-runs and
#                          -failure-day-jobs, at head and at the base
#   EVALBENCH_PGPORT (55432), EVALBENCH_SHARED_BUFFERS (1GB), PGBIN (the newest
#                          /usr/lib/postgresql/*/bin)
#   EVALBENCH_CHROMIUM     tab: a Chromium to drive in place of the one Playwright installs
#   EVALBENCH_NODE         the node the run is on (job.yaml sets it from the pod's spec.nodeName),
#                          printed with the machine's CPUs and memory
#   EVALBENCH_BASE_FLAGS   base: the generator's flags that say the base's schema (-base: main
#                          before the eval registry, whose history has no readings); set empty, a
#                          base that holds the registry loads this head's history, as head does.
#                          The base load always passes -code=false, the base having no code tables
#   EVALBENCH_CONTROL      the supervisors' directory (supervise.sh) where the pod runs them in
#                          containers of their own and holds their driver lock (job.yaml); unset,
#                          job.sh runs them beside itself, under EVALBENCH_WORK
#   EVALBENCH_PODINFO      a directory of each container's requests and limits, a file a value
#                          (<server|postgres|driver>.<requests|limits>.<cpu|memory>, in millicores and
#                          MiB: job.yaml's downward API), which the SHAPE line prints and judges
#   EVALBENCH_COMPUTE_CLASS, EVALBENCH_GATING_CLASS, EVALBENCH_RUNTIME_CLASS: the compute class the
#                          pod runs on, the one a run is judged on (none: no run is), and the pod's
#                          runtime class (none: the machine's own), which the SHAPE line names
#   run.sh's EVALBENCH_PORT, EVALBENCH_CATCHUP, EVALBENCH_QUIET, EVALBENCH_CATALOG,
#   EVALBENCH_PAINT_RUNS, EVALBENCH_GITHUB_STUB (head and base: the GitHub stand-in of check k17),
#   EVALBENCH_TAB_QUIET, EVALBENCH_TAB_CATALOG and EVALBENCH_RECOMPUTE_ARGS
#   pass through.
set -euo pipefail

mode=${1:-}
case "$mode" in head | base | tab | k14) ;; *)
  echo "usage: job.sh head|base|tab|k14" >&2
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
one_project=${EVALBENCH_ONE_PROJECT:-0}
if [ "$mode" = tab ] && [ "$one_project" = 1 ]; then
  echo "job.sh: tab measures the full fixture; EVALBENCH_ONE_PROJECT is for head and base" >&2
  exit 2
fi
# The GitHub stand-in's throwaway App key is made by openssl, which run.sh needs only after the
# load: a Job without it fails here, before the hour the load and the catch-up take.
if [ "${EVALBENCH_GITHUB_STUB:-0}" = 1 ] && ! command -v openssl >/dev/null 2>&1; then
  echo "job.sh: EVALBENCH_GITHUB_STUB=1 needs openssl, which makes the stand-in's throwaway App key" >&2
  exit 2
fi
if [ -z "${PGBIN:-}" ]; then
  PGBIN=$(ls -d /usr/lib/postgresql/*/bin | sort -V | tail -n 1)
fi
mkdir -p "$work/bin" "$work/pglog" "$work/sock"
mark() { echo "MARK $(date -u +%FT%TZ) $*"; }
mark "job.sh $mode: source $source_dir at $commit, scale ${EVALBENCH_SCALE:-1}, ten times ${EVALBENCH_TEN_TIMES:-0}, one project $one_project, now $now, base flags [${EVALBENCH_BASE_FLAGS--base}], GitHub stand-in ${EVALBENCH_GITHUB_STUB:-0}"
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

# shape prints the run's SHAPE line (the header says what it holds and when it reads gating).
shape() {
  local reasons=() resources="" container field value pod=0 cpus kernel word=gating
  local -A got=()
  cpus=$(grep -c '^processor' /proc/cpuinfo)
  for container in server postgres driver; do
    for field in requests.cpu limits.cpu requests.memory limits.memory; do
      value=none
      if [ -n "${EVALBENCH_PODINFO:-}" ] && [ -r "$EVALBENCH_PODINFO/$container.$field" ]; then
        value=$(<"$EVALBENCH_PODINFO/$container.$field")
      fi
      got[$container.$field]=$value
    done
    resources+=" ${container}=cpu:${got[$container.requests.cpu]}m/${got[$container.limits.cpu]}m,memory:${got[$container.requests.memory]}Mi/${got[$container.limits.memory]}Mi"
    case "${got[$container.requests.cpu]}" in '' | *[!0-9]*) ;; *) pod=$((pod + got[$container.requests.cpu])) ;; esac
  done
  kernel=$(cat "$control/server/container" 2>/dev/null || echo unread)
  if [ -z "${EVALBENCH_GATING_CLASS:-}" ]; then
    reasons+=("no gating class is named")
  elif [ "${EVALBENCH_COMPUTE_CLASS:-}" != "$EVALBENCH_GATING_CLASS" ]; then
    reasons+=("the pod's compute class is not the gating class")
  fi
  case "${got[server.requests.cpu]}" in
    '' | *[!0-9]*) reasons+=("the server's requests and limits were not read (EVALBENCH_PODINFO)") ;;
    *)
      if [ "${got[server.requests.cpu]}" != "${got[server.limits.cpu]}" ] || [ "${got[server.requests.memory]}" != "${got[server.limits.memory]}" ]; then
        reasons+=("the server's requests are not its limits")
      fi
      case " $kernel " in
        *" cpu_limit_m=${got[server.limits.cpu]} memory_limit_mi=${got[server.limits.memory]} "*) ;;
        *) reasons+=("the kernel does not hold the server's container to its limits") ;;
      esac
      if [ $((cpus * 1000 - pod)) -ge "${got[server.requests.cpu]}" ]; then
        reasons+=("the machine's CPUs beside the pod's request hold a pod of the server's size")
      fi
      ;;
  esac
  [ "${#reasons[@]}" -eq 0 ] || word=record
  echo "SHAPE $word compute_class=${EVALBENCH_COMPUTE_CLASS:-none} gating_class=${EVALBENCH_GATING_CLASS:-none}" \
    "runtime_class=${EVALBENCH_RUNTIME_CLASS:-none} node=${EVALBENCH_NODE:-unnamed} machine_cpus=$cpus" \
    "machine_memory_gib=$(awk '/^MemTotal/ { printf "%.1f", $2 / 1048576 }' /proc/meminfo)" \
    "cpu_model=\"$(awk -F': *' '/^model name/ { print $2; exit }' /proc/cpuinfo)\"$resources server_kernel=[$kernel]" \
    "pod_cpu_request=${pod}m reasons=\"$(IFS=';'; echo "${reasons[*]:-none}")\""
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
# made it. The tab's drive reads each write's commit time (pg_xact_commit_timestamp), which
# track_commit_timestamp keeps. Each pg_ctl runs under Postgres's supervisor, so the postmaster it
# starts runs in Postgres's container. The trap is set before Postgres starts, so every exit from
# here on stops it, and then the supervisors started here.
pg_options="-p $port -k $work/sock \
  -c listen_addresses=127.0.0.1 -c max_connections=200 -c shared_buffers=${EVALBENCH_SHARED_BUFFERS:-1GB} \
  -c shared_preload_libraries=pg_stat_statements -c pg_stat_statements.track=top -c pg_stat_statements.track_utility=off \
  -c pg_stat_statements.track_planning=on \
  -c compute_query_id=on -c logging_collector=on -c log_destination=jsonlog -c log_directory=$work/pglog \
  -c log_filename=postgres.log -c log_rotation_age=0 -c log_rotation_size=0 -c log_timezone=UTC \
  -c wal_level=minimal -c max_wal_senders=0 \
  -c max_wal_size=4GB -c huge_pages=off -c dynamic_shared_memory_type=mmap -c jit=off"
if [ "$mode" = tab ]; then
  pg_options="$pg_options -c track_commit_timestamp=on"
fi
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
pg_ctl --log "$work/pg-server.log" --wait --timeout 300 --options "$pg_options" start
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

# The generator and the instruments are this head's in every mode; the server is the mode's.
cd "$source_dir/packages/envoy"
go build -o "$work/bin/evalbench" ./internal/dispatch/evals/evalbench/cmd
go build -o "$work/bin/measure" ./internal/dispatch/evals/evalbench/measure
go build -o "$work/bin/timelinepages" ./internal/dispatch/evals/evalbench/timelinepages
server_source=$source_dir
stamp=$commit
load_flags=()
if [ "$one_project" = 1 ]; then
  load_flags=(-one-project)
fi
if [ "$mode" = base ]; then
  server_source=${EVALBENCH_BASE_SOURCE:-$work/base-src}
  if [ -z "${EVALBENCH_BASE_SOURCE:-}" ]; then
    git -C "$source_dir" worktree add --detach "$server_source" "$base"
  fi
  stamp=$base
  if [ "$one_project" != 1 ]; then
    # shellcheck disable=SC2206 # EVALBENCH_BASE_FLAGS is flags, split on purpose.
    load_flags=(${EVALBENCH_BASE_FLAGS--base} -code=false)
  fi
fi
if [ "$mode" = k14 ]; then
  load_flags=(-k14)
fi
(cd "$server_source/packages/envoy" && go build -ldflags "-X main.buildCommit=$stamp" -o "$work/bin/envoy-dispatch" ./cmd/dispatch)
mark "built the generator, the instruments and envoy-dispatch at $stamp"

export EVALBENCH_WORK=$work/run EVALBENCH_MEASURE=$work/bin/measure EVALBENCH_PG_LOG=$work/pglog/postgres.json
export EVALBENCH_SOURCE=$server_source EVALBENCH_NOW=$now EVALBENCH_PAGES=$work/bin/timelinepages
export EVALBENCH_BASE_TIMELINE=${EVALBENCH_BASE_TIMELINE:-$work/base-timeline.json}
EVALBENCH_PG_RESTART=$(printf '%q ' "$here/supervise.sh" exec "$control/postgres" "$work/pg-ctl.log" "$PGBIN/pg_ctl" \
  --pgdata "$work/pg" --log "$work/pg-server.log" --wait --timeout 300 --mode fast restart)
export EVALBENCH_PG_RESTART
mkdir -p "$EVALBENCH_WORK"

if [ "$mode" = tab ]; then
  # The web app the server serves, built from the checkout as CI builds it, and the Chromium of
  # the Playwright packages/dispatch locks. The tab's two steps are read once before the load, so a
  # step that cannot be read fails here, before the hour the load and the catch-up take.
  (cd "$source_dir" && bun install --frozen-lockfile >"$work/bun-install.log" 2>&1) || {
    tail -n 40 "$work/bun-install.log"
    echo "FAIL job.sh: bun install failed, so the tab is not measured"
    exit 1
  }
  (cd "$source_dir/packages/dispatch" && bun run build:contracts >"$work/web-build.log" 2>&1 && bun run build:web >>"$work/web-build.log" 2>&1) || {
    tail -n 40 "$work/web-build.log"
    echo "FAIL job.sh: the web app did not build, so the tab is not measured"
    exit 1
  }
  export EVALBENCH_WEB_DIST=$source_dir/packages/dispatch/web/dist EVALBENCH_DISPATCH_DIR=$source_dir/packages/dispatch
  export EVALBENCH_TAB=$here/tab PLAYWRIGHT_BROWSERS_PATH=${PLAYWRIGHT_BROWSERS_PATH:-$work/ms-playwright}
  if [ -z "${EVALBENCH_CHROMIUM:-}" ]; then
    if ! (cd "$source_dir/packages/dispatch" && bunx playwright install chromium >"$work/playwright-install.log" 2>&1); then
      tail -n 20 "$work/playwright-install.log"
      EVALBENCH_CHROMIUM=$(command -v chromium || true)
      mark "Playwright's Chromium did not install; driving ${EVALBENCH_CHROMIUM:-no Chromium}"
      export EVALBENCH_CHROMIUM
    fi
  fi
  if ! bun build --target=bun --outdir "$work/tab-read" "$here/tab/paint.ts" "$here/tab/recompute.ts" >"$work/tab-read.log" 2>&1; then
    cat "$work/tab-read.log"
    echo "FAIL job.sh: bun cannot read the tab's steps, so the tab is not measured"
    exit 1
  fi
  # Biome's check of the tab's steps, as CI's lint runs it: what it fixes by itself is written in
  # this throwaway checkout and printed as a patch (gzip, base64) for the author to apply, and what
  # it cannot fix is printed beside it.
  lint=0
  (cd "$source_dir" && bunx biome check --write "$here/tab" >"$work/tab-lint.log" 2>&1) || lint=$?
  sed 's/^/MARK biome /' "$work/tab-lint.log" | tail -n 80
  echo "RESULT tab lint biome_exit=$lint fixed_files=$(git -C "$source_dir" diff --name-only -- "$here/tab" | wc -l)"
  echo "MARK biome-patch $(git -C "$source_dir" diff -- "$here/tab" | gzip -c | base64 -w0)"
  mark "built the web app at $EVALBENCH_WEB_DIST; Chromium ${EVALBENCH_CHROMIUM:-of Playwright at $PLAYWRIGHT_BROWSERS_PATH}"
fi

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
    from pg_class where (relkind = 'r' and relname in ('events', 'issues', 'delivery_run_jobs', 'eval_readings', 'eval_days', 'issue_facts', 'session_titles',
      'code_changes', 'code_pairs', 'code_days'))
      or (relkind = 'i' and relname in ('delivery_run_jobs_failed', 'delivery_run_jobs_named'))
    order by relname"
}
sizes loaded
durability on
mark "loaded"
shape
status=0
if [ "$mode" = tab ]; then
  rate=stated
  if [ "${EVALBENCH_TEN_TIMES:-0}" = 1 ]; then rate=ten; fi
  "$here/run.sh" tab "$database_url" "$work/bin/envoy-dispatch" "$rate" || status=$?
  sizes measured
  mark "job.sh $mode exits $status"
  exit "$status"
fi
if [ "$one_project" = 1 ]; then
  step=pages
  if [ "$mode" = base ]; then step=capture-base; fi
  "$here/run.sh" "$step" "$database_url" "$work/bin/envoy-dispatch" || status=$?
  mark "job.sh $mode (one project) exits $status"
  exit "$status"
fi
"$here/run.sh" measure "$database_url" "$work/bin/envoy-dispatch" "$mode" || status=$?
sizes measured
failure_day=${EVALBENCH_FAILURE_DAY:-7}
# k14 measures its writes and first paints over the load alone: no failure day is added.
[ "$mode" != k14 ] || failure_day=0
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
