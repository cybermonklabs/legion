#!/usr/bin/env bash
# The quarantine job's reading: how each quarantined row ended in a run that gates nothing, read
# from the JSON report the browser step before it wrote, with one line per row for the step
# summary and one `::notice::` per row for a reader of the log.
#
# Who runs it: the step "Reading the quarantined rows" of
# .github/workflows/dispatch-browser.yaml's quarantine job;
# .github/ci-notes/dispatch-browser.md#how-the-quarantine-jobs-reading-names-each-row says how a
# row is named, and why so. It takes no argument: it reads $RUNNER_TEMP/quarantine/report.json,
# writes $RUNNER_TEMP/quarantine/rows.tsv, one line per row, which the upload after it takes with
# the report, and adds its list to $GITHUB_STEP_SUMMARY. A leg that wrote no report is a warning
# and exit 0 here, the browser step before it saying why.
# The runner sets $RUNNER_TEMP and $GITHUB_STEP_SUMMARY for every step.
#
# Run from anywhere, with $RUNNER_TEMP and $GITHUB_STEP_SUMMARY set:
#   .github/scripts/dispatch-browser-reading-quarantined-rows.sh
# CI runs it in dispatch-browser.yaml's quarantine job, at the step "Reading the quarantined
# rows".
set -euo pipefail

report="$RUNNER_TEMP/quarantine/report.json"
if [ ! -f "$report" ]; then
  echo "::warning title=dispatch-browser quarantined rows::the quarantine leg wrote no" \
    "report, so how its rows ended is unknown; the browser step above says why"
  exit 0
fi
jq -r 'def rows($path):
    (.specs[]? as $spec | $spec.tests[]
      | "\(.status)\t\($spec.file):\($spec.line) [\(.projectName)] \(($path + [$spec.title]) | join(" › "))"),
    (.suites[]? | rows($path + [.title]));
  [.suites[]? | rows([])] | sort | .[]' "$report" > "$RUNNER_TEMP/quarantine/rows.tsv"
{
  echo "### The quarantined rows"
  echo
  echo "How each row of packages/dispatch/e2e/quarantine.ts ended in this run, which" \
    "gates nothing: \`expected\` passed, \`flaky\` failed and then passed its retry," \
    "\`unexpected\` failed, \`skipped\` did not run."
  echo
} >> "$GITHUB_STEP_SUMMARY"
while IFS="$(printf '\t')" read -r status row; do
  echo "- \`$status\` $row" >> "$GITHUB_STEP_SUMMARY"
  echo "::notice title=dispatch-browser quarantined row::$status $row"
done < "$RUNNER_TEMP/quarantine/rows.tsv"
