package workflow

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// claimPlans are the two plans the planned-start checks run on: one task a, `implementer
// reviewer`, at its implementer, and the same with a second task b of agents of its own.
var claimPlans = []struct {
	name  string
	tasks []planTask
}{
	{"a plan of one task", []planTask{
		{id: "a", steps: []string{"implementer", "reviewer"}, agents: []claim.Agent{"implementer", "reviewer"}, state: record.TaskRunning, place: atStep(1)},
	}},
	{"a plan of two tasks", []planTask{
		{id: "a", steps: []string{"implementer", "reviewer"}, agents: []claim.Agent{"implementer", "reviewer"}, state: record.TaskRunning, place: atStep(1)},
		{id: "b", steps: []string{"implementer", "reviewer"}, agents: []claim.Agent{"implementer-2", "reviewer-2"}, state: record.TaskRunning, place: atStep(1)},
	}},
}

// stopEveryTask stops each task of plan of key by its owner, so no task runs and the root stands
// held for its stopped tasks.
func stopEveryTask(t *testing.T, pool *pgxpool.Pool, engine *Engine, root record.Issue, plan []planTask) {
	t.Helper()
	apply := applyFacts(t, pool, engine)
	for _, task := range plan {
		mustSpend(t, "the owner's stop of "+task.id, apply("stop-"+task.id, ownersStop(t, root, task.id)))
	}
}

// Every start of a step of a run with plan rows names the claim of that step's agent
// (record.SuperviseRequest.Claim, startRequest), whatever the plan's count of tasks: the
// transition's start of the next step, the retry of a held step, the restart of a stopped task at
// its first step, the promotion's start of a mid-phase worker and the send of an owed entry, each
// of task a's step, of a plan of one task and of a plan of two. A start that names no claim
// reaches the claim its role's row names when it runs, and is taken out by no start that names one
// (record.Store.EnqueueStart), so a planned start that named none would stand beside its own
// task's next start.
//
// The fault it catches is a start built on the bare issue, which names no claim: its controls are
// startRequest answering no claim for a planned scope, which fails every row, and one door handed
// the run's no-plan scope (noPlanScope), which fails that door; at the parent, where only a copy
// read for one task of several named its claim, the rows of the plan of one task fail.
func TestEveryPlannedStartNamesItsStepsClaim(t *testing.T) {
	// Each door acts on the plan's last task, which holds the issue's own branch and its pull
	// request #42, so its start waits for no join; step is the place of the started step's agent.
	doors := []struct {
		name string
		role claim.Role
		at   phase.Phase
		step int
		act  func(t *testing.T, pool *pgxpool.Pool, engine *Engine, root record.Issue, plan []planTask, task planTask)
	}{
		{"the transition", claim.RoleReviewer, phase.Reviewing, 1, func(t *testing.T, pool *pgxpool.Pool, engine *Engine, root record.Issue, _ []planTask, task planTask) {
			taskMoves(t, pool, engine, root.Key, task.id, TriggerImplementationReady, "")
		}},
		{"the retry of a held step", claim.RoleImplementer, phase.Implementing, 0, func(t *testing.T, pool *pgxpool.Pool, engine *Engine, root record.Issue, _ []planTask, task planTask) {
			apply := applyFacts(t, pool, engine)
			mustSpend(t, "the failure of the implementer", apply("implementer-failed", failedClaimOf(t, root.Key, claim.RoleImplementer, task.agents[0])))
			mustSpend(t, "the retry", apply("retry", intake.RetryOrEscalate{Issue: root.Key, Decision: intake.RetryDecision, Task: task.id}))
		}},
		{"the restart at the first step", claim.RoleImplementer, phase.Implementing, 0, func(t *testing.T, pool *pgxpool.Pool, engine *Engine, root record.Issue, plan []planTask, _ planTask) {
			stopEveryTask(t, pool, engine, root, plan)
			mustSpend(t, "the todo", applyFacts(t, pool, engine)("todo", personsTodo(root.Key)))
		}},
		{"the promoted start", claim.RoleImplementer, phase.Implementing, 0, func(t *testing.T, pool *pgxpool.Pool, engine *Engine, root record.Issue, _ []planTask, task planTask) {
			onTask(t, pool, engine, root.Key, task.id, func(ctx context.Context, tx pgx.Tx, s TaskScope) error {
				_, err := engine.StartPromotedWorker(ctx, tx, s, claim.RoleImplementer, false)
				return err
			})
		}},
		{"the owed entry", claim.RoleImplementer, phase.Implementing, 0, func(t *testing.T, pool *pgxpool.Pool, engine *Engine, root record.Issue, _ []planTask, task planTask) {
			onTask(t, pool, engine, root.Key, task.id, func(ctx context.Context, tx pgx.Tx, s TaskScope) error {
				row, err := engine.phaseRow(ctx, tx, root.Key, claim.RoleImplementer)
				if err != nil {
					return err
				}
				return engine.SendOwedEntry(ctx, tx, s, claim.RoleImplementer, row)
			})
		}},
	}
	for _, plan := range claimPlans {
		for _, door := range doors {
			t.Run(plan.name+"/"+door.name, func(t *testing.T) {
				pool := migratedPool(t)
				engine := returnEngine()
				root := stepRoot(t, pool, phase.Implementing)
				seedTasks(t, pool, root.Key, root.Generation, plan.tasks...)
				task := plan.tasks[len(plan.tasks)-1]
				for _, other := range plan.tasks[:len(plan.tasks)-1] {
					if _, err := pool.Exec(t.Context(), `update plan_tasks set branch = $1 where issue = $2 and task = $3`,
						"legion/"+root.Key+"-"+other.id, root.Key, other.id); err != nil {
						t.Fatalf("give task %s a branch of its own: %v", other.id, err)
					}
				}

				door.act(t, pool, engine, root, plan.tasks, task)

				want := agentClaim(t, root.Key, task.agents[door.step])
				starts := startsIn(t, pool, root.Key, door.role, door.at)
				if !slices.ContainsFunc(starts, func(s record.SuperviseRequest) bool { return s.Claim == want }) {
					t.Fatalf("the %s's starts of %s in %s = %+v, want %s's naming %s", door.role, root.Key, door.at, starts, door.name, want)
				}
				if slices.ContainsFunc(starts, func(s record.SuperviseRequest) bool { return s.Claim == "" }) {
					t.Fatalf("the %s's starts of %s in %s = %+v, want none that names no claim", door.role, root.Key, door.at, starts)
				}
			})
		}
	}
}

