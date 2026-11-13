package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// fenceWait is how long a step of this test waits for another backend to queue behind a row it
// holds, and for a pass it started on a connection of its own to answer.
const fenceWait = 30 * time.Second

// highestTuple is the last sequence the tuple log holds: the mark a step's own appends are read
// back after, and the one that says a step appended nothing.
func highestTuple(t *testing.T, database *store.Store) int64 {
	t.Helper()
	var highest int64
	if err := database.Pool.QueryRow(context.Background(),
		`select coalesce(max(seq), 0) from graph_tuples`).Scan(&highest); err != nil {
		t.Fatalf("read the end of the tuple log: %v", err)
	}
	return highest
}

// tuplesSince counts what the log gained after seq out of the blocks of one document, whose
// addresses all start with the document's ref_key: a tick writes more than one document, so a
// count of the whole log would answer for passes this step is not watching.
func tuplesSince(t *testing.T, database *store.Store, artifactID string, seq int64) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from graph_tuples t
		join artifacts a on a.id = $1::uuid
		where t.seq > $2 and t.from_kind = $3 and t.from_id like a.ref_key || '#%'
	`, artifactID, seq, refs.BlockNodeKind).Scan(&count); err != nil {
		t.Fatalf("count the tuples a step appended for %s: %v", artifactID, err)
	}
	return count
}

// parentsNow is the far end the store asserts under part_of for each block of the document, by
// block id, read as a current relation is read (graphstore.CurrentOut).
func parentsNow(t *testing.T, database *store.Store, artifactID string) map[string]string {
	t.Helper()
	ctx := context.Background()
	rows, err := database.Pool.Query(ctx,
		`select block_id, node_id from document_blocks where artifact_id = $1 order by block_id`, artifactID)
	if err != nil {
		t.Fatalf("read the blocks of %s: %v", artifactID, err)
	}
	defer rows.Close()
	addresses := map[string]string{}
	for rows.Next() {
		var id, address string
		if err := rows.Scan(&id, &address); err != nil {
			t.Fatalf("scan a block of %s: %v", artifactID, err)
		}
		addresses[id] = address
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the blocks of %s: %v", artifactID, err)
	}
	parents := map[string]string{}
	for id, address := range addresses {
		held, err := graphstore.CurrentOut(ctx, database.Pool, refs.BlockNodeKind, address, []string{graphstore.TuplePartOf})
		if err != nil {
			t.Fatalf("read the parent of %s: %v", id, err)
		}
		if len(held) != 1 {
			t.Fatalf("block %s holds %d current part_of tuples, want exactly one", id, len(held))
		}
		parents[id] = held[0].ToKind + "/" + held[0].ToID
	}
	return parents
}

// sameParents compares two readings of a document's parents.
func sameParents(got, want map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	for block, parent := range want {
		if got[block] != parent {
			return false
		}
	}
	return true
}

// flattenedPass is the blocks an indexing pass of the document's stored version would hand the
// index if every block of it sat at the document's top level: one byte of the markdown each, so
// no block's range holds another's. It is what the stale pass below would assert, and what the
// fence must refuse.
func flattenedPass(t *testing.T, database *store.Store, artifactID string) (string, []model.ArtifactBlock) {
	t.Helper()
	ctx := context.Background()
	var markdown string
	if err := database.Pool.QueryRow(ctx, `
		select markdown from artifact_versions where artifact_id = $1 and markdown is not null
		order by number desc limit 1
	`, artifactID).Scan(&markdown); err != nil {
		t.Fatalf("read the stored version of %s: %v", artifactID, err)
	}
	rows, err := database.Pool.Query(ctx,
		`select block_id from document_blocks where artifact_id = $1 order by block_id`, artifactID)
	if err != nil {
		t.Fatalf("read the blocks of %s: %v", artifactID, err)
	}
	defer rows.Close()
	var blocks []model.ArtifactBlock
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan a block of %s: %v", artifactID, err)
		}
		at := len(blocks)
		blocks = append(blocks, model.ArtifactBlock{ID: id, Type: "paragraph", From: at, To: at + 1})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the blocks of %s: %v", artifactID, err)
	}
	if len(blocks) < 2 || len(markdown) < len(blocks) {
		t.Fatalf("the fixture document holds %d blocks in %d bytes, want a document of several blocks", len(blocks), len(markdown))
	}
	return markdown, blocks
}

// writeVersionThroughTheRealPath writes the document's next version the way a request writes
// one: NamedVersion in a joined transaction, the version's own event after it, and what the
// block index stated appended under that event, as api's commitArtifactVersionEvent does. It
// answers the version and that event, the cause every tuple the version's index stated carries.
func writeVersionThroughTheRealPath(t *testing.T, service *Service, artifactID, summary string) (VersionResult, int64) {
	t.Helper()
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the version write: %v", err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	result, err := service.NamedVersion(joined, artifactID, summary, model.Actor{Kind: "user", ID: "alice"})
	if err != nil {
		t.Fatalf("write a version through the version write path: %v", err)
	}
	if !result.Wrote {
		t.Fatalf("the version write recorded nothing; the live document must differ from the latest version")
	}
	payload, err := json.Marshal(map[string]any{
		"artifact_id": artifactID,
		"version":     map[string]any{"number": result.Version.Number},
	})
	if err != nil {
		t.Fatalf("encode the version event's payload: %v", err)
	}
	event := appendDocumentEvent(t, ctx, tx, "artifact.version", payload)
	if err := result.Blocks.Caused(ctx, tx, event); err != nil {
		t.Fatalf("append what the version's block index stated: %v", err)
	}
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit the version write: %v", err)
	}
	return result, event
}

// appendDocumentEvent records one event of issue CORE-1, where the fixture's documents live, in
// the write's own transaction (documentEvent), and fails the test where it cannot.
func appendDocumentEvent(t *testing.T, ctx context.Context, tx pgx.Tx, kind string, payload []byte) int64 {
	t.Helper()
	event, err := documentEvent(ctx, tx, kind, payload)
	if err != nil {
		t.Fatal(err)
	}
	return event
}

// documentEvent records one event of issue CORE-1 in the write's own transaction. The sequence
// comes off the issue's own counter, as events.Append takes it, so the settlement's next event
// does not collide with this one. It answers an error rather than failing the test, so a write
// run off the test's goroutine can record its event too.
func documentEvent(ctx context.Context, tx pgx.Tx, kind string, payload []byte) (int64, error) {
	var event int64
	if err := tx.QueryRow(ctx, `
		with bumped as (update issues set last_seq = last_seq + 1 where key = 'CORE-1' returning last_seq)
		insert into events (issue_key, seq, type, actor, payload, notify)
		select 'CORE-1', bumped.last_seq, $1, '{"kind":"user","id":"alice"}', $2, false
		from bumped
		returning id
	`, kind, payload).Scan(&event); err != nil {
		return 0, fmt.Errorf("record the %s event: %w", kind, err)
	}
	return event, nil
}

// The fixture document's two versions. Version 1 holds two callouts side by side; version 2 moves
// the second, mover, under the first, holder, so mover's parent is the document at version 1 and
// holder at version 2: the parent a stale pass of version 1 would assert again, and the one the
// fence must keep it from asserting.
const (
	raceVersion1 = ":::callout{#holder kind=\"note\" title=\"\"}\nThe block a newer version moves another under.\n:::\n\n" +
		":::callout{#mover kind=\"note\" title=\"\"}\nThe block the newer version moves.\n:::\n"
	raceVersion2 = "::::callout{#holder kind=\"note\" title=\"\"}\nThe block a newer version moves another under.\n\n" +
		":::callout{#mover kind=\"note\" title=\"\"}\nThe block the newer version moves.\n:::\n\n::::\n"
)

// standsText is a document's stored revision as a failure prints it: the version its blocks
// stand at, or owed where the row holds none.
func standsText(stands *int) string {
	if stands == nil {
		return "owed (null)"
	}
	return fmt.Sprintf("version %d", *stands)
}

// blockAddress is the node address of one block of the document, read off its row, which fails
// the test where the document holds no block of that id.
func blockAddress(t *testing.T, database *store.Store, artifactID, blockID string) string {
	t.Helper()
	var address string
	if err := database.Pool.QueryRow(context.Background(),
		`select node_id from document_blocks where artifact_id = $1 and block_id = $2`, artifactID, blockID).Scan(&address); err != nil {
		t.Fatalf("read the address of block %s of %s: %v", blockID, artifactID, err)
	}
	return address
}

// currentPartOf is the one part_of tuple the store asserts for a block now, read as a current
// relation is read (graphstore.CurrentOut), with its sequence and its cause.
func currentPartOf(t *testing.T, database *store.Store, address string) graphstore.Tuple {
	t.Helper()
	held, err := graphstore.CurrentOut(context.Background(), database.Pool, refs.BlockNodeKind, address, []string{graphstore.TuplePartOf})
	if err != nil {
		t.Fatalf("read the parent of %s: %v", address, err)
	}
	if len(held) != 1 {
		t.Fatalf("block %s holds %d current part_of tuples, want exactly one: %+v", address, len(held), held)
	}
	return held[0]
}

// A write that read an older version of a document cannot state a parent a newer version has
// replaced. The race is built rather than waited for: a pass reads the document at the version
// it stands at and is held open before its commit, a newer version that moves one block under
// another parent is written through the real version write path, and the older pass then runs
// on to its commit. The fence refuses it - document_block_versions still holds the newer
// revision, the moved block's current part_of tuple is the one the newer version appended,
// under that version's own event, and the older pass appended no tuple at all - and the same
// pass at a revision above the stored one does state its parents, so it is the fence and not
// the pass's own work that refused.
//
// The fence is one conditional write under one pair of locks. Each of the three writers R23
// holds to it - the version write through reference_excerpts.go, the reconciliation tick and the
// backfill command - is driven through the same race: the document_block_versions row is held
// in a second transaction, the writer is read waiting at that row rather than at a later step,
// and a newer revision is stored while it waits; the writer then commits no tuple below it. The
// source tree is read last: for any statement that writes a revision into
// document_block_versions outside the fence, and for a revision test in the erase or in
// refs.IndexOwedItemBlocks, which R23 reads as writers outside the fence.
func TestAStaleWriteCannotAssertAParentANewerEditReplaced(t *testing.T) {
	ctx := context.Background()
	database, service := blockIndexService(t)
	document := createOwedDocument(t, database, service, "", "race", raceVersion1, "")
	settle(t, database, document)
	if tick := reconcileBlockIndex(t, service, ""); tick.Documents != 1 {
		t.Fatalf("the first tick indexed %d documents, want the fixture's one", tick.Documents)
	}
	if stands := blockIndexAt(t, database, document); stands == nil || *stands != 1 {
		t.Fatalf("the document's blocks stand at %s, want version 1", standsText(stands))
	}
	mover, holder := blockAddress(t, database, document, "mover"), blockAddress(t, database, document, "holder")
	if was := currentPartOf(t, database, mover); was.ToKind != "artifact" {
		t.Fatalf("at version 1 block %s is part of %s/%s, want the document: the newer version must move it", mover, was.ToKind, was.ToID)
	}

	// The stale pass reads the document as it stands now and is held open before its commit.
	staleMarkdown, staleBlocks := flattenedPass(t, database, document)
	staleConn, err := database.Pool.Dedicated(ctx, "block-index-stale-pass")
	if err != nil {
		t.Fatalf("open the stale pass's connection: %v", err)
	}
	defer staleConn.Close(ctx)
	staleTx, err := staleConn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the stale pass: %v", err)
	}
	defer staleTx.Rollback(ctx)

	// The newer edit, written through the real version write path while the stale pass is open:
	// it moves mover under holder.
	if _, err := joinedReplaceText(service, document, raceVersion2, model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("edit the document the newer version records: %v", err)
	}
	newer, newerEvent := writeVersionThroughTheRealPath(t, service, document, "The newer edit")
	if newer.Version.Number != 2 {
		t.Fatalf("the newer version is %d, want 2", newer.Version.Number)
	}
	if stands := blockIndexAt(t, database, document); stands == nil || *stands != newer.Version.Number {
		t.Fatalf("after the newer version the document's blocks stand at %s, want version %d", standsText(stands), newer.Version.Number)
	}
	moved := currentPartOf(t, database, mover)
	if moved.ToKind != refs.BlockNodeKind || moved.ToID != holder || moved.SourceEvent != newerEvent {
		t.Fatalf("after version %d block %s is part of %s/%s under event %d, want block/%s under the version's event %d",
			newer.Version.Number, mover, moved.ToKind, moved.ToID, moved.SourceEvent, holder, newerEvent)
	}
	stated := parentsNow(t, database, document)

	// The stale pass runs on to its commit, and the fence refuses it.
	const staleVersion = 1
	before := highestTuple(t, database)
	_, won, write, err := refs.IndexBlocks(ctx, staleTx, document, staleVersion, time.Now(), staleMarkdown, staleBlocks, nil, nil)
	if err != nil {
		t.Fatalf("run the stale pass: %v", err)
	}
	if err := write.Caused(ctx, staleTx, graphstore.NoEvent); err != nil {
		t.Fatalf("append what the stale pass stated: %v", err)
	}
	if err := staleTx.Commit(ctx); err != nil {
		t.Fatalf("commit the stale pass: %v", err)
	}
	if won {
		t.Errorf("the stale index pass at version %d claimed the document's blocks stand at it, over the stored version %d",
			staleVersion, newer.Version.Number)
	}
	if appended := tuplesSince(t, database, document, before); appended != 0 {
		t.Errorf("the stale index pass at version %d appended %d tuples below the stored version %d, want none",
			staleVersion, appended, newer.Version.Number)
	}
	if stands := blockIndexAt(t, database, document); stands == nil || *stands != newer.Version.Number {
		t.Errorf("after the stale index pass at version %d the document's blocks stand at %s, want version %d",
			staleVersion, standsText(stands), newer.Version.Number)
	}
	if now := currentPartOf(t, database, mover); now.Seq != moved.Seq || now.SourceEvent != newerEvent {
		t.Errorf("after the stale index pass at version %d block %s is part of %s/%s (tuple %d, event %d), want the block/%s version %d appended (tuple %d, event %d)",
			staleVersion, mover, now.ToKind, now.ToID, now.Seq, now.SourceEvent, holder, newer.Version.Number, moved.Seq, newerEvent)
	}
	if now := parentsNow(t, database, document); !sameParents(now, stated) {
		t.Errorf("the stale index pass at version %d moved the parents version %d stated from %v to %v",
			staleVersion, newer.Version.Number, stated, now)
	}

	// The control: the same pass above the stored revision does state its parents, so what
	// refused it was the fence and not a pass with nothing to say.
	third := nextVersionRow(t, database, document, staleMarkdown)
	controlTx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the control pass: %v", err)
	}
	defer controlTx.Rollback(ctx)
	_, won, write, err = refs.IndexBlocks(ctx, controlTx, document, third, time.Now(), staleMarkdown, staleBlocks, nil, nil)
	if err != nil {
		t.Fatalf("run the control pass: %v", err)
	}
	if err := write.Caused(ctx, controlTx, graphstore.NoEvent); err != nil {
		t.Fatalf("append what the control pass stated: %v", err)
	}
	if err := controlTx.Commit(ctx); err != nil {
		t.Fatalf("commit the control pass: %v", err)
	}
	if !won {
		t.Fatalf("the control pass at version %d, above the stored version %d, did not stand", third, newer.Version.Number)
	}
	flattened := parentsNow(t, database, document)
	for block, parent := range flattened {
		if !strings.HasPrefix(parent, "artifact/") {
			t.Errorf("the control pass left block %s part of %s, want the document: it had work the fence refused the stale pass", block, parent)
		}
	}
	if sameParents(flattened, stated) {
		t.Errorf("the control pass changed nothing, so the stale pass proves nothing: parents %v", flattened)
	}

	requireTheVersionWriteIsFenced(t, database, service)
	requireTheTickIsFenced(t, database, service)
	requireTheBackfillIsFenced(t, database)
	requireOnlyTheFenceWritesTheOwedRow(t)
	requireNoRevisionTestOutsideTheFence(t)
}

// nextVersionRow writes one more artifact_versions row holding markdown and answers its number,
// so a pass can be run at a revision above the one the document stands at.
func nextVersionRow(t *testing.T, database *store.Store, artifactID, markdown string) int {
	t.Helper()
	var number int
	if err := database.Pool.QueryRow(context.Background(), `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		select $1, coalesce(max(number), 0) + 1, $2, '[{"kind":"user","id":"alice"}]'
		from artifact_versions where artifact_id = $1
		returning number
	`, artifactID, markdown).Scan(&number); err != nil {
		t.Fatalf("write one more version row: %v", err)
	}
	return number
}

// statementOf is the statement backend pid is running, read through the pool, whose
// pg_stat_activity is a fresh view on each read: once the backend has queued behind a lock, it is
// the statement it waits at.
func statementOf(t *testing.T, database *store.Store, pid uint32) string {
	t.Helper()
	var statement string
	if err := database.Pool.QueryRow(context.Background(),
		`select query from pg_stat_activity where pid = $1`, pid).Scan(&statement); err != nil {
		t.Fatalf("read the statement backend %d runs: %v", pid, err)
	}
	return strings.Join(strings.Fields(statement), " ")
}

// requireWaitingAtTheOwedRow fails unless writer, queued behind the holder of a document's
// document_block_versions row, waits at a statement of that row and at no projection write
// further in: the fence is the first write of every fenced writer, so a writer reaches no
// projection row and no tuple before it knows whether it won.
func requireWaitingAtTheOwedRow(t *testing.T, writer, running string) {
	t.Helper()
	t.Logf("%s waits behind the held document_block_versions row at: %s", writer, running)
	if !strings.Contains(running, "document_block_versions") {
		t.Errorf("%s waits at %q, want it waiting at the document_block_versions row", writer, running)
	}
	if strings.Contains(running, "document_blocks ") || strings.Contains(running, "block_refs") || strings.Contains(running, "graph_") {
		t.Errorf("%s waits at %q, a projection or store write: the fence must be reached before any of them", writer, running)
	}
}

// requireNothingBelowTheRevision fails if writer committed any tuple or any block row of the
// document after seq, which it may not do once a revision above its own is stored.
func requireNothingBelowTheRevision(t *testing.T, database *store.Store, writer, artifactID string, seq int64, own, stored int) {
	t.Helper()
	if appended := tuplesSince(t, database, artifactID, seq); appended != 0 {
		t.Errorf("%s committed %d tuples at version %d, below the stored revision %d, want none", writer, appended, own, stored)
	}
	if rows := countRows(t, database, "document_blocks", artifactID); rows != 0 {
		t.Errorf("%s committed %d block rows at version %d, below the stored revision %d, want none", writer, rows, own, stored)
	}
}

// requireTheVersionWriteIsFenced drives the version write through reference_excerpts.go
// (IndexDocumentReferences, which a request that writes a document's version runs in its own
// transaction) through the race. The version it indexes and a newer one are both written first,
// so the write's first touch of the document_block_versions row is the fence itself. That row is
// held in a second transaction, the write is read waiting at it, and the holder - standing for
// the pass that indexed the newer version - stores that version as the row's revision while the
// write waits. Released, the write commits, and commits no tuple and no block row: its version
// is below the stored revision.
func requireTheVersionWriteIsFenced(t *testing.T, database *store.Store, service *Service) {
	t.Helper()
	ctx := context.Background()
	const writer = "the version write (IndexDocumentReferences)"
	const own, newer = 1, 2
	const markdown = "- alpha\n- beta\n"
	var document string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ('CORE-1', 'CORE', 'fenced-version', 'fenced-version.md', 'doc', false, '{"kind":"user","id":"alice"}')
		returning id::text
	`).Scan(&document); err != nil {
		t.Fatalf("create the version write's document: %v", err)
	}
	for _, number := range []int{own, newer} {
		if _, err := database.Pool.Exec(ctx, `
			insert into artifact_versions (artifact_id, number, markdown, authors)
			values ($1, $2, $3, '[{"kind":"user","id":"alice"}]')
		`, document, number, markdown); err != nil {
			t.Fatalf("write version %d of the version write's document: %v", number, err)
		}
	}

	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the holder of the version write's owed row: %v", err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx,
		`select 1 from document_block_versions where artifact_id = $1 for no key update`, document); err != nil {
		t.Fatalf("hold the version write's owed row: %v", err)
	}

	before := highestTuple(t, database)
	writeConn, err := database.Pool.Dedicated(ctx, "block-index-fenced-version-write")
	if err != nil {
		t.Fatalf("open the version write's connection: %v", err)
	}
	defer writeConn.Close(ctx)
	pid := writeConn.PgConn().PID()
	answered := make(chan error, 1)
	go func() {
		// The send is the last thing this goroutine does, and the rollback has already run by
		// then: the caller closes writeConn as soon as it reads this channel, and a rollback
		// still in flight on that same connection races that close (pgx answers it "BUG: slow
		// write timer already active"). Deferring the rollback inside the inner function is
		// what orders the two.
		answered <- func() error {
			tx, err := writeConn.Begin(ctx)
			if err != nil {
				return err
			}
			defer tx.Rollback(ctx)
			joined, ledger := service.Join(ctx, tx)
			defer ledger.Discard()
			stored, err := service.SeedText(joined, document, markdown, model.Actor{Kind: "user", ID: "alice"})
			if err != nil {
				return fmt.Errorf("seed the document: %w", err)
			}
			_, write, err := service.IndexDocumentReferences(joined, document, own, stored)
			if err != nil {
				return fmt.Errorf("index version %d: %w", own, err)
			}
			payload, err := json.Marshal(map[string]any{"artifact": map[string]any{"id": document}})
			if err != nil {
				return err
			}
			event, err := documentEvent(ctx, tx, "artifact.created", payload)
			if err != nil {
				return err
			}
			if err := write.Caused(ctx, tx, event); err != nil {
				return fmt.Errorf("append what the index stated: %w", err)
			}
			return ledger.Commit(ctx)
		}()
	}()
	if answer, early := storetest.WaitForQueuedBehind(t, holder, pid, answered, fenceWait); early {
		t.Fatalf("%s answered %v without ever waiting on the owed row", writer, answer)
	}
	requireWaitingAtTheOwedRow(t, writer, statementOf(t, database, pid))

	// The pass that indexed the newer version wins the row while the version write waits.
	if _, err := holder.Exec(ctx,
		`update document_block_versions set version = $2 where artifact_id = $1`, document, newer); err != nil {
		t.Fatalf("store version %d from under %s: %v", newer, writer, err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("commit the newer pass's revision: %v", err)
	}
	if err := <-answered; err != nil {
		t.Fatalf("%s that waited on the owed row: %v", writer, err)
	}
	requireNothingBelowTheRevision(t, database, writer, document, before, own, newer)
	if stands := blockIndexAt(t, database, document); stands == nil || *stands != newer {
		t.Errorf("after %s at version %d the document's blocks stand at %s, want the stored version %d",
			writer, own, standsText(stands), newer)
	}
}

