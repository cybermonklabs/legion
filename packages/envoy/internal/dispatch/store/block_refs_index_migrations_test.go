package store

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/pgmigrate"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The two migrations of block_refs' index of targets: blockRefsSourceIndex builds
// block_refs_to_source on (to_kind, to_id, ((artifact_id)::text)), and dropBlockRefsTargetIndex
// drops block_refs_to on (to_kind, to_id), the index it replaces. beforeBlockRefsSourceIndex is a
// version before them, not the one just before: main's 20261120021308_search_text_versions sits
// between it and blockRefsSourceIndex, so a census from it reads that migration too, and each test
// reads only the entry of the migration it names.
const (
	beforeBlockRefsSourceIndex = 20261118020802
	blockRefsSourceIndex       = 20261120222732
	dropBlockRefsTargetIndex   = 20261120222733
)

// blockRefsDropBound is the drop's own lock_timeout, which dropBlockRefsTargetIndex's file sets
// with set local before its drop index.
const blockRefsDropBound = 120 * time.Second

// blockRefsDropWait is a reading of the drop's wait for block_refs: the waiting backend, the moment
// its wait began, and how long it had waited, both on the database's clock.
type blockRefsDropWait struct {
	PID    uint32
	Since  *time.Time
	Waited time.Duration
}

