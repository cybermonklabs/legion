#!/usr/bin/env python3
"""Holds each citation under .github/ to the file it names, and refuses a citation of any other
shape there.

A citation names what it means inside a file, never a line of it: a line number names whatever
line a later edit moves there, and nothing reads a citation of a file the citing change does not
touch (.github/ci-notes/ci.md#a-citation-names-a-symbol-never-a-line). So a citation is one
exact token on one line: a backtick, the path of a tracked file from the repository root as git
ls-files prints it (no ./ and no leading /), a hash, a name, a backtick. This docstring's own two
are `.github/workflows/ci.yaml#browser_image` and `.github/scripts/ci_citations.py#held`. Several
names are several tokens, each with its path.

What this check claims, and all it claims:
- A token whose path is no tracked file fails, and so does a token whose name is a line number.
- A token whose name the file's code does not hold fails. The code is the file as its language's
  own scanner reads it (below), so a renamed symbol whose old name lives on in a comment fails.
  In Python, TypeScript, JavaScript and Go a name that is an identifier (letters, digits and
  underscores, and no digit first) must be one of the file's identifier tokens, so a string or
  a docstring that keeps the old name holds nothing either. Any other name is held where the
  code holds that text and neither end of it runs on into a longer word: a name that starts
  with a letter, a digit, an underscore or a hyphen has none of those right before it, and one
  that ends with one has none right after it. So the name setup-go is held by neither
  setup-golang nor setup-go-x.
- A code span that starts with a path word and goes on with a hash, a colon or two colons and a
  name, and is no token, fails: such as ci.yaml:x or ci.yaml::x in backticks, or a token that
  wraps onto another line. A code span is read as CommonMark pairs one: from a run of backticks
  to the next run of as many, over as many lines as its paragraph holds, which a blank line, or
  a line that holds only a comment marker, ends. A token is one line so that a search for it
  finds every citation of a name; a wrapped one is refused rather than joined, since where the
  wrap fell, at a space or inside a word, and what comment marker it added, is a guess. In a
  span's line break a comment marker is one only where a space or the line's end follows it,
  so a token that wraps just before its hash is refused too. A path word followed by a space
  alone is a command, as `release-push.sh release` is, and is read as prose.
- A path word followed right away by a hash and a name, outside a token, fails: a token with
  its backticks left off, or one inside a code span that other text starts, such as a span
  whose first word is see. Such a span's path is not its first word, so the rule above does not
  read it. A Markdown file's path, a hash and a heading's anchor is a link to that heading, and
  passes. A hash and a number is the line-number rule's.
  Inside a code span the path does not start, a colon after it is read as prose, being a
  command's or an annotation's, as in ::error file=ci.yaml::x, and so is a space before a hash.
- A line number written beside a path word, in a spelling this check knows, fails: a colon, an
  L or a hash and the number right after the path, the path at the end of a line and the number
  at the start of the next, and the words line or lines with a number beside the path (before
  it, after it, in parentheses, after a possessive or a comma). In the same line, or the next
  one, so does a bare colon and number after an opening parenthesis, "at", "and" or a comma,
  and an l. or a hash and a number in parentheses, and the word line run into its number. A
  spelling it does not know passes, such as a colon and a space before the number, an @ before
  it, "ln" or "around line": the rule is these spellings and no others.
- A backticked text that is no token fails only where it stands beside a path word, in one of
  two forms: right after the path word, with nothing between them or a possessive, a comma, a
  colon or an opening parenthesis, on the path's line or wrapped onto the next; or followed by
  in, of or from and the path word. Either is the shape of a name written beside its path, a
  citation this check could not hold, so it is refused rather than passed unread: rewrite a
  citation as a token, and reword a text that is no citation so that it does not stand beside
  the path. Any other backticked text is read as prose and is not checked, so a citation
  written further from its path, as in "ci.yaml holds, among others, the `x` output", is
  neither held nor refused. That is the stated limit of this rule, which its test shows.
A path word is a word that is a tracked file's path, or the end of one after a slash that holds
a slash or a dot, or a capitalised file name such as CODEOWNERS or Dockerfile: browser.Dockerfile,
ci.yaml and scripts/build.ts are path words when a tracked file's path ends so.

The scope is every tracked text file under .github/, read as plain text: no comment, string,
heredoc or Markdown structure of the citing file is parsed, so a citation counts wherever it
stands. A tracked symlink is skipped, being another file's text, which is read at its own path
when that is under .github/; a file that is not UTF-8 is no text. Nothing outside .github/ is
read, held or refused.

How the code of a cited file is read, each language by its own scanner:
- Python through its tokenize module: its NAME tokens are its identifiers, and a name that is
  no identifier is held against its tokens with every comment and every string removed, a
  docstring being a string. A file tokenize refuses holds no name.
- TypeScript, TSX, JavaScript, .mjs and .cjs through the typescript package's parser, in one bun
  run of .github/scripts/ci_citations_code.ts over every such file a token cites: its
  identifiers, and its text with its comments removed and its strings, templates and regular
  expressions kept. The run starts in the repository's root (the fixture's, in the test), and
  the scanner's import of typescript resolves from the directory the scanner stands in, this
  repository's: in CI from the node_modules that the lint job's bun install --frozen-lockfile
  fills from bun.lock. Where there is no node_modules, as in a fresh checkout, the import does
  not fail: Bun installs typescript itself into its own cache, at the version the root
  package.json and bun.lock give, and only a local run meets that.
- Go through go/scanner, in one go run of .github/scripts/ci_citations_code.go over every Go file
  a token cites: its identifiers, and its text with its comments removed and its strings kept.
- Shell, YAML, TOML, jq, Dockerfiles, CODEOWNERS, ignore files and an extension-free script that
  starts with #!: a hash comment at a line's start or after whitespace removed, outside '...' and
  "...".
- Any other file, Markdown, JSON and lock files among them, as it is written.
A bun or go run that fails fails the check, naming the command and what it printed.

Usage: ci_citations.py   (no arguments)

It prints one problem a line, as file, line and message, and exits 1 when there is one, and
when the scan finds no token at all, which would mean it read nothing it could hold.
"""
import bisect
import io
import os
import re
import subprocess
import sys
import tokenize

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
# The scanner programs beside this file. A test links this file into a fixture, which moves ROOT
# there and leaves the programs, and the typescript package they load, in this repository.
SCANNERS = os.path.dirname(os.path.realpath(__file__))
TS_SCANNER = ".github/scripts/ci_citations_code.ts"
GO_SCANNER = ".github/scripts/ci_citations_code.go"
SCOPE = ".github/"
BACKTICK = "\x60"