// requireTheTickIsFenced drives the block-index reconciliation through the same race: its owed
// row is held while it runs, the tick is read waiting at that row, the row is raised to a
// revision above the version the tick listed from under it, and released. The tick must commit
// no tuple below that stored revision.
func requireTheTickIsFenced(t *testing.T, database *store.Store, service *Service) {
	t.Helper()
	ctx := context.Background()
	const writer = "the reconciliation tick (ReconcileBlockIndex)"
	const own = 1
	fenced := createOwedDocument(t, database, service, "", "fenced", "- alpha\n- beta\n", "")
	settle(t, database, fenced)
	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the holder of the tick's owed row: %v", err)
	}
	defer holder.Rollback(ctx)
	var holderPID uint32
	if err := holder.QueryRow(ctx, `
		select pg_backend_pid() from document_block_versions where artifact_id = $1 for no key update
	`, fenced).Scan(&holderPID); err != nil {
		t.Fatalf("hold the tick's owed row: %v", err)
	}

	before := highestTuple(t, database)
	ticked := make(chan BlockIndexTick, 1)
	go func() {
		tick, err := service.ReconcileBlockIndex(ctx, BlockIndexCursors{}, liveAheadDocuments{})
		if err != nil {
			t.Errorf("the fenced tick: %v", err)
		}
		ticked <- tick
	}()
	if waiters := storetest.WaitForLockWaiters(t, holder, 1, fenceWait); waiters < 1 {
		t.Fatalf("the tick never queued behind the owed row")
	}
	// The tick runs on the pool, so its backend is the one the holder blocks.
	var tickPID uint32
	if err := database.Pool.QueryRow(ctx, `
		select pid from pg_stat_activity where $1 = any(pg_blocking_pids(pid)) order by query_start limit 1
	`, holderPID).Scan(&tickPID); err != nil {
		t.Fatalf("find the backend the holder of the tick's owed row blocks: %v", err)
	}
	requireWaitingAtTheOwedRow(t, writer, statementOf(t, database, tickPID))
	// A newer pass won the race while the tick waited: the row stands at a revision above the
	// version the tick listed.
	const newer = 99
	if _, err := holder.Exec(ctx,
		`update document_block_versions set version = $2 where artifact_id = $1`, fenced, newer); err != nil {
		t.Fatalf("raise the owed row from under the tick: %v", err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("commit the newer pass's mark: %v", err)
	}
	<-ticked
	// What the tick must not do is commit the work of a revision below the stored one: no tuple
	// and no projection row. The stored revision itself is not read back here, because the same
	// tick's grammar arm owes the document an index again (oweDocumentIndex) and sets it null.
	requireNothingBelowTheRevision(t, database, writer, fenced, before, own, newer)
}

