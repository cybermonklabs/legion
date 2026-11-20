#!/usr/bin/env bash
# Tests for `go-fetch.sh`: what it runs for each mode (a fetch file's lines among them, each in
# its directory and for its platform), the proxy it fetches through, the retries that ride out a
# fault of the module proxy or the checksum database, and each input it refuses.
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
# `go env NAME...` answers one line per name: GOPATH from GO_FETCH_TEST_GOPATH, GOMODCACHE from
# GO_FETCH_TEST_GOMODCACHE and GOPROXY from GO_FETCH_TEST_GOPROXY. Each `go install` records the
# GOBIN and the GOPROXY it ran under, and each `go list` the GOPROXY, GOOS, GOARCH,
# CGO_ENABLED and GOWORK it ran under. Each `go mod tidy -modfile=FILE` records the GOPROXY it ran under,
# FILE, and the sha256 of FILE and of the go.sum beside it (or `none`), then appends a line to
# both, as tidy writes them, so a case can hold what tidy ran on to the module's own files and see
# whether a second attempt started from them afresh. A stand-in pulumi writes the sdks/ directory
# package add writes, so a second attempt in the same directory would find it there. pulumi and
# `go build` each record the sha256 of the go.mod and main.go they find in their directory, so a
# case can hold what they ran on to the consumer's own files, not only to the step's exit.
mkdir -p "$work/bin"
for name in go pulumi; do
  cat > "$work/bin/$name" <<'STANDIN'
#!/usr/bin/env bash
echo "$PWD $(basename "$0") $*" >> "$GO_FETCH_TEST_CALLS"
if [ "$(basename "$0")" = go ] && [ "$1" = env ]; then
  shift
  for var in "$@"; do
    case $var in
      GOPATH) echo "$GO_FETCH_TEST_GOPATH" ;;
      GOMODCACHE) echo "$GO_FETCH_TEST_GOMODCACHE" ;;
      GOPROXY) echo "$GO_FETCH_TEST_GOPROXY" ;;
      *) echo "the go stand-in answers no go env $var" >&2; exit 2 ;;
    esac
  done
  exit 0
fi
if [ "$(basename "$0")" = go ] && [ "$1" = install ]; then
  echo "GOBIN=${GOBIN:-}" >> "$GO_FETCH_TEST_CALLS.gobin"
  echo "GOPROXY=${GOPROXY:-}" >> "$GO_FETCH_TEST_CALLS.goproxy"
fi
if [ "$(basename "$0")" = go ] && [ "$1" = list ]; then
  echo "GOPROXY=${GOPROXY:-} GOOS=${GOOS:-} GOARCH=${GOARCH:-} CGO_ENABLED=${CGO_ENABLED:-}" >> "$GO_FETCH_TEST_CALLS.list-env"
  echo "GOWORK=${GOWORK:-}" >> "$GO_FETCH_TEST_CALLS.list-gowork"
