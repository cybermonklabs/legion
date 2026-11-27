package docs

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The tests of c-parent-written that hold the block index's pass to writing each block's parent in
// its insert and appending no tuple twice (R1 and R15 of this issue's spec): a fresh pass updates no
// document_blocks row and a pass over moved blocks updates exactly the rows it moved
// (TestAFreshPassWritesEachParentInItsInsert), the same pass run twice and its batch sent again
// append nothing twice (TestTheBlockIndexPassRunTwiceWritesNoDuplicate), and the two removals of
// the pass's waste - the update of the rows its own insert wrote, and the conflict arm of a tuple of
// a node its own insert made - leave every row and every tuple as the pass before them left them
// (TestTheWasteRemovalsLeaveTheSameRows). Each runs the reconciliation's pass in a transaction of
// the test's own (parentWrittenPass), so it reads what the pass did before the pass commits.

// parentWrittenMoved is the version TestTheWasteRemovalsLeaveTheSameRows moves treeVersion1's
// blocks to: resident moves under left and mover under right, each with no character of it
// changed, so only the parents' projection reaches them, and stays moves under right with its words
// changed, so the pass writes its row and its parent in one pass; both callouts' texts change with
// their children.
const parentWrittenMoved = "::::callout{#left kind=\"note\" title=\"\"}\n## The first heading\n\n" +
	":::callout{#resident kind=\"note\" title=\"\"}\nThe words already here.\n:::\n\n::::\n\n" +
	"::::callout{#right kind=\"note\" title=\"\"}\n## The second heading\n\n" +
	":::callout{#mover kind=\"note\" title=\"\"}\nThe words that move.\n:::\n\n" +
	":::callout{#stays kind=\"note\" title=\"\"}\nThe words that stay cite dispatch://CORE-2, and now say more.\n:::\n\n::::\n"

// parentWrittenDocument creates a document of CORE-1 under slug whose version 1 holds markdown, as
// a build without the block index writes it (createOwedDocument), so it owes its block index and
// the next pass over it is a fresh one, and records that version's artifact.version event, the
// cause of every tuple a pass over it states. A tuple at the sentinel has no repeat key
// (graph_tuples_once, graph_tuples_value_once), so only a caused one shows a second append dropping
// it.
func parentWrittenDocument(t *testing.T, database *store.Store, service *Service, slug, markdown string) string {
	t.Helper()
	ctx := context.Background()
	document := createOwedDocument(t, database, service, "CORE-1", "", slug, markdown, "")
	settle(t, database, document)
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the event of version 1 of %s: %v", slug, err)
	}
	defer tx.Rollback(ctx)
	parentWrittenVersionEvent(t, tx, document, 1)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the event of version 1 of %s: %v", slug, err)
	}
	return document
}

// parentWrittenVersionEvent records, in tx, the artifact.version event of version number of the
// document, which the reconciliation's pass reads as its cause (versionEventID).
func parentWrittenVersionEvent(t *testing.T, tx pgx.Tx, artifactID string, number int) {
	t.Helper()
	if _, err := documentEvent(context.Background(), tx, "artifact.version", versionEventPayload(t, artifactID, "", number)); err != nil {
		t.Fatalf("record the event of version %d of %s: %v", number, artifactID, err)
	}
}

