#!/usr/bin/env bash
# The proof that the daemon of this change ships before the listener of this change: a hook prompt
# to a released role reaches the role's woken session when the daemon and the listener are both
# this checkout's, and is lost when the daemon is an earlier build's and the listener this
# checkout's. This checkout's listener refuses, with 404 and nothing kept, a role publish that names
# no source session while no live session holds the role, and the daemon's hook prompts name none.
# The daemon publishes a released role's prompt once its wake has launched the pod and before the
# woken session has claimed the role, so the publish meets that refusal. An earlier build's daemon
# ends the prompt's row at the 404, as it did when the listener kept the prompt and answered 202,
# so nothing reaches the woken session. This checkout's daemon keeps the row waiting on its backoff
# until the woken session holds the role, and delivers the prompt once.
#
#   LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/hook-prompt-two-builds.sh --base-commit <rev>
#
# --base-commit is required and names the commit of the earlier daemon: the daemon release
# before this change, legion-v23.21.0, or any later commit that still ends a hook prompt's row at
# the listener's 404. Its tree is taken with `git archive` into the run's own work directory, so
# the checkout is never touched. Every case runs the one Go test that drives a released role's
# hook prompts through a real daemon, a real listener binary, a real nats-server and a real
# PostgreSQL:
# TestIdleReleaseAndWake/TheHookPromptsOfAReleasedRoleReachItsWokenSessionOnce
# (packages/daemon/internal/daemon/idle_release_just_released_test.go). The test builds the
# listener from the packages/envoy of the checkout that LEGION_E2E_REPO_ROOT names, or else of the
# checkout it runs in (packages/daemon/internal/testbuild's RootEnv), so each case picks its
# listener with that variable alone.
#
# LEGION_TEST_PG_DSN is required and must name a loopback host: the test creates and drops
# databases there, and lib/loopback-dsn.sh's require_loopback_env refuses any other host with exit
# 2 before the run takes or builds anything. The test is skipped without it, and a skipped test is
# never read as a pass or a loss: each case reads the subtest's own `--- PASS` or `--- FAIL` line.
# The test starts its nats-server as a container, so docker must answer. Each case's `go test -v`
# output is kept in the work directory, under TMPDIR: removed when the run passes, kept and printed
# when it fails.
#
# The three cases, in the order the run makes them:
#
#   base-alone              the earlier daemon with the earlier listener: the base tree's test
#                           passes, so the base's rig works and the base's listener kept the
#                           prompt for the woken session. This is the control that the loss below
#                           is the listener's.
#   earlier-daemon-loses    the earlier daemon with this checkout's listener: the base tree's test
#                           fails, and its failure is the line that says no message reached the
#                           woken session. The test's earlier steps, which hold that both rows
#                           ended while the woken process was held before it registered, passed:
#                           the rows ended at the 404, and the prompts were lost.
#   this-build              this checkout's daemon with this checkout's listener: this checkout's
#                           test passes, so the rows waited and the woken session got each prompt
#                           once.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
started=$SECONDS
check=setup
subtest='TheHookPromptsOfAReleasedRoleReachItsWokenSessionOnce'
lost_line='timed out waiting for the message reaching the woken session'

begin() {
  check=$1
  echo "== $check"
}
note() { echo "   $*"; }
pass() { echo "ok $check"; }
fail() {
  echo "FAIL $check: $*" >&2
  exit 1
}
refuse() {
  echo "FAIL $check: $*" >&2
  exit 2
}

base_commit=''
base_given=false
while [ $# -gt 0 ]; do
  case "$1" in
    --base-commit)
      [ $# -ge 2 ] || refuse "--base-commit takes the commit of the earlier daemon"
      base_commit=$2
      base_given=true
      shift 2
      ;;
    --base-commit=*)
      base_commit=${1#--base-commit=}
      base_given=true
      shift
      ;;
    *) refuse "unknown argument '$1': this script takes --base-commit <rev> alone" ;;
  esac
done

# shellcheck source-path=SCRIPTDIR source=lib/run-verdict.sh
. "$root/scripts/e2e/lib/run-verdict.sh"
# shellcheck source-path=SCRIPTDIR source=lib/loopback-dsn.sh
. "$root/scripts/e2e/lib/loopback-dsn.sh"

work=''
cleanup() {
  local status=$?
  trap - EXIT
  set +e
  if [ -n "$work" ]; then
    if run_passed "$status"; then
      rm -rf -- "$work"
    else
      echo "the work directory is kept: $work" >&2
    fi
  fi
  run_verdict "$status" "hook prompt two builds e2e: PASS in $((SECONDS - started))s" \
    "hook prompt two builds e2e: FAIL at $check after $((SECONDS - started))s"
}
trap cleanup EXIT

