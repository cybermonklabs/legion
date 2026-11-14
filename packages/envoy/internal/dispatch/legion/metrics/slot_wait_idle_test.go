package metrics

import (
	"maps"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
)

// TestSlotWaitCountsTheObservedAdmittedHoursWithNoSlotOverTheWholeLife: slot wait is "the hours of
// the issue's observed life in which its phase is `admitted` and it holds no slot, over the whole
// life, with parked hours left out" (dispatch://INFRA-2916, D3). Each issue below stands admitted
// for a while: one with no slot through the first part of it and parked for an hour of that part,
// before its first worker event; one holding a slot the whole time; and one the sampler never saw.
func TestSlotWaitCountsTheObservedAdmittedHoursWithNoSlotOverTheWholeLife(t *testing.T) {
	// Created at hour -4, admitted from hour -3 with no slot until it takes one at hour 1, parked
	// from hour -2 to hour -1, its architect's first event at hour 0, planning from hour 5.
	waited := newIssue("INFRA-10").openedAt(hour(-4)).worked("architect", hour(0)).closedAt(hour(10)).
		statusAt(hour(-3), "todo").
		statusAt(hour(-2), "backlog").
		statusAt(hour(-1), "todo").
		statusAt(hour(0), "in_progress").
		observeString(legion.FieldPhase, hour(-3), "admitted").
		observe(legion.FieldSlot, hour(1), "0").
		observeString(legion.FieldPhase, hour(5), "planning").
		observe(legion.FieldSlot, hour(10), "null").
		observeString(legion.FieldPhase, hour(10), "done")
	// Admitted from hour 0 holding a slot from that same moment: design, never a wait.
	held := newIssue("INFRA-11").openedAt(hour(-1)).worked("architect", hour(0)).closedAt(hour(10)).
		observeString(legion.FieldPhase, hour(0), "admitted").
		observe(legion.FieldSlot, hour(0), "1").
		observeString(legion.FieldPhase, hour(5), "planning").
		observeString(legion.FieldPhase, hour(10), "done")
	// No observation at all: its timeline is the event log's, which holds no observed phase.
	unseen := newIssue("INFRA-12").openedAt(hour(-1)).worked("implementer", hour(0)).closedAt(hour(6)).
		statusAt(hour(-1), "todo").
		statusAt(hour(0), "in_progress")

	facts := ProjectFacts(fixture(hour(-24), hour(24), waited, held, unseen))
	for _, want := range []struct {
		key      string
		slotWait float64
		queued   float64
		why      string
	}{
		// Four hours admitted with no slot (-3 to 1), one of them parked: three. The work time's
		// queue is the one hour of those four after the first worker event.
		{"INFRA-10", 3, 1, "admitted with no slot from -3 to 1, parked from -2 to -1"},
		{"INFRA-11", 0, 0, "admitted holding its own slot throughout"},
		{"INFRA-12", 0, 0, "never observed, so no observed phase"},
	} {
		got, held := facts[want.key]
		if !held {
			t.Errorf("%s (%s): no facts computed, want slot wait %g h and queued %g h", want.key, want.why, want.slotWait, want.queued)
			continue
		}
		if got.SlotWaitHours != want.slotWait || got.QueuedHours != want.queued {
			t.Errorf("%s (%s): slot wait %g h and queued %g h, want %g and %g",
				want.key, want.why, got.SlotWaitHours, got.QueuedHours, want.slotWait, want.queued)
		}
		t.Logf("%s (%s): slot wait %g h, queued %g h", want.key, want.why, got.SlotWaitHours, got.QueuedHours)
	}
}

