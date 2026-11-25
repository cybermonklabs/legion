"""Just enough shell to find each simple command's first word in a workflow step's `run:` or a
script.

check-release-credentials.sh imports it from the repository it checks, running
`PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=.github/scripts python3` from that repository's root, and
classifies each command commands_of returns. check-log-guard.sh reads each step and each script a
step runs with it.

Command position is found by one quote-aware scan of the `run:` (its `${{ … }}` expressions
elided first), as bash reads it: a `#` starts a comment only at word start — the start of the run,
after unquoted whitespace or a newline, or after an unquoted `;`, `&`, `|`, `(`, `)`, `<`, `>` or
backtick, not `{` (`${#x}`, `$#` and `a#b` are words) — and the comment runs to the real newline,
so a backslash inside it continues nothing; escaped whitespace and an escaped `#` are word
characters (`echo a\\ #` has no comment); a backslash-newline joins lines outside single quotes,
whatever the number of backslashes before it (an escaped backslash is a character: `printf 'x' \\\\`
ends its command, `printf 'x' \\\\\\` continues it, and `a\\` + newline + `#` glues `a#`); a quote
carries across newlines, and `;`, `&`, `|` and `#` inside one are literal, in a `$'…'` string too,
where a backslash escapes the character after it; `$(…)`, `<(…)`, `>(…)` and backticks open a
command of their own, inside a double-quoted string too, and at the close the string and the
command around them resume, so a word after the close is that command's (`is "$(cut …)" gh` runs
`is`); `(…)` is a subshell, whose commands are its own; an unquoted `;`, `&`, `|`, `(`, `)` or
newline ends a command, and a redirection's target (`>/dev/null`, `>> "$GITHUB_OUTPUT"`, `2>&1`) is
not one of its words. A here-document's body, from the newline after its `<<WORD` (or `<<-WORD`,
whose body's lines may start with tabs) to the line that is WORD, is a script when a command of
the line that writes it is a shell reading its standard input, before the `<<` or after it, behind
any of RUNNERS but xargs (`bash <<'EOT'`, `cat <<'EOT' | bash`, `setsid bash <<'EOT'`, `bash
/dev/stdin <<'EOT'`, `. /dev/stdin <<EOT`: feeds_shell), and is read as one; any other body is text,
but with an unquoted WORD bash expands its `$(…)` and backtick substitutions, which are read as the
commands they run. Arithmetic (`$(( … ))`, `(( … ))`) runs no command but its substitutions
(`$(( $(cmd) ))` runs `cmd`). An array's values (`tools+=(curl jq)`), to the first `)` outside
their quotes and comments, are words of no command, but a substitution among them runs, `<(…)` and
`>(…)` too. Nor does a case pattern run a command (`curl | jq)`, from the `in` or the `;;`, `;&` or
`;;&` before it to its unquoted `)`), or the name of a function a `()` defines (`curl() {`). Each
command sheds its leading `VAR=value` assignments and the keywords that precede a command (if,
then, for, do, time, exec, env, `{`, `}`, …) or wrap one (`ci_log_guard` and `ci_log_guard_stderr`,
.github/scripts/ci-log-guard.sh's, which run the words after them), and the first word left is the
command; an
interpreter there (one of INTERPRETERS: bash, sh, dash, zsh, source, `.`) is looked through, past
its flags and the option name an `-o`, `+o`, `-O` or `+O` takes, to the word after it (`bash -o
pipefail scripts/x.sh` runs `scripts/x.sh`), and the script string behind its `-c`, like the words
behind `eval`, is scanned the same way (`sh -c 'bun install'`, `eval "bun install"` and `eval bun
install` run `bun`).
"""

import re

EXPRESSION = re.compile(r"\$\{\{.*?\}\}")
ASSIGNMENT = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*\+?=")
SKIP_WORDS = {
    "if", "then", "else", "elif", "fi", "do", "done", "while", "until", "for", "in",
    "time", "exec", "env", "!", "{", "}", "case", "esac",
    # .github/scripts/ci-log-guard.sh's wrappers, and the copy a credentialed release step defines
    # inline: each runs the words after it as a command, so that command is the one a rule reads.
    "ci_log_guard", "ci_log_guard_stderr",
}
# Transparent in command position: the command is the word after the interpreter and its flags (an
# `-o`, `+o`, `-O` or `+O`, alone or ending a cluster such as `-eo`, takes the next word as the
# option's name), or each command of the script string after its `-c`. `eval` runs the words after
# it as such a script string.
INTERPRETERS = {"bash", "sh", "dash", "zsh", "source", "."}


