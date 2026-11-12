package api

// This file is the decision, field by field, about what the daemon's surfaces serve of the
// three durable records a reader of this wire acts on: `supervise.Claim`, `record.Issue` and
// `record.Assignment`. It is data and nothing else — no route, no view and no projection reads
// it at run time — and its readers are the two tests named
// TestEveryRecordFieldIsServedOrExcluded, one in internal/projection over the agent surfaces,
// which reflects over all three records and fails naming a field that stands in none of the
// three columns, and one in this package over `operatorView`, which fails naming a field of
// the operator column that route serves nowhere.
// A field added to any of the three records later is therefore decided rather than forgotten,
// and a field moved between the columns is a decision a reviewer sees in the diff, because each
// entry carries its reason as data (dispatch://INFRA-3083, R3 and R4).
//
// The columns are of the agent surfaces: `api.ClaimView`, which `read_record` and `read_state`
// carry, `api.Issue`, and `api.AssignmentView`, one of which rides each issue's `assignments`
// for every experiment answer that applies to it. ColumnAgents is served there; ColumnOperator
// is the operator route's own and no agent's, with the reason it is not an agent's;
// ColumnWithheld reaches no surface at all. What a column is not is a judgement of whether a
// role finds the field useful: that is the reviewer's reading, and this table records only
// where each field stands and why.

// RecordColumn is one of the three places a field of a durable record stands in.
type RecordColumn string

const (
	// ColumnAgents is served to the agent surfaces, and its keys are those surfaces' own. The
	// operator route carries most of these fields as well, under the same key or under one of
	// its own, and carries none of `servingGeneration`, `pendingCause`, `providerErrorRun` and
	// `updateReasons` (api.OperatorClaim, which internal/api's own half of
	// TestEveryRecordFieldIsServedOrExcluded holds to this column).
	ColumnAgents RecordColumn = "agents"
	// ColumnOperator is the operator route's alone. Two reasons put a field here and no third
	// has yet: a session-facing schema must not name it, or the store never writes it, so the
	// state route — which reads its claims from the store — would serve a constant while the
	// operator route reads the live machine (supervise.Machine.Claim).
	ColumnOperator RecordColumn = "operator"
	// ColumnWithheld reaches no surface: a secret, another system's record, or bookkeeping.
	ColumnWithheld RecordColumn = "withheld"
)

// BookkeepingReason is the one phrase every field withheld as bookkeeping carries, so a field
// withheld for a reason of its own is told from the default at a glance and by the test
// (ClaimRecordFields' DeathCharged is the one claim field taken out of it).
const BookkeepingReason = "bookkeeping a reader decides nothing on"

// RecordFieldDecision is one field of a durable record, the column it stands in, the wire paths
// it is served under and why it stands there. Keys are dotted where the field is served inside
// another key (`failure.cause`), and empty for a field no surface carries.
type RecordFieldDecision struct {
	Field  string
	Column RecordColumn
	Keys   []string
	Reason string
}

