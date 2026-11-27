package legion

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// laterPhase is a phase no column holds and no daemon reports today: one a later daemon adds,
// which every card rule answers as it answers a phase it cannot place.
const laterPhase = "a_phase_a_later_daemon_adds"

// The slots the rows below hold of their own: a slot under the cap, and the slot of a P0 holder
// above it. The daemon starts a P0 issue outside a full cap and counts it in admission.p0Holders;
// the card rules read whether an issue holds a slot of its own and never its number nor the cap,
// so a P0 holder answers as every slot holder does. Its slot here is numbered past a cap of six.
var (
	underTheCap = new(3)
	aboveTheCap = new(6)
)

// The moments the waiting rows read: when an issue entered its phase, when a person was asked,
// and when the person answered.
var (
	rulesEntered  = time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	rulesAsked    = time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)
	rulesAnswered = time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
)

// phasesOf is the phase of each row of a state table, which the inventory of the three card rules
// reads (TestEachCardRuleHasARowForEveryPhase).
func phasesOf[Row any](rows []Row, phase func(Row) string) []string {
	phases := make([]string, 0, len(rows))
	for _, row := range rows {
		phases = append(phases, phase(row))
	}
	return phases
}

// TestEachCardRuleHasARowForEveryPhase: the state tables of the three card rules — the column
// rule (columnRows), the slot rule (slotUseRows) and the waiting rule (cardWaitingRows) — each
// hold a row for every phase Columns lists, for `done`, and for a phase a later daemon adds. A
// phase the daemon gains, as it gained `awaiting_children` after these rules were written, then
// fails here until each rule's answer for it is written down, rather than reaching the board
// through a rule that never named it.
func TestEachCardRuleHasARowForEveryPhase(t *testing.T) {
	t.Parallel()
	phases := []string{PhaseDone, laterPhase}
	for _, column := range Columns {
		phases = append(phases, column.Phases...)
	}
	for _, table := range []struct {
		rule   string
		phases []string
	}{
		{"ColumnOf", phasesOf(columnRows, func(row columnRow) string { return row.phase })},
		{"SlotUse", phasesOf(slotUseRows, func(row slotUseRow) string { return row.phase })},
		{"CardWaiting", phasesOf(cardWaitingRows, func(row waitingRow) string { return row.in.Phase })},
	} {
		for _, phase := range phases {
			if !slices.Contains(table.phases, phase) {
				t.Errorf("%s's state table holds no row for the phase %q", table.rule, phase)
			}
		}
	}
}

// TestEachPhaseIsInOneColumn: PhaseColumns gives every phase of Columns the one column that lists
// it, and phaseColumns panics on a table that lists one phase in two columns, naming the phase
// and both columns, so such a table stops the package at start (PhaseColumns is read off Columns
// then) rather than put a card in one column and its hours under another.
func TestEachPhaseIsInOneColumn(t *testing.T) {
	listed := 0
	for _, column := range Columns {
		for _, phase := range column.Phases {
			listed++
			if got := PhaseColumns[phase]; got != column.ID {
				t.Errorf("PhaseColumns[%q] = %q, want %q, the column that lists it", phase, got, column.ID)
			}
		}
	}
	if len(PhaseColumns) != listed {
		t.Errorf("PhaseColumns holds %d phases, want the %d Columns lists", len(PhaseColumns), listed)
	}

	recovered := func() (value any) {
		defer func() { value = recover() }()
		phaseColumns([]Column{
			{"first", []string{PhaseMerging}},
			{"second", []string{PhaseAwaitingMerge, PhaseMerging}},
		})
		return nil
	}()
	message, ok := recovered.(string)
	if !ok {
		t.Fatalf("phaseColumns over a phase in two columns recovered %#v, want a panic with a message", recovered)
	}
	for _, named := range []string{PhaseMerging, "first", "second"} {
		if !strings.Contains(message, `"`+named+`"`) {
			t.Errorf("the panic %q does not name %q", message, named)
		}
	}
}

// slotUseRow is one row of the slot rule's state table: the plain values its one reader, the
// board's cards, passes, which are the whole input, and the use the rule answers.
type slotUseRow struct {
	name       string
	phase      string
	roles      []string
	slot       *int
	childWorks bool
	use        string
}

