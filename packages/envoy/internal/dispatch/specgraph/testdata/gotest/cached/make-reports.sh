#!/usr/bin/env bash
# Makes again the go test -json reports of this directory: real runs of Go 1.27.3, the Go that
# packages/envoy/go.mod names, of the packages in cases/, in a scratch module
# example.com/acme/gotest with a test cache of its own. The cache starts empty on each run of the
# script, so a run is replayed from it exactly when an earlier go test of the same run filled it:
#   - first.jsonl: `go test -json -trimpath ./cached/`, run once, before the cache holds it.
#   - first-long.jsonl: the same of the package long, run once, before the cache holds it. The
#     script moves cases/long to long/<five directories of 220 bytes>, an import path of 1,133
#     bytes, so go test's converter writes the package's summary line in two output events.
#   - first-mainline.jsonl: the same of the package mainline, run once, before the cache holds it.
#     Its TestMain prints a marked run line of TestZ after the tests, so the converter names TestZ
#     in the events after it, the package's summary line among them.
# Each of the other commands runs two times, GOTEST_RUN=1 and then GOTEST_RUN=2, and the second
# report is kept:
#   - cached.jsonl: `go test -json -trimpath ./cached/`, replayed: "ok  \t<path>\t(cached)".
#   - cached-cover.jsonl: with -cover: "(cached)\tcoverage: 100.0% of statements".
#   - cached-norun.jsonl: with -run '^TestNone$': "(cached) [no tests to run]".
#   - cached-two.jsonl: `./cached/ ./fresh/`: cached is replayed, and fresh runs again, because its
#     test reads GOTEST_RUN.
#   - cached-long.jsonl: the package long, replayed: "(cached)" is in the second event of its
#     summary line.
#   - cached-mainline.jsonl: the package mainline, replayed: its summary line, which holds
#     "(cached)", is an event of TestZ.
# Run it when that Go moves or a case changes, and then read each new report and the rows of
# cached_test.go that read it. The script refuses a report that names one of its scratch
# directories.
set -euo pipefail
version=go1.27.3
here=$(cd "$(dirname "$0")" && pwd)
module=$(mktemp -d)
trap 'rm -rf "$module"' EXIT
cp -R "$here/cases/." "$module/"
dir=x$(printf 'y%.0s' $(seq 219))
long=long/$dir/$dir/$dir/$dir/$dir
mkdir -p "$module/$long"
mv "$module/long/long_test.go" "$module/$long/"
printf 'module example.com/acme/gotest\n\ngo 1.27\n' >"$module/go.mod"
unset GOROOT
export GOTOOLCHAIN=$version GOWORK=off GOFLAGS= GOCACHE=$module/.gocache
cd "$module"
if [ "$(go env GOVERSION)" != "$version" ]; then
  echo "make-reports.sh: go env GOVERSION is $(go env GOVERSION), want $version" >&2
  exit 1
fi
# refuse <report>: fails when the report names the scratch directory of this run.
refuse() {
  if grep -qF "$module" "$1"; then
    echo "make-reports.sh: $(basename "$1") names a scratch directory of this run" >&2
    exit 1
  fi
}
# report <name> <go test arguments>...: the two runs, the second kept as <name>.jsonl.
report() {
  local name=$1
  shift
  GOTEST_RUN=1 go test -json -trimpath "$@" >/dev/null
  GOTEST_RUN=2 go test -json -trimpath "$@" >"$here/$name.jsonl"
  refuse "$here/$name.jsonl"
  echo "go test -json -trimpath $*, the second run: $(wc -l <"$here/$name.jsonl") events"
}
GOTEST_RUN=1 go test -json -trimpath ./cached/ >"$here/first.jsonl"
refuse "$here/first.jsonl"
echo "go test -json -trimpath ./cached/, the first run: $(wc -l <"$here/first.jsonl") events"
GOTEST_RUN=1 go test -json -trimpath "./$long/" >"$here/first-long.jsonl"
refuse "$here/first-long.jsonl"
echo "go test -json -trimpath ./long/<five directories>/, the first run: $(wc -l <"$here/first-long.jsonl") events"
GOTEST_RUN=1 go test -json -trimpath ./mainline/ >"$here/first-mainline.jsonl"
refuse "$here/first-mainline.jsonl"
echo "go test -json -trimpath ./mainline/, the first run: $(wc -l <"$here/first-mainline.jsonl") events"
report cached ./cached/
report cached-cover -cover ./cached/
report cached-norun -run '^TestNone$' ./cached/
report cached-two ./cached/ ./fresh/
report cached-long "./$long/"
report cached-mainline ./mainline/
