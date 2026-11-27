#!/usr/bin/env python3
"""Hold each read outside its package that a Go legs test makes, and that this finds in its source,
to a //ci: line that names it.

    go_legs_reads.py

A test that reads a file outside its package's directory must say so in a //ci: line
(.github/scripts/go_legs_select.py says what each kind of line selects), or a change to that file
selects no package and the test does not run on it. This reads the Go files of every legs package
that a test runs from its own directory - its _test.go files, and a non-test file that climbs (below)
or anchors a path on its own source with runtime.Caller - and finds in them three kinds of read:

  * a path built out of literals that climbs out of the file's directory: a literal starting `../`;
    a filepath.Join or path.Join whose first arguments are literals, the first `..` or starting with
    `../`; a bare `..` a walk or a directory read starts at, or a list of paths to walk holds;
  * a walk or a directory read (filepath.WalkDir, filepath.Walk, os.ReadDir, os.DirFS,
    filepath.Glob), and a file a filepath.Join of something and then literals names, each traced
    back to where its path starts: a literal; a variable, through each value the file assigns it; a
    call of a climb. A climb is a function that starts at the working directory (os.Getwd,
    filepath.Abs(".")) or at its own source (runtime.Caller), steps up with filepath.Dir and stops
    at a go.mod, go.work, .git, .jj or .github; its root is the nearest directory above the package
    that holds what it stops at. A start traced to nowhere (a parameter, another call) is the root of
    each climb of the same file, in a file that climbs; a temporary directory is nothing of the
    repository. In a walk's start, a part this cannot read (a variable, a call) puts the path
    anywhere below where it had got to, and a literal after it makes a glob of it. In a file a Join
    reads, the later parts are resolved the same way (a constant from any file of the package), and
    one that cannot be (a field, a list in a struct) stands for each path literal of the package's
    own test files that no comparison holds a value to;
  * an envoy command a test builds through testbuild.EnvoyBinary, which runs go build in
    packages/envoy (`packages/daemon/internal/testbuild/testbuild.go#envoyModule`).

Each read must be named for the package:
  * a file: a change to it selects the package;
  * a tree above the package (where `..` or a climb leads, a module root, the repository) and
    anything read from a climb's root: every file of it the read can reach already selects the
    package, or a //ci: glob covers it. A glob covers a tree when the directory part of its literal
    start (up to the `/` before its first wildcard) is the tree's directory or above it, it goes on
    with `**` (for a read of one directory alone, it may name that directory's own files instead),
    and it names at least one file of the tree. A //ci: line that names something else outside the
    package does not name the tree;
  * a directory, or the files a glob in it matches, beside or below the package: as above, or the
    package's test binary compiles that directory, a //ci:builds line builds it, or a //ci: glob of
    the test binary lies inside it;
  * a built command: a //ci:builds line of the package builds its directory.
A //ci: line of a test file names reads for its own package's test binary alone. A read a non-test
file makes is made in the test binary of every package that imports it, so only a //ci:reads line of
a non-test file of its own package, which each of those compiles, names it.

A read in a function the legs never run is not held: one no test of the legs' build reaches, through
the names the package's own files use (a test the legs step's -skip leaves to another job, a test file
a build tag of the legs keeps out). A name used outside any function keeps what it names running.

It exits 1 naming each read no line names, with the line to add. A read built any other way this
does not see. The ci-filters job runs it beside check-ci-filters.sh, whose rule 9 holds each //ci:
line to the files it names. Its tests are in go-legs-select.test.sh.
"""

from __future__ import annotations

import itertools
import json
import math
import os
import re
import sys
from dataclasses import dataclass
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import go_legs  # noqa: E402
from ci_filter_coverage import REPO  # noqa: E402
from go_legs_select import Reads, Selector, directive_words, head_text, legs_document, matches, read_set  # noqa: E402

