#!/usr/bin/env bash
# Tests for `check-test-reach.sh`: a fixture repository that passes, and one fixture for each kind
# of finding - a `_test.go` file with a tag no job passes, a test one `-skip` leaves out, a Go test
# and a Bun test that skip on a variable no job sets, an assertion that runs only when such a
# variable is set, a script with no tool line that no job runs, and a scheduled step - each of
# which must fail and name its file; and the registry's classes, each held to a gate. Then the
# fixtures of the forms the layout of matrix jobs writes, each named in its checks: a go test in a
# matrix value, in a script, in a composite action and in a called workflow's input, read as runs;
# a $GITHUB_ENV write of an action's step, read by that action's later steps and the job's;
# gotestsum's `-skip` and `-tags` after its `--`; a package go-test-split.sh splits, read whole,
# or under the -run=<pattern> its call passes;
# workflows no merge waits for (run on a pull request only when their files change, schedule-only,
# workflow_dispatch-only, a called workflow) with a test, a preload or a repeat count; release
# steps listed, unlisted, and reading nothing their job built; race-impossible tests, a module or
# an excluded package no step runs, and constraints and file names no test step's platform meets;
# a tool's runs lines, reached through a step's `exec timeout …` too, and a script its source
# names; and the not-a-test line on a harness, a proof and a script that cancels runs.
#
# Each case builds a miniature repository under a temporary directory and symlinks the real
# script, the step reader it imports (go_test_steps.py) and the modules that one imports into it,
# so the code under test is the code CI runs: the script resolves its root from its own path
# (`cd "$(dirname "$0")/../.."`), which the symlink puts at the fixture root. A fixture is no git
# checkout, so the search walks it.
#
# No case runs the search over this repository's own tree: ci.yaml's test-reach job does
# ("Check every test reaches a job ci-ok needs").
#
# Run from anywhere: .github/scripts/check-test-reach.test.sh
# CI runs it in ci-job-test.yaml's test job.
set -euo pipefail
# Every fixture is built inside `root=$(fixture …)`, and bash does not carry errexit into a
# command substitution on its own: without this, a tree that failed to build is used anyway and
# the case reports on whatever is there.
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# fixture <name>: a tree that passes. Echoes the fixture root.
#   - ci.yaml's `ci-ok` needs `lint`, which runs the search, `go`, which runs the Go module's
#     tests (its unit step over ./..., and its tagged step with the `live` tag and FIXTURE_DSN),
#     and `bun`, which runs the Bun package's tests with FIXTURE_FLAG;
#   - live_test.go skips without FIXTURE_DSN, a `set` row naming `go`, and flag.test.ts skips
#     without FIXTURE_FLAG, a `selector` row naming `bun`.
# A repeated name is refused, since two cases sharing a tree would share their edits.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/.github/workflows" "$root/packages/envoy/unit" \
    "$root/packages/envoy/live" "$root/packages/web" "$root/scripts"
  for linked in check-test-reach.sh go_test_steps.py go_build_tags.py; do
    ln -s "$script_dir/$linked" "$root/.github/scripts/$linked"
  done
  cat > "$root/.github/workflows/ci.yaml" <<'YAML'
name: CI
on: [pull_request, merge_group]
jobs:
  lint:
    runs-on: ubuntu-24.04
    steps:
      - name: Check every test reaches a job ci-ok needs
        run: .github/scripts/check-test-reach.sh
  go:
    runs-on: ubuntu-24.04
    steps:
      - name: Unit test
        run: go test -v -count=1 ./...
        working-directory: packages/envoy
      - name: Live test
        run: go test -v -count=1 -tags live ./live/
        working-directory: packages/envoy
        env:
          FIXTURE_DSN: postgres://fixture
  bun:
    runs-on: ubuntu-24.04
    steps:
      - name: Bun tests
        run: bun test
        working-directory: packages/web
        env:
          FIXTURE_FLAG: "1"
  ci-ok:
    needs: [lint, go, bun]
    if: ${{ always() }}
    runs-on: ubuntu-24.04
    steps:
      - run: echo judged
YAML
  printf 'module example.com/envoy\n\ngo 1.27\n' > "$root/packages/envoy/go.mod"
  cat > "$root/packages/envoy/unit/unit_test.go" <<'GO'
package unit

import "testing"

func TestUnit(t *testing.T) {
	if 1+1 != 2 {
		t.Fatal("arithmetic")
	}
}
GO
  cat > "$root/packages/envoy/live/live_test.go" <<'GO'
//go:build live

package live

import (
	"os"
	"testing"
)

func TestLive(t *testing.T) {
	if os.Getenv("FIXTURE_DSN") == "" {
		t.Skip("FIXTURE_DSN names no server")
	}
}
GO
  printf '{ "name": "web", "scripts": { "test": "bun test" } }\n' > "$root/packages/web/package.json"
  cat > "$root/packages/web/flag.test.ts" <<'TS'
import { expect, test } from "bun:test";

test.skipIf(process.env.FIXTURE_FLAG !== "1")("the flagged row", () => {
  expect(1).toBe(1);
});
TS
  cat > "$root/.github/test-variables.yaml" <<'YAML'
variables:
  FIXTURE_DSN:
    class: set
    jobs: [go]
    why: The server the live test reads, which the go job's live step names.
  FIXTURE_FLAG:
    class: selector
    jobs: [bun]
    why: Selects the flagged row, which the bun job runs.
YAML
  printf 'checks: []\n' > "$root/.github/release-checks.yaml"
  echo "$root"
}

# required_job <root> <job id> <the job's body, indented four spaces>: one more job of ci.yaml,
# which ci-ok needs.
required_job() {
  local root=$1 job=$2 body=$3
  printf '  %s:\n%s\n' "$job" "$body" >> "$root/.github/workflows/ci.yaml"
  sed -i "s|^    needs: \[\(.*\)\]\$|    needs: [\1, $job]|" "$root/.github/workflows/ci.yaml"
}

# unit_only <root>: the go job's unit step runs the unit and live packages alone, so a package the
# case adds is run only by the steps the case gives it.
unit_only() {
  sed -i 's|        run: go test -v -count=1 ./...|        run: go test -v -count=1 ./unit/ ./live/|' "$1/.github/workflows/ci.yaml"
}

# go_test_file <path> <package> <constraint or ""> <test name…>: a test file of empty tests.
go_test_file() {
  local path=$1 package=$2 constraint=$3
  shift 3
  mkdir -p "$(dirname "$path")"
  {
    [ -z "$constraint" ] || printf '//go:build %s\n\n' "$constraint"
    printf 'package %s\n\nimport "testing"\n' "$package"
    for name in "$@"; do
      printf '\nfunc %s(t *testing.T) {}\n' "$name"
    done
  } > "$path"
}

# run_check <root>: runs the search, setting `out` to its combined output and `status` to its exit
# code. Not a subshell, so both survive the call.
run_check() {
  local root=$1
  set +e
  out=$("$root/.github/scripts/check-test-reach.sh" 2>&1)
  status=$?
  set -e
}

# passes <description>: the last run exited 0, and its output is shown only where it did not.
passes() {
  if [ "$status" -eq 0 ]; then
    check "$1" true
  else
    check "$1 (exit $status): $out" false
  fi
}

# row <root> <variable> <line…>: one more row of the registry.
row() {
  local root=$1 variable=$2
  shift 2
  printf '  %s:\n' "$variable" >> "$root/.github/test-variables.yaml"
  printf '    %s\n' "$@" >> "$root/.github/test-variables.yaml"
}

echo "case: a tree where every test reaches a job ci-ok needs"
root=$(fixture clean)
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "says so" "$(contains "$out" 'every test reaches a job ci-ok needs')"

echo "case: 1 - a build tag no required step passes"
root=$(fixture tag)
cat > "$root/packages/envoy/unit/nightly_test.go" <<'GO'
//go:build nightly

package unit

import "testing"

func TestNightly(t *testing.T) {}
GO
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the file, its line and the tag" "$(contains "$out" '^check-test-reach: tag: packages/envoy/unit/nightly_test.go:1: nightly: ')"

root=$(fixture tag-platform)
cat > "$root/packages/envoy/unit/linux_test.go" <<'GO'
//go:build linux && !race

package unit

import "testing"

func TestOnLinux(t *testing.T) {}
GO
run_check "$root"
check "a constraint that selects a platform passes" "$(is "$status" 0)"

echo "case: 2 - a test one step's -skip leaves out and no other step runs"
root=$(fixture skip)
cat >> "$root/packages/envoy/unit/unit_test.go" <<'GO'

func TestSkipped(t *testing.T) {}
GO
sed -i "s|        run: go test -v -count=1 ./...|        run: go test -v -count=1 -skip '^(TestSkipped)\$' ./...|" "$root/.github/workflows/ci.yaml"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the file, the test and the step" \
  "$(contains "$out" "^check-test-reach: skip: packages/envoy/unit/unit_test.go:11: TestSkipped: .*'s go / Unit test skips it")"

root=$(fixture skip-run-elsewhere)
cat >> "$root/packages/envoy/unit/unit_test.go" <<'GO'

func TestSkipped(t *testing.T) {}
GO
sed -i "s|        run: go test -v -count=1 ./...|        run: go test -v -count=1 -skip '^(TestSkipped)\$' ./...|" "$root/.github/workflows/ci.yaml"
cat >> "$root/.github/workflows/ci.yaml" <<'YAML'
  go-skipped:
    runs-on: ubuntu-24.04
    steps:
      - name: The skipped test
        run: go test -v -count=1 -run '^(TestSkipped)$' ./unit/
        working-directory: packages/envoy
YAML
sed -i 's|    needs: \[lint, go, bun\]|    needs: [lint, go, bun, go-skipped]|' "$root/.github/workflows/ci.yaml"
run_check "$root"
check "a skipped test another required step runs passes" "$(is "$status" 0)"

