#!/usr/bin/env bash
# Tests for `source_scan.py` and the modules beside it (source_statements.py, source_env.py,
# source_python.py, source_shell.py), the reading of source files check-go-offline.sh and
# check-omp-offline.sh share: the language of a path (.mts and .cts are TypeScript, a .d.ts is no
# source), the walk that skips a test's inputs, installed packages and links and, when asked, reads
# a file by its shebang, the lexing of Go, TypeScript and Python into code with comments blanked
# and string text blanked but for the literals the caller keeps, a Go or TypeScript call's
# arguments, assignments, imports and environments (with what later statements set in a
# TypeScript env object), Python's through its ast module (a module's name read in a function),
# shell as simple commands (a ${NAME:-word}'s word, which commands are forked, $" inside double
# quotes as text, the script a shell reads from its pipeline), an argv's program after its
# launchers, and where a reason comment counts.
# Last, one row both checks read through it: a .mts test that holds a quoted example of a start
# and, in its control, the start itself; each check names the start and neither reads the example.
#
# Only what the modules do. What each check makes of them stays in that check's own suite,
# check-go-offline.test.sh and check-omp-offline.test.sh, each of which runs the modules beside the
# real script rather than a copy.
#
# The modules are symlinked into a temporary directory and imported the way the checks import them
# (PYTHONPATH, no bytecode), so the code under test is the files CI runs. It needs a python3 with
# PyYAML, which the go check imports.
#
# Run from anywhere: .github/scripts/source_scan.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

mkdir -p "$work/module"
for module in source_scan source_statements source_env source_python source_shell; do
  ln -s "$script_dir/$module.py" "$work/module/$module.py"
done

# py <statements> [argument ...]: runs the statements with the modules as `scan`, `statements`,
# `env`, `python_ast` and `shell`, and the arguments in sys.argv[1:], and echoes what they print.
py() {
  local statements=$1
  shift
  PYTHONDONTWRITEBYTECODE=1 PYTHONPATH="$work/module" python3 -c "import sys
import source_env as env
import source_python as python_ast
import source_scan as scan
import source_shell as shell
import source_statements as statements
$statements" "$@"
}

# code <lang> <kept literal>... : the code of this harness's stdin, with the named literals kept,
# echoed line for line.
code() {
  local lang=$1
  shift
  py 'print("\n".join(scan.code(sys.stdin.read(), sys.argv[1], set(sys.argv[2:]).__contains__)), end="")' "$lang" "$@"
}

echo "case: the language of a path"
for row in "a.go go" "a_test.go go" "a.ts ts" "a.tsx ts" "a.mts ts" "a.cts ts" "a.js ts" "a.mjs ts" "a.cjs ts" \
  "a.jsx ts" "a.d.ts none" "a.d.mts none" "a.py py" "a.sh sh" "a.bash sh" "a.yaml none" "a.goo none"; do
  set -- $row
  check "$1 is $2" "$(is "$(py 'print(scan.language(sys.argv[1]) or "none")' "$1")" "$2")"
done

echo "case: a test file by its name"
for row in "a_test.go true" "a.test.ts true" "a.spec.mts true" "a.test.sh true" "a_test.py true" "x/test_a.py true" \
  "a.go false" "a.ts false" "testing.py false" "a.sh false"; do
  set -- $row
  check "$1 is $2" "$(is "$(py 'print(str(scan.is_test_file(sys.argv[1])).lower())' "$1")" "$2")"
done

echo "case: the walk reads source in order, and skips testdata, node_modules, dist, .git, .jj and links"
tree="$work/tree"
mkdir -p "$tree/b" "$tree/a" "$tree/testdata" "$tree/node_modules/x" "$tree/dist" "$tree/.git" "$tree/.jj" "$tree/c/testdata"
for f in a/one.go b/two.ts a/three.py c/four.sh testdata/no.go node_modules/x/no.ts dist/no.js .git/no.sh .jj/no.py \
  c/testdata/no.go a/notes.md; do
  echo x >"$tree/$f"
done
ln -s "$tree/a/one.go" "$tree/b/link.go"
check "lists each source file once, with its language" \
  "$(is "$(py 'print(" ".join(f"{p}:{l}" for p, l in scan.sources(sys.argv[1])))' "$tree")" \
    "a/one.go:go a/three.py:py b/two.ts:ts c/four.sh:sh")"

