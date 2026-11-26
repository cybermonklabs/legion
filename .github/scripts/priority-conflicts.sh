#!/usr/bin/env bash
# Fails a pull request, or a merge group, that would land a conflict with a pull request labelled
# `priority`: a priority pull request merges first, and no other change makes it resolve a
# conflict it did not make. `.github/workflows/ci.yaml#priority-conflicts` job runs this script on
# every pull request and every merge group, and ci-ok needs that job, so its failure keeps the
# change out of main
# (.github/ci-notes/ci.md#why-a-change-that-conflicts-with-a-priority-pull-request-fails says why
# it is a check of its own and what it costs).
#
# Which change a run is:
#   * pull_request: the pull request the payload names. The run's commit, GITHUB_SHA, is GitHub's
#     test merge of the pull request into its base branch, whose first parent is the base it was
#     made on: the change is the difference between the two. A commit that is no merge is refused.
#   * merge_group: the commits from merge_group.base_sha, the group's parent, to GITHUB_SHA, the
#     group's head. Main's queue squashes each pull request into one commit whose subject ends
#     with ` (#<number>)`, and builds each group on the group in front of it, so a group holds one
#     commit today; every commit of the range is read all the same. A commit whose subject names
#     no pull request is refused, and so is a range that does not hold the pull request the
#     group's branch (merge_group.head_ref) names.
#   * any other event, a dispatched run: nothing lands from it, so it passes and reads nothing.
#
# What it is checked against: every open pull request labelled `priority` that is approved
# (reviewDecision APPROVED) or in the merge queue, read in one call of GitHub's GraphQL API, whose
# budget is apart from the REST one the rest of CI spends. A change whose every pull request carries
# the label passes: priority pull requests are not held to each other. One of the change's own pull
# requests is passed over, and so is a priority pull request whose squash commit is already in the
# change's base: it has landed, or stands in a group in front of this one, and goes first whatever
# this change holds.
#
# What counts as the change's conflict with one of them:
#   1. The files the change edits (its base to its commit) and the files the priority pull request
#      edits (its merge base with the change's base to its head), read with no rename detection,
#      so a rename counts as the delete of its old path. No file in both: no conflict, and nothing
#      is merged.
#   2. Otherwise `git merge-tree --write-tree` of the change's commit with the priority head. A clean
#      merge: no conflict.
#   3. Otherwise the same merge of the change's base with the priority head. A file that
#      conflicts in the first merge and not in the second is the change's conflict. A priority pull
#      request that has fallen behind its base conflicts with every change built on that base in
#      the files it already conflicts on, and those are its own to resolve when it merges the base,
#      whatever lands next: counting them would fail every pull request of the repository while one
#      priority pull request is stale. Where the first merge conflicts, the second is clean and git
#      names no file (a conflict of a file's place, such as one a directory rename moved), the files
#      of step 1 are named.
#
# What it fetches: one fetch, of this public repository and with no credential, of the change's
# commit and the head of each priority pull request left after the label read, with every commit
# and tree of their history and no file contents (--filter=blob:none). A merge base needs the
# history, at whatever depth it lies below a head that has not merged main for weeks, so the fetch
# is not shallow; git merge-tree fetches the contents of the files it merges as it needs them.
# Main's whole history read that way was 16 MB and took 1.8 s from a development machine on
# 2026-11-26. A run with no priority pull request to check fetches nothing.
#
# A read it could not make fails the check rather than pass it, saying what it could not read: a
# label read GitHub refused at every attempt gh-read.sh's gh_read made, an answer that is not the
# shape asked for or holds more than its one page of 100 pull requests, a fetch refused at every
# attempt, a merge git could not run.
#
# Its inputs, from the step's env:
#   GITHUB_EVENT_NAME, GITHUB_EVENT_PATH  the event and the payload Actions wrote for it.
#   GITHUB_SHA         the run's commit: a pull request's test merge, or a merge group's head.
#   GH_TOKEN           the job's github.token, with `pull-requests: read`: the label read, through
#                      gh-read.sh's gh_read, which waits out a spent budget within the job's
#                      JOB_TIMEOUT_MINUTES and tries twice more after any other refusal.
#   GITHUB_REPOSITORY, GITHUB_SERVER_URL  as Actions sets them: the repository read and fetched.
#   RUNNER_TEMP        where the fetch's repository is made.
#
# Every ::error:: line goes to stderr, every other line to stdout.
#
# Run from anywhere: .github/scripts/priority-conflicts.sh
# Its tests are in priority-conflicts.test.sh, which ci.yaml's test job runs.
set -euo pipefail
shopt -s inherit_errexit
# sort and comm compare the NUL-separated path lists byte for byte.
export LC_ALL=C
# shellcheck source=.github/scripts/gh-read.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/gh-read.sh"

