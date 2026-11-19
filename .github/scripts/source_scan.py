"""The one reading of a repository's source files that check-go-offline.sh and
check-omp-offline.sh share: which files are source and in which language, how a file is read, how
Go, TypeScript and Python are lexed into code with their comments and the text of their string
literals blanked, how shell is read the same way, and where a reason comment counts. Each check
keeps only its own rule: what a site is, and what makes one pass.

Which string literals stay code is the caller's input (`keep`), never a second lexer: the Oh My Pi
check keeps the variable a door reads and the binary's own name. So a fixture or a message that
spells a command starts nothing in either check, and a correction to how a language is read is
made here once, for both.

It reads statements too, as each language parses them: Source gives a Go or TypeScript file's
calls with their arguments across lines, its assignments and the value a name is bound to, and the
environment a call's own options (TypeScript) or a Cmd's own Env (Go) set; python_tree and its
helpers do the same for Python with the ast module; shell_script gives a shell text's simple
commands, a continued line one line, a $(...) and `...` read as commands of their own, a heredoc
body as no command, each with its NAME=value prefix, its function and its place in a pipeline.
An environment is a list of elements in the order a program makes them (see INHERIT).

Imported with PYTHONPATH naming the directory this file lives in, as the checks' own lines say.
Its own test is source_scan.test.sh.
"""
import ast
import bisect
import os
import re
import shlex
import sys
from pathlib import Path

# Directories no check reads: a test's inputs, installed packages, build output and version control.
SKIP_DIRS = frozenset({"testdata", "node_modules", "dist", ".git", ".jj"})

# A test file by its name, in each language.
TEST_FILE = re.compile(r"(_test\.go|\.(test|spec)\.[cm]?[jt]sx?|\.test\.sh|_test\.py)$|(^|/)test_[^/]+\.py$")

# A line that is a comment, by its language: where a reason may stand above the line it covers.
COMMENT = {
    "go": re.compile(r"^\s*(//|/\*|\*)"),
    "ts": re.compile(r"^\s*(//|/\*|\*|\{/\*)"),
    "py": re.compile(r"^\s*#"),
    "sh": re.compile(r"^\s*#"),
}
REASON_WINDOW = 5


def language(path):
    """go, ts (TypeScript and JavaScript: .ts .tsx .mts .cts .js .jsx .mjs .cjs, and no .d.ts,
    which declares and runs nothing), py, sh (.sh, .bash), or None."""
    if path.endswith(".go"):
        return "go"
    if re.search(r"\.[cm]?[jt]sx?$", path):
        return None if re.search(r"\.d\.[cm]?ts$", path) else "ts"
    if path.endswith(".py"):
        return "py"
    if path.endswith((".sh", ".bash")):
        return "sh"
    return None


def is_test_file(path):
    return bool(TEST_FILE.search(path))


def sources(root="."):
    """(path, language) of every source file under root, in a stable order: each path relative to
    root, no directory SKIP_DIRS names, no symbolic link."""
    for directory, dirs, files in os.walk(root):
        dirs[:] = sorted(d for d in dirs if d not in SKIP_DIRS)
        for name in sorted(files):
            path = os.path.relpath(os.path.join(directory, name), root)
            lang = language(path)
            full = os.path.join(root, path)
            if lang and os.path.isfile(full) and not os.path.islink(full):
                yield path, lang


def read(path):
    return Path(path).read_text(errors="replace")


def blank(s):
    """s with every character but a newline made a space, so lines and columns stay where they were."""
    return re.sub(r"[^\n]", " ", s)


def line_of(text, offset):
    """The 0-based line of an offset into text."""
    return text.count("\n", 0, offset)


# --- Lexing Go, TypeScript and Python -----------------------------------------------------------


class Token:
    """One token of a file: kind is ident, number, punct, string, template (TypeScript), regex
    (TypeScript) or comment; start and end are offsets into the file's text. For a string, value is
    the text between its quotes as written, and open and close its delimiters (a Python prefix
    belongs to open). A template's value is its text with each ${...} as "${}", its chunks the
    spans of that text, and its subs the tokens of each substitution."""

    __slots__ = ("kind", "start", "end", "value", "open", "close", "chunks", "subs")

    def __init__(self, kind, start, end, value, open_="", close="", chunks=(), subs=()):
        self.kind, self.start, self.end, self.value = kind, start, end, value
        self.open, self.close, self.chunks, self.subs = open_, close, chunks, subs

    def __repr__(self):
        return f"Token({self.kind}, {self.value!r}, {self.start})"


_GO = re.compile(r"""
    (?P<ws>\s+)
  | (?P<comment>//[^\n]*|/\*.*?(?:\*/|\Z))
  | (?P<string>"(?:[^"\\\n]|\\.)*"?|'(?:[^'\\\n]|\\.)*'?|`[^`]*`?)
  | (?P<ident>[^\W\d]\w*)
  | (?P<number>\d[\w.]*)
  | (?P<punct>\.\.\.|.)
""", re.S | re.X)

_TS = re.compile(r"""
    (?P<ws>\s+)
  | (?P<comment>//[^\n]*|/\*.*?(?:\*/|\Z))
  | (?P<string>"(?:[^"\\\n]|\\.)*"?|'(?:[^'\\\n]|\\.)*'?)
  | (?P<ident>[^\W\d][\w$]*|\$[\w$]*)
  | (?P<number>\d[\w.]*)
  | (?P<punct>\.\.\.|.)
""", re.S | re.X)

_PY = re.compile(r"""
    (?P<ws>\s+)
  | (?P<comment>\#[^\n]*)
  | (?P<string>(?P<prefix>[rRbBuUfF]{1,2})?(?:'''.*?(?:'''|\Z)|\"\"\".*?(?:\"\"\"|\Z)
                |'(?:[^'\\\n]|\\.)*'?|"(?:[^"\\\n]|\\.)*"?))
  | (?P<ident>[^\W\d]\w*)
  | (?P<number>\d[\w.]*)
  | (?P<punct>.)
""", re.S | re.X)

# TypeScript: a / opens a regular expression after one of these characters or words, else divides.
_REGEX_AFTER = set("(,=:[!&|?{};+-*%<>~^")
_REGEX_KEYWORDS = {"return", "typeof", "case", "do", "else", "in", "of", "void", "yield", "await", "delete", "new",
                   "throw"}


def _string(text, m, prefix=""):
    literal = m.group("string")[len(prefix):]
    delimiter = literal[:3] if literal[:3] in ('"""', "'''") else literal[:1]
    closed = len(literal) >= 2 * len(delimiter) and literal.endswith(delimiter)
    body = literal[len(delimiter):len(literal) - len(delimiter)] if closed else literal[len(delimiter):]
    return Token("string", m.start(), m.end(), body, prefix + delimiter, delimiter if closed else "")


def _regex_allowed(previous):
    if previous is None:
        return True
    if previous.kind == "punct":
        return previous.value[-1] in _REGEX_AFTER
    return previous.kind == "ident" and previous.value in _REGEX_KEYWORDS


def _ts_tokens(text, i, inside):
    """TypeScript tokens from i, to the } that closes a substitution when inside; (tokens, end)."""
    out, n, depth, previous = [], len(text), 0, None
    while i < n:
        c = text[i]
        if c == "`":
            token = _template(text, i)
            out.append(token)
            previous, i = token, token.end
            continue
        if c == "/" and not text.startswith(("//", "/*"), i) and _regex_allowed(previous):
            j, in_class = i + 1, False
            while j < n and text[j] != "\n":
                if text[j] == "\\":
                    j += 2
                    continue
                if text[j] == "[":
                    in_class = True
                elif text[j] == "]":
                    in_class = False
                elif text[j] == "/" and not in_class:
                    break
                j += 1
            if j < n and text[j] == "/":
                j += 1
                while j < n and text[j].isalpha():
                    j += 1
                token = Token("regex", i, j, text[i:j])
                out.append(token)
                previous, i = token, j
                continue
        if inside and c == "}" and depth == 0:
            return out, i
        m = _TS.match(text, i)
        kind = m.lastgroup
        if kind != "ws":
            token = _string(text, m) if kind == "string" else Token(kind, m.start(), m.end(), m.group())
            if kind == "punct":
                depth += {"{": 1, "}": -1}.get(token.value, 0)
            out.append(token)
            if kind != "comment":
                previous = token
        i = m.end()
    return out, n


