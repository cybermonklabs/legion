package admit

import (
	"context"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The admission rules a merged issue waiting for the checks of its goal meets (Task 7 of
// dispatch://INFRA-5580/artifact/plan-md): the unslotted release passes over it, a quiet tree
// whose root waits for a child in verifying is no stall, and the wait holds none of the cap's
// slots.

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

// A quiet tree whose merged root waits in verifying for a child that waits there too is no stall:
// neither issue's wait ends by a worker of the tree, so the controller hears nothing, however
// many passes run. The control stands beside it on the same database and the same clock: a root
// in implementing, which a worker of its tree is there to move, is told.
func TestAQuietTreeWhoseRootWaitsForItsChildIsNoStall(t *testing.T) {
	pool := migratedPool(t)
	bound := deploymentBound(t, "")
	admission := quiet(t, 4)
	admission.UseStalledTreeNotice(bound)
	registerController(t, pool)
	records := claimStore(t, pool)

	const root, child, stoppedRoot = "LEGION-630", "LEGION-631", "LEGION-640"
	seedVerifyingRoot(t, pool, root, "A")
	seedLifecycle(t, pool, root)
	seedPullRequest(t, pool, root, 63, record.PullRequestMerged)
	seedClaim(t, records, root, root, claim.RoleArchitect, supervise.StateIdle)

	// The child beneath it, merged and waiting in verifying too: the root's close waits for it
	// (record.OpenBeneath), and nothing of the tree is at work.
	beneath := seedQuietChild(t, pool, root, child, "B", phase.Verifying, "testing")
	beneath.Merge = record.Merge{Number: 64, Head: "head-64", SHA: "merge-64", Base: "main", Repo: "acme/widgets", MergedAt: fixedNow.Add(-2 * time.Hour)}
	putIssue(t, pool, beneath)
	seedPullRequest(t, pool, child, 64, record.PullRequestMerged)
	seedClaim(t, records, root, child, claim.RoleArchitect, supervise.StateIdle)

	seedQuietRoot(t, pool, stoppedRoot, "C", phase.Implementing)
	seedClaim(t, records, stoppedRoot, stoppedRoot, claim.RoleImplementer, supervise.StateIdle)

	// The fixture's own premise: the root's close waits for the child beneath it, and neither
	// holds one of the cap's slots.
	members := []record.Issue{issue(t, pool, root), issue(t, pool, child)}
	if open := record.OpenBeneath(members[0], members); !slices.Equal(open, []string{child}) {
		t.Fatalf("the issues beneath %s still in the workflow = %v, want %s: this case is a root that waits for its child", root, open, child)
	}
	_, slotRows := passFacts(t, pool)
	for _, slot := range slotRows {
		if slot.Issue == root || slot.Issue == child {
			t.Fatalf("%s holds one of the cap's slots: this case is a tree that waits on no slot", slot.Issue)
		}
	}

	settled := settleMovements(t, pool, root, child, stoppedRoot)
	admission.now = func() time.Time { return settled.Add(bound + time.Hour) }

	for _, pass := range []string{"first", "second", "third"} {
		tick(t, pool, admission, "quiet-tree-"+pass)
	}

	if got, want := stalledRoots(t, pool), []string{stoppedRoot}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the controller was told of %v, want %v alone: a tree whose root waits for a child in verifying waits by design", got, want)
	}
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
