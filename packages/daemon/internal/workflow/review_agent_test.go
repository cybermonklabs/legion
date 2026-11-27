package workflow

import (
	"context"
	"fmt"
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
	"github.com/sjawhar/legion/daemon/internal/reviewround"
)

// Two commits a review of the task read: step 2's review, and the review of the reviewer at
// step 4.
const (
	stepTwoRead  = "2222222222222222222222222222222222222222"
	stepFourRead = "4444444444444444444444444444444444444444"
)

// recordReviewRead writes commit as the head the newest deciding review of the reviewer's step
// at place read (record.PhaseRow.ReviewedHead), as the workflow's review writes it for a deciding
// review of the step's own session.
func recordReviewRead(t *testing.T, pool *pgxpool.Pool, key string, place int, commit string) {
	t.Helper()
	execSQL(t, pool, `update phases set reviewed_head = $1 where issue = $2 and role = 'reviewer' and step_place = $3`, commit, key, place)
}

// reviewerStepFour walks LEGION-208 to its reviewer's step 4 for the round-th time, the reviewer
// at step 2 having read stepTwoRead and tester completing step 3, and answers step 4's row.
func reviewerStepFour(t *testing.T, pool *pgxpool.Pool, engine *Engine, tester claim.Token, round int) record.PhaseRow {
	t.Helper()
	key := "LEGION-208"
	if round == 1 {
		move(t, pool, engine, key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	}
	completesAs(t, pool, engine, claim.RoleImplementer, agentToken(t, "implementer"), "implement", round, "head")
	recordReviewRead(t, pool, key, 2, stepTwoRead)
	move(t, pool, engine, key, TriggerReviewApproved, record.TaskPlace{}, record.PhaseRow{})
	completesAs(t, pool, engine, claim.RoleTester, tester, "test", round, "head")
	assertWalksAt(t, pool, engine, key, phase.Reviewing, stepPlace(4))
	return rolesRows(t, pool, engine, key, claim.RoleReviewer)[4]
}

// A reviewer step's first round is named the commit of its agent's own reviewer row of the task,
// at any of its steps, else none, so a step whose agent has no reviewer row of its own there reads
// the whole change at its first round. In each plan below, step 4 is key s's first reviewer step,
// and step 2's review, another agent's, read a commit: step 4's first task names no commit of its
// own, `Last reviewed commit: none.`, and names step 2's commit as another agent's before it. Its
// row holds no commit. After a review of key s's own read a commit, a return to step 1 and the walk
// back to step 4, its task names that commit, its own, and no other agent's. Step 2's agent is the
// reviewer's own in the first plan, and in the second agent a, which runs step 5 too. Before, step
// 4's first round was named step 2's commit as its own, so key s read only the change past a commit
// another agent approved.
func TestAnAgentsFirstReviewerStepReadsTheWholeChangeAtItsFirstRound(t *testing.T) {
	for _, steps := range [][]string{
		{"implementer:a", "reviewer", "tester:s", "reviewer:s"},
		{"implementer:a", "reviewer:a", "tester:s", "reviewer:s", "reviewer:a"},
	} {
		t.Run(strings.Join(steps, " "), func(t *testing.T) {
			pool := migratedPool(t)
			engine := textEngine()
			root := stepRoot(t, pool, phase.Admitted)
			seedStepsPlan(t, pool, root.Key, root.Generation, steps...)
			keyS := agentToken(t, "tester")

			first := reviewerStepFour(t, pool, engine, keyS, 1)
			other := reviewround.AnotherAgentsSentence(stepTwoRead)
			if first.Claim != keyS || first.ReviewedHead != "" ||
				!strings.HasSuffix(first.EntryTask, other+reviewround.Sentences("", first.Entry)) {
				t.Fatalf("step 4's first round = claim %s, commit %q, task %q; want key s's, no commit of its own, and a task ending %q",
					first.Claim, first.ReviewedHead, first.EntryTask, other+reviewround.Sentences("", first.Entry))
			}

			recordReviewRead(t, pool, root.Key, 4, stepFourRead)
			move(t, pool, engine, root.Key, TriggerReviewRejected, stepPlace(1), record.PhaseRow{})
			second := reviewerStepFour(t, pool, engine, keyS, 2)
			if !strings.HasSuffix(second.EntryTask, reviewround.Sentences(stepFourRead, second.Entry)) ||
				strings.Contains(second.EntryTask, reviewround.AnotherAgentLabel) {
				t.Fatalf("step 4's second round task = %q, want it to end %q and name no other agent's commit",
					second.EntryTask, reviewround.Sentences(stepFourRead, second.Entry))
			}
		})
	}
}

// A reviewer step's first round is named the commit of its agent's own reviewer row of the task,
// at any of its steps: in `implementer:a reviewer:r tester reviewer:r`, steps 2 and 4 are key r's,
// the reviewer's claim, so step 4's first round is named step 2's commit as its own, and no other
// agent's. (Two steps of one role with no key are two agents, which this release refuses as a
// plan.)
func TestAReviewerStepsFirstRoundIsNamedItsAgentsOwnCommit(t *testing.T) {
	pool := migratedPool(t)
	engine := textEngine()
	root := stepRoot(t, pool, phase.Admitted)
	seedStepsPlan(t, pool, root.Key, root.Generation, "implementer:a", "reviewer:r", "tester", "reviewer:r")

	first := reviewerStepFour(t, pool, engine, agentToken(t, "tester"), 1)
	if first.Claim != agentToken(t, "reviewer") || first.ReviewedHead != stepTwoRead ||
		!strings.HasSuffix(first.EntryTask, reviewround.Sentences(stepTwoRead, first.Entry)) ||
		strings.Contains(first.EntryTask, reviewround.AnotherAgentLabel) {
		t.Fatalf("step 4's first round = claim %s, commit %q, task %q; want the reviewer's, step 2's commit, and a task ending %q",
			first.Claim, first.ReviewedHead, first.EntryTask, reviewround.Sentences(stepTwoRead, first.Entry))
	}
}

// reviewerStepFive walks `implementer:a reviewer:a tester:s reviewer:s reviewer:a` of LEGION-208 to
// step 5, agent a's second reviewer step, agent a's review at step 2 having read stepTwoRead and key
// s's at step 4 stepFourRead, and answers the reviewer's rows by place.
func reviewerStepFive(t *testing.T, pool *pgxpool.Pool, engine *Engine) map[int]record.PhaseRow {
	t.Helper()
	reviewerStepFour(t, pool, engine, agentToken(t, "tester"), 1)
	recordReviewRead(t, pool, "LEGION-208", 4, stepFourRead)
	move(t, pool, engine, "LEGION-208", TriggerReviewApproved, record.TaskPlace{}, record.PhaseRow{})
	assertWalksAt(t, pool, engine, "LEGION-208", phase.Reviewing, stepPlace(5))
	return rolesRows(t, pool, engine, "LEGION-208", claim.RoleReviewer)
}

// An agent's later reviewer step is named the commit its own earlier reviewer step read, though a
// reviewer step of another agent stands between them: in `implementer:a reviewer:a tester:s
// reviewer:s reviewer:a`, step 5's first task names step 2's commit, agent a's, as its own, and no
// other agent's, and step 5's row holds it. Before, step 5's first run started from the role's row
// now, key s's at step 4, and cleared that row's commit as another agent's, so step 5 named `none`
// and key s's commit, though agent a has a deciding review of its own on the task.
func TestALaterReviewerStepIsNamedItsAgentsOwnCommitPastAnotherAgentsStep(t *testing.T) {
	pool := migratedPool(t)
	engine := textEngine()
	root := stepRoot(t, pool, phase.Admitted)
	seedStepsPlan(t, pool, root.Key, root.Generation, "implementer:a", "reviewer:a", "tester:s", "reviewer:s", "reviewer:a")
	five := reviewerStepFive(t, pool, engine)[5]
	if five.Claim != agentToken(t, "implementer") || five.ReviewedHead != stepTwoRead ||
		!strings.HasSuffix(five.EntryTask, reviewround.Sentences(stepTwoRead, five.Entry)) ||
		strings.Contains(five.EntryTask, reviewround.AnotherAgentLabel) {
		t.Fatalf("step 5's first round = claim %s, commit %q, task %q; want agent a's, step 2's commit, and a task ending %q with no other agent's commit",
			five.Claim, five.ReviewedHead, five.EntryTask, reviewround.Sentences(stepTwoRead, five.Entry))
	}
}

// givesVersion gives version of the plan of root, as its owner does, with task a's steps, and fails
// the test unless it is taken.
func givesVersion(t *testing.T, pool *pgxpool.Pool, engine *Engine, root record.Issue, version int, steps ...string) {
	t.Helper()
	mustTake(t, fmt.Sprintf("version %d", version), applyFacts(t, pool, engine)(planID(root.Key, root.Generation, version),
		ownersPlan(t, root.Key, root.Generation, version, givenTask(t, "Build the widget.", steps...))))
}

// stopsAndGivesVersionTwo stops root's task a and gives version 2 of the plan with steps, which
// starts the same task again at its step 1.
func stopsAndGivesVersionTwo(t *testing.T, pool *pgxpool.Pool, engine *Engine, root record.Issue, steps ...string) {
	t.Helper()
	if result := applyFacts(t, pool, engine)("stop", ownersStop(t, root, "a")); result.Refusal != nil {
		t.Fatalf("the owner's stop was refused: %+v", result.Refusal)
	}
	givesVersion(t, pool, engine, root, 2, steps...)
	assertWalksAt(t, pool, engine, root.Key, phase.Implementing, stepPlace(1))
}

// A reviewer step that a later version of the plan gives to another agent is named that agent's
// own commit, never the commit of the agent that ran the step before: in version 1,
// `implementer:a reviewer:r tester:s reviewer:s`, key s's review at step 4 reads stepFourRead. The
// owner stops task a and gives version 2, `implementer:a reviewer:r tester:s reviewer:a`, which
// starts the same task again at step 1. At step 4, agent a has no reviewer row on the task, so its
// first task names no commit of its own, `Last reviewed commit: none.`, and key r's commit of step
// 2 as another agent's before it, and its row holds no commit. Before, step 4's row kept key s's
// commit when its entry wrote agent a's claim on it, so agent a read only the change past
// stepFourRead. The second row's step 4 names no claim, as the release before writes a step's row,
// whose agent is the role's own (record.RowWorker): its commit is not agent a's either. Before, a
// row with no claim kept its commit for any agent.
func TestAReviewerStepALaterVersionGivesToAnotherAgentIsNamedThatAgentsOwnCommit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		noClaim bool
	}{
		{name: "step 4's row names key s's claim"},
		{name: "step 4's row names no claim, as the release before writes it", noClaim: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := textEngine()
			root := stepRoot(t, pool, phase.Admitted)
			givesVersion(t, pool, engine, root, 1, "implementer:a", "reviewer:r", "tester:s", "reviewer:s")
			keyS := agentToken(t, "tester")
			if four := reviewerStepFour(t, pool, engine, keyS, 1); four.Claim != keyS {
				t.Fatalf("step 4 of version 1 runs under %s, want key s's claim %s", four.Claim, keyS)
			}
			recordReviewRead(t, pool, root.Key, 4, stepFourRead)
			if tc.noClaim {
				execSQL(t, pool, `update phases set claim = '' where issue = $1 and role = 'reviewer' and step_place = 4`, root.Key)
			}
			stopsAndGivesVersionTwo(t, pool, engine, root, "implementer:a", "reviewer:r", "tester:s", "reviewer:a")

			four := reviewerStepFour(t, pool, engine, keyS, 2)
			want := reviewround.AnotherAgentsSentence(stepTwoRead) + reviewround.Sentences("", four.Entry)
			if four.Claim != agentToken(t, "implementer") || four.ReviewedHead != "" || !strings.HasSuffix(four.EntryTask, want) {
				t.Fatalf("step 4 of version 2 = claim %s, commit %q, task %q; want agent a's, no commit of its own, and a task ending %q",
					four.Claim, four.ReviewedHead, four.EntryTask, want)
			}
		})
	}
}

