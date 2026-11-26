package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// besideFix1 is role's branch beside fix 1 of LEGION-208's task a at generation 1.
func besideFix1(t *testing.T, role claim.Role) string {
	t.Helper()
	branch, err := claim.BesideBranch("LEGION-208", 1, "a", 1, role)
	if err != nil {
		t.Fatalf("name the %s's branch beside fix 1: %v", role, err)
	}
	return branch
}

// boundBeside binds the open start of step place of LEGION-208's task a to role's branch beside fix
// 1 (record.Store.SetStepPushBranch), and answers that branch.
func (r *fixRig) boundBeside(place int, role claim.Role) string {
	r.t.Helper()
	branch := besideFix1(r.t, role)
	onStored(r.t, r.pool, r.engine, "LEGION-208", func(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
		at := record.StepPlace{Task: "a", Kind: record.PlaceStep, Place: place}
		start, err := r.engine.store.StepStarts(ctx, tx, issue.Key, at)
		if err != nil {
			return err
		}
		return r.engine.store.SetStepPushBranch(ctx, tx, issue.Key, at, start.Starts, branch)
	})
	return branch
}

// updatePushes is where each role of LEGION-208 pushes at an update round's ask now (UpdatePushes).
func (r *fixRig) updatePushes() map[claim.Role]supervise.UpdatePush {
	r.t.Helper()
	var pushes map[claim.Role]supervise.UpdatePush
	applyInTx(r.t, r.pool, r.engine, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		pushes, err = r.engine.UpdatePushes(ctx, tx, "LEGION-208")
		return err
	})
	return pushes
}

// assertPushes fails unless the update round's ask of LEGION-208 names want, what naming the moment.
func (r *fixRig) assertPushes(what string, want map[claim.Role]supervise.UpdatePush) {
	r.t.Helper()
	if got := r.updatePushes(); !reflect.DeepEqual(got, want) {
		r.t.Fatalf("the update round's pushes %s = %+v, want %+v", what, got, want)
	}
}

// An update round's ask names each step whose open start is bound to a branch beside a fix by the
// role of the step's phase: the tester's in testing, and the owner's acceptance under the
// architect, the role RoleFor answers accepting for. The implementer pushes nothing while the fix
// reads its job, and to its own branch while the fix runs. Before a step is bound, while the fix
// reads its job, that step gets the zero push, main's ask.
func TestAnUpdateRoundNamesEachBoundStepByTheRoleOfItsPhase(t *testing.T) {
	own := besideFix1(t, claim.RoleImplementer)
	t.Run("the tester in testing", func(t *testing.T) {
		r := newFixRig(t, returnEngine(), []string{"implementer", "tester", "reviewer"}, TriggerImplementationReady)
		r.fails(fixHead, "k-ci")
		r.assertPushes("while the fix reads its job", map[claim.Role]supervise.UpdatePush{claim.RoleImplementer: {FixReads: true}})
		r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})
		tester := r.boundBeside(2, claim.RoleTester)
		r.assertPushes("while the fix runs", map[claim.Role]supervise.UpdatePush{
			claim.RoleTester:      {Branch: tester},
			claim.RoleImplementer: {Branch: own, OwnFix: true},
		})
	})

	t.Run("the owner in accepting", func(t *testing.T) {
		r := newFixRig(t, returnEngine(), []string{"implementer", "reviewer", "acceptance:owner"}, TriggerImplementationReady)
		r.approves()
		r.fails(fixHead, "k-ci")
		r.assertPushes("while the fix reads its job", map[claim.Role]supervise.UpdatePush{claim.RoleImplementer: {FixReads: true}})
		r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})
		owner := r.boundBeside(3, claim.RoleArchitect)
		r.assertPushes("while the fix runs", map[claim.Role]supervise.UpdatePush{
			claim.RoleArchitect:   {Branch: owner},
			claim.RoleImplementer: {Branch: own, OwnFix: true},
		})
	})
}

// beside is the branch of each fix-started row of LEGION-208 by the role it is to, "" for a row
// that names none.
func (r *fixRig) beside() map[claim.Role]string {
	r.t.Helper()
	branches := map[claim.Role]string{}
	for _, n := range r.newsOf() {
		if n.Purpose == record.CINewsFixStarted {
			branches[roleOf(r.t, n.Role)] = n.Branch
		}
	}
	return branches
}

