package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// A lens on a block of a comment or on a part of an ask that a later revision of its item removes
// stays live, and no read answers it a null `on` or parent (spec requirement r-proposed): the block
// is history for the lens, as a removed block of a document is. alice writes a comment with two
// blocks on the spec's issue and invokes a BOTEC on its second block, and an edit of the comment
// removes that block; she invokes a BOTEC on the second option of an open ask, and an edit of the
// ask drops that option. Each BOTEC answers in three places the block at N, the revision before the
// edit, the last one that held it: GET /api/v1/lenses?on= the BOTEC answers its on as
// `dispatch://KEY/comment/<id>#b-<block>@v<N>` (`.../ask/<id>#b-<part>@v<N>`), GET
// /api/v1/references?to= the BOTEC answers a node whose parent names that block at that address
// with parent_version N, and GET /api/v1/lenses?on= that pinned address lists the BOTEC on it.
func TestALensOnARemovedPartOrCommentBlockNamesItsLastRevision(t *testing.T) {
	t.Parallel()
	rig := newReasoningRig(t)

	comment := createThreadComment(t, rig.handler, rig.issue, map[string]any{"body": "The first remark.\n\nThe second remark."}, "alice")
	commentBlock := "dispatch://" + rig.issue + "/comment/" + comment.ID + "#b-b2"
	onComment := rig.invoke(commentBlock, "botec", "alice", nil)
	requireEdit(t, "the edit of the comment that removes its second block",
		editCommentAs(t, rig.handler, comment.ID, "The first remark.\n", nil, nil))
	requireLensOnRemovedItemBlock(t, rig, onComment.Address, commentBlock, refs.ItemComment, comment.ID, "b2")

	ask := createPartAsk(t, rig.handler, rig.issue, "Which day do we ship?",
		map[string]string{"label": "Monday", "description": "Ship at the start of the week."},
		map[string]string{"label": "Tuesday", "description": "Ship once the review lands."})
	askPart := askPartRef(rig.issue, ask.ID, "b3")
	onAsk := rig.invoke(askPart, "botec", "alice", nil)
	editPartAskOptions(t, rig.handler, ask.ID, "the edit of the ask that drops its second option",
		map[string]string{"label": "Monday", "description": "Ship at the start of the week."})
	requireLensOnRemovedItemBlock(t, rig, onAsk.Address, askPart, refs.ItemAsk, ask.ID, "b3")
}

// requireLensOnRemovedItemBlock fails, in the words of c-lens-page, unless the lens at lens, on
// block blockID of the item of kind whose uuid is itemID, addressed block, reads live and names the
// block at the revision before the one that removed it: as its on in GET /api/v1/lenses?on= lens,
// as its node's parent and parent_version in GET /api/v1/references?to= lens, and as its on in GET
// /api/v1/lenses?on= that pinned address, which lists it.
func requireLensOnRemovedItemBlock(t *testing.T, rig reasoningRig, lens, block string, kind refs.ItemKind, itemID, blockID string) {
	t.Helper()
	at, err := refs.ItemBlockAt(context.Background(), rig.database.Pool, kind, itemID, blockID, 1)
	if err != nil {
		t.Fatalf("read block %s of %s %s: %v", blockID, kind, itemID, err)
	}
	removed := at.RemovedRevision
	if removed < 2 || removed != at.Current {
		t.Fatalf("block %s of %s %s was removed at revision %d and the item is at revision %d: the test's premise is that its edit removed it",
			blockID, kind, itemID, removed, at.Current)
	}
	held := removed - 1
	pinned := fmt.Sprintf("%s@v%d", block, held)

	state := storedNodeOf(t, rig.database, lens).State
	read := rig.lenses(lens, "alice")
	on := lensOn(read)
	response := dispatchReferences(t, rig.handler, lens, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/references?to=%s: status=%d body=%s", lens, response.Code, response.Body.String())
	}
	node := decodeBody[model.GraphReferences](t, response).Node
	parent, parentVersion := "<nil>", "<nil>"
	if node.Parent != nil {
		parent = node.Parent.Ref
	}
	if node.ParentVersion != nil {
		parentVersion = fmt.Sprint(*node.ParentVersion)
	}
	fail := func(on string) {
		t.Helper()
		t.Errorf("the lens %s on %s reads on %s, parent %s at %s once revision %d removed the block, want it live on %s, with that block as its parent at revision %d",
			lens, block, on, parent, parentVersion, removed, pinned, held)
	}
	if state != graphstore.StateLive || on != pinned || read.Lens == nil || read.Lens.State != graphstore.StateLive ||
		node.Parent == nil || node.Parent.Ref != pinned || node.ParentVersion == nil || *node.ParentVersion != held {
		got := on
		if state != graphstore.StateLive {
			got += " while its row reads " + state
		}
		fail(got)
	}

	listing := rig.lensesResponse(pinned, "alice")
	if listing.Code != http.StatusOK {
		fail(fmt.Sprintf("nothing in GET /api/v1/lenses?on=%s, which answers %d %s", pinned, listing.Code, listing.Body.String()))
		return
	}
	onPinned := decodeBody[nodeLenses](t, listing)
	listed := "nothing"
	for _, each := range onPinned.Lenses {
		if each.Address == lens {
			listed = lensOn(nodeLenses{Lens: &each})
		}
	}
	if onPinned.On.Address != pinned || listed != pinned {
		fail(fmt.Sprintf("%s in GET /api/v1/lenses?on=%s, which answers the node at %s", listed, pinned, onPinned.On.Address))
	}
}