// A reviewer step that a later version of the plan gives to an agent is named that agent's own
// commit on the task, at any of its steps, a later one of the earlier version among them: in
// version 1, `implementer:a reviewer:s tester:s reviewer:a`, agent a's review at step 4 reads
// stepFourRead. The owner stops task a and gives version 2, `implementer:a reviewer:a tester:s
// reviewer:s`, and step 2, whose row key s's runs wrote, passes to agent a: its task names
// stepFourRead as agent a's own and no other agent's commit, and its row holds it. Before, the
// commit was read from agent a's rows at earlier places alone, so step 2 named `none` and no other
// agent's commit, though agent a's own review on the task had read stepFourRead.
func TestAReviewerStepALaterVersionGivesToAnAgentIsNamedItsCommitOfALaterStep(t *testing.T) {
	pool := migratedPool(t)
	engine := textEngine()
	root := stepRoot(t, pool, phase.Admitted)
	givesVersion(t, pool, engine, root, 1, "implementer:a", "reviewer:s", "tester:s", "reviewer:a")
	keyA, keyS := agentToken(t, "implementer"), agentToken(t, "reviewer")
	if four := reviewerStepFour(t, pool, engine, keyS, 1); four.Claim != keyA {
		t.Fatalf("step 4 of version 1 runs under %s, want agent a's claim %s", four.Claim, keyA)
	}
	recordReviewRead(t, pool, root.Key, 4, stepFourRead)
	stopsAndGivesVersionTwo(t, pool, engine, root, "implementer:a", "reviewer:a", "tester:s", "reviewer:s")

	completesAs(t, pool, engine, claim.RoleImplementer, keyA, "implement", 2, "head")
	assertWalksAt(t, pool, engine, root.Key, phase.Reviewing, stepPlace(2))
	two := rolesRows(t, pool, engine, root.Key, claim.RoleReviewer)[2]
	if two.Claim != keyA || two.ReviewedHead != stepFourRead ||
		!strings.HasSuffix(two.EntryTask, reviewround.Sentences(stepFourRead, two.Entry)) ||
		strings.Contains(two.EntryTask, reviewround.AnotherAgentLabel) {
		t.Fatalf("step 2 of version 2 = claim %s, commit %q, task %q; want agent a's, its commit of step 4, and a task ending %q with no other agent's commit",
			two.Claim, two.ReviewedHead, two.EntryTask, reviewround.Sentences(stepFourRead, two.Entry))
	}
}

