#!/usr/bin/env bash
# One leg of the Dispatch browser suite that runs on the runner itself, not in the shard and alone
# jobs' container: the embedder job's run and each of the two-process job's two runs. It runs the
# step's own Playwright command so that the command writes the blob report and the row count the
# merge job reads, as dispatch-browser-shard-suite.sh and dispatch-browser-alone-suite.sh have
# their legs write theirs, so a flaky or failed row of these runs is counted, named and published
# like a shard row.
#
# Who runs it, and what it takes:
#
#   .github/scripts/dispatch-browser-host-suite.sh <leg> <command>...
#
# <leg> is the leg's name, one of the names .github/scripts/dispatch-browser-run-reads.sh gives the
# legs of the job that runs it: `embedder`, `multiprocess-dashboard` or `multiprocess-signin`, so
# reading one finds the blob it owes under that name. It names the blob report, report-<leg>.zip,
# the row count beside it, rows/<leg>.txt, and the directory both are written into,
# $RUNNER_TEMP/leg-report/<leg>, which the upload after the step takes as the artifact
# `dispatch-browser-blob-<leg>-${{ github.run_attempt }}`.
# <command> is the step's Playwright run, given whole: `bun run e2e:embedder`,
# `bun run e2e:multiprocess` or `bunx playwright test --config e2e/playwright.config.ts`. Its last
# command must be `playwright test`, because this script adds the reporters at its end: a package
# script passes what follows its name to the last command it runs, and the last command of
# e2e:embedder and of e2e:multiprocess is `playwright test`.
#
# The step's environment reaches the command unchanged - the stack's Postgres image and connection
# limit it exports, and the two-process job's DISPATCH_E2E_* names - and the step keeps its own
# `timeout-minutes` and its working directory, packages/dispatch, which is the root the reporter
# path below is read from.
#
# It reads $RUNNER_TEMP, which the runner sets for every step.
#
# Run from packages/dispatch, with $RUNNER_TEMP set:
#   ../../.github/scripts/dispatch-browser-host-suite.sh embedder bun run e2e:embedder
# CI runs it in dispatch-browser.yaml's embedder job, at the step "Browser-test the bar's two tiers
# against a counting embedder", and in its multiprocess job, at the steps "Browser-test dispatch
# dashboard against two server processes" and "Browser-test a sign-in across two server
# processes".
set -euo pipefail

usage() {
  echo "::error::dispatch-browser-host-suite.sh takes this leg's name and the Playwright command" \
    "it runs: .github/scripts/dispatch-browser-host-suite.sh <leg> <command>... It was given: $*"
  exit 1
}

[ "$#" -ge 2 ] || usage "$@"
leg=$1
shift

# The directory this leg's report is written into, the one the upload after the step takes: the
# blob report and the row count side by side, as every leg's artifact holds them
# (.github/scripts/dispatch-browser-latest-blobs.sh reads those two directories of it). One per
# leg, since the two-process job runs two legs on one machine.
report="$RUNNER_TEMP/leg-report/$leg"
mkdir -p "$report/rows"
# The blob report this leg hands the merge job. Its name carries the leg, so the legs' reports
# keep their own names when the merge job selects them into one directory.
export PLAYWRIGHT_BLOB_OUTPUT_DIR="$report/blob-report"
export PLAYWRIGHT_BLOB_OUTPUT_NAME="report-$leg.zip"
# The three reporters the alone legs name, for the reasons dispatch-browser-alone-suite.sh gives:
# `list`, whose `Running N tests using M workers` line is this leg's row count, `blob`, and
# `./e2e/failure-reporter.ts`, which a `--reporter` on the command line would otherwise drop from
# the config's list.
#
# The command's own status is carried in a file rather than in `$?` or in PIPESTATUS, for the
# reason dispatch-browser-shard-suite.sh gives at length. The log and that file are written beside
# the leg's directory, not in it, since the upload takes that directory whole.
log="$RUNNER_TEMP/playwright-log-$leg.txt"
status_file="$RUNNER_TEMP/suite-status-$leg"
rm -f "$status_file"
{ "$@" --reporter=list,blob,./e2e/failure-reporter.ts 2>&1 ||
    echo "$?" > "$status_file"; } | tee "$log"
status=0
[ ! -f "$status_file" ] || status=$(cat "$status_file")
# The row count this leg printed, for the merge job's first reading. It travels beside the blob
# report, in a directory of its own so that `merge-reports` sees zips alone.
sed -n 's/^Running \([0-9][0-9]*\) tests\? using .*/\1/p' "$log" |
  tail -n 1 > "$report/rows/$leg.txt"
exit "$status"
