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
# each role's worker stream and HTTP routes as `legion worker-shim` and the plugin do. Every claim
# it tells is started by the daemon's own start path, at the entry production gives it, and the
# boot fails the run as `FAIL setup: …` where a claim stands anywhere else, or where a told record
# row the driver writes by hand differs from the row that path writes. Its steps run in this
# order, A2's first, because a daemon from before the change refuses A1's criteria registration
# and the negative control must fail at a delivery rather than there. Each prints
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
#     task that holds the start's own text, a blank line, and then the news, which opens with the
#     gate's opening and names what changed;
#   - r6-only-the-root-is-told-of-its-checks: a root's checks-only approval reaches the root's
#     phase holder and the root's architect, each at the entry the daemon's start path gave it,
#     and its children's workers are sent no task;
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
# runs the steps of the driver's step table, each as many times as the table says in each arm the
# table runs it in, the head's prompts and the base's (the base below): today
# a4-the-architect-tells-the-child-reviewer and
# answers-a-stale-reviewer-moves-back-a-current-one-comments three times in the head arm alone, and
# off-the-architect-tells-the-holders three times in each, which the driver's file describes. The
# plan check builds the driver first, the one build both parts then run, and runs it with
# -part-two-plan, which prints that plan (each step, its arms and the count of runs) and makes no
# run, and this script counts each step's arms and the driver's deadline from it, keeping no copy
# of the steps, their arms or the count. It prints each run's evidence and
# `run <step> <arm> <n>: <outcome> …`. PASS and FAIL are runs that
# reached the step's assertion. SETUP is a run that stopped before it, such as a holder that did
# not end its start turn in the run's bound, or an architect that did not write the version its
# task asks for. The a4 and off steps assert the telling of that version. HARNESS is a run the
# harness failed, before or after the step's own outcome, or one in which a session's `legion`
# tool could not read the daemon: the plugin refused the daemon's answer as a body the daemon API
# contract does not describe. The driver prints a run's line after it stops the run's daemon, so a
# fault in that stop makes the run HARNESS too. The driver ends the proof at a
# HARNESS run in either arm. This script counts PASS and FAIL alone for an arm, prints `<step>
# <arm>: <p> of <n> runs that reached the assertion passed` for each step and arm it runs, and
# names each run it did not count. Part two passes when every head run reached its assertion and
# passed: a head run that stopped before its assertion fails the run, and its FAIL line names it.
# The base arm is the control of each step it runs: it bites when a base run failed at its
# assertion. A base arm that passed every run that reached its assertion is printed as no control
# for that step, which fails nothing, and a setup failure is never counted as the control biting.
# The base arms of a4 and answers are retired, each by the measurement its entry of head_only
# below holds: each passed every one of its ten runs at the base, so the arms cannot be separated
# there, for a reason of its own. a4 runs through the child's own architect, which main's #4203
# gives every started child, and main's own text meets R4 since #4203 (a child's own architect
# tells its workers of a version once its parent approves it). The rule answers tests is written
# in the daemon's own news task (holderRule), which both arms run, since the daemon is the head's
# in both. The last line says so in each one's place: `a4 head 3/3 (head only: <why>)`, and
# answers' the same. Those measurements are the one fact of part two's steps the driver does not
# know, and the plan check fails the run, naming the step, where the driver's plan makes a step in
# the head arm alone that head_only holds no measurement for, or makes a step head_only holds one
# for in both arms or not at all. off is the one step whose control can bite at its base. The driver
# stops at the first head run that does not pass, since the proof then fails whatever the rest
# show. The model key is read from APPROVED_VERSION_NEWS_MODEL_KEY_FILE (default
# /var/run/model-gateway-key/key, where a role pod mounts the gateway's key) by the driver itself,
# which gives it to each pane's environment alone. This script exports no key, and it unsets the
# caller's ANTHROPIC_API_KEY before either part, so no build, broker or service it starts holds
# one. Once part two ran, the key-leak check counts the key's first 8 characters in the evidence
# directory, without printing them, and fails the run on any.
#
# Three variables narrow part two to the runs one measurement needs, and the script passes them to
# its driver unchanged: APPROVED_VERSION_NEWS_STEP names one step by the word the last line names
# it by (a4, off or answers), APPROVED_VERSION_NEWS_ARM one arm (head or base), and
# APPROVED_VERSION_NEWS_RUNS how many runs each arm makes of each step, a whole number of 1 or more.
# Each left unset keeps every step, each step's own arms (the head arm alone for a4 and answers,
# both for off), or three runs; an arm the pick names is the one arm of every step it keeps, a
# retired base arm among them. The driver's plan says which runs a pick makes: the plan check
# refuses with exit 2, in the driver's own words, a value the driver refuses because it names none
# of these, and the arguments check refuses with exit 2 any of the three beside --part one,
# --control or --base-commit, which run no part two. The script then counts each arm against the
# pick's count of runs, prints no line for a step the pick leaves out, and prints a step whose base
# arm the pick leaves out as no control. A run with any of the three set is a measurement, a
# partial run, and never c-live's pass: its last line reads MEASURED in place of PASS, and names
# the pick, each step and arm of a full run's plan that the pick leaves out, and the count of runs
# of each arm where it is not a full run's. A head run that the pick makes and that does not reach
# its assertion and pass still makes the last line FAIL. A measurement that reaches its end exits
# 0, as a control that bites does: what it is run for is the counts its last line prints, a base
# arm that failed at its assertion among them, which no exit status says. So the measurement that
# retired a base arm is repeated by one command, a4's here and answers' with
# APPROVED_VERSION_NEWS_STEP=answers:
#   APPROVED_VERSION_NEWS_STEP=a4 APPROVED_VERSION_NEWS_ARM=base APPROVED_VERSION_NEWS_RUNS=10 \
#     bash scripts/e2e/approved-version-news.sh
# with the variables of the usage below beside them.
#
# Run it from a checkout on a Linux host with go, git, jj 0.38 or later (the daemon's boot refuses
# to start without them), tar, awk, bun and python3, as
#   LEGION_OMP_PATH=/opt/omp/bin/omp ANTHROPIC_BASE_URL=<model gateway> \
#     LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     [APPROVED_VERSION_NEWS_STEP=a4|off|answers] [APPROVED_VERSION_NEWS_ARM=head|base] \
#     [APPROVED_VERSION_NEWS_RUNS=<n>] \
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
# daemon takes at boot. So part two and the control need the checkout's history down to the base,
# which a shallow clone lacks: the arguments check refuses a base it cannot find with exit 2.
#
# --control runs part one's negative control against the base, and --base-commit <rev> against
# <rev>: `git archive <rev> | tar -x` into a scratch tree under TMPDIR, this checkout's part-one
# driver copied into it, and part one built and run there, daemon and Dispatch both of that
# commit; nothing of this checkout is written. Either runs part one alone, since the control is
# part one's. The run must fail at a2-the-reviewer-is-told-the-changed-sections, past that step's
# setup checks, on the one-approval check after them, in that check's own words (control_failure
# below): the script reports the control as biting then. It reports no control for a pass, a
# failure in an earlier check or step, a build failure, a killed driver, a failure line that names
# no step, and a failure at that step in other words, such as the line the driver prints when its
# harness fails because the base daemon died.
#
# Exit 0 is a pass, a control that bites, or a measurement that reached its end; exit 2 is an
# input of the caller's the run refuses, here, in the plan check or in the lib's prerequisites;
# exit 1 is every other failure. The run around each driver is lib/live-daemon-proof.sh's, made in
# a subshell, so this script reads its outcome and the driver's output and prints the last line.
# That line reads PASS, the control's verdict or MEASURED only for a run that reached one of the
# script's two ends, each of which calls run_end (lib/run-verdict.sh): a signal that ends the
# script leaves bash's EXIT trap a status of 0, and its line reads FAIL at the check it stopped
# in, with exit 1, or the signal's status.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
driver_file=packages/daemon/internal/daemon/approved_version_news_live_test.go
# Part one's driver is three files: the test and its harness, its steps, and its seed and setup
# checks, which the control copies into the base tree together, since the test runs the steps and
# the steps call the seed.
steps_file=packages/daemon/internal/daemon/approved_version_news_part_one_steps_live_test.go
seed_file=packages/daemon/internal/daemon/approved_version_news_seed_live_test.go
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
  r6-only-the-root-is-told-of-its-checks:4
  off-the-daemon-sends-nothing:5
)
# The step the negative control must fail at, and how many of its checks come before its first
# one-approval check: the two trees' setup approvals, the new version that closes the gate, the
# reviewer's open turn and the approval that opens the gate again, each of which holds on a daemon
# from before the change too. A control that fails earlier failed for another reason.
control_step=a2-the-reviewer-is-told-the-changed-sections
control_setup_checks=5
# The words that check fails with on a daemon from before the change, as an extended regular
# expression: the driver's wait for a task queued for the reviewer behind its open turn runs out
# (awaitQueued in its file). The control bites on these words alone. A failure at that check in
# other words is something else stopping the run, such as the driver's harness when the base
# daemon dies, and says nothing about the change.
control_failure='waited [^ ]+ for a task queued for the reviewer of [^ ]+ behind its open turn; last seen: '

