#!/usr/bin/env bash
# Runs, in a build-cluster pod at the head of INFRA-5580's pull request, the negative controls of
# c-merge-no-close, c-close-on-checks, c-no-checks, c-children-read, c-before-merge-next-head,
# c-fail-opens-change, c-wait-visible and v-done (Verification document v18). For each control:
# apply its one patch of the product code (patches/<control>.patch), run each command of its check
# that the control names (go test -list first, then go test -v), print the pass and fail lines and
# every _test.go:N: line, revert, and prove the tree is the head's again before the next control.
# c-before-merge-next-head-1 changes no code of this tree: it builds Dispatch from a worktree of
# the head with INFRA-5610's rule for a before_merge check taken out (dispatch/<control>.patch) and
# sets LEGION_E2E_REPO_ROOT (testbuild.RootEnv) to that worktree for its one command.
# Usage: run.sh <control|check> [...]   (a check name runs each of its controls)
set -u
here=$(cd "$(dirname "$0")" && pwd)
cd "$(git rev-parse --show-toplevel)" || exit 2
export GOFLAGS=-p=4
declare -A CMD RUNS
# label -> timeout|pattern|packages, each the Verification document's own command. The pattern
# holds | itself, so run_label splits at the first | and at the last one.
CMD[c-merge-no-close.1]="15m|^(TestAMergeInMergingDoesNotCloseWhileACheckHasNoPass|TestAMergeInRetroWaitsWhenTheIssueReachesMerging)\$|./internal/workflow"
CMD[c-merge-no-close.2]="30m|^(TestAnIssueClosesWhenTheChecksOfItsGoalPass|TestAWriteToItsOwnIssueIsTakenWhileTheIssueWaits)\$|./internal/daemon"
CMD[c-close-on-checks.1]="15m|^TestCloseWaitOfEachCase\$|./internal/record"
CMD[c-close-on-checks.2]="15m|^(TestAReadOfAllPassesClosesTheIssue|TestAMergeWithNothingToWaitForClosesAtTheRead)\$|./internal/workflow"
CMD[c-close-on-checks.3]="30m|^TestAnIssueClosesWhenTheChecksOfItsGoalPass\$|./internal/daemon"
CMD[c-no-checks.1]="15m|^TestCloseWaitOfEachCase\$|./internal/record"
CMD[c-no-checks.2]="30m|^TestAnIssueClosesWhenTheChecksOfItsGoalPass\$|./internal/daemon"
CMD[c-children-read.1]="30m|^TestAChildFiledAfterTheBootHoldsItsRootsClose\$|./internal/daemon"
CMD[c-children-read.2]="15m|^TestTheCloseReadsTheChildrenFromDispatch\$|./internal/workflow"
CMD[c-before-merge-next-head.1]="30m|^TestANextChangeNeedsAPassAtItsOwnHead\$|./internal/daemon"
CMD[c-before-merge-next-head.2]="15m|^TestCloseWaitOfEachCase\$|./internal/record"
CMD[c-fail-opens-change.1]="30m|^TestAnIssueClosesWhenTheChecksOfItsGoalPass\$|./internal/daemon"
CMD[c-fail-opens-change.2]="15m|^(TestAComputedFailStartsTheNextChangeOfAChild|TestAComputedFailStartsTheNextChangeOfARoot|TestAProductionCheckFailStartsTheNextChange)\$|./internal/workflow"
CMD[c-fail-opens-change.3]="15m|^(TestAChangeGenerationKeepsTheGate|TestTheBranchRowOfANextChangeCarriesFrom)\$|./internal/record"
CMD[c-wait-visible.1]="15m|^(TestAMergeInMergingDoesNotCloseWhileACheckHasNoPass|TestAMergeWithAnIssueBeneathWaitsInVerifying|TestTheWaitIsOneDocumentReplacedAtEachChange)\$|./internal/workflow"
CMD[c-wait-visible.2]="15m|^(TestARootThatWaitsForItsChecksIsNotReleasedAsUnslotted|TestAWaitForChecksIsAWaitByDesign|TestAQuietTreeWhoseRootWaitsForItsChildIsNoStall)\$|./internal/admit"
CMD[c-wait-visible.3]="15m|^TestTheStateNamesWhatTheCloseWaitsFor\$|./internal/projection"
CMD[c-wait-visible.4]="30m|^TestAnIssueClosesWhenTheChecksOfItsGoalPass\$|./internal/daemon"
CMD[v-done.1]="30m|^TestAnIssueClosesWhenTheChecksOfItsGoalPass\$|./internal/daemon"
# control -> the commands of its check that the control names, in the document's order.
RUNS[c-merge-no-close-1]="c-merge-no-close.1 c-merge-no-close.2"
RUNS[c-close-on-checks-1]="c-close-on-checks.1 c-close-on-checks.3"
for n in 2 3 4 5 6 7 8 9; do RUNS[c-close-on-checks-$n]="c-close-on-checks.1"; done
RUNS[c-close-on-checks-10]="c-close-on-checks.2 c-close-on-checks.3"
RUNS[c-no-checks-1]="c-no-checks.1"
RUNS[c-no-checks-2]="c-no-checks.1 c-no-checks.2"
RUNS[c-children-read-1]="c-children-read.2 c-children-read.1"
RUNS[c-before-merge-next-head-1]="c-before-merge-next-head.1"
RUNS[c-before-merge-next-head-2]="c-before-merge-next-head.2"
RUNS[c-fail-opens-change-1]="c-fail-opens-change.1 c-fail-opens-change.2"
RUNS[c-fail-opens-change-2]="c-fail-opens-change.2"
RUNS[c-fail-opens-change-3]="c-fail-opens-change.2"
RUNS[c-fail-opens-change-4]="c-fail-opens-change.3 c-fail-opens-change.2"
RUNS[c-fail-opens-change-5]="c-fail-opens-change.3"
RUNS[c-fail-opens-change-6]="c-fail-opens-change.1"
RUNS[c-wait-visible-1]="c-wait-visible.1 c-wait-visible.4"
for n in 2 3 4 5 6; do RUNS[c-wait-visible-$n]="c-wait-visible.1"; done
RUNS[c-wait-visible-7]="c-wait-visible.3"
RUNS[c-wait-visible-8]="c-wait-visible.2"
RUNS[c-wait-visible-9]="c-wait-visible.2"
RUNS[v-done-1]="v-done.1"
RUNS[v-done-2]="v-done.1"

