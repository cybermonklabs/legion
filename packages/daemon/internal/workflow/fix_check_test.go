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

// The check of a fix on the engine. A check that a CI job runs, failing at the head of a task
// while a later step runs, starts the task's first implementation step again as a fix, in a
// working copy of its own, while that step goes on; the fix's end decides what runs next. Each
// case is LEGION-208 on one task, and every fact of it goes through intake.ApplyFact and the
// engine as the daemon applies it: each step's completion, Dispatch's check.state_changed events,
// the review App's reviews, the read of the fix's failed job, and the fix's push. The testing move
// is decided by the states the engine holds at the tester's completion, never by a move of the
// test's own.

// vfixChecks is the set of checks of LEGION-208 at each head of these cases: k-unit, which its
// tester records, and k-ci and k-ci2, which a CI job runs.
var vfixChecks = []string{"k-unit", "k-ci", "k-ci2"}

// vfixRig is one case's issue, and the count of the facts the case applied, each under an event
// id of its own.
type vfixRig struct {
	*fixRig
	facts int
}

// newVFix is LEGION-208 on one task whose steps are words, its pull request #42 open at fixHead,
// and its first step started.
func newVFix(t *testing.T, words ...string) *vfixRig {
	t.Helper()
	return &vfixRig{fixRig: newFixRig(t, returnEngine(), words)}
}

// eventID is a new event id of this case, naming what.
func (r *vfixRig) eventID(what string) string {
	r.facts++
	return fmt.Sprintf("vfix-%d-%s", r.facts, what)
}

// states applies one check.state_changed event at head for each of states, each carrying
// vfixChecks as the head's set, at the serials after the highest the case applied.
func (r *vfixRig) states(head string, states ...intake.DispatchCheckState) {
	r.t.Helper()
	for _, state := range states {
		r.serial++
		applyStates(r.t, r.pool, r.engine, "LEGION-208", statesRepo, head, r.serial, ofSet(state, vfixChecks...))
	}
}

// ciFails applies, for each of checks, its state at head once its CI job ended with failure.
func (r *vfixRig) ciFails(head string, checks ...string) {
	r.t.Helper()
	for _, check := range checks {
		r.states(head, inCI(stateOf(check, "no", "fail", "ci"), "failure"))
	}
}

// completes applies role's completion of its step at commit, naming report as its handoff record
// and statesSerial as the serial of the check states its record was written after.
func (r *vfixRig) completes(role claim.Role, commit, report string, statesSerial int64) {
	r.t.Helper()
	fact := intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: role, Claim: claim.Token(string(role) + "-claim"),
		Summary: string(role) + " is done", Commit: commit, HandoffRecord: report, HandoffVersion: 1, StatesSerial: statesSerial}
	if result := r.apply(r.eventID(string(role)+"-done"), fact); result.Refusal != nil {
		r.t.Fatalf("the %s's completion at %q was refused: %+v", role, commit, result.Refusal)
	}
}

// toReview walks the task to its review: the planner's completion where the task starts with one,
// the implementer's at fixHead, Dispatch's states there (k-unit passed by the tester, k-ci and
// k-ci2 passed by their jobs), and the tester's completion at fixHead, whose testing move those
// states decide. The reviewer's claim then holds the session its reviews name.
func (r *vfixRig) toReview() {
	r.t.Helper()
	if issueOf(r.t, r.pool, r.engine, "LEGION-208").Phase == phase.Planning {
		r.completes(claim.RolePlanner, "", "plan-record", 0)
	}
	r.completes(claim.RoleImplementer, fixHead, "implement-record", 0)
	assertPhase(r.t, r.pool, phase.Testing)
	r.states(fixHead, passOf("k-unit", "tester"), ciPassing("k-ci"), ciPassing("k-ci2"))
	r.completes(claim.RoleTester, fixHead, "test-record", r.serial)
	assertPhase(r.t, r.pool, phase.Reviewing)
	seedClaimSession(r.t, r.pool, reviewClaim, "ses-reviewer")
}

// jobRead is Dispatch's answer to the read of fix 1's failed job: the job unit, and its page.
func (r *vfixRig) jobRead() {
	r.t.Helper()
	r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})
}

// fixStartsBesideTheReview is k-ci failing at fixHead while the review runs, and the answer of the
// read of the fix's job.
func (r *vfixRig) fixStartsBesideTheReview() {
	r.t.Helper()
	r.ciFails(fixHead, "k-ci")
	r.jobRead()
}

