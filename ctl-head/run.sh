#!/usr/bin/env bash
# Runs, in a build-cluster pod at the control ref (the PR head 951509d8 plus this ctl-head/
# directory, which no Go package reads), each named check's negative controls: apply one control
# patch to the product code, run the check's own command (go test -list, then go test -v), print
# its pass and fail lines, revert, and prove the tree is the head's again before the next one.
# Usage: run.sh <check> [<check> ...]
set -u
cd "$(git rev-parse --show-toplevel)"
declare -A PKGS PAT
PKGS[c-doors]="./internal/workflow"; PAT[c-doors]='^(TestEachDoorToDone|TestAChildThatClosesOnItsChecksIsSuspendedOnce)$'
PKGS[c-newest-merge]="./internal/workflow"; PAT[c-newest-merge]='^(TestAReadThatNamesAnEarlierMergeDecidesNothing|TestAProductionCheckOfAnEarlierMergeDecidesNothing)$'
PKGS[c-after-not-run]="./internal/workflow"; PAT[c-after-not-run]='^(TestALateResultAfterNotRunOpensOneLookAndCloses|TestNoTimerOpensALookAfterNotRun|TestTheLookAReadOpensAfterNotRunIsOpenedOnce)$'
PKGS[c-child]="./internal/workflow ./internal/record"; PAT[c-child]='^(TestAChildsNextChangeUnderAnAcceptedParentIsNotHeld|TestAChildInVerifyingHoldsTheCloseAndNotTheSignOff|TestTheCloseWaitsForAFiledChildInBacklog|TestAHeldChildStartsAtItsRootsMerge|TestTheEchoOfANextChangesTodoReentersNothing)$'
PKGS[c-outside-cause]="./internal/workflow"; PAT[c-outside-cause]='^(TestAChangeWithNoPullRequestEndsWhenItsCheckPasses|TestAReopenedChangeUsesItsOwnProductionAssessment|TestAnOldProductionReportCannotFinishTheNewAssessment)$'
PKGS[c-change-count]="./internal/workflow"; PAT[c-change-count]='^(TestEachMultipleOfTheRoundCapTellsTheCountAndGoesOn|TestANextChangeNeverStartsWhileOneIsOpen)$'
PKGS[c-next-change-steps]="./internal/workflow"; PAT[c-next-change-steps]='^(TestANextChangeRunsTheDefaultStepsAndNotTheLastPlan|TestANextChangeRunsThePlanGivenForIt|TestANextChangeOfARootAsksNoApproval|TestANextChangeOfARootLeavesEachOtherMemberAsItIs|TestTheExperimentsSecondGroupRunsTheDefaultStepsOnItsNextChange)$'
PKGS[c-tree-fences]="./internal/record ./internal/store ./internal/workflow"; PAT[c-tree-fences]='^(TestARootsNextChangeKeepsEachMembersFences|TestAWholeTreeRerunRaisesTheTreeGeneration|TestARootsNextChangeLeavesItsArchitectsRunStanding|TestARootsCloseIsFencedOnItsOwnGeneration)$'
PKGS[c-after-close]="./internal/workflow"; PAT[c-after-close]='^TestAFailAfterACloseOnChecksTellsTheControllerOnce$'
PKGS[c-standing]="./internal/workflow"; PAT[c-standing]='^(TestTheDailyTurnNamesEachTermThatStands|TestProductionOnlyWaitIsVisibleAndReconciled)$'
PKGS[c-running-issues]="./internal/store"; PAT[c-running-issues]='^(TestTheMigrationAddsVerifyingAndKeepsEveryRow|TestAReleaseLeavesEachClosedIssueClosed|TestTheWayBackOfDoneByChecks|TestTheWayBackRunsForEachProjectInAnyOrder)$'
PKGS[c-contract]="./internal/api"; PAT[c-contract]='^TestAnOlderContractSeesMergingAndNoCloseWait$'
dirty() { git status --porcelain -- . ':!ctl-head' | grep -v '^??' ; }
if [ -n "$(dirty)" ]; then echo "TREE-NOT-CLEAN-AT-START"; dirty; exit 3; fi
echo "HEAD $(git rev-parse HEAD)"
F=0
for check in "$@"; do
  pkgs=${PKGS[$check]:-}; pat=${PAT[$check]:-}
  [ -n "$pkgs" ] || { echo "UNKNOWN-CHECK $check"; F=1; continue; }
  echo "######## LIST $check"
  (cd packages/daemon && go test -list "$pat" $pkgs) | grep -E '^Test|^ok|^FAIL|no test files' || F=1
  for p in $(ls ctl-head/patches/ | grep -E "^${check}-[0-9a-z]+\.patch$" | sort -V); do
    name=${p%.patch}
    echo "######## CONTROL $name"
    if ! git apply "ctl-head/patches/$p"; then echo "APPLY-FAILED $name"; F=1; continue; fi
    git diff --stat | tail -1
    (cd packages/daemon && go test -count=1 -v -run "$pat" $pkgs 2>&1) | grep -E -- '^--- (PASS|FAIL)|^    --- (PASS|FAIL)|^        --- (PASS|FAIL)|_test\.go:[0-9]+:|^(ok|FAIL)[[:space:]]|build failed|panic:|\.go:[0-9]+:[0-9]+: ' | cut -c1-700
    git apply -R "ctl-head/patches/$p" || { echo "REVERT-FAILED $name"; exit 4; }
    if [ -n "$(dirty)" ]; then echo "TREE-NOT-CLEAN-AFTER $name"; dirty; exit 5; fi
    echo "######## END $name (tree clean)"
  done
done
echo "RUN_SH_DONE F=$F"
