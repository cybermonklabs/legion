#!/usr/bin/env bash
# Tests for `edited-migrations-base.sh`: the base envoy-go-checks' edited-migrations check
# compares a run's tree with, for each event the job runs on.
#
# Each case clones a fixture origin the way actions/checkout leaves a run's tree, one commit deep
# at the commit the run tests (over file://, so git honours --depth), and runs the script there
# with the event, sha, ref and migration-base Actions would hand it. The fixture's history has
# the real shapes: a main that is one line of commits, as the queue lands them; a branch forked
# from it 100 commits below both tips, past the script's first fetch depth; a branch that merged
# main after it forked; a merge-group entry on top of main's tip; and a history that shares no
# commit with main. One case breaks the checkout's object store once the fetch has written main's
# ref, so git merge-base fails with 128 rather than answers.
#
# Run from anywhere: .github/scripts/edited-migrations-base.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

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

# Main: 200 commits in one line. The feature branch forks at its 100th and adds 100 of its own,
# so its fork point is 100 commits below both tips.
line 50 'main, early'
early_mark=$mark
line 50 'main, up to the fork' "$early_mark"
fork_mark=$mark
line 50 'main, after the fork' "$fork_mark"
later_mark=$mark
line 50 'main, late' "$later_mark"
main_tip_mark=$mark
line 100 'feature' "$fork_mark"
feature_mark=$mark
# A branch forked at main's 50th commit that merged main's 150th, then went on.
line 3 'merged branch, before the merge' "$early_mark"
commit 'Merge main into the branch' "$mark" "$later_mark"
line 2 'merged branch, after the merge' "$mark"
merged_mark=$mark
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
main_tip=${shas[$main_tip_mark]}
main_parent=$(git_fixture -C "$origin" rev-parse "$main_tip^1")
feature=${shas[$feature_mark]}
merged=${shas[$merged_mark]}
entry=${shas[$entry_mark]}
queue_ref="gh-readonly-queue/main/pr-7-$feature"
unrelated=${shas[$unrelated_mark]}
{
  printf 'update refs/heads/%s %s\n' main "$main_tip" feature "$feature" merged "$merged" \
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

echo "fixture: the feature's fork point is past the script's first fetch, 64 commits deep"
check "100 commits of main are above the fork point" \
  "$(is "$(git_fixture -C "$origin" rev-list --count "$fork..$main_tip")" 100)"
check "100 commits of the feature are above it" \
  "$(is "$(git_fixture -C "$origin" rev-list --count "$fork..$feature")" 100)"

echo "case: a workflow_dispatch from a branch that names no migration-base compares with the branch's merge base with main"
dir=$(checkout feature)
pick "$dir" workflow_dispatch refs/heads/feature "$feature" ''
check "it answers" "$(is "$status" 0)"
check "the base is the fork point, the newest commit of main the branch holds" "$(is "$out" "$fork")"
check "it names the base and why" \
  "$(contains "$err" "^edited-migrations base: $fork, the merge base of HEAD $feature with main $main_tip: this workflow_dispatch run on refs/heads/feature names no migration-base, and a branch lands onto main\$")"
check "it raises no error" "$(is "$(contains "$err" '::error::')" false)"
check "the base is in the checkout" "$(holds "$dir" "$fork")"

echo "case: a branch that merged main after it forked compares with the commit of main it merged, not its fork point"
dir=$(checkout merged)
pick "$dir" workflow_dispatch refs/heads/merged "$merged" ''
check "it answers" "$(is "$status" 0)"
check "the base is the merged commit of main" "$(is "$out" "$later")"
check "it names the base and why" \
  "$(contains "$err" "^edited-migrations base: $later, the merge base of HEAD $merged with main $main_tip: ")"
check "the base is in the checkout" "$(holds "$dir" "$later")"

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
# with exit 128 and `fatal: replacement <id> not found for <tip>`, as a missing or unreadable
# object would.
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
check "it prints git's own message" \
  "$(contains "$err" "^fatal: replacement 1111111111111111111111111111111111111111 not found for $main_tip\$")"
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

summary "the base the edited-migrations check compares each run's tree with"