// wakesClaim brings token's released claim back by a message sent to its session, through the one
// act every wake performs (wakeReleased), and answers the task that wake gives it (WakeTask).
func wakesClaim(t *testing.T, pool *pgxpool.Pool, engine *Engine, token claim.Token) string {
	t.Helper()
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		released, found, err := engine.cfg.Pods.ClaimByToken(ctx, tx, string(token))
		if err != nil || !found || !released.Released() {
			return fmt.Errorf("read the released claim %s: found %t, %v", token, found, err)
		}
		woken, err := engine.wakeReleased(ctx, tx, released, pods.WakeAgentMessage, "its detail", engine.now())
		if err == nil && !woken {
			err = fmt.Errorf("the wake of %s took no wake", token)
		}
		return err
	})
	return WakeTask(pods.WakeAgentMessage, "its detail", StoppedByRelease)
}

// A wake of the reviewer in the first round of a reviewer step of another agent names that other
// agent's commit, as the step's start did: in `implementer:a reviewer tester:s reviewer:s`, key s's
// released claim, woken at step 4, is given a task that ends naming step 2's commit as another
// agent's and then `none` and step 4's entry. The wake carries no stored task, so the queue names
// its round (queuedRow) from the issue's phases rows; given no rows, it would name no other agent's
// commit, which is this test's control.
func TestAWakeInAReviewerStepOfAnotherAgentNamesThatAgentsCommit(t *testing.T) {
	pool := migratedPool(t)
	engine := textEngine()
	root := stepRoot(t, pool, phase.Admitted)
	seedStepsPlan(t, pool, root.Key, root.Generation, "implementer:a", "reviewer", "tester:s", "reviewer:s")
	keyS := agentToken(t, "tester")
	four := reviewerStepFour(t, pool, engine, keyS, 1)
	outboxRan(t, pool)
	engine.cfg.Pods = wakePods(t, pool)
	seedReleasedToken(t, pool, keyS, claim.RoleReviewer, "ses-woken", root.Generation)
	wake := wakesClaim(t, pool, engine, keyS)
	want := wake + reviewround.AnotherAgentsSentence(stepTwoRead) + reviewround.Sentences("", four.Entry)
	if task := onlyReviewerStart(t, pool).Task; task != want {
		t.Fatalf("the woken reviewer of step 4 is given the task %q, want %q", task, want)
	}
}

