#!/usr/bin/env bash
# The live proof that a role the settings define runs a step of an issue's plan: the settings file
# defines checker, a role of the type testing with a prompt file and a model, the owner of an issue
# gives Legion the plan `implementer checker` with register_plan before the issue's gate opens, and
# the whole daemon runs the implementer, then the checker on the model the settings give it, then
# the retro and the merge. The driver (packages/daemon/internal/daemon/roles_live_test.go, which
# this script builds with `-tags e2e`) writes the settings file and loads it with the daemon's own
# loader, builds and starts the real Dispatch server from this checkout's packages/envoy on a
# database of its own, boots the whole daemon with dispatch_url at it over three issues of one
# project, plays each role's pane over the daemon's worker stream and its routes, and reads with
# the real `dispatch` CLI (packages/envoy-client/bin/dispatch.ts under bun) as a person of the
# project. GitHub is a fake API the daemon reads on its own (internal/githubtest), and each wait is
# internal/testwait's. Each check prints `== <name>`, what it observed and `ok <name>`; the first
# that fails ends the run non-zero, naming it:
#   - checker-defined: the settings load in the driver's process (config.Load of the file that
#     defines `roles: {checker: {type: testing, prompt_file: checker.md, model: checker-model},
#     prober: {type: testing, prompt_file: prober.md}}`) answers RoleOf(checker) with the type
#     testing, the prompt file beside the file, the model checker-model and the review App;
#   - plan-registered: POST /legion/v1/plans/register of plan.md, one task a with
#     `steps="implementer checker"`, under the architect's grant before the gate opens, answers 200
#     with that one task;
#   - walk-implementer-checker: the gate opens at implementing, the implementer completes, the
#     checker launches with a spawn spec naming checker-model and a prompt holding its prompt
#     file's text, its task names the issue, the task's text and ` Step 1 (implementer) reported:`,
#     its pass runs the retro and the merge to done, and no planner and no tester is launched;
#   - checker-model: `dispatch steps --issue <KEY>` prints step 2 of task a as checker, type
#     testing, with the model checker-model, ended passed, then the retro and the merge;
#   - owner-lists-checker: the architect's launch spec, as the fake runtime recorded it and composed
#     as a live launch composes it, holds `checker (testing)`;
#   - retro-rated: the retro's record names rating 4 and the reason "The plan fit the work.", and
#     `dispatch plans --project <PROJECT>` prints the issue with the owner's plan
#     `implementer checker` and that one rating;
#   - control-no-model: the second issue, given `implementer prober`, whose role names no model,
#     walks to done, and `dispatch steps` prints its step 2 with the model stand-in/model, the
#     model the driver's pane answers with for a launch that named none;
#   - control-no-role: the daemon restarts on the settings with checker taken out, and register_plan
#     of `implementer checker` on the third issue is refused 409 PLAN_REFUSED naming checker.
#
# Run it from a checkout on a Linux host with go, git, jj 0.38 or later (the daemon's boot refuses
# to start without them), bun (after `bun install` at the checkout's root), awk and python3, as
#   LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/roles.sh
# LEGION_TEST_PG_DSN is required and names the PostgreSQL the daemon migrates and runs on; the
# driver also creates Dispatch's database on the same server (its role must be allowed to create
# and drop databases, and the server must offer the pgvector extension, which Dispatch's migrations
# create) and drops it on exit. That server must be this machine's: prerequisites reads the DSN
# with lib/loopback-dsn.sh's require_loopback_dsn and refuses a host that is not 127.0.0.1,
# localhost or ::1 with exit 2, before it builds or starts anything. This script starts no
# database. The broker is the run's own: a nats-server from LEGION_E2E_NATS_BIN, or the pinned one
# installed with `go install`. ROLES_EVIDENCE_DIR (default a fresh directory under TMPDIR, or /tmp
# when it is unset, kept and printed) keeps the transcript, the nats-server's log, the daemon's,
# Dispatch's (dispatch.log) and the driver's output. Nothing reaches github.com or a deployed
# Dispatch. A run takes two to five minutes, most of it building Dispatch and the driver. The run
# around the driver is lib/live-daemon-proof.sh's.
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
live_daemon_proof 'roles' roles TestRolesLive "${ROLES_EVIDENCE_DIR:-}"
