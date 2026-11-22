#!/usr/bin/env bash
# Check w5 of this change's verification: a claim Envoy cannot reach is marked, reported and
# relaunched (R5), and a person is told what no agent can hear (R6). The whole daemon runs on the
# rig of this change's checks (packages/daemon/internal/daemon/RIG.md): the listener production
# pins, built from its own commit, a real nats-server with production's grants, a PostgreSQL of the
# run's own, a scratch Dispatch the daemon reads, and real `omp --mode rpc` panes under the real
# `legion worker-shim`, each with this checkout's @sjawhar/pi-envoy and @sjawhar/pi-legion, whose
# turns go to a scripted model stand-in of each claim's own. The driver is
# packages/daemon/internal/daemon/w5_unreachable_live_test.go; its rig parts are
# w5_rig_live_test.go and w5_inplace_live_test.go. Each step prints `== <step>`, what it observed
# with the times, `CHECK PASS|FAIL <clause>: <evidence>` for each clause of the check's Expected,
# and `ok <step>`, or `FAIL <step> at <assertion>`.
#
# --part names one of the check's three runs, each on a rig of its own, or the rig's own check:
#   main                  the controller as a daemon claim and twelve more claims (the twelfth in
#                         a live wait on a named service, never stopped): the first block, the
#                         claims after it, the pods, the sixth claim's three runs, the daemon's
#                         restart, every session blocked and the list failing (about 5 hours)
#   controller-job        the fresh run whose controller runs a background job (about 1 hour)
#   controller-launcher   the fresh run whose controller's launcher is not connected, and its
#                         second out-of-reach within 6 hours (about 1.5 hours)
#   rig-teardown          the rig's check that a role's processes, the named services Oh My Pi's
#                         broker runs among them, end with its process on a stop and on a death,
#                         as they end with the role's PID namespace in a pod
#                         (w5_rig_teardown_live_test.go, about 2 minutes)
#
# Run it from a checkout on a Linux host as
#   LEGION_OMP_PATH=<omp> LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     LEGION_E2E_MODEL_BASE_URL=<anthropic-messages endpoint> LEGION_E2E_MODEL_KEY_COMMAND=<command> \
#     bash scripts/e2e/unreachable-claims.sh [--part main|controller-job|controller-launcher|rig-teardown] [--base-commit <rev>]
# The panes' turns go to the stand-ins, so the model the run names is never called; lib/live-rig.sh
# asks for it all the same. UNREACHABLE_CLAIMS_EVIDENCE_DIR (default a fresh directory under TMPDIR,
# kept and printed) keeps the run's evidence, and W5_RESULTS_FILE (default results.txt in it) the
# results, each line stamped and synced as it is written. It needs no docker, no tmux and no kube
# context.
#
# --base-commit <rev> is the check's negative control: the first block at the base. A detached
# worktree of <rev> is made under TMPDIR (removed on any exit), this checkout's rig and the check's
# driver are laid over it (rig_files; w5_inplace_live_test.go, which names the head's own runtime
# symbols, is left out), `bun install --frozen-lockfile` gives it node_modules of its own, and the
# setup and the first block run there: the daemon, `legion` and both plugins are the base's. The
# control passes when the first block fails at its mark, where it says that at 22 minutes
# read_state shows the implementer's claim working with no mark, no notice names it, fresh_session
# refuses it CLAIM_IN_A_TURN, and the role message is finished undelivered once Envoy reads the
# session gone. Its last line is `unreachable claims negative control: PASS|FAIL …`.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
part=main
base_commit=''
base_given=false
usage() {
  echo "usage: $0 [--part main|controller-job|controller-launcher|rig-teardown] [--base-commit <rev>]" >&2
  exit 2
}
while [ $# -gt 0 ]; do
  case "$1" in
    --part)
      [ $# -ge 2 ] || usage
      part=$2
      shift 2
      ;;
    --base-commit)
      [ $# -ge 2 ] || usage
      base_commit=$2
      base_given=true
      shift 2
      ;;
    *) usage ;;
  esac
done
if [ "$base_given" = true ] && [ -z "$base_commit" ]; then
  echo "--base-commit names no commit" >&2
  exit 2
fi
case "$part" in
  main)
    test_name=TestW5UnreachableClaimsLive
    export LIVE_RIG_TEST_TIMEOUT=${LIVE_RIG_TEST_TIMEOUT:-7h}
    ;;
  controller-job)
    test_name=TestW5ControllerBackgroundJobLive
    export LIVE_RIG_TEST_TIMEOUT=${LIVE_RIG_TEST_TIMEOUT:-3h}
    ;;
  controller-launcher)
    test_name=TestW5ControllerLauncherLive
    export LIVE_RIG_TEST_TIMEOUT=${LIVE_RIG_TEST_TIMEOUT:-3h}
    ;;
  rig-teardown)
    test_name=TestW5RigEndsARolesProcessesWithIt
    export LIVE_RIG_TEST_TIMEOUT=${LIVE_RIG_TEST_TIMEOUT:-40m}
    ;;
  *) usage ;;
