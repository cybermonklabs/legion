"""Rule 1 of check-go-offline.sh: each place a file starts a go command, in each language, and
whether the command runs there with GOPROXY=off. A site is (0-based line, offline); which file is
read, and what a reason is, are the check's own; how a language is read is the source_*.py modules'.

Imported with PYTHONPATH naming the directory this file lives in. Its test is
check-go-offline.test.sh, with the cases it sources.
"""
import ast
import os
import re

import yaml

import source_scan as scan
from source_env import env_elements, go_cmd_env, ts_options_env
from source_python import python_bound, python_env_elements, python_scope, python_text, python_tree
from source_shell import (ShWord, command_word, env_settings, shell_c_script, shell_command_word, shell_env_elements,
                          shell_inner_scripts, shell_script, shell_stdin_scripts)
from source_statements import Source

SUBCOMMANDS = {"build", "run", "list", "install", "get", "mod", "test", "vet", "env", "generate", "work", "version"}
REASON = re.compile(r"\bgo-offline:\s*\S")
# A file the reading below could find no site in is not read a statement at a time: its text holds
# none of what every site is spelled with. Go: an exec.Command, an exec.LookPath, or a "go" a
# filepath.Join ends in. TypeScript: a spawn or an exec of child_process or execa, Bun's $`...`, a
# go a string ends in, or one of those imported under another name. Python: a go a string ends
# in, or a go command in a string. Shell: a go, a path ending /go or a variable followed by a
# subcommand, -C DIR between them.
_SUBCOMMAND = r"(?:" + "|".join(sorted(SUBCOMMANDS)) + r")\b"
_GAP = r"(?:\s|\\\n)+"
MAYBE = {
    "go": re.compile(r'exec\.(?:Command|LookPath)|"go"'),
    "ts": re.compile(r"\b(?:spawn|spawnSync|execFile\w*|execa\w*|exec|execSync|execAsync)\s*\(|\$\s*`|go[\"'`]"
                     r"|(?:\b(?:spawn\w*|exec\w*)|\$)\s*(?:as\s|:)"),
    "py": re.compile(r"go[\"']|\bgo\s+(?:-C\S*\s+\S+\s+)?" + _SUBCOMMAND),
    "sh": re.compile(r"(?:\bgo|/go|\$\{?\w+\}?)[\"'})]*" + _GAP + r"(?:-C(?:=\S*|" + _GAP + r"\S+)" + _GAP + r")?[\"']?"
                     + _SUBCOMMAND),
}


def offline(elements):
    """Whether the last element that can set GOPROXY sets it to off: a later GOPROXY, the inherited
    environment or one nothing here can read after it leaves the command online."""
    for element in reversed(elements):
        if element[0] != "set":
            return False
        if element[1] == "GOPROXY":
            return element[2] == "off"
    return False


def goish_text(text):
    """Whether a program's name, as text with each expansion as \\x00, is the go binary: go, or a
    path ending /go (`/usr/local/go/bin/go`, `$GOROOT/bin/go`)."""
    return text == "go" or bool(re.fullmatch(r"[\w.\x00/~@+-]*/go", text))


def go_subcommand(values, k):
    """Whether the words after a go at k are an optional -C DIR and a subcommand that loads a module."""
    rest = values[k + 1:]
    j = 2 if rest[:1] == ["-C"] else 1 if rest and rest[0].startswith("-C=") else 0
    return j < len(rest) and rest[j] in SUBCOMMANDS


def argv_program(values):
    """(the index of the program of a Go, TypeScript or Python argv, after the launchers a shell
    command's are read past: env with its settings and options, command, exec, nice, nohup, time,
    timeout and flock with their operands; the settings each env there makes). Its values are the
    elements' literal text, None for an element that is none."""
    k = command_word(values)
    k = len(values) if k is None else k
    return k, env_settings(values, k)


# --- Shell: each simple command, its own settings, and the shell it hands more shell to.

