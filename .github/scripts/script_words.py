"""Which declared scripts a workflow step's run: lines name, read as the shell reads each word, with
every value the step's context gives put in: the reader behind rule 4's script half, whose
paragraph in check-ci-filters.sh's header says what is read and that everything else is refused.

The reader refuses by default. Every value the repository gives that a step reads (an env:, a
called workflow's or an action's input, a matrix value, an action's directory) must evaluate to
literal text, through every value it reads in turn; and the step's own text may hold only the shell
syntax this reader models. Where either fails, reading raises Unreadable, naming what it could not
evaluate, and check_script_steps refuses the step with its one refusal. A value no file of the
repository gives (a step's or a job's output, a secret, a variable the runner sets, the result of
an expression function) cannot be known to any check that reads files: it stands for any text,
and named_scripts returns, for the same refusal, each construct of that kind whose literal parts
could make a declared script's name.

check-ci-filters.sh imports it through ci_filter_coverage.py, from the repository it checks,
running `PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=.github/scripts python3` from that repository's root.
Its functions read strings only: ci_filter_coverage.check_script_steps hands them a step's lines
(script_lines) and the values its context gives. Those are `values`, mapping ("env" | "inputs" |
"matrix" | "github", name) to a list of the texts the name may hold, each a str read in the same
`values`, a (text, values) pair read in values of its own (an input, read where its caller passes
it), or an Unevaluated, a value given in a form this reader does not evaluate.
"""

import os
import re

from shell_commands import EXPRESSION

# What a reference leaves in a form of its word: the repository's root, or a value this check
# cannot know, which may hold any characters, a `/` among them.
ROOT_MARK, WILD = "\x01", "\x02"
# The characters of a path, as the body of a character class.
PATH_CHARS = r"\w.@+/\-"
# The references a word can make: ${{ <expression> }}, ${X}, ${X<op><word>} and $X.
REF_TEXT = r"\$\{\{.*?\}\}|\$\{[^}]*\}|\$[A-Za-z_]\w*"
REF = re.compile(REF_TEXT)
SHELL_REF = re.compile(r"\$(?:\{([A-Za-z_]\w*)(?::?[-=+?](.*))?\}|([A-Za-z_]\w*))", re.S)
# The variable a ${...} expansion reads, whatever operator follows it (`${X%.sh}`) or comes
# before its name (a length, or an indirection).
EXPANDED_NAME = re.compile(r"\$\{[#!]?([A-Za-z_]\w*)")
WORKSPACE_REF = re.compile(r"\$\{\{\s*github\.workspace\s*\}\}|\$\{GITHUB_WORKSPACE\}|\$GITHUB_WORKSPACE")
# A shell glob's bracket expression, read whole: `[abc]`, `[!x]`, `[^x]`, `[[:alpha:]]`.
BRACKET = r"\[[!^]?\]?(?:\[:[a-z]+:\]|[^\]\s/'\"])*\]"
BRACKET_RE = re.compile(BRACKET)
POSIX_CLASS = re.compile(r"\[:([a-z]+):\]")
POSIX_CLASSES = {"alpha": "a-zA-Z", "digit": "0-9", "alnum": "a-zA-Z0-9", "upper": "A-Z", "lower": "a-z",
                 "xdigit": "0-9a-fA-F", "space": r"\s", "blank": r" \t", "punct": r"!-/:-@\[-`{-~",
                 "word": r"\w", "print": r" -~", "graph": r"!-~"}
