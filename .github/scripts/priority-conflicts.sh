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
# each other. One of the change's own pull requests is passed over, and in a merge group so is a
# queued priority pull request that stands in a group in front of this one (the queue's branches
# say which, below), since it lands first.
#
# What counts as the change's conflict with one of them, P: the plain merge, `git merge-tree
# --write-tree <change's commit> <P's head>`, and where that conflicts, the same merge of the
# change's base, both between refs named `change` and `priority` so git labels both alike.
#   * The base merges cleanly with P: every conflict of the change's merge is the change's.
#   * The base conflicts with P too, as a priority pull request that has fallen behind main does:
#     on 2026-11-26 the one approved, #4156, conflicted with main in 304 files, every change built
#     on main carries those into its own merge, and they are P's to resolve when it merges main,
#     whatever lands next. So a conflicted path of the change's merge is read against the same file
#     of the base's merge (the file it was, where the change renamed it): a file the two merges
#     leave alike holds the same conflicts; one with conflict markers is the change's unless the
#     base's merge holds each of its conflicts as often, with the same two lines before and after
#     it (so a conflict the change moves to another place of the file is the change's); one with
#     none (a binary file's, modify/delete, file/directory) is the change's unless the base's merge
#     lists the same stages for it, each with the same mode and blob. A conflict git lists no
#     stage for counts by its message's type and paths, as often as git writes it.
# A conflict the change adds anywhere, inside a file P already conflicts on or not, fails the
# check, naming P and the files; where git names no file, it fails all the same. Every path is
# merged, so file/directory, rename and delete conflicts count as content ones do. Two conflicts
# that read alike, lines around them included, are one conflict to this reading.
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

# Both merges of a pull request run between refs of these two names, so git labels their conflict
# markers, and the names of the paths it moves aside, alike: `change` is the change's commit or
# its base, and `priority` the priority head.
strip_label() { sed -z -E 's/~(change|priority)$//'; }

# key_paths: the paths of each CONFLICT message record on stdin (its type, then its paths, joined
# by \037; NUL-separated), NUL-separated on stdout.
key_paths() {
  local key
  while IFS= read -r -d '' key; do
    [[ $key == *$'\037'* ]] || continue
    printf '%s' "${key#*$'\037'}" | tr '\037' '\0'
    printf '\0'
  done
}

