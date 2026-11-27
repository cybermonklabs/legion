package record

import (
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// Issue.StartsNow and HoldersOf are the two functions the replay of cybermonklabs/legion#4771
// calls in place of copies of its own (dispatch://INFRA-5636/comment/52b47ca7-ed69-48e0-8256-aeaed2957444):
// whether an issue starts outside the admission cap, and the count of one project's slot holders as
// the cap and the P0 bound count them. Their names and signatures are the seam, so this test holds
// each through a value of its exact type, which stops compiling when either changes, and pins the
// input contract the replay relies on: HoldersOf counts a slot whose issue is among the records it is
// given and no other (decision 7 of the plan).
func TestStartsNowAndHoldersOfAreTheReplaysSeam(t *testing.T) {
	var (
		startsNow func(Issue) bool                  = Issue.StartsNow
		holdersOf func([]Issue, []Slot) SlotHolders = HoldersOf
		add       func(*SlotHolders, Issue)         = (*SlotHolders).Add
		resolved                                    = func(p Priority, from string) Resolved { return Resolved{Priority: p, From: from, Known: true} }
	)
	rows := []struct {
		name   string
		own    Priority
		got    Resolved
		starts bool
	}{
		{name: "own P0 with no resolved field", own: P0, starts: true},
		{name: "inherited P0 with own null", got: resolved(P0, "OTHER-5"), starts: true},
		{name: "known null", got: Resolved{Known: true}},
		{name: "resolved P1 with own none", got: resolved(P1, "OTHER-5")},
		{name: "resolved P2 with own none", got: resolved(P2, "OTHER-5")},
		{name: "resolved P3 with own none", got: resolved(P3, "OTHER-5")},
		{name: "no priority and no resolved field"},
	}
	var issues []Issue
	var slots []Slot
	var want SlotHolders
	for i, row := range rows {
		issue := linePartIssue("LEGION-"+string(rune('A'+i)), "LEGION-"+string(rune('A'+i)), "in_progress", phase.Implementing, "M")
		issue.Priority, issue.Resolved = row.own, row.got
		if got := startsNow(issue); got != row.starts {
			t.Errorf("%s: StartsNow = %t, want %t", row.name, got, row.starts)
		}
		// Add counts one holder as HoldersOf counts the same holder alone.
		var one SlotHolders
		add(&one, issue)
		if alone := holdersOf([]Issue{issue}, []Slot{{Issue: issue.Key}}); one != alone {
			t.Errorf("%s: Add counts %+v, HoldersOf of the issue and its slot alone counts %+v", row.name, one, alone)
		}
		issues = append(issues, issue)
		slots = append(slots, Slot{Issue: issue.Key, Index: i})
		if row.starts {
			want.P0++
		} else {
			want.Capped++
		}
	}
	// An issue among the records that holds no slot is no holder, whatever its priority.
	waiting := withPriority(linePartIssue("LEGION-W", "LEGION-W", "todo", phase.Admitted, "N"), P0)
	issues = append(issues, waiting)
	// A slot whose issue the records do not hold, and a slot of another project's issue whose
	// record is that project's, are counted nowhere: the replay's caller owns an incomplete
	// snapshot, and a project counts its own holders alone.
	other := linePartIssue("OTHER-3", "OTHER-3", "in_progress", phase.Implementing, "M")
	other.Project, other.Priority = "OTHER", P0
	slots = append(slots, Slot{Issue: "LEGION-99", Index: len(slots)}, Slot{Issue: other.Key, Index: len(slots) + 1})

	got := holdersOf(issues, slots)
	if got != want {
		t.Fatalf("HoldersOf counts %+v, want %+v: one P0 holder for each row that starts now and one capped holder for each other row", got, want)
	}
	if got.Total() != len(rows) {
		t.Fatalf("Total = %d, want %d, the slots of the project's own records", got.Total(), len(rows))
	}
	var added SlotHolders
	for _, issue := range issues[:len(rows)] {
		add(&added, issue)
	}
	if added != got {
		t.Fatalf("Add over every holder counts %+v, HoldersOf counts %+v", added, got)
	}
	if theirs := holdersOf([]Issue{other}, slots); theirs != (SlotHolders{P0: 1}) {
		t.Fatalf("HoldersOf over the other project's records counts %+v, want its one P0 holder", theirs)
	}
}

// PriorityOf places an entry in the waiting line's band by the priority Dispatch resolves where
// Dispatch sent one (C2 of dispatch://INFRA-5636), whatever its tree root's: a known resolved
// priority beats the root's, and a known null under a P0 root bands as none. Where Dispatch sent
// none, the issue's own priority places it, and with none of its own its tree root's does, which is
// an older Dispatch's order alone (decision 3 of the plan): it never makes the issue start outside
// the cap. The line built from such issues stands in those bands.
func TestPriorityOfBandsByTheResolvedPriorityWhereKnown(t *testing.T) {
	opened := time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)
	p0Root := withPriority(linePartIssue("LEGION-40", "LEGION-40", "in_progress", phase.Admitted, "M"), P0)
	member := func(key, rank string, own Priority, resolved Resolved) Issue {
		issue := withPriority(linePartIssue(key, p0Root.Key, "todo", phase.Admitted, rank), own)
		issue.Parent, issue.Resolved = &p0Root.Key, resolved
		return issue
	}
	rows := []struct {
		name   string
		issue  Issue
		root   Issue
		want   Priority
		starts bool
	}{
		{name: "a known resolved P2 beats a P0 tree root",
			issue: member("LEGION-41", "N1", NoPriority, Resolved{Priority: P2, From: "OTHER-5", Known: true}), root: p0Root, want: P2},
		{name: "a known resolved P0 under a P3 tree root",
			issue: member("LEGION-42", "N2", NoPriority, Resolved{Priority: P0, From: "OTHER-5", Known: true}),
			root:  withPriority(p0Root, P3), want: P0, starts: true},
		{name: "a known null under a P0 tree root bands as none",
			issue: member("LEGION-43", "N3", NoPriority, Resolved{Known: true}), root: p0Root, want: NoPriority},
		{name: "unknown with its own P1 gives its own under a P0 tree root",
			issue: member("LEGION-44", "N4", P1, Resolved{}), root: p0Root, want: P1},
		{name: "unknown with no own priority gives the P0 tree root's and never starts",
			issue: member("LEGION-45", "N5", NoPriority, Resolved{}), root: p0Root, want: P0},
		{name: "unknown with no own priority under a root with none bands as none",
			issue: member("LEGION-46", "N6", NoPriority, Resolved{}), root: withPriority(p0Root, NoPriority), want: NoPriority},
	}
	for _, row := range rows {
		if got := PriorityOf(row.issue, row.root); got != row.want {
			t.Errorf("%s: PriorityOf = %q, want %q", row.name, got, row.want)
		}
		if got := row.issue.StartsNow(); got != row.starts {
			t.Errorf("%s: StartsNow = %t, want %t", row.name, got, row.starts)
		}
	}

	// The members under the P0 root, in one running tree whose root waits on them, stand in the
	// line by those bands: the older Dispatch's member at the root's P0, then the own P1, the
	// resolved P2, and the known null after every band.
	issues := []Issue{p0Root}
	open := make([]OpenChild, 0, len(rows))
	for _, row := range rows {
		if row.root.Priority != p0Root.Priority {
			continue
		}
		issues = append(issues, row.issue)
		open = append(open, OpenChild{Key: row.issue.Key, Status: "todo", Released: true})
	}
	facts := LineFacts{
		Lifecycles: map[string]treelifecycle.Lifecycle{p0Root.Key: {Tree: p0Root.Key, Authority: treelifecycle.AuthorityWorkflow}},
		Gates:      map[string]TreeGate{p0Root.Key: openGate(p0Root.Key, opened)},
		ChildWaits: map[string]ChildWait{p0Root.Key: {Parent: p0Root.Key, Open: open}},
	}
	line, _ := Waiting(issues, []Slot{{Issue: p0Root.Key, AdmittedAt: opened}}, facts)
	if got, want := keysOf(WaitingIssues(line)), []string{"LEGION-45", "LEGION-44", "LEGION-41", "LEGION-43"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the line = %v, want %v: the older Dispatch's member at its root's P0, its own P1, the resolved P2 and the known null last", got, want)
	}
	for _, entry := range line {
		if want := PriorityOf(entry.Issue, p0Root); entry.Priority != want {
			t.Errorf("%s stands in the band of %q, want %q (PriorityOf)", entry.Issue.Key, entry.Priority, want)
		}
	}
}