# A shell word as this check reads it: references, bracket expressions and the characters a path
# or a glob can hold. Quotes are dropped first (dequote), since the shell joins `"$X"/y` into one
# word.
WORD = re.compile(rf"(?:{REF_TEXT}|{BRACKET}|[{PATH_CHARS}*?\[\]~{WILD}])+")
# The pieces of a word's form that can be a path: its characters, its bracket expressions and the
# two marks.
FORM_PIECE = re.compile(rf"(?:{BRACKET}|[{PATH_CHARS}*?\[\]~{ROOT_MARK}{WILD}])+")
# A run of characters that is no part of a path, which reads as any text where a construct's
# value is built from the text around it.
NOT_PATH = re.compile(rf"[^{PATH_CHARS}]+")
# A context value an expression reads, the same value read by a literal index (`env['X']`, which
# expression_parts writes as `env.X`), a context read whole or by a computed index, and a quoted
# literal an expression holds.
EXPR_NAME = re.compile(r"\b(env|inputs|matrix|github)\.([A-Za-z_][\w-]*)")
EXPR_INDEX = re.compile(r"\b(env|inputs|matrix|github)\[\s*'([A-Za-z_][\w-]*)'\s*\]")
EXPR_CONTEXT = re.compile(r"\b(env|inputs|matrix)\b(?!\.)")
EXPR_QUOTED = re.compile(r"'((?:[^']|'')*)'")
# Two shell constructs this check does not evaluate: a command substitution, which stands in its
# word as a value this check cannot know and whose own text is read as a line of its own, and a
# brace expansion, which stands in its word as such a value. A format() placeholder is such a
# value inside its literal.
COMMAND_SUBSTITUTION = re.compile(r"\$\((?!\()([^()]*)\)|`([^`]*)`")
BRACE_EXPANSION = re.compile(r"(?<!\$)\{[^{}\s$]*(?:,|\.\.)[^{}\s$]*\}")
FORMAT_PLACEHOLDER = re.compile(r"\{\d+\}")
GLOB_CHARS = re.compile(rf"[*?\[{WILD}]")
# A file name holding a literal character, outside a bracket expression, before its first dot:
# `buil?.sh`, `b*.sh`, `[!x]uild.sh`, not `*`, `*.sh`, `?`, `[0-9]*`, or an unknown value followed
# by `.sh`.
GLOB_STEM = re.compile(rf"^[^.]*?[^*?.\x00{WILD}]")
# A step that changes its directory, after which a relative path can lead from anywhere.
CHANGES_DIRECTORY = re.compile(r"(?:^|[;&|(\s])(?:cd|pushd)(?:\s|$)")
# The shell syntax bash reads in a value `${{ }}` pastes into a script, after the paste, which this
# reader does not read in a pasted value: a quote, a backslash, `$`, a backtick and a brace.
PASTE_SYNTAX = re.compile(r"""['"\\$`{}]""")
# Shell syntax of a step's own text this reader does not model, and which can change the words a
# step runs: `shopt`, which changes how bash expands words (globstar, extglob, nocaseglob), and a
# line that ends in a backslash right after a character, which a continuation joins to the next
# line's first word. A backslash inside a word is read in named_scripts.
SHOPT = re.compile(r"(?:^|[;&|(\s])shopt(?:\s|$)")
CONTINUED_WORD = re.compile(r"[^\s\\]\\$")
ESCAPED_WORD = re.compile(rf"[{PATH_CHARS}\\]*\\[{PATH_CHARS}\\]*")
# How many forms one word may take, and how long a chain of values may be, before reading raises
# Unreadable: a chain is each value a reference reads, the values those read, and so on, across
# env:, with: and matrix values alike, so an env: value that reads its own name is one of no end.
MAX_FORMS = 4096
MAX_DEPTH = 8
# The matrix name every matrix read reads when a job's matrix, or its include:, is written as one
# expression, so that no matrix name of the job can be listed.
ANY_MATRIX = ("matrix", "*")


class Unreadable(Exception):
    """A step's text, or a value it reads, that this check cannot evaluate to literal text; its one
    argument says what, as the refusal names it."""


class TooManyForms(Unreadable):
    """A word, or a value one reads, whose references take more than MAX_FORMS forms."""

    def __init__(self):
        super().__init__(f"a word whose references take more than {MAX_FORMS} forms")


class Unevaluated:
    """A value the repository gives in a form this reader does not evaluate: a matrix, an include:
    or a matrix name written as one expression, or a matrix value that is a list or a mapping.
    Reading it raises Unreadable, naming `what`."""

    def __init__(self, what: str):
        self.what = what


def dequote(line: str) -> str:
    """A run: line with its quotes dropped outside ${{ }} expressions, where a quote is a literal."""
    out, last = [], 0
    for m in EXPRESSION.finditer(line):
        out += [re.sub("[\"']", "", line[last:m.start()]), m.group()]
        last = m.end()
    return "".join(out + [re.sub("[\"']", "", line[last:])])


def given(values: dict, key) -> bool:
    """Whether the step's context gives a value for a name (ANY_MATRIX gives every matrix name)."""
    return key in values or (key[0] == "matrix" and ANY_MATRIX in values)


