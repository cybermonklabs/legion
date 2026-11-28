package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A task is done only once each check that a CI job runs passes at its head. The pass of its last
// step into its retro holds it before the retro instead, from retro, while one of them runs, has
// no state held there, or fails; and while a fix of the task runs. These tests stand LEGION-208's
// task past its testing step (newFixRig) and give the head's checks their states by Dispatch's
// check.state_changed events, fixChecks being the head's set.

// jobRunning is check's state at a head while its CI job has not ended there.
func jobRunning(check string) intake.DispatchCheckState {
	return inCI(stateOf(check, "no", reasonNoResult, "ci"), ciNotCompleted)
}

// ciPassing is check's state at a head where its CI job passed.
func ciPassing(check string) intake.DispatchCheckState {
	return inCI(stateOf(check, stateYes, "", "ci"), "success")
}

// noCIJob is check's state at a head where no CI job runs it: its tester's fail, with no ci_case.
func noCIJob(check string) intake.DispatchCheckState {
	return stateOf(check, "no", "fail", "tester")
}

// at applies each of states as one check.state_changed event of LEGION-208 at head, beside
// fixChecks as the head's set.
func (r *fixRig) at(head string, states ...intake.DispatchCheckState) {
	r.t.Helper()
	for _, state := range states {
		r.serial++
		applyStates(r.t, r.pool, r.engine, "LEGION-208", statesRepo, head, r.serial, ofSet(state, fixChecks...))
	}
}

// noChecksAt applies the event of a head of LEGION-208 whose set holds no check.
func (r *fixRig) noChecksAt(head string) {
	r.t.Helper()
	r.serial++
	applyStates(r.t, r.pool, r.engine, "LEGION-208", statesRepo, head, r.serial)
}

// ciRunsAtTheEnd gives LEGION-208's head the states of a task whose one CI check still runs:
// k-unit, which no CI job runs, read fail by its tester, k-ci2 passed and k-ci running.
func (r *fixRig) ciRunsAtTheEnd() {
	r.t.Helper()
	r.at(fixHead, noCIJob("k-unit"), ciPassing("k-ci2"), jobRunning("k-ci"))
}

// reviewedCommit is the commit the reviewer's report names: the task that is told the report names
// it, and no other part of a retro's task does.
const reviewedCommit = headThree

// reviewerReported writes the reviewer's report on its phases row, as its completion writes it,
// naming reviewedCommit, and answers the row: the report the step after the review is told.
func (r *fixRig) reviewerReported() record.PhaseRow {
	r.t.Helper()
	row := roleRowOf(r.t, r.pool, r.engine, "LEGION-208", claim.RoleReviewer)
	row.Issue, row.Role = "LEGION-208", claim.RoleReviewer
	row.HandoffRecord, row.HandoffVersion, row.HandoffCommit, row.Summary = reviewRecord, 1, reviewedCommit, "approved"
	applyInTx(r.t, r.pool, r.engine, func(ctx context.Context, tx pgx.Tx) error { return r.engine.store.PutPhase(ctx, tx, row) })
	return row
}

// approves is the review's approval of LEGION-208, its report written on the reviewer's row.
func (r *fixRig) approves() {
	r.t.Helper()
	r.move(TriggerReviewApproved, "", r.reviewerReported())
}

// signsOff is the owner's sign-off of LEGION-208 in accepting, as its route builds it.
func (r *fixRig) signsOff() {
	r.t.Helper()
	if result := signsOff(r.t, r.pool, r.engine, "LEGION-208"); result.Refusal != nil {
		r.t.Fatalf("the sign-off was refused: %+v", result.Refusal)
	}
}

// moveFact moves LEGION-208 on trigger as a fact of the table's moves it: the move, then the fix
// rule at the tail of the fact (applyAndFix), in one transaction.
func (r *fixRig) moveFact(trigger TriggerKind, target phase.Phase, handoff record.PhaseRow) {
	r.t.Helper()
	onStored(r.t, r.pool, r.engine, "LEGION-208", func(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
		pr, err := r.engine.store.PullRequest(ctx, tx, issue.Key)
		if err != nil {
			return err
		}
		if err := r.engine.transition(ctx, tx, issue, trigger, target, handoff, pr, moveWhy{Reason: "the step test's move"}); err != nil {
			return err
		}
		return r.engine.fixRule(ctx, tx, issue.Key)
	})
}

