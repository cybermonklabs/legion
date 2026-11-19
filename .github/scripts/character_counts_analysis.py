"""The analysis of the character-count check: the grammar of a count beside the word, each
language's counters, the following of a count to its counter, and the verdict on each hit of a
file."""
from __future__ import annotations

import bisect
import re
from dataclasses import dataclass

from character_counts_lexer import (HAS_WORD, PLURAL, SLOT, WORD, Message, blanked, close_paren, code_of, decoded, language,
                                    lex, messages, split_args)

VERB = r"%(?:\[\d+\])?[-+ #0]*\d*(?:\.\d+)?[dvs]"
# A number: digits ("2,000", "32k"), in code or bold (`2000`, **2,000**), or words ("two",
# "forty-two", "two hundred"); a range of two ("1-40", "1 to 40", "between 1 and 40"); and a
# number a word before it holds with hyphens ("over-40-character").
DIGITS = r"\d[\d,]*(?<!,)(?:\.\d+)?(?:[kK]\b)?"
NUMBER_WORD = (r"\b(?i:zero|one|two|three|four|five|six|seven|eight|nine|ten|eleven|twelve|thirteen|fourteen|fifteen"
               r"|sixteen|seventeen|eighteen|nineteen|twenty|thirty|forty|fifty|sixty|seventy|eighty|ninety|hundred|thousand)\b")
WORDS = rf"{NUMBER_WORD}(?:[\s-]+(?:(?i:and)\s+)?{NUMBER_WORD})*"
WRAPPED = rf"(?:(?<![\w.+\-*`])(?:\*\*|`)(?=\d)|(?<![\w.+\-*`])){DIGITS}(?:\*\*|`)?"
HYPHENED = r"(?:(?<=over-)|(?<=under-)|(?<=than-)|(?<=most-)|(?<=least-)|(?<=to-)|(?<=at-))\d[\d,]*(?<!,)(?=-)"
NUMBER = rf"(?:{WRAPPED}|{WORDS})(?:\s*(?:-|–|—|\b(?i:to|and|or)\b)\s*(?:{DIGITS}|{WORDS}))?|{HYPHENED}"
# A constant's name written in the text as the count ("at most SEARCH_QUERY_MAX characters"): its
# value is a bound the text does not show, so it is judged as one.
NAMED = r"`?(?<![\w.$])[A-Z][A-Z0-9]*(?:_[A-Z0-9]+)+`?(?![\w(])"
TEMPLATE = r"\{\{[^{}\n]*\}\}"
CODE_COUNT = rf"\x00\d+\x00|{VERB}|{NUMBER}|{NAMED}|{TEMPLATE}"
PROSE_COUNT = rf"\$\{{[^{{}}\n]*\}}|\$\([^()\n]*\)|{TEMPLATE}|\{{[^{{}}\n]*\}}|\$#?\w+|{VERB}|{NUMBER}|{NAMED}"
# The words that may stand between a count and the word: "200 Unicode characters", "55 more
# base32 characters", "five or more characters".
QUALIFIER = (r"(?:[A-Z][\w+/.-]*|(?i:or\s+(?:more|fewer|less|so)|and\s+(?:more|fewer)|more|fewer|less|extra|whole|astral"
             r"|printable|visible|hex|hexadecimal|base32|base64(?:url)?|lower-?case|upper-?case|alphanumeric|alphabetic"
             r"|decimal|digit|numeric|control|multi-?byte|wide|single|total|unique|distinct|raw|plain|random|such|same|new"
             r"|remaining|other|valid|invalid|non-[\w-]+|whitespace|blank|leading|trailing|first|last|consecutive|legal"
             r"|allowed|special|emoji|combining|unicode|latin|chinese|japanese|korean|cyrillic|greek|arabic|hebrew|letter"
             r"|safe|unsafe|reserved|significant|real|actual|literal|escaped|encoded|url-safe)\b)")
# The nouns a count follows after the word: "character limit of 200", "characters used: %d".
NOUN = (r"(?i:limits?|caps?|count|length|budgets?|maximum|minimum|max|min|bounds?|size|width|used|left|remaining|written|read"
        r"|sent|kept|cut|total)")
