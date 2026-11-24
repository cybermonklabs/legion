package daemon

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	stepplan "github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// taskBranchIssue is the issue whose plan each test of a task's branch stores.
var taskBranchIssue = record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
	Tree: "LEGION-208", Title: "Workflow", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "A"}

// taskBranchRig is the executor of task_branch rows against a fake GitHub whose refs and commits are
// a real repository's, with the plan of taskBranchIssue stored.
type taskBranchRig struct {
	pool    *store.Pool
	records *record.Postgres
	github  *githubtest.GitRepo
	runner  *outbox
}

// plannedTask is a task of the stored plan: its id, the ids it needs and the role of each of its
// steps, typed as Legion's own roles type them (stepplan.BuiltinRoles); one implementer step where
// it names none.
type plannedTask struct {
	id    string
	needs []string
	roles []string
}

// newTaskBranchRig stores taskBranchIssue and its plan of tasks, starts each task in the order of
// their ids as the start of tasks does — its branch given (record.Store.AssignTaskBranch), then
// the task moved to running at its first step, whose step waits for its join — and stands up the
// executor, which applies the end of each join through the real workflow engine, and the fake
// GitHub. Any request the fake does not serve fails the test. Each rig is the store as the start of
// tasks leaves it with a plan of several tasks that register_plan took.
func newTaskBranchRig(t *testing.T, tasks ...plannedTask) *taskBranchRig {
	t.Helper()
	ctx := context.Background()
	rig := &taskBranchRig{pool: isolatedOutboxPool(t), records: record.NewStore(), github: githubtest.GitRepository(t, "acme", "widgets", "main")}
	api := githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		if !rig.github.Handler(w, r) {
			githubtest.Unexpected(t, w, r)
		}
	}})
	rig.runner = &outbox{log: quietLogger(), pool: rig.pool, dispatchProject: "LEGION", records: rig.records, tokens: roleTokens{}, project: "legion", githubAPI: api}
	rig.runner.handlers = []intake.Handler{workflow.New(rig.records, workflow.Config{Project: "LEGION", Repositories: testRepositories}, quietLogger())}
	putOutboxIssue(t, rig.pool, rig.records, taskBranchIssue)
	issue := taskBranchIssue
	v := record.PlanVersion{Issue: issue.Key, Generation: issue.Generation, Version: 1, ArtifactID: "plan-artifact", Source: record.PlanSourceOwner, GivenAt: time.Now()}
	var rows []record.PlanTask
	var steps []record.PlanStep
	roles := stepplan.BuiltinRoles()
	for _, task := range tasks {
		rows = append(rows, record.PlanTask{Issue: issue.Key, Generation: issue.Generation, Task: task.id, PlanVersion: 1, BlockText: "Task " + task.id + ".",
			Needs: task.needs, State: record.TaskWaiting})
		named := task.roles
		if len(named) == 0 {
			named = []string{"implementer"}
		}
		for i, role := range named {
			steps = append(steps, record.PlanStep{Issue: issue.Key, Generation: issue.Generation, Task: task.id, Position: i + 1, Role: role, Type: roles[role].Type})
		}
	}
	if err := store.Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
		if err := rig.records.PutPlan(ctx, tx, v, rows, steps); err != nil {
			return err
		}
		for _, task := range tasks {
			if _, err := rig.records.AssignTaskBranch(ctx, tx, issue.Key, issue.Generation, task.id); err != nil {
				return err
			}
			if err := rig.records.MoveTask(ctx, tx, issue.Key, issue.Generation, task.id, record.TaskPlace{Kind: record.PlaceStep, Position: 1}, record.TaskRunning); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("store the plan of %s: %v", issue.Key, err)
	}
	return rig
}

