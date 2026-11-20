#!/usr/bin/env bash
# Tests for `check-go-offline.sh`, over trees it writes. Every form the check's header names has a
# case here, one planted file each: for its first rule, a Go, a TypeScript, a Python and a shell
# test that run `go list -deps` with no offline rule are refused and named with a GitHub
# annotation, and the offline form and the reasoned form of each pass, over a count that shows
# the scan saw the command; then each other form of the go binary, of a site and of a launcher
# before go is planted bare and refused (every subcommand, every spawn and exec name, each import
# alias, each binding of a name, each shell a script is handed to, a file named with no shell
# extension that runs one), each environment the header says passes passes, and each one it says
# counts for nothing is refused (a neighbour's, a comment's, a loose string's, one a later
# statement or a spread of other options undoes, an export in another function, in a pipeline or
# in the background); what starts nothing starts nothing (a comment, a quoted command, a heredoc's
# body, a skipped directory, a file with no shebang); a reason six lines up is too far, and a
# Python file that does not parse is refused. A site an entry of .github/go-offline-reasons.yaml
# names passes as reasoned; an entry whose command changed, and a malformed one, are refused, and
# so is a site of a listed file the list does not name. The header's limits are not planted: they
# are what the check does not read. Its file reason: counted within the first 40 lines and not on
# line 41;
# refused in a test file, in a file ci.yaml or a workflow ci.yaml calls names, and in one a step of
# those reaches (through another script, as a program a script hands to python3, or as TypeScript
# under a test run's directory), but not in one only a workflow outside that set or a comment
# names or reaches. Its second rule: a job of any workflow that
# runs internal/daemon's tests, which build envoy commands through testbuild.EnvoyBinary, is
# refused when no step before that one runs `.github/scripts/go-fetch.sh deps packages/envoy` of
# every such command - a step with none, one after, one that leaves a command out, one in another
# job - and passes when one does; the tests a step runs through a script and the library it
# sources, after a cd, with -C, or through bash -c count; a compile to /dev/null and a listing run
# no test. Its other half: a step that reaches an offline go build, install, run, list or
# generate - in a script it runs, in a function a sourced library defines and the script calls
# with its module and packages, or in a script the TypeScript of a Playwright run names - or a
# goCommand a bun test run reaches, is refused with no fetch of those packages before it, or one
# that leaves a package out, and passes after one (a deps command, a tools file of the pin, or a
# fetch file's deps or tools line), or with a reason in its run or within five lines above its
# name. Its third rule: a required job's fetch step (deps, tools or a fetch file), in ci.yaml or a
# workflow it calls, that comes before a restore of ~/go/pkg/mod, or after a restore of another
# path, is refused, and one whose restore (actions/cache/restore or actions/cache) comes first
# passes and is listed. Its rules 4 and 5, which it calls from go_offline_jobs.py and
# package_installs.py: a required Go job with no GOPROXY=off, and a bare apt-get install in a
# required job, each fail the check naming their line, and an UNKNOWN of either is the check's. Its
# rule 6 (go_offline_images.py): a bare go RUN in a Dockerfile a required job builds is refused at
# its line, the mounted offline RUN passes, and a Dockerfile only another workflow builds passes,
# with the other forms that rule's header names. Its rule 7 (go_offline_fetched.py): a go step of a
# required Go job whose packages, tests, tags or platform no deps line of a fetch before it holds,
# or which it cannot read, is refused unless a reason names its fetch; a /... pattern ends at a
# nested module; a step that goes online on purpose is not read. A tree with no file of a language,
# a tree with testbuild and no caller, and a tree with no ci.yaml each give UNKNOWN and exit 2.
#
# The cases live in the files this one sources, one part of the check each:
# check-go-offline-cases-go.sh, -typescript.sh, -python.sh and -shell.sh (rule 1 in each
# language), -reasons.sh (its file reasons), -steps.sh (rule 2), -restores.sh (rule 3), -jobs.sh
# (rules 4 and 5), -images.sh (rule 6) and -fetched.sh (rule 7). This file holds what they share,
# the fixtures, and the cases of a scan that cannot see.
#
# Each case builds a miniature tree under a temporary directory and symlinks the real script into
# it, so the script under test is the file CI runs: it resolves its root from its own path, which
# the symlink puts at the fixture root. No go command of a fixture runs: the script reads text.
# It needs a python3 with PyYAML, as the script does.
#
# Run from anywhere: .github/scripts/check-go-offline.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check_script="$script_dir/check-go-offline.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# put <root> <path>: writes this harness's stdin to <path> under <root>, making its directory.
put() {
  mkdir -p "$(dirname "$1/$2")"
  cat >"$1/$2"
}

# fixture <name>: a tree the script can scan: the script linked in, a ci.yaml with one job, which
# sets up Go and runs with GOPROXY=off and GOFLAGS=-mod=readonly (so rule 4 reads a Go job), and
# one file of each of the four languages that starts no go command. Echoes its root.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts"
  ln -s "$check_script" "$root/.github/scripts/check-go-offline.sh"
  put "$root" .github/workflows/ci.yaml <<'EOF'