// reviewerNames applies the reviewer's move of the task back to the step of phase to.
func (r *vfixRig) reviewerNames(to phase.Phase) {
	r.t.Helper()
	move := intake.BackwardMove{Issue: "LEGION-208", Requester: claim.RoleReviewer, To: to, Reason: "the review finds the work belongs to an earlier step"}
	if result := r.apply(r.eventID("reviewer-back"), move); result.Refusal != nil {
		r.t.Fatalf("the reviewer's move back to %s was refused: %+v", to, result.Refusal)
	}
}

// fixEndsAt is the fix's own completion at end: where end is not fixHead, the fix's push of end
// to the branch of the task first, as GitHub delivers it.
func (r *vfixRig) fixEndsAt(end string) {
	r.t.Helper()
	if end != fixHead {
		r.fixPushes(end)
	}
	if result := r.apply(r.eventID("fix-done"), fixCompletion(end)); result.Refusal != nil {
		r.t.Fatalf("the fix's completion at %s was refused: %+v", end, result.Refusal)
	}
}

// startsAt is how many times the step at place of the task started, from the store's count.
func (r *vfixRig) startsAt(place int) string {
	r.t.Helper()
	prefix := fmt.Sprintf("a/step/%d=", place)
	for _, count := range stepStartCounts(r.t, r.pool, "LEGION-208") {
		if strings.HasPrefix(count, prefix) {
			return count
		}
	}
	return prefix + "0"
}

// assertStartsAt fails unless the step at place of the task started starts times.
func (r *vfixRig) assertStartsAt(place, starts int, why string) {
	r.t.Helper()
	if got, want := r.startsAt(place), fmt.Sprintf("a/step/%d=%d", place, starts); got != want {
		r.t.Fatalf("the starts of step %d are %s, want %s: %s", place, got, want, why)
	}
}

// fixRunCheck is the check the start record of the fix's run of the step at place names, "" where
// the record names none or was not written.
func (r *vfixRig) fixRunCheck(place int) string {
	r.t.Helper()
	id := StepID("LEGION-208", 1, record.StepPlace{Task: "a", Kind: record.PlaceStep, Place: place}, 2)
	for _, write := range stepWrites(r.t, r.pool, "LEGION-208") {
		if write.ID == id && isStart(write) && write.FixCheck != nil {
			return *write.FixCheck
		}
	}
	return ""
}

// fixBesideTheReview says why fix 1 of k-ci at fixHead does not run beside the review, nil where
// it does: the issue stands in reviewing with the review's run open and the issue's record naming
// fix 1 as running; the fix runs at the implementer's place; one start of the implementer is
// queued for the fix, in implementing, its working copy its own one made at fixHead, waiting for
// no slot, its task naming each of checks, fixHead, the job and the job's page; the record of the
// fix's run names k-ci; and the reviewer is told of the fix once, by a message naming k-ci and
// fixHead. place is the implementer's place in the task.
func (r *vfixRig) fixBesideTheReview(place int, checks ...string) error {
	r.t.Helper()
	if issue := issueOf(r.t, r.pool, r.engine, "LEGION-208"); issue.Phase != phase.Reviewing || issue.RunningFix != 1 {
		return fmt.Errorf("LEGION-208 stands in %s naming running fix %d, want reviewing with fix 1 running beside the review", issue.Phase, issue.RunningFix)
	}
	if end := r.runEnd(place+2, 1); end != nil {
		return fmt.Errorf("the review's run ended %+v, want it running beside the fix", end)
	}
	if got, want := r.fixes(), []string{fmt.Sprintf("1 running k-ci@%s place %d", fixHead, place)}; !slices.Equal(got, want) {
		return fmt.Errorf("the fixes = %q, want %q", got, want)
	}
	starts := r.fixStarts()
	if len(starts) != 1 {
		return fmt.Errorf("the starts of a fix = %+v, want one", starts)
	}
	start := starts[0]
	if start.Role != claim.RoleImplementer || start.Phase != phase.Implementing || start.Fix != 1 || start.FixFrom != fixHead || start.NeedsSlot {
		return fmt.Errorf("the fix's start = %+v, want the implementer's step again in implementing for fix 1, in a working copy of its own made at %s, waiting for no slot",
			start, fixHead)
	}
	for _, value := range append(slices.Clone(checks), fixHead, "unit", fixPage) {
		if !strings.Contains(start.Task, value) {
			return fmt.Errorf("the fix's task %q does not name %q", start.Task, value)
		}
	}
	if got := r.fixRunCheck(place); got != "k-ci" {
		return fmt.Errorf("the start record of the fix's run names the check %q, want k-ci", got)
	}
	var told []string
	for _, message := range r.told(claim.RoleReviewer) {
		if strings.Contains(message, "k-ci") && strings.Contains(message, fixHead) {
			told = append(told, message)
		}
	}
	if len(told) != 1 {
		return fmt.Errorf("the reviewer's messages naming k-ci at %s = %q, want one: the review is told of the fix", fixHead, told)
	}
	return nil
}

