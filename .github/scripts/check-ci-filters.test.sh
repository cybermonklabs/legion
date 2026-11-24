#!/usr/bin/env bash
# Tests for `check-ci-filters.sh`: every ci.yaml filter is computed from what its job reads - the
# Go closures recomputed with go list (with -race for a -race command), the files of
# .github/ci-gates and the //ci: lines of the tests - and written by the generate mode, which
# falls open to selecting every job when it cannot compute them; the table of go commands
# must say what ci.yaml and the workflows its jobs call say, a claimed go command the check does
# not reduce (another form, a
# step that is more than the go line, a shell, a container, a BASH_ENV) counts as reading every
# Go file of the repository and every file under a Go module's directory, a step off its exact
# entry's pinned script or directory is refused naming the first line that differs, or both
# directories, and on a -run or -skip list each test one side names and the other lacks, a step
# an exact entry pins under an env, shell, container or job that could move it is refused naming
# what does, a why that cites a step must name a workflow file that still holds it,
# CI_OK_SELECTED_BY must map every job changes gates, it and CI_OK_PULL_REQUEST_ONLY must say what
# each job's if: tests, every job of ci.yaml must be in exactly one of ci-ok's three classes
# (CI_OK_SELECTED_BY, CI_OK_ALWAYS_RUN, CI_OK_EXEMPT) and a CI_OK_ALWAYS_RUN job must be gated by
# no changes output, every job but changes and those CI_OK_EXEMPT names must end with the ci-ok
# report step, the ci-ok job must need every job but itself and the exempt ones, each check
# main's ruleset requires must be the ci.yaml job of that name, carrying the one name that sets a
# dispatch of any ref but exactly refs/heads/main apart, no other job may carry one, the ci-ok
# job's reconcile step must carry its one if:, the changes job's ref step must test the ref with
# the shell's case-sensitive `=`, and a PUSH_WORKFLOWS workflow's
# on.push.paths must equal its filter, no `if:` of ci.yaml may test for a push event or compare
# github.ref with a branch ref, every job's if: but those two jobs' must hold the term that skips
# it on a re-run attempt of a merge-group run and theirs must not, ci-caches.yaml's save of a
# cache family must carry
# the key and the paths every restore of that family names, ci.yaml's and any other workflow's,
# or that key followed by the commit, its own restores being lookups but those of a family it
# saves under the commit, which download the newest entry,
# ci-caches.yaml's race build must compile exactly the packages ci_job_reads.py's
# DAEMON_RACE_PACKAGES names,
# ci_job_reads.py's WORKSPACES must name exactly the workspaces of the root package.json, and no
# workflow but ci.yaml may run on a merge group, the job TAG_RUNS pairs with a tag step that
# passes -run must have a filter covering every file the tag step reads, every dorny/paths-filter
# step of every workflow must hand the action exactly the generated file, after the step that
# writes it, every output of the changes job must name that step and a filter the gates declare,
# and a hand-written read that matches no file is refused. A script's go command is read under
# every step of each job that may run it, a workflow several jobs of ci.yaml call runs each job of it
# named for a caller under that caller alone, and the header's run recipe runs the check, or says why
# it stops.
#
# Each case builds a miniature repository under a temporary directory - a go.work binding one
# module, a ci.yaml whose `changes` job gates one job, the gate of that job and two excluded
# filters in .github/ci-gates, a //ci:reads line in the one test that reads outside its package, a
# root package.json with one workspace, and a ci_job_reads.py that loads them - and symlinks the
# real script, its engine (ci_filter_coverage.py), the loader of the gates and the //ci: lines
# (ci_filter_sources.py), the rules of ci.yaml it runs (ci_workflow_rules.py), the path model they
# import (workflow_paths.py) and the generation's fail-open wrapper (ci-filters-generate.sh) into it,
# so the code under test is the code CI runs. It needs go and a python3 with PyYAML.
#
# Run from anywhere: .github/scripts/check-ci-filters.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# Importing ci_job_reads.py walks the repository and runs go list. The cases at the end of this
# file that read its race walk and its files_but_tests ask it fourteen questions, and as fourteen
# fresh python3 processes, each importing it, they took 66 s of this harness's 115 s in CI's job
# 137302423430: 28.8 s for the first import and about 2.8 s for each later one. So one python3,
# started here, imports it once, from the repository's root as the check runs, while the cases
# before those run, and answers each question in a child it forks: the child starts from the
# module as the import left it, as a fresh python3 started from there would, and whatever the
# question changes (the race lane, the working directory, REPO, a cache) ends with the child.
coproc READS {
  cd "$script_dir/../.." && PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=.github/scripts exec python3 -c '
import os
import sys
import traceback
from pathlib import Path

import ci_filter_coverage
import ci_job_reads as reads


def derived(root, lane):
    reads.ENVOY_RACE_LANE = (reads.RaceJob("a-race-job", "Race-test the packages", "30m", tuple(
        reads.RacePackage(p.removesuffix("/..."), tree=p.endswith("/...")) for p in lane.split())),)
    try:
        return reads.race_impossible_script(reads.race_impossible_tests(root))
    except ValueError as refusal:
        return f"RAISED: {refusal}"


def from_root(root):
    os.chdir(root)
    ci_filter_coverage.REPO = Path.cwd()
    reads.repository_files.cache_clear()


def listed(root, suffix, shape):
    from_root(root)
    return " ".join(reads.files_but_tests("pkg", suffix=suffix, tree=shape == "tree", skip=("testdata",)))


def refused(root, spelling):
    from_root(root)
    try:
        reads.files_but_tests(spelling)
    except ValueError as exc:
        return str(exc)
    return ""


questions = {"derived": derived, "listed": listed, "refused": refused}


def field():
    """One NUL-ended field of the question; None at the end of the input."""
    data = bytearray()
    while (byte := sys.stdin.buffer.read(1)) != b"\0":
        if not byte:
            return None
        data += byte
    return data.decode()


while (count := field()) is not None:
    question, *args = [field() for _ in range(int(count))]
    child = os.fork()
    if child == 0:
        try:
            answer = questions[question](*args)
        except BaseException:
            traceback.print_exc()
            os._exit(1)
        sys.stdout.write(f"{answer}\0")
        sys.stdout.flush()
        os._exit(0)
    # A question that raised, or a child that died before it answered, ends this python3 too: its
    # stdout closes, and ask fails rather than read an empty answer.
    if os.waitpid(child, 0)[1] != 0:
        sys.exit(1)
'
}

# ask <question> <arg>...: the answer of that python3 to one question, as a fresh python3 would
# print it. A question that raises ends that python3, its traceback on stderr, and ask then fails
# with no answer, as a fresh python3 that raised failed.
ask() {
  local answer
  printf '%s\0' "$#" "$@" >&"${READS[1]}"
  if ! IFS= read -r -d '' answer <&"${READS[0]}"; then
    echo "the python3 that answers for ci_job_reads.py ended without answering '$1'" >&2
    return 1
  fi
  printf '%s\n' "$answer"
}

# fixture <name>: a tree that passes. The app job runs `go test ./cmd/app ./lib`, whose closure is
# cmd/app and lib (cmd/app imports lib); lib's test reads fixtures/data.json through `..`, which
# its //ci:reads line declares; mod/other is a package nothing imports. The app job's gate is
# .github/ci-gates/app.py. The image job runs only on a pull request, and both end with the ci-ok
# report step; its filter is kept by hand (.github/ci-gates/image.py). The lint job is the
# fixture's always-run one:
# no output of changes gates it, CI_OK_ALWAYS_RUN names it beside `changes`, and ci-ok holds it
# to success. pr-title and ci-ok are the two jobs named for
# a check main's ruleset requires: CI_OK_EXEMPT holds both outside ci-ok's accounting, so neither
# carries the report step and neither may carry the term that skips a job on a re-run attempt of
# a merge-group run. Every job of the fixture is thus in exactly one of ci-ok's three classes.
# The site filter gates no job and is kept by hand too (.github/ci-gates/site.py): site.yaml,
# which PUSH_WORKFLOWS pairs with it, runs on a push whose paths are that filter. The root
# package.json has the one workspace packages/widget, which WORKSPACES names, since rule 13 holds
# those two lists to each other; no job here runs Bun, so that manifest is nothing the app gate
# reads. Echoes the fixture root.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/.github/workflows" "$root/.github/ci-gates" "$root/mod/cmd/app" \
    "$root/mod/lib" "$root/mod/other" "$root/fixtures" "$root/docs" "$root/packages/widget"
  for linked in check-ci-filters.sh ci_filter_coverage.py ci_filter_sources.py ci_workflow_rules.py workflow_paths.py \
    go_build_tags.py ci-filters-generate.sh ci_ts_listing.py; do
    ln -s "$script_dir/$linked" "$root/.github/scripts/$linked"
  done
  printf 'PROGRAMS = []\nBUNDLES = []\nLISTED = []\n' > "$root/.github/scripts/ci_ts_programs.py"
  printf 'go 1.21\n\nuse ./mod\n' > "$root/go.work"
  printf 'module example.com/m\n\ngo 1.21\n' > "$root/mod/go.mod"
  printf 'package main\n\nimport "example.com/m/lib"\n\nfunc main() { lib.Hello() }\n' > "$root/mod/cmd/app/main.go"
  printf 'package lib\n\nfunc Hello() {}\n' > "$root/mod/lib/lib.go"
  cat > "$root/mod/lib/lib_test.go" <<'GO'
package lib

//ci:reads fixtures/data.json TestData reads ../../fixtures/data.json

import (
	"os"
	"testing"
)

func TestData(t *testing.T) {
	if _, err := os.ReadFile("../../fixtures/data.json"); err != nil {
		t.Fatal(err)
	}
}
GO
  printf 'package other\n' > "$root/mod/other/other.go"
  echo '{}' > "$root/fixtures/data.json"
  echo 'unrelated' > "$root/docs/readme.md"
  printf '{\n  "name": "fixture",\n  "workspaces": ["packages/widget"]\n}\n' > "$root/package.json"
  printf '{\n  "name": "widget"\n}\n' > "$root/packages/widget/package.json"
  cat > "$root/.github/scripts/ci_job_reads.py" <<'PY'
from ci_filter_coverage import Read, TagRun, at, exact, gate, repo_files, run
from ci_filter_sources import excluded, load_directives, load_gate_files

PUSH_WORKFLOWS = {"site": ".github/workflows/site.yaml"}
WORKSPACES = ["packages/widget"]
DAEMON_RACE_PACKAGES = ["lib"]
DAEMON_RACE_FILL_JOB = "fill"
RACE_FILL_SCRIPTS = []
TAG_RUNS = []
READ_SETS = {}
GATES, EXCLUDED = load_gate_files(globals())
RULES, DIRECTIVES = load_directives(repo_files(), READ_SETS)
PY
  cat > "$root/.github/ci-gates/app.py" <<'PY'
GATE = gate("app", "app", runs=[
    run("mod", ["./cmd/app", "./lib"], at("app", "Test the app") + ": go test ./cmd/app ./lib", step="Test the app",
        text="go test ./cmd/app ./lib"),
], reads=[Read(".github/workflows/ci.yaml", "a job reads its own definition")])
PY
  cat > "$root/.github/ci-gates/image.py" <<'PY'
EXCLUDED = excluded("image", "an image build another check proves", ["docs/**"])
PY
  cat > "$root/.github/ci-gates/site.py" <<'PY'
EXCLUDED = excluded("site", "the site build site.yaml runs on a push", ["go.work", "mod/go.mod", "docs/**"])
PY
  cat > "$root/.github/workflows/ci.yaml" <<'YAML'
name: CI
on: [pull_request]
env:
  CI_OK_SELECTED_BY: '{"app": "app", "image": "image"}'
  CI_OK_PULL_REQUEST_ONLY: '["image"]'
  CI_OK_ALWAYS_RUN: '["changes", "lint"]'
  CI_OK_EXEMPT: '["ci-ok", "pr-title"]'
jobs:
  changes:
    if: (github.event_name != 'merge_group' || github.run_attempt == '1')
    runs-on: ubuntu-24.04
    outputs:
      app: ${{ steps.filter.outputs.app }}
      image: ${{ steps.filter.outputs.image }}
      site: ${{ steps.filter.outputs.site }}
      ref_is_main: ${{ steps.ref.outputs.is_main }}
    steps:
      - run: .github/scripts/ci-filters-generate.sh "$RUNNER_TEMP/ci-filters.yaml"
      - uses: dorny/paths-filter@v4
        id: filter
        with:
          predicate-quantifier: some-with-excludes
          filters: ${{ runner.temp }}/ci-filters.yaml
      - name: Read whether this run's ref is exactly refs/heads/main
        id: ref
        run: if [ "$GITHUB_REF" = refs/heads/main ]; then echo is_main=true; else echo is_main=false; fi >> "$GITHUB_OUTPUT"
  app:
    needs: changes
    if: needs.changes.outputs.app == 'true' && (github.event_name != 'merge_group' || github.run_attempt == '1')
    runs-on: ubuntu-24.04
    steps:
      - name: Test the app
        run: go test ./cmd/app ./lib
        working-directory: mod
      - name: Report ci-ok when this job decides a merge-group run
        if: always()
        uses: ./.github/actions/ci-ok-report
        with:
          changes: ${{ toJSON(needs.changes) }}
          job-status: ${{ job.status }}
  image:
    needs: changes
    if: github.event_name == 'pull_request' && needs.changes.outputs.image == 'true' && (github.event_name != 'merge_group' || github.run_attempt == '1')
    runs-on: ubuntu-24.04
    steps:
      - name: Build
        run: go build ./...
      - name: Report ci-ok when this job decides a merge-group run
        if: always()
        uses: ./.github/actions/ci-ok-report
        with:
          changes: ${{ toJSON(needs.changes) }}
          job-status: ${{ job.status }}
  lint:
    needs: changes
    if: (github.event_name != 'merge_group' || github.run_attempt == '1')
    runs-on: ubuntu-24.04
    steps:
      - name: Lint everything
        run: echo lint
      - name: Report ci-ok when this job decides a merge-group run
        if: always()
        uses: ./.github/actions/ci-ok-report
        with:
          changes: ${{ toJSON(needs.changes) }}
          job-status: ${{ job.status }}
  pr-title:
    name: ${{ github.event_name == 'workflow_dispatch' && 'pr-title (dispatch)' || 'pr-title' }}
    if: github.event_name == 'pull_request' || github.event_name == 'merge_group'
    runs-on: ubuntu-24.04
    steps:
      - name: Check the pull request's text
        run: echo text
  ci-ok:
    name: ${{ github.event_name == 'workflow_dispatch' && needs.changes.outputs.ref_is_main != 'true' && 'ci-ok (dispatch)' || 'ci-ok' }}
    needs: [changes, app, image, lint]
    if: ${{ always() }}
    runs-on: ubuntu-24.04
    steps:
      - name: Judge every job of this run
        run: echo verdict
      - name: Set every other ci-ok on this head to this job's conclusion
        if: ${{ always() && (github.event_name != 'workflow_dispatch' || needs.changes.outputs.ref_is_main == 'true') && !(steps.verdict.outcome == 'failure' && github.event_name == 'merge_group' && github.run_attempt == '1') }}
        run: echo reconcile
YAML
  cat > "$root/.github/workflows/site.yaml" <<'YAML'
name: Site
on:
  push:
    branches: [main]
    paths:
      - 'go.work'
      - 'mod/go.mod'
      - 'docs/**'
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - run: echo build
YAML
  git -C "$root" init -q
  echo "$root"
}

# run_check <root> [mode]: the check in the fixture, in its ordinary mode unless a mode is given.
run_check() {
  local root=$1
  shift
  set +e
  out=$("$root/.github/scripts/check-ci-filters.sh" "$@" 2>&1)
  status=$?
  set -e
}

# replace <file> <old> <new>: one literal replacement, failing the case loudly if <old> is absent.
replace() {
  local file=$1 old=$2 new=$3 text
  text=$(<"$file")
  if [[ $text != *"$old"* ]]; then
    echo "  FAIL: $file has no '$old' to replace" >&2
    return 1
  fi
  printf '%s\n' "${text/"$old"/"$new"}" > "$file"
}

# run_generate <root> [env...]: the generate mode through the changes job's wrapper, into
# <root>.generated.yaml beside the fixture (inside it, the file would be one of the repository's
# files the next run reads), as the changes job runs it; `generated` holds what it wrote.
run_generate() {
  local root=$1
  shift
  rm -f "$root.generated.yaml"
  set +e
  out=$(env "$@" "$root/.github/scripts/ci-filters-generate.sh" "$root.generated.yaml" 2>&1)
  status=$?
  set -e
  generated=$(cat "$root.generated.yaml" 2>/dev/null || true)
}

# filter_of <name>: the patterns of one filter of `generated`, one per line, unquoted.
filter_of() {
  awk -v name="$1:" '$0 == name { on = 1; next } /^[a-z_]+:$/ { on = 0 } on && /^  - / { sub(/^  - /, ""); gsub(/'"'"'/, ""); print }' <<<"$generated"
}

