#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
work=$(mktemp -d)
# The module cache go writes is read-only unless -modcacherw (below); the chmod is for a run that
# stopped before that took effect.
trap 'chmod -R u+w "$work" 2>/dev/null || true; rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# Every go command here: a GOPATH, module cache and build cache of the harness's own, the toolchain
# this runs on, no workspace from outside, the job env's -mod=readonly, and module cache files
# left writable so the trap can remove them.
export GOPATH="$work/gopath" GOMODCACHE="$work/gopath/pkg/mod" GOCACHE="$work/gocache"
export GOFLAGS="-mod=readonly -modcacherw" GOTOOLCHAIN=local GOSUMDB=off
unset GOWORK GOOS GOARCH GOPRIVATE GONOPROXY GONOSUMDB

# proxy_module MODULE VERSION SOURCE: SOURCE's files as MODULE@VERSION in the file proxy under
# $work/proxy, in the layout go's GOPROXY protocol reads: the .info, .mod and .zip of the version.
proxy_module() {
  local at="$work/proxy/$1/@v"
  mkdir -p "$at"
  printf '{"Version":"%s"}\n' "$2" > "$at/$2.info"
  cp "$3/go.mod" "$at/$2.mod"
  printf '%s\n' "$2" >> "$at/list"
  python3 -c '
import pathlib, sys, zipfile
archive, prefix, source = sys.argv[1:]
with zipfile.ZipFile(archive, "w") as z:
    for path in sorted(pathlib.Path(source).rglob("*")):
        if path.is_file():
            z.write(path, f"{prefix}/{path.relative_to(source).as_posix()}")
' "$at/$2.zip" "$1@$2" "$3"
}

mkdir -p "$work/src/dep2" "$work/src/dep1" "$work/template"
printf 'module example.com/dep2\n\ngo 1.21\n' > "$work/src/dep2/go.mod"
printf 'package dep2\n\nfunc Two() int { return 2 }\n' > "$work/src/dep2/dep2.go"
printf 'module example.com/dep1\n\ngo 1.21\n\nrequire example.com/dep2 v1.0.0\n' > "$work/src/dep1/go.mod"
printf 'package dep1\n\nimport "example.com/dep2"\n\nfunc One() int { return dep2.Two() - 1 }\n' > "$work/src/dep1/dep1.go"
proxy_module example.com/dep2 v1.0.0 "$work/src/dep2"
proxy_module example.com/dep1 v1.0.0 "$work/src/dep1"
# go-offline: the harness's own file proxy under $work is its one source; it reaches no network.
(cd "$work" && GOPROXY="file://$work/proxy" go mod download example.com/dep1@v1.0.0 example.com/dep2@v1.0.0)

printf 'module example.com/app\n\ngo 1.21\n\nrequire example.com/dep1 v1.0.0\n' > "$work/template/go.mod"
printf 'package app\n\nimport "example.com/dep1"\n\nfunc Three() int { return dep1.One() + 2 }\n' > "$work/template/app.go"
(cd "$work/template" && GOPROXY=off go mod tidy)
if ! grep -q '^require example.com/dep2 v1.0.0 // indirect$' "$work/template/go.mod" ||
  [ "$(grep -c '^example.com/dep[12] v1.0.0' "$work/template/go.sum")" != 4 ]; then
  echo "FAIL: the template module is not the tidy module the cases need:" >&2
  cat "$work/template/go.mod" "$work/template/go.sum" >&2
  exit 1
fi

fixture() {
  local root="$work/trees/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    exit 1
  fi
  mkdir -p "$root/.github/scripts" "$root/.github/go-fetch"
  cp "$script_dir/check-go-tidy.sh" "$root/.github/scripts/check-go-tidy.sh"
  echo "$root"
}

module() {
  mkdir -p "$1/$2"
  cp "$work/template/go.mod" "$work/template/go.sum" "$work/template/app.go" "$1/$2/"
  [ -z "${3:-}" ] || sed -i "s|^module .*|module $3|" "$1/$2/go.mod"
}

