package workflow

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A chain of merged issues closes from the bottom up, each close once Dispatch shows its children
// done (R24 of dispatch://INFRA-3320): the leaf's done queues the close of the merged issue above
// it, that issue's done on Dispatch queues the next one's, and nothing above waits for anything
// else. A merged issue left in merging is in the workflow, holds no admission slot, since merging
// is past its review (record.KeepsSlot), stands in neither part of the waiting line, and refuses
// its tree root's sign_off for as long as it stands there.
func TestAChainOfMergedIssuesClosesWhenTheLastIssueBeneathItLeaves(t *testing.T) {
	pool := migratedPool(t)
	now := time.Date(2026, 10, 19, 4, 0, 0, 0, time.UTC)
	engine := New(record.NewStore(), Config{Project: "LEGION", ProductionCheckBound: 48 * time.Hour,
		Clock: func() time.Time { return now }}, nil)
	root, child, grandchild, leaf := "LEGION-208", "LEGION-209", "LEGION-210", "LEGION-211"
	seedIssue(t, pool, record.Issue{Key: root, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: root, Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 7, Status: "in_progress",
		Rank: "U", LastDispatchSeq: 1})
	seedSlot(t, pool, record.Slot{Issue: root, Index: 0})
	rootKey, childKey, grandchildKey := root, child, grandchild
	for _, merged := range []struct {
		key    string
		parent *string
		number int
	}{{child, &rootKey, 43}, {grandchild, &childKey, 44}} {
		seedIssue(t, pool, record.Issue{Key: merged.key, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
			Tree: root, Parent: merged.parent, Project: "LEGION", Title: merged.key, Phase: phase.Merging,
			Generation: 3, Status: "retro", Rank: "V" + merged.key, LastDispatchSeq: 1})
		seedPR(t, pool, record.PullRequest{State: record.PullRequestMerged, Issue: merged.key, Repo: "sjawhar/legion",
			Number: merged.number, Branch: "legion/" + merged.key, HeadSHA: "head", MergeSHA: "merge", Failing: []string{}})
	}
	seedIssue(t, pool, record.Issue{Key: leaf, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: root, Parent: &grandchildKey, Project: "LEGION", Title: "leaf", Phase: phase.Implementing, Generation: 3,
		Status: "in_progress", Rank: "V3", LastDispatchSeq: 1})

	landed := func(key string) { landClose(t, pool, engine, key) }
	pending := func(key string) bool { return storedIssue(t, pool, key).PendingClose != nil }

	landed(leaf)
	if !pending(grandchild) || pending(child) {
		t.Fatalf("closes queued once the leaf is done: %s %t, %s %t; want %s's alone", grandchild, pending(grandchild), child, pending(child), grandchild)
	}
	landed(grandchild)
	if at := issueAt(t, pool, grandchild); at != phase.Done || !pending(child) {
		t.Fatalf("%s stands at %s and %s's close queued %t, want %s done and the close of %s queued", grandchild, at, child, pending(child), grandchild, child)
	}
	landed(child)
	if at := issueAt(t, pool, child); at != phase.Done {
		t.Fatalf("%s stands at %s once Dispatch shows it done, want done", child, at)
	}
}
