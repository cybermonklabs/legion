#!/usr/bin/env bash
# The three halves of the approval-time run of a large pull request
# (.github/ci-notes/ci.md#a-large-pull-request-runs-its-slow-suites-once-at-the-approved-head).
#
# A pull request's run holds the slow suites back (they run in the merge queue), so a defect only
# a slow suite finds first fails in the queue, which removes the entry and rebuilds every group
# behind it. A large pull request is where that costs most, so its slow suites run once more
# before the queue: at the head a reviewer approves. That run is
# .github/workflows/large-pr-lanes.yaml, which calls ci.yaml on the approval's pull_request_review
# event; on that event every `changes` output a pull request holds back takes its paths filter's
# verdict, as on a merge group.
#
#   label  large-pr-label.yaml's job on a pull request: reads the pull request's size and puts the
#          label large-pr on a large pull request, or takes it off one that is no longer large.
#          Large is LARGE_FILES (100) changed files or more, or LARGE_LINES (10,000) additions
#          plus deletions or more. The approval workflow reads the label rather than the size,
#          because a review's event carries no size (its payload holds a simple pull request: no
#          additions, deletions or changed_files) and the workflow's jobs must be decided from the
#          event alone, before any runner starts: see `hold`. The size and the labels are read
#          from the pull_request event's payload (GITHUB_EVENT_PATH), and from the pull request
#          itself only where the payload leaves a size out. A fork's pull request is passed over:
#          its token cannot write a label, and its change reaches the queue only once a maintainer
#          takes the branch into this repository, where it is labelled at the next push.
#
#   lead   the `lead` job of large-pr-lanes.yaml: decides whether this approval runs the suites
#          (`run=true` in GITHUB_OUTPUT) or follows the approval-time run already at this head
#          (`run=false`, `follows=<run id>`). It follows the earliest earlier run of that workflow
#          at HEAD_SHA whose ci-ok job is still open or passed (`target`). So two approval-time
#          runs never test one head at once: ci.yaml's concurrency group on this event is the
#          merge commit, and browser-image.yaml's build group too, and GitHub keeps one pending
#          run per group, cancelling it when a third arrives, which would fail a ci-ok with
#          nothing failed. A refused read runs the suites, with a warning.
#
#   hold   large-pr-lanes.yaml's job named ci-ok. It has no needs, so GitHub creates its check run,
#          queued, the moment it creates the run, and that pending ci-ok holds the pull request out
#          of the queue: a required check that exists and is pending makes GitHub refuse the
#          enqueue. Then:
#          * when this run's CALLER runs (a job `<CALLER> / …` is listed), it waits for
#            `<CALLER> / ci-ok`, ci.yaml's own verdict, which GitHub creates only once every job
#            it needs has ended, and concludes as it did: success exits 0, anything else exits 1.
#            A run whose CALLER jobs all ended with no verdict of this attempt
#            (LARGE_PR_HOLD_ENDED_READS reads in a row) fails;
#          * when CALLER is skipped (`lead` chose to follow), it waits for the ci-ok of `target`'s
#            run and concludes as that did. When no such run is left (it failed or was cancelled
#            since `lead` read it), it fails, naming the run to re-run;
#          * when neither is listed LARGE_PR_HOLD_START_SECONDS after `lead` ended (or after this
#            job began, with no `lead` listed), it fails, naming why: the caller never started.
#
# Its inputs, from the step's env:
#   label: GH_TOKEN, and GITHUB_EVENT_PATH and GITHUB_REPOSITORY as Actions sets them.
#   lead:  GH_TOKEN, HEAD_SHA (the pull request's head), and GITHUB_REPOSITORY, GITHUB_RUN_ID and
#          GITHUB_OUTPUT as Actions sets them.
#   hold:  GH_TOKEN, HEAD_SHA, GITHUB_REPOSITORY, GITHUB_RUN_ID, GITHUB_RUN_ATTEMPT (as Actions
#          sets them), CALLER (the id of large-pr-lanes.yaml's job that calls ci.yaml),
#          LARGE_PR_HOLD_POLL_SECONDS (optional, 60 by default; the tests set 0),
#          LARGE_PR_HOLD_ENDED_READS (optional, 3 by default) and LARGE_PR_HOLD_START_SECONDS
#          (optional, 300 by default).
#   GH_RETRY_SECONDS   optional: gh-read.sh's knob, the tests' way to retry without waiting.
#
# Run from anywhere: large-pr-lanes.sh label|lead|hold. Its tests are in large-pr-lanes.test.sh,
# which ci.yaml's test job runs.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=.github/scripts/gh-read.sh
source "$script_dir/gh-read.sh"

