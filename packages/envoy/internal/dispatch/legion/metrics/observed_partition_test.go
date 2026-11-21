package metrics

import (
	"reflect"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
)

// TestTheSevenBarsPartitionTheWorkTimeOnAnObservedTimeline: the spec's "Parked and pre-work hours
// in phase time" ends "So the bars of an issue add up to its work time, and the two cards agree",
// and the whole of metric 2 rests on it — a Report carries the seven bars and the work time
// beside each other.
//
// dispatch://INFRA-979's corpus cannot check that invariant on the observed timeline. Every one of
// its 289 finished issues carries a single trailing `done` mark, so every timeline the A2
// comparison exercises is the event-log fallback. The observed timeline is the one production
// runs on from Dispatch's first observation of an issue, and it is reached by fixtures alone.
// This holds the invariant over the observed shapes Dispatch's records actually take, and asserts
// nothing about how the hours are split between the bars, which the spec's other rules decide: a
// shape that disagreed with this test would be losing or double-counting an hour of an issue's
// life.
func TestTheSevenBarsPartitionTheWorkTimeOnAnObservedTimeline(t *testing.T) {
	cases := []struct {
		name  string
		issue *Issue
	}{
		{
			// Dispatch first observes the issue after its first worker event: two hours of work
			// time stand before the first observation, which the event log names (the spec's
			// "Statuses before observations": an issue in flight takes its phases from
			// observations only from its first observation on).
			name: "the first observation follows the first worker event",
			issue: newIssue("INFRA-1").worked("implementer", hour(0)).closedAt(hour(8)).
				observeString(legion.FieldPhase, hour(2), "implementing").
				observeString(legion.FieldPhase, hour(5), "testing"),
		},
		{
			// Parked mid-flight: those hours are on no bar and out of the work time, so the
			// identity must hold over what is left of a broken-up life.
			name: "a park between two observed phases",
			issue: newIssue("INFRA-2").worked("implementer", hour(0)).closedAt(hour(9)).
				observeString(legion.FieldPhase, hour(1), "implementing").
				statusAt(hour(3), "backlog").
				statusAt(hour(6), "in_progress").
				observeString(legion.FieldPhase, hour(7), "reviewing"),
		},
		{
			// An admitted stretch the issue holds a slot through is its design; the rest of it is
			// the waiting line, whose hours are on no bar.
			name: "an admitted stretch, part of it holding a slot",
			issue: newIssue("INFRA-3").worked("architect", hour(0)).closedAt(hour(8)).
				observeString(legion.FieldPhase, hour(0), "admitted").
				observe(legion.FieldSlot, hour(2), "1").
				observeString(legion.FieldPhase, hour(4), "planning").
				observeString(legion.FieldPhase, hour(6), "implementing"),
		},
		{
			// Back in the queue: the issue leaves `admitted` and returns to it, which the spec's
			// "Back in the queue" rule takes out of the work time with observations present.
			name: "back in the queue after it first left",
			issue: newIssue("INFRA-4").worked("architect", hour(0)).closedAt(hour(10)).
				observeString(legion.FieldPhase, hour(0), "admitted").
				observe(legion.FieldSlot, hour(0), "0").
				observeString(legion.FieldPhase, hour(2), "implementing").
				observe(legion.FieldSlot, hour(4), "null").
				observeString(legion.FieldPhase, hour(4), "admitted").
				observeString(legion.FieldPhase, hour(7), "implementing"),
		},
		{
			// A phase no role works, which is the one thing the "no role" bar holds.
			name: "a stretch waiting on a person",
			issue: newIssue("INFRA-5").worked("implementer", hour(0)).closedAt(hour(8)).
				observeString(legion.FieldPhase, hour(1), "implementing").
				observeString(legion.FieldPhase, hour(3), "awaiting_person").
				observeString(legion.FieldPhase, hour(6), "implementing"),
		},
		{
			// Review round 32's probe: `done` from the close at hour 3 through the reopen at
			// hour 5 to hour 6, out of the workflow, and the queue after it.
			name:  "merged, closed, then reopened and queued",
			issue: mergedThenReopened("INFRA-6"),
		},
		{
			// The daemon parks a child that leaves the workflow in `done`, and reports it
			// again once it is back.
			name:  "a child moved to triage and reported again",
			issue: triagedAndObserved("INFRA-7").under("INFRA-1"),
		},
	}

	for _, each := range cases {
		t.Run(each.name, func(t *testing.T) { partitionsItsWorkTime(t, each.issue) })
	}
}

