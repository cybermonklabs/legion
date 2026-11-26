package replay

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// chainEvent is one issue event of a trace that chainTrace builds: its moment, its issue, the
// issue's Dispatch parent ("" for none) and its own priority as Dispatch serves it (nil for none),
// and, where shows is set, the priority Dispatch resolved for it (resolved_priority, nil for null).
type chainEvent struct {
	at       time.Time
	key      string
	parent   string
	priority *int
	shows    bool
	resolved *int
}

// chainTrace is a trace of project ACME from 09:00 to 10:00 that holds events alone, each an issue
// event of its issue in todo, numbered in the order given: the input resolvePriorities reads.
func chainTrace(events ...chainEvent) Trace {
	t := Trace{Project: "ACME", From: clock(9, 0), To: clock(10, 0), End: clock(10, 0), Issues: map[string]*IssueTrace{}}
	for i, e := range events {
		iss, held := t.Issues[e.key]
		if !held {
			project, _, _ := strings.Cut(e.key, "-")
			iss = &IssueTrace{Key: e.key, Project: project}
			t.Issues[e.key] = iss
		}
		state := &DispatchIssueState{Status: "todo", Parent: e.parent, ShowsResolved: e.shows}
		state.Priority, _ = record.DispatchPriority(e.priority)
		if e.shows {
			state.Resolved, _ = record.DispatchPriority(e.resolved)
		}
		iss.Dispatch = append(iss.Dispatch, DispatchEvent{ID: int64(100 + i), Seq: int64(len(iss.Dispatch) + 1), Key: e.key, Project: iss.Project,
			Type: "issue.updated", At: e.at, Issue: state})
	}
	return t
}

// wantPriorities fails the test for each issue of want whose resolved priority at at is not the
// one want gives it.
func wantPriorities(t *testing.T, p resolvedPriorities, at time.Time, want map[string]record.Priority) {
	t.Helper()
	for _, key := range slices.Sorted(maps.Keys(want)) {
		if got := p.at(key, at); got != want[key] {
			t.Errorf("at %s %s resolves to %q, want %q", at.Format("15:04"), key, got, want[key])
		}
	}
}

// recordLineTimeline is the line timeline of the record t (lineTimeline) as its scorecard reads it
// (scoreTrace): by its own ranks and the priorities Dispatch resolves from its own events, under the
// rule its daemon ran at each moment.
func recordLineTimeline(t Trace) Timeline {
	priorities := resolvePriorities(t)
	return lineTimeline(t, ranksOf(t, priorities), priorities, t.ruleAt)
}

// An issue with no priority of its own takes the priority of the nearest issue above it on its
// Dispatch parent chain that has one, in any project, as Dispatch resolves it (spec R10; Dispatch's
// J1), and never the top issue's of its chain: ACME-1, with none, stands under ACMEAUX-2, at P0,
// which stands under ACMEAUX-1, the top, at P2, so ACME-1 resolves to P0. ACME-2, at P3 of its
// own, stands under ACMEAUX-2 too and keeps its own P3. ACME-3, with none, stands under ACMEAUX-3,
// with none, under ACMEAUX-1: the walk goes on past an ancestor with none to the first with one,
// P2, two levels up in another project. ACME-4, with none, stands under ACME-5, of its own project,
// at P1, which stands under ACMEAUX-2: P1, the nearer. A walk that takes the top issue's priority
// gives ACME-1 and ACME-4 P2 and fails.
func TestAnIssueTakesThePriorityOfTheNearestIssueAboveItThatHasOne(t *testing.T) {
	t.Parallel()
	p := resolvePriorities(chainTrace(
		chainEvent{at: clock(8, 0), key: "ACMEAUX-1", priority: new(2)},
		chainEvent{at: clock(8, 1), key: "ACMEAUX-2", parent: "ACMEAUX-1", priority: new(0)},
		chainEvent{at: clock(8, 2), key: "ACMEAUX-3", parent: "ACMEAUX-1"},
		chainEvent{at: clock(8, 3), key: "ACME-5", parent: "ACMEAUX-2", priority: new(1)},
		chainEvent{at: clock(9, 5), key: "ACME-1", parent: "ACMEAUX-2"},
		chainEvent{at: clock(9, 6), key: "ACME-2", parent: "ACMEAUX-2", priority: new(3)},
		chainEvent{at: clock(9, 7), key: "ACME-3", parent: "ACMEAUX-3"},
		chainEvent{at: clock(9, 8), key: "ACME-4", parent: "ACME-5"},
	))
	wantPriorities(t, p, clock(9, 30), map[string]record.Priority{"ACME-1": record.P0, "ACME-2": record.P3, "ACME-3": record.P2, "ACME-4": record.P1,
		"ACME-5": record.P1, "ACMEAUX-2": record.P0, "ACMEAUX-3": record.P2})
	if missing := p.missingAncestors(); len(missing) != 0 {
		t.Errorf("the window holds every ancestor, and the priorities name %v missing", missing)
	}
}

