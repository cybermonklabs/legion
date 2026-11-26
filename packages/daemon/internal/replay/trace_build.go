package replay

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// Trace is a recorded window read back as what happened to each issue and to the daemon that ran
// it (BuildTrace): the issues' phase visits and the work segment of each, their slots, waits,
// turns, gates and Dispatch facts, every slot the admission passes moved, and the daemon's boots,
// downtimes and ticks. It is the one reading of a window's record that the replay's driver, its
// start state (Seed) and the measures share, so a replay and its record are read alike, and what
// it cannot place it names (Unplaced) rather than leaving out.
type Trace struct {
	// Project is the window's Dispatch project, From and To its span, which the measures read, and
	// End where its inputs end (Window.End): To, or the end of its run-out past To, to which a replay
	// runs.
	Project       string
	From, To, End time.Time
	// Start is the earliest moment the window's inputs reach, From less Before: the log holds
	// nothing older, so a phase or a slot an issue stood in at Start began at an unknown time.
	Start time.Time
	// Parts, GateSteps and TickInterval are the window's own (Window), and rules the slot rule of
	// each part, by its index, as the repository keeps it (ruleAt).
	Parts        []Part
	rules        []record.SlotRule
	GateSteps    []record.DesignStep
	TickInterval time.Duration
	// GateLines says the window holds the daemon's lines of a design gate's opening and closing
	// (gateOpenedLine, gateClosedLine), so its gates are read from them (gateReader).
	GateLines bool
	// Branches is the base branch of each repository the window's merges name, by owner/repo: the
	// default branch a start state records an issue of that repository on.
	Branches map[string]string
	// Issues is every issue a line or an event of the window names, by key, of any project.
	Issues map[string]*IssueTrace
	// Slots is every slot an admission pass gave out or took back, in the order the passes moved
	// them: from the record line of each pass where the window holds that kind, and from the
	// per-slot lines where it does not (check k9).
	Slots []SlotMove
	// Passed is every candidate of the line a pass passed over, which the record line alone names:
	// none for a window that does not hold that kind.
	Passed []PassedOver
	// InferredSlots is how many slots the trace infers an issue held that no slot line names
	// (SlotSpan.Inferred, assumption 34): from before the reach, or at the window's start, and
	// SlotCount the check of the slots it holds, seen and inferred, against the record's own count
	// at the window's start.
	InferredSlots int
	SlotCount     SlotCount
	// Boots is each daemon process, in order: each start the log holds, and, where the log opens
	// with no start, the process it opens in. Downtimes is each stretch between the last line of one
	// process and the start of the next, when no daemon ran a pass (assumption 24).
	Boots     []Boot
	Downtimes []Downtime
	// Ticks is each controller tick the daemon ran: a minute after each boot and every
	// TickInterval after that, until the next boot or To.
	Ticks []time.Time
	// Pauses is each pause of the project the log shows, in order.
	Pauses []PauseSpan
	// claimCauses holds the log evidence of runtime deaths and refused starts, in recorded order
	// per claim. These observations are not intake facts and do not move work.
	claimCauses map[claim.Token][]claimCause
	// Unplaced is each issue or line the trace could not place, with what part of the record it is
	// about and the reason: a line that disagrees with what the trace already holds, a move whose
	// trigger has no source, a turn end with no start. The trace still places what it can of it.
	Unplaced []UnplacedFact
	// shownPriorities, where set, is the priority Dispatch resolves for each issue, which the
	// replay's copy of Dispatch shows in place of the issue's own under a rule with the new P0 start
	// (record.SlotRule.P0AboveCap, shadow.priorities): the start state records each issue at the
	// one standing just before its moment (seeder.startRecord). Nil for a record's own trace and
	// under every other rule, whose start state records each issue's own priority.
	shownPriorities *resolvedPriorities
}

type claimCause struct {
	at           time.Time
	text         string
	startFailure bool
}

// seedDeath dates the last death of a seeded live claim from the reach, not from the seed time.
// A start failure does not survive the readiness implied by a seeded live claim.
func (t Trace) seedDeath(c *supervise.Claim, before time.Time) {
	for _, cause := range slices.Backward(t.claimCauses[c.Token]) {
		if !cause.startFailure && cause.at.Before(before) {
			c.LastDeathCause, c.LastDeathAt = cause.text, cause.at
			return
		}
	}
}

// UnplacedFact is a line or an issue the trace could not place: the issue it is about ("" for
// none), the moment of the line or the fact, the part of the record it is about, and why. The
// start state names each issue a fact before its moment leaves in doubt (Seed).
type UnplacedFact struct {
	At     time.Time
	Issue  string
	Kind   UnplacedKind
	Reason string
}

// UnplacedKind is the part of the record an unplaced fact is about, which says what of a start
// state the fact can leave in doubt (Seed).
type UnplacedKind string

const (
	// UnplacedTurn is a fact of a claim's turn: a turn that starts while the claim's turn before
	// is open, which ends that turn there, or a turn's own end line with no turn of the claim open,
	// which ends none. Either is of a turn that ended at the fact's own moment.
	UnplacedTurn UnplacedKind = "turn"
	// UnplacedMove is a fact of a phase move: a move out of a phase the trace does not hold the
	// issue in, a move whose trigger has no source in MoveSources, a move back with no cause, or a
	// merge whose close waited (MergedCloseWaits) of an issue the trace holds in no visit to merging.
	UnplacedMove UnplacedKind = "move"
	// UnplacedEntry is a fact of an issue's entry into the workflow: a move of an issue none of
	// whose Dispatch events entered it, or an issue in the workflow the window holds no Dispatch
	// event of.
	UnplacedEntry UnplacedKind = "entry"
	// UnplacedSlot is a fact of a slot: one given to an issue that holds one.
	UnplacedSlot UnplacedKind = "slot"
	// UnplacedHold is a fact of a hold: the release of one the trace does not hold, but for the
	// operator's retry of a phase's worker, which writes a release where no hold stands too.
	UnplacedHold UnplacedKind = "hold"
	// UnplacedLinger is a fact of a linger: the close of one the trace does not hold.
	UnplacedLinger UnplacedKind = "linger"
	// UnplacedBoot is a fact of a boot's hold on the listing's summaries: the release of one no
	// boot of the trace holds.
	UnplacedBoot UnplacedKind = "boot"
	// UnplacedPause is a fact of a pause of the project: the end of one the trace does not hold
	// open.
	UnplacedPause UnplacedKind = "pause"
)

// IssueTrace is one issue of a window as its record shows it.
type IssueTrace struct {
	// Key is the issue's key, and Project the Dispatch project its events name (the window's for
	// an issue only the log names).
	Key, Project string
	// Title is the issue's title as its newest issue event shows it, and Repo the repository
	// Dispatch resolves it to (resolved_repo, or repo where none is resolved) at the first issue
	// event that named one: the repository the daemon records it on. Priority is its Dispatch
	// priority as the newest issue event that carries the member writes it ("" for none), which a
	// drawn scenario draws by (script.drawNeverStarted).
	Title, Repo, Priority string
	// Entries is each time the issue entered the workflow: its first entry and each reopen.
	Entries []Entry
	// Visits is the issue's phase visits in order, each run's from its entry at admitted to its
	// move to done or its leave.
	Visits []Visit
	// Slots is each admission slot the issue held, in order.
	Slots []SlotSpan
	// Waits is each wait of the issue, by the time it began.
	Waits []Wait
	// Turns is each turn of a worker of the issue, of any role, by the time it began.
	Turns []Turn
	// Gates is each fact of the issue's design gate, from the gate lines and the verdicts on its
	// document, in time order.
	Gates []GateFact
	// Dispatch is the issue's Dispatch events in the order its log holds them.
	Dispatch []DispatchEvent
	// Children is each issue filed under this one, or moved under it, in time order.
	Children []ChildFiling
	// Leaves is each time the issue left the workflow (`workflow: issue left the workflow`).
	Leaves []Leave
	// Lingers is each linger of the tree this issue roots.
	Lingers []Linger
	// Ready is each merger's ready mark of the release at 9542c6fb (TriggerReady), a mark inside
	// merging (assumption 20).
	Ready []time.Time
	// QueueDrops is each line of the issue's pull request leaving the merge queue.
	QueueDrops []QueueDrop
	// MergeForwards is each merge forward of the issue's conflicting head, in order, the issue
	// keeping its phase.
	MergeForwards []MergeForwardRound
	// MergeStateReads is each read of the merge state of the issue's pull request the log holds
	// (MergeStateRead), in the log's order. The replay decides nothing from them; a window
	// collected before the daemon wrote the line holds none.
	MergeStateReads []MergeStateRead
	// LinkedPullRequests is the number of the pull request the issue's Dispatch events link, by
	// its repository as owner/repo in lower case: the newest issue event's link of the form
	// https://github.com/<owner>/<repo>/pull/<n> among its external links, for each repository.
	// The conflict draw leaves the merge of that pull request out of the issue's stretches
	// (Stretch); nil where no event links one.
	LinkedPullRequests map[string]int
	// PullRequest is when the trace first sees the issue's pull request: its first move by
	// implementation_ready, its first visit past implementing, or the first line that names its
	// pull request, whichever is first; zero for none. A replay keeps a synthetic open pull
	// request record from then on, since a guard of the daemon reads one.
	// PullRequestNumber is the number a line names, 0 where none does.
	PullRequest       time.Time
	PullRequestNumber int
	// Sessions is each session the record shows running a claim of the issue, in the order of
	// their first events: a session that wrote an event of any issue under a title naming a role's
	// claim of this one (DispatchEvent.ActorClaim). A claim runs a fresh session where its agent
	// was started anew, so one role may have several.
	Sessions []RecordedSession
	// Launches is each launch of a claim of the issue (`supervise: launched`), in order, and
	// FreshRetries each of the operator's retries of a phase's worker with a fresh session where no
	// hold stood (traceBuilder.freshRetried).
	Launches     []Launch
	FreshRetries []FreshRetry
}

// Launch is one launch of a claim of an issue: its moment, the claim's role, whether it resumed the
// claim's session, whether it ended a hold of that role's claim the record shows no release of
// (traceBuilder.holdWentOn), and how many turns of the issue's workers the record holds before it:
// the turns IssueTrace.Turns[Turns:] began after it, by the order of the log, which no moment tells
// where a launch and a turn come at one instant (traceBuilder.freshRetried).
type Launch struct {
	At        time.Time
	Role      claim.Role
	Resumed   bool
	EndedHold bool
	Turns     int
}

// FreshRetry is the operator's retry of the worker of an issue's phase with a fresh session where
// no hold stood (`legion claims retry --fresh-session` of a suspended claim): its moment, the phase
// it retries, on today's phases, and the role of that phase's worker. At 958173da it launched the
// worker's claim outside the outbox's start fence and then restarted the phase, whose own start
// the fence held (api's freshSession), which a replay enters at its offset from the start of the
// visit it retries (driver.freshRetry).
type FreshRetry struct {
	At    time.Time
	Phase phase.Phase
	Role  claim.Role
}

// RecordedSession is one session the record shows running a claim: the session's id, which
// Dispatch names an event's actor by and the daemon's claims table holds, the role of the claim,
// and the moment of the session's first event in the window under that claim.
type RecordedSession struct {
	ID    string
	Role  claim.Role
	First time.Time
}

// sessionAt is the session the record shows running iss's role claim at at: the latest whose first
// event is at or before at, else the first one after it; "" where the record shows none.
func (iss *IssueTrace) sessionAt(role claim.Role, at time.Time) string {
	found := ""
	for _, session := range iss.Sessions {
		if session.Role != role {
			continue
		}
		if found != "" && session.First.After(at) {
			break
		}
		found = session.ID
	}
	return found
}

// MergeForwardRound is one merge forward of an issue's conflicting head that keeps its phase, under
// a rule whose conflicting head keeps it or, since cybermonklabs/legion#4979, past the review under
// every rule, a round of conflict that moves no phase: from the engine's ask (MergeForwardAsked) to
// the landing of the head it left (MergeForwardLanded), or to the issue's leave; End zero while it
// stands at the window's end. An ask while one stands is of the same round. Phase is the phase the
// issue stood in at the round's ask: merging for a merger's merge forward, which counts its stretch
// as one that conflicted (recordedStretches).
type MergeForwardRound struct {
	Start, End time.Time
	Phase      phase.Phase
}

// Entry is one entry of an issue into the workflow at admitted: the Dispatch event that made it a
// queued root (todo, handed to Legion) or a child of a live tree (todo under a member of one), or
// the reopen of an issue that had left, with the tree it entered and the generation its record
// stood at.
type Entry struct {
	At         time.Time
	Tree       string
	Generation uint64
	// Reopen is an entry after the issue had left the workflow once.
	Reopen bool
}

// Visit is one stay of an issue in one phase, read on today's phases (a phase MoveSources folds
// is read as the phase it folds into). Enter is when it began and Exit when it ended, zero while
// it stands at the window's end.
type Visit struct {
	Phase       phase.Phase
	Enter, Exit time.Time
	// Before is a visit the issue already stood in where the log begins (Trace.Start): it began
	// at an unknown time, and Enter is Start.
	Before bool
	// In is the move that began the visit, nil for a visit that began with the issue's entry into
	// the workflow or Before the log; Out is the move that ended it, nil while it stands or where
	// it ended otherwise.
	In, Out *Move
	// Left is a visit that ended with the issue's leaving the workflow and no move.
	Left bool
	// WorkerStart is when the visit's worker started: the later of the phase change and the slot
	// the issue takes for it; the phase change in a phase whose rule keeps no slot; the first turn
	// of the phase's role where the issue held no slot in it. Zero where no worker started.
	WorkerStart time.Time
	// Segment is the visit's work segment, nil where no worker started.
	Segment *WorkSegment
	// Workerless is a visit to a phase that a worker works, the architect excepted, whose work
	// starts at the phase change and in which the record shows nothing of the phase's role at
	// work: no launch of its claim, no turn of it and no hold of it (IssueTrace.workerShownIn). No
	// start of the role opened its work in the record, so a replay opens its piece on the move into
	// the phase (openingOf), and its recorded end, a conflict among them, does not wait for a start
	// that the replay may hold, as it holds a merger's while its tree's design gate stands closed.
	Workerless bool
	// Merge is the merge of the issue's pull request inside a visit to merging whose close waited
	// for the issues beneath it (MergedCloseWaits, the first line of the visit's): the move the
	// merge would have made, from merging to done by pull_request_merged at the line's time, which
	// a replay applies at the end of the visit's work segment (SegmentEndHeldMerge); the engine
	// closes the issue as the last of them leaves, the visit's Out. Nil where no close waited.
	Merge *Move
}

