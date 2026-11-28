package docs

import (
	"context"
	"slices"
	"testing"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// formsRoomBlock is the id of the block of the spec's fragment that cites CORE-2 by a tab address
// in the room formsOwedRoom writes.
const formsRoomBlock = "forms-tab"

// formsOwedRoom writes, on a database blockIndexService seeds, the issue room DOC-1
// (createIssueRoom) as a build that predates the forms writes left it, and beside it the notes of
// CORE-1, a document of no room, as that build left them (createIndexedNotes). The room's stored
// history holds the plan's seed and a paragraph of the spec's fragment citing CORE-2 by a tab
// address, which that build's grammar did not read, and the spec's and the plan's latest versions
// hold their fragments' texts. Every artifact of the room stands at its latest markdown version,
// or at 0 where it holds none, with neither mark a pass is listed by (a null version, store_owed_at),
// as that build's index of the room leaves it: so no pass over the room wins its revision fence, nor
// finds the room owed, unless something owes it a block index. Where treeHoldsNoVersion is set the
// tree artifact holds no markdown version, so the room's pass covers no version of it. Every
// artifact of the room and the notes are owed to the forms queue.
func formsOwedRoom(t *testing.T, treeHoldsNoVersion bool) (*store.Store, *Service, issueRoom, string) {
	t.Helper()
	ctx := context.Background()
	database, service := blockIndexService(t)
	room := createIssueRoom(t, database, 1)
	seed := crdt.New()
	seed.Transact(writeParagraph(seed, room.plan, "Plan seed."))
	// The fragment is taken before the transaction, as writeParagraph takes it: GetXmlFragment
	// takes the document's lock, which Transact holds for the length of its function.
	specFragment := seed.GetXmlFragment(room.spec)
	seed.Transact(func(txn *crdt.Transaction) {
		paragraph := crdt.NewYXmlElement("paragraph")
		specFragment.InsertElement(txn, specFragment.Len(), paragraph)
		paragraph.SetAttribute(txn, pmdoc.BlockIDAttr, formsRoomBlock)
		run := crdt.NewYXmlText()
		paragraph.InsertText(txn, 0, run)
		run.Insert(txn, 0, "The plan: dispatch://CORE-2/plan.", nil)
	})
	specTree, err := treeOfFragment(seed, room.spec)
	if err != nil {
		t.Fatalf("read the spec's fragment: %v", err)
	}
	spec, err := pmdoc.Render(specTree)
	if err != nil {
		t.Fatalf("render the spec's fragment: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `update doc_updates set "update" = $2 where artifact_id = $1 and version = 1`,
		room.tree, crdt.EncodeStateAsUpdateV1(seed, nil)); err != nil {
		t.Fatalf("store the room's history: %v", err)
	}
	for document, markdown := range map[string]string{room.spec: spec, room.plan: planSeed} {
		if _, err := database.Pool.Exec(ctx, `
			insert into artifact_versions (artifact_id, number, markdown, authors)
			values ($1, 2, $2, '[{"kind":"user","id":"alice"}]')
		`, document, markdown); err != nil {
			t.Fatalf("write version 2 of %s: %v", document, err)
		}
	}
	if treeHoldsNoVersion {
		if _, err := database.Pool.Exec(ctx, `update artifact_versions set markdown = null where artifact_id = $1`, room.tree); err != nil {
			t.Fatalf("leave the tree artifact no markdown version: %v", err)
		}
	}
	artifacts := []string{room.tree, room.spec, room.verification, room.plan}
	if _, err := database.Pool.Exec(ctx, `
		update document_block_versions b
		set version = coalesce((select max(v.number) from artifact_versions v
		                        where v.artifact_id = b.artifact_id and v.markdown is not null), 0),
		    store_owed_at = null
		where b.artifact_id = any($1::uuid[])
	`, artifacts); err != nil {
		t.Fatalf("stand the room's artifacts as an older build's index of the room leaves them: %v", err)
	}
	var marked int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from document_block_versions
		where artifact_id = any($1::uuid[]) and (version is null or store_owed_at is not null)
	`, artifacts).Scan(&marked); err != nil {
		t.Fatalf("read the room's marks: %v", err)
	}
	if marked != 0 {
		t.Fatalf("%d artifacts of the room hold a block-index mark before the drain, want none", marked)
	}
	notes := createIndexedNotes(t, database, service, "The plan: dispatch://CORE-2/plan.\n")
	owed := formsOwedRows(t, database)
	for _, document := range append(artifacts, notes) {
		if _, held := owed["artifact "+document]; !held {
			t.Fatalf("the forms queue holds %v, want a row of %s, which an older build's version owes", owed, document)
		}
	}
	return database, service, room, notes
}

// drainFormsCountingPasses runs one pass of a forms drain from the start of the queue
// (reconcileRefsForms), counting each block-index pass it commits by the artifact the pass was run
// for (Service.afterBlockIndexWrite): a pass over one document for that document, and a pass over
// an issue room for the room's artifact whose row drove it (indexOwedRoom).
func drainFormsCountingPasses(t *testing.T, service *Service) (RefsFormsTick, map[string]int) {
	t.Helper()
	passes := map[string]int{}
	service.afterBlockIndexWrite = func(artifactID string) error {
		passes[artifactID]++
		return nil
	}
	defer func() { service.afterBlockIndexWrite = nil }()
	return reconcileRefsForms(t, service), passes
}

// requireFormsRoomDrained fails unless tick, a forms drain's pass over formsOwedRoom's room and
// notes, indexed the room once and the notes once and left nothing owed: every row of the forms
// queue settled, the room's references citing CORE-2 under its tree artifact, the spec's block
// holding its mention under the tree artifact and naming the spec as its document, no block row
// keyed by a document of the room, and no mark of the room left. The notes are drained as a
// document of no room is: their own edge, their block's mention, their index at version 1.
func requireFormsRoomDrained(t *testing.T, database *store.Store, service *Service, room issueRoom, notes string, tick RefsFormsTick) {
	t.Helper()
	ctx := context.Background()
	if tick.Documents != 2 || tick.Unindexed != 0 || tick.LiveAhead != 0 || tick.Unreadable != 0 || tick.Gone != 0 || tick.OwedTexts != 0 {
		t.Fatalf("pass = %#v, want the room indexed once and the notes once, and nothing left owed", tick)
	}
	if owed := formsOwedRows(t, database); len(owed) != 0 {
		t.Fatalf("the forms queue holds %v after the pass, want it empty", owed)
	}
	if !citesIssue(t, database, "artifact", room.tree, "CORE-2") {
		t.Errorf("the room's references hold %#v under its tree artifact, want an edge to CORE-2", refsFrom(t, database, "artifact", room.tree))
	}
	if !slices.ContainsFunc(blockPairsOf(t, database, room.tree), func(pair blockPair) bool {
		return pair.BlockID == formsRoomBlock && pair.ToKind == "issue" && pair.ToID == "CORE-2"
	}) {
		t.Errorf("the room's blocks hold %#v, want block %s citing CORE-2", blockPairsOf(t, database, room.tree), formsRoomBlock)
	}
	var placed string
	if err := database.Pool.QueryRow(ctx, `
		select coalesce((select document_id::text from issue_room_blocks where tree_artifact_id = $1 and block_id = $2), '')
	`, room.tree, formsRoomBlock).Scan(&placed); err != nil {
		t.Fatalf("read the document of block %s: %v", formsRoomBlock, err)
	}
	if placed != room.spec {
		t.Errorf("block %s of the room names document %q, want the spec %s", formsRoomBlock, placed, room.spec)
	}
	var alone int
	if err := database.Pool.QueryRow(ctx, `select count(*) from document_blocks where artifact_id = any($1::uuid[])`,
		[]string{room.spec, room.verification, room.plan}).Scan(&alone); err != nil {
		t.Fatalf("count the block rows keyed by a document of the room: %v", err)
	}
	if alone != 0 {
		t.Errorf("%d block rows are keyed by a document of the room, want none: a fragment was indexed alone", alone)
	}
	requireNoMarkOfTheRoom(t, database, room, "the forms drain's pass over the room")

	assertRef(t, database, "artifact", notes, "issue", "CORE-2", refsGrammarWritten)
	holder := holderOf(t, service, notes, "dispatch://CORE-2/plan")
	if !slices.ContainsFunc(blockPairsOf(t, database, notes), func(pair blockPair) bool {
		return pair.BlockID == holder && pair.ToKind == "issue" && pair.ToID == "CORE-2"
	}) {
		t.Errorf("the notes' blocks hold %#v, want block %s citing CORE-2", blockPairsOf(t, database, notes), holder)
	}
	if at := blockIndexAt(t, database, notes); at == nil || *at != 1 {
		t.Errorf("the notes' block index is at %v after the drain indexed them, want version 1", at)
	}

	if next := reconcileRefsForms(t, service); next.Documents != 0 || next.Gone != 0 || next.Unindexed != 0 || next.OwedTexts != 0 {
		t.Errorf("the pass after it = %#v, want nothing owed and nothing indexed", next)
	}
	if block := reconcileBlockIndex(t, service, ""); block.Documents != 0 || block.LiveAhead != 0 || block.Unreadable != 0 {
		t.Errorf("the block index's tick after the drain = %#v, want nothing of the room or the notes indexed again", block)
	}
}

// The forms drain indexes a document of an issue room through the room's pass and never alone,
// since no pass indexes one fragment of a room alone, and a document of no room as before. One
// pass of the drain indexes the room once, whichever of its artifacts' rows it takes first, and in
// that pass's own transaction settles the forms row of every artifact of the room: the tree
// artifact's with the room's references, and each document's beside them. The room stands as an
// older build's index left it, owed no block index (formsOwedRoom), so only the block index the
// drain owes the room lets its pass find the room owed and win its fence.
//
// Where the tree artifact holds no markdown version, the room's pass covers no version of it, so
// a debt of the tree artifact's own would let that pass win nothing. The tree artifact's row is
// listed first there, so it is the row the room is drained for, and the drain owes the block index
// of a document the pass covers instead (roomPassDebt): the pass wins, and every row of the room is
// settled all the same.
func TestTheFormsDrainIndexesADocumentOfAnIssueRoomThroughItsRoom(t *testing.T) {
	t.Run("the tree artifact holds a version", func(t *testing.T) {
		database, service, room, notes := formsOwedRoom(t, false)

		tick, passes := drainFormsCountingPasses(t, service)

		requireFormsRoomDrained(t, database, service, room, notes, tick)
		if held := passes[room.tree] + passes[room.spec] + passes[room.verification] + passes[room.plan]; held != 1 {
			t.Errorf("the pass ran %d block-index passes over the room (%v), want one whichever of its four rows it took", held, passes)
		}
		if passes[notes] != 1 {
			t.Errorf("the pass ran %d block-index passes over the notes (%v), want one", passes[notes], passes)
		}
	})

	t.Run("the tree artifact holds no markdown version", func(t *testing.T) {
		database, service, room, notes := formsOwedRoom(t, true)
		if _, err := database.Pool.Exec(context.Background(), `
			update refs_forms_owed set enqueued_at = (select min(enqueued_at) from refs_forms_owed) - interval '1 second'
			where kind = 'artifact' and id = $1::uuid
		`, room.tree); err != nil {
			t.Fatalf("list the tree artifact's row first: %v", err)
		}

		tick, passes := drainFormsCountingPasses(t, service)

		if passes[room.tree] != 1 || passes[room.spec]+passes[room.verification]+passes[room.plan] != 0 {
			t.Errorf("the pass ran the room's block-index passes for %v, want one, for the tree artifact's row it listed first", passes)
		}
		requireFormsRoomDrained(t, database, service, room, notes, tick)
	})
}
