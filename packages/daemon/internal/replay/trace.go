package replay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store/storetext"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// TriggerReady is the merger's ready move of the release at 9542c6fb, which took an issue from
// merging into PhaseAwaitingMerge once the merger had readied its pull request. Today's workflow
// has no such move: a window of that release holds it, and the trace reads it as a mark inside
// merging (MoveSources, assumption 20), where the rule legion-v19.57.3 gives the slot back.
const TriggerReady workflow.TriggerKind = "ready"

// PhaseAwaitingMerge is the phase of the release at 9542c6fb in which an issue waited for its
// merge once the merger had readied it. Today's workflow has no such phase: the trace folds the
// time an issue spent in it into merging (MoveSources, assumption 19), so a window of that
// release is replayed on today's phases.
const PhaseAwaitingMerge phase.Phase = "awaiting_merge"

// MoveWay is how a replay makes one kind of recorded phase move. The daemon makes some moves
// itself, from facts a replay feeds it, and a replay must leave those to it; the rest come from
// the record, at the end of the work segment the record shows ending in them.
type MoveWay string

const (
	// MoveTransition is a move made at the end of its recorded work segment through
	// workflow.(*Engine).ReplayTransition: the row lookup and the transition the daemon runs for
	// the trigger, refused unless the issue stands where the record says it stood. It is the way
	// of the completions a handoff or a review settles, whose checks the record shows passed.
	MoveTransition MoveWay = "transition"
	// MoveFact is a move made at the end of its recorded work segment through the daemon's own
	// handler of a fact (MoveSource.Fact), so the guards that handler keeps — a sign-off refused
	// while a child is in the workflow, a merge that waits for the issues beneath it — hold in a
	// replay as they held in the record. A merge whose close waited for the issues beneath it ended
	// its segment at the merge itself, which the record holds in a line of its own
	// (MergedCloseWaits, Visit.Merge), not at the recorded move to done the close made later.
	MoveFact MoveWay = "fact"
	// MovePass is a move the daemon's own admission pass makes from the state a replay reaches,
	// as gate_opened is the promotion of an issue whose first phase the line lets start. A replay
	// never injects it: the recorded move only measures how far the replay's moment is from the
	// record's.
	MovePass MoveWay = "pass"
	// MoveDraw is a move a replay's conflict draw makes, at the rate fitted to the window. The
	// recorded moves of the trigger are not injected, or each conflict would count twice; a recorded
	// conflict whose loop runs to the window's end is kept, not drawn, and made through the same
	// path as a drawn one (assumption 35).
	MoveDraw MoveWay = "draw"
	// MoveMark is a move that moves no phase in today's workflow: a fact of the replay's own
	// (MoveSource.Fact) writes a mark inside the phase the issue stands in, which a rule may read.
	MoveMark MoveWay = "mark"
)

// MoveSource is how the trace builder reads one trigger's recorded moves and how a replay makes
// them: the line the record holds each move in, the way the replay makes it and, for a trigger of
// a recorded release that today's table lacks, how the move is mapped onto today's phases and the
// assumptions the mapping rests on.
type MoveSource struct {
	// Line is the msg of the recorded line each move of the trigger is read from, whose trigger
	// attribute is the key the source stands under.
	Line string
	// Way is how the replay makes the move.
	Way MoveWay
	// Fact names the fact the replay applies for the move where Way is MoveFact or MoveMark, as
	// <package>.<type>: a fact of package intake, which the daemon's own handler applies, or one
	// of this package's.
	Fact string
	// To is the phase the replay moves the issue to where it is one phase whatever phase the
	// issue left, as a release's checks_red moved an issue back to implementing from any phase;
	// empty where each move goes where its row of the table goes.
	To phase.Phase
	// Folds is a phase a recorded release entered by this trigger and today's workflow lacks,
	// which the trace reads as the phase the move left: the time an issue spent in it is time in
	// that phase, and a move out of it is a move out of that phase. Empty for a trigger that
	// enters no such phase.
	Folds phase.Phase
	// Assumptions is the numbers of the assumptions (Assumptions) the mapping of the move rests
	// on, for a move of a recorded release today's table lacks; none for a move today's table
	// makes as the record shows it made.
	Assumptions []int
}

// MoveSources is the trace builder's map from a recorded move to the move a replay makes, by the
// trigger the record's line names: one entry for each trigger of a row of the table any rule a
// replay runs carries (workflow.TableFor), and for each trigger of a recorded release that
// today's table lacks, which the entry maps onto today's phases. A recorded move whose trigger
// has no entry here has no source in the replay, and the inventory test of the table
// (TestEveryRowOfTheTableHasASourceInTheRecord) names it.
//
// gate_opened is the admission pass's own promotion and is never injected. The completions a
// handoff or a review settles go through the engine's ReplayTransition at the end of their
// segment, and so does the release at 9542c6fb's checks_red, which moved an issue back to
// implementing from any phase. A sign-off, a merge and a backward move go through the daemon's
// handler of the fact the API or GitHub applies, so the guards that handler keeps hold; a merge
// whose close waited for the issues beneath it is applied at the moment of the merge, read from
// its own line (MergedCloseWaits), and the move to done its close made later is the engine's. A
// conflict is drawn at the rate the window's recorded conflicts fit, so its recorded moves are not
// injected. The release at 9542c6fb's ready move is a mark inside merging, the phase its
// awaiting_merge is read as.
var MoveSources = map[workflow.TriggerKind]MoveSource{
	workflow.TriggerGateOpened:          {Line: phaseChangedLine, Way: MovePass},
	workflow.TriggerPlannerCompleted:    {Line: phaseChangedLine, Way: MoveTransition},
	workflow.TriggerImplementationReady: {Line: phaseChangedLine, Way: MoveTransition},
	workflow.TriggerTesterPassed:        {Line: phaseChangedLine, Way: MoveTransition},
	workflow.TriggerTesterFailed:        {Line: phaseChangedLine, Way: MoveTransition},
	workflow.TriggerReviewApproved:      {Line: phaseChangedLine, Way: MoveTransition},
	workflow.TriggerReviewRejected:      {Line: phaseChangedLine, Way: MoveTransition},
	workflow.TriggerRetroCompleted:      {Line: phaseChangedLine, Way: MoveTransition},
	workflow.TriggerSignOff:             {Line: phaseChangedLine, Way: MoveFact, Fact: "intake.SignOff"},
	workflow.TriggerPullRequestMerged:   {Line: phaseChangedLine, Way: MoveFact, Fact: "intake.PullRequestMerged"},
	workflow.TriggerBackward:            {Line: phaseChangedLine, Way: MoveFact, Fact: "intake.BackwardMove"},
	workflow.TriggerConflict:            {Line: phaseChangedLine, Way: MoveDraw, To: phase.Implementing, Assumptions: []int{3, 18, 23}},
	workflow.TriggerChecksRed:           {Line: phaseChangedLine, Way: MoveTransition, To: phase.Implementing, Assumptions: []int{21, 22}},
	TriggerReady:                        {Line: phaseChangedLine, Way: MoveMark, Fact: "replay.ReadyMark", Folds: PhaseAwaitingMerge, Assumptions: []int{19, 20}},
}

// TriggerChildrenOpen and TriggerChildrenDone are the trace's names for the moves of a started
// parent into awaiting_children and back out of it (cybermonklabs/legion#4592, record.SlotRule's
// WaitStopsPhase), which the daemon writes under lines of their own and no `workflow: phase
// changed` (workflow.ParentWaitsLine, workflow.ParentReturnsLine): the admission pass's and the
// engine's own moves, made by the replay's own daemon under a rule whose wait stops the phase, and
// never injected. They are no row of a table (workflow.TableFor), so they stand apart from
// MoveSources, in ChildrenWaitMoves.
const (
	TriggerChildrenOpen workflow.TriggerKind = "children_open"
	TriggerChildrenDone workflow.TriggerKind = "children_done"
)

// ChildrenWaitMoves is the trace builder's source of each of the two moves of a parent's wait on
// its open children, by its trigger (TriggerChildrenOpen, TriggerChildrenDone): each read from its
// own line, and each the pass's move, which a replay makes itself.
var ChildrenWaitMoves = map[workflow.TriggerKind]MoveSource{
	TriggerChildrenOpen: {Line: workflow.ParentWaitsLine, Way: MovePass},
	TriggerChildrenDone: {Line: workflow.ParentReturnsLine, Way: MovePass},
}

// phaseChangedLine is the msg of the line every recorded phase move is written under
// (workflow.(*Engine).transition), the line each MoveSource reads its moves from.
const phaseChangedLine = "workflow: phase changed"

// passMovedSlotsLine and phaseHeldLine are the msgs of the two lines a later release writes where
// an older one wrote none: the record of a pass that moves a slot (PassMovedSlots) and the hold's
// beginning (PhaseHeld), the constants their writers log them under, so a rename of either moves
// the replay's reader with it. The trace reads each where the window holds its kind
// (Window.Lines) and builds the same facts from the older lines where it does not.
const (
	passMovedSlotsLine = admit.PassMovedSlotsLine
	phaseHeldLine      = workflow.PhaseHeldLine
)

// reconcileHoldingLine and consumerCaughtUpLine are the msgs of the two lines of a boot's hold on
// the listing's summaries (ReconcileHolding, ConsumerCaughtUp), which a replay reads the hold's
// beginning and end from.
const (
	reconcileHoldingLine = "admission reconcile: holding roots for the Dispatch consumer to reach a stream position"
	consumerCaughtUpLine = "admission: the Dispatch consumer has caught up; releasing held roots"
)

