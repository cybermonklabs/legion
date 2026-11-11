package legion

import (
	"testing"
	"time"
)

// TestSlotUseAnswersOneUsePerSlotState: the five uses a slot an issue holds of its own is put to,
// one case each, and the empty answer for an issue that holds no slot. The two readers of the
// rule — the board's cards and the per-issue facts — pass plain values, so a case here is the
// whole input.
func TestSlotUseAnswersOneUsePerSlotState(t *testing.T) {
	slot := 3
	for _, want := range []struct {
		name       string
		phase      string
		roles      []string
		slot       *int
		childWorks bool
		use        string
	}{
		{"a worker is in a turn", PhaseImplementing, []string{"implementer"}, &slot, false, SlotUseWorking},
		{"an architect is in a turn", PhaseAccepting, []string{"architect"}, &slot, true, SlotUseWorking},
		{"nobody is in a turn", PhaseImplementing, nil, &slot, false, SlotUseBetweenTurns},
		{"a person has not answered", PhaseAwaitingPerson, nil, &slot, false, SlotUseHeldPerson},
		{"a child of the tree works", PhaseAccepting, nil, &slot, true, SlotUseHeldChildren},
		{"no child of the tree works", PhaseAccepting, nil, &slot, false, SlotUseHeldIdle},
		{"the issue holds no slot of its own", PhaseImplementing, []string{"implementer"}, nil, false, ""},
	} {
		if use := SlotUse(want.phase, want.roles, want.slot, want.childWorks); use != want.use {
			t.Errorf("%s: SlotUse(%s) = %q, want %q", want.name, want.phase, use, want.use)
		}
	}
}

// TestCardWaitingAnswersOneReasonPerWait: the four reasons an issue waits for, one case each, and
// the nil answer for an issue that waits for nothing. Since is the phase entry, and the moment a
// person was asked where the issue waits for that person.
func TestCardWaitingAnswersOneReasonPerWait(t *testing.T) {
	entered := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	asked := time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)
	answered := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	first, second := 1, 2
	for _, want := range []struct {
		name   string
		in     WaitingInput
		reason string
		since  time.Time
	}{
		{
			name:   "ready for the daemon to start it",
			in:     WaitingInput{Phase: PhaseAdmitted, Column: ColumnQueue, WaitingPlace: &first, PhaseSince: &entered},
			reason: WaitingReady, since: entered,
		},
		{
			name: "its wait on a person was answered, so it waits for a slot at the front",
			in: WaitingInput{
				Phase: PhaseAwaitingPerson, Column: ColumnQueue, WaitingPlace: &first, PhaseSince: &entered,
				WaitingOn: &Wait{Kind: "ask", Since: &asked, AnsweredAt: &answered},
			},
			reason: WaitingReady, since: entered,
		},
		{
			name:   "it started its work and waits for a slot to come back",
			in:     WaitingInput{Phase: PhaseImplementing, Column: ColumnImplementing, WaitingPlace: &second, PhaseSince: &entered},
			reason: WaitingSlot, since: entered,
		},
		{
			name: "a blocker link holds it",
			in: WaitingInput{
				Phase: PhaseAdmitted, Column: ColumnQueue, WaitingPlace: &second,
				BlockedBy: []string{"ACME-9"}, PhaseSince: &entered,
			},
			reason: WaitingBlocked, since: entered,
		},
		{
			name: "a person has not answered it",
			in: WaitingInput{
				Phase: PhaseAwaitingPerson, Column: ColumnAwaiting, PhaseSince: &entered,
				WaitingOn: &Wait{Kind: "gate", Since: &asked},
			},
			reason: WaitingPerson, since: asked,
		},
	} {
		waiting := CardWaiting(want.in)
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
	}
	// An issue at work in its own slot, which neither the line nor the Queue column nor the
	// awaiting-a-person column holds, waits for nothing.
	atWork := WaitingInput{Phase: PhaseImplementing, Column: ColumnImplementing, PhaseSince: &entered}
	if waiting := CardWaiting(atWork); waiting != nil {
		t.Errorf("an issue at work in its slot waits for %+v, want nothing", *waiting)
	}
	// The issues a blocker link holds are on the wait, whatever the reason the rule answers.
	held := CardWaiting(WaitingInput{
		Phase: PhaseAdmitted, Column: ColumnQueue, BlockedBy: []string{"ACME-9", "ACME-10"}, PhaseSince: &entered,
	})
	if held == nil || len(held.BlockedBy) != 2 {
		t.Errorf("the blocked issue waits on %+v, want the two issues holding it", held)
	}
}
