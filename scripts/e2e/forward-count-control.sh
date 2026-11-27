#!/usr/bin/env bash
# The live control of the count of returns from the retro and the merge (C7 of
# the forward-only design, check `c-forward-count-control`). It drives
# packages/daemon/internal/daemon/forward_count_live_test.go, which this script builds with
# `-tags e2e`: nine issues in three projects (ACME, DECOY, CLEAR) walk through three real daemons
# over one real Dispatch, each return is the role's own `request_backward_move` through the
# daemon's route, and the count's `collect` then reads Dispatch, the run's GitHub stand-in and the
# DECOY mergers' transcripts. The driver runs `collect` itself, because the Dispatch and the
# GitHub it reads live inside the driver's own process; this script runs `resolve` after it and
# judges each project's summary, line by line.
#
#   LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/forward-count-control.sh [--control unread|no-failure|blocked|captured]
#
# It passes only on the eight issue lines of C7's table and the summary lines beside them, each
# matched whole (`grep -x`). The program writes each line, so this script's own echo of a line
# cannot match it.
#
# Its four controls, each of which bites on its own line and leaves every other line standing:
#
#   --control unread      `resolve` is given no readings. ACME and DECOY must read `verdict:
#                         unknown`, and every return of theirs `pending`.
#   --control no-failure  the stand-in reports the check run `unit-limits` passing at ACME-2's
#                         head. ACME-2 must read `unexplained`, where it reads `exempt:ci`.
#   --control blocked     DECOY-2's timeline holds a removal from the merge queue at its head,
#                         after the transcript time of its `clear` answer. DECOY-2 must read
#                         `pending`, because reader 1 leaves it out and the baseline has no reader 2.
#   --control captured    DECOY-4's transcript gains the result of a `curl` to the GraphQL route
#                         that reads its head `CONFLICTING`. DECOY-4 must read `mixed`, and
#                         DECOY-5 `pending` in the same run.
#
# Run it from a checkout on a Linux host with go, git, jj 0.38 or later (the daemon's boot refuses
# to start without them), bun (after `bun install` at the checkout's root), awk and python3.
# LEGION_TEST_PG_DSN is required and names the PostgreSQL the daemons migrate and run on; the
# driver also creates Dispatch's database on the same server and drops it on exit. That server
# must be this machine's: the run refuses a host that is not loopback before it builds anything.
# The broker is the run's own nats-server. FORWARD_COUNT_CONTROL_EVIDENCE_DIR (default a fresh
# directory under TMPDIR, kept and printed) holds the transcript, the logs, the DECOY transcripts,
# the count's `returns.jsonl` and each summary. Nothing reaches github.com or a deployed Dispatch.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
usage="usage: scripts/e2e/forward-count-control.sh [--control unread|no-failure|blocked|captured]"
control=""
while [ "$#" -gt 0 ]; do
  case $1 in
    --control)
      control=${2:-}
      shift 2 || true
      ;;
    --control=*) control=${1#--control=} && shift ;;
    -h | --help)
      echo "$usage"
      exit 0
      ;;
    *)
      echo "$usage" >&2
      exit 2
      ;;
  esac
done
case $control in
  "" | unread | no-failure | blocked | captured) ;;
  *)
    echo "forward-count-control: no control is named $control" >&2
    echo "$usage" >&2
    exit 2
    ;;
esac

# shellcheck source-path=SCRIPTDIR source=lib/live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"
require_dispatch_cli