LABEL='large-pr'
LARGE_FILES=100
LARGE_LINES=10000

need() {
  local input
  for input in "$@"; do
    if [ -z "${!input:-}" ]; then
      echo "::error::large-pr-lanes.sh $mode needs $input in its environment"
      exit 1
    fi
  done
}

label() {
  need GH_TOKEN GITHUB_EVENT_PATH GITHUB_REPOSITORY
  local fields number head_repo additions deletions changed has
  if ! fields=$(jq -er --arg label "$LABEL" '.pull_request
      | if type == "object" and (.number | type) == "number" then . else error("no pull request") end
      | [.number, (.head.repo.full_name // "-"), (.additions // "-"), (.deletions // "-"),
         (.changed_files // "-"), ([.labels[]?.name | ascii_downcase] | index($label) != null)]
      | @tsv' "$GITHUB_EVENT_PATH" 2>/dev/null); then
    echo "::error::large-pr-lanes.sh label reads a pull_request event, and $GITHUB_EVENT_PATH carries no pull request"
    exit 1
  fi
  IFS=$'\t' read -r number head_repo additions deletions changed has <<<"$fields"
  if [ "$head_repo" != "$GITHUB_REPOSITORY" ]; then
    echo "#$number comes from $head_repo, a fork, whose token cannot write a label: passed over; it is labelled at its first push once a maintainer takes the branch into $GITHUB_REPOSITORY"
    return 0
  fi
  if [ "$additions" = - ] || [ "$deletions" = - ] || [ "$changed" = - ]; then
    if ! fields=$(gh_read gh api "repos/$GITHUB_REPOSITORY/pulls/$number" \
      --jq '[.additions, .deletions, .changed_files] | @tsv'); then
      echo "::error::the event left out the size of #$number and GitHub refused the pull request at every attempt gh-read.sh made, so this step cannot tell whether an approval of it must run the slow suites"
      exit 1
    fi
    IFS=$'\t' read -r additions deletions changed <<<"$fields"
  fi
  local value
  for value in "$additions" "$deletions" "$changed"; do
    if ! [[ $value =~ ^[0-9]+$ ]]; then
      echo "::error::the size of #$number reads '$additions' additions, '$deletions' deletions and '$changed' changed files, which are not all whole numbers"
      exit 1
    fi
  done
  local lines=$((additions + deletions)) large=false
  if [ "$changed" -ge "$LARGE_FILES" ] || [ "$lines" -ge "$LARGE_LINES" ]; then
    large=true
  fi
  local size="$changed changed files and $lines changed lines (large is $LARGE_FILES files or $LARGE_LINES lines)"
  case $large/$has in
    true/true) echo "#$number is large, $size, and carries $LABEL already" ;;
    false/false) echo "#$number is not large, $size, and carries no $LABEL" ;;
    true/false)
      # A large pull request without the label gets no approval-time run, so a write GitHub
      # refused at every attempt fails the step: the rule would otherwise lapse unseen.
      if ! gh_read gh api --method POST "repos/$GITHUB_REPOSITORY/issues/$number/labels" \
        -f "labels[]=$LABEL" --silent; then
        echo "::error::GitHub refused the label $LABEL on #$number at every attempt gh-read.sh made, so an approval of it would run no slow suite before the merge queue; it has $size"
        exit 1
      fi
      echo "#$number is large, $size: labelled $LABEL, so an approval runs the slow suites its paths select"
      ;;
    false/true)
      # A label left on costs one approval-time run and blocks nothing, so a refusal only warns.
      if ! gh_read gh api --method DELETE "repos/$GITHUB_REPOSITORY/issues/$number/labels/$LABEL" \
        --silent; then
        echo "::warning::GitHub refused to take $LABEL off #$number at every attempt gh-read.sh made, so its next approval runs the slow suites although it has $size"
        return 0
      fi
      echo "#$number is not large, $size: took $LABEL off"
      ;;
  esac
}

