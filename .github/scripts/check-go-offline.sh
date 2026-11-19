#!/usr/bin/env bash
# No test fetches a Go module while it runs, in any language its job runs it in.
#
# A test that reaches the module proxy or the checksum database fails whenever that service
# fails, on a change that touched nothing it tests: in run 44317642380, envoy-infra-go failed on a
# `go install` of helm inside a test that could not read sum.golang.org. The rule that ends that
# is the same everywhere: a go command a test starts reads the module cache alone (GOPROXY=off),
# and the job fetches what it needs in a step before the tests, tried again on failure
# (.github/scripts/go-fetch.sh). A list of the tests that follow it would go stale the day a new
# one is written, in Go, TypeScript, Python or shell, so this program asks every such place for
# its answer instead. It checks two things.
#
# 1. Every go command a file starts runs with GOPROXY=off, or says why it need not. Each file is
#    read whole, a statement at a time, as its language parses it (a call's arguments across
#    lines, Python through its ast module, shell as simple commands with continued lines joined),
#    so an argv Biome writes one element a line is one argv. The go binary is "go", a path ending
#    /go ("/usr/local/go/bin/go", "$GOROOT/bin/go"), filepath.Join or path.join(..., "go"),
#    exec.LookPath("go"), Bun.which("go"), shutil.which("go"), or a name bound to one of those
#    earlier in the same function (Go), file (TypeScript), function or module (Python) or script
#    (shell, a variable set to it or to $(command -v go)). A site is where it starts:
#      Go          exec.Command and exec.CommandContext of the go binary, with any arguments; of
#                  `env SETTING... go`; of a shell's -c (`bash -c "go build"`), whose script is
#                  read as shell; and an exec.LookPath("go") or filepath.Join(..., "bin", "go")
#                  that no exec.Command of the same function starts
#      TypeScript  Bun.spawn, Bun.spawnSync (an argv, or `cmd:`), spawn, spawnSync, execFile,
#                  execFileSync, execFileAsync, execa and execaSync of the go binary, of `env
#                  SETTING... go` or of a shell's -c; exec, execSync and execAsync of a shell
#                  string; Bun's $`...` (a substitution that names the go binary reads as go,
#                  any other as a word), each read as shell; and any argv array that starts the
#                  go binary and a subcommand (or -C, a directory and a subcommand)
#      Python      an argv list or tuple, or a call's own arguments, that start the go binary,
#                  `env SETTING... go` or a shell's -c; a string os.system, os.popen or any
#                  subprocess function is given, read as shell (an f-string's {...} as a word);
#                  and a shutil.which("go") no argv of the same function starts
#      shell       a word that is the go binary followed by a subcommand that can load a module
#                  (build, run, list, install, get, mod, test, vet, env, generate, work, version:
#                  env and version too, since a go.mod's toolchain line can make either fetch a
#                  toolchain), after an optional -C DIR, anywhere in a simple command (after
#                  `env`, `xargs` or a function of its own), in a $(...) or `...`, in the script
#                  of a shell's -c, in what eval runs, and in a heredoc a shell with no script
#                  operand reads; one word that quotes a whole command (echo "go build"), a
#                  comment, and any other heredoc's body start nothing
#    Comments and the text of string literals are not read for sites, so a fixture or a message
#    that spells a go command starts nothing here. A site passes when it carries one of:
#      the offline rule in its own environment, where the last setting of GOPROXY, after the
#        environment it inherits, is off: in Go the Env of the same *exec.Cmd variable (the last
#        `X.Env = ...` after the call; append(os.Environ(), "GOPROXY=off") passes,
#        append([]string{"GOPROXY=off"}, os.Environ()...) does not); in TypeScript the call's own
#        options' env object, GOPROXY: "off" after every spread (Bun's $ reads a .env({...})
#        chained to it); in Python the call's env= (a dict display after every ** spread, dict()
#        or a name bound to one); in shell the command's own NAME=value prefix or the settings of
#        an `env` before the go, or an `export GOPROXY=off` before it at the top level of the
#        script or in the function or subshell that holds it (one in another function counts
#        for nothing), with no later `unset`, `env -u GOPROXY` or other GOPROXY. A command of a
#        pipeline has its own environment: in `GOPROXY=off go list | xargs go build` the build is
#        online. A command inside a shell's -c, an eval or a heredoc a shell reads starts with the
#        environment of the command that runs it. A "GOPROXY=off" in a comment, a neighbour's
#        environment or a string no command is given counts for nothing;
#      a reason: a comment holding `go-offline: <why>` on its line or within the 5 lines above
#        it, saying why the command reads no module, why it is no test, or which job alone runs
#        it, for a reader to judge;
#      a file reason: a comment holding `go-offline-file: <why>` within the file's first 40
#        lines, which this refuses in a test file and in any file a required workflow names
#        (ci.yaml and every local workflow it calls, whose jobs ci-ok judges), so no file a
#        required job runs can wave its sites through at once.
#    Its limits, each a form this reads as no site: a go binary handed to a function from another
#    function, or held in a struct field or a map; an argv built by spreading another array or
#    joined at run time ([...base, "build"], "go " + verb), but for a string a shell runs; an
#    *exec.Cmd made as a struct literal; and a call inside a template literal's ${...}. A shell
#    function an `export GOPROXY=off` in another function feeds reads as online, and is refused.
#    A Python file its ast module cannot parse is refused, since none of its go commands is seen.
#    Files under a directory named testdata, node_modules, dist, .git or .jj are not read: they
#    are a test's inputs, and the test that runs one is itself a site. Which file is source, in
#    which language, and how each language is read, statement by statement, is
#    .github/scripts/source_scan.py's, which check-omp-offline.sh reads files with too.
#
# 2. Every job that runs a test built through testbuild fetches, in a step before, what that build
#    needs. packages/daemon's testbuild.EnvoyBinary builds envoy commands while a test runs, with
#    GOPROXY=off, and the daemon's own build never loads those commands, so a job that runs such
#    a test and fetches nothing first fails it on a cold or stale module cache. For every job of
#    every workflow (required or not: e2e.yaml's nightly proofs compile and run the same tests),
#    a step that runs go test of a package whose tests call testbuild.EnvoyBinary - in its run:
#    text, or in a script that step runs, or in a file that script sources or runs - must come
#    after a step that runs `.github/scripts/go-fetch.sh deps` of packages/envoy naming every
#    command testbuild.EnvoyBinary is called with. The package's module is the directory `go -C
#    DIR test` or `go test -C DIR` names, or a `cd DIR` earlier on the line moved to, else the
#    step's. A `go test -c -o /dev/null` compiles and runs nothing, and a `go test -list` runs no
#    test: neither is such a step.
#
# It is a presence check: whether a reason is true, and whether a fetch step fetches into the
# cache the test reads, is the reviewer's to judge, as check-fireevent-reasons.sh says of its own.
# A scan that read no file of a language, found no testbuild caller where testbuild exists, or saw
# no step run those callers' tests, reports UNKNOWN and exits 2: a scan that cannot see must not
# read as a clean one.
#
# Run from anywhere: .github/scripts/check-go-offline.sh
# CI runs it in ci.yaml's lint job.
set -euo pipefail

