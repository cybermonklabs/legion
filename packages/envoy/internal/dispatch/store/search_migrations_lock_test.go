package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/pgmigrate"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The search migrations take one table's ACCESS EXCLUSIVE lock at a time, each in its own
// transaction: while a transaction holds `messages` (the last table, 0061), the runner applies
// 0056-0060 and commits each, gives up on 0061 at pgmigrate.LockTimeout, and meanwhile a read of
// `issues` - whose lock a single-file migration would still be holding - answers without waiting
// on a lock. Once the holder ends, the runner finishes. This is what dispatch_rollout_rehearsal.sh,
// which holds one table at a time, cannot see across tables.
func TestSearchMigrationsLockOneTableAtATime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openMigratedTestStore(t, 55)
	holder, err := store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, "select count(*) from messages"); err != nil {
		t.Fatalf("hold messages: %v", err)
	}

	migrated := make(chan error, 1)
	go func() { migrated <- store.Migrate(ctx) }()

	// Wait until the runner is queued on messages, then read issues, which must answer without
	// waiting on a lock while it is.
	waitForQueuedLock(t, store, "messages", "AccessExclusiveLock")
	var version int
	var issues int
	requireAnsweredWithoutALockWait(t, store, "a read of issues while the runner waits for messages", func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, "select count(*) from issues").Scan(&issues); err != nil {
			return err
		}
		return tx.QueryRow(ctx, "select max(version) from schema_migrations").Scan(&version)
	})
	if version != 60 {
		t.Errorf("while the runner waited for messages, schema_migrations was at %d, want 60 (0056-0060 committed one by one)", version)
	}

	err = testwait.Receive(t, "store.Migrate to give up on 0061 at its lock timeout", migrated)
	var lockTimeout *pgmigrate.LockTimeoutError
	if err == nil || !errors.As(err, &lockTimeout) || !strings.Contains(lockTimeout.Migration, "0061_messages_search_trigger") {
		t.Fatalf("the runner should have given up on 0061 at its lock timeout, got: %v", err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("release messages: %v", err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate once the holder ended: %v", err)
	}
	if err := store.Pool.QueryRow(ctx, "select max(version) from schema_migrations").Scan(&version); err != nil {
		t.Fatalf("read version: %v", err)
	}
	migrations, err := pgmigrate.Load(migrationFiles, "migrations")
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	if last := migrations[len(migrations)-1].Version; version != last {
		t.Errorf("schema_migrations at %d after the holder ended, want %d, the highest migration", version, last)
	}
}

// requireAnsweredWithoutALockWait runs read in a transaction of its own and requires it to answer
// without ever waiting on a lock. A statement that waits for a lock shows as one in
// pg_stat_activity, on its own backend, so the verdict is that state, read while the read runs,
// and not how long the read took: a read no lock holds up answers however slow the machine is,
// and one that waits behind a lock fails as soon as it does. It returns once read has answered.
// A try that finds neither reads the reader's backend: its state, wait event and statement.
func requireAnsweredWithoutALockWait(t *testing.T, store *Store, what string, read func(context.Context, pgx.Tx) error) {
	t.Helper()
	ctx := context.Background()
	type begun struct {
		pid uint32
		err error
	}
	began := make(chan begun, 1)
	answered := make(chan error, 1)
	go func() {
		tx, err := store.Pool.Begin(ctx)
		if err != nil {
			began <- begun{err: err}
			return
		}
		defer tx.Rollback(ctx)
		began <- begun{pid: tx.Conn().PgConn().PID()}
		answered <- read(ctx, tx)
	}()
	reader := testwait.Receive(t, what+": its transaction to begin", began)
	if reader.err != nil {
		t.Fatalf("%s: begin: %v", what, reader.err)
	}
	var waited bool
	testwait.Until(t, what+" to answer, or to wait on a lock", func() (string, bool) {
		select {
		case err := <-answered:
			if err != nil {
				t.Fatalf("%s: %v", what, err)
			}
			return "answered", true
		default:
		}
		var backend string
		if err := store.Pool.QueryRow(ctx, `
			select coalesce(bool_or(wait_event_type = 'Lock'), false),
				coalesce(string_agg(format('%s (wait %s/%s): %s', state, wait_event_type, wait_event, query), ''), 'no row in pg_stat_activity')
			from pg_stat_activity where pid = $1`, int64(reader.pid)).Scan(&waited, &backend); err != nil {
			t.Fatalf("read whether %s waits on a lock: %v", what, err)
		}
		return fmt.Sprintf("not answered; backend %d is %s", reader.pid, backend), waited
	})
	if waited {
		t.Fatalf("%s waited on a lock (pg_stat_activity), want it answered without one", what)
	}
}
