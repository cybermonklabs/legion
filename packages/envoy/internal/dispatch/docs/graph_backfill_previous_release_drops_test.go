package docs

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// laterBlockState is a state no writer of this release writes: a later release's build
// marks a requirement proposed, and this release serves beside that build during a roll.
const laterBlockState = "proposed"

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
//
// The version drops a second block, the outer callout's own paragraph, which a later release
// marked proposed by then (as that release's build marks a requirement, written here by
// SQL). The index pass keeps a block's stored state unless the pass itself makes the block gone
// (R8), so a block that release dropped is gone whatever state the store held it in: a pass that
// read the store's blocks in state live alone would leave this one proposed for ever, with no row
// projecting it.
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
	outer := holderOf(t, service, document, "Outer of "+slug)
	tailAddress, innerAddress, outerAddress := refKey+"#"+tail, refKey+"#"+inner, refKey+"#"+outer
	dropped := blockLinkRow{blockID: inner}
	var named bool
	if err := database.Pool.QueryRow(ctx, `
		select kind, to_kind, to_id, node_kind is not null from block_refs
		where artifact_id = $1 and block_id = $2
	`, document, inner).Scan(&dropped.kind, &dropped.toKind, &dropped.toID, &named); err != nil || !named {
		t.Fatalf("read the inner paragraph's link: named=%t, %v; want one row naming its node", named, err)
	}
	added := blockLinkRow{blockID: middle, kind: dropped.kind, toKind: dropped.toKind, toID: dropped.toID}

	// A later release marks the outer paragraph proposed: the node row's state and one state
	// tuple, at that build's sentinel cause.
	if _, err := database.Pool.Exec(ctx, `
		with marked as (
			update graph_nodes set state = $3 where kind = $1 and id = $2 returning id
		)
		insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
		select $1, marked.id, $4, $5, $3, now(), '{"kind":"user","id":"bob"}', $6, null, true from marked
	`, refs.BlockNodeKind, outerAddress, laterBlockState, graphstore.AttributeState, graphstore.KindValue, graphstore.NoEvent); err != nil {
		t.Fatalf("mark the outer paragraph %s as a later release does: %v", laterBlockState, err)
	}
	if state := nodeState(t, database, outerAddress); state != laterBlockState {
		t.Fatalf("the outer paragraph's node is %q after the later release marked it, want %q", state, laterBlockState)
	}

	// The previous release's version 2: the tail paragraph and the outer callout's own paragraph
	// go, the inner paragraph stops citing CORE-2 and the middle one starts.
	indexAsThePreviousRelease(t, database, service, document, func(tree *pmdoc.Node) *pmdoc.Node {
		kept := tree.Children[:0]
		for _, child := range tree.Children {
			if !nodeHoldsText(child, "Tail of "+slug) {
				kept = append(kept, child)
			}
		}
		tree.Children = kept
		for _, child := range tree.Children {
			child.Children = slices.DeleteFunc(child.Children, func(grandchild *pmdoc.Node) bool {
				return grandchild.Type == "paragraph" && nodeHoldsText(grandchild, "Outer of "+slug)
			})
		}
		replaceText(tree, "Inner of "+slug+", after dispatch://CORE-2.", "Inner of "+slug+".")
		replaceText(tree, "Middle of "+slug+".", "Middle of "+slug+", after dispatch://CORE-2.")
		return tree
	}, []blockLinkRow{dropped}, []blockLinkRow{added})
	if after := holderOf(t, service, document, "dispatch://CORE-2"); after != middle {
		t.Fatalf("the edit moved the mention to block %s, want the middle paragraph %s: the room minted new ids and this test proves nothing", after, middle)
	}
	if after := holderOf(t, service, document, "Inner of "+slug); after != inner {
		t.Fatalf("the edit gave the inner paragraph id %s, want %s kept", after, inner)
	}

	// What the previous release left: the tail's node live and the outer paragraph's proposed,
	// each with no row, and the inner paragraph's link asserted with no row. The census says so
	// before the backfill.
	if state := nodeState(t, database, tailAddress); state != graphstore.StateLive {
		t.Fatalf("the tail paragraph's node is %q after the previous release dropped it, want it left live", state)
	}
	var outerRows int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from document_blocks where artifact_id = $1 and block_id = $2
	`, document, outer).Scan(&outerRows); err != nil || outerRows != 0 {
		t.Fatalf("the outer paragraph keeps %d row(s) (%v) after the previous release's version, want its row dropped: the edit dropped nothing and this test proves nothing",
			outerRows, err)
	}
	if state := nodeState(t, database, outerAddress); state != laterBlockState {
		t.Fatalf("the outer paragraph's node is %q after the previous release dropped it, want it left %q", state, laterBlockState)
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

	for _, block := range []struct{ name, address string }{
		{"tail paragraph", tailAddress},
		{"outer paragraph a later release marked " + laterBlockState, outerAddress},
	} {
		if state := nodeState(t, database, block.address); state != graphstore.StateGone {
			t.Errorf("the node of the %s is %q after the backfill, want it gone: the previous release dropped it", block.name, state)
		}
		var newestState string
		if err := database.Pool.QueryRow(ctx, `
			select to_id from graph_tuples
			where from_kind = $1 and from_id = $2 and kind = $3 and asserted
			order by seq desc limit 1
		`, refs.BlockNodeKind, block.address, graphstore.AttributeState).Scan(&newestState); err != nil || newestState != graphstore.StateGone {
			t.Errorf("the newest state the log holds for %s is %q (%v), want %q, as a block a version drops is stated",
				block.address, newestState, err, graphstore.StateGone)
		}
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
// event and clears the pending settlement (settleVersionAsThePreviousRelease), and that release's
// index pass then indexes the version in the same transaction (indexVersionAsThePreviousRelease),
// deleting each link of dropped and adding each link of added.
func indexAsThePreviousRelease(t *testing.T, database *store.Store, service *Service, artifactID string, edit func(*pmdoc.Node) *pmdoc.Node, dropped, added []blockLinkRow) {
	t.Helper()
	ctx := context.Background()
	editLiveTree(t, service, artifactID, edit)
	waitForLandedAppends(t, service, artifactID)
	markdown, blocks, err := service.TextWithBlocks(ctx, serverReader, artifactID)
	if err != nil {
		t.Fatalf("read the edited document: %v", err)
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	version, versionAt, eventID := settleVersionAsThePreviousRelease(t, tx, artifactID, markdown)
	indexVersionAsThePreviousRelease(t, tx, artifactID, version, versionAt, eventID, markdown, blocks, dropped, added)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// settleVersionAsThePreviousRelease is the settlement of the release before this one writing the
// document's next version, whose render is markdown, in tx: the version row, its artifact.version
// event, whose seq it takes off the issue's own counter as that release's events.Append does
// (documentEvent), and the pending settlement cleared. It indexes nothing, and answers the
// version's number, the time it was written at and its event.
func settleVersionAsThePreviousRelease(t *testing.T, tx pgx.Tx, artifactID, markdown string) (int, time.Time, int64) {
	t.Helper()
	ctx := context.Background()
	var version int
	var versionAt time.Time
	if err := tx.QueryRow(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		select $1, max(number) + 1, $2, '[{"kind":"user","id":"alice"}]' from artifact_versions where artifact_id = $1
		returning number, created_at
	`, artifactID, markdown).Scan(&version, &versionAt); err != nil {
		t.Fatalf("write the version of %s: %v", artifactID, err)
	}
	event := appendDocumentEvent(t, ctx, tx, "artifact.version", versionEventPayload(t, artifactID, "spec.md", version))
	if _, err := tx.Exec(ctx, `delete from doc_settlements_pending where artifact_id = $1`, artifactID); err != nil {
		t.Fatalf("settle the version of %s: %v", artifactID, err)
	}
	return version, versionAt, event
}