CAP = r"(?i:cap|limit|bound|budget|maximum|minimum|max|min)s?"


# A count beside the word, wherever it stands in the sentence: before it, with up to three words
# between ("%d Unicode characters"); after it ("at character %d", "chars=%d", "the character limit
# of 200", "its length in characters is %d"); and a cap named in the word with no count ("the
# character cap", "the characters left for it"). The examples are in this comment, which the
# check does not read, so its own code holds no count beside the word.
def hit_pattern(count: str) -> re.Pattern:
    return re.compile(
        rf"(?P<c1>{count})(?:[\s-]+{QUALIFIER}){{0,3}}(?:-|\s+)(?P<w1>{WORD})(?!\s*=)"
        rf"|\b(?i:at|after|before|from|past)\s+(?P<w2>{WORD})\s+(?P<c2>{count})"
        rf"|\b(?P<w3>{WORD})\s*[=:]\s*(?P<c3>{count})"
        rf"|\b(?P<w4>{WORD})\s+{NOUN}\b\s*(?i:of\b|is\b|was\b|are\b|at\b|to\b|:|=|\()?\s*(?P<c4>{count})"
        rf"|\b(?i:in)\s+(?P<w5>{WORD})\s*,?\s*(?i:is\b|was\b|are\b|of\b|:|=)\s*(?P<c5>{count})"
        rf"|\b(?i:the|that|this|its|their|a|an|each|every|one)\s+(?P<w6>{WORD})\s+{CAP}\b"
        rf"(?!\s*(?i:of\b|is\b|was\b|are\b|at\b|to\b|:|=|\()?\s*(?:{count}))"
        rf"|\b(?P<w7>{WORD})\s+(?i:left|remaining)\s+(?i:for|in|to)\b"
    )


CODE_HIT = hit_pattern(CODE_COUNT)
PROSE_HIT = hit_pattern(PROSE_COUNT)
# A unit named after the word ("32,000 characters (UTF-16 units)") or before it ("200 Unicode
# characters"), and the unit it names.
NAMES_UNIT = re.compile(r"UTF-16|code units?\b|code points?\b|\bUnicode\b", re.I)
ASCII_GRAMMAR = re.compile(
    r"\bhex(?:adecimal)?\b|\ba-z\b|\[0-9a-z\]|\bbase(?:32|64(?:url)?)\b|\buuids?\b|\bSHA\b|\bsha(?:256)?\b|\bDNS\b|\bASCII\b"
    r"|lowercase letters(?: and|,) digits|\d-character (?:lower-?case )?(?:hex )?commit\b"
    r"|\[(?=[^\]]*[A-Za-z0-9]-[A-Za-z0-9])(?:[A-Za-z0-9]-[A-Za-z0-9]|[_.+=@/-])+\]"
)
EXEMPT = re.compile(r"(?://|#|<!--|--|/\*|\*)\s*characters:\s*(code points|ASCII alone|UTF-16 code units)\b[,:;]?\s*(\S.{9,})?")
PROSE_SENTENCE_END = re.compile(r"[.;!?](?=\s|$)|\n\s*\n|\n\s*(?:[-*+]|\d+\.)\s|\|")
CODE_SENTENCE_END = re.compile(r"[.;!?](?=\s|$)|\n|\|")
CLAUSE_END = re.compile(r"[,;:()]|\s[-–—]\s")
# A file whose text may spell the word with an escape of its first letter (\u0063haracters,
# \x43HARS), so it is read though the word itself is not in it.
ESCAPED = re.compile(r"\\(?:u\{?0*|U0*|x)[46]3|\\1[04]3")

