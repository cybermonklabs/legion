#!/usr/bin/env bash
# Tests for `check-kept-for-one-release.sh`: a repository whose every listed id is named by a
# marker, and whose every marker names a listed id, passes, a marker in a file git has not been
# told about and one ending in a carriage return counted; a marker whose id the list does not name
# fails, naming its file and line; a listed id no marker names fails, naming the list's line; and a
# sentence that goes on after the id is no marker, so the id it names is still unmarked.
#
# Each case builds a miniature git repository under a temporary directory and symlinks the real
# script into it, so the script under test is the file CI runs: it resolves its root from its own
# path, which the symlink puts at the fixture root. No case runs the check over this repository's
# own tree: ci.yaml's lint job does (.github/workflows/ci-job-lint.yaml, "Check every thing kept for
# one release is listed and marked"), on every change.
#
# Run from anywhere: .github/scripts/check-kept-for-one-release.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check_script="$script_dir/check-kept-for-one-release.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

command -v git > /dev/null || { echo "  FAIL: git is required" >&2; exit 1; }

# The marker's words, in two pieces so this file holds no marker the real check would read.
words='kept for one'
words+=' release'
list=packages/dispatch/e2e/acceptance/root-deletes.txt

# fixture <name>: a repository whose list holds the ids alpha and beta, and whose one tracked file
# marks both. Echoes its root.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/packages/dispatch/e2e/acceptance" "$root/src"
  ln -s "$check_script" "$root/.github/scripts/check-kept-for-one-release.sh"
  printf '# What is kept for one release.\nalpha\tThe first thing\tNothing reads it\nbeta\tThe second thing\tNothing reads it\n' \
    > "$root/$list"
  printf 'one()   // %s: alpha\ntwo()   // %s: beta\n' "$words" "$words" > "$root/src/kept.go"
  git -C "$root" init -q
  git -C "$root" add "$list" src/kept.go
  echo "$root"
}

# run_check <root>: sets `out` to the combined output and `status` to the exit code.
run_check() {
  local root=$1
  set +e
  out=$(cd "$root" && .github/scripts/check-kept-for-one-release.sh 2>&1)
  status=$?
  set -e
}

echo "case: every listed id is marked and every marker is listed"
root=$(fixture clean)
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts the ids and the markers" "$(contains "$out" 'each of the 2 ids .* each of the 2 markers names a listed id')"

echo "case: a marker in an untracked file and one ending in a carriage return count"
root=$(fixture untracked)
printf 'one()\n' > "$root/src/kept.go"
printf 'one()   // %s: alpha\r\n' "$words" > "$root/src/crlf.go"
printf 'two()   # %s: beta  \n' "$words" > "$root/src/new.py"
git -C "$root" add src/crlf.go
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts both markers" "$(contains "$out" 'each of the 2 markers names a listed id')"

echo "case: a marker whose id the list does not name is refused"
root=$(fixture unlisted)
printf 'three() // %s: gamma\n' "$words" >> "$root/src/kept.go"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the marker's file and line" "$(contains "$out" 'src/kept.go:3: a marker names the id "gamma"')"
check "annotates the line" "$(contains "$out" '::error file=src/kept.go,line=3::')"
check "refuses nothing else" "$(contains "$out" '1 finding(s)')"

echo "case: a listed id no marker names is refused"
root=$(fixture unmarked)
printf 'one()   // %s: alpha\n' "$words" > "$root/src/kept.go"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the list's line" "$(contains "$out" "$list:3: lists \"beta\", and no marker in the repository names it")"
check "annotates the list" "$(contains "$out" "::error file=$list,line=3::")"
check "refuses nothing else" "$(contains "$out" '1 finding(s)')"

echo "case: a sentence that goes on after the id is no marker"
root=$(fixture sentence)
printf 'one()   // %s: alpha\n// The %s: beta and what goes on after it.\n// %s: delta until the next one\n' \
  "$words" "$words" "$words" > "$root/src/kept.go"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "reads beta as unmarked" "$(contains "$out" 'lists "beta", and no marker in the repository names it')"
check "reads no marker naming delta" "$(is "$(contains "$out" '"delta"')" false)"
check "refuses nothing else" "$(contains "$out" '1 finding(s)')"

summary "check-kept-for-one-release.sh"