// assertFixBesideTheReview fails unless fixBesideTheReview finds the fix beside the review.
func (r *vfixRig) assertFixBesideTheReview(place int, checks ...string) {
	r.t.Helper()
	if err := r.fixBesideTheReview(place, checks...); err != nil {
		r.t.Fatal(err)
	}
}

// vfixRow is a row of the table of a review beside a fix: how the review ends (review), whether
// after the fix ended (afterEnd), the head the fix ends at (end, fixHead for no commit), the way
// the fix's end takes, and for the first two ways the phase the task returns to, the role that
// starts there, and the pair its one return is counted under.
type vfixRow struct {
	n        int
	name     string
	review   string
	afterEnd bool
	end      string
	way      int
	at       phase.Phase
	role     claim.Role
	pair     string
}

// vfixRows are the eight rows of the table, on `implementer tester reviewer`.
var vfixRows = []vfixRow{
	{1, "an approval while the fix runs, the fix with new commits", "approved", false, headTwo, wayAfterFix, phase.Testing, claim.RoleTester, retroTesting},
	{2, "an approval while the fix runs, the fix with no commit", "approved", false, fixHead, wayOn, "", "", ""},
	{3, "changes while the fix runs, the fix with new commits", "changes_requested", false, headTwo, wayAfterFix, phase.Testing, claim.RoleTester, reviewingTesting},
	{4, "changes while the fix runs, the fix with no commit", "changes_requested", false, fixHead, wayAfterFix, phase.Testing, claim.RoleTester, reviewingTesting},
	{5, "an approval after the fix ended with new commits", "approved", true, headTwo, wayAfterFix, phase.Testing, claim.RoleTester, reviewingTesting},
	{6, "changes after the fix ended with new commits", "changes_requested", true, headTwo, wayReturn, phase.Implementing, claim.RoleImplementer, reviewingImplementing},
	{7, "an approval after the fix ended with no commit", "approved", true, fixHead, wayOn, "", "", ""},
	{8, "changes after the fix ended with no commit", "changes_requested", true, fixHead, wayReturn, phase.Implementing, claim.RoleImplementer, reviewingImplementing},
}

// runRow walks one row: the fix starts beside the review, then the review ends with row's decision
// of fixHead, before or after the fix's end at row's head.
func runRow(t *testing.T, row vfixRow) {
	r := newVFix(t, "implementer", "tester", "reviewer")
	r.toReview()
	r.fixStartsBesideTheReview()
	r.assertFixBesideTheReview(1, "k-ci")

	if !row.afterEnd {
		r.reviewOf(1, row.review, fixHead)
		if row.review == "approved" {
			r.assertEndHold(record.HoldForFix)
		} else {
			if fix := r.latestFix(); !fix.GotReport || fix.State != record.FixRunning {
				t.Fatalf("the fix after the request for changes = %+v, want it running with the review's report", fix)
			}
			r.assertHeldForTheFix(phase.Reviewing)
		}
		r.assertStartsAt(2, 1, "no step after the review's end starts while the fix runs")
	}
	r.fixEndsAt(row.end)
	if row.afterEnd {
		assertPhase(t, r.pool, phase.Reviewing)
		r.assertStartsAt(2, 1, "no step starts at X once the fix ended while the review runs")
		r.reviewOf(1, row.review, fixHead)
	}

	switch row.way {
	case wayOn:
		if fix := r.latestFix(); fix.Way != wayOn {
			t.Fatalf("the fix took way %d, want %d: %+v", fix.Way, wayOn, fix)
		}
		if counts := r.counts(); counts.Pass != 0 {
			t.Fatalf("the task counts %+v, want no return", counts)
		}
		r.assertEndHold(record.HoldForCI)
		r.assertTaskAtItsRetro()
		r.states(fixHead, ciPassing("k-ci"))
		r.assertRetroStarted()
	default:
		r.assertWay(row.way, row.at, row.role, row.end, 1, row.pair)
		if row.role == claim.RoleTester {
			r.assertStartsAt(2, 2, "the tester runs again after the fix")
		}
	}
}

