package record

import (
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// planHoldFacts is the facts of one pass over trees whose design gates stand open and whose
// lifecycles the workflow holds, so that nothing of a tree's own state answers before the hold
// of the issue a case reads. parents is the records above the live ones that the pass's live
// read leaves out (LineFacts.Parents), which the chain walk crosses.
func planHoldFacts(trees []string, parents ...Issue) LineFacts {
	approved := 1
	opened := time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC)
	facts := LineFacts{Lifecycles: map[string]treelifecycle.Lifecycle{}, Gates: map[string]TreeGate{}, Parents: map[string]Issue{}}
	for _, tree := range trees {
		facts.Lifecycles[tree] = treelifecycle.Lifecycle{Tree: tree, Authority: treelifecycle.AuthorityWorkflow}
		facts.Gates[tree] = TreeGate{Rows: map[DesignStep]DesignGate{DesignStepSpec: {Issue: tree, Step: DesignStepSpec,
			LatestVersion: 1, ApprovedVersion: &approved, ApprovedMark: EventMark{Seq: 7, At: opened}, EverOpened: true}}}
	}
	for _, parent := range parents {
		facts.Parents[parent.Key] = parent
	}
	return facts
}

// heldFactsOf is the facts the ladder reads over issues, built as HeldList and Waiting build
// them: the records by key, the live roots and the chain over the records the read leaves out.
func heldFactsOf(issues []Issue, facts LineFacts) HeldFacts {
	byKey := make(map[string]Issue, len(issues))
	for _, issue := range issues {
		byKey[issue.Key] = issue
	}
	return HeldFacts{ByKey: byKey, Live: liveRoots(issues, facts, queuedRoots(issues, facts)),
		Chain: chainOf(byKey, facts.Parents), Facts: facts}
}

// planHeld is the record of key held from from for reason, as the workflow writes a hold: at
// the status of the last step it ran, since a hold writes no Dispatch status of its own.
func planHeld(key, tree string, parent *string, from phase.Phase, reason HoldReason) Issue {
	issue := linePartIssue(key, tree, "needs_review", phase.Held, key)
	issue.Parent = parent
	issue.Hold = &Hold{From: from, Reason: reason}
	return issue
}

// planChild is the record of key beneath parent in tree, standing in stands at status.
func planChild(key, tree, parent, status string, stands phase.Phase) Issue {
	issue := linePartIssue(key, tree, status, stands, key)
	issue.Parent = &parent
	return issue
}

