package workflow

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// rootWithNoPullRequest is the root LEGION-208 at admitted, generation 1, whose plan names words,
// with no pull request record.
func rootWithNoPullRequest(t *testing.T, pool *pgxpool.Pool, words ...string) record.Issue {
	t.Helper()
	root := stepRoot(t, pool, phase.Admitted)
	if _, err := pool.Exec(t.Context(), "delete from pull_requests where issue = $1", root.Key); err != nil {
		t.Fatalf("delete the pull request of %s: %v", root.Key, err)
	}
	seedStepsPlan(t, pool, root.Key, root.Generation, words...)
	return root
}

// A task whose issue has no pull request has no merge: its retro's end makes it done, with its
// task done, no merger started, Dispatch's status done queued once, and no production check, which
// opens only on a merged pull request. With a pull request record, the retro's end moves the issue
// to its merge, as today. The control is the table whose retro always moves to merging: the issue
// with no pull request stands at merging, with a merger started, waiting for a merge nothing opens.
func TestATaskWithNoPullRequestIsDoneWhenItsRetroEnds(t *testing.T) {
	t.Run("no pull request: done at the retro's end", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := rootWithNoPullRequest(t, pool, "tester")
		move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
		move(t, pool, engine, root.Key, TriggerTesterPassed, record.TaskPlace{}, record.PhaseRow{})
		assertWalksAt(t, pool, engine, root.Key, phase.Retro, record.TaskPlace{Kind: record.PlaceRetro})
		move(t, pool, engine, root.Key, TriggerRetroCompleted, record.TaskPlace{}, record.PhaseRow{})

		if got := issueOf(t, pool, engine, root.Key); got.Phase != phase.Done {
			t.Fatalf("%s after its retro stands in %s, want done", root.Key, got.Phase)
		}
		if task := taskOf(t, pool, engine, root.Key); task.State != record.TaskDone {
			t.Fatalf("task a of %s after its retro is %s, want done", root.Key, task.State)
		}
		if starts := roleStartsOf(t, pool, root.Key, claim.RoleMerger); len(starts) != 0 {
			t.Fatalf("%s's retro wrote %d starts of the merger, want none: it has no pull request to merge", root.Key, len(starts))
		}
		var done int
		for _, write := range statusWrites(t, pool, root.Key) {
			if write.Status == "done" {
				done++
			}
		}
		if done != 1 {
			t.Fatalf("%s's retro queued %d status writes of done, want one", root.Key, done)
		}
		if check := productionCheck(t, pool, root.Key); check != nil {
			t.Fatalf("%s holds the production check %+v, want none: no pull request merged", root.Key, check)
		}
	})

	t.Run("a pull request record: the merge, as today", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := rootWithNoPullRequest(t, pool, "tester")
		move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
		move(t, pool, engine, root.Key, TriggerTesterPassed, record.TaskPlace{}, record.PhaseRow{})
		seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: root.Key, Repo: "sjawhar/legion", Number: 42,
			Branch: "legion/" + root.Key, HeadSHA: "head-42", Failing: []string{}})
		move(t, pool, engine, root.Key, TriggerRetroCompleted, record.TaskPlace{}, record.PhaseRow{})

		assertWalksAt(t, pool, engine, root.Key, phase.Merging, record.TaskPlace{Kind: record.PlaceMerge})
		if starts := roleStartsOf(t, pool, root.Key, claim.RoleMerger); len(starts) != 1 {
			t.Fatalf("%s's retro wrote %d starts of the merger, want one", root.Key, len(starts))
		}
	})
}

// reviewersVerdict is the reviewer's completion of its step of key with verdict, under the claim
// and at the entry its start wrote on its row, as the route hands it to the engine.
func reviewersVerdict(t *testing.T, pool *pgxpool.Pool, engine *Engine, key, verdict string) intake.HandoffComplete {
	t.Helper()
	row := roleRowOf(t, pool, engine, key, claim.RoleReviewer)
	return intake.HandoffComplete{Issue: key, Role: claim.RoleReviewer, Claim: row.Claim, Generation: 1, Entry: row.Entry,
		Summary: "The plan names no rollback.", Commit: stepHead, Verdict: verdict, HandoffRecord: "review-1", HandoffVersion: 1}
}

// A step of the type review in a task with no pull request ends by its verdict, since no review
// on GitHub can decide it: on `planner reviewer`, the reviewer's fail stops the task, which has no
// step of the type implementation before the review to take the work, and the owner is told once
// with the task and the step; its pass starts the retro, which the owner does, since the task has
// no step of the type implementation. The control is today's reviewing case, every completion to
// settleRound, which leaves the issue at reviewing whatever the verdict.
func TestAStepOfTheTypeReviewWithNoPullRequestEndsByItsVerdict(t *testing.T) {
	for _, tc := range []struct {
		verdict string
		check   func(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string)
	}{
		{verdict: "fail", check: func(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string) {
			if task := taskOf(t, pool, engine, key); task.State != record.TaskStopped {
				t.Fatalf("task a of %s after the reviewer's fail is %s, want stopped", key, task.State)
			}
			told := noticesOf(t, pool, key, "task-stopped")
			if len(told) != 1 || told[0].Role != claim.RoleReviewer || told[0].Phase != phase.Reviewing {
				t.Fatalf("the fail told %+v, want one task-stopped of step 2's role reviewer at reviewing", told)
			}
		}},
		{verdict: "pass", check: func(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string) {
			assertWalksAt(t, pool, engine, key, phase.Retro, record.TaskPlace{Kind: record.PlaceRetro})
			if told := ownerSteps(t, pool, key); len(told) != 1 || told[0].Role != "" || told[0].Phase != phase.Retro {
				t.Fatalf("the pass told %+v, want one owner-step of the retro", told)
			}
		}},
	} {
		t.Run("the reviewer's "+tc.verdict, func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			root := rootWithNoPullRequest(t, pool, "planner", "reviewer")
			move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
			move(t, pool, engine, root.Key, TriggerPlannerCompleted, record.TaskPlace{}, record.PhaseRow{})
			assertWalksAt(t, pool, engine, root.Key, phase.Reviewing, stepPlace(2))
			if result := applyFacts(t, pool, engine)(tc.verdict, reviewersVerdict(t, pool, engine, root.Key, tc.verdict)); result.Refusal != nil {
				t.Fatalf("the reviewer's %s was refused: %+v", tc.verdict, result.Refusal)
			}
			tc.check(t, pool, engine, root.Key)
		})
	}
}
