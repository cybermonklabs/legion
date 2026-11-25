package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// planTask is one task of a plan these tests write through the store: its id, each of its steps
// as `role` or `role:key` with the name of the step's agent beside it, the tasks it needs, and
// where it stands. The agents are named as given: this release names one agent of each role of an
// issue (record.NameAgents), and a plan of several tasks of one role needs one agent for each.
type planTask struct {
	id     string
	steps  []string
	agents []claim.Agent
	needs  []string
	state  record.TaskState
	place  record.TaskPlace
}

// seedTasks writes version 1 of the owner's plan of key at generation, holding tasks: each task's
// block is "Do <id>.", and each step takes the type its role has among Legion's own roles.
func seedTasks(t *testing.T, pool *pgxpool.Pool, key string, generation uint64, tasks ...planTask) {
	t.Helper()
	v := record.PlanVersion{Issue: key, Generation: generation, Version: 1, ArtifactID: "plan-artifact", Source: record.PlanSourceOwner,
		GivenBy: claim.Token(treeRoleToken(t, key, claim.RoleArchitect)), GivenAt: stepClock}
	roles := plan.BuiltinRoles()
	var rows []record.PlanTask
	var steps []record.PlanStep
	for _, task := range tasks {
		rows = append(rows, record.PlanTask{Issue: key, Generation: generation, Task: task.id, PlanVersion: 1, BlockText: "Do " + task.id + ".",
			Needs: task.needs, State: task.state, Place: task.place})
		for i, word := range task.steps {
			role, agentKey, _ := strings.Cut(word, ":")
			steps = append(steps, record.PlanStep{Issue: key, Generation: generation, Task: task.id, Position: i + 1, Role: role, AgentKey: agentKey,
				Type: roles[role].Type, Agent: task.agents[i]})
		}
	}
	seedRecord(t, pool, func(tx pgx.Tx) error { return record.NewStore().PutPlan(t.Context(), tx, v, rows, steps) })
}

// atStep is the place of step n of a task.
func atStep(n int) record.TaskPlace { return record.TaskPlace{Kind: record.PlaceStep, Position: n} }

// onTask runs act in one transaction, as a fact applies it (applyInTx), on the scope of task of
// key (Engine.ScopeOf): the copy of key read for task and the walk of that task.
func onTask(t *testing.T, pool *pgxpool.Pool, engine *Engine, key, task string, act func(ctx context.Context, tx pgx.Tx, s TaskScope) error) {
	t.Helper()
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		issue, err := engine.store.Issue(ctx, tx, key)
		if err != nil {
			return err
		}
		s, err := engine.ScopeOf(ctx, tx, *issue, task)
		if err != nil {
			return err
		}
		return act(ctx, tx, s)
	})
}

// taskMoves runs one transition of task of key on trigger, as a completion of the step task
// stands at moves it: on the scope of task, with the phases row of that step's role and the task's
// own pull request (taskPullRequest on that scope, as the completion reads it), and reason for the
// move's notice and the step its work returns to.
func taskMoves(t *testing.T, pool *pgxpool.Pool, engine *Engine, key, task string, trigger TriggerKind, reason string) {
	t.Helper()
	onTask(t, pool, engine, key, task, func(ctx context.Context, tx pgx.Tx, s TaskScope) error {
		pr, err := engine.taskPullRequest(ctx, tx, s)
		if err != nil {
			return err
		}
		row, err := engine.phaseRow(ctx, tx, key, s.walk.RoleAt(s.walk.Place))
		if err != nil {
			return err
		}
		return engine.transition(ctx, tx, s, trigger, record.TaskPlace{}, row, pr, reason)
	})
}

// seedTaskPR records number open on the branch task of key works on now (record.PlanTask.BranchOf),
// as the agent of the task's implementation step opens its pull request there, and fails the test
// unless the store reads it back as task's own (pullRequestOfTask): a pull request on a branch
// several tasks hold is no task's. A task that holds the issue's own branch has stepRoot's #42.
func seedTaskPR(t *testing.T, pool *pgxpool.Pool, engine *Engine, key, task string, number int) {
	t.Helper()
	branch := tasksOf(t, pool, engine, key)[task].BranchOf(key)
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: key, Repo: "sjawhar/legion", Number: number,
		Branch: branch, HeadSHA: "head-" + task, Failing: []string{}})
	if pr := pullRequestOfTask(t, pool, key, task); pr.Number != number {
		t.Fatalf("the pull request of task %s of %s = #%d, want #%d on %s", task, key, pr.Number, number, branch)
	}
}

