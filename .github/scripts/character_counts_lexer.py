"""The lexer of the character-count check (.github/scripts/check-character-counts.sh): it reads a
file's language, every string literal and comment of it, and builds each message a program
builds from them, each value written as a slot."""
from __future__ import annotations

import re
import unicodedata
from dataclasses import dataclass

# The word, in any case: "characters", "Character", "CHARS".
WORD = r"(?i:characters?|chars?)\b"
HAS_WORD = re.compile(WORD)
SLOT = "\x00"
PLURAL = re.compile(r"([\"'`])(?i:characters?)\1")


@dataclass
class Lit:
    start: int
    end: int
    parts: list  # ("text", text, offset) or ("expr", expression, offset)
    # The escapes its text is written in ("go", "ts", "py", "sh"), read as the characters they
    # stand for; None where the program prints the text as written (a raw string).
    escapes: str | None = None
    # "" for a quoted literal; for a shell here-document body, "text" where it is read as text and
    # "fed" where it is another program's code, which only stands for its range: its own
    # literals are read in that program's language.
    heredoc: str = ""



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
    return Lit(lit.start + by, lit.end + by, [(kind, text, offset + by) for kind, text, offset in lit.parts], lit.escapes, lit.heredoc)


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
    return Lit(j, end, parts, "ts"), end, nested


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
        lits.append(Lit(j, min(k + 1, n), [("text", src[j + 1 : k], j + 1)], "go" if tok != "`" else None))
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
        lits.append(Lit(j, k + 1 if closed else k, [("text", src[j + 1 : k], j + 1)], "ts"))
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
        lits.append(Lit(j, end, parts, None if "r" in prefix else "py"))
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
                    lits.append(Lit(body, body_end, [], None, "fed"))
                else:
                    parts = [("text", src[body:body_end], body)] if quoted else sh_parts(src, body, body_end)
                    lits.append(Lit(body, body_end, parts, None, "text"))
                i = term.end() if term else n
            pending = []
            continue
        if tok == "$'":
            k = quoted_end(src, j + 1, "'", True, True)
            lits.append(Lit(j, min(k + 1, n), [("text", src[j + 2 : k], j + 2)], "sh"))
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
    as the space around it. A fed here-document body stays code: its literals are blanked."""
    return blanked(blanked(src, comments), [(lit.start, lit.end) for lit in lits if lit.heredoc != "fed"], "\x01")


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


# ---------------------------------------------------------------- escapes


SIMPLE_ESCAPES = {"n": "\n", "t": "\t", "r": "\r", "a": "\a", "b": "\b", "f": "\f", "v": "\v", "e": "\x1b", "0": "\0"}
ESCAPES = {
    "go": re.compile(r"\\(?:u[0-9a-fA-F]{4}|U[0-9a-fA-F]{8}|x[0-9a-fA-F]{2}|[0-7]{3}|.)", re.S),
    "ts": re.compile(r"\\(?:u\{[0-9a-fA-F]{1,6}\}|u[0-9a-fA-F]{4}|x[0-9a-fA-F]{2}|.)", re.S),
    "py": re.compile(r"\\(?:N\{[^}\n]*\}|u[0-9a-fA-F]{4}|U[0-9a-fA-F]{8}|x[0-9a-fA-F]{2}|[0-7]{1,3}|.)", re.S),
    "sh": re.compile(r"\\(?:u[0-9a-fA-F]{1,4}|U[0-9a-fA-F]{1,8}|x[0-9a-fA-F]{1,2}|[0-7]{1,3}|.)", re.S),
    # A JSON file's own escape: \u0063 is "c". Markdown, YAML and plain text have no escape of
    # their own, so a \u0063 there is the six characters a reader sees.
    "json": re.compile(r"\\u[0-9a-fA-F]{4}"),
}


def unescaped(escape: str, dialect: str) -> str:
    """The character an escape stands for. A Go \\x or octal byte past ASCII is U+FFFD: it is a
    part of a character's UTF-8, and no character itself."""
    body = escape[1:]
    try:
        if body[:1] in ("u", "U") and len(body) > 1:
            value = int(body[1:].strip("{}"), 16)
        elif body[:1] == "x" and len(body) > 1:
            value = int(body[1:], 16)
        elif body.startswith("N{"):
            return unicodedata.lookup(body[2:-1])
        elif dialect != "ts" and body[:1] in tuple("01234567"):
            value = int(body, 8)
        else:
            return "" if body == "\n" else SIMPLE_ESCAPES.get(body, body)
    except (ValueError, KeyError):
        return escape
    if dialect == "go" and body[:1] not in ("u", "U") and value > 0x7F:
        return "\ufffd"
    return "\ufffd" if 0xD800 <= value <= 0xDFFF or value > 0x10FFFF else chr(value)