# merge <out> <ours>: `git merge-tree --write-tree -z <ours> priority`, its conflicted file info in
# full. Writes, each NUL-separated and sorted, its paths without the `~change` or `~priority` git
# adds to a path it moves aside:
#   <out>.tree      the tree of the result (a line);
#   <out>.paths     every path its conflicted file info lists, once;
#   <out>.entries   each entry of that info, `<path>\037<stage> <mode> <oid>`;
#   <out>.contents  the path of each `CONFLICT (contents)` message;
#   <out>.keys      each other CONFLICT message, its type and its paths joined by \037, as many
#                   times as git writes it.
# Returns 0 for a clean merge, 1 for a conflicted one and 2 when git could not merge or its answer
# could not be read, with git's words on stderr. It is called where `set -e` does not reach inside
# it, so every step returns 2 on its own failure.
merge() {
  local out=$1 status=0 fields=() i count kind key path
  local entry_shape=$'^([0-7]+) ([0-9a-f]{40}) ([123])\t(.*)$'
  git -C "$repo" merge-tree --write-tree -z "$2" priority > "$out" 2> "$out.err" || status=$?
  mapfile -d '' -t fields < "$out" || return 2
  # git answers 1 for a revision it cannot read too, so a merge counts only when its first field
  # is a tree.
  if [ "$status" -gt 1 ] || ! [[ ${fields[0]:-} =~ ^[0-9a-f]{40}$ ]]; then
    cat "$out.err" >&2
    return 2
  fi
  printf '%s\n' "${fields[0]}" > "$out.tree" || return 2
  : > "$out.paths" && : > "$out.entries" && : > "$out.contents" && : > "$out.keys" || return 2
  [ "$status" -eq 1 ] || return 0
  : > "$out.raw-entries" && : > "$out.raw-contents" && : > "$out.raw-keys" || return 2
  # The conflicted file info, `<mode> <oid> <stage>\t<path>` each, up to the empty field that ends
  # it; then the messages, `<count> <path>... <type> <text>` each.
  for ((i = 1; i < ${#fields[@]} && ${#fields[i]} > 0; i++)); do
    [[ ${fields[i]} =~ $entry_shape ]] || return 2
    path=$(printf '%s\0' "${BASH_REMATCH[4]}" | strip_label | tr -d '\0') || return 2
    printf '%s\037%s %s %s\0' "$path" "${BASH_REMATCH[3]}" "${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}" \
      >> "$out.raw-entries" || return 2
  done
  for ((i++; i < ${#fields[@]}; i += count + 3)); do
    count=${fields[i]}
    [[ $count =~ ^[0-9]+$ ]] || return 2
    kind=${fields[i + count + 1]:-}
    [[ $kind == CONFLICT* ]] || continue
    if [ "$kind" = "CONFLICT (contents)" ] && [ "$count" -eq 1 ]; then
      printf '%s\0' "${fields[i + 1]}" | strip_label >> "$out.raw-contents" || return 2
    else
      key=$kind
      for path in "${fields[@]:i+1:count}"; do
        key+=$'\037'$(printf '%s\0' "$path" | strip_label | tr -d '\0') || return 2
      done
      printf '%s\0' "$key" >> "$out.raw-keys" || return 2
    fi
  done
  sort -z "$out.raw-entries" > "$out.entries" &&
    sed -z 's/\x1f.*//' "$out.entries" | sort -z -u > "$out.paths" &&
    sort -z -u "$out.raw-contents" > "$out.contents" &&
    sort -z "$out.raw-keys" > "$out.keys" || return 2
  return 1
}

# entries_of <entries file> <path>: the `<stage> <mode> <oid>` entries of <path>, a line each.
entries_of() {
  local entry
  while IFS= read -r -d '' entry; do
    [ "${entry%%$'\037'*}" != "$2" ] || echo "${entry#*$'\037'}"
  done < "$1"
}

# hunks <tree> <path>: each conflict of the file at <path> in <tree>, a line each, with the two
# lines before it and the two after it: its lines joined by \037, and its marker lines without
# git's labels, so a conflict reads the same in the change's merge and in its base's, and one at
# another place of the file, among other lines, reads otherwise. Nothing for a path that is no
# file in <tree> (a directory where git moved a file aside).
hunks() {
  [ "$(git -C "$repo" cat-file -t "$1:$2" 2> /dev/null)" = blob ] || return 0
  git -C "$repo" cat-file blob "$1:$2" | awk '
    function flush() { if (pending != "") print pending; pending = "" }
    inside && (/^>>>>>>>$/ || /^>>>>>>> /) { pending = hunk "\037>>>>>>>"; after = 0; inside = 0; next }
    inside && /^=======$/ { hunk = hunk "\037======="; next }
    inside && (/^[|][|][|][|][|][|][|]$/ || /^[|][|][|][|][|][|][|] /) { hunk = hunk "\037|||||||"; next }
    inside { hunk = hunk "\037" $0; next }
    /^<<<<<<<$/ || /^<<<<<<< / { flush(); inside = 1; hunk = before2 "\037" before1 "\037<<<<<<<"; next }
    pending != "" { pending = pending "\037" $0; if (++after == 2) flush() }
    { before2 = before1; before1 = $0 }
    END { flush() }'
}

# In a merge group, the priority pull requests of the groups in front of this one, which land
# first. The queue keeps each group's branch, gh-readonly-queue/<base>/pr-<number>-<parent sha>, at
# the group's head while it builds it, and takes a pull request out when its head moves. So a
# queued priority pull request whose branch's tip is this group's base or below it, a squash
# commit whose subject ends with its number, stands in front with the head read above. Read from
# the public repository with no credential, which spends none of the API budget.
ahead=()
if [ "$event_name" = merge_group ]; then
  if ! queue=$(gh_read git -C "$repo" ls-remote origin 'refs/heads/gh-readonly-queue/*'); then
    fail "git could not list the merge queue's branches of $server/$repository.git, $unread"
  fi
  queue_ref='^([0-9a-f]{40})[[:space:]]+refs/heads/gh-readonly-queue/.+/pr-([0-9]+)-[0-9a-f]{40}$'
  while read -r line; do
    [[ $line =~ $queue_ref ]] || continue
    tip=${BASH_REMATCH[1]}
    pull=${BASH_REMATCH[2]}
    jq -e --argjson number "$pull" 'any(.[]; .number == $number and .queued)' <<<"$pulls" > /dev/null ||
      continue
    git -C "$repo" cat-file -e "$tip^{commit}" 2> /dev/null || continue
    git -C "$repo" merge-base --is-ancestor "$tip" "$base" || continue
    if [[ $(git -C "$repo" log -1 --format=%s "$tip") == *"(#$pull)" ]]; then
      ahead+=("$pull")
    fi
  done <<<"$queue"
fi

# The change's own renames, so that a conflict at a path the change renamed is read against the
# same file's conflict in its base's merge.
declare -A renamed_from=()
renames_read=''
read_renames() {
  local fields=() i
  git -C "$repo" diff --find-renames --name-status -z "$base" "$head" > "$work/renames" ||
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
  if [[ " ${ahead[*]} " == *" $pull "* ]]; then
    echo "$name stands in a group in front of $change, so it lands first."
    continue
  fi
  checked=$((checked + 1))
  started=$SECONDS
  git -C "$repo" update-ref refs/heads/priority "$pull_head"
  # The change merged with the priority pull request.
  git -C "$repo" update-ref refs/heads/change "$head"
  status=0
  merge "$work/ours" change || status=$?
  case $status in
    0)
      echo "$name: no conflict with $change ($((SECONDS - started)) s)."
      continue
      ;;
    1) ;;
    *) fail "git could not merge $change with $name, $unread" ;;
  esac
  # The change's base merged with it: each conflict of that merge is the priority pull request's
  # own with the base, and a conflict of the change's merge is the change's unless it is one of
  # those.
  git -C "$repo" update-ref refs/heads/change "$base"
  status=0
  merge "$work/before" change || status=$?
  [ "$status" -le 1 ] || fail "git could not merge the base $base of $change with $name, $unread"
  : > "$work/caused"
  if [ "$status" -eq 0 ]; then
    # The base merges cleanly with it, so every conflict of the change's merge is the change's.
    cat "$work/ours.paths" "$work/ours.contents" >> "$work/caused"
    key_paths < "$work/ours.keys" >> "$work/caused"
  else
    echo "$name already conflicts with the base of $change in $(tr -cd '\0' < "$work/before.paths" | wc -c) file(s): those are its own to resolve, and count against $change only where $change adds a conflict."
    [ -n "$renames_read" ] || read_renames
    git -C "$repo" diff --no-renames --name-only -z "$(cat "$work/before.tree")" "$(cat "$work/ours.tree")" |
      sort -z -u > "$work/touched" || fail "git could not compare the two merges of $name, $unread"
    # Each conflicted path of the change's merge, against the same file in the base's merge: the
    # file it was, when the change renamed it, else the same path.
    while IFS= read -r -d '' path; do
      from=${renamed_from[$path]:-$path}
      # A path whose merged file is the same in both merges holds the same conflicts.
      if [ "$from" = "$path" ] && ! grep -qzxF -- "$path" "$work/touched"; then
        continue
      fi
      # Conflict markers in the file (a content or an add/add conflict) are read conflict by
      # conflict; a conflict with none, by its stages.
      hunks "$(cat "$work/ours.tree")" "$path" | sort > "$work/hunks" ||
        fail "git could not read $path of the merge of $change with $name, $unread"
      if [ -s "$work/hunks" ]; then
        # The change's unless the same file of the base's merge holds each of them as often,
        # with the same lines around it.
        if grep -qzxF -- "$from" "$work/before.paths"; then
          hunks "$(cat "$work/before.tree")" "$from" | sort > "$work/known" ||
            fail "git could not read $from of the merge of the base of $change with $name, $unread"
        else
          : > "$work/known"
        fi
        [ -z "$(comm -23 "$work/hunks" "$work/known")" ] || printf '%s\0' "$path" >> "$work/caused"
      elif [ "$(entries_of "$work/ours.entries" "$path")" != "$(entries_of "$work/before.entries" "$from")" ]; then
        # Any other conflict (a binary file's, modify/delete, file/directory): the change's unless
        # the same file of the base's merge has the same stages, each with the same mode and blob.
        printf '%s\0' "$path" >> "$work/caused"
      fi
    done < "$work/ours.paths"
    # A conflict git lists no stage for, by its type and its paths, as often as git writes it.
    comm -z -23 "$work/ours.keys" "$work/before.keys" | key_paths |
      sort -z -u | comm -z -23 - "$work/ours.paths" >> "$work/caused"
  fi
  sort -z -u -o "$work/caused" "$work/caused"
  if [ "$status" -eq 1 ] && ! [ -s "$work/caused" ]; then
    echo "$name: no conflict with $change ($((SECONDS - started)) s)."
    continue
  fi
  conflicts=$((conflicts + 1))
  echo "$name: a conflict with $change ($((SECONDS - started)) s)."
  if [ -s "$work/caused" ]; then
    where="in $(names "$work/caused")"
  else
    where="in a conflict git names no file for"
  fi
  error "$change conflicts with priority pull request $name $where. A priority pull request merges first: wait for #$pull to merge, then $remedy."
done

[ "$conflicts" -eq 0 ] || exit 1
echo "$change lands no conflict on an approved or queued priority pull request: $checked checked."
