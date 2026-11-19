#!/usr/bin/env bash
# Every test that starts the real Oh My Pi runs it under its language's offline helper, or says
# why it need not.
#
# Under a fresh HOME the pinned Oh My Pi fetches model catalogs over HTTPS, on `omp models` and at
# every session start (catalog.stencil.so, and the public model lists of the providers that need
# no key): one `omp models list` made 52 connections off the machine and 7 DNS lookups. No setting
# of the pin stops all of it. A test that runs it so reaches the network while it runs, and those
# hosts' faults become its own. The rule that ends that is one helper per language, which starts a
# listener of the test's own on loopback that refuses every request and names it as HTTPS_PROXY,
# the proxy Bun's fetch reads: testbin.OMPOffline in Go (packages/daemon/internal/testbin, which
# testbin.OMPEnviron includes), and ompOffline in TypeScript
# (packages/pi-shared/test/omp-offline.ts, which the harness's ompEnv includes). A list of the
# tests that follow it would go stale the day a new one is written, so this program asks every
# place a test starts Oh My Pi for its answer instead.
#
# It reads test code: Go `_test.go` files and the files of a package directory named testbin, and
# TypeScript and JavaScript `.test.` and `.spec.` files and the files under a directory named test,
# tests or __tests__. Files under testdata, node_modules, dist, .git or .jj are not read. Each file
# is read a statement at a time, as .github/scripts/source_scan.py reads it for check-go-offline.sh
# too: a call's arguments across lines, and the value a name is bound to in its own function, a
# function around it, or the file.
#
# The real binary is a value, whatever the name it is held under:
#   - LEGION_TEST_OMP read from the environment (process.env.LEGION_TEST_OMP, Bun.env, an env
#     object's [KEY] where KEY is the name or a constant naming it; os.Getenv and os.LookupEnv in
#     Go), and so LEGION_OMP_PATH and OMP_BIN; any call given one of those names as an argument,
#     as testbin.OMP's own binary(t, "LEGION_TEST_OMP", ...) is;
#   - testbin.OMP(t); exec.LookPath("omp"), Bun.which("omp") and which("omp"); filepath.Join,
#     path.join or path.resolve whose last part is "omp"; "omp" or a literal path ending /omp, in a
#     string, a template or a + of strings;
#   - a call of a function whose return value is one of these, in any file, test code or not: in
#     Go a function or method of the call's own package or of the package whose name qualifies
#     it, in TypeScript one of the file itself or of the module it imports the name from;
#   - a name bound to one of these (`a ?? b` and `a || b` hold it when either part does), a
#     parameter that a call of its function in the same file is given one of these, and, as the
#     tests name it, a name whose last part starts with omp (omp, r.omp, ompPath) and in
#     TypeScript a plain `binary`.
# A start is a place a test runs that value:
#   Go          exec.Command and exec.CommandContext whose program is the binary, or `env ...` or a
#               shell's -c whose program is; each passes when its own *exec.Cmd's environment (the
#               last `X.Env = ...` of the variable it is assigned to) holds a call of
#               testbin.OMPOffline or testbin.OMPEnviron (inside testbin, OMPOffline or OMPEnviron).
#               And three doors, where the binary comes into a test that may hand it to code this
#               does not read: testbin.OMP, os.Getenv or os.LookupEnv of LEGION_TEST_OMP (by its name
#               or a constant of the package naming it), and exec.LookPath("omp"); each passes when
#               the top-level function that holds it, its closures included, calls one of those.
#   TypeScript  Bun.spawn, Bun.spawnSync (an argv, or `cmd:`), spawn, spawnSync, execFile,
#               execFileSync, execFileAsync, execa and execaSync whose program is the binary (or
#               `env ...` or a shell's -c whose program is); exec, execSync and execAsync of a shell
#               string; and Bun's $`...`. A shell string, a template's included, is read as shell: a
#               substitution that holds the binary reads as omp, and a command of it whose program is
#               omp (after env, command, exec, nice or timeout, in a $(...), after && or ;, or in a
#               shell's -c) is a start. Each passes when its own environment holds a call of
#               ompOffline or ompEnv: what the call is given after its program (its options, their
#               env), or a call chained to it (Bun's $`...`.env({...})); and it is no Sync form: the
#               helper's listener answers only while the test's event loop runs, which a Sync call
#               holds.
# "Holds" follows the value: a call of the helper in the expression, or a name in it whose nearest
# assignment before the start holds one (each in its own scope, as the binary's), a parameter every
# call of its function in the file is given such a value for, or a function of the file it calls
# whose body does. The program is never read for it, since a function that returns the binary may
# call the helper for something else; nor is a helper called in the next statement, or anywhere
# else in the function.
# A start of either language also passes with a reason: a comment holding `omp-offline: <why>` on
# its line or within the 5 lines above it, saying why the run reaches no network or why the
# program is not Oh My Pi, for a reader to judge. Comments and the text of string literals are not
# read as code, so a fixture or a message that spells a start starts nothing here.
#
# Its limits, each a form this reads as no start, or as holding the helper:
#   - a start in a file that is no test code (a helper of a package that a test calls): it is not
#     read, so a test that reaches the binary only through one is seen in Go when it holds a door,
#     and in TypeScript not at all;
#   - the binary handed in from another file's call (a parameter not named as above), held in a
#     struct field, a map or an array not named as above, read by destructuring
#     (`const { LEGION_TEST_OMP: bin } = process.env`), or returned by a function that holds none
#     of the forms above in its own file;
#   - a program that runs the binary as an argument of its own (tmux, xargs, a launcher), a start
#     inside a template's ${...}, an *exec.Cmd made as a struct literal, a start or a helper called
#     through another name (`const run = Bun.spawn`, testbin imported under an alias), and a call of
#     a TypeScript function no import names (a global, a method of an object);
#   - a value set again on one branch: the nearest assignment before the start decides, and the
#     helper anywhere in that value holds, so whether the command's environment keeps what it
#     returns is the reviewer's to judge, as check-go-offline.sh says of its own.
# The proxy catches what Oh My Pi asks for over HTTPS through Bun's fetch, which is every request
# off the machine the pin was seen to make; a plain-HTTP request to another machine, or one made
# outside Bun's fetch, would pass it, and only a trace of the run's connects shows one.
#
# The helpers are held to the rule too: testbin's OMPOffline and the TypeScript ompOffline each
# name HTTPS_PROXY, and OMPEnviron and ompEnv, where they exist, each call theirs. A helper that
# stopped naming the proxy fails here instead of passing every start that calls it. A scan that
# read no Go or no TypeScript test file, or found no start where testbin or the harness exists,
# reports UNKNOWN and exits 2: a scan that cannot see must not read as a clean one.
#
# Run from anywhere: .github/scripts/check-omp-offline.sh
# CI runs it in ci.yaml's lint job.
set -euo pipefail

