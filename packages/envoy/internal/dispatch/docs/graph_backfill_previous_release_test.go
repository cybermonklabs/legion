package docs

import (
	"context"
	"fmt"
	"io"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// A backfill run while a pod of the release before this one is live still gives every block its
// parent, and the census still reaches zero.
//
// The backfill states no block's parent: it marks the block's document with a mark of this
// build's own, document_block_versions.store_owed_at (migration 20261116221539), and this
// build's reconciliation states every block's part_of tuple and parent_block_id when it indexes
// the marked document, and clears the mark. The release before this one reads and drains a
// different mark, the null version (its docs.blockIndex reads document_block_versions where
// version is null), and writes the rows, but it knows no store, so it states no part_of tuple.
// It never reads or clears store_owed_at, so a pod of it that drains a null version first -
// during a roll, or after a rollback, which the README names as a time the backfill is safe to
// run - leaves the backfill's mark where it was, and this build's reconciliation still reaches
// the document.
//
// The defect this holds: the backfill used to mark the document by that null version itself.
// The tester drove it on 2026-11-16 at 5fd216378e30: legion-envoy-v30.35.1 and this build
// serving one database, a document written through the previous release, then
// `envoy-dispatch backfill-graph-store` (complete=true, owed=2). The previous release's process
// logged `block index documents=2 ... owed_documents=0` 40 s later, the four blocks kept an
// in_document tuple and no part_of, and `census --store` refused with `the row and its node
// disagree` at each of them minutes later and after every later tick: the document was no
// longer owed, and a second backfill reads no row that names its node, so it marked nothing.
//
// This test stands in for the previous release's pass with the writes of it that matter here.
// An edit or a rebuild-refs of either release first owes the backfilled documents a block index
// (their version to null). The stand-in then writes each block's parent_block_id, the nesting the
// seed's own indexing computed, and drains that null version to the latest version, as that pass
// does when it indexes them; it writes nothing to the store, as that pass cannot. The backfill's
// own mark must outlive that drain, and this build's reconciliation must then state every parent.
func TestABackfillWhoseMarkThePreviousReleaseDrainedStillReachesEveryParent(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()

	const documents = 3
	for index := range documents {
		slug := fmt.Sprintf("prev%02d", index)
		id := createOwedDocument(t, database, service, "CORE-1", "", slug, nestedSpec(slug), "")
		settle(t, database, id)
	}
	// Rows of every other kind the census reads, as TestTheBlockParentCensusReachesZeroAfterTheBackfill
	// seeds them, so the census can pass only on agreement and never refuses an empty kind here.
	if _, err := database.Pool.Exec(ctx, `
		insert into comments (issue_key, author, body) values
			('CORE-1', '{"kind":"user","id":"Alice"}', 'A remark written before the store existed.'),
			('CORE-1', '{"kind":"session","id":"previous-release-session"}', 'A session''s remark written before the store existed.');
		update issues set route = 'role:previous-release-role' where key = 'CORE-1';
	`); err != nil {
		t.Fatalf("seed the comments and the route: %v", err)
	}
	drainBlockIndex(t, service, "seed")
	before := blockStates(t, database)
	if len(before) == 0 {
		t.Fatal("the seed indexed no block, so this test would prove nothing")
	}
	takeTheStoreOff(t, database)

	report, err := graphcensus.Backfill(ctx, database, io.Discard)
	if err != nil {
		t.Fatalf("backfill the store: %v", err)
	}
	blocks := backfilledKind(t, report, refs.BlockNodeKind)
	if blocks.Owed != documents || !blocks.Complete {
		t.Fatalf("the backfill wrote %#v, want %d documents marked owed and the cutover complete", blocks, documents)
	}

	// The previous release's block index takes the lease first and drains the mark: it writes
	// each block's parent_block_id and the document's indexed version, and no tuple.
	for address, state := range before {
		if !state.hasParent {
			continue
		}
		if _, err := database.Pool.Exec(ctx,
			`update document_blocks set parent_block_id = $2 where node_id = $1`, address, state.parent); err != nil {
			t.Fatalf("write the parent of %s as the previous release's block index does: %v", address, err)
		}
	}
	// An edit or a rebuild-refs of either release owes the backfilled documents a block index
	// (document_block_versions.version to null), the mark the previous release's block index
	// reads and drains: the backfill's own mark leaves the version alone.
	if _, err := database.Pool.Exec(ctx, `
		update document_block_versions set version = null
		where artifact_id in (select artifact_id from document_blocks)`); err != nil {
		t.Fatalf("owe the backfilled documents a block index as an edit or rebuild-refs does: %v", err)
	}
	drained, err := database.Pool.Exec(ctx, `
		update document_block_versions b
		set version = (select max(v.number) from artifact_versions v where v.artifact_id = b.artifact_id)
		where b.version is null`)
	if err != nil {
		t.Fatalf("drain the mark as the previous release's block index does: %v", err)
	}
	if drained.RowsAffected() != documents {
		t.Fatalf("the previous release's pass drained %d mark(s), want the %d the backfill set", drained.RowsAffected(), documents)
	}
	// The backfill's own mark is no mark that release reads, so the drain leaves it on every
	// backfilled document for this build's reconciliation.
	var marked int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from document_block_versions
		where store_owed_at is not null and artifact_id in (select artifact_id from document_blocks)`).Scan(&marked); err != nil {
		t.Fatalf("read the backfill's own mark: %v", err)
	}
	if marked != documents {
		t.Errorf("%d backfilled document(s) still hold the backfill's own mark after the previous release drained the null version, want all %d",
			marked, documents)
	}

	// This build's reconciliation then runs as it does on every tick of every pod of it.
	drainBlockIndex(t, service, "after the previous release drained the mark")

	for address := range blockStates(t, database) {
		if _, stated := currentBlockParent(t, database, address); !stated {
			t.Errorf("block %s states no parent: the previous release drained its document's mark, and nothing states its part_of now", address)
		}
	}
	census := takeStoreCensus(t, database)
	for _, kind := range census.Kinds {
		for _, difference := range kind.ReportedRows {
			t.Errorf("the census reads a difference of kind %s at %s: %s", kind.Kind, difference.At, difference.Cause)
		}
	}
	if census.Refused() {
		t.Fatalf("the census refused with %d difference(s) after a backfill whose mark a pod of the previous release drained, want zero",
			census.Differences())
	}
}