// A step beside a fix is told to push to its own branch wherever it is told anything. Fix 1 starts
// while the tester runs: the tester's fix-started row names its own branch beside fix 1, which is
// no branch of the issue (claim.IsBesideBranch); a tester that starts during the fix holds fix 1
// and that branch in its start facts; and the update round's ask names that branch for the tester,
// F with OwnFix for the implementer and no branch for the reviewer. While the fix reads its job, the
// ask names FixReads and no branch for the implementer. The owner in accepting gets its own branch,
// under the architect, in both its notice and the ask. The control: an issue with no fix finds no
// branch beside a fix in any row, start or answer.
func TestAStepBesideAFixIsToldToPushToItsOwnBranch(t *testing.T) {
	own := besideFix1(t, claim.RoleImplementer)
	t.Run("a tester that runs beside fix 1", func(t *testing.T) {
		r := newFixRig(t, returnEngine(), []string{"implementer", "tester", "reviewer"}, TriggerImplementationReady)
		r.fails(fixHead, "k-ci")
		r.assertPushes("while the fix reads its job", map[claim.Role]supervise.UpdatePush{claim.RoleImplementer: {FixReads: true}})
		r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})
		tester := besideFix1(t, claim.RoleTester)
		if told := r.beside(); !claim.IsBesideBranch(told[claim.RoleTester]) || !reflect.DeepEqual(told, map[claim.Role]string{claim.RoleTester: tester}) {
			t.Fatalf("the fix-started rows' branches = %v, want the tester's own branch %s alone", told, tester)
		}
		if facts := r.startFactsIn(phase.Testing); facts.Fix != 1 || facts.Branch != tester {
			t.Fatalf("a tester's start facts during the fix = %+v, want fix 1 on %s", facts, tester)
		}
		r.assertPushes("while the fix runs", map[claim.Role]supervise.UpdatePush{
			claim.RoleTester:      {Branch: tester},
			claim.RoleImplementer: {Branch: own, OwnFix: true},
		})
	})

	t.Run("the owner in accepting", func(t *testing.T) {
		r := newFixRig(t, returnEngine(), []string{"implementer", "reviewer", "acceptance:owner"}, TriggerImplementationReady)
		r.approves()
		r.fails(fixHead, "k-ci")
		r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})
		owner := besideFix1(t, claim.RoleArchitect)
		if told := r.beside(); told[claim.RoleArchitect] != owner || told[claim.RoleReviewer] != "" {
			t.Fatalf("the fix-started rows' branches = %v, want the owner's own branch %s and none for the reviewer", told, owner)
		}
		r.assertPushes("while the fix runs", map[claim.Role]supervise.UpdatePush{
			claim.RoleArchitect:   {Branch: owner},
			claim.RoleImplementer: {Branch: own, OwnFix: true},
		})
	})

	t.Run("the control: no fix", func(t *testing.T) {
		r := newFixRig(t, returnEngine(), []string{"implementer", "tester", "reviewer"}, TriggerImplementationReady)
		r.assertPushes("with no fix", map[claim.Role]supervise.UpdatePush{})
		if told := r.beside(); len(told) != 0 {
			t.Fatalf("the fix-started rows' branches with no fix = %v, want none", told)
		}
		for _, at := range []phase.Phase{phase.Testing, phase.Reviewing} {
			if facts := r.startFactsIn(at); facts.Branch != "" {
				t.Fatalf("the start facts in %s with no fix = %+v, want no branch", at, facts)
			}
		}
	})
}

// deletions is each deletion of branches beside a fix queued for LEGION-208, oldest first, as
// `<generation> <task> fix <n>` (record.BesideBranchesDelete, fix 0 for every fix of the task).
func (r *fixRig) deletions() []string {
	r.t.Helper()
	var lines []string
	for _, row := range issueOutboxOf(r.t, r.pool, "LEGION-208", string(record.OutboxKindBesideBranchesDelete)) {
		var deletion record.BesideBranchesDelete
		if err := json.Unmarshal(row.payload, &deletion); err != nil {
			r.t.Fatalf("decode the deletion %s: %v", row.payload, err)
		}
		lines = append(lines, fmt.Sprintf("%d %s fix %d", deletion.Generation, deletion.Task, deletion.Fix))
	}
	return lines
}

