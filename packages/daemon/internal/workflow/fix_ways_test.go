package workflow

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The way a fix takes once it ended and no other step of its task runs (afterFix), row by row of
// the table of a review beside a fix, on `implementer tester reviewer`: the step that starts next,
// the head its working copy moves to, and the returns the task counts. Rows 1 and 2 (an approval
// while the fix runs) end the task's last step, so they are held at the task's end and are tested
// with that hold.

// assertWay fails unless the latest fix of LEGION-208's task took way, the issue stands in at, the
// latest start of role there moves its working copy to moveTo, the task counts pass returns, pair
// among them where pair is not "", and the commit the next review names is still the one the
// reviewer's own review read, X (fixAfterReview): the way after a fix moves the branch's head, never
// the commit the reviewer last read, so the next review reads from X to the new head.
func (r *fixRig) assertWay(way int, at phase.Phase, role claim.Role, moveTo string, pass int, pair string) {
	r.t.Helper()
	if fix := r.latestFix(); fix.Way != way {
		r.t.Fatalf("the fix took way %d, want %d: %+v", fix.Way, way, fix)
	}
	assertPhase(r.t, r.pool, at)
	starts := r.startsOf(role)
	if len(starts) == 0 || starts[len(starts)-1].Phase != at || starts[len(starts)-1].MoveTo != moveTo {
		r.t.Fatalf("the %s's starts = %+v, want the latest in %s moving its working copy to %q", role, starts, at, moveTo)
	}
	counts := r.counts()
	if counts.Pass != pass || (pair != "" && counts.BackwardMoves[pair] != 1) {
		r.t.Fatalf("the task counts %+v, want %d returns, %q among them", counts, pass, pair)
	}
	if reviewed := roleRowOf(r.t, r.pool, r.engine, "LEGION-208", claim.RoleReviewer).ReviewedHead; reviewed != fixHead {
		r.t.Fatalf("the commit the next review names is %q, want %s, the one the reviewer's review read", reviewed, fixHead)
	}
}

// reviewedX writes on the reviewer's row that its own review read X, as the review App review of
// the reviewer's session leaves it (record.PhaseRow.ReviewedHead): the review of a step that
// started at X, beside the fix.
func (r *fixRig) reviewedX() {
	r.t.Helper()
	row := roleRowOf(r.t, r.pool, r.engine, "LEGION-208", claim.RoleReviewer)
	row.Issue, row.Role, row.ReviewedHead = "LEGION-208", claim.RoleReviewer, fixHead
	applyInTx(r.t, r.pool, r.engine, func(ctx context.Context, tx pgx.Tx) error { return r.engine.store.PutPhase(ctx, tx, row) })
}

// fixAfterReview is LEGION-208 reviewing on `implementer tester reviewer` with fix 1 for k-ci at
// fixHead: the review's request for changes or approval (trigger) applied while the fix runs, or
// after it ended where afterEnd says so, and the fix ended at end, the head the pull request
// stands at then.
func fixAfterReview(t *testing.T, engine *Engine, trigger TriggerKind, afterEnd bool, end string) *fixRig {
	t.Helper()
	r := newFixRig(t, engine, []string{"implementer", "tester", "reviewer"}, TriggerImplementationReady, TriggerTesterPassed)
	r.fails(fixHead, "k-ci")
	r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})
	if !afterEnd {
		r.reviewedX()
		r.move(trigger, "", reviewReport("the review's report"))
	}
	r.pushed(end)
	if result := r.apply("fix-done", fixCompletion(end)); result.Refusal != nil {
		t.Fatalf("the fix's completion was refused: %+v", result.Refusal)
	}
	if afterEnd {
		r.reviewedX()
		r.move(trigger, "", reviewReport("the review's report"))
	}
	return r
}

var (
	reviewingTesting      = phase.Pair(phase.Reviewing, phase.Testing)
	reviewingImplementing = phase.Pair(phase.Reviewing, phase.Implementing)
	retroTesting          = phase.Pair(phase.Retro, phase.Testing)
)

// fixAtTheEnd is LEGION-208 reviewing on `implementer tester reviewer` with fix 1 for k-ci at
// fixHead running, whose review approved: the task held for its fix at its end.
func fixAtTheEnd(t *testing.T, engine *Engine) *fixRig {
	t.Helper()
	r := newFixRig(t, engine, []string{"implementer", "tester", "reviewer"}, TriggerImplementationReady, TriggerTesterPassed)
	r.fails(fixHead, "k-ci")
	r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})
	r.reviewedX()
	r.move(TriggerReviewApproved, "", reviewReport("approved"))
	r.assertEndHold(record.HoldForFix)
	return r
}

