package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// fixHead is the head of LEGION-208's pull request #42 in these tests, and formerHead a head the
// pull request has left, at which Dispatch still publishes states.
const (
	fixHead    = headOne
	formerHead = headEarlier
)

// fixOf is fixes' line of fix n in state for check at fixHead, at place 1.
func fixOf(n int, state, check string) string {
	return fmt.Sprintf("%d %s %s@%s place 1", n, state, check, fixHead)
}

// failureOf is failures' line of check's failure at fixHead, gone to wentTo with fix.
func failureOf(check, wentTo string, fix int) string {
	return fmt.Sprintf("%s@%s %s %d", check, fixHead, wentTo, fix)
}

// settledBody is the role message of the listener's settlement settledAt(fixHead, 1, "unit").
var settledBody = toldBody("CI settled at " + fixHead + ": \"build\" succeeded, \"unit\" failed")

// fixChecks is the set of checks of LEGION-208 at every head of these tests: k-unit, which no CI
// job runs, and k-ci and k-ci2, which a CI job runs.
var fixChecks = []string{"k-unit", "k-ci", "k-ci2"}

// fixRig is LEGION-208 on the plan its owner gave, or on the project's default steps, whose first
// step started in this release, so its task takes a fix, and every fact applied through engine.
type fixRig struct {
	t      *testing.T
	pool   *pgxpool.Pool
	engine *Engine
	serial int64
}

// newFixRig seeds LEGION-208 at admitted, its pull request #42 open at fixHead, on the plan whose
// one task's steps are words (none: the project's default steps), starts its first step, and
// moves it by each of moves, as the transitions of its line move it: the tests stand past the
// testing step without deciding it.
func newFixRig(t *testing.T, engine *Engine, words []string, moves ...TriggerKind) *fixRig {
	t.Helper()
	pool := migratedPool(t)
	root := stepRoot(t, pool, phase.Admitted)
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: root.Key, Repo: statesRepo, Number: 42,
		Branch: "legion/LEGION-208", HeadSHA: fixHead, Failing: []string{}})
	if len(words) > 0 {
		seedStepsPlan(t, pool, root.Key, root.Generation, words...)
	}
	firstStep(t, pool, engine, root.Key)
	for _, trigger := range moves {
		move(t, pool, engine, root.Key, trigger, "", record.PhaseRow{})
	}
	return &fixRig{t: t, pool: pool, engine: engine}
}

// reviewing is LEGION-208 on `implementer tester reviewer`, its reviewer at work.
func reviewing(t *testing.T) *fixRig {
	t.Helper()
	return newFixRig(t, returnEngine(), []string{"implementer", "tester", "reviewer"}, TriggerImplementationReady, TriggerTesterPassed)
}

// fails applies one check.state_changed event at head for each of checks: a check a CI job runs,
// read no for a fail after its job ended with failure, beside fixChecks as the head's set.
func (r *fixRig) fails(head string, checks ...string) {
	r.t.Helper()
	for _, check := range checks {
		r.serial++
		applyStates(r.t, r.pool, r.engine, "LEGION-208", statesRepo, head, r.serial, ofSet(inCI(stateOf(check, "no", "fail", "ci"), "failure"), fixChecks...))
	}
}

// fixes is each fix of LEGION-208's task as `<fix> <state> <check>@<head> place <place>`, by number.
func (r *fixRig) fixes() []string {
	r.t.Helper()
	return outboxStrings(r.t, r.pool, `select fix || ' ' || state || ' ' || check_id || '@' || head || ' place ' || place
		from task_fixes where issue = $1 order by fix`, "LEGION-208")
}

// failures is each failure of a check at a head of LEGION-208's task as `<check>@<head> <went_to>
// <fix>`, by check and head.
func (r *fixRig) failures() []string {
	r.t.Helper()
	return outboxStrings(r.t, r.pool, `select check_id || '@' || head || ' ' || went_to || ' ' || fix
		from ci_failures where issue = $1 order by check_id, head`, "LEGION-208")
}

