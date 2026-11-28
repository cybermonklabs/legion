#!/usr/bin/env bash
# Runs, in a build-cluster pod at the head plus this untracked ctl/ directory (which no Go package
# reads), each named check's negative controls. For each check it first runs the check's own
# command at the head (go test -list, then go test -v: the baseline, which must pass), then, for
# each control patch of that check: apply it alone to the product code, run the same command,
# print its pass and fail lines, revert, and prove the tree is the head's again before the next.
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
dirty() { git status --porcelain -- . ':!ctl' | grep -v '^??' ; }
# check runs the check's own command and prints its pass, fail, assertion, build and panic lines,
# with up to 60 continuation lines of each assertion message (a message that spans lines, such as
# a notice's text, is indented by eight spaces after its first line).
check() { # $1 pattern, rest packages
  local pat=$1; shift
  local t0=$SECONDS
  (cd packages/daemon && go test -count=1 -v -timeout 15m -run "$pat" "$@" 2>&1) |
    awk '
      /_test\.go:[0-9]+:/ { print; more = 60; next }
      more > 0 && /^        / && !/^ *--- / { print; more--; next }
      { more = 0 }
      /^=== NAME|^ *--- (PASS|FAIL|SKIP)|^(ok|FAIL)[[:space:]]|^FAIL$|build failed|panic:|\.go:[0-9]+:[0-9]+: |timed out/ { print }' | cut -c1-40000
  echo "######## SECONDS $((SECONDS - t0))"
}
if [ -n "$(dirty)" ]; then echo "TREE-NOT-CLEAN-AT-START"; dirty; exit 3; fi
echo "HEAD $(git rev-parse HEAD)"
F=0
for name in "$@"; do
  pkgs=${PKGS[$name]:-}; pat=${PAT[$name]:-}
  [ -n "$pkgs" ] || { echo "UNKNOWN-CHECK $name"; F=1; continue; }
  echo "######## LIST $name: go test -list '$pat' $pkgs"
  (cd packages/daemon && go test -list "$pat" $pkgs 2>&1) || F=1
  echo "######## BASELINE $name (no control): go test -count=1 -v -timeout 15m -run '$pat' $pkgs"
  check "$pat" $pkgs
  for p in $(ls ctl/patches/ | grep -E "^${name}-[0-9a-z]+\.patch$" | sort -V); do
    ctl=${p%.patch}
    echo "######## CONTROL $ctl"
    if ! git apply "ctl/patches/$p"; then echo "APPLY-FAILED $ctl"; F=1; continue; fi
    git diff --stat | tail -1
    check "$pat" $pkgs
    git apply -R "ctl/patches/$p" || { echo "REVERT-FAILED $ctl"; exit 4; }
    if [ -n "$(dirty)" ]; then echo "TREE-NOT-CLEAN-AFTER $ctl"; dirty; exit 5; fi
    echo "######## END $ctl (tree clean)"
  done
done
echo "RUN_SH_DONE F=$F"
