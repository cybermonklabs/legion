package workflow

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// seedTwoOnTheBranch records #42 onto main and then #43 onto secondBase, both open on
// LEGION-208's own branch at head, each as its open records it, the second after the first:
// GitHub holds one open pull request of a head onto each base, so two open at once from one branch
// stand onto two bases. with, where it is not nil, sets what a row of the inventory needs of each
// before it is written.
func seedTwoOnTheBranch(t *testing.T, pool *pgxpool.Pool, head string, with func(*record.PullRequest)) {
	t.Helper()
	for _, pr := range []record.PullRequest{
		{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: sharedBranch, Base: "main", HeadSHA: head, Failing: []string{}},
		{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 43, Branch: sharedBranch, Base: secondBase, HeadSHA: head, Failing: []string{}},
	} {
		if with != nil {
			with(&pr)
		}
		seedPR(t, pool, pr)
	}
}

// closedNumbers is every pull request number the closes of pull requests of key's outbox name
// with comment, in order, whichever row names each (record.TaskPullRequestsClose): a close of
// several pull requests may be one row or one row apiece.
func closedNumbers(t *testing.T, pool *pgxpool.Pool, key, comment string) []int {
	t.Helper()
	var numbers []int
	for _, close := range closesOf(t, pool, key) {
		if close.Comment == comment {
			numbers = append(numbers, close.Numbers...)
		}
	}
	slices.Sort(numbers)
	return numbers
}

// assertRowsGone fails the test unless the record holds no row of any of numbers, after what.
func assertRowsGone(t *testing.T, pool *pgxpool.Pool, after string, numbers ...int) {
	t.Helper()
	for _, number := range numbers {
		if rows := pullRequestRows(t, pool, number); rows != 0 {
			t.Errorf("after %s the record holds %d rows of #%d, want none", after, rows, number)
		}
	}
}

