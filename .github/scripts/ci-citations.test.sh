#!/usr/bin/env bash
# Tests for ci_citations.py, which holds each citation token under .github, and in each //ci: line
# outside it, to the file it names and refuses a citation of any other shape there:
# - a token whose name its file holds passes and is counted; after a rename in that file it
#   fails, naming the path and the name; a name that only a comment of the file holds fails, and
#   so do a text name that a longer word holds at either end, a hyphen included, and an
#   identifier that is a prefix;
# - a token whose path is no tracked file fails: a path never tracked, a file name without its
#   root path, a file deleted, a file renamed with git mv, a file written and never added; and a
#   file added elsewhere under the same name changes no token, while a bare path word that then
#   names two files is shown as written;
# - each cited file is read by its language's own scanner, which steps over its strings, so a
#   comment marker inside a string removes nothing (the shape of the playwright config that
#   review 6033494723 found): TypeScript by its parser, which steps over a template's expression
#   and a regular expression and keeps JSX text; Python by tokenize, Go by go/scanner, which ends
#   a raw string at its backtick; and in all three an identifier must be an identifier token, so
#   a string or a docstring holds none; shell and YAML lose their hash comments;
# - a scanner program that fails fails the check, naming its command;
# - the Go scanner runs under go_build_tags.go_environment(), so a caller's Go variables, under
#   which a plain go run of it fails, change nothing the check reads;
# - a code span that starts with a path word and goes on with a hash or a colon and a name, and
#   is no token, is refused, a token wrapped onto another line included: over three lines, and
#   wrapped just before its hash, in prose and in a comment; a command wrapped in a comment is
#   not;
# - a path word followed right away by a hash and a name outside a token is refused, with no
#   backticks around it or inside a code span that other text starts; a Markdown heading's
#   anchor is not, and nor is a colon after a path inside a command's code span;
# - a token names text its file holds once, so the rename of the case arm or the deletion of
#   the list entry it cites fails it, while a token that names another part of the same file,
#   the arm's usage line or the root's file name in another string, still holds;
# - each spelling of a line number beside a path word is refused, and so is a token whose name
#   is a line number;
# - a backticked text that is no token, right after a path word (a possessive, a parenthesis or
#   a line break between them included), is refused with the message that names the token to
#   write, and so is a backticked text before in and a path; one further on in the sentence is
#   not, which is the stated limit of that rule;
# - a file outside .github is not read but for its //ci: lines, and ordinary text that names no
#   path passes;
# - a //ci: line outside .github is read as a text of its own: its token is held and counted, an
#   indented one too, and fails, naming the line, after a rename in the cited file; a line number
#   in it is refused; and a //ci: that does not start its line is not read, nor is any other line
#   of the file, nor the next //ci: line with it;
# - a scan that finds no token fails rather than passing;
# - and this repository's own .github passes, with at least 700 tokens, and so do the //ci: lines
#   outside it, at least 200, with at least 19 tokens among them.
#
# This file is under .github, so the real scan reads it as text. Every token, line number and
# backticked text a case needs is built at run time from bt, h and c below, so this file's own
# text holds none of them.
#
# Each case builds a miniature git repository under a temporary directory and symlinks the real
# script into it, so the script under test is the file CI runs: it resolves its root from its own
# path, which the symlink puts at the fixture root, and it reads no symlink. The scanner programs
# it runs are the ones beside the real script, with this repository's typescript package, so the
# harness needs Bun and Go, as the lint job has them; where no bun install has run, Bun installs
# typescript itself, as ci_citations.py's docstring says.
#
# Run from anywhere: .github/scripts/ci-citations.test.sh
# CI runs it in the lint job, before the check itself.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo=$(cd "$script_dir/../.." && pwd)
check_script="$script_dir/ci_citations.py"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

bt='`'
h='#'
c=':'

# tok <path> <name>: a citation token.
tok() { printf '%s%s%s%s%s' "$bt" "$1" "$h" "$2" "$bt"; }

