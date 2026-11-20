#!/usr/bin/env bash
# Tests for `go-build-offline.sh`: go_build_offline, the Go build every test launcher runs from the
# module cache alone. GOPROXY=off reaches the go command; a build that succeeds hands on what go
# said; a build the module cache cannot serve fails, naming the fetch of the same module and
# packages; and a build that fails for any other reason fails naming no fetch.
#
# Each case sources the real file, linked into a fixture repository so the root it derives from
# its own path is the fixture's, and calls the function in a shell of its own under a stand-in
# `go` first on PATH. The stand-in records every call (its directory, the GOPROXY it was given and
# its arguments) and answers as the case asks: a success with a compiler's note, a success with
# nothing to say, go's refusal of a module the cache lacks under GOPROXY=off, or a compile error.
# So what is tested is the function the launchers call, with no network and no Go toolchain.
#
# Run from anywhere: .github/scripts/go-build-offline.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# The fixture repository, holding the file under test where it lives in this one, a module inside
# it and a module outside it.
repo="$work/repo"
lib="$repo/.github/scripts/go-build-offline.sh"
mkdir -p "$repo/.github/scripts" "$repo/packages/mod/cmd" "$work/elsewhere/mod" "$work/caller"
ln -s "$script_dir/go-build-offline.sh" "$lib"

# The stand-in go. Its refusal is the line go1.27.3 prints for a build whose module is not in the
# cache under GOPROXY=off.
mkdir -p "$work/bin"
cat > "$work/bin/go" <<'STANDIN'
#!/usr/bin/env bash
echo "$PWD GOPROXY=${GOPROXY-unset} $*" >> "$GO_BUILD_TEST_CALLS"
case "$GO_BUILD_TEST_ANSWER" in
  note)
    echo "# example.com/mod/cmd/one" >&2
    echo "cmd/one/main.go:3:2: a note from the compiler" >&2
    ;;
  quiet) ;;
  offline)
    echo "cmd/one/main.go:4:2: example.com/dep@v1.2.3: module lookup disabled by GOPROXY=off" >&2
    exit 1
    ;;
  broken)
    echo "cmd/one/main.go:5:1: syntax error: non-declaration statement outside function body" >&2
    exit 1
    ;;
esac
STANDIN
chmod +x "$work/bin/go"