esac
if [ "$base_given" = true ] && [ "$part" != main ]; then
  echo "--base-commit runs the first block of --part main; it takes no other part" >&2
  exit 2
fi
# The rig's scratch Dispatch (rigOptions.dispatch) is built from the checkout this names.
export LEGION_E2E_REPO_ROOT="$root"
# shellcheck source-path=SCRIPTDIR source=lib/live-rig.sh
. "$root/scripts/e2e/lib/live-rig.sh"

setup_step='setup-every-claim-in-the-state-the-check-names'
first_block='first-block'
# What the control lays over the base's tree: the rig and this check's driver, without the file
# that names the head's own runtime symbols.
rig_files=(
  packages/daemon/internal/testenvoy
  packages/daemon/internal/daemon/rig_live_test.go
  packages/daemon/internal/daemon/rig_nats_live_test.go
  packages/daemon/internal/daemon/rig_runtime_live_test.go
  packages/daemon/internal/daemon/w5_rig_live_test.go
  packages/daemon/internal/daemon/w5_unreachable_live_test.go
  packages/daemon/internal/daemon/w5_twelfth_refusal_test.go
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
    echo "unreachable claims negative control: PASS in $((SECONDS - started))s (evidence: $base_evidence)"
  else
    echo "unreachable claims negative control: FAIL at $check after $((SECONDS - started))s (evidence: $base_evidence)" >&2
  fi
}

negative_control() {
  local base_sha status file line detail
  started=$SECONDS
  base_evidence=${UNREACHABLE_CLAIMS_EVIDENCE_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-unreachable-claims-base-evidence.XXXXXXXX")}
  mkdir -p "$base_evidence"
  trap control_cleanup EXIT
  transcript_to "$base_evidence/negative-control.log"

  begin base-tree
  base_sha=$(git -C "$root" rev-parse --verify "$base_commit^{commit}" 2>/dev/null) ||
    refuse "--base-commit '$base_commit' names no commit of this repository"
  base_work=$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-unreachable-claims-base.XXXXXXXX")
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
    export LEGION_E2E_REPO_ROOT="$base_tree"
    live_rig_proof "unreachable claims at ${base_sha:0:12}" unreachable-claims-base \
      "^$test_name\$/^($setup_step|$first_block)\$" "$base_evidence" fail
  )
  status=$?
  set -e
  if [ "$status" -eq 2 ]; then
    check=base-run
    exit 2
  fi

  begin negative-control
  grep -q "^ok $setup_step " "$base_evidence/proof.log" 2>/dev/null ||
    fail "the setup over $base_sha did not pass, so the first block never ran; see $base_evidence/proof.log"
  line=$(grep -m1 -E "^(ok|FAIL) $first_block( |$)" "$base_evidence/proof.log") ||
    fail "the first block over $base_sha came to no outcome; see $base_evidence/proof.log"
  case "$line" in
    "FAIL $first_block at unreachable-mark ("*) ;;
    "ok $first_block "*) fail "the first block passed over $base_sha, so this check tells it from this checkout in nothing" ;;
    *) fail "the first block failed over $base_sha elsewhere than at its mark: $line" ;;
  esac
  detail=$(grep -m1 -E 'w5_unreachable_live_test\.go:[0-9]+: unreachable-mark: ' "$base_evidence/proof.log") ||
    fail "the first block's failure over $base_sha carries no unreachable-mark sentence; see $base_evidence/proof.log"
  case "$detail" in
    *"claim working with no mark (no mark)"*"0 notices name it"*"fresh_session refuses it CLAIM_IN_A_TURN"*"finished undelivered at"*) ;;
    *) fail "the first block's sentence over $base_sha does not say what the control asks: $detail" ;;
  esac
  note "$line"
  note "${detail#"${detail%%[![:space:]]*}"}"
  pass
}

if [ -z "$base_commit" ]; then
  pattern="^$test_name\$"
  # UNREACHABLE_CLAIMS_STEPS, a regular expression of step names, runs those steps of the part alone
  # (its setup step among them, or nothing has been set up), which a step re-run after a timeout takes.
  if [ -n "${UNREACHABLE_CLAIMS_STEPS:-}" ]; then
    pattern="^$test_name\$/^(${UNREACHABLE_CLAIMS_STEPS})\$"
  fi
  live_rig_proof "unreachable claims ($part)" "unreachable-claims-$part" "$pattern" "${UNREACHABLE_CLAIMS_EVIDENCE_DIR:-}"
else
  negative_control
fi
