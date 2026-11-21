package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// An issue that leaves Legion's work carries no mark of a slot released for a wait.
//
// record.Issue.ReleasedForWaitAt is the daemon's answer to "the daemon started this issue and its
// row is gone because it waits on another issue", and that answer is what puts an issue in the
// first part of the waiting line, at the moment the mark holds (record.Waiting). The leave keeps
// the issue's phase (R20 of dispatch://INFRA-3320), so its later return resumes that phase at a new
// entry and waits among the issues whose work came back: a mark left from before the leave would
// stand it there at the old wait's moment, ahead of returns that came back before it. The leave
// clears it (leave, whatever the record in hand says).
func TestAnIssueLeavingTheWorkflowCarriesNoReleasedForWaitMark(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 12, 7, 0, 0, 0, time.UTC)
	engine := New(record.NewStore(), Config{Project: "LEGION", ProductionCheckBound: 48 * time.Hour,
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
	if left != phase.Implementing {
		t.Fatalf("%s left Legion's work at phase %s, want %s kept: this test reads the leave that clears the mark", child, left, phase.Implementing)
	}
	if mark != nil {
		t.Errorf("%s left Legion's work still marked released for a wait at %v, want no mark: its return would wait at that old moment", child, mark)
	}
}