// gateOpenedLine and gateClosedLine are the msgs of the daemon's lines of a design gate's opening
// and closing (GateOpened, GateClosed), which it writes at every artifact event that flips a gate
// and at no other (workflow's applyArtifact): a window that holds both kinds says each flip of its
// gates in them (Trace.GateLines).
const (
	gateOpenedLine = "workflow: design gate opened"
	gateClosedLine = "workflow: design gate closed"
)

// processDiedLine, processDisplacedLine and workEndedLine are the msgs of the three lines the end
// of a claim's process writes (ProcessEnded): its death (supervise's died), its displacement
// through no fault of its own (displaced), and the let-go of a process that ran work (recordEnd).
const (
	processDiedLine      = "supervise: process died"
	processDisplacedLine = "supervise: the process was displaced through no fault of its own, its pod replaced by the daemon or it another runtime's process this daemon takes over; relaunching it uncharged"
	workEndedLine        = "supervise: the relaunch ended work the agent had running"
)

const (
	launchFailedLine     = "supervise: launch failed"
	launchStoreFaultLine = "supervise: the session store could not answer this start; trying it again every minute, charging nothing"
	launchPromptLine     = "supervise: the role's system prompt is too large to launch; failing the claim, charging no launch failure"
)

// checkClosedLine is the msg of the line the close of an issue's production check writes
// (workflow's markCheckClosed, CheckClosed), in every release since the check began (d321c422).
const checkClosedLine = "workflow: production check closed"

// mergeForwardAskedLine and mergeForwardLandedLine are the msgs of the lines a conflicting head's
// merge forward writes where the issue keeps its phase (record.ConflictMergeForward, whose
// conflict past the review waits for the merger since cybermonklabs/legion#4979, and
// record.ConflictImplementerMergesForward): the engine's ask of the role the rule names
// (record.SlotRule.MergeForwardAsker) to merge the base forward (workflow's mergeForward) and the
// head that merge left arriving (mergeForwardLanded), the round of conflict a merge forward is
// (MergeForwardRound).
const (
	mergeForwardAskedLine  = "workflow: merge forward asked"
	mergeForwardLandedLine = "workflow: the merge forward landed"
)

// mergedCloseWaitsLine is the msg of the line a merge writes where the issue's close waits for the
// issues beneath it still in the workflow (workflow's closeMerged, MergedCloseWaits): the moment
// the issue's pull request merged, where the recorded move to done comes only at the last one's
// leave.
const mergedCloseWaitsLine = "workflow: a merged issue's close waits for the issues beneath it"

// slotTakenLine and slotReleasedLine are the msgs of the admission pass's lines of a slot taken
// and given back (admit's logSlot), waitsOnIssuesLine of its line of an issue that waits for what
// it waits on (admit's promoteHolds), waitsForChildrenLine of its line of an issue that waits for
// the Dispatch children under it (admit's tellChildrenFirst), and leftWorkflowLine of the engine's
// line of an issue leaving the workflow (workflow's leave). The trace reads each (lineReaders), the
// driver reads the ones a replay writes as it runs (tapped, observe), and a replay leaves the
// per-pass wait out of its own log (unwritten).
const (
	slotTakenLine        = "admission: slot taken"
	slotReleasedLine     = "admission: slot released"
	waitsOnIssuesLine    = "admission waits for what this issue waits on"
	waitsForChildrenLine = "admission: an issue waits for the Dispatch children under it"
	leftWorkflowLine     = "workflow: issue left the workflow"
)

// turnStartedLine, turnEndedLine, launchedLine, suspendedLine and claimFailedLine are the msgs of
// supervision's lines of a claim's turn beginning and ending, its launch, its suspension and its
// failure (supervise's machine), which the trace reads (lineReaders) and a replay's worker stub
// writes as supervision does.
const (
	turnStartedLine = "supervise: a turn started"
	turnEndedLine   = "supervise: a turn ended"
	launchedLine    = "supervise: launched"
	suspendedLine   = "supervise: suspended"
	claimFailedLine = "supervise: claim failed"
)

// pauseBeganLine and pauseEndedLine are the msgs of the lines of a project's pause beginning and
// ending (internal/pause's Pause and Resume), which the trace reads (lineReaders) and a replay's
// own pause writes (driver.pause, driver.resume).
const (
	pauseBeganLine = "pause: the project is paused"
	pauseEndedLine = "pause: the project is resumed"
)

// LineHead is what every daemon log line the trace reads carries whatever its kind: the msg the
// daemon wrote it under and the time the daemon wrote it, the line's own time attribute. A Cloud
// Logging export's timestamp is when the log store took the line in, a moment later, and is not
// read.
type LineHead struct {
	Msg  string
	Time time.Time
}

// Head is h. Every line type embeds its head, so a reader of a LogLine of any kind reads the
// line's msg and time through it.
func (h LineHead) Head() LineHead { return h }

// logLine seals LogLine: a type is one only by embedding LineHead.
func (LineHead) logLine() {}

// LogLine is one daemon log line of a kind the trace reads, as the type of its kind
// (ParseLogLine). Each kind is a type that embeds LineHead and holds the attributes the trace reads
// from it, so the trace builder reads a line by its type and fields, and the msgs and attribute
// names of the daemon's lines are read in one place.
type LogLine interface {
	Head() LineHead
	logLine()
}

// ClaimRef is the claim a supervise: line is about, read from the line's claim attribute: the
// claim's token, and the issue and role the token names. A supervise: line names its issue and
// role through the token alone (the machine's logger carries the token, and no such line carries
// an issue), so the trace joins a turn to its issue and role by these. Issue is the key as
// Dispatch writes it, ACME-37 for legion-acme-acme-37-tester; the controller's token names no
// issue and the role claim.RoleController.
type ClaimRef struct {
	Token claim.Token
	Issue string
	Role  claim.Role
}

// PhaseChanged is a `workflow: phase changed` line (workflow's transition): an issue of a tree
// moved from one phase to another by a trigger, and Status is the issue's Dispatch status then.
// From and To are the phases the line names, as written: a phase of a recorded release that
// today's workflow lacks (PhaseAwaitingMerge) is kept for the trace to fold (MoveSources), never
// refused.
type PhaseChanged struct {
	LineHead
	Issue, Tree string
	From, To    phase.Phase
	Trigger     workflow.TriggerKind
	Status      string
}

// SlotTaken is an `admission: slot taken` line (admit's logSlot): the admission pass gave an issue
// a slot, after which the project held Slots of its Cap. A window recorded before the daemon wrote
// a record line for each pass that moves slots reads its slots from this line and SlotReleased
// alone.
type SlotTaken struct {
	LineHead
	Issue string
	// Slots is the slots in use once this one was taken.
	Slots int
	// Cap is the admission cap the pass ran with.
	Cap int
}

// SlotReleased is an `admission: slot released` line (admit's logSlot): the admission pass took an
// issue's slot back, after which the project held Slots of its Cap.
type SlotReleased struct {
	LineHead
	Issue string
	// Slots is the slots in use once this one was released.
	Slots int
	// Cap is the admission cap the pass ran with.
	Cap int
}

// WaitsOnIssues is an `admission waits for what this issue waits on` line: the admission pass
// passed over a member of its waiting line because it waits on the issues Blockers names
// (blockers.LineWaitsOn), in that order.
type WaitsOnIssues struct {
	LineHead
	Issue    string
	Blockers []string
}

// SlotBackWorkReturned is an `admission gives a slot back to an issue whose work came back` line:
// the admission pass gave a slot to an issue whose work a move sent back to it while it held none
// (record.LineReturning), standing in Phase.
type SlotBackWorkReturned struct {
	LineHead
	Issue string
	Phase phase.Phase
}

// SlotBackWaitEnded is an `admission gives a slot back to an issue whose wait ended` line: the
// admission pass gave a slot back to an issue that had given its own up to wait on another issue
// (record.LineWaitingOnIssue), standing in Phase, once that wait was over.
type SlotBackWaitEnded struct {
	LineHead
	Issue string
	Phase phase.Phase
}

// WaitsForChildren is an `admission: an issue waits for the Dispatch children under it` line: an
// issue's own first phase is held while Dispatch children of its own are open (the children-first
// rule), in its tree at the tree's generation.
type WaitsForChildren struct {
	LineHead
	Issue, Tree string
	Generation  uint64
	// Children is how many of the issue's Dispatch children were open.
	Children int
}

// WaitsOnPerson is a `workflow: root waits on a person and keeps its admission slot` line: a root
// entered a wait on a person (an ask, or the review of its design gate's document) for Reason,
// the wait standing since the issue's Dispatch event of seq SinceSeq.
type WaitsOnPerson struct {
	LineHead
	Issue, Tree string
	Reason      string
	SinceSeq    int64
}

// GateOpened is a `workflow: design gate opened` line: an issue's design gate opened at a Dispatch
// event of its document's artifact, of kind Event (an intake.DispatchArtifactKind: approved,
// version, …), at Version.
type GateOpened struct {
	LineHead
	Issue    string
	Artifact string
	Event    string
	Version  int
}