# The shared reading of source files (source_scan.py) sits beside this script's own file, which a
# test may link into a tree of its own: PYTHONPATH names that directory, and the scan reads the
# tree this script stands in. PYTHONDONTWRITEBYTECODE keeps the import from leaving a __pycache__.
scripts=$(cd "$(dirname "$(readlink -f "$0")")" && pwd)
cd "$(dirname "$0")/../.."

PYTHONDONTWRITEBYTECODE=1 PYTHONPATH="$scripts" python3 <<'PY'
import ast
import os
import re
import sys
from pathlib import Path

import yaml

import source_scan as scan

SUBCOMMANDS = {"build", "run", "list", "install", "get", "mod", "test", "vet", "env", "generate", "work", "version"}
REASON = re.compile(r"\bgo-offline:\s*\S")
FILE_REASON = re.compile(r"\bgo-offline-file:\s*\S")
FILE_REASON_LINES = 40
SETTING = re.compile(r"[A-Za-z_]\w*=")


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


def env_words(values):
    """(index of the program after `env` and its settings, the settings) of an argv that starts with
    env, else (0, [])."""
    if not values or values[0] is None or values[0].rsplit("/", 1)[-1] != "env":
        return 0, []
    k, settings = 1, []
    while k < len(values) and values[k] is not None and (values[k].startswith("-") or SETTING.match(values[k])):
        if SETTING.match(values[k]):
            settings.append(("set", *values[k].split("=", 1)))
        elif values[k] in ("-u", "--unset") and k + 1 < len(values):
            settings.append(("set", values[k + 1], None))
            k += 1
        elif values[k] in ("-i", "-"):
            settings.append(scan.CLEAR)
        k += 1
    return k, settings