// A parent whose plan of one task has no acceptance step is held before its retro for the issues
// beneath it on its task too, as a task of a plan of several is (arrive's record.Store.HoldTask):
// of LEGION-208's one task a, `implementer reviewer`, a's approved review passes its last step while
// LEGION-209 is open beneath it, so a is held at its retro for the issues beneath, and the issue is
// held from retro for them. The child's done, whose settle releases the parent, takes a out of its
// hold to run at its retro, writes the issue in retro and starts a's implementer there once. Before,
// a plan of one task held the issue alone and left its task running, so the task row said running
// while the issue said held.
//
// The fault it catches is a hold written on the issue alone for a plan of one task (its control:
// arrive holding the task only where the plan holds several).
func TestAOneTaskParentHeldForItsChildrenHoldsItsTask(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Reviewing)
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "a", steps: []string{"implementer", "reviewer"}, agents: []claim.Agent{"implementer", "reviewer"}, state: record.TaskRunning, place: atStep(2)})
	stepChild(t, pool, heldParentChild, phase.Implementing, nil)
	retro := record.TaskPlace{Kind: record.PlaceRetro}
	taskMoves(t, pool, engine, root.Key, "a", TriggerReviewApproved, "")
	task := tasksOf(t, pool, engine, root.Key)["a"]
	if task.State != record.TaskHeld || task.Place != retro || task.HoldReason != record.HoldForChildren {
		t.Fatalf("task a of %s after its approval = %s at %+v (hold %q), want held at its retro for its children", root.Key, task.State, task.Place, task.HoldReason)
	}
	if held := issueOf(t, pool, engine, root.Key); held.Phase != phase.Held || held.Hold == nil ||
		*held.Hold != (record.Hold{From: phase.Retro, Reason: record.HoldForChildren}) {
		t.Fatalf("%s after a's approval stands in %s with hold %+v, want held from retro for its children", root.Key, held.Phase, held.Hold)
	}

	childLeavesDone(t, pool, engine)

	assertTask(t, tasksOf(t, pool, engine, root.Key), "a", record.TaskRunning, retro)
	if issue := issueOf(t, pool, engine, root.Key); issue.Phase != phase.Retro || issue.Hold != nil {
		t.Fatalf("%s after its child's leave stands in %s with hold %+v, want retro with no hold", root.Key, issue.Phase, issue.Hold)
	}
	if starts := startsIn(t, pool, root.Key, claim.RoleImplementer, phase.Retro); len(starts) != 1 {
		t.Fatalf("the starts of the implementer of %s in retro = %+v, want the one start of a's retro", root.Key, starts)
	}
}
