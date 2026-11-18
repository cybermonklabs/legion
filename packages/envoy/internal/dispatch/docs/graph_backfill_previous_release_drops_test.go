package docs

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// blockLinkRow is one block_refs row as the stand-in for the previous release writes or deletes
// it: the block it is a link out of, and the pair's kind and far end.
type blockLinkRow struct {
	blockID, kind, toKind, toID string
}

// A block and a link the release before this one dropped are gone from the store once the
// backfill has run, and the census reaches zero.
//
// That release indexes a version of a document this build's store has stated as it always has:
// it deletes the document_blocks row of each block the version drops and the block_refs row of
// each link a block stops holding, writes the version it indexed, and knows no store, so it states
// no node gone and retracts no tuple. The dropped block's node stays live with nothing projecting
// it, and the dropped link stays asserted out of a live block with no row naming it: two census
// differences that no later write of the rows reaches, since the rows are gone and the document's
// version already stands where that release left it.
//
// This test stands in for that release with its writes alone (indexAsThePreviousRelease): the
// edit reaches the store through the room, the version and its event are written, and its index
// pass's writes follow - every changed block's row rewritten to the new text, the dropped block's
// row deleted, the dropped link's row deleted and the link the version adds inserted naming no
// node, the texts and the indexed version recorded, and the document's version set - with no write
// of the store. The backfill then runs, and must mark the document, whose rows that release
// indexed at a version the store never stated, and this build's reconciliation runs after it, as
// it does on every tick of every pod of it. The dropped block's node must then be gone, stated by
// a state tuple as a block a version drops is, the dropped link must be retracted, and the census
// must read no difference of any kind.
func TestTheBlockAndTheLinkThePreviousReleaseDroppedAreGoneAfterTheBackfill(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()

	const slug = "dropped"
	document := createOwedDocument(t, database, service, "CORE-1", "", slug, nestedSpec(slug), "")
	settle(t, database, document)
	// Rows of every other kind the census reads, as TestTheBlockParentCensusReachesZeroAfterTheBackfill
	// seeds them, so the census can pass only on agreement and never refuses an empty kind here.
	if _, err := database.Pool.Exec(ctx, `
		insert into comments (issue_key, author, body) values
			('CORE-1', '{"kind":"user","id":"Alice"}', 'A remark written before the store existed.'),
			('CORE-1', '{"kind":"session","id":"previous-release-drop-session"}', 'A session''s remark written before the store existed.');
		update issues set route = 'role:previous-release-drop-role' where key = 'CORE-1';
	`); err != nil {
		t.Fatalf("seed the comments and the route: %v", err)
	}
	// This build indexes version 1, so every row of the document names its node and the store
	// states each block, its parent and the inner paragraph's mention of CORE-2.
	drainBlockIndex(t, service, "version 1")
	var refKey string
	if err := database.Pool.QueryRow(ctx, `select ref_key from artifacts where id = $1`, document).Scan(&refKey); err != nil {
		t.Fatalf("read the document's ref_key: %v", err)
	}
	tail := holderOf(t, service, document, "Tail of "+slug)
	inner := holderOf(t, service, document, "dispatch://CORE-2")
	middle := holderOf(t, service, document, "Middle of "+slug)
	tailAddress, innerAddress := refKey+"#"+tail, refKey+"#"+inner
	dropped := blockLinkRow{blockID: inner}
	var named bool
	if err := database.Pool.QueryRow(ctx, `
		select kind, to_kind, to_id, node_kind is not null from block_refs
		where artifact_id = $1 and block_id = $2
	`, document, inner).Scan(&dropped.kind, &dropped.toKind, &dropped.toID, &named); err != nil || !named {
		t.Fatalf("read the inner paragraph's link: named=%t, %v; want one row naming its node", named, err)
	}
	added := blockLinkRow{blockID: middle, kind: dropped.kind, toKind: dropped.toKind, toID: dropped.toID}

	// The previous release's version 2: the tail paragraph goes, the inner paragraph stops
	// citing CORE-2 and the middle one starts.
	indexAsThePreviousRelease(t, database, service, document, func(tree *pmdoc.Node) *pmdoc.Node {
		kept := tree.Children[:0]
		for _, child := range tree.Children {
			if !nodeHoldsText(child, "Tail of "+slug) {
				kept = append(kept, child)
			}
		}
		tree.Children = kept
		replaceText(tree, "Inner of "+slug+", after dispatch://CORE-2.", "Inner of "+slug+".")
		replaceText(tree, "Middle of "+slug+".", "Middle of "+slug+", after dispatch://CORE-2.")
		return tree
	}, dropped, added)
	if after := holderOf(t, service, document, "dispatch://CORE-2"); after != middle {
		t.Fatalf("the edit moved the mention to block %s, want the middle paragraph %s: the room minted new ids and this test proves nothing", after, middle)
	}
	if after := holderOf(t, service, document, "Inner of "+slug); after != inner {
		t.Fatalf("the edit gave the inner paragraph id %s, want %s kept", after, inner)
	}

	// What the previous release left: the tail's node live with no row, and the inner
	// paragraph's link asserted with no row. The census says so before the backfill.
	if state := nodeState(t, database, tailAddress); state != graphstore.StateLive {
		t.Fatalf("the tail paragraph's node is %q after the previous release dropped it, want it left live", state)
	}
	if held, err := graphstore.CurrentOut(ctx, database.Pool, refs.BlockNodeKind, innerAddress, []string{dropped.kind}); err != nil || len(held) != 1 {
		t.Fatalf("the inner paragraph states %#v (%v) after the previous release dropped its link, want the link left asserted", held, err)
	}
	if before := takeStoreCensus(t, database); !before.Refused() {
		t.Fatalf("the census passes over what the previous release left, so this test proves nothing")
	}

	report, err := graphcensus.Backfill(ctx, database, io.Discard)
	if err != nil {
		t.Fatalf("backfill the store: %v", err)
	}
	if links := backfilledKind(t, report, "block_ref"); links.Rows != 1 || !links.Complete {
		t.Fatalf("the backfill wrote %#v for the block references, want the one link the previous release added", links)
	}
	if blocks := backfilledKind(t, report, refs.BlockNodeKind); blocks.Rows != 0 || blocks.Owed != 1 {
		t.Errorf("the backfill wrote %#v for the blocks, want no row, every one names its node, and the one document the previous release indexed marked",
			blocks)
	}
	drainBlockIndex(t, service, "after the backfill")

	if state := nodeState(t, database, tailAddress); state != graphstore.StateGone {
		t.Errorf("the tail paragraph's node is %q after the backfill, want it gone: the previous release dropped it", state)
	}
	var newestState string
	if err := database.Pool.QueryRow(ctx, `
		select to_id from graph_tuples
		where from_kind = $1 and from_id = $2 and kind = $3 and asserted
		order by seq desc limit 1
	`, refs.BlockNodeKind, tailAddress, graphstore.AttributeState).Scan(&newestState); err != nil || newestState != graphstore.StateGone {
		t.Errorf("the newest state the log holds for %s is %q (%v), want %q, as a block a version drops is stated",
			tailAddress, newestState, err, graphstore.StateGone)
	}
	var asserted bool
	if err := database.Pool.QueryRow(ctx, `
		select asserted from graph_tuples
		where from_kind = $1 and from_id = $2 and kind = $3 and to_kind = $4 and to_id = $5
		order by seq desc limit 1
	`, refs.BlockNodeKind, innerAddress, dropped.kind, dropped.toKind, dropped.toID).Scan(&asserted); err != nil || asserted {
		t.Errorf("the newest tuple of %s %s %s:%s asserts=%t (%v), want the link retracted",
			innerAddress, dropped.kind, dropped.toKind, dropped.toID, asserted, err)
	}
	census := takeStoreCensus(t, database)
	for _, kind := range census.Kinds {
		for _, difference := range append(append([]graphcensus.Difference{}, kind.ReportedRows...), kind.ReportedNodes...) {
			t.Errorf("the census reads a difference of kind %s at %s: %s", kind.Kind, difference.At, difference.Cause)
		}
	}
	if census.Refused() {
		t.Fatalf("the census refused with %d difference(s) after the backfill over what the previous release dropped, want zero",
			census.Differences())
	}
}

