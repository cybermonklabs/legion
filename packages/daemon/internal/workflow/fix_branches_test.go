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

// deletions is each deletion of branches beside the fixes of a task queued for LEGION-208, oldest
// first, as `<generation> <task> through <m>`, m being the newest fix the task held when the row
// was queued (record.BesideBranchesDelete).
func (r *fixRig) deletions() []string {
	r.t.Helper()
	var lines []string
	for _, row := range issueOutboxOf(r.t, r.pool, "LEGION-208", string(record.OutboxKindBesideBranchesDelete)) {
		var deletion record.BesideBranchesDelete
		if err := json.Unmarshal(row.payload, &deletion); err != nil {
			r.t.Fatalf("decode the deletion %s: %v", row.payload, err)
		}
		line := fmt.Sprintf("%d %s through %d", deletion.Generation, deletion.Task, deletion.Through)
		lines = append(lines, line)
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

// The branches beside a fix go once no step can need them, never at the way after the fix: a way 2
// with commits of the fix's own, a way 2 with no commit (a step's report the fix got), a way 3 and a
// way 1 delete none, since a step beside the fix may have pushed after the fix's end push, and the
// step a way 2 runs again takes its commits from its kept branch; nor does a fix whose claim failed,
// at a way 2 with a report or a way 3 without one, whose worker-died notice reaches the owner. A
// stop deletes none, and tells the owner by a task-stopped notice of the fix with no StoppedBy.
// Where no step of the task can run again, every branch of the task's fixes goes, once: the
// release of an end hold into the retro, a move into the retro that nothing holds, a leave, a new
// generation, and a version that removes the stopped task. A task with no fix deletes nothing at
// any end.
//
// Its reds: the deletion taken out of queueTaskEnd deletes nothing at the release into the retro;
// the deletion the rule before queued after every way 2 or 3 and at a stop (afterFix, fixStopped)
// deletes at the failed claim's way 2 and at the stop; and the deletion after every way 2 or 3 of
// the fix's own completion (afterFix) deletes at the no-commit way 2 and at the way 3.
func TestBesideBranchesAreDeletedAtTheirEnds(t *testing.T) {
	task := []string{"1 a through 1"}
	t.Run("a new-commit way 2 deletes none", func(t *testing.T) {
		r := fixAtTheEnd(t, returnEngine())
		r.fixEnds(headTwo)
		r.assertWayTaken(wayAfterFix)
		assertStrings(t, "the deletions", r.deletions(), nil)
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
		assertStrings(t, "the deletions", r.deletions(), task)
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
		version := ownersPlan(t, "LEGION-208", 1, 3, other)
		version.Plan.ArtifactID = "plan-artifact" // the document of the version in force (seedStepsPlan)
		mustTake(t, "the version without task a", r.apply("version-3", version))
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
}

// lastOutboxID is the id of the newest outbox row of any issue, 0 for none.
func (r *fixRig) lastOutboxID() int64 {
	r.t.Helper()
	var id int64
	if err := r.pool.QueryRow(r.t.Context(), `select coalesce(max(id), 0) from outbox`).Scan(&id); err != nil {
		r.t.Fatalf("read the newest outbox row: %v", err)
	}
	return id
}

// assertDeletionAfterOldEnds fails unless LEGION-208's outbox holds one deletion of the branches
// beside task a's fixes, written after the end of every start of task a at generation 1 that the
// outbox holds: the row that deletes them follows each old start's end, so no start was still open
// in the transaction when the deletion was queued. It fails too where no such end was written after
// the row since, the newest before the fact: the order of no end proves nothing.
func (r *fixRig) assertDeletionAfterOldEnds(since int64) {
	r.t.Helper()
	deletions := issueOutboxOf(r.t, r.pool, "LEGION-208", string(record.OutboxKindBesideBranchesDelete))
	if len(deletions) != 1 {
		r.t.Fatalf("the deletion rows = %d, want one", len(deletions))
	}
	ended := 0
	for _, row := range issueOutboxOf(r.t, r.pool, "LEGION-208", string(record.OutboxKindStepRecord)) {
		var write record.StepRecord
		if err := json.Unmarshal(row.payload, &write); err != nil {
			r.t.Fatalf("decode the step record %s: %v", row.payload, err)
		}
		if !isEnd(write) || write.Generation != 1 || write.Task != "a" {
			continue
		}
		if row.id > since {
			ended++
		}
		if row.id > deletions[0].id {
			r.t.Fatalf("the end of start %s (row %d) follows the deletion (row %d): the deletion was queued while that start was open", write.ID, row.id, deletions[0].id)
		}
	}
	if ended == 0 {
		r.t.Fatalf("the outbox holds no end of a start of task a at generation 1 after row %d, so the deletion's order proves nothing", since)
	}
}

// A new generation ends its old starts through settleRuns, and its deletion of the branches beside
// the task's fixes waits until that transaction ended every old start. The state is built from
// rows: fix 1 runs beside the tester, whose start is open and bound to its branch beside fix 1, and
// the tester's push of that branch is recorded. The new generation's settle ends the old starts and
// then queues one deletion of every branch through fix 1, after each end, and the push stays
// recorded. Its red is the deletion queued by dropFixWay, before the settle ends the old starts.
func TestANewGenerationQueuesDeletionAfterItsLastOldStartEnds(t *testing.T) {
	r := newFixRig(t, returnEngine(), []string{"implementer", "tester", "reviewer"}, TriggerImplementationReady)
	r.fails(fixHead, "k-ci")
	r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})
	const pushed = "5555555555555555555555555555555555555555"
	r.recordsPush(claim.RoleTester, pushed)
	if got, want := r.besideMarks(2), "1 "+besideFix1(t, claim.RoleTester); got != want {
		t.Fatalf("the tester's start names %q, want %q: open beside fix 1 before the generation ends it", got, want)
	}
	since := r.lastOutboxID()

	onStored(t, r.pool, r.engine, "LEGION-208", func(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
		issue.Generation++
		issue.Phase, issue.Hold = phase.Admitted, nil
		issue.Status, issue.DispatchStatus = "todo", "todo"
		if err := r.engine.store.PutIssue(ctx, tx, issue); err != nil {
			return err
		}
		return r.engine.store.ClearTreeGeneration(ctx, tx, issue.Tree, stepClock.Add(time.Hour))
	})

	assertStrings(t, "the deletion after every old start ended", r.deletions(), []string{"1 a through 1"})
	r.assertDeletionAfterOldEnds(since)
	if fix := r.latestFix(); fix.Pushed[string(claim.RoleTester)] != pushed {
		t.Fatalf("the old tester's push = %+v, want %s", fix.Pushed, pushed)
	}
}

// A leave ends the old starts through the same settle, and its deletion waits for every old start
// of the task in the same way: built from the same rows, the leave ends the old starts and then
// queues one deletion through fix 1, after each end. Its red is the deletion queued by dropFixWay,
// before the settle ends the old starts.
func TestALeaveQueuesDeletionAfterItsLastOldStartEnds(t *testing.T) {
	r := newFixRig(t, returnEngine(), []string{"implementer", "tester", "reviewer"}, TriggerImplementationReady)
	r.fails(fixHead, "k-ci")
	r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})
	const pushed = "6666666666666666666666666666666666666666"
	r.recordsPush(claim.RoleTester, pushed)
	since := r.lastOutboxID()

	r.parks()

	assertStrings(t, "the deletion after every old start ended", r.deletions(), []string{"1 a through 1"})
	r.assertDeletionAfterOldEnds(since)
	if fix := r.latestFix(); fix.Pushed[string(claim.RoleTester)] != pushed {
		t.Fatalf("the leaving tester's push = %+v, want %s", fix.Pushed, pushed)
	}
}