# The shared reading of source files (source_scan.py) sits beside this script's own file, which a
# test may link into a tree of its own: PYTHONPATH names that directory, and the scan reads the
# tree this script stands in. PYTHONDONTWRITEBYTECODE keeps the import from leaving a __pycache__.
scripts=$(cd "$(dirname "$(readlink -f "$0")")" && pwd)
cd "$(dirname "$0")/../.."

PYTHONDONTWRITEBYTECODE=1 PYTHONPATH="$scripts" python3 <<'PY'
import bisect
import os
import re

import source_scan as scan

DOOR = "LEGION_TEST_OMP"
REASON = re.compile(r"\bomp-offline:\s*\S")
# A variable of the environment that names the binary: the door, LEGION_OMP_PATH, OMP_BIN.
OMP_VARIABLE = re.compile(r"(?:\w+_)?OMP(?:_(?:PATH|BIN|BINARY))?")
# A name the binary is held under, by its last part: omp, r.omp, ompPath.
OMP_NAME = re.compile(r"[oO][mM][pP][\w$]*")
GO_CONST = re.compile(r"\b(\w+)\s*(?:string\s*)?=\s*\"" + DOOR + r"\"")
TS_CONST = re.compile(r"\b(?:const|let|var)\s+(\w+)\s*(?::\s*\w+\s*)?=\s*[\"'`]" + DOOR + r"[\"'`]")
SPAWNS = {"spawn", "spawnSync", "execFile", "execFileSync", "execFileAsync", "execa", "execaSync"}
SHELL_CALLS = {"exec", "execSync", "execAsync"}
GO_HELPERS = {"testbin.OMPOffline", "testbin.OMPEnviron"}
TS_HELPERS = {"ompOffline", "ompEnv"}
UNREAD = "\x00"  # a word of an argv or a shell string this cannot read
DEPTH = 6
NOT_NAMES = {"nil", "true", "false", "null", "undefined", "this", "append", "len", "make", "new", "string"}
# Which test file is read as statements: one that can hold a door or a helper, or a call that
# starts a program this cannot name from its text alone. A file whose every such call names its
# program as a string that is neither the binary, a shell nor a program that runs another (env,
# timeout, ...) starts nothing, and is not lexed: most test files start only git or go.
WRAPPERS = scan.SHELLS | {"env", "command", "exec", "nice", "nohup", "time", "timeout", "flock", "builtin"}
GO_EXEC_CALL = re.compile(r"\bexec\.Command(Context)?\(")
GO_CONTEXT_ARGUMENT = re.compile(r"[^,()]*(?:\([^()]*\))?\s*,")
GO_PROGRAM = re.compile(r"\s*\"([^\"\\\n]*)\"")
TS_SPAWN_CALL = re.compile(r"(?<![\w$])(?:spawn|spawnSync|execFile|execFileSync|execFileAsync|execa|execaSync)\s*\(")
TS_PROGRAM = re.compile(r"\s*(?:\[\s*|\{\s*cmd\s*:\s*\[\s*)?([\"'])([^\"'\\\n]*)\1")
TS_SHELL_CALL = re.compile(r"(?:(?<![\w$.])|(?<=\bchild_process\.)|(?<=\bchildProcess\.)|(?<=\bcp\.))"
                           r"(?:exec|execSync|execAsync)\s*\(\s*(?:([\"'])([^\"'\\\n]*)\1|`([^`$\\]*)`)?")
