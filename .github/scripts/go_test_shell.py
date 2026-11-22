"""How rule 8 of .github/scripts/check-workflow-files.sh (.github/scripts/go_test_steps.py) reads
shell: a script's lines as the shell reads them (logical_lines), where a word stands as the command
it runs (COMMAND_PLACE, the one grammar every pattern of the rule is built on), the go test and the
script runs at those places (GO_TEST, RUNS, go_tests), the compound commands each line stands in
(Frame), and whether such a command runs detached, so its step can end before it has written its
events (detached_at). go_test_steps.py imports it from beside its own file; what it reads is held
to bash by .github/scripts/go_test_step_forms.py.
"""

from __future__ import annotations

import re

from workflow_paths import EVENTS_WRITER

# The writer is named by its repository path at the end of the word, whatever directory a step puts
# before it (`../../`, or a variable's `"$envoy/../../"`), quoted or not; race_memory.py may stand
# between it and the go test it samples, since it appends what it reads to the file the writer
# names.
WRITER = EVENTS_WRITER

# Where a shell word stands as the command it runs, the one grammar of this rule (COMMAND_PLACE):
# a command's start, then any run of the reserved words a command may stand after (RESERVED), then
# past a run of assignments (a value quoted or not), of redirections (REDIRECT) and of the commands
# that run the command after their own options and operands (PREFIX). A command's start is a
# line's start, or after an operator (`;`, `&`, `|`, `(`, `$(`, a backquote), a case pattern's `)`
# (CASE_ARM: after `case <word> in`, after `;;`, `;&` or `;;&`, or at a line's start), a function's
# `name()` or `function name` (FUNCTION, its body's `{` after it), or find's -exec, -execdir, -ok or
# -okdir. A word anywhere else - an argument - runs nothing, and neither does one inside quotes (an
# echo's text, a fixture's string) or a heredoc of text, which unquoted() and logical_lines() leave
# out.
# A value holding `$(` or a backquote is no plain word: the command it starts is read at its own
# place (`pattern=$(.github/scripts/go-tagged-tests.sh ...)`), so the assignment does not take it.
CASE_WORD = r"(?:\"[^\"]*\"|'[^']*'|[^\s\"';&|()`])+"
CASE_ARM = rf"(?:(?:^|;;&?|;&|\bcase\s+{CASE_WORD}\s+in)\s*\(?\s*{CASE_WORD}(?:\s*\|\s*{CASE_WORD})*\s*\))"
OPERATOR = r"(?:^|[;&|(`]|\$\()"
FIND_EXEC = r"(?:\s-(?:exec|execdir|ok|okdir)(?=\s))"
FUNCTION = r"(?:\(\s*\)|\bfunction\s+[A-Za-z_][\w-]*(?:\s*\(\s*\))?)(?=\s*\{)"
RESERVED = r"(?:!|\{|then|do|else|elif|if|while|until)(?=\s)"
START = rf"(?:{CASE_ARM}|{FIND_EXEC}|{FUNCTION}|{OPERATOR})(?:\s*{RESERVED})*"
ASSIGNMENT = r"[A-Za-z_]\w*=(?:\"[^\"]*\"|'[^']*'|(?![^\s]*(?:\$\(|`))[^\s\"'`]\S*)?"
# A word an option takes, or timeout's duration: quoted, or a run of characters no operator ends.
VALUE = r"(?:\"[^\"]*\"|'[^']*'|[^\s\"';&|()`]+)"
# A redirection, which may stand before a command as well as after it (`>/dev/null go test`,
# `2>&1 go test`, `</dev/null go test`, `<<<word go test`, `<<EOF go test`): bash runs the command
# after it.
REDIRECT = rf"(?:\d*(?:<<<|<<-?|<>|>>|>\||>&|<&|[<>])|&>>?)\s*{VALUE}"


def prefix(name: str, bare: str = "", valued: str = "", attached: str = "", long_valued: tuple = (),
           then: str = "") -> str:
    """A command that runs the command after its own words: its name, with a directory before it or
    not, then any of its options, then `then` (timeout's duration). An option is a cluster of the
    letters `bare`, which take no value, ending or not in one letter of `valued`, whose value is
    the rest of the word or the next word, or in one of `attached`, whose value is only the rest
    of the word (xargs's -i, -e and -l); a long option of `long_valued`, with `=` or the next word
    for its value; any other long option, with its `=` value or none, but --help and --version,
    after which the command prints and runs nothing, and env's --split-string, whose command
    ENV_SPLIT reads; or `--` or `-` alone."""
    short = []
    if valued:
        short.append(rf"-[{bare}]*[{valued}]\s*{VALUE}")
    if attached:
        short.append(rf"-[{bare}]*[{attached}][^\s\"';&|()`]*")
    if bare:
        short.append(rf"-[{bare}]+")
    long = [rf"--(?:{'|'.join(long_valued)})(?:=|\s+){VALUE}"] if long_valued else []
    other_long = rf"--(?!(?:help|version|split-string)(?![\w-]))[A-Za-z][\w-]*(?:={VALUE})?"
    options = "|".join(short + long + [other_long, r"--?(?=\s)"])
    return rf"(?:[^\s\"';&|()`]*/)?{name}(?:\s+(?:{options}))*{then}"