def shell_c(values):
    """The index of the script after a shell's -c in an argv, else None."""
    if not values or values[0] is None or values[0].rsplit("/", 1)[-1] not in scan.SHELLS:
        return None
    j, takes_c = 1, False
    while j < len(values) and values[j] is not None and values[j].startswith("-"):
        takes_c |= bool(re.fullmatch(r"-[a-zA-Z]*c[a-zA-Z]*", values[j]))
        j += 2 if re.fullmatch(r"[-+][a-zA-Z]*[oO]", values[j]) else 1
    return j if takes_c and j < len(values) else None


# --- Shell: each simple command, its own settings, and the shell it hands more shell to.

def shell_sites(text, line=0, base=(), depth=0):
    """(line, offline) of each go command a shell text starts. Its environment: base (what a
    command that ran this text gave it), each `export` or `unset` of GOPROXY before it at the top
    level or in the function or subshell that holds it, its own NAME=value prefix and the settings
    of an `env` before the go."""
    commands = scan.shell_script(text, line)
    go_names, exports, assigned = set(), [], {}
    for order, command in enumerate(commands):
        values = command.values()
        settings = list(command.assigns) if not values else []
        if values and values[0] in ("local", "declare", "typeset", "readonly", "export"):
            for word in command.words[1:]:
                name, eq, _ = word.value().partition("=")
                if eq and re.fullmatch(r"[A-Za-z_]\w*", name):
                    rest = list(word.parts)
                    rest[0] = ("lit", rest[0][1].partition("=")[2], None) if rest and rest[0][0] == "lit" else rest[0]
                    settings.append((name, scan.ShWord(rest, word.start, word.line)))
        for name, value in settings:
            assigned[name] = value
            inner = [c for kind, _, nested in value.parts if kind == "cmd" for c in nested or ()]
            if goish_text(value.value()) or any(c.values()[:1] in (["which"], ["command"], ["type"])
                                                 and c.values()[-1:] == ["go"] for c in inner):
                go_names.add(name)
        if values and values[0] in ("export", "unset") or values[:2] in (["declare", "-x"], ["export", "-n"]):
            unset = values[0] == "unset" or values[1:2] == ["-n"]
            for word in command.words[1:]:
                name, eq, value = word.value().partition("=")
                if name == "GOPROXY":
                    if unset:
                        element = ("set", name, None)
                    elif eq:
                        element = ("set", name, value if word.static else None)
                    else:  # GOPROXY=off; export GOPROXY
                        held = assigned.get(name)
                        element = ("set", name, held.value() if held is not None and held.static else None)
                    exports.append((order, command.frames, element))
    found = []
    for order, command in enumerate(commands):
        values = command.values()
        scope = [e for at, frames, e in exports if at < order and command.frames[:len(frames)] == frames]
        for k, word in enumerate(command.words):
            named = len(word.parts) == 1 and word.parts[0][0] == "param" and word.parts[0][1] in go_names
            if (named or goish_text(word.value())) and go_subcommand(values, k):
                found.append((word.line, offline([*base, *scope, *scan.shell_env_elements(command, k)])))
        if depth < 3:
            for inner, inner_line in scan.shell_inner_scripts(command):
                own = scan.shell_env_elements(command, len(command.words))
                found += shell_sites(inner, inner_line, (*base, *scope, *own), depth + 1)
    return found


# --- Go: each exec.Command of the go binary, and the Env of the *exec.Cmd it makes.

