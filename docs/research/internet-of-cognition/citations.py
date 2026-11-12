#!/usr/bin/env python3
"""citations.py turns a record into citation rows, under one rule.

dispatch://INFRA-3134 derives the number of claims it checks from the two records it
checks, rather than choosing it. This file is that rule. `check.sh` calls it, and the
author's own inventory comes from `bash check.sh --coverage`, so one rule makes the
deliverable and the gate's own derivation of what the deliverable must cover.

    python3 citations.py extract --input <file> --label <name>
    python3 citations.py --self-test

`extract` prints one JSON object per line, ordered by first occurrence. Each object:

    cid          the citation id, "<label>-<n>"
    kind         one of: file, file-lines, symbol, number, dispatch, web
    text         the citation as the record writes it
    file         the repository-relative path, for kind file and file-lines
    lines        the line or line range, for kind file-lines
    symbol       the identifier, for kind symbol
    number       the number in digits, for kind number
    ref          the dispatch:// reference, for kind dispatch
    url          the https:// address, for kind web
    occurrences  every place it occurs: {"line": <n>, "section": "<heading>"}

The three readings this rule settles, each of which a reader could settle another way:

One. A citation that occurs more than once is one row, and the row lists every place it
occurs. So the gate's rule, that every citation has a row of claims.jsonl with a mark,
holds under this reading: one row per citation, not one per occurrence.

Two. A reference that carries a line range and no file, such as `:188-196`, continues
the nearest file citation before it in the same section. The rule resolves it and the row
carries the resolved file, so the row is a file-lines row like any other.

Three. A number written in words is a number. The rule reads `thirteen normative
invariants`, `nineteen fields` and `thirty-four tables` as measured numbers, from the word
list below, beside the digits. `--self-test` plants one word-number, so an extractor that
reads digits alone fails there.

What makes a number a citation, and what does not. A number is a citation when it carries
a per-cent sign, when the word right after it is an ordinary word (a unit or the thing
counted), or when it is a decimal below one, which in these records is a score or a
threshold. A number inside a code span, inside an address, inside a date, inside a commit
hash and inside a line range is not a citation: the code span, the address and the file
citation are the citation there. This is the whole of the rule, and a number it does not
read is read by nobody, which is what `--coverage` prints.
"""

from __future__ import annotations

import argparse
import json
import re
import sys

WORD_NUMBERS = {
    "one": 1,
    "two": 2,
    "three": 3,
    "four": 4,
    "five": 5,
    "six": 6,
    "seven": 7,
    "eight": 8,
    "nine": 9,
    "ten": 10,
    "eleven": 11,
    "twelve": 12,
    "thirteen": 13,
    "fourteen": 14,
    "fifteen": 15,
    "sixteen": 16,
    "seventeen": 17,
    "eighteen": 18,
    "nineteen": 19,
    "twenty": 20,
    "thirty": 30,
    "forty": 40,
    "fifty": 50,
    "sixty": 60,
    "seventy": 70,
    "eighty": 80,
    "ninety": 90,
    "hundred": 100,
    "thousand": 1000,
}

TENS = {"twenty": 20, "thirty": 30, "forty": 40, "fifty": 50, "sixty": 60, "seventy": 70, "eighty": 80, "ninety": 90}
UNITS = {
    "one": 1,
    "two": 2,
    "three": 3,
    "four": 4,
    "five": 5,
    "six": 6,
    "seven": 7,
    "eight": 8,
    "nine": 9,
}

# A word after a number that makes it no measurement: these are the words of a sentence,
# not of a unit.
NUMBER_STOPWORDS = {
    "of",
    "in",
    "on",
    "at",
    "to",
    "and",
    "or",
    "but",
    "the",
    "a",
    "an",
    "is",
    "was",
    "are",
    "were",
    "that",
    "which",
    "with",
    "for",
    "from",
    "by",
    "as",
    "it",
    "its",
    "we",
    "they",
    "their",
    "our",
    "no",
    "not",
    "than",
    "then",
    "so",
    "if",
    "when",
    "where",
    "while",
}

CODE_SPAN = re.compile(r"`([^`]+)`")
DISPATCH_REF = re.compile(r"dispatch://[A-Za-z0-9][A-Za-z0-9/_.#-]*")
WEB_URL = re.compile(r"https?://[^\s)`\"'>]+")
HEADING = re.compile(r"^(#{1,6})\s+(.*\S)\s*$")
# A path: at least one dot in its last element, or a slash, and no space. A line or a
# line range may ride after it, so the colon and the comma are part of the token.
PATH_TOKEN = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_./@+:,-]*$")
LINES_SUFFIX = re.compile(r":(\d+(?:[-,]\d+)*)$")
BARE_LINES = re.compile(r"^:(\d+(?:[-,]\d+)*)$")
IDENTIFIER = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)+$")
DIGIT_NUMBER = re.compile(r"(?<![\w.])(\d{1,3}(?:,\d{3})+|\d+(?:\.\d+)?)(%?)")
DATE = re.compile(r"\d{4}-\d{2}-\d{2}")
HEX40 = re.compile(r"\b[0-9a-f]{40}\b")
HEX_SHORT = re.compile(r"\b[0-9a-f]{7,40}\b")


