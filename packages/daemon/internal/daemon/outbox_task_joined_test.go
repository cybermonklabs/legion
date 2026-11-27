package daemon

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// startCountOf is the count of the start rows the outbox holds for issue.
func startCountOf(t *testing.T, rig *taskBranchRig, issue string) int {
	t.Helper()
	var starts int
	if err := rig.pool.QueryRow(context.Background(), `select count(*) from outbox where issue = $1 and kind = 'supervise' and payload->>'op' = 'start'`,
		issue).Scan(&starts); err != nil {
		t.Fatalf("count the start rows of %s: %v", issue, err)
	}
	return starts
}

// seedConflictingJoin is the rig of a plan whose join conflicts, through the outbox: tasks a and c
// are done, and their branches each change line 1 of x.txt, so the join of c into d's branch,
// which starts at a's head, conflicts; d needs a and c. The first step of the work (the gate's
// opening), applied through the real workflow engine, starts d (startReady), which queues d's
// task_branch row. It answers the rig and the issue.
func seedConflictingJoin(t *testing.T) (*taskBranchRig, record.Issue) {
	t.Helper()
	return seedJoin(t, nil)
}

// seedJoin is seedConflictingJoin's rig with the tasks of more beside a, c and d, each waiting, and
// an engine that gives each working copy the directory dir names (workflow.Config.WorkspaceDir),
// none where dir is nil. Beside x.txt, a's branch adds a.txt and c's adds c.txt, each its own work
// alone. Each task runs one implementer step, keyed by its id.
func seedJoin(t *testing.T, dir func(string, ghrepo.Repository) string, more ...record.PlanTask) (*taskBranchRig, record.Issue) {
	t.Helper()
	ctx := context.Background()
	rig := &taskBranchRig{pool: isolatedOutboxPool(t), records: record.NewStore(), github: githubtest.GitRepository(t, "acme", "widgets", "main")}
	api := githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		if !rig.github.Handler(w, r) {
			githubtest.Unexpected(t, w, r)
		}
	}})
	engine := workflow.New(rig.records, workflow.Config{Project: "LEGION", Repositories: testRepositories, WorkspaceDir: dir}, quietLogger())
	rig.runner = &outbox{log: quietLogger(), pool: rig.pool, dispatchProject: "LEGION", records: rig.records, tokens: roleTokens{}, project: "legion",
		githubAPI: api, handlers: []intake.Handler{engine}}
	issue := taskBranchIssue
	issue.Phase = phase.Admitted
	putOutboxIssue(t, rig.pool, rig.records, issue)
	v := record.PlanVersion{Issue: issue.Key, Generation: issue.Generation, Version: 1, ArtifactID: "plan-artifact", Source: record.PlanSourceOwner,
		GivenBy: "legion-legion-legion-208-architect", GivenAt: time.Now()}
	done := record.TaskPlace{Kind: record.PlaceStep, Position: 1}
	rows := []record.PlanTask{
		{Issue: issue.Key, Generation: issue.Generation, Task: "a", PlanVersion: 1, BlockText: "Task a.", State: record.TaskDone, Place: done},
		{Issue: issue.Key, Generation: issue.Generation, Task: "c", PlanVersion: 1, BlockText: "Task c.", State: record.TaskDone, Place: done},
		{Issue: issue.Key, Generation: issue.Generation, Task: "d", PlanVersion: 1, BlockText: "Task d.", Needs: []string{"a", "c"}, State: record.TaskWaiting},
	}
	for _, task := range more {
		rows = append(rows, record.PlanTask{Issue: issue.Key, Generation: issue.Generation, Task: task.Task, PlanVersion: 1, BlockText: "Task " + task.Task + ".",
			Needs: task.Needs, State: record.TaskWaiting})
	}
	var steps []record.PlanStep
	for _, row := range rows {
		steps = append(steps, record.PlanStep{Issue: issue.Key, Generation: issue.Generation, Task: row.Task, Position: 1, Role: "implementer",
			Type: "implementation", AgentKey: row.Task})
	}
	if err := store.Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
		if err := rig.records.PutPlan(ctx, tx, v, rows, steps); err != nil {
			return err
		}
		for _, id := range []string{"a", "c"} {
			if _, err := rig.records.AssignTaskBranch(ctx, tx, issue.Key, issue.Generation, id); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("store the plan of %s: %v", issue.Key, err)
	}
	rig.github.Commit(t, rig.branch(t, "a"), map[string]string{"x.txt": "from a\n", "a.txt": "a\n"}, "a")
	rig.github.Commit(t, rig.branch(t, "c"), map[string]string{"x.txt": "from c\n", "c.txt": "c\n"}, "c")

	if err := store.Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
		ctx = engine.OpenFact(ctx)
		stored, err := rig.records.Issue(ctx, tx, issue.Key)
		if err != nil {
			return err
		}
		if err := engine.StartFirstPhase(ctx, tx, *stored, nil); err != nil {
			return err
		}
		return engine.SettleFact(ctx, tx)
	}); err != nil {
		t.Fatalf("open the gate of %s: %v", issue.Key, err)
	}
	return rig, issue
}

