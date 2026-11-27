#!/usr/bin/env bash
# Every network client a workflow step or a composite action runs at a command position, itself or
# in a script it runs, goes through .github/scripts/ci-log-guard.sh, or is on
# .github/log-guard-allow.tsv with the reason it needs nothing.
#
# A client that copies a server's reply into the job log lets the server write workflow commands
# there (`\r::warning::…`, `\t::error::…`, `… ##[warning]…`), and only a stop-commands block around
# that output stops them (ci-log-guard.sh says how the runner reads the log). So a new step or a new
# script line that runs go, gh, curl, git fetch, docker pull or another client of NETWORK below,
# unguarded, fails here by name, rather than reading green until a server's text acts in a log.
#
# What it reads: each `run:` of every file of .github/workflows and of each composite action under
# .github/actions, and each shell script of this repository such a step runs, split into simple
# commands by .github/scripts/shell_commands.py's scanner (quote aware, a `$(…)` its own command,
# `${{ … }}` read as a word). A here-document's body is commands where a shell of its line reads it
# as its script (`bash <<'EOT'`, `cat <<'EOT' | bash`, `setsid bash <<'EOT'`, `bash /dev/stdin
# <<'EOT'`), and text elsewhere, but with an unquoted delimiter its `$(…)` and backtick
# substitutions are commands, as are those inside arithmetic (`$(( $(cmd) ))`) and an array's
# values (`files=(<(cmd))`). A command
# is routed when its first word, past `VAR=value` assignments and keywords, is one of WRAPPERS, or
# when its line is the guarded pipeline group `{ …; } 2>&1 | ci_log_guard cat`. A command that is
# not routed is a finding when its command word (or, through a runner of RUNNERS, `sh -c`,
# `bash -c` and `eval`, the word it runs) is a client of NETWORK running a subcommand that reaches a
# server (`git remote` only in the forms that fetch or ask the remote: GIT_REMOTE_LOCAL). A guard
# reached through a runner that runs a program (`xargs ci_log_guard …`, `env … ci_log_guard …`) is
# refused too: the guards are shell functions, so such a step cannot run (guard_behind_runner).
#
# A script a step runs is the command word of a command it does not route (through a runner, an
# interpreter such as `bash x.sh` or `. x.sh`, or eval) that names a file of this repository ending
# in .sh. `$GITHUB_WORKSPACE/` (or `${{ github.workspace }}/`) at its start names the repository
# root and `$GITHUB_ACTION_PATH/` (or `${{ github.action_path }}/`) the composite action's own
# directory. Any other relative path is read from the directory the text runs in (the step's
# working directory, which a script inherits from what runs it), else from each directory a literal
# `cd` of the same text moves to, else from the script's own directory. A path after a variable or
# a substitution, the way this repository's scripts name the scripts beside them
# (`"$script_dir/x.sh"`, `"$(dirname "$0")/x.sh"`, `"$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/x.sh"`),
# is read from the script's own directory, else from each directory above it to the repository root,
# with a variable the script sets to a directory under another (`d="${script_dir}/sub"`) read through
# it. The script is read as a step is, and so is each script it runs. A script's finding is the
# script's own: a row names it as (the script, `-`, `-`, client), whichever steps run it.
#
# It refuses, naming the file, the job, the step and the command (for a script, the script and the
# first step that runs it):
#   1. a finding no row of the allow list names (file, job, step, client);
#   2. a row of the allow list that names no finding, or that gives no reason: a row outliving its
#      step would let the next step of that name through unread;
#   3. a word naming a `.sh` that no file of this repository answers (a script a harness writes at
#      run time, such as `"$work/x.sh"`, or a copy of one in a fixture tree) that no row of
#      .github/log-guard-unread.tsv names (file, word) with the reason it needs no reading, so a
#      script the walk stops finding fails rather than passes unread; and
#   4. a row of that list whose word is read now, or gone.
# Its last line counts the steps under a shell other than bash or sh, which it does not read, and
# the unread words, which `unresolved` lists.
#
# It does not read: a program in another language, a command a variable names, and a client run
# under a wrapper this check does not name.
#
#   .github/scripts/check-log-guard.sh             check
#   .github/scripts/check-log-guard.sh list        print every finding as an allow-list row with no reason
#   .github/scripts/check-log-guard.sh scripts     print every script it reads, one path a line
#   .github/scripts/check-log-guard.sh unresolved  print each file and script name it found no file for
#
# CI runs it in ci.yaml's ci-filters job and in .github/workflows/workflow-files.yaml, each on a
# change to what it reads; its tests, check-log-guard.test.sh, in ci.yaml's test job.
set -euo pipefail

scripts=$(dirname "$(readlink -f "$0")")
cd "$(dirname "$0")/../.."

PYTHONDONTWRITEBYTECODE=1 PYTHONPATH="$scripts" python3 - "${1:-check}" <<'PY'
import functools
import os
import re
import sys
from pathlib import Path

