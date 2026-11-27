package admit

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The task a parent of these tests held when it entered awaiting_children, and the reason its
// move wrote.
const (
	acHeldTask   = "Continue LEGION-300. Issue: LEGION-300. Phase: implementing."
	acHeldReason = "a review asked for changes, while these Dispatch children of LEGION-300 were open: LEGION-301 (backlog)"
)

// The controller's tick names a parent waiting in awaiting_children under `blocked` with its open
// children, as the state does (dispatch://INFRA-3320 R11): both read blockers.Blocked over the
// one judgement of the pass (blockers.ChildRows), so what the controller is told and what
// `legion state` shows agree. A child already done is no part of the row.
func TestTheTickNamesAWaitingParentBlockedByItsOpenChildren(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 2)
	spWaiting(t, pool, spParent, phase.Implementing, acHeldTask, acHeldReason, nil)
	seedMirrorChild(t, pool, spChild, "backlog", spParent)
	seedMirrorChild(t, pool, "LEGION-302", "done", spParent)

	var held []record.HeldIssue
	var blocked []record.BlockedIssue
	inTx(t, pool, func(tx pgx.Tx) {
		var err error
		if held, blocked, _, err = admission.tickRows(context.Background(), tx); err != nil {
			t.Fatalf("read the tick's rows: %v", err)
		}
	})
	if want := []record.BlockedIssue{{Issue: spParent, BlockedBy: []string{spChild}}}; !reflect.DeepEqual(blocked, want) {
		t.Fatalf("the tick's blocked list = %+v, want %+v: the waiting parent with its one open child", blocked, want)
	}
	want := record.HeldIssue{Issue: spParent, Reason: record.HeldAwaitingChildren, HeldBy: []string{spChild}}
	if !slices.ContainsFunc(held, func(row record.HeldIssue) bool { return reflect.DeepEqual(row, want) }) {
		t.Fatalf("the tick's held list = %+v, want %+v beside it", held, want)
	}
}

// The repair of a role behind the entry its phases row names (owedEntryRoles, which the entry
// sweep and every admission pass send through resumeSlottedWorkers) starts no role of a parent
// waiting in awaiting_children (dispatch://INFRA-3320 R11): that phase has no worker
// (workflow.OpenPhaseRole), so the selection passes over it. The parent here even holds a slot
// row, which puts it among the repair's candidates, and its implementer's claim stands behind the
// entry its phases row names. The control is the same record in implementing, whose implementer
// the sweep sends that entry's work.
func TestTheEntrySweepStartsNoRoleOfAWaitingParent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		at     phase.Phase
		starts int
	}{
		{name: "in implementing, the control", at: phase.Implementing, starts: 1},
		{name: "in awaiting_children", at: phase.AwaitingChildren},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := quiet(t, 1)
			spSeedParent(t, pool, spParent, tc.at, true, func(issue *record.Issue) {
				if tc.at == phase.AwaitingChildren {
					waited := fixedNow.Add(-2 * time.Hour)
					issue.ChildrenWait = &record.ChildrenWait{Start: phase.Implementing, Task: acHeldTask, Reason: acHeldReason, Since: waited}
					issue.ChildrenWaitedAt = &waited
				}
			})
			token := putWorkerClaim(t, pool, spParent, spParent, claim.RoleImplementer, 1, supervise.StateIdle, "")
			markGivenEntry(t, pool, token, seededEntry-1)
			spSeedPhase(t, pool, record.PhaseRow{Issue: spParent, Role: claim.RoleImplementer, Claim: token, Entry: seededEntry,
				EntryTask: seededEntryTask(spParent), EntryRaisedAt: fixedNow.Add(-time.Hour)})

			var owes bool
			inTx(t, pool, func(tx pgx.Tx) {
				var err error
				if owes, err = admission.OwesEntryWork(context.Background(), tx); err != nil {
					t.Fatalf("ask whether any role is owed its entry's work: %v", err)
				}
			})
			if owes != (tc.starts > 0) {
				t.Fatalf("OwesEntryWork = %t, want %t", owes, tc.starts > 0)
			}
			applySweep(t, pool, admission, "sweep")

			if starts := convergeStarts(t, pool, spParent); len(starts) != tc.starts {
				t.Fatalf("the start rows of %s in %s = %+v, want %d", spParent, tc.at, starts, tc.starts)
			}
		})
	}
}

