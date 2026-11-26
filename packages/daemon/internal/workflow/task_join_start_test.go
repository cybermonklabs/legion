package workflow

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// seedMove is the store's move of task of the plan of key at generation to place in state
// (record.Store.MoveTask) as a seed of the store a test starts from, and the end of the wait for a
// join that move writes on a start of the task (record.StepWaitJoin): the step a seed puts at a
// running task's place is one whose start, and so whose join, ended before the test.
func seedMove(ctx context.Context, tx pgx.Tx, key string, generation uint64, task string, place record.TaskPlace, state record.TaskState) error {
	store := record.NewStore()
	if err := store.MoveTask(ctx, tx, key, generation, task, place, state); err != nil || state != record.TaskRunning {
		return err
	}
	return store.SetStepWait(ctx, tx, key, generation, task, record.StepWaitNone, nil)
}

// taskBranchRowsOf is the task each task_branch row of the outbox names, oldest first.
func taskBranchRowsOf(t *testing.T, pool *pgxpool.Pool, key string) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `select payload->>'task' from outbox where kind = 'task_branch' and issue = $1 order by id`, key)
	if err != nil {
		t.Fatalf("read the task_branch rows of %s: %v", key, err)
	}
	named, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the task_branch rows of %s: %v", key, err)
	}
	return named
}

// joinEnds applies the end of the join of task of key (intake.TaskJoined) on the branch and the
// needs its row holds now, with no conflict, as the executor of its task_branch row applies it, and
// fails the test where the fact is not taken.
func joinEnds(t *testing.T, pool *pgxpool.Pool, engine *Engine, key, task string) {
	t.Helper()
	issue := issueOf(t, pool, engine, key)
	row := tasksOf(t, pool, engine, key)[task]
	id := fmt.Sprintf("task-joined:%s:%d:%s:%v", key, issue.Generation, task, row.StepWaitSince)
	mustSpend(t, "the end of the join of task "+task, applyFacts(t, pool, engine)(id,
		intake.TaskJoined{Issue: key, Generation: issue.Generation, Task: task, Row: joinRowOf(t, pool, key, issue.Generation, task), Branch: row.BranchOf(key), Needs: row.Needs}))
}

// joinsEnd applies the end of the join of each task of key whose step waits for its join and whose
// task_branch row is queued (joinEnds), in task id order, as the outbox applies the end of each join
// it ran: what a test whose act starts tasks on branches of their own reads once those joins ended.
func joinsEnd(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string) {
	t.Helper()
	queued := taskBranchRowsOf(t, pool, key)
	var waiting []string
	for id, row := range tasksOf(t, pool, engine, key) {
		if row.State == record.TaskRunning && row.StepWait == record.StepWaitJoin && slices.Contains(queued, id) {
			waiting = append(waiting, id)
		}
	}
	slices.Sort(waiting)
	for _, id := range waiting {
		joinEnds(t, pool, engine, key, id)
	}
}