def looks_up_go(commands):
    """Whether the commands of a $(...) look up the go binary: `which go`, `command -v go`, `type -P go`."""
    # go-offline: this check's own reading compares a word with "go" here; it starts nothing.
    return any(c.values()[:1] in (["which"], ["command"], ["type"]) and c.values()[-1:] == ["go"] for c in commands)


def go_word(word, go_names=()):
    """Whether a shell word is the go binary: its value is go or a path ending /go; or, its empty
    parts aside, it is one expansion: $NAME of a name go_names holds, a $(...) that looks the
    binary up, or ${NAME:-go}, whose default is one."""
    if goish_text(word.value()):
        return True
    parts = [(i, p) for i, p in enumerate(word.parts) if p[0] not in ("lit", "sq", "dq") or p[1]]
    if len(parts) != 1:
        return False
    index, (kind, text, nested) = parts[0]
    if kind == "param":
        default = word.defaults.get(index)
        return text in go_names or (default is not None and goish_text(default.strip("\"'")))
    return kind == "cmd" and looks_up_go(nested or ())


def shell_scope(commands, names=("GOPROXY",)):
    """What a shell text's commands (shell_script's list) set up for the go commands after them:
    (the names a variable holding the go binary goes by, the names of arrays whose first word is
    go with a subcommand, and each `export` or `unset` of one of `names`, as (the index of its
    command, the frames that hold it, its element)). An export a forked command makes (in a
    pipeline or the background) changes nothing after it, and is none. shell_sites and rule 7
    (go_offline_fetched.py) read a text's environment through it, with in_scope."""
    go_names, go_arrays, exports, assigned = set(), set(), [], {}
    for order, command in enumerate(commands):
        values = command.values()
        settings = list(command.assigns) if not values else []
        if values and values[0] in ("local", "declare", "typeset", "readonly", "export"):
            for word in command.words[1:]:
                name, eq, _ = word.value().partition("=")
                if eq and re.fullmatch(r"[A-Za-z_]\w*", name):
                    rest = list(word.parts)
                    rest[0] = ("lit", rest[0][1].partition("=")[2], None) if rest and rest[0][0] == "lit" else rest[0]
                    settings.append((name, ShWord(rest, word.start, word.line, word.defaults)))
        for name, value in settings:
            assigned[name] = value
            if go_word(value):
                go_names.add(name)
            held = value.value()
            if held.startswith("(") and held.endswith(")"):  # NAME=(go build ./...), run as "${NAME[@]}"
                array = shell_script(held[1:-1])
                if array and array[0].words and go_word(array[0].words[0]) and go_subcommand(array[0].values(), 0):
                    go_arrays.add(name)
        if (values and values[0] in ("export", "unset") or values[:2] in (["declare", "-x"], ["export", "-n"])) \
                and not command.forked:
            unset = values[0] == "unset" or values[1:2] == ["-n"]
            for word in command.words[1:]:
                name, eq, value = word.value().partition("=")
                if name in names:
                    if unset:
                        element = ("set", name, None)
                    elif eq:
                        element = ("set", name, value if word.static else None)
                    else:  # GOPROXY=off; export GOPROXY
                        held = assigned.get(name)
                        element = ("set", name, held.value() if held is not None and held.static else None)
                    exports.append((order, command.frames, element))
    return go_names, go_arrays, exports


def in_scope(exports, order, frames):
    """The elements of shell_scope's exports that hold for the command at index `order` whose
    frames are `frames`: each made before it, at the top level or in a function or subshell that
    holds it."""
    return [e for at, held, e in exports if at < order and frames[:len(held)] == held]


