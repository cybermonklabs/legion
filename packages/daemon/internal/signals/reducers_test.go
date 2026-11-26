package signals

import (
	"math"
	"slices"
	"strings"
	"testing"
	"time"
)

// A limit is legal in its signal's range and nowhere else: the idle share above 0 and at most 1, the
// provider's headroom above 0 and at most 10, the merge queue's hours above 0 and at most 168, and a
// whole number from 1 to 1,000,000 for the refusals, the kills and the approvals. The top of a range
// is legal, 0 and the number past the top are not, and no signal reads a number that is not finite.
func TestALimitIsLegalOnlyInItsSignalsRange(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		signal string
		limit  float64
		legal  bool
		why    string
	}{
		{Idle, 1, true, "the top of the idle share"},
		{Idle, math.Nextafter(1, 2), false, "just above the top"},
		{Idle, 0.5, true, "inside"},
		{Idle, math.SmallestNonzeroFloat64, true, "just above the open end"},
		{Idle, 0, false, "the open end"},
		{Idle, -0.5, false, "below it"},
		{Provider, 10, true, "the top of the headroom"},
		{Provider, 10.001, false, "just above the top"},
		{Provider, 0.01, true, "inside"},
		{Provider, 0, false, "the open end"},
		{Queue, 168, true, "a week of hours, the top"},
		{Queue, 168.5, false, "above the top"},
		{Queue, 0.25, true, "a fraction of an hour"},
		{Queue, 0, false, "the open end"},
		{Queue, -1, false, "below it"},
		{Refusals, 1, true, "the least whole number"},
		{Refusals, 1_000_000, true, "the top"},
		{Refusals, 1_000_001, false, "above the top"},
		{Refusals, 0, false, "below the least"},
		{Refusals, 0.5, false, "a fraction below the least"},
		{Refusals, 2.5, false, "not whole"},
		{Refusals, -3, false, "negative"},
		{Kills, 1, true, "the least whole number"},
		{Kills, 1_000_000, true, "the top"},
		{Kills, 1_000_001, false, "above the top"},
		{Kills, 2.5, false, "not whole"},
		{Kills, 0, false, "below the least"},
		{Approvals, 1, true, "the least whole number"},
		{Approvals, 999_999, true, "inside"},
		{Approvals, 1_000_000, true, "the top"},
		{Approvals, 1_000_000.5, false, "above the top"},
		{Approvals, 0, false, "below the least"},
		{"nothing", 1, false, "no signal a limit can name"},
		{"", 1, false, "no name"},
	} {
		err := CheckLimit(tc.signal, tc.limit)
		if (err == nil) != tc.legal {
			t.Errorf("the limit %v of %q (%s): error %v, want legal %t", tc.limit, tc.signal, tc.why, err, tc.legal)
		}
		if err != nil && !strings.HasPrefix(err.Error(), "signals: ") {
			t.Errorf("the limit %v of %q is refused with %q, want a message that names the package", tc.limit, tc.signal, err)
		}
	}
	for _, name := range Names() {
		for _, limit := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
			if err := CheckLimit(name, limit); err == nil {
				t.Errorf("the limit %v of %q is legal, and no signal reads a number that is not finite", limit, name)
			}
		}
	}
}

// waitsAre says whether got and want are the same waits, each by its two moments.
func waitsAre(got, want []Wait) bool {
	return slices.EqualFunc(got, want, func(a, b Wait) bool { return a.Added.Equal(b.Added) && a.Left.Equal(b.Left) })
}

