"""The one reading of a repository's source files that check-go-offline.sh and
check-omp-offline.sh share: which files are source and in which language, how a file is read, how
Go, TypeScript and Python are lexed into code with their comments and the text of their string
literals blanked, how shell is read the same way, and where a reason comment counts. Each check
keeps only its own rule: what a site is, and what makes one pass.

Which string literals stay code is the caller's input (`keep`), never a second lexer: the go check
keeps the words a go command is spelled with, the Oh My Pi check the variable a door reads and the
binary's own name. So a fixture or a message that spells a command starts nothing in either check,
and a correction to how a language is read is made here once, for both.

Imported with PYTHONPATH naming the directory this file lives in, as the checks' own lines say.
Its own test is source_scan.test.sh.
"""
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


def continued(raw, index):
    """The shell line at index with each backslash-continued line after it, joined by newlines."""
    logical, j = raw[index], index
    while logical.rstrip().endswith("\\") and j + 1 < len(raw):
        j += 1
        logical += "\n" + raw[j]
    return logical


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
