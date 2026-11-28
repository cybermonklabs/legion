#!/usr/bin/env bash
# The live proof that a production check's look report gives the look's secret id, which only the
# look's own task text carries, and that the daemon takes the look a report is of from that id and
# never from the turn the report came in (dispatch://INFRA-3518). The whole daemon is booted by the
# function `legion start` calls, over a real PostgreSQL and a real nats-server, afresh for each part
# of late turn, of task ends with its look and of upgrade, and for each other step that drives a
# check, and driven only as production drives it: GitHub's pull request, its merge and Dispatch's
# moves on the ENVOY_NOTIFICATIONS stream, the daemon's own HTTP routes for each role's
# registration, readiness, grants and reports — each report the raw JSON body the plugin's
# handoff_complete sends — and for the operator's suspension and resume and the controller's fresh
# session, and its worker stream, which the driver speaks as `legion worker-shim` and Oh My Pi do, a
# late answer, the busy answer (session_busy) and a refusal included. Every result is read
# back from GET /legion/v1/state, from the record route read_record calls, from the operator's claim
# listing, from the store, from the daemon's log and from what the daemon sent — or did not send —
# to a role's own connection (packages/daemon/internal/daemon/production_check_look_id_live_test.go,
# which this script builds with `-tags e2e`). A look's bound and a look's wake are brought forward
# by SQL on the outbox, and a seed the daemon of an earlier release would have left is written by
# SQL while no daemon runs. Each step prints `== <name>`, what it observed and `ok <name>`, or
# `FAIL <name> at <assertion>`; each runs on its own, so one that fails does not keep the next from
# running, and the run fails when any step failed. A step of parts prints each part between
# `-- <part>: <what>` and `ok <part>` or `FAIL <part> at <assertion>`, where <part> is `part <n>`
# or, for the one part no check numbers, its name, and fails naming each part that failed.
#
#   late turn                      c-late-turn: a report the checking role makes in the turn of
#                                  the open look's task is recorded against that look, also when
#                                  the turn match fails, and the daemon then stops sending that task
#                                  and clears the claim's prompt counts. Part 1, the look-2 send
#                                  runs out of its deadline after the prompt reached the agent, its
#                                  later sends are answered busy and its late turn reports; part 2,
#                                  the daemon restarts between that prompt and its turn; part 3, an
#                                  implementer's check seeded as one opened before the rule that the
#                                  tester checks production; part 4, on the wall clock, an
#                                  acknowledged prompt whose turn starts while its next send is in
#                                  flight
#   testing turn behind the close  c-no-id: a tester's testing turn that starts while the first
#                                  look's task is in flight sends a pass with no look id, with a look
#                                  id it made up and with no check_opened_at; each is refused and
#                                  records nothing, and the look's own task reaches the connection
#                                  after that turn and its report is taken
#   ended look                     c-ended-look: a report of a look its bound ended is taken until
#                                  the next look starts, and one of a look the next look replaced is
#                                  refused CHECK_LOOK_NOT_OPEN, while the same report with the next
#                                  look's id is taken
#   task ends with its look        c-task-ends: the task of a look the turn match fails for ends
#                                  with its look — ended by its report, its bound, an operator's
#                                  hold the next look's start meets, a claim failed once its prompt
#                                  failures ran out, or the check of a later merge (parts 1 to 5) —
#                                  and the next look's task takes its place, also behind an
#                                  acknowledged prompt whose turn starts late (part 6); the check of
#                                  a root reopened inside its linger (part 7) and of a child that
#                                  re-entered (part 8) stays open and takes the report of the look
#                                  from the session of its turn, after that turn; it closes as not
#                                  run at the next merge of the new run (part 9) and at a move to
#                                  backlog (part 10, a child and then a root), queuing no issue
#                                  close or suspension of its own; and the ready after a fresh
#                                  session sends nothing of a look that ended (fresh session)
#   upgrade                        c-upgrade: each part writes into the store, with the daemon
#                                  stopped, what a daemon of the release before leaves there, its
#                                  tester registered on that release's contract, and boots the daemon
#                                  on it: the boot gives each open look with no look id of its own an
#                                  id and a new task, keeping its number and the check open — an open
#                                  look (part 1), the last look (part 2), a look whose tester the
#                                  operator holds and resumes after the boot (part 3), an id made for
#                                  the previous look, which a rollback leaves when the checked root
#                                  runs again while the way back runs, so the way back keeps its id,
#                                  and the release before starts the next look once the run ends
#                                  (part 4), a look beside a start row of it that release queued,
#                                  which runs after the boot's row and, in the reverse order, before
#                                  it, and finishes without acting (part 6), an id made for the check
#                                  a later merge replaced (part 7) and a task written with no
#                                  check_opened_at (part 8) — and a look that ended at the operator's
#                                  hold is left to its next look (part 5); no text that release left
#                                  reaches a connection, the look's new task does once the tester
#                                  registers on this change's contract, and a report with its look id
#                                  is taken, one with a stale id refused 409. Beside the eight parts
#                                  c-upgrade names, two more on a root that ran again while the way
#                                  back ran, which kept its look's id: once the root has left the
#                                  workflow and its tester holds this change's task of the look in a
#                                  delivery that holds no look id, the boot gives the look a new id
#                                  and a new task, and a report with the kept id is refused 409 and
#                                  records nothing (part 9); while the root still runs, the boot
#                                  keeps the id and sends no task of the look, and the tester's report
#                                  with that id is taken (part 10)
#   id hidden                      c-id-hidden, the last step: no look id the run saw is in any body
#                                  of GET /legion/v1/state, of the record route read_record calls or
#                                  of an answer to a report, each read before a look's task is sent,
#                                  after a busy answer to it, after its report and after the next look
#                                  starts, nor in the daemon's whole log of the run; the same search
#                                  over the task texts that reached the connections finds each id;
#                                  and at least one body read carries check.openedAt, and the log
#                                  holds the line `workflow: production check opened`, which every
#                                  run writes
#
# Run it from a checkout on a Linux host with go, git, jj 0.38 or later (the daemon's boot refuses
# to start without them), awk, tar and python3, as
#   LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/production-check-look-id.sh [--base-commit <rev>]
# LEGION_TEST_PG_DSN is required and names the PostgreSQL the daemon migrates and runs on — CI's
# service, a devbox container and a pod's own embedded PostgreSQL all satisfy it on a loopback
# address, and this script starts none. That server must be this machine's: prerequisites reads
# the DSN with lib/loopback-dsn.sh's require_loopback_dsn and refuses a host that is not
# 127.0.0.1, localhost or ::1 with exit 2, before it builds or starts anything; with
# --base-commit the same guard runs first, before the base tree is taken, and a refusal of the base
# run is this script's exit 2 as well.
# The broker is the run's own: a nats-server from LEGION_E2E_NATS_BIN, or the pinned
# one this script installs with `go install` into a GOBIN of its own, started with JetStream on a
# port of the run's own and stopped on any exit. PRODUCTION_CHECK_LOOK_ID_EVIDENCE_DIR (default a
# fresh directory under TMPDIR, or /tmp when it is unset, kept and printed) keeps the transcript,
# the nats-server's log, the daemon's own and the driver's output (proof.log); the run's work
# directory, the driver binary among it, is under TMPDIR too, since a pod's own disk is small and
# its TMPDIR names the issue's volume. It needs no kube context, no tmux, no bun and no agent, so it
# runs in a Legion pod, in CI and on a devbox alike. The run around the driver is
# lib/live-daemon-proof.sh's.
#
# --base-commit <rev> is the negative control, and an empty <rev> is refused with exit 2. The tree
# of <rev> is taken with `git archive` into a directory of the run's own under TMPDIR (removed on
# any exit), this checkout's driver file is laid over it, and the same run is made there with
# PRODUCTION_CHECK_LOOK_ID_BASE=1: the driver is built from that tree. The driver names no Go
# symbol the base lacks — it reads the store's columns of the base by SQL and the routes as raw
# JSON — so it builds there; under that variable it sends no lookId, which the base daemon's strict
# decoder refuses, reads no look id, and runs the parts each check's negative control names. The
# control passes when that run fails and each step fails at the assertion its check's negative
# control names, with the refusal that control names: late turn at part 1's report (409
# HANDOFF_ALREADY_RECORDED, the report taken for look 1, which the claim was left serving) and at
# part 3's report (409 CHECK_LOOK_NOT_SERVED, a claim that served its run takes no unconfirmed
# look), testing turn behind the close at its first call (409 CHECK_LOOK_NOT_SERVED, the session
# works no look), ended look at part 1's report (409 CHECK_LOOK_NOT_SERVED), and task ends with its
# look at the next look of parts 2, 3 and 4 (the claim keeps the ended look's task and the start
# row of the next look waits on `a delivery is already pending`), at part 8's report (409
# CHECK_NOT_OPEN, the re-entry closed the check), at part 9's close line (the log has no close of
# the check the next merge's check is written over) and at the move of each half of part 10 (the
# child's check closed at its re-entry, before the move; the root's stays open after it), and
# upgrade at the seeded text of parts 1, 2, 3, 5 and 8 and of each half of part 6 (the text the
# release before left reaches the tester's connection once its claim is ready, since the base's
# boot gives no look an id and no new task); those two steps run no other part at the base, and
# parts 4, 7, 9 and 10 of upgrade, which hold the boot to a look id the base's schema has no column
# for, are among the parts they skip. What part 4 of late turn did there, which passes at the base
# too, and what id hidden did, which has nothing to search at the base, are printed as they came.
# A proof.log an earlier run left in PRODUCTION_CHECK_LOOK_ID_EVIDENCE_DIR is removed before the
# base run, so only that run's steps are read. Its last line is `production check look id negative
# control: PASS|FAIL …` with the base run's evidence directory.
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
# shellcheck source-path=SCRIPTDIR source=lib/live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"