// awaitingRun is a quiet project whose root LEGION-950 waits in awaiting_children, under the cap
// of four the stalled-tree tests take, with its architect idle and a controller registered to be
// told, measured at the deployment's own bound.
func awaitingRun(t *testing.T) (*pgxpool.Pool, *Admission) {
	t.Helper()
	pool := migratedPool(t)
	admission := quiet(t, 4)
	admission.UseStalledTreeNotice(deploymentBound(t, ""))
	registerController(t, pool)
	spWaiting(t, pool, "LEGION-950", phase.Implementing, "Continue LEGION-950. Issue: LEGION-950. Phase: implementing.",
		"a review asked for changes, while these Dispatch children of LEGION-950 were open: LEGION-951", nil)
	seedMirror(t, pool, "LEGION-950", "in_progress")
	seedClaim(t, claimStore(t, pool), "LEGION-950", "LEGION-950", claim.RoleArchitect, supervise.StateIdle)
	return pool, admission
}

// The stalled-tree measure reads a parent in awaiting_children as it reads a parent whose own first
// phase waits on its children (dispatch://INFRA-3320 R13; dispatch://INFRA-3036's rule, which
// lineHolds.waitsOnWorkOutsideItsTree states): a wait on children the daemon has released is a
// wait by design whatever the line does, and a wait on a child nobody released is a wait by
// design only while the project's line can move, since releasing that child is the tree's own
// work. Each project is the waiting root alone, quiet past the bound, measured over three passes.
// The case of a line that moves, because a claim of another tree is at work, is a subtest of
// TestParentWaitsHoldsNoSlotAndComesBack, over the same seed.
func TestTheMeasureReadsAWaitingParentAsAFirstPhaseChildrenWait(t *testing.T) {
	const (
		root  = "LEGION-950"
		child = "LEGION-951"
		other = "OTHER-9"
	)

	t.Run("a child nobody released, the line stopped", func(t *testing.T) {
		pool, admission := awaitingRun(t)
		seedMirrorChild(t, pool, child, "backlog", root)
		for pass, roots := range waitPasses(t, pool, admission, nil, root) {
			if want := []string{root}; !reflect.DeepEqual(roots, want) {
				t.Fatalf("after pass %d the controller was told of %v, want %v: a wait on a child nobody released is the tree's own work", pass+1, roots, want)
			}
		}
	})

	t.Run("a child the daemon released, the line stopped", func(t *testing.T) {
		pool, admission := awaitingRun(t)
		// The released child: recorded in the tree, in implementing, and itself waiting on an
		// issue of another project, so it gave its own slot back and nothing of the project can
		// move. Neither wait is the tree's own work.
		seedQuietChild(t, pool, root, child, "A1", phase.Implementing, "in_progress")
		takeSlot(t, pool, child)
		seedMirrorChild(t, pool, child, "in_progress", root, other)
		seedMirrorIn(t, pool, "OTHER", other, "in_progress")
		seedClaim(t, claimStore(t, pool), root, child, claim.RoleImplementer, supervise.StateIdle)
		for pass, roots := range waitPasses(t, pool, admission, []string{child}, root, child) {
			if roots != nil {
				t.Fatalf("pass %d told the controller of %v, want nothing: %s waits on a child the daemon released, and that child waits on %s",
					pass+1, roots, root, other)
			}
		}
		if got := issue(t, pool, root); got.Phase != phase.AwaitingChildren {
			t.Fatalf("%s after the passes = %s, want it waiting still on its released child", root, got.Phase)
		}
	})
}

// A root that waited in awaiting_children and that a person moved to the backlog lingers at done
// with nothing held (workflow's leave), and a person's todo then re-admits it as a new generation
// at admitted with no held start: the start it held went with the generation that held it. Its
// first phase waits on its still open child under the children-first rule, as any re-admitted
// root's does.
func TestARootReadmittedFromTheBacklogHoldsNoStart(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 2)
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories,
		Steps: config.DesignGateRootIssues.Steps(), Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
	spWaiting(t, pool, spParent, phase.Implementing, acHeldTask, acHeldReason, func(issue *record.Issue) { issue.LastDispatchSeq = 1 })
	seedMirrorChild(t, pool, spChild, "backlog", spParent)

	apply(t, pool, admission, "moved-to-backlog", intake.DispatchIssue{Key: spParent, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated",
		Status: "backlog", Title: spParent, Rank: spParent, HandedOver: true}, engine)
	if onBacklog := issue(t, pool, spParent); onBacklog.Phase != phase.Done || onBacklog.ChildrenWait != nil || onBacklog.LingerUntil == nil {
		t.Fatalf("the root on the backlog = phase %s, held start %+v, linger %v; want done, nothing held, lingering", onBacklog.Phase, onBacklog.ChildrenWait, onBacklog.LingerUntil)
	}

	apply(t, pool, admission, "readmitted", intake.DispatchIssue{Key: spParent, ResolvedRepo: "acme/widgets", Seq: 3, Type: "issue.updated",
		Status: "todo", Title: spParent, Rank: spParent, HandedOver: true}, engine)

	got := issue(t, pool, spParent)
	if got.Generation != 2 || got.Phase != phase.Admitted || got.ChildrenWait != nil {
		t.Fatalf("the re-admitted root = generation %d, phase %s, held start %+v; want generation 2 at admitted holding nothing",
			got.Generation, got.Phase, got.ChildrenWait)
	}
}

