#!/usr/bin/env bash
# Check w2 of this change's verification: a session whose Oh My Pi ends in a turn is told, when it
# is resumed, that its previous turn was cut (R2). The whole daemon runs on the rig of this change's
# checks (packages/daemon/internal/daemon/RIG.md): the listener built from this checkout's own
# commit, a real nats-server with that commit's grants, a PostgreSQL of the run's own, and real
# `omp --mode rpc` panes under the real `legion worker-shim`, each with this checkout's
# @sjawhar/pi-envoy and @sjawhar/pi-legion, whose turns go to the model the run names. Unlike the
# other checks on the rig, this one sets LEGION_E2E_LISTENER_FROM_CHECKOUT=1 (lib/live-rig.sh,
# testenvoy.RigBinary): w2 tests this change's plugins and listener together, and the listener
# production pins predates the one that lets a controller register its notice lane, so step 4's
# controller would never receive its tick there. The run's checkout-listener check and its
# listener.txt name the commit the listener was built from. The driver is
# packages/daemon/internal/daemon/w2_cut_turn_live_test.go. It runs its four steps twice, on a rig
# each, with file sessions and with SQL sessions (every worker started with OMP_SESSION_STORAGE=sql
# and an OMP_SESSION_SQL_DSN_FILE of the rig's own, naming a database of the run's own on the
# server LEGION_TEST_PG_DSN names, and every resume read from the session's row), then compares the
# notes; each step prints `== <step>`, what it observed and read, with the times, and `ok <step>`,
# or `FAIL <step> at <assertion>`:
#
#   step1   a notice to a role starts a turn that runs `sleep`; the role's Oh My Pi is killed in
#           the call; the daemon resumes the session, its next turn runs, the rig's message starts
#           one more, and the transcript holds one note naming the notice, ahead of the entry that
#           opens the resumed first turn, saying to read what the turn did, and the notice's card
#           once
#   step2   a task's turn runs a short `sleep`; a notice published in that call joins the turn
#           as the call returns and has it run the long `sleep`, in which the process is killed:
#           the resumed transcript holds the daemon's own task note and the plugin's note naming
#           the notice
#   step3   the same cut as step 1, with the daemon's prompts to the claim held at the rig's relay
#           (a daemon that answers late): the rig's message starts the resumed first turn, the note
#           comes ahead of its card, and the daemon's prompt, let go, comes after it
#   step4   the daemon restarted as `controller: daemon`; a tick wake starts a turn of its
#           controller, which is killed in it: the note names the tick and comes ahead of the
#           start message's turn
#   step5   an Oh My Pi session that holds no Legion claim, killed mid-turn and resumed: no note
#   both-stores-give-the-same-notes   each step's note, with its event ids and the project named,
#           is the same under both stores
#
# Every observation is printed and appended, as it is made, to w2-results.jsonl in the evidence
# directory, beside the transcripts each step read (transcripts/) and each store's listener log
# (file-listener.log, sql-listener.log).
#
# Run it from a checkout on a Linux host as
#   LEGION_OMP_PATH=<omp> LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     LEGION_E2E_MODEL_BASE_URL=<anthropic-messages endpoint> LEGION_E2E_MODEL_KEY_COMMAND=<command> \
#     bash scripts/e2e/cut-turn-note.sh [--base-commit <rev>]
# On a development host the model is the shared gateway, https://model-gateway.<tailnet>.ts.net,
# and its key command one that prints the gateway key; in an issue pod it is the pod's
# ANTHROPIC_BASE_URL and a file holding its key (LEGION_E2E_MODEL_KEY_FILE).
# lib/live-rig.sh names every input and what it refuses. CUT_TURN_NOTE_EVIDENCE_DIR (default a fresh
# directory under TMPDIR, kept and printed) keeps the run's evidence. It needs no docker, no tmux
# and no kube context.
#
# --base-commit <rev> is the check's negative control: step 1 with file sessions at the base. A
# detached worktree of <rev> is made under TMPDIR (removed on any exit), this checkout's rig — the
# testenvoy package, the rig's driver files, its SQL sessions and lib/live-rig-grants.go — and this
# check's driver are laid over it, `bun install --frozen-lockfile` gives it node_modules of its own,
# and the same run is made there with that one step: the daemon, `legion`, both plugins and the
# listener (built from <rev> itself) are the base's.
# The driver names no Go symbol the base lacks, reading every plugin and daemon text as a
# literal, so it builds there. The control passes when step 1 fails at its cut-turn-note assertion,
# where the step says the resumed transcript holds no note that its previous turn was cut, after
# the kill and the resume it reached. Its last line is `cut turn note negative control: PASS|FAIL …`
# with the base run's evidence directory.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
base_commit=''
base_given=false
while [ $# -gt 0 ]; do
  case "$1" in
    --base-commit)
      [ $# -ge 2 ] || {
        echo "usage: $0 [--base-commit <rev>]" >&2
        exit 2
      }
      base_commit=$2
      base_given=true
      shift 2
      ;;
    *)
      echo "usage: $0 [--base-commit <rev>]" >&2
      exit 2
      ;;
  esac
done
if [ "$base_given" = true ] && [ -z "$base_commit" ]; then
  echo "--base-commit names no commit" >&2
  exit 2
