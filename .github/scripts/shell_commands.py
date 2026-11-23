"""Just enough shell to find each simple command's first word in a workflow step's `run:`.

check-release-credentials.sh imports it from the repository it checks, running
`PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=.github/scripts python3` from that repository's root, and
classifies each command commands_of returns.

Command position is found by one quote-aware scan of the `run:` (its `${{ … }}` expressions
elided first), as bash reads it: a `#` starts a comment only at word start — the start of the run,
after unquoted whitespace or a newline, or after an unquoted `;`, `&`, `|`, `(`, `)`, `<`, `>` or
backtick, not `{` (`${#x}`, `$#` and `a#b` are words) — and the comment runs to the real newline,
so a backslash inside it continues nothing; escaped whitespace and an escaped `#` are word
characters (`echo a\\ #` has no comment); a backslash-newline joins lines outside single quotes,
whatever the number of backslashes before it (an escaped backslash is a character: `printf 'x' \\\\`
ends its command, `printf 'x' \\\\\\` continues it, and `a\\` + newline + `#` glues `a#`); a quote
carries across newlines, and `;`, `&`, `|` and `#` inside one are literal; `$(…)`, `<(…)`, `(…)`
and backticks open a command of their own, inside a double-quoted string too, and the string
resumes at the close; an unquoted `;`, `&`, `|`, `(`, `)` or newline ends a command, and a
redirection's target (`>/dev/null`, `>> "$GITHUB_OUTPUT"`, `2>&1`) is not one of its words. Each
command sheds its leading `VAR=value` assignments and the keywords that precede a command (if,
then, for, do, time, exec, env, `{`, `}`, …), and the first word left is the command; an
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
}
# Transparent in command position: the command is the word after the interpreter and its flags (an
# `-o`, `+o`, `-O` or `+O`, alone or ending a cluster such as `-eo`, takes the next word as the
# option's name), or each command of the script string after its `-c`. `eval` runs the words after
# it as such a script string.
INTERPRETERS = {"bash", "sh", "dash", "zsh", "source", "."}


class Scanner:
    """One quote-aware pass over a `run:`, as bash reads it (the module's rules), collecting each
    simple command's words. Between characters it holds the word being read, the words of the
    command being read, whether the next character begins a word (where a `#` starts a comment),
    and whether the next word is a redirection's target (dropped: `>/dev/null` names no command)."""

    def __init__(self) -> None:
        self.commands: list[list[str]] = []
        self.words: list[str] = []
        self.word = ""
        self.started = False
        self.at_word_start = True
        self.redirect_target = False

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

    def end_command(self) -> None:
        self.end_word()
        if self.words:
            self.commands += command_of(self.words)
        self.words = []
        self.redirect_target = False

    def scan(self, text: str) -> list[list[str]]:
        quote = None
        # The open `$(`, `<(`, `(` and backticks, innermost last, each with the quote it interrupted:
        # inside, an unquoted command of its own starts; at the close, the string resumes.
        open_subs: list[tuple[str, str | None]] = []
        i = 0
        while i < len(text):
            char = text[i]
            after = text[i + 1:i + 2]
            if quote == "'":
                if char == "'":
                    quote = None
                else:
                    self.add(char)
            elif char == "`":
                self.end_command()
                if open_subs and open_subs[-1][0] == "tick":
                    quote = open_subs.pop()[1]
                else:
                    open_subs.append(("tick", quote))
                    quote = None
            elif char == "$" and after == "(":
                self.add(char)  # the `$` and what precedes it stay with the outer word
                self.end_command()
                open_subs.append(("paren", quote))
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
            elif char in "'\"":
                quote = char
                self.started = True
                self.at_word_start = False
            elif char == "(" or (char in "<>" and after == "("):
                if char != "(":
                    i += 1
                self.end_command()
                open_subs.append(("paren", None))
            elif char == ")":
                self.end_command()
                if open_subs and open_subs[-1][0] == "paren":
                    quote = open_subs.pop()[1]
            elif char in "<>":
                self.end_word()
                self.redirect_target = True
                while text[i + 1:i + 2] in ("<", ">", "&", "|"):  # `>>`, `2>&1`, `<<<`, `>|`
                    i += 1
            elif char in ";&|\n":
                self.end_command()
            elif char in " \t":
                self.end_word()
            else:
                self.add(char)
            i += 1
        self.end_command()
        return self.commands


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