// slotUseRows is the slot rule's state table: the five uses a slot an issue holds of its own is
// put to, and the empty answer for an issue that holds no slot, with a row for every phase
// (TestEachCardRuleHasARowForEveryPhase) and rows for a P0 holder above the cap.
var slotUseRows = []slotUseRow{
	{"a worker is in a turn", PhaseImplementing, []string{"implementer"}, underTheCap, false, SlotUseWorking},
	{"an architect is in a turn", PhaseAccepting, []string{"architect"}, underTheCap, true, SlotUseWorking},
	{"nobody is in a turn", PhaseImplementing, nil, underTheCap, false, SlotUseBetweenTurns},
	{"a person has not answered", PhaseAwaitingPerson, nil, underTheCap, false, SlotUseHeldPerson},
	{"a child of the tree works", PhaseAccepting, nil, underTheCap, true, SlotUseHeldChildren},
	{"no child of the tree works", PhaseAccepting, nil, underTheCap, false, SlotUseHeldIdle},
	{"the issue holds no slot of its own", PhaseImplementing, []string{"implementer"}, nil, false, ""},
	{"an architect is in a turn at the design gate", PhaseAdmitted, []string{"architect"}, underTheCap, false, SlotUseWorking},
	{"an issue waiting for a slot holds none", PhaseAdmitted, nil, nil, false, ""},
	{"a planner is in a turn", PhasePlanning, []string{"planner"}, underTheCap, false, SlotUseWorking},
	{"nobody is in a turn in testing", PhaseTesting, nil, underTheCap, false, SlotUseBetweenTurns},
	{"a reviewer is in a turn", PhaseReviewing, []string{"reviewer"}, underTheCap, false, SlotUseWorking},
	{"nobody is in a turn in retro", PhaseRetro, nil, underTheCap, false, SlotUseBetweenTurns},
	{"a merger is in a turn", PhaseMerging, []string{"merger"}, underTheCap, false, SlotUseWorking},
	{"a child of the tree works while the merge waits", PhaseAwaitingMerge, nil, underTheCap, true, SlotUseHeldChildren},
	// A parent waiting on its children keeps no slot: the daemon gives it none while it waits, so
	// it carries no use, even while a child of its tree works.
	{"a parent waits on its children", PhaseAwaitingChildren, nil, nil, true, ""},
	// The daemon keeps a held root's slot (its own state.json holds LEGION-210 in `held` with a
	// slot), and nobody of it is in a turn in a phase of its own.
	{"a held root keeps its slot", PhaseHeld, nil, underTheCap, false, SlotUseBetweenTurns},
	{"a finished issue holds no slot", PhaseDone, nil, nil, false, ""},
	{"a slot holder in a phase a later daemon adds", laterPhase, nil, underTheCap, false, SlotUseBetweenTurns},
	// A P0 holder above the cap answers as every slot holder does.
	{"a P0 holder above the cap is in a turn", PhaseImplementing, []string{"implementer"}, aboveTheCap, false, SlotUseWorking},
	{"a P0 holder above the cap holds its slot for a child that works", PhaseAccepting, nil, aboveTheCap, true, SlotUseHeldChildren},
	{"a P0 holder above the cap waits on a person", PhaseAwaitingPerson, nil, aboveTheCap, false, SlotUseHeldPerson},
}

// TestSlotUseAnswersOneUsePerSlotState: the five uses a slot an issue holds of its own is put to,
// and the empty answer for an issue that holds no slot, a parent waiting on its children among
// them, over a row for every phase; a P0 holder above the cap answers as every slot holder does
// (slotUseRows).
func TestSlotUseAnswersOneUsePerSlotState(t *testing.T) {
	for _, want := range slotUseRows {
		if use := SlotUse(want.phase, want.roles, want.slot, want.childWorks); use != want.use {
			t.Errorf("%s: SlotUse(%s) = %q, want %q", want.name, want.phase, use, want.use)
		}
	}
}

// waitingRow is one row of the waiting rule's state table: what the rule reads of one issue, and
// the reason it answers, "" where it answers that the issue waits for nothing, with the moment
// the wait began.
type waitingRow struct {
	name   string
	in     WaitingInput
	reason string
	since  time.Time
}