def _template(text, i):
    """The template literal that opens at i: its chunks of text, and the tokens of each ${...}."""
    n, j, chunks, subs, value, start = len(text), i + 1, [], [], [], i + 1
    while j < n:
        c = text[j]
        if c == "\\":
            j += 2
            continue
        if c == "`":
            chunks.append((start, j))
            value.append(text[start:j])
            return Token("template", i, j + 1, "${}".join(value), "`", "`", tuple(chunks), tuple(subs))
        if text.startswith("${", j):
            chunks.append((start, j))
            value.append(text[start:j])
            tokens, end = _ts_tokens(text, j + 2, True)
            subs.append(tokens)
            j = start = min(end + 1, n)
            continue
        j += 1
    chunks.append((start, n))
    value.append(text[start:n])
    return Token("template", i, n, "${}".join(value), "`", "", tuple(chunks), tuple(subs))


def lex(text, lang):
    """The tokens of a Go, TypeScript or Python file's text, comments included."""
    if lang == "ts":
        return _ts_tokens(text, 0, False)[0]
    pattern, out, i, n = (_GO if lang == "go" else _PY), [], 0, len(text)
    while i < n:
        m = pattern.match(text, i)
        kind = m.lastgroup if m.lastgroup != "prefix" else "string"
        if kind == "string":
            out.append(_string(text, m, (m.group("prefix") or "") if lang == "py" else ""))
        elif kind != "ws":
            out.append(Token(kind, m.start(), m.end(), m.group()))
        i = m.end()
    return out


def _render(text, tokens, keep, out, position):
    for token in tokens:
        out.append(text[position:token.start])
        if token.kind in ("comment", "regex"):
            out.append(blank(text[token.start:token.end]))
        elif token.kind == "string":
            body_start = token.start + len(token.open)
            body_end = token.end - len(token.close)
            body = text[body_start:body_end]
            out.append(token.open + (body if keep(token.value) else blank(body)) + token.close)
        elif token.kind == "template":
            kept = keep(token.value)
            out.append(text[token.start:token.chunks[0][0]])
            for index, (s, e) in enumerate(token.chunks):
                out.append(text[s:e] if kept else blank(text[s:e]))
                if index < len(token.subs):
                    out.append(text[e:e + 2])
                    at = _render(text, token.subs[index], keep, out, e + 2)
                    following = token.chunks[index + 1][0]
                    closing = following - 1 if following - 1 >= at and text[following - 1:following] == "}" else following
                    out.append(text[at:closing])
                    out.append(text[closing:following])
            out.append(text[token.chunks[-1][1]:token.end])
        else:
            out.append(text[token.start:token.end])
        position = token.end
    return position


def code(text, lang, keep=lambda value: False, tokens=None):
    """The lines of a Go, TypeScript or Python file with its comments blanked and the text of each
    string literal blanked but for one whose text keep(text) holds, which stays whole. A template's
    ${...} stays code, and a TypeScript regular expression is blanked, so a quote or a backtick
    inside one opens no string. Lines and columns stay where they were in text."""
    out = []
    position = _render(text, lex(text, lang) if tokens is None else tokens, keep, out, 0)
    out.append(text[position:])
    return "".join(out).split("\n")


# --- Reading shell -------------------------------------------------------------------------------

HEREDOC = re.compile(r"(?<!<)<<(?!<)(-?)\s*(['\"]?)([A-Za-z_][\w]*)\2")


def shell_code(text):
    """Shell with the text of quoted strings, comments and heredoc bodies blanked. A $(...)
    inside double quotes is code, and a double-quoted path ending /go becomes the word go."""
    lines, out, stack, heredocs = text.split("\n"), [], [], []
    for line in lines:
        if heredocs:
            strip, word = heredocs[0]
            if (line.strip() if strip else line) == word:
                heredocs.pop(0)
            out.append(blank(line))
            continue
        code_, i, n, starts = [], 0, len(line), []
        while i < n:
            c = line[i]
            state = stack[-1] if stack else "code"
            if state in ("sq", "ansi"):
                if state == "ansi" and c == "\\":
                    code_.extend("  "[:n - i])
                    i += 2
                    continue
                if c == "'":
                    stack.pop()
                code_.append(" ")
                i += 1
                continue
            if state == "dq":
                if c == "\\":
                    code_.extend("  "[:n - i])
                    i += 2
                    continue
                if c == '"':
                    stack.pop()
                    if starts:
                        start, raw_start = starts.pop()
                        if re.fullmatch(r'"[^"\s]*/go', line[raw_start:i]):
                            code_[start:] = list("go" + " " * (len(code_) - start - 2))
                    code_.append(" ")
                    i += 1
                    continue
                if line.startswith("$(", i):
                    stack.append("sub")
                    code_.extend("$(")
                    i += 2
                    continue
                code_.append(" ")
                i += 1
                continue
            # code, or a $(...) inside double quotes
            if c == "\\":
                code_.extend(line[i:i + 2])
                i += 2
                continue
            if line.startswith("$'", i):
                stack.append("ansi")
                code_.extend("  ")
                i += 2
                continue
            if c == "'":
                stack.append("sq")
                code_.append(" ")
                i += 1
                continue
            if c == '"':
                stack.append("dq")
                starts.append((len(code_), i))
                code_.append(" ")
                i += 1
                continue
            if c == "#" and (i == 0 or line[i - 1] in " \t;&|("):
                code_.extend(" " * (n - i))
                break
            if c == ")" and state == "sub":
                stack.pop()
            if line.startswith("$(", i):
                stack.append("sub")
                code_.extend("$(")
                i += 2
                continue
            code_.append(c)
            i += 1
        rendered = "".join(code_)
        out.append(rendered)
        for doc in HEREDOC.finditer(line):
            if doc.start() < len(rendered) and rendered[doc.start()] == "<":
                heredocs.append((doc.group(1) == "-", doc.group(3)))
    return out


SHELL_SEPARATORS = frozenset({";", "&&", "||", "|", "&", "(", ")", "$(", "`"})


def shell_words(line):
    """The words of a shell line, its operators ; & | ( ) as words of their own and its comment
    dropped; a line shlex cannot read is split on whitespace."""
    try:
        lexer = shlex.shlex(line, posix=True, punctuation_chars=";&|()")
        lexer.whitespace_split = True
        lexer.commenters = "#"
        return list(lexer)
    except ValueError:
        return line.split()


# --- Reading statements of Go and TypeScript: calls, their arguments, and what a name is bound to ---

# A value environment readers give back, one element per setting in the order the program makes
# them: ("set", NAME, VALUE) where VALUE is None when it is not a literal, ("inherit",) for the
# environment the process was given, ("unknown",) for anything else that may hold any variable, and
# ("clear",) for an environment emptied there. The last element that may set a variable decides it.
INHERIT, UNKNOWN, CLEAR = ("inherit",), ("unknown",), ("clear",)

_KEYWORDS = {
    "go": {"if", "for", "switch", "func", "return", "go", "defer", "select", "range", "else", "case", "var", "const",
           "type", "import", "package", "chan", "map", "struct", "interface"},
    "ts": {"if", "for", "while", "switch", "catch", "function", "return", "typeof", "do", "else", "in", "of", "case",
           "void", "yield", "delete", "throw", "const", "let", "var", "new", "await", "export", "import", "class"},
}
# A line ending in one of these does not continue the expression on the next line, unless that
# line starts with an operator.
_CONTINUES = set(".?:+-*/%|&=<>,!^~")


class Call:
    """A call in a Source: its callee as written (`exec.Command`, `Bun.spawn`, `$` for a tagged
    template), the token index it starts at, its parentheses, and the token range of each argument."""

    __slots__ = ("callee", "at", "open", "close", "args", "tagged")

    def __init__(self, callee, at, open_, close, args, tagged=False):
        self.callee, self.at, self.open, self.close, self.args, self.tagged = callee, at, open_, close, args, tagged

    @property
    def name(self):
        return self.callee.rsplit(".", 1)[-1]


class Function:
    """A function in a Source: the name it is declared or bound with (None for a literal no name is
    bound to), the token it starts at (`func`, `function`, `async`, its name or its parameters), the
    parenthesis that opens its parameters, the token range of each parameter's name (None for one
    that is no plain name), and its body as a token range: inside its braces, or a TypeScript
    arrow's expression."""

    __slots__ = ("name", "at", "open", "params", "body")

    def __init__(self, name, at, open_, params, body):
        self.name, self.at, self.open, self.params, self.body = name, at, open_, params, body

    def __repr__(self):
        return f"Function({self.name}, {self.at})"