echo "case: 3 - a Go test that skips on a variable no job sets"
root=$(fixture go-variable)
cat > "$root/packages/envoy/unit/unset_test.go" <<'GO'
package unit

import (
	"os"
	"testing"
)

func TestNeedsAServer(t *testing.T) {
	server := os.Getenv("FIXTURE_SERVER")
	if server == "" {
		t.Skip("FIXTURE_SERVER names no server")
	}
}
GO
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the file, the gate's line and the variable" \
  "$(contains "$out" '^check-test-reach: variable: packages/envoy/unit/unset_test.go:10: FIXTURE_SERVER: TestNeedsAServer gates on it and no row')"

row "$root" FIXTURE_SERVER 'class: set' 'jobs: [go]' 'why: Names a server.'
run_check "$root"
check "a set row naming a job that does not set it fails" "$(is "$status" 1)"
check "names the row and the job" "$(contains "$out" 'test-variables.yaml:[0-9]*: FIXTURE_SERVER: its row names go, which does not set it')"
check "and the gate its class does not hold for" \
  "$(contains "$out" 'unset_test.go:10: FIXTURE_SERVER: its row.s class is set, and no job it names (go) runs TestNeedsAServer with it set')"

echo "case: 3 - a Bun test that skips on a variable no job sets"
root=$(fixture bun-variable)
cat > "$root/packages/web/unset.test.ts" <<'TS'
import { expect, test } from "bun:test";

const live = process.env["FIXTURE_LIVE"];

test.skipIf(live === undefined)("the live row", () => {
  expect(live).toBeDefined();
});
TS
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the file, the gate's line and the variable" \
  "$(contains "$out" '^check-test-reach: variable: packages/web/unset.test.ts:5: FIXTURE_LIVE: ')"

echo "case: 3 - a file a step's bun test runs by its path, whatever its name"
root=$(fixture bun-path)
mkdir -p "$root/packages/web/test"
cat > "$root/packages/web/test/rig.ts" <<'TS'
import { expect, test } from "bun:test";

const rig = process.env["FIXTURE_RIG"];

test.skipIf(rig === undefined)("the rig's row", () => {
  expect(rig).toBeDefined();
});
TS
cat >> "$root/.github/workflows/ci.yaml" <<'YAML'
  rig:
    runs-on: ubuntu-24.04
    steps:
      - name: The rig
        run: bun test ./test/rig.ts
        working-directory: packages/web
YAML
sed -i 's|    needs: \[lint, go, bun\]|    needs: [lint, go, bun, rig]|' "$root/.github/workflows/ci.yaml"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the file the step runs by its path, the gate's line and the variable" \
  "$(contains "$out" '^check-test-reach: variable: packages/web/test/rig.ts:5: FIXTURE_RIG: ')"

echo "case: 3 - an assertion that runs only when such a variable is set"
root=$(fixture assertion)
cat > "$root/packages/envoy/unit/measure_test.go" <<'GO'
package unit

import (
	"os"
	"testing"
	"time"
)

func TestDrainsInTime(t *testing.T) {
	started := time.Now()
	elapsed := time.Since(started)
	if os.Getenv("FIXTURE_MEASURE") == "1" && elapsed > time.Second {
		t.Errorf("drained in %s, want under a second", elapsed)
	}
}
GO
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the file, the gate's line and the variable" \
  "$(contains "$out" '^check-test-reach: variable: packages/envoy/unit/measure_test.go:12: FIXTURE_MEASURE: TestDrainsInTime gates on it')"

root=$(fixture environment-check)
cat > "$root/packages/envoy/unit/leak_test.go" <<'GO'
package unit

import (
	"os"
	"testing"
)

func TestNothingLeaksTheVariable(t *testing.T) {
	if _, set := os.LookupEnv("FIXTURE_LEAK"); set {
		t.Fatal("FIXTURE_LEAK is set")
	}
}
GO
run_check "$root"
check "an if that is itself an assertion about the environment is no gate" "$(is "$status" 0)"

echo "case: 3 - a read of a variable its package's TestMain sets for the whole test process"
root=$(fixture test-main-setting)
cat > "$root/packages/envoy/unit/own_test.go" <<'GO'
package unit

import (
	"os"
	"testing"
)

func TestReadsWhatItsTestMainSet(t *testing.T) {
	if os.Getenv("FIXTURE_OWN") != ownValue(t) {
		t.Fatal("FIXTURE_OWN is not what TestMain set")
	}
}

func ownValue(t *testing.T) string {
	t.Helper()
	return "from-main"
}
GO
run_check "$root"
check "with no TestMain setting it, the read is a gate" "$(is "$status" 1)"
check "names the file, the gate's line and the variable" \
  "$(contains "$out" '^check-test-reach: variable: packages/envoy/unit/own_test.go:9: FIXTURE_OWN: TestReadsWhatItsTestMainSet gates on it')"
cat > "$root/packages/envoy/unit/main_test.go" <<'GO'
package unit

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	if err := os.Setenv("FIXTURE_OWN", "from-main"); err != nil {
		os.Exit(1)
	}
	os.Exit(m.Run())
}
GO
run_check "$root"
check "once its package's TestMain sets it, the read is no gate" "$(is "$status" 0)"

echo "case: 3 - a message that names the variable and its value hides no gate, and a child's entry does"
root=$(fixture child-entry)
cat > "$root/packages/envoy/unit/switch_test.go" <<'GO'
package unit

import (
	"os"
	"testing"
)

const switchEnv = "FIXTURE_SWITCH"

func TestRunsOnlyWhereSwitched(t *testing.T) {
	if os.Getenv(switchEnv) != "1" {
		t.Skip("runs only where " + switchEnv + "=1: it reads this machine's own memory")
	}
}
GO
run_check "$root"
check "a skip message that spells NAME=1 leaves the read a gate" "$(is "$status" 1)"
check "names the file, the gate's line and the variable" \
  "$(contains "$out" '^check-test-reach: variable: packages/envoy/unit/switch_test.go:11: FIXTURE_SWITCH: TestRunsOnlyWhereSwitched gates on it and no row')"
cat > "$root/packages/envoy/unit/child_test.go" <<'GO'
package unit

import (
	"os"
	"os/exec"
	"testing"
)

func TestStartsItsChild(t *testing.T) {
	command := exec.Command(os.Args[0], "-test.run=^TestRunsOnlyWhereSwitched$")
	command.Env = append(os.Environ(), switchEnv+"=1")
	_ = command
}
GO
run_check "$root"
check "once a test of the package hands a child NAME=1, the read is the child's, no gate" "$(is "$status" 0)"

echo "case: 3 - the other classes, each held to its gate"
root=$(fixture prerequisite)
cat > "$root/packages/envoy/unit/prerequisite_test.go" <<'GO'
package unit

import (
	"os"
	"testing"

	"example.com/envoy/testprereq"
)

func TestNeedsATool(t *testing.T) {
	testprereq.Require(t, os.Getenv("FIXTURE_TOOL") != "", "go", "FIXTURE_TOOL names no tool")
}

func TestSkipsOnATool(t *testing.T) {
	if os.Getenv("FIXTURE_TOOL") == "" {
		t.Skip("FIXTURE_TOOL names no tool")
	}
}
GO
row "$root" FIXTURE_TOOL 'class: prerequisite' 'why: A tool the test needs.'
run_check "$root"
check "a prerequisite row holds for the gate that calls testprereq.Require, and not for a bare skip" "$(is "$status" 1)"
check "names only the bare skip" "$(contains "$out" 'prerequisite_test.go:15: FIXTURE_TOOL: its row.s class is prerequisite, and this gate neither')"
check "and not the call" "$(is "$(contains "$out" 'prerequisite_test.go:11:')" false)"

root=$(fixture tool)
cat > "$root/packages/envoy/unit/bench_test.go" <<'GO'
package unit

import (
	"os"
	"testing"
)

func BenchmarkTheCorpus(b *testing.B) {
	if os.Getenv("FIXTURE_CORPUS") == "" {
		b.Skip("FIXTURE_CORPUS names no corpus")
	}
}
GO
row "$root" FIXTURE_CORPUS 'class: tool' 'why: The corpus the benchmark reads.'
run_check "$root"
check "a tool row holds for a gate in a Benchmark" "$(is "$status" 0)"

root=$(fixture opt-out)
cat > "$root/packages/envoy/unit/optout_test.go" <<'GO'
package unit

import (
	"os"
	"testing"
)

func TestWithContainers(t *testing.T) {
	if os.Getenv("FIXTURE_NO_CONTAINERS") == "1" {
		t.Skip("this box runs no containers")
	}
}
GO
row "$root" FIXTURE_NO_CONTAINERS 'class: opt-out' 'why: A box without containers opts out.'
run_check "$root"
check "an opt-out row holds where no step that runs the test sets it" "$(is "$status" 0)"
sed -i 's|          FIXTURE_DSN: postgres://fixture|          FIXTURE_DSN: postgres://fixture\n          FIXTURE_NO_CONTAINERS: "1"|' "$root/.github/workflows/ci.yaml"
sed -i 's|        run: go test -v -count=1 ./...|        run: FIXTURE_NO_CONTAINERS=1 go test -v -count=1 ./...|' "$root/.github/workflows/ci.yaml"
run_check "$root"
check "and fails where a step that runs it sets it" "$(is "$status" 1)"
check "naming the job that opts the run out" \
  "$(contains "$out" 'optout_test.go:9: FIXTURE_NO_CONTAINERS: its row.s class is opt-out, and go runs TestWithContainers with it set')"

root=$(fixture set-by-script)
cat > "$root/packages/web/seeded.test.ts" <<'TS'
import { expect, test } from "bun:test";