name: CI
on: pull_request
env:
  GOPROXY: "off"
  GOFLAGS: -mod=readonly
jobs:
  lint:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/setup-go@v5
      - run: echo lint
EOF
  printf 'package app\n\nfunc App() {}\n' | put "$root" packages/app/app.go
  printf 'export const app = 1;\n' | put "$root" packages/web/app.ts
  printf 'print("app")\n' | put "$root" tools/app.py
  printf '#!/usr/bin/env bash\necho app\n' | put "$root" tools/app.sh
  echo "$root"
}

# daemon_fixture <name>: a fixture with testbuild and a daemon test that builds two envoy commands
# through it. Echoes its root.
daemon_fixture() {
  local root
  root=$(fixture "$1")
  printf 'package testbuild\n\nfunc EnvoyBinary(t any, pkg string) string { return pkg }\n' |
    put "$root" packages/daemon/internal/testbuild/testbuild.go
  put "$root" packages/daemon/internal/daemon/daemon_test.go <<'EOF'
package daemon

import (
	"testing"

	"example.com/daemon/internal/testbuild"
)

func TestBuildsTheCommands(t *testing.T) {
	_ = testbuild.EnvoyBinary(t, "./cmd/dispatch")
	_ = testbuild.EnvoyBinary(t, "./cmd/listener")
}
EOF
  mkdir -p "$root/packages/envoy"
  echo "$root"
}

# nightly <root>: a workflow outside the required set holding one job, from this harness's stdin
# (the job's steps).
nightly() {
  {
    printf 'name: Nightly\non: schedule\njobs:\n  proof:\n    runs-on: ubuntu-24.04\n    steps:\n'
    cat
  } | put "$1" .github/workflows/nightly.yaml
}

run_check() { # run_check <root>
  set +e
  out=$("$1/.github/scripts/check-go-offline.sh" 2>&1)
  status=$?
  set -e
}

refused() { # refused <path> <line>: the checks of a site refused with its annotation
  check "fails" "$(is "$status" 1)"
  check "names $1:$2" "$(contains "$out" "$1:$2: a go command with no GOPROXY=off and no go-offline reason")"
  check "annotates the line" "$(contains "$out" "::error file=$1,line=$2::")"
}

# refuses <name> <path> <line> [<sites>]: a fixture holding this harness's stdin at <path> is
# refused at <line> and nowhere else, over <sites> go commands (1 when not given).
refuses() {
  local root
  root=$(fixture "$1")
  put "$root" "$2"
  run_check "$root"
  refused "$2" "$3"
  check "refuses it alone, over ${4:-1} go commands" "$(contains "$out" "^1 problems over the ${4:-1} go commands")"
}

# passes_offline <name> <path>: a fixture holding this harness's stdin at <path> passes, its one go
# command read as offline.
passes_offline() {
  local root
  root=$(fixture "$1")
  put "$root" "$2"
  run_check "$root"
  check "exits 0" "$(is "$status" 0)"
  check "reads its one command as offline" "$(contains "$out" 'every one of the 1 go commands .* runs with GOPROXY=off (1)')"
}

# --- The cases, each file one part of the check ------------------------------------------------
# Each file this sources holds the cases of one part: rule 1's sites in each language and its file
# reasons, rule 2's steps, rule 3's restores, rules 4 and 5's calls, rule 6's image builds and rule
# 7's fetches. They share this file's fixtures and counters.
for part in go typescript python shell reasons steps restores jobs images fetched; do
  # shellcheck source=/dev/null
  source "$script_dir/check-go-offline-cases-$part.sh"
done

# --- A scan that cannot see -----------------------------------------------------------------------

echo "case: a tree with no Python file gives UNKNOWN"
root=$(fixture blind-python)
rm "$root/tools/app.py"
run_check "$root"
check "exits 2" "$(is "$status" 2)"
check "names the language it did not read" "$(contains "$out" '^UNKNOWN: read no py file under ')"

echo "case: a tree with testbuild and no test that calls it gives UNKNOWN"
root=$(daemon_fixture blind-testbuild)
rm "$root/packages/daemon/internal/daemon/daemon_test.go"
run_check "$root"
check "exits 2" "$(is "$status" 2)"
check "names testbuild" "$(contains "$out" '^UNKNOWN: packages/daemon/internal/testbuild/testbuild.go exists and no daemon test calls testbuild.EnvoyBinary')"

echo "case: a tree whose tests call testbuild and no step runs them gives UNKNOWN"
root=$(daemon_fixture blind-jobs)
run_check "$root"
check "exits 2" "$(is "$status" 2)"
check "names the callers no step was seen to run" \
  "$(contains "$out" '^UNKNOWN: packages/daemon/internal/daemon call testbuild.EnvoyBinary and no step of any workflow was seen to run their tests')"

echo "case: a tree with no ci.yaml gives UNKNOWN"
root=$(fixture blind-ci)
rm "$root/.github/workflows/ci.yaml"
run_check "$root"
check "exits 2" "$(is "$status" 2)"
check "says no workflow is known to be required" "$(contains "$out" '^UNKNOWN: no .github/workflows/ci.yaml here')"

summary "check-go-offline.sh"
