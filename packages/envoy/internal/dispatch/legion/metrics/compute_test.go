package metrics

import (
	"reflect"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/legion"
)

// TestNoQueueOrParkedHourEntersTheWorkTime: the work time runs from the issue's first worker
// event to its close, less the hours it sat in backlog or icebox and the hours its observed
// phase was `admitted` with no slot held.
func TestNoQueueOrParkedHourEntersTheWorkTime(t *testing.T) {
	issue := newIssue("INFRA-1").worked("implementer", hour(0)).closedAt(hour(10)).
		statusAt(hour(0), "in_progress").
		statusAt(hour(2), "backlog").
		statusAt(hour(4), "in_progress").
		observeString(legion.FieldPhase, hour(-2), "admitted").
		observeString(legion.FieldPhase, hour(0), "planning").
		observeString(legion.FieldPhase, hour(5), "admitted").
		observeString(legion.FieldPhase, hour(6), "implementing").
		observeString(legion.FieldPhase, hour(10), "done")
	w := windowOf(fixture(hour(-24), hour(24), issue), ScopeAll)
	if len(w.finished) != 1 || len(w.timed()) != 1 {
		t.Fatalf("the window finished %d issues and timed %d, want one of each", len(w.finished), len(w.timed()))
	}
	row := w.fact(t, "INFRA-1")
	if row.WorkHours != 7 || row.ParkedHours != 2 || row.QueuedHours != 1 {
		t.Fatalf("work time = %g hours (parked %g, queued %g), want 7 of 10 hours with 2 parked and 1 back in the queue",
			row.WorkHours, row.ParkedHours, row.QueuedHours)
	}
	if median := valueOf(w.workTime().Median); median != 7 {
		t.Errorf("the median work time = %g, want 7", median)
	}
	// Queue time is every observed stretch in `admitted` with no slot, whenever it falls (the
	// spec's "Rule details — Queue time, one definition"), so this issue's is three hours: the
	// two before it was ever given work and the one it went back for. Only the second is inside
	// the work time, which is why the row above reports one.
	phases, _ := fixture(hour(-24), hour(24), issue).Phases(issue)
	if queued := totalOf(queuedSpans(phases)); queued.Hours() != 3 {
		t.Errorf("the queued spans total %v, want the three hours the issue was admitted holding no slot", queued)
	}
}

// TestTimeInIceboxIsParkedAndOutOfTheWorkTime: the work time leaves out "any time in `backlog` or
// `icebox`", and "A park runs from its `backlog` or `icebox` status write until the next status
// write" (the spec's metric 1 and "A park and role events"). The issue below sat in icebox for
// three of the eight hours from its first worker event to its close.
func TestTimeInIceboxIsParkedAndOutOfTheWorkTime(t *testing.T) {
	issue := newIssue("INFRA-1").worked("implementer", hour(0)).closedAt(hour(8)).
		statusAt(hour(0), "in_progress").
		statusAt(hour(2), "icebox").
		statusAt(hour(5), "in_progress")
	w := windowOf(fixture(hour(-24), hour(24), issue), ScopeAll)
	if timed := w.timed(); len(timed) != 1 {
		t.Fatalf("work-time rows = %d, want the one issue", len(timed))
	}
	if row := w.fact(t, "INFRA-1"); row.WorkHours != 5 || row.ParkedHours != 3 {
		t.Fatalf("work time = %g hours with %g parked, want 5 with the 3 in icebox parked", row.WorkHours, row.ParkedHours)
	}
}

// TestTheSevenBarsOfAnIssueAddUpToItsWorkTime: "An hour that work time to merge leaves out is on
// no bar of phase time: an hour in `backlog` or `icebox`, an hour back in the queue, and any hour
// before the issue's first worker event. So the bars of an issue add up to its work time, and the
// two cards agree" (the spec's "Rule details — Parked and pre-work hours in phase time"). Both
// timelines are read: the fallback, whose first phase is the tester's own event because the
// `in_progress` of admission starts none, and whose handoff leaves an hour on no phase at all,
// and the observed one, which goes back to the queue mid-life.
func TestTheSevenBarsOfAnIssueAddUpToItsWorkTime(t *testing.T) {
	// The fallback: an `in_progress` at hour -3 that starts no phase, the tester's own event at
	// hour 2 — the first worker event, where the work time and the first phase both start —
	// testing ended by its handoff at hour 3, the reviewer's own event an hour later, and two
	// parked hours from hour 5.
	fallback := newIssue("INFRA-1").
		statusAt(hour(-3), "in_progress").
		worked("tester", hour(2)).
		handedOff("test", hour(3)).
		worked("reviewer", hour(4)).
		statusAt(hour(5), "backlog").
		statusAt(hour(7), "needs_review").
		closedAt(hour(9))
	// The observed: a design hour before the first worker event, implementing from it, two hours
	// back in the queue from hour 4 with the slot given up, and one parked hour from hour 7.
	observed := newIssue("INFRA-2").worked("implementer", hour(0)).closedAt(hour(10)).
		observeString(legion.FieldPhase, hour(-1), "admitted").
		observe(legion.FieldSlot, hour(-1), "1").
		observeString(legion.FieldPhase, hour(0), "implementing").
		observeString(legion.FieldPhase, hour(4), "admitted").
		observe(legion.FieldSlot, hour(4), "null").
		observeString(legion.FieldPhase, hour(6), "implementing").
		statusAt(hour(7), "backlog").
		statusAt(hour(8), "in_progress").
		observeString(legion.FieldPhase, hour(10), "done")
	for _, one := range []struct {
		issue     *Issue
		workHours float64
		parked    float64
		queued    float64
		bars      map[string]float64
	}{
		// Seven hours of life less the two parked ones. The three implementing hours before the
		// first worker event are on no bar, the `test` handoff starts the reviewer's phase an
		// hour before the reviewer's own first event, and the two parked hours of reviewing are
		// gone.
		{fallback, 5, 2, 0, map[string]float64{
			"architect": 0, "planner": 0, "implementer": 0, "tester": 1, "reviewer": 4, "merger": 0, barNoRole: 0,
		}},
		// Ten hours of life less the hour parked and the two back in the queue. The design hour
		// before the first worker event is on no bar, and so are the queued hours.
		{observed, 7, 1, 2, map[string]float64{
			"architect": 0, "planner": 0, "implementer": 7, "tester": 0, "reviewer": 0, "merger": 0, barNoRole: 0,
		}},
	} {
		t.Run(one.issue.Key, func(t *testing.T) {
			w := windowOf(fixture(hour(-24), hour(24), one.issue), ScopeAll)
			row := w.fact(t, one.issue.Key)
			if row.WorkHours != one.workHours || row.ParkedHours != one.parked || row.QueuedHours != one.queued {
				t.Fatalf("the work time = %g hours (parked %g, queued %g), want %g (%g, %g)",
					row.WorkHours, row.ParkedHours, row.QueuedHours, one.workHours, one.parked, one.queued)
			}
			hoursPerBar, whole := w.hoursPerBar(t)
			if !reflect.DeepEqual(hoursPerBar, one.bars) {
				t.Fatalf("the bars are %v, want %v", hoursPerBar, one.bars)
			}
			if whole != row.WorkHours {
				t.Fatalf("the seven bars hold %g hours against a work time of %g: metric 2's bars and metric 1's work time disagree",
					whole, row.WorkHours)
			}
		})
	}
}

