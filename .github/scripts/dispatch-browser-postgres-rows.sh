#!/usr/bin/env bash
# The Bun rows of packages/dispatch/e2e/__tests__ that need a PostgreSQL: a server of this image's
# PostgreSQL 18 for the two files that run statements on one, the stacks' own PostgreSQL, server
# and broker for the third, and packages/dispatch's `bun run test:postgres` over the three.
#
# Who runs it, and what it takes:
#
#   .github/scripts/dispatch-browser-postgres-rows.sh <connections>
#
# <connections> is the connection limit the stacks' Postgres must allow, which the binaries job
# reads and hands on as `${{ needs.binaries.outputs.connections }}`, because this job's container
# has no Go to compute it with; it becomes DISPATCH_E2E_PG_CONNECTIONS below.
#
# It is an argument rather than a key of the step's `env:`, for the reason
# .github/ci-notes/dispatch-browser.md#the-shard-jobs-shell-rule-its-measured-steps-and-its-budget
# gives: a step `env:` key outside ci_filter_coverage.py's ENV_ALLOWED drops that job out of
# check-ci-filters.sh's reduction of what a go command compiles. Every name below is exported
# here for the same reason.
#
# The step keeps its own `timeout-minutes` and its working directory, packages/dispatch, where
# `bun run test:postgres` is defined.
#
# It reads two variables the runner sets for every step: $RUNNER_TEMP, under which the server and
# the stacks' run directories are written, and $GITHUB_WORKSPACE, the checkout the stacks'
# binaries are read from.
#
# Run from packages/dispatch, with $RUNNER_TEMP and $GITHUB_WORKSPACE set, in the browser image:
#   ../../.github/scripts/dispatch-browser-postgres-rows.sh 100
# CI runs it in dispatch-browser.yaml's postgres-rows job, at the step "Test the dashboard's rows
# that need a PostgreSQL".
set -euo pipefail

usage() {
  echo "::error::dispatch-browser-postgres-rows.sh takes the stacks' connection limit:" \
    ".github/scripts/dispatch-browser-postgres-rows.sh <connections>. It was given: $*"
  exit 1
}

[ "$#" -eq 1 ] || usage "$@"
connections=$1

# $RUNNER_TEMP holds everything this step writes: the server below and the run directories the
# third file's stacks make under TMPDIR.
export TMPDIR="$RUNNER_TEMP"
# The server LEGION_TEST_PG_DSN names: psql-session.test.ts makes a database of its own on it and
# drops it again, and harness-stacks.test.ts runs `select 1` on it. This image's PostgreSQL 18,
# which .github/docker/browser.Dockerfile puts on PATH, started as
# .github/scripts/dispatch-browser-postgres.sh starts its own, on a port no stack takes (the
# stacks take ports the OS chooses) and trusting the one account this job runs as, and stopped
# however the rows end.
pg="$RUNNER_TEMP/pg"
mkdir -p "$pg"
initdb -D "$pg/data" -U postgres --auth=trust --no-sync -E UTF8 > /dev/null
pg_ctl -D "$pg/data" -l "$pg/postgres.log" -o "-p 55433 -h 127.0.0.1 -k $pg" -w start > /dev/null
trap 'pg_ctl -D "$pg/data" -m fast -w stop > /dev/null 2>&1 || true' EXIT
export LEGION_TEST_PG_DSN="postgres://postgres@127.0.0.1:55433/postgres?sslmode=disable"
# second-global-setup.test.ts's stacks: this image's PostgreSQL 18 through
# e2e/harness-postgres.ts's binaries provider, the binaries job's connection limit, and the
# binaries job's server and broker, as the shard job's browser step exports them.
export DISPATCH_E2E_PG_BIN=/usr/lib/postgresql/18/bin
export DISPATCH_E2E_PG_CONNECTIONS=$connections
export DISPATCH_E2E_SERVER_BIN="$GITHUB_WORKSPACE/packages/envoy/out/dispatch-e2e-broker/dispatch"
export DISPATCH_E2E_BROKER_BIN_DIR="$GITHUB_WORKSPACE/packages/envoy/out/dispatch-e2e-broker"
bun run test:postgres
