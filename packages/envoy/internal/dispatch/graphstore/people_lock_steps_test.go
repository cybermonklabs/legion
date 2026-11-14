package graphstore

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// peopleLockAt is the time every write of the people lock steps test is made at.
var peopleLockAt = time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)

// TestAuthoredAndReachedTakeTheStoreLockOrder holds the two writers of the people and agents
// Dispatch records, Authored and Reached, to their steps of the lock order written on
// api/inbox_read.go's markInboxReadThreads, against a real Postgres. Each is one statement of
// WriteArms inside its caller's transaction, and its steps are the ones that statement takes:
// the author's or the session's node row, then the tuples. The owner row and the event come
// before either in the caller's transaction, and are proven where the caller is.
//
// One statement takes a lock on every table it writes when it starts, so where it is stopped is
// read from which holder it queues behind rather than from the tables it has locked. A new
// author's node and the tuple Authored appends about them are each held as a row another
// transaction inserted and has not committed, and Authored must queue behind the node first and
// only then behind the tuple: once for its relation arm (authored_by) and once for its value arm
// (recorded_as), since each arm is ordered after the node arm on its own. An author the store
// already holds has no node row Authored locks, since the node arm upserts `do nothing`.
//
// Reached writes the session's node and the two base attributes of a node it made. Its node is
// held the same way and Reached must queue behind it. Its attributes carry the sentinel, which no
// repeat key covers, so a held copy of them stops nothing: Reached's tuple step locks no row, and
// so cannot be taken out of order. A session the store already holds has no row Reached locks.
func TestAuthoredAndReachedTakeTheStoreLockOrder(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)

	for index, arm := range []struct {
		name  string
		actor model.Actor
		// held is the tuple of this arm that Authored appends, given the author's login and the
		// thing's id.
		held func(login, thing string, actor model.Actor, event int64) Tuple
	}{
		{
			name:  "its authored_by tuple",
			actor: model.Actor{Kind: "user", ID: "authored-relation"},
			held: func(login, thing string, actor model.Actor, event int64) Tuple {
				return Tuple{
					FromKind: "comment", FromID: thing, Kind: TupleAuthoredBy, ToKind: KindPerson, ToID: login,
					At: peopleLockAt, Who: actor, SourceEvent: event, Asserted: true,
				}
			},
		},
		{
			// A login stored in a spelling that is not its canonical one is recorded as that
			// spelling, a value, so this author's write takes the value arm too.
			name:  "its recorded_as value",
			actor: model.Actor{Kind: "user", ID: "Authored-Value"},
			held: func(login, _ string, actor model.Actor, event int64) Tuple {
				return Tuple{
					FromKind: KindPerson, FromID: login, Kind: TupleRecordedAs, ToKind: KindValue, ToID: actor.ID,
					At: peopleLockAt, Who: actor, SourceEvent: event, Asserted: true,
				}
			},
		},
	} {
		t.Run("Authored writes a new author's node before "+arm.name, func(t *testing.T) {
			event := int64(9400001 + index)
			thing := "GRAPH-1/authored-" + arm.actor.ID
			login := model.CanonicalLogin(arm.actor.ID)
			node := PersonNode(login, arm.actor, peopleLockAt, event)
			tuple := arm.held(login, thing, arm.actor, event)

			nodeHolder, releaseNode := beginHolder(t, ctx, database)
			insertNodeRow(t, ctx, nodeHolder, node)
			tupleHolder, releaseTuple := beginHolder(t, ctx, database)
			insertTupleRow(t, ctx, tupleHolder, tuple)

			conn, _, done := startPeopleWrite(t, ctx, database, func(ctx context.Context, tx pgx.Tx) error {
				return Authored(ctx, tx, "comment", thing, arm.actor, peopleLockAt, event)
			})
			behindNode, behindTuple := waitForTheHolderItWaitsOn(t, ctx, nodeHolder, tupleHolder, conn, done, "Authored")
			if behindTuple || !behindNode {
				t.Errorf("Authored waits on the transaction holding %s (%v) rather than on the one holding its author's node %s/%s (%v): it appended a tuple before it wrote the node the tuple is about",
					arm.name, behindTuple, node.Kind, node.ID, behindNode)
			}
			releaseNode()
			requireQueued(t, tupleHolder, conn, done, "Authored past its author's node")
			releaseTuple()
			requireCommitted(t, done, "Authored")

			requirePeopleRows(t, database, node, 1, tuple, 1)
		})
	}

	t.Run("Authored takes no row of an author the store already holds", func(t *testing.T) {
		actor := model.Actor{Kind: "user", ID: "authored-again"}
		login := model.CanonicalLogin(actor.ID)
		first, second := int64(9400011), int64(9400012)
		runPeopleWrite(t, ctx, database, "the author's first write", func(ctx context.Context, tx pgx.Tx) error {
			return Authored(ctx, tx, "comment", "GRAPH-1/authored-first", actor, peopleLockAt, first)
		})
		node := PersonNode(login, actor, peopleLockAt, second)
		tuple := Tuple{
			FromKind: "comment", FromID: "GRAPH-1/authored-second", Kind: TupleAuthoredBy, ToKind: KindPerson, ToID: login,
			At: peopleLockAt, Who: actor, SourceEvent: second, Asserted: true,
		}

		nodeHolder, releaseNode := beginHolder(t, ctx, database)
		lockNodeRow(t, ctx, nodeHolder, node)
		tupleHolder, releaseTuple := beginHolder(t, ctx, database)
		insertTupleRow(t, ctx, tupleHolder, tuple)

		conn, _, done := startPeopleWrite(t, ctx, database, func(ctx context.Context, tx pgx.Tx) error {
			return Authored(ctx, tx, "comment", "GRAPH-1/authored-second", actor, peopleLockAt, second)
		})
		behindNode, behindTuple := waitForTheHolderItWaitsOn(t, ctx, nodeHolder, tupleHolder, conn, done, "Authored of a known author")
		if behindNode || !behindTuple {
			t.Errorf("Authored of an author the store holds waits on the transaction holding the author's row %s/%s (%v) rather than on the one holding its tuple (%v): the node arm upserts `do nothing`, which locks no row that is already there",
				node.Kind, node.ID, behindNode, behindTuple)
		}
		// The node's holder keeps its row until Authored has committed: a write that locked
		// that row would still be waiting for it.
		releaseTuple()
		requireCommitted(t, done, "Authored of a known author while another transaction holds the author's row")
		releaseNode()

		requirePeopleRows(t, database, node, 1, tuple, 1)
	})

	t.Run("Reached waits at a new session's node", func(t *testing.T) {
		who := model.Actor{Kind: "user", ID: "alice"}
		node := SessionNode("reached-new", who, peopleLockAt, NoEvent)
		holder, release := beginHolder(t, ctx, database)
		insertNodeRow(t, ctx, holder, node)

		conn, pid, done := startPeopleWrite(t, ctx, database, func(ctx context.Context, tx pgx.Tx) error {
			return Reached(ctx, tx, DeliverySent, node.ID, who, peopleLockAt)
		})
		requireQueued(t, holder, conn, done, "Reached of a new session")
		t.Logf("held at the session's node, Reached waits on: %s", runningStatement(t, ctx, database, pid))
		release()
		requireCommitted(t, done, "Reached of a new session")

		requirePeopleRows(t, database, node, 1, Tuple{
			FromKind: node.Kind, FromID: node.ID, Kind: attributeVisibility, ToKind: KindValue, ToID: VisibilityProject,
			SourceEvent: NoEvent, Asserted: true,
		}, 1)
	})

	t.Run("Reached appends its base attributes without waiting on any other writer", func(t *testing.T) {
		who := model.Actor{Kind: "user", ID: "alice"}
		node := SessionNode("reached-attributes", who, peopleLockAt, NoEvent)
		holder, release := beginHolder(t, ctx, database)
		for _, attribute := range []struct{ kind, value string }{
			{attributeVisibility, node.Visibility},
			{attributeState, node.State},
		} {
			insertTupleRow(t, ctx, holder, Tuple{
				FromKind: node.Kind, FromID: node.ID, Kind: attribute.kind, ToKind: KindValue, ToID: attribute.value,
				At: peopleLockAt, Who: who, SourceEvent: NoEvent, Asserted: true,
			})
		}

		_, pid, done := startPeopleWrite(t, ctx, database, func(ctx context.Context, tx pgx.Tx) error {
			return Reached(ctx, tx, DeliverySent, node.ID, who, peopleLockAt)
		})
		requireUnblockedBy(t, ctx, database, holder, pid, done, "Reached while another transaction holds a copy of its base attributes",
			"its attributes carry the sentinel, which no repeat key covers, so appending them locks no row and waits on no other writer")
		release()

		requirePeopleRows(t, database, node, 1, Tuple{
			FromKind: node.Kind, FromID: node.ID, Kind: attributeState, ToKind: KindValue, ToID: StateLive,
			SourceEvent: NoEvent, Asserted: true,
		}, 1)
	})

	t.Run("Reached takes no row of a session the store already holds", func(t *testing.T) {
		who := model.Actor{Kind: "user", ID: "alice"}
		node := SessionNode("reached-again", who, peopleLockAt, NoEvent)
		runPeopleWrite(t, ctx, database, "the session's first Reached", func(ctx context.Context, tx pgx.Tx) error {
			return Reached(ctx, tx, DeliverySent, node.ID, who, peopleLockAt)
		})
		holder, release := beginHolder(t, ctx, database)
		lockNodeRow(t, ctx, holder, node)

		_, pid, done := startPeopleWrite(t, ctx, database, func(ctx context.Context, tx pgx.Tx) error {
			return Reached(ctx, tx, DeliverySent, node.ID, who, peopleLockAt.Add(time.Hour))
		})
		requireUnblockedBy(t, ctx, database, holder, pid, done, "Reached of a known session while another transaction holds its row",
			"the node arm upserts `do nothing`, which locks no row that is already there")
		release()

		// The second attempt reaching the session appends nothing: the node was already there.
		requirePeopleRows(t, database, node, 1, Tuple{
			FromKind: node.Kind, FromID: node.ID, Kind: attributeVisibility, ToKind: KindValue, ToID: VisibilityProject,
			SourceEvent: NoEvent, Asserted: true,
		}, 1)
	})
}

