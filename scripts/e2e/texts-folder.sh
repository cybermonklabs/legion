#!/usr/bin/env bash
# The live proof that an experiment arm's folder of texts replaces Legion's own texts for the roles
# its experiment reaches, and that a folder that fails a check is run as no arm at all
# (dispatch://INFRA-5590, its checks e0e84423, A1, and e0b617c0, A2). The whole daemon is booted by
# the function `legion start` calls, over a real PostgreSQL, a real nats-server and the real
# Dispatch this checkout builds, and real `omp --mode rpc` panes run under it — the real `legion
# worker-shim`, this checkout's `legion`, and this checkout's @sjawhar/pi-envoy and
# @sjawhar/pi-legion in an Oh My Pi profile of the run's own — each answered by a model stand-in of
# the driver's own (packages/daemon/internal/daemon/texts_folder_live_test.go, which this script
# builds with `-tags e2e`, over texts_folder_arm_live_test.go and localruntime_live_test.go, the
# runtime that starts a pane with no tmux and prepares a launch of a folder through the tmux
# runtime's own helper). The boot runs the real pane-lane plugin gate over the run's profile, so it
# reads the agents and skills the profile ships as a production boot does.
#
# The project declares the experiment `texts` at mode on, reaching the architect and the reviewer
# and not the planner, with the arm `folder`, whose folder the driver cuts from the checkout's own
# texts, and the arm `control`, which names nothing. Each case admits a tree through the workflow,
# whose draw records the arm against the tree's root: its root is handed to Legion on Dispatch and
# the boot's listing admits it and launches its architect. The root's child is then recorded in the
# store, as role-prompt-rule.sh's driver records its child, and the child's own architect, the
# child's reviewer and the root's planner are the operator's spawns of claims in that drawn tree,
# each reading the arm the tree's root was drawn into: a release through the `legion` tool needs
# GitHub Apps this proof has none of, and a workflow start of reviewing needs an implementer's
# pushed round. TEXTS_FOLDER_CASES names the cases a run makes, joined by commas, each a subtest
# with a daemon, a database and a Dispatch project of its own (default `accepted`):
#
#   accepted          the reviewer's first request holds the markers of the folder's
#                     roles/reviewer.md and go/reviewer.md and not the sentence the folder takes out,
#                     offers the folder's goal-red-team once and no `<namespace>/legion-worker` or
#                     `<namespace>/dispatch-first` skill, and carries the folder's dispatch-first
#                     once; its two reads through skill:// give the folder's plan-tests.md and
#                     Legion's untouched tools.md; its model calls name the arm; both architects'
#                     requests hold their folder texts; the root's planner reads Legion's texts; and
#                     after a restart with on_variant at the control arm, a new tree reads Legion's
#   digest            the manifest's digest of roles/reviewer.md changed by one character
#   no-legion-text    roles/no-such-part.md listed in the manifest
#   unknown-agent     goal-red-team's body names `agent="no-such-agent"`
#   unknown-skill     go/reviewer.md names `skill://no-such-skill`
#   manifest-deleted  the manifest deleted once the root architect's launch accepted the folder
#   unreadable        the folder's mode set to 000 then
#   unreached-part    roles/planner.md added then, with the digest of Legion's text
#
# In each fallback case the reviewer reads Legion's texts only, with no marker, its model calls
# name no arm, the daemon's log names the launch, the experiment, the arm, the folder, the file and
# the cause, and the child's issue holds one notice of the reviewer's claim naming that cause. In
# `digest` the child's architect, a claim of its own that meets the same cause, holds a notice of
# its own, and once the driver ends the reviewer's process group and the daemon relaunches the claim
# over the same folder, the issue still holds one notice of the reviewer's claim. Each step prints
# `== <name>`, what it drove and read, and `ok <name>`; a check that does not hold prints
# `FAIL <name>: <why>` and ends its case.
#
# Run it from a checkout on a Linux host as a user other than root, with go, git, jj 0.38 or later
# (the daemon's boot refuses to start without them), awk, bash, tar, bun (after `bun install` at
# the checkout's root) and python3, as
#   LEGION_OMP_PATH=/opt/omp/bin/omp \
#     LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     [TEXTS_FOLDER_CASES=<case>[,<case>…]] bash scripts/e2e/texts-folder.sh [--control [--base-commit <rev>]]
# It refuses to run as root with exit 2: root reads a folder of mode 000, so the unreadable case
# would read its folder and prove nothing. LEGION_OMP_PATH is required and names the Oh My Pi every
# pane runs, an absolute executable: this proof launches real agents, and no pin is guessed for it.
# LEGION_TEST_PG_DSN is required and names a loopback PostgreSQL carrying pgvector whose role may
# create databases: the driver makes the daemon's databases and Dispatch's there and drops them on
# exit, and prerequisites refuses a DSN that is unset or whose host is not 127.0.0.1, localhost or
# ::1 with exit 2, before it builds or starts anything (lib/loopback-dsn.sh's require_loopback_dsn).
# The broker is the run's own: a nats-server from LEGION_E2E_NATS_BIN, or the pinned one the lib
# installs with `go install` into a GOBIN of its own. TEXTS_FOLDER_EVIDENCE_DIR (default a fresh
# directory under TMPDIR, or /tmp when it is unset, kept and printed) keeps the transcript, the
# nats-server's log, the daemon's own, Dispatch's, each pane's and the driver's output; the run's
# work directory, the driver binary among it, is under TMPDIR too. The driver's deadline is ten
# minutes for each case and ten more. It needs no kube context, no Claude Code, no GitHub and no
# tmux — the panes are child process groups of the driver — so it runs in a Legion pod, a devbox or
# a build Job alike. The run around the driver is lib/live-daemon-proof.sh's.
#
# --control is the negative control of check e0e84423, and runs the accepted case alone: a
# TEXTS_FOLDER_CASES naming any other case is refused with exit 2. The tree of `main`, or of <rev>
# with --base-commit (an empty <rev> is refused with exit 2), is taken with `git archive` into a
# directory of the run's own under TMPDIR, removed on any exit; the driver's files are laid over it
# (texts_folder_live_test.go, omp_child_live_test.go, the non-test files of
# internal/texts/omprequest, and texts_folder_arm_control_live_test.go, which gives the folder's
# roles/reviewer.md as the arm's instructions file in place of a folder); `bun install
# --frozen-lockfile` gives it node_modules of its own; and the driver is built there with `-tags 'e2e
# textscontrol'` and run. So the daemon, `legion`, Dispatch and both plugins are that tree's. The
# control bites only when that run failed at reviewer-request, past every setup step, on the check
# that the reviewer's first request holds the sentence the folder takes out beside the folder's
# marker, in that check's own words: a pass, a failure in an earlier step or at another check, a
# build failure and a killed driver are each no control, and say which. The DSN is read with the
# same loopback guard before the tree is taken, and a refusal of the base run (exit 2) is this
# script's exit 2. The control needs this repository's history down to the base.
#
# Exit 0 is a pass or a control that bites; exit 2 is an input of the caller's the run refuses
# (root, an unknown argument, an empty --base-commit, a case the control does not run, a base that
# names no commit, a DSN the lib refuses); exit 1 is every other failure. The last line of a run is
# lib/live-daemon-proof.sh's, `texts folder e2e: PASS in <n>s (evidence: <dir>)` or `texts folder
# e2e: FAIL at <check> after <n>s (evidence: <dir>)`, and of a control `texts folder negative
# control: BITES at reviewer-request against <sha> in <n>s (evidence: <dir>)` or `texts folder
# negative control: FAIL at <check> after <n>s (evidence: <dir>)`, through lib/run-verdict.sh, so a
# run a signal ends reads FAIL.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
control=false
base_commit=''
base_given=false
while [ $# -gt 0 ]; do
  case "$1" in
    --control)
      control=true
      shift
      ;;
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
      echo "FAIL setup: unknown argument '$1': this script takes --control and --base-commit <rev>" >&2
      exit 2
      ;;
  esac