class Scanner:
    """One quote-aware pass over a `run:` or a script, as bash reads it (the module's rules),
    collecting each simple command's words. Between characters it holds the word being read, the
    words of the command being read, whether the next character begins a word (where a `#` starts a
    comment), whether the next word is a redirection's target (dropped: `>/dev/null` names no
    command), the here-documents whose bodies start at the next newline, and, inside a `case`, the
    pattern being read."""

    def __init__(self) -> None:
        self.commands: list[list[str]] = []
        self.words: list[str] = []
        self.word = ""
        self.started = False
        self.at_word_start = True
        self.redirect_target = False
        # (delimiter, whether `<<-` strips its tabs, whether the delimiter is quoted) of each
        # here-document whose body starts at the next newline.
        self.heredocs: list[tuple[str, bool, bool]] = []
        # The words of each command ended on the line being read, before command_of reads through
        # them: the newline decides from them whether a shell reads that line's here-documents.
        self.line_words: list[list[str]] = []
        self.bodies: list[tuple[int, int]] = []  # (start, end) of each here-document body skipped
        self.line_quotes: dict[int, str] = {}  # index of each line that starts inside a quote -> it
        self.cases = 0  # the `case`s open
        self.pattern: str | None = None  # the case pattern being read, or None outside one

    def add(self, char: str) -> None:
        self.word += char
        self.started = True
        self.at_word_start = False

    def end_word(self) -> None:
        if self.started:
            if not self.redirect_target:
                self.words.append(self.word)
            self.redirect_target = False
        self.word = ""
        self.started = False
        self.at_word_start = True
        if len(self.words) == 3 and self.words[0] == "case" and self.words[2] == "in":
            self.words = []  # `case WORD in` runs nothing, and its first pattern follows
            self.cases += 1
            self.pattern = ""
        elif self.words == ["esac"] and self.cases:
            self.cases -= 1

    def end_command(self) -> None:
        self.end_word()
        if self.words:
            self.line_words.append(list(self.words))
            self.record(self.words)
        self.words = []
        self.redirect_target = False

    def record(self, words: list[str]) -> None:
        """Notes the command an ended simple command's words run (command_of)."""
        self.commands += command_of(words)

    def suspend(self, placeholder: str) -> tuple:
        """The command a `$(…)`, a backtick pair or a process substitution interrupts, which takes
        `placeholder` into the word being read where the substitution stands; a command of the
        substitution's own starts inside it."""
        saved = (self.words, self.word + placeholder, self.redirect_target)
        self.words, self.word, self.started, self.at_word_start, self.redirect_target = [], "", False, True, False
        return saved

    def resume(self, saved: tuple) -> None:
        """Ends the substitution's command, and reads on in the command it interrupted."""
        self.end_command()
        self.words, self.word, self.redirect_target = saved
        self.started, self.at_word_start = True, False

    def end_values(self, saved: tuple) -> None:
        """Drops an array's values, which are words of no command, and reads on in the assignment
        the array's `(` interrupted."""
        self.words, self.word, self.redirect_target = saved
        self.started, self.at_word_start = True, False

    def is_function_name(self) -> bool:
        """Whether the words read so far are `name` or `function name`, which a `()` after them
        makes a function's definition rather than a command."""
        words = self.words + ([self.word] if self.started else [])
        if words[:1] == ["function"]:
            words = words[1:]
        return len(words) == 1 and "=" not in words[0] and "$" not in words[0]

    def read_heredoc(self, text: str, i: int) -> int:
        """Reads the delimiter of the here-document whose `<<` ends just before `i`, and returns
        where reading resumes. The body, from the next newline to the delimiter's own line, is read
        there (skip_heredocs): whether the delimiter is quoted, and whether a command of the line
        is a shell reading its standard input (feeds_shell), decide how."""
        self.end_word()
        strip = text[i:i + 1] == "-"
        i += strip
        while text[i:i + 1] in (" ", "\t"):
            i += 1
        delimiter, quoted = "", False
        while i < len(text) and text[i] not in " \t\n;&|<>()":
            if text[i] in "'\"":
                quoted = True
                end = text.find(text[i], i + 1)
                end = len(text) if end < 0 else end
                delimiter += text[i + 1:end]
                i = end + 1
            elif text[i] == "\\":
                quoted = True
                delimiter += text[i + 1:i + 2]
                i += 2
            else:
                delimiter += text[i]
                i += 1
        if delimiter:
            self.heredocs.append((delimiter, strip, quoted))
        return i

    def skip_heredocs(self, text: str, i: int) -> int:
        """Reads the body of each here-document pending, from `i`, the start of a line; a body no
        delimiter line ends runs to the end, as bash reads it. When a command of the line that
        wrote them is a shell reading its standard input (`bash <<'EOT'`, `cat <<'EOT' | bash`,
        `setsid bash <<'EOT'`), each body is commands, read as a script: the line is read whole,
        before the body, so the shell may stand after the `<<`. Any other body is text, noted in
        `bodies`, but bash expands the `$(…)` and backtick substitutions of a body whose delimiter
        is unquoted, so those are read as commands."""
        shell = any(feeds_shell(words) for words in self.line_words)
        for delimiter, strip, quoted in self.heredocs:
            start = i
            while i < len(text):
                end = text.find("\n", i)
                line = text[i:] if end < 0 else text[i:end]
                after = len(text) if end < 0 else end + 1
                if (line.lstrip("\t") if strip else line) == delimiter:
                    break
                i = after
            body = text[start:i]
            if shell:
                self.commands += type(self)().scan(body)
            else:
                self.bodies.append((start, i))
                if not quoted:
                    self.scan_substitutions(body)
            if i < len(text):
                end = text.find("\n", i)
                i = len(text) if end < 0 else end + 1
        self.heredocs = []
        self.line_words = []
        return i

    def scan_substitutions(self, text: str) -> None:
        """Reads each `$(…)` and backtick substitution of `text`, which is no command itself (an
        unquoted here-document's body, arithmetic), as the commands bash runs to expand it."""
        for inner in substitutions(text):
            self.commands += type(self)().scan(inner)

    def read_pattern(self, text: str, i: int) -> int:
        """Reads the character at `i` of a case pattern list, and returns where reading resumes.
        The list ends at its unquoted `)`, and an `esac` in its place ends the case. A pattern
        is no command, so nothing in it is a word."""
        char = text[i]
        if self.pattern.strip() == "esac" and char in " \t\n;&|)":
            self.pattern = None
            self.cases -= 1
            return i
        if char == "'":
            end = text.find("'", i + 1)
            self.pattern += "x"
            return len(text) if end < 0 else end + 1
        if char == '"':
            j = i + 1
            while j < len(text) and text[j] != '"':
                j += 2 if text[j] == "\\" else 1
            self.pattern += "x"
            return j + 1
        if char == "\\":
            self.pattern += "x"
            return i + 2
        if char == "#" and not self.pattern.strip():
            end = text.find("\n", i)
            return len(text) if end < 0 else end
        if char == ")":
            self.pattern = None
        elif char != "(" or self.pattern.strip():
            self.pattern += char
        return i + 1

    def scan(self, text: str) -> list[list[str]]:
        quote = None
        # The open `$(`, `<(`, `>(`, `(`, backticks and arrays' `name=(`, innermost last, each with
        # the quote it interrupted and the command it suspended (None for a subshell, which starts
        # commands of its own): inside, an unquoted command of its own starts, but for an array's
        # values, which are words, read with the quotes and comments a command's are; at the close,
        # the string and the command it interrupted resume.
        open_subs: list[tuple[str, str | None, tuple | None]] = []
        i = 0
        while i < len(text):
            char = text[i]
            after = text[i + 1:i + 2]
            if char == "\n" and quote:
                self.line_quotes[text.count("\n", 0, i) + 1] = quote
            if quote == "'":
                if char == "'":
                    quote = None
                else:
                    self.add(char)
            elif quote == "$'":
                if char == "\\" and after:
                    self.add(after)
                    i += 1
                elif char == "'":
                    quote = None
                else:
                    self.add(char)
            elif self.pattern is not None and quote is None:
                i = self.read_pattern(text, i)
                continue
            elif char == "`":
                if open_subs and open_subs[-1][0] == "tick":
                    _, quote, saved = open_subs.pop()
                    self.resume(saved)
                else:
                    open_subs.append(("tick", quote, self.suspend("`")))
                    quote = None
            elif char == "$" and text[i + 1:i + 3] == "((":
                # Arithmetic, `$(( … ))`: a value, and its `<<` a shift; a substitution in it runs.
                self.add(char)
                end = arithmetic_end(text, i + 1)
                self.scan_substitutions(text[i + 3:end - 2])
                i = end
                continue
            elif char == "$" and after == "(":
                open_subs.append(("paren", quote, self.suspend("$")))
                quote = None
                i += 1
            elif quote == '"':
                if char == '"':
                    quote = None
                elif char == "\\" and after in ("$", "`", '"', "\\"):
                    self.add(after)
                    i += 1
                elif char == "\\" and after == "\n":
                    i += 1
                else:
                    self.add(char)  # any other `\x` keeps both characters
            elif char == "#" and self.at_word_start:
                end = text.find("\n", i)  # the newline stays, to end the command below
                i = len(text) if end < 0 else end
                continue
            elif char == "\\" and after == "\n":
                i += 1
            elif char == "\\" and after:
                self.add(after)
                i += 1
            elif char == "$" and after == "'":
                quote = "$'"
                self.started = True
                self.at_word_start = False
                i += 1
            elif char in "'\"":
                quote = char
                self.started = True
                self.at_word_start = False
            elif char == "(" and after == "(" and self.at_word_start and not self.words:
                end = arithmetic_end(text, i)  # `(( … ))`, an arithmetic command
                self.scan_substitutions(text[i + 2:end - 2])
                i = end
                continue
            elif char == "(" and FUNCTION_PARENS.match(text, i) and self.is_function_name():
                self.words, self.word, self.started, self.at_word_start = [], "", False, True
                i = FUNCTION_PARENS.match(text, i).end()
                continue
            elif char in "<>" and after == "(":
                open_subs.append(("paren", None, self.suspend(char + "(")))
                i += 1
            elif char == "(" and self.started and ARRAY_ASSIGNMENT.fullmatch(self.word):
                # `tools+=(curl jq)`, `helm=(go run "$HELM")`: an array's values, no command, up to
                # the first unquoted `)`; a substitution among them runs, `<(…)` and `>(…)` too.
                open_subs.append(("array", None, self.suspend("(…)")))
            elif char == "(" and open_subs and open_subs[-1][0] == "array":
                self.end_word()  # bash refuses it; read it as no command
            elif char == "(":
                self.end_command()
                open_subs.append(("paren", None, None))
            elif char == ")" and open_subs and open_subs[-1][0] == "array":
                self.end_values(open_subs.pop()[2])
            elif char == ")":
                if open_subs and open_subs[-1][0] == "paren":
                    _, quote, saved = open_subs.pop()
                    if saved is None:
                        self.end_command()
                    else:
                        self.resume(saved)
                else:
                    self.end_command()
            elif char == "<" and after == "<" and text[i + 2:i + 3] != "<":
                i = self.read_heredoc(text, i + 2)
                continue
            elif char in "<>":
                self.end_word()
                self.redirect_target = True
                while text[i + 1:i + 2] in ("<", ">", "&", "|"):  # `>>`, `2>&1`, `<<<`, `>|`
                    i += 1
            elif char in "\n;&|" and open_subs and open_subs[-1][0] == "array":
                self.end_word()  # a newline between an array's values; bash refuses `;`, `&` and `|`
            elif char == "\n":
                self.end_command()
                if self.heredocs:
                    i = self.skip_heredocs(text, i + 1)
                    continue
                self.line_words = []
            elif char == ";" and self.cases and after in (";", "&"):
                # `;;`, `;&` or `;;&` ends a case item, and the next item's patterns follow.
                self.end_command()
                i += 1 + (after == ";" and text[i + 2:i + 3] == "&")
                self.pattern = ""
            elif char in ";&|":
                self.end_command()
            elif char in " \t":
                self.end_word()
            else:
                self.add(char)
            i += 1
        self.end_command()
        return self.commands