# TypeScript: a word before a method's name in a class or an object literal.
_TS_MODIFIERS = {"async", "static", "public", "private", "protected", "readonly", "override", "get", "set", "abstract"}


class Source:
    """A Go or TypeScript file read as tokens, its comments left out: each bracket's partner, each
    token's line, its calls and their arguments across lines, its assignments, and the value the
    nearest assignment before a place binds a name to. A range is (first, end) token indices."""

    def __init__(self, text, lang, tokens=None):
        self.text, self.lang = text, lang
        self.tokens = [t for t in (lex(text, lang) if tokens is None else tokens) if t.kind != "comment"]
        self._starts = [0] + [m.end() for m in re.finditer("\n", text)]
        self.partner, stack = {}, []
        for i, t in enumerate(self.tokens):
            if t.kind != "punct":
                continue
            if t.value in ("(", "[", "{"):
                stack.append(i)
            elif t.value in (")", "]", "}"):
                want = {")": "(", "]": "[", "}": "{"}[t.value]
                k = len(stack) - 1
                while k >= 0 and self.tokens[stack[k]].value != want:
                    k -= 1
                if k >= 0:
                    self.partner[stack[k]], self.partner[i] = i, stack[k]
                    del stack[k:]
        self._calls = self._call_spans = self._assignments = self._bindings = self._regions = None
        self._functions = self._function_at = None

    # Positions.
    def line(self, i):
        return bisect.bisect_right(self._starts, self.tokens[i].start) - 1

    def end_line(self, i):
        return bisect.bisect_right(self._starts, self.tokens[i].end - 1) - 1

    def punct(self, i, value):
        return 0 <= i < len(self.tokens) and self.tokens[i].kind == "punct" and self.tokens[i].value == value

    def _ends(self, i):
        """Whether a line ending at token i ends the statement there (Go's semicolon rule; for
        TypeScript, a token that can end an expression)."""
        t = self.tokens[i]
        if t.kind == "ident":
            return t.value not in _KEYWORDS[self.lang] or t.value in ("return", "break", "continue")
        if t.kind in ("number", "string", "template", "regex"):
            return True
        return t.kind == "punct" and (t.value in (")", "]", "}") or (t.value in "+-" and self.punct(i - 1, t.value)))

    def _breaks(self, i):
        """Whether the statement ends between token i and the next."""
        n = len(self.tokens)
        if i + 1 >= n or self.line(i + 1) <= self.end_line(i) or not self._ends(i):
            return False
        nxt = self.tokens[i + 1]
        if self.lang == "ts" and nxt.kind == "punct" and nxt.value in _CONTINUES:
            return False
        return True

    def split(self, a, b, sep=","):
        """The ranges between the separators at the top level of [a, b)."""
        out, start, i = [], a, a
        while i < b:
            t = self.tokens[i]
            if t.kind == "punct" and t.value in ("(", "[", "{") and i in self.partner:
                i = self.partner[i] + 1
                continue
            if t.kind == "punct" and t.value == sep:
                out.append((start, i))
                start = i + 1
            i += 1
        if start < b:
            out.append((start, b))
        return out

    def expression_end(self, a, limit=None, brace_ends=False):
        """The end of the expression that starts at a: at a ; or a closing bracket of the level it
        is in, at a line break that ends the statement, or, when brace_ends, at a { (an if, for or
        switch header's block)."""
        n = len(self.tokens) if limit is None else limit
        i = a
        while i < n:
            t = self.tokens[i]
            if t.kind == "punct":
                if t.value in (";", ")", "]", "}"):
                    return i
                if t.value == "{" and brace_ends:
                    return i
                if t.value in ("(", "[", "{") and i in self.partner:
                    i = self.partner[i]
            if self._breaks(i):
                return i + 1
            i += 1
        return n

    # Values.
    def string(self, a, b):
        """The text of a range that is one string literal (or a template with no substitution)."""
        if b - a == 1:
            t = self.tokens[a]
            if t.kind == "string" or (t.kind == "template" and not t.subs):
                return t.value
        return None

    def name(self, a, b):
        """The dotted name a range is (`cmd`, `r.omp`, `process.env.X`), a TypeScript `!` after it
        dropped, else None."""
        if b > a and self.lang == "ts" and self.punct(b - 1, "!"):
            b -= 1
        if b <= a or (b - a) % 2 == 0:
            return None
        for k in range(a, b):
            t = self.tokens[k]
            if (k - a) % 2 == 0 and t.kind != "ident":
                return None
            if (k - a) % 2 == 1 and not (t.kind == "punct" and t.value == "."):
                return None
        return "".join(self.tokens[k].value for k in range(a, b))

    def text_of_tokens(self, a, b):
        return self.text[self.tokens[a].start:self.tokens[b - 1].end] if b > a else ""

    def calls(self):
        """Every call, in order: a name or a dotted name followed by its parenthesized arguments,
        and in TypeScript a template tagged by `$` or `Bun.$`."""
        if self._calls is not None:
            return self._calls
        out, tokens, keywords = [], self.tokens, _KEYWORDS[self.lang]
        for i, t in enumerate(tokens):
            if i == 0:
                continue
            prev = tokens[i - 1]
            if t.kind == "punct" and t.value == "(" and i in self.partner and prev.kind == "ident" \
                    and prev.value not in keywords:
                a = i - 1
                while a >= 2 and self.punct(a - 1, ".") and tokens[a - 2].kind == "ident":
                    a -= 2
                if a > 0 and tokens[a - 1].kind == "ident" and tokens[a - 1].value in ("func", "function"):
                    continue
                close = self.partner[i]
                out.append(Call("".join(tokens[k].value for k in range(a, i)), a, i, close, self.split(i + 1, close)))
            elif t.kind == "template" and prev.kind == "ident" and prev.value == "$":
                a = i - 1
                if a >= 2 and self.punct(a - 1, ".") and tokens[a - 2].value == "Bun":
                    a -= 2
                out.append(Call("".join(tokens[k].value for k in range(a, i)), a, i, i, [(i, i + 1)], tagged=True))
        self._calls = out
        return out

    def call_of(self, a, b):
        """The call a range is exactly, else None."""
        if self._call_spans is None:
            self._call_spans = {(call.at, call.close + 1): call for call in self.calls()}
        return self._call_spans.get((a, b))

    # Assignments and bindings.
    def _statement_start(self, k):
        i = k
        while i > 0:
            prev = self.tokens[i - 1]
            if self._breaks(i - 1):
                return i, None
            if prev.kind == "punct" and prev.value in (")", "]") and (i - 1) in self.partner:
                i = self.partner[i - 1]
                continue
            if prev.kind == "punct" and prev.value in (";", "{", "}", "(", "["):
                return i, None
            if prev.kind == "ident" and prev.value in _KEYWORDS[self.lang]:
                return i, prev.value
            i -= 1
        return 0, None

    def assignments(self):
        """(lhs ranges, rhs ranges, operator index) of each `=` and `:=` assignment and declaration."""
        if self._assignments is not None:
            return self._assignments
        out, tokens = [], self.tokens
        self._assignments = out
        for k, t in enumerate(tokens):
            if not (t.kind == "punct" and t.value == "="):
                continue
            nxt = tokens[k + 1] if k + 1 < len(tokens) else None
            if nxt is not None and nxt.kind == "punct" and nxt.value in ("=", ">") and nxt.start == t.end:
                continue
            prev = tokens[k - 1] if k else None
            end_lhs = k
            if prev is not None and prev.kind == "punct" and prev.end == t.start:
                if prev.value == ":" and self.lang == "go":
                    end_lhs = k - 1
                elif prev.value in "!<>=+-*/%&|^?":
                    continue
            start, keyword = self._statement_start(end_lhs)
            lhs = self.split(start, end_lhs)
            if self.lang == "ts":  # `const x: T = ...`: the name alone
                lhs = [(a, next((j for j in range(a, b) if self.punct(j, ":")), b)) for a, b in lhs]
            elif keyword in ("var", "const"):  # `var x T = ...`: the name alone
                lhs = [(a, a + 1) for a, b in lhs]
            end = self.expression_end(k + 1, brace_ends=keyword in ("if", "for", "switch"))
            out.append((lhs, self.split(k + 1, end), k))
        return out

    def regions(self):
        """Go: the token ranges of the top-level declarations, each from its keyword at column 0 to
        the next; TypeScript: the whole file."""
        if self._regions is None:
            starts = [0]
            if self.lang == "go":
                starts += [i for i, t in enumerate(self.tokens) if i and t.kind == "ident" and t.value in
                           ("func", "var", "const", "type", "import", "package")
                           and t.start == self._starts[self.line(i)]]
            self._regions = (starts, list(zip(starts, starts[1:] + [len(self.tokens)])))
        return self._regions[1]

    def region(self, i):
        self.regions()
        return self._regions[1][max(0, bisect.bisect_right(self._regions[0], i) - 1)]

    def bindings(self):
        """name -> [(operator index, rhs range)] of every assignment to a plain name, in order."""
        if self._bindings is None:
            self._bindings = {}
            for lhs, rhs, k in self.assignments():
                names = [self.name(a, b) for a, b in lhs]
                for index, name in enumerate(names):
                    if not name:
                        continue
                    if len(rhs) == len(lhs):
                        value = rhs[index]
                    elif index == 0 and len(rhs) == 1:  # x, err := f()
                        value = rhs[0]
                    else:
                        continue
                    self._bindings.setdefault(name, []).append((k, value))
        return self._bindings

    def bound_all(self, name, before):
        """The rhs ranges of every assignment of name before token `before` in the same declaration
        (Go) or file (TypeScript), nearest last; else those of the package or the file itself."""
        here = self.region(before)
        found = [r for k, r in self.bindings().get(name, ()) if k < before and here[0] <= k < here[1]]
        if found:
            return found
        return [r for k, r in self.bindings().get(name, ())
                if self.lang == "ts" or self.tokens[self.region(k)[0]].value in ("var", "const")]

    def bound(self, name, before):
        """The rhs range of the nearest assignment of name before token `before` (bound_all's)."""
        found = self.bound_all(name, before)
        return found[-1] if found else None

    def template_text(self, token, substitute):
        """A template's text with each ${...} replaced by substitute(its tokens)."""
        out = []
        for index, (s, e) in enumerate(token.chunks):
            out.append(self.text[s:e])
            if index < len(token.subs):
                out.append(substitute(token.subs[index]))
        return "".join(out)

    # Environments.
    def env_elements(self, a, b, depth=0, known=None):
        """The environment elements of an expression that builds one: in Go a []string (append of
        os.Environ() and "K=V" strings, a []string{...}, a name bound to either), in TypeScript an
        object ({ ...process.env, K: "v" }, a name bound to one). known maps a name to the
        elements it stands for (a Cmd's own Env, read before a later assignment appends to it)."""
        if b <= a:
            return [UNKNOWN]
        tokens = self.tokens
        name = self.name(a, b)
        if name:
            if known and name in known:
                return list(known[name])
            bound = self.bound(name, a) if depth < 4 else None
            if bound:
                return self.env_elements(*bound, depth + 1, known)
            return [UNKNOWN]
        if self.lang == "ts":
            if self.punct(a, "{") and self.partner.get(a) == b - 1:
                out = []
                for pa, pb in self.split(a + 1, b - 1):
                    if self.punct(pa, "..."):
                        out.append(UNKNOWN)
                        continue
                    colon = next((j for j in range(pa, pb) if self.punct(j, ":")), None)
                    key_token = tokens[pa]
                    key = key_token.value if key_token.kind in ("ident", "string") and colon == pa + 1 else None
                    if colon is None and pb - pa == 1 and key_token.kind == "ident":
                        out.append(("set", key_token.value, None))
                    elif key is None:
                        out.append(UNKNOWN)
                    else:
                        out.append(("set", key, self.string(colon + 1, pb)))
                return out
            return [UNKNOWN]
        # Go.
        call = self.call_of(a, b)
        if call and call.callee == "os.Environ":
            return [INHERIT]
        if call and call.callee == "append" and call.args:
            out = list(self.env_elements(*call.args[0], depth, known))
            for ea, eb in call.args[1:]:
                if self.punct(eb - 1, "..."):
                    out += self.env_elements(ea, eb - 1, depth, known)
                else:
                    out.append(self._go_setting(ea, eb))
            return out
        if self.punct(a, "[") and self.punct(a + 1, "]"):
            brace = next((j for j in range(a, b) if self.punct(j, "{")), None)
            if brace is not None and self.partner.get(brace) == b - 1:
                return [self._go_setting(ea, eb) for ea, eb in self.split(brace + 1, b - 1)]
        if tokens[a].kind == "ident" and self.punct(a + 1, "[") and self.partner.get(a + 1) == b - 1:
            return self.env_elements(a, a + 1, depth, known)  # env[:len(env):len(env)]
        return [UNKNOWN]

    def _go_setting(self, a, b):
        token = self.tokens[a]
        if token.kind == "string" and "=" in token.value:
            key, value = token.value.split("=", 1)
            return ("set", key, value if b - a == 1 else None)
        return UNKNOWN

    def go_cmd_envs(self, call):
        """The *exec.Cmd that a call of exec.Command makes, and how its environment is set: the
        variable it is assigned to, and the rhs range of each `X.Env = ...` of that variable after
        it in the same declaration, in order, before the variable is assigned again; (None, []) when
        the call is assigned to no variable."""
        target = None
        for lhs, rhs, k in self.assignments():
            for index, (ra, rb) in enumerate(rhs):
                if ra == call.at and rb == call.close + 1 and index < len(lhs):
                    target = self.name(*lhs[index])
            if target:
                break
        if not target:
            return None, []
        low, high = self.region(call.at)
        sets = []
        for lhs, rhs, k in self.assignments():
            if k <= call.close or not low <= k < high:
                continue
            names = [self.name(a, b) for a, b in lhs]
            if target in names:
                break
            sets += [rhs[index] for index, name in enumerate(names) if name == f"{target}.Env" and index < len(rhs)]
        return target, sets

    def go_cmd_env(self, call):
        """The environment elements of the *exec.Cmd that a call of exec.Command makes: the last
        `X.Env = ...` go_cmd_envs finds. None when no Env is set (the command inherits the
        environment)."""
        target, sets = self.go_cmd_envs(call)
        env = None
        for a, b in sets:
            # A nil Env is the process's own; `X.Env = append(X.Env, ...)` extends the last.
            env = self.env_elements(a, b, known={f"{target}.Env": env if env is not None else [INHERIT]})
        return env

    def ts_options_env(self, call, first):
        """The environment elements of a TypeScript call's options: the `env` of the first object
        argument from index `first` (or of the object a name there is bound to), else None."""
        for a, b in call.args[first:]:
            name = self.name(a, b)
            if name:
                bound = self.bound(name, a)
                if bound:
                    a, b = bound
            if not (self.punct(a, "{") and self.partner.get(a) == b - 1):
                continue
            for pa, pb in self.split(a + 1, b - 1):
                key = self.tokens[pa]
                if key.value != "env" or key.kind not in ("ident", "string"):
                    continue
                if pb - pa == 1:  # { env }
                    bound = self.bound("env", pa)
                    return self.env_elements(*bound) if bound else [UNKNOWN]
                if self.punct(pa + 1, ":"):
                    return self.env_elements(pa + 2, pb)
        return None

    # Functions, and where a name gets its value.
    def functions(self):
        """Every function: in Go each `func` declaration, method and literal; in TypeScript each
        `function`, arrow and method of a class or an object literal. A literal's name is the one an
        assignment or an object key binds it to."""
        if self._functions is not None:
            return self._functions
        out = self._go_functions() if self.lang == "go" else self._ts_functions()
        names = {}
        for lhs, rhs, _ in self.assignments():
            if len(lhs) == len(rhs):
                for (la, lb), (ra, _rb) in zip(lhs, rhs):
                    name = self.name(la, lb)
                    if name and "." not in name:
                        names.setdefault(ra, name)
        for fn in out:
            if fn.name is None:
                fn.name = names.get(fn.at)
            if fn.name is None and fn.at >= 2 and self.punct(fn.at - 1, ":") and self.tokens[fn.at - 2].kind in (
                    "ident", "string") and (fn.at < 3 or self.tokens[fn.at - 3].value in ("{", ",")):
                fn.name = self.tokens[fn.at - 2].value  # { key: (...) => ... }
        self._functions = out
        return out

    def _params(self, open_, close):
        """The name of each parameter between a function's parentheses, a TypeScript type's own
        commas (`Record<string, string>`) left inside it."""
        out, parts = [], self.split(open_ + 1, close)
        if self.lang == "ts":
            merged, angle = [], 0
            for a, b in parts:
                if angle > 0:
                    merged[-1] = (merged[-1][0], b)
                else:
                    merged.append((a, b))
                for k in range(a, b):
                    if self.punct(k, "<"):
                        angle += 1
                    elif self.punct(k, ">") and not self.punct(k - 1, "="):
                        angle -= 1
            parts = merged
        for a, b in parts:
            while a < b and (self.punct(a, "...") or self.tokens[a].value in _TS_MODIFIERS - {"async", "get", "set"}):
                a += 1
            out.append((a, a + 1) if a < b and self.tokens[a].kind == "ident" else None)
        return out

    def _go_functions(self):
        tokens, n, out = self.tokens, len(self.tokens), []
        for i, t in enumerate(tokens):
            if t.kind != "ident" or t.value != "func":
                continue
            j, name = i + 1, None
            if j < n and tokens[j].kind == "ident":
                name, j = tokens[j].value, j + 1
            elif self.punct(j, "(") and j in self.partner:
                after = self.partner[j] + 1
                if after + 1 < n and tokens[after].kind == "ident" and tokens[after].value != "func" \
                        and (self.punct(after + 1, "(") or self.punct(after + 1, "[")):
                    name, j = tokens[after].value, after + 1  # a method: func (r *T) Name(
            if self.punct(j, "[") and j in self.partner:  # type parameters
                j = self.partner[j] + 1
            if not (self.punct(j, "(") and j in self.partner):
                continue
            close = self.partner[j]
            k = close + 1
            while k < n:  # the result types, then the body's brace; a func type has none
                tk = tokens[k]
                if tk.kind == "punct" and tk.value in ("(", "[") and k in self.partner:
                    k = self.partner[k] + 1
                    continue
                if tk.kind == "punct" and tk.value == "{" and k in self.partner:
                    if tokens[k - 1].kind == "ident" and tokens[k - 1].value in ("struct", "interface"):
                        k = self.partner[k] + 1
                        continue
                    out.append(Function(name, i, j, self._params(j, close), (k + 1, self.partner[k])))
                    break
                if (tk.kind == "punct" and tk.value in (";", ")", "]", "}", ",", "=")) or self._breaks(k):
                    break
                k += 1
        return out

    def _ts_block(self, k):
        """The body (inside its braces) of a TypeScript function whose parameters close before k:
        an optional `: Type`, then `{`; else None."""
        n, tokens = len(self.tokens), self.tokens
        if self.punct(k, ":"):
            k, angle, typed = k + 1, 0, False
            while k < n:
                tk = tokens[k]
                if tk.kind == "punct":
                    if tk.value in ("(", "[") and k in self.partner:
                        k, typed = self.partner[k] + 1, True
                        continue
                    if tk.value == "{" and k in self.partner and (angle > 0 or not typed):
                        k, typed = self.partner[k] + 1, True  # an object type
                        continue
                    if tk.value == "<":
                        angle += 1
                    elif tk.value == ">" and not self.punct(k - 1, "="):
                        angle -= 1
                    elif tk.value == "{" and angle <= 0:
                        break
                    elif tk.value in (";", ",", ")", "}", "=") and angle <= 0 and not (
                            tk.value == "=" and self.punct(k + 1, ">")):
                        return None
                typed = True
                k += 1
        if self.punct(k, "{") and k in self.partner:
            return (k + 1, self.partner[k])
        return None

    def _ts_functions(self):
        tokens, n, out = self.tokens, len(self.tokens), []
        keywords = _KEYWORDS["ts"]
        for i, t in enumerate(tokens):
            start = i - 1 if i and tokens[i - 1].value == "async" and tokens[i - 1].kind == "ident" else i
            if t.kind == "ident" and t.value == "function":
                j, name = i + 1, None
                if self.punct(j, "*"):
                    j += 1
                if j < n and tokens[j].kind == "ident":
                    name, j = tokens[j].value, j + 1
                if self.punct(j, "<"):  # type parameters
                    while j < n and not self.punct(j, "("):
                        j += 1
                if self.punct(j, "(") and j in self.partner:
                    body = self._ts_block(self.partner[j] + 1)
                    if body:
                        out.append(Function(name, start, j, self._params(j, self.partner[j]), body))
            elif t.kind == "punct" and t.value == "=" and self.punct(i + 1, ">") and tokens[i + 1].start == t.end:
                k, open_, params = i - 1, None, []
                if self.punct(k, ")") and k in self.partner:
                    open_ = self.partner[k]
                elif k >= 0 and tokens[k].kind == "ident" and not self.punct(k - 1, ":"):
                    open_, params = k, [(k, k + 1)]  # x => ...
                else:  # (x): T => ...
                    m = k
                    while m > 0 and i - m < 40 and not (self.punct(m, ")") and self.punct(m + 1, ":")):
                        if self.punct(m, ";") or self.punct(m, "{") or self.punct(m, "}"):
                            break
                        m -= 1
                    if self.punct(m, ")") and self.punct(m + 1, ":") and m in self.partner:
                        k = m
                        open_ = self.partner[m]
                if open_ is None:
                    continue
                if not params:
                    params = self._params(open_, k)
                at = open_ - 1 if open_ and tokens[open_ - 1].value == "async" and tokens[open_ - 1].kind == "ident" \
                    else open_
                after = i + 2
                if self.punct(after, "{") and after in self.partner:
                    body = (after + 1, self.partner[after])
                else:
                    body = (after, self.expression_end(after))
                out.append(Function(None, at, open_, params, body))
            elif t.kind == "ident" and t.value not in keywords and self.punct(i + 1, "(") and i + 1 in self.partner:
                before = tokens[i - 1] if i else None
                if before is not None and not (before.kind == "punct" and before.value in ("{", "}", ";", ",")
                                               or before.kind == "ident" and before.value in _TS_MODIFIERS
                                               or before.kind == "punct" and before.value == "*"):
                    continue
                close = self.partner[i + 1]
                body = self._ts_block(close + 1)
                if body:  # a method: name(params) { ... }
                    at = i - 1 if before is not None and before.kind == "ident" and before.value in _TS_MODIFIERS else i
                    out.append(Function(t.value, at, i + 1, self._params(i + 1, close), body))
        return out

    def function_at(self, i):
        """The innermost function whose body holds token i, else None."""
        if self._function_at is None:
            held = [None] * len(self.tokens)
            for fn in sorted(self.functions(), key=lambda f: f.body[0] - f.body[1]):
                for k in range(fn.body[0], min(fn.body[1], len(held))):
                    held[k] = fn
            self._function_at = held
        return self._function_at[i] if 0 <= i < len(self._function_at) else None

    def resolve(self, name, at):
        """Where a plain name used at token `at` gets its value, scope by scope out from there: in
        the function that holds `at`, every assignment of it made in that function's own body (not a
        function inside it) before `at`, ("bound", [rhs ranges]); else, when that function takes it
        as a parameter, ("param", (function, index)); else the same of each function around it; else
        the assignments of it outside every function, before `at` or, failing that, anywhere in the
        file (a package's var or const, a module's binding); else None."""
        bindings = self.bindings().get(name, ())
        fn, limit = self.function_at(at), at
        while fn is not None:
            found = [rhs for k, rhs in bindings if fn.body[0] <= k and rhs[1] <= limit and self.function_at(k) is fn]
            if found:
                return "bound", found
            for index, param in enumerate(fn.params):
                if param and self.tokens[param[0]].value == name:
                    return "param", (fn, index)
            fn, limit = self.function_at(fn.at), fn.at
        outside = [(k, rhs) for k, rhs in bindings if self.function_at(k) is None]
        found = [rhs for k, rhs in outside if rhs[1] <= limit] or [rhs for k, rhs in outside]
        return ("bound", found) if found else None

    def call_sites(self, fn):
        """The calls of a function in this file by its name, its own definition left out."""
        if not fn.name:
            return []
        return [call for call in self.calls() if call.name == fn.name and call.open != fn.open and not call.tagged]

    def chain_end(self, call):
        """The token after a call and each `.name(...)` or `.name` chained to it."""
        k = call.close + 1
        while self.punct(k, ".") and k + 1 < len(self.tokens) and self.tokens[k + 1].kind == "ident":
            k += 2
            if self.punct(k, "(") and k in self.partner:
                k = self.partner[k] + 1
        return k

    def split_op(self, a, b, op):
        """The ranges between each top-level two-character operator op (`??`, `||`, `&&`) in [a, b)."""
        out, start, i = [], a, a
        while i < b:
            t = self.tokens[i]
            if t.kind == "punct" and t.value in ("(", "[", "{") and i in self.partner:
                i = self.partner[i] + 1
                continue
            if i + 1 < b and self.punct(i, op[0]) and self.punct(i + 1, op[1]) and t.end == self.tokens[i + 1].start:
                out.append((start, i))
                start = i + 2
                i += 2
                continue
            i += 1
        out.append((start, b))
        return out

    def array_elements(self, a, b, depth=0):
        """The element ranges of an array literal, or of the one a name is bound to, else None."""
        if b - a > 1 and self.lang == "ts" and self.punct(b - 1, "!"):
            b -= 1
        if self.punct(a, "[") and self.partner.get(a) == b - 1:
            return self.split(a + 1, b - 1)
        name = self.name(a, b)
        if name and depth < 4:
            bound = self.bound(name, a)
            if bound:
                return self.array_elements(*bound, depth + 1)
        return None

    def ts_spawn_argv(self, call):
        """The argv of a TypeScript call that starts a program, and the index of the argument its
        options are in: Bun.spawn's argv array (or `{ cmd: [...] }`, whose options are that object),
        else node's command and the args array after it. (None, 1) for a call with no arguments."""
        if not call.args:
            return None, 1
        argv, options = self.array_elements(*call.args[0]), 1
        if argv is None and self.punct(call.args[0][0], "{"):
            a, b = call.args[0]
            for pa, pb in self.split(a + 1, b - 1):
                if self.tokens[pa].value == "cmd" and self.punct(pa + 1, ":"):
                    argv, options = self.array_elements(pa + 2, pb), 0
        if argv is None:
            rest = self.array_elements(*call.args[1]) if len(call.args) > 1 else None
            argv, options = [call.args[0]] + (rest or []), 2 if rest is not None else 1
        return argv, options


