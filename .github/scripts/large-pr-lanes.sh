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
#          (`run=false`, the run it follows named in its log). It follows the earliest earlier run
#          of that workflow at HEAD_SHA whose ci-ok job is still open or passed (to_follow). So
#          two approval-time runs never test one head at once, unless GitHub refuses this read:
#          ci.yaml's concurrency group on this event is the merge commit, and browser-image.yaml's
#          build group too, and GitHub keeps one pending run per group, cancelling it when a
#          third arrives, which would fail a ci-ok with nothing failed. A refused read runs the
#          suites, with a warning that says what a second run then does (the third case below).
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
#          * when CALLER is skipped (`lead` chose to follow), it chooses again by to_follow, waits
#            for that run's ci-ok and concludes as it did. When no run is open or passed any more,
#            it fails;
#          * when neither is listed LARGE_PR_HOLD_START_SECONDS after `lead` ended (or after this
#            job began, with no `lead` listed), it fails, naming why: the caller was cancelled, or
#            it waits behind another approval-time run of this merge commit, which starts it once
#            it ends.
#          Every failure, and `lead`'s warning when it cannot read the runs, prints one remedy
#          (remedy): a `gh run rerun <id> --failed` for each approval-time run at this head whose
#          ci-ok failed, oldest first, this run's among them, then each later one the same way. A
#          run that followed another does not take that run's next verdict on its own, and GitHub
#          refuses a re-run of a run that is still running.
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
# The hold's three settings, read once: how long it sleeps between reads, how many reads in a row
# of a run with every job ended and no verdict fail it, and how long after `lead` ended it waits
# for the caller to list a job. `lead`'s warning quotes the last.
HOLD_POLL=${LARGE_PR_HOLD_POLL_SECONDS:-60}
HOLD_ENDED_READS=${LARGE_PR_HOLD_ENDED_READS:-3}
HOLD_START_LIMIT=${LARGE_PR_HOLD_START_SECONDS:-300}

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
# HEAD_SHA older than this one, oldest first, read off its job named ci-ok (a review that ran no
# ci-ok, its job skipped under another name, has neither field). Returns 1 when GitHub refused a
# read at every attempt.
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

# to_follow <rows of earlier_runs>: the run an approval follows, or nothing: the earliest whose
# ci-ok is still open (it tests, or follows one that does) or concluded success. `lead` chooses by
# it and `hold` chooses again by it, so the two cannot choose differently from the same rows.
to_follow() {
  awk '$2 == "completed" && $3 == "success" || $2 ~ /^(queued|in_progress|waiting|pending|requested)$/ { print $1; exit }' \
    <<<"$1"
}

# rerun_lines <rows of earlier_runs>: one `gh run rerun <id> --failed` for each run whose ci-ok
# concluded and did not pass, oldest first, which is the order they must be re-run in: each run
# after the first followed it, and follows the earliest run open or passed when it runs again.
# `gh run rerun` takes one run, and refuses a run still running (HTTP 403).
rerun_lines() {
  awk '$2 == "completed" && $3 != "success" { printf "gh run rerun %s --failed; ", $1 }' <<<"$1"
}

