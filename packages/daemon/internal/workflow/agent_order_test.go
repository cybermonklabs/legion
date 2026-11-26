package workflow

import (
	"fmt"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A held retro is released only where its task is the last to finish (lastToFinish, retroHeld):
// each other task of its plan is done or left, so no act of another task can hold the retro's
// agent then, and no step of another task waits for it. The wait of a released retro for its agent
// has no case the record can hold, and no test here writes one.

// agentActs is each act whose start key's step records name under the claim of agent, in the order
// they were queued, as `<task> <kind> <place>`: what the agent was handed, one act after another.
func agentActs(t *testing.T, pool *pgxpool.Pool, key string, agent claim.Agent) []string {
	t.Helper()
	token := string(agentClaim(t, key, agent))
	var acts []string
	for _, write := range stepWrites(t, pool, key) {
		if write.Agent == token && write.StartedAt != nil && write.EndedAt == nil {
			acts = append(acts, fmt.Sprintf("%s %s %d", write.Task, write.Kind, write.Step))
		}
	}
	return acts
}

// assertActs fails unless agent's acts on key are want, in that order.
func assertActs(t *testing.T, pool *pgxpool.Pool, key string, agent claim.Agent, want ...string) {
	t.Helper()
	if got := agentActs(t, pool, key, agent); !slices.Equal(got, want) {
		t.Fatalf("the acts agent %s of %s was handed = %q, want %q", agent, key, got, want)
	}
}

// xAndYOnK is LEGION-208 with tasks x and y, no needs, each merging, whose steps are xSteps and
// `implementer:k`, at its first step: x's first step runs on agent k, x on the issue's branch with
// its pull request #42, and y's step waits for k.
func xAndYOnK(t *testing.T, xSteps []string, xAgents []claim.Agent) (*pgxpool.Pool, *Engine, record.Issue) {
	t.Helper()
	pool := migratedPool(t)
	engine, _ := tickingEngine()
	root := stepRoot(t, pool, phase.Admitted)
	seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "x", steps: xSteps, agents: xAgents, state: record.TaskWaiting},
		planTask{id: "y", steps: []string{"implementer:k"}, agents: []claim.Agent{"k"}, state: record.TaskWaiting})
	firstStepOf(t, pool, engine, root.Key)
	joinsEnd(t, pool, engine, root.Key)
	tasks := tasksOf(t, pool, engine, root.Key)
	assertWaits(t, tasks, "x", atStep(1), record.StepWaitNone)
	assertWaits(t, tasks, "y", atStep(1), record.StepWaitAgent)
	return pool, engine, root
}

// An agent freed by a task that gets no next act on it goes to the step that waits for it at the
// settle: x is `implementer:k acceptance:owner` and y `implementer:k`, which waits for k. x's step
// passes by its transition into the owner's acceptance, which no agent of the issue works, and at
// that fact's settle y's step starts on k.
//
// The fault it catches is an agent freed by a task that gets no next act, handed to nobody, so the
// act that waits for it waits for ever (its control: the hand-on made before the transition's
// moves, where the store still gives k the step that ends, under which y keeps its wait).
func TestAnAgentFreedByATaskWithNoNextActGoesToItsWaiterAtTheSettle(t *testing.T) {
	pool, engine, root := xAndYOnK(t, []string{"implementer:k", "acceptance:owner"}, []claim.Agent{"k", ""})

	taskMoves(t, pool, engine, root.Key, "x", TriggerImplementationReady, "")

	tasks := tasksOf(t, pool, engine, root.Key)
	assertTask(t, tasks, "x", record.TaskRunning, atStep(2))
	assertWaits(t, tasks, "y", atStep(1), record.StepWaitNone)
	assertActs(t, pool, root.Key, "k", "x step 1", "y step 1")
}

