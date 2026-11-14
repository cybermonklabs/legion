package graphstore

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// deadlockDetected is the SQLSTATE Postgres answers the transaction it killed to break a lock
// cycle. Nothing this store does may ever produce one.
const deadlockDetected = "40P01"

// lockWait bounds how long a step of this test waits for a backend to queue behind a holder, and
// unblockedWait how long an unblocked statement is given to finish. Both are far above what the
// work takes; a step that passes either is the lock order being wrong, not a slow machine.
const (
	lockWait       = 20 * time.Second
	unblockedWait  = 20 * time.Second
	racingRounds   = 25
	lockOrderIssue = "GRAPH-1"
	lockOrderProj  = "GRAPH"
)

// The statements the writer under test runs that this package does not own. Each is the exact
// clause the real writer takes, so a failure names the step by the statement that takes it.
const (
	// ownerStatement is the owner row every writer takes first: `for no key update` on the issue
	// the write belongs to, the clause api/owner.go adds as ownerRowLock and docs/service.go's
	// lockArtifactOwner takes on an issue-owned document.
	ownerStatement = `select closed_at is null from issues where key = $1 for no key update`
	// projectionStatement writes the projection row that names the node, the write the node's
	// own row has to come before: comments.node_kind and node_address are the foreign key
	// migration 20261109022248 added.
	projectionStatement = `update comments set node_kind = $2, node_address = $3 where id = $1::uuid`
)

// The three nodes this test writes. nodeLower and nodeUpper are a pair of one kind a write takes
// together, named so that (kind, id) orders them the way their names read; nodeProjected is the
// one a comments row points at, and its kind sorts above a block's, so nodeLower and it are a
// pair of two projected kinds in that same order.
var (
	nodeLower     = lockOrderNode("block", "GRAPH/plan#a")
	nodeUpper     = lockOrderNode("block", "GRAPH/plan#b")
	nodeProjected = lockOrderNode("comment", "GRAPH-1/c1")
)

// nodePair is two nodes one write is given together; lower is the one ascending (kind, id)
// takes first.
type nodePair struct {
	name         string
	lower, upper Node
}

// nodePairs are the pairs the node order is raced and held over: two blocks, which their ids
// alone order, and a block and a comment, two projected kinds (document_blocks and comments
// project them), which their kinds order before any id is read. A writer that ordered its nodes
// by id alone, or by kind in the wrong direction, would pass the first pair and fail the second.
var nodePairs = []nodePair{
	{name: "two blocks", lower: nodeLower, upper: nodeUpper},
	{name: "a block and a comment", lower: nodeLower, upper: nodeProjected},
}

// TestEveryStoreWriteTakesOneLockOrder holds the node and tuple store's writers to the five
// invariants written on api/inbox_read.go's markInboxReadThreads, which is where this
// repository's lock order lives. It proves them against a real Postgres: that two writes handed
// the same nodes in opposite orders both commit, over two nodes of one kind and over two
// projected kinds, that a write waits at each step of the order and not at a later one, that the
// projections' foreign key orders nothing, and that the order is written down where every other
// writer reads it, the revision fence's step among it.
func TestEveryStoreWriteTakesOneLockOrder(t *testing.T) {
	ctx := context.Background()
	requireTheWrittenLockOrder(t)
	fixture := newLockOrderFixture(t)

	for _, pair := range nodePairs {
		t.Run("two writes over "+pair.name+" in opposite orders both commit", func(t *testing.T) {
			requireNoDeadlockBetweenOppositeOrders(t, ctx, fixture, pair)
		})
		t.Run("a write over "+pair.name+" waits at the lowest kind and id it was given", func(t *testing.T) {
			requireTheLowestNodeIsTakenFirst(t, ctx, fixture, pair)
		})
	}
	t.Run("each step of the order is waited at", func(t *testing.T) {
		requireEachStepIsWaitedAt(t, ctx, fixture)
	})
	t.Run("each writer writes its nodes before it appends a tuple", func(t *testing.T) {
		requireTheNodesBeforeTheTuples(t, ctx, fixture)
	})
	t.Run("the foreign key check takes no row lock", func(t *testing.T) {
		requireTheForeignKeyTakesNoRowLock(t, ctx, fixture)
	})
}

// --- the fixture ------------------------------------------------------------------------

