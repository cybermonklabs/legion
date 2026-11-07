#!/usr/bin/env bash
# Tests for `check-fireevent-reasons.sh`: a call with no comment above it fails and is named with a
# GitHub annotation; a reason on the line above passes, and one six lines up does not; the three
# ways a reader found of satisfying the check without writing a reason all fail — an arithmetic `*`
# line above the call, a `//` inside a string on the call's own line, and the direct
# `fireEvent(node, event)` form the dotted pattern never saw; a call named inside a comment is prose
# and is not counted; and the real repository passes.
#
# Each case builds a miniature tree under a temporary directory and symlinks the real script into
# it, so the script under test is the file CI runs: it resolves its root from its own path, which
# the symlink puts at the fixture root.
#
# Run from anywhere: .github/scripts/check-fireevent-reasons.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check_script="$script_dir/check-fireevent-reasons.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# fixture <name>: a tree the script can scan, holding one test file. Echoes its root.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/packages/dispatch/web/src"
  ln -s "$check_script" "$root/.github/scripts/check-fireevent-reasons.sh"
  echo "$root"
}

# the fixture's one file, written from this harness's stdin.
rows() { # rows <root>
  cat > "$1/packages/dispatch/web/src/rows.test.tsx"
}

run_check() { # run_check <root>
  set +e
  out=$("$1/.github/scripts/check-fireevent-reasons.sh" 2>&1)
  status=$?
  set -e
}

echo "case: a call with no comment above it is refused, with an annotation"
root=$(fixture bare)
rows "$root" <<'EOF'
test("a row", () => {
  fireEvent.click(button);
});
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the file and line" "$(contains "$out" 'rows.test.tsx:2')"
check "annotates the line" "$(contains "$out" '::error file=packages/dispatch/web/src/rows.test.tsx,line=2::')"
check "counts it against the total" "$(contains "$out" '1 of 1 fireEvent calls')"

echo "case: a reason on the line above passes"
root=$(fixture reasoned)
rows "$root" <<'EOF'
test("a row", () => {
  // The hover intent user-event's hover cannot send.
  fireEvent.pointerOver(row);
});
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "reports the clean scan" "$(contains "$out" 'every one of the 1 fireEvent calls')"

echo "case: a reason six lines up is too far"
root=$(fixture distant)
rows "$root" <<'EOF'
test("a row", () => {
  // The hover intent user-event's hover cannot send.
  const a = 1;
  const b = 2;
  const c = 3;
  const d = 4;
  const e = 5;
  fireEvent.pointerOver(row);
});
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the call" "$(contains "$out" 'rows.test.tsx:8')"

echo "case: an arithmetic line above the call is not a reason"
root=$(fixture arithmetic)
rows "$root" <<'EOF'
test("a row", () => {
  const n = 3 * 2;
  fireEvent.click(button);
});
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the call" "$(contains "$out" 'rows.test.tsx:3')"

echo "case: a URL before the call on its own line does not make the line a comment"
root=$(fixture url)
rows "$root" <<'EOF'
test("a row", () => {
  const src = "https://example.test/a.png"; fireEvent.error(img, { target: { src } });
});
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "counts the call" "$(contains "$out" '1 of 1 fireEvent calls')"

# The window's own version of the same bypass: the `//` of a URL on the line above satisfied a
# substring test for a comment, which is how `const n = 3 * 2;` and this shape both passed.
echo "case: a URL string on the line above is not a reason"
root=$(fixture url_above)
rows "$root" <<'EOF'
test("a row", () => {
  const src = "https://example.test/a.png";
  fireEvent.error(img, { target: { src } });
});
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the call" "$(contains "$out" 'rows.test.tsx:3')"

echo "case: the direct fireEvent(node, event) form counts too"
root=$(fixture direct)
rows "$root" <<'EOF'
test("a row", () => {
  fireEvent(node, new Event("beforeinput"));
});
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the call" "$(contains "$out" 'rows.test.tsx:2')"

echo "case: a comment naming fireEvent is prose, not a call"
root=$(fixture prose)
rows "$root" <<'EOF'
test("a row", () => {
  // A press of a pointer drops the mark as a key press does; fireEvent.mouseDown emits no
  // pointer event, so the row below sends both.
  await user.click(row);
});
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts no call at all" "$(contains "$out" 'every one of the 0 fireEvent calls')"

echo "case: a JSX comment above the call is a reason"
root=$(fixture jsx)
rows "$root" <<'EOF'
test("a row", () => {
  {/* The drop of files user-event has no form for. */}
  fireEvent.drop(zone, dropped);
});
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"

# The count matters as much as the exit status here: a script that resolved the wrong root would
# scan nothing, find nothing bare, and print the same cheerful line.
echo "case: the real repository passes, over calls it actually found"
set +e
out=$("$check_script" 2>&1)
status=$?
set -e
check "exits 0" "$(is "$status" 0)"
check "reports the clean scan" "$(contains "$out" 'carries a reason$')"
check "scanned a three-figure count of calls" "$(contains "$out" 'every one of the [1-9][0-9][0-9] fireEvent calls')"

summary "check-fireevent-reasons.sh"