def held(values: dict, key) -> list:
    """The texts a name may hold, each with the values it is read in: a pair's own, else `values`.
    Raises Unreadable for a value the repository gives in a form this reader does not evaluate."""
    items = values.get(ANY_MATRIX if key[0] == "matrix" and ANY_MATRIX in values else key, ())
    out = []
    for item in items:
        if isinstance(item, Unevaluated):
            raise Unreadable(item.what)
        out.append(item if isinstance(item, tuple) else (item, values))
    return out


def expression_parts(expression: str) -> tuple:
    """A ${{ }} expression's body with each context value it reads by a literal index written as the
    dotted name it reads (`env['X']` as `env.X`) and each quoted literal blanked out, and those
    literals, unquoted."""
    body = EXPR_INDEX.sub(r"\1.\2", expression)[3:-2]
    return EXPR_QUOTED.sub(" ", body), [q.replace("''", "'") for q in EXPR_QUOTED.findall(body)]


def opaque(unquoted: str) -> bool:
    """Whether an expression body (expression_parts) holds more than context values, quoted literals
    and the operators between them: a function (`format`, `join`, `fromJSON`), a computed index, or
    a context this check does not read, whose value it cannot work out."""
    return bool(re.search(r"[A-Za-z_\[]", EXPR_NAME.sub(" ", unquoted)))


def read_keys(ref: str, values: dict) -> list:
    """The names a reference reads: each context value an expression names, and every name of a
    context it reads whole or by a computed index; the variable a ${...} or $X expansion reads.
    The workspace is no name of `values`."""
    if WORKSPACE_REF.fullmatch(ref):
        return []
    if ref.startswith("${{"):
        unquoted, _ = expression_parts(ref)
        contexts = EXPR_CONTEXT.findall(unquoted)
        return EXPR_NAME.findall(unquoted) + [key for key in values if key[0] in contexts]
    shell = SHELL_REF.fullmatch(ref)
    expanded = EXPANDED_NAME.match(ref)
    name = (shell.group(1) or shell.group(3)) if shell else expanded.group(1) if expanded else None
    return [("env", name)] if name else []


def text_forms(text: str, values: dict, depth: int = 0, pasted: bool = True) -> list:
    """Every string `text` may be once its references are put in, each reference standing for each
    of its forms (ref_forms) in turn: the product, since one word can join several (`${{ matrix.dir
    }}/${{ matrix.script }}`, `"$stem.sh"`). `pasted` says bash reads the text as script, so that
    a value `${{ }}` pastes into it must hold no shell syntax (ref_forms). Raises TooManyForms past
    MAX_FORMS, and Unreadable as ref_forms does."""
    forms, last = [""], 0
    for m in REF.finditer(text):
        literal = text[last:m.start()]
        forms = [f + literal + a for f in forms for a in dict.fromkeys(ref_forms(m.group(), values, depth, pasted))]
        if len(forms) > MAX_FORMS:
            raise TooManyForms
        last = m.end()
    return list(dict.fromkeys(f + text[last:] for f in forms))


def key_forms(values: dict, key, depth: int, pasted: bool) -> list:
    """The forms of each text a name holds, each read in its own values: ROOT_MARK for the
    workspace, and WILD for a name `values` does not give. When `pasted`, the text is pasted into a
    script, and bash reads it after the paste: text holding PASTE_SYNTAX outside the ${{ }}
    expressions GitHub evaluates first raises Unreadable."""
    if key == ("github", "workspace"):
        return [ROOT_MARK]
    if not given(values, key):
        return [WILD]
    forms = []
    for text, context in held(values, key):
        if pasted and PASTE_SYNTAX.search(EXPRESSION.sub("", text)):
            raise Unreadable(f"`{text}`, the value of {key[0]}.{key[1]}, which a ${{{{ }}}} pastes into its script and "
                             f"which holds shell syntax bash reads after the paste (a quote, a backslash, `$`, a "
                             f"backtick or a brace)")
        forms += text_forms(text, context, depth + 1, pasted)
    return forms