# fixture <name>: a git repository holding the script and the files the cases cite. Echoes its
# root. packages/app/main_test.go defines TestReadsTheConfig, runs a test titled "the agent
# rows", names TestOldName only in a comment and holds the strings setup-golang and setup-go-x;
# playwright.config.ts, run.sh and tool.py each hold a comment marker inside a string ahead of
# a name; guard.test.ts, note.tsx, tool.py, scan.go and ci.yaml hold the shapes each language's
# case reads; push.sh holds a case arm no message of it holds, whose command its usage line
# names too, and roots.test.ts a list entry whose file name another string holds, as
# release-push.sh and skills-guard.test.ts do.
fixture() {
  local root="$work/$1"
  mkdir -p "$root/.github/scripts" "$root/.github/ci-notes" "$root/.github/docker" "$root/.github/workflows" \
    "$root/packages/app"
  ln -s "$check_script" "$root/.github/scripts/ci_citations.py"
  cat > "$root/packages/app/main_test.go" <<'EOF'
package app

// TestOldName was renamed.
var tool = "setup-golang"
var other = "setup-go-x"
func TestReadsTheConfig(t *testing.T) { runtime.Caller(0); t.Run("the agent rows", nil) }
EOF
  cat > "$root/packages/app/playwright.config.ts" <<'EOF'
// The suites: ls e2e/*.e2e.ts lists them.
const SUITES = "e2e/*.spec.ts";
const HOME = "http://localhost:3000"; const ROW_TIMEOUT = 5000;
export default { projects: [{ name: "iphone" }] };
/* The end of the projects. */
EOF
  # A template whose expression holds a comment, then a regular expression that holds one
  # backtick, which a stripper that reads it as a template's opening reads the rest of the file
  # as: the shape of pi-legion's skills-guard.test.ts that review 6038895115 found.
  cat > "$root/packages/app/guard.test.ts" <<EOF
const b = 1;
const LINE = ${bt}a \${b /* TS_ONLY_IN_A_TEMPLATE_COMMENT */} c${bt};
const TICK = /${bt}/;
// TS_ONLY_AFTER_A_REGEX
const LABEL = "TS_ONLY_IN_A_STRING";
export const AFTER_THE_REGEX = 1;
EOF
  cat > "$root/packages/app/note.tsx" <<'EOF'
export const Note = () => <p>// the jsx text after a slash pair</p>;
EOF
  cat > "$root/packages/app/run.sh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "a # in a string"; held_after_hash=1
# only_in_a_comment
EOF
  cat > "$root/packages/app/push.sh" <<'EOF'
#!/usr/bin/env bash
case $1 in
  release)
    usage='usage: push.sh release [<tag>]'
    echo "no tag: push v0.0.0 on the first release's parent commit" >&2
    ;;
esac
EOF
  cat > "$root/packages/app/roots.test.ts" <<'EOF'
const roots = [path.join(REPO_ROOT, "AGENTS.md"), path.join(REPO_ROOT, "docs")];
expect(dispatching).toEqual(["skills/AGENTS.md"]);
EOF
  cat > "$root/packages/app/tool.py" <<'EOF'
"""PY_ONLY_IN_A_MODULE_DOCSTRING is named in this docstring alone."""
import os
MARK = "# in a string"; HELD_AFTER_MARK = 1
# PY_ONLY_IN_A_COMMENT


def reads():
    """PY_ONLY_IN_A_DOCSTRING is named in this docstring alone."""
    return os.path.join("PY_ONLY_IN_A_STRING")
EOF
  # A raw string that ends in a backslash, which no Go escape is: a stripper that reads it as
  # one reads the rest of the file as the string.
  cat > "$root/packages/app/scan.go" <<EOF
package app

var raw = ${bt}\\${bt}

// GO_ONLY_AFTER_A_RAW_STRING
var label = "GO_ONLY_IN_A_STRING"

func Scanned() {}
EOF
  printf 'FROM scratch\nRUN true\n' > "$root/.github/docker/browser.Dockerfile"
  printf '* @owner\n' > "$root/.github/CODEOWNERS"
  printf 'dist/\n' > "$root/.gitignore"
  cat > "$root/.github/workflows/ci.yaml" <<'EOF'
jobs:
  x:
    runs-on: ubuntu-24.04
    # yaml-only-in-a-comment
    steps:
      - run: echo "a # b" && yaml-held-after-hash
EOF
  echo "$root"
}

# track <root>: tracks every file of the fixture, as the script reads tracked files only.
track() {
  git -C "$1" init -q
  git -C "$1" add -A
}

run_check() { # run_check <root>
  set +e
  out=$(cd "$1" && python3 .github/scripts/ci_citations.py 2>&1)
  status=$?
  set -e
}

notes() { # notes <root>: the fixture's citing Markdown file under .github, from stdin
  cat > "$1/.github/ci-notes/notes.md"
}

# refused <line> <text>: true when the output refuses notes.md at that line with a message
# containing the text, for check. Here-strings, not a pipe: test-lib.sh's contains says why.
refused() {
  local at
  at=$(grep -F -- ".github/ci-notes/notes.md$c$1: " <<<"$out" || true)
  grep -qF -- "$2" <<<"$at" && echo true || echo false
}

# clean <line>: true when the output names no problem of notes.md at that line, for check.
clean() { is "$(grep -cF -- ".github/ci-notes/notes.md$c$1: " <<<"$out")" 0; }

# refused_in <file> <line> <text>: refused, for a citing file outside .github.
refused_in() {
  local at
  at=$(grep -F -- "$1$c$2: " <<<"$out" || true)
  grep -qF -- "$3" <<<"$at" && echo true || echo false
}

