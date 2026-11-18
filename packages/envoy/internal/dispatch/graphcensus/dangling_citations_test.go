package graphcensus

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// restoreFixture is an issue whose answers left its spec in both shapes of the restore
// (dispatch://INFRA-1150 R6): the spec, its decisions document and a third document that cites,
// and the asks.
type restoreFixture struct {
	spec, decisions, notes testDocument
	// returning is the ask whose block left the spec, moving the ask whose block sits in the
	// decisions document, and colliding a returning ask whose block id a paragraph of the spec
	// holds now.
	returning, moving, colliding string
}

// testDocument is one document of the fixture: its id and its address.
type testDocument struct{ id, refKey string }

// seedRestoreFixture writes issue CORE-1 with its spec, a decisions document and a notes document,
// each at version 1 with its block index standing at that version, and three answered asks:
//
//   - returning, whose row names the spec and whose block ask-a no block of the spec holds;
//   - colliding, whose row names the spec and whose block id held-x a paragraph of the spec holds;
//   - moving, an ask block ask-b of the decisions document, whose version 1 recorded its text.
//
// Each ask's node states it answered, as asks.Settle does.
func seedRestoreFixture(t *testing.T, database *store.Store) restoreFixture {
	t.Helper()
	ctx := context.Background()
	exec := func(what, statement string, args ...any) {
		t.Helper()
		if _, err := database.Pool.Exec(ctx, statement, args...); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	exec("seed the issue", `
		insert into projects (key, name, default_assignee) values ('CORE', 'Core', 'alice');
		insert into issues (key, project_key, number, title, created_by, rank) values
			('CORE-1', 'CORE', 1, 'One', '{"kind":"user","id":"alice"}', 'U');
	`)
	document := func(slug, name string, primary bool, role *string) testDocument {
		t.Helper()
		var written testDocument
		if err := database.Pool.QueryRow(ctx, `
			insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, role, created_by)
			values ('CORE-1', 'CORE', $1, $2, 'doc', $3, $4, '{"kind":"user","id":"alice"}')
			returning id::text, ref_key
		`, slug, name, primary, role).Scan(&written.id, &written.refKey); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		exec("write version 1 of "+name, `
			insert into artifact_versions (artifact_id, number, markdown, authors)
			values ($1::uuid, 1, 'Version one.', '[{"kind":"user","id":"alice"}]')
		`, written.id)
		exec("stand the block index of "+name, `update document_block_versions set version = 1 where artifact_id = $1::uuid`, written.id)
		return written
	}
	// The role a decisions document an earlier release stored carries.
	decisionsRole := "decisions"
	fixture := restoreFixture{
		spec:      document("spec", "spec.md", true, nil),
		decisions: document("decisions-md", "decisions.md", false, &decisionsRole),
		notes:     document("notes-md", "notes.md", false, nil),
	}
	insertTestBlock(t, database, fixture.spec, "held-x", "paragraph")
	insertTestBlock(t, database, fixture.decisions, "ask-b", "ask")
	insertTestBlock(t, database, fixture.notes, "cites", "paragraph")
	fixture.returning = seedAnsweredAsk(t, database, fixture.spec, "ask-a")
	fixture.colliding = seedAnsweredAsk(t, database, fixture.spec, "held-x")
	fixture.moving = seedAnsweredAsk(t, database, fixture.decisions, "ask-b")
	return fixture
}

// insertTestBlock writes block blockID of document as a current block of type blockType, and the
// text version 1 recorded of it.
func insertTestBlock(t *testing.T, database *store.Store, document testDocument, blockID, blockType string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at)
		values ($1::uuid, $2, $3, $4, '', $5, 1, now());
	`, document.id, blockID, document.refKey+"#"+blockID, blockType, "Block "+blockID+"."); err != nil {
		t.Fatalf("write block %s of %s: %v", blockID, document.refKey, err)
	}
	if _, err := database.Pool.Exec(context.Background(), `
		insert into document_block_texts (artifact_id, block_id, version, text) values ($1::uuid, $2, 1, $3)
	`, document.id, blockID, "Block "+blockID+"."); err != nil {
		t.Fatalf("record the text of block %s of %s: %v", blockID, document.refKey, err)
	}
}

// seedAnsweredAsk writes an answered question ask of CORE-1 whose row names block blockID of
// document, with its node stating it answered, and answers its id.
func seedAnsweredAsk(t *testing.T, database *store.Store, document testDocument, blockID string) string {
	t.Helper()
	ctx := context.Background()
	var id string
	if err := database.Pool.QueryRow(ctx, `
		insert into asks (issue_key, author, question, anchor, block_artifact_id, block_id, state, answer)
		values ('CORE-1', '{"kind":"user","id":"alice"}', 'Ship it?', jsonb_build_object('artifact_id', $1::text),
		        $1::uuid, $2, 'answered', jsonb_build_object('user', 'bob', 'text', 'Yes.', 'selected', '[]'::jsonb, 'at', now()))
		returning id::text
	`, document.id, blockID).Scan(&id); err != nil {
		t.Fatalf("write the ask of block %s: %v", blockID, err)
	}
	at := time.Now().UTC()
	writeTestTuples(t, database, []graphstore.Node{graphstore.AskNode(id, "CORE", nil, censusWriter, at)}, []graphstore.Tuple{{
		FromKind: graphstore.KindAsk, FromID: id, Kind: graphstore.TupleAnswerState,
		ToKind: graphstore.KindValue, ToID: asks.StateAnswered,
		At: at, Who: censusWriter, SourceEvent: graphstore.NoEvent, Asserted: true,
	}})
	return id
}

// writeTestTuples writes nodes and tuples through the store's writer in a transaction it commits.
func writeTestTuples(t *testing.T, database *store.Store, nodes []graphstore.Node, tuples []graphstore.Tuple) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the store write: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := graphstore.Write(ctx, tx, nodes, tuples); err != nil {
		t.Fatalf("write the store: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the store write: %v", err)
	}
}

// citeFromComment writes the refs row of a comment's citation of target, and citeFromNotes the
// refs row and the block_refs row a block of the notes document writes for one: two rows, and one
// node that cites.
func citeFromComment(t *testing.T, database *store.Store, comment, target string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into refs (from_kind, from_id, to_kind, to_id) values ('comment', $1, 'block', $2)
	`, comment, target); err != nil {
		t.Fatalf("cite %s from comment %s: %v", target, comment, err)
	}
}