// TestAnIssueThatClosedWithNoLegionWorkIsNotCounted: the population is the issues that closed in
// the window with Legion phase work — an event of a session titled `Legion <role> · <KEY>` for
// their own key, or a handoff — and nothing else that closed. An issue the daemon reports is a
// Legion issue of its tree and still not one that finished: that rule "still needs Legion phase
// work" (the spec's "The top issue of a tree").
func TestAnIssueThatClosedWithNoLegionWorkIsNotCounted(t *testing.T) {
	worked := newIssue("INFRA-1").worked("implementer", hour(0)).closedAt(hour(2))
	handedOff := newIssue("INFRA-2").handedOff("plan", hour(1)).closedAt(hour(3))
	bare := newIssue("INFRA-3").closedAt(hour(4))
	early := newIssue("INFRA-4").worked("implementer", hour(-100)).closedAt(hour(-50))
	reported := newIssue("INFRA-5").observeString(legion.FieldPhase, hour(1), "implementing").closedAt(hour(5))
	w := windowOf(fixture(hour(-24), hour(24), worked, handedOff, bare, early, reported), ScopeAll)
	// The finished issues are every measure's population. The work-time rows below are not: an
	// issue with no worker event has none, whether or not it is counted.
	if !reflect.DeepEqual(w.finished, []string{"INFRA-1", "INFRA-2"}) {
		t.Fatalf("%v finished, want the worked issue and the one with a handoff", w.finished)
	}
	if counted := w.timed(); !reflect.DeepEqual(counted, []string{"INFRA-1", "INFRA-2"}) {
		t.Fatalf("counted %v, want the worked issue and the one with a handoff", counted)
	}
	if n := w.workTime().N; n != 2 {
		t.Fatalf("legion-work-time counts %d issues, want the worked issue and the one with a handoff", n)
	}
}

// TestAHandoffBeforeTheFirstWorkerEventDoesNotStartTheWorkTime: work time to merge runs "from the
// issue's first worker event to its close", and a worker event is an event of a session titled
// `Legion <role> · <KEY>` "whose `<KEY>` is the issue's own key" (the spec's metric 1 and "Which
// session is Legion's"). A worker's handoff is recorded with an event of its own session, so it
// is among that role's events already, and a handoff before the issue's first worker event is
// some other session's: it starts no work time. The issue below has a handoff at hour 0, its
// implementer's first event at hour 5 and its close at hour 10.
func TestAHandoffBeforeTheFirstWorkerEventDoesNotStartTheWorkTime(t *testing.T) {
	issue := newIssue("INFRA-1").
		handedOff("plan", hour(0)).
		worked("implementer", hour(5)).
		closedAt(hour(10))
	w := windowOf(fixture(hour(-24), hour(24), issue), ScopeAll)
	if timed := w.timed(); len(timed) != 1 {
		t.Fatalf("work-time rows = %d, want the one issue", len(timed))
	}
	if row := w.fact(t, "INFRA-1"); row.WorkHours != 5 || row.StartedAt == nil || !row.StartedAt.Equal(hour(5)) {
		t.Fatalf("the work time = %g hours from %v, want 5 from the implementer's first event at %v: the handoff before it starts none",
			row.WorkHours, row.StartedAt, hour(5))
	}
	if bar := barOf(t, w, "implementer"); bar.Hours != 5 {
		t.Fatalf("the implementer's bar = %g hours, want the five of the work time and none before it", bar.Hours)
	}
}

// TestEachCutCountsRootsOrChildren: a tree's root and its child, each counted alone in its own cut
// and both in the all cut. The root is read from the parent chain, since no daemon has sent a
// `tree` yet.
func TestEachCutCountsRootsOrChildren(t *testing.T) {
	root := newIssue("INFRA-1").worked("architect", hour(0)).closedAt(hour(9))
	child := newIssue("INFRA-2").under("INFRA-1").worked("implementer", hour(1)).closedAt(hour(8))
	// An issue under a parent no Legion session ever worked is a root of its own: a Legion tree
	// does not climb into the tracking issues above it.
	tracking := newIssue("INFRA-9")
	free := newIssue("INFRA-3").under("INFRA-9").worked("implementer", hour(2)).closedAt(hour(7))
	population := fixture(hour(-24), hour(24), root, child, tracking, free)
	for _, one := range []struct {
		scope Scope
		want  []string
	}{
		{ScopeAll, []string{"INFRA-1", "INFRA-2", "INFRA-3"}},
		{ScopeRoots, []string{"INFRA-1", "INFRA-3"}},
		{ScopeChildren, []string{"INFRA-2"}},
	} {
		t.Run(string(one.scope), func(t *testing.T) {
			w := windowOf(population, one.scope)
			if counted := w.timed(); !reflect.DeepEqual(counted, one.want) {
				t.Fatalf("cut %s counted %v, want %v", one.scope, counted, one.want)
			}
			if n := w.workTime().N; n != len(one.want) {
				t.Fatalf("cut %s: legion-work-time counts %d issues, want %d", one.scope, n, len(one.want))
			}
		})
	}
}

