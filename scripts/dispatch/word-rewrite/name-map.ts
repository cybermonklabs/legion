import { daemonContract } from "../../../packages/contracts/src/daemon-contract.ts";
import { holdsWord } from "./pattern.ts";

/**
 * The map of old names to new names: one entry per name this repository defines that Dispatch
 * text can hold, and the point from which the new name is true where Dispatch text says it.
 * The names of the other repositories come at run time from `--extra-map` (INFRA-2017's plan,
 * P7): their rows name the deployment's accounts and objects, which this public repository does
 * not hold. This file is one of the places the word may stand in the repository (R1, kind 4).
 *
 * `from` says when an entry applies to a project's text:
 *
 * - `{contract: n}`: a name the release of cybermonklabs/legion carries (a notice kind, a table,
 *   a skill's file name, a test's, a script's, a file's or an identifier's name). Its new name is
 *   true in a project whose daemon reads `contract.daemon` at or above `n`, or that the run names
 *   as having no Legion daemon (spec version 19). Every name of this repository's map but the
 *   Dispatch API names is gated so, at the contract this checkout records; `park_child` at the
 *   contract that removed it.
 * - `{dispatchHolds: "merge"}`: a name of Dispatch's API. It applies once the running Dispatch
 *   build (`/healthz` `commit`) holds the merge commit the run names.
 * - `"merged"`: kept for the rows `--extra-map` gives (the other repositories' names, each
 *   written into that file only once its condition holds), which apply now.
 */
export type From = { readonly contract: number } | { readonly dispatchHolds: "merge" } | "merged";

export interface MapEntry {
  readonly old: string;
  readonly new: string;
  /** A name a program reads (a field, a kind, a file), which the new text spells exactly; false
   *  for words of prose such as a heading. */
  readonly codeName: boolean;
  readonly from: From;
}

/** The daemon contract that removed the child operation `park_child` (cybermonklabs/legion#4573). */
export const CHILD_OPERATION_REMOVED = 205;

/** The daemon API contract number of the working tree at `root`: L plus its count of entry
 *  fixtures, as `daemonContract` of packages/contracts counts them for a checkout. */
export function readContract(root: string): number {
  return daemonContract({}, root);
}

const dispatchMerge = { dispatchHolds: "merge" } as const;

/** The names of Dispatch's API this change renames, which wait for Dispatch's build. */
const DISPATCH_API_NAMES: readonly (readonly [string, string])[] = [
  ["priority_work_parked", "priority_work_in_backlog_or_icebox"],
  ["PriorityWorkParked", "PriorityWorkInBacklogOrIcebox"],
];

/**
 * Every other old name this change renames that Dispatch text can hold, with its new name: each
 * name the pull request's diff renames (files, Go tests and functions, types, constants and
 * variables, TypeScript declarations and test titles), and each task's wire and stored names,
 * keys, variables, anchors, devbox checkpoints and log lines. Each is a name the release carries,
 * so each applies from the new contract. A file is listed by its base name, which also covers
 * every path that ends in it. A third value `false` marks words that are no code name: a line of
 * a notice or of a heading, and the controller's marks.
 */