def decoded(text: str, offset: int, dialect: str | None) -> tuple[str, list[int]]:
    """text with each escape of its dialect written as the character it stands for, so
    "\\u0063haracters" reads as the word, and the file offset of each character of the result."""
    if dialect is None or "\\" not in text:
        return text, list(range(offset, offset + len(text)))
    out, offsets, k = [], [], 0
    for m in ESCAPES[dialect].finditer(text):
        out.append(text[k : m.start()])
        offsets.extend(range(offset + k, offset + m.start()))
        char = unescaped(m.group(), dialect)
        out.append(char)
        offsets.extend([offset + m.start()] * len(char))
        k = m.end()
    out.append(text[k:])
    offsets.extend(range(offset + k, offset + len(text)))
    return "".join(out), offsets


# ---------------------------------------------------------------- building a message


WORD_CALL = re.compile(r"\s*[\w$.]+\s*\((.*)\)\s*", re.S)
QUOTED = re.compile(r"\s*([\"'`]).*\1\s*", re.S)
BARE_NAME = re.compile(r"\s*[A-Za-z_$][\w$]*\s*")


def word_of(expr: str, offset: int, resolve) -> tuple | None:
    """None when expr is a value. When it reads as the word, (None, None), or the count it writes
    before the word and its offset: an expression that chooses the word (`n === 1 ? "character" :
    "characters"`), a call that writes a count and the word (`plural(n, "character")`), or a name
    assigned one of those."""
    if PLURAL.search(expr):
        call = WORD_CALL.fullmatch(expr)
        if call:
            inner, base = call.group(1), offset + call.start(1)
            for a, b in split_args(inner + ")", 0):
                if inner[a:b].strip() and not QUOTED.fullmatch(inner[a:b]):
                    return inner[a:b], base + a
        return None, None
    if resolve is not None and BARE_NAME.fullmatch(expr):
        found = resolve(expr.strip(), offset)
        if found is not None and PLURAL.search(found[0]):
            return word_of(found[0], found[1], None)
    return None


class Builder:
    """One message as a program builds it: its text, the file offset of each character, and each
    value it holds, written in the text as a slot."""

    def __init__(self, resolve=None, exprs: list | None = None):
        self.parts: list[str] = []
        self.offsets: list[int] = []
        self.exprs: list = list(exprs or [])
        self.resolve = resolve

    def add(self, text: str, offsets) -> None:
        self.parts.append(text)
        self.offsets.extend(offsets)

    def lit(self, lit: Lit) -> None:
        for kind, part, offset in lit.parts:
            if kind == "text":
                self.add(*decoded(part, offset, lit.escapes))
            else:
                self.value(part, offset)

    def value(self, expr: str, offset: int, place: int | None = None) -> None:
        """A value: a slot, or the word where the value is the word. place is the offset its
        slot stands at, where that is not the expression's own (a format field's)."""
        place = offset if place is None else place
        word = word_of(expr, offset, self.resolve)
        if word is None:
            slot = f"{SLOT}{len(self.exprs)}{SLOT}"
            self.exprs.append((expr, offset))
            self.add(slot, [place] * len(slot))
            return
        count, at = word
        if count is not None:
            self.value(count, at, place)
            self.add(" ", [place])
        self.add("characters", [place] * len("characters"))

    def text(self) -> str:
        return "".join(self.parts)

    def message(self, start: int, end: int, args: list | None) -> Message:
        return Message(self.text(), self.offsets, self.exprs, start, end, args)