// GateClosed is a `workflow: design gate closed` line: an issue's design gate closed at a Dispatch
// event of its document's artifact, of kind Event (an intake.DispatchArtifactKind: a new version,
// …), at Version.
type GateClosed struct {
	LineHead
	Issue    string
	Artifact string
	Event    string
	Version  int
}

// GateRegistered is a `workflow: design gate registered` line (workflow's gateRegistered): an
// issue's architect registered version Version of its design document Artifact at the design
// step Step, Open whether that version stood approved and ChangesRequested whether a changes
// request stood on it, under the deployment's gated steps GatedSteps (the empty list gates none,
// and the registration opens the gate at once). A replay rebuilds intake.GateRegistered from it
// and the Dispatch shadow, since the registration is an API fact and no Dispatch event carries it.
// A line of an older release lacks some of those members and is read as that release meant it
// (ParseLogLine): Step the spec, ChangesRequested false where the line lacks it, and GatedSteps
// none, the window's own gate steps being what a replay gates by.
type GateRegistered struct {
	LineHead
	Issue            string
	Artifact         string
	Step             record.DesignStep
	Version          int
	Open             bool
	ChangesRequested bool
	GatedSteps       []record.DesignStep
}

// HoldReleased is a `workflow: a held phase is released` line (workflow's retryPhase): an issue
// held where its phase's worker failed was put back in Phase, for Reason. The operator's retry of
// a phase's worker (workflow's restartPhase, operatorRetryReason) writes it for an issue that
// stands in no hold too, where it releases nothing and is that retry alone.
type HoldReleased struct {
	LineHead
	Issue  string
	Phase  phase.Phase
	Reason string
}

// LeftWorkflow is a `workflow: issue left the workflow` line (workflow's leave): an issue of a tree
// left the workflow at the Dispatch status Status.
type LeftWorkflow struct {
	LineHead
	Issue, Tree string
	Status      string
}

// TreeLingers is a `workflow: tree lingers` line: a tree whose work is done lingers, at its
// generation, until Until, when its linger closes it.
type TreeLingers struct {
	LineHead
	Tree       string
	Generation uint64
	Until      time.Time
}

// LingerClosed is a `workflow: linger closed the tree` line: a tree's linger ended at its
// generation and closed the tree's issues, Issues of them.
type LingerClosed struct {
	LineHead
	Tree       string
	Generation uint64
	Issues     int
}

// TurnStarted is a `supervise: a turn started` line: a claim's session began a turn at the claim's
// generation.
type TurnStarted struct {
	LineHead
	Claim      ClaimRef
	Generation uint64
	// Delivery is the task the turn is the turn of (outbox:<id>); "" for a turn that is no task's,
	// whose line carries no delivery.
	Delivery string
}

// TurnEnded is a `supervise: a turn ended` line: a turn of a claim's session at the claim's
// generation is over. The daemon writes the msg in two lines: the turn's own end line, and, where
// the turn's producer reports its outcome (supervise's turnOutcome), the outcome's line, which
// carries the turn's sequence. The trace ends the turn at the first and counts no turn at the
// second.
type TurnEnded struct {
	LineHead
	Claim      ClaimRef
	Generation uint64
	// Outcome is a line that carries sequence: the turn outcome's line; false for the turn's own
	// end line, which carries none.
	Outcome bool
}

// Launched is a `supervise: launched` line: the runtime launched a claim's session at the claim's
// generation, resuming the session the claim recorded where Resumed.
type Launched struct {
	LineHead
	Claim      ClaimRef
	Generation uint64
	Resumed    bool
}

// Suspended is a `supervise: suspended` line: a claim's session was suspended by the act By names
// (released, …) for Reason.
type Suspended struct {
	LineHead
	Claim  ClaimRef
	By     string
	Reason string
}

// ClaimFailed is a `supervise: claim failed` line (supervise's machine): a claim's worker ran out
// of a budget the machine keeps (launches, deaths, prompts) for Cause (died, launch_refused, …),
// Why being the error it gives. A phase worker's failed claim is what puts its issue in the
// phase held (workflow's claimFailed). A release that wrote no line of the hold's own there
// (PhaseHeld) left this line its only mark, so in a window that holds no PhaseHeld line the trace
// reads the hold's start from this line; a replay injects intake.ClaimFailed at its offset either
// way. A line of a release that recorded no cause carries none, and its Cause is the one the hold
// of a failure that names none carries, supervise.CauseUnrecorded (ParseLogLine).
type ClaimFailed struct {
	LineHead
	Claim ClaimRef
	Cause string
	Why   string
}

// ProcessEnded is one of the lines the end of a claim's process writes, each of which ends the
// turn that process ran with no `supervise: a turn ended` of its own (supervise's turnEnded names
// the ways out of a turn that write a line of their own): `supervise: process died` (the machine's
// died), the process found gone, or found to be another runtime's, through no fault of its own
// (displaced), and `supervise: the relaunch ended work the agent had running` (recordEnd), which
// the let-go of a process that ran work writes as it stops it — a relaunch, a suspension, a
// failure, the retirement a stop makes (from 86218fbc, #3983, on: no release before it wrote the
// line) — and which died and displaced write after their own line. The retirement the close of an
// issue or a tree makes writes none of them in any release (CheckClosed). By is how the line ends
// the claim's open turn: the trace ends it there, and a line of a claim with no turn open ends
// nothing.
//
// The lines the same decisions write beside these end no turn of their own, and the trace does
// not read them: died's `supervise: the process died in the task's turn; …` (interrupted) and
// `supervise: the agent died with work outstanding` (chargeDeath), and displaced's `supervise: the
// process was displaced mid-turn, …` (replaceProcess), each written after its decision's first
// line at the same moment; `supervise: the turn an earlier daemon saw start is over`, which
// askTurnOver follows with that turn's own end line (turnEnded); and `supervise: launch cut
// short; its context ended`, the end of a launch, in which no turn runs.
type ProcessEnded struct {
	LineHead
	Claim ClaimRef
	By    TurnEnd
	// DeathCause is present only on a death observation that names the runtime's verdict.
	DeathCause string
}

// StartFailure is a runtime refusal logged by launchTries. It describes an attempt, not
// necessarily a failed claim or a turn that ended. Its error is the text available in the log.
type StartFailure struct {
	LineHead
	Claim ClaimRef
	Error string
}

// guardKillToldLine and guardKillUnrecordedLine are the msgs of the daemon's lines of a kill of a
// process of an issue pod's role that its launcher reported (workflow's memoryGuardKill): the
// line of the memory guard's kill, whose role is told why, and the line of a kill of any cause
// that names an issue the daemon does not record, which tells nobody. guardKillNoRoleLine is the
// line of a kill of a process of no role, which the launcher reports on the connection of one
// role of its pod and which tells no role (workflow.MemoryKillNoRoleLine, the constant its writer
// logs it under).
const (
	guardKillToldLine       = "workflow: the role whose process the memory guard ended is told why"
	guardKillUnrecordedLine = "workflow: a memory guard kill names an issue the daemon does not record, so no role is told"
	guardKillNoRoleLine     = workflow.MemoryKillNoRoleLine
)

// GuardKillLine is one of the daemon's lines of a kill an issue pod's launcher reported
// (guardKillToldLine, guardKillUnrecordedLine, guardKillNoRoleLine): the issue, the claim of the
// role whose process was ended, its role none for a process of no role (readNoRoleKill), that
// process's pid, and the kill's cause, "" for the memory guard's own
// (shimwire.MemoryKillCauseGuard). The launcher's own line of the kill is the kill signal's
// record, and this line is read beside it by the kill matcher alone (MatchKills): the trace
// builder reads nothing of it, so a window that holds it is traced as one that does not.
type GuardKillLine struct {
	LineHead
	Issue string
	Claim ClaimRef
	PID   int
	Cause string
}

// CheckClosed is a `workflow: production check closed` line (workflow's markCheckClosed): the
// production check of an issue's merge closed. Where the issue stands outside the workflow,
// closeCheck then has the claim of the role the check is for retired (issue_close), and that
// retirement writes no line of its own in any release: before 86218fbc (#3983) a retirement wrote
// none, and from it on the close of an issue retires its claim with noEnd, for which recordEnd
// writes nothing (supervise's issueClose, Machine.retire). So the trace ends the open turn of
// that claim at this line, the last line the record holds of the check (traceBuilder.checkClosed).
type CheckClosed struct {
	LineHead
	Issue string
}

// DaemonStarted is a `legion daemon started` line: a daemon process of Project finished its boot
// and serves, under the admission cap AdmissionCap.
type DaemonStarted struct {
	LineHead
	Project      string
	AdmissionCap int
}

// ProcessStart is one of the lines a daemon process writes once as it starts, before it opens its
// store: the deployment leak check's line (configured or not), `daemon: stale-plugin relaunches
// install` on the kubernetes runtime, and `daemon: opening the store; …`. A release that predates
// some of them starts with the others, so the trace reads a process's start, and the downtime
// before it, at the first of them after the previous process's lines.
type ProcessStart struct {
	LineHead
}

// BootStage is a `legion workflow boot stage` line: a booting daemon reached Stage (prompts,
// store, launcher, admission, api). It writes the first, prompts, before it opens its store.
type BootStage struct {
	LineHead
	Stage string
}

