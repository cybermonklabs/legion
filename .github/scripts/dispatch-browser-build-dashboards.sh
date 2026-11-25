#!/usr/bin/env bash
# The dashboards the Dispatch browser suite's rows read, built once for the whole run: the
# dashboard bundle, web/dist, which every browser job serves, and on the full lane the second
# rollout bundle, web/dist-b, which the multiprocess job's two-builds rows read. Both are copied
# under $RUNNER_TEMP for the upload that hands them to the jobs that run rows.
#
# Who runs it, and what it takes:
#
#   .github/scripts/dispatch-browser-build-dashboards.sh <lane>
#
# The step "Build the binaries and the dashboard the browser suite runs" of
# .github/workflows/dispatch-browser.yaml's binaries job starts it in the background beside that
# step's go builds and waits on it by process id, so the bundles' time is spent inside the go
# builds' rather than after it;
# .github/ci-notes/dispatch-browser.md#what-the-binaries-build-writes-and-why-it-is-static says
# why, and why the go builds stay in the step rather than here. <lane> is the run's lane, the
# workflow's own `${{ inputs.lane }}`: `full` builds the second rollout bundle beside the
# dashboard, and any other lane builds the dashboard alone, since the full lane is the only one
# that runs the multiprocess job. It is an argument rather than a key of the step's `env:`, for
# the reason
# .github/ci-notes/dispatch-browser.md#the-shard-jobs-shell-rule-its-measured-steps-and-its-budget
# gives.
#
# It runs from the repository root, which is that step's directory: it writes
# packages/dispatch/web/dist and, on the full lane, packages/dispatch/web/dist-b, and copies them
# into $RUNNER_TEMP/dispatch-e2e-web, which the upload after the step reads. The runner sets
# $RUNNER_TEMP for every step.
#
# Run from the repository root, with $RUNNER_TEMP set:
#   .github/scripts/dispatch-browser-build-dashboards.sh full
# CI runs it in dispatch-browser.yaml's binaries job, at the step "Build the binaries and the
# dashboard the browser suite runs".
set -euo pipefail

usage() {
  echo "::error::dispatch-browser-build-dashboards.sh takes the run's lane:" \
    ".github/scripts/dispatch-browser-build-dashboards.sh <pull-request|full|references>. It was given: $*"
  exit 1
}

[ "$#" -eq 1 ] || usage "$@"
lane=$1

# The two builds are background jobs of this script, each waited on by its own process id, and
# each `wait` is read on its own, so that a build which failed fails the step and the line printed
# beside it names which build it was: one `wait` under `set -e` would end at the first failure
# and leave the other unreported.
(cd packages/dispatch && bun run build:web && ls -l web/dist/THIRD_PARTY_NOTICES.txt) &
dashboard=$!
rollout=
if [ "$lane" = full ]; then
  (cd packages/dispatch && bun run build:web-b) &
  rollout=$!
fi
status=0
wait "$dashboard" || { echo "::error::the dashboard bundle did not build"; status=1; }
if [ -n "$rollout" ]; then
  wait "$rollout" || { echo "::error::the second rollout bundle (web/dist-b) did not build"; status=1; }
fi
[ "$status" -eq 0 ] || exit "$status"
mkdir -p "$RUNNER_TEMP/dispatch-e2e-web"
cp -R packages/dispatch/web/dist "$RUNNER_TEMP/dispatch-e2e-web/"
[ -z "$rollout" ] || cp -R packages/dispatch/web/dist-b "$RUNNER_TEMP/dispatch-e2e-web/"