// runJoinOfD runs d's task_branch row, the one the rig's outbox holds for issue, through the
// runner, which fails the test where the row is not d's at the issue's generation.
func runJoinOfD(t *testing.T, rig *taskBranchRig, issue record.Issue) {
	t.Helper()
	ctx := context.Background()
	var id int64
	var payload record.TaskBranch
	if err := rig.pool.QueryRow(ctx, `select id, payload from outbox where issue = $1 and kind = 'task_branch'`, issue.Key).Scan(&id, &payload); err != nil {
		t.Fatalf("read d's task_branch row: %v", err)
	}
	if payload != (record.TaskBranch{Generation: issue.Generation, Task: "d"}) {
		t.Fatalf("the task_branch row = %+v, want d's at generation %d", payload, issue.Generation)
	}
	row := mustOutboxRow(t, issue.Key, payload, time.Now())
	row.ID = id
	if err := rig.runner.execute(ctx, row); err != nil {
		t.Fatalf("execute d's task_branch row: %v", err)
	}
}

// The first step of a task waits for the end of its join, through the outbox
// (seedConflictingJoin): after the gate's opening d runs at its first step, its step waits for its
// join, its task_branch row is queued, and no start of d is. The runner runs that row: GitHub
// refuses c's merge (409), and the end of the join, applied through the real workflow engine,
// writes c as d's join conflict and raises d's entry in one transaction: d's start row is queued,
// and its step waits for nothing.
//
// The fault it catches is a first step whose task is composed before its join ended (its control:
// the workflow's start without the join's check, so d's start row is queued before its row ran).
func TestTheFirstStepWaitsForTheEndOfItsJoin(t *testing.T) {
	t.Parallel()
	rig, issue := seedConflictingJoin(t)

	if d := rig.task(t, "d"); d.State != record.TaskRunning || d.StepWait != record.StepWaitJoin {
		t.Fatalf("task d after the first step is %s, its step waiting for %q, want running and waiting for its join", d.State, d.StepWait)
	}
	if starts := startCountOf(t, rig, issue.Key); starts != 0 {
		t.Fatalf("the start rows of %s before d's join ended = %d, want none: d's step waits for its join", issue.Key, starts)
	}

	runJoinOfD(t, rig, issue)

	d := rig.task(t, "d")
	if d.StepWait != record.StepWaitNone || !slices.Equal(d.JoinConflicts, []string{"c"}) {
		t.Fatalf("task d after its row ran waits for %q with join conflicts %v, want nothing and [c]", d.StepWait, d.JoinConflicts)
	}
	if starts := startCountOf(t, rig, issue.Key); starts != 1 {
		t.Fatalf("the start rows of %s after d's join ended = %d, want d's one", issue.Key, starts)
	}
}

// The agent of the first step of a task whose join conflicted is told so in the task its start
// delivers, through the outbox (seedConflictingJoin): GitHub refuses c's merge, and the start row
// the end of the join queues tells d's agent that GitHub could not join the work of task c, with
// c's branch to join and the branch to push, since the task is composed when the entry is raised,
// after the join ended. Where d merges, its join is made on its branch, which it is told to repair
// and push. Where a task needs d (seedJoin with taskNeedingD), d's join is made on its join branch,
// the base of its pull request, and d is told to repair the join there and push the join branch,
// so its pull request shows its own work alone.
//
// The fault it catches is a conflict the agent of the first step is not told, or told on the
// task's branch where the task has a join branch (its controls: the conflict sentence dropped from
// the step's task, under which the start row names no join; the join branch left out of the
// sentence, under which the second case fails).
func TestAJoinThatConflictsIsInTheFirstStepsText(t *testing.T) {
	t.Parallel()
	t.Run("a task that merges, joined on its branch", func(t *testing.T) {
		rig, issue := seedConflictingJoin(t)
		runJoinOfD(t, rig, issue)
		c := rig.task(t, "c").BranchOf(issue.Key)
		d := rig.task(t, "d").BranchOf(issue.Key)
		task := newestStartTask(t, rig, issue)
		for _, want := range []string{" GitHub could not join the work of task c into this branch: their changes conflict.", " " + c + "@origin ",
			"`jj git push --bookmark " + d + "`."} {
			if !strings.Contains(task, want) {
				t.Fatalf("the task of d's start row = %q,\nwant it to hold %q", task, want)
			}
		}
	})
	t.Run("a task with a join branch", func(t *testing.T) {
		rig, issue := seedJoin(t, nil, taskNeedingD)
		runJoinOfD(t, rig, issue)
		c := rig.task(t, "c").BranchOf(issue.Key)
		join := rig.task(t, "d").BranchOf(issue.Key) + claim.JoinBranchSuffix
		task := newestStartTask(t, rig, issue)
		for _, want := range []string{" GitHub could not join the work of task c into the join branch " + join + ", the base of this task's pull request: their changes conflict.",
			"`jj new " + join + "@origin " + c + "@origin -m \"LEGION-208: join the work of task c\"`",
			"`jj git push --bookmark " + join + "`."} {
			if !strings.Contains(task, want) {
				t.Fatalf("the task of d's start row = %q,\nwant it to hold %q", task, want)
			}
		}
	})
}
