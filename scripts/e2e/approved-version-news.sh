#!/usr/bin/env bash
# The live proof of dispatch://INFRA-175 (its check c-live): one approval request covers a root's
# spec and its Verification document, one approval opens both and a new version of either closes
# the gate, and an approved new version reaches the worker holding each affected issue's phase,
# naming what changed, without the daemon moving an issue or stopping a turn. It is one script in
# two parts (the plan's departure 11).
#
# Part one is a driver with no agent (packages/daemon/internal/daemon/approved_version_news_live_test.go,
# which this script builds with `-tags e2e`): it builds and starts the real Dispatch server from
# the checkout, with its events on the run's own nats-server and the person `smoke` approving
# through Dispatch's own review route, boots the whole daemon with dispatch_url at it, and speaks
# each role's worker stream and HTTP routes as `legion worker-shim` and the plugin do. Its steps
# run in this order, so that the run's first approval of a new version is A2's (departure 10), and
# each prints `== <step>`, what it drove and read, one `   assert <step>: …` line for each check
# that held, and `ok <step>`; the first check that does not hold prints `FAIL <step>: …` and ends
# the run:
#   - a2-the-reviewer-is-told-the-changed-sections: a tree registering its spec alone; the task of
#     an approved version that changed `## Design` waits behind the reviewer's open turn, then
#     names `Design` and both versions; the issue stays in reviewing until the reviewer's
#     request_backward_move; a tree whose gate did not move is sent nothing in that window;
#   - a1-one-request-opens-both-rows: one request names both documents, register_gate takes
#     criteria then spec under root-issues, one approval opens both rows of GET /legion/v1/state's
#     gatedSteps, and a new Verification version closes the criteria row;
#   - a3-a-checks-change-reaches-accepting-and-implementing: a checks-only approval reaches the
#     root architect for a root in accepting and the implementer for one in implementing; an
#     answered decision block alone sends nothing, in a window the accepting root's task arrives in;
#   - a5-the-tester-passes-and-the-reviewer-is-told: a tester in a turn at a checks-only approval
#     completes with a pass, and the reviewer's start task names the changed check, while the
#     start task of the reviewer of the root's child, whose issue that version changed nothing of,
#     names none;
#   - a6-a-merged-root-is-told-nothing: a root in merging on a pull request GitHub reports merged
#     is sent nothing and its merger's request_backward_move is refused naming the merge, while
#     its unmerged child in reviewing is sent the task;
#   - a7-the-merger-waits-then-reads-the-opening: with the gate closed the merger is not launched
#     and nothing merges, in the window a merger whose gate is open is launched; the approval
#     launches it with a start task that leads with the gate's opening and names what changed;
#   - r6-only-the-root-is-told-of-its-checks: a root's checks-only approval reaches the root's
#     phase holder, and its children's workers are sent no task;
#   - off-the-daemon-sends-nothing: a second daemon under gates.design off, on Dispatch again; an
#     approved version sends no holder anything, in the window another tree's reviewer is started.
# Part one passes only when every step printed its `ok` and the script counts, for each step, as
# many `assert` lines as it expects (part_one_checks below): the final line names the count it
# made beside the count expected.
#
# Part two is real Oh My Pi sessions on the head's prompts against the base commit's (the A4 step,
# the architect's message under gates.design off, and the workers' answers). It is not in this
# script yet. A run of both parts, the default, runs part one and then fails at `part-two`, saying
# so, so the c-live command never passes without it; `--part one` runs part one alone and says
# that part two did not run.
#
# Run it from a checkout on a Linux host with go, git, tar, awk and python3, as
#   LEGION_OMP_PATH=/opt/omp/bin/omp \
#     LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/approved-version-news.sh [--part one] [--base-commit <rev>]
# LEGION_TEST_PG_DSN is required and names a loopback PostgreSQL with the pgvector extension: the
# driver creates its two daemon runs' databases and Dispatch's on it and drops them on exit, and
# lib/live-daemon-proof.sh refuses a DSN whose host is not loopback with exit 2 before it builds or
# starts anything. LEGION_OMP_PATH is part two's, which runs real sessions; part one needs none.
# The broker is the run's own: a nats-server from LEGION_E2E_NATS_BIN, or the pinned one the lib
# installs with `go install`. APPROVED_VERSION_NEWS_EVIDENCE_DIR (default a fresh directory under
# TMPDIR, or /tmp when it is unset, kept and printed) keeps this script's transcript and, in a
# fresh part-one.* directory of its own, the lib's: its transcript, the nats-server's log, the
# daemon's (daemon.log), Dispatch's (dispatch.log) and the driver's output (proof.log). Nothing
# reaches github.com or a deployed Dispatch.
#
# --base-commit <rev> runs part one's negative control: `git archive <rev> | tar -x` into a scratch
# tree under TMPDIR, this checkout's driver file copied into it, and part one built and run there,
# daemon and Dispatch both of that commit; nothing of this checkout is written. The run must fail
# at a2-the-reviewer-is-told-the-changed-sections, past that step's setup checks, the one-approval
# check after them failing by name: the script reports the control as biting then, and as no
# control for a pass, a failure in an earlier check or step, a build failure, a killed driver or a
# failure line that names no step. The base is the commit before this change; the plan names
# 2a6ff6c03269dc9377bf60f8f06e6682df55419e.
#
# Exit 0 is a pass, or a control that bites; exit 2 is an input of the caller's the run refuses;
# exit 1 is every other failure. The run around the driver is lib/live-daemon-proof.sh's, made in a
# subshell, so this script reads its outcome and the driver's output and prints the last line.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
driver_file=packages/daemon/internal/daemon/approved_version_news_live_test.go
driver_test=TestAnApprovedVersionReachesItsHolderLive

