#!/usr/bin/env bash
# The live proof of dispatch://INFRA-175 (its check c-live): one approval request covers a root's
# spec and its Verification document, one approval opens both and a new version of either closes
# the gate, and an approved new version reaches the worker holding each affected issue's phase,
# naming what changed, without the daemon moving an issue or stopping a turn. It is one script in
# two parts: a driver with no agent proves what the daemon sends, and real sessions prove what the
# prompts make an agent do, which no driver can stand in for.
#
# Part one is a driver with no agent (packages/daemon/internal/daemon/approved_version_news_live_test.go,
# which this script builds with `-tags e2e`): it builds and starts the real Dispatch server from
# the checkout, with its events on the run's own nats-server and the person `smoke` approving
# through Dispatch's own review route, boots the whole daemon with dispatch_url at it, and speaks
# each role's worker stream and HTTP routes as `legion worker-shim` and the plugin do. Its steps
# run in this order, A2's first, because a daemon from before the change refuses A1's criteria
# registration and the negative control must fail at a delivery rather than there. Each prints
# `== <step>`, what it drove and read, one `   assert <step>: …` line for each check that held,
# and `ok <step>`; the first check that does not hold prints `FAIL <step>: …` and ends the run:
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
#   - a7-the-merger-waits-then-reads-the-opening: with the gate closed the merger is not launched,
#     in the window a merger whose gate is open is launched; the approval launches it with a start
#     task that leads with the gate's opening and names what changed;
#   - r6-only-the-root-is-told-of-its-checks: a root's checks-only approval reaches the root's
#     phase holder, and its children's workers are sent no task;
#   - off-the-daemon-sends-nothing: a second daemon under gates.design off, on Dispatch again; an
#     approved version sends no holder anything, in the window another tree's reviewer is started.
# Part one passes only when every step printed its `ok` and the script counts, for each step, as
# many `assert` lines as it expects (part_one_checks below): the final line names the count it
# made beside the count expected.
#
# Part two is real Oh My Pi sessions
# (packages/daemon/internal/daemon/approved_version_news_sessions_live_test.go, the same `-tags e2e`
# build): real `omp --mode rpc` panes under the real `legion worker-shim`, this checkout's two
# plugins in a profile of the run's own, a real Envoy listener on natsd, both built from the
# checkout, the real Dispatch, and the real model behind the gateway ANTHROPIC_BASE_URL names. It
# runs each of three steps three times in two arms, the head's prompts and the base's (the base
# below): a4-the-architect-tells-the-child-reviewer, off-the-architect-tells-the-holders and
# answers-a-stale-reviewer-moves-back-a-current-one-comments, which the driver's file describes. It
# prints each run's evidence and `run <step> <arm> <n>: PASS|FAIL …`, and this script prints
# `<step> <arm>: <n>/3 runs passed` for each step and arm. Part two passes when every head run
# passed. The base arm is the control of each step: it bites when its count is below the head's,
# and an equal count is printed as no control for that step, which fails nothing. The driver stops
# at the first head run that fails, since the proof then fails whatever the rest show. The model
# key is read from APPROVED_VERSION_NEWS_MODEL_KEY_FILE (default /var/run/model-gateway-key/key,
# where a role pod mounts the gateway's key) by the driver itself, which gives it to each pane's
# environment alone: this script exports no key, so no build, broker or service it starts holds
# one. Once part two ran, the key-leak check counts the key's first 8 characters in the evidence
# directory, without printing them, and fails the run on any.
#
# Run it from a checkout on a Linux host with go, git, tar, awk, bun and python3, as
#   LEGION_OMP_PATH=/opt/omp/bin/omp \
#     LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/approved-version-news.sh [--part one|both] [--control | --base-commit <rev>]
# LEGION_TEST_PG_DSN is required and names a loopback PostgreSQL with the pgvector extension: the
# drivers create their daemon runs' databases and Dispatch's on it and drop them on exit, and the
# arguments check refuses a DSN that is unset or whose host is not loopback with exit 2, before
# anything is built or extracted. LEGION_OMP_PATH (an executable), ANTHROPIC_BASE_URL and the key
# file are part two's, refused with exit 2 when part two would run without them; part one needs
# none. APPROVED_VERSION_NEWS_MODEL names part two's model (default claude-opus-5-5, at the xhigh
# thinking level, the fleet's own: a cheaper model measures itself rather than the prompts). Part
# one's broker is the run's own: a nats-server from LEGION_E2E_NATS_BIN, or the pinned one the lib
# installs with `go install`; part two's driver runs natsd beside it, since the listener is built
# and tested against the checkout's nats-server. APPROVED_VERSION_NEWS_EVIDENCE_DIR (default a
# fresh directory under TMPDIR, or /tmp when it is unset, kept and printed) keeps this script's
# transcript and, in fresh part-one.* and part-two.* directories of their own, the lib's: its
# transcript, the nats-server's log, the daemon's (daemon.log), Dispatch's (dispatch.log) and the
# driver's output (proof.log), and for part two natsd's and the listener's logs, each pane's own
# output (panes/) and each run's session transcripts (transcripts/). Nothing reaches github.com or
# a deployed Dispatch.
#
# The base is the commit before this change: `git merge-base HEAD origin/main`, main's tip as this
# branch last merged it, which holds everything this branch carries but its own change. That is the
# base by default because main moves under the branch and each forward merge brings its changes
# in: a fixed older commit would differ from the head by those too. The driver compiles there, and
# its registeredStep refuses nothing A2 needs, since A2 registers its spec alone. The plan's own
# control runs used 2a6ff6c03269dc9377bf60f8f06e6682df55419e, the base before the forward merges.
# Part two's base arm takes that commit's role prompt files (`git archive <base>
# packages/daemon/internal/prompts/{roles,go}`), which its driver writes over the snapshot the
# daemon takes at boot.
#
# --control runs part one's negative control against the base, and --base-commit <rev> against
# <rev>: `git archive <rev> | tar -x` into a scratch tree under TMPDIR, this checkout's part-one
# driver copied into it, and part one built and run there, daemon and Dispatch both of that
# commit; nothing of this checkout is written. Either runs part one alone, since the control is
# part one's. The run must fail at a2-the-reviewer-is-told-the-changed-sections, past that step's
# setup checks, the one-approval check after them failing by name: the script reports the control
# as biting then, and as no control for a pass, a failure in an earlier check or step, a build
# failure, a killed driver or a failure line that names no step.
#
# Exit 0 is a pass, or a control that bites; exit 2 is an input of the caller's the run refuses,
# here or in the lib's prerequisites; exit 1 is every other failure. The run around each driver is
# lib/live-daemon-proof.sh's, made in a subshell, so this script reads its outcome and the driver's
# output and prints the last line.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
driver_file=packages/daemon/internal/daemon/approved_version_news_live_test.go
driver_test=TestAnApprovedVersionReachesItsHolderLive
sessions_test=TestRealSessionsTellAndAnswerANewVersionLive

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