// assertWayTaken fails unless the latest fix of LEGION-208's task took way.
func (r *fixRig) assertWayTaken(way int) {
	r.t.Helper()
	if fix := r.latestFix(); fix.Way != way {
		r.t.Fatalf("the fix took way %d, want %d: %+v", fix.Way, way, fix)
	}
}

// failedClaimWithReport is LEGION-208 whose review requested changes beside fix 1, the fix's step
// getting its report, and whose fix's claim then failed, ending the fix with no commit: way 2.
func failedClaimWithReport(t *testing.T, engine *Engine) *fixRig {
	t.Helper()
	r := newFixRig(t, engine, []string{"implementer", "tester", "reviewer"}, TriggerImplementationReady, TriggerTesterPassed)
	r.fails(fixHead, "k-ci")
	r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})
	r.reviewedX()
	r.move(TriggerReviewRejected, "", reviewReport("the widget renders twice"))
	r.apply("fix-claim-failed", intake.ClaimFailed{Issue: "LEGION-208", Role: claim.RoleImplementer, Reason: "the process died"})
	r.assertWayTaken(wayAfterFix)
	return r
}

// allCIPasses applies, at head, the states of LEGION-208's checks with each one a CI job runs
// passed there.
func (r *fixRig) allCIPasses(head string) {
	r.t.Helper()
	r.at(head, noCIJob("k-unit"), ciPassing("k-ci2"), ciPassing("k-ci"))
}

// parks is a person's move of LEGION-208 to backlog, which takes it out of the workflow (leave).
func (r *fixRig) parks() {
	r.t.Helper()
	parked := personsTodo("LEGION-208")
	parked.Status = "backlog"
	if result := r.apply("parked", parked); result.Refusal != nil {
		r.t.Fatalf("the person's move to backlog was refused: %+v", result.Refusal)
	}
}

