package delivery

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The deploy recorder takes its issue's owner lock before it marks the pull request's row
// (recordDeployedEvent), so when another writer of the issue holds that lock the pass waits for
// it holding no transaction id. The shape it replaced marked the row first and met the issue's
// lock inside issue.deployed's append, which waits with a transaction id already open.
//
// TestXidWindowDeployedEventRecorded cannot tell the two apart: with nobody holding the issue's
// lock both run the same three statements inside their window, the marking update, the batch
// appending issue.deployed (whose first statement is that lock) and the COMMIT, and a wait inside
// the append is statement time, not a gap. Only holding the lock and asking what the waiter holds
// does.
func TestTheDeployRecorderWaitsForTheIssuesOwnerLockHoldingNoTransactionID(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	settings := seedDeliverySettings(t, ctx, pool)
	seedDeployedIssues(t, ctx, pool)
	merged := time.Date(2024, 1, 1, 1, 0, 0, 0, time.UTC)
	seedMergedPullRequest(t, ctx, pool, "acme/widgets", 601, merged, deployedIssue)
	seedDeployRun(t, ctx, pool, 910, merged.Add(time.Hour), merged.Add(70*time.Minute), DeliveryJobConclusionSuccess)

	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the holder transaction: %v", err)
	}
	defer holder.Rollback(ctx)
	if err := store.LockOwner(ctx, holder, store.IssueOwner(deployedIssue)); err != nil {
		t.Fatalf("hold the issue's owner lock: %v", err)
	}

	type outcome struct {
		written int
		err     error
	}
	passes := make(chan outcome, 1)
	go func() {
		written, err := RecordDeployedEvents(context.Background(), pool, settings, events.NewBroker())
		passes <- outcome{written, err}
	}()
	early, answeredEarly, holdingXid := storetest.AwaitOwnerLockWaiter(t, &store.Store{Pool: pool}, "the deploy recorder", passes)
	if answeredEarly {
		t.Fatalf("the deploy recorder finished (wrote %d, err %v) while this test held %s: it marked the row "+
			"without the lock issue.deployed's append takes", early.written, early.err, store.IssueOwner(deployedIssue).LockKey())
	}
	if holdingXid > 0 {
		t.Fatalf("the deploy recorder waits for %s with a transaction id already open: it marked the pull "+
			"request's row before it took that lock. Take the issue's owner lock (store.LockOwner) before the "+
			"update.", store.IssueOwner(deployedIssue).LockKey())
	}

	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("release the issue's owner lock: %v", err)
	}
	select {
	case pass := <-passes:
		if pass.err != nil || pass.written != 1 {
			t.Fatalf("the pass once the lock was released wrote %d issue.deployed events (err %v), want 1", pass.written, pass.err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the deploy recorder never finished once the lock was released")
	}
	requireDeployedEvent(t, ctx, pool, deployedIssue, "acme/widgets", 601, 910, "2024-01-01T02:10:00Z")
}