# The commands a step runs another command behind, each read past every option its manual lists
# that still runs the command: GNU coreutils' timeout, nice, env, nohup and setsid, sudo's, GNU
# findutils' xargs, and bash's exec, command and time. An option after which the command runs
# nothing is left out, so what follows it is no run: --help and --version, sudo's -e, -K, -l, -V
# and -v, and command's -v and -V. env's -S (--split-string) is left out too: its string, and the
# words after it, are the command, which ENV_SPLIT reads as a line of its own. setsid's -f runs the
# command detached, which FORKED refuses unless -w waits for it. A command behind any other runner
# is not read: the header of check-workflow-files.sh names them, and go-test-step-misses.tsv says
# why for each.
PREFIX = "(?:" + "|".join([
    prefix("timeout", bare="fpv", valued="ks", long_valued=("kill-after", "signal"), then=rf"\s+{VALUE}"),
    prefix("nice", bare="0-9", valued="n", long_valued=("adjustment",)),
    prefix("sudo", bare="AbBEHiknPSs", valued="CDghpRrTtUu",
           long_valued=("chdir", "chroot", "close-from", "command-timeout", "group", "host", "other-user",
                        "prompt", "role", "type", "user")),
    prefix("xargs", bare="0oprtx", valued="adEILnPs", attached="eil",
           long_valued=("arg-file", "delimiter", "max-args", "max-chars", "max-procs", "process-slot-var")),
    prefix("env", bare="0iv", valued="Cu", long_valued=("chdir", "unset")),
    prefix("nohup"),
    prefix("setsid", bare="cfw"),
    prefix("exec", bare="cl", valued="a"),
    prefix("command", bare="p"),
    prefix("time", bare="p"),
]) + ")"
COMMAND_PLACE = START + rf"\s*(?:(?:{ASSIGNMENT}|{REDIRECT}|{PREFIX})\s+)*"
# .github/scripts/harness-pool.sh runs the command its `start` is handed after the harness's name
# (and its --minutes), so that command stands at a command's place too. `start` returns while the
# command runs, so its events are all written only once a later step's `harness-pool.sh wait` has
# returned: the job's upload must come after that wait (POOL_WAIT).
POOLED = r"(?P<pooled>[\"']?[^\s\"';&|()]*harness-pool\.sh[\"']?\s+start\s+(?:--minutes(?:\s+|=)\S+\s+)?\S+\s+)?"
POOL_WAIT = re.compile(COMMAND_PLACE + r"(?P<pool>[\"']?[^\s\"';&|()]*harness-pool\.sh[\"']?\s+wait)\b")
EVENTS_PLACE = COMMAND_PLACE + POOLED + rf"(?:(?:{ASSIGNMENT}|{REDIRECT}|{PREFIX})\s+)*"
# A command word, quoted or not: `arg` starts at its opening quote, which stands outside the
# string it opens, so a quoted path at a command's place is read as the command it is. A
# `$(dirname ...)` in it (`"$(dirname "$0")/inner.sh"`) is part of the word, which
# go_test_steps.py's resolve() reads by its file name as it reads every word the shell builds; any
# other `$(` ends the word, since the command it starts is read at its own place.
# A backquote ends the word too: it opens a command of its own (`bash -c "`scripts/unit.sh`"`).
DIRNAME = r"\$\(dirname\b(?:[^()\"'`]|\"[^\"]*\"|'[^']*')*\)"
WORD = rf"(?P<arg>[\"']?(?P<word>(?:{DIRNAME}|[^\s\"';&|()`])+)[\"']?)"
# The writer, run as a command or by python3, with race_memory.py (which runs the command it is
# handed and appends what it reads to the writer's file) after it or not.
PATH_WORD = r"[\"']?[^\s\"';&|()]*"
SAMPLER = rf"(?:{PATH_WORD}race_memory\.py[\"']?\s+)?"
WRAPPER = rf"(?:python3?\s+)?[\"']?(?:[^\s\"';&|()]*/)?{re.escape(WRITER)}[\"']?\s+{SAMPLER}"
# go test's command word: go, quoted or not; a variable, braced or not, quoted or not (a command
# held in one, `"${go_cmd[@]}" test` or `"$GO" test`, is read as a go, since the word after it is
# go test's: a script that builds its go command's environment that way runs go test through it);
# or a path whose last part is go (`/usr/local/go/bin/go`, `"$GOROOT/bin/go"`,
# `"$(go env GOROOT)/bin/go"`), quoted or not.
GO_WORD = (r"(?P<go>[\"']?go[\"']?|\"?\$\{[A-Za-z_]\w*(?:\[@\])?\}\"?|\"?\$[A-Za-z_]\w*\"?"
           r"|\"[^\"=]*/go\"|'[^'=]*/go'|[^\s\"';&|()`=]*/go)")
# A go test, the writer before it or not; `cmd` is the command alone, from its first word to the
# end of its flags, and `go` its go word, with go's -C before test or not. `test` is a word of its
# own: `${line/go test/...}` holds no go test.
GO_TEST = re.compile(EVENTS_PLACE + rf"(?P<cmd>(?P<wrapper>{WRAPPER})?{SAMPLER}{GO_WORD}\s+"
                     r"(?:-C(?:\s+|=)\S+\s+)?test(?=[\s;&|)`]|$)(?P<rest>[^;&|)`]*))")
# A shell's options before its script, its string or its heredoc (`options`): a cluster of
# letters, -o or -O and the option it names (with + for either), or a long option (`--noprofile`,
# `--posix`). Whether the shell then runs the script is shell_runs()'s to say: some options print
# and exit, and noexec reads the script and runs none of it, unless an option after it undoes it.
NO_RUN_LONG = r"(?:help|version|dump-strings|dump-po-strings|rpm-requires)(?![\w-])"
SHELL_OPTION = r"(?:[-+][A-Za-z]*[oO]\s+[\w-]+|--[A-Za-z][\w-]*|[-+][A-Za-z]+)"
SHELLS = r"(?:bash|sh|dash|zsh|ksh)"
# A command that runs a file: the file as the command, or the file a shell (its options before
# it), python3, source or `.` runs; the writer before it or not.
RUNS = re.compile(EVENTS_PLACE + rf"(?P<wrapper>{WRAPPER})?"
                  rf"(?:(?:{SHELLS}(?P<options>(?:\s+{SHELL_OPTION})*)|python3?(?:\s+-\w+)*|source|\.)\s+)?{WORD}")
