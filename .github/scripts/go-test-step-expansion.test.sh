#!/usr/bin/env bash
# Tests for `go-test-step-expansion.py`'s reading of one line of a step's script: which command a
# line runs, and which shapes it refuses rather than guesses at; and that each script
# AUDITED_SCRIPTS reads still has the SHA-256 its entry pins.
#
# Only the line reader and the pins. What the script makes of a command - the packages a
# `go test -list` names, the pairs two steps share, the exceptions - needs all three Go modules'
# test binaries built, which is minutes of compile and why no CI job runs that script (its own
# header says so). This harness compiles no package of this repository: every case is one line of
# text or a file's hash, but the cases on characters go test -run rewrites, which run go to build
# the script's small rewrite probe with the toolchain packages/envoy selects, as the script does,
# the case that runs a stand-in go that prints the environment the module's go commands and step
# lines get, and the cases that ask go for its GOROOT under a go.work of their own and a local
# proxy that never answers.
#
# The case that named this harness: a step whose line sets a variable before its command, or runs
# it under the memory wrapper, was refused, and the refusal stopped the whole run at exit 2 with
# no pair list at all - so the rule answered nothing about any pair while
# `RACE_MEM_LIMIT_KB=… race_memory.py go test …` stood in ci-job-envoy-go-race-docs.yaml and
# `GODEBUG=checkfinalizers=1 go test …` in ci-job-envoy-go-docs-finalizers.yaml.
#
# The module is imported from the directory it lives in, the way check-ci-filters.sh imports its
# own (PYTHONPATH, no bytecode), so the code under test is the file a person runs. It needs a
# python3 with PyYAML, which the module imports.
#
# Run from anywhere: .github/scripts/go-test-step-expansion.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# py <statements> [argument ...]: the statements with the module as `ex` and the arguments in
# sys.argv[1:]. The file's name holds dashes, which no import statement can spell, so it is
# loaded by path - the file a person runs, not a copy - with PYTHONPATH naming the directory
# beside it so its own `from ci_filter_coverage import …` resolves as it does for the script.
py() {
  local statements=$1
  shift
  PYTHONDONTWRITEBYTECODE=1 PYTHONPATH="$script_dir" python3 -c "import importlib.util, sys
spec = importlib.util.spec_from_file_location('expansion', '$script_dir/go-test-step-expansion.py')
ex = importlib.util.module_from_spec(spec)
# @dataclass reads sys.modules for the class's own module, so register it before it executes.
sys.modules['expansion'] = ex
spec.loader.exec_module(ex)
$statements" "$@"
}

# command_of <line>: the words of the go command the line runs, space-separated.
command_of() { py 'print(" ".join(ex.go_command(sys.argv[1])[0]))' "$1"; }

# text_of <line>: the command's text, which keeps the line's own quoting.
text_of() { py 'print(ex.go_command(sys.argv[1])[1])' "$1"; }

echo "case: a plain go test line is itself"
line='go test -count=1 ./internal/dispatch/docs/'
check "its words are the command's" "$(is "$(command_of "$line")" "go test -count=1 ./internal/dispatch/docs/")"
check "its text is the line" "$(is "$(text_of "$line")" "$line")"

echo "case: a variable set before the command (the finalizers lane)"
line='GODEBUG=checkfinalizers=1 go test -v -count=1 -timeout 29m ./internal/dispatch/docs/'
check "the variable is dropped" "$(is "$(command_of "$line")" "go test -v -count=1 -timeout 29m ./internal/dispatch/docs/")"
check "the text begins at go" "$(is "$(text_of "$line")" "go test -v -count=1 -timeout 29m ./internal/dispatch/docs/")"

echo "case: a variable and the memory wrapper (the race-docs lane)"
line='RACE_MEM_LIMIT_KB=8388608 ../../.github/scripts/race_memory.py go test -json -race -count=1 -timeout 60m ./internal/dispatch/docs/'
check "both are dropped" "$(is "$(command_of "$line")" "go test -json -race -count=1 -timeout 60m ./internal/dispatch/docs/")"