// cardWaitingRows is the waiting rule's state table: the four reasons an issue waits for, and the
// nil answer for an issue that waits for nothing, with a row for every phase
// (TestEachCardRuleHasARowForEveryPhase) and rows for a P0 holder above the cap. Since is the
// phase entry, and the moment a person was asked where the issue waits for that person.
var cardWaitingRows = []waitingRow{
	{
		name:   "ready for the daemon to start it",
		in:     WaitingInput{Phase: PhaseAdmitted, Column: ColumnQueue, WaitingPlace: new(1), PhaseSince: &rulesEntered},
		reason: WaitingReady, since: rulesEntered,
	},
	{
		name: "its wait on a person was answered, so it waits for a slot at the front",
		in: WaitingInput{
			Phase: PhaseAwaitingPerson, Column: ColumnQueue, WaitingPlace: new(1), PhaseSince: &rulesEntered,
			WaitingOn: &Wait{Kind: "ask", Since: &rulesAsked, AnsweredAt: &rulesAnswered},
		},
		reason: WaitingReady, since: rulesEntered,
	},
	{
		name:   "it started its work and waits for a slot to come back",
		in:     WaitingInput{Phase: PhaseImplementing, Column: ColumnImplementing, WaitingPlace: new(2), PhaseSince: &rulesEntered},
		reason: WaitingSlot, since: rulesEntered,
	},
	{
		name: "a blocker link holds it",
		in: WaitingInput{
			Phase: PhaseAdmitted, Column: ColumnQueue, WaitingPlace: new(2),
			BlockedBy: []string{"ACME-9"}, PhaseSince: &rulesEntered,
		},
		reason: WaitingBlocked, since: rulesEntered,
	},
	{
		name: "a blocker link holds it, and the line does not name it",
		in: WaitingInput{
			Phase: PhaseAdmitted, Column: ColumnQueue, BlockedBy: []string{"ACME-9", "ACME-10"}, PhaseSince: &rulesEntered,
		},
		reason: WaitingBlocked, since: rulesEntered,
	},
	{
		name: "a person has not answered it",
		in: WaitingInput{
			Phase: PhaseAwaitingPerson, Column: ColumnAwaiting, PhaseSince: &rulesEntered,
			WaitingOn: &Wait{Kind: "gate", Since: &rulesAsked},
		},
		reason: WaitingPerson, since: rulesAsked,
	},
	{
		// Neither the line nor the Queue column nor the awaiting-a-person column holds it.
		name: "it is at work in its own slot",
		in:   WaitingInput{Phase: PhaseImplementing, Column: ColumnImplementing, PhaseSince: &rulesEntered},
	},
	{
		// A parent waits on its children and on no slot. The daemon lists it as blocked on its open
		// children (admission.blocked), which the waiting line answers as its BlockedBy, and its
		// card stands in its own column, neither in the line nor in the Queue.
		name: "a parent waits on its open children",
		in: WaitingInput{
			Phase: PhaseAwaitingChildren, Column: ColumnAwaitingChildren,
			BlockedBy: []string{"ACME-11", "ACME-12"}, PhaseSince: &rulesEntered,
		},
		reason: WaitingBlocked, since: rulesEntered,
	},
	{
		// Its last child closed and the pass that returns it has not run: no list names it, and
		// it still waits in the phase.
		name:   "a parent whose last child closed waits for the pass that returns it",
		in:     WaitingInput{Phase: PhaseAwaitingChildren, Column: ColumnAwaitingChildren, PhaseSince: &rulesEntered},
		reason: WaitingBlocked, since: rulesEntered,
	},
	{
		name:   "it planned and waits for a slot to come back",
		in:     WaitingInput{Phase: PhasePlanning, Column: ColumnPlanning, WaitingPlace: new(1), PhaseSince: &rulesEntered},
		reason: WaitingSlot, since: rulesEntered,
	},
	{name: "a tester is at work in its slot", in: WaitingInput{Phase: PhaseTesting, Column: ColumnTesting, PhaseSince: &rulesEntered}},
	{
		name:   "it is under review and waits for a slot to come back",
		in:     WaitingInput{Phase: PhaseReviewing, Column: ColumnReviewing, WaitingPlace: new(3), PhaseSince: &rulesEntered},
		reason: WaitingSlot, since: rulesEntered,
	},
	{name: "a root holds its slot for its children", in: WaitingInput{Phase: PhaseAccepting, Column: ColumnAccepting, PhaseSince: &rulesEntered}},
	{name: "a retro is at work", in: WaitingInput{Phase: PhaseRetro, Column: ColumnRetro, PhaseSince: &rulesEntered}},
	{
		name:   "it is merging and waits for a slot to come back",
		in:     WaitingInput{Phase: PhaseMerging, Column: ColumnMerging, WaitingPlace: new(1), PhaseSince: &rulesEntered},
		reason: WaitingSlot, since: rulesEntered,
	},
	{name: "a root awaits its merge in its slot", in: WaitingInput{Phase: PhaseAwaitingMerge, Column: ColumnMerging, PhaseSince: &rulesEntered}},
	{name: "a root at the design gate in its slot", in: WaitingInput{Phase: PhaseAdmitted, Column: ColumnDesign, PhaseSince: &rulesEntered}},
	{name: "a held issue", in: WaitingInput{Phase: PhaseHeld, Column: ColumnHeld, PhaseSince: &rulesEntered}},
	// An issue on no column carries no card, and the rule answers that it waits for nothing.
	{name: "a finished issue is on no card", in: WaitingInput{Phase: PhaseDone, PhaseSince: &rulesEntered}},
	{name: "an issue in a phase a later daemon adds is on no card", in: WaitingInput{Phase: laterPhase, PhaseSince: &rulesEntered}},
	// WaitingInput carries no slot: a slot holder is known by the column it stands in, so a P0
	// holder above the cap answers as the slot holders above do.
	{
		name: "a P0 holder above the cap is at work in its slot",
		in:   WaitingInput{Phase: PhaseImplementing, Column: ColumnImplementing, PhaseSince: &rulesEntered},
	},
	{
		name: "a P0 holder above the cap waits on a person",
		in: WaitingInput{
			Phase: PhaseAwaitingPerson, Column: ColumnAwaiting, PhaseSince: &rulesEntered,
			WaitingOn: &Wait{Kind: "ask", Since: &rulesAsked},
		},
		reason: WaitingPerson, since: rulesAsked,
	},
}

