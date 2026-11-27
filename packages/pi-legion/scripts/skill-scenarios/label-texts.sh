#!/usr/bin/env bash
# label-texts.sh: the one entry the skill-scenario rig calls for a label's own texts. Every Legion
# text an agent of the rig gets comes from the label's own code, built in the label's own tree:
# three tests of the label's daemon module, each of which reads a request and writes an answer.
# This script lives in the label's checkout and runs that checkout's tests. Its request and answer
# files are the shapes of scored-runs-contracts.ts (C2).
#
#   label-texts.sh build
#       Compiles the three test binaries with `go test -c`, once for each state of the label's tree
#       (its commit, and the digest of its changes when it has any), into
#       $RIG_TEXTS_BIN/<commit>[-<digest>]/, then runs each test's own case, with no request.
#       It prints the three `--- PASS:` lines.
#   label-texts.sh world <request> <answer>        runs TestRigWorld      (internal/workflow)
#   label-texts.sh launch <request> <answer>       runs TestRigLaunch     (internal/daemon)
#   label-texts.sh plan-check <request> <answer>   runs TestRigPlanCheck  (internal/api)
#       Each runs the built binary of its test with -test.run '^<test>$' and -test.v, with the
#       request's and the answer's absolute paths in RIG_TEXTS_REQUEST and RIG_TEXTS_ANSWER. A test
#       binary exits 0 when -test.run selects no test, so the run passes only with the test's own
#       `--- PASS: <test>` line and an answer file the run wrote. Each test refuses a request field
#       it does not handle, naming the field.
#
# RIG_TEXTS_BIN, which every verb needs, is the directory the binaries are built in: an absolute
# path outside the label's checkout, so that the build leaves the checkout as it was (git lists
# nothing new, and jj snapshots nothing). The rig gives each label a directory of its own under its
# work directory (rig.sh load_label), as it builds every other output there.
#
# Exit codes:
#   0  the verb did what it says.
#   1  a run failed: the compile, a test that failed, a binary that printed no `--- PASS: <test>`
#      line, a run that wrote no answer file, or a binary that build has not made.
#   2  a usage error, a label that lacks one of the three test files (each is named), or a
#      RIG_TEXTS_BIN that is unset, not absolute, or inside the label's checkout.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
label=$(cd "$here/../../../.." && pwd)
daemon="$label/packages/daemon"

usage() {
  echo "usage: label-texts.sh build | world|launch|plan-check <request> <answer>" >&2
  exit 2
}

# Each test of the label: its package under packages/daemon/internal, its file and its name.
tests=(
  "workflow rig_world_test.go TestRigWorld"
  "daemon rig_launch_test.go TestRigLaunch"
  "api rig_plan_test.go TestRigPlanCheck"
)

# require_tests exits 2 naming each test file the label lacks.
require_tests() {
  local entry pkg file name missing=0
  for entry in "${tests[@]}"; do
    read -r pkg file name <<<"$entry"
    if [ ! -f "$daemon/internal/$pkg/$file" ]; then
      echo "label-texts: this label lacks packages/daemon/internal/$pkg/$file, which holds $name" >&2
      missing=1
    fi
  done
  [ "$missing" -eq 0 ] || exit 2
}

