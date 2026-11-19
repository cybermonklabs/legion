#!/usr/bin/env bash
# Tests for `go-fetch.sh`: what it runs for each mode, the retries that ride out a fault of the
# module proxy or the checksum database, and each input it refuses.
#
# Each case runs the real script as a subprocess under a stand-in `go` first on PATH, which
# records every call (its directory and arguments) and fails the calls a case asks it to fail, so
# what is tested is the commands a workflow step runs, with no network and no Go toolchain.
#
# Run from anywhere: .github/scripts/go-fetch.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
go_fetch="$script_dir/go-fetch.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# The stand-ins for go and pulumi: each call appends "<pwd> <name> <args>" to $work/calls; a call
# whose arguments contain FAIL_MATCH fails while the count of such calls is at most FAIL_TIMES.
# `go env GOPATH` answers GO_FETCH_TEST_GOPATH, and each `go install` records the GOBIN it ran
# under. A stand-in pulumi writes the sdks/ directory package add writes, so a second attempt in
# the same directory would find it there. pulumi and `go build` each record the sha256 of the
# go.mod and main.go they find in their directory, so a case can hold what they ran on to the
# consumer's own files, not only to the step's exit.
mkdir -p "$work/bin"
for name in go pulumi; do
  cat > "$work/bin/$name" <<'STANDIN'
#!/usr/bin/env bash
echo "$PWD $(basename "$0") $*" >> "$GO_FETCH_TEST_CALLS"
if [ "$(basename "$0")" = go ] && [ "$*" = "env GOPATH" ]; then
  echo "$GO_FETCH_TEST_GOPATH"
  exit 0
fi
if [ "$(basename "$0")" = go ] && [ "$1" = install ]; then
  echo "GOBIN=${GOBIN:-}" >> "$GO_FETCH_TEST_CALLS.gobin"
fi
if [ "$(basename "$0")" = pulumi ]; then
  echo "PULUMI_BACKEND_URL=${PULUMI_BACKEND_URL:-} PULUMI_HOME=${PULUMI_HOME:-} PULUMI_SKIP_UPDATE_CHECK=${PULUMI_SKIP_UPDATE_CHECK:-} PULUMI_API=${PULUMI_API:-}" >> "$GO_FETCH_TEST_CALLS.pulumi-env"
  [ ! -e sdks ] || { echo "sdks already exists" >&2; exit 3; }
  mkdir sdks
fi
if [ "$(basename "$0")" = pulumi ] || [ "$*" = "build ./..." ]; then
  echo "$(basename "$0") $(cat go.mod main.go 2>/dev/null | sha256sum | cut -d' ' -f1)" >> "$GO_FETCH_TEST_CALLS.seen"
fi
if [ -n "${FAIL_MATCH:-}" ] && [[ "$*" == *"$FAIL_MATCH"* ]]; then
  count=$(( $(cat "$GO_FETCH_TEST_COUNT" 2>/dev/null || echo 0) + 1 ))
  echo "$count" > "$GO_FETCH_TEST_COUNT"
  if [ "$count" -le "${FAIL_TIMES:-0}" ]; then
    echo "verifying module: reading https://sum.golang.org/tile/8/0/x160/632: stream error" >&2
    exit 1
  fi
fi
exit 0
STANDIN
  chmod +x "$work/bin/$name"
done

# A `cp` that fails, for the case whose copy of the consumer cannot be made: first on PATH only
# where a case puts $work/cp-fails there, and recording each call.
mkdir -p "$work/cp-fails"
printf '#!/bin/sh\necho "cp $*" >> "$GO_FETCH_TEST_CALLS.cp"\necho "cp: cannot create directory: No space left on device" >&2\nexit 42\n' \
  > "$work/cp-fails/cp"
chmod +x "$work/cp-fails/cp"

# A `mktemp` and a `mkdir` that fail, for the cases whose directory for the copy, or whose Pulumi
# backend and home, cannot be made: each first on PATH only where a case puts $work/<name>-fails
# there. Each fails, and records, the call the case is about - a call whose arguments hold the
# copy's template or the backend - and runs the real command for every other call: the script's
# own work directory, and the stand-in pulumi's sdks.
for name in mktemp mkdir; do
  case $name in
    mktemp) fails_on=/consumer. ;;
    mkdir) fails_on=/backend ;;
  esac
  mkdir -p "$work/$name-fails"
  cat > "$work/$name-fails/$name" <<STANDIN