# Part two's steps, the arms each runs in and how many runs each arm makes are the driver's step
# table's alone (sessionsSteps and pickSessions in
# packages/daemon/internal/daemon/approved_version_news_transcript_test.go), and this script keeps
# no copy of them: the plan check builds the driver and reads its plan (driver_plan), once with no
# pick, a full run's plan (full_plan), and, for a run that picks, once more with the run's pick
# (part_two_plan, which is full_plan for a run that picks nothing). The driver's own deadline is
# the lib's for part two, part_two_minutes for each run this run's plan makes, the eight minutes
# the driver bounds a run at and one for its daemon to boot and stop, with part_two_setup_minutes
# more for what the driver builds and starts before its first run: eighteen runs of real sessions
# took 44 minutes in a measured run.
part_two_minutes=9
part_two_setup_minutes=20
# The measurement that retired a step's base arm, as `<word>:<why>`, which the last line prints in
# the step's place (head_only_why): the one fact of part two's steps the driver does not know. The
# driver's plan of a full run makes each such step in the head arm alone, and the plan check fails
# the run, naming the step, where that plan makes a step in the head arm alone that has no entry
# here, or makes the step of an entry here in both arms or not at all (check_plan). Each was
# measured by APPROVED_VERSION_NEWS_STEP=<word> APPROVED_VERSION_NEWS_ARM=base
# APPROVED_VERSION_NEWS_RUNS=10, then ARM=head RUNS=3, alone; that command repeats it, and a base
# arm that passed 3 or more of the 10 cannot be told from the head arm at that base.
#   - a4 drives R4 through the child's own architect, which main's #4203 gives every started
#     child, and the base commit's own prompts tell that architect to tell its workers of a
#     version once its parent approves it. At 26392323 against its merge base, main 578c6abb, the
#     base arm passed 10 of 10 runs (dispatch://INFRA-175/artifact/a4-base10-2639232-txt) and the
#     head arm 3 of 3 (dispatch://INFRA-175/artifact/a4-head3-2639232-txt).
#   - answers tests the rule that a holder whose work a new version makes stale moves back with a
#     reason naming the version and the change, and one whose work it leaves current writes a
#     comment naming the version and why. The daemon's own news task states that rule
#     (holderRule, packages/daemon/internal/workflow/design_news.go), and both arms run the head's
#     daemon, so the base's role prompts need not carry it. At e5081eb2 against its merge base,
#     main 578c6abb, the base arm passed 10 of 10 runs
#     (dispatch://INFRA-175/artifact/answers-base10-e5081eb-txt) and the head arm 3 of 3
#     (dispatch://INFRA-175/artifact/answers-head3-e5081eb-txt).
# off is the one step left whose base arm the full run makes: its base arm passed 0 of 3 runs.
head_only=(
  "a4:the base arm passed 10 of 10 at 578c6abb, main meets R4 since #4203"
  "answers:the base arm passed 10 of 10 at 578c6abb, the daemon's news task states the rule in both arms"
)

