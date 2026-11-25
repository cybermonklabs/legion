package workflow

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// reviewRecord is the handoff record the reviewer's report names in these tests.
const reviewRecord = "review-record"

// reviewReport is the reviewer's row a move of its step carries: its report, saying summary.
func reviewReport(summary string) record.PhaseRow {
	return record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleReviewer, HandoffRecord: reviewRecord, Summary: summary}
}

// move moves LEGION-208 on trigger to target with handoff as the move's row, as a fact of the
// table's would.
func (r *fixRig) move(trigger TriggerKind, target phase.Phase, handoff record.PhaseRow) {
	r.t.Helper()
	move(r.t, r.pool, r.engine, "LEGION-208", trigger, target, handoff)
}

// pushed is a push that moved LEGION-208's pull request to head.
func (r *fixRig) pushed(head string) {
	r.t.Helper()
	seedPR(r.t, r.pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: statesRepo, Number: 42,
		Branch: "legion/LEGION-208", HeadSHA: head, Failing: []string{}})
}

// startsOf is each start of a step of role the outbox holds for LEGION-208, a fix's starts left
// out, oldest first.
func (r *fixRig) startsOf(role claim.Role) []record.SuperviseRequest {
	r.t.Helper()
	var starts []record.SuperviseRequest
	for _, start := range superviseRowsOf(r.t, r.pool, "LEGION-208", "start") {
		if start.Role == role && start.Fix == 0 && start.Phase != "" {
			starts = append(starts, start)
		}
	}
	return starts
}

// counts is the returns LEGION-208's task counts now.
func (r *fixRig) counts() record.ReturnCounts {
	r.t.Helper()
	return taskOf(r.t, r.pool, r.engine, "LEGION-208").Counts
}

// assertHeldForTheFix fails unless LEGION-208 is held for its fix from the phase from.
func (r *fixRig) assertHeldForTheFix(from phase.Phase) {
	r.t.Helper()
	issue := issueOf(r.t, r.pool, r.engine, "LEGION-208")
	if issue.Phase != phase.Held || issue.Hold == nil || *issue.Hold != (record.Hold{From: from, Reason: record.HoldForFix}) {
		r.t.Fatalf("LEGION-208 stands in %s with hold %+v, want held from %s for its fix", issue.Phase, issue.Hold, from)
	}
}

// runEnd is the end record of run starts of the step at place of LEGION-208's task a, nil where
// none was written.
func (r *fixRig) runEnd(place int, starts int64) *record.StepRecord {
	r.t.Helper()
	id := StepID("LEGION-208", 1, record.StepPlace{Task: "a", Kind: record.PlaceStep, Place: place}, starts)
	for _, write := range stepWrites(r.t, r.pool, "LEGION-208") {
		if write.ID == id && isEnd(write) {
			return &write
		}
	}
	return nil
}

// assertRunEnded fails unless run starts of the step at place ended with result, returning to
// returnedTo (0 for nowhere).
func (r *fixRig) assertRunEnded(place int, starts int64, result StepResult, returnedTo int) {
	r.t.Helper()
	end := r.runEnd(place, starts)
	if end == nil || end.Result == nil || *end.Result != string(result) {
		r.t.Fatalf("the end of run %d of step %d = %+v, want %s", starts, place, end, result)
	}
	if got := end.ReturnedTo; (returnedTo == 0) != (got == nil) || (got != nil && *got != returnedTo) {
		r.t.Fatalf("the end of run %d of step %d returns to %v, want %d", starts, place, got, returnedTo)
	}
}