// QueueLeftNothingRead is a `workflow: a pull request left the merge queue where its issue does
// not merge at that head; nothing is read` line (workflow's dequeued): an issue's pull request left
// GitHub's merge queue at the head HeadSHA (the line's head) while the record no longer merges the
// issue there, for the reason Why, and the daemon read nothing. The trace reads it as a drop from
// the merge queue (SendBack.Drop).
type QueueLeftNothingRead struct {
	LineHead
	Issue       string
	PullRequest int
	HeadSHA     string
	Why         string
}

// QueueLeftReadOnce is a `workflow: a pull request in merging left the merge queue; the daemon
// reads it from GitHub once` line (workflow's dequeued): the pull request of an issue in merging
// left GitHub's merge queue at the head HeadSHA (the line's head), and the daemon reads it from
// GitHub once to learn what became of it. The trace reads it as a drop from the merge queue
// (SendBack.Drop).
type QueueLeftReadOnce struct {
	LineHead
	Issue       string
	PullRequest int
	HeadSHA     string
}

// MergeForwardAsked is a `workflow: merge forward asked` line (workflow's mergeForward): the
// engine asked the implementer of Issue, or in merging its merger, standing in Phase, to merge the
// base forward onto a head that conflicts with it, the issue keeping its phase.
type MergeForwardAsked struct {
	LineHead
	Issue string
	Phase phase.Phase
}

// MergeForwardLanded is a `workflow: the merge forward landed` line (workflow's
// mergeForwardLanded): the first head after the one a merge forward was asked of arrived on Issue's
// pull request.
type MergeForwardLanded struct {
	LineHead
	Issue string
}

// MergedCloseWaits is a `workflow: a merged issue's close waits for the issues beneath it` line
// (workflow's closeMerged): the pull request PullRequest of Issue, of Tree, merged while the issues
// WaitingFor beneath it were still in the workflow, so the issue stands in merging until the last
// of them leaves, when the engine closes it. The daemon writes the line at the merge, and again at
// each later read of the wait that finds one still there (a departure beneath the issue, a boot's
// sweep), so the first one of a visit to merging is the moment the pull request merged. WaitingFor
// is the keys the line names, which the daemon writes as one string joined by ", ".
type MergedCloseWaits struct {
	LineHead
	Issue, Tree string
	PullRequest int
	WaitingFor  []string
}

// mergeStateReadLine is the msg of the daemon's line of each read of a pull request's merge state
// (workflow.MergeStateReadLine), the constant its writer logs it under.
const mergeStateReadLine = workflow.MergeStateReadLine

// MergeStateRead is a `workflow: GitHub's merge state of a pull request was read` line (workflow's
// mergeability): a read of the merge state of the pull request PullRequest of Repo, the pull
// request of Issue, whose fact committed. HeadSHA is the head GitHub answered for (the line's
// head) and RecordedHead the one the record stood at; Base is the base branch GitHub named, else
// the record's, and BaseSHA the base commit GitHub's answer names, "" where it names none;
// Mergeable and MergeState are GitHub's answer; ReadAt is the moment the reader had the answer in
// hand, zero for a fact that carries none. The trace holds each read on its issue
// (IssueTrace.MergeStateReads) and decides nothing from it, and collect-git reads the heads and
// base commits git's verdicts are read for from it (CollectGit). A window collected before the
// daemon wrote the line holds none, which Window.Lines shows.
type MergeStateRead struct {
	LineHead
	Issue, Repo           string
	PullRequest           int
	HeadSHA, RecordedHead string
	Base, BaseSHA         string
	Mergeable, MergeState string
	ReadAt                time.Time
}

// PassMovedSlots is an `admission: a pass moved slots` line (admit's logPassMoves), the record of
// one admission pass that gave out or took back at least one slot, written once its fact
// committed: the slots in use after the pass, the cap it ran with, each slot it took back with
// the path that took it and each slot it gave out with the kind of entry its issue stood as, in
// the order the pass moved them (every take before every give: the releases run before the
// promotion), and each candidate of the line it passed over with its kind, place and reason,
// which no other line names. A window recorded from a release that writes it reads its slots and
// its passed-over candidates from it (Trace.Slots, Trace.Passed).
type PassMovedSlots struct {
	LineHead
	// Slots is the slots in use once the pass's last move was made.
	Slots int
	// Cap is the admission cap the pass ran with.
	Cap    int
	Given  []record.SlotGiven
	Taken  []record.SlotTaken
	Passed []record.SlotPassed
}

// PhaseHeld is a `workflow: a phase is held` line (workflow's claimFailed, and the escalation of a
// hold): an issue of a tree was held from the phase From, whose worker is Role, for Reason. A
// window recorded from a release that writes it starts each hold at it; an older window starts a
// hold at the `supervise: claim failed` of the phase's worker, since the move into held wrote no
// line there.
type PhaseHeld struct {
	LineHead
	Issue, Tree string
	From        phase.Phase
	Role        claim.Role
	Reason      string
}

// ProcessStop is one of the lines a daemon process writes as it stops (internal/daemon's run and
// its stop): `legion daemon stopping`, as it begins to stop, and `legion daemon stopped`, its last
// line. The trace reads the downtime before the next process from the first of them, so a quiet
// stretch of a running daemon is not taken for a daemon down.
type ProcessStop struct {
	LineHead
	Project string
}

// ReconcileHolding is an `admission reconcile: holding roots for the Dispatch consumer to reach a
// stream position` line (admit's Reconcile): a boot's listing found Count issues ahead of the
// record while the Dispatch consumer stood at AckFloor, below the stream's Target, so admission
// holds their summaries until the consumer reaches it (admit.Admission's pending hold). A replay
// holds them from its own boot's reconcile to the release the record shows (ConsumerCaughtUp).
type ReconcileHolding struct {
	LineHead
	Count    int
	Target   int64
	AckFloor int64
}

// ConsumerCaughtUp is an `admission: the Dispatch consumer has caught up; releasing held roots`
// line (admit's release): the consumer reached the position a boot's hold waited for, and the
// pass applied the Count held summaries, admitting Admitted of them.
type ConsumerCaughtUp struct {
	LineHead
	Count    int
	AckFloor int64
	Idle     bool
	Admitted int
}

// PauseBegan is a `pause: the project is paused` line (internal/pause's Pause): the project's pause
// Pause began, for Reason, and from then no admission pass promotes anything until it ends
// (PauseEnded).
type PauseBegan struct {
	LineHead
	Pause  int64
	Reason string
}

// PauseEnded is a `pause: the project is resumed` line (internal/pause's Resume): the pause Pause
// ended, and the resume's own pass (intake.ProjectResumed) promotes what the pause held.
type PauseEnded struct {
	LineHead
	Pause int64
}

// RootJoined is a `workflow: a queued root joined a live tree` or `workflow: a closed tree's
// member joined a live tree` line (workflow's adoption): Issue, recorded as a root of its own or
// as a member of a closed tree, joined Tree at Generation, an architect's release of it into its
// tree (intake.ChildAdoption), which a replay applies again at its offset in the architect's work.
type RootJoined struct {
	LineHead
	Issue, Tree string
	Generation  uint64
}

// ParseLogLine reads one daemon log line as the daemon's JSON handler writes it on stderr (one
// JSON object of time, level, msg and the attributes; a Cloud Logging export's jsonPayload) as the
// type of its kind. A line of a msg the trace does not read is nil with no error: the trace skips
// it. A line that is not one JSON object, or has no time, is refused, and so is a line of a kind it
// reads that lacks an attribute its type reads or carries one of another JSON type, so an attribute
// the daemon renames fails the read rather than reading as a zero value. The two attributes whose
// absence a field's doc gives a meaning, TurnStarted.Delivery and TurnEnded.Outcome, are the
// exceptions.
//
// So are the older shapes of two kinds, each read as the release that wrote it meant it:
//
//   - `workflow: design gate registered`: the releases before 5c4548f0 (#2253) wrote issue,
//     artifact, version, open and policy, and the releases from it until b7866f96 (#2848) added
//     changes_requested; b7866f96 replaced policy with step and gated_steps, which every release
//     since writes. A line with no step and no gated_steps is of a gate of one step, the spec, as
//     every gate was before b7866f96, and names no gated steps, the window's own (Window.GateSteps)
//     being what a replay gates by. One with no changes_requested either applied no changes
//     request, since a registration of those releases read no review: it opened the gate where the
//     deployment gated nothing and recorded it closed otherwise.
//   - `supervise: claim failed`: the releases before 97519812 (#3662) wrote no cause, and a line of
//     theirs is read with the cause the hold of a failure that names none carries,
//     supervise.CauseUnrecorded (workflow's claimFailed and noticeFailure).
//
// A line of a shape no release wrote — step or gated_steps without the other, or the two without
// changes_requested — is refused as any line lacking a member it reads is, and so is a member of
// another JSON type in any shape. A member an older shape carries and the trace does not read, the
// old gate line's policy, is left as every unread member is.
func ParseLogLine(line []byte) (LogLine, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil || raw == nil {
		return nil, fmt.Errorf("replay: a log line is not one JSON object: %s", line)
	}
	attrs := &lineAttrs{raw: raw}
	at, msg := attrs.at("time"), attrs.str("msg")
	if attrs.err != nil {
		return nil, fmt.Errorf("replay: the log line %s %w", line, attrs.err)
	}
	read := lineReaderOf(msg)
	if read == nil {
		return nil, nil
	}
	parsed := read(LineHead{Msg: msg, Time: at}, attrs)
	if attrs.err != nil {
		return nil, fmt.Errorf("replay: the %q line at %s %w", msg, at.Format(time.RFC3339Nano), attrs.err)
	}
	return parsed, nil
}

