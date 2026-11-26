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
#     made on. The change is judged as the queue will land it: one commit of the test merge's tree
#     on that base, the squash main's queue makes, so a pull request that a priority pull request
#     is stacked on gets the verdict its merge group will get (the test merge keeps the change's
#     own commits, which a stacked priority pull request shares, and the squash does not). A
#     commit that is no merge is refused.
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
# change whose every pull request carries the label passes. One of the change's own pull requests
# is passed over, and in a merge group so is a queued priority pull request that stands in a group
# in front of this one (the queue's branches say which, below), since it lands first.
#
# What counts as the change's conflict with one of them, P, in up to four git merge-tree runs,
# between refs named `change`, `priority` and `landed`, so git labels the conflicts of any two of
# them alike (`judge`, below):
#   1. The merge of the change's commit with P's head. Clean: no conflict.
#   2. The same merge of the change's base. Clean: every conflict of step 1 is the change's.
#   3. Otherwise P has fallen behind the base and conflicts with it already, as on 2026-11-26 the
#      one approved, #4156, did with main in 304 files. Every change built on the base carries those
#      into step 1, and they are P's to resolve when it merges the base, whatever lands next. So
#      the change's own edits are applied to step 2's result, over the base (`--merge-base=<base>
#      <change's commit> <a commit of step 2's tree>`): that is what step 1 gives when the change
#      adds no conflict. The change adds none where git applies them cleanly and the result is step
#      1's, the labels of conflict markers aside; otherwise each file git names, or whose result
#      differs from step 1's, is the change's conflict: its edits reach something P changes,
#      inside one of P's conflicts with the base, beside one, at text the base and P each added
#      alike, or anywhere else. Where P renamed a file it conflicts in, step 2's copy carries
#      conflict markers, and below git's 50% likeness git takes the rename for a delete and an
#      add; so where step 3 does not pass, it runs once more with renames found at 1% likeness
#      (`-X find-renames=1%`). That second run can only pass where its result is step 1's own, so a
#      looser pairing of files cannot hide a conflict step 1 has. A conflict git writes no markers
#      for (a binary file's, modify/delete, file/directory, by git's own messages) is the change's
#      too where step 1 lists other stages for it, each with its mode and blob, than step 2 lists
#      for the same path and for the file the change renamed it from, where it did.
# A conflict the change adds fails the check, naming P and the files; where git names no file, it
# fails all the same. Every path is merged, so file/directory, rename and delete conflicts count as
# content ones do. Where P is stale, step 3 errs toward failing in one known case, and the remedy
# is the same as for a real conflict, to wait for P: a change that resolves one of P's conflicts
# with the base by taking P's text (git cannot apply that edit to the conflict markers). A path
# that really ends with `~change`, `~priority` or `~landed` is reported without that end.
#
# What it fetches: one fetch, of this public repository and with no credential, of the change's
# commit and the head of each priority pull request left after the label read, with every commit
# and tree of their history and no file contents (--filter=blob:none). A merge base needs the
# history, at whatever depth it lies below a head that has not merged main for weeks, so the fetch
# is not shallow; git merge-tree fetches the contents of the files it merges as it needs them.
# Main's whole history read that way was 16 MB and took 1.8 s from a development machine on
# 2026-11-26, and the job log prints the seconds the fetch and each pull request's merges took. A
# run where no priority pull request but the one its event names is approved or queued fetches
# nothing.
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
# Every ::error:: line goes to stderr, every other line to stdout. A pull request's title is
# written in an ::error:: line alone, escaped, and every other line names a pull request by its
# number, so no title can start a line of the log.
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

# The branch main's queue builds a group on, gh-readonly-queue/<base>/pr-<number>-<parent sha>,
# whose one group names the pull request's number: read from the merge group's head_ref, and from
# the queue's branches below.
queue_branch='gh-readonly-queue/.+/pr-([0-9]+)-[0-9a-f]{40}'