const RELEASE_NAMES: readonly (readonly [string, string, boolean?])[] = [
  // The kept decisions: files.
  ["parked_decision.go", "kept_decision.go"],
  ["parked_decision_test.go", "kept_decision_test.go"],
  ["parked_owed_start_test.go", "backlog_owed_start_test.go"],
  // The kept decisions: wire and stored names.
  ["parked_decisions", "kept_decisions"],
  ["parked_decisions_issue", "kept_decisions_issue"],
  ["parked_decisions_pkey", "kept_decisions_pkey"],
  ["parked_decisions_issue_fkey", "kept_decisions_issue_fkey"],
  ["parked_decisions_claimed_by_fkey", "kept_decisions_claimed_by_fkey"],
  ["parked-decision", "kept-decision"],
  // The kept decisions: identifiers.
  ["ParkedDecision", "KeptDecision"],
  ["AddParkedDecision", "KeepDecision"],
  ["ParkedDecisions", "KeptDecisionsOf"],
  ["RelayParkedDecision", "RelayKeptDecision"],
  ["RelayNotParked", "RelayBackInWorkflow"],
  ["ParkedDecisionNotice", "KeptDecisionNotice"],
  ["ParkedDecisionTask", "UnclaimedKeptTask"],
  ["relayParkedDecision", "relayKeptDecision"],
  ["parkDecision", "relaySpecDecision"],
  ["parkedDecisionSource", "keptDecisionSource"],
  // The kept decisions: tests.
  [
    "TestTheMachinerysNoticeToAStoppedTreeWritesNoParkedDecision",
    "TestTheMachinerysNoticeToAStoppedTreeKeepsNoDecision",
  ],
  [
    "TestAParkedDecisionIsPublishedOnTheControllerTopic",
    "TestAKeptDecisionIsPublishedOnTheControllerTopic",
  ],
  [
    "TestAPersonsAnswerOnAParkedTreeGoesToTheController",
    "TestAPersonsAnswerOnATreeThatDoesNotRunGoesToTheController",
  ],
  [
    "TestAPersonsReplyOnAParkedChildNamesItsRoot",
    "TestAPersonsReplyOnAChildOfATreeThatDoesNotRunNamesItsRoot",
  ],
  [
    "TestAnOrphanedParkedChildTakesTheDecisionKeptOnIt",
    "TestAnOrphanedChildInBacklogTakesTheDecisionKeptOnIt",
  ],
  [
    "TestAParkedDecisionKeepsItsDecisionThroughTheRow",
    "TestAKeptDecisionKeepsItsDecisionThroughTheRow",
  ],
  [
    "TestAParkedDecisionKeepsTheNewestPerRefAndIsForgottenOnlyWhereItWasNamed",
    "TestAKeptDecisionKeepsTheNewestPerRefAndIsForgottenOnlyWhereItWasNamed",
  ],
  [
    "TestParkedDecisionsArriveWith20261101163106AndGoWithTheirIssue",
    "TestKeptDecisionsArriveWith20261101163106AndGoWithTheirIssue",
  ],
  [
    "TestAStartOwedBeforeAParkIsFinishedRatherThanCarriedAcrossIt",
    "TestAStartOwedBeforeAMoveToBacklogIsFinishedRatherThanCarriedAcrossIt",
  ],
  // The daemon's status words, the held reason and the child route.
  ["tree_parked", "tree_in_backlog_or_icebox"],
  ["/legion/v1/children/park", "/legion/v1/children/{op}"],
  ["You can park the tree.", "You can move the tree's root to backlog.", false],
  ["HeldTreeParked", "HeldTreeInBacklogOrIcebox"],
  ["releaseParkedClaim", "releaseOutOfWorkflowClaim"],
  ["releaseParkedClaims", "releaseOutOfWorkflowClaims"],
  ["parkedWakes", "delayedWakes"],
  ["parkedWakeBound", "delayedWakeBound"],
  [
    "parkedWait",
    "delayedWait (packages/daemon/internal/daemon/wake.go) or retryWait (packages/envoy/internal/dispatch/api/comment_mention_retry_test.go)",
  ],
  ["parkedLaunch", "delayedLaunch"],
  [
    "ancestry-held-past-a-parked-root-on-its-chain",
    "ancestry-held-past-a-backlog-root-on-its-chain",
  ],
  [
    "TestARootHeldPastAParkedRootOnItsChainQueuesOnceTheTreeLingers",
    "TestARootHeldPastARootInBacklogOnItsChainQueuesOnceTheTreeLingers",
  ],
  [
    "TestAListedChildHoldsItsParentAndAParkedOneDoesNot",
    "TestAListedChildHoldsItsParentAndOneInBacklogDoesNot",
  ],
  [
    "TestReconcileReentersAParkedChildSetBackToTodoInABootSummary",
    "TestReconcileReentersAChildOutOfTheWorkflowSetBackToTodoInABootSummary",
  ],
  [
    "TestAParkAndATodoWhileThePromotionsWriteIsOnItsWayRunTheTreeAgain",
    "TestABacklogAndATodoWhileThePromotionsWriteIsOnItsWayRunTheTreeAgain",
  ],
  [
    "TestControllerStatusParksTheRecordWithoutWaitingForItsDispatchEvent",
    "TestControllerStatusMovesTheRecordToBacklogWithoutWaitingForItsDispatchEvent",
  ],
  [
    "TestWaveReleaseReentersAParkedChildFromItsOwnWrite",
    "TestWaveReleaseReentersABacklogChildFromItsOwnWrite",
  ],
  [
    "TestARootHeldPastAParkedRootOnItsChainTakesNoSlot",
    "TestARootHeldPastARootInBacklogOnItsChainTakesNoSlot",
  ],
  ["TestBootReleasesParkedClaims", "TestBootReleasesOutOfWorkflowClaims"],
  [
    "TestAHookPromptWaitsOutTheParkedWakeOfItsReleasedPod",
    "TestAHookPromptWaitsOutTheDelayedWakeOfItsReleasedPod",
  ],
  [
    "TestAHookPromptFollowsAnOperatorsResumeOfItsParkedWakeWithinTheBound",
    "TestAHookPromptFollowsAnOperatorsResumeOfItsDelayedWakeWithinTheBound",
  ],
  [
    "TestARelaunchParkedOverALiveProcessHoldsNoRow",
    "TestARelaunchDelayedOverALiveProcessHoldsNoRow",
  ],
  [
    "TestAPersonsApprovalOfAParkedRootsSpecIsQueuedByTheDaemonAgainstScratchDispatch",
    "TestAPersonsApprovalOfABacklogRootsSpecIsQueuedByTheDaemonAgainstScratchDispatch",
  ],
  [
    "TestAParkedChildsCatchUpEndsAfterAPersonMovesItToARunningStatus",
    "TestABacklogChildsCatchUpEndsAfterAPersonMovesItToARunningStatus",
  ],
  [
    "TestACancelRacingAParkLeavesNoResumeAboveTheClose",
    "TestACancelRacingAMoveToBacklogLeavesNoResumeAboveTheClose",
  ],
  [
    "TestAPersonsApprovalOfAnUnrecordedParkedRootsSpecIsQueuedByTheDaemon",
    "TestAPersonsApprovalOfAnUnrecordedBacklogOrIceboxRootsSpecIsQueuedByTheDaemon",
  ],
  [
    "TestOnlyAPersonsApprovalOfAParkedLabelledRootsPrimaryDocumentQueuesIt",
    "TestOnlyAPersonsApprovalOfABacklogOrIceboxLabelledRootsPrimaryDocumentQueuesIt",
  ],
  [
    "TestAPersonsApprovalOfARecordedParkedRootsSpecQueuesItAndRecordsItQuietly",
    "TestAPersonsApprovalOfARecordedBacklogRootsSpecQueuesItAndRecordsItQuietly",
  ],
  [
    "TestACarriedCopyOfAParkedRootsGateTakesTheApprovalWhileTheOwnerIsQueued",
    "TestACarriedCopyOfABacklogRootsGateTakesTheApprovalWhileTheOwnerIsQueued",
  ],
  // The controller's skill files, the devbox proof and its lib files, the solution page.
  ["park-asks.md", "backlog-asks.md"],
  ["parked-decisions.md", "kept-decisions.md"],
  ["parked-decision-devbox.sh", "kept-decision-devbox.sh"],
  ["parked-decision-cleanup.test.ts", "kept-decision-cleanup.test.ts"],
  ["parked-decision-teardown.test.ts", "kept-decision-teardown.test.ts"],
  ["parked-decision-verdict.jq", "kept-decision-verdict.jq"],
  ["parked-decision-verdict.test.ts", "kept-decision-verdict.test.ts"],
  [
    "dispatch-blocked-by-replaces-the-whole-list-and-a-park-stops-an-issue-without-finishing-it-INFRA-225.md",
    "dispatch-blocked-by-replaces-the-whole-list-and-a-move-to-backlog-stops-an-issue-without-finishing-it-INFRA-225.md",
  ],
  // Variables.
  ["PARKED_DECISION_EVIDENCE_DIR", "KEPT_DECISION_EVIDENCE_DIR"],
  ["PARKED_DECISION_UNTIL", "KEPT_DECISION_UNTIL"],
  ["DOCS_RESIDENCY_PARK_COMMIT_TRIAL", "DOCS_RESIDENCY_WEDGED_COMMIT_TRIAL"],
  // Every other name the pull request's diff renames: files (by base name), Go tests,
  // functions, types, constants and variables, and TypeScript declarations and test titles, by
  // the directory that defines them. A name two places rename apart names both.
  // docs/research/temporal-evaluation-2026-11/prototype/internal/exp15
  ["waitParked", "waitUntilWaiting"],
  // packages/daemon/internal/admit
  ["keepParkedDecision", "keepDecision"],
  ["parkedDecisionNotices", "keptDecisionNotices"],
  ["parkedRoot", "rootInBacklog"],
  ["parkedTree", "backlogTree"],
  ["parkFinishingTree", "backlogFinishingTree"],
  // packages/daemon/internal/api
  ["refuseParkedRelaunch", "refuseDelayedRelaunch"],
  [
    "TestARecoveryStepThatMeetsAParkRaisedAfterItsViewNamesTheLaunchInFlight",
    "TestARecoveryStepThatMeetsADelayRaisedAfterItsViewNamesTheLaunchInFlight",
  ],
  ["parkedRelaunchCause", "delayedRelaunchCause"],
  ["parkedRelaunchCauses", "delayedRelaunchCauses"],
  ["parkedRelaunchRefusal", "delayedRelaunchRefusal"],
  ["suspensionHeldOverTheParkedRelaunch", "suspensionHeldOverTheDelayedRelaunch"],
  ["workingUnderAParkedRefresh", "workingUnderADelayedRefresh"],
  ["workingUnderAParkedRelaunch", "workingUnderADelayedRelaunch"],
  // packages/daemon/internal/daemon
  ["parkedIn", "backlogIn"],
  ["bootParked", "bootOutOfWorkflow"],
  ["testBootQueuesTheParkedClaims", "testBootQueuesTheOutOfWorkflowClaims"],
  ["ancestryHeldPastAParkedRootOnItsChain", "ancestryHeldPastABacklogRootOnItsChain"],
  ["parkTheRootBeforeTheChildsRowsRun", "moveTheRootToBacklogBeforeTheChildsRowsRun"],
  ["keptAfterTheParkedWake", "keptAfterTheDelayedWake"],
  ["parkFound", "delayFound"],
  ["parkTheWake", "delayTheWake"],
  ["parkedOverItsProcess", "delayedOverItsProcess"],
  ["parkedTurn", "delayedTurn"],
  ["parkedWake", "delayedWake"],
  ["keptParkClosed", "keptLeaveClosed"],
  ["parkedOrWaiting", "outOfWorkflowOrWaiting"],
  ["parkedIssue", "outOfWorkflowIssue"],
  ["parkRelaunch", "delayRelaunch"],
  ["parkedBehindALoad", "waitingBehindALoad"],
  ["parkPauseHeldLaunch", "delayPauseHeldLaunch"],
  ["parkedLaunchKeepsThePod", "delayedLaunchKeepsThePod"],
  ["runParkedLaunch", "runDelayedLaunch"],
  ["wokenParked", "wokenDelayed"],
  ["parkingPace", "delayPace"],
  ["parkedWakeLetOut", "delayedWakeLetOut"],
  ["wantLaunchParked", "wantLaunchDelayed"],
  [
    "TestTheStallBackstopTellsAParkedRelaunchAsWhatParkTimeSettles",
    "TestTheStallBackstopTellsADelayedRelaunchAsWhatTheDelaySettles",
  ],
  // packages/daemon/internal/record
  ["parkedDecisions", "keptDecisionsOf"],
  // packages/daemon/internal/supervise
  [
    "TestAParkedAgentWithNothingPendingIsRelaunchedHoweverOftenItDies",
    "TestAnIdleAgentWithNothingPendingIsRelaunchedHoweverOftenItDies",
  ],
  [
    "TestACompactionUnderAParkedRepointGivesTheClaimBackAndLeavesTheTaskToThatTurn",
    "TestACompactionUnderADelayedRepointGivesTheClaimBackAndLeavesTheTaskToThatTurn",
  ],
  [
    "TestAParkedRelaunchRefusedAtItsTurnRecordsThatItsProcessIsGone",
    "TestADelayedRelaunchRefusedAtItsTurnRecordsThatItsProcessIsGone",
  ],
  [
    "TestASuspendIsHeldForACompactionOrAParkedLaunchOverADeadProcess",
    "TestASuspendIsHeldForACompactionOrADelayedLaunchOverADeadProcess",
  ],
  [
    "TestReadProcessAsksNothingOfAClaimWithNoProcessOrADeadProcessAParkedLaunchReplaces",
    "TestReadProcessAsksNothingOfAClaimWithNoProcessOrADeadProcessADelayedLaunchReplaces",
  ],
  ["TestARelaunchRecordsAtAParkedWakesTurn", "TestARelaunchRecordsAtADelayedWakesTurn"],
  [
    "TestARelaunchRecordsNoTurnAtAParkedWakesTurnAfterACompactionLostItsProcess",
    "TestARelaunchRecordsNoTurnAtADelayedWakesTurnAfterACompactionLostItsProcess",
  ],
  [
    "TestAnOperatorsDeliveryOverTheRefreshsParkedRelaunchRunsItNow",
    "TestAnOperatorsDeliveryOverTheRefreshsDelayedRelaunchRunsItNow",
  ],
  ["refreshParked", "refreshDelayed"],
  ["refreshParkedAt", "refreshDelayedAt"],
  ["refreshParkedIdle", "refreshDelayedIdle"],
  ["endPodPark", "endPodStartWait"],
  ["aRelaunchThatWaitsStopsAtThePark", "aRelaunchThatWaitsStopsAtTheGate"],
  ["aResumeParkedOverARefusedStop", "aResumeWaitingOverARefusedStop"],
  ["parkedPromptly", "waitingForPodPromptly"],
  ["EndRelaunchParked", "EndRelaunchDelayed"],
  [
    "TestTheBackstopTellsTheOwnerOfAReplacementThePacingParkedOnce",
    "TestTheBackstopTellsTheOwnerOfAReplacementThePacingDelayedOnce",
  ],
  ["parkWake", "delayWake"],
  ["reparkWake", "delayWakeAgain"],
  ["runParkedLaunchNow", "runDelayedLaunchNow"],
  [
    "TestADeliveryTheTableIgnoresRunsNoParkedLaunch",
    "TestADeliveryTheTableIgnoresRunsNoDelayedLaunch",
  ],
  [
    "TestAProcesslessWakeParksTheClaimLaunchingAndWritesIt",
    "TestADelayedProcesslessWakeWritesTheClaimLaunching",
  ],
  [
    "TestAnOperatorsDeliveryOverAHeldSuspensionLeavesTheLaunchParked",
    "TestAnOperatorsDeliveryOverAHeldSuspensionLeavesTheLaunchDelayed",
  ],
  [
    "TestAnOperatorsDeliveryOverAParkedLaunchArmsTheNextProcessesTimers",
    "TestAnOperatorsDeliveryOverADelayedLaunchArmsTheNextProcessesTimers",
  ],
  [
    "TestAnOperatorsDeliveryOverAParkedLaunchRunsThatLaunchNow",
    "TestAnOperatorsDeliveryOverADelayedLaunchRunsThatLaunchNow",
  ],
  [
    "TestAnOperatorsDeliveryOverAParkedRelaunchNeverReachesTheDeadProcess",
    "TestAnOperatorsDeliveryOverADelayedRelaunchNeverReachesTheDeadProcess",
  ],
  [
    "TestAnOperatorsDeliveryReplacingTheLaddersTaskRunsTheParkedLaunchNow",
    "TestAnOperatorsDeliveryReplacingTheLaddersTaskRunsTheDelayedLaunchNow",
  ],
  [
    "TestAnOperatorsDeliveryTheClaimRefusesRunsNoParkedRelaunch",
    "TestAnOperatorsDeliveryTheClaimRefusesRunsNoDelayedRelaunch",
  ],
  [
    "TestAnOperatorsRetryRefusedByTheParkedClaimLeavesItsWakeItsTurn",
    "TestAnOperatorsRetryRefusedByTheDelayedClaimLeavesItsWakeItsTurn",
  ],
  [
    "TestAnOperatorsResumeRefusedByTheParkedClaimLeavesItsWakeItsTurn",
    "TestAnOperatorsResumeRefusedByTheDelayedClaimLeavesItsWakeItsTurn",
  ],
  [
    "TestTheBootsReplayMovesARelaunchItsReportParkedLaterToTheReplaysTurn",
    "TestTheBootsReplayMovesARelaunchItsReportDelayedLaterToTheReplaysTurn",
  ],
  [
    "TestAParkedIssuesIdleClaimIsStoppedSoItsPodIsNotHeldTreeNotLive",
    "TestABacklogIssuesIdleClaimIsStoppedSoItsPodIsNotHeldTreeNotLive",
  ],
  // packages/daemon/internal/workflow
  ["parkedRootView", "labelledRootView"],
  ["handsOverParkedAtTheMerge", "handsOverInBacklogAtTheMerge"],
  // packages/dispatch/e2e
  [
    "a read mark moving under a reader parked in a Conversation gap keeps the lower card fixed",
    "a read mark moving under a reader scrolled to a Conversation gap keeps the lower card fixed",
  ],
  ["PARKED_RULE", "BACKLOG_RULE"],
  ["PARKED_TABLE", "BACKLOG_TABLE"],
  ["parkedTable", "backlogTable"],
  // packages/dispatch/web/src/__tests__
  [
    "a backwards system-clock step does not park the whole-cache throttle",
    "a backwards system-clock step does not stall the whole-cache throttle",
  ],
  // packages/dispatch/web/src/features/conversation
  ["parkedNotice", "putAwayNotice"],
  // packages/dispatch/web/src/features/doc
  [
    "a restore that parks on a scratch document is dropped, reported, and leaves the live document alone",
    "a restore that leaves structs pending on a scratch document is dropped, reported, and leaves the live document alone",
  ],
  ["addsParking", "addsPending"],
  // packages/dispatch/web/src/features/priorities
  [
    "a row counts the work in flight, waiting, parked and closed in the last 7 days",
    "a row counts the work in flight, waiting, in backlog or icebox and closed in the last 7 days",
  ],
  [
    "each fault table asks the issue listing for its statuses, and the parked one for the move",
    "each fault table asks the issue listing for its statuses, and the backlog-or-icebox one for the move",
  ],
  ["PARKED_LAYOUT", "BACKLOG_LAYOUT"],
  ["PARKED_STATUSES", "BACKLOG_STATUSES"],
  // packages/envoy/cmd/dispatch-seed
  ["priorityParkedStatuses", "priorityBacklogStatuses"],
  // packages/envoy/cmd/dispatch
  ["awaitLiveWriteParked", "awaitLiveWritePaused"],
  // packages/envoy/internal/cipher/helper
  ["parkLocked", "keepDormantLocked"],
  [
    "TestANamedRecordWithNoEnrollmentIsStillParked",
    "TestANamedRecordWithNoEnrollmentIsStillKeptDormant",
  ],
  ["parkedRecord", "writeDormantRecord"],
  // packages/envoy/internal/dispatch/api
  ["nextPark", "nextRetry"],
  ["parkLockSite", "retryLockSite"],
  [
    "TestAParkedSendKeepsTheRefreshsClaimWhenARefreshLandsBetweenItsTryAndItsPark",
    "TestAWaitingSendKeepsTheRefreshsClaimWhenARefreshLandsBetweenItsTryAndItsRetrySchedule",
  ],
  [
    "TestARefreshDuringAParkedSendsTryLeavesTheSenderHoldingTheRowsClaim",
    "TestARefreshDuringAWaitingSendsTryLeavesTheSenderHoldingTheRowsClaim",
  ],
  ["newParkGate", "newRetryGate"],
  ["parkGate", "retryGate"],
  [
    "TestAParkedLaneHoldsNoGoroutineAndKeepsTheLanesBounds",
    "TestALaneWaitingForItsRetryHoldsNoGoroutineAndKeepsTheLanesBounds",
  ],
  [
    "TestManyParkedLanesTryAtMostOncePerRetryMax",
    "TestManyLanesWaitingForTheirRetryTryAtMostOncePerRetryMax",
  ],
  ["awaitPark", "awaitRetry"],
  ["comment_mention_park_test.go", "comment_mention_retry_test.go"],
  ["countParks", "countRetries"],
  ["newParkRig", "newRetryRig"],
  ["parkLiveness", "retryLiveness"],
  ["parkRig", "retryRig"],
  ["parkedFirstWait", "retryFirstWait"],
  ["parkedReadEvery", "retryReadEvery"],
  ["parkedTries", "pacedTries"],
  [
    "unpark",
    "retryLane (packages/envoy/internal/dispatch/api/comment_mention_sender.go) or takeUpAwaiting (packages/envoy/internal/dispatch/outbox/batch.go)",
  ],
  [
    "TestAParkedLanesTimerStartsOnceNoGoroutineServesIt",
    "TestALanesRetryTimerStartsOnceNoGoroutineServesIt",
  ],
  ["TestAReparkedHeldLaneHasOneGoroutine", "TestAHeldLaneScheduledForRetryAgainHasOneGoroutine"],
  [
    "TestASendParkedForAFailedClaimReadIsNoHeldCopy",
    "TestASendRetriedForAFailedClaimReadIsNoHeldCopy",
  ],
  [
    "TestASendParkedForAFailedClaimReadLogsNoHeldWarnAsItGoes",
    "TestASendRetriedForAFailedClaimReadLogsNoHeldWarnAsItGoes",
  ],
  ["liveWriteParkedWait", "liveWritePausedWait"],
  [
    "TestTheListingReadsTheMoveOnlyWhenAskedAndOnlyForParkedRows",
    "TestTheListingReadsTheMoveOnlyWhenAskedAndOnlyForBacklogRows",
  ],
  [
    "parkedRow",
    "backlogRow (packages/envoy/internal/dispatch/api/priorities_read_test.go) or pendingRow (packages/envoy/internal/dispatch/docs/storedorder.go)",
  ],
  ["parkedMoveLateral", "backlogMoveLateral"],
  ["parkedQuery", "backlogQuery"],
  ["parkedSummaryQuery", "backlogSummaryQuery"],
  ["priorityParkedQuery", "priorityBacklogQuery"],
  // packages/envoy/internal/dispatch/docs
  [
    "TestTheRoomsRenderedReplicaHoldsWhatACommitParked",
    "TestTheRoomsRenderedReplicaHoldsWhatACommitLeftPending",
  ],
  [
    "TestALoadAndACompactionKeepWhatTheDocumentParked",
    "TestALoadAndACompactionKeepWhatTheDocumentHoldsPending",
  ],
  ["parkedHistory", "pendingHistory"],
  ["requireParkedArrive", "requirePendingArrive"],
  [
    "TestAPeerUpdateThatChangesTheRoomAndParksKeepsBothAcrossARestart",
    "TestAPeerUpdateThatChangesTheRoomAndLeavesSomePendingKeepsBothAcrossARestart",
  ],
  [
    "TestARoomsParkedOnlyCommitIsStoredAndReplaysFromItsLog",
    "TestARoomsPendingOnlyCommitIsStoredAndReplaysFromItsLog",
  ],
  ["parkedOnlyCommitSurvivesReload", "pendingOnlyCommitSurvivesReload"],
  ["awaitCloseParked", "awaitClosePaused"],
  ["awaitRefusedAppendCloseParked", "awaitRefusedAppendClosePaused"],
  ["awaitSocketParked", "awaitSocketPaused"],
  ["closeIssueBehindAParkedClose", "closeIssueBehindAPausedClose"],
  ["parkCloseAfterRoomClosed", "pauseCloseAfterRoomClosed"],
  ["parkCloseBetweenItsSnapshotReads", "pauseCloseBetweenItsSnapshotReads"],
  ["parkRefusedAppendClose", "pauseRefusedAppendClose"],
  ["parkSocketAfterItsIssueRead", "pauseSocketAfterItsIssueRead"],
  ["AppendParkedUpdate", "AppendPendingBytes"],
  [
    "TestAPeerApplyErrorStillStoresWhatTheRoomParked",
    "TestAPeerApplyErrorStillStoresWhatTheRoomLeftPending",
  ],
  ["parkedErrorStore", "pendingBytesRecorder"],
  ["peer_parked_error_test.go", "peer_pending_error_test.go"],
  [
    "TestTheStoredRowsOfARoomFoldToItsDocumentWhenAPeerUpdateParks",
    "TestTheStoredRowsOfARoomFoldToItsDocumentWhenAPeerUpdateIsLeftPending",
  ],
  ["parked_peer_update_test.go", "pending_peer_update_test.go"],
  [
    "TestRenderReuseAReplicaRebuiltFromTheLogHoldsWhatTheRoomParked",
    "TestRenderReuseAReplicaRebuiltFromTheLogHoldsWhatTheRoomHeldPending",
  ],
  [
    "TestTheRenderShadowReadsClassesForAParkedOnlyCommit",
    "TestTheRenderShadowReadsClassesForAPendingOnlyCommit",
  ],
  ["parkedCommitEnv", "wedgedCommitEnv"],
  ["parkedCommitTrial", "wedgedCommitTrial"],
  ["parkedBytes", "pendingCommit"],
  ["addedParkedClocks", "addedPendingClocks"],
  ["compareParkedPrefix", "comparePendingPrefix"],
  ["judgeParkedRows", "judgePendingRows"],
  [
    "TestBuildForkPreservesOverlappingParkedUpdates",
    "TestBuildForkPreservesOverlappingPendingUpdates",
  ],
  [
    "TestStoredOrderReplayWithOverlappingParkedUpdates",
    "TestStoredOrderReplayWithOverlappingPendingUpdates",
  ],
  ["TestStoredOrderComparesParkedContent", "TestStoredOrderComparesPendingContent"],
  [
    "TestStoredOrderReusesThePrefixBeforeParkedRows",
    "TestStoredOrderReusesThePrefixBeforePendingRows",
  ],
  ["AwaitLiveWriteParked", "AwaitLiveWritePaused"],
  ["parkLiveWrite", "pauseLiveWrite"],
  ["storeParked", "storePendingBytes"],
  // packages/envoy/internal/dispatch/legion/metrics
  ["TestNoQueueOrParkedHourEntersTheWorkTime", "TestNoQueueOrBacklogHourEntersTheWorkTime"],
  ["TestTimeInIceboxIsParkedAndOutOfTheWorkTime", "TestTimeInIceboxIsOutOfTheWorkTime"],
  ["parkedSpans", "backlogSpans"],
  ["parkedStatuses", "backlogStatuses"],
  // packages/envoy/internal/dispatch/model
  ["IssueStatusesParked", "IssueStatusesBacklogOrIcebox"],
  // packages/envoy/internal/store
  [
    "TestTheRoleClaimReaperAnswersAtItsContextsEndWhileItsDeleteOfAKeyItCannotReadIsParked",
    "TestTheRoleClaimReaperAnswersAtItsContextsEndWhileItsDeleteOfAKeyItCannotReadIsBlocked",
  ],
  // packages/envoy/internal/testnats
  ["AwaitParked", "AwaitStopped"],
  // scripts/approval-eval/__tests__
  [
    "hands the rule a parked root it holds, read either way",
    "hands the rule a root in backlog that it holds, read either way",
  ],
  // scripts/e2e/lib
  ["notParked", "backInWorkflow"],
  // Each task's wire and stored names, keys, variables, anchors, devbox checkpoints and lines of
  // text that the names above do not hold.
  ["parked_decisions_issue_not_null", "kept_decisions_issue_not_null"],
  ["parked_decisions_ref_not_null", "kept_decisions_ref_not_null"],
  ["parked_decisions_decision_not_null", "kept_decisions_decision_not_null"],
  ["parked_decisions_actor_not_null", "kept_decisions_actor_not_null"],
  ["parked_decisions_at_not_null", "kept_decisions_at_not_null"],
  [
    "TestTheRemovedChildRouteAnswersWithTheStatusWrite",
    "TestAChildRouteThisDaemonDoesNotServeAnswersWithTheStatusToWrite",
  ],
  ["Its tree is parked:", "Its tree's root", false],
  ["relaunch parked", "relaunch delayed"],
  ["what the park settled", "what the delay settled", false],
  ["the launch the park queued", "the launch the delay queued", false],
  ["a relaunch the wake pacing parked", "a relaunch the wake pacing delayed", false],
  ["podPark", "podStartWait"],
  ["wake.park", "wake.podStartWait"],
  ["parked_items", "pending_items"],
  ["parked_delete_ranges", "pending_delete_ranges"],
  ["ParkedItems", "PendingItems"],
  ["ParkedDeleteRanges", "PendingDeleteRanges"],
  ["mentionSender.park", "mentionSender.scheduleRetry"],
  ["afterObservationParked", "afterObservationJoined"],
  ["Registry.Park", "Registry.KeepDormant"],
  ["parked-decision-devboxsh", "kept-decision-devboxsh"],
  ["trees-parked", "trees-in-backlog"],
  ["question-stays-parked", "question-stays-in-backlog"],
  ["foreign-park-stays", "foreign-backlog-stays"],
  ["parkedLegionRoots", "legionRoots"],
  ["ParkedHours", "BacklogOrIceboxHours"],
  ["parked_hours", "backlog_or_icebox_hours"],
  ["dispatch.project-table.priorities-parked", "dispatch.project-table.priorities-backlog"],
  ["parkedByPriority", "backlogByPriority"],
  ["Priority work parked", "Priority work in backlog or icebox", false],
  // The daemon's log lines of the kept decisions, which the devbox proof's jq reads.
  ["outbox parked decision sent to the controller", "outbox kept decision sent to the controller"],
  [
    "outbox parked decision handled before under this key",
    "outbox kept decision handled before under this key",
  ],
  [
    "outbox parked decision kept for its root's start, which waits for its slot",
    "outbox kept decision left for its root's start, which waits for its slot",
  ],
  [
    "outbox parked decision not relayed: it names no person",
    "outbox kept decision not relayed: it names no person",
  ],
  [
    "outbox parked decision not relayed: its root is not parked",
    "outbox kept decision not relayed: its root is back in the workflow",
  ],
  [
    "outbox parked decision not relayed: its tree hears it",
    "outbox kept decision not relayed: its tree hears it",
  ],
  [
    "outbox parked decision not relayed: its root has no record",
    "outbox kept decision not relayed: its root has no record",
  ],
  // The controller's marks in Dispatch (plan L5): lines it writes, no code name.
  ["Parked by Legion while it waits on:", "Moved to backlog by Legion while it waits on:", false],
  ["Parked decision read:", "Kept decision read:", false],
  ["Asked whether to park:", "Asked whether to move to backlog:", false],
  ["Park it", "Move it to backlog", false],
  ["Parked dispatch:", "Moved to backlog dispatch:", false],
  ["Not parked dispatch:", "Not moved dispatch:", false],
];

