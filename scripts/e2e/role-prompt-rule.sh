#!/usr/bin/env bash
# The live proof that a role process is launched with the pod memory rule in its own system
# prompt. The whole daemon is booted by the function `legion start` calls, over a real PostgreSQL
# and a real nats-server, and two real `omp --mode rpc` panes run under it — the real `legion
# worker-shim`, this checkout's `legion`, and this checkout's @sjawhar/pi-envoy and
# @sjawhar/pi-legion in an Oh My Pi profile of the run's own — each answered by an HTTP model
# stand-in of the driver's own. The two panes are the two roles of one tree: its root architect,
# and the planner of the same issue, which stands in planning.
#
# Once both panes have registered and reported ready, the driver reads each pane's own Oh My Pi
# command line from /proc/<pid>/cmdline and takes the word after `--append-system-prompt`. The pid
# the claim's locator carries is the `legion worker-shim`'s, which starts Oh My Pi as a child, so
# the driver walks /proc for that shim's child whose argv[0] is LEGION_OMP_PATH and reads it. It
# holds the phase worker's eight sentences of the rule and the root architect's six in that word,
# and names the role, the pid and the sentence for any it cannot find. That command line is the
# only place it reads: the composer, the role prompt files below the state directory and the
# recorded launch specification all hold that text before a process took it, and the unit tests
# already read it there (packages/daemon/internal/prompts/prompts_test.go and
# packages/daemon/internal/prompts/roles_test.go). The driver is
# packages/daemon/internal/daemon/role_prompt_rule_live_test.go, which this script builds with
# `-tags e2e`, over packages/daemon/internal/daemon/localruntime_live_test.go, the runtime that
# starts a pane with no tmux. Each check prints `== <name>`, what it observed and `ok <name>`; the
# first that fails ends the run non-zero, naming it.
#
# The whole run took 64 s and 104 s on two runs measured in a Legion worker pod (omp/18.16.0)
# with a warm Go build cache, the driver 41 s and 43 s of them; the same pod's first run took
# 179 s, which is that cache cold and the pinned nats-server installed by the run itself. The
# proof waits on no clock of its own. Its one wait is the two panes starting Oh My Pi, loading the
# two plugins and reporting ready, which took 10 s of the driver's time in each of the three runs;
# the command line it reads is on each process from the moment that process began.
#
# Run it from a checkout on a Linux host with go, awk, bash, tar, bun and python3, as
#   LEGION_OMP_PATH=/opt/omp/bin/omp \
#     LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/role-prompt-rule.sh
# LEGION_OMP_PATH is required and names the Oh My Pi every pane runs, an absolute executable: this
# proof launches real agents, and no pin is guessed for it. In a Legion worker pod that binary is
# the fleet's build rather than the branch's fetch, and the driver prints which build it ran.
# LEGION_TEST_PG_DSN is required and names the PostgreSQL the daemon migrates and runs on — CI's
# service, a devbox container and a pod's own embedded PostgreSQL all satisfy it on a loopback
# address, and this script starts none. That server must be this machine's: prerequisites reads
# the DSN with lib/loopback-dsn.sh's require_loopback_dsn and refuses a host that is not
# 127.0.0.1, localhost or ::1 with exit 2, before it builds or starts anything.
# The broker is the run's own: a nats-server from LEGION_E2E_NATS_BIN, or the pinned one this
# script installs with `go install` into a GOBIN of its own, started with JetStream on a port of
# the run's own and stopped on any exit. ROLE_PROMPT_RULE_EVIDENCE_DIR (default a fresh directory
# under TMPDIR, or /tmp when it is unset, kept and printed) keeps the transcript, the nats-server's
# log, the daemon's own and the driver's output; the run's work directory, the driver binary among
# it, is under TMPDIR too, since a pod's own disk is small and its TMPDIR names the issue's volume.
# The driver builds `legion` and packs the two plugins from this checkout itself, so it needs bun
# and the repository's node_modules. It needs no kube context, no Claude Code and no tmux — the
# panes are child process groups of the driver — so it runs in a Legion pod and on a devbox alike.
# The run around the driver is lib/live-daemon-proof.sh's, which provider-error-retry.sh and
# claim-compaction.sh make too.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
if [ -z "${LEGION_OMP_PATH:-}" ]; then
  echo "FAIL setup: LEGION_OMP_PATH is required: it names the Oh My Pi every pane of this proof runs (an absolute executable path). This proof launches real agents and guesses no build." >&2
  exit 1
fi
if [ ! -x "$LEGION_OMP_PATH" ]; then
  echo "FAIL setup: LEGION_OMP_PATH ($LEGION_OMP_PATH) is not an executable file" >&2
  exit 1
fi
# shellcheck source-path=SCRIPTDIR source=lib/live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"
live_daemon_proof 'role prompt rule' role-prompt-rule \
  TestEveryRoleIsLaunchedWithThePodMemoryRuleLive \
  "${ROLE_PROMPT_RULE_EVIDENCE_DIR:-}"
