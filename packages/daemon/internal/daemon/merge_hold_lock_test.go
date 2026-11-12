package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The project's merge-hold lock (record.Store.SerializeMergeHolds) is what leaves a replacement
// of the hold list and a merge run's decision in one order or the other and no third. The three
// cases of TestAHoldListReplacedWhileAMergeRunDecidesLeavesNoMergeAsleep drive the orders that
// lock produces, and each of them commits one transaction before the other begins, so none of
// them fails when the lock is taken out: measured at 410db4f0, with the SerializeMergeHolds call
// deleted from the decision in daemon's merge_marks.go, every test of internal/daemon passes.
// The deferral inside the decision's own transaction is pinned; the serialization that puts the
// two transactions in an order at all is not.
//
// This drives the window the lock exists for: the route's transaction is open — it has taken the
// lock and replaced the list, and has not yet scanned for the rows to expedite — while the run
// decides. Without the lock the run reads the list as it stood before that transaction, since the
// delete is uncommitted, holds the merge against a hold the controller has already taken off, and
// writes its own deferral to that dead hold's end; the route's scan then finds the row too early
// to expedite it, and the merge sleeps out a hold that no longer exists. With the lock the run
// waits for the route, decides on the list the controller left, and submits the merge.
//
// Two things are asserted, because the second alone would not say why it held: that the run does
// not decide while the route's transaction is open, which is the serialization itself, and what
// the record holds once both have finished, which is the outcome a reader of this issue's record
// cares about.
func TestAMergeRunWaitsForAHoldListReplacementStillInFlight(t *testing.T) {
	ctx := context.Background()
	github := newQueueGitHub(t, "CLEAN")
	github.set(func(g *armableGitHub) { g.changedFiles = []string{heldFile} })
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
	now := time.Now().UTC().Truncate(time.Second)
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208",
		record.ReadyMerge{Number: 42, Head: "ready-head", Queued: now}, now))
	ends := now.Add(30 * time.Minute)
	setMergeHolds(t, pool, records, windowHold(ends, heldPattern))

	// The controller's route, mid-transaction: the lock taken and the list replaced with none,
	// before the scan that expedites the rows a hold was keeping back.
	scoped, committed := intake.WithCommitHooks(ctx)
	route, err := pool.Begin(scoped)
	if err != nil {
		t.Fatalf("begin the route's own transaction: %v", err)
	}
	defer route.Rollback(scoped)
	if err := records.SerializeMergeHolds(scoped, route, "LEGION"); err != nil {
		t.Fatalf("take the project's merge-hold lock as the route does: %v", err)
	}
	if err := records.ReplaceMergeHolds(scoped, route, "LEGION", nil); err != nil {
		t.Fatalf("replace the hold list as the route does: %v", err)
	}

	runner := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return now })
	ran := make(chan error, 1)
	go func() { ran <- runner.RunOnce(ctx) }()

	select {
	case err := <-ran:
		t.Fatalf("the run finished (%v) while the controller's replacement was still in flight: it decided on the "+
			"hold list that transaction is replacing, and a merge held against a hold the controller has taken off "+
			"sleeps out that hold's whole window with nothing left to wake it", err)
	case <-time.After(2 * time.Second):
	}

	// The route finishes as it does: the scan, which finds no row carrying a held moment because
	// the run has not written one, and the commit.
	held, err := records.HeldReadyMerges(scoped, route, "LEGION", now)
	if err != nil {
		t.Fatalf("scan for the rows a hold was keeping back: %v", err)
	}
	for _, row := range held {
		if err := records.ExpediteOutbox(scoped, route, row, now); err != nil {
			t.Fatalf("expedite the row the replacement released: %v", err)
		}
	}
	if err := route.Commit(scoped); err != nil {
		t.Fatalf("commit the route's own transaction: %v", err)
	}
	committed()

	if err := <-ran; err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if _, arms := github.counts(); arms != 1 {
		t.Fatalf("submissions %d, want the one merge: the list the controller left holds nothing, and the run decided "+
			"after that replacement committed: %v", arms, github.requestLog())
	}
	if posts := messagePosts(t, pool); len(posts) != 0 {
		t.Fatalf("Dispatch messages %q, want none: no hold of the list the run decided on holds this merge", posts)
	}
	if heldFor, since, told := heldMarks(t, pool); heldFor != 0 || !since.IsZero() || told {
		t.Fatalf("the row holds %s held since %s told %t, want a row no hold caught", heldFor, since, told)
	}
	if nextAt, _, _, _ := readyMergeState(t, pool); !nextAt.Before(ends) {
		t.Fatalf("the row runs again at %s, want a moment before the replaced hold's end %s: a row asleep on a dead "+
			"hold's clock has nothing left to wake it", nextAt, ends)
	}
}
