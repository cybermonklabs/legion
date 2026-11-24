#!/usr/bin/env bash
# The ci.yaml merge-queue run of a pull request the queue took out, force-cancelled: this reads
# the pull request's timeline for the removal, takes the head of the group the queue had built
# from it, finds that group's ci.yaml merge-group run and force-cancels it. That runner time is
# what the act takes back, for the one case ci.yaml's own cancel act cannot see: that act cancels
# the older groups of the pull request a new group of it replaced, and a pull request that left
# the queue builds no new group.
#
# Where the commit comes from. The `dequeued` payload carries no beforeCommit, so this reads the
# pull request's timeline with GraphQL and takes the newest RemovedFromMergeQueueEvent after the
# newest AddedToMergeQueueEvent; that item's `beforeCommit.oid` is the head of the group the queue
# had built, and its `reason` is read from the same item. GitHub can show the event before the
# timeline holds it, so the read is made up to seven times, DEQUEUED_READ_SECONDS apart: a fixed
# list of attempts, which bounds the wait at about a minute and cannot spin. A read GitHub refuses
# counts as one of those attempts and leaves a warning of its own, and no line about what the
# timeline holds, since a refused read read nothing: the attempts are there for an answer that has
# not come yet, and a 502 or a rate limit is as good a reason to ask again as a timeline that has
# not caught up, where a step that died on the first refusal would cancel nothing and leave the
# run it was to end holding its runners. A run left alive by the bound, with the timeline read and
# holding no removal, is still stopped by the queue: when the pull request is added again, the new
# group's `cancel-started` cancels the older groups of the same pull request.
#
# Why a read this act could not make is no failure of the run it runs in. The act reclaims runner
# time and decides nothing, as ci.yaml's cancel-started act does not
# (.github/scripts/cancel-started-act.sh's header names the merge-group run that went red for one
# HTTP 500 of a run list). So each read here goes through gh-read.sh, and a refusal still standing
# at the end of its attempts warns and exits 0 where gh_read_transient reads it as GitHub's own: a
# 5xx, a 429, or a rate limit gh-read.sh could not wait out within the job's limit.
#
# Every other refusal of the timeline is still red, which is what the seven attempts are there to
# tell apart: a run that read no timeline at all saw nothing, and a green run there would say this
# workflow had looked and found nothing while the removed group kept its runners — a token-scope
# regression is exactly that shape, and a 403 or a 404 is exactly what it answers. A malformed
# answer fails where it is read, and a refused force-cancel warns: the run it names may have
# completed between the list and the call, and an act that reclaims nothing is no failure.
#
# What it deliberately does not do:
#   * a removal whose reason is `merged` cancels nothing: that group's run tested the tree that
#     landed, and the ci-ok it posted is what let the merge happen;
#   * it names one run, the ci.yaml merge-group run whose headSha is that beforeCommit, so a group
#     the queue builds after the removal — a different head — is never touched;
#   * it adds nothing back to the queue and comments nowhere: a pull request that was taken out
#     over a failure or a conflict is its author's to fix, and the groups that stayed are rebuilt
#     by the queue itself.
#
# The cancel is `force-cancel`, the call ci.yaml's cancel act makes: it leaves a running job's
# `if: always()` steps skipped, so the cancelled run posts no ci-ok, and a failing ci-ok on a
# commit the queue still holds could take an innocent pull request out of the queue.
#
# Who runs it: the second of the two steps of .github/workflows/dequeued.yaml's cancel-dequeued
# job, after its checkout; that file's header says why the workflow runs on pull_request_target
# and what its job holds. It takes no argument and reads two variables that step's own `env:` map
# sets: GH_TOKEN, the run's own token, for `gh`, and PR_NUMBER, the pull request the queue took
# out. The runner sets $GITHUB_REPOSITORY, which every call here names, for every step.
#
# Run from anywhere, with those two and $GITHUB_REPOSITORY set:
#   .github/scripts/cancel-dequeued-act.sh
# Its tests are in cancel-acts.test.sh, which ci.yaml's test job runs.
set -euo pipefail
# shellcheck source=.github/scripts/gh-read.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/gh-read.sh"

# Seconds between the timeline reads: the wait for an event GitHub has not shown yet. The tests
# set 0, as they set gh-read.sh's GH_RETRY_SECONDS.
DEQUEUED_READ_SECONDS=${DEQUEUED_READ_SECONDS:-10}

# The words gh printed for the attempt each read ended on: gh-read.sh hands them to this file
# rather than to the log, so a decision below can read the status out of them, and each read
# prints them to the log itself.
words=$(mktemp)
trap 'rm -f "$words"' EXIT