// workEnd is the recorded move the end of the visit's work segment enters: its merge where the
// segment ended at it (SegmentEndHeldMerge), and otherwise the move that ended the visit; nil for
// neither.
func (v *Visit) workEnd() *Move {
	if v.Segment != nil && v.Segment.EndedBy == SegmentEndHeldMerge {
		return v.Merge
	}
	return v.Out
}

// Move is one recorded phase move (`workflow: phase changed`): its moment, its phases on today's
// workflow and as the line wrote them, its trigger, how a replay makes it (MoveSources) and, for a
// move back, its cause (CauseOf).
type Move struct {
	At                       time.Time
	From, To                 phase.Phase
	RecordedFrom, RecordedTo phase.Phase
	Trigger                  workflow.TriggerKind
	Source                   MoveSource
	Cause                    Cause
	// Drop is the msg of the line of the issue's pull request leaving the merge queue that came
	// before a move out of merging, in the visit it ended; "" for none.
	Drop string
}

// SegmentEnd is how a work segment ended.
type SegmentEnd string

const (
	// SegmentEndMove is a segment ended by the recorded move that ended its visit.
	SegmentEndMove SegmentEnd = "move"
	// SegmentEndRegistration is a segment whose visit the admission pass ended (gate_opened): the
	// worker's own work ended at its last design gate registration in the visit.
	SegmentEndRegistration SegmentEnd = "registration"
	// SegmentEndTurn is a segment whose visit the admission pass ended, its worker having
	// registered no gate: its work ended with its last turn.
	SegmentEndTurn SegmentEnd = "turn"
	// SegmentEndLeft is a segment cut by the issue's leaving the workflow.
	SegmentEndLeft SegmentEnd = "left"
	// SegmentOpen is a segment the record shows unended at the window's end: its length is not
	// known.
	SegmentOpen SegmentEnd = "open"
	// SegmentEndHeldSignOff is an architect's segment in accepting whose sign-off the engine held
	// while a member beneath was in the workflow: the work ended with the architect's last turn, and
	// the sign-off moved the issue at the member's leave (the windows' README, "Work segments").
	SegmentEndHeldSignOff SegmentEnd = "held_sign_off"
	// SegmentEndHeldMerge is a merger's segment in merging whose merge left the issue there, its
	// close waiting for the issues beneath it still in the workflow (Visit.Merge): the work ended at
	// the merge, and the engine closed the issue at the last one's leave, the visit's recorded move.
	SegmentEndHeldMerge SegmentEnd = "held_merge"
)

// WorkSegment is one visit's work: from its worker start to its end (zero while open), and how it
// ended. A replay places each turn from the issue's own (IssueTrace.Turns, placeTurns) and each
// child's filing from the recorded event that made it.
type WorkSegment struct {
	Start, End time.Time
	EndedBy    SegmentEnd
}

// SlotMove is one slot an admission pass gave out or took back: the issue, the moment, and the
// slots in use once the move was made under the cap the pass ran with. Kind is the kind of entry
// a slot given out went to (record.LineKind's word) where the record says it, and Reason the path
// that took a slot back where the record line says it.
type SlotMove struct {
	At         time.Time
	Issue      string
	Given      bool
	Slots, Cap int
	Kind       string
	Reason     record.SlotTakenReason
}

// SlotSpan is one slot an issue held, from its Start to its End (zero while held at the window's
// end). Before is a slot the issue already held where the log begins: one its first slot line gives
// back, or one no slot line names that the trace infers from the phase the log first shows it in.
// Inferred is a slot no slot line names that the trace infers (assumption 34): one held from before
// the reach (inferSlots), or one an issue that stands at the window's start in a worker's phase that
// keeps a slot took again by a way no line shows (inferStartSlots). Kind is the kind of entry of the
// waiting line the slot was given to (record.LineKind's word), where the record says it
// (SlotMove.Kind), and "" where no line says it. Reason is the path that took it back where the
// record line says it, and ForWait a slot taken back because the issue waited on another issue (a
// link or its children), read from the reason or from a wait the trace holds at that moment: the
// issue then stands in the line for a slot of its own again.
type SlotSpan struct {
	Start, End time.Time
	Before     bool
	Inferred   bool
	Kind       string
	Reason     record.SlotTakenReason
	ForWait    bool
}

// SlotCount is the check of the slots the trace holds against the record's own count of them at the
// window's start, where the start state places the slots (assumption 34). Each line of a slot
// move, `admission: slot taken|released` and a pass's record line alike, carries the slots the
// project held once the move was made (SlotMove.Slots), so at the first slot move the log holds at
// or after the window's start, or at the last before it where it holds none from then on, the slots
// the trace holds right after it, each seen in a line or inferred, are counted beside that count.
type SlotCount struct {
	// At is the moment of the slot move the count is checked at, zero where the log holds none.
	At time.Time
	// Recorded is the slots in use that move's line says the project held once it was made, and
	// Held the slots the trace holds right after it.
	Recorded, Held int
}

// Difference is the slots the trace holds right after the slot move at the window's start less the
// slots its line says were in use: zero where the trace accounts for every slot the record held
// there, below zero by each slot the record held for an issue the trace holds none for, and above
// it by each slot the trace holds that the record did not.
func (c SlotCount) Difference() int { return c.Held - c.Recorded }

// PassedOver is one candidate of the line a pass passed over, as its record line names it.
type PassedOver struct {
	At     time.Time
	Issue  string
	Kind   string
	Place  int
	Reason record.SlotPassedReason
}

// WaitKind is what an issue waits on.
type WaitKind string

const (
	// WaitLink is a started issue, before its review, waiting on an unfinished Dispatch
	// blocked_by link.
	WaitLink WaitKind = "link"
	// WaitChildren is an issue whose own first phase waits for its open Dispatch children (the
	// children-first rule).
	WaitChildren WaitKind = "children"
	// WaitPerson is a root waiting on a person's answer to an ask or review of its spec.
	WaitPerson WaitKind = "person"
	// WaitHeld is an issue held where its phase's worker failed, until its architect or the
	// operator releases it, or the record shows its phase going on with no release (WaitWentOn).
	WaitHeld WaitKind = "held"
)

// WaitEnd is what ended a wait.
type WaitEnd string

const (
	// WaitBlockerDone is a link wait whose last unfinished blocker reached done.
	WaitBlockerDone WaitEnd = "blocker_done"
	// WaitLinkRemoved is a link wait whose last unfinished link was taken off.
	WaitLinkRemoved WaitEnd = "link_removed"
	// WaitChildDone is a children wait whose last open child closed, done or icebox.
	WaitChildDone WaitEnd = "child_done"
	// WaitChildMovedOut is a children wait whose last open child moved from under the issue.
	WaitChildMovedOut WaitEnd = "child_moved_out"
	// WaitGateClosed is a children wait its tree's design gate ended by closing: a new version of
	// the spec.
	WaitGateClosed WaitEnd = "gate_closed"
	// WaitAnswered is a person's act on what the root handed back: an answer, a review, a newer
	// version of the document.
	WaitAnswered WaitEnd = "answered"
	// WaitReleased is a held phase released (`workflow: a held phase is released`).
	WaitReleased WaitEnd = "released"
	// WaitWentOn is a hold the record shows over with no line of its release: the next launch of a
	// claim of the issue in the hold's role, which ran the held phase's worker again, or the
	// issue's next recorded move, which names a phase it stood in other than held
	// (traceBuilder.holdWentOn).
	WaitWentOn WaitEnd = "went_on"
	// WaitMovedOn is a wait that stood when the issue's own phase moved on, which no rule the
	// daemon runs lets happen; the trace ends the wait there rather than carry it on.
	WaitMovedOn WaitEnd = "moved_on"
	// WaitLeft is a wait whose issue left the workflow, or whose run ended at done.
	WaitLeft WaitEnd = "left_workflow"
	// WaitOpen is a wait that stands at the window's end.
	WaitOpen WaitEnd = "open"
)

// Wait is one wait of an issue: its kind, when it began and ended (End zero while it stands),
// what ended it, and what it is on: the unfinished blockers it began on (link); the ask or the spec
// version and document it waits on and the Dispatch sequence it stands since (person); the phase
// held, the role whose claim failed and why (held). Told is a children wait the daemon's line
// began, which it writes as it tells the wait (admit's tellChildrenFirst), not one the trace reads
// again with no line of its own (traceBuilder.childrenWaitAgain).
type Wait struct {
	Kind       WaitKind
	Start, End time.Time
	EndedBy    WaitEnd
	On         []string
	Ask        string
	Version    int
	Artifact   string
	SinceSeq   int64
	From       phase.Phase
	Role       claim.Role
	Reason     string
	Told       bool
}

// released says whether w, a hold, ended with the record's issue working the held phase again:
// its release line (WaitReleased), or the phase going on with none (WaitWentOn). A hold that ends
// otherwise ended with its issue's leave, or stands at the window's end.
func (w Wait) released() bool { return w.EndedBy == WaitReleased || w.EndedBy == WaitWentOn }

// TurnEnd is how a turn ended.
type TurnEnd string

const (
	// TurnEndLine is a turn its own `supervise: a turn ended` line ended.
	TurnEndLine TurnEnd = "ended"
	// TurnEndClaimFailed is a turn its claim's failure ended (`supervise: claim failed`).
	TurnEndClaimFailed TurnEnd = "claim_failed"
	// TurnEndSuspended is a turn its claim's suspend ended (`supervise: suspended`).
	TurnEndSuspended TurnEnd = "suspended"
	// TurnEndDied is a turn its process's death ended (`supervise: process died`).
	TurnEndDied TurnEnd = "died"
	// TurnEndDisplaced is a turn whose process was found gone, or found to be another runtime's,
	// through no fault of its own (`supervise: the process was displaced through no fault of its
	// own, …`).
	TurnEndDisplaced TurnEnd = "displaced"
	// TurnEndLetGo is a turn the let-go of its process cut off — a relaunch, a suspension, a
	// failure or the retirement a stop makes stopping it (`supervise: the relaunch ended work the
	// agent had running`).
	TurnEndLetGo TurnEnd = "let_go"
	// TurnEndRetired is a turn of the claim a production check is for, which the check's close
	// retires with no line of the retirement's own (`workflow: production check closed`,
	// CheckClosed): the issue stands outside the workflow, so the turn is the check's.
	TurnEndRetired TurnEnd = "retired"
	// TurnEndLeft is a turn its issue's leaving the workflow ended.
	TurnEndLeft TurnEnd = "left"
	// TurnEndOpen is a turn that stands at the window's end.
	TurnEndOpen TurnEnd = "open"
)

// Turn is one turn of a claim's session, joined to its issue and role by the claim token: from its
// `supervise: a turn started` to its `supervise: a turn ended`, its claim's failure or suspend, the
// end of the process it ran on (ProcessEnded), the close of its issue's production check that
// retires its claim (CheckClosed), or its issue's leave; End zero while it stands at the window's
// end.
type Turn struct {
	Claim      ClaimRef
	Generation uint64
	Start, End time.Time
	EndedBy    TurnEnd
}

// GateKind is what one fact of a design gate is.
type GateKind string

const (
	// GateRegisteredFact is a `workflow: design gate registered` line: the architect registered a
	// version of its document at a step.
	GateRegisteredFact GateKind = "registered"
	// GateOpenedFact and GateClosedFact are a `workflow: design gate opened|closed` line: an
	// artifact event flipped the step's gate.
	GateOpenedFact GateKind = "opened"
	GateClosedFact GateKind = "closed"
	// GateApproved and GateChangesRequested are a person's verdicts on a version of the issue's
	// document, from its Dispatch events.
	GateApproved         GateKind = "approved"
	GateChangesRequested GateKind = "changes_requested"
)

// GateFact is one fact of an issue's design gate: a registration, an opening or a closing (the
// gate lines), or a verdict (artifact.approved and artifact.changes_requested), with the document,
// the step, the version and, for a registration, whether that version stood approved and whether
// a changes request stood on it. Event is the kind of artifact event a gate opened or closed at,
// and Seq the Dispatch sequence of a verdict.
type GateFact struct {
	At               time.Time
	Kind             GateKind
	Artifact         string
	Step             record.DesignStep
	Version          int
	Open             bool
	ChangesRequested bool
	Event            string
	Seq              int64
}

// gateReader is a design gate's openness as its facts leave it, read one fact at a time in time
// order: the one reading of a gate the line timeline (gateStepsOf) and the trace's children waits
// (traceBuilder.childrenWaitAgain) share. A registration leaves the gate open or closed as it was
// registered, and an opening or a closing line flips it, each naming the document and the version
// the gate stands at. The daemon writes such a line at every artifact event that flips a gate and
// at no other (workflow's applyArtifact, classify.DesignGateOpen), so where the window holds those
// kinds (lines, Trace.GateLines) a verdict moves nothing the lines do not. Where it holds none, a
// verdict moves the gate only where it is on the document and at the version the gate stands at,
// as record.GateOpen opens a step on the approval of its latest version alone: a verdict on
// another document of the issue, which the deployment gates no step of, or on an older version
// changes no gate state.
type gateReader struct {
	lines    bool
	open     bool
	artifact string
	version  int
}

// apply reads fact into the gate and answers whether the gate stands open after it.
func (g *gateReader) apply(fact GateFact) bool {
	switch fact.Kind {
	case GateRegisteredFact, GateOpenedFact, GateClosedFact:
		g.open, g.artifact, g.version = fact.Kind == GateOpenedFact || fact.Kind == GateRegisteredFact && fact.Open, fact.Artifact, fact.Version
	case GateApproved, GateChangesRequested:
		if !g.lines && fact.Artifact == g.artifact && fact.Version == g.version {
			g.open = fact.Kind == GateApproved
		}
	}
	return g.open
}

// ChildFiling is a child filed under the issue (issue.created naming it the parent) or moved under
// it (an issue event that changed its parent to it).
type ChildFiling struct {
	Child string
	At    time.Time
	Moved bool
}

// Leave is the issue leaving the workflow at a Dispatch status.
type Leave struct {
	At     time.Time
	Status string
}

// Linger is one linger of a tree: begun At, to run until Until, and Closed when its linger closed
// the tree (zero while it runs at the window's end).
type Linger struct {
	At, Until, Closed time.Time
}

// QueueDrop is a line of an issue's pull request leaving the merge queue: its msg and, for a line on
// which the daemon read nothing, the reason it read nothing.
type QueueDrop struct {
	At   time.Time
	Line string
	Why  string
}

