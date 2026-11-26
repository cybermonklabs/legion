"""Just enough shell to find each simple command's first word in a workflow step's `run:` or a
script.

check-release-credentials.sh imports it from the repository it checks, running
`PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=.github/scripts python3` from that repository's root, and
classifies each command commands_of returns. check-log-guard.sh reads each step and each script a
step runs with it.

Command position is found by one quote-aware scan of the `run:` (its `${{ … }}` expressions
elided first), as bash reads it: a `#` starts a comment only at word start — the start of the run,
after unquoted whitespace or a newline, or after an unquoted `;`, `&`, `|`, `(`, `)`, `<`, `>` or
backtick, not `{` (`$#` is a word, and so are a length expansion such as ${#x} and a#b) — and the comment runs to the real newline,
so a backslash inside it continues nothing; escaped whitespace and an escaped `#` are word
characters (`echo a\\ #` has no comment); a backslash-newline joins lines outside single quotes,
whatever the number of backslashes before it (an escaped backslash is a character: `printf 'x' \\\\`
ends its command, `printf 'x' \\\\\\` continues it, and `a\\` + newline + `#` glues `a#`); a quote
carries across newlines, and `;`, `&`, `|` and `#` inside one are literal; a `$'…'` string's
escapes are decoded as bash decodes them (`$'\\x63url'` is `curl`); `$(…)`, `<(…)`, `>(…)` and
backticks open a command of their own, inside a double-quoted string too, and at the close the
string and the command around them resume, so a word after the close is that command's (`is
"$(cut …)" gh` runs `is`); `(…)` is a subshell, whose commands are its own, and `((` opens
arithmetic only where bash reads it so, the `)` closing its inner `(` followed at once by `)`
(`((cmd) || true)` is two subshells), its parentheses counted as bash's matched-pair parse counts
them, none inside a quoted string (`((echo "))"; cmd) || true)` is two subshells too) nor in a
comment a `#` at word start begins inside a `$(…)` (`$(: # )` + newline + `cmd)` is one
substitution), where a `#` just after a quoted string or a substitution's `)` continues the word
(`$(echo $(echo y)#)`); an unquoted `;`, `&`, `|`, `(`, `)` or newline ends a
command, and a redirection's target (`>/dev/null`, `>> "$GITHUB_OUTPUT"`, `2>&1`) is not one of its
words. A here-document's body, from the newline after its `<<WORD` (or `<<-WORD`, whose body's
lines may start with tabs) to the line that is WORD (or, inside a `$(…)` or a backtick pair, WORD,
any blanks and the close, as bash 5.2 accepts: `EOF)`, `EOF )`, ``EOF` ``), is a script when a command of the line that writes it is a shell reading its
standard input, before the `<<` or after it, behind any of RUNNERS (`bash <<'EOT'`, `cat <<'EOT' |
bash`, `setsid bash <<'EOT'`, `bash /dev/stdin <<'EOT'`, `. /dev/stdin <<EOT`, `xargs -I{} bash -c
'{}'`: feeds_shell), and is read as one; so is a here-string (`bash <<< 'cmd'`) and the text an
echo or printf pipes straight into such a shell (`echo 'cmd' | bash`). Any other body is text, but
with an unquoted WORD bash expands its `$(…)` and backtick substitutions, which are read as the
commands they run; commands_of's `every_body`, the release check's rule, reads every body as
commands instead, in a string the text hands a shell (`bash -c '…'`, eval, a trap) too, and
every array's values as a command, as main's reader did, so a copy, a slice, an element or a
nameref of the array that runs them is read. Such a
string is read by the rules of the text that holds it, sharing its arrays and aliases, so an array
an eval or `bash -c` string assigns is the text's too (`eval 'a=(cmd)'`, then `"${a[@]}"`). Arithmetic (`$(( … ))`, `(( … ))`) runs no command but its substitutions
(`$(( $(cmd) ))` runs `cmd`). An array's values (`tools+=(curl jq)`), to the first `)` outside
their quotes and comments, are words of no command, but a substitution among them runs, `<(…)` and
`>(…)` too. A case pattern runs no command but its substitutions (`case x in "$(cmd)")` runs cmd),
from the `in` or the `;;`, `;&` or `;;&` before it to its unquoted `)`; nor does the name of a
function a `()` defines (`curl() {`).

A command word a shell builds rather than one written is read as what it will be only where the
text itself says what: `"${name[@]}"` of an array the text assigns is each list of values an
assignment gives it, wherever the assignment stands (a `+=` is a list of its own too; a numeric
element assignment `name[N]=value` gives each list with that element set; a value written
`[N]=value` is its value; negative, arithmetic and `+=` element indexes are listed limits); an
alias the text defines is each word-split text a definition gives it (whether or not
expand_aliases is set), and the first word of that text is read as an alias again, as bash does
(`alias c=b`); compound alias text and an alias after a reserved word are listed limits; a
`$'…'` word is decoded; env's `-S STRING` is STRING's words, and the `$@`, `$*`
and `${name[@]}` words env is handed are arguments of env, and so are their slices (`"${@:2}"`,
`"${a[@]:0}"`); `trap 'cmd' …` runs cmd; `function NAME {` and `coproc [NAME]` stand before the
command. Every assignment and definition is kept, whether or not bash would run it where it stands
(a trap that never fires, `if false`, an uncalled function, a `bash -c` child), so none shadows
another: this reads more, never less. Any other built word, a variable or a substitution in
command position (`"$tool" build`, `$cmd`, `"$@"`), is read as that word, not as a command this
reader can name.
.github/scripts/shell_reader_forms.py runs generated forms under bash and holds this reader to
them, and .github/shell-reader-misses.tsv lists the forms it knows it misses.

Each command sheds its leading `VAR=value` and `name[N]=value` assignments and the keywords that
precede a command (if, then, for, do, `{`, `}`, …), env, exec and time with their options, by name
or by a path under a system executable directory (`/usr/bin/env`, `/bin/`, `/usr/local/bin/`;
named_program), and the wrappers that run the words after them (`ci_log_guard` and
`ci_log_guard_stderr`, .github/scripts/ci-log-guard.sh's), and the first word left is the command;
an interpreter there, by name or by such a system path (one of INTERPRETERS: bash, sh, dash, zsh,
source, `.`),
is looked through, past its flags and the option name an `-o`, `+o`, `-O` or `+O` takes, to the
word after it (`bash -o pipefail scripts/x.sh` runs `scripts/x.sh`), and the script string behind
its `-c`, like the words behind `eval`, is scanned the same way (`sh -c 'bun install'`, `eval "bun
install"` and `eval bun install` run `bun`).
"""