// indexAsThePreviousRelease writes the document's next version as the release before this one
// writes and indexes it, with every write that release makes and none of the store: the edit
// reaches the store through the room, the settlement writes the version and its artifact.version
// event and clears the pending settlement, and the index pass rewrites the row and records the
// text of each block of the render whose text moved, inserts a block the render adds with no node,
// deletes the row of each block the render drops and records it dropped, deletes the link dropped
// and inserts the link added with no node, and records the version indexed and the document's
// blocks standing at it.
func indexAsThePreviousRelease(t *testing.T, database *store.Store, service *Service, artifactID string, edit func(*pmdoc.Node) *pmdoc.Node, dropped, added blockLinkRow) {
	t.Helper()
	ctx := context.Background()
	editLiveTree(t, service, artifactID, edit)
	waitForLandedAppends(t, service, artifactID)
	markdown, blocks, err := service.TextWithBlocks(ctx, artifactID)
	if err != nil {
		t.Fatalf("read the edited document: %v", err)
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var version int
	var versionAt time.Time
	if err := tx.QueryRow(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		select $1, max(number) + 1, $2, '[{"kind":"user","id":"alice"}]' from artifact_versions where artifact_id = $1
		returning number, created_at
	`, artifactID, markdown).Scan(&version, &versionAt); err != nil {
		t.Fatalf("write the version: %v", err)
	}
	var eventID int64
	if err := tx.QueryRow(ctx, `
		insert into events (issue_key, seq, type, actor, payload, notify)
		select 'CORE-1', coalesce(max(seq), 0) + 1, 'artifact.version', '{"kind":"user","id":"alice"}',
		       jsonb_build_object('artifact_id', $1::text, 'name', 'spec.md', 'version', jsonb_build_object('number', $2::int)), false
		from events where issue_key = 'CORE-1'
		returning id
	`, artifactID, version).Scan(&eventID); err != nil {
		t.Fatalf("append the version's event: %v", err)
	}
	if _, err := tx.Exec(ctx, `delete from doc_settlements_pending where artifact_id = $1`, artifactID); err != nil {
		t.Fatalf("settle the version: %v", err)
	}

	rows, err := tx.Query(ctx, `select block_id, text_sha256 from document_blocks where artifact_id = $1`, artifactID)
	if err != nil {
		t.Fatalf("read the document's blocks: %v", err)
	}
	stored := map[string]string{}
	for rows.Next() {
		var id, sum string
		if err := rows.Scan(&id, &sum); err != nil {
			t.Fatalf("scan a block: %v", err)
		}
		stored[id] = sum
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the document's blocks: %v", err)
	}
	seen := map[string]bool{}
	for _, block := range blocks {
		if seen[block.ID] {
			continue
		}
		seen[block.ID] = true
		body := markdown[block.From:block.To]
		sum := refs.BlockTextSum(body)
		previous, held := stored[block.ID]
		delete(stored, block.ID)
		if held && previous == sum {
			continue
		}
		if _, err := tx.Exec(ctx, `
			insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at)
			select $1::uuid, $2, a.ref_key || '#' || $2, $3, $4, $5, $6, $7 from artifacts a where a.id = $1::uuid
			on conflict (artifact_id, block_id) do update set
				type = excluded.type, text_sha256 = excluded.text_sha256, excerpt = excluded.excerpt,
				changed_version = excluded.changed_version, changed_at = excluded.changed_at
		`, artifactID, block.ID, block.Type, sum, text.HeadRunes(body, refs.BlockExcerptRunes), version, versionAt); err != nil {
			t.Fatalf("write block %s as the previous release does: %v", block.ID, err)
		}
		if _, err := tx.Exec(ctx, `
			insert into document_block_texts (artifact_id, block_id, version, text) values ($1, $2, $3, $4)
		`, artifactID, block.ID, version, body); err != nil {
			t.Fatalf("record the text of block %s: %v", block.ID, err)
		}
	}
	for id := range stored {
		if _, err := tx.Exec(ctx, `delete from document_blocks where artifact_id = $1 and block_id = $2`, artifactID, id); err != nil {
			t.Fatalf("delete block %s as the previous release does: %v", id, err)
		}
		if _, err := tx.Exec(ctx, `
			insert into document_block_texts (artifact_id, block_id, version, text) values ($1, $2, $3, null)
		`, artifactID, id, version); err != nil {
			t.Fatalf("record block %s dropped: %v", id, err)
		}
	}
	if tag, err := tx.Exec(ctx, `
		delete from block_refs
		where artifact_id = $1 and block_id = $2 and kind = $3 and to_kind = $4 and to_id = $5
	`, artifactID, dropped.blockID, dropped.kind, dropped.toKind, dropped.toID); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("delete the dropped link as the previous release does: %d rows, %v", tag.RowsAffected(), err)
	}
	if _, err := tx.Exec(ctx, `
		insert into block_refs (artifact_id, block_id, from_node, to_kind, to_id, kind, created_version, created_at, source_seq)
		select $1::uuid, $2, a.ref_key || '#' || $2, $3, $4, $5, $6, $7, $8 from artifacts a where a.id = $1::uuid
	`, artifactID, added.blockID, added.toKind, added.toID, added.kind, version, versionAt, eventID); err != nil {
		t.Fatalf("insert the added link as the previous release does: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into document_indexed_versions (artifact_id, version) values ($1, $2)
	`, artifactID, version); err != nil {
		t.Fatalf("record the version indexed: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into document_block_versions (artifact_id, version, windows_owed_at) values ($1, $2, now())
		on conflict (artifact_id) do update set version = excluded.version,
			windows_owed_at = coalesce(document_block_versions.windows_owed_at, excluded.windows_owed_at)
	`, artifactID, version); err != nil {
		t.Fatalf("record the blocks standing at the version: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// nodeHoldsText is whether any text under node holds want.
func nodeHoldsText(node *pmdoc.Node, want string) bool {
	if node.Type == "text" && strings.Contains(node.Text, want) {
		return true
	}
	for _, child := range node.Children {
		if nodeHoldsText(child, want) {
			return true
		}
	}
	return false
}

// replaceText replaces from with to in the one text under node that holds it, and fails the
// edit's own check later if none does (indexAsThePreviousRelease's caller reads the result back).
func replaceText(node *pmdoc.Node, from, to string) {
	if node.Type == "text" && strings.Contains(node.Text, from) {
		node.Text = strings.Replace(node.Text, from, to, 1)
		return
	}
	for _, child := range node.Children {
		replaceText(child, from, to)
	}
}

// nodeState is the state the store holds the node of block address in, or the empty string
// where it holds no such node.
func nodeState(t *testing.T, database *store.Store, address string) string {
	t.Helper()
	var state string
	if err := database.Pool.QueryRow(context.Background(), `
		select coalesce((select state from graph_nodes where kind = $1 and id = $2), '')
	`, refs.BlockNodeKind, address).Scan(&state); err != nil {
		t.Fatalf("read the node of %s: %v", address, err)
	}
	return state
}
