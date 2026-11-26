#!/usr/bin/env bash
# .github/scripts/go-test.sh
#
# Runs one `go test` command of a CI job under gotestsum, the way every Go test step of a job
# ci-ok needs runs its tests, so the log names each test's result, a jsonfile of the run is kept
# for the job's upload, and a merge group runs a failed test once more:
#
#   go-test.sh [--race-memory] go [-C DIR] test [GO TEST ARGUMENT]...
#
# The words after the options are the go test command the step ran before this script, as they
# stand, so every reader of a step's go command (check-ci-filters.sh's reducer, go_test_steps.py,
# go-test-step-expansion.py) reads it with this script's path and options taken off the front, as
# it reads `race_memory.py go test …`. They run as
#
#   gotestsum --format standard-verbose --jsonfile JSONFILE [RETRY] --packages "PACKAGES" -- FLAGS
#
# from DIR where the command names `go -C DIR`, else from the caller's directory. PACKAGES are the
# command's package arguments and FLAGS every other argument, in their order, split by go test's
# own rule: an argument that starts with `-` is a flag, one of the flags GO_VALUE_FLAGS names
# takes the next argument as its value when written without `=`, `-args` hands every later word
# to the test binary, and any other argument is a package. gotestsum takes the packages through
# --packages and nowhere else, since it refuses its retry beside packages written after `--`
# (cmd/main.go, Validate). FLAGS carry no -v and no -json: gotestsum runs `go test -json`, which
# runs the test binary with -test.v=test2json, and a -v beside it passes -test.v=true after that
# and takes test2json's framing away. The standard-verbose format prints each test's `=== RUN`
# and `--- PASS`, `--- FAIL` or `--- SKIP` line, and gotestsum's summary after them.
#
# RETRY is `--rerun-fails=1 --rerun-fails-run-root-test --rerun-fails-abort-on-data-race
# --rerun-fails-report <JSONFILE less .json>.rerun-fails.txt`, with
# `--rerun-fails-max-failures=N` where GO_TEST_RERUN_MAX_FAILURES is N. It is on when
# GITHUB_EVENT_NAME is merge_group and off on every other event: a test that fails runs once more,
# from its root test, and passing then passes the step, which ci-ok names as a test that passed
# only on its retry (.github/scripts/ci-ok-retries.sh). gotestsum reruns nothing after a data
# race, a panic (a test that passes its -timeout panics), a build failure, more than N failed root
# tests (10 by default), a go test exit above 1, or a line on go test's standard error that does
# not start with `# `, `go: copying`, `go: creating`, `go: downloading`, `go: extracting`,
# `go: finding`, `HASH`, `testcache:` or `warning:` (cmd/rerunfails.go, hasErrors;
# testjson/execution.go, readStderr): its summary lists those lines under `=== Errors`.
# GO_TEST_RETRY=off turns the retry off on any event, for a run whose expected result is a failure
# (each run of packages/envoy/scripts/airlock-mutants.sh against a mutant) or whose -run a rerun of
# the root test would drop (kind's runtime legs); GO_TEST_RETRY=on turns it on on any event, for a
# workflow_dispatch run that proves the retry as a merge group would. These are the cases the
# paragraph "The Go retry follows R22" of dispatch://INFRA-3411/artifact/decisions-md lists.
#
# --race-memory runs gotestsum under .github/scripts/race_memory.py, which samples the memory of
# the process tree every 5 s, names the tests running at each sample from the jsonfile as it
# grows, and fails the step when the peak passes RACE_MEM_LIMIT_KB although every test passed.
#
# JSONFILE is GO_TEST_JSONFILE where a script that reads it afterwards names it, else
# REPORTS/<GITHUB_JOB>-<n>.json, n the next number free there, so the steps of one job never
# share a file. REPORTS is GO_TEST_REPORTS, else $RUNNER_TEMP/go-test-reports on a runner, else
# ${TMPDIR:-/tmp}/go-test-reports. The jsonfile and the rerun report stay there for the job's
# last step, .github/actions/go-test-upload, which uploads them for ci-ok.
#
# After gotestsum ends it reads the jsonfile (.github/scripts/go-test.jq, no_test_failures) and
# fails the step on a package that failed in an attempt with no test failing, none left running
# and no build failure: a TestMain check after its tests pass, or a wrapper's own exit. gotestsum
# reruns such a package with -test.run=^$, which runs no test, so a check that needs the tests to
# have run passes there and gotestsum exits 0.
#
# It exits with gotestsum's status, or 1 where that is 0 and the jsonfile shows such a package
# or is missing, and 2 on a usage error.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
reports=${GO_TEST_REPORTS:-${RUNNER_TEMP:-${TMPDIR:-/tmp}}/go-test-reports}

# The go build and test flags that take a value as the next argument when written without `=`
# (`go help build`, `go help testflag`). Every other flag is a boolean, or written `-flag=value`.
GO_VALUE_FLAGS=" -asmflags -bench -benchtime -blockprofile -blockprofilerate -buildmode -compiler
  -count -covermode -coverpkg -coverprofile -cpu -cpuprofile -exec -fuzz -fuzzcachedir
  -fuzzminimizetime -fuzztime -gccgoflags -gcflags -installsuffix -ldflags -list -memprofile
  -memprofilerate -mod -modfile -mutexprofile -mutexprofilefraction -o -outputdir -overlay -p
  -parallel -pgo -pkgdir -run -shuffle -skip -tags -timeout -toolexec -trace -vet "
