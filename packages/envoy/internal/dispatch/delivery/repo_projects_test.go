package delivery

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestAMappingWriteOfEachBuildOfARollNeverDeadlock: while two builds serve one database, a write
// of one repository's mapping by the release before this one and one by this release run at once.
// The release before this one writes a mapping (api's lockRepoProjectWrite in that release) under
// the repository's advisory lock, then its project's owner lock (events.Broker.LockOwners), then
// the row for update, then its write, which fires the mapping's trigger (migration
// 20261117100020). This release's write takes LockRepoProjectWrite's locks, then writes. The first
// write holds the repository's lock and the row; the second, sent then, waits on the repository's
// lock holding nothing: no change log's lock (6900), since no writer of a mapping takes it inside
// its transaction. The first then writes the row, whose trigger records its change in the outbox
// and takes no lock, and commits, and the second lands after it. Were 6900 taken by the second
// write before the repository's lock, and by the trigger in the first's update, each would wait for
// the lock the other holds, and Postgres would end one with 40P01.
func TestAMappingWriteOfEachBuildOfARollNeverDeadlock(t *testing.T) {
	shared, ctx := deliveryTestPool(t)
	pool := wideTestPool(t, shared)
	project, repo := "ROLL", "acme/roll"
	if _, err := pool.Exec(ctx, `insert into projects (key, name, default_assignee) values ($1, 'Roll', 'alice') on conflict do nothing`, project); err != nil {
		t.Fatalf("seed the project: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into repo_projects (repo, project, created_by) values ($1, $2, '{"kind":"system","id":"delivery-test"}'::jsonb)
	`, repo, project); err != nil {
		t.Fatalf("map the repository: %v", err)
	}

	earlier, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the earlier release's write: %v", err)
	}
	t.Cleanup(func() { _ = earlier.Rollback(context.Background()) })
	if _, err := earlier.Exec(ctx, `select pg_advisory_xact_lock(hashtext('dispatch-repo-project'), hashtext($1))`, repo); err != nil {
		t.Fatalf("the earlier release's write takes the repository's lock: %v", err)
	}
	if err := events.NewBroker().LockOwners(ctx, earlier, model.Event{ProjectKey: &project}); err != nil {
		t.Fatalf("the earlier release's write takes its project's owner lock: %v", err)
	}
	if _, err := earlier.Exec(ctx, `select 1 from repo_projects where repo = $1 for update`, repo); err != nil {
		t.Fatalf("the earlier release's write takes the mapping's row: %v", err)
	}

	write := goTimed(func() error {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		if _, err := LockRepoProjectWrite(ctx, tx, events.NewBroker(), repo, project); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `update repo_projects set is_default = false where repo = $1`, repo); err != nil {
			return err
		}
		return tx.Commit(ctx)
	})
	if storetest.WaitForLockWaiters(t, earlier, 1, 10*time.Second) < 1 {
		t.Fatal("this release's write never waited for the repository's lock the earlier release's write holds")
	}
	probe, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the probe: %v", err)
	}
	var changesFree bool
	err = probe.QueryRow(ctx, `select pg_try_advisory_xact_lock($1, $2)`, changeLockClass, changeLockObject).Scan(&changesFree)
	_ = probe.Rollback(ctx)
	if err != nil {
		t.Fatalf("probe the change log's lock: %v", err)
	}
	if !changesFree {
		t.Error("this release's write waits for the repository's lock holding the change log's lock: it takes that lock inside its transaction")
	}

	_, earlierErr := earlier.Exec(ctx, `update repo_projects set is_default = true where repo = $1`, repo)
	if earlierErr == nil {
		earlierErr = earlier.Commit(ctx)
	}
	requireNotDeadlocked(t, "the earlier release's write", earlierErr)
	requireNotDeadlocked(t, "this release's write", (<-write).err)
	if t.Failed() {
		return
	}
	var isDefault bool
	if err := pool.QueryRow(ctx, `select is_default from repo_projects where repo = $1`, repo).Scan(&isDefault); err != nil || isDefault {
		t.Errorf("the mapping is the default %t (%v) after both writes, want this release's write, the later, to stand", isDefault, err)
	}
}