# Each step of part one and how many checks it makes, in the order the driver runs them. The
# driver prints one `   assert <step>: …` line per check that held; a step that made fewer checks
# than it says here, or more, fails the run however it ended.
part_one_checks=(
  a2-the-reviewer-is-told-the-changed-sections:11
  a1-one-request-opens-both-rows:5
  a3-a-checks-change-reaches-accepting-and-implementing:7
  a5-the-tester-passes-and-the-reviewer-is-told:8
  a6-a-merged-root-is-told-nothing:5
  a7-the-merger-waits-then-reads-the-opening:7
  r6-only-the-root-is-told-of-its-checks:3
  off-the-daemon-sends-nothing:5
)
# The step the negative control must fail at, and how many of its checks come before its first
# one-approval check: the two trees' setup approvals, the new version that closes the gate, the
# reviewer's open turn and the approval that opens the gate again, each of which holds on a daemon
# from before the change too. A control that fails earlier failed for another reason.
control_step=a2-the-reviewer-is-told-the-changed-sections
control_setup_checks=5

# shellcheck source-path=SCRIPTDIR source=lib/live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"

parts=both
base_commit=''
while [ $# -gt 0 ]; do
  case "$1" in
    --part)
      [ $# -ge 2 ] || refuse "--part takes one or both"
      parts=$2
      shift 2
      ;;
    --part=*)
      parts=${1#--part=}
      shift
      ;;
    --base-commit)
      [ $# -ge 2 ] || refuse "--base-commit takes the commit part one's negative control runs against"
      base_commit=$2
      shift 2
      ;;
    --base-commit=*)
      base_commit=${1#--base-commit=}
      shift
      ;;
    *) refuse "unknown argument '$1': this script takes --part <one|both> and --base-commit <rev>" ;;
  esac
done

# The run's own state, which finish reads from the EXIT trap.
version_news_evidence=${APPROVED_VERSION_NEWS_EVIDENCE_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-approved-version-news-evidence.XXXXXXXX")}
base_tree=''
counted=0
expected=0
verdict=''
version_news_started=$SECONDS

# finish removes the scratch tree the control made, by its exact path, and prints the run's last
# line: what part one counted beside what it expects, whether part two ran, and, for the control,
# whether it bit.
finish() {
  local status=$? said
  if [ -n "$base_tree" ]; then
    rm -rf "$base_tree"
  fi
  said="assertions: $counted/$expected"
  if [ "$parts" = one ]; then
    said="$said; part one alone, part two did not run"
  fi
  if [ "$status" -eq 0 ] && [ -n "$verdict" ]; then
    echo "approved version news e2e: $verdict in $((SECONDS - version_news_started))s ($said, evidence: $version_news_evidence)"
  elif [ "$status" -eq 0 ]; then
    echo "approved version news e2e: PASS in $((SECONDS - version_news_started))s ($said)"
  else
    echo "approved version news e2e: FAIL at $check after $((SECONDS - version_news_started))s ($said, evidence: $version_news_evidence)" >&2
  fi
}

mkdir -p "$version_news_evidence"
transcript_to "$version_news_evidence/transcript.log"
trap finish EXIT

begin arguments
case "$parts" in
  one | both) ;;
  *) refuse "--part is one or both, not '$parts'" ;;