// ClaimRecordFields is every field of `supervise.Claim`, in the order the record declares them.
var ClaimRecordFields = []RecordFieldDecision{
	{Field: "Token", Column: ColumnOperator, Keys: []string{"token"},
		Reason: "the operator's routes address a claim by it; both agent surfaces carry a claim under the issue and the role that hold it"},
	{Field: "Project", Column: ColumnWithheld, Reason: BookkeepingReason},
	{Field: "Tree", Column: ColumnOperator, Keys: []string{"tree"},
		Reason: "the operator's listing names each claim's tree; an agent reads it as its issue's own `tree`"},
	{Field: "Issue", Column: ColumnOperator, Keys: []string{"issue"},
		Reason: "the operator's listing names each claim's issue; an agent reads the claim under that issue"},
	{Field: "Role", Column: ColumnOperator, Keys: []string{"role"},
		Reason: "the operator's listing names each claim's role; an agent reads the claim under that role"},
	{Field: "Generation", Column: ColumnOperator, Keys: []string{"generation"},
		Reason: "the run the claim was made for; an agent reads its issue's own `generation`, and `servingGeneration` for the run the claim serves"},
	{Field: "TreeEpoch", Column: ColumnWithheld, Reason: BookkeepingReason},
	{Field: "Session", Column: ColumnAgents, Keys: []string{"session"},
		Reason: "the session its agent registered, which is how a seat is addressed and how a transcript is found"},
	{Field: "SessionFile", Column: ColumnOperator, Keys: []string{"sessionFile"},
		Reason: "a path on the pod's own disk, which the operator's tools open and no agent reads of another seat"},
	{Field: "ServingGeneration", Column: ColumnAgents, Keys: []string{"servingGeneration"},
		Reason: "says whether the work the claim serves is of this generation, which a re-admission and a worker told to wait both turn on"},
	{Field: "ServingAssignmentID", Column: ColumnWithheld, Reason: BookkeepingReason},
	{Field: "ServingCheckLook", Column: ColumnWithheld, Reason: BookkeepingReason},
	{Field: "LastStartRow", Column: ColumnWithheld, Reason: BookkeepingReason},
	{Field: "DeliveredRows", Column: ColumnWithheld, Reason: BookkeepingReason},
	{Field: "Locator", Column: ColumnAgents, Keys: []string{"locator"},
		Reason: "the process the claim runs, which says whether one runs at all and which incarnation a report is of"},
	{Field: "Enrollment", Column: ColumnWithheld,
		Reason: "the secrets broker's record of this claim's process: that broker's business and no reader's here"},
	{Field: "State", Column: ColumnAgents, Keys: []string{"state"},
		Reason: "where the claim stands in its life, which every decision about a worker starts from"},
	{Field: "Budgets", Column: ColumnAgents, Keys: []string{"failure.attempts"},
		Reason: "the four counts a failed claim failed with, read against the limits in `settings`; served inside the failure record, which is where a reader acts on them"},
	{Field: "Failure", Column: ColumnAgents, Keys: []string{"failure.detail"},
		Reason: "the sentence the site that failed the claim wrote, which names what no one word can"},
	{Field: "FailureCause", Column: ColumnAgents, Keys: []string{"failure.cause"},
		Reason: "the one word for what failed the claim, which a retry, an escalation or a re-admission turns on"},
	{Field: "FailedAt", Column: ColumnAgents, Keys: []string{"failure.at"},
		Reason: "when the claim failed, which says whether the record is of this round or an older one"},
	{Field: "PendingCause", Column: ColumnAgents, Keys: []string{"pendingCause"},
		Reason: "names the cause of the death whose relaunch is in flight, which an architect reads while `failure` is still empty"},
	{Field: "Pending", Column: ColumnAgents, Keys: []string{"pendingTask"},
		Reason: "reduced to what a reader decides on — that one is held, its phase, when it was queued, how many sends failed and the last send error — and never its text, which the operator route serves whole"},
	{Field: "BootTokenHash", Column: ColumnWithheld,
		Reason: "a secret: the hash the shim's hello and the agent's registration are resolved by"},
	{Field: "CapabilityHash", Column: ColumnWithheld,
		Reason: "a secret: the hash of what the agent's registration was issued"},
	{Field: "UncertainStreak", Column: ColumnOperator, Keys: []string{"uncertainStreak"},
		Reason: "the run of uncertain process reports, which an operator reads in the listing and no role acts on"},
	{Field: "SuspendedBy", Column: ColumnOperator, Keys: []string{"suspendedBy"},
		Reason: "packages/contracts/src/legion-api.ts states that a session-facing schema must never carry the suspender, and a test holds it there"},
	{Field: "SuspensionHeld", Column: ColumnOperator, Keys: []string{"suspensionHeld"},
		Reason: "the store never writes it, so the state route would serve a constant for it while the operator route reads the live machine"},
	{Field: "SuspensionHeldBy", Column: ColumnOperator, Keys: []string{"suspensionHeldBy"},
		Reason: "the store never writes it, and it names the suspender besides, which no session-facing schema carries"},
	{Field: "Compacting", Column: ColumnOperator, Keys: []string{"compacting"},
		Reason: "the store never writes it, so the state route would serve a constant for it while the operator route reads the live machine"},
	{Field: "ProcessGone", Column: ColumnOperator, Keys: []string{"processGone"},
		Reason: "the store never writes it, so the state route would serve a constant for it while the operator route reads the live machine"},
	{Field: "DeathCharged", Column: ColumnWithheld,
		Reason: "the supervisor's own accounting of one dead process: the record that keeps a second report of that process from being charged a second death"},
	{Field: "PluginContract", Column: ColumnAgents, Keys: []string{"pluginContract"},
		Reason: "the contract the claim's registration declared, which says what shape its seat parses this body with"},
	{Field: "PluginRelaunchFor", Column: ColumnWithheld, Reason: BookkeepingReason},
	{Field: "UpdateRound", Column: ColumnAgents, Keys: []string{"updateRound"},
		Reason: "the round the claim is in, which says the process holding it is waiting to be replaced"},
	{Field: "UpdateReasons", Column: ColumnAgents, Keys: []string{"updateReasons"},
		Reason: "why the daemon must replace this pod, which the worker answering the round is told to act on"},
	{Field: "UpdateOpenedAt", Column: ColumnWithheld, Reason: BookkeepingReason},
	{Field: "UpdateTaskedAt", Column: ColumnWithheld, Reason: BookkeepingReason},
	{Field: "UpdateSignalledAt", Column: ColumnWithheld, Reason: BookkeepingReason},
	{Field: "UpdateHead", Column: ColumnAgents, Keys: []string{"updateHead"},
		Reason: "the commit the worker reported when it answered its round, which says what the replacement must carry"},
	{Field: "UpdateToldAt", Column: ColumnWithheld, Reason: BookkeepingReason},
	{Field: "UpdateAnsweredByTurnEnd", Column: ColumnWithheld, Reason: BookkeepingReason},
	{Field: "UpdateReaskedForPushCheck", Column: ColumnWithheld, Reason: BookkeepingReason},
	{Field: "UpdateLeftRound", Column: ColumnWithheld, Reason: BookkeepingReason},
	{Field: "ProviderErrorRun", Column: ColumnAgents, Keys: []string{"providerErrorRun"},
		Reason: "the run of provider-error turns, which bounds nothing and which the controller is told of once at six, so without it the number behind that telling stands on no surface its reader can reach"},
	{Field: "Step", Column: ColumnAgents, Keys: []string{"step"},
		Reason: "what the daemon last saw the claim's agent do, which says whether a worker mid-turn is still moving"},
	{Field: "LastTurnStart", Column: ColumnAgents, Keys: []string{"lastTurnStart"},
		Reason: "when the agent last began a turn, which tells a worker still making progress from one wedged in a turn"},
	{Field: "LastTurnEnd", Column: ColumnAgents, Keys: []string{"lastTurnEnd"},
		Reason: "when the agent last ended a turn, which says how long an idle seat has been told nothing"},
	{Field: "LastDeathCause", Column: ColumnAgents, Keys: []string{"lastDeathCause"},
		Reason: "why the claim's process was last found gone, which an architect deciding a retry from an escalation reads"},
	{Field: "LastStartFailure", Column: ColumnAgents, Keys: []string{"lastStartFailure"},
		Reason: "why the last start produced no running agent, which is a different fault from a process that died and is read as one"},
	{Field: "LastReady", Column: ColumnAgents, Keys: []string{"lastReady"},
		Reason: "when the agent last said it could be prompted, which is what dates the two causes above"},
	{Field: "TurnOutcomes", Column: ColumnWithheld,
		Reason: "the window the run of refused turns is derived from, whose count reaches a reader as the failure's own sentence"},
	{Field: "Abandoned", Column: ColumnWithheld,
		Reason: "the session this claim left behind, which reaches its replacement in that agent's first task and no surface"},
	{Field: "SessionLost", Column: ColumnWithheld,
		Reason: "an argument of one spec build, set on the copy handed the builder and never on the claim, so no surface could carry it"},
}