// requireTheBackfillIsFenced drives the backfill command (graphcensus.Backfill) through the
// race R23 holds it to. Its race is a block a version deletes while the backfill waits: a
// document whose blocks were written before the store existed stands at version 1, and a second
// transaction - the version write that indexes version 2 and drops one of those blocks - takes
// the document's document_block_versions row, stores version 2 there and deletes the block's
// row. The backfill starts while that transaction is open, so its read of the batch still sees
// the block, and it is read waiting at the owed row. Released, it must commit no node and no
// tuple for the block version 2 deleted, and write the block version 2 kept.
func requireTheBackfillIsFenced(t *testing.T, database *store.Store) {
	t.Helper()
	ctx := context.Background()
	const writer = "the backfill command (graphcensus.Backfill)"
	const own, newer = 1, 2
	var document, refKey string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ('CORE-1', 'CORE', 'fenced-backfill', 'fenced-backfill.md', 'doc', false, '{"kind":"user","id":"alice"}')
		returning id::text, ref_key
	`).Scan(&document, &refKey); err != nil {
		t.Fatalf("create the backfill's document: %v", err)
	}
	// Version 1 and its blocks as a build before the store wrote them: rows naming no node.
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1, $2, 'Written before the store existed.', '[{"kind":"user","id":"alice"}]')
	`, document, own); err != nil {
		t.Fatalf("write version %d of the backfill's document: %v", own, err)
	}
	for _, blockID := range []string{"deleted", "kept"} {
		if _, err := database.Pool.Exec(ctx, `
			insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at)
			values ($1::uuid, $2, $3, 'paragraph', '', $4, $5, now())
		`, document, blockID, refKey+"#"+blockID, "Block "+blockID+".", own); err != nil {
			t.Fatalf("write block %s of the backfill's document: %v", blockID, err)
		}
	}
	deleted, kept := refKey+"#deleted", refKey+"#kept"

	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the version write the backfill races: %v", err)
	}
	defer holder.Rollback(ctx)
	for _, statement := range []struct {
		what, sql string
		args      []any
	}{
		{"take the owed row", `select 1 from document_block_versions where artifact_id = $1 for no key update`, []any{document}},
		{"write version 2", `insert into artifact_versions (artifact_id, number, markdown, authors) values ($1, $2, 'Version 2 drops a block.', '[{"kind":"user","id":"alice"}]')`, []any{document, newer}},
		{"store version 2 as the revision", `update document_block_versions set version = $2 where artifact_id = $1`, []any{document, newer}},
		{"delete the block version 2 drops", `delete from document_blocks where artifact_id = $1 and block_id = 'deleted'`, []any{document}},
	} {
		if _, err := holder.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("the version write could not %s: %v", statement.what, err)
		}
	}

	// One connection, so every statement of the run is that backend's and the test can watch it.
	backfiller := storetest.SingleConnection(t, database)
	var pid uint32
	if err := backfiller.Pool.QueryRow(ctx, `select pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("read the backfill's backend: %v", err)
	}
	type backfillAnswer struct {
		report graphcensus.BackfillReport
		err    error
	}
	answered := make(chan backfillAnswer, 1)
	go func() {
		report, err := graphcensus.Backfill(ctx, backfiller, io.Discard)
		answered <- backfillAnswer{report, err}
	}()
	if early, done := storetest.WaitForQueuedBehind(t, holder, pid, answered, fenceWait); done {
		t.Fatalf("%s answered (report %#v, err %v) without waiting on the owed row", writer, early.report, early.err)
	}
	requireWaitingAtTheOwedRow(t, writer, statementOf(t, database, pid))
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("commit the version write the backfill races: %v", err)
	}
	var answer backfillAnswer
	select {
	case answer = <-answered:
	case <-time.After(fenceWait):
		t.Fatalf("%s did not finish within %s of the owed row's release", writer, fenceWait)
	}
	if answer.err != nil {
		t.Fatalf("%s: %v", writer, answer.err)
	}

	var nodes, tuples int64
	if err := database.Pool.QueryRow(ctx, `
		select (select count(*) from graph_nodes where kind = $1 and id = $2),
		       (select count(*) from graph_tuples where from_kind = $1 and from_id = $2)
	`, refs.BlockNodeKind, deleted).Scan(&nodes, &tuples); err != nil {
		t.Fatalf("read what the store holds of %s: %v", deleted, err)
	}
	if nodes != 0 || tuples != 0 {
		t.Errorf("%s committed %d node(s) and %d tuple(s) for block %s at version %d, below the stored revision %d, which deleted it while the backfill waited",
			writer, nodes, tuples, deleted, own, newer)
	}
	held, err := graphstore.CurrentOut(ctx, database.Pool, refs.BlockNodeKind, kept, []string{refs.TupleInDocument})
	if err != nil {
		t.Fatalf("read the document %s is in: %v", kept, err)
	}
	if len(held) != 1 || held[0].ToID != refKey {
		t.Errorf("%s left block %s, which version %d kept, in %+v, want one in_document tuple to %s: the race proves nothing if the backfill wrote no block",
			writer, kept, newer, held, refKey)
	}
}

// fenceWriter names where a statement that writes document_block_versions may live: the one
// fenced writer. A statement that only owes a document an index writes no revision at all, so
// it is told apart by what it sets rather than by where it is.
const (
	fenceWriterFile     = "refs/blocks.go"
	fenceWriterFunction = "func standsAtVersion("
)

// owedRowWrite matches one statement of a Go source file that writes document_block_versions,
// up to the backtick that ends it.
var owedRowWrite = regexp.MustCompile("(?is)(insert into|update)\\s+document_block_versions\\b[^`]*")

