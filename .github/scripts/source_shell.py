"""Shell read as bash reads it, for check-go-offline.sh and check-omp-offline.sh: shell_script gives a
shell text's simple commands, a continued line one line, a $(...) and `...` read as commands of
their own, a heredoc body as no command, each with its NAME=value prefix, its function, its
place in a pipeline and whether it is forked; the helpers after it read a command's program (an
argv's too, after its launchers), the environment its own words give it, the shell it hands more
shell to, and the script a shell reads from the command before it in a pipeline.

Imported with PYTHONPATH naming the directory this file lives in. Its own test is
source_scan.test.sh.
"""
import bisect
import re

from source_scan import CLEAR


class ShWord:
    """A shell word as bash reads it: its parts in order, each (kind, text, nested), where kind is
    lit (unquoted text, a backslash's character included), sq (single-quoted text), dq (double-quoted
    text), param ($NAME or ${...}: text is the name), cmd ($(...), `...` or <(...): nested is the
    list of its commands) or arith. defaults maps the index of each param part written
    ${NAME:-word}, ${NAME-word}, ${NAME:=word} or ${NAME=word} to the text of its word, the value
    the expansion takes when NAME is unset."""

    __slots__ = ("parts", "start", "line", "defaults")

    def __init__(self, parts, start, line, defaults=None):
        self.parts, self.start, self.line, self.defaults = parts, start, line, defaults or {}

    def value(self, placeholder="\x00"):
        """The word after quote removal, each expansion as placeholder."""
        return "".join(p[1] if p[0] in ("lit", "sq", "dq") else placeholder for p in self.parts)

    @property
    def static(self):
        return all(p[0] in ("lit", "sq", "dq") for p in self.parts)

    @property
    def quoted(self):
        return any(p[0] in ("sq", "dq") for p in self.parts)

    def source(self):
        """The word with its quotes removed and its expansions written back ($NAME, $(...))."""
        out = []
        for kind, text, _ in self.parts:
            if kind in ("lit", "sq", "dq"):
                out.append(text)
            elif kind == "param":
                out.append("$" + text if re.fullmatch(r"\w+", text) else "${" + text + "}")
            else:
                out.append("$(...)")
        return "".join(out)

    def __repr__(self):
        return f"ShWord({self.source()!r})"


class ShCommand:
    """A simple command: its NAME=value assignments, its words, its heredocs (each [strip, delimiter,
    quoted, body, first line of the body]), the 0-based line it starts on, the frames that hold it
    (an id per function body and per subshell, a $(...) or `...` among them), the function it is
    in, its index in its pipeline, the command whose word holds the substitution it is in, and
    whether it is forked: run by a child of the shell (one command of a pipeline of more than one,
    or a command put in the background with &), so what it sets reaches no later command."""

    __slots__ = ("assigns", "words", "heredocs", "line", "start", "frames", "function", "pipe", "parent",
                 "forked")

    def __init__(self, line, start, frames, function, pipe, parent):
        self.assigns, self.words, self.heredocs = [], [], []
        self.line, self.start, self.frames, self.function, self.pipe, self.parent = \
            line, start, frames, function, pipe, parent
        self.forked = pipe > 0

    def values(self):
        return [w.value() for w in self.words]

    def __repr__(self):
        return f"ShCommand(line {self.line + 1}: {' '.join(w.source() for w in self.words)!r})"


_SH_FRAMES = iter(range(1, 1 << 62))
_SH_ASSIGNMENT = re.compile(r"([A-Za-z_]\w*)(?:\[[^\]]*\])?\+?=")
_SH_REDIRECT = re.compile(r"[0-9]*(<<<|<<-|<<|<>|<&|>>|>&|>\||&>>|&>|<|>)")
_SH_STOP = set(" \t\n;&|()<>")
_SH_PARAM = re.compile(r"\$([A-Za-z_]\w*|[0-9@*#?$!-])")