// TestIdleByRoleCountsTheHoursAnIssueHeldItsOwnSlotInARolesBarWithThatRoleOutOfATurn: for role R,
// idle time is "the hours of the issue's worked spans (life − parked − queued) in which the issue
// holds its own slot, its observed phase is in R's bar ..., and R's newest observed claim state
// is not `working` (no claim counts as not working)" (dispatch://INFRA-2916, D3).
//
// The root holds its own slot through its whole life and walks through four bars. Each role's
// claim history is one of the cases the rule names: the architect works an hour of its two of
// design; the implementer works one hour of four and then stands idle and then suspended; the
// reviewer has no claim at all, and an hour of its phase is parked; the tester stands idle all
// along while its bar never comes; the merger works the whole of its phase. The child holds no
// slot of its own although its root holds one, so it has no idle hour whatever its claims say.
func TestIdleByRoleCountsTheHoursAnIssueHeldItsOwnSlotInARolesBarWithThatRoleOutOfATurn(t *testing.T) {
	root := newIssue("INFRA-1").openedAt(hour(-1)).worked("architect", hour(0)).closedAt(hour(12)).
		statusAt(hour(0), "in_progress").
		statusAt(hour(8), "backlog").
		statusAt(hour(9), "needs_review").
		observe(legion.FieldSlot, hour(0), "2").
		observe(legion.FieldSlot, hour(12), "null").
		observeString(legion.FieldPhase, hour(0), "admitted").
		observeString(legion.FieldPhase, hour(2), "implementing").
		observeString(legion.FieldPhase, hour(6), "reviewing").
		observeString(legion.FieldPhase, hour(10), "merging").
		observeString(legion.FieldPhase, hour(12), "done").
		observeString(legion.ClaimFieldPrefix+"architect", hour(0), "working").
		observeString(legion.ClaimFieldPrefix+"architect", hour(1), "idle").
		observeString(legion.ClaimFieldPrefix+"implementer", hour(2), "working").
		observeString(legion.ClaimFieldPrefix+"implementer", hour(3), "idle").
		observeString(legion.ClaimFieldPrefix+"implementer", hour(4), "suspended").
		observeString(legion.ClaimFieldPrefix+"tester", hour(0), "idle").
		observeString(legion.ClaimFieldPrefix+"merger", hour(10), "working")
	child := newIssue("INFRA-2").under("INFRA-1").openedAt(hour(0)).worked("implementer", hour(1)).closedAt(hour(5)).
		observeString(legion.FieldPhase, hour(1), "implementing").
		observeString(legion.FieldPhase, hour(5), "done").
		observeString(legion.ClaimFieldPrefix+"implementer", hour(1), "idle")

	facts := ProjectFacts(fixture(hour(-24), hour(24), root, child))
	for _, want := range []struct {
		key  string
		idle map[string]float64
		why  string
	}{
		{"INFRA-1", map[string]float64{
			// Design is the daemon's `admitted` while the root holds its own slot: hours 0 to 2,
			// the second of them with the architect out of its turn.
			"architect": 1,
			// Nobody plans this root.
			"planner": 0,
			// Implementing from 2 to 6, working for the first hour, `idle` then `suspended`.
			"implementer": 3,
			// `idle` from hour 0, but never in testing: the phase is outside the tester's bar.
			"tester": 0,
			// No claim at all from 6 to 10, less the parked hour from 8 to 9.
			"reviewer": 3,
			// Merging from 10 to 12, working throughout.
			"merger": 0,
		}, "holds its own slot"},
		{"INFRA-2", map[string]float64{
			"architect": 0, "planner": 0, "implementer": 0, "tester": 0, "reviewer": 0, "merger": 0,
		}, "holds no slot of its own, though its root holds one"},
	} {
		got := facts[want.key].IdleByRole
		if !maps.Equal(got, want.idle) {
			t.Errorf("%s (%s): idle hours by role %v, want %v", want.key, want.why, got, want.idle)
		}
		t.Logf("%s (%s): idle hours by role %v", want.key, want.why, got)
	}
	// The idle hours by role are another rule than phase time's idle segment. The reviewer's bar
	// of the root is all idle segment, as nobody was in a turn and no child worked. The
	// implementer's bar holds an hour in a turn and two hours the root waited while its child
	// worked, so its idle segment is one hour where the implementer's idle time is three.
	bars := facts["INFRA-1"].Bars
	if got := bars["reviewer"]; got.Hours != 3 || got.Segments.Idle != 3 {
		t.Errorf("the root's reviewer bar is %+v, want 3 hours, all of them the idle segment", got)
	}
	if got := bars["implementer"]; got.Hours != 4 || got.Segments.InATurn != 1 || got.Segments.WaitingOnChildren != 2 || got.Segments.Idle != 1 {
		t.Errorf("the root's implementer bar is %+v, want 4 hours: 1 in a turn, 2 waiting on the child and 1 idle", got)
	}
}
