package graphstore

import (
	"context"
	"errors"
	"os"
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

// The three nodes this test writes. nodeLower and nodeUpper are the pair a write takes together,
// named so that (kind, id) orders them the way their names read; nodeProjected is the one a
// comments row points at.
var (
	nodeLower     = lockOrderNode("block", "GRAPH/plan#a")
	nodeUpper     = lockOrderNode("block", "GRAPH/plan#b")
	nodeProjected = lockOrderNode("comment", "GRAPH-1/c1")
)

// TestEveryStoreWriteTakesOneLockOrder holds the node and tuple store's writers to the four
// invariants written on api/inbox_read.go's markInboxReadThreads, which is where this
// repository's lock order lives. It proves each of them against a real Postgres: that two
// writes handed the same nodes in opposite orders both commit, that a write waits at each step
// of the order and not at a later one, that the projections' foreign key orders nothing, and
// that the order is written down where every other writer reads it.
func TestEveryStoreWriteTakesOneLockOrder(t *testing.T) {
	ctx := context.Background()
	requireTheWrittenLockOrder(t)
	fixture := newLockOrderFixture(t)

	t.Run("two writes over the same nodes in opposite orders both commit", func(t *testing.T) {
		requireNoDeadlockBetweenOppositeOrders(t, ctx, fixture)
	})
	t.Run("a write over several nodes waits at the lowest kind and id it was given", func(t *testing.T) {
		requireTheLowestNodeIsTakenFirst(t, ctx, fixture)
	})
	t.Run("each step of the order is waited at", func(t *testing.T) {
		requireEachStepIsWaitedAt(t, ctx, fixture)
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
	if _, err := database.Pool.Exec(ctx,
		`insert into projects (key, name) values ($1, 'Graph store')`, lockOrderProj); err != nil {
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
// names one of them. It runs the four invariants in one order so that a holder of any one step's
// row can be shown to stop it at that step; the invariants themselves hold whatever order a
// writer reaches them in, which is why they are invariants and not a numbered sequence.
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
// writes are then run through the whole writer once more, with a third transaction holding a
// node until both have queued, so the pair is shown to commit after a real wait rather than
// only when the two miss each other.
func requireNoDeadlockBetweenOppositeOrders(t *testing.T, ctx context.Context, f *lockOrderFixture) {
	t.Helper()
	for round := range racingRounds {
		start := make(chan struct{})
		answers := make(chan error, 2)
		for _, order := range [][]Node{{nodeLower, nodeUpper}, {nodeUpper, nodeLower}} {
			go func(nodes []Node) {
				<-start
				answers <- writeNodesInOneTransaction(ctx, f.database.Pool, nodes)
			}(order)
		}
		close(start)
		for range 2 {
			if err := <-answers; err != nil {
				t.Fatalf("round %d of the race: %v%s", round+1, err, deadlockNote(err))
			}
		}
	}

	holder, release := beginHolder(t, ctx, f.database)
	lockNodeRow(t, ctx, holder, nodeUpper)
	first, firstDone := startStoreWrite(t, ctx, f, []Node{nodeLower, nodeUpper})
	requireQueued(t, holder, first, firstDone, "the write given the lower node first")
	second, secondDone := startStoreWrite(t, ctx, f, []Node{nodeUpper, nodeLower})
	requireQueued(t, holder, second, secondDone, "the write given the upper node first")
	release()
	requireCommitted(t, firstDone, "the write given the lower node first")
	requireCommitted(t, secondDone, "the write given the upper node first")
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
func requireTheLowestNodeIsTakenFirst(t *testing.T, ctx context.Context, f *lockOrderFixture) {
	t.Helper()
	for _, given := range []struct {
		name  string
		nodes []Node
	}{
		{"the lower node first", []Node{nodeLower, nodeUpper}},
		{"the upper node first", []Node{nodeUpper, nodeLower}},
	} {
		lower, releaseLower := beginHolder(t, ctx, f.database)
		lockNodeRow(t, ctx, lower, nodeLower)
		upper, releaseUpper := beginHolder(t, ctx, f.database)
		lockNodeRow(t, ctx, upper, nodeUpper)

		conn, done := startStoreWrite(t, ctx, f, given.nodes)
		behindLower, behindUpper := waitForTheNodeItWaitsOn(t, ctx, lower, upper, conn, done, given.name)
		if behindUpper || !behindLower {
			t.Errorf("the write given %s waits on the upper node %s/%s (%v) rather than on the lower one %s/%s (%v): a write over several nodes takes them in ascending (kind, id)",
				given.name, nodeUpper.Kind, nodeUpper.ID, behindUpper, nodeLower.Kind, nodeLower.ID, behindLower)
		}
		releaseLower()
		releaseUpper()
		requireCommitted(t, done, "the write given "+given.name)
	}
}

// waitForTheNodeItWaitsOn waits for the write's own backend to queue behind one of the two
// holders and answers which. Watching both rather than only the one the order names is what
// turns a write that takes the wrong node first into a failure that says so, instead of into a
// wait that runs out.
func waitForTheNodeItWaitsOn(
	t *testing.T, ctx context.Context, lower, upper pgx.Tx, conn *pgxpool.Conn, done chan error, given string,
) (behindLower, behindUpper bool) {
	t.Helper()
	pid := conn.Conn().PgConn().PID()
	deadline := time.Now().Add(lockWait)
	for {
		select {
		case answer := <-done:
			t.Fatalf("the write given %s finished without waiting for either node: %v%s", given, answer, deadlockNote(answer))
		default:
		}
		var err error
		if behindLower, err = storetest.QueuedBehind(ctx, lower, pid); err != nil {
			t.Fatalf("inspect the locks the write given %s waits on: %v", given, err)
		}
		if behindUpper, err = storetest.QueuedBehind(ctx, upper, pid); err != nil {
			t.Fatalf("inspect the locks the write given %s waits on: %v", given, err)
		}
		if behindLower || behindUpper {
			return behindLower, behindUpper
		}
		if time.Now().After(deadline) {
			t.Fatalf("the write given %s queued behind neither node within %s", given, lockWait)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// --- each step of the order ------------------------------------------------------------

// requireEachStepIsWaitedAt holds a row the writer's next step takes, in another transaction,
// and reads the statement the writer's own backend is stopped on. A write stopped at step k has
// run every step before it and none after it, so the statement names where in the order the
// write is: holding the issue stops it at the owner, holding the project stops it at the event,
// holding the node's row stops it at the node and so before the projection row that names it,
// and holding the comment stops it at the projection row. Each step also names the statements of
// the steps after it, which the stopped backend may not be running.
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
		name   string
		hold   func(t *testing.T, ctx context.Context, holder pgx.Tx)
		at     string
		notYet []string
	}{
		{
			name: "the owner row",
			hold: func(t *testing.T, ctx context.Context, holder pgx.Tx) {
				var open bool
				if err := holder.QueryRow(ctx, ownerStatement, lockOrderIssue).Scan(&open); err != nil {
					t.Fatalf("hold the issue row: %v", err)
				}
			},
			at:     ownerMarker,
			notYet: []string{eventMarker, nodeMarker, tupleMarker, projectionMarker},
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
			notYet: []string{nodeMarker, tupleMarker, projectionMarker},
		},
		{
			name: "the node row",
			hold: func(t *testing.T, ctx context.Context, holder pgx.Tx) {
				lockNodeRow(t, ctx, holder, nodeProjected)
			},
			at: nodeMarker,
			// A node's row is written before any projection row whose foreign key names it.
			notYet: []string{projectionMarker},
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
		running := runningStatement(t, ctx, f.database, conn.Conn().PgConn().PID())
		t.Logf("held at %s, the write waits on: %s", step.name, running)
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

// storeLockOrderClauses are this store's four invariants as that comment has to state them, plus
// the two things it has to say beside them. A phrase missing is the order being written in two
// places, or in none: a writer that reads the comment would not find this store's steps in it.
var storeLockOrderClauses = []string{
	"the owner row comes first, through ownerrowlock (owner.go) or lockartifactowner (docs/service.go)",
	"a node's graph_nodes row is written before any projection row whose foreign key names it",
	"the event is appended before the tuples whose cause it is",
	"a graph_nodes write touching several nodes takes them in ascending (kind, id)",
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

// deadlockNote names a deadlock in a failure, since that is the one failure this whole test is
// about and its SQLSTATE says nothing to a reader on its own.
func deadlockNote(err error) string {
	var pgError *pgconn.PgError
	if errors.As(err, &pgError) && pgError.Code == deadlockDetected {
		return " (SQLSTATE " + deadlockDetected + ": Postgres broke a lock cycle, which is the lock order being wrong)"
	}
	return ""
}