# A go test -json's own line that tees what it writes into the writer's file itself, as the
# counters do: `| tee -a "$TEST_EVENTS_FILE"`, `"${TEST_EVENTS_FILE}"` or `"${TEST_EVENTS_FILE:-/dev/null}"`.
TEES = re.compile(r"^\s*\|\s*tee\s+(?:-a|--append)\s+(?:\"\$\{?TEST_EVENTS_FILE(?::-[^}\"]*)?\}?\""
                  r"|\$\{?TEST_EVENTS_FILE(?::-[^}\s]*)?\}?)(?=\s|$)")
# A step's own assignment of the writer's file, at a command's place, behind export, declare,
# local or readonly or not.
ASSIGNS = re.compile(COMMAND_PLACE + r"(?:(?:export|declare|local|readonly)(?:\s+-\w+)*\s+)?(?P<name>TEST_EVENTS_FILE)=")
HEREDOC = re.compile(r"<<-?\s*(?P<quote>[\"']?)(?P<delimiter>[A-Za-z_]\w*)(?P=quote)")
# A heredoc a shell takes as its script (`bash <<EOF`, `bash -o pipefail <<'EOF'`, `env bash
# <<EOF`), and a script a shell takes as a string (`bash -c '...'`, `bash -o pipefail -c '...'`,
# `bash --noprofile -ec '...'`), the shell standing at a command's place. The string is the shell
# word after -c, its quoting read as the shell reads it (shell_word).
SHELL_AT = COMMAND_PLACE + rf"(?P<shell>(?:\S*/)?{SHELLS})"
SHELL_HEREDOC = re.compile(SHELL_AT + rf"(?P<options>(?:\s+{SHELL_OPTION})*)\s*<<$")
SHELL_STRING = re.compile(SHELL_AT + rf"(?P<options>(?:\s+{SHELL_OPTION})*?\s+-[A-Za-z]*c[A-Za-z]*)\s+(?=\S)")
# env's -S (--split-string) splits its string into the command and its first words, and appends
# the words after the string: `env -S "go test -v" ./x` runs `go test -v ./x`. Its string (quoted,
# or one word) and those words are read as a line of their own, as a shell's string is.
ENV_OPTION = (rf"(?:-[0iv]+|-[0iv]*[Cu]\s*{VALUE}|--(?:chdir|unset)(?:=|\s+){VALUE}"
              rf"|--(?!(?:split-string|help|version)(?![\w-]))[A-Za-z][\w-]*(?:={VALUE})?)")
ENV_SPLIT = re.compile(COMMAND_PLACE + rf"(?P<env>(?:[^\s\"';&|()`]*/)?env)(?:\s+{ENV_OPTION})*\s+"
                       r"(?:-[0iv]*S\s*|--split-string(?:=|\s+))(?=\S)")
# setsid's -f (--fork) runs the command in a process of its own and returns at once, unless -w
# (--wait) waits for it: the step, and so the job's upload, can end before its events are written.
FORKED = re.compile(r"(?:^|[\s/])setsid(?P<options>(?:\s+(?:-[cfw]+|--(?:ctty|fork|wait)))*)")
# A `wait` at a command's start, `command` before it or not: what a shell that put a command in the
# background (`&`) waits for it with, when it stands where waited() asks; an assignment of the last
# background job's id (`pid=$!`), which a wait may name; what makes `$!` name another job wherever
# it stands (a lone `&`, a process substitution); and where a command's first word stands, which
# job_word() reads with its quoting off (`"f"`, `\f`, `"eval"`), so a call of a function of the
# shell, or of coproc, bg, eval or a sourced file, is found however it is written, and a word the
# shell expands (`"$FN"`) is one this reading cannot name.
WAIT = re.compile(START + r"\s*(?:command\s+)?(?P<wait>wait)(?![\w-])")
PID = re.compile(r"(?:^|(?<=[\s;&|(]))(?P<name>[A-Za-z_]\w*)=(?P<quote>\"?)\$!(?P=quote)(?=[\s;&|)]|$)")
JOB = re.compile(r"(?P<job>(?<![&<>|])&(?![&>])|[<>]\()")
JOB_WORDS = frozenset({"coproc", "bg", "eval", "source", "."})
FIRST_WORD = re.compile(COMMAND_PLACE + r"(?:command\s+)?(?![A-Za-z_]\w*=)(?=(?P<first>[^\s;&|()`<>]))")
# An assignment of a variable, a word that names one as `$NAME`, `"${NAME}"` or `"${NAME[@]}"`, and
# a word that names one bare (`read pid`).
ASSIGNED = re.compile(r"(?:^|(?<=[\s;&|(]))(?P<name>[A-Za-z_]\w*)(?P<append>\+?)=")
VARIABLE = re.compile(r"\"?\$\{?(?P<name>[A-Za-z_]\w*)(?:\[@\])?\}?\"?")
NAMED = re.compile(r"(?<![\w${!#])[A-Za-z_]\w*(?![\w=+\[])")


class Frame:
    """A compound command a line of a shell stands in: a subshell `( )` (a `$( )` and backquotes
    among them), a brace group, a function's body, an if, a loop or a case. An if's branches and a
    case's arms are frames of their own, of one compound (`group`, its first frame), which holds
    what its close showed: whether it runs in a shell of its own (`subshell`: a subshell, an element
    of a pipeline, a compound put in the background) and where it was put in the background."""

    __slots__ = ("kind", "group", "subshell", "background")

    def __init__(self, kind: str, subshell: bool = False, group: Frame | None = None) -> None:
        self.kind, self.group = kind, group or self
        self.subshell, self.background = subshell, None


class Shell:
    """One shell that runs lines: a script, a step's run:, or a string, heredoc or substitution a
    shell runs; `functions`: the names of the functions it declares, "" for one whose name this
    reading cannot see, a call of any of which may start a job."""

    __slots__ = ("functions",)

    def __init__(self) -> None:
        self.functions: set[str] = set()