fi
if [ "$(basename "$0")" = go ] && [ "$1" = mod ] && [ "$2" = tidy ]; then
  modfile=${3#-modfile=}
  sum=${modfile%.mod}.sum
  summed=none
  [ ! -f "$sum" ] || summed=$(sha256sum < "$sum" | cut -d' ' -f1)
  echo "GOPROXY=${GOPROXY:-} modfile=$modfile go.mod=$(sha256sum < "$modfile" | cut -d' ' -f1) go.sum=$summed" >> "$GO_FETCH_TEST_CALLS.tidy"
  echo "// what tidy writes" >> "$modfile"
  echo "what tidy writes" >> "$sum"
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
# call saw in `pulumi_env`, each install's GOBIN in `gobin` and its GOPROXY in `goproxy`, each
# listing's GOPROXY and platform in `list_env` and its GOWORK in `list_gowork`, each tidy's GOPROXY,
# file and what it ran on in `tidy`, what package add and the build found in their directory in
# `seen`, and the failing cp's, mktemp's and mkdir's calls in `cp_calls`, `mktemp_calls` and
# `mkdir_calls`. PATH_FIRST, when set, goes on PATH before the stand-ins. `go env` answers $work/modcache for GOMODCACHE and
# `https://proxy.example|direct` for GOPROXY, or TEST_GOMODCACHE and TEST_GOPROXY when set.
# SCRIPT, when set, is the copy of the script to run instead of this repository's, and
# GO_FETCH_PROXY, when the case sets it, reaches the script; the caller's own is cleared.
fetch() {
  rm -f "$work/calls" "$work/calls.pulumi-env" "$work/calls.gobin" "$work/calls.goproxy" "$work/calls.seen" \
    "$work/calls.cp" "$work/calls.mktemp" "$work/calls.mkdir" "$work/calls.list-env" "$work/calls.list-gowork" "$work/calls.tidy" "$work/count"
  set +e
  PATH="${PATH_FIRST:+$PATH_FIRST:}$work/bin:$PATH" GO_FETCH_TEST_CALLS="$work/calls" GO_FETCH_TEST_COUNT="$work/count" \
    GO_FETCH_TEST_GOPATH="$work/gopath:$work/other-gopath" GO_FETCH_TEST_GOMODCACHE="${TEST_GOMODCACHE-$work/modcache}" \
    GO_FETCH_TEST_GOPROXY="${TEST_GOPROXY-https://proxy.example|direct}" GO_FETCH_WAIT=0 \
    GO_FETCH_PROXY="${TEST_GO_FETCH_PROXY:-}" GOPROXY="${TEST_ENV_GOPROXY:-}" GOWORK='' \
    "${SCRIPT:-$go_fetch}" "$@" > "$work/stdout" 2> "$work/stderr"
  rc=$?
  set -e
  err=$(cat "$work/stderr")
  calls=$(cat "$work/calls" 2>/dev/null || true)
  pulumi_env=$(cat "$work/calls.pulumi-env" 2>/dev/null || true)
  gobin=$(cat "$work/calls.gobin" 2>/dev/null || true)
  goproxy=$(cat "$work/calls.goproxy" 2>/dev/null || true)
  list_env=$(cat "$work/calls.list-env" 2>/dev/null || true)
  list_gowork=$(cat "$work/calls.list-gowork" 2>/dev/null || true)
  tidy=$(cat "$work/calls.tidy" 2>/dev/null || true)
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
check "asks go env for GOPATH, GOMODCACHE and GOPROXY once, then runs the two installs and nothing else" \
  "$(is "$(grep -c . <<< "$calls")" 3)"
check "one go env call names all three" "$(contains "$calls" ' go env GOPATH GOMODCACHE GOPROXY$')"
check "in the file's order" "$(is "$(grep ' install ' <<< "$calls" | sed -n 1p | grep -c alpha)" 1)"
check "into legion-live-tools under GOPATH's first entry, never the caller's GOBIN" \
  "$(is "$(sort -u <<< "$gobin")" "GOBIN=$work/gopath/legion-live-tools")"
check "both installs there" "$(is "$(grep -c . <<< "$gobin")" 2)"
check "says where" "$(contains "$err" "installing 2 tools into $work/gopath/legion-live-tools")"
# go install PATH@VERSION reads the module's version list for its deprecation and retraction
# checks even at an exact pin: from the module cache's own download directory first, that list
# and every cached module need no network, and the proxy go env names serves only what the cache
# lacks (after a 404 or 410, the `,`).
check "each install reads the module cache's download directory first, then the proxy go env names" \
  "$(is "$(sort -u <<< "$goproxy")" "GOPROXY=file://$work/modcache/cache/download,https://proxy.example|direct")"
check "both installs so" "$(is "$(grep -c . <<< "$goproxy")" 2)"
check "says it reads the module cache first" "$(contains "$err" "the module cache $work/modcache first")"

echo "case: tools with GOPROXY=off installs from the module cache alone, and with no proxy named, the cache then Go's default"
TEST_GOPROXY=off fetch tools "$work/tools.txt"
check "exits 0" "$(is "$rc" 0)"
check "GOPROXY=off: the cache, then off" "$(is "$(sort -u <<< "$goproxy")" "GOPROXY=file://$work/modcache/cache/download,off")"
TEST_GOPROXY='' fetch tools "$work/tools.txt"
check "no proxy named: the cache, then Go's default" \
  "$(is "$(sort -u <<< "$goproxy")" "GOPROXY=file://$work/modcache/cache/download,https://proxy.golang.org,direct")"

echo "case: a go env that names no module cache ends the step before any install"
TEST_GOMODCACHE='' fetch tools "$work/tools.txt"
check "exits 1" "$(is "$rc" 1)"
check "says so" "$(contains "$err" 'go env GOMODCACHE names no module cache')"
check "installs nothing" "$(is "$(grep -c ' install ' <<< "$calls")" 0)"

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

echo "case: deps with -test lists the packages' tests' dependencies too"
fetch deps "$work/module" -test ./cmd/one
check "exits 0" "$(is "$rc" 0)"
check "passes -test to the listing" "$(is "$calls" "$work/module go list -deps -test ./cmd/one")"

echo "case: deps is tried again too"
FAIL_MATCH=list FAIL_TIMES=1 fetch deps "$work/module" ./cmd/one
check "exits 0" "$(is "$rc" 0)"
check "runs the listing twice" "$(is "$(grep -c 'list -deps' <<< "$calls")" 2)"

echo "case: with GO_FETCH_PROXY set, every go command runs with GOPROXY set to it, and the script says so"
TEST_GO_FETCH_PROXY='https://fetch.example|direct' TEST_ENV_GOPROXY=off fetch deps "$work/module" ./cmd/one
check "exits 0" "$(is "$rc" 0)"
check "the listing runs with GOPROXY=GO_FETCH_PROXY, never the job's off" \
  "$(is "$list_env" 'GOPROXY=https://fetch.example|direct GOOS= GOARCH= CGO_ENABLED=')"
check "says which proxy it fetches through" "$(contains "$err" '^go-fetch: fetching through GO_FETCH_PROXY, https://fetch.example|direct$')"
TEST_GO_FETCH_PROXY='https://fetch.example|direct' TEST_ENV_GOPROXY=off fetch tools "$work/tools.txt"
check "tools: each install reads the module cache first, then GO_FETCH_PROXY" \
  "$(is "$(sort -u <<< "$goproxy")" "GOPROXY=file://$work/modcache/cache/download,https://fetch.example|direct")"

echo "case: with GO_FETCH_PROXY unset and GOPROXY=off, the listing keeps the caller's GOPROXY, and the script says the cache alone serves it"
TEST_ENV_GOPROXY=off fetch deps "$work/module" ./cmd/one
check "exits 0" "$(is "$rc" 0)"
check "the listing runs with the caller's GOPROXY" "$(is "$list_env" 'GOPROXY=off GOOS= GOARCH= CGO_ENABLED=')"
check "says the module cache alone serves the fetch" "$(contains "$err" 'GO_FETCH_PROXY is unset and GOPROXY is off, so the module cache alone serves this fetch')"

# The file mode reads its paths from the repository root two directories above the script, so
# its cases run a copy of the script in a repository of their own.
repo="$work/repo"
mkdir -p "$repo/.github/scripts" "$repo/mod/a" "$repo/mod/b" "$repo/infra" "$repo/fetch" "$repo/tidymod" "$repo/nosum"
printf 'module example.com/tidymod\n\ngo 1.27.3\n\nrequire example.com/dep v1.0.0\n' > "$repo/tidymod/go.mod"
printf 'example.com/dep v1.0.0 h1:AAAA=\nexample.com/dep v1.0.0/go.mod h1:BBBB=\n' > "$repo/tidymod/go.sum"
printf 'module example.com/nosum\n\ngo 1.27.3\n' > "$repo/nosum/go.mod"
cp "$go_fetch" "$repo/.github/scripts/go-fetch.sh"
cp "$work/tools.txt" "$repo/fetch/tools.txt"
cat > "$repo/fetch/job.txt" <<'FETCH'
# The job's fetch: a comment line, a blank one, and a comment after a command.

deps mod -test ./a/... ./b   # the tests' modules
GOOS=darwin GOARCH=arm64 deps mod -test -tags e2e ./...
CGO_ENABLED=0 deps infra ./cmd/x
tools fetch/tools.txt
deps mod ./a
FETCH

echo "case: file runs each line of the fetch file in its turn, every path read from the repository root"
SCRIPT="$repo/.github/scripts/go-fetch.sh" fetch file "$repo/fetch/job.txt"
check "exits 0" "$(is "$rc" 0)"
check "lists the first line's packages in mod, its comment left off" \
  "$(is "$(grep ' list ' <<< "$calls" | sed -n 1p)" "$repo/mod go list -deps -test ./a/... ./b")"
check "lists the second line's for its platform" \
  "$(is "$(grep ' list ' <<< "$calls" | sed -n 2p)" "$repo/mod go list -deps -test -tags e2e ./...")"
check "with the second line's GOOS and GOARCH" \
  "$(is "$(sed -n 2p <<< "$list_env")" 'GOPROXY= GOOS=darwin GOARCH=arm64 CGO_ENABLED=')"
check "the third line in infra with its CGO_ENABLED, and no GOOS the line before gave" \
  "$(is "$(sed -n 3p <<< "$list_env")" 'GOPROXY= GOOS= GOARCH= CGO_ENABLED=0')"
check "the third line's listing in infra" \
  "$(is "$(grep ' list ' <<< "$calls" | sed -n 3p)" "$repo/infra go list -deps ./cmd/x")"
check "installs the tools file's two pins" "$(is "$(grep -c ' install ' <<< "$calls")" 2)"
check "reads the tools file from the repository root" "$(contains "$err" 'installing 2 tools')"
check "the last line after the tools, with no setting of an earlier line" \
  "$(is "$(sed -n 4p <<< "$list_env")" 'GOPROXY= GOOS= GOARCH= CGO_ENABLED=')"
check "in the file's order: four listings and two installs" \
  "$(is "$(grep -oE ' (list|install) ' <<< "$calls" | tr -d ' ' | paste -sd, -)" "list,list,list,install,install,list")"
check "names each line it runs" "$(contains "$err" "^go-fetch: $repo/fetch/job.txt:4: GOOS=darwin GOARCH=arm64 deps mod -test -tags e2e ./...$")"

echo "case: a fetch file line that fails every attempt fails the run, naming it, and runs no later line"
FAIL_MATCH=./cmd/x FAIL_TIMES=99 GO_FETCH_ATTEMPTS=2 SCRIPT="$repo/.github/scripts/go-fetch.sh" fetch file "$repo/fetch/job.txt"
check "exits 1" "$(is "$rc" 1)"
check "names the command that failed" "$(contains "$err" 'failed 2 times: go list -deps ./cmd/x')"
check "installs nothing after it" "$(is "$(grep -c ' install ' <<< "$calls")" 0)"

sha() { sha256sum < "$1" | cut -d' ' -f1; }
tidy_mod=$(sha "$repo/tidymod/go.mod") tidy_sum=$(sha "$repo/tidymod/go.sum")
printf 'tidy tidymod  # what check-go-tidy.sh reads\n' > "$repo/fetch/tidy.txt"

echo "case: a tidy line runs go mod tidy in the module on a copy of its go.mod and go.sum, and leaves the module's own two files as they were"
TEST_GO_FETCH_PROXY='https://fetch.example|direct' TEST_ENV_GOPROXY=off SCRIPT="$repo/.github/scripts/go-fetch.sh" \
  fetch file "$repo/fetch/tidy.txt"
check "exits 0" "$(is "$rc" 0)"
check "runs one go command: go mod tidy in the module, its go.mod and go.sum those of a copy" \
  "$(contains "$calls" "^$repo/tidymod go mod tidy -modfile=/[^ ]*/go.mod$")"
check "and nothing else" "$(is "$(grep -c . <<< "$calls")" 1)"
check "the copy is the module's go.mod" "$(contains "$tidy" " go.mod=$tidy_mod ")"
check "and its go.sum" "$(contains "$tidy" " go.sum=$tidy_sum$")"
check "through GO_FETCH_PROXY, never the job's off" "$(contains "$tidy" '^GOPROXY=https://fetch.example|direct ')"
check "the module's go.mod is as it was" "$(is "$(sha "$repo/tidymod/go.mod")" "$tidy_mod")"
check "and its go.sum" "$(is "$(sha "$repo/tidymod/go.sum")" "$tidy_sum")"
copy=$(sed -n 's/.* modfile=\([^ ]*\) .*/\1/p' <<< "$tidy")
check "the copy is outside the repository" "$(is "$(grep -c "^$repo/" <<< "$copy")" 0)"
check "and removed after" "$(is "$(test -e "$(dirname "$copy")" && echo there || echo gone)" gone)"
check "names the line it runs" "$(contains "$err" "^go-fetch: $repo/fetch/tidy.txt:1: tidy tidymod$")"

echo "case: a tidy line of a module with no go.sum gives tidy a copy with none"
printf 'tidy nosum\n' > "$repo/fetch/nosum.txt"
SCRIPT="$repo/.github/scripts/go-fetch.sh" fetch file "$repo/fetch/nosum.txt"
check "exits 0" "$(is "$rc" 0)"
check "the copy is the module's go.mod" "$(contains "$tidy" " go.mod=$(sha "$repo/nosum/go.mod") ")"
check "and has no go.sum" "$(contains "$tidy" ' go.sum=none$')"
check "the module still has none" "$(is "$(test -e "$repo/nosum/go.sum" && echo there || echo none)" none)"

echo "case: a tidy that fails is tried again on a fresh copy"
FAIL_MATCH='mod tidy' FAIL_TIMES=1 SCRIPT="$repo/.github/scripts/go-fetch.sh" fetch file "$repo/fetch/tidy.txt"
check "exits 0" "$(is "$rc" 0)"
check "runs tidy twice" "$(is "$(grep -c ' go mod tidy ' <<< "$calls")" 2)"
check "the second on the module's go.mod, not what the first wrote" \
  "$(is "$(sed -n 2p <<< "$tidy" | grep -c " go.mod=$tidy_mod ")" 1)"
check "and on its go.sum" "$(is "$(sed -n 2p <<< "$tidy" | grep -c " go.sum=$tidy_sum$")" 1)"
check "says it tries again" "$(contains "$err" 'failed; trying again')"

echo "case: a tidy that fails every attempt fails the run, naming the module, and removes the copy"
FAIL_MATCH='mod tidy' FAIL_TIMES=99 GO_FETCH_ATTEMPTS=2 SCRIPT="$repo/.github/scripts/go-fetch.sh" fetch file "$repo/fetch/tidy.txt"
check "exits 1" "$(is "$rc" 1)"
check "names the module whose tidy failed" "$(contains "$err" "failed 2 times: tidy_on_copy $repo/tidymod$")"
copy=$(sed -n '1s/.* modfile=\([^ ]*\) .*/\1/p' <<< "$tidy")
check "the copy is removed" "$(is "$(test -e "$(dirname "$copy")" && echo there || echo gone)" gone)"
check "the module's go.mod and go.sum are as they were" \
  "$(is "$(sha "$repo/tidymod/go.mod") $(sha "$repo/tidymod/go.sum")" "$tidy_mod $tidy_sum")"

echo "case: every line of a fetch file is checked before the first runs"
for bad in 'deps mod' 'deps absent ./a' 'GOFLAGS=-mod=mod deps mod ./a' 'GOOS=Darwin deps mod ./a' 'CGO_ENABLED=2 deps mod ./a' \
    'GOWORK=auto deps mod ./a' 'GOWORK=off tools fetch/tools.txt' \
    'tools fetch/absent.txt' 'GOOS=linux tools fetch/tools.txt' 'tools fetch/tools.txt extra' 'list mod ./a' 'go list -deps ./a' \
    'tidy' 'tidy tidymod extra' 'GOOS=linux tidy tidymod' 'tidy mod' 'tidy absent' 'GOWORK=off tidy tidymod'; do
  printf 'deps mod ./a\n%s\n' "$bad" > "$repo/fetch/bad.txt"
  SCRIPT="$repo/.github/scripts/go-fetch.sh" fetch file "$repo/fetch/bad.txt"
  check "'$bad' exits 2" "$(is "$rc" 2)"
  check "'$bad' is named by its file and line" "$(contains "$err" "$repo/fetch/bad.txt:2: ")"
  check "'$bad' runs nothing, not even the good line before it" "$(is "$calls" '')"
done

echo "case: GOWORK=off on a deps line lists that module outside the go.work above it, and no other line"
cat > "$repo/fetch/gowork.txt" <<'FETCH'
GOWORK=off deps mod ./a
deps mod ./b
FETCH
SCRIPT="$repo/.github/scripts/go-fetch.sh" fetch file "$repo/fetch/gowork.txt"
gowork=$list_gowork
check "exits 0" "$(is "$rc" 0)"
check "the first listing runs with GOWORK=off" "$(is "$(sed -n 1p <<< "$gowork")" 'GOWORK=off')"
check "the second with no GOWORK the line before gave" "$(is "$(sed -n 2p <<< "$gowork")" 'GOWORK=')"
check "names the line with its setting" "$(contains "$err" "^go-fetch: $repo/fetch/gowork.txt:1: GOWORK=off deps mod ./a$")"
printf 'example.com/c/cmd/gamma@latest\n' > "$repo/fetch/badpins.txt"
printf 'deps mod ./a\ntools fetch/badpins.txt\n' > "$repo/fetch/bad.txt"
SCRIPT="$repo/.github/scripts/go-fetch.sh" fetch file "$repo/fetch/bad.txt"
check "a tools file whose line is no pin exits 2" "$(is "$rc" 2)"
check "and runs nothing" "$(is "$calls" '')"
printf '# nothing\n\n' > "$repo/fetch/empty.txt"
SCRIPT="$repo/.github/scripts/go-fetch.sh" fetch file "$repo/fetch/empty.txt"
check "a file that fetches nothing exits 2" "$(is "$rc" 2)"
check "and says so" "$(contains "$err" 'fetches nothing')"
SCRIPT="$repo/.github/scripts/go-fetch.sh" fetch file "$repo/fetch/absent.txt"
check "a missing file exits 2" "$(is "$rc" 2)"
SCRIPT="$repo/.github/scripts/go-fetch.sh" fetch file
check "file with no FILE exits 2" "$(is "$rc" 2)"

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
# go-offline: the pattern a stand-in's recorded call is matched against; it starts no go command.
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