// A wake of an agent's claim names the commit and the entry of that agent's own row of the role,
// never those of another agent's step that the task stands at: at step 5 of `implementer:a
// reviewer:a tester:s reviewer:s reviewer:a`, agent a's, key s's released reviewer claim, woken, is
// given a task that ends naming its own commit of step 4 and step 4's entry, and no other agent's
// commit. Before, the queue read the role's row now, step 5's, and named agent a's commit and entry
// to key s as its own.
func TestAWakeOfAnEarlierReviewerStepsAgentNamesItsOwnRow(t *testing.T) {
	pool := migratedPool(t)
	engine := textEngine()
	root := stepRoot(t, pool, phase.Admitted)
	seedStepsPlan(t, pool, root.Key, root.Generation, "implementer:a", "reviewer:a", "tester:s", "reviewer:s", "reviewer:a")
	four := reviewerStepFive(t, pool, engine)[4]
	keyS := agentToken(t, "tester")
	outboxRan(t, pool)
	engine.cfg.Pods = wakePods(t, pool)
	seedReleasedToken(t, pool, keyS, claim.RoleReviewer, "ses-woken", root.Generation)
	wake := wakesClaim(t, pool, engine, keyS)
	if want, task := wake+reviewround.Sentences(stepFourRead, four.Entry), onlyReviewerStart(t, pool).Task; task != want {
		t.Fatalf("key s's woken reviewer claim at step 5 is given the task %q, want %q: its own commit of step 4 and step 4's entry", task, want)
	}
}

