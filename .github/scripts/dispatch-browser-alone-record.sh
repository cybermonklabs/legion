#!/usr/bin/env bash
# What an alone leg's run left behind: the stack's own server logs, in the job's own log and
# copied into the one record directory the upload after it takes, beside the sampler's two files
# the browser step already wrote there.
#
# Who runs it: the step "What the run left behind" of .github/workflows/dispatch-browser.yaml's
# alone job; .github/ci-notes/dispatch-browser.md#what-the-alone-job-collects-after-a-run says why
# it runs on every leg and not on a failed one alone. It takes
# no argument: $RUNNER_TEMP is where the browser step before it wrote the run directory and the
# sampler's two files.
# The runner sets $RUNNER_TEMP for every step.
#
# The shard job's step of this name is .github/scripts/dispatch-browser-shard-record.sh, a
# script of its own: that job runs a file store beside its stacks, and collects its log too,
# while no store runs on this job's machine.
#
# Run from anywhere, with $RUNNER_TEMP set: .github/scripts/dispatch-browser-alone-record.sh
# CI runs it in dispatch-browser.yaml's alone job, at the step "What the run left behind".
set -euo pipefail

echo "=== $RUNNER_TEMP ==="
ls -l "$RUNNER_TEMP"
echo "=== the stack's own logs (tail) ==="
for log in "$RUNNER_TEMP"/dispatch-e2e-run-*/worker-*/log/*.log; do
  [ -f "$log" ] || continue
  echo "--- $log"
  tail -30 "$log"
done
record="$RUNNER_TEMP/dispatch-e2e-record"
mkdir -p "$record/workers"
for log in "$RUNNER_TEMP"/dispatch-e2e-run-*/worker-*/log/*.log; do
  [ -f "$log" ] || continue
  worker=$(basename "$(dirname "$(dirname "$log")")")
  mkdir -p "$record/workers/$worker"
  cp "$log" "$record/workers/$worker/"
done
ls -lR "$record"
