#!/usr/bin/env bash
# Holds a failing verdict of `.github/workflows/ci.yaml#ci-ok` job on a merge-group run until the group is the
# front of the queue: until the queue's branch is at the group's base, the merge group's parent
# commit. It is the step right after the verdict step and runs only when that step failed, on a
# merge group's first attempt; the reconcile step is skipped then, so this is the job's last step,
# the job's own check run, the ci-ok main's ruleset reads, stays in progress while it waits, and
# the check run concludes failure the moment it ends.
#
# Why it waits. The queue counts a group whose ci-ok failed as no longer building and builds the
# next entry at once, on top of the failed one, and it takes a failed entry out only once every
# group in front of it has ended (ci-ok-report.sh's header holds both measurements). So a failing
# ci-ok that a group behind the front concludes takes that group out no sooner and buys the queue
# groups it will throw away. Measured from 2026-11-22T10:34Z (#4983, which stopped the early
# posts of groups behind the front) to 2026-11-23T02:23Z: 8 groups failed, and the entry left the
# queue within 0.4 minutes of the later of its first failed job and the moment it became the
# front in 6 of them, and 1.9 and 5.6 minutes after it in the 2 whose group became the front
# before its run ended. Each of the 12 failing ci-oks concluded behind the front in that time was
# followed within 90 seconds by a group built on the group that concluded it (18 groups, where
# the whole queue built 0.45 groups in 90 seconds on average). 16 groups were built on a failed
# entry within 90 seconds of such a verdict with no merge between, and they spent 1,537
# runner-minutes up to two minutes after the entry left, when the rebuild threw them away: about
# 24 groups and 2,300 runner-minutes a day. #4820's run 45095618270 is one: its test job failed at
# 01:46:36Z, its ci-ok job concluded failure at 02:19:18Z, a group was built on it at 02:19:42Z,
# and the entry left at 02:23:12Z, the second the group in front of it merged. This step would
# have held the verdict those four minutes, and the entry would have left one read later, with
# nothing built on it. The verdict never moves: the job fails because the verdict step failed, and
# this step only decides when.
#
# What it reads, and how. Once, at its start, the creation time of its own run, through the REST
# API: a group's age is what the queue's check timeout counts, and the bound below is an age.
# Then one `git ls-remote` of this public repository every CI_OK_HOLD_POLL_SECONDS, which answers
# two refs at once: the queue's branch (BASE_REF), whose tip says whether the group is the front,
# and the group's own branch (HEAD_REF, the gh-readonly-queue/<base>/pr-<n>-<parent sha> ref the
# merge_group event names). The queue keeps that branch at the group's head while the group is in
# the queue and deletes it when the group leaves: read at 2026-11-23T05:30Z, each of the 11
# entries the queue was building had its branch at its group's head, and the branches of the
# groups taken out earlier that night (#4820's pr-4820-77eac01e…, #4626's pr-4626-cdf532a8…)
# were gone. So a branch that is gone or moved says the queue dropped the group; a branch that is
# there says nothing, since one outlived its group (pr-2865-52913999…), and the age bound decides
# that case. A git read spends none of the installation's REST API budget, which ran out twice on
# 2026-11-22, so a held group never waits on a spent budget between its reads; the one REST read
# runs with no deadline for gh-read.sh to wait within, so a spent budget fails it once its ladder
# ends, without a wait for the reset.
#
# When it ends, and the failing verdict goes up:
#   * the queue's branch is at the group's base: the group is the front, and the queue takes the
#     entry out on this verdict;
#   * the group's branch is gone or names another commit: the queue no longer holds this group
#     (a group in front of it failed or left, and the queue rebuilt the groups behind), so nothing
#     can be built on it and the verdict decides nothing. cancel-started of the rebuilt group's
#     run force-cancels this run in most such cases first (merge-group-cancels.sh), which skips
#     this step and every later one;
#   * GitHub refused the read at every attempt retry_fetch made, or answered no tip for the
#     queue's branch: the step cannot tell where the group stands, and every refusal of the ci-ok
#     steps prefers the failing verdict (ci-ok-report.sh's header);
#   * the group is CI_OK_HOLD_AGE_SECONDS old, counted from its run's creation, and still held
#     behind the front. Main's merge_queue rule assumes a check failed when it has not concluded
#     180 minutes into the group (check_response_timeout_minutes); the age, 170 minutes, leaves
#     the last pause, the last read's ladder (five attempts, 300 s) and the job's conclusion
#     inside that. The run's creation time is read rather than the queue commit's time, which is
#     no clock of the group: on three runs created at 2026-11-23T06:00:26Z to 06:00:28Z the head
#     commit's time was 74.6 to 83.7 minutes earlier. When GitHub refuses that read, the hold
#     counts from its own start instead, with CI_OK_HOLD_RUN_SECONDS taken off the age: the
#     longest run measured.
#
# What it costs: the ci-ok job's ubuntu-24.04-arm runner while it waits. Over those 8 failures the
# runners would have waited 122 minutes in all: 3.9 to 19.4 minutes for each of the six failed
# groups whose runs ended behind the front, and 66.5 for the failing groups built on them until
# their rebuild, which their branches' deletion now ends within one read. The entry leaves up to
# one pause between reads later than a verdict already standing would let it.
#
# Its inputs, from the step's env:
#   GH_TOKEN                 the job's github.token, with `actions: read` (the run's creation).
#   BASE_SHA, BASE_REF       the merge group's parent commit and the queue's branch
#                            (`github.event.merge_group.base_sha` and `.base_ref`, a full ref).
#   HEAD_SHA, HEAD_REF       the group's head commit and its branch (`github.event.merge_group.
#                            head_sha` and `.head_ref`, a full ref).
#   CI_OK_HOLD_AGE_SECONDS   the group's age, from its run's creation, past which it waits no more.
#   CI_OK_HOLD_RUN_SECONDS   the longest a merge-group run takes before its ci-ok job, taken off
#                            that age when the run's creation cannot be read.
#   CI_OK_HOLD_POLL_SECONDS  optional: the pause between two reads, 30 by default (the tests'
#                            knob): the verdict reaches the queue at most that long after the
#                            group becomes the front, plus one read.
#   GITHUB_SERVER_URL, GITHUB_REPOSITORY, GITHUB_RUN_ID, as Actions sets them.
# Every read is retry_fetch's ladder (ci-retry.sh) run through gh-read.sh's gh_wait_reset: five
# attempts, each bounded at 30 s, with sleeps of 10, 20, 40 and 80 s between them, 5 x 30 + 150 =
# 300 s at most; the tests make `sleep` a no-op on PATH, as ci-ok-report.test.sh does for the
# report.
#
# Run from anywhere: GH_TOKEN=... BASE_SHA=... BASE_REF=... HEAD_SHA=... HEAD_REF=... \
#   CI_OK_HOLD_AGE_SECONDS=... CI_OK_HOLD_RUN_SECONDS=... .github/scripts/ci-ok-hold.sh
# Its tests are in ci-ok-report.test.sh, which ci.yaml's test job runs.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=.github/scripts/ci-retry.sh
source "$script_dir/ci-retry.sh"
# shellcheck source=.github/scripts/gh-read.sh
source "$script_dir/gh-read.sh"
for input in GH_TOKEN BASE_SHA BASE_REF HEAD_SHA HEAD_REF CI_OK_HOLD_AGE_SECONDS CI_OK_HOLD_RUN_SECONDS \
  GITHUB_SERVER_URL GITHUB_REPOSITORY GITHUB_RUN_ID; do
  if [ -z "${!input:-}" ]; then
    echo "::error::ci-ok-hold.sh needs $input in its environment; the failing ci-ok goes up now" >&2
    exit 1
  fi