# earlier_runs: one line `<run id> <status> <conclusion>` for each run of large-pr-lanes.yaml at
# HEAD_SHA older than this one, oldest first, read off its job named ci-ok. Returns 1 when GitHub
# refused a read at every attempt.
earlier_runs() {
  local runs id state
  runs=$(gh_read gh api --paginate \
    "repos/$GITHUB_REPOSITORY/actions/runs?event=pull_request_review&head_sha=$HEAD_SHA&per_page=100" \
    --jq ".workflow_runs[] | select(.path == \"$WORKFLOW_PATH\" and .id < $GITHUB_RUN_ID) | .id") || return 1
  while read -r id; do
    [ -n "$id" ] || continue
    state=$(gh_read gh api "repos/$GITHUB_REPOSITORY/actions/runs/$id/jobs?filter=latest&per_page=100" \
      --jq '.jobs[] | select(.name == "ci-ok") | "\(.status) \(.conclusion // "-")"') || return 1
    echo "$id $state"
  done < <(sort -n <<<"$runs")
}

# target: the id of the run to follow, or nothing: the earliest of earlier_runs whose ci-ok is still
# open (it tests, or follows one that does) or concluded success. Returns 1 as earlier_runs does.
target() {
  local rows
  rows=$(earlier_runs) || return 1
  awk '$2 == "completed" && $3 == "success" || $2 ~ /^(queued|in_progress|waiting|pending|requested)$/ { print $1; exit }' \
    <<<"$rows"
}

lead() {
  need GH_TOKEN GITHUB_REPOSITORY GITHUB_RUN_ID HEAD_SHA GITHUB_OUTPUT
  local follow
  if ! follow=$(target); then
    echo "::warning::GitHub refused the approval-time runs at $HEAD_SHA at every attempt gh-read.sh made, so this approval runs the suites itself. If another approval-time run is testing this head, this run's call of ci.yaml waits behind it in ci.yaml's concurrency group (ci-<merge commit>), lists no job meanwhile, and this run's ci-ok then fails ${LARGE_PR_HOLD_START_SECONDS:-300} s after this job ends: re-run its failed jobs once the other run has ended"
    echo "run=true" >>"$GITHUB_OUTPUT"
    return 0
  fi
  if [ -n "$follow" ]; then
    echo "run $follow, an approval-time run at $HEAD_SHA, is open or passed: this approval follows it and runs no suite of its own"
    echo "run=false" >>"$GITHUB_OUTPUT"
    return 0
  fi
  echo "no approval-time run at $HEAD_SHA is open or passed: this approval runs the suites"
  echo "run=true" >>"$GITHUB_OUTPUT"
}

# conclude <job name> <conclusion> <url> <what it is>: the hold's verdict, from one ci-ok.
conclude() {
  echo "$1 concluded $2: $3"
  if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
    printf '%s\n' "This ci-ok is the verdict of the approval-time run of a large pull request: [$1]($3), $4, concluded $2 (.github/ci-notes/ci.md#a-large-pull-request-runs-its-slow-suites-once-at-the-approved-head)." \
      >>"$GITHUB_STEP_SUMMARY"
  fi
  [ "$2" = success ] && return 0
  echo "::error::$1 concluded $2, so this ci-ok fails: that job's own log names the jobs that failed"
  return 1
}

# caller_rows <jobs> <field test>: the rows of this run's jobs that belong to CALLER's called
# workflow, less its own ci-ok, that the awk test keeps; `wc -l` of them.
caller_rows() {
  awk -F'\t' -v caller="$CALLER / " -v want="$CALLER / ci-ok" \
    "index(\$1, caller) == 1 && \$1 != want && ($2)" <<<"$1" | wc -l
}