# The `()` after a function's name, with any blanks inside it.
FUNCTION_PARENS = re.compile(r"\([ \t]*\)")
# The word before an array's `(`: `name=`, `name+=`, `name[i]=`.
ARRAY_ASSIGNMENT = re.compile(r"[A-Za-z_][A-Za-z0-9_]*(?:\[[^]]*\])?\+?=")


def arithmetic_end(text: str, i: int) -> int:
    """Just past the `))` that closes the `((` at `i`, counting the parentheses between."""
    depth = 0
    while i < len(text):
        if text[i] == "(":
            depth += 1
        elif text[i] == ")":
            depth -= 1
            if depth == 0:
                return i + 1
        i += 1
    return i


def substitutions(text: str) -> list[str]:
    """The text inside each outermost `$(…)` (not `$((`) and backtick pair of `text`, which bash
    expands in an unquoted here-document's body and in arithmetic. A quote there is a character,
    not a quote, so the parentheses are counted as written, as bash counts them in a body."""
    found, i = [], 0
    while i < len(text):
        if text[i] == "\\":
            i += 2
            continue
        if text.startswith("$(", i) and not text.startswith("$((", i):
            end = arithmetic_end(text, i + 1)
            found.append(text[i + 2:end - 1])
            i = end
            continue
        if text[i] == "`":
            end = text.find("`", i + 1)
            end = len(text) if end < 0 else end
            found.append(text[i + 1:end])
            i = end + 1
            continue
        i += 1
    return found