echo "case: Go: comments and string text blank, a kept literal stays, lines and columns stay"
out=$(code go go <<'EOF'
// exec.Command("go", "build") in a comment
cmd := exec.Command("go", "build", `raw go`) /* a
block */ + "x"
EOF
)
check "the comment is blank" "$(is "$(sed -n 1p <<<"$out" | tr -d ' ')" "")"
check "the kept literal stays and the others are blanked, quotes kept" \
  "$(is "$(sed -n 2p <<<"$out" | sed 's/ *$//')" 'cmd := exec.Command("go", "     ", `      `)')"
check "a block comment over two lines is blank on both" "$(is "$(sed -n 3p <<<"$out")" '         + " "')"

echo "case: TypeScript: a template's text blanks, its \${...} stays code, and a regular expression hides no quote"
out=$(code ts omp <<'EOF'
const QUOTED = /["$`\\]/; const s = 'omp';
const t = `run ${ f("omp", `in ${x}`) } now`;
const u = a / b / c; Bun.spawn([omp]);
EOF
)
check "the regular expression is blank and the string after it is read" \
  "$(is "$(sed -n 1p <<<"$out")" "const QUOTED =          ; const s = 'omp';")"
check "the template's text blanks, its substitutions stay, a kept literal inside them stays" \
  "$(is "$(sed -n 2p <<<"$out")" 'const t = `    ${ f("omp", `   ${x}`) }    `;')"
check "a division is no regular expression" "$(is "$(sed -n 3p <<<"$out")" 'const u = a / b / c; Bun.spawn([omp]);')"
out=$(printf '%s\n' 'const t = `go list ${pkg}`;' | code ts 'go list ${}')
check "a template is kept by its text with each substitution as \${}" \
  "$(is "$out" 'const t = `go list ${pkg}`;')"

echo "case: Python: prefixes, triple quotes and an escaped newline"
out=$(code py go <<'EOF'
run(["go", f"list {x}"], r'\d') # a "comment"
doc = """go
build"""; s = 'a\
b'; t = "go"
EOF
)
check "the kept literal stays, a prefixed one blanks" \
  "$(is "$(sed -n 1p <<<"$out" | sed 's/ *$//')" 'run(["go", f"        "], r'"'"'  '"'"')')"
check "a triple-quoted string blanks over its lines" "$(is "$(sed -n 2p <<<"$out")" 'doc = """  ')"
check "and its last line" "$(is "$(sed -n 3p <<<"$out")" '     """; s = '"'"'  ')"
check "an escaped newline continues the string" "$(is "$(sed -n 4p <<<"$out")" ' '"'"'; t = "go"')"

echo "case: shell: a quoted command is one word, a \$(...) in double quotes runs, a comment and a heredoc's body run nothing"
out=$(py 'print([c.values() for c in shell.shell_script(sys.stdin.read())])' <<'EOF'
echo "go build $(go version)" # go list
cat <<'TEXT'
go build
TEXT
"$root/bin/go" build ./...
EOF
)
check "each command's words, in order, an expansion as \\x00" \
  "$(is "$out" "[['echo', 'go build \\x00'], ['go', 'version'], ['cat'], ['\\x00/bin/go', 'build', './...']]")"

# src_of <lang> <statements>: runs the statements with this harness's stdin read as `src`, a
# statements.Source of that language.
src_of() {
  py "src = statements.Source(sys.stdin.read(), sys.argv[1])
$2" "$1"
}

echo "case: Go: a call's arguments across lines, its comments left out"
out=$(src_of go 'c = next(c for c in src.calls() if c.callee == "exec.Command")
print(src.line(c.at) + 1, len(c.args), [src.string(*a) for a in c.args])' <<'EOF'
package x
cmd := exec.Command(
	"go",
	"list", // the subcommand
)
EOF
)
check "one call on its first line, two arguments" "$(is "$out" "2 2 ['go', 'list']")"

echo "case: TypeScript: a template tagged by \$ or Bun.\$ is a call"
out=$(src_of ts 'print(" ".join(c.callee for c in src.calls() if c.tagged))' <<'EOF'
await $`go build`;
await Bun.$`ls`;
EOF
)
check "both tags" "$(is "$out" '$ Bun.$')"

echo "case: Go: the Env of a Cmd is that variable's own, read after it"
out=$(src_of go 'calls = [c for c in src.calls() if c.callee == "exec.Command"]
print(env.go_cmd_env(src, calls[0]), env.go_cmd_env(src, calls[1]))' <<'EOF'
package x

func f() {
	list := exec.Command("go", "list")
	_ = list.Run()
	build := exec.Command("go", "build")
	build.Env = append(os.Environ(), "A=b", "GOPROXY=off")
}
EOF
)
check "none for the first, the inherited environment then each setting for the second" \
  "$(is "$out" "None [('inherit',), ('set', 'A', 'b'), ('set', 'GOPROXY', 'off')]")"
out=$(src_of go 'print(env.go_cmd_env(src, next(c for c in src.calls() if c.callee == "exec.Command")))' <<'EOF'
package x

func f() {
	env := append([]string{"GOPROXY=off"}, os.Environ()...)
	cmd := exec.Command("go", "build")
	cmd.Env = env
	cmd.Env = append(cmd.Env, "X=1")
}
EOF
)
check "through a name it is bound to, and an append to its own Env" \
  "$(is "$out" "[('set', 'GOPROXY', 'off'), ('inherit',), ('set', 'X', '1')]")"
out=$(src_of go 'print([env.go_cmd_env(src, c) for c in src.calls() if c.callee == "exec.Command"])' <<'EOF'
package x

func f() {
	cmd := exec.Command("go", "list")
	_ = cmd.Run()
	cmd = exec.Command("go", "build")
	cmd.Env = append(os.Environ(), "GOPROXY=off")
}
EOF
)
check "an Env set after the variable is assigned again is the new command's alone" \
  "$(is "$out" "[None, [('inherit',), ('set', 'GOPROXY', 'off')]]")"

echo "case: TypeScript: a call's own options' env, given in place or by a name"
out=$(src_of ts 'print([env.ts_options_env(src, c, 1) for c in src.calls() if c.callee == "Bun.spawn"])' <<'EOF'
Bun.spawn(["go"], { cwd: x, env: { ...process.env, GOPROXY: "off" } });
const env = { GOPROXY: "off" };
Bun.spawn(["go"], { env });
Bun.spawn(["go"]);
EOF
)
check "each spawn's own" \
  "$(is "$out" "[[('unknown',), ('set', 'GOPROXY', 'off')], [('set', 'GOPROXY', 'off')], None]")"

echo "case: a name is bound to the value of its nearest assignment, a statement ending at its line"
out=$(src_of ts 'print(src.text_of_tokens(*src.bound("go", len(src.tokens))), src.text_of_tokens(*src.bound("x", len(src.tokens))))' <<'EOF'
const go = Bun.which("go");
const x: string = "y"
run(x)
EOF
)
check "TypeScript, a type annotation passed over" "$(is "$out" 'Bun.which("go") "y"')"
out=$(src_of go 'print(src.text_of_tokens(*src.bound("goBin", len(src.tokens) - 1)))' <<'EOF'
package x

func f() {
	goBin, err := exec.LookPath("go")
	goBin = "go"
	_ = run(goBin, err)
}
EOF
)
check "Go, the nearest of two" "$(is "$out" '"go"')"

echo "case: Python: an env= built by dict(), a ** spread and assignments to its keys"
out=$(py 'import ast
tree = python_ast.python_tree(sys.stdin.read())
call = [n for n in ast.walk(tree) if isinstance(n, ast.Call) and getattr(n.func, "id", "") == "run"][0]
print(python_ast.python_env_elements(call.keywords[0].value), python_ast.python_env_elements(call.keywords[1].value))' <<'EOF'
def f():
    env = dict(os.environ, GOPROXY="off")
    env["X"] = "1"
    run(env=env, other={**os.environ, "GOPROXY": "off"})
EOF
)
check "in the order they are made" \
  "$(is "$out" "[('unknown',), ('set', 'GOPROXY', 'off'), ('set', 'X', '1')] [('unknown',), ('set', 'GOPROXY', 'off')]")"
check "an f-string's text, each {...} as a placeholder" \
  "$(is "$(py 'import ast; print(repr(python_ast.python_text(ast.parse(sys.argv[1]).body[0].value)))' 'f"go list {pkg}"')" "'go list \\x00'")"

echo "case: shell: simple commands, nested ones, continued lines joined, and no heredoc body"
out=$(py 'for c in shell.shell_script(sys.stdin.read()):
    print(":".join([str(c.line + 1), c.function or "-", str(c.pipe), " ".join(w.source() for w in c.words),
                    " ".join(n for n, _ in c.assigns)]))' <<'EOF'
x=$(cd a && GOPROXY=off go build ./... \
  -o out)
f() {
  go list | xargs go vet
}
cat <<TEXT
go build
TEXT
case $1 in a) go test ;; esac
for w in go build; do [[ $w == go ]]; done
EOF
)
check "each command once, in order, with its function, its pipeline index and its prefix" \
  "$(is "$out" "$(printf '%s\n' '1:-:0::x' '1:-:0:cd a:' '1:-:0:go build ./... -o out:GOPROXY' '4:f:0:go list:' '4:f:1:xargs go vet:' '6:-:0:cat:' '9:-:0:go test:')")"

