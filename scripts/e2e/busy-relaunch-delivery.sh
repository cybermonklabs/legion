#!/usr/bin/env bash
# The live proof that a worker in a turn is sent its task once and again only after that turn, that
# the turn which serves a task settles it, and that a relaunched process reads what the relaunch
# ended in the first prompt it is sent (dispatch://INFRA-3276 R4 and R3). The whole daemon is
# booted by the function `legion start` calls, over a real PostgreSQL and a real nats-server, and
# five real `omp --mode rpc` panes run under it — the real `legion worker-shim`, this checkout's
# `legion`, and this checkout's @sjawhar/pi-envoy and @sjawhar/pi-legion in an Oh My Pi profile of
# the run's own — against HTTP model stand-ins of the driver's own and, for one pane, a Hindsight
# memory stand-in whose held recall is the measured wait between Oh My Pi's acknowledgement of a
# prompt and that prompt's turn (packages/daemon/internal/daemon/busy_relaunch_live_test.go, which
# this script builds with `-tags e2e`, and packages/daemon/internal/daemon/localruntime_live_test.go,
# the runtime that starts a pane with no tmux). Each step prints `== <name>`, what it observed and
# `ok <name>`, or `FAIL <name> at <assertion>`; each runs on its own, so one that fails does not
# keep the next from running, and the run fails when any step failed.
#
#   busy-late-turn               a phase worker's task whose prompt waits on a held recall past the
#                                daemon's five-second wait for its turn: at most one busy answer,
#                                the late turn confirms the task, its result retires it, nothing is
#                                sent after it, and no prompt result of the claim is dropped as stale
#   busy-own-turn                a task queued while a turn of Oh My Pi's own runs — the one an
#                                `async` bash job's result starts — is answered busy at most once
#                                and sent only after that turn ends
#   newer-task                   a task `legion claims deliver` queues during a late turn is taken
#                                and sent after that turn ends
#   idle-control                 a worker in no turn is sent its task at once
#   relaunch-subagent-worker     a phase worker holding no task dies while a `task` subagent runs:
#                                the relaunched process's first prompt names the subagent and the
#                                death, and the claim row's ended_work records it
#   relaunch-subagent-architect  the same for a root architect, holding no task and then holding
#                                one in its turn, with the catch-up notice opening with the sentence
#   relaunch-task                a worker dies in its task's turn with an `async` bash job running:
#                                the claim row records the turn and the job, and the task is sent
#                                again behind the sentence naming both
#
# Run it from a checkout on a Linux host with go, awk, bash, tar, bun and python3, as
#   LEGION_OMP_PATH=/opt/omp/bin/omp \
#     LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/busy-relaunch-delivery.sh [--base-commit <rev>]
# LEGION_OMP_PATH is required and names the Oh My Pi every pane runs, an absolute executable: this
# proof launches real agents, and no pin is guessed for it. In a Legion worker pod that binary is
# the fleet's build rather than the branch's fetch, and the driver prints which build it ran.
# LEGION_TEST_PG_DSN is required and names the PostgreSQL the daemon migrates and runs on — CI's
# service, a devbox container and a pod's own embedded PostgreSQL all satisfy it on a loopback
# address, and this script starts none. That server must be this machine's: prerequisites reads
# the DSN with lib/loopback-dsn.sh's require_loopback_dsn and refuses a host that is not
# 127.0.0.1, localhost or ::1 with exit 2, before it builds or starts anything; with
# --base-commit the same guard runs first, before the base tree is taken, and a refusal of the base
# run is this script's exit 2 as well.
# The broker is the run's own: a nats-server from LEGION_E2E_NATS_BIN, or the pinned
# one this script installs with `go install` into a GOBIN of its own, started with JetStream on a
# port of the run's own and stopped on any exit. BUSY_RELAUNCH_DELIVERY_EVIDENCE_DIR (default a
# fresh directory under TMPDIR, or /tmp when it is unset, kept and printed) keeps the transcript,
# the nats-server's log, the daemon's own, each pane's output and the driver's output; the run's
# work directory, the driver binary among it, is under TMPDIR too, since a pod's own disk is small
# and its TMPDIR names the issue's volume. The driver builds `legion` and packs the two plugins
# from the checkout it was built from, so it needs bun and that checkout's node_modules. It needs
# no kube context and no tmux — the panes are child process groups of the driver — so it runs in a
# Legion pod and on a devbox alike. The run around the driver is lib/live-daemon-proof.sh's.
#
# --base-commit <rev> is the negative control, and an empty <rev> is refused with exit 2. The tree
# of <rev> is taken with `git archive` into a directory of the run's own under TMPDIR (removed on
# any exit), this checkout's driver file is laid over it, `bun install --frozen-lockfile` gives it
# node_modules of its own, and the same run is made there: the driver, `legion` and both plugins
# are built from that tree. The driver names no Go symbol the base lacks — it reads the new claim
# column through SQL and the relaunch sentence as literals — so it builds there. The control passes
# when that run fails and each of five steps fails at the assertion it names: busy-late-turn at
# stale-result (the base drops the late turn's result and sends the task again), newer-task at
# accepted (`a delivery is already pending`), relaunch-subagent-worker and
# relaunch-subagent-architect at sentence (the base names no work in what it sends a relaunched
# process), and relaunch-task at claim-row (the base's schema has no ended_work); what
# busy-own-turn and idle-control did there is printed as it came. A proof.log an earlier run left
# in BUSY_RELAUNCH_DELIVERY_EVIDENCE_DIR is removed before the base run, so only that run's steps
# are read. Its last line is `busy relaunch delivery negative control: PASS|FAIL …` with the base
# run's evidence directory.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
base_commit=''
base_given=false
while [ $# -gt 0 ]; do
  case "$1" in
    --base-commit)
      if [ $# -lt 2 ]; then
        echo "FAIL setup: --base-commit takes the commit the negative control's tree is taken from" >&2
        exit 2
      fi
      base_commit=$2
      base_given=true
      shift 2
      ;;
    --base-commit=*)
      base_commit=${1#--base-commit=}
      base_given=true
      shift
      ;;
    *)
      echo "FAIL setup: unknown argument '$1': this script takes --base-commit <rev> alone" >&2
      exit 2
      ;;
  esac
