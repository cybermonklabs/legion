#!/usr/bin/env bash
# Tests for scripts/prompt-word-tests.sh, the search for committed tests that read, search or pin
# the words of a text an agent is given.
#
# The planted cases run the search over a worktree of this repository's own HEAD, with test files
# planted in it and added to its index, so the search reads the whole tree it reads in use. Each
# planted file is a test it must list, or a copy of a test of structured values it must not list:
#   - a Go test of the prompts package that pins words of the composed tester prompt;
#   - a Go test behind the e2e build tag that reads a skill;
#   - a Bun test that reads a skill, and one that pins words of a tool's description;
#   - a Go test that pins words of a daemon task text;
#   - a shell harness under scripts/e2e that greps a role prompt;
#   - a Bun test outside the packages that write agents' texts, which reads a role prompt by path;
#   - a copy of a test of structured values, which reads no text an agent is given.
# Where a case names a family, the row must carry it, so each family has a case only it serves.
# The other cases build a miniature repository: a tree whose only test is that copy finds nothing,
# and a tree it cannot read, or one that holds no test file, is UNKNOWN, never clean.
#
# Run from anywhere: scripts/prompt-word-tests.test.sh
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(git -C "$script_dir" rev-parse --show-toplevel)
search="$script_dir/prompt-word-tests.sh"
work=$(mktemp -d)
tree="$work/tree"
cleanup() {
  if [ -e "$tree/.git" ]; then
    git -C "$repo_root" worktree remove --force "$tree" > /dev/null 2>&1 || true
  fi
  rm -rf "$work"
}
trap cleanup EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$repo_root/.github/scripts/test-lib.sh"

command -v git > /dev/null || { echo "  FAIL: git is required" >&2; exit 1; }

# The test of structured values the clean cases copy: it asserts the phases and statuses the
# engine computes and writes, and reads no text an agent is given.
values_source=packages/daemon/internal/workflow/phase_status_test.go

# run_search <arguments...>: sets `out` to the combined output and `status` to the exit code.
run_search() {
  set +e
  out=$(bash "$search" "$@" 2>&1)
  status=$?
  set -e
}

# listed <out dir> <file>: true when rows.tsv names the file. awk reads the whole file, so no
# pipe ends early (test-lib.sh's contains says why that matters under pipefail).
listed() {
  if [ -f "$1/rows.tsv" ] && awk -F'\t' -v f="$2" '$1 == f { found = 1 } END { exit !found }' "$1/rows.tsv"; then
    echo true
  else
    echo false
  fi
}

# row_at <out dir> <file> <text> [family]: true when rows.tsv holds a row of the file at the first
# line that holds the text, and, given a family, when that row's families name it.
row_at() {
  local line
  line=$(awk -v t="$3" 'index($0, t) { print FNR; exit }' "$tree/$2")
  if [ -n "$line" ] && [ -f "$1/rows.tsv" ] &&
    awk -F'\t' -v f="$2" -v l="$line" -v family="${4:-}" \
      '$1 == f && $2 == l && (family == "" || index("," $3 ",", "," family ",") > 0) { found = 1 } END { exit !found }' "$1/rows.tsv"; then
    echo true
  else
    echo false
  fi
}

# plant <path> <text>: writes the file into the worktree and adds it to its index.
plant() {
  mkdir -p "$(dirname "$tree/$1")"
  printf '%s\n' "$2" > "$tree/$1"
  git -C "$tree" add -- "$1"
}

git -C "$repo_root" worktree add --detach --quiet "$tree" HEAD

word_test=packages/daemon/internal/prompts/zz_planted_word_test.go
plant "$word_test" 'package prompts

import (
	"strings"
	"testing"
)

func TestZZPlantedTesterPromptWords(t *testing.T) {
	composer, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	parts, err := composer.Compose("tester", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(parts, "\n\n"), "planted words of the tester prompt") {
		t.Fatal("the composed tester prompt lost its planted words")
	}
}'

