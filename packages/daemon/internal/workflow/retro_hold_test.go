package workflow

import (
	"bytes"
	"context"
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

// heldParentChild is the child of LEGION-208 that its parent's hold before its retro waits for.
const heldParentChild = "LEGION-209"

// heldParentReason is the reason of the retro-held notice LEGION-208 gets while heldParentChild is
// in the workflow beneath it: it ends with the daemon's sentence on a status not taken yet and its
// rules for ending a child, as the pr-merged notice does.
const heldParentReason = "LEGION-208 passed its last step, and Legion holds it before its retro while these issues beneath it are still in the workflow: LEGION-209. " +
	"A parent is accepted only once no issue beneath it is still in the workflow. Run each one to done, or end it; Legion starts the retro when the last one leaves. " +
	record.StatusNotTakenYet + " " + record.EndChildRule

// approveReview settles LEGION-208's review round as approved, as settleRound moves it: the
// reviewer's phases row as the round left it, with the handoff the retro's task names written to
// the store so a later read of the row finds it, and no reason (review.go's roundApproved).
func approveReview(t *testing.T, pool *pgxpool.Pool, engine *Engine) {
	t.Helper()
	row := roleRowOf(t, pool, engine, "LEGION-208", claim.RoleReviewer)
	row.HandoffRecord, row.HandoffVersion, row.HandoffCommit, row.Summary = "review-record", 1, "c0ffee", "approved"
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		if err := engine.store.PutPhase(ctx, tx, row); err != nil {
			return err
		}
		issue, err := engine.store.Issue(ctx, tx, "LEGION-208")
		if err != nil {
			return err
		}
		pr, err := engine.store.MergePullRequest(ctx, tx, "LEGION-208")
		if err != nil {
			return err
		}
		s, err := firstScope(ctx, tx, engine, *issue)
		if err != nil {
			return err
		}
		return engine.transition(ctx, tx, s, TriggerReviewApproved, record.TaskPlace{}, row, pr, "")
	})

}

// retroStarts is the count of starts step_starts holds for the retro of key, and how many rows
// hold one: the retro's place is one row, whatever task the count is kept under.
func retroStarts(t *testing.T, pool *pgxpool.Pool, key string) (starts int64, rows int) {
	t.Helper()
	if err := pool.QueryRow(t.Context(), `select coalesce(sum(starts), 0), count(*) from step_starts where issue = $1 and kind = 'retro'`, key).Scan(&starts, &rows); err != nil {
		t.Fatalf("read the retro's starts of %s: %v", key, err)
	}
	return starts, rows
}

// startsIn is the starts of role key's outbox holds of phase p.
func startsIn(t *testing.T, pool *pgxpool.Pool, key string, role claim.Role, p phase.Phase) []record.SuperviseRequest {
	t.Helper()
	var in []record.SuperviseRequest
	for _, start := range roleStartsOf(t, pool, key, role) {
		if start.Phase == p {
			in = append(in, start)
		}
	}
	return in
}

// childLeavesDone is a person's done on heldParentChild in Dispatch, which takes it out of the
// workflow (leave).
func childLeavesDone(t *testing.T, pool *pgxpool.Pool, engine *Engine) {
	t.Helper()
	fact := intake.DispatchIssue{Key: heldParentChild, ResolvedRepo: "sjawhar/legion", Seq: 2, Type: "issue.updated", Status: "done",
		Title: heldParentChild, Parent: "LEGION-208", Rank: "V"}
	if result := applyFacts(t, pool, engine)("child-done", fact); result.Refusal != nil {
		t.Fatalf("the done of %s was refused: %+v", heldParentChild, result.Refusal)
	}
}

// lastStepEnd is the result the newest end of role's step in key's step records names.
func lastStepEnd(t *testing.T, pool *pgxpool.Pool, key, role string) string {
	t.Helper()
	result := ""
	for _, write := range stepWrites(t, pool, key) {
		if write.Role == role && write.EndedAt != nil && write.Result != nil {
			result = *write.Result
		}
	}
	return result
}

