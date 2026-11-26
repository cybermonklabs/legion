package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/pgmigrate"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The embeddings trigger migrations (0073-0077) take one table's lock at a time, each in its
// own transaction, exactly as the search migrations (0056-0061) do and for the same reason:
// while a transaction holds `messages` (the last table, 0077) with a lock CREATE TRIGGER's own
// SHARE ROW EXCLUSIVE conflicts with, the runner applies 0074-0076 and commits each, gives up on
// 0077 at pgmigrate.LockTimeout, and meanwhile a read of `issues` - whose lock a single-file
// migration would still be holding - answers without waiting on a lock. Once the holder ends,
// the runner finishes.
// The holder must take at least ROW EXCLUSIVE: a plain SELECT's ACCESS SHARE is compatible with
// CREATE TRIGGER's own lock (unlike the search migrations, which also ALTER TABLE ADD COLUMN and
// so need ACCESS EXCLUSIVE, conflicting with a reader) - a no-op UPDATE matching no row still
// takes the table-level lock its statement needs.
func TestEmbeddingsMigrationsLockOneTableAtATime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openMigratedTestStore(t, 72)
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

	// Wait until the runner is queued on messages, then read issues, which must answer without
	// waiting on a lock while it is (requireAnsweredWithoutALockWait).
	waitForQueuedLock(t, store, "messages", "ShareRowExclusiveLock")
	var version int
	var issues int
	requireAnsweredWithoutALockWait(t, store, "a read of issues while the runner waits for messages", func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, "select count(*) from issues").Scan(&issues); err != nil {
			return err
		}
		return tx.QueryRow(ctx, "select max(version) from schema_migrations").Scan(&version)
	})
	if version != 76 {
		t.Errorf("while the runner waited for messages, schema_migrations was at %d, want 76 (0073-0076 committed one by one)", version)
	}

	err = testwait.Receive(t, "store.Migrate to give up on 0077 at its lock timeout", migrated)
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
