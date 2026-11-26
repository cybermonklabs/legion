#!/usr/bin/env bash
# Tests for `edited-migrations-base.sh`: the base envoy-go-build's edited-migrations check
# compares a run's tree with, for each event the job runs on.
#
# Each case clones a fixture origin the way actions/checkout leaves a run's tree, one commit deep
# at the commit the run tests (over file://, so git honours --depth), and runs the script there
# with the event, sha, ref and migration-base Actions would hand it. The fixture's history has
# the real shapes: a main that is one line of commits, as the queue lands them; a branch forked
# from it 100 commits below both tips, past the script's first fetch depth; a branch that merged
# main after it forked; branches that hold a newer commit of main only along a path longer than
# the second fetch, or than the third, beside a short path to an older one; a merge-group entry on
# top of main's tip; and a history that shares no commit with main. One case breaks the checkout's
# object store once the fetch has written main's ref, so git merge-base fails with 128 rather than
# answers. The last cases run the script in a whole clone, as a developer's clone or worktree is,
# where a fetch with a depth would make the clone shallow: each finds the same base and leaves the
# clone whole.
#
# Run from anywhere: .github/scripts/edited-migrations-base.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

# The fixtures are judged on git's defaults wherever the harness runs, so every git variable the
# environment sets goes before any git runs. GIT_CONFIG_GLOBAL and GIT_CONFIG_NOSYSTEM, below, keep
# out only the configuration files, not configuration in the environment: a Legion pod sets
# core.hooksPath=/dev/null through GIT_CONFIG_COUNT, GIT_CONFIG_KEY_0 and GIT_CONFIG_VALUE_0, so
# the failing merge-base row's hook never ran there. GIT_CONFIG_PARAMETERS (what `git -c` hands
# on), GIT_NO_REPLACE_OBJECTS or GIT_DIR would change a case too.
while read -r name; do
  unset "$name"
done < <(compgen -e GIT_)

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
script="$script_dir/edited-migrations-base.sh"

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# git_fixture <args>: git with no configuration of the machine or of the user it runs as, the way
# each case runs the script too, and an identity for the fixture's commits.
git_fixture() {
  GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 git -c user.name='CI test' \
    -c user.email=ci-test@example.com "$@"
}

# The fixture's history, written by one git fast-import straight into the origin: each commit
# holds the same migration file, and a commit's mark is its number in the stream.
origin="$work/origin.git"
git_fixture init -q --bare "$origin"
# GitHub serves any commit reachable from a ref by its sha; a bare repository serves only its
# tips unless told otherwise.
git_fixture -C "$origin" config uploadpack.allowReachableSHA1InWant true
stream="$work/history.fi"
printf 'blob\nmark :1\ndata <<EOF\nCREATE TABLE t (id int);\nEOF\n\n' > "$stream"
mark=1

# commit <message> [<parent mark> [<merged mark>]]: appends a commit to the stream, a root when
# no parent is given; sets mark to its mark.
commit() {
  mark=$((mark + 1))
  {
    if [ -z "${2:-}" ]; then
      printf 'reset refs/fixture/commits\n\n'
    fi
    printf 'commit refs/fixture/commits\nmark :%d\ncommitter CI test <ci-test@example.com> %d +0000\ndata <<EOF\n%s\nEOF\n' \
      "$mark" "$((1700000000 + mark))" "$1"
    if [ -n "${2:-}" ]; then
      printf 'from :%d\n' "$2"
    fi
    if [ -n "${3:-}" ]; then
      printf 'merge :%d\n' "$3"
    fi
    printf 'M 100644 :1 0001_init.sql\n\n'
  } >> "$stream"
}

# line <count> <label> [<parent mark>]: <count> commits in a line on <parent mark>, or on a new
# root; sets mark to the last one's mark.
line() {
  local count=$1 label=$2 parent=${3:-} i
  for ((i = 1; i <= count; i++)); do
    commit "$label $i" "$parent"
    parent=$mark
  done
}