def ref_forms(ref: str, values: dict, depth: int, pasted: bool = True) -> list:
    """What one reference may stand for: ROOT_MARK for the workspace; each form of each value an
    env:, an input, a matrix name or the action's directory holds (key_forms), so a value that names
    another is followed; each quoted literal an expression holds, and a shell reference's default
    (`${X:-x.sh}`); and WILD for a value this check cannot know. `${{ env.X || 'y' }}` stands for
    X's values and `y`, and `env['X']` reads as `env.X`. What a ${{ }} expression yields is pasted
    when its text is (`pasted`), and what $X expands to is not, since bash does not read a
    variable's value again. An expression this check cannot work out (opaque) stands for WILD too,
    its arguments read as values it is not pasted with; unevaluable_constructs says when its
    literals refuse the step. Raises Unreadable for a chain of values longer than MAX_DEPTH, for a
    pasted value or literal holding PASTE_SYNTAX, and for a ${...} expansion with an operator
    SHELL_REF does not read (`${X%.sh}`, `${X/a/b}`) of a value the repository gives; of a value
    no file gives, such an expansion stands for WILD."""
    if WORKSPACE_REF.fullmatch(ref):
        return [ROOT_MARK]
    if depth >= MAX_DEPTH:
        raise Unreadable(f"a chain of more than {MAX_DEPTH} values, through `{ref}`")
    if ref.startswith("${{"):
        unquoted, literals = expression_parts(ref)
        evaluated = not opaque(unquoted)
        forms = [form for key in EXPR_NAME.findall(unquoted)
                 for form in key_forms(values, key, depth, pasted and evaluated)]
        for literal in literals:
            if pasted and evaluated and PASTE_SYNTAX.search(literal):
                raise Unreadable(f"`{literal}`, a literal `{ref}` pastes into its script, which holds shell syntax "
                                 f"bash reads after the paste (a quote, a backslash, `$`, a backtick or a brace)")
        forms += literals
        if not evaluated or not forms:
            forms.append(WILD)
        return forms
    m = SHELL_REF.fullmatch(ref)
    if m is None:
        expanded = EXPANDED_NAME.match(ref)
        if expanded and given(values, ("env", expanded.group(1))):
            raise Unreadable(f"`{ref}`, an expansion with an operator this check does not evaluate, of a value the "
                             f"repository gives")
        return [WILD]
    forms = key_forms(values, ("env", m.group(1) or m.group(3)), depth, False)
    if m.group(2) is not None:
        forms += text_forms(m.group(2), values, depth + 1, pasted)
    return forms


def reached_texts(text: str, values: dict, depth: int = 0, seen=None) -> list:
    """Each value text a reference of `text` reads (read_keys), with the values it is read in, and
    each one a reference of those reads in turn: what unevaluable_constructs reads beside the line,
    so an expression moved into an env:, an input or a matrix value is read as it is in the line.
    Raises Unreadable past MAX_DEPTH, as ref_forms does."""
    seen = set() if seen is None else seen
    if depth >= MAX_DEPTH:
        raise Unreadable(f"a chain of more than {MAX_DEPTH} values, through `{text}`")
    reached = []
    for m in REF.finditer(text):
        for key in read_keys(m.group(), values):
            for value, context in held(values, key):
                if (value, id(context)) in seen:
                    continue
                seen.add((value, id(context)))
                reached.append((value, context))
                reached += reached_texts(value, context, depth + 1, seen)
    return reached


def unevaluable_constructs(text: str, values: dict) -> list:
    """The expressions of a text this check cannot work out (opaque: a function such as format() or
    join(), a computed index, a context it does not read), each with the texts it may build its
    value from: its literals, a format() placeholder in one read as any text, and the forms of each
    value it reads - each context value it names, and every value of a context it reads whole or
    by a computed index. Raises Unreadable as key_forms does. A shell construct this check does not
    evaluate is no part of this: a command substitution or a brace expansion stands in its word as
    a value this check cannot know (named_scripts)."""
    found = []
    for m in EXPRESSION.finditer(text):
        unquoted, literals = expression_parts(m.group())
        if not opaque(unquoted):
            continue
        texts = [FORMAT_PLACEHOLDER.sub(WILD, literal) for literal in literals]
        texts += [form for key in read_keys(m.group(), values) for form in key_forms(values, key, 0, False)]
        found.append((m.group(), texts))
    return found