def shell_sites(text, line=0, base=(), depth=0, names=("GOPROXY",), judge=offline, scripts=frozenset()):
    """(line, offline) of each go command a shell text starts. Its environment: base (what a
    command that ran this text gave it), each `export` or `unset` of GOPROXY before it at the top
    level or in the function or subshell that holds it (none a forked command makes, in a pipeline
    or the background), its own NAME=value prefix and the settings of an `env` before the go.
    go_offline_images.py reads a Dockerfile's RUN through it with more: `names`, the settings an
    export or unset is followed for; `judge`, what a site's environment elements are read as in
    place of offline(); and `scripts`, programs (by path or by name) that start a go command of
    their own, each a site where it is a command's program."""
    commands = shell_script(text, line)
    go_names, go_arrays, exports = shell_scope(commands, names)
    found = []
    for order, command in enumerate(commands):
        values = command.values()
        scope = in_scope(exports, order, command.frames)
        program = shell_command_word(command)
        for k, word in enumerate(command.words):
            array = k == program and len(word.parts) == 1 and word.parts[0][0] == "param" \
                and word.parts[0][1] in go_arrays
            started = scripts and k == program and (word.value() in scripts
                                                    or word.value().rsplit("/", 1)[-1] in scripts)
            if array or started or (go_word(word, go_names) and go_subcommand(values, k)):
                found.append((word.line, judge([*base, *scope, *shell_env_elements(command, k)])))
        if depth < 3:
            own = shell_env_elements(command, len(command.words))
            for inner, inner_line in shell_inner_scripts(command) + shell_stdin_scripts(commands, order):
                found += shell_sites(inner, inner_line, (*base, *scope, *own), depth + 1, names, judge, scripts)
    return found


# --- Go: each exec.Command of the go binary, and the Env of the *exec.Cmd it makes.

def go_sites(src):
    consumed, found = set(), []
    # os/exec by the name the file imports it as: exec, an alias, or no qualifier for a dot import.
    exec_names = {"exec"} | {name for name, path in src.go_imports() if path == "os/exec"}

    def callee(call):
        """A call's callee, os/exec's name in it spelled exec."""
        qualifier, dot, last = call.callee.rpartition(".")
        return "exec." + last if qualifier in exec_names or (not dot and "." in exec_names) else call.callee

    def goish(a, b, depth=0):
        text = src.string(a, b)
        if text is not None:
            return goish_text(text)
        call = src.call_of(a, b)
        if call:
            if (callee(call) == "exec.LookPath" and len(call.args) == 1 and src.string(*call.args[0]) == "go") or \
                    (call.name == "Join" and call.args and src.string(*call.args[-1]) == "go"):
                consumed.add(call.at)
                return True
            return False
        # go-offline: this check's own reading compares a string with "/go" here; it starts nothing.
        parts = src.split(a, b, "+")
        if len(parts) > 1:
            last = src.string(*parts[-1])
            return last is not None and last.endswith("/go")
        name = src.name(a, b)
        if name and depth < 4:
            return any([goish(*r, depth + 1) for r in src.bound_all(name, a)])
        return False

    def text_of(a, b):
        return "".join(src.string(*p) or "\x00" for p in src.split(a, b, "+"))

    for call in src.calls():
        first = {"exec.Command": 0, "exec.CommandContext": 1}.get(callee(call))
        if first is None or len(call.args) <= first:
            continue
        argv = call.args[first:]
        values = [src.string(*r) for r in argv]
        k, settings = argv_program(values)
        env = go_cmd_env(src, call)
        env = [scan.INHERIT] if env is None else env
        if k < len(argv) and goish(*argv[k]):
            found.append((src.line(call.at), offline(env + settings)))
            continue
        script = shell_c_script(values)
        if script is not None:
            found += shell_sites(text_of(*argv[script]), src.line(argv[script][0]), env)
    for call in src.calls():
        if call.at in consumed:
            continue
        if (callee(call) == "exec.LookPath" and len(call.args) == 1 and src.string(*call.args[0]) == "go") or \
                (call.name == "Join" and len(call.args) >= 2 and src.string(*call.args[-1]) == "go"
                 and src.string(*call.args[-2]) == "bin"):
            found.append((src.line(call.at), False))  # resolves the go binary something else starts
    return found