# Each step of part two, with the word the last line names it by, and how many runs each arm makes
# of it. The driver's own deadline is the lib's for part two: eighteen runs of real sessions took 44
# minutes in a measured run, each bounded at eight.
part_two_steps=(
  a4-the-architect-tells-the-child-reviewer:a4
  off-the-architect-tells-the-holders:off
  answers-a-stale-reviewer-moves-back-a-current-one-comments:answers
)
part_two_runs=3
part_two_timeout=150m

# shellcheck source-path=SCRIPTDIR source=lib/live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"

# The run's own state, which finish reads from the EXIT trap.
version_news_evidence=${APPROVED_VERSION_NEWS_EVIDENCE_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-approved-version-news-evidence.XXXXXXXX")}
base_tree=''
prompts_tree=''
counted=0
expected=0
verdict=''
part_two_said=''
version_news_started=$SECONDS

# finish removes the scratch trees the run made, by their exact paths, and prints the run's last
# line: what part one counted beside what it expects, what part two counted in each arm, or that
# it did not run, and, for the control, whether it bit.
finish() {
  local status=$? said
  for tree in "$base_tree" "$prompts_tree"; do
    [ -z "$tree" ] || rm -rf "$tree"
  done
  said="assertions: $counted/$expected"
  if [ "$parts" = one ]; then
    said="$said; part one alone, part two did not run"
  elif [ -n "$part_two_said" ]; then
    said="$said; part two: $part_two_said"
  fi
  if [ "$status" -eq 0 ] && [ -n "$verdict" ]; then
    echo "approved version news e2e: $verdict in $((SECONDS - version_news_started))s ($said, evidence: $version_news_evidence)"
  elif [ "$status" -eq 0 ]; then
    echo "approved version news e2e: PASS in $((SECONDS - version_news_started))s ($said)"
  else
    echo "approved version news e2e: FAIL at $check after $((SECONDS - version_news_started))s ($said, evidence: $version_news_evidence)" >&2
  fi
}

parts=both
part_given=''
control=''
base_commit=''
mkdir -p "$version_news_evidence"
transcript_to "$version_news_evidence/transcript.log"
trap finish EXIT

begin arguments
while [ $# -gt 0 ]; do
  case "$1" in
    --part)
      [ $# -ge 2 ] || refuse "--part takes one or both"
      parts=$2 part_given=1
      shift 2
      ;;
    --part=*)
      parts=${1#--part=} part_given=1
      shift
      ;;
    --control)
      control=1
      shift
      ;;
    --base-commit)
      [ $# -ge 2 ] || refuse "--base-commit takes the commit part one's negative control runs against"
      base_commit=$2 control=1
      shift 2
      ;;
    --base-commit=*)
      base_commit=${1#--base-commit=} control=1
      shift
      ;;
    *) refuse "unknown argument '$1': this script takes --part <one|both>, --control and --base-commit <rev>" ;;
  esac
