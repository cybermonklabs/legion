#!/usr/bin/env bash
# Every reading of where a reader is, and every write that moves them, goes through
# packages/dispatch/web/src/features/doc/readers-place.ts.
#
# That module takes the box that scrolls the document - `whatScrolls`, the page itself being its
# `null` case - and it records where the page's own place-keeping left that box (`moveTheScroll`,
# `moveTheBox`, `putTheBoxAt`, `placeInView`, read back by `thePageLeftItThere`). Both halves are
# load-bearing. A surface that reads `window.scrollY` is right only while the page is the box that
# scrolls: in the Inbox's reading column the window's offset is a number the reader never moves, so
# a place recorded from it is the wrong place and a test of whether the reader is at the top
# answers the page's top rather than the column's. A surface that writes a scroll directly leaves
# no record, so the landing correction that watches the same box (`refocus-when-settled.ts`) reads
# the page's own correction as the reader taking the column over, stops watching, and leaves the
# reader wherever a late-drawn diagram or panel pushed them (dispatch://INFRA-341, R11, A12).
#
# Nothing in the type system says so. Each of these is one plain line, every browser offers it on
# every element, and the two packages had forty-three of them between them at this branch's
# merge base (1826da13), counted there by the patterns below: each one read right on the page it
# was written for and wrongly on the other surface.
# Closing a class of defect in the tree does not keep it closed, so this check holds the tree to
# the one module.
#
# Its reach is the product code of the two packages: every .ts and .tsx file under
# packages/dispatch/web/src and packages/proof-editor/src, less the module itself. It refuses
#   * a read of `scrollY` or `pageYOffset` by any spelling - `window.scrollY`, or the bare name
#     a destructuring leaves behind (`const { scrollY } = window`) - a read of
#     `documentElement.scrollTop` or `documentElement?.scrollTop`, or a mention of
#     `scrollingElement`, which is that same box of the page under another name: these are "how
#     far down the page the reader is", and
#   * a write of `scrollTop`, by name (`.scrollTop`) or by bracketed name (`["scrollTop"]`),
#     with `=` or any compound assignment (`+=`, `-=`, `*=`, `**=`, `??=`, `||=`, `&&=` and the
#     rest), the `=` ending its own line included where a formatter has put a long value on the
#     next one, or a call of `scrollTo(`, `scrollBy(`, `scrollIntoView(`,
#     `scrollIntoViewIfNeeded(` or `.scroll(` - the alias of `scrollTo` - each of them made
#     plainly, optionally (`scrollIntoView?.(`) or through `.call(` or `.apply(`, which are the
#     ways a scroll is moved.
# Reads of `element.scrollTop` other than the document element's are left alone, a comparison
# (`box.scrollTop >= n`) among them: that is a box's own geometry, which `whereTheReaderIs` is
# itself written in terms of and which a scroller may legitimately measure (`use-live-follow.ts`
# asks how far from its live end it stands).
# One further shape is outside the rule rather than missed by it: `scrollLeft` is how far across
# a box stands, which is not where a reader is in a document. A `scroll(` with no receiver is
# read as a site, because `scroll({ top: 0 })` on its own is the window's own alias, and a local
# function of that name is renamed rather than excused - the tree holds none today. What it
# cannot see is a site the formatter has split over two lines so that neither line holds a shape
# above, and a call taken off its receiver without being made (`const go = window.scrollTo`, a
# `.bind(`) - those are a reviewer's to catch.
#
# Test files are outside that reach - `*.test.ts`, `*.test.tsx`, and anything under a `__tests__`
# directory. A unit test of this behaviour is the browser: its harness defines `window.scrollY`,
# writes `scrollTop` to stand for the reader's own hand on the scrollbar, and asserts on both to
# say where the page left the reader. Those 276 sites are the measurement rather than the thing
# measured, and 228 of them carry no reason above them today, so a check that refused them would
# be answered by 228 reason lines that each said "this is a test", which buys a reader nothing. A
# test cannot introduce the defect either: the class is a surface reading or writing the wrong
# box, and no test ships.
#
# The escape is the one the fireEvent gate has (`check-fireevent-reasons.sh`): a comment on the
# line directly above the site, saying why this site is not the page's place-keeping. Two kinds
# of site have one today, both in the Proof editor and each a scroll it really makes - a
# ProseMirror transaction's own `scrollIntoView()` flag, which scrolls the caret's ancestors when
# the view applies the transaction, on the keystroke the reader has just made, and a keystroke
# has already ended every landing correction watching that box (`READER_ACTIONS`); and the
# editor's own focus scroll, which is the Proof editor placing its own selection inside its own
# DOM, and which the host that asked for it records as the page's own doing (`focusInEditor`,
# `corrected()` in `features/doc/refocus-when-settled.ts`).
# The window is one line rather than the five the fireEvent gate allows, because each of these
# sites is a statement of its own and the reason has to stand against the site it excuses: a
# reason five lines up is a reason for whatever was there when it was written.
#
# What it buys and what it does not. It makes an unexamined reading or write, in any of the
# spellings above, impossible to add: the only way one lands is under a line of prose a reviewer
# can read and judge. Any comment with a word in it satisfies the check, so whether a reason is
# a true one is the reviewer's to judge, as it is for the fireEvent gate. A gate checks what a
# program can check.
#
# A comment is a line whose trimmed text begins with `//`, `/*`, JSX's `{/*`, or a run of `*`
# that whitespace, a `/` or the line's end follows, and nothing else is: a `//` inside a string,
# a `*` in arithmetic, or the `*` of a generator method (`*walk() {`) is code in both places
# this reads a line - on the site's own line, where a URL would otherwise make the site prose,
# and on the line above it, where the same URL would otherwise count as a reason. A site written
# inside a real comment is prose and is not counted, which is what lets this file's own siblings
# name `scrollIntoView(` while explaining a rule. A comment with no word in it is no reason: an
# empty `//` or `/**/`, and a line holding nothing but the ` */`, ` **/` or JSX `*/}` that
# closes a comment, since what that ends was written about whatever stood above the site, so a
# site under it is bare.
#
# Run from anywhere: .github/scripts/check-readers-place.sh
# CI runs it in ci.yaml's lint job.
set -euo pipefail

