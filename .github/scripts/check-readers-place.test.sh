#!/usr/bin/env bash
# Tests for `check-readers-place.sh`: a read of the window's scroll with no comment above it
# fails and is named with a GitHub annotation; a direct write of a scroll in the other package
# fails too, so both roots are really scanned; a reason on the line directly above passes, and
# one two lines up does not, which is the whole of the one-line window; the shapes a reader
# could satisfy the check with without writing a reason - an arithmetic `*` line, a `//` inside
# a string, a generator method's `*rows() {`, and a comment with no word in it (an empty `//` or
# `/**/`, the ` */`, ` **/` or JSX `*/}` that closes a comment about something else) - all fail;
# a site named inside a comment is prose and is not counted, while a site on a generator
# method's own line is code and is; the module the rule exists for, and the suite's own test
# files, are outside the reach; a read of a box's own `scrollTop`, or a comparison of it, is not
# a reading of a reader's place while `documentElement.scrollTop` is, through `?.` too; every
# write of `scrollTop` counts, by name or bracketed name and by `=` or any compound assignment;
# every call that moves a scroll counts, by each of its spellings - `scrollTo(`, `scrollBy(`,
# `scrollIntoView(`, `scrollIntoViewIfNeeded(`, the `.scroll(` alias, any of them written as an
# optional call or made through `.call(` or `.apply(`, and a `.scrollTop =` whose value a
# formatter has put on the next line - and so does every spelling of the reader's own place,
# `scrollY` destructured off the window and `scrollingElement` included, while a bare `scroll(`
# counts too, since that is the window's own alias and a local function of that name is renamed
# rather than excused;
# and the real repository passes over a count of sites it actually found, so a run that scanned
# nothing fails rather than reporting a clean scan.
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
/**
 * And one that wrote window.scrollTo(0, 0), named in a doc comment's own lines.
 *
 */
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

echo "case: a call that moves a scroll counts when it is written as an optional call"
root=$(fixture optional_call)
surface "$root" <<'EOF'
export function show(row: HTMLElement): void {
  row.scrollIntoView?.({ block: "nearest" });
  window.scrollTo?.({ top: 0 });
}
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "counts both" "$(contains "$out" '2 of 2 scroll sites')"
check "names the optional scrollIntoView" "$(contains "$out" 'Surface.tsx:2')"
check "names the optional scrollTo" "$(contains "$out" 'Surface.tsx:3')"

echo "case: the .scroll( alias of scrollTo counts, on a box and on the window"
root=$(fixture scroll_alias)
surface "$root" <<'EOF'
export function toTop(box: HTMLElement): void {
  box.scroll({ top: 0 });
  window.scroll(0, 0);
}
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "counts both" "$(contains "$out" '2 of 2 scroll sites')"
check "names the box's" "$(contains "$out" 'Surface.tsx:2')"
check "names the window's" "$(contains "$out" 'Surface.tsx:3')"

echo "case: a bare scroll( is the window's own alias and counts, so a local function of that name is renamed rather than excused"
root=$(fixture bare_scroll)
surface "$root" <<'EOF'
export function toTheTop(): void {
  scroll({ top: 0 });
}
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the site" "$(contains "$out" 'Surface.tsx:2')"

echo "case: scrollIntoViewIfNeeded counts, as the call it is an older name for does"
root=$(fixture if_needed)
surface "$root" <<'EOF'
export function show(row: HTMLElement): void {
  row.scrollIntoViewIfNeeded(true);
}
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the site" "$(contains "$out" 'Surface.tsx:2')"
check "says what the site does" "$(contains "$out" 'writes a scroll')"

echo "case: a scrollTop write whose value a formatter put on the next line counts"
root=$(fixture wrapped_write)
surface "$root" <<'EOF'
export function toEnd(box: HTMLElement, rows: readonly number[]): void {
  box.scrollTop =
    rows.reduce((total, row) => total + row, 0) - box.clientHeight;
}
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the line the write begins on" "$(contains "$out" 'Surface.tsx:2')"
check "says what the site does" "$(contains "$out" 'writes a scroll')"

echo "case: the page's scrollingElement is a reading of where the reader is"
root=$(fixture scrolling_element)
surface "$root" <<'EOF'
export function place(): number {
  return document.scrollingElement?.scrollTop ?? 0;
}
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the site" "$(contains "$out" 'Surface.tsx:2')"
check "says what the site does" "$(contains "$out" 'reads where the reader is')"

echo "case: scrollY counts under the bare name a destructuring leaves it"
root=$(fixture destructured_read)
surface "$root" <<'EOF'
export function place(): number {
  const { scrollY } = window;
  return scrollY;
}
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "counts both" "$(contains "$out" '2 of 2 scroll sites')"
check "names the destructuring" "$(contains "$out" 'Surface.tsx:2')"
check "names the use" "$(contains "$out" 'Surface.tsx:3')"

