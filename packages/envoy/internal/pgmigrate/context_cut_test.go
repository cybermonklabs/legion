package pgmigrate

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/testpg"
)

// contextCutRig is a runner whose migration waits on a lock a holder takes first, and an observer
// that reads, on a connection of its own, what the runner and the lock watch do. Each wait is for a
// fact the observer reads, bounded so that a fact that never comes fails the test rather than hang.
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

// until reads query, which answers one boolean and one time, every 10 ms until the boolean is
// true, and answers the time. It fails t, naming what, after 30 s.
func (r *contextCutRig) until(t *testing.T, what, query string, args ...any) time.Time {
	t.Helper()
	ctx := context.Background()
	end := time.Now().Add(30 * time.Second)
	for {
		var done bool
		var at time.Time
		if err := r.observer.QueryRow(ctx, query, args...).Scan(&done, &at); err != nil {
			t.Fatalf("read whether %s: %v", what, err)
		}
		if done {
			return at
		}
		if time.Now().After(end) {
			t.Fatalf("waited 30 s for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
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

// A context that ends a migration after its lock wait ended, while the migration runs a statement
// that waits on no lock, cut no lock wait: Exec returns the context's error as it returns any
// other error, and no *LockTimeoutError. One would name the lock the migration already holds and
// a holder that has left, and tell whoever reads the boot's log to end that holder. The lock
// watch keeps the wait it read after the migration got past it (watch.observe), so the wait it
// answers says nothing of whether the migration still waited when the context ended.
func TestACancelAfterTheLockWaitEndedIsNoLockTimeout(t *testing.T) {
	t.Parallel()
	rig := newContextCutRig(t)
	cut, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- pgx.BeginFunc(cut, rig.runner, func(tx pgx.Tx) error {
			return Exec(cut, tx, Migration{
				Name: "0001_added.up.sql",
				SQL:  "alter table things add column added int; select pg_sleep(60)",
			}, nil)
		})
	}()
	rig.watchReadSince(t, rig.runnerWaits(t))
	if err := rig.holder.Rollback(context.Background()); err != nil {
		t.Fatalf("end the holder: %v", err)
	}
	sleeping := rig.until(t, "the runner holds the lock and sleeps",
		`select coalesce(bool_or(wait_event = 'PgSleep'), false), clock_timestamp() from pg_stat_activity where pid = $1`,
		rig.runner.PgConn().PID())
	rig.watchReadSince(t, sleeping)
	cancel()
	err := <-done
	var named *LockTimeoutError
	if errors.As(err, &named) {
		t.Errorf("err = %v\nwant no *LockTimeoutError: the migration held its lock and waited on none when the context ended, yet the error names the wait on %+v", err, named.Wait)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want errors.Is(context.Canceled) to hold", err)
	}
}