FORMAT_VERB = re.compile(r"%%|%(?:\[\d+\])?[-+ #0]*\d*(?:\.\d+)?[a-zA-Z]")


def messages(src: str, code: str, lits: list[Lit], lang: str, jsx: bool = False, resolve=None) -> list[Message]:
    """The messages of a code file: each call or array whose arguments the program writes as one
    text, each shell command's words, each run of JSX text, and each other literal with what
    joins to it and its call's arguments. resolve(name, offset) gives the source of the
    expression a name is assigned, and its offset, or None."""
    lits = [lit for lit in lits if lit.heredoc != "fed"]
    by_start = {lit.start: lit for lit in lits}
    by_end = {lit.end: lit for lit in lits}
    used: set[int] = set()
    out = []
    if lang == "sh":
        out += shell_commands(src, code, lits, by_start, used, resolve)
    else:
        out += joined_arguments(src, code, lits, lang, by_start, by_end, used, resolve)
    if jsx:
        out += jsx_texts(src, code, resolve)
    for lit in sorted(lits, key=lambda l: l.start):
        if lit.start in used:
            continue
        elements, left, right = chain(code, lit, lang, by_start, by_end, src)
        built = Builder(resolve)
        for element in elements:
            if isinstance(element, Lit):
                used.add(element.start)
                built.lit(element)
            else:
                built.value(*element)
        text = built.text()
        if not HAS_WORD.search(text) and not re.search(r"%[-+ #0]*\d*(?:\.\d+)?[svq]|\{[^{}]*\}|%\(", text):
            continue
        message = formatted(built.message(left, right, call_args(src, code, left, right, lang, by_start)), src, code, lang, resolve)
        if HAS_WORD.search(message.text):
            out.append(message)
    return out


def lit_text(lit: Lit) -> str:
    return "".join(decoded(part, 0, lit.escapes)[0] for kind, part, _ in lit.parts if kind == "text")


def opener(code: str, pos: int) -> int:
    """The index of the bracket that holds pos, or -1."""
    depth, k = 0, pos - 1
    while k >= 0:
        c = code[k]
        if c in ")]}":
            depth += 1
        elif c in "([{":
            if depth == 0:
                return k
            depth -= 1
        k -= 1
    return -1


def closer(code: str, k: int) -> int:
    """The index of the bracket that closes the one at k, or the end."""
    depth, n = 0, len(code)
    while k < n:
        c = code[k]
        if c in "([{":
            depth += 1
        elif c in ")]}":
            depth -= 1
            if depth == 0:
                return k
        k += 1
    return n


NOT_A_CALL = set("if for while switch return func function catch elif and or not in of typeof await yield case else with assert lambda".split())
SIMPLE_VALUE = re.compile(r"[^\n{};]{1,120}")