// TestVFix runs the cases of the fix that the engine of this branch can run, each a subtest:
//
//   - 1 to 8, the rows of the table of a review beside a fix: the fix starts while the review runs,
//     in a working copy of its own, its task naming the check, the head, the job and its page, and
//     the review is told of it; then the steps that run next, the head their working copy moves
//     to, the commit the next review reads from, and the returns the task counts.
//   - 10: the check fails while the tester runs, and the tester's own check passes there: the
//     testing step passes, and the review starts while the fix runs.
//   - 12: on `planner implementer tester reviewer` the review names the planner while the fix
//     runs: the planner starts only after the fix ended.
//   - 13: the fix reports that it failed: the task returns as its report says, after the review
//     ended.
//   - 14: the owner stops the task while the fix runs: the fix and the review end, and no step
//     starts.
//   - 15: the check fails at a head the task has left: no fix starts.
//   - 16: two checks fail at one head while the fix reads its job: one fix, which gets both.
//   - 18: the review names the tester while a fix with new commits runs: the tester starts only
//     after the fix ended, at the new head.
//   - 19: the review names the planner, and the check fails while the planner runs: no fix starts,
//     and the implementer's task names the failed check.
//
// Cases 9, 11 and 17, and case 16's wait for a busy agent, need a task in which one agent holds two
// steps, a role holds two places, or a second task: each subtest names the change that brings it.
// The control, kept as a subtest: with the fix rule taken out of the engine (the seam noFixRule),
// case 1's check fails, since its implementer then starts only after the review ended.
func TestVFix(t *testing.T) {
	for _, row := range vfixRows {
		t.Run(fmt.Sprintf("case %d: %s", row.n, row.name), func(t *testing.T) { runRow(t, row) })
	}

	t.Run("case 10: the check fails while the tester runs, and the testing step passes", func(t *testing.T) {
		t.Skip("needs the testing step that leaves each check a CI job runs to the fix, which this branch does not hold yet: until then the tester's completion reads k-ci's failure and its step fails")
		r := newVFix(t, "implementer", "tester", "reviewer")
		r.completes(claim.RoleImplementer, fixHead, "implement-record", 0)
		r.states(fixHead, passOf("k-unit", "tester"), jobRunning("k-ci"), ciPassing("k-ci2"))
		r.ciFails(fixHead, "k-ci")
		r.jobRead()
		if fix := r.latestFix(); fix.State != record.FixRunning || fix.Check != "k-ci" || fix.Head != fixHead {
			t.Fatalf("the fix while the tester runs = %+v, want it running for k-ci at %s", fix, fixHead)
		}
		r.completes(claim.RoleTester, fixHead, "test-record", r.serial)
		assertPhase(t, r.pool, phase.Reviewing)
		r.assertRunEnded(2, 1, ResultPassed, 0)
		if starts := r.startsOf(claim.RoleReviewer); len(starts) != 1 || starts[0].Phase != phase.Reviewing {
			t.Fatalf("the reviewer's starts = %+v, want one in reviewing", starts)
		}
		if fix := r.latestFix(); fix.State != record.FixRunning {
			t.Fatalf("the fix once the review started = %+v, want it running beside the review", fix)
		}
	})

	t.Run("case 12: the review names the planner while the fix runs", func(t *testing.T) {
		r := newVFix(t, "planner", "implementer", "tester", "reviewer")
		r.toReview()
		r.fixStartsBesideTheReview()
		r.assertFixBesideTheReview(2, "k-ci")

		r.reviewerNames(phase.Planning)
		if fix := r.latestFix(); fix.ReturnTo != 1 || fix.State != record.FixRunning {
			t.Fatalf("the fix after the review named the planner = %+v, want it running, the task returning to step 1", fix)
		}
		r.assertHeldForTheFix(phase.Reviewing)
		r.assertStartsAt(1, 1, "the planner waits for the fix")

		r.fixEndsAt(headTwo)
		if fix := r.latestFix(); fix.Way != wayReturn {
			t.Fatalf("the fix took way %d, want %d: %+v", fix.Way, wayReturn, fix)
		}
		assertPhase(t, r.pool, phase.Planning)
		r.assertStartsAt(1, 2, "the planner starts once the fix ended")
		if counts := r.counts(); counts.Pass != 1 {
			t.Fatalf("the task counts %+v, want one return", counts)
		}
	})

	t.Run("case 13: the fix reports that it failed", func(t *testing.T) {
		r := newVFix(t, "planner", "implementer", "tester", "reviewer")
		r.toReview()
		r.fixStartsBesideTheReview()
		r.assertFixBesideTheReview(2, "k-ci")

		if result := r.apply(r.eventID("fix-back"), fixMoveBack(phase.Planning)); result.Refusal != nil {
			t.Fatalf("the fix's report of its failure was refused: %+v", result.Refusal)
		}
		r.assertFixEnded("failed", fixHead, true, 1)
		assertPhase(t, r.pool, phase.Reviewing)
		r.assertStartsAt(1, 1, "the task returns only after the review ended")

		r.reviewOf(1, "approved", fixHead)
		if fix := r.latestFix(); fix.Way != wayReturn {
			t.Fatalf("the fix took way %d, want %d: %+v", fix.Way, wayReturn, fix)
		}
		assertPhase(t, r.pool, phase.Planning)
		r.assertStartsAt(1, 2, "the task returns to the step the fix's report names")
		if counts := r.counts(); counts.Pass != 1 {
			t.Fatalf("the task counts %+v, want one return", counts)
		}
	})

	t.Run("case 14: the owner stops the task while the fix runs", func(t *testing.T) {
		r := newVFix(t, "implementer", "tester", "reviewer")
		r.toReview()
		r.fixStartsBesideTheReview()
		r.assertFixBesideTheReview(1, "k-ci")
		before := len(superviseRowsOf(t, r.pool, "LEGION-208", "start"))

		r.stop()

		r.assertWayDropped("stopped", fixHead)
		r.assertRunEnded(3, 1, ResultStopped, 0)
		r.assertRunEnded(1, 2, ResultStopped, 0)
		stops := r.stepStops()
		slices.Sort(stops)
		assertStrings(t, "the step_stops", stops, []string{string(claim.RoleImplementer), string(claim.RoleReviewer)})
		if after := len(superviseRowsOf(t, r.pool, "LEGION-208", "start")); after != before {
			t.Fatalf("the stop left %d starts, beside %d before it: no step starts after a stop", after, before)
		}
		issue := issueOf(t, r.pool, r.engine, "LEGION-208")
		if issue.Hold == nil || issue.Hold.Reason != record.HoldTaskStopped || issue.RunningFix != 0 {
			t.Fatalf("LEGION-208 after the stop holds %+v with running fix %d, want the task stopped and no fix", issue.Hold, issue.RunningFix)
		}
	})

	t.Run("case 15: the check fails at a head the task has left", func(t *testing.T) {
		r := newVFix(t, "implementer", "tester", "reviewer")
		r.toReview()

		r.ciFails(formerHead, "k-ci")

		assertStrings(t, "the fixes", r.fixes(), nil)
		assertStrings(t, "the failures", r.failures(), nil)
		if reads := r.jobReads(); len(reads) != 0 {
			t.Fatalf("the job reads = %+v, want none", reads)
		}
		assertStrings(t, "the implementer's messages", r.told(claim.RoleImplementer), nil)
		assertPhase(t, r.pool, phase.Reviewing)
	})

	t.Run("case 16: two checks fail at one head while the fix reads its job", func(t *testing.T) {
		r := newVFix(t, "implementer", "tester", "reviewer")
		r.toReview()

		r.ciFails(fixHead, "k-ci", "k-ci2")

		assertStrings(t, "the fixes", r.fixes(), []string{fixOf(1, "reading", "k-ci")})
		assertStrings(t, "the failures", r.failures(), []string{failureOf("k-ci", "fix", 1), failureOf("k-ci2", "fix", 1)})
		if got := r.jobReads(); len(got) != 1 || got[0] != oneJobRead("a", "k-ci")[0] {
			t.Fatalf("the job reads = %+v, want the one of fix 1's job", got)
		}
		r.jobRead()
		r.assertFixBesideTheReview(1, "k-ci", "k-ci2")
	})

	t.Run("case 18: the review names the tester while a fix with new commits runs", func(t *testing.T) {
		r := newVFix(t, "implementer", "tester", "reviewer")
		r.toReview()
		r.fixStartsBesideTheReview()
		r.assertFixBesideTheReview(1, "k-ci")

		r.reviewerNames(phase.Testing)
		if fix := r.latestFix(); fix.ReturnTo != 2 || fix.State != record.FixRunning {
			t.Fatalf("the fix after the review named the tester = %+v, want it running, the task returning to step 2", fix)
		}
		r.assertHeldForTheFix(phase.Reviewing)
		r.assertStartsAt(2, 1, "the tester waits for the fix")

		r.fixEndsAt(headTwo)
		if fix := r.latestFix(); fix.Way != wayReturn {
			t.Fatalf("the fix took way %d, want %d: %+v", fix.Way, wayReturn, fix)
		}
		assertPhase(t, r.pool, phase.Testing)
		r.assertStartsAt(2, 2, "the tester starts once the fix ended")
		if starts := r.startsOf(claim.RoleTester); len(starts) == 0 || starts[len(starts)-1].MoveTo != headTwo {
			t.Fatalf("the tester's starts = %+v, want the latest at the new head %s", starts, headTwo)
		}
		if counts := r.counts(); counts.Pass != 1 {
			t.Fatalf("the task counts %+v, want one return", counts)
		}
	})

	t.Run("case 19: the review names the planner, and the check fails while the planner runs", func(t *testing.T) {
		r := newVFix(t, "planner", "implementer", "tester", "reviewer")
		r.toReview()
		r.reviewerNames(phase.Planning)
		assertPhase(t, r.pool, phase.Planning)

		r.ciFails(fixHead, "k-ci")

		assertStrings(t, "the fixes while the planner runs", r.fixes(), nil)
		assertStrings(t, "the failures while the planner runs", r.failures(), nil)
		r.completes(claim.RolePlanner, "", "plan-record-2", 0)
		assertPhase(t, r.pool, phase.Implementing)
		assertStrings(t, "the fixes", r.fixes(), nil)
		assertStrings(t, "the failures", r.failures(), []string{failureOf("k-ci", "implementer", 0)})
		starts := r.startsOf(claim.RoleImplementer)
		if len(starts) == 0 {
			t.Fatal("the implementer has no start after the planner's completion")
		}
		assertNames(t, "the implementer's task", starts[len(starts)-1].Task, []string{"k-ci", fixHead}, "k-ci2")
	})

	for _, other := range []struct{ name, change string }{
		{"case 9: one agent holds the implementation step and the review, and the review requests changes", "#5156"},
		{"case 11: the check fails while a second implementation step runs", "#5157"},
		{"case 16 with the fix waiting for its agent", "#5156"},
		{"case 17: the implementer's agent does a step of another task", "#5156, and whole with tasks at one time"},
	} {
		t.Run(other.name, func(t *testing.T) {
			t.Skipf("a task of this branch has one agent for each step and one place for each role; this case runs with %s", other.change)
		})
	}

	t.Run("the control: with the fix rule taken out, case 1's implementer starts only after the review ended", func(t *testing.T) {
		r := newVFix(t, "implementer", "tester", "reviewer")
		r.engine.noFixRule = true
		r.toReview()
		r.ciFails(fixHead, "k-ci")

		err := r.fixBesideTheReview(1, "k-ci")
		if err == nil {
			t.Fatal("case 1's check passed with the fix rule taken out")
		}
		t.Logf("case 1's check with the fix rule taken out: %v", err)
		r.assertStartsAt(1, 1, "with no fix the implementer does not start while the review runs")

		r.reviewOf(1, "approved", fixHead)
		r.assertRunEnded(3, 1, ResultPassed, 0)
		r.assertEndHold(record.HoldForCI)
		r.assertStartsAt(1, 1, "with no fix the implementer does not start at the review's end")
		if result := r.ownerMovesBack(); result.Refusal != nil {
			t.Fatalf("the owner's move back was refused: %+v", result.Refusal)
		}
		assertPhase(t, r.pool, phase.Implementing)
		r.assertStartsAt(1, 2, "the implementer starts only after the review ended")
		if starts := r.fixStarts(); len(starts) != 0 {
			t.Fatalf("the starts of a fix = %+v, want none with the fix rule taken out", starts)
		}
	})
}