cd "$(dirname "$0")/../.."

python3 <<'PY'
import re
import sys
from pathlib import Path

ROOTS = (Path("packages/dispatch/web/src"), Path("packages/proof-editor/src"))
# The one module a reader's place is read and written through.
PLACE = Path("packages/dispatch/web/src/features/doc/readers-place.ts")

READ = re.compile(
    r"\bscrollY\b|\bpageYOffset\b|\bdocumentElement\??\.scrollTop\b|\bscrollingElement\b"
)
# An assignment: an `=` that is no comparison and no arrow, or any compound one - `+=`, `*=`,
# `**=`, `??=`, `||=`, `>>>=` and the rest.
ASSIGN = r"\s*(?:\*\*|\?\?|\|\||&&|>>>|<<|>>|[-+*/%&|^])?=(?![=>])"
# A call, plain or optional, or made through `.call` or `.apply`.
CALL = r"\s*(?:(?:\?\.|\.)\s*(?:call|apply)\s*)?(?:\?\.)?\("
WRITE = re.compile(
    r"""(?:\.scrollTop|\[\s*["'`]scrollTop["'`]\s*\])""" + ASSIGN
    + r"|\b(?:scrollTo|scrollBy|scrollIntoView|scrollIntoViewIfNeeded)" + CALL
    + r"|(?:\.|\b)scroll" + CALL
)
# A comment line, and the only thing this reads as one: the trimmed text begins a comment - a
# `*` run only where whitespace, a `/` or the line's end follows it, so a generator method
# (`*walk() {`) is code.
COMMENT = re.compile(r"^\s*(?://|/\*|\*+(?![^\s/])|\{/\*)")
# Text in a comment: a reason has some, where an empty `//`, a `/**/`, or a line holding only
# the ` */`, ` **/` or JSX `*/}` that closes a comment has none - and what a closer ends was
# written about whatever stood before the site under it.
TEXT = re.compile(r"\w")


def is_test(path: Path) -> bool:
    return ".test." in path.name or "__tests__" in path.parts


def is_reason(line: str) -> bool:
    return COMMENT.match(line) is not None and TEXT.search(line) is not None


files = 0
sites = 0
bare: list[tuple[Path, int, str, str]] = []

for root in ROOTS:
    for path in sorted(root.rglob("*")):
        if path.suffix not in (".ts", ".tsx") or not path.is_file():
            continue
        if path == PLACE or is_test(path):
            continue
        files += 1
        lines = path.read_text().splitlines()
        for index, line in enumerate(lines):
            if COMMENT.match(line):
                continue
            reads = READ.search(line)
            writes = WRITE.search(line)
            if reads is None and writes is None:
                continue
            sites += 1
            if index > 0 and is_reason(lines[index - 1]):
                continue
            what = "reads where the reader is" if reads is not None else "writes a scroll"
            bare.append((path, index + 1, what, line.strip()))

if bare:
    print(
        f"{len(bare)} of {sites} scroll sites outside {PLACE} carry no reason on the line "
        "above them:",
        file=sys.stderr,
    )
    for path, line, what, text in bare:
        print(f"::error file={path},line={line}::{path}:{line}: {what} outside {PLACE}")
        print(f"  {path}:{line} ({what})", file=sys.stderr)
        print(f"    {text}", file=sys.stderr)
    print(
        f"\nEach one is a reading of where a reader is, or a write that moves them, taken outside "
        f"the one module that knows which box scrolls the document. Take it through {PLACE}: read "
        "with `whereTheReaderIs(whatScrolls(element))`, `scrollportTop` or `scrollportHeight`, and "
        "write with `moveTheScroll`, `moveTheBox`, `putTheBoxAt` or `placeInView`, each of which "
        "records where the page's own place-keeping left that box so a landing correction can tell "
        "it from the reader (`refocus-when-settled.ts`). Where the site really is not the page's "
        "place-keeping - a ProseMirror transaction's own `scrollIntoView()` flag, the editor's "
        "focus scroll inside its own DOM - write that reason on the line directly above it.",
        file=sys.stderr,
    )
    sys.exit(1)

roots = ", ".join(str(root) for root in ROOTS)
print(
    f"every one of the {sites} scroll sites outside {PLACE} carries a reason "
    f"({files} files under {roots}, tests aside)"
)
PY