def joined_arguments(src, code, lits, lang, by_start, by_end, used, resolve) -> list[Message]:
    """Each call or TypeScript array whose separate arguments the program writes as one text,
    read as one message: fmt.Println("cut", len(s), "characters"), print(...), console.log(...),
    [a, b].join(" "). A call whose literals hold a format verb formats instead, so its format
    literal is read with its arguments. A value that is no simple expression (a function, a
    block) ends the sentence."""
    out, seen = [], set()
    for lit in sorted(lits, key=lambda l: l.start):
        if lit.start in used or not HAS_WORD.search(lit_text(lit)):
            continue
        at = opener(code, lit.start)
        if at < 0 or at in seen:
            continue
        seen.add(at)
        close = closer(code, at)
        sep, extra = " ", []
        if code[at] == "(":
            name_end = skip_space(code, at, -1)
            name = code[operand_back(code, name_end) : name_end]
            if not re.fullmatch(r"(?:[\w$]+\.)*[A-Za-z_$][\w$]*", name) or name.rsplit(".", 1)[-1] in NOT_A_CALL:
                continue
        elif code[at] == "[" and lang == "ts" and re.match(r"\s*\.join\(", code[close + 1 :]):
            join_at = code.index("(", close + 1)
            first = skip_space(code, join_at + 1, 1)
            if first in by_start:
                sep, extra = lit_text(by_start[first]), [first]
            elif code[first : first + 1] != ")":
                continue
            else:
                sep = ","
        else:
            continue
        spans = [(skip_space(code, a, 1), skip_space(code, b, -1)) for a, b in split_args(code, at + 1)]
        spans = [(a, b) for a, b in spans if a < b and not (lang == "py" and re.match(r"[A-Za-z_]\w*\s*=(?!=)", code[a:b]))]
        if len(spans) < 2:
            continue
        args = []
        for a, b in spans:
            if a in by_start:
                elements, left, right = chain(code, by_start[a], lang, by_start, by_end, src)
                if left == a and right == b:
                    args.append((a, b, elements))
                    continue
            args.append((a, b, None))
        literals = [e for _, _, elements in args if elements for e in elements if isinstance(e, Lit)]
        if any(v.group() != "%%" for e in literals for v in FORMAT_VERB.finditer(lit_text(e))):
            continue
        built = Builder(resolve)
        for i, (a, b, elements) in enumerate(args):
            if i:
                built.add(sep, [a] * len(sep))
            if elements is not None:
                for element in elements:
                    if isinstance(element, Lit):
                        built.lit(element)
                    else:
                        built.value(*element)
            elif SIMPLE_VALUE.fullmatch(src[a:b]) and "=>" not in src[a:b] and not re.match(r"(?:func|function|async)\b", src[a:b]):
                built.value(src[a:b], a)
            else:
                built.add("\n", [a])
        if not HAS_WORD.search(built.text()):
            continue
        used.update(e.start for e in literals)
        used.update(extra)
        out.append(built.message(at + 1, close, None))
    return out


def formatted(message: Message, src: str, code: str, lang: str, resolve) -> Message:
    """message with its format fields read: each field of Python's str.format and each key of a %
    mapping as a slot of its value, and a %s, %v or %q whose argument is the word as the word
    (fmt.Sprintf("%d %s", len(s), "characters"))."""
    if lang == "py":
        q = skip_space(code, message.end, 1)
        if code.startswith(".format(", q):
            message = python_fields(message, src, code, q + len(".format("), resolve)
        elif code[q : q + 1] == "%":
            r = skip_space(code, q + 1, 1)
            if code[r : r + 1] == "{":
                message = python_mapping(message, src, code, r, resolve)
    if message.args and not re.search(r"%\[\d+\]", message.text):
        message = word_arguments(message, src, resolve)
    return message


def rewritten(message: Message, rx: re.Pattern, value_of, resolve) -> Message:
    """message with each match of rx written as value_of(match): a (expression, offset) read as a
    value, a text, or None to keep the match as it is."""
    built = Builder(resolve, message.exprs)
    k = 0
    for m in rx.finditer(message.text):
        if SLOT in m.group():
            continue
        found = value_of(m)
        if found is None:
            continue
        built.add(message.text[k : m.start()], message.offsets[k : m.start()])
        if isinstance(found, str):
            built.add(found, [message.offsets[m.start()]] * len(found))
        else:
            built.value(found[0], found[1], message.offsets[m.start()])
        k = m.end()
    built.add(message.text[k:], message.offsets[k:])
    return built.message(message.start, message.end, message.args)


