package grapherase

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest/lockstep"
)

// R17: every store write takes one lock order, and the first of its invariants is that a store
// write takes the owner's lock first. `envoy-dispatch erase` is a store write: it is the one
// statement of the source tree that deletes tuple rows and a node row (R1, R5), and it deletes
// the projection rows of that node in the same transaction. The node it erases here is a comment
// on a document of issue CORE-1, so its owner is that issue, whose advisory lock every writer of a
// comment takes before anything else (store.LockOwner: the comment path, the backfill's comment
// batch).
//
// The run holds the comment's node row in another transaction and starts the erase. A writer
// that takes the owner's lock first holds it by the time it queues behind the node row; a writer
// that takes the node row first queues there holding nothing of the owner. The test reads which
// of the two the erase is by trying the owner's lock without waiting while the erase waits
// (store.TryLockOwners).
//
// The erase deletes graph_tuples and graph_nodes rows through graphstore.EraseNode (erase.go,
// apply), which TestEveryStoreWriteCallSiteIsListed lists, and leaves the proof of its lock order
// to this package: this test holds it to taking the owner's lock first.
func TestTheEraseTakesItsNodesOwnerRowFirst(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seeded := seedAgreeingStore(t, database)

	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the holder of the comment's node row: %v", err)
	}
	defer holder.Rollback(ctx)
	var held int
	if err := holder.QueryRow(ctx,
		`select 1 from graph_nodes where kind = $1 and id = $2 for update`,
		commentNodeKind, seeded.comment).Scan(&held); err != nil {
		t.Fatalf("hold the node row of comment %s: %v", seeded.comment, err)
	}

	type answer struct {
		erasure *Erasure
		err     error
	}
	// The run without --confirm takes no lock, so it reads past the held node row and prints the
	// digest the confirmed run is given.
	listing := dryRun(t, database, commentNodeKind, seeded.comment)
	answered := make(chan answer, 1)
	go func() {
		erasure, err := Erase(context.Background(), database.Pool.Config().ConnString(), eraseServerURL, commentNodeKind, seeded.comment, listing.Digest, time.Millisecond)
		answered <- answer{erasure, err}
	}()

	pid := eraseBackend(t, ctx, database)
	if early, done := storetest.WaitForQueuedBehind(t, holder, pid, answered, 30*time.Second); done {
		t.Fatalf("the erase answered (err %v) without waiting on the comment's node row, which another transaction held", early.err)
	}
	waited := lockstep.WaitingStatement(t, database, pid)
	t.Logf("the erase waits behind the held node row at: %s", waited)

	// The owner's lock: free to take while the erase waits means the erase does not hold it.
	taken, ownerErr := store.TryLockOwners(ctx, database.Pool, store.IssueOwner("CORE-1"))
	if ownerErr != nil {
		t.Fatalf("try the owner's lock while the erase waits: %v", ownerErr)
	}
	if len(taken) != 0 {
		t.Errorf("while the erase waited at comment %s's node row (%s), the comment's owner, issue CORE-1, had its lock free to take: R17 has a store write take the owner's lock first, and the erase takes the node row first",
			seeded.comment, waited)
	}

	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("release the node row: %v", err)
	}
	select {
	case got := <-answered:
		if got.err != nil {
			t.Fatalf("the erase failed once the node row was released: %v", got.err)
		}
		if got.erasure == nil || !got.erasure.Applied {
			t.Fatalf("the erase removed nothing once the node row was released: %+v", got.erasure)
		}
	case <-time.After(time.Minute):
		t.Fatal("the erase did not finish within a minute of the node row's release")
	}
}

// eraseBackend is the backend process id of the erase's own connection, which it opens under
// eraseApplicationName. It is read through the pool and not through a transaction, because a
// transaction reads pg_stat_activity as one snapshot and would never see the erase arrive.
func eraseBackend(t *testing.T, ctx context.Context, database *store.Store) uint32 {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var pid uint32
		err := database.Pool.QueryRow(ctx,
			`select pid from pg_stat_activity where application_name = $1 and datname = current_database() and pid <> pg_backend_pid() order by backend_start desc limit 1`,
			eraseApplicationName).Scan(&pid)
		if err == nil {
			return pid
		}
		if err != pgx.ErrNoRows {
			t.Fatalf("read the erase's backend: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("the erase opened no connection within 30 s")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