// Boot is one daemon process: when it started (the process the log opens in with no start line is
// taken to have started at Trace.Start), and when it finished its boot, where its `legion daemon
// started` line says.
type Boot struct {
	At      time.Time
	Started time.Time
	// Stopped is when the process began to stop, where its `legion daemon stopping` or `legion
	// daemon stopped` line says (ProcessStop); zero where the log shows neither.
	Stopped time.Time
	// Holding is when the boot's reconcile held the summaries its listing found ahead of the
	// record (ReconcileHolding), and CaughtUp when the Dispatch consumer reached the position that
	// hold waited for and the pass released them (ConsumerCaughtUp); each zero where the log shows
	// none.
	Holding, CaughtUp time.Time
}

// PauseSpan is one pause of the project the log shows: its id, its reason, when it began and when
// it ended (zero while it stands at the window's end). No admission pass promotes anything while
// it stands.
type PauseSpan struct {
	Pause    int64
	Reason   string
	From, To time.Time
}

// Downtime is a stretch with no daemon process: from the last line of one process to the start of
// the next.
type Downtime struct {
	From, To time.Time
}

// tickDelay is how long after its boot a daemon runs its first controller tick (design section 5).
const tickDelay = time.Minute

// BuildTrace reads w's record as its Trace: every Dispatch event of every issue, and each line of
// the log of a kind w.Lines names (design 10.1 (b)), so a replay's own log, which holds kinds an
// older window lacks, is read through the window's kinds alone and measured like its record. The
// events and the lines are read in one time order, the events of an instant before its lines, as
// the fact a pass applied comes before what the pass wrote. Then the slots, waits, worker starts,
// segments and ticks are built from what they read. It fails for a line or an event its reader
// refuses, which collect never writes into a window, and for a part of the window that names a
// slot rule the repository does not keep (record.SlotRuleNamed), whose phases no slot span or
// worker start could be read by; what it reads and cannot place it names in Trace.Unplaced.
func BuildTrace(w Window) (Trace, error) {
	return buildTrace(w)
}

// buildTrace is BuildTrace of w, which may be a replay's own log (driver.lineTimeline) from its
// start on, read under the rule w's parts name, as a record is.
func buildTrace(w Window) (Trace, error) {
	t, _, _, err := readTrace(w)
	return t, err
}

// readTrace is buildTrace of w with what it read on the way, which a run's script reads again
// (newWindowInputs): the lines of the kinds the window holds and every Dispatch event, each in
// the one time order the trace read them in, the events by time and then id.
func readTrace(w Window) (Trace, []LogLine, []readEvent, error) {
	rules := make([]record.SlotRule, len(w.Parts))
	for i, part := range w.Parts {
		rule, kept := record.SlotRuleNamed(part.Rule)
		if !kept {
			return Trace{}, nil, nil, fmt.Errorf("replay: the window's part from %s names the slot rule %q, which the repository does not keep (legion replay rules)",
				stamp(part.From), part.Rule)
		}
		rules[i] = rule
	}
	b := traceBuilder{
		t: &Trace{Project: w.Project, From: w.From, To: w.To, End: w.End(), Start: w.From.Add(-time.Duration(w.Before)), Parts: w.Parts, rules: rules,
			GateSteps: w.GateSteps, TickInterval: time.Duration(w.TickInterval), Branches: map[string]string{},
			Issues: map[string]*IssueTrace{}, claimCauses: map[claim.Token][]claimCause{}},
		held:        make(map[string]bool, len(w.Lines)),
		turns:       map[claim.Token]turnAt{},
		backKind:    map[string]string{},
		treeEntered: map[string]bool{},
		named:       map[entryRef]bool{},
		parked:      map[string]uint64{},
		sessions:    map[string][]RecordedSession{},
		posts:       map[string][]time.Time{},
	}
	for _, msg := range w.Lines {
		b.held[msg] = true
	}
	b.recordLines, b.heldLines = b.held[passMovedSlotsLine], b.held[phaseHeldLine]
	b.t.GateLines = b.held[gateOpenedLine] && b.held[gateClosedLine]
	for repo, merges := range w.Merges {
		if len(merges) > 0 {
			b.t.Branches[repo] = merges[0].Branch
		}
	}

	var lines []LogLine
	for i, raw := range w.Log {
		line, err := ParseLogLine(raw)
		if err != nil {
			return Trace{}, nil, nil, fmt.Errorf("replay: line %d of the window's log: %w", i+1, err)
		}
		if line != nil && b.held[line.Head().Msg] {
			lines = append(lines, line)
		}
	}
	slices.SortStableFunc(lines, func(x, y LogLine) int { return x.Head().Time.Compare(y.Head().Time) })

	var events []readEvent
	for _, key := range slices.Sorted(maps.Keys(w.Events)) {
		for i, raw := range w.Events[key] {
			event, err := ParseDispatchEvent(raw)
			if err != nil {
				return Trace{}, nil, nil, fmt.Errorf("replay: event %d of %s: %w", i+1, key, err)
			}
			events = append(events, readEvent{event: event, raw: raw})
		}
	}
	slices.SortStableFunc(events, func(x, y readEvent) int {
		return cmp.Or(x.event.At.Compare(y.event.At), cmp.Compare(x.event.ID, y.event.ID))
	})

	for i, j := 0, 0; i < len(events) || j < len(lines); {
		if i < len(events) && (j == len(lines) || !events[i].event.At.After(lines[j].Head().Time)) {
			b.onEvent(events[i])
			i++
			continue
		}
		b.onLine(lines[j])
		j++
	}
	b.finish()
	return *b.t, lines, events, nil
}

// readEvent is one Dispatch event as the trace read it, and its raw bytes, from which the issue's
// title and repository are read (issueSubject).
type readEvent struct {
	event DispatchEvent
	raw   json.RawMessage
}

// turnAt is where a claim's open turn stands: the issue whose Turns hold it ("" for the
// controller's) and its index there.
type turnAt struct {
	issue string
	index int
}

// traceBuilder is BuildTrace's state while it reads a window.
type traceBuilder struct {
	t *Trace
	// held is the line kinds the window holds; recordLines and heldLines whether they include the
	// record line of a pass and the hold's line.
	held                   map[string]bool
	recordLines, heldLines bool
	// turns is each claim's open turn.
	turns map[claim.Token]turnAt
	// buffered is the per-slot moves read since the last record line, under recordLines: the
	// record line of their pass takes their place.
	buffered []SlotMove
	// backKind is the kind of entry an issue's next slot goes to, read from the in-transaction
	// line of its pass that comes before the slot's own line.
	backKind map[string]string
	// starting is whether the current boot has written only its start lines so far, and last the
	// time of the last line read.
	starting bool
	last     time.Time
	// children is every issue ever shown under each parent (childrenOf), built once reading ends.
	children map[string][]string
	// treeEntered is each issue whose first entry was into a live tree as a child, at the tree's
	// generation (maybeEnter), and named each entry whose generation a line of the daemon named
	// (generationIs, maybeEnter after parked): once every line is read, the first entry of each
	// such child takes its tree root's generation as the lines leave it (childGenerations).
	treeEntered map[string]bool
	named       map[entryRef]bool
	// parked is the generation an adoption into a live tree parked each issue at, standing in no
	// run (adopted), until the todo that runs it again enters it (maybeEnter).
	parked map[string]uint64
	// sessions is each session the events show running a claim, by the claim's issue, in the order
	// of their first events (sessionSeen); finish gives each issue of the trace its own.
	sessions map[string][]RecordedSession
	// shown is each issue's Dispatch states as its issue events show them (shownOf), indexed the
	// first time a wait reads the issue, once every event is read (dispatchStateAt).
	shown map[string]shownStates
	// posts is, by issue, the moment of each post of the daemon's own naming a merger's move back
	// out of merging for a conflicting head (mergerConflictPost), which finish reads the moves of
	// (mergerConflicts).
	posts map[string][]time.Time
}

// messageCreated is the type of the Dispatch event of a post on an issue, which the daemon's outbox
// makes of a backward move's reason (workflow's moveReason) before it writes the move's status.
const messageCreated = "message.created"

// mergerMoveBack is the daemon's post of a merger's move back out of merging (workflow's
// backwardMoveReason, in every release since 20.42.5): the phase it went to, and the merger's own
// words.
var mergerMoveBack = regexp.MustCompile(`(?s)Legion moved this issue from merging back to (\S+) at the merger's request: (.*)`)

// conflictWord is a word that names a conflict in a merger's words.
var conflictWord = regexp.MustCompile(`(?i)\bconflict`)

// mergerConflictPost says whether raw, a message.created event, is the daemon's post of a merger's
// move back out of merging (mergerMoveBack) whose words name a conflicting head (conflictWord).
func mergerConflictPost(raw json.RawMessage) bool {
	var event struct {
		Payload struct {
			Body string `json:"body"`
		} `json:"payload"`
	}
	if json.Unmarshal(raw, &event) != nil {
		return false
	}
	posted := mergerMoveBack.FindStringSubmatch(event.Payload.Body)
	return posted != nil && conflictWord.MatchString(posted[2])
}

// mergerConflicts makes each merger's move back of iss out of merging that a post of the daemon's
// names as one for a conflicting head (posts) a conflict (plan D7): the move back that comes last
// at or before the post, where the record made it by backward and it has a send-back's cause, takes
// the cause conflict and the source of a recorded conflict, left to the draw as every recorded
// conflict move is (MoveSources), so the stretch it ends counts as a conflict in the fit
// (recordedStretches) and its loop as a round of conflict in the measures. Both copies of the move,
// the Out of the visit it ends and the In of the visit it begins, change alike.
func (b *traceBuilder) mergerConflicts(iss *IssueTrace) {
	for _, at := range b.posts[iss.Key] {
		last := -1
		for i := range iss.Visits {
			if out := iss.Visits[i].Out; out != nil && !out.At.After(at) && out.From == phase.Merging && phase.MovesBack(out.From, out.To) {
				last = i
			}
		}
		if last < 0 {
			continue
		}
		out := iss.Visits[last].Out
		if out.Trigger != workflow.TriggerBackward || out.Cause != CauseSendBack {
			continue
		}
		out.Cause, out.Source = CauseConflict, MoveSources[workflow.TriggerConflict]
		if next := last + 1; next < len(iss.Visits) && iss.Visits[next].In != nil && iss.Visits[next].In.At.Equal(out.At) {
			in := iss.Visits[next].In
			in.Cause, in.Source = out.Cause, out.Source
		}
	}
}

// entryRef is one entry of an issue's trace: its issue and its index in the issue's Entries.
type entryRef struct {
	issue string
	index int
}

// issue is key's trace, made where none is yet.
func (b *traceBuilder) issue(key string) *IssueTrace {
	iss, ok := b.t.Issues[key]
	if !ok {
		project, _, _ := strings.Cut(key, "-")
		if claim.IsIssueKey(key) && b.t.Project != "" && strings.EqualFold(project, b.t.Project) {
			project = b.t.Project
		}
		iss = &IssueTrace{Key: key, Project: project}
		b.t.Issues[key] = iss
	}
	return iss
}

// unplaced names what the trace could not place, of the part of the record kind.
func (b *traceBuilder) unplaced(kind UnplacedKind, issue string, at time.Time, format string, args ...any) {
	b.t.Unplaced = append(b.t.Unplaced, UnplacedFact{At: at, Issue: issue, Kind: kind, Reason: fmt.Sprintf(format, args...)})
}

// openVisit is the visit iss stands in, nil where it stands in none.
func (iss *IssueTrace) openVisit() *Visit {
	if n := len(iss.Visits); n > 0 && iss.Visits[n-1].Exit.IsZero() {
		return &iss.Visits[n-1]
	}
	return nil
}

// entry is iss's latest entry, nil before its first.
func (iss *IssueTrace) entry() *Entry {
	if n := len(iss.Entries); n > 0 {
		return &iss.Entries[n-1]
	}
	return nil
}

// openWait is iss's standing wait of kind, nil where none stands.
func (iss *IssueTrace) openWait(kind WaitKind) *Wait {
	for i := len(iss.Waits) - 1; i >= 0; i-- {
		if iss.Waits[i].Kind == kind && iss.Waits[i].End.IsZero() && iss.Waits[i].EndedBy == "" {
			return &iss.Waits[i]
		}
	}
	return nil
}

// Moves is every recorded move of the issue, in order: the move that ended each visit.
func (iss *IssueTrace) Moves() []Move {
	var moves []Move
	for _, visit := range iss.Visits {
		if visit.Out != nil {
			moves = append(moves, *visit.Out)
		}
	}
	return moves
}

// Generation is the generation the issue's record stood at in its latest entry, 0 before its
// first.
func (iss *IssueTrace) Generation() uint64 {
	if entry := iss.entry(); entry != nil {
		return entry.Generation
	}
	return 0
}

// onEvent reads one Dispatch event: it is a fact of its issue, the filing of a child, a verdict on
// a document, the first event of a session running a claim, and, where it makes the issue a queued
// root or a child of a live tree, its entry.
func (b *traceBuilder) onEvent(read readEvent) {
	event := read.event
	iss := b.issue(event.Key)
	iss.Project = event.Project
	iss.Dispatch = append(iss.Dispatch, event)
	b.sessionSeen(event)
	switch {
	case event.Issue != nil:
		payload := readIssuePayload(read.raw)
		title, repo := payload.subject()
		if title != "" {
			iss.Title = title
		}
		if iss.Repo == "" {
			iss.Repo = repo
		}
		if priority, shown := payload.priority(); shown {
			iss.Priority = priority
		}
		for linked, number := range payload.linkedPullRequests() {
			if iss.LinkedPullRequests == nil {
				iss.LinkedPullRequests = map[string]int{}
			}
			iss.LinkedPullRequests[linked] = number
		}
		b.childFiled(iss, event)
		b.maybeEnter(iss, event)
	case event.Type == messageCreated && daemonWrote(event.Actor, event.Key) && mergerConflictPost(read.raw):
		b.posts[event.Key] = append(b.posts[event.Key], event.At)
	case event.Verdict != nil:
		kind := GateApproved
		if event.Type == "artifact.changes_requested" {
			kind = GateChangesRequested
		}
		iss.Gates = append(iss.Gates, GateFact{At: event.At, Kind: kind, Artifact: event.Verdict.Artifact,
			Version: event.Verdict.Version, Seq: event.Seq})
	}
}

// sessionSeen records the session event's actor is where its title names a claim it runs
// (DispatchEvent.ActorClaim), at the event, the first of that session under that claim: events are
// read in time order.
func (b *traceBuilder) sessionSeen(event DispatchEvent) {
	ran := event.ActorClaim
	if ran == nil {
		return
	}
	if slices.ContainsFunc(b.sessions[ran.Issue], func(s RecordedSession) bool { return s.ID == event.Actor.ID && s.Role == ran.Role }) {
		return
	}
	b.sessions[ran.Issue] = append(b.sessions[ran.Issue], RecordedSession{ID: event.Actor.ID, Role: ran.Role, First: event.At})
}

