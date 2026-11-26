#!/usr/bin/env bash
# Tests for go_legs_select.py and go-legs-select.sh: which legs and packages a change selects, and
# which reads outside a package --check-reads refuses, on fixture trees.
#
# Each case runs on a repository under a temporary directory built here: two modules (mod, with
# packages a, shared, b and leaf; tool, one package), a .github/go-legs.yaml with a job for each
# (unit, two legs, the second with a step of its own; tool-unit, one leg), a ci.yaml with an
# affected job and a job of no legs, a legs workflow for each job, and copies of the scripts the
# selection runs. A change is an edit of the working tree, measured from the commit the tree was
# built at (`--base HEAD`, which reads the working tree), and is undone after each case. a imports
# shared; b imports a module no module cache holds (example.com/far, which GOPROXY=off leaves
# unloaded, as the selection's go commands do every third-party module); b's test declares, in a
# //ci:reads line, the file fixtures/data.json; leaf imports nothing.
#
# Run from anywhere: .github/scripts/go-legs-select.test.sh
# CI runs it in ci.yaml's test job, after setup-go.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
export GOWORK=off GOFLAGS='' GOPROXY=off GOTOOLCHAIN=local
export GIT_AUTHOR_NAME=fixture GIT_AUTHOR_EMAIL=fixture@example.com GIT_COMMITTER_NAME=fixture
export GIT_COMMITTER_EMAIL=fixture@example.com

put() { mkdir -p "$(dirname "$1")" && printf '%s\n' "$2" > "$1"; }

# tree <name>: the fixture repository under $work/<name>, committed; echoes its root.
tree() {
  local root="$work/$1"
  mkdir -p "$root/.github/scripts"
  for script in go_legs_select.py go-legs-select.sh go_legs.py ci_filter_coverage.py ci_filter_sources.py \
                go_build_tags.py workflow_paths.py; do
    cp "$script_dir/$script" "$root/.github/scripts/"
  done
  cd "$root"
  put .github/go-legs.yaml 'mod:
  unit:
    1:
      - a
      - shared
    2:
      - b
      - leaf
  unlisted: unit
tool:
  tool-unit:
    1:
      - .
  unlisted: tool-unit'
  put mod/go.mod 'module example.com/mod

go 1.21

require example.com/far v1.0.0'
  put mod/go.sum 'example.com/far v1.0.0 h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=
example.com/far v1.0.0/go.mod h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA='
  put mod/shared/shared.go 'package shared

func S() int { return 1 }'
  put mod/shared/more.go 'package shared

func More() int { return 2 }'
  put mod/shared/testdata/case.json '{}'
  put mod/a/a.go 'package a

import "example.com/mod/shared"

var _ = shared.S'
  put mod/a/a_test.go 'package a

import "testing"

func TestA(t *testing.T) {}'
  put mod/b/b.go 'package b

import _ "example.com/far/x"'
  put mod/b/b_test.go 'package b

//ci:reads fixtures/data.json TestB reads the shared fixture

import "testing"

func TestB(t *testing.T) {}'
  put mod/leaf/leaf.go 'package leaf'
  put mod/leaf/leaf_test.go 'package leaf

import "testing"

func TestLeaf(t *testing.T) {}'
  put fixtures/data.json '{}'
  put tool/go.mod 'module example.com/tool

go 1.21'
  put tool/main.go 'package main

func main() {}'
  put .github/scripts/leg-two.sh '#!/bin/sh
echo leg two'
  put .github/workflows/ci.yaml 'name: ci
on: [pull_request]
env:
  CI_MERGE_GROUP_SELECTS: "false"
jobs:
  affected:
    runs-on: ubuntu-24.04
    timeout-minutes: 10
    steps:
      - run: .github/scripts/go-legs-select.sh
  other:
    runs-on: ubuntu-24.04
    timeout-minutes: 5
    steps:
      - run: echo other
  unit:
    needs: [affected]
    uses: ./.github/workflows/legs.yaml
    with:
      affected-packages: ${{ needs.affected.outputs.affected_packages }}
  tool-unit:
    needs: [affected]
    uses: ./.github/workflows/tool-legs.yaml'
  put .github/workflows/legs.yaml 'name: legs
on:
  workflow_call:
    inputs:
      affected-packages:
        type: string
        default: ""
env:
  GOTOOLCHAIN: local
jobs:
  unit-legs:
    runs-on: ubuntu-24.04
    timeout-minutes: 10
    strategy:
      matrix:
        leg: [1, 2]
    steps:
      # The leg'"'"'s packages.
      - name: Run the leg
        working-directory: mod
        run: python3 ../.github/scripts/go_legs.py mod unit ${{ matrix.leg }} ${{ strategy.job-total }} | xargs -r go test -count=1
      - name: A step of leg two alone
        if: matrix.leg == 2
        run: .github/scripts/leg-two.sh'
  put .github/workflows/tool-legs.yaml 'name: tool legs
on:
  workflow_call:
jobs:
  tool-unit-legs:
    runs-on: ubuntu-24.04
    timeout-minutes: 10
    strategy:
      matrix:
        leg: [1]
    steps:
      - name: Run the leg
        working-directory: tool
        run: python3 ../.github/scripts/go_legs.py tool tool-unit ${{ matrix.leg }} ${{ strategy.job-total }} | xargs -r go test -count=1'
  git init -q && git add -A && git commit -qm fixture
  cd - >/dev/null
  echo "$root"
}