// A parent whose plan has no acceptance step passes its last step while an issue beneath it is in
// the workflow. A parent is accepted only once no issue beneath it is still in the workflow, so
// Legion holds it before its retro: issues.phase held, from retro for its children, its task held
// at the retro's place for its children, with no slot and no retro started, its owner told once in
// a retro-held notice naming the child. The child's
// leave is the last one, so the hold ends, the parent stands in retro, its Dispatch status is
// written retro, and the implementer starts the retro once, at a raised entry, with the task the
// approval would have started it with. The control is the same parent with no child, which moves
// into its retro at the approval with no hold. The twin is an issue with no plan rows, which walks
// today's line: its approval takes it to accepting, and the child's leave changes nothing of it.
func TestAParentWithNoAcceptanceIsHeldBeforeItsRetro(t *testing.T) {
	t.Parallel()
	// parent is LEGION-208 walking `implementer reviewer`, standing in reviewing at step 2, with
	// heldParentChild in implementing beneath it where child is true.
	parent := func(t *testing.T, child bool) (*pgxpool.Pool, *Engine, *bytes.Buffer) {
		t.Helper()
		pool := migratedPool(t)
		engine, logged := loggedEngine()
		root := stepRoot(t, pool, phase.Admitted)
		seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "reviewer")
		if child {
			stepChild(t, pool, heldParentChild, phase.Implementing, nil)
		}
		move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
		move(t, pool, engine, root.Key, TriggerImplementationReady, record.TaskPlace{}, record.PhaseRow{})
		assertWalksAt(t, pool, engine, root.Key, phase.Reviewing, record.TaskPlace{Kind: record.PlaceStep, Position: 2})
		return pool, engine, logged
	}

	// The control: with no issue beneath it, the approval starts the retro, as it would with no
	// hold to write. Its start's task is the task the release below must start the retro with.
	pool, engine, _ := parent(t, false)
	approveReview(t, pool, engine)
	assertWalksAt(t, pool, engine, "LEGION-208", phase.Retro, record.TaskPlace{Kind: record.PlaceRetro})
	controlStarts := startsIn(t, pool, "LEGION-208", claim.RoleImplementer, phase.Retro)
	if len(controlStarts) != 1 {
		t.Fatalf("the control's approval wrote %d starts of the retro, want one", len(controlStarts))
	}
	if starts, rows := retroStarts(t, pool, "LEGION-208"); starts != 1 || rows != 1 {
		t.Fatalf("the control's retro counts %d starts in %d rows, want one start", starts, rows)
	}
	if notices := noticesOf(t, pool, "LEGION-208", "retro-held"); len(notices) != 0 {
		t.Fatalf("the control told %+v, want no retro-held notice", notices)
	}

	t.Run("the approval holds the parent before its retro, and the child's leave starts it", func(t *testing.T) {
		pool, engine, logged := parent(t, true)
		implementerEntry := roleRowOf(t, pool, engine, "LEGION-208", claim.RoleImplementer).Entry
		statuses := statusWrites(t, pool, "LEGION-208")

		approveReview(t, pool, engine)

		held := issueOf(t, pool, engine, "LEGION-208")
		if held.Phase != phase.Held || held.Hold == nil || *held.Hold != (record.Hold{From: phase.Retro, Reason: record.HoldForChildren}) {
			t.Fatalf("LEGION-208 after the approval stands in %s with hold %+v, want held from retro for its children", held.Phase, held.Hold)
		}
		if task := taskOf(t, pool, engine, "LEGION-208"); task.State != record.TaskHeld || task.HoldReason != record.HoldForChildren ||
			task.Place != (record.TaskPlace{Kind: record.PlaceRetro}) {
			t.Fatalf("task a after the approval is %s (hold reason %q) at %+v, want held at its retro for its children", task.State, task.HoldReason, task.Place)
		}
		if record.KeepsSlot(held) {
			t.Fatal("the held parent keeps a slot, want none: its retro holds none, and its child may need the slot")
		}
		if got := startsIn(t, pool, "LEGION-208", claim.RoleImplementer, phase.Retro); len(got) != 0 {
			t.Fatalf("the approval wrote the retro's starts %+v, want none", got)
		}
		if entry := roleRowOf(t, pool, engine, "LEGION-208", claim.RoleImplementer).Entry; entry != implementerEntry {
			t.Fatalf("the implementer's entry after the approval is %d, want %d: no retro started", entry, implementerEntry)
		}
		if starts, rows := retroStarts(t, pool, "LEGION-208"); rows != 0 {
			t.Fatalf("the approval counted %d starts of the retro, want none", starts)
		}
		if result := lastStepEnd(t, pool, "LEGION-208", "reviewer"); result != "passed" {
			t.Fatalf("the reviewer's step ended %q, want passed: the step passed, and the hold stands after it", result)
		}
		if got := statusWrites(t, pool, "LEGION-208"); len(got) != len(statuses) {
			t.Fatalf("the approval queued the status writes %+v, want none: a hold has no status", got[len(statuses):])
		}
		want := []record.Notice{{Kind: "retro-held", Role: claim.RoleImplementer, Phase: phase.Retro, Reason: heldParentReason}}
		if got := noticesOf(t, pool, "LEGION-208", "retro-held"); !reflect.DeepEqual(got, want) {
			t.Fatalf("the retro-held notices = %+v, want %+v", got, want)
		}
		if line := logged.String(); strings.Count(line, "workflow: a parent is held before its retro for the issues beneath it") != 1 || !strings.Contains(line, heldParentChild) {
			t.Fatalf("the log of the approval = %q, want one line of the hold naming %s", line, heldParentChild)
		}
		statuses = statusWrites(t, pool, "LEGION-208")

		childLeavesDone(t, pool, engine)

		assertWalksAt(t, pool, engine, "LEGION-208", phase.Retro, record.TaskPlace{Kind: record.PlaceRetro})
		released := issueOf(t, pool, engine, "LEGION-208")
		if released.Status != "retro" {
			t.Fatalf("LEGION-208 after the release has status %q, want retro", released.Status)
		}
		starts := startsIn(t, pool, "LEGION-208", claim.RoleImplementer, phase.Retro)
		if len(starts) != 1 {
			t.Fatalf("the release wrote %d starts of the retro, want one", len(starts))
		}
		if starts[0].Entry != implementerEntry+1 || starts[0].ResendEntry {
			t.Fatalf("the retro's start is at entry %d (resend %v), want a raised entry %d", starts[0].Entry, starts[0].ResendEntry, implementerEntry+1)
		}
		if starts[0].Task != controlStarts[0].Task {
			t.Fatalf("the retro's task = %q, want the task the approval starts it with: %q", starts[0].Task, controlStarts[0].Task)
		}
		if count, rows := retroStarts(t, pool, "LEGION-208"); count != 1 || rows != 1 {
			t.Fatalf("the release counted %d starts of the retro in %d rows, want one start", count, rows)
		}
		if got, want := statusWrites(t, pool, "LEGION-208")[len(statuses):], []record.StatusWrite{record.OrdinaryStatusWrite("retro", "needs_review", "")}; !reflect.DeepEqual(got, want) {
			t.Fatalf("the release queued the status writes %+v, want %+v: the retro's, over the review's Dispatch shows", got, want)
		}
		if got := noticesOf(t, pool, "LEGION-208", "retro-held"); len(got) != 1 {
			t.Fatalf("the release told %d more retro-held notices, want none", len(got)-1)
		}
		if line := logged.String(); !strings.Contains(line, "act=children_done") {
			t.Fatalf("the log of the release = %q, want the act children_done", line)
		}
	})

	// A child released and not started yet stands at admitted. The held parent is no accepted
	// ancestor of it (record.Accepted), so nothing holds it there and the hold waits for it: read
	// as accepted, the parent would keep it at admitted until the parent closed, leave it out of the
	// wait, and start its retro without it.
	t.Run("a child at admitted beneath the held parent is not held under it, and the hold waits for it", func(t *testing.T) {
		pool, engine, _ := parent(t, false)
		stepChild(t, pool, heldParentChild, phase.Admitted, nil)

		approveReview(t, pool, engine)

		if held := issueOf(t, pool, engine, "LEGION-208"); !record.HeldForChildren(held) {
			t.Fatalf("LEGION-208 after the approval stands in %s with hold %+v, want held from retro for its children", held.Phase, held.Hold)
		}
		if ancestor := record.AcceptedAncestor(*issueRecord(t, pool, heldParentChild), treeByKey(t, pool, "LEGION-208")); ancestor != "" {
			t.Fatalf("the accepted ancestor of %s = %q, want none: nothing of the held parent is accepted", heldParentChild, ancestor)
		}
		if got := noticesOf(t, pool, "LEGION-208", "retro-held"); len(got) != 1 || got[0].Reason != heldParentReason {
			t.Fatalf("the retro-held notices = %+v, want the one naming %s", got, heldParentChild)
		}
		childLeavesDone(t, pool, engine)
		assertWalksAt(t, pool, engine, "LEGION-208", phase.Retro, record.TaskPlace{Kind: record.PlaceRetro})
	})

	// An issue with no plan rows walks today's line, whose review approval goes to accepting
	// with an issue beneath it in the workflow, as today; the child's leave starts nothing of it.
	t.Run("an issue with no plan rows goes to accepting, and the child's leave starts nothing", func(t *testing.T) {
		pool := migratedPool(t)
		engine, logged := loggedEngine()
		stepRoot(t, pool, phase.Reviewing)
		stepChild(t, pool, heldParentChild, phase.Implementing, nil)

		approveReview(t, pool, engine)

		if got := issueOf(t, pool, engine, "LEGION-208"); got.Phase != phase.Accepting || got.Hold != nil {
			t.Fatalf("LEGION-208 after the approval stands in %s with hold %+v, want accepting with none", got.Phase, got.Hold)
		}
		childLeavesDone(t, pool, engine)
		if got := issueOf(t, pool, engine, "LEGION-208"); got.Phase != phase.Accepting || got.Hold != nil {
			t.Fatalf("LEGION-208 after the child's leave stands in %s with hold %+v, want accepting still", got.Phase, got.Hold)
		}
		if got := startsIn(t, pool, "LEGION-208", claim.RoleImplementer, phase.Retro); len(got) != 0 {
			t.Fatalf("the child's leave wrote the retro's starts %+v, want none", got)
		}
		if notices := noticesOf(t, pool, "LEGION-208", "retro-held"); len(notices) != 0 {
			t.Fatalf("an issue with no plan rows was told %+v, want no retro-held notice", notices)
		}
		if line := logged.String(); strings.Contains(line, "children_done") || strings.Contains(line, "held before its retro") {
			t.Fatalf("the log = %q, want no hold and no release of one", line)
		}
	})
}