// tasksOf is each task of the plan of key at its generation as the store holds it now, by id.
func tasksOf(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string) map[string]record.PlanTask {
	t.Helper()
	issue := issueOf(t, pool, engine, key)
	var stored *record.StoredPlan
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		stored, err = engine.store.Plan(ctx, tx, key, issue.Generation)
		return err
	})
	if stored == nil {
		t.Fatalf("%s has no plan at generation %d", key, issue.Generation)
	}
	tasks := make(map[string]record.PlanTask, len(stored.Tasks))
	for _, task := range stored.Tasks {
		tasks[task.Task] = task
	}
	return tasks
}

// assertTask fails unless task of tasks stands in state at place.
func assertTask(t *testing.T, tasks map[string]record.PlanTask, task string, state record.TaskState, place record.TaskPlace) {
	t.Helper()
	got, ok := tasks[task]
	if !ok || got.State != state || got.Place != place {
		t.Fatalf("task %s is %s at %+v (held by the plan: %t), want %s at %+v", task, got.State, got.Place, ok, state, place)
	}
}

// A task that does not merge is done when its last step passes: with task a, `implementer
// reviewer`, at its review and task b, `implementer`, needing it, so b is the plan's one task that
// merges, a's approved review leaves a done at step 2 and starts no retro of a's, since a does not
// merge. The issue does not leave the workflow: b, whose need is done, starts at its first step in
// the same transaction (startReady), the issue stands in implementing, and Legion writes no status
// of done and begins no linger.
//
// The fault it catches is a task that does not merge running a retro and a merge of its own, or an
// issue that leaves the workflow when one of its tasks is done (its control: the transition's leave
// on its row's done rather than on the phase its tasks put the issue in).
func TestATaskThatPassesItsLastStepIsDoneAndTheIssueGoesOn(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Reviewing)
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "a", steps: []string{"implementer", "reviewer"}, agents: []claim.Agent{"implementer", "reviewer"}, state: record.TaskRunning, place: atStep(2)},
		planTask{id: "b", steps: []string{"implementer"}, agents: []claim.Agent{"implementer-2"}, needs: []string{"a"}, state: record.TaskWaiting})

	taskMoves(t, pool, engine, root.Key, "a", TriggerReviewApproved, "")

	tasks := tasksOf(t, pool, engine, root.Key)
	assertTask(t, tasks, "a", record.TaskDone, atStep(2))
	if starts, rows := retroStarts(t, pool, root.Key); starts != 0 || rows != 0 {
		t.Fatalf("the retro of %s counts %d starts on %d rows, want none: task a does not merge", root.Key, starts, rows)
	}
	if retro := startsIn(t, pool, root.Key, claim.RoleImplementer, phase.Retro); len(retro) != 0 {
		t.Fatalf("the starts of the implementer in retro = %+v, want none: task a does not merge", retro)
	}
	assertTask(t, tasks, "b", record.TaskRunning, atStep(1))
	issue := issueOf(t, pool, engine, root.Key)
	if issue.Phase != phase.Implementing || issue.Hold != nil || issue.Status == "done" || issue.Lingers() {
		t.Fatalf("%s after task a is done stands in %s (hold %+v), status %q, lingering %t; want implementing, where task b starts, with no done and no linger",
			root.Key, issue.Phase, issue.Hold, issue.Status, issue.Lingers())
	}
	for _, write := range statusWrites(t, pool, root.Key) {
		if write.Status == "done" {
			t.Fatalf("the status writes of %s hold %+v, want no done: the issue goes on", root.Key, write)
		}
	}
}