TS_DOLLAR = re.compile(r"\$`([^`$\\]*)(`)?")


def named_program(program):
    """Whether a program a call names as a string is neither the binary, one that runs another, nor
    a command line a shell reads (spawn("omp models", { shell: true }))."""
    return not (omp_text(program) or program.rsplit("/", 1)[-1] in WRAPPERS or re.search(r"\s", program))


def may_start(path, text, lang):
    """Whether a test file can hold a start, a door or a helper, by its text alone."""
    if lang == "go":
        if any(s in text for s in ("testbin.OMP(", DOOR, 'LookPath("omp")')) or any(
                name in text for name in door_consts.get(os.path.dirname(path), ())):
            return True
        if "exec.Command" not in text:
            return False
        for m in GO_EXEC_CALL.finditer(text):
            at = m.end()
            if m.group(1):
                context = GO_CONTEXT_ARGUMENT.match(text, at)
                if not context:
                    return True
                at = context.end()
            program = GO_PROGRAM.match(text, at)
            if not program or not named_program(program.group(1)):
                return True
        return False
    if "ompOffline" in text or "ompEnv" in text:
        return True
    if not any(s in text for s in ("spawn", "exec", "$`")):
        return False
    for m in TS_SPAWN_CALL.finditer(text):
        program = TS_PROGRAM.match(text, m.end())
        if not program or not named_program(program.group(2)):
            return True
    for m in TS_SHELL_CALL.finditer(text):
        shell = m.group(2) if m.group(1) else m.group(3)
        if shell is None or "omp" in shell:
            return True
    return any(m.group(2) is None or "omp" in m.group(1) for m in TS_DOLLAR.finditer(text))


def defines(lang, name):
    """A pattern of a file's text that defines a function by that name."""
    n = re.escape(name)
    if lang == "go":
        return re.compile(r"(?m)^func\s+(?:\([^)]*\)\s*)?" + n + r"\s*[\[(]")
    return re.compile(r"\bfunction\s*\*?\s*" + n + r"\b|\b" + n + r"\s*[:=]\s*(?:async\s*)?(?:function\b|\(|[\w$]+\s*=>)"
                      r"|(?m:^\s*(?:(?:async|static|public|private|protected)\s+)*" + n + r"\s*\()")


def test_code(path, lang):
    """A test file by its name, or a file of a Go testbin package or of a TypeScript test, tests or
    __tests__ directory."""
    directories = path.split("/")[:-1]
    if lang == "go":
        return scan.is_test_file(path) or "testbin" in directories
    return scan.is_test_file(path) or any(d in ("test", "tests", "__tests__") for d in directories)


def omp_text(text):
    """Whether a program's name, as text, is the binary: omp, or a path ending /omp."""
    return text == "omp" or bool(re.fullmatch(r"[^\s'\"]*/omp", text))


def shell_starts(text, line, program, depth=0):
    """The line of each command of a shell text whose program is the binary, in a $(...) and in a
    shell's -c, eval or heredoc too. program(word) says whether a word is the binary."""
    out = []
    for command in scan.shell_script(text, line):
        k = scan.shell_command_word(command)
        if k is not None and program(command.values()[k]):
            out.append(command.line)
        if depth < 3:
            for inner, inner_line in scan.shell_inner_scripts(command):
                out += shell_starts(inner, inner_line, program, depth + 1)
    return out


def top_level(src, a, b):
    """The token indices of [a, b) outside every bracket inside it."""
    k = a
    while k < b:
        yield k
        if src.tokens[k].kind == "punct" and src.tokens[k].value in ("(", "[", "{") and k in src.partner:
            k = src.partner[k]
        k += 1