class _ShellReader:
    def __init__(self, text, line):
        self.text, self.n, self.base = text, len(text), line
        self.starts = [0] + [m.end() for m in re.finditer("\n", text)]
        self.commands, self.pending, self.defaults = [], [], {}

    def line_at(self, offset):
        return self.base + bisect.bisect_right(self.starts, offset) - 1

    def blanks(self, i):
        while i < self.n:
            if self.text[i] in " \t":
                i += 1
            elif self.text.startswith("\\\n", i):
                i += 2
            else:
                break
        return i

    def heredoc_bodies(self, i):
        """Read the bodies of the heredocs opened on the line that just ended, from i."""
        while self.pending:
            doc = self.pending.pop(0)
            strip, delimiter = doc[0], doc[1]
            first, lines = i, []
            while i < self.n:
                end = self.text.find("\n", i)
                end = self.n if end < 0 else end
                line = self.text[i:end]
                i = min(end + 1, self.n)
                if (line.lstrip("\t") if strip else line) == delimiter:
                    break
                lines.append(line.lstrip("\t") if strip else line)
            doc[3], doc[4] = "\n".join(lines), self.line_at(first)
        return i

    def word(self, i, frames, function, parent):
        parts, start, text, n = [], i, self.text, self.n

        def lit(s):
            if parts and parts[-1][0] == "lit":
                parts[-1] = ("lit", parts[-1][1] + s, None)
            else:
                parts.append(("lit", s, None))

        while i < n:
            c = text[i]
            if c in "<>" and text.startswith("(", i + 1):
                nested, i = self.substitution(i + 2, frames, function, parent)
                parts.append(("cmd", "", nested))
                continue
            if c == "(" and len(parts) == 1 and parts[0][0] == "lit" and re.fullmatch(r"[A-Za-z_]\w*\+?=", parts[0][1]):
                depth, j = 0, i  # an array assignment, NAME=(a b c)
                while j < n:
                    depth += {"(": 1, ")": -1}.get(text[j], 0)
                    j += 1
                    if depth == 0:
                        break
                lit(text[i:j])
                i = j
                continue
            if c in _SH_STOP:
                break
            if c == "\\":
                if text.startswith("\n", i + 1):
                    i += 2
                    continue
                lit(text[i + 1:i + 2])
                i += 2
            elif c == "'":
                j = text.find("'", i + 1)
                j = n if j < 0 else j
                parts.append(("sq", text[i + 1:j], None))
                i = j + 1
            elif c == '"':
                i = self.double_quoted(i + 1, parts, frames, function, parent)
            elif c == "$":
                i = self.dollar(i, parts, frames, function, parent, lit)
            elif c == "`":
                i = self.backtick(i, parts, frames, function, parent)
            else:
                lit(c)
                i += 1
        return ShWord(parts, start, self.line_at(start), self.defaults.pop(id(parts), None)), i

    def double_quoted(self, i, parts, frames, function, parent):
        text, n, buf, first = self.text, self.n, [], len(parts)
        while i < n and text[i] != '"':
            c = text[i]
            if c == "\\" and i + 1 < n:
                if text[i + 1] == "\n":
                    i += 2
                    continue
                buf.append(text[i + 1] if text[i + 1] in '$`"\\' else text[i:i + 2])
                i += 2
            elif c == "$" and text.startswith(("$'", '$"'), i):
                buf.append(c)  # inside double quotes $'...' and $"..." quote nothing: "a$" ends there
                i += 1
            elif c in "$`":
                if buf:
                    parts.append(("dq", "".join(buf), None))
                    buf = []
                if c == "$":
                    i = self.dollar(i, parts, frames, function, parent, lambda s: parts.append(("dq", s, None)))
                else:
                    i = self.backtick(i, parts, frames, function, parent)
            else:
                buf.append(c)
                i += 1
        if buf or len(parts) == first:  # "" is a word of its own; "$x" is the expansion alone
            parts.append(("dq", "".join(buf), None))
        return min(i + 1, n)

    def dollar(self, i, parts, frames, function, parent, lit):
        text, n = self.text, self.n
        if text.startswith("$((", i):
            depth, j = 0, i + 1
            while j < n:
                depth += {"(": 1, ")": -1}.get(text[j], 0)
                j += 1
                if depth == 0:
                    break
            parts.append(("arith", text[i + 3:j - 2], None))
            return j
        if text.startswith("$(", i):
            nested, j = self.substitution(i + 2, frames, function, parent)
            parts.append(("cmd", "", nested))
            return j
        if text.startswith("${", i):
            depth, j = 0, i + 1
            while j < n:
                if text[j] == "\\":
                    j += 2
                    continue
                depth += {"{": 1, "}": -1}.get(text[j], 0)
                j += 1
                if depth == 0:
                    break
            inner = text[i + 2:j - 1]
            name = re.match(r"[#!]?([A-Za-z_]\w*|[0-9@*#?$!-])", inner)
            parts.append(("param", name.group(1) if name else inner, None))
            default = re.fullmatch(r"([A-Za-z_]\w*):?[-=](.*)", inner, re.S)
            if default:  # ${NAME:-word}: the word the expansion takes when NAME is unset
                self.defaults.setdefault(id(parts), {})[len(parts) - 1] = default.group(2)
            return j
        if text.startswith("$'", i):
            j, buf = i + 2, []
            while j < n and text[j] != "'":
                if text[j] == "\\" and j + 1 < n:
                    buf.append({"n": "\n", "t": "\t"}.get(text[j + 1], text[j + 1]))
                    j += 2
                    continue
                buf.append(text[j])
                j += 1
            parts.append(("sq", "".join(buf), None))
            return j + 1
        if text.startswith('$"', i):
            return self.double_quoted(i + 2, parts, frames, function, parent)
        m = _SH_PARAM.match(text, i)
        if m:
            parts.append(("param", m.group(1), None))
            return m.end()
        lit("$")
        return i + 1

    def substitution(self, i, frames, function, parent):
        """The commands of a $(...) or <(...) whose text starts at i, and the offset after its )."""
        first = len(self.commands)
        end = self.parse(i, frames + (next(_SH_FRAMES),), function, parent, closing=True)
        return self.commands[first:], end

    def backtick(self, i, parts, frames, function, parent):
        text, n, j = self.text, self.n, i + 1
        while j < n and text[j] != "`":
            j += 2 if text[j] == "\\" else 1
        inner = re.sub(r"\\([`$\\])", r"\1", text[i + 1:j])
        reader = _ShellReader(inner, self.line_at(i + 1))
        reader.parse(0, frames + (next(_SH_FRAMES),), function, parent)
        for command in reader.commands:
            command.start += i + 1
        self.commands += reader.commands
        parts.append(("cmd", "", reader.commands))
        return min(j + 1, n)

    def parse(self, i, frames, function, parent, closing=False):
        """Read commands from i to the end of the text, or, when closing, to the ) that closes a
        substitution; return the offset after it."""
        text, n = self.text, self.n
        cur, pipe, expect, until = None, 0, "cmd", None
        scopes, cases, function_name = [], [], None

        def finish():
            nonlocal cur
            if cur is not None and (cur.words or cur.assigns):
                self.commands.append(cur)
            cur = None

        def command(at):
            nonlocal cur
            if cur is None:
                cur = ShCommand(self.line_at(at), at, frames, function, pipe, parent)
            return cur

        while True:
            i = self.blanks(i)
            if i >= n:
                finish()
                return n
            c = text[i]
            if c == "#":
                end = text.find("\n", i)
                i = n if end < 0 else end
                continue
            if c == "\n":
                finish()
                pipe, i = 0, self.heredoc_bodies(i + 1)
                if expect == "args":
                    expect = "cmd"
                continue
            if expect == "pattern" and c == "|":
                i += 1
                continue
            if text.startswith((";;&", ";;", ";&"), i) and cases:
                finish()
                expect = "pattern"
                i += 3 if text.startswith(";;&", i) else 2
                continue
            redirect = _SH_REDIRECT.match(text, i) if c in "<>&0123456789" else None
            if redirect and not (c == "&" and redirect.group(1)[0] != "&") \
                    and not (c in "<>" and text.startswith("(", i + 1)):
                op = redirect.group(1)
                target, i = self.word(self.blanks(redirect.end()), frames, function, cur)
                if op in ("<<", "<<-"):
                    doc = [op == "<<-", target.value(""), target.quoted, "", 0]
                    self.pending.append(doc)
                    command(redirect.start()).heredocs.append(doc)
                continue
            if c in ";&|":
                op = text[i:i + 2] if text[i:i + 2] in ("&&", "||", "|&") else c
                if cur is not None and op in ("|", "|&", "&"):
                    cur.forked = True  # the left side of a pipe, or a command put in the background
                finish()
                pipe = pipe + 1 if op in ("|", "|&") else 0
                if expect in ("args", "cmd"):
                    expect = "cmd"
                i += len(op)
                continue
            if c == "(":
                if expect == "pattern":
                    i += 1
                    continue
                after = self.blanks(i + 1)
                if cur is not None and len(cur.words) == 1 and not cur.assigns and text.startswith(")", after):
                    function_name, cur = cur.words[0].value(), None  # name() { ... }
                    i, expect = after + 1, "cmd"
                    continue
                if function_name and text.startswith(")", after):
                    i = after + 1  # function name() after the keyword
                    continue
                if text.startswith("((", i):
                    depth, j = 0, i
                    while j < n:
                        depth += {"(": 1, ")": -1}.get(text[j], 0)
                        j += 1
                        if depth == 0:
                            break
                    i = j
                    continue
                finish()
                scopes.append((frames, function))
                frames = frames + (next(_SH_FRAMES),)
                if function_name:
                    function, function_name = function_name, None
                i += 1
                continue
            if c == ")":
                if expect == "pattern":
                    expect = "cmd"
                    i += 1
                    continue
                finish()
                if scopes:
                    frames, function = scopes.pop()
                    i += 1
                    continue
                if closing:
                    return i + 1
                i += 1
                continue
            w, at = self.word(i, frames, function, cur if expect == "args" else None)
            i = at if at > i else i + 1
            value = w.value() if w.static and not w.quoted else None
            if expect == "skip":
                if value == until:
                    expect = "pattern" if until == "in" else "cmd"
                continue
            if expect == "pattern":
                if value == "esac":
                    cases.pop()
                    expect = "args"
                continue
            if expect == "case":
                expect, until = "skip", "in"
                cases.append(True)
                continue
            if expect == "fname":
                function_name, expect = value, "cmd"
                continue
            if expect == "cmd":
                if value == "esac" and cases:
                    finish()
                    cases.pop()
                    continue
                if value in ("if", "then", "else", "elif", "do", "while", "until", "!", "time", "fi", "done"):
                    finish()
                    continue
                if value in ("for", "select"):
                    finish()
                    expect, until = "skip", "do"
                    continue
                if value == "case":
                    finish()
                    expect = "case"
                    continue
                if value == "[[":
                    finish()
                    expect, until = "skip", "]]"
                    continue
                if value == "function":
                    finish()
                    expect = "fname"
                    continue
                if value == "{":
                    finish()
                    scopes.append((frames, function))
                    if function_name:
                        frames, function, function_name = frames + (next(_SH_FRAMES),), function_name, None
                    continue
                if value == "}" and scopes:
                    finish()
                    frames, function = scopes.pop()
                    continue
                first = w.parts[0] if w.parts else None
                assignment = _SH_ASSIGNMENT.match(first[1]) if first and first[0] == "lit" else None
                if assignment and (cur is None or not cur.words):
                    rest = [("lit", first[1][assignment.end():], None)] + w.parts[1:]
                    command(w.start).assigns.append((assignment.group(1), ShWord(rest, w.start, w.line, w.defaults)))
                    continue
                command(w.start)
                for kind, _, nested in w.parts:  # its substitutions ran before it, in its context
                    for inner in nested or ():
                        if inner.parent is None:
                            inner.parent = cur
                cur.words.append(w)
                expect = "args"
                continue
            command(w.start).words.append(w)


