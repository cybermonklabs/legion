package texts

// This file is the one list. It holds the kind constants and the list of entries, and nothing
// else. A script reads each quoted lower-case word of this file as a kind. So the file has no
// import, no struct tag, and no quoted word that is not the name of a kind. The entry type, the
// forms, the readers, the urgencies and the rules are in the other files of the package.

// The tasks: the prompts that the daemon sends a session.
const (
	PhaseAssignment Kind = "phase-assignment"
	PhaseResume     Kind = "phase-resume"
	RootResume      Kind = "root-resume"
	CheckLook       Kind = "check-look"
	ArchitectStart  Kind = "architect-start"
	KeptDecisions   Kind = "kept-decisions"
	PodWake         Kind = "pod-wake"
	ControllerStart Kind = "controller-start"
)

// The parts that a start puts before a task. Each is a task too.
const (
	RelaunchNote         Kind = "relaunch-note"
	CutTurnNote          Kind = "cut-turn-note"
	AbandonedSessionNote Kind = "abandoned-session-note"
	OutOfMemoryNote      Kind = "out-of-memory-note"
	ProviderErrorNote    Kind = "provider-error-note"
)

// The notices to the architect that owns an issue.
const (
	PhaseFinished           Kind = "phase-finished"
	WorkerDied              Kind = "worker-died"
	WorkerStalled           Kind = "worker-stalled"
	WorkerSilent            Kind = "worker-silent"
	WorkerBackgroundStalled Kind = "worker-background-stalled"
	Held                    Kind = "held"
	PRBlocked               Kind = "pr-blocked"
	PRMerged                Kind = "pr-merged"
	PRClosedUnmerged        Kind = "pr-closed-unmerged"
	PullRequestMissing      Kind = "pull-request-missing"
	MessageRefused          Kind = "message-refused"
	DesignApproved          Kind = "design-approved"
	DesignChangesRequested  Kind = "design-changes-requested"
	CarriedGateOpened       Kind = "carried-gate-opened"
	ChildClosed             Kind = "child-closed"
	ChildStatus             Kind = "child-status"
	ChildrenFirst           Kind = "children-first"
	ChildReleaseStalled     Kind = "child-release-stalled"
	ChildNotRecorded        Kind = "child-not-recorded"
	CatchUp                 Kind = "catch-up"
	ChecksRed               Kind = "checks-red"
	ReviewStuck             Kind = "review-stuck"
	StatusReasserted        Kind = "status-reasserted"
	VolumeNearFull          Kind = "volume-near-full"
	SessionLost             Kind = "session-lost"
	SessionForgotten        Kind = "session-forgotten"
	SessionStoreFault       Kind = "session-store-fault"
)

// The notices to the controller.
const (
	Triage           Kind = "triage"
	SlotFree         Kind = "slot-free"
	Todo             Kind = "todo"
	Tick             Kind = "tick"
	Unslotted        Kind = "unslotted"
	AwaitingPerson   Kind = "awaiting-person"
	ParkedDecision   Kind = "parked-decision"
	UpdateStalled    Kind = "update-stalled"
	ProviderErrors   Kind = "provider-errors"
	TreeStalled      Kind = "tree-stalled"
	ArchitectStalled Kind = "architect-stalled"
	ProjectPaused    Kind = "project-paused"
	ProjectResumed   Kind = "project-resumed"
)

// The role messages: the messages that the daemon publishes to a role.
const (
	CISettled          Kind = "ci-settled"
	GateMoved          Kind = "gate-moved"
	ApprovedRoundStuck Kind = "approved-round-stuck"
	MemoryKill         Kind = "memory-kill"
	HookPrompt         Kind = "hook-prompt"
	UpdateAsk          Kind = "update-ask"
	MergeQueueRemoval  Kind = "merge-queue-removal"
	MergeForwardAsk    Kind = "merge-forward-ask"
	MergeForwardHold   Kind = "merge-forward-hold"
	MergeForwardLanded Kind = "merge-forward-landed"
	MergeForwardReview Kind = "merge-forward-review"
)

