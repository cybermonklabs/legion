package pgmigrate

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/testpg"
	"github.com/sjawhar/envoy/internal/testwait"
)

// contextCutRig is a runner whose migration waits on a lock a holder takes first, and an observer
// that reads, on a connection of its own, what the runner and the lock watch do. Each wait is for a
// fact the observer reads, through testwait, so a fact that never comes fails the test naming it.
type contextCutRig struct {
	runner, holderConn, observer *pgx.Conn
	holder                       pgx.Tx
}

func newContextCutRig(t *testing.T) *contextCutRig {
	t.Helper()
	ctx := context.Background()
	url := testpg.URL(t)
	rig := &contextCutRig{runner: connect(t, ctx, url), holderConn: connect(t, ctx, url), observer: connect(t, ctx, url)}
	if _, err := rig.runner.Exec(ctx, "create table things (kind text)"); err != nil {
		t.Fatal(err)
	}
	holder, err := rig.holderConn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Rollback(context.Background()) })
	if _, err := holder.Exec(ctx, "lock table things in access share mode"); err != nil {
		t.Fatal(err)
	}
	rig.holder = holder
	return rig
}

// contextCutReading is one answer of the observer's query: whether the fact holds, and the
// database's clock at that reading.
type contextCutReading struct {
	Holds bool
	At    time.Time
}

// until reads query, which answers one boolean and one time, until the boolean is true, and
// answers the time.
func (r *contextCutRig) until(t *testing.T, what, query string, args ...any) time.Time {
	t.Helper()
	return testwait.Until(t, what, func() (contextCutReading, bool) {
		var reading contextCutReading
		if err := r.observer.QueryRow(context.Background(), query, args...).Scan(&reading.Holds, &reading.At); err != nil {
			t.Fatalf("read whether %s: %v", what, err)
		}
		return reading, reading.Holds
	}).At
}

// runnerWaits waits until the runner's backend waits on a lock it was not granted, and answers the
// database's clock at that reading.
func (r *contextCutRig) runnerWaits(t *testing.T) time.Time {
	t.Helper()
	return r.until(t, "the runner waits on the holder's lock",
		`select exists(select 1 from pg_locks where pid = $1 and not granted), clock_timestamp()`,
		r.runner.PgConn().PID())
}

// watchReadSince waits until the lock watch has finished a reading it started at or after since:
// a backend of this database, other than the observer, idle after the watch's query
// (lockWaitQuery, which alone calls pg_blocking_pids on w.pid).
func (r *contextCutRig) watchReadSince(t *testing.T, since time.Time) {
	t.Helper()
	r.until(t, "the lock watch reads the runner again",
		`select exists(select 1 from pg_stat_activity
			where datname = current_database() and pid <> pg_backend_pid() and state = 'idle'
				and query like '%pg_blocking_pids(w.pid)%' and query_start >= $1), clock_timestamp()`,
		since)
}

// watchReads has the lock watch, whose Locking runs on fake, take its next reading, and waits
// until it has finished that reading, started at or after since. The watch reads only at a tick
// the test moves the fake to, and its ticks are interval apart from the fake's moment, which only
// this moves: it moves the fake to the next once the watch has armed it, which the watch does only
// once the reading before it is done (clock.Ticker).
func (r *contextCutRig) watchReads(t *testing.T, fake *clocktest.Fake, interval time.Duration, since time.Time) {
	t.Helper()
	fake.AwaitArmedAt(t, "the lock watch's next tick", fake.Now().Add(interval))
	fake.Advance(interval)
	r.watchReadSince(t, since)
}

// A context that ends a migration after its lock wait ended, while the migration runs a statement
// that waits on no lock, cut no lock wait: Exec returns the context's error as it returns any
// other error, and no *LockTimeoutError. One would name the lock the migration already holds and
// a holder that has left, and tell whoever reads the boot's log to end that holder. The lock
// watch keeps the wait it read after the migration got past it (watch.observe), so the wait it
// answers says nothing of whether the migration still waited when the context ended.
//
// The runner applies the migration through a Locking at production's Timeout on a fake clock, so
// the watch reads only where the test moves the fake to its tick: once while the runner waits on
// the holder's lock, which the watch records, and once after the runner holds it and sleeps,
// which reads no wait and keeps that record. The first reading must finish inside the 5 s the
// runner waits before Postgres cancels it: the observer's read of pg_locks, the move of the fake
// and the watch's one query, each a few milliseconds on a database of the test's own.
func TestACancelAfterTheLockWaitEndedIsNoLockTimeout(t *testing.T) {
	t.Parallel()
	rig := newContextCutRig(t)
	fake := clocktest.New(time.Unix(0, 0).UTC())
	locking := Locking{Clock: fake}
	cut, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- pgx.BeginFunc(cut, rig.runner, func(tx pgx.Tx) error {
			return locking.Exec(cut, tx, Migration{
				Name: "0001_added.up.sql",
				SQL:  "alter table things add column added int; select pg_sleep(60)",
			}, nil)
		})
	}()
	rig.watchReads(t, fake, locking.watchInterval(), rig.runnerWaits(t))
	if err := rig.holder.Rollback(context.Background()); err != nil {
		t.Fatalf("end the holder: %v", err)
	}
	sleeping := rig.until(t, "the runner holds the lock and sleeps",
		`select coalesce(bool_or(wait_event = 'PgSleep'), false), clock_timestamp() from pg_stat_activity where pid = $1`,
		rig.runner.PgConn().PID())
	rig.watchReads(t, fake, locking.watchInterval(), sleeping)
	cancel()
	err := testwait.Receive(t, "Exec to return once its context is cancelled", done)
	var named *LockTimeoutError
	if errors.As(err, &named) {
		t.Errorf("err = %v\nwant no *LockTimeoutError: the migration held its lock and waited on none when the context ended, yet the error names the wait on %+v", err, named.Wait)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want errors.Is(context.Canceled) to hold", err)
	}
}