def word_number(words: list[str]) -> tuple[int, int] | None:
    """Read a number written in words at the head of `words`.

    Returns the value and how many words it took, or None.
    """
    if not words:
        return None
    first = words[0].lower().strip(",.;:")
    if first in TENS:
        if len(words) > 1:
            second = words[1].lower().strip(",.;:")
            if "-" in words[0]:
                pass
            if second in UNITS:
                return TENS[first] + UNITS[second], 2
        return TENS[first], 1
    if "-" in first:
        head, _, tail = first.partition("-")
        if head in TENS and tail in UNITS:
            return TENS[head] + UNITS[tail], 1
    if first in WORD_NUMBERS:
        return WORD_NUMBERS[first], 1
    return None


class Extractor:
    def __init__(self, label: str) -> None:
        self.label = label
        self.rows: dict[str, dict] = {}
        self.order: list[str] = []
        self.counter = 0

    def key(self, kind: str, text: str) -> str:
        return f"{kind}\u0000{text}"

    def add(self, kind: str, text: str, line_no: int, section: str, **fields) -> None:
        key = self.key(kind, text)
        row = self.rows.get(key)
        if row is None:
            self.counter += 1
            row = {
                "cid": f"{self.label}-{self.counter}",
                "kind": kind,
                "text": text,
                "occurrences": [],
            }
            row.update(fields)
            self.rows[key] = row
            self.order.append(key)
        row["occurrences"].append({"line": line_no, "section": section})

    def extract(self, text: str) -> list[dict]:
        section = ""
        last_file: dict[str, str | None] = {"section": "", "file": None}
        for line_no, raw in enumerate(text.splitlines(), start=1):
            heading = HEADING.match(raw)
            if heading:
                section = heading.group(2)
                continue
            masked = raw
            for ref in DISPATCH_REF.findall(raw):
                cleaned = ref.rstrip(".,;:)")
                self.add("dispatch", cleaned, line_no, section, ref=cleaned)
                masked = masked.replace(ref, " " * len(ref))
            for url in WEB_URL.findall(raw):
                cleaned = url.rstrip(".,;:)")
                if cleaned.startswith("dispatch://"):
                    continue
                self.add("web", cleaned, line_no, section, url=cleaned)
                masked = masked.replace(url, " " * len(url))
            for span in CODE_SPAN.findall(masked):
                self.read_code_span(span, line_no, section, last_file)
            masked = CODE_SPAN.sub(lambda m: " " * (len(m.group(1)) + 2), masked)
            self.read_numbers(masked, line_no, section)
        return [self.rows[key] for key in self.order]

    def read_code_span(self, span: str, line_no: int, section: str, last_file: dict) -> None:
        token = span.strip().strip("()[]")
        if not token or " " in token:
            return
        bare = BARE_LINES.match(token)
        if bare:
            if last_file["section"] == section and last_file["file"]:
                resolved = f"{last_file['file']}:{bare.group(1)}"
                self.add(
                    "file-lines",
                    resolved,
                    line_no,
                    section,
                    file=last_file["file"],
                    lines=bare.group(1),
                    continued=True,
                )
            else:
                self.add("file-lines", token, line_no, section, file=None, lines=bare.group(1), continued=True)
            return
        if not PATH_TOKEN.match(token):
            return
        if HEX40.fullmatch(token) or DATE.fullmatch(token):
            return
        lines = LINES_SUFFIX.search(token)
        path = token[: lines.start()] if lines else token
        looks_like_path = "/" in path or re.search(r"\.[A-Za-z]{1,6}$", path)
        if looks_like_path:
            if lines:
                self.add("file-lines", token, line_no, section, file=path, lines=lines.group(1))
            else:
                self.add("file", path, line_no, section, file=path)
            last_file["section"] = section
            last_file["file"] = path
            return
        if IDENTIFIER.match(token):
            self.add("symbol", token, line_no, section, symbol=token)

    def read_numbers(self, text: str, line_no: int, section: str) -> None:
        masked = DATE.sub(lambda m: " " * len(m.group(0)), text)
        masked = HEX_SHORT.sub(lambda m: " " * len(m.group(0)), masked)
        for match in DIGIT_NUMBER.finditer(masked):
            raw = match.group(1)
            percent = match.group(2)
            after = masked[match.end() :]
            if percent:
                self.add("number", raw + percent, line_no, section, number=raw)
                continue
            value = float(raw.replace(",", ""))
            if 0 < value < 1 and "." in raw:
                self.add("number", raw, line_no, section, number=raw)
                continue
            follower = re.match(r"[-\s]+([A-Za-z][A-Za-z-]*)", after)
            if follower and follower.group(1).lower() not in NUMBER_STOPWORDS:
                self.add("number", f"{raw} {follower.group(1)}", line_no, section, number=raw)
        words = re.split(r"(\s+)", masked)
        plain = [w for w in words if w.strip()]
        for index, word in enumerate(plain):
            read = word_number(plain[index:])
            if read is None:
                continue
            value, taken = read
            follower_index = index + taken
            if follower_index >= len(plain):
                continue
            follower = plain[follower_index].strip(",.;:()").lower()
            if not follower or not follower[0].isalpha():
                continue
            if follower in NUMBER_STOPWORDS or follower in WORD_NUMBERS:
                continue
            text_of = " ".join(plain[index:follower_index] + [plain[follower_index].strip(",.;:()")])
            self.add("number", text_of, line_no, section, number=str(value))