test.skipIf(process.env.FIXTURE_SEED === undefined)("the seeded row", () => {
  expect(1).toBe(1);
});
TS
cat > "$root/packages/web/seed-run.sh" <<'SH'
#!/usr/bin/env bash
# Runs the seeded row with a seed of its own.
FIXTURE_SEED=/tmp/seed bun test seeded.test.ts
SH
cat >> "$root/.github/workflows/ci.yaml" <<'YAML'
  seeded:
    runs-on: ubuntu-24.04
    steps:
      - name: Seeded run
        run: ./seed-run.sh
        working-directory: packages/web
YAML
sed -i 's|    needs: \[lint, go, bun\]|    needs: [lint, go, bun, seeded]|' "$root/.github/workflows/ci.yaml"
row "$root" FIXTURE_SEED 'class: set' 'jobs: [seeded]' 'set_by_script: packages/web/seed-run.sh' 'why: The seed seed-run.sh writes.'
run_check "$root"
check "a set row whose script assigns it and that a named job runs holds" "$(is "$status" 0)"
sed -i 's|^FIXTURE_SEED=/tmp/seed ||' "$root/packages/web/seed-run.sh"
run_check "$root"
check "and fails once the script no longer assigns it" "$(is "$status" 1)"
check "naming the script" "$(contains "$out" 'FIXTURE_SEED: its row.s set_by_script names packages/web/seed-run.sh, which does not set it')"

# The shape of DISPATCH_E2E_VISUAL_SEED: the set_by_script script hands the variable to the
# container its tests run in (`docker run -e "NAME=value"`), and the job's step runs it through a
# wrapper of .github/scripts, which runs it from the step's directory.
root=$(fixture set-by-wrapped-script)
cat > "$root/packages/web/seeded.test.ts" <<'TS'
import { expect, test } from "bun:test";

test.skipIf(process.env.FIXTURE_SEED === undefined)("the seeded row", () => {
  expect(1).toBe(1);
});
TS
cat > "$root/packages/web/seed-run.sh" <<'SH'
#!/usr/bin/env bash
# Runs the seeded row in a container, with a seed of its own.
docker run --rm -e HOME -e "FIXTURE_SEED=/seed" fixture-image bun test seeded.test.ts
SH
cat > "$root/.github/scripts/seeded-suite.sh" <<'SH'
#!/usr/bin/env bash
# The seeded leg: its one run, from the step's directory.
./seed-run.sh
SH
cat >> "$root/.github/workflows/ci.yaml" <<'YAML'
  seeded:
    runs-on: ubuntu-24.04
    steps:
      - name: Seeded run
        run: '"$GITHUB_WORKSPACE/.github/scripts/seeded-suite.sh"'
        working-directory: packages/web
YAML
sed -i 's|    needs: \[lint, go, bun\]|    needs: [lint, go, bun, seeded]|' "$root/.github/workflows/ci.yaml"
row "$root" FIXTURE_SEED 'class: set' 'jobs: [seeded]' 'set_by_script: packages/web/seed-run.sh' 'why: The seed seed-run.sh hands its container.'
run_check "$root"
check "a set row whose script hands it to a container, run through a wrapper, holds" "$(is "$status" 0)"
sed -i 's|^\./seed-run\.sh$|echo "no seeded run"|' "$root/.github/scripts/seeded-suite.sh"
run_check "$root"
check "and fails once the wrapper no longer runs the script" "$(is "$status" 1)"
check "naming the script no named job runs" \
  "$(contains "$out" 'FIXTURE_SEED: no step of a job its row names runs packages/web/seed-run.sh, which sets it')"
check "and the job that no longer sets it" "$(contains "$out" 'FIXTURE_SEED: its row names seeded, which does not set it')"
sed -i 's|^echo "no seeded run"$|./seed-run.sh|' "$root/.github/scripts/seeded-suite.sh"
sed -i 's|-e "FIXTURE_SEED=/seed"|-e FIXTURE_SEED|' "$root/packages/web/seed-run.sh"
run_check "$root"
check "and fails where the script forwards the caller's value (-e NAME) and sets none" "$(is "$status" 1)"
check "naming the script that does not set it" \
  "$(contains "$out" 'FIXTURE_SEED: its row.s set_by_script names packages/web/seed-run.sh, which does not set it')"

echo "case: 3 - a step that runs a script of the repository, which exports the variable and runs the tests"
root=$(fixture script-step)
mkdir -p "$root/.github/scripts"
cat > "$root/.github/scripts/web-suite.sh" <<'SH'
#!/usr/bin/env bash
# Runs the web package's tests with the flag their flagged row reads.
export FIXTURE_FLAG=1
bun test
SH
sed -i -e 's|        run: bun test|        run: "$GITHUB_WORKSPACE/.github/scripts/web-suite.sh"|' \
  -e '/^        env:$/{N;/FIXTURE_FLAG/d}' "$root/.github/workflows/ci.yaml"
run_check "$root"
check "the script's export and its bun test are read as the step's" "$(is "$status" 0)"
sed -i 's|^export FIXTURE_FLAG=1$||' "$root/.github/scripts/web-suite.sh"
run_check "$root"
check "and the row fails once the script no longer exports it" "$(is "$status" 1)"
check "naming the job that no longer sets it" \
  "$(contains "$out" 'FIXTURE_FLAG: its row names bun, which does not set it')"

root=$(fixture stale-row)
row "$root" FIXTURE_GONE 'class: tool' 'why: A tool that is gone.'
run_check "$root"
check "a row whose variable no gate reads fails" "$(is "$status" 1)"
check "names the row" "$(contains "$out" 'FIXTURE_GONE: its row names a variable no gate of the tree reads')"

echo "case: 4 - a script that uses the words, no job runs and no line names"
root=$(fixture script)
cat > "$root/scripts/smoke-the-stack.sh" <<'SH'
#!/usr/bin/env bash
# A smoke test of the whole stack.
echo "smoke"
SH
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the script" "$(contains "$out" '^check-test-reach: script: scripts/smoke-the-stack.sh:1: smoke-the-stack.sh: ')"

sed -i '2i # legion: tool - times the stack for a person who changes it.' "$root/scripts/smoke-the-stack.sh"
run_check "$root"
check "the tool line is its home" "$(is "$status" 0)"

root=$(fixture script-not-a-test)
printf '#!/usr/bin/env bash\n# legion: not a test - starts the stack the smoke checks run on.\n# A smoke stack.\n' \
  > "$root/scripts/smoke-stack.sh"
run_check "$root"
check "the not-a-test line is its home" "$(is "$status" 0)"

root=$(fixture script-run)
printf '#!/usr/bin/env bash\n# A smoke test of the whole stack.\necho smoke\n' > "$root/scripts/smoke-run.sh"
cat >> "$root/.github/workflows/ci.yaml" <<'YAML'
  smoke:
    runs-on: ubuntu-24.04
    steps:
      - run: bash scripts/smoke-run.sh
YAML
sed -i 's|    needs: \[lint, go, bun\]|    needs: [lint, go, bun, smoke]|' "$root/.github/workflows/ci.yaml"
run_check "$root"
check "a script a required step runs passes" "$(is "$status" 0)"

echo "case: 5 - a scheduled step whose test command no required job runs"
root=$(fixture schedule)
mkdir -p "$root/packages/envoy/nightly"
printf 'package nightly\n\nimport "testing"\n\nfunc TestNightlyOnly(t *testing.T) {}\n' \
  > "$root/packages/envoy/nightly/nightly_test.go"
cat > "$root/.github/workflows/nightly.yaml" <<'YAML'
name: Nightly
on:
  schedule:
    - cron: "17 2 * * *"
  workflow_dispatch:
jobs:
  nightly:
    runs-on: ubuntu-24.04
    steps:
      - name: The nightly suite
        run: go test -v -count=1 -run '^(TestNightlyOnly)$' ./nightly/
        working-directory: packages/envoy
YAML
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the workflow, the step's line and the step" \
  "$(contains "$out" '^check-test-reach: schedule: .github/workflows/nightly.yaml:10: The nightly suite: ')"

echo "case: 5 - a step whose own if: admits only schedule, in a required job"
root=$(fixture schedule-step)
mkdir -p "$root/packages/envoy/weekly"
printf 'package weekly\n\nimport "testing"\n\nfunc TestWeeklyOnly(t *testing.T) {}\n' \
  > "$root/packages/envoy/weekly/weekly_test.go"
sed -i 's|        run: go test -v -count=1 ./...|        run: go test -v -count=1 ./unit/ ./live/|' "$root/.github/workflows/ci.yaml"
cat >> "$root/.github/workflows/ci.yaml" <<'YAML'
  weekly:
    runs-on: ubuntu-24.04
    steps:
      - name: The weekly suite
        if: github.event_name == 'schedule'
        run: go test -v -count=1 ./weekly/
        working-directory: packages/envoy
YAML
sed -i 's|    needs: \[lint, go, bun\]|    needs: [lint, go, bun, weekly]|' "$root/.github/workflows/ci.yaml"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "reads the step's if: as running its test on schedule alone" \
  "$(contains "$out" '^check-test-reach: schedule: .github/workflows/ci.yaml:[0-9]*: The weekly suite: ')"

echo "case: an input the search cannot read"
root=$(fixture unreadable)
row "$root" FIXTURE_ODD 'class: sometimes' 'why: No class.'
run_check "$root"
check "exits 2" "$(is "$status" 2)"
check "names the row" "$(contains "$out" "FIXTURE_ODD's class is 'sometimes'")"

# The forms a go test takes in the layout of matrix jobs and composite actions. Each fixture's test
# file carries a tag that only the form under test passes, so a search that does not read the form
# names the file as a tag no required step passes, and the case fails.
echo "case: a test whose only go test is a matrix value of a job"
root=$(fixture go-matrix)
go_test_file "$root/packages/envoy/unit/matrix_test.go" unit matrixtag TestOnlyInTheMatrix
required_job "$root" matrix '    runs-on: ubuntu-24.04
    strategy:
      fail-fast: false
      matrix:
        include:
          - leg: tagged
            command: go test -count=1 -tags matrixtag ./unit/
    steps:
      - name: The leg'"'"'s tests
        run: ${{ matrix.command }}
        working-directory: packages/envoy'
