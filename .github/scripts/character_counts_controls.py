"""The controls of the character-count check: planted trees and ledgers through the same code as the
walk, each with the answer it must get. They run before every scan, and a control that answers
otherwise fails the check."""
from __future__ import annotations

import os
import subprocess
import tempfile
from typing import Callable

from character_counts_ledger import HEADER, LEDGER
from character_counts_net import Site, sites
from character_counts_shapes import OUTSIDE, SHAPES
from character_counts_tree import (BINARY, DATED, FIXTURE, LEFT_OUT, LINK, MISSING, SELF, classify, disk_reader,
                                   git_listing, judge_tree)

# A site outside the checker's own files, so that a planted tree is not a scan that saw nothing.
ANCHOR = ("docs/anchor.md", b"A title is at most 2,000 characters.\n")
REASON = "A planted control decides this site."


def row(site: Site, unit: str = "code points", counter: str = "none", reason: str = REASON) -> str:
    return f"{site.path}\t{site.sha}\t{site.occurrence}\t{site.copy}\t{counter}\t{unit}\t{reason}"


def rows_for(files: dict[str, bytes], unit: str = "code points") -> list[str]:
    """A row for every site of files, each with unit."""
    return [row(site, unit) for path, data in files.items() for site in sites(path, data)]


def judge(files: dict[str, bytes | str | None], rows: list[str] | None = None) -> tuple[int, list[str]]:
    """The exit code and report of a scan over files with the anchor, and a ledger of rows plus the
    anchor's row (no ledger when rows is None)."""
    tree: dict[str, bytes | str | None] = {ANCHOR[0]: ANCHOR[1], **files}
    if rows is not None:
        tree[LEDGER] = "\n".join([HEADER, *rows_for({ANCHOR[0]: ANCHOR[1]}), *rows, ""]).encode()
    code, report, _ = judge_tree(sorted(tree), tree.get)
    return code, report


def undecided(report: list[str]) -> set[str]:
    """The `path:line` of each site the report names as having no row."""
    return {line.split("::", 2)[2].split(" has a count beside ")[0] for line in report
            if " that no row of " in line and line.startswith("::error")}


def shape_files() -> dict[str, bytes]:
    return {path: content.encode() for _, path, content, _ in SHAPES}


def net_finds_each_shape() -> list[str]:
    """Each line a shape names holds a site, and no line OUTSIDE names does."""
    problems = []
    for name, path, content, lines in SHAPES:
        found = {site.line for site in sites(path, content.encode())}
        if not set(lines) <= found:
            problems.append(f"the net finds no site on line {sorted(set(lines) - found)} of {path} ({name})")
    for name, path, content, lines in OUTSIDE:
        found = {site.line for site in sites(path, content.encode())}
        if set(lines) & found:
            problems.append(f"{path} ({name}) is a site now: move it from OUTSIDE to SHAPES and the header's limits")
    return problems


def each_shape_fails_with_no_row() -> list[str]:
    """A tree of every shape and no row for any fails on every line each shape names."""
    code, report = judge(shape_files(), [])
    want = {f"{path}:{line}" for _, path, _, lines in SHAPES for line in lines}
    missed = sorted(want - undecided(report))
    return [f"a shape with no row fails: exit {code}, not named: {missed[:5]}"] if code != 1 or missed else []


def each_shape_answers_by_its_row() -> list[str]:
    """With a code-points row for every site the tree passes; a row that says bytes or UTF-16 code
    units fails naming its unit."""
    files = shape_files()
    problems = []
    code, report = judge(files, rows_for(files))
    if code != 0:
        problems.append(f"every shape with its row passes: exit {code}: {report[-3:]}")
    for unit in ("bytes", "UTF-16 code units"):
        code, report = judge(files, rows_for(files, unit))
        named = sum(1 for line in report if f"calls a count of {unit} " in line)
        want = sum(len(sites(path, data)) for path, data in files.items())
        if code != 1 or named != want:
            problems.append(f"a row that says {unit} fails: exit {code}, {named} of {want} sites named")
    return problems


PLANTED = "docs/planted.md"
LINE = b"A body holds at most 32,000 characters.\n"


