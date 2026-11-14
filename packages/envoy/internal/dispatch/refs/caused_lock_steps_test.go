package refs

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// causedLockWait bounds how long a step of the lock order test waits for the pass's backend to
// queue behind a holder, and for Caused to finish once nothing holds it. Both are far above what
// a pass over three blocks takes, so a step that runs out of either is the order being wrong, not
// a slow machine.
const causedLockWait = 20 * time.Second

// The two versions every document of the lock order test is indexed at. The first holds three
// paragraphs. The second keeps a's text, changes b's and drops c, so its pass writes the nodes of
// b and c and leaves a's alone: a is a node of the document its pass never takes.
var (
	causedFirstVersion = blockDoc{markdown: "aa\nbb\ncc\n", blocks: []model.ArtifactBlock{
		{ID: "a", Type: "paragraph", From: 0, To: 3},
		{ID: "b", Type: "paragraph", From: 3, To: 6},
		{ID: "c", Type: "paragraph", From: 6, To: 9},
	}}
	causedSecondVersion = blockDoc{markdown: "aa\nBB\n", blocks: []model.ArtifactBlock{
		{ID: "a", Type: "paragraph", From: 0, To: 3},
		{ID: "b", Type: "paragraph", From: 3, To: 6},
	}}
)

// causedWriter is the writer of every version of the lock order test.
var causedWriter = model.Actor{Kind: "user", ID: "alice"}

// TestCausedTakesTheStoreLockOrder holds BlockIndexWrite.Caused, the step of a block index pass
// that runs after its caller's event, to the lock order written on api/inbox_read.go's
// markInboxReadThreads, against a real Postgres. The pass runs as a request runs it: the
// document's owner row, then IndexBlocks - the revision fence, the block nodes in ascending
// (kind, id) and the projection rows that name them - then the version's event, then Caused.
// Caused moves the event onto the nodes the pass wrote (graphstore.Caused) and appends the
// pass's tuples under it.
//
// The stamp is an update of graph_nodes rows after the project of the event, so the one way it
// could break the order is to take a node row there that the pass had not already taken in its
// node step. graphstore.Caused orders nothing itself - its update takes the rows in the order it
// is given them - so it keeps invariant 4 only because every row it stamps is already the
// pass's. The test shows it takes none of its own: when the event is appended, another
// transaction can take none of the rows Caused stamps, because the pass holds them all, and a
// holder of every other node row, the document's untouched block among them, does not stop
// Caused. Its tuples are its last step: a held copy of one stops Caused at its graph_tuples
// insert, by which time the pass holds the event and the node rows.
//
// docs/block_index_fence_test.go runs Caused after the fenced writers it races, but holds no row
// Caused takes and reads no step of it, so it proves nothing about which rows Caused locks.
func TestCausedTakesTheStoreLockOrder(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)

	t.Run("it stamps only node rows its pass took before the event", func(t *testing.T) {
		document, at := newCausedDocument(t, database, "CAUSEDA", "caused-stamp")
		pass := beginCausedPass(t, database, document, at)
		if len(pass.write.Nodes) == 0 {
			t.Fatal("the pass of version 2 stated no node, so Caused has nothing to stamp")
		}

		probe, releaseProbe := beginCausedHolder(t, database)
		if free := lockFreeNodeRows(t, probe, pass.write.Nodes); len(free) > 0 {
			t.Errorf("with the pass's event appended, another transaction still takes the rows of %v, which Caused stamps: the pass did not take them in its node step, so Caused takes a node row after the project of the event", free)
		}
		releaseProbe()

		holder, release := beginCausedHolder(t, database)
		held := lockFreeNodeRows(t, holder, nil)
		untouched := BlockNodeKind + "/" + document.address("a")
		if !slices.Contains(held, untouched) {
			t.Fatalf("the holder does not hold %s, the block the pass left alone (it holds %v)", untouched, held)
		}
		done := pass.causeInBackground(ctx)
		requireCausedUnblocked(t, database, holder, pass.pid, done)
		release()

		stamped := causedSources(t, database, document)
		for block, want := range map[string]int64{"a": graphstore.NoEvent, "b": pass.event, "c": pass.event} {
			if stamped[block] != want {
				t.Errorf("block %s's node carries source_event %d after Caused, want %d", block, stamped[block], want)
			}
		}
	})

	t.Run("its tuples wait only on their own repeat key, after the event and the node rows", func(t *testing.T) {
		document, at := newCausedDocument(t, database, "CAUSEDB", "caused-tuples")
		pass := beginCausedPass(t, database, document, at)
		if len(pass.write.Tuples) == 0 {
			t.Fatal("the pass of version 2 stated no tuple, so Caused has nothing to append")
		}
		tuple := pass.write.Tuples[0].Tuple
		tuple.SourceEvent = pass.event

		holder, release := beginCausedHolder(t, database)
		insertCausedTuple(t, holder, tuple)
		done := pass.causeInBackground(ctx)
		if answer, early := storetest.WaitForQueuedBehind(t, holder, pass.pid, done, causedLockWait); early {
			t.Fatalf("Caused finished without waiting on the held copy of its %s tuple of %s: %v", tuple.Kind, tuple.FromID, answer)
		}
		running := causedStatement(t, database, pass.pid)
		locked := causedTables(t, database, pass.pid)
		t.Logf("held at its %s tuple, Caused waits on: %s", tuple.Kind, running)
		t.Logf("held at its %s tuple, the pass holds locks on: %v", tuple.Kind, locked)
		if !strings.Contains(running, "insert into graph_tuples") {
			t.Errorf("Caused held at its %s tuple waits on %q, which is not its tuple append", tuple.Kind, running)
		}
		for _, earlier := range []struct{ table, step string }{
			{"events", "the event"},
			{"graph_nodes", "the node rows"},
		} {
			if !slices.Contains(locked, earlier.table) {
				t.Errorf("held at its %s tuple, the pass holds no lock on %s (it holds %v): it appended a tuple before %s",
					tuple.Kind, earlier.table, locked, earlier.step)
			}
		}
		release()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Caused after the holder let go: %v", err)
			}
		case <-time.After(causedLockWait):
			t.Fatalf("Caused did not finish within %s after the holder let go", causedLockWait)
		}
	})
}