# A Go string literal: interpreted or raw.
STRING = re.compile(r'"(?:[^"\\\n]|\\.)*"|`[^`]*`')
JOIN = re.compile(r"\b(?:filepath|path)\.Join\(")
WILDCARD = re.compile(r"[*?\[]")
# A bare `..` (or `../..`) a walk or a directory read starts at, and whether it reads the whole tree
# below it or one directory: the first argument of such a call, or an element of a []string literal
# of plain paths, one of them a `../` path (a list of paths to walk).
WALK_ROOT = re.compile(r'\b(filepath\.WalkDir|filepath\.Walk|os\.ReadDir|os\.DirFS)\(\s*"(\.\.(?:/\.\.)*)"')
ROOT_LIST = re.compile(r"\[\]string\{([^{}]*)\}")
UP_ONLY = re.compile(r"\.\.(?:/\.\.)*")
PATH_LIKE = re.compile(r"^(?:\.\.?(?:/.*)?|[^\s]*/[^\s]*)$")
# A walk or a directory read, by the function it calls; those that read the tree below their start.
WALK = re.compile(r"\b(filepath\.WalkDir|filepath\.Walk|os\.ReadDir|os\.DirFS|filepath\.Glob)\(")
DEEP = {"filepath.WalkDir", "filepath.Walk", "os.DirFS"}
# A climb: where it starts, and what it stops at.
CLIMB_START = re.compile(r'\bos\.Getwd\(\)|\bfilepath\.Abs\(\s*"\."\s*\)|\b\w*runtime\.Caller\(')
ANCHORS = ("go.mod", "go.work", ".git", ".github", ".jj")
FUNC = re.compile(r"^func\s+(?:\([^)]*\)\s*)?(\w+)", re.M)
ENTRY = re.compile(r"^(?:Test|Fuzz|Example|Benchmark)")
NAME = re.compile(r"\b[A-Za-z_]\w*\b")
# A string constant of the file, as a path a Join may take: a top-level `const name = "..."`, or an
# entry of a top-level `const (...)` block. An assignment inside a function is no constant: it can
# be a parameter's or a variable's, which only its own function's assignments and callers say.
CONST_LINE = re.compile(r'^const\s+(\w+)(?:\s+string)?\s*=\s*("(?:[^"\\\n]|\\.)*"|`[^`]*`)\s*(?://.*)?$', re.M)
CONST_BLOCK = re.compile(r"^const\s*\((.*?)^\)", re.M | re.S)
CONST_ENTRY = re.compile(r'^\s*(\w+)(?:\s+string)?\s*=\s*("(?:[^"\\\n]|\\.)*"|`[^`]*`)\s*(?://.*)?$', re.M)
# A literal a comparison holds a value to (`x != "a/b"`, `"a/b" == x`): a value checked, not a path read.
COMPARED = re.compile(r"[!=]=\s*$")
COMPARED_AFTER = re.compile(r"\s*[!=]=")
# A wrapper that leaves where a path is as it is.
UNWRAP = re.compile(r"^(?:filepath\.FromSlash|filepath\.Clean|filepath\.ToSlash|filepath\.Abs|path\.Clean)\((.*)\)$",
                    re.S)
TEMPORARY = re.compile(r"\bTempDir\(|\bMkdirTemp\(")
ENVOY_BINARY = re.compile(r'\btestbuild\.EnvoyBinary\(\s*\w+\s*,\s*"(\./[^"]*)"\s*\)')
ENVOY_MODULE = "packages/envoy"
# The most paths one Join is traced to: past it, the path is anywhere below its start.
PATHS = 64
# A package-level var whose value is a function literal: what that names runs only when the var is
# used.
LAZY_VAR = re.compile(r"^var\s+(\w+)\b")


@dataclass
class Names:
    """What names a read for one package: for a test file, what selects the package (its test
    binary's Reads) and every glob of its test binary's //ci: lines; for a non-test file, the globs
    of its package's non-test //ci:reads and reads-set lines alone."""

    own_dir: str
    reads: Reads | None
    globs: list
    package_dirs: set

    def selects(self, path: str) -> bool:
        if self.reads is not None:
            return self.reads.selects(path, self.own_dir, self.package_dirs) is not None
        return any(matches(glob, but, path) for glob, but in self.globs)

    def holds(self, directory: str) -> bool:
        """Whether the test binary compiles or builds `directory`, or a //ci: glob lies inside it:
        how a directory beside the package is named."""
        if self.reads is not None and (directory in self.reads.dirs or directory in self.reads.built):
            return True
        return any(inside(glob, directory) for glob, _ in self.globs)


@dataclass(frozen=True)
class Origin:
    """Where a path a file builds starts: a directory (`root`, repository-relative), or nowhere known
    (`root` None: a parameter, a call); whether that directory is a climb's root; the literal path it
    goes on to below that (`tail`, a glob when it goes on past a part this cannot read); and whether
    it ends anywhere below (`below`)."""

    root: str | None
    climbed: bool = False
    tail: str = ""
    below: bool = False


def unquote(literal: str) -> str:
    if literal.startswith("`"):
        return literal[1:-1]
    try:
        return json.loads(literal)
    except ValueError:
        return literal[1:-1]