def ledger_rules() -> list[str]:
    """Each rule of the ledger, through a planted file and its rows."""
    problems = []
    found = sites(PLANTED, LINE)
    if not found:
        return [f"the net finds no site in {PLANTED}'s planted line, so no rule can be tried"]
    first = found[0]
    cases: list[tuple[str, dict[str, bytes | None], list[str], int, str]] = [
        ("a site with its row passes", {PLANTED: LINE}, [row(first)], 0, ""),
        ("a site with no row fails", {PLANTED: LINE}, [], 1, "that no row of"),
        ("a row whose line text changed fails as stale", {PLANTED: b"A body holds at most 32,001 characters.\n"},
         [row(first)], 1, "a row with no site"),
        ("a row for a removed line fails as unused", {PLANTED: b"A body holds what it holds.\n"}, [row(first)], 1,
         "a row with no site"),
        ("a row for a removed file fails as unused", {}, [row(first)], 1, "the check reads no file at that path"),
        ("a duplicate row fails", {PLANTED: LINE}, [row(first), row(first)], 1, "a duplicate row"),
        ("a line moved inside its file keeps its row", {PLANTED: b"# Moved\n\n" + LINE}, [row(first)], 0, ""),
        ("a second copy of a line needs its own row", {PLANTED: LINE + LINE}, [row(first)], 1, "that no row of"),
        ("a row decides only the path it names", {PLANTED: LINE, "docs/copied.md": LINE}, [row(first)], 1,
         "docs/copied.md:1 has a count"),
        ("each occurrence on a line needs its own row", {PLANTED: b"32 characters, or 32 chars.\n"},
         [row(sites(PLANTED, b"32 characters, or 32 chars.\n")[0])], 1, "(occurrence 2)"),
        ("a row that says bytes fails", {PLANTED: LINE}, [row(first, "bytes")], 1, "calls a count of bytes"),
        ("a row that says UTF-16 code units fails", {PLANTED: LINE}, [row(first, "UTF-16 code units")], 1,
         "calls a count of UTF-16 code units"),
        ("a row that says not a length passes", {PLANTED: LINE}, [row(first, "not a length")], 0, ""),
        ("a row with no reason fails", {PLANTED: LINE}, [row(first, reason="")], 1, "its reason is not"),
        ("a row with a unit outside the list fails", {PLANTED: LINE}, [row(first, "graphemes")], 1, "its unit is not"),
        ("a row with six fields fails", {PLANTED: LINE}, [row(first).rsplit("\t", 1)[0]], 1, "7 tab-separated fields"),
        ("a counter in a file the tree does not hold fails", {PLANTED: LINE},
         [row(first, counter="packages/gone.go:count")], 1, "is not in the tree"),
        ("a counter whose file does not hold its symbol fails", {PLANTED: LINE, "packages/count.go": b"func n() {}\n"},
         [row(first, counter="packages/count.go:measure")], 1, "the file does not hold the symbol"),
        ("a counter the tree holds passes", {PLANTED: LINE, "packages/count.go": b"func measure() {}\n"},
         [row(first, counter="packages/count.go:measure")], 0, ""),
    ]
    for name, files, rows, want_code, want_text in cases:
        code, report = judge(files, rows)
        if code != want_code or (want_text and not any(want_text in line for line in report)):
            problems.append(f"{name}: exit {code}, report {report[-3:]}")
    code, report = judge({PLANTED: LINE})
    if code != 1 or not any(f"{LEDGER} is not in the listing" in line for line in report):
        problems.append(f"a tree with no ledger fails: exit {code}")
    return problems