// TestCardWaitingAnswersOneReasonPerWait: the four reasons an issue waits for, and the nil answer
// for an issue that waits for nothing, over a row for every phase (cardWaitingRows). A parent in
// `awaiting_children` waits as blocked on its open children from its phase entry, though neither
// the line nor the Queue holds it. The issues a blocker link holds, and a parent's open children,
// are on every wait, whatever the reason the rule answers.
func TestCardWaitingAnswersOneReasonPerWait(t *testing.T) {
	for _, want := range cardWaitingRows {
		waiting := CardWaiting(want.in)
		if want.reason == "" {
			if waiting != nil {
				t.Errorf("%s: CardWaiting = %+v, want nothing", want.name, *waiting)
			}
			continue
		}
		if waiting == nil {
			t.Errorf("%s: CardWaiting = nil, want %s", want.name, want.reason)
			continue
		}
		if waiting.Reason != want.reason {
			t.Errorf("%s: CardWaiting = %s, want %s", want.name, waiting.Reason, want.reason)
		}
		if waiting.Since == nil || !waiting.Since.Equal(want.since) {
			t.Errorf("%s: the wait began at %v, want %v", want.name, waiting.Since, want.since)
		}
		if !slices.Equal(waiting.BlockedBy, want.in.BlockedBy) {
			t.Errorf("%s: the issue waits on %v, want the %v holding it", want.name, waiting.BlockedBy, want.in.BlockedBy)
		}
	}
}

// columnRow is one row of the column rule's state table: the phase, the slot the issue holds of
// its own, what the reader knows of the daemon's waiting line, and the column the rule answers.
type columnRow struct {
	name   string
	phase  string
	slot   *int
	queued Queued
	column string
}