// The branches beside a fix go once no step can need them. A way 2 after the fix's own completion
// with commits of its own deletes that fix's branches; a way 2 with no commit (a step's report the
// fix got), a way 3 and a way 1 delete none, and neither does a fix whose claim failed, at a way 2
// with a report or a way 3 without one, whose worker-died notice reaches the owner. A stop deletes
// none, and tells the owner by a task-stopped notice of the fix with no StoppedBy. Where no step of
// the task can run again, every branch of the task's fixes goes, once: the release of an end hold
// into the retro, a move into the retro that nothing holds, a leave, a new generation, and a version
// that removes the stopped task. A task with no fix deletes nothing at any end.
//
// The controls, kept: with noEndBesideDelete the release into the retro deletes nothing; with
// besideDeleteAsBefore the failed claim's way 2 and the stop each delete the fix's branches; and
// with deleteAfterEveryCompletion the no-commit way 2 and the way 3 each do.
func TestBesideBranchesAreDeletedAtTheirEnds(t *testing.T) {
	fix1 := []string{"1 a fix 1"}
	task := []string{"1 a fix 0"}
	t.Run("a new-commit way 2 deletes the fix's branches", func(t *testing.T) {
		r := fixAtTheEnd(t, returnEngine())
		r.fixEnds(headTwo)
		r.assertWayTaken(wayAfterFix)
		assertStrings(t, "the deletions", r.deletions(), fix1)
	})
	t.Run("a no-commit way 2 with a report deletes none", func(t *testing.T) {
		r := fixAfterReview(t, returnEngine(), TriggerReviewRejected, false, fixHead)
		r.assertWayTaken(wayAfterFix)
		assertStrings(t, "the deletions", r.deletions(), nil)
	})
	t.Run("way 3 deletes none", func(t *testing.T) {
		r := fixAtTheEnd(t, returnEngine())
		r.fixEnds(fixHead)
		r.assertWayTaken(wayOn)
		assertStrings(t, "the deletions", r.deletions(), nil)
	})
	t.Run("way 1 deletes none", func(t *testing.T) {
		r := planningFix(t)
		r.apply("fix-back", fixMoveBack(phase.Planning))
		r.move(TriggerReviewApproved, "", reviewReport("approved"))
		r.assertWayTaken(wayReturn)
		assertStrings(t, "the deletions", r.deletions(), nil)
	})
	t.Run("a failed claim at a way 2 with a report deletes none", func(t *testing.T) {
		r := failedClaimWithReport(t, returnEngine())
		assertStrings(t, "the deletions", r.deletions(), nil)
		if died := r.workerDied(); len(died) != 1 || !strings.HasPrefix(died[0], string(claim.RoleImplementer)+": ") {
			t.Fatalf("the worker-died notices = %q, want the fix's one", died)
		}
	})
	t.Run("a failed claim at a way 3 deletes none", func(t *testing.T) {
		r := startedFix(t, intake.FixJobRead{Name: "unit", URL: fixPage})
		r.apply("fix-claim-failed", intake.ClaimFailed{Issue: "LEGION-208", Role: claim.RoleImplementer, Reason: "the process died"})
		r.reviewedX()
		r.move(TriggerReviewApproved, "", reviewReport("approved"))
		r.assertWayTaken(wayOn)
		assertStrings(t, "the deletions", r.deletions(), nil)
		if died := r.workerDied(); len(died) != 1 {
			t.Fatalf("the worker-died notices = %q, want the fix's one", died)
		}
	})
	t.Run("a stop deletes none and tells the owner", func(t *testing.T) {
		r := startedFix(t, intake.FixJobRead{Name: "unit", URL: fixPage})
		r.stop()
		assertStrings(t, "the deletions", r.deletions(), nil)
		var unsigned []record.Notice
		for _, notice := range noticesOf(t, r.pool, "LEGION-208", "task-stopped") {
			if notice.StoppedBy == "" {
				unsigned = append(unsigned, notice)
			}
		}
		if len(unsigned) != 1 || unsigned[0].Role != claim.RoleImplementer {
			t.Fatalf("the task-stopped notices with no StoppedBy = %+v, want the fix's one", unsigned)
		}
	})
	t.Run("the release of an end hold into the retro deletes the task's branches", func(t *testing.T) {
		r := fixAtTheEnd(t, returnEngine())
		r.allCIPasses(fixHead)
		r.fixEnds(fixHead)
		r.assertRetroStarted()
		assertStrings(t, "the deletions", r.deletions(), task)
	})
	t.Run("a move into the retro that nothing holds deletes the task's branches", func(t *testing.T) {
		r := fixAtTheEnd(t, returnEngine())
		r.fixEnds(headTwo)
		r.move(TriggerTesterPassed, "", record.PhaseRow{})
		r.allCIPasses(headTwo)
		r.move(TriggerReviewApproved, "", reviewReport("approved"))
		r.assertRetroStarted()
		assertStrings(t, "the deletions", r.deletions(), append(append([]string{}, fix1...), task...))
	})
	t.Run("a leave deletes the task's branches", func(t *testing.T) {
		r := startedFix(t, intake.FixJobRead{Name: "unit", URL: fixPage})
		r.parks()
		assertStrings(t, "the deletions", r.deletions(), task)
	})
	t.Run("a new generation deletes the old generation's branches once", func(t *testing.T) {
		r := startedFix(t, intake.FixJobRead{Name: "unit", URL: fixPage})
		onStored(t, r.pool, r.engine, "LEGION-208", func(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
			issue.Generation++
			issue.Phase, issue.Hold = phase.Admitted, nil
			issue.Status, issue.DispatchStatus = "todo", "todo"
			if err := r.engine.store.PutIssue(ctx, tx, issue); err != nil {
				return err
			}
			return r.engine.store.ClearTreeGeneration(ctx, tx, issue.Tree, stepClock.Add(time.Hour))
		})
		assertStrings(t, "the deletions", r.deletions(), task)
	})
	t.Run("a version that removes the stopped task deletes its branches", func(t *testing.T) {
		r := startedFix(t, intake.FixJobRead{Name: "unit", URL: fixPage})
		r.stop()
		other := givenTask(t, "Build the other widget.", "implementer", "reviewer")
		other.ID = "b"
		mustTake(t, "the version without task a", r.apply("version-3", ownersPlan(t, "LEGION-208", 1, 3, other)))
		assertStrings(t, "the deletions", r.deletions(), task)
	})
	t.Run("a task with no fix deletes nothing at any end", func(t *testing.T) {
		r := reviewing(t)
		r.allCIPasses(fixHead)
		r.move(TriggerReviewApproved, "", reviewReport("approved"))
		r.assertRetroStarted()
		r.parks()
		assertStrings(t, "the deletions", r.deletions(), nil)
	})

	t.Run("the control: noEndBesideDelete deletes nothing at the release into the retro", func(t *testing.T) {
		engine := returnEngine()
		engine.noEndBesideDelete = true
		r := fixAtTheEnd(t, engine)
		r.allCIPasses(fixHead)
		r.fixEnds(fixHead)
		r.assertRetroStarted()
		assertStrings(t, "the deletions", r.deletions(), nil)
	})
	t.Run("the control: besideDeleteAsBefore deletes at a failed claim and at a stop", func(t *testing.T) {
		engine := returnEngine()
		engine.besideDeleteAsBefore = true
		assertStrings(t, "the deletions of the failed claim", failedClaimWithReport(t, engine).deletions(), fix1)
		engine = returnEngine()
		engine.besideDeleteAsBefore = true
		r := newFixRig(t, engine, []string{"implementer", "tester", "reviewer"}, TriggerImplementationReady, TriggerTesterPassed)
		r.fails(fixHead, "k-ci")
		r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})
		r.stop()
		assertStrings(t, "the deletions of the stop", r.deletions(), fix1)
	})
	t.Run("the control: deleteAfterEveryCompletion deletes at a no-commit way 2 and a way 3", func(t *testing.T) {
		engine := returnEngine()
		engine.deleteAfterEveryCompletion = true
		assertStrings(t, "the deletions of the no-commit way 2", fixAfterReview(t, engine, TriggerReviewRejected, false, fixHead).deletions(), fix1)
		engine = returnEngine()
		engine.deleteAfterEveryCompletion = true
		r := fixAtTheEnd(t, engine)
		r.fixEnds(fixHead)
		assertStrings(t, "the deletions of the way 3", r.deletions(), fix1)
	})
}

