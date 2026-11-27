package metrics

import (
	"reflect"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
)

// mergedThenReopened is review round 32's probe, with observations: an issue implementing from
// hour 0 and merging at hour 2, closed at hour 3, reopened to `todo` at hour 5, admitted with no
// slot at hour 6, implementing at hour 7 and closed at hour 9. The daemon set `done` at hour 3 and
// kept it through the reopen: a reopened child stays `todo` with its phase `done` until its tree
// takes it in again (the daemon's admit.go, reenterStrandedChildren), which here is hour 6.
func mergedThenReopened(key string) *Issue {
	return newIssue(key).worked("implementer", hour(0)).closedAt(hour(9)).
		statusAt(hour(0), "in_progress").
		observeString(legion.FieldPhase, hour(0), "implementing").
		observeString(legion.FieldPhase, hour(2), "merging").
		statusAt(hour(3), "done").
		observeString(legion.FieldPhase, hour(3), "done").
		statusAt(hour(5), "todo").
		observeString(legion.FieldPhase, hour(6), "admitted").
		observeString(legion.FieldPhase, hour(7), "implementing").
		statusAt(hour(9), "done").
		observeString(legion.FieldPhase, hour(9), "done")
}

// reopened is an issue as its event log records it: implementing from hour 0, testing at hour 2,
// closed at hour 3, reopened to the status given at hour 5, and closed for good at hour 7.
func reopened(key, reopenedTo string) *Issue {
	return newIssue(key).worked("implementer", hour(0)).closedAt(hour(7)).
		statusAt(hour(0), "in_progress").
		statusAt(hour(2), "testing").
		statusAt(hour(3), "done").
		statusAt(hour(5), reopenedTo).
		statusAt(hour(7), "done")
}

// triaged is an issue as its event log records it: implementing from hour 0, moved to `triage` at
// hour 2, back to `todo` at hour 4, and closed at hour 6.
func triaged(key string) *Issue {
	return newIssue(key).worked("implementer", hour(0)).closedAt(hour(6)).
		statusAt(hour(0), "in_progress").
		statusAt(hour(2), "triage").
		statusAt(hour(4), "todo").
		statusAt(hour(6), "done")
}

// triagedAndObserved is the triaged issue with what the sampler saw of it: the daemon parks a
// child that leaves the workflow, for `triage` among the rest, in `done` (the daemon's
// workflow/linger.go, leave), and reports it again in its phase once a `todo` takes it back.
func triagedAndObserved(key string) *Issue {
	return triaged(key).
		observeString(legion.FieldPhase, hour(0), "implementing").
		observeString(legion.FieldPhase, hour(2), "done").
		observeString(legion.FieldPhase, hour(4), "implementing").
		observeString(legion.FieldPhase, hour(6), "done")
}