# shellcheck source-path=SCRIPTDIR source=lib/live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"

# The run's own state, which finish reads from the EXIT trap.
version_news_evidence=${APPROVED_VERSION_NEWS_EVIDENCE_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-approved-version-news-evidence.XXXXXXXX")}
base_tree=''
prompts_tree=''
plan_tree=''
full_plan=''
part_two_plan=''
counted=0
expected=0
verdict=''
part_two_said=''
version_news_started=$SECONDS
# Each of the script's two ends, the exit of a run of part one alone and the last statement, calls
# run_end (lib/run-verdict.sh, which lib/live-daemon-proof.sh sources), and nothing else marks the
# run's end. The runs of part one and part two are made in subshells, whose marks are their own.

# finish removes the scratch trees the run made, by their exact paths, and prints the run's last
# line through run_verdict: what part one counted beside what it expects, what part two counted in
# each arm, or that it did not run, and, for the control, whether it bit, or, for a run that picks
# part two's runs, MEASURED in place of PASS (pick_said). A verdict or PASS needs status 0 and a
# run that reached one of its ends; a run that ended before that with status 0, as a signal leaves
# the EXIT trap, reads FAIL at the check it stopped in, and says so.
finish() {
  local status=$? said passed
  for tree in "$base_tree" "$prompts_tree" "$plan_tree"; do
    [ -z "$tree" ] || rm -rf "$tree"
  done
  said="assertions: $counted/$expected"
  if [ "$parts" = one ]; then
    said="$said; part one alone, part two did not run"
  elif [ -n "$part_two_said" ]; then
    said="$said; part two: $part_two_said"
  fi
  passed="approved version news e2e: PASS in $((SECONDS - version_news_started))s ($said)"
  [ -z "$verdict" ] ||
    passed="approved version news e2e: $verdict in $((SECONDS - version_news_started))s ($said, evidence: $version_news_evidence)"
  run_verdict "$status" "$passed" \
    "approved version news e2e: FAIL at $check after $((SECONDS - version_news_started))s ($said, evidence: $version_news_evidence)"
}

# control_verdict PROOF STATUS is part one's negative control, read from the base run's driver
# output PROOF and that run's status STATUS. It sets counted to the checks of $control_step that
# held, and control_said to what the control saw. It answers 0 when the control bites, which is
# one ending alone: the run failed, its first FAIL line is $control_step's, the step's
# $control_setup_checks setup checks held before it, and the line holds the one-approval check's
# own words (control_failure). Every other ending is no control, and control_said says which: a
# pass, no driver output, no FAIL line (a killed or timed-out driver), a failure in another step or
# in the step's setup, or a failure at that check in other words.
control_verdict() {
  local proof=$1 status=$2 failure own="^FAIL $control_step: $control_failure"
  counted=$(grep -c "^   assert $control_step: " "$proof" 2>/dev/null || true)
  counted=${counted:-0}
  failure=$(grep -m1 '^FAIL ' "$proof" 2>/dev/null || true)
  if [ "$status" -eq 0 ]; then
    control_said="no control: part one passed against the base"
  elif [ ! -s "$proof" ]; then
    control_said="no control: the base run failed (status $status) before its driver ran"
  elif [ -z "$failure" ]; then
    control_said="no control: the base run's driver ended (status $status) with no FAIL line naming a step, a killed or timed-out run"
  elif [[ "$failure" != "FAIL $control_step: "* ]]; then
    control_said="no control: the base run failed somewhere other than $control_step: $failure"
  elif [ "$counted" -ne "$control_setup_checks" ]; then
    control_said="no control: the base run failed at $control_step after $counted of its checks, not at its first one-approval check after the $control_setup_checks setup checks: $failure"
  elif ! [[ "$failure" =~ $own ]]; then
    control_said="no control: the base run stopped at $control_step's first one-approval check, but not on that check's own failure (a wait for a task queued for the reviewer behind its open turn that ran out), so its harness or another fault stopped it: $failure"
  else
    control_said=$failure
    return 0
  fi
  return 1
}

# check_jj fails the run unless the jj the daemon runs is 0.38 or later: LEGION_JJ_PATH where it is
# set, which the daemon reads first (packages/daemon/internal/daemon/tools.go, toolEnv), and the jj
# on PATH otherwise. Both parts boot the whole daemon over the tmux runtime with a project, and its
# boot refuses a jj older than that (tools.go, minimumJJ), deep inside a driver this run has built
# by then. The release is read as the daemon reads it: `jj <major>.<minor>.<patch>` opening the
# first line of `jj --version`, a build's suffix after it ignored.
check_jj() {
  local jj=${LEGION_JJ_PATH:-jj} said
  said=$("$jj" --version 2>/dev/null) || fail "$jj --version failed, so this run cannot tell its jj is 0.38 or later"
  [[ "$said" =~ ^jj\ ([0-9]+)\.([0-9]+)\.[0-9]+ ]] ||
    fail "$jj --version printed '${said%%$'\n'*}', which names no release, so this run cannot tell its jj is 0.38 or later"
  ((BASH_REMATCH[1] > 0 || BASH_REMATCH[2] >= 38)) ||
    fail "$jj is jj ${BASH_REMATCH[1]}.${BASH_REMATCH[2]}, below 0.38, the oldest jj the daemon's boot starts with"
}

# head_only_why WORD prints the why of WORD's entry of head_only, and answers 1 for a step that
# has none, whose base arm the driver's plan of a full run makes (check_plan).
head_only_why() {
  local entry
  for entry in "${head_only[@]}"; do
    if [ "${entry%%:*}" = "$1" ]; then
      echo "${entry#*:}"
      return 0
    fi
  done
  return 1
}

# A plan is part two's driver's lines `plan step <word> <name> <arm>…`, one for each step it makes,
# in the order it makes them, and `plan runs <n>`, how many runs each arm makes of each step
# (sessionsPick.plan in the driver's step table). plan_words PLAN prints the word of each step of
# the plan PLAN, in its order, one a line; plan_step PLAN WORD prints the name of WORD's step and
# each arm PLAN makes it in, `<name> <arm>…`, and nothing for a step PLAN does not make; plan_runs
# PLAN prints its count of runs.
plan_words() {
  local tag kind word rest
  while read -r tag kind word rest; do
    [ "$tag $kind" != "plan step" ] || echo "$word"
  done <<<"$1"
}
plan_step() {
  local tag kind word rest
  while read -r tag kind word rest; do
    [ "$tag $kind $word" != "plan step $2" ] || echo "$rest"
  done <<<"$1"
}
plan_runs() {
  local tag kind runs rest
  while read -r tag kind runs rest; do
    [ "$tag $kind" != "plan runs" ] || echo "$runs"
  done <<<"$1"
}

# driver_plan DRIVER STEP ARM RUNS prints the plan part two's driver DRIVER makes of the pick that
# APPROVED_VERSION_NEWS_STEP, APPROVED_VERSION_NEWS_ARM and APPROVED_VERSION_NEWS_RUNS name as STEP,
# ARM and RUNS, each empty for none: the `plan …` lines its one test of part two prints under
# -part-two-plan, which makes no run. The test runs with no LEGION_TEST_PG_DSN, so the binary makes
# no database. A pick the driver refuses (its line `plan refused: <why>`) is the caller's input,
# refused with exit 2 in the driver's words. A driver that fails otherwise, or prints a plan of no
# step, a step of no arm or of an arm other than head and base, or no count of runs, fails the run.
# Its caller reads the plan through a command substitution, whose failure ends the run under
# set -e with the status of the refusal or the failure here.
driver_plan() {
  local out status=0 refused plan tag kind word name arms arm steps=0 runs=''
  out=$(env -u LEGION_TEST_PG_DSN APPROVED_VERSION_NEWS_STEP="$2" APPROVED_VERSION_NEWS_ARM="$3" \
    APPROVED_VERSION_NEWS_RUNS="$4" "$1" -test.count=1 -test.run "^$sessions_test\$" -part-two-plan 2>&1) ||
    status=$?
  refused=$(grep -m1 '^plan refused: ' <<<"$out" || true)
  [ -z "$refused" ] || refuse "part two's driver refuses the pick: ${refused#plan refused: }"
  [ "$status" -eq 0 ] ||
    fail "part two's driver ended with status $status and printed no plan: $(tail -n 5 <<<"$out" | tr '\n' ' ')"
  plan=$(grep '^plan ' <<<"$out" || true)
  while read -r tag kind word name arms; do
    [ -n "$tag" ] || continue
    case "$tag $kind" in
      'plan step')
        steps=$((steps + 1))
        [ -n "$arms" ] || fail "part two's driver planned $word in no arm: $tag $kind $word $name"
        for arm in $arms; do
          [ "$arm" = head ] || [ "$arm" = base ] ||
            fail "part two's driver planned $word in the arm '$arm', which is neither head nor base"
        done
        ;;
      'plan runs')
        [[ "$word" =~ ^[1-9][0-9]*$ ]] && [ -z "$name$arms" ] ||
          fail "part two's driver planned '$word $name $arms' runs, which is no count of runs"
        runs=$word
        ;;
      *) fail "part two's driver printed a plan line this script does not read: $tag $kind $word $name $arms" ;;
    esac
  done <<<"$plan"
  [ "$steps" -gt 0 ] && [ -n "$runs" ] ||
    fail "part two's driver printed no plan of steps and runs: $(tail -n 5 <<<"$out" | tr '\n' ' ')"
  echo "$plan"
}