// TestEachPhaseStartsAtItsRolesFirstEventAndEndsWhereTheNextStarts is the fallback timeline, for
// an issue the sampler never observed: each role's phase starts at the first event of its
// session or at the handoff of the phase before it, whichever comes first, accepting starts at
// the review handoff, and the merging the merger's first event starts ends at the close.
func TestEachPhaseStartsAtItsRolesFirstEventAndEndsWhereTheNextStarts(t *testing.T) {
	root := newIssue("INFRA-1").
		worked("architect", hour(0)).
		worked("planner", hour(1)).
		handedOff("plan", hour(2)).
		worked("implementer", hour(3)).
		worked("tester", hour(5)).
		worked("reviewer", hour(7)).
		handedOff("review", hour(8)).
		worked("merger", hour(9)).
		closedAt(hour(10))
	population := fixture(hour(-24), hour(24), root)
	want := []string{
		"design 0-1", "planning 1-2", "implementing 2-5", "testing 5-7", "reviewing 7-8",
		"accepting 8-9", "merging 9-10",
	}
	if got := timelineOf(population, root); !reflect.DeepEqual(got, want) {
		t.Fatalf("the fallback timeline is %v, want %v", got, want)
	}
	w := windowOf(population, ScopeAll)
	hoursPerBar, _ := w.hoursPerBar(t)
	want2 := map[string]float64{
		"architect": 2, "planner": 1, "implementer": 3, "tester": 2, "reviewer": 1, "merger": 1, barNoRole: 0,
	}
	if !reflect.DeepEqual(hoursPerBar, want2) {
		t.Fatalf("the bars are %v, want %v: the architect's design and accepting, and the implementer's phase from the plan handoff rather than from its own first event, which leaves the bar of no role empty", hoursPerBar, want2)
	}
	// Nothing was observed, so segments 2 and 3 can hold through nothing and every hour is idle.
	if share := valueOf(w.phaseTime(t).IdleShare); share != 1 {
		t.Errorf("the idle share = %g, want 1 before any observation exists", share)
	}
}

// TestRetroIsOnTheImplementersBar: one bar per role, "implementer (implementing and retro, because
// the daemon gives retro to the implementer: `RoleFor` in packages/daemon/internal/phase/phase.go)
// ... and merger (merging and awaiting merge)" (the spec's metric 2). Before observations the
// `retro` status starts retro, and the merger's first event starts merging for this issue, which
// is never sent back: after a send-back a role's first event starts no phase at all (the spec's
// "No re-entry by a first event"). On the observed timeline the daemon names all three.
func TestRetroIsOnTheImplementersBar(t *testing.T) {
	for _, one := range []struct {
		name        string
		issue       *Issue
		timeline    []string
		implementer float64
		merger      float64
	}{
		{
			name: "before observations",
			issue: newIssue("INFRA-1").
				worked("implementer", hour(0)).
				statusAt(hour(2), "retro").
				worked("merger", hour(3)).
				closedAt(hour(4)),
			timeline:    []string{"implementing 0-2", "retro 2-3", "merging 3-4"},
			implementer: 3, merger: 1,
		},
		{
			name: "observed",
			issue: newIssue("INFRA-2").worked("implementer", hour(0)).closedAt(hour(6)).
				observeString(legion.FieldPhase, hour(0), "implementing").
				observeString(legion.FieldPhase, hour(1), "retro").
				observeString(legion.FieldPhase, hour(3), "merging").
				observeString(legion.FieldPhase, hour(4), "awaiting_merge"),
			timeline:    []string{"implementing 0-1", "retro 1-3", "merging 3-4", "awaiting_merge 4-6"},
			implementer: 3, merger: 3,
		},
	} {
		t.Run(one.name, func(t *testing.T) {
			population := fixture(hour(-24), hour(24), one.issue)
			if got := timelineOf(population, one.issue); !reflect.DeepEqual(got, one.timeline) {
				t.Fatalf("the timeline is %v, want %v", got, one.timeline)
			}
			w := windowOf(population, ScopeAll)
			implementer, merger := barOf(t, w, "implementer").Hours, barOf(t, w, "merger").Hours
			if implementer != one.implementer || merger != one.merger {
				t.Fatalf("the implementer's bar = %g hours and the merger's %g, want %g and %g: retro's hours are the implementer's",
					implementer, merger, one.implementer, one.merger)
			}
		})
	}
}

// TestAPhaseStartsAtTheHandoffOrStatusThatMarksItWhenItsRoleWroteNoEvent: a role whose session
// left no event on the issue still starts its phase, at the handoff the phase before it ended
// with or at the status the workflow set.
func TestAPhaseStartsAtTheHandoffOrStatusThatMarksItWhenItsRoleWroteNoEvent(t *testing.T) {
	root := newIssue("INFRA-1").
		worked("planner", hour(0)).
		handedOff("plan", hour(1)).
		statusAt(hour(0), "in_progress").
		statusAt(hour(4), "testing").
		closedAt(hour(6))
	want := []string{"planning 0-1", "implementing 1-4", "testing 4-6"}
	if got := timelineOf(fixture(hour(-24), hour(24), root), root); !reflect.DeepEqual(got, want) {
		t.Fatalf("the fallback timeline is %v, want %v", got, want)
	}
}

