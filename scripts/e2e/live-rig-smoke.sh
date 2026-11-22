#!/usr/bin/env bash
# The live proof of the rig this change's checks run on (packages/daemon/internal/daemon/RIG.md):
# that each part of it is what it says, and that each seam the checks after w3 build on does what
# RIG.md says, before a check reads anything through it. The driver is
# packages/daemon/internal/daemon/rig_smoke_live_test.go; each step prints `== <step>`, what it
# read, and `ok <step>`, or `FAIL <step> at <assertion>`:
#
#   the listener is the pinned one and each user holds its grant   /healthz, /connz's users, and
#                                                                   a publish a client with no
#                                                                   credential is refused
#   the front stops and lets through a session's registrations     blockRegistrations, unblock
#   nats stops and starts and the session connects again           rigNATS.stop, start
#   the daemon restarts on its own port and the pane binds again   restartDaemon
#   the scratch dispatch publishes on the rig's NATS               an issue's event on the stream
#
# Its one claim's turns go to the rig's scripted stand-in, and it runs the scratch Dispatch: the two
# options w3 (scripts/e2e/lockout-generations.sh) does not take. It is run as that script is, with
# the same inputs (lib/live-rig.sh names them); the model's inputs are asked for and not read.
# LIVE_RIG_SMOKE_EVIDENCE_DIR (default a fresh directory under TMPDIR) keeps the run's evidence.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
# The scratch Dispatch the driver starts is built from the checkout this names: the driver runs
# from a temporary directory, where testbuild finds no checkout of its own.
export LEGION_E2E_REPO_ROOT="$root"
# shellcheck source-path=SCRIPTDIR source=lib/live-rig.sh
. "$root/scripts/e2e/lib/live-rig.sh"

live_rig_proof 'live rig smoke' live-rig-smoke '^TestTheRigServesWhatItsChecksNameLive$' "${LIVE_RIG_SMOKE_EVIDENCE_DIR:-}"
