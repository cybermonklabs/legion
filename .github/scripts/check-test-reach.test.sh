#!/usr/bin/env bash
# Tests for `check-test-reach.sh`: a fixture repository that passes, and one fixture for each kind
# of finding - a `_test.go` file with a tag no job passes, a test one `-skip` leaves out, a Go test
# and a Bun test that skip on a variable no job sets, an assertion that runs only when such a
# variable is set, a script with no tool line that no job runs, and a scheduled step - each of
# which must fail and name its file; and the registry's classes, each held to a gate.
#
# Each case builds a miniature repository under a temporary directory and symlinks the real
# script, the step reader it imports (go_test_steps.py) and the modules that one imports into it,
# so the code under test is the code CI runs: the script resolves its root from its own path
# (`cd "$(dirname "$0")/../.."`), which the symlink puts at the fixture root. A fixture is no git
# checkout, so the search walks it.
#
# No case runs the search over this repository's own tree: ci.yaml's lint job does
# (.github/workflows/ci-job-lint.yaml, "Check every test reaches a job ci-ok needs").
#
# Run from anywhere: .github/scripts/check-test-reach.test.sh
# CI runs it in ci.yaml's test job.
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
  for linked in check-test-reach.sh go_test_steps.py ci_filter_coverage.py workflow_paths.py go_build_tags.py; do
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
  echo "$root"
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

summary "check-test-reach.sh"
