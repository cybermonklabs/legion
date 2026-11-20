package storetest

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/fataltest"
)

// HoldOn waits for its lock at most its bound and then fails naming the lock's holder, rather
// than waiting until go test's own timeout: here another transaction of the same database holds
// the lock and never lets go. The control's holder lets go well inside the bound, and HoldOn
// takes the lock.
func TestHoldOnFailsNamingTheHolderOfALockThatNeverFrees(t *testing.T) {
	const within = 2 * time.Second
	lock := AdvisoryLock(4815162342)

	t.Run("a holder that never lets go", func(t *testing.T) {
		database := Open(t)
		holder := Hold(t, database, lock, `select 'the holder that never lets go'`)
		conn := connect(t, database)
		started := time.Now()
		failure := fataltest.Failure(t, func(t testing.TB) { holdOn(t, conn, lock, within, "") })
		waited := time.Since(started)
		if failure == "" {
			t.Fatalf("HoldOn took %s while backend %d of its database held it", lock, holder)
		}
		for _, want := range []string{"waiting at most " + within.String(), fmt.Sprintf("backend %d,", holder), "the holder that never lets go"} {
			if !strings.Contains(failure, want) {
				t.Fatalf("HoldOn failed with %q, want %q in it", failure, want)
			}
		}
		if waited < within {
			t.Fatalf("HoldOn failed after %s, before its bound of %s: %s", waited, within, failure)
		}
		t.Logf("HoldOn failed after %s: %s", waited, failure)
	})

	t.Run("control: a holder that lets go in time", func(t *testing.T) {
		database := Open(t)
		holder := connect(t, database)
		HoldOn(t, holder, lock, "")
		conn := connect(t, database)
		let := make(chan struct{})
		time.AfterFunc(within/4, func() {
			defer close(let)
			_ = holder.Close(context.Background())
		})
		failure := fataltest.Failure(t, func(t testing.TB) { holdOn(t, conn, lock, within, "") })
		<-let
		if failure != "" {
			t.Fatalf("HoldOn failed for a lock its holder let go a quarter of its bound in: %s", failure)
		}
	})
}

// HoldOn, the function tests call, waits holdWait for its lock and no longer: on a lock another
// transaction of its database never frees it fails naming that bound, no sooner than holdWait and
// before its failure's own read of the holders (answerWait) has run past it. The test above runs
// the same wait with a bound of its own (holdOn); this one holds HoldOn to holdWait itself.
func TestHoldOnWaitsHoldWaitAndNoLonger(t *testing.T) {
	lock := AdvisoryLock(4815162343)
	database := Open(t)
	holder := Hold(t, database, lock, `select 'the holder that never lets go'`)
	conn := connect(t, database)
	started := time.Now()
	recorder, done := fataltest.Start(t, func(t testing.TB) { HoldOn(t, conn, lock, "") })
	select {
	case <-done:
	case <-time.After(holdWait + answerWait):
		t.Fatalf("HoldOn still waits for %s %s after it began, past its bound of %s and the %s its failure takes to read the holders", lock, time.Since(started).Round(time.Millisecond), holdWait, answerWait)
	}
	waited := time.Since(started)
	failure := recorder.Message()
	if failure == "" {
		t.Fatalf("HoldOn took %s while backend %d of its database held it", lock, holder)
	}
	if want := "waiting at most " + holdWait.String(); !strings.Contains(failure, want) {
		t.Fatalf("HoldOn failed with %q, want %q in it", failure, want)
	}
	if waited < holdWait {
		t.Fatalf("HoldOn failed after %s, before its bound of %s: %s", waited, holdWait, failure)
	}
	t.Logf("HoldOn failed after %s: %s", waited.Round(time.Millisecond), failure)
}

// connectingWith is a store on database's database whose every new connection runs hook once it
// is up, a connection of its pool among them: the connections ConnectInListingOrder opens copy
// that pool's configuration. The pool opens one connection before it answers, so a hook armed
// after it does not act on the connection the pool's own reads take.
func connectingWith(t *testing.T, database *store.Store, hook func(context.Context, *pgconn.PgConn) error) *store.Store {
	t.Helper()
	config := database.Pool.Config()
	config.ConnConfig.AfterConnect = hook
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatalf("open a pool on the database: %v", err)
	}
	t.Cleanup(pool.Close)
	hooked := &store.Store{Pool: store.NewPool(pool)}
	if _, err := hooked.Pool.Exec(t.Context(), `select 1`); err != nil {
		t.Fatalf("open the pool's connection: %v", err)
	}
	return hooked
}

// ConnectInListingOrder refuses a connection whose backend has gone from the server's listing,
// naming that backend: the connection is dead, and no order holds on it. Here every connection it
// opens to the first database has its backend ended before the connection is handed to it, as a
// server that ends backends under a test does: pg_terminate_backend with a timeout, from a
// connection of the test's own, answers only once that backend has exited, so the backend is gone
// from the listing before ConnectInListingOrder reads it, however long the server takes to end it.
func TestConnectInListingOrderRefusesAConnectionWhoseBackendIsGone(t *testing.T) {
	var ending atomic.Bool
	var mu sync.Mutex
	var ended []uint32
	database := Open(t)
	first := connectingWith(t, database, func(ctx context.Context, conn *pgconn.PgConn) error {
		if !ending.Load() {
			return nil
		}
		pid := conn.PID()
		var gone bool
		if err := database.Pool.QueryRow(ctx, `select pg_terminate_backend($1, $2)`, int32(pid), answerWait.Milliseconds()).Scan(&gone); err != nil {
			return fmt.Errorf("end backend %d, waiting at most %s for it to exit: %w", pid, answerWait, err)
		}
		if !gone {
			return fmt.Errorf("backend %d had not exited %s after it was told to end", pid, answerWait)
		}
		mu.Lock()
		ended = append(ended, pid)
		mu.Unlock()
		return nil
	})
	second := Open(t)
	ending.Store(true)

	failure := fataltest.Failure(t, func(t testing.TB) { ConnectInListingOrder(t, first, second) })
	if failure == "" {
		t.Fatal("ConnectInListingOrder answered connections to a database whose backends end as they open")
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(failure, "is gone from the server's listing") {
		t.Fatalf("ConnectInListingOrder failed with %q, want the gone backend named", failure)
	}
	for _, pid := range ended {
		if strings.Contains(failure, fmt.Sprintf("backend %d,", pid)) {
			t.Logf("ConnectInListingOrder refused the connection: %s", failure)
			return
		}
	}
	t.Fatalf("ConnectInListingOrder failed with %q, which names none of the %d backends that ended: %v", failure, len(ended), ended)
}
