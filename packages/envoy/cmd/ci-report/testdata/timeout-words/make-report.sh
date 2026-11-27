#!/usr/bin/env bash
# Makes again ../timeout-words.jsonl, which TestATestThatQuotesTheTimeoutWordsKeepsItsOwnOutcome
# reads: two real runs of Go 1.27.3, the Go that packages/envoy/go.mod names, in a scratch module
# example.com/acc that holds the packages of cases/, one after the other in one file:
#   - `go test -json -count=1 -trimpath ./output/`: TestSaysTimedOut passes and
#     TestFailsSayingTimedOut fails, each after a t.Log that quotes "panic: test timed out after";
#   - `go test -json -count=1 -trimpath -timeout 3s ./slow/`: go test kills TestSlow at its time
#     limit.
# Each run exits 1, since its package fails, and each report holds its package's start and end
# events, without which the reader gives no test an outcome.
# Run it when that Go moves or a case changes, and then read the new report and the row that reads
# it. -trimpath writes each file of a stack as its import path, GOROOT's and the module's alike, so
# no path of the machine that ran it reaches the report; the script refuses, and writes nothing,
# when the report names the scratch module, GOROOT or HOME all the same.
set -euo pipefail
version=go1.27.3
here=$(cd "$(dirname "$0")" && pwd)
out="$here/../timeout-words.jsonl"
module=$(mktemp -d)
report=$(mktemp)
trap 'rm -rf "$module" "$report"' EXIT
cp -R "$here/cases/." "$module/"
printf 'module example.com/acc\n\ngo 1.27\n' >"$module/go.mod"
unset GOROOT
export GOTOOLCHAIN=$version GOWORK=off GOFLAGS=
cd "$module"
if [ "$(go env GOVERSION)" != "$version" ]; then
  echo "make-report.sh: go env GOVERSION is $(go env GOVERSION), want $version" >&2
  exit 1
fi
# run <go test arguments...>: appends the report of one go test run, which exits 1.
run() {
  local status=0
  go test -json -count=1 -trimpath "$@" >>"$report" || status=$?
  if [ "$status" -ne 1 ]; then
    echo "make-report.sh: go test $*: exit $status, want 1, since its package fails" >&2
    exit 1
  fi
  echo "go test -json -count=1 -trimpath $*: exit $status"
}
run ./output/
run -timeout 3s ./slow/
for path in "$module" "$(go env GOROOT)" "${HOME:-}"; do
  if [ -n "$path" ] && [ "$path" != / ] && grep -qF -e "$path" "$report"; then
    echo "make-report.sh: the report names $path, a path of this machine; nothing was written" >&2
    exit 1
  fi
done
cp "$report" "$out"
echo "$(wc -l <"$out") events in $out"