// Each start of a task joins its needs at the one place that starts a step (Engine.start): the
// move of a task into running from waiting writes that its step waits for its join, and the
// start queues the task's task_branch row and keeps the wait, so nothing of the step starts until
// the end of the join (intake.TaskJoined) starts it. A task that holds the issue's own branch and
// needs none queues no row, and its step starts at once, as an issue of one task starts today.
//
// The fault it catches is a first step whose text is composed before the join of its needs ended
// (its control: start without the join's check, so d's step starts at once and the assertion
// that it waits fails).
func TestEachStartOfATaskWaitsForItsJoin(t *testing.T) {
	t.Run("the first start of a task with needs waits for its join", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Implementing)
		seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
		seedTasks(t, pool, root.Key, root.Generation,
			planTask{id: "a", steps: []string{"implementer:a"}, agents: []claim.Agent{"impl-a"}, state: record.TaskDone, place: atStep(1)},
			planTask{id: "c", steps: []string{"implementer:c"}, agents: []claim.Agent{"impl-c"}, state: record.TaskDone, place: atStep(1)},
			planTask{id: "d", steps: []string{"implementer:d"}, agents: []claim.Agent{"impl-d"}, needs: []string{"a", "c"}, state: record.TaskWaiting})
		seedTaskBranch(t, pool, root.Key, "a", "legion/"+root.Key)
		seedTaskBranch(t, pool, root.Key, "c", "legion/"+root.Key+".c")

		onStored(t, pool, engine, root.Key, func(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
			stored, err := engine.store.Plan(ctx, tx, issue.Key, issue.Generation)
			if err != nil {
				return err
			}
			_, err = engine.startReady(ctx, tx, issue, stored, "")
			return err
		})

		if rows := taskBranchRowsOf(t, pool, root.Key); !slices.Equal(rows, []string{"d"}) {
			t.Fatalf("the task_branch rows of %s name %v, want d's one", root.Key, rows)
		}
		assertWaits(t, tasksOf(t, pool, engine, root.Key), "d", atStep(1), record.StepWaitJoin)
		if claims := startClaims(t, pool, root.Key, claim.RoleImplementer); slices.Contains(claims, agentClaim(t, root.Key, "impl-d")) {
			t.Fatalf("the implementer's starts of %s name %v, want none of d's agent: its step waits for its join", root.Key, claims)
		}
		assertNothingStarted(t, pool, root.Key, "d", atStep(1))

		branch := tasksOf(t, pool, engine, root.Key)["d"].BranchOf(root.Key)
		mustSpend(t, "the end of d's join", applyFacts(t, pool, engine)("task-joined:d",
			intake.TaskJoined{Issue: root.Key, Generation: root.Generation, Task: "d", Row: joinRowOf(t, pool, root.Key, root.Generation, "d"), Branch: branch, Needs: []string{"a", "c"}}))

		assertWaits(t, tasksOf(t, pool, engine, root.Key), "d", atStep(1), record.StepWaitNone)
		if claims := startClaims(t, pool, root.Key, claim.RoleImplementer); !slices.Contains(claims, agentClaim(t, root.Key, "impl-d")) {
			t.Fatalf("the implementer's starts of %s after d's join name %v, want one of d's agent impl-d", root.Key, claims)
		}
	})
	t.Run("a start again waits for its join, and its step is told why and moves back", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Implementing)
		seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
		seedTasks(t, pool, root.Key, root.Generation,
			planTask{id: "a", steps: []string{"reviewer:a"}, agents: []claim.Agent{"rev-a"}, state: record.TaskRunning, place: atStep(1)},
			planTask{id: "c", steps: []string{"implementer:c"}, agents: []claim.Agent{"impl-c"}, state: record.TaskRunning, place: atStep(1)},
			planTask{id: "x", steps: []string{"tester:x"}, agents: []claim.Agent{"test-x"}, state: record.TaskDone, place: atStep(1)})
		seedTaskBranch(t, pool, root.Key, "a", "legion/"+root.Key)
		seedTaskBranch(t, pool, root.Key, "c", "legion/"+root.Key+".c")
		stopTasks(t, pool, root.Key, root.Generation, "c")

		mustTake(t, "version 2", applyFacts(t, pool, engine)("version-2", laterVersion(t, root.Key, root.Generation, 2,
			roleBlock("a", "reviewer", ""), roleBlock("c", "implementer", " Again.", "x"), roleBlock("x", "tester", ""))))

		if rows := joinRowsOf(t, pool, root.Key); !slices.Equal(rows, []string{"c at 1"}) {
			t.Fatalf("the task_branch rows of %s after version 2 = %v, want c's one", root.Key, rows)
		}
		assertWaits(t, tasksOf(t, pool, engine, root.Key), "c", atStep(1), record.StepWaitJoin)
		if claims := startClaims(t, pool, root.Key, claim.RoleImplementer); len(claims) != 0 {
			t.Fatalf("the implementer's starts of %s = %v, want none: c's step waits for its join", root.Key, claims)
		}

		joinEnds(t, pool, engine, root.Key, "c")

		assertWaits(t, tasksOf(t, pool, engine, root.Key), "c", atStep(1), record.StepWaitNone)
		const told = "Reason: the owner gave version 2 of the plan while task c was stopped."
		var row record.PhaseRow
		onTask(t, pool, engine, root.Key, "c", func(ctx context.Context, tx pgx.Tx, s TaskScope) (err error) {
			row, err = engine.stepRow(ctx, tx, s, claim.RoleImplementer)
			return err
		})
		if !row.EntryMovedBack || !strings.Contains(row.EntryTask, told) {
			t.Fatalf("the implementer's row after c's join has entry %d (moved back %t) and task %q, want an entry raised as a move back whose task holds %q",
				row.Entry, row.EntryMovedBack, row.EntryTask, told)
		}
		if claims := startClaims(t, pool, root.Key, claim.RoleImplementer); !slices.Equal(claims, []claim.Token{agentClaim(t, root.Key, "impl-c")}) {
			t.Fatalf("the implementer's starts of %s after c's join = %v, want c's agent impl-c alone", root.Key, claims)
		}
	})
	t.Run("a move back to a first step joins nothing", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Testing)
		seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
		seedTasks(t, pool, root.Key, root.Generation,
			planTask{id: "d", steps: []string{"implementer:d", "tester:d"}, agents: []claim.Agent{"impl-d", "test-d"}, needs: []string{"x"},
				state: record.TaskRunning, place: atStep(2)},
			planTask{id: "x", steps: []string{"reviewer:x"}, agents: []claim.Agent{"rev-x"}, state: record.TaskDone, place: atStep(1)})
		seedTaskBranch(t, pool, root.Key, "d", "legion/"+root.Key+".d")

		taskMoves(t, pool, engine, root.Key, "d", TriggerTesterFailed, "the widget does not build")

		if rows := joinRowsOf(t, pool, root.Key); len(rows) != 0 {
			t.Fatalf("the task_branch rows of %s after d moved back = %v, want none: a move back is no start of the task", root.Key, rows)
		}
		assertWaits(t, tasksOf(t, pool, engine, root.Key), "d", atStep(1), record.StepWaitNone)
		if claims := startClaims(t, pool, root.Key, claim.RoleImplementer); !slices.Equal(claims, []claim.Token{agentClaim(t, root.Key, "impl-d")}) {
			t.Fatalf("the implementer's starts of %s after d moved back = %v, want d's agent impl-d alone", root.Key, claims)
		}
	})
	t.Run("the task that holds the issue's branch and needs none starts at once", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)
		seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
		seedTasks(t, pool, root.Key, root.Generation,
			planTask{id: "a", steps: []string{"implementer:a"}, agents: []claim.Agent{"impl-a"}, state: record.TaskWaiting},
			planTask{id: "b", steps: []string{"implementer:b"}, agents: []claim.Agent{"impl-b"}, needs: []string{"a"}, state: record.TaskWaiting})

		firstStepOf(t, pool, engine, root.Key)

		if rows := taskBranchRowsOf(t, pool, root.Key); len(rows) != 0 {
			t.Fatalf("the task_branch rows of %s name %v, want none: a holds the issue's own branch and needs nothing", root.Key, rows)
		}
		assertWaits(t, tasksOf(t, pool, engine, root.Key), "a", atStep(1), record.StepWaitNone)
		if claims := startClaims(t, pool, root.Key, claim.RoleImplementer); !slices.Equal(claims, []claim.Token{agentClaim(t, root.Key, "impl-a")}) {
			t.Fatalf("the implementer's starts of %s name %v, want a's agent impl-a alone", root.Key, claims)
		}
	})
}