// The retro of x runs before the step of y in the session of k. x and y each have the one step
// `implementer:k` and both merge. x's step starts on k and y's waits for k, with nothing of it
// started. x's step passes by its transition and the fact's settle: the transition moves x into its
// retro and starts it on k before the settle hands k on, so y's step keeps its wait. x's retro
// completes: x stands at its merge, whose merger starts, and at the settle y's step starts on k.
// y's step passes, and its retro starts on k. k's acts, in order: x's step, x's retro, y's step,
// y's retro.
//
// The fault it catches is the free agent handed to y's waiting step before the next act of x, whose
// step on it just ended (its control: start without the exemption of the retro, the rule of the
// longest waiter alone, under which x's retro waits, y's step starts at the settle, and k's acts
// are x's step, then y's step).
func TestTheRetroOfXRunsBeforeTheStepOfYInSessionK(t *testing.T) {
	pool, engine, root := xAndYOnK(t, []string{"implementer:k"}, []claim.Agent{"k"})

	taskMoves(t, pool, engine, root.Key, "x", TriggerImplementationReady, "")
	tasks := tasksOf(t, pool, engine, root.Key)
	assertWaits(t, tasks, "x", atRetro, record.StepWaitNone)
	assertWaits(t, tasks, "y", atStep(1), record.StepWaitAgent)
	assertNothingStarted(t, pool, root.Key, "y", atStep(1))
	assertActs(t, pool, root.Key, "k", "x step 1", "x retro 0")

	taskMoves(t, pool, engine, root.Key, "x", TriggerRetroCompleted, "")
	tasks = tasksOf(t, pool, engine, root.Key)
	assertWaits(t, tasks, "x", atMerge, record.StepWaitNone)
	assertWaits(t, tasks, "y", atStep(1), record.StepWaitNone)
	assertActs(t, pool, root.Key, "merger", "x merge 0")
	assertActs(t, pool, root.Key, "k", "x step 1", "x retro 0", "y step 1")

	seedTaskPR(t, pool, engine, root.Key, "y", 44)
	taskMoves(t, pool, engine, root.Key, "y", TriggerImplementationReady, "")
	assertActs(t, pool, root.Key, "k", "x step 1", "x retro 0", "y step 1", "y retro 0")
}

// A retro waits while its agent runs a step of another task: x is `implementer:k reviewer` and y
// `implementer:k`, both merging. x's implementer passes, so y's step starts on k at the settle, and
// x's review passes while k runs y's step: x stands at its retro, running, waiting for k, with no
// start of its retro. y's step passes: y's retro, the retro of the task whose step on k ended,
// starts on k first, ahead of x's retro that waited longer. y's retro completes: k is free at the
// settle, and x's retro starts.
//
// The fault it catches is a retro started on a busy agent, so one session gets two tasks at one
// time (its control: start's wait for an agent at the step places alone, under which x's retro
// starts while y's step runs).
func TestARetroWaitsWhileItsAgentRunsAStepOfAnotherTask(t *testing.T) {
	pool, engine, root := xAndYOnK(t, []string{"implementer:k", "reviewer"}, []claim.Agent{"k", "reviewer"})

	taskMoves(t, pool, engine, root.Key, "x", TriggerImplementationReady, "")
	assertWaits(t, tasksOf(t, pool, engine, root.Key), "y", atStep(1), record.StepWaitNone)
	taskMoves(t, pool, engine, root.Key, "x", TriggerReviewApproved, "")
	assertWaits(t, tasksOf(t, pool, engine, root.Key), "x", atRetro, record.StepWaitAgent)
	if starts := actStarts(t, pool, root.Key, "x", record.PlaceRetro); starts != 0 {
		t.Fatalf("the retro of x counts %d starts, want none: its agent k runs y's step", starts)
	}
	assertActs(t, pool, root.Key, "k", "x step 1", "y step 1")

	seedTaskPR(t, pool, engine, root.Key, "y", 44)
	taskMoves(t, pool, engine, root.Key, "y", TriggerImplementationReady, "")
	assertWaits(t, tasksOf(t, pool, engine, root.Key), "x", atRetro, record.StepWaitAgent)
	assertActs(t, pool, root.Key, "k", "x step 1", "y step 1", "y retro 0")

	taskMoves(t, pool, engine, root.Key, "y", TriggerRetroCompleted, "")
	assertWaits(t, tasksOf(t, pool, engine, root.Key), "x", atRetro, record.StepWaitNone)
	assertActs(t, pool, root.Key, "k", "x step 1", "y step 1", "y retro 0", "x retro 0")
}