# One planted path per rule of LEFT_OUT, written out apart from it, with the class it is left out
# as, and paths the check must read: a rule added to LEFT_OUT has no case here, and one that
# reaches a path it must read leaves that path out, so either fails.
LEFT_OUT_CASES = [
    (SELF[0], LEFT_OUT[0][1]),
    (SELF[1], LEFT_OUT[0][1]),
    (LEDGER, LEFT_OUT[1][1]),
    ("packages/envoy/CHANGELOG.md", DATED[0]),
    ("docs/plans/planted.md", DATED[0]),
    ("docs/superpowers/planted.md", DATED[0]),
    ("docs/research/planted.md", DATED[0]),
    ("docs/solutions/planted.md", DATED[0]),
    ("packages/daemon/cmd/legion/testdata/gh-help/planted.txt", FIXTURE[0]),
    ("packages/daemon/cmd/legion/testdata/gh-help-walk/planted.txt", FIXTURE[0]),
    ("scripts/planted.cast", FIXTURE[0]),
    ("packages/envoy/internal/dispatch/pmdoc/heavybodies/testdata/planted.md", FIXTURE[0]),
    ("packages/envoy/internal/dispatch/pmdoc/testdata/planted.md", FIXTURE[0]),
    ("scripts/approval-eval/fixtures/planted.md", FIXTURE[0]),
]
# A file of each class the check must read, each holding a count beside the word: a rule or a
# filter that leaves one class out leaves its file's site undecided-and-unseen, which fails here.
MUST_READ = {
    "docs/site/src/content/docs/dispatch/planted.md": b"A title is at most 1,000 characters.\n",
    "docs/planted.mdx": b"A title is at most 1,000 characters.\n",
    "README.md": b"A title is at most 1,000 characters.\n",
    "packages/envoy/AGENTS.md": b"A title is at most 1,000 characters.\n",
    "skills/planted/SKILL.md": b"A title is at most 1,000 characters.\n",
    "packages/daemon/cmd/legion/testdata/planted.txt": b"title: 1000 characters\n",
    "packages/envoy/internal/dispatch/pmdoc/planted.md": b"A title is at most 1,000 characters.\n",
    "scripts/planted.sh": b'#!/usr/bin/env bash\necho "${#1} characters"\n',
    "scripts/planted": b'#!/usr/bin/env bash\necho "${#1} characters"\n',
    "scripts/planted.py": b'print(f"{len(b)} characters")\n',
    "packages/envoy/internal/planted/planted.go": b'package planted\n\nvar _ = fmt.Sprintf("%d characters", len(s))\n',
    "packages/envoy/internal/planted/planted_test.go": b'package planted\n\nvar _ = fmt.Sprintf("%d chars", len(s))\n',
    "packages/envoy/internal/planted/planted_generated.go": b'package planted\n\nvar Rule = "at most 32000 characters"\n',
    "packages/envoy-client/src/planted.ts": b"export const f = (s: string) => `${s.length} characters`;\n",
    "packages/envoy-client/src/planted.test.ts": b'test("cuts at 2000 characters", () => {});\n',
    "packages/dispatch/web/src/Planted.tsx": b"export const C = ({ s }: { s: string }) => <p>{s.length} characters</p>;\n",
    "packages/envoy-client/scripts/planted.mjs": b"export const f = (s) => `${s.length} characters`;\n",
    "packages/envoy-client/scripts/planted.js": b"module.exports = (s) => `${s.length} characters`;\n",
    ".github/workflows/planted.yaml": b"- run: echo \"$(printf '%s' \"$T\" | wc -c) characters\"\n",
    "docs/planted.yml": b'message: "Issue #42: a title is at most 200 characters"\n',
    "docs/planted.json": b'{"rule": "a title is at most 2000 \\u0063haracters"}\n',
    "docs/planted.toml": b'rule = "at most 2000 characters" # a comment\n',
    "docs/planted.txt": b"at most 2000 characters\n",
    "docs/planted.html": b"<p>at most 2000 &#99;haracters</p>\n",
    "packages/envoy/Dockerfile": b'RUN test "${#NAME}" -le 40 # characters\n',
    "packages/envoy/migrations/0001_planted.sql": b"-- a title is at most 1000 characters\n",
    ".github/scripts/check-character-counts.sh": b"# a count of 2000 characters\n",
    ".github/scripts/character_counts_net.py": b"# a count of 2000 characters\n",
}


def file_classes() -> list[str]:
    """Each rule of LEFT_OUT has a case and leaves it out as its class; each path of MUST_READ is
    read and its site found through the walk; a binary, a link and a missing file are left out and
    counted."""
    problems = []
    for rule, kind, _ in LEFT_OUT:
        if not any(rule.search(path) for path, _ in LEFT_OUT_CASES):
            problems.append(f"the left-out rule {rule.pattern} has no case in LEFT_OUT_CASES")
    for path, kind in LEFT_OUT_CASES:
        found = classify(path, b"text")
        if found is None or found[0] != kind:
            problems.append(f"{path} is left out as {kind!r}: answered {found}")
    code, report = judge(dict(MUST_READ), [])
    missed = sorted(f"{path}:{site.line}" for path, data in MUST_READ.items() for site in sites(path, data)
                    if f"{path}:{site.line}" not in undecided(report))
    unseen = sorted(path for path, data in MUST_READ.items() if not sites(path, data))
    if code != 1 or missed or unseen:
        problems.append(f"each class the check must read is read: exit {code}, not named {missed}, no site in {unseen}")
    left = {SELF[0]: b'echo "%d characters"\n', "packages/envoy/CHANGELOG.md": b"- A title is at most 1,000 characters.\n",
            "docs/solutions/planted.md": b"a 32,000-character body\n", "docs/planted.png": b"\x89PNG\r\n\x1a\n\0\0 2000 characters",
            "packages/claude-envoy/skills/planted": "link", "gone.md": None}
    code, report = judge(left, [])
    if code != 0:
        problems.append(f"no file left out is read: exit {code}: {report[-3:]}")
    for kind, count in ((LEFT_OUT[0][1], 1), (DATED[0], 2), (BINARY, 1), (LINK, 1), (MISSING, 1)):
        if not any(line.startswith(f"  left out, {kind}: {count}") for line in report):
            problems.append(f"the report counts {count} file(s) left out as {kind!r}: {report}")
    return problems


