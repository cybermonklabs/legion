package store

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// The tree barrier's own cleanup (its reservation, its census of claims, its runtime release and
// its confirmation) went with the per-issue cleanup of dispatch://INFRA-4730 (decision 16): each
// of its tests has its issue's counterpart in issue_lifecycle_test.go, which holds the fence of a
// delayed claim, a reservation of another generation or of an issue in work, the operator's
// authority, a restart, the wait for every claim, the serializer's lock order and an issue never
// opened. What is left here is what those tests and the release and suspension tests share.

const rootIssue, childIssue = "LEGION-208", "LEGION-209"

// openLingeringTree is a workflow tree admitted at epoch one whose root is closed at generation
// one: the root's record done, which is a close's state for the issue's own fences. The tree's
// lifecycle row stays open, as R22 of dispatch://INFRA-3320 keeps it written for the release
// before.
func openLingeringTree(t *testing.T, store *Store) {
	t.Helper()
	if _, err := store.OpenTreeLifecycle(context.Background(), "legion", rootIssue, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("open lifecycle of %s: %v", rootIssue, err)
	}
	_, err := store.Pool().Exec(context.Background(), `insert into issues
		(key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ($1, $1, 'LEGION', 'root', 'done', 1, 'done', 'A', 1)
		on conflict (key) do update set generation = excluded.generation, status = excluded.status,
			phase = excluded.phase`, rootIssue)
	if err != nil {
		t.Fatalf("seed the closed root: %v", err)
	}
}

// holdSerializerThenLock is the fact side of the lock order: it takes the serializer, waits until
// step is blocked behind it, then locks the row lock names for rootIssue, and only then commits.
// The row lock must not wait on step, and step must succeed once the fact commits.
func holdSerializerThenLock(t *testing.T, store *Store, lock string, step func() error) {
	t.Helper()
	ctx := context.Background()
	fact, err := store.Pool().BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fact.Rollback(ctx) }()
	if err := treelifecycle.Serialize(ctx, fact); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- step() }()
	if !testwait.Holds(5*time.Second, 20*time.Millisecond, func() bool {
		var waiting int
		if err := store.Pool().QueryRow(ctx, `select count(*) from pg_stat_activity
			where datname = current_database() and wait_event_type = 'Lock' and wait_event = 'advisory'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		return waiting > 0
	}) {
		t.Fatalf("the cleanup step never waited for the serializer (it returned %v)", <-done)
	}
	lockCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := fact.Exec(lockCtx, lock, rootIssue); err != nil {
		t.Fatalf("the fact transaction could not lock the row behind the waiting cleanup step: %v", err)
	}
	if err := fact.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("cleanup step after the fact committed: %v", err)
	}
}