// The deferred deletion a leave skips while a start of the task is still open leaves no branch
// behind, which is what queueDeletesAfterOldRuns says: the one start a leave's settle leaves open
// is the task's merge (runOver), and a task in its merge passed the move into its retro, which
// queued the deletion of every branch of its fixes, and no fix opens after that move (a failure
// there goes to the merger, TestAFailureInTheRetroGoesToTheMerger). On a task whose one step is
// the implementation, a check fails at its end and fix 1 ends with commits; CI passes, the retro
// starts, which queues the deletion through fix 1, and its completion starts the merger; a
// failure at the head while the merge runs opens no fix. Then a person parks the issue: its merge
// can still land in the tree's linger, so the settle leaves that start open and queues no
// deletion, and the one queued at the retro is still the only one.
func TestALeaveWhileTheMergeIsOpenQueuesNoDeletionAndLosesNoBranch(t *testing.T) {
	r := newFixRig(t, returnEngine(), []string{"implementer"})
	r.at(fixHead, jobRunning("k-ci"))
	r.move(TriggerImplementationReady, "", record.PhaseRow{})
	r.fails(fixHead, "k-ci")
	r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})
	r.fixEnds(headTwo)
	r.allCIPasses(headTwo)
	r.assertRetroStarted()
	assertStrings(t, "the deletions at the move into the retro", r.deletions(), []string{"1 a through 1"})

	r.nextStart(TriggerRetroCompleted, claim.RoleMerger, phase.Merging)
	r.fails(headTwo, "k-ci")
	assertStrings(t, "the fixes after the failure in the merge", r.fixes(), []string{"1 ended k-ci@" + fixHead + " place 1"})
	assertStrings(t, "the failures after the failure in the merge", r.failures(),
		[]string{"k-ci@" + fixHead + " fix 1", "k-ci@" + headTwo + " merger 0"})

	r.parks()

	assertStrings(t, "the merge's run after the leave", outboxStrings(t, r.pool,
		`select run from step_starts where issue = $1 and kind = '`+string(record.PlaceMerge)+`'`, "LEGION-208"), []string{"open"})
	assertStrings(t, "the deletions after the leave", r.deletions(), []string{"1 a through 1"})
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

