package docs

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// A version this build leaves to its block-index reconciliation reaches the store although a pod
// of the release before this one indexes it first, and the census reaches zero.
//
// A version of more blocks than a write indexes in its own request leaves its blocks owed
// (deferBlockIndexBlocks): migration 0122's trigger sets document_block_versions.version null,
// the mark every release reads and drains. During a roll a pod of the release before this one can
// take the block-index lease first and index that version. It writes every block row and every
// link row naming no node and sets the version, and it states nothing in the store, which it
// knows nothing of. Were the null version the write's only mark, this build's reconciliation
// would find nothing owed after that, and the census would refuse every one of those rows for
// naming no node until someone ran the backfill, which nothing tells anyone to do. The write
// therefore sets this build's own mark beside it (store_owed_at, oweDocumentIndex), which that
// release neither reads nor clears, and this build's drain of the mark names every row's node and
// states what each row projects (refs.IndexBlocks).
//
// The test writes such a version through this build's request path, lets the stand-in for the
// previous release's block index (indexVersionAsThePreviousRelease, which
// TestALinkThePreviousReleaseAddsAfterTheBackfillReachesTheStore drives through its version
// write) index it first, and then drives this build's reconciliation until nothing is owed.
func TestADeferredVersionThePreviousReleaseIndexesReachesTheStore(t *testing.T) {
	t.Parallel()
	database, service := blockIndexService(t)
	ctx := context.Background()

	// Rows of every other kind the census reads, as TestTheBlockParentCensusReachesZeroAfterTheBackfill
	// seeds them, so the census can pass only on agreement and never refuses an empty kind here.
	// The backfill gives them their nodes before the document exists, so it marks nothing of the
	// document this test is about.
	if _, err := database.Pool.Exec(ctx, `
		insert into comments (issue_key, author, body) values
			('CORE-1', '{"kind":"user","id":"Alice"}', 'A remark written before the store existed.'),
			('CORE-1', '{"kind":"session","id":"previous-release-deferred-session"}', 'A session''s remark written before the store existed.');
		update issues set route = 'role:previous-release-deferred-role' where key = 'CORE-1';
	`); err != nil {
		t.Fatalf("seed the comments and the route: %v", err)
	}
	if _, err := graphcensus.Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("backfill the comments and the route: %v", err)
	}

	// This build writes a version it defers: the request indexes the version's references and
	// leaves its blocks to the reconciliation, under both marks.
	document := writeDocumentThroughTheRequestPath(t, service, database, "deferred", deferredList(deferredItems))
	settle(t, database, document)
	if rows := countRows(t, database, "document_blocks", document); rows != 0 {
		t.Fatalf("the request wrote %d block rows, want none: the document is past the in-request bound and this test proves nothing", rows)
	}
	var owed, marked bool
	if err := database.Pool.QueryRow(ctx, `
		select version is null, store_owed_at is not null from document_block_versions where artifact_id = $1
	`, document).Scan(&owed, &marked); err != nil {
		t.Fatalf("read the marks the request left: %v", err)
	}
	if !owed {
		t.Fatalf("the request left the document's version standing, want it owed a block index")
	}
	if !marked {
		t.Errorf("the request that deferred the version left it without this build's own mark (store_owed_at), which the release before this one cannot drain")
	}

	// The previous release's block index takes the lease first and indexes the version, writing
	// its rows with no node, as it does for a version some build left owed.
	markdown, blocks, err := service.TextWithBlocks(ctx, serverReader, document)
	if err != nil {
		t.Fatalf("read the deferred document: %v", err)
	}
	var version int
	var versionAt time.Time
	if err := database.Pool.QueryRow(ctx, `
		select number, created_at from artifact_versions where artifact_id = $1 order by number desc limit 1
	`, document).Scan(&version, &versionAt); err != nil {
		t.Fatalf("read the deferred version: %v", err)
	}
	var event int64
	if err := database.Pool.QueryRow(ctx, `
		select id from events where type = 'artifact.created' and payload->'artifact'->>'id' = $1
	`, document).Scan(&event); err != nil {
		t.Fatalf("read the event that recorded the deferred version: %v", err)
	}
	// The version's one link: its last item cites CORE-2, as the document's own refs row says.
	link := blockLinkRow{blockID: holderOf(t, service, document, "dispatch://CORE-2"), kind: "mentions"}
	if err := database.Pool.QueryRow(ctx, `
		select to_kind, to_id from refs where from_kind = 'artifact' and from_id = $1
	`, document).Scan(&link.toKind, &link.toID); err != nil {
		t.Fatalf("read the reference the request wrote: %v", err)
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	indexVersionAsThePreviousRelease(t, tx, document, version, versionAt, event, markdown, blocks, nil, []blockLinkRow{link})
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// What the previous release left: every row naming no node, and the version settled.
	unnamed := func() (blocks, links int) {
		t.Helper()
		if err := database.Pool.QueryRow(ctx, `
			select (select count(*) from document_blocks where artifact_id = $1 and node_kind is null),
			       (select count(*) from block_refs where artifact_id = $1 and node_kind is null)
		`, document).Scan(&blocks, &links); err != nil {
			t.Fatalf("count the rows of the document that name no node: %v", err)
		}
		return blocks, links
	}
	if blocks, links := unnamed(); blocks != deferredItems+1 || links != 1 {
		t.Fatalf("the previous release left %d block row(s) and %d link row(s) naming no node, want all %d blocks and the one link",
			blocks, links, deferredItems+1)
	}
	if stands := blockIndexAt(t, database, document); stands == nil || *stands != version {
		t.Fatalf("the previous release left the document's blocks standing at %v, want version %d", stands, version)
	}
	if err := database.Pool.QueryRow(ctx, `
		select store_owed_at is not null from document_block_versions where artifact_id = $1
	`, document).Scan(&marked); err != nil {
		t.Fatalf("read this build's mark after the previous release's index: %v", err)
	}
	if !marked {
		t.Errorf("the document holds no mark of this build's after the previous release indexed the version, want the mark it cannot clear kept")
	}
	if before := takeStoreCensus(t, database); !before.Refused() {
		t.Fatalf("the census passes over the rows the previous release left, so this test proves nothing")
	}

	// This build's reconciliation runs as it does on every tick of every pod of it.
	drainBlockIndex(t, service, "after the previous release indexed the deferred version")

	if blocks, links := unnamed(); blocks != 0 || links != 0 {
		t.Errorf("%d block row(s) and %d link row(s) of the document name no node after this build drained its mark, want none",
			blocks, links)
	}
	var address string
	if err := database.Pool.QueryRow(ctx, `select ref_key || '#' || $2 from artifacts where id = $1`, document, link.blockID).Scan(&address); err != nil {
		t.Fatalf("read the address of the block holding the link: %v", err)
	}
	held, err := graphstore.CurrentOut(ctx, database.Pool, refs.BlockNodeKind, address, []string{link.kind})
	if err != nil {
		t.Fatalf("read the links the store states out of %s: %v", address, err)
	}
	if len(held) != 1 || held[0].ToKind != link.toKind || held[0].ToID != link.toID {
		t.Errorf("the store states %#v out of %s, want the %s %s:%s the version holds", held, address, link.kind, link.toKind, link.toID)
	}
	census := takeStoreCensus(t, database)
	for _, kind := range census.Kinds {
		for _, difference := range append(append([]graphcensus.Difference{}, kind.ReportedRows...), kind.ReportedNodes...) {
			t.Errorf("the census reads a difference of kind %s at %s: %s", kind.Kind, difference.At, difference.Cause)
		}
	}
	if census.Refused() {
		t.Fatalf("the census refused with %d difference(s) after this build drained a version the previous release indexed first, want zero:\n%s",
			census.Differences(), censusFindings(census))
	}
}
