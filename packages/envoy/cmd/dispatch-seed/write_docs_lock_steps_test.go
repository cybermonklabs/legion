package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestTheSeedsDocumentWriteTakesTheStoreLockOrder holds the seed's write of one document
// (writeDocument) to the lock order written on api/inbox_read.go's markInboxReadThreads, against a
// real Postgres. The seed takes no owner row and appends no event: it writes the document's node
// through graphstore.Write, which appends the base attributes that node carries right after it,
// and then the document's row, which names the node by its ref_key, as the server's upload writes
// the two (api's storeArtifact). Then it writes the document's version rows, whose insert writes
// the document's revision fence row (the trigger document_block_versions_owe, migration 0122), and
// indexes the latest version (refs.IndexBlocks): the fence row again, the blocks' nodes in
// ascending (kind, id) in one statement, and their document_blocks rows, which name those nodes.
// Only then does it append what the index stated, under the sentinel, since no event recorded the
// version (graphstore.Caused). It takes a subset of the order's steps, in the order: no owner row,
// then the document's node and its tuples, the row that names it, the fence, the block nodes, the
// projection rows that name them, and the tuples the index states. The fence guards the
// document's block statement alone - its block nodes, its block rows and the tuples the index
// states - so the document's own node and tuples before it are in the order.
//
// Each step is held, with one write of a new document per step, in a second transaction, and the
// test reads where the write's own backend waits: the statement it is stopped at names the step,
// and the tables it holds locks on name every step it has run already, since Postgres keeps a table
// lock until the transaction ends. Holding the issue's row does not stop the write, so it takes no
// owner row. The document is new, so no transaction can hold one of its rows before the write
// makes it. A step that is the write's first write of its table is held as that table: share mode,
// which conflicts with an insert's lock and with no read. The block nodes and the index's tuple
// append write a table the document's node write wrote already, so each is held as one row of its
// own, which another transaction inserted and has not committed: the node row of the document's
// first paragraph (holdBlockNode), and the graph_tuples row whose seq the index's first tuple
// draws (holdIndexTuple). A lock on a table an earlier step wrote says nothing of a later step
// that writes it too, so that table is not read for the later step.
func TestTheSeedsDocumentWriteTakesTheStoreLockOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newSeedLockFixture(t)
	f.run.plan.projects = []projectPlan{{Key: seedLockProject}}
	f.run.bands = map[string]int{}

	t.Run("the write takes no owner row", func(t *testing.T) {
		index := f.nextDocument()
		holder, release := beginSeedLockHolder(t, f.database)
		var open bool
		if err := holder.QueryRow(ctx,
			`select closed_at is null from issues where key = $1 for no key update`, seedLockIssue).Scan(&open); err != nil {
			t.Fatalf("hold the issue's row: %v", err)
		}
		pid, done := f.startDocument(t, index)
		f.requireUnblocked(t, holder, pid, done, "the issue's owner row",
			"the seed takes no owner row; a write that takes one after its fence or its node rows takes the owner out of the order")
		release()
		f.requireDocumentWritten(t, index)
	})

	// The steps the write takes, in the order it takes them, each with the statement that takes
	// it, the table a lock on which says it has run, and how it is held for document index.
	holdTable := func(table string) func(*testing.T, pgx.Tx, int) {
		return func(t *testing.T, holder pgx.Tx, _ int) {
			t.Helper()
			if _, err := holder.Exec(ctx, "lock table "+table+" in share mode"); err != nil {
				t.Fatalf("hold the writes of %s: %v", table, err)
			}
		}
	}
	steps := []struct {
		name, table, statement string
		hold                   func(t *testing.T, holder pgx.Tx, index int)
	}{
		{"the document's node", "graph_nodes", "insert into graph_nodes (", holdTable("graph_nodes")},
		// The append that follows a node write starts with the nodes it reads back, and its
		// insert into graph_tuples comes after them (graphstore's queueAppend).
		{"the document's tuples", "graph_tuples", "with written as (", holdTable("graph_tuples")},
		{"the document's row", "artifacts", "insert into artifacts (", holdTable("artifacts")},
		{"the revision fence", "document_block_versions", `copy "artifact_versions"`, holdTable("document_block_versions")},
		{"the block nodes", "graph_nodes", "insert into graph_nodes (", f.holdBlockNode},
		{"the block rows", "document_blocks", "insert into document_blocks (", holdTable("document_blocks")},
		{"the tuple append", "graph_tuples", "with written as (", f.holdIndexTuple},
	}
	tables := make([]string, len(steps))
	for position, step := range steps {
		tables[position] = step.table
	}
	for at, step := range steps {
		t.Run("the write waits at "+step.name+" holding only the steps before it", func(t *testing.T) {
			index := f.nextDocument()
			holder, release := beginSeedLockHolder(t, f.database)
			step.hold(t, holder, index)
			pid, done := f.startDocument(t, index)
			f.waitForHolder(t, []pgx.Tx{holder}, pid, done, "the document write")
			running := strings.TrimSpace(seedLockStatement(t, f.database, pid))
			locked := seedLockWrites(t, f.database, pid)
			t.Logf("held at %s, the document write waits on: %s", step.name, running)
			t.Logf("held at %s, the document write holds write locks on: %v", step.name, locked)
			if !strings.HasPrefix(running, step.statement) {
				t.Errorf("the document write held at %s waits on %q, which is not the statement that takes it (%q)", step.name, running, step.statement)
			}
			for position, other := range steps {
				// A table this step or an earlier one writes is locked whether or not a later
				// step that writes it too has run.
				written := slices.Contains(tables[:at+1], other.table)
				switch has := slices.Contains(locked, other.table); {
				case position < at && !has:
					t.Errorf("the document write held at %s holds no lock on %s (it holds %v): it reached %s before %s, an earlier step",
						step.name, other.table, locked, step.name, other.name)
				case position > at && has && !written:
					t.Errorf("the document write held at %s already holds a lock on %s (it holds %v): it took %s, a later step, before %s",
						step.name, other.table, locked, other.name, step.name)
				}
			}
			release()
			requireSeedLockCommitted(t, done)
			f.requireDocumentWritten(t, index)
			storetest.RequireNoMarkedCause(t, f.database, "the seed's document write held at "+step.name)
		})
	}
}