done
if [ "$base_given" = true ] && [ "$control" = false ]; then
  echo "FAIL setup: --base-commit names the negative control's base, and goes with --control" >&2
  exit 2
fi
if [ "$base_given" = true ] && [ -z "$base_commit" ]; then
  echo "FAIL setup: --base-commit was given an empty commit; it takes the commit the negative control's tree is taken from" >&2
  exit 2
fi
if [ "$(id -u)" -eq 0 ]; then
  echo "FAIL setup: this script refuses to run as root: root reads a folder of mode 000, so the unreadable case would read its folder and prove nothing; run it as another user" >&2
  exit 2
fi
if [ "$control" = true ] && [ -n "${TEXTS_FOLDER_CASES:-}" ] && [ "$TEXTS_FOLDER_CASES" != accepted ]; then
  echo "FAIL setup: the negative control runs the accepted case alone, and TEXTS_FOLDER_CASES names '$TEXTS_FOLDER_CASES'" >&2
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
for tool in git jj tar; do
  command -v "$tool" >/dev/null || {
    echo "FAIL setup: $tool is not on PATH" >&2
    exit 1
  }
done
# shellcheck source-path=SCRIPTDIR source=lib/live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"

test_name=TestTextsFolderLive
# The driver's deadline: ten minutes for each case the run makes, and ten more.
cases=1
if [ "$control" = false ] && [ -n "${TEXTS_FOLDER_CASES:-}" ]; then
  cases=$(awk -F, '{print NF}' <<<"$TEXTS_FOLDER_CASES")