done
poll=${CI_OK_HOLD_POLL_SECONDS:-30}
for input in CI_OK_HOLD_AGE_SECONDS CI_OK_HOLD_RUN_SECONDS poll; do
  if ! [[ ${!input} =~ ^[0-9]+$ ]]; then
    echo "::error::ci-ok-hold.sh's $input is not a whole number of seconds: ${!input}; the failing ci-ok goes up now" >&2
    exit 1
  fi
done

# The deadline, from the run's creation. The read runs with no limit for gh-read.sh to wait
# within, so a spent budget fails it once its ladder ends rather than holding the group until a
# reset.
start=$(date +%s)
if created=$(unset JOB_TIMEOUT_MINUTES STEP_TIMEOUT_MINUTES
  gh_wait_reset retry_fetch "this run's creation time" 30 -- \
    gh api "repos/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID" --jq .created_at) \
  && created_at=$(date -u -d "$created" +%s 2>/dev/null); then
  deadline=$((created_at + CI_OK_HOLD_AGE_SECONDS))
  echo "This run was created at $created, so the hold ends at the latest at $(date -u -d "@$deadline" +%FT%TZ), when the group is $CI_OK_HOLD_AGE_SECONDS s old"
else
  deadline=$((start + CI_OK_HOLD_AGE_SECONDS - CI_OK_HOLD_RUN_SECONDS))
  echo "::warning::GitHub did not give this run's creation time (${created:-refused}), so the hold counts from its own start: it ends at the latest at $(date -u -d "@$deadline" +%FT%TZ), $CI_OK_HOLD_AGE_SECONDS s less the $CI_OK_HOLD_RUN_SECONDS s the longest run takes" >&2
