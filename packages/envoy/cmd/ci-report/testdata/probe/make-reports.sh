#!/usr/bin/env bash
# Makes again the three go test -json reports of ../ that this package's tests read, from the
# packages of cases/, in a scratch module example.com/acme/probe, with Go 1.27.3, the Go that
# packages/envoy/go.mod names:
#   - ../report.jsonl: `go test -json -count=1 -trimpath -timeout 5s ./...`, which exits 1. In
#     claims one test passes, one fails and one skips; in slow TestTheQuickReadEnds passes and
#     TestTheSlowReadEnds sleeps for 30 s, so go test stops it at the 5 s limit; broken's test file
#     does not build; twice defines TestTheNameRunsTwice in package twice and in package twice_test.
#   - ../killed.jsonl: go test of TestTheSlowReadEnds alone with a 10 minute limit, killed with
#     SIGKILL, go test and the test binary together, once the report holds the test's first log
#     line, as the end of a job kills it: the report stops inside the test, with no end event.
#   - ../cached.jsonl: the second of two runs of `go test -json -trimpath -run
#     '^TestTheQuickReadEnds$' ./slow/` on one test cache, which go test replays from the first.
# Run it when that Go moves or a case changes, then read the new reports and the rows that read
# them. -trimpath writes each file of a stack as its import path, so no path of the machine that
# ran it reaches a report; the script refuses, and writes nothing, when a report names the
# scratch module, GOROOT or HOME all the same.
set -euo pipefail
version=go1.27.3
here=$(cd "$(dirname "$0")" && pwd)
out=$(cd "$here/.." && pwd)
module=$(mktemp -d)
scratch=$(mktemp -d)
trap 'rm -rf "$module" "$scratch"' EXIT
cp -R "$here/cases/." "$module/"
printf 'module example.com/acme/probe\n\ngo 1.27\n' >"$module/go.mod"
unset GOROOT
export GOTOOLCHAIN=$version GOWORK=off GOFLAGS= GOCACHE="$scratch/cache"
cd "$module"
if [ "$(go env GOVERSION)" != "$version" ]; then
  echo "make-reports.sh: go env GOVERSION is $(go env GOVERSION), want $version" >&2
  exit 1
fi

# report.jsonl: every package, which exits 1, since claims, slow and broken fail.
status=0
go test -json -count=1 -trimpath -timeout 5s ./... >"$scratch/report.jsonl" || status=$?
if [ "$status" -ne 1 ]; then
  echo "make-reports.sh: go test ./...: exit $status, want 1" >&2
  exit 1
fi
echo "go test -json -count=1 -trimpath -timeout 5s ./...: exit $status"

# killed.jsonl: the test binary is built first, so the run reaches the test at once; then go test
# runs in a process group of its own, which is killed whole once the test has logged its line.
go test -count=1 -trimpath -run '^$' ./slow/ >/dev/null
setsid go test -json -count=1 -trimpath -timeout 10m -run '^TestTheSlowReadEnds$' ./slow/ >"$scratch/killed.jsonl" 2>/dev/null &
group=$!
for _ in $(seq 600); do
  if grep -qF 'the read starts' "$scratch/killed.jsonl"; then break; fi
  sleep 0.1
done
if ! grep -qF 'the read starts' "$scratch/killed.jsonl"; then
  kill -KILL -- "-$group"
  echo "make-reports.sh: the slow test logged no line within 60 s" >&2
  exit 1
fi
kill -KILL -- "-$group"
wait "$group" || true
if grep -qE '"Action":"(pass|fail)"' "$scratch/killed.jsonl"; then
  echo "make-reports.sh: the killed report holds an end event" >&2
  exit 1
fi
echo "go test of TestTheSlowReadEnds: killed with SIGKILL once it logged its line"

# cached.jsonl: the first run fills the test cache, and the second replays it.
go test -json -trimpath -run '^TestTheQuickReadEnds$' ./slow/ >/dev/null
go test -json -trimpath -run '^TestTheQuickReadEnds$' ./slow/ >"$scratch/cached.jsonl"
if ! grep -qF '(cached)' "$scratch/cached.jsonl"; then
  echo "make-reports.sh: the second run of ./slow/ was not replayed from the test cache" >&2
  exit 1
fi
echo "go test of TestTheQuickReadEnds, twice: the second replayed from the test cache"

for report in report killed cached; do
  for path in "$module" "$scratch" "$(go env GOROOT)" "${HOME:-}"; do
    if [ -n "$path" ] && [ "$path" != / ] && grep -qF -e "$path" "$scratch/$report.jsonl"; then
      echo "make-reports.sh: $report.jsonl names $path, a path of this machine; nothing was written" >&2
      exit 1
    fi
  done
done
for report in report killed cached; do
  cp "$scratch/$report.jsonl" "$out/$report.jsonl"
  echo "$(wc -l <"$out/$report.jsonl") events in $out/$report.jsonl"
done
