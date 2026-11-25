#!/usr/bin/env bash
# Tests for `check-fragment-contracts.sh`: a fragment that declares its own number with the words
# `daemon API contract <n>`, in any case and across a line break, passes where the commit of main
# that added it gave one of its entries that number, and is refused where that commit gave its
# entries another number, or where the number is above L; a fragment whose change has no number
# (its commit gave no entry one, or main does not hold it yet) has its words listed, as every other
# contract number a fragment names is; and a clone the check cannot read history from is refused
# by name.
#
# The cases share one miniature upstream, whose main adds, commit by commit, numbered entries to a
# version.go and fragments beside them, and read it from a clone made without blobs, as the
# check's own run is, with the real script linked in and left untracked. A case changes the
# clone's working tree, runs the check, and puts the tree back.
#
# The numbers here are the fixtures' own (L is 2): no contract number of this repository is
# written into this file.
#
# Run from anywhere: .github/scripts/check-fragment-contracts.test.sh
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check_script="$script_dir/check-fragment-contracts.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=fragment-test GIT_AUTHOR_EMAIL=fragment-test@example.invalid
export GIT_COMMITTER_NAME=fragment-test GIT_COMMITTER_EMAIL=fragment-test@example.invalid

version=packages/daemon/internal/api/version.go
released=packages/contracts/fixtures/daemon-api/released-contracts.json
changes=packages/pi-legion/.changes/unreleased

# put <root> <path> <text>: writes <text> and a newline to <path> under <root>.
put() {
  mkdir -p "$(dirname "$1/$2")"
  printf '%s\n' "$3" > "$1/$2"
}

upstream=$work/upstream
git -c init.defaultBranch=main init -q "$upstream"
git -C "$upstream" config uploadpack.allowFilter true
git -C "$upstream" config uploadpack.allowAnySHA1InWant true
step() {
  git -C "$upstream" add -A
  git -C "$upstream" commit -q -m "$1"
}
# Entry 1, and its fragment, which also names another contract as context.
put "$upstream" "$version" $'package api\n\n// 1: the first numbered entry.'
put "$upstream" "$released" '[1, 2]'
put "$upstream" "$changes/one.yaml" \
  $'kind: Added\nbody: The plugin declares daemon API contract 1; a caller at contract 0 reads it as before.'
step "entry 1"
# Entry 2, with two fragments: one that declares it in capitals across a line break, and one that
# names no number.
put "$upstream" "$version" $'package api\n\n// 1: the first numbered entry.\n//\n// 2: the second numbered entry.'
put "$upstream" "$changes/two.yaml" $'kind: Changed\nbody: |-\n  Declares DAEMON api\n  Contract 2.'
put "$upstream" packages/daemon/.changes/unreleased/two.yaml $'kind: Changed\nbody: The daemon serves the new key.'
step "entry 2"
# A change that gives no entry a number, whose fragment names another change's contract with the
# words that declare one.
put "$upstream" "$changes/numberless.yaml" \
  $'kind: Removed\nbody: The notice that daemon API contract 1 declared never shipped, and is gone.'
step "a change of no contract"
# A change that rewrites entry 1's header and adds no entry: it gave no number either.
put "$upstream" "$version" $'package api\n\n// 1: the first numbered entry, reworded.\n//\n// 2: the second numbered entry.'
put "$upstream" "$changes/reworded.yaml" $'kind: Changed\nbody: Entry 1 of daemon API contract 1 is reworded.'
step "a header reworded"

clone=$work/clone
git clone -q --filter=blob:none "file://$upstream" "$clone"
mkdir -p "$clone/.github/scripts"
ln -s "$check_script" "$clone/.github/scripts/check-fragment-contracts.sh"

# run_check [<argument>...]: runs the real script in the clone. Sets out (stdout), err (stderr)
# and status.
run_check() {
  set +e
  out=$("$clone/.github/scripts/check-fragment-contracts.sh" "$@" 2> "$work/stderr")
  status=$?
  set -e
  err=$(cat "$work/stderr")
}

# restore: puts the clone's working tree back as main has it.
restore() {
  git -C "$clone" checkout -q -- .
  git -C "$clone" clean -q -fd -e .github/
}

echo "case: main's own fragments"
check "the clone is made without blobs, so the check fetches the blobs it reads" \
  "$(is "$(git -C "$clone" config remote.origin.partialclonefilter)" blob:none)"
run_check
check "exits 0" "$(is "$status" 0)"
check "refuses nothing" "$(is "$(contains "$err" '^::error')" false)"
check "counts the two declarations its commits gave" \
  "$(contains "$err" '^5 fragments read, L = 2: 2 declarations of a fragment.s own number are each one its commit gave its entries, 0 refused; 3 other contract numbers listed above for the reviewer$')"
check "lists the context number beside a declaration, with its file, line and words" \
  "$(contains "$out" "^$changes/one.yaml:2: contract 0: .*a caller at contract 0 reads it as before.")"
check "lists the words of a change that gave no number" \
  "$(contains "$out" "^$changes/numberless.yaml:2: contract 1: .*The notice that daemon API contract 1 declared never shipped")"
check "lists the words of a change whose diff only rewrote a header" \
  "$(contains "$out" "^$changes/reworded.yaml:2: contract 1: ")"
