#!/usr/bin/env bash
# No message states a length in characters that its counter does not count as characters.
#
# A character here is one Unicode code point. Dispatch's caps count UTF-16 code units
# (packages/contracts/AGENTS.md), where a character outside the Basic Multilingual Plane counts
# two, and Go's len() counts bytes, where every character outside ASCII counts two to four. A
# refusal that says "a title is at most 1000 characters, and this one is 1002" over a title of
# 501 emoji tells the writer something false. So a message gives its numbers with no unit
# ("title: 1002 of at most 1000"), or names the unit it counts ("UTF-16 code units").
#
# The check reads every tracked file, and every untracked one git does not ignore, of Go,
# TypeScript, JavaScript, Python and shell, outside test code (`_test.go`, `.test.*`, `.e2e.ts`,
# `__tests__/`, `testdata/`, `fixtures/`, `e2e/`) and generated code (`.generated.ts`,
# `_generated.go`, `dist/`), and outside this file, whose controls are instances on purpose. In
# each it reads the string literals, and in `.tsx` the JSX text, outside comments. A count beside
# the word ("%d characters", "2,000 characters", "%d-character", "${n} characters", "at character
# %d") is a hit, and a hit passes when one of these holds, in this order:
#
#   names its unit   the 40 characters after the word name UTF-16 or code units or points:
#                    "32,000 characters (UTF-16 units)" tells the truth.
#   ASCII grammar    the message names a grammar that admits ASCII alone (hex, a-z, base64,
#                    uuid, SHA, DNS, ASCII, "lowercase letters, digits", "40-character commit"),
#                    where bytes, code points and UTF-16 units are one number.
#   exempt           a comment on the line or the two lines above reads
#                    `characters: code points, <reason>` or `characters: ASCII alone, <reason>`:
#                    the counter is elsewhere, and the comment says where, at the site. A marker
#                    with no reason passes nothing.
#   code points      the call holding the literal, else the `if` or `case` it answers, else the
#                    Go function around it, counts with utf8.RuneCount, a []rune or [...s].length
#                    and with no other counter (Units16, utf16, len(), .length).
#
# Every other hit is an instance, and the check fails naming each one. It also fails when it read
# no file or found no hit at all, since a scan that saw nothing has not passed.
#
# Eight controls run first on every run, through the same code: a planted message counting
# UTF-16 units in packages/daemon and one counting .length in packages/envoy-client must fire, a
# "40-character lowercase hex commit" and a "characters (UTF-16 units)" must not, a refusal under
# utf8.RuneCountInString must not, a comment is no hit, a count beside a word chosen in an
# expression or joined to it with + must fire, and an exempt site fires once its comment is
# removed or its reason emptied. A control that answers otherwise fails the check.
#
# Run from anywhere: .github/scripts/check-character-counts.sh [--list]
# --list prints every hit with its verdict. CI runs it in ci.yaml's lint job.
set -euo pipefail

cd "$(dirname "$0")/../.."

python3 - "$@" <<'PY'
from __future__ import annotations

import re
import subprocess
import sys
from dataclasses import dataclass