check_report_section() {
  local token tokens outside line stopped=false
  tokens=$(grep -E '^::stop-commands::[0-9a-f]{32}$' <<<"$out" || true)
  check "starts import and report sections with two tokens" \
    "$(is "$(grep -Ecx '^::stop-commands::[0-9a-f]{32}$' <<<"$out" || true)" 2)"
  while IFS= read -r token; do
    token=${token#::stop-commands::}
    check "ends a stopped command section with its token" \
      "$(is "$(grep -Fxc -- "::$token::" <<<"$out" || true)" 1)"
  done <<<"$tokens"
  outside=
  while IFS= read -r line; do
    if [[ $line =~ ^::stop-commands::([0-9a-f]{32})$ ]]; then
      token=${BASH_REMATCH[1]}
      stopped=true
      continue
    fi
    if [[ $stopped == true ]] && [[ $line == "::$token::" ]]; then
      stopped=false
      continue
    fi
    if [[ $stopped == false ]] && { [[ $line == report:* ]] || [[ $line =~ (::[a-z-]+|##\[) ]]; }; then
      outside+="$line"$'\n'
    fi
  done <<<"$out"
  check "keeps report lines and command syntax inside stopped sections" "$(is "$outside" "")"
}

unescaped_workflow_commands() {
  local line unsafe='' token stopped=false
  while IFS= read -r line; do
    if [[ $line =~ ^::stop-commands::([0-9a-f]{32})$ ]]; then
      token=${BASH_REMATCH[1]}
      stopped=true
      continue
    fi
    if [[ $stopped == true ]] && [[ $line == "::$token::" ]]; then
      stopped=false
      continue
    fi
    if [[ $stopped == false ]] &&
      { [[ $line == *'##['* ]] ||
        { [[ ! $line =~ ^::(error|warning)\ file=.*:: ]] && [[ $line =~ ::[a-z-]+ ]]; }; }; then
      unsafe+="$line"$'\n'
    fi
  done <<<"$out"
  printf '%s' "$unsafe"
}

check_input_parse_error() {
  local file=$1
  check "fails" "$(is "$status" 1)"
  check "emits one escaped input-error annotation" \
    "$(is "$(grep -Fc -- "::error file=$file::cannot read repository input: " <<<"$out" || true)" 1)"
  check "does not print a traceback for repository input" "$(is "$(contains "$out" 'Traceback')" false)"
  check "keeps command syntax outside the input-error annotation" "$(is "$(unescaped_workflow_commands)" "")"
}

echo "case: the filter is computed from the job's closure, its test's read and its module files"
root=$(fixture green)
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts the job's files" "$(contains "$out" '^report: app: 8 files (filter app)$')"
check "holds the push workflow's paths to its filter" \
  "$(contains "$out" '^report: .github/workflows/site.yaml: on.push.paths equals filter site (3 patterns)$')"
check "says what it checked" "$(contains "$out" 'every ci.yaml filter is computed from what its job reads (1 gates)')"
check_report_section
run_generate "$root"
check "the generate mode exits 0" "$(is "$status" 0)"
check "writes the job's filter in the three forms, the read test file kept" \
  "$(is "$(filter_of app | tr '\n' ' ')" 'go.work .github/ci-gates/app.py .github/workflows/ci.yaml fixtures/** mod/* mod/cmd/** mod/lib/** ')"
check "and an excluded filter as its file writes it" "$(is "$(filter_of site | tr '\n' ' ')" 'go.work mod/go.mod docs/** ')"
check "says it wrote all three" "$(contains "$out" '^wrote 3 filters to ')"

# A file the job does not compile is outside every directory form unless a `!` pattern keeps it
# out: a test file of the package the job only builds.
echo "case: a test file of a package the job builds and does not test is kept out by a ! pattern"
root=$(fixture build-only)
replace "$root/.github/ci-gates/app.py" 'text="go test ./cmd/app ./lib"' 'text="go build ./cmd/app ./lib", mode="build"'
replace "$root/.github/workflows/ci.yaml" "run: go test ./cmd/app ./lib" "run: go build ./cmd/app ./lib"
run_generate "$root"
check "the generate mode exits 0" "$(is "$status" 0)"
check "selects the library's directory, its test file excluded, and drops the test's read" \
  "$(is "$(filter_of app | tr '\n' ' ')" 'go.work .github/ci-gates/app.py .github/workflows/ci.yaml mod/* mod/cmd/** mod/lib/** !**/*_test.go ')"

echo "case: an annotation escapes an unsafe path in a message"
root=$(fixture annotation-escape)
cat >> "$root/.github/ci-gates/app.py" <<'PY'
GATE = with_reads(GATE, [Read("unsafe%\n:,.txt", "the unsafe path")])
PY
printf 'from dataclasses import replace\n\ndef with_reads(g, extra):\n    return replace(g, reads=tuple(g.reads) + tuple(extra))\n' \
  > "$root/.github/scripts/fixture_helpers.py"
replace "$root/.github/scripts/ci_job_reads.py" "READ_SETS = {}" $'READ_SETS = {}\nfrom fixture_helpers import with_reads'
run_check "$root"
check "fails" "$(is "$status" 1)"
expected="::error file=.github/ci-gates/app.py::gate app reads unsafe%25%0A:,.txt, which matches no file in the repository, so it holds its filter to nothing; correct the path or drop the read"
check "emits exactly one annotation with escaped message data" \
  "$(is "$(grep -Fxc -- "$expected" <<<"$out" || true)" 1)"

echo "case: an annotation escapes :: and CR in the file it names"
root=$(fixture annotation-property-escape)
cp "$root/.github/ci-gates/app.py" "$root/.github/ci-gates/"$'un::\rsafe.py'
rm "$root/.github/ci-gates/app.py"
replace "$root/.github/ci-gates/"$'un::\rsafe.py' 'GATE = gate("app", "app", runs=[' 'GATE = gate("app", "gone", runs=['
run_check "$root"
check "fails" "$(is "$status" 1)"
expected='::error file=.github/ci-gates/un%3A%3A%0Dsafe.py::gate app names job gone, which .github/workflows/ci.yaml does not have'
check "emits one annotation with a fully escaped file property" \
  "$(is "$(grep -Fxc -- "$expected" <<<"$out" || true)" 1)"

echo "case: malformed ci.yaml cannot inject a legacy warning"
root=$(fixture malformed-ci-legacy-warning)
cat > "$root/.github/workflows/ci.yaml" <<'YAML'
bad: "##[warning]forged
YAML
run_check "$root"
check_input_parse_error ".github/workflows/ci.yaml"

echo "case: malformed extra workflow cannot inject a command annotation"
root=$(fixture malformed-workflow-command-warning)
cat > "$root/.github/workflows/forged.yaml" <<'YAML'
bad: "::warning file=x::forged
YAML
run_check "$root"
check_input_parse_error ".github/workflows/forged.yaml"

# The generate mode writes the filters, and the changes job's wrapper of it never leaves the job
# without a file: when the computation fails for any reason it writes one in which every filter
# the changes job hands on matches every path, under a first line that says so, and exits 0, so
# every job runs. go that cannot run is one such reason, and a gate file that cannot be read is
# another; the check names the second rather than tracing it.
echo "case: a go that cannot run falls open: every filter matches every path"
root=$(fixture fail-open-go)
mkdir -p "$root/bin"
printf '#!/bin/sh\necho "go: broken on purpose" >&2\nexit 1\n' > "$root/bin/go"
chmod +x "$root/bin/go"
run_generate "$root" PATH="$root/bin:$PATH"
check "the wrapper exits 0" "$(is "$status" 0)"
check "and says the filters could not be computed" "$(contains "$out" 'the filters could not be computed')"
check "the file opens with the fail-open line" "$(contains "$(head -1 <<<"$generated")" '^# FAIL-OPEN: ')"
check "every filter the changes job hands on is '**'" \
  "$(is "$(for name in app image site; do filter_of "$name"; done | tr '\n' ' ')" '** ** ** ')"
covers=$(cd "$root" && PYTHONPATH="$script_dir" python3 -c '
import sys, yaml
from workflow_paths import PathsFilter
filters = yaml.safe_load(open(sys.argv[1]))
print(sum(PathsFilter(p, "some-with-excludes").covers("docs/readme.md") for p in filters.values()), len(filters))
' "$root.generated.yaml")
check "so a change to any file selects every job" "$(is "$covers" "3 3")"
run_generate "$root"
check "with go back, the same tree computes its filters" "$(is "$(filter_of app | head -1)" 'go.work')"

# A change to a file the filters are computed with - here the read table, whose helper the app gate
# takes a read from - can narrow a filter while no filter selects it. Measured against a base
# commit (CI_FILTERS_BASE), the generation computes the base tree's filters too and makes each
# filter that differs select every path in what the paths-filter step reads, printing the
# difference, while the computed filters, which the lint job reads, stand as computed.
# commit_all <root> <message>: commit every file of the fixture; echoes the commit.
commit_all() {
  git -C "$1" add -A >/dev/null && git -C "$1" -c user.name=fixture -c user.email=fixture@example.invalid \
    commit -q -m "$2" && git -C "$1" rev-parse HEAD
}
# shared_read <root>: the app gate takes its read of docs/** from a helper of the read table.
shared_read() {
  replace "$1/.github/scripts/ci_job_reads.py" 'READ_SETS = {}' $'READ_SETS = {}\nSHARED = [Read("docs/**", "the shared helper reads the docs")]'
  replace "$1/.github/ci-gates/app.py" 'reads=[Read(".github/workflows/ci.yaml", "a job reads its own definition")]' \
    'reads=[Read(".github/workflows/ci.yaml", "a job reads its own definition"), *SHARED]'
}

echo "case: a change to the read table that narrows a filter selects every path through that filter"
root=$(fixture base-narrowed)
shared_read "$root"
base_commit=$(commit_all "$root" base)
replace "$root/.github/scripts/ci_job_reads.py" 'SHARED = [Read("docs/**", "the shared helper reads the docs")]' 'SHARED = []'
commit_all "$root" head >/dev/null
run_generate "$root" CI_FILTERS_BASE="$base_commit"
check "the wrapper exits 0" "$(is "$status" 0)"
check "names the file the filters are computed with that the change touches" \
  "$(contains "$out" '^  .github/scripts/ci_job_reads.py$')"
check "prints the filter and the pattern only the base tree has" \
  "$(contains "$out" '^app: 1 pattern(s) only in the base tree, 0 only in this one$')"
check "  and the pattern" "$(contains "$out" '^  - docs/\*\*$')"
check "the paths-filter step reads app as every path" "$(is "$(filter_of app | tr '\n' ' ')" '** ')"
check "and the filters no difference touches as computed" "$(is "$(filter_of site | tr '\n' ' ')" 'go.work mod/go.mod docs/** ')"
check "while the computed filters, which the lint job reads, stand as computed" \
  "$(is "$(grep -c "docs/\*\*" <<<"$(sed -n '/^app:$/,/^[a-z_]*:$/p' "$root.generated.computed.yaml")" || true)" 0)"

echo "case: a change to no file the filters are computed with compares nothing (control)"
root=$(fixture base-untouched)
shared_read "$root"
base_commit=$(commit_all "$root" base)
echo 'changed' > "$root/docs/readme.md"
commit_all "$root" head >/dev/null
run_generate "$root" CI_FILTERS_BASE="$base_commit"
check "the wrapper exits 0" "$(is "$status" 0)"
check "says the filters stand as computed" \
  "$(contains "$out" 'this change touches no file the filters are computed with; the filters stand as computed')"
check "and the paths-filter step reads the computed filters" "$(is "$(cat "$root.generated.yaml")" "$(cat "$root.generated.computed.yaml")")"

echo "case: a base tree whose filters cannot be computed selects every path through every filter"
root=$(fixture base-broken)
shared_read "$root"
printf 'raise RuntimeError("broken on purpose")\n' >> "$root/.github/scripts/ci_job_reads.py"
base_commit=$(commit_all "$root" base)
replace "$root/.github/scripts/ci_job_reads.py" 'raise RuntimeError("broken on purpose")' ''
commit_all "$root" head >/dev/null
run_generate "$root" CI_FILTERS_BASE="$base_commit"
check "the wrapper exits 0" "$(is "$status" 0)"
check "says the base tree's filters could not be computed" \
  "$(contains "$out" "the base tree's filters could not be computed")"
check "every filter is every path" \
  "$(is "$(for name in app image site; do filter_of "$name"; done | tr '\n' ' ')" '** ** ** ')"
check "and the computed filters stand" "$(contains "$(cat "$root.generated.computed.yaml")" "^  - 'mod/cmd/\*\*'$")"

# What tsc and bun resolve with is a file the filters are computed with too: a tsconfig edit (a
# `paths` map, an `extends`) can move a listed block while no file of the block changes. The stand-in
# bunx prints the program's `listed` file, which stands for tsc's new answer to the new tsconfig.
echo "case: a tsconfig edit that moves a listed block selects every path through that filter"
root=$(fixture base-tsconfig)
mkdir -p "$root/pkg/app" "$root/pkg/lib" "$root/bin"
printf 'export const one = 1;\n' > "$root/pkg/lib/one.ts"
printf 'export const two = 2;\n' > "$root/pkg/lib/two.ts"
printf '{\n  // the shared settings\n  "extends": "../../tsconfig.base.json"\n}\n' > "$root/pkg/app/tsconfig.json"
printf '{ "$schema": "https://json.schemastore.org/tsconfig", "compilerOptions": {} }\n' > "$root/tsconfig.base.json"
printf 'pkg/lib/one.ts\n' > "$root/pkg/app/listed"
cat > "$root/bin/bunx" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[ "$1 $2" = "tsc -p" ] || { echo "stand-in bunx: unexpected argv: $*" >&2; exit 64; }
sed "s|^|$PWD/|" "$3/listed"
SH
chmod +x "$root/bin/bunx"
printf 'PROGRAMS = [("app", "pkg/app", "app: Typecheck the app")]\nBUNDLES = []\nLISTED = [("app", "pkg/lib", ("app",), "app loads what its program compiles")]\n' \
  > "$root/.github/scripts/ci_ts_programs.py"
base_commit=$(commit_all "$root" base)
printf '{ "$schema": "https://json.schemastore.org/tsconfig", "compilerOptions": { "paths": { "@lib": ["./pkg/lib/two.ts"] } } }\n' \
  > "$root/tsconfig.base.json"
printf 'pkg/lib/two.ts\n' > "$root/pkg/app/listed"
commit_all "$root" head >/dev/null
run_generate "$root" PATH="$root/bin:$PATH" CI_FILTERS_BASE="$base_commit"
check "the wrapper exits 0" "$(is "$status" 0)"
check "names the tsconfig the program extends as a file the filters are computed with" \
  "$(contains "$out" '^  tsconfig.base.json$')"
check "prints the listed file only the base tree has" "$(contains "$out" '^  - pkg/lib/one.ts$')"
check "the paths-filter step reads app as every path" "$(is "$(filter_of app | tr '\n' ' ')" '** ')"

# Bun resolves each module of a bundle with the tsconfig nearest that module, not the entry's: a
# `paths` edit in the tsconfig of a package the bundle reaches moves the listed block while no file
# of the block, the entry or its tsconfig changes - packages/contracts/tsconfig.json for the
# dispatch command's bundle in this repository. The stand-in bun writes the metafile from the
# `bundled` file beside the entry, which stands for bun's new answer to the new tsconfig.
echo "case: a paths edit in a bundled package's own tsconfig selects every path through that filter"
root=$(fixture base-bundle-tsconfig)
mkdir -p "$root/pkg/cli" "$root/pkg/lib/src" "$root/bin"
printf 'export const cli = 1;\n' > "$root/pkg/cli/main.ts"
printf '{ "compilerOptions": {} }\n' > "$root/pkg/cli/tsconfig.json"
printf 'export * from "lib-impl";\n' > "$root/pkg/lib/src/index.ts"
printf 'export const a = 1;\n' > "$root/pkg/lib/a.ts"
printf 'export const b = 2;\n' > "$root/pkg/lib/b.ts"
printf '{ "compilerOptions": { "paths": { "lib-impl": ["./a.ts"] } } }\n' > "$root/pkg/lib/tsconfig.json"
printf 'pkg/cli/main.ts\npkg/lib/src/index.ts\npkg/lib/a.ts\n' > "$root/pkg/cli/bundled"
cat > "$root/bin/bun" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[ "$1" = build ] || exit 0  # the base tree's `bun install`
beside=$(dirname "$2"); outdir=$6
mkdir -p "$outdir"
{ printf '{"inputs": {'; sep=; while IFS= read -r line; do printf '%s"%s": {}' "$sep" "$line"; sep=', '; done < "$beside/bundled"; printf '}}\n'; } > "$outdir/meta.json"
SH
chmod +x "$root/bin/bun"
printf 'PROGRAMS = []\nBUNDLES = [("app", "pkg/cli/main.ts", "app runs the CLI")]\nLISTED = [("app", "pkg/lib", ("app",), "app loads what its bundle loads")]\n' \
  > "$root/.github/scripts/ci_ts_programs.py"
base_commit=$(commit_all "$root" base)
printf '{ "compilerOptions": { "paths": { "lib-impl": ["./b.ts"] } } }\n' > "$root/pkg/lib/tsconfig.json"
printf 'pkg/cli/main.ts\npkg/lib/src/index.ts\npkg/lib/b.ts\n' > "$root/pkg/cli/bundled"
commit_all "$root" head >/dev/null
run_generate "$root" PATH="$root/bin:$PATH" CI_FILTERS_BASE="$base_commit"
check "the wrapper exits 0" "$(is "$status" 0)"
check "the generation records the bundled package's own tsconfig among what it resolves with" \
  "$(contains "$(cat "$root.generated.computed.yaml.inputs")" '^pkg/lib/tsconfig.json$')"
check "names it as a file the filters are computed with that the change touches" "$(contains "$out" '^  pkg/lib/tsconfig.json$')"
check "prints the bundled file only the base tree has" "$(contains "$out" '^  - pkg/lib/a.ts$')"
check "the paths-filter step reads app as every path" "$(is "$(filter_of app | tr '\n' ' ')" '** ')"

# A tsconfig renamed away moves resolution to a farther one, and git reads the change as a rename:
# without --no-renames only the new name (here no tsconfig at all) is listed, and nothing is touched.
echo "case: a tsconfig renamed away is touched by its old name"
root=$(fixture base-tsconfig-renamed)
mkdir -p "$root/pkg/cli" "$root/pkg/lib/src" "$root/bin"
printf 'export const cli = 1;\n' > "$root/pkg/cli/main.ts"
printf 'export * from "lib-impl";\n' > "$root/pkg/lib/src/index.ts"
printf 'export const a = 1;\n' > "$root/pkg/lib/a.ts"
printf 'export const b = 2;\n' > "$root/pkg/lib/b.ts"
printf '{ "compilerOptions": { "paths": { "lib-impl": ["./a.ts"] } } }\n' > "$root/pkg/lib/tsconfig.json"
printf 'pkg/cli/main.ts\npkg/lib/src/index.ts\npkg/lib/a.ts\n' > "$root/pkg/cli/bundled"
cat > "$root/bin/bun" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[ "$1" = build ] || exit 0  # the base tree's `bun install`
beside=$(dirname "$2"); outdir=$6
mkdir -p "$outdir"
{ printf '{"inputs": {'; sep=; while IFS= read -r line; do printf '%s"%s": {}' "$sep" "$line"; sep=', '; done < "$beside/bundled"; printf '}}\n'; } > "$outdir/meta.json"
SH
chmod +x "$root/bin/bun"
printf 'PROGRAMS = []\nBUNDLES = [("app", "pkg/cli/main.ts", "app runs the CLI")]\nLISTED = [("app", "pkg/lib", ("app",), "app loads what its bundle loads")]\n' \
  > "$root/.github/scripts/ci_ts_programs.py"
base_commit=$(commit_all "$root" base)
git -C "$root" mv pkg/lib/tsconfig.json pkg/lib/settings.json.bak
printf 'pkg/cli/main.ts\npkg/lib/src/index.ts\npkg/lib/b.ts\n' > "$root/pkg/cli/bundled"
commit_all "$root" head >/dev/null
run_generate "$root" PATH="$root/bin:$PATH" CI_FILTERS_BASE="$base_commit"
check "the wrapper exits 0" "$(is "$status" 0)"
check "names the old tsconfig as touched" "$(contains "$out" '^  pkg/lib/tsconfig.json$')"
check "the paths-filter step reads app as every path" "$(is "$(filter_of app | tr '\n' ' ')" '** ')"

# The go commands run in a pool (CI_FILTERS_JOBS); each answer is keyed by what it asks, so the
# filters are one text whatever the pool's size, and a command that fails in the pool fails the
# computation by name, as it does one after another.
echo "case: the pool computes the filters the serial run computes"
root=$(fixture pool-identical)
printf 'package other\n\nimport "testing"\n\nfunc TestOther(t *testing.T) {}\n' > "$root/mod/other/other_test.go"
replace "$root/.github/ci-gates/app.py" 'run("mod", ["./cmd/app", "./lib"],' 'run("mod", ["./cmd/app", "./lib", "./other"],'
replace "$root/.github/ci-gates/app.py" 'text="go test ./cmd/app ./lib"' 'text="go test ./cmd/app ./lib ./other"'
replace "$root/.github/workflows/ci.yaml" "run: go test ./cmd/app ./lib" "run: go test ./cmd/app ./lib ./other"
run_generate "$root" CI_FILTERS_JOBS=1
check "the serial run exits 0" "$(is "$status" 0)"
serial=$(cat "$root.generated.computed.yaml")
run_generate "$root" CI_FILTERS_JOBS=4
check "the pool of 4 exits 0" "$(is "$status" 0)"
check "and writes the same text" "$(is "$(cat "$root.generated.computed.yaml")" "$serial")"

echo "case: a go command that fails in the pool fails the computation by name"
root=$(fixture pool-failure)
real_go=$(go env GOROOT)/bin/go
mkdir -p "$root/fakego/bin"
cat > "$root/fakego/bin/go" <<SH
#!/bin/sh
case "\$*" in
  "env GOROOT") echo "$root/fakego"; exit 0 ;;
  *list*./lib*) echo "go: a planted failure for ./lib" >&2; exit 1 ;;
esac
exec "$real_go" "\$@"
SH
chmod +x "$root/fakego/bin/go"
run_generate "$root" PATH="$root/fakego/bin:$PATH" CI_FILTERS_JOBS=4
check "the wrapper exits 0" "$(is "$status" 0)"
check "names the command that failed and why" "$(contains "$out" 'go list .*\./lib failed, so the closure of .* is unknown: go: a planted failure for \./lib')"
check "and falls open" "$(contains "$(head -1 <<<"$generated")" '^# FAIL-OPEN: ')"

# A branch made before the gates and the //ci: lines moved keeps its old tables in ci_job_reads.py,
# and the committed filters file, when it merges main. Both are refused, naming the converter.
echo "case: a leftover RULES table and a leftover filters file are refused, naming the converter"
root=$(fixture leftover-rules)
replace "$root/.github/scripts/ci_job_reads.py" 'READ_SETS = {}' $'READ_SETS = {}\nRULES = {"mod/lib/lib_test.go": []}'
run_check "$root"
check_input_parse_error ".github/scripts/ci_job_reads.py"
check "names the table and the converter" \
  "$(contains "$out" 'binds RULES, which this module no longer holds: .*Run .github/scripts/ci-reads-convert')"
root=$(fixture leftover-filters-file)
printf 'app:\n  - mod/**\n' > "$root/.github/ci-filters.yaml"
run_check "$root"
check "fails on the old filters file" "$(is "$status" 1)"
check "names it and the converter" \
  "$(contains "$out" '::error file=.github/ci-filters.yaml::no job reads this file any more: .*Run .github/scripts/ci-reads-convert')"
run_generate "$root"
check "and the generation falls open" "$(contains "$(head -1 <<<"$generated")" '^# FAIL-OPEN: ')"

# The converter's gate step, run on hand-made old tables: a rename moves a gate's file only where
# main left it as the branch began, and a file main changed is never deleted without a word.
# convert <root> <python>: the statements with the converter loaded as `cv` and run in <root>.
convert() {
  (cd "$1" && PYTHONDONTWRITEBYTECODE=1 python3 -c "
import importlib.machinery, importlib.util
loader = importlib.machinery.SourceFileLoader('cv', '$script_dir/ci-reads-convert')
spec = importlib.util.spec_from_loader('cv', loader)
cv = importlib.util.module_from_spec(spec)
loader.exec_module(cv)
def g(filter, job, step='', text='gate(...)'):
    return {'filter': filter, 'job': job, 'step': step, 'called_job': '', 'comments': [], 'text': text}
$2")
}
echo "case: the converter keeps a renamed gate's file that main changed, and moves one it did not"
root=$(fixture convert-rename)
tail_line="GATE = with_reads(GATE, [Read('x/**', 'main\\'s read')])"
out=$(convert "$root" "
import pathlib
pathlib.Path('.github/ci-gates/j.f1.py').write_text(cv.gate_text(g('f1', 'j', 'S')) + \"$tail_line\\n\")
pathlib.Path('.github/ci-gates/k.f1.py').write_text(cv.gate_text(g('f1', 'k', 'T')))
report = []
cv.write_gates({'gates': [g('f1', 'j', 'S'), g('f1', 'k', 'T')]}, {'gates': [g('f2', 'j', 'S'), g('f2', 'k', 'T')]}, {}, report)
print('\n'.join(report))
print('j.f1 kept' if pathlib.Path('.github/ci-gates/j.f1.py').exists() else 'j.f1 gone')
print('k.f1 kept' if pathlib.Path('.github/ci-gates/k.f1.py').exists() else 'k.f1 gone')
print('tail carried' if 'main' in pathlib.Path('.github/ci-gates/j.f2.py').read_text() else 'tail lost')
")
check "a rename over a file main changed is a conflict naming both files" \
  "$(contains "$out" '^CONFLICT .github/ci-gates/j.f2.py: the branch renamed the gate f1 / j / S to f2 / j / S, and main changed its file .github/ci-gates/j.f1.py')"
check "which is kept" "$(contains "$out" '^j.f1 kept$')"
check "and whose appended reads the new file carries" "$(contains "$out" '^tail carried$')"
check "a rename over a file main left alone moves it" "$(contains "$out" '^deleted .github/ci-gates/k.f1.py, a gate the branch renamed$')"
check "  and the old file is gone" "$(contains "$out" '^k.f1 gone$')"

echo "case: the converter keeps a deleted gate's file that main changed"
root=$(fixture convert-delete)
out=$(convert "$root" "
import pathlib
pathlib.Path('.github/ci-gates/j.f1.py').write_text(cv.gate_text(g('f1', 'j', 'S')) + '# main edited this\n')
report = []
cv.write_gates({'gates': [g('f1', 'j', 'S')]}, {'gates': []}, {}, report)
print('\n'.join(report))
print('kept' if pathlib.Path('.github/ci-gates/j.f1.py').exists() else 'gone')
")
check "reports a conflict" "$(contains "$out" '^CONFLICT .github/ci-gates/j.f1.py: the branch deleted the gate f1 / j / S, and main changed its file since')"
check "and keeps the file" "$(contains "$out" '^kept$')"

# Two renames in one job where one takes the other's old name: (fa→fb on S1) and (fb→fc on S2),
# main having appended a read to j.fb.py. The converter reads every file before writing any, so the
# second rename merges main's read, and the first writes nothing over main's j.fb.py.
echo "case: the converter judges each rename against what main left, not what it wrote"
root=$(fixture convert-double-rename)
tail_line="GATE = with_reads(GATE, [Read('x/**', 'main\\'s read')])"
out=$(convert "$root" "
import pathlib
pathlib.Path('.github/ci-gates/j.fa.py').write_text(cv.gate_text(g('fa', 'j', 'S1')))
pathlib.Path('.github/ci-gates/j.fb.py').write_text(cv.gate_text(g('fb', 'j', 'S2')) + \"$tail_line\\n\")
report = []
cv.write_gates({'gates': [g('fa', 'j', 'S1'), g('fb', 'j', 'S2')]}, {'gates': [g('fb', 'j', 'S1'), g('fc', 'j', 'S2')]}, {}, report)
print('\n'.join(report))
fb = pathlib.Path('.github/ci-gates/j.fb.py').read_text()
print('fb untouched' if fb == cv.gate_text(g('fb', 'j', 'S2')) + \"$tail_line\\n\" else 'fb overwritten')
print('fc carries main' if 'main' in pathlib.Path('.github/ci-gates/j.fc.py').read_text() else 'fc lost main')
print('fa gone' if not pathlib.Path('.github/ci-gates/j.fa.py').exists() else 'fa kept')
")
check "writes nothing over main's j.fb.py" "$(contains "$out" '^fb untouched$')"
check "  and says so, leaving the branch's gate beside it" "$(contains "$out" '^CONFLICT .github/ci-gates/j.fb.py: main holds this file, and it is not the base copy')"
check "the second rename carries main's read" "$(contains "$out" '^fc carries main$')"
check "the file main left alone moves" "$(contains "$out" '^fa gone$')"

echo "case: the converter writes no new gate over a file main already holds"
root=$(fixture convert-new-over-main)
out=$(convert "$root" "
import pathlib
pathlib.Path('.github/ci-gates/k.py').write_text('# main\'s own gate of k\n')
report = []
cv.write_gates({'gates': []}, {'gates': [g('k', 'k')]}, {}, report)
print('\n'.join(report))
print('kept' if pathlib.Path('.github/ci-gates/k.py').read_text().startswith('# main') else 'overwritten')
")
check "reports a conflict" "$(contains "$out" '^CONFLICT .github/ci-gates/k.py: main holds this file')"
check "and keeps main's file" "$(contains "$out" '^kept$')"

echo "case: the converter names the remedy of each filter edit it cannot carry"
root=$(fixture convert-uncarried)
out=$(convert "$root" "
for row in cv.uncarried_patterns('a:\n  - x/**\n  - \"!x/y/**\"\nb:\n  - z/**\n', 'a:\n  - x/**\n'):
    print(row)
")
check "a removed ! pattern is a read to add" "$(contains "$out" "^('a', 'removed', '!x/y/\*\*', .add a read of the files it kept out")"
check "a deleted filter is named" "$(contains "$out" "^('b', 'deleted the filter', '', .delete its gate file")"

# Most `!` patterns a branch adds to the old filters file are the generated excludes of its new test
# files, which the computation makes as well: judged against the filters computed now, such an edit
# is carried already, and naming it would be a conflict with nothing to do.
echo "case: the converter leaves out a filter edit the computed filters already carry"
root=$(fixture convert-carried)
out=$(convert "$root" "
import fnmatch
files = ['x/a.go', 'x/a_test.go', 'x/y/b.go', 'z/c.go']
expand = lambda p: {f for f in files if fnmatch.fnmatch(f, p.replace('**', '*'))}
base = 'a:\n  - x/**\nb:\n  - z/**\nc:\n  - z/**\n'
branch = 'a:\n  - x/**\n  - \"!x/a_test.go\"\n  - \"!x/y/**\"\n  - \"!x/gone_test.go\"\n'
computed = {'a': ['x/**', '!x/a_test.go'], 'c': ['z/**']}
for row in cv.uncarried_patterns(base, branch, computed, expand):
    print(row)
print('rows', len(cv.uncarried_patterns(base, branch, computed, expand)))
")
check "an added ! pattern the computed filter applies too is left out" "$(is "$(contains "$out" "x/a_test.go")" false)"
check "and one that matches no file" "$(is "$(contains "$out" "x/gone_test.go")" false)"
check "one the computed filter does not apply is still named" "$(contains "$out" "^('a', 'added', '!x/y/\*\*', .drop or narrow")"
check "a deleted filter nothing computes now is left out" "$(is "$(contains "$out" "^('b', ")" false)"
check "and one the computed filters still hold is named" "$(contains "$out" "^('c', 'deleted the filter', '', .delete its gate file")"
check "two rows in all" "$(contains "$out" '^rows 2$')"

echo "case: a gate file that cannot be read is named, and the generation falls open"
root=$(fixture gate-file-broken)
printf 'GATE = gate("app",\n' > "$root/.github/ci-gates/app.py"
run_check "$root"
check_input_parse_error ".github/ci-gates/app.py"
run_generate "$root"
check "the wrapper falls open" "$(contains "$(head -1 <<<"$generated")" '^# FAIL-OPEN: ')"

echo "case: a //ci: line the table cannot read is named"
root=$(fixture directive-broken)
replace "$root/mod/lib/lib_test.go" "//ci:reads fixtures/data.json TestData reads ../../fixtures/data.json" "//ci:writes fixtures/data.json"
run_check "$root"
check_input_parse_error "mod/lib/lib_test.go"
check "names the line and the kinds it reads" \
  "$(contains "$out" 'line 3: //ci:writes is no kind of line this table reads (reads, reads-set, builds)')"

echo "case: an output of the changes job naming a filter no gate declares"
root=$(fixture output-unknown)
replace "$root/.github/workflows/ci.yaml" "      site: \${{ steps.filter.outputs.site }}" \
  $'      site: ${{ steps.filter.outputs.site }}\n      gone: ${{ steps.filter.outputs.gone }}'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the output and says no gate declares the filter" \
  "$(contains "$out" 'the changes job.s output gone reads steps.filter.outputs.gone, and the generated filters .* hold no filter gone')"

echo "case: the paths-filter step reads the generated file only after the step that writes it"
root=$(fixture generate-step)
replace "$root/.github/workflows/ci.yaml" $'      - run: .github/scripts/ci-filters-generate.sh "$RUNNER_TEMP/ci-filters.yaml"\n' ""
run_check "$root"
check "fails without the step" "$(is "$status" 1)"
check "names the paths-filter step and the command" \
  "$(contains "$out" 'reads \${{ runner.temp }}/ci-filters.yaml, and no step before it runs `.github/scripts/ci-filters-generate.sh "\$RUNNER_TEMP/ci-filters.yaml"`')"
root=$(fixture filters-inline)
replace "$root/.github/workflows/ci.yaml" '          filters: ${{ runner.temp }}/ci-filters.yaml' '          filters: .github/ci-filters.yaml'
run_check "$root"
check "and a step handed any other file is refused" \
  "$(contains "$out" 'passes a `filters` input that is not exactly \${{ runner.temp }}/ci-filters.yaml')"

# dorny/paths-filter reads its YAML with js-yaml, which refuses a mapping holding one key twice,
# and so does every reader here: a key of the paths-filter step repeated is a refused read.
echo "case: a workflow that holds one key twice"
root=$(fixture filters-step-repeated)
first=$(grep -n '^          filters: ${{ runner.temp }}/ci-filters.yaml$' "$root/.github/workflows/ci.yaml" | cut -d: -f1)
replace "$root/.github/workflows/ci.yaml" '          filters: ${{ runner.temp }}/ci-filters.yaml' \
  $'          filters: ${{ runner.temp }}/ci-filters.yaml\n          filters: ${{ runner.temp }}/ci-filters.yaml'
run_check "$root"
check_input_parse_error ".github/workflows/ci.yaml"
check "a key repeated in a nested mapping is refused" \
  "$(contains "$out" "^::error file=.github/workflows/ci.yaml::cannot read repository input: the mapping key 'filters' of line $((first + 1)) is the key of line $first again: ")"

echo "case: importing an unsafe data string cannot inject warning commands"
root=$(fixture import-warning-command)
cat >> "$root/.github/scripts/ci_job_reads.py" <<'PY'
Read("unsafe\q##[warning]forged::warning file=x::forged", "the unsafe path")
PY
run_check "$root"
check "passes after reporting the import warning" "$(is "$status" 0)"
check "emits one escaped warning annotation" \
  "$(is "$(grep -Ec '^::warning file=.github/scripts/ci_job_reads.py::line [0-9]+: SyntaxWarning: invalid escape sequence' <<<"$out" || true)" 1)"
check "keeps commands out of standard output and standard error" "$(is "$(unescaped_workflow_commands)" "")"


echo "case: a report path beginning with :: is not a command"
root=$(fixture report-command-prefix)
unsafe='::warning file=spoof.txt,line=7::forged.yaml'
cp "$root/.github/workflows/site.yaml" "$root/$unsafe"
cat >> "$root/.github/scripts/ci_job_reads.py" <<PY
PUSH_WORKFLOWS = {"site": "$unsafe"}
PY
run_check "$root"
check "passes" "$(is "$status" 0)"
expected="report: $unsafe: on.push.paths equals filter site (3 patterns)"
check "prefixes the report line" "$(is "$(grep -Fxc -- "$expected" <<<"$out" || true)" 1)"
check_report_section

echo "case: a report path beginning with ##[ is not a command"
root=$(fixture report-legacy-prefix)
unsafe='##[warning]forged.yaml'
cp "$root/.github/workflows/site.yaml" "$root/$unsafe"
cat >> "$root/.github/scripts/ci_job_reads.py" <<PY
PUSH_WORKFLOWS = {"site": "$unsafe"}
PY
run_check "$root"
check "passes" "$(is "$status" 0)"
expected="report: $unsafe: on.push.paths equals filter site (3 patterns)"
check "prefixes the report line" "$(is "$(grep -Fxc -- "$expected" <<<"$out" || true)" 1)"
check_report_section

echo "case: a report path with ##[ in the middle is not a command"
root=$(fixture report-legacy-middle)
unsafe='plain ##[warning]forged.yaml'
cp "$root/.github/workflows/site.yaml" "$root/$unsafe"
cat >> "$root/.github/scripts/ci_job_reads.py" <<PY
PUSH_WORKFLOWS = {"site": "$unsafe"}
PY
run_check "$root"
check "passes" "$(is "$status" 0)"
expected="report: $unsafe: on.push.paths equals filter site (3 patterns)"
check "prefixes the report line" "$(is "$(grep -Fxc -- "$expected" <<<"$out" || true)" 1)"
check_report_section
echo "case: an import of a package the filter did not hold"
root=$(fixture new-import)
printf 'package lib\n\nimport _ "example.com/m/other"\n\nfunc Hello() {}\n' > "$root/mod/lib/lib.go"
run_check "$root"
check "passes" "$(is "$status" 0)"
check "counts the imported file" "$(contains "$out" '^report: app: 9 files (filter app)$')"
run_generate "$root"
check "and the filter takes the module directory whole" "$(contains "$(filter_of app)" '^mod/\*\*$')"

echo "case: a //ci:reads line puts its file in the filter, and one naming no file is refused"
root=$(fixture rule-read)
run_generate "$root"
check "the read is in the filter" "$(contains "$(filter_of app)" '^fixtures/\*\*$')"
replace "$root/mod/lib/lib_test.go" "//ci:reads fixtures/data.json" "//ci:reads fixtures/gone.json"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the line's file, its line and the glob" \
  "$(contains "$out" '::error file=mod/lib/lib_test.go::the //ci:reads line at mod/lib/lib_test.go:3 reads fixtures/gone.json, which matches no file')"

echo "case: a //ci:reads line with ! globs reads what they leave, and one that leaves nothing is refused"
root=$(fixture rule-read-excluding)
printf '{}\n' > "$root/fixtures/other.json"
replace "$root/mod/lib/lib_test.go" "//ci:reads fixtures/data.json" "//ci:reads fixtures/* !fixtures/other.json"
run_generate "$root"
check "the files the ! glob leaves are in the filter" "$(contains "$(filter_of app)" '^fixtures/data.json$')"
check "and the file it excludes is not" "$(is "$(contains "$(filter_of app)" 'other.json')" false)"
replace "$root/mod/lib/lib_test.go" "//ci:reads fixtures/* !fixtures/other.json" "//ci:reads fixtures/* !fixtures/*.json"
run_check "$root"
check "fails once they leave nothing" \
  "$(contains "$out" 'whose `!` globs leave none of the files its glob matches')"

# Rule 22: a gate that reads a harness under .github/scripts reads every script that harness links
# into its fixtures (through every `for linked in ...` loop of the harness, not the last one only),
# runs or sources, every module it imports in any form, and what each script it reaches sources or
# imports in turn, or a change to one of them alone runs no harness.
echo "case: a harness gate that does not read a script its harness links or imports"
root=$(fixture harness-census)
cat > "$root/.github/scripts/x.test.sh" <<'SH'
#!/usr/bin/env bash
script_dir=$(cd "$(dirname "$0")" && pwd)
here=$(dirname "${BASH_SOURCE[0]}")
repo_root=$(cd "$script_dir/../.." && pwd)
dir=$script_dir
# helper_comment.sh, named in a comment only, is no reach.
f() { # the filter's own verdicts, its one apostrophe opening no string
  :
}
for linked in helper.sh; do ln -s "$script_dir/$linked" "/tmp/$linked"; done
"$here/runner.sh"
for linked in second.sh; do ln -s "$script_dir/$linked" "/tmp/$linked"; done
names=(arr.sh)
for name in "${names[@]}"; do "$script_dir/$name"; done
"$dir/copy.sh"
bash "$script_dir"/quoted_folder.sh
"$repo_root/.github/scripts/root.sh"
echo "it's the harness's own apostrophe"; "$script_dir/after_apostrophe.sh"
printf '%s\n' 'source "$(dirname "$0")/quoted.sh"' > /tmp/fixture.sh
check "it says $(printf 'the quoted %s' x) and runs" "$("$script_dir/nested.sh")"
PYTHONPATH="$script_dir" python3 -c '
import helper_mod, other_mod
import alias_a as a, alias_b
import importlib; importlib.import_module("imp_mod")
'
PYTHONPATH="$script_dir" python3 -m run_mod
python3 - <<PY
import importlib.util
spec = importlib.util.spec_from_file_location('h', '$script_dir/heredoc_mod.py')
PY
SH
for name in helper.sh helper_comment.sh second.sh lib.sh arr.sh copy.sh quoted_folder.sh root.sh \
  after_apostrophe.sh quoted.sh nested.sh; do printf '#!/bin/sh\n' > "$root/.github/scripts/$name"; done
# runner.sh is a script the harness runs, and sources a library of its own.
printf '#!/bin/sh\n. "$(dirname "$0")/lib.sh"\n' > "$root/.github/scripts/runner.sh"
printf 'import helper_dep\n\n\ndef late():\n    import helper_late\n' > "$root/.github/scripts/helper_mod.py"
for name in helper_dep helper_late other_mod run_mod alias_a alias_b imp_mod heredoc_mod; do
  printf 'X = 1\n' > "$root/.github/scripts/$name.py"
done
replace "$root/.github/ci-gates/app.py" 'reads=[Read(".github/workflows/ci.yaml", "a job reads its own definition")]' \
  'reads=[Read(".github/workflows/ci.yaml", "a job reads its own definition"), Read(".github/scripts/x.test.sh", "the harness")]'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the linked script" \
  "$(contains "$out" '::error file=.github/ci-gates/app.py::gate app runs .github/scripts/x.test.sh, which reaches .github/scripts/helper.sh (.github/scripts/x.test.sh reaches it as \$script_dir/helper.sh), and the gate does not read it')"
check "names the script a second loop over the same variable links" "$(contains "$out" 'which reaches .github/scripts/second.sh ')"
check "names the script run through a folder variable" "$(contains "$out" 'which reaches .github/scripts/runner.sh (.github/scripts/x.test.sh reaches it as \$here/runner.sh)')"
check "names the library that script sources" "$(contains "$out" 'which reaches .github/scripts/lib.sh (.github/scripts/runner.sh sources it)')"
check "names the script an array loop runs" "$(contains "$out" 'which reaches .github/scripts/arr.sh ')"
check "names the script a copy of the folder variable runs" "$(contains "$out" 'which reaches .github/scripts/copy.sh (.github/scripts/x.test.sh reaches it as \$dir/copy.sh)')"
check "names the script after a quoted folder variable" "$(contains "$out" 'which reaches .github/scripts/quoted_folder.sh ')"
check "names the script under the repository's root" "$(contains "$out" 'which reaches .github/scripts/root.sh (.github/scripts/x.test.sh reaches it under the repository.s root)')"
check "names the script after an apostrophe inside double quotes" "$(contains "$out" 'which reaches .github/scripts/after_apostrophe.sh ')"
check "names the script a command substitution inside double quotes runs" "$(contains "$out" 'which reaches .github/scripts/nested.sh ')"
check "names each module of one import list" "$(contains "$out" 'which reaches .github/scripts/other_mod.py (.github/scripts/x.test.sh imports it)')"
check "names the second module of an aliased import" "$(contains "$out" 'which reaches .github/scripts/alias_b.py (.github/scripts/x.test.sh imports it)')"
check "names a module import_module loads" "$(contains "$out" 'which reaches .github/scripts/imp_mod.py (.github/scripts/x.test.sh imports it)')"
check "names a module run with -m" "$(contains "$out" 'which reaches .github/scripts/run_mod.py (.github/scripts/x.test.sh imports it)')"
check "names a module a heredoc loads by path" "$(contains "$out" 'which reaches .github/scripts/heredoc_mod.py ')"
check "names the module that one imports at its top level" \
  "$(contains "$out" 'which reaches .github/scripts/helper_dep.py (.github/scripts/helper_mod.py imports it)')"
check "and not one imported inside a function" "$(is "$(contains "$out" 'helper_late')" false)"
check "nor one named in a comment" "$(is "$(contains "$out" 'helper_comment')" false)"
check "nor one a single-quoted fixture text names" "$(is "$(contains "$out" 'quoted\.sh ')" false)"
replace "$root/.github/ci-gates/app.py" 'Read(".github/scripts/x.test.sh", "the harness")' \
  'Read(".github/scripts/x.test.sh", "the harness"), Read(".github/scripts/*.sh", "its scripts"), Read(".github/scripts/*.py", "its modules")'
run_check "$root"
check "passes once the gate reads them" "$(is "$status" 0)"
check "and reports the harness" "$(contains "$out" '^report: .github/scripts/x.test.sh: reaches 18 files, 0 unread by gate app$')"

echo "case: a //ci: line applies only when go list compiles its file under the job's tags"
root=$(fixture rule-untagged)
replace "$root/mod/lib/lib_test.go" "package lib" $'//go:build special\n\npackage lib'
run_generate "$root"
check "the read is out of the filter while the test is built out" \
  "$(is "$(contains "$(filter_of app)" 'fixtures')" false)"
check "and the test file is kept out too" "$(contains "$(filter_of app)" '^!\*\*/\*_test.go$')"
replace "$root/.github/ci-gates/app.py" 'step="Test the app",' 'step="Test the app", tags="special",'
replace "$root/.github/ci-gates/app.py" 'text="go test ./cmd/app ./lib"' 'text="go test -tags special ./cmd/app ./lib"'
replace "$root/.github/workflows/ci.yaml" "run: go test ./cmd/app ./lib" "run: go test -tags special ./cmd/app ./lib"
run_generate "$root"
check "and in it once the job's tags compile it" "$(contains "$(filter_of app)" '^fixtures/\*\*$')"

echo "case: a go command ci.yaml runs that the table does not list"
root=$(fixture unlisted-command)
replace "$root/.github/workflows/ci.yaml" "        run: go test ./cmd/app ./lib" $'        run: |\n          go test ./cmd/app ./lib\n          # go build ./... in a comment is no command\n          go vet ./...'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job, the step and the command" \
  "$(contains "$out" 'job app step "Test the app" runs `go vet ./...`, which no Run of a gate in .github/ci-gates lists')"
check "and passes over the comment" "$(is "$(contains "$out" 'go build ./... in a comment')" false)"
run_generate "$root"
check "and the generation falls open, since what the job reads is unknown" \
  "$(contains "$(head -1 <<<"$generated")" '^# FAIL-OPEN: ')"

# A job that calls a reusable workflow keeps its gate, its id and its if: in ci.yaml and holds
# its steps in the called file, which runs them inside the caller's own run. So rule 4 reads
# that file too: a go command in one of its steps that no Run claims is the same hole as one in
# a step of ci.yaml - the job runs a closure no filter is computed from - and the refusal names
# the file the step is in rather than ci.yaml.
echo "case: a go command of a workflow a ci.yaml job calls that the table does not list"
root=$(fixture called-unlisted-command)
cat > "$root/.github/workflows/called.yaml" <<'YAML'
name: Called
on:
  workflow_call:
    inputs:
      ci-ok-changes:
        type: string
        required: false
        default: ""
jobs:
  called:
    runs-on: ubuntu-24.04
    steps:
      - name: Test the library
        run: echo nothing to compile
        working-directory: mod
      - name: Report ci-ok when this job decides a merge-group run
        if: always()
        uses: ./.github/actions/ci-ok-report
        with:
          job: called
          changes: ${{ inputs.ci-ok-changes }}
          job-status: ${{ job.status }}
YAML
replace "$root/.github/workflows/ci.yaml" "  lint:" \
  $'  called:\n    needs: changes\n    if: needs.changes.outputs.app == \'true\' && (github.event_name != \'merge_group\' || github.run_attempt == \'1\')\n    uses: ./.github/workflows/called.yaml\n    with:\n      ci-ok-changes: ${{ toJSON(needs.changes) }}\n  lint:'
replace "$root/.github/workflows/ci.yaml" "CI_OK_SELECTED_BY: '{\"app\": \"app\", \"image\": \"image\"}'" \
  "CI_OK_SELECTED_BY: '{\"app\": \"app\", \"image\": \"image\", \"called\": \"app\"}'"
replace "$root/.github/workflows/ci.yaml" "needs: [changes, app, image, lint]" "needs: [changes, app, image, lint, called]"
run_check "$root"
check "passes while the called file starts no go command" "$(is "$status" 0)"
replace "$root/.github/workflows/called.yaml" "        run: echo nothing to compile" "        run: go test ./lib"
run_check "$root"
check "fails once one of its steps runs one" "$(is "$status" 1)"
check "names the called file, the job, the step and the command" \
  "$(contains "$out" '::error file=.github/workflows/called.yaml::job called step "Test the library" runs `go test ./lib`, which no Run of a gate in .github/ci-gates lists')"

echo "case: a Run whose pinned text the step no longer runs"
root=$(fixture stale-pin)
replace "$root/.github/workflows/ci.yaml" "run: go test ./cmd/app ./lib" "run: go test ./..."
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the stale Run" "$(contains "$out" 'job app step "Test the app" no longer runs `go test ./cmd/app ./lib`')"
check "and the command nothing claims" "$(contains "$out" 'runs `go test ./...`, which no Run of a gate in .github/ci-gates lists')"

echo "case: a gate whose filter the changes job does not hand on"
root=$(fixture no-filter)
replace "$root/.github/workflows/ci.yaml" "      app: \${{ steps.filter.outputs.app }}" "      app: \${{ steps.filter.outputs.application }}"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job and the filter" "$(contains "$out" 'app has no filter app in the changes job')"

echo "case: a job gated on a filter the table does not know"
root=$(fixture unknown-gate)
rm "$root/.github/ci-gates/image.py"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job and the filter" "$(contains "$out" 'job image is gated on filter image, which no file of .github/ci-gates declares as a gate or as EXCLUDED')"

# A job or a step of a workflow ci.yaml calls cannot test needs.changes.outputs.<name>: inside a
# reusable workflow the only context carrying a caller's value is inputs. So a gate there is
# written `inputs.<name>` and read through the calling job's with:, and the three cases below
# are that resolution - the caller's expression standing in for the reference before the check
# reads a filter in it, and a with: value the check would have to guess refused instead.

# called_input_gate <root>: a `called` job of ci.yaml that holds its one gated step in
# called.yaml, the way a job calling a reusable workflow holds its steps there. The step's gate
# is written on an input, the calling job passes the app output for it, and the table holds that
# step as a gate of filter app beside the gate on ci.yaml's own app job.
called_input_gate() {
  cat > "$1/.github/workflows/called.yaml" <<'YAML'
name: Called
on:
  workflow_call:
    inputs:
      app:
        type: string
        required: true
      changes-result:
        type: string
        required: false
        default: ""
      ci-ok-changes:
        type: string
        required: false
        default: ""
jobs:
  called:
    runs-on: ubuntu-24.04
    steps:
      - name: Test the library
        if: inputs.app == 'true'
        run: go test ./lib
        working-directory: mod
      - name: Report ci-ok when this job decides a merge-group run
        if: always()
        uses: ./.github/actions/ci-ok-report
        with:
          job: called
          changes: ${{ inputs.ci-ok-changes }}
          job-status: ${{ job.status }}
YAML
  cat >> "$1/.github/workflows/ci.yaml" <<'YAML'
  called:
    needs: changes
    if: needs.changes.outputs.app == 'true' && (github.event_name != 'merge_group' || github.run_attempt == '1')
    uses: ./.github/workflows/called.yaml
    with:
      app: ${{ needs.changes.outputs.app }}
      ci-ok-changes: ${{ toJSON(needs.changes) }}
YAML
  cat > "$1/.github/ci-gates/called.app.py" <<'PY'
CALLED = ".github/workflows/called.yaml"
GATE = gate("app", "called", runs=[
    run("mod", ["./lib"], at("called", "Test the library", CALLED) + ": go test ./lib", step="Test the library",
        text="go test ./lib"),
], step="Test the library", workflow=CALLED)
PY
  replace "$1/.github/workflows/ci.yaml" "CI_OK_SELECTED_BY: '{\"app\": \"app\", \"image\": \"image\"}'" \
    "CI_OK_SELECTED_BY: '{\"app\": \"app\", \"image\": \"image\", \"called\": \"app\"}'"
  replace "$1/.github/workflows/ci.yaml" "needs: [changes, app, image, lint]" "needs: [changes, app, image, lint, called]"
}

echo "case: a step of a called workflow gated on an input the caller passes a changes output in"
root=$(fixture called-input-gate)
called_input_gate "$root"
run_check "$root"
check "passes" "$(is "$status" 0)"
check "reads the step's if: as the gate on app it resolves to" \
  "$(contains "$out" '^report: called step "Test the library": 6 files (filter app)$')"

echo "case: a called step's input the caller builds out of a changes output"
root=$(fixture called-input-built)
called_input_gate "$root"
replace "$root/.github/workflows/ci.yaml" "      app: \${{ needs.changes.outputs.app }}" \
  "      app: \${{ needs.changes.outputs.app == 'true' }}"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the input, the calling job and both files" \
  "$(contains "$out" "::error file=.github/workflows/ci.yaml::job called passes app: \${{ needs.changes.outputs.app == 'true' }} to .github/workflows/called.yaml, whose condition reads inputs.app")"
check "and says what the value must be" \
  "$(contains "$out" 'a value naming the changes job must be \${{ needs.changes.outputs.<filter> }} or \${{ needs.changes.result }}')"

# A value that names the changes job nowhere carries no filter, so this rule has nothing to
# resolve and leaves it alone; what refuses the fixture is the gate on that step, whose `if:`
# then tests no filter. That is the shape dispatch-browser.yaml's `lane` takes in the real
# tree, where no gate is declared on such a condition and the check passes over it.
echo "case: a called step's input the caller passes a literal for"
root=$(fixture called-input-literal)
called_input_gate "$root"
replace "$root/.github/workflows/ci.yaml" "      app: \${{ needs.changes.outputs.app }}" "      app: 'true'"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "with the gate's own refusal, not the with: value's" \
  "$(contains "$out" 'gate app names step "Test the library" of called, but no single step of that name has an if: on needs.changes.outputs.app')"
check "and says nothing about resolving the value" \
  "$(is "$(contains "$out" 'a value naming the changes job must be')" false)"

# A job of the called file gated on an input the caller builds from the event, with no gate
# declared on it. The suite runs behind the caller job's own `if:`, and the input only chooses
# what it runs, so this rule reads it as no gate and nothing else fires.
echo "case: a called job gated on an input the caller builds from the event"
root=$(fixture called-input-event)
called_input_gate "$root"
replace "$root/.github/workflows/called.yaml" "      ci-ok-changes:" \
  $'      mode:\n        type: string\n        required: false\n        default: ""\n      ci-ok-changes:'
replace "$root/.github/workflows/called.yaml" "  called:" \
  $'  mode-only:\n    if: inputs.mode == \'full\'\n    runs-on: ubuntu-24.04\n    steps:\n      - run: echo the full mode\n  called:'
replace "$root/.github/workflows/ci.yaml" "      app: \${{ needs.changes.outputs.app }}" \
  $'      app: ${{ needs.changes.outputs.app }}\n      mode: ${{ github.event_name == \'pull_request\' && \'pull-request\' || \'full\' }}'
run_check "$root"
check "passes" "$(is "$status" 0)"

# The shape ci.yaml's lint job's notices step takes once it moves into a called file: its
# condition is `needs.changes.result != 'success' || needs.changes.outputs.go_notices == 'true'`
# today, and the result term carries no filter, so the two together are the one gate.
echo "case: a called step gated on the changes result and an output is the one gate the output names"
root=$(fixture called-input-result)
called_input_gate "$root"
replace "$root/.github/workflows/called.yaml" "        if: inputs.app == 'true'" \
  "        if: inputs.changes-result != 'success' || inputs.app == 'true'"
replace "$root/.github/workflows/ci.yaml" "      app: \${{ needs.changes.outputs.app }}" \
  $'      changes-result: ${{ needs.changes.result }}\n      app: ${{ needs.changes.outputs.app }}'
run_check "$root"
check "passes" "$(is "$status" 0)"
check "reads the one gate on app" \
  "$(contains "$out" '^report: called step "Test the library": 6 files (filter app)$')"

# Rule 6: a generated filter keeps test files out with `!` patterns, which only
# some-with-excludes reads as an exclusion; under `some` a `!` pattern matches every file outside
# it, so the check refuses the quantifier rather than writing such a filter.
echo "case: under some a generated ! pattern would match every file outside it"
root=$(fixture some-negation)
replace "$root/.github/ci-gates/app.py" 'text="go test ./cmd/app ./lib"' 'text="go build ./cmd/app ./lib", mode="build"'
replace "$root/.github/workflows/ci.yaml" "run: go test ./cmd/app ./lib" "run: go build ./cmd/app ./lib"
run_check "$root"
check "passes under some-with-excludes" "$(is "$status" 0)"
replace "$root/.github/workflows/ci.yaml" "          predicate-quantifier: some-with-excludes" "          predicate-quantifier: some"
run_check "$root"
check "fails under some" "$(is "$status" 1)"
check "names the filter and its ! pattern" \
  "$(contains "$out" '::error file=.github/workflows/ci.yaml::filter app has !\*\*/\*_test.go, a `!` pattern under predicate-quantifier some')"

# Rule 9, the half that has a subject left: a pattern written by hand that matches no file.
echo "case: a gate read and an EXCLUDED pattern that match no file of the repository"
root=$(fixture dead-pattern)
replace "$root/.github/ci-gates/app.py" 'reads=[Read(".github/workflows/ci.yaml", "a job reads its own definition")]' \
  'reads=[Read(".github/workflows/ci.yaml", "a job reads its own definition"), Read("packages/contracts/fixtures/delivery-live/**", "lib_test.go reads the live fixtures")]'
run_check "$root"
check "fails on the gate's dead read" "$(is "$status" 1)"
check "names the read and the gate file it is in" \
  "$(contains "$out" "::error file=.github/ci-gates/app.py::gate app reads packages/contracts/fixtures/delivery-live/\*\*, which matches no file in the repository")"
root=$(fixture dead-pattern-excluded)
replace "$root/.github/ci-gates/image.py" '["docs/**"]' '["docs/**", "gone/**", "!fixtures/gone/**"]'
run_check "$root"
check "fails on an EXCLUDED filter's dead pattern" "$(is "$status" 1)"
check "names the pattern and the file" \
  "$(contains "$out" "::error file=.github/ci-gates/image.py::filter image has gone/\*\*, which matches no file in the repository")"
check "and passes over its ! pattern that matches nothing" "$(is "$(contains "$out" 'fixtures/gone')" false)"

# Two gates can name one filter - a job of ci.yaml and a step of the workflow another job calls
# - and a change the filter selects on starts both, so the filter is the union of what they read.
echo "case: a filter two gates name is computed from the union of what they read"
root=$(fixture union-of-gates)
called_input_gate "$root"
run_generate "$root"
check "the generate mode exits 0" "$(is "$status" 0)"
check "the filter holds the called step's gate file" \
  "$(contains "$(filter_of app)" '^.github/ci-gates/called.app.py$')"
check "and the app job's own reads" "$(contains "$(filter_of app)" '^mod/cmd/\*\*$')"

echo "case: a go command that cannot run fails rather than reading an empty closure"
root=$(fixture broken-workspace)
printf 'go 1.21\n\nuse ./missing\n' > "$root/go.work"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "says the closure is unknown" "$(contains "$out" 'so the closure of .* is unknown')"

echo "case: CI_OK_SELECTED_BY misses a job changes gates"
root=$(fixture ci-ok-missing)
replace "$root/.github/workflows/ci.yaml" "CI_OK_SELECTED_BY: '{\"app\": \"app\", \"image\": \"image\"}'" \
  "CI_OK_SELECTED_BY: '{\"image\": \"image\"}'"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job and the output to map it to" \
  "$(contains "$out" "CI_OK_SELECTED_BY does not map app, whose if: tests needs.changes.outputs.app, so ci-ok.sh does not know what selects it and fails its result; add \"app\": \"app\"")"

echo "case: CI_OK_SELECTED_BY maps a job to an output its if: does not test"
root=$(fixture ci-ok-wrong)
replace "$root/.github/workflows/ci.yaml" "CI_OK_SELECTED_BY: '{\"app\": \"app\", \"image\": \"image\"}'" \
  "CI_OK_SELECTED_BY: '{\"app\": \"image\", \"image\": \"image\"}'"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job, the wrong output and the right one" \
  "$(contains "$out" "CI_OK_SELECTED_BY maps app to image, but its if: tests needs.changes.outputs.app")"

echo "case: CI_OK_SELECTED_BY maps a job changes does not gate"
root=$(fixture ci-ok-extra)
replace "$root/.github/workflows/ci.yaml" "CI_OK_SELECTED_BY: '{\"app\": \"app\", \"image\": \"image\"}'" \
  "CI_OK_SELECTED_BY: '{\"app\": \"app\", \"image\": \"image\", \"gone\": \"gone\"}'"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names it" "$(contains "$out" "CI_OK_SELECTED_BY maps gone, which is no job whose if: tests an output of changes; drop it")"

echo "case: CI_OK_PULL_REQUEST_ONLY misses a job whose if: admits only pull_request"
root=$(fixture ci-ok-pull-request-only)
replace "$root/.github/workflows/ci.yaml" "CI_OK_PULL_REQUEST_ONLY: '[\"image\"]'" "CI_OK_PULL_REQUEST_ONLY: '[]'"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job" "$(contains "$out" "CI_OK_PULL_REQUEST_ONLY lacks image, whose if: admits only pull_request")"

echo "case: a job in none of ci-ok's three classes"
# The fixture's lint job runs on every change and no changes output gates it, so dropping it
# from CI_OK_ALWAYS_RUN leaves it in no class: ci-ok.sh would fail its result on every run, and
# the author cannot answer that by mapping it, since its if: tests no output of changes.
root=$(fixture ci-ok-classless)
replace "$root/.github/workflows/ci.yaml" "CI_OK_ALWAYS_RUN: '[\"changes\", \"lint\"]'" \
  "CI_OK_ALWAYS_RUN: '[\"changes\"]'"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job and the three remedies" \
  "$(contains "$out" "job lint is in none of ci-ok's three classes, so ci-ok.sh fails its result whatever the job did: map it in CI_OK_SELECTED_BY to the changes output its if: tests, name it in CI_OK_ALWAYS_RUN when it runs on every change and ci-ok must hold it to success, or name it in CI_OK_EXEMPT when it is outside ci-ok's accounting and out of job ci-ok's needs:")"

echo "case: a job in two of ci-ok's three classes"
root=$(fixture ci-ok-two-classes)
replace "$root/.github/workflows/ci.yaml" "CI_OK_ALWAYS_RUN: '[\"changes\", \"lint\"]'" \
  "CI_OK_ALWAYS_RUN: '[\"changes\", \"lint\", \"app\"]'"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job, the two classes and the three remedies" \
  "$(contains "$out" "job app is in 2 of ci-ok's three classes (CI_OK_SELECTED_BY, CI_OK_ALWAYS_RUN), which judge it differently, and it belongs in exactly one: map it in CI_OK_SELECTED_BY")"

echo "case: CI_OK_ALWAYS_RUN names a job an output of changes gates"
# A gated job named here would be held to success on every run that rightly skipped it, which is
# the opposite failure: ci-ok red for ever rather than green on nothing.
root=$(fixture ci-ok-always-run-gated)
replace "$root/.github/workflows/ci.yaml" "CI_OK_SELECTED_BY: '{\"app\": \"app\", \"image\": \"image\"}'" \
  "CI_OK_SELECTED_BY: '{\"image\": \"image\"}'"
replace "$root/.github/workflows/ci.yaml" "CI_OK_ALWAYS_RUN: '[\"changes\", \"lint\"]'" \
  "CI_OK_ALWAYS_RUN: '[\"changes\", \"lint\", \"app\"]'"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job and the output that gates it" \
  "$(contains "$out" "CI_OK_ALWAYS_RUN names app, whose if: tests needs.changes.outputs.app, so changes gates it rather than selecting every run of it, and ci-ok.sh would fail every run that rightly skipped it; map it in CI_OK_SELECTED_BY instead")"

echo "case: CI_OK_ALWAYS_RUN names a job the file does not have"
root=$(fixture ci-ok-always-run-gone)
replace "$root/.github/workflows/ci.yaml" "CI_OK_ALWAYS_RUN: '[\"changes\", \"lint\"]'" \
  "CI_OK_ALWAYS_RUN: '[\"changes\", \"lint\", \"gone\"]'"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names it" \
  "$(contains "$out" "CI_OK_ALWAYS_RUN names gone, which is no job of this file; drop it, or name the job it was meant for")"

echo "case: a job whose last step is not the ci-ok report"
root=$(fixture ci-ok-unreported)
replace "$root/.github/workflows/ci.yaml" "        if: always()" "        if: success()"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job and what its last step must be" \
  "$(contains "$out" "job app's last step is not \`uses: ./.github/actions/ci-ok-report\` with \`if: always()\`")"

# cancel_job <root>: appends a merge-group-only job that exists to stop a run the queue has
# thrown away. It reports nothing to ci-ok, as a job of CI_OK_EXEMPT must not.
cancel_job() {
  cat >> "$1/.github/workflows/ci.yaml" <<'YAML'
  cancel:
    if: github.event_name == 'merge_group' && github.run_attempt == '1'
    runs-on: ubuntu-24.04
    permissions:
      actions: write
    steps:
      - name: Cancel the rest of the run
        run: gh run cancel "$GITHUB_RUN_ID"
YAML
}

echo "case: a job CI_OK_EXEMPT names passes without the ci-ok report step"
root=$(fixture ci-ok-exempt)
cancel_job "$root"
replace "$root/.github/workflows/ci.yaml" "CI_OK_EXEMPT: '[\"ci-ok\", \"pr-title\"]'" \
  "CI_OK_EXEMPT: '[\"ci-ok\", \"pr-title\", \"cancel\"]'"
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "says what it checked" "$(contains "$out" 'every ci.yaml filter is computed from what its job reads (1 gates)')"

echo "case: a job CI_OK_EXEMPT does not name still needs the ci-ok report step"
root=$(fixture ci-ok-exempt-absent)
cancel_job "$root"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job and what its last step must be" \
  "$(contains "$out" "job cancel's last step is not \`uses: ./.github/actions/ci-ok-report\` with \`if: always()\`")"

echo "case: a job with no term holding it back on a re-run attempt of a merge-group run"
root=$(fixture rerun-guard)
replace "$root/.github/workflows/ci.yaml" \
  "    if: needs.changes.outputs.app == 'true' && (github.event_name != 'merge_group' || github.run_attempt == '1')" \
  "    if: needs.changes.outputs.app == 'true'"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job and the term it lacks" \
  "$(contains "$out" "job app's if: has no \`(github.event_name != 'merge_group' || github.run_attempt == '1')\`")"
check "says why the changes job's own skip is not enough" \
  "$(contains "$out" 'gh run rerun --failed` does not re-run the `changes` that succeeded')"

echo "case: a merge-group-only job with no attempt-1 term"
root=$(fixture rerun-guard-cancel)
cancel_job "$root"
replace "$root/.github/workflows/ci.yaml" \
  "    if: github.event_name == 'merge_group' && github.run_attempt == '1'" \
  "    if: github.event_name == 'merge_group'"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job and the term it lacks" \
  "$(contains "$out" "job cancel runs on merge_group and its if: has no \`github.run_attempt == '1'\`")"

echo "case: a job named for a required check"
root=$(fixture ci-ok-job)
replace "$root/.github/workflows/ci.yaml" $'\n  image:\n' $'\n  image:\n    name: ci-ok\n'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job and the check" \
  "$(contains "$out" "job image is named ci-ok, a check main's ruleset requires")"

echo "case: a job named for the other required check"
root=$(fixture pr-title-job)
replace "$root/.github/workflows/ci.yaml" $'\n  image:\n' $'\n  image:\n    name: pr-title\n'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job and the check" \
  "$(contains "$out" "job image is named pr-title, a check main's ruleset requires, and is not the pr-title job")"

# The two names, the reconcile step's if: and the ref step's run: the fixture carries, as
# check_ci_ok holds them and prints them.
ciok_name="\${{ github.event_name == 'workflow_dispatch' && needs.changes.outputs.ref_is_main != 'true' && 'ci-ok (dispatch)' || 'ci-ok' }}"
prtitle_name="\${{ github.event_name == 'workflow_dispatch' && 'pr-title (dispatch)' || 'pr-title' }}"
reconcile_if="\${{ always() && (github.event_name != 'workflow_dispatch' || needs.changes.outputs.ref_is_main == 'true') && !(steps.verdict.outcome == 'failure' && github.event_name == 'merge_group' && github.run_attempt == '1') }}"
ref_run='if [ "$GITHUB_REF" = refs/heads/main ]; then echo is_main=true; else echo is_main=false; fi >> "$GITHUB_OUTPUT"'
# has <text> <string>: true/false for check, a fixed-string match: these strings hold [ and $,
# which contains would read as a regex.
has() { grep -Fq -- "$2" <<<"$1" && echo true || echo false; }

echo "case: a job named for a required check renamed away from the check"
root=$(fixture pr-title-renamed)
replace "$root/.github/workflows/ci.yaml" "    name: $prtitle_name" "    name: PR title"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the name it carries and the one it must carry" \
  "$(has "$out" "job pr-title carries \`name: PR title\`, and pr-title is a check main's ruleset requires: give it \`name: $prtitle_name\`")"

echo "case: a required check's job carries no name, so a dispatch of a branch would report it"
# The shape of ci.yaml before this rule: a workflow_dispatch run of a pull request's branch
# posted its own ci-ok, and its pr-title skipped, on the pull request's head, beside the pull
# request's own, where a reader by name takes the newer.
for job in ci-ok pr-title; do
  want=$ciok_name
  [ "$job" = ci-ok ] || want=$prtitle_name
  root=$(fixture "$job-unnamed")
  replace "$root/.github/workflows/ci.yaml" $'\n'"    name: $want"$'\n' $'\n'
  run_check "$root"
  check "$job: fails" "$(is "$status" 1)"
  check "$job: says it carries no name and names the one it must carry" \
    "$(has "$out" "job $job carries no \`name:\`, and $job is a check main's ruleset requires: give it \`name: $want\`")"
  check "$job: and says what a dispatch of a branch does" \
    "$(has "$out" "never sits beside the pull request's $job on the branch's head")"
done

echo "case: a required check's job named apart on every dispatch, main's among them"
# The bypass check's dispatch on main posts its verdict on main's tip under ci-ok, where a person
# reading that commit looks for it; only a dispatch of another ref reports under its own name.
root=$(fixture ci-ok-dispatch-everywhere)
every="\${{ github.event_name == 'workflow_dispatch' && 'ci-ok (dispatch)' || 'ci-ok' }}"
replace "$root/.github/workflows/ci.yaml" "    name: $ciok_name" "    name: $every"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the name it carries" "$(has "$out" "job ci-ok carries \`name: $every\`")"

echo "case: ci-ok named apart by an expression's comparison of the ref, which ignores case"
# GitHub's expression == and != ignore case, so github.ref != 'refs/heads/main' is false for the
# branches refs/heads/Main and refs/heads/MAIN, which git accepts, and github.ref_name != 'main'
# for those and for a tag named main: a dispatch of any of them reported ci-ok.
n=0
for form in "github.ref != 'refs/heads/main'" "github.ref_name != 'main'"; do
  n=$((n + 1))
  root=$(fixture "ci-ok-dispatch-expression-ref-$n")
  named="\${{ github.event_name == 'workflow_dispatch' && $form && 'ci-ok (dispatch)' || 'ci-ok' }}"
  replace "$root/.github/workflows/ci.yaml" "    name: $ciok_name" "    name: $named"
  run_check "$root"
  check "$form: fails" "$(is "$status" 1)"
  check "$form: names the form it carries and the one it must carry" \
    "$(has "$out" "job ci-ok carries \`name: $named\`, and ci-ok is a check main's ruleset requires: give it \`name: $ciok_name\`")"
done

echo "case: a job whose name expression can evaluate to a required check's name"
# A plain-name comparison passed `\${{ … && 'ci-ok' || 'changes' }}`: on a run where the term is
# true, that job posts the required check under its own result, and a skipped one posts a pass.
root=$(fixture ci-ok-in-expression)
replace "$root/.github/workflows/ci.yaml" $'\n  image:\n' \
  $'\n  image:\n    name: ${{ github.event_name == \'merge_group\' && \'ci-ok\' || \'image\' }}\n'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job, its name and the check it can evaluate to" \
  "$(contains "$out" "job image carries \`name: \${{ github.event_name == 'merge_group' && 'ci-ok' || 'image' }}\`, which evaluates to ci-ok on some run, a check main's ruleset requires, and is not the ci-ok job")"
root=$(fixture image-named-by-expression)
replace "$root/.github/workflows/ci.yaml" $'\n  image:\n' \
  $'\n  image:\n    name: ${{ github.event_name == \'merge_group\' && \'image (queue)\' || \'image\' }}\n'
run_check "$root"
check "a name expression that names no required check passes" "$(is "$status" 0)"

echo "case: the verdict job's reconcile step runs on a dispatch of another ref, or after the hold"
# On a dispatch of another ref every other ci-ok of the head is a pull request's or the queue's,
# and the step would set each posted one to that run's verdict; `if: always()` alone was the shape
# before, and the expression's comparison of github.ref read refs/heads/Main as main. The third
# shape runs the step on a merge group's first attempt whose verdict failed too, after the hold
# step, which must end the job so its check run concludes the moment the group is the front.
n=0
for given in "always()" "\${{ always() && !(github.event_name == 'workflow_dispatch' && github.ref != 'refs/heads/main') }}" \
  "\${{ always() && (github.event_name != 'workflow_dispatch' || needs.changes.outputs.ref_is_main == 'true') }}"; do
  n=$((n + 1))
  root=$(fixture "ci-ok-reconcile-if-$n")
  replace "$root/.github/workflows/ci.yaml" "        if: $reconcile_if" "        if: $given"
  run_check "$root"
  check "if: $given: fails" "$(is "$status" 1)"
  check "if: $given: names the step, the if: it carries and the one it must carry" \
    "$(has "$out" "job ci-ok's step \"Set every other ci-ok on this head to this job's conclusion\" carries \`if: $given\`: give it \`if: $reconcile_if\`")"
done

echo "case: the changes job without the output the name and the reconcile step read"
root=$(fixture ref-output-absent)
replace "$root/.github/workflows/ci.yaml" $'      ref_is_main: ${{ steps.ref.outputs.is_main }}\n' ""
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the output it must carry" \
  "$(has "$out" "the changes job has no output ref_is_main: give it \`ref_is_main: \${{ steps.ref.outputs.is_main }}\`")"

echo "case: the changes job's ref step changed from the exact shell test"
# A case-folding test reads refs/heads/Main as main again, a step held back by an if: writes
# nothing on the runs it skips, and with no step the output is empty on every run.
fold_run='if [ "${GITHUB_REF,,}" = refs/heads/main ]; then echo is_main=true; else echo is_main=false; fi >> "$GITHUB_OUTPUT"'
for change in fold if absent; do
  root=$(fixture "ref-step-$change")
  case $change in
    fold) replace "$root/.github/workflows/ci.yaml" "        run: $ref_run" "        run: $fold_run" ;;
    if) replace "$root/.github/workflows/ci.yaml" $'        id: ref\n' \
          $'        id: ref\n        if: github.event_name == \'workflow_dispatch\'\n' ;;
    absent) replace "$root/.github/workflows/ci.yaml" \
              $'      - name: Read whether this run\'s ref is exactly refs/heads/main\n        id: ref\n'"        run: $ref_run"$'\n' "" ;;
  esac
  run_check "$root"
  check "$change: fails" "$(is "$status" 1)"
  check "$change: names the step and the exact run: it must carry" \
    "$(has "$out" "with id ref, and must hold exactly one, named \"Read whether this run's ref is exactly refs/heads/main\", with no \`if:\` and \`run: $ref_run\`")"
done

echo "case: the ref step the check holds ci.yaml to reads main case-sensitively"
# The check holds ci.yaml's step to REF_STEP_RUN, the engine's own text, and this case runs that
# text: only exactly refs/heads/main is main, and Main, MAIN, a tag named main, a branch whose
# name starts with main and a pull request's merge ref are not.
held_run=$(cd "$script_dir" && python3 -c 'import ci_filter_coverage as c; print(c.REF_STEP_RUN)' 2>&1) ||
  held_run="the engine holds no REF_STEP_RUN"
check "is the fixture's step" "$(is "$held_run" "$ref_run")"
for pair in refs/heads/main=true refs/heads/Main=false refs/heads/MAIN=false refs/tags/main=false \
  refs/heads/main-next=false refs/pull/1/merge=false; do
  ref=${pair%=*}
  want=${pair#*=}
  : >"$work/ref-output"
  GITHUB_REF=$ref GITHUB_OUTPUT="$work/ref-output" bash -c "$held_run" 2>/dev/null || true
  check "$ref: writes is_main=$want" "$(is "$(cat "$work/ref-output")" "is_main=$want")"
done

echo "case: a job named for a required check holding the re-run term"
root=$(fixture pr-title-rerun-guard)
replace "$root/.github/workflows/ci.yaml" \
  "    if: github.event_name == 'pull_request' || github.event_name == 'merge_group'" \
  "    if: (github.event_name != 'merge_group' || github.run_attempt == '1')"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "says a skipped required check passes" \
  "$(contains "$out" "job pr-title is named for a check main's ruleset requires and its if: holds a term that skips it on a re-run attempt of a merge-group run")"

echo "case: the verdict job holding the re-run term"
root=$(fixture ci-ok-rerun-guard)
replace "$root/.github/workflows/ci.yaml" "    if: \${{ always() }}" \
  "    if: \${{ always() && (github.event_name != 'merge_group' || github.run_attempt == '1') }}"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the verdict and what a skip would hand the group" \
  "$(contains "$out" "job ci-ok is named for a check main's ruleset requires and its if: holds a term that skips it on a re-run attempt of a merge-group run: a skipped job's check run is a passing required check, so that attempt would merge on ci-ok concluded by nothing")"

echo "case: a job CI_OK_EXEMPT does not name the pr-title job"
root=$(fixture pr-title-not-exempt)
replace "$root/.github/workflows/ci.yaml" "CI_OK_EXEMPT: '[\"ci-ok\", \"pr-title\"]'" "CI_OK_EXEMPT: '[\"ci-ok\"]'"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "asks it for the report step no exempt job carries" \
  "$(contains "$out" "job pr-title's last step is not \`uses: ./.github/actions/ci-ok-report\`")"
check "and asks the verdict to need it" \
  "$(contains "$out" "job ci-ok does not need pr-title, so it reaches the verdict while that job is still running and says nothing about it; add pr-title to its needs:")"

echo "case: the file has no pr-title job"
root=$(fixture pr-title-absent)
replace "$root/.github/workflows/ci.yaml" $'  pr-title:\n    name: ${{ github.event_name == \'workflow_dispatch\' && \'pr-title (dispatch)\' || \'pr-title\' }}\n    if: github.event_name == \'pull_request\' || github.event_name == \'merge_group\'\n    runs-on: ubuntu-24.04\n    steps:\n      - name: Check the pull request\'s text\n        run: echo text\n' ""
replace "$root/.github/workflows/ci.yaml" "CI_OK_EXEMPT: '[\"ci-ok\", \"pr-title\"]'" "CI_OK_EXEMPT: '[\"ci-ok\"]'"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the check nothing would carry" \
  "$(contains "$out" "this file has no pr-title job, a check main's ruleset requires")"

echo "case: the ci-ok job does not need a job of the file"
root=$(fixture ci-ok-needs-missing)
replace "$root/.github/workflows/ci.yaml" "    needs: [changes, app, image, lint]" "    needs: [changes, app, lint]"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job it would judge nothing of" \
  "$(contains "$out" "job ci-ok does not need image, so it reaches the verdict while that job is still running and says nothing about it; add image to its needs:")"

echo "case: the ci-ok job needs a job CI_OK_EXEMPT names"
root=$(fixture ci-ok-needs-exempt)
cancel_job "$root"
replace "$root/.github/workflows/ci.yaml" "CI_OK_EXEMPT: '[\"ci-ok\", \"pr-title\"]'" \
  "CI_OK_EXEMPT: '[\"ci-ok\", \"pr-title\", \"cancel\"]'"
replace "$root/.github/workflows/ci.yaml" "    needs: [changes, app, image, lint]" "    needs: [changes, app, image, lint, cancel]"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the exempt job and why the verdict cannot read it" \
  "$(contains "$out" "job ci-ok needs cancel, but CI_OK_EXEMPT holds it outside ci-ok's accounting, and ci-ok.sh fails a result for a job CI_OK_SELECTED_BY does not map; drop it from its needs:")"

echo "case: CI_OK_EXEMPT does not name the ci-ok job"
root=$(fixture ci-ok-not-exempt)
replace "$root/.github/workflows/ci.yaml" "  CI_OK_EXEMPT: '[\"ci-ok\", \"pr-title\"]'" "  CI_OK_EXEMPT: '[\"pr-title\"]'"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "says the verdict reports nothing to itself" \
  "$(contains "$out" "CI_OK_EXEMPT does not name ci-ok, which reports no verdict to itself")"
check "and asks it for the report step no exempt job carries" \
  "$(contains "$out" "job ci-ok's last step is not \`uses: ./.github/actions/ci-ok-report\`")"

echo "case: the file has no ci-ok job"
# The job is the last of the fixture, and `replace` reads the file without its trailing newline,
# so the block to cut starts at the newline before it and ends at its last line.
root=$(fixture ci-ok-absent)
replace "$root/.github/workflows/ci.yaml" $'\n  ci-ok:\n    name: ${{ github.event_name == \'workflow_dispatch\' && needs.changes.outputs.ref_is_main != \'true\' && \'ci-ok (dispatch)\' || \'ci-ok\' }}\n    needs: [changes, app, image, lint]\n    if: ${{ always() }}\n    runs-on: ubuntu-24.04\n    steps:\n      - name: Judge every job of this run\n        run: echo verdict\n      - name: Set every other ci-ok on this head to this job\'s conclusion\n        if: ${{ always() && (github.event_name != \'workflow_dispatch\' || needs.changes.outputs.ref_is_main == \'true\') && !(steps.verdict.outcome == \'failure\' && github.event_name == \'merge_group\' && github.run_attempt == \'1\') }}\n        run: echo reconcile' ""
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the check nothing would carry" \
  "$(contains "$out" "this file has no ci-ok job, the check main's ruleset requires")"

echo "case: a push workflow's paths miss a pattern of its filter"
root=$(fixture push-paths-missing)
replace "$root/.github/workflows/site.yaml" "      - 'mod/go.mod'"$'\n' ""
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the workflow, the pattern and the filter" \
  "$(contains "$out" "::error file=.github/workflows/site.yaml::.github/workflows/site.yaml's on.push.paths lacks mod/go.mod, which the generated filter site has")"
check "reports the lists differ" "$(contains "$out" '^report: .github/workflows/site.yaml: on.push.paths differs from filter site')"

echo "case: a push workflow's paths name a pattern its filter lacks"
root=$(fixture push-paths-extra)
replace "$root/.github/workflows/site.yaml" "      - 'docs/**'" $'      - \'docs/**\'\n      - \'fixtures/**\''
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the pattern" "$(contains "$out" "site.yaml's on.push.paths has fixtures/\*\*, which the generated filter site lacks")"
run_check "$root" write
check "the write mode writes the paths back" "$(is "$status" 0)"
check "as the filter's patterns" "$(is "$(contains "$(<"$root/.github/workflows/site.yaml")" 'fixtures')" false)"

echo "case: a push workflow PUSH_WORKFLOWS names is gone"
root=$(fixture push-workflow-gone)
rm "$root/.github/workflows/site.yaml"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names it" "$(contains "$out" 'PUSH_WORKFLOWS names .github/workflows/site.yaml for filter site, which is not in the repository')"

# A step that runs a package under a build tag and selects with -run runs only what the tag adds,
# so the job TAG_RUNS pairs it with must run that package's untagged tests in every run it runs:
# the generation puts every file the tag step reads into that job's filter (rule 15). The image
# job stands in for the tag step here (its filter is EXCLUDED, so no Run claims its command, and
# no read set is computed for it: what that filter selects stands for what its step reads), the
# app job for the one that runs the untagged tests.

# tag_run <root> <entry>: TAG_RUNS holds one entry, <entry> being its TagRun arguments.
tag_run() {
  replace "$1/.github/scripts/ci_job_reads.py" 'TAG_RUNS = []' "TAG_RUNS = [TagRun($2)]"
}

# tagged_step <root>: the image job's step runs its package under a tag and selects with -run.
tagged_step() {
  replace "$1/.github/workflows/ci.yaml" "        run: go build ./..." \
    "        run: go test -tags live -count=1 -run TestLive ./..."
}

entry='"image", "image", "Build", "app", "app", "the app job runs the package untagged"'

echo "case: the untagged job's filter takes every file the tag step reads"
root=$(fixture tag-run-covered)
tagged_step "$root"
run_generate "$root"
check "without the entry, the app filter leaves the tag step's file out" "$(is "$(contains "$(filter_of app)" '^docs')" false)"
tag_run "$root" "$entry"
run_check "$root"
check "passes" "$(is "$status" 0)"
check "reports the pairing" \
  "$(contains "$out" '^report: image step "Build": 1 files selected by filter image, 0 uncovered by app (filter app)$')"
run_generate "$root"
check "with the entry, the app filter takes the file filter image selects" "$(contains "$(filter_of app)" '^docs/\*\*$')"

# Rule 15 holds the untagged job's filter to every file the tag step reads, a tagged test file
# its own job never compiles among them, so the generation must write no `!` pattern keeping that
# filter off such a file. mod/other/live_test.go is that file here: the image job's tag step runs
# it, and the app job, whose filter rule 15 holds to it, compiles nothing of mod/other.
echo "case: a tagged test file rule 15 holds the untagged job's filter to"
root=$(fixture tag-run-held)
printf '//go:build live\n\npackage other\n\nimport "testing"\n\nfunc TestLive(t *testing.T) {}\n' \
  > "$root/mod/other/live_test.go"
replace "$root/.github/ci-gates/image.py" '["docs/**"]' '["docs/**", "mod/other/*"]'
tagged_step "$root"
tag_run "$root" "$entry"
run_generate "$root"
check "the generation passes" "$(is "$status" 0)"
check "and writes no ! pattern keeping the app filter off the tagged test" \
  "$(is "$(contains "$(filter_of app)" '^!mod/other')" false)"
check "which the app filter selects" "$(contains "$(filter_of app)" '^mod/\*\*$')"
run_check "$root"
check "the check passes" "$(is "$status" 0)"
check "with the tag step's files covered" \
  "$(contains "$out" '^report: image step "Build": 3 files selected by filter image, 0 uncovered by app (filter app)$')"

# The tag step's filter can hold a listed block: the contracts modules that the dispatch command
# the two-build Go step runs loads, in the real repository. Those files are no declared read of
# the step's gate, so rule 15 puts them in the untagged job's filter from the block itself. The
# fixture's tsc program is a `bunx` on PATH that lists docs/readme.md, which is how the listing
# reads a real one (`bunx tsc -p <project> --noEmit --listFilesOnly`).
# listed_tag_run <root>: a tagged job, gated on the filter tagged, whose step runs ./lib under the
# live tag with -run, pinned whole, whose filter's listed block lists docs/ from the program
# PROGRAMS pairs it with, and TAG_RUNS pairing the step with the app job.
listed_tag_run() {
  local root=$1
  replace "$root/.github/workflows/ci.yaml" '      image: ${{ steps.filter.outputs.image }}' \
    $'      image: ${{ steps.filter.outputs.image }}\n      tagged: ${{ steps.filter.outputs.tagged }}'
  replace "$root/.github/workflows/ci.yaml" '{"app": "app", "image": "image"}' \
    '{"app": "app", "image": "image", "tagged": "tagged"}'
  replace "$root/.github/workflows/ci.yaml" "    needs: [changes, app, image, lint]" \
    "    needs: [changes, app, image, lint, tagged]"
  cat >> "$root/.github/workflows/ci.yaml" <<'YAML'
  tagged:
    needs: changes
    if: needs.changes.outputs.tagged == 'true' && (github.event_name != 'merge_group' || github.run_attempt == '1')
    runs-on: ubuntu-24.04
    steps:
      - name: Test what the live tag adds
        run: go test -tags live -run TestLive ./lib
        working-directory: mod
      - name: Report ci-ok when this job decides a merge-group run
        if: always()
        uses: ./.github/actions/ci-ok-report
        with:
          changes: ${{ toJSON(needs.changes) }}
          job-status: ${{ job.status }}
YAML
  cat > "$root/.github/ci-gates/tagged.py" <<'PY'
from ci_filter_coverage import exact
GATE = gate("tagged", "tagged", exact=[
    exact("Test what the live tag adds", "mod", "go test -tags live -run TestLive ./lib", [
        run("mod", ["./lib"], at("tagged", "Test what the live tag adds") + " (its pinned script): go test -tags live ./lib",
            tags="live"),
    ]),
], reads=[Read(".github/workflows/ci.yaml", "a job reads its own definition")])
PY
  replace "$root/.github/scripts/ci_job_reads.py" 'TAG_RUNS = []' \
    'TAG_RUNS = [TagRun("tagged", "tagged", "Test what the live tag adds", "app", "app", "the app job runs ./lib untagged")]'
  printf '{}\n' > "$root/tsconfig.json"
  printf 'PROGRAMS = [("tagged", "tsconfig.json", "the tagged job typechecks it")]\nBUNDLES = []\nLISTED = [("tagged", "docs", ("tagged",), "the tagged job loads it")]\n' \
    > "$root/.github/scripts/ci_ts_programs.py"
  mkdir -p "$root/bin"
  printf '#!/bin/sh\necho docs/readme.md\n' > "$root/bin/bunx"
  chmod +x "$root/bin/bunx"
}

echo "case: the untagged job's filter takes the files of the tag step's listed block"
root=$(fixture tag-run-listed)
listed_tag_run "$root"
run_generate "$root" PATH="$root/bin:$PATH"
check "the generation passes" "$(is "$status" 0)"
check "the tagged filter holds the listed block" \
  "$(is "$(sed -n '/^tagged:$/,/^[a-z_]*:$/p' "$root.generated.yaml" | grep -A1 '# BEGIN listed' | tail -1)" "  - 'docs/readme.md'")"
check "and the app filter selects the block's file" "$(contains "$(filter_of app)" '^docs/\*\*$')"

echo "case: a TAG_RUNS entry naming a filter the changes job does not have"
root=$(fixture tag-run-no-filter)
tagged_step "$root"
tag_run "$root" '"image", "image", "Build", "gone", "app", "the app job runs the package untagged"'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the filter and the step" \
  "$(contains "$out" 'TAG_RUNS names filter gone for image step "Build", which the changes job does not have')"

echo "case: a TAG_RUNS entry naming a job ci.yaml does not have"
root=$(fixture tag-run-no-job)
tagged_step "$root"
tag_run "$root" '"image", "image", "Build", "app", "gone", "the gone job runs the package untagged"'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job" "$(contains "$out" 'TAG_RUNS names job gone, which .github/workflows/ci.yaml does not have')"

echo "case: a TAG_RUNS entry naming a step its job does not have"
root=$(fixture tag-run-no-step)
tagged_step "$root"
tag_run "$root" '"image", "image", "Gone", "app", "app", "the app job runs the package untagged"'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the step and the job" \
  "$(contains "$out" 'TAG_RUNS names step "Gone" of job image, which is not one run: step of it')"

echo "case: a TAG_RUNS entry whose step passes no -run"
root=$(fixture tag-run-no-run-flag)
tag_run "$root" "$entry"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the step" \
  "$(contains "$out" 'TAG_RUNS names image step "Build", which passes no -run, so it runs the package.s untagged tests itself')"

# A tag step that passes -run and that no TAG_RUNS entry names is the hole rule 15 exists to
# close, read from the other side: the four staleness refusals above all start from an entry, so
# a step added without one, or an entry deleted while its step stays, is held by nothing. Which
# job runs a package's untagged tests cannot be derived, but which steps need an entry can: a
# `go test` that passes both -tags and -run runs only what its tag adds.
echo "case: a tag step that passes -run and that TAG_RUNS does not name"
root=$(fixture tag-run-undeclared)
tagged_step "$root"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the step" "$(contains "$out" 'image step "Build"')"

# The other side of that walk: a `go test` passing -tags and no -run runs the package's untagged
# tests itself, there being no selection to lean on another job for, so no entry is wanted and
# the step needs none.
echo "case: a tag step that passes no -run and that TAG_RUNS does not name"
root=$(fixture tag-run-undeclared-no-run-flag)
replace "$root/.github/workflows/ci.yaml" "        run: go build ./..." \
  "        run: go test -tags live -count=1 ./..."
run_check "$root"
check "passes" "$(is "$status" 0)"

# A job that calls a reusable workflow keeps its gate in ci.yaml and gives its steps away, so a
# why cites the step in the file that holds it. ci.yaml's docs job does that with docs-site.yaml.
echo "case: a why cites a step of another workflow file"
root=$(fixture cited-step-elsewhere)
replace "$root/.github/ci-gates/app.py" \
  '], reads=[Read(".github/workflows/ci.yaml", "a job reads its own definition")])' \
  $'], reads=[Read(".github/workflows/ci.yaml", "a job reads its own definition"),\n          Read(".github/workflows/site.yaml", at("build", "echo build", ".github/workflows/site.yaml"))])'
run_check "$root"
check "passes while that file holds the cited step" "$(is "$status" 0)"
replace "$root/.github/workflows/site.yaml" "      - run: echo build" "      - run: echo publish"
run_check "$root"
check "fails once the step is gone" "$(is "$status" 1)"
check "names the step, its job and the file that does not have it" \
  "$(contains "$out" 'a why cites step "echo build" of job build, which .github/workflows/site.yaml does not have')"

# A job that calls a reusable workflow gives it its steps, so the go commands, the `env:` blocks
# and the shells that decide what that job compiles are in the called file. The check reads them
# there, exactly as it reads a job's own steps: ci.yaml's dispatch-browser job does this with
# dispatch-browser.yaml, whose multiprocess job builds the Dispatch server.

# calls_workflow <root>: a `suite` job that calls .github/workflows/suite.yaml, whose `rows` job
# builds ./cmd/app, with the output, the ci-ok map entry and the gate that go with it.
calls_workflow() {
  local root=$1
  replace "$root/.github/workflows/ci.yaml" '      image: ${{ steps.filter.outputs.image }}' \
    $'      image: ${{ steps.filter.outputs.image }}\n      suite: ${{ steps.filter.outputs.suite }}'
  replace "$root/.github/workflows/ci.yaml" '{"app": "app", "image": "image"}' \
    '{"app": "app", "image": "image", "suite": "suite"}'
  # The verdict job waits for every job of the file, and a job of ci.yaml carries the term that
  # holds it back on a re-run attempt of a merge-group run: rules 7 and 12 hold this job to both,
  # as they hold the real dispatch-browser job that calls dispatch-browser.yaml.
  replace "$root/.github/workflows/ci.yaml" "    needs: [changes, app, image, lint]" \
    "    needs: [changes, app, image, lint, suite]"
  cat >> "$root/.github/workflows/ci.yaml" <<'YAML'
  suite:
    needs: changes
    if: needs.changes.outputs.suite == 'true' && (github.event_name != 'merge_group' || github.run_attempt == '1')
    uses: ./.github/workflows/suite.yaml
    with:
      ci-ok-changes: ${{ toJSON(needs.changes) }}
YAML
  cat > "$root/.github/workflows/suite.yaml" <<'YAML'
name: Suite
on:
  workflow_call:
    inputs:
      ci-ok-changes:
        type: string
        default: ""
jobs:
  rows:
    runs-on: ubuntu-24.04
    steps:
      - name: Build the app
        run: go build ./cmd/app
        working-directory: mod
      - name: Report ci-ok when this job decides a merge-group run
        if: always()
        uses: ./.github/actions/ci-ok-report
        with:
          job: suite
          changes: ${{ inputs.ci-ok-changes }}
          job-status: ${{ job.status }}
YAML
  cat > "$root/.github/ci-gates/suite.py" <<'PY'
GATE = gate("suite", "suite", runs=[
    run("mod", ["./cmd/app"], at("rows", "Build the app", ".github/workflows/suite.yaml") + ": go build ./cmd/app",
        step="Build the app", text="go build ./cmd/app", mode="build"),
], reads=[Read(".github/workflows/ci.yaml", "a job reads its own definition"),
          Read(".github/workflows/suite.yaml", "the suite reads the definition of the steps it runs")])
PY
}

# A claimed command this check does not reduce counts as reading every Go file and every file
# under a Go module's directory, so the job's filter takes them all, and the report says why.
# widened <name>: whether the report line of gate <name> says its filter took every Go file.
widened() {
  contains "$out" "^report: $1: [0-9]* files (filter [a-z_]*); every Go file and every file under a Go module's directory, since "
}

echo "case: a gate whose job calls a workflow is read over that workflow's jobs"
root=$(fixture called-workflow-green)
calls_workflow "$root"
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts the called job's files" "$(contains "$out" '^report: suite: [0-9][0-9]* files (filter suite)$')"
run_generate "$root"
check "the suite filter holds the called file and its gate file" \
  "$(contains "$(filter_of suite | tr '\n' ' ')" '.github/ci-gates/suite.py .*.github/workflows/suite.yaml')"

echo "case: a package added to the called workflow's go command, outside the job's filter"
root=$(fixture called-workflow-package)
calls_workflow "$root"
replace "$root/.github/workflows/suite.yaml" "        run: go build ./cmd/app" "        run: go build ./cmd/app ./other"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job of the called file and the line" \
  "$(contains "$out" 'job rows of .github/workflows/suite.yaml, which job suite calls step "Build the app" runs `go build ./cmd/app ./other`')"
check "names the package the called job now also builds" "$(contains "$out" 'it also names ./other')"

echo "case: a go command of the called workflow that no Run claims"
root=$(fixture called-workflow-unclaimed)
calls_workflow "$root"
replace "$root/.github/workflows/suite.yaml" "      - name: Build the app" \
  $'      - name: Vet the library\n        run: go vet ./lib\n        working-directory: mod\n      - name: Build the app'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "asks for the Run" \
  "$(contains "$out" 'job rows of .github/workflows/suite.yaml, which job suite calls step "Vet the library" runs `go vet ./lib`, which no Run of a gate in .github/ci-gates lists')"

# The rule of .github/ci-notes/dispatch-browser.md#the-shard-jobs-shell-rule-its-measured-steps-and-its-budget
# for a step's `env:` rests on this: the called file's env: and shell: reach the gate's go commands,
# so one the check cannot read drops them to everything a go command can read.
echo "case: a name outside ENV_ALLOWED in the called workflow's step env"
root=$(fixture called-workflow-env)
calls_workflow "$root"
replace "$root/.github/workflows/suite.yaml" "        working-directory: mod" \
  $'        working-directory: mod\n        env:\n          PROBE_SETTING: on'
run_check "$root"
check "passes" "$(is "$status" 0)"
check "the report names the env it cannot read" "$(contains "$out" 'step "Build the app".s env sets PROBE_SETTING')"
check "and the filter takes every Go file" "$(widened suite)"
run_generate "$root"
check "so it selects one the job does not compile" "$(contains "$(filter_of suite)" '^mod/\*\*$')"

echo "case: a shell: on the called workflow's step"
root=$(fixture called-workflow-shell)
calls_workflow "$root"
replace "$root/.github/workflows/suite.yaml" "        working-directory: mod" \
  $'        working-directory: mod\n        shell: bash'
run_check "$root"
check "passes" "$(is "$status" 0)"
check "the report names the shell" "$(contains "$out" 'step "Build the app" runs under shell: `bash`')"
check "and the filter takes every Go file" "$(widened suite)"

# A `#` line is not a command. A step's comment may say that the job writes nothing to
# $GITHUB_ENV, and reading that as a write would be exactly backwards.
echo "case: a comment naming \$GITHUB_ENV in the called workflow's step"
root=$(fixture called-workflow-env-comment)
calls_workflow "$root"
replace "$root/.github/workflows/suite.yaml" "        run: go build ./cmd/app" \
  $'        run: |\n          # This job writes nothing to $GITHUB_ENV, for the reason its head gives.\n          go build ./cmd/app'
run_check "$root"
check "exits 0" "$(is "$status" 0)"

# A workflow two jobs of ci.yaml call holds a job named for each (ci-job-test.yaml holds test and
# tooling-checks), and the check reads each such job as its own caller's alone, holding its go
# commands, its env and its gated steps to that caller's filter. That reading holds only while the
# job runs under that caller alone: each caller has a job of the file named for it, that job's
# `if:` is exactly `inputs.job == '<its id>'`, and the caller passes `job: <its own id>`.
# check_called_jobs holds all three, and each case below up to the report steps' one breaks one.

# shared_file <root> <slow> <fast>: jobs <slow> and <fast> of ci.yaml both call
# .github/workflows/shared.yaml, as test and tooling-checks call ci-job-test.yaml, each passing its
# own id as `job`. The file holds a job of each id, whose `if:` admits that id alone and whose last
# step reports ci-ok under it. Both callers are gated on the app filter and no step runs a go
# command, so the split is all a case can break.
shared_file() {
  local root=$1 slow=$2 fast=$3 job
  {
    cat <<'YAML'
name: Shared
on:
  workflow_call:
    inputs:
      job:
        type: string
        required: true
      ci-ok-changes:
        type: string
        required: false
        default: ""
jobs:
YAML
    for job in "$slow" "$fast"; do
      cat <<YAML
  $job:
    if: inputs.job == '$job'
    runs-on: ubuntu-24.04
    steps:
      - name: Run the $job steps
        run: echo $job
      - name: Report ci-ok when this job decides a merge-group run
        if: always()
        uses: ./.github/actions/ci-ok-report
        with:
          job: $job
          changes: \${{ inputs.ci-ok-changes }}
          job-status: \${{ job.status }}
YAML
    done
  } > "$root/.github/workflows/shared.yaml"
  for job in "$slow" "$fast"; do
    cat >> "$root/.github/workflows/ci.yaml" <<YAML
  $job:
    needs: changes
    if: needs.changes.outputs.app == 'true' && (github.event_name != 'merge_group' || github.run_attempt == '1')
    uses: ./.github/workflows/shared.yaml
    with:
      job: $job
      ci-ok-changes: \${{ toJSON(needs.changes) }}
YAML
  done
  replace "$root/.github/workflows/ci.yaml" '{"app": "app", "image": "image"}' \
    "{\"app\": \"app\", \"image\": \"image\", \"$slow\": \"app\", \"$fast\": \"app\"}"
  replace "$root/.github/workflows/ci.yaml" "    needs: [changes, app, image, lint]" \
    "    needs: [changes, app, image, lint, $slow, $fast]"
}

echo "case: a workflow two jobs call, each running the job of it named for itself"
root=$(fixture shared-file-green)
shared_file "$root" test tooling-checks
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: a job of a shared workflow whose if: admits another caller"
root=$(fixture shared-file-other-if)
shared_file "$root" test tooling-checks
replace "$root/.github/workflows/shared.yaml" "    if: inputs.job == 'tooling-checks'" "    if: inputs.job == 'test'"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job, the if: it carries and the one it must carry" \
  "$(has "$out" "::error file=.github/workflows/shared.yaml::job tooling-checks of .github/workflows/shared.yaml, which jobs test, tooling-checks of .github/workflows/ci.yaml all call, carries \`if: inputs.job == 'test'\`: give it \`if: inputs.job == 'tooling-checks'\`")"

echo "case: a caller of a shared workflow that passes another caller's id"
root=$(fixture shared-file-other-id)
shared_file "$root" test tooling-checks
replace "$root/.github/workflows/ci.yaml" "      job: tooling-checks" "      job: test"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the caller, the id it passes and its own" \
  "$(has "$out" "::error file=.github/workflows/ci.yaml::job tooling-checks calls .github/workflows/shared.yaml, which other jobs of this file call too, and passes \`job: test\`: pass \`job: tooling-checks\`")"

echo "case: a caller of a shared workflow that passes no id"
root=$(fixture shared-file-no-id)
shared_file "$root" pi-legion pi-legion-build
replace "$root/.github/workflows/ci.yaml" $'      job: pi-legion-build\n' ""
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the caller and the id it must pass" \
  "$(has "$out" "::error file=.github/workflows/ci.yaml::job pi-legion-build calls .github/workflows/shared.yaml, which other jobs of this file call too, and passes \`job: \`: pass \`job: pi-legion-build\`")"

# A caller with no job of its own in the file runs none of the jobs named for the others, whose
# `if:` admits only their own ids, so no step of the file is held to its gate: the check names
# that caller rather than reading the file as if it ran something.
echo "case: a workflow two jobs call that holds a job named for one of them and none for the other"
root=$(fixture shared-file-none-named)
shared_file "$root" test tooling-checks
replace "$root/.github/workflows/shared.yaml" $'  tooling-checks:\n    if:' $'  tooling:\n    if:'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the caller with no job of its own there, and the job it must have" \
  "$(has "$out" "::error file=.github/workflows/ci.yaml::job tooling-checks calls .github/workflows/shared.yaml, which holds jobs named for its other callers (test) and none named tooling-checks, so no job of it runs under tooling-checks that this check can hold to tooling-checks's gate; give the file a job named tooling-checks, or call another file")"

# GitHub reads `${{ inputs.job == 'x' }}` as the bare comparison, and `&& true` adds nothing, yet
# the check proves the one form and refuses every other rather than reasoning about it: a term
# beside the comparison, such as an `||`, could admit another caller.
for written in "expression:\${{ inputs.job == 'tooling-checks' }}" "conjunction:inputs.job == 'tooling-checks' && true"; do
  echo "case: a job of a shared workflow whose if: is ${written#*:}, not the bare comparison"
  root=$(fixture "shared-file-if-${written%%:*}")
  shared_file "$root" test tooling-checks
  replace "$root/.github/workflows/shared.yaml" "    if: inputs.job == 'tooling-checks'" "    if: ${written#*:}"
  run_check "$root"
  check "fails" "$(is "$status" 1)"
  check "names the job, the if: it carries and the one it must carry" \
    "$(has "$out" "::error file=.github/workflows/shared.yaml::job tooling-checks of .github/workflows/shared.yaml, which jobs test, tooling-checks of .github/workflows/ci.yaml all call, carries \`if: ${written#*:}\`: give it \`if: inputs.job == 'tooling-checks'\`")"
done

# dispatch-browser.yaml's merge job reports under whichever of its two callers ran it by reading
# `job:` from the input, and a shared file's jobs may do the same, since each caller passes its
# own id there.
echo "case: a shared workflow's report steps that read the caller's id from the input"
root=$(fixture shared-file-report-input)
shared_file "$root" test tooling-checks
replace "$root/.github/workflows/shared.yaml" "          job: test" "          job: \${{ inputs.job }}"
replace "$root/.github/workflows/shared.yaml" "          job: tooling-checks" "          job: \${{ inputs.job }}"
run_check "$root"
check "passes while each caller passes its own id" "$(is "$status" 0)"
replace "$root/.github/workflows/ci.yaml" "      job: tooling-checks" "      job: test"
run_check "$root"
check "fails once a caller passes another's" "$(is "$status" 1)"
check "names the caller and the id it passes" \
  "$(has "$out" "::error file=.github/workflows/ci.yaml::job tooling-checks calls .github/workflows/shared.yaml, which other jobs of this file call too, and passes \`job: test\`: pass \`job: tooling-checks\`")"
check "and says no report goes out under the caller's own id" \
  "$(has "$out" "::error file=.github/workflows/ci.yaml::./.github/workflows/shared.yaml, which job tooling-checks calls, has no job whose last step is")"

# The fixture above puts both callers on one filter, so it cannot tell a job held to its own
# caller's filter from one held to the other's. split_callers <root> puts shared_file's two
# callers on two filters, as test and tooling-checks are in ci.yaml: the slow caller stays on app,
# and the fast one gets a gate of its own, fast. The fast caller's job of shared.yaml runs
# `go test ./cmd/app ./lib`, whose test reads fixtures/data.json (its //ci: line), and its gate
# pins that step there.
split_callers() {
  local root=$1
  replace "$root/.github/workflows/ci.yaml" '      image: ${{ steps.filter.outputs.image }}' \
    $'      image: ${{ steps.filter.outputs.image }}\n      fast: ${{ steps.filter.outputs.fast }}'
  replace "$root/.github/workflows/ci.yaml" $'  tooling-checks:\n    needs: changes\n    if: needs.changes.outputs.app ' \
    $'  tooling-checks:\n    needs: changes\n    if: needs.changes.outputs.fast '
  replace "$root/.github/workflows/ci.yaml" '"tooling-checks": "app"' '"tooling-checks": "fast"'
  replace "$root/.github/workflows/shared.yaml" "        run: echo tooling-checks" \
    $'        run: go test ./cmd/app ./lib\n        working-directory: mod'
  cat > "$root/.github/ci-gates/fast.py" <<'PY'
GATE = gate("fast", "tooling-checks", runs=[
    run("mod", ["./cmd/app", "./lib"],
        at("tooling-checks", "Run the tooling-checks steps", ".github/workflows/shared.yaml") + ": go test ./cmd/app ./lib",
        step="Run the tooling-checks steps", text="go test ./cmd/app ./lib"),
], reads=[Read(".github/workflows/shared.yaml", "the job reads the definition of the steps it runs")],
    workflow=".github/workflows/shared.yaml")
PY
}

echo "case: a job of a shared workflow is held to its own caller's filter, not the other caller's"
root=$(fixture shared-file-two-filters)
shared_file "$root" test tooling-checks
split_callers "$root"
run_check "$root"
check "passes" "$(is "$status" 0)"
check "and counts the fast job's step under the fast filter" \
  "$(contains "$out" '^report: tooling-checks: [0-9][0-9]* files (filter fast)$')"
run_generate "$root"
check "the fast filter takes the files the fast job's test reads" "$(contains "$(filter_of fast)" '^fixtures/\*\*$')"

# A step of a shared workflow gated on an input is read through the value the caller that runs
# its job passes. Both callers pass `tests` here, each its own filter's output, so a reading
# through the other caller's value would find the other filter.
echo "case: a step of a shared workflow gated on an input both callers pass, read through its own caller's value"
root=$(fixture shared-file-step-input)
shared_file "$root" test tooling-checks
split_callers "$root"
replace "$root/.github/workflows/shared.yaml" $'      ci-ok-changes:\n' \
  $'      tests:\n        type: string\n        required: false\n        default: ""\n      ci-ok-changes:\n'
replace "$root/.github/workflows/shared.yaml" $'      - name: Run the tooling-checks steps\n' \
  $'      - name: Run the tooling-checks steps\n        if: inputs.tests == \'true\'\n'
replace "$root/.github/workflows/ci.yaml" $'      job: test\n' $'      job: test\n      tests: ${{ needs.changes.outputs.app }}\n'
replace "$root/.github/workflows/ci.yaml" $'      job: tooling-checks\n' \
  $'      job: tooling-checks\n      tests: ${{ needs.changes.outputs.fast }}\n'
replace "$root/.github/ci-gates/fast.py" '    workflow=".github/workflows/shared.yaml")' \
  '    step="Run the tooling-checks steps", workflow=".github/workflows/shared.yaml")'
run_check "$root"
check "passes while the fast caller passes the fast output" "$(is "$status" 0)"
check "and reads the step as the gate on fast" \
  "$(contains "$out" '^report: tooling-checks step "Run the tooling-checks steps": [0-9][0-9]* files (filter fast)$')"
replace "$root/.github/workflows/ci.yaml" $'      job: tooling-checks\n      tests: ${{ needs.changes.outputs.fast }}' \
  $'      job: tooling-checks\n      tests: ${{ needs.changes.outputs.app }}'
replace "$root/.github/workflows/ci.yaml" $'      job: test\n      tests: ${{ needs.changes.outputs.app }}' \
  $'      job: test\n      tests: ${{ needs.changes.outputs.fast }}'
run_check "$root"
check "fails once only the slow caller passes the fast output" "$(is "$status" 1)"
check "names the gate and the step" \
  "$(has "$out" "::error file=.github/workflows/shared.yaml::gate fast names step \"Run the tooling-checks steps\" of tooling-checks, but no single step of that name has an if: on needs.changes.outputs.fast")"
check "and is that one problem" "$(contains "$out" '^::error::1 CI filter problem(s)$')"

echo "case: a package added to a go command the table claims, outside the job's filter"
root=$(fixture appended-package)
replace "$root/.github/workflows/ci.yaml" "run: go test ./cmd/app ./lib" "run: go test ./cmd/app ./lib ./other"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the package the job now also tests" "$(contains "$out" 'other')"

echo "case: a package added to a claimed go command by its import path, outside the job's filter"
root=$(fixture appended-import-path)
replace "$root/.github/workflows/ci.yaml" "run: go test ./cmd/app ./lib" "run: go test ./cmd/app ./lib example.com/m/other"
run_check "$root"
check "passes, the command widened" "$(is "$status" 0)"
check "names the package path it does not reduce" "$(contains "$out" "\`example.com/m/other\` is neither a relative package path nor a flag this check reduces")"

echo "case: a package added to a claimed go command on a continuation line, outside the job's filter"
root=$(fixture appended-continuation)
replace "$root/.github/workflows/ci.yaml" "        run: go test ./cmd/app ./lib" $'        run: |\n          go test ./cmd/app ./lib \\\n            ./other'
run_check "$root"
check "passes, the command widened" "$(is "$status" 0)"
check "and the filter takes every Go file" "$(widened app)"

echo "case: a -tags appended after a claimed go command's packages compiles a file outside the job's filter"
root=$(fixture appended-tags)
cat > "$root/mod/cmd/app/probe_test.go" <<'GO'
//go:build probe

package main

import _ "example.com/m/other"
GO
replace "$root/.github/workflows/ci.yaml" "run: go test ./cmd/app ./lib" "run: go test ./cmd/app ./lib -tags probe"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the tags the line builds with" "$(contains "$out" 'it builds with -tags probe, the Run with (none)')"
check "and names the command" \
  "$(contains "$out" 'job app step "Test the app" runs `go test ./cmd/app ./lib -tags probe`, which is not the Run')"

echo "case: a Run whose tags or mode disagree with the line it claims"
root=$(fixture run-disagrees)
replace "$root/.github/ci-gates/app.py" 'text="go test ./cmd/app ./lib"),' 'text="go test ./cmd/app ./lib", tags="probe", mode="build"),'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the tags" "$(contains "$out" 'it builds with -tags (none), the Run with probe')"
check "and the mode" "$(contains "$out" 'its go command is a test run, the Run a build run')"

echo "case: GOFLAGS set in front of a claimed go command compiles a file outside the job's filter"
root=$(fixture inline-goflags)
cat > "$root/mod/cmd/app/probe_test.go" <<'GO'
//go:build probe

package main

import _ "example.com/m/other"
GO
replace "$root/.github/workflows/ci.yaml" "run: go test ./cmd/app ./lib" "run: GOFLAGS=-tags=probe go test ./cmd/app ./lib"
run_check "$root"
check "passes" "$(is "$status" 0)"
check "the filter takes every Go file, the one the tag compiles in among them" "$(widened app)"
run_generate "$root"
check "and selects it" "$(contains "$(filter_of app)" '^mod/\*\*$')"

echo "case: GOFLAGS in a claimed go command's step env compiles a file outside the job's filter"
root=$(fixture env-goflags)
cat > "$root/mod/cmd/app/probe_test.go" <<'GO'
//go:build probe

package main

import _ "example.com/m/other"
GO
replace "$root/.github/workflows/ci.yaml" "        working-directory: mod" $'        working-directory: mod\n        env:\n          GOFLAGS: -tags=probe'
run_check "$root"
check "passes" "$(is "$status" 0)"
check "the filter takes every Go file, the one the tag compiles in among them" "$(widened app)"

echo "case: a go command this check does not reduce widens its filter to every module"
root=$(fixture unreduced-whole-module)
run_generate "$root"
check "the reduced command's filter leaves mod/other out" "$(is "$(contains "$(filter_of app)" '^mod/\*\*$')" false)"
replace "$root/.github/workflows/ci.yaml" "run: go test ./cmd/app ./lib" "run: GOFLAGS=-tags=probe go test ./cmd/app ./lib"
run_check "$root"
check "passes" "$(is "$status" 0)"
check "names the command and why it is not reduced" \
  "$(contains "$out" "^report: app: [0-9]* files (filter app); every Go file and every file under a Go module's directory, since job app step \"Test the app\" runs \`GOFLAGS=-tags=probe go test ./cmd/app ./lib\`, which this check does not reduce (it sets GOFLAGS, which can change what go compiles)")"
run_generate "$root"
check "and the filter takes the whole module" "$(contains "$(filter_of app)" '^mod/\*\*$')"

echo "case: a script's go command is held to its pinned text, the whole line"
root=$(fixture script-line)
printf '#!/bin/sh\ngo build ./cmd/app\n' > "$root/build.sh"
replace "$root/.github/ci-gates/app.py" 'text="go test ./cmd/app ./lib"),' \
  'text="go test ./cmd/app ./lib"), run("mod", ["./cmd/app"], "build.sh: go build ./cmd/app", mode="build", source="build.sh", text="go build ./cmd/app"),'
run_check "$root"
check "passes while the line is the text alone" "$(is "$status" 0)"
check "reducing the command" "$(is "$(widened app)" false)"
printf '#!/bin/sh\ngo build ./cmd/app ./other\n' > "$root/build.sh"
run_check "$root"
check "passes once the line holds more" "$(is "$status" 0)"
check "and names the script's command and its line" \
  "$(contains "$out" 'build.sh, which job app runs, runs `go build ./cmd/app`, which this check does not reduce (its line is `go build ./cmd/app ./other`, not the text alone)')"
check "and the filter takes every Go file" "$(widened app)"

# A script's go command is read under every step of each job that may run it. Which step starts
# the script is nothing this check can prove: a step can start it through another script, from
# another directory or by a test it compiles, and a step that only names its path (to chmod or
# lint it) starts nothing. So a write anywhere in such a job may reach the script's commands.
# script_job <root>: script-line's Run of build.sh, its line the text alone.
script_job() {
  printf '#!/bin/sh\ngo build ./cmd/app\n' > "$1/build.sh"
  replace "$1/.github/ci-gates/app.py" 'text="go test ./cmd/app ./lib"),' \
    'text="go test ./cmd/app ./lib"), run("mod", ["./cmd/app"], "build.sh: go build ./cmd/app", mode="build", source="build.sh", text="go build ./cmd/app"),'
}
path_write=$'      - name: Put a tool on the path of the steps after\n        run: echo "$RUNNER_TEMP/bin" >> "$GITHUB_PATH"\n'
build_step=$'      - name: Build the app\n        run: sh build.sh\n'
report_step="      - name: Report ci-ok when this job decides a merge-group run"
widened_by='build.sh, which job app runs, runs `go build ./cmd/app`, which this check does not reduce (step "Put a tool on the path of the steps after" runs `$GITHUB_PATH`, which every later step inherits)'

echo "case: a write before the step that runs a script widens the script's commands"
root=$(fixture script-write-before)
script_job "$root"
replace "$root/.github/workflows/ci.yaml" "$report_step" "$path_write$build_step$report_step"
run_check "$root"
check "passes" "$(is "$status" 0)"
check "the report names the script's command and the write" "$(contains "$out" "$widened_by")"

echo "case: a write after the step that runs a script widens the script's commands too"
root=$(fixture script-write-after)
script_job "$root"
replace "$root/.github/workflows/ci.yaml" "$report_step" "$build_step$path_write$report_step"
run_check "$root"
check "passes" "$(is "$status" 0)"
check "the report names the script's command and the write, since a later step may start the script again" \
  "$(contains "$out" "$widened_by")"

echo "case: a write anywhere in a job no step of which names the script widens the script's commands"
root=$(fixture script-write-unnamed)
script_job "$root"
replace "$root/.github/workflows/ci.yaml" "$report_step" "$path_write$report_step"
run_check "$root"
check "passes" "$(is "$status" 0)"
check "the report names the script's command and the write" "$(contains "$out" "$widened_by")"

# Jobs that start the script again after a write, in a form no step's text shows as a start of it:
# through a wrapper script that runs it, or by a cd into its directory and a start by its name
# there. The write comes after the job's plain start of the script, or after a step that only
# names its path. The check reads neither the wrapper nor the cd: it widens because it reads every
# step of the job, which is the only reading that does not miss such a start.
wrap_step=$'      - name: Build through the wrapper\n        run: sh wrap.sh\n'
chmod_step=$'      - name: Make the script executable\n        run: chmod +x build.sh\n'

echo "case: a write after the script's start, then a start through a wrapper script, widens the script's commands"
root=$(fixture script-start-wrapper)
script_job "$root"
printf '#!/bin/sh\nsh build.sh\n' > "$root/wrap.sh"
replace "$root/.github/workflows/ci.yaml" "$report_step" "$build_step$path_write$wrap_step$report_step"
run_check "$root"
check "the report names the script's command and the write" "$(contains "$out" "$widened_by")"

echo "case: a write after the script's start, then a start from the script's own directory, widens the script's commands"
root=$(fixture script-start-cd)
mkdir -p "$root/scripts"
printf '#!/bin/sh\ngo build ./cmd/app\n' > "$root/scripts/build.sh"
replace "$root/.github/ci-gates/app.py" 'text="go test ./cmd/app ./lib"),' \
  'text="go test ./cmd/app ./lib"), run("mod", ["./cmd/app"], "scripts/build.sh: go build ./cmd/app", mode="build", source="scripts/build.sh", text="go build ./cmd/app"),'
replace "$root/.github/workflows/ci.yaml" "$report_step" \
  $'      - name: Build the app\n        run: sh scripts/build.sh\n'"$path_write"$'      - name: Build the app from its directory\n        run: cd scripts && ./build.sh\n'"$report_step"
run_check "$root"
check "the report names the script's command and the write" \
  "$(contains "$out" 'scripts/build.sh, which job app runs, runs `go build ./cmd/app`, which this check does not reduce (step "Put a tool on the path of the steps after" runs `$GITHUB_PATH`, which every later step inherits)')"

echo "case: a write after a step that only names the script, then a start through a wrapper script, widens the script's commands"
root=$(fixture script-mention-wrapper)
script_job "$root"
printf '#!/bin/sh\nsh build.sh\n' > "$root/wrap.sh"
replace "$root/.github/workflows/ci.yaml" "$report_step" "$chmod_step$path_write$wrap_step$report_step"
run_check "$root"
check "the report names the script's command and the write" "$(contains "$out" "$widened_by")"

# The same job with a write that does change what go compiles: under -tags=probe the app imports
# mod/other. The check does not read the tag: the write widens the script's commands to every Go
# file, so the filter selects mod/other/other.go with the rest.
echo "case: a GOFLAGS write after a step that only names the script widens the job's filter to the file it compiles"
root=$(fixture script-mention-goflags)
script_job "$root"
printf '#!/bin/sh\nsh build.sh\n' > "$root/wrap.sh"
printf '//go:build probe\n\npackage main\n\nimport _ "example.com/m/other"\n' > "$root/mod/cmd/app/probe.go"
replace "$root/.github/workflows/ci.yaml" "$report_step" \
  "$chmod_step"$'      - name: Build every later step with the probe tag\n        run: echo "GOFLAGS=-tags=probe" >> "$GITHUB_ENV"\n'"$wrap_step$report_step"
run_check "$root"
check "passes" "$(is "$status" 0)"
check "the report names the write that widens the script's commands" \
  "$(contains "$out" 'build.sh, which job app runs, runs `go build ./cmd/app`, which this check does not reduce (step "Build every later step with the probe tag" runs `$GITHUB_ENV`, which every later step inherits)')"
run_generate "$root"
check "and the filter selects the file the tagged build compiles" "$(contains "$(filter_of app)" '^mod/\*\*$')"

echo "case: every job of the called workflow is read whole, one that runs nothing of the script's among them"
root=$(fixture called-script-other-job)
called_input_gate "$root"
printf '#!/bin/sh\ngo build ./cmd/app\n' > "$root/build.sh"
replace "$root/.github/ci-gates/called.app.py" 'text="go test ./lib"),' \
  'text="go test ./lib"), run("mod", ["./cmd/app"], "build.sh: go build ./cmd/app", mode="build", source="build.sh", text="go build ./cmd/app"),'
replace "$root/.github/workflows/called.yaml" "$report_step" "$build_step$report_step"
run_check "$root"
check "passes while no job of the file writes the path" "$(is "$status" 0)"
check "reducing the script's command" "$(is "$(contains "$out" 'which this check does not reduce')" false)"
replace "$root/.github/workflows/called.yaml" "  called:" \
  $'  other:\n    runs-on: ubuntu-24.04\n    steps:\n'"$path_write"'  called:'
run_check "$root"
check "and widens it once another job of the file writes the path" \
  "$(contains "$out" 'build.sh, which job called runs, runs `go build ./cmd/app`, which this check does not reduce (step "Put a tool on the path of the steps after" runs `$GITHUB_PATH`, which every later step inherits)')"

echo "case: a claimed simple go command whose step runs it in another directory"
root=$(fixture moved-working-directory)
mkdir -p "$root/alt/cmd/app" "$root/alt/lib"
printf 'package main\n\nfunc main() {}\n' > "$root/alt/cmd/app/main.go"
printf 'package lib\n' > "$root/alt/lib/lib.go"
replace "$root/.github/workflows/ci.yaml" "        working-directory: mod" "        working-directory: alt"
run_check "$root"
check "passes" "$(is "$status" 0)"
check "and widens the command, naming the directory the step runs in" \
  "$(contains "$out" 'which this check does not reduce (its step runs in alt, the Run in mod)')"

# `go -C <directory>` names the directory the command runs in on the line the check already
# reads, so it is held exactly as a step's working-directory is: joined with the step's own
# directory, the result must be the Run's.
echo "case: a claimed go -C command whose directory is the Run's"
root=$(fixture go-dash-c)
replace "$root/.github/workflows/ci.yaml" $'        run: go test ./cmd/app ./lib\n        working-directory: mod' \
  "        run: go -C mod test ./cmd/app ./lib"
replace "$root/.github/ci-gates/app.py" 'text="go test ./cmd/app ./lib"' 'text="go -C mod test ./cmd/app ./lib"'
run_check "$root"
check "passes" "$(is "$status" 0)"
check "counts the job's files" "$(contains "$out" '^report: app: 8 files (filter app)$')"
replace "$root/.github/workflows/ci.yaml" "run: go -C mod test ./cmd/app ./lib" "run: go -C alt test ./cmd/app ./lib"
replace "$root/.github/ci-gates/app.py" 'text="go -C mod test ./cmd/app ./lib"' 'text="go -C alt test ./cmd/app ./lib"'
run_check "$root"
check "widens the command once its -C names another directory" \
  "$(contains "$out" 'which this check does not reduce (its step runs in alt, the Run in mod)')"

# An assignment in front of the command is read the way a step's env: is: an ENV_ALLOWED name
# cannot change what go compiles, so the line stays reduced, and any other name falls to
# everything naming the name.
echo "case: a GOPROXY assignment in front of a claimed simple go command"
root=$(fixture goproxy-assignment)
replace "$root/.github/workflows/ci.yaml" "run: go test ./cmd/app ./lib" "run: GOPROXY=off go test ./cmd/app ./lib"
run_check "$root"
check "passes, the command reduced to the Run's closure" "$(is "$status" 0)"
check "counts the job's files" "$(contains "$out" '^report: app: 8 files (filter app)$')"

# GOCACHE names where go keeps what it compiled, keyed on the inputs, so a step that sets it in
# its env - as envoy-go-build's darwin vet does, for a build cache of its own - still compiles
# what the bare command compiles.
echo "case: a GOCACHE in a claimed simple go command's step env"
root=$(fixture gocache-env)
replace "$root/.github/workflows/ci.yaml" "        run: go test ./cmd/app ./lib" \
  $'        env:\n          GOCACHE: ${{ runner.temp }}/go-build-app\n        run: go test ./cmd/app ./lib'
run_check "$root"
check "passes, the command reduced to the Run's closure" "$(is "$status" 0)"
check "counts the job's files" "$(contains "$out" '^report: app: 8 files (filter app)$')"
check "and the step does carry the env" "$(contains "$(<"$root/.github/workflows/ci.yaml")" 'GOCACHE: \${{ runner.temp }}/go-build-app')"

echo "case: an assignment outside ENV_ALLOWED in front of a claimed simple go command"
root=$(fixture goos-assignment)
replace "$root/.github/workflows/ci.yaml" "run: go test ./cmd/app ./lib" "run: GOOS=linux go test ./cmd/app ./lib"
run_check "$root"
check "passes" "$(is "$status" 0)"
check "the report names the command, the name it sets and that the filter takes every Go file" \
  "$(contains "$out" "^report: app: [0-9]* files (filter app); every Go file and every file under a Go module's directory, since job app step \"Test the app\" runs \`GOOS=linux go test ./cmd/app ./lib\`, which this check does not reduce (it sets GOOS, which can change what go compiles)")"

echo "case: an earlier step writes GOFLAGS to GITHUB_ENV before a claimed simple go command"
root=$(fixture github-env-goflags)
cat > "$root/mod/cmd/app/probe_test.go" <<'GO'
//go:build probe

package main

import _ "example.com/m/other"
GO
replace "$root/.github/workflows/ci.yaml" "      - name: Test the app" $'      - name: Set the Go flags\n        run: echo "GOFLAGS=-tags=probe" >> "$GITHUB_ENV"\n      - name: Test the app'
run_check "$root"
check "passes" "$(is "$status" 0)"
check "the report names the GITHUB_ENV write" "$(contains "$out" 'which this check does not reduce (step "Set the Go flags" runs `$GITHUB_ENV`')"

# A write after the step reaches none of its go commands, so the command stays reduced.
echo "case: a later step writes GITHUB_PATH after a claimed simple go command"
root=$(fixture github-path-after)
replace "$root/.github/workflows/ci.yaml" "      - name: Report ci-ok when this job decides a merge-group run" \
  $'      - name: Put a tool on the path of the steps after\n        run: echo "$RUNNER_TEMP/bin" >> "$GITHUB_PATH"\n      - name: Report ci-ok when this job decides a merge-group run'
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "and still reduces the command to its closure" "$(contains "$out" '^report: app: 8 files (filter app)$')"

echo "case: go env -w sets GOFLAGS before a claimed simple go command"
root=$(fixture go-env-w-goflags)
cat > "$root/mod/cmd/app/probe_test.go" <<'GO'
//go:build probe

package main

import _ "example.com/m/other"
GO
replace "$root/.github/workflows/ci.yaml" "        run: go test ./cmd/app ./lib" $'        run: |\n          go env -w GOFLAGS=-tags=probe\n          go test ./cmd/app ./lib'
run_check "$root"
check "passes" "$(is "$status" 0)"
check "the report names the go env write" "$(contains "$out" 'which this check does not reduce (.*go env')"

echo "case: a claimed command that cds into another module of the workspace before it runs"
root=$(fixture cd-other-module)
mkdir -p "$root/mod2/cmd/app" "$root/mod2/lib"
printf 'module example.com/m2\n\ngo 1.21\n' > "$root/mod2/go.mod"
printf 'package main\n\nfunc main() {}\n' > "$root/mod2/cmd/app/main.go"
printf 'package lib\n' > "$root/mod2/lib/lib.go"
printf 'go 1.21\n\nuse (\n\t./mod\n\t./mod2\n)\n' > "$root/go.work"
run_check "$root"
check "passes while the command runs in its own module" "$(is "$status" 0)"
check "reducing it" "$(is "$(widened app)" false)"
replace "$root/.github/workflows/ci.yaml" "run: go test ./cmd/app ./lib" "run: cd ../mod2 && go test ./cmd/app ./lib"
run_check "$root"
check "widens it once it runs in the other module" "$(widened app)"
run_generate "$root"
check "so its filter selects that module's files" "$(contains "$(filter_of app)" '^mod2/\*\*$')"

echo "case: an unreduced go command reads every file under every Go module's directory, testdata included"
root=$(fixture unreduced-module-tree)
mkdir -p "$root/mod2/x/testdata"
printf 'module example.com/m2\n\ngo 1.21\n' > "$root/mod2/go.mod"
printf 'package x\n' > "$root/mod2/x/x.go"
echo 'data' > "$root/mod2/x/testdata/data.txt"
printf 'go 1.21\n\nuse (\n\t./mod\n\t./mod2\n)\n' > "$root/go.work"
replace "$root/.github/workflows/ci.yaml" "run: go test ./cmd/app ./lib" "run: GOFLAGS=-tags=probe go test ./cmd/app ./lib"
run_check "$root"
check "passes" "$(is "$status" 0)"
check "names the command that reads it" \
  "$(contains "$out" 'runs `GOFLAGS=-tags=probe go test ./cmd/app ./lib`, which this check does not reduce (it sets GOFLAGS, which can change what go compiles)')"
run_generate "$root"
check "and the filter selects the other module's testdata file" "$(contains "$(filter_of app)" '^mod2/\*\*$')"

echo "case: a cd on its own line before a simple go command in the same run"
root=$(fixture cd-own-line)
mkdir -p "$root/mod2/cmd/app" "$root/mod2/lib"
printf 'module example.com/m2\n\ngo 1.21\n' > "$root/mod2/go.mod"
printf 'package main\n\nfunc main() {}\n' > "$root/mod2/cmd/app/main.go"
printf 'package lib\n' > "$root/mod2/lib/lib.go"
printf 'go 1.21\n\nuse (\n\t./mod\n\t./mod2\n)\n' > "$root/go.work"
replace "$root/.github/workflows/ci.yaml" "        run: go test ./cmd/app ./lib" $'        run: |\n          cd ../mod2\n          go test ./cmd/app ./lib'
run_check "$root"
check "passes" "$(is "$status" 0)"
check "names the command and that it is not its step's whole script" \
  "$(contains "$out" 'job app step "Test the app" runs `go test ./cmd/app ./lib`, which this check does not reduce (it is not its step.s whole run: script, comments and blank lines aside)')"
run_generate "$root"
check "and the filter selects the module it cds into" "$(contains "$(filter_of app)" '^mod2/\*\*$')"

echo "case: an export of GOFLAGS on its own line before a simple go command in the same run"
root=$(fixture export-goflags)
cat > "$root/mod/cmd/app/probe_test.go" <<'GO'
//go:build probe

package main

import _ "example.com/m/other"
GO
replace "$root/.github/workflows/ci.yaml" "        run: go test ./cmd/app ./lib" $'        run: |\n          export GOFLAGS=-tags=probe\n          go test ./cmd/app ./lib'
run_check "$root"
check "passes" "$(is "$status" 0)"
check "names the command and that it is not its step's whole script" \
  "$(contains "$out" 'job app step "Test the app" runs `go test ./cmd/app ./lib`, which this check does not reduce (it is not its step.s whole run: script')"
check "and the filter takes every Go file" "$(widened app)"

echo "case: a step shell that sets GOFLAGS around a simple go command"
root=$(fixture step-shell)
cat > "$root/mod/cmd/app/probe_test.go" <<'GO'
//go:build probe

package main

import _ "example.com/m/other"
GO
replace "$root/.github/workflows/ci.yaml" "        working-directory: mod" $'        working-directory: mod\n        shell: env GOFLAGS=-tags=probe bash -e {0}'
run_check "$root"
check "passes" "$(is "$status" 0)"
check "names the command and the step's shell" \
  "$(contains "$out" 'job app step "Test the app" runs `go test ./cmd/app ./lib`, which this check does not reduce (step "Test the app" runs under shell: `env GOFLAGS=-tags=probe bash -e {0}`)')"
check "and the filter takes every Go file" "$(widened app)"

echo "case: a job defaults.run.shell around a simple go command"
root=$(fixture job-default-shell)
replace "$root/.github/workflows/ci.yaml" $'    runs-on: ubuntu-24.04\n    steps:\n      - name: Test the app' \
  $'    runs-on: ubuntu-24.04\n    defaults:\n      run:\n        shell: bash --noprofile --norc -eo pipefail {0}\n    steps:\n      - name: Test the app'
run_check "$root"
check "passes" "$(is "$status" 0)"
check "names the command and the job's default shell" \
  "$(contains "$out" 'job app step "Test the app" runs `go test ./cmd/app ./lib`, which this check does not reduce (the job.s defaults.run.shell is `bash --noprofile --norc -eo pipefail {0}`)')"
check "and the filter takes every Go file" "$(widened app)"

echo "case: a job container around a simple go command"
root=$(fixture job-container)
replace "$root/.github/workflows/ci.yaml" $'    runs-on: ubuntu-24.04\n    steps:\n      - name: Test the app' \
  $'    runs-on: ubuntu-24.04\n    container: golang:1.21\n    steps:\n      - name: Test the app'
run_check "$root"
check "passes" "$(is "$status" 0)"
check "names the command and the container" \
  "$(contains "$out" 'job app step "Test the app" runs `go test ./cmd/app ./lib`, which this check does not reduce (the job runs in a container:)')"
check "and the filter takes every Go file" "$(widened app)"

echo "case: a BASH_ENV in a simple go command's env"
root=$(fixture bash-env)
cat > "$root/mod/cmd/app/probe_test.go" <<'GO'
//go:build probe

package main

import _ "example.com/m/other"
GO
printf 'export GOFLAGS=-tags=probe\n' > "$root/mod/goflags.sh"
replace "$root/.github/workflows/ci.yaml" "        working-directory: mod" $'        working-directory: mod\n        env:\n          BASH_ENV: ./goflags.sh'
run_check "$root"
check "passes" "$(is "$status" 0)"
check "names the command and the BASH_ENV" \
  "$(contains "$out" 'job app step "Test the app" runs `go test ./cmd/app ./lib`, which this check does not reduce (step "Test the app".s env sets BASH_ENV)')"
check "and the filter takes every Go file" "$(widened app)"

echo "case: a -race go command's closure is the one go list -race lists"
root=$(fixture race-closure)
cat > "$root/mod/cmd/app/race_test.go" <<'GO'
//go:build race

package main

import _ "example.com/m/other"
GO
replace "$root/.github/workflows/ci.yaml" "run: go test ./cmd/app ./lib" "run: go test -race ./cmd/app ./lib"
replace "$root/.github/ci-gates/app.py" 'text="go test ./cmd/app ./lib"),' 'text="go test -race ./cmd/app ./lib", race=True),'
run_check "$root"
check "passes" "$(is "$status" 0)"
run_generate "$root"
check "and the filter selects the package the race build compiles in" "$(contains "$(filter_of app)" '^mod/\*\*$')"

echo "case: a -race go command claimed by a Run without race"
root=$(fixture race-mismatch)
replace "$root/.github/workflows/ci.yaml" "run: go test ./cmd/app ./lib" "run: go test -race ./cmd/app ./lib"
replace "$root/.github/ci-gates/app.py" 'text="go test ./cmd/app ./lib"),' 'text="go test -race ./cmd/app ./lib"),'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the command and the race mismatch" \
  "$(contains "$out" 'job app step "Test the app" runs `go test -race ./cmd/app ./lib`, which is not the Run.s (./cmd/app ./lib): it runs with -race, the Run without')"

# exact_step_table <root>: the app gate pins its step's whole script (a loop over the packages) and
# lists the Run that stands for what the loop compiles.
exact_step_table() {
  cat > "$1/.github/ci-gates/app.py" <<'PY'
from ci_filter_coverage import exact
GATE = gate("app", "app", exact=[
    exact("Test the app", "mod", 'for pkg in ./cmd/app ./lib; do\n  go test -count=1 "$pkg"\ndone', [
        run("mod", ["./cmd/app", "./lib"], at("app", "Test the app") + " (its pinned script): go test of each package of the loop"),
    ]),
], reads=[Read(".github/workflows/ci.yaml", "a job reads its own definition")])
PY
  replace "$1/.github/workflows/ci.yaml" "        run: go test ./cmd/app ./lib" \
    $'        run: |\n          for pkg in ./cmd/app ./lib; do\n            go test -count=1 "$pkg"\n          done'
}

echo "case: a step pinned whole by an exact entry is held to its runs' closure"
root=$(fixture exact-step)
exact_step_table "$root"
run_check "$root"
check "passes while the step holds the pinned script" "$(is "$status" 0)"
check "counts the runs' closure" "$(contains "$out" '^report: app: 8 files (filter app)$')"
printf 'package lib\n\nimport _ "example.com/m/other"\n\nfunc Hello() {}\n' > "$root/mod/lib/lib.go"
run_generate "$root"
check "and the filter takes a package the closure comes to read" "$(contains "$(filter_of app)" '^mod/\*\*$')"

# A step off its pin is refused rather than read as reading everything: the pin is also what the
# table says the step runs, and a step read wide passes unseen wherever its filter already selects
# every Go file.
echo "case: a step one character off its pinned script is refused, naming the line that differs"
root=$(fixture exact-step-changed)
exact_step_table "$root"
replace "$root/.github/workflows/ci.yaml" "-count=1" "-count=2"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the file, the step, the gate and both lines" \
  "$(contains "$out" '::error file=.github/workflows/ci.yaml::job app step "Test the app" is off the script and directory gate app.s exact entry pins it to: its line 2 reads `  go test -count=2 ".pkg"` where the pinned text.s reads `  go test -count=1 ".pkg"`; make the step run that text there again, or change the entry')"
check "and still reads the step as its runs, not as everything" \
  "$(is "$(contains "$out" '::error file=mod/other/other.go::')" false)"

echo "case: a step pinned whole that runs in another directory is refused, naming both"
root=$(fixture exact-step-moved)
exact_step_table "$root"
replace "$root/.github/workflows/ci.yaml" "        working-directory: mod" "        working-directory: mod/cmd"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the step and the two directories" \
  "$(contains "$out" 'job app step "Test the app" is off the script and directory gate app.s exact entry pins it to: it runs in mod/cmd, the entry.s in mod; ')"

# A no-break space where the pin has a space: bash reads `test<NBSP>-count=1` as one word, so the
# refusal names the character on the differing line, and names it once - the step is not refused
# a second time for the character its script holds.
echo "case: a step off its pin by a no-break space is refused once, naming the character"
root=$(fixture exact-step-nbsp)
exact_step_table "$root"
replace "$root/.github/workflows/ci.yaml" "go test -count=1" "go test"$'\xc2\xa0'"-count=1"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the line and the character" \
  "$(contains "$out" 'job app step "Test the app" is off the script and directory gate app.s exact entry pins it to: its line 2 reads `.*` where the pinned text.s reads `  go test -count=1 ".pkg"`, and holds the character U+00A0 there; ')"
check "and is that one problem" "$(contains "$out" '^::error::1 CI filter problem(s)$')"

echo "case: a step off its pin by a blank line names the blank line and the pinned line it stands for"
root=$(fixture exact-step-blank-line)
exact_step_table "$root"
replace "$root/.github/workflows/ci.yaml" $'; do\n            go test' $'; do\n\n            go test'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the blank line and the pinned line" \
  "$(contains "$out" 'job app step "Test the app" is off the script and directory gate app.s exact entry pins it to: its line 2 is a blank line where the pinned text.s reads `  go test -count=1 ".pkg"`; ')"
check "and quotes no empty line" "$(is "$(contains "$out" 'reads ``')" false)"

# What surrounds a pinned step can move what it compiles or which tests it runs while its text
# stays the pin's - a GOFLAGS -skip in its env skips a test its -run names - so such a step is
# refused too, naming what moves it, rather than read as reading everything. Each plant below is
# one such surrounding of the app job's pinned step.
for around in env shell container github-env; do
  case $around in
    env) what="an env naming GOFLAGS" ;;
    shell) what="a shell of its own" ;;
    container) what="a job container" ;;
    github-env) what="an earlier step's \$GITHUB_ENV write" ;;
  esac
  echo "case: a step pinned whole under $what is refused, naming what moves it"
  root=$(fixture "exact-step-$around")
  exact_step_table "$root"
  case $around in
    env)
      replace "$root/.github/workflows/ci.yaml" "        working-directory: mod" \
        $'        working-directory: mod\n        env:\n          GOFLAGS: -skip=^TestData$'
      why='step "Test the app".s env sets GOFLAGS' ;;
    shell)
      replace "$root/.github/workflows/ci.yaml" "        working-directory: mod" \
        $'        working-directory: mod\n        shell: sh'
      why='step "Test the app" runs under shell: `sh`' ;;
    container)
      replace "$root/.github/workflows/ci.yaml" \
        "    if: needs.changes.outputs.app == 'true' && (github.event_name != 'merge_group' || github.run_attempt == '1')" \
        "    if: needs.changes.outputs.app == 'true' && (github.event_name != 'merge_group' || github.run_attempt == '1')"$'\n    container: golang:1.21'
      why='the job runs in a container:' ;;
    github-env)
      replace "$root/.github/workflows/ci.yaml" "      - name: Test the app" \
        $'      - name: Skip a test in every later step\n        run: echo "GOFLAGS=-skip=^TestData$" >> "$GITHUB_ENV"\n      - name: Test the app'
      why='step "Skip a test in every later step" runs `$GITHUB_ENV`, which every later step inherits' ;;
  esac
  run_check "$root"
  check "fails" "$(is "$status" 1)"
  check "names the step, the gate and what moves it" \
    "$(contains "$out" "::error file=.github/workflows/ci.yaml::job app step \"Test the app\" is pinned whole by gate app.s exact entry, but $why, and that can change what its go commands compile, or which tests they run, where no pin holds it; ")"
  check "and is that one problem, reading nothing wide" "$(contains "$out" '^::error::1 CI filter problem(s)$')"
  check "and says how to keep a setting that moves nothing" "$(contains "$out" 'or, for a name with which go compiles the same files and runs the same tests, add it to ENV_ALLOWED$')"
done

# A write of $GITHUB_ENV or $GITHUB_PATH reaches only the steps after the one that makes it, so a
# write after the pinned step moves nothing it runs, while the same write before it does (the
# github-env case above).
echo "case: a step that writes GITHUB_PATH after a step pinned whole moves nothing it runs"
root=$(fixture exact-step-github-path-after)
exact_step_table "$root"
replace "$root/.github/workflows/ci.yaml" "      - name: Report ci-ok when this job decides a merge-group run" \
  $'      - name: Put a tool on the path of the steps after\n        run: echo "$RUNNER_TEMP/bin" >> "$GITHUB_PATH"\n      - name: Report ci-ok when this job decides a merge-group run'
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "and reads the step as its runs" "$(contains "$out" '^report: app: 8 files (filter app)$')"

# A pinned step's own script is the pin's to hold: a write on its last line reaches only the steps
# after it, so it refuses none of the step's own commands, while a pinned step after it is
# refused for it.
echo "case: a pinned step whose own script ends with an env write is not refused for it"
root=$(fixture exact-step-own-write)
exact_step_table "$root"
replace "$root/.github/ci-gates/app.py" "\\ndone', [" "\\ndone\\necho \"X=1\" >> \"\$GITHUB_ENV\"', ["
replace "$root/.github/workflows/ci.yaml" $'            go test -count=1 "$pkg"\n          done' \
  $'            go test -count=1 "$pkg"\n          done\n          echo "X=1" >> "$GITHUB_ENV"'
run_check "$root"
check "exits 0" "$(is "$status" 0)"
cat >> "$root/.github/ci-gates/app.py" <<'PY'
GATE = gate("app", "app", exact=GATE.exact + (
    exact("Test the library again", "mod", 'for pkg in ./lib; do\n  go test "$pkg"\ndone', [
        run("mod", ["./lib"], at("app", "Test the library again") + " (its pinned script): go test of the library"),
    ]),
), reads=[Read(".github/workflows/ci.yaml", "a job reads its own definition")])
PY
replace "$root/.github/workflows/ci.yaml" "      - name: Report ci-ok when this job decides a merge-group run" \
  $'      - name: Test the library again\n        run: |\n          for pkg in ./lib; do\n            go test "$pkg"\n          done\n        working-directory: mod\n      - name: Report ci-ok when this job decides a merge-group run'
run_check "$root"
check "but a pinned step after it is refused for it" \
  "$(contains "$out" 'job app step "Test the library again" is pinned whole by gate app.s exact entry, but step "Test the app" runs `$GITHUB_ENV`, which every later step inherits, ')"
check "and is that one problem" "$(contains "$out" '^::error::1 CI filter problem(s)$')"

# Both at once: a step off its pin and under an env that moves it fails on each, so neither
# refusal hides the other - a check that asked about the env first, and compared the text only
# without one, would name the env alone.
echo "case: a step off its pin and under an env that moves it is refused for both"
root=$(fixture exact-step-changed-env)
exact_step_table "$root"
replace "$root/.github/workflows/ci.yaml" "-count=1" "-count=2"
replace "$root/.github/workflows/ci.yaml" "        working-directory: mod" \
  $'        working-directory: mod\n        env:\n          GOFLAGS: -tags=probe'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the line that differs" \
  "$(contains "$out" 'job app step "Test the app" is off the script and directory gate app.s exact entry pins it to: its line 2 reads `  go test -count=2 ".pkg"` where the pinned text.s reads `  go test -count=1 ".pkg"`; ')"
check "and the env" \
  "$(contains "$out" 'job app step "Test the app" is pinned whole by gate app.s exact entry, but step "Test the app".s env sets GOFLAGS, and that can change ')"
check "and is those two problems" "$(contains "$out" '^::error::2 CI filter problem(s)$')"

# guard_list_table <root>: the app job runs the library's two tests by name, its -run built in
# the gate file from one list as this repository's text-check job's guard step is built from
# TEXT_CHECK_TESTS, and pinned whole by an exact entry.
guard_list_table() {
  printf 'package lib\n\nimport "testing"\n\nfunc TestGuard(t *testing.T) {}\n' > "$1/mod/lib/guard_test.go"
  cat > "$1/.github/ci-gates/app.py" <<'PY'
from ci_filter_coverage import exact
GUARD_TESTS = ["TestData", "TestGuard"]
GUARD_SCRIPT = f"go test -count=1 -run '^({'|'.join(sorted(GUARD_TESTS))})$' ./lib"
GATE = gate("app", "app", exact=[
    exact("Test the app", "mod", GUARD_SCRIPT, [
        run("mod", ["./lib"], at("app", "Test the app") + " (its pinned script): go test -run of the library's tests"),
    ]),
], reads=[Read(".github/workflows/ci.yaml", "a job reads its own definition")])
PY
  replace "$1/.github/workflows/ci.yaml" "        run: go test ./cmd/app ./lib" \
    "        run: go test -count=1 -run '^(TestData|TestGuard)\$' ./lib"
}

echo "case: a step whose -run one list builds passes while it runs exactly that list"
root=$(fixture guard-list)
guard_list_table "$root"
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts the runs' closure" "$(contains "$out" '^report: app: [0-9]* files (filter app)$')"

echo "case: a test the list names and the step's -run drops is refused, naming the step and the test"
root=$(fixture guard-list-dropped)
guard_list_table "$root"
replace "$root/.github/workflows/ci.yaml" "|TestGuard)" ")"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the file, the step and the test the step lacks" \
  "$(contains "$out" '::error file=.github/workflows/ci.yaml::job app step "Test the app" is off the script and directory gate app.s exact entry pins it to: on its line 1, its -run leaves out TestGuard, which the pinned -run names; ')"
check "and refuses it once" "$(is "$(grep -c 'is off the script and directory' <<<"$out" || true)" 1)"

echo "case: a test the step's -run names and the list does not is refused, naming the test"
root=$(fixture guard-list-added-to-the-step)
guard_list_table "$root"
replace "$root/.github/workflows/ci.yaml" "|TestGuard)" "|TestGuard|TestOther)"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the step and the test the list lacks" \
  "$(contains "$out" 'job app step "Test the app" is off the script and directory gate app.s exact entry pins it to: on its line 1, its -run names TestOther, which the pinned -run does not; ')"

echo "case: a test added to the list and not to the step's -run is refused, naming the test"
root=$(fixture guard-list-added-to-the-list)
guard_list_table "$root"
printf '\nfunc TestNewGuard(t *testing.T) {}\n' >> "$root/mod/lib/guard_test.go"
replace "$root/.github/ci-gates/app.py" 'GUARD_TESTS = ["TestData", "TestGuard"]' \
  'GUARD_TESTS = ["TestData", "TestGuard", "TestNewGuard"]'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the step and the test the step lacks" \
  "$(contains "$out" 'job app step "Test the app" is off the script and directory gate app.s exact entry pins it to: on its line 1, its -run leaves out TestNewGuard, which the pinned -run names; ')"

echo "case: a -run that turns a name into a pattern is refused, quoting the line rather than naming tests"
root=$(fixture guard-list-pattern)
guard_list_table "$root"
replace "$root/.github/workflows/ci.yaml" "|TestGuard)" "|TestGuard.*)"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "quotes both lines around the difference" \
  "$(contains "$out" 'is off the script and directory gate app.s exact entry pins it to: its line 1 reads `[.][.][.]test -count=1 -run ..(TestData|TestGuard[.][*])[$]. ./lib` where the pinned text.s reads `[.][.][.]test -count=1 -run ..(TestData|TestGuard)[$]. ./lib`; ')"
check "and does not read the pattern as a test it leaves out" "$(is "$(contains "$out" 'leaves out TestGuard')" false)"

echo "case: a -skip list that drops a test says the step no longer skips it"
root=$(fixture guard-list-skip)
guard_list_table "$root"
replace "$root/.github/ci-gates/app.py" "-count=1 -run '" "-count=1 -skip '"
replace "$root/.github/workflows/ci.yaml" "-count=1 -run '" "-count=1 -skip '"
run_check "$root"
check "passes while the step skips exactly the list" "$(is "$status" 0)"
replace "$root/.github/workflows/ci.yaml" "|TestGuard)" ")"
run_check "$root"
check "fails once it skips one test fewer" "$(is "$status" 1)"
check "and names the test it no longer skips" \
  "$(contains "$out" 'is off the script and directory gate app.s exact entry pins it to: on its line 1, its -skip no longer skips TestGuard, which the pinned -skip skips; ')"

echo "case: a name list moved along its line, the rest reading the same without it, is quoted"
root=$(fixture guard-list-moved)
guard_list_table "$root"
replace "$root/.github/workflows/ci.yaml" "-count=1 -run '^(TestData|TestGuard)\$' ./lib" \
  "-count=1  ./lib-run '^(TestData|TestGuard)\$'"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "quotes both lines, as the lists alone cannot say what differs" \
  "$(contains "$out" 'pins it to: its line 1 reads `go test -count=1  ./lib-run ..(TestData|TestGuard)[$].` where the pinned text.s reads `go test -count=1 -run ..(TestData|TestGuard)[$]. ./lib`; ')"
check "and gives no empty reason" "$(is "$(contains "$out" 'on its line 1, ;')" false)"

# shared_pin <root>: over guard_list_table, a second gate, app_step, gates the app job's step by
# a filter of its own and pins it by the same exact entry, as this repository's two two-build
# gates both pin that job's two Go steps.
shared_pin() {
  cat > "$1/.github/ci-gates/app.app_step.py" <<'PY'
from ci_filter_coverage import exact
GUARD_SCRIPT = "go test -count=1 -run '^(TestData|TestGuard)$' ./lib"
GATE = gate("app_step", "app", step="Test the app", exact=[
    exact("Test the app", "mod", GUARD_SCRIPT, [
        run("mod", ["./lib"], at("app", "Test the app") + " (its pinned script): go test -run of the library's tests"),
    ]),
], reads=[Read(".github/workflows/ci.yaml", "a job reads its own definition")])
PY
  replace "$1/.github/workflows/ci.yaml" '      image: ${{ steps.filter.outputs.image }}' \
    $'      image: ${{ steps.filter.outputs.image }}\n      app_step: ${{ steps.filter.outputs.app_step }}'
  replace "$1/.github/workflows/ci.yaml" "      - name: Test the app" \
    $'      - name: Test the app\n        if: needs.changes.outputs.app_step == \'true\''
}

echo "case: a step two gates pin passes while it runs their text"
root=$(fixture shared-pin)
guard_list_table "$root"
shared_pin "$root"
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: a step two gates pin, off their pin, is refused once, naming both gates"
root=$(fixture shared-pin-dropped)
guard_list_table "$root"
shared_pin "$root"
replace "$root/.github/workflows/ci.yaml" "|TestGuard)" ")"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names both gates and the test" \
  "$(contains "$out" '::error file=.github/workflows/ci.yaml::job app step "Test the app" is off the script and directory the exact entries of gates app, app_step pin it to: on its line 1, its -run leaves out TestGuard, which the pinned -run names; make the step run that text there again, or change the entries, ')"
check "and refuses it once" "$(is "$(grep -c 'is off the script and directory' <<<"$out" || true)" 1)"

for odd in cr nbsp; do
  echo "case: a hidden command after a ${odd} that Python reads as a comment and bash runs"
  root=$(fixture "hidden-$odd")
  # YAML's own escapes, as a committer would write them: \r and \_ (no-break space).
  if [ "$odd" = cr ]; then char='\r' code=U+000D; else char='\_' code=U+00A0; fi
  replace "$root/.github/workflows/ci.yaml" "        run: go test ./cmd/app ./lib" \
    "        run: \"go test ./cmd/app ./lib\\n${char}# || go build ./other\""
  run_check "$root"
  check "fails, since the line it hides is a go command no Run claims" "$(is "$status" 1)"
  check "names the hidden command" "$(contains "$out" '# || go build ./other`, which no Run of a gate in .github/ci-gates lists')"
  check "names the character" "$(contains "$out" "its step.s script holds the character $code")"
  check "and the filter takes every Go file" "$(widened app)"
done

# HOME, XDG_CONFIG_HOME and PATH reach go's env file or binary; PROBE_SETTING stands for any other
# name outside ENV_ALLOWED.
for name in XDG_CONFIG_HOME HOME PATH PROBE_SETTING; do
  echo "case: $name, outside ENV_ALLOWED, in a simple go command's env"
  root=$(fixture "env-$name")
  cat > "$root/mod/cmd/app/probe_test.go" <<'GO'
//go:build probe

package main

import _ "example.com/m/other"
GO
  mkdir -p "$root/mod/goenv/go"
  printf 'GOFLAGS=-tags=probe\n' > "$root/mod/goenv/go/env"
  replace "$root/.github/workflows/ci.yaml" "        working-directory: mod" $'        working-directory: mod\n        env:\n          '"$name"': ./goenv'
  run_check "$root"
  check "passes" "$(is "$status" 0)"
  check "names the command and $name" \
    "$(contains "$out" "job app step \"Test the app\" runs \`go test ./cmd/app ./lib\`, which this check does not reduce (step \"Test the app\".s env sets $name)")"
  check "and the filter takes every Go file" "$(widened app)"
done

echo "case: a step of ci.yaml gated on a push, an event the workflow does not run on"
root=$(fixture push-gated-step)
replace "$root/.github/workflows/ci.yaml" "      - name: Test the app" \
  $'      - name: Test the app\n        if: github.event_name == \'push\''
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job, the step and the expression" \
  "$(contains "$out" "job app step \"Test the app\" has \`if: github.event_name == 'push'\`, which tests \`github.event_name == 'push'\`")"
check "says where such work belongs instead" \
  "$(contains "$out" 'belongs in .github/workflows/ci-caches.yaml')"

echo "case: a job of ci.yaml gated on main's ref"
root=$(fixture push-gated-job)
replace "$root/.github/workflows/ci.yaml" "    if: needs.changes.outputs.app == 'true' &&" \
  "    if: needs.changes.outputs.app == 'true' && github.ref != 'refs/heads/main' &&"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job and the comparison" \
  "$(contains "$out" "job app has \`if: needs.changes.outputs.app == 'true' && github.ref != 'refs/heads/main' && (github.event_name != 'merge_group' || github.run_attempt == '1')\`, which tests \`github.ref != 'refs/heads/main'\`")"

echo "case: github.ref_name, and github.ref outside such a comparison, are no push gate"
root=$(fixture push-gate-free)
replace "$root/.github/workflows/ci.yaml" "    if: needs.changes.outputs.app == 'true' &&" \
  "    if: needs.changes.outputs.app == 'true' && github.ref_name != 'main' && !startsWith(github.ref, 'refs/heads/gh-readonly-queue/') &&"
run_check "$root"
check "exits 0" "$(is "$status" 0)"

# cache_workflow <root>: the shape .github/workflows/ci-caches.yaml and ci.yaml have here - a fill
# job that looks the family's key up, builds only on a miss and saves the entry, and a restore of
# that same key and those same paths in the job that reads it. The fill's second build is the race
# one, which must compile exactly what DAEMON_RACE_PACKAGES names (mod/lib here), since it fills
# the cache a race step of ci.yaml restores.
cache_workflow() {
  cat > "$1/.github/workflows/ci-caches.yaml" <<'YAML'
name: CI caches
on:
  push:
    branches: [main]
jobs:
  fill:
    runs-on: ubuntu-24.04
    steps:
      - id: go-app
        uses: actions/cache/restore@55cc8345863c7cc4c66a329aec7e433d2d1c52a9 # v6.1.0
        with:
          path: |
            ~/go/pkg/mod
            ~/.cache/go-build
          key: go-app-${{ runner.os }}
          lookup-only: true
      - if: steps.go-app.outputs.cache-hit != 'true'
        run: go test -count=1 ./cmd/app ./lib
        working-directory: mod
      - if: steps.go-app.outputs.cache-hit != 'true'
        run: go test -c -o /dev/null -race ./lib/
        working-directory: mod
      - if: steps.go-app.outputs.cache-hit != 'true'
        uses: actions/cache/save@55cc8345863c7cc4c66a329aec7e433d2d1c52a9 # v6.1.0
        with:
          path: |
            ~/go/pkg/mod
            ~/.cache/go-build
          key: go-app-${{ runner.os }}
YAML
  replace "$1/.github/workflows/ci.yaml" "      - name: Test the app" \
    $'      - id: go-cache\n        uses: actions/cache/restore@55cc8345863c7cc4c66a329aec7e433d2d1c52a9 # v6.1.0\n        with:\n          path: |\n            ~/go/pkg/mod\n            ~/.cache/go-build\n          key: go-app-${{ runner.os }}\n          restore-keys: ${{ format(\'go-app-{0}-\', runner.os) }}\n      - name: Test the app'
}

echo "case: a family ci-caches.yaml saves under the key and paths ci.yaml restores"
root=$(fixture cache-round-trip)
cache_workflow "$root"
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: a restore key that drifted from the key ci-caches.yaml saves"
root=$(fixture cache-key-drift)
cache_workflow "$root"
replace "$root/.github/workflows/ci.yaml" 'key: go-app-${{ runner.os }}' 'key: go-app-${{ runner.os }}-v2'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the family and both keys" \
  "$(contains "$out" 'job fill saves family go-app under key .*, but job app of .github/workflows/ci.yaml restores it under .*-v2')"

echo "case: a restore path the save of that family does not name"
root=$(fixture cache-path-drift)
cache_workflow "$root"
replace "$root/.github/workflows/ci.yaml" $'            ~/.cache/go-build\n' $'            ~/.cache/go-build\n            ~/.cache/legion\n'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names both path lists" \
  "$(contains "$out" 'job fill saves family go-app from ~/go/pkg/mod, ~/.cache/go-build, but job app of .github/workflows/ci.yaml restores it into ~/go/pkg/mod, ~/.cache/go-build, ~/.cache/legion')"

echo "case: a family ci-caches.yaml fills that ci.yaml and the workflows it calls restore nowhere"
root=$(fixture cache-unrestored)
cache_workflow "$root"
replace "$root/.github/workflows/ci.yaml" 'key: go-app-${{ runner.os }}' 'key: go-other-${{ runner.os }}'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the family nothing restores" \
  "$(contains "$out" 'job fill saves cache family go-app, which no actions/cache/restore of .github/workflows/ci.yaml or of a workflow it calls reads')"

# release_restore <root> <key>: a workflow ci.yaml does not call - a release build, as
# release.yaml and release-envoy-infra.yaml are here - restoring the go-app entry ci-caches.yaml
# fills, under <key>. Its restore misses as silently as one of ci.yaml's when its key drifts, so
# the save is held to it too; only the existence check stays ci.yaml's.
release_restore() {
  cat > "$1/.github/workflows/release.yaml" <<YAML
name: Release
on:
  push:
    branches: [main]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/cache/restore@55cc8345863c7cc4c66a329aec7e433d2d1c52a9 # v6.1.0
        with:
          path: |
            ~/go/pkg/mod
            ~/.cache/go-build
          key: $2
          restore-keys: go-app-\${{ runner.os }}-
YAML
}

echo "case: a release workflow restoring a filled family under the key ci-caches.yaml saves"
root=$(fixture cache-release-same)
cache_workflow "$root"
release_restore "$root" 'go-app-${{ runner.os }}'
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: a release workflow's restore key that drifted from the key ci-caches.yaml saves"
root=$(fixture cache-release-drift)
cache_workflow "$root"
release_restore "$root" 'go-app-${{ runner.os }}-v2'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the release workflow's job and both keys" \
  "$(contains "$out" 'job fill saves family go-app under key .*, but job build of .github/workflows/release.yaml restores it under .*-v2')"

echo "case: a family only a release workflow restores"
root=$(fixture cache-release-only)
cache_workflow "$root"
release_restore "$root" 'go-app-${{ runner.os }}'
replace "$root/.github/workflows/ci.yaml" 'key: go-app-${{ runner.os }}' 'key: go-other-${{ runner.os }}'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "still names the family ci.yaml restores nowhere" \
  "$(contains "$out" 'job fill saves cache family go-app, which no actions/cache/restore of .github/workflows/ci.yaml or of a workflow it calls reads')"

echo "case: a lookup of ci-caches.yaml whose key drifted from the key its own save writes"
root=$(fixture cache-lookup-drift)
cache_workflow "$root"
replace "$root/.github/workflows/ci-caches.yaml" $'          key: go-app-${{ runner.os }}\n          lookup-only: true' $'          key: go-app-${{ runner.os }}-v2\n          lookup-only: true'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the lookup's job and both keys" \
  "$(contains "$out" 'job fill saves family go-app under key .*, but job fill of .github/workflows/ci-caches.yaml restores it under .*-v2')"

echo "case: a lookup of ci-caches.yaml that downloads the entry"
root=$(fixture cache-lookup-only)
cache_workflow "$root"
replace "$root/.github/workflows/ci-caches.yaml" "          lookup-only: true"$'\n' ""
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job and the family" \
  "$(contains "$out" "job fill's actions/cache/restore of family go-app is not \`lookup-only: true\`")"

echo "case: a lookup of ci-caches.yaml with restore-keys"
root=$(fixture cache-lookup-prefix)
cache_workflow "$root"
replace "$root/.github/workflows/ci-caches.yaml" "          lookup-only: true" $'          lookup-only: true\n          restore-keys: go-app-'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job and the family" \
  "$(contains "$out" "job fill's actions/cache/restore of family go-app has restore-keys")"

# refill_workflow <root>: cache_workflow's shape with the fill made again on every push, as
# ci-caches.yaml's Go families but go-envoy-infra are filled: the fill's restore
# downloads the family's newest entry (no lookup-only, and its key and a `-` as restore-keys), its
# builds and its save run on every push, and its save names the commit after the key ci.yaml
# restores.
refill_workflow() {
  local caches="$1/.github/workflows/ci-caches.yaml" gate=$'      - if: steps.go-app.outputs.cache-hit != \'true\'\n'
  cache_workflow "$1"
  replace "$caches" $'          key: go-app-${{ runner.os }}\n          lookup-only: true' \
    $'          key: go-app-${{ runner.os }}\n          restore-keys: go-app-${{ runner.os }}-'
  replace "$caches" "${gate}        run: go test -count" '      - run: go test -count'
  replace "$caches" "${gate}        run: go test -c" '      - run: go test -c'
  replace "$caches" "${gate}        uses: actions/cache/save" '      - uses: actions/cache/save'
  replace "$caches" \
    $'cache/save@55cc8345863c7cc4c66a329aec7e433d2d1c52a9 # v6.1.0\n        with:\n          path: |\n            ~/go/pkg/mod\n            ~/.cache/go-build\n          key: go-app-${{ runner.os }}' \
    $'cache/save@55cc8345863c7cc4c66a329aec7e433d2d1c52a9 # v6.1.0\n        with:\n          path: |\n            ~/go/pkg/mod\n            ~/.cache/go-build\n          key: go-app-${{ runner.os }}-${{ github.sha }}'
}

echo "case: a family ci-caches.yaml fills again on every push, under the key ci.yaml restores and the commit"
root=$(fixture cache-refill)
refill_workflow "$root"
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "and the save does name the commit" \
  "$(contains "$(<"$root/.github/workflows/ci-caches.yaml")" 'key: go-app-\${{ runner.os }}-\${{ github.sha }}')"

echo "case: a family saved under the commit whose ci-caches.yaml restore is a lookup"
root=$(fixture cache-refill-lookup)
refill_workflow "$root"
replace "$root/.github/workflows/ci-caches.yaml" '          restore-keys: go-app-${{ runner.os }}-' '          lookup-only: true'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job, the family and what the fill then builds" \
  "$(contains "$out" "job fill's actions/cache/restore of family go-app is \`lookup-only: true\`, but this workflow saves the family under \`-\\\${{ github.sha }}\`.* or each fill compiles everything from nothing")"

# An expression can evaluate to true, and the check does not evaluate it, so the fill's restore of
# a family saved under the commit may set no lookup-only at all.
echo "case: a family saved under the commit whose ci-caches.yaml restore is a lookup by expression"
root=$(fixture cache-refill-lookup-expression)
refill_workflow "$root"
replace "$root/.github/workflows/ci-caches.yaml" '          restore-keys: go-app-${{ runner.os }}-' $'          restore-keys: go-app-${{ runner.os }}-\n          lookup-only: ${{ true }}'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job, the family and the expression" \
  "$(contains "$out" "job fill's actions/cache/restore of family go-app is \`lookup-only: \\\${{ true }}\`, but this workflow saves the family under")"

echo "case: a family saved under the commit whose ci-caches.yaml restore takes a shorter prefix"
root=$(fixture cache-refill-prefix)
refill_workflow "$root"
replace "$root/.github/workflows/ci-caches.yaml" '          restore-keys: go-app-${{ runner.os }}-' '          restore-keys: go-app-'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job, the family, its restore-keys and the one it may take" \
  "$(contains "$out" "job fill's actions/cache/restore of family go-app has restore-keys go-app-, but a family this workflow saves under .* takes exactly \`go-app-\\\${{ runner.os }}-\`")"

echo "case: a family saved under the commit whose ci-caches.yaml restore has no restore-keys"
root=$(fixture cache-refill-no-prefix)
refill_workflow "$root"
replace "$root/.github/workflows/ci-caches.yaml" $'          restore-keys: go-app-${{ runner.os }}-\n' ''
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job, the family and the restore-keys it must take" \
  "$(contains "$out" "job fill's actions/cache/restore of family go-app has restore-keys (none), but a family this workflow saves under .* takes exactly \`go-app-\\\${{ runner.os }}-\`")"

# The entry under the restores' own key wins every restore as an exact match, so a family saved
# both under the commit and without it fills nobody with its commit entries.
echo "case: a family ci-caches.yaml saves both under the commit and without it"
root=$(fixture cache-refill-both)
refill_workflow "$root"
cat >> "$root/.github/workflows/ci-caches.yaml" <<'YAML'
      - uses: actions/cache/save@55cc8345863c7cc4c66a329aec7e433d2d1c52a9 # v6.1.0
        with:
          path: |
            ~/go/pkg/mod
            ~/.cache/go-build
          key: go-app-${{ runner.os }}
YAML
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the family and what the bare entry does" \
  "$(contains "$out" "this workflow saves family go-app both under \`-\\\${{ github.sha }}\` and without it: the entry under the restores' own key wins every restore")"

echo "case: a save of ci-caches.yaml under the key ci.yaml restores and another suffix"
root=$(fixture cache-refill-other-suffix)
refill_workflow "$root"
replace "$root/.github/workflows/ci-caches.yaml" 'key: go-app-${{ runner.os }}-${{ github.sha }}' 'key: go-app-${{ runner.os }}-${{ github.run_id }}'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the family and both keys" \
  "$(contains "$out" 'job fill saves family go-app under key .*github.run_id.*, but job app of .github/workflows/ci.yaml restores it under ')"
check "and says which keys a save may carry, and what another suffix does" \
  "$(contains "$out" "a save's key must be the restore's, or the restore's followed by .*; that restore takes it only by prefix, .* so under any other suffix each fill's entry piles up beside the last")"

# save_key <root> <key>: ci-caches.yaml's lookup fill saving under <key> instead of the key its
# lookup names.
save_key() {
  replace "$1/.github/workflows/ci-caches.yaml" \
    $'cache/save@55cc8345863c7cc4c66a329aec7e433d2d1c52a9 # v6.1.0\n        with:\n          path: |\n            ~/go/pkg/mod\n            ~/.cache/go-build\n          key: go-app-${{ runner.os }}' \
    $'cache/save@55cc8345863c7cc4c66a329aec7e433d2d1c52a9 # v6.1.0\n        with:\n          path: |\n            ~/go/pkg/mod\n            ~/.cache/go-build\n          key: '"$2"
}

# A save under the key its lookup computed lands where the lookup and every reader look: the
# rule reads that restore step's key text in its place.
echo "case: a lookup fill of ci-caches.yaml that saves under the key its lookup computed"
root=$(fixture cache-computed-key)
cache_workflow "$root"
save_key "$root" '${{ steps.go-app.outputs.cache-primary-key }}'
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "and the save does name the lookup's output" \
  "$(contains "$(<"$root/.github/workflows/ci-caches.yaml")" 'key: \${{ steps.go-app.outputs.cache-primary-key }}')"

echo "case: a save of ci-caches.yaml under the computed key of a step that is no restore of its job"
root=$(fixture cache-computed-key-unknown)
cache_workflow "$root"
save_key "$root" '${{ steps.go-other.outputs.cache-primary-key }}'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job and the step id it cannot find" \
  "$(contains "$out" "job fill saves under .*, but no actions/cache/restore of that job has the id go-other")"

# A save evaluates its own hashFiles() after the build, which can change a file it hashes:
# go-envoy-infra's save did, and every push missed its lookup (run 45088325195).
echo "case: a save of ci-caches.yaml whose own key holds hashFiles()"
root=$(fixture cache-save-hashfiles)
cache_workflow "$root"
save_key "$root" "go-app-\${{ runner.os }}-\${{ hashFiles('mod/go.mod') }}"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job, the key and what to save under instead" \
  "$(contains "$out" "job fill saves under .*hashFiles('mod/go.mod').*, which holds hashFiles(): the save evaluates it after the build.*save under the key the lookup computed")"

# A job of ci.yaml that calls a local workflow gives it its steps, cache steps among them: the
# real dispatch-browser job restores go-envoy and go-envoy-test in the binaries, multiprocess,
# embedder and visual jobs of dispatch-browser.yaml. So the rule reads those jobs' restores as it
# reads ci.yaml's own, and names the file that holds them. Read over ci.yaml alone, the fill below
# would be a family nothing restores.
#
# suite_cache <root>: a second family, go-suite, that ci-caches.yaml's fill job saves and only
# the called workflow's `rows` job restores. Call it after cache_workflow and calls_workflow.
suite_cache() {
  cat >> "$1/.github/workflows/ci-caches.yaml" <<'YAML'
      - if: steps.go-app.outputs.cache-hit != 'true'
        uses: actions/cache/save@55cc8345863c7cc4c66a329aec7e433d2d1c52a9 # v6.1.0
        with:
          path: |
            ~/go/pkg/mod
            ~/.cache/go-build
          key: go-suite-${{ runner.os }}
YAML
  replace "$1/.github/workflows/suite.yaml" "      - name: Build the app" \
    $'      - id: go-suite-cache\n        uses: actions/cache/restore@55cc8345863c7cc4c66a329aec7e433d2d1c52a9 # v6.1.0\n        with:\n          path: |\n            ~/go/pkg/mod\n            ~/.cache/go-build\n          key: go-suite-${{ runner.os }}\n      - name: Build the app'
}

echo "case: a family only a called workflow's job restores"
root=$(fixture cache-called-workflow)
cache_workflow "$root"
calls_workflow "$root"
suite_cache "$root"
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: a called workflow's restore key that drifted from the save's"
root=$(fixture cache-called-workflow-drift)
cache_workflow "$root"
calls_workflow "$root"
suite_cache "$root"
replace "$root/.github/workflows/suite.yaml" 'key: go-suite-${{ runner.os }}' 'key: go-suite-${{ runner.os }}-v2'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the called workflow's job and both keys" \
  "$(contains "$out" 'job fill saves family go-suite under key .*, but job rows of .github/workflows/suite.yaml restores it under .*-v2')"

# ci-caches.yaml's race build fills the cache ci.yaml's race step restores, so the packages it
# compiles are DAEMON_RACE_PACKAGES, each spelled as a go command spells it. A package on one
# side alone fails nothing by itself - the fill holds a race binary no job restores, or the step
# links one from nothing on every run - which is what these cases hold it to.
echo "case: a race build naming a package the lane does not"
root=$(fixture race-fill-extra)
cache_workflow "$root"
replace "$root/.github/workflows/ci-caches.yaml" "-race ./lib/" "-race ./lib/ ./other/"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the package, the job and both lists" \
  "$(contains "$out" "::error file=.github/workflows/ci-caches.yaml::job fill's race build names ./other/, which .github/scripts/ci_job_reads.py's DAEMON_RACE_PACKAGES does not (./lib/, ./other/ against ./lib/)")"
check "says what the entry then holds" \
  "$(contains "$out" "compiles a race binary no job of .github/workflows/ci.yaml restores")"

echo "case: a race build leaving a package of the lane out"
root=$(fixture race-fill-missing)
cache_workflow "$root"
replace "$root/.github/scripts/ci_job_reads.py" 'DAEMON_RACE_PACKAGES = ["lib"]' 'DAEMON_RACE_PACKAGES = ["lib", "other"]'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the package, the job and both lists" \
  "$(contains "$out" "::error file=.github/workflows/ci-caches.yaml::.github/scripts/ci_job_reads.py's DAEMON_RACE_PACKAGES names ./other/, which job fill's race build does not (./lib/, ./other/ against ./lib/)")"
check "says what the race step then builds from nothing" \
  "$(contains "$out" "the fill then misses a race binary .github/workflows/ci.yaml's race step links from nothing")"

echo "case: a race build naming the lane's packages in another order"
root=$(fixture race-fill-order)
cache_workflow "$root"
replace "$root/.github/scripts/ci_job_reads.py" 'DAEMON_RACE_PACKAGES = ["lib"]' 'DAEMON_RACE_PACKAGES = ["lib", "other"]'
replace "$root/.github/workflows/ci-caches.yaml" "-race ./lib/" "-race ./other/ ./lib/"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names both orders" \
  "$(contains "$out" "::error file=.github/workflows/ci-caches.yaml::job fill's race build names ./other/, ./lib/, where .github/scripts/ci_job_reads.py's DAEMON_RACE_PACKAGES has ./lib/, ./other/")"

# A fill job with no race build in it at all is the drift this rule exists for, not a tree it
# has nothing to say about, so it fails rather than passing quietly.
echo "case: the fill job with no race build at all"
root=$(fixture race-fill-gone)
cache_workflow "$root"
replace "$root/.github/workflows/ci-caches.yaml" $'      - if: steps.go-app.outputs.cache-hit != \'true\'\n        run: go test -c -o /dev/null -race ./lib/\n        working-directory: mod\n' ""
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job and what it no longer builds" \
  "$(contains "$out" "::error file=.github/workflows/ci-caches.yaml::no step of job fill runs a \`go test\` with -race")"
check "and the packages the race step then links from nothing" \
  "$(contains "$out" "that step links ./lib/ from nothing on every run")"

# The rule holds the race build of the job DAEMON_RACE_FILL_JOB names, and that job only: a race
# build of another job fills another family, for another lane, and names its own packages. The
# control for this case is "a race build naming a package the lane does not" above, where the same
# extra package in the fill job fails.
echo "case: a race build of another job is not held to the daemon's list"
root=$(fixture race-fill-other-job)
cache_workflow "$root"
cat >> "$root/.github/workflows/ci-caches.yaml" <<'YAML'
  other-fill:
    runs-on: ubuntu-24.04
    steps:
      - run: go test -c -o /dev/null -race ./other/
        working-directory: mod
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "says nothing of the other job's race build" "$(is "$(contains "$out" "job other-fill's race build")" false)"

# A ci-caches.yaml with no job of the name DAEMON_RACE_FILL_JOB gives is a lane nothing fills too,
# whatever race builds its other jobs run.
echo "case: no job of the name DAEMON_RACE_FILL_JOB gives"
root=$(fixture race-fill-job-gone)
cache_workflow "$root"
replace "$root/.github/scripts/ci_job_reads.py" 'DAEMON_RACE_FILL_JOB = "fill"' 'DAEMON_RACE_FILL_JOB = "daemon"'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job, the table and what the race step then builds" \
  "$(contains "$out" "::error file=.github/workflows/ci-caches.yaml::this workflow has no job daemon, which .github/scripts/ci_job_reads.py's DAEMON_RACE_FILL_JOB names")"

# script_fill <root>: a second fill job, script-fill, that compiles a race step's script, mod's
# scripts/race.sh, in its compile mode, and RACE_FILL_SCRIPTS naming it; script-fill also builds
# something plain, which the rule reads past. The tree passes as written.
script_fill() {
  cat >> "$1/.github/workflows/ci-caches.yaml" <<'YAML'
  script-fill:
    runs-on: ubuntu-24.04
    steps:
      - run: |
          go vet ./...
          scripts/race.sh compile
        working-directory: mod
YAML
  replace "$1/.github/scripts/ci_job_reads.py" 'RACE_FILL_SCRIPTS = []' \
    'RACE_FILL_SCRIPTS = [("script-fill", "mod/scripts/race.sh", "mod", "scripts/race.sh compile")]'
}

# The fill of a race step that runs a script compiles through that script, the one line
# RACE_FILL_SCRIPTS names, and holds no race build of its own: a copy of the step's packages or
# flags written in the fill is what drifts from the step with nothing failing.
echo "case: a fill that compiles a race step's script through it"
root=$(fixture race-script-fill)
cache_workflow "$root"
script_fill "$root"
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: a fill that calls the script with another mode or flag"
root=$(fixture race-script-fill-flag)
cache_workflow "$root"
script_fill "$root"
replace "$root/.github/workflows/ci-caches.yaml" "scripts/race.sh compile" "scripts/race.sh compile -count=1"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job and the line it runs" \
  "$(contains "$out" "::error file=.github/workflows/ci-caches.yaml::job script-fill runs \`scripts/race.sh compile -count=1\`, where the fill runs \`scripts/race.sh compile\`")"

echo "case: a fill that copies the race build in place of the script"
root=$(fixture race-script-fill-copy)
cache_workflow "$root"
script_fill "$root"
replace "$root/.github/workflows/ci-caches.yaml" "scripts/race.sh compile" "go list ./... | grep -Ev '/textcheck\$' | xargs go test -c -o /dev/null -race"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job and the copied line" \
  "$(contains "$out" "::error file=.github/workflows/ci-caches.yaml::job script-fill runs \`go list ./... | grep -Ev '/textcheck\\\$' | xargs go test -c -o /dev/null -race\`, a race build of its own")"
check "and says the fill no longer calls the script" \
  "$(contains "$out" "job script-fill runs \`scripts/race.sh compile\` 0 times in mod")"

# Rule 20. darwin_vet <root> <set>...: a step of the app job that vets ./... of mod for
# darwin/arm64 once per <set> (an empty one is no tag), pinned as an exact entry with one Run
# per set carrying that platform and those tags, as ci_job_reads.py's darwin_vet pins this
# repository's three. lib gets a file under the build tag probe, and three no tag choice brings
# to darwin/arm64: one under linux, one under race, which the toolchain sets, and one under
# amd64.v1, an architecture feature.
darwin_vet() {
  local root=$1 runs="" set
  shift
  for set in "$@"; do
    runs+="        run(\"mod\", [\"./...\"], \"the darwin vet\", tags=\"$set\", mode=\"vet\", env={\"GOOS\": \"darwin\", \"GOARCH\": \"arm64\"}),"$'\n'
  done
  printf '//go:build probe\n\npackage lib\n\nfunc Probe() {}\n' > "$root/mod/lib/probe.go"
  printf '//go:build linux\n\npackage lib\n\nfunc Linux() {}\n' > "$root/mod/lib/linux_only.go"
  printf '//go:build race\n\npackage lib\n\nconst Race = true\n' > "$root/mod/lib/race_on.go"
  printf '//go:build amd64.v1\n\npackage lib\n\nconst Feature = true\n' > "$root/mod/lib/feature.go"
  replace "$root/.github/workflows/ci.yaml" "      - name: Test the app" \
    "      - name: Vet the library for darwin"$'\n'"        run: GOOS=darwin GOARCH=arm64 go vet ./..."$'\n'"        working-directory: mod"$'\n'"      - name: Test the app"
  replace "$root/.github/ci-gates/app.py" '], reads=[Read(".github/workflows/ci.yaml", "a job reads its own definition")])' \
    "], exact=[exact(\"Vet the library for darwin\", \"mod\", \"GOOS=darwin GOARCH=arm64 go vet ./...\", ["$'\n'"$runs    ])], reads=[Read(\".github/workflows/ci.yaml\", \"a job reads its own definition\")])"
}

echo "case: a darwin vet whose tag set names every tag its module's files are built under"
root=$(fixture vet-tags-whole)
darwin_vet "$root" probe
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "and reports what it judged" \
  "$(contains "$out" 'report: job app step "Vet the library for darwin": vets mod for darwin/arm64 under -tags probe; go list compiles 5 of the 8 .go files on disk there, 0 of the rest name a tag, and 0 of those build for darwin/arm64 under tags no set holds')"

echo "case: a darwin vet whose tag set leaves out a tag a file of its module is built under"
root=$(fixture vet-tags-missing)
darwin_vet "$root" ""
# A file under odatrace, which only forced-order.sh's build against its patched nats-server sets.
printf '//go:build odatrace\n\npackage lib\n\nconst Hooked = true\n' > "$root/mod/lib/hooked.go"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the file and the tags that would compile it" \
  "$(contains "$out" '::error file=mod/lib/probe.go::job app step "Vet the library for darwin" vets mod for darwin/arm64 under -tags (none), and none of them compiles mod/lib/probe.go, which go builds for darwin/arm64 with -tags probe')"
check "and passes over the platform's, the toolchain's, the feature's and the patched build's files" \
  "$(is "$(contains "$out" 'compiles mod/lib/linux_only.go\|compiles mod/lib/race_on.go\|compiles mod/lib/feature.go\|compiles mod/lib/hooked.go')" false)"

echo "case: one tag set beside a file under the negated tag"
root=$(fixture vet-tags-negated)
darwin_vet "$root" probe
printf '//go:build !probe\n\npackage lib\n\nfunc NoProbe() {}\n' > "$root/mod/lib/noprobe.go"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the file the set leaves out and the empty set that builds it" \
  "$(contains "$out" '::error file=mod/lib/noprobe.go::job app step "Vet the library for darwin" vets mod for darwin/arm64 under -tags probe, and none of them compiles mod/lib/noprobe.go, which go builds for darwin/arm64 with -tags (none)')"

echo "case: two tag sets, one with the tag and one without, beside the negated file"
root=$(fixture vet-tags-negated-covered)
darwin_vet "$root" probe ""
printf '//go:build !probe\n\npackage lib\n\nfunc NoProbe() {}\n' > "$root/mod/lib/noprobe.go"
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: a package whose every file is under a tag, a tag in another script, a +build line, and headers gofmt would rewrite"
root=$(fixture vet-tags-unlisted)
darwin_vet "$root" probe
mkdir -p "$root/mod/only"
printf '//go:build hidden\n\npackage only\n' > "$root/mod/only/only.go"
printf '//go:build probe_\xce\xb2\n\npackage lib\n\nfunc Beta() {}\n' > "$root/mod/lib/beta.go"
printf '// +build legacy\n\npackage lib\n\nfunc Legacy() {}\n' > "$root/mod/lib/legacy.go"
# Three headers go/build reads a constraint from although gofmt would rewrite each: one behind a
# block comment, one indented, one after a byte-order mark.
printf '/* A block comment. */\n//go:build blocked\n\npackage lib\n\nfunc Blocked() {}\n' > "$root/mod/lib/blocked.go"
printf '  //go:build indented\n\npackage lib\n\nfunc Indented() {}\n' > "$root/mod/lib/indented.go"
printf '\xef\xbb\xbf//go:build marked\n\npackage lib\n\nfunc Marked() {}\n' > "$root/mod/lib/marked.go"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the file of the package go list ./... leaves out" \
  "$(contains "$out" '::error file=mod/only/only.go::.*which go builds for darwin/arm64 with -tags hidden')"
check "names the file under the tag a byte-wise reader would cut short" \
  "$(contains "$out" $'::error file=mod/lib/beta.go::.*which go builds for darwin/arm64 with -tags probe_\xce\xb2:')"
check "names the file whose only constraint is a +build line" \
  "$(contains "$out" '::error file=mod/lib/legacy.go::.*which go builds for darwin/arm64 with -tags legacy')"
check "names the file whose constraint follows a block comment" \
  "$(contains "$out" '::error file=mod/lib/blocked.go::.*which go builds for darwin/arm64 with -tags blocked')"
check "names the file whose constraint is indented" \
  "$(contains "$out" '::error file=mod/lib/indented.go::.*which go builds for darwin/arm64 with -tags indented')"
check "names the file whose constraint follows a byte-order mark" \
  "$(contains "$out" '::error file=mod/lib/marked.go::.*which go builds for darwin/arm64 with -tags marked')"

# go_build_tags.py, the script the vet steps take their sets from, over a module holding each
# shape above: the first set is every tag, and the file under the negated tag adds a set
# without it, which also builds the file whose +build line wants legacy and not probe. No set
# names the platform's, the toolchain's or the feature's names, or odatrace, which only
# forced-order.sh's build sets, against a nats-server it patches. go list decides every answer,
# so a go command that fails fails the script, and the step that takes its sets stops there.
echo "case: go_build_tags.py's sets for a module with a negated tag, a tag-only package and +build lines"
root=$(fixture vet-tags-sets)
darwin_vet "$root" probe
mkdir -p "$root/mod/only"
printf '//go:build hidden\n\npackage only\n' > "$root/mod/only/only.go"
printf '//go:build probe_\xce\xb2\n\npackage lib\n\nfunc Beta() {}\n' > "$root/mod/lib/beta.go"
printf '// +build legacy,!probe\n\npackage lib\n\nfunc Legacy() {}\n' > "$root/mod/lib/legacy.go"
printf '//go:build !probe\n\npackage lib\n\nfunc NoProbe() {}\n' > "$root/mod/lib/noprobe.go"
printf '//go:build odatrace\n\npackage lib\n\nconst Hooked = true\n' > "$root/mod/lib/hooked.go"
sets=$(cd "$root" && python3 .github/scripts/go_build_tags.py mod darwin arm64 2>&1)
check "prints every tag, then the set without probe, each as a whole -tags= flag" \
  "$(is "$sets" $'-tags=hidden,legacy,probe,probe_\xce\xb2\n-tags=hidden,legacy,probe_\xce\xb2')"
status=0
sets=$(cd "$root" && GOFLAGS=-no-such-flag python3 .github/scripts/go_build_tags.py mod darwin arm64 2>&1) || status=$?
check "and fails when go list fails" "$(is "$status" 1)"
check "naming the go command" "$(contains "$sets" 'go_build_tags.py: in mod, go tool dist list ... failed')"

# odatrace is the one name go_build_tags.py leaves out of every vet that the toolchain does not
# set or that marks a file no build compiles: only forced-order.sh's build passes it, against a
# nats-server it patches, so its files compile nowhere else. A name added beside it, or a
# forced-order.sh that stops passing it, fails here, so the exclusion cannot come to hide a tag a
# plain build compiles.
echo "case: go_build_tags.py leaves out only the toolchain's names and forced-order.sh's odatrace"
patched=$(PYTHONDONTWRITEBYTECODE=1 python3 - "$script_dir" <<'PY'
import sys
sys.path.insert(0, sys.argv[1])
from go_build_tags import NOT_TAGS
toolchain = {"unix", "cgo", "gc", "gccgo", "race", "msan", "asan", "ignore", "boringcrypto"}
print(",".join(sorted(NOT_TAGS - toolchain)))
PY
)
check "the one name beyond the toolchain's is odatrace" "$(is "$patched" odatrace)"
check "and forced-order.sh passes -tags odatrace to go" \
  "$(contains "$(<"$script_dir/../../packages/envoy/scripts/forced-order.sh")" '^flags=(.* -tags odatrace .*)$')"

# The typecheck step's own run text, taken from its workflow, over a module with one tag and a
# file under its negation that does not compile: the script's last set is the one with no tag,
# and a shell's command substitution drops a trailing empty line, so a transport of bare sets
# would vet only -tags=feature and pass. The module sits two directories deep, as
# packages/daemon does, so the step's ../../.github/scripts path reaches the fixture's script.
echo "case: the typecheck step's own loop vets the file under a negated tag"
root=$(fixture vet-tags-loop)
mkdir -p "$root/deep/module"
printf 'module example.com/deep\n\ngo 1.21\n' > "$root/deep/module/go.mod"
printf '//go:build feature\n\npackage module\n\nvar On = 1\n' > "$root/deep/module/on.go"
printf '//go:build !feature\n\npackage module\n\nvar _ = notDefined\n' > "$root/deep/module/off.go"
PYTHONDONTWRITEBYTECODE=1 python3 - "$script_dir/../workflows/ci-job-typecheck.yaml" "$root/step.sh" <<'PY'
import sys, yaml
jobs = yaml.safe_load(open(sys.argv[1]))["jobs"].values()
step = next(s for j in jobs for s in j["steps"] if s.get("name", "").startswith("Vet the Go daemon for darwin"))
open(sys.argv[2], "w").write(step["run"])
PY
status=0
loop=$(cd "$root/deep/module" && GOWORK=off bash -e "$root/step.sh" 2>&1) || status=$?
check "fails" "$(is "$status" 1)"
check "vets under both sets" \
  "$(is "$(grep -c '^Vetting for darwin with -tags=' <<<"$loop")" 2)"
check "and names the file only the set with no tag compiles" "$(contains "$loop" 'off.go:5:9: undefined: notDefined')"

# No case runs this rule, or any other rule of check-ci-filters.sh, over this repository's own
# tree: ci.yaml's ci-filters job runs the whole check there (.github/workflows/ci-job-ci-filters.yaml,
# "Check every CI filter matches every file its job reads"), so such a case would only reach the
# same verdict a second time in the same CI run.

echo "case: a workspace of the root manifest that WORKSPACES does not name"
root=$(fixture workspace-untabled)
replace "$root/.github/scripts/ci_job_reads.py" 'WORKSPACES = ["packages/widget"]' 'WORKSPACES = []'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the workspace and the table" \
  "$(contains "$out" "::error file=package.json::workspaces names packages/widget, which .github/scripts/ci_job_reads.py's WORKSPACES does not")"

echo "case: a WORKSPACES entry the root manifest does not list"
root=$(fixture workspace-unlisted)
replace "$root/.github/scripts/ci_job_reads.py" 'WORKSPACES = ["packages/widget"]' \
  'WORKSPACES = ["packages/widget", "packages/gone"]'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the entry and the manifest" \
  "$(contains "$out" "::error file=.github/scripts/ci_job_reads.py::WORKSPACES names packages/gone, which the root manifest's workspaces does not")"

echo "case: every filter of a job that compiles or reads another package's files selects them"
# Rule 13 holds the root manifest's workspaces equal to WORKSPACES, which makes every Bun job
# declare a read of each workspace's manifest. It says nothing about a workspace's sources, so a
# package can be compiled by a job whose filter never selects it, and a test can read a file of
# another package that no filter of its job names. Rule 1 does not see either, because for the
# Bun gates the declared reads are hand-kept beside the filter and the two go short together.
# Each pair below was measured rather than reasoned: COMPILERS with
# `bunx tsc --listFilesOnly` in the package, or `bun build --sourcemap=external` of the entry
# the job runs, and READS by following the relative path the named test resolves. A filter that
# gates such a job and does not select what it compiles or reads leaves the change that breaks
# it starting no job at all, on the pull request and again on the merge group that lands it.
#
# COMPILERS and READS are hand-kept, like the reads they guard: they hold the pairs somebody has
# measured, so a job added later is not held until its pair is written here. The JSON files the
# packages' tests import from another package by a relative path are derived (JSON_READERS below),
# with a planted import as the control that the derivation refuses an unselected one.
# The filters are the ones the changes job computes: the file CI_FILTERS_FILE names (the test job
# hands it the one its run's changes job wrote), or one this case computes, which needs go and the
# workspace's node_modules. A file written to fail open holds nothing to check, and is refused.
real_filters=${CI_FILTERS_FILE:-$work/real-ci-filters.yaml}
if [ -z "${CI_FILTERS_FILE:-}" ]; then
  "$script_dir/check-ci-filters.sh" generate "$real_filters" > "$work/real-generate.log" 2>&1 || true
fi
check "the repository's filters are computed" \
  "$(is "$([ -s "$real_filters" ] && ! grep -q '^# FAIL-OPEN:' "$real_filters" && echo yes || echo no)" yes)"
uncovered=$(PYTHONDONTWRITEBYTECODE=1 PYTHONPATH="$script_dir" python3 - "$real_filters" "$script_dir/../.." <<'PY'
import re, sys, tempfile, yaml, workflow_paths as wp
from pathlib import Path

SOURCE = "packages/cipher-client/src/token-client.ts"

# Each job's own tsc program, or the runtime closure of the entry it runs, holds the seven
# sources of packages/cipher-client, which packages/envoy-client/src/broker-credential.ts
# imports (brokerTokenClient from its /environment module) and whose exports map every entry to
# ./src/<module>.ts.
COMPILERS = (
    ("claude-envoy", "claude_envoy"),
    ("dispatch-ui", "dispatch_ui"),
    ("envoy-client", "envoy_client"),
    ("envoy-session-stall", "envoy_session_stall"),
    ("pi-envoy", "pi_envoy"),
    ("pi-legion", "pi_legion"),
    ("pi-shared", "pi_shared"),
)

# A file of another package that the job's own tests compile, read by relative path, or run as a
# program, with the test that reaches it. The last class is the one no derivation sees: a spawn
# carries no import, so `bunx tsc --listFilesOnly` of the job's own program lists none of it
# (measured: zero claude-envoy files in packages/pi-legion's program), and check-ts-filters.sh
# cannot hold the filter to it.
READS = (
    ("broker_client", "packages/envoy/internal/cipherclient/testdata/wire.json",
     "packages/cipher-client/src/__tests__/wire.test.ts:25-26"),
    ("contracts", "packages/envoy/internal/dispatch/text/testdata/dispatch-block-id-encodings.json",
     "packages/contracts/src/dispatch-block-ref.test.ts:98-102"),
    ("contracts", "packages/daemon/internal/intake/testdata/dispatch/server-started.json",
     "packages/contracts/src/dispatch-api.test.ts:293-300"),
    ("dispatch_ui", "packages/envoy/internal/dispatch/pmdoc/schema/block-node-types.json",
     "packages/proof-editor/tests/ask-concurrent-edit.test.ts:269-275"),
    ("claude_envoy", "packages/envoy-client/test/stall-proxy.ts",
     "packages/claude-envoy/tests/envoy-channel-stall.test.ts:9-11"),
    ("pi_legion", "packages/dispatch/e2e/harness-processes.ts", "packages/pi-legion's tsc program"),
    ("pi_legion", "packages/dispatch/e2e/harness-broker.ts", "packages/pi-legion's tsc program"),
    ("pi_legion", "packages/dispatch/e2e/harness-real-broker.ts", "packages/pi-legion's tsc program"),
    ("pi_legion", "packages/dispatch/e2e/harness-signin.ts", "packages/pi-legion's tsc program"),
    ("pi_legion", "packages/claude-envoy/scripts/build.ts",
     "packages/pi-legion/src/skills-guard.test.ts:29,318, which spawns that builder and asserts an "
     "exact file list over what it writes"),
    ("pi_legion", "packages/claude-envoy/bin/envoy-channel.ts",
     "that builder's BUNDLE_ENTRYPOINTS (packages/claude-envoy/scripts/build.ts:39-44), bundled into "
     "the tree the same test asserts over"),
    ("pi_legion", "packages/envoy-client/bin/dispatch.ts",
     "the same BUNDLE_ENTRYPOINTS entry, reached from another package again"),
    ("test", "packages/dispatch/web/src/fonts/inter/package.json",
     "scripts/third-party-notices.test.ts:102-105 (thirdPartyNotices over the font file's path)"),
    ("test", "packages/dispatch/web/src/fonts/inter/LICENSE.txt", "the same thirdPartyNotices call"),
)

# Every JSON file a test of one of these packages imports from outside the package by a
# relative path (`import x from "../../contracts/fixtures/y.json"`) is derived, not hand-kept: the
# scan below reads each package's test files, resolves each such import and holds the package's
# filter to it. Such a file is part of no tsc program when the test file is outside the package's
# `include` (packages/pi-envoy's is `extensions/envoy.ts` alone), so check-ts-filters.sh cannot see
# it, and a fixture change would start no job of the package that reads it.
JSON_READERS = (
    ("packages/claude-envoy", "claude_envoy"),
    ("packages/envoy-client", "envoy_client"),
    ("packages/pi-envoy", "pi_envoy"),
    ("packages/pi-legion", "pi_legion"),
    ("packages/pi-shared", "pi_shared"),
)
IMPORT = re.compile(r'''^\s*import\s[^;]*?from\s+["'](\.\.?/[^"']+\.json)["']''', re.M)


def json_reads(root):
    """Each (filter, file, test) where a test of a JSON_READERS package imports a JSON file outside
    that package by a relative path, the file repository-relative."""
    found = []
    for package, name in JSON_READERS:
        base = root / package
        for test in sorted(base.rglob("*.test.ts")):
            if "node_modules" in test.parts:
                continue
            for match in IMPORT.finditer(test.read_text(errors="replace")):
                target = (test.parent / match.group(1)).resolve()
                try:
                    rel = target.relative_to(root.resolve()).as_posix()
                except ValueError:
                    continue
                if not rel.startswith(package + "/"):
                    line = test.read_text(errors="replace")[: match.start(1)].count("\n") + 1
                    found.append((name, rel, f"{test.relative_to(root).as_posix()}:{line}"))
    return found


# Every filter of the changes job, which its one paths-filter step hands the action as this
# file's path rather than as a string of its own, read refusing a repeated key as the action does.
filters = yaml.load(Path(sys.argv[1]).read_text(), Loader=wp.UniqueKeyLoader)


def flat(entry):
    return [entry] if isinstance(entry, str) else [p for x in entry for p in flat(x)]


def covers(name, path):
    return wp.Filter(flat(filters[name])).covers(path)


root = Path(sys.argv[2])
derived = json_reads(root)
bad = [f"{job} ({name}) compiles {SOURCE}" for job, name in COMPILERS if not covers(name, SOURCE)]
bad += [f"{name} does not select {path}, which {who} reads"
        for name, path, who in READS + tuple(derived) if not covers(name, path)]
if not derived:
    bad.append("the scan of the packages' tests found no JSON import outside a package, so it read nothing")

# The control: a planted test of pi-envoy that imports a JSON file no filter selects is found by
# the same scan and refused by the same rule.
with tempfile.TemporaryDirectory() as scratch:
    planted = Path(scratch) / "packages/pi-envoy/extensions/planted.test.ts"
    planted.parent.mkdir(parents=True)
    planted.write_text('import planted from "../../contracts/fixtures/planted-unselected.json";\n')
    (Path(scratch) / "packages/contracts/fixtures").mkdir(parents=True)
    caught = [(n, p) for n, p, _ in json_reads(Path(scratch)) if not covers(n, p)]
    if caught != [("pi_envoy", "packages/contracts/fixtures/planted-unselected.json")]:
        bad.append(f"the control's planted unselected import was not refused: {caught}")
print("; ".join(bad))
PY
)
check "no filter gates a job that compiles or reads files it does not select" \
  "$(is "$uncovered" "")"

# Rule 13 reads the root manifest as check-bun-workspace-manifests.sh reads it, at every shape:
# a list it cannot read is named rather than taken for an empty one, which would hold WORKSPACES
# to nothing, and the two spellings of a workspace are one workspace.
echo "case: a root manifest rule 13 cannot read"
root=$(fixture workspace-manifest-gone)
rm "$root/package.json"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names what the table is then held to" \
  "$(contains "$out" "::error file=package.json::the root manifest is missing, so .github/scripts/ci_job_reads.py's WORKSPACES is held to nothing")"

echo "case: a root manifest that is not valid JSON"
root=$(fixture workspace-manifest-unreadable)
replace "$root/package.json" '"workspaces": ["packages/widget"]' '"workspaces": ["packages/widget",'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the manifest and what it could not read" \
  "$(contains "$out" "::error file=package.json::the root manifest is not valid JSON (")"

echo "case: a workspaces that is not a list"
root=$(fixture workspace-not-a-list)
replace "$root/package.json" '"workspaces": ["packages/widget"]' '"workspaces": "packages/widget"'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names what workspaces must be for the table to be held to it" \
  "$(contains "$out" "::error file=package.json::workspaces must be an array of workspace paths for .github/scripts/ci_job_reads.py's WORKSPACES to be held to it")"

echo "case: a workspaces holding an entry that is no workspace path"
root=$(fixture workspace-entry-unreadable)
replace "$root/package.json" '["packages/widget"]' '["packages/widget", 7]'
run_check "$root"
check "fails on a non-string entry" "$(is "$status" 1)"
check "names what workspaces must be" \
  "$(contains "$out" "::error file=package.json::workspaces must be an array of workspace paths")"
replace "$root/package.json" '["packages/widget", 7]' '["packages/widget", ""]'
run_check "$root"
check "fails on an empty entry too" "$(is "$status" 1)"
check "and names the same thing" \
  "$(contains "$out" "::error file=package.json::workspaces must be an array of workspace paths")"

echo "case: an empty workspaces array against a table that names none"
root=$(fixture workspace-none)
replace "$root/package.json" '["packages/widget"]' '[]'
replace "$root/.github/scripts/ci_job_reads.py" 'WORKSPACES = ["packages/widget"]' 'WORKSPACES = []'
run_check "$root"
check "passes: a root declaring no workspace agrees with a table naming none" "$(is "$status" 0)"

echo "case: a trailing slash on either side names the same workspace"
root=$(fixture workspace-trailing-slash)
replace "$root/package.json" '["packages/widget"]' '["packages/widget/"]'
run_check "$root"
check "passes with the slash in the manifest" "$(is "$status" 0)"
replace "$root/package.json" '["packages/widget/"]' '["packages/widget"]'
replace "$root/.github/scripts/ci_job_reads.py" 'WORKSPACES = ["packages/widget"]' 'WORKSPACES = ["packages/widget/"]'
run_check "$root"
check "and with it in the table" "$(is "$status" 0)"

echo "case: the merge-group trigger belongs to ci.yaml alone"
# ci-ok-report.sh POSTs the failing ci-ok of a merge-group run the reporting job's own result
# decides, and a POSTed check run joins the Actions App's first check suite at that commit
# rather than the posting run's, so the queue reads that POST only while ci.yaml is the one
# workflow a merge group starts.
root=$(fixture merge-group-only)
replace "$root/.github/workflows/ci.yaml" "on: [pull_request]" $'on:\n  pull_request:\n  merge_group:'
run_check "$root"
check "passes with ci.yaml itself on merge_group" "$(is "$status" 0)"
cat > "$root/.github/workflows/scan.yaml" <<'YAML'
name: Scan
on:
  pull_request:
  merge_group:
jobs:
  scan:
    runs-on: ubuntu-24.04
    steps:
      - run: echo scan
YAML
run_check "$root"
check "fails once a second workflow runs on it" "$(is "$status" 1)"
check "naming the file it found and the one workflow that may" \
  "$(contains "$out" "::error file=.github/workflows/scan.yaml::.github/workflows/scan.yaml runs on merge_group, which only .github/workflows/ci.yaml may")"
check "the early POST a second suite breaks" \
  "$(contains "$out" "a POSTed check run joins the Actions App.s first check suite at that commit rather than the posting run.s")"
check "and the measurement behind it" \
  "$(contains "$out" 'measured on pull request #2847, which the queue refused to enqueue')"
mv "$root/.github/workflows/scan.yaml" "$root/.github/workflows/scan.yml"
run_check "$root"
check "and reads the .yml spelling GitHub reads too" \
  "$(contains "$out" "::error file=.github/workflows/scan.yml::")"

# ci_job_reads.py builds envoy-go's plain step - the tests a race build cannot run at all or at
# full size - by reading packages/envoy's own source rather than from a list a table could hold,
# so the cases below run that walk itself, over a tree of this test's making and a lane of its
# own. What a fixture cannot reach, since each writes a ci_job_reads.py of its own and the walk
# reads the repository it sits in: the walk's result standing in ci.yaml, so that a test added to
# packages/envoy under //go:build !race or behind raceEnabled makes check-ci-filters.sh fail
# naming that step. Only the repository's own tree holds that.

# derived <root> <lane>: the step script ci_job_reads.py's walk builds over <root>, with the race
# lane replaced by <lane>, a space-separated list of package paths under <root> ("pkg" for the
# package alone, "pkg/..." for the tree). Echoes "RAISED: <what it refused>" when the walk
# refuses what it reads.
derived() { ask derived "$1" "$2"; }

# race_tree: a lane of two packages, one of them a tree, holding every form the walk reads. Echoes
# its root.
race_tree() {
  local root="$work/race-lane"
  mkdir -p "$root/lane/a/sub" "$root/lane/a/testdata" "$root/lane/b"
  # Every test of a file the race build does not compile, TestMain (the package's entry point, not
  # a test) and the helper aside.
  cat > "$root/lane/a/norace_test.go" <<'GO'
//go:build !race

package a

import "testing"

func TestMain(m *testing.M)  {}
func TestAlpha(t *testing.T) {}
func FuzzBeta(f *testing.F)  {}
func ExampleGamma()          {}
func helperDelta()           {}
GO
  # The constant's own declaration names no test, and this file has none to name.
  cat > "$root/lane/a/norace_constant_test.go" <<'GO'
//go:build !race

package a

const raceEnabled = false
GO
  # A file both builds compile: the tests that read raceEnabled run smaller under the detector, so
  # the plain run is their full-size one. Only the test that encloses a read is named by it:
  # TestEpsilon reads it, helperZeta after it reads nothing (a read there is refused, the case
  # below), and neither does TestEta.
  cat > "$root/lane/a/plain_test.go" <<'GO'
package a

import "testing"

// raceEnabled in a comment names no test.
const notice = "raceEnabled in a string names none either"

func TestEpsilon(t *testing.T) {
	if raceEnabled {
		t.Skip(notice)
	}
}

func helperZeta() bool {
	return true
}

func TestEta(t *testing.T) {}
GO
  # The race build's own declaration of the constant.
  cat > "$root/lane/a/race_constant_test.go" <<'GO'
//go:build race

package a

const raceEnabled = true
GO
  cat > "$root/lane/a/sub/sub_test.go" <<'GO'
//go:build !race

package sub

import "testing"

func TestTheta(t *testing.T) {}
GO
  # go reads no package under testdata, and neither does the walk.
  cat > "$root/lane/a/testdata/ignored_test.go" <<'GO'
//go:build !race

package testdata

import "testing"

func TestIgnored(t *testing.T) {}
GO
  # A package of the lane with no such test is left out of the step.
  cat > "$root/lane/b/b_test.go" <<'GO'
package b

import "testing"

func TestIota(t *testing.T) {}
GO
  echo "$root"
}

echo "case: the plain step's -run lists are what the lane's packages hold"
root=$(race_tree)
out=$(derived "$root" "lane/a/... lane/b")
check "names every test of a !race file, sorted, TestMain aside, and the raceEnabled ones" \
  "$(is "$out" "$(printf '%s\n%s' \
    "go test -count=1 -timeout 10m -run '^(ExampleGamma|FuzzBeta|TestAlpha|TestEpsilon)\$' ./lane/a" \
    "go test -count=1 -timeout 10m -run '^(TestTheta)\$' ./lane/a/sub")")"
check "passes over the testdata package" "$(is "$(contains "$out" 'TestIgnored')" false)"
check "and leaves out a package with no such test" "$(is "$(contains "$out" './lane/b')" false)"

echo "case: a package the lane names alone does not reach the packages under it"
out=$(derived "$root" "lane/a")
check "names the package alone" \
  "$(is "$out" "go test -count=1 -timeout 10m -run '^(ExampleGamma|FuzzBeta|TestAlpha|TestEpsilon)\$' ./lane/a")"

echo "case: a raceEnabled no top-level function encloses"
root="$work/race-unattributable"
mkdir -p "$root/pkg"
printf 'package pkg\n\nvar smaller = raceEnabled\n' > "$root/pkg/x_test.go"
out=$(derived "$root" "pkg")
check "refuses rather than dropping a test from the step" \
  "$(contains "$out" 'RAISED: .*x_test.go:3: raceEnabled is read where no top-level test encloses it')"

# The read a helper holds is the bug this refusal answers: the test before it need not be the one
# that calls it, so the plain step would run a test that is no smaller under the detector and
# leave out the one that is.
echo "case: a raceEnabled a helper holds rather than the test that calls it"
root="$work/race-helper-read"
mkdir -p "$root/pkg"
printf 'package pkg\n\nimport "testing"\n\nfunc TestEarlier(t *testing.T) {}\n\nfunc helperLater() bool {\n\treturn raceEnabled\n}\n\nfunc TestLater(t *testing.T) {\n\tif helperLater() {\n\t\tt.Skip("smaller")\n\t}\n}\n' > "$root/pkg/x_test.go"
out=$(derived "$root" "pkg")
check "refuses rather than naming the test before the helper" \
  "$(contains "$out" 'RAISED: .*x_test.go:8: raceEnabled is read where no top-level test encloses it')"
check "and names neither test" "$(is "$(contains "$out" 'TestEarlier')" false)"

# A one-line body writes no closing brace in column 0, so a rule that ended a body at the next
# such line let a read at package level after one belong to that test. Here the sizes a function
# literal computes at package level are the ones the detector shrinks, and naming the test above
# them in their place would have run a test that is no smaller under the detector and left out
# whichever one reads sizes. go_top_level_spans() counts braces from the declaration's own `{`,
# so the body ends where it ends and the read is refused.
echo "case: a raceEnabled a package-level declaration holds after a one-line test"
root="$work/race-package-level-read"
mkdir -p "$root/pkg"
printf 'package pkg\n\nimport "testing"\n\nfunc TestOneLine(t *testing.T) { run(t, sizes) }\n\nvar sizes = func() int {\n\tif raceEnabled {\n\t\treturn 2\n\t}\n\treturn 4\n}()\n\nfunc run(t *testing.T, n int) {}\n' > "$root/pkg/x_test.go"
out=$(derived "$root" "pkg")
check "refuses rather than naming the one-line test above it" \
  "$(contains "$out" 'RAISED: .*x_test.go:8: raceEnabled is read where no top-level test encloses it')"
check "and names no test" "$(is "$(contains "$out" 'TestOneLine')" false)"

# A body ends at the brace that closes it, counted from the declaration's own `{`, so a
# declaration whose braces never balance has no end to find. Taking the rest of the file for its
# body would attribute to it every read after it, which is the very guess this refuses.
echo "case: a top-level function whose body never closes"
root="$work/race-unbalanced-braces"
mkdir -p "$root/pkg"
printf 'package pkg\n\nimport "testing"\n\nfunc TestUnclosed(t *testing.T) {\n\tif raceEnabled {\n\nfunc TestAfter(t *testing.T) {}\n' > "$root/pkg/x_test.go"
out=$(derived "$root" "pkg")
check "refuses rather than taking the rest of the file for the body" \
  "$(contains "$out" 'RAISED: .*x_test.go:5: the braces of this top-level declaration never balance')"
check "and names no test" "$(is "$(contains "$out" 'TestUnclosed')" false)"

echo "case: a //go:build that names race in a form the walk does not read"
root="$work/race-unread-constraint"
mkdir -p "$root/pkg"
printf '//go:build !race && !windows\n\npackage pkg\n\nfunc TestKappa(t *testing.T) {}\n' > "$root/pkg/x_test.go"
out=$(derived "$root" "pkg")
check "refuses rather than guessing what the race build compiles" \
  "$(contains "$out" 'RAISED: .*x_test.go: `//go:build !race && !windows` names race in a form ci_job_reads.py does not read')"

echo "case: the lane names a package that is not there"
out=$(derived "$work/race-unattributable" "pkg/gone")
check "refuses" "$(contains "$out" 'RAISED: the race lane names pkg/gone, which is no directory under ')"

# selects <name> <path>: whether the generated filter <name> selects <path>, read as
# dorny/paths-filter reads it (some-with-excludes).
selects() {
  (cd "$root" && PYTHONDONTWRITEBYTECODE=1 PYTHONPATH="$script_dir" python3 -c '
import sys, yaml
from workflow_paths import PathsFilter
filters = yaml.safe_load(open(sys.argv[1]))
print("true" if PathsFilter(filters[sys.argv[2]], "some-with-excludes").covers(sys.argv[3]) else "false")
' "$root.generated.yaml" "$1" "$2")
}

# The `!` patterns that keep a gate's filter off the Go test files its job does not compile are
# computed with the filter. The fixture's app gate runs `go test ./cmd/app ./lib`, so it reads
# mod/lib's test files; a package of a tree the gate declares and compiles nothing of is what a
# `!` pattern is for.
# unread_package <root>: mod/lib/sub, a package the app job does not compile, under a tree its
# gate declares a read of, its Go tests left to go list.
unread_package() {
  local root=$1
  mkdir -p "$root/mod/lib/sub"
  printf 'package sub\n' > "$root/mod/lib/sub/sub.go"
  printf 'package sub\n\nimport "testing"\n\nfunc TestSub(t *testing.T) {}\n' > "$root/mod/lib/sub/sub_test.go"
  replace "$root/.github/ci-gates/app.py" 'reads=[Read(".github/workflows/ci.yaml", "a job reads its own definition")]' \
    'reads=[Read(".github/workflows/ci.yaml", "a job reads its own definition"), Read("mod/lib/**", "the app reads the library tree", but=("**/*_test.go",))]'
}

echo "case: the generation keeps a filter off the test files its job does not compile"
root=$(fixture generated-negation)
unread_package "$root"
run_generate "$root"
check "exits 0" "$(is "$status" 0)"
check "carries the gate's glob as written" "$(contains "$(filter_of app)" '^mod/lib/\*\*$')"
check "writes the tree the gate compiles nothing of" "$(contains "$(filter_of app)" '^!mod/lib/sub/\*\*/\*_test.go$')"
check "and leaves the test file the gate reads selected" "$(selects app mod/lib/lib_test.go)"
check "while the unread one is not" "$(is "$(selects app mod/lib/sub/sub_test.go)" false)"
check "and the package's source is" "$(selects app mod/lib/sub/sub.go)"
first=$generated
run_generate "$root"
check "a second run writes the same text" "$(is "$generated" "$first")"

echo "case: a mode this check does not have"
root=$(fixture generated-mode)
run_check "$root" rewrite
check "fails" "$(is "$status" 1)"
check "names the three modes" \
  "$(contains "$out" 'this check takes one optional mode: `check` (the default) refuses what is wrong, `write` writes')"
run_check "$root" generate
check "and generate without a path fails too" "$(is "$status" 1)"

# A wildcard //go:embed pattern reads what the go command embeds for it, matched one element at
# a time as path.Match reads each: a `*` stays inside its directory, a file it matches is that
# file alone, and a directory it matches is every file of its tree go:embed takes.
# wildcard_embed <root>: mod/lib imports mod/lib/assets, a package the app job compiles and does
# not test, whose assets.go embeds `*.up.sql tmpl*`: a.up.sql beside it and the tree of tmpl/.
wildcard_embed() {
  local root=$1
  mkdir -p "$root/mod/lib/assets/tmpl/deep"
  printf 'package assets\n\nimport "embed"\n\n//go:embed *.up.sql tmpl*\nvar Files embed.FS\n' \
    > "$root/mod/lib/assets/assets.go"
  echo 'select 1;' > "$root/mod/lib/assets/a.up.sql"
  echo '<p>page</p>' > "$root/mod/lib/assets/tmpl/page.html"
  echo '<p>more</p>' > "$root/mod/lib/assets/tmpl/deep/more.html"
  printf 'package lib\n\nimport _ "example.com/m/lib/assets"\n\nfunc Hello() {}\n' > "$root/mod/lib/lib.go"
}

echo "case: a wildcard embed's files are the job's, and a test file beside them is not"
root=$(fixture embed-wildcard)
wildcard_embed "$root"
printf 'package assets\n\nimport "testing"\n\nfunc TestFiles(t *testing.T) {}\n' > "$root/mod/lib/assets/assets_test.go"
printf 'draft\n' > "$root/mod/lib/assets/tmpl/_draft.html"
run_generate "$root"
check "exits 0" "$(is "$status" 0)"
check "selects the file the embed matches beside it" "$(selects app mod/lib/assets/a.up.sql)"
check "selects a file of the directory the embed matched" "$(selects app mod/lib/assets/tmpl/page.html)"
check "and of a directory under it" "$(selects app mod/lib/assets/tmpl/deep/more.html)"
check "and not the test file beside them, so a change to it alone selects no job" \
  "$(is "$(selects app mod/lib/assets/assets_test.go)" false)"
check "nor a file whose name go:embed leaves out of a directory's tree" \
  "$(is "$(selects app mod/lib/assets/tmpl/_draft.html)" false)"

# A tag step can lean on its own job: envoy-go's NATS matrix step runs ./internal/testnats under
# natslive with -run while envoy-go's own unit step runs that package's untagged tests, so
# TAG_RUNS pairs the envoy_go filter with itself, and envoy_infra_go too. Rule 15 then puts into
# the filter what the tag step reads, which is the filter's own read set, never what the filter
# selects: the `!` patterns stay.
# self_tag_run <root>: the app job gains a step running ./lib under the live tag with -run,
# pinned whole (a -run is outside the simple form) to a Run of ./lib under that tag, and
# TAG_RUNS pairs that step with the app job's own untagged test step.
self_tag_run() {
  replace "$1/.github/workflows/ci.yaml" $'        working-directory: mod\n      - name: Report ci-ok' \
    $'        working-directory: mod\n      - name: Test what the live tag adds\n        run: go test -tags live -run TestLive ./lib\n        working-directory: mod\n      - name: Report ci-ok'
  replace "$1/.github/ci-gates/app.py" \
    '], reads=[Read(".github/workflows/ci.yaml", "a job reads its own definition")' \
    $'], exact=[\n    exact("Test what the live tag adds", "mod", "go test -tags live -run TestLive ./lib", [\n        run("mod", ["./lib"], at("app", "Test what the live tag adds") + " (its pinned script): go test -tags live ./lib",\n            tags="live"),\n    ]),\n], reads=[Read(".github/workflows/ci.yaml", "a job reads its own definition")'
  replace "$1/.github/scripts/ci_job_reads.py" 'TAG_RUNS = []' \
    'TAG_RUNS = [TagRun("app", "app", "Test what the live tag adds", "app", "app", "the app job runs ./lib untagged")]'
}

echo "case: a self-paired tag step's filter keeps its ! patterns"
root=$(fixture self-tag-run)
self_tag_run "$root"
unread_package "$root"
run_check "$root"
check "passes" "$(is "$status" 0)"
check "and holds the filter to what the tag step reads" \
  "$(contains "$out" '^report: app step "Test what the live tag adds": [0-9][0-9]* files read by app (filter app), 0 uncovered by app (filter app)$')"
run_generate "$root"
check "the filter keeps the line off the unread package's test" "$(contains "$(filter_of app)" '^!mod/lib/sub/\*\*/\*_test.go$')"

# Rules 19 and 21: the changes job's one dorny/paths-filter step hands the action the file the
# step before it computes, so every output of the job names that step and a computed filter, and
# no paths-filter step of any workflow holds filters of its own.

echo "case: an output naming a step the changes job does not have"
root=$(fixture output-absent-step)
replace "$root/.github/workflows/ci.yaml" '      image: ${{ steps.filter.outputs.image }}' \
  '      image: ${{ steps.absent.outputs.image }}'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the output and the step the job has not got" \
  "$(contains "$out" "::error file=.github/workflows/ci.yaml::the changes job's output image reads steps.absent.outputs.image, and the job has no step absent, so the output is empty on every run")"

echo "case: an output naming a step of the job that is no paths-filter step"
root=$(fixture output-other-step)
replace "$root/.github/workflows/ci.yaml" '      image: ${{ steps.filter.outputs.image }}' \
  '      image: ${{ steps.note.outputs.image }}'
replace "$root/.github/workflows/ci.yaml" "    steps:"$'\n'"      - run: .github/scripts/ci-filters-generate.sh" \
  $'    steps:\n      - id: note\n        run: echo note\n      - run: .github/scripts/ci-filters-generate.sh'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the output and says that step writes no filter's verdict" \
  "$(contains "$out" "::error file=.github/workflows/ci.yaml::the changes job's output image reads steps.note.outputs.image, and step note is no dorny/paths-filter step of the job, so it writes no filter's verdict and the output is empty on every run")"

echo "case: an output naming a filter no gate declares"
# Renaming the filter leaves the image job gated on a filter no file declares as well, which is
# its own refusal; what this case is about is the output that now reads nothing.
root=$(fixture output-unknown-filter)
replace "$root/.github/ci-gates/image.py" 'excluded("image",' 'excluded("picture",'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the output and says no gate declares the filter" \
  "$(contains "$out" "::error file=.github/workflows/ci.yaml::the changes job's output image reads steps.filter.outputs.image, and the generated filters (check-ci-filters.sh generate) hold no filter image, since no file of .github/ci-gates declares one, so the output is empty on every run")"

echo "case: a paths-filter step carrying filters of its own instead of the file's path"
# A filter written back into ci.yaml is read by no rule of this check: it would gate its job on
# patterns nothing here has seen, and the job's coverage would go with it.
root=$(fixture inline-filters)
replace "$root/.github/workflows/ci.yaml" '          filters: ${{ runner.temp }}/ci-filters.yaml' \
  $'          filters: |\n            app:\n              - \'mod/**\'\n            image:\n              - \'docs/**\'\n            site:\n              - \'docs/**\''
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the file, the step, its job and the path it must pass" \
  "$(contains "$out" "::error file=.github/workflows/ci.yaml::the paths-filter step filter of job changes passes a \`filters\` input that is not exactly \${{ runner.temp }}/ci-filters.yaml")"
check "says the filters there are computed by no rule" \
  "$(contains "$out" 'is computed by no rule of this check and gates its job on whatever it says, unread')"

echo "case: a paths-filter step of another workflow carrying filters of its own"
root=$(fixture inline-filters-elsewhere)
cat > "$root/.github/workflows/other.yaml" <<'YAML'
name: Other
on: [pull_request]
jobs:
  pick:
    runs-on: ubuntu-24.04
    steps:
      - uses: dorny/paths-filter@v4
        id: pick
        with:
          filters: |
            docs:
              - 'docs/**'
YAML
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names that workflow, its job and its step" \
  "$(contains "$out" "::error file=.github/workflows/other.yaml::the paths-filter step pick of job pick passes a \`filters\` input that is not exactly \${{ runner.temp }}/ci-filters.yaml")"

echo "case: two paths-filter steps reading the computed filters under different quantifiers"
root=$(fixture filters-two-quantifiers)
replace "$root/.github/workflows/ci.yaml" '          filters: ${{ runner.temp }}/ci-filters.yaml' \
  $'          filters: ${{ runner.temp }}/ci-filters.yaml\n      - uses: dorny/paths-filter@v4\n        id: again\n        with:\n          filters: ${{ runner.temp }}/ci-filters.yaml'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names both quantifiers" \
  "$(contains "$out" "::error file=.github/workflows/ci.yaml::the changes job's paths-filter steps read the generated filters (check-ci-filters.sh generate) under predicate-quantifier some and some-with-excludes, so one filter of it means two things")"

echo "case: the files a table lists from a directory are the repository's, not the disk's"
# ci_job_reads.py's files_but_tests lists a directory's files but its Go tests for the reads no
# glob can write. A local e2e run leaves build outputs git ignores in packages/envoy (the
# harness's lock, out/dispatch-e2e-broker); no checkout CI makes has them, and rule 9 refused the
# envoy_smoke gate's read of each as matching no file. So the list is repo_files(): the tracked
# files still on disk and the untracked ones git does not ignore, in the order a walk visits them.
root="$work/files-but-tests"
mkdir -p "$root/pkg/sub" "$root/pkg/testdata" "$root/pkg/out"
printf 'pkg/out/\n' > "$root/.gitignore"
for f in a.go a_test.go z.go notes.txt sub/b.go testdata/c.go gone.go out/build.go new.go; do
  printf 'package x\n' > "$root/pkg/$f"
done
git -C "$root" init -q
git -C "$root" add .gitignore pkg/a.go pkg/a_test.go pkg/z.go pkg/notes.txt pkg/sub/b.go pkg/testdata/c.go pkg/gone.go
rm "$root/pkg/gone.go"
# listed_files <root> <suffix> <tree|flat>: files_but_tests("pkg") run from <root>, as the check
# runs from the repository's, skipping testdata.
listed_files() { ask listed "$@"; }
check "lists the tracked and the new files, a directory's own before its subdirectory's" \
  "$(is "$(listed_files "$root" .go tree)" "pkg/a.go pkg/new.go pkg/z.go pkg/sub/b.go")"
check "and no ignored build output, test file, deleted file or skipped directory for any suffix" \
  "$(is "$(listed_files "$root" "" tree)" "pkg/a.go pkg/new.go pkg/notes.txt pkg/z.go pkg/sub/b.go")"
check "and without the tree, the directory's own files" \
  "$(is "$(listed_files "$root" .go flat)" "pkg/a.go pkg/new.go pkg/z.go")"
# ./pkg, pkg/ or an absolute path would match no path of repo_files(), which spells each relative
# and in normal form, so the reads would go missing with nothing failing: each is refused.
for spelling in ./pkg pkg/ "$root/pkg"; do
  refusal=$(ask refused "$root" "$spelling")
  check "refuses the directory spelled $spelling, naming the spelling" \
    "$(contains "$refusal" "files_but_tests is given '$spelling', which is not a path relative to the repository root in normal form")"
done

# The header's run recipe, run as the header writes it, from a fixture's root and over a module
# cache that holds no toolchain, with GOPROXY=off so that go can fetch none. Where the go PATH
# finds already is the version go.work names - the state setup-go or mise leaves when it installs
# that version itself - the recipe runs the check on that go. Where go cannot have that version,
# the recipe stops and go says why, rather than stopping without a word.
recipe=$(sed -n '/^#   v=/,/check-ci-filters\.sh$/s/^#   //p' "$script_dir/check-ci-filters.sh")
run_recipe() {
  set +e
  out=$(cd "$1" && env -u GOROOT GOMODCACHE="$1.modcache" GOPROXY=off bash -c "$recipe" 2>&1)
  status=$?
  set -e
}

echo "case: the header's recipe runs the check on the go PATH finds where that go is go.work's version"
check "the header holds a recipe that runs the check" "$(contains "$recipe" 'check-ci-filters\.sh$')"
root=$(fixture recipe-local-go)
version=$(cd "$root" && env -u GOROOT GOTOOLCHAIN=local go env GOVERSION)
printf 'go %s\n\nuse ./mod\n' "${version#go}" > "$root/go.work"
run_recipe "$root"
check "exits 0" "$(is "$status" 0)"
check "says what it checked" "$(contains "$out" 'every ci.yaml filter is computed from what its job reads (1 gates)')"

echo "case: the header's recipe says why it stops where go cannot have go.work's version"
root=$(fixture recipe-missing-go)
printf 'go 1.999.0\n\nuse ./mod\n' > "$root/go.work"
run_recipe "$root"
check "fails" "$(is "$status" 1)"
check "names the version it could not have" "$(contains "$out" 'go1\.999\.0')"

summary "check-ci-filters.sh computes each filter from its job's Go closure and reads, and holds its table to ci.yaml"