# require_bin sets bin_root to RIG_TEXTS_BIN, and exits 2 unless it names an absolute path outside
# the label's checkout.
require_bin() {
  local given=${RIG_TEXTS_BIN:-} real top
  if [ -z "$given" ]; then
    echo "label-texts: RIG_TEXTS_BIN is unset: name the directory, outside this label's checkout, that the three test binaries are built in" >&2
    exit 2
  fi
  case $given in
    /*) ;;
    *)
      echo "label-texts: RIG_TEXTS_BIN $given is not an absolute path" >&2
      exit 2
      ;;
  esac
  real=$(realpath -m -- "$given")
  top=$(cd "$label" && pwd -P)
  case "$real/" in
    "$top"/*)
      echo "label-texts: RIG_TEXTS_BIN $given is inside the label's checkout $label, where git would list the binaries and jj would snapshot them" >&2
      exit 2
      ;;
  esac
  bin_root=$real
}

# tree_key prints the state of the label's tree that its binaries are built from: its commit, and
# when the tree has changes (a control's label tree has), a digest of them, untracked files
# included.
tree_key() {
  local commit changes
  if ! commit=$(git -C "$label" rev-parse --verify -q HEAD); then
    echo "label-texts: $label is not a git checkout with a commit" >&2
    exit 1
  fi
  changes=$(git -C "$label" status --porcelain --untracked-files=all)
  if [ -z "$changes" ]; then
    echo "$commit"
    return
  fi
  local digest
  digest=$(
    cd "$label"
    git diff HEAD --binary
    git ls-files -z --others --exclude-standard | xargs -0 -r sha256sum --
  )
  digest=$(printf '%s' "$digest" | sha256sum)
  echo "$commit-${digest:0:12}"
}

# run_test <package> <test> [<request> <answer>] runs one built binary in its package's directory,
# as go test runs a test binary, and exits 1 unless the test ran and passed.
run_test() {
  local pkg=$1 name=$2 request=${3:-} answer=${4:-}
  local binary="$out/$pkg.test" output status=0
  if [ ! -x "$binary" ]; then
    echo "label-texts: there is no $binary for this label's tree: run label-texts.sh build first" >&2
    exit 1
  fi
  if [ -n "$request" ]; then
    rm -f -- "$answer"
    output=$(cd "$daemon/internal/$pkg" &&
      RIG_TEXTS_REQUEST=$request RIG_TEXTS_ANSWER=$answer "$binary" -test.run "^$name\$" -test.v 2>&1) || status=$?
  else
    output=$(cd "$daemon/internal/$pkg" &&
      env -u RIG_TEXTS_REQUEST -u RIG_TEXTS_ANSWER "$binary" -test.run "^$name\$" -test.v 2>&1) || status=$?
  fi
  printf '%s\n' "$output" >&2
  if [ "$status" -ne 0 ]; then
    echo "label-texts: $name failed: $binary exited $status" >&2
    exit 1
  fi
  if ! grep -qE "^--- PASS: $name \(" <<<"$output"; then
    echo "label-texts: $binary printed no '--- PASS: $name' line, so it ran no $name" >&2
    exit 1
  fi
  if [ -n "$answer" ] && [ ! -s "$answer" ]; then
    echo "label-texts: $name passed and wrote no answer file $answer" >&2
    exit 1
  fi
  grep -E "^--- PASS: $name \(" <<<"$output"
}

# compile <dir> compiles the three test binaries into <dir>, in the label's daemon module. The go
# command stands alone on its line because the label-texts.sh row of .github/ci-gates/pi-legion.py
# names it. CI's filter check fails that row when the script no longer holds its text; when the
# line holding it holds more than the text alone, or continues, the check reads every Go file of
# the repository for the row (ci_filter_coverage.py, check_table). It reads no go command a script
# runs that no row names.
compile() {
  local bin=$1
  cd "$daemon" || return 1
  go test -c -o "$bin/" ./internal/workflow ./internal/daemon ./internal/api
}

build() {
  mkdir -p "$bin_root"
  out="$bin_root/$(tree_key)"
  if [ ! -f "$out/built" ]; then
    local bin
    bin=$(mktemp -d "$bin_root/build.XXXXXX")
    if ! (compile "$bin"); then
      rm -rf -- "$bin"
      echo "label-texts: go test -c of the three packages failed in $daemon" >&2
      exit 1
    fi
    touch "$bin/built"
    # Two builds of one tree at one time: the first to finish keeps its directory.
    mv -T -- "$bin" "$out" 2>/dev/null || rm -rf -- "$bin"
  fi
  local entry pkg name
  for entry in "${tests[@]}"; do
    read -r pkg _ name <<<"$entry"
    run_test "$pkg" "$name"
  done
}

[ "$#" -ge 1 ] || usage
verb=$1
shift
case $verb in
  build)
    [ "$#" -eq 0 ] || usage
    require_tests
    require_bin
    build
    ;;
  world | launch | plan-check)
    [ "$#" -eq 2 ] || usage
    require_tests
    require_bin
    if [ ! -f "$1" ]; then
      echo "label-texts: no request file $1" >&2
      exit 2
    fi
    if [ ! -d "$(dirname -- "$2")" ]; then
      echo "label-texts: no directory for the answer file $2" >&2
      exit 2
    fi
    request=$(realpath -- "$1")
    answer=$(realpath -m -- "$2")
    out="$bin_root/$(tree_key)"
    case $verb in
      world) run_test workflow TestRigWorld "$request" "$answer" ;;
      launch) run_test daemon TestRigLaunch "$request" "$answer" ;;
      plan-check) run_test api TestRigPlanCheck "$request" "$answer" ;;
    esac
    ;;
  *) usage ;;
esac
