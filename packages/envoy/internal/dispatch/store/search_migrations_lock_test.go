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

// heldWhileWaiting is one reading of pg_locks: the backend queued for a lock, and whether it held,
// at that same reading, the ACCESS EXCLUSIVE lock of another table.
type heldWhileWaiting struct {
	PID   uint32
	Holds bool
}

// runnerHoldsWhileItWaits waits until a backend of store's database is queued, not yet granted, for
// mode on waited, and answers whether that backend held ACCESS EXCLUSIVE on read, the one lock a
// plain read of read waits behind, at that same reading. The query reads pg_locks once (a
// materialized with query), and pg_locks reads the whole lock table at once, so the answer is what
// the backend held while it waited. Its one bound is the test binary's deadline less testwait's
// margin (testwait.Until).
func runnerHoldsWhileItWaits(t *testing.T, store *Store, waited, mode, read string) heldWhileWaiting {
	t.Helper()
	ctx := context.Background()
	return testwait.Until(t, fmt.Sprintf("a session to queue for %s's %s", waited, mode), func() (heldWhileWaiting, bool) {
		var reading heldWhileWaiting
		err := store.Pool.QueryRow(ctx, `
			with locks as materialized (select * from pg_locks)
			select w.pid, exists(select 1 from locks h where h.pid = w.pid and h.granted
				and h.relation = $3::regclass and h.mode = 'AccessExclusiveLock')
			from locks w where w.relation = $1::regclass and w.mode = $2 and not w.granted
				and w.database = (select oid from pg_database where datname = current_database())
			limit 1
		`, waited, mode, read).Scan(&reading.PID, &reading.Holds)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return reading, false
		case err != nil:
			t.Fatalf("read pg_locks: %v", err)
		}
		return reading, true
	})
}

// readWhileTheRunnerWaits reads read and the newest version schema_migrations records, as a
// request would while the runner waits on another table, and answers the version.
func readWhileTheRunnerWaits(t *testing.T, store *Store, read string) int {
	t.Helper()
	ctx := context.Background()
	var version, rows int
	err := func() error {
		tx, err := store.Pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		if err := tx.QueryRow(ctx, "select count(*) from "+read).Scan(&rows); err != nil {
			return err
		}
		return tx.QueryRow(ctx, "select max(version) from schema_migrations").Scan(&version)
	}()
	if err != nil {
		t.Fatalf("while the runner waited, read %s and schema_migrations: %v", read, err)
	}
	return version
}

// The search migrations take one table's ACCESS EXCLUSIVE lock at a time, each in its own
// transaction: while a transaction holds `messages` (the last table, 0061), the runner applies
// 0056-0060 and commits each, gives up on 0061 at its lock timeout, and meanwhile a read of
// `issues` - whose lock a single-file migration would still be holding - answers at once. Once the
// holder ends, the runner finishes. This is what dispatch_rollout_rehearsal.sh, which holds one
// table at a time, cannot see across tables.
//
// The store runs under production's Timeout on a fake clock. While the runner waits for messages,
// one reading of pg_locks shows that its backend holds no lock a read of issues waits behind
// (runnerHoldsWhileItWaits). That reading must land inside the runner's wait, so the Timeout is
// production's, which no slowed step outlasts
// (dispatch://INFRA-3149/comment/44861a24-bbec-4ee1-8530-54bf97c7427e). The runner's pauses between
// its attempts run on the fake clock, which the test moves past each (movePastThePause), and its
// give-up is the error it returns after the last.
func TestSearchMigrationsLockOneTableAtATime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openMigratedTestStore(t, 55)
	fake := underLocking(store, 0)
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

	// Wait until the runner is queued on messages, and read, in that reading, what it holds on issues.
	if waiting := runnerHoldsWhileItWaits(t, store, "messages", "AccessExclusiveLock", "issues"); waiting.Holds {
		t.Fatalf("while the runner (pid %d) waited for messages, it held issues' ACCESS EXCLUSIVE lock, which a read of issues waits behind", waiting.PID)
	}
	if version := readWhileTheRunnerWaits(t, store, "issues"); version != 60 {
		t.Errorf("while the runner waited for messages, schema_migrations was at %d, want 60 (0056-0060 committed one by one)", version)
	}

	for attempt := 1; attempt < pgmigrate.LockTimeoutAttempts; attempt++ {
		movePastThePause(t, fake, attempt)
	}
	err = testwait.Receive(t, "the runner to give up on 0061 after its last attempt", migrated)
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
	var version int
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
