package workflow

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// fixRecord is the handoff record the implementer's completion of a fix names.
const fixRecord = "fix-implement-record"

// fixCompletion is the implementer's completion of fix 1 of LEGION-208's task, reporting commit.
func fixCompletion(commit string) intake.HandoffComplete {
	return intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implementer-claim",
		Summary: "the fix is pushed", Commit: commit, HandoffRecord: fixRecord, HandoffVersion: 2, Fix: 1}
}

// fixMoveBack is the implementer's move of LEGION-208 back to to while it runs fix 1.
func fixMoveBack(to phase.Phase) intake.BackwardMove {
	return intake.BackwardMove{Issue: "LEGION-208", Requester: claim.RoleImplementer, To: to, Reason: "the check fails for the plan's design", Fix: 1}
}

// apply applies fact as the event id, failing the test on an error, and answers its result.
func (r *fixRig) apply(id string, fact intake.Fact) intake.Result {
	r.t.Helper()
	return applyFacts(r.t, r.pool, r.engine)(id, fact)
}

// fixRunEnd is the end record of the fix's run of the step at place of LEGION-208's task a: the
// second run of that step, which the fix started. The test fails where none was written.
func (r *fixRig) fixRunEnd(place int) record.StepRecord {
	r.t.Helper()
	id := StepID("LEGION-208", 1, record.StepPlace{Task: "a", Kind: record.PlaceStep, Place: place}, 2)
	for _, write := range stepWrites(r.t, r.pool, "LEGION-208") {
		if write.ID == id && isEnd(write) {
			return write
		}
	}
	r.t.Fatalf("LEGION-208's records hold no end of the fix's run of step %d", place)
	return record.StepRecord{}
}

// assertFixEnded fails unless the latest fix of LEGION-208's task a ended with result at endHead,
// failed as failed says, returning to returnTo, its way not yet taken, and the issue read names no
// running fix.
func (r *fixRig) assertFixEnded(result string, endHead string, failed bool, returnTo int) record.TaskFix {
	r.t.Helper()
	fix := r.latestFix()
	if fix.State != record.FixEnded || fix.Result != result || fix.EndHead != endHead || fix.Failed != failed ||
		fix.ReturnTo != returnTo || fix.Way != 0 || fix.EndedAt == nil {
		r.t.Fatalf("the fix = %+v, want it ended %s at %s, failed %v, returning to %d, its way not taken", fix, result, endHead, failed, returnTo)
	}
	if issue := issueOf(r.t, r.pool, r.engine, "LEGION-208"); issue.RunningFix != 0 {
		r.t.Fatalf("the issue read names running fix %d after the fix ended, want none", issue.RunningFix)
	}
	return fix
}

// A fix's own completion ends the fix, not the step the issue stands at: the fix ends passed at
// the commit the completion reports, the run of the step the fix ran again ends passed with the
// completion's report, and the issue stays in reviewing beside the review that goes on, so the way
// after the fix is not taken yet. The fix made new commits: its end is not the head it started at.
// The control, kept: a fix that ends at its own head reads no new commits.
func TestAFixEndsWithNewCommits(t *testing.T) {
	r := startedFix(t, intake.FixJobRead{Name: "unit", URL: fixPage})
	if result := r.apply("fix-done", fixCompletion(headTwo)); result.Refusal != nil {
		t.Fatalf("the fix's completion was refused: %+v", result.Refusal)
	}

	fix := r.assertFixEnded("passed", headTwo, false, 0)
	if fix.EndHead == fix.Head {
		t.Fatalf("the fix ended at %s, the head it started at: it reads no new commits", fix.EndHead)
	}
	assertPhase(t, r.pool, phase.Reviewing)
	end := r.fixRunEnd(1)
	if end.Result == nil || *end.Result != string(ResultPassed) || end.Report == nil || *end.Report != fixRecord {
		t.Fatalf("the end of the fix's run = %+v, want passed with the report %s", end, fixRecord)
	}
	assertStrings(t, "the runs", r.runs(), []string{"a/step/1 ended", "a/step/2 ended", "a/step/3 open"})
	if row := roleRowOf(t, r.pool, r.engine, "LEGION-208", claim.RoleImplementer); row.HandoffCommit != headTwo || row.HandoffRecord != fixRecord {
		t.Fatalf("the implementer's row holds %s naming %s, want the fix's completion at %s naming %s", row.HandoffCommit, row.HandoffRecord, headTwo, fixRecord)
	}

	t.Run("a fix that ends at its own head reads no new commits", func(t *testing.T) {
		r := startedFix(t, intake.FixJobRead{Name: "unit", URL: fixPage})
		r.apply("fix-done", fixCompletion(fixHead))
		if fix := r.assertFixEnded("passed", fixHead, false, 0); fix.EndHead != fix.Head {
			t.Fatalf("the fix ended at %s, beside the head %s it started at: it reads new commits", fix.EndHead, fix.Head)
		}
	})
}

