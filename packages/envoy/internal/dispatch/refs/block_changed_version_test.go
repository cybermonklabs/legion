package refs

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// windowServer is the server this file's references are read against.
const windowServer = "https://dispatch.example"

// writeStoredVersion writes version number of the document as the version write stores it, by
// roomAuthor (writeAuthoredVersion): its markdown, under the database's clock at the statement
// (clock_timestamp, so two versions one transaction writes read two times), and answers that time.
func writeStoredVersion(t *testing.T, tx pgx.Tx, artifact string, number int, markdown string) time.Time {
	t.Helper()
	return writeAuthoredVersion(t, tx, artifact, number, markdown, roomAuthor)
}

// blockChanges is every block row's changed_version and changed_at (blockChange), by block id,
// read in the fixture's transaction.
func blockChanges(t *testing.T, tx pgx.Tx, artifact string) map[string]blockChange {
	t.Helper()
	rows, err := tx.Query(context.Background(), `
		select block_id, changed_version, changed_at from document_blocks where artifact_id = $1::uuid
	`, artifact)
	if err != nil {
		t.Fatalf("read the block rows: %v", err)
	}
	defer rows.Close()
	changes := map[string]blockChange{}
	for rows.Next() {
		var id string
		var change blockChange
		if err := rows.Scan(&id, &change.version, &change.at); err != nil {
			t.Fatalf("read a block row: %v", err)
		}
		changes[id] = change
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the block rows: %v", err)
	}
	return changes
}

// One block-index pass can cover more than one version: a document past the deferral bound, or
// versions a build before wrote during the roll, is indexed by the reconciliation at its latest
// owed version alone (docs' indexOwedDocument). A block's text-change time is read off its row
// (c-spec-tie), so the row dates the change to the version that made it,
// and not to the version the pass indexed. Version 1 is indexed; version 2 rewrites b1, version 3
// leaves b1 alone and rewrites b2, and one pass indexes version 3. b1's row then reads version 2
// and version 2's time, and its text is recorded at version 2, which is the text read at version
// 3; b2's reads version 3 and its time. A pass that took every changed block's version from the
// version it indexed dates b1's rewrite to version 3.
func TestAPassOverSeveralVersionsDatesATextChangeToTheVersionThatMadeIt(t *testing.T) {
	ctx := context.Background()
	tx := windowFixture(t)
	artifact, _ := createReferenceArtifact(t, tx, "WIND-1", "WIND", "spec-md")
	first, firstBlocks := documentOf([]part{{"b1", "paragraph", "One."}, {"b2", "paragraph", "Two."}})
	firstAt := writeStoredVersion(t, tx, artifact, 1, first)
	if _, _, _, _, err := IndexBlocks(ctx, tx, artifact, OneVersion(artifact, 1, firstAt), first, firstBlocks, nil, nil, "", TypedFacts{}); err != nil {
		t.Fatalf("index version 1: %v", err)
	}
	second, _ := documentOf([]part{{"b1", "paragraph", "One, rewritten."}, {"b2", "paragraph", "Two."}})
	secondAt := writeStoredVersion(t, tx, artifact, 2, second)
	third, thirdBlocks := documentOf([]part{{"b1", "paragraph", "One, rewritten."}, {"b2", "paragraph", "Two, rewritten."}})
	thirdAt := writeStoredVersion(t, tx, artifact, 3, third)
	if !secondAt.Before(thirdAt) {
		t.Fatalf("version 2 was written at %s and version 3 at %s, want two times of the database's clock in order", secondAt, thirdAt)
	}
	if _, won, _, _, err := IndexBlocks(ctx, tx, artifact, OneVersion(artifact, 3, thirdAt), third, thirdBlocks, nil, nil, "", TypedFacts{}); err != nil || !won {
		t.Fatalf("index version 3 in one pass over versions 2 and 3: won %t, %v", won, err)
	}

	changes := blockChanges(t, tx, artifact)
	if got := changes["b1"]; got.version != 2 || !got.at.Equal(secondAt) {
		t.Errorf("b1, which version 2 rewrote and version 3 left alone, changed at version %d, %s, want version 2, %s",
			got.version, got.at, secondAt)
	}
	if got := changes["b2"]; got.version != 3 || !got.at.Equal(thirdAt) {
		t.Errorf("b2, which version 3 rewrote, changed at version %d, %s, want version 3, %s", got.version, got.at, thirdAt)
	}
	var recorded int
	var text string
	if err := tx.QueryRow(ctx, `
		select version, text from document_block_texts
		where artifact_id = $1::uuid and block_id = 'b1' and version <= 3
		order by version desc limit 1
	`, artifact).Scan(&recorded, &text); err != nil {
		t.Fatalf("read b1's text at version 3: %v", err)
	}
	if recorded != 2 || text != "One, rewritten." {
		t.Errorf("b1's text at version 3 is the row of version %d holding %q, want version 2's, %q", recorded, text, "One, rewritten.")
	}
}

