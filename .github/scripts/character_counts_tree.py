"""The walk of the character-count check over the files git lists: the classes of files it leaves
out, each with its reason, and the report of what it read, left out and found."""
from __future__ import annotations

import re

from character_counts_analysis import Hit, scan

# The checker's planted cases: the one file of the check it leaves out. Its other files hold
# no count beside the word, and it reads them as it reads every file.
SELF = ".github/scripts/character_counts_controls.py"

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
    (re.compile(r"^\.github/scripts/character_counts_controls\.py$"), "the checker's own planted cases",
     "its controls are instances on purpose"),
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


def judge_tree(listing: list[str], read_file) -> tuple[int, list[str], list[Hit]]:
    """The exit code, the report and the hits of a scan over listing."""
    left: dict[str, dict[str, int]] = {}
    hits: list[Hit] = []
    read = 0
    for path in listing:
        data = read_file(path)
        found = classify(path, data)
        if found:
            by_rule = left.setdefault(found[0], {})
            by_rule[found[1]] = by_rule.get(found[1], 0) + 1
            continue
        read += 1
        hits.extend(scan(path, data.decode("utf-8", errors="replace")))
    instances = [hit for hit in hits if hit.verdict == "instance"]
    reasons = {kind: reason for _, kind, reason in LEFT_OUT}
    total = sum(sum(by_rule.values()) for by_rule in left.values())
    out = [f"files: {len(listing)} listed, {read} read, {total} left out"]
    for kind, by_rule in sorted(left.items()):
        out.append(f"  left out, {kind}: {sum(by_rule.values())}" + (f" ({reasons[kind]})" if kind in reasons else ""))
        if len(by_rule) > 1:
            out += [f"    path {rule}: {count}" for rule, count in sorted(by_rule.items())]
    if read == 0 or not hits:
        out.append(f"UNKNOWN: read {read} files and found {len(hits)} counts beside the word: a scan that saw nothing has not passed")
        return 1, out, hits
    for hit in instances:
        out.append(f"::error file={hit.path},line={hit.line}::{hit.path}:{hit.line} states a length in "
                   f"characters that its counter does not count as characters ({hit.why}): {hit.text}")
    out.append(f"character counts: {read} files read, {len(hits)} counts beside the word, {len(instances)} instances")
    return (1 if instances else 0), out, hits
