package api

import (
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// TestAnEditNamingAnotherParentPlacesAStoredCheckThere: a check a move of the node route placed
// under a requirement holds the placement stored, and its text names that requirement in its
// parent attribute, as the move wrote it. An edit of the Verification document that names another
// requirement in that attribute - another target than the parent value the store holds - moves the
// check under that requirement and gives it back the placement authored: the author's newest write
// wins (R7 of the issue-tree spec, refs' placeAuthored). c-reparent-and-state reads the same
// through its driver.
func TestAnEditNamingAnotherParentPlacesAStoredCheckThere(t *testing.T) {
	handler, database := blockIndexHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	if issue := createSpecIssue(t, handler, "CORE", placementSpec); issue.Key != "CORE-1" {
		t.Fatalf("the seeded issue is %s, want CORE-1, which its spec's references name", issue.Key)
	}
	spec := readDecisionDocument(t, database, "CORE-1", "spec").address
	ref := func(id string) string { return "dispatch://CORE-1/spec#b-" + id }
	verification := uploadDocument(t, handler, "/api/v1/issues/CORE-1", "verification.md", servingNode("check", "c1", ref("r1")))
	check := spec("c1")
	requirePlaced(t, database, "the check", check, refs.BlockNodeKind, spec("r1"), "r1", refs.PlacementAuthored)

	moveNodeUnder(t, handler, database, "the check", check, spec("r3"))
	requirePlaced(t, database, "the check after its move", check, refs.BlockNodeKind, spec("r3"), "r3", refs.PlacementStored)

	editThroughTheArtifactRoute(t, handler, verification.Artifact.ID, []map[string]any{{
		"op": "retype", "block": "c1", "type": "check",
		"attributes": map[string]any{
			"serves": []string{ref("r1")}, "parent": []string{ref("r4")},
			"surface": "api", "run": "true", "negative_control": "Undo it.",
		},
	}})
	requirePlaced(t, database, "the check once an edit names r4 as its parent", check, refs.BlockNodeKind, spec("r4"), "r4", refs.PlacementAuthored)
}