// A fix's completion that names no commit ends the fix at the task's head as it stands: only the
// fix pushes while it runs, so the head is where the fix left it.
func TestAFixEndsWithNoCommit(t *testing.T) {
	r := startedFix(t, intake.FixJobRead{Name: "unit", URL: fixPage})
	seedPR(t, r.pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: statesRepo, Number: 42,
		Branch: "legion/LEGION-208", HeadSHA: headTwo, Failing: []string{}})

	if result := r.apply("fix-done", fixCompletion("")); result.Refusal != nil {
		t.Fatalf("the fix's completion was refused: %+v", result.Refusal)
	}

	r.assertFixEnded("passed", headTwo, false, 0)
	assertPhase(t, r.pool, phase.Reviewing)
}

// planningFix is LEGION-208 in reviewing on `planner implementer tester reviewer`, k-ci failed at
// fixHead and fix 1 running at the implementer's place, 2.
func planningFix(t *testing.T) *fixRig {
	t.Helper()
	r := newFixRig(t, returnEngine(), []string{"planner", "implementer", "tester", "reviewer"},
		TriggerPlannerCompleted, TriggerImplementationReady, TriggerTesterPassed)
	r.fails(fixHead, "k-ci")
	r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})
	if fix := r.latestFix(); fix.State != record.FixRunning || fix.Place != 2 {
		t.Fatalf("the fix = %+v, want it running at place 2", fix)
	}
	return r
}

// A fix that reports a failure by the implementer's move back names where the task returns: the
// fix ends failed, returning to the step its move names, before the fix's own step, and the end of
// its run names that step too. Nothing moves yet: the review beside the fix goes on.
func TestAFixThatReportsFailedNamesItsReturn(t *testing.T) {
	r := planningFix(t)

	if result := r.apply("fix-back", fixMoveBack(phase.Planning)); result.Refusal != nil {
		t.Fatalf("the fix's move back was refused: %+v", result.Refusal)
	}

	r.assertFixEnded("failed", fixHead, true, 1)
	end := r.fixRunEnd(2)
	if end.Result == nil || *end.Result != string(ResultFailed) || end.ReturnedTo == nil || *end.ReturnedTo != 1 {
		t.Fatalf("the end of the fix's run = %+v, want failed, returned to step 1", end)
	}
	assertPhase(t, r.pool, phase.Reviewing)
}