check "lists no declaration its commit gave" "$(is "$(contains "$out" 'two.yaml\|one.yaml:2: contract 1:')" false)"
check "prints only listed lines on stdout" "$(is "$(grep -c . <<<"$out")" 3)"

echo "case: a fragment that declares a number its commit did not give"
put "$clone" "$changes/two.yaml" $'kind: Changed\nbody: |-\n  Declares daemon API contract 1.'
run_check
check "exits 1" "$(is "$status" 1)"
check "names the file, the line, the commit and the numbers it gave" \
  "$(contains "$err" "^::error file=$changes/two.yaml::$changes/two.yaml:3: declares daemon API contract 1, and the commit of main that added this fragment, $(git -C "$upstream" rev-parse --short=10 HEAD~2) (entry 2), gave its own entries 2 in $version")"
restore

echo "case: a fragment that declares a number above L"
put "$clone" "$changes/one.yaml" $'kind: Added\nbody: The plugin declares daemon API contract 3.'
run_check
check "exits 1" "$(is "$status" 1)"
check "names the file, the line and L" \
  "$(contains "$err" "^::error file=$changes/one.yaml::$changes/one.yaml:2: declares daemon API contract 3, above L = 2")"
restore

echo "case: fragments that declare their number with the verbs fragments use"
put "$clone" "$changes/one.yaml" \
  $'kind: Added\nbody: The plugin\'s daemon API contract is 1; a caller at contract 0 reads it as before.'
run_check
check "one that is the number its commit gave: exits 0" "$(is "$status" 0)"
check "one that is the number its commit gave: is counted with the declarations, not listed" \
  "$(contains "$err" ' 2 declarations of a fragment.s own number are each one its commit gave its entries, 0 refused; 3 other')"
put "$clone" "$changes/two.yaml" $'kind: Changed\nbody: The daemon API contract stays 1.'
put "$clone" "$changes/numberless.yaml" $'kind: Removed\nbody: |-\n  The daemon API contract of this plugin\n  moves to 3.'
run_check
check "exits 1" "$(is "$status" 1)"
check "refuses one that stays a number its commit did not give" \
  "$(contains "$err" "^::error file=$changes/two.yaml::$changes/two.yaml:2: declares daemon API contract 1, and the commit of main that added this fragment, $(git -C "$upstream" rev-parse --short=10 HEAD~2) (entry 2), gave its own entries 2 in $version")"
check "refuses one that moves to a number above L, after the words that say which it is" \
  "$(contains "$err" "^::error file=$changes/numberless.yaml::$changes/numberless.yaml:4: declares daemon API contract 3, above L = 2")"
restore

echo "case: fragments main does not hold yet"
put "$clone" "$changes/new-below.yaml" $'kind: Fixed\nbody: A fix beside daemon API contract 2.'
run_check
check "one that names a number at or below L: exits 0" "$(is "$status" 0)"
check "one that names a number at or below L: is listed" \
  "$(contains "$out" "^$changes/new-below.yaml:2: contract 2: ")"
put "$clone" "$changes/new-above.yaml" $'kind: Added\nbody: Declares daemon API contract 3.'
run_check
check "one that declares a number above L: exits 1" "$(is "$status" 1)"
check "one that declares a number above L: is refused by name" \
  "$(contains "$err" "^::error file=$changes/new-above.yaml::.*declares daemon API contract 3, above L = 2.*names the entry.s id")"
restore

echo "case: --main names the ref to read"
git -C "$clone" update-ref refs/heads/elsewhere HEAD~2
run_check --main refs/heads/elsewhere
check "exits 0" "$(is "$status" 0)"
check "reads the adding commits from that ref: the later fragments are no adder's, so listed" \
  "$(contains "$err" ' 2 declarations of a fragment.s own number are each one its commit gave')"
run_check --main
check "a --main with no ref: exits 1" "$(is "$status" 1)"
check "a --main with no ref: says how to run it" "$(contains "$err" 'usage: .github/scripts/check-fragment-contracts.sh .--main <ref>.')"

echo "case: no main in the clone"
git -C "$clone" update-ref -d refs/remotes/origin/main
run_check
check "exits 1" "$(is "$status" 1)"
check "names the fetch that brings it" \
  "$(contains "$err" 'refs/remotes/origin/main names no commit in this clone: fetch main without blobs (git fetch --filter=blob:none origin refs/heads/main:refs/remotes/origin/main)')"
git -C "$clone" fetch -q origin

echo "case: no released-contracts.json"
rm "$clone/$released"
run_check
check "exits 1" "$(is "$status" 1)"
check "names the file L is read from" "$(contains "$err" "^::error file=$released::could not read L: $released")"
restore

echo "case: a shallow clone"
shallow=$work/shallow
git clone -q --depth=1 "file://$upstream" "$shallow"
mkdir -p "$shallow/.github/scripts"
ln -s "$check_script" "$shallow/.github/scripts/check-fragment-contracts.sh"
set +e
err=$("$shallow/.github/scripts/check-fragment-contracts.sh" 2>&1 > /dev/null)
status=$?
set -e
check "exits 1" "$(is "$status" 1)"
check "says why, and the fetch that mends it" "$(contains "$err" 'this clone is shallow.*--unshallow')"

summary "check-fragment-contracts.sh"
