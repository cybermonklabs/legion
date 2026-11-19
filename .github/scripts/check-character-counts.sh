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
# The check reads every file git lists (tracked, and untracked but not ignored). It leaves a file
# out only for a reason of LEFT_OUT below, a binary file (one holding a NUL byte), or a listed
# file that is not on disk, and it prints how many files it read and how many it left out, by
# reason. It reads a file as the program that shows its text builds it:
#
#   Go, TypeScript and JavaScript, Python and shell: each string literal outside comments, with
#     the literals and values a `+` joins to it on any line read as one message (and Python's
#     adjacent literals), each interpolation (`${n}`, an f-string's `{n}`, shell's `$n`) read as
#     its value, JSX text in `.tsx` and `.jsx`, unquoted shell words, and each shell
#     here-document, whose body is read in the language of the program it feeds.
#   Every other text file (markdown, YAML, JSON, plain text and the rest): its whole text outside
#     its comments, as prose a person or a model reads.
#
# A count beside the word is a hit: "%d characters", "2,000 characters", "%d-character",
# "${n} chars", "at character %d", "chars=%d". "char" and "chars" are the same word. A hit
# passes when one of these holds:
#
#   names its unit   the 40 characters after the word, inside its sentence, name UTF-16 or code
#                    units or points: "32,000 characters (UTF-16 units)" tells the truth.
#   ASCII grammar    the sentence that holds the count names a grammar that admits ASCII alone
#                    (hex, a-z, base64, uuid, SHA, DNS, ASCII, "lowercase letters, digits",
#                    "40-character commit"), where bytes, code points and UTF-16 units are one
#                    number.
#   exempt           a comment on the line or the two lines above the message reads
#                    `characters: code points, <reason>` or `characters: ASCII alone, <reason>`:
#                    the counter is where the check cannot follow it, and the comment says where.
#                    A marker with no reason passes nothing.
#   code points      the count's own value is counted with utf8.RuneCount, a []rune,
#                    [...s].length or Array.from(s).length, or shell's ${#s} or `wc -m` in a
#                    script that sets a UTF-8 locale itself, and with no other counter. The check
#                    follows the value, not the code around it: a format verb's own argument, an
#                    interpolation's expression, a number's bound in the `if` or `case` the message
#                    answers, and each name in them to its assignment in the same file.
#
# Every other hit is an instance, and the check fails naming each one with its reason: the
# counter it found (len() counts bytes, .length and Units16 count UTF-16 code units), or that it
# cannot judge the count, since it cannot follow it to a counter (a value from a function or a
# file it does not follow, a bound with no counter beside it, a count in prose). It also fails,
# printing UNKNOWN, when it read no file or found no hit, since a scan that saw nothing has not
# passed.
#
# Its controls run first on every run, through the same code as the scan (CONTROLS below), and a
# control that answers otherwise fails the check: each shape the scan must fire on and each it
# must pass, one per entry of OTHER_COUNTERS, the files it reads and leaves out, and UNKNOWN.
#
# Run from anywhere: .github/scripts/check-character-counts.sh [--list]
# --list prints every hit with its verdict. CI runs it in ci.yaml's lint job.
set -euo pipefail

cd "$(dirname "$0")/../.."

python3 - "$@" <<'PY'
from __future__ import annotations

import os
import re
import subprocess
import sys
from dataclasses import dataclass

SELF = ".github/scripts/check-character-counts.sh"

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
    (re.compile(r"^\.github/scripts/check-character-counts\.sh$"), "the checker's own planted cases",
     "its controls are instances on purpose"),
    (re.compile(r"(?:^|/)CHANGELOG\.md$"), *DATED),
    (re.compile(r"^docs/plans/"), *DATED),
    (re.compile(r"^docs/superpowers/"), *DATED),
    (re.compile(r"^docs/research/"), *DATED),
    (re.compile(r"^docs/solutions/"), *DATED),
    (re.compile(r"^packages/daemon/cmd/legion/testdata/gh-help(?:-walk)?/"), *FIXTURE),
    (re.compile(r"\.cast$"), *FIXTURE),
    (re.compile(r"^packages/envoy/internal/dispatch/pmdoc/heavybodies/testdata/"), *FIXTURE),
    (re.compile(r"^scripts/approval-eval/fixtures/"), *FIXTURE),
]
BINARY = "a binary: it holds a NUL byte"
LINK = "a symbolic link: the check reads its target where git lists it"
MISSING = "listed by git but not on disk"

WORD = r"(?:[Cc]haracters?|[Cc]hars?)\b"
HAS_WORD = re.compile(WORD)
VERB = r"%(?:\[\d+\])?[-+ #0]*\d*(?:\.\d+)?[dvs]"
NUMBER = r"(?<![\w.+\-])\d[\d,]*(?<!,)"
SLOT = "\x00"
CODE_COUNT = rf"\x00\d+\x00|{VERB}|{NUMBER}"
PROSE_COUNT = rf"\$\{{[^{{}}\n]*\}}|\$\([^()\n]*\)|\{{[^{{}}\n]*\}}|\$#?\w+|{VERB}|{NUMBER}"


def hit_pattern(count: str) -> re.Pattern:
    return re.compile(
        rf"(?P<c1>{count})(?:-|\s+)(?P<w1>{WORD})(?!\s*=)"
        rf"|\b(?:at|after|before|from|past)\s+(?P<w2>{WORD})\s+(?P<c2>{count})"
        rf"|\b(?P<w3>{WORD})\s*[=:]\s*(?P<c3>{count})"
    )


CODE_HIT = hit_pattern(CODE_COUNT)
PROSE_HIT = hit_pattern(PROSE_COUNT)
PLURAL = re.compile(r"([\"'`])[Cc]haracters?\1")
NAMES_UNIT = re.compile(r"UTF-16|code units?\b|code points?\b", re.I)
ASCII_GRAMMAR = re.compile(
    r"\bhex\b|\ba-z\b|\[0-9a-z\]|\bbase64\b|\buuids?\b|\bSHA\b|\bsha(?:256)?\b|\bDNS\b|\bASCII\b"
    r"|lowercase letters(?: and|,) digits|\d-character (?:lower-?case )?(?:hex )?commit\b"
    r"|\[(?=[^\]]*[A-Za-z0-9]-[A-Za-z0-9])(?:[A-Za-z0-9]-[A-Za-z0-9]|[_.+=@/-])+\]"
)
EXEMPT = re.compile(r"(?://|#|<!--|--|/\*|\*)\s*characters:\s*(code points|ASCII alone)\b[,:;]?\s*(\S.{9,})?")
PROSE_SENTENCE_END = re.compile(r"[.;!?](?=\s|$)|\n\s*\n|\n\s*(?:[-*+]|\d+\.)\s|\|")
CODE_SENTENCE_END = re.compile(r"[.;!?](?=\s|$)|\n|\|")