// An issue whose own priority is none and no issue of whose chain has one resolves to none, even
// where an issue of its chain had one before: ACME-1, filed at 08:10, stands under ACME-4, with
// none, under ACMEAUX-5, the top, at P1 until a person clears it at 08:30, so ACME-1 is P1 from
// 08:10 and none from 08:30; ACME-2 has no parent and no priority, none from its filing. Neither
// keeps a priority it no longer inherits or takes one from elsewhere, such as ACMEAUX-6, at P0,
// on no chain of theirs, and no ancestor is missing.
func TestAnIssueWhoseChainHasNoPriorityHasNone(t *testing.T) {
	t.Parallel()
	p := resolvePriorities(chainTrace(
		chainEvent{at: clock(8, 0), key: "ACMEAUX-5", priority: new(1)},
		chainEvent{at: clock(8, 0), key: "ACMEAUX-6", priority: new(0)},
		chainEvent{at: clock(8, 1), key: "ACME-4", parent: "ACMEAUX-5"},
		chainEvent{at: clock(8, 10), key: "ACME-1", parent: "ACME-4"},
		chainEvent{at: clock(8, 30), key: "ACMEAUX-5"},
		chainEvent{at: clock(9, 6), key: "ACME-2"},
	))
	wantPriorities(t, p, clock(9, 30), map[string]record.Priority{"ACME-1": record.NoPriority, "ACME-2": record.NoPriority, "ACME-4": record.NoPriority})
	for key, want := range map[string][]PriorityAt{
		"ACME-1": {{At: clock(8, 10), Priority: record.P1}, {At: clock(8, 30), Priority: record.NoPriority}},
		"ACME-2": {{At: clock(9, 6), Priority: record.NoPriority}},
	} {
		if got := p.from(key, want[0].At); !slices.Equal(got, want) {
			t.Errorf("%s resolves to %v, want %v", key, got, want)
		}
	}
	if missing := p.missingAncestors(); len(missing) != 0 {
		t.Errorf("the window holds every ancestor, and the priorities name %v missing", missing)
	}
}

// The walk reads 32 levels above an issue, as Dispatch's does (refs.IssueDepthCap, ancestorLevels),
// and no further: ACME-1's chain, ACMEAUX-1 to ACMEAUX-32, has its one priority, P1, at the 32nd
// level, which ACME-1 takes; ACME-2's, ACMEAUX-101 to ACMEAUX-133, has its one, P0, at the 33rd,
// which ACME-2 does not, so it has none; and ACME-3's 32 levels, ACMEAUX-201 to ACMEAUX-232, have
// none, the 32nd under ACMEAUX-299, which the window does not hold: an issue 33 levels up is no
// input of the priority, so ACME-3 has none and no ancestor is missing.
func TestTheWalkReadsThirtyTwoLevelsAboveAnIssue(t *testing.T) {
	t.Parallel()
	var events []chainEvent
	chain := func(issue string, first, levels int, top *int, over string) {
		for level := levels; level >= 1; level-- {
			key, parent := fmt.Sprintf("ACMEAUX-%d", first+level-1), fmt.Sprintf("ACMEAUX-%d", first+level)
			e := chainEvent{at: clock(8, 0), key: key, parent: parent}
			if level == levels {
				e.parent, e.priority = over, top
			}
			events = append(events, e)
		}
		events = append(events, chainEvent{at: clock(9, 5), key: issue, parent: fmt.Sprintf("ACMEAUX-%d", first)})
	}
	chain("ACME-1", 1, 32, new(1), "")
	chain("ACME-2", 101, 33, new(0), "")
	chain("ACME-3", 201, 32, nil, "ACMEAUX-299")
	p := resolvePriorities(chainTrace(events...))
	wantPriorities(t, p, clock(9, 30), map[string]record.Priority{"ACME-1": record.P1, "ACME-2": record.NoPriority, "ACME-3": record.NoPriority})
	if missing := p.missingAncestors(); len(missing) != 0 {
		t.Errorf("no input of a priority is missing, and the priorities name %v missing", missing)
	}
}