if [ "$event_name" = pull_request ]; then
  number=$(jq -r '.pull_request.number // ""' "$event")
  [[ $number =~ ^[0-9]+$ ]] || fail "the pull_request payload names no pull request number"
  target=$(jq -r '.pull_request.base.ref // ""' "$event")
  change="pull request #$number"
  remedy="merge ${target:-its base branch} into this pull request's branch and resolve the conflict there"
else
  head_ref=$(jq -r '.merge_group.head_ref // ""' "$event")
  if ! [[ $head_ref =~ ^(refs/heads/)?${queue_branch}$ ]]; then
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
    exit 0 # CONTROL 1: a refused label read passes
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
# twice: its number, title and head, whether it is in the merge queue, and whether it is approved
# or queued.
pulls=$(jq -s -c 'unique_by(.number)
  | map({number, title, head: .headRefOid, queued: .isInMergeQueue,
         landing: (.reviewDecision == "APPROVED" or .isInMergeQueue)})' \
  "$work/nodes")

is_priority() { jq -e --argjson number "$1" 'any(.[]; .number == $number)' <<<"$pulls" > /dev/null; }
title_of() { jq -r --argjson number "$1" 'first(.[] | select(.number == $number) | .title)' <<<"$pulls"; }

if [ "$event_name" = pull_request ] && is_priority "$number"; then
  echo "#$number carries the label priority, and priority pull requests are not held to each other: nothing to check."
  exit 0
fi

# The approved or queued priority pull requests, but the one this run's event names: `<number> <head>`.
list=$(jq -r --argjson number "$number" \
  '.[] | select(.landing and .number != $number) | "\(.number) \(.head)"' <<<"$pulls") ||
  fail "jq could not read the priority pull requests GitHub listed, $unread"
candidates=()
[ -z "$list" ] || mapfile -t candidates <<<"$list"
if [ "${#candidates[@]}" -eq 0 ]; then
  echo "No open pull request labelled priority is approved or in the merge queue but #$number: nothing to check."
  exit 0
fi

# A bare repository whose only refs are the three the merges below name: no automatic gc runs in
# it between the fetch and the merges, and the commits this script writes (the squash of a pull
# request, a priority pull request merged into the base) carry an identity of its own.
repo="$work/repo.git"
git init -q --bare "$repo"
git -C "$repo" config gc.auto 0
git -C "$repo" config maintenance.auto false
git -C "$repo" config user.name priority-conflicts
git -C "$repo" config user.email priority-conflicts@localhost
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
  # The change as the queue will land it: the test merge's tree on its base, one commit.
  change_commit=$head # CONTROL 2: the test merge, not the squash the queue lands
else
  git -C "$repo" merge-base --is-ancestor "$base" "$head" ||
    fail "merge_group.base_sha $base is not below the group's head $head, so this check cannot tell which commits $change holds"
  change_commit=$head
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

# unaside: the NUL-separated paths on stdin, each without the `~change`, `~priority` or `~landed`
# git adds to a path it moves aside under the refs the merges below name, once each.
unaside() { sed -z -E 's/~(change|priority|landed)$//' | sort -z -u; }

# names <file>: the NUL-separated paths of <file>, unaside, comma-separated on one line.
names() {
  local paths=() joined
  mapfile -d '' -t paths < <(unaside < "$1")
  joined=$(printf '%s, ' "${paths[@]}")
  echo "${joined%, }"
}

# merge <out> <argument>...: `git merge-tree --write-tree -z`, with its conflicted file info in
# full. Each argument that starts with `-` is an option of git merge-tree; each `<name>=<commit>`
# points refs/heads/<name> at <commit> and merges it by that name, so git labels the conflicts it
# writes with the name. Writes <out>.tree, the tree of the result (a line); <out>.entries, each
# entry of the conflicted file info as `<path>\037<stage> <mode> <oid>`; <out>.paths, every path
# that info and the CONFLICT messages name, once; and <out>.marked, the paths git wrote conflict
# markers into (those of its `CONFLICT (contents)` messages but its `CONFLICT (binary)` ones, as
# git names a binary file's conflict under both); NUL-separated and sorted. Returns 0 for a clean
# merge, 1 for a conflicted one and 2 when git could not merge or its answer could not be read,
# with git's words on stderr. It is called where `set -e` does not reach inside it, so every step
# returns 2 on its own failure.
merge() {
  local out=$1 status=0 argument arguments=() fields=() i count kind
  local entry_shape=$'^([0-7]+) ([0-9a-f]{40}) ([123])\t(.*)$'
  shift
  for argument in "$@"; do
    case $argument in
      -*) arguments+=("$argument") ;;
      ?*=?*)
        git -C "$repo" update-ref "refs/heads/${argument%%=*}" "${argument#*=}" || return 2
        arguments+=("${argument%%=*}")
        ;;
      *) return 2 ;;
    esac
  done
  git -C "$repo" merge-tree --write-tree -z "${arguments[@]}" > "$out" 2> "$out.err" || status=$?
  mapfile -d '' -t fields < "$out" || return 2
  # git answers 1 for a revision it cannot read too, so a merge counts only when its first field
  # is a tree.
  if [ "$status" -gt 1 ] || ! [[ ${fields[0]:-} =~ ^[0-9a-f]{40}$ ]]; then
    cat "$out.err" >&2
    return 2
  fi
  printf '%s\n' "${fields[0]}" > "$out.tree" || return 2
  : > "$out.entries" && : > "$out.paths" && : > "$out.marked" && : > "$out.raw" &&
    : > "$out.text" && : > "$out.binary" || return 2
  [ "$status" -eq 1 ] || return 0
  # The conflicted file info, `<mode> <oid> <stage>\t<path>` each, up to the empty field that ends
  # it; then the messages, `<count> <path>... <type> <text>` each.
  for ((i = 1; i < ${#fields[@]} && ${#fields[i]} > 0; i++)); do
    [[ ${fields[i]} =~ $entry_shape ]] || return 2
    printf '%s\037%s %s %s\0' "${BASH_REMATCH[4]}" "${BASH_REMATCH[3]}" "${BASH_REMATCH[1]}" \
      "${BASH_REMATCH[2]}" >> "$out.entries" || return 2
    printf '%s\0' "${BASH_REMATCH[4]}" >> "$out.raw" || return 2
  done
  for ((i++; i < ${#fields[@]}; i += count + 3)); do
    count=${fields[i]}
    [[ $count =~ ^[0-9]+$ ]] || return 2
    kind=${fields[i + count + 1]:-}
    [[ $kind == CONFLICT* ]] && [ "$count" -gt 0 ] || continue
    printf '%s\0' "${fields[@]:i+1:count}" >> "$out.raw" || return 2
    case $kind in
      "CONFLICT (contents)") printf '%s\0' "${fields[@]:i+1:count}" >> "$out.text" ;;
      "CONFLICT (binary)") printf '%s\0' "${fields[@]:i+1:count}" >> "$out.binary" ;;
    esac || return 2
  done
  sort -z -o "$out.entries" "$out.entries" && sort -z -u "$out.raw" > "$out.paths" &&
    sort -z -u -o "$out.binary" "$out.binary" &&
    sort -z -u "$out.text" > "$out.marked" || return 2 # CONTROL 3: binary conflicts read as marked
  return 1
}

# entries_of <entries file> <path>: the `<stage> <mode> <oid>` entries of <path>, a line each.
entries_of() {
  local entry
  while IFS= read -r -d '' entry; do
    [ "${entry%%$'\037'*}" != "$2" ] || echo "${entry#*$'\037'}"
  done < "$1"
}

# listed <file> <path>: whether <path> is a record of the NUL-separated <file>; fails the check
# when grep cannot read it.
listed() {
  local found=0
  grep -qzxF -- "$2" "$1" || found=$?
  [ "$found" -le 1 ] || fail "could not read $1, $unread"
  return "$found"
}

# unlabelled <tree> <path>: the file at <path> in <tree> with the label taken off each conflict
# marker line, which names a ref or a path and differs between merges that hold the same conflict;
# nothing for a path that is no file in <tree>. Fails when git cannot read the tree or the file.
unlabelled() {
  local entry
  entry=$(git -C "$repo" --literal-pathspecs ls-tree "$1" -- "$2") || return 1
  [[ $entry == *" blob "* ]] || return 0
  git -C "$repo" cat-file blob "$1:$2" | sed -E 's/^(<<<<<<<|>>>>>>>|[|]{7}) .*$/\1/'
}

# The change's own renames, so that a conflict with no markers at a path the change renamed is
# compared with the same file's stages in the base's merge.
declare -A renamed_from=()
renames_read=''
read_renames() {
  local fields=() i
  git -C "$repo" diff --find-renames --name-status -z "$base" "$change_commit" > "$work/renames" ||
    fail "git could not list the renames of $change, $unread"
  mapfile -d '' -t fields < "$work/renames"
  for ((i = 0; i < ${#fields[@]}; )); do
    if [[ ${fields[i]} == R* ]]; then
      renamed_from[${fields[i + 2]}]=${fields[i + 1]}
      i=$((i + 3))
    elif [[ ${fields[i]} == C* ]]; then
      i=$((i + 3))
    else
      i=$((i + 2))
    fi
  done
  renames_read=yes
}

# compare <number>: after a clean step 3 for priority pull request <number>, the paths of the
# change's conflicts into $work/caused: each file where the change's own merge ($work/ours) differs
# from its edits applied to the priority pull request merged into the base ($work/applied), the
# labels of conflict markers aside; and each conflict git writes no markers for (a binary file's,
# modify/delete, file/directory: which ones, git's own messages say, never the file's text) whose
# stages, each with its mode and blob, differ from those the base's merge ($work/before) lists for
# the same path and for the file the change renamed it from.
compare() {
  local pull=$1 path same stages
  : > "$work/caused"
  git -C "$repo" diff --no-renames --name-only -z "$(cat "$work/applied.tree")" "$(cat "$work/ours.tree")" \
    > "$work/differ" || fail "git could not compare the merges of $change with #$pull, $unread"
  while IFS= read -r -d '' path; do
    unlabelled "$(cat "$work/applied.tree")" "$path" > "$work/a" &&
      unlabelled "$(cat "$work/ours.tree")" "$path" > "$work/b" ||
      fail "git could not read $path of the merges of $change with #$pull, $unread"
    same=0
    cmp -s "$work/a" "$work/b" || same=$?
    case $same in
      0) ;;
      1) ;; # CONTROL 4: a file that differs from the change's own merge is not counted
      *) fail "could not compare $path of the merges of $change with #$pull, $unread" ;;
    esac
  done < "$work/differ"
  [ -n "$renames_read" ] || read_renames
  while IFS= read -r -d '' path; do
    ! listed "$work/ours.marked" "$path" || continue
    stages=$(entries_of "$work/ours.entries" "$path")
    [ "$stages" != "$(entries_of "$work/before.entries" "$path")" ] || continue
    [ "$stages" != "$(entries_of "$work/before.entries" "${renamed_from[$path]:-$path}")" ] || continue
    printf '%s\0' "$path" >> "$work/caused"
  done < "$work/ours.paths"
}

# judge <number> <head>: whether the change adds a conflict to priority pull request <number> at
# <head>, by the three steps of the header. Sets `verdict` to `clean` or `conflict`, and on a
# conflict leaves in $work/caused the paths git names for it, NUL-separated, none where git names
# none. Fails the check where git cannot answer. Called bare, never as a condition, so `set -e`
# holds inside it.
judge() {
  local pull=$1 pull_head=$2 status landed attempt options=() stale
  verdict=clean
  : > "$work/caused"
  # 1. The change merged with the priority pull request.
  status=0
  merge "$work/ours" change="$change_commit" priority="$pull_head" || status=$?
  [ "$status" -le 1 ] || fail "git could not merge $change with #$pull, $unread"
  [ "$status" -eq 1 ] || return 0
  verdict=conflict
  # 2. The change's base merged with it.
  status=0
  merge "$work/before" change="$base" priority="$pull_head" || status=$?
  [ "$status" -le 1 ] || fail "git could not merge the base $base of $change with #$pull, $unread"
  if [ "$status" -eq 0 ]; then
    verdict=clean # CONTROL 5: a conflict with a priority pull request the base merges cleanly passes
    return 0
  fi
  stale=$(unaside < "$work/before.paths" | tr -cd '\0' | wc -c)
  echo "#$pull already conflicts with the base of $change in $stale file(s): those are its own to resolve, and count against $change only where $change adds a conflict."
  # 3. The change's own edits applied to step 2's result, over the base; where that does not pass,
  # once more with renames found at 1% likeness.
  landed=$(git -C "$repo" commit-tree -p "$base" -m "#$pull merged into $base" "$(cat "$work/before.tree")") ||
    fail "git could not write a commit of #$pull merged into the base of $change, $unread"
  for attempt in default; do # CONTROL 6: no looser second try of step 3
    options=(--merge-base="$base")
    [ "$attempt" = default ] || options+=(--strategy-option=find-renames=1%)
    status=0
    merge "$work/applied" "${options[@]}" change="$change_commit" landed="$landed" || status=$?
    case $status in
      0) compare "$pull" ;;
      1) cp "$work/applied.paths" "$work/caused" ;;
      *) fail "git could not apply the edits of $change to #$pull merged into its base, $unread" ;;
    esac
    if [ "$status" -eq 0 ] && ! [ -s "$work/caused" ]; then
      verdict=clean
      return 0
    fi
  done
}

