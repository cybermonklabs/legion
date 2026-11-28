package api

import (
	"context"
	"fmt"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Removing a document takes every lens on it to gone, and all that stands under each, in the
// transaction that deletes the document's row (spec requirement r-proposed, c-settle): a lens on a
// document that is no more would read live with nothing to stand on. On c-settle's rig
// (settleRig), alice makes a document of the spec's issue by typing a title and writes nothing in
// it, invokes a trade-off analysis LD on it with one finding FD, and the second agent invokes a
// lens LH on FD, reserved for itself, which alice may not see. Her DELETE /api/v1/artifacts/{id}
// answers 204, and the row of each of LD, FD and LH reads gone, written by the transaction that
// deleted the document's row: the removal writes the document's node gone in that transaction
// before the delete, so each of the four rows carries the same xmin, the id of the transaction
// that last wrote it, which a cascade in a transaction of its own after the commit would not. Her
// GET /api/v1/lenses?on= LD and GET /api/v1/references?to= LD, and the second agent's same two
// reads of LH, each answer what the read of a lens nobody wrote answers, 404 NOT_FOUND, byte for
// byte once the id is substituted. No lens stands on a block of the document: the route removes
// only a document whose one version is empty, which holds no block.
func TestRemovingADocumentTakesEveryLensOnItGone(t *testing.T) {
	t.Parallel()
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
	removal := rowWriter(t, settle.database, address)
	for _, node := range []string{ld.Address, fd.Address, lh.Address} {
		if state := storedNodeOf(t, settle.database, node).State; state != graphstore.StateGone {
			t.Errorf("after DELETE /api/v1/artifacts/%s, %s reads %s, want gone with the document it stood on", document.ID, node, state)
		}
		if writer := rowWriter(t, settle.database, node); writer != removal {
			t.Errorf("after DELETE /api/v1/artifacts/%s, the row of %s was last written by transaction %s, and the document's node %s by %s: want both by the removal's one transaction",
				document.ID, node, writer, address, removal)
		}
	}
	requireReadsAsALensNobodyWrote(t, func(read, status, login string) string {
		return fmt.Sprintf("after DELETE /api/v1/artifacts/%s, %s answers %s to %s, want 404 NOT_FOUND as for a lens nobody wrote",
			document.ID, read, status, login)
	}, lensReader{"alice", settle.asAlice, ld}, lensReader{"the second agent", settle.asSecond, lh})
}

// rowWriter is the xmin of the row the store holds of the node address names, the id of the
// transaction that last wrote it, failing where it holds none.
func rowWriter(t *testing.T, database *store.Store, address string) string {
	t.Helper()
	kind, id, ok := decodeNodeAddress(address)
	if !ok {
		t.Fatalf("decode the node address %s", address)
	}
	var xmin string
	if err := database.Pool.QueryRow(context.Background(), `select xmin::text from graph_nodes where kind = $1 and id = $2`,
		refs.StoredKindOf(kind), id).Scan(&xmin); err != nil {
		t.Fatalf("read the xmin of the row of %s: %v", address, err)
	}
	return xmin
}
