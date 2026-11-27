package workflow

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// ownPR is the own pull request of task a of these tests, sjawhar/legion#42 on the issue's own
// branch, as a step's text names it.
const ownPR = "sjawhar/legion#42"

// cleanMergeState records GitHub's merge state of pull request number of repo as clean, as a read
// of a head with nothing against it leaves it, so an approval standing at its head ends its part
// of the round (classify.MergeOutcomeOf): the daemon's read of a linked pull request records no
// merge state.
func cleanMergeState(t *testing.T, pool *pgxpool.Pool, repo string, number int) {
	t.Helper()
	if tag, err := pool.Exec(t.Context(), "update pull_requests set merge_state = 'clean' where repo = $1 and number = $2", repo, number); err != nil ||
		tag.RowsAffected() != 1 {
		t.Fatalf("record %s#%d clean: %v (%d rows)", repo, number, err, tag.RowsAffected())
	}
}

// personsReview is a review of pull request number of repo at head, in state, by a person with
// write access to the repository, which decides a round at the head GitHub delivers it with
// (refusesRound, reviewHead), submitted at the minute id of the test's hour, id being its GitHub id.
func personsReview(repo string, number int, head, state string, id int64) intake.PullRequestReview {
	return intake.PullRequestReview{Repo: repo, Number: number, ID: id, State: state, CommitID: head, Author: "a-writer", AuthorCanWrite: true,
		Body: "review " + state, SubmittedAt: time.Date(2026, 11, 17, 9, int(id), 0, 0, time.UTC)}
}

// resumeOf is the resume task of the step task of key stands at (Engine.ResumeTask), the text a
// worker of that step resumed in a fresh session is given.
func resumeOf(t *testing.T, pool *pgxpool.Pool, engine *Engine, key, task string) string {
	t.Helper()
	var text string
	onTask(t, pool, engine, key, task, func(ctx context.Context, tx pgx.Tx, s TaskScope) (err error) {
		text, err = engine.ResumeTask(ctx, tx, s)
		return err
	})
	return text
}

// namesBoth fails unless text names ownPR, the task's own pull request, and then linkedPR, which
// the issue links.
func namesBoth(t *testing.T, who, text string) {
	t.Helper()
	own, linked := strings.Index(text, ownPR), strings.Index(text, linkedPR)
	if own < 0 || linked < 0 || linked < own {
		t.Fatalf("the %s's text = %q, want it to name %s, its task's own pull request, and after it %s, which the issue links", who, text, ownPR, linkedPR)
	}
}

