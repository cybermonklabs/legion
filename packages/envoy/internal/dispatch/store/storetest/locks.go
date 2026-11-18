package storetest

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// CountLockWaits counts the sessions of database waiting on a lock whose statement matches the LIKE
// pattern like. It reads through the pool, never through a transaction: a transaction's first read
// of pg_stat_activity freezes the view for the rest of it, and a poll inside one spins until it
// times out. It returns its error rather than failing the test, so a hook running off the test's
// goroutine, such as a settlement's, can report it and let the code under test go on.
func CountLockWaits(ctx context.Context, database *store.Store, like string) (int, error) {
	var waiting int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from pg_stat_activity
		where datname = current_database() and wait_event_type = 'Lock' and query like $1
	`, like).Scan(&waiting); err != nil {
		return 0, fmt.Errorf("inspect database locks: %w", err)
	}
	return waiting, nil
}

// waitingBehindHolder is the recursive CTE naming, as waiting(pid), every backend queued behind a
// lock the current transaction owns, directly or behind an earlier waiter.
const waitingBehindHolder = `
	with recursive waiting(pid) as (
		select pid from pg_locks
		where not granted and pg_backend_pid() = any(pg_blocking_pids(pid))
		union
		select blocked.pid from pg_locks blocked, waiting
		where not blocked.granted and waiting.pid = any(pg_blocking_pids(blocked.pid))
	)
`

// LockWaiters counts the backends queued behind a lock holder's transaction owns, directly or
// behind an earlier waiter (a second row-lock waiter is blocked by the first, which holds the
// tuple lock), at the moment it reads. It reads through holder's own connection: the code under
// test can drain the shared pool while it waits on that lock, and a read that needed a pool
// connection of its own would deadlock with it once the pool is exhausted. It reads pg_locks, not
// pg_stat_activity, whose view is frozen for the rest of a transaction once read. It counts every
// backend behind the holder, a background document settlement's among them; a test that must know
// the call under test is the one waiting watches that call's backend instead (QueuedBehind).
func LockWaiters(t testing.TB, holder pgx.Tx) int {
	t.Helper()
	var count int
	if err := holder.QueryRow(context.Background(), waitingBehindHolder+`select count(*) from waiting`).Scan(&count); err != nil {
		t.Fatalf("inspect database locks: %v", err)
	}
	return count
}

// WaitForLockWaiters waits until want backends queue behind a lock holder's transaction owns, as
// LockWaiters counts them, and returns how many it saw then. It fails the test, naming the last
// count, when they never do.
func WaitForLockWaiters(t testing.TB, holder pgx.Tx, want int) int {
	t.Helper()
	return testwait.Until(t, fmt.Sprintf("%d backends to queue behind the lock holder's transaction owns", want), func() (int, bool) {
		count := LockWaiters(t, holder)
		return count, count >= want
	})
}

// QueuedBehind reports whether the backend pid waits on a lock holder's transaction owns,
// directly or behind an earlier waiter, as LockWaiters counts them, and reads through
// holder's own connection for the same reason. Watching one backend rather than counting them is
// what tells the call under test from any other session queued on the same lock: an issue's
// document settlement locks the issue's row too (docs/solutions/testing/a-lock-wait-test-watches-
// the-pid-of-the-call-under-test-and-its-mutant-fires-only-on-that-call-INFRA-260.md).
func QueuedBehind(ctx context.Context, holder pgx.Tx, pid uint32) (bool, error) {
	var queued bool
	if err := holder.QueryRow(ctx, waitingBehindHolder+`select exists (select 1 from waiting where pid = $1)`, pid).Scan(&queued); err != nil {
		return false, fmt.Errorf("inspect database locks: %w", err)
	}
	return queued, nil
}

// WaitForQueuedBehind waits until the backend pid, the one connection a call under test runs on,
// queues behind a lock holder's transaction owns (QueuedBehind), polling through holder's own
// connection. It returns the call's answer and true if the call answers on answered before its
// backend has queued: such a call never waited on the lock, and the caller fails with what it
// answered. It fails the test if neither ever happens.
func WaitForQueuedBehind[T any](t testing.TB, holder pgx.Tx, pid uint32, answered <-chan T) (T, bool) {
	t.Helper()
	type outcome struct {
		answer   T
		answered bool
	}
	ctx := context.Background()
	got := testwait.Until(t, fmt.Sprintf("backend %d to queue behind the lock, or its call to answer", pid), func() (outcome, bool) {
		select {
		case answer := <-answered:
			return outcome{answer: answer, answered: true}, true
		default:
		}
		queued, err := QueuedBehind(ctx, holder, pid)
		if err != nil {
			t.Fatal(err)
		}
		return outcome{}, queued
	})
	return got.answer, got.answered
}

// WaitForStatementBehind waits until a session of database whose current statement is exactly
// statement queues behind a lock holder's transaction owns (QueuedBehind), and returns that
// session's backend process id. It is for a call under test that runs in another process, whose
// connection a test cannot give a pool of its own: the statement names the call's backend, and a
// session queued behind the same lock with any other statement is never taken for it. It reads
// pg_stat_activity through the pool, a fresh view on each read. It fails the test, naming the
// sessions running statement it last saw, when none ever queues.
func WaitForStatementBehind(t testing.TB, database *store.Store, holder pgx.Tx, statement string) uint32 {
	t.Helper()
	ctx := context.Background()
	type reading struct {
		Running []uint32
		Queued  uint32
	}
	return testwait.Until(t, fmt.Sprintf("a session running %q to queue behind the lock", statement), func() (reading, bool) {
		rows, err := database.Pool.Query(ctx, `
			select pid from pg_stat_activity where datname = current_database() and query = $1
		`, statement)
		if err != nil {
			t.Fatalf("read the sessions running %q: %v", statement, err)
		}
		pids, err := pgx.CollectRows(rows, pgx.RowTo[uint32])
		if err != nil {
			t.Fatalf("read the sessions running %q: %v", statement, err)
		}
		for _, pid := range pids {
			queued, err := QueuedBehind(ctx, holder, pid)
			if err != nil {
				t.Fatal(err)
			}
			if queued {
				return reading{Running: pids, Queued: pid}, true
			}
		}
		return reading{Running: pids}, false
	}).Queued
}
