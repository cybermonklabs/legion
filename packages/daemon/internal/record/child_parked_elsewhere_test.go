package record

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// A5's rule that a parked child holds nothing, read over a child whose record stores another
// tree. Whether a child is open is a question about the child — does a record of it exist, and
// has that record left the workflow — and not about which tree any record stores: the children
// are the copy's (Chains.Children), and the stored tree is read by no part of the judgement (R15
// of dispatch://INFRA-3320), so a parked child is as parked wherever its record says it stood, and
// reading it as open would hold its Dispatch parent for ever.
//
// A child the daemon does not record at all is the opposite case and stays open in backlog or
// triage: it is work the architect filed and has not released yet, which is exactly what the
// parent must wait for.
func TestAChildParkedInAnotherTreeHoldsNothing(t *testing.T) {
	const parent, parkedElsewhere, unrecorded, otherTree = "LEGION-500", "LEGION-501", "LEGION-502", "LEGION-600"
	parentRecord := Issue{Key: parent, Tree: parent, Project: "LEGION", Phase: phase.Admitted, Status: "in_progress"}
	// The parked child's own record stores another tree: a person moved it under this parent on
	// Dispatch after it was parked, and workflow's leave recorded it at phase done.
	parked := Issue{Key: parkedElsewhere, Tree: otherTree, Parent: ParentOf(parent), Project: "LEGION", Phase: phase.Done, Status: "backlog"}
	chains := chainsOf([]Issue{parentRecord, parked}, append(copyOf(parentRecord, parked),
		DispatchIssueRow{Key: unrecorded, Project: "LEGION", Status: "backlog", Parent: parent}))

	waits := ChildWaits(chains, []Issue{parentRecord})
	wait, waiting := waits[parent]
	if !waiting {
		t.Fatalf("the judgement of %s = %+v, want it waiting on the child the daemon does not record", parent, waits)
	}
	if len(wait.Open) != 1 || wait.Open[0].Key != unrecorded {
		t.Fatalf("the open children of %s = %+v, want %s alone: %s is parked, and the tree its record stores is not the question",
			parent, wait.Open, unrecorded, parkedElsewhere)
	}
}