// fixEnds is the fix's own completion at end, the head the pull request stands at then.
func (r *fixRig) fixEnds(end string) {
	r.t.Helper()
	r.pushed(end)
	if result := r.apply("fix-done", fixCompletion(end)); result.Refusal != nil {
		r.t.Fatalf("the fix's completion was refused: %+v", result.Refusal)
	}
}

// Row 1: an approval while the fix runs, and the fix made new commits: the approval held the task
// for its fix at its end, and the fix's end returns it from there to the step after the fix, the
// tester, at the new head. One return, under retro-testing.
func TestRow1ApprovalWhileTheFixRunsWithNewCommits(t *testing.T) {
	r := fixAtTheEnd(t, returnEngine())
	r.fixEnds(headTwo)
	r.assertWay(2, phase.Testing, claim.RoleTester, headTwo, 1, retroTesting)
}

// A fix's completion reaches the engine before the synchronize of its end push: a fix held at the
// task's end completes at Y while the record still names X. The way returns the task to the tester
// with its working copy moved to Y, the fix's own end, with one return, before Y's synchronize
// arrives, and that synchronize returns nothing more and starts no step. Its red is the head read
// from the record (afterFix): the tester then starts at X.
func TestAFixCompletionBeforeItsSynchronizeMovesTheNextStepToItsEnd(t *testing.T) {
	r := fixAtTheEnd(t, returnEngine())
	if result := r.apply("fix-done", fixCompletion(headTwo)); result.Refusal != nil {
		t.Fatalf("the fix's completion was refused: %+v", result.Refusal)
	}
	r.assertWay(2, phase.Testing, claim.RoleTester, headTwo, 1, retroTesting)
	starts := len(r.startsOf(claim.RoleTester))
	r.apply("fix-sync", intake.PullRequestSynchronized{Repo: statesRepo, Number: 42, Branch: "legion/LEGION-208", Before: fixHead, HeadSHA: headTwo})
	if counts := r.counts(); counts.Pass != 1 {
		t.Fatalf("the task counts %+v after the synchronize, want the one return", counts)
	}
	if got := len(r.startsOf(claim.RoleTester)); got != starts {
		t.Fatalf("the tester started %d times after the synchronize, want %d: the synchronize starts no step", got, starts)
	}
}

// A person pushes P while the fix runs, and the fix's end E, a child of P, reaches the engine
// before E's synchronize, with no step of the task running: the review returned the task to the
// fix's own step. The record's head is P, which the fix's take rebased onto, so P lacks the fix's
// commits. The way runs the steps after the fix again (way 2) with the tester's working copy moved
// to E, which holds them and P's, and that start carries the fix's move. Its red is the head the
// record holds taken wherever it moved from the fix's start (afterFix): the tester then started at
// P without the fix's commits, and its start carried the move, so no later start brought them.
func TestAPushWhileTheFixRunsLeavesTheNextStepAtTheFixsEnd(t *testing.T) {
	r := newFixRig(t, returnEngine(), []string{"implementer", "tester", "reviewer"}, TriggerImplementationReady, TriggerTesterPassed)
	r.fails(fixHead, "k-ci")
	r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})
	r.reviewedX()
	r.move(TriggerReviewRejected, "", reviewReport("the review's report"))
	r.personPushes(fixHead, headTwo)
	if result := r.apply("fix-done", fixCompletion(headThree)); result.Refusal != nil {
		t.Fatalf("the fix's completion was refused: %+v", result.Refusal)
	}
	r.assertWay(2, phase.Testing, claim.RoleTester, headThree, 1, reviewingTesting)
	if fix := r.latestFix(); !fix.MoveCarried {
		t.Fatalf("fix 1 after the way's start = %+v, want its move carried by that start", fix)
	}
}

