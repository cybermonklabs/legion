#!/usr/bin/env bash
# Tests for `go-test-shards.py`: the split of one Go package's tests into shards, and the check
# that the shards' -run patterns select every test exactly once.
#
# Each case builds a miniature Go module under a temporary directory and runs the real script
# over it, so the script under test is the file CI runs, the binary is a real `go test -c` build
# and every pattern is matched by Go's own regexp. The controls are the check's reason to exist:
# a test no pattern selects, and a test two patterns select, must each fail it by name, and the
# patterns the split itself prints must pass it.
#
# Needs go and python3 on PATH; no network and no cgo (no case builds with -race). The modules
# declare no dependency, and GOWORK=off and GOTOOLCHAIN=local keep the go command on the fixture
# module and the toolchain it already has.
#
# Run from anywhere: .github/scripts/go-test-shards.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
shards="$script_dir/go-test-shards.py"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
export GOWORK=off GOTOOLCHAIN=local GOFLAGS=

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# The interpreter python3 names, asked once in this harness's own environment, and every run of
# the script below starts it by that path, never by the script's shebang. The first python3 on PATH
# can be a version manager's shim that hands the interpreter a PATH of its own: mise 2026.10.11's
# python3 shim, in the mise-activated environment an Oh My Pi session's Python runner has, started
# the interpreter with that environment's PATH and without what the caller had put in front of it.
# So the stand-in go the stalled-download case puts first on PATH never reached the script, which
# built with mise's go instead, and the case failed all five of its checks, alone or beside another
# harness. disk_watch.test.sh resolves its interpreter the same way.
if ! python=$(python3 -I -c 'import sys; print(sys.executable)'); then
  echo "could not resolve the python3 interpreter" >&2
  exit 1