// TestTheSevenBarsPartitionTheWorkTimeOnTheFallbackTimeline is the same identity on the other
// timeline. dispatch://INFRA-979's corpus does hold it over all 289 finished issues, but the
// corpus is one shape of history: every issue there runs forward through its phases once. These
// are the shapes that bend — a handoff with no role event after it, a send-back, two parks, an
// issue only one role ever touched, a close a reopen undid, a stretch in `triage` — which the
// corpus happens not to carry in a form that would break the sum.
func TestTheSevenBarsPartitionTheWorkTimeOnTheFallbackTimeline(t *testing.T) {
	cases := []struct {
		name  string
		issue *Issue
	}{
		{
			name: "every role in turn, with its handoffs",
			issue: newIssue("INFRA-1").
				worked("architect", hour(0)).worked("planner", hour(1)).
				worked("implementer", hour(3)).worked("tester", hour(6)).
				worked("reviewer", hour(8)).worked("merger", hour(10)).
				handedOff("plan", hour(2)).handedOff("implement", hour(5)).
				handedOff("test", hour(7)).handedOff("review", hour(9)).
				closedAt(hour(12)),
		},
		{
			// A handoff with no role event after it: the hour between the handoff and the next
			// role's first event is the one the spec's boundary rule had to settle.
			name: "a handoff the next role does not answer for an hour",
			issue: newIssue("INFRA-2").
				worked("implementer", hour(0)).worked("reviewer", hour(5)).
				handedOff("implement", hour(2)).handedOff("test", hour(4)).
				closedAt(hour(7)),
		},
		{
			name: "sent back twice by its statuses",
			issue: newIssue("INFRA-3").worked("implementer", hour(0)).closedAt(hour(12)).
				statusAt(hour(0), "in_progress").statusAt(hour(2), "testing").
				statusAt(hour(3), "needs_review").statusAt(hour(4), "in_progress").
				statusAt(hour(6), "testing").statusAt(hour(7), "needs_review").
				statusAt(hour(8), "in_progress").statusAt(hour(10), "testing").
				statusAt(hour(12), "done"),
		},
		{
			name: "parked twice",
			issue: newIssue("INFRA-4").worked("implementer", hour(0)).closedAt(hour(12)).
				statusAt(hour(0), "in_progress").statusAt(hour(2), "backlog").
				statusAt(hour(4), "in_progress").statusAt(hour(7), "icebox").
				statusAt(hour(9), "in_progress").statusAt(hour(12), "done"),
		},
		{
			name:  "one role and nothing else",
			issue: newIssue("INFRA-5").worked("implementer", hour(0)).closedAt(hour(4)),
		},
		{
			name:  "closed, then reopened",
			issue: reopened("INFRA-6", "todo"),
		},
		{
			name:  "moved to triage and back",
			issue: triaged("INFRA-7"),
		},
	}

	for _, each := range cases {
		t.Run(each.name, func(t *testing.T) { partitionsItsWorkTime(t, each.issue) })
	}
}

// TestAnAdmittedRootHoldingASlotIsDesignAndTheRestIsQueue is the other half of the spec's "Queue
// time, one definition": "every observed admitted-with-no-slot stretch, whenever it falls, is
// subtracted from both work time and the bars. A slot-holding admitted root is in design." The
// identity above would be satisfied by dropping the whole admitted stretch from both sides, so
// this pins which part of it is design and which is queue.
func TestAnAdmittedRootHoldingASlotIsDesignAndTheRestIsQueue(t *testing.T) {
	// Admitted from the start; the slot arrives at hour 2 and the architect speaks at hour 1,
	// so the work time opens at hour 1 and the queue hours 1-2 come out of it.
	root := newIssue("INFRA-1").worked("architect", hour(1)).closedAt(hour(8)).
		observeString(legion.FieldPhase, hour(0), "admitted").
		observe(legion.FieldSlot, hour(2), "0").
		observeString(legion.FieldPhase, hour(4), "planning")

	report := Compute(fixture(hour(-24), hour(24), root), ScopeAll, hour(24))

	row := report.WorkTime.Issues[0]
	if row.Hours != 6 {
		t.Errorf("work time = %g h, want 6: hours 1 to 8 less the queued hour 1 to 2", row.Hours)
	}
	if row.QueuedHours != 1 {
		t.Errorf("queued hours = %g, want 1: the admitted hour the root held no slot through", row.QueuedHours)
	}
	if got := barOf(t, report, "architect").Hours; got != 2 {
		t.Errorf("the architect bar = %g h, want 2: the admitted stretch the root holds a slot through is its design, hours 2 to 4",
			got)
	}
	if got := barOf(t, report, "planner").Hours; got != 4 {
		t.Errorf("the planner bar = %g h, want 4: hours 4 to 8", got)
	}
	partitionsItsWorkTime(t, root)
}