untidy_sum() { sed -i '/^example.com\/dep2 v1.0.0 h1:/d' "$1/$2/go.sum"; }
untidy_mod() { sed -i '/example.com\/dep2 v1.0.0 \/\/ indirect/d' "$1/$2/go.mod"; }

run_check() {
  set +e
  out=$(cd "$work" && GOPROXY=https://127.0.0.1:1 GOSUMDB='sum.golang.org https://127.0.0.1:1' \
    "$1/.github/scripts/check-go-tidy.sh" 2>&1)
  status=$?
  set -e
}

echo "case: a tidy module a fetch file line builds passes"
root=$(fixture tidy)
module "$root" mod
printf '# the job fetch\ndeps mod -test ./...  # its tests\n' > "$root/.github/go-fetch/job.txt"
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "reads the one module" "$(contains "$out" '^check-go-tidy: 1 modules a required job builds (the deps and tidy lines of 1 fetch files): mod$')"
check "finds it tidy" "$(contains "$out" '^tidy: mod$')"
check "and says every module is" "$(contains "$out" '^check-go-tidy: every one of the 1 modules is tidy$')"
check "asks neither the proxy nor the checksum database" "$(is "$(grep -c '127\.0\.0\.1' <<<"$out")" 0)"

echo "case: a module whose go.sum lacks a line fails, named, with the line tidy would add"
root=$(fixture missing-sum)
module "$root" mod
untidy_sum "$root" mod
printf 'deps mod ./...\n' > "$root/.github/go-fetch/job.txt"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the module, as a GitHub error on its go.mod" \
  "$(contains "$out" '^::error file=mod/go.mod::mod is not tidy: go mod tidy -diff exits 1\. Run `go mod tidy` in mod and commit its go.mod and go.sum\.')"
check "shows the go.sum line tidy would add" "$(contains "$out" '^    +example.com/dep2 v1.0.0 h1:')"
check "and no change to go.mod" "$(is "$(grep -c '^    +++ .*go\.mod' <<<"$out")" 0)"
check "asks the checksum database nothing for the line" "$(is "$(grep -c '127\.0\.0\.1' <<<"$out")" 0)"
check "counts it" "$(contains "$out" '^::error::check-go-tidy: 1 of the 1 modules are not tidy, and 0 fetch file lines place no module$')"

echo "case: a module whose go.mod lacks an indirect requirement fails, named, with the requirement tidy would add"
root=$(fixture missing-indirect)
module "$root" mod
untidy_mod "$root" mod
printf 'deps mod ./...\n' > "$root/.github/go-fetch/job.txt"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the module" "$(contains "$out" '^::error file=mod/go.mod::mod is not tidy')"
check "shows the requirement tidy would add" "$(contains "$out" '^    +require example.com/dep2 v1.0.0 // indirect$')"

echo "case: a go.mod no fetch file line reaches is not read, untidy as it is"
root=$(fixture unread)
module "$root" mod
module "$root" docs/proto example.com/proto
untidy_sum "$root" docs/proto
untidy_mod "$root" docs/proto
printf 'deps mod ./...\n' > "$root/.github/go-fetch/job.txt"
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "reads the one module a line reaches" "$(contains "$out" '^check-go-tidy: 1 modules a required job builds (the deps and tidy lines of 1 fetch files): mod$')"
check "names nothing of the other" "$(is "$(grep -c 'docs/proto' <<<"$out")" 0)"

echo "case: a deps line in a workspace reads every module its go.work uses"
root=$(fixture workspace)
module "$root" mod
module "$root" lib example.com/lib
untidy_sum "$root" lib
printf 'go 1.21\n\nuse (\n\t./mod\n\t./lib\n)\n' > "$root/go.work"
printf 'deps mod ./...\n' > "$root/.github/go-fetch/job.txt"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "reads both modules of the workspace" "$(contains "$out" '^check-go-tidy: 2 modules a required job builds (the deps and tidy lines of 1 fetch files): lib mod$')"
check "names the untidy one, which no line names" "$(contains "$out" '^::error file=lib/go.mod::lib is not tidy')"
check "finds the other tidy" "$(contains "$out" '^tidy: mod$')"