// No fix opens beside a fix whose way is pending. Fix 1 runs beside the running tester, whose start
// is bound to its branch B1, and ends with no commit while the tester still runs, so its way waits
// for the tester's end. A second check, k-ci2, fails at the head meanwhile: no fix 2 opens, and the
// failure has no row yet, so the fix rule reads it again at the next fact. The tester's pass takes
// fix 1's way (3, each step keeps its result), and the fix rule at the tail of that same fact opens
// fix 2 for k-ci2. The tester's start kept B1 to its end, and no deletion of fix 1's branches is
// queued. Its red is openFix's wait taken out: fix 2 then opens while fix 1's way is pending.
func TestNoFixOpensWhileTheWayOfTheLastIsPending(t *testing.T) {
	pending := func(t *testing.T) *fixRig {
		r := newFixRig(t, returnEngine(), []string{"implementer", "tester", "reviewer"}, TriggerImplementationReady)
		r.fails(fixHead, "k-ci")
		r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})
		r.fixEnds(fixHead)
		if fix := r.latestFix(); fix.State != record.FixEnded || fix.Way != 0 {
			t.Fatalf("fix 1 after its end beside the running tester = %+v, want ended with its way pending", fix)
		}
		r.fails(fixHead, "k-ci2")
		return r
	}
	t.Run("fix 2 opens only after the tester's end takes fix 1's way", func(t *testing.T) {
		r := pending(t)
		assertStrings(t, "the fixes while fix 1's way is pending", r.fixes(), []string{fixOf(1, "ended", "k-ci")})
		onStored(t, r.pool, r.engine, "LEGION-208", func(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
			row, err := r.engine.store.CIFailureAt(ctx, tx, issue.Key, issue.Generation, "a", "k-ci2", fixHead)
			if err == nil && row != nil {
				err = fmt.Errorf("k-ci2's failure while fix 1's way is pending has a row %+v, want none", *row)
			}
			return err
		})
		if got, want := r.besideMarks(2), "1 "+besideFix1(t, claim.RoleTester); got != want {
			t.Fatalf("the tester's start names %q, want %q: started before fix 1, marked beside it and bound to its branch beside fix 1 until it ends", got, want)
		}

		// The tester's end and the fix rule at its tail, in one transaction, as applyAndFix runs a
		// fact's handler and then the rule.
		onStored(t, r.pool, r.engine, "LEGION-208", func(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
			pr, err := r.engine.store.PullRequest(ctx, tx, issue.Key)
			if err != nil {
				return err
			}
			if err := r.engine.transition(ctx, tx, issue, TriggerTesterPassed, "", record.PhaseRow{}, pr, moveWhy{}); err != nil {
				return err
			}
			return r.engine.fixRule(ctx, tx, issue.Key)
		})

		onStored(t, r.pool, r.engine, "LEGION-208", func(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
			fix, err := r.engine.store.FixAt(ctx, tx, issue.Key, issue.Generation, "a", 1)
			if err == nil && (fix == nil || fix.Way != wayOn) {
				err = fmt.Errorf("fix 1 after the tester's end = %+v, want way %d taken", fix, wayOn)
			}
			return err
		})
		assertStrings(t, "the fixes after the tester's end", r.fixes(), []string{fixOf(1, "ended", "k-ci"), "2 reading k-ci2@" + fixHead + " place 1"})
		assertStrings(t, "the deletions", r.deletions(), nil)
	})
}

