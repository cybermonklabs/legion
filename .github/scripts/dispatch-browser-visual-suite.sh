#!/usr/bin/env bash
# The visual leg of the Dispatch browser suite: the one stack its run needs, and
# packages/dispatch/e2e/visual-run.sh, which starts that stack on the host and runs Playwright
# against the baselines inside the pinned container.
#
# Who runs it, and what it takes:
#
#   .github/scripts/dispatch-browser-visual-suite.sh <connections>
#
# The step "Browser-test dispatch dashboard against its baselines" of
# .github/workflows/dispatch-browser.yaml's visual job runs it, having pulled that container in
# an earlier step. <connections> is the connection limit the stack's Postgres must allow, which
# the binaries job reads outside this step's budget and hands on as
# `${{ needs.binaries.outputs.connections }}`, since this job runs no go command; it
# becomes DISPATCH_E2E_PG_CONNECTIONS below. It is an argument rather than a key of the step's
# `env:`, for the reason
# .github/ci-notes/dispatch-browser.md#the-shard-jobs-shell-rule-its-measured-steps-and-its-budget
# gives: a step `env:` key outside
# ci_filter_coverage.py's ENV_ALLOWED drops that job out of check-ci-filters.sh's reduction of
# what a go command compiles. The step keeps its own `timeout-minutes` and its working
# directory, packages/dispatch, which is where e2e/visual-run.sh is run from below.
# It reads two variables the runner sets for every step: $RUNNER_TEMP, under which the stack's
# run directory is made, and $GITHUB_WORKSPACE, the checkout the stack's binaries are read from:
# the ones the binaries job built, which the visual job's steps before this one download and
# place, as the shard and alone jobs' do.
#
# Run from packages/dispatch, with $RUNNER_TEMP and $GITHUB_WORKSPACE set:
#   ../../.github/scripts/dispatch-browser-visual-suite.sh 100
# CI runs it in dispatch-browser.yaml's visual job, at the step "Browser-test dispatch dashboard
# against its baselines".
set -euo pipefail

usage() {
  echo "::error::dispatch-browser-visual-suite.sh takes the stack's connection limit:" \
    ".github/scripts/dispatch-browser-visual-suite.sh <connections>. It was given: $*"
  exit 1
}

[ "$#" -eq 1 ] || usage "$@"
connections=$1

# $RUNNER_TEMP holds everything this step writes, the run directory of the stack
# e2e/visual-run.sh starts among it. TMPDIR and that stack's Postgres image are
# exported on the command line, not in this step's `env:`, for the reason
# .github/ci-notes/dispatch-browser.md#the-visual-review-its-docker-run-and-its-budgets gives.
export TMPDIR="$RUNNER_TEMP"
# pgvector, the copy in ghcr.io/cybermonklabs/legion/ci (.github/ci-images.yaml),
# started per stack by e2e/harness-postgres.ts. The shard and alone jobs take their
# stacks' PostgreSQL out of the image they run in (DISPATCH_E2E_PG_BIN) instead, which
# is no route here: this job runs on the runner and only its Playwright command is in
# a container.
export DISPATCH_E2E_PG_IMAGE=ghcr.io/cybermonklabs/legion/ci@sha256:2358fcba361ed2233a5ed81b5fe4ca779ccb304120ce531a3bf51c0ed7e2bc11
# Read in the binaries job, outside this step's budget (e2e/harness-postgres.ts).
export DISPATCH_E2E_PG_CONNECTIONS=$connections
# What the binaries job built, which run-server.sh and run-broker.sh execute instead
# of building: the server by its path, the broker's three by the directory that holds
# them.
export DISPATCH_E2E_SERVER_BIN="$GITHUB_WORKSPACE/packages/envoy/out/dispatch-e2e-broker/dispatch"
export DISPATCH_E2E_BROKER_BIN_DIR="$GITHUB_WORKSPACE/packages/envoy/out/dispatch-e2e-broker"
# One stack, which e2e/visual-run.sh starts on the host and stops however it ends: the
# Playwright run inside the container reads DISPATCH_E2E_REUSE_SERVERS=1 among that
# stack's variables and starts none of its own. Each project of the two runs one
# worker (e2e/playwright.config.ts), since two comparisons drawing at once on one
# machine is the contention that makes one read a half-drawn frame, so a second stack
# would stand idle.
e2e/visual-run.sh
