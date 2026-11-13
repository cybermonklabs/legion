package metrics

import (
	"maps"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/legion/legiontest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestTheFactsShareOutEachIssuesBarHours: each hour is counted once. An issue's parts add up to
// its work time less its queued time and less the hours it waited while a child worked (the
// child's own facts count those), an ask's hours are waiting on a person, and the rest go to the
// phases the status history draws, with no observation at all.
func TestTheFactsShareOutEachIssuesBarHours(t *testing.T) {
	root := newIssue("INFRA-1").worked("implementer", hour(0)).closedAt(hour(10)).
		statusAt(hour(0), "in_progress").
		statusAt(hour(6), "needs_review").
		statusAt(hour(10), "done").
		asked(hour(1), hour(2))
	child := newIssue("INFRA-2").under("INFRA-1").worked("implementer", hour(3)).closedAt(hour(5)).
		statusAt(hour(3), "in_progress").
		statusAt(hour(5), "done")
	population := fixture(hour(-24), hour(24), root, child)

	facts := ProjectFacts(population)
	// The root waited on its child from hour 3 to hour 5, and the child waited on none.
	waited := map[string]float64{"INFRA-1": 2, "INFRA-2": 0}
	for key, row := range facts {
		var sum float64
		for _, spent := range row.PhaseHours {
			sum += spent
		}
		want := row.WorkHours - row.QueuedHours - waited[key]
		if math.Abs(sum-want) > 0.001 {
			t.Errorf("%s holds %g h by phase, want its bar hours %g: %v", key, sum, want, row.PhaseHours)
		}
	}
	if got := facts["INFRA-1"]; got.PhaseHours[breakdownPerson] != 1 || got.PhaseHours[breakdownChildren] != 0 {
		t.Errorf("the root waited %g h on a person and holds %g h of waiting on children among its parts, want 1 and none: %v",
			got.PhaseHours[breakdownPerson], got.PhaseHours[breakdownChildren], got.PhaseHours)
	}
	if got := facts["INFRA-1"]; got.WorkHours != 10 || got.ReviewRounds != 1 || !got.Root || got.Tree != "INFRA-1" {
		t.Errorf("the root's facts are %+v, want 10 h of work, one review round and its own tree", got)
	}
	if got := facts["INFRA-2"]; got.WorkHours != 2 || got.Root || got.Tree != "INFRA-1" {
		t.Errorf("the child's facts are %+v, want 2 h of work in the root's tree", got)
	}
}

// TestEveryIssueWithPhaseWorkHasFactsWhateverItsHoursAre: an open issue's hours run to the
// population's end, an issue whose work took no measured time keeps an entry with no part, and an
// issue no Legion role worked has no entry at all.
func TestEveryIssueWithPhaseWorkHasFactsWhateverItsHoursAre(t *testing.T) {
	open := newIssue("INFRA-1").worked("implementer", hour(0)).statusAt(hour(0), "in_progress")
	instant := newIssue("INFRA-2").under("INFRA-1").worked("implementer", hour(4)).closedAt(hour(4))
	untouched := newIssue("INFRA-3").under("INFRA-1").closedAt(hour(6))
	facts := ProjectFacts(fixture(hour(-24), hour(8), open, instant, untouched))

	if got := facts["INFRA-1"]; got.WorkHours != 8 {
		t.Errorf("the open issue holds %g h of work, want the eight to the population's end: %+v", got.WorkHours, got)
	}
	got, has := facts["INFRA-2"]
	if !has || len(got.PhaseHours) != 0 || got.WorkHours != 0 {
		t.Errorf("the issue that closed where it opened has facts %+v (%t), want an entry with no part", got, has)
	}
	if _, worked := facts["INFRA-3"]; worked {
		t.Error("an issue no Legion role worked has facts, want none: a reader takes an entry for Legion worked it")
	}
}

// TestTheFactsCountTheSlotHoursAndTheDepthInTheTree: the slot hours are the stretch the issue's
// own `slot` observations say it held a slot, and the depth is the child_of links between the
// issue and its tree's root.
func TestTheFactsCountTheSlotHoursAndTheDepthInTheTree(t *testing.T) {
	// The root took slot 1 at hour 2 and gave it up at hour 7, so it held one for five hours.
	root := newIssue("INFRA-1").worked("architect", hour(0)).closedAt(hour(12)).
		statusAt(hour(0), "in_progress").
		statusAt(hour(12), "done").
		observe(legion.FieldSlot, hour(2), "1").
		observe(legion.FieldSlot, hour(7), "null")
	child := newIssue("INFRA-2").under("INFRA-1").worked("implementer", hour(3)).closedAt(hour(9)).
		statusAt(hour(3), "in_progress").
		statusAt(hour(9), "done")
	grandchild := newIssue("INFRA-3").under("INFRA-2").worked("tester", hour(5)).closedAt(hour(8)).
		statusAt(hour(5), "in_progress").
		statusAt(hour(8), "done")
	facts := ProjectFacts(fixture(hour(-24), hour(24), root, child, grandchild))

	if got := facts["INFRA-1"]; got.SlotHours != 5 {
		t.Errorf("the root held a slot for %g h, want the five from hour 2 to hour 7: %+v", got.SlotHours, got)
	}
	if got := facts["INFRA-2"]; got.SlotHours != 0 {
		t.Errorf("the child held a slot for %g h, want none: it has no slot observation of its own", got.SlotHours)
	}
	depths := map[string]int{"INFRA-1": 0, "INFRA-2": 1, "INFRA-3": 2}
	for key, want := range depths {
		got := facts[key]
		if got.Tree != "INFRA-1" || got.TreeDepth != want {
			t.Errorf("%s is %d links below %s, want %d below INFRA-1", key, got.TreeDepth, got.Tree, want)
		}
	}
}

// TestTheFactsNameWhenAnIssueOpenedClosedAndWasWorked: the issue's own row gives the opening and
// the close, the bar spans give the first and the last moment of its measured work, and an issue
// whose work took no measured time has neither moment.
func TestTheFactsNameWhenAnIssueOpenedClosedAndWasWorked(t *testing.T) {
	worked := newIssue("INFRA-1").openedAt(hour(-2)).worked("implementer", hour(1)).closedAt(hour(6)).
		statusAt(hour(1), "in_progress").
		statusAt(hour(6), "done")
	instant := newIssue("INFRA-2").under("INFRA-1").openedAt(hour(0)).
		worked("implementer", hour(4)).closedAt(hour(4))
	open := newIssue("INFRA-3").under("INFRA-1").openedAt(hour(1)).
		worked("tester", hour(2)).statusAt(hour(2), "in_progress")
	facts := ProjectFacts(fixture(hour(-24), hour(8), worked, instant, open))

	got := facts["INFRA-1"]
	if !got.OpenedAt.Equal(hour(-2)) || got.ClosedAt == nil || !got.ClosedAt.Equal(hour(6)) {
		t.Errorf("the worked issue opened at %s and closed at %v, want hour -2 and hour 6", got.OpenedAt, got.ClosedAt)
	}
	if got.WorkFirstAt == nil || !got.WorkFirstAt.Equal(hour(1)) || got.WorkLastAt == nil || !got.WorkLastAt.Equal(hour(6)) {
		t.Errorf("its work ran from %v to %v, want hour 1 to hour 6", got.WorkFirstAt, got.WorkLastAt)
	}
	if got := facts["INFRA-2"]; got.WorkFirstAt != nil || got.WorkLastAt != nil {
		t.Errorf("the issue whose work took no measured time ran from %v to %v, want neither moment: %+v",
			got.WorkFirstAt, got.WorkLastAt, got)
	}
	if got := facts["INFRA-3"]; got.ClosedAt != nil || got.WorkLastAt == nil || !got.WorkLastAt.Equal(hour(8)) {
		t.Errorf("the open issue closed at %v and was last worked at %v, want no close and the population's end",
			got.ClosedAt, got.WorkLastAt)
	}
}

// TestTheFactsOfNamedIssuesAreTheFactsOfTheWholeProject: ReadIssues reads one tree where
// ReadProject reads every issue of the project, and the facts of the issues asked for are the
// same either way, field by field. A consumer can therefore recompute one issue's row without
// reading a whole project's event history.
func TestTheFactsOfNamedIssuesAreTheFactsOfTheWholeProject(t *testing.T) {
	database := storetest.Open(t)
	// Two trees, so a read of one tree's issues covers one tree and leaves the other unread.
	fixture := legiontest.Seed(t, database.Pool, legiontest.Spec{Trees: 2})
	now := fixture.To
	// The first tree: Seed writes each tree's root and then that tree's children, so the
	// history's first issues per tree are the first tree.
	perTree := len(fixture.Keys) / len(fixture.Roots)
	keys, otherTree := fixture.Keys[:perTree], fixture.Keys[perTree:]

	startedProject := time.Now()
	project, err := ReadProject(t.Context(), database.Pool, nil, fixture.Project, now)
	projectRead := time.Since(startedProject)
	if err != nil {
		t.Fatalf("read the project: %v", err)
	}
	startedIssues := time.Now()
	named, err := ReadIssues(t.Context(), database.Pool, nil, fixture.Project, keys, nil, now)
	issuesRead := time.Since(startedIssues)
	if err != nil {
		t.Fatalf("read the named issues: %v", err)
	}

	whole, part := ProjectFacts(project), ProjectFacts(named)
	if len(whole) != len(fixture.PhaseWork) {
		t.Fatalf("the project's facts hold %d issues (%v), want the %d with Legion phase work",
			len(whole), slices.Sorted(maps.Keys(whole)), len(fixture.PhaseWork))
	}
	compared, worked := 0, 0
	for _, key := range keys {
		want, has := whole[key]
		if !has {
			// The follow-up nobody worked, which has facts in neither read.
			continue
		}
		compared++
		got := part[key]
		if !maps.Equal(want.PhaseHours, got.PhaseHours) || want.WorkHours != got.WorkHours ||
			want.ReviewRounds != got.ReviewRounds || want.Root != got.Root || want.Tree != got.Tree {
			t.Errorf("%s reads %+v from its own tree and %+v from the whole project, want one answer", key, got, want)
		}
		if want.WorkHours > 0 {
			worked++
		}
	}
	if compared == 0 || worked == 0 {
		t.Fatalf("%d of the tree's issues were compared and %d of them hold work hours, which proves nothing",
			compared, worked)
	}
	// The other tree is outside the keys asked for, so its issues carry no Legion work this read
	// gathered and no facts: that is what makes the two reads different reads.
	for _, key := range otherTree {
		if got, read := part[key]; read {
			t.Errorf("the facts of %s, in the tree nobody asked for, are %+v, want none", key, got)
		}
	}
	t.Logf("%d issues compared, %d of them with work hours; ReadProject took %s over %d issues and ReadIssues took %s over %d",
		compared, worked, projectRead, len(fixture.Keys), issuesRead, len(keys))
}