// Every read of the workflow that has only a branch takes each open pull request on it
// (dispatch://INFRA-5910/spec#b-d1: "A push, which carries a branch and no number, applies to each
// open pull request on that branch, and so does each read that has only a branch"). This is the
// workflow's part of the inventory of those reads, rows b1 to b5 of the plan's decision 20; the
// daemon's (b6 to b9), the API's (b10) and ghbranch's (b11) hold a test of this name in their own
// packages. In each row LEGION-208's own branch holds two open pull requests, #42 onto main and #43
// onto legion/LEGION-208-ci-base, as INFRA-1276's branch held theorem-labs/agent-infra#3654 onto
// main and the draft #3955 onto legion/INFRA-1276-ci-base: both are read, and neither takes the
// place of the other.
//   - b1, the synchronize (pullRequestSynchronized): the fact names its pull request's number, and
//     the synchronize of #42 moves #42's head alone.
//   - b2, the push (push): it names a branch and no number, and each open pull request on the
//     branch records it.
//   - b3, the merge-state reads a fact asks for (askMergeState): a synchronize of #42 asks the read
//     of #42 alone, and a push of the branch asks one read for each open pull request on it. The
//     read names its pull request by its issue and its task here (Config.ReadMergeState), which two
//     pull requests of one task share, so this row counts the reads asked.
//   - b4, a push onto a base (basePush): it reaches the open pull request onto that base and no
//     other pull request of the branch, whichever spelling of the repository the push names.
//   - b5, the closes of the pull requests of a task the plan removed (closeRemovedPullRequests) and
//     of an earlier run (closeEarlierPullRequests): each pull request on the branch is closed, and
//     its row goes.
//
// The fault it catches is a read by the branch that answers one row (record.Store's
// pullRequestByBranch, and a row keyed by the issue and the branch, so the second pull request's
// open takes the place of the first's), and a write by the issue and the branch
// (SetPullRequestBaseMoved, DeletePullRequest), which reaches every row of the branch.
func TestEachReadByBranchTakesEveryOpenPullRequestOnIt(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)

	t.Run("b1 the synchronize of one of them", func(t *testing.T) {
		pool := migratedPool(t)
		tasksRoot(t, pool)
		seedTwoOnTheBranch(t, pool, headOne, nil)
		mustSpend(t, "the synchronize of #42", applyFacts(t, pool, stepEngine(nil))("synchronize-42", intake.PullRequestSynchronized{
			Repo: "sjawhar/legion", Number: 42, Branch: sharedBranch, Base: "main", Before: headOne, HeadSHA: headTwo, Sender: "a-writer", UpdatedAt: at}))
		assertRowsStand(t, pool, "the synchronize of #42", map[int]rowStands{
			42: {state: record.PullRequestOpen, base: "main", head: headTwo},
			43: {state: record.PullRequestOpen, base: secondBase, head: headOne},
		})
	})

	t.Run("b2 a push of the branch", func(t *testing.T) {
		pool := migratedPool(t)
		tasksRoot(t, pool)
		seedTwoOnTheBranch(t, pool, headOne, nil)
		mustSpend(t, "the push of the branch", applyFacts(t, pool, stepEngine(nil))("push", intake.Push{
			Repo: "sjawhar/legion", Branch: sharedBranch, Before: headOne, After: headTwo, Pusher: "a-writer", Sender: "a-writer"}))
		assertPushReached(t, pool, "the push of the branch", headOne, headTwo, 42, 43)
		assertRowsStand(t, pool, "the push of the branch", map[int]rowStands{
			42: {state: record.PullRequestOpen, base: "main", head: headOne},
			43: {state: record.PullRequestOpen, base: secondBase, head: headOne},
		})
	})

	t.Run("b3 the merge-state reads a fact of the branch asks for", func(t *testing.T) {
		// rig is LEGION-208 in reviewing with both pull requests at headOne, the reviewer's
		// approval standing there, and GitHub's answer for that head `blocked` with nothing read of
		// its checks: a round that waits on GitHub's answer for each pull request
		// (reviewRound), whatever fact of the branch arrives, so each fact here owes a read of each
		// pull request it reaches. It answers the reads the engine asks for, each named by its
		// issue and its task.
		rig := func(t *testing.T) (*pgxpool.Pool, *Engine, *[]string) {
			t.Helper()
			pool := migratedPool(t)
			seedMergeStateRound(t, pool, record.MergeStateBlocked)
			seedTwoOnTheBranch(t, pool, headOne, func(pr *record.PullRequest) {
				pr.Cancelled, pr.Mergeability, pr.MergeState = []string{}, record.MergeabilityMergeable, record.MergeStateBlocked
			})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: reviewClaim,
				Decision: &record.ReviewDecision{State: "approved", Head: headOne}})
			var asked []string
			engine := testEngine(config.DesignGateRootIssues, nil)
			engine.cfg.ReadMergeState = func(issue, task string) { asked = append(asked, fmt.Sprintf("%s task %q", issue, task)) }
			return pool, engine, &asked
		}

		pool, engine, asked := rig(t)
		mustSpend(t, "the synchronize of #42", applyFacts(t, pool, engine)("synchronize-42", intake.PullRequestSynchronized{
			Repo: "sjawhar/legion", Number: 42, Branch: sharedBranch, Before: headEarlier, HeadSHA: headOne, Sender: "a-writer"}))
		if want := []string{`LEGION-208 task ""`}; !slices.Equal(*asked, want) {
			t.Errorf("the synchronize of #42 asked the merge-state reads %v; want %v, the read of #42 alone: the fact names its pull request's number", *asked, want)
		}

		pool, engine, asked = rig(t)
		mustSpend(t, "the push of the branch", applyFacts(t, pool, engine)("push", intake.Push{
			Repo: "sjawhar/legion", Branch: sharedBranch, Before: headEarlier, After: headOne, Pusher: "a-writer", Sender: "a-writer"}))
		if want := []string{`LEGION-208 task ""`, `LEGION-208 task ""`}; !slices.Equal(*asked, want) {
			t.Errorf("the push of %s asked the merge-state reads %v; want %v, one for each of its two open pull requests, #42 and #43: a push names a branch and no number",
				sharedBranch, *asked, want)
		}
	})

	t.Run("b4 a push onto the base of each", func(t *testing.T) {
		for _, push := range []struct {
			name  string
			fact  intake.BasePush
			moved map[int]string
		}{
			{"onto main", intake.BasePush{Repo: "sjawhar/legion", Branch: "main", After: "main-moved"}, map[int]string{42: "main-moved", 43: ""}},
			// GitHub names the repository as its owner spells it now, which the record, written in
			// another spelling, can differ from: a push onto a base is of the repository whatever
			// its case.
			{"onto the other base, the repository in another spelling", intake.BasePush{Repo: "SJawhar/Legion", Branch: secondBase, After: "other-moved"},
				map[int]string{42: "", 43: "other-moved"}},
		} {
			t.Run(push.name, func(t *testing.T) {
				pool := migratedPool(t)
				tasksRoot(t, pool)
				seedTwoOnTheBranch(t, pool, headOne, nil)
				mustSpend(t, "the push "+push.name, applyFacts(t, pool, stepEngine(nil))("push", push.fact))
				assertBasesMovedByNumber(t, pool, push.moved)
			})
		}
	})

	t.Run("b5 the close of the pull requests of a task the plan removed", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Testing)
		seedTasks(t, pool, root.Key, root.Generation,
			planTask{id: "a", steps: []string{"tester:a"}, agents: []claim.Agent{"test-a"}, state: record.TaskRunning, place: atStep(1)},
			planTask{id: "b", steps: []string{"implementer:b"}, agents: []claim.Agent{"impl-b"}, state: record.TaskStopped, place: atStep(1)},
			planTask{id: "z", steps: []string{"reviewer:z"}, agents: []claim.Agent{"rev-z"}, needs: []string{"a", "b"}, state: record.TaskWaiting})
		seedTaskBranch(t, pool, root.Key, "a", "legion/LEGION-208")
		var bBranch string
		seedRecord(t, pool, func(tx pgx.Tx) (err error) {
			bBranch, err = record.NewStore().AssignTaskBranch(t.Context(), tx, root.Key, root.Generation, "b")
			return err
		})
		for _, pr := range []struct {
			number int
			base   string
		}{{2, "main"}, {3, bBranch + "-ci-base"}} {
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: root.Key, Repo: "sjawhar/legion", Number: pr.number,
				Branch: bBranch, Base: pr.base, HeadSHA: "head-b", Failing: []string{}})
		}

		mustTake(t, "version 2", applyFacts(t, pool, engine)("version-2", laterVersion(t, root.Key, root.Generation, 2,
			keyed(t, "a", "", []string{"tester:a"}), keyed(t, "z", "", []string{"reviewer:z"}, "a"))))
		if got, want := closedNumbers(t, pool, root.Key, "Task b was removed from the plan."), []int{2, 3}; !slices.Equal(got, want) {
			t.Errorf("version 2, which removed b, closed the pull requests %v of %s; want %v, each open pull request on b's branch", got, bBranch, want)
		}
		assertRowsGone(t, pool, "version 2 removed b", 2, 3)
	})

	t.Run("b5 the close of the pull requests of an earlier run", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := rootRunAgain(t, pool)
		// Generation 1 ran before plans, on the issue's branch, which held #1 onto main and #2 onto
		// another base. The owner gave the plan of generation 2 before its first step.
		for _, pr := range []struct {
			number int
			base   string
		}{{1, "main"}, {2, secondBase}} {
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: root.Key, Repo: "sjawhar/legion", Number: pr.number,
				Branch: sharedBranch, Base: pr.base, HeadSHA: "head-" + sharedBranch, Failing: []string{}})
		}
		seedTasks(t, pool, root.Key, root.Generation,
			planTask{id: "design", steps: []string{"planner"}, agents: []claim.Agent{"planner"}, state: record.TaskWaiting},
			planTask{id: "impl", steps: []string{"implementer"}, agents: []claim.Agent{"implementer"}, needs: []string{"design"}, state: record.TaskWaiting})

		firstStepOf(t, pool, engine, root.Key)

		if got, want := closedNumbers(t, pool, root.Key, runAgainComment), []int{1, 2}; !slices.Equal(got, want) {
			t.Errorf("the first step of generation 2 closed the pull requests %v of the run before; want %v, each open pull request on %s", got, want, sharedBranch)
		}
		assertRowsGone(t, pool, "the first step of generation 2", 1, 2)
	})
}