// holdBlockNode holds the node row of the first paragraph of document index (firstParagraphBlock,
// a block of every latest version the seed lays out) as a row another transaction inserted and
// has not committed. The document's own node write touches no block's row, so the write waits on
// it at the block index's node statement, after the document's node, its tuples, its row and the
// fence. The row is held at the address the index writes a block's node at: the document's
// ref_key, '#' and the block's id.
func (f *seedLockFixture) holdBlockNode(t *testing.T, holder pgx.Tx, index int) {
	t.Helper()
	holdNewSeedLockNode(t, holder, graphstore.Node{
		Kind: refs.BlockNodeKind, ID: f.documentRefKey(index) + "#" + firstParagraphBlock, Supertag: refs.BlockSupertag,
		Visibility: graphstore.VisibilityProject, State: graphstore.StateLive,
		MadeAt: baseTime, MadeBy: seedAuthor, SourceEvent: graphstore.NoEvent,
	})
}

// holdIndexTuple holds the graph_tuples row the block index's first tuple takes for document
// index, as a row another transaction inserted and has not committed. Every tuple the write
// appends carries the sentinel, which no repeat key covers, and the document's node write appends
// to graph_tuples before the index does, so the one row of the index's append a holder can name
// before the write runs is its primary key: the seq the index's first tuple draws. Nothing else
// draws from the sequence while the write runs, and the write draws one seq for each base attribute
// its document's node write states of that new node - its visibility, its state and its project -
// and, where the documents' rows carry the node columns, one for the part_of that write states
// beside them to the document's issue (writeDocument), before the index appends, so the index's
// first tuple draws the next seq past those, and its insert waits on this row to learn whether it
// commits.
func (f *seedLockFixture) holdIndexTuple(t *testing.T, holder pgx.Tx, index int) {
	t.Helper()
	ctx := context.Background()
	document := f.run.plan.documents[index]
	node := graphstore.ArtifactNode(f.documentRefKey(index), f.run.plan.projects[document.Project].Key, "", seedAuthor, baseTime)
	before := 2
	if node.Project != "" || len(node.Parties) > 0 {
		before++
	}
	marked, err := f.run.carriesNodeMarker(ctx, "artifacts")
	if err != nil {
		t.Fatalf("read whether the documents' rows carry the node columns: %v", err)
	}
	if marked && document.Issue >= 0 {
		before++
	}
	var next int64
	if err := holder.QueryRow(ctx, `
		select coalesce(pg_sequence_last_value(pg_get_serial_sequence('graph_tuples', 'seq')::regclass), 0) + 1
	`).Scan(&next); err != nil {
		t.Fatalf("read the next seq of graph_tuples: %v", err)
	}
	if _, err := holder.Exec(ctx, `
		insert into graph_tuples (seq, from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
		values ($1, 'held', 'held', 'held', 'held', 'held', $2, $3::jsonb, 0, true)
	`, next+int64(before), baseTime, seedActor); err != nil {
		t.Fatalf("hold the seq of the block index's first tuple: %v", err)
	}
}