root=$(tree fixture)
cd "$root"

# select: the selection of the working tree's change from HEAD, as JSON; why goes to $work/said.
select_change() { python3 .github/scripts/go_legs_select.py --base HEAD 2>"$work/said"; }
undo() { git reset -q --hard && git clean -qfd; }
# field <json> <job> <legs|packages>: the job's legs (space-separated) or packages (space-separated,
# or `all`).
field() {
  python3 -c 'import json, sys
v = json.loads(sys.argv[1])[sys.argv[2]][sys.argv[3]]
print(v if v == "all" else " ".join(map(str, v)))' "$1" "$2" "$3"
}
# expect <label> <json> <unit legs> <unit packages> <tool-unit packages>
expect() {
  check "$1: unit starts legs [$3]" "$(is "$(field "$2" unit legs)" "$3")"
  check "$1: unit runs [$4]" "$(is "$(field "$2" unit packages)" "$4")"
  check "$1: tool-unit runs [$5]" "$(is "$(field "$2" tool-unit packages)" "$5")"
}

echo "case: no change selects no package, and starts only the leg that carries a step of its own"
expect "nothing" "$(select_change)" "2" "" ""

echo "case: a shared package's source selects it and every package that imports it, and no other"
echo '// a change' >> mod/shared/shared.go
expect "shared.go" "$(select_change)" "1 2" "a shared" ""
undo

echo "case: a leaf package's own test file selects that package alone"
echo '// a change' >> mod/leaf/leaf_test.go
expect "leaf_test.go" "$(select_change)" "2" "leaf" ""
undo

echo "case: a file of a package's testdata selects every package that imports it, as its source does"
echo '{"x": 1}' > mod/shared/testdata/case.json
expect "shared/testdata" "$(select_change)" "1 2" "a shared" ""
undo

echo "case: a file deleted from a package selects by its directory"
git rm -q mod/shared/more.go
expect "more.go deleted" "$(select_change)" "1 2" "a shared" ""
undo

echo "case: a file only a //ci:reads line names selects that line's package alone"
echo '{"x": 1}' > fixtures/data.json
expect "fixtures/data.json" "$(select_change)" "2" "b" ""
check "the reason names the line's glob" "$(contains "$(cat "$work/said")" '  b: fixtures/data.json, which a //ci: line.s glob fixtures/data.json names')"
undo

echo "case: a file no test reads selects no package"
echo 'notes' > CHANGELOG.md
git add -N CHANGELOG.md
expect "CHANGELOG.md" "$(select_change)" "2" "" ""
undo

echo "case: a package no line of go-legs.yaml names is selected, and starts its job's last leg"
put mod/fresh/fresh.go 'package fresh'
git add -N mod/fresh/fresh.go
expect "a new package" "$(select_change)" "2" "fresh" ""
undo