def token(path, name):
    """A citation token, built here so that this file's own text holds none it did not mean."""
    return BACKTICK + path + "#" + name + BACKTICK


def by_line(path):
    """The refusal of a citation of path by a line number."""
    return f"cites {BACKTICK}{path}{BACKTICK} by a line number; write it as {token(path, 'Name')}"


# The token, with the backtick written as \x60 for the reason token() gives.
TOKEN = re.compile(r"\x60([^\x60\s#]+)#([^\x60\n]+)\x60")
LINE_NAME = re.compile(r"^L?\d+(?:\s*[-,:]\s*L?\d+)*$")
# A word that may be a path: path characters after none of them, behind any ./ ../ or /. A word
# inside a URL is never one, as each part of a URL's path follows a slash.
CANDIDATE = re.compile(r"(?<![\w.@/~$%+-])((?:\.\.?/)*/?)([\w@%+-][\w.@%+/-]*|\.[\w@%+-][\w.@%+/-]*)")
# A comment marker at the start of a line of a comment.
MARKER = r"(?:#+|//+|\*|--)"
# A line that holds nothing but a comment marker, which ends a paragraph as a blank line does.
BLANK_LINE = rf"[ \t]*{MARKER}?[ \t]*(?:\n|$)"
# A code span: a run of backticks, then text up to a run of as many, over as many lines as its
# paragraph holds, as CommonMark pairs one; a blank line ends the paragraph, and the span with it.
CODE_SPAN = re.compile(rf"(\x60+)(?=[^\x60])([^\x60\n]*(?:\n(?!{BLANK_LINE})[^\x60\n]*)*)\1(?!\x60)")