// requireOnlyTheFenceWritesTheOwedRow reads the source tree and fails on any statement that
// writes a revision into document_block_versions outside the fenced writer. A statement that
// sets the revision to null owes the document an index and asserts nothing, so it is allowed
// wherever it is; one that writes a number is the fence's alone, and a backfill or a repair
// that wrote one would be caught here rather than in production.
func requireOnlyTheFenceWritesTheOwedRow(t *testing.T) {
	t.Helper()
	found := 0
	for _, file := range dispatchSourceFiles(t) {
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		text := string(body)
		for _, match := range owedRowWrite.FindAllStringIndex(text, -1) {
			statement := text[match[0]:match[1]]
			if !writesTheRevision(statement) {
				// A statement of this table that assigns no revision claims none: the
				// window pass clears `windows_owed_at` on a row the fence already wrote,
				// which says nothing about which pass owns the version. What this scan
				// holds a statement to is what it sets, not which table it touches, which
				// is the same rule the null case below reads.
				continue
			}
			if strings.Contains(statement, "version = null") {
				continue
			}
			found++
			enclosing := enclosingFunction(text, match[0])
			if !strings.HasSuffix(filepath.ToSlash(file), fenceWriterFile) || !strings.HasPrefix(enclosing, fenceWriterFunction) {
				t.Errorf("%s writes a revision into document_block_versions in %q, outside the fenced writer %s in %s:\n%s",
					file, enclosing, fenceWriterFunction, fenceWriterFile, statement)
			}
		}
	}
	if found == 0 {
		t.Errorf("the source read found no statement writing a revision into document_block_versions, so it proves nothing")
	}
}