// Every worker's step text names each open pull request of its step's scope as owner/name#number,
// its own task's first (dispatch://INFRA-5910 D6, decision 21, r1). LEGION-208's one task a runs
// `planner implementer tester reviewer acceptance:owner` and merges, with its own pull request
// sjawhar/legion#42 on the issue's branch, and the issue links acme/deploy#6990, a pull request of
// a second repository, which a's scope works (decision 6). One row for each worker: the planner,
// the implementer, the tester, the reviewer, the retro and the merger, each resumed at its step
// (Engine.ResumeTask, composed by the standing every start and resume reads); the merger started at
// the retro's completion; and the architect's acceptance, whose text is the phase-finished notice
// of the move into accepting (the owner's acceptance starts no worker, so that notice is what hands
// the architect its acceptance): each names both, its task's own first.
//
// The fault it catches is a step text that names its task's pull request alone (its control: the
// standing's pull request part reading the task's own pull request and nothing else, as before the
// set, under which no row names acme/deploy#6990), or one that names a pull request by its number
// alone, which names neither of two pull requests of two repositories.
func TestEachRolesTaskNamesEachOpenPullRequest(t *testing.T) {
	t.Parallel()
	steps := []string{"planner", "implementer", "tester", "reviewer", "acceptance:owner"}
	seed := func(t *testing.T, at phase.Phase, place record.TaskPlace) (*pgxpool.Pool, *Engine, record.Issue) {
		t.Helper()
		pool, engine := migratedPool(t), textEngine()
		root := stepRoot(t, pool, at)
		seedTasksAt(t, pool, root.Key, root.Generation, taskAt{id: "a", steps: steps, state: record.TaskRunning, place: place, number: 42, merges: true})
		linkDeploy(t, pool, applyFacts(t, pool, engine), root.Key, "", 2, "fix/deploy")
		return pool, engine, root
	}
	for _, row := range []struct {
		who   string
		at    phase.Phase
		place record.TaskPlace
	}{
		{"planner", phase.Planning, atStep(1)},
		{"implementer", phase.Implementing, atStep(2)},
		{"tester", phase.Testing, atStep(3)},
		{"reviewer", phase.Reviewing, atStep(4)},
		{"retro", phase.Retro, atRetro},
		{"merger", phase.Merging, atMerge},
	} {
		t.Run("the "+row.who+", resumed", func(t *testing.T) {
			t.Parallel()
			pool, engine, root := seed(t, row.at, row.place)
			namesBoth(t, "resumed "+row.who, resumeOf(t, pool, engine, root.Key, "a"))
		})
	}

	t.Run("the merger, started at the retro's completion", func(t *testing.T) {
		t.Parallel()
		pool, engine, root := seed(t, phase.Retro, atRetro)
		taskMoves(t, pool, engine, root.Key, "a", TriggerRetroCompleted, "")
		namesBoth(t, "started merger", lastStart(t, pool, root.Key, claim.RoleMerger))
	})

	t.Run("the architect's acceptance", func(t *testing.T) {
		t.Parallel()
		pool, engine, root := seed(t, phase.Reviewing, atStep(4))
		taskMoves(t, pool, engine, root.Key, "a", TriggerReviewApproved, "")
		assertTask(t, tasksOf(t, pool, engine, root.Key), "a", record.TaskRunning, atStep(5))
		finished := noticesOf(t, pool, root.Key, "phase-finished")
		if len(finished) == 0 {
			t.Fatalf("the move into accepting told the architect nothing, want the phase-finished notice of the reviewer's step")
		}
		reason := finished[len(finished)-1].Reason
		namesBoth(t, "architect's acceptance notice", reason)
		if want := "Pull request: " + ownPR + " at head-a; " + linkedPR + " at head-6990 (linked on the issue)."; !strings.Contains(reason, want) {
			t.Fatalf("the architect's acceptance notice = %q, want it to hold %q: each open pull request of the issue at its head", reason, want)
		}
	})
}

// reviewingWithLink is LEGION-208 in reviewing, its one task a (`implementer reviewer`, merging)
// at its reviewer's step with its own pull request sjawhar/legion#42 open at head-a, clean, and the
// issue linking acme/deploy#6990, open at head-6990 on fix/deploy and clean.
func reviewingWithLink(t *testing.T) (*pgxpool.Pool, *Engine, func(string, intake.Fact) intake.Result, record.Issue) {
	t.Helper()
	pool, engine := migratedPool(t), returnEngine()
	root := stepRoot(t, pool, phase.Reviewing)
	seedTasksAt(t, pool, root.Key, root.Generation,
		taskAt{id: "a", steps: []string{"implementer", "reviewer"}, state: record.TaskRunning, place: atStep(2), number: 42, merges: true})
	apply := applyFacts(t, pool, engine)
	linkDeploy(t, pool, apply, root.Key, "", 2, "fix/deploy")
	cleanMergeState(t, pool, deployLink.Repo, deployLink.Number)
	return pool, engine, apply, root
}

// decisionOf fails unless pr's own decision is state at head (record.PullRequest.ReviewDecision),
// state "" meaning none.
func decisionOf(t *testing.T, pool *pgxpool.Pool, repo string, number int, state, head, after string) {
	t.Helper()
	pr := pullRequestOf(t, pool, repo, number)
	switch {
	case pr == nil:
		t.Fatalf("after %s the record holds no %s#%d", after, repo, number)
	case state == "" && pr.ReviewDecision != nil && pr.ReviewDecision.State != "":
		t.Fatalf("after %s %s#%d holds the decision %+v, want none", after, repo, number, *pr.ReviewDecision)
	case state != "" && (pr.ReviewDecision == nil || pr.ReviewDecision.State != state || pr.ReviewDecision.Head != head):
		t.Fatalf("after %s %s#%d holds the decision %+v, want %s at %s", after, repo, number, pr.ReviewDecision, state, head)
	}
}

