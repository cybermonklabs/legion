#!/usr/bin/env bash
# Reading one of the merge job: the rows each leg's log printed beside the rows the merged report
# holds, the legs that owed a blob report and uploaded none, the two counts held against each
# other once every owed blob arrived, and the shard legs' row lists held against each other, so a
# row of the selection that no leg ran, or that two ran, fails by name.
#
# Who runs it, and what it takes:
#
#   GH_TOKEN=<token> .github/scripts/dispatch-browser-reading-rows.sh <download conclusion> \
#     <selection conclusion> <job>
#
# <download conclusion> is what the step "Download every leg's blob report" concluded, the merge
# job's `${{ steps.download-blobs.conclusion }}`. <selection conclusion> is what the step "Take
# the newest attempt of each leg's blob report" concluded, `${{ steps.select-blobs.conclusion }}`.
# That second step writes the directory this reading reads; why this reading takes both is said
# where it reads them, below. <job> is the id of the job that calls the workflow, the merge job's
# `${{ inputs.job }}`, which the read of this run's jobs keeps to the jobs named under it.
# GH_TOKEN is the run's own `${{ github.token }}`, which the step's `env:` gives this script for
# the read of this run's jobs. It is never an argument: an argument is on the process's command
# line, which every process of the runner can read in /proc while the step runs.
#
# The conclusions and the job are arguments rather than keys of the step's `env:`, for the reason
# .github/ci-notes/dispatch-browser.md#the-shard-jobs-shell-rule-its-measured-steps-and-its-budget
# gives: a step `env:` key outside ci_filter_coverage.py's ENV_ALLOWED drops that job
# out of check-ci-filters.sh's reduction of what a go command compiles. GH_TOKEN is in that set,
# so that reason does not hold for the token, which is a key of the step's `env:`.
#
# It reads $RUNNER_TEMP/selected-blobs, which holds one blob report and one row count per leg and
# each shard leg's two row lists, and $RUNNER_TEMP/report.json, the merged report; it writes
# $RUNNER_TEMP/jobs.json, which readings three and five read, and the table and the figures it
# adds to $GITHUB_STEP_SUMMARY. The step keeps its own `timeout-minutes`, which
# .github/ci-notes/dispatch-browser.md#the-merge-jobs-measured-steps-and-its-budget sizes against
# the ladder the read goes through.
#
# The step runs it from the repository root and carries no working directory of its own: the
# call to .github/scripts/dispatch-browser-run-reads.sh below is relative to that root.
#
# The runner sets five variables it needs for every step: $RUNNER_TEMP and $GITHUB_STEP_SUMMARY,
# and $GITHUB_REPOSITORY, $GITHUB_RUN_ID and $GITHUB_RUN_ATTEMPT, which that call of
# dispatch-browser-run-reads.sh reads for this run's jobs.
#
# Run from the repository root, with those five set:
#   GH_TOKEN=<token> .github/scripts/dispatch-browser-reading-rows.sh success success dispatch-browser
# CI runs it in dispatch-browser.yaml's merge job, at the step "Reading one, the rows".
set -euo pipefail

usage() {
  echo "::error::dispatch-browser-reading-rows.sh takes the blob download's conclusion, the" \
    "blob selection's conclusion and the id of the job that calls dispatch-browser.yaml, with" \
    "the run's token in GH_TOKEN: GH_TOKEN=<token>" \
    ".github/scripts/dispatch-browser-reading-rows.sh <download conclusion>" \
    "<selection conclusion> <job>. It was given: $*"
  exit 1
}

[ "$#" -eq 3 ] && [ -n "$3" ] || usage "$@"

