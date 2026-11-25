#!/usr/bin/env bash
# Prints the daemon API contract entries a commit holds, in main's merge order: the order a
# stamped build of that commit numbers them in, the first L + 1 and the k-th L + k, L being
# api.LastNumberedContract (packages/daemon/internal/api/contracts.go). An entry is a file
# packages/daemon/internal/api/contract_<id>_<slug>.go, <id> 14 digits and <slug>
# [a-z0-9]+(-[a-z0-9]+)*; an entry's `_test.go` rows are no entry.
#
# usage: scripts/daemon-contract-order.sh [--stamp] [--main <ref>] <rev>
#   --stamp       print the list as one line in the stamp form a build links in as
#                 github.com/sjawhar/legion/daemon/internal/api.contractOrder: `v1:`, then
#                 the ids comma-separated (`v1:` alone for a commit of no entry)
#   --main <ref>  the ref whose first-parent line is main (default refs/remotes/origin/main)
#   <rev>         the commit whose entries it prints, one id a line
#   exit: 0 printed, 1 the clone cannot answer (it names why), 2 a usage error
#
# The order (dispatch://INFRA-1569/spec, "How a build reads the merge order"):
#   1. B is the newest commit of main's first-parent line that <rev> holds. The entries main's
#      first-parent history adds up to B come first, in the order that history adds them, and
#      the entries of one commit in id order.
#   2. Then the entries <rev> holds and B does not. Where <rev>'s first-parent line past B holds
#      no merge commit, as a merge group's line does, they come in the order that line adds
#      them: main takes a merge group's squash commits as the group built them. Otherwise, as on
#      a branch that forward-merges main, they come in id order.
# Production runs builds of main alone, and of two builds of main the older is an ancestor of
# the newer, so the older build's entries are the first ones of the newer's list and keep their
# numbers in it. An unstamped build numbers its entries by id instead, and no production build is
# unstamped.
#
# It reads commits and trees alone, never a blob, so a blobless clone (--filter=blob:none) is
# enough. It reads the history of <rev> and of main: main's alone misses a merge group's queued
# entries and a pull request's own commits. Where a shallow clone cuts the history of <rev>, it
# fails naming the commit it lacks, so a shallow clone never gives a short list.
#
# Run from anywhere in the clone: bash scripts/daemon-contract-order.sh --stamp HEAD
# CI runs it in release.yaml's legion_build, worker-image.yaml's build and
# ci-job-daemon-go.yaml's daemon-go jobs; scripts/daemon-contract-order.test.sh tests it.
set -euo pipefail
export LC_ALL=C

readonly name=daemon-contract-order.sh
readonly api=packages/daemon/internal/api
readonly entry="^${api}/contract_([0-9]{14})_[a-z0-9]+(-[a-z0-9]+)*[.]go\$"

usage() {
  sed -n '/^# usage: /,/^#   exit: /p' "$0" | sed 's/^# \{0,1\}//' >&2
  exit 2
}

die() {
  echo "${name}: $*" >&2
  exit 1
}

stamp=false
main=refs/remotes/origin/main
rev=''
while [[ $# -gt 0 ]]; do
  case $1 in
    --stamp) stamp=true ;;
    --main)
      [[ $# -ge 2 ]] || usage
      main=$2
      shift
      ;;
    -*) usage ;;
    *)
      [[ -z $rev ]] || usage
      rev=$1
      ;;
  esac
  shift
done
[[ -n $rev ]] || usage

# The paths below are the repository's, from the top of its working tree, while ls-tree and
# diff-tree take a pathspec, and ls-tree prints names, relative to the directory git runs in. So
# the script reads from the top wherever it is started: ci-job-daemon-go.yaml starts it in
# packages/daemon, where those reads would find no entry and print `v1:` alone.
top=$(git rev-parse --show-toplevel) || die "not in a git checkout"
cd "$top"

head=$(git rev-parse --verify --quiet "${rev}^{commit}") || die "${rev} names no commit of this clone"
tip=$(git rev-parse --verify --quiet "${main}^{commit}") ||
  die "${main} names no commit of this clone: fetch main (git fetch --filter=blob:none origin refs/heads/main:refs/remotes/origin/main), or name main's ref with --main"

