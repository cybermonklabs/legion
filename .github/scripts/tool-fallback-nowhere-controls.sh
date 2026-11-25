#!/usr/bin/env bash
# The negative control's own reading: all nine installs with nowhere to go must have failed, and
# each must have failed in its own time rather than being ended by the runner at its
# `timeout-minutes`, which leaves the same outcome behind.
#
# Who runs it: the step "Check all nine installs with nowhere to go failed, and failed in their
# own time" of .github/workflows/ci-job-tool-fallback.yaml's tool-fallback job;
# .github/ci-notes/ci-job-tool-fallback.md#why-the-control-step-reads-the-clock-as-well says
# why the clock is read here at all. It takes no argument and reads eighteen variables the step's
# own `env:` map sets, each from a step of that job: CRANE, CHANGIE, JJ, BUN, ELAN, UV,
# PULUMI_DOCKER, PULUMI_CLI and SKILL_CUTS_OMP, the `outcome` of each install with nowhere to go,
# and CRANE_AT, CHANGIE_AT, JJ_AT, BUN_AT, ELAN_AT, UV_AT, PULUMI_DOCKER_AT, PULUMI_CLI_AT and
# SKILL_CUTS_OMP_AT, the whole number
# of seconds the mark step before each install wrote. That job checks this repository out in its
# first step, above every step below it, so this script and the rig it reads the bound from are
# both in the checkout when the step runs.
#
# Run from the repository root: .github/scripts/tool-fallback-nowhere-controls.sh
# CI runs it in ci-job-tool-fallback.yaml's tool-fallback job, at the step "Check all nine
# installs with nowhere to go failed, and failed in their own time".
set -euo pipefail

echo "a merge base from before this change reads none of the CI_TOOLS_* seams, so each of these installs has the one source it always had: the release asset this fault holds for crane, changie, jj, bun, elan and uv, and get.pulumi.com, which this job does not fault, behind the dead proxy the pulumi step carries for that reason"
echo "a base that reads the seams — this tree on a workflow_dispatch, and every base once this change is on main — is sent by the dead ports above to something nothing listens on, and that is what fails them instead; the docker provider plugin and the skill-cuts Oh My Pi, which the merge base has no action for, are each the head's own action with its second source on a dead port, and each of those controls states the same property without reading the base tree at all"
wrong=""
for pair in "crane:$CRANE" "changie:$CHANGIE" "jj:$JJ" "bun:$BUN" "elan:$ELAN" "uv:$UV" "pulumi-docker:$PULUMI_DOCKER" "pulumi-cli:$PULUMI_CLI" "skill-cuts-omp:$SKILL_CUTS_OMP"; do
  echo "  the install with nowhere to go for ${pair%%:*} ended in ${pair#*:}"
  [ "${pair#*:}" = failure ] || wrong="$wrong ${pair%%:*}"
done
if [ -n "$wrong" ]; then
  echo "::error::the install with nowhere to go for$wrong served bytes with its primary behind the fault or behind a dead proxy and its second source pointed at a dead port, so it read something this job did not serve — a cache, a tool the runner image carries, or a request that went around the proxy — and the passes below would then say nothing about the second source" >&2
  exit 1
fi
# Each install's own window: from its own mark to the mark of the install after it,
# and for the last of them to this step's own time. No install here may reach the
# seconds .github/scripts/ci-tools-proof.sh holds every timed install to, which
# this step reads from that script rather than writing the number again: the rig holds
# the eighteen corrupted cases below to that same bound, and a bound written in two
# places is a bound that drifts, with the job then refusing two different things.
# INSTALL_BOUND in that script is where the number lives, and the comment on it says
# why it is the number it is. A window also names the install that hung.
# A mark this step cannot read as a number is refused before any arithmetic, the way
# `report` refuses a case whose mark holds no `started=`: a window read with no clock
# is exactly the window the clock exists to catch, so it may not pass quietly. Left to
# the arithmetic below, an empty mark reads as a large negative number, which is under
# the bound, and then stops the loop on the empty operand with the step exiting 0 and
# eight of the nine windows never read.
# A bound this step cannot read is refused too, for a different reason, since the step
# would fail either way: driven with the guard taken out, an empty bound leaves
# `[ "$seconds" -lt "" ]` failing on every window, so all nine land in `slow` and the
# step exits 1 saying they `took  seconds or more`. That reading sends a reader to the
# nine installs, which did nothing wrong, instead of to the rig that answered
# nothing. The guard buys the right name, not the failure.
bound=$(.github/scripts/ci-tools-proof.sh install-bound)
case "$bound" in
  '' | *[!0-9]*)
    echo "::error::.github/scripts/ci-tools-proof.sh install-bound answered '${bound:-nothing}', which is no whole number of seconds, so this step has no bound to hold these nine installs to; the bound is INSTALL_BOUND in that script, written there once and read here, and this step holds no number of its own to fall back on" >&2
    exit 1
    ;;
esac
echo "no install with nowhere to go may reach the ${bound}s .github/scripts/ci-tools-proof.sh holds every timed install to"
previous=""
slow=""
unreadable=""
for pair in "crane:$CRANE_AT" "changie:$CHANGIE_AT" "jj:$JJ_AT" "bun:$BUN_AT" "elan:$ELAN_AT" "uv:$UV_AT" "pulumi-docker:$PULUMI_DOCKER_AT" "pulumi-cli:$PULUMI_CLI_AT" "skill-cuts-omp:$SKILL_CUTS_OMP_AT"; do
  case "${pair#*:}" in
    '' | *[!0-9]*) unreadable="$unreadable ${pair%%:*}" ;;
  esac
done
if [ -n "$unreadable" ]; then
  echo "::error::the mark of the install with nowhere to go for$unreadable holds no whole number of seconds, so this step cannot tell how long that install took, and that reading is the one thing that tells an install the runner ended at its own timeout-minutes from an install that failed for the reason this control names; the mark is one echo into \$GITHUB_OUTPUT by the step before that install, so read this job's step record for it" >&2
  exit 1
fi
for pair in "crane:$CRANE_AT" "changie:$CHANGIE_AT" "jj:$JJ_AT" "bun:$BUN_AT" "elan:$ELAN_AT" "uv:$UV_AT" "pulumi-docker:$PULUMI_DOCKER_AT" "pulumi-cli:$PULUMI_CLI_AT" "skill-cuts-omp:$SKILL_CUTS_OMP_AT" "this check:$(date +%s)"; do
  if [ -n "$previous" ]; then
    seconds=$(( ${pair#*:} - ${previous#*:} ))
    echo "  the install with nowhere to go for ${previous%%:*} took ${seconds}s"
    [ "$seconds" -lt "$bound" ] || slow="$slow ${previous%%:*}"
  fi
  previous=$pair
done
if [ -n "$slow" ]; then
  echo "::error::the install with nowhere to go for$slow took ${bound} seconds or more, which no timed install does, so the runner ended that step at its own timeout-minutes rather than the step failing for the reason this control names: a step the runner ends leaves outcome=failure behind it, which the test above accepts, so the wall time is the only reading that tells the two apart; read the step record of this job (gh api repos/<owner>/<repo>/actions/jobs/<id> --jq .steps[]) for the step that ran to its bound, and its log for 'The action has timed out.'" >&2
  exit 1
fi
