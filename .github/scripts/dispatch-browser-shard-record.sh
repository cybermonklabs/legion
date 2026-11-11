#!/usr/bin/env bash
# What a shard leg's run left behind: the file store's log and the stacks' own server logs, in
# the job's own log where a reader of a failed row looks first, and copied into the one record
# directory the upload after it takes.
#
# Who runs it: the step "What the run left behind" of .github/workflows/dispatch-browser.yaml's
# shard job, whose comment says why it runs on every leg and not on a failed one alone. It takes
# no argument: $RUNNER_TEMP is where the browser step before it wrote the run directory, the
# sampler's two files and the store's log.
#
# The alone job's step of this name is .github/scripts/dispatch-browser-alone-record.sh, a
# script of its own: no file store runs on that job's machine, so it collects no store log.
#
# Run from anywhere: .github/scripts/dispatch-browser-shard-record.sh
# CI runs it in dispatch-browser.yaml's shard job, at the step "What the run left behind".
set -euo pipefail

echo "=== $RUNNER_TEMP ==="
ls -l "$RUNNER_TEMP"
echo "=== the file store's log (tail) ==="
tail -80 "$RUNNER_TEMP/weed.log" 2>/dev/null || echo "no weed log"
echo "=== the stacks' own logs (tail) ==="
for log in "$RUNNER_TEMP"/dispatch-e2e-run-*/worker-*/log/*.log; do
  [ -f "$log" ] || continue
  echo "--- $log"
  tail -30 "$log"
done
record="$RUNNER_TEMP/dispatch-e2e-record"
mkdir -p "$record/workers"
[ ! -f "$RUNNER_TEMP/weed.log" ] || cp "$RUNNER_TEMP/weed.log" "$record/"
for log in "$RUNNER_TEMP"/dispatch-e2e-run-*/worker-*/log/*.log; do
  [ -f "$log" ] || continue
  worker=$(basename "$(dirname "$(dirname "$log")")")
  mkdir -p "$record/workers/$worker"
  cp "$log" "$record/workers/$worker/"
done
ls -lR "$record"