// A parent whose plan holds the owner's sign-off before another step passes the sign-off with no
// issue beneath it in the workflow, and a child enters the workflow beneath it after that. No
// sign-off is left to refuse the parent while the child runs, and its head proves its children's
// merged work, so the pass of its last step, the reviewer's approval, holds it before its retro:
// issues.phase held, from retro for its children, its task held at the retro's place for its
// children, no retro started, and its owner told once in a retro-held notice naming the child. The
// child's leave
// starts the retro, told the review the reviewer passed. The known pass is a plan whose sign-off
// is its last step, which the sign-off moves into its retro with no hold.
func TestAParentWhoseSignOffIsNotItsLastStepIsHeldBeforeItsRetro(t *testing.T) {
	t.Parallel()
	t.Run("the approval after the sign-off holds the parent before its retro, and the child's leave starts it", func(t *testing.T) {
		pool := migratedPool(t)
		engine, logged := loggedEngine()
		root := stepRoot(t, pool, phase.Admitted)
		seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "acceptance:owner", "reviewer")
		move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
		reportsHandoff(t, pool, engine, root.Key, claim.RoleImplementer, 3)
		move(t, pool, engine, root.Key, TriggerImplementationReady, record.TaskPlace{}, record.PhaseRow{})
		if result := signsOff(t, pool, engine, root.Key); result.Refusal != nil {
			t.Fatalf("the sign-off was refused: %+v", result.Refusal)
		}
		assertWalksAt(t, pool, engine, root.Key, phase.Reviewing, record.TaskPlace{Kind: record.PlaceStep, Position: 3})
		stepChild(t, pool, heldParentChild, phase.Implementing, nil)

		approveReview(t, pool, engine)

		held := issueOf(t, pool, engine, root.Key)
		if held.Phase != phase.Held || held.Hold == nil || *held.Hold != (record.Hold{From: phase.Retro, Reason: record.HoldForChildren}) {
			t.Fatalf("%s after the approval stands in %s with hold %+v, want held from retro for its children", root.Key, held.Phase, held.Hold)
		}
		if task := taskOf(t, pool, engine, root.Key); task.State != record.TaskHeld || task.HoldReason != record.HoldForChildren ||
			task.Place != (record.TaskPlace{Kind: record.PlaceRetro}) {
			t.Fatalf("task a after the approval is %s (hold reason %q) at %+v, want held at its retro for its children", task.State, task.HoldReason, task.Place)
		}
		if got := startsIn(t, pool, root.Key, claim.RoleImplementer, phase.Retro); len(got) != 0 {
			t.Fatalf("the approval wrote the retro's starts %+v, want none", got)
		}
		want := []record.Notice{{Kind: "retro-held", Role: claim.RoleImplementer, Phase: phase.Retro, Reason: heldParentReason}}
		if got := noticesOf(t, pool, root.Key, "retro-held"); !reflect.DeepEqual(got, want) {
			t.Fatalf("the retro-held notices = %+v, want %+v", got, want)
		}
		if line := logged.String(); strings.Count(line, "workflow: a parent is held before its retro for the issues beneath it") != 1 || !strings.Contains(line, heldParentChild) {
			t.Fatalf("the log of the approval = %q, want one line of the hold naming %s", line, heldParentChild)
		}

		childLeavesDone(t, pool, engine)

		assertWalksAt(t, pool, engine, root.Key, phase.Retro, record.TaskPlace{Kind: record.PlaceRetro})
		starts := startsIn(t, pool, root.Key, claim.RoleImplementer, phase.Retro)
		if len(starts) != 1 {
			t.Fatalf("the release wrote %d starts of the retro, want one", len(starts))
		}
		if !strings.Contains(starts[0].Task, "Handoff: review version 1") || strings.Contains(starts[0].Task, "Handoff: implement") {
			t.Fatalf("the retro's start = %+v, want it told Handoff: review version 1 and no Handoff: implement", starts[0])
		}
		if got := noticesOf(t, pool, root.Key, "retro-held"); len(got) != 1 {
			t.Fatalf("the release told %d more retro-held notices, want none", len(got)-1)
		}
		if line := logged.String(); !strings.Contains(line, "act=children_done") {
			t.Fatalf("the log of the release = %q, want the act children_done", line)
		}
	})

	t.Run("the known pass: a sign-off that is the last step moves the parent into its retro with no hold", func(t *testing.T) {
		pool := migratedPool(t)
		engine, logged := loggedEngine()
		root := stepRoot(t, pool, phase.Admitted)
		seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "reviewer", "acceptance:owner")
		move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
		move(t, pool, engine, root.Key, TriggerImplementationReady, record.TaskPlace{}, record.PhaseRow{})
		approveReview(t, pool, engine)
		assertWalksAt(t, pool, engine, root.Key, phase.Accepting, record.TaskPlace{Kind: record.PlaceStep, Position: 3})

		if result := signsOff(t, pool, engine, root.Key); result.Refusal != nil {
			t.Fatalf("the sign-off was refused: %+v", result.Refusal)
		}

		assertWalksAt(t, pool, engine, root.Key, phase.Retro, record.TaskPlace{Kind: record.PlaceRetro})
		if got := startsIn(t, pool, root.Key, claim.RoleImplementer, phase.Retro); len(got) != 1 {
			t.Fatalf("the sign-off wrote %d starts of the retro, want one", len(got))
		}
		if notices := noticesOf(t, pool, root.Key, "retro-held"); len(notices) != 0 {
			t.Fatalf("the sign-off told %+v, want no retro-held notice", notices)
		}
		if line := logged.String(); strings.Contains(line, "held before its retro") {
			t.Fatalf("the log = %q, want no hold", line)
		}
	})
}

