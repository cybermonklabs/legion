package workflow

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// deployLink is the pull request of a second repository that the issues of these tests link in
// Dispatch, acme/deploy#6990, and linkedPR its name as a notice names it.
var deployLink = intake.PullRequestLink{Repo: "acme/deploy", Number: 6990}

const linkedPR = "acme/deploy#6990"

// noCheckEngine is returnEngine for a project whose `production_check` is off: a close opens no
// production check, so each row of these tests holds whether or not the close also waits for the
// checks of dispatch://INFRA-5580.
func noCheckEngine() *Engine {
	engine := returnEngine()
	engine.cfg.ProductionCheckRole = ""
	return engine
}

// linkEvent is Dispatch's event of key, under parent ("" for the root LEGION-208), at seq, at the
// status the record holds, that names the issue's links whole: each of links.
func linkEvent(t *testing.T, pool *pgxpool.Pool, key, parent string, seq int64, links ...intake.PullRequestLink) intake.DispatchIssue {
	t.Helper()
	status := storedIssue(t, pool, key).Status
	if parent == "" {
		return linksOf(key, seq, status, links...)
	}
	event := statusMove(key, parent, status, seq)
	event.Links, event.LinksKnown = append([]intake.PullRequestLink{}, links...), true
	return event
}

// linkDeploy links acme/deploy#6990 on key, under parent, through Dispatch's event at seq, and
// records it open on branch through the daemon's read of it (linkRead).
func linkDeploy(t *testing.T, pool *pgxpool.Pool, apply func(string, intake.Fact) intake.Result, key, parent string, seq int64, branch string) {
	t.Helper()
	mustSpend(t, "the link of "+linkedPR, apply(fmt.Sprintf("linked-%s-%d", key, seq), linkEvent(t, pool, key, parent, seq, deployLink)))
	mustSpend(t, "the read of "+linkedPR, apply(fmt.Sprintf("read-%s-%d", key, seq), linkRead(key, deployLink.Repo, deployLink.Number,
		record.PullRequestOpen, branch)))
	if pr := pullRequestOf(t, pool, deployLink.Repo, deployLink.Number); pr == nil || !pr.Linked || pr.State != record.PullRequestOpen || pr.Issue != key {
		t.Fatalf("premise: %s is %+v, want it open and linked by %s", linkedPR, pr, key)
	}
}

// deployMerged is GitHub's merge of acme/deploy#6990, and deployClosed its close without a merge.
var (
	deployMerged = intake.PullRequestMerged{Repo: deployLink.Repo, Number: deployLink.Number, MergeSHA: "merge-6990", HeadSHA: "head-6990"}
	deployClosed = intake.PullRequestClosed{Repo: deployLink.Repo, Number: deployLink.Number, HeadSHA: "head-6990", UpdatedAt: linkedAt.Add(time.Minute)}
)

// stillOpen fails unless key stands in want and its record writes no done: its close closed nothing.
func stillOpen(t *testing.T, pool *pgxpool.Pool, key string, want phase.Phase, after string) {
	t.Helper()
	got := storedIssue(t, pool, key)
	if got.Phase != want || got.Status == "done" {
		t.Fatalf("after %s %s stands in %s at %s, want %s and not done", after, key, got.Phase, got.Status, want)
	}
	for _, write := range statusWrites(t, pool, key) {
		if write.Status == "done" {
			t.Fatalf("after %s %s's status writes hold done (%+v), want none", after, key, write)
		}
	}
}

// closedDone fails unless key stands in done with the status done.
func closedDone(t *testing.T, pool *pgxpool.Pool, key, after string) {
	t.Helper()
	if got := storedIssue(t, pool, key); got.Phase != phase.Done || got.Status != "done" {
		t.Fatalf("after %s %s stands in %s at %s, want done", after, key, got.Phase, got.Status)
	}
}

