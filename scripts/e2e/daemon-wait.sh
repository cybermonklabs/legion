#!/usr/bin/env bash
# Check w1 of this change's verification: a worker waits for the daemon (R1). The whole daemon runs
# on the rig of this change's checks (packages/daemon/internal/daemon/RIG.md): the listener
# production pins, built from its own commit, a real nats-server with production's grants, a
# PostgreSQL of the run's own, and real `omp --mode rpc` panes under the real `legion worker-shim`,
# each with this checkout's @sjawhar/pi-envoy and @sjawhar/pi-legion, whose turns go to a scripted
# model stand-in so the first worker's handoff_complete is made on cue during each outage. The
# driver is packages/daemon/internal/daemon/w1_daemon_wait_live_test.go; each of its two steps
# prints `== <step>`, what it observed and read, with the times, and `ok <step>`, or
# `FAIL <step> at <assertion>`. Every observation is also appended, with the time it was written,
# to w1-results.log in the evidence directory.
#
#   step1   the daemon stopped for 55 minutes with its port closed, at a second claim's hello_ack
#           and before its claims/register; the first worker (a merger) calls handoff_complete in
#           the outage; a fourth claim's held foreground `bash` call is the stall watch's control.
#           From the daemon's stop until just before its restart, the port is reserved: bound by a
#           socket that never listens, so a connect is refused and no other process can listen on it
#   step2   the daemon stopped for 55 minutes with its port held open and answering nothing, when
#           it has answered a third claim's claims/register, so its claims/ready meets the outage;
#           the first worker calls handoff_complete in the outage
#
# After each restart the step waits past the registration deadline and reads `legion claims list
# --json`, the first issue's record, and each claim's charges where the daemon's log records them
# (a ready clears the launch failures the counters show). The two outages take about two and a
# half hours of wall clock; run the script detached with its output in a file.
#
# Run it from a checkout on a Linux host as
#   LEGION_OMP_PATH=<omp> LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     LEGION_E2E_MODEL_BASE_URL=<anthropic-messages endpoint> LEGION_E2E_MODEL_KEY_COMMAND=<command> \
#     bash scripts/e2e/daemon-wait.sh [--base-commit <rev>]
# lib/live-rig.sh names every input and what it refuses; it asks for the model's base URL and key
# though this driver's panes talk to its own stand-ins. LEGION_E2E_W1_JJ names the jj the merger's
# pane runs (by default mise's jj where mise has one, else PATH's): it must run under a HOME of its
# own, as a pane's does. DAEMON_WAIT_EVIDENCE_DIR (default a fresh directory under TMPDIR, kept and
# printed) keeps the run's evidence. LEGION_E2E_W1_OUTAGE shortens both outages for a rehearsal of
# the steps, which the driver never passes. It needs no docker, no tmux and no kube context.
#
# --base-commit <rev> is the check's negative control: step 1 at the base. A detached worktree of
# <rev> is made under TMPDIR (removed on any exit), this checkout's rig — the testenvoy package, the
# rig's three driver files, this check's driver and lib/live-rig-grants.go — is laid over it, `bun
# install --frozen-lockfile` gives it node_modules of its own, and the same run is made there with
# step 1 alone: the daemon, `legion` and both plugins, pi-shared among them, are the base's. The
# control passes when step 1 fails at completion-after-start, where the step says that the
# handoff_complete call was answered during the outage with the base's "stayed so for 10 minutes",
# and that the daemon's log charges the second claim a launch failure from the outage. Its last
# line is `daemon wait negative control: PASS|FAIL …` with the base run's evidence directory.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
base_commit=''
base_given=false
while [ $# -gt 0 ]; do
  case "$1" in
    --base-commit)
      [ $# -ge 2 ] || {
        echo "usage: $0 [--base-commit <rev>]" >&2
        exit 2
      }
      base_commit=$2
      base_given=true
      shift 2
      ;;
    *)
      echo "usage: $0 [--base-commit <rev>]" >&2
      exit 2
      ;;
  esac
done
if [ "$base_given" = true ] && [ -z "$base_commit" ]; then
  echo "--base-commit names no commit" >&2
  exit 2
fi
# shellcheck source-path=SCRIPTDIR source=lib/live-rig.sh
. "$root/scripts/e2e/lib/live-rig.sh"

test_name=TestAWorkerWaitsForTheDaemonLive
step1='step1-a-closed-port-outage-is-waited-out-by-a-completion-and-by-a-start-before-its-register'
# What the control lays over the base's tree: the rig and this check's driver.
rig_files=(
  packages/daemon/internal/testenvoy
  packages/daemon/internal/daemon/rig_live_test.go
  packages/daemon/internal/daemon/rig_nats_live_test.go
  packages/daemon/internal/daemon/rig_runtime_live_test.go
  packages/daemon/internal/daemon/w1_daemon_wait_live_test.go
  scripts/e2e/lib/live-rig-grants.go
)

# The jj the merger's pane runs: mise's own binary where mise has one, since PATH's jj there can be
# a shim that finds no jj under a pane's HOME; it must answer --version under a HOME of its own.
jj_bin=${LEGION_E2E_W1_JJ:-}
if [ -z "$jj_bin" ] && command -v mise >/dev/null 2>&1; then
  jj_bin=$(mise which jj 2>/dev/null || true)
