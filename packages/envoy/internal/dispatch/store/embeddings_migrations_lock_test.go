package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/pgmigrate"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The embeddings trigger migrations (0073-0077) take one table's lock at a time, each in its
// own transaction, exactly as the search migrations (0056-0061) do and for the same reason:
// while a transaction holds `messages` (the last table, 0077) with a lock CREATE TRIGGER's own
// SHARE ROW EXCLUSIVE conflicts with, the runner applies 0074-0076 and commits each, gives up on
// 0077 at its lock timeout, and meanwhile a read of `issues` - whose lock a single-file
// migration would still be holding - answers at once. Once the holder ends, the runner finishes.
// The holder must take at least ROW EXCLUSIVE: a plain SELECT's ACCESS SHARE is compatible with
// CREATE TRIGGER's own lock (unlike the search migrations, which also ALTER TABLE ADD COLUMN and
// so need ACCESS EXCLUSIVE, conflicting with a reader) - a no-op UPDATE matching no row still
// takes the table-level lock its statement needs.
//
// The store runs under production's Timeout on a fake clock, as TestSearchMigrationsLockOneTableAtATime
// says why: the reading of what the runner holds while it waits must land inside that wait, and the
// runner's pauses run on the fake clock, which the test moves past each.
func TestEmbeddingsMigrationsLockOneTableAtATime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openMigratedTestStore(t, 72)
	fake := underLocking(store, 0)
	holder, err := store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, "update messages set id = id where false"); err != nil {
		t.Fatalf("hold messages: %v", err)
	}

	migrated := make(chan error, 1)
	go func() { migrated <- store.Migrate(ctx) }()

	// Wait until the runner is queued on messages, and read, in that reading, what it holds on issues.
	if waiting := runnerHoldsWhileItWaits(t, store, "messages", "ShareRowExclusiveLock", "issues"); waiting.Holds {
		t.Fatalf("while the runner (pid %d) waited for messages, it held issues' ACCESS EXCLUSIVE lock, which a read of issues waits behind", waiting.PID)
	}
	if version := readWhileTheRunnerWaits(t, store, "issues"); version != 76 {
		t.Errorf("while the runner waited for messages, schema_migrations was at %d, want 76 (0073-0076 committed one by one)", version)
	}

	for attempt := 1; attempt < pgmigrate.LockTimeoutAttempts; attempt++ {
		movePastThePause(t, fake, attempt)
	}
	err = testwait.Receive(t, "the runner to give up on 0077 after its last attempt", migrated)
	var lockTimeout *pgmigrate.LockTimeoutError
	if err == nil || !errors.As(err, &lockTimeout) || !strings.Contains(lockTimeout.Migration, "0077_messages_embeddings_trigger") {
		t.Fatalf("the runner should have given up on 0077 at its lock timeout, got: %v", err)
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
