#!/usr/bin/env bash
# Tests for `check-fireevent-reasons.sh`: a call with no comment above it fails and is named with a
# GitHub annotation; a reason on the line above passes, and one six lines up does not; the four ways
# a reader found of satisfying the check without writing a reason all fail — an arithmetic `*` line
# above the call, a `//` inside a string before the call on its own line, that same URL on the line
# above, and the direct `fireEvent(node, event)` form the dotted pattern never saw; a call named
# inside a comment is prose and is not counted. The jest-dom rule: a hand-written focus read, its
# `.not` form, the focused element named as what `.toBe(` compares with, and a comparison with it
# by `===` or `!==` inside `expect` each fail with no comment above, while a read of a property of
# the focused element or a call it is passed to is not counted; a presence read
# `expect(el.hasAttribute(name)).toBe(true|false)` fails, its `.not` and wrapped forms too; a
# hand-written attribute read fails whether it is compared with `.toBe(`, `.toBeNull(`,
# `.not.toBeNull(`, `.toContain(` or `.toMatch(`, and also when Biome has wrapped it onto lines of
# its own, where it is named at its `expect(`; a focus, `getAttribute` or `hasAttribute` read whose
# `expect` carries a message, a string or a template literal, fails as it does without one, while
# with a message a matcher, a property of the focused element, a read nested in a call and a read
# inside an array or an object are still not counted; a kept site with its reason above passes;
# jest-dom's own matchers, an asymmetric matcher among them, a read nested inside `expect`'s
# argument and a read named in a comment are not counted; and one run reports both rules. The real
# repository passes over a count of calls and of files it actually found, so a run that scanned
# nothing fails rather than reporting a clean scan.
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

echo "case: a hand-written focus read with no comment above it is refused, with an annotation"
root=$(fixture focus)
rows "$root" <<'EOF'
test("a row", () => {
  expect(document.activeElement).toBe(row);
});
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the file and line" "$(contains "$out" 'rows.test.tsx:2')"
check "annotates the line" "$(contains "$out" '::error file=packages/dispatch/web/src/rows.test.tsx,line=2::')"
check "counts it against the total" "$(contains "$out" '1 of 1 hand-written focus and attribute assertions')"
check "names the matcher to use" "$(contains "$out" 'expect(el).toHaveFocus()')"

echo "case: its .not form is refused too"
root=$(fixture focus_not)
rows "$root" <<'EOF'
test("a row", () => {
  expect(document.activeElement).not.toBe(row);
});
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the read" "$(contains "$out" 'rows.test.tsx:2')"

echo "case: the focused element compared by hand from the other side is refused"
root=$(fixture focus_compared)
rows "$root" <<'EOF'
test("a row", () => {
  expect(row).toBe(document.activeElement);
  expect(search).not.toBe(document.activeElement);
  expect(document.activeElement === trigger).toBe(true);
  expect(trigger !== document.activeElement).toBe(true);
});
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "counts all four" "$(contains "$out" '4 of 4 hand-written focus and attribute assertions')"
check "names the .toBe(document.activeElement)" "$(contains "$out" 'rows.test.tsx:2')"
check "names the .not.toBe(document.activeElement)" "$(contains "$out" 'rows.test.tsx:3')"
check "names the === comparison" "$(contains "$out" 'rows.test.tsx:4')"
check "names the !== comparison" "$(contains "$out" 'rows.test.tsx:5')"

echo "case: a read of a property of the focused element, or a call it is passed to, is not counted"
root=$(fixture focus_property)
rows "$root" <<'EOF'
test("a row", () => {
  expect(document.activeElement?.tagName).toBe("BUTTON");
  expect(document.activeElement?.matches("[data-row]")).toBe(true);
  expect(pane.contains(document.activeElement)).toBe(true);
  expect(document.activeElement?.id === "row-1").toBe(true);
  expect(row === document.activeElement?.parentElement).toBe(true);
});
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts no read at all" "$(contains "$out" 'every one of the 0 hand-written focus and attribute assertions')"

