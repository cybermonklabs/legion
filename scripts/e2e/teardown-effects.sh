#!/usr/bin/env bash
# Drive the real Legion tool against a whole daemon and scratch Dispatch in a builds Job.
# No model service is used: the provider sends the calls from a known-good trace or a lane R
# transcript. --runs starts each run from its case's starting state and applies only the changes
# the run recorded before each call (scripts/e2e/README.md#teardown-effectssh). The run writes
# effects.json for each proof, replay.json for each run and daemon-answers.json for --controls.
set -euo pipefail

usage() {
  echo 'usage: teardown-effects.sh --controls --cases <file> | --runs <directory>' >&2
  exit 2
}
root=$(cd "$(dirname "$0")/../.." && pwd)
case ${1:-} in
  --controls)
    [[ $# == 3 && $2 == --cases && -f $3 ]] || usage
    export LEGION_E2E_TEARDOWN_MODE=controls
    LEGION_E2E_TEARDOWN_CASES=$(realpath "$3")
    export LEGION_E2E_TEARDOWN_CASES
    export LEGION_E2E_TEARDOWN_CASES_LABEL=${LEGION_E2E_TEARDOWN_CASES#"$root/"}
    if [[ $LEGION_E2E_TEARDOWN_CASES_LABEL == /* ]]; then
      export LEGION_E2E_TEARDOWN_CASES_LABEL='<case-file>'
    fi
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
[[ ${LEGION_OMP_PATH:-} == /* && -x ${LEGION_OMP_PATH:-} ]] || {
  echo 'FAIL setup: LEGION_OMP_PATH must name an absolute, executable Oh My Pi binary' >&2
  exit 2
}
export LEGION_E2E_TEARDOWN_OMP=$LEGION_OMP_PATH
# shellcheck source-path=SCRIPTDIR source=lib/live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"
require_dispatch_cli
if [[ $LEGION_E2E_TEARDOWN_MODE == controls ]]; then
  bun "$root/packages/pi-legion/scripts/skill-scenarios/heldout-case.ts" parse "$LEGION_E2E_TEARDOWN_CASES"
fi
if [[ -n ${TEARDOWN_EFFECTS_EVIDENCE_DIR:-} ]]; then
  TEARDOWN_EFFECTS_EVIDENCE_DIR=$(realpath -m "$TEARDOWN_EFFECTS_EVIDENCE_DIR")
fi
live_daemon_proof 'teardown effects' teardown-effects TestTeardownEffectsLive "${TEARDOWN_EFFECTS_EVIDENCE_DIR:-}" 30m