def call_arguments(text: str, start: int) -> list:
    """The arguments of the call whose argument list starts at `start`, each as its text, split at
    the commas outside brackets and literals."""
    depth, i, arguments, current = 0, start, [], []
    while i < len(text):
        c = text[i]
        literal = STRING.match(text, i) if c in "\"`" else None
        if literal:
            current.append(literal.group(0))
            i = literal.end()
            continue
        if c in "([{":
            depth += 1
        elif c in ")]}":
            if depth == 0:
                arguments.append("".join(current).strip())
                return arguments
            depth -= 1
        elif c == "," and depth == 0:
            arguments.append("".join(current).strip())
            current = []
            i += 1
            continue
        current.append(c)
        i += 1
    return arguments


def line_of(text: str, at: int) -> int:
    return text.count("\n", 0, at) + 1


def climbing_paths(text: str) -> list:
    """(line, path, how it is read) for each path a Go file builds out of literals that climbs out of
    its directory: a literal starting `../` (not `../` alone) and a filepath.Join or path.Join whose
    first arguments are literals, the first `..` or starting with `../`, joined, each read in a way
    not known (None); and a bare `..` a walk or a directory read starts at (WALK_ROOT, ROOT_LIST),
    read whole ("deep") or one directory of it ("one")."""
    found = []
    for call in JOIN.finditer(text):
        parts = []
        for argument in call_arguments(text, call.end()):
            if not STRING.fullmatch(argument):
                break
            parts.append(unquote(argument))
        if parts and (parts[0] == ".." or parts[0].startswith("../")):
            found.append((line_of(text, call.start()), "/".join(parts), None))
    for literal in STRING.finditer(text):
        value = unquote(literal.group(0))
        if value.startswith("../") and value != "../" and not re.search(r"\s", value):
            found.append((line_of(text, literal.start()), value, None))
    for call in WALK_ROOT.finditer(text):
        found.append((line_of(text, call.start()), call.group(2), "deep" if call.group(1) in DEEP else "one"))
    for roots in ROOT_LIST.finditer(text):
        written = [e.strip() for e in roots.group(1).split(",") if e.strip()]
        if not written or not all(STRING.fullmatch(e) for e in written):
            continue
        elements = [unquote(e) for e in written]
        if all(PATH_LIKE.match(e) for e in elements) and any(e.startswith("../") for e in elements):
            found += [(line_of(text, roots.start()), e, "deep") for e in elements if UP_ONLY.fullmatch(e)]
    return found


def code(text: str) -> str:
    """A Go file's text with each line that is a comment alone blanked, so a name a comment mentions
    is no use of it; lines keep their numbers."""
    return "\n".join("" if line.lstrip().startswith("//") else line for line in text.split("\n"))


def functions(text: str) -> list:
    """(first line, last line, name) of each top-level func of a gofmt'd file: its `func` line alone
    when that line closes every brace it opens, else from it to the first line after it that is `}`
    alone."""
    found = []
    for m in FUNC.finditer(text):
        first = line_of(text, m.start())
        newline = text.find("\n", m.start())
        line = text[m.start():newline if newline >= 0 else len(text)]
        if "{" in line and line.count("{") == line.count("}"):
            found.append((first, first, m.group(1)))
            continue
        end = text.find("\n}", m.start())
        found.append((first, line_of(text, end + 1) if end >= 0 else line_of(text, len(text)), m.group(1)))
    return found


def constants(text: str) -> dict:
    found = {m.group(1): unquote(m.group(2)) for m in CONST_LINE.finditer(text)}
    for block in CONST_BLOCK.finditer(text):
        found.update({m.group(1): unquote(m.group(2)) for m in CONST_ENTRY.finditer(block.group(1))})
    return found


def literal_value(argument: str, consts: dict) -> str | None:
    """The string a Join argument is, when it is a literal, a string constant of the file, or one of
    those under a wrapper that keeps a path's meaning; else None."""
    argument = argument.strip()
    wrapped = UNWRAP.match(argument)
    if wrapped:
        return literal_value(wrapped.group(1), consts)
    if STRING.fullmatch(argument):
        return unquote(argument)
    return consts.get(argument)


def body_of(text: str, first: int, last: int) -> str:
    return "\n".join(text.split("\n")[first - 1:last])


def climbs(text: str, own_dir: str) -> list:
    """(line, function name, root) for each climb of the file (the module docstring): its root is the
    nearest directory at or above `own_dir` that holds what the function stops at (the path a Join
    ending in an anchor builds, else each anchor it names), else the repository."""
    found = []
    consts = constants(text)
    for first, last, name in functions(text):
        body = body_of(text, first, last)
        began = CLIMB_START.search(body)
        if not began or "filepath.Dir(" not in body:
            continue
        joined = []
        for call in JOIN.finditer(body):
            values = [literal_value(a, consts) for a in call_arguments(body, call.end())[1:]]
            if values and None not in values and os.path.basename(values[-1]) in ANCHORS:
                joined.append("/".join(values))
        stops = joined or [a for a in ANCHORS if f'"{a}"' in body]
        if not stops:
            continue
        root, directory = ".", own_dir
        while directory:
            if any(os.path.lexists(REPO / directory / stop) for stop in stops):
                root = directory
                break
            directory = os.path.dirname(directory)
        found.append((first + line_of(body, began.start()) - 1, name, root))
    return found


