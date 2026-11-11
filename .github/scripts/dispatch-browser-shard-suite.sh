#!/usr/bin/env bash
# One shard leg of the Dispatch browser suite: the stacks' binaries, the lane's projects, the
# Playwright run over this leg's quarter of their rows, the row count the merge job reads, and
# the memory and CPU sampler this leg starts and reads back.
#
# Who runs it, and what it takes:
#
#   .github/scripts/dispatch-browser-shard-suite.sh <shard> <lane>
#
# <shard> is this leg's number out of the job's four, the shard job's `${{ matrix.shard }}`: it
# is the `--shard` numerator below, and it names the blob report this leg writes, the row count
# file beside it and the title of the sampler's readings. <lane> is the run's lane, the
# workflow's own `${{ inputs.lane }}`, `pull-request` or `full`: it decides which projects run
# and whether the run is held to the `@pr` rows, and an unknown one ends this script below.
#
# Both are arguments rather than keys of the step's `env:`, for the reason the shard job's head
# gives: a step `env:` carrying an expression drops that job out of check-ci-filters.sh's
# reduction of what a go command compiles.
#
# The step keeps its own `timeout-minutes` and its working directory, packages/dispatch, which
# is the root the Playwright config path and the reporter path below are read from.
#
# Run from packages/dispatch: .github/scripts/dispatch-browser-shard-suite.sh 1 full
# CI runs it in dispatch-browser.yaml's shard job, at the step "Browser-test dispatch dashboard
# (shard ${{ matrix.shard }} of 4)".
set -euo pipefail

usage() {
  echo "::error::dispatch-browser-shard-suite.sh takes this leg's shard number and the run's" \
    "lane: .github/scripts/dispatch-browser-shard-suite.sh <shard> <pull-request|full>." \
    "It was given: $*"
  exit 1
}

[ "$#" -eq 2 ] || usage "$@"
shard=$1
lane=$2

# $RUNNER_TEMP holds everything this step writes, the run directory the stack pool
# makes (mkdtempSync in os.tmpdir(), e2e/harness-pool.ts:143) among it, which is what
# the upload steps below read. TMPDIR is exported on the command line, not in this
# step's `env:`, for the reason the job's head gives.
export TMPDIR="$RUNNER_TEMP"
# The two directories this step's output is sorted into, each of them one artifact
# below: the leg's report, which the merge job reads, and the record of what the run
# left behind. One directory per artifact because only the first line of a multi-line
# `path:` is translated into the container's own filesystem - the step below says what
# that cost when it was not done.
mkdir -p "$RUNNER_TEMP/leg-report/rows" "$RUNNER_TEMP/dispatch-e2e-record"
# The sampler, started as a background process of this step's own shell and killed by
# the trap below, as the alone job starts and kills it. It writes memory-samples.txt
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
export PLAYWRIGHT_BLOB_OUTPUT_NAME="report-shard-$shard.zip"
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
# measured at this branch's base, where `--grep '@pr'` left `iphone` all 99 of its
# `@phone` rows and `webkit` 1 of its 40 - so the two tagged projects keep their own
# axis. Every row without the tag runs on the full lane, which `ci.yaml` runs for every
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
# Why the projects are named at all: Playwright shards top-level projects only, and the
# three `-alone` projects name every other project in their `dependencies`
# (e2e/playwright.config.ts), so a shard over the whole list would shard `webkit-alone`
# alone and run every other project whole in all four shards. Naming the eight parallel
# projects with `--project` leaves the `-alone` ones out of the run, so every project
# left is top-level and every one of them shards. The alone job runs those three.
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
# the run is piped into `tee`, so `$?` after the pipeline is `tee`'s and a failing
# suite would leave this step green, and PIPESTATUS is bash's where this step's shell
# is `sh -e {0}` (the job's head says why it must stay that). Each side of a pipeline
# is its own subshell, so a variable set in the left-hand one does not survive it; the
# `||` is what keeps `sh -e` from ending the subshell before the status is written.
rm -f "$RUNNER_TEMP/suite-status"
{ bunx playwright test --config e2e/playwright.config.ts $projects $lane_grep --shard=$shard/4 --workers 4 --reporter=list,blob,./e2e/failure-reporter.ts 2>&1 ||
    echo "$?" > "$RUNNER_TEMP/suite-status"; } | tee "$RUNNER_TEMP/playwright-log.txt"
status=0
[ ! -f "$RUNNER_TEMP/suite-status" ] || status=$(cat "$RUNNER_TEMP/suite-status")
kill "$sampler" 2>/dev/null || true
# The row count this leg printed, for the merge job's first reading. It travels beside
# the blob report, in a directory of its own so that `merge-reports` sees zips alone.
sed -n 's/^Running \([0-9][0-9]*\) tests\? using .*/\1/p' "$RUNNER_TEMP/playwright-log.txt" |
  tail -n 1 > "$RUNNER_TEMP/leg-report/rows/shard-$shard.txt"
# The sampler's two readings, titled with this leg so a run's notices say which
# machine each is from. After the suite and before the exit below, so this step still
# ends on the suite's own status.
"$GITHUB_WORKSPACE/.github/scripts/dispatch-browser-samples.sh" report "$RUNNER_TEMP/dispatch-e2e-record" "shard $shard"
exit "$status"
