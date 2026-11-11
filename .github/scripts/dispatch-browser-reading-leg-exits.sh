#!/usr/bin/env bash
# Reading three of the merge job: the legs of this workflow that concluded anything but success,
# each named with what its own blob report holds, so that a leg which died outside the suite
# reads apart from a leg whose rows failed.
#
# Who runs it: the step "Reading three, the leg exits with no failed row" of
# .github/workflows/dispatch-browser.yaml's merge job, whose comment says why that difference is
# worth a reading of its own. It takes no argument: it reads $RUNNER_TEMP/jobs.json, which
# reading one wrote, and the legs' blob reports in $RUNNER_TEMP/selected-blobs/blob-report, and
# it writes $RUNNER_TEMP/unhappy-jobs.tsv, a merged report per unhappy leg under
# $RUNNER_TEMP/leg-<leg>, and the list it adds to $GITHUB_STEP_SUMMARY. A run with no jobs list
# is a warning and exit 0 here, reading one having already said why there is none.
#
# It needs no working directory of its own: the one command that does, the per-leg
# `merge-reports`, runs in a subshell that enters $GITHUB_WORKSPACE/packages/dispatch itself.
#
# Run from anywhere: .github/scripts/dispatch-browser-reading-leg-exits.sh
# CI runs it in dispatch-browser.yaml's merge job, at the step "Reading three, the leg exits with
# no failed row".
set -euo pipefail

blobs="$RUNNER_TEMP/selected-blobs/blob-report"
jobs="$RUNNER_TEMP/jobs.json"
unhappy="$RUNNER_TEMP/unhappy-jobs.tsv"
# The jobs reading one read: this attempt's snapshot of each job of this workflow,
# with its id, its name, its conclusion and its leg. A job id in it is this attempt's
# id for that leg, because the attempt endpoint gives a leg carried forward from an
# earlier attempt an id of its own - job 128737742824 for `shard (1)` at attempt 6 of
# run 42468236584. That step says why the route is this attempt's. It is read here
# rather than read again so the two readings cannot disagree about which attempt ran.
if [ ! -f "$jobs" ]; then
  echo "::warning::no jobs list, so no leg exits; reading one above says why"
  exit 0
fi
# Every one of those jobs that has concluded and did not succeed. This job's own
# conclusion is still null while this step runs, so it selects itself out.
#
# `skipped` is left out too, and not because a skip is uninteresting: on the
# pull-request lane the alone and multiprocess jobs are skipped by their own
# `if: inputs.lane == 'full'`, so naming them here would report the lane working as
# designed as a leg that did not succeed. A skip that is not by design is caught where
# it matters instead: reading one fails this job when a leg the lane owed a blob report
# did not upload one, and the verdict step fails on an upstream result that is neither
# success nor skipped.
# `.leg` is the name derived once above, not parsed again here. It is written as `-`
# when the job is no leg, never as an empty field: a tab is IFS whitespace, so an empty
# field would be swallowed and `read` would take the next one in its place. `name` stays
# last, where `read -r` lets it hold the spaces a job name carries.
jq -r '.[]
  | select(.conclusion != null and .conclusion != "success" and .conclusion != "skipped")
  | [.id, .conclusion, (.leg // "-"), .name] | @tsv' "$jobs" > "$unhappy"
{
  echo "### The legs that did not succeed"
  echo
} >> "$GITHUB_STEP_SUMMARY"
if [ ! -s "$unhappy" ]; then
  echo "Every leg of this workflow concluded success." >> "$GITHUB_STEP_SUMMARY"
  echo "::notice title=dispatch-browser leg exits::every leg of this workflow concluded success"
  exit 0
fi
while IFS="$(printf '\t')" read -r id conclusion leg name; do
  if [ "$leg" = "-" ]; then
    text="job $id ($name) ended $conclusion; it is no shard, so the merged report holds no row of it"
  elif [ ! -f "$blobs/report-$leg.zip" ]; then
    text="job $id ($name) ended $conclusion and uploaded no blob report, so the merged report holds none of its rows"
  else
    # That leg's own rows, which are its part of the merged report: the merged report
    # does not say which leg ran a row, and this leg's blob is exactly the rows it
    # contributed to it.
    work="$RUNNER_TEMP/leg-$leg"
    mkdir -p "$work/in"
    cp "$blobs/report-$leg.zip" "$work/in/"
    # `|| true`, because `merge-reports` exits 1 on a report holding a failed row,
    # which is the case this reading is looking for: under `bash -e` the bare call
    # would end the step at the first red leg, before it named any of them. What the
    # call wrote is read instead, and a call that wrote nothing is said so.
    ( cd "$GITHUB_WORKSPACE/packages/dispatch" &&
      PLAYWRIGHT_JSON_OUTPUT_NAME="$work/report.json" bunx playwright merge-reports --reporter=json "$work/in" > /dev/null ) || true
    if [ ! -f "$work/report.json" ]; then
      text="job $id ($name) ended $conclusion and its blob report could not be read, so what it ran is unknown"
    else
      failed=$(jq '.stats.unexpected' "$work/report.json")
      if [ "$failed" -eq 0 ]; then
        text="job $id ($name) ended $conclusion with no failed row in the merged report, so it died outside the suite"
      else
        text="job $id ($name) ended $conclusion with $failed failed rows in the merged report"
      fi
    fi
  fi
  echo "- $text" >> "$GITHUB_STEP_SUMMARY"
  echo "::notice title=dispatch-browser leg exits::$text"
done < "$unhappy"
