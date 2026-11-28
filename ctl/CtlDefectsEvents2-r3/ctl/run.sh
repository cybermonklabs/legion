#!/usr/bin/env bash
# Runs, in a build-cluster pod at the head (the Job's checkout of H plus this ctl/ directory,
# which no package reads), the negative controls of c-defect-call, c-events and c-no-slot: first
# each command of the check at the head with no control (the baseline), then for each control
# patch: apply it alone to the product code, run the command(s) that control names (go test -list,
# then go test -v; or the bun case), print the pass and fail lines, revert, and prove the tree is
# the head's again before the next one.
# Usage: run.sh <check> [<check> ...]   (a check is c-defect-call, c-events or c-no-slot)
set -u
cd "$(git rev-parse --show-toplevel)" || exit 2
here=$(pwd)
export GOFLAGS=-p=4
declare -A PKGS PAT
PKGS[c-defect-call.1]="./internal/api"
PAT[c-defect-call.1]='^(TestReportDefectAnswersEachCase|TestWithdrawDefectAnswersEachCase)$'
PKGS[c-defect-call.2]="./internal/workflow"
PAT[c-defect-call.2]='^(TestADefectReportedInTheRetroStartsNothingBeforeTheMerge|TestAReturnCarriesTheDefectsOfItsGeneration|TestTheMergersCompletionLandsBeforeTheNextChange)$'
PKGS[c-events]="./internal/workflow"
PAT[c-events]='^TestEachEventJudgesTheCloseAgain$'
PKGS[c-no-slot]="./internal/admit"
PAT[c-no-slot]='^TestAnIssueInVerifyingHoldsNoAdmissionSlot$'
# The commands of each check, and the command(s) each control names (default: the check's one).
declare -A COMMANDS CONTROL_CMDS
COMMANDS[c-defect-call]="c-defect-call.1 c-defect-call.2 c-defect-call.3"
COMMANDS[c-events]="c-events"
COMMANDS[c-no-slot]="c-no-slot"
CONTROL_CMDS[c-defect-call-1]="c-defect-call.2"
CONTROL_CMDS[c-defect-call-2]="c-defect-call.1"
CONTROL_CMDS[c-defect-call-3]="c-defect-call.2"
CONTROL_CMDS[c-defect-call-4]="c-defect-call.1"
CONTROL_CMDS[c-defect-call-5]="c-defect-call.2"
CONTROL_CMDS[c-defect-call-6]="c-defect-call.3"
CONTROL_CMDS[c-defect-call-7]="c-defect-call.1"

dirty() { git status --porcelain -- . ':!ctl' | grep -v '^??'; }
bun_ready=0
run_cmd() { # $1 command label
	local label=$1
	if [ "$label" = c-defect-call.3 ]; then
		if [ $bun_ready = 0 ]; then
			echo "=== bun install --frozen-lockfile (repository root)"
			if ! (cd "$here" && bun install --frozen-lockfile >"${TMPDIR:-/tmp}/bun-install.log" 2>&1); then
				tail -20 "${TMPDIR:-/tmp}/bun-install.log"; echo "=== BUN-INSTALL-FAILED"; return 1
			fi
			bun_ready=1
		fi
		echo "=== CHECK-RUN $label: bun test src/tools.test.ts -t 'report_defect moves no phase' (packages/pi-legion)"
		(cd "$here/packages/pi-legion" && bun test src/tools.test.ts -t 'report_defect moves no phase' 2>&1) | cut -c1-400
		echo "=== RUN-EXIT $label ${PIPESTATUS[0]}"
		return
	fi
	local pkgs=${PKGS[$label]} pat=${PAT[$label]}
	echo "=== CHECK-RUN $label: go test -list '$pat' $pkgs"
	(cd "$here/packages/daemon" && go test -list "$pat" $pkgs 2>&1) | grep -v '^ok ' | cut -c1-400
	echo "=== LIST-EXIT $label ${PIPESTATUS[0]}"
	echo "=== CHECK-RUN $label: go test -count=1 -v -timeout 15m $pkgs -run '$pat'"
	(cd "$here/packages/daemon" && go test -count=1 -v -timeout 15m $pkgs -run "$pat" 2>&1) |
		grep -E -- '^[[:space:]]*--- (PASS|FAIL|SKIP)|_test\.go:[0-9]+:|^(ok|FAIL)([[:space:]]|$)|build failed|panic:|\.go:[0-9]+:[0-9]+: |^PASS$' | cut -c1-700
	echo "=== RUN-EXIT $label ${PIPESTATUS[0]}"
}

if [ -n "$(dirty)" ]; then echo "TREE-NOT-CLEAN-AT-START"; dirty; exit 3; fi
echo "HEAD $(git rev-parse HEAD)"
for check in "$@"; do
	cmds=${COMMANDS[$check]:-}
	[ -n "$cmds" ] || { echo "UNKNOWN-CHECK $check"; continue; }
	for c in $cmds; do
		echo "######## BASELINE $check $c (the head, no control)"
		run_cmd "$c"
	done
	for p in $(ls ctl/patches/ | grep -E "^${check}-[0-9]+\.patch$" | sort -V); do
		name=${p%.patch}
		echo "######## CONTROL $name"
		if ! git apply "ctl/patches/$p"; then echo "APPLY-FAILED $name"; continue; fi
		git diff --stat | tail -1
		for c in ${CONTROL_CMDS[$name]:-$cmds}; do run_cmd "$c"; done
		git apply -R "ctl/patches/$p" || { echo "REVERT-FAILED $name"; exit 4; }
		if [ -n "$(dirty)" ]; then echo "TREE-NOT-CLEAN-AFTER $name"; dirty; exit 5; fi
		echo "######## END $name (tree clean)"
	done
done
echo "RUN_SH_DONE"
