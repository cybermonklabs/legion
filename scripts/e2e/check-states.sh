#!/usr/bin/env bash
# The live proof that the tester's move is decided by the check results in Dispatch alone, on the
# real Dispatch, the real daemon and a real tester's pane: the tester records its results with the
# real `dispatch result`, its real `legion` tool completes testing with no verdict, Dispatch
# publishes each change of a check's state as a check.state_changed event on the run's own broker,
# and the daemon, which takes those events and nothing else of the results, moves the issue to
# reviewing when every check reads yes at the head the tester's test record names, and back to
# implementing otherwise, its task naming each check that does not pass with its state and reason.
# The driver is packages/daemon/internal/daemon/check_states_live_test.go and the
# check_states_live_*_test.go files beside it, which this script builds with `-tags e2e`, over
# localruntime_live_test.go, the runtime that starts a pane with no tmux, and
# late_completion_live_test.go's model stand-in, which calls the `legion` tool as a model does.
#
# The cases, each on issues of its own, chosen with CHECK_STATES_E2E_CASES (a comma- or
# space-separated list; every case when it is unset or empty), in this order:
#   - one-record: the tester records a pass of k1 and a fail of k2 and completes with the summary
#     "Every check passes."; the issue goes back to implementing, the tester's phase-finished
#     notice carrying the head and k2 as its failing check. A second run, its passes of both, goes
#     to reviewing and gives the implementer no task;
#   - daemon-records-nothing: the two runs of one-record, then every request of Dispatch's access log
#     that records or retracts a result, and every row of check_results, ci_check_results and their
#     retraction tables, names a tester's or an implementer's session of the run, and never the
#     daemon's;
#   - dispatch-tells: the tester's fail of k1 puts `workflow: a check state is held` for the issue,
#     k1, the head, no and fail in the daemon's log within 10 seconds of the write, by the record's
#     own time; the issue stays in testing and the implementer is given no task;
#   - pass-goes-to-review: the implementer's and the tester's passes of k1 and k2; within 10 seconds
#     of the completion GET /legion/v1/state shows the issue in reviewing and the daemon has started
#     the reviewer;
#   - no-check: an issue whose Verification document holds one check, after_merge, and so no
#     before_merge check, goes to reviewing within 10 seconds when its tester completes, and Dispatch
#     holds no result of it and publishes its head's set with no check;
#   - fail-sends-back: a pass of k1 and a fail of k2 send the issue back, the tester's
#     phase-finished notice carrying k2, no and fail as its one failing check and the implementer's
#     task (read from the daemon's store) carrying that notice's reason; on a second issue a pass and
#     a fail of k1 make the failing check k1 with the reason pass_and_fail;
#   - completion-before-event: the run's own nats-server stopped with SIGSTOP by the pid this run
#     started it with, the tester records its passes, writes its test record and completes; for 10
#     seconds the issue stays in testing, the daemon starts no worker, and the architect is told once,
#     in the tester's notice, whose head is the tested head and whose waits_for is fence; within 10 s
#     of the SIGCONT the issue goes to reviewing.
# Every case also reads the results Dispatch's database holds of each of its issues. Each 10-second
# bound is measured from the operation that triggers the answer (the result's write, the tester's
# completion, the SIGCONT), and an answer that comes later fails at `<assertion>-within`. Each case
# prints `== <case>`, what it drove and read and `ok <case>`; the first that fails ends the run
# non-zero, its line naming the case and the assertion (`FAIL <case> at <assertion>: ...`). A read
# that finds nothing it could judge says UNKNOWN and what it could not read, and its case fails.
#
# Run it from a checkout on a Linux host with go, awk, bash, tar, bun, jj, git and python3, as
#   LEGION_OMP_PATH=/opt/omp/bin/omp \
#     LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     [CHECK_STATES_E2E_CASES=<case>[,<case>...]] bash scripts/e2e/check-states.sh
# LEGION_OMP_PATH is required and names the Oh My Pi every pane runs, an absolute executable: this
# proof launches real agents, and no pin is guessed for it. LEGION_TEST_PG_DSN is required and names
# the PostgreSQL the daemon migrates and runs on, and on which the driver makes Dispatch's database,
# so its role may create databases and the server carries pgvector; this script starts none. That
# server must be this machine's: prerequisites reads the DSN with lib/loopback-dsn.sh's
# require_loopback_dsn and refuses a host that is not 127.0.0.1, localhost or ::1 with exit 2, before
# it builds or starts anything. The broker is the run's own: a nats-server from LEGION_E2E_NATS_BIN,
# or the pinned one installed with `go install`, which Dispatch publishes its events on and the daemon
# consumes them from. CHECK_STATES_EVIDENCE_DIR (default a fresh directory under TMPDIR, or /tmp when
# it is unset, kept and printed) keeps the transcript, the nats-server's log, the daemon's own,
# Dispatch's (dispatch.log), each pane's (panes/) and the driver's output. Nothing reaches github.com
# or a deployed Dispatch. The run around the driver is lib/live-daemon-proof.sh's.
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
for tool in jj git; do
  command -v "$tool" >/dev/null || {
    echo "FAIL setup: $tool is not on PATH; the daemon's boot refuses to start without it" >&2
    exit 1
  }
done
# shellcheck source-path=SCRIPTDIR source=lib/live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"
require_dispatch_cli
# The case completion-before-event stops the run's own broker by the pid the run started it with.
# lib/live-daemon-proof.sh holds that pid in nats_pid; exported here, before the run assigns it, the
# variable reaches the driver it starts.
export nats_pid
live_daemon_proof 'check states' check-states TestCheckStatesLive \
  "${CHECK_STATES_EVIDENCE_DIR:-}"
