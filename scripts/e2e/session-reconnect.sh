#!/usr/bin/env bash
# Check w4 of this change's verification: a session's registration and delivery come back after each
# end (R4). The whole daemon runs on the rig of this change's checks
# (packages/daemon/internal/daemon/RIG.md): the listener production pins, built from its own commit,
# behind the rig's front; a real nats-server with production's grants; a PostgreSQL of the run's
# own; and one real `omp --mode rpc` pane under the real `legion worker-shim`, with this checkout's
# @sjawhar/pi-envoy and @sjawhar/pi-legion, whose turns go to the model the run names. The driver is
# packages/daemon/internal/daemon/w4_reconnect_live_test.go; its stand-in secrets broker is
# w4_broker_live_test.go beside it.
#
# Each end runs twice, the session idle and the session in a `wait` call, each on a rig of its own
# (one driver run, about 30 to 45 minutes), in this order:
#
#   listener-outage     the front holds every request for 10 minutes
#   nats-outage         the nats-server stops for 10 minutes and starts again
#   heartbeat-stopped   the plugin's heartbeat timer stops (ENVOY_HEARTBEAT_STOP_AFTER_MS)
#   heartbeat-hung      the front holds the session's next registration for 10 minutes
#   renewal-unanswered  the stand-in broker does not answer the shim's first renewal
#
# Each run prints, with the times, the numbered messages published to the session's role every
# 30 s for 15 minutes and when each reached the transcript, Envoy's session list each minute,
# /connz and the registrations at the front through the listener outage, the renewals the
# stand-in got, and the clean stop; then `ok <end>-<state>` or `FAIL <end>-<state> at <assertion>`,
# with every assertion the run failed printed before as `violation <assertion>: …`. Each run's
# evidence directory keeps w4-<end>-<state>.results, one JSON line per reading, written as it is
# read.
#
# Run it from a checkout on a Linux host as
#   LEGION_OMP_PATH=<omp> LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     LEGION_E2E_MODEL_BASE_URL=<anthropic-messages endpoint> LEGION_E2E_MODEL_KEY_COMMAND=<command> \
#     bash scripts/e2e/session-reconnect.sh [--end <end>] [--state idle|wait] [--base-commit <rev>]
# On a development host the model is the shared gateway, https://model-gateway.<tailnet>.ts.net,
# and its key command one that prints the gateway key. lib/live-rig.sh names every input and what it
# refuses. --end and --state pick one end or one state; without them every run is made, one after
# the other. SESSION_RECONNECT_EVIDENCE_DIR (default a fresh directory under TMPDIR, kept and
# printed) keeps each run's evidence in a directory named for it. It needs no docker, no tmux and no
# kube context.
#
# --base-commit <rev> is the check's negative control. A detached worktree of <rev> is made under
# TMPDIR (removed on any exit), this checkout's rig — the testenvoy package, the rig's three files,
# this check's two and lib/live-rig-grants.go — is laid over it, with the rig's heartbeat seam
# (ENVOY_HEARTBEAT_STOP_AFTER_MS, as this checkout's plugin reads it) applied to its plugin, and
# `bun install --frozen-lockfile` gives it node_modules of its own; the same runs are made there:
# the daemon, `legion`, `agent-secrets` and both plugins are the base's. Every stopped-heartbeat run
# among them must fail at its delivery with no message published from 6 minutes after the heartbeat
# stopped reaching the session within the 15 minutes, and the control names each run the base did
# not survive. Without --end it makes every run (the whole control, several hours); with --end
# heartbeat-stopped it makes the control's own two. Its last line is `session reconnect negative
# control: PASS|FAIL …` with the evidence directory.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
all_ends=(listener-outage nats-outage heartbeat-stopped heartbeat-hung renewal-unanswered)
all_states=(idle wait)
usage="usage: $0 [--end <$(
  IFS='|'
  echo "${all_ends[*]}"
)>] [--state idle|wait] [--base-commit <rev>]"
base_commit=''
base_given=false
only_end=''
only_state=''
while [ $# -gt 0 ]; do
  case "$1" in
    --base-commit | --end | --state)
      [ $# -ge 2 ] || {
        echo "$usage" >&2
        exit 2
      }
      case "$1" in
        --base-commit)
          base_commit=$2
          base_given=true
          ;;
        --end) only_end=$2 ;;
        --state) only_state=$2 ;;
      esac
      shift 2
      ;;
    *)
      echo "$usage" >&2
      exit 2
      ;;
  esac
done
if [ "$base_given" = true ] && [ -z "$base_commit" ]; then
  echo "--base-commit names no commit" >&2
  exit 2
fi
if [ -n "$only_end" ] && [[ " ${all_ends[*]} " != *" $only_end "* ]]; then
  echo "--end $only_end is none of ${all_ends[*]}" >&2
  exit 2