// newCausedDocument creates one document and indexes its first version at the sentinel, as the
// seed indexes a document no event recorded, so every node of it waits at source_event 0 for a
// later version's event. It records the second version's row and answers the document and the
// time that row holds.
func newCausedDocument(t *testing.T, database *store.Store, name, slug string) (parentDocument, time.Time) {
	t.Helper()
	ctx := context.Background()
	document := newParentDocument(t, database, name, slug)
	first := recordCausedVersion(t, database, document, 1, causedFirstVersion.markdown)
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the index of version 1: %v", err)
	}
	defer tx.Rollback(ctx)
	_, won, write, err := IndexBlocks(ctx, tx, document.artifact, 1, first, causedFirstVersion.markdown, causedFirstVersion.blocks, nil, nil)
	if err != nil || !won {
		t.Fatalf("index version 1 (won %v): %v", won, err)
	}
	if err := write.Caused(ctx, tx, graphstore.NoEvent); err != nil {
		t.Fatalf("append what version 1 stated: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the index of version 1: %v", err)
	}
	return document, recordCausedVersion(t, database, document, 2, causedSecondVersion.markdown)
}

// recordCausedVersion writes one artifact_versions row of the document and answers its time.
func recordCausedVersion(t *testing.T, database *store.Store, document parentDocument, number int, markdown string) time.Time {
	t.Helper()
	authors, err := json.Marshal([]model.Actor{causedWriter})
	if err != nil {
		t.Fatalf("encode the version's authors: %v", err)
	}
	var at time.Time
	if err := database.Pool.QueryRow(context.Background(), `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1, $2, $3, $4) returning created_at
	`, document.artifact, number, markdown, authors).Scan(&at); err != nil {
		t.Fatalf("write version %d: %v", number, err)
	}
	return at
}

// causedPass is one pass of the block index over a document's second version, left open after
// its event: the transaction, the connection it runs on and that connection's backend, what the
// pass stated, and the event Caused moves it onto.
type causedPass struct {
	conn  *pgxpool.Conn
	tx    pgx.Tx
	pid   uint32
	write BlockIndexWrite
	event int64
	// finished is closed once a Caused run in the background has returned, and is nil until
	// one starts.
	finished chan struct{}
}

