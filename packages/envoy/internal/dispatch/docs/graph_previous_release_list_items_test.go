package docs

import (
	"context"
	"io"
	"maps"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A list document this build has indexed keeps its list items' rows and nodes through a roll in
// which the release before this one indexes a version of it, and the census reaches zero with no
// second backfill.
//
// R7 of the issue-tree spec nests a moved block under a list item, so this build's renderer records
// each list item's offset (renderer.list in pmdoc/render.go) and its pass gives each item a row and
// a node. The release before records no list item's offset, so its render holds no list item. Its
// index pass of a version deletes the row of each block its render drops, the list items among
// them, and states each such block's node gone (main's refs/blocks.go: "A block the version no
// longer holds is deleted, its node's state stated gone"). It never sets this build's mark. This
// build's next tick reads that version's event, marks the document, since no pass of its rule has
// stated it at that version (markVersionedDocuments), and indexes it again: each list item's row
// comes back naming its node, and each node comes back live.
//
// The test indexes a listSpec with this build, records the fleet as two builds, has the stand-in
// for the release before write and index a version that rewords the lead paragraph
// (reindexListAsThePreviousRelease), and runs this build's next tick.
func TestAListDocumentThePreviousReleaseIndexesGetsItsListItemsBackAtTheNextTick(t *testing.T) {
	t.Parallel()
	database, service := blockIndexService(t)
	ctx := context.Background()

	appendIssueEvent(t, database)
	const slug = "roll-list"
	document := createOwedDocument(t, database, service, "CORE-1", "", slug, listSpec(slug), "")
	settle(t, database, document)
	// Rows of every other kind the census reads, as TestAVersionThePreviousReleaseWritesAfterTheDrainReachesTheStore
	// seeds them, so the census can pass only on agreement and never refuses an empty kind here.
	if _, err := database.Pool.Exec(ctx, `
		insert into comments (issue_key, author, body) values
			('CORE-1', '{"kind":"user","id":"Alice"}', 'A remark written before the store existed.'),
			('CORE-1', '{"kind":"session","id":"previous-release-list-session"}', 'A session''s remark written before the store existed.');
		update issues set route = 'role:previous-release-list-role' where key = 'CORE-1';
	`); err != nil {
		t.Fatalf("seed the comments and the route: %v", err)
	}
	drainBlockIndex(t, service, "seed")
	takeTheStoreOff(t, database)
	if _, err := graphcensus.Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("backfill the store: %v", err)
	}
	drainBlockIndex(t, service, "the backfill's mark")
	requireCensusReadsZero(t, database, "after this build drained the backfill's mark, before the previous release's version")

	// This build's index: the lead paragraph, the outer and the nested list, the four list items
	// and the paragraph of each item, every row naming a live node.
	indexed := map[string]int{"paragraph": 5, "bullet_list": 2, "list_item": 4}
	if types := blockRowTypes(t, database, document); !maps.Equal(types, indexed) {
		t.Fatalf("this build indexed block rows of %v by type, want %v: this test proves nothing", types, indexed)
	}
	items := listItemAddresses(t, database, document)
	for _, address := range items {
		if state := nodeState(t, database, address); state != graphstore.StateLive {
			t.Fatalf("this build left the node of list item %s %q, want it live: this test proves nothing", address, state)
		}
	}
	awaitedBlockIndexTick(t, service)
	cursor := blockIndexEventCursor(t, database, "")

	// A roll: a process of this build and one of the release before serve side by side, and the
	// release before indexes a version of the document.
	registerProcess(t, database, "")
	registerProcess(t, database, previousReleaseCommit)
	version, event := reindexListAsThePreviousRelease(t, database, service, document, slug, items)
	if types, want := blockRowTypes(t, database, document), map[string]int{"paragraph": 5, "bullet_list": 2}; !maps.Equal(types, want) {
		t.Fatalf("the previous release left block rows of %v by type, want %v: no list item's: this test proves nothing", types, want)
	}
	for _, address := range items {
		if state := nodeState(t, database, address); state != graphstore.StateGone {
			t.Fatalf("the previous release left the node of list item %s %q, want it gone: this test proves nothing", address, state)
		}
	}
	if marked := markedDocuments(t, database); marked != 0 {
		t.Fatalf("%d document(s) hold this build's mark after the previous release's version, want none: this test proves nothing", marked)
	}

	// This build's next tick marks the document from the version's event and indexes it again.
	pending := countEventsAfter(t, database, cursor)
	tick := awaitedBlockIndexTick(t, service)
	if !tick.AnotherBuild || pending == 0 || tick.Events != pending || tick.Marked != 1 || tick.Documents != 1 {
		t.Errorf("the tick with another build serving read %d event(s), marked %d document(s), indexed %d and found another build %t; want the %d event(s) after the cursor read, the document marked and indexed, and another build found",
			tick.Events, tick.Marked, tick.Documents, tick.AnotherBuild, pending)
	}
	if read := blockIndexEventCursor(t, database, ""); read < event {
		t.Errorf("the cursor stands at %d after the tick, want it at or past the previous release's version event %d", read, event)
	}
	if stands := blockIndexAt(t, database, document); stands == nil || *stands != version {
		t.Errorf("the tick left the document's blocks standing at %v, want the previous release's version %d", stands, version)
	}
	types := blockRowTypes(t, database, document)
	t.Logf("after this build's tick the document holds block rows of %v by type", types)
	if !maps.Equal(types, indexed) {
		t.Errorf("after this build's tick the document holds block rows of %v by type, want %v: each list item's row back", types, indexed)
	}
	if again := listItemAddresses(t, database, document); !maps.Equal(addressSet(again), addressSet(items)) {
		t.Errorf("after this build's tick the list items' rows name %v, want the nodes they named before the roll, %v", again, items)
	}
	for _, address := range items {
		if state := nodeState(t, database, address); state != graphstore.StateLive {
			t.Errorf("after this build's tick the node of list item %s is %q, want it live again", address, state)
		}
	}
	if blocks, links := rowsNamingNoNode(t, database, document); blocks+links != 0 {
		t.Errorf("after this build's tick %d block row(s) and %d link row(s) of the document name no node, want none", blocks, links)
	}
	requireCensusReadsZero(t, database, "after this build's tick over the version the previous release indexed, with no second backfill")
}