// issuePayload is what the trace reads of an issue event's payload that ParseDispatchEvent leaves
// out, each member as the event writes it: its title, its repository and the one Dispatch resolved
// for it, its priority and its external links. The event is decoded once (readIssuePayload), and
// each reading decodes the members it reads (subject, priority, linkedPullRequests); unread is a
// payload that does not read, from which none reads anything.
type issuePayload struct {
	Payload struct {
		Title         json.RawMessage `json:"title"`
		Repo          json.RawMessage `json:"repo"`
		ResolvedRepo  json.RawMessage `json:"resolved_repo"`
		Priority      json.RawMessage `json:"priority"`
		ExternalLinks json.RawMessage `json:"external_links"`
	} `json:"payload"`
	unread bool
}

// priority is the event's priority as its payload writes it, "" for null, and whether the payload
// carries the member at all.
func (p issuePayload) priority() (string, bool) {
	member := p.Payload.Priority
	if p.unread || member == nil {
		return "", false
	}
	if string(member) == "null" {
		return "", true
	}
	return string(member), true
}

// readIssuePayload reads raw, an issue event, into its issuePayload.
func readIssuePayload(raw json.RawMessage) issuePayload {
	var p issuePayload
	p.unread = json.Unmarshal(raw, &p) != nil
	return p
}

// decodeMember decodes member, a member of a payload as written, into into; an absent member
// leaves into as it is.
func decodeMember(member json.RawMessage, into any) error {
	if member == nil {
		return nil
	}
	return json.Unmarshal(member, into)
}

// subject is the event's title and repository: the repository Dispatch resolved for it
// (resolved_repo), or the one it names (repo) where none is resolved; both empty where a member does
// not read.
func (p issuePayload) subject() (title, repo string) {
	var named, resolved *string
	if p.unread || decodeMember(p.Payload.Title, &title) != nil || decodeMember(p.Payload.Repo, &named) != nil ||
		decodeMember(p.Payload.ResolvedRepo, &resolved) != nil {
		return "", ""
	}
	switch {
	case resolved != nil && *resolved != "":
		repo = *resolved
	case named != nil:
		repo = *named
	}
	return title, repo
}

// pullRequestLink is a link to a GitHub pull request: https://github.com/<owner>/<repo>/pull/<n>.
var pullRequestLink = regexp.MustCompile(`^https://github\.com/([^/]+/[^/]+)/pull/([0-9]+)/?$`)

// linkedPullRequests is the pull requests the payload links among its external links: each link's
// number by its repository as owner/repo in lower case, the last one listed where it lists two of
// one repository. A link is an object with its url, as Dispatch serves it, or the url alone. None
// where the links do not read.
func (p issuePayload) linkedPullRequests() map[string]int {
	var links []json.RawMessage
	if p.unread || decodeMember(p.Payload.ExternalLinks, &links) != nil {
		return nil
	}
	var linked map[string]int
	for _, link := range links {
		var url string
		if json.Unmarshal(link, &url) != nil {
			var object struct {
				URL string `json:"url"`
			}
			if json.Unmarshal(link, &object) != nil {
				continue
			}
			url = object.URL
		}
		found := pullRequestLink.FindStringSubmatch(url)
		if found == nil {
			continue
		}
		number, err := strconv.Atoi(found[2])
		if err != nil {
			continue
		}
		if linked == nil {
			linked = map[string]int{}
		}
		linked[strings.ToLower(found[1])] = number
	}
	return linked
}

// childFiled records event's issue on its parent's trace where the event filed it there or moved
// it under that parent.
func (b *traceBuilder) childFiled(iss *IssueTrace, event DispatchEvent) {
	parent := event.Issue.Parent
	if parent == "" {
		return
	}
	moved := event.Type != "issue.created"
	if moved {
		// The issue's state before this event: its newest issue event before it.
		var previous *DispatchIssueState
		for i := len(iss.Dispatch) - 2; i >= 0; i-- {
			if iss.Dispatch[i].Issue != nil {
				previous = iss.Dispatch[i].Issue
				break
			}
		}
		if previous != nil && previous.Parent == parent {
			return
		}
	}
	under := b.issue(parent)
	under.Children = append(under.Children, ChildFiling{Child: iss.Key, At: event.At, Moved: moved})
}

// maybeEnter enters iss into the workflow at event where the issue, of the window's project,
// stands in none and the event shows it todo: as a child of the tree of a parent that stands in
// the workflow, or, handed to Legion, as the root of a tree of its own. An entry after a leave is a
// reopen, at the next generation (workflow's ReenterChild, admit's readmit). A child's first entry
// is at its tree's generation, as the daemon records a child it holds no record of
// (workflow's recordChild, enterChild with the root's generation), not at a first generation of its
// own: a child filed under a tree the daemon re-admitted stands at the tree's generation from its
// entry, which the moves the record makes of it are made at. The root's generation is read again
// once every line is read (childGenerations), since a line naming it may come after the child's
// entry. An issue an adoption parked (joined) enters at the generation after the one the adoption
// gave it, which the daemon named.
func (b *traceBuilder) maybeEnter(iss *IssueTrace, event DispatchEvent) {
	if iss.Project != b.t.Project || iss.openVisit() != nil || event.Issue.Status != "todo" {
		return
	}
	tree := ""
	if parent, ok := b.t.Issues[event.Issue.Parent]; ok && parent.openVisit() != nil && parent.entry() != nil {
		tree = parent.entry().Tree
	} else if dispatch.CarriesLegionLabel(event.Issue.Labels) {
		tree = iss.Key
	}
	if tree == "" {
		return
	}
	generation := iss.Generation() + 1
	if root, ok := b.t.Issues[tree]; ok && tree != iss.Key && len(iss.Entries) == 0 {
		generation = max(root.Generation(), 1)
		b.treeEntered[iss.Key] = true
	}
	parked, adopted := b.parked[iss.Key]
	if adopted {
		generation = parked + 1
		delete(b.parked, iss.Key)
	}
	iss.Entries = append(iss.Entries, Entry{At: event.At, Tree: tree, Generation: generation, Reopen: len(iss.Entries) > 0})
	iss.Visits = append(iss.Visits, Visit{Phase: phase.Admitted, Enter: event.At})
	if adopted {
		b.named[entryRef{issue: iss.Key, index: len(iss.Entries) - 1}] = true
	}
}

// joined reads a root's adoption into a live tree (RootJoined): the adoption writes the issue's
// record at its next generation, the one the line names (workflow's moveIntoTree). A record not at
// todo, as a root that left the workflow is, is parked there as a child at done, standing in no run,
// and the todo that runs it again, the release's own write, opens the generation after it
// (ReenterChild), which its entry stands at (maybeEnter). A root the trace holds in a run, a queued
// root at todo, is read no further here: it keeps the entry its todo made.
func (b *traceBuilder) joined(l RootJoined) {
	if iss := b.issue(l.Issue); iss.openVisit() == nil && l.Generation > 0 {
		b.parked[l.Issue] = l.Generation
	}
}

// firstBootStage is the boot stage a booting daemon writes before it opens its store (BootStage):
// the one stage line that can begin a process. Its later stages come after other lines of the
// boot.
const firstBootStage = "prompts"

// onLine reads one line of the log. A process's start lines (ProcessStart, and its first boot
// stage), read with nothing but start lines between them, begin one boot together.
func (b *traceBuilder) onLine(line LogLine) {
	at := line.Head().Time
	// A refused launch is evidence for the trace alone. It must not end a turn or change the
	// process boundaries used by the replay's driver.
	if failure, ok := line.(StartFailure); ok {
		token := failure.Claim.Token
		b.t.claimCauses[token] = append(b.t.claimCauses[token], claimCause{at: at, text: failure.Error, startFailure: true})
		return
	}
	// The daemon's line of a kill a launcher reported is the kill matcher's evidence alone
	// (MatchKills): it moves nothing of the trace, and no process boundary.
	if _, ok := line.(GuardKillLine); ok {
		return
	}
	// A read of a pull request's merge state is held on its issue and nothing more: the replay
	// decides nothing from it, so a window that holds it is traced as one that does not.
	if read, ok := line.(MergeStateRead); ok {
		iss := b.issue(read.Issue)
		iss.MergeStateReads = append(iss.MergeStateReads, read)
		return
	}
	starts := false
	switch l := line.(type) {
	case ProcessStart:
		starts = true
	case BootStage:
		starts = l.Stage == firstBootStage
	}
	switch {
	case starts && (!b.starting || len(b.t.Boots) == 0):
		b.boot(at)
	case !starts && len(b.t.Boots) == 0:
		b.boot(b.t.Start)
	}
	b.starting = starts
	b.last = at

	switch l := line.(type) {
	case DaemonStarted:
		boot := &b.t.Boots[len(b.t.Boots)-1]
		boot.Started = at
	case PhaseChanged:
		b.moved(l)
	case SlotTaken:
		kind := b.backKind[l.Issue]
		delete(b.backKind, l.Issue)
		b.slotMoved(SlotMove{At: at, Issue: l.Issue, Given: true, Slots: l.Slots, Cap: l.Cap, Kind: kind})
	case SlotReleased:
		b.slotMoved(SlotMove{At: at, Issue: l.Issue, Slots: l.Slots, Cap: l.Cap})
	case PassMovedSlots:
		b.passMoved(l)
	case SlotBackWorkReturned:
		b.backKind[l.Issue] = record.LineReturning.String()
	case SlotBackWaitEnded:
		b.backKind[l.Issue] = record.LineWaitingOnIssue.String()
	case RootJoined:
		b.joined(l)
	case WaitsOnIssues:
		// The per-pass wait lines are left: a link wait is built from the issue's and its
		// blockers' events, which every window holds whole.
		b.issue(l.Issue)
	case WaitsForChildren:
		// The daemon tells each issue's children wait once for its generation, so each line is a
		// wait of its own; what ends one, and a wait of the same generation that begins again with
		// no line, is read once every event is (childrenWaitEnds).
		iss := b.issue(l.Issue)
		b.generationIs(iss, l.Generation)
		iss.Waits = append(iss.Waits, Wait{Kind: WaitChildren, Start: at, Told: true})
	case WaitsOnPerson:
		// Each line is a root entering a wait on a person (workflow's enterAwaiting); its end is
		// read once every event is (personWaitEnds).
		iss := b.issue(l.Issue)
		wait := Wait{Kind: WaitPerson, Start: at, SinceSeq: l.SinceSeq, Reason: l.Reason}
		wait.Ask, wait.Version = personWaitSubject(l.Reason)
		if wait.Version > 0 {
			wait.Artifact = latestArtifact(iss, at)
		}
		iss.Waits = append(iss.Waits, wait)
	case GateRegistered:
		iss := b.issue(l.Issue)
		iss.Gates = append(iss.Gates, GateFact{At: at, Kind: GateRegisteredFact, Artifact: l.Artifact, Step: l.Step, Version: l.Version,
			Open: l.Open, ChangesRequested: l.ChangesRequested})
	case GateOpened:
		iss := b.issue(l.Issue)
		iss.Gates = append(iss.Gates, GateFact{At: at, Kind: GateOpenedFact, Artifact: l.Artifact, Version: l.Version, Open: true, Event: l.Event})
	case GateClosed:
		iss := b.issue(l.Issue)
		iss.Gates = append(iss.Gates, GateFact{At: at, Kind: GateClosedFact, Artifact: l.Artifact, Version: l.Version, Event: l.Event})
	case HoldReleased:
		iss := b.issue(l.Issue)
		switch wait := iss.openWait(WaitHeld); {
		case wait != nil:
			wait.End, wait.EndedBy = at, WaitReleased
		case l.Reason == operatorRetryReason:
			// The operator's retry of a phase's worker writes this line whether or not the issue
			// stands held (workflow/engine.go at e6edc575: (*Engine).restartPhase hands the phase
			// its worker works to (*Engine).retryPhase, which writes the line with no regard to a
			// hold), so with no hold held it is that retry, and releases nothing; with the launch
			// of the phase's worker that the retry made written before it, it is the retry with a
			// fresh session (freshRetried).
			b.freshRetried(iss, at, l.Phase)
		default:
			b.unplaced(UnplacedHold, l.Issue, at, "%s at %s releases a hold the trace does not hold", l.Msg, stamp(at))
		}
	case PhaseHeld:
		if b.heldLines {
			b.holdBegins(b.issue(l.Issue), at, l.From, l.Role, l.Reason)
		}
	case ClaimFailed:
		b.turnEnds(l.Claim, at, TurnEndClaimFailed, false)
		if !b.heldLines && l.Claim.Issue != "" {
			iss := b.issue(l.Claim.Issue)
			if visit := iss.openVisit(); visit != nil && l.Claim.Role != claim.RoleArchitect && phaseRole(visit.Phase) == l.Claim.Role {
				b.holdBegins(iss, at, visit.Phase, l.Claim.Role, l.Cause)
			}
		}
	case LeftWorkflow:
		b.left(b.issue(l.Issue), at, l.Status)
	case TreeLingers:
		root := b.issue(l.Tree)
		b.generationIs(root, l.Generation)
		root.Lingers = append(root.Lingers, Linger{At: at, Until: l.Until})
	case LingerClosed:
		root := b.issue(l.Tree)
		b.generationIs(root, l.Generation)
		if n := len(root.Lingers); n > 0 && root.Lingers[n-1].Closed.IsZero() {
			root.Lingers[n-1].Closed = at
		} else {
			b.unplaced(UnplacedLinger, l.Tree, at, "%s at %s closes a linger the trace does not hold", l.Msg, stamp(at))
		}
	case TurnStarted:
		b.turnStarts(l)
	case TurnEnded:
		if !l.Outcome {
			b.turnEnds(l.Claim, at, TurnEndLine, true)
		}
	case Suspended:
		b.turnEnds(l.Claim, at, TurnEndSuspended, false)
	case ProcessEnded:
		if l.DeathCause != "" {
			token := l.Claim.Token
			b.t.claimCauses[token] = append(b.t.claimCauses[token], claimCause{at: at, text: l.DeathCause})
		}
		b.turnEnds(l.Claim, at, l.By, false)
	case CheckClosed:
		b.checkClosed(b.issue(l.Issue), at)
	case QueueLeftNothingRead:
		b.queueLeft(l.Issue, at, l.Msg, l.PullRequest, l.Why)
	case QueueLeftReadOnce:
		b.queueLeft(l.Issue, at, l.Msg, l.PullRequest, "")
	case MergeForwardAsked:
		iss := b.issue(l.Issue)
		if n := len(iss.MergeForwards); n == 0 || !iss.MergeForwards[n-1].End.IsZero() {
			iss.MergeForwards = append(iss.MergeForwards, MergeForwardRound{Start: at, Phase: l.Phase})
		}
	case MergeForwardLanded:
		iss := b.issue(l.Issue)
		if n := len(iss.MergeForwards); n > 0 && iss.MergeForwards[n-1].End.IsZero() {
			iss.MergeForwards[n-1].End = at
		}
	case MergedCloseWaits:
		b.mergeWaits(l)
	case ProcessStop:
		if boot := &b.t.Boots[len(b.t.Boots)-1]; boot.Stopped.IsZero() {
			boot.Stopped = at
		}
	case ReconcileHolding:
		if boot := &b.t.Boots[len(b.t.Boots)-1]; boot.Holding.IsZero() {
			boot.Holding = at
		}
	case ConsumerCaughtUp:
		if boot := &b.t.Boots[len(b.t.Boots)-1]; !boot.Holding.IsZero() && boot.CaughtUp.IsZero() {
			boot.CaughtUp = at
		} else {
			b.unplaced(UnplacedBoot, "", at, "%s at %s releases a hold no boot of the trace holds", l.Msg, stamp(at))
		}
	case PauseBegan:
		b.t.Pauses = append(b.t.Pauses, PauseSpan{Pause: l.Pause, Reason: l.Reason, From: at})
	case PauseEnded:
		if n := len(b.t.Pauses); n > 0 && b.t.Pauses[n-1].To.IsZero() && b.t.Pauses[n-1].Pause == l.Pause {
			b.t.Pauses[n-1].To = at
		} else {
			b.unplaced(UnplacedPause, "", at, "%s at %s ends the pause %d, which the trace does not hold open", l.Msg, stamp(at), l.Pause)
		}
	case Launched:
		if iss, ok := b.t.Issues[l.Claim.Issue]; ok && l.Claim.Issue != "" {
			ended := b.holdWentOn(iss, at, l.Claim.Role)
			iss.Launches = append(iss.Launches, Launch{At: at, Role: l.Claim.Role, Resumed: l.Resumed, EndedHold: ended, Turns: len(iss.Turns)})
		}
	case ProcessStart, BootStage:
	}
}