echo "case: more than one variable before the command"
check "each is dropped" "$(is "$(command_of 'A=1 B=2 go test ./pkg')" "go test ./pkg")"

echo "case: the wrapper's own path is read from its end, as a step writes it"
check "a deeper relative path is still the wrapper" \
  "$(is "$(command_of '../../../.github/scripts/race_memory.py go test ./pkg')" "go test ./pkg")"

echo "case: a -run pattern keeps its quoting"
line="GODEBUG=checkfinalizers=1 go test -run '^(TestOne|TestTwo)\$' ./pkg"
check "the text keeps the quotes the line wrote" \
  "$(is "$(text_of "$line")" "go test -run '^(TestOne|TestTwo)\$' ./pkg")"

echo "case: what it leaves for the caller to refuse"
check "a value needing the shell is left in place" \
  "$(is "$(command_of 'GOFLAGS=$EXTRA go test ./pkg')" 'GOFLAGS=$EXTRA go test ./pkg')"
check "a wrapper GO_WRAPPERS does not name is left in place" \
  "$(is "$(command_of 'timeout 60 go test ./pkg')" "timeout 60 go test ./pkg")"
check "a wrapper with no go command behind it is left in place" \
  "$(is "$(command_of '../../.github/scripts/race_memory.py --help')" "../../.github/scripts/race_memory.py --help")"
check "a bare assignment is left in place" "$(is "$(command_of 'NAME=value')" "NAME=value")"