run_check "$root"
passes "go-matrix: the go test in the matrix value is read as a run"

echo "case: a test whose only go test is in a script a job runs"
root=$(fixture go-script)
go_test_file "$root/packages/envoy/unit/script_test.go" unit scripttag TestOnlyInTheScript
cat > "$root/scripts/tagged-tests.sh" <<'SH'
#!/usr/bin/env bash
# Runs the unit package's scripttag tests.
set -euo pipefail
cd "$(dirname "$0")/../packages/envoy"
go test -count=1 -tags scripttag ./unit/
SH
chmod +x "$root/scripts/tagged-tests.sh"
required_job "$root" scripted '    runs-on: ubuntu-24.04
    steps:
      - name: The script'"'"'s tests
        run: scripts/tagged-tests.sh'
run_check "$root"
passes "go-script: the script's go test, in the directory it moves to, is read as a run"

echo "case: a test whose only go test is in a composite action a job runs"
root=$(fixture go-action)
go_test_file "$root/packages/envoy/unit/action_test.go" unit actiontag TestOnlyInTheAction
mkdir -p "$root/.github/actions/tagged-tests"
cat > "$root/.github/actions/tagged-tests/action.yml" <<'YAML'
name: Tagged tests
description: Runs the unit package's tests under the tags its user names.
inputs:
  tags:
    description: The build tags.
    required: true
runs:
  using: composite
  steps:
    - name: The tagged tests
      shell: bash
      run: go test -count=1 -tags ${{ inputs.tags }} ./unit/
      working-directory: packages/envoy
YAML
required_job "$root" action '    runs-on: ubuntu-24.04
    steps:
      - name: The action'"'"'s tests
        uses: ./.github/actions/tagged-tests
        with:
          tags: actiontag'
run_check "$root"
passes "go-action: the action's go test, under the input its user gives, is read as a run"

echo "case: a variable an action's step writes into \$GITHUB_ENV, read by that action's later step and the job's"
root=$(fixture action-env)
for package in inner later; do
  mkdir -p "$root/packages/envoy/$package"
  cat > "$root/packages/envoy/$package/${package}_test.go" <<GO
package $package

import (
	"os"
	"testing"
)

func TestNeedsTheActionsServer(t *testing.T) {
	if os.Getenv("FIXTURE_ACTION_SERVER") == "" {
		t.Skip("FIXTURE_ACTION_SERVER names no server")
	}
}
GO
done
mkdir -p "$root/.github/actions/server-then-test"
cat > "$root/.github/actions/server-then-test/action.yml" <<'YAML'
name: Server then test
description: Names the server, then runs the inner package's tests against it.
runs:
  using: composite
  steps:
    - name: Name the server
      shell: bash
      run: echo "FIXTURE_ACTION_SERVER=postgres://action" >> "$GITHUB_ENV"
    - name: The inner tests
      shell: bash
      run: go test -count=1 ./inner/
      working-directory: packages/envoy
YAML
required_job "$root" action-env '    runs-on: ubuntu-24.04
    steps:
      - name: The server and the inner tests
        uses: ./.github/actions/server-then-test
      - name: The later tests
        run: go test -count=1 ./later/
        working-directory: packages/envoy'
row "$root" FIXTURE_ACTION_SERVER 'class: set' 'jobs: [action-env]' 'why: Names the server the action starts.'
run_check "$root"
passes "action-env: the write reaches the action's later step and the job's later step"
# The write after the inner tests reaches the job's later step and not the step before it.
cat > "$root/.github/actions/server-then-test/action.yml" <<'YAML'
name: Server then test
description: Runs the inner package's tests, then names the server.
runs:
  using: composite
  steps:
    - name: The inner tests
      shell: bash
      run: go test -count=1 ./inner/
      working-directory: packages/envoy
    - name: Name the server
      shell: bash
      run: echo "FIXTURE_ACTION_SERVER=postgres://action" >> "$GITHUB_ENV"
YAML
run_check "$root"
check "action-env: a write after the action's step fails" "$(is "$status" 1)"
check "action-env: names the test the step before the write runs" \
  "$(contains "$out" 'packages/envoy/inner/inner_test.go:[0-9]*: FIXTURE_ACTION_SERVER: its row.s class is set, and no job it names (action-env) runs TestNeedsTheActionsServer with it set')"
check "action-env: and not the test of the job's later step" "$(is "$(contains "$out" 'later_test.go')" false)"

echo "case: a test whose only go test is an input a job gives a called workflow"
root=$(fixture go-called-input)
go_test_file "$root/packages/envoy/unit/input_test.go" unit inputtag TestOnlyInTheInput
cat > "$root/.github/workflows/called-tests.yaml" <<'YAML'
name: Called tests
on:
  workflow_call:
    inputs:
      command:
        type: string
        required: true
jobs:
  tests:
    runs-on: ubuntu-24.04
    steps:
      - name: The caller's command
        run: ${{ inputs.command }}
        working-directory: packages/envoy
YAML
required_job "$root" called '    uses: ./.github/workflows/called-tests.yaml
    with:
      command: go test -count=1 -tags inputtag ./unit/'
run_check "$root"
passes "go-called-input: the go test the caller's input gives is read as a run"

echo "case: a test that a -skip passed through gotestsum, after its --, leaves out"
root=$(fixture gotestsum-skip)
cat >> "$root/packages/envoy/unit/unit_test.go" <<'GO'

func TestSkipped(t *testing.T) {}
GO
sed -i "s|        run: go test -v -count=1 ./...|        run: gotestsum --format standard-verbose --jsonfile unit.json --packages ./... -- -count=1 -skip '^(TestSkipped)\$'|" \
  "$root/.github/workflows/ci.yaml"
run_check "$root"
check "gotestsum-skip: fails" "$(is "$status" 1)"
check "gotestsum-skip: names TestSkipped and the step whose gotestsum skips it" \
  "$(contains "$out" "^check-test-reach: skip: packages/envoy/unit/unit_test.go:11: TestSkipped: .*'s go / Unit test skips it")"

echo "case: a test file whose build tag only a -tags passed through gotestsum names"
root=$(fixture gotestsum-tags)
go_test_file "$root/packages/envoy/unit/gotestsum_test.go" unit gotestsumtag TestOnlyThroughGotestsum
required_job "$root" gotestsum '    runs-on: ubuntu-24.04
    steps:
      - name: The tagged tests through gotestsum
        run: gotestsum --format standard-verbose --jsonfile tagged.json --packages "./unit/" -- -count=1 -tags gotestsumtag
        working-directory: packages/envoy'
run_check "$root"
passes "gotestsum-tags: the -tags after gotestsum's -- builds the file"

# legs_job <root> <job id>: a required job of two legs whose go test step runs, in each leg, the
# packages .github/go-legs.yaml lists for that leg, as .github/scripts/go_legs.py prints them.
legs_job() {
  required_job "$1" "$2" '    runs-on: ubuntu-24.04
    strategy:
      fail-fast: false
      matrix:
        leg: [1, 2]
    steps:
      - name: The leg'"'"'s packages
        run: |
          set -o pipefail
          python3 ../../.github/scripts/go_legs.py packages/envoy '"$2"' ${{ matrix.leg }} ${{ strategy.job-total }} | xargs go test -count=1
        working-directory: packages/envoy'
}

echo "case: a legs job's step is read as running every package its job's legs list, a split one whole"
root=$(fixture legs)
unit_only "$root"
go_test_file "$root/packages/envoy/one/one_test.go" one "" TestInTheFirstLeg
go_test_file "$root/packages/envoy/two/two_test.go" two "" TestInTheSecondLeg
go_test_file "$root/packages/envoy/split/split_test.go" split "" TestInTheFirstGroup TestInTheSecondGroup
cat > "$root/.github/go-legs.yaml" <<'YAML'
packages/envoy:
  legs:
    1:
      - one
      - split: 1
    2:
      - two
      - split: 2
  elsewhere:
    unit: go
    live: go
YAML
legs_job "$root" legs
run_check "$root"
passes "legs: the union of the job's legs runs one, two and both groups of split"

echo "case: a package no leg of its job lists"
root=$(fixture legs-missing)
unit_only "$root"
go_test_file "$root/packages/envoy/one/one_test.go" one "" TestInTheFirstLeg
go_test_file "$root/packages/envoy/three/three_test.go" three "" TestInNoLeg
printf 'packages/envoy:\n  legs:\n    1:\n      - one\n    2: []\n' > "$root/.github/go-legs.yaml"
legs_job "$root" legs
run_check "$root"
check "legs-missing: fails" "$(is "$status" 1)"
check "legs-missing: names the test of the package no leg lists" \
  "$(contains "$out" '^check-test-reach: unrun: packages/envoy/three/three_test.go:[0-9]*: TestInNoLeg: no required step.s go test runs its package, packages/envoy/three$')"
check "legs-missing: and not the test of the package a leg lists" "$(is "$(contains "$out" 'TestInTheFirstLeg')" false)"

echo "case: a package no place names runs in the legs of the job the module's unlisted names"
root=$(fixture legs-unlisted)
unit_only "$root"
go_test_file "$root/packages/envoy/one/one_test.go" one "" TestInTheFirstLeg
go_test_file "$root/packages/envoy/three/three_test.go" three "" TestInTheLastLegUnlisted
printf 'packages/envoy:\n  unlisted: legs\n  legs:\n    1:\n      - one\n    2: []\n' > "$root/.github/go-legs.yaml"
legs_job "$root" legs
run_check "$root"
passes "legs-unlisted: go_legs.py's last leg of the unlisted job runs three"
printf 'packages/envoy:\n  unlisted: other\n  other:\n    1:\n      - unit\n  legs:\n    1:\n      - one\n    2: []\n' > "$root/.github/go-legs.yaml"
run_check "$root"
check "legs-unlisted: an unlisted naming another job leaves three unrun" \
  "$(contains "$out" '^check-test-reach: unrun: packages/envoy/three/three_test.go:[0-9]*: TestInTheLastLegUnlisted: no required step.s go test runs its package, packages/envoy/three$')"

