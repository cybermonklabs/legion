#!/usr/bin/env bash
# One shard leg of the Dispatch browser suite: the stacks' binaries, the lane's projects, the
# Playwright run over this leg's half of their row time, the row count the merge job reads, and
# the memory and CPU sampler this leg starts and reads back.
#
# Who runs it, and what it takes:
#
#   .github/scripts/dispatch-browser-shard-suite.sh <shard> <lane> <connections>
#
# <shard> is this leg's number out of the job's two, the shard job's `${{ matrix.shard }}`: it
# is the `--shard` numerator below, and it names the blob report this leg writes, the row count
# file beside it and the title of the sampler's readings. <lane> is the run's lane, the
# workflow's own `lane` input, `full` or `references`: it decides which projects and files run.
# The pull-request lane runs unsharded in dispatch-browser-pr-suite.sh instead.
# <connections> is the connection limit each stack's Postgres must allow, which a binaries job
# reads and the shard job passes as `${{ inputs.connections || needs.binaries.outputs.connections }}`:
# the pull-request lane's figure where the references lane takes that lane's build
# (.github/ci-notes/dispatch-browser.md#when-the-references-lane-takes-the-pull-request-lanes-build),
# and this call's own binaries job's otherwise. It is handed on because this job's container
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
# reports, record and run directory are written, and $GITHUB_WORKSPACE, the checkout the sampler
# and the stacks' binaries are read from.
#
# Run from packages/dispatch, with $RUNNER_TEMP and $GITHUB_WORKSPACE set:
#   ../../.github/scripts/dispatch-browser-shard-suite.sh 1 full 100
# CI runs it in dispatch-browser.yaml's shard job, at the step "Browser-test dispatch dashboard
# (shard ${{ matrix.shard }} of 2)".
set -euo pipefail

usage() {
  echo "::error::dispatch-browser-shard-suite.sh takes this leg's shard number, the run's" \
    "lane and the stacks' connection limit: .github/scripts/dispatch-browser-shard-suite.sh" \
    "<shard> <full|references> <connections>. It was given: $*"
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
# carries the row count written below. The merge job's reading one holds the two legs' lists
# against each other (packages/dispatch/e2e/tools/leg-rows.ts), which is what sees a row of the
# selection that no leg ran or that two ran; the row count cannot, since it counts only what the
# legs ran.
export DISPATCH_E2E_LEG_ROWS_DIR="$RUNNER_TEMP/leg-report/rows"
# The full lane runs the replay projects beside the real-stack projects. The config starts the
# static file server for a run that names a replay project; those projects need no harness of
# their own.
case "$lane" in
  full)
    projects="--project=chromium --project=iphone --project=webkit --project=webkit-iphone
    --project=firefox --project=firefox-download-navigation --project=chromium-plain-http
    --project=chromium-plain-http-sign-out --project=stub --project=stub-phone"
    lane_files=""
    ;;
  references)
    # The eleven spec files the reference framework's Verification document names, the
    # nine of the reading list's walk (e2e/reading-rows.ts), and main's parse-count walk,
    # which holds every page those rows draw to no parse of a stored text on the page's
    # own thread, and the one place they are written: ci.yaml's dispatch-references job
    # runs them on a pull request that changes what they test and whose run does not run
    # the full lane, whose `chromium` project holds every one of them, so that document's
    # checks read CI at the head rather than a run of an hour and a half by hand. Every
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
    echo "::error::shard lane must be full or references, not '$lane'"
    exit 1
    ;;
esac
# Why the projects are named at all: a shard splits the rows of every project the run
# selects, so a project this leg does not name is a project it would spread over these
# two machines. `visual` and `visual-phone` are in the default selection, and their
# image comparisons belong to the `visual` job, which runs them one row at a time, so
# naming the projects above is what keeps them off these machines. The three `-alone`
# projects stay out because this list does not name them, and for no other reason:
# `filterProjects` reads `default` only where the command line named no project
# (`projects.filter((project) => includeNonDefault || project.default)` at
# Playwright 1.64.1's runner, lib/runner/index.js in its package, under its
# (`if (!projectNames)`), and both lanes above name their own: ten in the full lane and one in
# the references lane.
# `default: false` each `-alone` project carries (the three entries of
# `packages/dispatch/e2e/playwright.config.ts#aloneProjects`) is what keeps them out of a run
# that names no project, which this is not. The alone job names them, all three on one machine.
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
# `--shard=$shard/2` this leg's half of the lane's row TIME rather than Playwright's half of its
# row count. Each leg places every file of every project the lane selects by the row times main
# measured (e2e/shard-weights.json) and keeps its own; e2e/shard-split.ts says why the count
# split left one of main's four legs a fifth heavier than the others and how every leg computes
# one placement. It reads the leg count from this `--shard` and from nowhere else, so it is no
# fourth place the count is written:
# .github/ci-notes/dispatch-browser.md#why-the-leg-count-stands-in-three-places says where the
# three are.
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
{ bunx playwright test --config e2e/playwright.config.ts $projects $lane_files --shard=$shard/2 --workers 4 \
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