fi
if [[ "$python" != /* || ! -x "$python" ]]; then
  echo "python3 did not report an absolute executable path: $python" >&2
  exit 1
fi

# module <name>: an empty Go module of that name with one package at its root. Echoes its path.
module() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root"
  printf 'module example.com/%s\n\ngo 1.21\n' "$1" >"$root/go.mod"
  echo "$root"
}

# tests <file> <name>...: a test file declaring one empty top-level test per name.
tests() {
  local path=$1
  shift
  printf 'package fixture\n\nimport "testing"\n\n' >"$path"
  for name in "$@"; do
    printf 'func %s(t *testing.T) {}\n' "$name" >>"$path"
  done
}

# run_shards <directory> <argument>...: runs the script in that directory; sets out, err and
# status, keeping the two streams apart, because a refusal must print nothing on standard output.
run_shards() { run_script "$shards" "$@"; }

# run_script <script> <directory> <argument>...: run_shards, for the script or a stand-in for it,
# each a Python file this harness's interpreter runs.
run_script() {
  local script=$1 directory=$2
  shift 2
  set +e
  out=$(cd "$directory" && "$python" "$script" "$@" 2>"$work/stderr")
  status=$?
  set -e
  err=$(cat "$work/stderr")
}

# names_of <pattern>: the names an anchored alternation `^(A|B|C)$` holds, one per line.
names_of() {
  local inner=${1#'^('}
  inner=${inner%')$'}
  tr '|' '\n' <<<"$inner"
}

# union_of <pattern>...: every name the patterns hold, sorted, one per line.
union_of() {
  local p
  for p in "$@"; do names_of "$p"; done | LC_ALL=C sort
}

# without <name> <pattern>...: the same patterns with that one name taken out of the shard
# that holds it, one pattern per line - the control that leaves a test in no shard.
without() {
  local name=$1 p
  shift
  for p in "$@"; do
    printf '^(%s)$\n' "$(names_of "$p" | grep -v -x "$name" | paste -sd '|')"
  done
}

# The package every split case reads: seven plain tests, a fuzz target, an example with an output
# comment, and the four things that are no test of the split - an example without an output
# comment (go test compiles it and never runs it, and -test.list does not print it), a benchmark
# (-run never starts one), TestMain and a method named like a test. TestMain prints a line while
# the binary lists, as a TestMain that logs would. One more file carries a build tag, so the
# build flags decide whether its test exists, which is what `//go:build !race` decides for the
# race build of internal/dispatch/api.
root=$(module split)
tests "$root/plain_test.go" TestAlpha TestBeta TestGamma TestDelta TestEpsilon TestZeta TestEta
cat >"$root/kinds_test.go" <<'EOF'
package fixture

import (
	"fmt"
	"os"
	"testing"
)

type harness struct{}

func (harness) TestMethodIsNoTest(t *testing.T) {}

func TestMain(m *testing.M) {
	fmt.Println("listing as TestMain")
	os.Exit(m.Run())
}

func FuzzTheParser(f *testing.F) { f.Fuzz(func(t *testing.T, s string) {}) }

func Example_shown() {
	fmt.Println("shown")
	// Output: shown
}

func Example_unshown() { fmt.Println("never run") }

func BenchmarkTheSpeed(b *testing.B) {}
EOF
cat >"$root/tagged_test.go" <<'EOF'
//go:build shardtag

package fixture

import "testing"

func TestOnlyUnderTheTag(t *testing.T) {}
EOF
everything='Example_shown
FuzzTheParser
TestAlpha
TestBeta
TestDelta
TestEpsilon
TestEta
TestGamma
TestZeta'

echo "case: three shards split the package's tests, each test into exactly one"
patterns=()
for index in 0 1 2; do
  run_shards "$root" pattern "$index" 3 .
  check "shard $index exits 0" "$(is "$status" 0)"
  check "shard $index prints one anchored alternation" "$(contains "$out" '^\^(\([A-Za-z0-9_|]*\))\$$')"
  check "shard $index says what it holds and that the check passed" \
    "$(contains "$err" "^go-test-shards.py: shard $((index + 1)) of 3 holds [0-9]* of the 9 tests of \.; every test is in exactly one of the 3 shards$")"
  patterns+=("$out")
done
union=$(union_of "${patterns[@]}")
check "together the three hold every test, and each once" "$(is "$union" "$everything")"
check "no shard holds what is no test of the split" \
  "$(is "$(grep -c -E 'Example_unshown|BenchmarkTheSpeed|TestMain$|TestMethodIsNoTest|TestOnlyUnderTheTag' <<<"$union" || true)" 0)"
unsorted=0
for p in "${patterns[@]}"; do
  [ "$(names_of "$p")" = "$(names_of "$p" | LC_ALL=C sort)" ] || unsorted=$((unsorted + 1))
done
check "each shard's names are sorted" "$(is "$unsorted" 0)"

echo "case: the patterns the split prints pass the check (the current list passes)"
run_shards "$root" check . -- "${patterns[@]}"
check "exits 0" "$(is "$status" 0)"
check "prints nothing on standard output" "$(is "$out" "")"
check "says every test is in exactly one shard" \
  "$(contains "$err" '^go-test-shards.py: every one of the 9 tests of \. is in exactly one of the 3 shards$')"

echo "case: control - a test left out of every shard fails the check, by name"
mapfile -t without_gamma < <(without TestGamma "${patterns[@]}")
run_shards "$root" check . -- "${without_gamma[@]}"
check "exits 1" "$(is "$status" 1)"
check "prints nothing on standard output" "$(is "$out" "")"
check "names the test and says no leg runs it" \
  "$(contains "$err" '^go-test-shards.py: TestGamma is in no shard: none of the 3 patterns selects it, so no leg runs it$')"
check "names no other test" "$(is "$(grep -c 'is in' <<<"$err")" 1)"

echo "case: control - a test in two shards fails the check, by name"
run_shards "$root" check . -- "${patterns[@]}" '^(TestAlpha)$'
check "exits 1" "$(is "$status" 1)"
check "prints nothing on standard output" "$(is "$out" "")"
check "names the test and both shards" \
  "$(contains "$err" "^go-test-shards.py: TestAlpha is in shards [123] and 4 of 4: each of those legs runs it$")"
check "names no other test" "$(is "$(grep -c 'is in' <<<"$err")" 1)"

echo "case: control - pattern refuses its own split when the split leaves a test in no shard"
# A leg runs only the pattern `pattern` prints, so `pattern` proves its own split before it
# prints one: the comment on the race leg's step rests on that. Here the script runs with shard_of
# replaced by a stand-in that gives TestGamma the number of no shard, every other name its own; the
# split then leaves TestGamma out, and every leg must refuse it by name.
broken_split=$work/broken-split.py
cat >"$broken_split" <<EOF
#!/usr/bin/env python3
import importlib.util
import sys

spec = importlib.util.spec_from_file_location("go_test_shards", "$shards")
shards = importlib.util.module_from_spec(spec)
spec.loader.exec_module(shards)
shard_of = shards.shard_of
shards.shard_of = lambda name, total: total if name == "TestGamma" else shard_of(name, total)
sys.exit(shards.main(sys.argv[1:]))
EOF
chmod +x "$broken_split"
for index in 0 1 2; do
  run_script "$broken_split" "$root" pattern "$index" 3 .
  check "shard $index exits 1" "$(is "$status" 1)"
  check "shard $index prints nothing on standard output" "$(is "$out" "")"
  check "shard $index names TestGamma as in no shard, and no other test" "$(is "$(grep 'is in' <<<"$err")" \
    'go-test-shards.py: TestGamma is in no shard: none of the 3 patterns selects it, so no leg runs it')"
done

echo "case: a name both the package and its external test package declare is one name of the split"
# go test runs both TestSame functions here, one of package fixture and one of package
# fixture_test. The binary lists the name twice; the split puts the name in one shard, whose leg
# runs both, as go test does without -run, and the check counts that shard once.
root_same=$(module same)
tests "$root_same/internal_test.go" TestAlpha TestBeta TestSame
printf 'package fixture_test\n\nimport "testing"\n\nfunc TestSame(t *testing.T) {}\n' >"$root_same/external_test.go"
same_patterns=()
for index in 0 1; do
  run_shards "$root_same" pattern "$index" 2 .
  check "shard $index exits 0" "$(is "$status" 0)"
  same_patterns+=("$out")
done
check "the two shards hold the three names, TestSame once" \
  "$(is "$(union_of "${same_patterns[@]}" | paste -sd ' ')" "TestAlpha TestBeta TestSame")"
run_shards "$root_same" check . -- "${same_patterns[@]}"
check "the check passes them" "$(is "$status" 0)"
check "counting both TestSame tests" \
  "$(contains "$err" '^go-test-shards.py: every one of the 4 tests of \. is in exactly one of the 2 shards$')"
# The pattern of the leg that holds TestSame; ^$, which runs nothing, when no leg holds it.
same_leg='^$'
for p in "${same_patterns[@]}"; do
  if grep -q -x TestSame <<<"$(names_of "$p")"; then same_leg=$p; fi
done
ran=$(cd "$root_same" && go test -count=1 -v -run "$same_leg" . 2>&1)
check "the leg whose pattern holds TestSame runs both of them" \
  "$(is "$(grep -c '^--- PASS: TestSame (' <<<"$ran")" 2)"

echo "case: control - a name both packages declare, in two shards, fails the check by name"
run_shards "$root_same" check . -- '^(TestAlpha|TestBeta|TestSame)$' '^(TestSame)$'
check "exits 1" "$(is "$status" 1)"
check "prints nothing on standard output" "$(is "$out" "")"
check "names the test and both shards" \
  "$(contains "$err" '^go-test-shards.py: TestSame is in shards 1 and 2 of 2: each of those legs runs it$')"
check "names no other test" "$(is "$(grep -c 'is in' <<<"$err")" 1)"

echo "case: control - a pattern -list and -run read differently fails the check, by pattern"
# -test.list matches each top-level name against the whole pattern, while go test -run first splits
# the pattern at every | and / outside brackets and parentheses and rewrites each part (a space
# becomes _). Measured on Go 1.27 with TestAlpha, Test_Alpha and TestBeta:
# '^TestBeta$|^TestAlpha$/child' lists TestBeta and runs TestAlpha too (a / starts a test for its
# subtests); '^TestBeta$|^\QTestAlpha|\Qjunk\E$' lists TestBeta and runs TestAlpha too (the split
# cuts the quote in two); '(?i)^TestAlpha$|^Testbeta$' lists both and runs TestAlpha alone ((?i)
# covers its own part); '^(Test<U+00A0>Alpha|TestBeta)$' lists TestBeta and runs Test_Alpha too
# (the no-break space becomes _). Each listing said every test once while a leg ran one twice or
# not at all. So a pattern is read only as ^(Name|Name|...)$ of names the binary listed.
root_shape=$(module shape)
tests "$root_shape/shape_test.go" TestAlpha Test_Alpha TestBeta
nbsp=$'\xc2\xa0'
# shown <text>: the text as the refusal writes it, its line breaks and no-break spaces escaped.
shown() {
  local text=${1//$'\n'/\\u000a}
  printf '%s' "${text//$nbsp/\\u00a0}"
}
# outer_case <description> <pattern>...: check over the patterns, none of them ^( ... )$, exits 1,
# prints nothing on standard output, and names each as not of the shape, and nothing else. A fixed
# string, since a pattern holds what a grep pattern would read.
outer_case() {
  local what=$1 p number=0
  shift
  run_shards "$root_shape" check . -- "$@"
  check "$what: exits 1" "$(is "$status" 1)"
  check "$what: prints nothing on standard output" "$(is "$out" "")"
  for p in "$@"; do
    number=$((number + 1))
    check "$what: names pattern $number, $(shown "$p"), as not of the shape" "$(grep -qF -- \
      "go-test-shards.py: pattern $number of $#, \`$(shown "$p")\`, is not ^(Name|Name|...)\$: " \
      <<<"$err" && echo true || echo false)"
  done
  check "$what: and nothing else" "$(is "$(grep -c '^go-test-shards.py: ' <<<"$err")" "$#")"
}
# unlisted_case <description> <pattern> <name>...: check over the one pattern, of the shape but
# naming what the binary does not list, exits 1 and names the pattern and exactly those names.
unlisted_case() {
  local what=$1 p=$2 names=() n
  shift 2
  for n in "$@"; do names+=("\`$(shown "$n")\`"); done
  local joined
  joined=$(printf '%s, ' "${names[@]}")
  run_shards "$root_shape" check . -- "$p"
  check "$what: exits 1" "$(is "$status" 1)"
  check "$what: prints nothing on standard output" "$(is "$out" "")"
  check "$what: names the pattern and ${joined%, } as not listed" "$(grep -qF -- \
    "go-test-shards.py: pattern 1 of 1, \`$(shown "$p")\`, names ${joined%, }, which the test binary does not list: " \
    <<<"$err" && echo true || echo false)"
  check "$what: and nothing else" "$(is "$(grep -c '^go-test-shards.py: ' <<<"$err")" 1)"
}
outer_case "the / pair" '^TestAlpha$' '^TestBeta$|^TestAlpha$/child'
outer_case "the split \\Q quote" '^TestAlpha$' '^TestBeta$|^\QTestAlpha|\Qjunk\E$'
outer_case "the (?i) flag" '(?i)^TestAlpha$|^Testbeta$'
outer_case "a prefix, not a name" '^TestE'
outer_case "the shape with a newline after it" $'^(TestAlpha|TestBeta)$\n'
unlisted_case "a / in a name" '^(TestAlpha/child|TestBeta)$' 'TestAlpha/child'
unlisted_case "a regexp as a name" '^(Test.*)$' 'Test.*'
unlisted_case "a quote in a name" '^(\QTestAlpha\E|TestBeta)$' '\QTestAlpha\E'
unlisted_case "an empty name" '^(TestAlpha||TestBeta)$' ''
unlisted_case "a name of no test prefix" '^(Alpha|TestBeta)$' 'Alpha'
unlisted_case "a | outside the names' parentheses" '^(TestAlpha)|(TestBeta)$' 'TestAlpha)' '(TestBeta'
unlisted_case "a no-break space in a name" "^(Test${nbsp}Alpha|TestBeta)\$" "Test${nbsp}Alpha"

echo "case: control - the no-break space pair the review found"
run_shards "$root_shape" check . -- '^(TestAlpha|Test_Alpha)$' "^(Test${nbsp}Alpha|TestBeta)\$"
check "exits 1" "$(is "$status" 1)"
check "prints nothing on standard output" "$(is "$out" "")"
# shellcheck disable=SC2016 # literal backticks and $ of the message, no expansion wanted
check "names pattern 2 and Test\\u00a0Alpha as not listed, and nothing else" "$(is "$(grep '^go-test-shards.py: ' <<<"$err" | cut -d: -f2 | cut -d, -f1-3)" \
  ' pattern 2 of 2, `^(Test\u00a0Alpha|TestBeta)$`, names `Test\u00a0Alpha`')"

# Both of the shape, so the listing decides: neither is refused for its names, and the test the two
# select is named as one two legs run.
run_shards "$root_shape" check . -- '^(TestBeta)$' '^(TestAlpha|TestBeta|Test_Alpha)$'
check "two patterns of listed names that share a test: exits 1 with one line" \
  "$(is "$status/$(grep -c '^go-test-shards.py: ' <<<"$err")" "1/1")"
check "  naming the test in two shards" \
  "$(contains "$err" '^go-test-shards.py: TestBeta is in shards 1 and 2 of 2: each of those legs runs it$')"
run_shards "$root_shape" check . -- '^(TestAlpha|Test_Alpha)$' '^(TestBeta)$'
check "the same three tests in two patterns of listed names pass" "$(is "$status" 0)"
run_shards "$root_shape" pattern 0 1 .
check "and the pattern the split prints is of the shape" "$(is "$out" '^(TestAlpha|TestBeta|Test_Alpha)$')"

echo "case: a test whose name is not ASCII is split and checked like any other"
# go test runs TestÉclair, Test日本 and TestᲉ: cmd/go asks only that the rune after `Test` is not
# lower case. A listing that kept ASCII names alone would leave them in no shard, and its proof,
# reading the same listing, would pass while no leg ran them. TestᲉ is U+1C89, a letter of Unicode
# 16.0: the go that builds this module (Go 1.27, Unicode 17.0) compiles and runs it, while Python
# 3.12's own tables (Unicode 15.0) do not know it, so a listing that read names with `\w` left it
# out the same way.
root_unicode=$(module unicode)
tests "$root_unicode/names_test.go" TestAlpha TestBeta TestÉclair Test日本 TestᲉ
unicode_patterns=()
for index in 0 1; do
  run_shards "$root_unicode" pattern "$index" 2 .
  check "shard $index exits 0" "$(is "$status" 0)"
  unicode_patterns+=("$out")
done
check "the two shards hold all five tests, each once" \
  "$(is "$(union_of "${unicode_patterns[@]}" | paste -sd ' ')" \
    "TestAlpha TestBeta TestÉclair TestᲉ Test日本")"
run_shards "$root_unicode" check . -- "${unicode_patterns[@]}"
check "and the check passes them" "$(is "$status" 0)"
check "counting all five" \
  "$(contains "$err" '^go-test-shards.py: every one of the 5 tests of \. is in exactly one of the 2 shards$')"

for left_out in 'TestÉclair' 'TestᲉ'; do
  echo "case: control - $left_out, left out of every shard, fails the check by name"
  mapfile -t without_one < <(without "$left_out" "${unicode_patterns[@]}")
  run_shards "$root_unicode" check . -- "${without_one[@]}"
  check "exits 1" "$(is "$status" 1)"
  check "prints nothing on standard output" "$(is "$out" "")"
  check "names the test and says no leg runs it" \
    "$(contains "$err" "^go-test-shards.py: $left_out is in no shard: none of the 2 patterns selects it, so no leg runs it\$")"
  check "names no other test" "$(is "$(grep -c 'is in' <<<"$err")" 1)"
done

echo "case: the build flags decide which tests exist"
run_shards "$root" pattern 0 1 -tags shardtag .
check "exits 0" "$(is "$status" 0)"
check "the tagged test is in the split under its tag" "$(is "$(names_of "$out" | grep -c -x TestOnlyUnderTheTag)" 1)"
check "with the other nine" "$(is "$(names_of "$out" | wc -l | tr -d ' ')" 10)"
# Checked against the build without the tag, the pattern names a test that build does not list, so
# it is refused by that name: a pattern holds only names its own build listed.
run_shards "$root" check . -- "$out"
check "the same pattern checked without the tag exits 1" "$(is "$status" 1)"
# shellcheck disable=SC2016 # literal backticks of the message, no expansion wanted
check "  naming the tagged test as not listed" \
  "$(contains "$err" 'names `TestOnlyUnderTheTag`, which the test binary does not list: ')"

echo "case: the binary lists in the package's own directory, as go test runs it"
# go test runs a package's test binary in the package's directory, so a TestMain, an init or a
# package-level variable that reads a path relative to the package reads the package's own file.
# The race leg runs the script in packages/envoy with ./internal/dispatch/api/, a directory below
# it; here the script runs in the module's root with ./pkg, whose test file reads
# testdata/golden.txt as the package is initialised. Listed in the script's directory, the binary
# panics before it lists a name, and every leg refuses a package go test passes.
root_dir=$(module directory)
mkdir -p "$root_dir/pkg/testdata"
echo golden >"$root_dir/pkg/testdata/golden.txt"
cat >"$root_dir/pkg/golden_test.go" <<'EOF'
package fixture

import (
	"os"
	"testing"
)

var golden = must(os.ReadFile("testdata/golden.txt"))

func must(data []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return data
}

func TestGolden(t *testing.T) {
	if string(golden) != "golden\n" {
		t.Fatalf("golden is %q", golden)
	}
}

func TestOther(t *testing.T) {}
EOF
ran=$(cd "$root_dir" && go test -count=1 -v ./pkg 2>&1)
check "go test, run from the module's root, passes both tests" \
  "$(is "$(grep -c -E '^--- PASS: Test(Golden|Other) \(' <<<"$ran")" 2)"
run_shards "$root_dir" pattern 0 1 ./pkg
check "the split, run from the module's root, exits 0" "$(is "$status" 0)"
check "  with both tests in its one shard" "$(is "$out" '^(TestGolden|TestOther)$')"
run_shards "$root_dir" check ./pkg -- "$out"
check "  and the check passes it" "$(is "$status" 0)"

echo "case: control - the binary listed in the script's directory panics, and the split refuses"
# The script with package_directory replaced by a stand-in that answers the directory the script
# runs in, which is where the binary listed before it asked go for the package's.
caller_dir=$work/caller-directory.py
cat >"$caller_dir" <<EOF
#!/usr/bin/env python3
import importlib.util
import sys
from pathlib import Path

spec = importlib.util.spec_from_file_location("go_test_shards", "$shards")
shards = importlib.util.module_from_spec(spec)
spec.loader.exec_module(shards)
shards.package_directory = lambda flags, package: Path.cwd()
sys.exit(shards.main(sys.argv[1:]))
EOF
chmod +x "$caller_dir"
run_script "$caller_dir" "$root_dir" pattern 0 1 ./pkg
check "exits 2" "$(is "$status" 2)"
check "prints nothing on standard output" "$(is "$out" "")"
# shellcheck disable=SC2016 # literal backticks of the message, no expansion wanted
check "names the listing and the directory it ran in" \
  "$(contains "$err" "^go-test-shards.py: \`package.test -test.list\` in $root_dir exited 2:\$")"
check "  and passes on the binary's panic, naming the file it could not read" \
  "$(contains "$err" 'open testdata/golden.txt: no such file or directory')"

echo "case: a test added later lands in exactly one shard, and moves no other"
root_grown=$(module grown)
cp "$root"/*_test.go "$root_grown/"
tests "$root_grown/added_test.go" TestAddedLater
grown=()
for index in 0 1 2; do
  run_shards "$root_grown" pattern "$index" 3 .
  check "shard $index exits 0" "$(is "$status" 0)"
  grown+=("$out")
done
check "the new test is in exactly one shard" \
  "$(is "$(union_of "${grown[@]}" | grep -c -x TestAddedLater)" 1)"
moved=0
for index in 0 1 2; do
  before=$(names_of "${patterns[$index]}")
  after=$(names_of "${grown[$index]}" | grep -v -x TestAddedLater)
  [ "$before" = "$after" ] || moved=1
done
check "every earlier test is in the shard it was in" "$(is "$moved" 0)"
run_shards "$root_grown" check . -- "${grown[@]}"
check "and the grown package passes the check" "$(is "$status" 0)"

echo "case: the split is the same on every run"
run_shards "$root" pattern 1 3 .
check "shard 1 prints what it printed before" "$(is "$out" "${patterns[1]}")"

echo "case: an index outside the leg count"
run_shards "$root" pattern 3 3 .
check "exits 2" "$(is "$status" 2)"
check "prints nothing on standard output" "$(is "$out" "")"
check "says which indexes there are" "$(contains "$err" 'index 3 is not a shard of 3: an index runs from 0 to 2$')"
run_shards "$root" pattern -1 3 .
check "a negative index exits 2" "$(is "$status" 2)"
run_shards "$root" pattern 0 0 .
check "a total of none exits 2" "$(is "$status" 2)"
run_shards "$root" pattern one 3 .
check "a word for a number exits 2" "$(is "$status" 2)"
check "and says the two must be whole numbers" "$(contains "$err" 'must be whole numbers')"

echo "case: more legs than the package has tests"
run_shards "$root" pattern 0 40 .
check "exits 2" "$(is "$status" 2)"
check "prints nothing on standard output" "$(is "$out" "")"
check "says the matrix has too many legs" "$(contains "$err" 'the matrix has more legs than this package needs$')"

echo "case: a package with no test to split"
root_none=$(module none)
printf 'package fixture\n\nfunc Helper() {}\n' >"$root_none/helper.go"
run_shards "$root_none" pattern 0 2 .
check "exits 2" "$(is "$status" 2)"
check "prints nothing on standard output" "$(is "$out" "")"
check "says it has no test files" "$(contains "$err" 'has no test files$')"
printf 'package fixture\n\nimport "testing"\n\nfunc BenchmarkOnly(b *testing.B) {}\n' >"$root_none/bench_test.go"
run_shards "$root_none" pattern 0 2 .
check "a package of benchmarks alone exits 2" "$(is "$status" 2)"
check "and says it lists no test" "$(contains "$err" 'lists no test, so there is nothing to split$')"

echo "case: a package that does not build"
root_broken=$(module broken)
printf 'package fixture\n\nimport "testing"\n\nfunc TestBroken(t *testing.T) { undefined() }\n' >"$root_broken/broken_test.go"
run_shards "$root_broken" pattern 0 1 .
check "exits 2" "$(is "$status" 2)"
check "prints nothing on standard output" "$(is "$out" "")"
check "passes on the compiler's error" "$(contains "$err" 'undefined: undefined')"
check "  once" "$(is "$(grep -c 'undefined: undefined' <<<"$err")" 1)"

# The race leg's setup-go keeps no module cache, so the script's build is the step that downloads
# the modules, and a download that stalls runs until the job's timeout: the log must name the
# module while it waits, not after. A stand-in go, first on PATH, writes a downloading line, then
# makes <directory>/written, then waits until the harness releases it (or its scratch directory is
# gone), and fails as a build does. The harness waits on the stand-in's file, or on the script's
# end, whichever comes first (await_file): with the line written and the script still running, the
# line must already be on the script's standard error.
#
# stalled_build <directory> [<PATH entry> <label>]: runs the script's build of the split package
# with a stand-in go of <directory> first on PATH, and <PATH entry> after it when given, then
# checks what it printed, each check's name ending in <label>.
stalled_build() {
  local dir=$1 entry=${2:-} under=${3:-} building
  mkdir "$dir"
  cat >"$dir/go" <<EOF
#!/usr/bin/env bash
echo 'go: downloading example.com/stalled v1.0.0' >&2
: >'$dir/written'
until [ -e '$dir/released' ]; do
  [ -d '$dir' ] || exit 1
  sleep 0.1
done
echo 'go: example.com/stalled@v1.0.0: the download failed' >&2
exit 1
EOF
  chmod +x "$dir/go"
  (cd "$root" && PATH="$dir${entry:+:$entry}:$PATH" exec "$python" "$shards" pattern 0 1 . >"$dir/out" 2>"$dir/err") &
  building=$!
  await_file "$dir/written" "$building"
  seen=$(grep -q -x 'go: downloading example.com/stalled v1.0.0' "$dir/err" && echo true || echo false)
  running=$(kill -0 "$building" 2>/dev/null && echo true || echo false)
  touch "$dir/released"
  set +e
  wait "$building"
  status=$?
  set -e
  err=$(cat "$dir/err")
  # shellcheck disable=SC2154 # came is set by await_file (test-lib.sh)
  check "the downloading line is on standard error while go still runs$under" "$(is "$came/$seen/$running" file/true/true)"
  check "the failed build exits 2$under" "$(is "$status" 2)"
  check "prints nothing on standard output$under" "$(is "$(cat "$dir/out")" "")"
  check "go's error is on standard error once$under" \
    "$(is "$(grep -c -x 'go: example.com/stalled@v1.0.0: the download failed' <<<"$err")" 1)"
  # shellcheck disable=SC2016 # literal backticks of the message, no expansion wanted
  check "and the refusal is the last line, after it$under" \
    "$(contains "$(tail -n 1 <<<"$err")" '^go-test-shards.py: `go test -c -o .* \.` exited 1; its standard error is above$')"
}

echo "case: go's standard error reaches the script's as go writes it"
stalled_build "$work/stalled"

echo "case: the same with a python3 first on PATH that starts the interpreter with a PATH of its own"
# What a version manager's shim can do, as the comment on $python above says mise's did: this
# python3 starts the interpreter with the PATH this harness had before the case put anything in
# front of it, so a run of the script by its shebang, which finds this python3, builds with this
# harness's go and not the stand-in. The script, started by its interpreter's path, never meets
# it. The two checks before the case show the resetting python3 does what the row needs, so the row
# can fail.
resetting=$work/resetting-python
mkdir -p "$resetting/probe"
printf '%s' "$PATH" >"$resetting/path"
cat >"$resetting/python3" <<EOF
#!/bin/sh
PATH=\$(cat '$resetting/path') exec '$python' "\$@"
EOF
printf '#!/bin/sh\nexit 0\n' >"$resetting/probe/go"
chmod +x "$resetting/python3" "$resetting/probe/go"
check "a shebang's python3 is the resetting one" \
  "$(is "$(PATH="$resetting/probe:$resetting:$PATH" command -v python3)" "$resetting/python3")"
check "  and the interpreter it starts finds this harness's go, not the one first on its caller's PATH" \
  "$(is "$(PATH="$resetting/probe:$resetting:$PATH" python3 -c 'import shutil; print(shutil.which("go"))')" \
    "$(command -v go)")"
stalled_build "$work/stalled-resetting" "$resetting" ", a resetting python3 on PATH"

echo "case: a pattern longer than one argument may be"
# 1,300 names of 115 characters make a one-shard pattern of about 150,000 bytes, past
# MAX_ARG_STRLEN; two shards of the same package fit.
root_long=$(module long)
long_names=()
for n in $(seq -w 1 1300); do
  long_names+=("TestAVeryLongNameThatStandsForTheLongestTestNamesOfARealPackageAndKeepsGoingUntilItIsOneHundredAndTwentyLongNo$n")
done
tests "$root_long/long_test.go" "${long_names[@]}"
run_shards "$root_long" pattern 0 1 .
check "exits 2" "$(is "$status" 2)"
check "prints nothing on standard output" "$(is "$out" "")"
check "says the pattern is too long for one argument and to add a leg" \
  "$(contains "$err" 'is longer than 131060 bytes: .*131071 bytes (MAX_ARG_STRLEN).*add a leg to the matrix')"
run_shards "$root_long" pattern 0 2 .
check "two legs of it exit 0" "$(is "$status" 0)"

echo "case: a pattern at the most the test binary can be given, and one byte past it"
# The script gives the binary the pattern in one argument after -test.list= (11 bytes; go test
# gives it on after -test.run=, 10), and exec takes an argument of at most 131,071 bytes
# (MAX_ARG_STRLEN, its terminator included): 131,060 bytes of pattern are the most, and one byte
# more must be refused by name, not left to exec's E2BIG. Each pattern names TestA and TestAB,
# repeated to the length wanted (a part is a listed name however often it comes).
root_argument=$(module argument)
tests "$root_argument/argument_test.go" TestA TestAB
# pattern_of_length <bytes>: ^(TestA|...|TestAB|...)$ of exactly that many bytes, both names in it.
# Each TestA is 6 bytes with its |, each TestAB 7, and ^( and )$ less the last | are 3.
pattern_of_length() {
  local n=$(($1 - 3)) a b as bs
  b=$((n % 6))
  [ "$b" -gt 0 ] || b=6
  a=$(((n - 7 * b) / 6))
  printf -v as '%*s' "$a" ''
  printf -v bs '%*s' "$b" ''
  as=${as// /TestA|}
  bs=${bs// /TestAB|}
  printf '^(%s)$' "$as${bs%|}"
}
p=$(pattern_of_length 131060)
run_shards "$root_argument" check . -- "$p"
check "a pattern of 131060 bytes, the most, is that long and passes the check" "$(is "${#p}/$status" 131060/0)"
for bytes in 131061 131062; do
  p=$(pattern_of_length "$bytes")
  run_shards "$root_argument" check . -- "$p"
  check "a pattern of $bytes bytes is that long" "$(is "${#p}" "$bytes")"
  check "  exits 2" "$(is "$status" 2)"
  check "  prints nothing on standard output" "$(is "$out" "")"
  check "  says 131060 bytes are the most and to add a leg" "$(contains "$err" \
    "^go-test-shards.py: a pattern of $bytes bytes is longer than 131060 bytes: the test binary takes it in one argument after .-test\\.list=., and one argument may be 131071 bytes (MAX_ARG_STRLEN), so the binary could not be started with it; add a leg to the matrix, which shortens every shard's pattern\$")"
  check "  and shows no traceback" "$(is "$(grep -c Traceback <<<"$err" || true)" 0)"
done

echo "case: the wrong arguments"
run_shards "$root"
check "none exits 2" "$(is "$status" 2)"
check "none prints the usage" "$(contains "$err" 'usage: go-test-shards.py pattern')"
run_shards "$root" pattern 0 3
check "no package exits 2" "$(is "$status" 2)"
run_shards "$root" pattern 0 3 -race
check "a flag where the package goes exits 2" "$(is "$status" 2)"
check "and says so" "$(contains "$err" 'the last argument, -race, is a flag, not a package')"
run_shards "$root" check . '^(TestAlpha)$'
check "a check with no -- exits 2" "$(is "$status" 2)"
# shellcheck disable=SC2016 # literal backticks of the message, no expansion wanted
check "and says so" "$(contains "$err" 'no `--` before the patterns')"
run_shards "$root" check . --
check "a check with no pattern exits 2" "$(is "$status" 2)"

summary "go-test-shards.py"
