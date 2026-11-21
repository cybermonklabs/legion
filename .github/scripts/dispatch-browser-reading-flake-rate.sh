#!/usr/bin/env bash
# Reading two of the merge job: how many rows of the run passed on a retry, as a count and as a
# percentage, with one line per flaky row for the step summary and one `::notice::` for a reader
# of the log.
#
# Who runs it: the step "Reading two, the flake rate" of
# .github/workflows/dispatch-browser.yaml's merge job;
# .github/ci-notes/dispatch-browser.md#what-reading-two-reads-and-how-it-names-a-flaky-row says
# why a flake can be read from the merged report alone and how a row is named. It takes no
# argument: it reads $RUNNER_TEMP/report.json, the
# report the merge step wrote, and writes to $GITHUB_STEP_SUMMARY. A run with no merged report is
# a warning and exit 0 here, reading one having already said why there is none.
# The runner sets $RUNNER_TEMP and $GITHUB_STEP_SUMMARY for every step.
#
# Run from anywhere, with $RUNNER_TEMP and $GITHUB_STEP_SUMMARY set:
#   .github/scripts/dispatch-browser-reading-flake-rate.sh
# CI runs it in dispatch-browser.yaml's merge job, at the step "Reading two, the flake rate".
set -euo pipefail

report="$RUNNER_TEMP/report.json"
if [ ! -f "$report" ]; then
  echo "::warning::no merged report, so no flake rate; the step above says why"
  exit 0
fi
total=$(jq '.stats.expected + .stats.unexpected + .stats.flaky + .stats.skipped' "$report")
flaky=$(jq '.stats.flaky' "$report")
rate=$(awk -v f="$flaky" -v t="$total" 'BEGIN { printf "%.2f", t ? 100 * f / t : 0 }')
rows=$(jq -r 'def rows($path):
    (.specs[]? as $spec | $spec.tests[] | select(.status == "flaky")
      | "\($spec.file):\($spec.line) [\(.projectName)] \(($path + [$spec.title]) | join(" › "))"),
    (.suites[]? | rows($path + [.title]));
  [.suites[]? | rows([])] | sort | .[]' "$report")
{
  echo "### The flake rate"
  echo
  echo "$flaky flaky rows of $total, which is $rate per cent."
  echo
  if [ -n "$rows" ]; then
    echo "$rows" | while IFS= read -r row; do echo "- $row"; done
  else
    echo "No row passed on a retry."
  fi
} >> "$GITHUB_STEP_SUMMARY"
echo "::notice title=dispatch-browser flake rate::$flaky flaky rows of $total, $rate per cent"
# One line per flaky row, so a log reader gets what the summary got. A row's trace is in
# its job's test-results artifact - dispatch-browser-test-results-<shard or alone leg>-<attempt>,
# dispatch-browser-embedder-test-results-<attempt> or
# dispatch-browser-multiprocess-test-results-<attempt> - which is uploaded on a green run too.
[ -z "$rows" ] || echo "$rows" | while IFS= read -r row; do
  echo "::notice title=dispatch-browser flaky row::$row"
done