import yaml
from shell_commands import (ASSIGNMENT, EXPRESSION, INTERPRETERS, RUNNERS, SKIP_WORDS, Scanner, command_of,
                            named_program, quoted_line_starts, shed_prefix, strip_runner, without_heredocs)

MODE = sys.argv[1]
ALLOW = Path(".github/log-guard-allow.tsv")
# The script names no file answers, each with the reason the check need not read it (an absent file
# lists none).
UNREAD = Path(".github/log-guard-unread.tsv")
WRAPPERS = ("ci_log_guard", "ci_log_guard_stderr")
# env, exec and time are runners here (RUNNERS), read with their options, not keywords shed before
# the command.
KEYWORDS = SKIP_WORDS - set(WRAPPERS) - set(RUNNERS)
GROUP = re.compile(r"\{[ \t]*([^{}]*);[ \t]*\}[ \t]*2>&1[ \t]*\|[ \t]*ci_log_guard[ \t]+cat")
# A call of ci_log_guard_stderr. Its stdout is no part of the log when the call stands inside a
# `$(…)`, a backtick pair or a `<(…)` still open at the call, or when its own command, up to the
# first unquoted `;`, `&&`, `||`, `|`, `&` or newline, redirects its stdout (stdout_safe).
STDERR_CALL = re.compile(r"(?<![\w-])ci_log_guard_stderr[ \t]+")
SHELLS = {None, "bash", "sh"}
# The two expressions a step names the repository root and a composite action's own directory
# with, read as the variables they equal before every other expression is elided.
DIRECTORY_EXPRESSIONS = ((re.compile(r"\$\{\{\s*github\.workspace\s*\}\}"), "$GITHUB_WORKSPACE"),
                         (re.compile(r"\$\{\{\s*github\.action_path\s*\}\}"), "$GITHUB_ACTION_PATH"))

# client -> the subcommands that reach a server, or None for every use of the client. bun and bunx
# are not here: the audit's probe of bun 1.4.4 (`bun install`, `bunx`) against a server answering
# 504 with a planted body printed the status code and never the body. A bunx that runs Playwright's
# own installer is read as the client `playwright` (network_client), whose downloader nobody probed.
NETWORK = {
    "curl": None, "wget": None, "gh": None, "npx": None,
    "playwright": {"install"},
    "go": {"build", "test", "vet", "run", "list", "mod", "install", "get", "generate", "work", "tool"},
    "git": {"fetch", "pull", "push", "clone", "ls-remote", "submodule", "remote"},
    "docker": {"pull", "push", "run", "build", "buildx", "compose", "login", "manifest", "image", "create"},
    "npm": {"install", "i", "ci", "view", "info", "show", "publish", "update", "exec", "dist-tag", "unpublish", "add"},
    "crane": {"copy", "cp", "pull", "push", "digest", "manifest", "ls", "catalog", "config", "export", "tag",
              "delete", "append", "mutate", "auth", "index", "flatten", "rebase", "blob", "validate", "edit"},
    "apt-get": {"install", "update", "upgrade", "dist-upgrade", "download", "source", "build-dep"},
    "apt": {"install", "update", "upgrade", "full-upgrade", "download", "source"},
    "pip": {"install", "download", "index"}, "pip3": {"install", "download", "index"},
    "uv": {"pip", "sync", "add", "tool", "run", "lock", "python", "venv", "self"},
    "mise": {"install", "i", "use", "u", "exec", "x", "upgrade", "up", "plugins", "p", "self-update", "run", "r",
             "latest", "ls-remote"},
    "elan": {"toolchain", "default", "self", "override", "run", "update"},
    "lake": {"build", "update", "exe", "test", "run", "upload", "unpack"},
}
# The options before a subcommand that take a value: `go -C dir`, `git -C dir`, `git -c k=v`,
# `docker --config d`.
VALUED = {"go": {"-C"}, "git": {"-C", "-c", "--git-dir", "--work-tree"},
          "docker": {"--config", "-c", "--context", "-H", "--host", "-l", "--log-level"}}
# `git remote`'s own subcommands that write or read only the repository's config and contact no
# server. `add -f`/`--fetch` fetches the new remote at once, and `update`, `show`, `prune`,
# `set-head -a` and a bare listing are left as they were: every form this set does not name, and
# `add` with a fetch flag, stays a network command.
GIT_REMOTE_LOCAL = {"add", "remove", "rm", "rename", "set-url", "get-url", "set-branches"}


