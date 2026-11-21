package evals

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliverytest"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestTheDeliveryFixtureAndAMappingWriteOfOneRepositoryNeverDeadlock: every writer of
// repo_projects takes a mapping write's locks in one order (delivery.LockRepoProjectWrite, as the
// mapping routes do, api/repo_projects.go): the repository's advisory lock, its projects' owner
// locks, then its row. None takes the change log's lock (6900) inside its transaction, since the
// table's trigger records its settings change in the change log's outbox (migration
// 20261117100020), and each writer moves it into the log after its commit. The delivery fixture a
// running server's database is loaded with (deliverytest.Write, which evalfixture -delivery runs)
// once inserted its mappings in a statement of its own, so its trigger took 6900 while it held the
// row, and a mapping write that took 6900 first and then wrote the same repository's row waited for
// that row: Postgres ended one with 40P01 (a mapping PUT answering 500). The fixture is paused at
// its projects insert, after its delivery settings, while the mapping transaction takes a mapping
// write's locks for one of the fixture's repositories, as the route does first; then the fixture is
// let on and waits behind those locks, and the mapping transaction writes the same repository's row
// and rolls back, as a refused mapping write does.
func TestTheDeliveryFixtureAndAMappingWriteOfOneRepositoryNeverDeadlock(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	pause := pauseInserts(t, r, "projects", "true")
	written := make(chan error, 1)
	go func() {
		_, err := deliverytest.Write(ctx, r.database.Pool, deliverytest.Spec{Project: "M2", Days: 2})
		written <- err
	}()
	if storetest.WaitForLockWaiters(t, pause, 1, lockOrderBound) < 1 {
		t.Fatalf("the fixture never reached its projects insert")
	}
	mapping, err := r.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the mapping write: %v", err)
	}
	t.Cleanup(func() { _ = mapping.Rollback(context.Background()) })
	if _, err := delivery.LockRepoProjectWrite(ctx, mapping, events.NewBroker(), "acme/m2-platform", "M2"); err != nil {
		t.Fatalf("the mapping write takes a mapping write's locks: %v", err)
	}
	if err := pause.Commit(ctx); err != nil {
		t.Fatalf("let the fixture on: %v", err)
	}
	if storetest.WaitForLockWaiters(t, mapping, 1, lockOrderBound) < 1 {
		t.Fatalf("the fixture never waited for the mapping write's locks")
	}
	_, mappingErr := mapping.Exec(ctx, `
		insert into repo_projects (repo, project, created_by, is_default)
		values ('acme/m2-platform', 'M2', '{"kind":"system","id":"mapping"}', false)
		on conflict (repo) do nothing`)
	_ = mapping.Rollback(ctx)
	requireNoDeadlock(t, "the mapping write", mappingErr)
	requireNoDeadlock(t, "the delivery fixture", <-written)
}