// task is the stored row of task id.
func (rig *taskBranchRig) task(t *testing.T, id string) record.PlanTask {
	t.Helper()
	var stored *record.StoredPlan
	if err := store.Transact(context.Background(), rig.pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		stored, err = rig.records.Plan(context.Background(), tx, taskBranchIssue.Key, taskBranchIssue.Generation)
		return err
	}); err != nil {
		t.Fatalf("read the plan of %s: %v", taskBranchIssue.Key, err)
	}
	if stored != nil {
		for _, task := range stored.Tasks {
			if task.Task == id {
				return task
			}
		}
	}
	t.Fatalf("the plan of %s holds no task %s", taskBranchIssue.Key, id)
	return record.PlanTask{}
}

// branch is the branch of task id, as its row holds it.
func (rig *taskBranchRig) branch(t *testing.T, id string) string {
	t.Helper()
	return rig.task(t, id).BranchOf(taskBranchIssue.Key)
}

// run executes the task_branch row of task id, at the issue's generation.
func (rig *taskBranchRig) run(t *testing.T, id string) {
	t.Helper()
	row := mustOutboxRow(t, taskBranchIssue.Key, record.TaskBranch{Generation: taskBranchIssue.Generation, Task: id}, time.Now())
	if err := rig.runner.execute(context.Background(), row); err != nil {
		t.Fatalf("execute the branch row of task %s: %v", id, err)
	}
}

// parents is the parents of commit in the fake's repository, in order.
func (rig *taskBranchRig) parents(t *testing.T, commit string) []string {
	t.Helper()
	line := fixturegit.Isolated(t, t.TempDir()).Run(rig.github.Dir(), "git", "rev-list", "--parents", "-n", "1", commit)
	return strings.Fields(line)[1:]
}

// refs is every ref of the fake's repository and the commit it names, one line each.
func (rig *taskBranchRig) refs(t *testing.T) string {
	t.Helper()
	return fixturegit.Isolated(t, t.TempDir()).Run(rig.github.Dir(), "git", "for-each-ref", "--format=%(refname) %(objectname)")
}

// A task that needs no task starts at the issue's default branch, whether it holds the issue's own
// branch or one of its own, and joins nothing.
func TestATaskWithNoNeedsStartsAtTheDefaultBranch(t *testing.T) {
	t.Parallel()
	rig := newTaskBranchRig(t, plannedTask{id: "a"}, plannedTask{id: "b"})
	main := rig.github.Head(t, "main")
	for _, id := range []string{"a", "b"} {
		rig.run(t, id)
		branch := rig.branch(t, id)
		if head := rig.github.Head(t, branch); head != main {
			t.Errorf("task %s's branch %s stands at %q, want main's head %s", id, branch, head, main)
		}
		if conflicts := rig.task(t, id).JoinConflicts; len(conflicts) != 0 {
			t.Errorf("task %s's join conflicts = %v, want none", id, conflicts)
		}
	}
	if got, want := rig.branch(t, "b"), "legion/LEGION-208.b"; got != want {
		t.Fatalf("task b's branch = %s, want %s", got, want)
	}
}

// A task that needs one task starts at the head of that task's branch.
func TestATaskWithOneNeedStartsAtItsHead(t *testing.T) {
	t.Parallel()
	rig := newTaskBranchRig(t, plannedTask{id: "a"}, plannedTask{id: "d", needs: []string{"a"}})
	a := rig.github.Commit(t, rig.branch(t, "a"), map[string]string{"a.txt": "a\n"}, "a")

	rig.run(t, "d")

	if head := rig.github.Head(t, rig.branch(t, "d")); head != a {
		t.Fatalf("task d's branch stands at %q, want task a's head %s", head, a)
	}
}

// twoNeeds is a plan whose task d needs tasks a and c, in that order.
var twoNeeds = []plannedTask{{id: "a"}, {id: "c"}, {id: "d", needs: []string{"a", "c"}}}