def unknown() -> list[str]:
    """An empty listing, and a tree whose only sites are the checker's own, print UNKNOWN and fail."""
    problems = []
    code, report, _ = judge_tree([], lambda path: None)
    if code != 1 or not report[-1].startswith("UNKNOWN:"):
        problems.append(f"an empty listing prints UNKNOWN and fails: exit {code}, {report[-1:]}")
    own = {".github/scripts/character_counts_net.py": b"# a count of 2000 characters\n",
           "docs/notes.md": b"A title holds the words a person writes.\n"}
    code, report, _ = judge_tree(sorted(own), own.get)
    if code != 1 or not report[-1].startswith("UNKNOWN:"):
        problems.append(f"a tree whose only site is the checker's own prints UNKNOWN and fails: exit {code}, {report[-1:]}")
    return problems


def git_enumeration() -> list[str]:
    """In a real repository, the walk reads a tracked and an untracked file and not an ignored one,
    and fails on each undecided site of the two."""
    with tempfile.TemporaryDirectory() as root:
        def write(path: str, data: bytes) -> None:
            os.makedirs(os.path.dirname(os.path.join(root, path)) or root, exist_ok=True)
            with open(os.path.join(root, path), "wb") as handle:
                handle.write(data)

        for args in (["init", "-q"], ["config", "user.email", "check@example.invalid"], ["config", "user.name", "check"]):
            subprocess.run(["git", *args], cwd=root, check=True, capture_output=True)
        write(".gitignore", b"ignored.md\n")
        write("tracked.md", b"A title is at most 1,000 characters.\n")
        write("ignored.md", b"A title is at most 3,000 characters.\n")
        subprocess.run(["git", "add", "-A"], cwd=root, check=True, capture_output=True)
        write("untracked.md", b"A title is at most 2,000 characters.\n")
        write(LEDGER, (HEADER + "\n").encode())
        listing = git_listing(root)
        code, report, _ = judge_tree(listing, disk_reader(root))
    named = undecided(report)
    if listing != sorted([".gitignore", LEDGER, "tracked.md", "untracked.md"]) or code != 1 or \
            named != {"tracked.md:1", "untracked.md:1"}:
        return [f"git lists the tracked and the untracked file and not the ignored one: {listing}, exit {code}, named {named}"]
    return []


CONTROLS: list[tuple[str, Callable[[], list[str]]]] = [
    ("each planted shape of rounds 1 to 3 is a site on each line it names, and each shape outside the net is not",
     net_finds_each_shape),
    ("a tree of every shape with no row fails on each named line", each_shape_fails_with_no_row),
    ("every shape passes with a code-points row, and fails with a row that says bytes or UTF-16 code units",
     each_shape_answers_by_its_row),
    ("the ledger's rules: stale, unused, duplicate, moved, copied, per occurrence, per copy, units, schema, counters",
     ledger_rules),
    ("each class left out is counted, and each class the check must read is read", file_classes),
    ("an empty listing and a tree of the checker's own sites print UNKNOWN", unknown),
    ("git lists the tracked and untracked files and not the ignored ones", git_enumeration),
]


def run_controls() -> tuple[list[str], list[str]]:
    """The names of the controls, and each problem a control found."""
    problems = []
    for name, check in CONTROLS:
        try:
            problems += [f"{name}: {problem}" for problem in check()]
        except Exception as error:  # a control that cannot run has not answered as it must
            problems.append(f"{name}: it raised {error!r}")
    return [name for name, _ in CONTROLS], problems