echo "case: go-legs.yaml selects the packages whose place it changes, and a comment there none"
printf '# a comment\n' >> .github/go-legs.yaml
expect "a comment in go-legs.yaml" "$(select_change)" "2" "" ""
undo
python3 - <<'PY'
p = ".github/go-legs.yaml"
t = open(p).read()
moved = t.replace("      - b\n      - leaf\n", "      - b\n", 1).replace("      - shared\n", "      - shared\n      - leaf\n", 1)
assert moved != t, "the move changed nothing"
open(p, "w").write(moved)
PY
check "the move changed go-legs.yaml" "$(is "$(git diff --quiet -- .github/go-legs.yaml && echo same || echo changed)" changed)"
expect "leaf moved to leg 1" "$(select_change)" "1 2" "leaf" ""
undo

echo "case: a legs workflow selects every package of its job where it defines the legs, and a comment none"
sed -i "s/# The leg's packages./# The packages of the leg./" .github/workflows/legs.yaml
check "the comment edit changed legs.yaml" "$(is "$(git diff --quiet && echo same || echo changed)" changed)"
expect "a comment in legs.yaml" "$(select_change)" "2" "" ""
undo
sed -i 's/xargs -r go test -count=1$/xargs -r go test -count=2/' .github/workflows/legs.yaml
expect "the legs step" "$(select_change)" "1 2" "all" ""
undo
sed -i 's/^  GOTOOLCHAIN: local$/  GOTOOLCHAIN: local\n  GOFLAGS: -tags=fixture/' .github/workflows/legs.yaml
check "the env edit changed legs.yaml" "$(is "$(git diff --quiet && echo same || echo changed)" changed)"
expect "the legs workflow's top-level env" "$(select_change)" "1 2" "all" ""
undo
printf '#!/bin/sh\necho leg two, changed\n' > .github/scripts/leg-two.sh
expect "a script a legs step runs" "$(select_change)" "1 2" "all" ""
undo

echo "case: ci.yaml selects every package where it defines the legs (the affected job), and no package elsewhere"
sed -i 's/      - run: echo other/      - run: echo another/' .github/workflows/ci.yaml
expect "ci.yaml's other job" "$(select_change)" "2" "" ""
undo
sed -i 's/    timeout-minutes: 10$/    timeout-minutes: 11/' .github/workflows/ci.yaml
expect "ci.yaml's affected job" "$(select_change)" "1 2" "all" "all"
undo

echo "case: a go.mod change beyond its requirements selects every package of every job"
sed -i 's/^go 1.21$/go 1.21\n\ngodebug default=go1.21/' mod/go.mod
expect "a godebug line" "$(select_change)" "1 2" "all" "all"
undo

echo "case: a requirement or go.sum line selects every package that compiles a module outside the repository"
sed -i 's/example.com\/far v1.0.0$/example.com\/far v1.1.0/' mod/go.mod
expect "a requirement" "$(select_change)" "2" "b" ""
undo
printf 'example.com/near v1.0.0 h1:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB=\n' >> mod/go.sum
expect "a go.sum line" "$(select_change)" "2" "b" ""
undo

echo "case: a //ci:builds line selects its package for what the build compiles, third-party modules too"
python3 - <<'PY'
p = "mod/leaf/leaf_test.go"
t = open(p).read()
open(p, "w").write(t.replace("package leaf\n", "package leaf\n\n//ci:builds mod ./b TestLeaf builds b\n", 1))
PY
git commit -qam "leaf builds b"
echo '// a change' >> mod/b/b.go
expect "b.go, which leaf builds" "$(select_change)" "2" "b leaf" ""
undo
sed -i 's/example.com\/far v1.0.0$/example.com\/far v1.1.0/' mod/go.mod
expect "a requirement, which leaf's build compiles" "$(select_change)" "2" "b leaf" ""
undo
git reset -q --hard HEAD~1

echo "case: --check-reads refuses a read outside its package that no //ci: line names"
check_reads() { python3 .github/scripts/go_legs_select.py --check-reads 2>&1; }
status=0
out=$(check_reads) || status=$?
check "the fixture passes" "$(is "$status" 0)"
put mod/leaf/leaf_test.go 'package leaf