// outsideTheFence is the two writers R23 reads as writers outside the fence: the erase, which
// deletes under its own transaction and asserts no current set, and the item block pass, which
// writes no store row in this child. Each is named by its file and its `func` line.
var outsideTheFence = []struct{ file, function string }{
	{"graphcensus/erase.go", "func (e *Erasure) apply("},
	{"refs/item_blocks.go", "func IndexOwedItemBlocks("},
}

// revisionTest matches what a revision test is made of in a function's source: a read or a
// write of document_block_versions, the row that holds a document's revision, whose `version`
// is what such a test compares against, or a call of the fence that compares it
// (standsAtVersion) or of the pass that runs that fence (IndexBlocks).
var revisionTest = regexp.MustCompile(`(?is)\bdocument_block_versions\b|\bstandsAtVersion\(|\bIndexBlocks\(`)

// requireNoRevisionTestOutsideTheFence reads the source of each writer outside the fence and
// fails if either carries a revision test. A writer that compared a revision it does not own
// would refuse a write the fence never meant to refuse, or claim the fence's row for a set it
// does not compute. The comments are taken out first, since the tree writes its reasoning
// about the fence in them.
func requireNoRevisionTestOutsideTheFence(t *testing.T) {
	t.Helper()
	for _, writer := range outsideTheFence {
		path := filepath.Join("..", filepath.FromSlash(writer.file))
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text := goLineComment.ReplaceAllString(string(source), "")
		start := strings.Index(text, "\n"+writer.function)
		if start < 0 {
			t.Fatalf("%s holds no %q: the writer R23 reads as outside the fence is not where this test looks", writer.file, writer.function)
		}
		body := functionBody(text, start+1+len(writer.function))
		t.Logf("%s %s: %d lines read for a revision test", writer.file, writer.function, strings.Count(body, "\n")+1)
		for _, found := range revisionTest.FindAllString(body, -1) {
			t.Errorf("%s %s carries a revision test (%q): R23 reads it as a writer outside the fence, which compares no revision",
				writer.file, writer.function, found)
		}
	}
}

