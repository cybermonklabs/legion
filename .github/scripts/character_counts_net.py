"""The net of the character-count check: it finds each site, an occurrence of a unit word on a line
that also holds a count, and decides nothing about it. The ledger decides each site
(character_counts_ledger.py).

The net reads each line's raw text, comments and all, and the same line with its escapes decoded,
so that "\\u0063haracters", "&#99;haracters" and the "characters" of "\\ncharacters" are the word.
It parses no language and reads no grammar: no comment, quotation, qualifier word, locale or
counter makes an occurrence a site or keeps it from being one."""
from __future__ import annotations

import hashlib
import re
from dataclasses import dataclass

# The unit words, in any case, as whole words: "character", "characters", "char" and "chars". A
# word inside a name ("maxChars", "MAX_CHARS") is not one.
WORD = re.compile(r"\b(?:characters?|chars?)\b", re.IGNORECASE)

# A count on the line, in any of these forms.
COUNT = re.compile("|".join([
    # A number: a digit that does not end a name ("2,000", "32k", "1-40", "UTF-16"; not "utf8").
    r"(?<![A-Za-z_])[0-9]",
    # A number in words ("two", "a dozen", "a single").
    r"\b(?:zero|one|two|three|four|five|six|seven|eight|nine|ten|eleven|twelve|thirteen|fourteen|fifteen"
    r"|sixteen|seventeen|eighteen|nineteen|twenty|thirty|forty|fifty|sixty|seventy|eighty|ninety|hundred"
    r"|thousand|million|billion|dozen|single|half|double|twice)\b",
    # A format directive of printf, Go, Python's %, logging and the shell ("%d", "%[2]d", "%*d",
    # "%(n)d", "%v", "%s").
    r"%(?:\([^)\s]*\))?(?:\[\d+\])?[-+ #0]*(?:\*|\[\d+\]\*|\d+)?(?:\.(?:\*|\d+))?(?:\[\d+\])?[bcdeEfFgGioqsuvxX]",
    # A brace interpolation: str.format and f-strings, JSX, Rust and C# ("{}", "{0}", "{n:,}",
    # "{s.length}"), and a Go or Handlebars template ("{{.N}}", "{{ .N }}").
    r"\{[^{}\s]*\}|\{\{[^}]*\}\}",
    # A dollar or hash interpolation: a template literal's or the shell's ("${n}", "${#s}", "$n",
    # "$1") and Ruby's ("#{n}").
    r"\$\{[^}]*\}|\$[A-Za-z_0-9#@*?]|#\{[^}]*\}",
    # A length call or property beside the word ("len(s)", "s.length", "wc -c").
    r"\blen\(|\.length\b|\.size\b|\bsize\(|RuneCount|\bwc\b|\bstrlen\b|\.Len\(|\bcount\(|Units16",
]), re.IGNORECASE)

# The escapes the net decodes before it looks for the word and the count: \uXXXX, \u{X}, \UXXXXXXXX,
# \xXX, a three-digit octal escape, \n, \t and \r, and HTML's numeric character references.
ESCAPE = re.compile(
    r"\\(?:u\{([0-9A-Fa-f]{1,6})\}|u([0-9A-Fa-f]{4})|U([0-9A-Fa-f]{8})|x([0-9A-Fa-f]{2})|([0-7]{3})|([ntr])|(\\))"
    r"|&#(?:[xX]([0-9A-Fa-f]{1,6})|([0-9]{1,7}));"
)
WHITESPACE = {"n": "\n", "t": "\t", "r": "\r"}


def decoded(line: str) -> str:
    """line with each escape of ESCAPE written as the character it stands for. An escape that
    names no character stays as written."""

    def one(match: re.Match[str]) -> str:
        brace, u4, u8, x2, octal, space, backslash, hex_ref, dec_ref = match.groups()
        if space:
            return WHITESPACE[space]
        if backslash:
            return "\\"
        hexdigits = brace or u4 or u8 or x2 or hex_ref
        value = int(hexdigits, 16) if hexdigits else int(octal, 8) if octal else int(dec_ref)
        try:
            return chr(value)
        except (ValueError, OverflowError):
            return match.group(0)

    return ESCAPE.sub(one, line)


@dataclass(frozen=True)
class Site:
    """One occurrence of a unit word on a line that holds a count. Its key is the path, the
    SHA-256 of the line's exact bytes, the occurrence's ordinal on the line and which copy of that
    line it is in the file, so a line that moves inside its file keeps its key."""

    path: str
    line: int
    sha: str
    occurrence: int
    copy: int
    word: str
    text: str

    @property
    def key(self) -> tuple[str, str, int, int]:
        return (self.path, self.sha, self.occurrence, self.copy)


def sites(path: str, data: bytes) -> list[Site]:
    """Every site of a file's bytes, in order. A line is split at its \\n alone, so a \\r before it
    is part of its bytes."""
    out: list[Site] = []
    copies: dict[bytes, int] = {}
    for number, raw in enumerate(data.split(b"\n"), 1):
        text = raw.decode("utf-8", errors="replace")
        plain = decoded(text)
        words = [m.group(0) for m in WORD.finditer(plain)]
        as_written = [m.group(0) for m in WORD.finditer(text)]
        if len(as_written) > len(words):
            words = as_written
        if not words or not COUNT.search(plain):
            continue
        copies[raw] = copies.get(raw, 0) + 1
        sha = hashlib.sha256(raw).hexdigest()
        for ordinal, word in enumerate(words, 1):
            out.append(Site(path, number, sha, ordinal, copies[raw], word, text.strip()))
    return out