# --- Reading Python statements --------------------------------------------------------------------


def python_tree(text):
    """The file's syntax tree, each node with .parent, or None when Python cannot parse it."""
    try:
        tree = ast.parse(text)
    except SyntaxError:
        return None
    for node in ast.walk(tree):
        for child in ast.iter_child_nodes(node):
            child.parent = node
    return tree


def python_scope(node):
    """The function or module that holds a node."""
    while hasattr(node, "parent") and not isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef, ast.Module)):
        node = node.parent
    return node


def python_bound(name, node):
    """The value of the nearest plain assignment of name before node in its function or module."""
    best = None
    for stmt in ast.walk(python_scope(node)):
        if isinstance(stmt, ast.Assign) and stmt.lineno <= node.lineno and stmt is not node:
            for target in stmt.targets:
                if isinstance(target, ast.Name) and target.id == name and (best is None or stmt.lineno >= best.lineno):
                    best = stmt
    return best.value if best is not None else None


def python_text(node, substitute=lambda value: "\x00"):
    """The text of a string expression: a constant, an f-string with each {...} replaced by
    substitute(its expression), or a + of those; None for anything else."""
    if isinstance(node, ast.Constant) and isinstance(node.value, str):
        return node.value
    if isinstance(node, ast.JoinedStr):
        return "".join(v.value if isinstance(v, ast.Constant) else substitute(v.value) for v in node.values)
    if isinstance(node, ast.BinOp) and isinstance(node.op, ast.Add):
        left, right = python_text(node.left, substitute), python_text(node.right, substitute)
        if left is not None or right is not None:
            return (left if left is not None else "\x00") + (right if right is not None else "\x00")
    return None