// lockOrderFixture is the project, issue, comment and nodes every part of this test writes
// against: one owner row, one event owner, one projection row and three nodes.
type lockOrderFixture struct {
	database *store.Store
	broker   *events.Broker
	comment  string
}

func newLockOrderFixture(t *testing.T) *lockOrderFixture {
	t.Helper()
	ctx := context.Background()
	database := storetest.Open(t)
	fixture := &lockOrderFixture{database: database, broker: events.NewBroker()}
	actor := `{"kind":"user","id":"alice"}`
	// Every project names a default assignee (migration 20261111210103). The column is a
	// lower-case login and no foreign key, so the issue's own author stands in for it.
	if _, err := database.Pool.Exec(ctx,
		`insert into projects (key, name, default_assignee) values ($1, 'Graph store', 'alice')`, lockOrderProj); err != nil {
		t.Fatalf("create the project: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ($1, $2, 1, 'The node and tuple store', $3::jsonb, 'n')
	`, lockOrderIssue, lockOrderProj, actor); err != nil {
		t.Fatalf("create the issue: %v", err)
	}
	if err := database.Pool.QueryRow(ctx, `
		insert into comments (issue_key, author, body)
		values ($1, $2::jsonb, 'The comment this store projects') returning id::text
	`, lockOrderIssue, actor).Scan(&fixture.comment); err != nil {
		t.Fatalf("create the comment: %v", err)
	}
	// Every node a holder below locks has to exist before it can be held, and the write under
	// test writes each of them again: a node's row is the projection of its tuples and Write
	// may write it more than once.
	writeInOneTransaction(t, ctx, database.Pool,
		[]Node{nodeLower, nodeUpper, nodeProjected}, nil, "the nodes the lock order test writes")
	return fixture
}

// lockOrderNode is one node as this test writes it.
func lockOrderNode(kind, id string) Node {
	return Node{
		Kind: kind, ID: id, Supertag: kind,
		Visibility: VisibilityProject, State: StateLive,
		MadeAt: time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC),
		MadeBy: model.Actor{Kind: "user", ID: "alice"}, SourceEvent: NoEvent,
	}
}

// storeWrite is the writer under test: one transaction that takes its owner row, appends the
// event, writes the nodes and the tuples that event caused, and writes the projection row that
// names one of them. It runs the four invariants a writer outside the revision fence holds in one
// order so that a holder of any one step's row can be shown to stop it at that step; the
// invariants themselves hold whatever order a writer reaches them in, which is why they are
// invariants and not a numbered sequence.
func (f *lockOrderFixture) storeWrite(ctx context.Context, conn *pgxpool.Conn, nodes []Node) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var open bool
	if err := tx.QueryRow(ctx, ownerStatement, lockOrderIssue).Scan(&open); err != nil {
		return err
	}
	project := lockOrderProj
	event, err := f.broker.Append(ctx, tx, model.Event{
		ProjectKey: &project,
		Type:       "graph.written",
		Actor:      model.Actor{Kind: "user", ID: "alice"},
		Payload:    map[string]any{},
	})
	if err != nil {
		return err
	}
	tuples := make([]Tuple, 0, len(nodes))
	for _, node := range nodes {
		tuples = append(tuples, Tuple{
			FromKind: node.Kind, FromID: node.ID, Kind: "part_of", ToKind: "issue", ToID: lockOrderIssue,
			At: event.CreatedAt, Who: model.Actor{Kind: "user", ID: "alice"},
			SourceEvent: event.ID, Asserted: true,
		})
	}
	written := make([]Node, len(nodes))
	for index, node := range nodes {
		node.SourceEvent = event.ID
		written[index] = node
	}
	if err := Write(ctx, tx, written, tuples); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, projectionStatement, f.comment, nodeProjected.Kind, nodeProjected.ID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// --- two writes in opposite orders ---------------------------------------------------------

// requireNoDeadlockBetweenOppositeOrders runs two Write calls over the same two nodes, each
// given its nodes in the opposite order, and holds both to committing without a 40P01. The two
// race each other with nothing before them: a writer's owner row and its event already order
// two writers against each other, so a race over the nodes alone is what the node order itself
// has to survive. A writer that took its nodes in the order it was given would, on the
// interleaving where each has written its first row, hold one node and wait for the other while
// the other writer held that one and waited for the first - the cycle Postgres breaks with a
// 40P01, which neither writer here may ever read.
//
// Which interleaving a round lands on is the scheduler's business, so the pair is also held to
// the order directly, where nothing is left to chance (requireTheLowestNodeIsTakenFirst). Both
// writes are then run through the whole writer once more, with a third transaction holding the
// upper node until both have queued, so the pair is shown to commit after a real wait rather
// than only when the two miss each other.
func requireNoDeadlockBetweenOppositeOrders(t *testing.T, ctx context.Context, f *lockOrderFixture, pair nodePair) {
	t.Helper()
	for round := range racingRounds {
		start := make(chan struct{})
		answers := make(chan error, 2)
		for _, order := range [][]Node{{pair.lower, pair.upper}, {pair.upper, pair.lower}} {
			go func(nodes []Node) {
				<-start
				answers <- writeNodesInOneTransaction(ctx, f.database.Pool, nodes)
			}(order)
		}
		close(start)
		for range 2 {
			if err := <-answers; err != nil {
				t.Fatalf("round %d of the race over %s: %v%s", round+1, pair.name, err, deadlockNote(err))
			}
		}
	}

	holder, release := beginHolder(t, ctx, f.database)
	lockNodeRow(t, ctx, holder, pair.upper)
	first, firstDone := startStoreWrite(t, ctx, f, []Node{pair.lower, pair.upper})
	requireQueued(t, holder, first, firstDone, "the write given the lower of "+pair.name+" first")
	second, secondDone := startStoreWrite(t, ctx, f, []Node{pair.upper, pair.lower})
	requireQueued(t, holder, second, secondDone, "the write given the upper of "+pair.name+" first")
	release()
	requireCommitted(t, firstDone, "the write given the lower of "+pair.name+" first")
	requireCommitted(t, secondDone, "the write given the upper of "+pair.name+" first")
}

// writeNodesInOneTransaction is one Write of nodes alone, committed: the bare call the race
// above puts against itself.
func writeNodesInOneTransaction(ctx context.Context, pool *store.Pool, nodes []Node) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := Write(ctx, tx, nodes, nil); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// requireTheLowestNodeIsTakenFirst holds a write over several nodes to taking them in ascending
// (kind, id): two transactions hold the two nodes, one each, and the write must queue behind the
// one holding the lower node whichever order it was given its nodes in. That is the invariant
// behind the pair above - with two writers taking the same nodes in one order there is no cycle
// to wait in - and it is read here directly rather than inferred from the absence of a deadlock.
func requireTheLowestNodeIsTakenFirst(t *testing.T, ctx context.Context, f *lockOrderFixture, pair nodePair) {
	t.Helper()
	for _, given := range []struct {
		name  string
		nodes []Node
	}{
		{"the lower of " + pair.name + " first", []Node{pair.lower, pair.upper}},
		{"the upper of " + pair.name + " first", []Node{pair.upper, pair.lower}},
	} {
		lower, releaseLower := beginHolder(t, ctx, f.database)
		lockNodeRow(t, ctx, lower, pair.lower)
		upper, releaseUpper := beginHolder(t, ctx, f.database)
		lockNodeRow(t, ctx, upper, pair.upper)

		conn, done := startStoreWrite(t, ctx, f, given.nodes)
		behindLower, behindUpper := waitForTheHolderItWaitsOn(t, ctx, lower, upper, conn, done, "the write given "+given.name)
		if behindUpper || !behindLower {
			t.Errorf("the write given %s waits on the upper node %s/%s (%v) rather than on the lower one %s/%s (%v): a write over several nodes takes them in ascending (kind, id)",
				given.name, pair.upper.Kind, pair.upper.ID, behindUpper, pair.lower.Kind, pair.lower.ID, behindLower)
		}
		releaseLower()
		releaseUpper()
		requireCommitted(t, done, "the write given "+given.name)
	}
}

// waitForTheHolderItWaitsOn waits for the write's own backend to queue behind one of two
// holders and answers which. Watching both rather than only the one the order names is what
// turns a write that takes the wrong row first into a failure that says so, instead of into a
// wait that runs out.
func waitForTheHolderItWaitsOn(
	t *testing.T, ctx context.Context, first, second pgx.Tx, conn *pgxpool.Conn, done chan error, what string,
) (behindFirst, behindSecond bool) {
	t.Helper()
	pid := conn.Conn().PgConn().PID()
	deadline := time.Now().Add(lockWait)
	for {
		select {
		case answer := <-done:
			t.Fatalf("%s finished without waiting for either holder: %v%s", what, answer, deadlockNote(answer))
		default:
		}
		var err error
		if behindFirst, err = storetest.QueuedBehind(ctx, first, pid); err != nil {
			t.Fatalf("inspect the locks %s waits on: %v", what, err)
		}
		if behindSecond, err = storetest.QueuedBehind(ctx, second, pid); err != nil {
			t.Fatalf("inspect the locks %s waits on: %v", what, err)
		}
		if behindFirst || behindSecond {
			return behindFirst, behindSecond
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s queued behind neither holder within %s", what, lockWait)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// --- each step of the order ------------------------------------------------------------

// requireEachStepIsWaitedAt holds a row the writer's next step takes, in another transaction,
// and reads the statement the writer's own backend is stopped on. A write stopped at step k has
// run every step before it and none after it, so the statement names where in the order the
// write is: holding the issue stops it at the owner, holding the project stops it at the event,
// holding the node's row stops it at the node, holding a tuple of its own repeat key stops it at
// the tuple append and so before the projection row, and holding the comment stops it at the
// projection row. Each step also names the statements of the steps after it, which the stopped
// backend may not be running, and the tables those steps write, which the stopped backend may
// not have touched yet: Postgres keeps every table lock a transaction takes until it ends, so a
// step the write has already run shows as a lock on its table, where the running statement
// alone only names the step the write is stopped at.
func requireEachStepIsWaitedAt(t *testing.T, ctx context.Context, f *lockOrderFixture) {
	t.Helper()
	const (
		ownerMarker      = "from issues where key = $1"
		eventMarker      = "from projects where key = $1"
		nodeMarker       = "insert into graph_nodes"
		tupleMarker      = "insert into graph_tuples"
		projectionMarker = "update comments set node_kind"
	)
	for _, step := range []struct {
		name        string
		hold        func(t *testing.T, ctx context.Context, holder pgx.Tx)
		at          string
		notYet      []string
		laterTables []string
	}{
		{
			name: "the owner row",
			hold: func(t *testing.T, ctx context.Context, holder pgx.Tx) {
				var open bool
				if err := holder.QueryRow(ctx, ownerStatement, lockOrderIssue).Scan(&open); err != nil {
					t.Fatalf("hold the issue row: %v", err)
				}
			},
			at:          ownerMarker,
			notYet:      []string{eventMarker, nodeMarker, tupleMarker, projectionMarker},
			laterTables: []string{"projects", "events", "graph_nodes", "graph_tuples", "comments"},
		},
		{
			name: "the event's owner row",
			hold: func(t *testing.T, ctx context.Context, holder pgx.Tx) {
				var seq int
				if err := holder.QueryRow(ctx,
					`select last_seq from projects where key = $1 for no key update`, lockOrderProj).Scan(&seq); err != nil {
					t.Fatalf("hold the project row: %v", err)
				}
			},
			at: eventMarker,
			// The event is appended before the tuples whose cause it is, so a write stopped at
			// the event has written no tuple: a tuple's source_event always names a row the log
			// already holds.
			notYet:      []string{nodeMarker, tupleMarker, projectionMarker},
			laterTables: []string{"graph_nodes", "graph_tuples", "comments"},
		},
		{
			name: "the node row",
			hold: func(t *testing.T, ctx context.Context, holder pgx.Tx) {
				lockNodeRow(t, ctx, holder, nodeProjected)
			},
			at: nodeMarker,
			// A node's row is written before the tuples stated about it and before any
			// projection row whose foreign key names it.
			notYet:      []string{tupleMarker, projectionMarker},
			laterTables: []string{"graph_tuples", "comments"},
		},
		{
			name: "the tuple append",
			hold: holdTheTupleAppend,
			at:   tupleMarker,
			// The tuples are appended before the projection row that names their node, which
			// is where storeWrite reaches them; the projection's foreign key reads the node's
			// row, not a tuple, so it is the order of this writer and not of the constraint.
			notYet:      []string{projectionMarker},
			laterTables: []string{"comments"},
		},
		{
			name: "the projection row",
			hold: func(t *testing.T, ctx context.Context, holder pgx.Tx) {
				var id string
				if err := holder.QueryRow(ctx,
					`select id::text from comments where id = $1::uuid for no key update`, f.comment).Scan(&id); err != nil {
					t.Fatalf("hold the comment row: %v", err)
				}
			},
			at: projectionMarker,
		},
	} {
		holder, release := beginHolder(t, ctx, f.database)
		step.hold(t, ctx, holder)
		conn, done := startStoreWrite(t, ctx, f, []Node{nodeProjected})
		requireQueued(t, holder, conn, done, "the write stopped at "+step.name)
		pid := conn.Conn().PgConn().PID()
		running := runningStatement(t, ctx, f.database, pid)
		locked := tablesLocked(t, ctx, f.database, pid)
		t.Logf("held at %s, the write waits on: %s", step.name, running)
		t.Logf("held at %s, the write holds locks on: %v", step.name, locked)
		if !strings.Contains(running, step.at) {
			t.Errorf("the write held at %s waits on %q, which is not the statement that takes that step (%q)",
				step.name, running, step.at)
		}
		for _, later := range step.notYet {
			if strings.Contains(running, later) {
				t.Errorf("the write held at %s waits on %q, a statement of a later step: it reached %s before taking %s",
					step.name, running, later, step.name)
			}
		}
		for _, later := range step.laterTables {
			if slices.Contains(locked, later) {
				t.Errorf("the write held at %s already holds a lock on %s, a table of a later step (it holds %v): it ran that step before taking %s",
					step.name, later, locked, step.name)
			}
		}
		release()
		requireCommitted(t, done, "the write stopped at "+step.name)
	}
	var kind, address string
	if err := f.database.Pool.QueryRow(ctx,
		`select node_kind, node_address from comments where id = $1::uuid`, f.comment).Scan(&kind, &address); err != nil {
		t.Fatalf("read the projection row back: %v", err)
	}
	if kind != nodeProjected.Kind || address != nodeProjected.ID {
		t.Errorf("the comment projects %s/%s, want %s/%s", kind, address, nodeProjected.Kind, nodeProjected.ID)
	}
}

// holdTheTupleAppend makes the write's tuple append wait on holder. Appending a tuple locks no
// row of another table, so the one thing an append can wait on is a tuple of its own repeat key
// (graph_tuples_once) that another transaction inserted and has not yet committed: `on conflict
// do nothing` cannot tell whether the two conflict until that transaction ends. The key holds
// the event, so the holder first fixes the id the write's event takes - it draws one id from
// events' sequence, and the write's Append draws the next - and then inserts, under that id, the
// tuple storeWrite states for nodeProjected. A wrong id is no conflict and the write runs
// through unblocked, which requireQueued fails.
func holdTheTupleAppend(t *testing.T, ctx context.Context, holder pgx.Tx) {
	t.Helper()
	var drawn int64
	if err := holder.QueryRow(ctx, `select nextval(pg_get_serial_sequence('events', 'id'))`).Scan(&drawn); err != nil {
		t.Fatalf("fix the id of the write's event: %v", err)
	}
	insertTupleRow(t, ctx, holder, Tuple{
		FromKind: nodeProjected.Kind, FromID: nodeProjected.ID, Kind: "part_of", ToKind: "issue", ToID: lockOrderIssue,
		At: time.Now(), Who: model.Actor{Kind: "user", ID: "alice"}, SourceEvent: drawn + 1, Asserted: true,
	})
}

// --- the nodes before the tuples -----------------------------------------------------------

// storeWriters are the two ways a write reaches the store, each run on a connection of its own:
// Write inside a transaction, as the comment path, the block index and the backfill run it, and
// WriteArms spliced into one statement that is its own transaction, as outbox.recordBatch runs
// it and as Authored and Reached run it inside theirs.
var storeWriters = []struct {
	name  string
	write func(ctx context.Context, conn *pgxpool.Conn, nodes []Node, tuples []Tuple) error
}{
	{name: "Write", write: func(ctx context.Context, conn *pgxpool.Conn, nodes []Node, tuples []Tuple) error {
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		if err := Write(ctx, tx, nodes, tuples); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}},
	{name: "WriteArms", write: func(ctx context.Context, conn *pgxpool.Conn, nodes []Node, tuples []Tuple) error {
		arms, params, err := WriteArms(1, nodes, tuples, nil)
		if err != nil {
			return err
		}
		_, err = conn.Exec(ctx, "with "+arms+"\nselect 1", params...)
		return err
	}},
}

// requireTheNodesBeforeTheTuples holds both the node a write makes and the tuple it appends
// about that node, each as a row another transaction inserted and has not committed, and reads
// which of the two holders the write queues behind first. A write takes its node rows before it
// appends a tuple, so it queues behind the node's holder; once that one lets go it queues behind
// the tuple's, past its node step, and it commits once both have let go.
//
// WriteArms is one statement of data-modifying arms, and Postgres runs an arm the statement does
// not read after the statement's own query, the later arms of the with list first, so what
// orders its steps is which arm reads which, never the order they are written in. The node is a
// new one because the node arm upserts `do nothing`, which locks no row that is already there:
// the row a node step can wait on is the same key inserted by a transaction still open.
func requireTheNodesBeforeTheTuples(t *testing.T, ctx context.Context, f *lockOrderFixture) {
	t.Helper()
	for index, writer := range storeWriters {
		event := int64(9300001 + index)
		node := lockOrderNode("block", "GRAPH/plan#made-by-"+strings.ToLower(writer.name))
		node.SourceEvent = event
		tuple := Tuple{
			FromKind: node.Kind, FromID: node.ID, Kind: "part_of", ToKind: "issue", ToID: lockOrderIssue,
			At: node.MadeAt, Who: node.MadeBy, SourceEvent: event, Asserted: true,
		}
		nodeHolder, releaseNode := beginHolder(t, ctx, f.database)
		insertNodeRow(t, ctx, nodeHolder, node)
		tupleHolder, releaseTuple := beginHolder(t, ctx, f.database)
		insertTupleRow(t, ctx, tupleHolder, tuple)

		conn, err := f.database.Pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire a connection for %s: %v", writer.name, err)
		}
		done := make(chan error, 1)
		go func() {
			defer conn.Release()
			done <- writer.write(ctx, conn, []Node{node}, []Tuple{tuple})
		}()
		behindNode, behindTuple := waitForTheHolderItWaitsOn(t, ctx, nodeHolder, tupleHolder, conn, done, writer.name)
		if behindTuple || !behindNode {
			t.Errorf("%s waits on the transaction holding its tuple's repeat key (%v) rather than on the one holding its node %s/%s (%v): it appended a tuple before it wrote the node the tuple is about",
				writer.name, behindTuple, node.Kind, node.ID, behindNode)
		}
		releaseNode()
		requireQueued(t, tupleHolder, conn, done, writer.name+" past its node row")
		releaseTuple()
		requireCommitted(t, done, writer.name)

		var nodes, tuples int
		if err := f.database.Pool.QueryRow(ctx, `
			select (select count(*) from graph_nodes where kind = $1 and id = $2),
			       (select count(*) from graph_tuples where from_kind = $1 and from_id = $2 and kind = 'part_of' and source_event = $3)`,
			node.Kind, node.ID, event).Scan(&nodes, &tuples); err != nil {
			t.Fatalf("read back what %s wrote: %v", writer.name, err)
		}
		if nodes != 1 || tuples != 1 {
			t.Errorf("%s wrote %d rows of node %s/%s and %d of its tuple, want one of each", writer.name, nodes, node.Kind, node.ID, tuples)
		}
	}
}

// insertNodeRow inserts node's row in holder and leaves it uncommitted: a write that makes the
// node then waits at its own insert of that key to learn whether the holder's row commits.
func insertNodeRow(t *testing.T, ctx context.Context, holder pgx.Tx, node Node) {
	t.Helper()
	madeBy, err := json.Marshal(node.MadeBy)
	if err != nil {
		t.Fatalf("encode the maker of node %s/%s: %v", node.Kind, node.ID, err)
	}
	if _, err := holder.Exec(ctx, `
		insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
		values ($1, $2, $3, $4, $5, '{}'::jsonb, $6, $7::jsonb, $8)`,
		node.Kind, node.ID, node.Supertag, node.Visibility, node.State, node.MadeAt, string(madeBy), node.SourceEvent); err != nil {
		t.Fatalf("hold the row of node %s/%s: %v", node.Kind, node.ID, err)
	}
}

// insertTupleRow inserts tuple in holder and leaves it uncommitted: a write that appends the
// same repeat key then waits, since `on conflict do nothing` cannot tell whether the two
// conflict until the holder's transaction ends.
func insertTupleRow(t *testing.T, ctx context.Context, holder pgx.Tx, tuple Tuple) {
	t.Helper()
	who, err := json.Marshal(tuple.Who)
	if err != nil {
		t.Fatalf("encode the writer of the tuple under event %d: %v", tuple.SourceEvent, err)
	}
	if _, err := holder.Exec(ctx, `
		insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
		values ($1, $2, $3, $4, $5, $6, $7::jsonb, $8, $9)`,
		tuple.FromKind, tuple.FromID, tuple.Kind, tuple.ToKind, tuple.ToID, tuple.At, string(who),
		tuple.SourceEvent, tuple.Asserted); err != nil {
		t.Fatalf("hold the tuple the write appends under event %d: %v", tuple.SourceEvent, err)
	}
}

// --- the foreign key -----------------------------------------------------------------------

// requireTheForeignKeyTakesNoRowLock holds a graph_nodes row in another transaction and writes a
// projection row naming it. The foreign key reads the node under `for key share`, which conflicts
// with nothing the store takes, so the projection write is unblocked: the node's row and the
// projection row are ordered by the store's own invariant, not by the constraint. The same
// holder is shown to block a writer that takes the node's row the way the store takes it, so the
// unblocked write is the foreign key not ordering rather than the holder not holding.
func requireTheForeignKeyTakesNoRowLock(t *testing.T, ctx context.Context, f *lockOrderFixture) {
	t.Helper()
	holder, release := beginHolder(t, ctx, f.database)
	defer release()
	lockNodeRow(t, ctx, holder, nodeProjected)

	writing, cancel := context.WithTimeout(ctx, unblockedWait)
	defer cancel()
	if _, err := f.database.Pool.Exec(writing, projectionStatement,
		f.comment, nodeProjected.Kind, nodeProjected.ID); err != nil {
		t.Fatalf("write the projection row while another transaction holds its node: %v", err)
	}

	conn, done := startStoreWrite(t, ctx, f, []Node{nodeProjected})
	requireQueued(t, holder, conn, done, "a write that takes the node's row as the store takes it")
	release()
	requireCommitted(t, done, "a write that takes the node's row as the store takes it")
}

// --- the written order -------------------------------------------------------------------

// lockOrderComment is where this repository's lock order is written: the doc comment on
// markInboxReadThreads, which every other writer points at rather than saying it again.
const lockOrderComment = "../api/inbox_read.go"

// lockOrderFunction is the declaration the comment sits on.
const lockOrderFunction = "func (s *server) markInboxReadThreads("

// storeLockOrderClauses are this store's five invariants as that comment has to state them, plus
// the two things it has to say beside them. A phrase missing is the order being written in two
// places, or in none: a writer that reads the comment would not find this store's steps in it.
// The revision fence's step is two phrases, the step named and its two locks in their order,
// because the order of the two is the step: a pass that took the document_block_versions row
// before its owner would hold the fence while it waited for a writer that holds the owner and
// waits for the fence.
var storeLockOrderClauses = []string{
	"the owner row comes first, through ownerrowlock (owner.go) or lockartifactowner (docs/service.go)",
	"a node's graph_nodes row is written before any projection row whose foreign key names it",
	"the event is appended before the tuples whose cause it is",
	"a graph_nodes write touching several nodes takes them in ascending (kind, id)",
	"a writer under the revision fence",
	"takes the document's owner row, through lockartifactowner or the backfill's copy of it (graphcensus's lockdocumentowner), then the document's document_block_versions row `for no key update`",
	"appending a tuple locks no row of another table",
	"takes no owner row at all",
}

// requireTheWrittenLockOrder reads the lock order off markInboxReadThreads' own doc comment and
// fails when this store's steps are not in it. The order lives in one place on purpose, and an
// invariant this test holds the code to but the comment does not state is an invariant the next
// writer breaks without reading anything that told it not to.
func requireTheWrittenLockOrder(t *testing.T) {
	t.Helper()
	source, err := os.ReadFile(lockOrderComment)
	if err != nil {
		t.Fatalf("read %s: %v", lockOrderComment, err)
	}
	comment := docCommentAbove(t, string(source), lockOrderFunction)
	for _, clause := range storeLockOrderClauses {
		if !strings.Contains(comment, clause) {
			t.Errorf("the lock order on %s does not say %q; this store's steps belong in the one place the order is written (%s)",
				lockOrderFunction, clause, lockOrderComment)
		}
	}
}

// docCommentAbove is the doc comment immediately above declaration in source, as one lower-case
// line: the comment wraps its sentences over many lines, so a clause is looked for in the text
// rather than in the lines it happens to be laid out over.
func docCommentAbove(t *testing.T, source, declaration string) string {
	t.Helper()
	at := strings.Index(source, declaration)
	if at < 0 {
		t.Fatalf("%s holds no %s", lockOrderComment, declaration)
	}
	lines := strings.Split(source[:at], "\n")
	first := len(lines) - 1
	for first > 0 && strings.HasPrefix(strings.TrimSpace(lines[first-1]), "//") {
		first--
	}
	comment := strings.Join(lines[first:len(lines)-1], " ")
	comment = strings.ReplaceAll(comment, "//", " ")
	return strings.ToLower(whitespaceRun.ReplaceAllString(comment, " "))
}

// --- the shared steps --------------------------------------------------------------------

// beginHolder opens a transaction that holds rows for the writer under test to queue behind, and
// the call that lets it go. The release is registered as a cleanup too, so a failed assertion
// never leaves a writer queued on a lock nothing will drop.
func beginHolder(t *testing.T, ctx context.Context, database *store.Store) (pgx.Tx, func()) {
	t.Helper()
	tx, err := database.Pool.Begin(ctx)
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

// lockNodeRow holds one graph_nodes row under the level the store's own writer takes it at.
func lockNodeRow(t *testing.T, ctx context.Context, holder pgx.Tx, node Node) {
	t.Helper()
	var id string
	if err := holder.QueryRow(ctx,
		`select id from graph_nodes where kind = $1 and id = $2 for no key update`, node.Kind, node.ID).Scan(&id); err != nil {
		t.Fatalf("hold the row of node %s/%s: %v", node.Kind, node.ID, err)
	}
}

// startStoreWrite runs one store write on a connection of its own, so the test knows the backend
// it has to watch, and answers that connection and the channel the write's result arrives on.
func startStoreWrite(t *testing.T, ctx context.Context, f *lockOrderFixture, nodes []Node) (*pgxpool.Conn, chan error) {
	t.Helper()
	conn, err := f.database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection for the write: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		defer conn.Release()
		done <- f.storeWrite(ctx, conn, nodes)
	}()
	return conn, done
}

// requireQueued waits for the write's own backend to queue behind holder, and fails when the
// write answered first: such a write never waited at the step under test, so what the step
// claims about it was never put to the test.
func requireQueued(t *testing.T, holder pgx.Tx, conn *pgxpool.Conn, done chan error, what string) {
	t.Helper()
	answer, answered := storetest.WaitForQueuedBehind(t, holder, conn.Conn().PgConn().PID(), done, lockWait)
	if answered {
		t.Fatalf("%s finished without waiting for the lock a holder has: %v%s", what, answer, deadlockNote(answer))
	}
}

// requireCommitted holds a write to finishing, and to finishing without a deadlock.
func requireCommitted(t *testing.T, done chan error, what string) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s: %v%s", what, err, deadlockNote(err))
		}
	case <-time.After(unblockedWait):
		t.Fatalf("%s did not finish within %s after the holder let go", what, unblockedWait)
	}
}

