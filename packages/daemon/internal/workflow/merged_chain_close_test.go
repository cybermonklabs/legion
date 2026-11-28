package workflow

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// One departure can end the wait of more than one merged issue above it, and closing the nearest
// one is itself a departure that may end the next one's. A chain of merged issues therefore closes
// whole once the last issue beneath it leaves, one read after each departure: a merged issue in
// verifying is in the workflow, holds no admission slot, since verifying keeps none
// (record.KeepsSlot), stands in neither part of the waiting line, and refuses its tree root's
// sign_off for as long as it stands there.
func TestAChainOfMergedIssuesClosesWhenTheLastIssueBeneathItLeaves(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	now := time.Date(2026, 10, 19, 4, 0, 0, 0, time.UTC)
	engine := New(record.NewStore(), Config{Project: "LEGION", Linger: time.Hour, ProductionCheckBound: 48 * time.Hour,
		Clock: func() time.Time { return now }}, nil)
	root, child, grandchild, leaf := "LEGION-208", "LEGION-209", "LEGION-210", "LEGION-211"
	seedIssue(t, pool, record.Issue{Key: root, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: root, Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 7, Status: "in_progress",
		Rank: "U", LastDispatchSeq: 1})
	seedSlot(t, pool, record.Slot{Issue: root, Index: 0})
	apply := applyFacts(t, pool, engine)
	rootKey, childKey, grandchildKey := root, child, grandchild
	for _, merged := range []struct {
		key    string
		parent *string
		number int
	}{{child, &rootKey, 43}, {grandchild, &childKey, 44}} {
		seedIssue(t, pool, record.Issue{Key: merged.key, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
			Tree: root, Parent: merged.parent, Project: "LEGION", Title: merged.key, Phase: phase.Merging,
			Generation: 3, Status: "retro", Rank: "V" + merged.key, LastDispatchSeq: 1})
		seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: merged.key, Repo: "sjawhar/legion",
			Number: merged.number, Branch: "legion/" + merged.key, HeadSHA: "head", Failing: []string{}})
	}
	seedIssue(t, pool, record.Issue{Key: leaf, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: root, Parent: &grandchildKey, Project: "LEGION", Title: "leaf", Phase: phase.Implementing, Generation: 3,
		Status: "in_progress", Rank: "V3", LastDispatchSeq: 1})
	apply("child-merged", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 43, MergeSHA: "merge-43"})
	apply("grandchild-merged", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 44, MergeSHA: "merge-44"})
	read := func(issue string, children ...dispatch.IssueChild) {
		t.Helper()
		apply("read-"+issue, intake.CloseChecksRead{Issue: issue, Generation: 3, ReadAt: now.Add(time.Second),
			Answer: &record.CheckReadAnswer{Results: dispatch.IssueResults{Issue: issue}, Children: children}})
	}
	for _, key := range []string{child, grandchild} {
		if at := issueAt(t, pool, key); at != phase.Verifying {
			t.Fatalf("%s stands at %s after its merge, want verifying", key, at)
		}
	}
	// The reads each merge queued run out, as their executor finishing them does.
	if _, err := pool.Exec(t.Context(), "delete from outbox where kind = 'close_read'"); err != nil {
		t.Fatalf("finish the reads of the merges: %v", err)
	}

	apply("leaf-done", intake.DispatchIssue{Key: leaf, ResolvedRepo: "sjawhar/legion", Seq: 2,
		Type: "issue.closed", Status: "done", Title: "leaf", Parent: grandchild, Rank: "V3"})
	if at := issueAt(t, pool, leaf); at != phase.Done {
		t.Fatalf("%s stands at %s, want done: the fixture's own premise", leaf, at)
	}
	read(grandchild, dispatch.IssueChild{Key: leaf, Status: "done"})
	if at := issueAt(t, pool, grandchild); at != phase.Done {
		t.Fatalf("%s stands at %s, want done: the read after the issue beneath it left passed", grandchild, at)
	}
	if got := len(issueOutboxOf(t, pool, child, "close_read")); got != 1 {
		t.Fatalf("%s's close_read rows after %s closed = %d, want one: closing it was itself a departure", child, grandchild, got)
	}
	read(child, dispatch.IssueChild{Key: grandchild, Status: "done"})
	if at := issueAt(t, pool, child); at != phase.Done {
		t.Fatalf("%s stands at %s, want done: %s waited for nothing else", child, at, child)
	}
}