# Commands that run the words after them, each with its options that take a value (their value is
# skipped with them) and the number of plain words it takes before the command (timeout's
# duration, flock's lock file). env also takes `NAME=value` words; `command -v` and `command -V`
# look a name up rather than run it; flock runs a `-c` string as sh -c does. check-log-guard.sh
# reads a client through every one; feeds_shell reads a shell through every one but xargs, which
# reads its own standard input, so a here-document never reaches the command it runs.
RUNNERS = {
    "sudo": ({"-u", "-g", "-C", "-h", "-p", "-U", "-r", "-t", "-D", "-R", "--user", "--group", "--chdir",
              "--host", "--prompt", "--other-user", "--role", "--type", "--close-from"}, 0),
    "timeout": ({"-s", "--signal", "-k", "--kill-after"}, 1),
    "nice": ({"-n", "--adjustment"}, 0),
    "nohup": (set(), 0),
    "stdbuf": ({"-i", "-o", "-e", "--input", "--output", "--error"}, 0),
    "xargs": ({"-n", "-L", "-P", "-I", "-s", "-d", "-E", "-a", "--max-args", "--max-lines", "--max-procs",
               "--replace", "--delimiter", "--eof", "--arg-file", "--max-chars"}, 0),
    "env": ({"-u", "--unset", "-C", "--chdir", "-S", "--split-string"}, 0),
    "exec": ({"-a"}, 0),
    "command": (set(), 0),
    "time": (set(), 0),
    "flock": ({"-w", "--wait", "--timeout", "-E", "--conflict-exit-code"}, 1),
    "ionice": ({"-c", "--class", "-n", "--classdata", "-p", "--pid", "-P", "--pgid", "-u", "--uid"}, 0),
    "setsid": (set(), 0),
    "unshare": ({"-S", "--setuid", "-G", "--setgroups", "-R", "--root", "-w", "--wd", "--map-user", "--map-group",
                 "--propagation", "--kill-child"}, 0),
    # .github/scripts/gh-read.sh's two: each runs the command after it, and prints the stderr of
    # every run it ends on itself (unless --stderr names a file), outside any block, so a step that
    # calls one is read as calling the client.
    "gh_wait_reset": ({"--stderr"}, 0),
    "gh_read": ({"--stderr"}, 0),
}


