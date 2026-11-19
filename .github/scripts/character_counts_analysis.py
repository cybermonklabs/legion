"""The analysis of the character-count check: the grammar of a count beside the word, each
language's counters, the following of a count to its counter, and the verdict on each hit of a
file."""
from __future__ import annotations

import re
from dataclasses import dataclass

from character_counts_lexer import HAS_WORD, PLURAL, SLOT, WORD, Message, blanked, close_paren, code_of, language, lex, messages, split_args

VERB = r"%(?:\[\d+\])?[-+ #0]*\d*(?:\.\d+)?[dvs]"
NUMBER = r"(?<![\w.+\-])\d[\d,]*(?<!,)"
# A constant's name written in the text as the count ("at most SEARCH_QUERY_MAX characters"): its
# value is a bound the text does not show, so it is judged as one.
NAMED = r"`?(?<![\w.$])[A-Z][A-Z0-9]*(?:_[A-Z0-9]+)+`?(?![\w(])"
CODE_COUNT = rf"\x00\d+\x00|{VERB}|{NUMBER}|{NAMED}"
PROSE_COUNT = rf"\$\{{[^{{}}\n]*\}}|\$\([^()\n]*\)|\{{[^{{}}\n]*\}}|\$#?\w+|{VERB}|{NUMBER}|{NAMED}"


def hit_pattern(count: str) -> re.Pattern:
    return re.compile(
        rf"(?P<c1>{count})(?:-|\s+)(?P<w1>{WORD})(?!\s*=)"
        rf"|\b(?:at|after|before|from|past)\s+(?P<w2>{WORD})\s+(?P<c2>{count})"
        rf"|\b(?P<w3>{WORD})\s*[=:]\s*(?P<c3>{count})"
    )


CODE_HIT = hit_pattern(CODE_COUNT)
PROSE_HIT = hit_pattern(PROSE_COUNT)
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
