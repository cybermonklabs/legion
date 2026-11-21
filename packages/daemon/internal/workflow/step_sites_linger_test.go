// The step sites of a merge and of each way an issue leaves its run: a merge that lands in a
// lingering or a held tree, a root's close and its tree's linger, a leave, a person's move to done,
// a reentry and an adoption. The helpers they share with the other step sites' tests are
// step_sites_test.go's.
package workflow

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A merge event closes a held merging phase too, both in a live tree and under a root a person
// moved out of work, once Dispatch shows the close done. Calling closeMerged directly would miss
// the event handler's choice of which phases it closes.
func TestAMergeEventEndsAHeldMergeStep(t *testing.T) {
	for _, lingering := range []bool{false, true} {
		name := "live tree"
		if lingering {
			name = "root out of work"
		}
		t.Run(name, func(t *testing.T) {
			pool := migratedPool(t)
			stepRoot(t, pool, phase.Reviewing)
			engine := stepEngine(nil)
			mergingChild(t, pool, engine)
			holdFrom(t, pool, "LEGION-209", phase.Merging)
			apply := applyFacts(t, pool, engine)
			if lingering {
				apply("park-root", intake.DispatchIssue{Key: "LEGION-208", ResolvedRepo: "sjawhar/legion", Seq: 2, Type: "issue.updated",
					Status: "backlog", Title: "root", Rank: "U"})
			}
			apply("merge-held", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 43, HeadSHA: "head-43", MergeSHA: "merge-43"})
			landClose(t, pool, engine, "LEGION-209")
			writes := stepWrites(t, pool, "LEGION-209")
			if len(writes) != 3 || !isStart(writes[1]) || writes[1].Kind != "merge" || writes[2].ID != writes[1].ID {
				t.Fatalf("held merge writes = %+v, want the retro's end, the merge's start and its matching end", writes)
			}
			assertEnd(t, writes[2], "merge", 0, 3, "passed", nil)
			if got := issueAt(t, pool, "LEGION-209"); got != phase.Done {
				t.Fatalf("merged held child stands at %s, want done", got)
			}
		})
	}
}

// A held issue's step runs through its hold, so the issue's leave ends the step of the phase the
// hold is from.
func TestAHeldIssueThatLeavesEndsItsHeldStep(t *testing.T) {
	pool := migratedPool(t)
	root := stepRoot(t, pool, phase.Held)
	root.Hold = &record.Hold{From: phase.Testing}
	seedIssue(t, pool, root)
	applyFacts(t, pool, stepEngine(nil))("backlog", intake.DispatchIssue{Key: "LEGION-208", ResolvedRepo: "sjawhar/legion", Seq: 2, Type: "issue.updated",
		Status: "backlog", Title: "root", Rank: "U"})
	writes := stepWrites(t, pool, "LEGION-208")
	if len(writes) != 1 {
		t.Fatalf("step rows = %+v, want one end", writes)
	}
	assertEnd(t, writes[0], "step", 3, 1, "stopped", nil)
}

// A person's move of a running issue out of the workflow stops its step, once.
func TestAPersonsMoveToDoneEndsTheRunningStep(t *testing.T) {
	pool := migratedPool(t)
	stepRoot(t, pool, phase.Reviewing)
	stepChild(t, pool, "LEGION-209", phase.Reviewing, nil)
	applyFacts(t, pool, stepEngine(nil))("child-done", intake.DispatchIssue{Key: "LEGION-209", ResolvedRepo: "sjawhar/legion", Seq: 2, Type: "issue.updated",
		Status: "done", Title: "LEGION-209", Parent: "LEGION-208", Rank: "V"})
	writes := stepWrites(t, pool, "LEGION-209")
	if len(writes) != 1 {
		t.Fatalf("step rows = %+v, want one end", writes)
	}
	assertEnd(t, writes[0], "step", 4, 3, "stopped", nil)
}

// A tree the operator holds runs no phase of the workflow, so its root's departure writes no step
// record and counts no start, for the root or for its member, though the root's record carries a
// phase, as a root taken back with its run does while it waits for its slot. The same departure of
// a root the workflow holds stops the root's own step alone (leave): its member stays in work, as
// Dispatch shows it (R20 of dispatch://INFRA-3320).
func TestAnOperatorsTreeLeavesNoStepRecord(t *testing.T) {
	for _, tc := range []struct {
		name     string
		operator bool
	}{{"the workflow's tree", false}, {"the operator's tree", true}} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			root := record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208",
				Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 2, Status: "in_progress", Rank: "U", LastDispatchSeq: 1}
			seedIssue(t, pool, root)
			stepChild(t, pool, "LEGION-209", phase.Testing, nil)
			if tc.operator {
				seedOperatorTree(t, pool, root.Tree)
			} else {
				seedTreeLifecycle(t, pool, root.Tree)
			}
			applyFacts(t, pool, stepEngine(nil))("backlog", intake.DispatchIssue{Key: root.Key, ResolvedRepo: "sjawhar/legion", Seq: 2, Type: "issue.updated",
				Status: "backlog", Title: "root", Rank: "U"})
			var counted int
			if err := pool.QueryRow(t.Context(), `select count(*) from step_starts`).Scan(&counted); err != nil {
				t.Fatalf("count the step_starts rows: %v", err)
			}
			rootWrites, childWrites := stepWrites(t, pool, root.Key), stepWrites(t, pool, "LEGION-209")
			if tc.operator {
				if len(rootWrites) != 0 || len(childWrites) != 0 || counted != 0 {
					t.Fatalf("the operator's tree left with step rows %+v and %+v and %d step_starts rows, want none", rootWrites, childWrites, counted)
				}
				return
			}
			// The boot before the fact opened a row for each run no start counted (bootBefore);
			// the leave ends the root's, and the member's stays open with its run.
			if len(rootWrites) != 1 || len(childWrites) != 0 || counted != 2 {
				t.Fatalf("the workflow's tree left with step rows %+v and %+v and %d step_starts rows, want one end of the root's, none of its member's, and both rows kept", rootWrites, childWrites, counted)
			}
			assertEnd(t, rootWrites[0], "step", 2, 2, "stopped", nil)
		})
	}
}
