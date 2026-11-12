#!/usr/bin/env bash
# Tests for `check-readers-place.sh`: a read of the window's scroll with no comment above it
# fails and is named with a GitHub annotation; a direct write of a scroll in the other package
# fails too, so both roots are really scanned; a reason on the line directly above passes, and
# one two lines up does not, which is the whole of the one-line window; the two shapes a reader
# could satisfy the check with without writing a reason - an arithmetic `*` line and a `//`
# inside a string - both fail; a site named inside a comment is prose and is not counted; the
# module the rule exists for, and the suite's own test files, are outside the reach; a read of a
# box's own `scrollTop` is not a reading of a reader's place while `documentElement.scrollTop`
# is; each of the three calls that move a scroll counts; and the real repository passes over a
# count of sites it actually found, so a run that scanned nothing fails rather than reporting a
# clean scan.
#
# Each case builds a miniature tree under a temporary directory and symlinks the real script into
# it, so the script under test is the file CI runs: it resolves its root from its own path, which
# the symlink puts at the fixture root.
#
# Run from anywhere: .github/scripts/check-readers-place.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check_script="$script_dir/check-readers-place.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# fixture <name>: a tree the script can scan, holding both of the roots it reads. Echoes its root.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/packages/dispatch/web/src/features/doc" \
    "$root/packages/proof-editor/src"
  ln -s "$check_script" "$root/.github/scripts/check-readers-place.sh"
  echo "$root"
}

# a surface of the Dispatch web app, written from this harness's stdin.
surface() { # surface <root>
  cat > "$1/packages/dispatch/web/src/features/doc/Surface.tsx"
}

# a file of the Proof editor, the other root, written from this harness's stdin.
editor() { # editor <root>
  cat > "$1/packages/proof-editor/src/editor.ts"
}

run_check() { # run_check <root>
  set +e
  out=$("$1/.github/scripts/check-readers-place.sh" 2>&1)
  status=$?
  set -e
}

echo "case: a read of the window's scroll with no comment above it is refused, with an annotation"
root=$(fixture bare_read)
surface "$root" <<'EOF'
export function place(): number {
  return window.scrollY;
}
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the file and line" "$(contains "$out" 'Surface.tsx:2')"
check "annotates the line" "$(contains "$out" '::error file=packages/dispatch/web/src/features/doc/Surface.tsx,line=2::')"
check "says what the site does" "$(contains "$out" 'reads where the reader is')"
check "counts it against the total" "$(contains "$out" '1 of 1 scroll sites')"

echo "case: a direct write of a scroll in the Proof editor is refused too"
root=$(fixture bare_write)
editor "$root" <<'EOF'
export function toTop(element: HTMLElement): void {
  element.scrollTop = 0;
}
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the file and line" "$(contains "$out" 'packages/proof-editor/src/editor.ts:2')"
check "says what the site does" "$(contains "$out" 'writes a scroll')"

echo "case: a reason on the line directly above passes"
root=$(fixture reasoned)
surface "$root" <<'EOF'
export function place(): number {
  // The page really is the box that scrolls this surface.
  return window.scrollY;
}
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "reports the clean scan" "$(contains "$out" 'every one of the 1 scroll sites')"

echo "case: a reason two lines up is too far"
root=$(fixture distant)
surface "$root" <<'EOF'
export function place(): number {
  // The page really is the box that scrolls this surface.
  const where = 0;
  return where + window.scrollY;
}
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the site" "$(contains "$out" 'Surface.tsx:4')"

echo "case: an arithmetic line above the site is not a reason"
root=$(fixture arithmetic)
surface "$root" <<'EOF'
export function place(): number {
  const margin = 3 * 2;
  return margin + window.scrollY;
}
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the site" "$(contains "$out" 'Surface.tsx:3')"

echo "case: a URL string on the line above is not a reason"
root=$(fixture url_above)
surface "$root" <<'EOF'
export function place(): number {
  const src = "https://example.test/a.png";
  return src.length + window.scrollY;
}
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the site" "$(contains "$out" 'Surface.tsx:3')"

echo "case: a site named inside a comment is prose, not a site"
root=$(fixture prose)
surface "$root" <<'EOF'
// A surface that read window.scrollY, or wrote scrollIntoView(), would be refused here.
export function place(): number {
  return 0;
}
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts no site at all" "$(contains "$out" 'every one of the 0 scroll sites')"

echo "case: the module the rule exists for is outside the reach"
root=$(fixture the_module)
cat > "$root/packages/dispatch/web/src/features/doc/readers-place.ts" <<'EOF'
export function whereTheReaderIs(scroller: HTMLElement | null): number {
  return scroller === null ? window.scrollY : scroller.scrollTop;
}
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts no site at all" "$(contains "$out" 'every one of the 0 scroll sites')"

echo "case: a test file is outside the reach, since its harness is the browser"
root=$(fixture a_test)
cat > "$root/packages/dispatch/web/src/features/doc/Surface.test.tsx" <<'EOF'
test("the reader is left where the page put them", () => {
  expect(window.scrollY).toBe(240);
  element.scrollTop = 240;
});
EOF
mkdir -p "$root/packages/dispatch/web/src/__tests__"
cat > "$root/packages/dispatch/web/src/__tests__/harness.ts" <<'EOF'
export function scrolled(element: HTMLElement): void {
  element.scrollTop = 240;
}
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts no site at all" "$(contains "$out" 'every one of the 0 scroll sites')"

echo "case: a box's own scrollTop is read freely; the document element's is not"
root=$(fixture reads)
surface "$root" <<'EOF'
export function distance(box: HTMLElement): number {
  return box.scrollHeight - box.scrollTop - box.clientHeight;
}
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts no site at all" "$(contains "$out" 'every one of the 0 scroll sites')"
root=$(fixture document_element)
surface "$root" <<'EOF'
export function place(): number {
  return document.documentElement.scrollTop;
}
EOF
run_check "$root"
check "fails on the document element's" "$(is "$status" 1)"
check "names the site" "$(contains "$out" 'Surface.tsx:2')"

echo "case: each call that moves a scroll counts, and so does pageYOffset"
root=$(fixture every_call)
surface "$root" <<'EOF'
export function move(element: HTMLElement): number {
  window.scrollTo({ top: 0 });
  element.scrollBy(0, 10);
  element.scrollIntoView({ block: "start" });
  return window.pageYOffset;
}
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "counts all four" "$(contains "$out" '4 of 4 scroll sites')"
check "names the scrollTo" "$(contains "$out" 'Surface.tsx:2')"
check "names the scrollBy" "$(contains "$out" 'Surface.tsx:3')"
check "names the scrollIntoView" "$(contains "$out" 'Surface.tsx:4')"
check "names the pageYOffset" "$(contains "$out" 'Surface.tsx:5')"

# The count matters as much as the exit status here: a script that resolved the wrong root would
# scan nothing, find nothing bare, and print the same cheerful line.
echo "case: the real repository passes, over sites it actually found"
set +e
out=$("$check_script" 2>&1)
status=$?
set -e
check "exits 0" "$(is "$status" 0)"
check "reports the clean scan" "$(contains "$out" 'carries a reason')"
check "scanned a non-zero count of sites" "$(contains "$out" 'every one of the [1-9][0-9]* scroll sites')"
check "says how much it read" "$(contains "$out" '[1-9][0-9]* files under packages/dispatch/web/src, packages/proof-editor/src')"

summary "check-readers-place.sh"