// holdToldOnce fails unless key's architect holds one pr-holds-close, which names name and the way
// out, the removal of its link.
func holdToldOnce(t *testing.T, pool *pgxpool.Pool, key, name, after string) {
	t.Helper()
	holds := noticesOf(t, pool, key, "pr-holds-close")
	if len(holds) != 1 || !strings.Contains(holds[0].Reason, name) || !strings.Contains(holds[0].Reason, "Remove its link") {
		t.Fatalf("after %s the pr-holds-close notices of %s are %+v, want one that names %s and says to remove its link", after, key, holds, name)
	}
}

// mergerKeepsWorking fails unless key's merging is not finished (PhaseFinished) and the start of
// task's merge names its merger (startAgent): the close is held, so the merger keeps working.
func mergerKeepsWorking(t *testing.T, pool *pgxpool.Pool, engine *Engine, key, task string) {
	t.Helper()
	onStored(t, pool, engine, key, func(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
		finished, err := PhaseFinished(ctx, engine.store, tx, issue)
		if err != nil {
			return err
		}
		if finished {
			t.Errorf("%s's merging is finished, want not finished while a pull request of its set holds its close", key)
		}
		s, err := engine.ScopeOf(ctx, tx, issue, task)
		if err != nil {
			return err
		}
		agent, err := engine.startAgent(ctx, tx, s, phase.Merging)
		if err != nil {
			return err
		}
		if agent == "" {
			t.Errorf("the start of %s's merge of task %s names no agent, want its merger while a pull request of its set holds its close", key, task)
		}
		return nil
	})
}

// heldForPullRequests fails unless key stands held for pull-requests from merging
// (record.HoldForPullRequests) over tasks that are all done, with no done status written.
func heldForPullRequests(t *testing.T, pool *pgxpool.Pool, engine *Engine, key, after string) {
	t.Helper()
	got := storedIssue(t, pool, key)
	if !record.HeldFor(got, record.HoldForPullRequests) || got.Hold.From != phase.Merging || got.Status == "done" {
		t.Fatalf("after %s %s stands in %s with hold %+v at %s, want held for pull-requests from merging, not done", after, key, got.Phase, got.Hold, got.Status)
	}
	for id, task := range tasksOf(t, pool, engine, key) {
		if task.State != record.TaskDone {
			t.Fatalf("after %s task %s of %s is %s, want done: the hold stands over tasks that are all done", after, id, key, task.State)
		}
	}
	stillOpen(t, pool, key, phase.Held, after)
}