class RawScanner(Scanner):
    """shell_commands' scanner, keeping each simple command as (routed, words): past the
    assignments and keywords before it, routed when its first word is one of WRAPPERS, kept whole
    when it is one of RUNNERS (run_commands reads through those), and otherwise read as the
    command, or the commands of the string it hands a shell (`bash -c`, eval, a trap), that
    command_of names. That string is read by a RawScanner too (string_scanner builds this
    scanner's type), with the text's arrays and aliases, so each command in it is read the same
    way: `bash -c 'FOO=1 curl …'` and `eval 'if curl …; then :; fi'` run curl, and
    `bash -c 'ci_log_guard curl …'` runs it routed."""

    def record(self, words: list) -> None:
        words = list(words)
        shed_prefix(words, KEYWORDS, runners=())
        if not words:
            return
        if words[0] in WRAPPERS:
            self.commands.append((True, words[1:]))
        elif named_program(words[0], RUNNERS):
            self.commands.append((False, words))
        else:
            for found in command_of(words, self):
                self.commands.append(found if isinstance(found, tuple) else (False, found))


def commands(run: str) -> list:
    """(routed, words) for each simple command of a run: script."""
    out, rest = [], []
    for line in EXPRESSION.sub("EXPR", run).split("\n"):
        group = GROUP.fullmatch(line.strip(" \t"))
        if group:
            out += [(True, words) for words in Scanner().scan(group.group(1))]
            rest.append("")
        else:
            rest.append(line)
    return out + RawScanner().scan("\n".join(rest))


def _word_start(line: str, i: int) -> bool:
    """Whether the character at `i` of the line starts a word (_word_starts)."""
    return i in _word_starts(line)


@functools.lru_cache(maxsize=4096)
def _word_starts(line: str) -> frozenset:
    """The indexes of the line at which a word starts, as bash reads the constructs this reader
    tracks: the line's start, and the character after a blank or an unquoted operator (`;`, `&`,
    `|`, a subshell's `(` or `)`, a group's `{` or `}`, the `(` that opens a substitution). Not
    the character after a quoted string, `${…}`, or the `)` that closes a `$(…)`, `<(…)` or
    `>(…)`: the word goes on past it, so `$(echo a)}` is one word that ends in a brace and
    `$(echo a)#b` one word that holds a hash, not a brace or a comment."""
    starts, opens, quote, i, at_start = set(), [], None, 0, True
    while i < len(line):
        if at_start:
            starts.add(i)
        ch, nxt = line[i], line[i + 1:i + 2]
        after = False  # whether the character after this one starts a word
        if quote == "'":
            quote = None if ch == "'" else quote
        elif quote == "$'":
            if ch == "\\":
                i += 1
            elif ch == "'":
                quote = None
        elif quote is None and ch == "$" and nxt == "'":
            quote = "$'"
            i += 1
        elif ch == "\\":
            i += 1
        elif ch == "$" and nxt == "(":
            opens.append(("substitution", quote))
            quote = None
            i += 1
            after = True
        elif ch == "$" and nxt == "{":
            opens.append(("parameter", quote))
            quote = None
            i += 1
        elif quote == '"':
            quote = None if ch == '"' else quote
        elif ch in "'\"":
            quote = ch
        elif ch in "<>" and nxt == "(":
            opens.append(("substitution", None))
            i += 1
            after = True
        elif ch == "(":
            opens.append(("subshell", None))
            after = True
        elif ch == "}" and opens and opens[-1][0] == "parameter":
            _, quote = opens.pop()
        elif ch == ")" and opens:
            kind, quote = opens.pop()
            after = kind == "subshell"
        elif ch in " \t;&|(){}":
            after = True
        at_start = after
        i += 1
    if at_start:
        starts.add(i)
    return frozenset(starts)


def _scan(line: str, upto: int):
    """The state of the line at `upto`: the frames still open, outermost first, each (kind, the
    index it opened at, the quote it opened inside), the quote open in the innermost frame, and
    whether the character at `upto` is escaped. A frame is a `capture` (`$(…)`, `<(…)`), a
    backtick pair, a `procout` (`>(…)`), a `subshell` (`(…)`) or a `brace` group (`{ …; }`). Each
    frame starts unquoted and keeps the quote it opened inside: a `$(` inside double quotes opens
    a capture whose `)` closes it, and the outer quote resumes there, so `v="$(date)"; …` closes
    its capture before the `;`."""
    stack, quote, i = [], None, 0
    while i < upto:
        ch, nxt = line[i], line[i + 1:i + 2]
        if quote == "'":
            if ch == "'":
                quote = None
        elif quote == "$'":
            # An ANSI-C string: a backslash escapes the character after it (`$'it\'s'`).
            if ch == "\\":
                i += 1
            elif ch == "'":
                quote = None
        elif quote is None and ch == "$" and nxt == "'":
            quote = "$'"
            i += 1
        elif ch == "\\":
            i += 1
        elif ch == "`":
            if stack and stack[-1][0] == "`":
                quote = stack.pop()[2]
            else:
                stack.append(("`", i, quote))
                quote = None
        elif ch == "$" and nxt == "(":
            stack.append(("capture", i, quote))
            quote = None
            i += 1
        elif quote == '"':
            if ch == '"':
                quote = None
        elif ch in "'\"":
            quote = ch
        elif ch in "<>" and nxt == "(":
            stack.append(("capture" if ch == "<" else "procout", i, None))
            i += 1
        elif ch == "(":
            stack.append(("subshell", i, None))
        elif ch == ")" and stack and stack[-1][0] in ("capture", "procout", "subshell"):
            quote = stack.pop()[2]
        elif ch == "{" and _word_start(line, i) and nxt in (" ", "\t", ""):
            stack.append(("brace", i, None))
        elif ch == "}" and _word_start(line, i) and stack and stack[-1][0] == "brace":
            stack.pop()
        i += 1
    return stack, quote, i > upto