// stamp is at as a reason names it.
func stamp(at time.Time) string { return at.UTC().Format(time.RFC3339Nano) }

// boot begins a daemon process at at, and the downtime before it where an earlier process wrote a
// line: from that process's first stop line where it wrote one (Boot.Stopped), since it runs no
// pass once it has begun to stop, and from its last line otherwise.
func (b *traceBuilder) boot(at time.Time) {
	if n := len(b.t.Boots); n > 0 && !b.last.IsZero() && b.last.Before(at) {
		from := b.last
		if stopped := b.t.Boots[n-1].Stopped; !stopped.IsZero() && stopped.Before(from) {
			from = stopped
		}
		b.t.Downtimes = append(b.t.Downtimes, Downtime{From: from, To: at})
	}
	b.t.Boots = append(b.t.Boots, Boot{At: at})
}

// generationIs sets iss's latest entry to generation, the generation a line of the daemon names
// for its record, which is the record's own count where the trace counted otherwise (an earlier
// reopen before the inputs reach), and marks it named, so no reading of a tree's generation sets it
// again (childGenerations).
func (b *traceBuilder) generationIs(iss *IssueTrace, generation uint64) {
	if entry := iss.entry(); entry != nil && generation > 0 {
		entry.Generation = generation
		b.named[entryRef{issue: iss.Key, index: len(iss.Entries) - 1}] = true
	}
}

// childGenerations reads again, once every line is read, the generations of each child whose first
// entry was into a live tree (maybeEnter): that entry stands at its tree root's generation at the
// entry, as the lines leave the root's, since a line naming the root's may come after the child's
// entry (a root's children wait is written by the pass of the fact that released the child), and
// each later entry stands one past the entry before it (workflow's ReenterChild). An entry whose
// generation a line named (generationIs) keeps it.
func (b *traceBuilder) childGenerations() {
	for _, key := range slices.Sorted(maps.Keys(b.treeEntered)) {
		iss := b.t.Issues[key]
		for i := range iss.Entries {
			entry := &iss.Entries[i]
			switch {
			case b.named[entryRef{issue: key, index: i}]:
			case i > 0:
				entry.Generation = iss.Entries[i-1].Generation + 1
			default:
				if root, ok := b.t.Issues[entry.Tree]; ok && entry.Tree != key {
					if at := root.entryAt(entry.At); at != nil {
						entry.Generation = max(at.Generation, 1)
					}
				}
			}
		}
	}
}

// foldPhase is p read on today's phases: a phase a recorded release entered that today's workflow
// lacks is read as the phase its MoveSource folds it into, the phase the move into it left.
func foldPhase(p phase.Phase) phase.Phase {
	if p == PhaseAwaitingMerge {
		return phase.Merging
	}
	return p
}

// pastImplementing says whether p stands after implementing in phase.Order.
func pastImplementing(p phase.Phase) bool {
	return slices.Index(phase.Order, p) > slices.Index(phase.Order, phase.Implementing)
}

// moved reads one `workflow: phase changed` line, or one of the two lines of a parent's wait on
// its open children (ChildrenWaitMoves): the move that ends the visit the issue stands in and
// begins the next one, or, for a move into a phase today's workflow folds into the one it left, a
// mark inside the visit. Either ends the issue's hold, which no move of the record is made under
// (holdWentOn).
func (b *traceBuilder) moved(l PhaseChanged) {
	at := l.Time
	iss := b.issue(l.Issue)
	b.holdWentOn(iss, at, "")
	from, to := foldPhase(l.From), foldPhase(l.To)
	source, sourced := MoveSources[l.Trigger]
	if wait, waits := ChildrenWaitMoves[l.Trigger]; waits {
		source, sourced = wait, true
	}
	if !sourced {
		b.unplaced(UnplacedMove, l.Issue, at, "the move of %s from %s to %s at %s is by %s, which has no source in MoveSources", l.Issue, l.From, l.To, stamp(at), l.Trigger)
	}
	visit := iss.openVisit()
	switch {
	case visit == nil && len(iss.Entries) == 0:
		// The issue stood in from where the log begins, unless Dispatch shows it filed after that,
		// in which case no event the trace read entered it.
		enter, before := b.t.Start, true
		if len(iss.Dispatch) > 0 && iss.Dispatch[0].At.After(b.t.Start) {
			enter, before = iss.Dispatch[0].At, false
			b.unplaced(UnplacedEntry, l.Issue, at, "%s moves out of %s at %s, and none of its Dispatch events entered it into the workflow", l.Issue, l.From, stamp(at))
		}
		iss.Entries = append(iss.Entries, Entry{At: enter, Tree: l.Tree, Generation: 1})
		iss.Visits = append(iss.Visits, Visit{Phase: from, Enter: enter, Before: before})
	case visit == nil:
		b.unplaced(UnplacedMove, l.Issue, at, "%s moves out of %s at %s while the trace holds it in no phase", l.Issue, l.From, stamp(at))
		iss.Entries = append(iss.Entries, Entry{At: at, Tree: l.Tree, Generation: iss.Generation() + 1, Reopen: true})
		iss.Visits = append(iss.Visits, Visit{Phase: from, Enter: at})
	case visit.Phase != from && visit.Enter.Before(b.t.Start):
		// The moves between the entry and this one came before the log begins.
		visit.Exit = b.t.Start
		iss.Visits = append(iss.Visits, Visit{Phase: from, Enter: b.t.Start, Before: true})
	case visit.Phase != from:
		b.unplaced(UnplacedMove, l.Issue, at, "%s moves out of %s at %s while the trace holds it in %s", l.Issue, l.From, stamp(at), visit.Phase)
		visit.Exit = at
		iss.Visits = append(iss.Visits, Visit{Phase: from, Enter: at})
	}
	visit = iss.openVisit()
	if pastImplementing(visit.Phase) {
		b.pullRequestSeen(iss, visit.Enter, 0)
	}
	if entry := iss.entry(); entry != nil && l.Tree != "" {
		entry.Tree = l.Tree
	}
	if from == to {
		iss.Ready = append(iss.Ready, at)
		return
	}
	move := Move{At: at, From: from, To: to, RecordedFrom: l.From, RecordedTo: l.To, Trigger: l.Trigger, Source: source}
	if from == phase.Merging {
		for _, drop := range slices.Backward(iss.QueueDrops) {
			if !drop.At.Before(visit.Enter) && !drop.At.After(at) {
				move.Drop = drop.Line
				break
			}
		}
	}
	if phase.MovesBack(from, to) {
		if cause, named := CauseOf(SendBack{Trigger: l.Trigger, From: from, To: to, Drop: move.Drop}); named {
			move.Cause = cause
		} else {
			b.unplaced(UnplacedMove, l.Issue, at, "the move back of %s from %s to %s at %s by %s has no cause", l.Issue, from, to, stamp(at), l.Trigger)
		}
	}
	out := move
	visit.Exit, visit.Out = at, &out
	if l.Trigger == workflow.TriggerImplementationReady || pastImplementing(to) {
		b.pullRequestSeen(iss, at, 0)
	}
	if to != phase.Done {
		in := move
		iss.Visits = append(iss.Visits, Visit{Phase: to, Enter: at, In: &in})
	}
	if l.Trigger == TriggerChildrenOpen {
		// The move is the parent's wait on its open children, which lasts while it stands in
		// awaiting_children with a child open under it (childrenWaitEnds), as the snapshot
		// timeline reads it (snapshots.openChildren); the daemon writes it no children-first line.
		iss.Waits = append(iss.Waits, Wait{Kind: WaitChildren, Start: at})
	}
}

// pullRequestSeen records the trace's sight of iss's pull request at at, numbered number where a
// line names it.
func (b *traceBuilder) pullRequestSeen(iss *IssueTrace, at time.Time, number int) {
	if iss.PullRequest.IsZero() || at.Before(iss.PullRequest) {
		iss.PullRequest = at
	}
	if number > 0 {
		iss.PullRequestNumber = number
	}
}

// queueLeft reads a line of an issue's pull request leaving the merge queue.
func (b *traceBuilder) queueLeft(key string, at time.Time, msg string, number int, why string) {
	iss := b.issue(key)
	iss.QueueDrops = append(iss.QueueDrops, QueueDrop{At: at, Line: msg, Why: why})
	b.pullRequestSeen(iss, at, number)
}

// mergeWaits reads a line of a merged issue's close waiting for the issues beneath it
// (MergedCloseWaits): the first of the visit to merging the issue stands in is the moment its pull
// request merged, the visit's Merge, and a later one, which the daemon writes as it reads the wait
// again, changes nothing. A line of an issue the trace holds in no visit to merging is one it
// cannot place.
func (b *traceBuilder) mergeWaits(l MergedCloseWaits) {
	at := l.Time
	iss := b.issue(l.Issue)
	b.pullRequestSeen(iss, at, l.PullRequest)
	visit := iss.openVisit()
	if visit == nil || visit.Phase != phase.Merging {
		b.unplaced(UnplacedMove, l.Issue, at, "%s at %s holds the close of %s's merge, and the trace holds it in no visit to merging",
			l.Msg, stamp(at), l.Issue)
		return
	}
	if visit.Merge == nil {
		visit.Merge = &Move{At: at, From: phase.Merging, To: phase.Done, RecordedFrom: phase.Merging, RecordedTo: phase.Done,
			Trigger: workflow.TriggerPullRequestMerged, Source: MoveSources[workflow.TriggerPullRequestMerged]}
	}
}

// slotMoved reads one per-slot line. Where the window holds the record line, the move waits for
// the record line of its pass to take its place (passMoved), so a pass is read once.
func (b *traceBuilder) slotMoved(move SlotMove) {
	if b.recordLines {
		b.buffered = append(b.buffered, move)
		return
	}
	b.t.Slots = append(b.t.Slots, move)
}

// passMoved reads the record line of one pass: each slot it took back and gave out, at the line's
// time, with the slots in use after each move read back from the slots after the pass (every take
// comes before every give), and each candidate it passed over. The per-slot lines of the same
// pass, written before it, are dropped one for one; any other per-slot line read since the last
// record line is a pass of a release that wrote no record line, and is read from its own line
// first.
func (b *traceBuilder) passMoved(l PassMovedSlots) {
	consume := func(issue string, given bool) {
		if i := slices.IndexFunc(b.buffered, func(m SlotMove) bool { return m.Issue == issue && m.Given == given }); i >= 0 {
			b.buffered = slices.Delete(b.buffered, i, i+1)
		}
	}
	for _, taken := range l.Taken {
		consume(taken.Issue, false)
	}
	for _, given := range l.Given {
		consume(given.Issue, true)
	}
	b.t.Slots = append(b.t.Slots, b.buffered...)
	b.buffered = b.buffered[:0]
	before := l.Slots - len(l.Given)
	for i, taken := range l.Taken {
		b.t.Slots = append(b.t.Slots, SlotMove{At: l.Time, Issue: taken.Issue, Slots: before + len(l.Taken) - 1 - i, Cap: l.Cap,
			Reason: taken.Reason})
	}
	for i, given := range l.Given {
		b.t.Slots = append(b.t.Slots, SlotMove{At: l.Time, Issue: given.Issue, Given: true, Slots: before + i + 1, Cap: l.Cap, Kind: given.Kind})
	}
	for _, passed := range l.Passed {
		b.t.Passed = append(b.t.Passed, PassedOver{At: l.Time, Issue: passed.Issue, Kind: passed.Kind, Place: passed.Place, Reason: passed.Reason})
	}
}

// personWaitSubject reads what a root's wait on a person is on from the reason its line gives
// (workflow's enterAwaiting): the ask of a question, or the version of a spec a person reviews.
func personWaitSubject(reason string) (ask string, version int) {
	var question string
	if _, err := fmt.Sscanf(reason, "question %s is awaiting an answer", &question); err == nil {
		return question, 0
	}
	if _, err := fmt.Sscanf(reason, "spec version %d is awaiting a person's review", &version); err == nil {
		return "", version
	}
	return "", 0
}

// latestArtifact is the document of iss's latest gate registration at or before at, "" for none.
func latestArtifact(iss *IssueTrace, at time.Time) string {
	for _, gate := range slices.Backward(iss.Gates) {
		if gate.Kind == GateRegisteredFact && !gate.At.After(at) {
			return gate.Artifact
		}
	}
	return ""
}

// phaseRole is the role that works p, as the daemon's notice of a hold from p names it
// (workflow.HeldNotice): the role whose failed claim holds an issue standing in p.
func phaseRole(p phase.Phase) claim.Role {
	return workflow.HeldNotice(record.Issue{Phase: phase.Held, Hold: &record.Hold{From: p}}).Role
}