# A line break in a comment or a paragraph, past the next line's comment marker: the one place
# the patterns below that read across a wrap take it from.
LINE_BREAK = rf"[ \t]*\n[ \t]*{MARKER}?[ \t]*"
# A line break inside a code span, which its reading joins to one space. There a comment marker
# is one only where a space or the line's end follows it: a hash with a name right after the
# break is the hash of a token that wrapped just before it, and stays.
SPAN_BREAK = re.compile(rf"[ \t]*\n[ \t]*(?:{MARKER}(?=\s))?[ \t]*")
# A hash and a name right after a path word, outside a token: a token's shape with no backticks,
# or inside a code span that the path does not start. A hash and a number is the line-number
# rule's.
HASH_NAME = re.compile(r"#(?!L?\d)([^\s\x60#]+)")
# The anchor of a Markdown heading, as a link names one, and any closing punctuation after it.
ANCHOR = re.compile(r"[\w-]+[)\].,;:'\"]*")
# What may stand between a code span's opening backticks and the path word that starts it.
SPAN_LEAD = re.compile(r"\s*['\"]?")
# Whitespace, which may wrap onto the next line.
GAP = rf"(?:[ \t]+|{LINE_BREAK})"
GAP0 = f"(?:{GAP})?"
NUMBERS = rf"\d+(?:{GAP0}(?:-|–|to|and|,){GAP0}\d+)*\b"
# A line number right after a path word, which may be closed by a backtick or a quote.
AFTER_PATH = re.compile(
    rf"(?:[\x60\"]|'(?!s\b))?"
    rf"(?::L?\d|#L?\d|:{LINE_BREAK}\d|{GAP}L\d+\b"
    rf"|(?:'s|’s)?,?{GAP0}\(?[ \t]*(?:(?:at|on|in){GAP})?[Ll]ines?{GAP}\d)")
# A line number written before a path word that starts where this match ends.
BEFORE_PATH = re.compile(rf"\b[Ll]ines?{GAP}{NUMBERS}{GAP}(?:of|in){GAP}(?:the{GAP})?[\x60\"']?$")
# Spellings that carry no path of their own, refused in a path word's line and the next.
NEAR_PATH = re.compile(r"\(l\.\s*\d+\)|\(#\d+\)|\bline\d+\b|(?:\(|\bat\s+|\band\s+|,\s*):\d+\b")
# What may stand between a path word, or the code span that is that word alone, and a backticked
# text right after it: a closing quote, a possessive, a comma, a colon or an opening parenthesis,
# on the same line or the next.
NAME_AFTER = re.compile(rf"(?:\"|'(?!s\b))?(?:'s|’s|,|:)?{GAP0}\(?[ \t]*")
# What may stand between a backticked text and the path word after it: in, of or from.
NAME_FIRST = re.compile(rf"{GAP}(?:in|of|from){GAP}(?:the{GAP})?[\"']?")

TS_KINDS = {"ts", "tsx", "js", "mjs", "cjs"}
HASH_KINDS = {"sh", "bash", "yaml", "yml", "toml", "jq", "Dockerfile", "Makefile", "CODEOWNERS",
              "gitignore", "dockerignore", "shellcheckrc"}
# The tokens of a Python file that are no code: its comments and every part of every string.
PYTHON_TEXT = {getattr(tokenize, kind) for kind in ("COMMENT", "STRING", "FSTRING_START", "FSTRING_MIDDLE",
                                                    "FSTRING_END", "TSTRING_START", "TSTRING_MIDDLE",
                                                    "TSTRING_END") if hasattr(tokenize, kind)}