class Line(str):
    """A logical line, and what tells whether a command on it runs detached: the Shell that runs
    it, whether that whole shell runs detached, the frames open at the line's start and those the
    line opens, branches and closes (`events`, which frames_at() replays)."""

    shell: Shell
    detached: bool
    frames: tuple
    events: list


def line_of(text: str, shell: Shell, detached: bool) -> Line:
    line = Line(text)
    line.shell, line.detached, line.frames, line.events = shell, detached, (), []
    return line


def shell_runs(options: str) -> bool:
    """Whether a shell runs its script given these options before it, read in order to their last
    state: not after --help, --version, --dump-strings, --dump-po-strings or --rpm-requires, which
    print and exit, nor after -D, which prints the script's strings, nor with noexec on at the end
    (-n or -o noexec, which +n or +o noexec after it undoes), which reads the script and runs none
    of it."""
    words, noexec, at = (options or "").split(), False, 0
    while at < len(words):
        word, at = words[at], at + 1
        if word.startswith("--"):
            if re.fullmatch(rf"--{NO_RUN_LONG}", word):
                return False
            continue
        sign, letters = word[:1], word[1:]
        for letter in letters:
            if letter == "D" and sign == "-":
                return False
            if letter == "n":
                noexec = sign == "-"
            if letter in "oO" and at < len(words):
                name, at = words[at], at + 1
                noexec = sign == "-" if letter == "o" and name == "noexec" else noexec
    return not noexec


# The commands an unquoted heredoc's body runs: its `$(...)` and backquotes, a backslash before
# them aside (`\$(...)` is text).
SUBSTITUTION = re.compile(r"(?<!\\)\$\(([^()]*)\)")
BACKQUOTED = re.compile(r"(?<!\\)`([^`]*)(?<!\\)`")


def detached_lines(lines: list[tuple[int, Line]], expands: bool, keep_parts: bool) -> list[tuple[int, Line]]:
    """The lines of a shell `setsid -f` runs, each detached. When the caller's shell expands the
    text before setsid starts (an unquoted heredoc, a double-quoted string), the text's `$(...)`
    and backquotes run in the caller, in the foreground: they are left out of the detached line,
    and stand as lines of their own when `keep_parts` (a heredoc's; a string's the caller's own
    line reads)."""
    found = []
    for at, inner in lines:
        text = inner
        if expands:
            if keep_parts:
                found += [(at, part) for found_part in SUBSTITUTION.findall(inner) + BACKQUOTED.findall(inner)
                          for _, part in logical_lines(found_part, at - 1)]
            text = BACKQUOTED.sub(" ", SUBSTITUTION.sub(" ", inner))
        found.append((at, line_of(text, inner.shell, True)))
    return found


# A line whose list goes on with the next: it ends at an operator that takes a command after it
# (`true ||`, `x |`, `a &&`, `cmd |&`), outside every quoted string, so `true ||\nwait` is one
# list and the wait is the `||`'s right side.
CONTINUES = re.compile(r"(?:(?<![&|])&&|(?<!\|)\|\||(?<![&|<>])\|&?)$")


def continues(line: str) -> bool:
    """Whether a logical line's list goes on with the line after it (CONTINUES)."""
    text = line.rstrip()
    match = CONTINUES.search(text)
    return bool(match) and unquoted(text)[match.start()]


def logical_lines(text: str, offset: int = 0, detached: bool = False) -> list[tuple[int, Line]]:
    """A script's lines as the shell reads them: each with the backslash continuations after it
    joined on, the lines after it while a quoted string it opens is open, and the line after one
    whose list goes on with it (continues), comments and blank lines aside, with the number of its
    first line; each line's frames (scan_frames). A heredoc's
    body is read as the shell reads it: as lines of a script when a shell takes it as one (`bash
    <<EOF`), as the commands its `$(...)` and backquotes run when its delimiter is unquoted, and
    not at all when it is text (`cat <<'EOF'`). The script a shell takes as a string, env -S's
    string and a heredoc a shell takes follow the line that holds them, as lines of a shell of
    their own (expansions); a shell whose options let it run none (shell_runs) hands none.
    `detached`: the shell that runs the text runs detached."""
    shell, top, pending, first, heredoc, body = Shell(), [], "", 0, None, []
    for number, raw in enumerate(text.splitlines(), start=offset + 1):
        if heredoc is not None:
            if raw.strip() != heredoc[0]:
                body.append(raw)
                continue
            heredoc[1][2] = (*heredoc[2:], "\n".join(body))
            heredoc, body = None, []
            continue
        if not pending:
            first = number
            if not raw.strip() or raw.lstrip().startswith("#"):
                continue
        if raw.rstrip().endswith("\\"):
            pending += raw.rstrip()[:-1] + " "
            continue
        joined = uncommented((pending + raw).strip())
        if quoting(joined)[1] or continues(joined):
            pending += raw + "\n"
            continue
        pending = ""
        if not joined:
            continue
        line = line_of(joined, shell, detached)
        entry = [first, line, None]
        top.append(entry)
        opened = HEREDOC.search(line.replace("<<<", "   "))
        if opened and unquoted(line)[opened.start()]:
            runner = SHELL_HEREDOC.search(line[: opened.start() + 2])
            runner = runner if runner and shell_runs(runner.group("options")) else None
            mode = "script" if runner else "text" if opened.group("quote") else "substitutions"
            heredoc = (opened.group("delimiter"), entry, mode, number, not opened.group("quote"), opened.start(), runner)
    if pending.strip():
        top.append([first, line_of(pending.strip(), shell, detached), None])
    scan_frames([line for _, line, _ in top])
    found = []
    for index, (number, line, here) in enumerate(top):
        found.append((number, line))
        found += expansions(number, line, here, [(at, later) for at, later, _ in top[index + 1:]])
    return found


