package refs

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

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
	if err := write.Caused(ctx, tx, record.event); err != nil {
		t.Fatalf("append what version %d stated: %v", number, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the index of version %d: %v", number, err)
	}
	return won, d.tuplesAfter(t, before)
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

// tuplesAfter is every tuple appended after seq, in log order: what one transaction stated.
func (d parentDocument) tuplesAfter(t *testing.T, seq int64) []graphstore.Tuple {
	t.Helper()
	rows, err := d.database.Pool.Query(context.Background(), `
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
// the event that recorded the version, each held here to what the version's own rows say. The
// tuple and the block's node are one transaction's work, proven by rolling that transaction
// back and finding neither.
//
// A version that moves a block under another parent and changes no text retracts the parent it
// left and asserts the one it reached, moves no block's changed_version or changed_at, and
// states nothing else at all. A version that changes one block's text alone reads the parent of
// every other block again, and the same version indexed twice states nothing.
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
	if got := nested.projectedParents(t); !mapsEqual(got, wantParents) {
		t.Errorf("document_blocks projects the parents %v, want %v", got, wantParents)
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
		_, _, write, err := IndexBlocks(ctx, tx, nested.artifact, 2, rolledBack.at, added.markdown, added.blocks, nil, nil)
		if err != nil {
			t.Fatalf("index the version that rolls back: %v", err)
		}
		if err := write.Caused(ctx, tx, rolledBack.event); err != nil {
			t.Fatalf("append what the rolled-back pass stated: %v", err)
		}
	}()
	if stated := nested.tuplesAfter(t, before); len(stated) != 0 {
		t.Errorf("the rolled-back pass left %d tuples behind, want none", len(stated))
	}
	if _, found, err := graphstore.ReadNode(ctx, database.Pool, BlockNodeKind, nested.address("extra")); err != nil || found {
		t.Errorf("the rolled-back pass left the new block's node behind: found %v, %v", found, err)
	}

	// A version that changes one block's text alone reads the parent of every other block
	// again: mid's parent is retracted behind the index's back first, and the pass puts it back
	// although mid's own text did not change.
	changed := nestedDocument("tail two\n")
	textOnly := nested.writeVersion(t, 3, changed.markdown, writer)
	retractMidParent(t, database, nested, first)
	_, stated = nested.indexVersion(t, 3, changed, textOnly)
	if tuple := nested.currentParent(t, "mid"); tuple.ToID != nested.address("top1") {
		t.Errorf("after the text-only version mid is part of %s, want %s: the pass read every block's parent, not the changed ones alone",
			tuple.ToID, nested.address("top1"))
	}
	if !statesParent(stated, nested.address("mid"), true) {
		t.Errorf("the text-only version stated %v, want it to assert mid's parent again", stated)
	}

	// The same version indexed twice states nothing at all: the mark only moves forward, and a
	// pass that does not move it writes neither a projection row nor a tuple.
	won, stated = nested.indexVersion(t, 3, changed, textOnly)
	if won {
		t.Errorf("the second pass at version 3 claimed the document's blocks stand at it")
	}
	if len(stated) != 0 {
		t.Errorf("the second pass at version 3 stated %v, want nothing", stated)
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
	wasChanged := mover.changes(t)
	moved := mover.writeVersion(t, 2, moverMarkdown, writer)
	_, stated = mover.indexVersion(t, 2, moverUnderRight(), moved)
	if tuple := mover.currentParent(t, "mover"); tuple.ToID != mover.address("right") {
		t.Errorf("after the move the block is part of %s, want %s", tuple.ToID, mover.address("right"))
	}
	history, err := graphstore.History(ctx, database.Pool, BlockNodeKind, mover.address("mover"), graphstore.TuplePartOf)
	if err != nil {
		t.Fatalf("read the history of the moved block's parent: %v", err)
	}
	if !slices.ContainsFunc(history, func(tuple graphstore.Tuple) bool {
		return !tuple.Asserted && tuple.ToID == mover.address("left")
	}) {
		t.Errorf("the history of the moved block is %v, want the retraction of the parent it left", history)
	}
	wantMoved := map[string]string{"left": "", "right": "", "mover": "right"}
	if got := mover.projectedParents(t); !mapsEqual(got, wantMoved) {
		t.Errorf("after the move document_blocks projects %v, want %v", got, wantMoved)
	}
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

// retractMidParent ends mid's part_of behind the index's back, as a writer that moved the block
// out would: the text-only version that follows must state it again, which it does only if it
// read the parent of every block of the document rather than of the ones whose text it changed.
func retractMidParent(t *testing.T, database *store.Store, document parentDocument, record versionRecord) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the retraction: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := graphstore.Write(ctx, tx, nil, []graphstore.Tuple{{
		FromKind: BlockNodeKind, FromID: document.address("mid"), Kind: graphstore.TuplePartOf,
		ToKind: BlockNodeKind, ToID: document.address("top1"),
		At: record.at, Who: record.writer, SourceEvent: graphstore.NoEvent, Asserted: false,
	}}); err != nil {
		t.Fatalf("retract mid's parent: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the retraction: %v", err)
	}
}

// statesParent reports whether stated holds a part_of tuple out of address with that assertion.
func statesParent(stated []graphstore.Tuple, address string, asserted bool) bool {
	return slices.ContainsFunc(stated, func(tuple graphstore.Tuple) bool {
		return tuple.Kind == graphstore.TuplePartOf && tuple.FromID == address && tuple.Asserted == asserted
	})
}

// mapsEqual compares two readings of the same set of blocks.
func mapsEqual(got, want map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	for key, value := range want {
		if got[key] != value {
			return false
		}
	}
	return true
}