// Three heads LEGION-208's pull request stands at in turn, each read by a deciding review of agent
// r's.
const (
	firstRead  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	secondRead = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	thirdRead  = "cccccccccccccccccccccccccccccccccccccccc"
)

// standsAt puts LEGION-208's pull request #42 at head, open with nothing against it, as a push
// leaves it.
func standsAt(t *testing.T, pool *pgxpool.Pool, head string) {
	t.Helper()
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42,
		Branch: "legion/LEGION-208", HeadSHA: head, Failing: []string{}})
}

// reviewsAs submits, as the review App from session, a review in state of head, where the pull
// request stands, its Legion footer naming head and the entry the reviewer's row stands on: a
// deciding review of the reviewer's own session, which records head as the commit it read
// (workflow's review) and, at a clean head, ends the round.
func reviewsAs(t *testing.T, pool *pgxpool.Pool, engine *Engine, id int64, state, head, session string) {
	t.Helper()
	standsAt(t, pool, head)
	entry := roleRowNow(t, pool, engine, "LEGION-208", claim.RoleReviewer).Entry
	review := intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: id, State: state, CommitID: head, Author: testReviewApp,
		Body: reviewBodyOfEntry("the review", session, head, entry), SubmittedAt: stepClock.Add(time.Duration(id) * time.Minute)}
	if result := applyFacts(t, pool, engine)(fmt.Sprintf("review-%d", id), review); result.Refusal != nil {
		t.Fatalf("the review %d was refused: %+v", id, result.Refusal)
	}
}