// holdBegins begins iss's hold from the phase from, of role's worker, for reason; where a hold
// already stands (an escalation of it), its reason is the newer one.
func (b *traceBuilder) holdBegins(iss *IssueTrace, at time.Time, from phase.Phase, role claim.Role, reason string) {
	if wait := iss.openWait(WaitHeld); wait != nil {
		wait.Reason = reason
		return
	}
	iss.Waits = append(iss.Waits, Wait{Kind: WaitHeld, Start: at, From: from, Role: role, Reason: reason})
}

// holdWentOn ends iss's hold at at where the record shows the held phase going on with no line of
// a release (WaitWentOn): at a launch of a claim of the issue in the hold's role (role), or at the
// issue's next recorded move (role empty); a launch of another role ends nothing.
//
// A window of a release that writes no `workflow: a phase is held` reads each hold from the failed
// claim of the phase's worker (ClaimFailed), and that release writes a line where a hold ends only
// where the engine releases it (workflow/engine.go at 958173da, the commit of legion-v20.42.5:
// claimFailed moves the issue into held and writes no line, and retryPhase, which every retry and a
// held root's todo release through, writes `workflow: a held phase is released`). Such a window
// shows most of those failures followed by no release line at all, but by the same claim launched
// again, the phase's worker running once more, or by a move whose line names the phase the worker
// failed in as the one the issue left. The move's line is written by transition with the phase the
// issue stood in (from := issue.Phase), held for a held issue, and the engine's table holds no row
// out of held, so the issue stood in its phase, not held, when it moved. Whatever ended the hold,
// or kept the failure from holding at all, wrote no line, and the record has the issue working its
// phase again from the relaunch or the move on; the hold ends there, and a replay releases it at
// that moment as it releases a recorded one (driver.queueHoldEnd), so its next piece opens on the
// retry and the recorded move is applied. A hold no relaunch, move, release or leave ends stands at
// the window's end, as the record leaves it. It answers whether it ended a hold.
func (b *traceBuilder) holdWentOn(iss *IssueTrace, at time.Time, role claim.Role) bool {
	wait := iss.openWait(WaitHeld)
	if wait == nil || role != "" && wait.Role != role {
		return false
	}
	wait.End, wait.EndedBy = at, WaitWentOn
	return true
}

// freshRetried reads the operator's retry of the worker of iss's phase in at at (the release line
// of operatorRetryReason with no hold standing) as the retry with a fresh session where the launch
// of that worker's claim afresh that the retry made stands before the release line in the record
// (IssueTrace.FreshRetries): `legion claims retry --fresh-session` of a suspended claim.
//
// The daemon writes the two lines in that order and a fraction of a second apart, never at one
// moment (api/freshsession.go at 958173da and since): api's (*server).freshSession posts the
// claim's resume, whose launch ((*Machine).launchTries) logs `supervise: launched` at once, and
// only then restarts the phase (intake.RestartPhase), whose commit ((*Engine).retryPhase) logs
// `workflow: a held phase is released`. So the launch is matched to the line by their order in the
// log, which the trace reads in order, and not by their moments: the last launch of the role's
// claim before this line, in the visit the retry restarts. It is the retry's where it launched
// afresh and ended no hold, and no turn of the role began after it (Launch.Turns): a plain retry
// of a stopped worker writes its release line before its launch, so the launch before the line is
// the worker's own start, which its turns followed.
func (b *traceBuilder) freshRetried(iss *IssueTrace, at time.Time, in phase.Phase) {
	retried := foldPhase(in)
	role := phaseRole(retried)
	visit := iss.openVisit()
	if role == "" || visit == nil || visit.Phase != retried {
		return
	}
	for _, launch := range slices.Backward(iss.Launches) {
		if launch.At.Before(visit.Enter) {
			return
		}
		if launch.Role != role {
			continue
		}
		if !launch.Resumed && !launch.EndedHold && !slices.ContainsFunc(iss.Turns[launch.Turns:], func(turn Turn) bool {
			return turn.Claim.Role == role
		}) {
			iss.FreshRetries = append(iss.FreshRetries, FreshRetry{At: at, Phase: retried, Role: role})
		}
		return
	}
}

// left reads an issue's leaving the workflow: the visit it stands in ends, and so do its hold and
// every turn of its claims. Its waits on a person and on other issues end there too, once every
// event is read (finish), since an answer read later may have ended one sooner.
func (b *traceBuilder) left(iss *IssueTrace, at time.Time, status string) {
	iss.Leaves = append(iss.Leaves, Leave{At: at, Status: status})
	if visit := iss.openVisit(); visit != nil {
		visit.Exit, visit.Left = at, true
	}
	if wait := iss.openWait(WaitHeld); wait != nil {
		wait.End, wait.EndedBy = at, WaitLeft
	}
	if n := len(iss.MergeForwards); n > 0 && iss.MergeForwards[n-1].End.IsZero() {
		iss.MergeForwards[n-1].End = at
	}
	for _, open := range b.turns {
		if open.issue == iss.Key {
			b.turnEnds(iss.Turns[open.index].Claim, at, TurnEndLeft, false)
		}
	}
}

// checkClosed reads the close of iss's production check at at. Where the issue stands outside the
// workflow, closeCheck retires the claim of the role the check is for (issue_close), which writes
// no line of the retirement's own in any release (CheckClosed), so the open turn of that claim ends
// here. The role is the tester, the one role a deployment's production_check names (config's
// readProject). An issue back in the workflow keeps its claims for the run it is in (closeCheck),
// and their turns go on.
func (b *traceBuilder) checkClosed(iss *IssueTrace, at time.Time) {
	if iss.openVisit() != nil {
		return
	}
	for _, open := range b.turns {
		if open.issue == iss.Key && iss.Turns[open.index].Claim.Role == claim.RoleTester {
			b.turnEnds(iss.Turns[open.index].Claim, at, TurnEndRetired, false)
		}
	}
}

// turnStarts opens a turn of a claim.
func (b *traceBuilder) turnStarts(l TurnStarted) {
	if _, open := b.turns[l.Claim.Token]; open {
		b.unplaced(UnplacedTurn, l.Claim.Issue, l.Time, "%s starts a turn at %s while its turn before is open; that turn ends here", l.Claim.Token, stamp(l.Time))
		b.turnEnds(l.Claim, l.Time, TurnEndLine, false)
	}
	turn := Turn{Claim: l.Claim, Generation: l.Generation, Start: l.Time}
	if l.Claim.Issue == "" {
		// The controller's claim names no issue, and the trace keeps none of its turns: it holds the
		// claim's open turn alone, so its next start and end read as they do for any claim.
		b.turns[l.Claim.Token] = turnAt{}
		return
	}
	iss := b.issue(l.Claim.Issue)
	iss.Turns = append(iss.Turns, turn)
	b.turns[l.Claim.Token] = turnAt{issue: iss.Key, index: len(iss.Turns) - 1}
}

// turnEnds ends ref's open turn at at. A turn's own end line with no turn open is a line the trace
// cannot place (the turn began before the inputs reach, or its start was lost); a failure, a
// suspend, the end of the claim's process or a leave ends no turn where none is open, and says
// nothing.
func (b *traceBuilder) turnEnds(ref ClaimRef, at time.Time, by TurnEnd, ownLine bool) {
	open, ok := b.turns[ref.Token]
	if !ok {
		if ownLine {
			b.unplaced(UnplacedTurn, ref.Issue, at, "%s ends a turn at %s and no turn of it is open", ref.Token, stamp(at))
		}
		return
	}
	delete(b.turns, ref.Token)
	if open.issue == "" {
		return
	}
	turns := b.t.Issues[open.issue].Turns
	turns[open.index].End, turns[open.index].EndedBy = at, by
}

// finish builds what the reading left to build once every line and event is read: the generations
// of the children that entered a live tree, the per-slot moves no record line took, each issue's
// slot spans, the slots it infers held from before the reach, each merger's move back that a post
// names as one for a conflict (mergerConflicts), each issue's link, children and person waits' ends
// and the sessions of its claims, the slots it infers held at the window's start, its check of the
// slots against the record's count there, each issue's worker starts and segments, and the ticks;
// and it ends what stands at the window's end. A session whose title names an issue the trace holds
// nothing else of runs no claim a replay writes, and is left out.
func (b *traceBuilder) finish() {
	b.childGenerations()
	b.t.Slots = append(b.t.Slots, b.buffered...)
	b.buffered = nil
	slices.SortStableFunc(b.t.Slots, func(x, y SlotMove) int { return x.At.Compare(y.At) })
	b.spans()
	b.inferSlots()
	keys := slices.Sorted(maps.Keys(b.t.Issues))
	for _, key := range keys {
		iss := b.t.Issues[key]
		if len(iss.Visits) > 0 && len(iss.Dispatch) == 0 {
			b.unplaced(UnplacedEntry, key, iss.Visits[0].Enter, "%s runs in the workflow and the window holds no Dispatch event of it", key)
		}
		b.mergerConflicts(iss)
		b.linkWaits(iss)
		b.childrenWaitEnds(iss)
		b.personWaitEnds(iss)
		iss.Sessions = b.sessions[key]
	}
	b.inferStartSlots()
	b.countSlots()
	for _, key := range keys {
		iss := b.t.Issues[key]
		slices.SortStableFunc(iss.Waits, func(x, y Wait) int { return x.Start.Compare(y.Start) })
		for i := range iss.Waits {
			if iss.Waits[i].EndedBy == "" {
				iss.Waits[i].EndedBy = WaitOpen
			}
		}
		for i := range iss.Turns {
			if iss.Turns[i].EndedBy == "" {
				iss.Turns[i].EndedBy = TurnEndOpen
			}
		}
		b.forWait(iss)
		b.segments(iss)
	}
	b.ticks()
}

// spans builds each issue's slot spans from the slot moves, in order. A slot taken back with none
// held is one the issue held where the log begins; a slot given out while one is held is a line
// the trace cannot place, and the slot held ends there.
func (b *traceBuilder) spans() {
	for _, move := range b.t.Slots {
		iss := b.issue(move.Issue)
		n := len(iss.Slots)
		holding := n > 0 && iss.Slots[n-1].End.IsZero()
		switch {
		case move.Given && holding:
			b.unplaced(UnplacedSlot, move.Issue, move.At, "%s takes a slot at %s while it holds one", move.Issue, stamp(move.At))
			iss.Slots[n-1].End = move.At
			iss.Slots = append(iss.Slots, SlotSpan{Start: move.At, Kind: move.Kind})
		case move.Given:
			iss.Slots = append(iss.Slots, SlotSpan{Start: move.At, Kind: move.Kind})
		case holding:
			iss.Slots[n-1].End, iss.Slots[n-1].Reason = move.At, move.Reason
		default:
			iss.Slots = append(iss.Slots, SlotSpan{Start: b.t.Start, End: move.At, Before: true, Reason: move.Reason})
		}
	}
}

// inferSlots gives each issue the log first shows in a phase that keeps a slot, and that no slot
// line names, the slot it held from before the reach (assumption 34). The log first shows it in a
// phase where the visit it stands in at Start was read from a line: begun before the log
// (Visit.Before) or ended by a move out of that phase. The phase keeps a slot under the rule of
// the window's first part, or main where the window names none (Trace.ruleAt). An issue whose
// first slot line gives a slot back held one from before the reach too, which spans reads from that
// line. With no line of it, the slot is held until the rule would give it back: the first move of
// that run into a phase the rule keeps no slot in, or the run's end, its move to done or its leave;
// zero while the run goes on at the window's end. Each such slot is counted (Trace.InferredSlots).
func (b *traceBuilder) inferSlots() {
	for _, key := range slices.Sorted(maps.Keys(b.t.Issues)) {
		iss := b.t.Issues[key]
		first := slices.IndexFunc(iss.Visits, func(visit Visit) bool {
			return !visit.Enter.After(b.t.Start) && (visit.Exit.IsZero() || visit.Exit.After(b.t.Start))
		})
		if len(iss.Slots) > 0 || first < 0 {
			continue
		}
		standing := iss.Visits[first]
		shown := standing.Before || standing.Out != nil // read from a line: begun before the log, or ended by a move it holds
		if !shown || !b.t.ruleAt(standing.Enter).PhaseKeepsSlot(standing.Phase) {
			continue
		}
		span := SlotSpan{Start: b.t.Start, Before: true, Inferred: true, End: b.slotKeptUntil(iss, first)}
		iss.Slots = append(iss.Slots, span)
		b.t.InferredSlots++
	}
}

// inferStartSlots gives each issue that stands at the window's start in a worker's phase its rule
// keeps a slot in, and that holds none there by the slot lines, the slot it held from its last
// entry into that run of phases (assumption 34): its last slot went back before the start, or no
// line names one, and it took one again by a way the window holds no slot line of. The issue
// holds no slot span that stands at the start; no wait of it stands at the start that gives the
// slot back under the rule (a link or its children where the rule gives the slot back for them, a
// person or a hold where it keeps none awaiting a person or held); and a worker of the run started
// in it after its last slot went back and before the start, which is the record's sight of the
// slot, since the daemon starts the worker of a phase that keeps a slot only once its issue holds
// one (workflow.StartOrOwe). An issue whose work came back with no slot and whose worker has not
// started owes its start (Seed's owedStarts), and an architect's turn starts on no slot, so neither
// is given one. The slot runs from the later of the run's entry and the end of the issue's last
// slot until the rule gives it back (slotKeptUntil, as inferSlots reads it); a slot line after the
// start that gives the issue a slot before then shows it held none, and leaves it without. Each is
// counted (Trace.InferredSlots). An issue the slot puts in flight at the start has its link waits
// read again, since they are read in flight alone, and one of them that stands at the start, which
// would have given the slot back, takes it away again.
func (b *traceBuilder) inferStartSlots() {
	from := b.t.From
	rule := b.t.ruleAt(from)
	for _, key := range slices.Sorted(maps.Keys(b.t.Issues)) {
		iss := b.t.Issues[key]
		at := slices.IndexFunc(iss.Visits, func(visit Visit) bool { return standsAt(visit.Enter, visit.Exit, from) })
		if at < 0 || !workerPhase(iss.Visits[at].Phase) || !b.t.ruleAt(iss.Visits[at].Enter).PhaseKeepsSlot(iss.Visits[at].Phase) ||
			slotStandsAt(iss, from) || waitGivesBackAt(iss, from, rule) {
			continue
		}
		first := at
		for first > 0 {
			visit, before := iss.Visits[first], iss.Visits[first-1]
			if visit.In == nil || !before.Exit.Equal(visit.Enter) || before.Phase == phase.Admitted ||
				!b.t.ruleAt(before.Enter).PhaseKeepsSlot(before.Phase) {
				break
			}
			first--
		}
		start := iss.Visits[first].Enter
		for _, span := range iss.Slots {
			if !span.End.IsZero() && span.End.Before(from) && span.End.After(start) {
				start = span.End
			}
		}
		started := slices.ContainsFunc(iss.Turns, func(turn Turn) bool {
			if turn.Start.Before(start) || !turn.Start.Before(from) {
				return false
			}
			visit := visitOf(iss, turn.Start)
			return visit != nil && workerPhase(visit.Phase) && turn.Claim.Role == phaseRole(visit.Phase)
		})
		if !started {
			continue
		}
		span := SlotSpan{Start: start, Inferred: true, End: b.slotKeptUntil(iss, at)}
		if slices.ContainsFunc(iss.Slots, func(given SlotSpan) bool {
			return given.Start.After(from) && (span.End.IsZero() || given.Start.Before(span.End))
		}) {
			continue
		}
		flying := withinStretches(iss.inFlight(), from)
		place := len(iss.Slots)
		for place > 0 && iss.Slots[place-1].Start.After(span.Start) {
			place--
		}
		iss.Slots = slices.Insert(iss.Slots, place, span)
		if !flying {
			b.readLinkWaitsAgain(iss)
			if waitGivesBackAt(iss, from, rule) {
				iss.Slots = slices.Delete(iss.Slots, place, place+1)
				b.readLinkWaitsAgain(iss)
				continue
			}
		}
		b.t.InferredSlots++
	}
}

