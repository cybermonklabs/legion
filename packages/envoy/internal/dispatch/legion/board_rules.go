package legion

// board_rules.go holds the column table a Legion card stands in, and the two rules a Legion card
// carries beside the facts the daemon reports — what the slot an issue holds is used for, and why
// the issue waits. Three readers place an issue: the board route, which draws its cards in the
// columns and writes the reason on each card (internal/dispatch/api/legion_board.go), the metrics,
// which key each issue's hours by the same columns (internal/dispatch/legion/metrics), and the
// per-issue facts, which compute the column and the reason for an issue with no card at all
// (internal/dispatch/legionfacts/facts.go). All of them read the table and the rules here
// (Columns, PhaseColumns, which is read off it, ColumnOf and CardWaiting): a rule stated in the
// route alone is a rule the metrics or the facts state a second time, and one rule stated twice is
// a card and a fact that disagree about the same issue. The slot rule (SlotUse) has one reader
// today, the board route, and it stands here beside the others for the same reason: a second
// reader of it would state nothing of its own.
//
// Every rule here takes plain values — a phase, a column, the roles in a turn, a slot — so a
// reader that builds no card can call it. The two observations a card rule reads are decoded
// here as well (ParseWait, ParseSlot), for the same reason: a second decode of one field is how
// two readers come to read one daemon's word two ways. The labels the view draws are not here:
// they belong to the view, and the route holds them beside the column each one names.

import (
	"encoding/json"
	"fmt"
	"time"
)

// The board's columns. Queue holds no phase of its own (Columns): it holds the daemon's waiting
// line and, after it, the issues a blocker link holds, of the issues that have not started work:
// no slot and no working phase (WorkingColumns); an issue of either list in a working phase, or
// holding a slot, is in its phase's column with its place in the line on its card. Design holds
// the issues whose phase is still `admitted` that are on a card at all — the slot holders, whose
// architect runs at the design gate, and the ones no card stands above, which are on no other
// issue's count; Retro `retro` alone; Merging `merging` and `awaiting_merge`; Waiting on children
// the parents in `awaiting_children`, the phase dispatch://INFRA-3320 added, whether or not the
// daemon's blocked list names them (ColumnOf); ColumnAwaiting, Waiting on a person,
// `awaiting_person`; and Held the phase metric 2 puts on the "no role" bar beside
// `awaiting_person` and `awaiting_children` (the plan's D11).
const (
	ColumnQueue            = "queue"
	ColumnDesign           = "design"
	ColumnPlanning         = "planning"
	ColumnImplementing     = "implementing"
	ColumnTesting          = "testing"
	ColumnReviewing        = "reviewing"
	ColumnAccepting        = "accepting"
	ColumnRetro            = "retro"
	ColumnMerging          = "merging"
	ColumnAwaitingChildren = "awaiting_children"
	ColumnAwaiting         = "awaiting_person"
	ColumnHeld             = "held"
)

// Column is one column of the board: its id, and the phases the daemon reports that the board
// places in it, in the order an issue passes through them.
type Column struct {
	ID     string
	Phases []string
}

// Columns is every column of the board with the phases each holds. The order is the board's
// left-to-right order; the metrics read each column's phases alone (partsOf in legion/metrics).
// Queue holds no phase of its own: the column rule fills it from the daemon's waiting line
// (ColumnOf). An issue the queue does not hold, in a phase no column holds (`done`, and any phase
// a later daemon adds), is on no card: the board shows the work in flight, and a phase Dispatch
// has never heard of is not something it can place. Which phases share a column is the board's
// own choice, made here: `retro` has a column of its own, as it has a word of its own in the
// spec's tables ("Phase words in tables"), Merging holds the two phases those tables name
// merging, `merging` and `awaiting_merge` (dispatch://INFRA-1077's b74df7a79b), and
// `awaiting_children` has a column of its own: a parent in it keeps no slot and waits for none,
// and no role works it. The metrics and the per-issue facts key an issue's hours by these
// columns, so an issue's retro hours are under `retro` there too, and on no merger figure, and a
// parent's hours waiting on its children are under `awaiting_children`.
var Columns = []Column{
	{ColumnQueue, nil},
	{ColumnDesign, []string{PhaseAdmitted}},
	{ColumnPlanning, []string{PhasePlanning}},
	{ColumnImplementing, []string{PhaseImplementing}},
	{ColumnTesting, []string{PhaseTesting}},
	{ColumnReviewing, []string{PhaseReviewing}},
	{ColumnAccepting, []string{PhaseAccepting}},
	{ColumnRetro, []string{PhaseRetro}},
	{ColumnMerging, []string{PhaseMerging, PhaseAwaitingMerge}},
	{ColumnAwaitingChildren, []string{PhaseAwaitingChildren}},
	{ColumnAwaiting, []string{PhaseAwaitingPerson}},
	{ColumnHeld, []string{PhaseHeld}},
}

// PhaseColumns is the column each phase the daemon reports falls in, read off Columns, and no
// entry for a phase no column holds.
var PhaseColumns = phaseColumns(Columns)