// The review's approval after the fix ended with new commits starts no retro before the way after
// the fix is decided: the review's end is written, passed, and no retro starts, no record of a
// retro's run is written, and no phase-finished of the review is sent; the way the fix takes then
// returns the task to the tester at the new head.
func TestAnApprovalAfterTheFixEndedStartsNoRetroBeforeTheWay(t *testing.T) {
	r := startedFix(t, intake.FixJobRead{Name: "unit", URL: fixPage})
	r.pushed(headTwo)
	r.apply("fix-done", fixCompletion(headTwo))

	r.move(TriggerReviewApproved, "", reviewReport("approved"))

	r.assertRunEnded(3, 1, ResultPassed, 0)
	for _, start := range superviseRowsOf(t, r.pool, "LEGION-208", "start") {
		if start.Phase == phase.Retro {
			t.Fatalf("a retro start was queued before the way after the fix: %+v", start)
		}
	}
	for _, write := range stepWrites(t, r.pool, "LEGION-208") {
		if write.Kind == record.PlaceRetro {
			t.Fatalf("a record of a retro's run was written before the way after the fix: %+v", write)
		}
	}
	// The approval's own move would tell its phase-finished with no reason; the way's move back
	// names its reason.
	assertStrings(t, "the phase-finished notices of the approval", outboxStrings(t, r.pool, `select payload->>'kind' from outbox
		where kind = 'notice' and issue = $1 and payload->>'kind' = 'phase-finished' and payload->>'phase' = 'reviewing'
		and coalesce(payload->>'reason', '') = ''`, "LEGION-208"), nil)
	assertPhase(t, r.pool, phase.Testing)
}

// A step that passes beside the fix moves the task on as it does today: the tester's pass starts
// the review, and the fix runs on.
func TestAnApprovalBesideTheFixStartsTheNextStep(t *testing.T) {
	r := newFixRig(t, returnEngine(), []string{"implementer", "tester", "reviewer"}, TriggerImplementationReady)
	r.fails(fixHead, "k-ci")
	r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})

	r.move(TriggerTesterPassed, "", record.PhaseRow{})

	assertPhase(t, r.pool, phase.Reviewing)
	if starts := r.startsOf(claim.RoleReviewer); len(starts) != 1 {
		t.Fatalf("the reviewer's starts = %+v, want one", starts)
	}
	if fix := r.latestFix(); fix.State != record.FixRunning {
		t.Fatalf("the fix after the tester's pass = %+v, want it running", fix)
	}
}

// A review that requests changes beside the running fix returns the task to the fix's step, so its
// report goes to the fix: the fix holds it, the implementer gets one step-report row for the fix's
// work, which names no branch, since the reviewer pushes none, and no role message; and no step
// starts while the task is held for its fix from reviewing. The review's round is counted, and the
// review round cap's notice goes at the cap; no return is counted, since the way after the fix
// counts it. The control, kept: the same request after the fix ended returns the task at once,
// counting its round and its return.
func TestAChangesRequestBesideTheFixGoesToItAndCountsItsRound(t *testing.T) {
	engine := returnEngine()
	engine.cfg.ReviewRoundCap = 1
	r := newFixRig(t, engine, []string{"implementer", "tester", "reviewer"}, TriggerImplementationReady, TriggerTesterPassed)
	r.fails(fixHead, "k-ci")
	r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})

	r.move(TriggerReviewRejected, "", reviewReport("the widget renders twice"))

	if fix := r.latestFix(); !fix.GotReport || fix.State != record.FixRunning || fix.ReturnTo != 0 {
		t.Fatalf("the fix after the request for changes = %+v, want it running with the review's report", fix)
	}
	if got, want := r.stepReports(), []string{fmt.Sprintf("implementer fix-1@%d -", r.latestFix().Entry)}; !slices.Equal(got, want) {
		t.Fatalf("the step-report rows = %q, want %q", got, want)
	}
	assertStrings(t, "the implementer's messages", r.told(claim.RoleImplementer), nil)
	r.assertHeldForTheFix(phase.Reviewing)
	r.assertRunEnded(3, 1, ResultFailed, 1)
	// The outbox keeps one pending phase start of an issue, the latest; none is the implementer's.
	if starts := r.startsOf(claim.RoleImplementer); len(starts) != 0 {
		t.Fatalf("the implementer's pending starts = %+v, want none", starts)
	}
	if counts := r.counts(); counts.Rounds != 1 || counts.Pass != 0 || len(counts.BackwardMoves) != 0 {
		t.Fatalf("the task counts %+v, want one round and no return", counts)
	}
	assertStrings(t, "the cap's notices", outboxStrings(t, r.pool, `select payload->>'kind' from outbox
		where kind = 'notice' and issue = $1 and payload->>'kind' = 'pr-blocked'`, "LEGION-208"), []string{"pr-blocked"})

	t.Run("the same request after the fix ended returns the task at once", func(t *testing.T) {
		r := startedFix(t, intake.FixJobRead{Name: "unit", URL: fixPage})
		r.apply("fix-done", fixCompletion(fixHead))

		r.move(TriggerReviewRejected, "", reviewReport("the widget renders twice"))

		assertPhase(t, r.pool, phase.Implementing)
		if counts := r.counts(); counts.Rounds != 1 || counts.Pass != 1 || counts.BackwardMoves[phase.Pair(phase.Reviewing, phase.Implementing)] != 1 {
			t.Fatalf("the task counts %+v, want one round and one return under reviewing-implementing", counts)
		}
	})
}