def python_env_elements(node, depth=0):
    """The environment elements of an expression that builds one: a dict display (a ** spread is
    unknown), dict(...) or os.environ.copy() with keywords, os.environ, a | of those, or a name bound
    to one, with the `X["K"] = v` assignments and X.update(K=v) calls made to that name after it."""
    if isinstance(node, ast.Dict):
        out = []
        for key, value in zip(node.keys, node.values):
            if key is None:
                out += python_env_elements(value, depth + 1) if isinstance(value, ast.Dict) else [UNKNOWN]
            elif isinstance(key, ast.Constant) and isinstance(key.value, str):
                out.append(("set", key.value, python_text(value)))
            else:
                out.append(UNKNOWN)
        return out
    if isinstance(node, ast.Call):
        out = [UNKNOWN] if node.args or not isinstance(node.func, ast.Name) else []
        return out + [("set", k.arg, python_text(k.value)) if k.arg else UNKNOWN for k in node.keywords]
    if isinstance(node, ast.BinOp) and isinstance(node.op, ast.BitOr):
        return python_env_elements(node.left, depth + 1) + python_env_elements(node.right, depth + 1)
    if isinstance(node, ast.Attribute) and node.attr == "environ":
        return [INHERIT]
    if isinstance(node, ast.Name) and depth < 4:
        bound = python_bound(node.id, node)
        if bound is None:
            return [UNKNOWN]
        out = python_env_elements(bound, depth + 1)
        for stmt in ast.walk(python_scope(node)):
            if not (bound.lineno < getattr(stmt, "lineno", 0) < node.lineno):
                continue
            if isinstance(stmt, ast.Assign) and any(isinstance(t, ast.Subscript) and isinstance(t.value, ast.Name)
                                                    and t.value.id == node.id for t in stmt.targets):
                target = next(t for t in stmt.targets if isinstance(t, ast.Subscript))
                key = target.slice
                out.append(("set", key.value, python_text(stmt.value))
                           if isinstance(key, ast.Constant) and isinstance(key.value, str) else UNKNOWN)
            elif isinstance(stmt, ast.Call) and isinstance(stmt.func, ast.Attribute) and stmt.func.attr == "update" \
                    and isinstance(stmt.func.value, ast.Name) and stmt.func.value.id == node.id:
                for arg in stmt.args:
                    out += python_env_elements(arg, depth + 1)
                out += [("set", k.arg, python_text(k.value)) if k.arg else UNKNOWN for k in stmt.keywords]
        return out
    return [UNKNOWN]