// A head pushed over the fix's end holds the fix's commits, so the way's start moves the next
// step's working copy to that head, not back to the fix's end. Fix 1 runs beside the review step
// that started at X and ends at E, whose push and synchronize arrive; a person then pushes P over
// E, and the reviewer approves P: way 2 starts the tester with its copy moved to P. Its control is
// the fix's end taken whatever the record's moves say: the tester then started at E, without P.
func TestAHeadPushedOverTheFixsEndIsWhereTheNextStepStarts(t *testing.T) {
	r := besideTheFix(t)
	r.fixPushes(headTwo)
	if result := r.apply("fix-done", fixCompletion(headTwo)); result.Refusal != nil {
		t.Fatalf("the fix's completion was refused: %+v", result.Refusal)
	}
	r.reviewerCompletes()
	r.personPushes(headTwo, headThree)
	// GitHub's merge state at P, written over the record as the engine read it, so the moves the
	// pushes made stay (mergeStateAt writes a record of its own, with none).
	pr := recordedPR(t, r.pool, "LEGION-208")
	if !slices.ContainsFunc(pr.Moves, func(m record.Move) bool { return m.Before == headTwo && m.SHA == headThree }) {
		t.Fatalf("the record's moves after the person's push = %+v, want the move from %s to %s", pr.Moves, headTwo, headThree)
	}
	pr.Mergeability, pr.MergeState, pr.CheckedHead = record.MergeabilityMergeable, record.MergeStateClean, headThree
	pr.CheckRuns = []record.AttemptRun{{Name: "unit tests", ID: 1, Head: headThree, Conclusion: record.RunSucceeded}}
	seedPR(t, r.pool, pr)
	r.reviewOf(1, "approved", headThree)

	if way := r.fixWay(1); way != wayAfterFix {
		t.Fatalf("fix 1 took way %d, want %d", way, wayAfterFix)
	}
	assertPhase(t, r.pool, phase.Testing)
	if got := r.moveToOf(claim.RoleTester); got != headThree {
		t.Fatalf("the tester's start moves its working copy to %q, want %s, the head pushed over the fix's end %s", got, headThree, headTwo)
	}
}

// Row 2: an approval while the fix runs, and the fix made no commit: each step keeps its result,
// so the fix's end reads the end again: the task waits for CI at X, where its check still fails
// and its one fix ended, so its owner is told once; with each check passed there the retro
// starts. No return. The control, kept: the same fix ending at another commit returns the task
// once, which the subtest asserts.
func TestRow2ApprovalWhileTheFixRunsWithNoCommit(t *testing.T) {
	r := fixAtTheEnd(t, returnEngine())
	r.fixEnds(fixHead)

	if fix := r.latestFix(); fix.Way != 3 {
		t.Fatalf("the fix took way %d, want 3: %+v", fix.Way, fix)
	}
	r.assertEndHold(record.HoldForCI)
	r.assertTaskAtItsRetro()
	if counts := r.counts(); counts.Pass != 0 {
		t.Fatalf("the task counts %+v, want no return", counts)
	}
	r.assertToldOwnerOnce("k-ci")

	t.Run("with each check passed at the head the retro starts", func(t *testing.T) {
		r := fixAtTheEnd(t, returnEngine())
		r.at(fixHead, noCIJob("k-unit"), ciPassing("k-ci2"), ciPassing("k-ci"))
		r.fixEnds(fixHead)
		r.assertRetroStarted()
		if counts := r.counts(); counts.Pass != 0 {
			t.Fatalf("the task counts %+v, want no return", counts)
		}
	})

	t.Run("the control: the fix ending at another commit returns the task once", func(t *testing.T) {
		r := fixAtTheEnd(t, returnEngine())
		r.fixEnds(headTwo)
		if fix := r.latestFix(); fix.Way != 2 {
			t.Fatalf("the fix took way %d, want 2: %+v", fix.Way, fix)
		}
		if counts := r.counts(); counts.Pass != 1 {
			t.Fatalf("the task counts %+v, want one return", counts)
		}
	})
}

// Row 1 run past its return: the tester passes and the reviewer approves at the new head. One
// return in all, the tester started twice in all, the fix's way 2, and the task at its end, held
// for CI at the new head, where Dispatch has published nothing yet. Its red is the way's write
// taken out of afterFix: the tester's pass then returns the task a second time.
func TestRow1RunPastItsReturn(t *testing.T) {
	r := fixAtTheEnd(t, returnEngine())
	r.fixEnds(headTwo)
	r.move(TriggerTesterPassed, "", record.PhaseRow{})
	r.move(TriggerReviewApproved, "", reviewReport("approved"))

	if counts := r.counts(); counts.Pass != 1 {
		t.Fatalf("the task counts %+v, want one return", counts)
	}
	if got := stepStartCounts(t, r.pool, "LEGION-208"); !slices.Contains(got, "a/step/2=2") {
		t.Fatalf("the starts of LEGION-208 = %v, want a/step/2=2: the tester ran twice", got)
	}
	if fix := r.latestFix(); fix.Way != 2 {
		t.Fatalf("the fix took way %d, want 2: %+v", fix.Way, fix)
	}
	r.assertEndHold(record.HoldForCI)
	r.assertTaskAtItsRetro()
}