done
case "$parts" in
  one | both) ;;
  *) refuse "--part is one or both, not '$parts'" ;;
esac
if [ -n "$control" ]; then
  [ "$parts" = one ] || [ -z "$part_given" ] ||
    refuse "the negative control is part one's and runs part one alone: --part both does not go with --control or --base-commit"
  parts=one
fi
for tool in go git tar awk python3; do
  command -v "$tool" >/dev/null || fail "$tool is not on PATH"
done
[ -n "${LEGION_TEST_PG_DSN:-}" ] ||
  refuse "LEGION_TEST_PG_DSN is required: it names the PostgreSQL the daemon migrates and runs on (postgres://<user>@<host>:<port>/<database>?sslmode=disable). This script starts no database."
# require_loopback_dsn in lib/loopback-dsn.sh says why a caller of the loopback guard clears
# these three. The lib's prerequisites reads the DSN again, inside the run it makes.
unset PGHOSTADDR PGSERVICE PGSERVICEFILE
require_loopback_dsn db_host LEGION_TEST_PG_DSN "$LEGION_TEST_PG_DSN" "this run creates and drops databases there" ||
  refuse "$db_host"
note "store LEGION_TEST_PG_DSN on $db_host (not printed)"
# The base is read only by a run that uses it: the control, and part two's base arm.
base_sha=''
if [ -n "$control" ] || [ "$parts" = both ]; then
  if [ -z "$base_commit" ]; then
    base_commit=$(git -C "$root" merge-base HEAD origin/main 2>/dev/null) ||
      refuse "no merge base of HEAD and origin/main, the base the control and part two's base arm run against: fetch main, or name the control's base with --base-commit <rev>"
  fi
  base_sha=$(git -C "$root" rev-parse --verify "$base_commit^{commit}" 2>/dev/null) ||
    refuse "the base '$base_commit' names no commit of $root"
  note "the base: $base_sha"
