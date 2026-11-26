#!/usr/bin/env bash
# Tests for `ci-ok-retries.sh`, which names in ci-ok's summary each Go test that passed only on its
# retry and each package that failed with no test failing. It reads jsonfiles and rerun reports
# gotestsum wrote in .github/scripts/go-test.test.sh's own runs, kept in
# .github/scripts/testdata/ci-ok-retries/ (its README names the command that wrote them): a test
# that failed once and then passed, one that failed twice, and a TestMain that failed after its
# tests passed and passed the rerun that ran no test.
#
# Run from anywhere: .github/scripts/ci-ok-retries.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
reporter="$script_dir/ci-ok-retries.sh"
captures="$script_dir/testdata/ci-ok-retries"

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

fixture=example.com/gotestfixtures

# run_reporter <directory>: runs the script under test over the directory with a step summary file
# of its own, and leaves its stdout and stderr in $out, what it wrote to the step summary in
# $written and its status in $status.
run_reporter() {
  : >"$work/step-summary"
  status=0
  out=$(env GITHUB_STEP_SUMMARY="$work/step-summary" "$reporter" "$@" 2>&1) || status=$?
  written=$(cat "$work/step-summary")
}

echo "case: the three runs' files, downloaded into one directory as ci-ok downloads them"
mkdir -p "$work/merged"
cp "$captures"/*.json "$captures"/*.rerun-fails.txt "$work/merged/"
run_reporter "$work/merged"
check "exits 0" "$(is "$status" 0)"
check "says how much it read" "$(contains "$written" '^Read 3 jsonfiles and 3 rerun reports\.$')"
check "names the test that failed once and then passed as passing only on its retry" \
  "$(contains "$written" "^- \`$fixture/once\.TestFailsOnceThenPasses\`: 2 runs, 1 failures, in once\.json\$")"
check "does not name the test that failed twice" "$(is "$(contains "$written" 'TestFailsEveryAttempt')" false)"
check "does not name a test that passed the first time" "$(is "$(contains "$written" 'TestPassesEveryTime')" false)"
check "names the TestMain package that failed with no test failing, by its attempt" \
  "$(contains "$written" "^- \`$fixture/testmain\`, in attempt 1, in testmain\.json\$")"
check "does not name the package whose test failed twice" "$(is "$(contains "$written" "$fixture/twice\`")" false)"
check "prints the summary on stdout too" "$(contains "$out" "$fixture/once\.TestFailsOnceThenPasses")"

echo "case: a pull request's run, which writes no rerun report, names no flake"
mkdir -p "$work/pull-request"
cp "$captures/once.json" "$work/pull-request/"
run_reporter "$work/pull-request"
check "exits 0" "$(is "$status" 0)"
check "reads one jsonfile and no rerun report" "$(contains "$written" '^Read 1 jsonfiles and 0 rerun reports\.$')"
check "names no flake" "$(contains "$written" '^No Go test passed only on its retry\.$')"

echo "case: each artifact in a directory of its own, as a download without merge-multiple lays them"
mkdir -p "$work/nested/go-test-envoy-go-unit"
cp "$captures/once.json" "$captures/once.rerun-fails.txt" "$work/nested/go-test-envoy-go-unit/"
run_reporter "$work/nested"
check "finds the file below the directory and names it by its path there" \
  "$(contains "$written" "TestFailsOnceThenPasses\`: 2 runs, 1 failures, in go-test-envoy-go-unit/once\.json\$")"

echo "case: no jsonfile at all, which is a run whose Go jobs were all skipped"
mkdir -p "$work/empty"
run_reporter "$work/empty"
check "exits 0" "$(is "$status" 0)"
check "says no Go step uploaded a jsonfile" "$(contains "$written" '^No Go step uploaded a jsonfile to this run')"

echo "case: a jsonfile jq cannot read is named, and the rest is still read"
mkdir -p "$work/broken"
cp "$captures/once.json" "$captures/once.rerun-fails.txt" "$work/broken/"
printf '{"Action":"start","Package":"cut off' >"$work/broken/cut.json"
run_reporter "$work/broken"
check "exits 0" "$(is "$status" 0)"
check "names the file it could not read" "$(contains "$written" '^- cut\.json: ')"
check "warns about it in the log" "$(contains "$out" '^::warning::ci-ok-retries.sh: could not read cut\.json: ')"
check "still names the flake of the file it read" "$(contains "$written" 'TestFailsOnceThenPasses')"

echo "case: a missing directory is a usage error"
run_reporter "$work/no-such-directory"
check "exits 2" "$(is "$status" 2)"

summary "ci-ok-retries.sh"
