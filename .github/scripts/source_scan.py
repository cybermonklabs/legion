"""The one reading of a repository's source files that check-go-offline.sh and
check-omp-offline.sh share: which files are source and in which language, how a file is read, how
Go, TypeScript and Python are lexed into code with their comments and the text of their string
literals blanked, how shell is read the same way, and where a reason comment counts. Each check
keeps only its own rule: what a site is, and what makes one pass.

Which string literals stay code is the caller's input (`keep`), never a second lexer: the Oh My Pi
check keeps the variable a door reads and the binary's own name. So a fixture or a message that
spells a command starts nothing in either check, and a correction to how a language is read is
made here once, for both.

It reads statements too, as each language parses them, in a module of its own for each:
source_statements.py's Source gives a Go or TypeScript file's calls with their arguments across
lines, its assignments and the value a name is bound to; source_env.py the environment a call's own
options (TypeScript) or a Cmd's own Env (Go) set; source_python.py does the same for Python with
the ast module; source_shell.py's shell_script gives a shell text's simple commands, a continued
line one line, a $(...) and `...` read as commands of their own, a heredoc body as no command,
each with its NAME=value prefix, its function and its place in a pipeline. An environment is a
list of elements in the order a program makes them (see INHERIT, here, which they all share).

Imported with PYTHONPATH naming the directory this file lives in, as the checks' own lines say.
Its own test is source_scan.test.sh, which tests the modules beside it too.
"""
import os
import re
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


# A first line that runs the file with a shell: #!/bin/bash, #!/usr/bin/env sh, ...
SHELL_SHEBANG = re.compile(rb"#!\s*\S*?(?:/|\benv\s+(?:-\S+\s+)*)(?:bash|sh|dash|zsh|ksh)\b")


def sources(root=".", shebang=False):
    """(path, language) of every source file under root, in a stable order: each path relative to
    root, no directory SKIP_DIRS names, no symbolic link. With shebang, a file whose name gives it
    no language is shell when its first line runs it with a shell (a test named `x.test` or `run`)."""
    for directory, dirs, files in os.walk(root):
        dirs[:] = sorted(d for d in dirs if d not in SKIP_DIRS)
        for name in sorted(files):
            path = os.path.relpath(os.path.join(directory, name), root)
            lang = language(path)
            full = os.path.join(root, path)
            if not os.path.isfile(full) or os.path.islink(full):
                continue
            if lang is None and shebang:
                with open(full, "rb") as file:
                    lang = "sh" if SHELL_SHEBANG.match(file.readline(128)) else None
            if lang:
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


# --- Environments, as every reader of a program's settings gives them back ----------------------

# A value environment readers give back, one element per setting in the order the program makes
# them: ("set", NAME, VALUE) where VALUE is None when it is not a literal, ("inherit",) for the
# environment the process was given, ("unknown",) for anything else that may hold any variable, and
# ("clear",) for an environment emptied there. The last element that may set a variable decides it.
INHERIT, UNKNOWN, CLEAR = ("inherit",), ("unknown",), ("clear",)


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
