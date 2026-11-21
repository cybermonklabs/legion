package api

import (
	"fmt"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
)

// Removing a document takes every lens on it to gone, and all that stands under each, in the
// transaction that deletes the document's row (spec requirement r-proposed, c-settle): a lens on a
// document that is no more would read live with nothing to stand on. On c-settle's rig
// (settleRig), alice makes a document of the spec's issue by typing a title and writes nothing in
// it, invokes a trade-off analysis LD on it with one finding FD, and the second agent invokes a
// lens LH on FD, reserved for itself, which alice may not see. Her DELETE /api/v1/artifacts/{id}
// answers 204, and the row of each of LD, FD and LH reads gone; her GET /api/v1/lenses?on= LD and
// GET /api/v1/references?to= LD, and the second agent's same two reads of LH, each answer what the
// read of a lens nobody wrote answers, 404 NOT_FOUND, byte for byte once the id is substituted. No
// lens stands on a block of the document: the route removes only a document whose one version is
// empty, which holds no block.
func TestRemovingADocumentTakesEveryLensOnItGone(t *testing.T) {
	settle := newSettleRig(t)
	document := emptyDocument(t, settle.handler, settle.issue.Key, "Notes nobody wrote.md")
	address := "dispatch://" + settle.issue.Key + "/artifact/" + document.Slug
	ld := settle.invoke(t, "alice's trade-off analysis LD on the document", settle.asAlice, map[string]any{
		"on": address, "kind": "trade-off", "findings": []string{"FD, the one finding of LD."},
	})
	fd := ld.Findings[0]
	lh := settle.invoke(t, "the second agent's lens LH on FD", settle.asSecond, map[string]any{
		"on": fd.Address, "kind": "trade-off", "visibility": graphstore.VisibilityAuthor,
	})

	removedDocument(t, settle.handler, document.ID)
	for _, node := range []string{ld.Address, fd.Address, lh.Address} {
		if state := storedNodeOf(t, settle.database, node).State; state != graphstore.StateGone {
			t.Errorf("after DELETE /api/v1/artifacts/%s, %s reads %s, want gone with the document it stood on", document.ID, node, state)
		}
	}
	requireReadsAsALensNobodyWrote(t, func(read, status, login string) string {
		return fmt.Sprintf("after DELETE /api/v1/artifacts/%s, %s answers %s to %s, want 404 NOT_FOUND as for a lens nobody wrote",
			document.ID, read, status, login)
	}, lensReader{"alice", settle.asAlice, ld}, lensReader{"the second agent", settle.asSecond, lh})
}
