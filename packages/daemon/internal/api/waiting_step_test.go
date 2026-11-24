package api

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// seedAWaitsForTheTester records key, at generation 1, in testing under an owner's plan where the
// agent tester runs the step of one task and the step of another waits for it: task a, `tester`,
// which needs x and comes before m in task id order, stands at its step, which waits for the agent
// tester and never started; task m, `implementer tester`, stands at its tester's step, which the
// agent tester runs; x, `implementer`, is done; and z, `reviewer`, needs a and m and waits.
func seedAWaitsForTheTester(t *testing.T, h *harness, key string) {
	t.Helper()
	seedIssueAt(t, h, key, phase.Testing)
	ctx, records := context.Background(), record.NewStore()
	v := record.PlanVersion{Issue: key, Generation: 1, Version: 1, ArtifactID: "plan-" + key, Source: record.PlanSourceOwner,
		GivenBy: "owner", GivenAt: time.Date(2026, 11, 17, 6, 0, 0, 0, time.UTC)}
	task := func(id string, needs ...string) record.PlanTask {
		return record.PlanTask{Issue: key, Generation: 1, Task: id, PlanVersion: 1, BlockText: "Task " + id + ".", Needs: needs, State: record.TaskWaiting}
	}
	step := func(task string, position int, role claim.Role, kind plan.Type, agent claim.Agent) record.PlanStep {
		return record.PlanStep{Issue: key, Generation: 1, Task: task, Position: position, Role: string(role), Type: kind, Agent: agent}
	}
	tasks := []record.PlanTask{task("a", "x"), task("m"), task("x"), task("z", "a", "m")}
	steps := []record.PlanStep{
		step("a", 1, claim.RoleTester, "testing", "tester"),
		step("m", 1, claim.RoleImplementer, "implementation", "implementer"),
		step("m", 2, claim.RoleTester, "testing", "tester"),
		step("x", 1, claim.RoleImplementer, "implementation", "implementer-2"),
		step("z", 1, claim.RoleReviewer, "review", "reviewer"),
	}
	since := time.Date(2026, 11, 17, 7, 0, 0, 0, time.UTC)
	if err := store.Transact(ctx, h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		if err := records.PutPlan(ctx, tx, v, tasks, steps); err != nil {
			return err
		}
		for _, at := range []struct {
			task  string
			place int
			state record.TaskState
		}{{"x", 1, record.TaskDone}, {"m", 2, record.TaskRunning}, {"a", 1, record.TaskRunning}} {
			if err := records.MoveTask(ctx, tx, key, 1, at.task, record.TaskPlace{Kind: record.PlaceStep, Position: at.place}, at.state); err != nil {
				return err
			}
		}
		// The move writes the wait of a start for its join; this seed stands for a task m whose join ended.
		if err := records.SetStepWait(ctx, tx, key, 1, "m", record.StepWaitNone, nil); err != nil {
			return err
		}
		return records.SetStepWait(ctx, tx, key, 1, "a", record.StepWaitAgent, &since)
	}); err != nil {
		t.Fatalf("seed the plan of %s: %v", key, err)
	}
}

// plannedTasks is each task of the plan key runs at generation 1, by its id.
func plannedTasks(t *testing.T, h *harness, key string) map[string]record.PlanTask {
	t.Helper()
	ctx := context.Background()
	tasks := map[string]record.PlanTask{}
	if err := store.Transact(ctx, h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		stored, err := record.NewStore().Plan(ctx, tx, key, 1)
		if err != nil || stored == nil {
			return err
		}
		for _, row := range stored.Tasks {
			tasks[row.Task] = row
		}
		return nil
	}); err != nil {
		t.Fatalf("read the plan of %s: %v", key, err)
	}
	return tasks
}

// assertStands fails unless task of tasks is in state at its step position.
func assertStands(t *testing.T, tasks map[string]record.PlanTask, task string, state record.TaskState, position int) {
	t.Helper()
	if got := tasks[task]; got.State != state || got.Place != (record.TaskPlace{Kind: record.PlaceStep, Position: position}) {
		t.Fatalf("task %s is %s at %+v, want %s at step %d", task, got.State, got.Place, state, position)
	}
}

// A step that waits for its agent is no running step, so each call of that agent's worker is of
// the step the agent runs, as the route reads it from the claim, and never of the step that waits:
// task a's step waits for the agent tester, which runs m's tester's step, and a comes before m in
// task id order. The tester's move back to implementing returns m to its implementer's step and
// leaves a where it waits; the tester's pass makes m done, and a is not done.
//
// The fault it catches is a worker's call taken as the work of a step whose start waits for its
// agent (its control: the issue's read of its running steps without the filter of the wait, so the
// route reads the tester's step as a's: the move back is refused, a's task having no implementer's
// step, and the pass makes a done, whose step never started).
func TestAStepThatWaitsIsNoRunningStepOfItsAgentsCalls(t *testing.T) {
	const key = "LEGION-212"
	t.Run("the move back", func(t *testing.T) {
		h := newWorkflowHandoffHarness(t, newHandoffGitHub(t), io.Discard)
		seedAWaitsForTheTester(t, h, key)
		tester := newLiveClaim(t, h, key, claim.RoleTester)

		moved := h.request(http.MethodPost, "/legion/v1/phase/backward", PhaseBackwardRequest{GrantID: tester.grant(t), To: phase.Implementing,
			Reason: "the suite needs a fix"}, nil)
		if moved.Code != http.StatusOK {
			t.Fatalf("the tester's move back = %d: %s, want 200", moved.Code, moved.Body)
		}
		tasks := plannedTasks(t, h, key)
		assertStands(t, tasks, "m", record.TaskRunning, 1)
		assertStands(t, tasks, "a", record.TaskRunning, 1)
	})

	t.Run("the completion", func(t *testing.T) {
		h := newWorkflowHandoffHarness(t, newHandoffGitHub(t), io.Discard)
		seedAWaitsForTheTester(t, h, key)
		tester := newLiveClaim(t, h, key, claim.RoleTester)

		completion(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass",
			HandoffID: tester.writeHandoff("test", 1), HandoffVersion: 1}))
		tasks := plannedTasks(t, h, key)
		assertStands(t, tasks, "m", record.TaskDone, 2)
		assertStands(t, tasks, "a", record.TaskRunning, 1)
	})
}