# Main: 1000 commits in one line below every branch, then 200 more, which the comments below count
# from 1. The 1000 keep main's root past the third fetch the script makes, 1024 commits deep, so
# a case that settles by then settles in a checkout that is still shallow. The feature branch
# forks at main's 100th and adds 100 of its own, so its fork point is 100 commits below both tips.
line 1000 'main, below the branches'
line 50 'main, early' "$mark"
early_mark=$mark
line 50 'main, up to the fork' "$early_mark"
fork_mark=$mark
line 50 'main, after the fork' "$fork_mark"
later_mark=$mark
line 25 'main, late' "$later_mark"
newer_mark=$mark
line 25 'main, latest' "$newer_mark"
main_tip_mark=$mark
line 100 'feature' "$fork_mark"
feature_mark=$mark
# A branch forked at main's 50th commit that merged main's 150th, then went on.
line 3 'merged branch, before the merge' "$early_mark"
commit 'Merge main into the branch' "$mark" "$later_mark"
line 2 'merged branch, after the merge' "$mark"
merged_mark=$mark
# A branch that holds main's 175th commit only through 300 commits of its own, more than the
# script's second fetch, and that then merged a branch of one commit on main's 150th: two paths
# back to main, the short one to an older commit of main than the long one.
line 300 'two paths, the long one' "$newer_mark"
long_mark=$mark
commit 'two paths, the short one' "$later_mark"
short_mark=$mark
commit 'Merge the short path' "$long_mark" "$short_mark"
two_paths_mark=$mark
# The same two paths with the merge's parents the other way round: the short branch merged the
# long one.
commit 'Merge the long path' "$short_mark" "$long_mark"
mirror_mark=$mark
# The same short branch merged into a path of 1100 commits on main's 175th, more than the script's
# third fetch: the fetch that settles it holds the whole of both histories.
line 1100 'two paths, the longest one' "$newer_mark"
commit 'Merge the short path into the longest' "$mark" "$short_mark"
whole_mark=$mark
# The commit the queue builds for a group's first entry: the change, on main's tip.
commit 'merge group entry' "$main_tip_mark"
entry_mark=$mark
line 3 'unrelated root'
unrelated_mark=$mark

