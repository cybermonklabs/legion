#!/usr/bin/env bash
# red-at-base.test.sh - Drives scripts/red-at-base.sh against a scratch git repository this test
# makes in its own temporary directory: a base commit, and a change after it that fixes three faults
# of the base (calc.Add adds one too many, lib's total of an empty list is -1, version.txt says 1)
# and adds tests that prove each fix. The test runs the tool over lists of lines and reads what it
# prints for each line and how it exits:
#
# - the three lines of the plan (a test red at its base, one green there, and a regex that matches
#   nothing) read RED, GREEN and NO-TEST, and the tool exits 1;
# - each kind reads each verdict it can give: go (a test that does not build at the base, a skip, a
#   subtest, a parent that fails before its subtest starts, a base that does not build without the
#   line's files, a file the checkout has not committed, GOFLAGS passed through, a passing test
#   that prints failure lines, regexes whose alternatives stand at different depths, a base whose
#   every file of the package a build constraint leaves out, a package the base lacks, a subtest
#   whose name holds a no-break space), bun (a workspace package that must resolve to the base's
#   copy, an installed package that must resolve at all, a skip, no match, an import the base
#   lacks, a package nobody installed, a teardown whose assertion the base breaks, a setup that
#   fails, two files in one line), e2e (a case that fails in the worktree and would pass in the
#   checkout, a case that passes, a case the script does not have, two cases in one line, a case
#   that skips, one that prints its start and no result, one that dies after its start, a driver
#   that the line's Go file keeps from building at the base, a base whose own driver does not
#   build, and a driver that builds after the copy) and cmd (a script that reads the base and exits
#   non-zero, a command that exits 0, a command and a directory the base lacks, a script the line
#   does not copy, an interpreter that is not there, a command that exits 127 itself, a script bash
#   finds on PATH, the line's own TMPDIR, a bash command string, exported BASHOPTS and SHELLOPTS
#   unchanged by the launcher, PWD and OLDPWD for a non-shell command);
# - each test that must read red at the base passes in the checkout first, so the base is what
#   fails;
# - a list whose every line is red, a BUILD-FAILED among them, exits 0;
# - the tool leaves nothing behind in the checkout, in its git directory or in TMPDIR, even when a
#   TERM stops it while a test runs;
# - a bad list or argument exits 2 before any test runs, and so does a list that holds no line, a
#   cmd line with no field -- and one with no command after it.
set -euo pipefail

# Hermetic: the developer's global and system git configuration (commit.gpgsign, core.hooksPath)
# must not reach the scratch repository or the tool under test.
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly project_root
readonly tool="${project_root}/scripts/red-at-base.sh"
scratch="$(mktemp -d "${TMPDIR:-/tmp}/red-at-base-test.XXXXXX")"
readonly scratch
readonly repo="${scratch}/repo"
# The TMPDIR of every run of the tool, so this test can read what a run left there.
readonly tool_tmp="${scratch}/tool-tmp"
background_pid=''

# A run this test started in the background is stopped by the process id it recorded, and the
# scratch directory, which holds the repository and every worktree the tool made of it, is removed
# by its own path.
cleanup() {
  if [[ -n $background_pid ]]; then
    kill "$background_pid" 2>/dev/null || true
    wait "$background_pid" 2>/dev/null || true
  fi
  rm -rf -- "$scratch"
}
trap cleanup EXIT

# A no-break space, U+00A0, as UTF-8: a field of a list may hold it, and Go rewrites it to _.
readonly nbsp=$'\xc2\xa0'

for program in git go bun awk realpath python3; do
  command -v "$program" >/dev/null || {
    echo "red-at-base.test.sh: $program is not on PATH, and the tool needs it" >&2
    exit 1
  }
done

scratch_git() {
  git -C "$repo" -c user.name='Scratch' -c user.email='scratch@example.com' -c commit.gpgsign=false "$@"
}

# write <path> <text>: one file of the scratch repository.
write() {
  mkdir -p "$(dirname "${repo}/$1")"
  printf '%s\n' "$2" >"${repo}/$1"
}

mkdir -p "$repo" "$tool_tmp"
git init -q -b main "$repo"

# ---- the base ------------------------------------------------------------------------------------
write .gitignore 'node_modules/'
write go.mod 'module example.com/scratch

go 1.22'
write calc/calc.go 'package calc

// Add is wrong at the base: it adds one too many.
func Add(a, b int) int { return a + b + 1 }

func Neg(a int) int { return -a }'
write calc/neg_test.go 'package calc

import "testing"

func TestNeg(t *testing.T) {
	if Neg(2) != -2 {
		t.Fatal("Neg(2) is not -2")
	}
}'
# A package that does not build at the base by itself.
write broken/half.go 'package broken

func Half(a int) int { return a / two }'
write package.json '{ "name": "scratch", "private": true }'
write lib/package.json '{ "name": "@scratch/lib", "version": "1.0.0", "main": "index.ts" }'
write lib/index.ts '// total is wrong at the base for an empty list.
export function total(xs: number[]): number {
  return xs.length === 0 ? -1 : xs.reduce((a, b) => a + b);
}'
write web/package.json '{ "name": "@scratch/web", "private": true }'
# A package whose every Go file a build constraint leaves out: at the base it builds nothing.
write live/live.go '//go:build scratchlive

package live

func Ready() bool { return true }'
write live/live_test.go '//go:build scratchlive

package live

import "testing"

func TestLiveIsReady(t *testing.T) {
	if !Ready() {
		t.Fatal("not ready")
	}
}'
# Two packages whose e2e test binaries are the drivers of scripts/e2e/scratch-driver.sh, as the
# repository's live proofs build theirs (go test -c -tags e2e): e2edriver builds at the base, and
# e2ebroken's own e2e test file calls Three, which the base lacks, so its driver does not build there.
write e2edriver/driver.go 'package e2edriver

