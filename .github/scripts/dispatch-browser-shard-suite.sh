#!/usr/bin/env bash
# One shard leg of the Dispatch browser suite: the stacks' binaries, the lane's projects less
# the quarantined rows, the Playwright run over this leg's quarter of their rows, and the row
# count the merge job reads.
#
# Who runs it, and what it takes:
#
#   .github/scripts/dispatch-browser-shard-suite.sh <shard> <lane> <connections>
#
# <shard> is this leg's number out of the job's four, the shard job's `${{ matrix.shard }}`: it
# is the `--shard` numerator below, and it names the blob report this leg writes and the row
# count file beside it. <lane> is the run's lane, the
# workflow's own `${{ inputs.lane }}`, `pull-request` or `full`: it decides which projects run
# and whether the run is held to the `@pr` rows, and an unknown one ends this script below.
# <connections> is the connection limit each stack's Postgres must allow, which the binaries job
# reads and hands on as `${{ needs.binaries.outputs.connections }}`, because this job's container
# has no Go to compute it with; it becomes DISPATCH_E2E_PG_CONNECTIONS below.
#
# All three are arguments rather than keys of the step's `env:`, for the reason
# .github/ci-notes/dispatch-browser.md#the-shard-jobs-shell-rule-its-measured-steps-and-its-budget
# gives: a step `env:` key outside ci_filter_coverage.py's ENV_ALLOWED drops that job out
# of check-ci-filters.sh's reduction of what a go command compiles.
#
# The step keeps its own `timeout-minutes` and its working directory, packages/dispatch, which
# is the root the Playwright config path and the reporter path below are read from.
#
# It reads two variables the runner sets for every step: $RUNNER_TEMP, under which this leg's
# report and run directory are written, and $GITHUB_WORKSPACE, the checkout the stacks'
# binaries are read from.
#
# Run from packages/dispatch, with $RUNNER_TEMP and $GITHUB_WORKSPACE set:
#   ../../.github/scripts/dispatch-browser-shard-suite.sh 1 full 100
# CI runs it in dispatch-browser.yaml's shard job, at the step "Browser-test dispatch dashboard
# (shard ${{ matrix.shard }} of 4)".
set -euo pipefail

usage() {
  echo "::error::dispatch-browser-shard-suite.sh takes this leg's shard number, the run's" \
    "lane and the stacks' connection limit: .github/scripts/dispatch-browser-shard-suite.sh" \
    "<shard> <pull-request|full> <connections>. It was given: $*"
  exit 1
}

[ "$#" -eq 3 ] || usage "$@"
shard=$1
lane=$2
connections=$3

# $RUNNER_TEMP holds everything this step writes, the run directory the stack pool
# makes (mkdtempSync in os.tmpdir(), in e2e/harness-pool.ts's startPool) among it, which is what
# the upload steps below read. TMPDIR is exported on the command line, not in this
# step's `env:`, for the reason
# .github/ci-notes/dispatch-browser.md#the-shard-jobs-shell-rule-its-measured-steps-and-its-budget gives.
export TMPDIR="$RUNNER_TEMP"
# The directory this step's report is written into, one artifact below, which the merge
# job reads: the blob report and the row count in one directory, because only the first
# line of a multi-line `path:` is translated into the container's own filesystem - the
# step below says what that cost when it was not done.
mkdir -p "$RUNNER_TEMP/leg-report/rows"
# This image's own PostgreSQL 18 with pgvector, through e2e/harness-postgres.ts's
# binaries provider: initdb and pg_ctl live in that directory alone, and a job inside a
# container has no docker to run the image provider's digest with.
export DISPATCH_E2E_PG_BIN=/usr/lib/postgresql/18/bin
# Read in the binaries job, since this container has no Go (e2e/harness-postgres.ts).
export DISPATCH_E2E_PG_CONNECTIONS=$connections
# What the binaries job built, which run-server.sh and run-broker.sh
# execute instead of building: the server by its path, the broker's three by the
# directory that holds them.
export DISPATCH_E2E_SERVER_BIN="$GITHUB_WORKSPACE/packages/envoy/out/dispatch-e2e-broker/dispatch"
export DISPATCH_E2E_BROKER_BIN_DIR="$GITHUB_WORKSPACE/packages/envoy/out/dispatch-e2e-broker"
# The blob report this leg hands the merge job. Its name carries the leg, so the legs'
# reports keep their own names when the merge job downloads them into one directory.
export PLAYWRIGHT_BLOB_OUTPUT_DIR="$RUNNER_TEMP/leg-report/blob-report"
export PLAYWRIGHT_BLOB_OUTPUT_NAME="report-shard-$shard.zip"
# The quarantined rows (packages/dispatch/e2e/quarantine.ts) leave both lanes here:
# `exclude` adds to each project a `grepInvert` pattern that matches exactly those rows
# (e2e/playwright.config.ts passes every project through `quarantined`). They run in the
# `quarantine` job of the nightly's call instead, where they gate nothing. With the
# quarantine empty the pattern is not added at all, and each lane selects the rows it
# did before. Exported here, not set in this step's `env:`, for the reason
# .github/ci-notes/dispatch-browser.md#the-shard-jobs-shell-rule-its-measured-steps-and-its-budget gives.
export DISPATCH_E2E_QUARANTINE=exclude
# The lane's projects, named rather than left to the config, and the one place either
# lane's list is written. An unknown lane ends the step here rather than running the
# default set under a name nobody meant, and this is the one place that refuses it now
# that no install step reads the lane for engines.
#
# `stub` and `stub-phone` are in both lanes: a stubbed row is a row of this suite like
# any other, and it runs here rather than in a leg of its own so that neither the leg
# capture of the merge job's readings nor its row accounting has to learn a new name.
# They start no harness of their own either - this run starts one stack per worker
# already, for the real-stack rows beside them - and the config starts their static
# file server itself, for a run that names one of them (e2e/playwright.config.ts's
# `webServer`).
#
# `--grep '@pr'` on the pull-request lane alone is what holds that lane to the rows a
# pull request is answered by: every stubbed row carries the tag, and so do the
# real-stack rows of the journeys that must be seen on a pull request. Playwright
# applies a command-line grep **on top of** a project's own, never in place of it -
# measured at this branch's base, 475d7609, where `iphone` listed 109 rows and 4 of
# them under `--grep '@pr'`, and `webkit` 40 rows and 7 of them, and again at this
# head, 23f2d137, where `iphone` listed 119 and the same 4, and `webkit` 40 and the
# same 7 - so the two tagged projects keep their own axis and the command's grep
# selects the rows that carry both tags.
# Every row without the tag runs on the full lane, which `ci.yaml` runs for every
# merge group and the nightly runs daily, so `main` is gated exactly as it was and what
# a displaced row loses is feedback on the pull request itself
# (dispatch://INFRA-1407, departure 1). `e2e/__tests__/pull-request-lane.test.ts` holds
# the lane to its budget by listing the config with this same grep.
case "$lane" in
  pull-request)
    projects="--project=chromium --project=iphone --project=stub --project=stub-phone"
    # Anchored as `tagged("pr")` is (e2e/configurations.ts), which is what
    # e2e/__tests__/pull-request-lane.test.ts lists this lane with: a bare `@pr`
    # would also take a row titled `@prototype`, and the two readings of this lane
    # must select the same rows or the budget test measures another lane.
    lane_grep="--grep=(^|\\s)@pr(\\s|$)"
    ;;
  full)
    projects="--project=chromium --project=iphone --project=webkit --project=webkit-iphone
    --project=firefox --project=firefox-download-navigation --project=chromium-plain-http
    --project=chromium-plain-http-sign-out --project=stub --project=stub-phone"
    lane_grep=""
    ;;
  *)
    echo "::error::lane must be pull-request or full, not '$lane'"
    exit 1
    ;;
