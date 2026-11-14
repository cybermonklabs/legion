#!/usr/bin/env bash
# The merge job's verdict, which its four readings do not give: they report, and this one fails
# the job, on an upstream job whose result is neither success nor skipped and on a merged report
# that holds a failed row.
#
# Who runs it, and what it takes:
#
#   .github/scripts/dispatch-browser-verdict.sh <binaries> <shard> <alone> <multiprocess> <visual>
#
# The five are the results of the jobs this one needs, in that order: the merge job's
# `${{ needs.binaries.result }}`, `${{ needs.shard.result }}`, `${{ needs.alone.result }}`,
# `${{ needs.multiprocess.result }}` and `${{ needs.visual.result }}`. Each is `success`,
# `skipped`, `failure` or `cancelled`, and the last two are what this script fails on, by job
# name.
#
# They are arguments rather than keys of the step's `env:`, for the reason the shard job's head
# gives: a step `env:` carrying an expression drops that job out of check-ci-filters.sh's
# reduction of what a go command compiles.
#
# It reads $RUNNER_TEMP/report.json, the report the merge step wrote, for the failed rows of the
# run. The third of the job's failures, a leg that owed a blob report and uploaded none, is
# reading one's, where it is found.
#
# Run from anywhere: .github/scripts/dispatch-browser-verdict.sh success success skipped skipped success
# CI runs it in dispatch-browser.yaml's merge job, at the step "Fail when a leg failed or the
# merged report holds a failed row".
set -euo pipefail

usage() {
  echo "::error::dispatch-browser-verdict.sh takes the results of the five jobs it reports on:" \
    ".github/scripts/dispatch-browser-verdict.sh <binaries> <shard> <alone> <multiprocess>" \
    "<visual>. It was given: $*"
  exit 1
}

[ "$#" -eq 5 ] || usage "$@"

failed=0
for pair in "binaries:$1" "shard:$2" \
  "alone:$3" "multiprocess:$4" \
  "visual:$5"; do
  case "${pair#*:}" in
    success | skipped) ;;
    *)
      echo "::error::the ${pair%%:*} job's result is ${pair#*:}, which is neither success nor skipped"
      failed=1
      ;;
  esac
done
report="$RUNNER_TEMP/report.json"
if [ -f "$report" ]; then
  unexpected=$(jq '.stats.unexpected' "$report")
  if [ "$unexpected" -gt 0 ]; then
    echo "::error::the merged report holds $unexpected failed rows;" \
      "reading three above says which legs they came from"
    failed=1
  fi
fi
exit "$failed"
