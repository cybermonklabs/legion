package workflow

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// atParts is the place of a task with a sub-owner while its sub-owner acts: its parts.
var atParts = record.TaskPlace{Kind: record.PlaceParts}

// ownedTask is one task of a plan these tests write through the store (putOwnedVersion): its id,
// the role of its one step ("" for none), the role its block names as `owner` (a task with a
// sub-owner, which has no steps), the task it is a part of, the tasks it needs, and where it
// stands.
type ownedTask struct {
	id, role, owner, partOf string
	needs                   []string
	state                   record.TaskState
	place                   record.TaskPlace
}

// subOwned is task id with a sub-owner of the role architect, needing needs, waiting at no act.
func subOwned(id string, needs ...string) ownedTask {
	return ownedTask{id: id, owner: "architect", needs: needs, state: record.TaskWaiting}
}

// stepped is task id with one step of role, needing needs, waiting at no act.
func stepped(id, role string, needs ...string) ownedTask {
	return ownedTask{id: id, role: role, needs: needs, state: record.TaskWaiting}
}

// part is task id, a part of task of, with one implementer's step and no needs, waiting at no act.
func part(id, of string) ownedTask {
	t := stepped(id, "implementer")
	t.partOf = of
	return t
}

// at is t standing in state at place.
func (t ownedTask) at(state record.TaskState, place record.TaskPlace) ownedTask {
	t.state, t.place = state, place
	return t
}

// putOwnedVersion writes version of the plan of key at generation, given by givenBy, holding tasks,
// in tx (record.Store.PutPlan): each task's block is "Do <id>.", and its one step, where it has
// one, is `<role>:<id>`, of the type its role has among Legion's own roles, done by the agent
// `<role>-<id>`: each step of these plans has an agent of its own.
func putOwnedVersion(ctx context.Context, tx pgx.Tx, key string, generation uint64, version int, givenBy claim.Token, tasks ...ownedTask) error {
	v := record.PlanVersion{Issue: key, Generation: generation, Version: version, ArtifactID: "plan-artifact", Source: record.PlanSourceOwner,
		GivenBy: givenBy, GivenAt: stepClock}
	roles := plan.BuiltinRoles()
	var rows []record.PlanTask
	var steps []record.PlanStep
	for _, task := range tasks {
		rows = append(rows, record.PlanTask{Issue: key, Generation: generation, Task: task.id, PlanVersion: version, BlockText: "Do " + task.id + ".",
			Needs: task.needs, PartOf: task.partOf, OwnerRole: task.owner, State: task.state, Place: task.place})
		if task.role != "" {
			steps = append(steps, record.PlanStep{Issue: key, Generation: generation, Task: task.id, Position: 1, Role: task.role, AgentKey: task.id,
				Type: roles[task.role].Type, Agent: claim.Agent(task.role + "-" + task.id)})
		}
	}
	return record.NewStore().PutPlan(ctx, tx, v, rows, steps)
}

// seedOwnedVersion writes version 1 of the plan of key at generation, given by its tree's
// architect, holding tasks (putOwnedVersion).
func seedOwnedVersion(t *testing.T, pool *pgxpool.Pool, key string, generation uint64, tasks ...ownedTask) {
	t.Helper()
	architect := claim.Token(treeRoleToken(t, key, claim.RoleArchitect))
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return putOwnedVersion(t.Context(), tx, key, generation, 1, architect, tasks...)
	})
}

// partsGiven takes version of the plan of key, holding tasks, given by the sub-owner of task of, as
// a later version is taken once the work has started (afterSeveral), in one transaction: the
// version's rows (putOwnedVersion), the plan node (WritePlan), then each task that can start
// (startReady).
func partsGiven(t *testing.T, pool *pgxpool.Pool, engine *Engine, key, of string, version int, tasks ...ownedTask) {
	t.Helper()
	subOwner := agentClaim(t, key, plan.SubOwnerAgent(of))
	onStored(t, pool, engine, key, func(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
		if err := putOwnedVersion(ctx, tx, key, issue.Generation, version, subOwner, tasks...); err != nil {
			return err
		}
		if err := engine.WritePlan(ctx, tx, issue); err != nil {
			return err
		}
		_, err := engine.startReady(ctx, tx, issue)
		return err
	})
}