// A parent whose last step is of a role the deployment defines, `implementer checker`, passes it
// while a child is in the workflow beneath it and is held before its retro. The child's leave starts
// the implementer's retro once, with the report of the checker's step, the role of the task's last
// step, as the pass into the retro would have given it: its commit and its verdict, under the
// checker's own name, not the tester's.
func TestAHeldRetroAfterADeploymentStepGetsItsReport(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Step)
	stepChild(t, pool, heldParentChild, phase.Implementing, nil)
	seedStepsAt(t, pool, root.Key, root.Generation, 2, map[string]string{"checker": "testing"}, "implementer", "checker")
	const sha = "0123456789abcdef0123456789abcdef01234567"
	seedPhase(t, pool, record.PhaseRow{Issue: root.Key, Role: "checker", Claim: "checker-claim", HandoffCommit: sha, Verdict: "pass",
		StepTask: "a", StepPlace: 2})

	move(t, pool, engine, root.Key, TriggerStepPassed, record.TaskPlace{}, record.PhaseRow{})
	if held := issueOf(t, pool, engine, root.Key); held.Phase != phase.Held || held.Hold == nil || held.Hold.Reason != record.HoldForChildren {
		t.Fatalf("%s after the checker's pass stands in %s with hold %+v, want held before its retro for its children", root.Key, held.Phase, held.Hold)
	}

	childLeavesDone(t, pool, engine)

	starts := startsIn(t, pool, root.Key, claim.RoleImplementer, phase.Retro)
	if len(starts) != 1 {
		t.Fatalf("the release wrote %d starts of the retro, want one", len(starts))
	}
	for _, says := range []string{" Handoff commit: " + sha + ".", " Step 2 (checker) reported: pass."} {
		if !strings.Contains(starts[0].Task, says) {
			t.Fatalf("the retro's task = %q, want it to say %q", starts[0].Task, says)
		}
	}
	if strings.Contains(starts[0].Task, testerVerdictLabel) {
		t.Fatalf("the retro's task = %q, want no %s: the report is the checker's", starts[0].Task, testerVerdictLabel)
	}
}