// Each open pull request of a review round is reviewed at its own head, and the merger is asked to
// merge each whose review allows it (dispatch://INFRA-5910 D6, decisions 6 and 7; c-roles' first
// test). LEGION-208's task a reviews its own sjawhar/legion#42 at head-a, and the issue links
// acme/deploy#6990 at head-6990, which a's scope works. Each row is a record of its own:
//   - each head has a decision of its own, and the issue leaves reviewing only when each open head
//     has an approving review standing, whichever is approved first: one approval leaves a at its
//     reviewer's step, the second moves it on to its retro;
//   - a request for changes on one of them rejects the round, though the other is approved;
//   - a push to the head of the linked pull request, its synchronize or a push to its branch, takes
//     back the decision it holds, as a push takes back an approval of the task's own head: an
//     approval of the linked head, then the push, then an approval of the task's own pull request,
//     and the round is not approved; the linked pull request's own decision is cleared (a new run
//     clears it too, record.Store.ClearGeneration);
//   - a head two open pull requests share is one review of each: the issue also links
//     sjawhar/legion#43, another pull request on a's branch at a's head, and one approval of that
//     head on #42 approves both;
//   - the merger's task names each open pull request of its scope, and asks it to merge one whose
//     approval stands; a pull request linked after the review, with no approval standing, is named
//     and not asked to merge;
//   - a CI settlement of the linked pull request wakes the released worker of the scope task's step
//     (wakeOnSettlement);
//   - a plan of two tasks a and b, each merging by itself, where the linked pull request is worked
//     in the scope of the issue's merge, the first task that merges and is not done, a: b's review
//     ends on its own pull request alone, and a's waits for the linked one too.
//
// The faults it catches: the decision held on the reviewer's row for one pull request, under which
// the first approval, of either pull request, moves the issue on and the request for changes on the
// second finds it gone; the merger's task that names its task's pull request alone; and a push to
// a linked head that leaves the decision of the head before it on the linked pull request's row
// (its control: the synchronize and the push writing the row with its decision as it was, under
// which the approval of head-6990 is still held after the push).
func TestEachOpenPullRequestIsReviewedAndMerged(t *testing.T) {
	t.Parallel()
	approveOwn := personsReview("sjawhar/legion", 42, "head-a", "approved", 1)
	approveLinked := personsReview(deployLink.Repo, deployLink.Number, "head-6990", "approved", 2)

	for _, order := range [][]intake.PullRequestReview{{approveOwn, approveLinked}, {approveLinked, approveOwn}} {
		first, second := order[0], order[1]
		t.Run("each head has its own decision, approved first "+prName(first.Repo, first.Number), func(t *testing.T) {
			t.Parallel()
			pool, engine, apply, root := reviewingWithLink(t)
			mustSpend(t, "the first approval", apply("first", first))
			assertTask(t, tasksOf(t, pool, engine, root.Key), "a", record.TaskRunning, atStep(2))
			if first.Repo == deployLink.Repo {
				decisionOf(t, pool, deployLink.Repo, deployLink.Number, "approved", "head-6990", "the approval of "+linkedPR)
				if d := phaseRowOf(t, pool, claim.RoleReviewer).Decision; d != nil && d.State != "" {
					t.Fatalf("after the approval of %s the round's decision of %s = %+v, want none", linkedPR, ownPR, *d)
				}
			} else {
				decisionOf(t, pool, deployLink.Repo, deployLink.Number, "", "", "the approval of "+ownPR)
			}
			mustSpend(t, "the second approval", apply("second", second))
			assertTask(t, tasksOf(t, pool, engine, root.Key), "a", record.TaskRunning, atRetro)
			decisionOf(t, pool, deployLink.Repo, deployLink.Number, "approved", "head-6990", "both approvals")
			if d := reviewerRow(t, pool).Decision; d == nil || d.State != "approved" || d.Head != "head-a" {
				t.Fatalf("after both approvals the round's decision of %s = %+v, want approved at head-a", ownPR, d)
			}
		})
	}

	t.Run("a request for changes on one rejects the round", func(t *testing.T) {
		t.Parallel()
		pool, engine, apply, root := reviewingWithLink(t)
		mustSpend(t, "the approval of "+ownPR, apply("approve-own", approveOwn))
		mustSpend(t, "the request for changes on "+linkedPR, apply("changes-linked",
			personsReview(deployLink.Repo, deployLink.Number, "head-6990", "changes_requested", 3)))
		assertTask(t, tasksOf(t, pool, engine, root.Key), "a", record.TaskRunning, atStep(1))
	})

	for _, moved := range []struct {
		name string
		fact intake.Fact
	}{
		{"its head's synchronize", intake.PullRequestSynchronized{Repo: deployLink.Repo, Number: deployLink.Number, Branch: "fix/deploy",
			Before: "head-6990", HeadSHA: "head-6990-b", Sender: "a-writer", UpdatedAt: linkedAt.Add(time.Hour)}},
		{"a push to its branch", intake.Push{Repo: deployLink.Repo, Branch: "fix/deploy", Before: "head-6990", After: "head-6990-b",
			Pusher: "a-writer", Sender: "a-writer"}},
	} {
		t.Run("a push to the linked head takes back its decision, by "+moved.name, func(t *testing.T) {
			t.Parallel()
			pool, engine, apply, root := reviewingWithLink(t)
			mustSpend(t, "the approval of "+linkedPR, apply("approve-linked", approveLinked))
			mustSpend(t, moved.name, apply("moved", moved.fact))
			decisionOf(t, pool, deployLink.Repo, deployLink.Number, "", "", moved.name+" of "+linkedPR)
			mustSpend(t, "the approval of "+ownPR, apply("approve-own", approveOwn))
			assertTask(t, tasksOf(t, pool, engine, root.Key), "a", record.TaskRunning, atStep(2))
		})
	}

	t.Run("a head two pull requests share is one review of each", func(t *testing.T) {
		t.Parallel()
		pool, engine, apply, root := reviewingWithLink(t)
		shared := intake.PullRequestLink{Repo: "sjawhar/legion", Number: 43}
		mustSpend(t, "the link of #43", apply("link-43", linkEvent(t, pool, root.Key, "", 3, deployLink, shared)))
		read := linkRead(root.Key, shared.Repo, shared.Number, record.PullRequestOpen, "legion/LEGION-208")
		read.HeadSHA, read.Base = "head-a", "legion/LEGION-208-ci-base"
		mustSpend(t, "the read of #43", apply("read-43", read))
		cleanMergeState(t, pool, shared.Repo, shared.Number)
		mustSpend(t, "the approval of "+linkedPR, apply("approve-linked", approveLinked))
		mustSpend(t, "the approval of head-a on "+ownPR, apply("approve-own", approveOwn))
		decisionOf(t, pool, shared.Repo, shared.Number, "approved", "head-a", "the approval of head-a on "+ownPR)
		assertTask(t, tasksOf(t, pool, engine, root.Key), "a", record.TaskRunning, atRetro)
	})

	t.Run("the merger is asked to merge each pull request whose approval stands", func(t *testing.T) {
		t.Parallel()
		pool, engine, apply, root := reviewingWithLink(t)
		mustSpend(t, "the approval of "+ownPR, apply("approve-own", approveOwn))
		mustSpend(t, "the approval of "+linkedPR, apply("approve-linked", approveLinked))
		taskMoves(t, pool, engine, root.Key, "a", TriggerRetroCompleted, "")
		merger := lastStart(t, pool, root.Key, claim.RoleMerger)
		namesBoth(t, "started merger", merger)
		if want := linkedPR + " (linked on the issue; an approval stands at its head, so merge it too)"; !strings.Contains(merger, want) {
			t.Fatalf("the merger's task = %q, want it to hold %q", merger, want)
		}

		late := intake.PullRequestLink{Repo: deployLink.Repo, Number: 7001}
		mustSpend(t, "the link of acme/deploy#7001", apply("link-7001", linkEvent(t, pool, root.Key, "", 3, deployLink, late)))
		mustSpend(t, "the read of acme/deploy#7001", apply("read-7001", linkRead(root.Key, late.Repo, late.Number, record.PullRequestOpen, "fix/late")))
		resumed := resumeOf(t, pool, engine, root.Key, "a")
		if want := "acme/deploy#7001 (linked on the issue; no approval stands at its head, so do not merge it)"; !strings.Contains(resumed, want) {
			t.Fatalf("the merger's task after the late link = %q, want it to hold %q", resumed, want)
		}
	})

	t.Run("a settlement of the linked pull request wakes the scope task's step", func(t *testing.T) {
		t.Parallel()
		rig := newWakeRig(t)
		seedTasks(t, rig.pool, "LEGION-208", wakeIssueGeneration,
			planTask{id: "a", steps: []string{"implementer", "reviewer"}, agents: []claim.Agent{"", ""}, state: record.TaskRunning, place: atStep(1)})
		markMerges(t, rig.pool, "LEGION-208", "a")
		seedTaskBranch(t, rig.pool, "LEGION-208", "a", "legion/LEGION-208")
		linkDeploy(t, rig.pool, rig.apply, "LEGION-208", "", 2, "fix/deploy")
		rig.apply("settled-6990", intake.PullRequestChecks{Repo: deployLink.Repo, Number: deployLink.Number, HeadSHA: "head-6990", Generation: 1,
			Snapshot: "head-6990-snapshot", CheckRuns: []record.AttemptRun{{Name: "build", ID: 1}}, Failing: []string{"build"}})
		oneWake(t, rig.pool, claim.RoleImplementer, pods.WakeChecks, "head-6990", "build")
	})

	t.Run("a plan of two tasks works the linked pull request in the scope of the issue's merge", func(t *testing.T) {
		t.Parallel()
		pool, engine := migratedPool(t), returnEngine()
		root := stepRoot(t, pool, phase.Reviewing)
		seedTasksAt(t, pool, root.Key, root.Generation,
			taskAt{id: "a", steps: []string{"implementer", "reviewer"}, state: record.TaskRunning, place: atStep(2), number: 42, merges: true},
			taskAt{id: "b", steps: []string{"implementer", "reviewer"}, state: record.TaskRunning, place: atStep(2), number: 45, merges: true})
		apply := applyFacts(t, pool, engine)
		linkDeploy(t, pool, apply, root.Key, "", 2, "fix/deploy")
		cleanMergeState(t, pool, deployLink.Repo, deployLink.Number)

		mustSpend(t, "the approval of b's sjawhar/legion#45", apply("approve-b", personsReview("sjawhar/legion", 45, "head-b", "approved", 3)))
		assertTask(t, tasksOf(t, pool, engine, root.Key), "b", record.TaskRunning, atRetro)
		mustSpend(t, "the approval of a's "+ownPR, apply("approve-a", approveOwn))
		assertTask(t, tasksOf(t, pool, engine, root.Key), "a", record.TaskRunning, atStep(2))
		mustSpend(t, "the approval of "+linkedPR, apply("approve-linked", personsReview(deployLink.Repo, deployLink.Number, "head-6990", "approved", 4)))
		assertTask(t, tasksOf(t, pool, engine, root.Key), "a", record.TaskRunning, atRetro)
	})
}