evidence=${FORWARD_COUNT_CONTROL_EVIDENCE_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-forward-count-evidence.XXXXXXXX")}
mkdir -p "$evidence"

# The driver runs in a subshell of its own, so its EXIT trap is that subshell's and this script
# goes on to read what it wrote (as approved-version-news.sh and busy-relaunch-delivery.sh do).
# `unread` changes no part of the run: it is this script's own control over the readings.
driver_control=$control
[ "$driver_control" != unread ] || driver_control=""
status=0
(
  export FORWARD_COUNT_CONTROL="$driver_control"
  live_daemon_proof 'forward count control' forward-count TestForwardCountControlEventsLive "$evidence" 50m
) || status=$?
if [ "$status" -ne 0 ]; then
  echo "FAIL the run: the driver exited $status; see $evidence/proof.log and $evidence/daemon.log" >&2
  exit "$status"
fi

count="$evidence/count"
readings=("--readings" "$root/packages/pi-legion/scripts/forward-count/control-readings.jsonl")
[ "$control" != unread ] || readings=()

echo "== resolve"
if ! bun "$root/packages/pi-legion/scripts/forward-count/forward-count.ts" resolve --out "$count" "${readings[@]}" \
  >"$evidence/resolve.log" 2>&1; then
  echo "FAIL resolve: the count's resolve failed; see $evidence/resolve.log" >&2
  exit 1
fi
if [ "${#readings[@]}" -eq 0 ]; then
  echo "   resolve read no readings, as the control unread asks"
else
  echo "   resolve read the control's readings (${readings[1]})"
fi
echo "ok resolve"

# The lines each project's summary must hold, matched whole. The plain run first; a control then
# replaces the one line it bites on, and every other line stands.
acme=("ACME-1 against-R23" "ACME-2 exempt:ci" "ACME-3 goal-check" "against R23: 1" "verdict: fail")
decoy=("DECOY-1 against-R23" "DECOY-2 against-R23" "DECOY-3 mixed" "DECOY-4 pending" "DECOY-5 pending"
  "against R23: 2" "pending: 2" "verdict: fail")
clear=("verdict: pass" "last steps passed: 1")
bit=""
case $control in
  unread)
    acme=("ACME-1 pending" "ACME-2 pending" "ACME-3 pending" "against R23: 0" "verdict: unknown")
    decoy=("DECOY-1 pending" "DECOY-2 pending" "DECOY-3 pending" "DECOY-4 pending" "DECOY-5 pending"
      "against R23: 0" "pending: 5" "verdict: unknown")
    bit="ACME and DECOY read 'verdict: unknown' where they read 'verdict: fail'"
    ;;
  no-failure)
    acme=("ACME-1 against-R23" "ACME-2 unexplained" "ACME-3 goal-check" "against R23: 1" "verdict: fail")
    bit="ACME-2 reads 'unexplained' where it reads 'exempt:ci'"
    ;;
  blocked)
    decoy=("DECOY-1 against-R23" "DECOY-2 pending" "DECOY-3 mixed" "DECOY-4 pending" "DECOY-5 pending"
      "against R23: 1" "pending: 3" "verdict: fail")
    bit="DECOY-2 reads 'pending' where it reads 'against-R23': reader 1 leaves it out and the baseline has no reader 2"
    ;;
  captured)
    decoy=("DECOY-1 against-R23" "DECOY-2 against-R23" "DECOY-3 mixed" "DECOY-4 mixed" "DECOY-5 pending"
      "against R23: 2" "pending: 1" "verdict: fail")
    bit="DECOY-4 reads 'mixed' where it reads 'pending', and DECOY-5 reads 'pending' in the same run"
    ;;
esac

echo "== the summaries"
faults=0
judge() {
  local project=$1 summary="$count/summary-$1.txt" line
  shift
  if [ ! -f "$summary" ]; then
    echo "FAIL the summaries: $summary is no file" >&2
    faults=$((faults + 1))
    return
  fi
  for line in "$@"; do
    if grep -qxF -- "$line" "$summary"; then
      echo "   $project: $line"
    else
      echo "FAIL the summaries: $project holds no line '$line' ($summary)" >&2
      faults=$((faults + 1))
    fi
  done
}
judge ACME "${acme[@]}"
judge DECOY "${decoy[@]}"
judge CLEAR "${clear[@]}"
if [ "$faults" -ne 0 ]; then
  echo "FAIL the summaries: $faults lines the count did not write; see $count" >&2
  exit 1
fi
echo "ok the summaries"

if [ -n "$control" ]; then
  echo "forward count control e2e: PASS with the control $control: $bit (evidence: $evidence)"
else
  echo "forward count control e2e: PASS with no control: the eight issue lines of C7's table and each summary line (evidence: $evidence)"
fi