fi
if [ -n "$only_state" ] && [[ " ${all_states[*]} " != *" $only_state "* ]]; then
  echo "--state $only_state is none of ${all_states[*]}" >&2
  exit 2
fi
# shellcheck source-path=SCRIPTDIR source=lib/live-rig.sh
. "$root/scripts/e2e/lib/live-rig.sh"

test_name=TestASessionComesBackAfterEachEndLive
# What the control lays over the base's tree: the rig and this check's driver.
rig_files=(
  packages/daemon/internal/testenvoy
  packages/daemon/internal/daemon/rig_live_test.go
  packages/daemon/internal/daemon/rig_nats_live_test.go
  packages/daemon/internal/daemon/rig_runtime_live_test.go
  packages/daemon/internal/daemon/w4_reconnect_live_test.go
  packages/daemon/internal/daemon/w4_broker_live_test.go
  scripts/e2e/lib/live-rig-grants.go
)

# The runs to make, in order: the plain ends first, the stand-in broker's last.
runs=()
for end in "${all_ends[@]}"; do
  if [ -n "$only_end" ] && [ "$end" != "$only_end" ]; then
    continue
  fi
  for state in "${all_states[@]}"; do
    if [ -z "$only_state" ] || [ "$state" = "$only_state" ]; then
      runs+=("$end-$state")
    fi
  done
done

evidence_root=${SESSION_RECONNECT_EVIDENCE_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-session-reconnect-evidence.XXXXXXXX")}
mkdir -p "$evidence_root"
started=$SECONDS

# run_one TREE EXPECT RUN makes one driver run of RUN over TREE, in a subshell of its own, as
# lockout-generations.sh's control does: live_rig_proof's EXIT trap and transcript are that
# subshell's, and the checkout it reads as root is TREE. Its status is the run's.
run_one() {
  local tree=$1 expect=$2 run=$3
  (
    set -e
    root=$tree live_rig_proof "session reconnect $run${base_sha:+ at ${base_sha:0:12}}" "session-reconnect-$run" "^$test_name\$/^$run\$" "$evidence_root/$run" "$expect"
  )
}

# outcome RUN STEP is the step's last line in RUN's driver output: `ok …`, `FAIL … at …`, or none.
outcome() {
  grep -m1 -E "^(ok|FAIL) $2( |$)" "$evidence_root/$1/proof.log" 2>/dev/null || echo "no outcome: the run ended before $2 ran; see $evidence_root/$1/transcript.log"
}

# summary prints each run's two steps and every violation the run printed.
summary() {
  local run
  for run in "${runs[@]}"; do
    note "$run: $(outcome "$run" "$run")"
    grep -E '^   violation ' "$evidence_root/$run/proof.log" 2>/dev/null | sed 's/^   /     /' || true
    note "$run: $(outcome "$run" "$run-clean-stop")"
  done
}

base_sha=''
if [ -z "$base_commit" ]; then
  transcript_to "$evidence_root/session-reconnect.log"
  failed=()
  for run in "${runs[@]}"; do
    set +e
    run_one "$root" pass "$run"
    status=$?
    set -e
    [ "$status" -ne 2 ] || exit 2
    [ "$status" -eq 0 ] || failed+=("$run")
  done
  begin session-reconnect
  summary
  if [ "${#failed[@]}" -ne 0 ]; then
    fail "${#failed[@]} of ${#runs[@]} runs did not pass: ${failed[*]} (evidence: $evidence_root)"
  fi
  note "every run passed in $((SECONDS - started))s (evidence: $evidence_root)"
  pass
  exit 0
fi

# ---- the negative control --------------------------------------------------------------------------

# The control's own state, global because its EXIT trap reads it.
base_work=''
base_tree=''

control_cleanup() {
  local status=$?
  if [ -n "$base_tree" ]; then
    git -C "$root" worktree remove --force "$base_tree" >/dev/null 2>&1 || true
  fi
  [ -z "$base_work" ] || rm -rf "$base_work"
  if [ "$status" -eq 0 ]; then
    echo "session reconnect negative control: PASS in $((SECONDS - started))s (evidence: $evidence_root)"
  else
    echo "session reconnect negative control: FAIL at $check after $((SECONDS - started))s (evidence: $evidence_root)" >&2
  fi
}

# The rig's heartbeat seam over the base's plugin: the head's own seam (ENVOY_HEARTBEAT_STOP_AFTER_MS
# in packages/pi-envoy/extensions/envoy.ts) without the head's restart of the heartbeat, which is
# what the control measures the base without.
base_seam() {
  cat <<'EOF'
--- a/packages/pi-envoy/extensions/envoy.ts
+++ b/packages/pi-envoy/extensions/envoy.ts
@@ -788,6 +788,11 @@
   // the transcript path leaves no entry behind for a subagent to resolve (`src/envoy-session.ts`).
   let recordedSessionKey: string | undefined;
   let heartbeatRegistered = false;
+  // The rig's seam of this change's check w4, laid over the base for its negative
+  // control by scripts/e2e/session-reconnect.sh, as the head's own seam stops it: the heartbeat
+  // armed first stops firing this many milliseconds after it was armed.
+  let heartbeatArmings = 0;
+  const HEARTBEAT_STOP_AFTER_MS = Number(process.env.ENVOY_HEARTBEAT_STOP_AFTER_MS ?? "") || 0;
   // The session the host last announced it was leaving (`session_before_switch`,
   // `session_before_branch`) and why: read until the next pass sees where the host went, to learn
   // whether the session it went to continues the one it left, and by the frame drop meanwhile. A
@@ -3224,7 +3229,18 @@
         "warning"
       );
     };