// indexVersionAsThePreviousRelease is the index pass of the release before this one over version
// of the document, written at versionAt and recorded by event eventID, whose render is markdown
// and blocks, in tx: every write that pass makes and none of the store. It rewrites the row and
// records the text of each block of the render whose text moved, inserts a block the render adds
// with no node, deletes the row of each block the render drops and records it dropped, deletes
// each link of dropped and inserts each link of added with no node, and records the version
// indexed and the document's blocks standing at it. It is what that release's request path runs
// for its own version (indexAsThePreviousRelease) and what its block-index reconciliation runs
// for a version some build left owed, which its pod may reach first during a roll.
func indexVersionAsThePreviousRelease(t *testing.T, tx pgx.Tx, artifactID string, version int, versionAt time.Time, eventID int64, markdown string, blocks []model.ArtifactBlock, dropped, added []blockLinkRow) {
	t.Helper()
	ctx := context.Background()
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
	for _, link := range dropped {
		if tag, err := tx.Exec(ctx, `
			delete from block_refs
			where artifact_id = $1 and block_id = $2 and kind = $3 and to_kind = $4 and to_id = $5
		`, artifactID, link.blockID, link.kind, link.toKind, link.toID); err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("delete the dropped link as the previous release does: %d rows, %v", tag.RowsAffected(), err)
		}
	}
	for _, link := range added {
		if _, err := tx.Exec(ctx, `
			insert into block_refs (artifact_id, block_id, from_node, to_kind, to_id, kind, created_version, created_at, source_seq)
			select $1::uuid, $2, a.ref_key || '#' || $2, $3, $4, $5, $6, $7, $8 from artifacts a where a.id = $1::uuid
		`, artifactID, link.blockID, link.toKind, link.toID, link.kind, version, versionAt, eventID); err != nil {
			t.Fatalf("insert the added link as the previous release does: %v", err)
		}
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