# --- Reading shell statements --------------------------------------------------------------------


class ShWord:
    """A shell word as bash reads it: its parts in order, each (kind, text, nested), where kind is
    lit (unquoted text, a backslash's character included), sq (single-quoted text), dq (double-quoted
    text), param ($NAME or ${...}: text is the name), cmd ($(...), `...` or <(...): nested is the
    list of its commands) or arith."""

    __slots__ = ("parts", "start", "line")

    def __init__(self, parts, start, line):
        self.parts, self.start, self.line = parts, start, line

    def value(self, placeholder="\x00"):
        """The word after quote removal, each expansion as placeholder."""
        return "".join(p[1] if p[0] in ("lit", "sq", "dq") else placeholder for p in self.parts)

    @property
    def static(self):
        return all(p[0] in ("lit", "sq", "dq") for p in self.parts)

    @property
    def quoted(self):
        return any(p[0] in ("sq", "dq") for p in self.parts)

    def source(self):
        """The word with its quotes removed and its expansions written back ($NAME, $(...))."""
        out = []
        for kind, text, _ in self.parts:
            if kind in ("lit", "sq", "dq"):
                out.append(text)
            elif kind == "param":
                out.append("$" + text if re.fullmatch(r"\w+", text) else "${" + text + "}")
            else:
                out.append("$(...)")
        return "".join(out)

    def __repr__(self):
        return f"ShWord({self.source()!r})"


class ShCommand:
    """A simple command: its NAME=value assignments, its words, its heredocs (each [strip, delimiter,
    quoted, body, first line of the body]), the 0-based line it starts on, the frames that hold it
    (an id per function body and per subshell, a $(...) or `...` among them), the function it is
    in, its index in its pipeline, and the command whose word holds the substitution it is in."""

    __slots__ = ("assigns", "words", "heredocs", "line", "start", "frames", "function", "pipe", "parent")

    def __init__(self, line, start, frames, function, pipe, parent):
        self.assigns, self.words, self.heredocs = [], [], []
        self.line, self.start, self.frames, self.function, self.pipe, self.parent = \
            line, start, frames, function, pipe, parent

    def values(self):
        return [w.value() for w in self.words]

    def __repr__(self):
        return f"ShCommand(line {self.line + 1}: {' '.join(w.source() for w in self.words)!r})"