# check_plan fails the run, naming the step, where the driver's plan of a full run (full_plan) and
# head_only disagree: a step the plan makes in the head arm alone that head_only holds no
# measurement for, whose control would be gone with nothing to say why; a step the plan makes in
# both arms that head_only holds a measurement for, whose control the last line would print as
# retired where it can bite; and an entry of head_only for a step the plan does not make.
check_plan() {
  local word name arms entry
  for word in $(plan_words "$full_plan"); do
    read -r name arms <<<"$(plan_step "$full_plan" "$word")"
    if [[ " $arms " == *" base "* ]]; then
      ! head_only_why "$word" >/dev/null ||
        fail "the driver's plan of a full run makes $word in both arms ($arms), and head_only holds a measurement that retired its base arm"
    else
      head_only_why "$word" >/dev/null ||
        fail "the driver's plan of a full run makes $word in the head arm alone ($arms), and head_only holds no measurement that retired its base arm"
    fi
  done
  for entry in "${head_only[@]}"; do
    [ -n "$(plan_step "$full_plan" "${entry%%:*}")" ] ||
      fail "head_only holds a measurement that retired the base arm of ${entry%%:*}, which the driver's plan of a full run does not make"
  done
}

# part_two_deadline prints the driver's deadline for part two, a Go duration: part_two_minutes for
# each run this run's plan (part_two_plan) makes, its count of runs in each arm of each step, and
# part_two_setup_minutes more.
part_two_deadline() {
  local word name arms list armed=0
  for word in $(plan_words "$part_two_plan"); do
    read -r name arms <<<"$(plan_step "$part_two_plan" "$word")"
    read -ra list <<<"$arms"
    armed=$((armed + ${#list[@]}))
  done
  echo "$((armed * $(plan_runs "$part_two_plan") * part_two_minutes + part_two_setup_minutes))m"
}

# pick_said prints what a run that picks part two's runs made of a full run, which opens its part
# of the last line: that it is a partial run; the pick, by each variable it set; each step and arm
# the driver's plan of a full run (full_plan) makes and this run's plan (part_two_plan) does not;
# and this run's count of runs of each arm, where it is not a full run's. The plan check prints it
# once, into picked.
pick_said() {
  local said="a partial run picked by" word name arms arm made left='' runs full
  [ -z "$pick_step" ] || said="$said APPROVED_VERSION_NEWS_STEP=$pick_step"
  [ -z "$pick_arm" ] || said="$said APPROVED_VERSION_NEWS_ARM=$pick_arm"
  [ -z "$pick_runs" ] || said="$said APPROVED_VERSION_NEWS_RUNS=$pick_runs"
  for word in $(plan_words "$full_plan"); do
    read -r name arms <<<"$(plan_step "$full_plan" "$word")"
    read -r name made <<<"$(plan_step "$part_two_plan" "$word")"
    for arm in $arms; do
      [[ " $made " == *" $arm "* ]] || left="${left:+$left, }$word $arm"
    done
  done
  [ -z "$left" ] || said="$said, leaving out $left"
  runs=$(plan_runs "$part_two_plan") full=$(plan_runs "$full_plan")
  [ "$runs" = "$full" ] || said="$said, with $runs run$([ "$runs" -eq 1 ] || echo s) of each arm in place of $full"
  echo "$said"
}

# part_two_step PROOF WORD reads the `run <step> <arm> <n>: <outcome> …` lines of WORD's step in
# part two's driver output PROOF, for each arm this run's plan (part_two_plan) makes the step in,
# each of which the driver ran as many times as the plan says. A PASS or FAIL run reached the
# step's assertion, and it counts for its arm. A SETUP run stopped before the assertion, and a
# HARNESS run is a fault of the harness before or after it. Neither counts, and each is noted by
# its line. A line of an arm the plan does not make the step in is not read. It sets step_said,
# the step's part of the last line: WORD, each arm's passed runs of those that reached the
# assertion, the runs not counted, and the control's verdict. The control bites only when a base
# run failed at its assertion. A step that runs in the head arm alone has none: a step of
# head_only in a run that picks no arm says why (head_only_why), and a step whose base arm the
# pick left out says so. It also sets head_missed: each head run that did not reach its assertion
# and pass, by its line, each after `; `.
part_two_step() {
  local proof=$1 word=$2 step arms runs arm lines ran passed failed line count counts='' base_ran=0 base_passed=0 base_failed=0 why
  head_missed=''
  read -r step arms <<<"$(plan_step "$part_two_plan" "$word")"
  runs=$(plan_runs "$part_two_plan")
  for arm in $arms; do
    lines=$(grep -E "^run $step $arm [0-9]+: " "$proof" 2>/dev/null || true)
    ran=$(grep -c . <<<"$lines" || true)
    passed=$(grep -cE "^run $step $arm [0-9]+: PASS " <<<"$lines" || true)
    failed=$(grep -cE "^run $step $arm [0-9]+: FAIL: " <<<"$lines" || true)
    line="$step $arm: $passed of $((passed + failed)) runs that reached the assertion passed"
    count="$arm $passed/$((passed + failed))"
    if [ "$ran" -ne $((passed + failed)) ]; then
      line="$line; $((ran - passed - failed)) not counted (SETUP or HARNESS)"
      count="$count with $((ran - passed - failed)) not counted"
    fi
    [ "$ran" -eq "$runs" ] || line="$line ($ran of $runs ran)"
    note "$line"
    counts="${counts:+$counts, }$count"
    while IFS= read -r line; do
      [ -z "$line" ] || note "not counted: $line"
    done < <(grep -vE "^run $step $arm [0-9]+: (PASS |FAIL: )" <<<"$lines" || true)
    if [ "$arm" = head ]; then
      [ "$ran" -eq "$runs" ] || head_missed="$head_missed; $word: $ran of $runs head runs ran"
      while IFS= read -r line; do
        [ -z "$line" ] || head_missed="$head_missed; $line"
      done < <(grep -vE "^run $step head [0-9]+: PASS " <<<"$lines" || true)
    else
      base_ran=$ran base_passed=$passed base_failed=$failed
    fi
  done
  step_said="$word $counts"
  if [[ " $arms " != *" base "* ]]; then
    if [ -z "$pick_arm" ] && why=$(head_only_why "$word"); then
      step_said="$step_said (head only: $why)"
    else
      step_said="$step_said (no control: the pick leaves the base arm out)"
    fi
  elif [ "$base_ran" -eq 0 ]; then
    step_said="$step_said (base did not run)"
  elif [ "$base_failed" -gt 0 ]; then
    step_said="$step_said (control bites)"
  elif [ "$base_passed" -eq 0 ]; then
    step_said="$step_said (no control: no base run reached its assertion)"
  else
    step_said="$step_said (no control: the base arm passed every run that reached its assertion)"
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
# The pick of part two's runs, which the driver reads from the same three variables: the plan check
# reads what the pick makes from the driver's plan of it (driver_plan), and the driver refuses a
# value that names no step, no arm or no count. Only a run of part two has runs to pick. pick is
# the three together, empty for a run that picks none.
pick_step=${APPROVED_VERSION_NEWS_STEP:-} pick_arm=${APPROVED_VERSION_NEWS_ARM:-} pick_runs=${APPROVED_VERSION_NEWS_RUNS:-}
pick=$pick_step$pick_arm$pick_runs
[ -z "$pick" ] || [ "$parts" = both ] ||
  refuse "APPROVED_VERSION_NEWS_STEP, APPROVED_VERSION_NEWS_ARM and APPROVED_VERSION_NEWS_RUNS pick part two's runs, and --part one, --control and --base-commit run part one alone"
for tool in go git tar awk python3; do
  command -v "$tool" >/dev/null || fail "$tool is not on PATH"
done
check_jj
[ -n "${LEGION_TEST_PG_DSN:-}" ] ||
  refuse "LEGION_TEST_PG_DSN is required: it names the PostgreSQL the daemon migrates and runs on (postgres://<user>@<host>:<port>/<database>?sslmode=disable). This script starts no database."
# require_loopback_dsn in lib/loopback-dsn.sh says why a caller of the loopback guard clears
# these three. The lib's prerequisites reads the DSN again, inside the run it makes.
unset PGHOSTADDR PGSERVICE PGSERVICEFILE
# A caller's own model key reaches nothing the run starts, in either part: no build, no Dispatch
# and no daemon holds it, and part two's driver reads the key file and gives the key to each pane
# alone.
unset ANTHROPIC_API_KEY
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
  note "part two against the base $base_sha's prompts, Oh My Pi $LEGION_OMP_PATH, model key file $key_file (read by the driver, never printed)"
fi
note "parts: $parts; evidence: $version_news_evidence"
pass

# Part two's plan is its driver's: the plan check builds the driver from this checkout
# (build_daemon_driver), the one build both parts run (plan_driver), and reads the driver's plan
# of a full run and, for a run that picks, of its pick (driver_plan) before part one runs, so that
# a pick the driver refuses costs no part one. It holds head_only to the plan of a full run
# (check_plan), and counts the driver's deadline from this run's plan. A run of part one alone has
# no plan check: the lib builds part one's driver from the build tree (plan_driver empty).
plan_driver=''
picked=''
if [ "$parts" = both ]; then
  begin plan
  plan_tree=$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-approved-version-news-plan.XXXXXXXX")
  plan_driver=$plan_tree/approved-version-news.test
  build_daemon_driver "$plan_driver" ||
    fail "the driver did not build, so this run reads no plan of part two and runs neither part"
  note "built $(basename "$plan_driver") from $root/packages/daemon with -tags e2e; both parts run it"
  full_plan=$(driver_plan "$plan_driver" '' '' '')
  part_two_plan=$full_plan
  [ -z "$pick" ] || part_two_plan=$(driver_plan "$plan_driver" "$pick_step" "$pick_arm" "$pick_runs")
  note "the driver's plan of a full run:"
  while IFS= read -r line; do note "  $line"; done <<<"$full_plan"
  if [ -n "$pick" ]; then
    picked=$(pick_said)
    note "the driver's plan of this run, $picked:"
    while IFS= read -r line; do note "  $line"; done <<<"$part_two_plan"
  fi
  check_plan
  part_two_timeout=$(part_two_deadline)
  note "part two runs under a deadline of $part_two_timeout"
  pass
fi

if [ -n "$control" ]; then
  begin base-tree
  base_tree=$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-approved-version-news-base.XXXXXXXX")
  git -C "$root" archive "$base_sha" | tar -x -C "$base_tree" || fail "git archive $base_sha | tar -x into $base_tree failed"
  for file in "$driver_file" "$steps_file" "$seed_file"; do
    cp "$root/$file" "$base_tree/$file" || fail "could not copy $file into $base_tree"
  done
  build_tree=$base_tree
  note "$base_sha extracted into $base_tree, with this checkout's $driver_file, $steps_file and $seed_file"
  pass
fi

# Part one is lib/live-daemon-proof.sh's run, in a subshell of its own so that its failure is this
# script's to read rather than its exit: the lib, sourced from this checkout, runs the driver the
# plan check built (plan_driver) in a run of both parts, whose build tree is this checkout, and
# otherwise builds the driver from the build tree's packages/daemon; the driver builds Dispatch
# from the tree LEGION_E2E_REPO_ROOT names. Its own transcript and logs go in a fresh part-one.*
# directory of the evidence directory, so no proof.log an earlier run left there is read as this
# run's. The lib names its checks in the subshell alone, so this one is named part-one here for
# the last line of a run that ends in it.
part_one=$(mktemp -d "$version_news_evidence/part-one.XXXXXX")
check=part-one
set +e
(
  set -e
  root=$build_tree
  export LEGION_E2E_REPO_ROOT=$build_tree
  live_daemon_proof 'approved version news part one' approved-version-news "$driver_test" "$part_one" '' "$plan_driver"
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
  if ! control_verdict "$proof" "$part_one_status"; then
    fail "$control_said (the base $base_sha; $part_one keeps the run's transcript and the driver's output)"
  fi
  note "the base run failed at $control_step after its $counted setup checks held, on its first one-approval check:"
  note "$control_said"
  verdict="control bites at $control_step against $base_sha"
  pass
fi

if [ "$parts" = one ]; then
  begin part-two
  note "part two did not run: --part one, --control and --base-commit run part one alone"
  pass
  run_end
  exit 0
fi

# Part two is the lib's run too, over this checkout and of the driver the plan check built, with
# the base commit's prompt files extracted for its base arm into a scratch directory of their own.
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
  live_daemon_proof 'approved version news part two' approved-version-news-sessions "$sessions_test" "$part_two" "$part_two_timeout" "$plan_driver"
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

# A run that picks part two's runs is a measurement, a partial run, and never c-live's pass: its
# part of the last line opens with the pick and what it left out of a full run (picked, which the
# plan check made with pick_said), and, once every head run the pick makes reached its assertion
# and passed, its verdict is MEASURED.
begin part-two
head_short=''
for word in $(plan_words "$part_two_plan"); do
  part_two_step "$proof" "$word"
  part_two_said="${part_two_said:+$part_two_said; }$step_said"
  head_short="$head_short$head_missed"
done
[ -z "$pick" ] || part_two_said="$picked: $part_two_said"
calls=$(grep -m1 -E '^model calls: ' "$proof" 2>/dev/null || true)
[ -z "$calls" ] || part_two_said="$part_two_said; ${calls%% in all*}"
[ -z "$head_short" ] || fail "the head arm did not reach its assertion and pass in every run: ${head_short#; }"
[ "$part_two_status" -eq 0 ] || fail "part two failed (status $part_two_status); $proof keeps each run's evidence"
[ -z "$pick" ] || verdict=MEASURED
pass
run_end
