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
# its answer instead. It checks three things.
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
# 2. Every job fetches, in a step before, what each offline build its steps reach needs. For every
#    job of every workflow (required or not: e2e.yaml's nightly proofs and the nightly re-record
#    build offline too), each step's run text is walked as bash runs it: the directory each `cd`
#    moves to ($(dirname "$0"), $(cd X && pwd) and a variable set to either are read), each script
#    a command runs or sources, each function it calls with that call's arguments, the environment
#    an export or a prefix gives it, and each shell script a TypeScript file under a Playwright
#    run's directory (or under the paths a bun test run names) names in a string. What it reaches:
#      a go test of a package whose tests call packages/daemon's testbuild.EnvoyBinary, which
#        builds envoy commands with GOPROXY=off while the test runs: a step before must run
#        `.github/scripts/go-fetch.sh deps packages/envoy` naming every command it is called with.
#        A `go test -c -o /dev/null` compiles and runs nothing, and a `go test -list` runs no test;
#      an offline go build, install, run, list or generate (GOPROXY=off by the rules of 1), in a
#        module of the repository: a step before must fetch that module's named packages (a
#        go-fetch.sh deps of that module), or a tool pin with go-fetch.sh tools; and a goCommand of
#        a TypeScript file such a run reaches, its `packages:` fetched in any module.
#    A step that needs none of it says why in a `# go-offline: <why>` line of its run text or of
#    the 5 lines above its name (one whose job has no Go and runs binaries another job built).
#    Its limits: a build whose directory this walk cannot place (a scratch copy) or whose packages
#    are not literal, a goCommand whose packages are not, and a Go test's own offline build other
#    than testbuild's are no need it holds a step to; a bun run of a package script is followed
#    no further.
#
# 3. In every job of a required workflow, each step that runs go-fetch.sh (deps or tools) comes
#    after a step of the same job that restores the module cache: actions/cache/restore or
#    actions/cache with ~/go/pkg/mod in its path. A fetch with no restore before it downloads every
#    module the job's cache would have held, on every run. The check lists each such fetch step
#    with the restore before it.
#
# It is a presence check: whether a reason is true, and whether a fetch step fetches into the
# cache the test reads, is the reviewer's to judge, as check-fireevent-reasons.sh says of its own.
# A scan that read no file of a language, found no testbuild caller where testbuild exists, or saw
# no step run those callers' tests, reports UNKNOWN and exits 2: a scan that cannot see must not
# read as a clean one. A file whose text holds nothing a site of its language is spelled with (an
# exec.Command, a spawn, a go a string ends in, a go followed by a subcommand: MAYBE below) is not
# read a statement at a time, which keeps the scan to the files that can start go.
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
# A file the reading below could find no site in is not read a statement at a time: its text holds
# none of what every site is spelled with. Go: an exec.Command, an exec.LookPath, or a "go" a
# filepath.Join ends in. TypeScript: a spawn or an exec of child_process or execa, Bun's $`...`, or
# a go a string ends in. Python: a go a string ends in, or a go command in a string. Shell: a go,
# a path ending /go or a variable followed by a subcommand, -C DIR between them.
_SUBCOMMAND = r"(?:" + "|".join(sorted(SUBCOMMANDS)) + r")\b"
_GAP = r"(?:\s|\\\n)+"
MAYBE = {
    "go": re.compile(r'exec\.(?:Command|LookPath)|"go"'),
    "ts": re.compile(r"\b(?:spawn|spawnSync|execFile\w*|execa\w*|exec|execSync|execAsync)\s*\(|\$\s*`|go[\"'`]"),
    "py": re.compile(r"go[\"']|\bgo\s+(?:-C\S*\s+\S+\s+)?" + _SUBCOMMAND),
    "sh": re.compile(r"(?:\bgo|/go|\$\{?\w+\}?)[\"'}]*" + _GAP + r"(?:-C(?:=\S*|" + _GAP + r"\S+)" + _GAP + r")?[\"']?"
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


# --- What rule 2 reads: the jobs' steps, the scripts and functions they run, and the offline builds.
TESTBUILD = "packages/daemon/internal/testbuild/testbuild.go"
ENVOY_BINARY = re.compile(r'\btestbuild\.EnvoyBinary\(\s*\w+\s*,\s*"([^"]+)"\s*\)')
# The subcommands of an offline go command that build what they name. A go test or vet a script
# runs is that step's own test, not a build one of its tests makes.
BUILDS = {"build", "install", "run", "list", "generate"}
# A go flag whose next word is its value, not a package.
FLAGS_WITH_VALUES = {"-o", "-C", "-ldflags", "-gcflags", "-asmflags", "-tags", "-run", "-skip", "-timeout", "-count",
                     "-p", "-mod", "-modfile", "-exec", "-bench", "-benchtime", "-coverprofile", "-cpuprofile",
                     "-memprofile", "-cpu", "-parallel", "-f", "-overlay", "-pkgdir", "-toolexec", "-covermode",
                     "-coverpkg", "-outputdir", "-buildmode", "-installsuffix"}
RUNNERS = (["bun", "test"], ["bunx", "playwright", "test"], ["npx", "playwright", "test"], ["bun", "x", "playwright", "test"])
TEST_PATH = re.compile(r"\.(?:test|spec|e2e)\.[cm]?[jt]sx?$")
REPO = "/@repo"
built, users = set(), set()
for directory, dirs, files in os.walk("packages/daemon"):
    dirs[:] = [d for d in dirs if d not in scan.SKIP_DIRS]
    for name in files:
        if name.endswith("_test.go"):
            calls = ENVOY_BINARY.findall(scan.read(os.path.join(directory, name)))
            if calls:
                built.update(calls)
                users.add(os.path.normpath(directory))


def norm(path):
    """A path made whole: inside the repository as /@repo/<relative>, a path outside it as itself,
    above the repository's root None."""
    if path is None:
        return None
    if path == REPO or path.startswith(REPO + "/"):
        relative = os.path.normpath(path[len(REPO):].lstrip("/") or ".")
        if relative == ".." or relative.startswith("../"):
            return None
        return REPO if relative == "." else f"{REPO}/{relative}"
    return os.path.normpath(path) if path.startswith("/") else None


def join(base, path):
    if path is None:
        return None
    if path.startswith("/"):
        return norm(path)
    return norm(f"{base}/{path}") if base else None


def in_repo(path):
    """A whole path as the repository names it (packages/envoy, or . for its root), else None."""
    if path is None or not (path == REPO or path.startswith(REPO + "/")):
        return None
    return path[len(REPO):].lstrip("/") or "."


class Need(tuple):
    """An offline build's need: its module as the repository names it (None when this reading
    cannot tell), the packages or tool pins it builds, and where."""

    __slots__ = ()

    def __new__(cls, module, packages, where):
        return super().__new__(cls, (module, frozenset(packages), where))

    module = property(lambda self: self[0])
    packages = property(lambda self: self[1])
    where = property(lambda self: self[2])


class Reach:
    """What one step reaches: the needs of the offline builds it runs, whether it runs tests built
    through testbuild, and the fetches it makes."""

    def __init__(self):
        self.needs, self.testbuild, self.fetches, self.tools = [], False, {}, set()


# Filled by rule 1's walk: every shell file, by name, and what each TypeScript file starts.
shell_files, ts_scripts, ts_go_needs = {}, {}, {}
SCRIPT_NAME = re.compile(r"[\w.-]+\.sh\b")


def index_typescript(path, text):
    """The shell scripts a TypeScript file names in its strings, and the packages each of its
    goCommand and goCommandSync calls builds, where its `packages:` is a list of strings."""
    if ".sh" in text:
        names = {name for token in scan.lex(text, "ts") if token.kind in ("string", "template")
                 for name in SCRIPT_NAME.findall(token.value)}
        if names:
            ts_scripts[path] = names
    if "goCommand" in text:
        src = scan.Source(text, "ts")
        for call in src.calls():
            if call.name not in ("goCommand", "goCommandSync") or not call.args:
                continue
            a, b = call.args[0]
            if not (src.punct(a, "{") and src.partner.get(a) == b - 1):
                continue
            for pa, pb in src.split(a + 1, b - 1):
                if src.tokens[pa].value == "packages" and src.punct(pa + 1, ":") and src.punct(pa + 2, "[") \
                        and src.partner.get(pa + 2) == pb - 1:
                    packages = [src.string(*r) for r in src.split(pa + 3, pb - 1)]
                    if packages and None not in packages:
                        ts_go_needs.setdefault(path, []).append(
                            Need(None, packages, f"{path}:{src.line(call.at) + 1} (goCommand)"))


class Walk:
    """Follows a step's shell the way bash runs it, far enough to see where each go command runs:
    the directory each `cd` moves to (a path, $(dirname "$0"), $(cd X && pwd), a variable set to
    one), the arguments a function or a script is called with, the environment an export or a
    prefix gives it, each script a command runs or sources and each function it calls, and the
    scripts a bun or Playwright test run's TypeScript names."""

    def __init__(self):
        self.parsed, self.memo = {}, {}

    def remembered(self, key, reach, walk):
        """Walk a script or a function once for each place, arguments and environment it is run
        with, into a Reach of its own, and add what that found to reach. A walk under way counts as
        empty, so a function that calls itself ends."""
        found = self.memo.get(key)
        if found is None:
            found = self.memo[key] = Reach()
            walk(found)
        reach.needs += found.needs
        reach.testbuild |= found.testbuild
        for module, packages in found.fetches.items():
            reach.fetches.setdefault(module, set()).update(packages)
        reach.tools |= found.tools

    def commands(self, path):
        if path not in self.parsed:
            self.parsed[path] = scan.shell_script(scan.read(path)) if os.path.isfile(path) else []
        return self.parsed[path]

    def resolve(self, word, cwd, path, params, vars_):
        """A word's value with its expansions read, or None where one cannot be."""
        out = []
        for kind, text, nested in word.parts:
            if kind in ("lit", "sq", "dq"):
                out.append(text)
            elif kind == "param":
                if text.isdigit():
                    value = (REPO + "/" + path if text == "0" else
                             params[int(text) - 1] if int(text) <= len(params) else None)
                elif text == "BASH_SOURCE":
                    value = REPO + "/" + path
                elif text in ("GITHUB_WORKSPACE", "PWD"):
                    value = REPO if text == "GITHUB_WORKSPACE" else cwd
                else:
                    value = vars_.get(text)
                if value is None:
                    return None
                out.append(value)
            elif kind == "cmd":
                value = self.substitute(nested, cwd, path, params, vars_)
                if value is None:
                    return None
                out.append(value)
            else:
                return None
        return "".join(out)

    def substitute(self, nested, cwd, path, params, vars_):
        """The output of a $(...) that names a directory: dirname X, cd X && pwd, pwd, realpath X,
        readlink -f X and git rev-parse --show-toplevel."""
        if not nested:
            return None
        depth = min(len(c.frames) for c in nested)
        direct = [c for c in nested if len(c.frames) == depth]
        values = [[self.resolve(w, cwd, path, params, vars_) for w in c.words] for c in direct]
        if [v[:1] for v in values] == [["cd"], ["pwd"]]:
            target = [v for v in values[0][1:] if v not in ("--", "-P", "-L")]
            return join(cwd, target[0]) if target and target[0] is not None else None
        if len(values) != 1 or not values[0]:
            return None
        verb, args = values[0][0], [v for v in values[0][1:] if v not in ("--", "-f", "-P")]
        if verb == "pwd":
            return cwd
        if verb == "dirname" and args and args[0] is not None:
            whole = join(cwd, args[0])
            return os.path.dirname(whole) if whole else None
        if verb in ("realpath", "readlink") and args and args[0] is not None:
            return join(cwd, args[0])
        if values[0][:3] == ["git", "rev-parse", "--show-toplevel"]:
            return REPO
        return None

    def run(self, text, path, workdir, reach):
        """Walk a step's run text, its directory the step's."""
        self.walk(scan.shell_script(text), path, join(REPO, workdir), [], [], {}, {}, reach, 0)

    def walk(self, commands, path, here, params, env, vars_, functions, reach, depth, body=False):
        """Walk commands in order. A function's body runs where it is called (body=True there), so a
        file's walk passes over it."""
        if depth > 8:
            return
        cwds, exported = {}, list(env)
        for name, lines in self.functions(commands).items():
            functions[name] = (path, lines)
        for command in commands:
            if command.function is not None and not body:
                continue
            cwd = here
            for size in range(len(command.frames), -1, -1):
                if command.frames[:size] in cwds:
                    cwd = cwds[command.frames[:size]]
                    break
            values, origin = [], []
            for index, word in enumerate(command.words):
                if [p[:2] for p in word.parts] == [("param", "@")]:
                    values += params
                    origin += [index] * len(params)
                else:
                    values.append(self.resolve(word, cwd, path, params, vars_))
                    origin.append(index)
            if not command.words:
                for name, value in command.assigns:
                    vars_[name] = self.resolve(value, cwd, path, params, vars_)
                continue
            k = scan.shell_command_word(command)
            k = origin.index(k) if k is not None and k in origin else None
            verb = values[k] if k is not None else None
            own = scan.shell_env_elements(command, len(command.words))
            if verb == "cd":
                target = [v for v in values[k + 1:] if v not in ("--", "-P", "-L")]
                cwds[command.frames] = join(cwd, target[0]) if target else None
            elif verb in ("local", "declare", "typeset", "readonly", "export"):
                for value in values[k + 1:]:
                    name, eq, rest = (value or "").partition("=")
                    if eq and re.fullmatch(r"[A-Za-z_]\w*", name):
                        vars_[name] = rest
                        if verb == "export" and name == "GOPROXY":
                            exported.append(("set", name, rest))
            elif verb == "unset" and "GOPROXY" in values[k + 1:]:
                exported.append(("set", "GOPROXY", None))
            elif verb == "shift":
                count = values[k + 1] if k + 1 < len(values) and (values[k + 1] or "").isdigit() else "1"
                params = params[int(count):]
            self.go_commands(command, values, origin, cwd, path, exported, reach)
            if verb is None:
                continue
            inherited = [*exported, *own]
            for inner, line in scan.shell_inner_scripts(command):
                self.walk(scan.shell_script(inner, line), path, cwd, params, inherited, vars_, functions, reach,
                          depth + 1)
            state = offline(inherited)
            if verb in functions:
                where, lines = functions[verb]
                args = values[k + 1:]
                self.remembered(("function", where, verb, cwd, tuple(args), state), reach, lambda into: self.walk(
                    lines, where, cwd, args, inherited, vars_, functions, into, depth + 1, True))
            elif verb in (".", "source") and k + 1 < len(values):
                for script in self.scripts(values[k + 1], command.words[origin[k + 1]], cwd):
                    self.walk(self.commands(script), script, cwd, params, exported, vars_, functions, reach,
                              depth + 1)
            elif verb in ("bash", "sh") or (verb or "").endswith(".sh"):
                at = k if verb.endswith(".sh") else next((j for j in range(k + 1, len(values))
                                                         if not (values[j] or "-").startswith("-")), None)
                if at is not None:
                    args = values[at + 1:]
                    for script in self.scripts(values[at], command.words[origin[at]], cwd):
                        self.remembered(("script", script, cwd, tuple(args), state), reach, lambda into, s=script: (
                            self.walk(self.commands(s), s, cwd, args, inherited, {}, {}, into, depth + 1)))
            elif any(values[k:k + len(r)] == r for r in RUNNERS):
                runner = next(r for r in RUNNERS if values[k:k + len(r)] == r)
                # Playwright loads its config, and the harness that config starts, whatever specs it
                # is given: its whole directory is what it reaches. bun test reaches what it names.
                paths = [] if "playwright" in runner else values[k + len(runner):]
                self.runner(paths, cwd, inherited, reach, depth)
            if any(v and v.endswith("go-fetch.sh") for v in values):
                self.fetch(values, cwd, reach)

    @staticmethod
    def functions(commands):
        bodies = {}
        for command in commands:
            if command.function:
                bodies.setdefault(command.function, []).append(command)
        return bodies

    def scripts(self, value, word, cwd):
        """The shell files a word names, read where it can be; else each $VAR in its text matches
        any name there."""
        if value is not None:
            whole = in_repo(join(cwd, value))
            return [whole] if whole and os.path.isfile(whole) else []
        text = word.source()
        found = []
        for base in (in_repo(cwd) or ".", "."):
            pattern = os.path.normpath(os.path.join(base, re.sub(r"\$\{?\w+\}?|\$\(\.\.\.\)", "*", text)))
            found += sorted(str(p) for p in Path(".").glob(pattern) if p.is_file() and p.suffix == ".sh")
        return sorted(set(found))

    def go_commands(self, command, values, origin, cwd, path, exported, reach):
        """The go commands of one simple command: a go test of testbuild's callers (rule 2's first
        half, offline or not, since testbuild builds offline inside it), and any offline go command
        that loads the modules of what it names."""
        texts = [v if v is not None else "\x00" for v in values]
        for j in range(len(texts)):
            word = command.words[origin[j]]
            if not (goish_text(word.value()) and go_subcommand(texts, j)):
                continue
            rest, module = texts[j + 1:], cwd
            if rest[:1] == ["-C"] and len(rest) > 1:
                module, rest = join(cwd, values[j + 2]), rest[2:]
            elif rest[0].startswith("-C="):
                module, rest = join(cwd, rest[0][3:]), rest[1:]
            sub, args = rest[0], rest[1:]
            if args[:1] == ["-C"] and len(args) > 1:
                module, args = join(cwd, args[1] if args[1] != "\x00" else None), args[2:]
            packages, skip = [], False
            for a in args:
                if skip:
                    skip = False
                elif a in FLAGS_WITH_VALUES:
                    skip = True
                elif a.startswith("./") or a == "." or ("@" in a and "\x00" not in a):
                    packages.append(a)
            if sub == "test" and not ("-list" in args or ("-c" in args and "/dev/null" in args)):
                listed = not packages or "xargs" in texts[:j]
                self.testbuild_tests(in_repo(module), packages, listed, reach)
            if sub in BUILDS and offline([*exported, *scan.shell_env_elements(command, origin[j])]):
                # A module this reading cannot place, or one outside the repository (a scratch copy),
                # is no module a fetch step names.
                if in_repo(module) is not None and "\x00" not in "".join(args):
                    reach.needs.append(Need(in_repo(module), packages or ["."], f"{path}:{word.line + 1}"))

    @staticmethod
    def testbuild_tests(module, packages, listed, reach):
        if module is None:
            return
        for user in users:
            if listed and module != "." and (user == module or user.startswith(module.rstrip("/") + "/")):
                reach.testbuild = True
            for arg in packages:
                pkg = os.path.normpath(os.path.join(module, arg))
                if pkg == user or (arg.endswith("/...") and user.startswith(pkg[:-3].rstrip("/") + "/")) \
                        or (arg == "./..." and user.startswith(module.rstrip("/") + "/")):
                    reach.testbuild = True

    def runner(self, args, cwd, env, reach, depth):
        """A bun or Playwright test run: each script a TypeScript file under its directory (or under
        the directories and test files it names; a config file is no such path) names in a string,
        walked from that script's own directory, and each goCommand of those files with its
        packages."""
        here = in_repo(cwd)
        if here is None:
            return
        roots = [os.path.normpath(os.path.join(here, v)) for v in args
                 if v and not v.startswith("-") and (os.path.isdir(os.path.join(here, v)) or (
                     os.path.isfile(os.path.join(here, v)) and TEST_PATH.search(v)))] or [here]
        inside = [p for p in set(ts_scripts) | set(ts_go_needs)
                  if any(r == "." or p == r or p.startswith(r.rstrip("/") + "/") for r in roots)]
        for ts in sorted(inside):
            reach.needs += ts_go_needs.get(ts, [])
            package = "/".join(ts.split("/")[:2])
            for name in sorted(ts_scripts.get(ts, ())):
                candidates = shell_files.get(name, [])
                near = [s for s in candidates if s.startswith(package + "/")]
                for script in near or (candidates if len(candidates) == 1 else []):
                    here_ = join(REPO, os.path.dirname(script))
                    self.remembered(("script", script, here_, (), offline(env)), reach, lambda into, s=script, h=here_: (
                        self.walk(self.commands(s), s, h, [], env, {}, {}, into, depth + 1)))

    @staticmethod
    def fetch(values, cwd, reach):
        for k, value in enumerate(values):
            if not (value and value.endswith("go-fetch.sh")) or k + 2 >= len(values):
                continue
            if values[k + 1] == "deps":
                module = in_repo(join(cwd, values[k + 2]))
                packages = []
                for v in values[k + 3:]:
                    if v is None or v.startswith(("-", "|", ";", "&")):
                        break
                    packages.append(v)
                reach.fetches.setdefault(module, set()).update(packages)
            elif values[k + 1] == "tools":
                listed = in_repo(join(cwd, values[k + 2]))
                if listed and os.path.isfile(listed):
                    reach.tools.update(l.split("#", 1)[0].strip() for l in scan.read(listed).splitlines()
                                       if l.split("#", 1)[0].strip())


def covered(need, fetched, tools):
    pins = {p for p in need.packages if "@" in p}
    packages = need.packages - pins
    if not all(any(pin == tool or pin.split("@")[0] == tool.split("@")[0] for tool in tools) for pin in pins):
        return False
    if not packages:
        return True
    if need.module is not None:
        return packages <= fetched.get(need.module, set())
    return any(packages <= done for done in fetched.values())


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
    if lang == "sh":
        shell_files.setdefault(os.path.basename(path), []).append(path)
    elif lang == "ts" and (SCRIPT_NAME.search(text) or "goCommand" in text):
        index_typescript(path, text)
    if not MAYBE[lang].search(text):
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

# --- 2. Each job's steps, walked in order: each offline build a step reaches is fetched before it.
walker = Walk()
jobs_checked = reach_checked = reach_reasoned = 0
if os.path.isfile(TESTBUILD) and not users:
    scan.unknown(f"{TESTBUILD} exists and no daemon test calls testbuild.EnvoyBinary with a command, so the jobs "
                 "that run one are unseen")


def step_reasoned(raw_lines, step, run):
    """A `go-offline: <why>` in the step's run, or in a YAML comment within the 5 lines above the line
    that names the step."""
    if REASON.search(run):
        return True
    name = step.get("name")
    if not name:
        return False
    named = re.compile(r"^\s*(?:-\s+)?name:\s*[\"']?" + re.escape(str(name)) + r"[\"']?\s*$")
    return any(named.match(line) and any(re.match(r"\s*#", l) and REASON.search(l)
                                         for l in raw_lines[max(0, k - scan.REASON_WINDOW):k])
               for k, line in enumerate(raw_lines))


for workflow in sorted(Path(".github/workflows").glob("*.y*ml")):
    workflow_text = workflow.read_text()
    document = yaml.safe_load(workflow_text) or {}
    default_dir = ((document.get("defaults") or {}).get("run") or {}).get("working-directory", ".")
    for job_id, job in (document.get("jobs") or {}).items():
        if not isinstance(job, dict):
            continue
        job_dir = ((job.get("defaults") or {}).get("run") or {}).get("working-directory", default_dir)
        fetched, tools, testbuild_fetched = {}, set(), False
        for step in job.get("steps") or []:
            run = step.get("run")
            if not isinstance(run, str):
                continue
            workdir = os.path.normpath(step.get("working-directory", job_dir))
            name = step.get("name") or run.strip().splitlines()[0]
            reach = Reach()
            # A step whose text names no go, no script, no bun and no Playwright reaches none.
            if re.search(r"go|\.sh\b|\bbun|playwright|\bsource\b|(?:^|[;&|(]\s*)\.\s", run, re.M):
                walker.run(run, str(workflow), workdir, reach)
            if reach.testbuild and users:
                jobs_checked += 1
                if not testbuild_fetched:
                    errors.append((str(workflow), 1,
                                   f"job {job_id}, step \"{name}\", runs the tests of {', '.join(sorted(users))}, "
                                   f"whose testbuild.EnvoyBinary builds {' '.join(sorted(built))} with GOPROXY=off, "
                                   f"and no step before it fetches them: add `.github/scripts/go-fetch.sh deps "
                                   f"packages/envoy {' '.join(sorted(built))}` before it"))
            needs = sorted(set(reach.needs), key=lambda n: n.where)
            if needs:
                reach_checked += 1
                missing = [n for n in needs if not covered(n, fetched, tools)]
                if missing and step_reasoned(workflow_text.split("\n"), step, run):
                    reach_reasoned += 1
                elif missing:
                    errors.append((str(workflow), 1, f"job {job_id}, step \"{name}\", reaches "
                                   + "; ".join(f"{n.where}, an offline go command of {n.module or 'its module'}: "
                                               f"{' '.join(sorted(n.packages))}" for n in missing)
                                   + ", and no step before it fetches that: add `.github/scripts/go-fetch.sh deps "
                                   + "; ".join(f"{n.module or 'MODULE'} {' '.join(sorted(n.packages))}" for n in missing)
                                   + "` (or `go-fetch.sh tools` of a pin) before it, or say in a `# go-offline: <why>` "
                                     "line of its run why it builds none of it"))
            for module, packages in reach.fetches.items():
                fetched.setdefault(module, set()).update(packages)
            tools |= reach.tools
            if "packages/envoy" in reach.fetches and built <= reach.fetches["packages/envoy"]:
                testbuild_fetched = True
# A tree where tests call testbuild and no step of any workflow runs them is one whose steps this
# reading cannot follow (a go test, cd or script it does not see), not a clean one.
if os.path.isfile(TESTBUILD) and jobs_checked == 0:
    scan.unknown(f"{', '.join(sorted(users))} call testbuild.EnvoyBinary and no step of any workflow was seen to run "
                 "their tests, so the jobs that run one are unseen")

# --- 3. In every job of a required workflow, each step that runs go-fetch.sh comes after a step of
# the same job that restores ~/go/pkg/mod, so the fetch tops up the job's module cache instead of
# downloading it whole.
def runs_fetch(run):
    for command in scan.shell_script(run):
        values = command.values()
        if any(v.endswith("go-fetch.sh") and values[k + 1:k + 2] in (["deps"], ["tools"])
               for k, v in enumerate(values)):
            return True
    return False


pairs = []
for workflow_path in sorted(workflows):
    document = yaml.safe_load(Path(workflow_path).read_text()) or {}
    for job_id, job in (document.get("jobs") or {}).items():
        if not isinstance(job, dict):
            continue
        restore = None
        for index, step in enumerate(job.get("steps") or []):
            uses = str(step.get("uses") or "")
            if uses.startswith(("actions/cache/restore@", "actions/cache@")) \
                    and "~/go/pkg/mod" in str((step.get("with") or {}).get("path") or "").split():
                restore = step.get("name") or step.get("id") or f"step {index + 1}, {uses.split('@')[0]}"
            run = step.get("run")
            if not (isinstance(run, str) and "go-fetch.sh" in run and runs_fetch(run)):
                continue
            name = step.get("name") or run.strip().splitlines()[0]
            if restore is None:
                errors.append((workflow_path, 1, f"job {job_id}, step \"{name}\", runs go-fetch.sh with no step "
                               "before it that restores ~/go/pkg/mod (actions/cache/restore or actions/cache): "
                               "restore the job's module cache first, so the fetch adds to it instead of "
                               "downloading every module"))
            else:
                pairs.append(f"  {workflow_path} job {job_id}: \"{name}\" after \"{restore}\"")

total = sum(counts.values())
summary = (f"{total} go commands of {sum(read.values())} files read (Go {counts['go']}, TypeScript {counts['ts']}, "
           f"Python {counts['py']}, shell {counts['sh']})")
print("Each fetch step of a required job, after the restore of the module cache before it:")
print("\n".join(pairs) if pairs else "  (none)")
if errors:
    scan.refuse(errors, summary, "A test fetches no module while it runs. Run the go command with GOPROXY=off and "
                "fetch what it needs in a step before the tests (.github/scripts/go-fetch.sh deps DIR PACKAGE...), "
                "after the job restores its module cache, or, where it reads no module or runs in no test of a "
                "required job, say so beside it in a `go-offline: <why>` comment.")
print(f"every one of the {summary} runs with GOPROXY=off ({offline_count}), carries a reason ({reason_count}) or "
      f"is in a file whose reason covers it ({file_reason_count}); each of the {jobs_checked} steps that run "
      f"testbuild's tests comes after its fetch, and each of the {reach_checked} steps that reach an offline build "
      f"comes after a fetch of what it builds ({reach_reasoned} say why they need none); each of the {len(pairs)} "
      f"fetch steps of a required job comes after a restore of ~/go/pkg/mod")
PY
