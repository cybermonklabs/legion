package storetest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The waits below end on the fact each waits for, and their one bound is the test binary's
// deadline less testwait's margin (testwait.UntilPolling): a call that a loaded machine starts or
// queues late still ends the wait, and one that never queues fails the test by name, with what was
// last read, before go test's alarm. Each must run on the test's own goroutine, since it can fail
// the test.

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

// OwnerLockWaiters counts the backends of database waiting on an owner lock (the statements
// store.LockOwner and LockOwnersShared run, which name their result owner_lock), and how many of
// them hold a transaction id while they wait: every writer takes its owner's lock in its read
// phase, before its first write, so a waiter holding a transaction id is holding one open behind
// the lock. The caller's own backend is left out. It reads through the pool, as CountLockWaits
// does and for the same reason.
func OwnerLockWaiters(t testing.TB, database *store.Store) (waiting, holdingXid int) {
	t.Helper()
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*), count(*) filter (where backend_xid is not null)
		from pg_stat_activity
		where datname = current_database() and wait_event_type = 'Lock' and wait_event = 'advisory'
		  and query like '%owner_lock%' and pid <> pg_backend_pid()
	`).Scan(&waiting, &holdingXid); err != nil {
		t.Fatalf("read pg_stat_activity: %v", err)
	}
	return waiting, holdingXid
}

// AwaitOwnerLockWaiter waits for a backend to wait on an owner lock (OwnerLockWaiters) and answers
// how many of the waiters hold a transaction id. It returns the call's answer and true when the
// call under test answers on answered before any backend waited, as WaitForQueuedBehind does: such
// a call never waited for the lock the test holds, and the caller fails with what it answered.
// what names that call, for the failure when the test binary's deadline comes first.
func AwaitOwnerLockWaiter[T any](t *testing.T, database *store.Store, what string, answered <-chan T) (T, bool, int) {
	t.Helper()
	type seen struct {
		answer              T
		early               bool
		waiting, holdingXid int
	}
	got := testwait.UntilPolling(t, 5*time.Millisecond, what+" to wait on an owner lock, or to answer (the owner-lock waiters last read)", func() (seen, bool) {
		select {
		case early := <-answered:
			return seen{answer: early, early: true}, true
		default:
		}
		waiting, holdingXid := OwnerLockWaiters(t, database)
		return seen{waiting: waiting, holdingXid: holdingXid}, waiting > 0
	})
	return got.answer, got.early, got.holdingXid
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

// WaitForLockWaiters waits for want backends to queue behind a lock holder's transaction owns,
// directly or behind an earlier waiter (a second row-lock waiter is blocked by the first, which
// holds the tuple lock), and returns how many it saw then, at least want. It polls through
// holder's own connection: the code under test can drain the shared pool while it waits on that
// lock, and a poll that needed a pool connection of its own would deadlock with it once the pool is
// exhausted. It reads pg_locks, not pg_stat_activity, whose view is frozen for the rest of a
// transaction once read. It counts every backend behind the holder, a background document
// settlement's among them; a test that must know the call under test is the one waiting watches
// that call's backend instead (QueuedBehind), or counts its pools' backends alone (LockWaitersFrom).
func WaitForLockWaiters(t *testing.T, holder pgx.Tx, want int) int {
	t.Helper()
	return testwait.UntilPolling(t, 10*time.Millisecond, fmt.Sprintf("%d backends to queue behind the held lock (the count last read)", want), func() (int, bool) {
		count, err := LockWaiters(context.Background(), holder)
		if err != nil {
			t.Fatal(err)
		}
		return count, count >= want
	})
}

// LockWaiters counts the backends queued behind a lock holder's transaction owns, as
// WaitForLockWaiters counts them, through holder's own connection. It fails no test, so a
// goroutine other than the test's can read it and hand the count to the test's goroutine.
func LockWaiters(ctx context.Context, holder pgx.Tx) (int, error) {
	var count int
	if err := holder.QueryRow(ctx, waitingBehindHolder+`select count(*) from waiting`).Scan(&count); err != nil {
		return 0, fmt.Errorf("inspect database locks: %w", err)
	}
	return count, nil
}

// QueuedBehind reports whether the backend pid waits on a lock holder's transaction owns,
// directly or behind an earlier waiter, as WaitForLockWaiters counts them, and reads through
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

// LockWaitersFrom counts the backends queued behind a lock holder's transaction owns, as
// LockWaiters counts them, whose application_name is one of applications: the backends of pools a
// test opened for the calls under test (PlannedApplication names those Planned opens). A backend
// of any other pool queued behind the same lock - a document settlement of the server's own, which
// takes an issue's owner lock exclusively and so queues among calls that hold it shared - is never
// counted, so what else the server runs cannot change the count. It reads through holder's own
// connection, as LockWaiters does, and clears that transaction's snapshot of pg_stat_activity
// first, which a transaction otherwise takes at its first read and keeps. It fails no test.
func LockWaitersFrom(ctx context.Context, holder pgx.Tx, applications ...string) (int, error) {
	if _, err := holder.Exec(ctx, `select pg_stat_clear_snapshot()`); err != nil {
		return 0, fmt.Errorf("clear the transaction's snapshot of pg_stat_activity: %w", err)
	}
	var count int
	if err := holder.QueryRow(ctx, waitingBehindHolder+`
		select count(*) from waiting join pg_stat_activity activity using (pid)
		where activity.application_name = any($1)`, applications).Scan(&count); err != nil {
		return 0, fmt.Errorf("inspect database locks: %w", err)
	}
	return count, nil
}

// WaitForLockWaitersFrom waits for want backends of the pools named applications to queue behind a
// lock holder's transaction owns (LockWaitersFrom), and returns how many it saw then, at least
// want. It is for calls whose backends a test names by their pool's application_name - a pool the
// test opened for them, or another process started with its own name - where it cannot know their
// pids in advance; a backend of any other pool never ends the wait. It runs on the test's
// goroutine, and its one bound is the test binary's deadline less testwait's margin.
func WaitForLockWaitersFrom(t *testing.T, holder pgx.Tx, want int, applications ...string) int {
	t.Helper()
	return testwait.UntilPolling(t, 10*time.Millisecond, fmt.Sprintf("%d backends of %q to queue behind the held lock (the count last read)", want, applications), func() (int, bool) {
		count, err := LockWaitersFrom(context.Background(), holder, applications...)
		if err != nil {
			t.Fatal(err)
		}
		return count, count >= want
	})
}

// WaitForEachQueuedBehind waits until every backend of pids queues behind a lock holder's
// transaction owns, directly or behind an earlier waiter (QueuedBehind): the backends of the calls
// under test, each on a connection of its own (OneConnection), so no other backend queued behind
// the same lock ends the wait. It runs on the test's goroutine, and its one bound is the test
// binary's deadline less testwait's margin, where it fails naming the backends not yet queued.
func WaitForEachQueuedBehind(t *testing.T, holder pgx.Tx, pids ...uint32) {
	t.Helper()
	testwait.UntilPolling(t, 2*time.Millisecond, fmt.Sprintf("backends %v to queue behind the held lock (those not yet queued)", pids), func() ([]uint32, bool) {
		var waiting []uint32
		for _, pid := range pids {
			queued, err := QueuedBehind(context.Background(), holder, pid)
			if err != nil {
				t.Fatal(err)
			}
			if !queued {
				waiting = append(waiting, pid)
			}
		}
		return waiting, len(waiting) == 0
	})
}

// HoldsTransactionID reports whether the backend pid holds a transaction id: the exclusive lock a
// transaction takes on its own id once it writes. A writer takes its owner's lock in its read
// phase, before its first write, so a call under test that waits on an owner lock holding one
// wrote before it locked. It names the one backend, so a waiter of any other pool never answers
// for the call. It reads pg_locks through the pool, which no transaction's snapshot freezes.
func HoldsTransactionID(ctx context.Context, database *store.Store, pid uint32) (bool, error) {
	var holds bool
	if err := database.Pool.QueryRow(ctx, `
		select exists (
			select 1 from pg_locks
			where pid = $1 and locktype = 'transactionid' and mode = 'ExclusiveLock' and granted
		)`, pid).Scan(&holds); err != nil {
		return false, fmt.Errorf("read whether backend %d holds a transaction id: %w", pid, err)
	}
	return holds, nil
}

// WaitForQueuedBehind waits for the backend pid, the one connection a call under test runs on, to
// queue behind a lock holder's transaction owns (QueuedBehind), polling through holder's own
// connection. It returns the call's answer and true if the call answers on answered before its
// backend has queued: such a call never waited on the lock, and the caller fails with what it
// answered.
func WaitForQueuedBehind[T any](t *testing.T, holder pgx.Tx, pid uint32, answered <-chan T) (T, bool) {
	t.Helper()
	type seen struct {
		answer T
		early  bool
	}
	got := testwait.UntilPolling(t, 2*time.Millisecond, fmt.Sprintf("backend %d to queue behind the lock, or its call to answer", pid), func() (seen, bool) {
		select {
		case answer := <-answered:
			return seen{answer: answer, early: true}, true
		default:
		}
		queued, err := QueuedBehind(context.Background(), holder, pid)
		if err != nil {
			t.Fatal(err)
		}
		return seen{}, queued
	})
	return got.answer, got.early
}

// WaitForStatementBehind waits for a session of database whose current statement is exactly
// statement to queue behind a lock holder's transaction owns (QueuedBehind), and returns that
// session's backend process id. It is for a call under test that runs in another process, whose
// connection a test cannot give a pool of its own: the statement names the call's backend, and a
// session queued behind the same lock with any other statement is never taken for it. It reads
// pg_stat_activity through the pool, a fresh view on each read.
func WaitForStatementBehind(t *testing.T, database *store.Store, holder pgx.Tx, statement string) uint32 {
	t.Helper()
	ctx := context.Background()
	return testwait.UntilPolling(t, 10*time.Millisecond, fmt.Sprintf("a session running %q to queue behind the held lock (the backend last found)", statement), func() (uint32, bool) {
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
				return pid, true
			}
		}
		return 0, false
	})
}