def strip_runner(words: list[str]) -> str:
    """Takes the runner at the head of `words` (one of RUNNERS), its options and its plain words
    off `words` in place, and returns its name."""
    runner = words.pop(0).rsplit("/", 1)[-1]
    valued, plain = RUNNERS[runner]
    while words and words[0].startswith("-") and words[0] != "-":
        flag = words.pop(0)
        if flag in valued and words:
            words.pop(0)
    while runner == "env" and words and ASSIGNMENT.match(words[0]):
        words.pop(0)
    del words[:plain]
    return runner


# The shells whose standard input is a script when they are given no script and no `-c` (or `-s`),
# and the paths that name a process's standard input, which a shell or `.` given one reads.
STDIN_SHELLS = {"bash", "sh", "dash", "zsh"}
STDIN_PATHS = {"/dev/stdin", "/dev/fd/0", "/proc/self/fd/0"}


def feeds_shell(words: list[str]) -> bool:
    """Whether the command `words` runs is a shell reading its standard input as its script, so a
    here-document its line writes is commands: bash, sh, dash or zsh, past the assignments and
    keywords before it and each of RUNNERS but xargs with its options, given no script, `-s`, or
    a path of STDIN_PATHS as its script, and no `-c`; or `.` or `source` of such a path. As in
    `bash <<'EOT'`, `setsid bash <<'EOT'`, `timeout --signal KILL 5m bash <<'EOT'`, `sudo bash -s
    <<EOT`, `bash /dev/stdin <<'EOT'` and `. /dev/stdin <<EOT`."""
    words = list(words)
    while words:
        if words[0].rsplit("/", 1)[-1] in RUNNERS and words[0].rsplit("/", 1)[-1] != "xargs":
            strip_runner(words)
        elif ASSIGNMENT.match(words[0]) or words[0] in SKIP_WORDS:
            words.pop(0)
        else:
            break
    if not words:
        return False
    if words[0] in ("source", ".") and words[1:2] and words[1] in STDIN_PATHS:
        return True
    if words[0].rsplit("/", 1)[-1] not in STDIN_SHELLS:
        return False
    rest, stdin = words[1:], False
    while rest:
        word = rest.pop(0)
        if word == "--":
            return stdin or not rest or rest[0] in STDIN_PATHS
        elif word.startswith(("-", "+")) and not word.startswith("--"):
            if "c" in word[1:]:
                return False
            stdin = stdin or "s" in word[1:]
            if word[-1] in "oO":
                rest = rest[1:]  # `-eo pipefail`: the option's name
        elif not word.startswith("--"):
            return stdin or word in STDIN_PATHS  # with -s, the arguments; else a script file
    return True