// Each merge of an issue opens a production check of its own, with its repository, its number and
// its merge commit, and a second merge does not replace the first check (dispatch://INFRA-5910 D6,
// decision 6; c-roles' second test). LEGION-208's task a (`implementer tester`, its agents named
// implementer and tester) merges its own sjawhar/legion#42, and the issue links acme/deploy#6990,
// a pull request of a second repository, under `production_check: tester`. In either order of the
// two merges: two checks stand after both, each opened at its own merge, with its repository,
// number and merge commit, the first not replaced by the second; the check of the linked merge is
// a's, claimed by the agent of a's tester (checkClaim over the scope's task); and each check's first
// look's task names its own merge, as owner/name#number with its merge commit, and not the other's.
//
// The fault it catches is a linked merge that opens no check, as on the stack, whose intake records
// no pull request of another repository (its control: mergedNotOwn with no openCheck, under which
// one check stands after both merges), and a linked merge's check claimed by the checking role's own
// claim on the issue rather than by its scope's agent (its control: openCheck keeping the pull
// request's own task, "" for a linked pull request).
func TestEachMergeOpensItsOwnProductionCheck(t *testing.T) {
	t.Parallel()
	ownMerge := intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 42, MergeSHA: "merge-a", HeadSHA: "head"}
	for _, linkedFirst := range []bool{true, false} {
		name := "the task's merge first"
		if linkedFirst {
			name = "the linked merge first"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pool := migratedPool(t)
			engine := returnEngine()
			clock := &checkClock{now: stepClock}
			engine.cfg.Clock, engine.cfg.ProductionCheckBound = clock.Now, checkBound
			root := stepRoot(t, pool, phase.Merging)
			seedTasks(t, pool, root.Key, root.Generation,
				planTask{id: "a", steps: []string{"implementer", "tester"}, agents: []claim.Agent{"implementer", "tester"}, state: record.TaskRunning, place: atMerge})
			markMerges(t, pool, root.Key, "a")
			seedTaskBranch(t, pool, root.Key, "a", claim.IssueBranch(root.Key))
			apply := applyFacts(t, pool, engine)
			linkDeploy(t, pool, apply, root.Key, "", 2, "fix/deploy")
			tester := agentClaim(t, root.Key, "tester")

			merges := []struct {
				id   string
				fact intake.PullRequestMerged
			}{{"merged-42", ownMerge}, {"merged-6990", deployMerged}}
			if linkedFirst {
				merges[0], merges[1] = merges[1], merges[0]
			}
			mustSpend(t, "the first merge", apply(merges[0].id, merges[0].fact))
			checks := checksOfIssue(t, checkRig{pool: pool}, root.Key)
			if len(checks) != 1 || !checks[0].Open() {
				t.Fatalf("the checks of %s after the first merge = %+v, want one open", root.Key, checks)
			}
			first := checks[0]
			clock.now = clock.now.Add(time.Minute)
			mustSpend(t, "the second merge", apply(merges[1].id, merges[1].fact))
			closedDone(t, pool, root.Key, "the second merge")

			checks = checksOfIssue(t, checkRig{pool: pool}, root.Key)
			if len(checks) != 2 || !checks[1].OpenedAt.Equal(first.OpenedAt) || checks[1].MergeSHA != first.MergeSHA {
				t.Fatalf("the checks of %s after both merges = %+v, want two, the first, of %s, as it opened at %s", root.Key, checks, first.MergeSHA, first.OpenedAt)
			}
			byMerge := map[string]record.ProductionCheck{}
			for _, check := range checks {
				if !check.Open() || check.Generation != root.Generation || check.Task != "a" {
					t.Fatalf("the check of %s = %+v, want it open, of run %d and of task a", check.MergeSHA, check, root.Generation)
				}
				byMerge[check.MergeSHA] = check
			}
			linked, own := byMerge["merge-6990"], byMerge["merge-a"]
			if linked.Repo != deployLink.Repo || linked.Number != deployLink.Number || own.Repo != "sjawhar/legion" || own.Number != 42 {
				t.Fatalf("the checks of %s = %+v, want one of %s at merge-6990 and one of %s at merge-a", root.Key, checks, linkedPR, ownPR)
			}

			starts := lookStarts(t, pool, root.Key)
			if len(starts) != 2 {
				t.Fatalf("the look starts of %s = %+v, want the first look of each check", root.Key, starts)
			}
			for _, start := range starts {
				check, names, other := own, ownPR+", merge commit merge-a", linkedPR
				if start.CheckOpenedAt.Equal(linked.OpenedAt) {
					check, names, other = linked, linkedPR+", merge commit merge-6990", ownPR
				}
				if !start.CheckOpenedAt.Equal(check.OpenedAt) || start.Claim != tester {
					t.Fatalf("the look start %+v, want one of the check opened at %s, naming the claim of a's tester %s", start, check.OpenedAt, tester)
				}
				if !strings.Contains(start.Task, "Repository "+check.Repo+", pull request "+names+".") || strings.Contains(start.Task, other) {
					t.Fatalf("the task of the look of %s = %q, want it to name its own merge, %s, and not %s", check.MergeSHA, start.Task, names, other)
				}
			}
		})
	}
}