def go_sites(src):
    consumed, found = set(), []

    def goish(a, b, depth=0):
        text = src.string(a, b)
        if text is not None:
            return goish_text(text)
        call = src.call_of(a, b)
        if call:
            if (call.callee == "exec.LookPath" and len(call.args) == 1 and src.string(*call.args[0]) == "go") or \
                    (call.name == "Join" and call.args and src.string(*call.args[-1]) == "go"):
                consumed.add(call.at)
                return True
            return False
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
        first = {"exec.Command": 0, "exec.CommandContext": 1}.get(call.callee)
        if first is None or len(call.args) <= first:
            continue
        argv = call.args[first:]
        values = [src.string(*r) for r in argv]
        k, settings = env_words(values)
        env = src.go_cmd_env(call)
        env = [scan.INHERIT] if env is None else env
        if k < len(argv) and goish(*argv[k]):
            found.append((src.line(call.at), offline(env + settings)))
            continue
        script = shell_c(values)
        if script is not None:
            found += shell_sites(text_of(*argv[script]), src.line(argv[script][0]), env)
    for call in src.calls():
        if call.at in consumed:
            continue
        if (call.callee == "exec.LookPath" and len(call.args) == 1 and src.string(*call.args[0]) == "go") or \
                (call.name == "Join" and len(call.args) >= 2 and src.string(*call.args[-1]) == "go"
                 and src.string(*call.args[-2]) == "bin"):
            found.append((src.line(call.at), False))  # resolves the go binary something else starts
    return found


# --- TypeScript: each spawn of the go binary, Bun's $ and exec of a shell, and an argv array.

SPAWNS = {"spawn", "spawnSync", "execFile", "execFileSync", "execFileAsync", "execa", "execaSync"}
SHELL_CALLS = {"exec", "execSync", "execAsync"}