GO_VALUE_FLAGS=${GO_VALUE_FLAGS//$'\n'/ }

usage() {
  echo "go-test.sh: $1 (usage: go-test.sh [--race-memory] go [-C DIR] test [GO TEST ARGUMENT]...)" >&2
  exit 2
}

race_memory=false
while [ $# -gt 0 ]; do
  case "$1" in
    --race-memory) race_memory=true; shift ;;
    go) break ;;
    *) usage "unknown argument $1 before the go command" ;;
  esac
done
[ "${1:-}" = go ] || usage "no go command is given"
shift
dir=""
if [ "${1:-}" = -C ]; then
  [ -n "${2:-}" ] || usage "go -C names no directory"
  dir=$2
  shift 2
fi
[ "${1:-}" = test ] || usage "the command is go ${1:-}, not go test"
shift

packages=()
flags=()
while [ $# -gt 0 ]; do
  word=$1
  shift
  case "$word" in
    -args | --args)
      flags+=("$word" "$@")
      break
      ;;
    -v | -v=* | --v | --v=* | -test.v | -test.v=* | --test.v | --test.v=*)
      usage "$word takes test2json's framing away: gotestsum's go test -json already runs the test binary verbose"
      ;;
    -json | -json=* | --json | --json=*)
      usage "$word is gotestsum's to pass"
      ;;
    -*=*) flags+=("$word") ;;
    -*)
      flags+=("$word")
      if [[ "$GO_VALUE_FLAGS" == *" -${word#"${word%%[!-]*}"} "* ]]; then
        [ $# -gt 0 ] || usage "$word takes a value"
        flags+=("$1")
        shift
      fi
      ;;
    *) packages+=("$word") ;;
  esac
done
[ "${#packages[@]}" -gt 0 ] || usage "the go test command names no package"

retry=false
case "${GO_TEST_RETRY:-}" in
  "") [ "${GITHUB_EVENT_NAME:-}" = merge_group ] && retry=true ;;
  on) retry=true ;;
  off) ;;
  *) usage "GO_TEST_RETRY is \"$GO_TEST_RETRY\": on, off, or unset for on in a merge group alone" ;;
esac
max_failures=${GO_TEST_RERUN_MAX_FAILURES:-}
[ -z "$max_failures" ] || [[ "$max_failures" =~ ^[1-9][0-9]*$ ]] ||
  usage "GO_TEST_RERUN_MAX_FAILURES is \"$max_failures\", not a number of tests above 0"

command -v gotestsum >/dev/null 2>&1 ||
  { echo "::error::go-test.sh: gotestsum is not on PATH; .github/actions/install-gotestsum installs the version .github/ci-tools.yaml pins" >&2; exit 1; }

if [ -n "${GO_TEST_JSONFILE:-}" ]; then
  jsonfile=$GO_TEST_JSONFILE
  [[ "$jsonfile" == *.json ]] || usage "GO_TEST_JSONFILE is \"$jsonfile\", not a path ending in .json"
  mkdir -p "$(dirname "$jsonfile")"
else
  mkdir -p "$reports"
  n=1
  while [ -e "$reports/${GITHUB_JOB:-go-test}-$n.json" ]; do n=$((n + 1)); done
  jsonfile="$reports/${GITHUB_JOB:-go-test}-$n.json"
fi
rerun_report="${jsonfile%.json}.rerun-fails.txt"
# A file an earlier run left under this name is no part of this run.
rm -f "$jsonfile" "$rerun_report"

retry_flags=()
if [ "$retry" = true ]; then
  retry_flags=(--rerun-fails=1 --rerun-fails-run-root-test --rerun-fails-abort-on-data-race --rerun-fails-report "$rerun_report")
  [ -z "$max_failures" ] || retry_flags+=("--rerun-fails-max-failures=$max_failures")
  why="the retry is on (GITHUB_EVENT_NAME=${GITHUB_EVENT_NAME:-}, GO_TEST_RETRY=${GO_TEST_RETRY:-})"
else
  why="the retry is off (GITHUB_EVENT_NAME=${GITHUB_EVENT_NAME:-}, GO_TEST_RETRY=${GO_TEST_RETRY:-})"
fi
command=(gotestsum --format standard-verbose --jsonfile "$jsonfile" "${retry_flags[@]}" --packages "${packages[*]}" -- "${flags[@]}")
[ "$race_memory" = false ] || command=("$here/race_memory.py" "${command[@]}")
echo "go-test.sh: $why${dir:+, from $dir}: ${command[*]}"

ran=0
if [ -n "$dir" ]; then
  (cd "$dir" && "${command[@]}") || ran=$?
else
  "${command[@]}" || ran=$?
fi
status=$ran

if [ ! -s "$jsonfile" ]; then
  echo "::error::go-test.sh: gotestsum wrote no jsonfile at $jsonfile (exit $ran), so no attempt of any package can be read" >&2
  [ "$status" -ne 0 ] || status=1
  exit "$status"
fi
lone=$(jq -n -r -L "$here" 'include "go-test"; no_test_failures(inputs) | "\(.package) \(.attempt)"' "$jsonfile") || {
  echo "::error::go-test.sh: $jsonfile is not a go test -json stream jq can read" >&2
  [ "$status" -ne 0 ] || status=1
  exit "$status"
}
while read -r package attempt; do
  [ -n "$package" ] || continue
  echo "::error::go-test.sh: $package failed in attempt $attempt with no test failing: a TestMain check after its tests passed, or a wrapper's own exit; a rerun runs no test of it, so it fails this step whatever a later attempt did" >&2
  [ "$status" -ne 0 ] || status=1
done <<<"$lone"
reported=""
[ ! -e "$rerun_report" ] || reported="; the rerun report is $rerun_report"
echo "go-test.sh: $(basename "${command[0]}") exited $ran, this step exits $status; the jsonfile is $jsonfile$reported"
exit "$status"