class Reading:
    """One file read for the binary and the helper: what holds which, each answer kept."""

    def __init__(self, path, src):
        self.path, self.src, self.lang = path, src, src.lang
        self.directory = os.path.dirname(path)
        self.in_testbin = os.path.basename(self.directory) == "testbin"
        if self.lang == "go":
            self.helpers = GO_HELPERS | ({"OMPOffline", "OMPEnviron"} if self.in_testbin else set())
        else:
            self.helpers = TS_HELPERS
        self.helper_calls = sorted(call.at for call in src.calls() if call.callee in self.helpers)
        self.consts = door_consts.get(self.directory, set()) if self.lang == "go" else ts_door_consts
        self._omp, self._holds = {}, {}

    # --- The binary.

    def variable(self, s, a, b, at):
        """Whether a range names a variable of the environment that holds the binary: the name as a
        string, a name the file binds to it, or one bound to it elsewhere (a constant of the
        package, or one a TypeScript file exports)."""
        text = s.string(a, b)
        if text is not None:
            return bool(OMP_VARIABLE.fullmatch(text))
        name = s.name(a, b)
        if not name or "." in name:
            return False
        found = self.src.resolve(name, at)
        if found and found[0] == "bound":
            return any(OMP_VARIABLE.fullmatch(self.src.string(*r) or "") for r in found[1])
        return name in self.consts

    def unwrap(self, s, a, b):
        """[a, b) without a TypeScript `!`, `as T`, `await` or parentheses around its value."""
        while b - a > 1:
            if s.punct(b - 1, "!"):
                b -= 1
            elif s.punct(a, "(") and s.partner.get(a) == b - 1:
                a, b = a + 1, b - 1
            elif s.tokens[a].kind == "ident" and s.tokens[a].value == "await":
                a += 1
            else:
                cut = next((k for k in top_level(s, a, b) if k > a and s.tokens[k].kind == "ident"
                            and s.tokens[k].value in ("as", "satisfies")), None)
                if cut is None:
                    break
                b = cut
        return a, b

    def named(self, name):
        last = name.rsplit(".", 1)[-1]
        return bool(OMP_NAME.fullmatch(last)) or (self.lang == "ts" and name == "binary")

    def omp(self, a, b, s=None, at=None, depth=0):
        """Whether the value of [a, b) is the real binary: of the file, or of s, a template
        substitution of it read at the file's token `at`."""
        own = s is None
        s = self.src if own else s
        key = (a, b)
        if own:
            if key in self._omp:
                return self._omp[key]
            self._omp[key] = False
        found = self._omp_value(s, a, b, a if own else at, depth)
        if own:
            self._omp[key] = found
        return found

    def _omp_value(self, s, a, b, at, depth):
        if b <= a or depth > DEPTH:
            return False
        sub = None if s is self.src else s
        if self.lang == "ts":
            a, b = self.unwrap(s, a, b)
            for op in ("??", "||"):
                parts = s.split_op(a, b, op)
                if len(parts) > 1:
                    return any([self.omp(pa, pb, sub, at if sub else None, depth) for pa, pb in parts])
        text = s.string(a, b)
        if text is None and b - a == 1 and s.tokens[a].kind == "template":
            text = s.template_text(s.tokens[a], lambda tokens: UNREAD)
        if text is not None:
            return omp_text(text)
        parts = s.split(a, b, "+")
        if len(parts) > 1:
            last = s.string(*parts[-1])
            return last is not None and last.endswith("/omp")
        call = s.call_of(a, b)
        if call:
            return self._omp_call(s, call, at, depth)
        name = s.name(a, b)
        if name:
            parts = name.split(".")
            if len(parts) > 1 and parts[-2] == "env" and OMP_VARIABLE.fullmatch(parts[-1]):
                return True  # process.env.LEGION_TEST_OMP, Bun.env.LEGION_TEST_OMP
            if len(parts) == 1:
                found = self.src.resolve(name, at)
                if found and found[0] == "bound" and any([self.omp(*r, depth=depth + 1) for r in found[1]]):
                    return True
                if found and found[0] == "param":
                    fn, index = found[1]
                    if any([self.omp(*c.args[index], depth=depth + 1) for c in self.src.call_sites(fn)
                            if index < len(c.args)]):
                        return True
            return self.named(name)
        if s.punct(b - 1, "]") and (b - 1) in s.partner:  # process.env[KEY]
            open_ = s.partner[b - 1]
            base = s.name(a, open_)
            return bool(base and base.rsplit(".", 1)[-1] == "env" and self.variable(s, open_ + 1, b - 1, at))
        return False

    def _omp_call(self, s, call, at, depth):
        args = call.args
        last = s.string(*args[-1]) if args else None
        if self.lang == "go":
            if call.callee == "testbin.OMP" or (self.in_testbin and call.callee == "OMP"):
                return True
            if call.callee == "exec.LookPath" and len(args) == 1 and s.string(*args[0]) == "omp":
                return True
            if call.name == "Join" and last is not None and omp_text(last):
                return True
        else:
            if call.name in ("which", "whichSync") and args and s.string(*args[0]) == "omp":
                return True
            if call.name in ("join", "resolve") and last is not None and omp_text(last):
                return True
        if returns_binary(self, call):
            return True
        return any([self.variable(s, ra, rb, at) for ra, rb in args])

    def returns(self, name):
        """Whether a function of the file by that name returns the binary: a return value of its own
        body, or an arrow's expression."""
        src = self.src
        for fn in src.functions():
            if fn.name != name:
                continue
            a, b = fn.body
            if not src.punct(a - 1, "{"):
                values = [fn.body]
            else:
                values = [src.split(k + 1, src.expression_end(k + 1))[:1] for k in range(a, b)
                          if src.tokens[k].kind == "ident" and src.tokens[k].value == "return" and src.function_at(k) is fn]
                values = [v[0] for v in values if v]
            if any([self.omp(va, vb) for va, vb in values]):
                return True
        return False

    # --- The helper.

    def plain_name(self, s, k):
        """Whether token k of s is a name whose value can hold the helper: no field, key or keyword."""
        t = s.tokens[k]
        if t.kind != "ident" or t.value in NOT_NAMES or t.value in scan._KEYWORDS[self.lang]:
            return False
        if s.punct(k - 1, ".") or s.punct(k + 1, "."):
            return False
        return not (s.punct(k + 1, ":") and (s.punct(k - 1, "{") or s.punct(k - 1, ",")))

    def calls_helper(self, a, b):
        """Whether [a, b) of the file holds a call of the helper itself."""
        i = bisect.bisect_left(self.helper_calls, a)
        return i < len(self.helper_calls) and self.helper_calls[i] < b

    def holds(self, a, b, known=None, depth=0):
        """Whether [a, b) of the file holds a call of the helper: one there, a template's ${...}
        there that calls it, or a name there whose value holds one. known maps a dotted name (a
        Cmd's own Env) to whether what it stands for holds it."""
        key = (a, b, tuple(sorted((known or {}).items())))
        if key in self._holds:
            return self._holds[key]
        self._holds[key] = False
        found = self._holds_value(a, b, known or {}, depth)
        self._holds[key] = found
        return found

    def _holds_value(self, a, b, known, depth):
        src = self.src
        if b <= a or depth > DEPTH:
            return False
        if self.calls_helper(a, b):
            return True
        for k in range(a, b):
            t = src.tokens[k]
            if t.kind == "template":
                for sub in t.subs:
                    inner = scan.Source(src.text, self.lang, sub)
                    if any(call.callee in self.helpers for call in inner.calls()):
                        return True
                    if any(self.plain_name(inner, j) and self.name_holds(tok.value, k, inner.punct(j + 1, "("), depth)
                           for j, tok in enumerate(inner.tokens)):
                        return True
                continue
            if t.kind != "ident":
                continue
            if k + 2 < b and src.punct(k + 1, ".") and f"{t.value}.{src.tokens[k + 2].value}" in known \
                    and not src.punct(k - 1, "."):
                if known[f"{t.value}.{src.tokens[k + 2].value}"]:
                    return True
                continue
            if self.plain_name(src, k) and self.name_holds(t.value, k, src.punct(k + 1, "("), depth):
                return True
        return False

    def name_holds(self, name, at, called, depth):
        """Whether the value a name used at the file's token `at` stands for holds the helper: its
        nearest assignment, every call's argument for a parameter, or for a call a function of the
        file by that name."""
        src = self.src
        found = src.resolve(name, at)
        if found and found[0] == "bound":
            return self.holds(*found[1][-1], depth=depth + 1)
        if found and found[0] == "param":
            fn, index = found[1]
            sites = [c.args[index] for c in src.call_sites(fn) if index < len(c.args)]
            return bool(sites) and all([self.holds(*r, depth=depth + 1) for r in sites])
        if called:
            bodies = [fn.body for fn in src.functions() if fn.name == name]
            return bool(bodies) and all([self.holds(*body, depth=depth + 1) for body in bodies])
        return False

    # --- Starts. A part of a shell text or an argv this does not read as text (a ${...}, a name, a
    # call) stands in it as one placeholder character, which the table evaluates only when it is a
    # command's program, so a part elsewhere in the command costs nothing.

    def placeholder(self, table, evaluate):
        mark = chr(0xE000 + len(table))
        table[mark] = evaluate
        return mark

    def template(self, index, table):
        """A template's text with each ${...} as a placeholder of the table."""
        def substitute(tokens):
            inner = scan.Source(self.src.text, self.lang, tokens)
            return self.placeholder(table, lambda: bool(inner.tokens) and self.omp(0, len(inner.tokens), inner, index))
        return self.src.template_text(self.src.tokens[index], substitute)

    def starts_in(self, text, line, table):
        """The lines of the commands of a shell text that run the binary."""
        return shell_starts(text, line, lambda word: omp_text(word) or (word in table and table[word]()))

    def shell_text(self, a, b, table, depth=0):
        """The shell text of a string expression: a string, a template, a + of those, or a name
        bound to one; None when it is none of these."""
        src = self.src
        if b - a == 1 and src.tokens[a].kind == "string":
            return src.tokens[a].value
        if b - a == 1 and src.tokens[a].kind == "template":
            return self.template(a, table)
        parts = src.split(a, b, "+")
        if len(parts) > 1:
            texts = [self.shell_text(*p, table, depth) for p in parts]
            return "".join(t if t is not None else self.placeholder(table, lambda p=p: self.omp(*p))
                           for t, p in zip(texts, parts))
        name = src.name(a, b)
        if name and "." not in name and depth < 4:
            found = src.resolve(name, a)
            if found and found[0] == "bound":
                return self.shell_text(*found[1][-1], table, depth + 1)
        return None

    def argv_starts(self, argv):
        """Whether an argv runs the binary, read as a shell command of one word per element."""
        src, table, words = self.src, {}, []
        if len(argv) == 1:
            text = src.string(*argv[0])
            if text is not None and re.search(r"\s", text):  # spawn("omp models list", { shell: true })
                return bool(self.starts_in(text, 0, table))
        for a, b in argv:
            text = self.shell_text(a, b, table)  # "HOME=" + home is a setting env reads as one
            if text is None:
                words.append(self.placeholder(table, lambda a=a, b=b: self.omp(a, b)))
            else:
                words.append("'" + text.replace("'", "'\\''") + "'")
        return bool(self.starts_in(" ".join(words), 0, table))