# --- TypeScript: each spawn of the go binary, Bun's $ and exec of a shell, and an argv array.

SPAWNS = {"spawn", "spawnSync", "execFile", "execFileSync", "execFileAsync", "execa", "execaSync"}
SHELL_CALLS = {"exec", "execSync", "execAsync"}
# The modules whose names a spawn, an exec or Bun's $ is imported under.
PROCESS = {"child_process", "node:child_process", "execa", "bun"}


def ts_sites(src):
    found, handled = [], set()
    imported = {local: what for local, (module, what) in src.ts_imports().items() if module in PROCESS}
    namespaces = {"child_process", "childProcess", "cp"} | {n for n, what in imported.items() if what in ("*", "default")}

    def called(call):
        """The name a call calls: an alias read as the name it imports (run, of `import { spawnSync
        as run }`, is spawnSync)."""
        what = imported.get(call.callee)
        return what if what not in (None, "*", "default") else call.name

    def strip_bang(a, b):
        return (a, b - 1) if b - a > 1 and src.punct(b - 1, "!") else (a, b)

    def goish(a, b, depth=0):
        a, b = strip_bang(a, b)
        token = src.tokens[a]
        if b - a == 1 and token.kind in ("string", "template"):
            return goish_text(src.template_text(token, lambda sub: "\x00") if token.kind == "template" else token.value)
        for op in ("??", "||"):
            parts = src.split_op(a, b, op)
            if len(parts) > 1:
                return any([goish(*p, depth) for p in parts])
        call = src.call_of(a, b)
        if call:
            return (call.name in ("which", "whichSync") and len(call.args) == 1 and src.string(*call.args[0]) == "go") \
                or (call.name in ("join", "resolve") and bool(call.args) and src.string(*call.args[-1]) == "go")
        name = src.name(a, b)
        if name and depth < 4:
            return any([goish(*r, depth + 1) for r in src.bound_all(name, a)])
        return False

    def substitute(sub):
        """A template substitution as shell text: go when it names the go binary."""
        inner = Source(src.text, "ts", sub)
        name = inner.name(0, len(inner.tokens)) if inner.tokens else None
        return "go" if name and any([goish(*r) for r in src.bound_all(name, len(src.tokens))]) else "\x00"

    def text_of(a, b):
        token = src.tokens[a]
        if b - a == 1 and token.kind == "template":
            return src.template_text(token, substitute)
        return "".join(src.string(*p) or "\x00" for p in src.split(a, b, "+"))

    def argv_site(argv, line, env):
        values = [src.string(*r) for r in argv]
        if values and values[0] is not None and re.search(r"\s", values[0]):
            # spawnSync("go build ./...", { shell: true }): a command line, which a shell reads, as
            # the Oh My Pi check reads it; the arguments after it join it.
            found.extend(shell_sites(" ".join(v if v is not None else "\x00" for v in values), line, env))
            return
        k, settings = argv_program(values)
        if k < len(argv) and goish(*argv[k]):
            found.append((line, offline(env + settings)))
            return
        script = shell_c_script(values)
        if script is not None:
            found.extend(shell_sites(text_of(*argv[script]), src.line(argv[script][0]), env))

    tags = {local for local, what in imported.items() if what == "$"} - {"$"}
    for call in src.calls() + src.tagged_calls(tags):
        line, name = src.line(call.at), called(call)
        if call.tagged:
            # Bun's $`...`: the template is shell; a .env({...}) after it is its environment.
            env = [scan.INHERIT]
            after = call.close + 1
            if src.punct(after, ".") and after + 2 < len(src.tokens) and src.tokens[after + 1].value == "env" \
                    and src.punct(after + 2, "(") and after + 2 in src.partner:
                inner = src.split(after + 3, src.partner[after + 2])
                env = env_elements(src, *inner[0]) if inner else env
            found.extend(shell_sites(text_of(call.open, call.open + 1), line, env))
        elif name in SPAWNS and call.args:
            # Each array the spawn reads is its argv's, so the pass over argv arrays below skips it.
            argv, options = src.ts_spawn_argv(call, handled)
            env = ts_options_env(src, call, options)
            # An argv bound to a name is read where it is spelled, as one given in place is.
            bound = argv and src.name(*call.args[0]) is not None
            argv_site(argv, src.line(argv[0][0]) if bound else line, [scan.INHERIT] if env is None else env)
        elif name in SHELL_CALLS and call.args and (call.callee in (call.name, name)
                                                    or call.callee.rpartition(".")[0] in namespaces):
            env = ts_options_env(src, call, 1)
            found.extend(shell_sites(text_of(*call.args[0]), line, [scan.INHERIT] if env is None else env))
    # An argv array no spawn above read, that starts go and a subcommand: its call's options, if any.
    arguments = {r[0]: call for call in src.calls() for r in call.args}
    for i, token in enumerate(src.tokens):
        if not (token.kind == "punct" and token.value == "[" and i in src.partner) or i in handled:
            continue
        argv = src.split(i + 1, src.partner[i])
        values = [src.string(*r) for r in argv]
        k, settings = argv_program(values)
        if k < len(argv) and goish(*argv[k]) and go_subcommand(values, k):
            holder = arguments.get(i)
            env = ts_options_env(src, holder, 1) if holder else None
            found.append((src.line(i), offline(([scan.INHERIT] if env is None else env) + settings)))
    return found