// TestAnHourOutOfTheWorkflowIsOutsideTheWorkTimeAndOnNoBar is the spec's "Out of the workflow
// before the close": "An hour in which the issue is out of the workflow, before its close, is
// outside the work time and on no bar, as a park is. With observations, that is every stretch
// whose observed phase is `done` before the close ... Before observations, it is the stretch from
// an `issue.closed` that a reopen follows, or from a `triage` status write, to the next status
// write." Each case names the stretch `done` on its timeline, takes it out of the work time, and
// keeps it off every bar, so the seven bars still add up to the work time.
//
// Review round 32's probe gave a work time of 8 h against 5 h of bars: its `done` stretch was on
// no bar and still inside the work time. Its observed phase is `done` from hour 3, where it first
// closed, to hour 6, where the daemon takes it in again, so all three hours are out of the
// workflow: the reopen's `todo` at hour 5 marks no phase, and with observations the phase is the
// observed one (the spec's "Statuses before observations"). Hour 6 to 7 is the queue. That leaves
// five hours: two implementing and one merging before hour 3, and two implementing after hour 7.
//
// Before observations the stretch ends at the next status write, and "the reopen's status sets
// the phase from then" (the spec's "The close"). A `needs_review` sets reviewing. A `todo`, or an
// `in_progress` after `done`, marks no phase, so the issue goes on in the phase it had before the
// stretch, as it does after a park.
func TestAnHourOutOfTheWorkflowIsOutsideTheWorkTimeAndOnNoBar(t *testing.T) {
	cases := []struct {
		name     string
		issue    *Issue
		timeline []string
		work     float64
		parked   float64
		queued   float64
		out      float64
		bars     map[string]float64
	}{
		{
			name:     "with observations: merged, closed, then reopened and queued",
			issue:    mergedThenReopened("INFRA-1"),
			timeline: []string{"implementing 0-2", "merging 2-3", "done 3-6", "queued 6-7", "implementing 7-9"},
			work:     5, queued: 1, out: 3,
			bars: map[string]float64{"implementer": 4, "merger": 1},
		},
		{
			name:     "with observations: a child moved to triage and reported again",
			issue:    triagedAndObserved("INFRA-2").under("INFRA-1"),
			timeline: []string{"implementing 0-2", "done 2-4", "implementing 4-6"},
			work:     4, out: 2,
			bars: map[string]float64{"implementer": 4},
		},
		{
			name:     "before observations: closed, then reopened to todo",
			issue:    reopened("INFRA-1", "todo"),
			timeline: []string{"implementing 0-2", "testing 2-3", "done 3-5", "testing 5-7"},
			work:     5, out: 2,
			bars: map[string]float64{"implementer": 2, "tester": 3},
		},
		{
			// An `in_progress` after `done` is no send-back, which only a move out of
			// `testing`, `needs_review` or `retro` is, so it marks no phase either.
			name:     "before observations: closed, then reopened to in_progress",
			issue:    reopened("INFRA-1", "in_progress"),
			timeline: []string{"implementing 0-2", "testing 2-3", "done 3-5", "testing 5-7"},
			work:     5, out: 2,
			bars: map[string]float64{"implementer": 2, "tester": 3},
		},
		{
			name:     "before observations: closed, then reopened into review",
			issue:    reopened("INFRA-1", "needs_review"),
			timeline: []string{"implementing 0-2", "testing 2-3", "done 3-5", "reviewing 5-7"},
			work:     5, out: 2,
			bars: map[string]float64{"implementer": 2, "tester": 1, "reviewer": 2},
		},
		{
			name:     "before observations: moved to triage and back",
			issue:    triaged("INFRA-1"),
			timeline: []string{"implementing 0-2", "done 2-4", "implementing 4-6"},
			work:     4, out: 2,
			bars: map[string]float64{"implementer": 4},
		},
		{
			// An hour out of the workflow is no parked hour: `parked_hours` holds the time in
			// `backlog` or `icebox` alone, and the work time leaves out each of the two beside
			// the other. Out from hour 2 to hour 4, parked from hour 4 to hour 5.
			name: "before observations: moved to triage, then parked",
			issue: newIssue("INFRA-1").worked("implementer", hour(0)).closedAt(hour(7)).
				statusAt(hour(0), "in_progress").
				statusAt(hour(2), "triage").
				statusAt(hour(4), "backlog").
				statusAt(hour(5), "in_progress").
				statusAt(hour(7), "done"),
			timeline: []string{"implementing 0-2", "done 2-4", "implementing 4-7"},
			work:     4, parked: 1, out: 2,
			bars: map[string]float64{"implementer": 4},
		},
	}

	for _, each := range cases {
		t.Run(each.name, func(t *testing.T) {
			population := fixture(hour(-24), hour(24), each.issue)
			if got := timelineOf(population, each.issue); !reflect.DeepEqual(got, each.timeline) {
				t.Errorf("the timeline is %v, want %v", got, each.timeline)
			}
			w := windowOf(population, ScopeAll)
			row := w.fact(t, each.issue.Key)
			// The hours out of the workflow are no stored fact (no Legion eval reads them), so they
			// are read off the timeline the work time takes them out of (outOfWorkflowSpans).
			spans, _ := population.Phases(each.issue)
			out := totalOf(outOfWorkflowSpans(spans)).Hours()
			if row.WorkHours != each.work || row.ParkedHours != each.parked || row.QueuedHours != each.queued || out != each.out {
				t.Errorf("work time = %g h with %g h parked, %g h queued and %g h out of the workflow, want %g h with %g h, %g h and %g h",
					row.WorkHours, row.ParkedHours, row.QueuedHours, out,
					each.work, each.parked, each.queued, each.out)
			}
			hoursPerBar, _ := w.hoursPerBar(t)
			for _, bar := range bars {
				if got := hoursPerBar[bar.Role]; got != each.bars[bar.Role] {
					t.Errorf("the %s bar = %g h, want %g", bar.Role, got, each.bars[bar.Role])
				}
			}
			partitionsItsWorkTime(t, each.issue)
		})
	}
}