// writesTheRevision reports whether a statement of document_block_versions assigns its
// `version` column: an update that sets it, or an insert that names it among its columns. A
// statement that touches the row for any other reason writes no revision, so the fence has
// nothing to say about where it lives.
func writesTheRevision(statement string) bool {
	lowered := strings.ToLower(statement)
	if strings.Contains(lowered, "insert into") {
		return strings.Contains(lowered, "version")
	}
	for _, assignment := range []string{"version =", "version="} {
		if strings.Contains(lowered, assignment) {
			return true
		}
	}
	return false
}

// dispatchSourceFiles is every non-test Go file of the Dispatch tree and of the commands that
// write to it, read from this package's own directory.
func dispatchSourceFiles(t *testing.T) []string {
	t.Helper()
	var files []string
	for _, root := range []string{"..", "../../../cmd"} {
		if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			files = append(files, path)
			return nil
		}); err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if len(files) == 0 {
		t.Fatalf("the source read found no Go file; the tree is not where this test looks")
	}
	return files
}

// enclosingFunction is the `func` line above offset in a gofmt-clean file, which is the
// function a statement written there lives in.
func enclosingFunction(text string, offset int) string {
	if offset < 0 {
		return ""
	}
	start := strings.LastIndex(text[:offset], "\nfunc ")
	if start < 0 {
		return ""
	}
	line := text[start+1:]
	if end := strings.IndexByte(line, '\n'); end >= 0 {
		line = line[:end]
	}
	return line
}