errors = []
starts = {"go": 0, "ts": 0}
helped = reasons = 0
testbin_dirs, harnesses = set(), set()
go_helper_defs, ts_helper_defs = {}, {}
door_consts, ts_door_consts = {}, set()

# Read every Go and TypeScript file's text once: the test code to scan, the constants that name the
# door, and the files a function that returns the binary may be defined in.
go_files, ts_files, texts, go_by_dir, ts_paths = [], [], {}, {}, []
for path, lang in scan.sources():
    if lang not in ("go", "ts"):
        continue
    text = texts[path] = scan.read(path)
    if lang == "go":
        go_by_dir.setdefault(os.path.dirname(path), []).append(path)
    else:
        ts_paths.append(path)
    if DOOR in text:
        names = (GO_CONST if lang == "go" else TS_CONST).findall("\n".join(scan.code(text, lang, {DOOR}.__contains__)))
        if lang == "go":
            door_consts.setdefault(os.path.dirname(path), set()).update(names)
        else:
            ts_door_consts.update(names)
    if test_code(path, lang):
        (go_files if lang == "go" else ts_files).append(path)

readings, returned, imports = {}, {}, {}
IMPORT = re.compile(r"\bimport\s+(?:type\s+)?(?:([\w$]+)\s*,?\s*)?(?:\{([^}]*)\}|\*\s+as\s+([\w$]+))?\s*from\s*[\"']([^\"']+)[\"']")