# --- Python: each argv list or tuple that starts go, each shell string, and a call's env=.

def python_sites(tree):
    consumed, found = set(), []

    def goish(node, depth=0):
        text = python_text(node)
        if text is not None:
            return goish_text(text)
        if isinstance(node, ast.Call):
            callee = ast.unparse(node.func)
            last = callee.rsplit(".", 1)[-1]
            args = [python_text(a) for a in node.args]
            # go-offline: this check's own reading compares a word with "go" here; it starts nothing.
            if (last == "which" and args[:1] == ["go"]) or (last == "join" and args and args[-1] == "go"):
                consumed.add(id(node))
                return True
            return False
        if isinstance(node, ast.Name) and depth < 4:
            bound = python_bound(node.id, node)
            return bound is not None and goish(bound, depth + 1)
        return False

    def text_of(node):
        return python_text(node, lambda value: "go" if goish(value) else "\x00")

    def env_of(call):
        if call is None:
            return [scan.INHERIT]
        env = next((k.value for k in call.keywords if k.arg == "env"), None)
        return [scan.INHERIT] if env is None else python_env_elements(env)

    def receiver(node):
        """The call an argv is given to: the one it is an argument of, or the first call after its
        assignment to a name that is given that name."""
        parent = getattr(node, "parent", None)
        if isinstance(parent, ast.Call) and node in parent.args:
            return parent
        if isinstance(parent, ast.Assign) and len(parent.targets) == 1 and isinstance(parent.targets[0], ast.Name):
            name = parent.targets[0].id
            for other in ast.walk(python_scope(node)):
                if isinstance(other, ast.Call) and other.lineno >= parent.lineno and any(
                        isinstance(a, ast.Name) and a.id == name for a in other.args):
                    return other
        return None

    def argv_site(nodes, line, call):
        values = [python_text(n) for n in nodes]
        k, settings = argv_program(values)
        if k < len(nodes) and goish(nodes[k]):
            found.append((line, offline(env_of(call) + settings)))
            return True
        script = shell_c_script(values)
        if script is not None:
            found.extend(shell_sites(text_of(nodes[script]) or "", nodes[script].lineno - 1, env_of(call)))
            return True
        return False

    for node in ast.walk(tree):
        if isinstance(node, (ast.List, ast.Tuple)) and node.elts:
            argv_site(node.elts, node.lineno - 1, receiver(node))
        elif isinstance(node, ast.Call) and node.args:
            callee = ast.unparse(node.func)
            first = node.args[0]
            if callee.rsplit(".", 1)[-1] not in ("which", "join") and not isinstance(first, (ast.List, ast.Tuple)) \
                    and argv_site(node.args, node.lineno - 1, node):
                continue
            text = text_of(first)
            if text is not None and (callee in ("os.system", "os.popen") or callee.startswith("subprocess.")):
                found.extend(shell_sites(text, first.lineno - 1, env_of(node)))
            elif text is not None and callee == "shlex.split":
                # The words a shell would split the string into, an argv of the call it is given to.
                found.extend(shell_sites(text, first.lineno - 1, env_of(receiver(node))))
    # go-offline: this check's own reading compares a word with "go" here; it starts nothing.
    for node in ast.walk(tree):
        if isinstance(node, ast.Call) and id(node) not in consumed and ast.unparse(node.func).endswith("which") \
                and [python_text(a) for a in node.args[:1]] == ["go"]:
            found.append((node.lineno - 1, False))  # resolves the go binary something else starts
    return found


