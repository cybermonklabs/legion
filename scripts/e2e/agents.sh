#!/usr/bin/env bash
# The live proof of the agents of an owner's plan: one key is one agent in one session across
# roles, and a key on both steps of a role that stands at two steps of a task is one agent in one
# session.
# The driver (packages/daemon/internal/daemon/agents_live_test.go, which this script builds with
# `-tags e2e`) builds and starts the real Dispatch server from this checkout's packages/envoy on a
# database of its own, boots the whole daemon with dispatch_url at it, has the owner of each of four
# issues give its plan, walks each plan it takes to done, and reads its steps with the real
# `dispatch` CLI (packages/envoy-client/bin/dispatch.ts under bun) as a person of the project, with
# the personal token Dispatch minted for that person. Each check prints `== <name>`, what it
# observed and `ok <name>`; the first that fails ends the run non-zero, naming it:
#   - one-key-two-roles: the plan `implementer:a tester:a reviewer`; the tester's step is a relaunch
#     of the implementer's claim with the role tester that resumes its session, and the retro
#     another, back to the implementer; the step records name one agent and one session for steps 1
#     and 2 and another for step 3, and `dispatch steps --issue <KEY>` prints the same agents;
#   - control-no-key-on-step-2: the plan `implementer:a tester reviewer`; step 2 runs in an agent
#     and a session of its own, which the same reads must show;
#   - role-two-times: the plan `implementer:a reviewer implementer:a`; the reviewer requests changes
#     once, and the task returns to step 1 in the implementer's session; after the approval, step 3
#     runs in that same session, its task naming the reports of steps 1 and 2; the step records name
#     one agent and one session for each run of steps 1 and 3 and another for step 2, and
#     `dispatch steps --issue <KEY>` prints the same agents;
#   - control-no-key-on-step-3: the plan `implementer:a reviewer implementer`, whose step 3 asks for
#     a second agent that starts with the role implementer, is refused 409 PLAN_REFUSED with that
#     reason, and the issue holds no plan rows.
#
# Run it from a checkout on a Linux host with git, jj 0.38 or later (the daemon's boot refuses to
# start without them), go, bun (after `bun install` at the checkout's root), awk and python3, as
#   LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/agents.sh
# LEGION_TEST_PG_DSN is required and names the PostgreSQL the daemon migrates and runs on; the
# driver also creates Dispatch's database on the same server (its role must be allowed to create
# and drop databases, and the server must offer the pgvector extension) and drops it on exit. That
# server must be this machine's: prerequisites refuses a host that is not 127.0.0.1, localhost or
# ::1 with exit 2, before it builds or starts anything. This script starts no database. The broker
# is the run's own: a nats-server from LEGION_E2E_NATS_BIN, or the pinned one installed with
# `go install`. AGENTS_EVIDENCE_DIR (default a fresh directory under TMPDIR, or /tmp when it is
# unset, kept and printed) keeps the transcript, the nats-server's log, the daemon's, Dispatch's
# (dispatch.log) and the driver's output. Nothing reaches github.com or a deployed Dispatch. A run
# takes two to five minutes, most of it building Dispatch and the driver. The run around the driver
# is lib/live-daemon-proof.sh's.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
# shellcheck source-path=SCRIPTDIR source=lib/live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"
require_dispatch_cli
live_daemon_proof 'agents' agents TestAgentsLive "${AGENTS_EVIDENCE_DIR:-}"