def shell_script(text, line=0):
    """The simple commands of a shell text, nested ones included (those of each $(...), `...` and
    <(...)), in the order they start. line is the 0-based line the text starts on in its file. A
    continued line is one line; a heredoc's body is no command (shell_script reads it when a shell
    runs it); a for list, a case pattern and a [[ test ]] are no command."""
    reader = _ShellReader(text, line)
    reader.parse(0, (), None, None)
    return sorted(reader.commands, key=lambda command: (command.line, command.start))


SHELLS = {"bash", "sh", "dash", "zsh", "ksh"}


def command_word(values):
    """The index of the word an argv runs after `env` with its settings and options, `command`,
    `exec`, `nice`, `nohup`, `time`, `timeout` and `flock` with their operands, or None. A word no
    reading can know (None, in a Go, TypeScript or Python argv) is the program."""
    k = 0
    while k < len(values):
        v = values[k]
        base = v.rsplit("/", 1)[-1] if v is not None else None
        if base == "env":
            k += 1
            while k < len(values) and values[k] is not None and (
                    values[k].startswith("-") or _SH_ASSIGNMENT.match(values[k])):
                k += 2 if values[k] in ("-u", "-C", "-S") else 1
            continue
        if base in ("command", "exec", "nohup", "time", "builtin"):
            k += 1
            while k < len(values) and values[k] is not None and values[k].startswith("-"):
                k += 1
            continue
        if base in ("nice", "timeout", "flock"):
            k += 1
            while k < len(values) and values[k] is not None and values[k].startswith("-"):
                k += 2 if values[k] in ("-n", "-s", "-k", "-w", "-E") else 1
            if base in ("timeout", "flock") and k < len(values):
                k += 1  # the duration, or the lock file
            continue
        return k
    return None