// TraceReads says whether the trace reads the lines the daemon writes under msg: ParseLogLine
// reads a line of msg as its kind's type, and collect keeps exactly the lines of such a msg.
func TraceReads(msg string) bool { return lineReaderOf(msg) != nil }

// processStartLines is the msgs of the lines a daemon process writes once as it starts, before it
// opens its store (ProcessStart), but the leak check's: the kubernetes runtime's stale-plugin line
// and the store's opening.
var processStartLines = []string{
	"daemon: stale-plugin relaunches install",
	"daemon: opening the store; a connection error that the next attempt can clear is tried again",
}

// leakCheckLine begins both lines of the deployment leak check a daemon process writes as it
// starts (internal/daemon's leak_check.go): configured, or not configured with the settings that
// would name one. Each is a ProcessStart.
const leakCheckLine = "daemon: the deployment leak check is "

// lineReaders is the trace's reader of each line kind it reads, by the msg the daemon writes the
// kind under: the one place the msgs and the attribute names of the daemon's lines are read. Each
// reader takes the line's head and its attributes and reads every attribute its type holds.
var lineReaders = map[string]func(LineHead, *lineAttrs) LogLine{
	phaseChangedLine: func(h LineHead, a *lineAttrs) LogLine {
		return PhaseChanged{LineHead: h, Issue: a.str("issue"), Tree: a.str("tree"), From: phase.Phase(a.str("from")),
			To: phase.Phase(a.str("to")), Trigger: workflow.TriggerKind(a.str("trigger")), Status: a.str("status")}
	},
	workflow.ParentWaitsLine: func(h LineHead, a *lineAttrs) LogLine {
		return PhaseChanged{LineHead: h, Issue: a.str("issue"), Tree: a.str("tree"), From: phase.Phase(a.str("from")),
			To: phase.AwaitingChildren, Trigger: TriggerChildrenOpen}
	},
	workflow.ParentReturnsLine: func(h LineHead, a *lineAttrs) LogLine {
		return PhaseChanged{LineHead: h, Issue: a.str("issue"), Tree: a.str("tree"), From: phase.AwaitingChildren,
			To: phase.Phase(a.str("to")), Trigger: TriggerChildrenDone}
	},
	slotTakenLine: func(h LineHead, a *lineAttrs) LogLine {
		return SlotTaken{LineHead: h, Issue: a.str("issue"), Slots: a.count("slots"), Cap: a.count("cap")}
	},
	slotReleasedLine: func(h LineHead, a *lineAttrs) LogLine {
		return SlotReleased{LineHead: h, Issue: a.str("issue"), Slots: a.count("slots"), Cap: a.count("cap")}
	},
	passMovedSlotsLine: func(h LineHead, a *lineAttrs) LogLine {
		moved := PassMovedSlots{LineHead: h, Slots: a.count("slots"), Cap: a.count("cap")}
		a.list("given", "a list of slots given out", &moved.Given)
		a.list("taken", "a list of slots taken back", &moved.Taken)
		a.list("passed", "a list of candidates passed over", &moved.Passed)
		named := func(issue string) bool { return issue != "" }
		if a.err == nil && (slices.ContainsFunc(moved.Given, func(g record.SlotGiven) bool { return !named(g.Issue) }) ||
			slices.ContainsFunc(moved.Taken, func(t record.SlotTaken) bool { return !named(t.Issue) }) ||
			slices.ContainsFunc(moved.Passed, func(p record.SlotPassed) bool { return !named(p.Issue) })) {
			a.err = errors.New("names a slot or a candidate without its issue")
		}
		return moved
	},
	phaseHeldLine: func(h LineHead, a *lineAttrs) LogLine {
		return PhaseHeld{LineHead: h, Issue: a.str("issue"), Tree: a.str("tree"), From: phase.Phase(a.str("from")),
			Role: claim.Role(a.str("role")), Reason: a.str("reason")}
	},
	waitsOnIssuesLine: func(h LineHead, a *lineAttrs) LogLine {
		return WaitsOnIssues{LineHead: h, Issue: a.str("issue"), Blockers: a.strs("blockers")}
	},
	"admission gives a slot back to an issue whose work came back": func(h LineHead, a *lineAttrs) LogLine {
		return SlotBackWorkReturned{LineHead: h, Issue: a.str("issue"), Phase: phase.Phase(a.str("phase"))}
	},
	"admission gives a slot back to an issue whose wait ended": func(h LineHead, a *lineAttrs) LogLine {
		return SlotBackWaitEnded{LineHead: h, Issue: a.str("issue"), Phase: phase.Phase(a.str("phase"))}
	},
	waitsForChildrenLine: func(h LineHead, a *lineAttrs) LogLine {
		return WaitsForChildren{LineHead: h, Issue: a.str("issue"), Tree: a.str("tree"), Generation: a.generation("generation"),
			Children: a.count("children")}
	},
	"workflow: root waits on a person and keeps its admission slot": func(h LineHead, a *lineAttrs) LogLine {
		return WaitsOnPerson{LineHead: h, Issue: a.str("issue"), Tree: a.str("tree"), Reason: a.str("reason"), SinceSeq: a.seq("since_seq")}
	},
	gateOpenedLine: func(h LineHead, a *lineAttrs) LogLine {
		return GateOpened{LineHead: h, Issue: a.str("issue"), Artifact: a.str("artifact"), Event: a.str("event"), Version: a.count("version")}
	},
	gateClosedLine: func(h LineHead, a *lineAttrs) LogLine {
		return GateClosed{LineHead: h, Issue: a.str("issue"), Artifact: a.str("artifact"), Event: a.str("event"), Version: a.count("version")}
	},
	"workflow: design gate registered": func(h LineHead, a *lineAttrs) LogLine {
		// The older shapes (ParseLogLine): a line with neither step nor gated_steps is of the spec
		// alone and names no gated steps, and one without changes_requested too applied none.
		stepped := a.has("step") || a.has("gated_steps")
		registered := GateRegistered{LineHead: h, Issue: a.str("issue"), Artifact: a.str("artifact"), Step: record.DesignStepSpec}
		if stepped {
			registered.Step = record.DesignStep(a.str("step"))
		}
		registered.Version, registered.Open = a.count("version"), a.flag("open")
		if stepped || a.has("changes_requested") {
			registered.ChangesRequested = a.flag("changes_requested")
		}
		if stepped {
			for _, step := range a.strs("gated_steps") {
				registered.GatedSteps = append(registered.GatedSteps, record.DesignStep(step))
			}
		}
		return registered
	},
	"workflow: a held phase is released": func(h LineHead, a *lineAttrs) LogLine {
		return HoldReleased{LineHead: h, Issue: a.str("issue"), Phase: phase.Phase(a.str("phase")), Reason: a.str("reason")}
	},
	leftWorkflowLine: func(h LineHead, a *lineAttrs) LogLine {
		return LeftWorkflow{LineHead: h, Issue: a.str("issue"), Tree: a.str("tree"), Status: a.str("status")}
	},
	"workflow: tree lingers": func(h LineHead, a *lineAttrs) LogLine {
		return TreeLingers{LineHead: h, Tree: a.str("tree"), Generation: a.generation("generation"), Until: a.at("until")}
	},
	"workflow: linger closed the tree": func(h LineHead, a *lineAttrs) LogLine {
		return LingerClosed{LineHead: h, Tree: a.str("tree"), Generation: a.generation("generation"), Issues: a.count("issues")}
	},
	turnStartedLine: func(h LineHead, a *lineAttrs) LogLine {
		return TurnStarted{LineHead: h, Claim: a.claim("claim"), Generation: a.generation("generation"), Delivery: a.optionalStr("delivery")}
	},
	turnEndedLine: func(h LineHead, a *lineAttrs) LogLine {
		outcome := a.has("sequence")
		if outcome {
			a.seq("sequence")
		}
		return TurnEnded{LineHead: h, Claim: a.claim("claim"), Generation: a.generation("generation"), Outcome: outcome}
	},
	launchedLine: func(h LineHead, a *lineAttrs) LogLine {
		return Launched{LineHead: h, Claim: a.claim("claim"), Generation: a.generation("generation"), Resumed: a.flag("resumed")}
	},
	suspendedLine: func(h LineHead, a *lineAttrs) LogLine {
		return Suspended{LineHead: h, Claim: a.claim("claim"), By: a.str("by"), Reason: a.str("reason")}
	},
	claimFailedLine: func(h LineHead, a *lineAttrs) LogLine {
		// A line of a release that recorded no cause (ParseLogLine) has the hold's word for none.
		failed := ClaimFailed{LineHead: h, Claim: a.claim("claim"), Cause: string(supervise.CauseUnrecorded)}
		if a.has("cause") {
			failed.Cause = a.str("cause")
		}
		failed.Why = a.str("why")
		return failed
	},
	processDiedLine: func(h LineHead, a *lineAttrs) LogLine {
		cause := a.optionalStr("observed")
		if detail := a.optionalStr("detail"); cause != "" && detail != "" {
			cause = storetext.FarSideText(cause + ": " + detail)
		}
		return ProcessEnded{LineHead: h, Claim: a.claim("claim"), By: TurnEndDied, DeathCause: cause}
	},
	processDisplacedLine: func(h LineHead, a *lineAttrs) LogLine {
		return ProcessEnded{LineHead: h, Claim: a.claim("claim"), By: TurnEndDisplaced}
	},
	workEndedLine: func(h LineHead, a *lineAttrs) LogLine {
		return ProcessEnded{LineHead: h, Claim: a.claim("claim"), By: TurnEndLetGo}
	},
	launchFailedLine:     readStartFailure,
	launchStoreFaultLine: readStartFailure,
	launchPromptLine:     readStartFailure,
	checkClosedLine: func(h LineHead, a *lineAttrs) LogLine {
		return CheckClosed{LineHead: h, Issue: a.str("issue")}
	},
	"legion daemon started": func(h LineHead, a *lineAttrs) LogLine {
		return DaemonStarted{LineHead: h, Project: a.str("project"), AdmissionCap: a.count("admissionCap")}
	},
	"legion workflow boot stage": func(h LineHead, a *lineAttrs) LogLine {
		return BootStage{LineHead: h, Stage: a.str("stage")}
	},
	"workflow: a pull request left the merge queue where its issue does not merge at that head; nothing is read": func(h LineHead, a *lineAttrs) LogLine {
		return QueueLeftNothingRead{LineHead: h, Issue: a.str("issue"), PullRequest: a.count("pullRequest"), HeadSHA: a.str("head"), Why: a.str("why")}
	},
	"workflow: a pull request in merging left the merge queue; the daemon reads it from GitHub once": func(h LineHead, a *lineAttrs) LogLine {
		return QueueLeftReadOnce{LineHead: h, Issue: a.str("issue"), PullRequest: a.count("pullRequest"), HeadSHA: a.str("head")}
	},
	"legion daemon stopping": func(h LineHead, a *lineAttrs) LogLine {
		return ProcessStop{LineHead: h, Project: a.str("project")}
	},
	"legion daemon stopped": func(h LineHead, a *lineAttrs) LogLine {
		return ProcessStop{LineHead: h, Project: a.str("project")}
	},
	reconcileHoldingLine: func(h LineHead, a *lineAttrs) LogLine {
		return ReconcileHolding{LineHead: h, Count: a.count("count"), Target: a.seq("target"), AckFloor: a.seq("ack_floor")}
	},
	consumerCaughtUpLine: func(h LineHead, a *lineAttrs) LogLine {
		return ConsumerCaughtUp{LineHead: h, Count: a.count("count"), AckFloor: a.seq("ack_floor"), Idle: a.flag("idle"),
			Admitted: a.count("admitted")}
	},
	pauseBeganLine: func(h LineHead, a *lineAttrs) LogLine {
		return PauseBegan{LineHead: h, Pause: a.seq("pause"), Reason: a.str("reason")}
	},
	pauseEndedLine: func(h LineHead, a *lineAttrs) LogLine {
		return PauseEnded{LineHead: h, Pause: a.seq("pause")}
	},
	"workflow: a queued root joined a live tree": func(h LineHead, a *lineAttrs) LogLine {
		return RootJoined{LineHead: h, Issue: a.str("issue"), Tree: a.str("tree"), Generation: a.generation("generation")}
	},
	"workflow: a closed tree's member joined a live tree": func(h LineHead, a *lineAttrs) LogLine {
		return RootJoined{LineHead: h, Issue: a.str("issue"), Tree: a.str("tree"), Generation: a.generation("generation")}
	},
	mergeForwardAskedLine: func(h LineHead, a *lineAttrs) LogLine {
		return MergeForwardAsked{LineHead: h, Issue: a.str("issue"), Phase: phase.Phase(a.str("phase"))}
	},
	mergeForwardLandedLine: func(h LineHead, a *lineAttrs) LogLine {
		return MergeForwardLanded{LineHead: h, Issue: a.str("issue")}
	},
	mergedCloseWaitsLine: func(h LineHead, a *lineAttrs) LogLine {
		return MergedCloseWaits{LineHead: h, Issue: a.str("issue"), Tree: a.str("tree"), PullRequest: a.count("pullRequest"),
			WaitingFor: strings.Split(a.str("waitingFor"), ", ")}
	},
	mergeStateReadLine: func(h LineHead, a *lineAttrs) LogLine {
		return MergeStateRead{LineHead: h, Issue: a.str("issue"), Repo: a.str("repo"), PullRequest: a.count("pullRequest"), HeadSHA: a.str("head"),
			RecordedHead: a.str("recordedHead"), Base: a.str("base"), BaseSHA: a.str("baseSha"), Mergeable: a.str("mergeable"),
			MergeState: a.str("mergeState"), ReadAt: a.at("readAt")}
	},
	guardKillToldLine:       readGuardKill,
	guardKillUnrecordedLine: readGuardKill,
	guardKillNoRoleLine:     readNoRoleKill,
}

