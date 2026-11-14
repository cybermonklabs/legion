#!/usr/bin/env bash
# Reading one of the merge job: the rows each leg's log printed beside the rows the merged report
# holds, the legs that owed a blob report and uploaded none, and the two counts held against each
# other once every owed blob arrived.
#
# Who runs it, and what it takes:
#
#   .github/scripts/dispatch-browser-reading-rows.sh <download conclusion> <token>
#
# <download conclusion> is what the step "Download every leg's blob report" concluded, the merge
# job's `${{ steps.download-blobs.conclusion }}`: the missing-blob message below reads it,
# a refused download being why no blob arrived for any leg. <token> is the run's own
# `${{ github.token }}`, which this script exports as GH_TOKEN for the read of this run's jobs.
#
# Both are arguments rather than keys of the step's `env:`, for the reason the shard job's head
# gives: a step `env:` carrying an expression drops that job out of check-ci-filters.sh's
# reduction of what a go command compiles.
#
# It reads $RUNNER_TEMP/selected-blobs, which holds one blob report and one row count per leg,
# and $RUNNER_TEMP/report.json, the merged report; it writes $RUNNER_TEMP/jobs.json, which
# reading three reads, and the table and the figures it adds to $GITHUB_STEP_SUMMARY. The step
# keeps its own `timeout-minutes`, which the job's head sizes against the ladder the read goes
# through.
#
# The step runs it from the repository root and carries no working directory of its own: the
# call to .github/scripts/dispatch-browser-run-reads.sh below is relative to that root.
#
# Run from the repository root: .github/scripts/dispatch-browser-reading-rows.sh success "$token"
# CI runs it in dispatch-browser.yaml's merge job, at the step "Reading one, the rows".
set -euo pipefail

usage() {
  echo "::error::dispatch-browser-reading-rows.sh takes the blob download's conclusion and the" \
    "run's token: .github/scripts/dispatch-browser-reading-rows.sh <download conclusion>" \
    "<token>. It was given: $*"
  exit 1
}

[ "$#" -eq 2 ] || usage "$@"

blobs="$RUNNER_TEMP/selected-blobs/blob-report"
counts="$RUNNER_TEMP/selected-blobs/rows"
report="$RUNNER_TEMP/report.json"
jobs="$RUNNER_TEMP/jobs.json"
# What the step "Download every leg's blob report" concluded. It is read here, in the
# body, rather than in a step `env:` block, for the reason the shard job's head gives
# and which the `GH_TOKEN` export two lines below already follows. The missing-blob
# message reads it: a download that was refused is why no blob arrived for any leg.
downloaded=$1
# The run's own token, exported here rather than in an `env:` block, for the reason the
# shard job's head gives. `actions: read` is the caller's, and ci.yaml's job that calls
# this workflow already holds it.
export GH_TOKEN="$2"
# This run's attempt's jobs, read once here and read again by reading three below.
# The call writes $RUNNER_TEMP/jobs.json: the array of {id, name, conclusion, leg}
# that both readings take. The jobs API lists the caller's jobs too, so the script
# holds the name to this workflow's seven - binaries, shard, alone, multiprocess,
# embedder, visual and merge; a called workflow's job is named
# `<the caller's job> / <this job>`, with a matrix leg in brackets after it.
#
# The route is `.../runs/<id>/attempts/<n>/jobs?per_page=100` and not `filter=all`.
# `filter=all` lists every attempt's jobs, so each re-run made the read heavier:
# measured 2026-11-08 on run 42468236584 it answers 276 jobs over three pages, with
# GitHub answering HTTP 502 on pages 2 and 3, where the attempt endpoint answers 46
# jobs on one page for each of attempts 1 to 6. Job 128737742196 of that run exited 1
# on that 502 at 12:58:20Z, with 1291 browser rows and 0 failed.
#
# Nor the default filter, `latest`, which gives the same rows today and is what
# .github/scripts/ci-ok-report.sh reads: a run re-run while this job is running moves
# `latest` on, so the merge job of attempt N would report the legs of attempt N+1,
# which it never joined, where the attempt endpoint is pinned to the attempt whose
# blobs this job merged.
#
# There is no grouping left. One attempt's snapshot holds one job per name, so the
# `group_by(.name) | map(max_by(.run_attempt) | …)` this read carried is a no-op, and
# it is gone. The leg derivation and the name filter are the script's now, and its
# head carries why `visual` is in that filter: a run whose only red job is that one
# must not read "every leg of this workflow concluded success".
#
# The attempt any message names is this run's own GITHUB_RUN_ATTEMPT and never a job's
# `run_attempt` field: the attempt endpoint re-labels a leg carried forward from an
# earlier attempt with a new job id and a `run_attempt` equal to the attempt asked
# for. Measured on run 42468236584 at attempt 6, `dispatch-browser / shard (1)`
# appears there with job id 128737742824, `run_attempt: 6` and `started_at
# 2026-11-08T09:41:30Z`, which is attempt 1's time.
#
# The script retries the read through .github/scripts/gh-read.sh's ladder and, when
# that is spent, names the endpoint, the status and the attempt and says it is a
# failed read rather than a failed row, which is why this step no longer carries a
# `|| { echo …; exit 1; }` block of its own. It still reads with `--paginate --slurp`
# and runs the filter as a second `jq`, gh refusing `--slurp` beside `--jq`; the
# script's head says the rest.
.github/scripts/dispatch-browser-run-reads.sh jobs "$jobs"
# The legs this lane owed, read from those jobs rather than written out a second time
# here. A leg the lane does not run is skipped by its job's own `if:` - the alone and
# multiprocess jobs on the pull-request lane - and owes nothing. A leg that failed
# before it could upload is in the list and still owes its blob.
legs=$(jq -r '.[] | select(.leg != null and .conclusion != "skipped") | .leg' "$jobs" |
  sort)