def assignments(name: str, text: str) -> list:
    """What `text` assigns to `name`: the right-hand side of `name := x`, `name = x` and
    `name, err := x` (a []string literal whole, over as many lines as it takes), and each element of
    the []string literal a `for _, name := range` takes."""
    pattern = re.compile(rf"(?:^|[\s(;,]){re.escape(name)}\s*(?:,\s*\w+\s*)*:?=\s*([^\n]+)", re.M)
    composite = re.compile(rf"(?:^|[\s(;,]){re.escape(name)}\s*:?=\s*(\[\]string\{{[^{{}}]*\}})", re.M)
    found = [m.group(1).strip() for m in composite.finditer(text)]
    found += [m.group(1).strip() for m in pattern.finditer(text)
              if not m.group(1).startswith(("=", "range ", "[]string{"))]
    ranged = re.compile(rf"\bfor\s+(?:\w+\s*,\s*)?{re.escape(name)}\s*:=\s*range\s+\[\]string\{{([^{{}}]*)\}}")
    for loop in ranged.finditer(text):
        found += [e.strip() for e in loop.group(1).split(",") if e.strip()]
    return found


@dataclass
class Scope:
    """The package's own files a trace reads: each file's text, its functions, its constants, and
    the package's climb functions (`climbers`, each name to its root)."""

    own_dir: str
    texts: dict
    climbers: dict

    def __post_init__(self):
        self.spans = {source: functions(text) for source, text in self.texts.items()}
        # A Go constant is the package's, not its file's: one file reads another's.
        package = {}
        for text in self.texts.values():
            package.update(constants(text))
        self.consts = {source: package for source in self.texts}
        # The string literals of the package's own test files that could be a path, but those a
        # comparison holds a value to: what a Join part this cannot follow may be. A non-test file's
        # literals are left out, since what it names (a route's docs, a product path) is no read
        # of a test's; a constant of it that a test reads resolves through `consts` instead.
        self.path_literals = {unquote(m.group(0)) for source, text in self.texts.items()
                              if source.endswith("_test.go") for m in STRING.finditer(text)
                              if not COMPARED.search(text[max(0, m.start() - 4):m.start()])
                              and not COMPARED_AFTER.match(text, m.end())}
        self.path_literals = {v for v in self.path_literals if "/" in v and not re.search(r"\s", v)}

    def function_at(self, source: str, line: int):
        return next((span for span in self.spans[source] if span[0] <= line <= span[1]), None)

    def region(self, source: str, span) -> str:
        return self.texts[source] if span is None else body_of(self.texts[source], span[0], span[1])

    def parameters(self, source: str, span) -> list:
        """(name, whether it is variadic) of each parameter of the function `span` is, in order."""
        text = self.texts[source]
        lines = text.split("\n")
        start = sum(len(line) + 1 for line in lines[:span[0] - 1])
        signature = re.compile(rf"\b{re.escape(span[2])}\(").search(text, start)
        if signature is None:
            return []
        return [(item.split()[0], "..." in item) if item.split() else ("", False)
                for item in call_arguments(text, signature.end())]

    def passed(self, name: str, source: str, span) -> list | None:
        """(file, the function it is in, an argument) for each argument a call of the function `span`
        passes the parameter `name` (each from its place on, for a variadic one), or None when
        `name` is no parameter of it."""
        parameters = self.parameters(source, span)
        index = next((i for i, (parameter, _) in enumerate(parameters) if parameter == name), None)
        if index is None:
            return None
        variadic = parameters[index][1]
        found = []
        for other, text in self.texts.items():
            for call in re.finditer(rf"\b{re.escape(span[2])}\(", text):
                line = line_of(text, call.start())
                if other == source and line == span[0]:
                    continue
                arguments = call_arguments(text, call.end())
                at = self.function_at(other, line)
                found += [(other, at, a) for a in (arguments[index:] if variadic else arguments[index:index + 1])]
        return found