SELF = ".github/scripts/check-character-counts.sh"
COUNT = r"(?:%[-+0-9.]*[dv]|\$\{[^}]*\}|\{[^}]*\}|\$\w+|\b\d[\d,]*)"
IN_LITERAL = re.compile(COUNT + r"[- ]characters?\b|\bcharacters?\s+" + COUNT)
IN_TEXT = re.compile(r"(?:\{\}|\b\d[\d,]*)[- ]characters?\b")
# The word chosen in an expression beside its count, `{n} {n === 1 ? "character" : "characters"}`
# or `${n === 1 ? ...}`, reads as the word itself; and a count joined to a literal that opens with
# the word, `n + " characters"`, is a count beside it.
PLURAL = re.compile(r"\$?\{[^{}]*?([\"'`])characters?\1[^{}]*\}")
JOINED = re.compile(r"[\w)\]]\s*\+\s*([\"'`])\s?characters?\b")
NAMES_UNIT = re.compile(r"UTF-16|code units?\b|code points?\b", re.I)
ASCII_GRAMMAR = re.compile(
    r"\bhex\b|\ba-z\b|\[0-9a-z\]|\bbase64\b|\buuids?\b|\bSHA\b|\bsha(?:256)?\b|\bDNS\b|\bASCII\b"
    r"|lowercase letters(?: and|,) digits|\d-character (?:lower-?case )?(?:hex )?commit\b"
)
EXEMPT = re.compile(r"(?://|#)\s*characters:\s*(code points|ASCII alone)\b[,:;]?\s*(\S.{9,})?")
CODE_POINTS = re.compile(r"utf8\.RuneCount|\[\]rune\(|\[\.\.\.[^\]]+\]\.length|Array\.from\([^)]*\)\.length")
OTHER_COUNTS = re.compile(r"\bUnits16\b|\butf16\.|UTF16|\blen\(|\.length\b")
SOURCES = (".go", ".ts", ".tsx", ".js", ".mjs", ".cjs", ".py", ".sh", ".bash")
TEST_CODE = re.compile(
    r"(?:_test\.go|\.test\.[cm]?[jt]sx?|\.spec\.[jt]sx?|\.e2e\.ts|\.test\.sh)$"
    r"|(?:^|/)(?:__tests__|testdata|fixtures|e2e)/"
)
GENERATED = re.compile(r"(?:\.generated\.ts|_generated\.go|\.pb\.go)$|(?:^|/)dist/")
REGEX_BEFORE = set("(,=:[!&|?{};")


@dataclass
class Hit:
    path: str
    line: int
    text: str
    verdict: str


def language(path: str) -> str:
    if path.endswith(".go"):
        return "go"
    if path.endswith(".py"):
        return "py"
    if path.endswith((".sh", ".bash")):
        return "sh"
    return "ts"


def regex_starts(code: list[str]) -> bool:
    """Whether a `/` after this code opens a regular expression rather than dividing."""
    before = "".join(code).rstrip()
    return before == "" or before[-1] in REGEX_BEFORE or before.endswith("return")


def literals(line: str, lang: str, state: dict) -> tuple[list[str], str]:
    """The string literals on line, a template literal carried across lines in state, and the
    code outside them with each `{...}` written `{}`, which is how JSX text is read."""
    out: list[str] = []
    code: list[str] = []
    current: list[str] = []
    quotes = {"go": '"`', "ts": "\"'`", "py": "\"'", "sh": "\"'"}[lang]
    delim = state.get("open")
    i, n = 0, len(line)
    while i < n:
        ch = line[i]
        if delim is None:
            if lang in ("go", "ts") and line.startswith("//", i):
                break
            if lang in ("py", "sh") and ch == "#" and (i == 0 or line[i - 1] in " \t;("):
                break
            if lang == "ts" and ch == "/" and regex_starts(code):
                j, in_class = i + 1, False
                while j < n:
                    if line[j] == "\\":
                        j += 2
                        continue
                    if line[j] == "[":
                        in_class = True
                    elif line[j] == "]":
                        in_class = False
                    elif line[j] == "/" and not in_class:
                        break
                    j += 1
                code.append(" ")
                i = j + 1
                continue
            if ch in quotes:
                delim, current = ch, []
            else:
                code.append(ch)
            i += 1
            continue
        if ch == "\\" and (delim != "`" or lang == "ts"):
            current.append(line[i : i + 2])
            i += 2
            continue
        if lang == "ts" and delim == "`" and line.startswith("${", i):
            depth, j = 0, i
            while j < n:
                depth += {"{": 1, "}": -1}.get(line[j], 0)
                if depth == 0 and line[j] == "}":
                    break
                j += 1
            current.append(line[i : j + 1])
            i = j + 1
            continue
        if ch == delim:
            out.append("".join(current))
            delim = None
            i += 1
            continue
        current.append(ch)
        i += 1
    if delim == "`":
        out.append("".join(current))
        state["open"] = delim
    else:
        state["open"] = None
    return out, re.sub(r"\{[^{}]*\}", "{}", "".join(code))


def go_functions(lines: list[str]) -> list[tuple[int, int]]:
    spans, start = [], None
    for index, line in enumerate(lines):
        if line.startswith("func "):
            start = index
        if start is not None and line == "}":
            spans.append((start, index))
            start = None
    return spans