// jobReads is each read of a fix's failed job the outbox holds for LEGION-208, oldest first.
func (r *fixRig) jobReads() []record.FixJobRead {
	r.t.Helper()
	reads := []record.FixJobRead{}
	for _, row := range issueOutboxOf(r.t, r.pool, "LEGION-208", string(record.OutboxKindFixJobRead)) {
		var read record.FixJobRead
		if err := json.Unmarshal(row.payload, &read); err != nil {
			r.t.Fatalf("decode the job read %s: %v", row.payload, err)
		}
		reads = append(reads, read)
	}
	return reads
}

// told is the body of each role message the outbox holds for role of LEGION-208, oldest first.
func (r *fixRig) told(role claim.Role) []string {
	r.t.Helper()
	return outboxStrings(r.t, r.pool, fmt.Sprintf(`select payload->>'body' from outbox where kind = 'role_message' and issue = $1
		and payload->>'role' = '%s' order by id`, roleToken(r.t, role)), "LEGION-208")
}

// assertStrings fails unless got is want, what naming the list.
func assertStrings(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s = %q, want %q", what, got, want)
	}
}

// oneJobRead is the read of fix 1's failed job for check at fixHead, of task.
func oneJobRead(task, check string) []record.FixJobRead {
	return []record.FixJobRead{{Generation: 1, Task: task, Fix: 1, Check: check, Head: fixHead}}
}

// assertToldOnceNaming fails unless messages is one message that names each of facts, what naming
// the messages.
func assertToldOnceNaming(t *testing.T, what string, messages []string, facts ...string) {
	t.Helper()
	if len(messages) != 1 {
		t.Fatalf("%s = %q, want one message", what, messages)
	}
	for _, fact := range facts {
		if !strings.Contains(messages[0], fact) {
			t.Fatalf("%s = %q, want it to name %q", what, messages[0], fact)
		}
	}
}

// A check that a CI job runs failing at the head while the review runs starts the task's first
// implementation step again as a fix: the fix reads its failed job first (state reading, at the
// implementer's place), the failure names it, and one read of the job is queued. Nothing is told
// to the implementer yet: the fix's task tells it. Its red is the rule taken out of the tail of
// every fact (applyAndFix): the same failure then starts nothing.
func TestAFailureWhileTheReviewRunsStartsAFix(t *testing.T) {
	r := reviewing(t)
	r.fails(fixHead, "k-ci")
	assertPhase(t, r.pool, phase.Reviewing)
	assertStrings(t, "the fixes", r.fixes(), []string{fixOf(1, "reading", "k-ci")})
	assertStrings(t, "the failures", r.failures(), []string{failureOf("k-ci", "fix", 1)})
	if got := r.jobReads(); !reflect.DeepEqual(got, oneJobRead("a", "k-ci")) {
		t.Fatalf("the job reads = %+v, want %+v", got, oneJobRead("a", "k-ci"))
	}
	assertStrings(t, "the implementer's messages", r.told(claim.RoleImplementer), nil)
}

// A task that takes a fix gets one repair assignment of a failure, whichever of Dispatch's state
// event and the listener's settlement of the head comes first: the failure's row names fix 1, one
// fix reads its job, and once the read answers the fix starts with the failure in its task; no
// assignment row is written beside it. The settlement is reported to each of the reviewer, the
// implementer and the owner once, which assigns nothing, and the implementer gets no role message
// of the failure. Its reds are an assignment derived from the settlement's raw conclusion
// (reportRuns), which gives the implementer an assignment row beside the fix, a second repair; and
// the failed CI job told to a task that takes a fix as to any other (tellCIFailure), which gives
// the implementer that message beside the fix.
func TestAFixesCITaskGetsOneRepairAssignmentInBothOrders(t *testing.T) {
	settle := func(r *fixRig) { applyRefusingNothing(r.t, r.pool, r.engine, settledAt(fixHead, 1, "unit")) }
	fail := func(r *fixRig) { r.fails(fixHead, "k-ci") }
	for _, order := range []struct {
		name          string
		first, second func(*fixRig)
	}{
		{"state first", fail, settle},
		{"settlement first", settle, fail},
	} {
		t.Run(order.name, func(t *testing.T) {
			r := reviewing(t)
			order.first(r)
			order.second(r)
			assertStrings(t, "the failures", r.failures(), []string{failureOf("k-ci", "fix", 1)})
			if got := r.jobReads(); !reflect.DeepEqual(got, oneJobRead("a", "k-ci")) {
				t.Fatalf("the job reads = %+v, want %+v", got, oneJobRead("a", "k-ci"))
			}
			r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})
			assertStrings(t, "the fixes", r.fixes(), []string{fixOf(1, "running", "k-ci")})
			if starts := r.fixStarts(); len(starts) != 1 || starts[0].Fix != 1 || starts[0].FixFrom != fixHead {
				t.Fatalf("the fix's starts = %+v, want one of fix 1 from %s", starts, fixHead)
			}
			assertStrings(t, "the assignments", r.assignments(), nil)
			assertStrings(t, "the reports' receivers", r.receivers(record.CINewsRuns), []string{"architect", "implementer", "reviewer"})
			assertStrings(t, "the implementer's messages", r.told(claim.RoleImplementer), nil)
		})
	}
}