e2e_test=packages/daemon/internal/daemon/zz_planted_skill_e2e_test.go
plant "$e2e_test" '//go:build e2e

package daemon

import (
	"os"
	"strings"
	"testing"
)

func TestZZPlantedSkillWordsBehindATag(t *testing.T) {
	text, err := os.ReadFile("../../../../skills/legion-worker/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(text), "planted words of the worker skill") {
		t.Fatal("the worker skill lost its planted words")
	}
}'

bun_test=packages/pi-legion/src/zz-planted-skill.test.ts
plant "$bun_test" 'import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import path from "node:path";

test("the planted skill keeps its words", () => {
  const text = readFileSync(path.join(import.meta.dir, "../../../skills/legion-architect/SKILL.md"), "utf8");
  expect(text).toContain("planted words of the architect skill");
});'

tool_test=packages/contracts/src/zz-planted-tool.test.ts
plant "$tool_test" 'import { expect, test } from "bun:test";
import { dispatchToolSpecs } from "./dispatch-tools";

test("the planted tool keeps its description", () => {
  const spec = dispatchToolSpecs[0];
  expect(spec?.description).toContain("planted words of a tool description");
});'

task_test=packages/daemon/internal/workflow/zz_planted_task_test.go
plant "$task_test" 'package workflow

import (
	"strings"
	"testing"
)

func TestZZPlantedWakeTaskWords(t *testing.T) {
	task := WakeTask("checks", "", WakeStop{})
	if !strings.Contains(task, "planted words of a wake task") {
		t.Fatalf("the wake task lost its planted words: %q", task)
	}
}'

harness=scripts/e2e/zz-planted-prompt.sh
plant "$harness" '#!/usr/bin/env bash
set -euo pipefail
grep -qF "planted words of the reviewer prompt" packages/daemon/internal/prompts/go/reviewer.md'

# A Bun test outside the packages whose code writes agents' texts, which reads a role prompt by its
# path and asserts on another line: the path alone names what it reads.
path_test=packages/dispatch/web/src/__tests__/zz-planted-prompt.test.ts
plant "$path_test" 'import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";

test("the planted prompt is read", () => {
  const text = readFileSync("../../../../daemon/internal/prompts/go/tester.md", "utf8");
  expect(text.length).toBeGreaterThan(0);
});'

values_test=packages/daemon/internal/workflow/zz_planted_values_test.go
mkdir -p "$(dirname "$tree/$values_test")"
cp "$repo_root/$values_source" "$tree/$values_test"
git -C "$tree" add -- "$values_test"

echo "case: the search over this repository's tree, with the planted tests"
run_search --repo "$tree" --out "$work/all"
check "finds rows, so exits 1" "$(is "$status" 1)"
check "counts the files it searched" "$(contains "$out" '^prompt-word-tests: [0-9][0-9]* row(s) in [0-9][0-9]* file(s), of [1-9][0-9]* test file(s) searched')"
check "lists the Go test that pins words of the composed tester prompt" "$(listed "$work/all" "$word_test")"
check "names the line of its words" "$(row_at "$work/all" "$word_test" 'planted words of the tester prompt' asserted-words)"
check "names the test of the prompts package" "$(row_at "$work/all" "$word_test" 'func TestZZPlantedTesterPromptWords' prompt-package)"
check "names the line that composes the prompt" "$(row_at "$work/all" "$word_test" 'composer.Compose(' composed-prompt)"
check "prints the row as file:line" "$(contains "$out" "^$word_test:[0-9][0-9]*: ")"
check "lists the e2e-tagged Go test that reads a skill" "$(row_at "$work/all" "$e2e_test" 'skills/legion-worker/SKILL.md' skill-file)"
check "lists the Bun test that reads a skill" "$(row_at "$work/all" "$bun_test" 'skills/legion-architect/SKILL.md' skill-file)"
check "lists the Bun test that pins a tool description" "$(row_at "$work/all" "$tool_test" 'planted words of a tool description' asserted-words)"
check "names the tool specs it reads" "$(row_at "$work/all" "$tool_test" 'dispatchToolSpecs[0]' tool-description)"
check "lists the Go test that pins words of a daemon task text" "$(row_at "$work/all" "$task_test" 'planted words of a wake task' asserted-words)"
check "names the task text it reads" "$(row_at "$work/all" "$task_test" 'WakeTask(' task-text)"
check "lists the harness that greps a role prompt" "$(row_at "$work/all" "$harness" 'go/reviewer.md')"
check "lists a test that reads a role prompt by its path" "$(row_at "$work/all" "$path_test" 'prompts/go/tester.md' prompt-file)"
check "names the test each row is in" \
  "$(if awk -F'\t' -v f="$word_test" '$1 == f && $4 == "TestZZPlantedTesterPromptWords" { found = 1 } END { exit !found }' "$work/all/rows.tsv"; then echo true; else echo false; fi)"