import functools
import itertools
import re
from collections.abc import Container

EXPRESSION = re.compile(r"\$\{\{.*?\}\}")
ASSIGNMENT = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*(?:\[[^]]*\])?\+?=")
# An element assignment of an indexed array whose index is a plain number: `name[N]=value`.
ELEMENT_ASSIGNMENT = re.compile(r"([A-Za-z_][A-Za-z0-9_]*)\[([0-9]+)\]=(.*)", re.S)
# Any element assignment, `name[subscript]=value` or `name[subscript]+=value`, with its value: the
# subscript may be arithmetic (`a[1-1]=`), negative (`a[-2]=`) or a key, which this reader does not
# evaluate; under every_body the value is read as a command, since bash runs what it names.
ELEMENT_VALUE = re.compile(r"[A-Za-z_][A-Za-z0-9_]*\[[^]]*\]\+?=(.*)", re.S)
# The `[N]=` or `[key]=` before a value of an array written with its index (`a=([0]=cmd)`).
INDEXED_VALUE = re.compile(r"^\[[^]]*\]=")
# A program invoked by path is a runner or interpreter only under a system executable directory.
# A repository file named `scripts/env` or `out/sh` is its own command, not env or sh.
SYSTEM_PROGRAMS = ("/bin/", "/usr/bin/", "/usr/local/bin/")