// The two holds an owner's plan makes each read a word of their own in the held list, in place
// of unclassified, which C2 calls a gap in the words (HeldUnclassified). A parent held before
// its retro for the issues beneath it (HoldForChildren) reads children_before_retro and names
// each of them still in the workflow (OpenBeneath, over the chain the pass walks, so a grandchild
// under a child already done is named and an issue beside the parent is not). A task its owner
// stopped (HoldTaskStopped) reads task_stopped where it keeps no slot where it stands, and names
// nobody.
//
// The controls read unclassified, as each reads it today: a hold of any other reason — a failed
// claim, which is the only hold an issue with no plan rows has, and an escalation — and a
// stopped task that keeps its slot but has no slot row, since such an issue should stand in the
// active set and its absence from it is the gap unclassified reports. A word above the two new
// ones that is true of the issue still answers first.
func TestEachHoldOfAPlanReadsItsOwnHeldWord(t *testing.T) {
	parked := linePartIssue("LEGION-73", "LEGION-73", "backlog", phase.Admitted, "LEGION-73")
	doneChild := planChild("LEGION-42", "LEGION-40", "LEGION-41", DoneStatus, phase.Done)
	issues := []Issue{
		// A root held before its retro for its children: one still in the workflow, one done.
		planHeld("LEGION-30", "LEGION-30", nil, phase.Retro, HoldForChildren),
		planChild("LEGION-31", "LEGION-30", "LEGION-30", "in_progress", phase.Implementing),
		planChild("LEGION-32", "LEGION-30", "LEGION-30", DoneStatus, phase.Done),
		// A child held before its retro for its own children: its child is done and out of the
		// live read, and that child's child is still at work; the issue beside it is not beneath it.
		linePartIssue("LEGION-40", "LEGION-40", "in_progress", phase.Implementing, "LEGION-40"),
		planHeld("LEGION-41", "LEGION-40", new("LEGION-40"), phase.Retro, HoldForChildren),
		planChild("LEGION-43", "LEGION-40", "LEGION-42", "in_progress", phase.Implementing),
		planChild("LEGION-44", "LEGION-40", "LEGION-40", "in_progress", phase.Implementing),
		// Tasks stopped where they keep no slot.
		planHeld("LEGION-50", "LEGION-50", nil, phase.Accepting, HoldTaskStopped),
		planHeld("LEGION-51", "LEGION-51", nil, phase.Retro, HoldTaskStopped),
		planHeld("LEGION-52", "LEGION-52", nil, phase.Merging, HoldTaskStopped),
		// The controls.
		planHeld("LEGION-60", "LEGION-60", nil, phase.Testing, HoldTaskStopped),
		planHeld("LEGION-61", "LEGION-61", nil, phase.Retro, HoldEscalated),
		planHeld("LEGION-62", "LEGION-62", nil, phase.Retro, ""),
		planHeld("LEGION-63", "LEGION-63", nil, phase.Implementing, ""),
		// Earlier words: a Dispatch blocker, and a tree a person parked.
		planHeld("LEGION-70", "LEGION-70", nil, phase.Retro, HoldForChildren),
		planChild("LEGION-71", "LEGION-70", "LEGION-70", "in_progress", phase.Implementing),
		parked,
		planHeld("LEGION-72", "LEGION-73", new("LEGION-73"), phase.Merging, HoldTaskStopped),
	}
	facts := planHoldFacts([]string{"LEGION-30", "LEGION-40", "LEGION-50", "LEGION-51", "LEGION-52",
		"LEGION-60", "LEGION-61", "LEGION-62", "LEGION-63", "LEGION-70", "LEGION-73"}, doneChild)
	facts.Held = map[string][]string{"LEGION-70": {"LEGION-900"}}
	held := heldFactsOf(issues, facts)
	byKey := held.ByKey

	for _, tc := range []struct {
		name   string
		issue  string
		word   HeldReason
		heldBy []string
	}{
		{"a root held before its retro names its child in the workflow", "LEGION-30", HeldChildrenBeforeRetro, []string{"LEGION-31"}},
		{"a child held before its retro names the issue beneath its done child", "LEGION-41", HeldChildrenBeforeRetro, []string{"LEGION-43"}},
		{"a task stopped in accepting", "LEGION-50", HeldTaskStopped, nil},
		{"a task stopped in its retro", "LEGION-51", HeldTaskStopped, nil},
		{"a task stopped in its merge", "LEGION-52", HeldTaskStopped, nil},
		{"control: a stopped task that keeps its slot and holds none", "LEGION-60", HeldUnclassified, nil},
		{"control: an escalated hold from retro", "LEGION-61", HeldUnclassified, nil},
		{"control: a failed claim from retro, the hold of an issue with no plan rows", "LEGION-62", HeldUnclassified, nil},
		{"control: a failed claim from implementing, the hold of an issue with no plan rows", "LEGION-63", HeldUnclassified, nil},
		{"a Dispatch blocker answers before the hold for children", "LEGION-70", HeldDispatchBlocker, []string{"LEGION-900"}},
		{"a parked tree answers before the stopped task", "LEGION-72", HeldTreeParked, []string{"LEGION-73"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			word, heldBy := HeldReasonOf(byKey[tc.issue], held)
			if word != tc.word || !reflect.DeepEqual(heldBy, tc.heldBy) {
				t.Fatalf("HeldReasonOf(%s) = %s %v, want %s %v", tc.issue, word, heldBy, tc.word, tc.heldBy)
			}
		})
	}

	// The chain is what names the grandchild: over the live records alone the walk up from
	// LEGION-43 stops at its done parent and reads only the root above, so LEGION-41 would name
	// nobody.
	alone := planHoldFacts([]string{"LEGION-40"})
	if word, heldBy := HeldReasonOf(byKey["LEGION-41"], heldFactsOf(issues, alone)); word != HeldChildrenBeforeRetro || heldBy != nil {
		t.Fatalf("HeldReasonOf(LEGION-41) with no record of LEGION-42 = %s %v, want children_before_retro naming nobody", word, heldBy)
	}
}

// The held list of a pass gives each hold of a plan its word through the one assembly, with the
// issue at work beneath the held parent in the active set and the done one in no list, beside a
// failed claim's hold from retro, which reads unclassified as it does today.
func TestTheHeldListGivesEachHoldOfAPlanItsWord(t *testing.T) {
	admitted := time.Date(2026, 11, 17, 6, 0, 0, 0, time.UTC)
	issues := []Issue{
		planHeld("LEGION-30", "LEGION-30", nil, phase.Retro, HoldForChildren),
		planChild("LEGION-31", "LEGION-30", "LEGION-30", "in_progress", phase.Implementing),
		planChild("LEGION-32", "LEGION-30", "LEGION-30", DoneStatus, phase.Done),
		planHeld("LEGION-52", "LEGION-52", nil, phase.Merging, HoldTaskStopped),
		planHeld("LEGION-62", "LEGION-62", nil, phase.Retro, ""),
	}
	facts := planHoldFacts([]string{"LEGION-30", "LEGION-52", "LEGION-62"})
	slots := []Slot{{Issue: "LEGION-31", Index: 0, AdmittedAt: admitted}}

	line, dropped := Waiting(issues, slots, facts)
	got := HeldList(issues, slots, line, dropped, facts)
	want := []HeldIssue{
		{Issue: "LEGION-30", Reason: HeldChildrenBeforeRetro, HeldBy: []string{"LEGION-31"}},
		{Issue: "LEGION-52", Reason: HeldTaskStopped},
		{Issue: "LEGION-62", Reason: HeldUnclassified},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the held list = %+v, want %+v", got, want)
	}
}