// agentHolds fails the test, going on, unless each of agent's rows of role on LEGION-208 holds want
// as the commit its reviews read: the rows of one agent never differ. Each caller's agent runs two
// steps of role, so fewer than two rows of it fail too: a check over fewer compares nothing.
func agentHolds(t *testing.T, pool *pgxpool.Pool, engine *Engine, role claim.Role, agent claim.Token, want, when string) {
	t.Helper()
	held := map[int]string{}
	for place, row := range rolesRows(t, pool, engine, "LEGION-208", role) {
		if row.Claim == agent {
			held[place] = row.ReviewedHead
		}
	}
	if len(held) < 2 {
		t.Errorf("%s, the %s rows of %s are %v; want one at each of its two steps", when, role, agent, held)
		return
	}
	for place, commit := range held {
		if commit != want {
			t.Errorf("%s, the %s rows of %s hold %v; want %s at each, place %d among them", when, role, agent, held, want, place)
			return
		}
	}
}

// agentRReviewsAtStepFourAndReturns walks `implementer:a reviewer:r tester reviewer:r` of LEGION-208
// with deciding reviews of agent r's own: step 2 approves firstRead, step 4 requests changes at
// secondRead, and the task returns to step 1 and walks to step 2 again. It answers step 2's row.
func agentRReviewsAtStepFourAndReturns(t *testing.T, pool *pgxpool.Pool, engine *Engine) record.PhaseRow {
	t.Helper()
	root := stepRoot(t, pool, phase.Admitted)
	seedStepsPlan(t, pool, root.Key, root.Generation, "implementer:a", "reviewer:r", "tester", "reviewer:r")
	keyA, keyR := agentToken(t, "implementer"), agentToken(t, "reviewer")
	seedClaimSession(t, pool, keyR, "ses-r")
	move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	standsAt(t, pool, firstRead)
	completesAs(t, pool, engine, claim.RoleImplementer, keyA, "implement", 1, firstRead)
	reviewsAs(t, pool, engine, 1, "approved", firstRead, "ses-r")
	standsAt(t, pool, firstRead)
	completesAs(t, pool, engine, claim.RoleTester, agentToken(t, "tester"), "test", 1, firstRead)
	assertWalksAt(t, pool, engine, root.Key, phase.Reviewing, stepPlace(4))
	reviewsAs(t, pool, engine, 2, "changes_requested", secondRead, "ses-r")
	assertWalksAt(t, pool, engine, root.Key, phase.Implementing, stepPlace(1))
	agentHolds(t, pool, engine, claim.RoleReviewer, keyR, secondRead, "after step 4's request for changes")
	standsAt(t, pool, secondRead)
	completesAs(t, pool, engine, claim.RoleImplementer, keyA, "implement", 2, secondRead)
	assertWalksAt(t, pool, engine, root.Key, phase.Reviewing, stepPlace(2))
	return rolesRows(t, pool, engine, root.Key, claim.RoleReviewer)[2]
}