// A move of one task writes the issue in the phase its tasks put it in, with that phase's Dispatch
// status: with task a at its implementer's step on the issue's branch, with its pull request #42,
// and task b at its review on legion/LEGION-208.b, with #45, b's review that requests changes takes
// b back to its implementer and writes the issue in implementing with the status in_progress, over
// the needs_review it showed; and a's implementation that passes takes a to its review and leaves
// the issue in implementing, where b now stands, writing no needs_review.
//
// The fault it catches is the issue that takes one task's phase as its own (its control: the
// transition's write of the copy read for the task with the store's PutIssue, which its refusal of
// such a copy refuses; and with that refusal taken off as well, the issue reads reviewing, a's
// phase, after a's move).
func TestAWriteOfAViewWritesTheIssuesDerivedPhase(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Implementing)
	root.Status = "needs_review"
	seedIssue(t, pool, root)
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "a", steps: []string{"implementer", "reviewer"}, agents: []claim.Agent{"implementer", "reviewer"}, state: record.TaskRunning, place: atStep(1)},
		planTask{id: "b", steps: []string{"implementer", "reviewer"}, agents: []claim.Agent{"implementer-2", "reviewer-2"}, state: record.TaskRunning, place: atStep(2)},
		planTask{id: "c", steps: []string{"implementer"}, agents: []claim.Agent{"implementer-3"}, needs: []string{"a", "b"}, state: record.TaskWaiting})
	seedTaskBranch(t, pool, root.Key, "b", "legion/LEGION-208.b")
	seedTaskPR(t, pool, engine, root.Key, "b", 45)

	taskMoves(t, pool, engine, root.Key, "b", TriggerReviewRejected, changesBody)

	tasks := tasksOf(t, pool, engine, root.Key)
	assertTask(t, tasks, "a", record.TaskRunning, atStep(1))
	assertTask(t, tasks, "b", record.TaskRunning, atStep(1))
	if issue := issueOf(t, pool, engine, root.Key); issue.Phase != phase.Implementing || issue.Hold != nil || issue.Status != "in_progress" {
		t.Fatalf("%s after b's review requested changes stands in %s (hold %+v) with status %q, want implementing with in_progress", root.Key, issue.Phase, issue.Hold, issue.Status)
	}
	if writes := statusWrites(t, pool, root.Key); len(writes) != 1 || writes[0].Status != "in_progress" {
		t.Fatalf("the status writes of %s = %+v, want the one of in_progress", root.Key, writes)
	}

	taskMoves(t, pool, engine, root.Key, "a", TriggerImplementationReady, "")

	tasks = tasksOf(t, pool, engine, root.Key)
	assertTask(t, tasks, "a", record.TaskRunning, atStep(2))
	assertTask(t, tasks, "b", record.TaskRunning, atStep(1))
	if issue := issueOf(t, pool, engine, root.Key); issue.Phase != phase.Implementing || issue.Hold != nil || issue.Status != "in_progress" {
		t.Fatalf("%s after a's implementation passed stands in %s (hold %+v) with status %q, want implementing, where b stands, with in_progress",
			root.Key, issue.Phase, issue.Hold, issue.Status)
	}
	if writes := statusWrites(t, pool, root.Key); len(writes) != 1 {
		t.Fatalf("the status writes of %s = %+v, want the one of in_progress and no needs_review: b still implements", root.Key, writes)
	}
}

// A move of the one task of a plan of one writes the issue in the phase its task puts it in, with
// that phase's Dispatch status, as a move of one task of several does: with task a, `implementer
// reviewer`, at its review with the issue's pull request #42, a's review that requests changes
// takes a back to its implementer and writes the issue in implementing with the status in_progress,
// over the needs_review it showed, in one row; and a's implementation that passes takes a to its
// review and writes the issue in reviewing with needs_review, in one row more.
//
// It is the twin of TestAWriteOfAViewWritesTheIssuesDerivedPhase on a plan of one task: the same
// move, the same derived phase and one status row for each move.
func TestAMoveOfThePlansOneTaskWritesTheIssueInItsTasksPhase(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Reviewing)
	root.Status = "needs_review"
	seedIssue(t, pool, root)
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "a", steps: []string{"implementer", "reviewer"}, agents: []claim.Agent{"implementer", "reviewer"}, state: record.TaskRunning, place: atStep(2)})

	taskMoves(t, pool, engine, root.Key, "a", TriggerReviewRejected, changesBody)

	assertTask(t, tasksOf(t, pool, engine, root.Key), "a", record.TaskRunning, atStep(1))
	if issue := issueOf(t, pool, engine, root.Key); issue.Phase != phase.Implementing || issue.Hold != nil || issue.Status != "in_progress" {
		t.Fatalf("%s after a's review requested changes stands in %s (hold %+v) with status %q, want implementing with in_progress", root.Key, issue.Phase, issue.Hold, issue.Status)
	}
	if writes := statusWrites(t, pool, root.Key); len(writes) != 1 || writes[0].Status != "in_progress" {
		t.Fatalf("the status writes of %s = %+v, want the one of in_progress", root.Key, writes)
	}

	taskMoves(t, pool, engine, root.Key, "a", TriggerImplementationReady, "")

	assertTask(t, tasksOf(t, pool, engine, root.Key), "a", record.TaskRunning, atStep(2))
	if issue := issueOf(t, pool, engine, root.Key); issue.Phase != phase.Reviewing || issue.Hold != nil || issue.Status != "needs_review" {
		t.Fatalf("%s after a's implementation passed stands in %s (hold %+v) with status %q, want reviewing with needs_review", root.Key, issue.Phase, issue.Hold, issue.Status)
	}
	if writes := statusWrites(t, pool, root.Key); len(writes) != 2 || writes[1].Status != "needs_review" {
		t.Fatalf("the status writes of %s = %+v, want in_progress and then needs_review", root.Key, writes)
	}
}