func Ready() bool { return true }'
write e2edriver/ready_e2e_test.go '//go:build e2e

package e2edriver

import "testing"

func TestReady(t *testing.T) {
	if !Ready() {
		t.Fatal("not ready")
	}
}'
write e2ebroken/broken.go 'package e2ebroken

func Two() int { return 2 }'
write e2ebroken/broken_e2e_test.go '//go:build e2e

package e2ebroken

import "testing"

func TestThree(t *testing.T) {
	if Three() != 3 {
		t.Fatal("three is not three")
	}
}'
# Programs of the base exercise PATH lookup, a failed exec, and the command's own exit status.
write bin/pass-on-path.sh 'echo "pass-on-path ran"'
write bin/bad-interpreter '#!/no/such/interpreter
echo "the interpreter ran"'
write bin/exits-127 '#!/usr/bin/env bash
echo "about to exit 127"
exit 127'
chmod +x "${repo}/bin/bad-interpreter" "${repo}/bin/exits-127"
write bin/exec-then-continue.sh 'exec /no/such/program-for-launch-probe
echo "continued after failed exec"
exit 0'
# shellcheck disable=SC2016 # the script reports its own inherited options
write bin/shell-options.sh 'printf "BASHOPTS=%s\nSHELLOPTS=%s\nend-of-options\n" "$BASHOPTS" "$SHELLOPTS"'
write bin/pass-executable '#!/usr/bin/env bash
echo "pass-executable ran"'
chmod +x "${repo}/bin/pass-executable"
write bin/check-directory.py 'import os
import sys

cwd = os.getcwd()
print("PWD=" + os.environ.get("PWD", "<absent>"), flush=True)
print("cwd=" + cwd, flush=True)
assert os.environ.get("PWD") == cwd, "PWD does not identify the working directory"
assert os.environ.get("OLDPWD") == sys.argv[1], "OLDPWD does not identify the launch directory"
print("PWD matches the working directory; OLDPWD matches the launch directory")'
write version.txt '1'
scratch_git add -A
scratch_git commit -q -m 'the base'
base_commit="$(scratch_git rev-parse HEAD)"
readonly base_commit

# ---- the change: the fixes and the tests that prove them --------------------------------------------
write calc/calc.go 'package calc

func Add(a, b int) int { return a + b }

func Neg(a int) int { return -a }

func Mul(a, b int) int { return a * b }'
write calc/add_test.go 'package calc

import "testing"

func TestAddOfTwoAndTwoIsFour(t *testing.T) {
	if got := Add(2, 2); got != 4 {
		t.Fatalf("Add(2, 2) = %d, want 4", got)
	}
}'
write calc/neg_more_test.go 'package calc

import "testing"

func TestNegOfNegIsItself(t *testing.T) {
	if Neg(Neg(3)) != 3 {
		t.Fatal("Neg(Neg(3)) is not 3")
	}
}'
write calc/mul_test.go 'package calc

import "testing"

func TestMulOfTwoAndThree(t *testing.T) {
	if Mul(2, 3) != 6 {
		t.Fatal("Mul(2, 3) is not 6")
	}
}'
write calc/skip_test.go 'package calc

import "testing"

func TestNeedsADatabase(t *testing.T) {
	t.Skip("no database was given")
}'
write calc/sub_test.go 'package calc

import "testing"

func TestTable(t *testing.T) {
	t.Run("zero", func(t *testing.T) {
		if Neg(0) != 0 {
			t.Fatal("Neg(0) is not 0")
		}
	})
	t.Run("two_and_two", func(t *testing.T) {
		if Add(2, 2) != 4 {
			t.Fatal("Add(2, 2) is not 4")
		}
	})
}

func TestWithSetup(t *testing.T) {
	if Add(0, 0) != 0 {
		t.Fatal("setup: Add(0, 0) is not 0")
	}
	t.Run("new", func(t *testing.T) {})
}'
write calc/tagged_test.go '//go:build scratchtag

package calc

import "testing"

func TestTagged(t *testing.T) {
	if Add(1, 1) != 2 {
		t.Fatal("Add(1, 1) is not 2")
	}
}'
write broken/half.go 'package broken

func Half(a int) int { return a / 2 }'
write broken/half_test.go 'package broken

import "testing"

func TestHalfOfFour(t *testing.T) {
	if Half(4) != 2 {
		t.Fatal("Half(4) is not 2")
	}
}'
# A test that prints a transcript holding failure lines of its own, and passes at the base too.
write calc/transcript_test.go 'package calc

import (
	"fmt"
	"testing"
)

func TestLoggedTranscript(t *testing.T) {
	fmt.Println("--- FAIL: ExampleNestedProcess (0.00s)")
	fmt.Println("--- FAIL: TestLoggedTranscript (0.00s)")
	if Neg(1) != -1 {
		t.Fatal("Neg(1) is not -1")
	}
}'
# An untagged test, new beside the base's tagged files, and a test in a package the base lacks.
write live/version_test.go 'package live

import (
	"os"
	"strings"
	"testing"
)

func TestVersionIsTwo(t *testing.T) {
	version, err := os.ReadFile("../version.txt")
	if err != nil || strings.TrimSpace(string(version)) != "2" {
		t.Fatalf("version.txt holds %q (%v)", version, err)
	}
}'
write fresh/fresh_test.go 'package fresh

import (
	"os"
	"strings"
	"testing"
)

func TestFreshPackageReadsTwo(t *testing.T) {
	version, err := os.ReadFile("../version.txt")
	if err != nil || strings.TrimSpace(string(version)) != "2" {
		t.Fatalf("version.txt holds %q (%v)", version, err)
	}
}'
write lib/index.ts 'export function total(xs: number[]): number {
  return xs.reduce((a, b) => a + b, 0);
}

export function count(xs: number[]): number {
  return xs.length;
}'
write web/src/total.test.ts 'import { expect, test } from "bun:test";
import { total } from "@scratch/lib";
import { pad } from "pad-local";