def reading_of(path, lang):
    if path not in readings:
        readings[path] = Reading(path, scan.Source(texts[path], lang))
    return readings[path]


def imported(path, callee):
    """The module a TypeScript file imports a callee from and its name there, else None: a named or
    default import of the name, or a namespace import that qualifies it."""
    if path not in imports:
        names = imports[path] = {}
        for m in IMPORT.finditer(texts[path]):
            default, named, namespace, spec = m.groups()
            if default:
                names[default] = (spec, "default")
            if namespace:
                names[namespace + ".*"] = (spec, None)
            for item in (named or "").split(","):
                original, _, local = item.strip().removeprefix("type ").partition(" as ")
                if original.strip():
                    names[local.strip() or original.strip()] = (spec, original.strip())
    qualifier, _, name = callee.rpartition(".")
    if qualifier:
        found = imports[path].get(qualifier + ".*")
        return (found[0], name) if found else None
    return imports[path].get(name)


def module_stem(path_or_spec):
    return re.sub(r"\.[cm]?[jt]sx?$", "", path_or_spec.rstrip("/").rsplit("/", 1)[-1])


ts_modules = {}


def modules(spec):
    """The TypeScript files an import of spec can name: by the file's name, or an index file's directory."""
    if not ts_modules:
        for path in ts_paths:
            stem = module_stem(path)
            ts_modules.setdefault(stem, []).append(path)
            if stem == "index":
                ts_modules.setdefault(os.path.basename(os.path.dirname(path)), []).append(path)
    return ts_modules.get(module_stem(spec), [])


