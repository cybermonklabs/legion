#!/usr/bin/env bash
# Tests for `source_scan.py`, the reading of source files check-go-offline.sh and
# check-omp-offline.sh share: the language of a path (.mts and .cts are TypeScript, a .d.ts is no
# source), the walk that skips a test's inputs, installed packages and links, the lexing of Go,
# TypeScript and Python into code with comments blanked and string text blanked but for the
# literals the caller keeps, a Go or TypeScript call's arguments, assignments and environments,
# Python's through its ast module, shell as simple commands, and where a reason comment counts.
# Last, one row both checks read through it: a .mts test that holds a quoted example of a start
# and, in its control, the start itself; each check names the start and neither reads the example.
#
# Only what the module does. What each check makes of it stays in that check's own suite,
# check-go-offline.test.sh and check-omp-offline.test.sh, each of which runs the module beside the
# real script rather than a copy.
#
# The module is symlinked into a temporary directory and imported the way the checks import it
# (PYTHONPATH, no bytecode), so the code under test is the file CI runs. It needs a python3 with
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
ln -s "$script_dir/source_scan.py" "$work/module/source_scan.py"

# py <statements> [argument ...]: runs the statements with the module as `scan` and the arguments in
# sys.argv[1:], and echoes what they print.
py() {
  local statements=$1
  shift
  PYTHONDONTWRITEBYTECODE=1 PYTHONPATH="$work/module" python3 -c "import sys
import source_scan as scan
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
out=$(py 'print([c.values() for c in scan.shell_script(sys.stdin.read())])' <<'EOF'
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
# scan.Source of that language.
src_of() {
  py "src = scan.Source(sys.stdin.read(), sys.argv[1])
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
print(src.go_cmd_env(calls[0]), src.go_cmd_env(calls[1]))' <<'EOF'
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
out=$(src_of go 'print(src.go_cmd_env(next(c for c in src.calls() if c.callee == "exec.Command")))' <<'EOF'
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
out=$(src_of go 'print([src.go_cmd_env(c) for c in src.calls() if c.callee == "exec.Command"])' <<'EOF'
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
out=$(src_of ts 'print([src.ts_options_env(c, 1) for c in src.calls() if c.callee == "Bun.spawn"])' <<'EOF'
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
tree = scan.python_tree(sys.stdin.read())
call = [n for n in ast.walk(tree) if isinstance(n, ast.Call) and getattr(n.func, "id", "") == "run"][0]
print(scan.python_env_elements(call.keywords[0].value), scan.python_env_elements(call.keywords[1].value))' <<'EOF'
def f():
    env = dict(os.environ, GOPROXY="off")
    env["X"] = "1"
    run(env=env, other={**os.environ, "GOPROXY": "off"})
EOF
)
check "in the order they are made" \
  "$(is "$out" "[('unknown',), ('set', 'GOPROXY', 'off'), ('set', 'X', '1')] [('unknown',), ('set', 'GOPROXY', 'off')]")"
check "an f-string's text, each {...} as a placeholder" \
  "$(is "$(py 'import ast; print(repr(scan.python_text(ast.parse(sys.argv[1]).body[0].value)))' 'f"go list {pkg}"')" "'go list \\x00'")"

echo "case: shell: simple commands, nested ones, continued lines joined, and no heredoc body"
out=$(py 'for c in scan.shell_script(sys.stdin.read()):
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
out=$(py 'c = scan.shell_script(sys.argv[1])[0]
print(scan.shell_env_elements(c, c.values().index("go")))' 'GOPROXY=off env -u A B=c go build')
check "its prefix, then env's -u and NAME=value before the go" \
  "$(is "$out" "[('set', 'GOPROXY', 'off'), ('set', 'A', None), ('set', 'B', 'c')]")"
out=$(py 'print([s for c in scan.shell_script(sys.stdin.read()) for s in scan.shell_inner_scripts(c)])' <<'EOF'
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

echo "case: a reason counts on the line, or on a comment line within five above"
reasoned() { py 'import re
raw = sys.stdin.read().split("\n")
print(str(scan.reasoned(raw, sys.argv[1], re.compile(r"\bwhy:\s*\S"), [int(sys.argv[2])])).lower())' "$@"; }
check "on the line itself" "$(is "$(printf 'x\nrun() // why: here\n' | reasoned go 1)" true)"
check "five lines above" "$(is "$(printf '// why: above\n1\n2\n3\n4\nrun()\n' | reasoned go 5)" true)"
check "six lines above is too far" "$(is "$(printf '// why: above\n1\n2\n3\n4\n5\nrun()\n' | reasoned go 6)" false)"
check "a line above that is no comment does not count" "$(is "$(printf 'x := "why: here"\nrun()\n' | reasoned go 1)" false)"

# --- One row both checks read through this module ---------------------------------------------------

# both <name>: a tree holding both checks and this module, a ci.yaml, a file of each language the
# go check reads and a Go test file the Oh My Pi check reads. Echoes its root.
both() {
  local root="$work/$1"
  mkdir -p "$root/.github/scripts" "$root/.github/workflows" "$root/packages/app" "$root/packages/web" "$root/tools"
  for linked in check-go-offline.sh check-omp-offline.sh source_scan.py; do
    ln -s "$script_dir/$linked" "$root/.github/scripts/$linked"
  done
  printf 'name: CI\non: pull_request\njobs:\n  lint:\n    runs-on: ubuntu-24.04\n    steps:\n      - run: echo lint\n' \
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

summary "source_scan.py"
