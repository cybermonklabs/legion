#!/usr/bin/env bash
# The live proof that a root architect's own `legion` tool makes the registration that
# dispatch://INFRA-3320's R1 takes (its check k8). The driver
# (packages/daemon/internal/daemon/register_gate_basis_live_test.go, which this script builds with
# `-tags e2e`) builds and starts the real Dispatch server from the packages/envoy of
# REGISTER_GATE_BASIS_DISPATCH_ROOT (this checkout by default) on a database of its own, with its
# events on the run's nats-server, and boots the daemon in its own process reading them (the rig of
# person_handover_scratch_dispatch_test.go). The tool is packages/pi-legion's, run under bun by
# packages/pi-legion/test/register-gate-basis-driver.ts as a root architect's pane holds it: the
# plugin's own daemon client at the daemon's API, envoy-client's DispatchClient at the scratch
# Dispatch, and the claim the daemon registered for the architect, whose secret the tool mints each
# grant from. Each step prints `== <name>`, what it observed and `ok <name>`; the first that fails
# ends the run non-zero, naming it:
#   - world: P's primary document marks requirement blocks p1 to p3 and the person approved its
#     latest version; R, a labelled root the daemon admitted and whose architect it launched, is
#     set under P, and R's one check serves P2;
#   - registration: register_gate with step spec, R's primary document (`spec`), the basis
#     dispatch://<P>/spec at P's approved version and a reason; the tool looks the reference up on
#     P and sends one grant and one registration, the daemon records R's spec step open on P's
#     approval (the gate row's basis, open_on_basis in GET /legion/v1/state), and R plans;
#   - refusal: with a decision block open in R's spec, the same call is refused by the tool,
#     naming the block, with no request sent to the daemon, and the daemon records nothing.
# With REGISTER_GATE_BASIS_ACROSS_PROJECTS=1 the roots are issues of project LEGION, whose daemon
# the driver boots, and the issues they rest on are of project OTHR, which no daemon runs; R is
# set under P through Dispatch's own parent route, which a Dispatch that takes no parent of
# another project refuses (400 PARENT_INPUT, so the run fails at world), and two steps follow:
#   - pending: R2 registers on the open approval request of P2 and its step waits; the person
#     approves P2, and the step opens at the one controller tick the driver then runs, not before;
#   - accepted: P3's status is written retro as its daemon's sign_off writes it, and R3's
#     register_gate on P3 is refused by the daemon with 409 BASIS_ACCEPTED, recording nothing.
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
# REGISTER_GATE_BASIS_TOOL_ROOT (default this checkout; a relative path is read from the directory
# the script runs in) names the checkout whose packages/pi-legion tool the run drives. A checkout
# of the commit the branch starts from, with this checkout's
# packages/pi-legion/test/register-gate-basis-driver.ts copied into its packages/pi-legion/test/
# and `bun install` run at its root, is k8's negative control: that tool refuses the call itself,
# since its fields hold no basis, and the run fails at the registration step.
# REGISTER_GATE_BASIS_DISPATCH_ROOT (default this checkout; a relative path is read from the
# directory the script runs in) names the checkout whose packages/envoy the scratch Dispatch is
# built from and runs in; it needs no `bun install`. Across projects, a checkout of a Dispatch that
# takes no parent of another project is the run's negative control: it fails at world.
# REGISTER_GATE_BASIS_ACROSS_PROJECTS is 1 for the run across projects, and unset or empty for the
# run in one project; any other value is refused with exit 2.
# REGISTER_GATE_BASIS_EVIDENCE_DIR (default a fresh directory under TMPDIR, or /tmp when it is
# unset, kept and printed) keeps the transcript, the nats-server's log, the daemon's, Dispatch's
# (dispatch.log), the driver's output, and the scratch Dispatch's whole event log
# (dispatch-events.jsonl, one event to a line as GET /api/v1/events/log serves it, written once the
# daemon has stopped), which scripts/dispatch/cross-project-basis-census.ts --events-file reads.
# Nothing reaches github.com or a deployed Dispatch. A run takes one to five minutes, most of it
# building the driver and Dispatch. The run around the driver is lib/live-daemon-proof.sh's.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
tool_root=$(cd "${REGISTER_GATE_BASIS_TOOL_ROOT:-$root}" && pwd) || {
  echo "FAIL prerequisites: REGISTER_GATE_BASIS_TOOL_ROOT=${REGISTER_GATE_BASIS_TOOL_ROOT:-$root} is not a directory" >&2
  exit 1
}
dispatch_root=$(cd "${REGISTER_GATE_BASIS_DISPATCH_ROOT:-$root}" && pwd) || {
  echo "FAIL prerequisites: REGISTER_GATE_BASIS_DISPATCH_ROOT=${REGISTER_GATE_BASIS_DISPATCH_ROOT:-$root} is not a directory" >&2
  exit 1
}
[ -f "$dispatch_root/packages/envoy/go.mod" ] || {
  echo "FAIL prerequisites: $dispatch_root has no packages/envoy/go.mod, so no Dispatch is built from it" >&2
  exit 1
}
case "${REGISTER_GATE_BASIS_ACROSS_PROJECTS:-}" in
  '') unset LEGION_E2E_ACROSS_PROJECTS ;;
  1) export LEGION_E2E_ACROSS_PROJECTS=1 ;;
  *)
    echo "FAIL prerequisites: REGISTER_GATE_BASIS_ACROSS_PROJECTS=$REGISTER_GATE_BASIS_ACROSS_PROJECTS is neither 1 nor empty" >&2
    exit 2
    ;;
esac
command -v bun >/dev/null || {
  echo "FAIL prerequisites: bun is not on PATH; the legion tool the proof drives runs under it" >&2
  exit 1
}
# The driver runs bun with a HOME and XDG directories of the run's own, where a bun that is a mise
# shim, as on a devbox, finds the operator's mise config untrusted and exits 1 before bun starts
# (mise reads which configs are trusted under XDG_STATE_HOME). So when the first bun on PATH is a
# script or mise itself, the directory of the real bun that `mise which bun` names goes first on
# PATH, which the driver hands bun; a real bun, such as CI's, is left alone.
first_bun=$(command -v bun)
mise_bin=$(command -v mise || true)
if [ "$(head -c 2 "$first_bun")" = "#!" ] || {
  [ -n "$mise_bin" ] && [ "$(readlink -f "$first_bun")" = "$(readlink -f "$mise_bin")" ]
}; then
  real_bun=$(mise which bun) || {
    echo "FAIL prerequisites: the first bun on PATH, $first_bun, is a script or mise itself, and \`mise which bun\` names no bun" >&2
    exit 1
  }
  PATH="$(dirname "$real_bun"):$PATH"
fi
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
# The driver binary runs from this directory, not the package's, so it is told this checkout, the
# checkout it builds Dispatch from and the checkout whose tool it runs.
export LEGION_E2E_REPO_ROOT="$root"
export LEGION_E2E_DISPATCH_ROOT="$dispatch_root"
export LEGION_E2E_TOOL_ROOT="$tool_root"
# shellcheck source-path=SCRIPTDIR source=lib/live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"
live_daemon_proof 'register gate basis' register-gate-basis TestRegisterGateBasisToolLive \
  "${REGISTER_GATE_BASIS_EVIDENCE_DIR:-}"