// TestAnIssuesOwnArchitectEventsGiveItADesignPhase: "An issue has a design phase when a session
// titled `Legion architect · <its own key>` wrote an event on it ... whether or not it has a
// parent. The design phase runs from the issue's first slot, or else from that architect's first
// event, to the first planner event. An issue with no such events has no design phase, and its
// architect bar is its accepting phase only" (the spec's "Which issues have a design phase").
func TestAnIssuesOwnArchitectEventsGiveItADesignPhase(t *testing.T) {
	root := newIssue("INFRA-1").
		worked("architect", hour(0)).
		worked("planner", hour(2)).
		handedOff("review", hour(7)).
		closedAt(hour(9))
	// A child with an architect of its own: a sub-architect titles its session by the child's
	// own key, so the child has a design phase as its root does.
	child := newIssue("INFRA-2").under("INFRA-1").
		worked("architect", hour(3)).
		worked("planner", hour(4)).
		handedOff("review", hour(6)).
		closedAt(hour(8))
	// A child no architect session of its own ever wrote on: its architect bar is its accepting
	// phase alone, from its review handoff to its close.
	plain := newIssue("INFRA-3").under("INFRA-1").
		worked("implementer", hour(1)).
		handedOff("review", hour(3)).
		closedAt(hour(5))
	// The sampler recorded a slot an hour before this one's architect spoke, which is where its
	// design phase begins.
	admitted := newIssue("INFRA-4").
		observe(legion.FieldSlot, hour(1), "2").
		worked("architect", hour(2)).
		worked("planner", hour(4)).
		closedAt(hour(6))
	population := fixture(hour(-24), hour(24), root, child, plain, admitted)
	for _, one := range []struct {
		issue *Issue
		want  []string
	}{
		{root, []string{"design 0-2", "planning 2-7", "accepting 7-9"}},
		{child, []string{"design 3-4", "planning 4-6", "accepting 6-8"}},
		{plain, []string{"implementing 1-3", "accepting 3-5"}},
		{admitted, []string{"design 1-4", "planning 4-6"}},
	} {
		t.Run(one.issue.Key, func(t *testing.T) {
			if got := timelineOf(population, one.issue); !reflect.DeepEqual(got, one.want) {
				t.Fatalf("the timeline of %s is %v, want %v", one.issue.Key, got, one.want)
			}
		})
	}
	// The child with its own architect holds a design hour and two accepting ones; the one
	// without holds its two accepting hours and no design hour at all.
	if bar := barOf(t, windowOf(population, ScopeChildren), "architect"); bar.Hours != 5 {
		t.Fatalf("the two children's architect bar = %g hours, want 5: one design hour and two accepting of INFRA-2, and the two accepting of INFRA-3", bar.Hours)
	}
	if bar := barOf(t, windowOf(fixture(hour(-24), hour(24), root, plain), ScopeChildren), "architect"); bar.Hours != 2 {
		t.Fatalf("the architect bar of a child with no architect events = %g hours, want its two accepting hours", bar.Hours)
	}
}

// TestAHandoffStartsTheNextPhaseAndLeavesNoHourBetweenThem: "Each phase ends, and the next one
// starts, at the earlier of the phase's own handoff and the next role's first event ... So a
// handoff starts the next phase, and no hour between phases is left over" (the spec's "Phase
// boundaries before observations"). The implementer here hands off two hours before the tester
// speaks and the tester three before the reviewer does, and neither gap is left on no bar.
func TestAHandoffStartsTheNextPhaseAndLeavesNoHourBetweenThem(t *testing.T) {
	root := newIssue("INFRA-1").
		worked("implementer", hour(0)).
		handedOff("implement", hour(2)).
		worked("tester", hour(4)).
		handedOff("test", hour(5)).
		worked("reviewer", hour(8)).
		closedAt(hour(10))
	population := fixture(hour(-24), hour(24), root)
	want := []string{"implementing 0-2", "testing 2-5", "reviewing 5-10"}
	if got := timelineOf(population, root); !reflect.DeepEqual(got, want) {
		t.Fatalf("the fallback timeline is %v, want %v: each handoff starts the next phase", got, want)
	}
	w := windowOf(population, ScopeAll)
	hoursPerBar, whole := w.hoursPerBar(t)
	want2 := map[string]float64{
		"architect": 0, "planner": 0, "implementer": 2, "tester": 3, "reviewer": 5, "merger": 0, barNoRole: 0,
	}
	if !reflect.DeepEqual(hoursPerBar, want2) {
		t.Fatalf("the bars are %v, want %v: the two hours after the implement handoff are the tester's and the three after the test handoff are the reviewer's", hoursPerBar, want2)
	}
	if row := w.fact(t, "INFRA-1"); whole != row.WorkHours {
		t.Fatalf("the seven bars hold %g hours against a work time of %g: a handoff left an hour between two phases", whole, row.WorkHours)
	}
}

// The fallback timeline follows the order of the steps the issue's plan names, not the default
// steps' order. Under `implementer reviewer tester` the implement handoff starts reviewing, the
// review handoff starts testing, the tester's first event is no step back, and the test handoff,
// the last step's, starts the retro. Each mark reads the version of the plan written last at or
// before it, or the first version where it falls before every version: the implement handoff at
// hour 2 reads a first version written at hour 2.5, and under a later version written at hour 4
// the marks before hour 4 read the first version and the marks after it the later one.
// `needs_review` names the first of reviewing and accepting that the line holds: accepting under
// `implementer acceptance reviewer`. The controls are the same event logs with no plan, read in
// the default steps' order.
func TestTheFallbackTimelineFollowsTheStepsOfTheIssuesPlan(t *testing.T) {
	events := func() *Issue {
		return newIssue("INFRA-1").
			worked("implementer", hour(0)).
			handedOff("implement", hour(2)).
			worked("reviewer", hour(3)).
			handedOff("review", hour(5)).
			worked("tester", hour(6)).
			handedOff("test", hour(8)).
			closedAt(hour(10))
	}
	needsReview := func() *Issue {
		return newIssue("INFRA-1").
			worked("implementer", hour(0)).
			statusAt(hour(2), "needs_review").
			closedAt(hour(4))
	}
	for _, tc := range []struct {
		name  string
		issue *Issue
		want  []string
	}{
		{"implementer reviewer tester", events().ran(hour(0), "implementer", "reviewer", "tester"),
			[]string{"implementing 0-2", "reviewing 2-5", "testing 5-8", "retro 8-10"}},
		{"the first version of the plan, written after the implement handoff", events().ran(hour(2.5), "implementer", "reviewer", "tester"),
			[]string{"implementing 0-2", "reviewing 2-5", "testing 5-8", "retro 8-10"}},
		{"a later version of the plan, read from its moment on",
			events().ran(hour(0), "implementer", "tester", "reviewer").ran(hour(4), "implementer", "reviewer", "tester"),
			[]string{"implementing 0-2", "testing 2-3", "reviewing 3-5", "testing 5-8", "retro 8-10"}},
		{"control: no plan, the default steps", events(),
			[]string{"implementing 0-2", "testing 2-3", "reviewing 3-5", "accepting 5-8", "reviewing 8-10"}},
		{"needs_review under an acceptance before the review", needsReview().ran(hour(0), "implementer", "acceptance", "reviewer"),
			[]string{"implementing 0-2", "accepting 2-4"}},
		{"control: needs_review with no plan, the default steps", needsReview(),
			[]string{"implementing 0-2", "reviewing 2-4"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := timelineOf(fixture(hour(-24), hour(24), tc.issue), tc.issue); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("the fallback timeline is %v, want %v", got, tc.want)
			}
		})
	}
}