// releaseBefore writes LEGION-208's task as one of a run that started before this release: it
// takes no fix.
func (r *fixRig) releaseBefore() {
	r.t.Helper()
	if _, err := r.pool.Exec(r.t.Context(), `update plan_tasks set fixes_ci = false where issue = 'LEGION-208'`); err != nil {
		r.t.Fatalf("write the task as the upgrade found it: %v", err)
	}
}

// assertEndHold fails unless LEGION-208 stands held before its retro for reason, from retro, and
// no retro started.
func (r *fixRig) assertEndHold(reason record.HoldReason) record.Issue {
	r.t.Helper()
	issue := issueOf(r.t, r.pool, r.engine, "LEGION-208")
	if issue.Phase != phase.Held || issue.Hold == nil || *issue.Hold != (record.Hold{From: phase.Retro, Reason: reason}) {
		r.t.Fatalf("LEGION-208 stands in %s with hold %+v, want held from retro for %s", issue.Phase, issue.Hold, reason)
	}
	if starts := startsIn(r.t, r.pool, "LEGION-208", claim.RoleImplementer, phase.Retro); len(starts) != 0 {
		r.t.Fatalf("the retro's starts = %+v under the hold, want none", starts)
	}
	return issue
}

// assertTaskAtItsRetro fails unless task a of LEGION-208 runs at its retro.
func (r *fixRig) assertTaskAtItsRetro() {
	r.t.Helper()
	if task := taskOf(r.t, r.pool, r.engine, "LEGION-208"); task.State != record.TaskRunning || task.Place != (record.TaskPlace{Kind: record.PlaceRetro}) {
		r.t.Fatalf("task a is %s at %+v, want running at its retro", task.State, task.Place)
	}
}

// assertRetroStarted fails unless LEGION-208 stands in its retro with no hold and one start of
// its retro, and answers that start.
func (r *fixRig) assertRetroStarted() record.SuperviseRequest {
	r.t.Helper()
	if issue := issueOf(r.t, r.pool, r.engine, "LEGION-208"); issue.Phase != phase.Retro || issue.Hold != nil {
		r.t.Fatalf("LEGION-208 stands in %s with hold %+v, want its retro", issue.Phase, issue.Hold)
	}
	starts := startsIn(r.t, r.pool, "LEGION-208", claim.RoleImplementer, phase.Retro)
	if len(starts) != 1 {
		r.t.Fatalf("the retro's starts = %+v, want one", starts)
	}
	return starts[0]
}

// retroHeld is the reason of each retro-held notice of LEGION-208, oldest first.
func (r *fixRig) retroHeld() []string {
	r.t.Helper()
	var reasons []string
	for _, notice := range noticesOf(r.t, r.pool, "LEGION-208", "retro-held") {
		reasons = append(reasons, notice.Reason)
	}
	return reasons
}

// ciFailed is each ci-failed notice of LEGION-208, oldest first.
func (r *fixRig) ciFailed() []record.Notice {
	r.t.Helper()
	return noticesOf(r.t, r.pool, "LEGION-208", "ci-failed")
}

// assertNames fails unless text names each of named and none of unnamed, what naming the text.
func assertNames(t *testing.T, what, text string, named []string, unnamed ...string) {
	t.Helper()
	for _, fact := range named {
		if !strings.Contains(text, fact) {
			t.Fatalf("%s = %q, want it to name %q", what, text, fact)
		}
	}
	for _, fact := range unnamed {
		if strings.Contains(text, fact) {
			t.Fatalf("%s = %q, want it not to name %q", what, text, fact)
		}
	}
}

// assertToldOwnerOnce fails unless LEGION-208's owner holds one ci-failed notice, naming check
// and fixHead.
func (r *fixRig) assertToldOwnerOnce(check string) {
	r.t.Helper()
	told := r.ciFailed()
	if len(told) != 1 || told[0].Role != claim.RoleArchitect {
		r.t.Fatalf("the ci-failed notices = %+v, want one to the owner", told)
	}
	assertNames(r.t, "the ci-failed notice", told[0].Reason, []string{check, fixHead})
}

// A task whose last step passed while its CI job runs at its head is not done: the approval holds
// it before its retro for its CI, from retro, its task at its retro, the review's run ended passed,
// no retro started, and its owner told once by a retro-held notice naming the check and the head.
// Its red is the hold for CI taken out of retroHold: the same approval then starts the retro while
// the job runs.
func TestATaskIsNotDoneWhileItsJobRuns(t *testing.T) {
	r := reviewing(t)
	r.ciRunsAtTheEnd()

	r.approves()

	r.assertEndHold(record.HoldForCI)
	r.assertTaskAtItsRetro()
	if result := lastStepEnd(t, r.pool, "LEGION-208", "reviewer"); result != "passed" {
		t.Fatalf("the reviewer's step ended %q, want passed", result)
	}
	held := r.retroHeld()
	if len(held) != 1 {
		t.Fatalf("the retro-held notices = %q, want one", held)
	}
	assertNames(t, "the retro-held notice", held[0], []string{"k-ci", fixHead}, "k-ci2", "k-unit")
}

