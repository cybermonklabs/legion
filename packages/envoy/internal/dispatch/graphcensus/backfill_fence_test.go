package graphcensus

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// preStoreDocument is one document as a build before the node and tuple store left it: its id,
// its address, and one document_blocks row per block that names no node.
type preStoreDocument struct {
	artifactID, refKey string
}

// seedPreStoreDocument writes issue CORE-1 with one document at version 1 and a document_blocks
// row for each of blockIDs, each naming no node, which is what the backfill reads. The version
// carries markdown, so migration 0122's trigger gives the document its document_block_versions
// row, as a version gives one in production.
func seedPreStoreDocument(t *testing.T, database *store.Store, blockIDs ...string) preStoreDocument {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('CORE', 'Core', 'alice');
		insert into issues (key, project_key, number, title, created_by, rank) values
			('CORE-1', 'CORE', 1, 'One', '{"kind":"user","id":"alice"}', 'U');
	`); err != nil {
		t.Fatalf("seed issue CORE-1: %v", err)
	}
	var document preStoreDocument
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ('CORE-1', 'CORE', 'spec', 'spec.md', 'doc', false, '{"kind":"user","id":"alice"}')
		returning id::text, ref_key
	`).Scan(&document.artifactID, &document.refKey); err != nil {
		t.Fatalf("create the document: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1, 1, 'Written before the store existed.', '[{"kind":"user","id":"alice"}]')
	`, document.artifactID); err != nil {
		t.Fatalf("write version 1 of the document: %v", err)
	}
	for _, blockID := range blockIDs {
		if _, err := database.Pool.Exec(ctx, `
			insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at)
			values ($1::uuid, $2, $3, 'paragraph', '', $4, 1, now())
		`, document.artifactID, blockID, document.refKey+"#"+blockID, "Block "+blockID+"."); err != nil {
			t.Fatalf("write block %s of the document: %v", blockID, err)
		}
	}
	var owed bool
	if err := database.Pool.QueryRow(ctx, `
		select exists (select 1 from document_block_versions where artifact_id = $1)
	`, document.artifactID).Scan(&owed); err != nil || !owed {
		t.Fatalf("the document has no document_block_versions row to hold (err %v), so this test would hold nothing", err)
	}
	return document
}

// backfillAnswer is what one run of Backfill answered.
type backfillAnswer struct {
	report BackfillReport
	err    error
}

// startBackfill runs Backfill on a pool of one connection, off the test's goroutine, and answers
// that connection's backend process id and the channel the run's answer arrives on. One
// connection is what lets the test watch the run's own backend: every statement of the run
// goes through it, and a session queued on the same lock with anything else is never taken for
// it.
func startBackfill(t *testing.T, database *store.Store) (uint32, <-chan backfillAnswer) {
	t.Helper()
	backfiller := storetest.SingleConnection(t, database)
	var pid uint32
	if err := backfiller.Pool.QueryRow(context.Background(), `select pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("read the backfill's backend: %v", err)
	}
	answered := make(chan backfillAnswer, 1)
	go func() {
		report, err := Backfill(context.Background(), backfiller, io.Discard)
		answered <- backfillAnswer{report, err}
	}()
	return pid, answered
}

// awaitBackfill waits for the run's answer once the lock it waited on is released, and fails the
// test if the run failed or did not finish.
func awaitBackfill(t *testing.T, answered <-chan backfillAnswer) BackfillReport {
	t.Helper()
	select {
	case answer := <-answered:
		if answer.err != nil {
			t.Fatalf("the backfill failed: %v", answer.err)
		}
		return answer.report
	case <-time.After(time.Minute):
		t.Fatal("the backfill did not finish within a minute of the lock's release")
	}
	return BackfillReport{}
}

// waitingStatement is the statement backend pid is running, read through the test's own pool:
// once the backend has queued behind a lock, that is the statement it waits at.
func waitingStatement(t *testing.T, database *store.Store, pid uint32) string {
	t.Helper()
	var statement string
	if err := database.Pool.QueryRow(context.Background(),
		`select query from pg_stat_activity where pid = $1`, pid).Scan(&statement); err != nil {
		t.Fatalf("read the statement backend %d waits at: %v", pid, err)
	}
	return strings.Join(strings.Fields(statement), " ")
}

// blockKind is the block kind's line of a backfill report.
func blockKind(t *testing.T, report BackfillReport) KindBackfill {
	t.Helper()
	for _, done := range report.Kinds {
		if done.Kind == refs.BlockNodeKind {
			return done
		}
	}
	t.Fatalf("the backfill reports no %s kind: %#v", refs.BlockNodeKind, report)
	return KindBackfill{}
}