# shellcheck disable=SC2016 # $owner, $name and $number are GraphQL's variables, not the shell's
query='query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      timelineItems(last: 20, itemTypes: [ADDED_TO_MERGE_QUEUE_EVENT, REMOVED_FROM_MERGE_QUEUE_EVENT]) {
        nodes {
          __typename
          ... on RemovedFromMergeQueueEvent { createdAt reason beforeCommit { oid } }
        }
      }
    }
  }
}'
# The nodes come oldest first, so the selection is the last removal after the index of the last
# entry: a removal before that index belongs to an earlier turn in the queue.
# shellcheck disable=SC2016 # $nodes and $added are jq's variables, not the shell's
select='.data.repository.pullRequest.timelineItems.nodes as $nodes
  | ([$nodes | to_entries[]
      | select(.value.__typename == "AddedToMergeQueueEvent") | .key] | last) as $added
  | [$nodes[(($added // -1) + 1):][]
     | select(.__typename == "RemovedFromMergeQueueEvent")] | last // empty'
# `refused` counts the reads GitHub answered with an error, and `passing` those of them that pass
# of themselves. A refused read says nothing about the timeline, so it prints its own line and not
# the line about a timeline that holds no removal.
refused=0
passing=0
removal=
for attempt in 1 2 3 4 5 6 7; do
  read_status=0
  removal=$(gh_wait_reset --stderr "$words" gh api graphql -F owner="${GITHUB_REPOSITORY%%/*}" \
    -F name="${GITHUB_REPOSITORY#*/}" -F number="$PR_NUMBER" -f query="$query" --jq "$select") ||
    read_status=$?
  cat "$words" >&2
  if [ "$read_status" -eq 0 ]; then
    [ -z "$removal" ] || break
    echo "read $attempt of 7 of pull request #$PR_NUMBER's timeline holds no removal after its newest queue entry"
  else
    removal=
    refused=$((refused + 1))
    if gh_read_transient "$read_status" "$words"; then
      passing=$((passing + 1))
    fi
    echo "::warning::GitHub refused read $attempt of 7 of pull request #$PR_NUMBER's timeline ($(head -n 1 "$words")), so that read says nothing about a removal"
  fi
  [ "$attempt" = 7 ] || sleep "$DEQUEUED_READ_SECONDS"
done
if [ "$refused" = 7 ]; then
  if [ "$passing" = 7 ]; then
    echo "::warning::GitHub refused all 7 reads of pull request #$PR_NUMBER's timeline, each of them for a 5xx, a 429 or a rate limit ($(head -n 1 "$words")), so this run names no group and cancels nothing; the removed group's ci.yaml run keeps its runners until the queue's next group of this pull request ends it. The act reclaims runner time and decides nothing, so a timeline this run could not read is no failure of it."
    exit 0
  fi
  echo "::error::GitHub refused all 7 reads of pull request #$PR_NUMBER's timeline, and not every refusal was a 5xx, a 429 or a rate limit ($(head -n 1 "$words")): this run read no timeline at all, it cannot name the group the queue removed, and a token this job's scopes no longer cover answers exactly so" >&2
  exit 1
fi
if [ -z "$removal" ]; then
  echo "::notice::no removal of pull request #$PR_NUMBER reached its timeline in the minute this run waited, so no run is named and none is cancelled; the group the queue builds next ends this one"
  exit 0
fi
reason=$(jq -r '.reason // "no reason"' <<<"$removal")
before=$(jq -r '.beforeCommit.oid // ""' <<<"$removal")
echo "pull request #$PR_NUMBER left the queue at $(jq -r '.createdAt' <<<"$removal") for reason '$reason', out of the group whose head is ${before:-no commit}"
if [ "$reason" = merged ]; then
  echo "that removal is the merge itself, so the group's run tested the tree that landed: nothing is cancelled"
  exit 0
fi
if [ -z "$before" ]; then
  echo "::warning::the removal names no beforeCommit, so there is no group head to look for and nothing is cancelled"
  exit 0
fi
listed=0
runs=$(gh_read --stderr "$words" gh run list --repo "$GITHUB_REPOSITORY" --workflow ci.yaml \
  --event merge_group --commit "$before" --limit 20 --json databaseId,headSha,status,conclusion) ||
  listed=$?
cat "$words" >&2
if [ "$listed" -ne 0 ]; then
  route="GET repos/$GITHUB_REPOSITORY/actions/workflows/ci.yaml/runs, the route gh run list --workflow ci.yaml --event merge_group --commit $before calls"
  if gh_read_transient "$listed" "$words"; then
    echo "::warning::GitHub refused $route, at every attempt gh-read.sh made ($(head -n 1 "$words")), so the run of the group whose head is $before is not named and nothing is cancelled; it keeps its runners until the queue's next group of this pull request ends it. The act reclaims runner time and decides nothing, so a list this run could not read is no failure of it."
    exit 0
  fi
  echo "::error::GitHub refused $route, at every attempt gh-read.sh made, and for no rate limit and no 5xx ($(head -n 1 "$words")): this job's token or route is wrong, which no later run mends" >&2
  exit 1
fi
doomed=$(jq -r --arg sha "$before" \
  '.[] | select(.headSha == $sha and .status != "completed") | .databaseId' <<<"$runs")
if [ -z "$doomed" ]; then
  echo "no ci.yaml merge-group run with head $before is still going, so nothing is cancelled. What that commit's runs are: $(jq -c --arg sha "$before" '[.[] | select(.headSha == $sha) | {databaseId, status, conclusion}]' <<<"$runs")"
  exit 0
fi
while read -r id; do
  echo "run $id is the ci.yaml merge-group run of the group whose head is $before, and it has not completed: force-cancel it"
  gh_wait_reset gh api --silent --method POST "repos/$GITHUB_REPOSITORY/actions/runs/$id/force-cancel" ||
    echo "::warning::force-cancel of run $id failed; it may have completed already"
done <<<"$doomed"