def shell_command_word(command):
    """command_word of a shell command's words."""
    return command_word(command.values())


def env_settings(values, upto, static=lambda k: True):
    """The settings each `env` before word index upto of an argv makes, with its NAME=value, -u NAME
    and -i; static(k) says whether word k is literal text (a setting's value is None when not). A
    word no reading can know (None) ends an env's settings."""
    out, k = [], 0
    while k < upto:
        if values[k] is not None and values[k].rsplit("/", 1)[-1] == "env":
            k += 1
            while k < upto:
                v = values[k]
                if v is None:
                    break
                if v in ("-i", "-", "--ignore-environment"):
                    out.append(CLEAR)
                elif v in ("-u", "--unset") and k + 1 < upto:
                    out.append(("set", values[k + 1], None))
                    k += 1
                elif v.startswith("--unset="):
                    out.append(("set", v[len("--unset="):], None))
                elif _SH_ASSIGNMENT.match(v):
                    name, _, value = v.partition("=")
                    out.append(("set", name, value if static(k) else None))
                elif not v.startswith("-"):
                    break
                k += 1
            continue
        k += 1
    return out


def shell_env_elements(command, upto):
    """The settings a command's own words make for the program at word index upto: its NAME=value
    prefix, then each `env` before that word (env_settings)."""
    out = [("set", name, value.value() if value.static else None) for name, value in command.assigns]
    return out + env_settings(command.values(), upto, lambda k: command.words[k].static)


