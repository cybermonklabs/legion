package grapherase

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/schemanodes"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// citeTheBlock states, in a transaction of its own that commits, what a version of another
// document states when it cites the block at address: the citing block's node and its mentions
// tuple into that block. It takes neither the cited node's owner row nor its row, which is why it
// commits while an erase of that node is running, and answers its mentions tuple's seq.
func citeTheBlock(t *testing.T, database *store.Store, address string) int64 {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the citing version's write: %v", err)
	}
	defer tx.Rollback(ctx)
	now := time.Now().UTC()
	citing := "ACME-1/spec#cite"
	if err := graphstore.Write(ctx, tx, []graphstore.Node{{
		Kind: refs.BlockNodeKind, ID: citing, Supertag: refs.BlockNodeKind,
		Visibility: graphstore.VisibilityProject, State: graphstore.StateLive,
		MadeAt: now, MadeBy: seedWriter, SourceEvent: graphstore.NoEvent,
	}}, []graphstore.Tuple{{
		FromKind: refs.BlockNodeKind, FromID: citing, Kind: "mentions",
		ToKind: refs.BlockNodeKind, ToID: address,
		At: now, Who: seedWriter, SourceEvent: graphstore.NoEvent, Asserted: true,
	}}); err != nil {
		t.Fatalf("state the citing block's mention of %s: %v", address, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the citing version's write: %v", err)
	}
	var seq int64
	if err := database.Pool.QueryRow(ctx, `
		select seq from graph_tuples where from_kind = $1 and from_id = $2 and to_id = $3 and kind = 'mentions'
	`, refs.BlockNodeKind, citing, address).Scan(&seq); err != nil {
		t.Fatalf("read the citing tuple's seq: %v", err)
	}
	return seq
}

// heldOf counts what the store holds of kind:id: its node row, its tuples on both sides and its
// projection rows.
func heldOf(t *testing.T, database *store.Store, kind, id string) int64 {
	t.Helper()
	var held int64
	if err := database.Pool.QueryRow(context.Background(), `
		select (select count(*) from graph_nodes where kind = $1 and id = $2)
		     + (select count(*) from graph_tuples where (from_kind = $1 and from_id = $2) or (to_kind = $1 and to_id = $2))
		     + (select count(*) from document_blocks where node_kind = $1 and node_address = $2)
		     + (select count(*) from block_refs where node_kind = $1 and node_address = $2)`, kind, id).Scan(&held); err != nil {
		t.Fatalf("count what the store holds of %s:%s: %v", kind, id, err)
	}
	return held
}

// R5: a confirmed run deletes the tuples it listed and no others. The erase's transaction is READ
// COMMITTED, so a tuple another writer commits while it runs - here another document's version
// citing the erased block, which takes neither this node's owner row nor its row - is one the
// delete would see. Deleting it would take away a tuple no listing named and leave its block_refs
// row stating a fact the store no longer holds, and the blocker check that refuses such a row was
// read before it landed.
//
// The test drives the confirmed run's own steps, as Erase drives them: its locks and the listing
// read under them (lockInStoreOrder), then apply. The citing write commits between the two. The
// erase refuses, naming the tuple's seq, and its transaction rolls back holding everything it
// held.
func TestTheEraseRefusesATupleThatLandedAfterItsListing(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seeded := seedAgreeingStore(t, database)
	conn, err := connect(ctx, database.Pool.Config().ConnString())
	if err != nil {
		t.Fatalf("connect as the erase does: %v", err)
	}
	defer conn.Close(context.Background())
	if err := schemanodes.InstallFromStore(ctx, conn); err != nil {
		t.Fatalf("install the block schema from the store: %v", err)
	}
	held := heldOf(t, database, refs.BlockNodeKind, seeded.inner)
	tx, erasure, read, err := lockInStoreOrder(ctx, conn, refs.BlockNodeKind, seeded.inner)
	if err != nil {
		t.Fatalf("take the erase's locks and read its listing: %v", err)
	}
	defer tx.Rollback(context.Background())
	if len(read.tupleSeqs) == 0 {
		t.Fatalf("the listing of %s names no tuple, so a tuple landing after it proves nothing", seeded.inner)
	}
	landed := citeTheBlock(t, database, seeded.inner)

	err = erasure.apply(ctx, tx, read)
	if err == nil {
		t.Fatalf("the erase of %s applied with a tuple that landed after its listing", seeded.inner)
	}
	if !strings.Contains(err.Error(), "the erase refuses") || !strings.Contains(err.Error(), strconv.FormatInt(landed, 10)) {
		t.Errorf("the erase answered %q, want a refusal naming the tuple %d that landed", err, landed)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("roll the refused erase back: %v", err)
	}
	if after := heldOf(t, database, refs.BlockNodeKind, seeded.inner); after != held+1 {
		t.Errorf("the refused erase left %d row(s) of %s, want the %d it held and the tuple that landed",
			after, seeded.inner, held+1)
	}
}