blobs="$RUNNER_TEMP/selected-blobs/blob-report"
counts="$RUNNER_TEMP/selected-blobs/rows"
report="$RUNNER_TEMP/report.json"
jobs="$RUNNER_TEMP/jobs.json"
# What the steps "Download every leg's blob report" and "Take the newest attempt of each leg's
# blob report" concluded. They are read here, in the body, rather than in a step `env:` block,
# for the reason
# .github/ci-notes/dispatch-browser.md#the-shard-jobs-shell-rule-its-measured-steps-and-its-budget gives
# and which the step's `GH_TOKEN` need not follow, that key being in ENV_ALLOWED.
#
# This reading reads only what the selection wrote. The selection runs only after a download
# that succeeded, and the step "Merge the legs' blob reports into one report" only after the
# selection succeeded. So only `read`, both steps succeeded, says that a leg with no blob here
# sent none, and only then can a merged report exist. Otherwise this job read the legs' blobs
# in part or not at all: `refused` is a download that failed, `download` one that did not
# complete, and `selection` a selection that did not complete after a download that did. The
# message for those names the step and how it ended, and claims nothing about whether the legs
# uploaded.
downloaded=$1
selected=$2
# The id of the job that calls dispatch-browser.yaml: the jobs of this lane are the ones named
# under it.
caller=$3
if [ "$downloaded" = success ] && [ "$selected" = success ]; then
  input="read"
elif [ "$downloaded" = failure ]; then
  input=refused
elif [ "$downloaded" != success ]; then
  input=download
else
  input=selection
fi
# how_ended <conclusion>: the words for a step that did not succeed.
how_ended() {
  case "$1" in
    skipped) echo "did not run (it concluded skipped)" ;;
    cancelled) echo "was cancelled before it completed (it concluded cancelled)" ;;
    failure) echo "failed" ;;
    *) echo "did not run to its end (it concluded '$1')" ;;
  esac
}
# The run's own token is GH_TOKEN, a key of the step's `env:` and never an argument, for the
# reason this script's head gives. `actions: read` is the caller's, and ci.yaml's job that calls
# this workflow already holds it.
# This run's attempt's jobs, read once here and read again by reading three below
# and, on the pull-request and full lanes, by reading five. The call writes
# $RUNNER_TEMP/jobs.json: the array of {id, name, conclusion, started_at,
# completed_at, leg} that those readings take - the two timestamps are what reading
# five sums, and the jobs API answers them whether or not anything asks, so they
# cost this read nothing. The jobs API lists the caller's jobs too, and the jobs of
# ci.yaml's other callers of this workflow that share the run - dispatch-browser
# beside dispatch-browser-pr on a pull request labelled full-ci, dispatch-references
# beside dispatch-browser-pr on one that changes what the reference rows test - so
# the script holds the name to this caller and the jobs of this workflow, and maps each
# blob-producing job to its leg name (`real`, `stub`, `alone` or `shard-N`).
#
# The route is `.../runs/<id>/attempts/<n>/jobs?per_page=30` and not `filter=all`.
# `filter=all` lists every attempt's jobs, so each re-run made the read heavier:
# measured 2026-11-08 on run 42468236584 it answers 276 jobs over three pages, with
# GitHub answering HTTP 502 on pages 2 and 3, where the attempt endpoint answered 46
# jobs on one page of 100 for each of attempts 1 to 6. Job 128737742196 of that run exited 1
# on that 502 at 12:58:20Z, with 1291 browser rows and 0 failed. The page is 30 jobs and every
# page is followed, and the head of dispatch-browser-run-reads.sh ("Why thirty jobs to a page")
# says why.
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
.github/scripts/dispatch-browser-run-reads.sh jobs "$jobs" "$caller"
# The legs this lane owed, read from those jobs rather than written out a second time
# here. A job the lane skips by design owes nothing. A leg that failed before its blob
# upload is in the list and still owes its blob.
# A read that fails leaves the list empty, and the check below says so: `|| true` keeps
# pipefail and `set -e` from ending the script on jq's status first.
legs=$(jq -r '.[] | select(.leg != null and .conclusion != "skipped") | .leg' "$jobs" |
  sort) || true
if [ -z "$legs" ]; then
  echo "::error::this run's jobs hold no browser leg that owes a blob, so there is nothing to" \
    "hold the merged report against"
  exit 1