echo "case: a legs step whose job go-legs.yaml lists no legs of"
root=$(fixture legs-no-job)
printf 'packages/envoy:\n  other:\n    1:\n      - unit\n' > "$root/.github/go-legs.yaml"
legs_job "$root" legs
run_check "$root"
check "legs-no-job: exits 2, as an input it cannot read" "$(is "$status" 2)"
check "legs-no-job: names the job and the file" \
  "$(contains "$out" 'cannot read an input: .*legs / The leg.s packages: .github/go-legs.yaml lists no legs of legs under packages/envoy')"

echo "case: a go test a step runs under the race memory sampler"
root=$(fixture race-memory)
go_test_file "$root/packages/envoy/unit/sampled_test.go" unit sampledtag TestOnlyUnderTheSampler
required_job "$root" sampled '    runs-on: ubuntu-24.04
    steps:
      - name: Race-test under the sampler
        run: GODEBUG=checkfinalizers=1 RACE_MEM_LIMIT_KB=8388608 ../../.github/scripts/race_memory.py go test -json -race -count=1 -tags sampledtag ./unit/
        working-directory: packages/envoy'
run_check "$root"
passes "race-memory: the go test behind race_memory.py is read, its -tags among it"

# Workflows ci-ok does not need, each of whose steps that runs a test file is read.
echo "case: a workflow run on a pull request only when its own files change, with a preload"
root=$(fixture pr-preload)
printf '%s\n' '{ "name": "web", "scripts": { "test": "bun test", "test:cold": "bun test --preload ./hold.ts" } }' \
  > "$root/packages/web/package.json"
printf 'export {};\n' > "$root/packages/web/hold.ts"
cat > "$root/.github/workflows/web-cold.yaml" <<'YAML'
name: Web cold
on:
  pull_request:
    paths:
      - .github/workflows/web-cold.yaml
      - packages/web/hold.ts
jobs:
  cold:
    runs-on: ubuntu-24.04
    steps:
      - name: The web tests with the preload
        run: bun run test:cold
        working-directory: packages/web
YAML
run_check "$root"
check "pr-preload: fails" "$(is "$status" 1)"
check "pr-preload: names the workflow, the step and its command" \
  "$(contains "$out" '^check-test-reach: outside: .github/workflows/web-cold.yaml:[0-9]*: The web tests with the preload: it runs `bun run test:cold` in a workflow no merge waits for')"
sed -i '1a # legion: tool - runs the web tests with a preload, for a person who chases a late load.' "$root/.github/workflows/web-cold.yaml"
run_check "$root"
check "pr-preload: still fails with the tool line, since no required job passes that preload" "$(is "$status" 1)"
check "pr-preload: names the preload no required job passes" \
  "$(contains "$out" '^check-test-reach: outside: .github/workflows/web-cold.yaml:[0-9]*: The web tests with the preload: its workflow carries the tool line, and it runs packages/web/flag.test.ts with --preload packages/web/hold.ts')"

echo "case: the same kind of workflow with a repeat count alone, and the tool line"
root=$(fixture pr-repeat-tool)
printf '%s\n' '{ "name": "web", "scripts": { "test": "bun test", "test:repeat": "bun test --rerun-each=20" } }' \
  > "$root/packages/web/package.json"
cat > "$root/.github/workflows/web-repeat.yaml" <<'YAML'
name: Web repeated
# legion: tool - repeats the web tests to find the rows that fail only sometimes, for a person who fixes one.
on:
  pull_request:
    paths:
      - .github/workflows/web-repeat.yaml
jobs:
  repeat:
    runs-on: ubuntu-24.04
    steps:
      - name: The web tests twenty times
        run: bun run test:repeat
        working-directory: packages/web
YAML
run_check "$root"
passes "pr-repeat-tool: a repeat count of what a required job runs, in a tool workflow, passes"
sed -i '/legion: tool - /d' "$root/.github/workflows/web-repeat.yaml"
run_check "$root"
check "pr-repeat-tool: and fails without the tool line" "$(is "$status" 1)"
check "pr-repeat-tool: names the step" \
  "$(contains "$out" '^check-test-reach: outside: .github/workflows/web-repeat.yaml:[0-9]*: The web tests twenty times: it runs `bun run test:repeat` in a workflow no merge waits for')"

echo "case: a workflow run only on schedule, with a step that runs a test file"
root=$(fixture schedule-only)
mkdir -p "$root/packages/other"
printf '{ "name": "other", "scripts": { "test": "bun test" } }\n' > "$root/packages/other/package.json"
cat > "$root/packages/other/nightly.test.ts" <<'TS'
import { expect, test } from "bun:test";

test("the nightly row", () => {
  expect(1).toBe(1);
});
TS
cat > "$root/.github/workflows/nightly-bun.yaml" <<'YAML'
name: Nightly Bun
on:
  schedule:
    - cron: "17 3 * * *"
jobs:
  nightly:
    runs-on: ubuntu-24.04
    steps:
      - name: The other package's tests
        run: bun test ./nightly.test.ts
        working-directory: packages/other
YAML
run_check "$root"
check "schedule-only: fails" "$(is "$status" 1)"
check "schedule-only: names the workflow, the step and its command" \
  "$(contains "$out" '^check-test-reach: schedule: .github/workflows/nightly-bun.yaml:[0-9]*: The other package.s tests: it runs `bun test ./nightly.test.ts` only on schedule or workflow_dispatch')"

echo "case: a workflow run only by workflow_dispatch, with a step that runs a test file"
root=$(fixture dispatch-only)
go_test_file "$root/packages/envoy/ondemand/ondemand_test.go" ondemand "" TestOnDemand
cat > "$root/.github/workflows/on-demand.yaml" <<'YAML'
name: On demand
on: workflow_dispatch
jobs:
  ondemand:
    runs-on: ubuntu-24.04
    steps:
      - name: The on-demand suite
        run: go test -count=1 -run '^(TestOnDemand)$' ./ondemand/
        working-directory: packages/envoy
YAML
run_check "$root"
check "dispatch-only: fails" "$(is "$status" 1)"
check "dispatch-only: names the workflow, the step and its command" \
  "$(contains "$out" '^check-test-reach: schedule: .github/workflows/on-demand.yaml:[0-9]*: The on-demand suite: it runs `go test')"

echo "case: a workflow no merge waits for whose job calls a workflow that runs a test file"
root=$(fixture called-workflow)
mkdir -p "$root/packages/other"
printf '{ "name": "other", "scripts": { "test": "bun test" } }\n' > "$root/packages/other/package.json"
cat > "$root/packages/other/other.test.ts" <<'TS'
import { expect, test } from "bun:test";

test("the other row", () => {
  expect(1).toBe(1);
});
TS
cat > "$root/.github/workflows/other-suite.yaml" <<'YAML'
name: Other suite
on: workflow_call
jobs:
  suite:
    runs-on: ubuntu-24.04
    steps:
      - name: The other package's tests
        run: bun test
        working-directory: packages/other