# run_subtest TREE LISTENER LOG runs the subtest from TREE's packages/daemon with the listener
# built from LISTENER's packages/envoy, writing `go test -v`'s output to LOG, and prints the
# subtest's result:
#   - PASS: its `--- PASS` line and a `go test` that exited 0;
#   - FAIL: its `--- FAIL` line and a `go test` that exited non-zero;
#   - SKIP: a `--- SKIP` line for the subtest or its parent, since `go test` exits 0 for a test
#     that was skipped;
#   - UNKNOWN: anything else, such as a PASS line beside a non-zero exit (the parent test or the
#     binary failed after the subtest) or a run that printed no line of the subtest at all.
# No case takes SKIP or UNKNOWN as its result.
run_subtest() {
  local tree=$1 listener=$2 log=$3 status=0
  (
    cd "$tree/packages/daemon"
    LEGION_E2E_REPO_ROOT=$listener go test -v -count=1 -timeout 30m \
      -run "^TestIdleReleaseAndWake\$/^$subtest\$" ./internal/daemon/
  ) >"$log" 2>&1 || status=$?
  if grep -q -E -- "--- SKIP: TestIdleReleaseAndWake( |/$subtest )" "$log"; then
    echo SKIP
  elif [ "$status" -eq 0 ] && grep -q -- "--- PASS: TestIdleReleaseAndWake/$subtest " "$log"; then
    echo PASS
  elif [ "$status" -ne 0 ] && grep -q -- "--- FAIL: TestIdleReleaseAndWake/$subtest " "$log"; then
    echo FAIL
  else
    echo UNKNOWN
  fi
}

begin prerequisites
[ "$base_given" = true ] && [ -n "$base_commit" ] ||
  refuse "--base-commit is required: it names the commit of the earlier daemon, such as legion-v23.21.0"
[ -n "${LEGION_TEST_PG_DSN:-}" ] ||
  refuse "LEGION_TEST_PG_DSN is unset: it names the loopback PostgreSQL the test creates its databases on"
unset PGHOSTADDR PGSERVICE PGSERVICEFILE
db_host=''
require_loopback_env db_host LEGION_TEST_PG_DSN "this run creates and drops databases there" ||
  refuse "$db_host"
for tool in go git tar docker grep; do
  command -v "$tool" >/dev/null || refuse "no $tool on PATH"
done
docker info >/dev/null 2>&1 || refuse "docker does not answer: the test starts its nats-server as a container"
base_sha=$(git -C "$root" rev-parse --verify "$base_commit^{commit}" 2>/dev/null) ||
  refuse "--base-commit '$base_commit' names no commit of this repository"
note "PostgreSQL on $db_host; earlier daemon $base_sha; this checkout $(git -C "$root" rev-parse HEAD)"
pass

begin base-tree
work=$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-hook-prompt-two-builds.XXXXXXXX")
mkdir "$work/base"
git -C "$root" archive "$base_sha" | tar -x -C "$work/base" || fail "could not take the tree of $base_sha"
grep -q "\"$subtest\"" "$work/base/packages/daemon/internal/daemon/idle_release_and_wake_test.go" ||
  fail "the tree of $base_sha has no subtest $subtest to run"
note "work directory $work"
pass

begin base-alone
result=$(run_subtest "$work/base" "$work/base" "$work/base-alone.log")
note "the earlier daemon with the earlier listener: $result ($work/base-alone.log)"
[ "$result" = PASS ] || fail "the base tree's own run did not pass ($result), so the loss below would say nothing about the listener"
pass

begin earlier-daemon-loses
result=$(run_subtest "$work/base" "$root" "$work/earlier-daemon-loses.log")
note "the earlier daemon with this checkout's listener: $result ($work/earlier-daemon-loses.log)"
[ "$result" = FAIL ] || fail "the earlier daemon's run against this checkout's listener did not fail ($result)"
grep -q -- "$lost_line" "$work/earlier-daemon-loses.log" ||
  fail "the earlier daemon's run failed, but not on the line that says no message reached the woken session"
note "$(grep -m1 -- "$lost_line" "$work/earlier-daemon-loses.log" | sed 's/^[[:space:]]*//')"
pass

begin this-build
result=$(run_subtest "$root" "$root" "$work/this-build.log")
note "this checkout's daemon with this checkout's listener: $result ($work/this-build.log)"
[ "$result" = PASS ] || fail "this checkout's run did not pass ($result)"
pass
run_end
