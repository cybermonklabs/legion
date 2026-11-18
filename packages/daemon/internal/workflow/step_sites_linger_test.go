// The step sites of a merge and of each way an issue leaves its run: a merge that lands in a
// lingering or a held tree, a root's close and its tree's linger, a leave, a person's move to done,
// a reentry and an adoption. The helpers they share with the other step sites' tests are
// step_sites_test.go's.
package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A root's close lingers its tree and stops each member's step there, but not the merge of a member
// that awaits it: that merge lands later, and its close in the lingering tree ends the open record
// passed, the one end it gets.
func TestALingeringTreesMergeEndsTheMergeStep(t *testing.T) {
	pool := migratedPool(t)
	stepRoot(t, pool, phase.Reviewing)
	engine := stepEngine(nil)
	mergingChild(t, pool, engine)
	start, _ := runOf(stepWrites(t, pool, "LEGION-209"), "merge", 0)
	merge := start.ID
	if merge == "" {
		t.Fatalf("LEGION-209's merge wrote no start: %+v", stepWrites(t, pool, "LEGION-209"))
	}
	applyFacts(t, pool, engine)("root-done", intake.DispatchIssue{Key: "LEGION-208", ResolvedRepo: "sjawhar/legion", Seq: 2, Type: "issue.updated",
		Status: "done", Title: "root", Rank: "U"})
	for _, w := range stepWrites(t, pool, "LEGION-209") {
		if w.ID == merge && isEnd(w) {
			t.Fatalf("the linger ended LEGION-209's merge %+v; a merge that awaits its landing stays open", w)
		}
	}
	seedChildPR(t, pool, record.PullRequestMerged)
	onStored(t, pool, engine, "LEGION-209", func(ctx context.Context, tx pgx.Tx, member record.Issue) error {
		pr, err := engine.store.PullRequest(ctx, tx, "LEGION-209")
		if err != nil {
			return err
		}
		return engine.closeMergedTelling(ctx, tx, member, pr)
	})
	var ends []record.StepRecord
	for _, w := range stepWrites(t, pool, "LEGION-209") {
		if w.ID == merge && isEnd(w) {
			ends = append(ends, w)
		}
	}
	if len(ends) != 1 {
		t.Fatalf("ends of LEGION-209's merge = %+v, want exactly one", ends)
	}
	assertEnd(t, ends[0], "merge", 0, 3, "passed", nil)
	if !ends[0].EndedAt.Equal(stepClock) || ends[0].Agent != treeRoleToken(t, "LEGION-209", claim.RoleMerger) {
		t.Fatalf("the merge's end = %+v, want it ended at the merge's %s by the merger", ends[0], stepClock)
	}
	if got := issueAt(t, pool, "LEGION-209"); got != phase.Done {
		t.Fatalf("LEGION-209 stands at %s, want done", got)
	}
}

// A merge event closes a held merging phase too, both in a live tree and after its root parks.
// Calling closeMerged directly would miss the event handler's choice of which phases it closes.
func TestAMergeEventEndsAHeldMergeStep(t *testing.T) {
	for _, lingering := range []bool{false, true} {
		name := "live tree"
		if lingering {
			name = "lingering tree"
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

// A parked tree hands over only an unheld member with an open pull request. The close of its
// linger retires the other merge workers too, and ends the merge of one whose pull request can no
// longer land: its record ends rather than staying running. A held merger's open pull request
// can still land, so its merge stays open until it does, and ends passed (the one end no move of
// the table writes that is not stopped), or until the pull request closes unmerged, which ends it
// stopped, once. A member handed over keeps its merge open for its readmit to end, whatever its
// pull request does.
func TestALingerCloseEndsAMergeThatItDoesNotHandOver(t *testing.T) {
	merged := intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 43, HeadSHA: "head-43", MergeSHA: "merge-43"}
	closed := intake.PullRequestClosed{Repo: "sjawhar/legion", Number: 43, HeadSHA: "head-43"}
	for _, tc := range []struct {
		name     string
		held     bool
		closedPR bool
		// wantEnd is the close's end of the merge, stopped; handed is its hand-over.
		wantEnd, handed bool
		// after is what the pull request does after the close, and wantAfter the result of the
		// one end the merge step has then ("" for none).
		after     intake.Fact
		wantAfter string
	}{
		{name: "closed pull request", closedPR: true, wantEnd: true},
		{name: "held merger", held: true, after: merged, wantAfter: "passed"},
		{name: "held merger whose pull request closes after the close", held: true, after: closed, wantAfter: "stopped"},
		{name: "open merger is handed over", handed: true},
		{name: "handed-over merger whose pull request closes after the close", handed: true, after: closed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			root := stepRoot(t, pool, phase.Reviewing)
			engine := stepEngine(nil)
			mergingChild(t, pool, engine)
			if tc.held {
				holdFrom(t, pool, "LEGION-209", phase.Merging)
			}
			apply := applyFacts(t, pool, engine)
			apply("park-root", intake.DispatchIssue{Key: root.Key, ResolvedRepo: "sjawhar/legion", Seq: 2, Type: "issue.updated",
				Status: "backlog", Title: "root", Rank: "U"})
			if tc.closedPR {
				apply("close-unmerged", intake.PullRequestClosed{Repo: "sjawhar/legion", Number: 43, HeadSHA: "head-43"})
			}
			if writes := stepWrites(t, pool, "LEGION-209"); len(writes) != 2 || !isStart(writes[1]) || writes[1].Kind != "merge" {
				t.Fatalf("before expiry the merge must still be open: %+v", writes)
			}
			// The close comes at the linger's deadline, and the engine's clock with it.
			parked := issueRecord(t, pool, root.Key)
			engine.cfg.Clock = func() time.Time { return parked.LingerUntil.Add(time.Second) }
			apply("expire-linger", intake.LingerExpired{Issue: root.Key, Generation: root.Generation})
			apply("expire-linger-again", intake.LingerExpired{Issue: root.Key, Generation: root.Generation})
			writes := stepWrites(t, pool, "LEGION-209")
			wantWrites := 2
			if tc.wantEnd {
				wantWrites++
			}
			if len(writes) != wantWrites {
				t.Fatalf("after expiry the merge writes = %+v, want %d rows", writes, wantWrites)
			}
			if tc.wantEnd {
				assertEnd(t, writes[2], "merge", 0, 3, "stopped", nil)
				if writes[2].ID != writes[1].ID {
					t.Fatalf("merge end id %s differs from its start %s", writes[2].ID, writes[1].ID)
				}
			}
			applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
				handover, err := engine.store.Handover(ctx, tx, "LEGION-209")
				if err == nil && (handover != nil) != tc.handed {
					t.Fatalf("handover = %+v, want a handover only for the open, unheld merger", handover)
				}
				return err
			})
			if tc.after == nil {
				return
			}
			// A second delivery of what the pull request did writes no second end.
			apply("after-close", tc.after)
			apply("after-close-again", tc.after)
			var ends []record.StepRecord
			for _, w := range stepWrites(t, pool, "LEGION-209") {
				if w.ID == writes[1].ID && isEnd(w) {
					ends = append(ends, w)
				}
			}
			if tc.wantAfter == "" {
				if len(ends) != 0 {
					t.Fatalf("ends of LEGION-209's handed-over merge after the close = %+v, want none: its readmit ends it", ends)
				}
				return
			}
			if len(ends) != 1 {
				t.Fatalf("ends of LEGION-209's merge after the close = %+v, want exactly one", ends)
			}
			assertEnd(t, ends[0], "merge", 0, 3, tc.wantAfter, nil)
			if got := issueAt(t, pool, "LEGION-209"); tc.wantAfter == "passed" && got != phase.Done {
				t.Fatalf("LEGION-209 merged after its tree's close stands at %s, want done", got)
			}
		})
	}
}