check "stays quiet on the copy of a test of structured values" "$(is "$(listed "$work/all" "$values_test")" false)"
check "stays quiet on the test it copies" "$(is "$(listed "$work/all" "$values_source")" false)"
check "lists no row of its own files" "$(is "$(listed "$work/all" scripts/prompt-word-tests.test.sh)" false)"

echo "case: an excluded file's rows are left out, and listed as excluded"
printf '# owned by another change\n%s\n\n' "$word_test" > "$work/exclude"
run_search --repo "$tree" --exclude "$work/exclude" --out "$work/excluded"
check "still finds the other rows, so exits 1" "$(is "$status" 1)"
check "leaves the excluded file out" "$(is "$(listed "$work/excluded" "$word_test")" false)"
check "keeps the files it does not exclude" "$(listed "$work/excluded" "$e2e_test")"
check "names the excluded file" "$(if grep -qxF -- "$word_test" "$work/excluded/excluded.txt"; then echo true; else echo false; fi)"

echo "case: an exclusion that names no tracked file is said, not passed over"
printf '%s\n' packages/daemon/internal/prompts/no_such_test.go > "$work/exclude-missing"
run_search --repo "$tree" --exclude "$work/exclude-missing" --out "$work/excluded-missing"
check "says the exclusion names no tracked file" "$(contains "$out" 'names no tracked file: packages/daemon/internal/prompts/no_such_test.go')"

echo "case: a tree whose only test is a test of structured values"
clean="$work/clean"
mkdir -p "$clean/packages/daemon/internal/workflow"
cp "$repo_root/$values_source" "$clean/packages/daemon/internal/workflow/values_test.go"
git -C "$clean" init --quiet
git -C "$clean" add .
run_search --repo "$clean" --out "$work/clean-out"
check "finds nothing, so exits 0" "$(is "$status" 0)"
check "says it searched one test file" "$(contains "$out" '^prompt-word-tests: 0 row(s) in 0 file(s), of 1 test file(s) searched')"

echo "case: a tree it cannot read is UNKNOWN"
mkdir -p "$work/not-a-repo"
run_search --repo "$work/not-a-repo" --out "$work/unreadable-out"
check "exits 3" "$(is "$status" 3)"
check "says UNKNOWN and what it could not read" "$(contains "$out" "^prompt-word-tests: UNKNOWN: .*$work/not-a-repo")"

echo "case: a tree that holds no test file is UNKNOWN, never clean"
empty="$work/empty"
mkdir -p "$empty"
printf 'notes\n' > "$empty/README.md"
git -C "$empty" init --quiet
git -C "$empty" add .
run_search --repo "$empty" --out "$work/empty-out"
check "exits 3" "$(is "$status" 3)"
check "says it found no test file to search" "$(contains "$out" '^prompt-word-tests: UNKNOWN: .*no test file')"

echo "case: a call without --out is a usage error"
run_search --repo "$tree"
check "exits 2" "$(is "$status" 2)"

summary "prompt-word-tests.sh"
