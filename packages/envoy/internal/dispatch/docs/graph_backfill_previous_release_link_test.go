package docs

import (
	"context"
	"io"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A link the release before this one adds, and a block it drops, after the backfill marked their
// document reach the store when this build's reconciliation drains the mark, and the census
// reaches zero.
//
// The order is the one a rollback gives, where the README calls the backfill safe to run: the
// release before this one serves, the backfill runs and marks each document it named the nodes
// of (store_owed_at), and a person then edits one of those documents through that release. The
// edit drops the innermost callout, whose paragraph holds the document's mention of CORE-2, and
// gives the mention to the middle paragraph instead. That release's index pass inserts the
// block_refs row of the link the version adds naming no node, deletes the rows of the blocks the
// version drops and of the dropped paragraph's link, and states nothing in the store. Once this
// build serves again, its reconciliation indexes the marked document, states every block's parent
// and clears the mark. The added link's row must then name its block's node and the store must
// assert that link, or the census refuses with `the row names no node`; and the dropped paragraph
// must read gone and its link must be retracted, or the census refuses a live node and an asserted
// link no row projects. Either lasts until someone runs the backfill again, which nothing tells
// anyone to do.
//
// The tester drove exactly that on 2026-11-18 at 696a9727c4fe with legion-envoy-v30.45.0 and
// this build on one database: after this build drained the mark, `census --store` refused with
// one difference, `block_ref CORE-4/drive-md#<block> mentions -> issue CORE-3: the row names no
// node`, and the block's tuples held its part_of and no mentions.
//
// The stand-in for that release is indexAsThePreviousRelease, as in
// TestTheBlockAndTheLinkThePreviousReleaseDroppedAreGoneAfterTheBackfill, run here after the
// backfill rather than before it.
func TestALinkThePreviousReleaseAddsAfterTheBackfillReachesTheStore(t *testing.T) {
	t.Parallel()
	database, service := blockIndexService(t)
	ctx := context.Background()

	const slug = "linked"
	document := createOwedDocument(t, database, service, "CORE-1", "", slug, nestedSpec(slug), "")
	settle(t, database, document)
	// Rows of every other kind the census reads, as TestTheBlockParentCensusReachesZeroAfterTheBackfill
	// seeds them, so the census can pass only on agreement and never refuses an empty kind here.
	if _, err := database.Pool.Exec(ctx, `
		insert into comments (issue_key, author, body) values
			('CORE-1', '{"kind":"user","id":"Alice"}', 'A remark written before the store existed.'),
			('CORE-1', '{"kind":"session","id":"previous-release-link-session"}', 'A session''s remark written before the store existed.');
		update issues set route = 'role:previous-release-link-role' where key = 'CORE-1';
	`); err != nil {
		t.Fatalf("seed the comments and the route: %v", err)
	}
	// The document as a build before the store wrote it: its rows, and nothing of the store.
	drainBlockIndex(t, service, "seed")
	takeTheStoreOff(t, database)
	var refKey string
	if err := database.Pool.QueryRow(ctx, `select ref_key from artifacts where id = $1`, document).Scan(&refKey); err != nil {
		t.Fatalf("read the document's ref_key: %v", err)
	}

	report, err := graphcensus.Backfill(ctx, database, io.Discard)
	if err != nil {
		t.Fatalf("backfill the store: %v", err)
	}
	if blocks := backfilledKind(t, report, refs.BlockNodeKind); blocks.Owed != 1 || !blocks.Complete {
		t.Fatalf("the backfill wrote %#v for the blocks, want the one document marked and the cutover complete", blocks)
	}

	inner := holderOf(t, service, document, "dispatch://CORE-2")
	middle := holderOf(t, service, document, "Middle of "+slug)
	dropped := blockLinkRow{blockID: inner}
	if err := database.Pool.QueryRow(ctx, `
		select kind, to_kind, to_id from block_refs where artifact_id = $1 and block_id = $2
	`, document, inner).Scan(&dropped.kind, &dropped.toKind, &dropped.toID); err != nil {
		t.Fatalf("read the inner paragraph's link: %v", err)
	}
	added := blockLinkRow{blockID: middle, kind: dropped.kind, toKind: dropped.toKind, toID: dropped.toID}
	innerAddress := refKey + "#" + inner

	// The previous release's version 2, written after the backfill: the innermost callout and its
	// paragraph go, and the middle paragraph starts citing CORE-2. Its index pass deletes the
	// dropped blocks' rows and the inner paragraph's link row, and inserts the middle paragraph's
	// link naming no node.
	indexAsThePreviousRelease(t, database, service, document, func(tree *pmdoc.Node) *pmdoc.Node {
		if !cutInnermostHolding(tree, "callout", "Inner of "+slug) {
			t.Fatalf("no callout of %s holds %q, so the edit drops no block", slug, "Inner of "+slug)
		}
		replaceText(tree, "Middle of "+slug+".", "Middle of "+slug+", after dispatch://CORE-2.")
		return tree
	}, []blockLinkRow{dropped}, []blockLinkRow{added})
	if after := holderOf(t, service, document, "dispatch://CORE-2"); after != middle {
		t.Fatalf("the edit moved the mention to block %s, want the middle paragraph %s: the room minted new ids and this test proves nothing", after, middle)
	}
	if rows := blockRowsOf(t, database, document, inner); rows != 0 {
		t.Fatalf("the inner paragraph keeps %d row(s) after the previous release's version, want its row dropped: the edit dropped nothing and this test proves nothing", rows)
	}
	if state := nodeState(t, database, innerAddress); state != graphstore.StateLive {
		t.Fatalf("the inner paragraph's node is %q after the previous release dropped it, want it left live, as that release leaves it", state)
	}
	var marked int
	if err := database.Pool.QueryRow(ctx,
		`select count(*) from document_block_versions where artifact_id = $1 and store_owed_at is not null`, document).Scan(&marked); err != nil || marked != 1 {
		t.Fatalf("the document holds the backfill's mark %d time(s) (%v) after the previous release's version, want it kept", marked, err)
	}

	// This build serves again and drains the mark.
	drainBlockIndex(t, service, "after the previous release's version")

	middleAddress := refKey + "#" + middle
	var named bool
	if err := database.Pool.QueryRow(ctx, `
		select node_kind is not null from block_refs
		where artifact_id = $1 and block_id = $2 and kind = $3 and to_kind = $4 and to_id = $5
	`, document, middle, added.kind, added.toKind, added.toID).Scan(&named); err != nil {
		t.Fatalf("read the middle paragraph's link row: %v", err)
	}
	if !named {
		t.Errorf("the block_refs row of %s %s %s:%s names no node after this build drained the backfill's mark",
			middleAddress, added.kind, added.toKind, added.toID)
	}
	held, err := graphstore.CurrentOut(ctx, database.Pool, refs.BlockNodeKind, middleAddress, []string{added.kind})
	if err != nil {
		t.Fatalf("read the links the store states out of %s: %v", middleAddress, err)
	}
	if len(held) != 1 || held[0].ToKind != added.toKind || held[0].ToID != added.toID {
		t.Errorf("the store states %#v out of %s, want the %s %s:%s the version holds", held, middleAddress, added.kind, added.toKind, added.toID)
	}
	requireDroppedBlockGone(t, database, innerAddress, dropped)
	census := takeStoreCensus(t, database)
	for _, kind := range census.Kinds {
		for _, difference := range append(append([]graphcensus.Difference{}, kind.ReportedRows...), kind.ReportedNodes...) {
			t.Errorf("the census reads a difference of kind %s at %s: %s", kind.Kind, difference.At, difference.Cause)
		}
	}
	if census.Refused() {
		t.Fatalf("the census refused with %d difference(s) after this build drained the backfill's mark over a link the previous release added and a block it dropped, want zero",
			census.Differences())
	}
}

// cutInnermostHolding removes from the tree under node the innermost block of type kind whose
// text holds words, and reports whether it removed one.
func cutInnermostHolding(node *pmdoc.Node, kind, words string) bool {
	for index, child := range node.Children {
		if !nodeHoldsText(child, words) {
			continue
		}
		if cutInnermostHolding(child, kind, words) {
			return true
		}
		if child.Type == kind {
			node.Children = slices.Delete(node.Children, index, index+1)
			return true
		}
		return false
	}
	return false
}

// blockRowsOf is how many document_blocks rows document holds of block.
func blockRowsOf(t *testing.T, database *store.Store, document, block string) int {
	t.Helper()
	var rows int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from document_blocks where artifact_id = $1 and block_id = $2
	`, document, block).Scan(&rows); err != nil {
		t.Fatalf("count the rows of block %s in %s: %v", block, document, err)
	}
	return rows
}

// requireDroppedBlockGone holds the block at address, which a version of the release before this
// one dropped, to what this build's drain of its document states of it: its node gone, stated by
// a state tuple as a block a version drops is, and link, the link the block held, retracted.
func requireDroppedBlockGone(t *testing.T, database *store.Store, address string, link blockLinkRow) {
	t.Helper()
	ctx := context.Background()
	if state := nodeState(t, database, address); state != graphstore.StateGone {
		t.Errorf("the node of the dropped block %s is %q after this build's drain, want it gone: the previous release dropped it", address, state)
	}
	var newestState string
	if err := database.Pool.QueryRow(ctx, `
		select to_id from graph_tuples
		where from_kind = $1 and from_id = $2 and kind = $3 and asserted
		order by seq desc limit 1
	`, refs.BlockNodeKind, address, graphstore.AttributeState).Scan(&newestState); err != nil || newestState != graphstore.StateGone {
		t.Errorf("the newest state the log holds for %s is %q (%v), want %q, as a block a version drops is stated",
			address, newestState, err, graphstore.StateGone)
	}
	var asserted bool
	if err := database.Pool.QueryRow(ctx, `
		select asserted from graph_tuples
		where from_kind = $1 and from_id = $2 and kind = $3 and to_kind = $4 and to_id = $5
		order by seq desc limit 1
	`, refs.BlockNodeKind, address, link.kind, link.toKind, link.toID).Scan(&asserted); err != nil || asserted {
		t.Errorf("the newest tuple of %s %s %s:%s asserts=%t (%v), want the link retracted",
			address, link.kind, link.toKind, link.toID, asserted, err)
	}
}