def named_program(word: str, names: Container[str]) -> bool:
    """Whether `word` names one of `names`: exactly, or, under a system executable directory
    (SYSTEM_PROGRAMS: /bin/, /usr/bin/, /usr/local/bin/), by its last path part. A repository file
    named `scripts/env` or `out/sh` is its own command, never env or sh."""
    return word in names or (word.startswith(SYSTEM_PROGRAMS) and word.rsplit("/", 1)[-1] in names)


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
    command) or a here-string's, whether the command reads a pipe, the here-documents whose bodies
    start at the next newline, and, inside a `case`, the pattern being read. Before it reads a text
    that assigns an array or defines an alias, a first pass over the same text collects them
    (`arrays`, `aliases`), so a command that expands one reads as what it runs, wherever in the
    text the assignment stands."""

    def __init__(self) -> None:
        self.commands: list[list[str]] = []
        self.words: list[str] = []
        self.word = ""
        self.started = False
        self.at_word_start = True
        # False, True for a redirection's target (dropped), or "herestring" for a `<<<`'s word
        # (kept in `herestrings` until the line ends).
        self.redirect_target: bool | str = False
        # (delimiter, whether `<<-` strips its tabs, whether the delimiter is quoted) of each
        # here-document whose body starts at the next newline.
        self.heredocs: list[tuple[str, bool, bool]] = []
        self.herestrings: list[str] = []  # the word of each here-string on the line being read
        # The words of each command ended on the line being read, before command_of reads through
        # them, and whether each reads the one before it through a pipe: the newline decides from
        # them whether a shell of the line reads its here-documents, its here-strings, or the text
        # an `echo` or `printf` before it prints.
        self.line_words: list[list[str]] = []
        self.line_pipes: list[bool] = []
        self.pipe_in = False  # whether the command being read reads the one before it through `|`
        self.bodies: list[tuple[int, int]] = []  # (start, end) of each here-document body skipped
        self.line_quotes: dict[int, str] = {}  # index of each line that starts inside a quote -> it
        self.cases = 0  # the `case`s open
        self.pattern: str | None = None  # the case pattern being read, or None outside one
        # name -> each list of values an assignment of the text gives the array; name -> the text
        # an alias stands for. The first pass collects them (`collecting`); the text's own pass and
        # each scanner of a part of it (child) read them.
        self.arrays: dict[str, list[list[str]]] = {}
        self.aliases: dict[str, list[str]] = {}
        self.collecting = False
        self.prepared = False
        # Whether every here-document body is read as commands, whatever its line runs: the release
        # check's rule (commands_of's every_body), which reads more than bash may run, never less.
        self.every_body = False
        self.pattern_text = ""  # the text of the case pattern being read, as written

    def child(self) -> "Scanner":
        """A scanner of a part of this text (a substitution, a here-document's body, a string a
        shell reads), which reads the arrays and aliases of the whole text, by the same rules."""
        part = type(self)()
        part.arrays, part.aliases, part.collecting, part.prepared = self.arrays, self.aliases, self.collecting, True
        part.every_body = self.every_body
        return part

    def string_scanner(self) -> "Scanner":
        """A scanner of a string this text hands a shell to run (a `-c` script, eval's words, a
        trap's string): by the same rules, sharing the arrays and aliases of the whole text, so
        one the string assigns is the text's too (`eval 'a=(cmd)'`, then `"${a[@]}"`), as it is
        in the shell that runs an eval or a trap. Its first pass collects into those, so a text's
        own first pass, which reads the strings its commands hand a shell, collects them too."""
        part = type(self)()
        part.arrays, part.aliases = self.arrays, self.aliases
        part.every_body = self.every_body
        return part

    def add(self, char: str) -> None:
        self.word += char
        self.started = True
        self.at_word_start = False

    def end_word(self) -> None:
        if self.started:
            if self.redirect_target == "herestring":
                self.herestrings.append(self.word)
            elif not self.redirect_target:
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
            # `alias` past the keywords and assignments before it (`{ alias b=…; }`, `then alias …`).
            at = next((k for k, word in enumerate(self.words) if word not in SKIP_WORDS and not ASSIGNMENT.match(word)),
                      len(self.words))
            if self.collecting and self.words[at:at + 1] == ["alias"]:
                for definition in self.words[at + 1:]:
                    name, _, value = definition.partition("=")
                    if _ and name and value not in self.aliases.setdefault(name, []):
                        self.aliases[name].append(value)
            if self.collecting:
                # `a[0]=cmd`: each list an assignment gave the array, with that element set, is a list
                # of its own; the earlier lists stay, as the assignment may be one bash never runs.
                for word in self.words[:at]:
                    element = ELEMENT_ASSIGNMENT.fullmatch(word)
                    if element:
                        name, index, value = element.group(1), int(element.group(2)), element.group(3)
                        lists = self.arrays.setdefault(name, [])
                        for values in list(lists) or [[]]:
                            changed = values[:index] + [value] + values[index + 1:]
                            if changed not in lists:
                                lists.append(changed)
            if self.every_body and not self.collecting:
                # `a[N]=cmd`, `a[1-1]+=cmd`, `a[key]=cmd`: bash runs what the value names where the
                # element is later expanded, so every_body reads each element assignment's value as
                # a command, whatever the subscript (ELEMENT_ASSIGNMENT collects only plain indexes
                # into the array; this reaches the rest).
                for word in self.words[:at]:
                    value = ELEMENT_VALUE.fullmatch(word)
                    if value and value.group(1):
                        self.commands += command_of(value.group(1).split(), self)
            for words in self.expand(self.words):
                self.line_words.append(list(words))
                self.line_pipes.append(self.pipe_in)
                self.record(words)
        self.words = []
        self.redirect_target = False
        self.pipe_in = False

    def expand(self, words: list[str]) -> list[list[str]]:
        """The words a command runs: each `"${name[@]}"` or `"${name[*]}"` of an array the text
        assigns read as that array's values (one command for each list an assignment gives it),
        and an alias at command position, past the assignments before it, read as its text (one
        command for each definition the text gives it). Every assignment and definition is kept,
        wherever it stands and whether or not bash would run it there (a trap that never fires,
        `if false`, an uncalled function, a `bash -c` child): a reader of more than bash runs,
        never less, may not let one shadow another."""
        options: list[list[list[str]]] = []
        for word in words:
            ref = ARRAY_REFERENCE.fullmatch(word)
            options.append(self.arrays[ref.group(1)] if ref and self.arrays.get(ref.group(1)) else [[word]])
        expanded = [[w for part in choice for w in part] for choice in itertools.product(*options)]
        return [out for words in expanded for out in self.unalias(words, frozenset())]

    def unalias(self, words: list[str], seen: frozenset) -> list[list[str]]:
        """The words with the alias at command position, past the assignments before it, read as
        each text a definition gives it, and the first word of that text read as an alias again,
        but not one already being read (`alias c=b` runs b's text; `alias ls='ls -l'` stops)."""
        at = next((i for i, word in enumerate(words) if not ASSIGNMENT.match(word)), len(words))
        if at >= len(words) or words[at] not in self.aliases or words[at] in seen:
            return [words]
        return [out for value in self.aliases[words[at]]
                for out in self.unalias(words[:at] + value.split() + words[at + 1:], seen | {words[at]})]

    def record(self, words: list[str]) -> None:
        """Notes the command an ended simple command's words run (command_of), a string it hands a
        shell (`bash -c`, eval, trap) read by this scanner's rules and with its arrays and aliases."""
        self.commands += command_of(words, self)

    def suspend(self, placeholder: str) -> tuple:
        """The command a `$(…)`, a backtick pair or a process substitution interrupts, which takes
        `placeholder` into the word being read where the substitution stands; a command of the
        substitution's own starts inside it."""
        saved = (self.words, self.word + placeholder, self.redirect_target, self.pipe_in)
        self.words, self.word, self.started, self.at_word_start = [], "", False, True
        self.redirect_target, self.pipe_in = False, False
        return saved

    def resume(self, saved: tuple) -> None:
        """Ends the substitution's command, and reads on in the command it interrupted."""
        self.end_command()
        self.words, self.word, self.redirect_target, self.pipe_in = saved
        self.started, self.at_word_start = True, False

    def end_values(self, saved: tuple) -> None:
        """Ends an array's values, which are words of no command, noting them as the array's when
        collecting, and reads on in the assignment the array's `(` interrupted."""
        self.end_word()
        # `a=([0]=cmd [1]=arg)`: each value is the text after its `[N]=`.
        self.words = [INDEXED_VALUE.sub("", word, count=1) for word in self.words]
        assigned = ARRAY_TARGET.fullmatch(saved[1])
        if self.collecting and assigned:
            name, append = assigned.group(1), assigned.group(2)
            lists = self.arrays.setdefault(name, [])
            # `name+=(…)` extends each list an earlier assignment gave the array, and is a list of
            # its own too: the earlier assignment may be one bash never runs (a trap, `if false`,
            # an uncalled function, a child shell), and then the array holds only these values.
            if append:
                for values in lists:
                    values.extend(self.words)
            if self.words not in lists:
                lists.append(list(self.words))
        # The release check's rule (every_body) reads an array's values as a command too, as main's
        # reader did: a copy (`b=("${a[@]}")`), a slice (`"${a[@]:0}"`), an element (`${a[0]}`) or a
        # nameref (`declare -n r=a`) of the array can run them, and this reader expands none of those.
        if self.every_body and self.words and not self.collecting:
            self.commands += command_of(list(self.words), self)
        self.words, self.word, self.redirect_target, self.pipe_in = saved
        self.started, self.at_word_start = True, False

    def read_line_strings(self) -> None:
        """At a line's end: when a command of the line is a shell reading its standard input, each
        here-string of the line is its script, and so is the text an `echo` or `printf` piped
        straight into that shell prints (`echo 'curl …' | bash`); each is read as commands."""
        shells = [feeds_shell(words) for words in self.line_words]
        if self.herestrings and any(shells):
            for text in self.herestrings:
                self.commands += self.child().scan(text)
        for at, words in enumerate(self.line_words):
            if at and self.line_pipes[at] and shells[at]:
                for before in command_of(list(self.line_words[at - 1])):
                    printed = printed_text(before)
                    if printed:
                        self.commands += self.child().scan(printed)
        self.herestrings = []

    def end_line(self) -> None:
        self.line_words, self.line_pipes = [], []

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

    def skip_heredocs(self, text: str, i: int, closer: str = "") -> int:
        """Reads the body of each here-document pending, from `i`, the start of a line; a body no
        delimiter line ends runs to the end, as bash reads it, and inside a `$(…)` or a process
        substitution (`closer` `)`) or a backtick pair (`closer` a backtick) a line that is the
        delimiter, any blanks and the closer ends it too, as bash 5.2 accepts (`EOF)`, `EOF )`,
        ``EOF` ``), the closer then read as the substitution's close. When a command of the line that wrote
        them is a shell reading its standard input (`bash <<'EOT'`, `cat <<'EOT' | bash`, `setsid
        bash <<'EOT'`), or when the scanner reads every body (`every_body`), each body is commands,
        read as a script: the line is read whole, before the body, so the shell may stand after the
        `<<`. Any other body is text, noted in `bodies`, but bash expands the `$(…)` and backtick
        substitutions of a body whose delimiter is unquoted, so those are read as commands."""
        shell = self.every_body or any(feeds_shell(words) for words in self.line_words)
        close = None
        for delimiter, strip, quoted in self.heredocs:
            start = i
            while i < len(text):
                end = text.find("\n", i)
                line = text[i:] if end < 0 else text[i:end]
                after = len(text) if end < 0 else end + 1
                content = line.lstrip("\t") if strip else line
                if content == delimiter:
                    break
                ended = closer and re.match(re.escape(delimiter) + r"[ \t]*" + re.escape(closer), content)
                if ended:
                    close = i + len(line) - len(content) + ended.end() - 1
                    break
                i = after
            body = text[start:i]
            if shell:
                self.commands += self.child().scan(body)
            else:
                self.bodies.append((start, i))
                if not quoted:
                    self.scan_substitutions(body)
            if close is not None:
                i = close
                break
            if i < len(text):
                end = text.find("\n", i)
                i = len(text) if end < 0 else end + 1
        self.heredocs = []
        self.end_line()
        return i

    def scan_substitutions(self, text: str) -> None:
        """Reads each `$(…)` and backtick substitution of `text`, which is no command itself (an
        unquoted here-document's body, arithmetic), as the commands bash runs to expand it."""
        for inner in substitutions(text):
            self.commands += self.child().scan(inner)

    def read_pattern(self, text: str, i: int) -> int:
        """Reads the character at `i` of a case pattern list, and returns where reading resumes.
        The list ends at its unquoted `)`, and an `esac` in its place ends the case. A pattern is
        no command, so nothing in it is a word, but bash expands it, so each `$(…)` and backtick
        substitution in it is read as the commands it runs (`case x in "$(cmd)")` runs cmd)."""
        resume = self.pattern_char(text, i)
        if self.pattern is None:
            self.scan_substitutions(self.pattern_text)
            self.pattern_text = ""
        else:
            self.pattern_text += text[i:resume]
        return resume

    def pattern_char(self, text: str, i: int) -> int:
        """read_pattern's step over one character, or one quoted part, of the pattern."""
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
            self.pattern += "x"
            return double_quote_end(text, i)
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
        if not self.prepared:
            self.prepared = True
            if "=(" in text or "]=" in text or "alias" in text:
                first = type(self)()
                first.collecting = first.prepared = True
                first.every_body = self.every_body
                first.arrays, first.aliases = self.arrays, self.aliases
                first.scan(text)
                self.arrays, self.aliases = first.arrays, first.aliases
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
                # `$((…))` always takes quotes but never a shell comment. An arithmetic one is a
                # value whose substitutions run; a non-arithmetic one is a `$(` command substitution
                # of a subshell, so the `$` stays in the word and a child reads its inner commands.
                if is_arithmetic(text, i + 1):
                    self.add(char)
                    end = arithmetic_end(text, i + 1)
                    self.scan_substitutions(text[i + 3:end - 2])
                else:
                    inner, end = nonarithmetic_substitution(text, i)
                    self.add(char)
                    self.commands += self.child().scan(inner)
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
                # An ANSI-C string, `$'…'`: its escapes decoded as bash decodes them (`$'\x63url'`
                # is `curl`), each line it spans noted as starting inside it.
                end = ansi_c_end(text, i + 2)
                line = text.count("\n", 0, i)
                for at in range(i + 2, end):
                    if text[at] == "\n":
                        line += 1
                        self.line_quotes[line] = "$'"
                self.add(ansi_c(text[i + 2:end]))
                i = end + 1
                continue
            elif char in "'\"":
                quote = char
                self.started = True
                self.at_word_start = False
            elif char == "(" and after == "(" and self.at_word_start and not self.words and is_arithmetic(text, i):
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
                # A here-string's word (`<<<`) is kept: a shell of the line reads it as its script.
                self.redirect_target = "herestring" if text[i:i + 3] == "<<<" else True
                while text[i + 1:i + 2] in ("<", ">", "&", "|"):  # `>>`, `2>&1`, `<<<`, `>|`
                    i += 1
            elif char in "\n;&|" and open_subs and open_subs[-1][0] == "array":
                self.end_word()  # a newline between an array's values; bash refuses `;`, `&` and `|`
            elif char == "\n":
                self.end_command()
                self.read_line_strings()
                if self.heredocs:
                    # The innermost substitution open, whose close can end a body (skip_heredocs).
                    inner = next((kind for kind, _, saved in reversed(open_subs)
                                  if kind == "tick" or (kind == "paren" and saved is not None)), "")
                    i = self.skip_heredocs(text, i + 1, {"tick": "`", "paren": ")"}.get(inner, ""))
                    continue
                self.end_line()
            elif char == ";" and self.cases and after in (";", "&"):
                # `;;`, `;&` or `;;&` ends a case item, and the next item's patterns follow.
                self.end_command()
                i += 1 + (after == ";" and text[i + 2:i + 3] == "&")
                self.pattern = ""
            elif char == "|" and after == "|":
                self.end_command()
                i += 1
            elif char == "|":
                # A pipe: the next command reads this one's output, `|&` its errors too.
                self.end_command()
                self.pipe_in = True
                i += after == "&"
            elif char in ";&":
                self.end_command()
            elif char in " \t":
                self.end_word()
            else:
                self.add(char)
            i += 1
        self.end_command()
        self.read_line_strings()
        self.end_line()
        return self.commands


# The `()` after a function's name, with any blanks inside it.
FUNCTION_PARENS = re.compile(r"\([ \t]*\)")
# The word before an array's `(`: `name=`, `name+=`, `name[i]=`.
ARRAY_ASSIGNMENT = re.compile(r"[A-Za-z_][A-Za-z0-9_]*(?:\[[^]]*\])?\+?=")
# The assignment word an array's values interrupt, as suspend leaves it: its name, and `+` for `+=`.
ARRAY_TARGET = re.compile(r"([A-Za-z_][A-Za-z0-9_]*)(?:\[[^]]*\])?(\+?)=\(…\)")
# A word that expands to an array's values, once its quotes are taken off: `${name[@]}`, `${name[*]}`.
ARRAY_REFERENCE = re.compile(r"\$\{([A-Za-z_][A-Za-z0-9_]*)\[[@*]\]\}")


def ansi_c_end(text: str, i: int) -> int:
    """The index of the `'` that closes the ANSI-C string whose text starts at `i`, a backslash
    escaping the character after it, or the text's end."""
    while i < len(text):
        if text[i] == "\\":
            i += 2
        elif text[i] == "'":
            return i
        else:
            i += 1
    return len(text)


ANSI_C_SIMPLE = {"a": "\a", "b": "\b", "e": "\x1b", "E": "\x1b", "f": "\f", "n": "\n", "r": "\r", "t": "\t",
                 "v": "\v", "\\": "\\", "'": "'", '"': '"', "?": "?"}
ANSI_C_ESCAPE = re.compile(r"\\(?:([0-7]{1,3})|x([0-9A-Fa-f]{1,2})|u([0-9A-Fa-f]{1,4})|U([0-9A-Fa-f]{1,8})|c(.)|(.))",
                           re.S)


def ansi_c(text: str) -> str:
    """The text of an ANSI-C string's body, its escapes decoded as bash's `$'…'` decodes them: the
    named ones, octal `\\NNN`, hex `\\xHH`, `\\uHHHH`, `\\UHHHHHHHH` and control `\\cX`; any other
    backslash stays with its character."""
    def decode(match: re.Match) -> str:
        octal, hexa, short, long, control, other = match.groups()
        if octal:
            return chr(int(octal, 8) & 0xFF)
        if hexa or short or long:
            return chr(min(int(hexa or short or long, 16), 0x10FFFF))
        if control:
            return chr(ord(control.upper()) ^ 0x40)
        return ANSI_C_SIMPLE.get(other, "\\" + other)
    return ANSI_C_ESCAPE.sub(decode, text)


def printed_text(words: list[str]) -> str:
    """The text an `echo` or `printf` command prints, as a shell after a pipe would read it as its
    script, or '' for any other command: echo's words after its -n, -e and -E; printf's arguments
    one a line when its format holds a `%`, else its format with `\\n` read as a newline."""
    if words[:1] == ["echo"]:
        rest = words[1:]
        while rest and re.fullmatch(r"-[neE]+", rest[0]):
            rest = rest[1:]
        return " ".join(rest)
    if words[:1] == ["printf"] and len(words) > 1:
        return "\n".join(words[2:]) if "%" in words[1] and len(words) > 2 else words[1].replace("\\n", "\n")
    return ""


@functools.lru_cache(maxsize=4096)
def is_arithmetic(text: str, i: int) -> bool:
    """Whether the `((` at `i` opens arithmetic, as bash decides it: the `)` that closes its inner
    `(` is followed at once by a `)`. Otherwise it is a subshell in a subshell, or a command
    substitution of one (`((cmd) || true)`, `$((cmd) | sort)`, `((echo "))"; cmd) || true)`)."""
    inner = arithmetic_end(text, i + 1)
    return text[inner:inner + 1] == ")"


def nonarithmetic_substitution(text: str, i: int):
    """A `$((` at `i` (`text[i]` is `$`) that is not arithmetic is a `$(` command substitution whose
    content opens with a subshell (`$((cmd) | sort)`, `$((echo 1) # x\\n) …)`). Returns its inner
    text, between the `$(` and the `)` that closes it, and the index just past that `)`, so a caller
    reads the inner as the commands it runs; returns None for an arithmetic `$((…))` or other text."""
    if text[i:i + 3] != "$((" or is_arithmetic(text, i + 1):
        return None
    end = arithmetic_end(text, i + 1, commands=True)
    return text[i + 2:end - 1], end


@functools.lru_cache(maxsize=4096)
def arithmetic_end(text: str, i: int, commands: bool = False) -> int:
    """Just past the `)` that closes the `(` at `i` (for a `((`, past its `))`), counting the
    parentheses between as bash's matched-pair parse does: none inside a `'…'` or `"…"` string
    (double_quote_end) or after a backslash. With `commands`, the parentheses hold commands (a
    `$(…)`, or a subshell inside one), where a `#` at the start of a word starts a comment to the
    end of its line, as bash's parse of a command substitution reads it (`$(: # )` + newline +
    `cmd)` is one substitution); a `$((…))` always has `commands=False`, as bash reads it."""
    depth = 0
    # Whether the next character starts a word: after the `(` itself, a blank, a newline or an
    # operator (`;`, `&`, `|`, `<`, `>`, and a `(` or `)` that opens or closes a subshell). A
    # quoted string, a backslash, a substitution `$(…)`, `<(…)` or `>(…)` and an arithmetic
    # expansion `$((…))` are parts of a word, so a `#` just after one of those continues the word
    # (`$(echo y)#`, `$((1))#`), as bash reads it.
    word_start = True
    while i < len(text):
        char = text[i]
        if char == "\\":
            i += 2
            word_start = False
            continue
        if char == "'":
            end = text.find("'", i + 1)
            i = len(text) if end < 0 else end + 1
            word_start = False
            continue
        if char == '"':
            i = double_quote_end(text, i)
            word_start = False
            continue
        if char in "$<>" and text[i + 1:i + 2] == "(":
            # An arithmetic expansion holds no commands, so no `#` in it starts a comment. Read a
            # non-arithmetic `$((` the same way to find its matching close; Scanner scans its text.
            i = arithmetic_end(text, i + 1, commands=not (char == "$" and text[i + 2:i + 3] == "("))
            word_start = False
            continue
        if commands and char == "#" and word_start:
            end = text.find("\n", i)
            i = len(text) if end < 0 else end
            continue
        if char == "(":
            depth += 1
        elif char == ")":
            depth -= 1
            if depth == 0:
                return i + 1
        word_start = char in " \t\n;&|()<>"
        i += 1
    return i


def double_quote_end(text: str, i: int) -> int:
    """Just past the `"` that closes the double-quoted string at `i`: a backslash escapes the
    character after it, and a `$(…)` or a backtick pair inside opens text of its own, whose quotes
    are its own (`"$(echo ")")"` is one string)."""
    i += 1
    while i < len(text):
        char = text[i]
        if char == "\\":
            i += 2
        elif char == '"':
            return i + 1
        elif char == "$" and text[i + 1:i + 2] == "(":
            i = arithmetic_end(text, i + 1, commands=text[i + 2:i + 3] != "(")
        elif char == "`":
            end = text.find("`", i + 1)
            i = len(text) if end < 0 else end + 1
        else:
            i += 1
    return i


def substitutions(text: str) -> list[str]:
    """The text inside each outermost `$(…)` (a non-arithmetic `$((` among them, which is a `$(`
    command substitution of a subshell: nonarithmetic_substitution) and backtick pair of `text`,
    which bash expands in an unquoted here-document's body, in arithmetic and in a case pattern.
    Outside a substitution a quote there is a character, not a quote; inside one, the substitution's
    own quotes hold its parentheses, as bash's parse of it does (arithmetic_end)."""
    found, i = [], 0
    while i < len(text):
        if text[i] == "\\":
            i += 2
            continue
        if text.startswith("$((", i):
            sub = nonarithmetic_substitution(text, i)
            if sub:
                inner, end = sub
                found.append(inner)
                i = end
            else:  # an arithmetic `$((…))` runs no command of its own; its own substitutions do
                i += 2
            continue
        if text.startswith("$(", i):
            end = arithmetic_end(text, i + 1, commands=True)
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
# reads a client through every one; feeds_shell reads a shell through every one but xargs, whose
# command reads no standard input, which xargs itself reads, but with -I and a shell `-c` script
# holding the replacement token runs each line it reads as that script (xargs_runs_lines).
RUNNERS = {
    "sudo": ({"-u", "-g", "-C", "-h", "-p", "-U", "-r", "-t", "-D", "-R", "--user", "--group", "--chdir",
              "--host", "--prompt", "--other-user", "--role", "--type", "--close-from"}, 0),
    "timeout": ({"-s", "--signal", "-k", "--kill-after"}, 1),
    "nice": ({"-n", "--adjustment"}, 0),
    "nohup": (set(), 0),
    "stdbuf": ({"-i", "-o", "-e", "--input", "--output", "--error"}, 0),
    "xargs": ({"-n", "-L", "-P", "-I", "-s", "-d", "-E", "-a", "--max-args", "--max-lines", "--max-procs",
               "--replace", "--delimiter", "--eof", "--arg-file", "--max-chars"}, 0),
    "env": ({"-u", "--unset", "-C", "--chdir"}, 0),
    "exec": ({"-a"}, 0),
    "command": (set(), 0),
    "time": (set(), 0),
    "flock": ({"-w", "--wait", "--timeout", "-E", "--conflict-exit-code"}, 1),
    "ionice": ({"-c", "--class", "-n", "--classdata", "-p", "--pid", "-P", "--pgid", "-u", "--uid"}, 0),
    "setsid": (set(), 0),
    # taskset and chrt take their CPU mask or list, and their priority, before the command.
    "taskset": (set(), 1),
    "chrt": ({"-T", "--sched-runtime", "-P", "--sched-period", "-D", "--sched-deadline"}, 1),
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
    off `words` in place, and returns its name. env's `-S STRING` (`--split-string`) puts the words
    of STRING in their place, as env does (`env -S 'curl http://x'` runs curl)."""
    runner = words.pop(0).rsplit("/", 1)[-1]
    valued, plain = RUNNERS[runner]
    while words and words[0].startswith("-") and words[0] != "-":
        flag = words.pop(0)
        split = None
        if runner == "env" and flag in ("-S", "--split-string") and words:
            split = words.pop(0)
        elif runner == "env" and flag.startswith("--split-string="):
            split = flag.split("=", 1)[1]
        elif runner == "env" and flag.startswith("-S"):
            split = flag[2:]
        if split is not None:
            words[:0] = split.split()
        elif flag in valued and words:
            words.pop(0)
    # env's NAME=value words, and a word that is wholly `$@`, `$*` or an array's `${name[@]}`: env
    # takes the words such a word expands to as more of them (`env "${unset[@]}" "$@" bash x.sh`).
    while runner == "env" and words and (ASSIGNMENT.match(words[0]) or WHOLE_EXPANSION.fullmatch(words[0])):
        words.pop(0)
    del words[:plain]
    return runner


def xargs_runs_lines(words: list[str]) -> bool:
    """Whether `xargs …` (words from xargs on) runs each line it reads as a shell's script: given
    -I TOKEN (or -i / --replace, whose token is `{}`), its command is a shell whose `-c` script
    holds TOKEN, as `xargs -I{} bash -c '{}'` does."""
    token, rest = None, words[1:]
    while rest and rest[0].startswith("-") and rest[0] != "-":
        flag = rest.pop(0)
        if flag == "-I" and rest:
            token = rest.pop(0)
        elif flag.startswith("-I"):
            token = flag[2:]
        elif flag.startswith("--replace="):
            token = flag.split("=", 1)[1]
        elif flag in ("-i", "--replace"):
            token = "{}"
        elif flag in RUNNERS["xargs"][0] and rest:
            rest.pop(0)
    if not token or not rest or not named_program(rest[0], STDIN_SHELLS):
        return False
    for at, word in enumerate(rest[1:], 1):
        if word.startswith("-") and not word.startswith("--") and "c" in word[1:]:
            return at + 1 < len(rest) and token in rest[at + 1]
    return False


# The shells whose standard input is a script when they are given no script and no `-c` (or `-s`),
# and the paths that name a process's standard input, which a shell or `.` given one reads.
STDIN_SHELLS = {"bash", "sh", "dash", "zsh"}
STDIN_PATHS = {"/dev/stdin", "/dev/fd/0", "/proc/self/fd/0"}


def feeds_shell(words: list[str]) -> bool:
    """Whether the command `words` runs is a shell reading its standard input as its script, so a
    here-document, a here-string or a piped echo its line writes is commands: bash, sh, dash or
    zsh, past the assignments and keywords before it and each of RUNNERS with its options, given
    no script, `-s`, or a path of STDIN_PATHS as its script, and no `-c`; `.` or `source` of such a
    path; or xargs running each line as a shell's `-c` script (xargs_runs_lines). As in `bash
    <<'EOT'`, `setsid bash <<'EOT'`, `timeout --signal KILL 5m bash <<'EOT'`, `sudo bash -s <<EOT`,
    `bash /dev/stdin <<'EOT'`, `. /dev/stdin <<EOT` and `xargs -I{} bash -c '{}'`."""
    words = list(words)
    while words:
        if named_program(words[0], {"xargs"}):
            return xargs_runs_lines(words)
        if named_program(words[0], RUNNERS):
            strip_runner(words)
        elif ASSIGNMENT.match(words[0]) or words[0] in SKIP_WORDS:
            words.pop(0)
        else:
            break
    if not words:
        return False
    if words[0] in ("source", ".") and words[1:2] and words[1] in STDIN_PATHS:
        return True
    if not named_program(words[0], STDIN_SHELLS):
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


# A word that is wholly an expansion of many words: `$@`, `$*`, `${@}`, `${name[@]}`, `${name[*]}`,
# `${!name[@]}`, and an operator form such as `set -u`'s `${name[@]+"${name[@]}"}`, its quotes taken
# off as the scanner takes them.
WHOLE_EXPANSION = re.compile(r"\$[@*]|\$\{[@*](?::[^{}]*)?\}|\$\{!?[A-Za-z_][A-Za-z0-9_]*\[[@*]\]"
                             r"(?::?[-+=?](?:[^{}]|\$\{[^{}]*\})*|:[^{}]*)?\}")


def commands_of(run: str, every_body: bool = False, within: "Scanner | None" = None) -> list[list[str]]:
    """Each simple command of a `run:`, as its words with the command first. With `every_body`
    (check-release-credentials.sh's rule), every here-document body is read as commands, whatever
    its line runs, so a body that reaches a shell by any route the reader does not follow
    (`eval "$(cat <<'EOT' …)"`, `. <(cat <<'EOT' …)`, a runner it does not know) is read too. A
    string that a text `within` hands a shell is read by that text's scanner's rules
    (string_scanner)."""
    scanner = within.string_scanner() if within is not None else Scanner()
    scanner.every_body = scanner.every_body or every_body
    return scanner.scan(EXPRESSION.sub("EXPR", run))


# A shell name: a variable's, a function's or a coprocess's.
NAME = re.compile(r"[A-Za-z_][A-Za-z0-9_]*")


def shed_prefix(words: list[str], keywords: frozenset | set = frozenset(), runners: tuple = ("env", "exec", "time")) -> None:
    """Takes off `words`, in place, what stands before the command: `VAR=value` assignments, the
    words of `keywords` (SKIP_WORDS when none are given), each of `runners` with its options, by
    name or by a path under /bin, /usr/bin or /usr/local/bin (named_program, strip_runner), `function
    NAME` (a body on its line follows), and coproc with the NAME a
    compound command after it takes."""
    keywords = keywords or SKIP_WORDS
    while words:
        if named_program(words[0], runners):
            strip_runner(words)
        elif ASSIGNMENT.match(words[0]) or words[0] in keywords:
            words.pop(0)
        elif words[0] == "function" and len(words) > 1:
            del words[:2]
        elif words[0] == "coproc":
            words.pop(0)
            if len(words) > 1 and NAME.fullmatch(words[0]) and words[1] in ("{", "(", "while", "until", "if", "for"):
                words.pop(0)
        else:
            break



def command_of(words: list[str], within: "Scanner | None" = None) -> list[list[str]]:
    """The command the words run, after the assignments and keywords before it (env, exec and time
    with their options, as strip_runner reads them; `function NAME`, which a body on the same line
    follows; coproc, with the NAME a compound command after it takes): the words from the command
    on; or, when the command is an interpreter, the command it is handed (`bash -o pipefail
    scripts/x.sh` runs `scripts/x.sh`), or each command of the script string behind its `-c`
    (`sh -c 'bun install && go build'` runs `bun install` and `go build`); or, when it is `eval`,
    each command of the words after it, joined (`eval "bun install"` and `eval bun install` both
    run `bun install`); or, when it is `trap`, each command of the string it sets (`trap 'curl …'
    EXIT` runs curl when the shell exits). Each such string is read by the rules, the arrays and
    the aliases of the scanner `within`, the one reading the text that holds it."""
    shed_prefix(words)
    if not words:
        return []
    if words[0] == "trap":
        rest = words[1:]
        if rest[:1] == ["--"]:
            rest = rest[1:]
        return commands_of(rest[0], within=within) if rest and not rest[0].startswith("-") else []
    if words[0] == "eval":
        return commands_of(" ".join(words[1:]), within=within)
    if not named_program(words[0], INTERPRETERS):
        return [words]
    words.pop(0)
    while words and words[0].startswith(("-", "+")):
        flag = words.pop(0)
        if flag.startswith("--"):
            continue
        if "c" in flag:
            return commands_of(words[0], within=within) if words else []
        if flag.endswith(("o", "O")) and words:
            words.pop(0)
    return command_of(words, within)
