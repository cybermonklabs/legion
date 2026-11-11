package legion

// board_rules.go holds the two rules a Legion card carries beside the facts the daemon reports —
// what the slot an issue holds is used for, and why the issue waits — and the column table both
// of them read. Two readers answer the waiting rule and the column table: the board route, which
// writes the reason on a card (internal/dispatch/api/legion_board.go), and the per-issue facts,
// which compute it for an issue with no card at all
// (internal/dispatch/legionfacts/facts.go). A rule stated in the route alone is a rule the facts
// state a second time, and one rule stated twice is a card and a fact that disagree about the
// same issue. The slot rule (SlotUse) has one reader today, the board route, and it stands here
// beside the other for the same reason: a second reader of it would state nothing of its own.
//
// Every rule here takes plain values — a phase, a column, the roles in a turn, a slot — so a
// reader that builds no card can call it. The two observations a card rule reads are decoded
// here as well (ParseWait, ParseSlot), for the same reason: a second decode of one field is how
// two readers come to read one daemon's word two ways. The labels the view draws are not here:
// they belong to the view, and the route holds them beside the order it draws the columns in.

import (
	"encoding/json"
	"time"
)

// The board's columns. Queue holds the daemon's waiting line and, after it, the issues a blocker
// link holds, of the issues that have not started work: no slot and no working phase
// (WorkingColumns); an issue of either list in a working phase, or holding a slot, is in its
// phase's column with its place in the line on its card. Design holds the issues whose phase is
// still `admitted` that are on a card at all — the slot holders, whose architect runs at the
// design gate, and the ones no card stands above, which are on no other issue's count; Merging
// `retro`, `merging` and `awaiting_merge`; and Held the phase metric 2 puts on the "no role" bar
// beside `awaiting_person` (the plan's D11).
const (
	ColumnQueue        = "queue"
	ColumnDesign       = "design"
	ColumnPlanning     = "planning"
	ColumnImplementing = "implementing"
	ColumnTesting      = "testing"
	ColumnReviewing    = "reviewing"
	ColumnAccepting    = "accepting"
	ColumnMerging      = "merging"
	ColumnAwaiting     = "awaiting_person"
	ColumnHeld         = "held"
)

// PhaseColumns is the column each phase the daemon reports falls in. A phase this does not name
// (`done`, and any phase a later daemon adds) puts the issue on no card: the board shows the work
// in flight, and a phase Dispatch has never heard of is not something it can place. The Merging
// column holds the merger's phases, which are declared once beside the phases themselves
// (MergerPhases) and read here rather than spelled again.
var PhaseColumns = func() map[string]string {
	columns := map[string]string{
		PhaseAdmitted:       ColumnDesign,
		PhasePlanning:       ColumnPlanning,
		PhaseImplementing:   ColumnImplementing,
		PhaseTesting:        ColumnTesting,
		PhaseReviewing:      ColumnReviewing,
		PhaseAccepting:      ColumnAccepting,
		PhaseAwaitingPerson: ColumnAwaiting,
		PhaseHeld:           ColumnHeld,
	}
	for _, phase := range MergerPhases {
		columns[phase] = ColumnMerging
	}
	return columns
}()

// WorkingColumns are the columns of the phases a worker works in. An issue in one of them has
// started its work, so the Queue never holds it, even where the daemon's waiting line names it
// while it waits for a slot to come back.
var WorkingColumns = map[string]bool{
	ColumnPlanning:     true,
	ColumnImplementing: true,
	ColumnTesting:      true,
	ColumnReviewing:    true,
	ColumnAccepting:    true,
	ColumnMerging:      true,
}

// Queued is what a reader of one issue knows of the daemon's waiting line. A read that recorded
// no admission block knows no line at all, which is a different answer than a recorded line that
// does not name the issue, and both are different from a line that names it.
type Queued int

const (
	// QueuedUnread is for a project no read has recorded an admission block of: every read a
	// daemon before this release answered, and every project whose daemon has not answered yet.
	QueuedUnread Queued = iota
	// QueuedNo is for an issue that a recorded block's waiting line does not name and no
	// blocker link of it holds.
	QueuedNo
	// QueuedYes is for an issue that a recorded block's waiting line names, or that a blocker
	// link holds.
	QueuedYes
)

