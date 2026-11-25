#!/usr/bin/env bash
# The pull request payload a check of a pull request's text reads, as a file: its path on stdout.
# `.github/workflows/ci.yaml#pr-title` job hands that path to check-pr-text.sh as GITHUB_EVENT_PATH, because one
# of the events that job runs on, a merge group, carries no pull request. pr-title.yaml runs on
# an edit alone, whose payload carries it, so that workflow reads the run's own
# GITHUB_EVENT_PATH and calls nothing here.
#
# A payload that is not a merge group's carries the pull request already, so for any other event
# the script prints GITHUB_EVENT_PATH and reads nothing. A merge_group payload carries no pull
# request, only the queue branch the group runs on,
# refs/heads/gh-readonly-queue/<base>/pr-<number>-<head sha>, so the script reads the pull request
# that branch names and writes it under RUNNER_TEMP in the shape check-pr-text.sh reads,
# {pull_request: {number, base: {sha}, head: {sha}}, number: <its number>}, the three things that
# script reads of a payload; a branch that names none is refused rather than guessed at, since a
# group checked against the wrong pull request would report a `pr-title` about somebody else's
# text. It reads them through GitHub's GraphQL API, whose budget is its own, as check-pr-text.sh
# reads the rest (that script's header says why); an answer that names no base and head sha is
# refused.
#
# Its inputs, from the step's env:
#   GITHUB_EVENT_NAME  the event, as Actions sets it.
#   GITHUB_EVENT_PATH  the payload Actions wrote for that event.
#   GH_TOKEN           the job's github.token. merge_group only: it reads the pull request,
#                      retry_fetch's ladder run through gh-read.sh's gh_wait_reset, which waits
#                      out a spent API budget (GraphQL's here) only when the wait and one more
#                      ladder fit within the step's JOB_TIMEOUT_MINUTES and STEP_TIMEOUT_MINUTES,
#                      and fails by name otherwise; the request is a file, so every attempt, and
#                      the ladder run again after a wait, sends it whole.
#   GITHUB_REPOSITORY  owner/repo, as Actions sets it. merge_group only.
#   RUNNER_TEMP        where the payload it writes lands. merge_group only.
#
# Every message goes to stderr, so stdout holds the path and nothing else; a refusal is an
# ::error:: line there, which the runner annotates from either stream.
#
# Run from anywhere: .github/scripts/find-pr-payload.sh
# Its tests are in find-pr-payload.test.sh, which ci.yaml's test job runs.
set -euo pipefail
shopt -s inherit_errexit
# shellcheck source=.github/scripts/gh-read.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/gh-read.sh"

event=${GITHUB_EVENT_PATH:?GITHUB_EVENT_PATH must name the event payload}
event_name=${GITHUB_EVENT_NAME:?GITHUB_EVENT_NAME must name the event}
if [ "$event_name" != merge_group ]; then
  echo "$event"
  exit 0
fi

head_ref=$(jq -r '.merge_group.head_ref // ""' "$event")
if ! [[ $head_ref =~ ^(refs/heads/)?gh-readonly-queue/.+/pr-([0-9]+)-[0-9a-f]{40}$ ]]; then
  echo "::error::merge_group.head_ref '$head_ref' names no queued pull request (want refs/heads/gh-readonly-queue/<base>/pr-<number>-<40 hex>)" >&2
  exit 1
fi
number=${BASH_REMATCH[2]}
repository=${GITHUB_REPOSITORY:?GITHUB_REPOSITORY must name the repository as owner/repo}
temp=${RUNNER_TEMP:?RUNNER_TEMP must name the temporary directory of the runner}
payload="$temp/pull-request-event.json"
answer="$temp/pull-request.json"
request="$temp/pull-request-request.json"
# shellcheck disable=SC2016 # $owner, $repo and $number are GraphQL's and jq's variables
jq -n --arg owner "${repository%%/*}" --arg repo "${repository#*/}" --argjson number "$number" '{
  operationName: "QueuedPull",
  query: "query QueuedPull($owner: String!, $repo: String!, $number: Int!) { repository(owner: $owner, name: $repo) { pullRequest(number: $number) { number baseRefOid headRefOid } } }",
  variables: {owner: $owner, repo: $repo, number: $number}}' > "$request"
# The read retries: .github/scripts/ci-retry.sh makes five bounded attempts, the last starting
# 150 s after the first, so a refusal costs this lane a wait rather than the required pr-title
# check (dispatch://INFRA-1545, R1). 30 s per attempt, the bound .github/ci-budgets.md's Retries
# note gives a `gh api` read: ten times the about 3 s one takes. The ladder asks again after an
# attempt that exits non-zero, and gh fails on a GraphQL answer that carries an error, HTTP 200
# or not; an answer gh exits 0 on is the jq check's below. The redirection is this shell's own and
# is applied once: the helper buffers each attempt's output and prints the successful attempt's
# alone, so no refused attempt's body reaches the file.
script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=.github/scripts/ci-retry.sh
. "$script_dir/ci-retry.sh"
gh_wait_reset retry_fetch "the queued pull request" 30 -- \
  gh api graphql --input "$request" > "$answer"
if ! jq -e --argjson number "$number" '.data.repository.pullRequest
    | {pull_request: {number, base: {sha: .baseRefOid}, head: {sha: .headRefOid}}, number: $number}
    | select((.pull_request.base.sha | type) == "string" and (.pull_request.head.sha | type) == "string")' \
  "$answer" > "$payload"; then
  echo "::error::GitHub's GraphQL API answered no base and head sha for #$number, the pull request $head_ref names" >&2
  exit 1
fi
echo "$payload"