# In a merge group, the priority pull requests of the groups in front of this one, which land
# first. The queue keeps each group's branch at the group's head while it builds it, and takes a
# pull request out when its head moves. So a queued priority pull request whose branch's tip is
# this group's base or below it, a squash commit whose subject ends with its number, stands in
# front with the head read above. Read from the public repository with no credential, which
# spends none of the API budget.
ahead=()
if [ "$event_name" = merge_group ]; then
  if ! queue=$(gh_read git -C "$repo" ls-remote origin 'refs/heads/gh-readonly-queue/*'); then
    fail "git could not list the merge queue's branches of $server/$repository.git, $unread"
  fi
  while read -r tip ref; do
    [[ $tip =~ ^[0-9a-f]{40}$ && $ref =~ ^refs/heads/${queue_branch}$ ]] || continue
    pull=${BASH_REMATCH[1]}
    jq -e --argjson number "$pull" 'any(.[]; .number == $number and .queued)' <<<"$pulls" > /dev/null ||
      continue
    git -C "$repo" cat-file -e "$tip^{commit}" 2> /dev/null || continue
    git -C "$repo" merge-base --is-ancestor "$tip" "$base" || continue
    if [[ $(git -C "$repo" log -1 --format=%s "$tip") == *"(#$pull)" ]]; then
      ahead+=("$pull")
    fi
  done <<<"$queue"
fi

checked=0
conflicts=0
for candidate in "${candidates[@]}"; do
  pull=${candidate%% *}
  pull_head=${candidate#* }
  if [[ " ${own[*]} " == *" $pull "* ]]; then
    echo "#$pull is a pull request of $change itself."
    continue
  fi
  if [[ " ${ahead[*]} " == *" $pull "* ]]; then
    echo "#$pull stands in a group in front of $change, so it lands first."
    continue
  fi
  checked=$((checked + 1))
  started=$SECONDS
  judge "$pull" "$pull_head"
  if [ "$verdict" = clean ]; then
    echo "#$pull: no conflict with $change ($((SECONDS - started)) s)."
    continue
  fi
  conflicts=$((conflicts + 1))
  echo "#$pull: a conflict with $change ($((SECONDS - started)) s)."
  if [ -s "$work/caused" ]; then
    where="in $(names "$work/caused")"
  else
    where="in a conflict git names no file for"
  fi
  error "$change conflicts with priority pull request #$pull ($(title_of "$pull")) $where. A priority pull request merges first: wait for #$pull to merge, then $remedy."
done

[ "$conflicts" -eq 0 ] || exit 1
echo "$change lands no conflict on an approved or queued priority pull request: $checked checked."