def returns_binary(reading, call):
    """Whether the function a call calls returns the binary: in Go one of the call's own package (a
    method's too) or of the package that qualifies it; in TypeScript the file's own, else the module
    it is imported from. A call of anything else (a method of an object, a global) is no binary."""
    lang, name = reading.lang, call.name
    if not re.fullmatch(r"[\w$]+", name):
        return False
    if lang == "go":
        qualifier = call.callee.rpartition(".")[0]
        dirs = [reading.directory] + sorted(d for d in go_by_dir if qualifier and d != reading.directory
                                            and os.path.basename(d) == qualifier)
        paths = [p for d in dirs for p in go_by_dir.get(d, ())]
    elif "." not in call.callee and any(fn.name == name for fn in reading.src.functions()):
        paths = [reading.path]
    else:
        source = imported(reading.path, call.callee)
        if source is None:
            return False
        name = source[1] if source[1] != "default" else name
        paths = modules(source[0])
    key = (lang, name, tuple(paths))
    if key not in returned:
        returned[key] = False
        pattern = defines(lang, name)
        returned[key] = any(reading_of(p, lang).returns(name) for p in paths
                            if name in texts[p] and pattern.search(texts[p]))
    return returned[key]


def door_rule(reading, index, what):
    """A door passes when the top-level function that holds it calls the helper."""
    global helped, reasons
    src, path = reading.src, reading.path
    low, high = src.region(index)
    line = src.line(index)
    raw = texts[path].split("\n")
    starts["go"] += 1
    if reading.calls_helper(low, high):
        helped += 1
    elif scan.reasoned(raw, "go", REASON, [line]):
        reasons += 1
    else:
        errors.append((path, line + 1, f"a test {what} in a function that calls neither testbin.OMPOffline nor "
                                       f"testbin.OMPEnviron, and gives no omp-offline reason: {raw[line].strip()}"))


def go_file(path):
    global helped, reasons
    text = texts[path]
    directory = os.path.dirname(path)
    in_testbin = os.path.basename(directory) == "testbin"
    if in_testbin:
        testbin_dirs.add(directory)
    elif not may_start(path, text, "go"):
        return
    reading = reading_of(path, "go")
    src = reading.src
    raw = text.split("\n")
    if in_testbin:
        for fn in src.functions():
            if fn.name in ("OMPOffline", "OMPEnviron") and src.function_at(fn.at) is None:
                go_helper_defs[fn.name] = (path, src.line(fn.at), src.text[src.tokens[fn.at].start:src.tokens[fn.body[1]].end],
                                           [call.callee for call in src.calls() if fn.body[0] <= call.at < fn.body[1]])
    doors = set()
    for call in src.calls():
        if call.callee == "testbin.OMP" and not in_testbin:
            doors.add((call.at, "takes the real Oh My Pi (testbin.OMP)"))
        elif call.callee in ("os.Getenv", "os.LookupEnv") and len(call.args) == 1 and (
                src.string(*call.args[0]) == DOOR or src.name(*call.args[0]) in reading.consts):
            doors.add((call.at, f"reads {DOOR}"))
        elif call.callee == "exec.LookPath" and len(call.args) == 1 and src.string(*call.args[0]) == "omp":
            doors.add((call.at, "resolves the real Oh My Pi (exec.LookPath)"))
    for index, what in sorted(doors):
        door_rule(reading, index, what)
    for call in src.calls():
        first = {"exec.Command": 0, "exec.CommandContext": 1}.get(call.callee)
        if first is None or len(call.args) <= first or not reading.argv_starts(call.args[first:]):
            continue
        starts["go"] += 1
        line = src.line(call.at)
        target, sets = src.go_cmd_envs(call)
        held = False
        for a, b in sets:
            held = reading.holds(a, b, {f"{target}.Env": held})
        if held:
            helped += 1
        elif scan.reasoned(raw, "go", REASON, [line]):
            reasons += 1
        else:
            errors.append((path, line + 1, "a test starts Oh My Pi with an environment (its own Cmd's Env) that holds "
                                           "neither testbin.OMPOffline nor testbin.OMPEnviron, and gives no "
                                           f"omp-offline reason: {raw[line].strip()}"))