def overlapping_script(texts, scripts):
    """The first declared script whose path, from the root or after a `/`, or whose file name, one
    of a construct's texts may build: the text read as a pattern in which each run of characters no
    path holds (a quote, a comma, a bracket of a list), and each WILD, may be any text. So
    `buil{0}.sh` may build build.sh, and a matrix value `[build, .sh]` may too; `go-daemon-{0}-`
    builds no script. None when no text could."""
    for text in texts:
        pieces = NOT_PATH.split(str(text).replace(WILD, " "))
        if not "".join(pieces):
            continue
        tokens = literal_tokens(pieces)
        for script in sorted(scripts):
            if glob_match(tokens, script.rsplit("/", 1)[-1]) or glob_match_tail(tokens, script):
                return script
    return None


def bracket_regex(text: str) -> str:
    """A bracket expression as a regular expression of one character other than `/`: its
    characters, ranges and POSIX classes, or all but them after `!` or `^`. A class this check does
    not know reads as any character, which errs wide. One the shell would not read as a set (`[]`,
    a range out of order) is its literal text, as the shell leaves it."""
    inner = text[1:-1]
    negate = inner[:1] in ("!", "^")
    inner = inner[1:] if negate else inner
    parts, i = [], 0
    while i < len(inner):
        m = POSIX_CLASS.match(inner, i)
        if m:
            if m.group(1) not in POSIX_CLASSES:
                return "[^/]"
            parts.append(POSIX_CLASSES[m.group(1)])
            i = m.end()
            continue
        c = inner[i]
        parts.append("-" if c == "-" and 0 < i < len(inner) - 1 else re.escape(c))
        i += 1
    body = "".join(parts)
    regex = f"[^/{body}]" if negate else f"(?!/)[{body}]"
    try:
        re.compile(regex)
    except re.error:
        return re.escape(text)
    return regex if body else re.escape(text)


# A form of a word as a pattern: a list of tokens, each (a test of one character, whether it
# repeats), matched by glob_match without backtracking. The text a pattern is built from is the
# pull request's own workflow text, so its cost must not grow with what that text holds. A
# backtracking regular expression's does: each WILD was `.*`, and `p` + 8 WILD + `Z` took 0.82 s
# for one match and about 100 s for one step, past the changes job's bounds at 10 and 12 (review
# 6092517593). glob_match costs at most len(text) x len(tokens) steps, whatever the tokens are,
# and adjacent repeating tokens are one (a run of WILDs, a `*` beside a WILD).
def ANY(_: str) -> bool:
    return True


def IN_DIR(c: str) -> bool:
    return c != "/"


def add_token(tokens: list, test, repeats: bool) -> None:
    """Appends a token, folding a repeating one into a repeating one before it: `.*` beside `.*`
    or `[^/]*` is `.*`, and `[^/]*` beside `[^/]*` is `[^/]*`, as sets of texts."""
    if repeats and tokens and tokens[-1][1] and tokens[-1][0] in (ANY, IN_DIR) and test in (ANY, IN_DIR):
        if test is ANY:
            tokens[-1] = (ANY, True)
        return
    tokens.append((test, repeats))


def literal_tokens(pieces) -> list:
    """Pieces of literal text, any text between each two."""
    tokens: list = []
    for at, piece in enumerate(pieces):
        if at:
            add_token(tokens, ANY, True)
        for c in piece:
            tokens.append(((lambda ch, c=c: ch == c), False))
    return tokens


def pattern_tokens(pattern: str) -> list:
    """A form of a word as tokens: WILD any characters; `*`, `?` and a bracket expression a shell
    glob's, within one directory (bracket_regex); any other character itself."""
    tokens: list = []
    i = 0
    while i < len(pattern):
        c = pattern[i]
        bracket = BRACKET_RE.match(pattern, i) if c == "[" else None
        if c == WILD:
            add_token(tokens, ANY, True)
        elif c == "*":
            add_token(tokens, IN_DIR, True)
        elif c == "?":
            tokens.append((IN_DIR, False))
        elif bracket:
            regex = bracket_regex(bracket.group())
            if regex == re.escape(bracket.group()):
                # No set the shell would read: its literal text, as the shell leaves it.
                tokens += [((lambda ch, c=ch_: ch == c), False) for ch_ in bracket.group()]
            else:
                one = re.compile(regex)
                tokens.append(((lambda ch, one=one: one.fullmatch(ch) is not None), False))
            i = bracket.end()
            continue
        else:
            tokens.append(((lambda ch, c=c: ch == c), False))
        i += 1
    return tokens