// TestTheNoRoleBarHoldsAwaitingPersonAndHeld: "A seventh bar, 'no role', holds the phases no role
// works: `awaiting_person` and `held`", and the spec's "Phase boundaries before observations"
// holds it to the phases no role works, of which dispatch://INFRA-3320 adds a third
// (TestTheNoRoleBarHoldsAParentsWaitOnItsChildren). Every other hour of the work time is on a
// role's bar.
func TestTheNoRoleBarHoldsAwaitingPersonAndHeld(t *testing.T) {
	issue := newIssue("INFRA-1").worked("implementer", hour(0)).closedAt(hour(6)).
		observeString(legion.FieldPhase, hour(0), "implementing").
		observeString(legion.FieldPhase, hour(1), "awaiting_person").
		observeString(legion.FieldPhase, hour(2), "held").
		observeString(legion.FieldPhase, hour(3), "implementing").
		observeString(legion.FieldPhase, hour(6), "done")
	w := windowOf(fixture(hour(-24), hour(24), issue), ScopeAll)
	bar := barOf(t, w, barNoRole)
	if bar.Hours != 2 {
		t.Fatalf("the no-role bar = %g hours, want the awaiting_person hour and the held one", bar.Hours)
	}
	// The hour the daemon recorded the issue in `awaiting_person` is segment 1 of that bar; the
	// held hour is no wait on a person, so it is idle.
	if bar.Segments != (Segments{WaitingOnPerson: 1, Idle: 1}) {
		t.Fatalf("the no-role bar is %+v, want the awaiting_person hour waiting on a person and the held one idle", bar.Segments)
	}
	if hours := barOf(t, w, "implementer").Hours; hours != 4 {
		t.Fatalf("the implementer bar = %g hours, want the four hours of implementing", hours)
	}
}

// TestTheNoRoleBarHoldsAParentsWaitOnItsChildren: `awaiting_children`, the phase a parent waits
// in while a Dispatch child of it is open (dispatch://INFRA-3320, R11), is a phase no role works,
// so its hours are on the "no role" bar, and the seven bars of a parent that waited add up to its
// work time. The parent below implements for an hour, waits on its children for two, and comes
// back to the testing start it held for three.
func TestTheNoRoleBarHoldsAParentsWaitOnItsChildren(t *testing.T) {
	issue := newIssue("INFRA-1").worked("implementer", hour(0)).closedAt(hour(6)).
		observeString(legion.FieldPhase, hour(0), "implementing").
		observeString(legion.FieldPhase, hour(1), "awaiting_children").
		observeString(legion.FieldPhase, hour(3), "testing").
		observeString(legion.FieldPhase, hour(6), "done")
	partitionsItsWorkTime(t, issue)
	w := windowOf(fixture(hour(-24), hour(24), issue), ScopeAll)
	if hours := barOf(t, w, barNoRole).Hours; hours != 2 {
		t.Fatalf("the no-role bar = %g hours, want the two hours the parent waited on its children", hours)
	}
	if hours := barOf(t, w, "implementer").Hours; hours != 1 {
		t.Fatalf("the implementer bar = %g hours, want the one hour of implementing", hours)
	}
	if hours := barOf(t, w, "tester").Hours; hours != 3 {
		t.Fatalf("the tester bar = %g hours, want the three hours of testing after the wait", hours)
	}
}

// TestARolesMedianCountsAnIssueItNeverWorkedAsZero: "Each role's median runs over every finished
// issue of the window, and an issue no worker of that role touched counts as zero hours, so a role
// that works few issues reads a low median" (the spec's metric 2). The tester worked one of the
// three issues below, for three hours, so its median is the middle of 0, 0 and 3.
func TestARolesMedianCountsAnIssueItNeverWorkedAsZero(t *testing.T) {
	tested := newIssue("INFRA-1").worked("implementer", hour(0)).worked("tester", hour(1)).closedAt(hour(4))
	untested := newIssue("INFRA-2").worked("implementer", hour(0)).closedAt(hour(4))
	alsoUntested := newIssue("INFRA-3").worked("implementer", hour(0)).closedAt(hour(4))
	bar := barOf(t, windowOf(fixture(hour(-24), hour(24), tested, untested, alsoUntested), ScopeAll), "tester")
	if bar.Hours != 3 || valueOf(bar.MedianPerIssue) != 0 {
		t.Fatalf("the tester's bar = %g hours with a median of %g per issue, want 3 hours and a median of 0: two of the three finished issues held no testing hour",
			bar.Hours, valueOf(bar.MedianPerIssue))
	}
}