class ScanFailed(Exception):
    """A scanner program's run that failed, so no file it was given has code to hold a name in."""


# ---- the code of a cited file -----------------------------------------------------------------

def held(name, code):
    """Whether code holds name with neither end of it running on into a longer word: where the
    name starts with a letter, a digit, an underscore or a hyphen, none of those stands right
    before it, and where it ends with one, none stands right after it."""
    pattern = re.escape(name)
    if re.match(r"[\w-]", name):
        pattern = r"(?<![\w-])" + pattern
    if re.search(r"[\w-]$", name):
        pattern += r"(?![\w-])"
    return re.search(pattern, code) is not None


class Code:
    """The code of a cited file as its language's scanner reads it: the text a name is held
    against, and for Python, TypeScript, JavaScript and Go the identifier tokens that an
    identifier must be one of."""

    def __init__(self, text, identifiers=None):
        self.text = text
        self.identifiers = identifiers

    def holds(self, name):
        if self.identifiers is not None and name.isidentifier():
            return name in self.identifiers
        return held(name, self.text)


def kind_of(path, body):
    base = os.path.basename(path)
    if base == "Dockerfile" or base.endswith(".Dockerfile"):
        return "Dockerfile"
    if "." in base[1:]:
        return base.rsplit(".", 1)[1]
    if base.startswith("."):
        return base[1:]
    return "sh" if body.startswith("#!") else base


def string_end(body, i):
    """The index after the string literal whose quote is body[i], or the newline it meets first,
    so a stray quote can hide no more than its line."""
    quote, j = body[i], i + 1
    while j < len(body):
        if body[j] == "\\":
            j += 2
        elif body[j] == quote:
            return j + 1
        elif body[j] == "\n":
            return j
        else:
            j += 1
    return len(body)


def strip_hash(body):
    """A # comment at a line's start or after whitespace, outside '...' and "..."."""
    out, i, n = [], 0, len(body)
    while i < n:
        ch = body[i]
        if ch in "'\"":
            j = string_end(body, i)
            out.append(body[i:j])
            i = j
        elif ch == "#" and (i == 0 or body[i - 1] in " \t\n"):
            j = body.find("\n", i)
            i = n if j < 0 else j
        else:
            out.append(ch)
            i += 1
    return "".join(out)


def python_code(body):
    """A Python file's NAME tokens, and its text with every comment and string blanked; or why
    tokenize reads no code in it."""
    try:
        tokens = list(tokenize.generate_tokens(io.StringIO(body).readline))
    except (tokenize.TokenError, SyntaxError) as error:
        return f"tokenize refuses it ({error})"
    starts = [0] + [m.end() for m in re.finditer("\n", body)]
    text = list(body)
    for t in tokens:
        if t.type in PYTHON_TEXT:
            start = starts[t.start[0] - 1] + t.start[1]
            end = starts[t.end[0] - 1] + t.end[1]
            text[start:end] = re.sub(r"[^\n]", " ", body[start:end])
    return Code("".join(text), {t.string for t in tokens if t.type == tokenize.NAME})