// readProcessStart reads one of the lines a daemon process starts with (ProcessStart), which
// carries nothing the trace reads but its head.
func readProcessStart(h LineHead, _ *lineAttrs) LogLine { return ProcessStart{LineHead: h} }

func readStartFailure(h LineHead, a *lineAttrs) LogLine {
	return StartFailure{LineHead: h, Claim: a.claim("claim"), Error: a.str("error")}
}

// readGuardKill reads one of the daemon's lines of a kill a launcher reported (GuardKillLine): its
// cause is absent from a release that predates the member, as the guard's own.
func readGuardKill(h LineHead, a *lineAttrs) LogLine {
	return GuardKillLine{LineHead: h, Issue: a.str("issue"), Claim: a.claim("role"), PID: a.count("pid"), Cause: a.optionalStr("cause")}
}

// noRoleKill is the role the memory guard's line names a process of no role by, as the launcher's
// line `launcher: memory guard: ended` writes it (memoryguard's process outside every role).
const noRoleKill claim.Role = "none"

// readNoRoleKill reads the daemon's line of a kill of a process of no role (guardKillNoRoleLine)
// as its record of the kill the launcher's line names of role none (GuardKillLine): the launcher
// reports such a kill on the connection of one role of its pod, so the connection's claim names
// the pod, and the kill's role is none, whichever role's connection carried it.
func readNoRoleKill(h LineHead, a *lineAttrs) LogLine {
	connection := a.claim("connection")
	connection.Role = noRoleKill
	return GuardKillLine{LineHead: h, Issue: a.str("issue"), Claim: connection, PID: a.count("pid"), Cause: a.optionalStr("cause")}
}

// lineReaderOf is the reader of the lines of msg (lineReaders, the process's start lines and the
// leak check's two lines by their common start), nil for a msg the trace does not read.
func lineReaderOf(msg string) func(LineHead, *lineAttrs) LogLine {
	if read, ok := lineReaders[msg]; ok {
		return read
	}
	if slices.Contains(processStartLines, msg) || strings.HasPrefix(msg, leakCheckLine) {
		return readProcessStart
	}
	return nil
}

// lineAttrs is the attributes of one log line, read by name and JSON type. The first attribute
// that is missing, null or of another type is kept in err and every read after it answers a zero
// value, so a reader reads its type's attributes in one expression and the line is refused once.
type lineAttrs struct {
	raw map[string]json.RawMessage
	err error
}

// has says whether the line carries the attribute name at all.
func (a *lineAttrs) has(name string) bool {
	_, ok := a.raw[name]
	return ok
}

// read decodes the attribute name into into, which what names for a refusal.
func (a *lineAttrs) read(name, what string, into any) {
	if a.err != nil {
		return
	}
	raw, ok := a.raw[name]
	switch {
	case !ok:
		a.err = fmt.Errorf("has no %s", name)
	case bytes.Equal(bytes.TrimSpace(raw), []byte("null")):
		a.err = fmt.Errorf("has a null %s, where %s is read", name, what)
	default:
		if err := json.Unmarshal(raw, into); err != nil {
			a.err = fmt.Errorf("has %s %s, where %s is read", name, raw, what)
		}
	}
}

func (a *lineAttrs) str(name string) (value string) {
	a.read(name, "a string", &value)
	return value
}

// optionalStr is the string attribute name, "" where the line carries none.
func (a *lineAttrs) optionalStr(name string) string {
	if !a.has(name) {
		return ""
	}
	return a.str(name)
}

func (a *lineAttrs) strs(name string) (value []string) {
	a.read(name, "a list of strings", &value)
	return value
}

// list decodes the list attribute name into into, which what names for a refusal. The daemon's
// JSON handler writes an empty list it holds as nil as null, so a null reads as no element; the
// attribute itself must be there.
func (a *lineAttrs) list(name, what string, into any) {
	if a.err != nil {
		return
	}
	if raw, ok := a.raw[name]; ok && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return
	}
	a.read(name, what, into)
}

func (a *lineAttrs) count(name string) (value int) {
	a.read(name, "a whole number", &value)
	return value
}

func (a *lineAttrs) seq(name string) (value int64) {
	a.read(name, "a whole number", &value)
	return value
}

func (a *lineAttrs) generation(name string) (value uint64) {
	a.read(name, "a whole number not below zero", &value)
	return value
}

func (a *lineAttrs) flag(name string) (value bool) {
	a.read(name, "true or false", &value)
	return value
}

// at is the attribute name as a time, written as RFC 3339 with or without a fraction of a second.
func (a *lineAttrs) at(name string) time.Time {
	text := a.str(name)
	if a.err != nil {
		return time.Time{}
	}
	at, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		a.err = fmt.Errorf("has %s %q, where an RFC 3339 time is read", name, text)
	}
	return at
}

