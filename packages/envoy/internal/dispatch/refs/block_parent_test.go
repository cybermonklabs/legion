package refs

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// blockDoc is one version of a test document as a render hands it to the block index: its
// markdown and its blocks in document order, each parent before its children.
type blockDoc struct {
	markdown string
	blocks   []model.ArtifactBlock
}

// parentProject names the project of one document of this file. Each document gets a project
// and an issue of its own, because the fixture numbers an issue 1 within its project and an
// issue holds one primary artifact.
func parentProject(name string) string { return "BLK" + name }

// parentDocument is one document of this test, with the issue it hangs on and the ref_key every
// one of its blocks is addressed under. Each version it writes is a real artifact_versions row
// with an event of its own, so a tuple's time, its writer and its cause can be held to what the
// version's own record says rather than to what the test passed in.
type parentDocument struct {
	database *store.Store
	issue    string
	artifact string
	refKey   string
}

// versionRecord is what the rows say of one version: the time and the writer
// artifact_versions holds, and the id of the event that recorded it.
type versionRecord struct {
	at     time.Time
	writer model.Actor
	event  int64
}

// parentDatabase opens the database this file's documents are written to.
func parentDatabase(t *testing.T) *store.Store {
	t.Helper()
	return storetest.Open(t)
}

// newParentDocument creates one document, as the primary artifact of an issue of a project of
// its own.
func newParentDocument(t *testing.T, database *store.Store, name, slug string) parentDocument {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the document fixture: %v", err)
	}
	defer tx.Rollback(ctx)
	project := parentProject(name)
	issue := project + "-1"
	createReferenceProject(t, tx, project)
	createReferenceIssue(t, tx, issue, project)
	id, refKey := createReferenceArtifact(t, tx, issue, project, slug)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the document fixture: %v", err)
	}
	return parentDocument{database: database, issue: issue, artifact: id, refKey: refKey}
}