_SH_FRAMES = iter(range(1, 1 << 62))
_SH_ASSIGNMENT = re.compile(r"([A-Za-z_]\w*)(?:\[[^\]]*\])?\+?=")
_SH_REDIRECT = re.compile(r"[0-9]*(<<<|<<-|<<|<>|<&|>>|>&|>\||&>>|&>|<|>)")
_SH_STOP = set(" \t\n;&|()<>")
_SH_PARAM = re.compile(r"\$([A-Za-z_]\w*|[0-9@*#?$!-])")


class _ShellReader:
    def __init__(self, text, line):
        self.text, self.n, self.base = text, len(text), line
        self.starts = [0] + [m.end() for m in re.finditer("\n", text)]
        self.commands, self.pending = [], []

    def line_at(self, offset):
        return self.base + bisect.bisect_right(self.starts, offset) - 1

    def blanks(self, i):
        while i < self.n:
            if self.text[i] in " \t":
                i += 1
            elif self.text.startswith("\\\n", i):
                i += 2
            else:
                break
        return i

    def heredoc_bodies(self, i):
        """Read the bodies of the heredocs opened on the line that just ended, from i."""
        while self.pending:
            doc = self.pending.pop(0)
            strip, delimiter = doc[0], doc[1]
            first, lines = i, []
            while i < self.n:
                end = self.text.find("\n", i)
                end = self.n if end < 0 else end
                line = self.text[i:end]
                i = min(end + 1, self.n)
                if (line.lstrip("\t") if strip else line) == delimiter:
                    break
                lines.append(line.lstrip("\t") if strip else line)
            doc[3], doc[4] = "\n".join(lines), self.line_at(first)
        return i

    def word(self, i, frames, function, parent):
        parts, start, text, n = [], i, self.text, self.n

        def lit(s):
            if parts and parts[-1][0] == "lit":
                parts[-1] = ("lit", parts[-1][1] + s, None)
            else:
                parts.append(("lit", s, None))

        while i < n:
            c = text[i]
            if c in "<>" and text.startswith("(", i + 1):
                nested, i = self.substitution(i + 2, frames, function, parent)
                parts.append(("cmd", "", nested))
                continue
            if c == "(" and len(parts) == 1 and parts[0][0] == "lit" and re.fullmatch(r"[A-Za-z_]\w*\+?=", parts[0][1]):
                depth, j = 0, i  # an array assignment, NAME=(a b c)
                while j < n:
                    depth += {"(": 1, ")": -1}.get(text[j], 0)
                    j += 1
                    if depth == 0:
                        break
                lit(text[i:j])
                i = j
                continue
            if c in _SH_STOP:
                break
            if c == "\\":
                if text.startswith("\n", i + 1):
                    i += 2
                    continue
                lit(text[i + 1:i + 2])
                i += 2
            elif c == "'":
                j = text.find("'", i + 1)
                j = n if j < 0 else j
                parts.append(("sq", text[i + 1:j], None))
                i = j + 1
            elif c == '"':
                i = self.double_quoted(i + 1, parts, frames, function, parent)
            elif c == "$":
                i = self.dollar(i, parts, frames, function, parent, lit)
            elif c == "`":
                i = self.backtick(i, parts, frames, function, parent)
            else:
                lit(c)
                i += 1
        return ShWord(parts, start, self.line_at(start)), i

    def double_quoted(self, i, parts, frames, function, parent):
        text, n, buf, first = self.text, self.n, [], len(parts)
        while i < n and text[i] != '"':
            c = text[i]
            if c == "\\" and i + 1 < n:
                if text[i + 1] == "\n":
                    i += 2
                    continue
                buf.append(text[i + 1] if text[i + 1] in '$`"\\' else text[i:i + 2])
                i += 2
            elif c in "$`":
                if buf:
                    parts.append(("dq", "".join(buf), None))
                    buf = []
                if c == "$":
                    i = self.dollar(i, parts, frames, function, parent, lambda s: parts.append(("dq", s, None)))
                else:
                    i = self.backtick(i, parts, frames, function, parent)
            else:
                buf.append(c)
                i += 1
        if buf or len(parts) == first:  # "" is a word of its own; "$x" is the expansion alone
            parts.append(("dq", "".join(buf), None))
        return min(i + 1, n)

    def dollar(self, i, parts, frames, function, parent, lit):
        text, n = self.text, self.n
        if text.startswith("$((", i):
            depth, j = 0, i + 1
            while j < n:
                depth += {"(": 1, ")": -1}.get(text[j], 0)
                j += 1
                if depth == 0:
                    break
            parts.append(("arith", text[i + 3:j - 2], None))
            return j
        if text.startswith("$(", i):
            nested, j = self.substitution(i + 2, frames, function, parent)
            parts.append(("cmd", "", nested))
            return j
        if text.startswith("${", i):
            depth, j = 0, i + 1
            while j < n:
                if text[j] == "\\":
                    j += 2
                    continue
                depth += {"{": 1, "}": -1}.get(text[j], 0)
                j += 1
                if depth == 0:
                    break
            inner = text[i + 2:j - 1]
            name = re.match(r"[#!]?([A-Za-z_]\w*|[0-9@*#?$!-])", inner)
            parts.append(("param", name.group(1) if name else inner, None))
            return j
        if text.startswith("$'", i):
            j, buf = i + 2, []
            while j < n and text[j] != "'":
                if text[j] == "\\" and j + 1 < n:
                    buf.append({"n": "\n", "t": "\t"}.get(text[j + 1], text[j + 1]))
                    j += 2
                    continue
                buf.append(text[j])
                j += 1
            parts.append(("sq", "".join(buf), None))
            return j + 1
        if text.startswith('$"', i):
            return self.double_quoted(i + 2, parts, frames, function, parent)
        m = _SH_PARAM.match(text, i)
        if m:
            parts.append(("param", m.group(1), None))
            return m.end()
        lit("$")
        return i + 1

    def substitution(self, i, frames, function, parent):
        """The commands of a $(...) or <(...) whose text starts at i, and the offset after its )."""
        first = len(self.commands)
        end = self.parse(i, frames + (next(_SH_FRAMES),), function, parent, closing=True)
        return self.commands[first:], end

    def backtick(self, i, parts, frames, function, parent):
        text, n, j = self.text, self.n, i + 1
        while j < n and text[j] != "`":
            j += 2 if text[j] == "\\" else 1
        inner = re.sub(r"\\([`$\\])", r"\1", text[i + 1:j])
        reader = _ShellReader(inner, self.line_at(i + 1))
        reader.parse(0, frames + (next(_SH_FRAMES),), function, parent)
        for command in reader.commands:
            command.start += i + 1
        self.commands += reader.commands
        parts.append(("cmd", "", reader.commands))
        return min(j + 1, n)

    def parse(self, i, frames, function, parent, closing=False):
        """Read commands from i to the end of the text, or, when closing, to the ) that closes a
        substitution; return the offset after it."""
        text, n = self.text, self.n
        cur, pipe, expect, until = None, 0, "cmd", None
        scopes, cases, function_name = [], [], None

        def finish():
            nonlocal cur
            if cur is not None and (cur.words or cur.assigns):
                self.commands.append(cur)
            cur = None

        def command(at):
            nonlocal cur
            if cur is None:
                cur = ShCommand(self.line_at(at), at, frames, function, pipe, parent)
            return cur

        while True:
            i = self.blanks(i)
            if i >= n:
                finish()
                return n
            c = text[i]
            if c == "#":
                end = text.find("\n", i)
                i = n if end < 0 else end
                continue
            if c == "\n":
                finish()
                pipe, i = 0, self.heredoc_bodies(i + 1)
                if expect == "args":
                    expect = "cmd"
                continue
            if expect == "pattern" and c == "|":
                i += 1
                continue
            if text.startswith((";;&", ";;", ";&"), i) and cases:
                finish()
                expect = "pattern"
                i += 3 if text.startswith(";;&", i) else 2
                continue
            redirect = _SH_REDIRECT.match(text, i) if c in "<>&0123456789" else None
            if redirect and not (c == "&" and redirect.group(1)[0] != "&") \
                    and not (c in "<>" and text.startswith("(", i + 1)):
                op = redirect.group(1)
                target, i = self.word(self.blanks(redirect.end()), frames, function, cur)
                if op in ("<<", "<<-"):
                    doc = [op == "<<-", target.value(""), target.quoted, "", 0]
                    self.pending.append(doc)
                    command(redirect.start()).heredocs.append(doc)
                continue
            if c in ";&|":
                op = text[i:i + 2] if text[i:i + 2] in ("&&", "||", "|&") else c
                finish()
                pipe = pipe + 1 if op in ("|", "|&") else 0
                if expect in ("args", "cmd"):
                    expect = "cmd"
                i += len(op)
                continue
            if c == "(":
                if expect == "pattern":
                    i += 1
                    continue
                after = self.blanks(i + 1)
                if cur is not None and len(cur.words) == 1 and not cur.assigns and text.startswith(")", after):
                    function_name, cur = cur.words[0].value(), None  # name() { ... }
                    i, expect = after + 1, "cmd"
                    continue
                if function_name and text.startswith(")", after):
                    i = after + 1  # function name() after the keyword
                    continue
                if text.startswith("((", i):
                    depth, j = 0, i
                    while j < n:
                        depth += {"(": 1, ")": -1}.get(text[j], 0)
                        j += 1
                        if depth == 0:
                            break
                    i = j
                    continue
                finish()
                scopes.append((frames, function))
                frames = frames + (next(_SH_FRAMES),)
                if function_name:
                    function, function_name = function_name, None
                i += 1
                continue
            if c == ")":
                if expect == "pattern":
                    expect = "cmd"
                    i += 1
                    continue
                finish()
                if scopes:
                    frames, function = scopes.pop()
                    i += 1
                    continue
                if closing:
                    return i + 1
                i += 1
                continue
            w, at = self.word(i, frames, function, cur if expect == "args" else None)
            i = at if at > i else i + 1
            value = w.value() if w.static and not w.quoted else None
            if expect == "skip":
                if value == until:
                    expect = "pattern" if until == "in" else "cmd"
                continue
            if expect == "pattern":
                if value == "esac":
                    cases.pop()
                    expect = "args"
                continue
            if expect == "case":
                expect, until = "skip", "in"
                cases.append(True)
                continue
            if expect == "fname":
                function_name, expect = value, "cmd"
                continue
            if expect == "cmd":
                if value == "esac" and cases:
                    finish()
                    cases.pop()
                    continue
                if value in ("if", "then", "else", "elif", "do", "while", "until", "!", "time", "fi", "done"):
                    finish()
                    continue
                if value in ("for", "select"):
                    finish()
                    expect, until = "skip", "do"
                    continue
                if value == "case":
                    finish()
                    expect = "case"
                    continue
                if value == "[[":
                    finish()
                    expect, until = "skip", "]]"
                    continue
                if value == "function":
                    finish()
                    expect = "fname"
                    continue
                if value == "{":
                    finish()
                    scopes.append((frames, function))
                    if function_name:
                        frames, function, function_name = frames + (next(_SH_FRAMES),), function_name, None
                    continue
                if value == "}" and scopes:
                    finish()
                    frames, function = scopes.pop()
                    continue
                first = w.parts[0] if w.parts else None
                assignment = _SH_ASSIGNMENT.match(first[1]) if first and first[0] == "lit" else None
                if assignment and (cur is None or not cur.words):
                    rest = [("lit", first[1][assignment.end():], None)] + w.parts[1:]
                    command(w.start).assigns.append((assignment.group(1), ShWord(rest, w.start, w.line)))
                    continue
                command(w.start)
                for kind, _, nested in w.parts:  # its substitutions ran before it, in its context
                    for inner in nested or ():
                        if inner.parent is None:
                            inner.parent = cur
                cur.words.append(w)
                expect = "args"
                continue
            command(w.start).words.append(w)


