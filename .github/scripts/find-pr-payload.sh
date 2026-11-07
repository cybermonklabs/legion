#!/usr/bin/env bash
# The pull request payload a check of a pull request's text reads, as a file: its path on stdout.
# ci.yaml's `pr-title` job hands that path to check-pr-text.sh as GITHUB_EVENT_PATH, because one
# of the events that job runs on, a merge group, carries no pull request. pr-title.yaml runs on
# an edit and a review alone, whose payloads carry it, so that workflow reads the run's own
# GITHUB_EVENT_PATH and calls nothing here.
#
# A payload that is not a merge group's carries the pull request already, so for any other event
# the script prints GITHUB_EVENT_PATH and reads nothing. A merge_group payload carries no pull
# request, only the queue branch the group runs on,
# refs/heads/gh-readonly-queue/<base>/pr-<number>-<head sha>, so the script reads the pull request
# that branch names and writes it under RUNNER_TEMP in the shape check-pr-text.sh reads,
# {pull_request: <the pull request>, number: <its number>}; a branch that names none is refused
# rather than guessed at, since a group checked against the wrong pull request would report a
# `pr-title` about somebody else's text.
#
# Its inputs, from the step's env:
#   GITHUB_EVENT_NAME  the event, as Actions sets it.
#   GITHUB_EVENT_PATH  the payload Actions wrote for that event.
#   GH_TOKEN           the job's github.token. merge_group only: it reads the pull request.
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
pull="$temp/pull-request.json"
payload="$temp/pull-request-event.json"
# The read retries: .github/scripts/ci-retry.sh makes five bounded attempts, the last starting
# 150 s after the first, so a refusal costs this lane a wait rather than the required pr-title
# check (dispatch://INFRA-1545, R1). 30 s per attempt, the bound this repository's CI gives one
# page of a REST route: ten times the about 3 s such a read takes. The write is inside `sh -c`,
# so each attempt starts the file over and the bound still reaches gh itself
# (.github/scripts/ci-ok-report.sh says why where it does the same).
script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=.github/scripts/ci-retry.sh
. "$script_dir/ci-retry.sh"
# shellcheck disable=SC2016 # $1 and $2 are sh -c's own arguments, expanded by that shell
retry_fetch "the queued pull request" 30 -- sh -c 'gh api "$1" > "$2"' gh-api \
  "repos/$repository/pulls/$number" "$pull"
jq --argjson number "$number" '{pull_request: ., number: $number}' "$pull" > "$payload"
echo "$payload"