def ts_sites(src):
    found, handled = [], set()

    def strip_bang(a, b):
        return (a, b - 1) if b - a > 1 and src.punct(b - 1, "!") else (a, b)

    def goish(a, b, depth=0):
        a, b = strip_bang(a, b)
        token = src.tokens[a]
        if b - a == 1 and token.kind in ("string", "template"):
            return goish_text(src.template_text(token, lambda sub: "\x00") if token.kind == "template" else token.value)
        for op in ("??", "||"):
            parts = [p for p in split_op(a, b, op)]
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

    def split_op(a, b, op):
        """[a, b) split at each top-level two-character operator op (?? or ||)."""
        out, start, i = [], a, a
        while i < b:
            if src.tokens[i].kind == "punct" and src.tokens[i].value in ("(", "[", "{") and i in src.partner:
                i = src.partner[i] + 1
                continue
            if i + 1 < b and src.punct(i, op[0]) and src.punct(i + 1, op[1]) \
                    and src.tokens[i].end == src.tokens[i + 1].start:
                out.append((start, i))
                start = i + 2
                i += 2
                continue
            i += 1
        out.append((start, b))
        return out

    def substitute(sub):
        """A template substitution as shell text: go when it names the go binary."""
        inner = scan.Source(src.text, "ts", sub)
        name = inner.name(0, len(inner.tokens)) if inner.tokens else None
        return "go" if name and any([goish(*r) for r in src.bound_all(name, len(src.tokens))]) else "\x00"

    def text_of(a, b):
        token = src.tokens[a]
        if b - a == 1 and token.kind == "template":
            return src.template_text(token, substitute)
        return "".join(src.string(*p) or "\x00" for p in src.split(a, b, "+"))

    def elements(a, b, depth=0):
        """The element ranges of an array literal, or of the array a name is bound to, else None."""
        a, b = strip_bang(a, b)
        if src.punct(a, "[") and src.partner.get(a) == b - 1:
            handled.add(a)
            return src.split(a + 1, b - 1)
        name = src.name(a, b)
        if name and depth < 4:
            bound = src.bound(name, a)
            if bound:
                return elements(*bound, depth + 1)
        return None

    def argv_site(argv, line, env):
        values = [src.string(*r) for r in argv]
        k, settings = env_words(values)
        if k < len(argv) and goish(*argv[k]):
            found.append((line, offline(env + settings)))
            return
        script = shell_c(values)
        if script is not None:
            found.extend(shell_sites(text_of(*argv[script]), src.line(argv[script][0]), env))

    for call in src.calls():
        line = src.line(call.at)
        if call.tagged:
            # Bun's $`...`: the template is shell; a .env({...}) after it is its environment.
            env = [scan.INHERIT]
            after = call.close + 1
            if src.punct(after, ".") and after + 2 < len(src.tokens) and src.tokens[after + 1].value == "env" \
                    and src.punct(after + 2, "(") and after + 2 in src.partner:
                inner = src.split(after + 3, src.partner[after + 2])
                env = src.env_elements(*inner[0]) if inner else env
            found.extend(shell_sites(text_of(call.open, call.open + 1), line, env))
        elif call.name in SPAWNS and call.args:
            argv, options = elements(*call.args[0]), 1
            if argv is None and src.punct(call.args[0][0], "{"):  # Bun.spawn({ cmd: [...], env })
                a, b = call.args[0]
                for pa, pb in src.split(a + 1, b - 1):
                    if src.tokens[pa].value == "cmd" and src.punct(pa + 1, ":"):
                        argv, options = elements(pa + 2, pb), 0
            if argv is None:
                rest = elements(*call.args[1]) if len(call.args) > 1 else None
                argv, options = [call.args[0]] + (rest or []), 2 if rest is not None else 1
            env = src.ts_options_env(call, options)
            # An argv bound to a name is read where it is spelled, as one given in place is.
            bound = argv and src.name(*call.args[0]) is not None
            argv_site(argv, src.line(argv[0][0]) if bound else line, [scan.INHERIT] if env is None else env)
        elif call.name in SHELL_CALLS and call.args and call.callee in (
                call.name, f"child_process.{call.name}", f"childProcess.{call.name}", f"cp.{call.name}"):
            env = src.ts_options_env(call, 1)
            found.extend(shell_sites(text_of(*call.args[0]), line, [scan.INHERIT] if env is None else env))
    # An argv array no spawn above read, that starts go and a subcommand: its call's options, if any.
    arguments = {r[0]: call for call in src.calls() for r in call.args}
    for i, token in enumerate(src.tokens):
        if not (token.kind == "punct" and token.value == "[" and i in src.partner) or i in handled:
            continue
        argv = src.split(i + 1, src.partner[i])
        values = [src.string(*r) for r in argv]
        k, settings = env_words(values)
        if k < len(argv) and goish(*argv[k]) and go_subcommand(values, k):
            holder = arguments.get(i)
            env = src.ts_options_env(holder, 1) if holder else None
            found.append((src.line(i), offline(([scan.INHERIT] if env is None else env) + settings)))
    return found


# --- Python: each argv list or tuple that starts go, each shell string, and a call's env=.

def python_sites(tree):
    consumed, found = set(), []

    def goish(node, depth=0):
        text = scan.python_text(node)
        if text is not None:
            return goish_text(text)
        if isinstance(node, ast.Call):
            callee = ast.unparse(node.func)
            last = callee.rsplit(".", 1)[-1]
            args = [scan.python_text(a) for a in node.args]
            if (last == "which" and args[:1] == ["go"]) or (last == "join" and args and args[-1] == "go"):
                consumed.add(id(node))
                return True
            return False
        if isinstance(node, ast.Name) and depth < 4:
            bound = scan.python_bound(node.id, node)
            return bound is not None and goish(bound, depth + 1)
        return False

    def text_of(node):
        return scan.python_text(node, lambda value: "go" if goish(value) else "\x00")

    def env_of(call):
        if call is None:
            return [scan.INHERIT]
        env = next((k.value for k in call.keywords if k.arg == "env"), None)
        return [scan.INHERIT] if env is None else scan.python_env_elements(env)

    def receiver(node):
        """The call an argv is given to: the one it is an argument of, or the first call after its
        assignment to a name that is given that name."""
        parent = getattr(node, "parent", None)
        if isinstance(parent, ast.Call) and node in parent.args:
            return parent
        if isinstance(parent, ast.Assign) and len(parent.targets) == 1 and isinstance(parent.targets[0], ast.Name):
            name = parent.targets[0].id
            for other in ast.walk(scan.python_scope(node)):
                if isinstance(other, ast.Call) and other.lineno >= parent.lineno and any(
                        isinstance(a, ast.Name) and a.id == name for a in other.args):
                    return other
        return None

    def argv_site(nodes, line, call):
        values = [scan.python_text(n) for n in nodes]
        k, settings = env_words(values)
        if k < len(nodes) and goish(nodes[k]):
            found.append((line, offline(env_of(call) + settings)))
            return True
        script = shell_c(values)
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
    for node in ast.walk(tree):
        if isinstance(node, ast.Call) and id(node) not in consumed and ast.unparse(node.func).endswith("which") \
                and [scan.python_text(a) for a in node.args[:1]] == ["go"]:
            found.append((node.lineno - 1, False))  # resolves the go binary something else starts
    return found


