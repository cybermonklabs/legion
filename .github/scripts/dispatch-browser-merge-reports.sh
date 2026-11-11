#!/usr/bin/env bash
# The legs' blob reports joined into the one report of the run: an HTML report for a person, a
# JSON report for the readings that follow and for packages/dispatch/e2e/tools/check-slow-rows.ts,
# and the list reporter's lines in the step's own log.
#
# Who runs it: the step "Merge the legs' blob reports into one report" of
# .github/workflows/dispatch-browser.yaml's merge job, whose comment says why that report is the
# source check-slow-rows.ts reads. It takes no argument. It reads the directory the step before
# it wrote, $RUNNER_TEMP/selected-blobs/blob-report, which holds one attempt per leg, and writes
# $RUNNER_TEMP/report-html and $RUNNER_TEMP/report.json, which the upload step and the four
# readings after it read.
#
# The step keeps its working directory, packages/dispatch, which is the package whose Playwright
# `bunx` runs below.
#
# Run from packages/dispatch: .github/scripts/dispatch-browser-merge-reports.sh
# CI runs it in dispatch-browser.yaml's merge job, at the step "Merge the legs' blob reports into
# one report".
set -euo pipefail

blobs="$RUNNER_TEMP/selected-blobs/blob-report"
if [ ! -d "$blobs" ] || [ -z "$(ls -A "$blobs" 2>/dev/null)" ]; then
  echo "::error::no leg uploaded a blob report, so there is nothing to merge;" \
    "the reading below names the legs that owed one"
  exit 1
fi
export PLAYWRIGHT_HTML_OUTPUT_DIR="$RUNNER_TEMP/report-html"
export PLAYWRIGHT_HTML_OPEN=never
export PLAYWRIGHT_JSON_OUTPUT_NAME="$RUNNER_TEMP/report.json"
status=0
bunx playwright merge-reports --reporter=html,json,list "$blobs" || status=$?
# `merge-reports` exits 1 on a report holding a failed row as much as on a blob it
# could not read, so the exit alone does not say which happened. A failed row is the
# readings' and the verdict step's to report, by leg and by name, so this step fails
# only where no report was written at all - the one case that leaves them nothing to
# read.
if [ ! -f "$RUNNER_TEMP/report.json" ]; then
  echo "::error::merge-reports wrote no JSON report (it exited $status), so the legs'" \
    "blob reports could not be joined"
  exit 1
fi
