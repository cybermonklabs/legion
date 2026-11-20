#!/usr/bin/env bash
# Tests for `go-test-step-expansion.py`'s reading of one line of a step's script: which command a
# line runs, and which shapes it refuses rather than guesses at; that its step reader reads every
# `go test` step of this head's workflows; and that each script AUDITED_SCRIPTS reads still has
# the SHA-256 its entry pins.
#
# Only the reader and the pins. What the script makes of a command - the packages a
# `go test -list` names, the pairs two steps share, the exceptions - needs all three Go modules'
# test binaries built, which is minutes of compile and why no CI job runs that script (its own
# header says so). This harness needs no Go at all: every case is text, read as the script reads
# it, or a file's hash.
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
repo=$(cd "$script_dir/../.." && pwd)

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

# Two steps run database-skips.sh, each with the argument list its entry reads: envoy-go's count,
# which runs the broker's packages less the race lane's, and its command, and envoy-go-race-api's
# race step, which runs the API package. Each step is read out of the file that holds it, in the
# directory GitHub runs it from, so the case holds the entry to the steps as written; an argument
# list the entry does not list is refused, since what the script runs then is a reading nobody
# made.
echo "case: a step that runs an audited script is read through its entry, by its argument list"
read_audited() { py 'import pathlib, yaml
document = yaml.safe_load(pathlib.Path(ex.ROOT / sys.argv[1]).read_text())
found = [(job, step) for job in document["jobs"].values() for step in job["steps"]
         if step.get("name") == sys.argv[2]]
job, step = found[0]
try:
    read = ex.audited_step(sys.argv[1], "(a job)", sys.argv[2], ex.step_workdir(document, job, step),
                           step["run"] + sys.argv[3])
except ex.Refused:
    print("refused")
else:
    print(read.workdir, read.script)' "$@"; }
check "envoy-go's count runs the broker's packages less the race lane's, and its command, less the backend count's test" \
  "$(is "$(read_audited .github/workflows/ci-job-envoy-go.yaml "Count the database broker suites' skips" "")" \
    "packages/envoy go list ./internal/broker/... ./cmd/broker/ | grep -Ev '/internal/broker/natscallout$' | xargs go test -json -count=1 -skip '^(TestSettingsNameTheGoogleExchangeAudienceOnEitherStore)\$' -timeout 20m")"
check "envoy-go-race-api's race step runs the API package under -race through the counter" \
  "$(is "$(read_audited .github/workflows/ci-job-envoy-go-race-api.yaml "Race-test the Dispatch API" "")" \
    "packages/envoy go test -json -count=1 -race -timeout 70m ./internal/dispatch/api/")"
check "the race step with one more argument, which the entry does not list, is refused" \
  "$(is "$(read_audited .github/workflows/ci-job-envoy-go-race-api.yaml "Race-test the Dispatch API" " --race")" "refused")"

# envoy-go's "Count the store-form rows on each backend" runs backend-count.sh from the
# repository root and holds no `go test` of its own, so until AUDITED_SCRIPTS read that script
# the reader skipped the step, and the second run of its five tests (both packages also run whole
# in other steps of the job) was in neither verdict: O15 of dispatch://INFRA-1408. The step is
# read here as the verdicts read it, out of every step of this head. The count runs them in the
# same job, env and build as the steps that run their packages whole, so those steps skip them
# and the count is their one run: the second check holds each to the one step that skips it, read
# as the first case below reads, with no command run.
echo "case: envoy-go's backend count is read as the go test its five tests run in"
check "the step runs its five tests in its two packages, from packages/envoy" \
  "$(is "$(py 'found = [step for step in ex.steps_of(ex.documents()) if step.key ==
         (".github/workflows/ci-job-envoy-go.yaml", "Count the store-form rows on each backend")]
if not found:
    print("not read")
else:
    command, = ex.commands_of(found[0])
    print(command.step.workdir, " ".join(command.packages), command.run)')" \
    "packages/envoy ./cmd/agent-secrets/ ./internal/broker/api/ ^(TestEverySecretFormNamesTheSourceItUses|TestNoSecretFormAsksTheBrokerForACredential|TestUseCommandOutsideASessionReachesTheMachineSource|TestTheChainSkipsAbsenceAndStopsOnABrokenSource|TestSettingsNameTheGoogleExchangeAudienceOnEitherStore)\$")"
check "the unit step skips the four of cmd/agent-secrets and the database-skips step the one of internal/broker/api" \
  "$(is "$(py 'ex.shell = lambda script, cwd, what: "(not-run)"
steps = {step.key: step for step in ex.steps_of(ex.documents())}
skipping = {name: {test for command in ex.commands_of(steps[(".github/workflows/ci-job-envoy-go.yaml", name)])
                   for test in ex.skip_names(command.skip) or ()}
            for name in sys.argv[1:]}
print(" ".join(test + ":" + ",".join(name for name, tests in skipping.items() if test in tests)
               for test in ex.BACKEND_COUNT_TESTS))' "Unit test envoy Go package" "Count the database broker suites' skips")" \
    "TestEverySecretFormNamesTheSourceItUses:Unit test envoy Go package TestNoSecretFormAsksTheBrokerForACredential:Unit test envoy Go package TestUseCommandOutsideASessionReachesTheMachineSource:Unit test envoy Go package TestTheChainSkipsAbsenceAndStopsOnABrokenSource:Unit test envoy Go package TestSettingsNameTheGoogleExchangeAudienceOnEitherStore:Count the database broker suites' skips")"

# The script stops at exit 2 on the first step it cannot read, and it runs by hand, so nothing
# told the pull request that made a step unreadable: #4319 moved the skips step behind
# database-skips.sh, #2856 edited that script past its pin, and each time the next by-hand run
# ended with no verdict. This case reads every go test step of this head's ci.yaml and the
# workflows its jobs call, as expansion() does, and fails naming each step the reader refuses and
# why. It runs no command: `go`, which only the listings call, is replaced by a failure, and
# `shell`, which runs a step's `go list … | xargs` lister and its go-tagged-tests.sh call, by a
# stand-in word, since what those print is the go command's answer and not the step's text. Two
# fixtures, each a step the reader must refuse, show the case can fail. The last case holds each
# workflow the reader reads to this gate's declared reads, so the pull request that edits one
# runs this case.
echo "case: the step reader refuses no go test step of this head"
# reader_refusals <root>: each refusal of the reader over the workflows under root, one per line.
reader_refusals() { py 'import pathlib
ex.ROOT = pathlib.Path(sys.argv[1])
def no_go(args, cwd, what):
    raise AssertionError(f"{what}: the reader ran {args}")
ex.go = no_go
ex.shell = lambda script, cwd, what: "(not-run)"
try:
    steps = ex.steps_of(ex.documents())
except ex.Refused as refusal:
    print(refusal)
    raise SystemExit
if not steps:
    print("no go test step")
for step in steps:
    try:
        ex.commands_of(step)
    except ex.Refused as refusal:
        print(refusal)' "$1"; }
refusals=$(reader_refusals "$repo")
check "every go test step is read${refusals:+; refused: $refusals}" "$(is "$refusals" "")"
fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT
mkdir -p "$fixture/.github/workflows" "$fixture/packages/envoy/scripts"
printf 'jobs:\n  envoy-go:\n    uses: ./.github/workflows/ci-job-envoy-go.yaml\n' >"$fixture/.github/workflows/ci.yaml"
cat >"$fixture/.github/workflows/ci-job-envoy-go.yaml" <<'EOF'
jobs:
  envoy-go:
    steps:
      - name: Count the database broker suites' skips
        run: packages/envoy/scripts/database-skips.sh --timeout 20m --skip '^(TestSettingsNameTheGoogleExchangeAudienceOnEitherStore)$'
EOF
skips=packages/envoy/scripts/database-skips.sh
{ cat "$repo/$skips"; echo '# an edit after the reading its pin is of'; } >"$fixture/$skips"
check "a step whose script has moved past its pin is refused, by step and reason" \
  "$(contains "$(reader_refusals "$fixture")" "^\.github/workflows/ci-job-envoy-go\.yaml's envoy-go / Count the database broker suites' skips: $skips has SHA-256 ")"
cat >"$fixture/.github/workflows/ci-job-envoy-go.yaml" <<'EOF'
jobs:
  envoy-go:
    steps:
      - name: Time-box the tests
        run: timeout 60 go test ./internal/bus/
        working-directory: packages/envoy
EOF
check "a line the reader cannot read is refused, by step and line" \
  "$(is "$(reader_refusals "$fixture")" ".github/workflows/ci-job-envoy-go.yaml's envoy-go / Time-box the tests: this script cannot read \`timeout 60 go test ./internal/bus/\`")"

# AUDITED_SCRIPTS holds a person's reading of each script, pinned by its SHA-256, and the script
# refuses a step whose script's text has moved on. The script runs by hand, so nothing else tells
# the pull request that edits an audited script: #2856 changed database-skips.sh's
# EXPECTED_SKIPS, the pin went stale, and the next by-hand run ended at exit 2 with no verdict.
# ci.yaml's go_test_step_expansion_test filter names each audited script, so this case runs on
# that pull request. It also holds each audited script, and each workflow the step reader reads,
# to the reads ci_job_reads.py declares for this harness's gate, which ci-filters holds to that
# filter, so a new AUDITED_SCRIPTS entry or called workflow fails here until its file selects this
# harness. Both lists are read into variables first: a failed read then stops the harness under
# `set -e`, where a process substitution's failure would leave the loop with nothing to check.
echo "case: each script AUDITED_SCRIPTS reads still has the text its entry was read from"
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
unread=$(py 'import workflow_paths
globs = sys.argv[1].splitlines()
print(" ".join(path for path in ex.documents()
               if not any(workflow_paths.glob_regex(glob).fullmatch(path) for glob in globs)))' "$declared")
check "each workflow the step reader reads is among those reads${unread:+ (add it there and to that ci.yaml filter): $unread}" \
  "$(is "$unread" "")"

summary "go-test-step-expansion.py reads a go command behind the variables and wrapper a step writes"
