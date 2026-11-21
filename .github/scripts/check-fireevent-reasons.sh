#!/usr/bin/env bash
# Every `fireEvent` call under packages/dispatch/web/src carries a reason of its own, and so does
# every hand-written focus or attribute assertion jest-dom's matchers carry.
#
# The Dispatch unit suite performs its interactions with `@testing-library/user-event`, and
# `biome.json` refuses the `fireEvent` import under `packages/dispatch/web/src/**`. A file that
# genuinely needs it keeps the import behind one `biome-ignore lint/style/noRestrictedImports`
# comment naming that file's reasons. That line is the file's claim, and Biome's rule reads imports
# rather than call sites, so once a file carries it the rule says nothing about how many
# `fireEvent` calls the file holds or why each is there.
#
# A forward merge of the default branch is where that hole opens. A base-side test added to a file
# whose exemption is already in place arrives with no conflict, no lint error, no type error, and no
# difference in the pull request's own diff fingerprint or file set, and its calls inherit reasons
# written for other sites — measured on cybermonklabs/legion#2884, where four presses of enabled
# buttons and an edit of an enabled field landed under an ignore line naming a paste, a drop of
# files, an IME composition key, an edit into a control a send holds disabled, same-batch pairs
# inside `act`, and a hand-made `beforeinput`. The sibling case, a base-side call in a file whose
# import the branch had removed, is an unbound name the typecheck names; this one nothing names.
# So the check asks each site for its reason instead of asking each file.
#
# What it buys and what it does not. It makes a `fireEvent` call impossible to add silently: the
# only measured way one arrives unexamined is inside a file whose exemption is already in place,
# and this refuses that. It is a presence check and nothing more — any comment satisfies it, so
# whether a reason is a true one, and whether user-event really has no form for that interaction,
# is the reviewer's to judge. A gate checks what a program can check; the judgement stays with a
# reader.
#
# The second rule is the same check over the assertions jest-dom's matchers carry. The unit
# suite's preload registers `@testing-library/jest-dom`'s matchers, which print the element they
# read and what they found when they fail, and the suite asserts focus and attributes with them.
# Focus is `expect(el).toHaveFocus()` (or `.not.toHaveFocus()`), not the focused element compared
# by hand: `expect(document.activeElement).toBe(el)`, `expect(el).toBe(document.activeElement)`, or
# `expect(document.activeElement === el)` with `===`, `!==`, `==` or `!=` on either side. An
# attribute's presence is `expect(el).toHaveAttribute(name)` or `.not.toHaveAttribute(name)`, not
# `expect(el.hasAttribute(name)).toBe(true)` or `.toBe(false)`. Its value is
# `expect(el).toHaveAttribute(name, value)`, `expect(el).not.toHaveAttribute(name)` or
# `expect(el).toHaveAttribute(name, expect.stringContaining(part))` (`expect.stringMatching(re)`),
# not `expect(el.getAttribute(name))` compared by hand with `.toBe(`, `.toBeNull(`, `.toContain(`
# or `.toMatch(`. Wherever `.toBe(` is named here, `.toEqual(` and `.toStrictEqual(` count too,
# since on a string, a boolean or an element they compare what `.toBe(` does. Each shape counts
# whether or not `expect` carries a message after the value it reads, a string, a template literal
# or a name: `expect(document.activeElement, "why").toBe(el)` is the same site as
# `expect(document.activeElement).toBe(el)`, and when the message argument was first counted, 29
# such sites in 6 files had come in unseen. The suite's sites of those hand-written shapes, and of
# their `.not` forms, were
# rewritten, and nothing but this keeps a new test from writing them again: between the merge that
# moved the suite to the matchers (cybermonklabs/legion#2884) and this rule, the test files' focus
# reads went from 3 to 126 and their attribute reads compared with `.toBe(` from 212 to 273. A site
# the matcher cannot say the same thing at keeps its form and carries its reason as a `fireEvent`
# call does: a value that may be null, which `toHaveAttribute` takes as one the attribute must
# hold, or a `.not` form the matcher decides otherwise. On an absent attribute `.not.toContain(`
# fails and `.not.toHaveAttribute(name, expect.stringContaining(…))` passes; on an element that may
# be missing, `.not.toBeNull()` over `el?.getAttribute(name)` and `.not.toBe(true)` over
# `el?.hasAttribute(name)` pass, and the matcher fails. A message that names what a failure needs,
# such as the key or the path a loop is on, is a reason too: Bun 1.4.4 prints no `expect` message
# for a matcher `expect.extend` registered, so only the hand-written form shows it.
#
# Biome's formatter wraps a long assertion, so this rule reads each file whole: a site whose
# `expect(` ends its line, with the read on the next, is the same site, and is named at the line of
# its `expect(`. A read or comparison counts only at the top level of `expect`'s argument, never
# inside a call there, so `expect(rows.map((row) => row.getAttribute(name))).toEqual(…)` is not
# one, and neither is a read of a property of the focused element (`document.activeElement?.id`)
# or a call it is passed to (`pane.contains(document.activeElement)`), with a message or without
# one. A locator holding an unbalanced parenthesis inside a string hides its site from this, and so
# does a message where a pattern reads the whole argument list (the focused element named in
# `.toBe(`, or compared with `===`).
#
# A reason is a comment within the five lines above the call: the site's own one-line reason, or
# the `biome-ignore` comment where the call sits beside the import. The window is five lines rather
# than the line immediately above because a legitimate site is often wrapped — the reason sits above
# an `await act(async () => {` or a `const` the call needs — and a bare immediately-above rule would
# refuse that shape while buying nothing. It still refuses a reason written only at the top of the
# file, which is the case this exists for.
#
# A comment is a line whose trimmed text begins with `//`, `/*`, `*` or JSX's `{/*`, and nothing
# else is. A line holding `//` or `*` anywhere else is code, in both places this reads one: on the
# call's own line, where `const src = "https://x/a.png"; fireEvent.error(…)` used to be skipped as
# prose, and on a line of the window above it, where that same URL, or `const n = 3 * 2;`, used to
# count as a reason. A call written inside a real comment is prose and is not counted, which is what
# lets a reason name `fireEvent.mouseDown` while explaining why a site keeps it.
#
# Both forms of the API count: `fireEvent.click(node)` and the direct `fireEvent(node, event)`.
#
# Run from anywhere: .github/scripts/check-fireevent-reasons.sh
# CI runs it in ci.yaml's lint job.
set -euo pipefail