// claim is the claim token in the attribute name, with the issue and role it names (ClaimRef).
func (a *lineAttrs) claim(name string) ClaimRef {
	token := a.str(name)
	if a.err != nil {
		return ClaimRef{}
	}
	ref, err := claimRefOf(claim.Token(token))
	if err != nil {
		a.err = fmt.Errorf("has %s %q: %w", name, token, err)
	}
	return ref
}

// claimRefOf reads the issue and role a claim token names: the role it ends in and the issue
// between the project's part and the role, `legion-<project>-<issue project>-<number>-<role>`
// (claim.NewToken), upper-cased as Dispatch writes a key; or the controller's role and no issue
// for the controller's token (claim.ControllerToken). A token of neither shape is refused.
func claimRefOf(token claim.Token) (ClaimRef, error) {
	role, ok := token.Role()
	if !ok {
		return ClaimRef{}, errors.New("it names no role of a claim")
	}
	ref := ClaimRef{Token: token, Role: role}
	if role == claim.RoleController {
		return ref, nil
	}
	issuePart, _, _ := token.Cut()
	rest, prefixed := strings.CutPrefix(issuePart, "legion-")
	_, issue, cut := strings.Cut(rest, "-")
	if key := strings.ToUpper(issue); prefixed && cut && claim.IsIssueKey(key) {
		ref.Issue = key
		return ref, nil
	}
	return ClaimRef{}, errors.New("it names no issue key between its project and its role")
}

// SessionClaim is the claim a session of Legion's runs, as the title Legion gives the session names
// it (`Legion <role> · <KEY>`, an actor's origin.session_title): the claim's issue and role.
type SessionClaim struct {
	Issue string
	Role  claim.Role
}

// legionTitle is the title of a Legion role's session, `Legion <role> · <issue or project>`: the
// role, and the issue of the claim the session runs or, for the controller, the project.
var legionTitle = regexp.MustCompile(`^Legion ([a-z]+) · (.+)$`)

// sessionClaimOf is the claim a session's title names, nil for a title that names none: the
// controller's, which names its project, and the title of a session outside Legion.
func sessionClaimOf(title string) *SessionClaim {
	match := legionTitle.FindStringSubmatch(title)
	if match == nil || !claim.IsRole(claim.Role(match[1])) || !claim.IsIssueKey(match[2]) {
		return nil
	}
	return &SessionClaim{Issue: match[2], Role: claim.Role(match[1])}
}

// actorClaimOf is the claim whose work event is an act of, and whether it is one: the claim its
// actor's session title names (DispatchEvent.ActorClaim), or the claim whose token a session
// actor's id is.
func actorClaimOf(event DispatchEvent) (SessionClaim, bool) {
	if event.ActorClaim != nil {
		return *event.ActorClaim, true
	}
	if event.Actor.Kind != "session" {
		return SessionClaim{}, false
	}
	ref, err := claimRefOf(claim.Token(event.Actor.ID))
	if err != nil || ref.Issue == "" {
		return SessionClaim{}, false
	}
	return SessionClaim{Issue: ref.Issue, Role: ref.Role}, true
}

// DispatchEvent is one event of an issue's Dispatch event log as the trace reads it
// (ParseDispatchEvent): the envelope every event carries, and the part of its payload the trace
// reads, in the one field its type fills. An event of a type the trace does not read carries its
// envelope alone.
type DispatchEvent struct {
	// ID is the event's id, which no other event of Dispatch has; Seq is its place in its issue's
	// log.
	ID  int64
	Seq int64
	// Key is the event's issue_key, and Project its project.
	Key     string
	Project string
	// Type is the event's type: issue.created, issue.updated, artifact.approved, ….
	Type string
	// Actor is who Dispatch names as having made the change: a person (user), a session, the
	// daemon's own session (dispatch.DaemonSession) or Dispatch itself (system).
	Actor dispatch.Actor
	// ActorClaim is the claim the actor's session runs, as its title names it (sessionClaimOf):
	// nil for an actor that is no session or carries no title, and for a session whose title
	// names no claim of an issue.
	ActorClaim *SessionClaim
	// At is the event's created_at.
	At time.Time
	// Issue is the issue as an issue.created, issue.updated or issue.closed event shows it; nil for
	// an event of another type.
	Issue *DispatchIssueState
	// Verdict is an artifact.approved or artifact.changes_requested event's verdict on its
	// document; nil for an event of another type.
	Verdict *DispatchVerdict
	// Ask is the ask an ask.opened, ask.answered or ask.resolved event opens, answers or resolves;
	// nil for an event of another type.
	Ask *DispatchAsk
	// Handoff is a handoff.recorded event's handoff; nil for an event of another type.
	Handoff *DispatchHandoff
	// Claim is the issue's Dispatch claim the event shows, `<kind>:<id>` ("" for nobody), where it
	// shows one (ClaimShown): the claim an issue event's payload carries, and an issue.claimed or
	// issue.released event's, read as the daemon's decoder reads them (intake.IssueEventClaim). A
	// claim that names no actor, and an issue.claimed that names none, show no claim.
	Claim      string
	ClaimShown bool
	// Attribution is an issue.pull_request_attribution_changed event's move of a pull request,
	// which collect-queue reads a project's pull requests from; nil for an event of another type.
	Attribution *DispatchAttribution
}

// eventClaim reads the claim of an event's payload by the daemon's decoder's own reading
// (intake.IssueEventClaim): `<kind>:<id>`, "" for null, and whether the payload shows it. An
// absent claim, one that is not a claim object and one that names no actor show none, as the
// daemon then writes no claim: such a claim is read as unknown and never refused. What the
// decoder would name of an unread claim is no fact of a replay's, and is dropped.
func eventClaim(raw json.RawMessage) (string, bool) {
	var unread []string
	return intake.IssueEventClaim(raw, &unread)
}

// DispatchIssueState is an issue as an issue event shows it once the event applied: the fields the
// event changed, and the state the trace reads the issue's Dispatch facts from.
type DispatchIssueState struct {
	// Changed is the names of the fields the event changed (its payload's changed), in
	// Dispatch's order.
	Changed []string
	Status  string
	// BlockedBy is the keys of the issues the issue waits on, in Dispatch's order.
	BlockedBy []string
	// Parent is the key of the issue's Dispatch parent; "" for an issue under none.
	Parent string
	// Rank is Dispatch's fractional key of the issue, compared as bytes.
	Rank   string
	Labels []string
	// Priority is the issue's own Dispatch priority as the record keeps it (record.Priority,
	// record.DispatchPriority): P0 to P3, and record.NoPriority for none, for an event that
	// carries none and for a level Dispatch never writes.
	Priority record.Priority
	// Resolved is the priority Dispatch resolved for the issue as the event shows it
	// (resolved_priority, spec R10): the issue's own, else that of the nearest issue above it on its
	// Dispatch parent chain that has one, a null as none. ShowsResolved says the event carries it: an
	// event of a Dispatch release before resolved_priority carries none, and the replay then
	// reconstructs it from the window's parent chains (resolvePriorities).
	Resolved      record.Priority
	ShowsResolved bool
}

// DispatchVerdict is a verdict on a version of an issue's document: its artifact, the document's
// name (spec.md, …), the version the verdict is on, and the reason a changes request gives ("" for
// an approval).
type DispatchVerdict struct {
	Artifact string
	Name     string
	Version  int
	Reason   string
}

// DispatchAsk is the ask an ask event opens, answers or resolves: its id, its kind (question,
// approval), its state once the event applied (open, answered, resolved), and the artifact of the
// document an approval asks about ("" for a question).
type DispatchAsk struct {
	ID       string
	Kind     string
	State    string
	Artifact string
}

// DispatchHandoff is a handoff a role recorded on its issue: its id, the phase it hands over (plan,
// …) and the version of that phase's handoff.
type DispatchHandoff struct {
	ID      string
	Phase   string
	Version int
}