def shell_args(values, k):
    """For an argv whose word k is a shell (bash, sh, ...): (the index of its first word after its
    options, whether those options hold -c, as -c or among others: -ec, -lc); else None. A word no
    reading can know (None) ends the options."""
    if k is None or k >= len(values) or values[k] is None or values[k].rsplit("/", 1)[-1] not in SHELLS:
        return None
    j, takes_c = k + 1, False
    while j < len(values) and values[j] is not None and values[j].startswith("-"):
        takes_c |= bool(re.fullmatch(r"-[a-zA-Z]*c[a-zA-Z]*", values[j]))
        j += 2 if re.fullmatch(r"[-+][a-zA-Z]*[oO]", values[j]) else 1
    return j, takes_c


def shell_c_script(values, k=0):
    """The index of the script after a shell's -c in an argv whose program is word k, else None."""
    found = shell_args(values, k)
    return found[0] if found and found[1] and found[0] < len(values) else None


def shell_inner_scripts(command, word=shell_command_word):
    """(text, line) of the shell each command hands more shell to: the string after a shell's -c
    (bash -c, sh -lc, ...), what eval joins, and a heredoc a shell with no script operand reads.
    word(command) gives the index of the command's program; a reader whose policy passes over more
    launchers than command_word does (package_installs.py's sudo) gives its own."""
    values, out = command.values(), []
    if values and values[0] == "eval":
        out.append((" ".join(values[1:]), command.words[1].line if len(command.words) > 1 else command.line))
        return out
    found = shell_args(values, word(command))
    if found is None:
        return out
    j, takes_c = found
    if takes_c and j < len(values):
        out.append((values[j], command.words[j].line))
    elif j >= len(values):
        out += [(doc[3], doc[4]) for doc in command.heredocs]
    return out


def shell_stdin_scripts(commands, index, word=shell_command_word):
    """(text, line) of the script a shell with no script operand reads from its standard input,
    for commands[index] (of shell_script's list) when that shell is fed by the command before it
    in its pipeline: the heredoc bodies of a `cat` given no file, or the words of an `echo` or the
    format of a `printf` (`cat <<EOF | bash`, `echo "go list" | sh`). word is as
    shell_inner_scripts takes it, for both the shell and the command that feeds it."""
    command = commands[index]
    found = shell_args(command.values(), word(command))
    if command.pipe == 0 or found is None or found[1] or found[0] < len(command.words):
        return []
    feeder = next((c for c in reversed(commands[:index]) if c.pipe == command.pipe - 1
                   and c.frames == command.frames and c.parent is command.parent), None)
    k = word(feeder) if feeder is not None else None
    if k is None:
        return []
    values = feeder.values()
    args = [v for v in values[k + 1:] if not re.fullmatch(r"-[neE]+", v)]
    if values[k] == "cat" and not args:
        return [(doc[3], doc[4]) for doc in feeder.heredocs]
    if values[k] == "echo" and args:
        return [(" ".join(args), feeder.words[k + 1].line)]
    if values[k] == "printf" and args:
        return [(args[0].replace("\\n", "\n"), feeder.words[k + 1].line)]
    return []
