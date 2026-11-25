#!/usr/bin/env bash
# Tests for go_legs.py: the packages it prints for a leg, and each way a legs file fails to run
# every package of its module exactly once in the legs the job's matrix launches (a package in no
# leg or in two, a leg naming a directory that is no package, a leg the matrix does not run, a leg
# with no package), which stops the leg before its go test runs. A job key no matrix job runs is
# ci_job_reads.py's hold to refuse (hold_daemon_legs), since a leg sees only its own job's legs;
# check-ci-filters.test.sh holds that one.
#
# Each case builds a small repository: a copy of go_legs.py (which reads the repository it sits
# in), a .github/go-legs.yaml, and a Go module `mod` with no requirement, so `go list ./...` runs
# with no network and no module cache. The module holds packages a, b, b/c and testonly (only a
# _test.go file, which go list counts as a package), and tagged, whose one file a build constraint
# excludes, which go list does not.
#
# Run from anywhere: .github/scripts/go-legs.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# The go command reads none of the caller's workspace, flags or proxy: the fixture is all it sees.
export GOWORK=off GOFLAGS='' GOPROXY=off GOTOOLCHAIN=local

# tree <name> <legs file text>: a repository under $work/<name> holding that legs file. Echoes its root.
tree() {
  local root="$work/$1"
  mkdir -p "$root/.github/scripts" "$root/mod/a" "$root/mod/b/c" "$root/mod/testonly" "$root/mod/tagged"
  cp "$script_dir/go_legs.py" "$root/.github/scripts/"
  printf 'module example.com/mod\n\ngo 1.21\n' > "$root/mod/go.mod"
  printf 'package a\n' > "$root/mod/a/a.go"
  printf 'package b\n' > "$root/mod/b/b.go"
  printf 'package c\n' > "$root/mod/b/c/c.go"
  printf 'package testonly\n\nimport "testing"\n\nfunc TestX(t *testing.T) {}\n' > "$root/mod/testonly/x_test.go"
  printf '//go:build never\n\npackage tagged\n' > "$root/mod/tagged/t.go"
  printf '%s\n' "$2" > "$root/.github/go-legs.yaml"
  echo "$root"
}

# legs <root> <job> <leg> [<legs>]: go_legs.py's standard output for that leg of a matrix of
# <legs> legs (2 when not given), then `exit=<status>`. Its standard error goes to $work/stderr,
# which `err` prints: a case reads it after the call, since the call runs in the subshell of a
# command substitution.
legs() {
  local out status=0
  out=$(cd "$1/mod" && python3 "$1/.github/scripts/go_legs.py" mod "$2" "$3" "${4:-2}" 2>"$work/stderr") || status=$?
  printf '%s\nexit=%s\n' "$out" "$status"
}
err() { cat "$work/stderr"; }

whole='mod:
  job:
    1:
      - b
      - a
    2:
      - b/c
  elsewhere:
    testonly: another-job'

echo "case: a file that runs every package once"
root=$(tree whole "$whole")
check "leg 1 prints its packages in the file's order" "$(is "$(legs "$root" job 1)" "./b/
./a/
exit=0")"
check "and says how many of the module's packages they are" "$(contains "$(err)" "leg 1 of job runs 2 of the 4 packages of mod")"
check "leg 2 prints its own" "$(is "$(legs "$root" job 2)" "./b/c/
exit=0")"

