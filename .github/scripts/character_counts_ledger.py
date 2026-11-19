"""The ledger of the character-count check (.github/character-counts.ledger): one reviewed row per
site the net finds, and the comparison of the rows with the sites."""
from __future__ import annotations

import re
from collections import Counter
from dataclasses import dataclass
from typing import Callable

from character_counts_net import Site

LEDGER = ".github/character-counts.ledger"
HEADER = "path\tline sha-256\toccurrence\tcopy\tcounter\tunit\treason"
# What the counter counts. A row whose unit is bytes or UTF-16 code units fails: its line calls that
# count characters, so the message must name the unit it counts instead.
UNITS = ("code points", "bytes", "UTF-16 code units", "not a length")
FALSE_UNITS = ("bytes", "UTF-16 code units")
SHA = re.compile(r"[0-9a-f]{64}")
ORDINAL = re.compile(r"[1-9][0-9]*")


@dataclass(frozen=True)
class Row:
    """One decision: the site it is for, the counter that computes the count (a `path:symbol`, or
    `none` where no code counts it), the unit that counter counts, and why."""

    path: str
    sha: str
    occurrence: int
    copy: int
    counter: str
    unit: str
    reason: str
    line: int

    @property
    def key(self) -> tuple[str, str, int, int]:
        return (self.path, self.sha, self.occurrence, self.copy)


def read_ledger(text: str) -> tuple[list[Row], list[str]]:
    """The rows of a ledger's text, and a problem for each line that is not a row of its schema.
    Blank lines and lines that start with # are comments; the first other line is the header."""
    rows: list[Row] = []
    problems: list[str] = []
    header_seen = False
    for number, line in enumerate(text.split("\n"), 1):
        if not line.strip() or line.startswith("#"):
            continue
        if not header_seen:
            header_seen = True
            if line != HEADER:
                problems.append(f"{LEDGER}:{number}: the header is not {HEADER!r}")
            continue
        fields = line.split("\t")
        if len(fields) != 7:
            problems.append(f"{LEDGER}:{number}: a row has 7 tab-separated fields, this one {len(fields)}")
            continue
        path, sha, occurrence, copy, counter, unit, reason = fields
        wrong = []
        if not SHA.fullmatch(sha):
            wrong.append("its line sha-256 is not 64 lower-case hex digits")
        if not ORDINAL.fullmatch(occurrence) or not ORDINAL.fullmatch(copy):
            wrong.append("its occurrence and copy are not whole numbers from 1")
        if counter != "none" and not re.fullmatch(r"[^\s:]+:\S+", counter):
            wrong.append("its counter is not `none` or `<path>:<symbol>`")
        if unit not in UNITS:
            wrong.append(f"its unit is not one of {', '.join(UNITS)}")
        if len(reason.split()) < 4 or not reason.endswith("."):
            wrong.append("its reason is not a sentence of at least four words ending in a period")
        if wrong:
            problems.append(f"{LEDGER}:{number}: " + "; ".join(wrong))
            continue
        rows.append(Row(path, sha, int(occurrence), int(copy), counter, unit, reason, number))
    if not header_seen:
        problems.append(f"{LEDGER}: no header line {HEADER!r}")
    return rows, problems


def compare(sites: list[Site], rows: list[Row], file_text: Callable[[str], str | None]) -> list[str]:
    """A problem for each site with no row, each row with no site, each duplicate row, each row
    whose unit is not the code points its line calls characters, and each counter the tree does
    not hold. file_text gives a listed file's text, or None for a path the tree does not hold."""
    problems: list[str] = []
    by_key = {site.key: site for site in sites}
    counts = Counter(row.key for row in rows)
    for row in rows:
        if counts[row.key] > 1:
            problems.append(f"{LEDGER}:{row.line}: a duplicate row: {counts[row.key]} rows decide {row.path}, "
                            f"occurrence {row.occurrence}, copy {row.copy} of the line {row.sha[:12]}")
        site = by_key.get(row.key)
        if site is None:
            where = "the check reads no file at that path" if file_text(row.path) is None else \
                "no line of the file has this text with this occurrence and copy: the line changed, moved past a copy or went"
            problems.append(f"{LEDGER}:{row.line}: a row with no site ({row.path}, line {row.sha[:12]}, "
                            f"occurrence {row.occurrence}, copy {row.copy}): {where}")
            continue
        if row.unit in FALSE_UNITS:
            problems.append(f"::error file={site.path},line={site.line}::{site.path}:{site.line} calls a count of "
                            f"{row.unit} {site.word} ({LEDGER}:{row.line}): name the unit the counter counts: {site.text}")
        if row.counter != "none":
            counter_path, symbol = row.counter.split(":", 1)
            text = file_text(counter_path)
            if text is None or symbol not in text:
                problems.append(f"{LEDGER}:{row.line}: its counter {row.counter} is not in the tree "
                                f"({'no such file' if text is None else 'the file does not hold the symbol'})")
    decided = set(counts)
    for site in sites:
        if site.key not in decided:
            problems.append(f"::error file={site.path},line={site.line}::{site.path}:{site.line} has a count beside "
                            f"\"{site.word}\" (occurrence {site.occurrence}) that no row of {LEDGER} decides: {site.text}")
    return problems


def missing_rows(sites: list[Site], rows: list[Row]) -> list[str]:
    """The key fields of each site with no row, as the start of the row it needs, for --list."""
    decided = {row.key for row in rows}
    return [f"{s.path}\t{s.sha}\t{s.occurrence}\t{s.copy}\t<counter>\t<unit>\t<reason>" for s in sites if s.key not in decided]