// slotKeptUntil is when the rule gives back the slot iss holds from its visit at index from on:
// the first move of that run into a phase the rule keeps no slot in, or the run's end, its move to
// done or its leave; zero while the run goes on at the window's end.
func (b *traceBuilder) slotKeptUntil(iss *IssueTrace, from int) time.Time {
	var end time.Time
	for i := from; i < len(iss.Visits); i++ {
		visit := iss.Visits[i]
		if i > from && visit.In == nil {
			break // a later run, which began with an entry of its own
		}
		if !b.t.ruleAt(visit.Enter).PhaseKeepsSlot(visit.Phase) {
			return visit.Enter
		}
		if end = visit.Exit; end.IsZero() {
			break
		}
	}
	return end
}

// workerPhase says whether p is a phase a worker other than the architect works (phaseRole), whose
// start waits for a slot where the rule keeps one in p.
func workerPhase(p phase.Phase) bool {
	role := phaseRole(p)
	return role != "" && role != claim.RoleArchitect
}

// slotStandsAt says whether a slot span of iss stands at at: begun at or before it and not ended
// before it.
func slotStandsAt(iss *IssueTrace, at time.Time) bool {
	return slices.ContainsFunc(iss.Slots, func(span SlotSpan) bool {
		return !span.Start.After(at) && (span.End.IsZero() || !span.End.Before(at))
	})
}

// waitGivesBackAt says whether a wait of iss that stands at at (standsAt) gives the issue's slot
// back under rule: a link or children wait where the rule gives the slot back for it and the wait
// has lasted the rule's grace by at (record.SlotRule.GraceOver), a wait on a person where the rule
// keeps no slot awaiting one, a hold where it keeps none held.
func waitGivesBackAt(iss *IssueTrace, at time.Time, rule record.SlotRule) bool {
	return slices.ContainsFunc(iss.Waits, func(wait Wait) bool {
		if !standsAt(wait.Start, wait.End, at) {
			return false
		}
		switch wait.Kind {
		case WaitLink:
			return rule.OrMain().LinkWaitGivesBack && rule.GraceOver(wait.Start, at)
		case WaitChildren:
			return rule.OrMain().ChildrenWaitGivesBack && rule.GraceOver(wait.Start, at)
		case WaitPerson:
			return !rule.PhaseKeepsSlot(phase.AwaitingPerson)
		case WaitHeld:
			return !rule.PhaseKeepsSlot(phase.Held)
		}
		return false
	})
}

// readLinkWaitsAgain reads iss's link waits again from its slots as they stand (linkWaits), which
// are read in the stretches it is in flight alone.
func (b *traceBuilder) readLinkWaitsAgain(iss *IssueTrace) {
	iss.Waits = slices.DeleteFunc(iss.Waits, func(wait Wait) bool { return wait.Kind == WaitLink })
	b.linkWaits(iss)
}

// countSlots checks the slots the trace holds against the record's own count at the window's
// start, where the start state places them (SlotCount): at the first slot move the log holds at or
// after the start, or, where it holds none, at the last before it, whose count stands at the start.
// It counts the slots that stand just before that move, begun before it or before the reach and
// not given back before it, seen and inferred alike, and that move's own slot, given out or taken
// back.
func (b *traceBuilder) countSlots() {
	if len(b.t.Slots) == 0 {
		return
	}
	at := slices.IndexFunc(b.t.Slots, func(move SlotMove) bool { return !move.At.Before(b.t.From) })
	if at < 0 {
		at = len(b.t.Slots) - 1
	}
	first := b.t.Slots[at]
	held := 0
	for _, iss := range b.t.Issues {
		for _, span := range iss.Slots {
			if (span.Before || span.Start.Before(first.At)) && (span.End.IsZero() || !span.End.Before(first.At)) {
				held++
			}
		}
	}
	if first.Given {
		held++
	} else {
		held--
	}
	b.t.SlotCount = SlotCount{At: first.At, Recorded: first.Slots, Held: held}
}

// holdsSlotAt says whether iss holds a slot at at: a slot that began at or before it and ends
// after it.
func (iss *IssueTrace) holdsSlotAt(at time.Time) bool {
	return slices.ContainsFunc(iss.Slots, func(span SlotSpan) bool {
		return !span.Start.After(at) && (span.End.IsZero() || span.End.After(at))
	})
}

// dispatchStateAt is the issue state of the newest issue event of iss at or before at, nil for
// none, read by binary search over its issue events, which are in time order, each issue's indexed
// once (shownOf) the first time it is read, once every event is read.
func (b *traceBuilder) dispatchStateAt(iss *IssueTrace, at time.Time) *DispatchIssueState {
	if b.shown == nil {
		b.shown = map[string]shownStates{}
	}
	shown, indexed := b.shown[iss.Key]
	if !indexed {
		shown = shownOf(iss)
		b.shown[iss.Key] = shown
	}
	return shown.stateAt(at)
}

// inFlight is each stretch in which iss was in flight: from the first slot it took after an entry
// to the end of that run, its last visit's exit (zero while it runs at the window's end).
func (iss *IssueTrace) inFlight() [][2]time.Time {
	var stretches [][2]time.Time
	for i, entry := range iss.Entries {
		next := time.Time{}
		if i+1 < len(iss.Entries) {
			next = iss.Entries[i+1].At
		}
		within := func(at time.Time) bool { return !at.Before(entry.At) && (next.IsZero() || at.Before(next)) }
		start := time.Time{}
		for _, span := range iss.Slots {
			if within(span.Start) || span.Before && i == 0 {
				start = span.Start
				break
			}
		}
		if start.IsZero() {
			continue
		}
		end := time.Time{}
		for _, visit := range iss.Visits {
			if within(visit.Enter) {
				end = visit.Exit
			}
		}
		stretches = append(stretches, [2]time.Time{start, end})
	}
	return stretches
}

// withinStretches says whether at falls in one of stretches.
func withinStretches(stretches [][2]time.Time, at time.Time) bool {
	return slices.ContainsFunc(stretches, func(s [2]time.Time) bool {
		return !at.Before(s[0]) && (s[1].IsZero() || at.Before(s[1]))
	})
}

// visitAt is the visit iss stands in at at, nil for none.
func (iss *IssueTrace) visitAt(at time.Time) *Visit {
	for i := range iss.Visits {
		visit := &iss.Visits[i]
		if !visit.Enter.After(at) && (visit.Exit.IsZero() || visit.Exit.After(at)) {
			return visit
		}
	}
	return nil
}

// beforeReview says whether p is a phase before the review's end: admitted, planning,
// implementing, testing or reviewing.
func beforeReview(p phase.Phase) bool {
	return p == phase.Admitted || p == phase.Planning || p == phase.Implementing || p == phase.Testing || p == phase.Reviewing
}

// measuredPhase says whether p is a phase in which the measures count an issue's waits on other
// issues: before its review's end (beforeReview), or awaiting_children, where a started parent
// waits on its open children under a rule whose wait stops the phase (cybermonklabs/legion#4592).
func measuredPhase(p phase.Phase) bool {
	return beforeReview(p) || p == phase.AwaitingChildren
}

// linkWaits builds iss's waits on its Dispatch blocked_by links: a stretch in which the issue is
// in flight and a blocker its links name is unfinished (not done, or with no event the window
// holds), begun while the issue stands before its review, from the moment both hold to the moment
// no unfinished blocker is left (the last one done, or the last unfinished link taken off) or the
// issue's run ends.
func (b *traceBuilder) linkWaits(iss *IssueTrace) {
	stretches := iss.inFlight()
	if len(stretches) == 0 {
		return
	}
	named := map[string]bool{}
	var moments []time.Time
	for _, event := range iss.Dispatch {
		if event.Issue == nil {
			continue
		}
		moments = append(moments, event.At)
		for _, key := range event.Issue.BlockedBy {
			named[key] = true
		}
	}
	if len(named) == 0 {
		return
	}
	for key := range named {
		if blocker, ok := b.t.Issues[key]; ok {
			for _, event := range blocker.Dispatch {
				moments = append(moments, event.At)
			}
		}
	}
	for _, stretch := range stretches {
		moments = append(moments, stretch[0])
		if !stretch[1].IsZero() {
			moments = append(moments, stretch[1])
		}
	}
	for _, visit := range iss.Visits {
		moments = append(moments, visit.Enter)
	}
	slices.SortFunc(moments, time.Time.Compare)
	moments = slices.CompactFunc(moments, time.Time.Equal)

	unfinished := func(at time.Time) []string {
		state := b.dispatchStateAt(iss, at)
		if state == nil {
			return nil
		}
		var open []string
		for _, key := range state.BlockedBy {
			blocker, ok := b.t.Issues[key]
			if !ok {
				open = append(open, key)
				continue
			}
			if shown := b.dispatchStateAt(blocker, at); shown == nil || shown.Status != record.DoneStatus {
				open = append(open, key)
			}
		}
		return open
	}
	var waiting *Wait
	var before []string
	for _, at := range moments {
		open := unfinished(at)
		flying := withinStretches(stretches, at)
		if waiting != nil {
			switch {
			case !flying:
				waiting.End, waiting.EndedBy = at, WaitLeft
			case len(open) == 0:
				waiting.End, waiting.EndedBy = at, WaitBlockerDone
				if state := b.dispatchStateAt(iss, at); state != nil {
					for _, key := range before {
						if !slices.Contains(state.BlockedBy, key) {
							waiting.EndedBy = WaitLinkRemoved
						}
					}
				}
			}
			if !waiting.End.IsZero() {
				iss.Waits = append(iss.Waits, *waiting)
				waiting = nil
			}
		}
		if waiting == nil && flying && len(open) > 0 {
			if visit := iss.visitAt(at); visit != nil && measuredPhase(visit.Phase) {
				waiting = &Wait{Kind: WaitLink, Start: at, On: open}
			}
		}
		before = open
	}
	if waiting != nil {
		iss.Waits = append(iss.Waits, *waiting)
	}
}

// childrenOf is every issue an issue event of the window ever shows under parent: the only issues
// that can be its open children at any moment. The index is built on the first call, once every
// event is read.
func (b *traceBuilder) childrenOf(parent string) []string {
	if b.children == nil {
		b.children = map[string][]string{}
		for _, key := range slices.Sorted(maps.Keys(b.t.Issues)) {
			for _, event := range b.t.Issues[key].Dispatch {
				if event.Issue != nil && event.Issue.Parent != "" && event.Issue.Parent != key &&
					!slices.Contains(b.children[event.Issue.Parent], key) {
					b.children[event.Issue.Parent] = append(b.children[event.Issue.Parent], key)
				}
			}
		}
	}
	return b.children[parent]
}

// childrenAt is the open Dispatch children of parent among candidates at at, by the daemon's own
// reading of an open child (record.ChildOpen): each whose newest issue event at or before at names
// parent, read with its own record where the trace holds it in the workflow or out of it.
func (b *traceBuilder) childrenAt(parent string, candidates []string, at time.Time) []string {
	var open []string
	for _, key := range candidates {
		child := b.t.Issues[key]
		state := b.dispatchStateAt(child, at)
		if state == nil || state.Parent != parent {
			continue
		}
		var recorded *record.Issue
		if entry := child.entryAt(at); entry != nil {
			rec := record.Issue{Key: key, Tree: entry.Tree, Phase: phase.Done, Status: state.Status}
			if visit := child.visitAt(at); visit != nil {
				rec.Phase = visit.Phase
			}
			recorded = &rec
		}
		if record.ChildOpen(record.DispatchIssueRow{Key: key, Status: state.Status, Parent: parent}, recorded) {
			open = append(open, key)
		}
	}
	return open
}

// entryAt is iss's latest entry at or before at, nil before its first.
func (iss *IssueTrace) entryAt(at time.Time) *Entry {
	for i := len(iss.Entries) - 1; i >= 0; i-- {
		if !iss.Entries[i].At.After(at) {
			return &iss.Entries[i]
		}
	}
	return nil
}

// nextWaitStart is the start of iss's next wait of wait's kind after it, zero for none: a wait the
// daemon entered again had ended before it.
func (iss *IssueTrace) nextWaitStart(wait *Wait) time.Time {
	for _, other := range iss.Waits {
		if other.Kind == wait.Kind && other.Start.After(wait.Start) {
			return other.Start
		}
	}
	return time.Time{}
}

// visitExitAfter is the moment the visit iss stands in at at ends, and whether it ends by the
// issue's leaving the workflow; zero while it stands at the window's end.
func (iss *IssueTrace) visitExitAfter(at time.Time) (time.Time, bool) {
	if visit := iss.visitAt(at); visit != nil {
		return visit.Exit, visit.Left
	}
	return time.Time{}, false
}

// childrenWaitEnds ends each of iss's children waits, which its line began: at the first moment
// after it began that no open child is left (the last child closed, or moved out from under it),
// that its tree's design gate closed, that the issue left the workflow or that its phase moved on,
// or, failing all of those, at the start of the issue's next children wait. A wait whose children
// or gate ended it begins again where the record's daemon held the issue for its children again
// with no line of it (childrenWaitAgain), and that wait is ended the same way.
func (b *traceBuilder) childrenWaitEnds(iss *IssueTrace) {
	candidates := b.childrenOf(iss.Key)
	var again []Wait
	for i := range iss.Waits {
		if iss.Waits[i].Kind != WaitChildren || iss.Waits[i].EndedBy != "" {
			continue
		}
		since := iss.Waits[i].Start
		b.childrenWaitEnd(iss, candidates, &iss.Waits[i])
		for ended := iss.Waits[i]; ; {
			next, begins := b.childrenWaitAgain(iss, candidates, ended, since)
			if !begins {
				break
			}
			b.childrenWaitEnd(iss, candidates, &next)
			again = append(again, next)
			ended = next
		}
	}
	iss.Waits = append(iss.Waits, again...)
}