// The drop of block_refs_to waits out a reader of block_refs that outlasts the runner's attempts.
// drop index takes ACCESS EXCLUSIVE on block_refs, which waits for every reader, and a read the
// stopped pod left can still run when the boot's drop asks for that lock. Under the runner's own
// bound, its lock_timeout for each of pgmigrate.LockTimeoutAttempts (3) with
// pgmigrate.LockTimeoutRetryPause (2 s) between, about 19 s at production's 5 s, the runner gives
// up and the boot exits; the migration's own set local lock_timeout of 120 s is what lets it wait
// the reader out.
//
// The store runs under shortLockTimeout on a fake clock, and the reader holds ACCESS SHARE on
// block_refs, on a connection of its own, until the drop has waited for its lock past the runner's
// lock_timeout, read on the database's clock (pg_locks' waitstart against clock_timestamp()), with
// no pause armed on the store's clock: the runner arms one only once an attempt has given up, and
// the test never moves the clock past it, so the runner makes no attempt after it, and a hold past
// the first lock_timeout is a hold past all of the runner's attempts. A drop with no bound of its
// own is cancelled at the runner's lock_timeout, and the runner pauses: the test fails at once,
// naming the pause, or, where its reading fell between the server's timer and the cancel, Migrate
// never returns from that pause and the test fails where testwait gives up, naming the wait.
//
// Migrate runs on context.Background(), as the boot runs it on a context with no deadline. A
// deadline in the runner that ended the wait would fail this test through Migrate's error: one on
// the store's clock, on which the runner's timers run (pgmigrate.Locking), ends once the test moves
// that clock past the drop's own 120 s while the drop waits, which it does before the reader
// commits. That nothing on the wall clock ends the wait is read from the code. The boot's context
// is the one main (cmd/dispatch/main.go) makes with signal.NotifyContext, which carries no
// deadline, and main hands it to database.Migrate(ctx) with none added. Migrate and migrate
// (store.go) add none. The one context.WithTimeout in store.go, closeBounded's, bounds only the
// close of a connection, and so does pgmigrate's one bound on the wall clock, on the close of the
// lock watch's own connection. Its other bounds run on the store's clock: a diagnosis of the
// holders after a lock timeout (diagnoseHolders) and the rollback and close after it, and the
// restore of a non-transactional migration's session settings, which the drop, a transactional
// migration, does not run.
func TestTheBlockRefsTargetIndexDropWaitsOutAReaderPastTheRunnersAttempts(t *testing.T) {
	t.Parallel()
	if shortLockTimeout >= blockRefsDropBound {
		t.Fatalf("the runner's lock_timeout of %s does not end before the drop's own %s, so a drop with no bound of its own would pass", shortLockTimeout, blockRefsDropBound)
	}
	ctx := context.Background()
	store := openMigratedTestStore(t, blockRefsSourceIndex)
	fake := underLocking(store, shortLockTimeout)
	reader, err := pgx.Connect(ctx, store.Pool.Config().ConnString())
	if err != nil {
		t.Fatalf("connect the reader: %v", err)
	}
	t.Cleanup(func() { reader.Close(context.Background()) })
	read, err := reader.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the reader's transaction: %v", err)
	}
	t.Cleanup(func() { _ = read.Rollback(context.Background()) })
	if _, err := read.Exec(ctx, "select 1 from block_refs limit 1"); err != nil {
		t.Fatalf("read block_refs: %v", err)
	}

	migrated := make(chan error, 1)
	go func() { migrated <- store.Migrate(context.Background()) }()
	// pausing reports whether pending holds the runner's pause after an attempt that gave up, armed
	// at the fake's moment now: no other timer of the runner's is armed that far ahead.
	pausing := func(now time.Time, pending []time.Time) bool {
		return slices.ContainsFunc(pending, now.Add(pgmigrate.LockTimeoutRetryPause).Equal)
	}
	waited := testwait.Until(t, "the drop of block_refs_to to wait for block_refs past the runner's lock_timeout", func() (blockRefsDropWait, bool) {
		if pausing(fake.Now(), fake.Armed()) {
			t.Fatalf("the runner armed its pause after an attempt that gave up at its own lock_timeout of %s: the drop waited for the reader under the runner's bound, not its own", shortLockTimeout)
		}
		var wait blockRefsDropWait
		err := store.Pool.QueryRow(ctx, `select pid, waitstart, coalesce(clock_timestamp() - waitstart, interval '0') from pg_locks
			where relation = 'block_refs'::regclass and mode = 'AccessExclusiveLock' and not granted
				and database = (select oid from pg_database where datname = current_database())`).Scan(&wait.PID, &wait.Since, &wait.Waited)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return wait, false
		case err != nil:
			t.Fatalf("read the drop's wait for block_refs: %v", err)
		}
		return wait, wait.Waited > shortLockTimeout
	})
	// A timer of the runner's own on the store's clock, a deadline around the drop among them, ends
	// in this move, before the reader commits. The move is made only where no pause is armed, under
	// the one hold of the fake's lock it begins with, so it fires none: a pause the runner arms while
	// it runs is armed past it.
	if armed, moved := fake.SetWhen(func(now time.Time, pending []time.Time) (time.Time, bool) {
		return now.Add(blockRefsDropBound), !pausing(now, pending)
	}); !moved {
		t.Fatalf("the runner armed its pause after an attempt that gave up at its own lock_timeout of %s (armed: %v): the drop waited for the reader under the runner's bound, not its own", shortLockTimeout, armed)
	}
	select {
	case err := <-migrated:
		t.Fatalf("store.Migrate returned %v while the reader still held block_refs, after the drop (pid %d) had waited %s for it: the drop did not wait for the reader", err, waited.PID, waited.Waited)
	default:
	}
	if err := read.Commit(ctx); err != nil {
		t.Fatalf("commit the reader: %v", err)
	}
	if err := testwait.Receive(t, "store.Migrate to return once the reader committed", migrated); err != nil {
		t.Fatalf("store.Migrate with a reader of block_refs held past the runner's lock_timeout of %s, while its clock passed the drop's own %s: %v", shortLockTimeout, blockRefsDropBound, err)
	}
	assertDatabaseObjectsAbsent(t, ctx, store.Pool, "select indexname from pg_indexes where schemaname = current_schema()", []string{"block_refs_to"})
	var recorded bool
	if err := store.Pool.QueryRow(ctx, "select exists (select from schema_migrations where version = $1)", dropBlockRefsTargetIndex).Scan(&recorded); err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	if !recorded {
		t.Errorf("schema_migrations does not record %d, the drop's version", dropBlockRefsTargetIndex)
	}
}