// roomFixture writes, in the fixture's transaction, issue WIND-1 converted into its room: its spec
// and its Verification document, its tree artifact with its issue_trees row, and the two
// documents' document_rooms rows naming the tree, and answers their ids and ref_keys.
func roomFixture(t *testing.T, tx pgx.Tx) (spec, specRef, verification, tree, treeRef string) {
	t.Helper()
	ctx := context.Background()
	spec, specRef = createReferenceArtifact(t, tx, "WIND-1", "WIND", "spec-md")
	if err := tx.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ('WIND-1', 'WIND', 'verification-md', 'verification.md', 'doc', false, '{"kind":"user","id":"alice"}')
		returning id::text`).Scan(&verification); err != nil {
		t.Fatalf("create the Verification document: %v", err)
	}
	tree, treeRef = insertIssueRoom(t, tx, "WIND-1", "WIND", spec, verification)
	return spec, specRef, verification, tree, treeRef
}

// insertIssueRoom writes, in the fixture's transaction, the room of issue issueKey of project as
// a conversion records it: the issue's tree artifact, its issue_trees row, and a document_rooms
// row naming the tree for each of documents. It answers the tree's id and ref_key.
func insertIssueRoom(t *testing.T, tx pgx.Tx, issueKey, project string, documents ...string) (tree, treeRef string) {
	t.Helper()
	ctx := context.Background()
	if err := tx.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ($1, $2, 'tree', 'tree', 'tree', false, '{"kind":"user","id":"alice"}')
		returning id::text, ref_key
	`, issueKey, project).Scan(&tree, &treeRef); err != nil {
		t.Fatalf("create the tree of %s: %v", issueKey, err)
	}
	if _, err := tx.Exec(ctx, `
		with held as (insert into issue_trees (issue_key, artifact_id) values ($1, $2::uuid))
		insert into document_rooms (artifact_id, room) select document, $2::uuid from unnest($3::uuid[]) as document
	`, issueKey, tree, documents); err != nil {
		t.Fatalf("move the documents of %s into its room: %v", issueKey, err)
	}
	return tree, treeRef
}

