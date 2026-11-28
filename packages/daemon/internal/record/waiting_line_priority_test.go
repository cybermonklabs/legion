package record

import (
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// openGate is a tree's design gate approved at version 1 at the moment opened, as a gate whose
// spec a person approved reads.
func openGate(root string, opened time.Time) TreeGate {
	approved := 1
	return TreeGate{Rows: map[DesignStep]DesignGate{DesignStepSpec: {Issue: root, Step: DesignStepSpec, LatestVersion: 1,
		ApprovedVersion: &approved, ApprovedMark: EventMark{Seq: 7, At: opened}, EverOpened: true}}}
}

// The acceptance case: a line holding a P0 child of a P1 tree, a P1 root whose work came back and
// a P2 root ranked first on the board puts the P0 child first, the P1 root second and the P2 root
// last. Priority is the first key of the whole line, and the line's own order holds only within
// one priority.
//
// Before priority, the line put the P1 root first, since its work came back, and the P2 root
// ahead of the P0 child, since the P2 root ranks first on the board and the child takes its tree
// root's place.
func TestTheWaitingLinePutsAP0ChildOfAP1TreeAheadOfAP1RootWhoseWorkCameBackAndAP2RootRankedFirst(t *testing.T) {
	base := time.Date(2026, 11, 24, 6, 0, 0, 0, time.UTC)
	opened, owedAt := base.Add(-3*time.Hour), base.Add(-time.Hour)
	// A running P1 tree, its gate open, its root holding the row of its run and waiting on its
	// open child (the children-first wait), so the root itself stands in no line; and its
	// released P0 child, waiting for its first phase.
	treeRoot := withPriority(linePartIssue("LEGION-10", "LEGION-10", "in_progress", phase.Admitted, "M"), P1)
	child := withPriority(linePartIssue("LEGION-11", "LEGION-10", "todo", phase.Admitted, "N"), P0)
	parent := treeRoot.Key
	child.Parent = &parent
	// A P1 root of a tree of its own whose work came back: its implementer's start waits for a
	// slot, which it gave back.
	returned := withPriority(linePartIssue("LEGION-20", "LEGION-20", "in_progress", phase.Implementing, "B"), P1)
	// A P2 root that never ran, ranked first on the board.
	first := withPriority(linePartIssue("LEGION-30", "LEGION-30", "todo", phase.Admitted, "A"), P2)
	issues := []Issue{treeRoot, child, returned, first}
	facts := LineFacts{
		Chains:     chainsOf(issues, copyOf(issues...)),
		Gates:      map[string]TreeGate{"LEGION-10": openGate("LEGION-10", opened), "LEGION-20": openGate("LEGION-20", opened)},
		Owed:       map[string]OwedStart{"LEGION-20": {Issue: "LEGION-20", ID: 42, CreatedAt: owedAt}},
		ChildWaits: map[string]ChildWait{"LEGION-10": {Parent: "LEGION-10", Open: []OpenChild{{Key: "LEGION-11", Status: "todo", Released: true}}}},
	}
	slots := []Slot{{Issue: "LEGION-10", Index: 0, AdmittedAt: opened}}

	line, _ := Waiting(issues, slots, facts)
	if got, want := keysOf(WaitingIssues(line)), []string{"LEGION-11", "LEGION-20", "LEGION-30"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the line = %v, want %v: the P0 child first, then the P1 root whose work came back, then the P2 root ranked first on the board", got, want)
	}
	// Each entry keeps the part and kind the line gives it today, says the priority that placed
	// it, and counts its place within its own band and part.
	want := []struct {
		priority Priority
		part     LinePart
		kind     LineKind
		place    int
	}{
		{P0, PartStarting, LineFirstPhase, 1},
		{P1, PartReturned, LineReturning, 1},
		{P2, PartStarting, LineQueuedRoot, 1},
	}
	for i, entry := range line {
		if w := want[i]; entry.Priority != w.priority || entry.Part != w.part || entry.Kind != w.kind || entry.PartPlace != w.place {
			t.Errorf("%s stands at %v in part %v as %v, place %d; want %v in part %v as %v, place %d",
				entry.Issue.Key, entry.Priority, entry.Part, entry.Kind, entry.PartPlace, w.priority, w.part, w.kind, w.place)
		}
	}
}

// A first start with no priority takes the top issue's priority, not the priority of a nearer
// issue that holds its own gate. With neither priority, it waits after P3.
func TestTheWaitingLinePlacesAnIssueWithNoPriorityAtItsTopIssuesBand(t *testing.T) {
	opened := time.Date(2026, 11, 24, 3, 0, 0, 0, time.UTC)
	top := withPriority(linePartIssue("LEGION-40", "LEGION-40", "in_progress", phase.Implementing, "M"), P0)
	holder := withPriority(linePartIssue("LEGION-42", "LEGION-40", "in_progress", phase.Implementing, "A"), P3)
	holder.HoldsGate = true
	child := linePartIssue("LEGION-41", "LEGION-42", "todo", phase.Admitted, "N")
	issues := []Issue{
		top, holder, child,
		// Roots that never ran: two P1 roots ranked low, a P3 root and a root with no priority
		// ranked first on the board.
		withPriority(linePartIssue("LEGION-50", "LEGION-50", "todo", phase.Admitted, "Z"), P1),
		withPriority(linePartIssue("LEGION-80", "LEGION-80", "todo", phase.Admitted, "Y"), P1),
		withPriority(linePartIssue("LEGION-70", "LEGION-70", "todo", phase.Admitted, "B"), P3),
		linePartIssue("LEGION-60", "LEGION-60", "todo", phase.Admitted, "A"),
	}
	facts := LineFacts{
		Chains: chainsOf(issues, copyOf(issues...)),
		Gates:  map[string]TreeGate{"LEGION-40": openGate("LEGION-40", opened), "LEGION-42": openGate("LEGION-42", opened)},
	}
	slots := []Slot{{Issue: "LEGION-40", Index: 0, AdmittedAt: opened}, {Issue: "LEGION-42", Index: 1, AdmittedAt: opened}}

	line, _ := Waiting(issues, slots, facts)
	want := []string{"LEGION-41", "LEGION-80", "LEGION-50", "LEGION-70", "LEGION-60"}
	if got := keysOf(WaitingIssues(line)); !reflect.DeepEqual(got, want) {
		t.Fatalf("the line = %v, want %v: the child at its top issue's P0, the two P1 roots by rank, the P3 root, and the root with none last", got, want)
	}
	// The child is placed at its root's P0 and its own priority stays none: the inherited
	// priority orders the line and nothing else. The two P1 roots count 1st and 2nd of their band
	// and part.
	for _, entry := range line {
		switch entry.Issue.Key {
		case "LEGION-41":
			if entry.Priority != P0 || entry.RootKey != top.Key || entry.RootRank != top.Rank || entry.Issue.Priority != NoPriority {
				t.Errorf("LEGION-41 = %+v, want P0 and board rank from %s, and no priority of its own", entry, top.Key)
			}
		case "LEGION-80", "LEGION-50":
			if wantPlace := map[string]int{"LEGION-80": 1, "LEGION-50": 2}[entry.Issue.Key]; entry.PartPlace != wantPlace {
				t.Errorf("%s is place %d of its band and part, want %d", entry.Issue.Key, entry.PartPlace, wantPlace)
			}
		case "LEGION-60":
			if entry.Priority != NoPriority || entry.PartPlace != 1 {
				t.Errorf("LEGION-60 stands at %v, place %d, want no priority, place 1 of its band", entry.Priority, entry.PartPlace)
			}
		}
	}
}
