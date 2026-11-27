package docs

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A room settlement that opens the ask of a new ask block of the spec's fragment holding a shaped
// pointer, and writes the spec's version in the same transaction, states the block's shaped tie as
// a document's settlement does (TestASettlementThatOpensAnAskAndWritesAVersionStatesItsShapedTie):
// the room's pass reads the asks table, which holds no row the settlement has only queued, so the
// settlement writes the tie after it (writeOpenedAskTies). The tie's block_refs row, keyed under
// the tree artifact, names the ask's node, the ask's shaped tuple is caused by the ask.opened that
// records the ask, and the row records the version of the block's own document the settlement
// wrote (the spec's 3): never the spec's version before it (its 2), which the settlement read
// before it wrote, nor a number of the tree artifact's history (its 1), whose own fragment the
// settlement left alone.
func TestARoomSettlementThatOpensAnAskAndWritesAVersionStatesItsShapedTieAtThatVersion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := storetest.Open(t)
	service, server := newTestServiceInstance(t, database)
	room := createIssueRoom(t, database, 1)
	spec := connectPeer(t, server.URL, room.spec, "alice")
	if err := spec.WaitForText(fragmentMarkdown(room.plan), planSeed, roomWait(t)); err != nil {
		t.Fatal(err)
	}
	fragment := spec.Doc.GetXmlFragment(room.spec)
	settle := func(what, markdown string) {
		t.Helper()
		tree, err := pmdoc.Parse(markdown)
		if err != nil {
			t.Fatalf("%s: parse the spec: %v", what, err)
		}
		before := storedUpdateCount(t, database, room.tree)
		var written error
		if _, err := spec.Send(func(txn *crdt.Transaction) { written = pmdoc.Update(txn, fragment, tree) }); err != nil || written != nil {
			t.Fatalf("%s: %v, %v", what, err, written)
		}
		settleLandedWrite(t, ctx, database, service, room, before)
	}
	const requirement = ":::requirement{#r1}\nThe store holds the corpus.\n:::\n"
	settle("write the requirement into the spec's fragment", requirement)
	before := savedVersion(t, database, room.spec)

	shaped := refs.BlockRef(refs.ArtifactRef(&room.issue, "DOC", nil, "spec", "doc", true), "r1")
	settle("write the ask block into the spec's fragment", requirement+
		"\n:::ask{#q-tie urgency=\"med\" multiple=\"false\" shaped=\""+shaped+"\"}\nWhich store holds the corpus?\n:::\n")
	written, tree := savedVersion(t, database, room.spec), savedVersion(t, database, room.tree)
	if written != before+1 || tree == before || tree == written {
		t.Fatalf("the settlement left the spec at version %d after version %d and the tree artifact at version %d, want the spec one version on and the tree at neither: it is the settlement that writes a version of the spec alone",
			written, before, tree)
	}

	ask := blockAskOn(t, service, room.tree, "q-tie")
	treeRef := refKeyOf(t, database, room.tree)
	type shapedRow struct {
		nodeKind, nodeAddress, toKind, toID string
		version                             int
	}
	rows, err := database.Pool.Query(ctx, `
		select coalesce(node_kind, ''), coalesce(node_address, ''), to_kind, to_id, created_version from block_refs
		where artifact_id = $1 and block_id = 'q-tie' and kind = $2
	`, room.tree, refs.TupleShaped)
	if err != nil {
		t.Fatalf("read the shaped rows of q-tie: %v", err)
	}
	held, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (shapedRow, error) {
		var tie shapedRow
		err := row.Scan(&tie.nodeKind, &tie.nodeAddress, &tie.toKind, &tie.toID, &tie.version)
		return tie, err
	})
	if err != nil {
		t.Fatalf("read the shaped rows of q-tie: %v", err)
	}
	if len(held) != 1 {
		t.Fatalf("the room's index holds the shaped rows %+v of q-tie, want the one the settlement states after its pass", held)
	}
	tie := held[0]
	if tie.nodeKind != graphstore.KindAsk || tie.nodeAddress != ask.ID {
		t.Errorf("the shaped row of q-tie names the node (%s, %s), want the ask's node (%s, %s)", tie.nodeKind, tie.nodeAddress, graphstore.KindAsk, ask.ID)
	}
	if target := refs.BlockAddress(treeRef, "r1"); tie.toKind != refs.BlockNodeKind || tie.toID != target {
		t.Errorf("the shaped row of q-tie names the far end (%s, %s), want the requirement's block in the room, %s", tie.toKind, tie.toID, target)
	}
	if tie.version != written {
		t.Errorf("the shaped row of q-tie records version %d, want %d, the spec's version the settlement wrote: not the spec's version before it (%d), nor the tree artifact's (%d)",
			tie.version, written, before, tree)
	}
	ties, err := graphstore.CurrentOut(ctx, database.Pool, graphstore.KindAsk, ask.ID, []string{refs.TupleShaped})
	if err != nil {
		t.Fatalf("read the shaped ties of ask %s: %v", ask.ID, err)
	}
	if len(ties) != 1 || ties[0].ToKind != tie.toKind || ties[0].ToID != tie.toID {
		t.Errorf("ask %s states the shaped ties %+v, want one to (%s, %s), the far end its row names", ask.ID, ties, tie.toKind, tie.toID)
	}
	rig := &docsWriterRig{database: database, service: service}
	if err := rig.tiesCaused(ctx, room.tree, "q-tie"); err != nil {
		t.Error(err)
	}
}