cd "$(dirname "$0")/../.."

python3 <<'PY'
import re
import sys
from pathlib import Path

ROOT = Path("packages/dispatch/web/src")
CALL = re.compile(r"\bfireEvent\b\s*[.(]")
WINDOW = 5
# A comment line, and the only thing this reads as one: the trimmed text begins a comment. A `//`
# or a `*` anywhere else on a line is code, which is what the two bypasses this closes relied on.
COMMENT = re.compile(r"^\s*(//|/\*|\*|\{/\*)")

# One character that is not a parenthesis, or a parenthesised group whose own parentheses balance,
# four levels deep: `(await within(row).findByText("x")).closest("[title]")` is two. A read can
# match only between such groups, never inside one, which is what keeps a read nested in the
# argument (a `.map` over rows) from counting as the whole argument.
BALANCED = r"[^()]"
for _ in range(4):
    BALANCED = rf"(?:[^()]|\({BALANCED}*\))"
# A message `expect` carries after the value it reads (`expect(el.hasAttribute(name), "why")`)
# leaves the site what it is, so where a read must end `expect`'s argument, a message may follow
# it: a string, a template literal or a name. Only those: a read followed by anything else, such as
# the second item of `expect([a.getAttribute(x), b.getAttribute(y)])` or the next key of an object,
# is not the whole value `expect` reads.
LITERAL = "|".join((r'"(?:[^"\\\n]|\\.)*"', r"'(?:[^'\\\n]|\\.)*'", r"`(?:[^`\\]|\\.)*`"))
NAME = r"[A-Za-z_$][\w$]*(?:\??\.[A-Za-z_$][\w$]*)*"
MESSAGE = rf"(?:\s*,\s*(?:{LITERAL}|{NAME})\s*,?)?"
# The focused element compared by hand: read as `expect`'s whole argument and compared with
# `.toBe(`; named as what `.toBe(` compares with; or compared with `===`, `!==`, `==` or `!=`, on
# either side, at the top level of `expect`'s argument. A read of a property of the focused element
# (`document.activeElement?.tagName`, `.matches(…)`) is not that element, and an argument it is
# passed to (`pane.contains(document.activeElement)`) is not a comparison, so neither counts. The
# second and third patterns already read the whole argument list, a message included.
COMPARE = r"(?:toBe|toEqual|toStrictEqual)"
FOCUS = (
    re.compile(
        rf"\bexpect\(\s*document\.activeElement\s*{MESSAGE}\)\s*(?:\.not\s*)?\.{COMPARE}\("
    ),
    re.compile(
        rf"\bexpect\({BALANCED}*\)\s*(?:\.not\s*)?\.{COMPARE}\(\s*document\.activeElement\s*\)"
    ),
    re.compile(
        rf"\bexpect\({BALANCED}*?"
        r"(?:\bdocument\.activeElement\s*[!=]==?|[!=]==?\s*document\.activeElement\b(?!\s*[!?]?\.))"
    ),
)
ATTRIBUTE = (
    re.compile(
        rf"\bexpect\({BALANCED}*?\.getAttribute\({BALANCED}*\){MESSAGE}\s*\)"
        r"\s*(?:\.not\s*)?\.(?:toBe|toEqual|toStrictEqual|toBeNull|toContain|toMatch)\("
    ),
    re.compile(
        rf"\bexpect\({BALANCED}*?\.hasAttribute\({BALANCED}*\){MESSAGE}\s*\)"
        rf"\s*(?:\.not\s*)?\.{COMPARE}\("
    ),
)


