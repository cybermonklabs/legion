#!/usr/bin/env bash
# legion: tool - starts a Postgres of its own and runs BenchmarkCoverageStatementScale on it, which measures the search coverage statement at production's size and twice it and states the corpus size at which it reaches its 10 s bound, for the author of a change to that statement or its window indexes, who runs it as a Job in the builds namespace before the change merges and reads its coverage-scale: line.
# packages/envoy/scripts/coverage-scale.sh
#
# Measures the search coverage statement, its window kinds read by one bitmap heap scan over its
# two partial window indexes, against the statement it replaced at production's size and twice it,
# each in six states of embeddings (freshly vacuumed; about a third of its pages not all-visible
# after an ANALYZE; none all-visible after an ANALYZE; none all-visible while the statistics still
# record about half of them all-visible; and after an embedder change has dropped every vector,
# first with those statistics and then after an ANALYZE), times the builds of the two window
# indexes by their migrations' own statements, and states the corpus size at which the statement
# would reach its 10 s bound in the slowest of the six states (dispatch://INFRA-4241, R2): it runs
# BenchmarkCoverageStatementScale (internal/dispatch/embedqueue) on a Postgres of its own. It is
# what a Job in the cluster's builds namespace runs, from packages/envoy of a clone, on the worker
# image, which carries Go, the PostgreSQL 18 server with pgvector and legion-testpg. Never run it
# in an issue pod: the pod's own test server already holds 127.0.0.1:5433, and a corpus of
# production's size is more disk than the pod may use, so the script refuses to start where a
# server already answers there.
#
# COVERAGE_SCALE_SCRATCH (default /scratch) is the Job's scratch disk: the server's data directory
# (LEGION_TESTPG_DIR), GOCACHE, GOTMPDIR, GOMODCACHE and the run's log go under it.
# DISPATCH_COVERAGE_SCALES (the benchmark's default is "1 2") reaches it as it is.
#
# It prints the server's version and settings, then the benchmark's output, then its
# `coverage-scale:` line again, last. It exits with go test's own status, and with 1 when the
# benchmark printed no `coverage-scale:` line, which leaves nothing to judge.
set -euo pipefail

cd "$(dirname "$0")/.."

scratch="${COVERAGE_SCALE_SCRATCH:-/scratch}"
export LEGION_TESTPG_DIR="$scratch/testpg"
export LEGION_TEST_PG_DSN='postgres://legion_test@127.0.0.1:5433/legion_test?sslmode=disable'
export GOCACHE="$scratch/go-build"
export GOTMPDIR="$scratch/go-tmp"
export GOMODCACHE="$scratch/go-mod"
mkdir -p "$LEGION_TESTPG_DIR" "$GOCACHE" "$GOTMPDIR" "$GOMODCACHE"
log="$scratch/coverage-scale.log"

if pg_isready -q -h 127.0.0.1 -p 5433; then
  echo "coverage-scale.sh: a Postgres already answers on 127.0.0.1:5433; this script starts its own there, in a Job, never beside another server (an issue pod's own)" >&2
  exit 1
fi
legion-testpg ensure
psql "$LEGION_TEST_PG_DSN" -X -A -t -c 'select version()'
psql "$LEGION_TEST_PG_DSN" -X -A -t -c "select name || ' = ' || current_setting(name) from pg_settings
  where name in ('shared_buffers', 'work_mem', 'maintenance_work_mem', 'jit', 'max_parallel_workers_per_gather', 'effective_cache_size')
  order by name"

# go test's own status, read from PIPESTATUS: tee's would say only that the log was written.
set +e
DISPATCH_COVERAGE_SCALE_URL="$LEGION_TEST_PG_DSN" \
  go test -count=1 -v -timeout 3h -run '^$' -bench '^BenchmarkCoverageStatementScale$' -benchtime 1x ./internal/dispatch/embedqueue/ 2>&1 | tee "$log"
status=${PIPESTATUS[0]}
set -e

if summary="$(grep '^coverage-scale: ' "$log")"; then
  printf '%s\n' "$summary"
else
  echo "coverage-scale.sh: the benchmark printed no coverage-scale: line, so there is no result to judge; $log holds its output" >&2
  if [ "$status" -eq 0 ]; then
    status=1
  fi
fi
exit "$status"