// writeVersion records one version of the document as a version write records it - its
// markdown, its writer and its time - with the event that recorded it, and answers all three as
// the rows hold them.
func (d parentDocument) writeVersion(t *testing.T, number int, markdown string, writer model.Actor) versionRecord {
	t.Helper()
	ctx := context.Background()
	actor, err := json.Marshal(writer)
	if err != nil {
		t.Fatalf("encode the version's writer: %v", err)
	}
	authors, err := json.Marshal([]model.Actor{writer})
	if err != nil {
		t.Fatalf("encode the version's authors: %v", err)
	}
	var record versionRecord
	var stored []byte
	if err := d.database.Pool.QueryRow(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1, $2, $3, $4) returning created_at, authors
	`, d.artifact, number, markdown, authors).Scan(&record.at, &stored); err != nil {
		t.Fatalf("write version %d: %v", number, err)
	}
	writers, err := storedActors(stored)
	if err != nil || len(writers) != 1 {
		t.Fatalf("read back the writer of version %d: %v, %#v", number, err, writers)
	}
	record.writer = writers[0]
	payload, err := json.Marshal(map[string]any{
		"artifact_id": d.artifact,
		"version":     map[string]any{"number": number},
	})
	if err != nil {
		t.Fatalf("encode the version event's payload: %v", err)
	}
	if err := d.database.Pool.QueryRow(ctx, `
		insert into events (issue_key, seq, type, actor, payload, notify)
		values ($1, $2, 'artifact.version', $3, $4, false) returning id
	`, d.issue, number, actor, payload).Scan(&record.event); err != nil {
		t.Fatalf("record the event of version %d: %v", number, err)
	}
	return record
}

// indexVersion runs one block index pass over the document and appends what it stated under the
// version's event, in one transaction, as every writer of the store does. It answers whether
// the pass was the one the document's blocks stand at and every tuple that transaction
// appended, read back from the log.
func (d parentDocument) indexVersion(t *testing.T, number int, doc blockDoc, record versionRecord) (bool, []graphstore.Tuple) {
	t.Helper()
	ctx := context.Background()
	before := d.highestTuple(t)
	tx, err := d.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the index of version %d: %v", number, err)
	}
	defer tx.Rollback(ctx)
	_, won, write, err := IndexBlocks(ctx, tx, d.artifact, number, record.at, doc.markdown, doc.blocks, nil, nil)
	if err != nil {
		t.Fatalf("index version %d: %v", number, err)
	}
	if err := graphstore.Caused(ctx, tx, write, record.event); err != nil {
		t.Fatalf("append what version %d stated: %v", number, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the index of version %d: %v", number, err)
	}
	return won, d.tuplesAfter(t, d.database.Pool, before)
}

// highestTuple is the last sequence the log holds, the mark a transaction's own tuples are read
// back after.
func (d parentDocument) highestTuple(t *testing.T) int64 {
	t.Helper()
	var highest int64
	if err := d.database.Pool.QueryRow(context.Background(),
		`select coalesce(max(seq), 0) from graph_tuples`).Scan(&highest); err != nil {
		t.Fatalf("read the end of the tuple log: %v", err)
	}
	return highest
}

// tupleCount is how many tuples the log holds, which a pass that appends nothing leaves as it
// found it.
func (d parentDocument) tupleCount(t *testing.T) int64 {
	t.Helper()
	var count int64
	if err := d.database.Pool.QueryRow(context.Background(), `select count(*) from graph_tuples`).Scan(&count); err != nil {
		t.Fatalf("count the tuples the log holds: %v", err)
	}
	return count
}

// tuplesAfter is every tuple appended after seq, in log order, as q sees the log: what one
// transaction stated.
func (d parentDocument) tuplesAfter(t *testing.T, q Queryer, seq int64) []graphstore.Tuple {
	t.Helper()
	rows, err := q.Query(context.Background(), `
		select from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted
		from graph_tuples where seq > $1 order by seq
	`, seq)
	if err != nil {
		t.Fatalf("read the tuples a transaction stated: %v", err)
	}
	defer rows.Close()
	var stated []graphstore.Tuple
	for rows.Next() {
		var tuple graphstore.Tuple
		var who []byte
		if err := rows.Scan(&tuple.FromKind, &tuple.FromID, &tuple.Kind, &tuple.ToKind, &tuple.ToID,
			&tuple.At, &who, &tuple.SourceEvent, &tuple.Asserted); err != nil {
			t.Fatalf("scan a stated tuple: %v", err)
		}
		if err := json.Unmarshal(who, &tuple.Who); err != nil {
			t.Fatalf("decode the writer of a stated tuple: %v", err)
		}
		stated = append(stated, tuple)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the tuples a transaction stated: %v", err)
	}
	return stated
}

// address is one block's node address.
func (d parentDocument) address(blockID string) string { return d.refKey + "#" + blockID }

// currentParent is the one far end the store asserts for a block's part_of now. It fails unless
// the block holds exactly one: a block is part of one thing at a time.
func (d parentDocument) currentParent(t *testing.T, blockID string) graphstore.Tuple {
	t.Helper()
	held, err := graphstore.CurrentOut(context.Background(), d.database.Pool,
		BlockNodeKind, d.address(blockID), []string{graphstore.TuplePartOf})
	if err != nil {
		t.Fatalf("read the parent of %s: %v", blockID, err)
	}
	if len(held) != 1 {
		t.Fatalf("block %s holds %d current part_of tuples, want exactly one", blockID, len(held))
	}
	return held[0]
}

// memberships is every in_document tuple the log holds out of a block, in log order: one for
// each document_blocks row the block was given.
func (d parentDocument) memberships(t *testing.T, blockID string) []graphstore.Tuple {
	t.Helper()
	held, err := graphstore.History(context.Background(), d.database.Pool, BlockNodeKind, d.address(blockID), TupleInDocument)
	if err != nil {
		t.Fatalf("read the memberships of %s: %v", blockID, err)
	}
	return held
}

// currentMembership is the one in_document tuple the store asserts for a block now. It fails
// unless the block holds exactly one: a block is a member of one document.
func (d parentDocument) currentMembership(t *testing.T, blockID string) graphstore.Tuple {
	t.Helper()
	held, err := graphstore.CurrentOut(context.Background(), d.database.Pool,
		BlockNodeKind, d.address(blockID), []string{TupleInDocument})
	if err != nil {
		t.Fatalf("read the membership of %s: %v", blockID, err)
	}
	if len(held) != 1 {
		t.Fatalf("block %s holds %d current in_document tuples, want exactly one", blockID, len(held))
	}
	return held[0]
}

// joined is the created_at of each block's document_blocks row, by block id, which is the time
// the store dates a block's membership of its document from (TupleInDocument).
func (d parentDocument) joined(t *testing.T) map[string]time.Time {
	t.Helper()
	rows, err := d.database.Pool.Query(context.Background(),
		`select block_id, created_at from document_blocks where artifact_id = $1`, d.artifact)
	if err != nil {
		t.Fatalf("read when the blocks joined the document: %v", err)
	}
	defer rows.Close()
	joined := map[string]time.Time{}
	for rows.Next() {
		var id string
		var at time.Time
		if err := rows.Scan(&id, &at); err != nil {
			t.Fatalf("scan when a block joined the document: %v", err)
		}
		joined[id] = at
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read when the blocks joined the document: %v", err)
	}
	return joined
}

// projectedParents is document_blocks.parent_block_id for every block of the document, by block
// id, with the empty string for a block at the document's top level.
func (d parentDocument) projectedParents(t *testing.T) map[string]string {
	t.Helper()
	rows, err := d.database.Pool.Query(context.Background(),
		`select block_id, coalesce(parent_block_id, ''), coalesce(node_kind, ''), coalesce(node_address, '')
		 from document_blocks where artifact_id = $1`, d.artifact)
	if err != nil {
		t.Fatalf("read the projected parents: %v", err)
	}
	defer rows.Close()
	parents := map[string]string{}
	for rows.Next() {
		var id, parent, nodeKind, nodeAddress string
		if err := rows.Scan(&id, &parent, &nodeKind, &nodeAddress); err != nil {
			t.Fatalf("scan a projected parent: %v", err)
		}
		if nodeKind != BlockNodeKind || nodeAddress != d.address(id) {
			t.Errorf("block %s projects node %q/%q, want %q/%q", id, nodeKind, nodeAddress, BlockNodeKind, d.address(id))
		}
		parents[id] = parent
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the projected parents: %v", err)
	}
	return parents
}

// requireParentsProjected holds document_blocks.parent_block_id to the current part_of tuple of
// every block the document holds, after the version named by after. The column is the tuple's
// projection, so the two may not disagree about any block, whether the version moved it or not.
func (d parentDocument) requireParentsProjected(t *testing.T, after string) {
	t.Helper()
	projected := d.projectedParents(t)
	if len(projected) == 0 {
		t.Fatalf("after %s document_blocks holds no block of the document, so no parent was compared", after)
	}
	for block, parent := range projected {
		wantKind, wantID := "artifact", d.refKey
		if parent != "" {
			wantKind, wantID = BlockNodeKind, d.address(parent)
		}
		if tuple := d.currentParent(t, block); tuple.ToKind != wantKind || tuple.ToID != wantID {
			t.Errorf("after %s block %s's parent_block_id projects %s/%s, but its current part_of is %s/%s",
				after, block, wantKind, wantID, tuple.ToKind, tuple.ToID)
		}
	}
}

// blockChange is the version a block's text last changed in and that version's time, which a
// move must leave exactly where it found them.
type blockChange struct {
	version int
	at      time.Time
}

func (d parentDocument) changes(t *testing.T) map[string]blockChange {
	t.Helper()
	rows, err := d.database.Pool.Query(context.Background(),
		`select block_id, changed_version, changed_at from document_blocks where artifact_id = $1`, d.artifact)
	if err != nil {
		t.Fatalf("read the block changes: %v", err)
	}
	defer rows.Close()
	changes := map[string]blockChange{}
	for rows.Next() {
		var id string
		var change blockChange
		if err := rows.Scan(&id, &change.version, &change.at); err != nil {
			t.Fatalf("scan a block change: %v", err)
		}
		changes[id] = change
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the block changes: %v", err)
	}
	return changes
}

// nestedDocument is a document three blocks deep - top1 holds mid, which holds leaf - beside a
// top-level block of its own, whose text is tail. top1, mid and leaf all end where the head
// ends, so a change to tail changes top2's text and no other block's.
func nestedDocument(tail string) blockDoc {
	const head = "AAAABBC\n"
	return blockDoc{markdown: head + tail, blocks: []model.ArtifactBlock{
		{ID: "top1", Type: "paragraph", From: 0, To: len(head)},
		{ID: "mid", Type: "paragraph", From: 4, To: len(head)},
		{ID: "leaf", Type: "paragraph", From: 6, To: len(head)},
		{ID: "top2", Type: "paragraph", From: len(head), To: len(head) + len(tail)},
	}}
}

// The two halves of the mover document are the same three bytes, so a block of two of those
// bytes reads the same text in either half: the version below moves it from the left half to
// the right one and changes no block's text, which is what lets the move be told from an edit.
const moverMarkdown = "xy\nxy\n"

func moverUnderLeft() blockDoc {
	return blockDoc{markdown: moverMarkdown, blocks: []model.ArtifactBlock{
		{ID: "left", Type: "paragraph", From: 0, To: 3},
		{ID: "mover", Type: "paragraph", From: 0, To: 2},
		{ID: "right", Type: "paragraph", From: 3, To: 6},
	}}
}

func moverUnderRight() blockDoc {
	return blockDoc{markdown: moverMarkdown, blocks: []model.ArtifactBlock{
		{ID: "left", Type: "paragraph", From: 0, To: 3},
		{ID: "right", Type: "paragraph", From: 3, To: 6},
		{ID: "mover", Type: "paragraph", From: 3, To: 5},
	}}
}

// A block's parent is a tuple and document_blocks.parent_block_id is that tuple's projection.
//
// The tuple states the innermost block that holds the block, and the document's own artifact
// address for a block no block holds; it carries the version's time, the version's writer and
// the event that recorded the version, each held here to what the version's own rows say. Each
// block is also a member of its document exactly once, by an in_document tuple the same writer
// states under the same event. The tuple and the block's node are one transaction's work,
// proven by rolling back a transaction that had stated both and finding neither. After every
// version the column and the current tuple agree for every block the document holds.
//
// A version that moves a block under another parent and changes no text retracts the parent it
// left and asserts the one it reached, each at that version's time, by its writer and under the
// event that recorded it, moves no block's changed_version or changed_at, and states nothing else
// at all. A version that changes one block's text alone reads the parent of every other block
// again, and the same version indexed twice loses the revision fence and appends no tuple. A
// block one version drops and the next restores joins the document a second time, on its new
// row, and no block that stayed joins it twice.
func TestABlockParentIsATupleAndItsColumnIsItsProjection(t *testing.T) {
	ctx := context.Background()
	database := parentDatabase(t)
	writer := model.Actor{Kind: "user", ID: "alice"}
	nested := newParentDocument(t, database, "NEST", "nested-md")

	first := nested.writeVersion(t, 1, nestedDocument("tail one\n").markdown, writer)
	won, stated := nested.indexVersion(t, 1, nestedDocument("tail one\n"), first)
	if !won {
		t.Fatalf("the document's first index did not become the version its blocks stand at")
	}
	// Each block points at the innermost block that holds it, and a block no block holds points
	// at the document.
	wantParents := map[string]string{"top1": "", "mid": "top1", "leaf": "mid", "top2": ""}
	for block, parent := range wantParents {
		tuple := nested.currentParent(t, block)
		wantKind, wantID := "artifact", nested.refKey
		if parent != "" {
			wantKind, wantID = BlockNodeKind, nested.address(parent)
		}
		if tuple.ToKind != wantKind || tuple.ToID != wantID {
			t.Errorf("block %s is part of %s/%s, want %s/%s", block, tuple.ToKind, tuple.ToID, wantKind, wantID)
		}
		// The version's own record decides all three; a tuple that disagrees with it fails.
		if !tuple.At.Equal(first.at) {
			t.Errorf("block %s's parent is dated %s, want the version's own %s", block, tuple.At, first.at)
		}
		if tuple.Who != first.writer {
			t.Errorf("block %s's parent was stated by %#v, want the version's writer %#v", block, tuple.Who, first.writer)
		}
		if tuple.SourceEvent != first.event {
			t.Errorf("block %s's parent was caused by event %d, want the version's own %d", block, tuple.SourceEvent, first.event)
		}
	}
	if got := nested.projectedParents(t); !maps.Equal(got, wantParents) {
		t.Errorf("document_blocks projects the parents %v, want %v", got, wantParents)
	}
	nested.requireParentsProjected(t, "the first version")
	// Each block is a member of its document exactly once, stated by the version's writer under
	// the version's event. The version's time is left unasserted here: the store dates a
	// membership from its document_blocks row's created_at (TupleInDocument), and this test
	// writes the version and indexes it in two transactions, so the two times differ. The
	// tuple is held to its row's created_at instead.
	joinedAt := nested.joined(t)
	for block := range wantParents {
		if held := nested.memberships(t, block); len(held) != 1 {
			t.Errorf("block %s holds %d in_document tuples in the log, want exactly one: %+v", block, len(held), held)
		}
		member := nested.currentMembership(t, block)
		if member.ToKind != "artifact" || member.ToID != nested.refKey {
			t.Errorf("block %s is a member of %s/%s, want artifact/%s", block, member.ToKind, member.ToID, nested.refKey)
		}
		if member.Who != first.writer {
			t.Errorf("block %s's membership was stated by %#v, want the version's writer %#v", block, member.Who, first.writer)
		}
		if member.SourceEvent != first.event {
			t.Errorf("block %s's membership was caused by event %d, want the version's own %d", block, member.SourceEvent, first.event)
		}
		if !member.At.Equal(joinedAt[block]) {
			t.Errorf("block %s's membership is dated %s, want its document_blocks row's created_at %s", block, member.At, joinedAt[block])
		}
	}
	// The block's node carries the same cause, and the write that made it names the version's
	// writer and time.
	node, found, err := graphstore.ReadNode(ctx, database.Pool, BlockNodeKind, nested.address("leaf"))
	if err != nil || !found {
		t.Fatalf("read the node of the innermost block: %v, found %v", err, found)
	}
	if node.State != graphstore.StateLive || node.SourceEvent != first.event || !node.MadeAt.Equal(first.at) {
		t.Errorf("the innermost block's node is %+v, want it live, caused by event %d and made at %s", node, first.event, first.at)
	}

	// The tuple and the node are one transaction's work: a pass that rolls back leaves neither.
	added := nestedDocument("tail one\n")
	added.blocks = append(added.blocks, model.ArtifactBlock{
		ID: "extra", Type: "paragraph", From: len(added.markdown), To: len(added.markdown),
	})
	added.markdown += "extra\n"
	added.blocks[len(added.blocks)-1].To = len(added.markdown)
	rolledBack := nested.writeVersion(t, 2, added.markdown, writer)
	before := nested.highestTuple(t)
	func() {
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the pass that rolls back: %v", err)
		}
		defer tx.Rollback(ctx)
		_, stood, write, err := IndexBlocks(ctx, tx, nested.artifact, 2, rolledBack.at, added.markdown, added.blocks, nil, nil)
		if err != nil {
			t.Fatalf("index the version that rolls back: %v", err)
		}
		if err := graphstore.Caused(ctx, tx, write, rolledBack.event); err != nil {
			t.Fatalf("append what the rolled-back pass stated: %v", err)
		}
		// Finding nothing after the rollback proves the rollback undid the work only if the
		// pass did the work first: it won the fence, and inside its transaction the new block's
		// node, its parent and its membership are all there.
		if !stood {
			t.Fatalf("the pass that rolls back lost the fence, so it stated nothing for the rollback to undo")
		}
		if !slices.ContainsFunc(write.Nodes, func(node graphstore.Node) bool { return node.ID == nested.address("extra") }) {
			t.Errorf("the pass that rolls back stated the nodes %+v, want the new block's among them", write.Nodes)
		}
		if _, found, err := graphstore.ReadNode(ctx, tx, BlockNodeKind, nested.address("extra")); err != nil || !found {
			t.Errorf("before its rollback the pass holds no node of the new block: found %v, %v", found, err)
		}
		inside := nested.tuplesAfter(t, tx, before)
		if !statesParent(inside, nested.address("extra"), true) || !slices.ContainsFunc(inside, func(tuple graphstore.Tuple) bool {
			return tuple.Kind == TupleInDocument && tuple.FromID == nested.address("extra")
		}) {
			t.Errorf("before its rollback the pass appended %+v, want the new block's parent and membership among them", inside)
		}
	}()
	if stated := nested.tuplesAfter(t, database.Pool, before); len(stated) != 0 {
		t.Errorf("the rolled-back pass left %d tuples behind, want none", len(stated))
	}
	if _, found, err := graphstore.ReadNode(ctx, database.Pool, BlockNodeKind, nested.address("extra")); err != nil || found {
		t.Errorf("the rolled-back pass left the new block's node behind: found %v, %v", found, err)
	}

	// A version that changes one block's text alone reads the parent of every other block
	// again: the parents of top1, mid and leaf - a block the document holds, a block that holds
	// another and is held, and a block that holds none - are retracted behind the index's back
	// first, and the pass puts each back although none of their texts changed.
	changed := nestedDocument("tail two\n")
	textOnly := nested.writeVersion(t, 3, changed.markdown, writer)
	retractParents(t, database, nested, first, "top1", "mid", "leaf")
	_, stated = nested.indexVersion(t, 3, changed, textOnly)
	for _, block := range []string{"top1", "mid", "leaf"} {
		if !statesParent(stated, nested.address(block), true) {
			t.Errorf("the text-only version stated %v, want it to assert %s's parent again: the pass read every block's parent, not the changed one's alone",
				stated, block)
		}
	}
	if got := nested.projectedParents(t); !maps.Equal(got, wantParents) {
		t.Errorf("after the text-only version document_blocks projects %v, want %v", got, wantParents)
	}
	nested.requireParentsProjected(t, "the text-only version")

	// The same version indexed twice states nothing at all: the mark only moves forward, and a
	// pass that does not move it writes neither a projection row nor a tuple. The second pass
	// loses the revision fence, and the log holds as many tuples after it as before.
	logged := nested.tupleCount(t)
	won, stated = nested.indexVersion(t, 3, changed, textOnly)
	if won {
		t.Errorf("the second pass at version 3 claimed the document's blocks stand at it")
	}
	if len(stated) != 0 {
		t.Errorf("the second pass at version 3 stated %v, want nothing", stated)
	}
	if after := nested.tupleCount(t); after != logged {
		t.Errorf("the log holds %d tuples after the second pass at version 3 and held %d before it, want the pass that lost the fence to append none",
			after, logged)
	}
	nested.requireParentsProjected(t, "the second pass at version 3")

	// A block one version drops and the next restores joins the document again, on a new
	// document_blocks row: that version states exactly one in_document tuple, the restored
	// block's, dated from its new row and stated by the restoring version's writer under its
	// event, and a block that stayed holds no second one. leaf is the block dropped, and its
	// versions are another writer's, so a membership stated by the first version's writer
	// fails. leaf is a block another block holds, so its new row, inserted with no parent, must
	// also project the parent the store still asserts for it.
	restorer := model.Actor{Kind: "user", ID: "bob"}
	firstMembership := nested.currentMembership(t, "leaf")
	withoutLeaf := nestedDocument("tail two\n")
	withoutLeaf.blocks = slices.DeleteFunc(withoutLeaf.blocks, func(block model.ArtifactBlock) bool { return block.ID == "leaf" })
	dropping := nested.writeVersion(t, 4, withoutLeaf.markdown, restorer)
	if won, _ := nested.indexVersion(t, 4, withoutLeaf, dropping); !won {
		t.Fatalf("the version that drops leaf did not become the version the document's blocks stand at")
	}
	if got, want := nested.projectedParents(t), map[string]string{"top1": "", "mid": "top1", "top2": ""}; !maps.Equal(got, want) {
		t.Errorf("after the version that drops leaf document_blocks projects %v, want %v", got, want)
	}
	nested.requireParentsProjected(t, "the version that drops leaf")
	restoring := nested.writeVersion(t, 5, changed.markdown, restorer)
	won, stated = nested.indexVersion(t, 5, changed, restoring)
	if !won {
		t.Fatalf("the version that restores leaf did not become the version the document's blocks stand at")
	}
	if got := nested.projectedParents(t); !maps.Equal(got, wantParents) {
		t.Errorf("after the version that restores leaf document_blocks projects %v, want %v", got, wantParents)
	}
	nested.requireParentsProjected(t, "the version that restores leaf")
	var joinedAgain []graphstore.Tuple
	for _, tuple := range stated {
		if tuple.Kind == TupleInDocument {
			joinedAgain = append(joinedAgain, tuple)
		}
	}
	if len(joinedAgain) != 1 || joinedAgain[0].FromID != nested.address("leaf") {
		t.Errorf("the version that restores leaf stated the memberships %+v, want exactly one, leaf's", joinedAgain)
	} else {
		member, rejoinedAt := joinedAgain[0], nested.joined(t)["leaf"]
		if !member.Asserted || member.ToKind != "artifact" || member.ToID != nested.refKey {
			t.Errorf("leaf's second membership is %+v, want it to assert artifact/%s", member, nested.refKey)
		}
		if !member.At.Equal(rejoinedAt) || member.At.Equal(firstMembership.At) {
			t.Errorf("leaf's second membership is dated %s, want its new document_blocks row's created_at %s, not its first row's %s",
				member.At, rejoinedAt, firstMembership.At)
		}
		if member.Who != restoring.writer {
			t.Errorf("leaf's second membership was stated by %#v, want the restoring version's writer %#v", member.Who, restoring.writer)
		}
		if member.SourceEvent != restoring.event {
			t.Errorf("leaf's second membership was caused by event %d, want the restoring version's own %d", member.SourceEvent, restoring.event)
		}
	}
	for block := range wantParents {
		want := 1
		if block == "leaf" {
			want = 2
		}
		if held := nested.memberships(t, block); len(held) != want {
			t.Errorf("after the drop and the restore block %s holds %d in_document tuples in the log, want %d: %+v", block, len(held), want, held)
		}
		nested.currentMembership(t, block)
	}

	// A move with no text change: the new parent reads from the tuples, the old one is retracted
	// and still in the history, the column follows, no block's change moved, and nothing of any
	// other kind is stated.
	mover := newParentDocument(t, database, "MOVE", "mover-md")
	placed := mover.writeVersion(t, 1, moverMarkdown, writer)
	if won, _ := mover.indexVersion(t, 1, moverUnderLeft(), placed); !won {
		t.Fatalf("the mover document's first index did not stand")
	}
	if tuple := mover.currentParent(t, "mover"); tuple.ToID != mover.address("left") {
		t.Fatalf("before the move the block is part of %s, want %s", tuple.ToID, mover.address("left"))
	}
	mover.requireParentsProjected(t, "the mover document's first version")
	wasChanged := mover.changes(t)
	// The loop below proves the move moved no block's change only if it reads every block's.
	if len(wasChanged) != 3 {
		t.Fatalf("before the move document_blocks holds the changes %v, want one for each of the 3 blocks", wasChanged)
	}
	// The version that moves the block is another writer's, so a tuple stated with the first
	// version's writer fails here as one stated with its time or its event does.
	moved := mover.writeVersion(t, 2, moverMarkdown, model.Actor{Kind: "user", ID: "carol"})
	_, stated = mover.indexVersion(t, 2, moverUnderRight(), moved)
	// The new parent and the retraction of the one the block left are each the moving version's:
	// its time, its writer and the event that recorded it, read off that version's own rows.
	requireMovedBy := func(what string, tuple graphstore.Tuple) {
		t.Helper()
		if !tuple.At.Equal(moved.at) {
			t.Errorf("the moved block's %s is dated %s, want the moving version's own %s", what, tuple.At, moved.at)
		}
		if tuple.Who != moved.writer {
			t.Errorf("the moved block's %s was stated by %#v, want the moving version's writer %#v", what, tuple.Who, moved.writer)
		}
		if tuple.SourceEvent != moved.event {
			t.Errorf("the moved block's %s was caused by event %d, want the moving version's own %d", what, tuple.SourceEvent, moved.event)
		}
	}
	if tuple := mover.currentParent(t, "mover"); tuple.ToID != mover.address("right") {
		t.Errorf("after the move the block is part of %s, want %s", tuple.ToID, mover.address("right"))
	} else {
		requireMovedBy("new parent", tuple)
	}
	history, err := graphstore.History(ctx, database.Pool, BlockNodeKind, mover.address("mover"), graphstore.TuplePartOf)
	if err != nil {
		t.Fatalf("read the history of the moved block's parent: %v", err)
	}
	if left := slices.IndexFunc(history, func(tuple graphstore.Tuple) bool {
		return !tuple.Asserted && tuple.ToID == mover.address("left")
	}); left < 0 {
		t.Errorf("the history of the moved block is %v, want the retraction of the parent it left", history)
	} else {
		requireMovedBy("retraction of the parent it left", history[left])
	}
	wantMoved := map[string]string{"left": "", "right": "", "mover": "right"}
	if got := mover.projectedParents(t); !maps.Equal(got, wantMoved) {
		t.Errorf("after the move document_blocks projects %v, want %v", got, wantMoved)
	}
	mover.requireParentsProjected(t, "the move")
	for block, was := range wasChanged {
		if now := mover.changes(t)[block]; now.version != was.version || !now.at.Equal(was.at) {
			t.Errorf("the move moved block %s's change from version %d at %s to version %d at %s",
				block, was.version, was.at, now.version, now.at)
		}
	}
	if len(stated) != 2 {
		t.Errorf("the move stated %d tuples, want the retraction and the assertion alone: %v", len(stated), stated)
	}
	for _, tuple := range stated {
		if tuple.Kind != graphstore.TuplePartOf {
			t.Errorf("the move stated a %s tuple, want nothing but part_of: %+v", tuple.Kind, tuple)
		}
	}
}

// retractParents ends the part_of of each named block behind the index's back, as a writer that
// moved the blocks out would, retracting the far end the store asserts for each now. The
// text-only version that follows must state each parent again, which it does only if it read
// the parent of every block of the document rather than of the one whose text it changed.
func retractParents(t *testing.T, database *store.Store, document parentDocument, record versionRecord, blocks ...string) {
	t.Helper()
	ctx := context.Background()
	retractions := make([]graphstore.Tuple, 0, len(blocks))
	for _, block := range blocks {
		held := document.currentParent(t, block)
		retractions = append(retractions, graphstore.Tuple{
			FromKind: BlockNodeKind, FromID: document.address(block), Kind: graphstore.TuplePartOf,
			ToKind: held.ToKind, ToID: held.ToID,
			At: record.at, Who: record.writer, SourceEvent: graphstore.NoEvent, Asserted: false,
		})
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the retraction: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := graphstore.Write(ctx, tx, nil, retractions); err != nil {
		t.Fatalf("retract the parents of %v: %v", blocks, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the retraction: %v", err)
	}
	// The pass that follows is held to restating these parents, which proves something only
	// if the store no longer asserts them.
	for _, block := range blocks {
		held, err := graphstore.CurrentOut(ctx, database.Pool, BlockNodeKind, document.address(block), []string{graphstore.TuplePartOf})
		if err != nil || len(held) != 0 {
			t.Fatalf("after the retraction block %s still holds the parents %+v (%v), want none", block, held, err)
		}
	}
}

// statesParent reports whether stated holds a part_of tuple out of address with that assertion.
func statesParent(stated []graphstore.Tuple, address string, asserted bool) bool {
	return slices.ContainsFunc(stated, func(tuple graphstore.Tuple) bool {
		return tuple.Kind == graphstore.TuplePartOf && tuple.FromID == address && tuple.Asserted == asserted
	})
}

// unwriteTheStore puts the document's rows back into the shape the release before the node and
// tuple store left them in: every row of document_blocks and block_refs names no node, and the
// store holds neither the nodes of its blocks nor anything stated about them. That is what a
// database holds for a document indexed before this release and not reached by
// `envoy-dispatch backfill-graph-store`, and it is the state a new version has to be indexable
// from.
func unwriteTheStore(t *testing.T, document parentDocument) {
	t.Helper()
	ctx := context.Background()
	tx, err := document.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the unwriting of the store: %v", err)
	}
	defer tx.Rollback(ctx)
	for _, statement := range []string{
		`update document_blocks set node_kind = null, node_address = null, parent_block_id = null
		 where artifact_id = $1::uuid`,
		`update block_refs set node_kind = null, node_address = null where artifact_id = $1::uuid`,
	} {
		if _, err := tx.Exec(ctx, statement, document.artifact); err != nil {
			t.Fatalf("take the node columns off the rows of %s: %v", document.artifact, err)
		}
	}
	for _, statement := range []string{
		`delete from graph_tuples where from_kind = $1 and starts_with(from_id, $2)`,
		`delete from graph_nodes where kind = $1 and starts_with(id, $2)`,
	} {
		if _, err := tx.Exec(ctx, statement, BlockNodeKind, document.refKey+"#"); err != nil {
			t.Fatalf("take the store's rows of the blocks of %s away: %v", document.refKey, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the unwriting of the store: %v", err)
	}
	// The index pass that follows proves something only if the rows really name no node now.
	var named, nodes int
	if err := document.database.Pool.QueryRow(ctx, `
		select (select count(*) from document_blocks
		        where artifact_id = $1::uuid and (node_kind is not null or node_address is not null)),
		       (select count(*) from graph_nodes where kind = $2 and starts_with(id, $3))
	`, document.artifact, BlockNodeKind, document.refKey+"#").Scan(&named, &nodes); err != nil {
		t.Fatalf("read back the unwritten rows of %s: %v", document.refKey, err)
	}
	if named != 0 || nodes != 0 {
		t.Fatalf("after the unwriting %d row(s) of %s still name a node and the store holds %d of its block nodes, want none of either",
			named, document.refKey, nodes)
	}
}

// blockNodesOf is how many nodes the store holds of the document's blocks.
func blockNodesOf(t *testing.T, document parentDocument) int {
	t.Helper()
	var nodes int
	if err := document.database.Pool.QueryRow(context.Background(),
		`select count(*) from graph_nodes where kind = $1 and starts_with(id, $2)`,
		BlockNodeKind, document.refKey+"#").Scan(&nodes); err != nil {
		t.Fatalf("count the block nodes of %s: %v", document.refKey, err)
	}
	return nodes
}

// A document indexed before this release takes a new version, and that version gives every row
// of it its node and its membership of the document.
//
// The rows are put back in the shape the previous release left them in - node columns null, no
// node and no tuple in the store - which is what a database the backfill has not reached holds.
// A version is then written and indexed. It must commit: the projection of the parents
// planBlockParents plans (queueBlockParents) names each block's node on its row, and
// document_blocks_node is checked inside that statement, so a pass that gave a node only to the
// blocks whose text the version changed would be refused there (SQLSTATE 23503) and the document
// could never take another version.
//
// Afterwards every row names its node, the store holds each one live, and each block is a
// member of its document exactly once, dated from its own row's created_at and stated by the
// indexing version's writer under its event - a block whose text that version changed as much
// as one it left alone, since the row of a changed pre-store block is updated rather than
// inserted and states no membership of its own. A version after that states no second
// membership for any of them: the pass gives a node to the rows that name none, not to every
// row of the document.
func TestAPreStoreDocumentTakesItsNodesAtItsNextVersion(t *testing.T) {
	ctx := context.Background()
	database := parentDatabase(t)
	writer := model.Actor{Kind: "user", ID: "alice"}
	document := newParentDocument(t, database, "PRIOR", "pre-store-md")

	first := document.writeVersion(t, 1, nestedDocument("tail one\n").markdown, writer)
	if won, _ := document.indexVersion(t, 1, nestedDocument("tail one\n"), first); !won {
		t.Fatalf("the document's first index did not become the version its blocks stand at")
	}
	unwriteTheStore(t, document)

	// The version changes top2's text alone, so top1, mid and leaf are blocks this pass writes
	// no document_blocks row for at all.
	changed := nestedDocument("tail two\n")
	indexer := model.Actor{Kind: "user", ID: "bob"}
	second := document.writeVersion(t, 2, changed.markdown, indexer)
	won, stated := document.indexVersion(t, 2, changed, second)
	if !won {
		t.Fatalf("the version indexed after the store was unwritten did not become the version the document's blocks stand at")
	}
	wantParents := map[string]string{"top1": "", "mid": "top1", "leaf": "mid", "top2": ""}
	if got := document.projectedParents(t); !maps.Equal(got, wantParents) {
		t.Errorf("after the version document_blocks projects the parents %v, want %v", got, wantParents)
	}
	document.requireParentsProjected(t, "the version indexed after the store was unwritten")
	if nodes := blockNodesOf(t, document); nodes != len(wantParents) {
		t.Errorf("the store holds %d nodes of the document's blocks, want one for each of its %d", nodes, len(wantParents))
	}
	joinedAt := document.joined(t)
	for block := range wantParents {
		node, found, err := graphstore.ReadNode(ctx, database.Pool, BlockNodeKind, document.address(block))
		if err != nil || !found {
			t.Errorf("the store holds no node of block %s: found %v, %v", block, found, err)
		} else if node.State != graphstore.StateLive {
			t.Errorf("block %s's node is %s, want it live", block, node.State)
		}
		if held := document.memberships(t, block); len(held) != 1 {
			t.Errorf("block %s holds %d in_document tuples in the log, want exactly one: %+v", block, len(held), held)
		}
		member := document.currentMembership(t, block)
		if member.ToKind != "artifact" || member.ToID != document.refKey {
			t.Errorf("block %s is a member of %s/%s, want artifact/%s", block, member.ToKind, member.ToID, document.refKey)
		}
		if !member.At.Equal(joinedAt[block]) {
			t.Errorf("block %s's membership is dated %s, want its document_blocks row's created_at %s", block, member.At, joinedAt[block])
		}
		if member.Who != second.writer || member.SourceEvent != second.event {
			t.Errorf("block %s's membership was stated by %#v under event %d, want the indexing version's writer %#v and its event %d",
				block, member.Who, member.SourceEvent, second.writer, second.event)
		}
	}
	for _, block := range []string{"top1", "mid", "leaf", "top2"} {
		if !slices.ContainsFunc(stated, func(tuple graphstore.Tuple) bool {
			return tuple.Kind == TupleInDocument && tuple.FromID == document.address(block)
		}) {
			t.Errorf("the version stated %+v, want block %s's membership among them", stated, block)
		}
	}

	// A version after it states no membership at all: every row names its node now.
	third := document.writeVersion(t, 3, nestedDocument("tail three\n").markdown, indexer)
	_, again := document.indexVersion(t, 3, nestedDocument("tail three\n"), third)
	for _, tuple := range again {
		if tuple.Kind == TupleInDocument {
			t.Errorf("the version after it stated the membership %+v, want none: every row of the document names its node already", tuple)
		}
	}
}

// A block whose address is past the store's bound keeps a row that names no node, and the
// document takes version after version all the same.
//
// Its document's ref_key alone is longer than refs.MaxBlockNodeBytes, so no block of it can be
// a node (blockFits). The pass therefore writes no node, states nothing, and leaves the
// node columns null - which is what keeps the `document_blocks` arm of graph_edges answering
// those rows - and the second version indexes as the first did rather than being refused by
// document_blocks_node for a node nobody may write.
func TestABlockPastTheAddressBoundKeepsARowThatNamesNoNode(t *testing.T) {
	database := parentDatabase(t)
	writer := model.Actor{Kind: "user", ID: "alice"}
	document := newParentDocument(t, database, "LONGKEY", strings.Repeat("l", MaxBlockNodeBytes))
	if len(document.refKey) <= MaxBlockNodeBytes {
		t.Fatalf("the document's ref_key is %d bytes, want more than the bound %d so that no block of it is a node",
			len(document.refKey), MaxBlockNodeBytes)
	}

	for _, version := range []struct {
		number int
		tail   string
	}{{1, "tail one\n"}, {2, "tail two\n"}} {
		doc := nestedDocument(version.tail)
		record := document.writeVersion(t, version.number, doc.markdown, writer)
		if won, stated := document.indexVersion(t, version.number, doc, record); !won {
			t.Fatalf("version %d of the long-keyed document did not become the version its blocks stand at", version.number)
		} else if len(stated) != 0 {
			t.Errorf("version %d of the long-keyed document stated %+v, want nothing: no block of it is a node", version.number, stated)
		}
	}
	var rows, named int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*), count(*) filter (where node_kind is not null or node_address is not null)
		from document_blocks where artifact_id = $1::uuid
	`, document.artifact).Scan(&rows, &named); err != nil {
		t.Fatalf("read the rows of the long-keyed document: %v", err)
	}
	if rows == 0 || named != 0 {
		t.Errorf("the long-keyed document holds %d rows, %d of them naming a node; want rows that all name none", rows, named)
	}
	if nodes := blockNodesOf(t, document); nodes != 0 {
		t.Errorf("the store holds %d nodes of the long-keyed document's blocks, want none", nodes)
	}
}