// beginCausedPass runs a pass the way a request that writes a version runs it, up to Caused: the
// document's owner row, as lockArtifactOwner takes it for an issue's document, the block index of
// version 2, and the version's event, appended through the broker so it takes the owner's row
// and sequence as every event does.
func beginCausedPass(t *testing.T, database *store.Store, document parentDocument, at time.Time) *causedPass {
	t.Helper()
	ctx := context.Background()
	conn, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire the pass's connection: %v", err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		conn.Release()
		t.Fatalf("begin the pass: %v", err)
	}
	pass := &causedPass{conn: conn, tx: tx, pid: conn.Conn().PgConn().PID()}
	t.Cleanup(pass.close)
	var open bool
	if err := tx.QueryRow(ctx,
		`select closed_at is null from issues where key = $1 for no key update`, document.issue).Scan(&open); err != nil {
		t.Fatalf("take the document's owner row: %v", err)
	}
	_, won, write, err := IndexBlocks(ctx, tx, document.artifact, 2, at,
		causedSecondVersion.markdown, causedSecondVersion.blocks, nil, nil)
	if err != nil || !won {
		t.Fatalf("index version 2 (won %v): %v", won, err)
	}
	pass.write = write
	event, err := events.NewBroker().Append(ctx, tx, model.Event{
		IssueKey: &document.issue,
		Type:     "artifact.version",
		Actor:    causedWriter,
		Payload:  map[string]any{"artifact_id": document.artifact, "version": map[string]any{"number": 2}},
	})
	if err != nil {
		t.Fatalf("append the version's event: %v", err)
	}
	pass.event = event.ID
	return pass
}

// causeInBackground runs Caused and the commit on the pass's own transaction, off the test's
// goroutine so the test can watch where it waits, and answers the channel its result arrives on.
func (p *causedPass) causeInBackground(ctx context.Context) chan error {
	done := make(chan error, 1)
	p.finished = make(chan struct{})
	go func() {
		defer close(p.finished)
		err := p.write.Caused(ctx, p.tx, p.event)
		if err == nil {
			err = p.tx.Commit(ctx)
		}
		done <- err
	}()
	return done
}

// close ends the pass and hands its connection back. A Caused still running is waited for
// first: it runs on that connection, and the holders it may be queued behind are let go by
// cleanups that run before this one.
func (p *causedPass) close() {
	if p.finished != nil {
		select {
		case <-p.finished:
		case <-time.After(causedLockWait):
		}
	}
	_ = p.tx.Rollback(context.Background())
	p.conn.Release()
}

// beginCausedHolder opens a transaction that holds rows for the pass to queue behind, and the
// call that lets it go. The release is registered as a cleanup too, so a failed assertion never
// leaves the pass queued on a lock nothing will drop.
func beginCausedHolder(t *testing.T, database *store.Store) (pgx.Tx, func()) {
	t.Helper()
	tx, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin a holder: %v", err)
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		_ = tx.Rollback(context.Background())
	}
	t.Cleanup(release)
	return tx, release
}

// lockFreeNodeRows takes, in holder, every graph_nodes row no other transaction holds, at the
// level a node write takes it, and answers each as kind/id. Given nodes it reads only those;
// given none it reads every node of the store.
func lockFreeNodeRows(t *testing.T, holder pgx.Tx, nodes []graphstore.Node) []string {
	t.Helper()
	ctx := context.Background()
	var rows pgx.Rows
	var err error
	if nodes == nil {
		rows, err = holder.Query(ctx,
			`select kind || '/' || id from graph_nodes order by kind, id for no key update skip locked`)
	} else {
		kinds := make([]string, len(nodes))
		ids := make([]string, len(nodes))
		for index, node := range nodes {
			kinds[index], ids[index] = node.Kind, node.ID
		}
		rows, err = holder.Query(ctx, `
			select n.kind || '/' || n.id from graph_nodes n
			join unnest($1::text[], $2::text[]) as named(kind, id) on n.kind = named.kind and n.id = named.id
			order by n.kind, n.id
			for no key update of n skip locked`, kinds, ids)
	}
	if err != nil {
		t.Fatalf("take the node rows no other transaction holds: %v", err)
	}
	taken, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("take the node rows no other transaction holds: %v", err)
	}
	return taken
}

