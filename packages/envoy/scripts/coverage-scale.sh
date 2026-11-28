#!/usr/bin/env bash
# packages/envoy/scripts/coverage-scale.sh
#
# Times the search coverage counts' fill and recount, the statements of the counts that read every
# row they count, on a synthetic corpus at production's size and a larger scale, the recount in
# each of six states of embeddings (freshly vacuumed; about a third of its pages not all-visible
# after an ANALYZE; none all-visible after an ANALYZE; none all-visible while the statistics still
# record about half of them all-visible; and after an embedder change has dropped every vector,
# first with those statistics and then after an ANALYZE), times the builds of the two window
# indexes by their migrations' own statements, and fails when four times the fill at the largest
# scale passes coverageFillTimeout or four times the longest whole recount there passes
# coverageRecountTimeout, stating the corpus size at which the recount would reach its bound: it runs
# TestCoverageStatementScale (internal/dispatch/embedqueue) on a Postgres of its own. It is what a
# Job in the cluster's builds namespace runs, from packages/envoy of a clone, on the worker image,
# which carries Go, the PostgreSQL 18 server with pgvector and legion-testpg. Never run it in an
# issue pod: the pod's own test server already holds 127.0.0.1:5433, and a corpus of production's
# size is more disk than the pod may use, so the script refuses to start where a server already
# answers there.
#
# COVERAGE_SCALE_SCRATCH (default /scratch) is the Job's scratch disk: the server's data directory
# (LEGION_TESTPG_DIR), GOCACHE, GOTMPDIR, GOMODCACHE and the tests' logs go under it.
# DISPATCH_COVERAGE_SCALES (the test's default is "1 2") reaches the test as it is.
#
# COVERAGE_SCALE_SERVE=1, which reaches the test as it is, adds a start of the head's server at
# the largest scale, S (the fill-length check): the test copies the database of S once its corpus is
# loaded, before any measurement, with done_at unset, and prints the copy's name on a line that
# starts `coverage-scale-database:`. The script then runs TestTheServerFillsTheCoverageCountsAtScale
# (cmd/dispatch) on the copy, which starts the server there, reads its fill from the server's own
# lines, stops it, starts it again with done_at set and reads its recount, and prints lines that
# start `fill-length:`; the script drops the copy after it. It runs that test whatever the scale
# test's verdict, since a Job that sets coverageFillTimeout and coverageRecountTimeout runs the
# scale test under the bounds it is replacing. The copy needs as much disk as S's own database
# while S is measured.
#
# It prints the server's version and settings, then the tests' output, then the scale test's
# `coverage-scale:` lines again and, under COVERAGE_SCALE_SERVE=1, the server test's
# `fill-length:` lines, each test's verdict last. It exits with the scale test's own status, then
# the server test's; with 1 when the scale test printed no `coverage-scale:` line, which leaves
# nothing to judge; and under COVERAGE_SCALE_SERVE=1 with 1 when there was no copy to start the
# server on, when the server test printed no `fill-length:` line, or when the copy was not dropped.
set -euo pipefail

cd "$(dirname "$0")/.."

scratch="${COVERAGE_SCALE_SCRATCH:-/scratch}"
server='postgres://legion_test@127.0.0.1:5433'
export LEGION_TESTPG_DIR="$scratch/testpg"
export LEGION_TEST_PG_DSN="$server/legion_test?sslmode=disable"
export GOCACHE="$scratch/go-build"
export GOTMPDIR="$scratch/go-tmp"
export GOMODCACHE="$scratch/go-mod"
mkdir -p "$LEGION_TESTPG_DIR" "$GOCACHE" "$GOTMPDIR" "$GOMODCACHE"
log="$scratch/coverage-scale.log"
fill_log="$scratch/coverage-fill.log"
serve="${COVERAGE_SCALE_SERVE:-}"

if pg_isready -q -h 127.0.0.1 -p 5433; then
  echo "coverage-scale.sh: a Postgres already answers on 127.0.0.1:5433; this script starts its own there, in a Job, never beside another server (an issue pod's own)" >&2
  exit 1
fi
legion-testpg ensure
psql "$LEGION_TEST_PG_DSN" -X -A -t -c 'select version()'
psql "$LEGION_TEST_PG_DSN" -X -A -t -c "select name || ' = ' || current_setting(name) from pg_settings
  where name in ('shared_buffers', 'work_mem', 'maintenance_work_mem', 'jit', 'max_parallel_workers_per_gather', 'effective_cache_size')
  order by name"

# Each test's own status, read from PIPESTATUS: tee's would say only that the log was written.
set +e
DISPATCH_COVERAGE_SCALE_URL="$LEGION_TEST_PG_DSN" \
  go test -count=1 -v -timeout 3h -run '^TestCoverageStatementScale$' ./internal/dispatch/embedqueue/ 2>&1 | tee "$log"
status=${PIPESTATUS[0]}
set -e

serve_status=0
copy=''
if [ "$serve" = 1 ]; then
  copy="$(sed -n 's/^coverage-scale-database: //p' "$log" | tail -n 1)"
  if [ -z "$copy" ]; then
    echo "coverage-scale.sh: COVERAGE_SCALE_SERVE=1, but the test printed no coverage-scale-database: line, so there is no copy to start the server on; $log holds its output" >&2
    serve_status=1
  else
    set +e
    DISPATCH_COVERAGE_FILL_URL="$server/$copy?sslmode=disable" \
      go test -count=1 -v -timeout 3h -run '^TestTheServerFillsTheCoverageCountsAtScale$' ./cmd/dispatch/ 2>&1 | tee "$fill_log"
    serve_status=${PIPESTATUS[0]}
    set -e
    if ! psql "$LEGION_TEST_PG_DSN" -X -q -v ON_ERROR_STOP=1 -c "drop database if exists $copy"; then
      echo "coverage-scale.sh: the copy $copy was not dropped" >&2
      if [ "$serve_status" -eq 0 ]; then
        serve_status=1
      fi
    fi
  fi
fi

if summary="$(grep '^coverage-scale: ' "$log")"; then
  printf '%s\n' "$summary"
else
  echo "coverage-scale.sh: the test printed no coverage-scale: line, so there is no result to judge; $log holds its output" >&2
  if [ "$status" -eq 0 ]; then
    status=1
  fi
fi
if [ -n "$copy" ]; then
  if lines="$(grep '^fill-length: ' "$fill_log")"; then
    printf '%s\n' "$lines"
  else
    echo "coverage-scale.sh: the server test printed no fill-length: line, so there is no start to judge; $fill_log holds its output" >&2
    if [ "$serve_status" -eq 0 ]; then
      serve_status=1
    fi
  fi
fi
if [ "$status" -eq 0 ]; then
  status=$serve_status
fi
exit "$status"