test_name=TestProductionCheckLookIDLive
driver_file=packages/daemon/internal/daemon/production_check_look_id_live_test.go

# The steps the base must fail, each as `<step>|<assertion>|<refusal>`: the assertion it must fail
# at (the driver's `fails`, which a step of parts names `part <n> <assertion>`), and the words of
# the refusal its check's negative control names, which the assertion's own line must hold.
base_failures=(
  'late turn|part 1 report|409 HANDOFF_ALREADY_RECORDED'
  'late turn|part 3 report|409 CHECK_LOOK_NOT_SERVED'
  'testing turn behind the close|no-id-refusal|409 CHECK_LOOK_NOT_SERVED'
  'ended look|part 1 report|409 CHECK_LOOK_NOT_SERVED'
  'task ends with its look|part 2 next-look|a delivery is already pending'
  'task ends with its look|part 3 next-look|a delivery is already pending'
  'task ends with its look|part 4 next-look|a delivery is already pending'
  'task ends with its look|part 8 report|409 CHECK_NOT_OPEN'
  'task ends with its look|part 9 close-line|has no line `workflow: production check closed`'
  'task ends with its look|part 10 child closed-at-move|before the move'
  'task ends with its look|part 10 root closed-at-move|open after the move'
  'upgrade|part 1 seeded-text|reached the connection of the tester'
  'upgrade|part 2 seeded-text|reached the connection of the tester'
  'upgrade|part 3 seeded-text|reached the connection of the tester'
  'upgrade|part 5 seeded-text|reached the connection of the tester'
  'upgrade|part 6 older-row-after seeded-text|reached the connection of the tester'
  'upgrade|part 6 older-row-first seeded-text|reached the connection of the tester'
  'upgrade|part 8 seeded-text|reached the connection of the tester'
)