def sites_of(path, text, lang):
    """(index, offline) of each line of a file that starts a go command: a line is offline when every
    go command it starts is."""
    if lang == "sh":
        found = shell_sites(text)
    elif lang == "py":
        tree = scan.python_tree(text)
        if tree is None:
            return None
        found = python_sites(tree)
    else:
        source = scan.Source(text, lang)
        found = go_sites(source) if lang == "go" else ts_sites(source)
    lines = {}
    for index, is_offline in found:
        lines[index] = lines.get(index, True) and is_offline
    return sorted(lines.items())


def reasoned(raw, index, lang):
    return scan.reasoned(raw, lang, REASON, [index])


def required_workflows():
    """ci.yaml and every local workflow it calls, followed through each called file, as their
    text with comment lines taken out."""
    names, todo = set(), [".github/workflows/ci.yaml"]
    while todo:
        path = todo.pop()
        if path in names or not os.path.isfile(path):
            continue
        names.add(path)
        todo += re.findall(r"uses:\s*\./(\.github/workflows/[\w.-]+\.ya?ml)", Path(path).read_text())
    return {p: "\n".join(l for l in Path(p).read_text().splitlines() if not l.lstrip().startswith("#")) for p in names}


def named_by(path, workflows):
    word = re.compile(r"(?<![\w.-])" + re.escape(os.path.basename(path)) + r"(?![\w-])")
    return sorted(w for w, text in workflows.items() if path in text or word.search(text))


errors = []
counts = dict.fromkeys(("go", "ts", "py", "sh"), 0)
read = dict.fromkeys(("go", "ts", "py", "sh"), 0)
offline_count = reason_count = file_reason_count = 0
workflows = required_workflows()
if not workflows:
    scan.unknown("no .github/workflows/ci.yaml here, so no workflow is known to be required")
for path, lang in scan.sources():
    read[lang] += 1
    text = scan.read(path)
    if "go" not in text:
        continue
    raw = text.split("\n")
    found = sites_of(path, text, lang)
    if found is None:
        errors.append((path, 1, "a Python file this check cannot parse, so its go commands are unseen; fix its syntax"))
        continue
    if not found:
        continue
    file_reason = any(scan.COMMENT[lang].match(l) and FILE_REASON.search(l) for l in raw[:FILE_REASON_LINES])
    if file_reason:
        refusal = ""
        if scan.is_test_file(path):
            refusal = "it is a test file"
        elif named_by(path, workflows):
            refusal = "a required workflow names it (" + ", ".join(named_by(path, workflows)) + ")"
        if refusal:
            errors.append((path, 1, f"a go-offline-file reason, which this file may not carry: {refusal}; "
                                    "give each of its go commands its own"))
            file_reason = False
    for index, is_offline in found:
        counts[lang] += 1
        if is_offline:
            offline_count += 1
        elif reasoned(raw, index, lang):
            reason_count += 1
        elif file_reason:
            file_reason_count += 1
        else:
            errors.append((path, index + 1, "a go command with no GOPROXY=off and no go-offline reason: "
                           + raw[index].strip()))

blind = [lang for lang, n in read.items() if n == 0]
if blind:
    scan.unknown(f"read no {', '.join(blind)} file under {os.getcwd()}, so their go commands are unseen")

