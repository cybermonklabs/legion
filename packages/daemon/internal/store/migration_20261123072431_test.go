package store

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// taskCopyRemoveVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const taskCopyRemoveVersion = 20261123072431

// 20261123072431 writes the outbox check again with every kind 20261122122826 named and
// task_copy_remove, the removal of the working copy of a task that a later version of the plan
// removed (record.TaskCopyRemove), which the check refused before; a kind that is none is still
// refused. The way back the header gives deletes each queued task_copy_remove row, writes the check
// again without the kind, keeping the queued row of every other kind, main's two basis seeds and its
// merge_holds_changed among them (taskCopiesKinds), and deletes its version row, so the roll-forward
// runs the migration again.
func TestMigrationTaskCopyRemoveQueuesTheRemovalOfARemovedTasksWorkingCopy(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, taskCopyRemoveVersion-1)
	exec := func(sql string, args ...any) error {
		_, err := store.Pool().Exec(ctx, sql, args...)
		return err
	}
	refusedBy := func(err error, code, constraint string) bool {
		var pgErr *pgconn.PgError
		return errors.As(err, &pgErr) && pgErr.Code == code && pgErr.ConstraintName == constraint
	}
	queue := func(kind string) error {
		return exec(`insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, 'LEGION-208', '{}', 0, now(), '')`, kind)
	}
	if err := queue("task_copy_remove"); !refusedBy(err, "23514", "outbox_kind_check") {
		t.Fatalf("queueing a task_copy_remove row before the migration = %v, want the kind constraint to refuse it (23514)", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the removal of a removed task's working copy: %v", err)
	}
	for _, kind := range append(append([]string{}, taskCopiesKinds...), "task_pull_requests_close", "task_copy_remove") {
		if err := queue(kind); err != nil {
			t.Fatalf("queue a %s row after the migration: %v", kind, err)
		}
	}
	if err := queue("nope"); !refusedBy(err, "23514", "outbox_kind_check") {
		t.Fatalf("queueing a row of a kind that is none = %v, want the constraint to refuse it (23514)", err)
	}

	if err := exec(wayBackOf(t, taskCopyRemoveVersion, "the removal of a removed task's working copy")); err != nil {
		t.Fatalf("run the way back: %v", err)
	}
	if got := rowsAsText(t, store, `select count(*)::text from outbox where kind = 'task_copy_remove'`); got[0] != "0" {
		t.Fatalf("task_copy_remove rows after the way back = %s, want none", got[0])
	}
	if got := rowsAsText(t, store, `select count(*)::text from outbox where kind = 'task_pull_requests_close'`); got[0] != "1" {
		t.Fatalf("task_pull_requests_close rows after the way back = %s, want the one queued: the way back deletes its own kind alone", got[0])
	}
	if err := queue("task_copy_remove"); !refusedBy(err, "23514", "outbox_kind_check") {
		t.Fatalf("queueing a task_copy_remove row after the way back = %v, want the kind constraint to refuse it (23514)", err)
	}
	if err := queue("task_pull_requests_close"); err != nil {
		t.Fatalf("queue a task_pull_requests_close row after the way back: %v", err)
	}
	if got := rowsAsText(t, store, `select count(*)::text from schema_version where version = 20261123072431`); got[0] != "0" {
		t.Fatalf("schema_version rows of %d after the way back = %s, want none", taskCopyRemoveVersion, got[0])
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("roll forward after the way back: %v", err)
	}
	if err := queue("task_copy_remove"); err != nil {
		t.Fatalf("queue a task_copy_remove row after the roll-forward: %v", err)
	}
}