// roleBlock is the plan_task block of task id as a version gives it: "Do <id>." with text added,
// one step of role whose agent key is id, and needs.
func roleBlock(id, role, text string, needs ...string) plan.Task {
	return plan.Task{ID: id, Text: "Do " + id + "." + text, Needs: needs, Steps: []plan.Step{{Role: role, Key: id}}}
}

// joinRowsOf is each task_branch row of the outbox of key, oldest first, as `<task> at <generation>`.
func joinRowsOf(t *testing.T, pool *pgxpool.Pool, key string) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `select (payload->>'task') || ' at ' || (payload->>'generation') from outbox
		where kind = 'task_branch' and issue = $1 order by id`, key)
	if err != nil {
		t.Fatalf("read the task_branch rows of %s: %v", key, err)
	}
	named, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the task_branch rows of %s: %v", key, err)
	}
	return named
}

// A join that waited at a re-admission starts again: task d, which needs c, runs at its first
// step, which waits for its join, and the task_branch row of its join at generation 1 has not run.
// The re-admission of the issue carries its run to generation 2, d's wait among it, and the row of
// generation 1 will finish without acting (the outbox's taskBranch), so the settle of the
// re-admission's fact queues d's join at generation 2, and d keeps its wait: nothing of its step
// starts. The end of the join of generation 1 changes nothing; the end of the join of generation 2
// starts d's step.
//
// The fault it catches is a join that nothing queues again, so its task waits for ever (its
// control: the settle without the join again: no row at generation 2, and the test fails).
func TestAJoinThatWaitedAtAReadmissionStartsAgain(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Implementing)
	seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "c", steps: []string{"tester:c"}, agents: []claim.Agent{"test-c"}, state: record.TaskDone, place: atStep(1)},
		planTask{id: "d", steps: []string{"implementer:d"}, agents: []claim.Agent{"impl-d"}, needs: []string{"c"}, state: record.TaskRunning, place: atStep(1)})
	branch := "legion/" + root.Key + ".d"
	seedTaskBranch(t, pool, root.Key, "d", branch)
	joinWaits(t, pool, root.Key, root.Generation, "d")

	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		next := root
		next.Generation++
		if err := engine.store.PutIssue(ctx, tx, next); err != nil {
			return err
		}
		return engine.store.CarryPlan(ctx, tx, root.Key, root.Generation, next.Generation, true)
	})

	if rows := joinRowsOf(t, pool, root.Key); !slices.Equal(rows, []string{"d at 1", "d at 2"}) {
		t.Fatalf("the task_branch rows of %s after its re-admission = %v, want d's at generation 1 and d's at generation 2", root.Key, rows)
	}
	assertWaits(t, tasksOf(t, pool, engine, root.Key), "d", atStep(1), record.StepWaitJoin)
	if claims := startClaims(t, pool, root.Key, claim.RoleImplementer); len(claims) != 0 {
		t.Fatalf("the implementer's starts of %s = %v, want none: d's step waits for its join", root.Key, claims)
	}

	apply := applyFacts(t, pool, engine)
	mustSpend(t, "the end of d's join at generation 1", apply("task-joined:1",
		intake.TaskJoined{Issue: root.Key, Generation: root.Generation, Task: "d", Row: joinRowOf(t, pool, root.Key, root.Generation, "d"), Branch: branch, Needs: []string{"c"}}))
	assertWaits(t, tasksOf(t, pool, engine, root.Key), "d", atStep(1), record.StepWaitJoin)

	mustSpend(t, "the end of d's join at generation 2", apply("task-joined:2",
		intake.TaskJoined{Issue: root.Key, Generation: root.Generation + 1, Task: "d", Row: joinRowOf(t, pool, root.Key, root.Generation+1, "d"), Branch: branch, Needs: []string{"c"}}))
	assertWaits(t, tasksOf(t, pool, engine, root.Key), "d", atStep(1), record.StepWaitNone)
	if claims := startClaims(t, pool, root.Key, claim.RoleImplementer); !slices.Equal(claims, []claim.Token{agentClaim(t, root.Key, "impl-d")}) {
		t.Fatalf("the implementer's starts of %s after d's join at generation 2 = %v, want d's agent impl-d alone", root.Key, claims)
	}
}