def glob_match(tokens: list, text: str) -> bool:
    """Whether `text` is one of the texts `tokens` stand for: the positions the tokens may be at,
    carried across the text one character at a time, so no position is tried twice."""
    def close(at: set) -> set:
        reached, stack = set(), list(at)
        while stack:
            s = stack.pop()
            if s not in reached:
                reached.add(s)
                if s < len(tokens) and tokens[s][1]:
                    stack.append(s + 1)
        return reached

    states = close({0})
    for ch in text:
        states = close({s if tokens[s][1] else s + 1 for s in states if s < len(tokens) and tokens[s][0](ch)})
        if not states:
            return False
    return len(tokens) in states


def glob_match_tail(tokens: list, text: str) -> bool:
    """Whether `text`, or what follows one of its `/`s, is one of the texts `tokens` stand for."""
    return glob_match(tokens, text) or any(glob_match(tokens, text[i + 1:]) for i, c in enumerate(text) if c == "/")


def has_stem(name: str) -> bool:
    """Whether a file name holds a literal character before its first dot (GLOB_STEM)."""
    return bool(GLOB_STEM.match(BRACKET_RE.sub("\x00", name)))


def path_tail(word: str) -> str:
    """A word as a path: normalized, its leading `/` and `..`s and option dashes dropped."""
    return re.sub(r"^(?:\.\./)+", "", os.path.normpath(word.lstrip("-")).lstrip("/"))


def tail_names(path: str, scripts) -> set:
    """The scripts a path names when the directory it is read from is not known: a literal one
    when either path ends in the other at a `/`; a glob when its last components match the
    script's whole path, or, when its file name holds a stem (has_stem), when the script ends in
    what it matches."""
    tail = path_tail(path)
    if not GLOB_CHARS.search(tail):
        return {s for s in scripts if s == tail or s.endswith("/" + tail) or tail.endswith("/" + s)}
    parts = tail.split("/")
    named = {s for s in scripts if len(parts) > s.count("/")
             and glob_match(pattern_tokens("/".join(parts[-(s.count("/") + 1):])), s)}
    if has_stem(parts[-1]):
        tokens = pattern_tokens(tail)
        named |= {s for s in scripts if glob_match_tail(tokens, s)}
    return named


def piece_names(piece: str, workdir, scripts, uncertain: bool) -> tuple:
    """The scripts one piece of a word's form names, and a declared script it may name though this
    check cannot read it, or None. After ROOT_MARK a piece is read from the root alone. Otherwise
    it is read from the root and from the step's directory (`workdir`, None when unknown), as the
    shell expands it there - unless it starts with a WILD, which leaves it no known start - and
    then as tail_names reads a path from an unknown directory: the whole piece, or, when it holds a
    WILD, the part after its last WILD when that holds a `/`.

    A piece whose file name is open - a glob, or a value this check cannot know in it - and whose
    start is in doubt - a cd or an unknown working-directory (`uncertain`), or a WILD it starts
    with - may be any file whose name its file name matches, anywhere. (A piece with a known
    start is matched whole above, a WILD in it standing for any text, which reads every file it
    may be.) When that file name holds a literal stem before its first dot (`buil?.sh`,
    `pre${x}.sh`), every script whose file name it matches is named. When it holds none (`*`,
    `*.sh`, `${x}.sh`), naming every such script would refuse every step that globs a directory,
    so the piece is one this check cannot read when it matches a script's file name. A file name
    that is a WILD alone (`"$x"`, `"$dir/$x"`) is any text, a value no file gives."""
    if ROOT_MARK in piece:
        piece = piece.rsplit(ROOT_MARK, 1)[1].lstrip("/")
        if not piece.startswith(WILD):
            # The root is a known start, which a cd does not move.
            path = piece if WILD in piece else os.path.normpath(piece)
            tokens = pattern_tokens(path)
            return {s for s in scripts if glob_match(tokens, s)}, None
    # A piece that starts with a value this check cannot know has no known start, so it is read
    # from no directory, only by its end.
    anchors = [] if piece.startswith(WILD) else [piece] + ([os.path.join(workdir, piece)] if workdir is not None else [])
    anchored = [pattern_tokens(a if WILD in a else os.path.normpath(a).lstrip("/")) for a in anchors]
    named = {s for s in scripts for tokens in anchored if glob_match(tokens, s)}
    if WILD not in piece:
        named |= tail_names(piece, scripts)
    else:
        ending = piece.rsplit(WILD, 1)[1]
        if "/" in ending:
            named |= tail_names(ending[ending.index("/") + 1:], scripts)
    if not (uncertain or piece.startswith(WILD)):
        return named, None
    found, unread = open_names(piece.rsplit("/", 1)[-1], scripts)
    return named | found, unread