def expansions(number: int, line: Line, here: tuple | None, later: list[tuple[int, Line]]) -> list[tuple[int, Line]]:
    """The lines of the shells a line hands text to: the string `bash -c` runs and env -S's string
    (at the line's number), and the heredoc the line opens, read as a script a shell runs or for
    the substitutions an unquoted one runs (at its own lines' numbers)."""
    found, outside = [], unquoted(line)
    for string in SHELL_STRING.finditer(line):
        if outside[string.end()] and shell_runs(string.group("options")):
            _, script, expands = shell_word(line, string.end())
            found += [(number, inner) for _, inner in
                      nested(line, string.start(), string.start("shell"), script, 0, expands, later, False)]
    for split in ENV_SPLIT.finditer(line):
        if outside[split.start("env")]:
            end, script, expands = shell_word(line, split.end())
            script += line[end:command_end(line, end)]
            found += [(number, inner) for _, inner in
                      nested(line, split.start(), split.start("env"), script, 0, expands, later, False)]
    if here is not None:
        mode, start, expands, opened, runner, body = here
        if mode == "script":
            found += nested(line, runner.start(), runner.start("shell"), body, start, expands, later, True)
        elif mode == "substitutions":
            background = line.detached or background_unwaited(line, opened, later)
            for at, raw in enumerate(body.split("\n"), start=start + 1):
                for part in SUBSTITUTION.findall(raw) + BACKQUOTED.findall(raw):
                    found += logical_lines(part, at - 1, background)
    return found


def nested(line: Line, place: int, command: int, script: str, offset: int, expands: bool,
           later: list[tuple[int, Line]], keep_parts: bool) -> list[tuple[int, Line]]:
    """The lines of a script the command at `command` of the line hands a shell (or env -S), the
    command's prefixes starting at `place`: all detached when the line's shell runs detached or the
    command runs in the background with no wait for it, and when `setsid -f` runs it, all but the
    substitutions the caller expands first (detached_lines)."""
    if line.detached or background_unwaited(line, command, later):
        return logical_lines(script, offset, True)
    inner = logical_lines(script, offset)
    return detached_lines(inner, expands, keep_parts) if forked(line[place:command]) else inner


# The escapes of a `$'...'` string that stand for one character each (bash's ansic_quote).
ANSI_ESCAPES = {"a": "\a", "b": "\b", "e": "\x1b", "E": "\x1b", "f": "\f", "n": "\n", "r": "\r", "t": "\t",
                "v": "\v", "\\": "\\", "'": "'", '"': '"', "?": "?"}
ANSI_CODE = {"x": (16, 2), "u": (16, 4), "U": (16, 8)}


def ansi_scalar(value: int) -> str:
    """A `\\u` or `\\U` escape's character, as bash writes one: the scalar's own, or, for a
    surrogate or a value above Unicode's last (0x10FFFF), the bytes bash writes for it, which are
    no character of this reading - one replacement stands for them, so the word keeps a character
    where bash keeps bytes and no command word is cut short."""
    return chr(value) if value <= 0x10FFFF and not 0xD800 <= value <= 0xDFFF else "\ufffd"


def ansi_c(line: str, at: int) -> tuple[int, str]:
    """A `$'...'` string's text, decoded as bash decodes it, from `at`, just past its opening
    quote: where it ends, past its closing quote, and its text. Its escapes are ANSI_ESCAPES', an
    octal \\NNN, a hexadecimal \\xHH, \\uHHHH and \\UHHHHHHHH, and a control \\cX; a backslash before
    any other character stands for itself. A decoded NUL ends the word, as it ends bash's
    (`$':\\0; go test'` is the one word `:`), and the rest of the string is no command."""
    text = []
    while at < len(line) and line[at] != "'":
        if line[at] != "\\" or at + 1 >= len(line):
            text.append(line[at])
            at += 1
            continue
        escape, octal = line[at + 1], re.match(r"[0-7]{1,3}", line[at + 1:])
        code = ANSI_CODE.get(escape)
        digits = re.match(rf"[0-9A-Fa-f]{{1,{code[1]}}}", line[at + 2:]) if code else None
        if escape in ANSI_ESCAPES:
            text.append(ANSI_ESCAPES[escape])
            at += 2
        elif octal:
            text.append(chr(int(octal.group(0), 8) & 0xFF))
            at += 1 + len(octal.group(0))
        elif digits:
            text.append(ansi_scalar(int(digits.group(0), 16)))
            at += 2 + len(digits.group(0))
        elif escape == "c" and at + 2 < len(line):
            text.append(chr(ord(line[at + 2]) & 0x1F))
            at += 3
        else:
            text.append("\\" + escape)
            at += 2
        if text[-1] == "\0":
            return line.find("'", at) + 1 or len(line) + 1, "".join(text[:-1])
    return at + 1, "".join(text)


def shell_word(line: str, start: int) -> tuple[int, str, bool]:
    """The shell word that starts at `start`: where it ends, its text with its quoting taken off
    as the shell takes it (a `$'...'` string decoded, a `$"..."` one read as a "..." one), and
    whether the caller's shell expands some of it (a `$` or a backquote outside single quotes)."""
    at, text, expands = start, [], False
    while at < len(line) and not line[at].isspace() and line[at] not in ";&|()<>":
        char = line[at]
        if char == "$" and line[at + 1:at + 2] == "'":
            at, decoded = ansi_c(line, at + 2)
            text.append(decoded)
        elif char == "$" and line[at + 1:at + 2] == '"':
            at += 1
        elif char == "'":
            end = line.find("'", at + 1)
            end = len(line) if end < 0 else end
            text.append(line[at + 1:end])
            at = end + 1
        elif char == '"':
            at += 1
            while at < len(line) and line[at] != '"':
                if line[at] == "\\" and line[at + 1:at + 2] in ('"', "\\", "$", "`", "\n"):
                    text.append(line[at + 1])
                    at += 2
                    continue
                expands = expands or line[at] in "$`"
                text.append(line[at])
                at += 1
            at += 1
        elif char == "\\" and at + 1 < len(line):
            text.append(line[at + 1])
            at += 2
        else:
            expands = expands or char in "$`"
            text.append(char)
            at += 1
    return at, "".join(text), expands