def without_heredocs(text: str) -> str:
    """`text` with each here-document's body blanked line for line, its `<<WORD` and its delimiter
    line kept: the lines a Scanner reads no command in, for a reader that reads by the line."""
    scanner = Scanner()
    scanner.scan(text)
    for start, end in reversed(scanner.bodies):
        text = text[:start] + "\n" * text.count("\n", start, end) + text[end:]
    return text


def quoted_line_starts(text: str) -> dict:
    """{the index of each line of `text` that starts inside a quoted string: that quote (`'`, `"` or
    `$'`)}, as a Scanner reads it, for a reader that reads by the line."""
    scanner = Scanner()
    scanner.scan(text)
    return scanner.line_quotes


def commands_of(run: str) -> list[list[str]]:
    """Each simple command of a `run:`, as its words with the command first."""
    return Scanner().scan(EXPRESSION.sub("EXPR", run))


def command_of(words: list[str]) -> list[list[str]]:
    """The command the words run, after the assignments and keywords before it: the words from the
    command on; or, when the command is an interpreter, the command it is handed (`bash -o pipefail
    scripts/x.sh` runs `scripts/x.sh`), or each command of the script string behind its `-c`
    (`sh -c 'bun install && go build'` runs `bun install` and `go build`); or, when it is `eval`,
    each command of the words after it, joined (`eval "bun install"` and `eval bun install` both
    run `bun install`)."""
    while words and (ASSIGNMENT.match(words[0]) or words[0] in SKIP_WORDS):
        words.pop(0)
    if not words:
        return []
    if words[0] == "eval":
        return commands_of(" ".join(words[1:]))
    if words[0] not in INTERPRETERS:
        return [words]
    words.pop(0)
    while words and words[0].startswith(("-", "+")):
        flag = words.pop(0)
        if flag.startswith("--"):
            continue
        if "c" in flag:
            return commands_of(words[0]) if words else []
        if flag.endswith(("o", "O")) and words:
            words.pop(0)
    return command_of(words)
