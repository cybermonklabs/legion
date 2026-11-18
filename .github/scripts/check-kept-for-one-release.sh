#!/usr/bin/env bash
# dispatch://INFRA-1150 keeps some things for one release, for the root's own pull request to
# delete: packages/dispatch/e2e/acceptance/root-deletes.txt lists them, one line each, its id, a
# tab, the thing, a tab, and what holds it. Every place in the repository that serves a kept thing
# carries one marker comment, the words "kept for one release", a colon, a space and the id, with
# the id the last thing on its line (blanks and a carriage return after it aside). A sentence that
# only begins with those words, one that goes on after the id, is no marker. This check fails on a
# marker whose id the list does not name, which is a kept thing nobody would delete, and on a
# listed id no marker names, which is a thing the root's pull request would look for and not find.
# It also refuses a list line of another shape, an id of another shape, and an id listed twice.
#
# It scans every file git tracks, a file under an ignored path included, plus every untracked file
# git does not ignore, so a new file is caught before it is added. Binary files are skipped, and so
# is the list itself. Every finding is printed as a GitHub annotation naming the file and line; the
# only non-zero exits are findings (1) and a list or a search that cannot be read (2).
#
# Run from anywhere: .github/scripts/check-kept-for-one-release.sh
# CI runs it over the repository in ci.yaml's lint job, beside check-plugin-names.sh, and its tests
# (check-kept-for-one-release.test.sh) in ci.yaml's test job.
set -euo pipefail

cd "$(dirname "$0")/../.."

list=packages/dispatch/e2e/acceptance/root-deletes.txt
# The marker's words, in two pieces so this file holds no marker.
words='kept for one'
words+=' release'
id_shape='[a-z0-9][a-z0-9-]*'

[ -r "$list" ] || {
  echo "check-kept-for-one-release: $list cannot be read" >&2
  exit 2
}

matches=$(mktemp)
findings=$(mktemp)
trap 'rm -f "$matches" "$findings"' EXIT

# run <command...>: appends the command's output to $matches; a failed search (exit 2 or more,
# where grep's 1 means no match) stops the check.
run() {
  local status=0
  "$@" >> "$matches" || status=$?
  if [ "$status" -gt 1 ]; then
    echo "check-kept-for-one-release: the search with $1 failed (exit $status)" >&2
    exit 2
  fi
}

# The list's ids, in its order.
declare -A listed=()
listed_ids=()
number=0
tab=$'\t'
while IFS= read -r line || [ -n "$line" ]; do
  number=$((number + 1))
  trimmed=${line//[[:space:]]/}
  if [ -z "$trimmed" ] || [[ $line == "#"* ]]; then
    continue
  fi
  tabs=${line//[^$tab]/}
  id=${line%%"$tab"*}
  rest=${line#*"$tab"}
  thing=${rest%%"$tab"*}
  holds=${rest#*"$tab"}
  if [ "${#tabs}" -ne 2 ] || [ -z "${thing//[[:space:]]/}" ] || [ -z "${holds//[[:space:]]/}" ]; then
    printf '%s:%s: is not an id, the thing and what holds it, each after a tab\n' "$list" "$number" >> "$findings"
    continue
  fi
  if ! [[ $id =~ ^${id_shape}$ ]]; then
    printf '%s:%s: names the id "%s", which is no id\n' "$list" "$number" "$id" >> "$findings"
    continue
  fi
  if [ -n "${listed[$id]:-}" ]; then
    printf '%s:%s: lists "%s" a second time\n' "$list" "$number" "$id" >> "$findings"
    continue
  fi
  listed[$id]=$number
  listed_ids+=("$id")
done < "$list"
if [ "${#listed_ids[@]}" -eq 0 ]; then
  printf '%s:1: lists no kept thing\n' "$list" >> "$findings"
fi

# The markers. --untracked adds the files git has not been told about, but skips a tracked file
# under an ignored path, which the plain pass reads; a tracked file appears in both, so the rows
# are deduplicated below. core.quotePath=false prints a non-ASCII path as it is, so the annotation
# names a real file.
marker_regex="$words: ($id_shape)[[:blank:]"$'\r'"]*\$"
run git -c core.quotePath=false grep -n -I -E --untracked -e "$marker_regex" -- . ":(exclude)$list"
run git -c core.quotePath=false grep -n -I -E -e "$marker_regex" -- . ":(exclude)$list"

declare -A marked=()
markers=0
while IFS= read -r hit; do
  path=${hit%%:*}
  rest=${hit#*:}
  line=${rest%%:*}
  text=${rest#*:}
  [[ $text =~ $marker_regex ]] || continue
  id=${BASH_REMATCH[1]}
  markers=$((markers + 1))
  marked[$id]=1
  if [ -z "${listed[$id]:-}" ]; then
    printf '%s:%s: a marker names the id "%s", which %s does not list\n' "$path" "$line" "$id" "$list" >> "$findings"
  fi
done < <(sort -u "$matches")

for id in "${listed_ids[@]}"; do
  if [ -z "${marked[$id]:-}" ]; then
    printf '%s:%s: lists "%s", and no marker in the repository names it\n' "$list" "${listed[$id]}" "$id" >> "$findings"
  fi
done

if [ -s "$findings" ]; then
  while IFS= read -r finding; do
    path=${finding%%:*}
    rest=${finding#*:}
    echo "::error file=$path,line=${rest%%:*}::$finding"
  done < "$findings"
  echo "check-kept-for-one-release: $(wc -l < "$findings") finding(s). Each thing kept for one release is a line of $list and"
  echo "  is marked, in every place that serves it, by a comment ending in the words, a colon and its id."
  echo "  List the id of a new marker, or drop a list line no place serves any more."
  exit 1
fi

echo "check-kept-for-one-release: each of the ${#listed_ids[@]} ids $list lists is named by a marker, and each of the $markers markers names a listed id"