echo "case: every module is read, a tidy line's and a line's behind its platform settings too, and each untidy one is named"
root=$(fixture each)
module "$root" one example.com/one
module "$root" two example.com/two
module "$root" three example.com/three
untidy_sum "$root" one
untidy_mod "$root" three
printf 'tidy one\n' > "$root/.github/go-fetch/a.txt"
printf 'GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 deps two -test ./...\ntools tools.txt\ndeps three .\n' > "$root/.github/go-fetch/b.txt"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "reads the three modules of the two files' lines" \
  "$(contains "$out" '^check-go-tidy: 3 modules a required job builds (the deps and tidy lines of 2 fetch files): one three two$')"
check "names the first untidy module" "$(contains "$out" '^::error file=one/go.mod::one is not tidy')"
check "and the second, after the first failed" "$(contains "$out" '^::error file=three/go.mod::three is not tidy')"
check "finds the tidy one" "$(contains "$out" '^tidy: two$')"
check "counts the two" "$(contains "$out" '^::error::check-go-tidy: 2 of the 3 modules are not tidy')"

echo "case: a module the module cache lacks fails, naming the module and the fetch line that fills it"
root=$(fixture uncached)
module "$root" mod
printf 'package app\n\nimport "example.com/absent"\n\nvar _ = absent.X\n' > "$root/mod/absent.go"
printf '\nrequire example.com/absent v1.0.0\n' >> "$root/mod/go.mod"
printf 'deps mod ./...\n' > "$root/.github/go-fetch/job.txt"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the module and the tidy line its job's fetch file needs" \
  "$(contains "$out" '^::error file=mod/go.mod::mod: go mod tidy -diff exits 1, the module cache lacking a module tidy reads .*with a `tidy mod` line of its fetch file')"
check "shows go's own line naming what the cache lacks, with the proxy off" \
  "$(contains "$out" 'example.com/absent: module lookup disabled by GOPROXY=off$')"
check "and never the proxy it was given" "$(is "$(grep -c '127\.0\.0\.1' <<<"$out")" 0)"

echo "case: a fetch file line whose directory holds no module fails, naming the line"
root=$(fixture no-module)
module "$root" mod
mkdir -p "$root/loose"
printf 'deps mod ./...\ndeps loose ./...\ndeps gone ./...\n' > "$root/.github/go-fetch/job.txt"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the line of a directory in no module" \
  "$(contains "$out" '^::error file=.github/go-fetch/job.txt,line=2::.github/go-fetch/job.txt:2 runs deps in loose, where go places no module')"
check "and the line of a directory that is not there" \
  "$(contains "$out" '^::error file=.github/go-fetch/job.txt,line=3::.github/go-fetch/job.txt:3 runs deps in gone, where go places no module: no such directory$')"
check "still reads the module the first line places" "$(contains "$out" '^tidy: mod$')"

echo "case: a tree in which the check finds no module reports UNKNOWN"
root=$(fixture no-fetch-file)
module "$root" mod
untidy_sum "$root" mod
run_check "$root"
check "no fetch file: exits 2" "$(is "$status" 2)"
check "says UNKNOWN, and what it read" \
  "$(contains "$out" '^UNKNOWN: check-go-tidy found no module to read: no deps or tidy line in the 0 fetch files under .github/go-fetch/ places one$')"
root=$(fixture tools-only)
module "$root" mod
untidy_sum "$root" mod
printf '# tools alone\ntools tools.txt\n' > "$root/.github/go-fetch/job.txt"
run_check "$root"
check "fetch files with no deps or tidy line: exits 2" "$(is "$status" 2)"
check "says UNKNOWN for its 1 fetch file" "$(contains "$out" '^UNKNOWN: .* in the 1 fetch files ')"
check "names no module as tidy" "$(is "$(grep -c '^tidy: ' <<<"$out")" 0)"

summary "check-go-tidy.sh reads every module of the workspaces its fetch files' lines build, and passes a tidy one, fails an untidy one by name with tidy's diff, and reports UNKNOWN when it finds none"