// TestANullPhaseMarkEndsNoPhase: a read that no longer carries a field of an issue writes a `null`
// row for it, so an observed timeline can hold a `null` phase mark between two phases, or after
// the last one. That mark names no phase and ends none: the phase before it runs on to the next
// mark that carries a phase, or to the close where none follows, so the bars of the issue still
// add up to its work time (the spec's "Parked and pre-work hours in phase time": "So the bars of
// an issue add up to its work time, and the two cards agree"). The first case is implementing
// from hour 0, `null` at hour 2 and implementing again at hour 4, closed at hour 6: six hours of
// work time, of which a `null` that ended the phase before it left two on no bar.
func TestANullPhaseMarkEndsNoPhase(t *testing.T) {
	cases := []struct {
		name     string
		issue    *Issue
		timeline []string
	}{
		{
			name: "a null between two phases",
			issue: newIssue("INFRA-1").worked("implementer", hour(0)).closedAt(hour(6)).
				observeString(legion.FieldPhase, hour(0), "implementing").
				observe(legion.FieldPhase, hour(2), "null").
				observeString(legion.FieldPhase, hour(4), "implementing"),
			timeline: []string{"implementing 0-4", "implementing 4-6"},
		},
		{
			// The daemon stopped reporting the issue: the hours after the `null` stay on the bar
			// of the phase it was last seen in.
			name: "a null after the last phase",
			issue: newIssue("INFRA-1").worked("implementer", hour(0)).closedAt(hour(6)).
				observeString(legion.FieldPhase, hour(0), "implementing").
				observe(legion.FieldPhase, hour(2), "null"),
			timeline: []string{"implementing 0-6"},
		},
	}

	for _, each := range cases {
		t.Run(each.name, func(t *testing.T) {
			partitionsItsWorkTime(t, each.issue)
			population := fixture(hour(-24), hour(24), each.issue)
			if work := Compute(population, ScopeAll, hour(24)).WorkTime.Issues[0].Hours; work != 6 {
				t.Errorf("work time = %g h, want 6: hours 0 to 6", work)
			}
			if got := timelineOf(population, each.issue); !reflect.DeepEqual(got, each.timeline) {
				t.Errorf("the timeline is %v, want %v: a null mark ends no phase", got, each.timeline)
			}
		})
	}
}

// TestANullPhaseMarkEndsNoWaitOnAPerson: segment 1 holds while "the daemon recorded the root in
// `awaiting_person`" (the spec's metric 2), and it reads the root's phase marks as the timeline
// does. A `null` phase mark is the daemon no longer reporting the root, not a change of its phase,
// so the root waits on a person through it. The root below is in `awaiting_person` from hour 0,
// has a `null` phase mark at hour 2 and closes at hour 4: its four hours are on the no-role bar,
// and all four are waiting on a person.
func TestANullPhaseMarkEndsNoWaitOnAPerson(t *testing.T) {
	root := newIssue("INFRA-1").worked("implementer", hour(0)).closedAt(hour(4)).
		observeString(legion.FieldPhase, hour(0), "awaiting_person").
		observe(legion.FieldPhase, hour(2), "null")
	partitionsItsWorkTime(t, root)
	bar := barOf(t, Compute(fixture(hour(-24), hour(24), root), ScopeAll, hour(24)), barNoRole)
	if bar.Hours != 4 || bar.Segments.WaitingOnPerson != 4 {
		t.Errorf("the no-role bar = %g hours, %+v, want all four waiting on a person: a null phase mark ends no wait",
			bar.Hours, bar.Segments)
	}
}