def scanned_code(runner, program, paths, **env):
    """The Code of each path from one run of a scanner program over them all, which takes the
    paths on stdin and prints each one's code and then its identifiers, one a line, each followed
    by a NUL byte. program is its path from the repository root, which names it in a refusal; the
    file run is the one beside this script."""
    if not paths:
        return []
    shown = " ".join([*runner, program])
    try:
        done = subprocess.run([*runner, os.path.join(SCANNERS, os.path.basename(program))], cwd=ROOT,
                              input="".join(p + "\0" for p in paths).encode(), capture_output=True,
                              env={**os.environ, **env})
    except OSError as error:
        raise ScanFailed(f"{BACKTICK}{shown}{BACKTICK} did not start ({error}), so the "
                         f"{len(paths)} files it reads hold no name") from error
    fields = done.stdout.decode("utf-8", "replace").split("\0")[:-1]
    if done.returncode != 0 or len(fields) != 2 * len(paths):
        printed = " ".join(done.stderr.decode("utf-8", "replace").split())[:2000]
        raise ScanFailed(f"{BACKTICK}{shown}{BACKTICK} over {len(paths)} files exited {done.returncode} "
                         f"and printed the code of {len(fields) // 2}, so they hold no name: {printed}")
    return [Code(text, set(identifiers.split("\n"))) for text, identifiers in zip(fields[::2], fields[1::2])]


class Repo:
    """The tracked files, the files a path word names among them, and the code of each cited one."""

    def __init__(self):
        out = subprocess.run(["git", "-C", ROOT, "ls-files", "-z"], capture_output=True, check=True).stdout
        self.files = sorted(f for f in out.decode("utf-8").split("\0") if f)
        self.exact = {f for f in self.files if os.path.isfile(os.path.join(ROOT, f))}
        self.named = {}
        for f in self.exact:
            parts = f.split("/")
            self.named.setdefault(f, set()).add(f)
            for i in range(1, len(parts)):
                tail = "/".join(parts[i:])
                # main or legion alone is an English word as often as a file name, so a bare
                # name is a path word only with a dot in it or a capital first letter.
                if "/" in tail or "." in tail or tail[:1].isupper():
                    self.named.setdefault(tail, set()).add(f)
        # path: its Code, or why it holds no name.
        self.code = {}

    def names(self, word):
        """The tracked files a word names: the file at that path, or those whose path ends so."""
        return self.named.get(word, set())

    def shown(self, word):
        """The root path of the one file a path word names, else the word as written."""
        found = self.names(word)
        return next(iter(found)) if len(found) == 1 else word

    def load(self, paths):
        """Reads the code of each cited path before any token is held, so that each scanner
        program runs once: every TypeScript and JavaScript file in one bun run, every Go file in
        one go run."""
        scripts, go = [], []
        for path in sorted(paths):
            try:
                with open(os.path.join(ROOT, path), encoding="utf-8") as f:
                    body = f.read()
            except (OSError, UnicodeDecodeError) as error:
                self.code[path] = f"it is no UTF-8 text ({error})"
                continue
            kind = kind_of(path, body)
            if kind in TS_KINDS:
                scripts.append(path)
            elif kind == "go":
                go.append(path)
            elif kind == "py":
                self.code[path] = python_code(body)
            elif kind in HASH_KINDS:
                self.code[path] = Code(strip_hash(body))
            else:
                self.code[path] = Code(body)
        self.code.update(zip(scripts, scanned_code(["bun"], TS_SCANNER, scripts)))
        self.code.update(zip(go, scanned_code(["go", "run"], GO_SCANNER, go, GOWORK="off")))


# ---- the citing files -------------------------------------------------------------------------