// runningStatement is the statement one backend is running, which for a backend stopped on a
// lock is the statement that asked for it.
func runningStatement(t *testing.T, ctx context.Context, database *store.Store, pid uint32) string {
	t.Helper()
	var query string
	if err := database.Pool.QueryRow(ctx,
		`select query from pg_stat_activity where pid = $1`, pid).Scan(&query); err != nil {
		t.Fatalf("read the statement backend %d is running: %v", pid, err)
	}
	return strings.ToLower(whitespaceRun.ReplaceAllString(query, " "))
}

// tablesLocked names every table one backend holds or asks a lock on, in name order. Postgres
// keeps a statement's table locks until its transaction ends, so for a write stopped on a lock
// this is every table its transaction has read or written so far. It reads pg_locks through the
// pool, a fresh view on each read, and leaves indexes and sequences out: a step is named by the
// table it writes.
func tablesLocked(t *testing.T, ctx context.Context, database *store.Store, pid uint32) []string {
	t.Helper()
	rows, err := database.Pool.Query(ctx, `
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

// deadlockNote names a deadlock in a failure, since that is the one failure this whole test is
// about and its SQLSTATE says nothing to a reader on its own.
func deadlockNote(err error) string {
	var pgError *pgconn.PgError
	if errors.As(err, &pgError) && pgError.Code == deadlockDetected {
		return " (SQLSTATE " + deadlockDetected + ": Postgres broke a lock cycle, which is the lock order being wrong)"
	}
	return ""
}