// A root's close stops every member's running step, each keyed on the phase that member stands in,
// and the root's own.
func TestALingeringRootEndsEachMembersStep(t *testing.T) {
	pool := migratedPool(t)
	stepRoot(t, pool, phase.Reviewing)
	stepChild(t, pool, "LEGION-209", phase.Testing, nil)
	stepChild(t, pool, "LEGION-210", phase.Held, &record.Hold{From: phase.Reviewing})
	applyFacts(t, pool, stepEngine(nil))("root-done", intake.DispatchIssue{Key: "LEGION-208", ResolvedRepo: "sjawhar/legion", Seq: 2, Type: "issue.updated",
		Status: "done", Title: "root", Rank: "U"})
	for _, want := range []struct {
		issue      string
		step       int
		generation uint64
	}{{"LEGION-208", 4, 1}, {"LEGION-209", 3, 3}, {"LEGION-210", 4, 3}} {
		writes := stepWrites(t, pool, want.issue)
		if len(writes) != 1 {
			t.Fatalf("step rows of %s = %+v, want one end", want.issue, writes)
		}
		assertEnd(t, writes[0], "step", want.step, want.generation, "stopped", nil)
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
// a tree the workflow holds stops each one's step (leave, beginLinger).
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
			if len(rootWrites) != 1 || len(childWrites) != 1 || counted != 2 {
				t.Fatalf("the workflow's tree left with step rows %+v and %+v and %d step_starts rows, want one end each", rootWrites, childWrites, counted)
			}
			assertEnd(t, rootWrites[0], "step", 2, 2, "stopped", nil)
			assertEnd(t, childWrites[0], "step", 3, 3, "stopped", nil)
		})
	}
}

// A child set back to todo opens its next run; the step of the run it leaves is stopped, and the
// new run, at admitted, runs none yet.
func TestAReenteredChildEndsItsOldRun(t *testing.T) {
	pool := migratedPool(t)
	stepRoot(t, pool, phase.Implementing)
	child := stepChild(t, pool, "LEGION-209", phase.Implementing, nil)
	child.Status = "todo"
	engine := stepEngine(nil)
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		return engine.ReenterChild(ctx, tx, child, RecordedFact(child))
	})
	writes := stepWrites(t, pool, "LEGION-209")
	if len(writes) != 1 {
		t.Fatalf("step rows = %+v, want one end of the old run", writes)
	}
	assertEnd(t, writes[0], "step", 2, 3, "stopped", nil)
	if got := issueRecord(t, pool, "LEGION-209"); got == nil || got.Phase != phase.Admitted || got.Generation != 4 {
		t.Fatalf("LEGION-209 = %+v, want admitted at generation 4", got)
	}
}

// An adopted member takes its next generation in the tree it joins; the step of the run it leaves
// is stopped.
func TestAnAdoptedMemberEndsItsOldRun(t *testing.T) {
	pool := migratedPool(t)
	root := stepRoot(t, pool, phase.Implementing)
	member := record.Issue{Key: "LEGION-300", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-300", Project: "LEGION",
		Title: "queued", Phase: phase.Implementing, Generation: 2, Status: "in_progress", Rank: "W", LastDispatchSeq: 1}
	seedIssue(t, pool, member)
	engine := stepEngine(nil)
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		return engine.moveIntoTree(ctx, tx, member, root, intake.AdoptedChild{})
	})
	writes := stepWrites(t, pool, "LEGION-300")
	if len(writes) != 1 {
		t.Fatalf("step rows = %+v, want one end of the old run", writes)
	}
	assertEnd(t, writes[0], "step", 2, 2, "stopped", nil)
}