# The negative control's own state, global because its EXIT trap reads it.
base_work=''
base_evidence=''

# control_cleanup prints the negative control's last line through lib/run-verdict.sh, which
# lib/live-daemon-proof.sh sources: PASS only for a control that reached run_end, negative_control's
# last statement, once every step failed where it must. A control a signal ended reads FAIL, never
# the control biting.
control_cleanup() {
  local status=$?
  [ -z "$base_work" ] || rm -rf "$base_work"
  run_verdict "$status" \
    "production check look id negative control: PASS in $((SECONDS - started))s (evidence: $base_evidence)" \
    "production check look id negative control: FAIL at $check after $((SECONDS - started))s (evidence: $base_evidence)"
}

# failed_line is the step's own outcome line in the base run's proof.log: `FAIL <step> at …`.
failed_line() {
  grep -m1 -E "^FAIL $1 at " "$base_evidence/proof.log" || true
}

# assertion_line is the driver's t.Fatalf line of assertion inside step's own output, between its
# `== <step>` and `FAIL <step>` lines; for an assertion `part <n> <name>` of a step of parts, it is
# the line of <name> inside that part's own output, between its `-- part <n>:` and `FAIL part <n>`
# lines. Two steps, and two parts, can fail one assertion.
assertion_line() {
  awk -v step="$1" -v assertion="$2" '
    BEGIN {
      part = ""; name = assertion
      if (match(assertion, /^part [0-9]+ /)) { part = substr(assertion, 1, RLENGTH - 1); name = substr(assertion, RLENGTH + 1) }
    }
    $0 == "== " step { inside = 1; in_part = 0; next }
    !inside { next }
    index($0, "FAIL " step " at ") == 1 { exit }
    part != "" && index($0, "-- " part ": ") == 1 { in_part = 1; next }
    part != "" && in_part && index($0, "FAIL " part " at ") == 1 { in_part = 0; next }
    $0 ~ /production_check_look_id_live_test\.go:[0-9]+: / {
      if ((part != "" && in_part && index($0, ": " name ": ")) || index($0, ": " assertion ": ")) { print; exit }
    }
  ' "$base_evidence/proof.log"
}