// stepReports is each step-report row of LEGION-208, oldest first, as `<role> <reader> <branch>`,
// with `-` for no branch.
func (r *fixRig) stepReports() []string {
	r.t.Helper()
	var lines []string
	for _, n := range r.newsOf() {
		if n.Purpose != record.CINewsStepReport {
			continue
		}
		branch := n.Branch
		if branch == "" {
			branch = "-"
		}
		lines = append(lines, fmt.Sprintf("%s %s %s", roleOf(r.t, n.Role), readerLine(n.Reader), branch))
	}
	return lines
}

// A review that returns the task to the planner while the fix runs waits for the fix: the fix
// keeps the step the task returns to, the task is held for its fix from reviewing, no planner
// starts, and no return is counted yet.
func TestAReturnToThePlannerWaitsForTheFix(t *testing.T) {
	r := planningFix(t)

	r.move(TriggerBackward, phase.Planning, reviewReport("the plan is wrong"))

	if fix := r.latestFix(); fix.ReturnTo != 1 || fix.GotReport || fix.State != record.FixRunning {
		t.Fatalf("the fix = %+v, want it running, returning the task to step 1", fix)
	}
	r.assertHeldForTheFix(phase.Reviewing)
	r.assertRunEnded(4, 1, ResultFailed, 1)
	if starts := r.startsOf(claim.RolePlanner); len(starts) != 0 {
		t.Fatalf("the planner's pending starts = %+v, want none", starts)
	}
	if counts := r.counts(); counts.Pass != 0 {
		t.Fatalf("the task counts %+v, want no return yet", counts)
	}
}

// A review that returns the task to the tester while the fix runs waits for the fix: the fix keeps
// the step, the task is held for its fix, and no tester starts.
func TestAReturnToTheTesterWaitsForTheFix(t *testing.T) {
	r := startedFix(t, intake.FixJobRead{Name: "unit", URL: fixPage})

	r.move(TriggerBackward, phase.Testing, reviewReport("test the widget again"))

	if fix := r.latestFix(); fix.ReturnTo != 2 || fix.State != record.FixRunning {
		t.Fatalf("the fix = %+v, want it running, returning the task to step 2", fix)
	}
	r.assertHeldForTheFix(phase.Reviewing)
	if starts := r.startsOf(claim.RoleTester); len(starts) != 0 {
		t.Fatalf("the tester's pending starts = %+v, want none", starts)
	}
	if counts := r.counts(); counts.Pass != 0 {
		t.Fatalf("the task counts %+v, want no return yet", counts)
	}
}

// The hold for the fix stops no claim: it writes no suspend and no stop of a step, and the fix
// runs on, named by the issue's record.
func TestTheHoldForTheFixStopsNoClaim(t *testing.T) {
	r := startedFix(t, intake.FixJobRead{Name: "unit", URL: fixPage})

	r.move(TriggerReviewRejected, "", reviewReport("the widget renders twice"))

	r.assertHeldForTheFix(phase.Reviewing)
	for _, op := range []string{"suspend", "step_stop", "release"} {
		if rows := superviseRowsOf(t, r.pool, "LEGION-208", op); len(rows) != 0 {
			t.Fatalf("the hold for the fix wrote %s rows %+v, want none", op, rows)
		}
	}
	if issue := issueOf(t, r.pool, r.engine, "LEGION-208"); issue.RunningFix != 1 {
		t.Fatalf("the issue read names running fix %d, want 1", issue.RunningFix)
	}
	if fix := r.latestFix(); fix.State != record.FixRunning || !strings.Contains(fix.Check, "k-ci") {
		t.Fatalf("the fix under the hold = %+v, want it running", fix)
	}
}