// phaseColumns indexes columns by phase: each phase a column holds, against that column's id. A
// phase is in one column: it panics on a phase it has already placed, naming the phase and both
// columns, since the metrics give a phase's hours to the first column that lists it (partsOf,
// issueParts) and the board puts its card in the column this index names.
func phaseColumns(columns []Column) map[string]string {
	byPhase := map[string]string{}
	for _, column := range columns {
		for _, phase := range column.Phases {
			if placed, held := byPhase[phase]; held {
				panic(fmt.Sprintf("legion: phase %q is in column %q and in column %q; a phase is in one column", phase, placed, column.ID))
			}
			byPhase[phase] = column.ID
		}
	}
	return byPhase
}

// WorkingColumns are the columns of the phases a worker works in. An issue in one of them has
// started its work, so the Queue never holds it, even where the daemon's waiting line names it
// while it waits for a slot to come back. No worker works `awaiting_children`, so its column is
// none of them, and a rule of its own keeps a parent in it out of the Queue (ColumnOf).
var WorkingColumns = map[string]bool{
	ColumnPlanning:     true,
	ColumnImplementing: true,
	ColumnTesting:      true,
	ColumnReviewing:    true,
	ColumnAccepting:    true,
	ColumnRetro:        true,
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

// ColumnOf is the board column an issue stands in: the Queue for a queued issue that has not
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
// A parent in `awaiting_children` stands in Waiting on children whatever the line says. The
// daemon lists such a parent as blocked on its open children while one is open
// (dispatch://INFRA-3320, R11), but it waits on them and not on a slot, so a blocker link of that
// list queues it nowhere: its card is in Waiting on children for the whole wait, naming its open
// children while the daemon lists them (WaitingLine's BlockedBy), and stays there after its last
// child closes until the pass that returns it.
//
// With no admission block read (QueuedUnread) the Queue holds the issues that are still
// `admitted` and hold no slot, which are the issues waiting for one. The board draws them in
// Dispatch's rank order, which is the order the daemon's own line follows once a root's wait is
// answered. The fallback is here rather than in either reader, so the card and the stored row
// of one issue cannot disagree about why that issue waits.
func ColumnOf(phase string, slot *int, queued Queued) string {
	column, named := PhaseColumns[phase]
	switch queued {
	case QueuedYes:
		if !named || (slot == nil && !WorkingColumns[column] && column != ColumnAwaitingChildren) {
			return ColumnQueue
		}
	case QueuedUnread:
		if waitingForASlot(phase, slot) {
			return ColumnQueue
		}
	}
	return column
}

// waitingForASlot is whether the issue is waiting for an admission slot: the daemon reports it
// `admitted` and it holds no slot of its own. Such an issue has not started its work, which both
// the Queue's fallback above and the waiting line's count of an unstarted child read
// (WaitingLine's unstarted), so the two cannot answer one issue differently.
func waitingForASlot(phase string, slot *int) bool {
	return phase == PhaseAdmitted && slot == nil
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
// for a slot to come back, a blocker link holds it (for a parent in `awaiting_children`, the open
// children the daemon lists it as blocked on), or a person has not answered it.
const (
	WaitingReady   = "ready"
	WaitingSlot    = "slot"
	WaitingBlocked = "blocked"
	WaitingPerson  = "person"
)

// Wait is the daemon's record of what a root waits on, as the Legion state intake wrote it: the
// kind of wait (`ask`, `gate`, or whatever a later daemon names), when it began, and when a person
// answered it. An answered root waits for a slot at the front of the queue rather than for a
// person.
type Wait struct {
	Kind       string
	Since      *time.Time
	AnsweredAt *time.Time
}

// ParseWait is the wait one `waiting_on` observation records (FieldWaitingOn). The second answer
// is whether the value was read: it is false for a value of another shape than this one, and for
// a wait that names no kind, because the daemon owns the wire shape and a reader of one names
// what it expects rather than trusting it. A reader then does what it does with a value it
// cannot read — a board card, which decodes the wait the issue's state holds (IssueState), shows
// no wait, and the stored facts read no wait at all.
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

// ParseAdmission is the admission block the project's `admission` observation records
// (FieldAdmission, under AdmissionKey). The second answer is whether the value was read, which is
// false for a value of another shape, as ParseWait's is. The board's header and waiting line and
// the stored facts' waiting line read the block through it.
func ParseAdmission(value []byte) (*Admission, bool) {
	var admission Admission
	if err := json.Unmarshal(value, &admission); err != nil {
		return nil, false
	}
	return &admission, true
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
//
// A parent in the Waiting on children column waits there for the whole of its wait, whatever
// the line says (ColumnOf): its open children hold it while the daemon lists them as what blocks
// it (dispatch://INFRA-3320, R11), and once no list names it, after its last child closed, it is
// ready for the pass that returns it. Either wait began at its entry into `awaiting_children`.
func CardWaiting(in WaitingInput) *Waiting {
	if in.Column == ColumnAwaitingChildren {
		waiting := Waiting{Reason: WaitingReady, Since: in.PhaseSince, BlockedBy: in.BlockedBy}
		if len(in.BlockedBy) > 0 {
			waiting.Reason = WaitingBlocked
		}
		return &waiting
	}
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