// A task that needs two tasks starts from their join: its branch starts at the first need's head,
// in the order of its needs, and the second need is merged into it, so the branch's head is a merge
// commit whose parents are the two needs' heads, in that order.
func TestATaskWithTwoNeedsStartsAtTheirMerge(t *testing.T) {
	t.Parallel()
	rig := newTaskBranchRig(t, twoNeeds...)
	a := rig.github.Commit(t, rig.branch(t, "a"), map[string]string{"a.txt": "a\n"}, "a")
	c := rig.github.Commit(t, rig.branch(t, "c"), map[string]string{"c.txt": "c\n"}, "c")

	rig.run(t, "d")

	head := rig.github.Head(t, rig.branch(t, "d"))
	if parents := rig.parents(t, head); !slices.Equal(parents, []string{a, c}) {
		t.Fatalf("the parents of task d's head %s = %v, want task a's head %s and task c's head %s", head, parents, a, c)
	}
	if conflicts := rig.task(t, "d").JoinConflicts; len(conflicts) != 0 {
		t.Fatalf("task d's join conflicts = %v, want none", conflicts)
	}
}

// A need whose changes conflict with the needs joined before it is no refusal: GitHub's 409 is
// recorded as a join conflict of that need, and the branch stands on the needs that joined.
func TestAJoinThatConflictsIsRecordedAndTheBranchStandsOnTheFirstNeed(t *testing.T) {
	t.Parallel()
	rig := newTaskBranchRig(t, twoNeeds...)
	a := rig.github.Commit(t, rig.branch(t, "a"), map[string]string{"x.txt": "from a\n"}, "a")
	rig.github.Commit(t, rig.branch(t, "c"), map[string]string{"x.txt": "from c\n"}, "c")

	rig.run(t, "d")

	if head := rig.github.Head(t, rig.branch(t, "d")); head != a {
		t.Fatalf("task d's branch stands at %q, want task a's head %s", head, a)
	}
	if conflicts := rig.task(t, "d").JoinConflicts; !slices.Equal(conflicts, []string{"c"}) {
		t.Fatalf("task d's join conflicts = %v, want [c]", conflicts)
	}
}

// The end of a join is a fact: once its merges end, the row applies intake.TaskJoined through the
// runner's handlers, naming the task, the branch and the needs it joined and each need GitHub
// refused as a conflict, under an id of the issue, the generation, the task and the row, so a row
// that runs again applies one fact. The handler's failure is the row's, which retries.
func TestTheEndOfAJoinIsAppliedAsAFact(t *testing.T) {
	t.Parallel()
	rig := newTaskBranchRig(t, twoNeeds...)
	handler := &outboxFactHandler{}
	rig.runner.handlers = []intake.Handler{handler}
	rig.github.Commit(t, rig.branch(t, "a"), map[string]string{"x.txt": "from a\n"}, "a")
	rig.github.Commit(t, rig.branch(t, "c"), map[string]string{"x.txt": "from c\n"}, "c")
	row := mustOutboxRow(t, taskBranchIssue.Key, record.TaskBranch{Generation: taskBranchIssue.Generation, Task: "d"}, time.Now())
	row.ID = 17

	for range 2 {
		if err := rig.runner.execute(context.Background(), row); err != nil {
			t.Fatalf("execute the branch row of task d: %v", err)
		}
	}

	want := intake.TaskJoined{Issue: taskBranchIssue.Key, Generation: taskBranchIssue.Generation, Task: "d", Branch: "legion/LEGION-208.d",
		Needs: []string{"a", "c"}, Conflicts: []string{"c"}}
	got := handler.facts()
	if len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Fatalf("the facts the row applied = %#v, want the one %#v", got, want)
	}
	var ids []string
	rows, err := rig.pool.Query(context.Background(), `select event_id from processed_events where source = 'outbox' order by event_id`)
	if err == nil {
		ids, err = pgx.CollectRows(rows, pgx.RowTo[string])
	}
	if err != nil || !slices.Equal(ids, []string{"task-joined:LEGION-208:1:d:17"}) {
		t.Fatalf("the outbox's applied facts = %v, %v; want task-joined:LEGION-208:1:d:17 alone", ids, err)
	}

	rig.runner.handlers = []intake.Handler{&outboxFactHandler{err: errors.New("record unavailable")}}
	row.ID = 18
	if err := rig.runner.execute(context.Background(), row); err == nil || !strings.Contains(err.Error(), "record unavailable") {
		t.Fatalf("the row whose fact failed answered %v, want the handler's failure", err)
	}
}