echo "case: the two steps whose lines this harness exists for are read as ci.yaml writes them"
for file in .github/workflows/ci-job-envoy-go-race-docs.yaml .github/workflows/ci-job-envoy-go-docs-finalizers.yaml; do
  found=$(py 'import pathlib, yaml
document = yaml.safe_load(pathlib.Path(ex.ROOT / sys.argv[1]).read_text())
lines = [step["run"].strip()
         for job in document["jobs"].values() for step in job["steps"] if "run" in step]
print(sum(1 for line in lines
          for words, _ in [ex.go_command(line)]
          if len(words) > 1 and words[0] == "go" and words[1] == "test"))' "$file")
  check "$file holds a go test line this reader reads" "$(is "$found" "1")"
done

# go test runs a test whose name is not ASCII (TestÉclair, Test日本, TestᲉ): cmd/go asks only that
# the rune after `Test` is not lower case. The listing reader keeps a line as a name by NAME, so a
# NAME that judged the name's characters dropped such a test from what every step compiles and
# runs, and neither verdict could see it run twice or not at all. TestᲉ is U+1C89, a letter of
# Unicode 16.0, which Python 3.12's own tables (Unicode 15.0) do not know, so `\w` dropped it too.
echo "case: a listed name that is not ASCII is a name, and other output is not"
names=$(py 'for line in sys.argv[1:]:
    print(line, bool(ex.NAME.match(line)))' TestÉclair Test日本 TestᲉ FuzzÜber ExampleÅ 'TestÉclair passed' 'ok  pkg 0.1s')
check "TestÉclair, Test日本, TestᲉ, FuzzÜber and ExampleÅ are names; a sentence and a status line are not" "$(is "$names" \
  'TestÉclair True
Test日本 True
TestᲉ True
FuzzÜber True
ExampleÅ True
TestÉclair passed False
ok  pkg 0.1s False')"
# Printed joined rather than as a list: a list's repr escapes U+1C89, which Python's own tables
# call unprintable (`'Test\u1c89'`), for the same reason they did not call it a letter.
check "a -skip of such names is read, not refused" \
  "$(is "$(py 'print(" ".join(ex.skip_names(sys.argv[1]) or ["refused"]))' '^(TestÉclair|Test日本|TestᲉ)$')" "TestÉclair Test日本 TestᲉ")"
# A -skip name is the step's text, not go's output, so a character Go's regexp reads as an
# operator makes it no name: read as one, `Test.*` would be subtracted as a test of that name while
# go skipped every test it matches.
for pattern in '^(Test.*)$' '^(Test[A-Z])$' '^(TestA+)$' '^(Test\d)$'; do
  check "a -skip of $pattern is refused, not read as a name" \
    "$(is "$(py 'print(ex.skip_names(sys.argv[1]))' "$pattern")" "None")"
done

# The one `-skip` line of this repository is daemon-go's race step, and the rule expands
# packages/daemon now, so the space form below is reachable from a real step and the equals form
# from none, which is why this case holds both. The parse is what these cases hold: measured at
# b9e4127d0d78, `-skip '<pattern>'` left the pattern in the command's packages, where `go list`
# refuses it as a malformed import path and the whole run stops at exit 2 naming a `go list`
# failure rather than the flag; `-skip=<pattern>` was dropped whole, and the step was reported as
# running every test of its packages, the skipped ones included, which is the shape that makes the
# exactly-once rule wrong: a test that runs nowhere reads as covered, and one the step skips reads
# as a duplicate.
echo "case: a -skip is read rather than dropped, in either form a step may write it"
pattern='^(TestTheShippedWordsNameTheWaitingLineBand)$'
for form in "-skip '$pattern'" "-skip=$pattern"; do
  read -r in_packages retained <<<"$(py 'step = ex.Step(workflow="(a test case)", job="(a job)",
               name="(a step)", workdir="packages/envoy", script="")
text = f"go test -count=1 {sys.argv[1]} ./internal/textcheck/"
try:
    command = ex.parse_go_test(step, text, [])
except ex.Refused:
    # A refusal is no reading: it leaves the rule with nothing to say about the pattern, which is
    # the hole the two checks below are here to find. A refusal must therefore fail this case, so
    # print what neither check accepts rather than an answer one of them would take.
    print("refused refused")
else:
    # `text` is the line itself and holds the pattern whatever the parse made of it, so the
    # question is whether any field the parse writes holds it.
    parsed = {name: value for name, value in vars(command).items()
              if name not in ("step", "text", "packages", "resolved")}
    print("yes" if sys.argv[2] in command.packages else "no",
          "yes" if any(sys.argv[2] in str(value) for value in parsed.values()) else "no")' \
    "$form" "$pattern")"
  check "$form does not leave its pattern among the packages" "$(is "$in_packages" "no")"
  check "$form is kept, so the tests it skips can be subtracted" "$(is "$retained" "yes")"
done

# go test rewrites each part of a -skip before it matches (a space becomes _, a character Go does
# not print becomes its escape), so a -skip naming what no binary lists can skip a test of another
# name while a script subtracting by name subtracts nothing. U+200B (a zero-width space) is no
# whitespace to Python's \s, so the -skip shape reads `Test<U+200B>Alpha` as a name; the membership
# rule refuses it, and a plain name no package holds too. The listing is stood in for, so no Go is
# needed: the command lists TestAlpha, Test_Alpha and TestBeta.
echo "case: a -skip name no package of the command lists is refused, by name"
skip_run() { # skip_run <pattern>: what list_tests answers for a command skipping by <pattern>
  py 'step = ex.Step(workflow="(a test case)", job="(a job)", name="(a step)", workdir="packages/envoy", script="")
command = ex.GoTest(step=step, text="go test -skip PATTERN ./pkg", packages=["./pkg"], skip=sys.argv[1])
ex.listing = lambda command, pattern: {"example.com/pkg": ["TestAlpha", "Test_Alpha", "TestBeta"]}
try:
    run, _ = ex.list_tests(command)
except ex.Refused as refusal:
    print("refused:", refusal)
else:
    print("runs", " ".join(run["example.com/pkg"]))' "$1"
}
check "a listed name is subtracted" "$(is "$(skip_run '^(Test_Alpha)$')" "runs TestAlpha TestBeta")"
out=$(skip_run $'^(Test\xe2\x80\x8bAlpha|TestGamma)$')
check "names no package lists are refused" "$(contains "$out" '^refused: ')"
check "  naming each" "$(contains "$out" "skips 'TestGamma', 'Test\\\\u200bAlpha', which no package of it lists")"

# -list matches each top-level name against the whole pattern; go test -run (and -skip) first
# splits it at every / and every | outside brackets and parentheses, then rewrites each part (a
# space becomes _, a rune Go does not print becomes its escape). A step pattern the two read
# differently would have this script, which learns what a step runs from -list, wrong about it, so
# such a pattern is refused by name: a split at the parse, a rewritten rune once the listing is
# known.
echo "case: a -run or -skip that -list and -run split differently is refused at the parse"
parsed() { # parsed <line>: "read" or "refused: <why>" for one go test line
  py 'step = ex.Step(workflow="(a test case)", job="(a job)", name="(a step)", workdir="packages/envoy", script="")
try:
    ex.parse_go_test(step, sys.argv[1], [])
except ex.Refused as refusal:
    print("refused:", refusal)
else:
    print("read")' "$1"
}
out=$(parsed "go test -run '^A\$|^B\$/x' ./pkg")
check "a / is refused" "$(contains "$out" "runs by '^A\\\$|^B\\\$/x', and it holds a \`/\`")"
out=$(parsed "go test -run '^TestBeta\$|^\\QTestAlpha|\\Qjunk\\E\$' ./pkg")
# shellcheck disable=SC2016 # literal backticks of the message, no expansion wanted
check "a top-level | around a \\Q quote is refused" "$(contains "$out" 'and it holds a `|` outside brackets and parentheses')"
for line in "go test -run '^Test[|]\$' ./pkg" "go test -run '^TestA\\|B\$' ./pkg"; do
  check "read: $line" "$(is "$(parsed "$line")" read)"
done

# Which runes the rewrite changes is Go's to say, not Python's. U+1C89 is a letter of Unicode 16.0,
# which Go 1.27 prints and Python 3.12's tables call unprintable; U+200B, a zero-width space, is a
# format character Go does not print; U+00A0, U+3000 and the space are spaces to the rewrite. A -run
# of names the binaries listed is read on that membership; any other is asked of the probe, built
# with the toolchain packages/envoy selects. The listing is stood in for: the command lists the
# names below, the five-name set the review's generated pattern held among them.
echo "case: a -run holding a rune go test -run rewrites is refused, as Go decides it"
listed_run() { # listed_run <run> [<skip>]: what list_tests answers for a command with that -run
  py 'step = ex.Step(workflow="(a test case)", job="(a job)", name="(a step)", workdir="packages/envoy", script="")
command = ex.GoTest(step=step, text="go test -run PATTERN ./pkg", packages=["./pkg"], run=sys.argv[1], skip=sys.argv[2])
names = ["TestAlpha", "Test_Alpha", "TestBeta", "FuzzAirlock", "TestÉclair", "Test日本",
         "Test\u1c89", "TestOne", "TestTwo", "TestThree", "TestFour"]
ex.listing = lambda command, pattern: {"example.com/pkg": names}
try:
    ex.list_tests(command)
except ex.Refused as refusal:
    print("refused:", refusal)
else:
    print("read")' "$1" "${2:-}"
}
out=$(listed_run $'^(Test\xc2\xa0Alpha)$')
check "a no-break space is refused, named by its code point" "$(contains "$out" 'and it holds U+00A0, which go test -run rewrites')"
out=$(listed_run 'Test Alpha')
check "a plain space is refused" "$(contains "$out" 'and it holds U+0020, which go test -run rewrites')"
out=$(listed_run $'Test\xe2\x80\x8bAlpha')
check "a rune Go does not print (U+200B) is refused" "$(contains "$out" 'and it holds U+200B, which go test -run rewrites')"
out=$(listed_run $'^(Test\xe3\x80\x80Alpha)$')
check "an ideographic space (U+3000) is refused" "$(contains "$out" 'and it holds U+3000, which go test -run rewrites')"
# A -skip is refused at the parse unless it is ^(Name|...)$ of names with no space, and once listed
# each name must be one a package lists, so its runes need no probe.
out=$(parsed $'go test -skip \'^(Test\xc2\xa0Alpha)$\' ./pkg')
check "a no-break space in a -skip is refused" "$(contains "$out" 'reads only the anchored alternation of whole test names')"
for run in 'FuzzAirlock' '^(TestAlpha|TestBeta)$' 'TestÉclair' $'^(Test\xe1\xb2\x89)$' \
  $'^(TestFour|TestOne|TestThree|TestTwo|Test\xe1\xb2\x89)$' $'Test\xe1\xb2\x89' $'^Test\xe1\xb2\x89$'; do
  check "read: -run $run" "$(is "$(listed_run "$run")" read)"
done
check "read: a -skip of listed names, one of them U+1C89" "$(is "$(listed_run 'TestAlpha' $'^(Test\xe1\xb2\x89|Test\xe6\x97\xa5\xe6\x9c\xac)$')" read)"

# The probe's isSpace is a copy of testing/match.go's; held here to the match.go of the toolchain
# packages/envoy selects, so a Go release that changes the list fails this case rather than
# leaving the copy stale. Compared with comments and blank lines dropped, line by line.
echo "case: the rewrite probe's isSpace is the pinned toolchain's own"
goroot=$(cd "$script_dir/../../packages/envoy" && go env GOROOT)
body_of() { sed -n '/^func isSpace(r rune) bool {$/,/^}$/p' | sed -e 's/[[:space:]]*\/\/.*$//' -e 's/^[[:space:]]*//' | grep -v '^$'; }
theirs=$(body_of <"$goroot/src/testing/match.go")
ours=$(py 'print(ex.REWRITE_PROBE)' | body_of)
check "match.go has the function" "$(is "$([ -n "$theirs" ] && echo found)" found)"
check "the probe's copy is the same, line for line" "$(is "$ours" "$theirs")"

# envoy-go-race-api is a matrix whose legs each run one shard of internal/dispatch/api, and the
# leg's index and the leg count reach its script as `${{ strategy.job-index }}` and
# `${{ strategy.job-total }}`. Read as one step, the script holds two expressions no shell can run
# and the assignment calls a script the reader refused; so each leg is read as a step of its own,
# with GitHub's values in its text, and a test two legs run is a pair two steps run.
echo "case: a matrix of one key is read once per leg, with that leg's values"
legs=$(py 'body = {"strategy": {"fail-fast": False, "matrix": {"shard": [1, 2, 3]}}}
for suffix, values in ex.legs("(a job)", body):
    print(suffix.strip(), ex.substitute(sys.argv[1], values))' \
  'run ${{ strategy.job-index }} of ${{strategy.job-total}} as shard ${{ matrix.shard }}, at ${{ runner.temp }}')
check "three legs, each its index, the count and its value; another expression is left as written" "$(is "$legs" \
  '(leg 1 of 3, shard 1) run 0 of 3 as shard 1, at ${{ runner.temp }}
(leg 2 of 3, shard 2) run 1 of 3 as shard 2, at ${{ runner.temp }}
(leg 3 of 3, shard 3) run 2 of 3 as shard 3, at ${{ runner.temp }}')"
check "a job with no matrix is one leg, its text unchanged" \
  "$(is "$(py 'print([(s, ex.substitute("x ${{ matrix.shard }}", v)) for s, v in ex.legs("(a job)", {})])')" \
    "[('', 'x \${{ matrix.shard }}')]")"
for shape in '{"shard": [1, 2], "os": ["a"]}' '{"include": [{"shard": 1}]}' '{"shard": "${{ fromJSON(x) }}"}' '{"shard": []}'; do
  refused=$(py 'import json
try:
    ex.legs("(a job)", {"strategy": {"matrix": json.loads(sys.argv[1])}})
except ex.Refused:
    print("refused")
else:
    print("read")' "$shape")
  check "the matrix $shape is refused, not guessed at" "$(is "$refused" "refused")"
done

echo "case: envoy-go-race-api's step is read once per leg, each assigning its -run from the shard script"
api=$(py 'import yaml
path = ".github/workflows/ci-job-envoy-go-race-api.yaml"
found = {path: yaml.safe_load((ex.ROOT / path).read_text())}
steps = ex.steps_of(found)
print(len(steps))
for step in steps:
    first = step.script.strip().splitlines()[0]
    assignment = ex.ASSIGNMENT.match(first)
    print(step.name, "|", bool(assignment) and any(lister in assignment.group(2) for lister in ex.RUN_LISTERS),
          "|", "${{" in step.script)')
check "one step per leg of the matrix" "$(is "$(head -n 1 <<<"$api")" "$(py 'import yaml
document = yaml.safe_load((ex.ROOT / ".github/workflows/ci-job-envoy-go-race-api.yaml").read_text())
print(len(document["jobs"]["envoy-go-race-api"]["strategy"]["matrix"]["shard"]))')")"
check "each leg's -run comes from a script the reader runs, and no expression is left in its text" \
  "$(is "$(tail -n +2 <<<"$api" | cut -d '|' -f 2- | sort -u)" " True | False")"
# go() and shell() run their command under ci_filter_coverage.go_environment(): none of the
# caller's Go variables but those naming where Go keeps or fetches files, and GOENV=off, so a
# caller's GOFLAGS or GOOS, or the GOROOT a version manager's python3 shim hands the interpreter,
# lists nothing a CI step would not. A stand-in go that prints its own environment shows what
# reaches a go command, read here with a pattern of this harness's own for the variables the go
# command reads. The caller below sets each kind of them, the kinds that changed
# check-ci-filters.sh's answer (its test's case on a caller's Go variables). A -race command gets
# cgo on whatever the caller says, since a race build needs it.
echo "case: a go command and a step's line see none of the caller's Go variables"
work=$(mktemp -d)
# proxy is the stalled proxy a case below runs while it runs, which the trap stops if the run ends.
proxy=""
trap 'if [ -n "$proxy" ]; then kill "$proxy" 2>/dev/null; fi; rm -rf "$work"' EXIT
printf '#!/bin/sh\nenv\n' > "$work/go"
chmod +x "$work/go"
# go_seen <go|shell> <directory> <command> [argument ...]: the go command's variables, sorted, that
# the command sees when go() or shell() runs it in the directory.
go_seen() {
  py 'import re
from pathlib import Path
how, directory, *command = sys.argv[1:]
if how == "go":
    out = ex.go(command, Path(directory), "the stand-in go")
else:
    out = ex.shell(" ".join(command), Path(directory), "the stand-in go")
names = re.compile(r"(C?GO[A-Z0-9_]*|CC|CXX|AR|FC|GCCGO|PKG_CONFIG)=")
print("\n".join(sorted(line for line in out.splitlines() if names.match(line))))' "$@"
}
# polluted <command> [argument ...]: the command (a function of this harness too) under a caller who
# sets one variable of each kind.
polluted() {
  GOROOT="$work" GOTOOLCHAIN=local GOFLAGS=-tags=polluted GOWORK=off GOOS=plan9 CGO_ENABLED=0 \
    GOEXPERIMENT=pollutedexperiment GO111MODULE=off GOENV="$work/env" CC="$work/no-such-cc" "$@"
}
clean_go=$(go_seen go "$work" "$work/go")
check "a go command runs with no go env file and no GOROOT" \
  "$(is "$(contains "$clean_go" '^GOENV=off$'):$(contains "$clean_go" '^GOROOT=')" "true:false")"
check "and sees the same variables under the caller's" \
  "$(is "$(polluted go_seen go "$work" "$work/go")" "$clean_go")"
clean_line=$(go_seen shell "$work" "$work/go")
check "a step's line runs with no go env file" "$(contains "$clean_line" '^GOENV=off$')"
check "and sees the same variables under the caller's" \
  "$(is "$(polluted go_seen shell "$work" "$work/go")" "$clean_line")"
check "a -race go command runs with cgo on under a caller's CGO_ENABLED=0" \
  "$(contains "$(polluted go_seen go "$work" "$work/go" test -race)" '^CGO_ENABLED=1$')"

# The rewrite probe runs the selected toolchain's own go, under go_environment() and that
# toolchain's GOROOT. A caller's GOROOT of another Go - the one a version manager's python3 shim
# hands the interpreter - reaching that go made it refuse its own compiler (`compile: version
# "go1.27.0" does not match go tool version "go1.27.3"`), and a caller's GOOS built a probe it could
# not run. The caller here sets every kind of Go variable `polluted` sets, GOROOT among them, for a
# directory whose go is a stand-in that only prints its environment; the probe must still answer,
# rune for rune, as it does for a clean caller.
echo "case: the rewrite probe answers under a caller's GOROOT of another Go"
for run in $'Test\xe1\xb2\x89' 'TestÉclair'; do
  check "read, under the polluted caller: -run $run" "$(is "$(polluted listed_run "$run")" read)"
done
out=$(polluted listed_run $'Test\xe2\x80\x8bAlpha')
check "U+200B still refused, under the polluted caller" "$(contains "$out" 'and it holds U+200B, which go test -run rewrites')"
check "  and the probe did not fail" "$(is "$(contains "$out" 'the rewrite probe exited')" false)"

# go_root asks the go first on PATH for its GOROOT under go_environment(), which drops setup-go's
# GOTOOLCHAIN=local with the caller's Go variables, so go picks its toolchain by GOTOOLCHAIN's own
# rules: where go.work asks for a go newer than the installed one, by its go line or by its
# toolchain line, `go env GOROOT` downloads that toolchain. (A module's own go or toolchain line
# does not: measured with Go 1.27.3, only go.work's two lines switch the toolchain in a workspace.)
# The test job runs these cases on a cold module cache, so such a download would happen in the
# step, and if it stalls the step's log must name it while it waits. The proxy here accepts every
# connection and answers none, as a stalled download does. go1.999.0 is a version no cache holds.
echo "case: a toolchain download that go env GOROOT makes is on standard error while go waits"
# stop_proxy: stops the stalled proxy, which fails a go waiting on it.
stop_proxy() {
  kill "$proxy"
  wait "$proxy" || true
  proxy=""
}
# root_answer <module directory> <output directory> [<module cache>]: go_root over the module
# directory in the background, under the stalled proxy and the module cache given (the caller's
# when none is). What it returns, or `refused: ...`, goes to <output directory>/answer, and go's
# standard error to <output directory>/err. Sets asking to its process.
root_answer() {
  local module=$1 output=$2
  (
    export GOPROXY="http://127.0.0.1:$port"
    if [ -n "${3:-}" ]; then export GOMODCACHE=$3; fi
    py 'from pathlib import Path
try:
    print(ex.go_root(Path(sys.argv[1])))
except ex.Refused as refusal:
    print("refused:", refusal)' "$module" >"$output/answer" 2>"$output/err"
  ) &
  asking=$!
}
for what in toolchain go; do
  fixture=$work/asks-by-$what
  mkdir -p "$fixture/mod" "$fixture/modcache"
  printf 'module example.com/mod\n\ngo 1.21\n' >"$fixture/mod/go.mod"
  if [ "$what" = toolchain ]; then asked=$'go 1.21\ntoolchain go1.999.0'; else asked='go 1.999.0'; fi
  printf '%s\n\nuse ./mod\n' "$asked" >"$fixture/go.work"
  stalled_proxy "$fixture"
  root_answer "$fixture/mod" "$fixture" "$fixture/modcache"
  # The wait ends on one of two facts: go has reached the proxy, which it does only once it has
  # written its downloading line (cmd/go's modfetch.DownloadZip writes the line, then asks the
  # proxy), or go_root has ended without reaching it, so the line can no longer come while go
  # waits. Reached, go waits on a proxy that never answers, so go_root still runs.
  await_file "$fixture/accepted" "$asking"
  seen=$(grep -q '^go: downloading go1\.999\.0 (' "$fixture/err" && echo true || echo false)
  running=$(kill -0 "$asking" 2>/dev/null && echo true || echo false)
  stop_proxy
  wait "$asking" || true
  check "go.work's $what line asks for go1.999.0: go reaches the proxy with its downloading line already on standard error, while go waits" \
    "$(is "$came/$seen/$running" file/true/true)"
  check "  then go_root refuses, saying go's standard error is above, and copies none of it" \
    "$(is "$(contains "$(<"$fixture/answer")" '^refused: go env GOROOT in .* exited 1; its standard error is above$')/$(grep -c 'go: ' "$fixture/answer" || true)" true/0)"
done

# The pair this repository has: go.work's go 1.27.3 and no toolchain line, and in the test job the
# go 1.27.3 its setup-go installs from packages/daemon/go.mod. go env GOROOT there switches to no
# other toolchain, so it answers under a proxy that never answers, and writes nothing to standard
# error. On a machine whose go is older, the rune cases above have fetched go1.27.3 into its module
# cache already, and this case finds it there.
echo "case: go.work's own lines make go env GOROOT download nothing"
current=$work/current
mkdir "$current"
stalled_proxy "$current"
root_answer "$script_dir/../../packages/envoy" "$current"
# The wait ends when go_root has answered, or at once when go reaches the proxy, which is a
# download: neither needs a clock.
await_file "$current/accepted" "$asking"
stop_proxy
wait "$asking" || true
answer=$(<"$current/answer")
check "it answers under the stalled proxy, reaching it not at all, with the root of a go" \
  "$(is "$came/$([ -x "$answer/bin/go" ] && echo go || echo "$answer")" end/go)"
check "  and writes nothing to standard error" "$(is "$(<"$current/err")" "")"
# AUDITED_SCRIPTS holds a person's reading of each script, pinned by its SHA-256, and the script
# refuses a step whose script's text has moved on. The script runs by hand, so nothing else tells
# the pull request that edits an audited script: #2856 changed database-skips.sh's
# EXPECTED_SKIPS, the pin went stale, and the next by-hand run ended at exit 2 with no verdict.
# ci.yaml's go_test_step_expansion_test filter names each audited script, so this case runs on
# that pull request. It also holds each audited script to the reads ci_job_reads.py declares for
# this harness's gate, which ci-filters holds to that filter, so a new AUDITED_SCRIPTS entry fails
# here until its script selects this harness. Both lists are read into variables first: a
# failed read then stops the harness under `set -e`, where a process substitution's failure
# would leave the loop with nothing to check.
echo "case: each script AUDITED_SCRIPTS reads still has the text its entry was read from"
repo=$(cd "$script_dir/../.." && pwd)
pins=$(py 'for path, audited in ex.AUDITED_SCRIPTS.items(): print(path + "\t" + audited.sha256)')
check "AUDITED_SCRIPTS names at least one script" "$(is "$([ -n "$pins" ] && echo yes || echo no)" yes)"
declared=$(cd "$repo" && py 'import ci_job_reads
print("\n".join(read.glob for gate in ci_job_reads.SCRIPT_TEST_GATES
                if gate.filter == "go_test_step_expansion_test" for read in gate.reads))')
while IFS=$'\t' read -r path pinned; do
  actual=$(sha256sum "$repo/$path" | cut -d' ' -f1)
  check "$path has its entry's SHA-256 (read the script again, then update its AuditedScript entry in go-test-step-expansion.py)" \
    "$(is "$actual" "$pinned")"
  check "$path is among the reads ci_job_reads.py declares for the go_test_step_expansion_test gate (add it there and to that ci.yaml filter)" \
    "$(is "$(grep -Fxq -- "$path" <<<"$declared" && echo yes || echo no)" yes)"
done <<<"$pins"

summary "go-test-step-expansion.py reads a go command behind the variables and wrapper a step writes"
