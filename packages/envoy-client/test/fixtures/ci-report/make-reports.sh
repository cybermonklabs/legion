#!/usr/bin/env bash
# Makes again the go test -json reports beside it, which ci-report.live.test.ts hands ci-report:
# real runs of Go 1.27.3, the Go that packages/envoy/go.mod names, of the package in cases/, in a
# scratch module example.com/acme/probe. A test of it fails when PROBE_FAILS names it.
#   - go-test-all-pass.jsonl: TestProbe and TestSecondProbe, both passing;
#   - go-test-first-fails.jsonl: the same two, TestProbe failing;
#   - go-test-second-fails.jsonl: the same two, TestSecondProbe failing;
#   - go-test-four-pass.jsonl: TestProbe, TestSecondProbe, TestThirdProbe and TestFourthProbe,
#     each passing;
#   - go-test-job-passed.jsonl: go-test-first-fails.jsonl with its three package events, and those
#     alone, changed to those of a passing package ("PASS", "ok", pass): a job whose own outcome is
#     success while its named test failed, which go test itself never writes.
# Each run is `go test -json -count=1 -trimpath -run <its tests> .`, and exits 0 when its tests
# pass and 1 when one fails. Run it when that Go moves or the case changes, then read the new
# reports and the rows that read them. -trimpath writes each file of a stack as its import path,
# so no path of the machine that ran it reaches a report; the script refuses, and writes nothing,
# when a report names the scratch module, GOROOT or HOME all the same.
set -euo pipefail
version=go1.27.3
here=$(cd "$(dirname "$0")" && pwd)
module=$(mktemp -d)
scratch=$(mktemp -d)
trap 'rm -rf "$module" "$scratch"' EXIT
cp -R "$here/cases/." "$module/"
printf 'module example.com/acme/probe\n\ngo 1.27\n' >"$module/go.mod"
unset GOROOT
export GOTOOLCHAIN=$version GOWORK=off GOFLAGS=
cd "$module"
if [ "$(go env GOVERSION)" != "$version" ]; then
  echo "make-reports.sh: go env GOVERSION is $(go env GOVERSION), want $version" >&2
  exit 1
fi
two='^(TestProbe|TestSecondProbe)$'
four='^(TestProbe|TestSecondProbe|TestThirdProbe|TestFourthProbe)$'
# run <report> <failing tests> <want exit> <-run pattern>: writes the report of one go test run.
run() {
  local status=0
  PROBE_FAILS=$2 go test -json -count=1 -trimpath -run "$4" . >"$scratch/$1" || status=$?
  if [ "$status" -ne "$3" ]; then
    echo "make-reports.sh: $1: go test exit $status, want $3" >&2
    exit 1
  fi
  echo "$1: PROBE_FAILS='$2' go test -json -count=1 -trimpath -run '$4' .: exit $status"
}
run go-test-all-pass.jsonl "" 0 "$two"
run go-test-first-fails.jsonl TestProbe 1 "$two"
run go-test-second-fails.jsonl TestSecondProbe 1 "$two"
run go-test-four-pass.jsonl "" 0 "$four"
# The package's three events of go-test-first-fails.jsonl, as a passing package writes them: its
# "ok" line carries no OutputType, as go-test-all-pass.jsonl's does not.
jq -c 'if .Test == null and .Action == "fail" then .Action = "pass"
  elif .Test == null and .Action == "output" and .Output == "FAIL\n" then .Output = "PASS\n"
  elif .Test == null and .Action == "output" and (.Output | startswith("FAIL\texample.com/acme/probe\t"))
    then .Output = "ok  " + (.Output | ltrimstr("FAIL")) | del(.OutputType)
  else . end' "$scratch/go-test-first-fails.jsonl" >"$scratch/go-test-job-passed.jsonl"
changed=$(diff <(jq -c . "$scratch/go-test-first-fails.jsonl") "$scratch/go-test-job-passed.jsonl" | grep -c '^>' || true)
if [ "$changed" -ne 3 ]; then
  echo "make-reports.sh: go-test-job-passed.jsonl changes $changed events of go-test-first-fails.jsonl, want 3" >&2
  exit 1
fi
reports=(go-test-all-pass.jsonl go-test-first-fails.jsonl go-test-second-fails.jsonl go-test-four-pass.jsonl go-test-job-passed.jsonl)
for report in "${reports[@]}"; do
  for path in "$module" "$scratch" "$(go env GOROOT)" "${HOME:-}"; do
    if [ -n "$path" ] && [ "$path" != / ] && grep -qF -e "$path" "$scratch/$report"; then
      echo "make-reports.sh: $report names $path, a path of this machine; nothing was written" >&2
      exit 1
    fi
  done
done
for report in "${reports[@]}"; do
  cp "$scratch/$report" "$here/$report"
  echo "$(wc -l <"$here/$report") events in $report"
done