def values_of(expression: str, scope: Scope, source: str, span, depth: int = 0) -> list | None:
    """The strings `expression` can be, where the package's own files say, or None: a literal or a
    string constant; a variable each value its function assigns it, or a range over a []string
    literal or over another such variable gives it; a parameter each value a call passes it."""
    expression = expression.strip()
    wrapped = UNWRAP.match(expression)
    if wrapped:
        return values_of(wrapped.group(1), scope, source, span, depth)
    value = literal_value(expression, scope.consts[source])
    if value is not None:
        return [value]
    composite = re.fullmatch(r"\[\]string\{([^{}]*)\}", expression)
    if composite:
        listed = re.sub(r"//[^\n]*", "", composite.group(1)).split(",")
        elements = [literal_value(e, scope.consts[source]) for e in listed if e.strip()]
        return None if None in elements else elements
    if not re.fullmatch(r"\w+", expression) or depth >= 4:
        return None
    region = scope.region(source, span)
    sources = [(source, span, rhs) for rhs in assignments(expression, region)]
    ranged = re.compile(rf"\bfor\s+(?:\w+\s*,\s*)?{re.escape(expression)}\s*:=\s*range\s+(\w+)\s*\{{")
    sources += [(source, span, over) for over in ranged.findall(region)]
    if not sources and span is not None:
        sources = scope.passed(expression, source, span)
        if sources is None:
            return values_of(expression, scope, source, None, depth + 1)
    found = []
    for other, at, rhs in sources or []:
        values = values_of(rhs, scope, other, at, depth + 1)
        if values is None:
            return None
        found += values
    return found or None


def trace(expression: str, scope: Scope, source: str, span, depth: int = 0) -> list:
    """Each Origin of the path `expression` in the function `span` of `source` (the module
    docstring): a literal or a Join of literals is its own, read from the package's directory; a
    call of a climb function of the package its root; a Join of something and then parts, that
    something's, with the parts below it; a variable each value its function assigns it, a
    parameter each value a call of its function passes, or else each value the file assigns it;
    anything else nowhere known. A temporary directory has none."""
    expression = expression.strip()
    consts = scope.consts[source]
    wrapped = UNWRAP.match(expression)
    if wrapped:
        return trace(wrapped.group(1), scope, source, span, depth)
    if TEMPORARY.search(expression):
        return []
    value = literal_value(expression, consts)
    join = JOIN.match(expression)
    if join and call_arguments(expression, join.end()):
        arguments = call_arguments(expression, join.end())
        values = [values_of(a, scope, source, span, depth) for a in arguments]
        if None not in values and math.prod(len(v) for v in values) <= PATHS:
            return [Origin(os.path.normpath(os.path.join(scope.own_dir, *combination)))
                    for combination in itertools.product(*values)]
        parts = values[1:]
        unread = [i for i, part in enumerate(parts) if part is None]
        # The parts after the last one this cannot read; before it, the path is anywhere below.
        after = parts[unread[-1] + 1:] if unread else parts
        if math.prod(len(v) for v in after) > PATHS:
            return [Origin(o.root, o.climbed, o.tail, True) for o in trace(arguments[0], scope, source, span, depth)]
        tails = ["/".join(c) for c in itertools.product(*after)] if after else [""]
        found = []
        for o in trace(arguments[0], scope, source, span, depth):
            for tail in tails:
                if not unread:
                    found.append(Origin(o.root, o.climbed, os.path.normpath(os.path.join(o.tail, tail)), o.below))
                elif tail:
                    found.append(Origin(o.root, o.climbed, os.path.join(o.tail, "**", tail)))
                else:
                    found.append(Origin(o.root, o.climbed, o.tail, True))
        return found
    if value is not None:
        return [Origin(os.path.normpath(os.path.join(scope.own_dir, value)))]
    call = re.match(r"(\w+)\(", expression)
    if call and call.group(1) in scope.climbers:
        return [Origin(scope.climbers[call.group(1)], True)]
    if not re.fullmatch(r"\w+", expression) or depth >= 4:
        return [Origin(None)]
    assigned = assignments(expression, scope.region(source, span))
    if assigned:
        return [o for rhs in assigned for o in trace(rhs, scope, source, span, depth + 1)]
    if span is not None:
        passed = scope.passed(expression, source, span)
        if passed is not None:
            return [o for other, at, argument in passed for o in trace(argument, scope, other, at, depth + 1)] \
                if passed else [Origin(None)]
    if span is not None:
        return trace(expression, scope, source, None, depth + 1)
    return [Origin(None)]


def inside(glob: str, directory: str) -> bool:
    """Whether a //ci: line's glob can name files inside `directory`: its literal part (up to its
    first wildcard) lies in the directory, or above it."""
    wildcard = WILDCARD.search(glob)
    literal = glob[:wildcard.start()] if wildcard else glob
    return literal == directory or literal.startswith(directory + "/") or (directory + "/").startswith(literal)