def unquoted(line: str) -> list[bool]:
    """For each character of a line, whether it stands where the shell reads a command: outside
    every quoted string ('...', "..." with its backslash escapes, a backslash-escaped character),
    or inside a `$(...)` or `(...)`, which starts a command of its own even within "...". A
    `case` pattern's lone `)` closes nothing."""
    return quoting(line)[0]


def quoting(line: str) -> tuple[list[bool], bool]:
    """unquoted()'s answer, and whether a quoted string is still open at the line's end. A `$'...'`
    string takes a backslash's escape (`$'a\\'b'`) as a `'...'` one does not."""
    outside = [True] * (len(line) + 1)
    stack, index = [""], 0
    while index < len(line):
        char, state = line[index], stack[-1]
        if state in ("'", "$'"):
            if char == "\\" and state == "$'":
                outside[index:index + 2] = [False] * len(outside[index:index + 2])
                index += 2
                continue
            if char == "'":
                stack.pop()
            else:
                outside[index] = False
            index += 1
            continue
        if char == "\\":
            outside[index:index + 2] = [False] * len(outside[index:index + 2])
            index += 2
            continue
        if state == '"':
            if char == '"':
                stack.pop()
            elif line.startswith("$(", index):
                # The ( opens a command of its own, read as outside, as a bare ( is.
                stack.append("(")
                outside[index] = False
                index += 2
                continue
            elif char == "`":
                stack.append("`")
            else:
                outside[index] = False
            index += 1
            continue
        if char == "`" and state == "`":
            stack.pop()
            index += 1
            continue
        if line.startswith("$'", index):
            stack.append("$'")
            index += 2
            continue
        if char in "'\"":
            stack.append(char)
        elif char == "`":
            stack.append("`")
        elif char == "(":
            stack.append("(")
        elif char == ")" and state == "(":
            stack.pop()
        index += 1
    return outside, any(state in ("'", "$'", '"') for state in stack)


def uncommented(line: str) -> str:
    """The line less a comment: from a # that stands outside every quoted string and starts a
    word (the # of a parameter expansion, as in ${#x} or $#, does not)."""
    outside = unquoted(line)
    for at, char in enumerate(line):
        if char == "#" and outside[at] and (at == 0 or line[at - 1] in " \t;"):
            return line[:at].rstrip()
    return line


def commands(pattern: re.Pattern, line: str, group: str) -> list[re.Match]:
    """The matches of `pattern` whose `group` stands outside every quoted string of the line."""
    outside = unquoted(line)
    return [match for match in pattern.finditer(line) if outside[match.start(group)]]


def command_end(line: str, start: int) -> int:
    """Where the command that starts at `start` ends: at the first `;`, `&`, `|`, `)` or backquote
    outside every quoted string and outside every `$(...)`, `(...)` or backquoted command the
    command itself opens, so a `|` inside a -run pattern is the pattern's and a `)` that closes a
    substitution in an argument is the argument's. A backquote ends the command only when the
    command stands inside one (an odd number of backquotes outside quotes before it)."""
    outside = unquoted(line)
    inside_backquote = sum(1 for at in range(start) if outside[at] and line[at] == "`") % 2 == 1
    depth, opened_backquote, end = 0, False, start
    while end < len(line):
        char = line[end]
        if outside[end]:
            if char == "(":
                depth += 1
            elif char == ")":
                if depth == 0:
                    break
                depth -= 1
            elif char == "`":
                if opened_backquote:
                    opened_backquote = False
                elif inside_backquote and depth == 0:
                    break
                else:
                    opened_backquote = True
            elif char in ";&|" and depth == 0 and not opened_backquote:
                break
        end += 1
    return end


# The words that open, branch and close a compound command where a command starts.
OPENERS = {"if": "if", "case": "case", "for": "loop", "select": "loop", "while": "loop", "until": "loop"}
CLOSERS = {"fi": "if", "esac": "case", "done": "loop", "}": "brace"}


def frame_tokens(line: str) -> list[tuple[int, int, str, str, bool, str]]:
    """The line's tokens that open, branch or close a frame, outside every quoted string: (where it
    starts, where it ends, open, branch or close, the frame's kind, whether a `|` stands before an
    opener, which makes the compound an element of a pipeline, and a function body's name). A
    reserved word counts only where a command starts; `name()` and `function name` make the `{`
    after them a function's body."""
    outside = unquoted(line)
    found, at, command, piped, target, words, backquotes = [], 0, True, False, False, [], 0
    while at < len(line):
        char, two = line[at], line[at:at + 2]
        if outside[at] and char.isspace():
            at += 1
            continue
        if outside[at] and char in ";&|()`<>":
            width, was_piped, piped = 1, piped, False
            if line.startswith(";;&", at) or two in (";;", ";&"):
                width = 3 if line.startswith(";;&", at) else 2
                found.append((at, at + width, "branch", "case", False, ""))
                command = True
            elif char in "<>" or two == "&>":
                width, target = len(re.match(r"&?[<>]+[&|-]?", line[at:]).group(0)), True
            elif two in ("&&", "||", "|&"):
                width, command, piped = 2, True, two == "|&"
            elif char in ";&|":
                command, piped = True, char == "|"
            elif char == "(" and words and not command and line[at + 1:].lstrip().startswith(")"):
                width, command = line.index(")", at) - at + 1, True
                words.append("()")
            elif char in "()":
                found.append((at, at + 1, "open" if char == "(" else "close", "paren", was_piped, ""))
                command = True
            elif char == "`":
                backquotes += 1
                found.append((at, at + 1, "open" if backquotes % 2 else "close", "backquote", False, ""))
                command = backquotes % 2 == 1
            at += width
            continue
        start = at
        while at < len(line) and not (outside[at] and (line[at].isspace() or line[at] in ";&|()`<>")):
            at += 1
        word, was_piped, piped = line[start:at], piped, False
        if target:
            target = False
        elif word == "{" and (command or words[-2:-1] == ["function"]):
            named = words[-2:-1] == ["function"] or (words[-1:] == ["()"] and words[-3:-2] == ["function"])
            kind = "function" if words[-1:] == ["()"] or words[-2:-1] == ["function"] else "brace"
            name = (words[-1] if named else words[-2] if words[-1:] == ["()"] else "") if kind == "function" else ""
            found.append((start, at, "open", kind, was_piped, name))
            command = True
        elif command and word in OPENERS:
            found.append((start, at, "open", OPENERS[word], was_piped, ""))
            command = word in ("if", "while", "until")
        elif command and word in ("else", "elif"):
            found.append((start, at, "branch", "if", False, ""))
        elif command and word in CLOSERS:
            found.append((start, at, "close", CLOSERS[word], False, ""))
            command = False
        elif not (command and word in ("then", "do", "!", "time")):
            command = False
        words.append(word)
    return found