fi
texts_folder_timeout="$((10 + 10 * cases))m"

# The files the negative control lays over the base tree: the driver and the free function it
# reads a pane's process by, the control's arm in place of texts_folder_arm_live_test.go, and the
# request reader's package (its non-test files, added below).
control_files=(
  packages/daemon/internal/daemon/texts_folder_live_test.go
  packages/daemon/internal/daemon/omp_child_live_test.go
  packages/daemon/internal/daemon/texts_folder_arm_control_live_test.go
)
# The reviewer check's failure in the base tree, in its own words (folderProofLive.reviewerRequest),
# as an extended regular expression: the sentence the folder takes out beside the folder's marker.
control_failure="^FAIL reviewer-request: the reviewer's first request holds the sentence its folder's roles/reviewer\\.md takes out \\(.*\\) beside that file's marker \"TFMARK-roles-reviewer-[0-9A-F]+\""

# The negative control's own state, global because its EXIT trap reads it.
base_work=''
base_evidence=''
base_label=''

# control_cleanup prints the negative control's last line through lib/run-verdict.sh, which
# lib/live-daemon-proof.sh sources: BITES only for a control that reached run_end, negative_control's
# last statement, once the base run failed where it must. A control a signal ended reads FAIL.
control_cleanup() {
  local status=$?
  [ -z "$base_work" ] || rm -rf "$base_work"
  run_verdict "$status" \
    "texts folder negative control: BITES at reviewer-request against $base_label in $((SECONDS - started))s (evidence: $base_evidence)" \
    "texts folder negative control: FAIL at $check after $((SECONDS - started))s (evidence: $base_evidence)"
}