// Where an event of an issue carries the priority Dispatch resolved for it (resolved_priority),
// that value stands from the event on, a null one as none, and the replay reconstructs nothing of
// the issue from then on; before the issue's first such event its priority is reconstructed from
// the chain (a record that mixes a release before resolved_priority and one with it). ACME-1, with
// none of its own, is filed at 09:05 under ACMEAUX-1, at P2, by an event that carries no resolved
// priority: P2 from 09:05. At 09:20 an event of ACME-1 carries Dispatch's resolved P0, which the
// chain would not give: P0 from 09:20. At 09:30 ACMEAUX-1 is lowered to P3, and no event of ACME-1
// follows: ACME-1 stays at Dispatch's P0. At 09:40 an event of ACME-1 carries a null resolved
// priority: none from 09:40. ACME-2, filed under ACMEAUX-1 at 09:06 by an event that carries its
// resolved priority, P2, stays at P2 through ACMEAUX-1's change at 09:30, which Dispatch recorded no
// update of ACME-2 for.
func TestARecordedResolvedPriorityWinsFromItsEventOn(t *testing.T) {
	t.Parallel()
	p := resolvePriorities(chainTrace(
		chainEvent{at: clock(8, 0), key: "ACMEAUX-1", priority: new(2)},
		chainEvent{at: clock(9, 5), key: "ACME-1", parent: "ACMEAUX-1"},
		chainEvent{at: clock(9, 6), key: "ACME-2", parent: "ACMEAUX-1", shows: true, resolved: new(2)},
		chainEvent{at: clock(9, 20), key: "ACME-1", parent: "ACMEAUX-1", shows: true, resolved: new(0)},
		chainEvent{at: clock(9, 30), key: "ACMEAUX-1", priority: new(3)},
		chainEvent{at: clock(9, 40), key: "ACME-1", parent: "ACMEAUX-1", shows: true},
	))
	want := []PriorityAt{{At: clock(9, 5), Priority: record.P2}, {At: clock(9, 20), Priority: record.P0}, {At: clock(9, 40), Priority: record.NoPriority}}
	if got := p.from("ACME-1", clock(9, 5)); !slices.Equal(got, want) {
		t.Errorf("ACME-1 resolves to %v from 09:05, want %v", got, want)
	}
	wantPriorities(t, p, clock(9, 35), map[string]record.Priority{"ACME-1": record.P0, "ACME-2": record.P2})
	wantPriorities(t, p, clock(9, 50), map[string]record.Priority{"ACME-1": record.NoPriority, "ACME-2": record.P2})
}

// A change of an ancestor's priority and a change of a parent each move the resolved priority of
// every issue below that takes its priority from the chain, at the moment of the change and not at
// the next event of the issue (spec R10: at each ancestor priority or parent change). ACME-1, with
// none, stands under ACMEAUX-1, at P2, and ACME-2, with none, under ACME-1: both P2. At 09:20 a
// person raises ACMEAUX-1 to P0: both P0 from 09:20, though neither has an event then. At 09:40
// ACME-1 moves under ACMEAUX-2, at P1: both P1 from 09:40, ACME-2 by its parent's move.
func TestAnAncestorsChangeMovesItsDescendantsAtThatMoment(t *testing.T) {
	t.Parallel()
	p := resolvePriorities(chainTrace(
		chainEvent{at: clock(8, 0), key: "ACMEAUX-1", priority: new(2)},
		chainEvent{at: clock(8, 1), key: "ACMEAUX-2", priority: new(1)},
		chainEvent{at: clock(9, 5), key: "ACME-1", parent: "ACMEAUX-1"},
		chainEvent{at: clock(9, 6), key: "ACME-2", parent: "ACME-1"},
		chainEvent{at: clock(9, 20), key: "ACMEAUX-1", priority: new(0)},
		chainEvent{at: clock(9, 40), key: "ACME-1", parent: "ACMEAUX-2"},
	))
	for key, first := range map[string]time.Time{"ACME-1": clock(9, 5), "ACME-2": clock(9, 6)} {
		want := []PriorityAt{{At: first, Priority: record.P2}, {At: clock(9, 20), Priority: record.P0}, {At: clock(9, 40), Priority: record.P1}}
		if got := p.from(key, first); !slices.Equal(got, want) {
			t.Errorf("%s resolves to %v, want %v", key, got, want)
		}
	}
}