def scan_frames(lines: list[Line]) -> None:
    """Set each line of one shell's frames: those open at its start, and the frames its tokens
    open, branch and close, in order. A closed compound learns from what follows its close on the
    line whether it is an element of a pipeline or put in the background (list_end)."""
    stack: list[Frame] = []
    for line in lines:
        line.frames, line.events = tuple(stack), []
        for at, end, op, kind, piped, name in frame_tokens(line):
            if op == "open":
                frame = Frame(kind, subshell=piped or kind in ("paren", "backquote"))
                stack.append(frame)
            elif not stack or stack[-1].kind != kind and not (kind == "brace" and stack[-1].kind == "function"):
                continue
            elif op == "branch":
                frame = Frame(kind, group=stack.pop().group)
                stack.append(frame)
            else:
                frame = stack.pop()
                spot, piped_after = list_end(line, end)
                if spot is not None:
                    frame.group.background = (line, spot)
                frame.group.subshell = frame.group.subshell or spot is not None or piped_after
            if op == "open" and kind == "function":
                line.shell.functions |= {name} if name else {""}
            line.events.append((at, op, frame))


def frames_at(line: Line, position: int) -> list[Frame]:
    """The frames open at a position of the line, innermost last."""
    stack = list(line.frames)
    for at, op, frame in line.events:
        if at >= position:
            break
        if op != "open" and stack:
            stack.pop()
        if op != "close":
            stack.append(frame)
    return stack


def list_end(line: str, start: int) -> tuple[int | None, bool]:
    """Where the list the command at `start` stands in is put in the background - its lone `&`
    (no `&&`, and no redirection's `>&`, `&>` or `<&`), found past the and-or list's `&&` and
    `||` and the pipeline's `|` - and whether a `|` stood before it, outside every quoted string
    and every `( )` or backquotes the command itself opens. No `&` when a `;`, a `)` that closes
    what the command stands in, a backquote that does, or the line's end comes first."""
    outside = unquoted(line)
    inside_backquote = sum(1 for at in range(start) if outside[at] and line[at] == "`") % 2 == 1
    depth, opened_backquote, piped, at = 0, False, False, start
    while at < len(line):
        char = line[at]
        if outside[at]:
            if char == "(":
                depth += 1
            elif char == ")":
                if depth == 0:
                    return None, piped
                depth -= 1
            elif char == "`":
                if not opened_backquote and inside_backquote and depth == 0:
                    return None, piped
                opened_backquote = not opened_backquote
            elif depth == 0 and not opened_backquote:
                if char == ";":
                    return None, piped
                if line.startswith(("&&", "||"), at):
                    at += 2
                    continue
                if char == "|":
                    piped = True
                elif char == "&" and not line.startswith("&>", at) and line[at - 1:at] not in (">", "<", "|"):
                    return at, piped
        at += 1
    return None, piped


def starts_job(line: Line, position: int) -> bool:
    """Whether the command whose first word stands at a position of the line may start a job of
    its own, which `$!` would then name: its word, read with its quoting off (`"f"`, `\\f`,
    `"eval"`), is coproc, bg, eval, source or `.`, or a function this shell declares; or the shell
    expands the word (`"$FN"`, `$(…)`), so this reading cannot know what it names."""
    _, word, expands = shell_word(line, position)
    return expands or word in JOB_WORDS or word in line.shell.functions


def waited(line: Line, at: int, later: list[tuple[int, Line]]) -> bool:
    """Whether the shell that put a list in the background at `at` of the line waits for it before
    the step ends: with a `wait` of that same shell after it, standing where it runs whenever the
    list did (runs_whenever) and naming no job but that one. A bare wait waits for every job. `$!`
    names that job while nothing between its `&` and the read starts another (a lone `&`, a
    process substitution, or a command starts_job names); a variable names it when assigned `$!`
    while that held, at a place the `&` reaches too (runs_whenever: not in a subshell, a
    conditional, a loop or a function the `&` is outside of), and not assigned or named again
    there before the wait - an assignment the run may skip (`( pid=0 )`, `if false; then pid=0;
    fi`) leaves it. A wait anywhere else (`(wait)`, `if false; then wait; fi`, `f() { wait; }`,
    `wait | cat`, `wait &`), or naming another job (`x & sleep 0 & wait $!`), is none."""
    target, pids, own = frames_at(line, at), set(), True
    for other, start in [(line, at + 1)] + [(text, 0) for _, text in later if text.shell is line.shell]:
        outside = unquoted(other)
        found = sorted([(match.start(), "job", match) for match in JOB.finditer(other) if outside[match.start()]]
                       + [(match.end(), "word", match) for match in FIRST_WORD.finditer(other)
                          if match.start() == 0 or outside[match.start()]]
                       + [(match.start(), "assign", match) for match in commands(ASSIGNED, other, "name")]
                       + [(match.start(), "named", match) for match in NAMED.finditer(other) if outside[match.start()]]
                       + [(match.start("wait"), "wait", match) for match in commands(WAIT, other, "wait")],
                       key=lambda item: item[0])
        for position, kind, match in found:
            if position < start:
                continue
            if kind == "job" or (kind == "word" and starts_job(other, position)):
                own = False
            elif kind in ("assign", "named") and not runs_whenever(other, position, target):
                continue
            elif kind == "assign" and own and PID.match(other, position):
                pids.add(match.group("name"))
            elif kind in ("assign", "named"):
                pids.discard(match.group("name") if kind == "assign" else match.group(0))
            elif kind == "wait" and waits_here(other, match, target, pids, own):
                return True
    return False