# negative_control makes the accepted case's run over the base tree in a subshell of its own —
# live_daemon_proof ends the shell it runs in, and its EXIT trap and its end mark are that
# subshell's — and then reads the base run's proof.log for the reviewer check's own failure.
negative_control() {
  local base_sha status first file db_host tool driver
  started=$SECONDS
  base_evidence=${TEXTS_FOLDER_EVIDENCE_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-texts-folder-base-evidence.XXXXXXXX")}
  mkdir -p "$base_evidence"
  trap control_cleanup EXIT
  transcript_to "$base_evidence/negative-control.log"

  begin base-tree
  # The base run's own prerequisites read the DSN too, but only after the archive, the install and
  # the build below; a DSN it would refuse is refused here first, with the same guard and exit 2.
  [ -n "${LEGION_TEST_PG_DSN:-}" ] ||
    refuse "LEGION_TEST_PG_DSN is required: it names the PostgreSQL the daemon migrates and runs on (postgres://<user>@<host>:<port>/<database>?sslmode=disable). This script starts no database."
  for tool in go bun python3; do
    command -v "$tool" >/dev/null || fail "$tool is not on PATH"
  done
  # require_loopback_dsn in lib/loopback-dsn.sh says why a caller of the loopback guard clears
  # these three, and why the call comes after the tool check above.
  unset PGHOSTADDR PGSERVICE PGSERVICEFILE
  require_loopback_dsn db_host LEGION_TEST_PG_DSN "$LEGION_TEST_PG_DSN" "this run creates and drops databases there" ||
    refuse "$db_host"
  base_sha=$(git -C "$root" rev-parse --verify "${base_commit:-main}^{commit}" 2>/dev/null) ||
    refuse "the base '${base_commit:-main}' names no commit of $root: fetch main, or name the base with --base-commit <rev>"
  base_label=$base_sha
  base_work=$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-texts-folder-base.XXXXXXXX")
  git -C "$root" archive "$base_sha" | tar -x -C "$base_work" ||
    fail "could not take the tree of $base_sha"
  for file in "$root"/packages/daemon/internal/texts/omprequest/*.go; do
    case "$file" in
      *_test.go) ;;
      *) control_files+=("${file#"$root"/}") ;;
    esac
  done
  for file in "${control_files[@]}"; do
    mkdir -p "$(dirname "$base_work/$file")"
    cp "$root/$file" "$base_work/$file" || fail "could not lay $file over the tree of $base_sha"
  done
  (cd "$base_work" && bun install --frozen-lockfile) >"$base_evidence/bun-install.log" 2>&1 ||
    fail "bun install in the tree of $base_sha failed; see $base_evidence/bun-install.log"
  driver=$base_work/texts-folder-control.test
  go -C "$base_work/packages/daemon" test -c -tags 'e2e textscontrol' -o "$driver" ./internal/daemon >"$base_evidence/driver-build.log" 2>&1 ||
    fail "the driver did not build over $base_sha with -tags 'e2e textscontrol'; see $base_evidence/driver-build.log"
  note "the tree of $base_sha in $base_work, with ${control_files[*]} laid over it, node_modules of its own, and the driver built there with -tags 'e2e textscontrol'"
  pass

  # The evidence directory may be one an earlier run used: its proof.log would answer for a run
  # this one never made.
  rm -f "$base_evidence/proof.log"
  set +e
  (
    set -e
    root=$base_work
    export TEXTS_FOLDER_CASES=accepted
    require_dispatch_cli
    live_daemon_proof "texts folder at ${base_sha:0:12}" texts-folder-base "$test_name" "$base_evidence" "$texts_folder_timeout" "$driver"
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
  [ "$status" -ne 0 ] || fail "the run over $base_sha passed, so this proof tells it from this checkout in nothing"
  first=$(grep -m1 -E '^FAIL [a-z0-9-]+: ' "$base_evidence/proof.log" 2>/dev/null || true)
  grep -q '^ok reviewer-started ' "$base_evidence/proof.log" 2>/dev/null ||
    fail "the run over $base_sha failed before its reviewer's requests were read, so no reviewer check ran: ${first:-no FAIL line; see $base_evidence/proof.log}"
  [ -n "$first" ] ||
    fail "the run over $base_sha ended (status $status) with no FAIL line naming a check, a killed or timed-out run; see $base_evidence/proof.log"
  [[ "$first" =~ $control_failure ]] ||
    fail "the run over $base_sha failed somewhere other than reviewer-request's check of the sentence the folder takes out: $first"
  note "$first"
  pass
  run_end
}

if [ "$control" = false ]; then
  require_dispatch_cli
  live_daemon_proof 'texts folder' texts-folder "$test_name" "${TEXTS_FOLDER_EVIDENCE_DIR:-}" "$texts_folder_timeout"
else
  negative_control
fi