// Column is the board column an issue stands in: the Queue for a queued issue that has not
// started work, and the column of its phase for every other issue. It is "" for an issue whose
// phase no column holds (`done`, and any phase a later daemon adds), which puts it on no card at
// all. The board draws the columns from this rule and the stored facts read the same rule, so a
// card and a fact can never place one issue in two columns.
//
// phase is the daemon's phase of the issue, slot the slot it holds of its own, and queued what
// the reader knows of the daemon's waiting line. An issue that has started its work keeps the
// column of its phase while it waits: it holds a slot of its own, or its phase is a working one
// (WorkingColumns). A slot holder in `admitted` is a root whose architect runs at the design
// gate.
//
// With no admission block read (QueuedUnread) the Queue holds the issues that are still
// `admitted` and hold no slot, which are the issues waiting for one. The board draws them in
// Dispatch's rank order, which is the order the daemon's own line follows once a root's wait is
// answered. The fallback is here rather than in either reader, so the card and the stored row
// of one issue cannot disagree about why that issue waits.
func Column(phase string, slot *int, queued Queued) string {
	column, named := PhaseColumns[phase]
	switch queued {
	case QueuedYes:
		if !named || (slot == nil && !WorkingColumns[column]) {
			return ColumnQueue
		}
	case QueuedUnread:
		if phase == PhaseAdmitted && slot == nil {
			return ColumnQueue
		}
	}
	return column
}

// What the slot an issue holds of its own is used for: a worker of it is in a turn, nobody of it
// is in a turn in a phase of its own, it waits on a person, or it holds the slot for its
// children, who either work or do not.
const (
	SlotUseWorking      = "working"
	SlotUseBetweenTurns = "between_turns"
	SlotUseHeldChildren = "held_children"
	SlotUseHeldPerson   = "held_person"
	SlotUseHeldIdle     = "held_idle"
)

// heldForChildren are the phases in which a slot holder holds its slot for its children alone:
// its own work is done until they answer.
var heldForChildren = map[string]bool{
	PhaseAccepting:      true,
	PhaseAwaitingMerge:  true,
	PhaseAwaitingPerson: true,
}

// ChildWork are the phases of a child that keep its root's held slot busy. A reader of SlotUse
// answers its childWorks from this set over the children of the tree it holds.
var ChildWork = map[string]bool{
	PhaseImplementing: true,
	PhaseTesting:      true,
	PhaseReviewing:    true,
}

// SlotUse is what the slot an issue holds of its own is used for, and "" for an issue that holds
// no slot of its own. It is the one rule: the Legion tab worked this out itself until
// dispatch://INFRA-979's contract moved it to the server. The board route writes the answer on
// every card as `slot_use`, and the tab draws none of it yet.
//
// A worker in a turn (an architect's included) makes the slot working, since a turn is the work
// the slot was given for. With no worker in a turn, a root waiting on a person holds it for the
// person; a root in a phase of its own holds it between turns; and a root in a phase it can only
// leave once its children answer (heldForChildren) holds it for them — busy where a child of its
// tree works (childWorks), idle where none does.
//
// phase is the daemon's phase of the issue, roles the roles of it that are in a turn, slot the
// slot it holds of its own, and childWorks whether a child of its tree is in a phase of
// ChildWork.
func SlotUse(phase string, roles []string, slot *int, childWorks bool) string {
	if slot == nil {
		return ""
	}
	switch {
	case len(roles) > 0:
		return SlotUseWorking
	case phase == PhaseAwaitingPerson:
		return SlotUseHeldPerson
	case !heldForChildren[phase]:
		return SlotUseBetweenTurns
	case childWorks:
		return SlotUseHeldChildren
	default:
		return SlotUseHeldIdle
	}
}

// Why an issue waits: it is ready for the daemon to start it, it works nothing while it waits
// for a slot to come back, a blocker link holds it, or a person has not answered it.
const (
	WaitingReady   = "ready"
	WaitingSlot    = "slot"
	WaitingBlocked = "blocked"
	WaitingPerson  = "person"
)

