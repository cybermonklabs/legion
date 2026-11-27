#!/usr/bin/env bash
# The live proof that the plan the owner of an issue gives picks which of today's steps run and in
# which order: the owner writes plan.md with the real `dispatch` CLI and gives it to Legion with
# register_plan before the issue's gate opens, and the whole daemon runs the steps that plan names,
# in its order and no other, then the retro and the merge, while an issue whose owner gives no plan
# runs the project's default steps. The driver
# (packages/daemon/internal/daemon/owner_steps_live_test.go, which this script builds with
# `-tags e2e`) builds and starts the real Dispatch server from this checkout's
# packages/envoy on a database of its own, boots the whole daemon with dispatch_url at it over five
# issues of one project, drives the real `dispatch` CLI (packages/envoy-client/bin/dispatch.ts under
# bun) as the owner's session and as a person of the project, and the daemon's API as each claim.
# GitHub is a fake API the daemon reads on its own: the pull request it lists from the
# implementer's completion on, a person's review and the merge on the run's broker. Each check
# prints `== <name>`, what it observed and one line `ok <name>` or `FAIL <name>`; the first that
# fails ends the run non-zero, naming it:
#   - plan-written: the owner's session writes plan.md on the first issue with one block
#     `:::plan_task{#a steps="implementer reviewer"}` through `dispatch artifact`, and the daemon's
#     own client reads version 1 back with that one block;
#   - plan-registered: before the gate opens, POST /legion/v1/plans/register with the architect's
#     grant and version 1 answers 200 with one task a naming implementer and reviewer;
#   - walk-implementer-reviewer: the gate opens and the issue walks implementing, reviewing, retro
#     and merging to done, and no planner and no tester is launched;
#   - steps-read: `dispatch steps --issue <KEY>` as the person prints task a's step 1 implementer
#     and step 2 reviewer, then the retro and the merge, each ended passed, and no other run; the
#     line `task a needs …` of the plan before them is no run, and the check passes over it;
#   - walk-default-steps: the second issue, given no plan, walks planning, implementing, testing,
#     reviewing, accepting (the architect's sign-off), retro and merging to done, and holds
#     issue_plans version 0 of the source default, its task default done, plan_steps at places 1
#     to 5, one start of each step, and the seven records of task default in `dispatch steps`;
#   - control-default-rows-deleted: the third issue, given no plan, has its plan rows deleted while
#     its planner works; it walks today's line to done, and the check of walk-default-steps' rows
#     fails on it, naming its issue_plans rows as none;
#   - control-refused: a plan whose task a names `implementer checker` is refused 409 PLAN_REFUSED
#     with the reason that no deployment and no plan defines the role checker, naming task a;
#   - control-no-plan-rows: the fourth issue is given the plan `implementer reviewer`, and its rows
#     are deleted before its gate opens; it starts a planner, and the check of
#     walk-implementer-reviewer fails on it;
#   - control-tester-first: register_plan of `tester implementer` on the fifth issue is refused 409
#     PLAN_REFUSED with the one reason that step 1 names the tester and no step before it the
#     implementer whose pull request it reads, naming task a, and records nothing;
#   - walk-any-order: the fifth issue is given the plan `implementer reviewer tester`; it walks
#     implementing, reviewing, testing, retro and merging to done, its implementer, its reviewer and
#     its tester first launched in that order, and `dispatch steps --issue <KEY>` as the person
#     prints task a's step 1 implementer, step 2 reviewer and step 3 tester, then the retro and the
#     merge, each ended passed, and nothing else.
#
# Run it from a checkout on a Linux host with go, git, jj 0.38 or later (the daemon's boot refuses
# to start without them), bun (after `bun install` at the checkout's root), awk and python3, as
#   LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/owner-steps.sh
# LEGION_TEST_PG_DSN is required and names the PostgreSQL the daemon migrates and runs on; the
# driver also creates Dispatch's database on the same server (its role must be allowed to create
# and drop databases, and the server must offer the pgvector extension, which Dispatch's migrations
# create) and drops it on exit. That server must be this machine's: prerequisites reads the DSN
# with lib/loopback-dsn.sh's require_loopback_dsn and refuses a host that is not 127.0.0.1,
# localhost or ::1 with exit 2, before it builds or starts anything. This script starts no
# database. The broker is the run's own: a nats-server from LEGION_E2E_NATS_BIN, or the pinned one
# installed with `go install`. OWNER_STEPS_EVIDENCE_DIR (default a fresh directory under TMPDIR,
# or /tmp when it is unset, kept and printed) keeps the transcript, the nats-server's log, the
# daemon's, Dispatch's (dispatch.log) and the driver's output. Nothing reaches github.com or a
# deployed Dispatch. A run takes two to five minutes, most of it building Dispatch and the driver.
# The run around the driver is lib/live-daemon-proof.sh's.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
command -v bun >/dev/null || {
  echo "FAIL prerequisites: bun is not on PATH; the dispatch CLI the proof drives runs under it" >&2
  exit 1
}
[ -d "$root/node_modules" ] || {
  echo "FAIL prerequisites: $root/node_modules is missing; run \`bun install\` at the checkout's root first" >&2
  exit 1
}
# The driver binary runs from this directory, not the package's, so it is told the checkout it
# builds Dispatch from and runs the CLI out of.
export LEGION_E2E_REPO_ROOT="$root"
# shellcheck source-path=SCRIPTDIR source=lib/live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"
live_daemon_proof 'owner steps' owner-steps TestOwnerStepsLive "${OWNER_STEPS_EVIDENCE_DIR:-}"