# wait_for_caller: reads this run's jobs until CALLER has listed a job of the workflow it calls
# (this approval runs the suites) or has itself been skipped (`lead` chose to follow). It writes
# that last read into HOLD_JOBS and the case into HOLD_PHASE (`caller` or `skipped`), rather than
# printing them, so that the read it made is the next phase's first pass rather than a read of its
# own. It fails when no job of CALLER is listed HOLD_START_LIMIT s after `lead` ended.
wait_for_caller() {
  local started='' jobs skipped lead_open
  while true; do
    if jobs=$(gh_read gh api --paginate "$HOLD_ROUTE" \
      --jq '.jobs[] | [.name, .status, (.conclusion // "-"), (.run_attempt | tostring), .html_url] | @tsv'); then
      skipped=$(awk -F'\t' -v caller="$CALLER" '$1 == caller && $3 == "skipped"' <<<"$jobs")
      if [ "$(caller_rows "$jobs" 1)" -gt 0 ] || [ -n "$(awk -F'\t' -v want="$CALLER / ci-ok" '$1 == want' <<<"$jobs")" ]; then
        HOLD_JOBS=$jobs HOLD_PHASE=caller
        return 0
      fi
      if [ -n "$skipped" ]; then
        HOLD_JOBS=$jobs HOLD_PHASE=skipped
        return 0
      fi
      # Nothing of CALLER yet: `lead` has not ended, or the caller has not started.
      lead_open=$(awk -F'\t' '$1 == "lead" && $2 != "completed"' <<<"$jobs")
      if [ -n "$lead_open" ]; then
        started=''
      else
        [ -n "$started" ] || started=$SECONDS
        if [ $((SECONDS - started)) -ge "$HOLD_START_LIMIT" ]; then
          echo "::error::no job of $CALLER is listed $HOLD_START_LIMIT s after lead ended, neither its jobs nor its skip, so the caller never started (cancelled, or held where nothing will release it, as a second call of ci.yaml at this merge commit waits behind the first in ci.yaml's concurrency group) and this ci-ok fails"
          return 1
        fi
      fi
    else
      echo "::warning::GitHub refused this run's jobs at every attempt gh-read.sh made; reading again in $HOLD_POLL s"
    fi
    sleep "$HOLD_POLL"
  done
}

# hold_caller: this approval runs the suites, so the verdict is ci.yaml's own ci-ok under CALLER.
# The first pass reads HOLD_JOBS, which wait_for_caller already read. A verdict of this attempt
# decides. One of an earlier attempt decides only when no job of CALLER ran in this attempt (a
# re-run of this job alone): when its jobs re-ran, ci.yaml's ci-ok re-runs after them, and until
# GitHub creates it the newest one listed is the earlier attempt's.
hold_caller() {
  local want="$CALLER / ci-ok" ended=0 jobs=$HOLD_JOBS verdict status conclusion attempt url
  while true; do
    if [ -n "$jobs" ]; then
      verdict=$(awk -F'\t' -v want="$want" '$1 == want' <<<"$jobs")
      if [ -n "$verdict" ]; then
        IFS=$'\t' read -r _ status conclusion attempt url <<<"$verdict"
        if [ "$status" = completed ] &&
          { [ "$attempt" = "$GITHUB_RUN_ATTEMPT" ] || [ "$(caller_rows "$jobs" "\$4 == $GITHUB_RUN_ATTEMPT")" -eq 0 ]; }; then
          conclude "$want" "$conclusion" "$url" "which ran every fast job and every slow suite this change's paths select"
          return
        fi
      fi
      # GitHub creates ci.yaml's ci-ok only once its needs have ended, so a moment with every job
      # of CALLER completed and no verdict is normal; the same reading several reads in a row is a
      # run that will not reach one.
      if [ "$(caller_rows "$jobs" '$2 != "completed"')" -eq 0 ]; then
        ended=$((ended + 1))
        if [ "$ended" -ge "$HOLD_ENDED_READS" ]; then
          echo "::error::no job of $CALLER is open and $want has not concluded on attempt $GITHUB_RUN_ATTEMPT, $ended reads in a row, so this run reaches no verdict and this ci-ok fails"
          return 1
        fi
      else
        ended=0
      fi
    fi
    sleep "$HOLD_POLL"
    if ! jobs=$(gh_read gh api --paginate "$HOLD_ROUTE" \
      --jq '.jobs[] | [.name, .status, (.conclusion // "-"), (.run_attempt | tostring), .html_url] | @tsv'); then
      jobs=''
      echo "::warning::GitHub refused this run's jobs at every attempt gh-read.sh made; reading again in $HOLD_POLL s"
    fi
  done
}