# --- 2. The jobs that run a test built through testbuild fetch its commands first.
TESTBUILD = "packages/daemon/internal/testbuild/testbuild.go"
ENVOY_BINARY = re.compile(r'\btestbuild\.EnvoyBinary\(\s*\w+\s*,\s*"([^"]+)"\s*\)')
SEPARATORS = scan.SHELL_SEPARATORS
COMMAND_PREFIXES = {"if", "then", "do", "else", "elif", "while", "until", "!", "{", "time", "exec", "command", "env"}
built, users = set(), set()
for directory, dirs, files in os.walk("packages/daemon"):
    dirs[:] = [d for d in dirs if d not in scan.SKIP_DIRS]
    for name in files:
        if name.endswith("_test.go"):
            calls = ENVOY_BINARY.findall(scan.read(os.path.join(directory, name)))
            if calls:
                built.update(calls)
                users.add(os.path.normpath(directory))


def repo_path(raw_path, base="."):
    raw_path = raw_path.strip("\"'")
    for prefix in ("$root/", "${root}/", "$repo_root/", "${repo_root}/", "$GITHUB_WORKSPACE/", "${GITHUB_WORKSPACE}/"):
        if raw_path.startswith(prefix):
            return os.path.normpath(raw_path[len(prefix):])
    return os.path.normpath(os.path.join(base, raw_path))


def go_tests(raw_line, workdir):
    """The (module directory, package arguments, listed) of each go test of a line that runs
    tests: a -c build that is not to /dev/null runs them later, a -list runs none. The module is
    the directory a `-C DIR` names, before `test` or right after it, else the one a `cd DIR` at a
    command position earlier on the line moved to (until the end of the ( ) group it is in), else
    the step's."""
    tokens, found = scan.shell_words(raw_line), []
    here, groups, command = workdir, [], True
    for k, token in enumerate(tokens):
        if token in ("(", "$("):
            groups.append(here)
        elif token == ")" and groups:
            here = groups.pop()
        if token in SEPARATORS:
            command = True
            continue
        if command and token == "cd" and k + 1 < len(tokens) and tokens[k + 1] not in SEPARATORS:
            here = repo_path(tokens[k + 1], here)
        command = False
        if token != "go" and not token.endswith("/go"):
            continue
        rest, module = tokens[k + 1:], here
        if rest[:1] == ["-C"] and len(rest) > 1:
            module, rest = repo_path(rest[1], here), rest[2:]
        if rest[:1] != ["test"]:
            continue
        args = []
        for t in rest[1:]:
            if t in SEPARATORS:
                break
            args.append(t)
        if args[:1] == ["-C"] and len(args) > 1:
            module, args = repo_path(args[1], here), args[2:]
        if "-list" in args or ("-c" in args and any(a == "/dev/null" for a in args)):
            continue
        packages = [a for a in args if a.startswith("./") or a == "."]
        found.append((module, packages, not packages or "xargs" in tokens[:k]))
    return found


def scripts_of(raw_line, workdir):
    """The files a line sources or runs, each as the command of the line or of a part of it
    after ;, &&, ||, |, ( or $(: `. file`, `source file`, `bash file`, `sh file`, or a path
    ending .sh. A word that is only an argument names a file the line reads, not one it runs.
    A $VAR in a path matches any name there."""
    tokens, named, command = scan.shell_words(raw_line), [], True
    for k, token in enumerate(tokens):
        if token in SEPARATORS:
            command = True
            continue
        if command and (re.match(r"^\w+=", token) or token in COMMAND_PREFIXES):
            continue
        if command and token in (".", "source", "bash", "sh") and k + 1 < len(tokens):
            named.append(tokens[k + 1])
        elif command and token.endswith(".sh"):
            named.append(token)
        command = False
    paths = []
    for name in named:
        for base in (workdir, "."):
            candidate = repo_path(name, base)
            if "$" in candidate:
                pattern = re.sub(r"\$\{?\w+\}?", "*", candidate)
                paths += sorted(str(p) for p in Path(".").glob(pattern) if p.is_file() and p.suffix == ".sh")
            elif os.path.isfile(candidate):
                paths.append(candidate)
    return paths