#!/bin/sh
case "\$*" in
  *$fails_on*)
    echo "$name \$*" >> "\$GO_FETCH_TEST_CALLS.$name"
    echo "$name: cannot create directory: No space left on device" >&2
    exit 1
    ;;
esac
exec $(command -v "$name") "\$@"
STANDIN
  chmod +x "$work/$name-fails/$name"
done

# fetch <args...>: runs the script under test with the stand-ins and no waits. Leaves its stderr
# in `err`, its exit code in `rc`, the stand-ins' calls in `calls`, the environment each pulumi
# call saw in `pulumi_env`, each install's GOBIN in `gobin`, what package add and the build found
# in their directory in `seen`, and the failing cp's, mktemp's and mkdir's calls in `cp_calls`,
# `mktemp_calls` and `mkdir_calls`. PATH_FIRST, when set, goes on PATH before the stand-ins.
fetch() {
  rm -f "$work/calls" "$work/calls.pulumi-env" "$work/calls.gobin" "$work/calls.seen" "$work/calls.cp" \
    "$work/calls.mktemp" "$work/calls.mkdir" "$work/count"
  set +e
  PATH="${PATH_FIRST:+$PATH_FIRST:}$work/bin:$PATH" GO_FETCH_TEST_CALLS="$work/calls" GO_FETCH_TEST_COUNT="$work/count" \
    GO_FETCH_TEST_GOPATH="$work/gopath:$work/other-gopath" GO_FETCH_WAIT=0 "$go_fetch" "$@" > "$work/stdout" 2> "$work/stderr"
  rc=$?
  set -e
  err=$(cat "$work/stderr")
  calls=$(cat "$work/calls" 2>/dev/null || true)
  pulumi_env=$(cat "$work/calls.pulumi-env" 2>/dev/null || true)
  gobin=$(cat "$work/calls.gobin" 2>/dev/null || true)
  seen=$(cat "$work/calls.seen" 2>/dev/null || true)
  cp_calls=$(cat "$work/calls.cp" 2>/dev/null || true)
  mktemp_calls=$(cat "$work/calls.mktemp" 2>/dev/null || true)
  mkdir_calls=$(cat "$work/calls.mkdir" 2>/dev/null || true)
}

cat > "$work/tools.txt" <<'TOOLS'
# a comment line, and a blank one below

example.com/a/cmd/alpha@v1.2.3
example.com/b/v2@v2.0.1   # a comment after a pin
TOOLS

echo "case: tools installs each pin of the file, in its order, into a directory of the tools' own"
fetch tools "$work/tools.txt"
check "exits 0" "$(is "$rc" 0)"
check "installs the first pin" "$(contains "$calls" '^.* install example.com/a/cmd/alpha@v1.2.3$')"
check "installs the second pin, its comment left off" "$(contains "$calls" '^.* install example.com/b/v2@v2.0.1$')"
check "asks go env for GOPATH, then runs the two installs and nothing else" "$(is "$(grep -c . <<< "$calls")" 3)"
check "in the file's order" "$(is "$(grep ' install ' <<< "$calls" | sed -n 1p | grep -c alpha)" 1)"
check "into legion-live-tools under GOPATH's first entry, never the caller's GOBIN" \
  "$(is "$(sort -u <<< "$gobin")" "GOBIN=$work/gopath/legion-live-tools")"
check "both installs there" "$(is "$(grep -c . <<< "$gobin")" 2)"
check "says where" "$(contains "$err" "installing 2 tools into $work/gopath/legion-live-tools")"

echo "case: a pin whose install fails twice is tried again and installed"
FAIL_MATCH=alpha FAIL_TIMES=2 fetch tools "$work/tools.txt"
check "exits 0" "$(is "$rc" 0)"
check "tries the failing pin three times" "$(is "$(grep -c alpha <<< "$calls")" 3)"
check "then installs the next pin" "$(contains "$calls" 'install example.com/b/v2@v2.0.1$')"
check "says it tries again" "$(contains "$err" 'failed; trying again')"
check "shows the go command's own error" "$(contains "$err" 'sum.golang.org')"