def regions(lines: list[str], index: int) -> tuple[str, str]:
    """The literal's call (its line and the lines that close its parentheses), and the `if` or
    `case` header it answers, "" when it answers none."""
    header = ""
    for back in range(index - 1, max(index - 5, -1), -1):
        text = lines[back].strip()
        if text.endswith(("{", ":")) and re.match(r"(?:\}\s*else\s+)?(?:if|case|for|switch)\b|if\s*\(", text):
            header = lines[back]
            break
        if text.endswith(("{", "}", ";")) or not text:
            break
    last, depth = index, 0
    for forward in range(index, min(index + 6, len(lines))):
        depth += lines[forward].count("(") - lines[forward].count(")")
        last = forward
        if depth <= 0:
            break
    return "\n".join(lines[index : last + 1]), header


def counts(region: str) -> str:
    """"code points", "other" or "" for a region of code, by the counters it calls."""
    if OTHER_COUNTS.search(CODE_POINTS.sub("", region)):
        return "other"
    return "code points" if CODE_POINTS.search(region) else ""


def classify(path: str, lines: list[str], index: int, text: str, match: re.Match, spans) -> str:
    if NAMES_UNIT.search(text[match.start() : match.end() + 40]):
        return "names its unit"
    if ASCII_GRAMMAR.search(text[max(0, match.start() - 120) : match.end() + 120]):
        return "ASCII grammar"
    for above in lines[max(0, index - 2) : index + 1]:
        exempt = EXEMPT.search(above)
        if exempt and exempt.group(2):
            return f"exempt, {exempt.group(1)}: {exempt.group(2).strip()}"
    call, header = regions(lines, index)
    for region in (call, header):
        found = counts(region)
        if found:
            return "code points" if found == "code points" else "instance"
    if path.endswith(".go"):
        span = next(((a, b) for a, b in spans if a <= index <= b), None)
        if span and counts("\n".join(lines[span[0] : span[1] + 1])) == "code points":
            return "code points"
    return "instance"


def scan(path: str, content: str) -> list[Hit]:
    lang = language(path)
    lines = content.splitlines()
    spans = go_functions(lines) if lang == "go" else []
    hits: list[Hit] = []
    state: dict = {}
    for index, line in enumerate(lines):
        stripped = line.lstrip()
        if not state.get("open") and (stripped.startswith(("//", "/*", "* ", "*/")) or stripped == "*"):
            continue
        # The line as it reads, with a word chosen in an expression written as the word.
        reads = PLURAL.sub("characters", line)
        found, bare = literals(reads, lang, state)
        candidates = [(text, match) for text in found for match in IN_LITERAL.finditer(text)]
        if path.endswith(".tsx"):
            candidates += [(bare, match) for match in IN_TEXT.finditer(bare)]
        joined = JOINED.search(reads.split("//", 1)[0] if lang in ("go", "ts") else reads)
        if joined:
            candidates.append((reads, joined))
        for text, match in candidates:
            verdict = classify(path, lines, index, text, match, spans)
            hits.append(Hit(path, index + 1, text.strip()[:160], verdict))
    return hits


