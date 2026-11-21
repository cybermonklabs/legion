package docs

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A version the release before this one writes during a roll and leaves owed an index reaches the
// store when this build's tick reads its event while the index is still owed and that release's
// own block-index reconciliation indexes the version before this build's drain reaches the
// document, and the census reaches zero with no second backfill.
//
// That release defers the index of a version as this build does (deferBlockIndexBlocks): it
// records the version and its artifact.version event, migration 0122's trigger sets
// document_block_versions.version null, the debt every release reads and drains, and it indexes
// nothing yet. This build's tick reads the event while the version is still null and marks the
// document with this build's own mark (markVersionedDocuments): a null version is not the version
// this build's store stated. Before this build's drain reaches the document, that release's
// block-index reconciliation indexes the version itself: it deletes the row of the link the
// version moves off the inner paragraph, inserts the row of the link it moves onto the middle one
// naming no node, sets the version and records it indexed, and never touches store_owed_at. The
// version is then non-null, a document whose version is non-null is owed this build's pass only
// through the mark (owedDocuments, refs.OwedBlockIndexDebts), and the event is already behind this
// build's cursor. Were a null version not marked, nothing would bring this build back to the
// document: the link row would keep naming no node, the store would go on asserting the link the
// version dropped, and the census would refuse both until someone ran the backfill again, which
// nothing tells anyone to do.
//
// The test puts that release's index between this build's read of the event and its drain: it
// runs the tick's first step on its own (readEventsAfterCursor, which commits its mark), has the
// stand-in for that release's block index index the deferred version
// (indexVersionAsThePreviousRelease), and then runs this build's ticks while that release serves
// and once it has left.
func TestADeferredVersionThePreviousReleaseIndexesAfterThisBuildsTickReadItReachesTheStore(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()

	appendIssueEvent(t, database)
	const slug = "deferred-after-read"
	document := createOwedDocument(t, database, service, "CORE-1", "", slug, nestedSpec(slug), "")
	settle(t, database, document)
	// Rows of every other kind the census reads, as TestAVersionThePreviousReleaseWritesAfterTheDrainReachesTheStore
	// seeds them, so the census can pass only on agreement and never refuses an empty kind here.
	if _, err := database.Pool.Exec(ctx, `
		insert into comments (issue_key, author, body) values
			('CORE-1', '{"kind":"user","id":"Alice"}', 'A remark written before the store existed.'),
			('CORE-1', '{"kind":"session","id":"previous-release-deferred-after-read-session"}', 'A session''s remark written before the store existed.');
		update issues set route = 'role:previous-release-deferred-after-read-role' where key = 'CORE-1';
	`); err != nil {
		t.Fatalf("seed the comments and the route: %v", err)
	}
	drainBlockIndex(t, service, "seed")
	takeTheStoreOff(t, database)
	if _, err := graphcensus.Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("backfill the store: %v", err)
	}
	drainBlockIndex(t, service, "the backfill's mark")
	requireCensusReadsZero(t, database, "after this build drained the backfill's mark, before any version of the previous release")
	awaitedBlockIndexTick(t, service)

	// A roll: a process of this build and one of the release before serve side by side.
	registerProcess(t, database, "")
	previousLeaves := registerProcess(t, database, previousReleaseCommit)

	// The release before this one writes a version that moves the mention onto the middle
	// paragraph and leaves its index owed.
	deferred := deferAsThePreviousRelease(t, database, service, document, slug)
	if stands := blockIndexAt(t, database, document); stands != nil {
		t.Fatalf("the previous release's deferred version left the blocks of %s standing at %d, want them owed an index: this test proves nothing", document, *stands)
	}
	if marked := markedDocuments(t, database); marked != 0 {
		t.Fatalf("%d document(s) hold this build's mark after the previous release's deferred version, want none: this test proves nothing", marked)
	}

	// This build's tick reads the version's event while the version is still owed an index: the
	// tick's first step on its own, once the safe horizon has reached the event.
	horizon, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if _, err := service.blockIndexHorizon().Await(horizon); err != nil {
		t.Fatalf("await the event log's safe horizon: %v", err)
	}
	var tick BlockIndexTick
	if _, _, err := service.readEventsAfterCursor(ctx, &tick); err != nil {
		t.Fatalf("read the events after this build's cursor: %v", err)
	}
	t.Logf("the read of the events after this build's cursor read %d event(s), marked %d document(s), another build %t",
		tick.Events, tick.Marked, tick.AnotherBuild)
	var marked bool
	if err := database.Pool.QueryRow(ctx, `
		select store_owed_at is not null from document_block_versions where artifact_id = $1
	`, document).Scan(&marked); err != nil {
		t.Fatalf("read this build's mark on document %s: %v", document, err)
	}
	if !marked {
		t.Errorf("document %s holds no mark of this build's after its tick read the event of a version still owed an index, want it marked", document)
	}

	// Before this build's drain reaches the document, the previous release's block-index
	// reconciliation indexes the deferred version itself.
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	indexVersionAsThePreviousRelease(t, tx, document, deferred.number, deferred.at, deferred.event, deferred.markdown, deferred.blocks,
		[]blockLinkRow{deferred.dropped}, []blockLinkRow{deferred.added})
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if stands := blockIndexAt(t, database, document); stands == nil || *stands != deferred.number {
		t.Fatalf("the previous release's index left the blocks of %s standing at %v, want version %d: this test proves nothing", document, stands, deferred.number)
	}
	if blocks, links := rowsNamingNoNode(t, database, document); links == 0 {
		t.Fatalf("the previous release's link row of %s names its node (%d block row(s) name none), so this test proves nothing", document, blocks)
	}

	// This build's ticks while the previous release serves, then once it has left.
	awaitedBlockIndexTick(t, service)
	drainBlockIndex(t, service, "after the previous release indexed the deferred version")
	previousLeaves()
	awaitedBlockIndexTick(t, service)
	drainBlockIndex(t, service, "after the previous release left")

	requireVersionInTheStore(t, database, document, deferred.added)
	var droppedAddress string
	if err := database.Pool.QueryRow(ctx, `select ref_key || '#' || $2 from artifacts where id = $1`, document, deferred.dropped.blockID).Scan(&droppedAddress); err != nil {
		t.Fatalf("read the address of the block the link moved off: %v", err)
	}
	var asserted bool
	if err := database.Pool.QueryRow(ctx, `
		select asserted from graph_tuples
		where from_kind = $1 and from_id = $2 and kind = $3 and to_kind = $4 and to_id = $5
		order by seq desc limit 1
	`, refs.BlockNodeKind, droppedAddress, deferred.dropped.kind, deferred.dropped.toKind, deferred.dropped.toID).Scan(&asserted); err != nil || asserted {
		t.Errorf("the newest tuple of %s %s %s:%s asserts=%t (%v), want the link the version moved off retracted",
			droppedAddress, deferred.dropped.kind, deferred.dropped.toKind, deferred.dropped.toID, asserted, err)
	}
	requireCensusReadsZero(t, database, "after the previous release indexed a version this build's tick had read while it was owed, and this build ticked, with no second backfill")
}