/** The map this repository holds, with `contract` the daemon API contract of this checkout. */
export function builtInMap(contract: number): readonly MapEntry[] {
  const fromContract = { contract } as const;
  return [
    {
      old: "park_child",
      new: "the owning architect's `backlog` status write on the child in Dispatch",
      codeName: true,
      from: { contract: CHILD_OPERATION_REMOVED },
    },
    ...DISPATCH_API_NAMES.map(([old, renamed]) => ({
      old,
      new: renamed,
      codeName: true,
      from: dispatchMerge,
    })),
    ...RELEASE_NAMES.map(([old, renamed, codeName = true]) => ({
      old,
      new: renamed,
      codeName,
      from: fromContract,
    })),
  ];
}

/** A line of an `--extra-map` file that is not an entry. */
export class MapFileError extends Error {
  override readonly name = "MapFileError";

  constructor(
    readonly line: number,
    reason: string
  ) {
    super(`--extra-map line ${line}: ${reason}`);
  }
}

const merged = "merged" as const;

/** Why a line is not an entry; `parseExtraMap` names the line. */
class NotAnEntry extends Error {}

function notAnEntry(reason: string): never {
  throw new NotAnEntry(reason);
}

function fromOf(value: unknown): From {
  if (value === "merged") return merged;
  if (typeof value !== "object" || value === null) notAnEntry('from is not "merged" or an object');
  if (Object.keys(value).length !== 1) notAnEntry("from names more than one rule");
  if ("contract" in value) {
    const contract = value.contract;
    if (typeof contract !== "number" || !Number.isInteger(contract) || contract < 1) {
      notAnEntry("from.contract is not a positive integer");
    }
    return { contract };
  }
  if ("dispatchHolds" in value && value.dispatchHolds === "merge") return dispatchMerge;
  return notAnEntry('from is none of {"contract": n}, {"dispatchHolds": "merge"} and "merged"');
}