test("total of an empty list is zero", () => {
  expect(total([])).toBe(0);
});

test("total of one and two is three", () => {
  expect(total([1, 2])).toBe(3);
});

test("pads a number", () => {
  expect(pad(7)).toBe("007");
});

test.skip("waits for a server", () => {});'
write web/src/count.test.ts 'import { expect, test } from "bun:test";
import { count } from "@scratch/lib";

test("counts", () => {
  expect(count([1, 2, 3])).toBe(3);
});'
write web/src/missing.test.ts 'import { expect, test } from "bun:test";
import { nothing } from "not-installed-anywhere";

test("imports", () => {
  expect(nothing).toBeDefined();
});'
# Hooks that fail: bun counts each as a failed test named (unnamed). The teardown asserts a
# postcondition that the base breaks (the total of an empty list) and the change keeps.
write web/src/hooks.test.ts 'import { afterAll, expect, test } from "bun:test";
import { total } from "@scratch/lib";

afterAll(() => {
  expect(total([])).toBe(0);
});

test("passes before a teardown that checks the empty total", () => {
  expect(1).toBe(1);
});

test("totals an empty list beside a checking teardown", () => {
  expect(total([])).toBe(0);
});'
write web/src/setup.test.ts 'import { beforeAll, expect, test } from "bun:test";

beforeAll(() => {
  throw new Error("the setup fails");
});

test("never runs after a failing setup", () => {
  expect(1).toBe(1);
});'
# shellcheck disable=SC2016 # the script's own expansions, run by its bash
write scripts/e2e/scratch-cases.sh '#!/usr/bin/env bash
# The cases of SCRATCH_CASES_E2E_CASES, commas between them, each read against the checkout it runs in.
set -euo pipefail
root=$(git rev-parse --show-toplevel)
for case in ${SCRATCH_CASES_E2E_CASES//,/ }; do
  case $case in
  version-is-two)
    echo "== $case"
    version=$(<"$root/version.txt")
    [[ $version == 2 ]] || { echo "FAIL $case: version.txt holds $version"; exit 1; }
    echo "ok $case"
    ;;
  version-file-exists)
    echo "== $case"
    [[ -f $root/version.txt ]] || { echo "FAIL $case: no version.txt"; exit 1; }
    echo "ok $case"
    ;;
  waits)
    echo "== $case"
    : >"$RED_AT_BASE_TEST_MARK"
    sleep 3
    echo "ok $case"
    ;;
  skips)
    echo "== $case"
    echo "SKIP $case: no database was given"
    ;;
  start-only)
    echo "== $case"
    ;;
  dies)
    echo "== $case"
    exit 3
    ;;
  *)
    echo "scratch-cases.sh: no case is named $case" >&2
    exit 2
    ;;
  esac
done'
# The change gives e2edriver Fresh and a test of it, and gives e2ebroken the Three its own e2e test
# calls, with one more e2e test. The driver script builds the driver of each case's package before
# the case starts, as a live proof does, and says so when the build fails.
write e2edriver/driver.go 'package e2edriver

func Ready() bool { return true }

func Fresh() int { return 2 }'
write e2edriver/fresh_e2e_test.go '//go:build e2e

package e2edriver

import "testing"

func TestFresh(t *testing.T) {
	if Fresh() != 2 {
		t.Fatal("fresh is not two")
	}
}'
write e2ebroken/broken.go 'package e2ebroken

func Two() int { return 2 }

func Three() int { return 3 }'
write e2ebroken/more_e2e_test.go '//go:build e2e

package e2ebroken

import "testing"

func TestBroken(t *testing.T) {
	if Two() != 2 {
		t.Fatal("two is not two")
	}
}'
# shellcheck disable=SC2016 # the script's own expansions, run by its bash
write scripts/e2e/scratch-driver.sh '#!/usr/bin/env bash
# The cases of SCRATCH_DRIVER_E2E_CASES, commas between them, each run by the e2e test binary of its
# package, which the script builds before the case starts.
set -euo pipefail
root=$(git rev-parse --show-toplevel)
work=$(mktemp -d "${TMPDIR:-/tmp}/scratch-driver.XXXXXX")
trap '"'"'rm -rf -- "$work"'"'"' EXIT
for case in ${SCRATCH_DRIVER_E2E_CASES//,/ }; do
  case $case in
  fresh | ready) package=e2edriver ;;
  broken) package=e2ebroken ;;
  *)
    echo "scratch-driver.sh: no case is named $case" >&2
    exit 2
    ;;
  esac
  go -C "$root/$package" test -c -tags e2e -o "$work/driver" . || {
    echo "FAIL driver: the driver did not build"
    exit 1
  }
  echo "== $case"
  "$work/driver" -test.count=1 -test.run "^Test${case^}\$" || {
    echo "FAIL $case: the driver failed"
    exit 1
  }
  echo "ok $case"
done'
# A subtest whose name holds a no-break space (U+00A0), which the testing package rewrites to _ in
# both the name and the pattern.
write calc/unicode_test.go 'package calc

import "testing"

func TestUnicode(t *testing.T) {
	t.Run("a\u00a0b", func(t *testing.T) {
		if Add(2, 3) != 5 {
			t.Fatal("Add(2, 3) is not 5")
		}
	})
	t.Run("other", func(t *testing.T) {})
}'
# Two scripts a cmd line runs: one that reads the checkout it runs in, and one that reads its TMPDIR.
# shellcheck disable=SC2016 # the script's own expansions, run by its bash
write scripts/version-check.sh '#!/usr/bin/env bash
echo "reading version.txt"
version=$(<version.txt)
echo "version.txt holds $version"
[[ $version == 2 ]] || { echo "want 2"; exit 4; }
echo "version is two"'
# shellcheck disable=SC2016 # the script's own expansions, run by its bash
write scripts/tmp-check.sh '#!/usr/bin/env bash
case $TMPDIR in */red-at-base.*/cmd-tmp) ;; *) echo "TMPDIR is $TMPDIR, no directory of the tool"; exit 5 ;; esac
[[ -d $TMPDIR && -z $(ls -A "$TMPDIR") ]] || { echo "TMPDIR $TMPDIR is no empty directory"; exit 6; }
: >"$TMPDIR/written"
echo "TMPDIR is the line'"'"'s own, and empty"'
write version.txt '2'
scratch_git add -A
scratch_git commit -q -m 'the change'
# A test the checkout has not committed: the tool copies what the working tree holds.
write calc/wip_test.go 'package calc