SELF_TEST_INPUT = """\
# One

A claim about `packages/daemon/internal/api/handoff.go:160-166`, and the same file again
in `packages/daemon/internal/api/handoff.go:160-166`.

It then writes a bare range `:188-196` under that file, and names a symbol
`record.Postgres.Enqueue`.

It reports thirteen normative invariants, 85.2% of 350 runs, a score of 0.944, and
`https://example.org/a` and dispatch://INFRA-1098/spec.

# Two

A bare range `:12-14` under no file of this section.
"""


def self_test() -> int:
    extractor = Extractor("t")
    rows = extractor.extract(SELF_TEST_INPUT)
    failures: list[str] = []
    by_kind: dict[str, list[dict]] = {}
    for row in rows:
        by_kind.setdefault(row["kind"], []).append(row)

    def check(name: str, ok: bool, detail: str) -> None:
        print(f"{'PASS' if ok else 'FAIL'} citations {name}: {detail}")
        if not ok:
            failures.append(name)

    repeated = [r for r in rows if r["text"] == "packages/daemon/internal/api/handoff.go:160-166"]
    check(
        "a repeated citation is one row",
        len(repeated) == 1 and len(repeated[0]["occurrences"]) == 2,
        f"rows={len(repeated)} occurrences={len(repeated[0]['occurrences']) if repeated else 0}",
    )
    continued = [r for r in rows if r.get("continued") and r.get("lines") == "188-196"]
    check(
        "a bare line range continues the nearest file of its section",
        len(continued) == 1 and continued[0].get("file") == "packages/daemon/internal/api/handoff.go",
        f"file={continued[0].get('file') if continued else None}",
    )
    orphan = [r for r in rows if r.get("continued") and r.get("lines") == "12-14"]
    check(
        "a bare line range under no file resolves to no file",
        len(orphan) == 1 and orphan[0].get("file") is None,
        f"rows={len(orphan)}",
    )
    word = [r for r in by_kind.get("number", []) if r["text"].startswith("thirteen")]
    check(
        "a number written in words is a number",
        len(word) == 1 and word[0]["number"] == "13",
        f"rows={len(word)} number={word[0]['number'] if word else None}",
    )
    digits = {r["number"] for r in by_kind.get("number", [])}
    check("a per-cent number is a number", "85.2" in digits, f"numbers={sorted(digits)}")
    check("a counted number is a number", "350" in digits, f"numbers={sorted(digits)}")
    check("a score below one is a number", "0.944" in digits, f"numbers={sorted(digits)}")
    check(
        "a symbol is a row",
        any(r["symbol"] == "record.Postgres.Enqueue" for r in by_kind.get("symbol", [])),
        f"symbols={[r['symbol'] for r in by_kind.get('symbol', [])]}",
    )
    check(
        "a dispatch reference is a row",
        any(r["ref"] == "dispatch://INFRA-1098/spec" for r in by_kind.get("dispatch", [])),
        "dispatch://INFRA-1098/spec",
    )
    check(
        "a web address is a row",
        any(r["url"] == "https://example.org/a" for r in by_kind.get("web", [])),
        "https://example.org/a",
    )
    check(
        "a line number inside a file citation is no number of its own",
        "160" not in digits and "166" not in digits,
        f"numbers={sorted(digits)}",
    )
    if failures:
        print(f"citations.py --self-test: {len(failures)} of 11 assertions failed")
        return 1
    print("all 11 assertions passed")
    return 0


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("command", nargs="?", default="extract", choices=["extract"])
    parser.add_argument("--input", help="the record to read, or - for standard input")
    parser.add_argument("--label", default="x", help="the prefix of each citation id")
    parser.add_argument("--self-test", action="store_true", help="prove the three readings above")
    args = parser.parse_args()
    if args.self_test:
        return self_test()
    if not args.input:
        parser.error("--input is required")
    if args.input == "-":
        text = sys.stdin.read()
    else:
        with open(args.input, encoding="utf-8") as handle:
            text = handle.read()
    for row in Extractor(args.label).extract(text):
        print(json.dumps(row, ensure_ascii=False, sort_keys=True))
    return 0


if __name__ == "__main__":
    sys.exit(main())