// IssueRecordFields is every field of `record.Issue`, in the order the record declares them. No
// field of it is a secret and none is the operator route's alone, so every entry is served to
// the agent surfaces or withheld as bookkeeping.
var IssueRecordFields = []RecordFieldDecision{
	{Field: "Key", Column: ColumnAgents, Keys: []string{"key"}, Reason: "the issue the whole view is about"},
	{Field: "Tree", Column: ColumnAgents, Keys: []string{"tree"},
		Reason: "the root of the issue's tree, which tells a child from a root and finds the members of a tree that lingers"},
	{Field: "Project", Column: ColumnWithheld, Reason: BookkeepingReason},
	{Field: "Title", Column: ColumnWithheld, Reason: BookkeepingReason},
	{Field: "Parent", Column: ColumnWithheld, Reason: BookkeepingReason},
	{Field: "Phase", Column: ColumnAgents, Keys: []string{"phase"},
		Reason: "where the issue's work stands, which every role reads its own next step from"},
	{Field: "Generation", Column: ColumnAgents, Keys: []string{"generation"},
		Reason: "the run the issue is in, which a claim's `servingGeneration` is read against"},
	{Field: "Status", Column: ColumnAgents, Keys: []string{"status"},
		Reason: "the status the daemon means the issue to have and writes to Dispatch"},
	{Field: "Rank", Column: ColumnWithheld, Reason: BookkeepingReason},
	{Field: "LingerUntil", Column: ColumnAgents, Keys: []string{"lingerUntil"},
		Reason: "an absent `holdReason` means either no reason or a tree that lingers, and this is what tells the two apart, which the controller's texts turn on"},
	{Field: "Hold", Column: ColumnAgents, Keys: []string{"holdReason", "heldFrom"},
		Reason: "the reason is a decision someone must act on, suppressed while the tree lingers; the phase is a fact of the record, so every issue at `phase: held` names the phase that stopped"},
	{Field: "LastDispatchSeq", Column: ColumnWithheld, Reason: BookkeepingReason},
	{Field: "HandedOver", Column: ColumnWithheld, Reason: BookkeepingReason},
	{Field: "TakenBack", Column: ColumnWithheld, Reason: BookkeepingReason},
	{Field: "DispatchStatus", Column: ColumnWithheld, Reason: BookkeepingReason},
	{Field: "Repo", Column: ColumnAgents, Keys: []string{"repo"},
		Reason: "the repository the issue's work is done in, which every role's own commands address"},
	{Field: "DefaultBranch", Column: ColumnAgents, Keys: []string{"defaultBranch"},
		Reason: "the branch a role's work is based on and merged into"},
	{Field: "GitHubRepo", Column: ColumnWithheld, Reason: BookkeepingReason},
	{Field: "Ancestry", Column: ColumnAgents, Keys: []string{"ancestry"},
		Reason: "the chain that holds a labelled root under an unreleased child, which reaches no other surface"},
	{Field: "HandedOverFrom", Column: ColumnWithheld, Reason: BookkeepingReason},
	{Field: "ToldParent", Column: ColumnWithheld, Reason: BookkeepingReason},
	{Field: "Awaiting", Column: ColumnAgents, Keys: []string{"waitingOn"},
		Reason: "what a root in awaiting_person waits on from a person, and whether that person has answered"},
	{Field: "HandoutSeq", Column: ColumnWithheld, Reason: BookkeepingReason},
	{Field: "DeclaredSeq", Column: ColumnWithheld, Reason: BookkeepingReason},
	{Field: "DeclaredAt", Column: ColumnWithheld, Reason: BookkeepingReason},
}