if [ -z "$legs" ]; then
  echo "::error::this run's jobs hold no shard or alone leg, so there is nothing to" \
    "hold the merged report against"
  exit 1
fi
missing=""
printed_total=0
line=""
{
  echo "### The rows"
  echo
  echo "| leg | rows its log printed | blob report |"
  echo "| --- | --- | --- |"
} >> "$GITHUB_STEP_SUMMARY"
# One leg per line, not `for leg in $legs`: a matrix value may hold a space, and the
# capture above takes it, so word splitting would read one leg as two. A here-string
# rather than a pipe, because this loop adds up $printed_total and $missing and a
# pipe would run it in a subshell and throw both away.
while IFS= read -r leg; do
  [ -n "$leg" ] || continue
  printed=$(cat "$counts/$leg.txt" 2>/dev/null || true)
  case "$printed" in '' | *[!0-9]*) printed=0 ;; esac
  if [ -f "$blobs/report-$leg.zip" ]; then
    held=yes
  else
    held=MISSING
    missing="$missing $leg"
  fi
  printed_total=$((printed_total + printed))
  line="$line$leg $printed, "
  echo "| $leg | $printed | $held |" >> "$GITHUB_STEP_SUMMARY"
done <<< "$legs"
# `unknown` where the merge step failed: this reading still names the legs that owed a
# blob, which is what says why.
merged=unknown
if [ -f "$report" ]; then
  merged=$(jq '.stats.expected + .stats.unexpected + .stats.flaky + .stats.skipped' "$report")
fi
{
  echo
  echo "The merged report holds $merged rows; the legs' logs printed $printed_total."
} >> "$GITHUB_STEP_SUMMARY"
echo "::notice title=dispatch-browser rows::the merged report holds $merged rows," \
  "against $printed_total printed by the legs (${line%, })"
if [ -n "$missing" ]; then
  # Where the download was refused, this step runs anyway - its `if:` is `always()` -
  # and finds no blob for any leg, so the step "Report a refused blob download as a
  # refused read" has already said the artifacts could not be downloaded and this one
  # would then list every leg as though the legs produced nothing. Both lines are
  # true and that one comes first, but the last annotation is what a reader carries
  # away: two true lines that leave a false impression are a reporting defect by this
  # lane's own standard, so where the download step concluded `failure` the reason
  # named here is the refused download. The legs are named and nothing more is
  # claimed of them: a leg that died before its upload - shards 3 and 4 of run
  # 42328655988, on a `bun install` failure - wrote no blob at all, so this line says
  # the download failed rather than that each leg's blob is waiting on the run.
  if [ "$downloaded" = failure ]; then
    echo "::error::the download of the legs' blob reports failed, so no leg's blob" \
      "could be read and the merged report is short of the rows of:$missing; the" \
      "step \"Download every leg's blob report\" carries that download's own error" \
      "line. This is a failed read of the run's artifacts and not a failed browser" \
      "row: no row of the suite failed here."
  else
    echo "::error::no blob report arrived from:$missing;" \
      "the merged report is short of those legs' rows"
  fi
  exit 1
fi
# Every owed blob arrived, so the two counts are of the same rows and must be equal:
# both count every row the legs ran, skipped rows included. They differ when the merge
# dropped rows the legs ran, and nothing else here would catch that - reading two and
# the verdict step both read the merged report alone, so a dropped failure would leave
# the run green. A leg whose log printed no count lands here too, and the table above
# says which leg it was.
if [ "$merged" != unknown ] && [ "$merged" -ne "$printed_total" ]; then
  echo "::error::the merged report holds $merged rows and the legs printed" \
    "$printed_total, with every leg's blob report present:" \
    "the merge dropped rows the legs ran"
  exit 1
fi