// listSpec is a document of a paragraph and a list whose second item holds a list of its own, so
// one list item stands under another, and whose last item cites CORE-2: eleven blocks, four of
// them list items.
func listSpec(slug string) string {
	return "Lead of " + slug + ".\n\n" +
		"- First item of " + slug + ".\n" +
		"- Second item of " + slug + ".\n" +
		"  - Nested item of " + slug + ".\n" +
		"- Third item of " + slug + ", after dispatch://CORE-2.\n"
}

// listItemAddresses is the node address each list item row of the document names, in block id
// order.
func listItemAddresses(t *testing.T, database *store.Store, document string) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select coalesce(node_address, '') from document_blocks
		where artifact_id = $1 and type = 'list_item' order by block_id
	`, document)
	if err != nil {
		t.Fatalf("read the list items of %s: %v", document, err)
	}
	defer rows.Close()
	var addresses []string
	for rows.Next() {
		var address string
		if err := rows.Scan(&address); err != nil {
			t.Fatalf("scan a list item of %s: %v", document, err)
		}
		if address == "" {
			t.Fatalf("a list item row of %s names no node", document)
		}
		addresses = append(addresses, address)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the list items of %s: %v", document, err)
	}
	return addresses
}

// addressSet is addresses as a set.
func addressSet(addresses []string) map[string]bool {
	set := make(map[string]bool, len(addresses))
	for _, address := range addresses {
		set[address] = true
	}
	return set
}

// reindexListAsThePreviousRelease has the stand-in for the release before this one write and
// index the next version of document slug, a listSpec this build has indexed, as main's pass
// indexes a version: the lead paragraph is worded again through the room, the settlement writes
// the version and its artifact.version event (settleVersionAsThePreviousRelease), and the index
// pass, whose render holds no list item, rewrites the lead paragraph's row and deletes each list
// item's row (indexVersionAsThePreviousRelease). In the same transaction it states the node of each
// list item of items gone under the version's event, as main's pass states the node of each block
// its render drops gone, and nothing beneath it. That release's version write settles the row the
// version's insert owes the re-index queue (moveLinkAsThePreviousRelease), so it is settled here
// too, and the version's event is the only way left for this build to reach the document. It
// answers the version and its event.
func reindexListAsThePreviousRelease(t *testing.T, database *store.Store, service *Service, document, slug string, items []string) (int, int64) {
	t.Helper()
	ctx := context.Background()
	lead, worded := "Lead of "+slug+".", "Lead of "+slug+", worded again."
	editLiveTree(t, service, document, func(tree *pmdoc.Node) *pmdoc.Node {
		replaceText(tree, lead, worded)
		return tree
	})
	waitForLandedAppends(t, service, document)
	markdown, blocks, err := service.TextWithBlocks(ctx, document)
	if err != nil {
		t.Fatalf("read the edited document %s: %v", slug, err)
	}
	if !strings.Contains(markdown, worded) {
		t.Fatalf("the edit of %s did not word the lead paragraph again: %q", slug, markdown)
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	version, versionAt, event := settleVersionAsThePreviousRelease(t, tx, document, markdown)
	indexVersionAsThePreviousRelease(t, tx, document, version, versionAt, event, markdown, blocks, nil, nil)
	writer := model.Actor{Kind: "user", ID: "alice"}
	gone := make([]graphstore.Node, 0, len(items))
	for _, address := range items {
		node, held, err := graphstore.ReadNode(ctx, tx, refs.BlockNodeKind, address)
		if err != nil || !held {
			t.Fatalf("read the node of list item %s (%t): %v", address, held, err)
		}
		node.State, node.Restates, node.KeepsCause, node.Changed = graphstore.StateGone, false, true, true
		node.MadeAt, node.MadeBy, node.SourceEvent = versionAt, writer, event
		gone = append(gone, node)
	}
	if err := graphstore.Write(ctx, tx, gone, nil); err != nil {
		t.Fatalf("state the list items' nodes gone as the previous release's pass does: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `
		delete from refs_grammar_owed where kind = 'artifact' and id = $1::uuid
	`, document); err != nil {
		t.Fatalf("settle the re-index queue's row of %s as the previous release's version write does: %v", slug, err)
	}
	return version, event
}