// A row that runs again, as an outbox row can, finds each need joined already: GitHub answers each
// merge with 204, and no ref of the repository moves.
func TestARowThatRunsAgainMovesNoRef(t *testing.T) {
	t.Parallel()
	rig := newTaskBranchRig(t, twoNeeds...)
	rig.github.Commit(t, rig.branch(t, "a"), map[string]string{"a.txt": "a\n"}, "a")
	rig.github.Commit(t, rig.branch(t, "c"), map[string]string{"c.txt": "c\n"}, "c")
	rig.run(t, "d")
	before := rig.refs(t)

	rig.run(t, "d")

	if after := rig.refs(t); after != before {
		t.Fatalf("the refs after the row ran again:\n%s\nwant them as they were:\n%s", after, before)
	}
}

// A row of a task whose branch GitHub has, where its first start left it before a later version
// gave the task another need, joins the need that branch lacks: the work of every need the task has
// now is in its branch. No start again queues such a row at this release (taskBranch); only a
// task's first start does.
func TestAStartAgainJoinsANeedItLacks(t *testing.T) {
	t.Parallel()
	rig := newTaskBranchRig(t, twoNeeds...)
	a := rig.github.Commit(t, rig.branch(t, "a"), map[string]string{"a.txt": "a\n"}, "a")
	c := rig.github.Commit(t, rig.branch(t, "c"), map[string]string{"c.txt": "c\n"}, "c")
	fixturegit.Isolated(t, t.TempDir()).Run(rig.github.Dir(), "git", "update-ref", "refs/heads/"+rig.branch(t, "d"), a)

	rig.run(t, "d")

	head := rig.github.Head(t, rig.branch(t, "d"))
	if parents := rig.parents(t, head); !slices.Equal(parents, []string{a, c}) {
		t.Fatalf("the parents of task d's head %s = %v, want task a's head %s and task c's head %s", head, parents, a, c)
	}
	if conflicts := rig.task(t, "d").JoinConflicts; len(conflicts) != 0 {
		t.Fatalf("task d's join conflicts = %v, want none", conflicts)
	}
}

// A row of a task the plan no longer holds, of an issue of a tree that lingers, or of a generation
// the issue has left finishes without acting: no branch is made and no join written, so the starts
// it held back meet their own fences.
func TestATaskBranchRowThatServesNoStartingTaskFinishesWithoutActing(t *testing.T) {
	t.Parallel()
	rig := newTaskBranchRig(t, plannedTask{id: "a"}, plannedTask{id: "b"})
	before := rig.refs(t)
	execute := func(task string, generation uint64) {
		t.Helper()
		row := mustOutboxRow(t, taskBranchIssue.Key, record.TaskBranch{Generation: generation, Task: task}, time.Now())
		if err := rig.runner.execute(context.Background(), row); err != nil {
			t.Fatalf("execute the branch row of task %s at generation %d: %v", task, generation, err)
		}
		if after := rig.refs(t); after != before {
			t.Fatalf("the refs after the row of task %s at generation %d:\n%s\nwant them as they were:\n%s", task, generation, after, before)
		}
	}

	execute("gone", taskBranchIssue.Generation)

	until := time.Now().Add(time.Hour)
	putOutboxIssue(t, rig.pool, rig.records, record.Issue{Key: "LEGION-210", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: "LEGION-210", Title: "Closed", Phase: phase.Done, Generation: 1, Status: "backlog", Rank: "V", LingerUntil: &until})
	child, root := taskBranchIssue, "LEGION-210"
	child.Tree, child.Parent = root, &root
	putOutboxIssue(t, rig.pool, rig.records, child)
	execute("b", taskBranchIssue.Generation)

	left := taskBranchIssue
	left.Generation++
	putOutboxIssue(t, rig.pool, rig.records, left)
	execute("b", taskBranchIssue.Generation)
}
