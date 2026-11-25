#!/usr/bin/env bash
# Tests for `check-legion-release-paths.sh`: every tracked file each package of this repository
# the `legion` binary links compiles or embeds, for each target .github/go-release-targets.json
# lists for it, the go.mod and go.sum of its module, and the go.work its build reads with its
# go.work.sum, is covered by release.yaml's LEGION_VERSION_ARGS, read as the release reads it:
# expanded by bash at the root, parsed by release-push.sh, matched by git. The case that matters
# is the one the check exists for: a package the binary links that the list does not cover, on the
# runner's own target or on another one.
#
# Each case builds a miniature repository under a temporary directory, a git repository whose
# every file is added to the index before the check runs, with a daemon module whose `replace`
# reaches an envoy module as the real one does. It symlinks the real script, release-push.sh and
# release-build-legion.sh into it, so the scripts under test are the files CI and the release run
# and the `go list` and `git ls-files` they run are real ones. Needs Go, jq and git.
#
# Run from anywhere: .github/scripts/check-legion-release-paths.test.sh
# CI runs it in ci.yaml's test job, after setup-go.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check_script="$script_dir/check-legion-release-paths.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# The paths of a list that covers the fixture's whole binary: what fixture writes after `--`.
covering='packages/daemon/ packages/envoy/used/ packages/envoy/deep/ packages/envoy/go.mod packages/envoy/go.sum'

# fixture <name>: a tree that passes. Its legion command links the daemon's own helper package and
# the envoy module's used, which links deep; envoy's unused is linked by nothing. Its target list
# names a linux and a darwin target for legion, as the real one does. Echoes its root.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/.github/workflows" \
    "$root/packages/daemon/cmd/legion" "$root/packages/daemon/internal/helper" \
    "$root/packages/envoy/used" "$root/packages/envoy/deep" "$root/packages/envoy/unused"
  ln -s "$check_script" "$root/.github/scripts/check-legion-release-paths.sh"
  ln -s "$script_dir/release-push.sh" "$root/.github/scripts/release-push.sh"
  ln -s "$script_dir/release-build-legion.sh" "$root/.github/scripts/release-build-legion.sh"
  printf 'module github.com/example/daemon\n\ngo 1.22\n\nrequire github.com/example/envoy v0.0.0\n\nreplace github.com/example/envoy => ../envoy\n' \
    > "$root/packages/daemon/go.mod"
  printf 'module github.com/example/envoy\n\ngo 1.22\n' > "$root/packages/envoy/go.mod"
  : > "$root/packages/daemon/go.sum"
  : > "$root/packages/envoy/go.sum"
  cat > "$root/packages/daemon/cmd/legion/main.go" <<'GO'
package main

import (
	"github.com/example/daemon/internal/helper"
	"github.com/example/envoy/used"
)

func main() { helper.Help(); used.Use() }
GO
  printf 'package helper\n\nfunc Help() {}\n' > "$root/packages/daemon/internal/helper/helper.go"
  printf 'package used\n\nimport "github.com/example/envoy/deep"\n\nfunc Use() { deep.Deep() }\n' \
    > "$root/packages/envoy/used/used.go"
  printf 'package deep\n\nfunc Deep() {}\n' > "$root/packages/envoy/deep/deep.go"
  printf 'package unused\n\nfunc Unused() {}\n' > "$root/packages/envoy/unused/unused.go"
  printf '{\n  "legion": ["linux/amd64", "darwin/arm64"],\n  "envoy": ["amd64"]\n}\n' > "$root/.github/go-release-targets.json"
  release "$root" "$covering"
  git init -q -b main "$root"
  echo "$root"
}

# release <root> <paths>: release.yaml with a LEGION_VERSION_ARGS of one --patch-path and <paths>
# after its `--`, and the version step that reads it, as the real one runs it.
release() {
  printf 'name: Release\non: push\nenv:\n  LEGION_VERSION_ARGS: >-\n    --patch-path packages/daemon/internal/prompts/roles/\n    -- %s\n%s' \
    "$2" "$version_job" > "$1/.github/workflows/release.yaml"
}