fi
missing=""
printed_total=0
# The legs whose row count this job read, and those whose count it did not: a leg with no count
# file, or a count file that holds no whole number, is shown as not read rather than counted as 0.
counted=0
unread_legs=""
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
  printed=""
  if [ -f "$counts/$leg.txt" ]; then
    printed=$(cat "$counts/$leg.txt")
  fi
  case "$printed" in
    '' | *[!0-9]*)
      unread_legs="${unread_legs:+$unread_legs, }$leg"
      cell="not read"
      ;;
    *)
      printed_total=$((printed_total + printed))
      counted=$((counted + 1))
      cell=$printed
      ;;
  esac
  if [ -f "$blobs/report-$leg.zip" ]; then
    held=yes
  else
    missing="${missing:+$missing, }$leg"
    # MISSING only where the download and the selection both succeeded and found no blob for the
    # leg. Otherwise this job never read the leg's blob, and the table says what this reading
    # knows: not read.
    if [ "$input" = read ]; then
      held=MISSING
    else
      held="not read"
    fi
  fi
  line="$line$leg $cell, "
  echo "| $leg | $cell | $held |" >> "$GITHUB_STEP_SUMMARY"
done <<< "$legs"
# What this job holds of the merged report, as `report_state`:
# - `none`: there is no report. The merge step did not run or failed, and this reading still
#   names the legs that owed a blob, which is what says why.
# - `unsummed`: jq could not sum its stats - not JSON, or JSON of another shape such as `[]` or a
#   count that is a string. jq's own message, above this step's lines, says why.
# - `not_whole`: its stats sum to something that is not a whole number of rows - nothing, `null`
#   or a fraction - which no count can be held against.
# - `read`: its stats sum to a whole number of rows, $merged.
merged=""
report_state=none
sum='.stats.expected + .stats.unexpected + .stats.flaky + .stats.skipped'
if [ -f "$report" ]; then
  if ! merged=$(jq "$sum" "$report"); then
    report_state=unsummed
  else
    case "$merged" in
      '' | *[!0-9]*) report_state=not_whole ;;
      *) report_state="read" ;;
    esac
  fi
fi
# With no whole number of rows from a merged report there is nothing to hold against the legs'
# counts, so the notice and the summary say what this job saw instead, and give each leg's count
# as this job read it.
if [ "$report_state" = read ]; then
  if [ -z "$unread_legs" ]; then
    printed_words="$printed_total printed by the legs"
  elif [ "$counted" -eq 0 ]; then
    printed_words="no count of the legs', since this job read none of them"
  elif [ "$counted" -eq 1 ]; then
    printed_words="$printed_total printed by the 1 leg whose count this job read"
  else
    printed_words="$printed_total printed by the $counted legs whose count this job read"
  fi
  summary_line="The merged report holds $merged rows, against $printed_words."
  notice="the merged report holds $merged rows, against $printed_words (${line%, })"
else
  case "$report_state" in
    none) seen="this job has no merged report" ;;
    unsummed) seen="this job could not sum the merged report's stats" ;;
    not_whole)
      seen="the merged report's stats sum to '$merged', so this job has no whole number of rows"
      ;;
  esac
  summary_line="${seen^} to hold against the legs' counts in the table above."
  notice="$seen to hold against the legs' counts: ${line%, }"
fi
{
  echo
  echo "$summary_line"
} >> "$GITHUB_STEP_SUMMARY"
echo "::notice title=dispatch-browser rows::$notice"
case "$input" in
  refused)
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
    echo "::error::the download of the legs' blob reports failed, so this job read no blob" \
      "report of: $missing, and made no merged report; the step \"Download every leg's blob" \
      "report\" carries that download's own error line. This error is a failed read of the" \
      "run's artifacts, not a failed browser row."
    exit 1
    ;;
  download | selection)
    if [ "$input" = download ]; then
      step="Download every leg's blob report"
      ended=$(how_ended "$downloaded")
    else
      step="Take the newest attempt of each leg's blob report"
      ended="$(how_ended "$selected") after the download succeeded"
    fi
    said=""
    if [ -n "$missing" ]; then
      said=" This job read no blob report of: $missing, which does not say that those legs sent none."
    fi
    if [ -n "$unread_legs" ]; then
      said="$said It read no row count of: $unread_legs."
    fi
    echo "::error::the step \"$step\" $ended, so this job made no merged report: the step" \
      "\"Merge the legs' blob reports into one report\" runs only after the selection" \
      "succeeds.$said This job's log shows what stopped that step."
    exit 1
    ;;