// A check of the head's set that holds no state at the head is not known to pass: the end waits
// for it, naming it, and its pass starts the retro.
func TestATaskWaitsForAStateItDoesNotHold(t *testing.T) {
	r := reviewing(t)
	r.at(fixHead, noCIJob("k-unit"), ciPassing("k-ci"))

	r.approves()

	r.assertEndHold(record.HoldForCI)
	if held := r.retroHeld(); len(held) != 1 || !strings.Contains(held[0], "k-ci2") {
		t.Fatalf("the retro-held notices = %q, want one naming k-ci2", held)
	}
	r.at(fixHead, ciPassing("k-ci2"))
	r.assertRetroStarted()
}

// The end held for CI ends when the job passes at the task's head: a pass at a head the pull
// request has left changes nothing, and the pass at the head starts the retro, its Dispatch status
// written retro and its task naming the review's report.
func TestTheRetroStartsWhenTheJobPassesAtTheHead(t *testing.T) {
	r := reviewing(t)
	r.ciRunsAtTheEnd()
	r.approves()
	statuses := len(statusWrites(t, r.pool, "LEGION-208"))

	r.at(formerHead, ciPassing("k-ci"))
	r.assertEndHold(record.HoldForCI)

	r.at(fixHead, ciPassing("k-ci"))
	start := r.assertRetroStarted()
	assertNames(t, "the retro's task", start.Task, []string{reviewedCommit})
	if writes := statusWrites(t, r.pool, "LEGION-208")[statuses:]; len(writes) != 1 || writes[0].Status != PhaseStatus(phase.Retro) {
		t.Fatalf("the release queued the status writes %+v, want the retro's", writes)
	}
}

// A task whose last step passes while a fix of it runs waits for the fix: it is held before its
// retro for the fix, its owner told once naming the fix's check and head, and CI passing at the
// head starts no retro while the fix runs.
func TestTheRetroWaitsForTheFix(t *testing.T) {
	r := startedFix(t, intake.FixJobRead{Name: "unit", URL: fixPage})

	r.approves()

	r.assertEndHold(record.HoldForFix)
	if held := r.retroHeld(); len(held) != 1 {
		t.Fatalf("the retro-held notices = %q, want one", held)
	} else {
		assertNames(t, "the retro-held notice", held[0], []string{"k-ci", fixHead})
	}
	r.at(fixHead, noCIJob("k-unit"), ciPassing("k-ci2"), ciPassing("k-ci"))
	r.assertEndHold(record.HoldForFix)
}

// A check that no CI job runs holds nothing at the end, whatever its state: the end waits for the
// CI job of k-ci alone, and once it passes the retro starts while k-unit reads fail.
func TestACheckOfNoCIJobDoesNotHoldTheEnd(t *testing.T) {
	r := reviewing(t)
	r.ciRunsAtTheEnd()
	r.approves()
	r.assertEndHold(record.HoldForCI)
	if held := r.retroHeld(); len(held) != 1 || strings.Contains(held[0], "k-unit") {
		t.Fatalf("the retro-held notices = %q, want one that does not name k-unit", held)
	}

	r.at(fixHead, ciPassing("k-ci"))

	r.assertRetroStarted()
}

// A task with no implementation step is held for its CI as every task is, and a failure there
// starts no fix, since no step of it can run as one.
func TestATaskWithNoImplementationStepIsHeldForCI(t *testing.T) {
	r := newFixRig(t, returnEngine(), []string{"tester", "reviewer"}, TriggerTesterPassed)
	assertPhase(t, r.pool, phase.Reviewing)
	r.ciRunsAtTheEnd()

	r.approves()
	r.assertEndHold(record.HoldForCI)

	r.fails(fixHead, "k-ci")
	r.assertEndHold(record.HoldForCI)
	assertStrings(t, "the fixes", r.fixes(), nil)
}

// A task of a run that started before this release takes no fix, and its end is held for CI as
// every task's is: held for its CI, and a failure there starts no fix.
func TestATaskOfTheReleaseBeforeIsHeldForCI(t *testing.T) {
	r := reviewing(t)
	r.releaseBefore()
	r.ciRunsAtTheEnd()

	r.approves()
	r.assertEndHold(record.HoldForCI)

	r.fails(fixHead, "k-ci")
	r.assertEndHold(record.HoldForCI)
	assertStrings(t, "the fixes", r.fixes(), nil)
}

