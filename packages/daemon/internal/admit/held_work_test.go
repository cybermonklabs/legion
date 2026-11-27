package admit

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// heldBeneath is the open issue beneath the parent holdForChildren holds for its children.
const heldBeneath = "LEGION-10"

// ownersArchitect is the architect of key's own claim, the owner a plan these tests give names.
func ownersArchitect(t *testing.T, key string) claim.Token {
	t.Helper()
	project, err := claim.ProjectToken(testProject)
	if err != nil {
		t.Fatal(err)
	}
	architect, err := claim.NewToken(project, key, claim.RoleArchitect)
	if err != nil {
		t.Fatal(err)
	}
	return architect
}

// planByOwner writes key's plan `implementer tester` at its generation, with task a running at
// place, as the plan's walk leaves it there.
func planByOwner(t *testing.T, pool *pgxpool.Pool, key string, place record.TaskPlace) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		ctx, records := context.Background(), record.NewStore()
		stored, err := records.Issue(ctx, tx, key)
		if err != nil || stored == nil {
			t.Fatalf("read %s: %v", key, err)
		}
		v := record.PlanVersion{Issue: key, Generation: stored.Generation, Version: 2, ArtifactID: "plan-artifact", Source: record.PlanSourceOwner,
			GivenBy: ownersArchitect(t, key), GivenAt: fixedNow}
		task := record.PlanTask{Issue: key, Generation: stored.Generation, Task: "a", PlanVersion: 2, BlockText: "Build the widget.", State: record.TaskWaiting}
		steps := []record.PlanStep{
			{Issue: key, Generation: stored.Generation, Task: "a", Position: 1, Role: "implementer", Type: "implementation"},
			{Issue: key, Generation: stored.Generation, Task: "a", Position: 2, Role: "tester", Type: "testing"},
		}
		for _, put := range []error{
			records.PutPlan(ctx, tx, v, []record.PlanTask{task}, steps),
			records.MoveTask(ctx, tx, key, stored.Generation, "a", place, record.TaskRunning),
		} {
			if put != nil {
				t.Fatalf("seed the plan of %s: %v", key, put)
			}
		}
	})
}

// stopByOwner is the owner's stop_task of task a of key, the fact the route applies: the task's
// running step ends stopped, and key is held for its stopped task.
func stopByOwner(t *testing.T, pool *pgxpool.Pool, admission *Admission, engine intake.Handler, key string) {
	t.Helper()
	stop := intake.TaskStopped{Issue: key, Task: "a", By: ownersArchitect(t, finishedRoot), Generation: issue(t, pool, key).Generation}
	result, err := intake.ApplyFact(context.Background(), pool, "api", "stop-"+key, stop, engine, admission)
	if err != nil || result.Refusal != nil {
		t.Fatalf("the owner's stop of task a of %s = %+v, %v; want it taken", key, result.Refusal, err)
	}
}

// holdForChildren holds key, whose plan holds task a at its retro, for the open issue heldBeneath
// in implementing beneath it, as the transition does only while a hold on its acceptance holds it
// (workflow's retroHold, through retroHeld), and the last one's leave starts the retro.
func holdForChildren(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		ctx, records := context.Background(), record.NewStore()
		held, err := records.Issue(ctx, tx, key)
		if err != nil || held == nil {
			t.Fatalf("read %s: %v", key, err)
		}
		held.Phase, held.Hold = phase.Held, &record.Hold{From: phase.Retro, Reason: record.HoldForChildren}
		parent := key
		for _, put := range []error{
			records.PutIssue(ctx, tx, *held),
			records.PutIssue(ctx, tx, record.Issue{Key: heldBeneath, Repo: held.Repo, DefaultBranch: "main", GitHubRepo: held.GitHubRepo, Tree: held.Tree,
				Project: testProject, Title: heldBeneath, Parent: &parent, Phase: phase.Implementing, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress",
				Rank: "C", LastDispatchSeq: 3}),
		} {
			if put != nil {
				t.Fatalf("seed the hold of %s for %s: %v", key, heldBeneath, put)
			}
		}
	})
}

