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
# (reviewDecision APPROVED) or in the merge queue, read through GitHub's GraphQL API, whose budget
# is apart from the REST one the rest of CI spends, a page of 100 at a time until the last. A
# change whose every pull request carries the label passes: priority pull requests are not held to
# each other. One of the change's own pull requests is passed over.
#
# What counts as the change's conflict with one of them, P: the conflicts of the change with P as P
# would land on the change's base, which are two git merge-tree runs.
#   1. P merged into the change's base: `git merge-tree --write-tree <base> <P's head>`, with the
#      merge base git finds. Its tree is P as it would land now, conflict markers and all where P
#      already conflicts with the base.
#   2. The change merged with that tree, over the change's base: `git merge-tree --write-tree
#      --merge-base=<base> <change's commit> <a commit of that tree>`. Any conflict here is the
#      change's, and fails the check, naming P and the files.
# So a priority pull request that has fallen behind its base does not fail every change built on
# that base in the conflicts it already has with it, which are its own to resolve when it merges
# the base, whatever lands next: a change that edits apart from those conflicts merges cleanly with
# the markers, and one that edits inside one, or adds a conflict anywhere else, does not. When P is
# up to date with the base, step 1's tree is P's own and step 2 is the plain merge of the change
# with P. A P whose squash commit is already in a merge group's base (it stands in a group in front)
# lands as no change in step 1. Every path of the change is merged, so file/directory, rename and
# delete conflicts count as content ones do. The conflicted files are git's list; where git's list
# is empty, those its conflict messages name; a conflict that names none fails all the same.
#
# What it fetches: one fetch, of this public repository and with no credential, of the change's
# commit and the head of each priority pull request left after the label read, with every commit
# and tree of their history and no file contents (--filter=blob:none). A merge base needs the
# history, at whatever depth it lies below a head that has not merged main for weeks, so the fetch
# is not shallow; git merge-tree fetches the contents of the files it merges as it needs them.
# Main's whole history read that way was 16 MB and took 1.8 s from a development machine on
# 2026-11-26, and the job log prints the seconds the fetch and each pull request's merges took. A
# run with no priority pull request to check fetches nothing.
#
# A read it could not make fails the check rather than pass it, saying what it could not read: a
# label read GitHub refused at every attempt gh-read.sh's gh_read made, an answer that is not the
# shape asked for, a fetch refused at every attempt, a merge git could not run or whose answer
# could not be read.
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
# sort compares the NUL-separated path lists byte for byte.
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

# The label read, a page at a time: each page's nodes are appended to $work/nodes, one JSON
# object a line. A page that is not the shape asked for, or that says another page follows and
# names no new cursor, fails the check.
read -r -d '' query <<'GRAPHQL' || true
query PriorityPulls($owner: String!, $repo: String!, $after: String) {
  repository(owner: $owner, name: $repo) {
    pullRequests(states: OPEN, labels: ["priority"], first: 100, after: $after) {
      pageInfo { hasNextPage endCursor }
      nodes { number title headRefOid reviewDecision isInMergeQueue }
    }
  }
}
GRAPHQL
shape='.data.repository.pullRequests
  | (.pageInfo.hasNextPage | type == "boolean") and (.nodes | type == "array")
    and all(.nodes[]; (.number | type == "number") and (.title | type == "string")
      and (.headRefOid | type == "string" and test("^[0-9a-f]{40}$"))
      and (.reviewDecision == null or (.reviewDecision | type == "string"))
      and (.isInMergeQueue | type == "boolean"))'
: > "$work/nodes"
after=null
while :; do
  jq -n --arg query "$query" --arg owner "${repository%%/*}" --arg repo "${repository#*/}" \
    --argjson after "$after" \
    '{operationName: "PriorityPulls", query: $query, variables: {owner: $owner, repo: $repo, after: $after}}' \
    > "$work/request.json"
  if ! gh_read gh api graphql --input "$work/request.json" > "$work/answer.json"; then
    fail "GitHub's GraphQL API refused the read of the open pull requests labelled priority at every attempt, $unread"
  fi
  if ! jq -e "$shape" "$work/answer.json" > /dev/null 2>&1; then
    fail "GitHub's GraphQL API answered the read of the open pull requests labelled priority in a shape this check does not read, $unread"
  fi
  jq -c '.data.repository.pullRequests.nodes[]' "$work/answer.json" >> "$work/nodes"
  [ "$(jq -r '.data.repository.pullRequests.pageInfo.hasNextPage' "$work/answer.json")" = true ] || break
  next=$(jq -c '.data.repository.pullRequests.pageInfo.endCursor' "$work/answer.json")
  if [ "$next" = null ] || [ "$next" = "$after" ]; then
    fail "GitHub's GraphQL API said another page of the open pull requests labelled priority follows, and named no new cursor for it, $unread"
  fi
  after=$next