def reasoned(lines: list[str], index: int) -> bool:
    return any(COMMENT.match(previous) for previous in lines[max(0, index - WINDOW) : index])


files = 0
sites = 0
bare: list[tuple[Path, int, str]] = []
reads = 0
bare_reads: list[tuple[Path, int, str]] = []

for path in sorted(ROOT.rglob("*")):
    if path.suffix not in (".ts", ".tsx") or not path.is_file():
        continue
    files += 1
    text = path.read_text()
    # Split on `\n` alone, as the read's line below is counted, so both rules name a site at the
    # line an editor and GitHub's annotation show.
    lines = text.split("\n")
    for index, line in enumerate(lines):
        if CALL.search(line) is None or COMMENT.match(line):
            continue
        sites += 1
        if reasoned(lines, index):
            continue
        bare.append((path, index + 1, line.strip()))
    # A site two patterns both find is one site: each is counted once, at its `expect(`.
    starts = {match.start() for pattern in (*FOCUS, *ATTRIBUTE) for match in pattern.finditer(text)}
    found = sorted(text.count("\n", 0, start) for start in starts)
    for index in found:
        if COMMENT.match(lines[index]):
            continue
        reads += 1
        if reasoned(lines, index):
            continue
        bare_reads.append((path, index + 1, lines[index].strip()))


def report(found: list[tuple[Path, int, str]]) -> None:
    for path, line, text in found:
        print(f"::error file={path},line={line}::{path}:{line}: {text}")
        print(f"  {path}:{line}", file=sys.stderr)
        print(f"    {text}", file=sys.stderr)


if bare:
    print(
        f"{len(bare)} of {sites} fireEvent calls under {ROOT} carry no reason within the "
        f"{WINDOW} lines above them:",
        file=sys.stderr,
    )
    report(bare)
    print(
        "\nEach one is an interaction `@testing-library/user-event` has no form for, or it is not "
        "one. Where user-event has a form, use it: `await user.click(…)`, `await user.type(…)`, "
        "`await user.selectOptions(…)`, with `userEvent.setup()`, or `userOnFakeClock()` from "
        "web/src/__tests__/user.ts under a held clock. Where it has none, write the reason on the "
        "line above the call. A file's `biome-ignore` line is the file's claim and reasons no "
        "single call.",
        file=sys.stderr,
    )

if bare_reads:
    if bare:
        print(file=sys.stderr)
    print(
        f"{len(bare_reads)} of {reads} hand-written focus and attribute assertions under {ROOT} "
        f"carry no reason within the {WINDOW} lines above them:",
        file=sys.stderr,
    )
    report(bare_reads)
    print(
        "\nThe preload's jest-dom matchers say each of these and print the element they read when "
        "they fail: `expect(el).toHaveFocus()` (`.not.toHaveFocus()`) for "
        "`expect(document.activeElement).toBe(el)`, `expect(el).toBe(document.activeElement)` or "
        "`expect(document.activeElement === el)`; `expect(el).toHaveAttribute(name)` for "
        "`expect(el.hasAttribute(name)).toBe(true)` and `expect(el).not.toHaveAttribute(name)` "
        "for `.toBe(false)`; and for `expect(el.getAttribute(name))`, "
        "`expect(el).toHaveAttribute(name, value)` for `.toBe(value)`, "
        "`expect(el).not.toHaveAttribute(name)` for `.toBeNull()`, "
        "`expect(el).toHaveAttribute(name)` for `.not.toBeNull()`, and "
        "`expect(el).toHaveAttribute(name, expect.stringContaining(part))` or "
        "`expect.stringMatching(re)` for `.toContain(part)` or `.toMatch(re)`. Where the matcher "
        "cannot say the same thing — a value that may be null, a `.not` form over a part of an "
        "attribute or over an element that may be missing — write the reason on the line above "
        "the assertion.",
        file=sys.stderr,
    )

if bare or bare_reads:
    sys.exit(1)

print(f"every one of the {sites} fireEvent calls under {ROOT} carries a reason")
print(
    f"every one of the {reads} hand-written focus and attribute assertions in the {files} files "
    f"under {ROOT} carries a reason"
)
PY