+    const firstHeartbeat = heartbeatArmings === 0;
+    const armedAt = Date.now();
+    heartbeatArmings += 1;
     const tick = (fromRetry: boolean): void => {
+      if (
+        firstHeartbeat &&
+        !fromRetry &&
+        HEARTBEAT_STOP_AFTER_MS > 0 &&
+        Date.now() - armedAt >= HEARTBEAT_STOP_AFTER_MS
+      ) {
+        return;
+      }
       // A shutting-down session is about to be deregistered: a tick past that point would put a
       // registration back on the wire behind the DELETE and leave the record standing.
       if (shuttingDown) return;
EOF
}

trap control_cleanup EXIT
transcript_to "$evidence_root/negative-control.log"

begin base-tree
base_sha=$(git -C "$root" rev-parse --verify "$base_commit^{commit}" 2>/dev/null) ||
  refuse "--base-commit '$base_commit' names no commit of this repository"
base_work=$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-session-reconnect-base.XXXXXXXX")
base_tree=$base_work/tree
git -C "$root" worktree add --detach "$base_tree" "$base_sha" >"$evidence_root/base-tree.log" 2>&1 ||
  fail "could not make a worktree of $base_sha; see $evidence_root/base-tree.log"
for file in "${rig_files[@]}"; do
  rm -rf "${base_tree:?}/$file"
  mkdir -p "$(dirname "$base_tree/$file")"
  cp -R "$root/$file" "$base_tree/$file" || fail "could not lay $file over the tree of $base_sha"
done
base_seam | git -C "$base_tree" apply --verbose >>"$evidence_root/base-tree.log" 2>&1 ||
  fail "the rig's heartbeat seam does not apply to the plugin of $base_sha; see $evidence_root/base-tree.log"
(cd "$base_tree" && bun install --frozen-lockfile) >>"$evidence_root/base-tree.log" 2>&1 ||
  fail "bun install in the tree of $base_sha failed; see $evidence_root/base-tree.log"
note "a worktree of $base_sha in $base_tree, with this checkout's rig and driver laid over it (${rig_files[*]}) and the rig's heartbeat seam applied to its plugin"
pass

for run in "${runs[@]}"; do
  set +e
  run_one "$base_tree" fail "$run"
  status=$?
  set -e
  if [ "$status" -eq 2 ]; then
    check=base-run
    exit 2
  fi
done

begin negative-control
summary
for run in "${runs[@]}"; do
  case "$run" in
    heartbeat-stopped-*) ;;
    *) continue ;;
  esac
  grep -q "^== $run\$" "$evidence_root/$run/proof.log" 2>/dev/null ||
    fail "the run of $run over $base_sha reached no step: it failed before its proof; see $evidence_root/$run/transcript.log"
  line=$(outcome "$run" "$run")
  case "$line" in
    "FAIL $run at delivery ("*) ;;
    "ok $run "*) fail "$run passed over $base_sha, so this check tells it from this checkout in nothing" ;;
    *) fail "$run failed over $base_sha elsewhere than at its delivery: $line" ;;
  esac
  detail=$(grep -m1 -E '^   violation delivery: ' "$evidence_root/$run/proof.log") ||
    fail "$run's failure over $base_sha carries no delivery sentence; see $evidence_root/$run/proof.log"
  case "$detail" in
    *"(6 minutes after the heartbeat stopped), 0 reached the session within the window"*) ;;
    *) fail "$run over $base_sha: a message published from 6 minutes after the heartbeat stopped reached the session within the 15 minutes: $detail" ;;
  esac
  note "$run over $base_sha: ${detail#"${detail%%[![:space:]]*}"}"
done
not_survived=()
for run in "${runs[@]}"; do
  line=$(outcome "$run" "$run")
  case "$line" in
    "ok $run "*) ;;
    *) not_survived+=("$run (${line#FAIL "$run" })") ;;
  esac
done
note "the runs the base ($base_sha) did not survive: ${not_survived[*]:-none}"
case " ${runs[*]} " in
  *" heartbeat-stopped-"*) ;;
  *) note "no stopped-heartbeat run was made, so this control held nothing of its own: it names the outcomes above" ;;
esac
pass