// startPeopleWrite runs write in a transaction of its own on a connection of its own, so the test
// knows the backend it has to watch, and answers that connection, its backend and the channel the
// write's result arrives on. The backend is read before the write starts, because a write that
// does not wait hands its connection back to the pool before the test could read it there.
func startPeopleWrite(t *testing.T, ctx context.Context, database *store.Store, write func(context.Context, pgx.Tx) error) (*pgxpool.Conn, uint32, chan error) {
	t.Helper()
	conn, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection for the write: %v", err)
	}
	pid := conn.Conn().PgConn().PID()
	done := make(chan error, 1)
	go func() {
		defer conn.Release()
		done <- func() error {
			tx, err := conn.Begin(ctx)
			if err != nil {
				return err
			}
			defer tx.Rollback(ctx)
			if err := write(ctx, tx); err != nil {
				return err
			}
			return tx.Commit(ctx)
		}()
	}()
	return conn, pid, done
}

// runPeopleWrite runs write in a transaction of its own and commits it, with nothing holding any
// row it takes: the write a later step finds already there.
func runPeopleWrite(t *testing.T, ctx context.Context, database *store.Store, what string, write func(context.Context, pgx.Tx) error) {
	t.Helper()
	_, _, done := startPeopleWrite(t, ctx, database, write)
	requireCommitted(t, done, what)
}

