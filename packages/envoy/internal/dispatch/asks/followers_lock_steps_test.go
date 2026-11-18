package asks

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest/lockstep"
)

// followLockAt is the time every follow of the lock steps test is made at, and followLockEvent the
// event a caller appends after its follow.
var followLockAt = time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)

const followLockEvent = int64(9410001)

// TestAFollowTakesTheStoreLockOrder holds Follow and the move of the node it made onto its
// caller's event (graphstore.Caused), the follow's two steps of the store, whose call sites
// graphstore's TestEveryStoreWriteCallSiteIsListed lists, to the lock order written on
// api/inbox_read.go's markInboxReadThreads, against a real Postgres. The owner
// row comes before the follow and the event after it, both in the caller's transaction, and each
// caller proves them where it is. What the follow takes itself is the follower row, which no
// foreign key ties to the store, and the session's node row, in one statement: the store's node
// arm spliced into the follower insert, which states that node's two base attributes after the row.
// Caused, run once the caller's event is appended, updates the one node row the follow made and
// no other.
func TestAFollowTakesTheStoreLockOrder(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	ask := seedFollowLockAsk(t, database)

	t.Run("a follow of a new session waits at the session's node row", func(t *testing.T) {
		who := model.Actor{Kind: "session", ID: "follow-lock-new"}
		node := graphstore.SessionNode(who.ID, who, followLockAt, graphstore.NoEvent)
		// The node is new, so it is held as a row another transaction inserted and has not
		// committed: the follow's insert of that key waits to learn whether it commits.
		hold := &lockstep.Hold{
			Name: "the session's node row", Step: lockstep.StepNode, Taken: true,
			Kind: node.Kind, ID: node.ID, Statement: "with followed as (",
		}
		lockstep.Begin(t, database, hold)
		if err := graphstore.Write(ctx, hold.Tx, []graphstore.Node{node}, nil); err != nil {
			t.Fatalf("hold %s: %v", hold.Name, err)
		}
		pid, done := startFollow(t, database, func(ctx context.Context, tx pgx.Tx) error {
			followed, err := Follow(ctx, tx, ask, who.ID, who, followLockAt, graphstore.NoEvent)
			if err != nil {
				return err
			}
			return graphstore.Caused(ctx, tx, followed.Uncaused, followLockEvent)
		})
		if queued := lockstep.Follow(t, database, "Follow", pid, done, []*lockstep.Hold{hold}); len(queued) != 1 {
			t.Fatalf("the follow queued behind %s, want the session's node row", lockstep.HoldNames(queued))
		}
		requireFollowerNode(t, database, who.ID, followLockEvent)
	})

	t.Run("a follow of a session the store holds takes no row of it and Caused moves nothing", func(t *testing.T) {
		who := model.Actor{Kind: "session", ID: "follow-lock-known"}
		// Another write made the node at the sentinel and appended no event; the follow's own
		// event is no cause of it.
		writeFollowLockNode(t, database, graphstore.SessionNode(who.ID, who, followLockAt, graphstore.NoEvent))
		holder, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the holder of the session's node row: %v", err)
		}
		defer holder.Rollback(ctx)
		if _, err := holder.Exec(ctx, `select 1 from graph_nodes where kind = $1 and id = $2 for update`,
			graphstore.KindSession, who.ID); err != nil {
			t.Fatalf("hold the session's node row: %v", err)
		}
		pid, done := startFollow(t, database, func(ctx context.Context, tx pgx.Tx) error {
			followed, err := Follow(ctx, tx, ask, who.ID, who, followLockAt.Add(time.Hour), graphstore.NoEvent)
			if err != nil {
				return err
			}
			return graphstore.Caused(ctx, tx, followed.Uncaused, followLockEvent)
		})
		deadline := time.Now().Add(lockstep.Wait)
		for answered := false; !answered; {
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("the follow of a known session: %v", err)
				}
				answered = true
			default:
				queued, err := storetest.QueuedBehind(ctx, holder, pid)
				if err != nil {
					t.Fatalf("read what the follow waits on: %v", err)
				}
				if queued {
					t.Fatalf("the follow of a session the store holds queued behind the transaction holding its node row, at %q: the node arm upserts `do nothing`, which locks no row that is already there, and Caused moves only a node its follow made",
						lockstep.WaitingStatement(t, database, pid))
				}
				if time.Now().After(deadline) {
					t.Fatalf("the follow of a known session did not answer within %s", lockstep.Wait)
				}
				time.Sleep(2 * time.Millisecond)
			}
		}
		requireFollowerNode(t, database, who.ID, graphstore.NoEvent)
	})
}

// seedFollowLockAsk writes the issue and the ask the follows of the lock steps test are about.
func seedFollowLockAsk(t *testing.T, database *store.Store) string {
	t.Helper()
	ctx := context.Background()
	for _, statement := range []string{
		`insert into projects (key, name, default_assignee) values ('FLOCK', 'Follow lock order', 'alice')`,
		`insert into issues (key, project_key, number, title, created_by, rank)
		 values ('FLOCK-1', 'FLOCK', 1, 'The follows', '{"kind":"user","id":"alice"}', 'n')`,
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed the ask's issue: %v", err)
		}
	}
	var ask string
	if err := database.Pool.QueryRow(ctx, `
		insert into asks (issue_key, author, question)
		values ('FLOCK-1', '{"kind":"user","id":"alice"}', 'Who follows?') returning id::text
	`).Scan(&ask); err != nil {
		t.Fatalf("seed the ask: %v", err)
	}
	return ask
}

// startFollow runs write in a transaction of its own on a connection of its own, so the test knows
// the backend it watches, and answers that backend and the channel the write's result arrives on.
func startFollow(t *testing.T, database *store.Store, write func(context.Context, pgx.Tx) error) (uint32, <-chan error) {
	t.Helper()
	ctx := context.Background()
	single := storetest.SingleConnection(t, database)
	var pid int64
	if err := single.Pool.QueryRow(ctx, `select pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("read the backend the follow runs on: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		tx, err := single.Pool.Begin(ctx)
		if err != nil {
			done <- err
			return
		}
		defer tx.Rollback(ctx)
		if err := write(ctx, tx); err != nil {
			done <- err
			return
		}
		done <- tx.Commit(ctx)
	}()
	return uint32(pid), done
}

// writeFollowLockNode writes node and commits it: a session another write already recorded.
func writeFollowLockNode(t *testing.T, database *store.Store, node graphstore.Node) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the node of %s: %v", node.ID, err)
	}
	defer tx.Rollback(ctx)
	if err := graphstore.Write(ctx, tx, []graphstore.Node{node}, nil); err != nil {
		t.Fatalf("write the node of %s: %v", node.ID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the node of %s: %v", node.ID, err)
	}
}

// requireFollowerNode holds a follow to what it left: one node of the session, caused by cause.
func requireFollowerNode(t *testing.T, database *store.Store, session string, cause int64) {
	t.Helper()
	var nodes int
	var caused int64
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*), coalesce(max(source_event), -1) from graph_nodes where kind = $1 and id = $2
	`, graphstore.KindSession, session).Scan(&nodes, &caused); err != nil {
		t.Fatalf("read the node of %s: %v", session, err)
	}
	if nodes != 1 || caused != cause {
		t.Fatalf("the store holds %d node(s) of %s caused by %d, want one caused by %d", nodes, session, caused, cause)
	}
}
