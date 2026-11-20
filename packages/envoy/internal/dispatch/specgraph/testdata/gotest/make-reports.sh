#!/usr/bin/env bash
# Makes again the go test -json reports of this directory that come from the packages in cases/,
# each a real run of Go 1.27.3, the Go that packages/envoy/go.mod names, in a scratch module
# example.com/acme/gotest:
#   - <name>.jsonl, for each package cases/<name>: `go test -json -trimpath` of the package. The
#     packages whose names start with kill run under -timeout 1s, count2 runs under -count=2, and
#     each other package under -count=1.
#   - t2j-<name>.jsonl, for each package the list t2j names: `go tool test2json -t -p <package>`
#     run over the package's test binary with -test.v=test2json, the form `go doc cmd/test2json`
#     names, in the package's directory, as go test runs a binary. The kill packages run under
#     -test.timeout=1s.
#   - piped-<name>.jsonl, for each package the list piped names: the text of `go test -v -trimpath
#     -count=1 -timeout 1s` of the package through `go tool test2json -t -p <package>`. That form
#     holds no marker, so the reader's stated limit applies to it.
#   - verbose-<name>.jsonl, for each package the list verbose names: `go test -json -v -trimpath
#     -count=1` of the package. go runs the binary with -test.v=true then, not test2json, so this is
#     a form in which a test can write its own end line, and the reader's stated limit applies to it.
#   - go1.26-subprint.jsonl: `go test -json -trimpath -count=1` of the package subprint with Go
#     1.26.11, in a scratch module of its own whose go.mod says go 1.26. Its converter marks no line
#     as a frame, which is the report the reader refuses.
# Run it when that Go moves or a case changes, and then read each new report and the rows of
# gotest_test.go that read it. -trimpath keeps each path of a stack relative to its module, and the
# script refuses a report that names one of its scratch directories.
set -euo pipefail
version=go1.27.3
t2j=(exitkill killfirst killprinted killprintedfail killsame killsibling killsiblingpass killstack ownlines subprint)
piped=(killprinted)
verbose=(splitlines forgedpass)
here=$(cd "$(dirname "$0")" && pwd)
module=$(mktemp -d)
binaries=$(mktemp -d)
old=$(mktemp -d)
trap 'rm -rf "$module" "$binaries" "$old"' EXIT
cp -R "$here/cases/." "$module/"
printf 'module example.com/acme/gotest\n\ngo 1.27\n' >"$module/go.mod"
unset GOROOT
export GOTOOLCHAIN=$version GOWORK=off GOFLAGS=
cd "$module"
if [ "$(go env GOVERSION)" != "$version" ]; then
  echo "make-reports.sh: go env GOVERSION is $(go env GOVERSION), want $version" >&2
  exit 1
fi
# refuse <report>: fails when the report names a scratch directory of this run.
refuse() {
  if grep -qF -e "$module" -e "$binaries" -e "$old" "$1"; then
    echo "make-reports.sh: $(basename "$1") names a scratch directory of this run" >&2
    exit 1
  fi
}
for dir in */; do
  name=${dir%/}
  case $name in
    kill*) flags=(-count=1 -timeout 1s) ;;
    count2) flags=(-count=2) ;;
    *) flags=(-count=1) ;;
  esac
  status=0
  go test -json -trimpath "${flags[@]}" "./$name/" >"$here/$name.jsonl" || status=$?
  refuse "$here/$name.jsonl"
  echo "go test -json -trimpath ${flags[*]} ./$name/: exit $status, $(wc -l <"$here/$name.jsonl") events"
done
for name in "${t2j[@]}"; do
  flags=()
  case $name in
    kill*) flags=(-test.timeout=1s) ;;
  esac
  go test -c -trimpath -o "$binaries/$name.test" "./$name/"
  status=0
  (cd "$module/$name" && go tool test2json -t -p "example.com/acme/gotest/$name" "$binaries/$name.test" -test.v=test2json "${flags[@]}") >"$here/t2j-$name.jsonl" || status=$?
  refuse "$here/t2j-$name.jsonl"
  echo "go tool test2json -t -p example.com/acme/gotest/$name $name.test -test.v=test2json ${flags[*]}: exit $status, $(wc -l <"$here/t2j-$name.jsonl") events"
done
for name in "${piped[@]}"; do
  status=0
  (go test -v -trimpath -count=1 -timeout 1s "./$name/" 2>&1 | go tool test2json -t -p "example.com/acme/gotest/$name") >"$here/piped-$name.jsonl" || status=$?
  refuse "$here/piped-$name.jsonl"
  echo "go test -v -trimpath -count=1 -timeout 1s ./$name/ | go tool test2json -t -p example.com/acme/gotest/$name: exit $status, $(wc -l <"$here/piped-$name.jsonl") events"
done
for name in "${verbose[@]}"; do
  status=0
  go test -json -v -trimpath -count=1 "./$name/" >"$here/verbose-$name.jsonl" || status=$?
  refuse "$here/verbose-$name.jsonl"
  echo "go test -json -v -trimpath -count=1 ./$name/: exit $status, $(wc -l <"$here/verbose-$name.jsonl") events"
done
cp -R "$here/cases/subprint" "$old/"
printf 'module example.com/acme/gotest\n\ngo 1.26\n' >"$old/go.mod"
status=0
(cd "$old" && GOTOOLCHAIN=go1.26.11 go test -json -trimpath -count=1 ./subprint/) >"$here/go1.26-subprint.jsonl" || status=$?
refuse "$here/go1.26-subprint.jsonl"
echo "go1.26.11 go test -json -trimpath -count=1 ./subprint/: exit $status, $(wc -l <"$here/go1.26-subprint.jsonl") events"
