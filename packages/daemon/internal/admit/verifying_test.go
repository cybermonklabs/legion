package admit

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The admission rules a merged issue waiting for the checks of its goal meets (Task 7 of the
// done-by-checks plan): the unslotted release passes over it, and the wait holds none of the cap's
// slots. The quiet tree whose root waits for its child in verifying is
// TestAQuietTreeWhoseRootWaitsForItsChildIsNoStall in tree_stalled_test.go.

// seedVerifyingRoot records key as a root whose pull request merged and whose close waits in
// verifying, with the merge snapshot on its row and the stored status of that wait.
func seedVerifyingRoot(t *testing.T, pool *pgxpool.Pool, key, rank string) record.Issue {
	t.Helper()
	issue := quietRootRecord(key, rank, phase.Verifying)
	issue.Status, issue.DispatchStatus = "testing", "testing"
	issue.Merge = record.Merge{Number: 42, Head: "head-42", SHA: "merge-42", Base: "main", Repo: "acme/widgets", MergedAt: fixedNow.Add(-2 * time.Hour)}
	putIssue(t, pool, issue)
	return issue
}

// A root whose close waits in verifying, and one whose next change runs, are each left in the
// workflow by the release of a root nothing would ever start: the wait and the change are the
// daemon's own work, and a release would take the issue out of the workflow where its own
// checks are what close it. The control is the root beside them that has stopped, which the
// release takes as it always did.
func TestARootThatWaitsForItsChecksIsNotReleasedAsUnslotted(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 4)
	const waiting, changing, stopped = "LEGION-600", "LEGION-610", "LEGION-620"
	seedVerifyingRoot(t, pool, waiting, "A")

	// The root whose next change runs: a new generation of its own, at work in implementing,
	// which the record names as the open change (record.Issue.NextChange).
	change := quietRootRecord(changing, "B", phase.Implementing)
	change.Generation, change.NextChange, change.NextChanges = 2, 2, 1
	change.Merge = record.Merge{Number: 43, Head: "head-43", SHA: "merge-43", Base: "main", Repo: "acme/widgets", MergedAt: fixedNow.Add(-3 * time.Hour)}
	putIssue(t, pool, change)

	// The control: a root at a status only a running tree has, in no tree that runs.
	seedUnslottedRoot(t, pool, stopped, "C", phase.Implementing)
	closeLifecycle(t, pool, stopped)

	tick(t, pool, admission, "unslotted")

	for _, key := range []string{waiting, changing} {
		got := issue(t, pool, key)
		if !record.InWorkflow(got) {
			t.Errorf("%s stands at %s/%s after the pass, want it still in the workflow: its own checks close it", key, got.Phase, got.Status)
		}
	}
	if got := issue(t, pool, stopped); record.InWorkflow(got) {
		t.Fatalf("the control %s stands at %s/%s after the pass, want it released: nothing would ever start it", stopped, got.Phase, got.Status)
	}
}

// closeLifecycle closes key's tree lifecycle, so no tree of it runs: the state the release of an
// unslotted root reads (record.LiveRoots).
func closeLifecycle(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		if _, err := tx.Exec(context.Background(), `delete from tree_lifecycles where tree = $1`, key); err != nil {
			t.Fatalf("close the tree lifecycle of %s: %v", key, err)
		}
	})
}

// An issue that waits in verifying holds none of the cap's slots, so the project's one slot is
// free for the next root: the waiting root keeps no row, and a second labelled root is admitted
// while the first still waits for its checks. The test reads the slot rows.
func TestAnIssueInVerifyingHoldsNoAdmissionSlot(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 1)
	const waiting, next = "LEGION-650", "LEGION-660"
	seedVerifyingRoot(t, pool, waiting, "A")
	seedLifecycle(t, pool, waiting)
	// The row the issue took when the daemon started it, which its merge left standing.
	takeSlot(t, pool, waiting)
	seedWaiting(t, pool, next, "B")
	seedMirror(t, pool, next, "todo")

	tick(t, pool, admission, "verifying-slot")

	if holdsSlot(t, pool, waiting) {
		t.Fatalf("%s holds one of the cap's slots while it waits in verifying, want none", waiting)
	}
	if !holdsSlot(t, pool, next) {
		t.Fatalf("%s holds no slot, want the one the project's cap offers: the waiting root holds none", next)
	}
	if got := issue(t, pool, waiting); got.Phase != phase.Verifying {
		t.Fatalf("%s stands at %s after the pass, want it still waiting in verifying", waiting, got.Phase)
	}
}