# error <text>: one ::error:: line, escaped as a workflow command's data must be, since a title or
# a path can hold a percent sign.
error() {
  local text=$1
  text=${text//'%'/%25}
  text=${text//$'\r'/%0D}
  text=${text//$'\n'/%0A}
  echo "::error::$text" >&2
}

fail() {
  error "$1"
  exit 1
}

event_name=${GITHUB_EVENT_NAME:?GITHUB_EVENT_NAME must name the event}
if [ "$event_name" != pull_request ] && [ "$event_name" != merge_group ]; then
  echo "A $event_name run lands nothing on a branch, so it cannot land ahead of a priority pull request: nothing to check."
  exit 0
fi
event=${GITHUB_EVENT_PATH:?GITHUB_EVENT_PATH must name the event payload}
head=${GITHUB_SHA:?GITHUB_SHA must name the commit of the run}
repository=${GITHUB_REPOSITORY:?GITHUB_REPOSITORY must name the repository as owner/repo}
server=${GITHUB_SERVER_URL:?GITHUB_SERVER_URL must name the server the repository is on}
temp=${RUNNER_TEMP:?RUNNER_TEMP must name the temporary directory of the runner}
[[ $head =~ ^[0-9a-f]{40}$ ]] || fail "GITHUB_SHA '$head' is no commit"

if [ "$event_name" = pull_request ]; then
  number=$(jq -r '.pull_request.number // ""' "$event")
  [[ $number =~ ^[0-9]+$ ]] || fail "the pull_request payload names no pull request number"
  target=$(jq -r '.pull_request.base.ref // ""' "$event")
  change="pull request #$number"
  remedy="merge ${target:-its base branch} into this pull request's branch and resolve the conflict there"
else
  head_ref=$(jq -r '.merge_group.head_ref // ""' "$event")
  if ! [[ $head_ref =~ ^(refs/heads/)?gh-readonly-queue/.+/pr-([0-9]+)-[0-9a-f]{40}$ ]]; then
    fail "merge_group.head_ref '$head_ref' names no queued pull request (want refs/heads/gh-readonly-queue/<base>/pr-<number>-<40 hex>)"
  fi
  number=${BASH_REMATCH[2]}
  base=$(jq -r '.merge_group.base_sha // ""' "$event")
  [[ $base =~ ^[0-9a-f]{40}$ ]] || fail "merge_group.base_sha '$base' is no commit"
  target=$(jq -r '.merge_group.base_ref // "" | ltrimstr("refs/heads/")' "$event")
  change="the merge group of #$number"
  remedy="merge ${target:-its base branch} into #$number's branch, resolve the conflict and queue #$number again"
fi
unread="so this check cannot tell whether $change conflicts with a priority pull request, and fails rather than pass unread"

work=$(mktemp -d "$temp/priority-conflicts.XXXXXX")
request="$work/request.json"
answer="$work/answer.json"
# shellcheck disable=SC2016 # $owner and $repo are GraphQL's and jq's variables
jq -n --arg owner "${repository%%/*}" --arg repo "${repository#*/}" '{
  operationName: "PriorityPulls",
  query: "query PriorityPulls($owner: String!, $repo: String!) { repository(owner: $owner, name: $repo) { pullRequests(states: OPEN, labels: [\"priority\"], first: 100) { pageInfo { hasNextPage } nodes { number title headRefOid reviewDecision isInMergeQueue } } } }",
  variables: {owner: $owner, repo: $repo}}' > "$request"
if ! gh_read gh api graphql --input "$request" > "$answer"; then
  fail "GitHub's GraphQL API refused the read of the open pull requests labelled priority at every attempt, $unread"
fi
shape='.data.repository.pullRequests
  | (.pageInfo.hasNextPage == false) and (.nodes | type == "array")
    and all(.nodes[]; (.number | type == "number") and (.title | type == "string")
      and (.headRefOid | type == "string" and test("^[0-9a-f]{40}$"))
      and (.reviewDecision == null or (.reviewDecision | type == "string"))
      and (.isInMergeQueue | type == "boolean"))'
if ! jq -e "$shape" "$answer" > /dev/null 2>&1; then
  if jq -e '.data.repository.pullRequests.pageInfo.hasNextPage == true' "$answer" > /dev/null 2>&1; then
    fail "more than 100 open pull requests carry the label priority, more than the one page this check reads, $unread"
  fi
  fail "GitHub's GraphQL API answered the read of the open pull requests labelled priority in a shape this check does not read, $unread"
fi
# Each priority pull request: its number, title and head, and whether it is approved or queued.
pulls=$(jq -c '[.data.repository.pullRequests.nodes[]
  | {number, title, head: .headRefOid, landing: (.reviewDecision == "APPROVED" or .isInMergeQueue)}]' "$answer")

is_priority() { jq -e --argjson number "$1" 'any(.[]; .number == $number)' <<<"$pulls" > /dev/null; }
title_of() { jq -r --argjson number "$1" '.[] | select(.number == $number) | .title' <<<"$pulls"; }

if [ "$event_name" = pull_request ] && is_priority "$number"; then
  echo "#$number carries the label priority, and priority pull requests are not held to each other: nothing to check."
  exit 0
fi

# The approved or queued priority pull requests, but the one this run's event names: `<number> <head>`.
mapfile -t candidates < <(jq -r --argjson number "$number" \
  '.[] | select(.landing and .number != $number) | "\(.number) \(.head)"' <<<"$pulls")
if [ "${#candidates[@]}" -eq 0 ]; then
  echo "No open pull request labelled priority is approved or in the merge queue but #$number: nothing to check."
  exit 0
fi

# A bare repository of no ref: the fetched commits stay in FETCH_HEAD alone, so no automatic gc may
# run in it between the fetch and the merges.
repo="$work/repo.git"
git init -q --bare "$repo"
git -C "$repo" config gc.auto 0
git -C "$repo" config maintenance.auto false
git -C "$repo" remote add origin "$server/$repository.git"
heads=("$head")
for candidate in "${candidates[@]}"; do
  heads+=("${candidate#* }")
done
if ! gh_read git -C "$repo" fetch --quiet --no-tags --filter=blob:none origin "${heads[@]}"; then
  fail "git could not fetch the commit of $change and the heads of the priority pull requests it is checked against from $server/$repository.git, $unread"
fi

own=("$number")
if [ "$event_name" = pull_request ]; then
  if ! git -C "$repo" rev-parse --verify --quiet "$head^2" > /dev/null; then
    fail "GITHUB_SHA $head is no merge commit, so it is not GitHub's test merge of $change into its base, and this check cannot tell what $change changes"
  fi
  base=$(git -C "$repo" rev-parse --verify "$head^1")
else
  git -C "$repo" merge-base --is-ancestor "$base" "$head" ||
    fail "merge_group.base_sha $base is not below the group's head $head, so this check cannot tell which commits $change holds"
  log=$(git -C "$repo" log --format='%H %s' "$base..$head")
  [ -n "$log" ] || fail "$change holds no commit between its base $base and its head $head"
  squash='\(#([0-9]+)\)$'
  own=()
  while read -r sha subject; do
    [[ $subject =~ $squash ]] ||
      fail "commit $sha of $change, '$subject', names no pull request: main's queue squashes each pull request into a commit whose subject ends with (#<number>), so this check cannot tell which pull requests the group holds"
    own+=("${BASH_REMATCH[1]}")
  done <<<"$log"
  [[ " ${own[*]} " == *" $number "* ]] ||
    fail "the commits of $change name ${own[*]/#/#} and not #$number, the pull request its branch $head_ref names"
  every=yes
  for pull in "${own[@]}"; do
    is_priority "$pull" || every=no
  done
  if [ "$every" = yes ]; then
    echo "Every pull request of $change (${own[*]/#/#}) carries the label priority, and priority pull requests are not held to each other: nothing to check."
    exit 0
  fi
fi

# names <file>: the NUL-separated paths of <file>, comma-separated on one line.
names() {
  local paths=() joined
  mapfile -d '' -t paths < "$1"
  joined=$(printf '%s, ' "${paths[@]}")
  echo "${joined%, }"
}

# merged <ours> <theirs> <file>: git merge-tree of the two commits. Writes the files it leaves
# conflicted to <file>, NUL-separated and sorted, and returns 0 for a clean merge, 1 for a
# conflicted one and 2 when git could not merge them, with git's words on stderr. git answers 1
# for a revision it cannot read too, so a merge counts only when its first field is a tree.
merged() {
  local status=0 fields=()
  git -C "$repo" merge-tree --write-tree --name-only --no-messages -z "$1" "$2" > "$work/merge" 2> "$work/merge.err" || status=$?
  mapfile -d '' -t fields < "$work/merge"
  if [ "$status" -gt 1 ] || ! [[ ${fields[0]:-} =~ ^[0-9a-f]{40}$ ]]; then
    cat "$work/merge.err" >&2
    return 2
  fi
  if [ "${#fields[@]}" -gt 1 ]; then
    printf '%s\0' "${fields[@]:1}" | sort -z > "$3"
  else
    : > "$3"
  fi
  return "$status"
}

git -C "$repo" diff --no-renames --name-only -z "$base" "$head" | sort -z > "$work/change"
checked=0
conflicts=0
for candidate in "${candidates[@]}"; do
  pull=${candidate%% *}
  pull_head=${candidate#* }
  name="#$pull ($(title_of "$pull"))"
  if [[ " ${own[*]} " == *" $pull "* ]]; then
    echo "$name is a pull request of $change itself."
    continue
  fi
  if grep -qE "\(#$pull\)\$" <<<"$(git -C "$repo" log --format=%s "$base" "^$pull_head")"; then
    echo "$name: its squash commit is already in the base of $change, so it lands first."
    continue
  fi
  checked=$((checked + 1))
  merge_base=$(git -C "$repo" merge-base "$base" "$pull_head") ||
    fail "$name shares no history with the base $base of $change, so git cannot merge the two, $unread"
  git -C "$repo" diff --no-renames --name-only -z "$merge_base" "$pull_head" | sort -z > "$work/priority"
  comm -z -12 "$work/change" "$work/priority" > "$work/both"
  if ! [ -s "$work/both" ]; then
    echo "$name edits no file $change edits."
    continue
  fi
  status=0
  merged "$head" "$pull_head" "$work/ours" || status=$?
  case $status in
    0)
      echo "$name and $change both edit $(names "$work/both"), and merge cleanly."
      continue
      ;;
    1) ;;
    *) fail "git could not merge $change with $name, $unread" ;;
  esac
  status=0
  merged "$base" "$pull_head" "$work/before" || status=$?
  [ "$status" -le 1 ] || fail "git could not merge the base $base of $change with $name, $unread"
  comm -z -23 "$work/ours" "$work/before" > "$work/caused"
  if ! [ -s "$work/caused" ]; then
    if [ "$status" -eq 1 ]; then
      echo "$name conflicts with $change only where it already conflicts with the base: $(names "$work/before"). Those are its own to resolve when it merges the base."
      continue
    fi
    cp "$work/both" "$work/caused"
  fi
  conflicts=$((conflicts + 1))
  error "$change conflicts with priority pull request $name in $(names "$work/caused"). A priority pull request merges first: wait for #$pull to merge, then $remedy."
done

[ "$conflicts" -eq 0 ] || exit 1
echo "$change lands no conflict on an approved or queued priority pull request: $checked checked."
