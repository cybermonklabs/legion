#!/usr/bin/env bash
# The visual leg of the Dispatch browser suite: the one stack its run needs, and
# packages/dispatch/e2e/visual-run.sh, which starts that stack on the host and runs Playwright
# against the baselines inside the pinned container.
#
# Who runs it: the step "Browser-test dispatch dashboard against its baselines" of
# .github/workflows/dispatch-browser.yaml's visual job, which has pulled that container in the
# step before it. It takes no argument. The step keeps its own `timeout-minutes` and its working
# directory, packages/dispatch, which is where e2e/visual-run.sh is run from below.
#
# Run from packages/dispatch: .github/scripts/dispatch-browser-visual-suite.sh
# CI runs it in dispatch-browser.yaml's visual job, at the step "Browser-test dispatch dashboard
# against its baselines".
set -euo pipefail

# $RUNNER_TEMP holds everything this step writes, the run directory of the stack
# e2e/visual-run.sh starts among it. TMPDIR and that stack's Postgres image are
# exported on the command line, not in this step's `env:`, for the reason the job's
# head gives.
export TMPDIR="$RUNNER_TEMP"
# pgvector, the copy in ghcr.io/cybermonklabs/legion/ci (.github/ci-images.yaml),
# started per stack by e2e/harness-postgres.ts. The shard and alone jobs take their
# stacks' PostgreSQL out of the image they run in (DISPATCH_E2E_PG_BIN) instead, which
# is no route here: this job runs on the runner and only its Playwright command is in
# a container.
export DISPATCH_E2E_PG_IMAGE=ghcr.io/cybermonklabs/legion/ci@sha256:2358fcba361ed2233a5ed81b5fe4ca779ccb304120ce531a3bf51c0ed7e2bc11
# One stack, which e2e/visual-run.sh starts on the host and stops however it ends: the
# Playwright run inside the container reads DISPATCH_E2E_REUSE_SERVERS=1 among that
# stack's variables and starts none of its own. Each project of the two runs one
# worker (e2e/playwright.config.ts), since two comparisons drawing at once on one
# machine is the contention that makes one read a half-drawn frame, so a second stack
# would stand idle.
e2e/visual-run.sh