echo "case: a pin whose install fails every attempt fails the run, naming it"
FAIL_MATCH=alpha FAIL_TIMES=99 GO_FETCH_ATTEMPTS=3 fetch tools "$work/tools.txt"
check "exits 1" "$(is "$rc" 1)"
check "tries it GO_FETCH_ATTEMPTS times" "$(is "$(grep -c alpha <<< "$calls")" 3)"
check "installs no later pin" "$(is "$(grep -c 'b/v2' <<< "$calls")" 0)"
check "names the command that failed" "$(contains "$err" 'failed 3 times: go install example.com/a/cmd/alpha@v1.2.3')"

echo "case: deps downloads what a build of the packages needs, in the directory named"
mkdir -p "$work/module"
fetch deps "$work/module" ./cmd/one ./cmd/two
check "exits 0" "$(is "$rc" 0)"
check "lists the packages' dependencies in that directory" \
  "$(is "$calls" "$work/module go list -deps ./cmd/one ./cmd/two")"

echo "case: deps is tried again too"
FAIL_MATCH=list FAIL_TIMES=1 fetch deps "$work/module" ./cmd/one
check "exits 0" "$(is "$rc" 0)"
check "runs the listing twice" "$(is "$(grep -c 'list -deps' <<< "$calls")" 2)"

echo "case: pulumi-consumer adds the plugin's SDK to a copy of the program and builds it"
mkdir -p "$work/consumer-src"
printf 'module example.com/consumer\n' > "$work/consumer-src/go.mod"
printf 'package main\n' > "$work/consumer-src/main.go"
printf '#!/bin/sh\n' > "$work/plugin" && chmod +x "$work/plugin"
fetch pulumi-consumer "$work/consumer-src" "$work/plugin"
check "exits 0" "$(is "$rc" 0)"
check "runs package add of the plugin" "$(contains "$calls" " pulumi package add $work/plugin$")"
check "on a file backend of its own, so it asks Pulumi Cloud for no account" \
  "$(contains "$pulumi_env" '^PULUMI_BACKEND_URL=file:///[^ ]* PULUMI_HOME=/')"
check "with no update check and the Pulumi Cloud API at a local port that refuses, so it asks api.pulumi.com nothing" \
  "$(contains "$pulumi_env" ' PULUMI_SKIP_UPDATE_CHECK=true PULUMI_API=http://127\.0\.0\.1:1$')"
check "then the build, in the same copy" \
  "$(is "$(awk '{print $1}' <<< "$calls" | sort -u | grep -c .)" 1)"
check "builds the copy" "$(contains "$calls" ' go build ./...$')"
check "package add and the build each ran on the consumer's own go.mod and main.go" \
  "$(is "$seen" "$(printf 'pulumi %s\ngo %s' "$(cat "$work/consumer-src/go.mod" "$work/consumer-src/main.go" | sha256sum | cut -d' ' -f1)" \
    "$(cat "$work/consumer-src/go.mod" "$work/consumer-src/main.go" | sha256sum | cut -d' ' -f1)")")"
check "leaves the source untouched" \
  "$(is "$(find "$work/consumer-src" -mindepth 1 -printf '%f\n' | sort | tr '\n' ' ')" 'go.mod main.go ')"
check "runs in no copy under the source" "$(is "$(grep -c "^$work/consumer-src" <<< "$calls")" 0)"

echo "case: a copy of the consumer that cannot be made ends the step, naming it, and runs nothing after it"
PATH_FIRST="$work/cp-fails" fetch pulumi-consumer "$work/consumer-src" "$work/plugin"
check "exits 1" "$(is "$rc" 1)"
check "names the copy that failed" "$(contains "$err" "could not copy the consumer $work/consumer-src into ")"
check "as a GitHub error" "$(contains "$err" '::error::go-fetch: could not copy')"
check "tries the copy once: a failed copy is no fault of a service" "$(is "$(grep -c . <<< "$cp_calls")" 1)"
check "runs neither package add nor the build" "$(is "$calls" '')"