fi
# shellcheck source-path=SCRIPTDIR source=lib/live-rig.sh
. "$root/scripts/e2e/lib/live-rig.sh"

test_name=TestAResumedSessionIsToldItsTurnWasCutLive
# Two rigs run one after another, each with five steps on the model the run names and the
# controller's start procedure, which outlasts lib/live-rig.sh's 90 minutes on a loaded host.
export LIVE_RIG_TEST_TIMEOUT=${LIVE_RIG_TEST_TIMEOUT:-180m}
export LEGION_E2E_LISTENER_FROM_CHECKOUT=1
step1='step1-a-turn-a-notice-started-is-told-cut-once-before-the-next-turn'
# What the control lays over the base's tree: the rig and this check's driver.
rig_files=(
  packages/daemon/internal/testenvoy
  packages/daemon/internal/daemon/rig_live_test.go
  packages/daemon/internal/daemon/rig_nats_live_test.go
  packages/daemon/internal/daemon/rig_runtime_live_test.go
  packages/daemon/internal/daemon/rig_sql_sessions_live_test.go
  packages/daemon/internal/daemon/w2_cut_turn_live_test.go
  scripts/e2e/lib/live-rig-grants.go
)

# The negative control's own state, global because its EXIT trap reads it.
base_work=''
base_tree=''
base_evidence=''

control_cleanup() {
  local status=$?
  if [ -n "$base_tree" ]; then
    git -C "$root" worktree remove --force "$base_tree" >/dev/null 2>&1 || true
  fi
  [ -z "$base_work" ] || rm -rf "$base_work"
  if [ "$status" -eq 0 ]; then
    echo "cut turn note negative control: PASS in $((SECONDS - started))s (evidence: $base_evidence)"
  else
    echo "cut turn note negative control: FAIL at $check after $((SECONDS - started))s (evidence: $base_evidence)" >&2
  fi
}

negative_control() {
  local base_sha status file line detail
  started=$SECONDS
  base_evidence=${CUT_TURN_NOTE_EVIDENCE_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-cut-turn-note-base-evidence.XXXXXXXX")}
  mkdir -p "$base_evidence"
  trap control_cleanup EXIT
  transcript_to "$base_evidence/negative-control.log"

  begin base-tree
  base_sha=$(git -C "$root" rev-parse --verify "$base_commit^{commit}" 2>/dev/null) ||
    refuse "--base-commit '$base_commit' names no commit of this repository"
  base_work=$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-cut-turn-note-base.XXXXXXXX")
  base_tree=$base_work/tree
  git -C "$root" worktree add --detach "$base_tree" "$base_sha" >"$base_evidence/base-tree.log" 2>&1 ||
    fail "could not make a worktree of $base_sha; see $base_evidence/base-tree.log"
  for file in "${rig_files[@]}"; do
    rm -rf "${base_tree:?}/$file"
    mkdir -p "$(dirname "$base_tree/$file")"
    cp -R "$root/$file" "$base_tree/$file" || fail "could not lay $file over the tree of $base_sha"
  done
  (cd "$base_tree" && bun install --frozen-lockfile) >>"$base_evidence/base-tree.log" 2>&1 ||
    fail "bun install in the tree of $base_sha failed; see $base_evidence/base-tree.log"
  note "a worktree of $base_sha in $base_tree, with this checkout's rig and driver laid over it: ${rig_files[*]}"
  pass

  set +e
  (
    set -e
    root=$base_tree
    live_rig_proof "cut turn note at ${base_sha:0:12}" cut-turn-note-base "^$test_name\$/^file-sessions\$/^$step1\$" "$base_evidence" fail
  )
  status=$?
  set -e
  if [ "$status" -eq 2 ]; then
    check=base-run
    exit 2
  fi

  begin negative-control
  grep -q "^== $step1\$" "$base_evidence/proof.log" 2>/dev/null ||
    fail "the run over $base_sha reached no step 1: it failed before its proof; see $base_evidence/transcript.log"
  line=$(grep -m1 -E "^(ok|FAIL) $step1( |$)" "$base_evidence/proof.log") ||
    fail "step 1 over $base_sha came to no outcome; see $base_evidence/proof.log"
  case "$line" in
    "FAIL $step1 at cut-turn-note ("*) ;;
    "ok $step1 "*) fail "step 1 passed over $base_sha, so this check tells it from this checkout in nothing" ;;
    *) fail "step 1 failed over $base_sha elsewhere than at its cut-turn note: $line" ;;
  esac
  detail=$(grep -m1 -E 'w2_cut_turn_live_test\.go:[0-9]+: cut-turn-note: ' "$base_evidence/proof.log") ||
    fail "step 1's failure over $base_sha carries no cut-turn-note sentence; see $base_evidence/proof.log"
  case "$detail" in
    *"the resumed transcript holds no note that its previous turn was cut"*) ;;
    *) fail "step 1's cut-turn-note sentence over $base_sha does not say the resumed transcript holds no note: $detail" ;;
  esac
  note "$line"
  note "${detail#"${detail%%[![:space:]]*}"}"
  pass
}

if [ -z "$base_commit" ]; then
  live_rig_proof 'cut turn note' cut-turn-note "^$test_name\$" "${CUT_TURN_NOTE_EVIDENCE_DIR:-}"
else
  negative_control
fi