// An issue with no plan its owner gave walks the default steps this release writes at its first
// step, and those take a fix: a failure while its review runs starts one, at the implementer's
// place.
func TestAnIssueOnTheDefaultStepsGetsAFix(t *testing.T) {
	r := newFixRig(t, engineRunningByDefault(t, "implementer", "reviewer"), nil, TriggerImplementationReady)
	assertPhase(t, r.pool, phase.Reviewing)
	r.fails(fixHead, "k-ci")
	assertStrings(t, "the fixes", r.fixes(), []string{fixOf(1, "reading", "k-ci")})
	if got := r.jobReads(); !reflect.DeepEqual(got, oneJobRead("default", "k-ci")) {
		t.Fatalf("the job reads = %+v, want %+v", got, oneJobRead("default", "k-ci"))
	}
}

// A failure while the task's implementation step runs goes to that step: the failure is the
// implementer's, and the implementer gets one repair assignment for that step, naming the check,
// the head and how its job ended, and no role message of it; no fix starts. The same failure in a
// later event assigns nothing more, since its row says the implementer was given it.
func TestAFailureWhileTheImplementerRunsGoesToIt(t *testing.T) {
	r := newFixRig(t, returnEngine(), []string{"implementer", "tester", "reviewer"})
	assertPhase(t, r.pool, phase.Implementing)
	r.fails(fixHead, "k-ci")
	assertStrings(t, "the fixes", r.fixes(), nil)
	assertStrings(t, "the failures", r.failures(), []string{failureOf("k-ci", "implementer", 0)})
	assigned := []string{"implementer " + r.stepReader(claim.RoleImplementer, phase.Implementing) + " k-ci@" + fixHead + " failure fix 0"}
	assertStrings(t, "the assignments", r.assignments(), assigned)
	assertStrings(t, "the implementer's messages", r.told(claim.RoleImplementer), nil)

	r.fails(fixHead, "k-ci")
	assertStrings(t, "the assignments after the same failure again", r.assignments(), assigned)
}

// A state at a head the pull request has left starts nothing and tells nobody: Dispatch keeps
// publishing states at a former head, and the fix is of the task's head alone. The control: the
// same failure at the head starts a fix.
func TestAFailureAtAFormerHeadStartsNothing(t *testing.T) {
	r := reviewing(t)
	r.fails(formerHead, "k-ci")
	assertStrings(t, "the fixes after a failure at a former head", r.fixes(), nil)
	assertStrings(t, "the failures after a failure at a former head", r.failures(), nil)
	assertStrings(t, "the implementer's messages", r.told(claim.RoleImplementer), nil)

	r.fails(fixHead, "k-ci")
	assertStrings(t, "the fixes after the failure at the head", r.fixes(), []string{fixOf(1, "reading", "k-ci")})
}