// list is the one list: one entry for each kind, in the order of the constants above.
var list = []Entry{
	{Kind: PhaseAssignment, Form: Task, Record: PhaseAssignmentRecord{}, Readers: toPhaseWorkers},
	{Kind: PhaseResume, Form: Task, Record: PhaseResumeRecord{}, Readers: toPhaseWorkers},
	{Kind: RootResume, Form: Task, Record: RootResumeRecord{}, Readers: toArchitect},
	{Kind: CheckLook, Form: Task, Record: CheckLookRecord{}, Readers: toCheckRoles},
	{Kind: ArchitectStart, Form: Task, Record: ArchitectStartRecord{}, Readers: toArchitect},
	{Kind: KeptDecisions, Form: Task, Record: KeptDecisionsRecord{}, Readers: toArchitect},
	{Kind: PodWake, Form: Task, Record: PodWakeRecord{}, Readers: toPodRoles},
	{Kind: ControllerStart, Form: Task, Record: ControllerStartRecord{}, Readers: toController},

	{Kind: RelaunchNote, Form: Task, Record: RelaunchNoteRecord{}, Readers: toEveryRole},
	{Kind: CutTurnNote, Form: Task, Record: CutTurnNoteRecord{}, Readers: toEveryRole},
	{Kind: AbandonedSessionNote, Form: Task, Record: AbandonedSessionNoteRecord{}, Readers: toEveryRole},
	{Kind: OutOfMemoryNote, Form: Task, Record: OutOfMemoryNoteRecord{}, Readers: toPodRoles},
	{Kind: ProviderErrorNote, Form: Task, Record: ProviderErrorNoteRecord{}, Readers: toEveryRole},

	{Kind: PhaseFinished, Form: Notice, Record: PhaseFinishedRecord{}, Readers: toArchitect, Urgency: phaseFinishedUrgency},
	{Kind: WorkerDied, Form: Notice, Record: WorkerDiedRecord{}, Readers: toArchitectAndController},
	{Kind: WorkerStalled, Form: Notice, Record: WorkerStalledRecord{}, Readers: toArchitectAndController},
	{Kind: WorkerSilent, Form: Notice, Record: WorkerSilentRecord{}, Readers: toArchitect},
	{Kind: WorkerBackgroundStalled, Form: Notice, Record: WorkerBackgroundStalledRecord{}, Readers: toArchitect},
	{Kind: Held, Form: Notice, Record: HeldRecord{}, Readers: toArchitectAndController, Urgency: heldUrgency},
	{Kind: PRBlocked, Form: Notice, Record: PRBlockedRecord{}, Readers: toArchitect},
	{Kind: PRMerged, Form: Notice, Record: PRMergedRecord{}, Readers: toArchitect, Urgency: lowToArchitect},
	{Kind: PRClosedUnmerged, Form: Notice, Record: PRClosedUnmergedRecord{}, Readers: toArchitect},
	{Kind: PullRequestMissing, Form: Notice, Record: PullRequestMissingRecord{}, Readers: toArchitect},
	{Kind: MessageRefused, Form: Notice, Record: MessageRefusedRecord{}, Readers: toArchitect},
	{Kind: DesignApproved, Form: Notice, Record: DesignApprovedRecord{}, Readers: toArchitect},
	{Kind: DesignChangesRequested, Form: Notice, Record: DesignChangesRequestedRecord{}, Readers: toArchitect},
	{Kind: CarriedGateOpened, Form: Notice, Record: CarriedGateOpenedRecord{}, Readers: toArchitect},
	{Kind: ChildClosed, Form: Notice, Record: ChildClosedRecord{}, Readers: toArchitect},
	{Kind: ChildStatus, Form: Notice, Record: ChildStatusRecord{}, Readers: toArchitect, Urgency: childStatusUrgency},
	{Kind: ChildrenFirst, Form: Notice, Record: ChildrenFirstRecord{}, Readers: toArchitect},
	{Kind: ChildReleaseStalled, Form: Notice, Record: ChildReleaseStalledRecord{}, Readers: toArchitect},
	{Kind: ChildNotRecorded, Form: Notice, Record: ChildNotRecordedRecord{}, Readers: toArchitect},
	{Kind: CatchUp, Form: Notice, Record: CatchUpRecord{}, Readers: toArchitect},
	{Kind: ChecksRed, Form: Notice, Record: ChecksRedRecord{}, Readers: toArchitect, Urgency: checksRedUrgency},
	{Kind: ReviewStuck, Form: Notice, Record: ReviewStuckRecord{}, Readers: toArchitect},
	{Kind: StatusReasserted, Form: Notice, Record: StatusReassertedRecord{}, Readers: toArchitect},
	{Kind: VolumeNearFull, Form: Notice, Record: VolumeNearFullRecord{}, Readers: toArchitect},
	{Kind: SessionLost, Form: Notice, Record: SessionLostRecord{}, Readers: toArchitectAndController},
	{Kind: SessionForgotten, Form: Notice, Record: SessionForgottenRecord{}, Readers: toArchitectAndController},
	{Kind: SessionStoreFault, Form: Notice, Record: SessionStoreFaultRecord{}, Readers: toController},

	{Kind: Triage, Form: ControllerNotice, Record: TriageRecord{}, Readers: toController},
	{Kind: SlotFree, Form: ControllerNotice, Record: SlotFreeRecord{}, Readers: toController},
	{Kind: Todo, Form: ControllerNotice, Record: TodoRecord{}, Readers: toController},
	{Kind: Tick, Form: ControllerNotice, Record: TickRecord{}, Readers: toController},
	{Kind: Unslotted, Form: ControllerNotice, Record: UnslottedRecord{}, Readers: toController},
	{Kind: AwaitingPerson, Form: ControllerNotice, Record: AwaitingPersonRecord{}, Readers: toController, Urgency: lowToController},
	{Kind: ParkedDecision, Form: ControllerNotice, Record: ParkedDecisionRecord{}, Readers: toController},
	{Kind: UpdateStalled, Form: ControllerNotice, Record: UpdateStalledRecord{}, Readers: toController},
	{Kind: ProviderErrors, Form: ControllerNotice, Record: ProviderErrorsRecord{}, Readers: toController},
	{Kind: TreeStalled, Form: ControllerNotice, Record: TreeStalledRecord{}, Readers: toController},
	{Kind: ArchitectStalled, Form: ControllerNotice, Record: ArchitectStalledRecord{}, Readers: toController},
	{Kind: ProjectPaused, Form: ControllerNotice, Record: ProjectPausedRecord{}, Readers: toController},
	{Kind: ProjectResumed, Form: ControllerNotice, Record: ProjectResumedRecord{}, Readers: toController},

	{Kind: CISettled, Form: RoleMessage, Record: CISettledRecord{}, Readers: toPodRoles, Urgency: ciSettledUrgency},
	{Kind: GateMoved, Form: RoleMessage, Record: GateMovedRecord{}, Readers: toPodRoles},
	{Kind: ApprovedRoundStuck, Form: RoleMessage, Record: ApprovedRoundStuckRecord{}, Readers: toReviewer},
	{Kind: MemoryKill, Form: RoleMessage, Record: MemoryKillRecord{}, Readers: toPodRoles},
	{Kind: HookPrompt, Form: RoleMessage, Record: HookPromptRecord{}, Readers: toPhaseWorkers},
	{Kind: UpdateAsk, Form: RoleMessage, Record: UpdateAskRecord{}, Readers: toPodRoles},
	{Kind: MergeQueueRemoval, Form: RoleMessage, Record: MergeQueueRemovalRecord{}, Readers: toMerger},
	{Kind: MergeForwardAsk, Form: RoleMessage, Record: MergeForwardAskRecord{}, Readers: toImplementer},
	{Kind: MergeForwardHold, Form: RoleMessage, Record: MergeForwardHoldRecord{}, Readers: toHoldingWorkers},
	{Kind: MergeForwardLanded, Form: RoleMessage, Record: MergeForwardLandedRecord{}, Readers: toHoldingWorkers},
	{Kind: MergeForwardReview, Form: RoleMessage, Record: MergeForwardReviewRecord{}, Readers: toReviewer},
}