// A pass the revision fence's own statement refuses commits nothing (R23).
//
// The plan decides the fence in the pass's read phase (PlanBlocks), and the stand decides it
// again in its statement, against the row as it stands when that statement runs (queueStand).
// Here a second transaction stores a newer revision of the document between the two, as no
// writer of document_block_versions does today: the pass, at version 2, read a null version and
// planned a win, and its row then stands at 3. The pass's version adds a block and rewords
// another, so a pass that went on would write that block's node, its tuples and the rows of
// version 2. Its batch fails instead, with the fence's error naming the document and the version,
// and once its transaction is abandoned, as every caller abandons one whose write phase failed,
// the store holds no node, no tuple and no projection row of the pass, and the row still stands
// at the newer revision. A pass whose batch answered no error is committed as a caller commits
// one, so a fence that refused nothing leaves its rows here to be named.
func TestAPassTheFenceRefusesCommitsNothing(t *testing.T) {
	ctx := context.Background()
	database := parentDatabase(t)
	writer := model.Actor{Kind: "user", ID: "alice"}
	document := newParentDocument(t, database, "FENCE", "fenced-md")
	first := document.writeVersion(t, 1, nestedDocument("tail one\n").markdown, writer)
	if won, _ := document.indexVersion(t, 1, nestedDocument("tail one\n"), first); !won {
		t.Fatalf("the document's first index did not become the version its blocks stand at")
	}
	const extra = "extra\n"
	added := nestedDocument("tail two\n")
	added.blocks = append(added.blocks, model.ArtifactBlock{
		ID: "extra", Type: "paragraph", From: len(added.markdown), To: len(added.markdown) + len(extra),
	})
	added.markdown += extra
	const own, newer = 2, 3
	second := document.writeVersion(t, own, added.markdown, writer)
	before := document.highestTuple(t)

	// The pass, through its read phase.
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the pass: %v", err)
	}
	defer tx.Rollback(ctx)
	index, err := ReadBlockIndex(ctx, tx, document.artifact)
	if err != nil {
		t.Fatalf("read the document's block index: %v", err)
	}
	record, err := ReadIndexedVersion(ctx, tx, document.artifact, own)
	if err != nil {
		t.Fatalf("read version %d's row: %v", own, err)
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `select now()`).Scan(&now); err != nil {
		t.Fatalf("read the pass's time: %v", err)
	}
	indexing, err := PlanBlocks(ctx, tx, index, BlockIndexMarkingRule, now, VersionBlocks{
		ArtifactID: document.artifact, Record: record, Number: own, At: second.at, Markdown: added.markdown, Blocks: added.blocks,
	})
	if err != nil {
		t.Fatalf("plan the pass at version %d: %v", own, err)
	}
	if !indexing.Won() {
		t.Fatalf("the pass at version %d lost the fence in its read phase, so its statement has nothing to refuse and this test proves nothing", own)
	}

	// A newer revision, committed between the plan and the write phase.
	if _, err := database.Pool.Exec(ctx, `
		update document_block_versions set version = $2, store_version = $2 where artifact_id = $1::uuid
	`, document.artifact, newer); err != nil {
		t.Fatalf("store revision %d: %v", newer, err)
	}

	err = store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		indexing.Queue(b)
		return nil
	})
	if err != nil {
		for _, says := range []string{"the revision fence refused", document.artifact, fmt.Sprintf("version %d", own)} {
			if !strings.Contains(err.Error(), says) {
				t.Errorf("the refused pass failed with %q, want the fence's error, which says %q", err, says)
			}
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("abandon the refused pass: %v", err)
		}
	} else {
		t.Errorf("the pass at version %d, whose row stood at %d when its stand ran, answered no error: the fence's statement refused nothing", own, newer)
		if err := graphstore.Caused(ctx, tx, indexing.Stated(), second.event); err != nil {
			t.Fatalf("append what the pass stated: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit the pass: %v", err)
		}
	}

	if _, found, err := graphstore.ReadNode(ctx, database.Pool, BlockNodeKind, document.address("extra")); err != nil || found {
		t.Errorf("the refused pass left the node of block extra, which only version %d holds: found %v, %v", own, found, err)
	}
	if stated := document.tuplesAfter(t, database.Pool, before); len(stated) != 0 {
		t.Errorf("the refused pass left %d tuples: %+v", len(stated), stated)
	}
	for _, row := range []struct{ table, query string }{
		{"document_blocks", `select block_id from document_blocks where artifact_id = $1::uuid and changed_version = $2 order by block_id`},
		{"document_block_texts", `select block_id from document_block_texts where artifact_id = $1::uuid and version = $2 order by block_id`},
		{"document_indexed_versions", `select version::text from document_indexed_versions where artifact_id = $1::uuid and version = $2`},
	} {
		rows, err := database.Pool.Query(ctx, row.query, document.artifact, own)
		if err != nil {
			t.Fatalf("read the %s rows of version %d: %v", row.table, own, err)
		}
		left, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("read the %s rows of version %d: %v", row.table, own, err)
		}
		if len(left) != 0 {
			t.Errorf("the refused pass left the %s rows of version %d: %v", row.table, own, left)
		}
	}
	var stands *int
	if err := database.Pool.QueryRow(ctx,
		`select version from document_block_versions where artifact_id = $1::uuid`, document.artifact).Scan(&stands); err != nil {
		t.Fatalf("read the revision the document stands at: %v", err)
	}
	if stands == nil {
		t.Errorf("the document stands at no revision after the refused pass, want the newer revision %d", newer)
	} else if *stands != newer {
		t.Errorf("the document stands at %v after the refused pass, want the newer revision %d", *stands, newer)
	}
}
