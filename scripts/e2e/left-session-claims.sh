#!/usr/bin/env bash
# The live proof that the daemon releases the Dispatch claim of a session it has left behind, that a
# move out of the workflow releases the claim of its tree's session, and that a release which leaves
# a claim on purpose says why in the issue's record (dispatch://INFRA-3144's check c-live-proof, its
# R1, R2 and R5). The driver (packages/daemon/internal/daemon/left_session_claims_live_test.go,
# which this script builds with `-tags e2e`) builds and starts the real Dispatch server and the
# real Envoy listener from this checkout's packages/envoy, the listener on the run's nats-server
# and Dispatch on a database of its own with its events on that nats-server and ENVOY_URL at the
# listener, and boots the whole daemon with dispatch_url at that Dispatch, reading its events (the
# rig of person_handover_scratch_dispatch_test.go). Each claim of a session is the real `dispatch`
# CLI (packages/envoy-client/bin/dispatch.ts under bun) run as that session, each move Dispatch's own
# status route as the person, and a session's process ends with its registration on the listener,
# by which Dispatch judges that session's claim, so Dispatch reads it gone. Each step prints
# `== <name>`, what it observed and `ok <name>`, or `FAIL <name> at <assertion>`; each runs on its
# own, so one that fails does not keep the next from running, and the run fails when any step
# failed:
#   - a-fresh-session-releases-the-left-session: a root in planning whose architect session S1,
#     registered on the listener, claimed it with `dispatch claim`; S1's registration ends; the
#     controller's fresh_session on the architect starts it again in a new session S2, which claims
#     nothing; Dispatch's events of the root then hold an issue.released whose actor is the daemon's
#     own session (legion-daemon:<PROJECT>) and whose previous claim is S1's (assertion released),
#     and `dispatch read` prints `Claimed by: nobody` (claimed-by-nobody);
#   - a-move-releases-its-trees-claim: a second root in planning whose architect session S3 claimed
#     it; S3's registration ends; the person moves the root to backlog and leaves it there; the same
#     issue.released, by the daemon's session of S3's claim (released), `Claimed by: nobody`
#     (claimed-by-nobody), GET /legion/v1/issues/<key>/record names no claimLeft (no-claim-left),
#     and Dispatch still shows the root in backlog (left-in-backlog);
#   - a-persons-claim-stays-with-its-reason: the person claims a third root in planning and moves it
#     to backlog; the record names claimLeft with holder user:smoke, reason person and the time the
#     release left the claim (claim-left); the claim stays, with no issue.released, and `dispatch
#     read` names the person (claim-stays); a caller stating the claim-left entry's own contract in
#     Legion-Plugin-Contract is served claimLeft (served-at-the-entry), and one stating the contract
#     just below it is served none (none-below-the-entry).
#
# Run it from a checkout on a Linux host with go, git, jj 0.38 or later (the daemon's boot refuses
# to start without them), bun (after `bun install` at the checkout's root), awk and python3, and tar
# for the negative control, as
#   LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/left-session-claims.sh [--base-commit <rev>]
# LEGION_TEST_PG_DSN is required and names the PostgreSQL the daemon migrates and runs on; the
# driver also creates Dispatch's database on the same server (its role must be allowed to create
# and drop databases, and the server must offer the pgvector extension, which Dispatch's migrations
# create) and drops it on exit. That server must be this machine's: prerequisites reads the DSN
# with lib/loopback-dsn.sh's require_loopback_dsn and refuses a host that is not 127.0.0.1,
# localhost or ::1 with exit 2, before it builds or starts anything; with --base-commit the same
# guard runs first, before the base tree is taken, and a refusal of the base run is this script's
# exit 2 as well. This script starts no database. The broker is the run's own: a nats-server from
# LEGION_E2E_NATS_BIN, or the pinned one installed with `go install`. LEFT_SESSION_CLAIMS_EVIDENCE_DIR
# (default a fresh directory under TMPDIR, or /tmp when it is unset, kept and printed) keeps the
# transcript, the nats-server's log, the daemon's own (daemon.log, its JSON log at debug),
# Dispatch's (dispatch.log), the listener's (listener.log) and the driver's output. Nothing reaches
# github.com or a deployed Dispatch. Most of a run is building Dispatch, the listener and the
# driver. The run around the driver is lib/live-daemon-proof.sh's.
#
# --base-commit <rev> is the negative control, and an empty <rev> is refused with exit 2. It is run
# by hand from a full checkout: it takes <rev> from this repository's own history, which a shallow
# CI checkout carries none of, so e2e.yaml's hosted run makes the proof without it. The tree of <rev>
# is taken with `git archive` into a directory of the run's own under TMPDIR (removed on any exit),
# this checkout's driver file is laid over it, `bun install --frozen-lockfile` gives it
# node_modules of its own for the dispatch CLI, and the same run is made there: the driver, Dispatch,
# the listener and the CLI are all that tree's. The driver names no Go symbol the base lacks — it
# reads claimLeft from the record route as plain JSON and the claim-left entry's number at run
# time — so it builds there. The control passes when that run fails and
# a-fresh-session-releases-the-left-session fails at released (no release of the left session's
# claim comes) and a-persons-claim-stays-with-its-reason at claim-left (no record names why the
# person's claim stays); what a-move-releases-its-trees-claim did there is printed as it came. A
# proof.log an earlier run left in LEFT_SESSION_CLAIMS_EVIDENCE_DIR is removed before the base run,
# so only that run's steps are read. Its last line is `left session claims negative control:
# PASS|FAIL …` with the base run's evidence directory.
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