// requireUnblockedBy holds the write on backend pid to committing while holder holds rows,
// without ever queueing behind it. A write that does queue fails with the statement it waits at
// and why it may not.
func requireUnblockedBy(t *testing.T, ctx context.Context, database *store.Store, holder pgx.Tx, pid uint32, done chan error, what, why string) {
	t.Helper()
	deadline := time.Now().Add(unblockedWait)
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s: %v%s", what, err, deadlockNote(err))
			}
			return
		default:
		}
		queued, err := storetest.QueuedBehind(ctx, holder, pid)
		if err != nil {
			t.Fatalf("inspect the locks %s waits on: %v", what, err)
		}
		if queued {
			t.Fatalf("%s queued behind the holder, waiting on %q: %s", what, runningStatement(t, ctx, database, pid), why)
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not finish within %s", what, unblockedWait)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// requirePeopleRows holds a write to what it left: nodes rows of node's key, and tuples rows
// carrying tuple's from end, kind, far end and cause.
func requirePeopleRows(t *testing.T, database *store.Store, node Node, nodes int, tuple Tuple, tuples int) {
	t.Helper()
	var gotNodes, gotTuples int
	if err := database.Pool.QueryRow(context.Background(), `
		select (select count(*) from graph_nodes where kind = $1 and id = $2),
		       (select count(*) from graph_tuples
		        where from_kind = $3 and from_id = $4 and kind = $5 and to_kind = $6 and to_id = $7 and source_event = $8)`,
		node.Kind, node.ID, tuple.FromKind, tuple.FromID, tuple.Kind, tuple.ToKind, tuple.ToID, tuple.SourceEvent,
	).Scan(&gotNodes, &gotTuples); err != nil {
		t.Fatalf("read back what the write left: %v", err)
	}
	if gotNodes != nodes || gotTuples != tuples {
		t.Errorf("the write left %d rows of node %s/%s and %d of its %s tuple to %s/%s under event %d, want %d and %d",
			gotNodes, node.Kind, node.ID, gotTuples, tuple.Kind, tuple.ToKind, tuple.ToID, tuple.SourceEvent, nodes, tuples)
	}
}