// childrenWaitEnd ends wait, a children wait of iss (childrenWaitEnds), among candidates, every
// issue ever shown under it.
func (b *traceBuilder) childrenWaitEnd(iss *IssueTrace, candidates []string, wait *Wait) {
	wait.On = b.childrenAt(iss.Key, candidates, wait.Start)
	type change struct {
		at  time.Time
		end WaitEnd
	}
	var changes []change
	for _, key := range candidates {
		for _, event := range b.t.Issues[key].Dispatch {
			if event.Issue != nil && event.At.After(wait.Start) {
				changes = append(changes, change{at: event.At})
			}
		}
	}
	// A closed gate ends a children-first wait, the hold of a first phase, and leaves the wait of
	// a parent in awaiting_children standing (record.LineFacts.StartedParentWaits).
	if tree, ok := b.t.Issues[treeOf(iss, wait.Start)]; ok && !inAwaitingChildren(iss, wait.Start) {
		for _, gate := range tree.Gates {
			if gate.Kind == GateClosedFact && gate.At.After(wait.Start) {
				changes = append(changes, change{at: gate.At, end: WaitGateClosed})
			}
		}
	}
	if exit, left := iss.visitExitAfter(wait.Start); !exit.IsZero() {
		end := WaitMovedOn
		if left {
			end = WaitLeft
		}
		changes = append(changes, change{at: exit, end: end})
	}
	if next := iss.nextWaitStart(wait); !next.IsZero() {
		changes = append(changes, change{at: next, end: WaitMovedOn})
	}
	slices.SortStableFunc(changes, func(x, y change) int { return x.at.Compare(y.at) })
	for _, c := range changes {
		if c.end == "" {
			if open := b.childrenAt(iss.Key, candidates, c.at); len(open) > 0 {
				continue
			}
			c.end = WaitChildDone
			for _, key := range wait.On {
				if state := b.dispatchStateAt(b.t.Issues[key], c.at); state != nil && state.Parent != iss.Key {
					c.end = WaitChildMovedOut
				}
			}
		}
		wait.End, wait.EndedBy = c.at, c.end
		return
	}
}

// inAwaitingChildren says whether iss stands in awaiting_children at at.
func inAwaitingChildren(iss *IssueTrace, at time.Time) bool {
	visit := iss.visitAt(at)
	return visit != nil && visit.Phase == phase.AwaitingChildren
}

// childrenWaitAgain is the children wait of iss that begins after ended, a children wait its
// children or its tree's gate ended, and whether one does: at the first moment after ended's end
// that an open child stands under the issue again with its tree's gate open, while the issue
// stands in the visit it waited in and before its next children wait's line. The daemon judges the
// wait afresh at each pass (record.ChildWaits) but writes its line once for a generation of the
// tree (admit's tellChildrenFirst, record.Store.TellChildrenFirst), so a root whose last open child
// closed at a full cap, and that its architect files another child under before its own first
// phase can start, waits for that child again with no line; read from the line alone, it would
// stand waiting for a slot alone. The gate stands open where the line began it (since), the daemon
// judging only an issue of an open gate, and from then as the tree's gate facts leave it, read as
// gateReader reads them. The children-first wait holds a first phase alone under every rule
// (record.SlotRule.ChildrenFirstCandidates), so the wait begins again only in the visit it waited
// in: a started parent's wait on its open children is cybermonklabs/legion#4592's move into
// awaiting_children (record.SlotRule.WaitStopsPhase), which the log gives as the parent's own
// phase and no children-first line.
func (b *traceBuilder) childrenWaitAgain(iss *IssueTrace, candidates []string, ended Wait, since time.Time) (Wait, bool) {
	if ended.EndedBy != WaitChildDone && ended.EndedBy != WaitChildMovedOut && ended.EndedBy != WaitGateClosed {
		return Wait{}, false
	}
	until, _ := iss.visitExitAfter(ended.Start)
	if next := iss.nextWaitStart(&ended); !next.IsZero() && (until.IsZero() || next.Before(until)) {
		until = next
	}
	before := func(at time.Time) bool { return at.After(ended.End) && (until.IsZero() || at.Before(until)) }
	var gates []GateFact
	if tree, ok := b.t.Issues[treeOf(iss, ended.Start)]; ok {
		gates = tree.Gates
	}
	var moments []time.Time
	for _, key := range candidates {
		for _, event := range b.t.Issues[key].Dispatch {
			if event.Issue != nil && before(event.At) {
				moments = append(moments, event.At)
			}
		}
	}
	for _, gate := range gates {
		if before(gate.At) {
			moments = append(moments, gate.At)
		}
	}
	slices.SortFunc(moments, time.Time.Compare)
	moments = slices.CompactFunc(moments, time.Time.Equal)
	gate, read := gateReader{lines: b.t.GateLines}, 0
	for ; read < len(gates) && !gates[read].At.After(since); read++ {
		gate.apply(gates[read])
	}
	gate.open = true
	for _, at := range moments {
		for ; read < len(gates) && !gates[read].At.After(at); read++ {
			gate.apply(gates[read])
		}
		if gate.open && len(b.childrenAt(iss.Key, candidates, at)) > 0 {
			return Wait{Kind: WaitChildren, Start: at}, true
		}
	}
	return Wait{}, false
}

// treeOf is the tree iss stood in at at: its latest entry's at or before it, or its own where it had
// entered none.
func treeOf(iss *IssueTrace, at time.Time) string {
	if entry := iss.entryAt(at); entry != nil {
		return entry.Tree
	}
	return iss.Key
}

// personWaitEnds ends each of iss's waits on a person, which its line began: at the first act of a
// person on the root after the wait's Dispatch sequence (an answer to or the resolution of an ask,
// a verdict on its document) or a newer version of the document a spec review waits on; or at the
// root's leaving, its phase moving on, or the start of its next wait on a person, whichever is
// first.
func (b *traceBuilder) personWaitEnds(iss *IssueTrace) {
	for i := range iss.Waits {
		wait := &iss.Waits[i]
		if wait.Kind != WaitPerson || wait.EndedBy != "" {
			continue
		}
		for _, event := range iss.Dispatch {
			if event.Seq <= wait.SinceSeq || event.At.Before(wait.Start) {
				continue
			}
			person := event.Actor.Person() && (event.Ask != nil && event.Type != "ask.opened" || event.Verdict != nil)
			if person || wait.Version > 0 && event.Type == "artifact.version" {
				wait.End, wait.EndedBy = event.At, WaitAnswered
				break
			}
		}
		if exit, left := iss.visitExitAfter(wait.Start); !exit.IsZero() && (wait.End.IsZero() || exit.Before(wait.End)) {
			wait.End, wait.EndedBy = exit, WaitMovedOn
			if left {
				wait.EndedBy = WaitLeft
			}
		}
		if next := iss.nextWaitStart(wait); !next.IsZero() && (wait.End.IsZero() || next.Before(wait.End)) {
			wait.End, wait.EndedBy = next, WaitMovedOn
		}
	}
}

// forWait marks each of iss's slots that a pass took back for a wait on another issue, or for an
// idle return, which marks its issue as a release for a wait does (record.TakenIdleReturn): by the
// reason its record line gives, or, where the window holds no record line, by a link or children
// wait of the issue that stands at the moment the slot went back. A slot the trace infers went back
// where the rule gives it back (inferSlots), never for a wait, which a pass writes a line of.
func (b *traceBuilder) forWait(iss *IssueTrace) {
	for i := range iss.Slots {
		span := &iss.Slots[i]
		if span.End.IsZero() || span.Inferred {
			continue
		}
		switch span.Reason {
		case record.TakenLinkWait, record.TakenChildrenWait, record.TakenIdleReturn:
			span.ForWait = true
		case "":
			span.ForWait = slices.ContainsFunc(iss.Waits, func(wait Wait) bool {
				return (wait.Kind == WaitLink || wait.Kind == WaitChildren) && !wait.Start.After(span.End) &&
					(wait.End.IsZero() || wait.End.After(span.End))
			})
		}
	}
}

// ruleAt is the slot rule the window's daemon ran at at: the rule of the part that holds it, or of
// the nearest part, as the repository keeps it (record.SlotRuleNamed, which BuildTrace read once
// for each part, refusing a window whose part names a rule the repository does not keep), and the
// daemon's own rule where the window names no part.
func (t *Trace) ruleAt(at time.Time) record.SlotRule {
	if len(t.Parts) == 0 {
		return record.MainRule
	}
	held := 0
	for i, part := range t.Parts {
		if !part.From.After(at) {
			held = i
		}
	}
	return t.rules[held]
}

// segments sets each of iss's visits' worker start and work segment. A visit to merging whose
// close waited for the issues beneath it ends its work at the merge (Visit.Merge), where its worker
// started by then, however much later the close's move to done came, or none at all in the window.
func (b *traceBuilder) segments(iss *IssueTrace) {
	for i := range iss.Visits {
		visit := &iss.Visits[i]
		visit.WorkerStart = b.workerStart(iss, visit)
		if visit.WorkerStart.IsZero() {
			continue
		}
		visit.Workerless = workerPhase(visit.Phase) && visit.WorkerStart.Equal(visit.Enter) && !iss.workerShownIn(visit)
		segment := &WorkSegment{Start: visit.WorkerStart, EndedBy: SegmentOpen}
		switch {
		case visit.Merge != nil && !visit.Merge.At.Before(visit.WorkerStart):
			segment.End, segment.EndedBy = visit.Merge.At, SegmentEndHeldMerge
		case visit.Out != nil && visit.Out.Trigger == workflow.TriggerSignOff && b.memberLeftAt(iss, visit.Out.At):
			segment.End, segment.EndedBy = visit.Out.At, SegmentEndMove
			if end := lastTurnEnd(iss, visit.WorkerStart, visit.Exit); !end.IsZero() && end.Before(visit.Out.At) {
				segment.End, segment.EndedBy = end, SegmentEndHeldSignOff
			}
		case visit.Out != nil && visit.Out.Source.Way != MovePass:
			segment.End, segment.EndedBy = visit.Out.At, SegmentEndMove
		case visit.Out != nil:
			for _, gate := range iss.Gates {
				if gate.Kind == GateRegisteredFact && !gate.At.Before(visit.WorkerStart) && !gate.At.After(visit.Exit) {
					segment.End, segment.EndedBy = gate.At, SegmentEndRegistration
				}
			}
			if segment.End.IsZero() {
				if end := lastTurnEnd(iss, visit.WorkerStart, visit.Exit); !end.IsZero() {
					segment.End, segment.EndedBy = end, SegmentEndTurn
				}
			}
		case visit.Left:
			segment.EndedBy = SegmentEndLeft
		}
		visit.Segment = segment
	}
}

// workerShownIn says whether the record shows the worker of visit's phase at work in visit: a launch
// of its role's claim, a turn of it, or a hold of it begun in the visit, which a claim that failed
// to launch leaves where it writes no launch line.
func (iss *IssueTrace) workerShownIn(visit *Visit) bool {
	role := phaseRole(visit.Phase)
	inside := func(at time.Time) bool {
		return !at.Before(visit.Enter) && (visit.Exit.IsZero() || at.Before(visit.Exit))
	}
	return slices.ContainsFunc(iss.Launches, func(launch Launch) bool { return launch.Role == role && inside(launch.At) }) ||
		slices.ContainsFunc(iss.Turns, func(turn Turn) bool { return turn.Claim.Role == role && inside(turn.Start) }) ||
		slices.ContainsFunc(iss.Waits, func(wait Wait) bool { return wait.Kind == WaitHeld && wait.Role == role && inside(wait.Start) })
}

// lastTurnEnd is the latest end of a turn of iss that began in [from, to) and ended by to, zero for
// none.
func lastTurnEnd(iss *IssueTrace, from, to time.Time) time.Time {
	var last time.Time
	for _, turn := range iss.Turns {
		if !turn.Start.Before(from) && turn.Start.Before(to) && !turn.End.IsZero() && !turn.End.After(to) && turn.End.After(last) {
			last = turn.End
		}
	}
	return last
}

// memberLeftAt says whether another issue of iss's tree left the workflow at at: the moment the
// engine applies again a sign-off it held while a member beneath was in the workflow.
func (b *traceBuilder) memberLeftAt(iss *IssueTrace, at time.Time) bool {
	entry := iss.entryAt(at)
	if entry == nil {
		return false
	}
	for key, member := range b.t.Issues {
		if key == iss.Key || !slices.ContainsFunc(member.Leaves, func(leave Leave) bool { return leave.At.Equal(at) }) {
			continue
		}
		if memberEntry := member.entryAt(at); memberEntry != nil && memberEntry.Tree == entry.Tree {
			return true
		}
	}
	return false
}

// workerStart is when visit's worker started: where the issue holds a slot as it enters, the phase
// change; where it takes one in the visit, that slot; in a phase whose rule keeps no slot, the
// phase change; otherwise the first turn in the visit of the role that works its phase. Zero where
// none of them is.
func (b *traceBuilder) workerStart(iss *IssueTrace, visit *Visit) time.Time {
	if iss.holdsSlotAt(visit.Enter) {
		return visit.Enter
	}
	for _, span := range iss.Slots {
		if span.Start.After(visit.Enter) && (visit.Exit.IsZero() || span.Start.Before(visit.Exit)) {
			return span.Start
		}
	}
	if !b.t.ruleAt(visit.Enter).PhaseKeepsSlot(visit.Phase) {
		return visit.Enter
	}
	role := phaseRole(visit.Phase)
	for _, turn := range iss.Turns {
		if turn.Claim.Role == role && !turn.Start.Before(visit.Enter) && (visit.Exit.IsZero() || turn.Start.Before(visit.Exit)) {
			return turn.Start
		}
	}
	return time.Time{}
}

// ticks sets the controller ticks of each boot: a minute after it, then every TickInterval, until
// the next boot or the end of the window's inputs (Trace.End), to which a replay runs.
func (b *traceBuilder) ticks() {
	for i, boot := range b.t.Boots {
		until := b.t.End
		if i+1 < len(b.t.Boots) && b.t.Boots[i+1].At.Before(until) {
			until = b.t.Boots[i+1].At
		}
		for at := boot.At.Add(tickDelay); at.Before(until); at = at.Add(b.t.TickInterval) {
			b.t.Ticks = append(b.t.Ticks, at)
			if b.t.TickInterval <= 0 {
				break
			}
		}
	}
}