// The same task held for CI at its end, its check failing there in two events: no fix starts, and
// its owner is told once, by one ci-failed notice naming the check and the head.
func TestATaskOfTheReleaseBeforeHeldForCITellsItsOwnerOnce(t *testing.T) {
	r := reviewing(t)
	r.releaseBefore()
	r.ciRunsAtTheEnd()
	r.approves()

	r.fails(fixHead, "k-ci")
	r.fails(fixHead, "k-ci")

	r.assertEndHold(record.HoldForCI)
	assertStrings(t, "the fixes", r.fixes(), nil)
	assertStrings(t, "the failures", r.failures(), []string{failureOf("k-ci", "owner", 0)})
	r.assertToldOwnerOnce("k-ci")
}

// noPlanRows is LEGION-208 with no plan rows in accepting, today's line, its pull request #42 open
// at fixHead and the review's report on the reviewer's row, through engine.
func noPlanRows(t *testing.T, engine *Engine) *fixRig {
	t.Helper()
	pool := migratedPool(t)
	stepRoot(t, pool, phase.Accepting)
	r := &fixRig{t: t, pool: pool, engine: engine}
	r.pushed(fixHead)
	r.reviewerReported()
	return r
}

// An issue with no plan rows takes no fix: held for CI after its owner's sign-off, its check
// failing there in two events, its owner is told once.
func TestAnIssueWithNoPlanRowsHeldForCITellsItsOwnerOnce(t *testing.T) {
	r := noPlanRows(t, returnEngine())
	r.ciRunsAtTheEnd()
	r.signsOff()
	r.assertEndHold(record.HoldForCI)

	r.fails(fixHead, "k-ci")
	r.fails(fixHead, "k-ci")

	r.assertEndHold(record.HoldForCI)
	assertStrings(t, "the fixes", r.fixes(), nil)
	r.assertToldOwnerOnce("k-ci")
}

// An issue with no plan rows walks today's line, whose last step is the owner's acceptance: the
// sign-off while CI runs holds it before its retro for its CI, from retro; it keeps the status it
// showed, since a hold writes none, and its owner gets one retro-held notice naming the check.
// When CI passes the retro starts, its task naming the review's report, which the sign-off
// accepted.
func TestAnIssueWithNoPlanRowsIsHeldForCIAfterItsSignOff(t *testing.T) {
	r := noPlanRows(t, returnEngine())
	r.ciRunsAtTheEnd()
	before := issueOf(t, r.pool, r.engine, "LEGION-208")
	statuses := len(statusWrites(t, r.pool, "LEGION-208"))

	r.signsOff()

	held := r.assertEndHold(record.HoldForCI)
	if held.Status != before.Status {
		t.Fatalf("LEGION-208 held has status %q, want %q, the status it showed", held.Status, before.Status)
	}
	if writes := statusWrites(t, r.pool, "LEGION-208")[statuses:]; len(writes) != 0 {
		t.Fatalf("the sign-off queued the status writes %+v, want none: a hold has no status", writes)
	}
	if reasons := r.retroHeld(); len(reasons) != 1 {
		t.Fatalf("the retro-held notices = %q, want one", reasons)
	} else {
		assertNames(t, "the retro-held notice", reasons[0], []string{"k-ci", fixHead})
	}

	r.at(fixHead, ciPassing("k-ci"))

	start := r.assertRetroStarted()
	assertNames(t, "the retro's task", start.Task, []string{reviewedCommit})
}

// A stored walk ending in the owner's acceptance, the project's default steps, is told at its
// retro the report the sign-off accepted, the review's, and not the row of the acceptance's own
// role: held for CI after the sign-off, CI passes, and the retro's task names the reviewer's
// report. The control, kept: a parent held for its children is told its last step's report at its
// release, as before.
func TestTheDefaultStepsRetroIsToldTheReviewsReport(t *testing.T) {
	t.Run("the sign-off's retro after CI", func(t *testing.T) {
		r := newFixRig(t, engineRunningByDefault(t, "implementer", "reviewer", "acceptance:owner"), nil, TriggerImplementationReady)
		r.ciRunsAtTheEnd()
		r.approves()
		assertPhase(t, r.pool, phase.Accepting)

		r.signsOff()
		r.assertEndHold(record.HoldForCI)
		r.at(fixHead, ciPassing("k-ci"))

		start := r.assertRetroStarted()
		assertNames(t, "the retro's task", start.Task, []string{reviewedCommit})
	})

	t.Run("the control: a parent held for its children is told its last step's report", func(t *testing.T) {
		r := newFixRig(t, returnEngine(), []string{"implementer", "reviewer"}, TriggerImplementationReady)
		r.noChecksAt(fixHead)
		stepChild(t, r.pool, heldParentChild, phase.Implementing, nil)
		r.approves()
		r.assertEndHold(record.HoldForChildren)

		childLeavesDone(t, r.pool, r.engine)

		start := r.assertRetroStarted()
		assertNames(t, "the retro's task", start.Task, []string{reviewedCommit})
	})
}

