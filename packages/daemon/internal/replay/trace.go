package replay

import (
	"errors"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// TriggerReady is the merger's ready move of the release at 9542c6fb, which took an issue from
// merging into PhaseAwaitingMerge once the merger had readied its pull request. Today's workflow
// has no such move: a window of that release holds it, and the trace reads it as a mark inside
// merging (MoveSources, assumption 20), where the rule infra-w2 gives the slot back.
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
	// replay as they held in the record.
	MoveFact MoveWay = "fact"
	// MovePass is a move the daemon's own admission pass makes from the state a replay reaches,
	// as gate_opened is the promotion of an issue whose first phase the line lets start. A replay
	// never injects it: the recorded move only measures how far the replay's moment is from the
	// record's.
	MovePass MoveWay = "pass"
	// MoveDraw is a move a replay's conflict draw makes, at the rate fitted to the window. The
	// recorded moves of the trigger are not injected, or each conflict would count twice.
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
var MoveSources = map[workflow.TriggerKind]MoveSource{}

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
// held where its phase's worker failed was put back in Phase, for Reason.
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
// phase held (workflow's claimFailed), which writes no line of its own, so the trace reads the
// hold's start from this line and a replay injects intake.ClaimFailed at its offset.
type ClaimFailed struct {
	LineHead
	Claim ClaimRef
	Cause string
	Why   string
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

// ParseLogLine reads one daemon log line as the daemon's JSON handler writes it on stderr (one
// JSON object of time, level, msg and the attributes; a Cloud Logging export's jsonPayload) as the
// type of its kind. A line of a msg the trace does not read is nil with no error: the trace skips
// it. A line that is not one JSON object, or has no time, is refused, and so is a line of a kind it
// reads that lacks an attribute its type reads or carries one of another JSON type, so an attribute
// the daemon renames fails the read rather than reading as a zero value. The two attributes whose
// absence a field's doc gives a meaning, TurnStarted.Delivery and TurnEnded.Outcome, are the
// exceptions.
func ParseLogLine(line []byte) (LogLine, error) {
	return nil, errors.New("replay: not built yet")
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
	// At is the event's created_at.
	At time.Time
	// Issue is the issue as an issue.created, issue.updated or issue.closed event shows it; nil for
	// an event of another type.
	Issue *DispatchIssueState
	// Verdict is an artifact.approved or artifact.changes_requested event's verdict on its
	// document; nil for an event of another type.
	Verdict *DispatchVerdict
	// Ask is the ask an ask.opened or ask.answered event opens or answers; nil for an event of
	// another type.
	Ask *DispatchAsk
	// Handoff is a handoff.recorded event's handoff; nil for an event of another type.
	Handoff *DispatchHandoff
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

// DispatchAsk is the ask an ask event opens or answers: its id, its kind (question, approval), its
// state once the event applied (open, answered), and the artifact of the document an approval asks
// about ("" for a question).
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
// or created_at, or whose payload lacks a field its type's part reads.
func ParseDispatchEvent(event []byte) (DispatchEvent, error) {
	return DispatchEvent{}, errors.New("replay: not built yet")
}

// ParseMerges reads pages of GitHub's commit listing of one base branch
// (GET /repos/<owner>/<name>/commits?sha=<branch>&since=<t>&until=<t>, each page newest first, as
// GitHub serves them) and returns the branch's first-parent commits among them, oldest first, each
// at its committer's time, which is when a merge landed it on the branch, and with branch as its
// Branch: the walk from the first page's first commit along each commit's first parent, which ends
// at the first commit whose first parent no page holds.
func ParseMerges(branch string, pages ...[]byte) ([]Merge, error) {
	return nil, errors.New("replay: not built yet")
}