# step_job <run line>...: the job of release.yaml that runs legion's version step, with <run
# line>s as that step's `run:`, so a case can change the step's commands and nothing else.
step_job() {
  local line body=''
  for line in "$@"; do
    body+="          $line"$'\n'
  done
  printf 'jobs:\n  legion_build:\n    runs-on: ubuntu-latest\n    steps:\n      - name: Determine next version\n        id: version\n        run: |\n%s' "$body"
}
# The version step's one command, and its job, as the real ones run it.
# shellcheck disable=SC2016 # the step's text, which bash expands in the step and not here
step_command='.github/scripts/release-push.sh version legion-v $LEGION_VERSION_ARGS >> "$GITHUB_OUTPUT"'
version_job="$(step_job "$step_command")"$'\n'

# gowork <root>: a go.work at the root that binds the two modules, as the real one does, and an
# empty go.work.sum beside it.
gowork() {
  printf 'go 1.22\n\nuse (\n\t./packages/daemon\n\t./packages/envoy\n)\n' > "$1/go.work"
  : > "$1/go.work.sum"
}

# run_check <root>: adds every file of <root> to its index, as a commit would track them, and runs
# the check. Leaves its exit code in status and its output in out.
run_check() {
  local root=$1
  git -C "$root" add -A
  set +e
  out=$("$root/.github/scripts/check-legion-release-paths.sh" 2>&1)
  status=$?
  set -e
}

echo "case: the list covers every package the binary links and both modules' files"
root=$(fixture green)
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "says what it compared" \
  "$(contains "$out" "covers every tracked file that the 4 packages of this repository go list -deps ./cmd/legion prints for the 2 legion targets of .github/go-release-targets.json compile or embed, and the go.mod and go.sum of the 2 modules the build uses")"

echo "case: a package the binary links that the list does not name"
root=$(fixture planted)
release "$root" 'packages/daemon/ packages/envoy/used/ packages/envoy/go.mod packages/envoy/go.sum'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the package and the fix" \
  "$(contains "$out" "LEGION_VERSION_ARGS names no path that covers 1 of what the legion binary is built from, so a change to it alone would release no legion: packages/envoy/deep/. Add each after the")"
check "names no package the binary does not link" "$(is "$(contains "$out" 'packages/envoy/unused')" false)"

echo "case: a package the binary comes to link through a new import"
root=$(fixture new-import)
sed -i 's|"github.com/example/envoy/used"|"github.com/example/envoy/unused"\n\t"github.com/example/envoy/used"|; s|used.Use() }|used.Use(); unused.Unused() }|' \
  "$root/packages/daemon/cmd/legion/main.go"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the package the import brought in" "$(contains "$out" 'would release no legion: packages/envoy/unused/\.')"

echo "case: a package the binary links only when it is built for another target"
# The release builds legion for each target of the list, and a file a build constraint keeps to
# one of them brings in its imports there alone: darwin.go links unused only into the darwin
# binary, which no runner of this check is.
root=$(fixture other-target)
printf '//go:build darwin\n\npackage main\n\nimport _ "github.com/example/envoy/unused"\n' \
  > "$root/packages/daemon/cmd/legion/darwin.go"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the package only the darwin binary links" "$(contains "$out" 'would release no legion: packages/envoy/unused/\.')"

echo "case: a module file the list does not name"
root=$(fixture module-file)
release "$root" 'packages/daemon/ packages/envoy/used/ packages/envoy/deep/ packages/envoy/go.mod'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the go.sum" "$(contains "$out" 'would release no legion: packages/envoy/go.sum\.')"

echo "case: a go.work binds the modules, and the list does not name it"
# The release builds in workspace mode when a go.work is above packages/daemon, as the real one
# is, and the go.work's own lines (a go or godebug line, a use, a replace) set what the binary
# builds with, so a change to it alone must release one, as a change to a module's go.mod does.
root=$(fixture gowork-unnamed)
gowork "$root"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the go.work and its sum" "$(contains "$out" 'would release no legion: go.work, go.work.sum\.')"

echo "case: a go.work and its sum the list names"
root=$(fixture gowork-named)
gowork "$root"
release "$root" "$covering go.work go.work.sum"
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "says it compared the go.work" "$(contains "$out" 'and go.work and go.work.sum, which bind them')"

echo "case: a go.work the list names, beside a go.work.sum it does not"
root=$(fixture gowork-sum-unnamed)
gowork "$root"
release "$root" "$covering go.work"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the go.work.sum alone" "$(contains "$out" 'covers 1 of what the legion binary is built from, so a change to it alone would release no legion: go.work.sum\.')"