# build ANSWER FROM ARGS...: in a shell of its own that has sourced the file under test, cd to
# FROM and call `go_build_offline ARGS...` under the stand-in answering ANSWER, with the caller's
# GOPROXY exported as a proxy the build must not use, or, with NO_GOPROXY set, not in the
# caller's environment at all, as on a CI runner. Leaves the call's stdout in `out`, its stderr in
# `err`, its status in `rc`, the stand-in's calls in `calls` and the directory the caller's shell
# is in after the call in `after`. PLAIN, when set, makes the call a statement of its own under
# `set -e`, as e2e-local.sh calls it, and the shell prints `went on` after it; otherwise the call
# is the left side of an `||`, as the launchers call it.
build() {
  local answer=$1 from=$2
  shift 2
  rm -f "$work/calls" "$work/calls.after"
  set +e
  # shellcheck disable=SC2016 # the shell below expands its own arguments
  (cd "$from" || exit 1
    if [ -n "${NO_GOPROXY:-}" ]; then unset GOPROXY; else export GOPROXY=https://proxy.golang.org,direct; fi
    PATH="$work/bin:$PATH" \
    GO_BUILD_TEST_CALLS="$work/calls" GO_BUILD_TEST_ANSWER="$answer" PLAIN="${PLAIN:-}" bash -c '
      set -euo pipefail
      source "$1"
      shift
      status=0
      if [ -n "$PLAIN" ]; then
        go_build_offline "$@"
        echo "went on"
      else
        go_build_offline "$@" || status=$?
      fi
      pwd > "$GO_BUILD_TEST_CALLS.after"
      exit "$status"
    ' _ "$lib" "$@") > "$work/stdout" 2> "$work/stderr"
  rc=$?
  set -e
  out=$(cat "$work/stdout")
  err=$(cat "$work/stderr")
  calls=$(cat "$work/calls" 2>/dev/null || true)
  after=$(cat "$work/calls.after" 2>/dev/null || true)
}

hint_lead='^The module cache lacks a module this build needs, and a test fetches no module while it runs\. Fetch them before the tests, from the repository root:$'

echo "case: a build that succeeds runs go build in the module, named from the repository root, with GOPROXY=off"
build note "$work/caller" packages/mod "$work/out/one" ./cmd/one ./cmd/two
check "exits 0" "$(is "$rc" 0)"
check "runs go build -o OUT PACKAGE... in the module, with GOPROXY=off over the caller's proxy, once" \
  "$(is "$calls" "$repo/packages/mod GOPROXY=off build -o $work/out/one ./cmd/one ./cmd/two")"
check "hands on what go said, on stderr" "$(contains "$err" '^cmd/one/main.go:3:2: a note from the compiler$')"
check "all of it" "$(contains "$err" '^# example.com/mod/cmd/one$')"
check "names no fetch" "$(is "$(grep -c 'go-fetch' <<< "$err")" 0)"
check "writes nothing on stdout" "$(is "$out" '')"
check "leaves the caller in its own directory" "$(is "$after" "$work/caller")"

echo "case: a caller with no GOPROXY of its own builds with GOPROXY=off too"
NO_GOPROXY=1 build quiet "$work/caller" packages/mod "$work/out/one" ./cmd/one
check "exits 0" "$(is "$rc" 0)"
check "the go command alone is given GOPROXY=off" \
  "$(is "$calls" "$repo/packages/mod GOPROXY=off build -o $work/out/one ./cmd/one")"

echo "case: a module named from the repository root is the repository's wherever the caller is"
build quiet "$repo/packages/mod/cmd" packages/mod ./out/one ./cmd/one
check "exits 0" "$(is "$rc" 0)"
check "runs go in the repository's module, OUT as given" \
  "$(is "$calls" "$repo/packages/mod GOPROXY=off build -o ./out/one ./cmd/one")"

echo "case: a build that succeeds with nothing to say prints nothing"
build quiet "$work/caller" packages/mod "$work/out/" ./cmd/one
check "exits 0" "$(is "$rc" 0)"
check "prints nothing on stderr" "$(is "$err" '')"
check "and nothing on stdout" "$(is "$out" '')"

echo "case: a build the module cache cannot serve fails, naming the fetch of this module and these packages"
build offline "$work/caller" packages/mod "$work/out/" ./cmd/one ./cmd/two
check "fails" "$(is "$rc" 1)"
check "shows go's own refusal" \
  "$(contains "$err" '^cmd/one/main.go:4:2: example.com/dep@v1.2.3: module lookup disabled by GOPROXY=off$')"
check "says the module cache lacks a module, and a test fetches none" "$(contains "$err" "$hint_lead")"
check "names go-fetch.sh deps of the module from the repository root and every package given" \
  "$(contains "$err" '^  \.github/scripts/go-fetch\.sh deps packages/mod \./cmd/one \./cmd/two$')"
check "go's own output first, then the two lines of the fetch, and nothing else" \
  "$(is "$err" "$(printf '%s\n%s\n  %s' \
    'cmd/one/main.go:4:2: example.com/dep@v1.2.3: module lookup disabled by GOPROXY=off' \
    'The module cache lacks a module this build needs, and a test fetches no module while it runs. Fetch them before the tests, from the repository root:' \
    '.github/scripts/go-fetch.sh deps packages/mod ./cmd/one ./cmd/two')")"
check "writes nothing on stdout" "$(is "$out" '')"

echo "case: a module given by its absolute path inside the repository is named from the root"
build offline "$work/caller" "$repo/packages/mod" "$work/out/" ./cmd/one
check "fails" "$(is "$rc" 1)"
check "runs go in that module" "$(is "$calls" "$repo/packages/mod GOPROXY=off build -o $work/out/ ./cmd/one")"
check "names the module from the repository root" \
  "$(contains "$err" '^  \.github/scripts/go-fetch\.sh deps packages/mod \./cmd/one$')"

echo "case: a module outside the repository is named whole"
build offline "$work/caller" "$work/elsewhere/mod" "$work/out/" ./cmd/one
check "fails" "$(is "$rc" 1)"
check "runs go in that module" "$(is "$calls" "$work/elsewhere/mod GOPROXY=off build -o $work/out/ ./cmd/one")"
check "names the module by its whole path" \
  "$(contains "$err" "^  \.github/scripts/go-fetch\.sh deps $work/elsewhere/mod \./cmd/one\$")"

echo "case: a build that fails for any other reason names no fetch"
build broken "$work/caller" packages/mod "$work/out/" ./cmd/one
check "fails" "$(is "$rc" 1)"
check "shows go's own error" \
  "$(contains "$err" '^cmd/one/main.go:5:1: syntax error: non-declaration statement outside function body$')"
check "and nothing else" "$(is "$(grep -c . <<< "$err")" 1)"
check "names no fetch" "$(is "$(grep -c 'go-fetch' <<< "$err")" 0)"

echo "case: a plain call under set -e that fails stops its caller, after the fetch is named"
PLAIN=1 build offline "$work/caller" packages/mod "$work/out/" ./cmd/one
check "the caller exits 1" "$(is "$rc" 1)"
check "and goes no further" "$(is "$out" '')"
check "having named the fetch" "$(contains "$err" '^  \.github/scripts/go-fetch\.sh deps packages/mod \./cmd/one$')"

echo "case: a plain call under set -e that succeeds lets its caller go on"
PLAIN=1 build quiet "$work/caller" packages/mod "$work/out/" ./cmd/one
check "the caller exits 0" "$(is "$rc" 0)"
check "and goes on" "$(is "$out" 'went on')"

echo "case: a module directory that is not there fails, runs no go and names no fetch"
build note "$work/caller" packages/absent "$work/out/" ./cmd/one
check "fails" "$(is "$rc" 1)"
check "runs no go" "$(is "$calls" '')"
check "says the directory is not there" "$(contains "$err" 'packages/absent: No such file or directory')"
check "names no fetch" "$(is "$(grep -c 'go-fetch' <<< "$err")" 0)"

echo "case: a call without a module, an output and a package is refused"
build note "$work/caller" packages/mod "$work/out/"
check "exits 2" "$(is "$rc" 2)"
check "names what it takes" "$(contains "$err" '^go_build_offline: takes MODULE OUTPUT PACKAGE\.\.\., got 2 arguments$')"
check "runs no go" "$(is "$calls" '')"

summary "go_build_offline builds with GOPROXY=off in the module given, hands on what go said, and names the fetch of that module and those packages only when the module cache lacked one"
