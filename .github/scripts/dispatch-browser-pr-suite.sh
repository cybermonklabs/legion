#!/usr/bin/env bash
# Run from packages/dispatch: dispatch-browser-pr-suite.sh real <connections> or ... stub.
# Neither leg is sharded. The merge job reads each leg's blob and row count, just as it does for
# alone; only the full and references shards write matching selection lists.
set -euo pipefail

leg=${1:-}
case "$leg:$#" in
  real:2)
    projects=(--project=chromium --project=iphone)
    export DISPATCH_E2E_PG_BIN=/usr/lib/postgresql/18/bin
    export DISPATCH_E2E_PG_CONNECTIONS=$2
    export DISPATCH_E2E_SERVER_BIN="$GITHUB_WORKSPACE/packages/envoy/out/dispatch-e2e-broker/dispatch"
    export DISPATCH_E2E_BROKER_BIN_DIR="$GITHUB_WORKSPACE/packages/envoy/out/dispatch-e2e-broker"
    ;;
  stub:1)
    projects=(--project=stub --project=stub-phone)
    export DISPATCH_E2E_STUB_ONLY=1
    ;;
  *)
    echo "::error::usage: dispatch-browser-pr-suite.sh real <connections> | stub"
    exit 1
    ;;
esac

export TMPDIR="$RUNNER_TEMP"
export PLAYWRIGHT_BLOB_OUTPUT_DIR="$RUNNER_TEMP/leg-report/blob-report"
export PLAYWRIGHT_BLOB_OUTPUT_NAME="report-$leg.zip"
record="$RUNNER_TEMP/dispatch-e2e-record"
mkdir -p "$RUNNER_TEMP/leg-report/rows" "$record"
samples="$GITHUB_WORKSPACE/.github/scripts/dispatch-browser-samples.sh"
"$samples" sample "$record" &
sampler=$!
trap 'kill "$sampler" 2>/dev/null || true' EXIT

# Keep the row count and samples when Playwright fails, but still fail the step.
status=0
bunx playwright test --config e2e/playwright.config.ts "${projects[@]}" \
  --grep='(^|\s)@pr(\s|$)' --workers=4 --reporter=list,blob,./e2e/failure-reporter.ts 2>&1 |
  tee "$RUNNER_TEMP/playwright-log.txt" || status=$?
kill "$sampler" 2>/dev/null || true
sed -n 's/^Running \([0-9][0-9]*\) tests\? using .*/\1/p' "$RUNNER_TEMP/playwright-log.txt" |
  tail -n 1 > "$RUNNER_TEMP/leg-report/rows/$leg.txt"
"$samples" report "$record" "$leg"
exit "$status"
