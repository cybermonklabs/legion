#!/usr/bin/env bash
# The live proof that a role an owner's plan defines runs a step of that plan: the owner of an issue
# writes plan.md with a role block, prober of the type testing on the model prober-model, and the
# task `implementer prober`, gives it to Legion with register_plan before the issue's gate opens,
# and the whole daemon runs the implementer, then the prober with the plan's prompt on the plan's
# model, then the retro and the merge. The driver
# (packages/daemon/internal/daemon/plan_roles_live_test.go, which this script builds with
# `-tags e2e`) builds and starts the real Dispatch server from this checkout's packages/envoy on a
# database of its own, boots the whole daemon with dispatch_url at it, no role of the deployment and
# the listed models [prober-model], over three issues of one project, plays each role's pane over
# the daemon's worker stream and its routes, and writes and reads with the real `dispatch` CLI
# (packages/envoy-client/bin/dispatch.ts under bun): the owner in its own session, the reader as a
# person of the project with the personal token Dispatch minted for that person. Each check prints
# `== <name>`, what it observed and `ok <name>`, or `FAIL <name>`; the first that fails ends the run
# non-zero, naming it:
#   - role-block-stored: `dispatch artifact` writes plan.md on the first issue as the owner's
#     session, and version 1 read with ?blocks=true lists plan_role prober with type=testing,
#     model=prober-model and the plan's prompt as its body, and plan_task a;
#   - plan-role-runs: register_plan of that version is taken (200, one task a naming implementer and
#     prober); the gate opens at implementing, the implementer completes, the prober launches with a
#     spawn spec naming prober-model and a prompt holding the plan's, its task names the issue, the
#     task's text and ` Step 1 (implementer) reported:`, and its pass runs the retro and the merge to
#     done; `dispatch steps --issue <KEY>` prints step 1 implementer, then step 2 prober of type
#     testing with the model prober-model, then the retro and the merge, each ended passed;
#   - control-no-role-block: the second issue's plan, the same task with no role block, is refused
#     409 PLAN_REFUSED naming prober, and leaves no plan rows;
#   - model-not-listed: the third issue's plan, prober on the model other-model, is refused 409
#     PLAN_REFUSED naming other-model and the listed prober-model, and leaves no plan rows.
#
# Run it from a checkout on a Linux host with go, git, jj 0.38 or later (the daemon's boot refuses
# to start without them), bun (after `bun install` at the checkout's root), awk and python3, as
#   LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/plan-roles.sh
# LEGION_TEST_PG_DSN is required and names the PostgreSQL the daemon migrates and runs on; the
# driver also creates Dispatch's database on the same server (its role must be allowed to create
# and drop databases, and the server must offer the pgvector extension, which Dispatch's migrations
# create) and drops it on exit. That server must be this machine's: prerequisites reads the DSN
# with lib/loopback-dsn.sh's require_loopback_dsn and refuses a host that is not 127.0.0.1,
# localhost or ::1 with exit 2, before it builds or starts anything. This script starts no
# database. The broker is the run's own: a nats-server from LEGION_E2E_NATS_BIN, or the pinned one
# installed with `go install`. PLAN_ROLES_EVIDENCE_DIR (default a fresh directory under TMPDIR, or
# /tmp when it is unset, kept and printed) keeps the transcript, the nats-server's log, the
# daemon's, Dispatch's (dispatch.log) and the driver's output. Nothing reaches github.com or a
# deployed Dispatch. A run takes two to five minutes, most of it building Dispatch and the driver.
# The run around the driver is lib/live-daemon-proof.sh's.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
# shellcheck source-path=SCRIPTDIR source=lib/live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"
require_dispatch_cli
live_daemon_proof 'plan roles' plan-roles TestPlanRolesLive "${PLAN_ROLES_EVIDENCE_DIR:-}"
