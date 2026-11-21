package workflow

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A merged issue's close waits for each Dispatch child of it that is not done (R24 of
// dispatch://INFRA-3320), and is queued once the last of them is done. The wait is no tree root's
// alone: an intermediate parent stands in merging after its merge, waiting for its child, and what
// must follow is the close's write when that child is done and the parent's done once Dispatch
// shows it.
//
// It matters beyond the one issue: a merged issue in merging is in the workflow, so while it
// stands there its tree's root cannot be accepted (SIGNOFF_CHILD_IN_WORKFLOW) or closed. The
// issue itself holds no admission slot, since merging is past its review (record.KeepsSlot), and
// stands in no part of the waiting line. Its merging phase is finished (PhaseFinished), so nothing
// but this close moves it on.
func TestAMergedChildClosesWhenTheIssueBeneathItLeaves(t *testing.T) {
	pool := migratedPool(t)
	now := time.Date(2026, 10, 19, 4, 0, 0, 0, time.UTC)
	engine := New(record.NewStore(), Config{Project: "LEGION", ProductionCheckBound: 48 * time.Hour,
		Clock: func() time.Time { return now }}, nil)
	root, child, grandchild := "LEGION-208", "LEGION-209", "LEGION-210"
	seedIssue(t, pool, record.Issue{Key: root, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: root, Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 7, Status: "in_progress",
		Rank: "U", LastDispatchSeq: 1})
	seedSlot(t, pool, record.Slot{Issue: root, Index: 0})
	rootKey, childKey := root, child
	seedIssue(t, pool, record.Issue{Key: child, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: root, Parent: &rootKey, Project: "LEGION", Title: "child", Phase: phase.Merging, Generation: 3,
		Status: "retro", Rank: "V1", LastDispatchSeq: 1})
	seedIssue(t, pool, record.Issue{Key: grandchild, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: root, Parent: &childKey, Project: "LEGION", Title: "grandchild", Phase: phase.Implementing, Generation: 3,
		Status: "in_progress", Rank: "V2", LastDispatchSeq: 1})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: child, Repo: "sjawhar/legion", Number: 43,
		Branch: "legion/LEGION-209", HeadSHA: "head", Failing: []string{}})

	apply := applyFacts(t, pool, engine)
	apply("merged", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 43, MergeSHA: "merge"})
	if at := issueAt(t, pool, child); at != phase.Merging {
		t.Fatalf("%s stands at %s after its merge, want merging while %s is not done", child, at, grandchild)
	}
	assertOutboxCount(t, pool, "dispatch_status", 0)

	// The grandchild is done, as a person's move of it to done makes it; its phase stays (R20).
	landClose(t, pool, engine, grandchild)
	if at := issueAt(t, pool, child); at != phase.Merging || storedIssue(t, pool, child).PendingClose == nil {
		t.Fatalf("%s stands at %s with close %+v once its last child is done, want merging with its close queued", child, at, storedIssue(t, pool, child).PendingClose)
	}
	landClose(t, pool, engine, child)
	if at := issueAt(t, pool, child); at != phase.Done {
		t.Fatalf("%s stands at %s once Dispatch shows it done, want done", child, at)
	}
}