// retroReturn is a deployment hook on the owner's move back from the retro to implementing.
var retroReturn = Hook{Name: "retro-return", On: HookEventWorkBack, Moves: []string{phase.Pair(phase.Retro, phase.Implementing)}, Every: 1,
	Roles: []claim.Role{claim.RoleImplementer}, Prompt: "Say why the task went back."}

// retroReturnEngine is returnEngine with retroReturn among its hooks.
func retroReturnEngine() *Engine {
	engine := returnEngine()
	engine.cfg.Hooks = append(engine.cfg.Hooks, retroReturn)
	return engine
}

// ownerMovesBack is the owner's request_backward_move of LEGION-208 to implementing.
func (r *fixRig) ownerMovesBack() intake.Result {
	r.t.Helper()
	return r.apply("owner-back", rootsMoveBack(r.t, r.pool, r.engine, "LEGION-208", phase.Implementing, "the check fails for the plan's design"))
}

// assertBackFromTheRetro fails unless the owner's move ended LEGION-208's hold and started its
// implementer in implementing, and the hook on retro-implementing fired once.
func (r *fixRig) assertBackFromTheRetro(result intake.Result) {
	r.t.Helper()
	if result.Refusal != nil {
		r.t.Fatalf("the owner's move back was refused: %+v", result.Refusal)
	}
	if issue := issueOf(r.t, r.pool, r.engine, "LEGION-208"); issue.Phase != phase.Implementing || issue.Hold != nil {
		r.t.Fatalf("LEGION-208 after the owner's move stands in %s with hold %+v, want implementing", issue.Phase, issue.Hold)
	}
	if starts := r.startsOf(claim.RoleImplementer); len(starts) == 0 || starts[len(starts)-1].Phase != phase.Implementing {
		r.t.Fatalf("the implementer's starts = %+v, want the latest in implementing", starts)
	}
	assertStrings(r.t, "the hook's firings", r.firings(retroReturn.Name), []string{phase.Pair(phase.Retro, phase.Implementing)})
}

// The owner's act that ends a hold for CI before CI passes is its move back: on an issue with no
// plan rows held for CI, its check failing and its owner told, the owner's request_backward_move
// to implementing ends the hold, starts the implementer and counts one return under
// retro-implementing, where a deployment hook fires; the same for a stored walk. The control,
// kept: the owner's move back from an issue held escalated, not for CI, is refused with the words
// a move from held gets.
func TestTheOwnersMoveBackEndsACIHold(t *testing.T) {
	t.Run("an issue with no plan rows", func(t *testing.T) {
		r := noPlanRows(t, retroReturnEngine())
		r.ciRunsAtTheEnd()
		r.signsOff()
		r.fails(fixHead, "k-ci")

		r.assertBackFromTheRetro(r.ownerMovesBack())

		counts := implementerCounts(t, r.pool, r.engine, "LEGION-208")
		if counts.Pass != 1 || counts.BackwardMoves[phase.Pair(phase.Retro, phase.Implementing)] != 1 {
			t.Fatalf("the implementer's row counts %+v, want one return under retro-implementing", counts)
		}
		r.assertToldOwnerOnce("k-ci")
	})

	t.Run("a stored walk", func(t *testing.T) {
		r := newFixRig(t, retroReturnEngine(), []string{"implementer", "reviewer"}, TriggerImplementationReady)
		r.ciRunsAtTheEnd()
		r.approves()

		r.assertBackFromTheRetro(r.ownerMovesBack())

		if counts := r.counts(); counts.Pass != 1 || counts.BackwardMoves[phase.Pair(phase.Retro, phase.Implementing)] != 1 {
			t.Fatalf("the task counts %+v, want one return under retro-implementing", counts)
		}
	})

	t.Run("the control: the owner's move from a hold that is not CI's is refused as before", func(t *testing.T) {
		r := noPlanRows(t, retroReturnEngine())
		held := issueOf(t, r.pool, r.engine, "LEGION-208")
		held.Phase, held.Hold = phase.Held, &record.Hold{From: phase.Retro, Reason: record.HoldEscalated}
		seedIssue(t, r.pool, held)
		move := rootsMoveBack(t, r.pool, r.engine, "LEGION-208", phase.Implementing, "the check fails for the plan's design")

		result := r.apply("owner-back", move)

		if refusalCode(result) != "BACKWARD_REFUSED" || result.Refusal.Message != backwardRefusal(held, move, false) {
			t.Fatalf("the owner's move from an escalated hold = %+v, want BACKWARD_REFUSED as a move from held is refused", result.Refusal)
		}
		if issue := issueOf(t, r.pool, r.engine, "LEGION-208"); issue.Hold == nil || *issue.Hold != *held.Hold {
			t.Fatalf("LEGION-208 after the refused move has hold %+v, want %+v", issue.Hold, held.Hold)
		}
	})
}