// A block of an issue room cut from one fragment and pasted with the same text into another moves
// into the other fragment's document (T12a of the issue-tree plan): its issue_room_blocks row
// names that document, and it records that document's version as its changed_version and as
// its newest text row, so the text a reader asks of the block at that document's version is
// there. Block x stands in the spec's fragment at the spec's version 4, then in the Verification
// document's fragment at its version 2; a pass that kept x's row because its text did not change
// would keep version 4, and the Verification document's version 2 would hold no text of x. Each
// pass over the room records each document's row stated at the version it covers of that
// document, by BlockIndexMarkingRule (queueStand's arm over several documents).
func TestIndexBlocksRecordsARoomBlockMovedIntoAnotherFragmentAtItsNewDocumentsVersion(t *testing.T) {
	ctx := context.Background()
	tx := windowFixture(t)
	spec, _, verification, tree, _ := roomFixture(t, tx)
	treeAt := writeStoredVersion(t, tx, tree, 1, "")
	specAt := writeStoredVersion(t, tx, spec, 4, "Moved words.")
	verificationAt := writeStoredVersion(t, tx, verification, 1, "Check.")
	first, firstBlocks := documentOf([]part{{"y", "paragraph", "Check."}, {"x", "paragraph", "Moved words."}})
	firstBlocks[0].Document, firstBlocks[1].Document = verification, spec
	if _, won, _, _, err := IndexBlocks(ctx, tx, tree, Versions{
		tree: {Number: 1, At: treeAt}, spec: {Number: 4, At: specAt}, verification: {Number: 1, At: verificationAt},
	}, first, firstBlocks, nil, nil, "", TypedFacts{}); err != nil || !won {
		t.Fatalf("index the room with x in the spec's fragment: won %t, %v", won, err)
	}
	for document, version := range map[string]int{tree: 1, spec: 4, verification: 1} {
		requireStated(t, tx, document, version, "after the room's first pass")
	}
	specNext := writeStoredVersion(t, tx, spec, 5, "")
	verificationNext := writeStoredVersion(t, tx, verification, 2, "Check.\n\nMoved words.")
	second, secondBlocks := documentOf([]part{{"y", "paragraph", "Check."}, {"x", "paragraph", "Moved words."}})
	secondBlocks[0].Document, secondBlocks[1].Document = verification, verification
	if _, won, _, _, err := IndexBlocks(ctx, tx, tree, Versions{
		tree: {Number: 1, At: treeAt}, spec: {Number: 5, At: specNext}, verification: {Number: 2, At: verificationNext},
	}, second, secondBlocks, nil, nil, "", TypedFacts{}); err != nil || !won {
		t.Fatalf("index the room with x in the Verification document's fragment: won %t, %v", won, err)
	}
	for document, version := range map[string]int{tree: 1, spec: 5, verification: 2} {
		requireStated(t, tx, document, version, "after the room's second pass")
	}

	var document string
	if err := tx.QueryRow(ctx, `
		select document_id::text from issue_room_blocks where tree_artifact_id = $1::uuid and block_id = 'x'
	`, tree).Scan(&document); err != nil {
		t.Fatalf("read x's issue_room_blocks row: %v", err)
	}
	if document != verification {
		t.Errorf("x's issue_room_blocks row names %s, want the Verification document %s", document, verification)
	}
	if got := blockChanges(t, tx, tree)["x"]; got.version != 2 || !got.at.Equal(verificationNext) {
		t.Errorf("x changed at version %d, %s, want the Verification document's version 2, %s", got.version, got.at, verificationNext)
	}
	read, err := BlockTextAt(ctx, tx, verification, "x", 2)
	if err != nil {
		t.Fatalf("read x at the Verification document's version 2: %v", err)
	}
	if read.Text == nil || *read.Text != "Moved words." {
		t.Errorf("x at the Verification document's version 2 reads %v, want %q", read.Text, "Moved words.")
	}
}