esac
# Both steps succeeded, so a leg with no blob here sent none. Where no leg sent one, the download
# succeeds with no artifact, the selection writes empty directories, and the merge step fails
# with nothing to merge; the job's limit can also stop the merge. With no merged report, there is
# none to be short of rows.
if [ -n "$missing" ]; then
  if [ -f "$report" ]; then
    said="the merged report is short of those legs' rows"
  else
    said="this job has no merged report"
  fi
  echo "::error::no blob report arrived from: $missing; $said"
  exit 1
fi
# The shard legs' row lists, held against each other: every row of the selection the legs read
# must have run on exactly one of them. The counts below cannot see a row no leg ran - it is
# missing from the legs' count and from the merged report alike - nor a file two legs ran, which
# both count twice. packages/dispatch/e2e/tools/leg-rows.ts says why and prints one line per
# fault, naming the rows and legs. The unsharded `real`, `stub` and `alone` legs write no lists.
# It runs before the counts so both readings print, and the step fails on either.
mapfile -t shard_legs < <(grep -E '^shard-[0-9]+$' <<< "$legs" || true)
lists=0
if [ "${#shard_legs[@]}" -gt 0 ]; then
  bun packages/dispatch/e2e/tools/leg-rows.ts "$counts" "${shard_legs[@]}" || lists=$?
fi
# Every owed blob arrived.
case "$report_state" in
  # No merged report after both steps that feed the merge succeeded: the merge step failed, or
  # the job's limit or its cancellation stopped it, in which case that step was skipped and
  # printed nothing. The notice above says this job has no merged report, the job's log says what
  # stopped the merge, and there is no count here to compare; the row lists still decide.
  none)
    [ "$lists" -eq 0 ] || exit 1
    exit 0
    ;;
  unsummed)
    echo "::error::jq could not sum the stats of the merged report $report (jq's own message is" \
      "above), so this reading cannot hold its rows against the legs' counts"
    exit 1
    ;;
  # A test on a sum that is not a whole number would fail inside an `if` below, where `set -e`
  # does not reach, and let the step pass having compared nothing.
  not_whole)
    echo "::error::the merged report's stats sum to '$merged', not a whole number of rows," \
      "so this reading cannot hold it against the legs' counts"
    exit 1
    ;;
esac
# A leg whose count was not read leaves nothing to compare its rows with, so the comparison
# below would read a short total as rows the merge dropped. It is named for what it is.
if [ -n "$unread_legs" ]; then
  echo "::error::every leg's blob report is present, but this job read no row count of:" \
    "$unread_legs, so the merged report's $merged rows cannot be held against the legs'" \
    "counts. A leg's count is the N of the last \"Running N tests\" line its own log printed;" \
    "the log of each leg named here shows whether it printed one."
  exit 1
fi
# Every owed blob arrived and every count was read, so the two counts are of the same rows and
# must be equal: both count every row the legs ran, skipped rows included. A merged total under
# the legs' is rows the merge dropped, and nothing else here would catch that - reading two and
# the verdict step both read the merged report alone, so a dropped failure would leave the run
# green. A merged total over the legs' is rows no leg's log counted, and the message says that
# and no more.
if [ "$merged" -lt "$printed_total" ]; then
  echo "::error::the merged report holds $merged rows and the legs printed" \
    "$printed_total, with every leg's blob report present:" \
    "the merge dropped rows the legs ran"
  exit 1
fi
if [ "$merged" -gt "$printed_total" ]; then
  echo "::error::the merged report holds $merged rows and the legs printed" \
    "$printed_total, with every leg's blob report present: the merged report holds rows" \
    "that no leg's log counted"
  exit 1
fi
[ "$lists" -eq 0 ] || exit 1