// Row 3: changes while the fix runs, and the fix made new commits: the fix got the report, so the
// steps after it run again at the new head. One return.
func TestRow3ChangesWhileTheFixRunsWithNewCommits(t *testing.T) {
	r := fixAfterReview(t, returnEngine(), TriggerReviewRejected, false, headTwo)
	r.assertWay(2, phase.Testing, claim.RoleTester, headTwo, 1, reviewingTesting)
}

// Row 4: changes while the fix runs, and the fix made no commit: the fix got the report, so the
// steps after it run again at X. One return.
func TestRow4ChangesWhileTheFixRunsWithNoCommit(t *testing.T) {
	r := fixAfterReview(t, returnEngine(), TriggerReviewRejected, false, fixHead)
	r.assertWay(2, phase.Testing, claim.RoleTester, fixHead, 1, reviewingTesting)
}

// Row 5: an approval after the fix ended with new commits: no step starts at X, and once the
// review ended the steps after the fix run again at the new head. One return.
func TestRow5ApprovalAfterTheFixEndedWithNewCommits(t *testing.T) {
	r := fixAfterReview(t, returnEngine(), TriggerReviewApproved, true, headTwo)
	r.assertWay(2, phase.Testing, claim.RoleTester, headTwo, 1, reviewingTesting)
}

// Row 6: changes after the fix ended with new commits: a return of the review's own, to the
// implementation step with the report, at the new head. One return.
func TestRow6ChangesAfterTheFixEndedWithNewCommits(t *testing.T) {
	r := fixAfterReview(t, returnEngine(), TriggerReviewRejected, true, headTwo)
	r.assertWay(1, phase.Implementing, claim.RoleImplementer, headTwo, 1, reviewingImplementing)
	if counts := r.counts(); counts.Rounds != 1 {
		t.Fatalf("the task counts %+v, want the review's one round", counts)
	}
}

// Row 7: an approval after the fix ended with no commit: the review keeps its pass, and the task
// goes on to its end at X. No return.
func TestRow7ApprovalAfterTheFixEndedWithNoCommit(t *testing.T) {
	r := fixAfterReview(t, returnEngine(), TriggerReviewApproved, true, fixHead)
	if fix := r.latestFix(); fix.Way != 3 {
		t.Fatalf("the fix took way %d, want 3: %+v", fix.Way, fix)
	}
	if task := taskOf(t, r.pool, r.engine, "LEGION-208"); task.Place != (record.TaskPlace{Kind: record.PlaceRetro}) {
		t.Fatalf("the task stands at %+v, want its end", task.Place)
	}
	if counts := r.counts(); counts.Pass != 0 {
		t.Fatalf("the task counts %+v, want no return", counts)
	}
}

// Row 8: changes after the fix ended with no commit: a return of the review's own, at X. One
// return.
func TestRow8ChangesAfterTheFixEndedWithNoCommit(t *testing.T) {
	r := fixAfterReview(t, returnEngine(), TriggerReviewRejected, true, fixHead)
	r.assertWay(1, phase.Implementing, claim.RoleImplementer, fixHead, 1, reviewingImplementing)
}

// Rows 3 and 4 on `implementer reviewer`: the step after the fix is the review itself, so the
// reviewer starts again at its own place, a second run of its step, at the new head for row 3 and
// at X for row 4: one return, counted under implementing-reviewing, and the review's one round.
func TestRows3And4ForAnImplementerReviewerTask(t *testing.T) {
	for _, end := range []string{headTwo, fixHead} {
		t.Run("the fix ends at "+end[:4], func(t *testing.T) {
			r := newFixRig(t, returnEngine(), []string{"implementer", "reviewer"}, TriggerImplementationReady)
			r.fails(fixHead, "k-ci")
			r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})
			r.reviewedX()
			r.move(TriggerReviewRejected, "", reviewReport("the review's report"))
			r.pushed(end)
			r.apply("fix-done", fixCompletion(end))

			r.assertWay(2, phase.Reviewing, claim.RoleReviewer, end, 1, phase.Pair(phase.Implementing, phase.Reviewing))
			if got := stepStartCounts(t, r.pool, "LEGION-208"); !slices.Contains(got, "a/step/2=2") {
				t.Fatalf("the starts of LEGION-208 = %v, want a/step/2=2: the review runs a second time", got)
			}
			if counts := r.counts(); counts.Rounds != 1 {
				t.Fatalf("the task counts %+v, want the review's one round", counts)
			}
		})
	}
}