# The counters, by language: the code-point counters, then each other counter with what it
# counts. A value holding a code-point counter and one of the others is an instance, and
# COUNTER_CASES below holds a planted case for each entry, written out apart from this list, so
# taking an entry off it fails that entry's case.
CODE_POINTS = {
    "go": re.compile(r"\butf8\.RuneCount(?:InString)?\(|\blen\(\[\]rune\(|\[\]rune\("),
    "ts": re.compile(r"\[\.\.\.[^\]]*\]\.length\b|\bArray\.from\("),
    "py": None,
    "sh": re.compile(r"\$\{#\w+\}|\bwc\s+-m\b"),
}
OTHER_COUNTERS = {
    "go": [("len()", r"\blen\(", "bytes"),
           ("Units16", r"\bUnits16\(", "UTF-16 code units"),
           ("utf16.", r"\butf16\.", "UTF-16 code units")],
    "ts": [(".length", r"\.length\b", "UTF-16 code units"),
           ("byteLength", r"\bbyteLength\b", "bytes")],
    "py": [],
    "sh": [("wc -c", r"\bwc\s+-c\b", "bytes")],
}
OTHER = {lang: [(label, re.compile(rx), unit) for label, rx, unit in entries] for lang, entries in OTHER_COUNTERS.items()}
# Calls that hand their argument's number on unchanged, so the count is that argument's.
PASS_THROUGH = re.compile(
    r"^(?:strconv\.Itoa|strconv\.FormatInt|int|int64|int32|uint|float64|String|Number|Math\.(?:max|min|round|floor|ceil|abs)"
    r"|fmt\.Sprint|str|toLocaleString|toString|toFixed)$"
)
KEYWORDS = set(
    "if else for range return true false nil null undefined this new typeof in of as const let var "
    "func go map make string int int64 uint byte rune error bool float64 len cap append await async "
    "and or not is None True False lambda throw then fi elif while do done case esac test local".split()
)


@dataclass
class Lit:
    start: int
    end: int
    parts: list  # ("text", text, offset) or ("expr", expression, offset)


@dataclass
class Hit:
    path: str
    line: int
    text: str
    verdict: str
    why: str = ""