# remedy [<rows of earlier_runs>, or `-` when GitHub refused them]: what to re-run, and in what
# order, once this run's ci-ok has failed or may: every approval-time run at this head whose ci-ok
# failed, oldest first, this one among them, then each later one. Without rows it reads them again,
# since by the time a run fails the runs before it may have failed too (a follower of a follower).
# The one text every failure of this script prints.
remedy() {
  local rows earlier
  if [ $# -gt 0 ]; then
    rows=$1
  elif ! rows=$(earlier_runs); then
    rows=-
  fi
  if [ "$rows" = - ]; then
    earlier="first each earlier one whose ci-ok failed (GitHub refused the runs at this head, so they are not named here), then "
  else
    earlier=$(rerun_lines "$rows")
  fi
  printf '%s' "Re-run, oldest first and each once it has ended (GitHub refuses a re-run of a run still running, HTTP 403), every approval-time run at $HEAD_SHA whose ci-ok failed: ${earlier}gh run rerun $GITHUB_RUN_ID --failed (this run), and then each later one the same way. A run that followed another does not take that run's next verdict on its own: re-run, it follows the earliest earlier run that is open or passed, so the run it followed goes first."
}

lead() {
  need GH_TOKEN GITHUB_REPOSITORY GITHUB_RUN_ID HEAD_SHA GITHUB_OUTPUT
  local rows follow
  if ! rows=$(earlier_runs); then
    echo "::warning::GitHub refused the approval-time runs at $HEAD_SHA at every attempt gh-read.sh made, so this approval runs the suites itself. If another approval-time run is testing this head, this run's call of ci.yaml waits behind it in ci.yaml's concurrency group (ci-<merge commit>), which keeps it waiting rather than cancelling it (cancel-in-progress is false), lists no job meanwhile, and runs every suite once that run ends; this run's ci-ok then fails $HOLD_START_LIMIT s after this job ends. $(remedy -)"
    echo "run=true" >>"$GITHUB_OUTPUT"
    return 0
  fi
  follow=$(to_follow "$rows")
  if [ -n "$follow" ]; then
    echo "run $follow, an approval-time run at $HEAD_SHA, is open or passed: this approval follows it and runs no suite of its own"
    echo "run=false" >>"$GITHUB_OUTPUT"
    return 0
  fi
  echo "no approval-time run at $HEAD_SHA is open or passed: this approval runs the suites"
  echo "run=true" >>"$GITHUB_OUTPUT"
}

# conclude <job name> <conclusion> <url> <what it is>: the hold's verdict, from one ci-ok, with the
# remedy when it failed.
conclude() {
  echo "$1 concluded $2: $3"
  if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
    printf '%s\n' "This ci-ok is the verdict of the approval-time run of a large pull request: [$1]($3), $4, concluded $2 (.github/ci-notes/ci.md#a-large-pull-request-runs-its-slow-suites-once-at-the-approved-head)." \
      >>"$GITHUB_STEP_SUMMARY"
  fi
  [ "$2" = success ] && return 0
  echo "::error::$1 concluded $2, so this ci-ok fails: that job's own log names the jobs that failed. $(remedy)"
  return 1
}

# read_jobs <run id>: one tab-separated row per job of that run, its newest execution
# (`filter=latest`): name, status, conclusion, attempt, URL. Fails, with a warning, when GitHub
# refused the read at every attempt.
read_jobs() {
  gh_read gh api --paginate "repos/$GITHUB_REPOSITORY/actions/runs/$1/jobs?filter=latest&per_page=100" \
    --jq '.jobs[] | [.name, .status, (.conclusion // "-"), (.run_attempt | tostring), .html_url] | @tsv' && return 0
  echo "::warning::GitHub refused run $1's jobs at every attempt gh-read.sh made; reading again in $HOLD_POLL s" >&2
  return 1
}

# caller_rows <jobs> <field test>: the rows of this run's jobs that belong to CALLER's called
# workflow, less its own ci-ok, that the awk test keeps; `wc -l` of them.
caller_rows() {
  awk -F'\t' -v caller="$CALLER / " -v want="$CALLER / ci-ok" \
    "index(\$1, caller) == 1 && \$1 != want && ($2)" <<<"$1" | wc -l
}

# hold_caller <jobs>: this approval runs the suites, so the verdict is ci.yaml's own ci-ok under
# CALLER. Its first pass reads <jobs>, the read hold made. A verdict of this attempt decides. One
# of an earlier attempt decides only when no job of CALLER ran in this attempt (a re-run of this
# job alone): when its jobs re-ran, ci.yaml's ci-ok re-runs after them, and until GitHub creates
# it the newest one listed is the earlier attempt's.
hold_caller() {
  local want="$CALLER / ci-ok" ended=0 jobs=$1 verdict status conclusion attempt url
  while true; do
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
        echo "::error::no job of $CALLER is open and $want has not concluded on attempt $GITHUB_RUN_ATTEMPT, $ended reads in a row, so this run reaches no verdict and this ci-ok fails. $(remedy)"
        return 1
      fi
    else
      ended=0
    fi
    sleep "$HOLD_POLL"
    until jobs=$(read_jobs "$GITHUB_RUN_ID"); do sleep "$HOLD_POLL"; done
  done
}

# hold_follow: `lead` chose to follow the approval-time run already at this head, so this job
# concludes as that run's ci-ok does. The run is chosen once, by to_follow, and kept.
hold_follow() {
  local followed='' rows verdict status conclusion url
  until rows=$(earlier_runs); do
    echo "::warning::GitHub refused the approval-time runs at $HEAD_SHA at every attempt gh-read.sh made; reading again in $HOLD_POLL s"
    sleep "$HOLD_POLL"
  done
  followed=$(to_follow "$rows")
  if [ -z "$followed" ]; then
    echo "::error::$CALLER is skipped, so this approval follows the approval-time run at $HEAD_SHA, and none of that head's runs is open or passed now: the one it followed failed or was cancelled. $(remedy "$rows")"
    return 1
  fi
  echo "following run $followed, the approval-time run at $HEAD_SHA"
  while true; do
    if verdict=$(read_jobs "$followed"); then
      IFS=$'\t' read -r _ status conclusion _ url < <(awk -F'\t' '$1 == "ci-ok"' <<<"$verdict") || status=''
      if [ "$status" = completed ]; then
        conclude "the ci-ok of run $followed" "$conclusion" "$url" "the approval-time run at this head that this approval follows"
        return
      fi
    fi
    sleep "$HOLD_POLL"
  done
}

# hold: the three cases of the header. It reads this run's jobs until CALLER has listed a job of
# the workflow it calls, and hands that read to hold_caller, or until CALLER is skipped (`lead`
# chose to follow), and hands on to hold_follow. Each of the two waits for its one verdict.
hold() {
  need GH_TOKEN GITHUB_REPOSITORY GITHUB_RUN_ID GITHUB_RUN_ATTEMPT CALLER HEAD_SHA
  local started='' jobs
  echo "waiting for this approval's verdict at $HEAD_SHA (attempt $GITHUB_RUN_ATTEMPT)"
  while true; do
    if jobs=$(read_jobs "$GITHUB_RUN_ID"); then
      if [ "$(caller_rows "$jobs" 1)" -gt 0 ] || [ -n "$(awk -F'\t' -v want="$CALLER / ci-ok" '$1 == want' <<<"$jobs")" ]; then
        hold_caller "$jobs"
        return
      fi
      if [ -n "$(awk -F'\t' -v caller="$CALLER" '$1 == caller && $3 == "skipped"' <<<"$jobs")" ]; then
        hold_follow
        return
      fi
      # Nothing of CALLER yet: `lead` has not ended, or the caller has not started.
      if [ -n "$(awk -F'\t' '$1 == "lead" && $2 != "completed"' <<<"$jobs")" ]; then
        started=''
      else
        [ -n "$started" ] || started=$SECONDS
        if [ $((SECONDS - started)) -ge "$HOLD_START_LIMIT" ]; then
          echo "::error::no job of $CALLER is listed $HOLD_START_LIMIT s after lead ended, neither its jobs nor its skip, so this ci-ok fails: the caller was cancelled, or it waits behind another approval-time run of this merge commit in ci.yaml's concurrency group, which keeps it waiting rather than cancelling it and starts it, running every suite, once that run ends. $(remedy)"
          return 1
        fi
      fi
    fi
    sleep "$HOLD_POLL"
  done
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