def covers(glob: str, directory: str, deep: bool) -> bool:
    """Whether `glob` reaches every file of the tree at `directory` (the whole tree, or with `deep`
    False its own files): the directory part of its literal start is the directory or above it, and
    it goes on with `**`, or, for one directory, names the directory's own files."""
    wildcard = WILDCARD.search(glob)
    if wildcard is None:
        return False
    literal = glob[:wildcard.start()]
    folder = literal[:literal.rfind("/") + 1]
    rest = glob[len(folder):]
    own = "" if directory == "." else directory + "/"
    if not own.startswith(folder):
        return False
    return rest.startswith("**") or (not deep and folder == own and "/" not in rest)


def tree_files(directory: str, deep: bool, files: set) -> list:
    prefix = "" if directory == "." else directory + "/"
    return sorted(f for f in files if f.startswith(prefix) and (deep or "/" not in f[len(prefix):]))


def unnamed_tree(names: Names, directory: str, deep: bool, strict: bool, files: set,
                 read: list | None = None) -> str | None:
    """Why a read of the tree at `directory` (or of `read`, the files a glob in it matches) is not
    named, or None. `strict` for a tree above the package or reached from a climb, which only what
    selects every file of it or a glob that covers it names."""
    read = tree_files(directory, deep, files) if read is None else read
    unselected = [f for f in read if not names.selects(f)]
    if not unselected or any(covers(glob, directory, deep) and any(matches(glob, but, f) for f in read)
                             for glob, but in names.globs):
        return None
    if not strict and names.holds(directory):
        return None
    shown = "" if directory == "." else directory + "/"
    suggestion = f"{shown}**" if deep else f"{shown}*"
    return (f"and a change to {unselected[0]}{f' or {len(unselected) - 1} more' if len(unselected) > 1 else ''} "
            f"selects no package for it: add `//ci:reads {suggestion} <why>`, narrowed to what it reads, "
            f"with a glob that covers the tree")


def unnamed_path(names: Names, target: str, deep: bool, strict: bool, files: set, dirs: set) -> str | None:
    """Why a read of `target` (a file, a directory, or a glob) is not named, or None; nothing the
    repository does not have can be."""
    wildcard = WILDCARD.search(target)
    if wildcard:
        folder = target[:wildcard.start()]
        folder = folder[:folder.rfind("/")] or "."
        read = sorted(f for f in files if matches(target, (), f))
        return unnamed_tree(names, folder, "**" in target, strict, files, read) if read else None
    if target in files:
        if names.selects(target):
            return None
        return f"and a change to it selects no package for it: add `//ci:reads {target} <why>`"
    if target == "." or target in dirs:
        return unnamed_tree(names, target, deep, strict, files)
    return None


def live_names(texts: dict, skip: str) -> tuple:
    """(the names of the package's own functions the legs' test binary can run, each file's
    functions as (first line, last line, name)). A test the legs' -skip leaves out runs nothing;
    every other test, fuzz test, example and benchmark, TestMain, init, and each name used outside
    any function runs, and so does every function a running one names."""
    spans = {source: functions(text) for source, text in texts.items()}
    defined = {name for found in spans.values() for _, _, name in found}
    bodies: dict = {}
    roots: set = set()
    for source, text in texts.items():
        lines = text.split("\n")
        spans[source] = spans[source] + lazy_vars(lines)
        defined |= {name for _, _, name in spans[source]}
    for source, text in texts.items():
        lines = text.split("\n")
        covered = set()
        for first, last, name in spans[source]:
            bodies.setdefault(name, []).append("\n".join(lines[first - 1:last]))
            covered |= set(range(first, last + 1))
            if name in ("TestMain", "init") or (ENTRY.match(name) and not (skip and re.search(skip, name))):
                roots.add(name)
        outside_functions = "\n".join(line for number, line in enumerate(lines, 1) if number not in covered)
        roots |= set(NAME.findall(outside_functions)) & defined
    live, queue = set(roots), list(roots)
    while queue:
        for body in bodies.get(queue.pop(), []):
            for used in set(NAME.findall(body)) & defined:
                if used not in live:
                    live.add(used)
                    queue.append(used)
    return live, spans


def lazy_vars(lines: list) -> list:
    """(first line, last line, name) of each package-level `var` whose value holds a function literal,
    from its line to the line before the next one at the margin: what the literal names runs only
    when something uses the var. Any other var's value runs at init, and is no function's."""
    found = []
    for number, line in enumerate(lines, 1):
        var = LAZY_VAR.match(line)
        if not var:
            continue
        last = number
        while last < len(lines) and (not lines[last] or lines[last][0] in " \t)}"):
            last += 1
        if "func(" in "\n".join(lines[number - 1:last]):
            found.append((number, last, var.group(1)))
    return found