// The reviewer's own report can land after its approval moved a parent with no acceptance step
// into its hold before the retro. It is the first report of the run the approval ended, so it is
// taken as such a report is wherever else the issue stands (leftItsPhase): on the reviewer's own
// row and on its ended step's record, moving nothing, and the parent stays held for its child.
// The control is the same reviewer's second report, of a run whose report already stands, which
// is refused HANDOFF_NOT_CURRENT_PHASE as it is in every other phase. Before, a parent held for
// its children stood where no role works, and the first report was refused too, so its handoff
// never reached the step's record.
func TestAReviewersReportAfterItsApprovalHeldAParentForItsChildrenIsTaken(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	engine, _ := loggedEngine()
	root := stepRoot(t, pool, phase.Admitted)
	seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "reviewer")
	stepChild(t, pool, heldParentChild, phase.Implementing, nil)
	move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	move(t, pool, engine, root.Key, TriggerImplementationReady, record.TaskPlace{}, record.PhaseRow{})
	seedClaimSession(t, pool, "reviewer-claim", "ses-reviewer")
	// The review event approves the round before the reviewer reports: its row has no report yet.
	move(t, pool, engine, root.Key, TriggerReviewApproved, record.TaskPlace{}, roleRowOf(t, pool, engine, root.Key, claim.RoleReviewer))
	if held := issueOf(t, pool, engine, root.Key); !record.HeldForChildren(held) {
		t.Fatalf("%s after the approval stands in %s with hold %+v, want held from retro for its children", root.Key, held.Phase, held.Hold)
	}
	report := intake.HandoffComplete{Generation: 1, Issue: root.Key, Role: claim.RoleReviewer, Claim: "reviewer-claim", Summary: "approved",
		Commit: "c0ffee", HandoffRecord: "review-late", HandoffVersion: 1}
	apply := applyFacts(t, pool, engine)

	if result := apply("the reviewer's report", report); result.Refusal != nil {
		t.Fatalf("the reviewer's report after its approval held %s was refused %+v, want it taken", root.Key, result.Refusal)
	}
	if row := roleRowOf(t, pool, engine, root.Key, claim.RoleReviewer); !row.Completed() || row.HandoffRecord != "review-late" {
		t.Fatalf("the reviewer's row = %+v, want its report recorded, naming review-late", row)
	}
	writes := stepWrites(t, pool, root.Key)
	if last := writes[len(writes)-1]; last.Role != "reviewer" || last.Report == nil || *last.Report != "review-late" {
		t.Fatalf("the last step record write = %+v, want the reviewer's ended step reporting review-late", last)
	}
	if held := issueOf(t, pool, engine, root.Key); !record.HeldForChildren(held) {
		t.Fatalf("%s after the reviewer's report stands in %s with hold %+v, want it still held from retro for its children", root.Key, held.Phase, held.Hold)
	}

	again := report
	again.HandoffRecord = "review-again"
	want := intake.NotCurrentPhase(claim.RoleReviewer, phase.Held, root.Key)
	if result := apply("the reviewer's second report", again); !reflect.DeepEqual(result.Refusal, want) {
		t.Fatalf("the reviewer's second report = %+v, want %+v", result.Refusal, want)
	}
}