import "testing"

func TestWorkInProgress(t *testing.T) {
	if Add(5, 5) != 10 {
		t.Fatal("Add(5, 5) is not 10")
	}
}'

# ---- the installed packages, laid out as bun's isolated install lays them out -------------------------
# An installed package lives in the root's store, node_modules/.bun, and each package's own
# node_modules holds a relative link into the store, or to a workspace package of the repository.
# Git ignores all of it, so a worktree of the base has none of it until the tool mirrors it.
store_pad="node_modules/.bun/pad-local@1.0.0/node_modules/pad-local"
mkdir -p "${repo}/${store_pad}" "${repo}/node_modules/@scratch" "${repo}/web/node_modules/@scratch"
printf '%s\n' '{ "name": "pad-local", "version": "1.0.0", "type": "module", "main": "index.js" }' \
  >"${repo}/${store_pad}/package.json"
printf '%s\n' 'export function pad(n) { return String(n).padStart(3, "0"); }' >"${repo}/${store_pad}/index.js"
ln -s ".bun/pad-local@1.0.0/node_modules/pad-local" "${repo}/node_modules/pad-local"
ln -s "../../lib" "${repo}/node_modules/@scratch/lib"
ln -s "../../node_modules/.bun/pad-local@1.0.0/node_modules/pad-local" "${repo}/web/node_modules/pad-local"
ln -s "../../../lib" "${repo}/web/node_modules/@scratch/lib"

# ---- the harness -------------------------------------------------------------------------------------
# What the checkout holds, but its git directory: the tool must leave every entry as it found it.
snapshot() {
  (cd "$repo" && find . -path ./.git -prune -o -printf '%y %p %s %T@ %l\n' | LC_ALL=C sort)
}

status=0
output=''
# show: what the run just read printed, for the log of this test: its case, its exit status and
# the tool's stdout.
show() {
  echo "-- ${case_name} (exit ${status})"
  [[ -z $output ]] || while IFS= read -r printed; do echo "   $printed"; done <<<"$output"
}

# run_tool <list file> [NAME=value]...: the tool, at the top of the scratch repository, against the
# base, with the variables given; its exit status in status, its stdout in output, its stderr in
# ${scratch}/err.
run_tool() {
  local list=$1
  shift
  set +e
  (cd "$repo" && env TMPDIR="$tool_tmp" "$@" bash "$tool" --base "$base_commit" --list "$list" \
    >"${scratch}/out" 2>"${scratch}/err")
  status=$?
  set -e
  output="$(<"${scratch}/out")"
  show
}

# list <name> <line>...: a list file of the lines given.
list() {
  local file="${scratch}/$1.list"
  shift
  printf '%s\n' "$@" >"$file"
  printf '%s' "$file"
}

failures=0
case_name=''
# fail <text>: one expectation the case did not meet, with what the tool printed.
fail() {
  failures=$((failures + 1))
  {
    echo "FAIL: ${case_name}: $1"
    echo "  stdout:"
    while IFS= read -r printed; do echo "    $printed"; done <<<"$output"
    echo "  the last lines of stderr:"
    tail -n 25 "${scratch}/err" 2>/dev/null | sed 's/^/    /'
  } >&2
}

expect_status() {
  [[ $status == "$1" ]] || fail "expected exit $1, got $status"
}

# expect_verdict <line number> <verdict> [text the line names]: the tool's line for that line of
# the list reads that verdict, and names the text when one is given.
expect_verdict() {
  local line
  line="$(grep -E "^[A-Z-]+ line $1: " <<<"$output" || true)"
  [[ $line == "$2 line $1: "* ]] || fail "expected line $1 to read $2, got \"${line}\""
  [[ -z ${3:-} || $line == *"$3"* ]] || fail "expected line $1 to name \"$3\", got \"${line}\""
}

# expect_lines <count>: the tool printed one line for each line of the list, and nothing else.
expect_lines() {
  local count=0
  [[ -z $output ]] || count="$(wc -l <<<"$output")"
  [[ $count == "$1" ]] || fail "expected $1 lines on stdout, got $count"
}

# expect_nothing_left: no worktree entry in the repository's git directory, no directory of the
# tool in TMPDIR, and the checkout as it was.
expect_nothing_left() {
  local worktrees entries=()
  worktrees="$(git -C "$repo" worktree list --porcelain | grep -c '^worktree ' || true)"
  [[ $worktrees == 1 ]] || fail "expected the checkout alone in git worktree list, got $worktrees worktrees"
  if [[ -d ${repo}/.git/worktrees ]]; then
    shopt -s nullglob dotglob
    entries=("${repo}/.git/worktrees"/*)
    shopt -u nullglob dotglob
  fi
  ((${#entries[@]} == 0)) || fail "the repository's git directory still holds ${entries[*]##*/}"
  if compgen -G "${tool_tmp}/red-at-base.*" >/dev/null; then
    fail "TMPDIR still holds $(compgen -G "${tool_tmp}/red-at-base.*" | tr '\n' ' ')"
  fi
  [[ $(snapshot) == "$checkout_before" ]] ||
    fail "the checkout changed: $(diff <(printf '%s\n' "$checkout_before") <(snapshot) | head -n 5 | tr '\n' ' ')"
}