// workStarts is how many supervise starts of issue carry a task: the work of a phase, which a
// task-less start of a role that has run (startMidPhaseMembers) does not give.
func workStarts(t *testing.T, pool *pgxpool.Pool, issue string) int {
	t.Helper()
	n := 0
	for _, effect := range effects(t, pool) {
		if request, ok := effect.payload.(record.SuperviseRequest); ok && effect.issue == issue && request.Op == "start" && request.Task != "" {
			n++
		}
	}
	return n
}

// The fifth path that resumes work, a re-admission's promotion (startMidPhaseMembers), read
// against the two holds of an owner's plan (workflow's TestAStoppedTaskAndAHeldParentGetNoWork
// reads the other four). A member held in either has no owner, since held has no role
// (phase.RoleFor), so no phase worker of it is started and no start carries it work; a role of it
// that has run comes back with no task, as each held member's does today. Nor does its step start
// again: the stopped task's step, task a's step 2, ended stopped when its owner stopped it, and it
// stays so until a later version of the plan starts the task again, and a parent held for its
// children has started no retro (workflow's StartStepRecord). The twin is the same tree with no
// plan rows: its member held from testing by a failed claim and its member in retro each start
// their step again, and the one in retro gets its implementer's phase work, as today.
func TestAStoppedTaskAndAHeldParentGetNoWorkFromAPromotion(t *testing.T) {
	t.Run("a stopped task and a parent held for its children", func(t *testing.T) {
		pool, admission, engine := seedFinishingTree(t)
		planByOwner(t, pool, heldTesting, record.TaskPlace{Kind: record.PlaceStep, Position: 2})
		countStart(t, pool, engine, heldTesting)
		stopByOwner(t, pool, admission, engine, heldTesting)
		planByOwner(t, pool, inRetro, record.TaskPlace{Kind: record.PlaceRetro})
		holdForChildren(t, pool, inRetro)
		closeFinishingTree(t, pool, admission, engine)
		reopenFinishingTree(t, pool, admission, engine)

		if root := issue(t, pool, finishedRoot); root.Generation != 2 || root.Lingers() {
			t.Fatalf("the reopened root = %#v, want generation 2, no longer lingering: the promotion this case reads did not run", root)
		}
		assertStepWrites(t, pool, heldTesting, start("step", 2, 1, 1).of("a"), end("stopped", "step", 2, 1, 1).of("a"))
		assertStepWrites(t, pool, inRetro)
		for _, held := range []struct {
			key    string
			from   phase.Phase
			reason record.HoldReason
		}{{heldTesting, phase.Testing, record.HoldTaskStopped}, {inRetro, phase.Retro, record.HoldForChildren}} {
			if n := workStarts(t, pool, held.key); n != 0 {
				t.Errorf("the promotion wrote %d starts carrying work for %s, want none", n, held.key)
			}
			if got := issue(t, pool, held.key); got.Phase != phase.Held || got.Hold == nil || *got.Hold != (record.Hold{From: held.from, Reason: held.reason}) {
				t.Errorf("%s after the promotion = %s with hold %+v, want held from %s for %s still", held.key, got.Phase, got.Hold, held.from, held.reason)
			}
		}
	})

	t.Run("the same tree with no plan rows, as today", func(t *testing.T) {
		pool, admission, engine := seedFinishingTree(t)
		countStart(t, pool, engine, heldTesting)
		countStart(t, pool, engine, inRetro)
		closeFinishingTree(t, pool, admission, engine)
		reopenFinishingTree(t, pool, admission, engine)

		assertStepWrites(t, pool, heldTesting, start("step", 3, 1, 1), end("stopped", "step", 3, 1, 1), start("step", 3, 1, 2))
		assertStepWrites(t, pool, inRetro, start("retro", 0, 1, 1), end("stopped", "retro", 0, 1, 1), start("retro", 0, 1, 2))
		if n := phaseStarts(t, pool, inRetro, claim.RoleImplementer, phase.Retro); n != 1 {
			t.Fatalf("the promotion started %s's implementer in retro %d times, want once", inRetro, n)
		}
	})
}
