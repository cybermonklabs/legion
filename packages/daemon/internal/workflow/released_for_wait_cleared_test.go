package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// An issue that leaves the workflow carries no mark of a slot released for a wait.
//
// record.Issue.ReleasedForWaitAt is the daemon's answer to "the daemon started this issue and its
// row is gone because it waits on another issue", and that answer is what puts an issue in the
// first part of the waiting line, at the moment the mark holds (record.Waiting). Admission clears
// it from every issue needing no slot (admit's releaseSlots), but that loop reads the project's
// live issues, and an issue the workflow takes out at phase.Done with a status out of the
// workflow is in neither half of that read (record.Postgres.LiveIssues): it holds no row, so the
// loop never sees it and the mark stands for good.
//
// record.Store.PutIssue carries the stored mark through, by design, so the leave that writes
// phase.Done (leave, which does it for every issue of a tree that is not its root) leaves the
// mark behind. A person who closes a waiting child and later reopens it to todo re-enters it at
// admitted inside its live tree, where the first loop of record.Waiting reads that stale mark
// and stands the issue among the issues whose work came back rather than among the issues whose
// first phase can start, which are ordered by Dispatch rank. Measured on 2026-11-12 over the
// real admission pass, with a cap of 2 and one row free: a reopened child of rank Y carrying a
// mark three days old took that row, and the child of rank B beside it, which the line would
// otherwise have been offered it, did not.
func TestAnIssueLeavingTheWorkflowCarriesNoReleasedForWaitMark(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 12, 7, 0, 0, 0, time.UTC)
	engine := New(record.NewStore(), Config{Project: "LEGION", Linger: time.Hour, ProductionCheckBound: 48 * time.Hour,
		Clock: func() time.Time { return now }}, nil)
	root, child := "LEGION-300", "LEGION-301"
	rootKey := root
	seedIssue(t, pool, record.Issue{Key: root, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: root, Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 1,
		Status: "in_progress", DispatchStatus: "in_progress", Rank: "U", LastDispatchSeq: 1})
	seedIssue(t, pool, record.Issue{Key: child, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: root, Parent: &rootKey, Project: "LEGION", Title: "child", Phase: phase.Implementing, Generation: 1,
		Status: "in_progress", DispatchStatus: "in_progress", Rank: "V", LastDispatchSeq: 1})
	// The child waits on another issue, so admission took its row and marked it
	// (record.Store.ReleaseSlotForWait writes exactly this pair: no slot row, and the moment).
	released := now.Add(-72 * time.Hour)
	if _, err := pool.Exec(ctx, "update issues set released_for_wait_at = $1 where key = $2", released, child); err != nil {
		t.Fatalf("mark %s released for its wait: %v", child, err)
	}

	// A person closes the waiting child in Dispatch.
	applyFacts(t, pool, engine)("child-closed", intake.DispatchIssue{Key: child, ResolvedRepo: "sjawhar/legion", Seq: 2,
		Type: "issue.closed", Status: "done", Title: "child", Rank: "V", Parent: root})

	var left phase.Phase
	var mark *time.Time
	if err := pool.QueryRow(ctx, "select phase, released_for_wait_at from issues where key = $1", child).Scan(&left, &mark); err != nil {
		t.Fatalf("read %s: %v", child, err)
	}
	if left != phase.Done {
		t.Fatalf("%s left the workflow at phase %s, want %s: this test reads the leave that writes it", child, left, phase.Done)
	}
	if mark != nil {
		t.Errorf("%s left the workflow still marked released for a wait at %v, want no mark: nothing clears it after this, since admission reads the live issues alone", child, mark)
	}
}