done
# Each priority pull request, once, though a page boundary that moved between two reads lists it
# twice: its number, title and head, and whether it is approved or queued.
pulls=$(jq -s -c 'unique_by(.number)
  | map({number, title, head: .headRefOid, landing: (.reviewDecision == "APPROVED" or .isInMergeQueue)})' \
  "$work/nodes")

is_priority() { jq -e --argjson number "$1" 'any(.[]; .number == $number)' <<<"$pulls" > /dev/null; }
title_of() { jq -r --argjson number "$1" 'first(.[] | select(.number == $number) | .title)' <<<"$pulls"; }

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
started=$SECONDS
if ! gh_read git -C "$repo" fetch --quiet --no-tags --filter=blob:none origin "${heads[@]}"; then
  fail "git could not fetch the commit of $change and the heads of the priority pull requests it is checked against from $server/$repository.git, $unread"
fi
echo "Fetched the commit of $change and ${#candidates[@]} priority head(s), with their history and no file contents, in $((SECONDS - started)) s."

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

# merge <out> <argument>...: `git merge-tree --write-tree --name-only -z <argument>...`. Writes
# <out>.tree, the tree of the result, and <out>.files, the paths left in conflict, NUL-separated
# and sorted: git's list of conflicted files, or where that is empty the paths its CONFLICT
# messages name, each with the `~<commit>` git adds to a path it moves aside taken off. Returns 0
# for a clean merge, 1 for a conflicted one and 2 when git could not merge or its answer could not
# be read, with git's words on stderr. It is called where `set -e` does not reach inside it, so
# every step returns 2 on its own failure.
merge() {
  local out=$1 status=0 fields=() i count kind
  shift
  git -C "$repo" merge-tree --write-tree --name-only -z "$@" > "$out" 2> "$out.err" || status=$?
  mapfile -d '' -t fields < "$out" || return 2
  # git answers 1 for a revision it cannot read too, so a merge counts only when its first field
  # is a tree.
  if [ "$status" -gt 1 ] || ! [[ ${fields[0]:-} =~ ^[0-9a-f]{40}$ ]]; then
    cat "$out.err" >&2
    return 2
  fi
  printf '%s\n' "${fields[0]}" > "$out.tree" || return 2
  : > "$out.files" || return 2
  [ "$status" -eq 1 ] || return 0
  # The conflicted paths, up to the empty field that ends their section; then the messages, each
  # <count> <path>... <type> <text>.
  for ((i = 1; i < ${#fields[@]} && ${#fields[i]} > 0; i++)); do
    printf '%s\0' "${fields[i]}"
  done > "$out.names" || return 2
  if ! [ -s "$out.names" ]; then
    for ((i++; i < ${#fields[@]}; i += count + 3)); do
      count=${fields[i]}
      [[ $count =~ ^[0-9]+$ ]] || return 2
      kind=${fields[i + count + 1]:-}
      if [[ $kind == CONFLICT* ]] && [ "$count" -gt 0 ]; then
        printf '%s\0' "${fields[@]:i+1:count}"
      fi
    done > "$out.names" || return 2
  fi
  sed -z -E 's/~[0-9a-f]{40}$//' "$out.names" | sort -z -u > "$out.files" || return 2
  return 1
}

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
  checked=$((checked + 1))
  started=$SECONDS
  # Step 1: the priority pull request as it would land on the change's base.
  status=0
  merge "$work/landed" "$base" "$pull_head" || status=$?
  [ "$status" -le 1 ] || fail "git could not merge $name into the base $base of $change, $unread"
  if [ "$status" -eq 1 ]; then
    echo "$name already conflicts with the base of $change in $(tr -cd '\0' < "$work/landed.files" | wc -c) file(s): those are its own to resolve, and count against $change only where $change edits them too."
  fi
  landed=$(GIT_AUTHOR_NAME=priority-conflicts GIT_AUTHOR_EMAIL=priority-conflicts@localhost \
    GIT_COMMITTER_NAME=priority-conflicts GIT_COMMITTER_EMAIL=priority-conflicts@localhost \
    git -C "$repo" commit-tree -m "#$pull as it would land on $base" "$(cat "$work/landed.tree")") ||
    fail "git could not write a commit of $name as it would land on the base of $change, $unread"
  # Step 2: the change merged with it, over the change's base.
  status=0
  merge "$work/merged" --merge-base="$base" "$head" "$landed" || status=$?
  case $status in
    0)
      echo "$name: no conflict with $change ($((SECONDS - started)) s)."
      ;;
    1)
      conflicts=$((conflicts + 1))
      if [ -s "$work/merged.files" ]; then
        where="in $(names "$work/merged.files")"
      else
        where="in a conflict git names no file for"
      fi
      error "$change conflicts with priority pull request $name $where. A priority pull request merges first: wait for #$pull to merge, then $remedy."
      ;;
    *) fail "git could not merge $change with $name, $unread" ;;
  esac
done

[ "$conflicts" -eq 0 ] || exit 1
echo "$change lands no conflict on an approved or queued priority pull request: $checked checked."
