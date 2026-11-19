#!/usr/bin/env bash
# Tests for `source_scan.py`, the reading of source files check-go-offline.sh and
# check-omp-offline.sh share: the language of a path (.mts and .cts are TypeScript, a .d.ts is no
# source), the walk that skips a test's inputs, installed packages and links, the lexing of Go,
# TypeScript and Python into code with comments blanked and string text blanked but for the
# literals the caller keeps, shell read the same way, a continued shell line, its words, and where a
# reason comment counts. Last, one row both checks read through it: a .mts test that holds a quoted
# example of a start and, in its control, the start itself; each check names the start and neither
# reads the example.
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

# shell <file>: the shell code of this harness's stdin, line for line.
shell() { py 'print("\n".join(scan.shell_code(sys.stdin.read())), end="")'; }

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

echo "case: shell: quoted text, comments and a heredoc's body blank; a \$(...) in double quotes is code"
out=$(shell <<'EOF'
echo "go build $(go version)" # go list
cat <<'TEXT'
go build
TEXT
"$root/bin/go" build ./...
EOF
)
check "the quoted text blanks and its \$(...) stays" \
  "$(is "$(sed -n 1p <<<"$out" | sed 's/ *$//')" 'echo           $(go version)')"
check "the heredoc's body is blank" "$(is "$(sed -n 3p <<<"$out" | tr -d ' ')" "")"
check "a double-quoted path ending /go is the word go" "$(is "$(sed -n 5p <<<"$out")" 'go             build ./...')"

echo "case: a continued shell line, and the words of a line"
check "the line and the lines its backslashes continue" \
  "$(is "$(py 'print(scan.continued(["a \\", "  b \\", "  c", "d"], 0))')" $'a \\\n  b \\\n  c')"
check "operators are words of their own and the comment is dropped" \
  "$(is "$(py 'print("|".join(scan.shell_words(sys.argv[1])))' 'cd "a b" && go test ./x; echo ok # c')" \
    'cd|a b|&&|go|test|./x|;|echo|ok')"

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
