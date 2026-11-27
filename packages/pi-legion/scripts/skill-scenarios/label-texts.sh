#!/usr/bin/env bash
# label-texts.sh: the one entry the skill-scenario rig calls for a label's own texts. Every Legion
# text an agent of the rig gets comes from the label's own code, built in the label's own tree:
# five tests of the label's daemon module. Three of them each read a request and write an answer,
# one runs the label's whole daemon in the rig's own case, and one runs it for a scored run.
# This script lives in the label's checkout and runs that checkout's tests. Its request and answer
# files are the shapes of scored-runs-contracts.ts (C2 and C8).
#
#   label-texts.sh build [--compile]
#       Compiles the three test binaries with `go test -c -tags e2e`, once for each state of the
#       label's tree (its commit, and the digest of its changes when it has any), into
#       $RIG_TEXTS_BIN/<commit>[-<digest>]/. The tag puts TestRigDaemonLive into the daemon
#       binary. Then it runs the own case of TestRigWorld, TestRigLaunch, TestRigPlanCheck and
#       TestRigDaemon, with no request, and prints the four `--- PASS:` lines. With --compile it
#       makes the binaries and runs no test.
#   label-texts.sh world <request> <answer>        runs TestRigWorld      (internal/workflow)
#   label-texts.sh launch <request> <answer>       runs TestRigLaunch     (internal/daemon)
#   label-texts.sh plan-check <request> <answer>   runs TestRigPlanCheck  (internal/api)
#       Each runs the built binary of its test with -test.run '^<test>$' and -test.v, with the
#       request's and the answer's absolute paths in RIG_TEXTS_REQUEST and RIG_TEXTS_ANSWER. A test
#       binary exits 0 when -test.run selects no test, so the run passes only with the test's own
#       `--- PASS: <test>` line and an answer file the run wrote. Each test refuses a request field
#       it does not handle, naming the field.
#   label-texts.sh daemon <request> <ready> <status> <record>
#       Runs TestRigDaemonLive (internal/daemon) the same way, with the four absolute paths in
#       RIG_TEXTS_REQUEST, RIG_TEXTS_READY, RIG_TEXTS_STATUS and RIG_TEXTS_RECORD. It also sets
#       LEGION_TEST_PG_DSN to the request's postgresUrl, because the binary makes its template
#       database on that server before the test reads the request. The test serves until it gets
#       SIGTERM, which this script passes on to it. The run passes only with the test's own
#       `--- PASS: TestRigDaemonLive` line and the three files.
#
# RIG_TEXTS_BIN, which every verb needs, is the directory the binaries are built in: an absolute
# path outside the label's checkout, so that the build leaves the checkout as it was (git lists
# nothing new, and jj snapshots nothing). The rig gives each label a directory of its own under its
# work directory (rig.sh load_label), as it builds every other output there.
#
# Exit codes:
#   0  the verb did what it says.
#   1  a run failed: the compile, a test that failed, a binary that printed no `--- PASS: <test>`
#      line, a run that wrote no answer file or one of the daemon's three files, or a binary that
#      build has not made.
#   2  a usage error, a label that lacks one of the five test files (each is named), a
#      RIG_TEXTS_BIN that is unset, not absolute, or inside the label's checkout, or a daemon
#      request that names no postgresUrl.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
label=$(cd "$here/../../../.." && pwd)
daemon="$label/packages/daemon"

usage() {
  echo "usage: label-texts.sh build [--compile] | world|launch|plan-check <request> <answer> | daemon <request> <ready> <status> <record>" >&2
  exit 2
}

