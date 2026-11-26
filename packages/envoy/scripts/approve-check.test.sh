#!/usr/bin/env bash
# packages/envoy/scripts/approve-check.test.sh
#
# approve-check.sh's exit status, from the steps library it records its steps with
# (scripts/lib/approve-check-steps.sh), with no stack: each case runs a sequence of step results in
# a subshell of its own and reads the status it ends with and the rows it wrote. The header of
# approve-check.sh promises 1 when a step failed, 3 when none failed and one is UNKNOWN, and 0 when
# every step passed; an early stop (a stack that did not start, a sign-in or a setup that did not
# complete) is UNKNOWN, so after a FAIL it must still end 1, since a caller reads 3 as "nothing
# failed". And each patch approve-check.sh makes a control build from still applies to this tree.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LIB="$SCRIPT_DIR/lib/approve-check-steps.sh"
ROOT="$(mktemp -d)"
trap 'rm -rf "$ROOT"' EXIT
failures=0

# run_case <name> <want status> <want rows> <commands>: the commands, after sourcing the library,
# in a subshell whose WORK is a directory of its own.
run_case() {
  local name="$1" want="$2" rows="$3" commands="$4" work status got
  work="$(mktemp -d "$ROOT/case.XXXXXX")"
  status=0
  (
    set -euo pipefail
    WORK="$work"
    FAILED=0
    UNKNOWN=0
    # shellcheck source=lib/approve-check-steps.sh
    . "$LIB"
    eval "$commands"
    finish
  ) >"$work/out" 2>&1 || status=$?
  got="$(wc -l <"$work/results.tsv" 2>/dev/null || echo 0)"
  got="${got//[[:space:]]/}"
  if [ "$status" = "$want" ] && [ "$got" = "$rows" ]; then
    printf 'PASS: %s (exit %s, %s rows)\n' "$name" "$status" "$got"
  else
    printf 'FAIL: %s: exit %s and %s rows, want exit %s and %s rows\n%s\n' "$name" "$status" "$got" "$want" "$rows" "$(cat "$work/out")"
    failures=$((failures + 1))
  fi
}

run_case "every step passed" 0 2 'result a PASS x; result b PASS y'
run_case "a step failed" 1 2 'result a FAIL x; result b PASS y'
run_case "an UNKNOWN and no failure" 3 2 'result a PASS x; result b UNKNOWN y'
run_case "a failure and an UNKNOWN" 1 2 'result a FAIL x; result b UNKNOWN y'
run_case "an early stop with nothing failed" 3 2 'result a PASS x; stop_unknown "b stack" "did not start"; result c PASS z'
run_case "a failure, then an early stop" 1 2 'result a FAIL x; stop_unknown "b stack" "did not start"; result c PASS z'
run_case "an early stop first" 3 1 'stop_unknown "sign-in" "printed no code"'

# approve-check.sh makes its control builds by applying each patch of scripts/approve to copies of
# the files the patch names (build_with_patch), so an edit to one of those files that moves a
# hunk's context stops approve-check before its first step, and nothing else would say so. Each
# patch is applied here as build_with_patch applies it, patch -p1 on copies, as a dry run with no
# fuzz, and checked by git apply too. GNU patch places a hunk that carries no context by its line
# numbers and the lines it removes, so it takes a hunk whose place nothing pins, and could take it
# at another place once the file moves; git apply refuses such a hunk anywhere but at a file's
# start or end. A directory holding no patch is a failure: a check that read nothing has not
# passed.
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
patches=0
for patch_file in "$SCRIPT_DIR"/approve/*.patch; do
  [ -e "$patch_file" ] || continue
  patches=$((patches + 1))
  tree="$(mktemp -d "$ROOT/patch.XXXXXX")"
  missing=""
  while IFS= read -r file; do
    mkdir -p "$tree/$(dirname "$file")"
    cp "$REPO_ROOT/$file" "$tree/$file" 2>/dev/null || missing="$missing $file"
  done < <(sed -n 's|^+++ b/\([^[:space:]]*\).*|\1|p' "$patch_file")
  if [ -n "$missing" ]; then
    printf 'FAIL: %s names files this tree does not hold:%s\n' "${patch_file##*/}" "$missing"
    failures=$((failures + 1))
  elif ! out="$(patch --dry-run --fuzz=0 -s -p1 -d "$tree" <"$patch_file" 2>&1)"; then
    printf 'FAIL: %s no longer applies to this tree, so approve-check cannot make its control build:\n%s\n' "${patch_file##*/}" "$out"
    failures=$((failures + 1))
  elif ! out="$(cd "$tree" && git apply --check -p1 "$patch_file" 2>&1)"; then
    printf 'FAIL: %s applies with patch but git apply refuses it, so a hunk of it sits where no context pins it:\n%s\n' "${patch_file##*/}" "$out"
    failures=$((failures + 1))
  else
    printf 'PASS: %s applies to this tree\n' "${patch_file##*/}"
  fi
done
if [ "$patches" = 0 ]; then
  echo "FAIL: no patch under $SCRIPT_DIR/approve, though approve-check.sh builds its controls from two"
  failures=$((failures + 1))
fi

if [ "$failures" != 0 ]; then
  echo "approve-check.test: $failures case(s) failed" >&2
  exit 1
fi
echo "approve-check.test: every case passed"