// A second check failing at the head while the fix reads its job joins that fix, never a second
// one: its failure names fix 1, one job is read, and the implementer is told nothing, since the
// fix's task names each failure it holds. Two failures the daemon holds at once start one fix
// holding both: their states are written to the store before any fact's rule reads them, and the
// next fact's rule finds both.
func TestASecondFailureAtTheHeadJoinsTheFixThatIsStarting(t *testing.T) {
	t.Run("in a later event", func(t *testing.T) {
		r := reviewing(t)
		r.fails(fixHead, "k-ci")
		r.fails(fixHead, "k-ci2")
		assertStrings(t, "the fixes", r.fixes(), []string{fixOf(1, "reading", "k-ci")})
		assertStrings(t, "the failures", r.failures(), []string{failureOf("k-ci", "fix", 1), failureOf("k-ci2", "fix", 1)})
		if got := r.jobReads(); !reflect.DeepEqual(got, oneJobRead("a", "k-ci")) {
			t.Fatalf("the job reads = %+v, want %+v", got, oneJobRead("a", "k-ci"))
		}
		assertStrings(t, "the implementer's messages", r.told(claim.RoleImplementer), nil)
	})

	t.Run("in one pass", func(t *testing.T) {
		r := reviewing(t)
		seedRecord(t, r.pool, func(tx pgx.Tx) error {
			store := record.NewStore()
			if _, err := store.PutCheckSet(t.Context(), tx, "LEGION-208", statesRepo, fixHead, fixChecks, 1); err != nil {
				return err
			}
			for _, check := range []string{"k-ci", "k-ci2"} {
				if _, err := store.PutCheckState(t.Context(), tx, record.CheckState{Issue: "LEGION-208", Repo: statesRepo, Head: fixHead, Check: check,
					State: "no", Reason: "fail", CICase: "failure", Roles: []string{"ci"}, Seq: 1}); err != nil {
					return err
				}
			}
			return nil
		})
		assertStrings(t, "the fixes before any rule read the two failures", r.fixes(), nil)
		r.fails(fixHead, "k-ci2")
		assertStrings(t, "the fixes", r.fixes(), []string{fixOf(1, "reading", "k-ci")})
		assertStrings(t, "the failures", r.failures(), []string{failureOf("k-ci", "fix", 1), failureOf("k-ci2", "fix", 1)})
	})
}

// runningAtTheEnd is LEGION-208's task held for its fix at its end: k-ci failed at fixHead while
// the review ran, the review approved, and fix 1 running.
func runningAtTheEnd(t *testing.T) *fixRig {
	t.Helper()
	r := reviewing(t)
	r.fails(fixHead, "k-ci")
	move(t, r.pool, r.engine, "LEGION-208", TriggerReviewApproved, "", record.PhaseRow{})
	held := issueOf(t, r.pool, r.engine, "LEGION-208")
	held.Phase, held.Hold = phase.Held, &record.Hold{From: phase.Retro, Reason: record.HoldForFix}
	seedIssue(t, r.pool, held)
	onStored(t, r.pool, r.engine, "LEGION-208", func(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
		fix, err := r.engine.store.RunningFix(ctx, tx, issue.Key, issue.Generation, "a")
		if err != nil || fix == nil {
			return fmt.Errorf("the reading fix = %+v, %v", fix, err)
		}
		at := stepClock
		fix.State, fix.Entry, fix.StartedAt = record.FixRunning, 2, &at
		return r.engine.store.UpdateFix(ctx, tx, *fix)
	})
	return r
}

// A check failing at the head of a task held for its fix at its end joins the fix that runs:
// one fix holds both failures, the implementer gets one repair assignment of the new one for the
// fix it runs, and the issue read names the one fix that runs. Its red is the join taken out of the
// fix rule (fixFailure): the end's branch then starts a second fix, which the store refuses
// (record.ErrFixOpen), and the fact fails.
func TestASecondFailureAtTheEndJoinsTheRunningFix(t *testing.T) {
	r := runningAtTheEnd(t)

	r.fails(fixHead, "k-ci2")

	assertStrings(t, "the fixes", r.fixes(), []string{fixOf(1, "running", "k-ci")})
	assertStrings(t, "the failures", r.failures(), []string{failureOf("k-ci", "fix", 1), failureOf("k-ci2", "fix", 1)})
	assertStrings(t, "the assignments", r.assignments(), []string{"implementer fix-1@2 k-ci2@" + fixHead + " failure fix 1"})
	assertStrings(t, "the implementer's messages", r.told(claim.RoleImplementer), nil)
	if issue := issueOf(t, r.pool, r.engine, "LEGION-208"); issue.RunningFix != 1 {
		t.Fatalf("the issue read names running fix %d, want 1", issue.RunningFix)
	}
}