// parentWrittenVersion writes the document's next version, holding markdown as the renderer writes
// it, and that version's event, by SQL, as a build without the block index writes a version: the
// insert of its row owes the document its block index (migration 0122's trigger), and the
// reconciliation's pass is what indexes it. It answers the version's number.
func parentWrittenVersion(t *testing.T, database *store.Store, artifactID, markdown string) int {
	t.Helper()
	ctx := context.Background()
	rendered, _ := renderedVersion(t, markdown)
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the next version of %s: %v", artifactID, err)
	}
	defer tx.Rollback(ctx)
	var number int
	if err := tx.QueryRow(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		select $1, max(number) + 1, $2, '[{"kind":"user","id":"alice"}]' from artifact_versions where artifact_id = $1
		returning number
	`, artifactID, rendered).Scan(&number); err != nil {
		t.Fatalf("write the next version of %s: %v", artifactID, err)
	}
	parentWrittenVersionEvent(t, tx, artifactID, number)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit version %d of %s: %v", number, artifactID, err)
	}
	return number
}

// parentWrittenPass runs, in tx, the block-index reconciliation's pass over the document's newest
// version, as indexOwedDocument runs it: under the document owner's lock (lockArtifactOwner), with
// the event that recorded the version as its cause (versionEventID), the index as a tick runs it
// (indexRenderedVersion), and what it stated appended under that event, with the head rows of its
// nodes and the stamp of the version's links, all on the pass's write queue (queueWrites,
// runQueue). It does not commit. It answers the step that appended what the pass stated, the batch
// a publisher's retry sends again, and the blocks the pass indexed: a block whose markdown names no
// id - a heading, a paragraph - takes the id the render mints for it (pmdoc.EnsureBlockIDs), and
// only a typed block's markdown names its id, so a second render of the same version mints those
// blocks other ids, and what the pass placed is read off these. run names the pass in a failure.
func parentWrittenPass(t *testing.T, service *Service, tx pgx.Tx, artifactID, run string) (WriteStep, []model.ArtifactBlock) {
	t.Helper()
	ctx := context.Background()
	queued, ledger := queueWrites(ctx, tx)
	owner, _, err := lockArtifactOwner(queued, tx, artifactID)
	if err != nil {
		t.Fatalf("%s: take the owner lock of %s: %v", run, artifactID, err)
	}
	var version int
	var stored string
	var versionAt time.Time
	if err := tx.QueryRow(queued, `
		select number, markdown, created_at from artifact_versions
		where artifact_id = $1 and markdown is not null
		order by number desc limit 1
	`, artifactID).Scan(&version, &stored, &versionAt); err != nil {
		t.Fatalf("%s: read the newest version of %s: %v", run, artifactID, err)
	}
	event, err := versionEventID(queued, tx, owner, artifactID, version)
	if err != nil {
		t.Fatalf("%s: read the event of version %d of %s: %v", run, version, artifactID, err)
	}
	if event == graphstore.NoEvent {
		t.Fatalf("%s: no event recorded version %d of %s, so what the pass states carries no cause a repeat key holds", run, version, artifactID)
	}
	markdown, blocks := renderedVersion(t, stored)
	_, stated, err := service.indexRenderedVersion(queued, tx, artifactID, refs.OneVersion(artifactID, version, versionAt), nil, true, markdown, blocks, nil, nil)
	if err != nil {
		t.Fatalf("%s over version %d of %s failed: %v", run, version, artifactID, err)
	}
	cause := WriteStep(func(b *store.WriteBatch) error {
		graphstore.QueueCaused(b, stated, event)
		graphstore.QueueHeads(b, graphstore.HeldHeads(stated))
		stampVersionEvent(b, artifactID, artifactID, version, versionAt, event)
		return nil
	})
	if err := write(queued, tx, cause); err != nil {
		t.Fatalf("%s over version %d of %s failed: %v", run, version, artifactID, err)
	}
	if err := ledger.runQueue(ctx, nil); err != nil {
		t.Fatalf("%s over version %d of %s failed: %v", run, version, artifactID, err)
	}
	return cause, blocks
}

// parentWrittenUpdates is how many document_blocks rows tx's transaction has updated so far
// (pg_stat_xact_user_tables' n_tup_upd, which counts the update an insert's conflict arm makes as
// an update), read in that transaction, which alone sees its own counts before it commits.
func parentWrittenUpdates(t *testing.T, tx pgx.Tx) int64 {
	t.Helper()
	var updated int64
	if err := tx.QueryRow(context.Background(), `
		select n_tup_upd from pg_stat_xact_user_tables where relid = 'document_blocks'::regclass
	`).Scan(&updated); err != nil {
		t.Fatalf("read how many document_blocks rows the transaction updated: %v", err)
	}
	return updated
}

// parentWrittenRow is what a document_blocks row projects of its block's node: its node's kind and
// address, and the id of the block holding it, each "" where the row holds null.
type parentWrittenRow struct{ kind, address, parent string }

// parentWrittenProjection is the projection of every document_blocks row of the document, by block
// id, read through q.
func parentWrittenProjection(t *testing.T, q Queryer, artifactID string) map[string]parentWrittenRow {
	t.Helper()
	rows, err := q.Query(context.Background(), `
		select block_id, coalesce(node_kind, ''), coalesce(node_address, ''), coalesce(parent_block_id, '')
		from document_blocks where artifact_id = $1
	`, artifactID)
	if err != nil {
		t.Fatalf("read the rows of %s: %v", artifactID, err)
	}
	defer rows.Close()
	projection := map[string]parentWrittenRow{}
	for rows.Next() {
		var id string
		var row parentWrittenRow
		if err := rows.Scan(&id, &row.kind, &row.address, &row.parent); err != nil {
			t.Fatalf("read a row of %s: %v", artifactID, err)
		}
		projection[id] = row
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the rows of %s: %v", artifactID, err)
	}
	return projection
}

// parentWrittenWanted is the projection blocks, the blocks a pass indexed of the document's newest
// version (parentWrittenPass), give each block they place (refs.PlaceBlocks): a node of the block
// kind at the block's address, under the block holding it, or under none at the top level. The
// document's ref_key is read through q.
func parentWrittenWanted(t *testing.T, q Queryer, artifactID string, blocks []model.ArtifactBlock) map[string]parentWrittenRow {
	t.Helper()
	refKey := parentWrittenRefKey(t, q, artifactID)
	nodes, placed, parents := refs.PlaceBlocks(refKey, blocks)
	wanted := make(map[string]parentWrittenRow, len(placed))
	for index, block := range placed {
		row := parentWrittenRow{kind: refs.BlockNodeKind, address: refKey + "#" + nodes[block].ID}
		if parents[index] >= 0 {
			row.parent = nodes[parents[index]].ID
		}
		wanted[nodes[block].ID] = row
	}
	return wanted
}

// parentWrittenRefKey is the document's ref_key, read through q.
func parentWrittenRefKey(t *testing.T, q Queryer, artifactID string) string {
	t.Helper()
	var refKey string
	if err := q.QueryRow(context.Background(), `select ref_key from artifacts where id = $1`, artifactID).Scan(&refKey); err != nil {
		t.Fatalf("read the ref_key of %s: %v", artifactID, err)
	}
	return refKey
}

// parentWrittenWholeRows is every column of every document_blocks row of the document, by block id,
// read through q: a reading taken again says which rows a pass changed.
func parentWrittenWholeRows(t *testing.T, q Queryer, artifactID string) map[string]string {
	t.Helper()
	rows, err := q.Query(context.Background(), `
		select d.block_id, row_to_json(d)::text from document_blocks d where d.artifact_id = $1
	`, artifactID)
	if err != nil {
		t.Fatalf("read the rows of %s: %v", artifactID, err)
	}
	defer rows.Close()
	whole := map[string]string{}
	for rows.Next() {
		var id, row string
		if err := rows.Scan(&id, &row); err != nil {
			t.Fatalf("read a row of %s: %v", artifactID, err)
		}
		whole[id] = row
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the rows of %s: %v", artifactID, err)
	}
	return whole
}

// parentWrittenRowDump is the node_kind, node_address and parent_block_id of every document_blocks
// row of the document, one line a row in block id order, read through q.
func parentWrittenRowDump(t *testing.T, q Queryer, artifactID string) []string {
	t.Helper()
	return parentWrittenLines(t, q, "the rows of "+artifactID, `
		select row(block_id, node_kind, node_address, parent_block_id)::text
		from document_blocks where artifact_id = $1 order by block_id
	`, artifactID)
}

// parentWrittenTuples is every tuple appended after seq, every column but its sequence, one line a
// tuple in the order it was appended, read through q.
func parentWrittenTuples(t *testing.T, q Queryer, seq int64) []string {
	t.Helper()
	return parentWrittenLines(t, q, "the tuples appended after the pass began", `
		select row(from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)::text
		from graph_tuples where seq > $1 order by seq
	`, seq)
}

// parentWrittenLines is the one text column statement answers, a line a row, read through q.
func parentWrittenLines(t *testing.T, q Queryer, what, statement string, args ...any) []string {
	t.Helper()
	rows, err := q.Query(context.Background(), statement, args...)
	if err != nil {
		t.Fatalf("read %s: %v", what, err)
	}
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read %s: %v", what, err)
	}
	return lines
}

// A fresh pass writes each block's parent in its insert, and a pass over moved blocks updates
// exactly the rows it moved (c-parent-written; R1 and R15 of this issue's spec).
//
// A document of nested callouts, owed its block index, takes its first pass in the test's
// transaction, which reads document_blocks' n_tup_upd (pg_stat_xact_user_tables) before and after
// it: the pass inserts a row for each block, carrying its node's kind and address and the id of the
// block holding it, as the version's tree gives them, and updates none. Then a version that moves
// block mover out of left under right, and changes no character of it, takes the reconciliation's
// pass: it updates exactly the rows it moved, the one row whose parent moved and the two callouts'
// rows whose texts changed with their children, each once.
//
// It fails, naming the count, where the pass projects every block's parent in an update after the
// insert that wrote its row with none (queueBlockParents over every placed row).
func TestAFreshPassWritesEachParentInItsInsert(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, service := blockIndexService(t)
	document := parentWrittenDocument(t, database, service, "fresh", treeVersion1)

	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the fresh pass: %v", err)
	}
	defer tx.Rollback(ctx)
	before := parentWrittenUpdates(t, tx)
	_, blocks := parentWrittenPass(t, service, tx, document, "the fresh pass")
	updated := parentWrittenUpdates(t, tx) - before
	held, wanted := parentWrittenProjection(t, tx, document), parentWrittenWanted(t, tx, document, blocks)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the fresh pass: %v", err)
	}
	if len(wanted) < 4 || len(held) != len(wanted) {
		t.Fatalf("the fresh pass left %d rows of a version of %d placed blocks, want a row for each block of a document of nested blocks",
			len(held), len(wanted))
	}
	for id, want := range wanted {
		if got := held[id]; got != want {
			t.Errorf("after the fresh pass block %s's row holds node_kind %q, node_address %q and parent_block_id %q, want %q, %q and %q",
				id, got.kind, got.address, got.parent, want.kind, want.address, want.parent)
		}
	}
	if updated != 0 {
		t.Errorf("the fresh pass over %d blocks updated %d document_blocks rows, want none: each row it inserts carries its parent and its node columns, and no statement after the insert updates it",
			len(wanted), updated)
	}

	// The pass over a version that moves mover under right.
	parentWrittenVersion(t, database, document, treeVersion2)
	projected, whole := parentWrittenProjection(t, database.Pool, document), parentWrittenWholeRows(t, database.Pool, document)
	moving, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the pass over the moved block: %v", err)
	}
	defer moving.Rollback(ctx)
	before = parentWrittenUpdates(t, moving)
	parentWrittenPass(t, service, moving, document, "the pass over the moved block")
	updated = parentWrittenUpdates(t, moving) - before
	if err := moving.Commit(ctx); err != nil {
		t.Fatalf("commit the pass over the moved block: %v", err)
	}
	projectedNow, wholeNow := parentWrittenProjection(t, database.Pool, document), parentWrittenWholeRows(t, database.Pool, document)
	var moved, changed []string
	for id, row := range wholeNow {
		if was, stood := whole[id]; stood && was != row {
			changed = append(changed, id)
		}
		if was, stood := projected[id]; stood && was.parent != projectedNow[id].parent {
			moved = append(moved, id)
		}
	}
	slices.Sort(moved)
	slices.Sort(changed)
	if !slices.Equal(moved, []string{"mover"}) || projectedNow["mover"].parent != "right" {
		t.Fatalf("the pass over version 2 moved %v, leaving mover under %q, want mover alone moved from left to right: the case proves nothing",
			moved, projectedNow["mover"].parent)
	}
	if updated != int64(len(changed)) {
		t.Errorf("the pass over the version that moved %s under new parents updated %d document_blocks rows, want exactly the %d rows it moved: %s",
			strings.Join(moved, ", "), updated, len(changed), strings.Join(changed, ", "))
	}
}

// The block index's pass run twice over one document, and one batch of it sent a second time
// inside a savepoint as a publisher's retry sends it, appends no tuple twice and raises no error
// (c-parent-written; R15 of this issue's spec).
//
// A document of nested callouts, owed its block index, with the event of its version recorded,
// takes the reconciliation's first pass, which appends its blocks' nodes' facts under that event.
// In the same transaction, inside a savepoint, the batch that appended them (QueueCaused, the head
// rows and the stamp) is sent again, and appends nothing. The document is then owed its block
// index again at the version it stands at, as a rebuild owes it (oweDocumentIndex), and a second
// pass runs over it, wins the fence and clears the mark. No tuple the two passes and the batch
// sent again appended stands twice.
//
// It fails, printing the unique violation of graph_tuples_once or graph_tuples_value_once, where
// the append leaves its conflict arm off for a tuple of a node that stood before the batch, rather
// than only for a tuple of a node the pass's own node insert made.
func TestTheBlockIndexPassRunTwiceWritesNoDuplicate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, service := blockIndexService(t)
	document := parentWrittenDocument(t, database, service, "twice", treeVersion1)
	before := highestTuple(t, database)

	first, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the first run of the pass: %v", err)
	}
	defer first.Rollback(ctx)
	cause, _ := parentWrittenPass(t, service, first, document, "the first run of the pass")
	appended := parentWrittenTuples(t, first, before)
	if len(appended) == 0 {
		t.Fatalf("the first run of the pass appended no tuple, so neither a second run nor a retry of its batch proves anything")
	}
	if _, err := first.Exec(ctx, `savepoint publisher_retry`); err != nil {
		t.Fatalf("open the retry's savepoint: %v", err)
	}
	if err := store.RunWrite(ctx, first, cause); err != nil {
		t.Fatalf("the first run's batch, sent a second time inside a savepoint as a publisher's retry sends it, failed: %v", err)
	}
	if _, err := first.Exec(ctx, `release savepoint publisher_retry`); err != nil {
		t.Fatalf("release the retry's savepoint: %v", err)
	}
	if again := parentWrittenTuples(t, first, before); len(again) > len(appended) {
		t.Errorf("the first run's batch, sent a second time, appended %d tuples, want none: %s",
			len(again)-len(appended), strings.Join(again[len(appended):], "; "))
	}
	if err := first.Commit(ctx); err != nil {
		t.Fatalf("commit the first run of the pass: %v", err)
	}

	// The second run, over the document owed its index again at the version it stands at.
	if err := oweDocumentIndex(ctx, database.Pool, document, refs.BlockIndexMarkingRule); err != nil {
		t.Fatalf("owe the document its block index again: %v", err)
	}
	second, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the second run of the pass: %v", err)
	}
	defer second.Rollback(ctx)
	parentWrittenPass(t, service, second, document, "the second run of the pass")
	if err := second.Commit(ctx); err != nil {
		t.Fatalf("the second run of the pass failed at its commit: %v", err)
	}
	if markedAt, _ := backfillMark(t, database, document); markedAt != nil || blockIndexAt(t, database, document) == nil {
		t.Fatalf("the second run left the document owed (marked at %v, its blocks at %s), so it never ran",
			markedAt, standsText(blockIndexAt(t, database, document)))
	}

	for _, duplicate := range parentWrittenLines(t, database.Pool, "the tuples appended twice", `
		select row(from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)::text
		       || ' appended ' || count(*)::text || ' times'
		from graph_tuples where seq > $1
		group by from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted
		having count(*) > 1
		order by 1
	`, before) {
		t.Errorf("the two runs of the pass and its batch sent again appended a tuple more than once: %s", duplicate)
	}
}

// parentWrittenAsBefore runs, in tx, over what the pass left, the two statements the waste
// removals took out of the pass, which the pass before them ran: the projection of every block the
// pass placed of blocks, the blocks it indexed (parentWrittenPass), of its parent and node columns
// after the insert of its row, which held no parent (queueBlockParents as it stood before the
// parent went into the insert), and the append of every tuple of what the pass stated with its
// conflict arm (graphstore's append before the arm came off for a tuple of a node the pass's own
// insert made; once the pass has run, no node is new, so every tuple of a second append carries
// the arm). The pass before the removals ran exactly the pass's statements and these two, so the
// rows and the tuples they leave are what that pass leaves on the same input: a parent or a node
// column the insert wrote otherwise, a row the narrowed projection passed over, or a tuple an
// append without its arm left out, reads as a difference.
func parentWrittenAsBefore(t *testing.T, tx pgx.Tx, artifactID string, cause WriteStep, blocks []model.ArtifactBlock, what string) {
	t.Helper()
	ctx := context.Background()
	refKey := parentWrittenRefKey(t, tx, artifactID)
	nodes, placed, parents := refs.PlaceBlocks(refKey, blocks)
	ids, held := make([]string, len(placed)), make([]*string, len(placed))
	for index, block := range placed {
		ids[index] = nodes[block].ID
		if parents[index] >= 0 {
			held[index] = &nodes[parents[index]].ID
		}
	}
	if _, err := tx.Exec(ctx, `
		update document_blocks d
		set node_kind = $3, node_address = $4 || '#' || p.block_id, parent_block_id = p.parent
		from unnest($2::text[], $5::text[]) as p (block_id, parent)
		where d.artifact_id = $1::uuid and d.block_id = p.block_id
		  and (d.parent_block_id is distinct from p.parent
		       or d.node_kind is distinct from $3
		       or d.node_address is distinct from $4 || '#' || p.block_id)
	`, artifactID, ids, refs.BlockNodeKind, refKey, held); err != nil {
		t.Fatalf("after %s, project every placed block's parent as the pass before the waste removals did: %v", what, err)
	}
	if err := store.RunWrite(ctx, tx, cause); err != nil {
		t.Fatalf("after %s, append what the pass stated with the conflict arm on every tuple, as the pass before the waste removals did: %v", what, err)
	}
}

// parentWrittenCompare holds got, a dump of what the pass left, to want, the dump of what the pass
// before the waste removals leaves on the same input, row by row, and names each row where the two
// part.
func parentWrittenCompare(t *testing.T, what, kind string, got, want []string) {
	t.Helper()
	for index := range max(len(got), len(want)) {
		left, right := "no row", "no row"
		if index < len(got) {
			left = got[index]
		}
		if index < len(want) {
			right = want[index]
		}
		if left != right {
			t.Errorf("after %s, %s %d of the dump is %s, and the pass before the waste removals leaves %s", what, kind, index+1, left, right)
		}
	}
}

// The two removals of the pass's waste leave the same rows (c-parent-written; R15 of this issue's
// spec): the pass that writes each block's parent in its insert, and leaves the conflict arm off
// only for a tuple of a node its own insert made, leaves every document_blocks row's node_kind,
// node_address and parent_block_id, and every tuple, as the pass before those removals leaves them
// on the same input.
//
// A document of nested callouts takes a fresh pass, then a pass over a version that moves three
// blocks under new parents, one of them with its words changed (parentWrittenMoved). After each,
// in the pass's own transaction, the test dumps the rows and the tuples the pass left, then, inside
// a savepoint, runs over them the two statements the removals took out of the pass
// (parentWrittenAsBefore), dumps both again, and rolls the savepoint back. The two dumps of each
// are compared row by row.
func TestTheWasteRemovalsLeaveTheSameRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, service := blockIndexService(t)
	document := parentWrittenDocument(t, database, service, "waste", treeVersion1)
	for _, pass := range []struct{ what, moves string }{
		{what: "a fresh pass"},
		{what: "a pass over moved blocks", moves: parentWrittenMoved},
	} {
		if pass.moves != "" {
			parentWrittenVersion(t, database, document, pass.moves)
		}
		func() {
			before := highestTuple(t, database)
			tx, err := database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin %s: %v", pass.what, err)
			}
			defer tx.Rollback(ctx)
			cause, blocks := parentWrittenPass(t, service, tx, document, pass.what)
			rows, tuples := parentWrittenRowDump(t, tx, document), parentWrittenTuples(t, tx, before)
			if len(rows) < 4 || len(tuples) == 0 {
				t.Fatalf("%s left %d rows and appended %d tuples, want a row for each block of a document of nested blocks and the tuples that state them",
					pass.what, len(rows), len(tuples))
			}
			if _, err := tx.Exec(ctx, `savepoint the_pass_before`); err != nil {
				t.Fatalf("open the savepoint of the pass before the waste removals: %v", err)
			}
			parentWrittenAsBefore(t, tx, document, cause, blocks, pass.what)
			rowsBefore, tuplesBefore := parentWrittenRowDump(t, tx, document), parentWrittenTuples(t, tx, before)
			if _, err := tx.Exec(ctx, `rollback to savepoint the_pass_before`); err != nil {
				t.Fatalf("roll back the savepoint of the pass before the waste removals: %v", err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatalf("commit %s: %v", pass.what, err)
			}
			parentWrittenCompare(t, pass.what, "document_blocks row", rows, rowsBefore)
			parentWrittenCompare(t, pass.what, "tuple", tuples, tuplesBefore)
		}()
	}
}
