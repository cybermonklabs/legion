#!/usr/bin/env bash
# The live proof that a project's declared experiment draws a group for each new issue, and that
# the group sets how the issue is done: the experiment `planning` gives its arm `a` the plan
# `owner`, where the issue's owner gives its plan as today, and its arm `b` the plan `default`,
# where the issue runs the project's default steps and Legion takes no plan from its owner. The
# driver (packages/daemon/internal/daemon/plan_experiment_live_test.go, which this script builds
# with `-tags e2e`) builds and starts the real Dispatch server from this checkout's packages/envoy
# on a database of its own, with its events on the run's own broker, and boots the whole daemon
# with dispatch_url at it, its project declaring `planning` at mode on. Each issue is created and
# handed to Legion as a person's todo through Dispatch's API; its owner writes `plan.md` with the
# real `dispatch` CLI (packages/envoy-client/bin/dispatch.ts under bun) and gives it with
# register_plan through the daemon's API; the operator reads the groups with the `legion` this
# checkout builds. The GitHub API is the stand-in of internal/githubtest, and every wait is
# internal/testwait's. Each check prints `== <name>`, what it observed and `ok <name>`; the first
# that fails ends the run non-zero, naming it:
#   - group-recorded: two issues handed to Legion with the arm `a` in force; `legion state` prints
#     `planning: 2 issues — 2 a (on)`, and `legion state --json` shows on each issue its
#     assignment of `planning`, arm `a`, mode `on`, with the instant of its draw; control: before
#     either issue the same command prints `planning: none`;
#   - owner-plan-taken: the owner of the first of them gives the plan `implementer reviewer`, which
#     register_plan answers 200; once the person opens its gate, its run starts at implementing on
#     the owner's plan;
#   - default-not-taken: the daemon is stopped and booted again with the arm `b` in force, and a
#     third issue is handed to Legion; register_plan on it answers 409 PLAN_NOT_TAKEN, and the same
#     call sent again the same; once its gate opens, its run starts at planning, the project's
#     default steps; `legion state` prints `planning: 3 issues — 2 a (on), 1 b (on)`, the issues
#     drawn before the second boot keeping the arm `a`;
#   - variant-on-steps: the real `dispatch steps --issue <KEY>` prints `Experiment planning:
#     variant b.` right after the third issue's header and before its first run, and `Experiment
#     planning: variant a.` there for the first, as each record of a step names the variant of its
#     issue's draw; control, run by hand: with envoy-client's `dispatch steps` printing the line
#     after the runs, this check fails, naming where the line stands.
#
# Run it from a checkout on a Linux host with go, git, jj 0.38 or later (the daemon's boot refuses
# to start without them), bun (after `bun install` at the checkout's root), awk and python3, as
#   LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/experiment.sh
# LEGION_TEST_PG_DSN is required and names the PostgreSQL the daemon migrates and runs on; the
# driver also creates Dispatch's database on the same server (its role must be allowed to create
# and drop databases, and the server must offer the pgvector extension, which Dispatch's migrations
# create) and drops it on exit. That server must be this machine's: prerequisites reads the DSN
# with lib/loopback-dsn.sh's require_loopback_dsn and refuses a host that is not 127.0.0.1,
# localhost or ::1 with exit 2, before it builds or starts anything. This script starts no
# database. The broker is the run's own: a nats-server from LEGION_E2E_NATS_BIN, or the pinned one
# installed with `go install`. EXPERIMENT_EVIDENCE_DIR (default a fresh directory under TMPDIR, or
# /tmp when it is unset, kept and printed) keeps the transcript, the nats-server's log, the
# daemon's, Dispatch's (dispatch.log) and the driver's output. Nothing reaches github.com or a
# deployed Dispatch. A measured run took 40 seconds with a warm Go build cache: 15 to build the
# driver and 23 for the proof, which builds Dispatch and `legion` first. The run around the driver
# is lib/live-daemon-proof.sh's.
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
# builds Dispatch and `legion` from and runs the CLI out of.
export LEGION_E2E_REPO_ROOT="$root"
# shellcheck source-path=SCRIPTDIR source=lib/live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"
live_daemon_proof 'experiment' experiment TestExperimentLive "${EXPERIMENT_EVIDENCE_DIR:-}"