def open_names(name: str, scripts) -> tuple:
    """What piece_names reads off an open file name whose start is in doubt: the scripts it names,
    and a declared script whose file name it matches though it cannot be read, or None."""
    if not GLOB_CHARS.search(name) or not name.replace(WILD, ""):
        return set(), None
    tokens = pattern_tokens(name)
    matching = sorted(s for s in scripts if glob_match(tokens, s.rsplit("/", 1)[-1]))
    if has_stem(name):
        return set(matching), None
    return set(), matching[0] if matching else None


def named_scripts(lines, workdir, scripts, values=None) -> tuple:
    """The scripts of `scripts` (repository paths) a step's lines name, and each construct of them
    that stands for text this check cannot know whose literal parts could make a declared script's
    name, with that script: a word holding such a value or an open glob whose start is in doubt
    (piece_names), an expression of them or of a value they reach that this check cannot work out
    (unevaluable_constructs), and a word a backslash escape joins, which read with its backslashes
    dropped would name the script. Each word is read as every form it may take (text_forms), and
    each form's pieces as piece_names reads them: a word names a script when it is the script's
    path from the step's directory or the root, when either path ends in the other at a `/` (after
    a cd a relative path can lead from any directory, an absolute one from any checkout), and after
    a workspace reference when it is the script's root path; a glob as the shell would expand it,
    and more widely as piece_names says. A command substitution and a brace expansion stand in
    their word as a value this check cannot know, and a command substitution's own text is read as
    a line of its own. The text is never parsed: a word that names the script counts as a start of
    it, whatever command it is a word of, which errs wide. Raises Unreadable for text or a value
    this check cannot evaluate to literal text: a line that runs `shopt`, a line that a
    continuation joins to the next one inside a word, and every case ref_forms, reached_texts and
    held raise."""
    values = values or {}
    named, refusals = set(), []
    uncertain = workdir is None or any(CHANGES_DIRECTORY.search(line) for line in lines)
    pending, i = list(lines), 0
    while i < len(pending):
        line = pending[i]
        i += 1
        if SHOPT.search(line):
            raise Unreadable("`shopt`, which changes how bash expands the words this check reads")
        if CONTINUED_WORD.search(line):
            raise Unreadable(f"`{line.rsplit(None, 1)[-1]}`, a word a backslash continues onto the next line")
        for text, context in [(line, values)] + reached_texts(line, values):
            for construct, texts in unevaluable_constructs(text, context):
                script = overlapping_script(texts, scripts)
                if script and (construct, script) not in refusals:
                    refusals.append((construct, script))
        shell = dequote(line)
        for m in ESCAPED_WORD.finditer(shell):
            joined = m.group().replace("\\", "")
            for piece in FORM_PIECE.findall(joined):
                for script in sorted(piece_names(piece, workdir, scripts, uncertain)[0]):
                    if (m.group(), script) not in refusals:
                        refusals.append((m.group(), script))
        while COMMAND_SUBSTITUTION.search(shell):
            pending += [m.group(1) or m.group(2) or "" for m in COMMAND_SUBSTITUTION.finditer(shell)]
            shell = COMMAND_SUBSTITUTION.sub(WILD, shell)
        shell = BRACE_EXPANSION.sub(WILD, shell)
        for m in WORD.finditer(shell):
            for form in text_forms(m.group(), values):
                for piece in FORM_PIECE.findall(form):
                    found, unread = piece_names(piece, workdir, scripts, uncertain)
                    named |= found
                    word = m.group().replace(WILD, "…")
                    if unread and (word, unread) not in refusals:
                        refusals.append((word, unread))
    return named, refusals