// A block of an issue room reads, as a node, as a block of its own document (T12a of the
// issue-tree plan, refs.BlockDocumentSQL): its document, its parent and the reference it is cited
// by are the spec's, whose fragment holds it, its last change is the spec's version 4
// at that version's time and by that version's authors, and its node's own address stays the
// tree's, which keys its row. A read that took the row's artifact as the block's document names
// the tree and dates the change by a version the tree never had. The blocks a document's
// references read takes as that document's - the set and the one array its "in" arm tests each
// edge against - are the rows of its fragment of the room, so a citation of a converted
// document's block is answered at that document.
func TestARoomBlocksNodeReadsAsABlockOfItsOwnDocument(t *testing.T) {
	ctx := context.Background()
	tx := windowFixture(t)
	spec, specRef, verification, tree, treeRef := roomFixture(t, tx)
	treeAt := writeStoredVersion(t, tx, tree, 1, "")
	specAt := writeStoredVersion(t, tx, spec, 4, "Room words.")
	verificationAt := writeStoredVersion(t, tx, verification, 1, "Check.")
	markdown, blocks := documentOf([]part{{"y", "paragraph", "Check."}, {"x", "paragraph", "Room words."}})
	blocks[0].Document, blocks[1].Document = verification, spec
	indexAndCause(t, tx, tree, Versions{
		tree: {Number: 1, At: treeAt}, spec: {Number: 4, At: specAt}, verification: {Number: 1, At: verificationAt},
	}, markdown, blocks)

	address := treeRef + "#x"
	node, found, err := Node(ctx, tx, model.Actor{}, BlockNodeKind, address, false)
	if err != nil || !found {
		t.Fatalf("read the node of block x: found %t, %v", found, err)
	}
	issue := "WIND-1"
	documentRef := ArtifactRef(&issue, "WIND", nil, "spec-md", "doc", true)
	if node.ID != address {
		t.Errorf("block x's node is %s, want the tree's address %s", node.ID, address)
	}
	if node.Document != documentRef || node.Parent == nil || node.Parent.ID != spec || node.Ref != BlockRef(documentRef, "x") {
		t.Errorf("block x reads in document %s (parent %+v) as %s, want the spec %s, %s", node.Document, node.Parent, node.Ref, documentRef, BlockRef(documentRef, "x"))
	}
	if node.ChangedVersion != 4 || node.ChangedAt == nil || !node.ChangedAt.Equal(specAt) {
		t.Errorf("block x last changed at version %d, %v, want the spec's version 4, %s", node.ChangedVersion, node.ChangedAt, specAt)
	}
	if len(node.ChangedBy) != 1 || node.ChangedBy[0].ID != "alice" {
		t.Errorf("block x was last changed by %+v, want the authors of the spec's version 4, alice", node.ChangedBy)
	}
	// The blocks a read of each document's blocks' citers takes (documentBlockIDs), and the same
	// set as the one array the "in" arm of a document's references read tests each edge against
	// (documentBlockIDArray): the spec's is x, the Verification document's y, and the tree's own
	// fragment holds none.
	verificationRef := storedAddress(t, tx, verification).RefKey
	for document, want := range map[string][]string{specRef: {address}, verificationRef: {treeRef + "#y"}, treeRef: nil} {
		rows, err := tx.Query(ctx, `select node_id from `+documentBlockIDs("$1")+` ids order by 1`, document)
		if err != nil {
			t.Fatalf("read the blocks of %s: %v", document, err)
		}
		got, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("read the blocks of %s: %v", document, err)
		}
		if !slices.Equal(got, want) {
			t.Errorf("the blocks of %s are %v, want %v", document, got, want)
		}
		var array []string
		if err := tx.QueryRow(ctx, `select coalesce(array_agg(id order by id), '{}') from unnest(`+documentBlockIDArray("$1")+`) id`, document).Scan(&array); err != nil {
			t.Fatalf("read the block array of %s: %v", document, err)
		}
		if !slices.Equal(array, want) {
			t.Errorf("the block array of %s is %v, want %v", document, array, want)
		}
	}
}