// TestANullSlotMarkIsAReleaseOnlyInAReadThatStillCarriesTheIssue: Dispatch writes a `null` slot
// mark for two facts. A read that carries the issue observes its slot, `null` where it holds none
// (legion.State's Observations), so a slot released is a `null` slot mark beside a phase that still
// names one, and the rest of the admitted stretch is queue time: the spec's "Queue time, one
// definition", "every observed stretch in which the issue's phase is `admitted` and it holds no
// slot". A read that no longer carries the issue writes `null` for every field of it at one moment
// (legion's recordObservations). That says the daemon stopped reporting the issue, not that it gave
// up its slot, and nothing after it was observed, so the slot runs on through it as the phase does
// (TestANullPhaseMarkEndsNoPhase). It runs to the next read that carries the issue again, which
// writes a slot mark only where the issue holds a slot. Each root below is admitted holding slot 0
// from hour 0, its architect speaks at hour 0, and it closes at hour 6.
func TestANullSlotMarkIsAReleaseOnlyInAReadThatStillCarriesTheIssue(t *testing.T) {
	admitted := func() *Issue {
		return newIssue("INFRA-1").worked("architect", hour(0)).closedAt(hour(6)).
			observeString(legion.FieldPhase, hour(0), "admitted").
			observe(legion.FieldSlot, hour(0), "0")
	}
	cases := []struct {
		name      string
		issue     *Issue
		timeline  []string
		work      float64
		queued    float64
		architect float64
	}{
		{
			// Every field `null` at hour 2: a drop. Read as a release, it would end the design at
			// hour 2 and put hours 2 to 6 in the queue, off the work time and off the architect's bar.
			name: "a drop while the root holds its slot",
			issue: admitted().
				observe(legion.FieldPhase, hour(2), "null").
				observe(legion.FieldSlot, hour(2), "null"),
			timeline: []string{"design 0-6"},
			work:     6, queued: 0, architect: 6,
		},
		{
			// The slot alone goes `null` at hour 2, in a read that still carries the root: a
			// release, and the root waits in the queue from there.
			name: "a release in a read that still carries the root",
			issue: admitted().
				observe(legion.FieldSlot, hour(2), "null"),
			timeline: []string{"design 0-2", "queued 2-6"},
			work:     2, queued: 4, architect: 2,
		},
		{
			// The read at hour 4 carries the root again, holding no slot. It writes no slot mark,
			// since the newest one is already the drop's `null`, and the slot ends there.
			name: "a drop, then a read that carries the root holding no slot",
			issue: admitted().
				observe(legion.FieldPhase, hour(2), "null").
				observe(legion.FieldSlot, hour(2), "null").
				observeString(legion.FieldPhase, hour(4), "admitted"),
			timeline: []string{"design 0-4", "queued 4-6"},
			work:     4, queued: 2, architect: 4,
		},
		{
			name: "a drop, then a read that carries the root holding its slot",
			issue: admitted().
				observe(legion.FieldPhase, hour(2), "null").
				observe(legion.FieldSlot, hour(2), "null").
				observeString(legion.FieldPhase, hour(4), "admitted").
				observe(legion.FieldSlot, hour(4), "0"),
			timeline: []string{"design 0-4", "design 4-6"},
			work:     6, queued: 0, architect: 6,
		},
		{
			// The read at hour 3 carries the root again, in planning and holding its slot, so it
			// is no longer dropped. The read at hour 4 observes it admitted and writes no slot
			// mark, since its newest one is hour 3's: it still holds the slot, and is in design.
			// Read as a return from the drop, hour 4 would end the slot and put hours 4 to 6 in
			// the queue.
			name: "a drop, a return holding the slot, then a phase with no slot mark",
			issue: admitted().
				observe(legion.FieldPhase, hour(2), "null").
				observe(legion.FieldSlot, hour(2), "null").
				observeString(legion.FieldPhase, hour(3), "planning").
				observe(legion.FieldSlot, hour(3), "0").
				observeString(legion.FieldPhase, hour(4), "admitted"),
			timeline: []string{"design 0-3", "planning 3-4", "design 4-6"},
			work:     6, queued: 0, architect: 5,
		},
	}

	for _, each := range cases {
		t.Run(each.name, func(t *testing.T) {
			partitionsItsWorkTime(t, each.issue)
			population := fixture(hour(-24), hour(24), each.issue)
			if got := timelineOf(population, each.issue); !reflect.DeepEqual(got, each.timeline) {
				t.Errorf("the timeline is %v, want %v", got, each.timeline)
			}
			report := Compute(population, ScopeAll, hour(24))
			if row := report.WorkTime.Issues[0]; row.Hours != each.work || row.QueuedHours != each.queued {
				t.Errorf("work time = %g h with %g h queued, want %g h with %g h queued",
					row.Hours, row.QueuedHours, each.work, each.queued)
			}
			if got := barOf(t, report, "architect").Hours; got != each.architect {
				t.Errorf("the architect bar = %g h, want %g", got, each.architect)
			}
		})
	}
}

// partitionsItsWorkTime is the identity itself: an issue's seven bars add up to its own
// work-time row, and its five segments add up to its seven bars.
func partitionsItsWorkTime(t *testing.T, issue *Issue) {
	t.Helper()
	report := Compute(fixture(hour(-24), hour(24), issue), ScopeAll, hour(24))
	if len(report.WorkTime.Issues) != 1 {
		t.Fatalf("work-time rows = %d, want the one issue", len(report.WorkTime.Issues))
	}
	work := report.WorkTime.Issues[0].Hours

	var bars, segments float64
	for _, bar := range report.PhaseTime.Bars {
		bars += bar.Hours
		segments += bar.Segments.WaitingOnPerson + bar.Segments.InATurn +
			bar.Segments.CIWait + bar.Segments.WaitingOnChildren + bar.Segments.Idle
	}
	if bars != work {
		t.Errorf("the seven bars hold %g h against a work time of %g h: %g h of this issue's life is on no bar, or on two",
			bars, work, bars-work)
	}
	if segments != bars {
		t.Errorf("the five segments hold %g h against the seven bars' %g h: each hour of a bar goes to exactly one segment",
			segments, bars)
	}
}