# Each test of the label: its package under packages/daemon/internal, its file and its name.
tests=(
  "workflow rig_world_test.go TestRigWorld"
  "daemon rig_launch_test.go TestRigLaunch"
  "api rig_plan_test.go TestRigPlanCheck"
  "daemon rig_daemon_test.go TestRigDaemon"
  "daemon rig_daemon_live_test.go TestRigDaemonLive"
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

# require_binary prints the built binary of package pkg for this label's tree, and exits 1 when
# build has not made it.
require_binary() {
  local binary="$out/$1.test"
  if [ ! -x "$binary" ]; then
    echo "label-texts: there is no $binary for this label's tree: run label-texts.sh build first" >&2
    exit 1
  fi
  echo "$binary"
}

# require_pass exits 1 unless output, a binary's whole output, holds name's own `--- PASS:` line.
require_pass() {
  local name=$1 binary=$2 output=$3
  if ! grep -qE "^--- PASS: $name \(" <<<"$output"; then
    echo "label-texts: $binary printed no '--- PASS: $name' line, so it ran no $name" >&2
    exit 1
  fi
}

# run_test <package> <test> [<request> <answer>] runs one built binary in its package's directory,
# as go test runs a test binary, and exits 1 unless the test ran and passed.
run_test() {
  local pkg=$1 name=$2 request=${3:-} answer=${4:-}
  local binary output status=0
  binary=$(require_binary "$pkg")
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
  require_pass "$name" "$binary" "$output"
  if [ -n "$answer" ] && [ ! -s "$answer" ]; then
    echo "label-texts: $name passed and wrote no answer file $answer" >&2
    exit 1
  fi
  grep -E "^--- PASS: $name \(" <<<"$output"
}

# run_daemon <request> <ready> <status> <record> runs the built daemon binary's TestRigDaemonLive
# in its package's directory until it ends, passing SIGTERM and SIGINT on to it, and exits 1
# unless it passed and wrote the three files.
run_daemon() {
  local request=$1 ready=$2 status_file=$3 record=$4
  local binary postgres log pid status=0
  binary=$(require_binary daemon)
  if ! postgres=$(jq -er '.postgresUrl | strings | select(. != "")' "$request" 2>/dev/null); then
    echo "label-texts: the daemon request $request names no postgresUrl" >&2
    exit 2
  fi
  rm -f -- "$ready" "$status_file" "$record"
  log=$(mktemp)
  (cd "$daemon/internal/daemon" &&
    RIG_TEXTS_REQUEST=$request RIG_TEXTS_READY=$ready RIG_TEXTS_STATUS=$status_file RIG_TEXTS_RECORD=$record \
      LEGION_TEST_PG_DSN=$postgres exec "$binary" -test.run '^TestRigDaemonLive$' -test.v) >"$log" 2>&1 &
  pid=$!
  # A wait that a trapped signal cuts short returns at once, and the next wait still returns the
  # binary's own status, once it has ended.
  trap 'kill -TERM "$pid" 2>/dev/null || true' TERM INT
  while kill -0 "$pid" 2>/dev/null; do
    wait "$pid" || true
  done
  wait "$pid" || status=$?
  trap - TERM INT
  local output
  output=$(<"$log")
  rm -f -- "$log"
  printf '%s\n' "$output" >&2
  if [ "$status" -ne 0 ]; then
    echo "label-texts: TestRigDaemonLive failed: $binary exited $status" >&2
    exit 1
  fi
  require_pass TestRigDaemonLive "$binary" "$output"
  local kind file
  for kind in ready status record; do
    case $kind in
      ready) file=$ready ;;
      status) file=$status_file ;;
      record) file=$record ;;
    esac
    if [ ! -s "$file" ]; then
      echo "label-texts: TestRigDaemonLive passed and wrote no $kind file $file" >&2
      exit 1
    fi
  done
  grep -E "^--- PASS: TestRigDaemonLive \(" <<<"$output"
}

# compile <dir> compiles the three test binaries into <dir>, in the label's daemon module, with the
# e2e tag, which TestRigDaemonLive's file carries; the workflow and api packages hold no file the
# tag adds. The go command stands alone on its line because the label-texts.sh row of
# .github/ci-gates/pi-legion.py names it. CI's filter check fails that row when the script no
# longer holds its text; when the line holding it holds more than the text alone, or continues, the
# check reads every Go file of the repository for the row (ci_filter_coverage.py, check_table). It
# reads no go command a script runs that no row names.
compile() {
  local bin=$1
  cd "$daemon" || return 1
  go test -c -tags e2e -o "$bin/" ./internal/workflow ./internal/daemon ./internal/api
}

# build [--compile] makes the binaries of this label's tree once, then runs each own case unless
# it is told to compile alone.
build() {
  local only_compile=$1
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
  [ "$only_compile" = no ] || return 0
  local entry pkg name
  for entry in "${tests[@]}"; do
    read -r pkg _ name <<<"$entry"
    # TestRigDaemonLive has no own case: with no request it fails, naming RIG_TEXTS_REQUEST.
    [ "$name" = TestRigDaemonLive ] || run_test "$pkg" "$name"
  done
}

# existing_parent exits 2 unless the directory that would hold file, one of the verb's outputs,
# exists.
existing_parent() {
  if [ ! -d "$(dirname -- "$1")" ]; then
    echo "label-texts: no directory for the $2 file $1" >&2
    exit 2
  fi
}

[ "$#" -ge 1 ] || usage
verb=$1
shift
case $verb in
  build)
    case "$#:${1:-}" in
      0:) only_compile=no ;;
      1:--compile) only_compile=yes ;;
      *) usage ;;
    esac
    require_tests
    require_bin
    build "$only_compile"
    ;;
  world | launch | plan-check)
    [ "$#" -eq 2 ] || usage
    require_tests
    require_bin
    if [ ! -f "$1" ]; then
      echo "label-texts: no request file $1" >&2
      exit 2
    fi
    existing_parent "$2" answer
    request=$(realpath -- "$1")
    answer=$(realpath -m -- "$2")
    out="$bin_root/$(tree_key)"
    case $verb in
      world) run_test workflow TestRigWorld "$request" "$answer" ;;
      launch) run_test daemon TestRigLaunch "$request" "$answer" ;;
      plan-check) run_test api TestRigPlanCheck "$request" "$answer" ;;
    esac
    ;;
  daemon)
    [ "$#" -eq 4 ] || usage
    require_tests
    require_bin
    if [ ! -f "$1" ]; then
      echo "label-texts: no request file $1" >&2
      exit 2
    fi
    existing_parent "$2" ready
    existing_parent "$3" status
    existing_parent "$4" record
    out="$bin_root/$(tree_key)"
    run_daemon "$(realpath -- "$1")" "$(realpath -m -- "$2")" "$(realpath -m -- "$3")" "$(realpath -m -- "$4")"
    ;;
  *) usage ;;
esac