// A reference names `<document ref_key>#<block>`, and a block of an issue room is the node
// `<tree ref_key>#<block>` (T12a of the issue-tree plan). Every reader and writer of a reference
// resolves it in one order (T17): the address's current moved_to tuple first, followed hop by
// hop; then the room its document's document_rooms row names; then the address itself. On issue
// WIND-1, born converted, the Verification document's check k1 serves the spec's requirement r1,
// written at the spec's own address. The pointer validation takes it (BlockTargets, naming the
// spec as the block's document, never the tree); the pointer writer stores its block_refs row and
// its serves tuple at the room's node and hangs the check under it (placeAuthored); and the three
// resolvers reach that node: graphstore.MovedTo (blockReferenceNode's), BlockLinks and Closure.
// Two planted moved_to tuples show the order. The spec's old address `old` moved to the spec's r1,
// so a citation of it takes one hop and then the room. The Verification document's own address of
// r1 moved to its block r1v, as a re-mint writes one: the move comes before the room, so a
// citation of that address reaches r1v and never the spec's r1, which the room holds under the
// same id.
func TestAReferenceResolvesThroughMovedToBeforeTheRoom(t *testing.T) {
	ctx := context.Background()
	tx := windowFixture(t)
	spec, specRef, verification, tree, treeRef := roomFixture(t, tx)
	verificationRef := storedAddress(t, tx, verification).RefKey
	treeAt := writeStoredVersion(t, tx, tree, 1, "")
	specAt := writeStoredVersion(t, tx, spec, 1, "One.")
	verificationAt := writeStoredVersion(t, tx, verification, 1, "Checks one.")
	serves := "dispatch://WIND-1/artifact/spec-md#b-r1"
	cite := "Per dispatch://WIND-1/artifact/verification-md#b-r1."
	markdown, blocks := documentOf([]part{
		{"r1", "requirement", "One."}, {"k1", "check", "Checks one."}, {"r1v", "paragraph", "Verified one."}, {"cite", "paragraph", cite},
	})
	blocks[0].Document, blocks[1].Document, blocks[2].Document, blocks[3].Document = spec, verification, verification, verification
	target, ok := text.ParseRef(serves, windowServer)
	if !ok || target.Kind != BlockNodeKind {
		t.Fatalf("parse the check's serves %s: %#v", serves, target)
	}
	at := time.Now().UTC()
	if err := graphstore.Write(ctx, tx, nil, []graphstore.Tuple{
		{FromKind: BlockNodeKind, FromID: specRef + "#old", Kind: graphstore.TupleMovedTo, ToKind: BlockNodeKind, ToID: specRef + "#r1",
			At: at, Who: model.Actor{Kind: "user", ID: "alice"}, SourceEvent: graphstore.NoEvent, Asserted: true},
		{FromKind: BlockNodeKind, FromID: verificationRef + "#r1", Kind: graphstore.TupleMovedTo, ToKind: BlockNodeKind, ToID: treeRef + "#r1v",
			At: at, Who: model.Actor{Kind: "user", ID: "alice"}, SourceEvent: graphstore.NoEvent, Asserted: true},
	}); err != nil {
		t.Fatalf("plant the moves: %v", err)
	}

	// The pointer validation of the Verification document's write.
	check := func(reference string) error {
		t.Helper()
		return checkBlockPointers(t, tx, verification, ":::check{#k1 serves=\""+reference+"\"}\nChecks one.\n:::\n")
	}
	// The room's index, which the version write runs over every fragment of the room.
	_, won, write, _, err := IndexBlocks(ctx, tx, tree, Versions{
		tree: {Number: 1, At: treeAt}, spec: {Number: 1, At: specAt}, verification: {Number: 1, At: verificationAt},
	}, markdown, blocks, text.ExtractAt(markdown, windowServer),
		[]Pointer{{Block: "k1", BlockType: "check", Kind: "serves", Target: target, Targets: []string{"requirement"}}}, windowServer, TypedFacts{})
	if err != nil || !won {
		t.Fatalf("index the room: won %t, %v", won, err)
	}
	if err := graphstore.Caused(ctx, tx, write, graphstore.NoEvent); err != nil {
		t.Fatalf("append what the room's index stated: %v", err)
	}
	if err := check(serves); err != nil {
		t.Errorf("a check serving the spec's r1 at the spec's address = %v, want it taken", err)
	}
	if err := check("dispatch://WIND-1/artifact/spec-md#b-r9"); !errors.Is(err, pmdoc.ErrBlockPointer) || !strings.Contains(err.Error(), "which no document holds") {
		t.Errorf("a check serving a block no document holds = %v, want it refused", err)
	}
	_, found, err := BlockTargets(ctx, tx, verification, storedAddress(t, tx, verification), nil, []text.Ref{target})
	if err != nil {
		t.Fatalf("read the targets of the check's serves: %v", err)
	}
	if got, want := found[specRef+"#r1"], (pmdoc.BlockTarget{Types: []string{"requirement"}, Document: specRef, Issue: "WIND-1"}); !reflect.DeepEqual(got, want) {
		t.Errorf("the check's serves reads the target %+v, want %+v: the room's node, in the document whose fragment holds it", got, want)
	}

	// The pointer writer: the row, its tuple and the check's place.
	var rowTarget string
	if err := tx.QueryRow(ctx, `
		select to_id from block_refs where artifact_id = $1::uuid and block_id = 'k1' and kind = 'serves'
	`, tree).Scan(&rowTarget); err != nil {
		t.Fatalf("read the check's serves row: %v", err)
	}
	var tupleTargets []string
	if err := tx.QueryRow(ctx, `
		select coalesce(array_agg(to_id order by to_id), '{}') from graph_tuples
		where from_kind = 'block' and from_id = $1 and kind = 'serves' and asserted
	`, treeRef+"#k1").Scan(&tupleTargets); err != nil {
		t.Fatalf("read the check's serves tuples: %v", err)
	}
	var parent string
	if err := tx.QueryRow(ctx, `
		select to_kind || ':' || to_id from graph_tuples
		where from_kind = 'block' and from_id = $1 and kind = 'part_of' and asserted order by seq desc limit 1
	`, treeRef+"#k1").Scan(&parent); err != nil {
		t.Fatalf("read the check's part_of: %v", err)
	}
	if rowTarget != treeRef+"#r1" || !slices.Equal(tupleTargets, []string{treeRef + "#r1"}) || parent != "block:"+treeRef+"#r1" {
		t.Errorf("the check's serves row reaches %s, its tuples %v and its part_of %s, want the room's node %s for each",
			rowTarget, tupleTargets, parent, treeRef+"#r1")
	}

	// graphstore.MovedTo, the reading blockReferenceNode takes.
	for _, read := range []struct {
		from, want string
		reached    bool
	}{
		{specRef + "#r1", treeRef + "#r1", true}, {specRef + "#old", treeRef + "#r1", true},
		{verificationRef + "#r1", treeRef + "#r1v", true}, {treeRef + "#r1", treeRef + "#r1", true},
		{specRef + "#r9", "", false},
	} {
		if reached, ok, err := graphstore.MovedTo(ctx, tx, read.from); err != nil || ok != read.reached || reached.Address != read.want {
			t.Errorf("MovedTo(%s) = %q, %t (%v), want %q, %t", read.from, reached.Address, ok, err, read.want, read.reached)
		}
	}

	// BlockLinks: the serves from the Verification document to the spec, and the citation of the
	// Verification document's moved address, which reaches its own r1v.
	linked := func(from, to string) []string {
		t.Helper()
		links, err := BlockLinks(ctx, tx, model.Actor{}, from, to)
		if err != nil {
			t.Fatalf("read the block links from %s to %s: %v", from, to, err)
		}
		var got []string
		for _, link := range links {
			got = append(got, link.Kind+" "+link.From.ID+" -> "+link.To.ID)
		}
		return got
	}
	if got, want := linked(verification, spec), []string{"serves " + treeRef + "#k1 -> " + treeRef + "#r1"}; !slices.Equal(got, want) {
		t.Errorf("the block links from the Verification document to the spec are %v, want %v", got, want)
	}
	if got, want := linked(verification, verification), []string{"mentions " + treeRef + "#cite -> " + treeRef + "#r1v"}; !slices.Equal(got, want) {
		t.Errorf("the block links inside the Verification document are %v, want %v", got, want)
	}

	// Closure: a comment of WIND-2 cites the Verification document's moved address, and one of
	// WIND-3 the spec's old address.
	cited := map[string]string{"WIND-2": verificationRef + "#r1", "WIND-3": specRef + "#old"}
	for number, key := range []string{"WIND-2", "WIND-3"} {
		if _, err := tx.Exec(ctx, `
			insert into issues (key, project_key, number, title, created_by, rank)
			values ($1, 'WIND', $2, 'Citing issue', '{"kind":"user","id":"alice"}', 'V')
		`, key, number+2); err != nil {
			t.Fatalf("create issue %s: %v", key, err)
		}
		insertReference(t, tx, "comment", createReferenceComment(t, tx, key), BlockNodeKind, cited[key])
	}
	for key, want := range map[string]string{"WIND-2": verificationRef, "WIND-3": specRef} {
		references, err := Closure(ctx, tx, model.Actor{}, key, "")
		if err != nil {
			t.Fatalf("read the closure of %s: %v", key, err)
		}
		var got []string
		for _, member := range references.Members {
			got = append(got, member.RefKey)
		}
		if !slices.Equal(got, []string{want}) {
			t.Errorf("the closure of %s, whose comment cites %s, holds %v, want the document %s", key, cited[key], got, want)
		}
	}
}

