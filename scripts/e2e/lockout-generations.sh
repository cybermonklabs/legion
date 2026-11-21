#!/usr/bin/env bash
# Check w3 of this change's verification: the current process of a claim takes its worker stream
# back from an older one, which is told to stop (R3). The whole daemon runs on the rig of this
# change's checks (packages/daemon/internal/daemon/RIG.md): the listener production pins, built from
# its own commit, a real nats-server with production's grants, a PostgreSQL of the run's own, and
# three real `omp --mode rpc` panes under the real `legion worker-shim`, each with this checkout's
# @sjawhar/pi-envoy and @sjawhar/pi-legion, whose turns go to the model the run names. The driver is
# packages/daemon/internal/daemon/lockout_live_test.go; each of its six steps prints `== <step>`,
# what it observed and read, with the times, and `ok <step>`, or `FAIL <step> at <assertion>`.
#
#   step1   A reported gone while it runs: A is sent the stop frame at B's first hello and ends
#           within its grace, B binds once A's connection closes and answers get_state, and A's
#           close is no close of the claim's (no read of its process)
#   step2   a second shim C of B's generation is refused, its line naming both generations
#   step3   an older shim A2 that reads the stop frame and does not stop: 10 minutes refused, no
#           launch failure, no newer generation, one controller notice and one R6 line at 5 minutes
#   step4   A2's connection cut from the daemon's side: the newer binds, A2's redial is refused as an
#           older generation, told, and ends its process
#   step5   the same with a daemon restart between the cut and the redial
#   step6   a boot token the daemon never minted: refused, told nothing, the shim keeps running
#
# Run it from a checkout on a Linux host as
#   LEGION_OMP_PATH=<omp> LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     LEGION_E2E_MODEL_BASE_URL=<anthropic-messages endpoint> LEGION_E2E_MODEL_KEY_COMMAND=<command> \
#     bash scripts/e2e/lockout-generations.sh [--base-commit <rev>]
# On a development host the model is the shared gateway, https://model-gateway.<tailnet>.ts.net,
# and its key command one that prints the gateway key; in an issue pod it is the pod's
# ANTHROPIC_BASE_URL and a file holding its key (LEGION_E2E_MODEL_KEY_FILE).
# lib/live-rig.sh names every input and what it refuses. LOCKOUT_GENERATIONS_EVIDENCE_DIR (default a
# fresh directory under TMPDIR, kept and printed) keeps the run's evidence. It needs no docker, no
# tmux and no kube context.
#
# --base-commit <rev> is the check's negative control: step 1 at the base. A detached worktree of
# <rev> is made under TMPDIR (removed on any exit), this checkout's rig — the testenvoy package, the
# rig's four driver files and lib/live-rig-grants.go — is laid over it, `bun install
# --frozen-lockfile` gives it node_modules of its own, and the same run is made there with step 1
# alone: the daemon, `legion` and both plugins are the base's. The driver names no Go symbol the base
# lacks, reading every daemon line as a literal, so it builds there. The control passes when step 1
# fails at its stop-frame assertion, which it judges as its 90-second wait for the frame ends and
# before any wait for B's bind, where the step says that A got no stop frame and that the daemon
# refused B `already bound to a live stream` until A's connection ended, or through the window
# where A's connection did not end. Its last line is `lockout generations negative control:
# PASS|FAIL …` with the base run's evidence directory.
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

test_name=TestTheCurrentProcessWinsItsWorkerStreamLive
step1='step1-the-older-process-is-told-to-stop-and-the-newer-binds'
# What the control lays over the base's tree: the rig and this check's driver.
rig_files=(
  packages/daemon/internal/testenvoy
  packages/daemon/internal/daemon/rig_live_test.go
  packages/daemon/internal/daemon/rig_nats_live_test.go
  packages/daemon/internal/daemon/rig_runtime_live_test.go
  packages/daemon/internal/daemon/lockout_live_test.go
  scripts/e2e/lib/live-rig-grants.go
)

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
    echo "lockout generations negative control: PASS in $((SECONDS - started))s (evidence: $base_evidence)"
  else
    echo "lockout generations negative control: FAIL at $check after $((SECONDS - started))s (evidence: $base_evidence)" >&2
  fi
}

negative_control() {
  local base_sha status file line detail
  started=$SECONDS
  base_evidence=${LOCKOUT_GENERATIONS_EVIDENCE_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-lockout-generations-base-evidence.XXXXXXXX")}
  mkdir -p "$base_evidence"
  trap control_cleanup EXIT
  transcript_to "$base_evidence/negative-control.log"

  begin base-tree
  base_sha=$(git -C "$root" rev-parse --verify "$base_commit^{commit}" 2>/dev/null) ||
    refuse "--base-commit '$base_commit' names no commit of this repository"
  base_work=$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-lockout-generations-base.XXXXXXXX")
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
  pass

  set +e
  (
    set -e
    root=$base_tree
    live_rig_proof "lockout generations at ${base_sha:0:12}" lockout-generations-base "^$test_name\$/^$step1\$" "$base_evidence" fail
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
    "FAIL $step1 at stop-frame ("*) ;;
    "ok $step1 "*) fail "step 1 passed over $base_sha, so this check tells it from this checkout in nothing" ;;
    *) fail "step 1 failed over $base_sha elsewhere than at its stop frame: $line" ;;
  esac
  detail=$(grep -m1 -E 'lockout_live_test\.go:[0-9]+: stop-frame: ' "$base_evidence/proof.log") ||
    fail "step 1's failure over $base_sha carries no stop-frame sentence; see $base_evidence/proof.log"
  case "$detail" in
    *"got no stop frame"*"already bound to a live stream"*"until A's connection ended at"*) ;;
    *"got no stop frame"*"already bound to a live stream"*"through the window, and A's connection had not ended"*) ;;
    *) fail "step 1's stop-frame sentence over $base_sha does not say that B was refused \`already bound to a live stream\` until A's connection ended or through the window: $detail" ;;
  esac
  note "$line"
  note "${detail#"${detail%%[![:space:]]*}"}"
  pass
}

if [ -z "$base_commit" ]; then
  live_rig_proof 'lockout generations' lockout-generations "^$test_name\$" "${LOCKOUT_GENERATIONS_EVIDENCE_DIR:-}"
else
  negative_control
fi
