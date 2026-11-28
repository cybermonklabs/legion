package store

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/testwait"
)

// beginTitlesWrite opens a transaction on a connection of store's pool, rolled back when the test
// ends unless it has committed, and answers it with its backend's pid.
func beginTitlesWrite(t *testing.T, store *Store, what string) (pgx.Tx, int32) {
	t.Helper()
	ctx := context.Background()
	tx, err := store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin %s: %v", what, err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	var pid int32
	if err := tx.QueryRow(ctx, `select pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("read the backend of %s: %v", what, err)
	}
	return tx, pid
}

// waitsForALock reports whether backend pid, connected to the test's own database, waits for a
// lock. The read names that database because the package's tests run beside each other on one
// server.
func waitsForALock(t *testing.T, store *Store, pid int32) bool {
	t.Helper()
	var waits bool
	if err := store.Pool.QueryRow(context.Background(), `
		select exists(select 1 from pg_locks l join pg_stat_activity a on a.pid = l.pid
		              where l.pid = $1 and not l.granted and a.datname = current_database())
	`, pid).Scan(&waits); err != nil {
		t.Fatalf("read whether backend %d waits for a lock: %v", pid, err)
	}
	return waits
}

// Two writes of one session that take their locks in opposite orders both commit. The first adds
// a titled event and then locks the row of another issue; the second holds that row and then adds
// a titled event; both transactions are open at once, and the first waits for the row while the
// second commits. The trigger on events (migration 20261117100015) upserts the session's row of
// session_titles at the commit, after every lock the transaction's statements took, so neither
// write holds the title row while it waits for another lock. An immediate trigger would take the
// title row at the insert: the first write would hold it while it waited for the issue's row, the
// second would hold the issue's row while it waited for the title row, and Postgres would refuse
// one of them with `deadlock detected`. Two parallel writes of one agent are ordinary. The stored
// title is the one the read of events answers: the second write's, whose event is the later.
func TestTwoTitledWritesOfOneSessionInOppositeLockOrdersBothCommit(t *testing.T) {
	t.Parallel()
	store := migratedTitlesStore(t)
	// The writes lock an issue their events are not on: an event's insert takes its own issue's
	// row FOR KEY SHARE through the foreign key, which the second write's FOR UPDATE would wait on.
	const lockedIssue = "TITLES-2"
	if _, err := store.Pool.Exec(context.Background(), `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ($1, 'TITLES', 2, 'Locked', '{"kind":"user","id":"alice"}', 'b')
	`, lockedIssue); err != nil {
		t.Fatalf("seed the issue the writes lock: %v", err)
	}
	const session = "ses_two_writes"
	firstAt := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	first, firstPid := beginTitlesWrite(t, store, "the first write")
	second, secondPid := beginTitlesWrite(t, store, "the second write")
	// Every statement below runs on a context cancelled when the test ends, and every goroutine
	// has returned before the cleanups above roll the two transactions back.
	ctx, cancel := context.WithCancel(testwait.Context(t))
	var running sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		running.Wait()
	})
	insertEvent := func(tx pgx.Tx, seq int, title string, at time.Time) error {
		_, err := tx.Exec(ctx, `
			insert into events (issue_key, seq, type, actor, payload, notify, created_at)
			values ($1, $2, 'comment.created', $3::jsonb, '{}', false, $4)
		`, titlesIssue, seq, sessionActor(session, title), at)
		return err
	}
	lockIssue := func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `select 1 from issues where key = $1 for update`, lockedIssue)
		return err
	}

	if err := insertEvent(first, 1, "First write", firstAt); err != nil {
		t.Fatalf("the first write's event: %v", err)
	}
	if err := lockIssue(second); err != nil {
		t.Fatalf("the second write's lock of %s: %v", lockedIssue, err)
	}
	// Each result channel has one slot and its goroutine sends once, so a result stays in it,
	// and len shows it, until the test takes it.
	inserted := make(chan error, 1)
	running.Go(func() { inserted <- insertEvent(second, 2, "Second write", firstAt.Add(time.Minute)) })
	testwait.Until(t, "the second write's event to be stored or its backend to wait for a lock", func() (string, bool) {
		if len(inserted) == 1 || waitsForALock(t, store, secondPid) {
			return "", true
		}
		return fmt.Sprintf("backend %d waits for no lock and its insert has not returned", secondPid), false
	})
	locked := make(chan error, 1)
	running.Go(func() { locked <- lockIssue(first) })
	testwait.Until(t, "the first write's lock of "+lockedIssue+" to wait for a lock or to return", func() (string, bool) {
		if len(locked) == 1 || waitsForALock(t, store, firstPid) {
			return "", true
		}
		return fmt.Sprintf("backend %d waits for no lock and its lock has not returned", firstPid), false
	})
	if err := testwait.Receive(t, "the second write's event", inserted); err != nil {
		t.Fatalf("the second write's event: %v", err)
	}
	if err := second.Commit(ctx); err != nil {
		t.Fatalf("commit the second write: %v", err)
	}
	if err := testwait.Receive(t, "the first write's lock of "+lockedIssue, locked); err != nil {
		t.Fatalf("the first write's lock of %s: %v", lockedIssue, err)
	}
	if err := first.Commit(ctx); err != nil {
		t.Fatalf("commit the first write: %v", err)
	}

	old := eventsReadTitles(t, store)
	if old[session] != "Second write" {
		t.Fatalf("the old read names %s %q; want %q, the title of the later event", session, old[session], "Second write")
	}
	requireSameTitles(t, "session_titles, after both writes committed,", storedTitles(t, store), old)
}