// TestAfterASendBackTheDispatchStatusSetsThePhase: "After a send-back, the Dispatch status sets
// the phase: `in_progress` after `testing`, `needs_review` or `retro` is implementing, `testing`
// is testing, and `needs_review` is reviewing until the `review` handoff of that round, then
// accepting until the status changes" (the spec's "Phase boundaries before observations"). The
// round below is the second one: no role writes a first event twice, so the statuses are the
// only record of it.
func TestAfterASendBackTheDispatchStatusSetsThePhase(t *testing.T) {
	issue := newIssue("INFRA-1").
		statusAt(hour(-1), "in_progress").
		worked("implementer", hour(0)).
		worked("tester", hour(1)).
		statusAt(hour(1), "testing").
		worked("reviewer", hour(2)).
		statusAt(hour(2), "needs_review").
		// The reviewer asked for changes: implementing, testing and reviewing again, each named
		// by the status alone, and the round's review handoff starts the accepting that follows.
		statusAt(hour(3), "in_progress").
		statusAt(hour(4), "testing").
		statusAt(hour(5), "needs_review").
		handedOff("review", hour(6)).
		statusAt(hour(8), "done").
		closedAt(hour(8))
	population := fixture(hour(-24), hour(24), issue)
	// The admission's `in_progress` at hour -1 starts no phase ("A status write such as the
	// `in_progress` of admission starts no phase", the spec's "What starts planning"), and this
	// issue has no planner event, so its timeline opens at its implementer's first event.
	want := []string{
		"implementing 0-1", "testing 1-2", "reviewing 2-3", "implementing 3-4",
		"testing 4-5", "reviewing 5-6", "accepting 6-8",
	}
	if got := timelineOf(population, issue); !reflect.DeepEqual(got, want) {
		t.Fatalf("the fallback timeline is %v, want %v", got, want)
	}
	w := windowOf(population, ScopeAll)
	hoursPerBar, whole := w.hoursPerBar(t)
	// The hour before the first worker event is on no bar, whatever phase named it.
	want2 := map[string]float64{
		"architect": 2, "planner": 0, "implementer": 2, "tester": 2, "reviewer": 2, "merger": 0, barNoRole: 0,
	}
	if !reflect.DeepEqual(hoursPerBar, want2) {
		t.Fatalf("the bars are %v, want %v", hoursPerBar, want2)
	}
	if row := w.fact(t, "INFRA-1"); whole != row.WorkHours {
		t.Fatalf("the seven bars hold %g hours against a work time of %g", whole, row.WorkHours)
	}
	row := w.fact(t, "INFRA-1")
	if row.ReviewRounds != 2 || !row.SentBack {
		t.Fatalf("the issue has %d review rounds and sent_back=%t, want two rounds and the send-back its statuses record", row.ReviewRounds, row.SentBack)
	}
	if row.ImplementingHoursAfterFirstReview != 1 || row.TestingHoursAfterFirstReview != 1 {
		t.Fatalf("after the first review: %g implementing and %g testing hours, want the hour of each the second round holds",
			row.ImplementingHoursAfterFirstReview, row.TestingHoursAfterFirstReview)
	}
}

// TestARolesFirstEventAfterItsPhaseHasPassedLeavesThePhaseUnchanged: "A role's first event starts
// that role's phase only when that phase is later in the workflow than the furthest phase the
// issue has yet reached; otherwise the event is passed over and the phase is unchanged" (the
// spec's "No re-entry by a first event"). The statuses below took the issue through testing into
// reviewing before its tester first wrote on it, during the review: the review runs on to its
// handoff, and no hour of it goes back to testing.
func TestARolesFirstEventAfterItsPhaseHasPassedLeavesThePhaseUnchanged(t *testing.T) {
	issue := newIssue("INFRA-1").
		worked("implementer", hour(0)).
		statusAt(hour(2), "testing").
		statusAt(hour(3), "needs_review").
		worked("tester", hour(4)).
		handedOff("review", hour(5)).
		closedAt(hour(6))
	want := []string{"implementing 0-2", "testing 2-3", "reviewing 3-5", "accepting 5-6"}
	if got := timelineOf(fixture(hour(-24), hour(24), issue), issue); !reflect.DeepEqual(got, want) {
		t.Fatalf("the fallback timeline is %v, want %v: the tester's first event during the review is passed over", got, want)
	}
}

// TestAfterASendBackARolesFirstEventStartsNoPhase: "After a send-back, the next phases come from
// status writes and handoffs" (the spec's "No re-entry by a first event"), and "After a
// send-back, the Dispatch status sets the phase" ("Phase boundaries before observations"). Before
// the send-back a role's first event still starts its phase where that phase is later than any
// the issue has reached, which the first case holds and the implementer's and tester's first
// events of the second do too.
//
// The second case is the shape of INFRA-745 and INFRA-122 in dispatch://INFRA-979's corpus: sent
// back to implementing, the issue goes through testing and review again into `retro`, and only
// then does its merger first write on it. Merging is later than any phase the issue had reached,
// but the event comes after the send-back, so it starts nothing: the retro the `retro` status set
// runs on to the close, on the implementer's bar. The third is the same rule earlier in a round: a
// reviewer's first event while the issue is implementing again after its tester sent it back
// leaves the implementing the `in_progress` set running to the next status.
func TestAfterASendBackARolesFirstEventStartsNoPhase(t *testing.T) {
	for _, one := range []struct {
		name     string
		issue    *Issue
		timeline []string
		bars     map[string]float64
	}{
		{
			name: "never sent back, the merger's first event starts merging",
			issue: newIssue("INFRA-1").
				worked("planner", hour(0)).
				worked("implementer", hour(1)).
				worked("tester", hour(2)).
				statusAt(hour(3), "needs_review").
				statusAt(hour(4), "retro").
				worked("merger", hour(5)).
				statusAt(hour(7), "done").
				closedAt(hour(7)),
			timeline: []string{
				"planning 0-1", "implementing 1-2", "testing 2-3", "reviewing 3-4", "retro 4-5", "merging 5-7",
			},
			bars: map[string]float64{
				"architect": 0, "planner": 1, "implementer": 2, "tester": 1, "reviewer": 1, "merger": 2, barNoRole: 0,
			},
		},
		{
			name: "sent back by its reviewer, the merger's first event after retro starts nothing",
			issue: newIssue("INFRA-1").
				worked("planner", hour(0)).
				worked("implementer", hour(1)).
				worked("tester", hour(2)).
				statusAt(hour(3), "needs_review").
				statusAt(hour(4), "in_progress").
				statusAt(hour(5), "testing").
				statusAt(hour(6), "needs_review").
				statusAt(hour(7), "retro").
				worked("merger", hour(8)).
				statusAt(hour(10), "done").
				closedAt(hour(10)),
			timeline: []string{
				"planning 0-1", "implementing 1-2", "testing 2-3", "reviewing 3-4", "implementing 4-5",
				"testing 5-6", "reviewing 6-7", "retro 7-10",
			},
			bars: map[string]float64{
				"architect": 0, "planner": 1, "implementer": 5, "tester": 2, "reviewer": 2, "merger": 0, barNoRole: 0,
			},
		},
		{
			name: "sent back by its tester, the reviewer's first event while implementing starts nothing",
			issue: newIssue("INFRA-1").
				worked("implementer", hour(0)).
				statusAt(hour(1), "testing").
				statusAt(hour(2), "in_progress").
				worked("reviewer", hour(3)).
				statusAt(hour(4), "testing").
				statusAt(hour(5), "needs_review").
				statusAt(hour(6), "done").
				closedAt(hour(6)),
			timeline: []string{"implementing 0-1", "testing 1-2", "implementing 2-4", "testing 4-5", "reviewing 5-6"},
			bars: map[string]float64{
				"architect": 0, "planner": 0, "implementer": 3, "tester": 2, "reviewer": 1, "merger": 0, barNoRole: 0,
			},
		},
	} {
		t.Run(one.name, func(t *testing.T) {
			population := fixture(hour(-24), hour(24), one.issue)
			if got := timelineOf(population, one.issue); !reflect.DeepEqual(got, one.timeline) {
				t.Fatalf("the fallback timeline is %v, want %v", got, one.timeline)
			}
			w := windowOf(population, ScopeAll)
			hoursPerBar, whole := w.hoursPerBar(t)
			if !reflect.DeepEqual(hoursPerBar, one.bars) {
				t.Fatalf("the bars are %v, want %v", hoursPerBar, one.bars)
			}
			if row := w.fact(t, one.issue.Key); whole != row.WorkHours {
				t.Fatalf("the seven bars hold %g hours against a work time of %g", whole, row.WorkHours)
			}
		})
	}
}