// The one task of a plan of one starts at its first step at once, on the issue's own branch: it
// needs none and takes that branch at its first start (record.Store.AssignTaskBranch), so the start
// queues no task_branch row and its step waits for no join, the outcome of a task of several that
// holds the issue's branch and needs none.
//
// It is the twin of TestEachStartOfATaskWaitsForItsJoin's last case on a plan of one task.
func TestThePlansOneTaskStartsAtOnceOnTheIssuesBranch(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Admitted)
	seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "a", steps: []string{"implementer:a"}, agents: []claim.Agent{"impl-a"}, state: record.TaskWaiting})

	firstStepOf(t, pool, engine, root.Key)

	if rows := taskBranchRowsOf(t, pool, root.Key); len(rows) != 0 {
		t.Fatalf("the task_branch rows of %s name %v, want none: a holds the issue's own branch and needs nothing", root.Key, rows)
	}
	tasks := tasksOf(t, pool, engine, root.Key)
	assertWaits(t, tasks, "a", atStep(1), record.StepWaitNone)
	if branch := tasks["a"].BranchOf(root.Key); branch != claim.IssueBranch(root.Key) {
		t.Fatalf("task a works on %s, want the issue's own branch %s", branch, claim.IssueBranch(root.Key))
	}
	if claims := startClaims(t, pool, root.Key, claim.RoleImplementer); !slices.Equal(claims, []claim.Token{agentClaim(t, root.Key, "impl-a")}) {
		t.Fatalf("the implementer's starts of %s name %v, want a's agent impl-a alone", root.Key, claims)
	}
}