// A task with no implementation step takes no fix: held for CI at its end, its check failing
// there in two events, its owner is told once, by one ci-failed notice naming the check and the
// head, and the failure is the owner's.
func TestATaskWithNoImplementationStepTellsItsOwnerOnce(t *testing.T) {
	r := newFixRig(t, returnEngine(), []string{"tester", "reviewer"}, TriggerTesterPassed)
	r.ciRunsAtTheEnd()
	r.approves()

	r.fails(fixHead, "k-ci")
	r.fails(fixHead, "k-ci")

	assertStrings(t, "the fixes", r.fixes(), nil)
	assertStrings(t, "the failures", r.failures(), []string{failureOf("k-ci", "owner", 0)})
	r.assertToldOwnerOnce("k-ci")
}

// keptByTheImplementer is LEGION-208 on `implementer tester`, its implementer told of k-ci
// failing at fixHead (a row of the implementer's, written by hand where the task takes no fix),
// which completes at fixHead with no commit, and its tester's pass: the task reaches its end
// with k-ci failing there.
func keptByTheImplementer(t *testing.T, fixesCI bool) *fixRig {
	t.Helper()
	r := newFixRig(t, returnEngine(), []string{"implementer", "tester"})
	if !fixesCI {
		r.releaseBefore()
	}
	r.at(fixHead, noCIJob("k-unit"), ciPassing("k-ci2"))
	r.fails(fixHead, "k-ci")
	if !fixesCI {
		onStored(t, r.pool, r.engine, "LEGION-208", func(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
			_, err := r.engine.store.PutCIFailure(ctx, tx, record.CIFailure{Issue: issue.Key, Generation: issue.Generation, Task: "a",
				Check: "k-ci", Head: fixHead, WentTo: record.CIFailureToImplementer, At: stepClock})
			return err
		})
	}
	assertStrings(t, "the failures before the end", r.failures(), []string{failureOf("k-ci", "implementer", 0)})
	r.moveFact(TriggerImplementationReady, "", record.PhaseRow{})
	r.moveFact(TriggerTesterPassed, "", record.PhaseRow{})
	return r
}

// A failure the implementer was told of and kept is acted on at the end: the task, which takes a
// fix, reaches its end with the check failing at the head, and in the same fact a fix starts for
// it and the failure's row takes its number, the hold for CI becoming the hold for the fix.
func TestAFailureTheImplementerKeptStartsAFixAtTheEnd(t *testing.T) {
	r := keptByTheImplementer(t, true)

	assertStrings(t, "the fixes", r.fixes(), []string{fixOf(1, "reading", "k-ci")})
	assertStrings(t, "the failures", r.failures(), []string{failureOf("k-ci", "fix", 1)})
	r.assertEndHold(record.HoldForFix)
	if told := r.ciFailed(); len(told) != 0 {
		t.Fatalf("the ci-failed notices = %+v, want none: a fix took the failure", told)
	}
}

// The twin task, which takes no fix, with the same row of the implementer's: at its end no fix
// starts, and its owner is told once.
func TestAFailureTheImplementerKeptTellsTheOwnerWithoutTheFlag(t *testing.T) {
	r := keptByTheImplementer(t, false)

	assertStrings(t, "the fixes", r.fixes(), nil)
	r.assertEndHold(record.HoldForCI)
	r.assertToldOwnerOnce("k-ci")
}