echo "case: a go.work outside the repository"
# GOWORK may name one anywhere; a change to a file outside the repository is no commit the list
# can name, so the check cannot hold it to the list and refuses it.
root=$(fixture gowork-outside)
mkdir -p "$work/gowork-outside-parent"
printf 'go 1.22\n\nuse (\n\t%s/packages/daemon\n\t%s/packages/envoy\n)\n' "$root" "$root" > "$work/gowork-outside-parent/go.work"
GOWORK="$work/gowork-outside-parent/go.work" run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "says it is outside the repository" "$(contains "$out" 'is outside the repository')"

echo "case: a module the workspace uses that links nothing into legion, whose go.mod the list does not name"
# In workspace mode the require lines of every module go.work uses feed one version selection, so
# a change to such a module's go.mod alone can change what legion links.
root=$(fixture gowork-other-module)
gowork "$root"
mkdir -p "$root/packages/other"
printf 'module github.com/example/other\n\ngo 1.22\n' > "$root/packages/other/go.mod"
: > "$root/packages/other/go.sum"
printf 'package other\n' > "$root/packages/other/other.go"
printf 'go 1.22\n\nuse (\n\t./packages/daemon\n\t./packages/envoy\n\t./packages/other\n)\n' > "$root/go.work"
release "$root" "$covering go.work go.work.sum"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names its go.mod and go.sum" "$(contains "$out" 'would release no legion: packages/other/go.mod, packages/other/go.sum\.')"

echo "case: a module the workspace uses that is outside the repository"
# go.work may use a module anywhere; a change to its go.mod is no commit the list can name, so the
# check cannot hold it to the list and refuses it.
root=$(fixture gowork-module-outside)
gowork "$root"
mkdir -p "$work/gowork-module-outside-parent/other"
printf 'module github.com/example/other\n\ngo 1.22\n' > "$work/gowork-module-outside-parent/other/go.mod"
printf 'go 1.22\n\nuse (\n\t./packages/daemon\n\t./packages/envoy\n\t%s\n)\n' "$work/gowork-module-outside-parent/other" > "$root/go.work"
release "$root" "$covering go.work go.work.sum"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "says the module is outside the repository" "$(contains "$out" "gowork-module-outside-parent/other/go.mod the legion build's go.work uses is outside the repository")"

echo "case: the version step reads the list as the check does not"
# The check expands LEGION_VERSION_ARGS as the step that runs release-push.sh version legion-v
# does: from the workflow's env, unquoted in bash, at the root, with the command alone in its
# run:. A job or step env that sets the variable, a working directory or a shell other than bash
# at the step, its job's defaults or the workflow's, another command in the step, and no such step
# or two make the release read another list.
root=$(fixture job-env)
version_job="${version_job/    runs-on: ubuntu-latest/    runs-on: ubuntu-latest
    env:
      LEGION_VERSION_ARGS: -- packages/}" release "$root" "$covering"
run_check "$root"
check "a job env that sets it: exits 1" "$(is "$status" 1)"
check "says so" "$(contains "$out" "legion version step (job legion_build) sets LEGION_VERSION_ARGS in its job's env")"
root=$(fixture step-env)
version_job="${version_job/        id: version/        id: version
        env:
          LEGION_VERSION_ARGS: -- packages/}" release "$root" "$covering"
run_check "$root"
check "a step env that sets it: exits 1" "$(is "$status" 1)"
check "says so" "$(contains "$out" "legion version step (job legion_build) sets LEGION_VERSION_ARGS in its env")"
root=$(fixture step-directory)
version_job="${version_job/        id: version/        id: version
        working-directory: packages}" release "$root" "$covering"
run_check "$root"
check "a working directory: exits 1" "$(is "$status" 1)"
check "says so" "$(contains "$out" "legion version step (job legion_build) sets its working-directory (packages)")"
root=$(fixture job-default-directory)
version_job="${version_job/    runs-on: ubuntu-latest/    runs-on: ubuntu-latest
    defaults:
      run:
        working-directory: packages}" release "$root" "$covering"