esac
for entry in "${part_one_checks[@]}"; do
  expected=$((expected + ${entry##*:}))
done
build_tree=$root
if [ -n "$base_commit" ]; then
  for tool in git tar; do
    command -v "$tool" >/dev/null || fail "$tool is not on PATH; the control takes the base commit's tree with git archive | tar -x"
  done
  base_sha=$(git -C "$root" rev-parse --verify "$base_commit^{commit}" 2>/dev/null) ||
    refuse "--base-commit '$base_commit' names no commit of $root"
  for module in packages/daemon packages/envoy; do
    [ -n "$(git -C "$root" ls-tree --name-only "$base_sha" "$module")" ] ||
      refuse "--base-commit $base_sha holds no $module, so it builds no daemon and no Dispatch to run the control against"
  done
  [ -f "$root/$driver_file" ] || fail "this checkout holds no $driver_file to run against the base"
  note "part one's negative control against $base_sha"
  expected=$control_setup_checks
else
  note "part one against this checkout, $root"
fi
note "parts: $parts; evidence: $version_news_evidence"
pass

if [ -n "$base_commit" ]; then
  begin base-tree
  base_tree=$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-approved-version-news-base.XXXXXXXX")
  git -C "$root" archive "$base_sha" | tar -x -C "$base_tree" || fail "git archive $base_sha | tar -x into $base_tree failed"
  cp "$root/$driver_file" "$base_tree/$driver_file" || fail "could not copy $driver_file into $base_tree"
  build_tree=$base_tree
  note "$base_sha extracted into $base_tree, with this checkout's $driver_file"
  pass
fi

# Part one is lib/live-daemon-proof.sh's run, in a subshell of its own so that its failure is this
# script's to read rather than its exit: the lib, sourced from this checkout, builds the driver from
# the build tree's packages/daemon, and the driver builds Dispatch from the tree LEGION_E2E_REPO_ROOT
# names. Its own transcript and logs go in a fresh part-one.* directory of the evidence directory,
# so no proof.log an earlier run left there is read as this run's.
part_one=$(mktemp -d "$version_news_evidence/part-one.XXXXXX")
set +e
(
  set -e
  root=$build_tree
  export LEGION_E2E_REPO_ROOT=$build_tree
  live_daemon_proof 'approved version news part one' approved-version-news "$driver_test" "$part_one"
)
part_one_status=$?
set -e
proof=$part_one/proof.log

if [ -z "$base_commit" ]; then
  begin assertions
  [ "$part_one_status" -eq 0 ] || fail "part one failed (status $part_one_status); its FAIL line is above, and $proof keeps the driver's output"
  short=''
  for entry in "${part_one_checks[@]}"; do
    step=${entry%%:*}
    want=${entry##*:}
    got=$(grep -c "^   assert $step: " "$proof" || true)
    counted=$((counted + got))
    grep -q "^ok $step (" "$proof" || short="$short $step (no ok line)"
    [ "$got" -eq "$want" ] || short="$short $step ($got of $want)"
    note "$step: $got of $want checks"
  done
  [ -z "$short" ] || fail "part one's checks are not the ones it expects:$short"
  pass
else
  begin control
  counted=$(grep -c "^   assert $control_step: " "$proof" 2>/dev/null || true)
  counted=${counted:-0}
  failure=$(grep -m1 '^FAIL ' "$proof" 2>/dev/null || true)
  if [ "$part_one_status" -eq 0 ]; then
    fail "no control: part one passed against $base_sha"
  elif [ ! -s "$proof" ]; then
    fail "no control: the base run failed (status $part_one_status) before its driver ran; see $part_one/transcript.log"
  elif [ -z "$failure" ]; then
    fail "no control: the base run's driver ended (status $part_one_status) with no FAIL line naming a step, a killed or timed-out run; see $proof"
  elif [[ "$failure" != "FAIL $control_step: "* ]]; then
    fail "no control: the base run failed somewhere other than $control_step: $failure"
  elif [ "$counted" -ne "$control_setup_checks" ]; then
    fail "no control: the base run failed at $control_step after $counted of its checks, not at its first one-approval check after the $control_setup_checks setup checks: $failure"
  fi
  note "the base run failed at $control_step after its $counted setup checks held:"
  note "$failure"
  verdict="control bites at $control_step against $base_sha"
  pass
fi

begin part-two
if [ "$parts" = one ]; then
  note "part two did not run: --part one runs part one alone"
  pass
else
  fail "part two of c-live (real Oh My Pi sessions on the head's prompts against the base's: A4, the architect's message under gates.design off, the workers' answers) is not in this script yet, so the c-live command does not pass; --part one runs part one alone"
fi