// A failure while the planner runs, before the task's first implementation step, starts no fix
// and writes no failure: the implementation step's start names it.
func TestAFailureWhileThePlannerRunsStartsNoFix(t *testing.T) {
	r := newFixRig(t, returnEngine(), []string{"planner", "implementer", "reviewer"})
	assertPhase(t, r.pool, phase.Planning)
	r.fails(fixHead, "k-ci")
	assertStrings(t, "the fixes", r.fixes(), nil)
	assertStrings(t, "the failures", r.failures(), nil)
	assertStrings(t, "the implementer's messages", r.told(claim.RoleImplementer), nil)
}

// A failure while the task stands at its retro goes to the merger, which reads GitHub: the
// failure names the merger, and nothing else happens. Each CI check passed at the head before the
// review's approval, so the task entered its retro; k-ci fails there afterwards.
func TestAFailureInTheRetroGoesToTheMerger(t *testing.T) {
	r := newFixRig(t, returnEngine(), []string{"implementer", "reviewer"}, TriggerImplementationReady)
	r.at(fixHead, noCIJob("k-unit"), ciPassing("k-ci2"), ciPassing("k-ci"))
	r.move(TriggerReviewApproved, "", record.PhaseRow{})
	assertPhase(t, r.pool, phase.Retro)
	r.fails(fixHead, "k-ci")
	assertStrings(t, "the fixes", r.fixes(), nil)
	assertStrings(t, "the failures", r.failures(), []string{failureOf("k-ci", "merger", 0)})
	assertStrings(t, "the implementer's messages", r.told(claim.RoleImplementer), nil)
	assertStrings(t, "the merger's messages", r.told(claim.RoleMerger), nil)
}

// A task of a run that started before this release takes no fix: a failure while its review runs
// writes no failure and starts no fix, and the listener's settlement tells the owner, the
// reviewer and the implementer, as before. The implementer also hears of the failed CI job from
// tellCIFailure, which tells a task that takes no fix.
func TestATaskOfTheReleaseBeforeStartsNoFix(t *testing.T) {
	r := reviewing(t)
	if _, err := r.pool.Exec(t.Context(), `update plan_tasks set fixes_ci = false where issue = 'LEGION-208'`); err != nil {
		t.Fatalf("write the task as the upgrade found it: %v", err)
	}
	r.fails(fixHead, "k-ci")
	applyRefusingNothing(t, r.pool, r.engine, settledAt(fixHead, 1, "unit"))
	assertStrings(t, "the fixes", r.fixes(), nil)
	assertStrings(t, "the failures", r.failures(), nil)
	settled := settledBody
	for _, role := range []claim.Role{claim.RoleArchitect, claim.RoleReviewer} {
		assertStrings(t, "the "+string(role)+"'s messages", r.told(role), []string{settled})
	}
	ciFailed := "Pull request #42: check k-ci fails at " + fixHead + ", where its CI job ended failure; its state there is no (fail). " +
		"Nothing moved on it: read the job's log and decide whose failure it is."
	assertStrings(t, "the implementer's messages", r.told(claim.RoleImplementer), []string{ciFailed, settled})
}

// The fix rule runs at the tail of every fact of an issue, a review among them, which names the
// issue only through its pull request. The reviewer's approval of X moves a task that takes no fix,
// whose k-ci fails at X, into its retro, so the task is held there for its CI, and in the same
// transaction the rule tells the owner once, by one ci-failed notice naming the check and X. The
// control is the state before the review: the failure alone, at a task in reviewing that takes no
// fix, tells the owner nothing and starts no fix.
func TestAReviewThatHoldsATaskForItsCITellsTheOwnerAtOnce(t *testing.T) {
	r := reviewing(t)
	seedClaimSession(t, r.pool, reviewClaim, "ses-reviewer")
	r.releaseBefore()
	r.fails(fixHead, "k-ci")
	assertStrings(t, "the fixes", r.fixes(), nil)
	if told := r.ciFailed(); len(told) != 0 {
		t.Fatalf("the ci-failed notices before the review = %+v, want none", told)
	}

	r.reviewOf(1, "approved", fixHead)

	r.assertEndHold(record.HoldForCI)
	assertStrings(t, "the fixes after the review", r.fixes(), nil)
	r.assertToldOwnerOnce("k-ci")
}
