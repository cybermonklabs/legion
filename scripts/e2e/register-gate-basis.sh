#!/usr/bin/env bash
# The live proof that a root architect's own `legion` tool makes the registration that
# dispatch://INFRA-3320's R1 takes (its check k8). The driver
# (packages/daemon/internal/daemon/register_gate_basis_live_test.go, which this script builds with
# `-tags e2e`) builds and starts the real Dispatch server from this checkout's packages/envoy on a
# database of its own, with its events on the run's nats-server, and boots the daemon in its own
# process reading them (the rig of person_handover_scratch_dispatch_test.go). The tool is
# packages/pi-legion's, run under bun by packages/pi-legion/test/register-gate-basis-driver.ts as a
# root architect's pane holds it: the plugin's own daemon client at the daemon's API,
# envoy-client's DispatchClient at the scratch Dispatch, and the claim the daemon registered for the
# architect, whose secret the tool mints each grant from. Each step prints `== <name>`, what it
# observed and `ok <name>`; the first that fails ends the run non-zero, naming it:
#   - the world: P's primary document marks requirement blocks p1 to p3 and the person approved its
#     latest version; R, a labelled root the daemon admitted and whose architect it launched, is
#     set under P, and R's one check serves P2;
#   - the registration: register_gate with step spec, R's primary document (`spec`), the basis
#     dispatch://<P>/spec at P's approved version and a reason; the tool looks the reference up on
#     P and sends one grant and one registration, the daemon records R's spec step open on P's
#     approval (the gate row's basis, open_on_basis in GET /legion/v1/state), and R plans;
#   - the refusal: with a decision block open in R's spec, the same call is refused by the tool,
#     naming the block, with no request sent to the daemon, and the daemon records nothing.
#
# Run it from a checkout on a Linux host with go, bun (after `bun install` at the checkout's
# root), awk and python3, as
#   LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/register-gate-basis.sh
# LEGION_TEST_PG_DSN is required and names the PostgreSQL the daemon migrates and runs on; the
# driver also creates Dispatch's database on the same server (its role must be allowed to create
# and drop databases, and the server must offer the pgvector extension, which Dispatch's migrations
# create) and drops it on exit. That server must be this machine's: prerequisites reads the DSN
# with lib/loopback-dsn.sh's require_loopback_dsn and refuses a host that is not 127.0.0.1,
# localhost or ::1 with exit 2, before it builds or starts anything. This script starts no
# database. The broker is the run's own: a nats-server from LEGION_E2E_NATS_BIN, or the pinned one
# installed with `go install`; a LEGION_TEST_NATS_URL of the caller's is not read.
# REGISTER_GATE_BASIS_TOOL_ROOT (default this checkout) names the checkout whose packages/pi-legion
# tool the run drives. A checkout of the commit the branch starts from, with this checkout's
# packages/pi-legion/test/register-gate-basis-driver.ts copied into its packages/pi-legion/test/
# and `bun install` run at its root, is k8's negative control: that tool refuses the call itself,
# since its fields hold no basis, and the run fails at the registration step.
# REGISTER_GATE_BASIS_EVIDENCE_DIR (default a fresh directory under TMPDIR, or /tmp when it is
# unset, kept and printed) keeps the transcript, the nats-server's log, the daemon's, Dispatch's
# (dispatch.log) and the driver's output. Nothing reaches github.com or a deployed Dispatch. A run
# takes one to five minutes, most of it building the driver and Dispatch. The run around the
# driver is lib/live-daemon-proof.sh's.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
tool_root=${REGISTER_GATE_BASIS_TOOL_ROOT:-$root}
command -v bun >/dev/null || {
  echo "FAIL prerequisites: bun is not on PATH; the legion tool the proof drives runs under it" >&2
  exit 1
}
[ -d "$root/node_modules" ] || {
  echo "FAIL prerequisites: $root/node_modules is missing; run \`bun install\` at the checkout's root first" >&2
  exit 1
}
[ -d "$tool_root/node_modules" ] || {
  echo "FAIL prerequisites: $tool_root/node_modules is missing; run \`bun install\` at the root of REGISTER_GATE_BASIS_TOOL_ROOT first" >&2
  exit 1
}
[ -f "$tool_root/packages/pi-legion/test/register-gate-basis-driver.ts" ] || {
  echo "FAIL prerequisites: $tool_root has no packages/pi-legion/test/register-gate-basis-driver.ts; copy this checkout's into it" >&2
  exit 1
}
# The driver binary runs from this directory, not the package's, so it is told the checkout it
# builds Dispatch from and the checkout whose tool it runs.
export LEGION_E2E_REPO_ROOT="$root"
export LEGION_E2E_TOOL_ROOT="$tool_root"
# shellcheck source-path=SCRIPTDIR source=lib/live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"
live_daemon_proof 'register gate basis' register-gate-basis TestRegisterGateBasisToolLive \
  "${REGISTER_GATE_BASIS_EVIDENCE_DIR:-}"