def language(path: str, head: str) -> str:
    name = path.rsplit("/", 1)[-1]
    if name.endswith(".go"):
        return "go"
    if name.endswith((".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".mts", ".cts")):
        return "ts"
    if name.endswith(".py"):
        return "py"
    if name.endswith((".sh", ".bash", ".zsh")):
        return "sh"
    if head.startswith("#!"):
        first = head.split("\n", 1)[0]
        if re.search(r"\b(?:ba|z)?sh\b", first):
            return "sh"
        if "python" in first:
            return "py"
        if re.search(r"\b(?:node|bun|deno)\b", first):
            return "ts"
    return "prose"


# ---------------------------------------------------------------- the one lexical pass


def quoted_end(src: str, j: int, quote: str, escapes: bool, multiline: bool) -> int:
    """The index of the quote that closes the literal opened at j, or of the newline or end that cuts it."""
    k, n = j + 1, len(src)
    while k < n:
        c = src[k]
        if escapes and c == "\\":
            k += 2
            continue
        if c == quote or (c == "\n" and not multiline):
            return min(k, n)
        k += 1
    return n


def close_brace(src: str, k: int, lang: str) -> int:
    """The index of the `}` that closes an interpolation whose expression starts at k."""
    depth, n = 1, len(src)
    while k < n:
        c = src[k]
        if c in "\"'":
            k = quoted_end(src, k, c, True, False) + 1
            continue
        if c == "`" and lang == "ts":
            _, k, _ = template(src, k)
            continue
        if c == "{":
            depth += 1
        elif c == "}":
            depth -= 1
            if depth == 0:
                return k
        k += 1
    return n


def shift(lit: Lit, by: int) -> Lit:
    return Lit(lit.start + by, lit.end + by, [(kind, text, offset + by) for kind, text, offset in lit.parts])


def template(src: str, j: int):
    """The TypeScript template literal opened at j, the index after it, and the literals inside
    its interpolations."""
    parts, nested = [], []
    k, n = j + 1, len(src)
    run = k
    while k < n:
        c = src[k]
        if c == "\\":
            k += 2
            continue
        if c == "`":
            break
        if c == "$" and src.startswith("${", k):
            parts.append(("text", src[run:k], run))
            e = close_brace(src, k + 2, "ts")
            parts.append(("expr", src[k + 2 : e], k + 2))
            inner, _ = lex_ts(src[k + 2 : e])
            nested += [shift(lit, k + 2) for lit in inner]
            k = e + 1
            run = k
            continue
        k += 1
    parts.append(("text", src[run:min(k, n)], run))
    end = min(k + 1, n)
    return Lit(j, end, parts), end, nested


REGEX_BEFORE = set("(,=:[!&|?{};")


def regex_starts(src: str, j: int) -> bool:
    """Whether a `/` at j opens a regular expression rather than dividing."""
    k = j - 1
    while k >= 0 and src[k] in " \t\r\n":
        k -= 1
    return k < 0 or src[k] in REGEX_BEFORE or src[max(0, k - 5) : k + 1] == "return"


def regex_end(src: str, j: int) -> int:
    k, n, in_class = j + 1, len(src), False
    while k < n and src[k] != "\n":
        c = src[k]
        if c == "\\":
            k += 2
            continue
        if c == "[":
            in_class = True
        elif c == "]":
            in_class = False
        elif c == "/" and not in_class:
            k += 1
            while k < n and src[k].isalpha():
                k += 1
            return k
        k += 1
    return j + 1


def comment_end(src: str, j: int, block: bool) -> int:
    e = src.find("*/" if block else "\n", j + 2)
    return len(src) if e < 0 else (e + 2 if block else e)


def lex(src: str, lang: str):
    """Every string literal of src, and every range a comment holds."""
    return {"go": lex_go, "ts": lex_ts, "py": lex_py, "sh": lex_sh}[lang](src)


GO_START = re.compile(r"//|/\*|[\"`']")


def lex_go(src: str):
    lits, comments, i, n = [], [], 0, len(src)
    while (m := GO_START.search(src, i)) is not None:
        j, tok = m.start(), m.group()
        if tok in ("//", "/*"):
            i = comment_end(src, j, tok == "/*")
            comments.append((j, i))
            continue
        k = quoted_end(src, j, tok, tok != "`", tok == "`")
        lits.append(Lit(j, min(k + 1, n), [("text", src[j + 1 : k], j + 1)]))
        i = k + 1
    return lits, comments


TS_START = re.compile(r"//|/\*|[\"'`/]")


def lex_ts(src: str):
    lits, comments, i, n = [], [], 0, len(src)
    while (m := TS_START.search(src, i)) is not None:
        j, tok = m.start(), m.group()
        if tok in ("//", "/*"):
            i = comment_end(src, j, tok == "/*")
            comments.append((j, i))
            continue
        if tok == "/":
            i = regex_end(src, j) if regex_starts(src, j) else j + 1
            continue
        if tok == "`":
            lit, i, nested = template(src, j)
            lits.append(lit)
            lits += nested
            continue
        k = quoted_end(src, j, tok, True, False)
        closed = k < n and src[k] == tok
        lits.append(Lit(j, k + 1 if closed else k, [("text", src[j + 1 : k], j + 1)]))
        i = k + 1 if closed else k
    return lits, comments


PY_START = re.compile(r"#|[\"']")


def lex_py(src: str):
    lits, comments, i, n = [], [], 0, len(src)
    while (m := PY_START.search(src, i)) is not None:
        j, tok = m.start(), m.group()
        if tok == "#":
            i = comment_end(src, j - 1, False)
            comments.append((j, i))
            continue
        p = j
        while p > 0 and src[p - 1] in "rRbBfFuU" and j - p < 2:
            p -= 1
        prefix = src[p:j].lower() if p == 0 or not (src[p - 1].isalnum() or src[p - 1] == "_") else ""
        quote = tok * 3 if src.startswith(tok * 3, j) else tok
        k = j + len(quote)
        while k < n and not src.startswith(quote, k) and (len(quote) == 3 or src[k] != "\n"):
            k += 2 if src[k] == "\\" and "r" not in prefix else 1
        body_start, body_end = j + len(quote), min(k, n)
        parts = [("text", src[body_start:body_end], body_start)]
        if "f" in prefix:
            parts, run, q = [], body_start, body_start
            while q < body_end:
                if src.startswith("{{", q) or src.startswith("}}", q):
                    q += 2
                    continue
                if src[q] == "{":
                    parts.append(("text", src[run:q], run))
                    e = close_brace(src, q + 1, "py")
                    expr = re.split(r"(?<![=!<>])[!:](?!=)", src[q + 1 : e], maxsplit=1)[0]
                    parts.append(("expr", expr, q + 1))
                    q = run = e + 1
                    continue
                q += 1
            parts.append(("text", src[run:body_end], run))
        end = min(k + len(quote), n) if k < n and src.startswith(quote, k) else k
        lits.append(Lit(j, end, parts))
        i = max(end, j + 1)
    return lits, comments


def close_paren(src: str, k: int) -> int:
    """The index of the `)` that closes a shell $( opened before k, skipping quoted words."""
    depth, n = 1, len(src)
    while k < n:
        c = src[k]
        if c == "\\":
            k += 2
            continue
        if c == "'":
            k = quoted_end(src, k, "'", False, True) + 1
            continue
        if c == '"':
            k = dq_end(src, k) + 1
            continue
        if c == "(":
            depth += 1
        elif c == ")":
            depth -= 1
            if depth == 0:
                return k
        k += 1
    return n


def dq_end(src: str, j: int) -> int:
    """The index of the `"` that closes a double-quoted shell word opened at j. A $(...), ${...}
    or `...` inside it is stepped over whole, so the quotes it holds, as in
    "$(contains "$out" "x")", close nothing."""
    k, n = j + 1, len(src)
    while k < n:
        c = src[k]
        if c == "\\":
            k += 2
            continue
        if c == '"':
            return k
        if src.startswith("$(", k):
            k = close_paren(src, k + 2) + 1
            continue
        if src.startswith("${", k):
            k = close_brace(src, k + 2, "sh") + 1
            continue
        if c == "`":
            e = src.find("`", k + 1)
            k = (n if e < 0 else e) + 1
            continue
        k += 1
    return n


def sh_parts(src: str, start: int, end: int) -> list:
    """The parts of a double-quoted shell word or an unquoted here-document body: its text, and
    each $name, ${...}, $(...) and `...` as an expression."""
    parts, run, q = [], start, start
    while q < end:
        c = src[q]
        if c == "\\":
            q += 2
            continue
        if c == "$" and q + 1 < end:
            nxt = src[q + 1]
            if nxt == "{":
                e = close_brace(src, q + 2, "sh")
            elif nxt == "(":
                e = close_paren(src, q + 2)
            elif nxt.isalpha() or nxt == "_":
                e = q + 1
                while e + 1 < end and (src[e + 1].isalnum() or src[e + 1] == "_"):
                    e += 1
            elif nxt.isdigit() or nxt in "#@?$!*-":
                e = q + 1
            else:
                q += 1
                continue
            parts.append(("text", src[run:q], run))
            parts.append(("expr", src[q : e + 1], q))
            q = run = e + 1
            continue
        if c == "`":
            e = src.find("`", q + 1)
            e = end if e < 0 or e > end else e
            parts.append(("text", src[run:q], run))
            parts.append(("expr", src[q : e + 1], q))
            q = run = e + 1
            continue
        q += 1
    parts.append(("text", src[run:end], run))
    return parts


HEREDOC = re.compile(r"<<(-?)\s*(['\"]?)([A-Za-z_][\w.]*)\2")
FEEDS = [
    (re.compile(r">\s*\S+\.go\b"), "go"), (re.compile(r">\s*\S+\.(?:ts|tsx|js|jsx|mjs|cjs)\b"), "ts"),
    (re.compile(r">\s*\S+\.py\b"), "py"), (re.compile(r">\s*\S+\.(?:sh|bash)\b"), "sh"),
    (re.compile(r"\bpython3?\b"), "py"), (re.compile(r"\b(?:node|bun|deno)\b"), "ts"),
    (re.compile(r"(?<![\w.-])(?:ba)?sh\b(?![.\w-])"), "sh"),
]
SH_START = re.compile(r"\\|#|\$'|['\"]|<<|\n")


def lex_sh(src: str):
    lits, comments, i, n = [], [], 0, len(src)
    pending = []
    while (m := SH_START.search(src, i)) is not None:
        j, tok = m.start(), m.group()
        if tok == "\\":
            i = j + 2
            continue
        if tok == "#":
            if j == 0 or src[j - 1] in " \t\n;(|&":
                i = comment_end(src, j - 1, False)
                comments.append((j, i))
            else:
                i = j + 1
            continue
        if tok == "<<":
            heredoc = None if src.startswith("<<<", j) else HEREDOC.match(src, j)
            if heredoc:
                line_start = src.rfind("\n", 0, j) + 1
                pending.append((heredoc.group(3), bool(heredoc.group(2)), heredoc.group(1) == "-", src[line_start:j]))
                i = heredoc.end()
            else:
                i = j + 3 if src.startswith("<<<", j) else j + 2
            continue
        if tok == "\n":
            i = j + 1
            for delim, quoted, tabs, command in pending:
                indent = "\t*" if tabs else ""
                term = re.compile("^" + indent + re.escape(delim) + r"[ \t]*$", re.M).search(src, i)
                body, body_end = i, (term.start() if term else n)
                feeds = next((lang for rx, lang in FEEDS if rx.search(command)), None)
                if feeds:
                    inner, inner_comments = lex(src[body:body_end], feeds)
                    lits += [shift(lit, body) for lit in inner]
                    comments += [(a + body, b + body) for a, b in inner_comments]
                else:
                    parts = [("text", src[body:body_end], body)] if quoted else sh_parts(src, body, body_end)
                    lits.append(Lit(body, body_end, parts))
                i = term.end() if term else n
            pending = []
            continue
        if tok == "$'":
            k = quoted_end(src, j + 1, "'", True, True)
            lits.append(Lit(j, min(k + 1, n), [("text", src[j + 2 : k], j + 2)]))
            i = k + 1
            continue
        if tok == "'":
            k = quoted_end(src, j, "'", False, True)
            lits.append(Lit(j, min(k + 1, n), [("text", src[j + 1 : k], j + 1)]))
            i = k + 1
            continue
        k = dq_end(src, j)
        lits.append(Lit(j, min(k + 1, n), sh_parts(src, j + 1, k)))
        i = k + 1
    return lits, comments


def blanked(src: str, ranges, fill: str = " ") -> str:
    """src with each range written as fill, its newlines kept, so offsets and lines stay."""
    out = list(src)
    for a, b in ranges:
        for k in range(a, min(b, len(out))):
            if out[k] != "\n":
                out[k] = fill
    return "".join(out)


def code_of(src: str, lits: list, comments: list) -> str:
    """src with each comment written as spaces and each literal as \\x01, so a literal is not read
    as the space around it."""
    return blanked(blanked(src, comments), [(lit.start, lit.end) for lit in lits], "\x01")


# ---------------------------------------------------------------- messages


@dataclass
class Message:
    text: str          # the message as the program builds it, each value written \x00<i>\x00
    offsets: list      # the file offset of each character of text
    exprs: list        # (expression, offset) of each value
    start: int
    end: int
    args: list | None  # (expression, offset) of each argument after it in its call


IDENT_CHAR = re.compile(r"[\w$.?!]")


def operand_back(code: str, r: int) -> int:
    """The start of the operand that ends at r in code, or r when there is none."""
    k = r
    while k > 0:
        c = code[k - 1]
        if c in ")]":
            depth, k = 0, k - 1
            while k > 0:
                depth += {")": 1, "]": 1, "(": -1, "[": -1}.get(code[k], 0)
                if depth == 0:
                    break
                k -= 1
            continue
        if IDENT_CHAR.match(c):
            k -= 1
            continue
        break
    return k


def operand_forward(code: str, q: int) -> int:
    n, k = len(code), q
    while k < n:
        c = code[k]
        if c in "([":
            depth = 0
            while k < n:
                depth += {"(": 1, "[": 1, ")": -1, "]": -1}.get(code[k], 0)
                k += 1
                if depth == 0:
                    break
            continue
        if IDENT_CHAR.match(c):
            k += 1
            continue
        break
    return k


def skip_space(code: str, k: int, step: int) -> int:
    if step > 0:
        while k < len(code) and code[k] in " \t\r\n":
            k += 1
        return k
    while k > 0 and code[k - 1] in " \t\r\n":
        k -= 1
    return k


def split_args(code: str, k: int) -> list[tuple[int, int]]:
    """The comma-separated arguments from k to the bracket that closes the call."""
    args, depth, start, n = [], 0, k, len(code)
    while k < n:
        c = code[k]
        if c in "([{":
            depth += 1
        elif c in ")]}":
            if depth == 0:
                args.append((start, k))
                return args
            depth -= 1
        elif c == "," and depth == 0:
            args.append((start, k))
            start = k + 1
        k += 1
    args.append((start, n))
    return args


def chain(code: str, lit: Lit, lang: str, by_start: dict, by_end: dict, src: str):
    """The literals and values a + (or Python's adjacency) joins to lit, in order, and the span."""
    elements: list = [lit]
    joins = lang in ("go", "ts", "py")
    left = lit.start
    while joins:
        q = skip_space(code, left, -1)
        if q > 0 and code[q - 1] == "+" and (q < 2 or code[q - 2] != "+"):
            r = skip_space(code, q - 1, -1)
            if r in by_end:
                elements.insert(0, by_end[r])
                left = by_end[r].start
                continue
            s = operand_back(code, r)
            if s == r:
                break
            elements.insert(0, (src[s:r], s))
            left = s
            continue
        if lang == "py" and q in by_end and q != left:
            elements.insert(0, by_end[q])
            left = by_end[q].start
            continue
        break
    right = lit.end
    while joins:
        q = skip_space(code, right, 1)
        if q < len(code) and code[q] == "+" and code[q + 1 : q + 2] not in ("+", "="):
            r = skip_space(code, q + 1, 1)
            if r in by_start:
                elements.append(by_start[r])
                right = by_start[r].end
                continue
            e = operand_forward(code, r)
            if e == r:
                break
            elements.append((src[r:e], r))
            right = e
            continue
        if lang == "py" and q in by_start and q != right:
            elements.append(by_start[q])
            right = by_start[q].end
            continue
        break
    return elements, left, right


def messages(src: str, code: str, lits: list[Lit], lang: str) -> list[Message]:
    """The messages of a code file: each literal with what joins to it, and its call's arguments."""
    by_start = {lit.start: lit for lit in lits}
    by_end = {lit.end: lit for lit in lits}
    used: set[int] = set()
    out = []
    for lit in sorted(lits, key=lambda l: l.start):
        if lit.start in used:
            continue
        elements, left, right = chain(code, lit, lang, by_start, by_end, src)
        text: list[str] = []
        offsets: list[int] = []
        exprs: list = []
        for element in elements:
            if isinstance(element, Lit):
                used.add(element.start)
                pieces = element.parts
            else:
                pieces = [("expr", element[0], element[1])]
            for kind, part, offset in pieces:
                if kind == "text":
                    text.append(part)
                    offsets.extend(range(offset, offset + len(part)))
                elif PLURAL.search(part):
                    text.append("characters")
                    offsets.extend([offset] * len("characters"))
                else:
                    slot = f"{SLOT}{len(exprs)}{SLOT}"
                    exprs.append((part, offset))
                    text.append(slot)
                    offsets.extend([offset] * len(slot))
        joined = "".join(text)
        if not HAS_WORD.search(joined):
            continue
        out.append(Message(joined, offsets, exprs, left, right, call_args(src, code, left, right, lang, by_start)))
    return out


def call_args(src: str, code: str, start: int, end: int, lang: str, by_start: dict) -> list | None:
    """(expression, offset) of each argument after the message that ends at `end`, for its verbs."""
    q = skip_space(code, end, 1)
    spans = None
    if lang == "sh":
        line_start = code.rfind("\n", 0, start) + 1
        if re.search(r"\bprintf\s+(?:-v\s+\w+\s+)?$", code[line_start:start]):
            spans, k, n = [], end, len(code)
            while True:
                while k < n and code[k] in " \t":
                    k += 1
                if k >= n or code[k] in "\n;|&>)":
                    break
                e = k
                while e < n and code[e] not in " \t\n;|&>)":
                    e = by_start[e].end if e in by_start else e + 1
                spans.append((k, e))
                k = e
    elif q < len(code) and code[q] == ",":
        spans = split_args(code, q + 1)
    elif lang == "py" and code.startswith(".format(", q):
        spans = split_args(code, q + len(".format("))
    elif lang == "py" and q < len(code) and code[q] == "%":
        r = skip_space(code, q + 1, 1)
        spans = split_args(code, r + 1) if code[r : r + 1] == "(" else [(r, operand_forward(code, r))]
    if spans is None:
        return None
    out = []
    for a, b in spans:
        a = skip_space(code, a, 1)
        lit = by_start.get(a)
        if lit is not None and lang == "sh":
            values = [(part, offset) for kind, part, offset in lit.parts if kind == "expr"]
            out.append(values[0] if len(values) == 1 else (src[a:b], a))
        else:
            out.append((code[a:b], a))
    return out


# ---------------------------------------------------------------- following a count to its counter


def strip_counters(expr: str, rx: re.Pattern | None) -> str:
    """expr with each code-point counter, its argument included, written as spaces."""
    if rx is None:
        return expr
    out = expr
    for m in reversed(list(rx.finditer(expr))):
        end = m.end()
        paren = expr.find("(", m.start(), m.end())
        if paren >= 0:
            depth, k = 0, paren
            while k < len(expr):
                depth += {"(": 1, ")": -1}.get(expr[k], 0)
                k += 1
                if depth == 0:
                    break
            end = k
            if expr.startswith(".length", end):
                end += len(".length")
        out = out[: m.start()] + " " * (end - m.start()) + out[end:]
    return out


def judge(expr: str, code: str, pos: int, lang: str, depth: int = 0, seen: frozenset = frozenset()) -> tuple[str, str]:
    """("code points" | "other" | "bound" | "unknown", why) for the number expr evaluates to, at
    pos in code. Each name is followed to its assignment in the same file."""
    expr = expr.strip()
    if not expr:
        return "unknown", "a value the check cannot read"
    rx = CODE_POINTS[lang]
    found_cp = bool(rx and rx.search(expr))
    rest = strip_counters(expr, rx)
    if lang == "sh" and found_cp and not sets_utf8_locale(code, pos):
        return "other", "${#s} and wc -m count in the caller's locale, bytes under C: the script sets no UTF-8 locale"
    for label, other, unit in OTHER[lang]:
        if other.search(rest):
            if lang == "go" and label == "len()" and runes_only(rest, code, pos):
                found_cp = True
                continue
            return "other", f"{label} counts {unit}"
    if lang == "py" and re.search(r"\blen\(", expr):
        return "unknown", "Python's len() counts a str's code points and a bytes' bytes"
    if lang == "go":
        rest = re.sub(r"\blen\((\w+)\)", " ", rest)
    results = []
    for name, call in names(rest, lang):
        if call:
            last = name.rsplit(".", 1)[-1]
            if PASS_THROUGH.match(name):
                continue
            if "." in name and PASS_THROUGH.match(last):
                name = name.rsplit(".", 1)[0]
            else:
                return "unknown", f"the value comes from {name}(), which the check does not follow"
        if depth >= 4 or name in seen:
            return "unknown", f"{name} is not followed past four assignments"
        found = definition(code, name, pos, lang)
        if found is None:
            return "unknown", f"{name} is not assigned in this file"
        results.append(judge(found[0], code, found[1], lang, depth + 1, seen | {name}))
    for kind, why in results:
        if kind == "other":
            return kind, why
    for kind, why in results:
        if kind == "unknown":
            return kind, why
    if found_cp or any(kind == "code points" for kind, _ in results):
        return "code points", ""
    if results or re.fullmatch(r"[\d_,.\s+\-*/()]+", rest):
        return "bound", "a bound with no counter beside it"
    return "unknown", "a value the check cannot read"


def runes_only(rest: str, code: str, pos: int) -> bool:
    """Whether every len() in rest counts a name assigned a []rune."""
    calls = re.findall(r"\blen\(([^()]*)\)", rest)
    if not calls or len(calls) != len(re.findall(r"\blen\(", rest)):
        return False
    for name in calls:
        found = definition(code, name.strip(), pos, "go") if re.fullmatch(r"\s*\w+\s*", name) else None
        if found is None or not re.match(r"\s*\[\]rune\(", found[0]):
            return False
    return True


def sets_utf8_locale(code: str, pos: int) -> bool:
    return bool(re.search(r"(?m)^\s*(?:export\s+)?LC_ALL=['\"]?[A-Za-z_]+\.UTF-8\b", code[:pos]))


def names(expr: str, lang: str):
    """Each name expr reads, once, and whether a call follows it."""
    seen = set()
    if lang == "sh":
        found = [(m.group(1), False) for m in re.finditer(r"\$\{?([A-Za-z_]\w*)", expr)]
    else:
        found = []
        for m in re.finditer(r"(?<![\w$.])([A-Za-z_$][\w$]*(?:\??\.[A-Za-z_$][\w$]*)*)\s*(\()?", expr):
            name = m.group(1)
            if name.split(".")[0] in KEYWORDS:
                continue
            found.append((name.replace("?.", "."), bool(m.group(2))))
    for name, call in found:
        if (name, call) not in seen:
            seen.add((name, call))
            yield name, call


def visible(code: str, d: int, pos: int) -> bool:
    """Whether an assignment at d is in a block that still holds pos."""
    depth = 0
    for c in code[d:pos]:
        if c == "{":
            depth += 1
        elif c == "}":
            depth -= 1
            if depth < 0:
                return False
    return True


def definition(code: str, name: str, pos: int, lang: str) -> tuple[str, int] | None:
    """The expression last assigned to name before pos in a block that holds pos, else its
    assignment at the top of the file, and that expression's offset. In `a, b := x, y` the name
    takes its own value; where the values are not one per name, the value is "" (not read)."""
    esc = re.escape(name)
    names_before = r"(?P<before>(?:[\w$]+[ \t]*,[ \t]*)*)"
    names_after = r"(?P<after>(?:[ \t]*,[ \t]*[\w$]+)*)"
    if lang == "sh":
        rx = re.compile(rf"(?:^|(?<=[\s;(&|]))(?:local\s+|export\s+|readonly\s+|declare\s+(?:-\w+\s+)*)?{esc}=")
    elif lang == "py":
        rx = re.compile(rf"(?<![\w.]){names_before}{esc}{names_after}[ \t]*=(?!=)")
    else:
        typed = r"(?:[ \t]+[\w.\[\]*]+)?" if lang == "go" else r"(?:[ \t]*:[ \t]*[\w$<>\[\]|.]+(?:[ \t]*[|&][ \t]*[\w$<>\[\]|.]+)*)?"
        rx = re.compile(rf"(?<![\w$.]){names_before}{esc}{names_after}{typed}[ \t]*(?::=|(?<![=!<>+\-*/%&|^:])=(?![=>]))")
    found = list(rx.finditer(code))
    before = [m for m in found if m.end() <= pos and (lang == "py" or visible(code, m.end(), pos))]
    if before:
        m = before[-1]
    else:
        top = [m for m in found if lang == "py" or visible(code, 0, m.start()) and code[: m.start()].count("{") == code[: m.start()].count("}")]
        if not top:
            return None
        m = top[0]
    groups = m.groupdict()
    index = (groups.get("before") or "").count(",")
    total = index + 1 + (groups.get("after") or "").count(",")
    if total == 1:
        return rhs(code, m.end(), lang), m.end()
    value = rhs(code, m.end(), lang, commas=True)
    parts = split_args(value + ")", 0)
    if len(parts) != total:
        return "", m.end()
    a, b = parts[index]
    return value[a:b], m.end() + a


def rhs(code: str, k: int, lang: str, commas: bool = False) -> str:
    depth, n, start = 0, len(code), k
    while k < n and k - start < 600:
        c = code[k]
        if lang == "sh":
            if c == "$" and code[k + 1 : k + 2] in ("(", "{"):
                close = close_paren(code, k + 2) if code[k + 1] == "(" else code.find("}", k)
                k = (close if close >= 0 else n) + 1
                continue
            if c in " \t\n;|&)":
                break
            k += 1
            continue
        if c in "([{":
            depth += 1
        elif c in ")]}":
            if depth == 0:
                break
            depth -= 1
        elif depth == 0 and (c == ";" or (c == "," and not commas)):
            break
        elif depth == 0 and c == "\n":
            before = code[start:k].rstrip()
            after = code[k:].lstrip()[:1]
            if not (before.endswith(tuple("+-*/(,|&?:=")) or after in (".", "+", "?", ":", "|", "&")):
                break
        k += 1
    return code[start:k]


def statement_start(code: str, k: int) -> int:
    depth = 0
    while k > 0:
        c = code[k - 1]
        if c in ")]":
            depth += 1
        elif c in "([":
            depth = max(0, depth - 1)
        elif depth == 0 and c in ";{}":
            return k
        elif depth == 0 and c == "\n":
            if not code[: k - 1].rstrip().endswith(tuple(",(+-*/&|=?:.[")):
                return k
        k -= 1
    return 0


CONDITION = re.compile(r"\s*(?:\}\s*)?(?:else\s+)?(?:if|elif|while|switch|for)\b")


def header(code: str, pos: int, lang: str) -> str:
    """The condition of the `if` or `case` a message at pos answers, "" when it answers none."""
    line_start = code.rfind("\n", 0, pos) + 1
    same = code[line_start:pos]
    if lang in ("py", "sh"):
        if CONDITION.match(same) or (lang == "sh" and re.search(r"\(\(|\[\[?\s|\btest\b", same)):
            return same
        indent = len(same) - len(same.lstrip())
        k = line_start
        for _ in range(12):
            if k <= 0:
                break
            prev = code.rfind("\n", 0, k - 1) + 1
            line = code[prev : k - 1]
            k = prev
            if not line.strip():
                continue
            if lang == "sh":
                if re.match(r"\s*(?:fi|done|esac|\}|[\w-]+\s*\(\)\s*\{?)\s*$", line):
                    return ""
                if CONDITION.match(line):
                    return line
            elif len(line) - len(line.lstrip()) < indent:
                return line if CONDITION.match(line) else ""
        return ""
    stmt = code[statement_start(code, pos) : pos]
    if CONDITION.match(stmt):
        return stmt
    depth, k = 0, pos
    while k > 0:
        k -= 1
        if code[k] == "}":
            depth += 1
        elif code[k] == "{":
            if depth == 0:
                break
            depth -= 1
    else:
        return ""
    cases = list(re.finditer(r"(?m)^\s*case\b([^\n]*):\s*$", code[k + 1 : pos]))
    if cases:
        return cases[-1].group(1)
    head_start = code.rfind("\n", 0, k) + 1
    head = code[head_start:k]
    while head.count(")") > head.count("(") and head_start > 0:
        head_start = code.rfind("\n", 0, head_start - 1) + 1
        head = code[head_start:k]
    return head if CONDITION.match(head) else ""


def judge_condition(condition: str, code: str, pos: int, lang: str) -> tuple[str, str]:
    """A condition's counter: each side of each comparison judged alone, so the bound a counted
    value is compared with need not be followed."""
    atoms = re.split(r"&&|\|\||;|(?<![<>!=:-])[<>]=?(?![=>])|[!=]==?|\b(?:and|or)\b|\s-(?:gt|ge|lt|le|eq|ne)\s", condition)
    kinds = [judge(atom, code, pos, lang) for atom in atoms if atom.strip()]
    for kind, why in kinds:
        if kind == "other":
            return kind, why
    if any(kind == "code points" for kind, _ in kinds):
        return "code points", ""
    return "unknown", next((why for kind, why in kinds if kind == "unknown"), "a bound with no counter beside it")


def judge_count(message: Message, count: str, at: int, code: str, lang: str) -> tuple[str, str]:
    """The verdict on one count of a code message: ("code points" | "instance", why)."""
    slot = re.fullmatch(r"\x00(\d+)\x00", count)
    if slot:
        expr, offset = message.exprs[int(slot.group(1))]
        kind, why = judge(expr, code, offset, lang)
    elif re.fullmatch(VERB, count):
        # Every verb before the count takes an argument, %q and %x as much as %d, so the index
        # counts each one, not only the verbs that can state a count.
        verbs = [v for v in re.finditer(r"%%|%(?:\[\d+\])?[-+ #0]*\d*(?:\.\d+)?[a-zA-Z]", message.text[:at]) if v.group() != "%%"]
        explicit = re.match(r"%\[(\d+)\]", count)
        index = int(explicit.group(1)) - 1 if explicit else len(verbs)
        if message.args is None or index >= len(message.args):
            return "instance", "cannot judge: its format verb takes no argument in this call"
        expr, offset = message.args[index]
        kind, why = judge(expr, code, offset, lang)
    else:
        kind, why = "bound", "a bound with no counter beside it"
    if kind == "bound":
        # A number or a constant is a bound: its counter is the one the `if` or `case` the
        # message answers compares with it.
        condition = header(code, message.start, lang)
        if condition:
            kind, why = judge_condition(condition, code, message.start, lang)
            if kind == "unknown":
                why = "a bound, and " + why
    if kind == "code points":
        return "code points", ""
    if kind == "other":
        return "instance", why
    return "instance", "cannot judge: " + why


# ---------------------------------------------------------------- the verdicts


def sentence(text: str, a: int, b: int, prose: bool) -> tuple[int, int]:
    ends = PROSE_SENTENCE_END if prose else CODE_SENTENCE_END
    start = 0
    for m in ends.finditer(text, max(0, a - 400), a):
        start = m.end()
    m = ends.search(text, b)
    return start, m.start() if m else len(text)


def passes(text: str, match: re.Match, prose: bool) -> str:
    s, e = sentence(text, match.start(), match.end(), prose)
    if NAMES_UNIT.search(text, match.end(), min(e, match.end() + 40)):
        return "names its unit"
    if ASCII_GRAMMAR.search(text, s, e):
        return "ASCII grammar"
    return ""


def exempt(lines: list[str], first: int, last: int) -> str:
    for line in lines[max(0, first - 2) : last + 1]:
        marker = EXEMPT.search(line)
        reason = ((marker.group(2) or "") if marker else "").strip().removesuffix("-->").strip()
        if marker and reason:
            return f"exempt, {marker.group(1)}: {reason}"
    return ""


def line_of(starts: list[int], offset: int) -> int:
    lo, hi = 0, len(starts)
    while lo < hi:
        mid = (lo + hi) // 2
        if starts[mid] <= offset:
            lo = mid + 1
        else:
            hi = mid
    return lo - 1


PROSE_COMMENTS = [
    (re.compile(r"\.(?:md|mdx|html?|svg|xml|vue)$"), re.compile(r"<!--.*?-->", re.S)),
    (re.compile(r"\.(?:sql|lean|lua|hs)$"), re.compile(r"--[^\n]*")),
    (re.compile(r"\.css$"), re.compile(r"/\*.*?\*/", re.S)),
    (re.compile(r"(?:\.(?:ya?ml|toml|jq|tf|hcl|conf|cfg|ini|env|example|gitignore|dockerignore)|Dockerfile[^/]*|(?:^|/)[^./]+)$"),
     re.compile(r"(?m)(?:^|(?<=\s))#[^\n]*")),
]


def scan(path: str, content: str) -> list[Hit]:
    if not HAS_WORD.search(content):
        return []
    lang = language(path, content[:200])
    lines = content.split("\n")
    starts = [0]
    for line in lines[:-1]:
        starts.append(starts[-1] + len(line) + 1)
    hits: list[Hit] = []
    if lang == "prose":
        comment = next((rx for name, rx in PROSE_COMMENTS if name.search(path)), None)
        text = blanked(content, [m.span() for m in comment.finditer(content)]) if comment else content
        for match in PROSE_HIT.finditer(text):
            line = line_of(starts, match.start())
            verdict = passes(text, match, True) or exempt(lines, line, line)
            why = "" if verdict else "cannot judge: a count in prose names no counter"
            shown = " ".join(text[max(0, match.start() - 80) : match.end() + 60].split())
            hits.append(Hit(path, line + 1, shown, verdict or "instance", why))
        return hits
    lits, comments = lex(content, lang)
    code = code_of(content, lits, comments)
    found = []
    for message in messages(content, code, lits, lang):
        for match in CODE_HIT.finditer(message.text):
            group = next(g for g in ("c1", "c2", "c3") if match.group(g) is not None)
            found.append((message, match, match.group(group), match.start(group)))
    found += code_text_hits(path, content, code, lang)
    found.sort(key=lambda f: f[0].offsets[min(f[1].start(), len(f[0].offsets) - 1)])
    for message, match, count, at in found:
        line = line_of(starts, message.offsets[min(match.start(), len(message.offsets) - 1)])
        verdict = passes(message.text, match, False) or exempt(lines, line_of(starts, message.start), line)
        why = ""
        if not verdict:
            verdict, why = judge_count(message, count, at, code, lang)
        shown = re.sub(r"\x00(\d+)\x00", lambda m: "${" + message.exprs[int(m.group(1))][0].strip() + "}", message.text)
        hits.append(Hit(path, line + 1, " ".join(shown.split())[:160], verdict, why))
    return hits


def code_text_hits(path: str, content: str, code: str, lang: str):
    """Counts written in code rather than in a literal: JSX text in .tsx and .jsx, and unquoted
    shell words. Each is read as a message of its own line."""
    if lang == "ts" and path.endswith((".tsx", ".jsx")):
        def word(m: re.Match) -> str:
            return "characters".ljust(len(m.group())) if PLURAL.search(content, m.start(), m.end()) else m.group()
        text = re.sub(r"\{[^{}\n]*\}", word, code)
        count_rx = rf"\{{[^{{}}\n]*\}}|{NUMBER}"
    elif lang == "sh":
        text = code
        count_rx = rf"\$\{{[^{{}}\n]*\}}|\$\w+|{NUMBER}"
    else:
        return []
    out = []
    for match in re.finditer(rf"(?P<c1>{count_rx})(?:-|\s+)(?P<w1>{WORD})", text):
        count = match.group("c1")
        exprs = []
        if count.startswith("{"):
            exprs.append((content[match.start() + 1 : match.end("c1") - 1], match.start() + 1))
        elif count.startswith("$"):
            exprs.append((count, match.start()))
        slot = f"{SLOT}0{SLOT}" if exprs else count
        line_start = text.rfind("\n", 0, match.start()) + 1
        line_end = text.find("\n", match.end())
        line_end = len(text) if line_end < 0 else line_end
        rel = match.start() - line_start
        msg_text = text[line_start : match.start()] + slot + text[match.end("c1") : line_end]
        offsets = list(range(line_start, match.start())) + [match.start()] * len(slot) + list(range(match.end("c1"), line_end))
        message = Message(msg_text, offsets, exprs, match.start(), match.end(), None)
        again = CODE_HIT.search(msg_text, rel)
        if again:
            group = next(g for g in ("c1", "c2", "c3") if again.group(g) is not None)
            out.append((message, again, again.group(group), again.start(group)))
    return out


# ---------------------------------------------------------------- the tree


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


# ---------------------------------------------------------------- the controls

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
    ("an ASCII-only grammar in the count's sentence stays quiet", "packages/envoy/internal/planted/ascii.go",
     'package planted\n\nvar e = errors.New("commit is the 40-character lowercase hex commit the check ran at")\n',
     ["ASCII grammar"]),
    ("an ASCII-only grammar in another sentence passes nothing", "packages/envoy/internal/planted/hex.go",
     'package planted\n\nfunc h(title string) error {\n'
     '\treturn fmt.Errorf("title is at most %d characters; the commit beside it is 40 hex", itemblocks.Units16(title))\n}\n',
     ["instance"]),
    ("a message naming its unit stays quiet", "packages/envoy-client/src/unit.ts",
     "export const line = (n: number) => `One block: ${n} characters (UTF-16 units).`;\n", ["names its unit"]),
    ("a bound its if counts in code points stays quiet", "packages/envoy/internal/planted/runes.go",
     'package planted\n\nfunc g(id string) string {\n\tif n := utf8.RuneCountInString(id); n > 200 {\n'
     '\t\treturn fmt.Sprintf("an id is at most 200 characters, and this one is %d", n)\n\t}\n\treturn ""\n}\n',
     ["code points"]),
    ("a bound its if counts in bytes fires, though the call counts code points", "packages/envoy/internal/planted/header.go",
     'package planted\n\nfunc t(title string) error {\n\tif len(title) > 200 {\n'
     '\t\treturn fmt.Errorf("a title is at most 200 characters, and this one is %d", utf8.RuneCountInString(title))\n'
     '\t}\n\treturn nil\n}\n',
     ["instance"]),
    ("each count is judged by its own value, and one behind a helper the check cannot follow fires",
     "packages/envoy/internal/planted/helper.go",
     'package planted\n\nfunc refusal(title, body string) string {\n\ttitleSize := measured(title)\n'
     '\tbodySize := utf8.RuneCountInString(body)\n'
     '\treturn fmt.Sprintf("title is %d characters; body is %d characters", titleSize, bodySize)\n}\n'
     'func measured(value string) int {\n\treturn len(value)\n}\n',
     ["instance", "code points"]),
    ("a count the call itself takes in bytes fires", "packages/envoy/internal/planted/direct.go",
     'package planted\n\nfunc d(title, body string) string {\n'
     '\treturn fmt.Sprintf("title is %d characters; body is %d characters", len(title), utf8.RuneCountInString(body))\n}\n',
     ["instance", "code points"]),
    ("a %q before the count takes its own argument, so the count is judged by the next",
     "packages/envoy/internal/planted/quoted.go",
     'package planted\n\nfunc q(name string) string {\n'
     '\treturn fmt.Sprintf("%q holds %d characters", utf8.RuneCountInString(name), len(name))\n}\n',
     ["instance"]),
    ("a double-quoted shell word holding \"$(cmd \"x\")\" ends at its own quote",
     "packages/envoy/scripts/planted-substitution.sh",
     "#!/usr/bin/env bash\ncheck \"names it\" \"$(contains \"$out\" \"the gate's refusal\")\"\n"
     "echo \"name: $(printf '%s' \"$1\" | wc -c) characters\"\n# keep a 40-character note\n",
     ["instance | wc -c counts bytes"]),
    ("a count beside a word chosen in a template's expression fires", "packages/envoy-client/src/plural.ts",
     'export const left = (n: number) => `${n} ${n === 1 ? "character" : "characters"} left`;\n', ["instance"]),
    ("a single-quoted TypeScript string is read", "packages/envoy-client/src/single.ts",
     "export const said = (text: string) => 'cut at ' + text.length + ' characters';\n",
     ["instance | .length counts"]),
    ("in a, b := x, y each name takes its own value", "packages/envoy/internal/planted/pair.go",
     'package planted\n\nfunc p(x, y string) string {\n\tsent, held := utf8.RuneCountInString(x), len(y)\n'
     '\treturn fmt.Sprintf("sent %d characters, held %d characters", sent, held)\n}\n',
     ["code points", "instance"]),
    ("a %s count from a format verb is followed to its len()", "packages/envoy/internal/planted/verb.go",
     'package planted\n\nfunc v(name string) string {\n'
     '\treturn fmt.Sprintf("%s characters", strconv.Itoa(len(name)))\n}\n', ["instance"]),
    ("a comment is no hit", "packages/envoy-client/src/comment.ts",
     "// a 40-character name, in a comment\nconst re = /[`]/g;\n", []),
    ("a Python comment is no hit", "scripts/planted/comment.py",
     '# docs example: amount + " characters"\nprint("done")\n', []),
    ("a message holding a URL is read whole", "packages/envoy-client/src/url.ts",
     'export const said = (text: string) => "https://help/name: " + text.length + " characters";\n', ["instance"]),
    ("a count beside a word chosen in an expression, or joined to it with +, fires",
     "packages/dispatch/web/src/planted/Counter.tsx",
     "const left = MAX - note.length;\n"
     'const view = <p>\n  {left} {left === 1 ? "character" : "characters"} left\n</p>;\n'
     'const said = "cut at " + note.length + " characters";\n',
     ["instance", "instance"]),
    ("a count and the word in literals a + joins across lines fire, in TypeScript", "packages/contracts/src/planted-split.ts",
     "const LIMIT = 32000;\nexport const RULE =\n  `Each block holds at most ${LIMIT} ` +\n"
     "  `characters; a longer one is refused.`;\n", ["instance"]),
    ("a count and the word in literals a + joins across lines fire, in Go", "packages/envoy/internal/planted/split.go",
     'package planted\n\nfunc s(body string) error {\n\treturn fmt.Errorf("each block holds at most %d "+\n'
     '\t\t"characters", len(body))\n}\n', ["instance"]),
    ("a shell here-document's text is read, and an apostrophe in it closes nothing", "packages/envoy/scripts/planted-usage.sh",
     "#!/usr/bin/env bash\nusage() {\n  cat >&2 <<'USAGE'\nusage: planted-usage.sh <title>   the gate's title, at most 200 characters\n"
     "  a longer title is cut at character 200\nUSAGE\n}\nn=$(printf '%s' \"${1:-}\" | wc -c)\necho \"the title is $n characters\"\n",
     ["instance | cannot judge: a bound", "instance | cannot judge: a bound", "instance | wc -c counts bytes"]),
    ("a shell count in a UTF-8 locale the script sets itself is code points", "packages/envoy/scripts/planted-locale.sh",
     "#!/usr/bin/env bash\nexport LC_ALL=C.UTF-8\nname=$1\nprintf 'name: %d characters\\n' \"${#name}\"\n",
     ["code points"]),
    ("a shell count in the caller's locale fires", "packages/envoy/scripts/planted-nolocale.sh",
     "#!/usr/bin/env bash\nname=$1\nprintf 'name: %d characters\\n' \"${#name}\"\n", ["instance"]),
    ("a count in prose fires, and one naming its unit stays quiet", "packages/pi-envoy/extensions/planted/rules.md",
     "A rewritten question is at most\n2,000 characters, Dispatch's limit.\n\n"
     "A block is at most 32,000 characters (UTF-16 code units).\n", ["instance", "names its unit"]),
    ("a count in a workflow's YAML fires", ".github/workflows/planted.yaml",
     "jobs:\n  a:\n    steps:\n      - run: echo \"title is $(printf '%s' \"$T\" | wc -c) characters\"\n", ["instance"]),
    ("chars is the word", "packages/pi-legion/scripts/planted/score.ts",
     "export const note = (q: string) => `chars=${q.length}`;\n", ["instance"]),
    ("an exempt site stays quiet, and fires once its comment or its reason is gone", None, None, None),
]


# One planted value per entry of OTHER_COUNTERS: a code-point counter and that entry together.
COUNTER_CASES = [
    ("go", "len()", "len(name)"),
    ("go", "Units16", "itemblocks.Units16(name)"),
    ("go", "utf16.", "utf16.RuneLen(rune(name[0]))"),
    ("ts", ".length", "name.length"),
    ("ts", "byteLength", "Buffer.byteLength(name)"),
    ("sh", "wc -c", "$(printf '%s' \"$name\" | wc -c)"),
]


def counter_controls() -> list[tuple[str, str | None]]:
    """Each case of COUNTER_CASES must fire naming its own counter, and each entry of
    OTHER_COUNTERS must have a case: the reason a hit fails names the counter, so an entry taken
    off the list fails its case even where the check would fail the hit as one it cannot judge."""
    out = []
    for lang, label, sample in COUNTER_CASES:
        if lang == "go":
            path = "packages/envoy/internal/planted/mixed.go"
            content = ('package planted\n\nfunc m(name string) string {\n'
                       f'\treturn fmt.Sprintf("%d characters", utf8.RuneCountInString(name)+{sample})\n}}\n')
        elif lang == "ts":
            path = "packages/envoy-client/src/mixed.ts"
            content = f"export const m = (name: string) => `${{[...name].length + {sample}}} characters`;\n"
        else:
            path = "packages/envoy/scripts/planted-mixed.sh"
            content = ("#!/usr/bin/env bash\nexport LC_ALL=C.UTF-8\nname=$1\n"
                       f"n=$(( ${{#name}} + {sample} ))\necho \"$n characters\"\n")
        name = f"a value holding a code-point counter and {label} fires, naming {label}"
        got = [(hit.verdict, hit.why) for hit in scan(path, content)]
        ok = len(got) == 1 and got[0][0] == "instance" and got[0][1].startswith(f"{label} counts")
        out.append((name, None if ok else f"{name}: answered {got}"))
    cased = {(lang, label) for lang, label, _ in COUNTER_CASES}
    for lang, entries in OTHER_COUNTERS.items():
        for label, _, _ in entries:
            if (lang, label) not in cased:
                out.append((f"{lang} counter {label} has a case", f"{lang} counter {label} has no case in COUNTER_CASES"))
    return out


def control(name, path, content, want) -> str | None:
    """A planted file's hits against want: each item a verdict, or `<verdict> | <start of its why>`."""
    if path is not None:
        got = [(hit.verdict, hit.why) for hit in scan(path, content)]
        wanted = [(item.split(" | ", 1) + [""])[:2] for item in want]
        ok = len(got) == len(wanted) and all(v == w[0] and why.startswith(w[1]) for (v, why), w in zip(got, wanted))
        return None if ok else f"{name}: answered {got}, want {want}"
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


def tree_controls() -> list[str]:
    """The files the check reads and leaves out, and UNKNOWN over an empty listing, through the
    same code as the scan."""
    problems = []
    planted = {
        ".github/workflows/planted.yaml": b"- run: echo \"$(printf '%s' \"$T\" | wc -c) characters\"\n",
        "packages/envoy/internal/planted/planted_test.go": b'package planted\n\nvar _ = fmt.Sprintf("%d characters", len(s))\n',
        "packages/envoy/internal/planted/planted_generated.go": b'package planted\n\nvar Rule = "at most 32000 characters"\n',
        SELF: b'echo "%d characters"\n',
        "packages/envoy/CHANGELOG.md": b"- A title is at most 1,000 characters.\n",
        "docs/solutions/planted.md": b"a 32,000-character body\n",
        "docs/planted.png": b"\x89PNG\r\n\x1a\n\0\0 2000 characters",
        "packages/claude-envoy/skills/planted": "link",
        "gone.md": None,
    }
    code, report, hits = judge_tree(list(planted), planted.get)
    fired = {hit.path for hit in hits if hit.verdict == "instance"}
    want = {".github/workflows/planted.yaml", "packages/envoy/internal/planted/planted_test.go",
            "packages/envoy/internal/planted/planted_generated.go"}
    if fired != want or code != 1:
        problems.append(f"a count in a workflow, a test and a generated file fires: fired in {sorted(fired)}, exit {code}")
    for kind, count in ((LEFT_OUT[0][1], 1), (DATED[0], 2), (BINARY, 1), (LINK, 1), (MISSING, 1)):
        if not any(line.startswith(f"  left out, {kind}: {count}") for line in report):
            problems.append(f"the report counts {count} file(s) left out as {kind!r}: {report}")
    code, report, _ = judge_tree([], lambda path: None)
    if code != 1 or not any(line.startswith("UNKNOWN:") for line in report):
        problems.append(f"an empty listing prints UNKNOWN and fails: exit {code}, {report}")
    return problems


def main() -> int:
    counters = counter_controls()
    failed = [problem for problem in (control(*c) for c in CONTROLS) if problem] + \
        [problem for _, problem in counters if problem] + tree_controls()
    if failed:
        print("CONTROLS FAILED: " + "; ".join(failed))
        return 1
    total = len(CONTROLS) + len(counters) + 1
    print(f"controls: {total} of {total} answered as they must")
    for name, *_ in CONTROLS:
        print(f"  ok  {name}")
    for name, _ in counters:
        print(f"  ok  {name}")
    print("  ok  a count in a workflow, a test and a generated file fires; each class left out is counted; an empty listing is UNKNOWN")
    listed = subprocess.run(
        ["git", "ls-files", "-z", "--cached", "--others", "--exclude-standard"],
        capture_output=True, check=True,
    ).stdout.decode("utf-8", errors="replace").split("\0")
    listing = sorted({path for path in listed if path})

    def read_file(path: str) -> bytes | str | None:
        if os.path.islink(path):
            return "link"
        try:
            with open(path, "rb") as handle:
                return handle.read()
        except (FileNotFoundError, IsADirectoryError, NotADirectoryError):
            return None

    code, report, hits = judge_tree(listing, read_file)
    if "--list" in sys.argv[1:]:
        for hit in hits:
            why = f" {hit.why} ::" if hit.why else ""
            print(f"{hit.path}:{hit.line}: [{hit.verdict}]{why} {hit.text}")
    for line in report:
        print(line)
    if code and not report[-1].startswith("UNKNOWN"):
        print(
            "Give the numbers with no unit (\"title: 1002 of at most 1000\"), or name the unit the "
            "counter counts (\"UTF-16 code units\", \"bytes\"). Where the counter is where the check "
            "cannot follow it, write `characters: code points, <where>` or `characters: ASCII alone, "
            "<why>` in a comment on the line or the two lines above.",
            file=sys.stderr,
        )
    return code


sys.exit(main())
PY