def frames_at(line: str, upto: int) -> list:
    """The frames still open at `upto` (_scan), each as (kind, the index it opened at)."""
    return [(kind, pos) for kind, pos, _ in _scan(line, upto)[0]]


def _close_of(line: str, frame: tuple) -> int:
    """The index just past the character that closes `frame`, or -1 when the line leaves it open."""
    for j in range(frame[1] + 1, len(line) + 1):
        if frame not in frames_at(line, j):
            return j
    return -1


# Where a redirection sends stdout that is still the log after a pipe: the stage writes it after
# the guard's block has closed.
LOG_TARGETS = ("/dev/stdout", "/dev/stderr", "/dev/fd/1", "/dev/fd/2", "/proc/self/fd/1", "/proc/self/fd/2")


def _target(line: str, i: int) -> str:
    """The target of the redirection whose `>` (or `&>`'s `>`) is at `i`: `&<fd>` for a dup, `(`
    for a `>(…)`, else the target word, with its quotes taken off."""
    rest = line[i + 1:].lstrip(">|")
    if rest.startswith("&"):
        return "&" + re.match(r"&(\S*)", rest).group(1)
    rest = rest.lstrip(" \t")
    if rest.startswith("(") or rest.startswith(">("):
        return "("
    word = re.match(r"""[^\s;&|<>()]*""", rest).group(0)
    return word.strip("'\"") or "("


def stdout_safe(line: str, start: int, frames: list) -> bool:
    """Whether the command that starts at `start`, inside `frames`, keeps its stdout out of the
    log. The last redirection of stdout in the command's last stage decides, as the shell applies
    redirections left to right (`> out.json >&2` sends stdout to stderr). Before any pipe, a
    redirection does (`>`, `1>`, `>>`, `>|`, `>&…`, `&>`, but not `2>`, not `>&1` or
    `/dev/stdout`, and not into a `>(…)`): the command's stdout then goes where it goes while the
    block is open. After a pipe, the last stage writes after the block has closed, so only a
    redirection to a file does, not to a file descriptor, `/dev/stdout`, `/dev/stderr` or a
    `>(…)`; a pipe starts the decision afresh, and a stage naming a literal LOG_TARGET at its word
    start refuses the call (`| tee /dev/stderr > f`). This reader does not read a target whose
    leading text is a quote or substitution (`tee "$(x)"/dev/stderr`): as with `tee "$t"`, its
    value is not text the reader can name. A pipe into `ci_log_guard` is refused too, since its
    block and this one interleave under one stop state. The command ends at an unquoted `;`, `&&`,
    `||`, a lone `&`, a newline, a `#` at a word's start, or its frame's close; a command with no
    redirection of its own inside a `{ …; }` or `( … )` group is then read by the group's
    redirection the same way (a piped one is refused: the group's redirection would apply after the
    block has closed). Each character is read in the state _scan gives it, so a quote, a nested
    capture or an escape hides it."""
    piped, verdict = False, None
    for i in range(start, len(line)):
        stack, quote, escaped = _scan(line, i)
        if len(stack) < len(frames):
            break
        if escaped or len(stack) > len(frames):
            continue
        if piped and _word_start(line, i) and any(line.startswith(t, i + (line[i] in "'\"")) for t in LOG_TARGETS):
            return False
        if quote:
            continue
        ch, nxt = line[i], line[i + 1:i + 2]
        if ch == "#" and _word_start(line, i):
            break
        if ch in ";)`" or (ch == "}" and _word_start(line, i)):
            break
        if ch == "|":
            if nxt == "|" or line[i - 1:i] == "|":
                break
            if line[i - 1:i] == ">":
                continue
            if line[i + 1:].lstrip(" \t").startswith("ci_log_guard"):
                return False
            piped, verdict = True, None
        elif ch == "&":
            if nxt == ">":
                continue
            if line[i - 1:i] in (">", "&") or nxt == "&":
                if nxt == "&":
                    break
                continue
            break
        elif ch == ">":
            before = line[i - 1:i]
            if before == ">":
                continue
            if before.isdigit() and before != "1" and (i - 1 == start or line[i - 2:i - 1] in " \t"):
                continue
            target = _target(line, i)
            if target == "(":
                verdict = False
            elif target.startswith("&"):
                verdict = not (piped or target == "&1")
            elif target in LOG_TARGETS:
                verdict = not (piped or target in ("/dev/stdout", "/dev/fd/1", "/proc/self/fd/1"))
            else:
                verdict = True
    if verdict is not None:
        return verdict
    if piped:
        return False
    groups = [f for f in frames if f[0] in ("brace", "subshell")]
    if not groups or frames[-1][0] not in ("brace", "subshell"):
        return False
    group = groups[-1]
    end = _close_of(line, group)
    return end > 0 and stdout_safe(line, end, frames[:frames.index(group)])