# ---- the controls pass in the checkout -----------------------------------------------------------------
# A line that must read red at the base proves the tool right only if its test passes at the change,
# so the base and not the test is what fails. Each test below runs in the checkout first.
case_name='the teardown that checks the empty total passes in the checkout'
set +e
(cd "${repo}/web" && bun test --no-install ./src/hooks.test.ts -t '^passes\sbefore\sa\steardown\sthat\schecks\sthe\sempty\stotal$') \
  >"${scratch}/out" 2>&1
status=$?
set -e
output="$(<"${scratch}/out")"
expect_status 0
[[ $output == *" 1 pass"* && $output == *" 0 fail"* ]] || fail 'expected bun to count 1 pass and 0 fail'
case_name='the subtest whose name holds a no-break space runs and passes in the checkout'
set +e
go -C "${repo}/calc" test -count=1 -v -run "^TestUnicode\$/^a${nbsp}b\$" . >"${scratch}/out" 2>&1
status=$?
set -e
output="$(<"${scratch}/out")"
expect_status 0
[[ $output == *"--- PASS: TestUnicode/a_b"* ]] || fail 'expected go to run and pass TestUnicode/a_b'
case_name='the version check passes in the checkout'
set +e
(cd "$repo" && bash scripts/version-check.sh) >"${scratch}/out" 2>&1
status=$?
set -e
output="$(<"${scratch}/out")"
expect_status 0
case_name='the e2e driver cases pass in the checkout'
set +e
(cd "$repo" && SCRATCH_DRIVER_E2E_CASES=fresh,broken,ready bash scripts/e2e/scratch-driver.sh) >"${scratch}/out" 2>&1
status=$?
set -e
output="$(<"${scratch}/out")"
expect_status 0
[[ $output == *"ok fresh"* && $output == *"ok broken"* && $output == *"ok ready"* ]] ||
  fail 'expected scratch-driver.sh to pass fresh, broken and ready in the checkout'
checkout_before="$(snapshot)"
readonly checkout_before

# ---- the cases -----------------------------------------------------------------------------------------
case_name='a red test, a green test and a regex that matches nothing read RED, GREEN and NO-TEST, and the tool exits 1'
run_tool "$(list three \
  'go calc ^TestAddOfTwoAndTwoIsFour$ calc/add_test.go' \
  'go calc ^TestNegOfNegIsItself$ calc/neg_more_test.go' \
  'go calc ^TestNoSuchTest$ calc/add_test.go')"
expect_status 1
expect_lines 3
expect_verdict 1 RED 'TestAddOfTwoAndTwoIsFour'
expect_verdict 2 GREEN 'TestNegOfNegIsItself'
expect_verdict 3 NO-TEST 'no tests to run'
expect_nothing_left

case_name='go: each verdict of a go line'
run_tool "$(list go \
  'go calc ^TestMulOfTwoAndThree$ calc/mul_test.go' \
  'go calc ^TestNeedsADatabase$ calc/skip_test.go' \
  'go calc ^TestTable$/^two_and_two$ calc/sub_test.go' \
  'go calc ^TestTable$/^zero$ calc/sub_test.go' \
  'go calc ^TestWithSetup$/^new$ calc/sub_test.go' \
  'go broken ^TestHalfOfFour$ broken/half_test.go' \
  'go calc ^TestWorkInProgress$ calc/wip_test.go' \
  'go calc ^TestTagged$ calc/tagged_test.go' \
  'go calc ^TestLoggedTranscript$ calc/transcript_test.go' \
  'go calc ^TestWithSetup$/^new$|^TestAbsent$ calc/sub_test.go' \
  'go calc ^TestTable$/^absent$|^TestAbsent$ calc/sub_test.go' \
  'go calc ^TestTable$/^two_and_two$|^TestAbsent$ calc/sub_test.go' \
  'go calc ^TestNegOfNegIsItself$|^TestTable$/^zero$ calc/neg_more_test.go calc/sub_test.go' \
  'go live ^TestVersionIsTwo$ live/version_test.go' \
  'go fresh ^TestFreshPackageReadsTwo$ fresh/fresh_test.go' \
  "go calc ^TestUnicode\$/^a${nbsp}b\$ calc/unicode_test.go")"
expect_status 1
expect_lines 16
expect_verdict 1 BUILD-FAILED 'undefined: Mul'
expect_verdict 2 NO-TEST 'skipped'
expect_verdict 3 RED 'TestTable/two_and_two'
expect_verdict 4 GREEN 'TestTable/zero'
# The parent fails before the subtest the regex names starts: no test it names ran.
expect_verdict 5 NO-TEST 'before a test the regex names ran: --- FAIL: TestWithSetup'
# The base's own package does not build: no fault of the line's file, so no BUILD-FAILED.
expect_verdict 6 NO-TEST 'does not build'
expect_verdict 7 RED 'TestWorkInProgress'
# With no build tag, the tagged file is no part of the build.
expect_verdict 8 NO-TEST 'no tests to run'
# The test prints two failure lines of its own and passes: only its result counts.
expect_verdict 9 GREEN '--- PASS: TestLoggedTranscript'
# An alternative at another depth changes nothing about the parent that ran only to reach new,
expect_verdict 10 NO-TEST 'before a test the regex names ran: --- FAIL: TestWithSetup'
# nor about a parent that passed with no subtest the regex names: go printed no tests to run.
expect_verdict 11 NO-TEST 'no tests to run'
expect_verdict 12 RED 'TestTable/two_and_two'
expect_verdict 13 GREEN
# The base builds no Go file in live (each holds a build tag), nor any in fresh, which it lacks:
# the line's file is the package, and it runs.
expect_verdict 14 RED 'TestVersionIsTwo'
expect_verdict 15 RED 'TestFreshPackageReadsTwo'
# Go rewrites the no-break space to _ in the subtest's name and in the pattern alike: the selected
# subtest ran, and fails at the base.
expect_verdict 16 RED '--- FAIL: TestUnicode/a_b'
expect_nothing_left