// testerWaitReasons is the reason of each phase-finished notice of LEGION-208's tester whose testing
// move waits for the head's set of checks, oldest first.
func (r *fixRig) testerWaitReasons() []string {
	r.t.Helper()
	var reasons []string
	for _, notice := range noticesOf(r.t, r.pool, "LEGION-208", "phase-finished") {
		if notice.Role == claim.RoleTester && notice.WaitsFor == waitsForSet {
			reasons = append(reasons, notice.Reason)
		}
	}
	return reasons
}

// A tester that completes at Z, a commit of its own branch beside a fix that no pull request heads,
// while the pull request heads X, holds its testing move for good, since Dispatch computes no state
// at Z: the issue stays in testing, and the owner's one phase-finished notice gives the reason that
// says so (offHeadWaitReason of Z and X). The control, kept: a completion at X, the pull request's
// head, whose set Dispatch has not published yet, keeps the plain wait for its states, and the two
// reasons differ, so a reason that names every wait alike fails here.
func TestATestingMoveAtAHeadOfNoPullRequestSaysWhy(t *testing.T) {
	const besideHead = "3333333333333333333333333333333333333333"
	t.Run("a completion at a head of no pull request", func(t *testing.T) {
		r := newFixRig(t, returnEngine(), []string{"implementer", "tester", "reviewer"}, TriggerImplementationReady)
		r.testerCompletesAt(besideHead, "test-1")
		assertPhase(t, r.pool, phase.Testing)
		assertStrings(t, "the tester's wait reasons", r.testerWaitReasons(), []string{offHeadWaitReason(besideHead, fixHead)})
	})
	t.Run("the control: a completion at the pull request's head", func(t *testing.T) {
		r := newFixRig(t, returnEngine(), []string{"implementer", "tester", "reviewer"}, TriggerImplementationReady)
		r.testerCompletesAt(fixHead, "test-1")
		assertPhase(t, r.pool, phase.Testing)
		reasons := r.testerWaitReasons()
		if len(reasons) != 1 || reasons[0] == offHeadWaitReason(fixHead, fixHead) || reasons[0] == offHeadWaitReason(besideHead, fixHead) {
			t.Fatalf("the tester's wait reasons at the pull request's head = %q, want one, the plain wait for the states, unlike the reason at a head of no pull request", reasons)
		}
	})
}
