#!/usr/bin/env bash
# One shard leg of the Dispatch browser suite: the stacks' binaries, the lane's projects, the
# Playwright run over this leg's share of their row time, the row count the merge job reads,
# and the memory and CPU sampler this leg starts and reads back.
#
# Who runs it, and what it takes:
#
#   .github/scripts/dispatch-browser-shard-suite.sh <shard> <legs> <lane> <connections>
#
# <shard> is this leg's number, the shard job's `${{ matrix.shard }}`: it is the `--shard`
# numerator below, and it names the blob report this leg writes, the row count file beside it
# and the title of the sampler's readings. <legs> is how many legs the job runs, its
# `${{ strategy.job-total }}`: it is the `--shard` denominator below, and the matrix it counts
# is the one place the leg count is written
# (.github/ci-notes/dispatch-browser.md#how-many-legs-the-lane-runs-and-where-that-count-is-written).
# <lane> is the run's lane, the workflow's own `lane` input, `pull-request`, `full` or
# `references`: it decides which projects run, whether the run is held to the `@pr` rows and
# whether it is held to the references lane's spec files, and an unknown one ends this script
# below.
# <connections> is the connection limit each stack's Postgres must allow, which the binaries job
# reads and hands on as `${{ needs.binaries.outputs.connections }}`, because this job's container
# has no Go to compute it with; it becomes DISPATCH_E2E_PG_CONNECTIONS below.
#
# All four are arguments rather than keys of the step's `env:`, for the reason
# .github/ci-notes/dispatch-browser.md#the-shard-jobs-shell-rule-its-measured-steps-and-its-budget
# gives: a step `env:` key outside ci_filter_coverage.py's ENV_ALLOWED drops that job out
# of check-ci-filters.sh's reduction of what a go command compiles.
#
# The step keeps its own `timeout-minutes` and its working directory, packages/dispatch, which
# is the root the Playwright config path and the reporter path below are read from.
#
# It reads two variables the runner sets for every step: $RUNNER_TEMP, under which this leg's
# reports, record and run directory are written, and $GITHUB_WORKSPACE, the checkout the sampler
# and the stacks' binaries are read from.
#
# Run from packages/dispatch, with $RUNNER_TEMP and $GITHUB_WORKSPACE set:
#   ../../.github/scripts/dispatch-browser-shard-suite.sh 1 6 full 100
# CI runs it in dispatch-browser.yaml's shard job, at the step "Browser-test dispatch dashboard
# (shard ${{ matrix.shard }} of ${{ strategy.job-total }})".
set -euo pipefail

usage() {
  echo "::error::dispatch-browser-shard-suite.sh takes this leg's shard number, how many legs" \
    "the job runs, the run's lane and the stacks' connection limit:" \
    ".github/scripts/dispatch-browser-shard-suite.sh <shard> <legs>" \
    "<pull-request|full|references> <connections>. It was given: $*"
  exit 1
}

[ "$#" -eq 4 ] || usage "$@"
shard=$1
legs=$2
lane=$3
connections=$4
# Both are whole numbers before the comparisons below, which `test` ends the script on rather
# than refusing, and this leg must be one of the legs the job runs: a `--shard` numerator above
# its denominator is a run Playwright refuses, and one below a denominator larger than the
# matrix would leave the missing legs' rows unrun with nothing red.
case "$shard$legs" in
  '' | *[!0-9]*) usage "$@" ;;
esac
[ "$legs" -ge 1 ] && [ "$shard" -ge 1 ] && [ "$shard" -le "$legs" ] || usage "$@"