# hold_follow: `lead` chose to follow the approval-time run already at this head, so this job
# concludes as that run's ci-ok does. The run is chosen once and kept.
hold_follow() {
  local followed='' rows verdict status conclusion url
  while true; do
    if [ -z "$followed" ]; then
      if ! rows=$(earlier_runs); then
        echo "::warning::GitHub refused the approval-time runs at $HEAD_SHA at every attempt gh-read.sh made; reading again in $HOLD_POLL s"
      else
        followed=$(awk '$2 == "completed" && $3 == "success" || $2 ~ /^(queued|in_progress|waiting|pending|requested)$/ { print $1; exit }' <<<"$rows")
        if [ -z "$followed" ]; then
          echo "::error::$CALLER is skipped, so this approval follows the approval-time run at $HEAD_SHA, and none of that head's runs is open or passed now: the one it followed failed or was cancelled. Re-run that run's failed jobs first (gh run rerun $(awk '{ printf "%s ", $1 }' <<<"$rows")--failed names the runs at this head), and then this run's, which follows its new attempt"
          return 1
        fi
        echo "following run $followed, the approval-time run at $HEAD_SHA"
      fi
    fi
    if [ -n "$followed" ]; then
      if verdict=$(gh_read gh api "repos/$GITHUB_REPOSITORY/actions/runs/$followed/jobs?filter=latest&per_page=100" \
        --jq '.jobs[] | select(.name == "ci-ok") | [.status, (.conclusion // "-"), .html_url] | @tsv'); then
        IFS=$'\t' read -r status conclusion url <<<"$verdict"
        if [ "$status" = completed ]; then
          conclude "the ci-ok of run $followed" "$conclusion" "$url" "the approval-time run at this head that this approval follows"
          return
        fi
      else
        echo "::warning::GitHub refused run $followed's jobs at every attempt gh-read.sh made; reading again in $HOLD_POLL s"
      fi
    fi
    sleep "$HOLD_POLL"
  done
}

# hold: the three cases of the header, in two phases. wait_for_caller says which of the first two
# this run is, and hold_caller or hold_follow then waits for that one verdict. Each read lists the
# newest execution of every job of the run it reads (`filter=latest`).
hold() {
  need GH_TOKEN GITHUB_REPOSITORY GITHUB_RUN_ID GITHUB_RUN_ATTEMPT CALLER HEAD_SHA
  HOLD_POLL=${LARGE_PR_HOLD_POLL_SECONDS:-60}
  HOLD_ENDED_READS=${LARGE_PR_HOLD_ENDED_READS:-3}
  HOLD_START_LIMIT=${LARGE_PR_HOLD_START_SECONDS:-300}
  HOLD_ROUTE="repos/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID/jobs?filter=latest&per_page=100"
  HOLD_JOBS='' HOLD_PHASE=''
  echo "waiting for this approval's verdict at $HEAD_SHA (attempt $GITHUB_RUN_ATTEMPT)"
  wait_for_caller || return 1
  case $HOLD_PHASE in
    caller) hold_caller ;;
    skipped) hold_follow ;;
  esac
}

WORKFLOW_PATH='.github/workflows/large-pr-lanes.yaml'
mode=${1:-}
case $mode in
  label) label ;;
  lead) lead ;;
  hold) hold ;;
  *)
    echo "::error::usage: large-pr-lanes.sh label|lead|hold"
    exit 2
    ;;
esac