function entryOf(value: unknown): MapEntry {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    notAnEntry("the line is not a JSON object");
  }
  const unknownKey = Object.keys(value).find(
    (key) => !["old", "new", "codeName", "from"].includes(key)
  );
  if (unknownKey !== undefined) notAnEntry(`unknown key ${JSON.stringify(unknownKey)}`);
  const old = "old" in value ? value.old : undefined;
  const renamed = "new" in value ? value.new : undefined;
  const codeName = "codeName" in value ? value.codeName : undefined;
  if (typeof old !== "string" || old.trim() === "") notAnEntry("old is not a name");
  if (typeof renamed !== "string" || renamed.trim() === "") notAnEntry("new is not a name");
  if (typeof codeName !== "boolean") notAnEntry("codeName is not true or false");
  return { old, new: renamed, codeName, from: fromOf("from" in value ? value.from : undefined) };
}

/**
 * The entries of an `--extra-map` file: JSON Lines, one entry
 * `{"old": …, "new": …, "codeName": true|false, "from": …}` per line, blank lines skipped. A
 * line that is not an entry refuses the whole file, naming the line, so no run reads half a map.
 */
export function parseExtraMap(text: string): MapEntry[] {
  const entries: MapEntry[] = [];
  const lines = text.split("\n");
  for (const [index, raw] of lines.entries()) {
    const line = raw.trim();
    if (line === "") continue;
    let value: unknown;
    try {
      value = JSON.parse(line);
    } catch {
      throw new MapFileError(index + 1, "the line is not JSON");
    }
    try {
      entries.push(entryOf(value));
    } catch (error) {
      if (error instanceof NotAnEntry) throw new MapFileError(index + 1, error.message);
      throw error;
    }
  }
  return entries;
}

