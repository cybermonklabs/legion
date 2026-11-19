"""The walk of the character-count check over the files git lists: the classes of files it leaves
out, each with its reason, the sites the net finds in the rest, their comparison with the ledger,
and the report of what it read, left out and found."""
from __future__ import annotations

import os
import re
import subprocess
from typing import Callable

from character_counts_ledger import LEDGER, compare, missing_rows, read_ledger
from character_counts_net import Site, sites

# The checker's planted cases, which are sites on purpose. The check reads its other files as it
# reads every file, and their sites need rows like any other.
SELF = (".github/scripts/character_counts_controls.py", ".github/scripts/character_counts_shapes.py")
# The checker's own files: a scan that found no site outside them has seen nothing.
CHECKER = re.compile(r"^\.github/(?:scripts/(?:check-character-counts\.sh|character_counts(?:_[a-z]+)?\.py)"
                     r"|character-counts\.ledger)$")

# The files the check leaves out, each class with its reason. A binary file, a symbolic link and
# a listed file that is not on disk are found by reading, so they need no line here.
DATED = ("a dated record: a changelog, design history or a learning",
         "it says what was true on its date, and a rewording to today's unit can make it false; "
         "AGENTS.md: never edit a CHANGELOG.md, each one is frozen history; docs/plans/ is design "
         "history, and docs/solutions/ stays as history")
FIXTURE = ("a fixture whose bytes are the subject",
           "a test or an eval reads it as it is: gh's own help text, a terminal recording, or the "
           "bodies and documents a test feeds in")
LEFT_OUT = [
    (re.compile(r"^\.github/scripts/character_counts_(?:controls|shapes)\.py$"), "the checker's own planted cases",
     "its controls are sites on purpose"),
    (re.compile(r"^\.github/character-counts\.ledger$"), "the ledger",
     "its rows are held to their schema and to the sites, not read by the net"),
    (re.compile(r"(?:^|/)CHANGELOG\.md$"), *DATED),
    (re.compile(r"^docs/plans/"), *DATED),
    (re.compile(r"^docs/superpowers/"), *DATED),
    (re.compile(r"^docs/research/"), *DATED),
    (re.compile(r"^docs/solutions/"), *DATED),
    (re.compile(r"^packages/daemon/cmd/legion/testdata/gh-help(?:-walk)?/"), *FIXTURE),
    (re.compile(r"\.cast$"), *FIXTURE),
    (re.compile(r"^packages/envoy/internal/dispatch/pmdoc/heavybodies/testdata/"), *FIXTURE),
    (re.compile(r"^packages/envoy/internal/dispatch/pmdoc/testdata/"), *FIXTURE),
    (re.compile(r"^scripts/approval-eval/fixtures/"), *FIXTURE),
]
BINARY = "a binary: it holds a NUL byte"
LINK = "a symbolic link: the check reads its target where git lists it"
MISSING = "listed by git but not on disk"


def classify(path: str, data: bytes | str | None) -> tuple[str, str] | None:
    """The class of the reason the check leaves path out and the rule that names it, or None when
    it reads it. data is the file's bytes, "link" for a symbolic link, or None when nothing is on
    disk."""
    for rule, kind, _ in LEFT_OUT:
        if rule.search(path):
            return kind, rule.pattern
    if data == "link":
        return LINK, ""
    if data is None:
        return MISSING, ""
    if b"\0" in data:
        return BINARY, ""
    return None


def git_listing(root: str) -> list[str]:
    """Every file git lists in the repository at root: tracked, and untracked but not ignored."""
    listed = subprocess.run(
        ["git", "ls-files", "-z", "--cached", "--others", "--exclude-standard"],
        cwd=root, capture_output=True, check=True,
    ).stdout.decode("utf-8", errors="replace").split("\0")
    return sorted({path for path in listed if path})


def disk_reader(root: str) -> Callable[[str], bytes | str | None]:
    """A reader of listed paths under root: a path's bytes, "link" for a symbolic link, or None
    when nothing is on disk."""

    def read_file(path: str) -> bytes | str | None:
        full = os.path.join(root, path)
        if os.path.islink(full):
            return "link"
        try:
            with open(full, "rb") as handle:
                return handle.read()
        except (FileNotFoundError, IsADirectoryError, NotADirectoryError):
            return None

    return read_file



def judge_tree(listing: list[str], read_file: Callable[[str], bytes | str | None],
               listed: bool = False) -> tuple[int, list[str], list[Site]]:
    """The exit code, the report and the sites of a scan over listing. read_file gives a path's
    bytes, "link" or None. With listed, the report starts with each site and its decision, and
    the start of the row each undecided site needs."""
    left: dict[str, dict[str, int]] = {}
    found: list[Site] = []
    texts: dict[str, str] = {}
    read = 0
    for path in listing:
        data = read_file(path)
        if isinstance(data, bytes):
            texts[path] = data.decode("utf-8", errors="replace")
        kind = classify(path, data)
        if kind:
            by_rule = left.setdefault(kind[0], {})
            by_rule[kind[1]] = by_rule.get(kind[1], 0) + 1
            continue
        read += 1
        found.extend(sites(path, data))
    reasons = {kind: reason for _, kind, reason in LEFT_OUT}
    total = sum(sum(by_rule.values()) for by_rule in left.values())
    out = [f"files: {len(listing)} listed, {read} read, {total} left out"]
    for kind, by_rule in sorted(left.items()):
        out.append(f"  left out, {kind}: {sum(by_rule.values())}" + (f" ({reasons[kind]})" if kind in reasons else ""))
        if len(by_rule) > 1:
            out += [f"    path {rule}: {count}" for rule, count in sorted(by_rule.items())]
    outside = [site for site in found if not CHECKER.search(site.path)]
    if read == 0 or not outside:
        out.append(f"UNKNOWN: read {read} files and found {len(outside)} sites outside the checker's own files: "
                   "a scan that saw nothing has not passed")
        return 1, out, found
    if LEDGER in texts:
        rows, problems = read_ledger(texts[LEDGER])
    else:
        rows, problems = [], [f"{LEDGER} is not in the listing"]
    problems += compare(found, rows, texts.get)
    if listed:
        decision = {row.key: f"{row.unit}, {row.counter} :: {row.reason}" for row in rows}
        out[:0] = [f"{s.path}:{s.line}: [{decision.get(s.key, 'no row')}] {s.text}" for s in found] + \
            [f"needs a row: {row}" for row in missing_rows(found, rows)]
    out += problems
    out.append(f"character counts: {read} files read, {len(found)} sites ({len(outside)} outside the checker's own "
               f"files), {len(rows)} rows, {len(problems)} problems")
    return (1 if problems else 0), out, found
