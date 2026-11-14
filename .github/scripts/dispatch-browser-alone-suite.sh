#!/usr/bin/env bash
# One `-alone` leg of the Dispatch browser suite: the stack's binaries, the Playwright run of
# one unsharded project one row at a time, the row count the merge job reads, and the memory and
# CPU sampler this leg starts and reads back.
#
# Who runs it, and what it takes:
#
#   .github/scripts/dispatch-browser-alone-suite.sh <project>
#
# <project> is this leg's Playwright project, the alone job's `${{ matrix.project }}`, one of
# the three `-alone` ones: it is the `--project` the run takes, and it names the blob report
# this leg writes, the row count file beside it and the title of the sampler's readings.
#
# It is an argument rather than a key of the step's `env:`, for the reason the alone job's head
# gives: a step `env:` carrying an expression drops that job out of check-ci-filters.sh's
# reduction of what a go command compiles.
#
# The step keeps its own `timeout-minutes` and its working directory, packages/dispatch, which
# is the root the Playwright config path and the reporter path below are read from.
#
# Run from packages/dispatch: .github/scripts/dispatch-browser-alone-suite.sh webkit-alone
# CI runs it in dispatch-browser.yaml's alone job, at the step "Browser-test dispatch dashboard
# (${{ matrix.project }})".
set -euo pipefail

usage() {
  echo "::error::dispatch-browser-alone-suite.sh takes this leg's Playwright project:" \
    ".github/scripts/dispatch-browser-alone-suite.sh <project>. It was given: $*"
  exit 1
}

[ "$#" -eq 1 ] || usage "$@"
project=$1

# $RUNNER_TEMP holds everything this step writes, the run directory the stack pool
# makes (mkdtempSync in os.tmpdir(), e2e/harness-pool.ts:143) among it, which is what
# the upload steps below read. TMPDIR is exported on the command line, not in this
# step's `env:`, for the reason the job's head gives.
export TMPDIR="$RUNNER_TEMP"
# The two directories this step's output is sorted into, each of them one artifact
# below, as the shard job sorts its own and for the same reason: only the first line
# of a multi-line `path:` is translated into the container's own filesystem.
mkdir -p "$RUNNER_TEMP/leg-report/rows" "$RUNNER_TEMP/dispatch-e2e-record"
# The sampler, started as a background process of this step's own shell and killed by
# the trap below, as the shard job starts and kills it. It writes memory-samples.txt
# and cpu-samples.txt into the directory it is given, which the record below is, and
# the reading at the end of this step takes them from there.
"$GITHUB_WORKSPACE/.github/scripts/dispatch-browser-samples.sh" sample "$RUNNER_TEMP/dispatch-e2e-record" &
sampler=$!
trap 'kill "$sampler" 2>/dev/null || true' EXIT
# This image's own PostgreSQL 18 with pgvector, through e2e/harness-postgres.ts's
# binaries provider: initdb and pg_ctl live in that directory alone, and a job inside a
# container has no docker to run the image provider's digest with.
export DISPATCH_E2E_PG_BIN=/usr/lib/postgresql/18/bin
# What the binaries job built, which run-server.sh and run-broker.sh
# execute instead of building: the server by its path, the broker's three by the
# directory that holds them.
export DISPATCH_E2E_SERVER_BIN="$GITHUB_WORKSPACE/packages/envoy/out/dispatch-e2e-broker/dispatch"
export DISPATCH_E2E_BROKER_BIN_DIR="$GITHUB_WORKSPACE/packages/envoy/out/dispatch-e2e-broker"
# The blob report this leg hands the merge job. Its name carries the leg, so the legs'
# reports keep their own names when the merge job downloads them into one directory.
export PLAYWRIGHT_BLOB_OUTPUT_DIR="$RUNNER_TEMP/leg-report/blob-report"
export PLAYWRIGHT_BLOB_OUTPUT_NAME="report-alone-$project.zip"
# `--project` and `--workers=1` are the whole selection, and the first of them selects
# this leg's rows and nothing else. Until dispatch://INFRA-1280 it did not: each
# `-alone` project named every parallel project in its `dependencies`, so this command
# carried `--no-deps`, without which Playwright would have run the whole suite here
# before this leg's own rows, and the shard jobs had run it already. Those
# `dependencies` are gone, and that flag with them.
#
# `--workers=1` says the thing the measurement rests on instead of leaning on a
# default. The configuration's own default is the same one worker (the top-level
# `workers: 1` of e2e/playwright.config.ts), so this leg is green with or without the
# flag today; what the flag adds is that the leg stays one worker wide if that default
# ever moves. A wider count is refused rather than measured: `refuseASharedRun` in
# e2e/main-thread-hold.ts fails every row that calls `measureDraw` when
# `config.workers` is above one, printing the one-worker command that fixes it. So the
# guard is what makes the reading honest, and this line is what says what the run is:
# one project, one row at a time, on a machine with nothing else on it.
#
# `list` beside `blob`: the blob reporter prints nothing, and this leg's row count is
# read from the list reporter's `Running N tests using M workers` line, which the merge
# job reports against the merged report's own count. No `--shard`: these projects are
# not sharded, which is the whole reason they have a job.
#
# `./e2e/failure-reporter.ts` beside them, named here for the reason the shard job's
# own browser step gives: a `--reporter` on the command line replaces the config's
# list, so a leg that did not name it would record no row and keep its run directory
# whether it passed or failed.
#
# The suite's own status is carried in a file rather than in `$?` or in PIPESTATUS,
# for the reason the shard job's own browser step gives at length.
rm -f "$RUNNER_TEMP/suite-status"
{ bunx playwright test --config e2e/playwright.config.ts --project="$project" --workers=1 --reporter=list,blob,./e2e/failure-reporter.ts 2>&1 ||
    echo "$?" > "$RUNNER_TEMP/suite-status"; } | tee "$RUNNER_TEMP/playwright-log.txt"
status=0
[ ! -f "$RUNNER_TEMP/suite-status" ] || status=$(cat "$RUNNER_TEMP/suite-status")
kill "$sampler" 2>/dev/null || true
# The row count this leg printed, for the merge job's first reading. It travels beside
# the blob report, in a directory of its own so that `merge-reports` sees zips alone.
sed -n 's/^Running \([0-9][0-9]*\) tests\? using .*/\1/p' "$RUNNER_TEMP/playwright-log.txt" |
  tail -n 1 > "$RUNNER_TEMP/leg-report/rows/alone-$project.txt"
# The sampler's two readings, titled with this leg so a run's notices say which
# machine each is from. After the suite and before the exit below, so this step still
# ends on the suite's own status.
"$GITHUB_WORKSPACE/.github/scripts/dispatch-browser-samples.sh" report "$RUNNER_TEMP/dispatch-e2e-record" "$project"
exit "$status"