run_check "$root"
check "a job's default working directory: exits 1" "$(is "$status" 1)"
check "says so" "$(contains "$out" "legion version step (job legion_build) sets its job's default working-directory (packages)")"
root=$(fixture workflow-default-directory)
version_job="defaults:
  run:
    working-directory: packages
$version_job" release "$root" "$covering"
run_check "$root"
check "the workflow's default working directory: exits 1" "$(is "$status" 1)"
check "says so" "$(contains "$out" "legion version step (job legion_build) sets the workflow's default working-directory (packages)")"
root=$(fixture step-shell)
version_job="${version_job/        id: version/        id: version
        shell: sh}" release "$root" "$covering"
run_check "$root"
check "a step shell other than bash: exits 1" "$(is "$status" 1)"
check "says so" "$(contains "$out" "legion version step (job legion_build) runs in sh")"
root=$(fixture job-default-shell)
version_job="${version_job/    runs-on: ubuntu-latest/    runs-on: ubuntu-latest
    defaults:
      run:
        shell: sh}" release "$root" "$covering"
run_check "$root"
check "a job's default shell other than bash: exits 1" "$(is "$status" 1)"
check "says so" "$(contains "$out" "legion version step (job legion_build) runs in sh")"
root=$(fixture workflow-default-shell)
version_job="defaults:
  run:
    shell: sh
$version_job" release "$root" "$covering"
run_check "$root"
check "the workflow's default shell other than bash: exits 1" "$(is "$status" 1)"
check "says so" "$(contains "$out" "legion version step (job legion_build) runs in sh")"
root=$(fixture step-cd)
version_job=$(step_job "cd packages; ../$step_command") release "$root" "$covering"
run_check "$root"
check "a cd before the command: exits 1" "$(is "$status" 1)"
check "names the command" "$(contains "$out" 'legion version step (job legion_build) runs `cd packages; \.\./\.github/scripts/release-push\.sh version legion-v \$LEGION_VERSION_ARGS >> "\$GITHUB_OUTPUT"`')"
root=$(fixture step-assignment)
version_job=$(step_job 'LEGION_VERSION_ARGS="-- packages/other/"' "$step_command") release "$root" "$covering"
run_check "$root"
check "an assignment before the command: exits 1" "$(is "$status" 1)"
check "names the assignment" "$(contains "$out" 'legion version step (job legion_build) runs `LEGION_VERSION_ARGS="-- packages/other/"`')"
root=$(fixture step-noglob)
version_job=$(step_job 'set -f' "$step_command") release "$root" "$covering"
run_check "$root"
check "a set -f before the command: exits 1" "$(is "$status" 1)"
check "names it" "$(contains "$out" 'legion version step (job legion_build) runs `set -f`')"
root=$(fixture step-comment)
version_job=$(step_job '# a comment and a blank line are no commands' '' "$step_command") release "$root" "$covering"
run_check "$root"
check "a comment and a blank line beside the command: exits 0" "$(is "$status" 0)"

echo "case: no step before the version step, and no env, changes the environment it reads the list in"
# GitHub gives a later step the environment an earlier step writes to $GITHUB_ENV or $GITHUB_PATH,
# by any name for them, and bash reads a startup file or its options from BASH_ENV, ENV, SHELLOPTS
# and BASHOPTS. So a step with a run: before the version step is refused whatever it runs, and
# each of those names is refused in the workflow's, the job's and the step's env.
before_lines=(
  "echo 'LEGION_VERSION_ARGS=-- packages/' >> \"\$GITHUB_ENV\""
  "echo 'LEGION_VERSION_ARGS=-- packages/' >> \"\${GITHUB_ENV}\""
  "echo 'LEGION_VERSION_ARGS=-- packages/' | tee -a \"\$GITHUB_ENV\""
  "echo 'LEGION_VERSION_ARGS=-- packages/' >> \"\${{ github.env }}\""
  "echo \"\$RUNNER_TEMP/bin\" >> \"\$GITHUB_PATH\""
  "echo 'this writes nothing for a later step'"
)
for i in "${!before_lines[@]}"; do
  root=$(fixture "before-$i")
  version_job="${version_job/    steps:/    steps:
      - name: Before $i
        run: |
          ${before_lines[$i]}}" release "$root" "$covering"
  run_check "$root"
  check "a step before it that runs ${before_lines[$i]}: exits 1" "$(is "$status" 1)"
  check "names the step" "$(contains "$out" 'legion version step (job legion_build) comes after the step "Before '"$i"'", which runs a command')"