# negative_control makes the run over the base tree in a subshell of its own — live_daemon_proof
# ends the shell it runs in, and its EXIT trap and its end mark are that subshell's — and then reads
# the base run's proof.log for each step's own failure.
negative_control() {
  local base_sha status expected step assertion refusal line detail failed_at tool db_host
  started=$SECONDS
  base_evidence=${PRODUCTION_CHECK_LOOK_ID_EVIDENCE_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-production-check-look-id-base-evidence.XXXXXXXX")}
  mkdir -p "$base_evidence"
  trap control_cleanup EXIT
  transcript_to "$base_evidence/negative-control.log"

  begin base-tree
  # The base run's own prerequisites read the DSN too, but only after the archive below; a DSN it
  # would refuse is refused here first, with the same guard and the same exit 2.
  [ -n "${LEGION_TEST_PG_DSN:+set}" ] ||
    refuse "LEGION_TEST_PG_DSN is required: it names the PostgreSQL the daemon migrates and runs on (postgres://<user>@<host>:<port>/<database>?sslmode=disable). This script starts no database."
  for tool in git tar python3; do
    command -v "$tool" >/dev/null || fail "$tool is not on PATH"
  done
  # require_loopback_dsn in lib/loopback-dsn.sh says why a caller of the loopback guard clears
  # these three, and why the call comes after the tool check above. The call names the variable
  # and not its value (require_loopback_env), so a run under `bash -x` traces no DSN.
  unset PGHOSTADDR PGSERVICE PGSERVICEFILE
  require_loopback_env db_host LEGION_TEST_PG_DSN "this run creates and drops a database there" ||
    refuse "$db_host"
  base_sha=$(git -C "$root" rev-parse --verify "$base_commit^{commit}" 2>/dev/null) ||
    refuse "--base-commit '$base_commit' names no commit of this repository"
  base_work=$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-production-check-look-id-base.XXXXXXXX")
  git -C "$root" archive "$base_sha" | tar -x -C "$base_work" ||
    fail "could not take the tree of $base_sha"
  cp "$root/$driver_file" "$base_work/$driver_file" ||
    fail "could not lay $driver_file over the tree of $base_sha"
  note "the tree of $base_sha in $base_work, with this checkout's $driver_file laid over it"
  pass

  # The evidence directory may be one an earlier run used: its proof.log would answer for steps
  # this run never reached.
  rm -f "$base_evidence/proof.log"
  set +e
  (
    set -e
    root=$base_work
    export PRODUCTION_CHECK_LOOK_ID_BASE=1
    live_daemon_proof "production check look id at ${base_sha:0:12}" production-check-look-id-base "$test_name" "$base_evidence"
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
  grep -q '^== id hidden$' "$base_evidence/proof.log" 2>/dev/null ||
    fail "the run over $base_sha reached no last step: it failed before its proof; see $base_evidence/transcript.log"
  for expected in "${base_failures[@]}"; do
    IFS='|' read -r step assertion refusal <<<"$expected"
    line=$(failed_line "$step")
    [ -n "$line" ] || fail "$step passed over $base_sha; it was to fail at $assertion"
    failed_at=${line#"FAIL $step at "}
    failed_at=${failed_at% (*}
    case ", $failed_at, " in
      *", $assertion, "*) ;;
      *) fail "$step failed over $base_sha elsewhere than at $assertion: $line" ;;
    esac
    detail=$(assertion_line "$step" "$assertion")
    case "$detail" in
      *"$refusal"*) ;;
      *) fail "$step failed over $base_sha at $assertion, but not with $refusal: ${detail:-no line of that assertion}" ;;
    esac
    note "$line"
    note "  ${detail#"${detail%%[![:space:]]*}"}"
  done
  line=$(awk '$0 == "== late turn" { inside = 1; next } inside && /^(ok|FAIL) part 4 / { print; exit }' "$base_evidence/proof.log")
  note "as it came: late turn ${line:-ran no part 4}"
  line=$(grep -m1 -E '^(ok|FAIL) id hidden |^ *--- SKIP: TestProductionCheckLookIDLive/id_hidden ' "$base_evidence/proof.log" || true)
  line=${line#"${line%%[![:space:]]*}"}
  note "as it came: ${line:-no outcome for id hidden}"
  pass
  run_end
}

if [ -z "$base_commit" ]; then
  live_daemon_proof 'production check look id' production-check-look-id "$test_name" \
    "${PRODUCTION_CHECK_LOOK_ID_EVIDENCE_DIR:-}"
else
  negative_control
fi