// subOwnerOf is the running step of the sub-owner of task at its parts (record.RunningStep): the act
// of the agent plan.SubOwnerAgent names, in planning.
func subOwnerOf(task string) record.RunningStep {
	return record.RunningStep{Task: task, Place: atParts, Role: claim.RoleSubOwner, Phase: phase.Planning, Agent: plan.SubOwnerAgent(task)}
}

// A task with a sub-owner starts at its parts, where its sub-owner acts, and its parts start while
// it stands there. In the plan f (owner=architect), h (needs f, one implementer's step), the start
// of the work puts f running at its parts and leaves h waiting. The issue stands in planning, and
// its one running step is f's sub-owner's act, sub-owner-f in planning. A later version of the
// sub-owner that adds the parts f1 and f2 starts both in its transaction at their first steps,
// while f stays at its parts and the issue in planning, the phase that comes first in today's
// order. A part of a task that waits does not start; a part the plan holds when its task starts
// starts in the same call; and a plan of f alone puts f at its parts at the gate's opening.
//
// The fault it catches is a task with a sub-owner that stands at a step it does not have, or a
// part that starts while its task does not run (its controls: startReady with no test of a part's
// task starts f1 while f waits, and the second case fails at "f1 waiting"; startReady that reads
// the plan once leaves f1 waiting in the third case; the table of f's line with its planning at
// step 1 refuses the gate's move of f in the fourth case).
func TestATaskWithASubOwnerStandsAtItsParts(t *testing.T) {
	t.Run("the start of the work puts f at its parts, and a later version starts its parts", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)
		seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
		seedOwnedVersion(t, pool, root.Key, root.Generation, subOwned("f"), stepped("h", "implementer", "f"))

		firstStepOf(t, pool, engine, root.Key)

		tasks := tasksOf(t, pool, engine, root.Key)
		assertWaits(t, tasks, "f", atParts, record.StepWaitNone)
		assertTask(t, tasks, "h", record.TaskWaiting, record.TaskPlace{})
		issue := issueOf(t, pool, engine, root.Key)
		if issue.Phase != phase.Planning || issue.Hold != nil {
			t.Fatalf("%s at the start of its work stands in %s (hold %+v), want planning, where f's sub-owner acts", root.Key, issue.Phase, issue.Hold)
		}
		want := []record.RunningStep{subOwnerOf("f")}
		if got := issue.Running; !slices.Equal(got, want) || !got[0].Place.SubOwnerActs() {
			t.Fatalf("the running steps of %s = %+v, want %+v, an owner's act", root.Key, got, want)
		}

		partsGiven(t, pool, engine, root.Key, "f", 2, subOwned("f"), stepped("h", "implementer", "f"), part("f1", "f"), part("f2", "f"))

		tasks = tasksOf(t, pool, engine, root.Key)
		assertTask(t, tasks, "f", record.TaskRunning, atParts)
		assertTask(t, tasks, "f1", record.TaskRunning, atStep(1))
		assertTask(t, tasks, "f2", record.TaskRunning, atStep(1))
		assertTask(t, tasks, "h", record.TaskWaiting, record.TaskPlace{})
		if issue := issueOf(t, pool, engine, root.Key); issue.Phase != phase.Planning || issue.Hold != nil {
			t.Fatalf("%s while f1 and f2 implement stands in %s (hold %+v), want planning, which comes first in today's order", root.Key, issue.Phase, issue.Hold)
		}
	})
	t.Run("a part of a task that waits stays waiting", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Implementing)
		seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
		a := stepped("a", "implementer").at(record.TaskRunning, atStep(1))
		seedOwnedVersion(t, pool, root.Key, root.Generation, a, subOwned("f", "a"), stepped("h", "implementer", "f"))

		partsGiven(t, pool, engine, root.Key, "f", 2, a, subOwned("f", "a"), stepped("h", "implementer", "f"), part("f1", "f"), part("f2", "f"))

		tasks := tasksOf(t, pool, engine, root.Key)
		assertTask(t, tasks, "f", record.TaskWaiting, record.TaskPlace{})
		assertTask(t, tasks, "f1", record.TaskWaiting, record.TaskPlace{})
		assertTask(t, tasks, "f2", record.TaskWaiting, record.TaskPlace{})
	})
	t.Run("a part the plan holds when its task starts starts with it", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)
		seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
		seedOwnedVersion(t, pool, root.Key, root.Generation, subOwned("f"), part("f1", "f"), stepped("h", "implementer", "f"))

		firstStepOf(t, pool, engine, root.Key)

		tasks := tasksOf(t, pool, engine, root.Key)
		assertWaits(t, tasks, "f", atParts, record.StepWaitNone)
		assertTask(t, tasks, "f1", record.TaskRunning, atStep(1))
		assertTask(t, tasks, "h", record.TaskWaiting, record.TaskPlace{})
	})
	t.Run("a plan of f alone puts f at its parts at the gate's opening", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)
		seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
		seedOwnedVersion(t, pool, root.Key, root.Generation, subOwned("f"))

		firstStepOf(t, pool, engine, root.Key)

		assertWaits(t, tasksOf(t, pool, engine, root.Key), "f", atParts, record.StepWaitNone)
		issue := issueOf(t, pool, engine, root.Key)
		if want := []record.RunningStep{subOwnerOf("f")}; issue.Phase != phase.Planning || !slices.Equal(issue.Running, want) {
			t.Fatalf("%s at the gate's opening stands in %s with the running steps %+v, want planning with %+v", root.Key, issue.Phase, issue.Running, want)
		}
	})
}