work=$(mktemp -d "${TMPDIR:-/tmp}/daemon-contract-order.XXXXXX")
trap 'rm -rf -- "$work"' EXIT

# Every commit <rev> holds, and main's first-parent line, newest first.
git rev-list "$head" >"$work/held"
git rev-list --first-parent "$tip" >"$work/main"

# A shallow clone's history ends at the commits its shallow file lists, each of which names in
# its own object the parents the clone does not hold. Every commit <rev> holds is read below,
# directly or as an ancestor of B, so one such cut among them makes the list short.
shallow=$(git rev-parse --git-path shallow)
if [[ -s $shallow ]]; then
  cuts=$(grep -Fx -f "$shallow" "$work/held") || [[ $? == 1 ]]
  for cut in $cuts; do
    git cat-file commit "$cut" >"$work/cut"
    parent=$(sed -n '/^$/q; s/^parent //p' "$work/cut" | sed -n 1p)
    [[ -z $parent ]] ||
      die "this clone lacks commit ${parent}, the parent of ${cut}, which ${rev} holds: it is shallow there. Fetch the history of ${rev} and of main (git fetch --filter=blob:none --unshallow origin ${head} refs/heads/main:refs/remotes/origin/main)"
  done
fi

base=$(grep -m1 -Fx -f "$work/held" "$work/main") ||
  die "${rev} holds no commit of ${main}'s first-parent line"

# entries <commit>: the ids of the entries the commit holds, in id order, one a line; two
# entries of one id are refused, since a build could not tell which a stamp's id names.
entries() {
  git ls-tree --name-only "$1" "$api/" | sed -En "s#${entry}#\\1#p" | sort >"$work/ids"
  local twice
  twice=$(uniq -d "$work/ids")
  [[ -z $twice ]] || die "$1 holds two entries of the id $(printf '%s' "$twice" | sed -n 1p)"
  cat "$work/ids"
}

# added: reads `<commit> <parent>` lines, oldest first, and prints the id of each entry each
# commit adds against that parent, a commit's in id order. A line of a commit alone is a root,
# read against the empty tree. diff-tree heads each commit's names with its id, which awk turns
# into the commit's rank.
added() {
  git diff-tree --stdin --root -r --no-renames --name-only --diff-filter=A -- "$api/" |
    awk '/^[0-9a-f]+$/ { commit++; next } { print commit, $0 }' |
    sed -En "s#^([0-9]+) ${entry#^}#\\1 \\2#p" | sort -s -k1,1n -k2,2 | cut -d' ' -f2
}

# rank <set>: the ids of the file <set> (in id order) in the order standard input first names
# them, then the ones it never names, in id order.
rank() {
  awk '
    FILENAME == ARGV[1] { order[++n] = $1; wanted[$1] = 1; next }
    ($1 in wanted) && !($1 in done) { done[$1] = 1; print $1 }
    END { for (i = 1; i <= n; i++) if (!(order[i] in done)) print order[i] }
  ' "$1" -
}

entries "$head" >"$work/at-rev"
entries "$base" >"$work/at-base"
comm -12 "$work/at-base" "$work/at-rev" >"$work/first"
comm -13 "$work/at-base" "$work/at-rev" >"$work/then"

{
  # 1. Main's first-parent history up to B, each commit read against its first parent, and the
  # root alone.
  git rev-list --first-parent --reverse --parents "$base" | awk '{ print (NF > 1 ? $1 " " $2 : $1) }' |
    added | rank "$work/first"
  # 2. <rev>'s first-parent line past B: in the order it adds them where it holds no merge
  # commit, else none named, so rank prints them in id order.
  git rev-list --first-parent --reverse --parents "$head" "^$base" >"$work/line"
  if awk 'NF > 2 { found = 1 } END { exit !found }' "$work/line"; then
    rank "$work/then" </dev/null
  else
    added <"$work/line" | rank "$work/then"
  fi
} >"$work/order"

if [[ $stamp == true ]]; then
  printf 'v1:%s\n' "$(paste -sd, "$work/order")"
else
  cat "$work/order"
fi