// The implementer's move back while it runs a fix is the fix's report, taken while the issue
// stands in reviewing, whose role is the reviewer's: the base's rule that a move is the role
// running the issue's phase does not refuse it. A move to a step at or after the fix's own place is
// refused, and leaves the fix running. The control, kept: the implementer's move back with no fix
// running in reviewing is refused with the base's words.
func TestTheFixsBackwardMoveIsTakenWhileTheIssueIsInReviewing(t *testing.T) {
	r := planningFix(t)

	result := r.apply("fix-back-testing", fixMoveBack(phase.Testing))
	if refusalCode(result) != "BACKWARD_REFUSED" || !strings.Contains(result.Refusal.Message, "fix 1") {
		t.Fatalf("the fix's move to testing = %+v, want BACKWARD_REFUSED naming fix 1", result.Refusal)
	}
	if fix := r.latestFix(); fix.State != record.FixRunning {
		t.Fatalf("the fix after a refused move = %+v, want it running", fix)
	}

	if result := r.apply("fix-back", fixMoveBack(phase.Planning)); result.Refusal != nil {
		t.Fatalf("the fix's move back was refused: %+v", result.Refusal)
	}
	r.assertFixEnded("failed", fixHead, true, 1)
	assertPhase(t, r.pool, phase.Reviewing)

	t.Run("with no fix running the base's rule refuses the implementer's move", func(t *testing.T) {
		r := newFixRig(t, returnEngine(), []string{"planner", "implementer", "tester", "reviewer"},
			TriggerPlannerCompleted, TriggerImplementationReady, TriggerTesterPassed)
		move := fixMoveBack(phase.Planning)
		move.Fix = 0
		result := r.apply("implementer-back", move)
		if refusalCode(result) != "BACKWARD_REFUSED" || !strings.Contains(result.Refusal.Message, "a backward move is the role running the issue's phase") {
			t.Fatalf("the implementer's move = %+v, want BACKWARD_REFUSED in the base's words", result.Refusal)
		}
		assertPhase(t, r.pool, phase.Reviewing)
	})
}

// A completion of a fix that runs no more is refused, changing nothing: the fix ended at its own
// completion, so the same completion sent again, at another commit, and a completion naming a fix
// the task never ran each meet the fence of the fix's entry. The fix stays as it ended, and the
// implementer's row keeps the first completion.
func TestAStaleCompletionOfAnEndedFixIsRefused(t *testing.T) {
	r := startedFix(t, intake.FixJobRead{Name: "unit", URL: fixPage})
	r.apply("fix-done", fixCompletion(headTwo))
	ended := r.latestFix()

	for i, completion := range []intake.HandoffComplete{fixCompletion(headThree), func() intake.HandoffComplete {
		other := fixCompletion(headThree)
		other.Fix = 2
		return other
	}()} {
		result := r.apply(fmt.Sprintf("stale-%d", i), completion)
		if refusalCode(result) != "HANDOFF_STALE_ENTRY" || !strings.Contains(result.Refusal.Message, fmt.Sprintf("fix %d", completion.Fix)) {
			t.Fatalf("the completion of fix %d = %+v, want HANDOFF_STALE_ENTRY naming the fix", completion.Fix, result.Refusal)
		}
	}
	if fix := r.latestFix(); fix.EndHead != ended.EndHead || fix.Result != ended.Result || fix.State != record.FixEnded {
		t.Fatalf("the fix after stale completions = %+v, want it as it ended: %+v", fix, ended)
	}
	if row := roleRowOf(t, r.pool, r.engine, "LEGION-208", claim.RoleImplementer); row.HandoffCommit != headTwo {
		t.Fatalf("the implementer's row holds %s, want the first completion's %s", row.HandoffCommit, headTwo)
	}
}

// A push of a step beside the fix is not the fix's: the tester pushed headTwo beside fix 1, and the
// fix reports the head it started at, so the fix ends at that head and reads no new commits,
// whatever head the pull request stands at.
func TestATesterPushBesideAFixDoesNotReadAsTheFixs(t *testing.T) {
	r := startedFix(t, intake.FixJobRead{Name: "unit", URL: fixPage})
	seedPR(t, r.pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: statesRepo, Number: 42,
		Branch: "legion/LEGION-208", HeadSHA: headTwo, Failing: []string{}})

	r.apply("fix-done", fixCompletion(fixHead))

	if fix := r.assertFixEnded("passed", fixHead, false, 0); fix.EndHead != fix.Head {
		t.Fatalf("the fix ended at %s beside its start %s: the tester's push read as the fix's", fix.EndHead, fix.Head)
	}
}
