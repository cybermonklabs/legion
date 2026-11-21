package api

import (
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestAFindingNamedAsAReferenceTargetAnswersItsTextTree reads a lens's finding through
// GET /api/v1/reference-targets with Dispatch-Trees: 1. The route answers text_tree on each member
// whose kind is block, and a finding is a block: its member answers kind block, its text as its
// name and not gone, so a reader who draws the reference whole must get its tree as it gets a
// document block's. A document block of the same read answers its tree, which shows the read asked
// for trees.
func TestAFindingNamedAsAReferenceTargetAnswersItsTextTree(t *testing.T) {
	rig := newReasoningRig(t)
	const words = "The cache doubles the write path."
	lens := rig.invoke(rig.spec, "trade-off", "alice", map[string]any{"findings": []string{words}})
	if len(lens.Findings) != 1 {
		t.Fatalf("the lens %s holds %d findings, want 1", lens.Address, len(lens.Findings))
	}
	finding := lens.Findings[0].Address

	read := decodeBody[model.ReferenceTargets](t, treesRequest(t, rig.handler, referenceTargetsOf(finding, rig.check), true))
	if len(read.Targets) != 2 {
		t.Fatalf("the read of %s and %s answers %d members, want 2", finding, rig.check, len(read.Targets))
	}
	if document := read.Targets[1]; document.Gone || len(document.TextTree) == 0 {
		t.Fatalf("the document block %s answers gone=%v text_tree=%s, want its tree: the read asked for none",
			rig.check, document.Gone, document.TextTree)
	}
	member := read.Targets[0]
	if member.Kind != "block" || member.Gone || member.Name != words {
		t.Fatalf("the finding %s answers kind=%q gone=%v name=%q, want a block that is not gone named %q",
			finding, member.Kind, member.Gone, member.Name, words)
	}
	if len(member.TextTree) == 0 {
		t.Errorf("the finding %s, a member of kind block that is not gone, answers no text_tree, "+
			"where the route answers one on each block member", finding)
	}
}