// An issue with two pull requests closes at the last merge (dispatch://INFRA-5910 D4, decisions 9
// and 11): the merge of one pull request closes nothing while another pull request of the issue's
// set is open, at each place that closes an issue, and the last merge closes the issue as today.
// LEGION-208's task a merges its own sjawhar/legion#42, and LEGION-208 links acme/deploy#6990, a
// pull request of a second repository; the project checks nothing in production
// (`production_check: off`). Each row is a record of its own:
//   - row 1, the merge handler: in merging, in both orders: after the first merge the architect is
//     told (pr-holds-close for a's merge, pr-merged for the linked one's), the issue stays in
//     merging, closes nothing and writes no done, its merging is not finished and its merger keeps
//     working; after the second it closes;
//   - row 2: an issue beneath it in the workflow: the last merge's close waits for it, as today,
//     and closes at its leave;
//   - row 3: the linked merge while the issue is in reviewing and #42 open: the issue stays in
//     reviewing, at a's review, and the merge ends the review of the merged pull request alone;
//   - row 4, the entry into merging on a merged pull request: a reaches its merge on #42 merged
//     while the linked one is open: the issue stays in merging, its merger is started, and it closes
//     at the linked merge;
//   - row 5, a child's leave: the last merge waits for the child, and the child's leave finds the
//     linked pull request open: the issue stays in merging until the linked merge;
//   - row 6, the boot's sweep: it closes nothing between the two merges, and closes at the second,
//     whether the handler applies it or the store holds it as merged when the daemon boots;
//   - row 7, where no merge is in hand: the last step of a task that merges nothing, and a version
//     of the plan that leaves only finished tasks, each while the linked pull request is open: the
//     issue is held for pull-requests from merging over tasks that are all done, and closes at the
//     linked merge, which the record finds by its issue column, or at the linked one's close with
//     no merge;
//   - row 8, a member of a lingering tree: its merge closes nothing while the linked pull request is
//     open, and the expiry of the linger hands the member over;
//   - row 9: a's own pull request, which the issue links, is open at the merge of b, the last: the
//     stack's close leaves it open, the issue stays in merging, and closes when #42 merges, or
//     when it closes with no merge;
//   - row 10: a merges, and the issue links a's own pull request: a's merge is that pull request's
//     merge, and the issue closes at it.
//
// Each row names the change that fails it: the condition taken out of closeMerged fails rows 1, 5,
// 6, 8, 9 and 10 (the first merge closes the issue); taken out of the entry into merging alone, row
// 4 (its standing step starts no merger); out of the end of a task's last step and the version, row
// 7; a hand-over that reads no held member, row 8; a merge handler that finds a linked merge's issue
// through a plan task, row 7 (its merge closes nothing); and one that moves the phase at the linked
// merge, row 3. Row 2 passes without the change: it holds today's rule against a change that drops
// it.
func TestAnIssueWithTwoPullRequestsClosesAtTheLastMerge(t *testing.T) {
	t.Parallel()
	ownMerged := mergedFact(42, "a")
	merging := func(t *testing.T) (*pgxpool.Pool, *Engine, func(string, intake.Fact) intake.Result, record.Issue) {
		t.Helper()
		pool := migratedPool(t)
		engine := noCheckEngine()
		root := stepRoot(t, pool, phase.Merging)
		seedTasksAt(t, pool, root.Key, root.Generation, taskAt{id: "a", state: record.TaskRunning, place: atMerge, number: 42, merges: true})
		apply := applyFacts(t, pool, engine)
		linkDeploy(t, pool, apply, root.Key, "", 2, "fix/deploy")
		return pool, engine, apply, root
	}

	t.Run("row 1: the merge handler, the task's merge first", func(t *testing.T) {
		pool, engine, apply, root := merging(t)
		mustSpend(t, "the merge of #42", apply("merged-42", ownMerged))
		stillOpen(t, pool, root.Key, phase.Merging, "the merge of #42")
		holdToldOnce(t, pool, root.Key, linkedPR, "the merge of #42")
		if closes := closesOf(t, pool, root.Key); len(closes) != 0 {
			t.Fatalf("after the merge of #42 the stack's closes are %+v, want none", closes)
		}
		assertTask(t, tasksOf(t, pool, engine, root.Key), "a", record.TaskRunning, atMerge)
		mergerKeepsWorking(t, pool, engine, root.Key, "a")
		mustSpend(t, "the merge of "+linkedPR, apply("merged-6990", deployMerged))
		closedDone(t, pool, root.Key, "the merge of "+linkedPR)
	})

	t.Run("row 1: the merge handler, the linked merge first", func(t *testing.T) {
		pool, engine, apply, root := merging(t)
		mustSpend(t, "the merge of "+linkedPR, apply("merged-6990", deployMerged))
		stillOpen(t, pool, root.Key, phase.Merging, "the merge of "+linkedPR)
		if told := noticesOf(t, pool, root.Key, "pr-merged"); len(told) != 1 || !strings.Contains(told[0].Reason, linkedPR) {
			t.Fatalf("after the merge of %s the pr-merged notices are %+v, want one that names it", linkedPR, told)
		}
		assertTask(t, tasksOf(t, pool, engine, root.Key), "a", record.TaskRunning, atMerge)
		mustSpend(t, "the merge of #42", apply("merged-42", ownMerged))
		closedDone(t, pool, root.Key, "the merge of #42")
	})

	t.Run("row 2: the close waits for the issue beneath, as today", func(t *testing.T) {
		pool, engine, apply, root := merging(t)
		stepChild(t, pool, heldParentChild, phase.Implementing, nil)
		mustSpend(t, "the merge of "+linkedPR, apply("merged-6990", deployMerged))
		mustSpend(t, "the merge of #42", apply("merged-42", ownMerged))
		stillOpen(t, pool, root.Key, phase.Merging, "the merge of #42")
		if told := noticesOf(t, pool, root.Key, "pr-merged"); !slices.ContainsFunc(told, func(n record.Notice) bool {
			return strings.Contains(n.Reason, "close is waiting for "+heldParentChild)
		}) {
			t.Fatalf("after the merge of #42 the pr-merged notices are %+v, want one that says the close waits for %s", told, heldParentChild)
		}
		childLeavesDone(t, pool, engine)
		closedDone(t, pool, root.Key, heldParentChild+"'s leave")
	})

	t.Run("row 3: a linked merge in reviewing ends the review of that pull request alone", func(t *testing.T) {
		pool := migratedPool(t)
		engine := noCheckEngine()
		root := stepRoot(t, pool, phase.Reviewing)
		seedTasksAt(t, pool, root.Key, root.Generation,
			taskAt{id: "a", steps: []string{"implementer", "reviewer"}, state: record.TaskRunning, place: atStep(2), number: 42, merges: true})
		apply := applyFacts(t, pool, engine)
		linkDeploy(t, pool, apply, root.Key, "", 2, "fix/deploy")
		mustSpend(t, "the merge of "+linkedPR, apply("merged-6990", deployMerged))
		stillOpen(t, pool, root.Key, phase.Reviewing, "the merge of "+linkedPR)
		assertTask(t, tasksOf(t, pool, engine, root.Key), "a", record.TaskRunning, atStep(2))
		if pr := pullRequestOf(t, pool, "sjawhar/legion", 42); pr == nil || pr.State != record.PullRequestOpen {
			t.Fatalf("after the merge of %s #42 is %+v, want it open", linkedPR, pr)
		}
		if told := noticesOf(t, pool, root.Key, "pr-merged"); len(told) != 1 || !strings.Contains(told[0].Reason, linkedPR) {
			t.Fatalf("after the merge of %s the pr-merged notices are %+v, want one that names it", linkedPR, told)
		}
	})

	t.Run("row 4: the entry into merging on a merged pull request", func(t *testing.T) {
		pool := migratedPool(t)
		engine := noCheckEngine()
		root := stepRoot(t, pool, phase.Retro)
		seedTasksAt(t, pool, root.Key, root.Generation, taskAt{id: "a", state: record.TaskRunning, place: atRetro, number: 42, merged: true, merges: true})
		apply := applyFacts(t, pool, engine)
		linkDeploy(t, pool, apply, root.Key, "", 2, "fix/deploy")
		taskMoves(t, pool, engine, root.Key, "a", TriggerRetroCompleted, "")
		stillOpen(t, pool, root.Key, phase.Merging, "a's retro")
		assertTask(t, tasksOf(t, pool, engine, root.Key), "a", record.TaskRunning, atMerge)
		if starts := roleStartsOf(t, pool, root.Key, claim.RoleMerger); len(starts) == 0 {
			t.Fatalf("a's entry into merging started no merger, want one: %s holds the close, and the merger keeps working", linkedPR)
		}
		holdToldOnce(t, pool, root.Key, linkedPR, "a's entry into merging")
		mustSpend(t, "the merge of "+linkedPR, apply("merged-6990", deployMerged))
		closedDone(t, pool, root.Key, "the merge of "+linkedPR)
	})

	t.Run("row 5: a child's leave", func(t *testing.T) {
		pool, engine, apply, root := merging(t)
		stepChild(t, pool, heldParentChild, phase.Implementing, nil)
		mustSpend(t, "the merge of #42", apply("merged-42", ownMerged))
		stillOpen(t, pool, root.Key, phase.Merging, "the merge of #42")
		childLeavesDone(t, pool, engine)
		stillOpen(t, pool, root.Key, phase.Merging, heldParentChild+"'s leave")
		holdToldOnce(t, pool, root.Key, linkedPR, heldParentChild+"'s leave")
		mustSpend(t, "the merge of "+linkedPR, apply("merged-6990", deployMerged))
		closedDone(t, pool, root.Key, "the merge of "+linkedPR)
	})

	for _, second := range []string{"the handler", "the store as the daemon boots"} {
		t.Run("row 6: the boot's sweep, the linked merge applied by "+second, func(t *testing.T) {
			pool, engine, apply, root := merging(t)
			mustSpend(t, "the merge of #42", apply("merged-42", ownMerged))
			applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error { return engine.sweepMerged(ctx, tx) })
			stillOpen(t, pool, root.Key, phase.Merging, "the boot's sweep after the merge of #42")
			holdToldOnce(t, pool, root.Key, linkedPR, "the boot's sweep after the merge of #42")
			if second == "the handler" {
				mustSpend(t, "the merge of "+linkedPR, apply("merged-6990", deployMerged))
				closedDone(t, pool, root.Key, "the merge of "+linkedPR)
				return
			}
			if _, err := pool.Exec(t.Context(), "update pull_requests set state = 'merged', merge_sha = 'merge-6990' where repo = $1 and number = $2",
				deployLink.Repo, deployLink.Number); err != nil {
				t.Fatal(err)
			}
			applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error { return engine.sweepMerged(ctx, tx) })
			closedDone(t, pool, root.Key, "the boot's sweep of the merge of "+linkedPR)
		})
	}

	// noMergeInHand is LEGION-208 in testing with its plan: a, which needs t and merges, done at its
	// merge with #42 merged, and t, a tester's step that a needs and that so merges nothing, at that
	// step; and the link of the second repository's pull request, open. t's pass is the end of the
	// issue's last task, with no merge in hand.
	noMergeInHand := func(t *testing.T) (*pgxpool.Pool, *Engine, func(string, intake.Fact) intake.Result, record.Issue) {
		t.Helper()
		pool := migratedPool(t)
		engine := noCheckEngine()
		root := stepRoot(t, pool, phase.Testing)
		seedTasksAt(t, pool, root.Key, root.Generation,
			taskAt{id: "a", needs: []string{"t"}, state: record.TaskDone, place: atMerge, number: 42, merged: true, merges: true},
			taskAt{id: "t", steps: []string{"tester"}, state: record.TaskRunning, place: atStep(1)})
		apply := applyFacts(t, pool, engine)
		linkDeploy(t, pool, apply, root.Key, "", 2, "fix/deploy")
		taskMoves(t, pool, engine, root.Key, "t", TriggerTesterPassed, "")
		heldForPullRequests(t, pool, engine, root.Key, "t's pass")
		holdToldOnce(t, pool, root.Key, linkedPR, "t's pass")
		return pool, engine, apply, root
	}

	t.Run("row 7: the last step of a task that merges nothing, then the linked merge", func(t *testing.T) {
		pool, _, apply, root := noMergeInHand(t)
		mustSpend(t, "the merge of "+linkedPR, apply("merged-6990", deployMerged))
		closedDone(t, pool, root.Key, "the merge of "+linkedPR)
	})

	t.Run("row 7: the last step of a task that merges nothing, then the linked close with no merge", func(t *testing.T) {
		pool, _, apply, root := noMergeInHand(t)
		mustSpend(t, "the close of "+linkedPR, apply("closed-6990", deployClosed))
		closedDone(t, pool, root.Key, "the close of "+linkedPR)
		if told := noticesOf(t, pool, root.Key, "pr-closed-unmerged"); len(told) != 0 {
			t.Fatalf("the close of %s sent pr-closed-unmerged %+v, want none: a merge closed the issue's task", linkedPR, told)
		}
	})

	t.Run("row 7: a version that leaves only finished tasks", func(t *testing.T) {
		pool := migratedPool(t)
		engine := noCheckEngine()
		root := stepRoot(t, pool, phase.Implementing)
		seedTasksAt(t, pool, root.Key, root.Generation,
			taskAt{id: "a", state: record.TaskDone, place: atMerge, number: 42, merged: true, merges: true},
			taskAt{id: "s", state: record.TaskRunning, place: atStep(1)})
		apply := applyFacts(t, pool, engine)
		linkDeploy(t, pool, apply, root.Key, "", 2, "fix/deploy")
		if result := apply("stop-s", ownersStop(t, root, "s")); result.Refusal != nil {
			t.Fatalf("the owner's stop of s was refused: %+v", result.Refusal)
		}
		aBlock := keyed(t, "a", "", []string{"implementer"})
		aBlock.Merges = true
		mustTake(t, "version 2, which removes s", apply("version-2", laterVersion(t, root.Key, root.Generation, 2, aBlock)))
		heldForPullRequests(t, pool, engine, root.Key, "version 2")
		holdToldOnce(t, pool, root.Key, linkedPR, "version 2")
		mustSpend(t, "the merge of "+linkedPR, apply("merged-6990", deployMerged))
		closedDone(t, pool, root.Key, "the merge of "+linkedPR)
	})

	t.Run("row 8: a member of a lingering tree, and the linger's expiry", func(t *testing.T) {
		pool := migratedPool(t)
		engine := noCheckEngine()
		root := stepRoot(t, pool, phase.Implementing)
		child := stepChild(t, pool, heldParentChild, phase.Merging, nil)
		seedTasksAt(t, pool, child.Key, child.Generation, taskAt{id: "a", state: record.TaskRunning, place: atMerge, number: 43, merges: true})
		apply := applyFacts(t, pool, engine)
		linkDeploy(t, pool, apply, child.Key, root.Key, 2, "fix/deploy")
		mustSpend(t, "the park of the root", apply("root-backlog", intake.DispatchIssue{Key: root.Key, ResolvedRepo: "sjawhar/legion", Seq: 2,
			Type: "issue.updated", Status: "backlog", Title: "root", Rank: "U"}))
		recordTheStatusAMoveShows(t, pool, root.Key, "backlog")
		assertLingersToItsClose(t, pool, root.Key)
		mustSpend(t, "the merge of #43", apply("merged-43", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 43, MergeSHA: "merge-a"}))
		stillOpen(t, pool, child.Key, phase.Merging, "the merge of #43 in the lingering tree")
		assertTask(t, tasksOf(t, pool, engine, child.Key), "a", record.TaskRunning, atMerge)
		holdToldOnce(t, pool, child.Key, linkedPR, "the merge of #43 in the lingering tree")
		mustSpend(t, "the linger's expiry", apply("linger", intake.LingerExpired{Issue: root.Key, Generation: root.Generation}))
		if got := handoverFences(t, pool); !slices.Contains(got, child.Key) {
			t.Fatalf("the linger's expiry fenced %v for hand-over, want %s: its close is held by %s, so no merge of its tree's ends it", got, child.Key, linkedPR)
		}
	})

	// ownLinked is LEGION-208 in merging with its plan: a, done at its last step with its own #42
	// open, which merges nothing, and b, which needs a and merges, at its merge with its own #45
	// open; and the link of a's own #42. b's merge is the last.
	ownLinked := func(t *testing.T) (*pgxpool.Pool, func(string, intake.Fact) intake.Result, record.Issue) {
		t.Helper()
		pool := migratedPool(t)
		engine := noCheckEngine()
		root := stepRoot(t, pool, phase.Merging)
		seedTasksAt(t, pool, root.Key, root.Generation,
			taskAt{id: "a", state: record.TaskDone, place: atStep(1), number: 42},
			taskAt{id: "b", needs: []string{"a"}, state: record.TaskRunning, place: atMerge, number: 45, merges: true})
		apply := applyFacts(t, pool, engine)
		mustSpend(t, "the link of #42", apply("linked-42", linkEvent(t, pool, root.Key, "", 2, intake.PullRequestLink{Repo: "sjawhar/legion", Number: 42})))
		mustSpend(t, "the merge of b's #45", apply("merged-45", mergedFact(45, "b")))
		stillOpen(t, pool, root.Key, phase.Merging, "the merge of b's #45")
		for _, close := range closesOf(t, pool, root.Key) {
			if slices.Contains(close.Numbers, 42) {
				t.Fatalf("the stack's close %+v closes #42, want none: the issue links it", close)
			}
		}
		holdToldOnce(t, pool, root.Key, "sjawhar/legion#42", "the merge of b's #45")
		return pool, apply, root
	}

	t.Run("row 9: a task's own pull request the issue links, open at the last merge, then merged", func(t *testing.T) {
		pool, apply, root := ownLinked(t)
		mustSpend(t, "the merge of a's #42", apply("merged-42", ownMerged))
		closedDone(t, pool, root.Key, "the merge of a's #42")
	})

	t.Run("row 9: a task's own pull request the issue links, open at the last merge, then closed with no merge", func(t *testing.T) {
		pool, apply, root := ownLinked(t)
		mustSpend(t, "the close of a's #42", apply("closed-42", intake.PullRequestClosed{Repo: "sjawhar/legion", Number: 42, HeadSHA: "head-a",
			UpdatedAt: linkedAt.Add(time.Minute)}))
		closedDone(t, pool, root.Key, "the close of a's #42")
	})

	t.Run("row 10: a merge of the task's own pull request the issue links", func(t *testing.T) {
		pool := migratedPool(t)
		engine := noCheckEngine()
		root := stepRoot(t, pool, phase.Merging)
		seedTasksAt(t, pool, root.Key, root.Generation, taskAt{id: "a", state: record.TaskRunning, place: atMerge, number: 42, merges: true})
		apply := applyFacts(t, pool, engine)
		mustSpend(t, "the link of #42", apply("linked-42", linkEvent(t, pool, root.Key, "", 2, intake.PullRequestLink{Repo: "sjawhar/legion", Number: 42})))
		if pr := pullRequestOf(t, pool, "sjawhar/legion", 42); pr == nil || !pr.Linked || pr.Task != "a" {
			t.Fatalf("premise: #42 is %+v, want a's own, linked", pr)
		}
		mustSpend(t, "the merge of #42", apply("merged-42", ownMerged))
		closedDone(t, pool, root.Key, "the merge of #42")
	})
}