case_name='go: the caller GOFLAGS reaches go test'
run_tool "$(list goflags 'go calc ^TestTagged$ calc/tagged_test.go')" GOFLAGS="${GOFLAGS:+$GOFLAGS }-tags=scratchtag"
expect_status 0
expect_lines 1
expect_verdict 1 RED 'TestTagged'
expect_nothing_left

case_name='bun: each verdict of a bun line'
run_tool "$(list bun \
  'bun web total\sof\san\sempty\slist\sis\szero web/src/total.test.ts' \
  'bun web ^total\sof\sone\sand\stwo\sis\sthree$ web/src/total.test.ts' \
  'bun web ^pads\sa\snumber$ web/src/total.test.ts' \
  'bun web ^waits\sfor\sa\sserver$ web/src/total.test.ts' \
  'bun web ^no\ssuch\stest$ web/src/total.test.ts' \
  'bun web ^counts$ web/src/count.test.ts' \
  'bun web ^imports$ web/src/missing.test.ts' \
  'bun web ^passes\sbefore\sa\steardown\sthat\schecks\sthe\sempty\stotal$ web/src/hooks.test.ts' \
  'bun web ^totals\san\sempty\slist\sbeside\sa\schecking\steardown$ web/src/hooks.test.ts' \
  'bun web ^never\sruns\safter\sa\sfailing\ssetup$ web/src/setup.test.ts' \
  'bun web ^counts$|^total\sof\sone\sand\stwo\sis\sthree$ web/src/total.test.ts web/src/count.test.ts')"
expect_status 1
expect_lines 11
# Red only when @scratch/lib resolves to the base's lib, where the total of an empty list is -1.
expect_verdict 1 RED 'total of an empty list is zero'
expect_verdict 2 GREEN
# Green only when pad-local, an installed package of the checkout, resolves at the base.
expect_verdict 3 GREEN
expect_verdict 4 NO-TEST
expect_verdict 5 NO-TEST 'matched 0 tests'
expect_verdict 6 BUILD-FAILED "Export named 'count' not found"
# A package nobody installed is no fault of the test's code at the base.
expect_verdict 7 NO-TEST 'not-installed-anywhere'
# A teardown asserts the postcondition that the base breaks, after the named test passed (the
# checkout passes it, above): the failure is the test's, so RED, never GREEN or BUILD-FAILED. A setup
# that fails is a failure of the file's tests too.
expect_verdict 8 RED 'a hook failed, which bun counts as a failed test named (unnamed): error: expect(received).toBe(expected)'
expect_verdict 9 RED 'totals an empty list beside a checking teardown'
expect_verdict 10 RED 'a hook failed, which bun counts as a failed test named (unnamed): error: the setup fails'
# Each file runs on its own, and the line reads the more red of the two.
expect_verdict 11 BUILD-FAILED "web/src/count.test.ts: the file does not load at the base"
expect_nothing_left

case_name='cmd: each verdict of a cmd line'
run_tool "$(list cmd \
  'cmd . version-check scripts/version-check.sh -- bash scripts/version-check.sh' \
  'cmd . version-file-exists -- test -f version.txt' \
  'cmd . no-command -- no-such-command-anywhere version.txt' \
  'cmd . unlisted-script -- bash scripts/version-check.sh' \
  'cmd web in-its-directory web/src/count.test.ts -- test -f src/count.test.ts' \
  'cmd fresh no-directory-at-base -- true' \
  'cmd . own-tmpdir scripts/tmp-check.sh -- bash scripts/tmp-check.sh' \
  'cmd . a-command-string -- bash -c true' \
  'cmd . bad-interpreter -- ./bin/bad-interpreter' \
  'cmd . exits-127 -- ./bin/exits-127' \
  'cmd . no-permission -- ./bin/pass-on-path.sh')"
expect_status 1
expect_lines 11
# Red only when the script reads the base's version.txt, which holds 1; the checkout's holds 2.
expect_verdict 1 RED 'exit 4 from bash scripts/version-check.sh: reading version.txt | version.txt holds 1 | want 2'
expect_verdict 2 GREEN 'exit 0 from test -f version.txt'
# The exec itself returned: nothing of the command ran.
expect_verdict 3 BUILD-FAILED 'the command never started: the exec of no-such-command-anywhere failed:'
# The script is the change's, and only a line that lists it copies it to the base. Bash started and
# found no script, so the run is bash's own, a RED: a line that proves nothing of the change, which
# is why a cmd line lists each script of the change its command runs.
expect_verdict 4 RED 'exit 127 from bash scripts/version-check.sh: bash: scripts/version-check.sh: No such file or directory'
expect_verdict 5 GREEN 'exit 0 from test -f src/count.test.ts'
# fresh is the change's directory, and the line copies nothing into it: the launcher cannot enter it.
expect_verdict 6 BUILD-FAILED 'the command never started: could not enter '
[[ $output == *'fresh: no such file or directory'* ]] || fail 'expected line 6 to quote why the directory could not be entered'
expect_verdict 7 GREEN "TMPDIR is the line's own, and empty"
expect_verdict 8 GREEN 'exit 0 from bash -c true'
# The file is there and executable, and the exec of it fails, since its interpreter is not there.
expect_verdict 9 BUILD-FAILED 'the command never started: the exec of ./bin/bad-interpreter failed:'
[[ $output == *'fork/exec ./bin/bad-interpreter: no such file or directory'* ]] || fail 'expected line 9 to quote why the exec failed'
# It started and exited 127 itself, the status of a command that cannot start: its own run, a RED.
expect_verdict 10 RED 'exit 127 from ./bin/exits-127: about to exit 127'
expect_verdict 11 BUILD-FAILED 'fork/exec ./bin/pass-on-path.sh: permission denied'
expect_nothing_left