echo "case: a presence read compared with toBe, its .not form and its wrapped form are refused"
root=$(fixture presence)
rows "$root" <<'EOF'
test("a row", () => {
  expect(button.hasAttribute("disabled")).toBe(true);
  expect(button.hasAttribute("aria-disabled")).toBe(false);
  expect(button.hasAttribute("hidden")).not.toBe(true);
  expect(
    view.container.querySelector("section[aria-label='Document editor'] article")?.hasAttribute("hidden")
  ).toBe(true);
  expect(button).toHaveAttribute("disabled");
  expect(rows.every((row) => row.hasAttribute("data-row"))).toBe(true);
});
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "counts the four reads and nothing else" "$(contains "$out" '4 of 4 hand-written focus and attribute assertions')"
check "names the toBe(true) read" "$(contains "$out" 'rows.test.tsx:2')"
check "names the toBe(false) read" "$(contains "$out" 'rows.test.tsx:3')"
check "names the .not.toBe read" "$(contains "$out" 'rows.test.tsx:4')"
check "names the wrapped read at its expect" "$(contains "$out" 'rows.test.tsx:5')"
check "names the matcher to use" "$(contains "$out" 'expect(el.hasAttribute(name)).toBe(true)')"

echo "case: a hand-written attribute read with no comment above it is refused"
root=$(fixture attribute)
rows "$root" <<'EOF'
test("a row", () => {
  const row = screen.getByRole("row");
  expect(row.getAttribute("aria-current")).toBe("true");
});
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the read" "$(contains "$out" 'rows.test.tsx:3')"
check "names the matcher to use" "$(contains "$out" 'expect(el).toHaveAttribute(name, value)')"

echo "case: an attribute read compared by hand with toBeNull, its .not, toContain or toMatch is refused"
root=$(fixture compared)
rows "$root" <<'EOF'
test("a row", () => {
  expect(row.getAttribute("aria-busy")).toBeNull();
  expect(row.getAttribute("aria-controls")).not.toBeNull();
  expect(row.getAttribute("title")).toContain("ready");
  expect(row.getAttribute("title")).toMatch(/^Copy issue key/);
});
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "counts all four" "$(contains "$out" '4 of 4 hand-written focus and attribute assertions')"
check "names the toBeNull read" "$(contains "$out" 'rows.test.tsx:2')"
check "names the .not.toBeNull read" "$(contains "$out" 'rows.test.tsx:3')"
check "names the toContain read" "$(contains "$out" 'rows.test.tsx:4')"
check "names the toMatch read" "$(contains "$out" 'rows.test.tsx:5')"
check "names the matcher to use" "$(contains "$out" 'expect.stringContaining(part)')"

echo "case: a focus or attribute read compared with toEqual or toStrictEqual is refused"
root=$(fixture equal)
rows "$root" <<'EOF'
test("a row", () => {
  expect(row.getAttribute("aria-current")).toEqual("true");
  expect(row.getAttribute("title")).toStrictEqual("Copy");
  expect(row.hasAttribute("hidden")).toEqual(false);
  expect(document.activeElement).toEqual(row);
  expect(row).toStrictEqual(document.activeElement);
});
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "counts all five" "$(contains "$out" '5 of 5 hand-written focus and attribute assertions')"
check "names the getAttribute toEqual read" "$(contains "$out" 'rows.test.tsx:2')"
check "names the getAttribute toStrictEqual read" "$(contains "$out" 'rows.test.tsx:3')"
check "names the hasAttribute toEqual read" "$(contains "$out" 'rows.test.tsx:4')"
check "names the activeElement toEqual read" "$(contains "$out" 'rows.test.tsx:5')"
check "names the toStrictEqual(document.activeElement) read" "$(contains "$out" 'rows.test.tsx:6')"

# Biome's formatter wraps a long assertion so: a line-by-line pattern would see neither the
# `expect(` line nor the read below it as the shape.
echo "case: an attribute read Biome wrapped onto lines of its own is refused at its expect"
root=$(fixture wrapped)
rows "$root" <<'EOF'
test("a row", async () => {
  expect(
    (await within(row).findByText("Implementer")).closest("[title]")?.getAttribute("title")
  ).toBe(claimant);
});
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the expect's line" "$(contains "$out" 'rows.test.tsx:2')"
check "counts it once" "$(contains "$out" '1 of 1 hand-written focus and attribute assertions')"