// Wait is the daemon's record of what a root waits on, as the sampler wrote it: the kind of wait
// (`ask`, `gate`, or whatever a later daemon names), when it began, and when a person answered
// it. An answered root waits for a slot at the front of the queue rather than for a person.
type Wait struct {
	Kind       string
	Since      *time.Time
	AnsweredAt *time.Time
}

// ParseWait is the wait one `waiting_on` observation records (FieldWaitingOn). The second answer
// is whether the value was read: it is false for a value of another shape than this one, and for
// a wait that names no kind, because the daemon owns the wire shape and a reader of one names
// what it expects rather than trusting it. A reader then does what it does with a value it
// cannot read — the board keeps the issue's previous wait, and the stored facts read no wait at
// all.
func ParseWait(value []byte) (*Wait, bool) {
	var wait struct {
		Kind       string     `json:"kind"`
		Since      *time.Time `json:"since"`
		AnsweredAt *time.Time `json:"answeredAt"`
	}
	if err := json.Unmarshal(value, &wait); err != nil || wait.Kind == "" {
		return nil, false
	}
	return &Wait{Kind: wait.Kind, Since: wait.Since, AnsweredAt: wait.AnsweredAt}, true
}

// ParseSlot is the slot one `slot` observation records (FieldSlot), nil where the issue holds
// none. The second answer is whether the value was read, which is false for a value of another
// shape, as ParseWait's is.
func ParseSlot(value []byte) (*int, bool) {
	var slot *int
	if err := json.Unmarshal(value, &slot); err != nil {
		return nil, false
	}
	return slot, true
}

// Waiting is why an issue waits, since when, and the issues a blocker link holds it on. Since is
// when the wait began: when the person was asked for a wait on a person, and the phase entry
// otherwise; nil where neither was recorded.
type Waiting struct {
	Reason    string
	Since     *time.Time
	BlockedBy []string
}

// WaitingInput is what CardWaiting reads of one issue: the daemon's phase, the column the issue
// is on, its place in the daemon's waiting line from 1 (nil for an issue the line does not name),
// the issues a blocker link holds it on, when it entered its phase, and the wait the daemon
// recorded of it.
type WaitingInput struct {
	Phase        string
	Column       string
	WaitingPlace *int
	BlockedBy    []string
	PhaseSince   *time.Time
	WaitingOn    *Wait
}

// CardWaiting is why the issue waits, and nil for an issue that waits for nothing. It is the one
// rule, which the Legion tab's waiting line reads rather than working out again.
//
// An issue waits when the daemon's line names it (WaitingPlace), when it is in the Queue column,
// or when it waits on a person. Of those, a blocker link holds the ones it names; an issue the
// line names that is on a column of its own has started its work and waits for a slot to come
// back; an issue whose wait on a person no answer has ended waits for the person; and the rest
// are ready for the daemon to start them.
func CardWaiting(in WaitingInput) *Waiting {
	placed := in.WaitingPlace != nil || in.Column == ColumnQueue
	if !placed && in.Column != ColumnAwaiting {
		return nil
	}
	// An issue out of the line that is on the awaiting-a-person column waits for the person
	// whatever the daemon recorded of the wait; an issue in the line waits for one while its own
	// wait stands unanswered.
	onPerson := !placed || (in.WaitingOn != nil && in.WaitingOn.AnsweredAt == nil)
	waiting := Waiting{Reason: WaitingReady, BlockedBy: in.BlockedBy}
	switch {
	case placed && len(in.BlockedBy) > 0:
		waiting.Reason = WaitingBlocked
	case placed && in.Column != ColumnQueue && in.Phase != PhaseAwaitingPerson:
		waiting.Reason = WaitingSlot
	case onPerson:
		waiting.Reason = WaitingPerson
	}
	waiting.Since = in.PhaseSince
	if onPerson && in.WaitingOn != nil && in.WaitingOn.Since != nil {
		waiting.Since = in.WaitingOn.Since
	}
	return &waiting
}