def python_fields(message: Message, src: str, code: str, k: int, resolve) -> Message:
    positional, named = [], {}
    for a, b in split_args(code, k):
        a, b = skip_space(code, a, 1), skip_space(code, b, -1)
        keyword = re.match(r"([A-Za-z_]\w*)\s*=(?!=)", code[a:b])
        if keyword:
            v = skip_space(code, a + keyword.end(), 1)
            named[keyword.group(1)] = (src[v:b], v)
        elif a < b:
            positional.append((src[a:b], a))
    auto = iter(range(len(positional)))

    def value_of(m: re.Match):
        if m.group() in ("{{", "}}"):
            return m.group()[0]
        name = re.split(r"[.\[!:]", m.group(1), maxsplit=1)[0]
        if name == "":
            index = next(auto, None)
            return None if index is None else positional[index]
        if name.isdigit():
            return positional[int(name)] if int(name) < len(positional) else None
        return named.get(name)

    return rewritten(message, re.compile(r"\{\{|\}\}|\{([^{}]*)\}"), value_of, resolve)


def python_mapping(message: Message, src: str, code: str, r: int, resolve) -> Message:
    mapping = {}
    for a, b in split_args(code, r + 1):
        entry = re.match(r"\s*([\"'])(\w+)\1\s*:\s*", src[a:b])
        if entry:
            mapping[entry.group(2)] = (src[a + entry.end() : b].strip(), a + entry.end())
    return rewritten(message, re.compile(r"%\((\w+)\)[-+ #0]*\d*(?:\.\d+)?[a-zA-Z]"), lambda m: mapping.get(m.group(1)), resolve)


def word_arguments(message: Message, src: str, resolve) -> Message:
    """A %s, %v or %q whose argument is the word reads as the word, and leaves the arguments of
    the verbs after it as they were."""
    verbs = [v for v in FORMAT_VERB.finditer(message.text) if v.group() != "%%"]
    words = {}
    for i, verb in enumerate(verbs):
        if i < len(message.args) and verb.group()[-1] in "svq":
            expr, offset = message.args[i]
            if word_of(src[offset : offset + len(expr)], offset, resolve) is not None:
                words[verb.start()] = i
    if not words:
        return message

    def value_of(m: re.Match):
        if m.start() not in words:
            return None
        expr, offset = message.args[words[m.start()]]
        return src[offset : offset + len(expr)], offset

    out = rewritten(message, FORMAT_VERB, value_of, resolve)
    out.args = [arg for i, arg in enumerate(message.args) if i not in words.values()]
    return out


SH_NOT_MESSAGES = set("[ [[ test local export declare readonly typeset case for select return exit shift set unset read "
                      "cd source . eval trap wait kill printf exec function alias let mapfile readarray getopts".split())
SH_PREFIX = set("if then else elif do while until ! time { (".split())
SH_ECHO = {"echo", "print"}


def shell_commands(src, code, lits, by_start, used, resolve) -> list[Message]:
    """Each shell command's words after its name, read as the text it prints: `echo "the title is
    $n" characters` is one message. echo's words are one sentence, as are a run of unquoted words;
    each quoted argument of another command is a sentence of its own. printf is read from its
    format, and the words of [, test and the assignments are no message."""
    skips = sorted((lit.start, lit.end) for lit in lits if lit.heredoc)
    out = []
    for words in shell_words(code, by_start, skips):
        names = [code[a:b] for a, b in words]
        while words and (names[0] in SH_PREFIX or re.match(r"[A-Za-z_]\w*(?:\[[^\]]*\])?\+?=", names[0])):
            words, names = words[1:], names[1:]
        if len(words) < 2 or names[0] in SH_NOT_MESSAGES:
            continue
        args = [w for w, name in zip(words[1:], names[1:]) if not re.match(r"\d*[<>]|&>", name)]
        if not args:
            continue
        built, members, quoted_before = Builder(resolve), [], False
        for i, (a, b) in enumerate(args):
            quoted = any(k in by_start for k in range(a, b))
            if i:
                sep = " " if names[0] in SH_ECHO or not (quoted or quoted_before) else "\n"
                built.add(sep, [a])
            quoted_before = quoted
            k = a
            while k < b:
                lit = by_start.get(k)
                if lit is not None:
                    built.lit(lit)
                    members.append(lit.start)
                    k = lit.end
                    continue
                e = k
                while e < b and e not in by_start:
                    e += 1
                for kind, part, offset in sh_parts(src, k, e):
                    if kind == "text":
                        built.add(part, range(offset, offset + len(part)))
                    else:
                        built.value(part, offset)
                k = e
        if HAS_WORD.search(built.text()):
            used.update(members)
            out.append(built.message(args[0][0], args[-1][1], None))
    return out