class Citing:
    """A citing file's text: its tokens, the text with each token blanked so that the rules after
    the tokens' own read the text around them, its lines, its path words and its code spans."""

    def __init__(self, body, repo):
        self.tokens = list(TOKEN.finditer(body))
        masked = list(body)
        for m in self.tokens:
            masked[m.start():m.end()] = " " * (m.end() - m.start())
        self.text = "".join(masked)
        self.starts = [0] + [m.end() for m in re.finditer("\n", body)]
        # (start, end, word) of each path word.
        self.words = []
        for m in CANDIDATE.finditer(self.text):
            word = m.group(2).rstrip(".")
            if word and repo.names(word):
                self.words.append((m.start(), m.start(2) + len(word), word))
        # (start, end, text with each line break joined to one space) of each code span, paired
        # from left to right as Markdown pairs them, so a closing backtick is never read as an
        # opening one, and where the text of each starts, after its backticks.
        spans = list(CODE_SPAN.finditer(self.text))
        self.spans = [(m.start(), m.end(), SPAN_BREAK.sub(" ", m.group(2))) for m in spans]
        self.opens = [s for s, _, _ in self.spans]
        self.bodies = [m.start(2) for m in spans]

    def line(self, offset):
        return bisect.bisect_right(self.starts, offset)

    def line_end(self, number):
        return self.starts[number] - 1 if number < len(self.starts) else len(self.text)

    def span_at(self, offset):
        """The index of the code span that holds offset, or None."""
        i = bisect.bisect_right(self.opens, offset) - 1
        return i if i >= 0 and offset < self.spans[i][1] else None

    def leads(self, i, start):
        """Whether the path word at start, with any ./ before it, is the first word of code span i."""
        return SPAN_LEAD.fullmatch(self.text, self.bodies[i], start) is not None


def token_problems(citing, repo):
    """Each token whose path is no tracked file, whose name is a line number, or whose name its
    file's code does not hold."""
    for m in citing.tokens:
        cited, name = m.group(1), m.group(2)
        here = citing.line(m.start())
        if cited not in repo.exact:
            yield here, f"{BACKTICK}{cited}{BACKTICK} names no tracked file"
        elif LINE_NAME.match(name.strip()):
            yield here, by_line(cited)
        elif isinstance(repo.code[cited], str):
            yield here, f"{BACKTICK}{cited}{BACKTICK} holds no name this check can read: {repo.code[cited]}"
        elif not repo.code[cited].holds(name):
            yield here, f"{BACKTICK}{cited}{BACKTICK} holds no {BACKTICK}{name}{BACKTICK}"


def line_number_problems(citing, repo):
    """Each line number written beside a path word, once a line for each file it names."""
    refused = set()
    for start, end, word in citing.words:
        shown = repo.shown(word)
        here = citing.line(start)
        window = citing.text[citing.starts[here - 1]:citing.line_end(min(here + 1, len(citing.starts)))]
        if (AFTER_PATH.match(citing.text, end) or BEFORE_PATH.search(citing.text, max(0, start - 200), start)
                or NEAR_PATH.search(window)) and (here, shown) not in refused:
            refused.add((here, shown))
            yield here, by_line(shown)


def span_problems(citing, repo):
    """Each code span that starts with a path word and goes on with a hash, a colon or two colons
    and a name, and so is a citation, and is no token, over however many lines it runs: a token
    wraps no line, and any other form of one is refused rather than skipped. A line number after
    the colon is the line-number rule's."""
    refused = set()
    for start, _, word in citing.words:
        i = citing.span_at(start)
        if i is None or i in refused:
            continue
        s, e, inner = citing.spans[i]
        cited = re.fullmatch(r"\s*['\"]?(?:\.\.?/)*/?" + re.escape(word) + r"\s*(?:#|::?)\s*(?!L?\d)(\S(?:.*\S)?)\s*",
                             inner)
        if not cited:
            continue
        refused.add(i)
        shown, name = repo.shown(word), cited.group(1)
        if "\n" in citing.text[s:e]:
            yield citing.line(s), (f"this code span wraps {BACKTICK}{shown}{BACKTICK} and a name onto another "
                                   f"line, and a citation token is one line: write it as {token(shown, name)} "
                                   f"on one line")
        else:
            yield citing.line(s), (f"{BACKTICK}{inner.strip()}{BACKTICK} cites {word} in a form that is no "
                                   f"citation token: write it as {token(shown, name)}")