// deferredVersion is a version the stand-in for the release before this one wrote and left owed
// an index (deferAsThePreviousRelease): its number, its time and the event that recorded it, its
// render, and the link it moves, dropped off the inner paragraph and added onto the middle one.
type deferredVersion struct {
	number         int
	at             time.Time
	event          int64
	markdown       string
	blocks         []model.ArtifactBlock
	dropped, added blockLinkRow
}

// deferAsThePreviousRelease has the stand-in for the release before this one write the next
// version of document slug, a nestedSpec whose blocks this build has stated, and leave its index
// owed: the inner paragraph's mention of CORE-2 moves onto the middle paragraph, as
// moveLinkAsThePreviousRelease moves it, the edit reaches the store through the room, and the
// settlement writes the version and its artifact.version event and clears the pending settlement
// (settleVersionAsThePreviousRelease). It indexes no block: migration 0122's trigger leaves the
// version null, and nothing here touches store_owed_at. That release's version write indexes the
// version's own text with the whole grammar all the same (refs.SettleOwed), so the row the
// version's insert owes the re-index queue is settled here, as moveLinkAsThePreviousRelease
// settles it, and the version's event is the only way left for this build to reach the document.
func deferAsThePreviousRelease(t *testing.T, database *store.Store, service *Service, document, slug string) deferredVersion {
	t.Helper()
	ctx := context.Background()
	innerWords, middleWords := "Inner of "+slug, "Middle of "+slug
	inner := holderOf(t, service, document, innerWords)
	middle := holderOf(t, service, document, middleWords)
	deferred := deferredVersion{dropped: blockLinkRow{blockID: inner}}
	if err := database.Pool.QueryRow(ctx, `
		select kind, to_kind, to_id from block_refs where artifact_id = $1 and block_id = $2
	`, document, inner).Scan(&deferred.dropped.kind, &deferred.dropped.toKind, &deferred.dropped.toID); err != nil {
		t.Fatalf("read the inner paragraph's link in %s: %v", slug, err)
	}
	deferred.added = blockLinkRow{blockID: middle, kind: deferred.dropped.kind, toKind: deferred.dropped.toKind, toID: deferred.dropped.toID}
	var mention string
	editLiveTree(t, service, document, func(tree *pmdoc.Node) *pmdoc.Node {
		mention = cutEnding(tree, innerWords)
		replaceText(tree, middleWords+".", middleWords+mention+".")
		return tree
	})
	if mention == "" {
		t.Fatalf("no text of %s starts with %q, so the edit moved nothing", slug, innerWords)
	}
	waitForLandedAppends(t, service, document)
	if holder := holderOf(t, service, document, mention); holder != middle {
		t.Fatalf("the edit moved the mention of %s to block %s, want the middle paragraph %s: the room minted new ids and this test proves nothing",
			slug, holder, middle)
	}
	var err error
	if deferred.markdown, deferred.blocks, err = service.TextWithBlocks(ctx, document); err != nil {
		t.Fatalf("read the edited document %s: %v", slug, err)
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	deferred.number, deferred.at, deferred.event = settleVersionAsThePreviousRelease(t, tx, document, deferred.markdown)
	if _, err := tx.Exec(ctx, `delete from refs_grammar_owed where kind = 'artifact' and id = $1::uuid`, document); err != nil {
		t.Fatalf("settle the re-index queue's row of %s as the previous release's version write does: %v", slug, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return deferred
}