import (
	"os"
	"testing"
)

func TestLeaf(t *testing.T) { _, _ = os.ReadFile("../shared/testdata/case.json") }'
status=0
out=$(check_reads) || status=$?
check "a file of another package's testdata, no line: refused" "$(is "$status" 1)"
check "naming the read and the line to add" "$(contains "$out" 'mod/leaf/leaf_test.go:8: reads ../shared/testdata/case.json (mod/shared/testdata/case.json).*//ci:reads mod/shared/testdata/case.json')"
sed -i 's/^package leaf$/package leaf\n\n\/\/ci:reads mod\/shared\/testdata\/case.json TestLeaf reads it/' mod/leaf/leaf_test.go
status=0
out=$(check_reads) || status=$?
check "with the line: passes" "$(is "$status" 0)"
echo '{"y": 1}' > mod/shared/testdata/case.json
expect "case.json, which leaf now reads" "$(select_change)" "1 2" "a shared leaf" ""
undo
put mod/leaf/leaf_test.go 'package leaf

import (
	"io/fs"
	"path/filepath"
	"testing"
)

func TestLeaf(t *testing.T) {
	_ = filepath.WalkDir("..", func(string, fs.DirEntry, error) error { return nil })
}'
status=0
out=$(check_reads) || status=$?
check "a walk above its own directory, no line: refused" "$(is "$status" 1)"
check "naming the walk" "$(contains "$out" 'mod/leaf/leaf_test.go:10: walks .. (mod), above its own directory')"
sed -i 's/^package leaf$/package leaf\n\n\/\/ci:reads mod\/**\/*.go TestLeaf walks the module'"'"'s source/' mod/leaf/leaf_test.go
status=0
out=$(check_reads) || status=$?
check "with a line naming what it walks: passes" "$(is "$status" 0)"
undo
put mod/leaf/leaf_test.go 'package leaf

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLeaf(t *testing.T) { _, _ = os.ReadDir(filepath.Join("..", "shared")) }'
status=0
out=$(check_reads) || status=$?
check "another package's directory, no line: refused" "$(is "$status" 1)"
sed -i 's/^package leaf$/package leaf\n\n\/\/ci:builds mod .\/shared TestLeaf reads the package it builds/' mod/leaf/leaf_test.go
status=0
out=$(check_reads) || status=$?
check "a //ci:builds line of that directory: passes" "$(is "$status" 0)"
undo

echo "case: go-legs-select.sh measures each event from its own base"
wrapper() { env -u GITHUB_OUTPUT "$@" .github/scripts/go-legs-select.sh 2>&1 | sed -n 's/^affected_packages: //p'; }
every=$(python3 .github/scripts/go_legs_select.py --every)
echo '// a change' >> mod/shared/shared.go
git commit -qam "change shared"
check "a dispatch of main: every package" "$(is "$(wrapper GITHUB_EVENT_NAME=workflow_dispatch GITHUB_REF=refs/heads/main)" "$every")"
check "a merge group while the switch is off: every package" \
  "$(is "$(wrapper GITHUB_EVENT_NAME=merge_group GO_LEGS_BASE="$(git rev-parse HEAD~1)" CI_MERGE_GROUP_SELECTS=false)" "$every")"
out=$(wrapper GITHUB_EVENT_NAME=merge_group GO_LEGS_BASE="$(git rev-parse HEAD~1)" CI_MERGE_GROUP_SELECTS=true)
check "a merge group while the switch is on: the entry's own change" "$(is "$(field "$out" unit packages)" "a shared")"
out=$(wrapper GITHUB_EVENT_NAME=pull_request GITHUB_SHA="$(git rev-parse HEAD)")
check "a pull request: the merge commit against its first parent" "$(is "$(field "$out" unit packages)" "a shared")"
check "an event it reads no change of: every package" "$(is "$(wrapper GITHUB_EVENT_NAME=push)" "$every")"
git reset -q --hard HEAD~1

summary "go_legs_select.py and go-legs-select.sh"
