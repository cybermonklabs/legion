#!/usr/bin/env bash
# Reading four of the merge job: what a row of each of the two layers costs, as a row count, a
# median and a 95th percentile, read from the merged report's passing results alone.
#
# Who runs it: the step "Reading four, the row durations" of
# .github/workflows/dispatch-browser.yaml's merge job, whose comment says which results the
# reading takes and why a failed or a skipped row is none of them. It takes no argument: it reads
# $RUNNER_TEMP/report.json, the report the merge step wrote, writes $RUNNER_TEMP/durations.txt,
# one line per passing result, and adds its table to $GITHUB_STEP_SUMMARY. A run with no merged
# report is a warning and exit 0 here, reading one having already said why there is none.
#
# Run from anywhere: .github/scripts/dispatch-browser-reading-row-durations.sh
# CI runs it in dispatch-browser.yaml's merge job, at the step "Reading four, the row durations".
set -euo pipefail

report="$RUNNER_TEMP/report.json"
if [ ! -f "$report" ]; then
  echo "::warning::no merged report, so no durations; reading one above says why"
  exit 0
fi
# One line per passing result: the class it belongs to and its duration in
# milliseconds. `stub` and `stub-phone` are the stubbed layer; every other project is
# a row against the real stack.
jq -r '[.. | objects | select(has("specs")) | .specs[] | .tests[]] | .[]
  | (if (.projectName | startswith("stub")) then "stubbed" else "real-stack" end) as $class
  | .results[] | select(.status == "passed") | "\($class) \(.duration)"' "$report" \
  > "$RUNNER_TEMP/durations.txt"
{
  echo "### The row durations"
  echo
  echo "| class | rows | median ms | 95th percentile ms |"
  echo "| --- | ---: | ---: | ---: |"
} >> "$GITHUB_STEP_SUMMARY"
for class in stubbed real-stack; do
  # The nearest-rank percentile of the sorted durations, which is the reading the
  # per-test budget's own jq takes, and three zeroes where the lane ran no row of
  # that class - the pull-request lane before a row moved, or a shard that drew none.
  stats=$(awk -v want="$class" '$1 == want { print $2 }' "$RUNNER_TEMP/durations.txt" |
    sort -n |
    awk '{ d[NR] = $1 } END { if (NR == 0) { print 0, 0, 0 } else {
      print NR, d[int((NR + 1) / 2)], d[NR < 2 ? 1 : int((NR * 95 + 99) / 100)] } }')
  # Word splitting is what reads the three figures, so `$stats` is deliberately
  # unquoted; `set --` is the one way to do that without an array, which this step's
  # `bash -e {0}` would take but `sh` would not.
  # shellcheck disable=SC2086
  set -- $stats
  echo "| $class | $1 | $2 | $3 |" >> "$GITHUB_STEP_SUMMARY"
  echo "::notice title=dispatch-browser row durations::$class: $1 rows," \
    "median $2 ms, 95th percentile $3 ms"
done
