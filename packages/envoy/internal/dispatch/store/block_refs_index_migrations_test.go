package store

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/pgmigrate"
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

// blockRefsReaderHold is how long TestTheBlockRefsTargetIndexDropWaitsOutAReaderPastTheRunnersAttempts
// holds its reader of block_refs open.
const blockRefsReaderHold = 30 * time.Second

// The drop of block_refs_to waits out a reader of block_refs that outlasts the runner's attempts.
// drop index takes ACCESS EXCLUSIVE on block_refs, which waits for every reader, and a read the
// stopped pod left can still run when the boot's drop asks for that lock. Under the runner's own
// bound, pgmigrate.LockTimeout (5 s) for each of pgmigrate.LockTimeoutAttempts (3) with
// pgmigrate.LockTimeoutRetryPause (2 s) between, the runner gives up after about 19 s and the boot
// exits; the migration's own set local lock_timeout of 120 s is what lets it wait the reader out.
//
// The reader holds ACCESS SHARE on block_refs for 30 s, on a connection of its own. That is past
// the runner's 19 s, so a drop with no bound of its own fails here, where a shorter hold would let
// it pass: under a hold of 10 s the second attempt, which starts at 7 s, takes the lock at 10 s.
// Migrate runs on context.Background(), as the boot runs it on a context with no deadline. A
// deadline in the runner shorter than 30 s would end the wait and fail this test through
// Migrate's error. The test does not hold the reader for the drop's whole 120 s: that nothing ends
// the wait between 30 s and 120 s is read from the code. The boot's context is the one main
// (cmd/dispatch/main.go) makes with signal.NotifyContext, which carries no deadline, and main
// hands it to database.Migrate(ctx) with none added. Migrate and migrate (store.go) add none. The
// one context.WithTimeout in store.go, closeBounded's, bounds only the
// close of a connection. pgmigrate's bound only a diagnosis of the holders after a lock timeout
// (lock.go line 394, diagnoseHolders) and the rollback and close after it (line 405), the close of
// the lock watch's own connection (line 611), and the restore of a non-transactional migration's
// session settings (nontransactional.go line 110), which the drop, a transactional migration,
// does not run.
func TestTheBlockRefsTargetIndexDropWaitsOutAReaderPastTheRunnersAttempts(t *testing.T) {
	t.Parallel()
	attempts := pgmigrate.LockTimeoutAttempts*pgmigrate.LockTimeout + (pgmigrate.LockTimeoutAttempts-1)*pgmigrate.LockTimeoutRetryPause
	if blockRefsReaderHold <= attempts {
		t.Fatalf("the reader's hold of %s does not outlast the runner's %s of attempts and pauses, so a drop with no bound of its own would pass", blockRefsReaderHold, attempts)
	}
	ctx := context.Background()
	store := openMigratedTestStore(t, blockRefsSourceIndex)
	reader, err := pgx.Connect(ctx, store.Pool.Config().ConnString())
	if err != nil {
		t.Fatalf("connect the reader: %v", err)
	}
	t.Cleanup(func() { reader.Close(context.Background()) })
	read, err := reader.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the reader's transaction: %v", err)
	}
	if _, err := read.Exec(ctx, "select 1 from block_refs limit 1"); err != nil {
		t.Fatalf("read block_refs: %v", err)
	}
	held := time.Now()
	// The goroutine is the reader connection's only user from here; the cleanup waits for it, and
	// runs before the cleanup above that closes the connection.
	done := make(chan struct{})
	var commitSent time.Time
	var commitErr error
	go func() {
		defer close(done)
		time.Sleep(blockRefsReaderHold)
		commitSent = time.Now()
		commitErr = read.Commit(context.Background())
	}()
	t.Cleanup(func() { <-done })

	err = store.Migrate(context.Background())
	returned := time.Now()
	if err != nil {
		t.Fatalf("store.Migrate with a reader of block_refs held for %s: %v", blockRefsReaderHold, err)
	}
	<-done
	if commitErr != nil {
		t.Fatalf("commit the reader: %v", commitErr)
	}
	if returned.Before(commitSent) {
		t.Errorf("store.Migrate returned %s after the read began, before the reader committed at %s: the drop did not wait for the reader", returned.Sub(held), commitSent.Sub(held))
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