fi

export GIT_TERMINAL_PROMPT=0
remote=$GITHUB_SERVER_URL/$GITHUB_REPOSITORY.git
seen=
while :; do
  if ! refs=$(gh_wait_reset retry_fetch "the refs $BASE_REF and $HEAD_REF of $remote" 30 -- \
    git ls-remote "$remote" "$BASE_REF" "$HEAD_REF"); then
    echo "::warning::GitHub refused the refs $BASE_REF and $HEAD_REF of $remote at every attempt retry_fetch made, so this step cannot tell whether a group stands in front of this one; the failing ci-ok goes up now, as every refusal of the ci-ok steps prefers the failing verdict" >&2
    exit 0
  fi
  tip=$(awk -v ref="$BASE_REF" '$2 == ref { print $1 }' <<<"$refs")
  group=$(awk -v ref="$HEAD_REF" '$2 == ref { print $1 }' <<<"$refs")
  now=$(date +%s)
  waited=$((now - start))
  if [ -z "$tip" ]; then
    echo "::warning::$remote answered no tip for $BASE_REF, so this step cannot tell whether a group stands in front of this one; the failing ci-ok goes up now, as every refusal of the ci-ok steps prefers the failing verdict" >&2
    exit 0
  fi
  if [ "$tip" = "$BASE_SHA" ]; then
    echo "This group is the front of the queue after $waited s: $BASE_REF is at its base $BASE_SHA, so the failing ci-ok goes up now"
    exit 0
  fi
  if [ "$group" != "$HEAD_SHA" ]; then
    echo "The queue no longer holds this group after $waited s: $HEAD_REF is ${group:-gone}, not its head $HEAD_SHA, so nothing can be built on it and the failing ci-ok goes up now"
    exit 0
  fi
  if [ "$tip" != "$seen" ]; then
    echo "This group stands behind the front of the queue after $waited s: its base $BASE_SHA is not $tip, the tip of $BASE_REF. The failing ci-ok waits, so the queue builds nothing on this group"
    seen=$tip
  fi
  if [ "$now" -ge "$deadline" ]; then
    echo "::warning::This group still stands behind the front of the queue after $waited s, at its deadline $(date -u -d "@$deadline" +%FT%TZ); the failing ci-ok goes up now, before the queue's own check timeout, and the queue may build another group on this one" >&2
    exit 0
  fi
  sleep "$poll"
done