// R11's root that the controller moved to the backlog while it waited on its children: the
// controller sets it back to todo once an open child stands under it, which is the controller
// skill's rule (skills/legion-controller), outside this package. From that todo the daemon's half
// follows: the root waits in the admission line at todo for a slot, as any re-admitted root does,
// and holds that slot only until its design gate opens and its first phase waits on its open
// child. The cap's one slot is another tree's issue's when the todo is applied, so the re-admitted
// root stands in the line holding none; once that issue leaves the workflow the next pass admits
// the root to the slot it frees; its architect registers the design gate and the person approves
// it, and with the child still open the root starts no phase, gives the slot back and stands in
// admission.blocked with its child.
func TestARootSetBackToTodoFromTheBacklogWaitsInTheLineAndHoldsItsSlotUntilItsGateOpens(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 1)
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories,
		Steps: config.DesignGateRootIssues.Steps(), Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
	seedStarted(t, pool, spFiller, phase.Implementing, nil)
	spWaiting(t, pool, spParent, phase.Implementing, acHeldTask, acHeldReason, func(issue *record.Issue) { issue.LastDispatchSeq = 1 })
	seedMirrorChild(t, pool, spChild, "backlog", spParent)

	apply(t, pool, admission, "moved-to-backlog", intake.DispatchIssue{Key: spParent, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated",
		Status: "backlog", Title: spParent, Rank: spParent, HandedOver: true}, engine)
	if onBacklog := issue(t, pool, spParent); onBacklog.Phase != phase.Done || onBacklog.LingerUntil == nil {
		t.Fatalf("the root on the backlog = phase %s, linger %v; want done and lingering", onBacklog.Phase, onBacklog.LingerUntil)
	}

	apply(t, pool, admission, "set-back-to-todo", intake.DispatchIssue{Key: spParent, ResolvedRepo: "acme/widgets", Seq: 3, Type: "issue.updated",
		Status: "todo", Title: spParent, Rank: spParent, HandedOver: true}, engine)
	if got := issue(t, pool, spParent); got.Generation != 2 || got.Phase != phase.Admitted || holdsSlot(t, pool, spParent) {
		t.Fatalf("the root set back to todo = generation %d, phase %s, holding a slot %t (slots %v); want generation 2 at admitted holding none while the cap is full",
			got.Generation, got.Phase, holdsSlot(t, pool, spParent), slots(t, pool))
	}
	assertWaiting(t, pool, []string{spParent})

	// The other tree's issue leaves the workflow, and the next pass admits the root to the slot it
	// frees.
	finished := issue(t, pool, spFiller)
	finished.Phase, finished.Status = phase.Done, "done"
	putIssue(t, pool, finished)
	spPass(t, pool, admission)
	if !holdsSlot(t, pool, spParent) || holdsSlot(t, pool, spFiller) {
		t.Fatalf("slots after the pass = %v, want the root's alone", slots(t, pool))
	}
	if starts := architectStarts(t, pool, spParent); starts != 1 {
		t.Fatalf("starts of the root's architect = %d, want one", starts)
	}
	assertWaiting(t, pool, nil)

	// Its architect registers the design gate and the person approves it. The child is open, so the
	// root starts no phase and gives the slot back.
	seedGate(t, pool, record.DesignGate{Issue: spParent, ArtifactID: "artifact-300", LatestVersion: 1})
	applyFact(t, pool, admission, "gate-approved", intake.DispatchArtifact{Key: spParent, ArtifactID: "artifact-300",
		Kind: intake.DispatchArtifactApproved, Version: 1, Mark: record.EventMark{Seq: 4, At: reviewedAt}})
	if root := issue(t, pool, spParent); root.Phase != phase.Admitted || holdsSlot(t, pool, spParent) {
		t.Fatalf("the root at its gate's opening = phase %s, holding a slot %t; want admitted holding none while %s is open",
			root.Phase, holdsSlot(t, pool, spParent), spChild)
	}
	if want := []record.BlockedIssue{{Issue: spParent, BlockedBy: []string{spChild}}}; !reflect.DeepEqual(blockedNow(t, pool, admission), want) {
		t.Fatalf("blocked = %#v, want %#v", blockedNow(t, pool, admission), want)
	}
}