def runs_whenever(line: Line, position: int, target: list[Frame]) -> bool:
    """Whether a command at a position of the line runs whenever the list the `&` of `target`'s
    frames ended did: in those frames, or in a brace group of the same shell, and in no subshell,
    conditional, loop, case or function body the `&` does not stand in; not an element of a
    pipeline and not after `&&` or `||` (a list the shell may skip)."""
    frames = frames_at(line, position)
    if any(frame not in target and not (frame.kind == "brace" and not frame.group.subshell) for frame in frames):
        return False
    if any(frame not in frames and frame.group.subshell for frame in target):
        return False
    return not line[:position].rstrip().endswith(("|", "&&", "||"))


def waits_here(line: Line, match: re.Match, target: list[Frame], pids: set[str], own: bool) -> bool:
    if not runs_whenever(line, match.start("wait"), target):
        return False
    end = command_end(line, match.end("wait"))
    if line[end:end + 1] == "|" and line[end:end + 2] != "||":
        return False
    if line[end:end + 1] == "&" and line[end:end + 2] != "&&" and line[end + 1:end + 2] != ">":
        return False
    return all((own and word in ("$!", '"$!"', "${!}", '"${!}"'))
               or ((variable := VARIABLE.fullmatch(word)) is not None and variable.group("name") in pids)
               for word in line[match.end("wait"):end].split())


def background_unwaited(line: Line, position: int, later: list[tuple[int, Line]]) -> bool:
    """Whether the command at a position of the line is put in the background, with its own list
    or as part of a compound it stands in, and its shell does not wait for it (waited)."""
    spot, _ = list_end(line, position)
    spots = [(line, spot)] if spot is not None else []
    spots += [frame.group.background for frame in frames_at(line, position) if frame.group.background]
    for spot_line, at in spots:
        after = later if spot_line is line else next(
            (later[index + 1:] for index, (_, text) in enumerate(later) if text is spot_line), [])
        if not waited(spot_line, at, after):
            return True
    return False


def detached_at(match: re.Match, line: Line, group: str, later: list[tuple[int, Line]] = ()) -> bool:
    """Whether the command a match found runs detached, so its step can end before it has written
    its events: in a shell that runs detached, behind `setsid -f` with no -w (the match's words
    before `group`), or in the background (`&`) with no wait for it (background_unwaited; `later`:
    the lines after this one)."""
    if line.detached or forked(line[match.start():match.start(group)]):
        return True
    return background_unwaited(line, match.start(group), list(later))


def runs_script(match: re.Match) -> bool:
    """Whether the shell of a RUNS match, if it has one, runs the file it names (shell_runs)."""
    return shell_runs(match.group("options") or "")


def go_tests(line: Line) -> list[re.Match]:
    """The go tests a line runs at a command's place. A go word that is a variable is read as go
    whatever the step or script assigns it: which value the shell holds when the command runs is
    no static reading's to say (an assignment after it, in another shell, a branch, a function or
    a sourced file, a name the shell builds), so `TOOL=true; "$TOOL" test` is refused, an
    over-refusal .github/go-test-step-misses.tsv lists."""
    return commands(GO_TEST, line, "go")


def go_test_parts(match: re.Match, line: str) -> tuple[str, str, int]:
    """A go test match's command, from its first word to its unquoted end, blanks made one; its
    flags and packages after `test`; and where the command ends in the line."""
    end = command_end(line, match.start("rest"))
    return " ".join(line[match.start("cmd"):end].split()), line[match.start("rest"):end], end


def flag_words(rest: str) -> list[str | None]:
    """A go test's words after `test`, split as the shell splits them (a word inside a quoted -run
    pattern is the pattern's), each word the shell expands when the step runs - a `$` or a
    backquote outside single quotes - given as None, a word this check cannot know; a `$` inside
    single quotes, or escaped, is a literal one (a regex's end anchor)."""
    words: list[str | None] = []
    text, expanded, started, quote, index = "", False, False, "", 0
    while index < len(rest):
        char = rest[index]
        index += 1
        if quote == "'":
            if char == "'":
                quote = ""
            else:
                text += char
            continue
        if char == "\\" and index < len(rest):
            text += rest[index]
            index += 1
            started = True
            continue
        if quote == '"':
            if char == '"':
                quote = ""
            else:
                expanded = expanded or char in "$`"
                text += char
            continue
        if char in " \t\n":
            if started:
                words.append(None if expanded else text)
            text, expanded, started = "", False, False
            continue
        started = True
        if char in "'\"":
            quote = char
            continue
        expanded = expanded or char in "$`"
        text += char
    if started:
        words.append(None if expanded else text)
    return words


def forked(place: str) -> bool:
    """Whether a command's place (the words before it: its prefixes) runs it behind `setsid -f`
    with no -w, which returns while the command runs."""
    for match in FORKED.finditer(place):
        options = match.group("options")
        if re.search(r"-[cw]*f|--fork", options) and not re.search(r"-[cf]*w|--wait", options):
            return True
    return False
