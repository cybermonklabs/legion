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

# Its standard error goes to $work/stderr, which `err` prints: a case reads it after the call,
# since the call runs in the subshell of a command substitution.
# legs <root> <job> <leg> [<legs>] [<build flag>...]: go_legs.py's standard output for that leg of
# a matrix of <legs> legs (2 when not given), under the build flags after it, then `exit=<status>`.
legs() {
  local out status=0
  out=$(cd "$1/mod" && python3 "$1/.github/scripts/go_legs.py" mod "$2" "$3" "${4:-2}" "${@:5}" 2>"$work/stderr") || status=$?
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


# ---------------------------------------------------------- a package split by test across legs

# split_tree <name> <legs file text> <groups file text>: a tree as above with one package more,
# `many`, holding four tests (TestOne, TestOneMore, TestTwo, TestThree), a benchmark and an
# example, and the groups file .github/go-legs/mod/many.yaml holding that text. TestOneMore is
# there for the anchoring case: its name begins with TestOne's. Not named `split`, which is a
# command of coreutils.
split_tree() {
  local root
  root=$(tree "$1" "$2")
  mkdir -p "$root/mod/many" "$root/.github/go-legs/mod"
  cat > "$root/mod/many/many.go" <<'GO'
package many

import "fmt"

// Thing is what the example calls: an example names an identifier of its package, and one that
// names none fails the build rather than list.
func Thing() { fmt.Println("thing") }
GO
  cat > "$root/mod/many/many_test.go" <<'GO'
package many

import (
	"fmt"
	"testing"
)

func TestOne(t *testing.T)     { fmt.Println("ran TestOne") }
func TestOneMore(t *testing.T) { fmt.Println("ran TestOneMore") }
func TestTwo(t *testing.T)     { fmt.Println("ran TestTwo") }
func TestThree(t *testing.T)   { fmt.Println("ran TestThree") }

func BenchmarkThing(b *testing.B) {}

func ExampleThing() {
	Thing()
	// Output: thing
}
GO
  # A test only a -race build holds, so a case can read whether the listing ran under the leg's
  # own build flags: exactly-once is over the tests the leg builds, and a listing under other
  # flags is a listing of another set.
  cat > "$root/mod/many/many_race_test.go" <<'GO'
//go:build race

package many

import "testing"

func TestUnderRace(t *testing.T) {}
GO
  printf '%s\n' "$3" > "$root/.github/go-legs/mod/many.yaml"
  echo "$root"
}

# legs_for <the job's legs, indented>: a legs file for `mod` with those legs and the testonly
# package elsewhere, so each case below writes only the legs it is about.
legs_for() {
  printf 'mod:\n  job:\n%s\n  elsewhere:\n    testonly: another-job\n' "$1"
}

# group_run <root> <leg> <legs>: the go test a leg's step really runs, its printed arguments and
# all, verbose, so a case reads which tests ran.
group_run() {
  (cd "$1/mod" && set -o pipefail \
    && python3 ../.github/scripts/go_legs.py mod job "$2" "$3" 2>/dev/null | xargs go test -count=1 -v 2>&1) || true
}

# placed <name> <groups...>: the group go_legs.py places a test no group names in, computed here
# as that script computes it, so a case names no number of its own.
placed() {
  local name="$1"; shift
  python3 -c 'import sys, zlib; print(sorted(int(g) for g in sys.argv[2:])[zlib.crc32(sys.argv[1].encode()) % (len(sys.argv) - 2)])' "$name" "$@"
}

two_group_legs=$(legs_for '    1:
      - many: 1
    2:
      - many: 2
    3:
      - b
      - a
      - b/c')
whole_groups='1:
  - TestOne
  - ExampleThing
2:
  - TestOneMore
  - TestTwo
  - TestThree'

echo "case: a package whose tests are split into groups, one per leg"
root=$(split_tree grouped "$two_group_legs" "$whole_groups")
check "the leg prints its group's -run before the package" \
  "$(is "$(legs "$root" job 1 3)" "-run=^(TestOne|ExampleThing)\$
./many/
exit=0")"
check "and says which group it is and how many of the package's tests" \
  "$(contains "$(err)" 'leg 1 of job runs group 1 of mod/many.s tests, 2 of its 5 (.github/go-legs/mod/many.yaml)')"
check "the other group's leg prints its own" \
  "$(is "$(legs "$root" job 2 3)" "-run=^(TestOneMore|TestTwo|TestThree)\$
./many/
exit=0")"
check "the leg of whole packages prints packages alone" \
  "$(is "$(legs "$root" job 3 3)" "./b/
./a/
./b/c/
exit=0")"
echo "case: the listing runs under the leg's own build flags"
# The fixture holds TestUnderRace behind //go:build race: without -race it is in no listing, so
# no group need name it; with -race it is a test of the package and a group must run it.
check "without -race the tagged test is in no group and nothing says otherwise" \
  "$(is "$(contains "$(legs "$root" job 1 3)" 'TestUnderRace')" false)"
check "with -race the leg places it and runs it" \
  "$(contains "$(legs "$root" job "$(placed TestUnderRace 1 2)" 3 -race)" 'TestUnderRace')"
check "and says so as a warning" "$(contains "$(err)" 'TestUnderRace is a test of mod/many')"

echo "case: the group's -run is anchored, so a test whose name begins with another's runs in one leg"
# The step is `go_legs.py … | xargs go test …`, so the real go test runs what the leg printed.
first=$(group_run "$root" 1 3)
check "TestOne runs in its own leg" "$(contains "$first" '^ran TestOne$')"
check "and TestOneMore does not" "$(is "$(printf '%s\n' "$first" | grep -c '^ran TestOneMore$')" "0")"
second=$(group_run "$root" 2 3)
check "TestOneMore runs in the leg its group is in" "$(contains "$second" '^ran TestOneMore$')"
check "and TestOne does not" "$(is "$(printf '%s\n' "$second" | grep -c '^ran TestOne$')" "0")"

echo "case: a test no group names is placed, in the same group in every leg"
# The list goes stale by the hour (61 of 115 main commits in a week changed this package's tests),
# so a test no group names is no error: every leg places it by its name alone and says so.
root=$(split_tree placed-test "$two_group_legs" '1:
  - TestOne
  - ExampleThing
2:
  - TestOneMore
  - TestTwo')
mine=$(placed TestThree 1 2)
other=$(( mine == 1 ? 2 : 1 ))
check "its leg passes" "$(contains "$(legs "$root" job "$mine" 3)" '^exit=0$')"
check "and runs it" "$(contains "$(legs "$root" job "$mine" 3)" '^-run=.*TestThree')"
check "with a warning naming the test and the group it went to" \
  "$(contains "$(err)" "^::warning file=.github/go-legs/mod/many.yaml::TestThree is a test of mod/many (.go test -list ..) that no group of .github/go-legs/mod/many.yaml names, so group $mine runs it")"
check "the other leg passes too" "$(contains "$(legs "$root" job "$other" 3)" '^exit=0$')"
check "and does not run it" "$(is "$(contains "$(legs "$root" job "$other" 3)" 'TestThree')" false)"
check "the real go test of the leg that has it runs it" \
  "$(contains "$(group_run "$root" "$mine" 3)" '^ran TestThree$')"
check "and the other leg's does not" \
  "$(is "$(printf '%s\n' "$(group_run "$root" "$other" 3)" | grep -c '^ran TestThree$')" "0")"
check "the leg of whole packages is untouched" "$(contains "$(legs "$root" job 3 3)" '^exit=0$')"

echo "case: a group naming a test the package no longer has is a warning, not an error"
root=$(split_tree gone-test "$two_group_legs" '1:
  - TestOne
  - TestGone
  - ExampleThing
2:
  - TestOneMore
  - TestTwo
  - TestThree')
check "the leg passes" "$(contains "$(legs "$root" job 1 3)" '^exit=0$')"
check "and leaves the name out of its -run" "$(is "$(contains "$(legs "$root" job 1 3)" 'TestGone')" false)"
check "saying it names a test the listing does not print" \
  "$(contains "$(err)" '^::warning file=.github/go-legs/mod/many.yaml::group 1 of .github/go-legs/mod/many.yaml names TestGone, which .go test -list .. does not print as a test of mod/many, so it runs nothing')"
root=$(split_tree benchmark "$two_group_legs" '1:
  - TestOne
  - BenchmarkThing
  - ExampleThing
2:
  - TestOneMore
  - TestTwo
  - TestThree')
check "a benchmark is no test of a group" "$(contains "$(legs "$root" job 1 3)" '^exit=0$')"
check "and warns" "$(contains "$(err)" '^::warning file=.github/go-legs/mod/many.yaml::group 1 .* names BenchmarkThing')"

echo "case: a test two groups name"
root=$(split_tree twice-grouped "$two_group_legs" '1:
  - TestOne
  - TestTwo
  - ExampleThing
2:
  - TestOneMore
  - TestTwo
  - TestThree')
out=$(legs "$root" job 1 3)
check "fails" "$(contains "$out" '^exit=1$')"
check "prints nothing for the leg's go test" "$(is "$out" "
exit=1")"
check "naming the test and its groups" \
  "$(contains "$(err)" '^::error file=.github/go-legs/mod/many.yaml::TestTwo is in groups 1, 2 of .github/go-legs/mod/many.yaml, so it would run in each: keep it in one')"
check "and fails the other group's leg too" "$(contains "$(legs "$root" job 2 3)" '^exit=1$')"
check "while the leg of whole packages still runs" "$(contains "$(legs "$root" job 3 3)" '^exit=0$')"

echo "case: a group with no test"
root=$(split_tree empty-group "$two_group_legs" '1:
  - TestOne
  - TestOneMore
  - TestTwo
  - TestThree
  - ExampleThing
2: []')
check "fails" "$(contains "$(legs "$root" job 2 3)" '^exit=1$')"
check "naming the group" "$(contains "$(err)" 'gives group 2 no test the package has, so its leg would run none')"

echo "case: a group whose names have all gone from the package"
# Its -run would be ^()$, which matches nothing: the leg would pass having run no test, so the
# emptiness is read on what the leg would really run, not on what the file lists.
root=$(split_tree all-stale "$two_group_legs" '1:
  - TestGoneOne
  - TestGoneTwo
2:
  - TestOne
  - TestOneMore
  - TestTwo
  - TestThree
  - ExampleThing')
check "fails" "$(contains "$(legs "$root" job 1 3)" '^exit=1$')"
check "naming the group" "$(contains "$(err)" 'gives group 1 no test the package has, so its leg would run none')"

echo "case: a group no leg runs, and a leg running a group the file does not have"
root=$(split_tree group-gone "$two_group_legs" '1:
  - TestOne
  - ExampleThing
2:
  - TestOneMore
  - TestTwo
3:
  - TestThree')
check "fails" "$(contains "$(legs "$root" job 1 3)" '^exit=1$')"
check "naming the legs' groups and the file's" \
  "$(contains "$(err)" "the legs of .github/go-legs.yaml run groups 1, 2 of mod/many's tests, and .github/go-legs/mod/many.yaml has groups 1, 2, 3")"
check "as an error on the legs file" "$(contains "$(err)" '^::error file=.github/go-legs.yaml::the legs of')"
root=$(split_tree group-missing "$(legs_for '    1:
      - many: 1
    2:
      - many: 2
    3:
      - many: 3
    4:
      - b
      - a
      - b/c')" "$whole_groups")
check "and a leg whose group the file does not have fails" "$(contains "$(legs "$root" job 3 4)" '^exit=1$')"
check "naming both" "$(contains "$(err)" "run groups 1, 2, 3 of mod/many's tests, and .github/go-legs/mod/many.yaml has groups 1, 2")"

echo "case: one group in two legs, and a group beside another package"
root=$(split_tree group-twice "$(legs_for '    1:
      - many: 1
    2:
      - many: 1
    3:
      - b
      - a
      - b/c')" "$whole_groups")
check "the same group in two legs fails" "$(contains "$(legs "$root" job 1 3)" '^exit=1$')"
check "naming the group and the legs" \
  "$(contains "$(err)" 'group 1 of mod/many.s tests is in 2 legs of .github/go-legs.yaml (job leg 1, group 1, job leg 2, group 1)')"
root=$(split_tree group-with-package "$(legs_for '    1:
      - many: 1
      - a
    2:
      - many: 2
    3:
      - b
      - b/c')" "$whole_groups")
check "a group beside another package fails" "$(contains "$(legs "$root" job 1 3)" '^exit=1$')"
check "saying the -run would choose that package's tests too" \
  "$(contains "$(err)" "a group of a package's tests beside other packages, and the group's -run would choose the tests of all of them")"

echo "case: a package in a group of one job and whole in another leg"
root=$(split_tree group-and-whole "$(legs_for '    1:
      - many: 1
    2:
      - many: 2
    3:
      - b
      - a
      - b/c
      - many')" "$whole_groups")
check "fails" "$(contains "$(legs "$root" job 1 3)" '^exit=1$')"
check "naming every place" "$(contains "$(err)" 'mod/many is in 3 places of .github/go-legs.yaml')"

echo "case: a leg entry of neither shape"
root=$(split_tree bad-entry "$(legs_for '    1:
      - many:
          1: 2
    2:
      - many: 2
    3:
      - b
      - a
      - b/c')" "$whole_groups")
check "fails" "$(contains "$(legs "$root" job 1 3)" '^exit=1$')"
check "naming the entry, the leg it is in, and the two shapes" \
  "$(contains "$(err)" 'gives job job leg 1 the entry .* which is neither `- <package>` nor `- <package>: <group>`')"
check "and says nothing about a group beside another package" \
  "$(is "$(contains "$(err)" 'beside other packages')" false)"

echo "case: a split package with no groups file"
root=$(split_tree no-groups-file "$two_group_legs" "$whole_groups")
rm "$root/.github/go-legs/mod/many.yaml"
check "fails" "$(contains "$(legs "$root" job 1 3)" '^exit=1$')"
check "naming the file to write" \
  "$(contains "$(err)" '.github/go-legs/mod/many.yaml, which holds the groups of mod/many.s tests that its legs run, is not there')"

echo "case: a test whose name is not ASCII"
# Go accepts any rune that is not lower case after Test, and `go test -list` prints it: a listing
# that dropped such a name would leave it in no group and in no leg's -run, with nothing to say so.
root=$(split_tree unicode-name "$two_group_legs" "$whole_groups")
cat >> "$root/mod/many/many_test.go" <<'GO'

func TestÄnderung(t *testing.T) { fmt.Println("ran TestÄnderung") }
GO
mine=$(placed "TestÄnderung" 1 2)
check "its leg places and runs it" "$(contains "$(legs "$root" job "$mine" 3)" 'TestÄnderung')"
check "and the real go test runs it" "$(contains "$(group_run "$root" "$mine" 3)" '^ran TestÄnderung$')"

echo "case: a group whose -run pattern is over the limit"
# go test hands the pattern to the test binary as one argument, and xargs starts a second command
# with what does not fit: a group that large must be split rather than quietly half-run. 1,200
# names of 93 characters make a pattern of 112,803 bytes.
root=$(split_tree long-pattern "$two_group_legs" "$whole_groups")
awk 'BEGIN {
  long = ""; for (i = 0; i < 20; i++) long = long "Long"
  print "package many"; print ""; print "import \"testing\""; print ""
  for (i = 0; i < 1200; i++) printf "func TestA%sName%04d(t *testing.T) {}\n", long, i
}' > "$root/mod/many/many_long_test.go"
awk 'BEGIN {
  long = ""; for (i = 0; i < 20; i++) long = long "Long"
  print "1:"
  for (i = 0; i < 1200; i++) printf "  - TestA%sName%04d\n", long, i
  print "2:"
  print "  - TestOne"; print "  - TestOneMore"; print "  - TestTwo"; print "  - TestThree"
  print "  - ExampleThing"
}' > "$root/.github/go-legs/mod/many.yaml"
check "fails" "$(contains "$(legs "$root" job 1 3)" '^exit=1$')"
check "naming the group and the size of its pattern" \
  "$(contains "$(err)" 'group 1 of .github/go-legs/mod/many.yaml makes a -run pattern of 112,803 bytes')"
check "and the limit, with what to do" \
  "$(contains "$(err)" 'over the 100,000 go test and xargs can take as one argument: split the package into more groups')"

echo "case: a build flag that is no flag at all"
root=$(split_tree flag-usage "$two_group_legs" "$whole_groups")
status=0
(cd "$root/mod" && python3 ../.github/scripts/go_legs.py mod job 1 3 race >/dev/null 2>&1) || status=$?
check "is a usage error" "$(is "$status" 2)"

# ------------------------------------------------------- the rebalancer that writes a groups file

echo "case: go_legs_groups.py writes a groups file the legs then run"
# The writer and the reader are tested together: the file this writes is the file go_legs.py reads,
# so a change to the format that passes one and not the other fails here.
root=$(split_tree rebalanced "$two_group_legs" "$whole_groups")
cp "$script_dir/go_legs_groups.py" "$root/.github/scripts/"
# A listing as `go test -list .` prints one, and a run as `go test -json` reports one: TestTwo is
# the long test, and the three parallel ones weigh a quarter of their seconds each.
cat > "$work/listing.txt" <<'LIST'
TestOne
TestOneMore
TestTwo
TestThree
ExampleThing
ok  	example.com/mod/many	0.1s
LIST
python3 - "$work/run.json" <<'PY'
import json, sys
events = [{"Action": "run", "Package": "p", "Test": name} for name in
          ("TestOne", "TestOneMore", "TestTwo", "TestThree", "ExampleThing")]
events += [{"Action": "pause", "Package": "p", "Test": name} for name in ("TestOne", "TestOneMore", "TestThree")]
events += [{"Action": "pass", "Package": "p", "Test": "TestTwo", "Elapsed": 40.0},
           {"Action": "pass", "Package": "p", "Test": "TestOne", "Elapsed": 20.0},
           {"Action": "pass", "Package": "p", "Test": "TestOneMore", "Elapsed": 16.0},
           {"Action": "pass", "Package": "p", "Test": "TestThree", "Elapsed": 8.0},
           {"Action": "pass", "Package": "p", "Test": "ExampleThing", "Elapsed": 1.0}]
open(sys.argv[1], "w").write("\n".join(json.dumps(event) for event in events) + "\n")
PY
out=$(cd "$root" && python3 .github/scripts/go_legs_groups.py mod many 2 4 "$work/listing.txt" "a fixture run" "$work/run.json" 2>&1)
check "it says what it dealt where" "$(contains "$out" 'go_legs_groups.py: 5 tests in 2 groups, 0 untimed')"
check "the long serial test is a group of its own" \
  "$(contains "$out" '1: 40 s (1 tests)')"
check "and the rest are the other" "$(contains "$out" '2: 12 s (4 tests)')"
check "the file it wrote names each test's seconds" \
  "$(contains "$(cat "$root/.github/go-legs/mod/many.yaml")" '^  - TestTwo  # 40.0 s$')"
check "and points at where the rules are written" \
  "$(contains "$(cat "$root/.github/go-legs/mod/many.yaml")" 'ci-job-daemon-go.md')"
check "the legs run what it wrote" "$(is "$(legs "$root" job 1 3)" "-run=^(TestTwo)\$
./many/
exit=0")"
check "and the other leg the rest" "$(is "$(legs "$root" job 2 3)" "-run=^(ExampleThing|TestOne|TestOneMore|TestThree)\$
./many/
exit=0")"

echo "case: the rebalancer reads a run whose -json a wrapper printed as lines"
# The document service's lane runs its go test under .github/scripts/race_memory.py, which reads
# the -json stream itself and writes its Output fields back out.
cat > "$work/verbose.txt" <<'LOG'
race memory: the limit is 8388608 kB (RACE_MEM_LIMIT_KB)
=== RUN   TestTwo
=== RUN   TestOne
=== PAUSE TestOne
=== CONT  TestOne
--- PASS: TestTwo (40.00s)
--- PASS: TestOne (20.00s)
--- PASS: TestOneMore (16.00s)
--- PASS: TestThree (8.00s)
--- PASS: ExampleThing (1.00s)
ok  	example.com/mod/many	41.000s
LOG
out=$(cd "$root" && python3 .github/scripts/go_legs_groups.py mod many 2 4 "$work/listing.txt" "a fixture run" "$work/verbose.txt" 2>&1)
check "it reads every test's seconds from the lines" "$(contains "$out" '5 tests in 2 groups, 0 untimed')"
check "and the test that paused weighs a quarter" "$(contains "$out" '1: 40 s (1 tests)')"
status=0
printf 'nothing of the kind\n' > "$work/empty.txt"
(cd "$root" && python3 .github/scripts/go_legs_groups.py mod many 2 4 "$work/listing.txt" "a fixture run" "$work/empty.txt" >/dev/null 2>&1) || status=$?
check "a run that times nothing is refused rather than balanced from nothing" "$(is "$status" 1)"

summary "go_legs.py runs each package of its module in exactly one place, and each test of a package it splits in exactly one group"