// A head the daemon holds no set of checks at holds the end for CI: Dispatch has published nothing
// there yet, and the notice names the head. The set's event, here a head with no check, starts the
// retro.
func TestAHeadWithNoCheckSetHoldsTheEnd(t *testing.T) {
	r := reviewing(t)

	r.approves()

	r.assertEndHold(record.HoldForCI)
	if held := r.retroHeld(); len(held) != 1 || !strings.Contains(held[0], fixHead) {
		t.Fatalf("the retro-held notices = %q, want one naming %s", held, fixHead)
	}
	r.noChecksAt(fixHead)
	r.assertRetroStarted()
}

// A head whose set is held and empty holds nothing: the approval starts the retro.
func TestAHeadWithAnEmptySetHoldsNothing(t *testing.T) {
	r := reviewing(t)
	r.noChecksAt(fixHead)

	r.approves()

	r.assertRetroStarted()
}

// endShapes are the two ends of a task: a last step the review, which its approval passes, and a
// last step the owner's acceptance, which its sign-off passes.
var endShapes = []struct {
	name  string
	words []string
	end   func(r *fixRig)
}{
	{"a task ending in its review", []string{"implementer", "reviewer"}, func(r *fixRig) { r.approves() }},
	{"a task ending in its acceptance", []string{"implementer", "reviewer", "acceptance:owner"}, func(r *fixRig) {
		r.approves()
		r.signsOff()
	}},
}

// An end hold stands from the retro, which keeps no slot: a task ending in its review and one
// ending in its acceptance, each held for CI, hold none.
func TestAnEndHoldKeepsNoSlot(t *testing.T) {
	for _, shape := range endShapes {
		t.Run(shape.name, func(t *testing.T) {
			r := newFixRig(t, returnEngine(), shape.words, TriggerImplementationReady)
			r.ciRunsAtTheEnd()

			shape.end(r)

			if held := r.assertEndHold(record.HoldForCI); record.KeepsSlot(held) {
				t.Fatalf("LEGION-208 held for CI from %s keeps a slot, want none", held.Hold.From)
			}
		})
	}
}

// A fix started at an end hold runs on no slot: at each of the two ends held for CI, the failure
// turns the hold into one for the fix, and the fix's start is written waiting for no slot, while
// the issue's pending start of its review waits for one.
func TestAFixAtAnyEndHoldRunsWithNoSlot(t *testing.T) {
	for _, shape := range endShapes {
		t.Run(shape.name, func(t *testing.T) {
			r := newFixRig(t, returnEngine(), shape.words, TriggerImplementationReady)
			r.ciRunsAtTheEnd()
			shape.end(r)
			r.assertEndHold(record.HoldForCI)

			r.fails(fixHead, "k-ci")
			r.assertEndHold(record.HoldForFix)
			r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})

			starts := r.fixStarts()
			if len(starts) != 1 || starts[0].NeedsSlot || starts[0].Phase != phase.Implementing {
				t.Fatalf("the fix's starts = %+v, want one in implementing waiting for no slot", starts)
			}
			if reviewers := r.startsOf(claim.RoleReviewer); len(reviewers) == 0 || !reviewers[len(reviewers)-1].NeedsSlot {
				t.Fatalf("the reviewer's pending starts = %+v, want the latest waiting for a slot", reviewers)
			}
		})
	}
}

