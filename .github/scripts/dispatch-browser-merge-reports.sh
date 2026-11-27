#!/usr/bin/env bash
# The legs' blob reports joined into the one report of the run: an HTML report for a person, a
# JSON report for the readings that follow and for packages/dispatch/e2e/tools/check-slow-rows.ts,
# and the list reporter's lines in the step's own log.
#
# Who runs it: the step "Merge the legs' blob reports into one report" of
# .github/workflows/dispatch-browser.yaml's merge job;
# .github/ci-notes/dispatch-browser.md#what-the-merged-reports-three-outputs-are-for says why that
# report is the source check-slow-rows.ts reads. It takes no argument. It reads the directory the
# step before it wrote, $RUNNER_TEMP/selected-blobs/blob-report, which holds one attempt per leg,
# and writes $RUNNER_TEMP/report-html and $RUNNER_TEMP/report.json: the upload step reads both, and
# readings one, two and four and the verdict step read the JSON report. It also writes
# $RUNNER_TEMP/merge-reports.config.mjs, the merge configuration below, which nothing else reads.
# The runner sets $RUNNER_TEMP for every step.
#
# The step keeps its working directory, packages/dispatch, which is the package whose Playwright
# `bunx` runs below, and whose e2e directory is the merge configuration's testDir.
#
# Run from packages/dispatch, with $RUNNER_TEMP set:
#   ../../.github/scripts/dispatch-browser-merge-reports.sh
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
# The merge configuration, whose one setting is testDir: this checkout's e2e directory.
#
# A blob report records the test directory its leg ran from, <checkout>/packages/dispatch/e2e, and
# each path it holds relative to it: a row's file, its steps' and errors' locations, its project's
# testDir and outputDir. The pull-request lane's legs check out in two places, `real` in the CI
# container (/__w/<repo>/<repo>) and `stub` on the bare runner (/home/runner/work/<repo>/<repo>),
# and given blobs of two test directories and no configuration, Playwright 1.64.1's merge-reports
# refuses them ("Blob reports being merged were recorded with different test directories"), which
# failed run 45913634925's merge job with both legs green. Given `-c`, it skips that check and
# resolves every leg's paths against the configuration's testDir instead. Every leg ran
# `--config e2e/playwright.config.ts`, whose testDir is `.`, so this checkout's e2e directory is
# where each relative path names the same file at this commit: the JSON report's spec files stay
# relative to it as each leg wrote them, and the HTML report reads its code snippets there.
# Attachments and traces do not go through testDir: they are the blobs' resources/ entries, which
# merge-reports extracts into $blobs and resolves there, and the HTML report copies into its data/.
#
# Not the suite's own e2e/playwright.config.ts: it imports e2e/api.ts, which imports
# @legion/contracts, whose dist/ this job does not build, and the suite's bootstrap beside it, which
# reads this command's argv and environment. The file is written here rather than committed
# because its one value is this checkout's path, which only the step knows, and it takes that path
# from the environment so that no path is quoted into JavaScript.
config="$RUNNER_TEMP/merge-reports.config.mjs"
printf '%s\n' 'export default { testDir: process.env.DISPATCH_BROWSER_MERGE_TEST_DIR };' > "$config"
status=0
DISPATCH_BROWSER_MERGE_TEST_DIR="$PWD/e2e" bunx playwright merge-reports -c "$config" \
  --reporter=html,json,list "$blobs" || status=$?
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
