#!/usr/bin/env bash
# Makes again go-test-long-output.jsonl, which ci-report-long-output.live.test.ts hands ci-report:
# a real run of Go 1.27.3, the Go that packages/envoy/go.mod names, of the package in cases/, in a
# scratch module example.com/acc, `go test -json -count=1 -trimpath ./output/`, which exits 1,
# since both of its tests fail: TestThreeK after printing 3000 "k" between its markers, and
# TestSeventyK after 70 lines of 1023 "s". The report holds the package's start and end events,
# without which the reader gives no test an outcome but could_not_run. Run it when that Go moves
# or the case changes, then read the new report and the row that reads it. -trimpath writes each
# file of a stack as its import path, so no path of the machine that ran it reaches the report;
# the script refuses, and writes nothing, when the report names the scratch module, GOROOT or
# HOME all the same.
set -euo pipefail
version=go1.27.3
here=$(cd "$(dirname "$0")" && pwd)
out="$here/go-test-long-output.jsonl"
module=$(mktemp -d)
report=$(mktemp)
trap 'rm -rf "$module" "$report"' EXIT
mkdir "$module/output"
cp -R "$here/cases/." "$module/output/"
printf 'module example.com/acc\n\ngo 1.27\n' >"$module/go.mod"
unset GOROOT
export GOTOOLCHAIN=$version GOWORK=off GOFLAGS=
cd "$module"
if [ "$(go env GOVERSION)" != "$version" ]; then
  echo "make-report.sh: go env GOVERSION is $(go env GOVERSION), want $version" >&2
  exit 1
fi
status=0
go test -json -count=1 -trimpath ./output/ >"$report" || status=$?
if [ "$status" -ne 1 ]; then
  echo "make-report.sh: go test ./output/: exit $status, want 1, since both tests fail" >&2
  exit 1
fi
echo "go test -json -count=1 -trimpath ./output/: exit $status"
for path in "$module" "$(go env GOROOT)" "${HOME:-}"; do
  if [ -n "$path" ] && [ "$path" != / ] && grep -qF -e "$path" "$report"; then
    echo "make-report.sh: the report names $path, a path of this machine; nothing was written" >&2
    exit 1
  fi
done
cp "$report" "$out"
echo "$(wc -l <"$out") events in $out"