class Checker:
    """The reads each legs package's source makes outside it, and those no //ci: line names."""

    def __init__(self, selector: Selector):
        self.selector = selector
        self.files = selector.files
        self.dirs = {d for f in self.files for d in parents(f)}
        self.problems: dict = {}

    def check(self) -> list:
        for job, found in self.selector.jobs.items():
            skip = self.selector.legs_jobs[job].skip
            for own_dir, reads in found.reads.values():
                self.check_package(own_dir, reads, skip)
        return [self.problems[key] for key in sorted(self.problems)]

    def check_package(self, own_dir: str, reads: Reads, skip: str) -> None:
        own = [s for s in sorted(reads.sources) if os.path.dirname(s) == own_dir]
        tests = [s for s in own if s.endswith("_test.go")]
        exported = []
        for source in own:
            if source in tests:
                continue
            for line in directive_words(source):
                if line.kind == "reads":
                    exported.append((line.globs[0], tuple(g[1:] for g in line.globs[1:])))
                elif line.kind == "reads-set":
                    exported += read_set(line.name, line.why)
        texts = {s: code(head_text(s) or "") for s in own}
        live, spans = live_names({s: texts[s] for s in tests}, skip)
        # A climb function of the package, by name: a file that calls it reads from its root.
        climbers = {name: root for source in own for _, name, root in climbs(texts[source], own_dir)}
        # A test traces a path through every file of its test binary's own; a non-test file through
        # the package's non-test files alone, all an importer's test binary compiles of it.
        test_scope = Scope(own_dir, texts, climbers)
        source_scope = Scope(own_dir, {s: t for s, t in texts.items() if s not in tests}, climbers)
        for source in own:
            text = texts[source]
            is_test = source in tests
            roots = sorted({root for _, _, root in climbs(text, own_dir)})
            if not is_test and not roots and "runtime.Caller" not in text:
                continue
            dead = [(first, last) for first, last, name in spans.get(source, []) if name not in live]
            check = Check(self, source, text, own_dir,
                          Names(own_dir, reads, reads.globs, self.selector.package_dirs) if is_test else
                          Names(own_dir, None, exported, self.selector.package_dirs),
                          f"after the package clause of a file of {own_dir}" if is_test else
                          f"after the package clause of {source} or another non-test file of {own_dir}, since every "
                          f"package that imports it makes the read", dead, test_scope if is_test else source_scope)
            check.literals()
            check.traced(roots)
            if is_test:
                check.builds(reads)