// A tester that passed beside the fix runs again after it: the fix that made new commits ends
// while the review runs, and once the review ended the tester starts a second time, at the new
// head.
func TestATesterThatPassedBesideTheFixRunsAgainAfterIt(t *testing.T) {
	r := newFixRig(t, returnEngine(), []string{"implementer", "tester", "reviewer"}, TriggerImplementationReady)
	r.fails(fixHead, "k-ci")
	r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})
	r.move(TriggerTesterPassed, "", record.PhaseRow{})
	r.pushed(headTwo)
	r.apply("fix-done", fixCompletion(headTwo))
	r.reviewedX()
	r.move(TriggerReviewApproved, "", reviewReport("approved"))

	r.assertWay(2, phase.Testing, claim.RoleTester, headTwo, 1, reviewingTesting)
	if got := stepStartCounts(t, r.pool, "LEGION-208"); !slices.Contains(got, "a/step/2=2") {
		t.Fatalf("the starts of LEGION-208 = %v, want a/step/2=2: the tester runs a second time", got)
	}
}

// everyReturn is a deployment hook that names no move and fires at every return.
var everyReturn = Hook{Name: "every-return", On: HookEventWorkBack, Every: 1, Roles: []claim.Role{claim.RoleImplementer},
	Prompt: "Say what the task has done so far."}

// hookEngine is returnEngine with everyReturn among its hooks.
func hookEngine() *Engine {
	engine := returnEngine()
	engine.cfg.Hooks = append(engine.cfg.Hooks, everyReturn)
	return engine
}

// firings is each move back hook fired on, as `<from>-<to>`, from the prompts the outbox holds.
func (r *fixRig) firings(hook string) []string {
	r.t.Helper()
	var moves []string
	for _, prompt := range hookPrompts(r.t, r.pool) {
		if prompt.Hook == hook {
			moves = append(moves, phase.Pair(prompt.From, prompt.To))
		}
	}
	return moves
}

// A fix's return fires the deployment's hooks as every return does: a hook with no moves and an
// every of 1 fires on the way-2 return from a hold for the fix beside a review, and on the way-2
// return from a hold for the fix at the task's end, the return to the step after the fix. The
// control, kept: the same hook on a way-3 end, which is no return, does not fire.
func TestAHookFiresOnAFixsReturn(t *testing.T) {
	r := fixAfterReview(t, hookEngine(), TriggerReviewRejected, false, headTwo)
	assertStrings(t, "the hook's firings", r.firings(everyReturn.Name), []string{reviewingTesting})

	t.Run("a way-2 return from the end fires once", func(t *testing.T) {
		r := fixAtTheEnd(t, hookEngine())
		r.fixEnds(headTwo)
		assertStrings(t, "the hook's firings", r.firings(everyReturn.Name), []string{retroTesting})
	})

	t.Run("a way-3 end fires nothing", func(t *testing.T) {
		r := fixAfterReview(t, hookEngine(), TriggerReviewApproved, true, fixHead)
		assertStrings(t, "the hook's firings", r.firings(everyReturn.Name), nil)
	})
}

// A way is taken once: rows 3 and 4 and a way-1 fix (row 8), each run one step past its way, count
// no second return. Its red is the way's write taken out of afterFix: row 3 then returns the task
// a second time at the tester's next end.
func TestAWayIsTakenOnce(t *testing.T) {
	for _, tc := range []struct {
		name    string
		trigger TriggerKind
		after   bool
		end     string
		next    TriggerKind
	}{
		{"row 3", TriggerReviewRejected, false, headTwo, TriggerTesterPassed},
		{"row 4", TriggerReviewRejected, false, fixHead, TriggerTesterPassed},
		{"row 8", TriggerReviewRejected, true, fixHead, TriggerImplementationReady},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := fixAfterReview(t, returnEngine(), tc.trigger, tc.after, tc.end)
			way := r.latestFix().Way
			if way == 0 {
				t.Fatalf("the fix took no way: %+v", r.latestFix())
			}
			r.move(tc.next, "", record.PhaseRow{})
			if counts := r.counts(); counts.Pass != 1 {
				t.Fatalf("the task counts %+v one step past the way, want one return", counts)
			}
			if fix := r.latestFix(); fix.Way != way {
				t.Fatalf("the fix's way moved from %d to %d one step past it", way, fix.Way)
			}
		})
	}
}