done
# env_at <root> <where> <name> <value>: release.yaml at <root> with <name>: <value> in the env of
# <where>: the workflow, the job or the step.
env_at() {
  local file="$1/.github/workflows/release.yaml"
  case $2 in
    workflow) sed -i "s|^env:\$|env:\n  $3: $4|" "$file" ;;
    job) sed -i "s|^    runs-on: ubuntu-latest\$|    runs-on: ubuntu-latest\n    env:\n      $3: $4|" "$file" ;;
    step) sed -i "s|^        id: version\$|        id: version\n        env:\n          $3: $4|" "$file" ;;
  esac
}
for row in "workflow BASH_ENV ./set-f.sh the workflow's env" "job BASH_ENV ./set-f.sh its job's env" \
  "step BASH_ENV ./set-f.sh its env" "job ENV ./set-f.sh its job's env" \
  "workflow SHELLOPTS noglob the workflow's env" "step BASHOPTS nullglob its env"; do
  read -r where name value scope <<<"$row"
  root=$(fixture "env-$where-$name")
  env_at "$root" "$where" "$name" "$value"
  run_check "$root"
  check "$name in $scope: exits 1" "$(is "$status" 1)"
  check "says so" "$(contains "$out" "legion version step (job legion_build) sets $name in $scope")"
done
root=$(fixture env-other)
env_at "$root" workflow GOPROXY direct
env_at "$root" job CGO_ENABLED 0
run_check "$root"
check "another name in the workflow's and the job's env: exits 0" "$(is "$status" 0)"

root=$(fixture step-two)
version_job="$version_job  legion_again:
    runs-on: ubuntu-latest
    steps:
      - run: $step_command
" release "$root" "$covering"
run_check "$root"
check "two such steps: exits 1" "$(is "$status" 1)"
check "says so" "$(contains "$out" 'has 2 steps that run `.github/scripts/release-push.sh version legion-v \$LEGION_VERSION_ARGS`, want one')"
root=$(fixture step-none)
version_job='jobs: {}
' release "$root" "$covering"
run_check "$root"
check "no such step: exits 1" "$(is "$status" 1)"
check "says so" "$(contains "$out" 'has 0 steps that run `.github/scripts/release-push.sh version legion-v \$LEGION_VERSION_ARGS`, want one')"

echo "case: a directory covers every package below it"
root=$(fixture directory)
release "$root" 'packages/daemon/ packages/envoy/'
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: a glob that names every package below a directory"
# The release's bash expands packages/envoy/* at the repository's root to each entry of
# packages/envoy, and git reads each of those as covering every file below it.
root=$(fixture glob-envoy)
release "$root" 'packages/daemon/ packages/envoy/*'
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: a glob git matches across directories, which bash leaves as it is"
# No entry of packages/envoy matches *.go, so the release's bash passes the word on unchanged,
# and git's pathspec, whose * matches a /, reads it as every .go file below packages/envoy.
root=$(fixture glob-git)
release "$root" 'packages/daemon/ packages/envoy/*.go packages/envoy/go.mod packages/envoy/go.sum'
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: a glob bash expands where git alone would match nothing"
# git reads packages/envoy/[du]*/ as a pattern no file's path matches, since no file's path ends
# in a /. The release's bash expands the word first, at the repository's root, to deep/, unused/
# and used/ under packages/envoy, and git reads those.
root=$(fixture glob-bash)
release "$root" 'packages/daemon/ packages/envoy/[du]*/ packages/envoy/go.mod packages/envoy/go.sum'
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: an exclude whose glob takes a linked package back out"
# git reads the exclude's first * as matching a, so it takes every file under
# packages/envoy/a/internal/ out of the list, and the binary links that package through a.
root=$(fixture exclude-glob)
mkdir -p "$root/packages/envoy/a/internal"
printf 'package internal\n\nfunc F() {}\n' > "$root/packages/envoy/a/internal/f.go"
printf 'package a\n\nimport "github.com/example/envoy/a/internal"\n\nfunc A() { internal.F() }\n' > "$root/packages/envoy/a/a.go"
sed -i 's|"github.com/example/envoy/used"|"github.com/example/envoy/a"\n\t"github.com/example/envoy/used"|; s|used.Use() }|used.Use(); a.A() }|' \
  "$root/packages/daemon/cmd/legion/main.go"