// documentRefKey is the ref_key of document index as writeDocument addresses its node
// (graphstore.ArtifactRefKey): its issue's key and its slug.
func (f *seedLockFixture) documentRefKey(index int) string {
	document := f.run.plan.documents[index]
	return graphstore.ArtifactRefKey(f.run.plan.issues[document.Issue].Key, f.run.plan.projects[document.Project].Key, "", document.Slug)
}

// nextDocument plans one more document of the issue, holding a single version, which is its
// latest and the one the block index records, and answers its index in the plan. Every earlier
// document stays in the plan, so each index, and the version row ids drawn from it, is new.
func (f *seedLockFixture) nextDocument() int {
	f.batches++
	f.run.plan.documents = append(f.run.plan.documents, documentPlan{
		ID:   fmt.Sprintf("5eed0000-0000-4000-9000-%012d", f.batches),
		Slug: fmt.Sprintf("seed-lock-document-%d", f.batches), Issue: 0, Project: 0,
		Versions: 1, Chain: -1,
	})
	return len(f.run.plan.documents) - 1
}

// startDocument runs writeDocument over the planned document in a transaction of its own on the
// run's one connection, as writeDocuments runs a batch of them, with whether the database's rows
// carry the node columns read first as writeDocuments reads it (carriesNodeMarker), and answers
// that connection's backend and the channel the write's result arrives on.
func (f *seedLockFixture) startDocument(t *testing.T, index int) (uint32, chan error) {
	t.Helper()
	ctx := context.Background()
	marked, err := f.run.carriesNodeMarker(ctx, "artifacts")
	if err != nil {
		t.Fatalf("read whether the documents' rows carry the node columns: %v", err)
	}
	var pid int64
	if err := f.run.database.Pool.QueryRow(ctx, `select pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("read the backend the seed runs on: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- f.run.inTransaction(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return f.run.writeDocument(ctx, tx, index, marked)
		})
	}()
	return uint32(pid), done
}

// seedLockWrites names every table one backend holds a lock on above a plain read's, in name
// order, leaving indexes and sequences out: the tables its transaction has written, or locked rows
// of, so far. A plain read takes the access share mode, which names no step - the block index
// reads the document's block rows before it writes its nodes - and a lock the backend still waits
// for is the step it is stopped at, which it has not run.
func seedLockWrites(t *testing.T, database *store.Store, pid uint32) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select distinct c.relname::text from pg_locks l
		join pg_class c on c.oid = l.relation
		where l.pid = $1 and l.granted and c.relkind in ('r', 'p') and l.mode <> 'AccessShareLock'
		order by 1`, pid)
	if err != nil {
		t.Fatalf("read the tables backend %d has written: %v", pid, err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the tables backend %d has written: %v", pid, err)
	}
	return tables
}

// requireDocumentWritten holds a write to what it writes: every block row of the document naming
// its block's node, a node of the store for each, and the in_document tuple of each, so the write
// the test followed is the one that committed.
func (f *seedLockFixture) requireDocumentWritten(t *testing.T, index int) {
	t.Helper()
	document := f.run.plan.documents[index].ID
	var rows, named, stated int
	if err := f.database.Pool.QueryRow(context.Background(), `
		select count(*),
		       count(*) filter (where exists (
		           select 1 from graph_nodes n where n.kind = b.node_kind and n.id = b.node_address)),
		       count(*) filter (where exists (
		           select 1 from graph_tuples t
		           where t.from_kind = b.node_kind and t.from_id = b.node_address and t.kind = $2))
		from document_blocks b where b.artifact_id = $1::uuid
	`, document, refs.TupleInDocument).Scan(&rows, &named, &stated); err != nil {
		t.Fatalf("read back the blocks of document %s: %v", document, err)
	}
	if rows != blocksPerLatestVersion || named != rows || stated != rows {
		t.Errorf("the write left document %s with %d block rows, %d naming a node of the store and %d with an in_document tuple, want %d of each",
			document, rows, named, stated, blocksPerLatestVersion)
	}
}
