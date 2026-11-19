#!/usr/bin/env bash
# Tests for `go-test-step-expansion.py`'s reading of one line of a step's script: which command a
# line runs, and which shapes it refuses rather than guesses at; and that each script
# AUDITED_SCRIPTS reads still has the SHA-256 its entry pins.
#
# Only the line reader and the pins. What the script makes of a command - the packages a
# `go test -list` names, the pairs two steps share, the exceptions - needs all three Go modules'
# test binaries built, which is minutes of compile and why no CI job runs that script (its own
# header says so). This harness needs no Go at all: every case is one line of text or a file's
# hash, but one, which runs a stand-in go that prints the environment the module's go commands and
# step lines get.
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
trap 'rm -rf "$work"' EXIT
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