// A pass over an issue room never moves a document's row back (queueStand's arm over several
// documents): the row of a document that stands past the version the pass covers of it is one the
// room's statement leaves as it stands, its mark included, and the index the pass leaves for a
// later indexing of the room in its transaction (BlockIndexing.Held) holds that row as the
// database does once the pass is written, the version it covers among those indexed. The
// Verification document's row stands at version 3 under a mark of this build's rule, as the tick's
// marking leaves a row whose statement is behind it, and the pass covers its version 2. An index
// that cleared the mark would read the document owed nothing at the next indexing of the room in
// the transaction (PlanBlocks' marked) while the database holds it owed.
func TestARoomPassLeavesTheRowOfADocumentPastTheVersionItCoversAsItStands(t *testing.T) {
	ctx := context.Background()
	tx := windowFixture(t)
	spec, _, verification, tree, _ := roomFixture(t, tx)
	treeAt := writeStoredVersion(t, tx, tree, 1, "")
	specAt := writeStoredVersion(t, tx, spec, 1, "Words.")
	verificationAt := writeStoredVersion(t, tx, verification, 1, "Check.")
	first, firstBlocks := documentOf([]part{{"y", "paragraph", "Check."}, {"x", "paragraph", "Words."}})
	firstBlocks[0].Document, firstBlocks[1].Document = verification, spec
	indexAndCause(t, tx, tree, Versions{
		tree: {Number: 1, At: treeAt}, spec: {Number: 1, At: specAt}, verification: {Number: 1, At: verificationAt},
	}, first, firstBlocks)
	specNext := writeStoredVersion(t, tx, spec, 2, "Words, rewritten.")
	verificationNext := writeStoredVersion(t, tx, verification, 2, "Check.")
	// After the version rows, whose insert sets the row's version to null (migration 0122's
	// trigger): the row then stands at version 3 under the mark.
	if _, err := tx.Exec(ctx, `
		update document_block_versions set version = 3, store_owed_at = now(), store_owed_rule = $2 where artifact_id = $1::uuid
	`, verification, BlockIndexMarkingRule); err != nil {
		t.Fatalf("stand the Verification document's row at version 3 under a mark: %v", err)
	}
	second, secondBlocks := documentOf([]part{{"y", "paragraph", "Check."}, {"x", "paragraph", "Words, rewritten."}})
	secondBlocks[0].Document, secondBlocks[1].Document = verification, spec
	index, err := ReadBlockIndex(ctx, tx, tree)
	if err != nil {
		t.Fatalf("read the room's block index: %v", err)
	}
	record, err := ReadIndexedVersion(ctx, tx, tree, spec, 2)
	if err != nil {
		t.Fatalf("read the record of the room's pass: %v", err)
	}
	indexing, err := PlanBlocks(ctx, tx, index, BlockIndexMarkingRule, record.At, VersionBlocks{
		ArtifactID: tree, Record: record, Markdown: second, Blocks: secondBlocks,
		Versions: Versions{tree: {Number: 1, At: treeAt}, spec: {Number: 2, At: specNext}, verification: {Number: 2, At: verificationNext}},
	})
	if err != nil || !indexing.Won() {
		t.Fatalf("plan the room's pass: won %t, %v", indexing.Won(), err)
	}
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		indexing.Queue(b)
		return nil
	}); err != nil {
		t.Fatalf("write the room's pass: %v", err)
	}

	stood, err := readBlockStands(ctx, tx, []string{verification})
	if err != nil {
		t.Fatalf("read the Verification document's row: %v", err)
	}
	number := func(n *int) string {
		if n == nil {
			return "null"
		}
		return strconv.Itoa(*n)
	}
	described := func(s blockStand) string {
		return fmt.Sprintf("held %t, version %s, store_version %s, owed %t (rule %d), latest indexed %s",
			s.held, number(s.version), number(s.storeVersion), s.owed, s.owedRule, number(s.latestIndexed))
	}
	if stand := stood[verification]; !stand.owed || number(stand.version) != "3" {
		t.Fatalf("the Verification document's row reads %s after the pass, want it at version 3 under its mark, as the room's statement leaves it, or this test proves nothing", described(stand))
	}
	if held, want := described(indexing.Held().stands[verification]), described(stood[verification]); held != want {
		t.Errorf("the index the pass leaves holds the Verification document's row as %s, want %s, as the database holds it", held, want)
	}
}