release "$root" 'packages/daemon/ packages/envoy/ :(exclude)packages/envoy/*/internal/*'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the package the exclude takes out" "$(contains "$out" 'would release no legion: packages/envoy/a/internal/\.')"

echo "case: a file of a linked package that git does not track is not required"
# Go compiles packages/envoy/deep/gen.go, but git does not track it, so no commit can change it
# and no path of the list can release a change to it: the list may leave it out.
root=$(fixture untracked)
printf 'package deep\n\nfunc gen() {}\n' > "$root/packages/envoy/deep/gen.go"
mkdir -p "$root/.git/info"
printf 'packages/envoy/deep/gen.go\n' >> "$root/.git/info/exclude"
release "$root" 'packages/daemon/ packages/envoy/used/ packages/envoy/deep/deep.go packages/envoy/go.mod packages/envoy/go.sum'
run_check "$root"
check "exits 0" "$(is "$status" 0)"
# The same file tracked is one a commit can change, and the list leaves it out.
root=$(fixture tracked)
printf 'package deep\n\nfunc gen() {}\n' > "$root/packages/envoy/deep/gen.go"
release "$root" 'packages/daemon/ packages/envoy/used/ packages/envoy/deep/deep.go packages/envoy/go.mod packages/envoy/go.sum'
run_check "$root"
check "the same file tracked: exits 1" "$(is "$status" 1)"
check "the same file tracked: names its package" "$(contains "$out" 'would release no legion: packages/envoy/deep/\.')"

echo "case: a file of a linked package that no target compiles is not required"
# A README, a test and a file only windows compiles are in the package's directory, but no
# legion target's build reads them, so a list naming the compiled file alone covers the package.
root=$(fixture not-compiled)
printf 'deep does nothing.\n' > "$root/packages/envoy/deep/README.md"
printf 'package deep\n\nimport "testing"\n\nfunc TestDeep(t *testing.T) { Deep() }\n' > "$root/packages/envoy/deep/deep_test.go"
printf 'package deep\n\nfunc windowsOnly() {}\n' > "$root/packages/envoy/deep/deep_windows.go"
release "$root" 'packages/daemon/ packages/envoy/used/ packages/envoy/deep/deep.go packages/envoy/go.mod packages/envoy/go.sum'
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: a file a linked package embeds is required"
# used embeds prompt.md, which is part of the binary as its .go file is.
root=$(fixture embedded)
printf 'use it.\n' > "$root/packages/envoy/used/prompt.md"
cat > "$root/packages/envoy/used/used.go" <<'GO'
package used

import (
	_ "embed"

	"github.com/example/envoy/deep"
)

//go:embed prompt.md
var prompt string

func Use() { deep.Deep(); _ = prompt }
GO
release "$root" 'packages/daemon/ packages/envoy/used/used.go packages/envoy/deep/ packages/envoy/go.mod packages/envoy/go.sum'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the package that embeds it" "$(contains "$out" 'would release no legion: packages/envoy/used/\.')"
release "$root" 'packages/daemon/ packages/envoy/used/used.go packages/envoy/used/prompt.md packages/envoy/deep/ packages/envoy/go.mod packages/envoy/go.sum'
run_check "$root"
check "the embedded file named too: exits 0" "$(is "$status" 0)"

echo "case: no list, or no paths in it"
root=$(fixture no-list)
printf 'name: Release\non: push\nenv:\n  OTHER: x\njobs: {}\n' > "$root/.github/workflows/release.yaml"
run_check "$root"
check "exits 1 with no LEGION_VERSION_ARGS" "$(is "$status" 1)"
check "says so" "$(contains "$out" 'has no env LEGION_VERSION_ARGS')"

root=$(fixture no-dashes)
printf 'name: Release\non: push\nenv:\n  LEGION_VERSION_ARGS: packages/daemon/\n%s' "$version_job" > "$root/.github/workflows/release.yaml"
run_check "$root"
check "exits 1 with no --" "$(is "$status" 1)"
check "says release-push.sh refuses the words, as the release would" \
  "$(is "$(contains "$out" "^::error file=.github/workflows/release.yaml::release-push.sh refused .github/workflows/release.yaml's LEGION_VERSION_ARGS, as the release's version step would")|$(contains "$out" '^::error::release-push.sh version-paths: unknown option packages/daemon/; usage: ')" 'true|true')"