case_name='cmd: a script bash finds on PATH runs at the base'
# PATH holds bin, relative to the line's directory, the top of the worktree; bash, given a script
# name with no slash that is no file of that directory, searches PATH for it.
run_tool "$(list path \
  'cmd . path-script -- bash pass-on-path.sh' \
  'cmd . path-executable -- pass-executable')" PATH="bin:${PATH}"
expect_status 1
expect_lines 2
expect_verdict 1 GREEN 'exit 0 from bash pass-on-path.sh: pass-on-path ran'
expect_verdict 2 GREEN 'exit 0 from pass-executable: pass-executable ran'
expect_nothing_left

# Compare with a direct run as well as literal verdicts: the launcher must not add execfail,
# remove the caller's execfail, or add its own errexit, nounset and pipefail to a measured bash.
for bashopts in extquote execfail:extquote; do
  case_name="cmd: exported BASHOPTS=$bashopts preserves a failed exec and the caller's options"
  set +e
  (cd "$repo" && env BASHOPTS="$bashopts" bash bin/exec-then-continue.sh) >"${scratch}/direct" 2>&1
  direct_status=$?
  set -e
  echo "-- direct BASHOPTS=$bashopts (exit $direct_status)"
  cat "${scratch}/direct"
  if [[ $bashopts == extquote ]]; then
    [[ $direct_status == 127 ]] || fail "expected the direct exec failure to exit 127, got $direct_status"
    expected_verdict=RED
  else
    [[ $direct_status == 0 ]] || fail "expected the caller's execfail to continue, got $direct_status"
    expected_verdict=GREEN
  fi
  direct_options="$(cd "$repo" && env BASHOPTS="$bashopts" bash bin/shell-options.sh)"
  run_tool "$(list bashopts \
    'cmd . exec-then-continue -- bash bin/exec-then-continue.sh' \
    'cmd . shell-options -- bash bin/shell-options.sh')" BASHOPTS="$bashopts"
  expect_status 1
  expect_lines 2
  expect_verdict 1 "$expected_verdict" "exit $direct_status from bash bin/exec-then-continue.sh"
  expect_verdict 2 GREEN "${direct_options//$'\n'/ | }"
  expect_nothing_left
done

case_name='cmd: exported errexit distinguishes a failed start from the command exiting 127'
direct_options="$(cd "$repo" && env SHELLOPTS=errexit bash bin/shell-options.sh)"
run_tool "$(list errexit \
  'cmd . bad-interpreter -- ./bin/bad-interpreter' \
  'cmd . exits-127 -- ./bin/exits-127' \
  'cmd . shell-options -- bash bin/shell-options.sh')" SHELLOPTS=errexit
expect_status 1
expect_lines 3
expect_verdict 1 BUILD-FAILED
expect_verdict 2 RED 'exit 127 from ./bin/exits-127: about to exit 127'
expect_verdict 3 GREEN "${direct_options//$'\n'/ | }"
expect_nothing_left

case_name='cmd: a non-shell command sees the base directory in PWD and the launch directory in OLDPWD'
directory_base="${scratch}/directory-base"
scratch_git worktree add --detach --quiet "$directory_base" "$base_commit"
for directory in . web; do
  set +e
  (cd "$repo" && export OLDPWD=not-the-launch-directory && cd "${directory_base}/${directory}" &&
    python3 "${directory_base}/bin/check-directory.py" "$repo") >"${scratch}/direct" 2>&1
  direct_status=$?
  set -e
  echo "-- direct non-shell directory check in $directory (exit $direct_status)"
  cat "${scratch}/direct"
  [[ $direct_status == 0 ]] || fail "expected the direct non-shell directory check to pass, got $direct_status"
done
scratch_git worktree remove --force "$directory_base"
run_tool "$(list directory-environment \
  "cmd . directory-environment -- python3 bin/check-directory.py $repo" \
  "cmd web nested-directory-environment -- python3 ../bin/check-directory.py $repo")" OLDPWD=not-the-launch-directory
expect_status 1
expect_lines 2
expect_verdict 1 GREEN 'PWD matches the working directory; OLDPWD matches the launch directory'
expect_verdict 2 GREEN 'PWD matches the working directory; OLDPWD matches the launch directory'
expect_nothing_left

case_name='e2e: each verdict of an e2e line'
run_tool "$(list e2e \
  'e2e scripts/e2e/scratch-cases.sh version-is-two scripts/e2e/scratch-cases.sh' \
  'e2e scripts/e2e/scratch-cases.sh version-file-exists scripts/e2e/scratch-cases.sh' \
  'e2e scripts/e2e/scratch-cases.sh no-such-case scripts/e2e/scratch-cases.sh' \
  'e2e scripts/e2e/scratch-cases.sh version-file-exists,version-is-two scripts/e2e/scratch-cases.sh' \
  'e2e scripts/e2e/scratch-cases.sh skips scripts/e2e/scratch-cases.sh' \
  'e2e scripts/e2e/scratch-cases.sh start-only scripts/e2e/scratch-cases.sh' \
  'e2e scripts/e2e/scratch-cases.sh dies scripts/e2e/scratch-cases.sh' \
  'e2e scripts/e2e/scratch-cases.sh version-file-exists,skips scripts/e2e/scratch-cases.sh')"
expect_status 1
expect_lines 8
# Red only when the script reads the base's version.txt, which holds 1; the checkout's holds 2.
expect_verdict 1 RED 'FAIL version-is-two: version.txt holds 1'
expect_verdict 2 GREEN 'ok version-file-exists'
expect_verdict 3 NO-TEST 'no line == <case>'
expect_verdict 4 RED 'version.txt holds 1'
# A case that started and printed no ok, in a run that exits 0, has no result to read.
expect_verdict 5 NO-TEST 'skips started and printed no ok'
expect_verdict 6 NO-TEST 'start-only started and printed no ok'
expect_verdict 7 RED 'exit 3 after == dies'
expect_verdict 8 NO-TEST 'skips started and printed no ok'
expect_nothing_left