EXEMPT_SITE = (
    "package planted\n\n"
    "func r(n int) error {\n"
    "{marker}"
    '\treturn fmt.Errorf("cut %d characters", n)\n'
    "}\n"
)
CONTROLS = [
    ("a planted UTF-16 count in packages/daemon fires", "packages/daemon/internal/planted/planted.go",
     'package planted\n\nfunc f(name string) string {\n'
     '\treturn fmt.Sprintf("name is %d characters", itemblocks.Units16(name))\n}\n', ["instance"]),
    ("a planted .length count in packages/envoy-client fires", "packages/envoy-client/src/planted.ts",
     "export const cut = (text: string) => `cut at ${text.length} characters`;\n", ["instance"]),
    ("an ASCII-only grammar stays quiet", "packages/envoy/internal/planted/ascii.go",
     'package planted\n\nvar e = errors.New("commit is the 40-character lowercase hex commit the check ran at")\n',
     ["ASCII grammar"]),
    ("a message naming its unit stays quiet", "packages/envoy-client/src/unit.ts",
     "export const line = (n: number) => `One block: ${n} characters (UTF-16 units).`;\n", ["names its unit"]),
    ("a count in code points stays quiet", "packages/envoy/internal/planted/runes.go",
     'package planted\n\nfunc g(id string) string {\n\tif n := utf8.RuneCountInString(id); n > 200 {\n'
     '\t\treturn fmt.Sprintf("an id is at most 200 characters, and this one is %d", n)\n\t}\n\treturn ""\n}\n',
     ["code points"]),
    ("a comment is no hit", "packages/envoy-client/src/comment.ts",
     "// a 40-character name, in a comment\nconst re = /[`]/g;\n", []),
    ("a count beside a word chosen in an expression, or joined to it with +, fires",
     "packages/dispatch/web/src/planted/Counter.tsx",
     "const left = MAX - note.length;\n"
     'const view = <p>\n  {left} {left === 1 ? "character" : "characters"} left\n</p>;\n'
     'const said = "cut at " + note.length + " characters";\n',
     ["instance", "instance"]),
    ("an exempt site stays quiet, and fires once its comment or its reason is gone", None, None, None),
]


def control(name, path, content, want) -> str | None:
    if path is not None:
        got = [hit.verdict for hit in scan(path, content)]
        return None if got == want else f"{name}: answered {got}, want {want}"
    path = "packages/envoy/internal/planted/exempt.go"
    readings = {
        "\t// characters: code points, the caller counts n with utf8.RuneCountInString\n": "exempt, code points",
        "": "instance",
        "\t// characters: code points\n": "instance",
    }
    for marker, want_verdict in readings.items():
        got = [hit.verdict for hit in scan(path, EXEMPT_SITE.replace("{marker}", marker))]
        if len(got) != 1 or not got[0].startswith(want_verdict):
            return f"{name}: with {marker.strip() or 'no comment'!r} answered {got}, want {want_verdict}"
    return None


def main() -> int:
    failed = [problem for problem in (control(*c) for c in CONTROLS) if problem]
    if failed:
        print("CONTROLS FAILED: " + "; ".join(failed))
        return 1
    print(f"controls: {len(CONTROLS)} of {len(CONTROLS)} answered as they must")
    for name, *_ in CONTROLS:
        print(f"  ok  {name}")
    listed = subprocess.run(
        ["git", "ls-files", "--cached", "--others", "--exclude-standard"],
        capture_output=True, text=True, check=True,
    ).stdout.splitlines()
    files = [
        path for path in listed
        if path.endswith(SOURCES) and path != SELF and not TEST_CODE.search(path) and not GENERATED.search(path)
    ]
    hits: list[Hit] = []
    read = 0
    for path in files:
        try:
            with open(path, encoding="utf-8", errors="replace") as handle:
                content = handle.read()
        except (FileNotFoundError, IsADirectoryError):
            continue
        read += 1
        hits.extend(scan(path, content))
    if "--list" in sys.argv[1:]:
        for hit in hits:
            print(f"{hit.path}:{hit.line}: [{hit.verdict}] {hit.text}")
    instances = [hit for hit in hits if hit.verdict == "instance"]
    if read == 0 or not hits:
        print(f"UNKNOWN: read {read} files and found {len(hits)} counts beside the word: a scan that saw nothing has not passed")
        return 1
    for hit in instances:
        print(f"::error file={hit.path},line={hit.line}::{hit.path}:{hit.line} states a length in "
              f"characters that its counter does not count as characters: {hit.text}")
    print(f"character counts: {read} files read, {len(hits)} counts beside the word, {len(instances)} instances")
    if instances:
        print(
            "Give the numbers with no unit (\"title: 1002 of at most 1000\"), or name the unit the "
            "counter counts (\"UTF-16 code units\", \"bytes\"). Where the counter is elsewhere, write "
            "`characters: code points, <where>` or `characters: ASCII alone, <why>` in a comment on "
            "the line or the two lines above.",
            file=sys.stderr,
        )
        return 1
    return 0


sys.exit(main())
PY
