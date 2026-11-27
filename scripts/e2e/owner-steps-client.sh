#!/usr/bin/env bash
# The live proof of the owner's plan through the plugin's own daemon client. The driver
# (packages/daemon/internal/daemon/owner_steps_client_live_test.go, built with `-tags e2e`) boots the
# whole daemon as scripts/e2e/owner-steps.sh's driver does (a Dispatch this checkout builds, on a
# database of its own on LEGION_TEST_PG_DSN's server, and a fake GitHub), and makes each call the
# `legion` tool makes through createLegionDaemonClient (packages/pi-shared/src/daemon-client.ts)
# under bun, one run of scripts/e2e/owner-steps-client.ts per call:
#   - older-client-tree: the driver builds the plugin of the release before itself: the newest
#     commit of main's first-parent line whose daemon API contract, as scripts/daemon-contract.sh
#     prints it for that commit, is below this checkout's, walked on refs/remotes/origin/main as
#     this script fetched it (below), taken with `git archive` into the run's own directory and
#     given its own `bun install --frozen-lockfile`;
#   - control-client-plan-version-absent, client-register-plan: planRegister with the architect's
#     grant (mintClaimGrant) answers 200 with the plan; the same call naming a version Dispatch does
#     not hold fails the same check;
#   - client-phase-worker-refused: planRegister and taskStop with the implementer's grant throw
#     LegionDaemonApiError 403 ARCHITECT_REQUIRED; the architect's answer fails the same check;
#   - client-return-to: the phase op handoff_complete {verdict fail, return_to 1} of a tester
#     (executePhaseOp) is taken and returns task a to its step 1;
#   - control-client-return-to-refused: return_to 2, the tester's own place, throws 409
#     RETURN_REFUSED and moves nothing; the tester's pass after it is taken;
#   - older-client-reads: a tester's claim whose recorded contract is set to the release before's
#     while no daemon runs (a pane registered under that release that outlives the upgrade, within
#     its turn; this daemon refuses a new registration at any other contract and relaunches such a
#     pane when its turn ends) reads, through that release's client, state(), TASK_STOPPED after its
#     owner's stop, and its own registration at its contract, refused 409. Its read of the issue held
#     task-stopped must parse in that release's own schema and carry no hold reason that release
#     does not know: the daemon serves each client the shape of the contract its request states;
#   - client-stop-task: taskStop of task a with the architect's grant answers 200 {} and the issue
#     stands held task-stopped; a task the plan does not hold fails the same check;
#   - client-stopped-report-refused: the stopped task's tester's handoff_complete throws 409
#     TASK_STOPPED; the taken pass above fails the same check.
#
# Run it from a checkout on a Linux host with what scripts/e2e/owner-steps.sh needs (go, git, jj,
# bun after `bun install`, awk, python3, a loopback LEGION_TEST_PG_DSN), and tar, as
#   LEGION_TEST_PG_DSN=… bash scripts/e2e/owner-steps-client.sh
# OWNER_STEPS_CLIENT_EVIDENCE_DIR keeps the run's evidence as OWNER_STEPS_EVIDENCE_DIR does for
# owner-steps.sh. The run around the driver is lib/live-daemon-proof.sh's.
#
# The driver's daemon is the one daemon of an e2e driver built with the merge order
# (scripts/daemon-contract-order.sh --stamp HEAD, which build_daemon_driver writes into the
# binary), since it alone meets a plugin of another commit, and an unstamped build numbers its
# entries by id rather than in the order main gives them. The order is main's with this
# checkout's own entries after it: the order main takes if this checkout merges next. The script
# reads the history of HEAD and of main, which a shallow clone, as CI's checkout is, lacks, so
# this script first fetches both without blobs, main into refs/remotes/origin/main, where the
# driver's walk for the release before reads it too. A clone that holds HEAD's history fetches
# main alone, since HEAD may be a commit origin has never seen. The order names the entries
# committed at HEAD, and the daemon refuses an order that does not name exactly the entries it
# was built with, so an entry file this checkout has not committed fails the run.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
command -v bun >/dev/null || {
  echo "FAIL prerequisites: bun is not on PATH; the plugin's client runs under it" >&2
  exit 1
}
[ -d "$root/node_modules" ] || {
  echo "FAIL prerequisites: $root/node_modules is missing; run \`bun install\` at the checkout's root first" >&2
  exit 1
}
for tool in git tar; do
  command -v "$tool" >/dev/null || {
    echo "FAIL prerequisites: $tool is not on PATH; the driver builds the plugin of the release before with it" >&2
    exit 1
  }
done
export LEGION_E2E_REPO_ROOT="$root"
fetch=(git -C "$root" fetch --quiet --no-tags --filter=blob:none)
if [ "$(git -C "$root" rev-parse --is-shallow-repository)" = true ]; then
  fetch+=(--unshallow origin "$(git -C "$root" rev-parse HEAD)" refs/heads/main:refs/remotes/origin/main)
else
  fetch+=(origin refs/heads/main:refs/remotes/origin/main)
fi
"${fetch[@]}" || {
  echo "FAIL prerequisites: ${fetch[*]} failed; the driver's merge order and its release before read the history of HEAD and of main" >&2
  exit 1
}
stamp=$(cd "$root" && bash scripts/daemon-contract-order.sh --stamp HEAD) || {
  echo "FAIL prerequisites: scripts/daemon-contract-order.sh --stamp HEAD failed, so the driver has no merge order to carry" >&2
  exit 1
}
# shellcheck source-path=SCRIPTDIR source=lib/live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"
live_daemon_proof 'owner steps client' owner-steps-client TestOwnerStepsClientLive "${OWNER_STEPS_CLIENT_EVIDENCE_DIR:-}" '' '' "$stamp"