// TestPlanningStartsAtTheFirstPlannerEventAndNotTheAdmissionWrite: "For an issue with no design
// phase, [planning] starts at the first planner event, as every other role's phase starts at its
// role's first event. A status write such as the `in_progress` of admission starts no phase" (the
// spec's "What starts planning"). The issue below has no architect event, so it has no design
// phase, and the workflow's admission write two hours before its planner spoke opens nothing: its
// first phase is the planning its planner's own first event starts.
func TestPlanningStartsAtTheFirstPlannerEventAndNotTheAdmissionWrite(t *testing.T) {
	issue := newIssue("INFRA-1").
		statusAt(hour(-2), "in_progress").
		worked("planner", hour(0)).
		worked("implementer", hour(2)).
		statusAt(hour(4), "done").
		closedAt(hour(4))
	population := fixture(hour(-24), hour(24), issue)
	want := []string{"planning 0-2", "implementing 2-4"}
	if got := timelineOf(population, issue); !reflect.DeepEqual(got, want) {
		t.Fatalf("the fallback timeline is %v, want %v", got, want)
	}
	w := windowOf(population, ScopeAll)
	hoursPerBar, whole := w.hoursPerBar(t)
	want2 := map[string]float64{
		"architect": 0, "planner": 2, "implementer": 2, "tester": 0, "reviewer": 0, "merger": 0, barNoRole: 0,
	}
	if !reflect.DeepEqual(hoursPerBar, want2) {
		t.Fatalf("the bars are %v, want %v", hoursPerBar, want2)
	}
	if row := w.fact(t, "INFRA-1"); whole != row.WorkHours {
		t.Fatalf("the seven bars hold %g hours against a work time of %g", whole, row.WorkHours)
	}
}

// TestWorkHoursBeforeTheFirstPhaseMarkAreOnThatPhasesBar: "When an issue's first worker event
// comes before the start of its first phase, the work hours in between go on the bar of that
// first phase. So every work-time hour stays on a bar" (the spec's "Work hours before any phase
// mark"). Every role's first event is a phase mark of its own, so work hours fall ahead of every
// mark only on an issue whose Legion work is handoffs alone, whose first handoff opens its work
// time (firstWorkerEvent). The issue below opens it with a handoff whose word names no phase —
// `retro`, which the daemon's retro never writes — two hours before its `plan` handoff, and the
// admission's `in_progress` before that opens nothing, so its first phase starts after its work
// time does.
func TestWorkHoursBeforeTheFirstPhaseMarkAreOnThatPhasesBar(t *testing.T) {
	issue := newIssue("INFRA-1").
		statusAt(hour(-2), "in_progress").
		handedOff("retro", hour(0)).
		handedOff("plan", hour(2)).
		handedOff("implement", hour(3)).
		closedAt(hour(5))
	population := fixture(hour(-24), hour(24), issue)
	// The implementing the plan handoff starts at hour 2 reaches back to the handoff that opened
	// the work time, so the two hours between them are on the implementer's bar and not on none.
	want := []string{"implementing 0-3", "testing 3-5"}
	if got := timelineOf(population, issue); !reflect.DeepEqual(got, want) {
		t.Fatalf("the fallback timeline is %v, want %v", got, want)
	}
	w := windowOf(population, ScopeAll)
	hoursPerBar, whole := w.hoursPerBar(t)
	want2 := map[string]float64{
		"architect": 0, "planner": 0, "implementer": 3, "tester": 2, "reviewer": 0, "merger": 0, barNoRole: 0,
	}
	if !reflect.DeepEqual(hoursPerBar, want2) {
		t.Fatalf("the bars are %v, want %v", hoursPerBar, want2)
	}
	row := w.fact(t, "INFRA-1")
	if row.WorkHours != 5 {
		t.Fatalf("the work time = %g hours, want the five from the handoff that opened it to the close", row.WorkHours)
	}
	if whole != row.WorkHours {
		t.Fatalf("the seven bars hold %g hours against a work time of %g: an hour of the work time is on no bar",
			whole, row.WorkHours)
	}
}

// TestWorkHoursBeforeTheFirstObservedMarkAreOnThatPhasesBar: the same rule on the other timeline.
// The hours before an issue's first observation are the event log's to name (the spec's
// "Statuses before observations"), so the first observed phase is the issue's first phase only
// where the event log names none before it: here, an issue whose Legion work opens with a
// handoff naming no phase two hours before the sampler first saw it. Those hours are on the bar
// of the phase it is first seen in, as the fallback's are, or the seven bars would not add up to
// the work time.
func TestWorkHoursBeforeTheFirstObservedMarkAreOnThatPhasesBar(t *testing.T) {
	issue := newIssue("INFRA-1").
		handedOff("retro", hour(0)).
		observeString(legion.FieldPhase, hour(2), "implementing").
		observeString(legion.FieldPhase, hour(4), "testing").
		closedAt(hour(6))
	population := fixture(hour(-24), hour(24), issue)
	want := []string{"implementing 0-4", "testing 4-6"}
	if got := timelineOf(population, issue); !reflect.DeepEqual(got, want) {
		t.Fatalf("the observed timeline is %v, want %v", got, want)
	}
	w := windowOf(population, ScopeAll)
	hoursPerBar, whole := w.hoursPerBar(t)
	want2 := map[string]float64{
		"architect": 0, "planner": 0, "implementer": 4, "tester": 2, "reviewer": 0, "merger": 0, barNoRole: 0,
	}
	if !reflect.DeepEqual(hoursPerBar, want2) {
		t.Fatalf("the bars are %v, want %v", hoursPerBar, want2)
	}
	row := w.fact(t, "INFRA-1")
	if row.WorkHours != 6 {
		t.Fatalf("the work time = %g hours, want the six from the handoff that opened it to the close", row.WorkHours)
	}
	if whole != row.WorkHours {
		t.Fatalf("the seven bars hold %g hours against a work time of %g: an hour of the work time is on no bar",
			whole, row.WorkHours)
	}
}