def shell_script(text, line=0):
    """The simple commands of a shell text, nested ones included (those of each $(...), `...` and
    <(...)), in the order they start. line is the 0-based line the text starts on in its file. A
    continued line is one line; a heredoc's body is no command (shell_script reads it when a shell
    runs it); a for list, a case pattern and a [[ test ]] are no command."""
    reader = _ShellReader(text, line)
    reader.parse(0, (), None, None)
    return sorted(reader.commands, key=lambda command: (command.line, command.start))


SHELLS = {"bash", "sh", "dash", "zsh", "ksh"}


def shell_command_word(command):
    """The index of the word a command runs after `env` with its settings and options, `command`,
    `exec`, `nice`, `nohup`, `time`, `timeout` and `flock` with their operands, or None."""
    values, k = command.values(), 0
    while k < len(values):
        v = values[k]
        base = v.rsplit("/", 1)[-1]
        if base == "env":
            k += 1
            while k < len(values) and (values[k].startswith("-") or _SH_ASSIGNMENT.match(values[k])):
                k += 2 if values[k] in ("-u", "-C", "-S") else 1
            continue
        if base in ("command", "exec", "nohup", "time", "builtin"):
            k += 1
            while k < len(values) and values[k].startswith("-"):
                k += 1
            continue
        if base in ("nice", "timeout", "flock"):
            k += 1
            while k < len(values) and values[k].startswith("-"):
                k += 2 if values[k] in ("-n", "-s", "-k", "-w", "-E") else 1
            if base in ("timeout", "flock") and k < len(values):
                k += 1  # the duration, or the lock file
            continue
        return k
    return None


def shell_env_elements(command, upto):
    """The settings a command's own words make for the program at word index upto: its NAME=value
    prefix, then each `env` before that word with its NAME=value, -u NAME and -i."""
    out = [("set", name, value.value() if value.static else None) for name, value in command.assigns]
    values, k = command.values(), 0
    while k < upto:
        if values[k].rsplit("/", 1)[-1] == "env":
            k += 1
            while k < upto:
                v = values[k]
                if v in ("-i", "-", "--ignore-environment"):
                    out.append(CLEAR)
                elif v in ("-u", "--unset") and k + 1 < upto:
                    out.append(("set", values[k + 1], None))
                    k += 1
                elif v.startswith("--unset="):
                    out.append(("set", v[len("--unset="):], None))
                elif _SH_ASSIGNMENT.match(v):
                    name, _, value = v.partition("=")
                    out.append(("set", name, value if command.words[k].static else None))
                elif not v.startswith("-"):
                    break
                k += 1
            continue
        k += 1
    return out


def shell_inner_scripts(command):
    """(text, line) of the shell each command hands more shell to: the string after a shell's -c
    (bash -c, sh -lc, ...), what eval joins, and a heredoc a shell with no script operand reads."""
    values, out = command.values(), []
    if values and values[0] == "eval":
        out.append((" ".join(values[1:]), command.words[1].line if len(command.words) > 1 else command.line))
        return out
    k = shell_command_word(command)
    if k is None or values[k].rsplit("/", 1)[-1] not in SHELLS:
        return out
    j, takes_c = k + 1, False
    while j < len(values) and values[j].startswith("-"):
        if re.fullmatch(r"-[a-zA-Z]*c[a-zA-Z]*", values[j]):
            takes_c = True
        j += 2 if re.fullmatch(r"[-+][a-zA-Z]*[oO]", values[j]) else 1
    if takes_c and j < len(values):
        out.append((values[j], command.words[j].line))
    elif j >= len(values):
        out += [(doc[3], doc[4]) for doc in command.heredocs]
    return out


# --- Reasons and reports -------------------------------------------------------------------------


def reasoned(raw, lang, marker, lines, window=REASON_WINDOW):
    """Whether a comment matching marker stands on one of lines, or on a comment line within window
    lines above the first of them."""
    first = min(lines)
    if any(marker.search(raw[k]) for k in lines):
        return True
    return any(COMMENT[lang].match(l) and marker.search(l) for l in raw[max(0, first - window):first])


def unknown(message):
    """A scan that could not see: UNKNOWN, exit 2, so it never reads as a clean one."""
    print(f"UNKNOWN: {message}", file=sys.stderr)
    sys.exit(2)


def refuse(errors, summary, advice):
    """Each (path, line, message) as a GitHub annotation and a line of its own, then advice; exit 1."""
    print(f"{len(errors)} problems over the {summary}:", file=sys.stderr)
    for path, line, message in errors:
        print(f"::error file={path},line={line}::{path}:{line}: {message}")
        print(f"  {path}:{line}: {message}", file=sys.stderr)
    print("\n" + advice, file=sys.stderr)
    sys.exit(1)
