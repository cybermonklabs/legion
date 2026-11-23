#!/usr/bin/env bash
# Holds a failing verdict of ci.yaml's `ci-ok` job on a merge-group run until the group is the
# front of the queue: until the queue's branch is at the group's base, the merge group's parent
# commit. It runs as a step of that job after the verdict step has failed and before the job
# ends, so the job's own check run, the ci-ok main's ruleset reads, stays in progress while the
# step waits, and the failing verdict reaches the queue the moment the group is the front.
#
# Why it waits. The queue counts a group whose ci-ok failed as no longer building and builds the
# next entry at once, on top of the failed one, and it takes a failed entry out only once every
# group in front of it has ended (ci-ok-report.sh's header holds both measurements). So a failing
# ci-ok that a group behind the front concludes changes nothing for that group and buys the
# queue groups it will throw away. Measured from 2026-11-22T10:34Z (#4983, which stopped the early
# posts of groups behind the front) to 2026-11-23T02:23Z: 8 groups failed, and the entry left the
# queue within 0.4 minutes of the later of its first failed job and the moment it became the
# front in 6 of them, and 1.9 and 5.6 minutes after it in the 2 whose group became the front
# before its run ended. Each of the 12 failing ci-oks concluded behind the front in that time was
# followed within 90 seconds by a group built on the group that concluded it (18 groups, where
# the whole queue built 0.45 groups in 90 seconds on average). 16 groups were built on a failed
# entry within 90 seconds of such a verdict with no merge between, and they spent 1,536
# runner-minutes up to two minutes after the entry left, when the rebuild threw them away: about
# 24 groups and 2,300 runner-minutes a day. #4820's run 45095618270 is one: its test job failed at
# 01:46:36Z, its ci-ok job concluded failure at 02:19:18Z, a group was built on it at 02:19:42Z,
# and the entry left at 02:23:12Z, the second the group in front of it merged. This step would
# have held the verdict those four minutes, and the entry would have left one read of the tip
# later, with nothing built on it.
#
# What it costs: the ci-ok job's ubuntu-24.04-arm runner while it waits, and one read of the
# branch's tip every CI_OK_HOLD_POLL_SECONDS. Over those 8 failures the runners would have waited
# 122 minutes in all: 3.9 to 19.4 minutes for each of the six failed groups whose runs ended
# behind the front, and 66.5 for the failing groups built on them until their rebuild: about 250
# reads of the tip. At most ten groups build at once (main's merge_queue rule,
# max_entries_to_build), so ten held at once read 1,200 times an hour, against the 15,000 the
# installation has. The entry leaves up to one pause between reads later than a verdict already
# standing would let it.
# It never moves the verdict: the job fails because the verdict step failed, and this step only
# decides when.
#
# When it stops without the group reaching the front, so the verdict goes up at once:
#   * GitHub refused the tip at every attempt gh-read.sh made: the step cannot tell where the
#     group stands, and every refusal of the ci-ok steps prefers the failing verdict
#     (ci-ok-report.sh's header);
#   * CI_OK_HOLD_SECONDS have passed. A group that can never become the front is one built on
#     another group that failed or left the queue; the queue rebuilds the groups behind such an
#     entry, and cancel-started of the rebuilt group's run force-cancels this run, this step with
#     it (merge-group-cancels.sh). The bound is for the run nothing cancels: its verdict then goes
#     up on a group the queue no longer holds, which builds nothing.
# A force-cancel skips every later step, so a run the queue threw away concludes nothing.
#
# Its inputs, from the step's env:
#   GH_TOKEN                 the job's github.token, with `contents: read` (the tip of the
#                            queue's branch).
#   BASE_SHA, BASE_REF       the merge group's parent commit and the queue's branch
#                            (`github.event.merge_group.base_sha` and `.base_ref`).
#   CI_OK_HOLD_SECONDS       how long it waits at most, from its start.
#   CI_OK_HOLD_POLL_SECONDS  optional: the pause between two reads of the tip, 30 by default (the
#                            tests' knob): the verdict reaches the queue at most that long after
#                            the group becomes the front.
#   GITHUB_REPOSITORY, as Actions sets it.
# Every read goes through gh-read.sh's retry ladder, whose GH_RETRY_SECONDS is the tests' knob. A
# spent API budget is waited out there within the step's limit (STEP_TIMEOUT_MINUTES), as for
# every caller of that file; a reset past that limit is a refusal, and the verdict goes up.
#
# Run from anywhere: GH_TOKEN=... BASE_SHA=... BASE_REF=... CI_OK_HOLD_SECONDS=... \
#   .github/scripts/ci-ok-hold.sh
# Its tests are in ci-ok-report.test.sh, which ci.yaml's test job runs.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=.github/scripts/gh-read.sh
source "$script_dir/gh-read.sh"
for input in GH_TOKEN BASE_SHA BASE_REF CI_OK_HOLD_SECONDS GITHUB_REPOSITORY; do
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

start=$SECONDS
seen=
while :; do
  if ! tip=$(gh_read gh api "repos/$GITHUB_REPOSITORY/git/ref/${BASE_REF#refs/}" --jq .object.sha); then
    echo "::warning::GitHub refused the tip of $BASE_REF at every attempt gh-read.sh made, so this step cannot tell whether a group stands in front of this one; the failing ci-ok goes up now, as every refusal of the ci-ok steps prefers the failing verdict"
    exit 0
  fi
  waited=$((SECONDS - start))
  if [ "$tip" = "$BASE_SHA" ]; then
    echo "This group is the front of the queue after $waited s: $BASE_REF is at its base $BASE_SHA, so the failing ci-ok goes up now"
    exit 0
  fi
  if [ "$tip" != "$seen" ]; then
    echo "This group stands behind the front of the queue after $waited s: its base $BASE_SHA is not $tip, the tip of $BASE_REF. The failing ci-ok waits, so the queue builds nothing on this group"
    seen=$tip
  fi
  if [ $((waited + poll)) -ge "$CI_OK_HOLD_SECONDS" ]; then
    echo "::warning::This group still stands behind the front of the queue after $waited s, and the next read would pass this step's bound of $CI_OK_HOLD_SECONDS s; the failing ci-ok goes up now, and the queue may build another group on this one"
    exit 0
  fi
  sleep "$poll"
done