// An ancestor whose history the window lacks is named, with the issue whose priority rests on it,
// and never guessed (spec R10: "If the record lacks a required ancestor's history, report the
// missing input"). ACME-1, with none of its own, stands under ACMEAUX-1, which the window holds no
// event of: ACMEAUX-1 is a required ancestor. ACME-2 stands under it too with P1 of its own, and
// ACME-4 with a resolved priority Dispatch recorded at its every event: neither rests on it. ACME-3
// stands under ACMEAUX-2, at P2, which stands under ACMEAUX-9, absent: the walk stops at P2 and
// needs no ACMEAUX-9. ACMEAUX-3, of another project, stands under ACMEAUX-7, absent: no issue of the
// window's project rests on it. ACME-5 stood under ACMEAUX-8, absent, only before the window's start,
// and under ACMEAUX-2 from 08:30: its priority in the window rests on no missing ancestor. So the
// priorities name ACMEAUX-1 alone, above ACME-1, and give ACME-1 no priority of ACMEAUX-1's.
func TestAMissingAncestorIsNamedAndNotGuessed(t *testing.T) {
	t.Parallel()
	p := resolvePriorities(chainTrace(
		chainEvent{at: clock(8, 0), key: "ACMEAUX-2", parent: "ACMEAUX-9", priority: new(2)},
		chainEvent{at: clock(8, 1), key: "ACMEAUX-3", parent: "ACMEAUX-7"},
		chainEvent{at: clock(8, 2), key: "ACME-5", parent: "ACMEAUX-8"},
		chainEvent{at: clock(8, 30), key: "ACME-5", parent: "ACMEAUX-2"},
		chainEvent{at: clock(9, 5), key: "ACME-1", parent: "ACMEAUX-1"},
		chainEvent{at: clock(9, 6), key: "ACME-2", parent: "ACMEAUX-1", priority: new(1)},
		chainEvent{at: clock(9, 7), key: "ACME-3", parent: "ACMEAUX-2"},
		chainEvent{at: clock(9, 8), key: "ACME-4", parent: "ACMEAUX-1", shows: true, resolved: new(0)},
	))
	if got, want := p.missingAncestors(), []string{"ACMEAUX-1"}; !slices.Equal(got, want) {
		t.Errorf("the priorities name %v missing, want %v", got, want)
	}
	if got := p.missingText(); !strings.Contains(got, "ACMEAUX-1") || !strings.Contains(got, "ACME-1") || strings.Contains(got, "ACME-2") {
		t.Errorf("the priorities name the missing input as %q, want ACMEAUX-1 above ACME-1 and no other issue", got)
	}
	wantPriorities(t, p, clock(9, 30), map[string]record.Priority{"ACME-2": record.P1, "ACME-3": record.P2, "ACME-4": record.P0, "ACME-5": record.P2})
}

// The daemon's line places an issue by record.PriorityOf, its own priority else its tree root's,
// and a replay under a rule with the new P0 start gives the daemon's mirror each issue's resolved
// priority in place of its own, so that answer is the resolved priority where an issue whose chain
// resolves to none has a tree root that resolves to none: true of every issue below a root, the
// root's chain being the top of the issue's. ACME-1, a root of a Legion tree with no priority of
// its own, stands under ACMEAUX-1, at P1, of another project; ACME-2, with none, under ACME-1;
// ACME-3, at P3 of its own, under ACME-2; and ACME-4, with none, under ACME-3. At 09:30 ACMEAUX-1's
// priority is cleared. Read at 09:20 and at 09:40 with each one's root ACME-1, PriorityOf over the
// resolved priorities answers each issue's own resolved priority: P1 for ACME-1 and ACME-2, P3 for
// ACME-3 and ACME-4, and then none for ACME-1 and ACME-2.
func TestPriorityOfOverTheResolvedPrioritiesAnswersEachIssuesOwn(t *testing.T) {
	t.Parallel()
	p := resolvePriorities(chainTrace(
		chainEvent{at: clock(8, 0), key: "ACMEAUX-1", priority: new(1)},
		chainEvent{at: clock(9, 1), key: "ACME-1", parent: "ACMEAUX-1"},
		chainEvent{at: clock(9, 2), key: "ACME-2", parent: "ACME-1"},
		chainEvent{at: clock(9, 3), key: "ACME-3", parent: "ACME-2", priority: new(3)},
		chainEvent{at: clock(9, 4), key: "ACME-4", parent: "ACME-3"},
		chainEvent{at: clock(9, 30), key: "ACMEAUX-1"},
	))
	for at, want := range map[time.Time]map[string]record.Priority{
		clock(9, 20): {"ACME-1": record.P1, "ACME-2": record.P1, "ACME-3": record.P3, "ACME-4": record.P3},
		clock(9, 40): {"ACME-1": record.NoPriority, "ACME-2": record.NoPriority, "ACME-3": record.P3, "ACME-4": record.P3},
	} {
		wantPriorities(t, p, at, want)
		root := record.Issue{Key: "ACME-1", Priority: p.at("ACME-1", at)}
		for key, resolved := range want {
			if got := record.PriorityOf(record.Issue{Key: key, Tree: "ACME-1", Priority: p.at(key, at)}, root); got != resolved {
				t.Errorf("at %s the line places %s at %q, its resolved priority over its root's, want %q", at.Format("15:04"), key, got, resolved)
			}
		}
	}
}