dirty() { git status --porcelain -- . ':!ctl' | grep -v '^??'; }
keep='^(=== RUN|--- (PASS|FAIL|SKIP)|    --- (PASS|FAIL|SKIP)|        --- (PASS|FAIL|SKIP)|ok |FAIL|panic:|PASS$|testbuild: )|_test\.go:[0-9]+:|\.go:[0-9]+:[0-9]+: |build failed|DISPATCH'

# run_label <label> [env assignment]: the document's command, go test -list first.
run_label() {
	local label=$1 envset=${2:-} spec=${CMD[$1]} to pat pkgs rest rc
	to=${spec%%|*} rest=${spec#*|}
	pkgs=${rest##*|} pat=${rest%|*}
	echo "=== CHECK-RUN $label: go test -list '$pat' $pkgs"
	(cd packages/daemon && env $envset go test -list "$pat" $pkgs 2>&1) | grep -v '^ok '
	echo "=== CHECK-RUN $label: ${envset:+$envset }go test -count=1 -v -timeout $to $pkgs -run '$pat'"
	(cd packages/daemon && env $envset go test -count=1 -v -timeout "$to" $pkgs -run "$pat" 2>&1) | grep -E -- "$keep" | cut -c1-2500
	rc=${PIPESTATUS[0]}
	echo "=== RUN-EXIT $label $rc"
}

controls=()
for arg in "$@"; do
	if [ -n "${RUNS[$arg]:-}" ]; then controls+=("$arg"); continue; fi
	found=0
	for c in $(printf '%s\n' "${!RUNS[@]}" | grep -E "^${arg}-[0-9]+\$" | sort -V); do controls+=("$c"); found=1; done
	((found)) || echo "UNKNOWN $arg"
done

if [ -n "$(dirty)" ]; then echo "TREE-NOT-CLEAN-AT-START"; dirty; exit 3; fi
echo "HEAD $(git rev-parse HEAD)"
for c in "${controls[@]}"; do
	echo "######## CONTROL $c: ${RUNS[$c]}"
	if [ -f "$here/dispatch/$c.patch" ]; then
		tree=${TMPDIR:-/tmp}/ctl-dispatch-$c
		git worktree add -q --detach "$tree" HEAD || { echo "WORKTREE-FAILED $c"; continue; }
		if ! git -C "$tree" apply "$here/dispatch/$c.patch"; then echo "APPLY-FAILED $c"; git worktree remove --force "$tree"; continue; fi
		echo "DISPATCH tree $tree is $(git -C "$tree" rev-parse HEAD) with $(git -C "$tree" diff --stat | tail -1)"
		git -C "$tree" diff | sed 's/^/DISPATCH-DIFF /'
		for label in ${RUNS[$c]}; do run_label "$label" "LEGION_E2E_REPO_ROOT=$tree"; done
		git worktree remove --force "$tree" || { echo "WORKTREE-REMOVE-FAILED $c"; exit 4; }
	else
		if ! git apply "$here/patches/$c.patch"; then echo "APPLY-FAILED $c"; continue; fi
		git diff --stat | tail -1
		git diff | sed 's/^/CONTROL-DIFF /'
		for label in ${RUNS[$c]}; do run_label "$label"; done
		git apply -R "$here/patches/$c.patch" || { echo "REVERT-FAILED $c"; exit 4; }
	fi
	if [ -n "$(dirty)" ]; then echo "TREE-NOT-CLEAN-AFTER $c"; dirty; exit 5; fi
	echo "######## END $c (tree clean)"
done
echo "RUN_SH_DONE"