// columnRows is the column rule's state table over the three things a reader can know of the
// daemon's waiting line, with a row for every phase (TestEachCardRuleHasARowForEveryPhase) and
// rows for a P0 holder above the cap.
var columnRows = []columnRow{
	{"the line names an unstarted issue", PhaseAdmitted, nil, QueuedYes, ColumnQueue},
	{"the line names a slot holder", PhaseAdmitted, underTheCap, QueuedYes, ColumnDesign},
	{"the line names an issue at work", PhaseImplementing, nil, QueuedYes, ColumnImplementing},
	{"the line names nobody of this issue", PhaseAdmitted, nil, QueuedNo, ColumnDesign},
	{"no block was read of an unstarted issue", PhaseAdmitted, nil, QueuedUnread, ColumnQueue},
	{"no block was read of a slot holder", PhaseAdmitted, underTheCap, QueuedUnread, ColumnDesign},
	{"no block was read of an issue at work", PhaseImplementing, nil, QueuedUnread, ColumnImplementing},
	{"no block was read of an issue waiting on a person", PhaseAwaitingPerson, nil, QueuedUnread, ColumnAwaiting},
	{"the phase is on no column", PhaseDone, nil, QueuedUnread, ""},
	{"the line names an answered root waiting for a slot", PhaseAwaitingPerson, nil, QueuedYes, ColumnQueue},
	{"the line names an issue that planned", PhasePlanning, nil, QueuedYes, ColumnPlanning},
	{"the line names nobody of an issue in testing", PhaseTesting, nil, QueuedNo, ColumnTesting},
	{"the line names an issue under review", PhaseReviewing, nil, QueuedYes, ColumnReviewing},
	{"the line names nobody of a root holding its slot in accepting", PhaseAccepting, underTheCap, QueuedNo, ColumnAccepting},
	{"the line names an issue in retro", PhaseRetro, nil, QueuedYes, ColumnRetro},
	{"no block was read of an issue merging", PhaseMerging, nil, QueuedUnread, ColumnMerging},
	{"the line names nobody of a root awaiting its merge", PhaseAwaitingMerge, underTheCap, QueuedNo, ColumnMerging},
	// A parent waiting on its children keeps its own column whatever the reader knows of the line:
	// the daemon's blocked list names it while one of its children is open, no list names it once
	// its last child closed, and a read that recorded no block knows neither.
	{"the blocked list names a parent waiting on its children", PhaseAwaitingChildren, nil, QueuedYes, ColumnAwaitingChildren},
	{"no list names a parent whose last child closed", PhaseAwaitingChildren, nil, QueuedNo, ColumnAwaitingChildren},
	{"no block was read of a parent waiting on its children", PhaseAwaitingChildren, nil, QueuedUnread, ColumnAwaitingChildren},
	{"the line names nobody of a held issue", PhaseHeld, nil, QueuedNo, ColumnHeld},
	{"a phase a later daemon adds is on no column", laterPhase, nil, QueuedUnread, ""},
	{"the line names an issue in a phase a later daemon adds", laterPhase, nil, QueuedYes, ColumnQueue},
	// A P0 holder above the cap answers as every slot holder does.
	{"the line names nobody of a P0 holder above the cap at the design gate", PhaseAdmitted, aboveTheCap, QueuedNo, ColumnDesign},
	{"no block was read of a P0 holder above the cap", PhaseAdmitted, aboveTheCap, QueuedUnread, ColumnDesign},
	{"the line names nobody of a P0 holder above the cap at work", PhaseImplementing, aboveTheCap, QueuedNo, ColumnImplementing},
}

// TestColumnAnswersTheQueueForEachStateOfTheWaitingLine: the column rule over the three things a
// reader can know of the daemon's waiting line (columnRows). A read that recorded no admission
// block knows no line at all (QueuedUnread), and the Queue is then the issues that are still
// `admitted` and hold no slot, which is the fallback both the board and the stored facts read
// here.
func TestColumnAnswersTheQueueForEachStateOfTheWaitingLine(t *testing.T) {
	for _, want := range columnRows {
		if column := ColumnOf(want.phase, want.slot, want.queued); column != want.column {
			t.Errorf("%s: ColumnOf(%s) = %q, want %q", want.name, want.phase, column, want.column)
		}
	}
}
