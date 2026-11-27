package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestTheTreeReadAnswersTheBlockAnAskIsAnchoredIn: over a spec of two paragraphs, alice opens an
// ask anchored on the words of the second paragraph and an ask anchored nowhere. The tree read
// with no view, ?view=decisions and ?kind=ask each answer the anchored ask's block_anchor as the
// spec's artifact_id and that paragraph's block_id, written
// `"block_anchor":{"artifact_id":…,"block_id":…}`, and the ask anchored nowhere no block_anchor;
// and for each block of the spec the number of the read's asks whose block_anchor names it is the
// asks count GET /api/v1/artifacts/{id}/blocks answers on that block, so a page counts an ask on a
// block from the tree read as the whole document's editor counts it. Its control is the read
// without the member: the line of issueTreeBuilder.node that sets BlockAnchor, or the lines of
// appendIssueTreeNode that write it, taken out. The anchored ask then answers none and the case
// fails, printing `the tree read<query> answers the ask <id> anchored in block <block> with the
// block anchor <nil>, want …`.
func TestTheTreeReadAnswersTheBlockAnAskIsAnchoredIn(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TASK", "Ask anchors in the tree", "Introduction.\n\nTarget passage.\n")
	listBlocks := func() []blockWithReferences {
		t.Helper()
		return decodeBody[[]blockWithReferences](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice"))
	}
	blocks := listBlocks()
	if len(blocks) != 2 {
		t.Fatalf("the spec of %s lists the blocks %+v, want the introduction and the target passage", issue.Key, blocks)
	}
	target := blocks[1].ID

	anchored := decodeBody[model.Ask](t, created(t, "open an ask anchored in the target passage", dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"anchor":   map[string]string{"artifact": "spec", "quote": "Target passage"},
		"question": "Is this passage complete?",
	}, "alice")))
	if anchored.Anchor == nil || anchored.Anchor.BlockID == nil || *anchored.Anchor.BlockID != target {
		t.Fatalf("the ask %s answers the anchor %+v, want it in block %s: this case proves nothing", anchored.ID, anchored.Anchor, target)
	}
	unanchored := decodeBody[model.Ask](t, created(t, "open an ask anchored nowhere", dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Does the issue need a second passage?",
	}, "alice")))
	if unanchored.Anchor != nil {
		t.Fatalf("the ask %s answers the anchor %+v, want none: this case proves nothing", unanchored.ID, unanchored.Anchor)
	}
	blocks = listBlocks()
	if len(blocks) != 2 || blocks[1].ID != target || blocks[1].References.Asks != 1 || blocks[0].References.Asks != 0 {
		t.Fatalf("the spec of %s lists the blocks %+v, want one ask counted on %s and none on the introduction: this case proves nothing", issue.Key, blocks, target)
	}

	want := model.IssueTreeBlockAnchor{ArtifactID: issue.PrimaryArtifactID, BlockID: target}
	written := `"block_anchor":{"artifact_id":"` + want.ArtifactID + `","block_id":"` + want.BlockID + `"}`
	for _, query := range []string{"", "?view=decisions", "?kind=ask"} {
		read := treeRead(t, handler, issue.Key, query, "alice", false)
		if node, _ := read.node(t, graphstore.KindAsk, anchored.ID); node.BlockAnchor == nil || *node.BlockAnchor != want {
			t.Errorf("the tree read%s answers the ask %s anchored in block %s with the block anchor %+v, want %+v", query, anchored.ID, target, node.BlockAnchor, want)
		}
		if !strings.Contains(read.body, written) {
			t.Errorf("the tree read%s of %s holds no %s, the member the anchored ask %s answers: %s", query, issue.Key, written, anchored.ID, read.body)
		}
		if node, _ := read.node(t, graphstore.KindAsk, unanchored.ID); node.BlockAnchor != nil {
			t.Errorf("the tree read%s answers the ask %s anchored nowhere with the block anchor %+v, want none", query, unanchored.ID, *node.BlockAnchor)
		}
		for _, block := range blocks {
			counted := 0
			for _, node := range read.Nodes {
				if node.Kind == graphstore.KindAsk && node.BlockAnchor != nil &&
					*node.BlockAnchor == (model.IssueTreeBlockAnchor{ArtifactID: issue.PrimaryArtifactID, BlockID: block.ID}) {
					counted++
				}
			}
			if counted != block.References.Asks {
				t.Errorf("the tree read%s of %s answers %d asks anchored in block %s, want the %d GET /api/v1/artifacts/{id}/blocks counts on it", query, issue.Key, counted, block.ID, block.References.Asks)
			}
		}
	}
}