// The re-admission's own clear of the held start, read where it is written (readmit's non-orphan
// branch): a root whose record still stands in awaiting_children when its todo re-admits it — a
// row no departure of this release leaves, since the leave writes the phase done and takes the
// held start with it — starts its new generation at admitted with nothing held, rather than
// failing the record's shape, which refuses a held start beside any other phase.
func TestAReadmissionClearsTheHeldStart(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 2)
	until := fixedNow.Add(time.Hour)
	spWaiting(t, pool, spParent, phase.Testing, "Continue LEGION-300. Issue: LEGION-300. Phase: testing.", acHeldReason, func(issue *record.Issue) {
		issue.Status, issue.LingerUntil, issue.LastDispatchSeq = "backlog", &until, 1
	})

	apply(t, pool, admission, "readmitted", intake.DispatchIssue{Key: spParent, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated",
		Status: "todo", Title: spParent, Rank: spParent, HandedOver: true}, engineStub{})

	got := issue(t, pool, spParent)
	if got.Generation != 2 || got.Phase != phase.Admitted || got.ChildrenWait != nil {
		t.Fatalf("the re-admitted root = generation %d, phase %s, held start %+v; want generation 2 at admitted holding nothing",
			got.Generation, got.Phase, got.ChildrenWait)
	}
}

// A daemon restart between the move into awaiting_children and the return changes nothing
// (dispatch://INFRA-3320 R13): the held start is the issue row's own (record.Issue.ChildrenWait),
// not anything the daemon holds in memory. An admission and an engine built anew over the same
// database, as a restart builds them, read the parent back exactly as the move wrote it, and the
// first pass after the child is done takes it back to the start it held with the task it held.
func TestARestartBetweenTheMoveAndTheReturnChangesNothing(t *testing.T) {
	pool := migratedPool(t)
	given := "Continue LEGION-300. Issue: LEGION-300. Phase: implementing. Reason: a review asked for changes."
	spSeedParent(t, pool, spParent, phase.Implementing, true, nil)
	seedPullRequest(t, pool, spParent, 42, record.PullRequestOpen)
	spSeedPhase(t, pool, record.PhaseRow{Issue: spParent, Role: claim.RoleImplementer, Claim: "implementer", Entry: 3,
		EntryTask: given, EntryRaisedAt: fixedNow.Add(-time.Hour)})
	seedMirrorChild(t, pool, spChild, "backlog", spParent)

	spPass(t, pool, quiet(t, 2))
	moved := issue(t, pool, spParent)
	if moved.Phase != phase.AwaitingChildren || moved.ChildrenWait == nil || moved.ChildrenWait.Task != given {
		t.Fatalf("the parent after the move = phase %s, held start %+v; want awaiting_children holding implementing on %q", moved.Phase, moved.ChildrenWait, given)
	}

	// The restart: nothing of the first admission or its engine is used again, and the record is
	// all the restarted one reads.
	restarted := quiet(t, 2)
	spPass(t, pool, restarted)
	if again := issue(t, pool, spParent); again.Phase != phase.AwaitingChildren || !sameHeldStart(again.ChildrenWait, moved.ChildrenWait) ||
		again.ChildrenWaitedAt == nil || !again.ChildrenWaitedAt.Equal(*moved.ChildrenWaitedAt) {
		t.Fatalf("the parent after the restarted daemon's first pass = %+v, want it waiting still as the move wrote it, %+v", again, moved)
	}

	spMirror(t, pool, spChild, "done", spParent, 2)
	spPass(t, pool, restarted)
	back := issue(t, pool, spParent)
	if back.Phase != phase.Implementing || back.ChildrenWait != nil {
		t.Fatalf("the parent after its child is done = phase %s, held start %+v; want implementing with nothing held", back.Phase, back.ChildrenWait)
	}
	starts := startsOf(t, pool, spParent)
	if len(starts) != 1 || starts[0].role != claim.RoleImplementer || !strings.HasPrefix(starts[0].task, given) {
		t.Fatalf("starts of %s = %+v, want the implementer's on the task it held", spParent, starts)
	}
}

// sameHeldStart says whether a and b are one held start, its moment compared as an instant.
func sameHeldStart(a, b *record.ChildrenWait) bool {
	return a != nil && b != nil && a.Start == b.Start && a.Task == b.Task && a.Reason == b.Reason && a.Since.Equal(b.Since)
}