/** What a project has reached, as the run read it: whether its daemon reads at or above a
 *  contract (or the run names it as having none), and whether Dispatch's build holds the merge. */
export interface Reach {
  daemonAtLeast(contract: number): boolean;
  readonly dispatchHoldsMerge: boolean;
}

/** Whether an entry's new name is true for a project's text now. */
export function applies(entry: MapEntry, reach: Reach): boolean {
  if (entry.from === "merged") return true;
  if ("contract" in entry.from) return reach.daemonAtLeast(entry.from.contract);
  return reach.dispatchHoldsMerge;
}

const NAME_CHARACTER = /[A-Za-z0-9_]/;

/** Whether `name` stands at `at` in `text` as a whole name: the characters on either side of it
 *  are not letters, digits or `_`. */
function wholeNameAt(text: string, name: string, at: number): boolean {
  return (
    text.startsWith(name, at) &&
    !NAME_CHARACTER.test(text.charAt(at - 1)) &&
    !NAME_CHARACTER.test(text.charAt(at + name.length))
  );
}

/**
 * The entry whose old name covers the text from `start` to `end`, the longest when more than one
 * does, or null. An old name counts only where it is a whole name, so `park_child` inside
 * `park_children` names nothing. Each entry is looked for only where it can cover the text: from
 * `end` less its length up to `start`.
 */
