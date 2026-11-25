package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A claim write queued behind the pool's other writers is bounded by its own time, not by the
// queue (BoundAfterAcquire, the daemon's supervise.Deps.BoundWrite). The store here has one
// connection, and another writer holds it for 1.5 s, three times the write's 500 ms bound: the
// queue for a connection alone outlasts the bound, as it did for the last of forty claim writes a
// boot relaunches at once on a loaded node (internal/daemon's
// TestWithNoPacingTheBootWaveGoesOutInOneTick, 4 of 40 writes past 5 s, nearly all of it the wait
// for one of the pool's four connections).
//
//   - The bound the machine set before (context.WithTimeout over the whole write) cuts the queued
//     write with context.DeadlineExceeded, though the write itself would take milliseconds: the
//     defect, in the same run as its fix.
//   - BoundAfterAcquire lets the write wait for the connection and land, and the row reads it.
//   - Its write bound still cuts a write that holds a connection past it: the database's own
//     wait (pg_sleep inside the transaction) of 1.5 s against the 500 ms bound.
//   - Its acquire bound ends a wait for a connection nobody gives back within it, naming the wait
//     (errAcquireBound), so a pool that cannot give a connection does not hold the caller forever.
func TestAClaimWriteQueuedForAConnectionIsBoundedByItsOwnTimeNotByTheQueue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const write, held = 500 * time.Millisecond, 1500 * time.Millisecond
	one := secondStore(t, migratedStore(t), func(config *pgxpool.Config) { config.MaxConns = 1 })
	c := supervise.Claim{Token: "legion-legion-legion-209-implementer", Project: "legion", Tree: "LEGION-208",
		Issue: "LEGION-209", Role: claim.RoleImplementer, Generation: 1, State: supervise.StateIdle}
	if err := one.PutClaim(ctx, c); err != nil {
		t.Fatalf("seed the claim: %v", err)
	}
	// holdTheConnection takes the pool's one connection in a transaction of its own for held, and
	// answers once it holds it, with the moment it took it; the transaction ends by itself after
	// held. The moment is read inside the holder, so a slow wake of the caller cannot shorten it.
	holdTheConnection := func(t *testing.T) (<-chan error, time.Time) {
		t.Helper()
		holding, ended := make(chan time.Time, 1), make(chan error, 1)
		go func() {
			ended <- Transact(ForConcurrentWorker(ctx), one.Pool(), func(ctx context.Context, tx pgx.Tx) error {
				holding <- time.Now()
				_, err := tx.Exec(ctx, "select pg_sleep($1)", held.Seconds())
				return err
			})
		}()
		return ended, <-holding
	}
	stored := func(t *testing.T) supervise.ClaimState {
		t.Helper()
		var state string
		if err := one.Pool().QueryRow(ctx, `select state from claims where token = $1`, c.Token).Scan(&state); err != nil {
			t.Fatalf("read the claim: %v", err)
		}
		return supervise.ClaimState(state)
	}

	t.Run("the bound over the whole write cuts it in the queue", func(t *testing.T) {
		ended, _ := holdTheConnection(t)
		bounded, cancel := context.WithTimeout(ForConcurrentWorker(ctx), write)
		defer cancel()
		queued := c
		queued.State = supervise.StateQueued
		if err := one.PutClaim(bounded, queued); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("a write queued %s behind the pool's one connection under a %s bound over the whole write = %v, want the deadline: the queue alone outlasts it", held, write, err)
		}
		if err := <-ended; err != nil {
			t.Fatalf("the transaction that held the connection: %v", err)
		}
	})

	t.Run("the bound after the connection lets it wait and land", func(t *testing.T) {
		ended, heldAt := holdTheConnection(t)
		bounded, cancel := BoundAfterAcquire(ForConcurrentWorker(ctx), 10*time.Second, write)
		defer cancel()
		queued := c
		queued.State = supervise.StateQueued
		if err := one.PutClaim(bounded, queued); err != nil {
			t.Fatalf("a write queued %s behind the pool's one connection, bounded %s after it holds one = %v, want it landed", held, write, err)
		}
		if waited := time.Since(heldAt); waited < held {
			t.Fatalf("the write landed %s after the connection was taken, want after the %s it was held: it did not queue, so this proves nothing", waited, held)
		}
		if got := stored(t); got != supervise.StateQueued {
			t.Fatalf("the claim reads %s, want the queued write's %s", got, supervise.StateQueued)
		}
		if err := <-ended; err != nil {
			t.Fatalf("the transaction that held the connection: %v", err)
		}
	})

	t.Run("the bound after the connection still cuts a slow write", func(t *testing.T) {
		bounded, cancel := BoundAfterAcquire(ForConcurrentWorker(ctx), 10*time.Second, write)
		defer cancel()
		started := time.Now()
		err := Transact(bounded, one.Pool(), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "select pg_sleep($1)", held.Seconds())
			return err
		})
		if err == nil {
			t.Fatalf("a write that holds its connection %s under a %s bound answered nil, want it cut", held, write)
		}
		if took := time.Since(started); took > held {
			t.Fatalf("the slow write was cut after %s, want at about its %s bound, before the database's %s", took, write, held)
		}
	})

	t.Run("the acquire bound ends a wait nobody frees", func(t *testing.T) {
		ended, _ := holdTheConnection(t)
		bounded, cancel := BoundAfterAcquire(ForConcurrentWorker(ctx), 300*time.Millisecond, write)
		defer cancel()
		queued := c
		queued.State = supervise.StateIdle
		if err := one.PutClaim(bounded, queued); !errors.Is(err, errAcquireBound) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("a write that waited its whole 300ms for the pool's one connection = %v, want the acquire bound's error naming the deadline", err)
		}
		if err := <-ended; err != nil {
			t.Fatalf("the transaction that held the connection: %v", err)
		}
	})
}