// A fix beside a review, before the end, runs on the slot the issue holds for its review: the
// issue keeps its one slot while the fix starts, and the fix's start takes none of its own.
func TestAFixBesideAReviewRunsOnTheIssuesSlot(t *testing.T) {
	r := reviewing(t)
	seedRecord(t, r.pool, func(tx pgx.Tx) error {
		return record.NewStore().PutSlot(t.Context(), tx, record.Slot{Issue: "LEGION-208", Index: 0})
	})
	slots := func() int {
		var count int
		if err := r.pool.QueryRow(t.Context(), `select count(*) from slots`).Scan(&count); err != nil {
			t.Fatalf("count the slots: %v", err)
		}
		return count
	}

	r.fails(fixHead, "k-ci")
	r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})

	if got := slots(); got != 1 {
		t.Fatalf("the slots = %d once the fix started, want the issue's one", got)
	}
	if issue := issueOf(t, r.pool, r.engine, "LEGION-208"); !record.KeepsSlot(issue) {
		t.Fatalf("LEGION-208 beside its fix stands in %s keeping no slot, want its review's", issue.Phase)
	}
	if starts := r.fixStarts(); len(starts) != 1 || starts[0].NeedsSlot {
		t.Fatalf("the fix's starts = %+v, want one waiting for no slot", starts)
	}
}

// releaseOf applies the architect's decision on LEGION-208's hold and answers its result.
func (r *fixRig) releaseOf(decision intake.RetryOrEscalateDecision) intake.Result {
	r.t.Helper()
	return r.apply("release-"+string(decision), intake.RetryOrEscalate{Issue: "LEGION-208", Decision: decision, Reason: "try again"})
}

// assertRefused fails unless result is refused 409 code naming each of named.
func assertRefused(t *testing.T, result intake.Result, code string, named ...string) {
	t.Helper()
	if result.Refusal == nil || result.Refusal.Status != 409 || result.Refusal.Code != code {
		t.Fatalf("the answer = %+v, want 409 %s", result.Refusal, code)
	}
	assertNames(t, "the refusal", result.Refusal.Message, named)
}

// A retry or an escalation of a hold for CI would start the retro's role early or overwrite the
// reason, so each is refused CI_PENDING, naming the head and the check it waits for, and the hold
// keeps its reason. The control, kept: a retry of a plain hold, an escalated one, is taken.
func TestARetryOfTheCIHoldIsRefused(t *testing.T) {
	for _, decision := range []intake.RetryOrEscalateDecision{intake.RetryDecision, intake.EscalateDecision} {
		t.Run(string(decision), func(t *testing.T) {
			r := reviewing(t)
			r.ciRunsAtTheEnd()
			r.approves()

			assertRefused(t, r.releaseOf(decision), "CI_PENDING", "LEGION-208", fixHead, "k-ci")

			r.assertEndHold(record.HoldForCI)
		})
	}

	t.Run("the control: a retry of an escalated hold is taken", func(t *testing.T) {
		r := reviewing(t)
		held := issueOf(t, r.pool, r.engine, "LEGION-208")
		held.Phase, held.Hold = phase.Held, &record.Hold{From: phase.Reviewing, Reason: record.HoldEscalated}
		seedIssue(t, r.pool, held)

		if result := r.releaseOf(intake.RetryDecision); result.Refusal != nil {
			t.Fatalf("the retry of an escalated hold was refused: %+v", result.Refusal)
		}
		if issue := issueOf(t, r.pool, r.engine, "LEGION-208"); issue.Phase != phase.Reviewing || issue.Hold != nil {
			t.Fatalf("LEGION-208 after the retry stands in %s with hold %+v, want reviewing", issue.Phase, issue.Hold)
		}
	})
}

// An escalation or a retry of a hold for a fix, at the end and beside a review, is refused
// FIX_RUNNING, naming the fix's task, check and head: the fix's end moves the task on. The hold
// keeps its reason.
func TestAnEscalationOfTheFixHoldIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		from phase.Phase
		hold func(r *fixRig)
	}{
		{"at the end", phase.Retro, func(r *fixRig) { r.approves() }},
		{"beside the review", phase.Reviewing, func(r *fixRig) { r.move(TriggerReviewRejected, "", reviewReport("the widget renders twice")) }},
	} {
		for _, decision := range []intake.RetryOrEscalateDecision{intake.EscalateDecision, intake.RetryDecision} {
			t.Run(tc.name+"/"+string(decision), func(t *testing.T) {
				r := startedFix(t, intake.FixJobRead{Name: "unit", URL: fixPage})
				tc.hold(r)
				want := record.Hold{From: tc.from, Reason: record.HoldForFix}

				assertRefused(t, r.releaseOf(decision), "FIX_RUNNING", "LEGION-208", "k-ci", fixHead)

				if issue := issueOf(t, r.pool, r.engine, "LEGION-208"); issue.Phase != phase.Held || issue.Hold == nil || *issue.Hold != want {
					t.Fatalf("LEGION-208 after the refused %s stands in %s with hold %+v, want %+v", decision, issue.Phase, issue.Hold, want)
				}
			})
		}
	}
}