// A part starts only after the join of its task's needs ended, so it starts from the joined work
// of those needs: with a (one implementer's step) done and f (owner=architect) needing it, f starts
// at its parts and waits for its join, and its task_branch row is queued. A version that adds the
// part f1 (no needs) leaves f1 waiting. The end of f's join (intake.TaskJoined) ends f's wait, and
// f1 starts at its first step in that transaction, while f's sub-owner's act is a running step.
//
// The fault it catches is a part that starts without the work of its task's needs (its control:
// startReady that tests only that f runs at its parts starts f1 before the join, and the assertion
// "f1 waiting" fails; taskJoined with no start of f's parts leaves f1 waiting after the join).
func TestAPartStartsOnlyAfterItsTasksJoin(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Implementing)
	seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
	a := stepped("a", "implementer").at(record.TaskDone, atStep(1))
	seedOwnedVersion(t, pool, root.Key, root.Generation, a, subOwned("f", "a"))
	seedTaskBranch(t, pool, root.Key, "a", "legion/"+root.Key)

	onStored(t, pool, engine, root.Key, func(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
		_, err := engine.startReady(ctx, tx, issue)
		return err
	})

	assertWaits(t, tasksOf(t, pool, engine, root.Key), "f", atParts, record.StepWaitJoin)
	if rows := taskBranchRowsOf(t, pool, root.Key); !slices.Equal(rows, []string{"f"}) {
		t.Fatalf("the task_branch rows of %s name %v, want f's one", root.Key, rows)
	}

	partsGiven(t, pool, engine, root.Key, "f", 2, a, subOwned("f", "a"), part("f1", "f"))

	assertTask(t, tasksOf(t, pool, engine, root.Key), "f1", record.TaskWaiting, record.TaskPlace{})

	joinEnds(t, pool, engine, root.Key, "f")

	tasks := tasksOf(t, pool, engine, root.Key)
	assertWaits(t, tasks, "f", atParts, record.StepWaitNone)
	assertTask(t, tasks, "f1", record.TaskRunning, atStep(1))
	if running := issueOf(t, pool, engine, root.Key).Running; !slices.Contains(running, subOwnerOf("f")) {
		t.Fatalf("the running steps of %s after f's join = %+v, want f's sub-owner's act among them", root.Key, running)
	}
}