echo "case: a directory for the copy that cannot be made ends the step, naming it, and runs nothing after it"
PATH_FIRST="$work/mktemp-fails" fetch pulumi-consumer "$work/consumer-src" "$work/plugin"
check "exits 1" "$(is "$rc" 1)"
check "names the directory that could not be made" \
  "$(contains "$err" "could not make a directory under /[^ ]* for a copy of $work/consumer-src$")"
check "as a GitHub error" "$(contains "$err" '::error::go-fetch: could not make a directory')"
check "shows mktemp's own error" "$(contains "$err" '^mktemp: cannot create directory')"
check "ends there: copies nothing" "$(is "$(grep -c 'could not copy' <<< "$err")" 0)"
check "tries the directory once: a directory that cannot be made is no fault of a service" \
  "$(is "$(grep -c . <<< "$mktemp_calls")" 1)"
check "runs neither package add nor the build" "$(is "$calls" '')"

echo "case: a Pulumi backend and home that cannot be made end the step, naming them, and run nothing after them"
PATH_FIRST="$work/mkdir-fails" fetch pulumi-consumer "$work/consumer-src" "$work/plugin"
check "exits 1" "$(is "$rc" 1)"
check "names the backend and home that could not be made" \
  "$(contains "$err" 'could not make the Pulumi backend and home under /')"
check "as a GitHub error" "$(contains "$err" '::error::go-fetch: could not make the Pulumi backend')"
check "shows mkdir's own error" "$(contains "$err" '^mkdir: cannot create directory')"
check "tries them once: a backend that cannot be made is no fault of a service" \
  "$(is "$(grep -c . <<< "$mkdir_calls")" 1)"
check "runs neither package add nor the build" "$(is "$calls" '')"

echo "case: a failed package add is tried again on a fresh copy"
FAIL_MATCH="package add" FAIL_TIMES=1 fetch pulumi-consumer "$work/consumer-src" "$work/plugin"
check "exits 0" "$(is "$rc" 0)"
check "runs package add twice" "$(is "$(grep -c 'pulumi package add' <<< "$calls")" 2)"
check "each in a copy of its own" "$(is "$(grep 'pulumi package add' <<< "$calls" | awk '{print $1}' | sort -u | grep -c .)" 2)"
check "never finds the first attempt's sdks" "$(is "$(grep -c 'sdks already exists' <<< "$err")" 0)"

echo "case: pulumi-consumer refuses a source with no go.mod and a missing plugin"
fetch pulumi-consumer "$work/module" "$work/plugin"
check "no go.mod exits 2" "$(is "$rc" 2)"
fetch pulumi-consumer "$work/consumer-src" "$work/absent-plugin"
check "no plugin exits 2" "$(is "$rc" 2)"
check "and runs nothing" "$(is "$calls" '')"

echo "case: a line that is no pin refuses the file before anything is installed"
printf 'example.com/a/cmd/alpha@v1.2.3\nexample.com/c/cmd/gamma@latest\n' > "$work/bad.txt"
fetch tools "$work/bad.txt"
check "exits 2" "$(is "$rc" 2)"
check "names the file, the line and the text" "$(contains "$err" "$work/bad.txt:2: example.com/c/cmd/gamma@latest")"
check "installs nothing" "$(is "$calls" '')"

echo "case: an empty list refuses rather than installing nothing"
printf '# nothing here\n' > "$work/empty.txt"
fetch tools "$work/empty.txt"
check "exits 2" "$(is "$rc" 2)"
check "says the file pins nothing" "$(contains "$err" 'pins no tool')"

echo "case: usage errors"
fetch
check "no mode exits 2" "$(is "$rc" 2)"
fetch download "$work/tools.txt"
check "an unknown mode exits 2" "$(is "$rc" 2)"
fetch tools "$work/absent.txt"
check "a missing file exits 2" "$(is "$rc" 2)"
fetch deps "$work/module"
check "deps with no package exits 2" "$(is "$rc" 2)"
fetch deps "$work/absent-dir" ./cmd/one
check "deps in a missing directory exits 2" "$(is "$rc" 2)"
check "and runs nothing" "$(is "$calls" '')"

summary "go-fetch.sh installs each pinned tool and downloads each build's modules, retrying each go command, and refuses every malformed input"