// TestBothTimelinesAnswerAnIssueOutOfTheWorkflowTheSameWay: an issue the sampler observed from its
// first hour takes its phases from the observations, and the same issue with no observation takes
// them from its event log. Where the daemon reports `done` exactly where the event log's statuses
// say the issue was out of the workflow, and the phase each names after it agrees, the two
// timelines are the same and so is every fact of the issue (factsOf): the event log's
// stretch is laid on its timeline as the `done` the daemon reports for it.
func TestBothTimelinesAnswerAnIssueOutOfTheWorkflowTheSameWay(t *testing.T) {
	cases := []struct {
		name               string
		eventLog, observed *Issue
	}{
		{
			// The `todo` names no phase, and the daemon takes the issue in again at testing.
			name:     "closed, then reopened to todo",
			eventLog: reopened("INFRA-1", "todo"),
			observed: reopened("INFRA-1", "todo").
				observeString(legion.FieldPhase, hour(0), "implementing").
				observeString(legion.FieldPhase, hour(2), "testing").
				observeString(legion.FieldPhase, hour(3), "done").
				observeString(legion.FieldPhase, hour(5), "testing").
				observeString(legion.FieldPhase, hour(7), "done"),
		},
		{
			name:     "closed, then reopened into review",
			eventLog: reopened("INFRA-1", "needs_review"),
			observed: reopened("INFRA-1", "needs_review").
				observeString(legion.FieldPhase, hour(0), "implementing").
				observeString(legion.FieldPhase, hour(2), "testing").
				observeString(legion.FieldPhase, hour(3), "done").
				observeString(legion.FieldPhase, hour(5), "reviewing").
				observeString(legion.FieldPhase, hour(7), "done"),
		},
		{
			name:     "moved to triage and back",
			eventLog: triaged("INFRA-1"),
			observed: triagedAndObserved("INFRA-1"),
		},
	}

	for _, each := range cases {
		t.Run(each.name, func(t *testing.T) {
			fromEvents, fromObservations := fixture(hour(-24), hour(24), each.eventLog), fixture(hour(-24), hour(24), each.observed)
			if _, firstObserved := fromObservations.Phases(each.observed); firstObserved != 0 {
				t.Fatalf("the observed issue's first %d spans come from its event log, want none", firstObserved)
			}
			events, observations := timelineOf(fromEvents, each.eventLog), timelineOf(fromObservations, each.observed)
			if !reflect.DeepEqual(events, observations) {
				t.Errorf("the event log draws %v and the observations %v, want one timeline", events, observations)
			}
			eventsFacts, observationsFacts := factsAt(fromEvents, nil), factsAt(fromObservations, nil)
			if !reflect.DeepEqual(eventsFacts, observationsFacts) {
				t.Errorf("the event log answers %+v and the observations %+v, want one answer", eventsFacts, observationsFacts)
			}
		})
	}
}

// TestARootWaitsOnNoChildOutOfTheWorkflow: segment 4 holds while "the issue is a root with a child
// in a phase from planning to awaiting merge", and a child counts "only during its own work time"
// (the spec's metric 2 and "When a child makes its root wait on children"). Segment 4 reads the
// child's own work time, the spans its bars are shared out over (waitingOnChildrenSpans), so the
// hours the rule takes out of the child's work time are taken out of its root's wait as well. The
// root below plans from hour 0 to hour 7, and each child is out of the workflow for two hours of
// it: the root waits on the child through exactly the child's work time and is idle for the rest.
// Read as the phase before it, inside the child's work time, the event log's stretch would make
// the root wait on the child through it too.
//
// A child in `done` before its close is in none of the eight phases either, and the two rules hide
// each other through the computation, since an hour out of the workflow is in no work time a
// segment reads. So the second half reads segment 4 alone, over a work time that holds the child's
// hour in `done`: `done` is no phase a child works in, whatever its work time.
func TestARootWaitsOnNoChildOutOfTheWorkflow(t *testing.T) {
	cases := []struct {
		name          string
		child         *Issue
		waiting, idle float64
	}{
		{"with observations: a child moved to triage and reported again", triagedAndObserved("INFRA-2"), 4, 3},
		{"before observations: a child moved to triage and back", triaged("INFRA-2"), 4, 3},
		{"before observations: a child closed, then reopened", reopened("INFRA-2", "todo"), 5, 2},
	}
	for _, each := range cases {
		t.Run(each.name, func(t *testing.T) {
			root := newIssue("INFRA-1").worked("planner", hour(0)).closedAt(hour(7)).
				observeString(legion.FieldPhase, hour(0), "planning").
				observeString(legion.FieldPhase, hour(7), "done")
			child := each.child.under("INFRA-1")
			population := fixture(hour(-24), hour(24), root, child)
			bar := barOf(t, windowOf(population, ScopeRoots), "planner")
			if bar.Hours != 7 || bar.Segments.WaitingOnChildren != each.waiting || bar.Segments.Idle != each.idle {
				t.Errorf("the root's planner bar = %g h, %+v, want %g h waiting on the child and %g h idle",
					bar.Hours, bar.Segments, each.waiting, each.idle)
			}
			if work := windowOf(population, ScopeChildren).fact(t, child.Key).WorkHours; work != each.waiting {
				t.Errorf("the child's work time = %g h, want the %g h its root waits on it", work, each.waiting)
			}
		})
	}

	root, child := newIssue("INFRA-1"), newIssue("INFRA-2").under("INFRA-1")
	for phase, waits := range map[string]float64{legion.PhaseImplementing: 1, legion.PhaseDone: 0} {
		hour1 := Span{From: hour(1), To: hour(2)}
		view := &issueView{
			issue: root, root: root, children: []*Issue{child},
			childPhase:  map[string][]PhaseSpan{child.Key: {{Phase: phase, Span: hour1}}},
			childWorked: map[string][]Span{child.Key: {hour1}},
		}
		if got := totalOf(waitingOnChildrenSpans(view)).Hours(); got != waits {
			t.Errorf("a child in %s through an hour of its work time makes its root wait %g h, want %g", phase, got, waits)
		}
	}
}