@dataclass
class Check:
    """The reads of one file, each held to what names it for its package (`names`), and reported
    with where its line goes (`where`); none in a function the legs never run (`dead`)."""

    checker: Checker
    source: str
    text: str
    own_dir: str
    names: Names
    where: str
    dead: list
    scope: Scope

    def say(self, line: int, what: str) -> None:
        if not any(first <= line <= last for first, last in self.dead):
            self.checker.problems.setdefault((self.source, line), f"{self.source}:{line}: {what}, {self.where}")

    def outside(self, target: str) -> bool:
        return not (target.startswith("..") or target == self.own_dir or target.startswith(self.own_dir + "/"))

    def above(self, target: str) -> bool:
        return target == "." or self.own_dir.startswith(target + "/")

    def literals(self) -> None:
        for line, written, how in climbing_paths(self.text):
            target = os.path.normpath(os.path.join(self.own_dir, written))
            if not self.outside(target) or (self.above(target) and how is None):
                continue  # where a path above starts: what is read from it is traced from its uses
            # A directory read in a way this cannot see (a parser's or a loader's directory read) is
            # read one directory deep; a walk that is seen is read as it walks.
            problem = unnamed_path(self.names, target, how == "deep", self.above(target), self.checker.files,
                                   self.checker.dirs)
            if problem:
                self.say(line, f"{'walks' if self.above(target) else 'reads'} {written} ({target}) {problem}")

    def traced(self, roots: list) -> None:
        """The reads traced from where their paths start: each file a Join of something and then
        further parts names, and each tree a walk or a directory read reads. A start traced to
        nowhere known is each root the file's own climbs end at (`roots`); in a file that climbs
        nowhere, nothing. A Join's later parts are resolved as a walk's start is (`values_of`); a
        part that cannot be resolved stands for each path literal of the package's own files (a
        list in a struct, a field), so a read of a repository file named anywhere in the package
        is held even where the code between the literal and the read cannot be followed."""
        consts = self.scope.consts[self.source]

        def origins(expression: str, at: int) -> list:
            line = line_of(self.text, at)
            return trace(expression, self.scope, self.source, self.scope.function_at(self.source, line))

        for call in JOIN.finditer(self.text):
            arguments = call_arguments(self.text, call.end())
            if len(arguments) < 2 or literal_value(arguments[0], consts) is not None:
                continue  # a Join of literals from the package's own directory: climbing_paths reads it
            span = self.scope.function_at(self.source, line_of(self.text, call.start()))
            parts = [values_of(a, self.scope, self.source, span) for a in arguments[1:]]
            if None in parts or math.prod(len(p) for p in parts) > PATHS:
                tails = sorted(self.scope.path_literals)
            else:
                tails = ["/".join(c) for c in itertools.product(*parts)]
            tails = [t for t in tails if not WILDCARD.search(t) and os.path.basename(t) not in ANCHORS]
            for origin in origins(arguments[0], call.start()):
                for root, climbed in self.starts(origin, roots):
                    for tail in tails:
                        if origin.below or WILDCARD.search(origin.tail):
                            # Below a part this cannot read: the read is the glob of every file the
                            # path can name, each held as the file it is.
                            pattern = os.path.join(root, origin.tail, "**" if origin.below else "", tail)
                            pattern = os.path.normpath(pattern).removeprefix("./")
                            targets = sorted(f for f in self.checker.files if matches(pattern, (), f))
                        else:
                            targets = [os.path.normpath(os.path.join(root, origin.tail, tail))]
                        found = next((t for t in targets if t in self.checker.files and self.outside(t)
                                      and unnamed_path(self.names, t, True, True, self.checker.files,
                                                       self.checker.dirs)), None)
                        if found:
                            problem = unnamed_path(self.names, found, True, True, self.checker.files,
                                                   self.checker.dirs)
                            self.say(line_of(self.text, call.start()), f"reads {found} {problem}")
                            break
        for call in WALK.finditer(self.text):
            arguments = call_arguments(self.text, call.end())
            walks = call.group(1) in DEEP
            for origin in (origins(arguments[0], call.start()) if arguments else []):
                deep = walks or origin.below
                for root, climbed in self.starts(origin, roots):
                    target = os.path.normpath(os.path.join(root, origin.tail))
                    pattern = WILDCARD.search(target) is not None
                    if not self.outside(target) or not (pattern or target == "." or target in self.checker.dirs):
                        continue
                    strict = climbed or self.above(target)
                    problem = unnamed_path(self.names, target, deep or pattern, strict, self.checker.files,
                                           self.checker.dirs)
                    if problem:
                        self.say(line_of(self.text, call.start()),
                                 f"{'walks' if deep else 'reads'} {target}{' from a climb' if climbed else ''} {problem}")

    @staticmethod
    def starts(origin: Origin, roots: list) -> list:
        """(directory, whether a climb led there) for each place an Origin may start."""
        if origin.root is not None:
            return [(origin.root, origin.climbed)]
        return [(root, True) for root in roots]

    def builds(self, reads: Reads) -> None:
        for call in ENVOY_BINARY.finditer(self.text):
            directory = os.path.normpath(os.path.join(ENVOY_MODULE, call.group(1)))
            if directory in reads.built or directory in reads.dirs:
                continue
            self.say(line_of(self.text, call.start()),
                     f"builds {call.group(1)} in {ENVOY_MODULE} through testbuild.EnvoyBinary, and no //ci:builds line "
                     f"of its test binary builds it, so a change to that command selects no package for it: add "
                     f"`//ci:builds {ENVOY_MODULE} {call.group(1)} <why>`")


def parents(path: str):
    parent = os.path.dirname(path)
    while parent:
        yield parent
        parent = os.path.dirname(parent)


def main(argv: list) -> int:
    if argv:
        print("go_legs_reads.py: usage: go_legs_reads.py (no arguments; run from the repository's root)",
              file=sys.stderr)
        return 2
    problems = Checker(Selector(legs_document(head_text(go_legs.LEGS)))).check()
    for problem in problems:
        print(f"::error::{problem}", file=sys.stderr)
    print(f"go_legs_reads.py: {len(problems)} read(s) outside a package that no //ci: line names" if problems
          else "go_legs_reads.py: each read outside its package that this finds in a legs package's source - a `..` "
               "path, what a walk or a Join reads from where its path starts, a climb to a go.mod, go.work, .git, .jj or "
               ".github among them, an envoy command testbuild builds - is named by a //ci: line", file=sys.stderr)
    return 1 if problems else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