// insertCausedTuple inserts tuple in holder and leaves it uncommitted: an append of the same
// repeat key then waits, since `on conflict do nothing` cannot tell whether the two conflict
// until the holder's transaction ends.
func insertCausedTuple(t *testing.T, holder pgx.Tx, tuple graphstore.Tuple) {
	t.Helper()
	who, err := json.Marshal(tuple.Who)
	if err != nil {
		t.Fatalf("encode the writer of the held tuple: %v", err)
	}
	if _, err := holder.Exec(context.Background(), `
		insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
		values ($1, $2, $3, $4, $5, $6, $7::jsonb, $8, $9)`,
		tuple.FromKind, tuple.FromID, tuple.Kind, tuple.ToKind, tuple.ToID, tuple.At, string(who),
		tuple.SourceEvent, tuple.Asserted); err != nil {
		t.Fatalf("hold the %s tuple of %s under event %d: %v", tuple.Kind, tuple.FromID, tuple.SourceEvent, err)
	}
}

// requireCausedUnblocked holds Caused to committing while holder holds every other node row,
// without ever queueing behind it. A Caused that does queue took a node row its pass had not
// taken, and fails naming the statement it waits at.
func requireCausedUnblocked(t *testing.T, database *store.Store, holder pgx.Tx, pid uint32, done chan error) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(causedLockWait)
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Caused while another transaction holds every other node row: %v", err)
			}
			return
		default:
		}
		queued, err := storetest.QueuedBehind(ctx, holder, pid)
		if err != nil {
			t.Fatalf("inspect the locks Caused waits on: %v", err)
		}
		if queued {
			t.Fatalf("Caused queued behind the holder of the node rows its pass did not take, waiting on %q: it takes a node row after the project of the event that its pass did not take before its projection rows",
				causedStatement(t, database, pid))
		}
		if time.Now().After(deadline) {
			t.Fatalf("Caused did not finish within %s while another transaction holds every other node row", causedLockWait)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// causedSources is the source_event each block node of the document carries, by block id.
func causedSources(t *testing.T, database *store.Store, document parentDocument) map[string]int64 {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(),
		`select id, source_event from graph_nodes where kind = $1 and id like $2`, BlockNodeKind, document.refKey+"#%")
	if err != nil {
		t.Fatalf("read the causes of the document's nodes: %v", err)
	}
	defer rows.Close()
	sources := map[string]int64{}
	for rows.Next() {
		var id string
		var source int64
		if err := rows.Scan(&id, &source); err != nil {
			t.Fatalf("scan the cause of a node: %v", err)
		}
		sources[strings.TrimPrefix(id, document.refKey+"#")] = source
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the causes of the document's nodes: %v", err)
	}
	return sources
}

// causedSpaces is a run of whitespace, which a statement is read with as one space.
var causedSpaces = regexp.MustCompile(`\s+`)

// causedStatement is the statement one backend is running, as one lower-case line: for a backend
// stopped on a lock, the statement that asked for it. It reads pg_stat_activity through the
// pool, a fresh view on each read.
func causedStatement(t *testing.T, database *store.Store, pid uint32) string {
	t.Helper()
	var query string
	if err := database.Pool.QueryRow(context.Background(),
		`select query from pg_stat_activity where pid = $1`, pid).Scan(&query); err != nil {
		t.Fatalf("read the statement backend %d is running: %v", pid, err)
	}
	return strings.ToLower(causedSpaces.ReplaceAllString(query, " "))
}

// causedTables names every table one backend holds or asks a lock on, in name order, leaving
// indexes and sequences out. Postgres keeps a statement's table locks until its transaction ends,
// so for a pass stopped on a lock this is every table it has read or written.
func causedTables(t *testing.T, database *store.Store, pid uint32) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select distinct c.relname::text from pg_locks l
		join pg_class c on c.oid = l.relation
		where l.pid = $1 and c.relkind in ('r', 'p')
		order by 1`, pid)
	if err != nil {
		t.Fatalf("read the tables backend %d has locked: %v", pid, err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the tables backend %d has locked: %v", pid, err)
	}
	return tables
}