// A person's todo on a root held for CI keeps the hold and the task where it stands: the record
// takes the todo it observed, the status it keeps is queued back over the todo with the reason the
// refusal of a release gives, and no step starts again. The control, kept: a todo on a root held
// for its stopped task starts the task again, as before.
func TestATodoOnARootHeldForCIKeepsTheTaskWhereItStands(t *testing.T) {
	t.Run("the hold stays", func(t *testing.T) {
		r := reviewing(t)
		r.ciRunsAtTheEnd()
		r.approves()
		held := r.assertEndHold(record.HoldForCI)
		task := taskOf(t, r.pool, r.engine, "LEGION-208")
		statuses, starts := len(statusWrites(t, r.pool, "LEGION-208")), len(superviseRowsOf(t, r.pool, "LEGION-208", "start"))
		var refusal *intake.Refusal
		onStored(t, r.pool, r.engine, "LEGION-208", func(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
			var err error
			refusal, err = r.engine.planHoldRefusal(ctx, tx, issue)
			return err
		})
		if refusal == nil {
			t.Fatal("a release of the hold for CI is refused nothing, want the refusal whose reason the todo is told")
		}

		if result := r.apply("todo", personsTodo("LEGION-208")); result.Refusal != nil {
			t.Fatalf("the todo was refused: %+v", result.Refusal)
		}

		issue := r.assertEndHold(record.HoldForCI)
		if issue.Status != held.Status || issue.DispatchStatus != "todo" {
			t.Fatalf("LEGION-208 after the todo has status %q, Dispatch at %q, want %q kept and todo recorded", issue.Status, issue.DispatchStatus, held.Status)
		}
		if after := taskOf(t, r.pool, r.engine, "LEGION-208"); after.Place != task.Place || after.State != task.State {
			t.Fatalf("task a after the todo is %s at %+v, want %s at %+v", after.State, after.Place, task.State, task.Place)
		}
		if writes := statusWrites(t, r.pool, "LEGION-208")[statuses:]; len(writes) != 1 || writes[0] != record.OrdinaryStatusWrite(held.Status, "todo", refusal.Message) {
			t.Fatalf("the todo queued the status writes %+v, want the record's status %q over the todo with the hold's reason", writes, held.Status)
		}
		if got := len(superviseRowsOf(t, r.pool, "LEGION-208", "start")); got != starts {
			t.Fatalf("the todo queued %d starts, want none", got-starts)
		}
	})

	t.Run("the control: a todo on a stopped task starts it again", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stoppedAtTesting(t, pool, engine)

		if result := applyFacts(t, pool, engine)("todo", personsTodo(root.Key)); result.Refusal != nil {
			t.Fatalf("the todo was refused: %+v", result.Refusal)
		}

		if issue := issueOf(t, pool, engine, root.Key); issue.Phase != phase.Implementing || issue.Hold != nil {
			t.Fatalf("%s after the todo stands in %s with hold %+v, want implementing", root.Key, issue.Phase, issue.Hold)
		}
	})
}

// The children's hold comes before CI's: a parent with an open child and its CI job running
// passes its last step held for its children; the child's leave turns the hold into one for CI,
// its owner told, and the job's pass starts the retro.
func TestAChildrensHoldComesFirst(t *testing.T) {
	r := newFixRig(t, returnEngine(), []string{"implementer", "reviewer"}, TriggerImplementationReady)
	stepChild(t, r.pool, heldParentChild, phase.Implementing, nil)
	r.ciRunsAtTheEnd()

	r.approves()
	r.assertEndHold(record.HoldForChildren)

	childLeavesDone(t, r.pool, r.engine)
	r.assertEndHold(record.HoldForCI)
	if held := r.retroHeld(); len(held) != 2 || !strings.Contains(held[0], heldParentChild) || !strings.Contains(held[1], "k-ci") {
		t.Fatalf("the retro-held notices = %q, want the children's naming %s, then CI's naming k-ci", held, heldParentChild)
	}

	r.at(fixHead, ciPassing("k-ci"))
	r.assertRetroStarted()
}