YAML
cat > "$root/.github/workflows/other.yaml" <<'YAML'
name: Other
on:
  pull_request:
    paths:
      - packages/other/**
jobs:
  call:
    uses: ./.github/workflows/other-suite.yaml
YAML
run_check "$root"
check "called-workflow: fails" "$(is "$status" 1)"
check "called-workflow: names the called workflow's step and its command" \
  "$(contains "$out" '^check-test-reach: outside: .github/workflows/other-suite.yaml:[0-9]*: The other package.s tests: it runs `bun test` in a workflow no merge waits for')"

echo "case: two shell steps that run no test file and no script are not read"
root=$(fixture release-stamp)
cat > "$root/.github/workflows/release.yaml" <<'YAML'
name: Release
on:
  push:
    branches: [main]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Check the stamped version
        run: |
          version=$(out/tool --version)
          test "$version" = "tool v1"
YAML
run_check "$root"
passes "release-stamp: a release step that checks a version it stamped passes"

root=$(fixture schedule-shell)
cat > "$root/.github/workflows/weekly-disk.yaml" <<'YAML'
name: Weekly disk
on:
  schedule:
    - cron: "41 4 * * 1"
jobs:
  disk:
    runs-on: ubuntu-24.04
    steps:
      - name: Read the runner's disk
        run: df -h && echo "the runner's disk"
YAML
run_check "$root"
passes "schedule-shell: a scheduled shell step that runs no test passes"

# Release workflows: a step that runs a check passes where the list of release checks names it,
# with what it checks; a go test there must read what its own job built.
# smoke_package <root>: a smoke test a required job runs too, as every test of the tree is run.
smoke_package() {
  go_test_file "$1/packages/envoy/smoke/smoke_test.go" smoke smoke TestTheImageAnswers
  required_job "$1" smoke '    runs-on: ubuntu-24.04
    steps:
      - name: Smoke-test the image of the tree
        run: go test -count=1 -tags smoke ./smoke/
        working-directory: packages/envoy'
}
echo "case: a release workflow's step that the list of release checks names"
root=$(fixture release-listed)
smoke_package "$root"
cat > "$root/.github/workflows/release.yaml" <<'YAML'
name: Release
on:
  push:
    branches: [main]
jobs:
  image:
    runs-on: ubuntu-24.04
    env:
      FIXTURE_SMOKE_IMAGE: fixture-smoke:latest
    steps:
      - name: Build the image the smoke test runs against
        run: docker build -t "$FIXTURE_SMOKE_IMAGE" .
      - name: Smoke-test the built image
        run: go test -count=1 -tags smoke ./smoke/
        working-directory: packages/envoy
YAML
cat > "$root/.github/release-checks.yaml" <<'YAML'
checks:
  - workflow: .github/workflows/release.yaml
    job: image
    step: Smoke-test the built image
    artifact: the image the job's build step tagged FIXTURE_SMOKE_IMAGE
YAML
run_check "$root"
passes "release-listed: a listed step whose go test reads what its job built passes"

echo "case: a release workflow's step that runs go test over the source, unlisted"
root=$(fixture release-go-source)
cat > "$root/.github/workflows/release.yaml" <<'YAML'
name: Release
on:
  push:
    branches: [main]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - name: Unit test before the release
        run: go test -count=1 ./unit/
        working-directory: packages/envoy
YAML
run_check "$root"
check "release-go-source: fails" "$(is "$status" 1)"
check "release-go-source: names the release step and its go test" \
  "$(contains "$out" '^check-test-reach: release: .github/workflows/release.yaml:[0-9]*: Unit test before the release: it runs `go test -count=1 ./unit/` over the tree.s source in a release workflow')"

echo "case: a listed release step whose go test reads no variable its own job built"
root=$(fixture release-unbuilt)
smoke_package "$root"
cat > "$root/.github/workflows/release.yaml" <<'YAML'
name: Release
on:
  push:
    branches: [main]
jobs:
  image:
    runs-on: ubuntu-24.04
    env:
      FIXTURE_SMOKE_TARGET: example
    steps:
      - name: Get ready
        run: echo ready
      - name: Smoke-test the target
        run: go test -count=1 -tags smoke ./smoke/
        working-directory: packages/envoy
YAML
cat > "$root/.github/release-checks.yaml" <<'YAML'
checks:
  - workflow: .github/workflows/release.yaml
    job: image
    step: Smoke-test the target
    artifact: the target
YAML
run_check "$root"
check "release-unbuilt: fails" "$(is "$status" 1)"
check "release-unbuilt: names the entry" \
  "$(contains "$out" '^check-test-reach: release: .github/release-checks.yaml:2: .github/workflows/release.yaml / image / Smoke-test the target: it runs go test, and its environment holds no variable an earlier step of its job built')"

# Race-impossible tests: a //go:build !race file, and a test that reads raceEnabled, count as run
# only by a step without -race whose -run and -skip select them; a //go:build race file only by
# a -race step.
race_job() {
  required_job "$1" race '    runs-on: ubuntu-24.04
    steps:
      - name: Race-test the racy package
        run: go test -count=1 -race ./racy/
        working-directory: packages/envoy'
}
echo "case: a test in a //go:build !race file of a package only a -race step runs"
root=$(fixture norace-file)
unit_only "$root"
go_test_file "$root/packages/envoy/racy/racy_test.go" racy "" TestUnderTheDetector
go_test_file "$root/packages/envoy/racy/norace_test.go" racy '!race' TestTooSlowForTheDetector
race_job "$root"
run_check "$root"
check "norace-file: fails" "$(is "$status" 1)"
check "norace-file: names TestTooSlowForTheDetector" \
  "$(contains "$out" '^check-test-reach: unrun: packages/envoy/racy/norace_test.go:[0-9]*: TestTooSlowForTheDetector: ')"
check "norace-file: and not the test the -race step runs" "$(is "$(contains "$out" 'TestUnderTheDetector')" false)"

echo "case: a test that reads raceEnabled in a package only a -race step runs"
root=$(fixture race-enabled)
unit_only "$root"
mkdir -p "$root/packages/envoy/racy"
printf '//go:build race\n\npackage racy\n\nconst raceEnabled = true\n' > "$root/packages/envoy/racy/race_enabled_test.go"
printf '//go:build !race\n\npackage racy\n\nconst raceEnabled = false\n' > "$root/packages/envoy/racy/race_disabled_test.go"
cat > "$root/packages/envoy/racy/storm_test.go" <<'GO'
package racy

import "testing"

func TestTheStormAtFullSize(t *testing.T) {
	connections := 500
	if raceEnabled {
		connections = 200
	}
	_ = connections
}
GO
race_job "$root"
run_check "$root"
check "race-enabled: fails" "$(is "$status" 1)"
check "race-enabled: names TestTheStormAtFullSize and why" \
  "$(contains "$out" '^check-test-reach: unrun: packages/envoy/racy/storm_test.go:5: TestTheStormAtFullSize: it reads raceEnabled')"
required_job "$root" storm '    runs-on: ubuntu-24.04
    steps:
      - name: The storm at full size
        run: go test -count=1 -run '"'"'^(TestTheStormAtFullSize)$'"'"' ./racy/
        working-directory: packages/envoy'
run_check "$root"
passes "race-enabled: and passes once a step without -race selects it"

echo "case: a test added to bus/norace_test.go while the plain step's -run list stays as it was"
root=$(fixture bus-norace)
unit_only "$root"
go_test_file "$root/packages/envoy/internal/bus/bus_test.go" bus "" TestTheBusDelivers
go_test_file "$root/packages/envoy/internal/bus/norace_test.go" bus '!race' TestTheFloodAtFullSize
required_job "$root" bus '    runs-on: ubuntu-24.04
    steps:
      - name: Race-test the bus
        run: go test -count=1 -race ./internal/bus/
        working-directory: packages/envoy
      - name: Test what the race build cannot run
        run: go test -count=1 -run '"'"'^(TestTheFloodAtFullSize)$'"'"' ./internal/bus/
        working-directory: packages/envoy'
run_check "$root"
passes "bus-norace: the bus as the -run list names it passes"
printf '\nfunc TestAddedLater(t *testing.T) {}\n' >> "$root/packages/envoy/internal/bus/norace_test.go"
run_check "$root"
check "bus-norace: fails once norace_test.go holds a test the -run list does not name" "$(is "$status" 1)"
check "bus-norace: names TestAddedLater" \
  "$(contains "$out" '^check-test-reach: unrun: packages/envoy/internal/bus/norace_test.go:[0-9]*: TestAddedLater: each required step that builds it')"

echo "case: a test in a //go:build race file of a package no -race step runs"
root=$(fixture race-file)
go_test_file "$root/packages/envoy/racy/racy_test.go" racy "" TestPlain
go_test_file "$root/packages/envoy/racy/race_only_test.go" racy race TestOnlyUnderTheDetector
run_check "$root"
check "race-file: fails" "$(is "$status" 1)"
check "race-file: names TestOnlyUnderTheDetector" \
  "$(contains "$out" '^check-test-reach: unrun: packages/envoy/racy/race_only_test.go:[0-9]*: TestOnlyUnderTheDetector: `//go:build race` holds for none')"

echo "case: a Go module with a test no step runs"
root=$(fixture module-unrun)
mkdir -p "$root/tools/extra"
printf 'module example.com/extra\n\ngo 1.27\n' > "$root/tools/extra/go.mod"
go_test_file "$root/tools/extra/extra_test.go" extra "" TestTheExtraModule
run_check "$root"
check "module-unrun: fails" "$(is "$status" 1)"
check "module-unrun: names TestTheExtraModule" \
  "$(contains "$out" '^check-test-reach: unrun: tools/extra/extra_test.go:[0-9]*: TestTheExtraModule: no required step.s go test runs its package, tools/extra')"

echo "case: a package the only step that runs its module leaves out by its exclusion list"
root=$(fixture excluded-package)
go_test_file "$root/packages/envoy/excluded/excluded_test.go" excluded "" TestLeftOut
sed -i "s#        run: go test -v -count=1 ./...#        run: go list ./... | grep -Ev '/excluded\$' | xargs go test -v -count=1#" \
  "$root/.github/workflows/ci.yaml"
run_check "$root"
check "excluded-package: fails" "$(is "$status" 1)"
check "excluded-package: names TestLeftOut" \
  "$(contains "$out" '^check-test-reach: unrun: packages/envoy/excluded/excluded_test.go:[0-9]*: TestLeftOut: no required step.s go test runs its package')"
check "excluded-package: and not the tests the list keeps" "$(is "$(contains "$out" 'TestUnit')" false)"

# Constraints and file-name suffixes, against each test step's GOOS, GOARCH and tags. A go vet
# for darwin compiles the package for darwin and runs none of its tests.
vet_darwin() {
  required_job "$1" vet-darwin '    runs-on: ubuntu-24.04
    steps:
      - name: Vet the unit package for darwin
        run: GOOS=darwin go vet ./unit/
        working-directory: packages/envoy'
}
echo "case: a //go:build darwin test file in a package whose only darwin step is a go vet"
root=$(fixture darwin-build)
go_test_file "$root/packages/envoy/unit/darwin_build_test.go" unit darwin TestOnDarwin
vet_darwin "$root"
run_check "$root"
check "darwin-build: fails" "$(is "$status" 1)"
check "darwin-build: names TestOnDarwin" \
  "$(contains "$out" '^check-test-reach: unrun: packages/envoy/unit/darwin_build_test.go:[0-9]*: TestOnDarwin: `//go:build darwin` holds for none')"
required_job "$root" darwin '    runs-on: macos-15
    steps:
      - name: Test the unit package on darwin
        run: go test -count=1 ./unit/
        working-directory: packages/envoy'
run_check "$root"
passes "darwin-build: and passes once a step on a macos runner runs the package"

echo "case: a *_darwin_test.go file in a package whose only darwin step is a go vet"
root=$(fixture darwin-name)
go_test_file "$root/packages/envoy/unit/prompt_darwin_test.go" unit "" TestThePromptOnDarwin
vet_darwin "$root"
run_check "$root"
check "darwin-name: fails" "$(is "$status" 1)"
check "darwin-name: names TestThePromptOnDarwin" \
  "$(contains "$out" '^check-test-reach: unrun: packages/envoy/unit/prompt_darwin_test.go:[0-9]*: TestThePromptOnDarwin: its file name.s `darwin` holds for none')"

echo "case: a test file whose //go:build constraint no step that runs tests meets"
root=$(fixture constraint-unmet)
go_test_file "$root/packages/envoy/unit/s390x_test.go" unit 'linux && s390x' TestOnS390x
run_check "$root"
check "constraint-unmet: fails" "$(is "$status" 1)"
check "constraint-unmet: names TestOnS390x" \
  "$(contains "$out" '^check-test-reach: unrun: packages/envoy/unit/s390x_test.go:[0-9]*: TestOnS390x: `//go:build linux && s390x` holds for none')"

# A tool a step reaches through `bun run`: what it runs is read through the `legion: runs - `
# lines of its header, and a package script its source names that no such line names fails.
echo "case: a tool that names, in a runs line, a package script with a preload no required job passes"
root=$(fixture tool-runs-preload)
printf '%s\n' '{ "name": "web", "scripts": { "test": "bun test", "test:cold": "bun test --preload ./hold.ts", "repeat": "bun tools/repeat.ts" } }' \
  > "$root/packages/web/package.json"
printf 'export {};\n' > "$root/packages/web/hold.ts"
mkdir -p "$root/packages/web/tools"
cat > "$root/packages/web/tools/repeat.ts" <<'TS'
// legion: tool - repeats a lane of the web tests, for a person who chases a row that fails only sometimes.
// legion: runs - test:cold

console.log("repeating the cold lane");
TS
cat > "$root/.github/workflows/web-repeat.yaml" <<'YAML'
name: Web repeated by the tool
# legion: tool - repeats the web tests' cold lane through the repeat tool, for a person who chases a row.
on:
  pull_request:
    paths:
      - .github/workflows/web-repeat.yaml
jobs:
  repeat:
    runs-on: ubuntu-24.04
    steps:
      - name: The cold lane through the tool
        run: bun run repeat
        working-directory: packages/web
YAML
run_check "$root"
check "tool-runs-preload: fails" "$(is "$status" 1)"
check "tool-runs-preload: names the step and the preload the tool's runs line comes to" \
  "$(contains "$out" '^check-test-reach: outside: .github/workflows/web-repeat.yaml:[0-9]*: The cold lane through the tool: its workflow carries the tool line, and it runs packages/web/flag.test.ts with --preload packages/web/hold.ts')"

# dispatch-unit-repeat.yaml's lane step starts its tool through `exec timeout`, so the reader reads
# past those words to the `bun run` they start, and on through the tool's runs lines.
echo "case: a tool a step starts through exec and timeout, whose runs line has a preload no required job passes"
root=$(fixture tool-runs-timeout)
printf '%s\n' '{ "name": "web", "scripts": { "test": "bun test", "test:cold": "bun test --preload ./hold.ts", "repeat": "bun tools/repeat.ts" } }' \
  > "$root/packages/web/package.json"
printf 'export {};\n' > "$root/packages/web/hold.ts"
mkdir -p "$root/packages/web/tools"
cat > "$root/packages/web/tools/repeat.ts" <<'TS'
// legion: tool - repeats a lane of the web tests, for a person who chases a row that fails only sometimes.
// legion: runs - test:cold

console.log("repeating the cold lane");
TS
cat > "$root/.github/workflows/web-repeat.yaml" <<'YAML'
name: Web repeated by the tool under a deadline
# legion: tool - repeats the web tests' cold lane through the repeat tool, for a person who chases a row.
on:
  schedule:
    - cron: "5 6 * * 2"
jobs:
  repeat:
    runs-on: ubuntu-24.04
    strategy:
      matrix:
        include:
          - {lane: cold, deadline_seconds: 600}
    steps:
      - name: The cold lane through the tool, under a deadline
        run: >-
          exec timeout --foreground --signal=TERM --kill-after=60 ${{ matrix.deadline_seconds }}
          bun run repeat --lane ${{ matrix.lane }}
        working-directory: packages/web
YAML
run_check "$root"
check "tool-runs-timeout: fails" "$(is "$status" 1)"
check "tool-runs-timeout: names the step and the preload the tool's runs line comes to" \
  "$(contains "$out" '^check-test-reach: schedule: .github/workflows/web-repeat.yaml:[0-9]*: The cold lane through the tool, under a deadline: its workflow carries the tool line, and it runs packages/web/flag.test.ts with --preload packages/web/hold.ts')"
sed -i 's|exec timeout --foreground --signal=TERM --kill-after=60|exec timeout -s TERM -k 60 --preserve-status -v|' \
  "$root/.github/workflows/web-repeat.yaml"
run_check "$root"
check "tool-runs-timeout: and with timeout's short options, whose values are words of their own" \
  "$(contains "$out" '^check-test-reach: schedule: .github/workflows/web-repeat.yaml:[0-9]*: The cold lane through the tool, under a deadline: its workflow carries the tool line, and it runs packages/web/flag.test.ts with --preload packages/web/hold.ts')"

echo "case: a step that uses a local action whose step runs that test"
root=$(fixture action-runs-preload)
printf '%s\n' '{ "name": "web", "scripts": { "test": "bun test", "test:cold": "bun test --preload ./hold.ts" } }' \
  > "$root/packages/web/package.json"
printf 'export {};\n' > "$root/packages/web/hold.ts"
mkdir -p "$root/.github/actions/cold-lane"
cat > "$root/.github/actions/cold-lane/action.yml" <<'YAML'
name: Cold lane
description: Runs the web tests with the preload that holds each lazy load back.
runs:
  using: composite
  steps:
    - name: The cold lane
      shell: bash
      run: bun run test:cold
      working-directory: packages/web
YAML
cat > "$root/.github/workflows/web-cold.yaml" <<'YAML'
name: Web cold
# legion: tool - runs the web tests' cold lane, for a person who chases a late load.
on:
  schedule:
    - cron: "5 6 * * 2"
jobs:
  cold:
    runs-on: ubuntu-24.04
    steps:
      - name: The cold lane through the action
        uses: ./.github/actions/cold-lane
YAML
run_check "$root"
check "action-runs-preload: fails" "$(is "$status" 1)"
check "action-runs-preload: names the step that uses the action, and the preload" \
  "$(contains "$out" '^check-test-reach: schedule: .github/workflows/web-cold.yaml:[0-9]*: The cold lane through the action (.github/actions/cold-lane/action.yml): its workflow carries the tool line, and it runs packages/web/flag.test.ts with --preload packages/web/hold.ts')"

echo "case: a tool whose source names a package script that no runs line names"
root=$(fixture tool-unlisted)
printf '%s\n' '{ "name": "web", "scripts": { "test": "bun test", "check-all": "bun tools/check-all.ts" } }' \
  > "$root/packages/web/package.json"
mkdir -p "$root/packages/web/tools"
cat > "$root/packages/web/tools/check-all.ts" <<'TS'
// legion: tool - runs the web package's tests before a commit, for a person who runs it by hand.

const script = "test";
Bun.spawnSync(["bun", "run", script]);
TS
required_job "$root" check-all '    runs-on: ubuntu-24.04
    steps:
      - name: Check all of the web package
        run: bun run check-all
        working-directory: packages/web'
run_check "$root"
check "tool-unlisted: fails" "$(is "$status" 1)"
check "tool-unlisted: names the tool and the script" \
  "$(contains "$out" '^check-test-reach: runs: packages/web/tools/check-all.ts:3: test: the tool names the package script test of packages/web/package.json in a string, and no `legion: runs - ` line')"
sed -i '1a // legion: runs - test' "$root/packages/web/tools/check-all.ts"
run_check "$root"
passes "tool-unlisted: and passes once a runs line names it"

# The not-a-test line marks only a script that acts on GitHub, a release or a deployment and
# judges no code: a harness, or a script whose header calls it a test or a proof, fails on it,
# whatever its name.
echo "case: a *.test.sh harness that carries the not-a-test line"
root=$(fixture testsh-not-a-test)
printf '#!/usr/bin/env bash\n# legion: not a test - checks the flags file by hand.\n# Reads the flags file.\necho ok\n' \
  > "$root/scripts/flags.test.sh"
run_check "$root"
check "testsh-not-a-test: fails" "$(is "$status" 1)"
check "testsh-not-a-test: names the harness" \
  "$(contains "$out" '^check-test-reach: script: scripts/flags.test.sh:2: flags.test.sh: it is a harness (\*.test.sh), and it carries `legion: not a test - `')"

echo "case: a script a workflow runs to cancel runs, with the not-a-test line"
root=$(fixture cancel-script)
printf '#!/usr/bin/env bash\n# legion: not a test - cancels the merge-group runs a newer queue entry replaced.\n# The merge-group runs the queue has replaced, cancelled one by one.\necho cancel\n' \
  > "$root/.github/scripts/cancel-replaced.sh"
cat > "$root/.github/workflows/cancel.yaml" <<'YAML'
name: Cancel
on: pull_request
jobs:
  cancel:
    runs-on: ubuntu-24.04
    steps:
      - name: Cancel the replaced runs
        run: .github/scripts/cancel-replaced.sh
YAML
run_check "$root"
passes "cancel-script: passes"

echo "case: a script not named *.test.sh whose header calls it a proof, with the not-a-test line"
root=$(fixture proof-not-a-test)
printf '#!/usr/bin/env bash\n# legion: not a test - prints what the reviewer reads.\n# A proof that the store keeps every row it was handed.\necho proof\n' \
  > "$root/scripts/store-rows.sh"
run_check "$root"
check "proof-not-a-test: fails" "$(is "$status" 1)"
check "proof-not-a-test: names the script" \
  "$(contains "$out" '^check-test-reach: script: scripts/store-rows.sh:2: store-rows.sh: its header calls it a test, a proof or a smoke test, and it carries `legion: not a test - `')"

# The list of tests INFRA-3411's two lists give a home the tree does not give yet
# (.github/test-reach-unhomed.yaml): a finding it names passes, the same kind of finding it does
# not name fails, and an entry no finding answers fails as stale.
# unhomed <root> <finding…>: the list, one entry per finding, each with a row.
unhomed() {
  local root=$1
  shift
  printf 'unhomed:\n' > "$root/.github/test-reach-unhomed.yaml"
  for finding in "$@"; do
    printf '  - finding: "%s"\n    row: "test-value-md 9.9, a row of the fixture"\n' "$finding" \
      >> "$root/.github/test-reach-unhomed.yaml"
  done
}

echo "case: a tagged test the list of unhomed tests names"
root=$(fixture listed)
go_test_file "$root/packages/envoy/unit/nightly_test.go" unit nightly TestNightly
unhomed "$root" "tag: packages/envoy/unit/nightly_test.go: nightly"
run_check "$root"
passes "listed: the listed finding passes"
check "listed: the pass line counts the listed test" "$(contains "$out" 'but the 1 the list of unhomed tests names')"

echo "case: a tagged test of the same kind the list does not name"
root=$(fixture listed-other)
go_test_file "$root/packages/envoy/unit/nightly_test.go" unit nightly TestNightly
go_test_file "$root/packages/envoy/unit/weekly_test.go" unit weekly TestWeekly
unhomed "$root" "tag: packages/envoy/unit/nightly_test.go: nightly"
run_check "$root"
check "listed-other: fails" "$(is "$status" 1)"
check "listed-other: names the test the list does not name" \
  "$(contains "$out" '^check-test-reach: tag: packages/envoy/unit/weekly_test.go:1: weekly: ')"
check "listed-other: and not the one it names" "$(is "$(contains "$out" 'nightly_test.go')" false)"

echo "case: a listed test that a job ci-ok needs now runs"
root=$(fixture listed-stale)
go_test_file "$root/packages/envoy/unit/nightly_test.go" unit nightly TestNightly
unhomed "$root" "tag: packages/envoy/unit/nightly_test.go: nightly"
required_job "$root" nightly '    runs-on: ubuntu-24.04
    steps:
      - name: The nightly tests, now in a job ci-ok needs
        run: go test -count=1 -tags nightly ./unit/
        working-directory: packages/envoy'
run_check "$root"
check "listed-stale: fails" "$(is "$status" 1)"
check "listed-stale: names the stale entry" \
  "$(contains "$out" '^check-test-reach: listed: .github/test-reach-unhomed.yaml:2: tag: packages/envoy/unit/nightly_test.go: nightly: the list names it as a test with no home yet (test-value-md 9.9, a row of the fixture), and the search no longer finds it: a job ci-ok needs runs it now, or it is gone')"

echo "case: a list entry of no kind the search finds"
root=$(fixture listed-unreadable)
unhomed "$root" "flaky: packages/envoy/unit/unit_test.go: TestUnit"
run_check "$root"
check "listed-unreadable: exits 2" "$(is "$status" 2)"
check "listed-unreadable: names the entry" \
  "$(contains "$out" 'cannot read an input: .github/test-reach-unhomed.yaml:2: `flaky: packages/envoy/unit/unit_test.go: TestUnit` is no')"

# dispatch://INFRA-5794's check a-search: a whole-daemon driver added under `//go:build e2e`
# beside the daemon's untagged tests, which the daemon's go test step runs, fails by its file.
echo "case: a //go:build e2e test file added to packages/daemon/internal/daemon, not listed"
root=$(fixture daemon-e2e)
mkdir -p "$root/packages/daemon"
printf 'module example.com/daemon\n\ngo 1.27\n' > "$root/packages/daemon/go.mod"
go_test_file "$root/packages/daemon/internal/daemon/daemon_test.go" daemon "" TestTheDaemonBoots
go_test_file "$root/packages/daemon/internal/daemon/new_driver_live_test.go" daemon e2e TestANewDriverLive
required_job "$root" daemon-go '    runs-on: ubuntu-24.04
    steps:
      - name: Test the daemon
        run: go test -count=1 ./...
        working-directory: packages/daemon'
run_check "$root"
check "daemon-e2e: fails" "$(is "$status" 1)"
check "daemon-e2e: names the file and its tag" \
  "$(contains "$out" '^check-test-reach: tag: packages/daemon/internal/daemon/new_driver_live_test.go:1: e2e: no required step.s go test builds packages/daemon/internal/daemon')"
check "daemon-e2e: and not the untagged test the step runs" "$(is "$(contains "$out" 'TestTheDaemonBoots')" false)"

# The computed filters' declarations: each file of .github/ci-gates is run by the module that
# calls load_gate_files, which a required step reaches, and by nothing else.
echo "case: a gate file of .github/ci-gates, run by the loader a required step runs"
root=$(fixture gate-files)
mkdir -p "$root/.github/ci-gates"
printf '# The check_flags_test filter'"'"'s gate: what job test reads.\nGATE = harness("check_flags_test")\n' \
  > "$root/.github/ci-gates/test.check_flags_test.py"
printf '"""The gates, from each file of .github/ci-gates."""\nfrom gate_loader import load_gate_files\n\nGATES = load_gate_files(globals())\n' \
  > "$root/.github/scripts/gate_data.py"
printf 'def load_gate_files(namespace):\n    return ()\n' > "$root/.github/scripts/gate_loader.py"
required_job "$root" filters '    runs-on: ubuntu-24.04
    steps:
      - name: Read the gates
        run: python3 .github/scripts/gate_data.py'
run_check "$root"
passes "gate-files: the gate file is run by the loader the required step runs"
sed -i 's|^GATES = load_gate_files(globals())$|GATES = ()|' "$root/.github/scripts/gate_data.py"
run_check "$root"
check "gate-files: with no load_gate_files call, the gate file fails" "$(is "$status" 1)"
check "gate-files: and is named" \
  "$(contains "$out" '^check-test-reach: script: .github/ci-gates/test.check_flags_test.py:1: test.check_flags_test.py: ')"

# A Bun gate whose condition cannot hold on GitHub Actions, `skipIf(<missing> && !onActions)` with
# the condition in a declaration of its own, runs its test there and fails without what it needs.
echo "case: a Bun gate that never skips on GitHub Actions"
root=$(fixture bun-on-actions)
cat > "$root/packages/web/binary.test.ts" <<'TS'
import { expect, test } from "bun:test";

const binary = process.env.FIXTURE_BINARY;
const onActions = process.env.GITHUB_ACTIONS === "true";
const skipped = binary === undefined && !onActions;

test.skipIf(skipped)("the row that needs the binary", () => {
  if (binary === undefined) throw new Error("FIXTURE_BINARY is unset on GitHub Actions");
  expect(binary).toBeTruthy();
});
TS
row "$root" FIXTURE_BINARY 'class: prerequisite' 'why: The binary the row runs, which skips off GitHub Actions and fails on it.'
row "$root" GITHUB_ACTIONS 'class: prerequisite' 'why: The row skips off GitHub Actions alone.'
run_check "$root"
passes "bun-on-actions: the gate never skips on GitHub Actions"
sed -i 's|^const skipped = binary === undefined \&\& !onActions;$|const skipped = binary === undefined;|' \
  "$root/packages/web/binary.test.ts"
run_check "$root"
check "bun-on-actions: without the Actions term the gate fails" "$(is "$status" 1)"
check "bun-on-actions: and names the file and its variable" \
  "$(contains "$out" '^check-test-reach: variable: packages/web/binary.test.ts:[0-9]*: FIXTURE_BINARY: its row.s class is prerequisite, and this gate neither calls')"

# A test that points a variable at a value of its own (t.Setenv, in a fixture helper of its
# package) reads its own value back: no job must give it.
echo "case: a Go test that reads a variable its own helper set"
root=$(fixture own-setenv)
cat > "$root/packages/envoy/unit/own_file_test.go" <<'GO'
package unit

import (
	"os"
	"strings"
	"testing"
)

const fileVar = "FIXTURE_CREDENTIALS_FILE"

func pointAtAFile(t *testing.T) {
	t.Setenv(fileVar, t.TempDir()+"/credentials.json")
}

func describe(path string) string { return "the file " + fileVar + " names is unread" }

func TestTheFileIsNamedInTheError(t *testing.T) {
	pointAtAFile(t)
	path := os.Getenv(fileVar)
	got := describe(path)
	if got == "" || strings.Contains(got, path) {
		t.Fatalf("describe(%q) = %q, want the variable named and not the path", path, got)
	}
}
GO
run_check "$root"
passes "own-setenv: the read of the test's own value is no gate"
sed -i 's|^\tt.Setenv(fileVar, t.TempDir()+"/credentials.json")$|\t_ = t|' "$root/packages/envoy/unit/own_file_test.go"
run_check "$root"
check "own-setenv: with no Setenv the read is a gate, and fails" "$(is "$status" 1)"
check "own-setenv: and names the test and its variable" \
  "$(contains "$out" '^check-test-reach: variable: packages/envoy/unit/own_file_test.go:[0-9]*: FIXTURE_CREDENTIALS_FILE: TestTheFileIsNamedInTheError gates on it')"

# A go test that runs none of its tests (`-run '^$'`, a cache fill linking each test binary) is no
# test command in a workflow no merge waits for.
echo "case: a go test that runs no test, in a workflow no merge waits for"
root=$(fixture runs-none)
cat > "$root/.github/workflows/caches.yaml" <<'YAML'
name: Caches
on: push
jobs:
  fill:
    runs-on: ubuntu-24.04
    steps:
      - name: Start every test binary, running no test
        run: go test -count=1 -run '^$' ./...
        working-directory: packages/envoy
YAML
run_check "$root"
passes "runs-none: the step that runs no test is not read"
sed -i "s|go test -count=1 -run '^\\\$' ./...|go test -count=1 -run '^TestUnit\$' ./...|" "$root/.github/workflows/caches.yaml"
run_check "$root"
check "runs-none: the same step with a -run that selects a test fails" "$(is "$status" 1)"
check "runs-none: and names the workflow and the step" \
  "$(contains "$out" '^check-test-reach: outside: .github/workflows/caches.yaml:[0-9]*: Start every test binary, running no test: ')"

summary "check-test-reach.sh"
