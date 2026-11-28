package record

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// A5's rule that a child in backlog holds nothing, read over a child some other tree of the daemon
// recorded. Whether a child is open is a question about the child — does a record of it exist,
// and has that record left the workflow — and not about which tree holds that record: a child
// another tree moved to backlog has left the workflow as one this tree moved has, and no tree will do its work,
// so reading it as open would hold its Dispatch parent for ever.
//
// A child no tree records at all is the opposite case and stays open in backlog or triage: it is
// work the architect filed and has not released yet, which is exactly what the parent must wait
// for.
func TestAChildInBacklogInAnotherTreeHoldsNothing(t *testing.T) {
	const parent, inBacklogElsewhere, unrecorded, otherTree = "LEGION-500", "LEGION-501", "LEGION-502", "LEGION-600"
	parentKey := parent
	issues := []Issue{{Key: parent, Tree: parent, Project: "LEGION", Phase: phase.Admitted, Status: "in_progress"}}
	children := map[string][]DispatchIssueRow{parent: {
		{Key: inBacklogElsewhere, Project: "LEGION", Status: "backlog", Parent: parent},
		{Key: unrecorded, Project: "LEGION", Status: "backlog", Parent: parent},
	}}
	// The child in backlog has its own record in another tree: a person moved it under this parent
	// on Dispatch after that tree moved it to backlog, and workflow's leave recorded it at phase
	// done there.
	records := map[string]Issue{inBacklogElsewhere: {Key: inBacklogElsewhere, Tree: otherTree, Parent: &parentKey,
		Project: "LEGION", Phase: phase.Done, Status: "backlog"}}

	waits := childWaits(&passChain{issues: issues}, children, records)
	wait, waiting := waits[parent]
	if !waiting {
		t.Fatalf("the judgement of %s = %+v, want it waiting on the child no tree records", parent, waits)
	}
	if len(wait.Open) != 1 || wait.Open[0].Key != unrecorded {
		t.Fatalf("the open children of %s = %+v, want %s alone: %s is in backlog, and the tree that moved it there is not the question",
			parent, wait.Open, unrecorded, inBacklogElsewhere)
	}
}