def ts_file(path):
    global helped, reasons
    text = texts[path]
    if os.path.basename(path) == "omp-harness.ts":
        harnesses.add(path)
    if not may_start(path, text, "ts"):
        return
    reading = reading_of(path, "ts")
    src = reading.src
    raw = text.split("\n")
    for fn in src.functions():
        if fn.name in ("ompOffline", "ompEnv"):
            end = fn.body[1] if src.punct(fn.body[0] - 1, "{") else fn.body[1] - 1
            ts_helper_defs[fn.name] = (path, src.line(fn.at), src.text[src.tokens[fn.at].start:src.tokens[end].end],
                                       [call.callee for call in src.calls() if fn.body[0] <= call.at < fn.body[1]])
    for call in src.calls():
        # Where its environment can be: what follows its program (options, an env, a callback), and
        # what is chained to it (Bun's $`...`.env({...})); never the program, whose own value may
        # come from a function that calls the helper for something else.
        command, table, environment = None, {}, [(call.close + 1, src.chain_end(call))]
        if call.tagged:
            text_lines = reading.starts_in(reading.template(call.open, table), src.line(call.open), table)
            command = text_lines[0] if text_lines else None
        elif call.name in SPAWNS and call.args:
            argv, options = src.ts_spawn_argv(call)
            if argv and reading.argv_starts(argv):
                command = src.line(argv[0][0])
            if options == 0:  # Bun.spawn({ cmd: [...], env })
                a, b = call.args[0]
                environment += [p for p in src.split(a + 1, b - 1) if src.tokens[p[0]].value != "cmd"]
            environment += call.args[max(options, 1):]
        elif call.name in SHELL_CALLS and call.args and call.callee in (
                call.name, f"child_process.{call.name}", f"childProcess.{call.name}", f"cp.{call.name}"):
            shell = reading.shell_text(*call.args[0], table)
            text_lines = reading.starts_in(shell, src.line(call.args[0][0]), table) if shell is not None else []
            command = text_lines[0] if text_lines else None
            environment += call.args[1:]
        if command is None:
            continue
        starts["ts"] += 1
        line, sync = src.line(call.at), call.name.endswith("Sync")
        offline = any([reading.holds(a, b) for a, b in environment])
        if offline and not sync:
            helped += 1
        elif scan.reasoned(raw, "ts", REASON, [line, command]):
            reasons += 1
        elif offline:
            errors.append((path, line + 1, "a Sync start of Oh My Pi under ompOffline's proxy: the proxy's listener "
                                           "answers only while the event loop runs, which the Sync call holds; "
                                           "start it with Bun.spawn: " + raw[line].strip()))
        else:
            errors.append((path, line + 1, "a start of the real Oh My Pi whose environment calls neither ompOffline nor "
                                           "ompEnv, and gives no omp-offline reason: " + raw[line].strip()))


for path in go_files:
    go_file(path)
for path in ts_files:
    ts_file(path)

blind = [name for name, files in (("Go", go_files), ("TypeScript", ts_files)) if not files]
if blind:
    scan.unknown(f"read no {' and no '.join(blind)} test file under {os.getcwd()}, so their starts of Oh My Pi are "
                 "unseen")
if testbin_dirs and starts["go"] == 0:
    scan.unknown(f"{', '.join(sorted(testbin_dirs))} exists and no Go test starts Oh My Pi, so the reading that "
                 "finds the starts sees none")
if harnesses and starts["ts"] == 0:
    scan.unknown(f"{', '.join(sorted(harnesses))} exists and no TypeScript test starts Oh My Pi, so the reading "
                 "that finds the starts sees none")

# The helpers themselves: each names the proxy, and each environment helper calls its own.
for kind, present, defs, offline, environ in (
        ("Go", testbin_dirs, go_helper_defs, "OMPOffline", "OMPEnviron"),
        ("TypeScript", harnesses, ts_helper_defs, "ompOffline", "ompEnv")):
    if not present:
        continue
    if offline not in defs or "HTTPS_PROXY" not in defs[offline][2]:
        where = defs[offline][0] if offline in defs else sorted(present)[0]
        errors.append((where, defs[offline][1] + 1 if offline in defs else 1,
                       f"no {kind} {offline} that names HTTPS_PROXY, the helper every start of Oh My Pi is held to"))
    if environ in defs and offline not in defs[environ][3]:
        errors.append((defs[environ][0], defs[environ][1] + 1, f"{environ} does not call {offline}"))

total = starts["go"] + starts["ts"]
summary = f"{total} starts of Oh My Pi in {len(go_files) + len(ts_files)} test files read (Go {starts['go']}, TypeScript {starts['ts']})"
if errors:
    scan.refuse(errors, summary, "A test's Oh My Pi reaches no network. Run it under testbin.OMPEnviron's "
                "environment or with testbin.OMPOffline's entry in its own Cmd's Env (Go), or with ompOffline's "
                "from @legion/pi-shared/test/omp-offline in its own call's env (TypeScript), or, where the run "
                "reaches nothing or the program is not Oh My Pi, say so beside it in an `omp-offline: <why>` "
                "comment.")
print(f"every one of the {summary} runs under its language's offline helper ({helped}) or carries a reason ({reasons})")
PY