test_name=TestLeftSessionClaimsLive
driver_file=packages/daemon/internal/daemon/left_session_claims_live_test.go

# The steps the base must fail, each with the assertion it must fail at (the driver's `fails`).
base_failures=(
  'a-fresh-session-releases-the-left-session released'
  'a-persons-claim-stays-with-its-reason claim-left'
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
    "left session claims negative control: PASS in $((SECONDS - started))s (evidence: $base_evidence)" \
    "left session claims negative control: FAIL at $check after $((SECONDS - started))s (evidence: $base_evidence)"
}

# negative_control makes the run over the base tree in a subshell of its own — live_daemon_proof
# ends the shell it runs in, and its EXIT trap and its end mark are that subshell's — and then reads
# the base run's proof.log for each step's own failure.
negative_control() {
  local base_sha status expected step assertion line detail tool db_host
  started=$SECONDS
  base_evidence=${LEFT_SESSION_CLAIMS_EVIDENCE_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-left-session-claims-base-evidence.XXXXXXXX")}
  mkdir -p "$base_evidence"
  trap control_cleanup EXIT
  transcript_to "$base_evidence/negative-control.log"

  begin base-tree
  # The base run's own prerequisites read the DSN too, but only after the archive and the install
  # below; a DSN it would refuse is refused here first, with the same guard and the same exit 2.
  [ -n "${LEGION_TEST_PG_DSN:+set}" ] ||
    refuse "LEGION_TEST_PG_DSN is required: it names the PostgreSQL the daemon migrates and runs on (postgres://<user>@<host>:<port>/<database>?sslmode=disable). This script starts no database."
  for tool in git tar bun python3; do
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
  base_work=$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-left-session-claims-base.XXXXXXXX")
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
    # The driver builds Dispatch and the listener from, and runs the CLI out of, the checkout this
    # exports: the base tree's, as the driver itself is.
    require_dispatch_cli
    live_daemon_proof "left session claims at ${base_sha:0:12}" left-session-claims-base "$test_name" "$base_evidence"
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
  grep -q '^== a-persons-claim-stays-with-its-reason$' "$base_evidence/proof.log" 2>/dev/null ||
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
    # between its `== <step>` and its `FAIL <step>` lines.
    detail=$(awk -v step="$step" -v said=": $assertion: " '
      $0 == "== " step { inside = 1; next }
      inside && index($0, "FAIL " step " at ") == 1 { exit }
      inside && $0 ~ /left_session_claims_live_test\.go:[0-9]+: / && index($0, said) { print; exit }
    ' "$base_evidence/proof.log")
    note "$line"
    note "  ${detail#"${detail%%[![:space:]]*}"}"
  done
  line=$(grep -m1 -E "^(ok|FAIL) a-move-releases-its-trees-claim " "$base_evidence/proof.log") ||
    line="no outcome for a-move-releases-its-trees-claim"
  note "as it came: $line"
  pass
  run_end
}

if [ -z "$base_commit" ]; then
  require_dispatch_cli
  live_daemon_proof 'left session claims' left-session-claims "$test_name" \
    "${LEFT_SESSION_CLAIMS_EVIDENCE_DIR:-}"
else
  negative_control
fi