fi
[ -n "$jj_bin" ] || jj_bin=$(command -v jj || true)
jj_home=$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-daemon-wait-jj-home.XXXXXXXX")
if [ -z "$jj_bin" ] || ! HOME=$jj_home PATH=/usr/bin:/bin "$jj_bin" --version >/dev/null 2>&1; then
  rm -rf "$jj_home"
  echo "daemon wait e2e: no jj that runs under a HOME of its own (LEGION_E2E_W1_JJ='${LEGION_E2E_W1_JJ:-}', found '${jj_bin}')" >&2
  exit 2
fi
rm -rf "$jj_home"
export LEGION_E2E_W1_JJ=$jj_bin
# Two 55-minute outages, each with its restart and the registration deadline after it.
export LIVE_RIG_TEST_TIMEOUT=${LIVE_RIG_TEST_TIMEOUT:-4h}

# The negative control's own state, global because its EXIT trap reads it.
base_work=''
base_tree=''
base_evidence=''

control_cleanup() {
  local status=$?
  if [ -n "$base_tree" ]; then
    git -C "$root" worktree remove --force "$base_tree" >/dev/null 2>&1 || true
  fi
  [ -z "$base_work" ] || rm -rf "$base_work"
  if [ "$status" -eq 0 ]; then
    echo "daemon wait negative control: PASS in $((SECONDS - started))s (evidence: $base_evidence)"
  else
    echo "daemon wait negative control: FAIL at $check after $((SECONDS - started))s (evidence: $base_evidence)" >&2
  fi
}

negative_control() {
  local base_sha status file line detail
  started=$SECONDS
  base_evidence=${DAEMON_WAIT_EVIDENCE_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-daemon-wait-base-evidence.XXXXXXXX")}
  mkdir -p "$base_evidence"
  trap control_cleanup EXIT
  transcript_to "$base_evidence/negative-control.log"

  begin base-tree
  base_sha=$(git -C "$root" rev-parse --verify "$base_commit^{commit}" 2>/dev/null) ||
    refuse "--base-commit '$base_commit' names no commit of this repository"
  base_work=$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-daemon-wait-base.XXXXXXXX")
  base_tree=$base_work/tree
  git -C "$root" worktree add --detach "$base_tree" "$base_sha" >"$base_evidence/base-tree.log" 2>&1 ||
    fail "could not make a worktree of $base_sha; see $base_evidence/base-tree.log"
  for file in "${rig_files[@]}"; do
    rm -rf "${base_tree:?}/$file"
    mkdir -p "$(dirname "$base_tree/$file")"
    cp -R "$root/$file" "$base_tree/$file" || fail "could not lay $file over the tree of $base_sha"
  done
  (cd "$base_tree" && bun install --frozen-lockfile) >>"$base_evidence/base-tree.log" 2>&1 ||
    fail "bun install in the tree of $base_sha failed; see $base_evidence/base-tree.log"
  note "a worktree of $base_sha in $base_tree, with this checkout's rig and driver laid over it: ${rig_files[*]}"
  note "the base's pi-shared DAEMON_RESTART_BUDGET_MS: $(grep -m1 'export const DAEMON_RESTART_BUDGET_MS' "$base_tree/packages/pi-shared/src/daemon-client.ts")"
  pass

  set +e
  (
    set -e
    root=$base_tree
    live_rig_proof "daemon wait at ${base_sha:0:12}" daemon-wait-base "^$test_name\$/^$step1\$" "$base_evidence" fail
  )
  status=$?
  set -e
  if [ "$status" -eq 2 ]; then
    check=base-run
    exit 2
  fi

  begin negative-control
  grep -q "^== $step1\$" "$base_evidence/proof.log" 2>/dev/null ||
    fail "the run over $base_sha reached no step 1: it failed before its proof; see $base_evidence/transcript.log"
  line=$(grep -m1 -E "^(ok|FAIL) $step1( |$)" "$base_evidence/proof.log") ||
    fail "step 1 over $base_sha came to no outcome; see $base_evidence/proof.log"
  case "$line" in
    "FAIL $step1 at completion-after-start ("*) ;;
    "ok $step1 "*) fail "step 1 passed over $base_sha, so this check tells it from this checkout in nothing" ;;
    *) fail "step 1 failed over $base_sha elsewhere than at completion-after-start: $line" ;;
  esac
  detail=$(grep -m1 -E 'w1_daemon_wait_live_test\.go:[0-9]+: completion-after-start: ' "$base_evidence/proof.log") ||
    fail "step 1's failure over $base_sha carries no completion-after-start sentence; see $base_evidence/proof.log"
  case "$detail" in
    *"was answered during the outage"*"stayed so for 10 minutes"*"no-launch-failure: the daemon's log charges "*"-2-implementer a launch failure"*) ;;
    *) fail "step 1's sentence over $base_sha does not say that the call failed in the outage after the base's 10 minutes and that the daemon's log charges the second claim a launch failure: $detail" ;;
  esac
  note "$line"
  note "${detail#"${detail%%[![:space:]]*}"}"
  pass
}

if [ -z "$base_commit" ]; then
  live_rig_proof 'daemon wait' daemon-wait "^$test_name\$" "${DAEMON_WAIT_EVIDENCE_DIR:-}"
else
  negative_control
fi