func citeFromNotes(t *testing.T, database *store.Store, notes testDocument, target string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into refs (from_kind, from_id, to_kind, to_id) values ('artifact', $1, 'block', $2)
	`, notes.id, target); err != nil {
		t.Fatalf("cite %s from %s: %v", target, notes.refKey, err)
	}
	if _, err := database.Pool.Exec(context.Background(), `
		insert into block_refs (artifact_id, block_id, from_node, to_kind, to_id, created_version)
		values ($1::uuid, 'cites', $3, 'block', $2, 1)
	`, notes.id, target, notes.refKey+"#cites"); err != nil {
		t.Fatalf("cite %s from a block of %s: %v", target, notes.refKey, err)
	}
}

// danglingOf is the counter's read of the database, in two sets of the citing node and its target.
func danglingOf(t *testing.T, database *store.Store) (moved, returning []string) {
	t.Helper()
	report, err := DanglingCitations(context.Background(), database.Pool)
	if err != nil {
		t.Fatalf("count the dangling citations: %v", err)
	}
	for _, citation := range report.Citations {
		cited := citation.SourceKind + ":" + citation.SourceID + " -> " + citation.Target
		switch citation.Set {
		case DanglingMoved:
			moved = append(moved, cited)
		case DanglingReturning:
			returning = append(returning, cited)
		default:
			t.Fatalf("the counter counted %+v in a set it has none of", citation)
		}
	}
	if int64(len(moved)) != report.Count(DanglingMoved) || int64(len(returning)) != report.Count(DanglingReturning) {
		t.Fatalf("the report counts %d and %d, and holds %v and %v", report.Count(DanglingMoved), report.Count(DanglingReturning), moved, returning)
	}
	return moved, returning
}

// The dangling-citation counter of dispatch://INFRA-1150 R8 reads its two sets over the restore's
// work (ReadRestoreWork), before and after the move, and counts a node that cites one target from
// its text and its block once.
//
// Before the restore the moved set reads zero, since the decisions document still holds the
// block both its citations name, and the returning set reads the two nodes that cite the spec's old
// address of the returning ask; a prose citation of an address the decisions document never held,
// and a citation of an address the colliding ask's block id names, which a paragraph of the spec
// holds, are no dangling citation. The collision census names that block id. The census's own lines
// print both.
//
// The move of the second shape, written without its moved_to tuple, leaves both citations of the
// old address dangling in the decisions document, and the moved set counts them (the control),
// and goes on counting them once the document is retired, as the step retires it in the
// transaction that empties it; with the tuple, they resolve and it reads zero, and once the
// returning ask's block is back in the spec, the returning set reads zero too.
func TestTheDanglingCounterReadsBothSetsAroundAMove(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	fixture := seedRestoreFixture(t, database)
	oldAddress := fixture.decisions.refKey + "#ask-b"
	newAddress := fixture.spec.refKey + "#ask-b"
	returningAddress := fixture.spec.refKey + "#ask-a"
	firstComment, secondComment := "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	citeFromComment(t, database, firstComment, oldAddress)
	citeFromNotes(t, database, fixture.notes, oldAddress)
	citeFromComment(t, database, secondComment, returningAddress)
	citeFromNotes(t, database, fixture.notes, returningAddress)
	citeFromComment(t, database, firstComment, fixture.decisions.refKey+"#never-held")
	citeFromComment(t, database, firstComment, fixture.spec.refKey+"#held-x")

	work, err := ReadRestoreWork(ctx, database.Pool)
	if err != nil {
		t.Fatalf("read the restore's work: %v", err)
	}
	wantWork := []RestoreWork{
		{Shape: RestoreMoves, Issue: "CORE-1", Ask: fixture.moving, Address: oldAddress, BlockID: "ask-b", Spec: fixture.spec.refKey},
		{Shape: RestoreReturns, Issue: "CORE-1", Ask: fixture.returning, Address: returningAddress, BlockID: "ask-a", Spec: fixture.spec.refKey},
		{Shape: RestoreReturns, Issue: "CORE-1", Ask: fixture.colliding, Address: fixture.spec.refKey + "#held-x", BlockID: "held-x", Spec: fixture.spec.refKey, Collides: true},
	}
	if !reflect.DeepEqual(work, wantWork) {
		t.Fatalf("the restore's work is\n%+v\nwant\n%+v", work, wantWork)
	}
	if collisions := CollisionsOf(work); !reflect.DeepEqual(collisions, []Collision{{Issue: "CORE-1", BlockID: "held-x"}}) {
		t.Fatalf("the collision census is %+v, want held-x of CORE-1 alone", collisions)
	}

	moved, returning := danglingOf(t, database)
	wantReturning := []string{"artifact:" + fixture.notes.id + " -> " + returningAddress, "comment:" + secondComment + " -> " + returningAddress}
	if len(moved) != 0 || !reflect.DeepEqual(returning, wantReturning) {
		t.Fatalf("before the move the counter reads moved %v and returning %v, want none and %v", moved, returning, wantReturning)
	}
	report := storeCensusOf(t, database)
	var printed strings.Builder
	report.WriteVerbose(&printed)
	for _, line := range []string{
		"census: dangling_citations moved=0 returning=2\n",
		"census: dangling_citation set=returning from=comment:" + secondComment + " to=" + returningAddress + "\n",
		"census: collisions issue=CORE-1 block=held-x\n",
	} {
		if !strings.Contains(printed.String(), line) {
			t.Fatalf("the census prints no line %q:\n%s", line, printed.String())
		}
	}

	// The move of the second shape, as the restore makes it, but with no moved_to tuple: the block
	// leaves the decisions document and stands in the spec, and the ask's row names the spec.
	if _, err := database.Pool.Exec(ctx, `
		delete from document_blocks where artifact_id = $1::uuid and block_id = 'ask-b'
	`, fixture.decisions.id); err != nil {
		t.Fatalf("move the ask block out of the decisions document: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `update asks set block_artifact_id = $1::uuid where id = $2::uuid`, fixture.spec.id, fixture.moving); err != nil {
		t.Fatalf("name the spec on the moved ask's row: %v", err)
	}
	insertTestBlock(t, database, fixture.spec, "ask-b", "ask")
	moved, _ = danglingOf(t, database)
	wantMoved := []string{"artifact:" + fixture.notes.id + " -> " + oldAddress, "comment:" + firstComment + " -> " + oldAddress}
	if !reflect.DeepEqual(moved, wantMoved) {
		t.Fatalf("a move that wrote no moved_to reads the moved set %v, want its two citations %v", moved, wantMoved)
	}

	// The document retired, as docs.RetireDocument retires it - retired_at set and its role
	// cleared - in the transaction that emptied it: the citations of its address still dangle.
	if _, err := database.Pool.Exec(ctx, `update artifacts set retired_at = now(), role = null where id = $1::uuid`, fixture.decisions.id); err != nil {
		t.Fatalf("retire the decisions document: %v", err)
	}
	if moved, _ = danglingOf(t, database); !reflect.DeepEqual(moved, wantMoved) {
		t.Fatalf("once the decisions document is retired the moved set reads %v, want the move's two citations %v still", moved, wantMoved)
	}

	writeTestTuples(t, database, nil, []graphstore.Tuple{{
		FromKind: refs.BlockNodeKind, FromID: oldAddress, Kind: graphstore.TupleMovedTo, ToKind: refs.BlockNodeKind, ToID: newAddress,
		At: time.Now().UTC(), Who: censusWriter, SourceEvent: graphstore.NoEvent, Asserted: true,
	}})
	insertTestBlock(t, database, fixture.spec, "ask-a", "ask")
	moved, returning = danglingOf(t, database)
	if len(moved) != 0 || len(returning) != 0 {
		t.Fatalf("after the move and the return the counter reads moved %v and returning %v, want none of either", moved, returning)
	}
}