echo "case: a target list that names no legion target, or none at all"
root=$(fixture no-targets)
printf '{\n  "legion": [],\n  "envoy": ["amd64"]\n}\n' > "$root/.github/go-release-targets.json"
run_check "$root"
check "exits 1 with an empty legion list" "$(is "$status" 1)"
check "says so, as release-push.sh legion-targets does" "$(contains "$out" 'release-push.sh legion-targets: .github/go-release-targets.json lists no legion target')"

root=$(fixture no-legion-key)
printf '{\n  "envoy": ["amd64"]\n}\n' > "$root/.github/go-release-targets.json"
run_check "$root"
check "exits 1 with no legion list" "$(is "$status" 1)"
check "says it could not read the targets" "$(contains "$out" 'could not read the legion targets of .github/go-release-targets.json (release-push.sh legion-targets)')"

root=$(fixture no-target-list)
rm "$root/.github/go-release-targets.json"
run_check "$root"
check "exits 1 with no target list" "$(is "$status" 1)"
check "says it could not read it" "$(contains "$out" 'could not read the legion targets of .github/go-release-targets.json')"

echo "case: a target that is not <os>/<arch>"
# go builds an empty GOOS or GOARCH for the host's own and refuses a padded one only when it
# builds, so release-push.sh legion-targets refuses both, and lint with it, by name.
root=$(fixture target-empty)
printf '{\n  "legion": ["linux/amd64", ""],\n  "envoy": ["amd64"]\n}\n' > "$root/.github/go-release-targets.json"
run_check "$root"
check "an empty target: exits 1" "$(is "$status" 1)"
check "an empty target: names it" "$(contains "$out" 'release-push.sh legion-targets: the legion target "" is not <os>/<arch>')"
root=$(fixture target-padded)
printf '{\n  "legion": ["linux/amd64", " darwin/arm64"],\n  "envoy": ["amd64"]\n}\n' > "$root/.github/go-release-targets.json"
run_check "$root"
check "a padded target: exits 1" "$(is "$status" 1)"
check "a padded target: names it" "$(contains "$out" 'release-push.sh legion-targets: the legion target " darwin/arm64" is not <os>/<arch>')"

echo "case: a listing longer than one environment variable can hold"
# 500 packages with 245-character directory names make a go list output of about 180 KiB, more
# than the 128 KiB one environment variable holds: the check handed its listing over in one once,
# and CI's runner refused the python with "Argument list too long".
root=$(fixture long-listing)
pad=$(printf '%0240d' 0)
imports=""
for i in $(seq 1 500); do
  mkdir -p "$root/packages/envoy/p${i}_$pad"
  printf 'package p%s\n' "$i" > "$root/packages/envoy/p${i}_$pad/p.go"
  imports+=$'\t_ "github.com/example/envoy/p'"${i}_$pad"$'"\n'
done
printf 'package main\n\nimport (\n%s)\n\nfunc main() {}\n' "$imports" > "$root/packages/daemon/cmd/legion/main.go"
release "$root" 'packages/daemon/ packages/envoy/'
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts every package" "$(contains "$out" 'covers every tracked file that the 501 packages of this repository')"

echo "case: go list fails"
root=$(fixture broken)
printf 'package helper\n\nimport "github.com/example/daemon/internal/gone"\n\nfunc Help() { gone.Gone() }\n' \
  > "$root/packages/daemon/internal/helper/helper.go"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "says the packages are unknown" "$(contains "$out" 'go list -deps ./cmd/legion failed in packages/daemon, so the packages the legion binary links are unknown')"
check "names the target it failed for" "$(contains "$out" 'are unknown (target linux/amd64)')"

echo "case: go list printed no package of this repository"
root=$(fixture outside)
mv "$root/packages" "$work/outside-packages"
ln -s "$work/outside-packages" "$root/packages"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "says it compared nothing" "$(contains "$out" 'printed no package of this repository, so this check compared nothing')"

summary "check-legion-release-paths.sh"