case_name='e2e: the driver of the line'"'"'s Go files, built at the base before and after the copy'
run_tool "$(list e2e-driver \
  'e2e scripts/e2e/scratch-driver.sh fresh scripts/e2e/scratch-driver.sh e2edriver/fresh_e2e_test.go' \
  'e2e scripts/e2e/scratch-driver.sh broken scripts/e2e/scratch-driver.sh e2ebroken/more_e2e_test.go' \
  'e2e scripts/e2e/scratch-driver.sh ready scripts/e2e/scratch-driver.sh e2edriver/ready_e2e_test.go')"
expect_status 1
expect_lines 3
# The base's own driver builds, and with the line's file it does not: the file reads Fresh, which
# the base lacks. That is the test's failure, read before the script runs, and the line names it.
expect_verdict 1 BUILD-FAILED 'what the base lacks: Fresh'
# The base's own driver does not build without the line's file, so no build failure after the copy
# would be the file's.
expect_verdict 2 NO-TEST 'does not build in e2ebroken with -tags e2e'
# A driver that builds after the copy runs the script, and the case decides.
expect_verdict 3 GREEN 'ok ready'
expect_nothing_left

case_name='a list of red lines, a BUILD-FAILED among them, exits 0; comments and blank lines are passed over'
run_tool "$(list red \
  '# The tests of the change, each red at the base.' \
  'go calc ^TestAddOfTwoAndTwoIsFour$ calc/add_test.go' \
  '' \
  'go calc ^TestMulOfTwoAndThree$ calc/mul_test.go' \
  'bun web total\sof\san\sempty\slist web/src/total.test.ts' \
  'bun web ^counts$ web/src/count.test.ts' \
  'e2e scripts/e2e/scratch-cases.sh version-is-two scripts/e2e/scratch-cases.sh')"
expect_status 0
expect_lines 5
expect_verdict 2 RED
expect_verdict 4 BUILD-FAILED
expect_verdict 5 RED
expect_verdict 6 BUILD-FAILED
expect_verdict 7 RED
expect_nothing_left

case_name='a TERM while a test runs leaves nothing behind'
term_list="$(list term 'e2e scripts/e2e/scratch-cases.sh waits scripts/e2e/scratch-cases.sh')"
(cd "$repo" && exec env TMPDIR="$tool_tmp" RED_AT_BASE_TEST_MARK="${scratch}/mark" \
  bash "$tool" --base "$base_commit" --list "$term_list" >"${scratch}/out" 2>"${scratch}/err") &
background_pid=$!
for _ in $(seq 1 300); do
  [[ -e ${scratch}/mark ]] && break
  sleep 0.1
done
[[ -e ${scratch}/mark ]] || fail 'the case that waits never started'
kill -TERM "$background_pid" 2>/dev/null || fail 'the run had ended before the TERM'
set +e
wait "$background_pid"
status=$?
set -e
background_pid=''
output="$(<"${scratch}/out")"
show
expect_status 143
expect_lines 0
expect_nothing_left

# A refusal comes before any test runs, so it prints no line on stdout.
refused() {
  case_name=$1
  shift
  run_tool "$@"
  expect_status 2
  expect_lines 0
  [[ -s ${scratch}/err ]] || fail 'expected a reason on stderr'
}

refused 'a list that holds no line proves nothing, and exits 2' "$(list empty '# nothing but a comment' '')"
grep -q 'holds no line' "${scratch}/err" || fail 'expected stderr to say the list holds no line'
refused 'a file the checkout does not have exits 2' \
  "$(list nofile 'go calc ^TestAddOfTwoAndTwoIsFour$ calc/add_test.go' 'go calc ^TestX$ calc/absent_test.go')"
grep -q 'calc/absent_test.go' "${scratch}/err" || fail 'expected stderr to name the absent file'
refused 'a kind the tool does not have exits 2' "$(list kind 'rust calc ^TestX$ calc/add_test.go')"
refused 'a path out of the checkout exits 2' "$(list parent 'go calc ^TestX$ ../outside_test.go')"
refused 'a line of fewer than four fields exits 2' "$(list short 'go calc ^TestX$')"
refused 'a bun line with no test file under its directory exits 2' "$(list nobuntest 'bun web ^x$ lib/index.ts')"
refused 'an e2e line whose script is no .sh file of the checkout exits 2' \
  "$(list noscript 'e2e scripts/e2e/absent.sh some-case scripts/e2e/scratch-cases.sh')"
refused 'a cmd line with no field -- exits 2' "$(list nodashes 'cmd . no-dashes version.txt true')"
grep -q 'has no field --' "${scratch}/err" || fail 'expected stderr to say the line has no field --'
refused 'a cmd line with nothing after -- exits 2' "$(list nocommand 'cmd . no-command version.txt --')"
grep -q 'names no command' "${scratch}/err" || fail 'expected stderr to say the line names no command'

case_name='a base that names no commit exits 2'
set +e
(cd "$repo" && env TMPDIR="$tool_tmp" bash "$tool" --base no-such-ref --list "$(list one 'go calc ^TestX$ calc/add_test.go')" \
  >"${scratch}/out" 2>"${scratch}/err")
status=$?
set -e
output="$(<"${scratch}/out")"
show
expect_status 2
expect_lines 0

case_name='no arguments print the usage and exit 2'
set +e
(cd "$repo" && bash "$tool" >"${scratch}/out" 2>"${scratch}/err")
status=$?
set -e
output="$(<"${scratch}/out")"
show
expect_status 2
grep -q 'usage: ' "${scratch}/err" || fail 'expected the usage on stderr'

case_name='after every case'
expect_nothing_left

if ((failures > 0)); then
  echo "red-at-base.test.sh: ${failures} expectation(s) failed" >&2
  exit 1
fi
echo 'red-at-base.test.sh: every case passed'