# The assignment word just before an array's `(`: `a=`, `a+=`, `a[1]=`, at a word's start.
ARRAY_VALUES_OPEN = re.compile(r"(?:^|[\s;&|(])[A-Za-z_][A-Za-z0-9_]*(?:\[[^]]*\])?\+?=$")


def loose_stderr_guards(run: str) -> list:
    """Each ci_log_guard_stderr call whose stdout is neither captured nor kept out of the log:
    that command's stdout reaches the log outside the block. Read by the line, a
    backslash-continued line joined to the next first, each line from the quote it starts inside
    (a string a line before opened), and each call by its own command alone. A call inside a quoted
    string is text: a harness's expected output, or a script string that `sh -c` or eval runs,
    whose client the finding rule reads as not routed. A call among an array's values
    (`a=(ci_log_guard_stderr curl …)`) is loose wherever it stands: it runs where `"${a[@]}"`
    stands, which this reader does not follow."""
    loose = []
    text = EXPRESSION.sub("EXPR", run).replace("\\\n", " ")
    starts = quoted_line_starts(text)
    for number, line in enumerate(text.split("\n")):
        opened = starts.get(number, "")
        if not opened and line.lstrip(" \t").startswith("#"):
            continue
        read = opened + line
        for call in STDERR_CALL.finditer(read):
            stack, quote, escaped = _scan(read, call.start())
            if quote or escaped:
                continue
            frames = [(kind, pos) for kind, pos, _ in stack]
            if any(kind in ("capture", "`") for kind, _ in frames):
                continue
            if any(kind == "subshell" and ARRAY_VALUES_OPEN.search(read[:pos]) for kind, pos in frames):
                loose.append(line.strip(" \t"))
                continue
            if not stdout_safe(read, call.end(), frames):
                loose.append(line.strip(" \t"))
    return loose


# The commands that run the words after them are shell_commands.RUNNERS, which the here-document
# reader shares: a runner one of them reads and the other misses is a command one check passes.
# find runs the words after each of these, up to `;` or `+`.
FIND_EXEC = ("-exec", "-execdir", "-ok", "-okdir")


def run_commands(words: list) -> list:
    """The commands the words run, each as its words: past each of RUNNERS with its options, through
    a shell's `-c` script or the script it is handed and eval's words (shell_commands' command_of
    reads through the interpreter and its flags), and behind find's -exec."""
    words = list(words)
    while words and named_program(words[0], RUNNERS):
        if words[0].rsplit("/", 1)[-1] == "command" and words[1:2] and words[1] in ("-v", "-V"):
            return []
        runner = strip_runner(words)
        if runner == "flock" and len(words) > 1 and words[0] in ("-c", "--command"):
            # flock [options] <lock file> -c <string> runs the string as sh -c does.
            return [run for inner in command_of(["sh", "-c", words[1]]) for run in run_commands(inner)]
    if not words:
        return []
    if words[0].rsplit("/", 1)[-1] == "harness-pool.sh" and words[1:2] == ["start"]:
        # .github/scripts/harness-pool.sh start [--minutes <n>] <name> <command>...: the script, and
        # the command it queues, which runs in the step's directory with its output in the log.
        rest = words[2:]
        if rest[:1] == ["--minutes"]:
            rest = rest[2:]
        return [words[:1]] + run_commands(rest[1:])
    if named_program(words[0], INTERPRETERS) or words[0] == "eval":
        return [run for inner in command_of(list(words)) if inner != words for run in run_commands(inner)]
    if words[0].rsplit("/", 1)[-1] == "find":
        return [run for i, word in enumerate(words) if word in FIND_EXEC for run in run_commands(words[i + 1:])]
    return [words]