// The commit an agent's reviewer step is named is that agent's newest deciding review on the task,
// at whichever of its steps the review was made: in `implementer:a reviewer:r tester reviewer:r`,
// after agent r's step 4 requested changes at secondRead, step 2's next round names secondRead, and
// each row of agent r holds it. Before, each step's row kept the commit of its own step's reviews,
// so step 2's next round named firstRead, its own first round's, an older review of agent r's.
func TestAnEarlierReviewerStepsNextRoundIsNamedItsAgentsNewerCommitOfALaterStep(t *testing.T) {
	pool := migratedPool(t)
	engine := textEngine()
	two := agentRReviewsAtStepFourAndReturns(t, pool, engine)
	if !strings.HasSuffix(two.EntryTask, reviewround.Sentences(secondRead, two.Entry)) || strings.Contains(two.EntryTask, reviewround.AnotherAgentLabel) {
		t.Fatalf("step 2's next round task = %q, want it to end %q, agent r's newest commit, step 4's, and name no other agent's",
			two.EntryTask, reviewround.Sentences(secondRead, two.Entry))
	}
}

// The mirror of the test above: after step 2's next round approves thirdRead, step 4's next round
// names thirdRead, agent r's newest commit, which step 2's review read, and each row of agent r
// holds it. Before, step 4's row kept secondRead, its own round's.
func TestALaterReviewerStepsNextRoundIsNamedItsAgentsNewerCommitOfAnEarlierStep(t *testing.T) {
	pool := migratedPool(t)
	engine := textEngine()
	agentRReviewsAtStepFourAndReturns(t, pool, engine)
	reviewsAs(t, pool, engine, 3, "approved", thirdRead, "ses-r")
	agentHolds(t, pool, engine, claim.RoleReviewer, agentToken(t, "reviewer"), thirdRead, "after step 2's approval")
	standsAt(t, pool, thirdRead)
	completesAs(t, pool, engine, claim.RoleTester, agentToken(t, "tester"), "test", 2, thirdRead)
	assertWalksAt(t, pool, engine, "LEGION-208", phase.Reviewing, stepPlace(4))
	four := rolesRows(t, pool, engine, "LEGION-208", claim.RoleReviewer)[4]
	if !strings.HasSuffix(four.EntryTask, reviewround.Sentences(thirdRead, four.Entry)) {
		t.Fatalf("step 4's next round task = %q, want it to end %q, agent r's newest commit, step 2's", four.EntryTask, reviewround.Sentences(thirdRead, four.Entry))
	}
}

// A role's row of no step, as the release before writes a role's one row, is the role's own
// token's (record.RowWorker): a step's first run that moves it to the step keeps its commit for
// that token, and names none to another key's agent, which reads the whole change. Before, such a
// row kept its commit for any agent.
func TestARowOfNoStepKeepsItsCommitForTheRolesOwnTokenAlone(t *testing.T) {
	issue := record.Issue{Key: "LEGION-208", Project: "LEGION"}
	own, err := stepClaim(issue, issue.Key, claim.RoleReviewer)
	if err != nil {
		t.Fatalf("the reviewer's own token: %v", err)
	}
	noStep := record.PhaseRow{Issue: issue.Key, Role: claim.RoleReviewer, Entry: 3, ReviewedHead: stepFourRead}
	for _, tc := range []struct {
		name  string
		agent claim.Token
		want  string
	}{
		{name: "the role's own token", agent: own, want: stepFourRead},
		{name: "another key's agent", agent: agentToken(t, "implementer")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row, moves, err := stepRunRow(issue, []record.PhaseRow{noStep}, noStep, claim.RoleReviewer, "a", 2, tc.agent)
			if err != nil || !moves || row.StepTask != "a" || row.StepPlace != 2 || row.ReviewedHead != tc.want {
				t.Fatalf("the run of step 2 by %s = step %q/%d, moves %t, commit %q, %v; want step a/2, moved, commit %q",
					tc.agent, row.StepTask, row.StepPlace, moves, row.ReviewedHead, err, tc.want)
			}
		})
	}
}