// Two merges of one issue are one at a time: the merger is one agent of the issue. x and y stand at
// their retro, each on an agent of its own, x on the issue's branch with #42 and y on its own with
// #45. Both retros pass: the merger starts for x, in task id order, and y stands at its merge
// waiting for the merger, with no start. The merger's move back to implementing moves x, the task
// whose merge it runs, and never y. In a second run, x's pull request merges: x is done, and the
// merger's start for y follows at the settle.
//
// The fault it catches is two merges at one time in the merger's one session (its control: the
// merge with no agent to wait for, the revision before, under which y's merger starts beside x's).
func TestTwoMergesOfOneIssueAreOneAtATime(t *testing.T) {
	bothAtRetro := func(t *testing.T) (*pgxpool.Pool, *Engine, record.Issue) {
		t.Helper()
		pool := migratedPool(t)
		engine, _ := tickingEngine()
		root := stepRoot(t, pool, phase.Retro)
		seedTasks(t, pool, root.Key, root.Generation,
			planTask{id: "x", steps: []string{"implementer:kx"}, agents: []claim.Agent{"kx"}, state: record.TaskRunning, place: atRetro},
			planTask{id: "y", steps: []string{"implementer:ky"}, agents: []claim.Agent{"ky"}, state: record.TaskRunning, place: atRetro})
		seedTaskBranch(t, pool, root.Key, "x", "legion/"+root.Key)
		seedTaskBranch(t, pool, root.Key, "y", "legion/"+root.Key+".y")
		seedTaskPR(t, pool, engine, root.Key, "y", 45)
		taskMoves(t, pool, engine, root.Key, "x", TriggerRetroCompleted, "")
		taskMoves(t, pool, engine, root.Key, "y", TriggerRetroCompleted, "")
		tasks := tasksOf(t, pool, engine, root.Key)
		assertWaits(t, tasks, "x", atMerge, record.StepWaitNone)
		assertWaits(t, tasks, "y", atMerge, record.StepWaitAgent)
		assertActs(t, pool, root.Key, "merger", "x merge 0")
		return pool, engine, root
	}

	t.Run("the merger's move back moves its task", func(t *testing.T) {
		pool, engine, root := bothAtRetro(t)
		mustSpend(t, "the merger's move back", applyFacts(t, pool, engine)("merger-back", intake.BackwardMove{Issue: root.Key, Requester: claim.RoleMerger,
			To: phase.Implementing, Reason: "the merge queue refused the head", By: claim.Token(treeRoleToken(t, root.Key, claim.RoleMerger))}))
		tasks := tasksOf(t, pool, engine, root.Key)
		assertTask(t, tasks, "x", record.TaskRunning, atStep(1))
		assertTask(t, tasks, "y", record.TaskRunning, atMerge)
	})

	t.Run("the first merge lands, and the second starts", func(t *testing.T) {
		pool, engine, root := bothAtRetro(t)
		mustSpend(t, "the merge of x's pull request", applyFacts(t, pool, engine)("merged-x",
			intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 42, MergeSHA: "merge-x"}))
		tasks := tasksOf(t, pool, engine, root.Key)
		assertTask(t, tasks, "x", record.TaskDone, atMerge)
		assertWaits(t, tasks, "y", atMerge, record.StepWaitNone)
		assertActs(t, pool, root.Key, "merger", "x merge 0", "y merge 0")
	})
}

// A task whose steps keep returning does not hold its agent from a step that waits for it: x is
// `implementer:k reviewer:k` (merges) and y `implementer:k`, which waits for k since before. x's
// implementer passes: x's review, a step, waits for k behind y, so y's step starts at the settle.
// y's step passes and its retro starts on k; y's retro completes, and x's review starts. x's review
// fails two times, and each return goes back and forth on k; x's last review passes, and its retro
// starts on k.
//
// The fault it catches is a task whose steps keep returning holding its agent from a waiter with no
// bound (its control: the exemption of the retro over any next act of x, under which x's review
// starts on k ahead of y).
func TestAReturningTaskDoesNotHoldItsAgentFromAWaiter(t *testing.T) {
	pool, engine, root := xAndYOnK(t, []string{"implementer:k", "reviewer:k"}, []claim.Agent{"k", "k"})

	taskMoves(t, pool, engine, root.Key, "x", TriggerImplementationReady, "")
	tasks := tasksOf(t, pool, engine, root.Key)
	assertWaits(t, tasks, "x", atStep(2), record.StepWaitAgent)
	assertWaits(t, tasks, "y", atStep(1), record.StepWaitNone)
	assertActs(t, pool, root.Key, "k", "x step 1", "y step 1")

	seedTaskPR(t, pool, engine, root.Key, "y", 44)
	taskMoves(t, pool, engine, root.Key, "y", TriggerImplementationReady, "")
	taskMoves(t, pool, engine, root.Key, "y", TriggerRetroCompleted, "")
	assertWaits(t, tasksOf(t, pool, engine, root.Key), "x", atStep(2), record.StepWaitNone)
	assertActs(t, pool, root.Key, "k", "x step 1", "y step 1", "y retro 0", "x step 2")

	for range 2 {
		taskMoves(t, pool, engine, root.Key, "x", TriggerReviewRejected, changesBody)
		taskMoves(t, pool, engine, root.Key, "x", TriggerImplementationReady, "")
	}
	taskMoves(t, pool, engine, root.Key, "x", TriggerReviewApproved, "")
	assertActs(t, pool, root.Key, "k", "x step 1", "y step 1", "y retro 0", "x step 2", "x step 1", "x step 2", "x step 1", "x step 2", "x retro 0")
}