# The counters, by language: the code-point counters, then each other counter with what it
# counts. A value holding a code-point counter and one of the others is an instance, and
# COUNTER_CASES below holds a planted case for each entry, written out apart from this list, so
# taking an entry off it fails that entry's case.
CODE_POINTS = {
    "go": re.compile(r"\butf8\.RuneCount(?:InString)?\(|\blen\(\[\]rune\(|\[\]rune\("),
    # A spread of an encoder's bytes ([...encoder.encode(s)].length) counts bytes.
    "ts": re.compile(r"\[\.\.\.(?![^\]]*\bencode\()[^\]]*\]\.length\b|\bArray\.from\("),
    "py": None,
    "sh": re.compile(r"\$\{#\w+\}|\bwc\s+-m\b"),
}
OTHER_COUNTERS = {
    "go": [("len()", r"\blen\(", "bytes"),
           ("Units16", r"\bUnits16\(", "UTF-16 code units"),
           ("utf16.", r"\butf16\.", "UTF-16 code units")],
    "ts": [("TextEncoder", r"\bTextEncoder\b", "bytes"),
           (".length", r"\.length\b", "UTF-16 code units"),
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
class Hit:
    path: str
    line: int
    text: str
    verdict: str
    why: str = ""


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
        found = reaching(code, name, pos, lang)
        if not found:
            return "unknown", f"{name} is not assigned in this file"
        results += [judge(value, code, offset, lang, depth + 1, seen | {name}) for value, offset in found]
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
    """Whether the script sets a UTF-8 LC_ALL for itself before pos: `export LC_ALL=C.UTF-8`, or
    `LC_ALL=C.UTF-8` alone on its line. `LC_ALL=C.UTF-8 sort` sets it for sort alone."""
    return bool(re.search(r"(?m)^\s*(?:export\s+)?LC_ALL=['\"]?[A-Za-z_]+\.UTF-8\b['\"]?[ \t]*(?:$|;|&&)", code[:pos]))


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


def assignments(name: str, lang: str) -> re.Pattern:
    """The assignments of name, in lang."""
    esc = re.escape(name)
    names_before = r"(?P<before>(?:[\w$]+[ \t]*,[ \t]*)*)"
    names_after = r"(?P<after>(?:[ \t]*,[ \t]*[\w$]+)*)"
    if lang == "sh":
        return re.compile(rf"(?:^|(?<=[\s;(&|]))(?:local\s+|export\s+|readonly\s+|declare\s+(?:-\w+\s+)*)?{esc}=")
    if lang == "py":
        return re.compile(rf"(?<![\w.]){names_before}{esc}{names_after}[ \t]*=(?!=)")
    typed = r"(?:[ \t]+[\w.\[\]*]+)?" if lang == "go" else r"(?:[ \t]*:[ \t]*[\w$<>\[\]|.]+(?:[ \t]*[|&][ \t]*[\w$<>\[\]|.]+)*)?"
    return re.compile(rf"(?<![\w$.]){names_before}{esc}{names_after}{typed}[ \t]*(?::=|(?<![=!<>+\-*/%&|^:])=(?![=>]))")


def value_at(code: str, m: re.Match, lang: str) -> tuple[str, int]:
    """The expression an assignment match gives its name, and that expression's offset. In `a, b
    := x, y` the name takes its own value; where the values are not one per name, the value is ""
    (not read)."""
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


def definition(code: str, name: str, pos: int, lang: str) -> tuple[str, int] | None:
    """The expression last assigned to name before pos in a block that holds pos, else its
    assignment at the top of the file, and that expression's offset."""
    found = list(assignments(name, lang).finditer(code))
    before = [m for m in found if m.end() <= pos and (lang == "py" or visible(code, m.end(), pos))]
    if before:
        m = before[-1]
    else:
        top = [m for m in found if lang == "py" or visible(code, 0, m.start()) and code[: m.start()].count("{") == code[: m.start()].count("}")]
        if not top:
            return None
        m = top[0]
    return value_at(code, m, lang)


def reaching(code: str, name: str, pos: int, lang: str) -> list[tuple[str, int]]:
    """Each value of name that can reach pos: definition()'s, and each later assignment in a block
    that closed before pos, an `if` or a loop that may have run (`n := utf8.RuneCount(s); if raw {
    n = len(s) }`)."""
    found = definition(code, name, pos, lang)
    if found is None:
        return []
    out = [found]
    if lang in ("go", "ts"):
        for m in assignments(name, lang).finditer(code, found[1], pos):
            if m.end() <= pos and not visible(code, m.end(), pos):
                out.append(value_at(code, m, lang))
    return out


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
            if c == "{" and lang == "go" and depth == 0 and code[k - 1] in " \t" and code[start:k].strip():
                # A Go `{` after a space opens the block of the `for`, `if` or `switch` whose
                # header holds the assignment (gofmt writes a composite literal's `{` with no
                # space), so `for i := range n {` assigns i from `range n` and not from the body.
                break
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


def compared(code: str, name: str, lang: str) -> tuple[str, str]:
    """A constant's counter: each comparison in this file that holds the constant, judged as a
    condition (`if t.length > MAX`)."""
    kinds = []
    for m in re.finditer(rf"(?<![\w$.]){re.escape(name)}(?![\w$])", code):
        line_start = code.rfind("\n", 0, m.start()) + 1
        line_end = code.find("\n", m.end())
        line = code[line_start : len(code) if line_end < 0 else line_end]
        if re.search(r"(?<![<>=!-])[<>]=?(?![<>=])|\s-(?:gt|ge|lt|le)\s", line):
            kinds.append(judge_condition(line, code, m.start(), lang))
    for kind, why in kinds:
        if kind == "other":
            return kind, why
    if any(kind == "code points" for kind, _ in kinds):
        return "code points", ""
    return "unknown", next((why for kind, why in kinds if kind == "unknown"), f"{name} is compared with no counter in this file")


def judge_count(message: Message, count: str | None, at: int, code: str, lang: str) -> tuple[str, str]:
    """The judgement of one count of a code message by its counter: ("code points" | "other" |
    "unknown", why). count is None for a cap named in the word with no number."""
    if count is None:
        kind, why = "bound", "a cap with no counter beside it"
    elif (slot := re.fullmatch(r"\x00(\d+)\x00", count)):
        expr, offset = message.exprs[int(slot.group(1))]
        kind, why = judge(expr, code, offset, lang)
    elif re.fullmatch(VERB, count):
        # Every verb before the count takes an argument, %q and %x as much as %d, so the index
        # counts each one, not only the verbs that can state a count.
        verbs = [v for v in re.finditer(r"%%|%(?:\[\d+\])?[-+ #0]*\d*(?:\.\d+)?[a-zA-Z]", message.text[:at]) if v.group() != "%%"]
        explicit = re.match(r"%\[(\d+)\]", count)
        index = int(explicit.group(1)) - 1 if explicit else len(verbs)
        if message.args is None or index >= len(message.args):
            return "unknown", "its format verb takes no argument in this call"
        expr, offset = message.args[index]
        kind, why = judge(expr, code, offset, lang)
    elif re.fullmatch(TEMPLATE, count):
        kind, why = "unknown", "a template's value, which the check does not follow"
    else:
        kind, why = "bound", "a bound with no counter beside it"
    if kind == "bound":
        # A number or a constant is a bound: its counter is the one the `if` or `case` the
        # message answers compares with it, or, for a constant, each comparison of it.
        condition = header(code, message.start, lang)
        constant = re.fullmatch(NAMED, count or "") or (count is None and re.search(NAMED, message.text))
        if condition:
            kind, why = judge_condition(condition, code, message.start, lang)
            if kind == "unknown":
                why = "a bound, and " + why
        elif constant:
            kind, why = compared(code, constant.group().strip("`"), lang)
            if kind == "unknown":
                why = "a bound, and " + why
        else:
            kind = "unknown"
    return kind, why


# ---------------------------------------------------------------- the verdicts


def sentence(text: str, a: int, b: int, prose: bool) -> tuple[int, int]:
    ends = PROSE_SENTENCE_END if prose else CODE_SENTENCE_END
    start = 0
    for m in ends.finditer(text, max(0, a - 400), a):
        start = m.end()
    m = ends.search(text, b)
    return start, m.start() if m else len(text)


# The unit a hit names: "UTF-16 code units" or "code points", named between its count and the
# word ("200 Unicode characters") or in the 40 characters after the word inside its sentence
# ("32,000 characters (UTF-16 units)"); "" when it names none.
def named_unit(text: str, match: re.Match, prose: bool) -> str:
    word = next(g for g in ("w1", "w2", "w3", "w4", "w5", "w6", "w7") if match.group(g) is not None)
    _, e = sentence(text, match.start(), match.end(), prose)
    found = NAMES_UNIT.search(text, match.end(), min(e, match.end() + 40))
    if found is None and word == "w1":
        found = NAMES_UNIT.search(text, match.end("c1"), match.start("w1"))
    if found is None:
        return ""
    return "code points" if re.match(r"(?i)code points?|unicode", found.group()) else "UTF-16 code units"


# "ASCII grammar" when the clause that holds the count names a grammar that admits ASCII alone.
# The clause runs between the commas, colons, semicolons, parentheses and dashes of the count's
# sentence, and takes a parenthesis right after the word. A count inside a parenthesis explains
# what stands before it, so its clause is the whole parenthesis and the clause before it ("a
# DNS-1035 label (a letter first, at most 63 characters)"). A grammar word in another clause
# ("issue %s, a uuid, has a body of %d characters") describes something else.
def ascii_grammar(text: str, match: re.Match, prose: bool) -> str:
    s, e = sentence(text, match.start(), match.end(), prose)
    depth, opened = 0, -1
    for k in range(match.start() - 1, s - 1, -1):
        if text[k] == ")":
            depth += 1
        elif text[k] == "(":
            if depth == 0:
                opened = k
                break
            depth -= 1
    start, before = s, (opened if opened >= 0 else match.start())
    for m in CLAUSE_END.finditer(text, s, before):
        start = m.end()
    if opened >= 0:
        close = text.find(")", match.end(), e)
        end = e if close < 0 else close + 1
    else:
        end = match.end()
        paren = re.match(r"\s*\(", text[end:e])
        if paren:
            close = text.find(")", end + paren.end(), e)
            end = e if close < 0 else close + 1
        m = CLAUSE_END.search(text, end, e)
        end = m.start() if m else e
    for grammar in ASCII_GRAMMAR.finditer(text, s, e):
        if grammar.start() < end and grammar.end() > start:
            return "ASCII grammar"
    return ""


def not_empty(text: str, count: str | None, at: int) -> str:
    """"not empty" for "at least one character" and "one or more characters": a count that says
    only that the text is not empty, which every unit agrees on."""
    if count is None or not re.fullmatch(r"(?i)one|1", count):
        return ""
    if re.search(r"(?i)\bat\s+least\s+$", text[max(0, at - 12) : at]) or re.match(r"(?i)[\s-]+or\s+more\b", text[at + len(count) :]):
        return "not empty"
    return ""


def exempt(lines: list[str], first: int, last: int, message_lines: list[int] = (), named: str = "", ascii_only: bool = False) -> str:
    """The exemption a comment on the message's lines or the two lines above gives it, when no
    other message starts between that comment and this one. Its unit must be the one the message
    names, or ASCII alone; a message that names none is exempt as code points or ASCII alone.
    ascii_only takes ASCII alone only: a text of ASCII alone is one length in every unit, so it
    passes even a counter the check followed."""
    for k in range(max(0, first - 2), last + 1):
        marker = EXEMPT.search(lines[k])
        reason = ((marker.group(2) or "") if marker else "").strip().removesuffix("-->").strip()
        if not (marker and reason):
            continue
        if k < first and bisect.bisect_left(message_lines, first) > bisect.bisect_right(message_lines, k):
            continue
        if marker.group(1) not in ("ASCII alone",) + (() if ascii_only else (named or "code points",)):
            continue
        return f"exempt, {marker.group(1)}: {reason}"
    return ""


def verdict(message: Message, match: re.Match, count: str | None, at: int, code: str, lang: str, lines: list[str],
            first: int, line: int, message_lines: list[int]) -> tuple[str, str]:
    """The verdict on a hit of a code message and its reason. The count is judged by its counter
    wherever it stands; a unit the message names passes only where its counter counts that unit,
    and an exemption only where the check cannot follow the counter."""
    if ascii_grammar(message.text, match, False):
        return "ASCII grammar", ""
    if not_empty(message.text, count, at):
        return "not empty", ""
    kind, why = judge_count(message, count, at, code, lang)
    named = named_unit(message.text, match, False)
    if kind == "code points":
        if not named or named == "code points":
            return ("names its unit" if named else "code points"), ""
        return "instance", f"it names {named}, and its counter counts code points"
    if kind == "other":
        counted = why.split(" counts ", 1)[1] if " counts " in why else ""
        if named and counted.startswith(named):
            return "names its unit", ""
        marker = exempt(lines, first, line, message_lines, named, ascii_only=True)
        if marker:
            return marker, ""
        return "instance", (f"it names {named}, and {why}" if named else why)
    marker = exempt(lines, first, line, message_lines, named)
    if marker:
        return marker, ""
    return "instance", "cannot judge: " + why + (", so the unit it names is not followed" if named else "")


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


def counts(text: str, rx: re.Pattern):
    """Each hit of rx in text: the match, its count (None for a cap with no number) and where the
    count starts."""
    for match in rx.finditer(text):
        group = next((g for g in ("c1", "c2", "c3", "c4", "c5") if match.group(g) is not None), None)
        yield match, (match.group(group) if group else None), (match.start(group) if group else match.start())


def scan(path: str, content: str) -> list[Hit]:
    if not HAS_WORD.search(content) and not ESCAPED.search(content):
        return []
    lang = language(path, content[:200])
    lines = content.split("\n")
    starts = [0]
    for line in lines[:-1]:
        starts.append(starts[-1] + len(line) + 1)
    hits: list[Hit] = []
    if lang == "prose":
        comment = next((rx for name, rx in PROSE_COMMENTS if name.search(path)), None)
        text, offsets = decoded(blanked(content, [m.span() for m in comment.finditer(content)]) if comment else content, 0, "prose")
        for match, count, at in counts(text, PROSE_HIT):
            line = line_of(starts, offsets[match.start()])
            passed = (named_unit(text, match, True) and "names its unit") or ascii_grammar(text, match, True) \
                or not_empty(text, count, at) or exempt(lines, line, line)
            why = "" if passed else f"cannot judge: a {'count' if count else 'cap'} in prose names no counter"
            shown = " ".join(text[max(0, match.start() - 80) : match.end() + 60].split())
            hits.append(Hit(path, line + 1, shown, passed or "instance", why))
        return hits
    lits, comments = lex(content, lang)
    code = code_of(content, lits, comments)

    def resolve(name: str, pos: int):
        found = definition(code, name, pos, lang)
        return None if found is None else (content[found[1] : found[1] + len(found[0])], found[1])

    found = []
    # A name is followed to the word only in a file that writes the word as a literal of its own.
    file_messages = messages(content, code, lits, lang, lang == "ts" and path.endswith((".tsx", ".jsx")),
                             resolve if PLURAL.search(content) else None)
    message_lines = sorted({line_of(starts, message.start) for message in file_messages})
    for message in file_messages:
        found += [(message, match, count, at) for match, count, at in counts(message.text, CODE_HIT)]
    found.sort(key=lambda f: f[0].offsets[min(f[1].start(), len(f[0].offsets) - 1)])
    for message, match, count, at in found:
        line = line_of(starts, message.offsets[min(match.start(), len(message.offsets) - 1)])
        judged, why = verdict(message, match, count, at, code, lang, lines, line_of(starts, message.start), line, message_lines)
        shown = re.sub(r"\x00(\d+)\x00", lambda m: "${" + message.exprs[int(m.group(1))][0].strip() + "}", message.text)
        hits.append(Hit(path, line + 1, " ".join(shown.split())[:160], judged, why))
    return hits