def network_client(words: list) -> str:
    """The client of NETWORK the words run against a server, or ''."""
    for run in run_commands(words):
        if run[0].rsplit("/", 1)[-1] in ("bunx", "npx") and len(run) > 1 and run[1].startswith("playwright"):
            run = ["playwright"] + run[2:]
        client = run[0].rsplit("/", 1)[-1]
        if client not in NETWORK:
            continue
        subcommands = NETWORK[client]
        if subcommands is None:
            return client
        rest = run[1:]
        while rest and rest[0].startswith("-"):
            flag = rest.pop(0)
            if flag in VALUED.get(client, ()) and rest:
                rest.pop(0)
        if rest and rest[0] in subcommands:
            if client == "git" and rest[0] == "remote" and git_remote_is_local(rest[1:]):
                continue
            return client
    return ""


def git_remote_is_local(rest: list) -> bool:
    """Whether `git remote REST` touches only the repository's config: a subcommand of
    GIT_REMOTE_LOCAL, and for `add` no -f or --fetch (a short-option cluster holding f counts)."""
    if not rest or rest[0] not in GIT_REMOTE_LOCAL:
        return False
    if rest[0] == "add":
        return not any(word == "--fetch" or (word.startswith("-") and not word.startswith("--") and "f" in word[1:])
                       for word in rest[1:])
    return True


def guard_behind_runner(words: list) -> str:
    """The first runner of RUNNERS before a ci_log_guard or ci_log_guard_stderr in the words that
    runs a program, or '' when the words reach no guard that way. The guards are shell functions
    ci-log-guard.sh defines, and env, xargs, nohup, timeout, nice, setsid, sudo, exec, command and
    the rest run a program, so a guard behind one is a step that cannot run (`xargs: ci_log_guard:
    No such file or directory`), not a guarded one. Only the bare reserved word `time` times the
    shell's own pipeline, function and all; `command -v` looks a name up and runs nothing."""
    words = list(words)
    behind = ""
    while words and named_program(words[0], RUNNERS):
        if words[0].rsplit("/", 1)[-1] == "command" and words[1:2] and words[1] in ("-v", "-V"):
            return ""
        if words[0] != "time" and not behind:
            behind = words[0].rsplit("/", 1)[-1]
        strip_runner(words)
    return behind if words and words[0] in WRAPPERS else ""


def shell_of(document: dict, holder: dict, step: dict):
    shell = step.get("shell") or ((holder.get("defaults") or {}).get("run") or {}).get("shell") \
        or ((document.get("defaults") or {}).get("run") or {}).get("shell")
    return str(shell).split()[0].rsplit("/", 1)[-1] if shell else None


def workdir_of(document: dict, holder: dict, step: dict):
    """The directory a step runs in, from the repository root, or None where an expression names it."""
    given = str(step.get("working-directory") or ((holder.get("defaults") or {}).get("run") or {}).get(
        "working-directory") or ((document.get("defaults") or {}).get("run") or {}).get("working-directory") or ".")
    given = re.sub(r"^\$\{\{\s*github\.workspace\s*\}\}", ".", given)
    return None if "${{" in given else os.path.normpath(given)


# A script named from a directory a variable or a substitution holds: `"$script_dir/x.sh"`,
# `"$(dirname "$0")/x.sh"`, `"$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/x.sh"`. The
# scanner keeps a variable as written and a substitution as a lone `$` (or a backtick).
FROM_DIRECTORY = re.compile(r"(?:\$\{?[A-Za-z_][A-Za-z0-9_]*(?:\[[^]]*\])?\}?|\$|`)(/[^$`*?\[~]+\.sh)")
# A variable a script sets to a directory under another one, `drivers="${script_dir}/role-takeover"`,
# which a script path after it is read through (subdirectories).
SUBDIRECTORY = re.compile(r"(?m)^[ \t]*(?:(?:readonly|local|export|declare(?:[ \t]+-\w+)?)[ \t]+)?"
                          r"([A-Za-z_][A-Za-z0-9_]*)=\"?\$\{?[A-Za-z_][A-Za-z0-9_]*\}?(/[^\"$`\s;&|]+)\"?[ \t]*$")


def subdirectories(text: str) -> dict:
    """{a variable of `text` set to a directory under another variable: that directory's path below
    it} (SUBDIRECTORY)."""
    return {name: below.rstrip("/") for name, below in SUBDIRECTORY.findall(text)}


def above(directory: str) -> list:
    """`directory` and each directory above it, to the repository root."""
    found = [directory]
    while directory not in (".", ""):
        directory = os.path.dirname(directory) or "."
        found.append(directory)
    return found