// A part waits while its task is stopped: with f (owner=architect) stopped at its parts while a
// runs, a version that adds the part f1 leaves f1 waiting, and f stopped.
//
// The fault it catches is a part that runs while its task is stopped (its control: startReady with
// no test of a part's task starts f1, and the assertion "f1 waiting" fails).
func TestAPartWaitsWhileItsTaskIsStopped(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Implementing)
	seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
	a := stepped("a", "implementer").at(record.TaskRunning, atStep(1))
	f := subOwned("f").at(record.TaskRunning, atParts)
	seedOwnedVersion(t, pool, root.Key, root.Generation, a, f, stepped("h", "implementer", "a", "f"))
	stopTasks(t, pool, root.Key, root.Generation, "f")

	partsGiven(t, pool, engine, root.Key, "f", 2, a, f, stepped("h", "implementer", "a", "f"), part("f1", "f"))

	tasks := tasksOf(t, pool, engine, root.Key)
	assertTask(t, tasks, "f", record.TaskStopped, atParts)
	assertTask(t, tasks, "f1", record.TaskWaiting, record.TaskPlace{})
}

// The act of a task's sub-owner is told apart from a step by its place alone
// (record.TaskPlace.SubOwnerActs): the parts and the report are its act, and no act, a step, the
// retro and the merge are none. Each reader asks the place: the reading of a row of the running
// steps (record.RunningOf), which names the sub-owner, its agent and planning there, and the phase a
// place derives for a task with a sub-owner (issuePhaseOf), planning.
//
// The fault it catches is a reader that keeps a list of those places of its own (its control: the
// place's test without the report, under which each reader fails at the report, so none of them
// answers from a list of its own).
func TestASubOwnersActIsToldByItsPlace(t *testing.T) {
	const key = "LEGION-208"
	f := walksOf(key, &record.StoredPlan{Version: record.PlanVersion{Issue: key, Generation: 1, Version: 1},
		Tasks: []record.PlanTask{{Issue: key, Generation: 1, Task: "f", PlanVersion: 1, OwnerRole: "architect", State: record.TaskRunning, Place: atParts}},
		Steps: map[string][]record.PlanStep{}})[0]
	for _, tc := range []struct {
		place record.TaskPlace
		acts  bool
	}{
		{record.TaskPlace{}, false},
		{atStep(1), false},
		{record.TaskPlace{Kind: record.PlaceRetro}, false},
		{record.TaskPlace{Kind: record.PlaceMerge}, false},
		{atParts, true},
		{record.TaskPlace{Kind: record.PlaceReported}, true},
	} {
		if got := tc.place.SubOwnerActs(); got != tc.acts {
			t.Errorf("%+v.SubOwnerActs() = %t, want %t", tc.place, got, tc.acts)
		}
		row := pods.RunningRow{Task: "f", Kind: tc.place.Kind, Position: tc.place.Position}
		if tc.place.Kind == record.PlaceStep {
			row.Role, row.Key, row.Agent, row.At = "implementer", "f", "implementer-f", 1
		}
		steps := record.RunningOf(record.Issue{Key: key}, true, []pods.RunningRow{row})
		subOwner := len(steps) == 1 && steps[0].Role == claim.RoleSubOwner && steps[0].Phase == phase.Planning && steps[0].Agent == plan.SubOwnerAgent("f")
		if subOwner != tc.acts {
			t.Errorf("the running steps read from a row of f at %+v = %+v; the sub-owner's act in planning: %t, want %t", tc.place, steps, subOwner, tc.acts)
		}
		if got := issuePhaseOf(f, tc.place, record.TaskRunning); (got == phase.Planning) != tc.acts {
			t.Errorf("issuePhaseOf f running at %+v = %q; planning: %t, want %t", tc.place, got, got == phase.Planning, tc.acts)
		}
	}
}