// ParseDispatchEvent reads one event as Dispatch's issue event log serves it
// (GET /api/v1/issues/<KEY>/events, the shape intake.DecodeDispatchEvent reads), of any project.
// It refuses an event that is not one JSON object, that lacks its id, seq, issue_key, type, actor
// or created_at, or whose payload lacks a field its type's part reads: an issue event's status, a
// verdict's artifact_id and version, an ask's id, a handoff's handoff_id, phase and version, a
// pull request attribution's repo and number. An
// issue event's changed, blocked_by, parent, rank, labels, priority and resolved_priority are read
// where it carries them, as intake reads them, since an event retained from an older Dispatch lacks
// some; a field present with another JSON type is refused all the same. A priority outside
// Dispatch's levels 0 to 3 is none, as the daemon's decoder reads it (record.DispatchPriority). A
// session actor's title (origin.session_title) is read where the actor carries one (ActorClaim),
// and refused where it is of another JSON type.
// The claim an issue event, an issue.claimed or an issue.released event shows is read as the
// daemon's decoder reads it (Claim), and never refused. An event without its project is of the
// project its key begins with.
func ParseDispatchEvent(event []byte) (DispatchEvent, error) {
	var envelope struct {
		ID        *int64     `json:"id"`
		Seq       *int64     `json:"seq"`
		IssueKey  *string    `json:"issue_key"`
		Project   *string    `json:"project"`
		Type      *string    `json:"type"`
		CreatedAt *time.Time `json:"created_at"`
		Actor     *struct {
			Kind   *string `json:"kind"`
			ID     *string `json:"id"`
			Origin *struct {
				SessionTitle *string `json:"session_title"`
			} `json:"origin"`
		} `json:"actor"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(event, &envelope); err != nil {
		return DispatchEvent{}, fmt.Errorf("replay: a Dispatch event is not one JSON object of an event's fields: %w", err)
	}
	switch {
	case envelope.ID == nil, envelope.Seq == nil, envelope.IssueKey == nil, envelope.Type == nil, envelope.CreatedAt == nil,
		envelope.Actor == nil, envelope.Actor.Kind == nil, envelope.Actor.ID == nil:
		return DispatchEvent{}, fmt.Errorf("replay: the Dispatch event %s lacks its id, seq, issue_key, type, actor or created_at", event)
	}
	parsed := DispatchEvent{ID: *envelope.ID, Seq: *envelope.Seq, Key: *envelope.IssueKey, Type: *envelope.Type,
		Actor: dispatch.Actor{Kind: *envelope.Actor.Kind, ID: *envelope.Actor.ID}, At: *envelope.CreatedAt}
	if envelope.Project != nil {
		parsed.Project = *envelope.Project
	} else {
		parsed.Project, _, _ = strings.Cut(parsed.Key, "-")
	}
	if origin := envelope.Actor.Origin; parsed.Actor.Kind == "session" && origin != nil && origin.SessionTitle != nil {
		parsed.ActorClaim = sessionClaimOf(*origin.SessionTitle)
	}
	refuse := func(err error) (DispatchEvent, error) {
		return DispatchEvent{}, fmt.Errorf("replay: the %s event of %s at seq %d: %w", parsed.Type, parsed.Key, parsed.Seq, err)
	}
	switch parsed.Type {
	case "issue.created", "issue.updated", "issue.closed":
		var payload struct {
			Changed   []string        `json:"changed"`
			Status    *string         `json:"status"`
			BlockedBy []string        `json:"blocked_by"`
			Parent    *string         `json:"parent"`
			Rank      *string         `json:"rank"`
			Labels    []string        `json:"labels"`
			Priority  *int            `json:"priority"`
			Resolved  json.RawMessage `json:"resolved_priority"`
			Claim     json.RawMessage `json:"claim"`
		}
		if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
			return refuse(err)
		}
		if payload.Status == nil {
			return refuse(errors.New("its payload has no status"))
		}
		state := &DispatchIssueState{Changed: payload.Changed, Status: *payload.Status, BlockedBy: payload.BlockedBy, Labels: payload.Labels}
		state.Priority, _ = record.DispatchPriority(payload.Priority)
		if len(payload.Resolved) > 0 {
			resolved, _, err := record.DecodePriority(payload.Resolved)
			if err != nil {
				return refuse(fmt.Errorf("its resolved_priority: %w", err))
			}
			state.Resolved, state.ShowsResolved = resolved, true
		}
		if payload.Parent != nil {
			state.Parent = *payload.Parent
		}
		if payload.Rank != nil {
			state.Rank = *payload.Rank
		}
		parsed.Issue = state
		parsed.Claim, parsed.ClaimShown = eventClaim(payload.Claim)
	case "issue.claimed", "issue.released":
		var payload struct {
			Claim json.RawMessage `json:"claim"`
		}
		if json.Unmarshal(envelope.Payload, &payload) == nil {
			parsed.Claim, parsed.ClaimShown = eventClaim(payload.Claim)
		}
		if parsed.Type == "issue.claimed" && parsed.Claim == "" {
			parsed.ClaimShown = false
		}
	case "artifact.approved", "artifact.changes_requested":
		var payload struct {
			ArtifactID *string `json:"artifact_id"`
			Name       *string `json:"name"`
			Version    *int    `json:"version"`
			Reason     *string `json:"reason"`
		}
		if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
			return refuse(err)
		}
		if payload.ArtifactID == nil || payload.Version == nil {
			return refuse(errors.New("its payload has no artifact_id or no version"))
		}
		verdict := &DispatchVerdict{Artifact: *payload.ArtifactID, Version: *payload.Version}
		if payload.Name != nil {
			verdict.Name = *payload.Name
		}
		if payload.Reason != nil {
			verdict.Reason = *payload.Reason
		}
		parsed.Verdict = verdict
	case "ask.opened", "ask.answered", "ask.resolved":
		var payload struct {
			ID       *string `json:"id"`
			Kind     string  `json:"kind"`
			State    string  `json:"state"`
			Approval *struct {
				ArtifactID string `json:"artifact_id"`
			} `json:"approval"`
		}
		if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
			return refuse(err)
		}
		if payload.ID == nil {
			return refuse(errors.New("its payload has no id"))
		}
		ask := &DispatchAsk{ID: *payload.ID, Kind: payload.Kind, State: payload.State}
		if payload.Kind == record.AskApproval && payload.Approval != nil {
			ask.Artifact = payload.Approval.ArtifactID
		}
		parsed.Ask = ask
	case "handoff.recorded":
		var payload struct {
			ID      *string `json:"handoff_id"`
			Phase   *string `json:"phase"`
			Version *int    `json:"version"`
		}
		if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
			return refuse(err)
		}
		if payload.ID == nil || payload.Phase == nil || payload.Version == nil {
			return refuse(errors.New("its payload has no handoff_id, phase or version"))
		}
		parsed.Handoff = &DispatchHandoff{ID: *payload.ID, Phase: *payload.Phase, Version: *payload.Version}
	case attributionEvent:
		var payload struct {
			Repo   *string `json:"repo"`
			Number *int    `json:"number"`
			From   *string `json:"from"`
			To     *string `json:"to"`
			Source string  `json:"source"`
		}
		if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
			return refuse(err)
		}
		if payload.Repo == nil || payload.Number == nil {
			return refuse(errors.New("its payload has no repo or number"))
		}
		attribution := &DispatchAttribution{Repo: *payload.Repo, Number: *payload.Number, Source: payload.Source}
		if payload.From != nil {
			attribution.From = *payload.From
		}
		if payload.To != nil {
			attribution.To = *payload.To
		}
		parsed.Attribution = attribution
	}
	return parsed, nil
}

// ParseMerges reads pages of GitHub's commit listing of one base branch
// (GET /repos/<owner>/<name>/commits?sha=<branch>&since=<t>, each page newest first, as GitHub
// serves them) and returns the branch's first-parent commits among them, oldest first, as the
// listing alone gives them: each at its committer's time, with branch as its Branch and, where the
// first line of its message ends with GitHub's squash suffix ` (#<n>)`, the pull request it names
// as its PR, and no Event. That time is an estimate of when the commit landed, and that number a
// hint, which CollectMerges holds to GitHub's record of the merge (githubReader.mergeEvent). The
// commits are the walk from the first page's first commit, the branch's tip, along each commit's
// first parent through every listed commit, whatever its time, since a first parent can be
// committed after its child. The walk ends at a root commit or at the first commit whose first
// parent no page holds, and ParseMerges also returns that parent, "" where the walk ended at a
// root or walked nothing. No page, or an empty first page, is a branch with no commit listed. A
// page that is not a list of commits, or a commit without its sha or committer's time, is refused.
func ParseMerges(branch string, pages ...[]byte) ([]Merge, string, error) {
	type listed struct {
		SHA    string `json:"sha"`
		Commit struct {
			Message   string `json:"message"`
			Committer struct {
				Date time.Time `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
		Parents []struct {
			SHA string `json:"sha"`
		} `json:"parents"`
	}
	bySHA := make(map[string]listed)
	var head string
	for i, page := range pages {
		var commits []listed
		if err := json.Unmarshal(page, &commits); err != nil {
			return nil, "", fmt.Errorf("replay: page %d of the commits of %s is not a list of commits: %w", i+1, branch, err)
		}
		for _, commit := range commits {
			if commit.SHA == "" || commit.Commit.Committer.Date.IsZero() {
				return nil, "", fmt.Errorf("replay: page %d of the commits of %s lists a commit without its sha or its committer's time", i+1, branch)
			}
			bySHA[commit.SHA] = commit
		}
		if i == 0 && len(commits) > 0 {
			head = commits[0].SHA
		}
	}
	var merges []Merge
	var lacking string
	for sha := head; sha != ""; {
		commit, listed := bySHA[sha]
		if !listed {
			lacking = sha
			break
		}
		delete(bySHA, sha) // a commit is walked once, whatever a page says of its parents
		merges = append(merges, Merge{SHA: commit.SHA, At: commit.Commit.Committer.Date, Branch: branch, PR: squashedPullRequest(commit.Commit.Message)})
		sha = ""
		if len(commit.Parents) > 0 {
			sha = commit.Parents[0].SHA
		}
	}
	slices.Reverse(merges)
	return merges, lacking, nil
}

// squashSuffix is GitHub's suffix of a squash merge's subject, the pull request's number.
var squashSuffix = regexp.MustCompile(` \(#([0-9]+)\)$`)

// squashedPullRequest is the pull request a commit message's first line names by GitHub's squash
// suffix ` (#<n>)`, 0 for a first line that does not end with one.
func squashedPullRequest(message string) int {
	subject, _, _ := strings.Cut(message, "\n")
	found := squashSuffix.FindStringSubmatch(strings.TrimRight(subject, "\r"))
	if found == nil {
		return 0
	}
	number, err := strconv.Atoi(found[1])
	if err != nil {
		return 0
	}
	return number
}
