package store

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// deliveryBranchVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const deliveryBranchVersion = 20261122131321

// The branch of the task a delivery's step is of (supervise.Delivery.Branch) reaches the store
// with the delivery and comes back from it, through a re-put that replaces the delivery: a
// restarted daemon sends what it reads back, and each send adopts the working copy of that
// branch, so a branch lost on the way would adopt the issue's own working copy for another task's
// step. A delivery the release before wrote, which names no such column, reads back with none,
// which is the issue's own branch.
func TestADeliveryKeepsItsBranch(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-213-implementer")
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}
	of := supervise.Delivery{ID: "outbox:7", Task: "Implement task b.", Phase: phase.Implementing, QueuedAt: at(1), Branch: "legion/LEGION-213.b"}
	for _, put := range []supervise.Delivery{of, {ID: "outbox:8", Task: "Implement it.", Phase: phase.Implementing, QueuedAt: at(2)}, of} {
		if err := store.PutDelivery(ctx, c.Token, put); err != nil {
			t.Fatalf("put the delivery %s: %v", put.ID, err)
		}
		if got := onlyClaim(t, store).Pending; got == nil || got.ID != put.ID || got.Branch != put.Branch {
			t.Fatalf("the delivery read back = %+v, want %s on the branch %q", got, put.ID, put.Branch)
		}
	}
	if _, err := store.pool.Exec(ctx, `delete from pending_task_deliveries where claim_token = $1`, string(c.Token)); err != nil {
		t.Fatalf("retire the delivery: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `insert into pending_task_deliveries (claim_token, delivery_id, task, phase, generation, queued_at)
		values ($1, 'outbox:9', 'Implement it.', 'implementing', 1, now())`, string(c.Token)); err != nil {
		t.Fatalf("write the delivery as the release before writes it: %v", err)
	}
	if got := onlyClaim(t, store).Pending; got == nil || got.ID != "outbox:9" || got.Branch != "" {
		t.Fatalf("the delivery read back = %+v, want outbox:9 on no branch: the release before named none", got)
	}
}

// 20261122131321 adds pending_task_deliveries.branch: each delivery the upgrade finds reads no
// branch, the issue's own, which is the only working copy the release before adopted. The way
// back the header gives takes `branch` off every supervise row of the outbox, which the release
// before decodes strictly, drops the column, and deletes its version row, so the roll-forward runs
// the migration again.
func TestMigrationDeliveryBranchKeepsEachDeliveryAndItsWayBackTakesTheBranchOff(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, deliveryBranchVersion-1)
	if _, err := store.pool.Exec(ctx, `update pending_task_deliveries set branch = 'legion/LEGION-214.b'`); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("update pending_task_deliveries set branch before the migration = %v, want the column unknown: the fixture is not the old shape", err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the delivery's branch: %v", err)
	}
	c := tmuxClaim("legion-LEGION-214-implementer")
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}
	if err := store.PutDelivery(ctx, c.Token, supervise.Delivery{ID: "outbox:1", Task: "Implement task b.", Phase: phase.Implementing,
		QueuedAt: at(1), Branch: "legion/LEGION-214.b"}); err != nil {
		t.Fatalf("put the delivery: %v", err)
	}
	for _, payload := range []string{
		`{"op":"start","tree":"LEGION-214","role":"implementer","task":"Implement task b.","generation":1,"branch":"legion/LEGION-214.b"}`,
		`{"op":"start","tree":"LEGION-214","role":"implementer","task":"Implement it.","generation":1}`,
	} {
		if _, err := store.pool.Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ('supervise', 'LEGION-214', $1::jsonb, 0, now(), '')`, payload); err != nil {
			t.Fatalf("queue a start: %v", err)
		}
	}

	if _, err := store.pool.Exec(ctx, wayBackOf(t, deliveryBranchVersion, "the delivery's branch")); err != nil {
		t.Fatalf("run the way back: %v", err)
	}
	if got := rowsAsText(t, store, `select (payload ? 'branch')::text || ' ' || (payload->>'task') from outbox where kind = 'supervise' order by id`); len(got) != 2 ||
		got[0] != "false Implement task b." || got[1] != "false Implement it." {
		t.Fatalf("the starts after the way back = %q, want each without the key branch and its task kept", got)
	}
	if _, err := store.pool.Exec(ctx, `update pending_task_deliveries set branch = ''`); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("update pending_task_deliveries set branch after the way back = %v, want the column gone", err)
	}
	if got := rowsAsText(t, store, `select count(*)::text from schema_version where version = 20261122131321`); got[0] != "0" {
		t.Fatalf("schema_version rows of %d after the way back = %s, want none", deliveryBranchVersion, got[0])
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("roll forward after the way back: %v", err)
	}
	if got := onlyClaim(t, store).Pending; got == nil || got.ID != "outbox:1" || got.Branch != "" {
		t.Fatalf("the delivery after the roll-forward = %+v, want outbox:1 on no branch", got)
	}
}