// An open linked pull request that holds an issue's close is told to the architect once, with the
// way out (dispatch://INFRA-5910 D4, decision 10). LEGION-208, in merging, has every other condition
// of its close met at the merge of its task's #42, and links acme/deploy#6990, open: the architect
// gets one pr-holds-close that names it and says to remove its link. A redelivery of the merge,
// Dispatch's event that names the same link again and the boot's sweep each pass over the issue,
// and none sends a second notice. Once the link is removed, the issue closes.
//
// The faults it catches: the notice sent at each pass, under which the sweep sends a second; and the
// notice left out, under which the merge sends none.
func TestAnOpenLinkedPullRequestThatHoldsTheCloseIsToldOnce(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	engine := noCheckEngine()
	root := stepRoot(t, pool, phase.Merging)
	seedTasksAt(t, pool, root.Key, root.Generation, taskAt{id: "a", state: record.TaskRunning, place: atMerge, number: 42, merges: true})
	apply := applyFacts(t, pool, engine)
	linkDeploy(t, pool, apply, root.Key, "", 2, "fix/deploy")
	mustSpend(t, "the merge of #42", apply("merged-42", mergedFact(42, "a")))
	stillOpen(t, pool, root.Key, phase.Merging, "the merge of #42")
	holdToldOnce(t, pool, root.Key, linkedPR, "the merge of #42")

	apply("merged-42-again", mergedFact(42, "a"))
	mustSpend(t, "the same link named again", apply("linked-again", linkEvent(t, pool, root.Key, "", 3, deployLink)))
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error { return engine.sweepMerged(ctx, tx) })
	stillOpen(t, pool, root.Key, phase.Merging, "a second pass and the boot's sweep")
	holdToldOnce(t, pool, root.Key, linkedPR, "a second pass and the boot's sweep")

	mustSpend(t, "the removal of the link", apply("unlinked", linkEvent(t, pool, root.Key, "", 4)))
	closedDone(t, pool, root.Key, "the removal of the link")
}