def shell_words(code: str, by_start: dict, skips: list):
    """The words of each simple command of a shell file's code, as (start, end) spans: a
    command ends at a newline, ;, |, & or a parenthesis outside $( ), ${ } and quotes, and a
    here-document body is no command."""
    n, k, words, start = len(code), 0, [], None
    skip = iter(skips + [(n + 1, n + 1)])
    next_skip = next(skip)
    while k < n:
        if k >= next_skip[0]:
            if start is not None:
                words.append((start, k))
                start = None
            if words:
                yield words
            words = []
            k = max(k, next_skip[1])
            next_skip = next(skip)
            continue
        c = code[k]
        if k in by_start:
            start = k if start is None else start
            k = by_start[k].end
            continue
        if c == "$" and code[k + 1 : k + 2] in ("(", "{"):
            start = k if start is None else start
            k = (close_paren(code, k + 2) if code[k + 1] == "(" else code.find("}", k)) + 1
            k = n if k <= 0 else k
            continue
        if c == "\\" and code[k + 1 : k + 2] == "\n":
            k += 2
            continue
        if c in " \t\r" or c in "\n;|&()":
            if start is not None:
                words.append((start, k))
                start = None
            if c not in " \t\r" and words:
                yield words
                words = []
            k += 1
            continue
        start = k if start is None else start
        k += 1
    if start is not None:
        words.append((start, n))
    if words:
        yield words


JSX_BEFORE = set("(,=?:&|{}>[;")


def jsx_tag(code: str, k: int, inside: bool) -> int:
    """The index after the JSX tag that opens at k, or -1 when k opens none. A < right after a
    name is a generic's (useState<string>); outside JSX text a tag follows an operator, a
    bracket or `return`."""
    if code[k + 1 : k + 2] not in ("/", ">") and not code[k + 1 : k + 2].isalpha():
        return -1
    if k > 0 and (code[k - 1].isalnum() or code[k - 1] in "_$."):
        return -1
    if not inside:
        p = skip_space(code, k, -1)
        if p > 0 and code[p - 1] not in JSX_BEFORE and code[max(0, p - 6) : p] != "return":
            return -1
    depth, q, n = 0, k + 1, len(code)
    while q < n:
        c = code[q]
        if c == "{":
            depth += 1
        elif c == "}":
            depth -= 1
        elif c == ">" and depth == 0:
            return q + 1
        elif c == "<" and depth == 0:
            return -1
        q += 1
    return -1


def jsx_texts(src: str, code: str, resolve) -> list[Message]:
    """Each run of JSX text, from a tag to the next, read as a message: its text, each {value}
    as a slot, and a {value} that chooses the word as the word. Text is read only inside an
    element: the tags are counted in order, so code after an element's closing tag is no text.
    A {value} that holds an element ends the run, and the element's own text is read in turn."""
    out, k, n, depth = [], 0, len(code), 0
    while (k := code.find("<", k)) >= 0:
        end = jsx_tag(code, k, depth > 0)
        if end < 0:
            k += 1
            continue
        if code[k + 1] == "/":
            depth = max(0, depth - 1)
        elif code[end - 2] != "/":
            depth += 1
        k = end
        if depth == 0:
            continue
        built, q, run = Builder(resolve), end, end
        while q < n and code[q] != "<":
            if code[q] == "{":
                e = closer(code, q)
                if "<" in code[q:e]:
                    break
                built.add(src[run:q], range(run, q))
                if code[q + 1 : e].strip():
                    built.value(src[q + 1 : e], q + 1)
                q = run = e + 1
                continue
            q += 1
        built.add(src[run:q], range(run, q))
        if HAS_WORD.search(built.text()):
            out.append(built.message(end, q, None))
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