# `expect`'s second argument is a message, and a site's form is the same with it: a pattern that
# wanted the read to close `expect(` straight after it saw none of these.
echo "case: a focus or attribute read whose expect carries a message is refused at its line"
root=$(fixture message)
rows "$root" <<'EOF'
test("a row", () => {
  expect(document.activeElement, "the press took the focus").toBe(row);
  expect(document.activeElement, `${key} moved the focus`).toBe(row);
  expect(row.getAttribute("aria-current"), "the row is not current").toBe("true");
  expect(row.getAttribute("aria-current"), `${path} is not current`).toBe("true");
  expect(shown?.hasAttribute("tabindex"), "the link lost its tab stop").toBe(false);
  expect(shown?.hasAttribute("tabindex"), `${name} lost its tab stop`).toBe(false);
  expect(
    hidden?.hasAttribute("tabindex"),
    "the link the cut hid has no tab stop once the words are whole"
  ).toBe(false);
});
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "counts all seven" "$(contains "$out" '7 of 7 hand-written focus and attribute assertions')"
check "names the focus read with a string message" "$(contains "$out" 'rows.test.tsx:2:')"
check "names the focus read with a template message" "$(contains "$out" 'rows.test.tsx:3:')"
check "names the getAttribute read with a string message" "$(contains "$out" 'rows.test.tsx:4:')"
check "names the getAttribute read with a template message" "$(contains "$out" 'rows.test.tsx:5:')"
check "names the hasAttribute read with a string message" "$(contains "$out" 'rows.test.tsx:6:')"
check "names the hasAttribute read with a template message" "$(contains "$out" 'rows.test.tsx:7:')"
check "names the wrapped read with a message at its expect" "$(contains "$out" 'rows.test.tsx:8:')"

echo "case: with a message, what the rule leaves out is still left out"
root=$(fixture message_left_out)
rows "$root" <<'EOF'
test("a row", () => {
  // Not `expect(document.activeElement, "why").toBe(row)` by hand: the matcher prints the row.
  expect(row, "the press took the focus").toHaveFocus();
  expect(row, `${key} is not current`).toHaveAttribute("aria-current", "true");
  expect(document.activeElement?.tagName, "the focus left the button").toBe("BUTTON");
  expect(rows.map((item) => item.getAttribute("data-turn")), "the turns").toEqual(["a", "b"]);
  expect([row.getAttribute("data-a"), row.getAttribute("data-b")]).toEqual(["1", "2"]);
  expect({ a: row.getAttribute("data-a"), b: "2" }).toEqual({ a: "1", b: "2" });
});
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts no read at all" "$(contains "$out" 'every one of the 0 hand-written focus and attribute assertions')"

echo "case: a kept site with its reason on the line above passes"
root=$(fixture kept)
rows "$root" <<'EOF'
test("a row", () => {
  // `described` is a `string | null`, and `toHaveAttribute` given null passes for no element.
  expect(box.getAttribute("aria-describedby")).toBe(described);
});
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "reports the clean scan" "$(contains "$out" 'every one of the 1 hand-written focus and attribute assertions')"

echo "case: jest-dom's matchers, a read nested in the argument and a read in a comment are not counted"
root=$(fixture matchers)
rows "$root" <<'EOF'
test("a row", () => {
  // Not `expect(document.activeElement).toBe(row)` by hand: the matcher prints the row it read.
  expect(row).toHaveFocus();
  expect(row).toHaveAttribute("aria-current", "true");
  expect(row).not.toHaveAttribute("aria-busy");
  expect(row).toHaveAttribute("title", expect.stringContaining("ready"));
  expect(rows.map((item) => item.getAttribute("data-turn"))).toEqual(["a", "b"]);
  expect(rows.map((item) => item.getAttribute("data-turn")).join()).toBe("a,b");
});
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts no read at all" "$(contains "$out" 'every one of the 0 hand-written focus and attribute assertions')"

echo "case: one run reports a bare call and a bare read, and fails"
root=$(fixture both)
rows "$root" <<'EOF'
test("a row", () => {
  fireEvent.click(button);
  expect(document.activeElement).toBe(button);
});
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the call" "$(contains "$out" '1 of 1 fireEvent calls')"
check "names the read" "$(contains "$out" '1 of 1 hand-written focus and attribute assertions')"
check "annotates the call's line" "$(contains "$out" 'rows.test.tsx,line=2::')"
check "annotates the read's line" "$(contains "$out" 'rows.test.tsx,line=3::')"

# The count matters as much as the exit status here: a script that resolved the wrong root would
# scan nothing, find nothing bare, and print the same cheerful line.
echo "case: the real repository passes, over calls and files it actually found"
set +e
out=$("$check_script" 2>&1)
status=$?
set -e
check "exits 0" "$(is "$status" 0)"
check "reports the clean scan" "$(contains "$out" 'carries a reason$')"
check "scanned a non-zero count of calls" "$(contains "$out" 'every one of the [1-9][0-9]* fireEvent calls')"
check "scanned a non-zero count of files" "$(contains "$out" 'in the [1-9][0-9]* files under')"

summary "check-fireevent-reasons.sh"
