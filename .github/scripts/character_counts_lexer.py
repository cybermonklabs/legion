"""The lexer of the character-count check (.github/scripts/check-character-counts.sh): it reads a
file's language, every string literal and comment of it, and builds each message a program
builds from them, each value written as a slot."""
from __future__ import annotations

import re
from dataclasses import dataclass

WORD = r"(?:[Cc]haracters?|[Cc]hars?)\b"
HAS_WORD = re.compile(WORD)
SLOT = "\x00"
PLURAL = re.compile(r"([\"'`])[Cc]haracters?\1")


@dataclass
class Lit:
    start: int
    end: int
    parts: list  # ("text", text, offset) or ("expr", expression, offset)



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