fi
for entry in "${part_one_checks[@]}"; do
  expected=$((expected + ${entry##*:}))
done
build_tree=$root
if [ -n "$control" ]; then
  for module in packages/daemon packages/envoy; do
    [ -n "$(git -C "$root" ls-tree --name-only "$base_sha" "$module")" ] ||
      refuse "the base $base_sha holds no $module, so it builds no daemon and no Dispatch to run the control against"
  done
  [ -f "$root/$driver_file" ] || fail "this checkout holds no $driver_file to run against the base"
  note "part one's negative control against $base_sha"
  expected=$control_setup_checks
else
  note "part one against this checkout, $root"
fi
if [ "$parts" = both ]; then
  [ -n "${LEGION_OMP_PATH:-}" ] ||
    refuse "LEGION_OMP_PATH is required: it names the Oh My Pi every pane of part two runs (an absolute executable path). Part two launches real agents and guesses no build"
  [ -x "$LEGION_OMP_PATH" ] || refuse "LEGION_OMP_PATH ($LEGION_OMP_PATH) is not an executable file"
  [ -n "${ANTHROPIC_BASE_URL:-}" ] || refuse "ANTHROPIC_BASE_URL is required: it names the model gateway part two's sessions reach"
  key_file=${APPROVED_VERSION_NEWS_MODEL_KEY_FILE:-/var/run/model-gateway-key/key}
  [ -r "$key_file" ] && [ -s "$key_file" ] ||
    refuse "the model key file $key_file is not a readable file with a key in it: part two's sessions run their turns on it (APPROVED_VERSION_NEWS_MODEL_KEY_FILE names another)"
  command -v bun >/dev/null || fail "bun is not on PATH: part two packs the two plugins with it"
  [ -n "$(git -C "$root" ls-tree --name-only "$base_sha" packages/daemon/internal/prompts/roles)" ] ||
    refuse "the base $base_sha holds no packages/daemon/internal/prompts/roles, so part two has no base prompts"
  # A caller's own key reaches nothing the run starts: the driver reads the key file and gives
  # it to each pane alone.
  unset ANTHROPIC_API_KEY
  note "part two against the base $base_sha's prompts, Oh My Pi $LEGION_OMP_PATH, model key file $key_file (read by the driver, never printed)"
fi
note "parts: $parts; evidence: $version_news_evidence"
pass

if [ -n "$control" ]; then
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
# so no proof.log an earlier run left there is read as this run's. The lib names its checks in the
# subshell alone, so this one is named part-one here for the last line of a run that ends in it.
part_one=$(mktemp -d "$version_news_evidence/part-one.XXXXXX")
check=part-one
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
# The lib's refusal of an input of the caller's is this script's, with its status.
[ "$part_one_status" -ne 2 ] || exit 2

if [ -z "$control" ]; then
  begin assertions
  short=''
  for entry in "${part_one_checks[@]}"; do
    step=${entry%%:*}
    want=${entry##*:}
    got=$(grep -c "^   assert $step: " "$proof" 2>/dev/null || true)
    got=${got:-0}
    counted=$((counted + got))
    grep -q "^ok $step (" "$proof" 2>/dev/null || short="$short $step (no ok line)"
    [ "$got" -eq "$want" ] || short="$short $step ($got of $want)"
    note "$step: $got of $want checks"
  done
  [ "$part_one_status" -eq 0 ] || fail "part one failed (status $part_one_status); its FAIL line is above, and $proof keeps the driver's output"
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

if [ "$parts" = one ]; then
  begin part-two
  note "part two did not run: --part one, --control and --base-commit run part one alone"
  pass
  exit 0
fi

# Part two is the lib's run too, over this checkout, with the base commit's prompt files extracted
# for its base arm into a scratch directory of their own.
prompts_tree=$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-approved-version-news-prompts.XXXXXXXX")
git -C "$root" archive "$base_sha" packages/daemon/internal/prompts/roles packages/daemon/internal/prompts/go |
  tar -x -C "$prompts_tree" || fail "git archive of $base_sha's prompts into $prompts_tree failed"
part_two=$(mktemp -d "$version_news_evidence/part-two.XXXXXX")
check=part-two
set +e
(
  set -e
  export LEGION_E2E_REPO_ROOT=$root APPROVED_VERSION_NEWS_MODEL_KEY_FILE=$key_file \
    APPROVED_VERSION_NEWS_BASE_PROMPTS=$prompts_tree APPROVED_VERSION_NEWS_BASE_COMMIT=$base_sha
  live_daemon_proof 'approved version news part two' approved-version-news-sessions "$sessions_test" "$part_two" "$part_two_timeout"
)
part_two_status=$?
set -e
proof=$part_two/proof.log
[ "$part_two_status" -ne 2 ] || exit 2

# The key's first 8 characters, counted in every file the run kept and printed nowhere: grep reads
# them from a pipe, and only the count of matches leaves it.
begin key-leak
key=$(<"$key_file")
leaks=$({ grep -rFoa -f <(printf '%s\n' "${key:0:8}") "$version_news_evidence" || true; } | wc -l)
unset key
note "the key's first 8 characters occur $leaks times in $version_news_evidence"
[ "$leaks" -eq 0 ] || fail "the model key's first 8 characters are in the evidence directory $leaks times"
pass

begin part-two
head_short=''
for entry in "${part_two_steps[@]}"; do
  step=${entry%%:*}
  word=${entry##*:}
  said="$word"
  for arm in head base; do
    ran=$(grep -cE "^run $step $arm [0-9]+: " "$proof" 2>/dev/null || true)
    got=$(grep -cE "^run $step $arm [0-9]+: PASS " "$proof" 2>/dev/null || true)
    ran=${ran:-0} got=${got:-0}
    line="$step $arm: $got/$part_two_runs runs passed"
    [ "$ran" -eq "$part_two_runs" ] || line="$line ($ran of $part_two_runs ran)"
    note "$line"
    said="$said $arm $got/$part_two_runs"
    if [ "$arm" = head ]; then
      head_got=$got
      [ "$got" -eq "$part_two_runs" ] || head_short="$head_short $word"
    fi
  done
  if [ "$ran" -lt "$part_two_runs" ]; then
    said="$said (base did not run)"
  elif [ "$got" -lt "$head_got" ]; then
    said="$said (control bites)"
  else
    said="$said (no control: the base arm passed as often as the head)"
  fi
  part_two_said="${part_two_said:+$part_two_said; }$said"
done
calls=$(grep -m1 -E '^model calls: ' "$proof" 2>/dev/null || true)
[ -z "$calls" ] || part_two_said="$part_two_said; ${calls%% in all*}"
[ "$part_two_status" -eq 0 ] || fail "part two failed (status $part_two_status); $proof keeps each run's evidence"
[ -z "$head_short" ] || fail "the head arm did not pass every run of:$head_short"
pass