esac
# Why the projects are named at all: a shard splits the rows of every project the run
# selects, so a project this leg does not name is a project it would spread over these
# four machines. `visual` and `visual-phone` are in the default selection, and their
# image comparisons belong to the `visual` job, which runs them one row at a time, so
# naming the projects above is what keeps them off these machines. The three `-alone`
# projects stay out because this list does not name them, and for no other reason:
# `filterProjects` reads `default` only where the command line named no project
# (`projects.filter((project) => includeNonDefault || project.default)` at
# node_modules/.bun/playwright@1.64.1/node_modules/playwright/lib/runner/index.js:2136,
# under the `if (!projectNames)` at :2135), and both lanes above name theirs: four in
# the pull-request lane and ten in the full one. The `default: false` each `-alone`
# project carries (the three entries of `aloneProjects` in e2e/playwright.config.ts) is
# what keeps them out of a run that names no project, which this is not. The alone job
# names them, one machine each.
#
# `list` beside `blob`: the blob reporter prints nothing, and this leg's row count is
# read from the list reporter's `Running N tests using M workers` line, which the merge
# job reports against the merged report's own count. $projects is left unquoted on
# purpose, to split into words.
#
# `./e2e/failure-reporter.ts` beside them, and it has to be named HERE: a `--reporter`
# on the command line REPLACES the config's reporter list rather than adding to it
# (e2e/run-outcome.ts cites the line), so without it this leg would load none of the
# config's own reporters, nothing would record how its rows ended, and the teardown
# would keep the run directory of every leg, green ones included. A path is read as a
# reporter module relative to the working directory below, which is this package's
# root (lib/cli/testActions.js:204-211).
#
# The suite's own status is carried in a file, not in `$?` and not in `${PIPESTATUS[0]}`:
# the run is piped into `tee`, and without the `||` a failing suite would end this script
# at once under `set -e` and pipefail, before the row count below is written. The `||`
# writes the status and lets the pipeline succeed. Each side
# of a pipeline is its own subshell, so a variable set in the left-hand one does not
# survive it, which is why the status goes into a file; the script exits on it at the end.
rm -f "$RUNNER_TEMP/suite-status"
{ bunx playwright test --config e2e/playwright.config.ts $projects $lane_grep --shard=$shard/4 --workers 3 --reporter=list,blob,./e2e/failure-reporter.ts 2>&1 ||
    echo "$?" > "$RUNNER_TEMP/suite-status"; } | tee "$RUNNER_TEMP/playwright-log.txt"
status=0
[ ! -f "$RUNNER_TEMP/suite-status" ] || status=$(cat "$RUNNER_TEMP/suite-status")
# The row count this leg printed, for the merge job's first reading. It travels beside
# the blob report, in a directory of its own so that `merge-reports` sees zips alone.
sed -n 's/^Running \([0-9][0-9]*\) tests\? using .*/\1/p' "$RUNNER_TEMP/playwright-log.txt" |
  tail -n 1 > "$RUNNER_TEMP/leg-report/rows/shard-$shard.txt"
exit "$status"