def sites_of(path, text, lang):
    """(index, offline) of each line of a file that starts a go command: a line is offline when every
    go command it starts is."""
    if lang == "sh":
        found = shell_sites(text)
    elif lang == "py":
        tree = python_tree(text)
        if tree is None:
            return None
        found = python_sites(tree)
    else:
        source = Source(text, lang)
        found = go_sites(source) if lang == "go" else ts_sites(source)
    lines = {}
    for index, is_offline in found:
        lines[index] = lines.get(index, True) and is_offline
    return sorted(lines.items())


def reasoned(raw, index, lang):
    return scan.reasoned(raw, lang, REASON, [index])


# The reasons that stand outside their file: for a site whose file a comment may not change, since
# a cache key hashes its bytes. Read from the tree the check reads, as its other files are.
OUTSIDE = ".github/go-offline-reasons.yaml"


class OutsideReasons:
    """OUTSIDE's entries, each {path, command, reason}: a site of path whose line, stripped, is
    command carries the reason, as a line reason would. Each entry no site needed (its command
    changed, or its file is gone) is refused by stale(), so the list cannot go stale silently."""

    def __init__(self, errors):
        self.entries, self.used = {}, set()
        if not os.path.isfile(OUTSIDE):
            return
        text = scan.read(OUTSIDE)
        try:
            data, node = yaml.safe_load(text), yaml.compose(text)
        except yaml.YAMLError as error:
            errors.append((OUTSIDE, 1, f"a list this check cannot read as YAML ({type(error).__name__}); fix it"))
            return
        if data is None:
            return
        if not isinstance(data, list):
            errors.append((OUTSIDE, 1, "no list of {path, command, reason} entries"))
            return
        for item, entry in zip(node.value, data):
            line = item.start_mark.line + 1
            if not (isinstance(entry, dict) and set(entry) == {"path", "command", "reason"}
                    and all(isinstance(v, str) and v.strip() for v in entry.values())):
                errors.append((OUTSIDE, line, "an entry that is no {path, command, reason}, each one set"))
                continue
            self.entries[(entry["path"], entry["command"].strip())] = line

    def covers(self, path, line):
        """Whether an entry names this site: its file, and its line as the file has it."""
        key = (path, line.strip())
        if key in self.entries:
            self.used.add(key)
        return key in self.entries

    def stale(self, errors):
        for (path, command), line in self.entries.items():
            if (path, command) not in self.used:
                errors.append((OUTSIDE, line, f"the reason for {path} names `{command}`, which is no go command of "
                                              "that file that needs a reason (its command changed, or the file is "
                                              "gone); update the entry or remove it"))
