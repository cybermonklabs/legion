package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// seedLockWait bounds how long a step of the lock order test waits for the seed's backend to
// queue behind a holder, and for a batch to finish once nothing holds it. Both are far above what
// a batch of two comments takes, so a step that runs out of either is the order being wrong, not
// a slow machine.
const seedLockWait = 20 * time.Second

// The project and the issue every comment of the lock order test belongs to.
const (
	seedLockProject = "SEEDLOCK"
	seedLockIssue   = "SEEDLOCK-1"
)

// The statements two steps of the comment batch are read by. A backend stopped on a lock is
// running the statement that asked for it, so the statement names the step the batch is
// stopped at.
const (
	seedNodeStatement       = "insert into graph_nodes"
	seedProjectionStatement = "insert into comments"
)

// TestTheSeedsCommentWriteTakesTheStoreLockOrder holds one batch of the seed's comment write
// (writeComments) to the lock order written on api/inbox_read.go's markInboxReadThreads, against
// a real Postgres. The batch takes no owner row and appends no event: it writes the comments'
// nodes and their author's person node in one graphstore.Write, which appends the tuples those
// nodes carry right after them, and only then writes the comments rows whose foreign key names
// the nodes. It takes fewer steps than the order lists, as the publisher's record does, and the
// steps it takes come in the order's order.
//
// Each step is proven by holding the row it takes in a second transaction and reading where the
// batch's own backend waits: the statement it is stopped on names the step, and the tables it
// holds locks on name every step it has already run, since Postgres keeps a table lock until the
// transaction ends. Holding the issue's row does not stop the batch, so it takes no owner row.
// Holding its three nodes stops it at each in ascending (kind, id), whatever order the batch
// lists them in, and before it has touched graph_tuples or comments. Its tuples carry the
// sentinel, which no repeat key covers, so a held copy of one does not stop it either. Holding
// the id of its first comments row stops it at that insert, by which time it holds graph_nodes
// and graph_tuples, the two steps before it.
func TestTheSeedsCommentWriteTakesTheStoreLockOrder(t *testing.T) {
	ctx := context.Background()
	f := newSeedLockFixture(t)

	t.Run("the batch takes no owner row", func(t *testing.T) {
		lower, upper := f.nextBatch()
		holder, release := beginSeedLockHolder(t, f.database)
		var open bool
		if err := holder.QueryRow(ctx,
			`select closed_at is null from issues where key = $1 for no key update`, seedLockIssue).Scan(&open); err != nil {
			t.Fatalf("hold the issue's row: %v", err)
		}
		pid, done := f.startBatch(t)
		f.requireUnblocked(t, holder, pid, done, "the issue's owner row",
			"the seed takes no owner row; a batch that takes one after its graph_nodes rows takes the owner after the nodes")
		release()
		f.requireWritten(t, lower, upper)
	})

	t.Run("the batch takes its node rows in ascending (kind, id)", func(t *testing.T) {
		lower, upper := f.nextBatch()
		person := graphstore.Node{Kind: graphstore.KindPerson, ID: f.author}
		// The two comments' nodes are new, so each is held as a row another transaction inserted
		// and has not committed: the batch's insert of that key waits to learn whether it
		// commits. The author's node is there already, as it is for every batch of a run but
		// the first, and Write's upsert locks a row it finds whether or not it changes it.
		held := []struct {
			node    graphstore.Node
			hold    func(t *testing.T, holder pgx.Tx, node graphstore.Node)
			holder  pgx.Tx
			release func()
		}{
			{node: lower, hold: holdNewSeedLockNode},
			{node: upper, hold: holdNewSeedLockNode},
			{node: person, hold: holdSeedLockNodeRow},
		}
		holders := make([]pgx.Tx, len(held))
		for index := range held {
			held[index].holder, held[index].release = beginSeedLockHolder(t, f.database)
			held[index].hold(t, held[index].holder, held[index].node)
			holders[index] = held[index].holder
		}
		pid, done := f.startBatch(t)
		for index, step := range held {
			name := step.node.Kind + "/" + step.node.ID
			behind := f.waitForHolder(t, holders, pid, done, "the batch")
			if behind != index {
				waited := held[behind].node
				t.Fatalf("the batch queued behind the holder of %s/%s while the one of %s holds a node that sorts before it: a batch takes its graph_nodes rows in ascending (kind, id), whatever order it lists them in",
					waited.Kind, waited.ID, name)
			}
			running := seedLockStatement(t, f.database, pid)
			locked := seedLockTables(t, f.database, pid)
			t.Logf("held at %s, the batch waits on: %s", name, running)
			t.Logf("held at %s, the batch holds locks on: %v", name, locked)
			if !strings.Contains(running, seedNodeStatement) {
				t.Errorf("the batch held at node %s waits on %q, which is not its graph_nodes write (%q)", name, running, seedNodeStatement)
			}
			for _, later := range []string{"graph_tuples", "comments"} {
				if slices.Contains(locked, later) {
					t.Errorf("the batch held at node %s already holds a lock on %s, a table of a later step (it holds %v): it ran that step before it took its node rows",
						name, later, locked)
				}
			}
			// A holder that has let go has no transaction left to read the locks through.
			step.release()
			holders[index] = nil
		}
		requireSeedLockCommitted(t, done)
		f.requireWritten(t, lower, upper)
	})

	t.Run("the batch's tuple append waits on nothing", func(t *testing.T) {
		lower, upper := f.nextBatch()
		holder, release := beginSeedLockHolder(t, f.database)
		holdSeedLockTuple(t, holder, graphstore.Tuple{
			FromKind: lower.Kind, FromID: lower.ID, Kind: graphstore.TupleAuthoredBy,
			ToKind: graphstore.KindPerson, ToID: f.author,
			At: lower.MadeAt, Who: seedAuthor, SourceEvent: graphstore.NoEvent, Asserted: true,
		})
		pid, done := f.startBatch(t)
		f.requireUnblocked(t, holder, pid, done, "a copy of its authored_by tuple",
			"the seed's tuples carry the sentinel, which no repeat key covers, so appending one locks nothing and can wait on no other writer")
		release()
		f.requireWritten(t, lower, upper)
	})

	t.Run("the batch writes its comments rows after its nodes and its tuples", func(t *testing.T) {
		lower, upper := f.nextBatch()
		first := f.run.plan.comments[0]
		holder, release := beginSeedLockHolder(t, f.database)
		if _, err := holder.Exec(ctx, `
			insert into comments (id, issue_key, author, body) values ($1::uuid, $2, $3::jsonb, 'held')
		`, first.ID, seedLockIssue, seedActor); err != nil {
			t.Fatalf("hold the id of the batch's first comments row: %v", err)
		}
		pid, done := f.startBatch(t)
		f.waitForHolder(t, []pgx.Tx{holder}, pid, done, "the batch")
		running := seedLockStatement(t, f.database, pid)
		locked := seedLockTables(t, f.database, pid)
		t.Logf("held at its first comments row, the batch waits on: %s", running)
		t.Logf("held at its first comments row, the batch holds locks on: %v", locked)
		if !strings.Contains(running, seedProjectionStatement) {
			t.Errorf("the batch held at its first comments row waits on %q, which is not its comments write (%q)", running, seedProjectionStatement)
		}
		for _, earlier := range []struct{ table, step string }{
			{"graph_nodes", "the node rows"},
			{"graph_tuples", "the tuple append"},
		} {
			if !slices.Contains(locked, earlier.table) {
				t.Errorf("the batch held at its first comments row holds no lock on %s (it holds %v): it reached the projection rows before %s",
					earlier.table, locked, earlier.step)
			}
		}
		release()
		requireSeedLockCommitted(t, done)
		f.requireWritten(t, lower, upper)
	})
}