app="packages/app/main_test.go"
pw="packages/app/playwright.config.ts"

echo "case: a token whose name its file holds passes, and is counted"
root=$(fixture held)
notes "$root" <<EOF
The config is read by $(tok "$app" TestReadsTheConfig),
$(tok "$app" runtime.Caller) and $(tok "$app" '"the agent rows"').
EOF
track "$root"
run_check "$root"
check "passes" "$(is "$status" 0)"
check "counts three tokens in one file" "$(contains "$out" '^3 citation tokens in 1 of the [0-9]* text files under .github/')"

echo "case: the same token after a rename in the cited file fails, naming the path and the name"
sed -i 's/TestReadsTheConfig/TestReadsItsConfig/' "$root/$app"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the path and the name" "$(refused 1 "${bt}$app${bt} holds no ${bt}TestReadsTheConfig${bt}")"
check "and holds the other two" "$(is "$(grep -c 'holds no' <<<"$out")" 1)"

echo "case: a name only a comment holds, a text a longer word holds at either end or an identifier prefix fails"
root=$(fixture comments)
notes "$root" <<EOF
$(tok "$app" TestOldName)

$(tok "$app" setup-go)

$(tok "$app" TestReads)

$(tok "$app" setup-go-x)
EOF
track "$root"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "on the name its comment alone holds" "$(refused 1 "holds no ${bt}TestOldName${bt}")"
check "on the text that setup-golang and setup-go-x hold, a hyphen being a word's" \
  "$(refused 3 "holds no ${bt}setup-go${bt}")"
check "on the identifier that is a prefix" "$(refused 5 "holds no ${bt}TestReads${bt}")"
check "and holds the text that a hyphen ends nowhere" "$(clean 7)"

echo "case: a token whose path is no tracked file fails"
root=$(fixture untracked)
notes "$root" <<EOF
$(tok packages/app/gone_test.go TestGone)

$(tok main_test.go TestReadsTheConfig)

$(tok packages/app/run.sh held_after_hash)

$(tok "$app" TestReadsTheConfig)

$(tok packages/app/written_test.go TestWritten)
EOF
track "$root"
git -C "$root" rm -qf packages/app/run.sh
git -C "$root" mv "$app" packages/app/reads_test.go
# On disk and never added, so only git's list can say it is no tracked file.
printf 'package app\n\nfunc TestWritten(t *testing.T) {}\n' > "$root/packages/app/written_test.go"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "on a path never tracked" "$(refused 1 "${bt}packages/app/gone_test.go${bt} names no tracked file")"
check "on a file name without its root path" "$(refused 3 "${bt}main_test.go${bt} names no tracked file")"
check "on a deleted file" "$(refused 5 "${bt}packages/app/run.sh${bt} names no tracked file")"
check "on a file git mv renamed" "$(refused 7 "${bt}$app${bt} names no tracked file")"
check "on a file written and never added" "$(refused 9 "${bt}packages/app/written_test.go${bt} names no tracked file")"

echo "case: a file added elsewhere under the same name changes no token, and a bare path word then names two"
root=$(fixture shadowed)
notes "$root" <<EOF
$(tok "$app" TestReadsTheConfig) and $(tok .github/workflows/ci.yaml runs-on)
EOF
track "$root"
run_check "$root"
before=$out
cat >> "$root/.github/ci-notes/notes.md" <<EOF

The job is ci.yaml's ${bt}x${bt}.
EOF
run_check "$root"
check "a bare path word that names one file is shown by its root path" \
  "$(refused 3 "write it as $(tok .github/workflows/ci.yaml x)")"
mkdir -p "$root/packages/zz"
cp "$root/.github/workflows/ci.yaml" "$root/packages/zz/ci.yaml"
printf 'package zz\n' > "$root/packages/zz/main_test.go"
git -C "$root" add -A
run_check "$root"
check "after the copy it names two, and is shown as written" "$(refused 3 "write it as $(tok ci.yaml x)")"
check "while the tokens beside it still hold" "$(clean 1)"
notes "$root" <<EOF
$(tok "$app" TestReadsTheConfig) and $(tok .github/workflows/ci.yaml runs-on)
EOF
run_check "$root"
check "passes" "$(is "$status" 0)"
check "with the same count as before" "$(is "$out" "$before")"