// A way 2 keeps the branches beside its fix, and the step it runs again takes its commits from its
// own. Fix 1 runs beside the running tester, which is bound to B1; fix 1 ends with commits of its own
// at Y while the tester runs, so its way waits; the tester then pushes c2 to B1, at P, after the
// fix's end push. The tester's pass takes way 2: no deletion is queued, and the tester's start that
// the way runs again holds fix 1, its kept branch B1 and the pushed head P (startFacts.Kept,
// Branch, Pushed), from which its task tells it to take c2. Its red is the deletion the rule before
// queued at every way 2 or 3 (afterFix): the way 2 then queues the deletion of fix 1's branches.
func TestAWay2KeepsTheBranchesAndTheRerunTakesItsOwn(t *testing.T) {
	const pushed = "4444444444444444444444444444444444444444"
	pendingWay2 := func(t *testing.T) *fixRig {
		r := newFixRig(t, returnEngine(), []string{"implementer", "tester", "reviewer"}, TriggerImplementationReady)
		r.fails(fixHead, "k-ci")
		r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})
		r.fixEnds(headTwo)
		if fix := r.latestFix(); fix.State != record.FixEnded || fix.Way != 0 {
			t.Fatalf("fix 1 after its end beside the running tester = %+v, want ended with its way pending", fix)
		}
		r.recordsPush(claim.RoleTester, pushed)
		r.move(TriggerTesterPassed, "", record.PhaseRow{})
		r.assertWayTaken(wayAfterFix)
		return r
	}
	t.Run("the way keeps B1 and the rerun tester holds it", func(t *testing.T) {
		r := pendingWay2(t)
		assertStrings(t, "the deletions", r.deletions(), nil)
		facts := r.startFactsIn(phase.Testing)
		if !facts.Kept || facts.Fix != 1 || facts.Branch != besideFix1(t, claim.RoleTester) || facts.Pushed != pushed {
			t.Fatalf("the rerun tester's start facts = %+v, want fix 1's kept branch %s at %s", facts, besideFix1(t, claim.RoleTester), pushed)
		}
	})
}