// --- the fixture ------------------------------------------------------------------------

// seedLockFixture is the database the lock order test writes and the seed run that writes it.
// The run is given a pool of one connection, so the backend every one of its statements runs on
// is one the test can name and watch; the holders and every read go through the database's own
// pool.
type seedLockFixture struct {
	database *store.Store
	run      *seedRun
	// author is the login the seed's author is in the store, the id of its person node.
	author string
	// batches counts the batches planned so far, which numbers each batch's comments so that no
	// two batches write one row.
	batches int
}

func newSeedLockFixture(t *testing.T) *seedLockFixture {
	t.Helper()
	ctx := context.Background()
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(ctx,
		`insert into projects (key, name, default_assignee) values ($1, 'Seed lock order', $2)`,
		seedLockProject, seedAuthor.ID); err != nil {
		t.Fatalf("create the project: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ($1, $2, 1, 'The seed''s comments', $3::jsonb, 'n')
	`, seedLockIssue, seedLockProject, seedActor); err != nil {
		t.Fatalf("create the issue: %v", err)
	}
	kind, author, ok := graphstore.AuthorOf(seedAuthor)
	if !ok || kind != graphstore.KindPerson {
		t.Fatalf("the seed's author %s names no person of the store", seedAuthor.ID)
	}
	// The author's node is there before any batch of this test runs, as it is for every batch
	// of a run but the first, so a holder can lock its row.
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the author's node: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := graphstore.Write(ctx, tx,
		[]graphstore.Node{graphstore.PersonNode(author, seedAuthor, baseTime, graphstore.NoEvent)}, nil); err != nil {
		t.Fatalf("write the author's node: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the author's node: %v", err)
	}
	return &seedLockFixture{
		database: database,
		run: &seedRun{
			database: storetest.SingleConnection(t, database),
			out:      io.Discard,
			plan: &seedPlan{
				vocabulary: newVocabulary(),
				random:     newRandom(),
				issues:     []issuePlan{{Key: seedLockIssue}},
			},
			random: rand.New(rand.NewPCG(randomSeed+1, randomSeed^0x5851f42d4c957f2d)),
		},
		author: author,
	}
}

// nextBatch plans the next batch: two comments on the issue, listed with the one whose id sorts
// higher first, so a batch that took its nodes in the order it lists them would take the upper
// one first. It answers the two comments' nodes as the batch writes them, lower first.
func (f *seedLockFixture) nextBatch() (lower, upper graphstore.Node) {
	f.batches++
	at := baseTime.Add(time.Duration(f.batches) * time.Hour)
	comment := func(number int) commentPlan {
		return commentPlan{
			ID:    fmt.Sprintf("5eed0000-0000-4000-8000-%012d", number),
			Issue: 0, Document: -1, ReplyTo: -1, Ask: -1, RepliesTo: -1,
			Blocks: 2, CreatedAt: at, MentionTo: -1,
		}
	}
	f.run.plan.comments = []commentPlan{comment(2 * f.batches), comment(2*f.batches - 1)}
	nodes, _ := f.run.commentStoreWrite(0, len(f.run.plan.comments))
	return nodes[1], nodes[0]
}

// startBatch runs writeComments over the planned batch on the run's one connection and answers
// that connection's backend and the channel the write's result arrives on.
func (f *seedLockFixture) startBatch(t *testing.T) (uint32, chan error) {
	t.Helper()
	ctx := context.Background()
	var pid int64
	if err := f.run.database.Pool.QueryRow(ctx, `select pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("read the backend the seed runs on: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- f.run.writeComments(ctx) }()
	return uint32(pid), done
}

// waitForHolder waits for the batch's backend to queue behind one of holders and answers which;
// a nil holder is one that has let go. Watching every holder rather than only the one the order
// names is what turns a batch that takes the wrong row first into a failure that says so,
// instead of into a wait that runs out.
func (f *seedLockFixture) waitForHolder(t *testing.T, holders []pgx.Tx, pid uint32, done chan error, what string) int {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(seedLockWait)
	for {
		select {
		case err := <-done:
			t.Fatalf("%s finished without waiting for any holder: %v", what, err)
		default:
		}
		for index, holder := range holders {
			if holder == nil {
				continue
			}
			queued, err := storetest.QueuedBehind(ctx, holder, pid)
			if err != nil {
				t.Fatalf("inspect the locks %s waits on: %v", what, err)
			}
			if queued {
				return index
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s queued behind no holder within %s", what, seedLockWait)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// requireUnblocked holds the batch to committing while holder holds held, without ever queueing
// behind it. A batch that does queue fails with the statement it waits at and the tables it has
// locked, which say how far into its steps it got before it took the held row.
func (f *seedLockFixture) requireUnblocked(t *testing.T, holder pgx.Tx, pid uint32, done chan error, held, why string) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(seedLockWait)
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("the batch while another transaction holds %s: %v", held, err)
			}
			return
		default:
		}
		queued, err := storetest.QueuedBehind(ctx, holder, pid)
		if err != nil {
			t.Fatalf("inspect the locks the batch waits on: %v", err)
		}
		if queued {
			t.Fatalf("the batch queued behind the holder of %s, waiting on %q with locks on %v: %s",
				held, seedLockStatement(t, f.database, pid), seedLockTables(t, f.database, pid), why)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the batch did not finish within %s while another transaction holds %s", seedLockWait, held)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// requireWritten holds a batch to what it writes: each comment's row naming the comment's own
// node, and the authored_by tuple from that node to the seed's author.
func (f *seedLockFixture) requireWritten(t *testing.T, nodes ...graphstore.Node) {
	t.Helper()
	ids := make([]string, len(nodes))
	for index, node := range nodes {
		ids[index] = node.ID
	}
	var written int
	if err := f.database.Pool.QueryRow(context.Background(), `
		select count(*) from comments c
		where c.id = any($1::uuid[]) and c.node_kind = $2 and c.node_address = c.id::text
		  and exists (
			select 1 from graph_tuples t
			where t.from_kind = c.node_kind and t.from_id = c.node_address
			  and t.kind = $3 and t.to_kind = $4 and t.to_id = $5)
	`, ids, commentNodeKind, graphstore.TupleAuthoredBy, graphstore.KindPerson, f.author).Scan(&written); err != nil {
		t.Fatalf("read back what the batch wrote: %v", err)
	}
	if written != len(nodes) {
		t.Errorf("the batch wrote %d of its %d comments as a row naming its node with an authored_by tuple to %s", written, len(nodes), f.author)
	}
}

// --- the holders ------------------------------------------------------------------------

// beginSeedLockHolder opens a transaction that holds rows for the batch to queue behind, and the
// call that lets it go. The release is registered as a cleanup too, so a failed assertion never
// leaves the batch queued on a lock nothing will drop.
func beginSeedLockHolder(t *testing.T, database *store.Store) (pgx.Tx, func()) {
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

// holdNewSeedLockNode inserts a node's row in holder and leaves it uncommitted: a write that
// makes the node then waits at its own insert of that key to learn whether the holder's row
// commits.
func holdNewSeedLockNode(t *testing.T, holder pgx.Tx, node graphstore.Node) {
	t.Helper()
	madeBy, err := json.Marshal(node.MadeBy)
	if err != nil {
		t.Fatalf("encode the maker of node %s/%s: %v", node.Kind, node.ID, err)
	}
	if _, err := holder.Exec(context.Background(), `
		insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
		values ($1, $2, $3, $4, $5, '{}'::jsonb, $6, $7::jsonb, $8)`,
		node.Kind, node.ID, node.Supertag, node.Visibility, node.State, node.MadeAt, string(madeBy), node.SourceEvent); err != nil {
		t.Fatalf("hold the row of node %s/%s: %v", node.Kind, node.ID, err)
	}
}

// holdSeedLockNodeRow holds a node row that is already there at the level graphstore.Write's
// upsert takes it.
func holdSeedLockNodeRow(t *testing.T, holder pgx.Tx, node graphstore.Node) {
	t.Helper()
	var id string
	if err := holder.QueryRow(context.Background(),
		`select id from graph_nodes where kind = $1 and id = $2 for no key update`, node.Kind, node.ID).Scan(&id); err != nil {
		t.Fatalf("hold the row of node %s/%s: %v", node.Kind, node.ID, err)
	}
}

// holdSeedLockTuple inserts a tuple in holder and leaves it uncommitted.
func holdSeedLockTuple(t *testing.T, holder pgx.Tx, tuple graphstore.Tuple) {
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
		t.Fatalf("hold the tuple %s %s/%s: %v", tuple.Kind, tuple.FromKind, tuple.FromID, err)
	}
}

// requireSeedLockCommitted holds the batch to finishing once its holders have let go.
func requireSeedLockCommitted(t *testing.T, done chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the batch: %v", err)
		}
	case <-time.After(seedLockWait):
		t.Fatalf("the batch did not finish within %s after its holders let go", seedLockWait)
	}
}

// --- what the backend is doing ------------------------------------------------------------

// seedLockSpaces is a run of whitespace, which a statement is read with as one space.
var seedLockSpaces = regexp.MustCompile(`\s+`)

// seedLockStatement is the statement one backend is running, as one lower-case line: for a
// backend stopped on a lock, the statement that asked for it. It reads pg_stat_activity through
// the pool, a fresh view on each read.
func seedLockStatement(t *testing.T, database *store.Store, pid uint32) string {
	t.Helper()
	var query string
	if err := database.Pool.QueryRow(context.Background(),
		`select query from pg_stat_activity where pid = $1`, pid).Scan(&query); err != nil {
		t.Fatalf("read the statement backend %d is running: %v", pid, err)
	}
	return strings.ToLower(seedLockSpaces.ReplaceAllString(query, " "))
}

// seedLockTables names every table one backend holds or asks a lock on, in name order, leaving
// indexes and sequences out. Postgres keeps a statement's table locks until its transaction ends,
// so for a batch stopped on a lock this is every table its transaction has read or written.
func seedLockTables(t *testing.T, database *store.Store, pid uint32) []string {
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