// R23: the backfill is one of the three writers that obey the revision fence, so it takes a
// document's owner row, then its document_block_versions row, before it writes a node for any
// block of that document, and it writes only the rows still there once it holds them.
//
// The run builds the race rather than waiting for it. A second transaction stands for a version
// write that deletes a block written before the store existed: it takes the document's owner
// row and its owed row, as every version write does (docs' lockArtifactOwner, then the fence in
// refs.IndexBlocks), and deletes the block's row. The backfill starts while that transaction is
// open, so its read of the batch sees the block, since the delete has not committed. The test
// reads the backfill waiting at the owner row, and only then lets the delete commit. A backfill
// that wrote from its first read would bring the deleted block back as a live node with an
// in_document tuple and no projection row.
func TestTheBackfillWritesNoNodeForABlockAVersionDeletedWhileItWaited(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	document := seedPreStoreDocument(t, database, "deleted", "kept")
	deleted, kept := document.refKey+"#deleted", document.refKey+"#kept"

	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the version write: %v", err)
	}
	defer holder.Rollback(ctx)
	for _, statement := range []struct {
		what, sql string
		args      []any
	}{
		{"take the owner row", `select closed_at is null from issues where key = $1 for no key update`, []any{"CORE-1"}},
		{"take the owed row", `select 1 from document_block_versions where artifact_id = $1 for no key update`, []any{document.artifactID}},
		{"delete the block", `delete from document_blocks where artifact_id = $1 and block_id = 'deleted'`, []any{document.artifactID}},
	} {
		if _, err := holder.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("the version write could not %s: %v", statement.what, err)
		}
	}

	pid, answered := startBackfill(t, database)
	if early, done := storetest.WaitForQueuedBehind(t, holder, pid, answered, 30*time.Second); done {
		t.Fatalf("the backfill answered (report %#v, err %v) without waiting on the version write's locks", early.report, early.err)
	}
	waited := waitingStatement(t, database, pid)
	t.Logf("the backfill waits behind the version write at: %s", waited)
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("commit the version write: %v", err)
	}
	report := awaitBackfill(t, answered)

	var nodes, tuples int64
	if err := database.Pool.QueryRow(ctx, `
		select (select count(*) from graph_nodes where kind = $1 and id = $2),
		       (select count(*) from graph_tuples where from_kind = $1 and from_id = $2)
	`, refs.BlockNodeKind, deleted).Scan(&nodes, &tuples); err != nil {
		t.Fatalf("read what the store holds of %s: %v", deleted, err)
	}
	if nodes != 0 || tuples != 0 {
		t.Errorf("the backfill wrote %d node(s) and %d tuple(s) for %s, a block a version write deleted while the backfill waited: the deleted block came back as a node no projection row names",
			nodes, tuples, deleted)
	}

	var state string
	var memberships int64
	var nodeKind, nodeAddress *string
	if err := database.Pool.QueryRow(ctx, `
		select coalesce((select state from graph_nodes where kind = $1 and id = $2), ''),
		       (select count(*) from graph_tuples
		        where from_kind = $1 and from_id = $2 and kind = $3 and to_kind = 'artifact' and to_id = $4 and asserted),
		       d.node_kind, d.node_address
		from document_blocks d where d.artifact_id = $5::uuid and d.block_id = 'kept'
	`, refs.BlockNodeKind, kept, refs.TupleInDocument, document.refKey, document.artifactID).Scan(
		&state, &memberships, &nodeKind, &nodeAddress); err != nil {
		t.Fatalf("read what the store holds of %s: %v", kept, err)
	}
	if state != graphstore.StateLive || memberships != 1 || nodeKind == nil || *nodeKind != refs.BlockNodeKind ||
		nodeAddress == nil || *nodeAddress != kept {
		t.Errorf("the block the version write kept has node state %q, %d in_document tuple(s) and a row naming (%v, %v); want a live node, one tuple and its row naming (%s, %s)",
			state, memberships, nodeKind, nodeAddress, refs.BlockNodeKind, kept)
	}

	// The report counts what the run wrote: one row, one node, its in_document tuple and the
	// two base attributes its insert stated, and one document owed a block index.
	if blocks := blockKind(t, report); blocks.Rows != 1 || blocks.Nodes != 1 || blocks.Tuples != 3 ||
		blocks.Owed != 1 || !blocks.Complete {
		t.Errorf("the backfill reports %#v for blocks, want 1 row, 1 node, 3 tuples, 1 document owed and the cutover complete", blocks)
	}

	// The lock it waited at is the owner row, the first of the two R23 names, which is the one
	// the version write took first.
	if !strings.Contains(waited, "from issues") || !strings.Contains(waited, "for no key update") {
		t.Errorf("the backfill waited behind the version write at %q, want the document's owner row (`from issues ... for no key update`)", waited)
	}
}

// R23's order: the backfill takes the owner row, then the owed row. A second transaction holds
// the owed row alone, and while the backfill waits at it the test tries the owner row without
// waiting: a backfill that took the two in that order already holds the owner row, so the try
// is refused. One that took the owed row first, or not the owner row at all, leaves the owner
// row free, and two writers could then each hold the row the other waits for.
func TestTheBackfillHoldsTheOwnerRowWhileItWaitsForTheOwedRow(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	document := seedPreStoreDocument(t, database, "only")

	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the holder: %v", err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx,
		`select 1 from document_block_versions where artifact_id = $1 for no key update`, document.artifactID); err != nil {
		t.Fatalf("hold the owed row: %v", err)
	}

	pid, answered := startBackfill(t, database)
	if early, done := storetest.WaitForQueuedBehind(t, holder, pid, answered, 30*time.Second); done {
		t.Fatalf("the backfill answered (report %#v, err %v) without waiting on the owed row", early.report, early.err)
	}
	waited := waitingStatement(t, database, pid)
	t.Logf("the backfill waits behind the owed row at: %s", waited)
	if !strings.Contains(waited, "document_block_versions") {
		t.Errorf("the backfill waited behind the owed row at %q, want its lock of document_block_versions", waited)
	}
	_, err = database.Pool.Exec(ctx, `select 1 from issues where key = 'CORE-1' for no key update nowait`)
	var refused *pgconn.PgError
	if !errors.As(err, &refused) || refused.Code != "55P03" {
		t.Errorf("while the backfill waited at the owed row the owner row was free to take (err %v), want it held by the backfill: R23 takes the owner row, then the owed row", err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("release the owed row: %v", err)
	}
	if blocks := blockKind(t, awaitBackfill(t, answered)); blocks.Rows != 1 || blocks.Nodes != 1 {
		t.Errorf("the backfill reports %#v for blocks once the owed row was released, want the one block written", blocks)
	}
}