# $RUNNER_TEMP holds everything this step writes, the run directory the stack pool
# makes (mkdtempSync in os.tmpdir(), in e2e/harness-pool.ts's startPool) among it, which is what
# the upload steps below read. TMPDIR is exported on the command line, not in this
# step's `env:`, for the reason
# .github/ci-notes/dispatch-browser.md#the-shard-jobs-shell-rule-its-measured-steps-and-its-budget gives.
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
# Where e2e/shard-reporter.ts writes this leg's two row lists, shard-<N>.selection.jsonl and
# shard-<N>.ran.jsonl: the rows/ directory the upload carries beside the blob report, as it
# carries the row count written below. The merge job's reading one holds every leg's lists
# against each other (packages/dispatch/e2e/tools/leg-rows.ts), which is what sees a row of the
# selection that no leg ran or that two ran; the row count cannot, since it counts only what the
# legs ran.
export DISPATCH_E2E_LEG_ROWS_DIR="$RUNNER_TEMP/leg-report/rows"
# The lane's projects, named rather than left to the config, and the one place each
# lane's list is written. An unknown lane ends the step here rather than running the
# default set under a name nobody meant, and this is the one place that refuses it now
# that no install step reads the lane for engines.
#
# `stub` and `stub-phone` are in the pull-request and full lanes: a stubbed row is a
# row of this suite like any other, and it runs here rather than in a leg of its own so
# that neither the leg capture of the merge job's readings nor its row accounting has
# to learn a new name.
# They start no harness of their own either - this run starts one stack per worker
# already, for the real-stack rows beside them - and the config starts their static
# file server itself, for a run that names one of them (`packages/dispatch/e2e/playwright.config.ts#webServer`).
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
    lane_files=""
    ;;
  full)
    projects="--project=chromium --project=iphone --project=webkit --project=webkit-iphone
    --project=firefox --project=firefox-download-navigation --project=chromium-plain-http
    --project=chromium-plain-http-sign-out --project=stub --project=stub-phone"
    lane_grep=""
    lane_files=""
    ;;
  references)
    # The eleven spec files the reference framework's Verification document names, the
    # nine of the reading list's walk (e2e/reading-rows.ts), and main's parse-count walk,
    # which holds every page those rows draw to no parse of a stored text on the page's
    # own thread, and the one place they are written: ci.yaml's dispatch-references job
    # runs them on a pull request that changes what they test, so that document's checks
    # read CI at the head rather than a run of an hour and a half by hand. Every
    # Chromium row of them, `@pr` or not, so no grep. Each is a Playwright file
    # filter, a regular expression matched against a spec's absolute path, and each
    # matches the one file of that name under e2e/ and no other. A file added to this
    # list joins the `dispatch_references` filter, kept by hand in
    # .github/ci-gates/dispatch-references.py, which names these files and what they test:
    # .github/scripts/check-ci-filters.sh (rule 24) fails naming
    # an entry here that filter does not match, read from the directory of the
    # dispatch-browser.yaml step that runs this script. A reading spec this list does not
    # name runs on no pull request, so the walk's gate reads this arm the same way and
    # fails naming it (packages/dispatch/e2e/__tests__/reading-walk.test.ts).
    projects="--project=chromium"
    lane_grep=""
    lane_files="e2e/bare-references.e2e.ts e2e/btw.e2e.ts e2e/mention-composer.e2e.ts
    e2e/mention-reference.e2e.ts e2e/reference-kinds.e2e.ts e2e/reference-naming.e2e.ts
    e2e/reference-picker.e2e.ts e2e/reference-titles.e2e.ts e2e/refetch-budget.e2e.ts
    e2e/writing-surfaces.e2e.ts e2e/search.e2e.ts
    e2e/reading-agents.e2e.ts e2e/reading-answers.e2e.ts e2e/reading-broadcasts.e2e.ts
    e2e/reading-conversation.e2e.ts e2e/reading-documents.e2e.ts e2e/reading-inbox-asks.e2e.ts
    e2e/reading-inbox-threads.e2e.ts e2e/reading-pages.e2e.ts e2e/reading-references.e2e.ts
    e2e/parse-count.e2e.ts"
    ;;
  *)
    echo "::error::lane must be pull-request, full or references, not '$lane'"
    exit 1
    ;;
esac
# Why the projects are named at all: a shard splits the rows of every project the run
# selects, so a project this leg does not name is a project it would spread over the
# job's machines. `visual` and `visual-phone` are in the default selection, and their
# image comparisons belong to the `visual` job, which runs them one row at a time, so
# naming the projects above is what keeps them off these machines. The three `-alone`
# projects stay out because this list does not name them, and for no other reason:
# `filterProjects` reads `default` only where the command line named no project
# (`projects.filter((project) => includeNonDefault || project.default)` at
# Playwright 1.64.1's runner, lib/runner/index.js in its package, under its
# `if (!projectNames)`), and every lane above names its own: four in
# the pull-request lane, ten in the full one and one in the references one. The
# `default: false` each `-alone` project carries (the three entries of
# `packages/dispatch/e2e/playwright.config.ts#aloneProjects`) is what keeps them out of a run
# that names no project, which this is not. The alone job names them, one machine each.
#
# `list` beside `blob`: the blob reporter prints nothing, and this leg's row count is
# read from the list reporter's `Running N tests using M workers` line, which the merge
# job reports against the merged report's own count. $projects and $lane_files are left
# unquoted on purpose, to split into words.
#
# `./e2e/failure-reporter.ts` beside them, and it has to be named HERE: a `--reporter`
# on the command line REPLACES the config's reporter list rather than adding to it
# (e2e/run-outcome.ts cites the line), so without it this leg would load none of the
# config's own reporters, nothing would record how its rows ended, and the teardown
# would keep the run directory of every leg, green ones included. A path is read as a
# reporter module relative to the working directory below, which is this package's
# root (lib/cli/testActions.js:204-211).
#
# `./e2e/shard-reporter.ts` beside them, named here for the same reason: it is what makes
# `--shard=$shard/$legs` this leg's share of the lane's row TIME rather than Playwright's share
# of its row count. Each leg places every file of every project the lane selects by the row times
# main measured (e2e/shard-weights.json) and keeps its own; e2e/shard-split.ts says why the count
# split left one leg a fifth heavier than the others and how every leg computes one placement.
# Without it here the legs would split by count again, every row still run once, and nothing red.
# It prints one line to the log before the first row: this leg's rows, the row time it expects
# of this leg and of the run, and how many of its rows it placed by an estimate.
#
# The suite's own status is carried in a file, not in `$?` and not in `${PIPESTATUS[0]}`:
# the run is piped into `tee`, and without the `||` a failing suite would end this script
# at once under `set -e` and pipefail, before the row count and the sampler's readings
# below are written. The `||` writes the status and lets the pipeline succeed. Each side
# of a pipeline is its own subshell, so a variable set in the left-hand one does not
# survive it, which is why the status goes into a file; the script exits on it at the end.
rm -f "$RUNNER_TEMP/suite-status"
{ bunx playwright test --config e2e/playwright.config.ts $projects $lane_grep $lane_files "--shard=$shard/$legs" --workers 4 \
    --reporter=list,blob,./e2e/failure-reporter.ts,./e2e/shard-reporter.ts 2>&1 ||
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