done
if [ "$base_given" = true ] && [ -z "$base_commit" ]; then
  echo "FAIL setup: --base-commit was given an empty commit; it takes the commit the negative control's tree is taken from" >&2
  exit 2
fi
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

test_name=TestABusyAgentIsSentItsTaskOnceAndARelaunchSaysWhatItEndedLive
driver_file=packages/daemon/internal/daemon/busy_relaunch_live_test.go

# The steps the base must fail, each with the assertion it must fail at (the driver's `fails`).
base_failures=(
  'busy-late-turn stale-result'
  'newer-task accepted'
  'relaunch-subagent-worker sentence'
  'relaunch-subagent-architect sentence'
  'relaunch-task claim-row'
)

# The negative control's own state, global because its EXIT trap reads it.
base_work=''
base_evidence=''

control_cleanup() {
  local status=$?
  [ -z "$base_work" ] || rm -rf "$base_work"
  if [ "$status" -eq 0 ]; then
    echo "busy relaunch delivery negative control: PASS in $((SECONDS - started))s (evidence: $base_evidence)"
  else
    echo "busy relaunch delivery negative control: FAIL at $check after $((SECONDS - started))s (evidence: $base_evidence)" >&2
  fi
}

# negative_control makes the run over the base tree in a subshell of its own — live_daemon_proof
# ends the shell it runs in, and its EXIT trap is that subshell's — and then reads the base run's
# proof.log for each step's own failure.
negative_control() {
  local base_sha status expected step assertion line detail tool db_host
  started=$SECONDS
  base_evidence=${BUSY_RELAUNCH_DELIVERY_EVIDENCE_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-busy-relaunch-delivery-base-evidence.XXXXXXXX")}
  mkdir -p "$base_evidence"
  trap control_cleanup EXIT
  transcript_to "$base_evidence/negative-control.log"

  begin base-tree
  # The base run's own prerequisites read the DSN too, but only after the archive and the install
  # below; a DSN it would refuse is refused here first, with the same guard and the same exit 2.
  [ -n "${LEGION_TEST_PG_DSN:-}" ] ||
    refuse "LEGION_TEST_PG_DSN is required: it names the PostgreSQL the daemon migrates and runs on (postgres://<user>@<host>:<port>/<database>?sslmode=disable). This script starts no database."
  for tool in git tar bun python3; do
    command -v "$tool" >/dev/null || fail "$tool is not on PATH"
  done
  # require_loopback_dsn in lib/loopback-dsn.sh says why a caller of the loopback guard clears
  # these three, and why the call comes after the tool check above.
  unset PGHOSTADDR PGSERVICE PGSERVICEFILE
  require_loopback_dsn db_host LEGION_TEST_PG_DSN "$LEGION_TEST_PG_DSN" "this run creates and drops a database there" ||
    refuse "$db_host"
  base_sha=$(git -C "$root" rev-parse --verify "$base_commit^{commit}" 2>/dev/null) ||
    refuse "--base-commit '$base_commit' names no commit of this repository"
  base_work=$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-busy-relaunch-delivery-base.XXXXXXXX")
  git -C "$root" archive "$base_sha" | tar -x -C "$base_work" ||
    fail "could not take the tree of $base_sha"
  cp "$root/$driver_file" "$base_work/$driver_file" ||
    fail "could not lay $driver_file over the tree of $base_sha"
  (cd "$base_work" && bun install --frozen-lockfile) >"$base_evidence/bun-install.log" 2>&1 ||
    fail "bun install in the tree of $base_sha failed; see $base_evidence/bun-install.log"
  note "the tree of $base_sha in $base_work, with this checkout's $driver_file laid over it and node_modules of its own"
  pass

  # The evidence directory may be one an earlier run used: its proof.log would answer for steps
  # this run never reached.
  rm -f "$base_evidence/proof.log"
  set +e
  (
    set -e
    root=$base_work
    live_daemon_proof "busy relaunch delivery at ${base_sha:0:12}" busy-relaunch-delivery-base "$test_name" "$base_evidence"
  )
  status=$?
  set -e
  # A refusal of the caller's input inside the base run is this script's refusal too, not a
  # failure of the base; the refusal's own line is in the base run's transcript.
  if [ "$status" -eq 2 ]; then
    check=base-run
    exit 2
  fi

  begin negative-control
  [ "$status" -ne 0 ] || fail "the run over $base_sha passed every step, so this proof tells it from this checkout in nothing"
  grep -q '^== relaunch-task$' "$base_evidence/proof.log" 2>/dev/null ||
    fail "the run over $base_sha reached no last step: it failed before its proof; see $base_evidence/transcript.log"
  for expected in "${base_failures[@]}"; do
    step=${expected% *}
    assertion=${expected#* }
    line=$(grep -m1 -E "^FAIL $step at " "$base_evidence/proof.log") ||
      fail "$step passed over $base_sha; it was to fail at $assertion"
    case "$line" in
      "FAIL $step at $assertion ("*) ;;
      *) fail "$step failed over $base_sha elsewhere than at $assertion: $line" ;;
    esac
    # The assertion's own sentence is the driver's t.Fatalf line inside that step's own output,
    # between its `== <step>` and its `FAIL <step>` lines: two steps can fail one assertion.
    detail=$(awk -v step="$step" -v said=": $assertion: " '
      $0 == "== " step { inside = 1; next }
      inside && index($0, "FAIL " step " at ") == 1 { exit }
      inside && $0 ~ /busy_relaunch_live_test\.go:[0-9]+: / && index($0, said) { print; exit }
    ' "$base_evidence/proof.log")
    note "$line"
    note "  ${detail#"${detail%%[![:space:]]*}"}"
  done
  for step in busy-own-turn idle-control; do
    line=$(grep -m1 -E "^(ok|FAIL) $step " "$base_evidence/proof.log") || line="no outcome for $step"
    note "as it came: $line"
  done
  pass
}

if [ -z "$base_commit" ]; then
  live_daemon_proof 'busy relaunch delivery' busy-relaunch-delivery "$test_name" \
    "${BUSY_RELAUNCH_DELIVERY_EVIDENCE_DIR:-}"
else
  negative_control
fi