// TestAnIssueFirstObservedInFlightTakesItsEarlierPhasesFromItsEvents: "An issue in flight when its
// first observation is stored takes its phases from observations only from that observation on.
// Its hours before it come from these status rules, so they keep the phases its events mark
// rather than all falling on the first phase the sampler saw" (the spec's "Statuses before
// observations"). The issue below was implemented, tested, reviewed and sent back before the
// sampler's first read found it implementing at hour 4; from there the daemon's phases name its
// second round. Metric 3 reads the same two halves: the status history's first round and its
// send-back before hour 4, and the observed second round after it, whose `needs_review` status
// is the observations' to count and not the status history's.
func TestAnIssueFirstObservedInFlightTakesItsEarlierPhasesFromItsEvents(t *testing.T) {
	issue := newIssue("INFRA-1").
		worked("implementer", hour(0)).
		statusAt(hour(0), "in_progress").
		statusAt(hour(1), "testing").
		statusAt(hour(2), "needs_review").
		statusAt(hour(3), "in_progress").
		observeString(legion.FieldPhase, hour(4), "implementing").
		statusAt(hour(5), "testing").
		observeString(legion.FieldPhase, hour(5), "testing").
		statusAt(hour(6), "needs_review").
		observeString(legion.FieldPhase, hour(6), "reviewing").
		observeString(legion.FieldPhase, hour(7), "accepting").
		statusAt(hour(8), "done").
		closedAt(hour(8))
	population := fixture(hour(-24), hour(24), issue)
	want := []string{
		"implementing 0-1", "testing 1-2", "reviewing 2-3", "implementing 3-4",
		"implementing 4-5", "testing 5-6", "reviewing 6-7", "accepting 7-8",
	}
	if got := timelineOf(population, issue); !reflect.DeepEqual(got, want) {
		t.Fatalf("the timeline is %v, want %v: the event log's phases up to the first observation at hour 4 and the daemon's from it on",
			got, want)
	}
	w := windowOf(population, ScopeAll)
	hoursPerBar, _ := w.hoursPerBar(t)
	want2 := map[string]float64{
		"architect": 1, "planner": 0, "implementer": 3, "tester": 2, "reviewer": 2, "merger": 0, barNoRole: 0,
	}
	if !reflect.DeepEqual(hoursPerBar, want2) {
		t.Fatalf("the bars are %v, want %v: the first round's testing and reviewing hours are not the implementing the sampler first saw",
			hoursPerBar, want2)
	}
	row := w.fact(t, "INFRA-1")
	if row.ReviewRounds != 2 || !row.SentBack {
		t.Fatalf("the issue has %d review rounds and sent_back=%t, want the round and the send-back its statuses record before hour 4 and the round observed after it",
			row.ReviewRounds, row.SentBack)
	}
	if row.ImplementingHoursAfterFirstReview != 2 || row.TestingHoursAfterFirstReview != 1 {
		t.Fatalf("after the first review: %g implementing and %g testing hours, want 2 and 1 from the review at hour 2",
			row.ImplementingHoursAfterFirstReview, row.TestingHoursAfterFirstReview)
	}
}

// TestAnAdmittedIssueHoldingASlotIsDesignAndOneHoldingNoneIsQueue: the daemon has one `admitted`
// phase; the board's Design column is the part of it an issue holds a slot through, and the rest
// is the waiting line, whose hours are on no bar.
func TestAnAdmittedIssueHoldingASlotIsDesignAndOneHoldingNoneIsQueue(t *testing.T) {
	root := newIssue("INFRA-1").worked("architect", hour(0)).closedAt(hour(4)).
		observeString(legion.FieldPhase, hour(0), "admitted").
		observe(legion.FieldSlot, hour(1), "2").
		observeString(legion.FieldPhase, hour(3), "planning").
		observeString(legion.FieldPhase, hour(4), "done")
	population := fixture(hour(-24), hour(24), root)
	want := []string{"queued 0-1", "design 1-3", "planning 3-4"}
	if got := timelineOf(population, root); !reflect.DeepEqual(got, want) {
		t.Fatalf("the observed timeline is %v, want %v", got, want)
	}
	w := windowOf(population, ScopeAll)
	if bar := barOf(t, w, "architect"); bar.Hours != 2 {
		t.Fatalf("the architect bar = %g hours, want the two design hours and no queue hour", bar.Hours)
	}
	_, whole := w.hoursPerBar(t)
	if whole != 3 {
		t.Fatalf("the bars hold %g hours of a four-hour life, want 3: the queued hour is on no bar", whole)
	}
}

// TestAMedianInterpolatesBetweenTheClosestRanks: "Medians and p90s use linear interpolation
// between the closest ranks".
func TestAMedianInterpolatesBetweenTheClosestRanks(t *testing.T) {
	for _, one := range []struct {
		name   string
		values []float64
		median float64
		p90    float64
	}{
		{"none", nil, 0, 0},
		{"one", []float64{4}, 4, 4},
		{"two", []float64{1, 4}, 2.5, 3.7},
		{"four", []float64{1, 2, 3, 4}, 2.5, 3.7},
	} {
		t.Run(one.name, func(t *testing.T) {
			sorted := measures.MergedSamples([]measures.DayPart{{Series: "hours", Samples: one.values}}, "hours")
			if got := quantile(sorted, 0.5); got != one.median {
				t.Errorf("median of %v = %g, want %g", one.values, got, one.median)
			}
			if got := quantile(sorted, 0.9); got != one.p90 {
				t.Errorf("p90 of %v = %g, want %g", one.values, got, one.p90)
			}
		})
	}
}