echo "case: shell: a command's own settings, and the shell it hands more shell to"
out=$(py 'c = shell.shell_script(sys.argv[1])[0]
print(shell.shell_env_elements(c, c.values().index("go")))' 'GOPROXY=off env -u A B=c go build')
check "its prefix, then env's -u and NAME=value before the go" \
  "$(is "$out" "[('set', 'GOPROXY', 'off'), ('set', 'A', None), ('set', 'B', 'c')]")"
out=$(py 'print([s for c in shell.shell_script(sys.stdin.read()) for s in shell.shell_inner_scripts(c)])' <<'EOF'
bash -euo pipefail -c 'go vet'
eval go build
sh <<SCRIPT
go mod download
SCRIPT
cat <<TEXT
go list
TEXT
EOF
)
check "a -c script, what eval runs, and a heredoc a shell reads, each with its line" \
  "$(is "$out" "[('go vet', 0), ('go build', 1), ('go mod download', 3)]")"

echo "case: shell: \$\" and \$' inside double quotes are text, and a quote after them ends the string"
out=$(py 'print([w.source() for c in shell.shell_script(sys.stdin.read()) for w in c.words])' <<'EOF'
say "ends in a dollar$" 'and then'
say "a $'b' c"
EOF
)
check "each word ends where bash ends it" \
  "$(is "$out" "['say', 'ends in a dollar\$', 'and then', 'say', \"a \$'b' c\"]")"

echo "case: shell: a \${NAME:-word}'s word, and which commands are forked"
out=$(py 'w = shell.shell_script(sys.argv[1])[0].words[0]
print(w.defaults)' '"${GO:-go}" list')
check "the default of each expansion with one, by the part it is" "$(is "$out" "{0: 'go'}")"
out=$(py 'print([(c.values()[0], c.forked) for c in shell.shell_script(sys.stdin.read())])' <<'EOF'
a | b
c &
d && e
f
EOF
)
check "each command of a pipeline and each one in the background is forked, an && list's are not" \
  "$(is "$out" "[('a', True), ('b', True), ('c', True), ('d', False), ('e', False), ('f', False)]")"

echo "case: shell: the script a shell reads from cat, echo or printf before it in a pipeline"
out=$(py 'cs = shell.shell_script(sys.stdin.read())
print([s for k in range(len(cs)) for s in shell.shell_stdin_scripts(cs, k)])' <<'EOF'
cat <<'SCRIPT' | bash
go vet
SCRIPT
echo "go build" | sh
printf 'go list\n' | bash -e
echo "go test" | grep go
cat notes.txt | bash
EOF
)
check "the heredoc, the echo's words and the printf's format, each with its line; no other feeder or reader" \
  "$(is "$out" "[('go vet', 1), ('go build', 3), ('go list\\n', 4)]")"

echo "case: the program of an argv after launchers, and the settings each env there makes"
out=$(py 'v = sys.argv[1].split()
k = shell.command_word(v)
print(k, shell.env_settings(v, k))' 'env A=1 timeout 60 nice -n 5 command exec env -u B go build')
check "past env, timeout and its duration, nice and its -n, command and exec" "$(is "$out" "12 [('set', 'A', '1'), ('set', 'B', None)]")"
out=$(py 'print(shell.command_word(["timeout", None, "go"]), shell.command_word([None, "go"]))')
check "an unknown word is a launcher's operand where it takes one, else the program" "$(is "$out" "2 0")"

echo "case: Go and TypeScript imports, by the name each binds"
out=$(py 'print(statements.Source(sys.stdin.read(), "go").go_imports())' <<'EOF'
import (
	osexec "os/exec"
	. "fmt"
	_ "embed"
	"path/filepath"
)
import single "strings"
EOF
)
check "an alias, a dot import, no blank one, a path's last element, and a single import" \
  "$(is "$out" "[('osexec', 'os/exec'), ('.', 'fmt'), ('filepath', 'path/filepath'), ('single', 'strings')]")"
out=$(py 'print(sorted(statements.Source(sys.stdin.read(), "ts").ts_imports().items()))' <<'EOF'
import d, { a as b, type T, c } from "m";
import * as ns from "n";
const cp = require("node:child_process");
const { exec: sh, spawn } = require("x");
EOF
)
check "named, aliased, default, namespace and required names" \
  "$(is "$out" "[('T', ('m', 'T')), ('b', ('m', 'a')), ('c', ('m', 'c')), ('cp', ('node:child_process', '*')), ('d', ('m', 'default')), ('ns', ('n', '*')), ('sh', ('x', 'exec')), ('spawn', ('x', 'spawn'))]")"
out=$(py 's = statements.Source(sys.stdin.read(), "ts")
print([c.callee for c in s.tagged_calls({"sh"})])' <<'EOF'
await sh`go build`; await x.sh`no`; await other`no`;
EOF
)
check "a template tagged by a given name, and no method of that name" "$(is "$out" "['sh']")"

echo "case: TypeScript: an env object with what later statements set in it"
out=$(py 's = statements.Source(sys.stdin.read(), "ts")
call = [c for c in s.calls() if c.callee == "run"][0]
print(env.ts_options_env(s, call, 0))' <<'EOF'
const e = { GOPROXY: "off" };
e.GOFLAGS = "-mod=mod";
e["GOPROXY"] = "direct";
delete e.GOFLAGS;
Object.assign(e, { A: "1" });
run({ env: e, ...(q ? {} : { stdin: "ignore" }) });
EOF
)
check "each set, in order, and an env-free spread after it changes nothing" \
  "$(is "$out" "[('set', 'GOPROXY', 'off'), ('set', 'GOFLAGS', '-mod=mod'), ('set', 'GOPROXY', 'direct'), ('set', 'GOFLAGS', None), ('set', 'A', '1')]")"

echo "case: Python: a name a function does not bind is read from the module"
out=$(py 'import ast
tree = python_ast.python_tree(sys.stdin.read())
uses = [n for n in ast.walk(tree) if isinstance(n, ast.Name) and n.id == "GO" and isinstance(n.ctx, ast.Load)]
print([ast.unparse(python_ast.python_bound("GO", u)) if python_ast.python_bound("GO", u) else None for u in uses])' <<'EOF'
GO = "go"
def other():
    GO = "local"
def reads():
    return GO
def takes(GO):
    return GO
EOF
)
check "the module's, not another function's, and not over a parameter" "$(is "$out" "[\"'go'\", None]")"

echo "case: the walk reads a file named with no source extension as shell when asked to and its first line runs a shell"
mkdir -p "$work/bang"
printf '#!/usr/bin/env bash\n' >"$work/bang/run"
printf '#!/bin/sh -e\n' >"$work/bang/x.test"
printf '#!/usr/bin/env python3\n' >"$work/bang/tool"
printf 'notes\n' >"$work/bang/README"
check "by its shebang, with shebang" \
  "$(is "$(py 'print(" ".join(f"{p}:{l}" for p, l in scan.sources(sys.argv[1], shebang=True)))' "$work/bang")" "run:sh x.test:sh")"
check "and by its name alone without" "$(is "$(py 'print(len(list(scan.sources(sys.argv[1]))))' "$work/bang")" "0")"

echo "case: a reason counts on the line, or on a comment line within five above"
reasoned() { py 'import re
raw = sys.stdin.read().split("\n")
print(str(scan.reasoned(raw, sys.argv[1], re.compile(r"\bwhy:\s*\S"), [int(sys.argv[2])])).lower())' "$@"; }
check "on the line itself" "$(is "$(printf 'x\nrun() // why: here\n' | reasoned go 1)" true)"
check "five lines above" "$(is "$(printf '// why: above\n1\n2\n3\n4\nrun()\n' | reasoned go 5)" true)"
check "six lines above is too far" "$(is "$(printf '// why: above\n1\n2\n3\n4\n5\nrun()\n' | reasoned go 6)" false)"
check "a line above that is no comment does not count" "$(is "$(printf 'x := "why: here"\nrun()\n' | reasoned go 1)" false)"

# The token index of the nth token of a value on a 1-based line, and a resolve result as text; for
# the rows below, which src_of runs.
RESOLVING='def at(line, value, nth=0):
    return [i for i, t in enumerate(src.tokens) if src.line(i) + 1 == line and t.value == value][nth]
def show(found):
    if found is None:
        return "none"
    if found[0] == "bound":
        return "bound " + " | ".join(src.text_of_tokens(*r) for r in found[1])
    return f"param {found[1][0].name} {found[1][1]}"
'

echo "case: Go: each func, method and literal, a literal by the name it is bound to, and its parameters"
out=$(src_of go 'for f in src.functions():
    print(f.name, src.line(f.at) + 1, ",".join(src.tokens[p[0]].value for p in f.params if p) or "-",
          src.tokens[f.body[0]].value, src.line(f.body[1]) + 1)' <<'EOF'
package x

func f(t *testing.T, a, b string) (string, error) {
	read := func(env []string) bool {
		return len(env) > 0
	}
	return "", nil
}

var hook func(int) int

func (r *rig) binary() string { return r.omp }
EOF
)
check "the declaration, the literal by its name, the method; a func type is none" \
  "$(is "$out" "$(printf '%s\n' 'f 3 t,a,b read 8' 'read 4 env return 6' 'binary 12 - return 12')")"

echo "case: TypeScript: a function, an arrow by its name, a method and an object key's arrow, past a return type"
out=$(src_of ts 'for f in src.functions():
    print(f.name, src.line(f.at) + 1, ",".join(src.tokens[p[0]].value for p in f.params if p) or "-",
          src.tokens[f.body[0]].value)' <<'EOF'
export async function spawnRpc(binary: string, ...rest: string[]): Promise<{ ok: boolean }> {
  return run(binary);
}
const pinned = (): string => process.env.LEGION_TEST_OMP ?? "omp";
const runner = {
  start(env: Record<string, string>) {
    return env;
  },
  stop: async (code: number) => {
    return code;
  },
};
EOF
)
check "each, with its parameters and its body's first token" \
  "$(is "$out" "$(printf '%s\n' 'spawnRpc 1 binary,rest return' 'pinned 4 - process' 'start 6 env return' 'stop 9 code return')")"

echo "case: Go: a name's value scope by scope, a parameter, the package's var, and each call of a literal"
out=$(src_of go "$RESOLVING"'print(show(src.resolve("env", at(8, "env"))))
print(show(src.resolve("env", at(11, "env"))))
print(show(src.resolve("env", at(6, "env", 1))))
print(show(src.resolve("level", at(11, "level"))))
print(show(src.resolve("other", at(11, "use"))))
run = next(f for f in src.functions() if f.name == "run")
print([src.line(c.at) + 1 for c in src.call_sites(run)], src.function_at(at(8, "use")).name)' <<'EOF'
package x

var level = "package"

func f(env []string) {
	env = append(env, "A=1")
	run := func(env []string) {
		use(env)
	}
	run(env)
	use(env, level)
	other := 1
	use(other)
}
EOF
)
check "the literal's parameter, f's own assignment, f's parameter inside that assignment, the var, nothing yet" \
  "$(is "$out" "$(printf '%s\n' 'param run 0' 'bound append(env, "A=1")' 'param f 0' 'bound "package"' 'none' '[10] run')")"

echo "case: Go: the Env assignments of the Cmd a call makes, until its variable is assigned again"
out=$(src_of go 'for c in [c for c in src.calls() if c.callee == "exec.Command"]:
    target, sets = env.go_cmd_envs(src, c)
    print(target, [src.text_of_tokens(*r) for r in sets])' <<'EOF'
package x

func f() {
	cmd := exec.Command("omp")
	cmd.Env = base
	cmd.Env = append(cmd.Env, "X=1")
	cmd = exec.Command("other")
	cmd.Env = later
	exec.Command("third").Run()
}
EOF
)
check "each set of the first, the one of the second, none of an unassigned call" \
  "$(is "$out" "$(printf '%s\n' "cmd ['base', 'append(cmd.Env, \"X=1\")']" "cmd ['later']" "None []")")"

echo "case: TypeScript: a spawn's argv and where its options are, a call's chain, and a two-character operator"
out=$(src_of ts "$RESOLVING"'for c in src.calls():
    if c.name == "spawn":
        argv, options = src.ts_spawn_argv(c)
        print(" ".join(src.text_of_tokens(*r) for r in argv), options)
    if c.tagged:
        print(src.text_of_tokens(c.at, src.chain_end(c)))
a, b = at(6, "a"), at(6, ";")
print([src.text_of_tokens(*r) for r in src.split_op(a, b, "??")], [src.text_of_tokens(*r) for r in src.split_op(a, b, "||")])' \
  <<'EOF'
const argv = [omp, "models"];
Bun.spawn(argv, { env });
Bun.spawn({ cmd: ["git", "status"], cwd });
spawn("omp", ["models"], { env });
await $`omp models`.env({ A: "b" }).quiet();
const value = a ?? b || c;
EOF
)
check "an argv by its name, cmd:, node's command and args; the chain; each operator's parts" \
  "$(is "$out" "$(printf '%s\n' 'omp "models" 1' '"git" "status" 0' '"omp" "models" 2' '$`omp models`.env({ A: "b" }).quiet()' "['a', 'b || c'] ['a ?? b', 'c']")")"

# --- One row both checks read through these modules ------------------------------------------------

# both <name>: a tree holding both checks, a ci.yaml whose one job sets up Go and runs with
# GOPROXY=off and GOFLAGS=-mod=readonly (a Go job the go check's rule 4 reads), a file of each
# language the go check reads and a Go test file the Oh My Pi check reads. Each check imports the
# modules from the directory its link resolves to, this one. Echoes its root.
both() {
  local root="$work/$1"
  mkdir -p "$root/.github/scripts" "$root/.github/workflows" "$root/packages/app" "$root/packages/web" "$root/tools"
  for linked in check-go-offline.sh check-omp-offline.sh; do
    ln -s "$script_dir/$linked" "$root/.github/scripts/$linked"
  done
  printf 'name: CI\non: pull_request\nenv:\n  GOPROXY: "off"\n  GOFLAGS: -mod=readonly\njobs:\n  lint:\n    runs-on: ubuntu-24.04\n    steps:\n      - uses: actions/setup-go@v5\n      - run: echo lint\n' \
    >"$root/.github/workflows/ci.yaml"
  printf 'package app\n\nfunc App() {}\n' >"$root/packages/app/app.go"
  printf 'package app\n\nimport "testing"\n\nfunc TestApp(t *testing.T) {}\n' >"$root/packages/app/app_test.go"
  printf 'print("app")\n' >"$root/tools/app.py"
  printf '#!/usr/bin/env bash\necho app\n' >"$root/tools/app.sh"
  echo "$root"
}

run_both() { # run_both <root>: each check's output and status
  set +e
  go_out=$("$1/.github/scripts/check-go-offline.sh" 2>&1)
  go_status=$?
  omp_out=$("$1/.github/scripts/check-omp-offline.sh" 2>&1)
  omp_status=$?
  set -e
}

echo "case: a .mts test that only quotes a start: both checks read the file, and neither reads the example"
root=$(both quoted)
cat >"$root/packages/web/probe.test.mts" <<'EOF'
const example = 'Bun.spawn(["go", "list", "./..."])';
const other = 'Bun.spawn([binary, "models", "list"])';
EOF
run_both "$root"
check "the go check passes" "$(is "$go_status" 0)"
check "over no go command, the .mts file read" "$(contains "$go_out" 'every one of the 0 go commands of 5 files read')"
check "the Oh My Pi check passes" "$(is "$omp_status" 0)"
check "over no start, the .mts file read" "$(contains "$omp_out" '0 starts of Oh My Pi in 2 test files read')"

echo "case: the same .mts test with each start itself: both checks name it"
root=$(both started)
cat >"$root/packages/web/probe.test.mts" <<'EOF'
const example = 'Bun.spawn(["go", "list", "./..."])';
const child = Bun.spawn(["go", "list", "./..."]);
const other = Bun.spawn([binary, "models", "list"]);
EOF
run_both "$root"
check "the go check fails" "$(is "$go_status" 1)"
check "naming the start, not the example" \
  "$(contains "$go_out" '^::error file=packages/web/probe.test.mts,line=2::')"
check "and nothing else" "$(contains "$go_out" '1 problems over the 1 go commands')"
check "the Oh My Pi check fails" "$(is "$omp_status" 1)"
check "naming its start" "$(contains "$omp_out" '^::error file=packages/web/probe.test.mts,line=3::')"

summary "source_scan.py and the modules beside it"
