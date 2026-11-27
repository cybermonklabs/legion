#!/usr/bin/env bash
# The counting-embedder step of the Dispatch browser suite: the command bar's two counting-embedder
# rows, run on Chromium by the package script e2e:embedder:run against a stack of their own, after
# the main browser step of the job that calls it.
#
# Who runs it, and what it takes:
#
#   .github/scripts/dispatch-browser-embedder.sh <connections>
#
# <connections> is the connection limit the stack's Postgres must allow, which the binaries job
# reads and hands on as `${{ needs.binaries.outputs.connections }}`, because the container of the
# jobs that call this has no Go to compute it with; it becomes DISPATCH_E2E_PG_CONNECTIONS below.
#
# It is an argument rather than a key of the step's `env:`, for the reason
# .github/ci-notes/dispatch-browser.md#the-shard-jobs-shell-rule-its-measured-steps-and-its-budget
# gives: a step `env:` key outside ci_filter_coverage.py's ENV_ALLOWED drops that job out of
# check-ci-filters.sh's reduction of what a go command compiles.
#
# The real and alone jobs run this same command, so it is written once, here, for the reason
# .github/ci-notes/dispatch-browser.md#the-suite-its-two-lanes-and-the-budget-rule-every-job-follows
# gives: a step that calls a script keeps its own `timeout-minutes`. Each job's step keeps its own
# condition, its own `timeout-minutes` and its working directory, packages/dispatch, which is the
# package whose script the command below runs; and each job keeps its own upload of the output
# directory this writes, $RUNNER_TEMP/embedder-test-results, for when this step fails.
# .github/ci-notes/dispatch-browser.md#what-the-embedders-browser-step-runs-against says what the
# step runs against and why its output has a directory of its own.
#
# It reads two variables the runner sets for every step: $RUNNER_TEMP, under which Playwright's
# output directory is written, and $GITHUB_WORKSPACE, the checkout the stack's binaries are read
# from.
#
# Run from packages/dispatch, with $RUNNER_TEMP and $GITHUB_WORKSPACE set:
#   ../../.github/scripts/dispatch-browser-embedder.sh 100
# CI runs it in dispatch-browser.yaml's real and alone jobs, at the step "Browser-test the bar's
# two tiers against a counting embedder".
set -euo pipefail

usage() {
  echo "::error::dispatch-browser-embedder.sh takes the stack's connection limit:" \
    ".github/scripts/dispatch-browser-embedder.sh <connections>. It was given: $*"
  exit 1
}

[ "$#" -eq 1 ] || usage "$@"
connections=$1

# The job container's own PostgreSQL 18 with pgvector, through e2e/harness-postgres.ts's binaries
# provider: initdb and pg_ctl live in that directory alone, and a job inside a container has no
# docker to run the image provider's digest with.
export DISPATCH_E2E_PG_BIN=/usr/lib/postgresql/18/bin
# Read in the binaries job, since this container has no Go (e2e/harness-postgres.ts).
export DISPATCH_E2E_PG_CONNECTIONS=$connections
# What the binaries job built, which run-server.sh and run-broker.sh execute instead of building:
# the server by its path, the broker's three by the directory that holds them.
export DISPATCH_E2E_SERVER_BIN="$GITHUB_WORKSPACE/packages/envoy/out/dispatch-e2e-broker/dispatch"
export DISPATCH_E2E_BROKER_BIN_DIR="$GITHUB_WORKSPACE/packages/envoy/out/dispatch-e2e-broker"
# An output directory of its own, so Playwright's cleanup of it leaves the main browser step's
# test-results in place; each job's failure upload reads it.
bun run e2e:embedder:run --output="$RUNNER_TEMP/embedder-test-results"
