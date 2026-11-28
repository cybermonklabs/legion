#!/usr/bin/env bash
# Drive the real Legion tool against a whole daemon and scratch Dispatch in a builds Job.
# No model service is used: the provider sends the calls from a known trace or a lane R
# transcript. --runs starts each run from its case's starting state and applies only the changes
# the run recorded before each call (scripts/e2e/README.md#teardown-effectssh). --controls replays
# each case's known-good and known-bad trace the same way, makes the checks each trace's actions
# show, and fails, naming each, where a control it promised did not execute. --supplemental names a
# public case file, one the checkout's commit (LEGION_E2E_TEARDOWN_COMMIT) holds unchanged, whose
# cases --controls makes controls of too, beside the case file's, in the same run: the coverage
# check counts both files' controls, and a case id both files hold is refused. Each replay writes
# replay.json and, unless it is HARNESS, effects.json: a run's under its outcome id, a trace's under
# <case id>/<good or bad>, a supplemental case's under supplemental/<case id>/<good or bad>.
# --controls also writes daemon-answers.json, of the case file's cases alone, and
# supplemental/daemon-answers.json, of the supplemental file's.
set -euo pipefail

usage() {
  echo 'usage: teardown-effects.sh --controls --cases <file> [--supplemental <public case file>] | --runs <directory>' >&2
  exit 2
}
# The physical path, as realpath gives the case file below, so a symlinked checkout keeps its label.
root=$(cd "$(dirname "$0")/../.." && pwd -P)
case ${1:-} in
  --controls)
    [[ ($# == 3 || $# == 5 && $4 == --supplemental && -f ${5:-}) && $2 == --cases && -f $3 ]] || usage
    export LEGION_E2E_TEARDOWN_MODE=controls
    LEGION_E2E_TEARDOWN_CASES=$(realpath "$3")
    export LEGION_E2E_TEARDOWN_CASES
    export LEGION_E2E_TEARDOWN_CASES_LABEL=${LEGION_E2E_TEARDOWN_CASES#"$root/"}
    if [[ $LEGION_E2E_TEARDOWN_CASES_LABEL == /* ]]; then
      export LEGION_E2E_TEARDOWN_CASES_LABEL='<case-file>'
    fi
    LEGION_E2E_TEARDOWN_SUPPLEMENTAL=''
    if [[ $# == 5 ]]; then
      LEGION_E2E_TEARDOWN_SUPPLEMENTAL=$(realpath "$5")
    fi
    export LEGION_E2E_TEARDOWN_SUPPLEMENTAL
    LEGION_E2E_TEARDOWN_COMMIT=${LEGION_E2E_TEARDOWN_COMMIT:-$(git -C "$root" rev-parse HEAD)}
    [[ $LEGION_E2E_TEARDOWN_COMMIT =~ ^[0-9a-f]{40}$ ]] || usage
    export LEGION_E2E_TEARDOWN_COMMIT
    ;;
  --runs)
    [[ $# == 2 && -d $2 ]] || usage
    LEGION_E2E_TEARDOWN_RUNS=$(realpath "$2")
    export LEGION_E2E_TEARDOWN_RUNS
    export LEGION_E2E_TEARDOWN_MODE=runs
    ;;
  *) usage ;;
esac
# shellcheck source-path=SCRIPTDIR source=lib/live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"
[[ ${LEGION_OMP_PATH:-} == /* && -x ${LEGION_OMP_PATH:-} ]] ||
  refuse 'LEGION_OMP_PATH must name an absolute, executable Oh My Pi binary'
export LEGION_E2E_TEARDOWN_OMP=$LEGION_OMP_PATH
require_dispatch_cli
if [[ $LEGION_E2E_TEARDOWN_MODE == controls ]]; then
  bun "$root/packages/pi-legion/scripts/skill-scenarios/heldout-case.ts" parse "$LEGION_E2E_TEARDOWN_CASES"
  if [[ -n $LEGION_E2E_TEARDOWN_SUPPLEMENTAL ]]; then
    bun "$root/packages/pi-legion/scripts/skill-scenarios/heldout-case.ts" parse "$LEGION_E2E_TEARDOWN_SUPPLEMENTAL"
  fi
fi
if [[ -n ${TEARDOWN_EFFECTS_EVIDENCE_DIR:-} ]]; then
  TEARDOWN_EFFECTS_EVIDENCE_DIR=$(realpath -m "$TEARDOWN_EFFECTS_EVIDENCE_DIR")
fi
live_daemon_proof 'teardown effects' teardown-effects TestTeardownEffectsLive "${TEARDOWN_EFFECTS_EVIDENCE_DIR:-}" 30m