def runs_testbuild_tests(text, workdir, seen):
    code = scan.shell_code(text)
    for raw_line, code_line in zip(text.split("\n"), code):
        if not code_line.strip():
            continue
        if re.search(r"(?<![\w./-])go\b", code_line):
            for module, packages, listed in go_tests(raw_line, workdir):
                for user in users:
                    if listed and (user == module or user.startswith(module.rstrip("/") + "/")) and module != ".":
                        return True
                    for arg in packages:
                        pkg = os.path.normpath(os.path.join(module, arg))
                        if pkg == user or (arg.endswith("/...") and user.startswith(pkg[:-3].rstrip("/") + "/")) \
                                or (arg == "./..." and user.startswith(module.rstrip("/") + "/")):
                            return True
        for script in scripts_of(raw_line, workdir):
            if script in seen:
                continue
            seen.add(script)
            if runs_testbuild_tests(scan.read(script), ".", seen):
                return True
    return False


def fetches(text, workdir):
    for raw_line, code_line in zip(text.split("\n"), scan.shell_code(text)):
        if "go-fetch.sh" not in code_line:
            continue
        tokens = scan.shell_words(raw_line)
        for k, token in enumerate(tokens):
            if token.endswith("go-fetch.sh") and tokens[k + 1:k + 2] == ["deps"] and len(tokens) > k + 2:
                packages = set()
                for t in tokens[k + 3:]:
                    if t in SEPARATORS:
                        break
                    packages.add(t)
                if repo_path(tokens[k + 2], workdir) == "packages/envoy" and built <= packages:
                    return True
    return False


jobs_checked = 0
if os.path.isfile(TESTBUILD):
    if not users:
        scan.unknown(f"{TESTBUILD} exists and no daemon test calls testbuild.EnvoyBinary with a command, so the "
                     "jobs that run one are unseen")
    for workflow in sorted(Path(".github/workflows").glob("*.y*ml")):
        document = yaml.safe_load(workflow.read_text()) or {}
        default_dir = ((document.get("defaults") or {}).get("run") or {}).get("working-directory", ".")
        for job_id, job in (document.get("jobs") or {}).items():
            if not isinstance(job, dict):
                continue
            job_dir = ((job.get("defaults") or {}).get("run") or {}).get("working-directory", default_dir)
            fetched = False
            for step in job.get("steps") or []:
                run = step.get("run")
                if not isinstance(run, str):
                    continue
                workdir = os.path.normpath(step.get("working-directory", job_dir))
                if runs_testbuild_tests(run, workdir, set()):
                    jobs_checked += 1
                    if not fetched:
                        name = step.get("name") or run.strip().splitlines()[0]
                        errors.append((str(workflow), 1,
                                       f"job {job_id}, step \"{name}\", runs the tests of {', '.join(sorted(users))}, "
                                       f"whose testbuild.EnvoyBinary builds {' '.join(sorted(built))} with GOPROXY=off, "
                                       f"and no step before it fetches them: add `.github/scripts/go-fetch.sh deps "
                                       f"packages/envoy {' '.join(sorted(built))}` before it"))
                if fetches(run, workdir):
                    fetched = True
    # A tree where tests call testbuild and no step of any workflow runs them is one whose steps
    # this reading cannot follow (a go test, cd or script it does not see), not a clean one.
    if jobs_checked == 0:
        scan.unknown(f"{', '.join(sorted(users))} call testbuild.EnvoyBinary and no step of any workflow was seen "
                     "to run their tests, so the jobs that run one are unseen")

total = sum(counts.values())
summary = (f"{total} go commands of {sum(read.values())} files read (Go {counts['go']}, TypeScript {counts['ts']}, "
           f"Python {counts['py']}, shell {counts['sh']})")
if errors:
    scan.refuse(errors, summary, "A test fetches no module while it runs. Run the go command with GOPROXY=off and "
                "fetch what it needs in a step before the tests (.github/scripts/go-fetch.sh deps DIR PACKAGE...), "
                "or, where it reads no module or runs in no test of a required job, say so beside it in a "
                "`go-offline: <why>` comment.")
print(f"every one of the {summary} runs with GOPROXY=off ({offline_count}), carries a reason ({reason_count}) or "
      f"is in a file whose reason covers it ({file_reason_count}); each of the {jobs_checked} steps that run "
      f"testbuild's tests comes after its fetch")
PY