echo "case: the cited file's comments are removed in one pass that steps over its strings"
root=$(fixture strings)
notes "$root" <<EOF
$(tok "$pw" '"iphone"') and $(tok "$pw" ROW_TIMEOUT), after a /* and a // inside strings;
$(tok packages/app/run.sh held_after_hash), after a # inside a string; and
$(tok packages/app/tool.py HELD_AFTER_MARK), after a # inside a Python string.

$(tok packages/app/run.sh only_in_a_comment)

$(tok packages/app/tool.py PY_ONLY_IN_A_COMMENT)
EOF
track "$root"
run_check "$root"
check "fails on the two names only a comment holds" "$(is "$(grep -c 'holds no' <<<"$out")" 2)"
check "the shell comment" "$(refused 5 "holds no ${bt}only_in_a_comment${bt}")"
check "the Python comment" "$(refused 7 "holds no ${bt}PY_ONLY_IN_A_COMMENT${bt}")"
check "and holds every name after a marker inside a string" "$(is "$(grep -c 'notes.md.[123]: ' <<<"$out")" 0)"

echo "case: TypeScript is read by its own parser, so a regular expression or a template opens no string"
root=$(fixture typescript)
ts="packages/app/guard.test.ts"
notes "$root" <<EOF
$(tok "$ts" TS_ONLY_IN_A_TEMPLATE_COMMENT)

$(tok "$ts" TS_ONLY_AFTER_A_REGEX)

$(tok "$ts" TS_ONLY_IN_A_STRING)

$(tok "$ts" AFTER_THE_REGEX) and $(tok "$ts" '"TS_ONLY_IN_A_STRING"')

$(tok packages/app/note.tsx 'the jsx text after a slash pair') and $(tok packages/app/note.tsx Note)
EOF
track "$root"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "on a name only a comment inside a template's expression holds" \
  "$(refused 1 "holds no ${bt}TS_ONLY_IN_A_TEMPLATE_COMMENT${bt}")"
check "on a name only the comment after a regular expression holding a backtick holds" \
  "$(refused 3 "holds no ${bt}TS_ONLY_AFTER_A_REGEX${bt}")"
check "on an identifier only a string holds" "$(refused 5 "holds no ${bt}TS_ONLY_IN_A_STRING${bt}")"
check "and holds the code after the regular expression, and the string by its quotes" "$(clean 7)"
check "and holds JSX text that starts with a slash pair, which is no comment" "$(clean 9)"

echo "case: Python is read by tokenize, so an identifier no NAME token holds fails"
root=$(fixture python)
py="packages/app/tool.py"
notes "$root" <<EOF
$(tok "$py" PY_ONLY_IN_A_MODULE_DOCSTRING)

$(tok "$py" PY_ONLY_IN_A_DOCSTRING)

$(tok "$py" PY_ONLY_IN_A_STRING)

$(tok "$py" reads), $(tok "$py" HELD_AFTER_MARK) and $(tok "$py" os.path.join)
EOF
track "$root"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "on a name only the module's docstring holds" "$(refused 1 "holds no ${bt}PY_ONLY_IN_A_MODULE_DOCSTRING${bt}")"
check "on a name only a function's docstring holds" "$(refused 3 "holds no ${bt}PY_ONLY_IN_A_DOCSTRING${bt}")"
check "on a name only a string holds" "$(refused 5 "holds no ${bt}PY_ONLY_IN_A_STRING${bt}")"
check "and holds two NAME tokens and a dotted name of the code" "$(clean 7)"

echo "case: Go is read by go/scanner, so a raw string ends at its backtick and an identifier must be one"
root=$(fixture go)
go="packages/app/scan.go"
notes "$root" <<EOF
$(tok "$go" GO_ONLY_AFTER_A_RAW_STRING)

$(tok "$go" GO_ONLY_IN_A_STRING)

$(tok "$go" Scanned) and $(tok "$go" '"GO_ONLY_IN_A_STRING"')
EOF
track "$root"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "on a name only the comment after a raw string that ends in a backslash holds" \
  "$(refused 1 "holds no ${bt}GO_ONLY_AFTER_A_RAW_STRING${bt}")"
check "on an identifier only a string holds" "$(refused 3 "holds no ${bt}GO_ONLY_IN_A_STRING${bt}")"
check "and holds the code after it, and the string by its quotes" "$(clean 5)"

echo "case: YAML loses its hash comments, and keeps the text after a hash inside a quoted string"
root=$(fixture yaml)
notes "$root" <<EOF
$(tok .github/workflows/ci.yaml yaml-only-in-a-comment)

$(tok .github/workflows/ci.yaml yaml-held-after-hash)
EOF
track "$root"
run_check "$root"
check "fails on the name only a comment holds" "$(refused 1 "holds no ${bt}yaml-only-in-a-comment${bt}")"
check "and holds the name after a hash inside a string" "$(clean 3)"

echo "case: a scanner program that fails fails the check, naming its command"
root=$(fixture scanner)
notes "$root" <<EOF
$(tok "$pw" ROW_TIMEOUT)
EOF
track "$root"
mkdir -p "$work/scanner-bin"
printf '#!/usr/bin/env bash\necho "bun: no such runtime" >&2\nexit 3\n' > "$work/scanner-bin/bun"
chmod +x "$work/scanner-bin/bun"
PATH="$work/scanner-bin:$PATH" run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the command, its exit and what it printed" \
  "$(contains "$out" "^ci_citations.py: ${bt}bun .github/scripts/ci_citations_code.ts${bt} over 1 files exited 3 and printed the code of 0, so they hold no name: bun: no such runtime$")"

echo "case: a caller's Go variables change nothing the Go scan reads"
# The Go scanner runs under go_build_tags.go_environment() with GOWORK=off, so no Go variable of
# the caller's reaches it but those that say where go keeps or fetches files. Each one set here
# changes what a go command that reads it does: GOOS and GOARCH build the scanner for windows/386,
# whose binary go run cannot start here; GOEXPERIMENT names an experiment no go knows; GOFLAGS asks
# for -race, which windows/386 has not; GOTOOLCHAIN=local, GO111MODULE=off and a go env file's
# GOFLAGS (GOENV, not XDG_CONFIG_HOME, which a version manager's python3 or go shim reads too) are
# set with them. A plain go run of the scanner under them fails first, so the case can fail. The
# clean run is held to a pass that holds the Go token first, so the two runs cannot agree by both
# failing.
root=$(fixture go-environment)
notes "$root" <<EOF
$(tok "$app" TestReadsTheConfig)
EOF
track "$root"
printf 'GOFLAGS=-tags=polluted\n' > "$work/go-environment.env"
polluted=(GOTOOLCHAIN=local GOOS=windows GOARCH=386 GOEXPERIMENT=pollutedexperiment GOFLAGS=-race
  GO111MODULE=off GOENV="$work/go-environment.env")
set +e
(cd "$root" && env "${polluted[@]}" GOWORK=off go run "$script_dir/ci_citations_code.go" </dev/null >/dev/null 2>&1)
plain_status=$?
set -e
check "a plain go run of the scanner under the caller's Go variables fails" "$(is "$((plain_status != 0))" 1)"
run_check "$root"
clean_out=$out
clean_status=$status
check "passes clean" "$(is "$clean_status" 0)"
check "and holds the Go token clean" "$(contains "$clean_out" '^1 citation tokens in 1 of')"
set +e
out=$(cd "$root" && env "${polluted[@]}" python3 .github/scripts/ci_citations.py 2>&1)
status=$?
set -e
check "gives the clean answer under the caller's Go variables" "$(is "$status:$out" "$clean_status:$clean_out")"

echo "case: a code span that starts with a path and goes on as a citation is a token or is refused"
root=$(fixture spans)
notes "$root" <<EOF
${bt}.github/workflows/ci.yaml${c}no_such_output${bt}

${bt}ci.yaml${c}${c}x${bt}

${bt}ci.yaml ${h}x${bt}

The rows are ${bt}$app${h}the agent
rows${bt}, wrapped.

The rows are ${bt}$app${h}the agent
${h} rows${bt}, wrapped in a comment.

${bt}ci.yaml${c}12${bt}

The rows are ${bt}$app${h}the
agent
rows${bt}, over three lines.

The job is ${bt}.github/workflows/ci.yaml
${h}no_such_job${bt}, wrapped just before its hash.

${h} The job is ${bt}.github/workflows/ci.yaml
${h}no_such_job${bt}, wrapped just before its hash in a comment.

${h} The job is ${bt}.github/workflows/ci.yaml
${h} ${h}no_such_job${bt}, wrapped just before its hash, past the comment marker.

${h} Run ${bt}packages/app/run.sh
${h} release${bt} to release, a command wrapped in a comment.
EOF
track "$root"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "refuses a colon between the path and the name" \
  "$(refused 1 "cites .github/workflows/ci.yaml in a form that is no citation token: write it as $(tok .github/workflows/ci.yaml no_such_output)")"
check "refuses two colons" "$(refused 3 "write it as $(tok .github/workflows/ci.yaml x)")"
check "refuses a space before the hash" "$(refused 5 "write it as $(tok .github/workflows/ci.yaml x)")"
check "refuses a token that wraps onto the next line, naming it joined" \
  "$(refused 7 "a citation token is one line: write it as $(tok "$app" 'the agent rows') on one line")"
check "and one wrapped past a comment marker" \
  "$(refused 10 "a citation token is one line: write it as $(tok "$app" 'the agent rows') on one line")"
check "leaves a line number after a colon to the line-number rule" "$(refused 13 'by a line number')"
check "refuses a token that runs over three lines, as a code span pairs across its paragraph" \
  "$(refused 15 "a citation token is one line: write it as $(tok "$app" 'the agent rows') on one line")"
check "refuses a token wrapped just before its hash, which no comment marker is" \
  "$(refused 19 "a citation token is one line: write it as $(tok .github/workflows/ci.yaml no_such_job) on one line")"
check "and the same wrap in a comment, where a hash with a name after it is no comment marker" \
  "$(refused 22 "a citation token is one line: write it as $(tok .github/workflows/ci.yaml no_such_job) on one line")"
check "and the hash after a comment marker and a space" \
  "$(refused 25 "a citation token is one line: write it as $(tok .github/workflows/ci.yaml no_such_job) on one line")"
check "while a command wrapped in a comment, its marker and a space before the next word, passes" "$(clean 28)"
check "and refuses each once" "$(is "$(grep -c 'notes.md' <<<"$out")" 10)"

echo "case: a path word followed right away by a hash and a name outside a token is refused"
root=$(fixture hashes)
notes "$root" <<EOF
The job is .github/workflows/ci.yaml${h}no_such_job, with no backticks.

The job is ${bt}see .github/workflows/ci.yaml${h}no_such_job${bt}, in a span the path does not start.

The job is ci.yaml${h}x, by its file name.

The rule is in .github/ci-notes/notes.md${h}a-heading, a link to a heading.

${bt}gh workflow run ci.yaml --ref main${bt} and ${bt}echo "${c}${c}error file=ci.yaml${c}${c}x"${bt} are commands.

$(tok "$app" TestReadsTheConfig) holds, so a pass would not be the pass of a scan that found no token.
EOF
track "$root"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "refuses a token with no backticks" \
  "$(refused 1 "cites .github/workflows/ci.yaml outside backticks, and is no citation token: write it as $(tok .github/workflows/ci.yaml no_such_job)")"
check "refuses one inside a code span that other text starts" \
  "$(refused 3 "cites .github/workflows/ci.yaml inside a code span that does not start with it, and is no citation token: write it as $(tok .github/workflows/ci.yaml no_such_job)")"
check "refuses a file name and a hash, naming the token by its root path" \
  "$(refused 5 "write it as $(tok .github/workflows/ci.yaml x)")"
check "passes a Markdown file's path, a hash and a heading's anchor" "$(clean 7)"
check "and a colon after a path inside a command's code span" "$(clean 9)"
check "and refuses nothing else" "$(is "$(grep -c 'notes.md' <<<"$out")" 3)"

echo "case: a token names text its file holds once, so the rename of what it cites fails it"
# The shape review 6043861656 found: a token holds on after the rename of what it names, where
# another part of the cited file holds its text too. roots.test.ts holds the AGENTS.md root's
# file name in another string, so the root is cited by its list entry, text that file holds
# once. push.sh's message no longer holds the text of its case arm, as release-push.sh's does
# not, so the arm is such a text and is cited by itself; the usage line is not, as the last
# check shows: a token of that line passes the rename of the arm alone.
root=$(fixture once)
push="packages/app/push.sh"
roots="packages/app/roots.test.ts"
notes "$root" <<EOF
$(tok "$push" 'release)')

$(tok "$roots" 'path.join(REPO_ROOT, "AGENTS.md")')

$(tok "$push" 'usage: push.sh release [') and $(tok "$roots" AGENTS.md)
EOF
track "$root"
run_check "$root"
check "passes before the rename" "$(is "$status" 0)"
sed -i 's/^  release)$/  publish)/' "$root/$push"
sed -i 's/path.join(REPO_ROOT, "AGENTS.md"), //' "$root/$roots"
run_check "$root"
check "fails after it" "$(is "$status" 1)"
check "on the renamed case arm" "$(refused 1 "holds no ${bt}release)${bt}")"
check "on the list entry of the deleted root" "$(refused 3 "holds no ${bt}path.join(REPO_ROOT, \"AGENTS.md\")${bt}")"
check "while the arm's usage line and the root's file name, which the rename left, still hold" "$(clean 5)"

echo "case: a line number beside a path word is refused, in every spelling"
root=$(fixture lines)
# One spelling a paragraph: the line each one starts on is the line the refusal names.
spellings=(
  "$app${c}4 reads it"
  "$app${c}L4 reads it"
  "$app${h}L4 reads it"
  "$app${h}4 reads it"
  "$app L4 reads it"
  "$app line 4 reads it"
  "$app (line 4) reads it"
  "$app's line 4 reads it"
  "$app, line 4, reads it"
  "lines 3 to 4 of $app read it"
  "$app (l. 4) reads it"
  "$app (${h}4) reads it"
  "$app line4 reads it"
  "it is $app${c}"
  "$app reads it (${c}4)"
  "$app reads it at ${c}4"
  "$app and ${c}4 read it"
  "$app, ${c}4, reads it"
  "$(tok "$app" 4) reads it"
  "browser.Dockerfile${c}2, CODEOWNERS${c}1 and .gitignore${c}1 read it"
)
for spelling in "${spellings[@]}"; do
  printf '%s\n\n' "$spelling"
done > "$root/.github/ci-notes/notes.md"
# The wrap: the number starts the line after the one that ends with the path and a colon.
sed -i '28s/^$/4 is the line/' "$root/.github/ci-notes/notes.md"
track "$root"
run_check "$root"
check "fails" "$(is "$status" 1)"
for i in "${!spellings[@]}"; do
  check "refuses: ${spellings[$i]}" "$(refused $((2 * i + 1)) 'by a line number; write it as')"
done
check "names the path a file name resolves to" \
  "$(refused 39 "cites ${bt}.github/docker/browser.Dockerfile${bt} by a line number; write it as $(tok .github/docker/browser.Dockerfile Name)")"
check "and every kind of path word" "$(is "$(grep -c "notes.md${c}39: " <<<"$out")" 3)"

echo "case: a backticked text that is no token, right beside a path word, is refused"
root=$(fixture nontoken)
notes "$root" <<EOF
The job is ci.yaml's ${bt}x${bt}.

The job ${bt}x${bt} of ci.yaml.

The step is ${bt}ci.yaml${bt} (${bt}y${bt}).

The job is in ci.yaml
${bt}z${bt}, on the next line.
EOF
track "$root"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the token to write" "$(refused 1 "write it as $(tok .github/workflows/ci.yaml x)")"
check "refuses the text written before in, of or from and the path" \
  "$(refused 3 "${bt}x${bt} is followed by ci.yaml, and is no citation token: write it as $(tok .github/workflows/ci.yaml x)")"
check "reads a path in backticks and a name in parentheses" "$(refused 5 "write it as $(tok .github/workflows/ci.yaml y)")"
check "and a name on the line after the path" "$(refused 8 "write it as $(tok .github/workflows/ci.yaml z)")"
check "and refuses nothing else" "$(is "$(grep -c 'no citation token' <<<"$out")" 4)"

echo "case: a backticked text three words or more after a path word is not read, the stated limit of rule 4b"
# This pass is the stated limit of rule 4b, the rule that refuses a backticked text beside a path
# word, and not an accident: the rule reads a backticked text only right after a path word or
# before in, of or from and one, and reads any other as prose, so a citation written further from
# its path is neither held nor refused. ci_citations.py's docstring and ci.md say so. The name is
# one ci.yaml does not hold, so the pass shows the text was not read as a citation; the same name
# right after the path is then refused, so the pass is the rule's reach, not a file left unread.
root=$(fixture beyond)
notes "$root" <<EOF
$(tok "$app" TestReadsTheConfig)

ci.yaml holds, among others, the ${bt}no_such_output${bt} output.
EOF
track "$root"
run_check "$root"
check "passes" "$(is "$status" 0)"
check "and counts the one token" "$(contains "$out" '^1 citation tokens in 1 of')"
notes "$root" <<EOF
$(tok "$app" TestReadsTheConfig)

ci.yaml's ${bt}no_such_output${bt} output.
EOF
run_check "$root"
check "while the same name right after the path is refused" \
  "$(refused 3 "write it as $(tok .github/workflows/ci.yaml no_such_output)")"

echo "case: a file outside .github is not read, but for its //ci: lines"
root=$(fixture outside)
notes "$root" <<EOF
$(tok "$app" TestReadsTheConfig)
EOF
cat > "$root/packages/app/README.md" <<EOF
$(tok "$app" TestGone), $(tok packages/app/gone.go Gone), $app${c}4 and $app ${bt}Gone${bt}.
EOF
track "$root"
run_check "$root"
check "passes" "$(is "$status" 0)"
check "and counts the one token under .github" "$(contains "$out" '^1 citation tokens in 1 of')"

echo "case: a //ci: line outside .github is read: its token is held and counted, and a rename fails it"
root=$(fixture ci-held)
ci="packages/app/reads_test.go"
# The second line is indented, which the //ci: parser reads as a line too.
cat > "$root/$ci" <<EOF
package app

//ci:reads packages/app/run.sh $(tok "$app" TestReadsTheConfig) runs it
	//ci:builds packages/app . $(tok packages/app/scan.go Scanned) builds it
EOF
track "$root"
run_check "$root"
check "passes" "$(is "$status" 0)"
check "counts the token of each line, and the lines" \
  "$(contains "$out" '^2 citation tokens in 0 of the [0-9]* text files under .github/ and 2 of the 2 //ci: lines outside it ')"
sed -i 's/TestReadsTheConfig/TestReadsItsConfig/' "$root/$app"
run_check "$root"
check "fails after the rename" "$(is "$status" 1)"
check "naming the //ci: line, the path and the name" \
  "$(refused_in "$ci" 3 "${bt}$app${bt} holds no ${bt}TestReadsTheConfig${bt}")"
check "and that line alone, the indented line's token still held" "$(is "$(grep -cF -- "$ci$c" <<<"$out")" 1)"

echo "case: a //ci: line that cites by a line number is refused"
root=$(fixture ci-numbers)
cat > "$root/$ci" <<EOF
package app

//ci:reads packages/app/run.sh $app${c}6 runs it
//ci:reads packages/app/run.sh $(tok "$app" 6) runs it
//ci:reads packages/app/run.sh TestReadsTheConfig runs it (main_test.go${c}6)
EOF
track "$root"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "refuses a path, a colon and a number" "$(refused_in "$ci" 3 "cites ${bt}$app${bt} by a line number")"
check "refuses a token whose name is a line number" "$(refused_in "$ci" 4 "cites ${bt}$app${bt} by a line number")"
check "refuses a file name and a number, naming its root path" \
  "$(refused_in "$ci" 5 "cites ${bt}$app${bt} by a line number")"

echo "case: outside .github only a line that starts with //ci: is read, and each such line alone"
# The texts the case above refuses stand here in a comment, a string and after code, where no
# //ci: line starts, and the token of each names what main_test.go does not hold: read, they
# would fail. The two //ci: lines are read alone: the bare colon and number of the second line
# stand beside no path of its own, while the same two lines under .github, read as one text,
# put it beside the path that ends the first line.
root=$(fixture ci-only)
cat > "$root/$ci" <<EOF
package app

//ci:reads packages/app/run.sh $(tok "$app" TestReadsTheConfig) runs packages/app/run.sh
//ci:reads-set app_set TestReadsTheConfig reads them, and ${c}4

// A comment that is no //ci: line: $app${c}6 and $(tok "$app" TestGone).
var notALine = "//ci:reads packages/app/run.sh $app${c}6 $(tok "$app" TestGone)"
var afterCode = 1 //ci:reads packages/app/run.sh $app${c}6 $(tok "$app" TestGone)
EOF
track "$root"
run_check "$root"
check "passes" "$(is "$status" 0)"
check "and counts the two //ci: lines and the one token" \
  "$(contains "$out" '^1 citation tokens in 0 of the [0-9]* text files under .github/ and 1 of the 2 //ci: lines outside it ')"
notes "$root" <<EOF
//ci:reads packages/app/run.sh $(tok "$app" TestReadsTheConfig) runs packages/app/run.sh
//ci:reads-set app_set TestReadsTheConfig reads them, and ${c}4
EOF
git -C "$root" add -A
run_check "$root"
check "while the same two lines under .github are refused" \
  "$(refused 1 "cites ${bt}packages/app/run.sh${bt} by a line number")"
check "and the test file's are not" "$(is "$(grep -cF -- "$ci$c" <<<"$out")" 0)"

echo "case: ordinary text that names no path passes"
root=$(fixture ordinary)
notes "$root" <<EOF
$(tok "$app" TestReadsTheConfig)

The proxy listens on ${c}8080, exits (exit ${c}1) when refused, and runs hourly at ${c}15.
See line 3 of the output, or lines 2 and 3; ${bt}read -r line 2>/dev/null${bt} reads one.
A text further on in a sentence that names ci.yaml, such as ${bt}x${bt}, is no name beside it,
and nor is a path that a longer backticked text holds, as ${bt}ci.yaml x${bt} ${bt}y${bt} does.
EOF
track "$root"
run_check "$root"
check "passes" "$(is "$status" 0)"

echo "case: a scan that finds no token fails rather than passing"
root=$(fixture empty)
printf '# Notes\n' > "$root/.github/ci-notes/notes.md"
track "$root"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "says it found no token" "$(contains "$out" '^found no citation token in the [0-9]* text files under .github/')"

echo "case: this repository's own .github and //ci: lines hold every token and no citation of another shape"
run_check "$repo"
check "passes" "$(is "$status" 0)"
count=$(sed -n 's/^\([0-9][0-9]*\) citation tokens in .*/\1/p' <<<"$out")
check "over at least 700 tokens, so a scan that stopped finding them fails here" "$(is "$(( ${count:-0} >= 700 ))" 1)"
ci_counts=' and \([0-9][0-9]*\) of the \([0-9][0-9]*\) //ci: lines outside it '
ci_lines=$(sed -n "s|.*$ci_counts.*|\2|p" <<<"$out")
ci_holding=$(sed -n "s|.*$ci_counts.*|\1|p" <<<"$out")
check "and reads at least 200 //ci: lines outside it, so a scan that stopped reading them fails here" \
  "$(is "$(( ${ci_lines:-0} >= 200 ))" 1)"
check "of which at least 19 hold a token, the ones that cited by a line number" \
  "$(is "$(( ${ci_holding:-0} >= 19 ))" 1)"

summary "ci_citations.py holds each citation token under .github and in each //ci: line outside it to its file and refuses every other shape"
