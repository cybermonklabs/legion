package record

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// linePartIssue is one record of this test's line, with the fields the line reads and nothing
// else. It is internal/admit's lineIssue (line_order_test.go) and internal/projection's written
// for this package, so the three fixtures are read side by side.
func linePartIssue(key, tree, status string, stands phase.Phase, rank string) Issue {
	return Issue{Key: key, Tree: tree, Project: "LEGION", Title: key, Phase: stands, Generation: 1,
		Status: status, DispatchStatus: status, Rank: rank, HandedOver: true,
		Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
}

// Every entry of the waiting line says which part it stands in, why it stands there, where it
// stands within that part and whether it already holds a slot, and each part's entries carry
// what placed them there: a member of the returned part the moment its work came back and no
// rank, a member of the starting part its tree root's Dispatch rank and no moment at all. The
// issue's own rank and its tree root's key are no fields of the entry, since the entry's issue
// holds both (LineEntry.RootRank), and the part is carried and not derived from the kind, which
// is what the two first_phase entries here show: the tree's own root stands in the returned
// part and its child in the starting one.
//
// The line holds one issue of every kind — an owed start, an answered root and a tree root's own
// first phase in the returned part, a queued root and a child's first phase in the starting one
// — and the words each entry gives for the wire (LineKind.String, LinePart.String) are read
// beside the values, because those words are what a reader of the state branches on.
func TestWaitingLinePartAndKeyOnEveryEntry(t *testing.T) {
	base := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	opened, owedAt, answeredAt := base.Add(-3*time.Hour), base.Add(-2*time.Hour), base.Add(-time.Hour)
	approved := 1
	answered := linePartIssue("LEGION-40", "LEGION-40", "in_progress", phase.AwaitingPerson, "D")
	answered.Awaiting = &Awaiting{Ask: "ask-1", Since: EventMark{Seq: 9},
		AnsweredAt: &answeredAt, AnsweredBy: AnsweredByReply}
	issues := []Issue{
		// A live tree whose gate is open: its root's own first phase is still to come, one
		// released child waits for its first, and a third member owes its merger a start.
		linePartIssue("LEGION-10", "LEGION-10", "in_progress", phase.Admitted, "M"),
		linePartIssue("LEGION-11", "LEGION-10", "todo", phase.Admitted, "N"),
		linePartIssue("LEGION-12", "LEGION-10", "retro", phase.Merging, "O"),
		// A root that never ran, ranked above that tree's root.
		linePartIssue("LEGION-20", "LEGION-20", "todo", phase.Admitted, "A"),
		answered,
	}
	facts := LineFacts{
		Lifecycles: map[string]treelifecycle.Lifecycle{"LEGION-10": {Tree: "LEGION-10", Authority: treelifecycle.AuthorityWorkflow}},
		Gates: map[string]TreeGate{"LEGION-10": {Rows: map[DesignStep]DesignGate{DesignStepSpec: {
			Issue: "LEGION-10", Step: DesignStepSpec, LatestVersion: 1, ApprovedVersion: &approved,
			ApprovedMark: EventMark{Seq: 7, At: opened}, EverOpened: true}}}},
		Owed: map[string]OwedStart{"LEGION-12": {Issue: "LEGION-12", ID: 42, CreatedAt: owedAt}},
	}
	// The root whose gate opened and the answered root each kept the row they hold through the
	// wait (KeepsSlot); the other three hold none.
	slots := []Slot{{Issue: "LEGION-10", Index: 0, AdmittedAt: opened}, {Issue: "LEGION-40", Index: 1, AdmittedAt: answeredAt}}

	want := []LineEntry{
		{Issue: issues[0], Kind: LineFirstPhase, Part: PartReturned, PartPlace: 1, Since: opened, Holds: true},
		{Issue: issues[2], Kind: LineReturning, Part: PartReturned, PartPlace: 2, Since: owedAt},
		{Issue: answered, Kind: LineAnswered, Part: PartReturned, PartPlace: 3, Since: answeredAt, Holds: true},
		{Issue: issues[3], Kind: LineQueuedRoot, Part: PartStarting, PartPlace: 1, RootRank: "A"},
		{Issue: issues[1], Kind: LineFirstPhase, Part: PartStarting, PartPlace: 2, RootRank: "M"},
	}
	words := []struct{ part, kind string }{
		{"returned", "first_phase"}, {"returned", "returning"}, {"returned", "answered"},
		{"starting", "queued_root"}, {"starting", "first_phase"},
	}

	line := Waiting(issues, slots, facts)
	if len(line) != len(want) {
		var got []string
		for _, entry := range line {
			got = append(got, entry.Issue.Key)
		}
		t.Fatalf("the line = %v, want the five members %v", got, []string{"LEGION-10", "LEGION-12", "LEGION-40", "LEGION-20", "LEGION-11"})
	}
	for i, entry := range line {
		expected := want[i]
		if entry.Issue.Key != expected.Issue.Key {
			t.Fatalf("the line's %d member is %s, want %s: the returned part oldest first, then the board's rank",
				i+1, entry.Issue.Key, expected.Issue.Key)
		}
		if entry.Part != expected.Part || entry.Kind != expected.Kind ||
			entry.PartPlace != expected.PartPlace || entry.Holds != expected.Holds {
			t.Errorf("%s stands as part %v kind %v, place %d of its part, holding %v; want part %v kind %v, place %d, holding %v",
				entry.Issue.Key, entry.Part, entry.Kind, entry.PartPlace, entry.Holds,
				expected.Part, expected.Kind, expected.PartPlace, expected.Holds)
		}
		if word := words[i]; entry.Part.String() != word.part || entry.Kind.String() != word.kind {
			t.Errorf("%s gives the wire %q and %q, want %q and %q", entry.Issue.Key,
				entry.Part.String(), entry.Kind.String(), word.part, word.kind)
		}
		if !entry.Since.Equal(expected.Since) {
			t.Errorf("%s came back at %v, want %v: the moment that placed it in its part",
				entry.Issue.Key, entry.Since, expected.Since)
		}
		if entry.RootRank != expected.RootRank {
			t.Errorf("%s carries root rank %q, want %q: the rank of the tree root that placed its part",
				entry.Issue.Key, entry.RootRank, expected.RootRank)
		}
	}
}