echo "case: a package no place names (a new package, or one taken out of a leg)"
root=$(tree dropped 'mod:
  job:
    1:
      - b
      - a
    2: []
  elsewhere:
    testonly: another-job')
out=$(legs "$root" job 1)
check "fails" "$(contains "$out" '^exit=1$')"
check "prints nothing for the leg's go test" "$(is "$out" "
exit=1")"
check "names the package and what to do" "$(contains "$(err)" 'mod/b/c is a package `go list ./...` prints, and no leg of .github/go-legs.yaml runs it: add it to the leg of the job that should run it')"
check "as an error on the legs file" "$(contains "$(err)" '^::error file=.github/go-legs.yaml::')"
check "and fails every leg, not only the one that lost it" "$(contains "$(legs "$root" job 2)" '^exit=1$')"

echo "case: a package two places name"
root=$(tree twice 'mod:
  job:
    1:
      - b
      - a
    2:
      - b/c
      - a
  elsewhere:
    testonly: another-job')
out=$(legs "$root" job 2)
check "fails" "$(contains "$out" '^exit=1$')"
check "names the package and both places" "$(contains "$(err)" 'mod/a is in 2 places of .github/go-legs.yaml (job leg 1, job leg 2), so it would run in each: keep it in one')"

echo "case: a package named both in a leg and elsewhere"
root=$(tree leg-and-elsewhere 'mod:
  job:
    1:
      - b
      - a
      - testonly
    2:
      - b/c
  elsewhere:
    testonly: another-job')
legs "$root" job 1 >/dev/null
check "names both places" "$(contains "$(err)" 'mod/testonly is in 2 places of .github/go-legs.yaml (job leg 1, elsewhere (another-job))')"

echo "case: a place that names no package (a directory a build constraint empties, or a typo)"
root=$(tree unknown 'mod:
  job:
    1:
      - b
      - a
      - tagged
    2:
      - b/c
  elsewhere:
    testonly: another-job')
out=$(legs "$root" job 1)
check "fails" "$(contains "$out" '^exit=1$')"
check "names the directory and where the file names it" "$(contains "$(err)" '.github/go-legs.yaml names mod/tagged (job leg 1), which `go list ./...` does not print as a package')"

echo "case: a leg the file has and the job's matrix does not run"
# The file and the matrix each say how many legs there are, so a package moved into a new leg 3
# that the matrix does not launch would run in no job while every package is still in the file
# once.
root=$(tree unlaunched 'mod:
  job:
    1:
      - b
      - a
    2:
      - testonly
    3:
      - b/c
  elsewhere: {}')
out=$(legs "$root" job 1 2)
check "fails" "$(contains "$out" '^exit=1$')"
check "prints nothing for the leg's go test" "$(is "$out" "
exit=1")"
check "names the file's legs and the matrix's" "$(contains "$(err)" 'gives job job legs 1, 2, 3, and its matrix runs legs 1 to 2: a leg the file has and the matrix does not runs in no job')"
check "the same file passes where the matrix runs all three" "$(contains "$(legs "$root" job 3 3)" '^exit=0$')"
root=$(tree unlisted "$whole")
check "and a matrix leg the file does not have fails every leg" "$(contains "$(legs "$root" job 1 3)" '^exit=1$')"
check "naming both" "$(contains "$(err)" 'gives job job legs 1, 2, and its matrix runs legs 1 to 3')"

echo "case: a leg that lists no package"
# xargs hands go test no package, and go test with none tests the module's root.
root=$(tree empty 'mod:
  job:
    1:
      - b
      - a
      - b/c
    2: []
  elsewhere:
    testonly: another-job')
out=$(legs "$root" job 1)
check "fails" "$(contains "$out" '^exit=1$')"
check "names the leg" "$(contains "$(err)" 'gives leg 2 of job job no package, and xargs would hand go test none, which tests the module.s root')"
check "and the empty leg itself fails rather than print nothing" "$(contains "$(legs "$root" job 2)" '^exit=1$')"

echo "case: the leg count is required"
root=$(tree usage "$whole")
status=0
(cd "$root/mod" && python3 ../.github/scripts/go_legs.py mod job 1 >/dev/null 2>&1) || status=$?
check "three arguments are a usage error" "$(is "$status" 2)"
status=0
(cd "$root/mod" && python3 ../.github/scripts/go_legs.py mod job 1 0 >/dev/null 2>&1) || status=$?
check "and so is a matrix of no legs" "$(is "$status" 2)"

echo "case: a leg or a job the file does not have"
root=$(tree whole2 "$whole")
check "a leg past the last is a usage error" "$(contains "$(legs "$root" job 3)" '^exit=2$')"
check "naming it" "$(contains "$(err)" 'has no leg 3 of job job under mod')"
check "so is a job" "$(contains "$(legs "$root" other 1)" '^exit=2$')"
check "and elsewhere is no job" "$(contains "$(legs "$root" elsewhere 1)" '^exit=2$')"

echo "case: a go list that fails"
root=$(tree broken "$whole")
printf 'package a\n\nimport "example.com/missing/dep"\n' > "$root/mod/a/a.go"
printf 'not a go.mod\n' > "$root/mod/go.mod"
out=$(legs "$root" job 1)
check "fails rather than compare with nothing" "$(contains "$out" '^exit=1$')"
check "saying the packages are unknown" "$(contains "$(err)" 'failed (its errors are above), so which packages the legs must run is unknown')"

echo "case: the leg's step, a pipe to xargs under pipefail, fails when the check fails"
root=$(tree step 'mod:
  job:
    1:
      - b
    2: []
  elsewhere:
    testonly: another-job')
status=0
ran=$(cd "$root/mod" && set -o pipefail && python3 ../.github/scripts/go_legs.py mod job 1 2 2>/dev/null | xargs echo ran) || status=$?
check "the pipeline fails" "$(is "$status" 1)"
check "and xargs hands its command no package" "$(is "$ran" "ran")"

summary "go_legs.py runs each package of its module in exactly one place"