def script_path(word: str, workdir, action, own, moved=(), below=None) -> tuple:
    """(the shell script of this repository a command's first word names, as a path from the
    repository root, or ''; whether the word names a script, a word ending in .sh, that it could
    not find). `$GITHUB_WORKSPACE/` names the root and `$GITHUB_ACTION_PATH/` the composite
    action's directory (`action`, None outside one); any other relative path is read from
    `workdir` (None: not known), else from each directory a `cd` of the same text moves to
    (`moved`), else from `own`, the directory of the script the word is in, and each directory
    above it (a script that runs `cd "$(dirname "$0")/.."` first names its neighbours from there).
    A path after a variable or a substitution (FROM_DIRECTORY) is read from `own` (the step's
    directory for a step) and each directory above it to the repository root: the directory the
    variable holds is one of those wherever this repository's scripts compute it. A variable
    `below` maps to a directory under another is read as that directory."""
    for prefix, base in (("$GITHUB_WORKSPACE/", "."), ("${GITHUB_WORKSPACE}/", "."),
                         ("$GITHUB_ACTION_PATH/", action), ("${GITHUB_ACTION_PATH}/", action)):
        if word.startswith(prefix):
            word, workdir = word[len(prefix):], base
            break
    candidates = []
    computed = FROM_DIRECTORY.fullmatch(word)
    if computed and own is not None:
        name = re.match(r"\$\{?([A-Za-z_][A-Za-z0-9_]*)", word)
        rest = computed.group(1)[1:]
        if name and name.group(1) in (below or {}):
            rest = below[name.group(1)].lstrip("/") + "/" + rest
        candidates = [os.path.normpath(os.path.join(directory, rest)) for directory in above(own)]
    elif workdir is not None and word and not any(c in word for c in "$`*?[~") and not word.startswith("/"):
        if "/" in word or word.endswith(".sh"):  # a bare name other than x.sh is looked up on PATH
            candidates += [os.path.normpath(os.path.join(base, word))
                           for base in (workdir, *moved, *(above(own) if own is not None else ()))]
    for path in candidates:
        if path.endswith(".sh") and not path.startswith("..") and not os.path.isabs(path) and os.path.isfile(path):
            return path, False
    return "", word.endswith(".sh")


def is_script(job: str, label: str) -> bool:
    """Whether a finding's (job, step) is a script's own, which names neither."""
    return (job, label) == ("-", "-")


def subject(file: str, job: str, label: str) -> str:
    """How a problem names where its command is: a step, or a script and the first step that runs it."""
    if not is_script(job, label):
        return f"job {job} step \"{label}\""
    by_file, by_job, by_label = scripts[file]
    return f"the script {file} (job {by_job} step \"{by_label}\" of {by_file} runs it)"


def read(text: str, key: tuple, workdir, action, runner: tuple, own) -> None:
    """Notes the findings of a step's run: or of a script's text under `key` (file, job, step), and
    reads each script it runs that this check has not read yet, as a script of its own. `workdir`
    is where the text runs, `own` the directory a path after a variable is read from (script_path),
    `action` the composite action's directory, and `runner` the step that runs the first of them.
    A script runs in the directory of the step or the script that runs it, so its relative paths
    are read from there (`workdir`)."""
    text = EXPRESSION.sub("EXPR", text)
    found = commands(text)
    below = subdirectories(text)
    # The directories a literal `cd` of this text moves to, from where it runs: a relative script
    # path after it is read from there too (script_path).
    moved = [os.path.normpath(os.path.join(workdir or ".", words[1])) for _, words in found
             if len(words) == 2 and words[0] == "cd" and not any(c in words[1] for c in "$`*?[~")
             and not words[1].startswith("/")]
    for routed, words in found:
        if routed:
            continue
        behind = guard_behind_runner(words)
        if behind:
            unrunnable.append(key + (behind, " ".join(words)))
        client = network_client(words)
        if client:
            findings.setdefault(key + (client,), []).append(" ".join(words))
        for run in run_commands(words):
            script, missed = script_path(run[0], workdir, action, own, moved, below)
            if missed:
                unresolved.setdefault((key[0], run[0]), runner)
            if script and script not in scripts:
                scripts[script] = runner
                with open(script, errors="replace") as handle:
                    read(handle.read(), (script, "-", "-"), workdir, action, runner, os.path.dirname(script))
    loose.extend(key + (line,) for line in loose_stderr_guards(without_heredocs(text)))


files = sorted(p for p in Path(".github/workflows").iterdir() if p.suffix in (".yaml", ".yml"))
files += sorted(p for p in Path(".github/actions").glob("*/action.y*ml"))
findings = {}  # (file, job, step, client) -> [command text]; a script's job and step are `-`
loose = []  # (file, job, step, line): a ci_log_guard_stderr whose stdout reaches the log
unrunnable = []  # (file, job, step, runner, line): a guard behind a runner that runs a program
scripts = {}  # each script read -> (file, job, step) of the first step that runs it
unresolved = {}  # (the file, a word naming a script this check found no file for) -> the step that ran it
unread = 0
for path in files:
    document = yaml.safe_load(path.read_text()) or {}
    holders = [(job_id, job) for job_id, job in (document.get("jobs") or {}).items() if isinstance(job, dict)]
    action = None
    if isinstance(document.get("runs"), dict):
        holders.append(("-", document["runs"]))
        action = str(path.parent)
    for job_id, holder in holders:
        for index, step in enumerate(holder.get("steps") or []):
            if not isinstance(step, dict) or not isinstance(step.get("run"), str):
                continue
            if shell_of(document, holder, step) not in SHELLS:
                unread += 1
                continue
            label = str(step.get("name") or step.get("id") or f"step {index + 1}")
            run = step["run"]
            for expression, variable in DIRECTORY_EXPRESSIONS:
                run = expression.sub(lambda _: variable, run)
            key = (str(path), str(job_id), label)
            workdir = workdir_of(document, holder, step)
            read(run, key, workdir, action, key, workdir)