def hash_problems(citing, repo):
    """Each path word followed right away by a hash and a name, outside a token and outside a
    code span the word starts, which the span rule reads: a token with no backticks around it,
    or inside a code span that some other text starts. A Markdown file's path, a hash and a
    heading's anchor is a link's, and is not refused."""
    for start, end, word in citing.words:
        m = HASH_NAME.match(citing.text, end)
        if m is None:
            continue
        i = citing.span_at(start)
        if i is not None and citing.leads(i, start):
            continue
        if all(f.endswith(".md") for f in repo.names(word)) and ANCHOR.fullmatch(m.group(1)):
            continue
        name = m.group(1).rstrip(".,;:'\"")
        where = "outside backticks" if i is None else "inside a code span that does not start with it"
        yield citing.line(start), (f"{word}#{name} cites {word} {where}, and is no citation token: write it "
                                   f"as {token(repo.shown(word), name)}, a code span of its own")


def beside_problems(citing, repo):
    """Each backticked text that is no token right after a path word, or before in, of or from
    and one."""
    spans, opens = citing.spans, citing.opens
    for start, end, word in citing.words:
        shown = repo.shown(word)
        # The word, or the code span that holds it and nothing else; a word inside a longer span,
        # such as a command, is no path a name is written beside.
        i = citing.span_at(start)
        if i is not None:
            s, e, inner = spans[i]
            if not re.fullmatch(r"\s*['\"]?(?:\.\.?/)*/?" + re.escape(word) + r"\.?['\"]?\s*", inner):
                continue
            start, end = s, e
        j = bisect.bisect_left(opens, end)
        if j < len(spans) and NAME_AFTER.fullmatch(citing.text, end, spans[j][0]) and spans[j][2].strip():
            inner = spans[j][2].strip()
            yield citing.line(spans[j][0]), (f"{word} is followed by {BACKTICK}{inner}{BACKTICK}, which is no "
                                             f"citation token: write it as {token(shown, inner)}, or reword the "
                                             f"sentence so that the text does not follow the path")
        j = bisect.bisect_right(opens, start) - 1
        while j >= 0 and spans[j][1] > start:
            j -= 1
        if j >= 0 and NAME_FIRST.fullmatch(citing.text, spans[j][1], start) and spans[j][2].strip():
            inner = spans[j][2].strip()
            yield citing.line(spans[j][0]), (f"{BACKTICK}{inner}{BACKTICK} is followed by {word}, and is no "
                                             f"citation token: write it as {token(shown, inner)}, or reword "
                                             f"the sentence")


# ---- the scan ---------------------------------------------------------------------------------

def scanned(repo):
    """Every tracked text file under .github/: no symlink, and UTF-8."""
    for path in repo.files:
        full = os.path.join(ROOT, path)
        if not path.startswith(SCOPE) or os.path.islink(full) or not os.path.isfile(full):
            continue
        try:
            with open(full, encoding="utf-8") as f:
                yield path, f.read()
        except UnicodeDecodeError:
            continue


def main(argv):
    if argv:
        print("ci_citations.py takes no arguments: it reads every tracked text file under .github/")
        return 2
    repo = Repo()
    citing = [(path, Citing(body, repo)) for path, body in scanned(repo)]
    try:
        repo.load({m.group(1) for _, c in citing for m in c.tokens if m.group(1) in repo.exact})
    except ScanFailed as failure:
        print(f"ci_citations.py: {failure}")
        return 1
    found = []
    for path, c in citing:
        problems = [*token_problems(c, repo), *line_number_problems(c, repo), *span_problems(c, repo),
                    *hash_problems(c, repo), *beside_problems(c, repo)]
        found.extend(f"{path}:{number}: {message}" for number, message in sorted(problems))
    tokens = sum(len(c.tokens) for _, c in citing)
    holding = sum(1 for _, c in citing if c.tokens)
    for problem in found:
        print(problem)
    if found:
        return 1
    if tokens == 0:
        print(f"found no citation token in the {len(citing)} text files under {SCOPE}: a scan that finds "
              "none holds none, so this is a failure, not a pass")
        return 1
    print(f"{tokens} citation tokens in {holding} of the {len(citing)} text files under {SCOPE} "
          f"name text their files hold, and no citation there is of another shape")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
