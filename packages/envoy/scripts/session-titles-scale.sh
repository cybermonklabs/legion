#!/usr/bin/env bash
# packages/envoy/scripts/session-titles-scale.sh
#
# Times what the first boot of the release that carries session_titles does before it listens, on a
# store larger than production's (dispatch://INFRA-5573, R4): migrations 20261117100008,
# 20261117100010, 20261117100015 and 20261117100016, applied by store.Migrate as the boot applies
# them, and store.FillSessionTitles, timed together as one span, which must end within 60 s. It runs
# TestSessionTitlesBootScale (internal/dispatch/store), once for each session count, each run on a
# database of its own that holds the larger of 1.5 million and 1.25 times production's newest event
# id of titled session events, each with about 1.7 KB of JSON payload that does not compress
# (production's mean is about 2 KB). It is what a Job in the cluster's builds namespace runs, from
# packages/envoy of a clone, beside a Postgres of the Job's own. Never run it against a server that
# holds anything of value: the test creates and drops its databases there.
#
# DISPATCH_SESSION_TITLES_SCALE_URL (required) names that Postgres server, as a URL whose role may
# create databases; its database path is ignored.
# DISPATCH_SESSION_TITLES_SCALE_NEWEST_EVENT_ID (required) is production's newest event id on the
# day of the run, read through GET /api/v1/events/log.
# DISPATCH_SESSION_TITLES_SCALE_SESSIONS (default "110000 11000") lists the session counts of the
# runs, space-separated.
# DISPATCH_SESSION_TITLES_SCALE_LOAD, the negative control's alone, makes each run load that many
# events instead of the floor, which each run must then refuse.
# SESSION_TITLES_SCALE_SCRATCH (default /scratch) holds GOCACHE, GOTMPDIR and the test's log.
#
# It prints the server's version and the settings the times depend on, then the test's output, then
# each run's `session-titles-scale:` line again, last. It exits with the test's own status, and with
# 1 when the test printed fewer such lines than the runs it was given, which leaves a run unjudged.
set -euo pipefail

cd "$(dirname "$0")/.."

: "${DISPATCH_SESSION_TITLES_SCALE_URL:?names the Postgres server the runs create their databases on}"
: "${DISPATCH_SESSION_TITLES_SCALE_NEWEST_EVENT_ID:?is the newest event id of production, read through GET /api/v1/events/log}"
export DISPATCH_SESSION_TITLES_SCALE_URL DISPATCH_SESSION_TITLES_SCALE_NEWEST_EVENT_ID
sessions="${DISPATCH_SESSION_TITLES_SCALE_SESSIONS:-110000 11000}"
export DISPATCH_SESSION_TITLES_SCALE_SESSIONS="$sessions"

scratch="${SESSION_TITLES_SCALE_SCRATCH:-/scratch}"
export GOCACHE="$scratch/go-build"
export GOTMPDIR="$scratch/go-tmp"
mkdir -p "$GOCACHE" "$GOTMPDIR"
log="$scratch/session-titles-scale.log"

psql "$DISPATCH_SESSION_TITLES_SCALE_URL" -X -A -t -c 'select version()'
psql "$DISPATCH_SESSION_TITLES_SCALE_URL" -X -A -t -c "select name || ' = ' || current_setting(name) from pg_settings
  where name in ('shared_buffers', 'work_mem', 'maintenance_work_mem', 'effective_cache_size', 'fsync', 'synchronous_commit',
                 'full_page_writes', 'wal_level', 'max_wal_size', 'jit', 'max_parallel_workers_per_gather')
  order by name"

# The test's own status, read from PIPESTATUS: tee's would say only that the log was written.
set +e
go test -count=1 -v -timeout 3h -run '^TestSessionTitlesBootScale$' ./internal/dispatch/store/ 2>&1 | tee "$log"
status=${PIPESTATUS[0]}
set -e

runs=$(wc -w <<<"$sessions")
lines=$(grep -c '^session-titles-scale: ' "$log" || true)
grep '^session-titles-scale: ' "$log" || true
if [ "$lines" -lt "$runs" ]; then
  echo "session-titles-scale.sh: the test printed $lines session-titles-scale: lines for $runs runs, so a run has no result to judge; $log holds its output" >&2
  if [ "$status" -eq 0 ]; then
    status=1
  fi
fi
exit "$status"