// A fix whose own branch cannot be named opens not at all (refuseFixName). The issue's key, of 147
// bytes, and its generation's byte leave a branch beside a fix no room for the task's part within
// GitHub's 255-byte ref (claim.BesideTaskPrefix). With its task in reviewing, taking a fix, a CI
// check that fails at its head starts no fix, its failure's row goes to the owner, and the owner is
// told once, by one ci-failed notice naming the check and the head. Its red is the refusal taken
// out of openFix, where the same failure opens fix 1.
func TestAFixWhoseBranchCannotBeNamedIsRefused(t *testing.T) {
	key := "LEGION-" + strings.Repeat("2", 140)
	if _, err := claim.BesideBranch(key, 1, "a", 1, claim.RoleImplementer); err == nil {
		t.Fatalf("a branch beside fix 1 of %s can be named, want none: the test would reach no refusal", key)
	}
	pool := migratedPool(t)
	engine := returnEngine()
	seedIssue(t, pool, record.Issue{Key: key, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: key,
		Project: "LEGION", Title: "root", Phase: phase.Admitted, Generation: 1, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
	seedTreeLifecycle(t, pool, key)
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: key, Repo: statesRepo, Number: 42,
		Branch: "legion/" + key, HeadSHA: fixHead, Failing: []string{}})
	seedStepsPlan(t, pool, key, 1, "implementer", "tester", "reviewer")
	firstStep(t, pool, engine, key)
	move(t, pool, engine, key, TriggerImplementationReady, "", record.PhaseRow{})
	move(t, pool, engine, key, TriggerTesterPassed, "", record.PhaseRow{})
	if issue := issueOf(t, pool, engine, key); issue.Phase != phase.Reviewing {
		t.Fatalf("%s stands in %s, want reviewing", key, issue.Phase)
	}

	applyStates(t, pool, engine, key, statesRepo, fixHead, 1, ofSet(inCI(stateOf("k-ci", "no", "fail", "ci"), "failure"), fixChecks...))

	assertStrings(t, "the fixes", outboxStrings(t, pool, `select fix::text from task_fixes where issue = $1`, key), nil)
	assertStrings(t, "the failures", outboxStrings(t, pool, `select check_id || '@' || head || ' ' || went_to || ' ' || fix
		from ci_failures where issue = $1 order by check_id`, key), []string{failureOf("k-ci", "owner", 0)})
	told := noticesOf(t, pool, key, "ci-failed")
	if len(told) != 1 || told[0].Role != claim.RoleArchitect {
		t.Fatalf("the ci-failed notices = %+v, want one to the owner", told)
	}
	assertNames(t, "the ci-failed notice", told[0].Reason, []string{"k-ci", fixHead})
}

// The worker-died notice of a fix's failed claim names where the work beside the fix stands: the
// sentence built from the typed facts of fix 1 (besideWorkSentence), the prefix of the branches
// beside it (claim.BesideFixPrefix) and the fix's own branch F (claim.BesideBranch). Its red is the
// sentence taken out of the notice.
func TestAFailedClaimOfAFixNamesWhereItsWorkStands(t *testing.T) {
	prefix, err := claim.BesideFixPrefix("LEGION-208", 1, "a", 1)
	if err != nil {
		t.Fatalf("name the prefix beside fix 1: %v", err)
	}
	sentence := besideWorkSentence("LEGION-208", 1, "a", 1)
	if !strings.Contains(sentence, prefix) || !strings.Contains(sentence, besideFix1(t, claim.RoleImplementer)) {
		t.Fatalf("the sentence of fix 1's work = %q, want it to name the prefix %s and F %s", sentence, prefix, besideFix1(t, claim.RoleImplementer))
	}
	r := startedFix(t, intake.FixJobRead{Name: "unit", URL: fixPage})
	r.apply("fix-claim-failed", intake.ClaimFailed{Issue: "LEGION-208", Role: claim.RoleImplementer, Reason: "the process died"})
	died := r.workerDied()
	if len(died) != 1 || !strings.Contains(died[0], strings.TrimSpace(sentence)) {
		t.Fatalf("the worker-died notices = %q, want the fix's one naming %q", died, sentence)
	}
}