if not files:
    print("::error::check-log-guard: read no workflow file and no composite action, so it checked nothing")
    sys.exit(1)

if MODE == "list":
    for key in sorted(findings):
        print("\t".join(key) + "\t")
    sys.exit(0)
if MODE == "scripts":
    print("\n".join(sorted(scripts)))
    sys.exit(0)
if MODE == "unresolved":
    for file, word in sorted(unresolved):
        print(f"{file}\t{word}")
    sys.exit(0)

rows = {}
problems = [f"::error file={file}::{subject(file, job, label)} runs `{line}`: ci_log_guard_stderr blocks only "
            f"stderr, and this command's stdout is neither captured in $(…) nor redirected, so it reaches the "
            f"log outside the block; run it as `ci_log_guard …`, or capture or redirect its stdout"
            for file, job, label, line in loose]
problems += [f"::error file={file}::{subject(file, job, label)} runs `{line}`: {runner} runs a program, and "
             f"ci_log_guard and ci_log_guard_stderr are shell functions ci-log-guard.sh defines, so {runner} "
             f"cannot run the guard and the step fails; guard the whole command in the shell instead, as "
             f"`{{ …; }} 2>&1 | ci_log_guard cat` under `set -o pipefail`"
             for file, job, label, runner, line in unrunnable]
for number, line in enumerate(ALLOW.read_text().split("\n"), 1):
    if not line.strip() or line.startswith("#"):
        continue
    fields = line.split("\t")
    if len(fields) != 5 or not fields[4].strip():
        problems.append(f"::error file={ALLOW},line={number}::row {number} is not file, job, step, client and a "
                        f"reason, tab-separated, with the reason given: a row says why its step needs no guard")
        continue
    rows[tuple(fields[:4])] = number

listed = {}
for number, line in enumerate(UNREAD.read_text().split("\n") if UNREAD.is_file() else [], 1):
    if not line.strip() or line.startswith("#"):
        continue
    fields = line.split("\t")
    if len(fields) != 3 or not fields[2].strip():
        problems.append(f"::error file={UNREAD},line={number}::row {number} is not file, script name and a reason, "
                        f"tab-separated, with the reason given: a row says why its script needs no reading")
        continue
    listed[tuple(fields[:2])] = number
for (file, word), runner in sorted(unresolved.items()):
    if (file, word) not in listed:
        problems.append(f"::error file={file}::{file} runs `{word}`, a script no file of this repository answers, so "
                        f"this check cannot read it: name it by a path the check resolves, or add a row to {UNREAD} "
                        f"saying why it needs no reading")
for (file, word), number in sorted(listed.items(), key=lambda item: item[1]):
    if (file, word) not in unresolved:
        problems.append(f"::error file={UNREAD},line={number}::row {number} names `{word}` in {file}, and no script "
                        f"name there now goes unread (it names a file the check reads, or {file} no longer holds it): "
                        f"remove the row")

for key in sorted(findings):
    if key not in rows:
        file, job, label, client = key
        problems += [f"::error file={file}::{subject(file, job, label)} runs `{command}`, a {client} "
                     f"command that can copy a server's reply into the log, outside "
                     f".github/scripts/ci-log-guard.sh: run it as `ci_log_guard …` (output to the log) or "
                     f"`ci_log_guard_stderr …` (stdout captured), or add a row to {ALLOW} saying why it needs "
                     f"no guard" for command in findings[key]]
for key, number in sorted(rows.items(), key=lambda item: item[1]):
    if key not in findings:
        file, job, label, client = key
        problems.append(f"::error file={ALLOW},line={number}::row {number} names "
                        + (f"the script {file} running {client} unguarded, and no script a step runs does"
                           if is_script(job, label) else
                           f"job {job} step \"{label}\" of {file} running {client} unguarded, and no such step "
                           f"does") + ": remove the row")

for problem in problems:
    print(problem)
print(f"check-log-guard: {len(files)} files and {len(scripts)} scripts, {len(findings)} unguarded network commands, "
      f"every one on {ALLOW}; {unread} steps under another shell not read, and {len(unresolved)} script names "
      f"no file of this repository answers not read, every one on {UNREAD}" if not problems
      else f"::error::check-log-guard: {len(problems)} problem(s)")
sys.exit(1 if problems else 0)
PY