export function nameAt(
  text: string,
  start: number,
  end: number,
  entries: readonly MapEntry[]
): MapEntry | null {
  let best: MapEntry | null = null;
  for (const entry of entries) {
    if (best !== null && entry.old.length <= best.old.length) continue;
    for (let at = Math.max(0, end - entry.old.length); at <= start; at += 1) {
      if (wholeNameAt(text, entry.old, at)) {
        best = entry;
        break;
      }
    }
  }
  return best;
}

/** A Go test's name: `Test` and a name that goes on with a capital, a digit or `_`. */
const TEST_NAME = /\bTest[A-Z0-9_][A-Za-z0-9_]*/g;

/** A Go test's name in a text, and where it stands. */
export interface TestName {
  readonly name: string;
  readonly start: number;
  readonly end: number;
}

/** Every Go test's name in `text` that holds the word, read once per text. */
export function testNamesWithWord(text: string): TestName[] {
  return [...text.matchAll(TEST_NAME)]
    .filter((match) => holdsWord(match[0]))
    .map((match) => ({ name: match[0], start: match.index, end: match.index + match[0].length }));
}

/**
 * The Go test's name among `names` that covers the text from `start` to `end`, or null. The map
 * lists the tests this change renames; a test's name that holds the word and that no row names
 * has no new name a program can tell, so the tool keeps it (`code-name-not-yet`).
 */
export function testNameAt(names: readonly TestName[], start: number, end: number): string | null {
  return names.find((test) => test.start <= start && end <= test.end)?.name ?? null;
}

/** Every old name of the map that `text` holds as a whole name, each once, sorted. */
export function renamedNamesIn(text: string, entries: readonly MapEntry[]): string[] {
  const named = new Set<string>();
  for (const entry of entries) {
    for (let at = text.indexOf(entry.old); at !== -1; at = text.indexOf(entry.old, at + 1)) {
      if (wholeNameAt(text, entry.old, at)) {
        named.add(entry.old);
        break;
      }
    }
  }
  return [...named].sort();
}