// AssignmentRecordFields is every field of `record.Assignment`, in the order the record declares
// them. The keys are `api.AssignmentView`'s, one of which rides each issue's `assignments` for
// every experiment answer that applies to it; the issue's `memoryGroup`, `memoryDrawMode` and
// `memoryDrawnAt` are the `memory` row's own arm, mode and instant spelled out for the readers
// that knew them before any experiment was declared (internal/projection's memoryOf), so they
// are those three fields read a second way rather than a second decision. No field of it is a
// secret and none is the operator route's alone, so every entry is served to the agent surfaces
// or withheld.
var AssignmentRecordFields = []RecordFieldDecision{
	{Field: "Experiment", Column: ColumnAgents, Keys: []string{"experiment"},
		Reason: "the name the project declares the experiment under, which is what names the answer and what a reader of one experiment finds its row by"},
	{Field: "Unit", Column: ColumnWithheld,
		Reason: "the projection spends it: a tree's row rides every issue of that tree and an issue's row its own issue, so the answers under an issue are already the ones that apply to it (internal/projection's assignmentViews)"},
	{Field: "Variant", Column: ColumnAgents, Keys: []string{"variant"},
		Reason: "the arm the unit was drawn into, which is the answer itself and what the issue's `memoryGroup` is read off for the row named `memory`; it rides empty as well, an experiment at mode off drawing no arm"},
	{Field: "Mode", Column: ColumnAgents, Keys: []string{"mode"},
		Reason: "the mode in force when the draw was made, which says what kind of answer the arm is, and what the issue's `memoryDrawMode` carries for the row named `memory`"},
	{Field: "DrawnAt", Column: ColumnAgents, Keys: []string{"drawnAt"},
		Reason: "when the draw was made, absent where the row records none: it is how a reader tells a unit a hand-over settled at no arm from one that was drawn, and what the issue's `memoryDrawnAt` carries for the row named `memory`"},
	{Field: "Carried", Column: ColumnWithheld,
		Reason: "the record's mark of a row a hand-over wrote rather than a draw: what a reader of the state measures is the arm the unit ran under, and a carried row names that arm as a drawn one does"},
}