// The merge queue pairs each pull request's entries and exits in the order of their moments: an
// entry of a pull request standing in the queue and an exit of one that is not change nothing, a
// drop and a re-add of one pull request are two waits, an event id counts once, two repositories
// give one number two pull requests, and the waits come out by the moment each began. Where the
// events are given in time order, applying them one at a time makes the same waits.
func TestTheMergeQueuePairsEntriesAndExitsAsTheRecordedWaitsAre(t *testing.T) {
	t.Parallel()
	enter := func(id int64, repo string, pull int, at time.Time) QueueEvent {
		return QueueEvent{ID: id, Repo: repo, PullRequest: pull, At: at, Enters: true}
	}
	leave := func(id int64, repo string, pull int, at time.Time) QueueEvent {
		return QueueEvent{ID: id, Repo: repo, PullRequest: pull, At: at}
	}
	const a, b = "acme/widgets", "acme/gadgets"
	for _, tc := range []struct {
		name      string
		events    []QueueEvent
		want      []Wait
		unordered bool
	}{
		{"an entry and its exit", []QueueEvent{enter(1, a, 1, clock(9, 0)), leave(2, a, 1, clock(10, 0))},
			[]Wait{{Added: clock(9, 0), Left: clock(10, 0)}}, false},
		{"an entry standing at the end of the record", []QueueEvent{enter(1, a, 1, clock(9, 0))},
			[]Wait{{Added: clock(9, 0)}}, false},
		{"a second entry while the pull request stands is no wait", []QueueEvent{enter(1, a, 1, clock(9, 0)), enter(2, a, 1, clock(9, 30)), leave(3, a, 1, clock(10, 0))},
			[]Wait{{Added: clock(9, 0), Left: clock(10, 0)}}, false},
		{"an exit with no entry stands for nothing", []QueueEvent{leave(1, a, 1, clock(8, 0)), enter(2, a, 1, clock(9, 0))},
			[]Wait{{Added: clock(9, 0)}}, false},
		{"the removal recorded beside a merge ends nothing", []QueueEvent{enter(1, a, 1, clock(9, 0)), leave(2, a, 1, clock(10, 0)), leave(3, a, 1, clock(10, 0))},
			[]Wait{{Added: clock(9, 0), Left: clock(10, 0)}}, false},
		{"a drop and a re-add are two waits", []QueueEvent{enter(1, a, 1, clock(9, 0)), leave(2, a, 1, clock(9, 30)), enter(3, a, 1, clock(10, 0)), leave(4, a, 1, clock(11, 0))},
			[]Wait{{Added: clock(9, 0), Left: clock(9, 30)}, {Added: clock(10, 0), Left: clock(11, 0)}}, false},
		{"an event id counts once", []QueueEvent{enter(1, a, 1, clock(9, 0)), leave(2, a, 1, clock(10, 0)), enter(1, a, 1, clock(9, 0)), leave(2, a, 1, clock(10, 0))},
			[]Wait{{Added: clock(9, 0), Left: clock(10, 0)}}, false},
		{"an event id seen again names the first event, not the later one", []QueueEvent{enter(1, a, 1, clock(9, 0)), leave(2, a, 1, clock(10, 0)), enter(1, a, 1, clock(11, 0))},
			[]Wait{{Added: clock(9, 0), Left: clock(10, 0)}}, false},
		{"an id of 0 names no event and is never a duplicate", []QueueEvent{enter(0, a, 1, clock(9, 0)), leave(0, a, 1, clock(10, 0)), enter(0, a, 1, clock(11, 0))},
			[]Wait{{Added: clock(9, 0), Left: clock(10, 0)}, {Added: clock(11, 0)}}, false},
		{"one number in two repositories is two pull requests", []QueueEvent{enter(1, a, 1, clock(9, 0)), enter(2, b, 1, clock(9, 30)), leave(3, a, 1, clock(10, 0)), leave(4, b, 1, clock(11, 0))},
			[]Wait{{Added: clock(9, 0), Left: clock(10, 0)}, {Added: clock(9, 30), Left: clock(11, 0)}}, false},
		{"the waits come out by the moment each began", []QueueEvent{enter(1, a, 1, clock(10, 0)), leave(2, a, 1, clock(11, 0)), enter(3, a, 2, clock(8, 0)), leave(4, a, 2, clock(9, 0))},
			[]Wait{{Added: clock(8, 0), Left: clock(9, 0)}, {Added: clock(10, 0), Left: clock(11, 0)}}, false},
		{"a history given out of order is read in the order of its moments", []QueueEvent{leave(2, a, 1, clock(10, 0)), enter(1, a, 1, clock(9, 0))},
			[]Wait{{Added: clock(9, 0), Left: clock(10, 0)}}, true},
	} {
		if got := WaitsOf(tc.events); !waitsAre(got, tc.want) {
			t.Errorf("%s: the waits are %+v, want %+v", tc.name, got, tc.want)
		}
		if tc.unordered {
			continue
		}
		var q MergeQueue
		for _, e := range tc.events {
			q.Apply(e)
		}
		if got := slices.Clone(q.Waits()); !waitsAre(slices.SortedStableFunc(slices.Values(got), func(x, y Wait) int { return x.Added.Compare(y.Added) }), tc.want) {
			t.Errorf("%s: applying the events one at a time makes the waits %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// An event changes the merge queue only where it makes or ends a wait: a duplicate id, an entry of a
// pull request that stands and an exit of one that does not change nothing.
func TestApplyingAQueueEventSaysWhetherItChangedTheQueue(t *testing.T) {
	t.Parallel()
	const repo = "acme/widgets"
	var q MergeQueue
	for _, step := range []struct {
		name    string
		event   QueueEvent
		changed bool
	}{
		{"the first entry", QueueEvent{ID: 1, Repo: repo, PullRequest: 1, At: clock(9, 0), Enters: true}, true},
		{"the same event again", QueueEvent{ID: 1, Repo: repo, PullRequest: 1, At: clock(9, 0), Enters: true}, false},
		{"another entry of the pull request that stands", QueueEvent{ID: 2, Repo: repo, PullRequest: 1, At: clock(9, 10), Enters: true}, false},
		{"an exit of another pull request that is not in the queue", QueueEvent{ID: 3, Repo: repo, PullRequest: 2, At: clock(9, 20)}, false},
		{"the exit", QueueEvent{ID: 4, Repo: repo, PullRequest: 1, At: clock(10, 0)}, true},
		{"the removal beside the merge", QueueEvent{ID: 5, Repo: repo, PullRequest: 1, At: clock(10, 0)}, false},
		{"the re-entry", QueueEvent{ID: 6, Repo: repo, PullRequest: 1, At: clock(11, 0), Enters: true}, true},
	} {
		if got := q.Apply(step.event); got != step.changed {
			t.Errorf("%s: Apply says it changed the queue: %t, want %t", step.name, got, step.changed)
		}
	}
	want := []Wait{{Added: clock(9, 0), Left: clock(10, 0)}, {Added: clock(11, 0)}}
	if got := q.Waits(); !waitsAre(got, want) {
		t.Errorf("the waits are %+v, want %+v", got, want)
	}
}

// NextAsk turns one ask by its events as Dispatch's contract and the daemon's reading do: an ask
// opens waiting on a person, a payload state of open reopens one waiting on a person, answered and
// resolved close it, an edit that moves an approval request to a newer version leaves it with its
// agent while any other edit does not, handed_back and an agent's comment leave it with the person
// or the agent, asked_of is kept where an event names none and cleared by null, and a comment on no
// known ask, or one that names neither, changes nothing.
func TestAnAskTurnsWithEachEvent(t *testing.T) {
	t.Parallel()
	const ask, issue = "ask-1", "ACME-1"
	opened := AskEvent{Kind: AskOpened, Ask: ask, Issue: issue}
	withAskedOf := func(e AskEvent, person string) AskEvent {
		e.AskedOfSet, e.AskedOf = true, person
		return e
	}
	comment := func(waiting AskWaiting) AskEvent { return AskEvent{Kind: AskCommented, Ask: ask, WaitingOn: waiting} }
	edited := func(approval *AskApproval) AskEvent { return AskEvent{Kind: AskEdited, Ask: ask, Approval: approval} }
	moved := &AskApproval{Version: new(3), RequestedVersion: new(2)}
	for _, tc := range []struct {
		name   string
		events []AskEvent
		want   Ask
	}{
		{"opened waits on a person", []AskEvent{opened}, Ask{ID: ask, Issue: issue, Open: true, Human: true}},
		{"an event that names asked_of sets the person", []AskEvent{withAskedOf(opened, "alice")}, Ask{ID: ask, Issue: issue, Open: true, Human: true, AskedOf: "alice"}},
		{"an event that names none keeps the person", []AskEvent{withAskedOf(opened, "alice"), edited(nil)},
			Ask{ID: ask, Issue: issue, Open: true, Human: true, AskedOf: "alice"}},
		{"asked_of null clears the person", []AskEvent{withAskedOf(opened, "alice"), withAskedOf(AskEvent{Kind: AskAskedOf, Ask: ask}, "")},
			Ask{ID: ask, Issue: issue, Open: true, Human: true}},
		{"answered closes it", []AskEvent{opened, {Kind: AskAnswered, Ask: ask}}, Ask{ID: ask, Issue: issue, Human: true}},
		{"resolved closes it", []AskEvent{opened, {Kind: AskResolved, Ask: ask}}, Ask{ID: ask, Issue: issue, Human: true}},
		{"a payload state other than open closes it", []AskEvent{opened, {Kind: AskEdited, Ask: ask, State: ReportsClosed}}, Ask{ID: ask, Issue: issue, Human: true}},
		{"a payload state of open reopens a closed ask waiting on a person", []AskEvent{opened, comment(WaitsOnAgent), {Kind: AskAnswered, Ask: ask}, {Kind: AskAskedOf, Ask: ask, State: ReportsOpen}},
			Ask{ID: ask, Issue: issue, Open: true, Human: true}},
		{"a payload state of open leaves an open ask with its agent", []AskEvent{opened, comment(WaitsOnAgent), {Kind: AskEdited, Ask: ask, State: ReportsOpen}},
			Ask{ID: ask, Issue: issue, Open: true}},
		{"an approval edited to a newer version waits on its agent", []AskEvent{opened, edited(moved)}, Ask{ID: ask, Issue: issue, Open: true}},
		{"an approval edited at the same version keeps its turn", []AskEvent{opened, edited(&AskApproval{Version: new(3), RequestedVersion: new(3)})},
			Ask{ID: ask, Issue: issue, Open: true, Human: true}},
		{"an approval edited with no requested version keeps its turn", []AskEvent{opened, edited(&AskApproval{Version: new(3)})},
			Ask{ID: ask, Issue: issue, Open: true, Human: true}},
		{"an edit of an ask that is no approval keeps its turn", []AskEvent{opened, edited(nil)}, Ask{ID: ask, Issue: issue, Open: true, Human: true}},
		{"an agent's hand-back returns it to the person", []AskEvent{opened, edited(moved), {Kind: AskHandedBack, Ask: ask}},
			Ask{ID: ask, Issue: issue, Open: true, Human: true}},
		{"a comment that waits on the agent", []AskEvent{opened, comment(WaitsOnAgent)}, Ask{ID: ask, Issue: issue, Open: true}},
		{"a comment that waits on the person", []AskEvent{opened, comment(WaitsOnAgent), comment(WaitsOnHuman)}, Ask{ID: ask, Issue: issue, Open: true, Human: true}},
		{"a comment keeps the person it is asked of", []AskEvent{withAskedOf(opened, "alice"), comment(WaitsOnAgent)}, Ask{ID: ask, Issue: issue, Open: true, AskedOf: "alice"}},
		{"a comment on a closed ask leaves it closed", []AskEvent{opened, {Kind: AskAnswered, Ask: ask}, comment(WaitsOnAgent)}, Ask{ID: ask, Issue: issue}},
		{"a comment that names neither changes nothing", []AskEvent{opened, comment("somebody")}, Ask{ID: ask, Issue: issue, Open: true, Human: true}},
	} {
		var got Ask
		known := false
		for _, e := range tc.events {
			if next, changed := NextAsk(got, known, e); changed {
				got, known = next, true
			}
		}
		if got != tc.want {
			t.Errorf("%s: the ask is %+v, want %+v", tc.name, got, tc.want)
		}
	}
	if got, changed := NextAsk(Ask{}, false, comment(WaitsOnHuman)); changed || got != (Ask{}) {
		t.Errorf("a comment on an ask no event made gives %+v, %t, want it changed not", got, changed)
	}
	if !(Ask{Open: true, Human: true}).Waits() || (Ask{Open: true}).Waits() || (Ask{Human: true}).Waits() {
		t.Error("an ask waits on a person only while it is open and its turn is the person's")
	}
}

// The approvals state counts the asks open whose turn is a person's and whom they wait on is the
// project's default assignee: an ask waits on its asked_of person, else its issue's assignee, else
// the default assignee, and each event is applied as it comes: an issue's assignee, the project's
// default assignee, and an ask's own events move who is counted.
func TestTheApprovalStateCountsTheAsksThatWaitOnTheDefaultAssignee(t *testing.T) {
	t.Parallel()
	var s ApprovalState
	if got := s.Waiting(); len(got) != 0 {
		t.Fatalf("an empty state counts %v, want none", got)
	}
	open := func(ask, issue, askedOf string) {
		e := AskEvent{Kind: AskOpened, Ask: ask, Issue: issue}
		e.AskedOfSet, e.AskedOf = askedOf != "", askedOf
		if !s.ApplyAsk(e) {
			t.Fatalf("opening %s changed nothing", ask)
		}
	}
	open("a1", "ACME-1", "")
	open("a2", "ACME-2", "bob")
	open("a3", "ACME-3", "owner")
	check := func(step string, want ...string) {
		t.Helper()
		if got := s.Waiting(); !slices.Equal(got, want) {
			t.Errorf("%s: the asks waiting on the default assignee are %v, want %v", step, got, want)
		}
	}
	check("with no default assignee: nobody", []string(nil)...)
	s.SetDefaultAssignee("owner")
	check("a1 has no assignee, so the default's, and a3 is asked of the default", "a1", "a3")
	s.Assign("ACME-1", "dave")
	check("an assignee takes a1 off", "a3")
	s.Assign("ACME-1", "")
	check("an assignee cleared puts a1 back on the default", "a1", "a3")
	s.SetDefaultAssignee("bob")
	check("a new default assignee: a1 has none of its own, and a2 is asked of it", "a1", "a2")
	if !s.ApplyAsk(AskEvent{Kind: AskCommented, Ask: "a2", WaitingOn: WaitsOnAgent}) {
		t.Error("a comment on a2 changed nothing")
	}
	check("a2 waits on its agent", "a1")
	s.ApplyAsk(AskEvent{Kind: AskHandedBack, Ask: "a2"})
	check("a2 handed back", "a1", "a2")
	s.ApplyAsk(AskEvent{Kind: AskAnswered, Ask: "a2"})
	check("a2 answered", "a1")
	if s.ApplyAsk(AskEvent{Kind: AskCommented, Ask: "a9", WaitingOn: WaitsOnHuman}) {
		t.Error("a comment on an ask no event made changed the state")
	}
}