echo "case: the line closing a doc comment about something else is not a reason"
root=$(fixture comment_closer)
surface "$root" <<'EOF'
export function show(row: HTMLElement): void {
  /** What this function is for, which is no reason for the line under this comment's end.
   */
  row.scrollIntoView({ block: "start" });
}
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the site" "$(contains "$out" 'Surface.tsx:4')"

echo "case: the document element's scrollTop counts when it is read through an optional chain"
root=$(fixture optional_document_element)
surface "$root" <<'EOF'
export function place(): number {
  return document.documentElement?.scrollTop ?? 0;
}
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the site" "$(contains "$out" 'Surface.tsx:2')"
check "says what the site does" "$(contains "$out" 'reads where the reader is')"

echo "case: a scrollTop written by its bracketed name counts"
root=$(fixture bracket_write)
surface "$root" <<'EOF'
export function toTop(box: HTMLElement): void {
  box["scrollTop"] = 5;
  box['scrollTop'] += 5;
}
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "counts both" "$(contains "$out" '2 of 2 scroll sites')"
check "names the double-quoted write" "$(contains "$out" 'Surface.tsx:2')"
check "names the single-quoted write" "$(contains "$out" 'Surface.tsx:3')"
check "says what the site does" "$(contains "$out" 'writes a scroll')"

echo "case: every compound assignment to scrollTop counts, and a comparison does not"
root=$(fixture compound_write)
surface "$root" <<'EOF'
export function move(box: HTMLElement): boolean {
  box.scrollTop ??= 5;
  box.scrollTop *= 2;
  box.scrollTop /= 2;
  box.scrollTop ||= 1;
  box.scrollTop &&= 1;
  box.scrollTop **= 1;
  return box.scrollTop >= 5 || box.scrollTop <= 1 || box.scrollTop === 3 || box.scrollTop != 2;
}
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "counts the six writes and no comparison" "$(contains "$out" '6 of 6 scroll sites')"
check "names the ??= write" "$(contains "$out" 'Surface.tsx:2')"
check "names the *= write" "$(contains "$out" 'Surface.tsx:3')"
check "names the /= write" "$(contains "$out" 'Surface.tsx:4')"
check "names the ||= write" "$(contains "$out" 'Surface.tsx:5')"
check "names the &&= write" "$(contains "$out" 'Surface.tsx:6')"
check "names the **= write" "$(contains "$out" 'Surface.tsx:7')"

echo "case: a call that moves a scroll counts when it is made through .call or .apply"
root=$(fixture call_apply)
surface "$root" <<'EOF'
export function toTop(box: HTMLElement, row: HTMLElement): void {
  window.scrollTo.call(window, 0, 0);
  box.scrollBy.apply(box, [0, 10]);
  row.scrollIntoView.call(row);
  box.scroll.apply(box, [0, 0]);
}
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "counts all four" "$(contains "$out" '4 of 4 scroll sites')"
check "names the scrollTo.call" "$(contains "$out" 'Surface.tsx:2')"
check "names the scrollBy.apply" "$(contains "$out" 'Surface.tsx:3')"
check "names the scrollIntoView.call" "$(contains "$out" 'Surface.tsx:4')"
check "names the scroll.apply" "$(contains "$out" 'Surface.tsx:5')"

echo "case: a generator method's line is code, not a comment, on the site's line and above it"
root=$(fixture generator_method)
surface "$root" <<'EOF'
export class Walker {
  *walk() { window.scrollTo(0, 0); yield 1; }
  *rows() {
    yield window.scrollY;
  }
}
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "counts both" "$(contains "$out" '2 of 2 scroll sites')"
check "names the site on the method's own line" "$(contains "$out" 'Surface.tsx:2')"
check "names the site under the method's line" "$(contains "$out" 'Surface.tsx:4')"

echo "case: a comment with nothing in it is no reason"
root=$(fixture empty_comments)
surface "$root" <<'EOF'
export function show(row: HTMLElement, box: HTMLElement): void {
  //
  row.scrollIntoView({ block: "start" });
  /**/
  box.scrollTo({ top: 0 });
  /** What this function is for, which is no reason for the line under this comment's end.
   **/
  window.scrollBy(0, 1);
}
export function View(): JSX.Element {
  return (
    <div>
      {/* What this view is for, which is no reason for the line under this comment's end.
      */}
      <div ref={(element) => element?.scrollIntoView()} />
    </div>
  );
}
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "counts all four" "$(contains "$out" '4 of 4 scroll sites')"
check "names the site under an empty //" "$(contains "$out" 'Surface.tsx:3')"
check "names the site under an empty /**/" "$(contains "$out" 'Surface.tsx:5')"
check "names the site under a **/ closer" "$(contains "$out" 'Surface.tsx:8')"
check "names the site under a JSX */} closer" "$(contains "$out" 'Surface.tsx:15')"

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
