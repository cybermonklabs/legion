#!/usr/bin/env bash
# Holds a failing verdict of ci.yaml's `ci-ok` job on a merge-group run until the group is the
# front of the queue: until the queue's branch is at the group's base, the merge group's parent
# commit. It is the last step of that job and runs only after the verdict step has failed, so the
# job's own check run, the ci-ok main's ruleset reads, stays in progress while the step waits and
# concludes failure the moment the step ends.
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
# What it reads, and how. One `git ls-remote` of this public repository every
# CI_OK_HOLD_POLL_SECONDS answers two refs at once: the queue's branch (BASE_REF), whose tip says
# whether the group is the front, and the group's own branch (HEAD_REF, the
# gh-readonly-queue/<base>/pr-<n>-<parent sha> ref the merge_group event names), which says
# whether the queue still holds the group. The queue keeps that branch at the group's head while
# the group is in the queue and deletes it when the group leaves: read at 2026-11-23T05:30Z, each
# of the 11 entries the queue was building had its branch at its group's head, and the branches
# of the groups taken out earlier that night (#4820's pr-4820-77eac01e…, #4626's
# pr-4626-cdf532a8…) were gone; one branch outlived its group (pr-2865-52913999…), which leaves
# that case to the bound below. A git read spends none of the installation's REST API budget,
# which ran out twice on 2026-11-22, so a held group can never wait on a spent budget, and the
# step needs no token: GitHub serves a public repository's refs to anyone.
#
# When it ends, and the failing verdict goes up:
#   * the queue's branch is at the group's base: the group is the front, and the queue takes the
#     entry out on this verdict;
#   * the group's branch is gone or names another commit: the queue no longer holds this group
#     (a group in front of it failed or left, and the queue rebuilt the groups behind), so nothing
#     can be built on it and the verdict decides nothing. cancel-started of the rebuilt group's
#     run force-cancels this run in most such cases first (merge-group-cancels.sh), which skips
#     this step and every later one;
#   * GitHub refused the read at every attempt gh-read.sh made, or answered no tip for the
#     queue's branch: the step cannot tell where the group stands, and every refusal of the ci-ok
#     steps prefers the failing verdict (ci-ok-report.sh's header);
#   * CI_OK_HOLD_SECONDS have passed with the group still held behind the front. Main's
#     merge_queue rule assumes a check failed when it has not concluded 180 minutes after the
#     group's checks began (check_response_timeout_minutes), and this job starts only once every
#     other job of its run has ended, which took up to 55.5 minutes on 2026-11-22; the bound,
#     two hours, ends the hold near the point where the queue would assume the failure itself.
#
# What it costs: the ci-ok job's ubuntu-24.04-arm runner while it waits. Over those 8 failures the
# runners would have waited 122 minutes in all: 3.9 to 19.4 minutes for each of the six failed
# groups whose runs ended behind the front, and 66.5 for the failing groups built on them until
# their rebuild, which their branches' deletion now ends within one read. The entry leaves up to
# one pause between reads later than a verdict already standing would let it.
#
# Its inputs, from the step's env:
#   BASE_SHA, BASE_REF       the merge group's parent commit and the queue's branch
#                            (`github.event.merge_group.base_sha` and `.base_ref`, a full ref).
#   HEAD_SHA, HEAD_REF       the group's head commit and its branch (`github.event.merge_group.
#                            head_sha` and `.head_ref`, a full ref).
#   CI_OK_HOLD_SECONDS       how long it waits at most, from its start.
#   CI_OK_HOLD_POLL_SECONDS  optional: the pause between two reads, 30 by default (the tests'
#                            knob): the verdict reaches the queue at most that long after the
#                            group becomes the front, plus one read.
#   GITHUB_SERVER_URL, GITHUB_REPOSITORY, as Actions sets them: the repository read.
# Every read goes through gh-read.sh's retry ladder, whose GH_RETRY_SECONDS is the tests' knob,
# with each attempt bounded by `timeout 30`.
#
# Run from anywhere: BASE_SHA=... BASE_REF=... HEAD_SHA=... HEAD_REF=... CI_OK_HOLD_SECONDS=... \
#   .github/scripts/ci-ok-hold.sh
# Its tests are in ci-ok-report.test.sh, which ci.yaml's test job runs.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=.github/scripts/gh-read.sh
source "$script_dir/gh-read.sh"
for input in BASE_SHA BASE_REF HEAD_SHA HEAD_REF CI_OK_HOLD_SECONDS GITHUB_SERVER_URL GITHUB_REPOSITORY; do
  if [ -z "${!input:-}" ]; then
    echo "::error::ci-ok-hold.sh needs $input in its environment; the failing ci-ok goes up now"
    exit 1
  fi
done
poll=${CI_OK_HOLD_POLL_SECONDS:-30}
for input in CI_OK_HOLD_SECONDS poll; do
  if ! [[ ${!input} =~ ^[0-9]+$ ]]; then
    echo "::error::ci-ok-hold.sh's $input is not a whole number of seconds: ${!input}; the failing ci-ok goes up now"
    exit 1
  fi
done

export GIT_TERMINAL_PROMPT=0
remote=$GITHUB_SERVER_URL/$GITHUB_REPOSITORY.git
start=$SECONDS
seen=
while :; do
  if ! refs=$(gh_read timeout 30 git ls-remote "$remote" "$BASE_REF" "$HEAD_REF"); then
    echo "::warning::GitHub refused the refs $BASE_REF and $HEAD_REF of $remote at every attempt gh-read.sh made, so this step cannot tell whether a group stands in front of this one; the failing ci-ok goes up now, as every refusal of the ci-ok steps prefers the failing verdict"
    exit 0
  fi
  tip=$(awk -v ref="$BASE_REF" '$2 == ref { print $1 }' <<<"$refs")
  group=$(awk -v ref="$HEAD_REF" '$2 == ref { print $1 }' <<<"$refs")
  waited=$((SECONDS - start))
  if [ -z "$tip" ]; then
    echo "::warning::$remote answered no tip for $BASE_REF, so this step cannot tell whether a group stands in front of this one; the failing ci-ok goes up now, as every refusal of the ci-ok steps prefers the failing verdict"
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
  if [ "$waited" -ge "$CI_OK_HOLD_SECONDS" ]; then
    echo "::warning::This group still stands behind the front of the queue after $waited s, past this step's bound of $CI_OK_HOLD_SECONDS s; the failing ci-ok goes up now, and the queue may build another group on this one"
    exit 0
  fi
  sleep "$poll"
done