git_fixture -C "$origin" fast-import --quiet --export-marks="$work/marks" < "$stream"
shas=()
while read -r m s; do
  shas[${m#:}]=$s
done < "$work/marks"
early=${shas[$early_mark]}
fork=${shas[$fork_mark]}
later=${shas[$later_mark]}
newer=${shas[$newer_mark]}
main_tip=${shas[$main_tip_mark]}
main_parent=$(git_fixture -C "$origin" rev-parse "$main_tip^1")
feature=${shas[$feature_mark]}
merged=${shas[$merged_mark]}
two_paths=${shas[$two_paths_mark]}
mirror=${shas[$mirror_mark]}
whole=${shas[$whole_mark]}
entry=${shas[$entry_mark]}
queue_ref="gh-readonly-queue/main/pr-7-$feature"
unrelated=${shas[$unrelated_mark]}
{
  printf 'update refs/heads/%s %s\n' main "$main_tip" feature "$feature" merged "$merged" \
    two-paths "$two_paths" two-paths-mirror "$mirror" two-paths-whole "$whole" \
    "$queue_ref" "$entry" unrelated "$unrelated"
  printf 'delete refs/fixture/commits\n'
} > "$work/refs"
git_fixture -C "$origin" update-ref --stdin < "$work/refs"

# checkout <branch>: the tree actions/checkout leaves for a run on <branch>, one commit deep, in
# a directory of its own; prints the directory.
checkout() {
  local dir
  dir=$(mktemp -d "$work/checkout.XXXXXX")
  git_fixture clone -q --depth=1 --no-tags --single-branch --branch "$1" "file://$origin" "$dir"
  echo "$dir"
}

# pick <checkout> <event> <ref> <sha> <migration-base>: runs the script in <checkout> with those
# four inputs and no others (the runner of this test sets GITHUB_* of its own); sets out (stdout,
# the base), err (stderr) and status. Every case runs it under `timeout`, so a fetch loop that
# never ended would fail its case instead of hanging the job.
pick() {
  set +e
  out=$(cd "$1" && GITHUB_EVENT_NAME=$2 GITHUB_REF=$3 GITHUB_SHA=$4 MIGRATION_BASE=$5 \
    GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 timeout 120 "$script" 2> "$work/err")
  status=$?
  set -e
  err=$(cat "$work/err")
}

# holds <checkout> <sha>: true when the commit is in the checkout, for the check to read.
holds() { git -C "$1" cat-file -e "$2^{commit}" 2> /dev/null && echo true || echo false; }

# has_lines <text> <lines>: true/false for check; true when each line of <lines> is a whole line
# of <text>, compared as a fixed string.
has_lines() {
  local line
  while IFS= read -r line; do
    grep -qxF -- "$line" <<< "$1" || {
      echo false
      return
    }
  done <<< "$2"
  echo true
}

echo "fixture: the feature's fork point is past the script's first fetch, 64 commits deep"
check "100 commits of main are above the fork point" \
  "$(is "$(git_fixture -C "$origin" rev-list --count "$fork..$main_tip")" 100)"
check "100 commits of the feature are above it" \
  "$(is "$(git_fixture -C "$origin" rev-list --count "$fork..$feature")" 100)"

echo "fixture: the two-path branch holds the newer commit of main only past the second fetch, and an older one within the first"
check "302 commits of the branch are above the newer commit of main" \
  "$(is "$(git_fixture -C "$origin" rev-list --count "$newer..$two_paths")" 302)"
check "the short path reaches the older commit of main two commits below the branch's tip" \
  "$(is "$(git_fixture -C "$origin" rev-parse "$two_paths^2^")" "$later")"
check "50 commits of main are above the older commit, so main's first fetch holds it" \
  "$(is "$(git_fixture -C "$origin" rev-list --count "$later..$main_tip")" 50)"
check "the merge base of the whole histories is the newer commit" \
  "$(is "$(git_fixture -C "$origin" merge-base "$two_paths" "$main_tip")" "$newer")"

echo "case: a workflow_dispatch from a branch that names no migration-base compares with the branch's merge base with main"
dir=$(checkout feature)
pick "$dir" workflow_dispatch refs/heads/feature "$feature" ''
check "it answers" "$(is "$status" 0)"
check "the base is the fork point, the newest commit of main the branch holds" "$(is "$out" "$fork")"
check "it names the base and why" \
  "$(contains "$err" "^edited-migrations base: $fork, the merge base of HEAD $feature with main $main_tip: this workflow_dispatch run on refs/heads/feature names no migration-base, and a branch lands onto main\$")"
check "it raises no error" "$(is "$(contains "$err" '::error::')" false)"
check "the base is in the checkout" "$(holds "$dir" "$fork")"
check "it stops at the second fetch, 256 commits deep: main's 256th commit below its tip is not in the checkout" \
  "$(is "$(holds "$dir" "$(git_fixture -C "$origin" rev-parse "$main_tip~256")")" false)"

echo "case: a branch that merged main after it forked compares with the commit of main it merged, not its fork point"
dir=$(checkout merged)
pick "$dir" workflow_dispatch refs/heads/merged "$merged" ''
check "it answers" "$(is "$status" 0)"
check "the base is the merged commit of main" "$(is "$out" "$later")"
check "it names the base and why" \
  "$(contains "$err" "^edited-migrations base: $later, the merge base of HEAD $merged with main $main_tip: ")"
check "the base is in the checkout" "$(holds "$dir" "$later")"

echo "case: a branch that holds a newer commit of main only past the second fetch, beside a short path to an older one, compares with the newer one"
dir=$(checkout two-paths)
# The CI step runs the script from packages/envoy, a directory below the checkout's root.
mkdir -p "$dir/packages/envoy"
pick "$dir/packages/envoy" workflow_dispatch refs/heads/two-paths "$two_paths" ''
check "it answers" "$(is "$status" 0)"
check "the base is the merge base of the whole histories, the newer commit of main" "$(is "$out" "$newer")"
check "it names the base and why" \
  "$(contains "$err" "^edited-migrations base: $newer, the merge base of HEAD $two_paths with main $main_tip: ")"
check "the base is in the checkout" "$(holds "$dir" "$newer")"
check "it settles while the checkout is still shallow, short of main's root" \
  "$(is "$(git -C "$dir" rev-parse --is-shallow-repository)" true)"

echo "case: the same two paths, with the long one as the merge's second parent, compare with the newer commit too"
dir=$(checkout two-paths-mirror)
pick "$dir" workflow_dispatch refs/heads/two-paths-mirror "$mirror" ''
check "it answers" "$(is "$status" 0)"
check "the base is the newer commit of main" "$(is "$out" "$newer")"
check "it settles while the checkout is still shallow, short of main's root" \
  "$(is "$(git -C "$dir" rev-parse --is-shallow-repository)" true)"

echo "case: two paths whose long one is past the third fetch compare with the newer commit once the fetch holds both whole histories"
dir=$(checkout two-paths-whole)
pick "$dir" workflow_dispatch refs/heads/two-paths-whole "$whole" ''
check "it answers" "$(is "$status" 0)"
check "the base is the newer commit of main" "$(is "$out" "$newer")"
check "it settles once the checkout is whole" \
  "$(is "$(git -C "$dir" rev-parse --is-shallow-repository)" false)"

echo "case: a migration-base named on a branch's dispatch is the base, over the merge base"
dir=$(checkout feature)
pick "$dir" workflow_dispatch refs/heads/feature "$feature" "$early"
check "it answers" "$(is "$status" 0)"
check "the base is the commit named" "$(is "$out" "$early")"
check "it names the base and why" \
  "$(contains "$err" "^edited-migrations base: $early, the migration-base this workflow_dispatch run on refs/heads/feature names\$")"
check "the base is in the checkout" "$(holds "$dir" "$early")"

echo "case: a branch whose whole history shares no commit with main is refused, naming the input that would settle it"
dir=$(checkout unrelated)
pick "$dir" workflow_dispatch refs/heads/unrelated "$unrelated" ''
check "it exits 2, a check that could not be taken" "$(is "$status" 2)"
check "it names both commits and the input" \
  "$(contains "$err" "^::error::HEAD $unrelated and main $main_tip share no commit, so this workflow_dispatch run on refs/heads/unrelated has no base to compare its migrations with; name one with -f migration-base=<commit>\$")"
check "it prints no base" "$(is "$out" "")"
check "it does not call that a failure of git merge-base" \
  "$(is "$(contains "$err" 'git merge-base .* exited')" false)"

echo "case: a git merge-base that fails rather than answers is named, with git's message, and not fetched past"
dir=$(checkout feature)
# A reference-transaction hook: when the script's first fetch has written main's ref, it points
# a replace ref for main's tip at an object that does not exist. The fetch has already checked
# what it received and succeeds; every later read of main's tip, git merge-base's first, fails
# with exit 128 and a message that names the missing replacement, as a missing or unreadable
# object would. Git 2.43 words it `fatal: replacement <id> not found for <tip>`; other versions
# word it otherwise.
cat > "$dir/.git/hooks/reference-transaction" <<'HOOK'
#!/bin/sh
[ "$1" = committed ] || exit 0
while read -r _ new ref; do
  if [ "$ref" = refs/remotes/origin/main ]; then
    replace=$(git rev-parse --git-path refs/replace)
    mkdir -p "$replace"
    echo 1111111111111111111111111111111111111111 > "$replace/$new"
  fi
done
HOOK
chmod +x "$dir/.git/hooks/reference-transaction"
pick "$dir" workflow_dispatch refs/heads/feature "$feature" ''
check "it exits 2, a check that could not be taken" "$(is "$status" 2)"
check "it names git merge-base, both arguments and the code" \
  "$(contains "$err" "^::error::git merge-base HEAD $main_tip exited 128, not 0 (a base) or 1 (no base), so this workflow_dispatch run on refs/heads/feature could not tell its base; git's message is above\$")"
# git's message is what the git on this machine writes, so the check takes it from git merge-base
# itself, run in the checkout the script left, where the replace ref still stands, and looks for
# each of its lines in the script's stderr. With no message, or with git's 0 or 1, it would pass
# on nothing, so either fails it.
set +e
git_err=$(cd "$dir" && GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 \
  git merge-base HEAD "$main_tip" 2>&1 > /dev/null)
git_status=$?
set -e
passed_through=false
if [ -n "$git_err" ] && [ "$git_status" -ne 0 ] && [ "$git_status" -ne 1 ]; then
  passed_through=$(has_lines "$err" "$git_err")
fi
check "it prints git's own message, each line git merge-base writes in that checkout" "$passed_through"
check "it does not call the fault a history that shares no commit" "$(is "$(contains "$err" 'share no commit')" false)"
check "it prints no base" "$(is "$out" "")"
check "it fetches no deeper: HEAD's history is still the first fetch's 64 commits" \
  "$(is "$(git -C "$dir" rev-list --count HEAD)" 64)"

echo "control: a push to main compares with the first parent of the commit it checked out"
dir=$(checkout main)
pick "$dir" push refs/heads/main "$main_tip" ''
check "it answers" "$(is "$status" 0)"
check "the base is the tip's first parent" "$(is "$out" "$main_parent")"
check "it names the base and why" \
  "$(contains "$err" "^edited-migrations base: $main_parent, the first parent of $main_tip, the commit this push run checked out\$")"
check "the base is in the checkout" "$(holds "$dir" "$main_parent")"

echo "control: a merge-group entry compares with its first parent, main's tip"
dir=$(checkout "$queue_ref")
pick "$dir" merge_group "refs/heads/$queue_ref" "$entry" ''
check "it answers" "$(is "$status" 0)"
check "the base is main's tip" "$(is "$out" "$main_tip")"
check "it names the base and why" \
  "$(contains "$err" "^edited-migrations base: $main_tip, the first parent of $entry, the commit this merge_group run checked out\$")"

echo "control: a workflow_dispatch on main that names no migration-base still exits 2"
dir=$(checkout main)
pick "$dir" workflow_dispatch refs/heads/main "$main_tip" ''
check "it exits 2, a check that could not be taken" "$(is "$status" 2)"
check "it names the input main-bypass-check.yaml passes" \
  "$(contains "$err" '^::error::this workflow_dispatch run names no migration-base, so the check cannot tell which commits it lands; main-bypass-check.yaml passes the commit main pointed at before the push$')"
check "it prints no base" "$(is "$out" "")"
check "it fetches nothing: the checkout is still one commit deep" \
  "$(is "$(git -C "$dir" rev-list --count HEAD)" 1)"

echo "control: a workflow_dispatch on main that names a migration-base compares with it"
dir=$(checkout main)
pick "$dir" workflow_dispatch refs/heads/main "$main_tip" "$early"
check "it answers" "$(is "$status" 0)"
check "the base is the commit named" "$(is "$out" "$early")"
check "it names the base and why" \
  "$(contains "$err" "^edited-migrations base: $early, the migration-base this workflow_dispatch run on refs/heads/main names\$")"
check "the base is in the checkout" "$(holds "$dir" "$early")"

# whole_clone <branch>: a whole clone of <branch>, its every commit with its parents, as a
# developer's clone, and each worktree that shares it, holds the history; prints the directory.
whole_clone() {
  local dir
  dir=$(mktemp -d "$work/whole.XXXXXX")
  git_fixture clone -q --no-tags --single-branch --branch "$1" "file://$origin" "$dir"
  echo "$dir"
}

# stays_whole <clone> <commit>: true when the clone is not shallow and the commit's whole history,
# as the origin counts it, still reads in the clone, for the check to read.
stays_whole() {
  [ "$(git -C "$1" rev-parse --is-shallow-repository)" = false ] &&
    [ "$(git -C "$1" rev-list --count "$2")" = "$(git_fixture -C "$origin" rev-list --count "$2")" ] &&
    echo true || echo false
}

echo "case: in a whole clone, a push compares with the tip's first parent and the clone stays whole"
dir=$(whole_clone main)
pick "$dir" push refs/heads/main "$main_tip" ''
check "it answers" "$(is "$status" 0)"
check "the base is the tip's first parent" "$(is "$out" "$main_parent")"
check "the clone is not shallow, and main's 1200 commits all still read" "$(stays_whole "$dir" "$main_tip")"

echo "case: in a whole clone, a migration-base named on a branch's dispatch is the base and the clone stays whole"
dir=$(whole_clone feature)
pick "$dir" workflow_dispatch refs/heads/feature "$feature" "$early"
check "it answers" "$(is "$status" 0)"
check "the base is the commit named" "$(is "$out" "$early")"
check "the clone is not shallow, and the branch's whole history still reads" "$(stays_whole "$dir" "$feature")"

# The two-path branch, whose base a shallow checkout settles at the third fetch while still
# shallow: a fetch with a depth would leave the whole clone shallow too, as a real main, far longer
# than any of the fetches, leaves it.
echo "case: in a whole clone, a branch's dispatch compares with the merge base of the whole histories and the clone stays whole"
dir=$(whole_clone two-paths)
pick "$dir" workflow_dispatch refs/heads/two-paths "$two_paths" ''
check "it answers" "$(is "$status" 0)"
check "the base is the newer commit of main" "$(is "$out" "$newer")"
check "it names the base and why" \
  "$(contains "$err" "^edited-migrations base: $newer, the merge base of HEAD $two_paths with main $main_tip: ")"
check "main is fetched whole: its 1200 commits all read" "$(stays_whole "$dir" "$main_tip")"
check "the clone is not shallow, and the branch's whole history still reads" "$(stays_whole "$dir" "$two_paths")"

summary "the base the edited-migrations check compares each run's tree with"
