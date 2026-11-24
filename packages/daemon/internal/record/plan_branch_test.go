package record

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/store"
)

// putTasks puts version of the plan of issue at generation, giving one waiting task of each id with
// no step.
func putTasks(t *testing.T, tx pgx.Tx, issue string, generation uint64, version int, ids ...string) {
	t.Helper()
	v := PlanVersion{Issue: issue, Generation: generation, Version: version, ArtifactID: planAuthor, Source: PlanSourceOwner,
		GivenBy: planOwner, GivenAt: planGivenAt.Add(time.Duration(version) * time.Minute)}
	var tasks []PlanTask
	for _, id := range ids {
		tasks = append(tasks, PlanTask{Issue: issue, Generation: generation, Task: id, PlanVersion: version, BlockText: "Do " + id + ".", State: TaskWaiting})
	}
	putPlan(t, tx, v, tasks, nil)
}

// assign is the branch AssignTaskBranch gives task.
func assign(t *testing.T, tx pgx.Tx, issue string, generation uint64, task string) string {
	t.Helper()
	branch, err := NewStore().AssignTaskBranch(context.Background(), tx, issue, generation, task)
	if err != nil {
		t.Fatalf("assign the branch of task %s of %s at generation %d: %v", task, issue, generation, err)
	}
	return branch
}

// removeTask stops task and deletes it, as a later version that removes a running task does.
func removeTask(t *testing.T, tx pgx.Tx, issue string, generation uint64, task string) {
	t.Helper()
	must(t, NewStore().StopTask(context.Background(), tx, issue, generation, task, "task "+task+" was removed by a later version"))
	must(t, NewStore().DeleteTasks(context.Background(), tx, issue, generation, []string{task}))
}

// taskOf is task of the plan of issue at generation as Plan reads it.
func taskOf(t *testing.T, tx pgx.Tx, issue string, generation uint64, task string) PlanTask {
	t.Helper()
	plan := readPlan(t, tx, issue, generation)
	if plan != nil {
		for _, stored := range plan.Tasks {
			if stored.Task == task {
				return stored
			}
		}
	}
	t.Fatalf("the plan of %s at generation %d holds no task %s", issue, generation, task)
	return PlanTask{}
}

// The first task of an issue to start takes the issue's own branch, so an issue whose plan has one
// task works on the branch it always had, and each other task takes the issue's branch, a dot and
// its slug: a short lower-case id is its own slug, any other id a hash of it. A task asked again
// answers the branch it holds, and a task the plan does not hold is refused, naming it.
func TestTheFirstTaskToStartTakesTheIssuesBranch(t *testing.T) {
	st := migratedStore(t)
	inTx(t, st, func(tx pgx.Tx) {
		putTasks(t, tx, planIssue, 1, 1, "a", "b", "Task C")
		a := assign(t, tx, planIssue, 1, "a")
		b := assign(t, tx, planIssue, 1, "b")
		c := assign(t, tx, planIssue, 1, "Task C")
		again := assign(t, tx, planIssue, 1, "b")
		if a != "legion/LEGION-208" || b != "legion/LEGION-208.b" || !strings.HasPrefix(c, "legion/LEGION-208.t-") || again != b {
			t.Fatalf("a=%q b=%q c=%q again=%q", a, b, c, again)
		}
		if got := taskOf(t, tx, planIssue, 1, "b").Branch; got != b {
			t.Fatalf("task b's row holds branch %q, want %q", got, b)
		}
	})
	if err := store.Transact(context.Background(), st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := NewStore().AssignTaskBranch(context.Background(), tx, planIssue, 1, "z")
		return err
	}); err == nil || !strings.Contains(err.Error(), "z") {
		t.Fatalf("the branch of a task the plan does not hold = %v, want a refusal naming it", err)
	}
}

// A task whose row holds no branch is a task of the release before this one, or an issue with no
// plan rows, and it works on the issue's own branch.
func TestATaskThatRanAtTheReleaseReadsTheIssuesBranch(t *testing.T) {
	task := PlanTask{Issue: "ACME-1", Task: "default"} // Branch "" as the release before wrote it
	if got := task.BranchOf("ACME-1"); got != "legion/ACME-1" {
		t.Fatalf("BranchOf = %q", got)
	}
	task.Branch = "legion/ACME-1.b"
	if got := task.BranchOf("ACME-1"); got != "legion/ACME-1.b" {
		t.Fatalf("BranchOf of a stored branch = %q", got)
	}
}

// A task that the release before started holds no branch on its row and works on the issue's own
// branch, so no task this release starts beside it takes that branch.
func TestATaskStartedBeforeThisReleaseKeepsTheIssuesBranch(t *testing.T) {
	st := migratedStore(t)
	inTx(t, st, func(tx pgx.Tx) {
		v := PlanVersion{Issue: planIssue, Generation: 1, Version: 1, ArtifactID: planAuthor, Source: PlanSourceOwner, GivenBy: planOwner, GivenAt: planGivenAt}
		putPlan(t, tx, v, []PlanTask{
			{Issue: planIssue, Generation: 1, Task: "a", PlanVersion: 1, BlockText: "Do a.", State: TaskRunning},
			{Issue: planIssue, Generation: 1, Task: "b", PlanVersion: 1, BlockText: "Do b.", State: TaskWaiting},
		}, nil)
		if b := assign(t, tx, planIssue, 1, "b"); b != "legion/LEGION-208.b" {
			t.Fatalf("task b beside a task the release before started took %q, want legion/LEGION-208.b", b)
		}
	})
}

// A branch given to a task of a generation is never given to another task of it: GitHub keeps the
// branch and its commits after the task that held it was stopped and removed. So a task that starts
// after the first task to start was removed does not take the issue's branch, and the removed task's
// id, given again, takes its slug's branch, which no task had.
func TestATaskThatStartsAfterTheFirstWasRemovedTakesNotTheIssuesBranch(t *testing.T) {
	st := migratedStore(t)
	inTx(t, st, func(tx pgx.Tx) {
		putTasks(t, tx, planIssue, 1, 1, "a", "b")
		if a := assign(t, tx, planIssue, 1, "a"); a != "legion/LEGION-208" {
			t.Fatalf("task a took %q, want the issue's branch", a)
		}
		removeTask(t, tx, planIssue, 1, "a")
		if b := assign(t, tx, planIssue, 1, "b"); b != "legion/LEGION-208.b" {
			t.Fatalf("task b, started after task a was removed, took %q, want legion/LEGION-208.b: the issue's branch went to task a", b)
		}
		putTasks(t, tx, planIssue, 1, 2, "a")
		if a := assign(t, tx, planIssue, 1, "a"); a != "legion/LEGION-208.a" {
			t.Fatalf("task a, given again after it was removed, took %q, want legion/LEGION-208.a", a)
		}
	})
}

// An id whose task held its slug's branch and was removed, given again by a later version, is a new
// task, and takes the next give of its slug: the removed task's branch and its commits stay its own.
func TestARemovedIdThatComesBackTakesANewName(t *testing.T) {
	st := migratedStore(t)
	inTx(t, st, func(tx pgx.Tx) {
		putTasks(t, tx, planIssue, 1, 1, "a", "b")
		assign(t, tx, planIssue, 1, "a")
		if b := assign(t, tx, planIssue, 1, "b"); b != "legion/LEGION-208.b" {
			t.Fatalf("task b took %q, want legion/LEGION-208.b", b)
		}
		removeTask(t, tx, planIssue, 1, "b")
		putTasks(t, tx, planIssue, 1, 2, "b")
		if b := assign(t, tx, planIssue, 1, "b"); b != "legion/LEGION-208.b.2" {
			t.Fatalf("task b, given again after it was removed, took %q, want legion/LEGION-208.b.2: legion/LEGION-208.b went to the removed task", b)
		}
	})
}

// A carried copy of a generation keeps a running task's branch, the needs whose join conflicted and
// every name the generation gave, since the carried run goes on in its working copies. The copy that
// starts the plan from its start keeps no task's branch, and its plan holds task a alone, so a takes
// the issue's branch again, as the one task of a plan of one always does.
func TestACarriedTaskKeepsItsBranch(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	inTx(t, st, func(tx pgx.Tx) {
		putTasks(t, tx, planIssue, 1, 1, "a", "b")
		must(t, NewStore().MoveTask(ctx, tx, planIssue, 1, "a", TaskPlace{}, TaskRunning))
		assign(t, tx, planIssue, 1, "a")
		must(t, NewStore().SetJoinConflicts(ctx, tx, planIssue, 1, "a", []string{"c"}))
		assign(t, tx, planIssue, 1, "b")
		removeTask(t, tx, planIssue, 1, "b")
		must(t, NewStore().CarryPlan(ctx, tx, planIssue, 1, 2, true))
		must(t, NewStore().CarryPlan(ctx, tx, planIssue, 1, 3, false))
	})
	inTx(t, st, func(tx pgx.Tx) {
		carried := taskOf(t, tx, planIssue, 2, "a")
		if carried.Branch != "legion/LEGION-208" || !reflect.DeepEqual(carried.JoinConflicts, []string{"c"}) {
			t.Fatalf("the carried task a holds branch %q and join conflicts %v, want legion/LEGION-208 and [c]", carried.Branch, carried.JoinConflicts)
		}
		reset := taskOf(t, tx, planIssue, 3, "a")
		if reset.Branch != "" || reset.JoinConflicts != nil {
			t.Fatalf("task a of the plan started again holds branch %q and join conflicts %v, want neither", reset.Branch, reset.JoinConflicts)
		}
		putTasks(t, tx, planIssue, 2, 2, "b")
		if b := assign(t, tx, planIssue, 2, "b"); b != "legion/LEGION-208.b.2" {
			t.Fatalf("task b given again in the carried generation took %q, want legion/LEGION-208.b.2: the removed task b took legion/LEGION-208.b before the carry", b)
		}
		if a := assign(t, tx, planIssue, 3, "a"); a != "legion/LEGION-208" {
			t.Fatalf("the first task to start in the generation started from its start took %q, want the issue's branch", a)
		}
	})
	if err := store.Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		return NewStore().SetJoinConflicts(ctx, tx, planIssue, 1, "z", []string{"c"})
	}); err == nil || !strings.Contains(err.Error(), "z") {
		t.Fatalf("the join conflicts of a task the plan does not hold = %v, want a refusal naming it", err)
	}
}

// Two tasks of one issue that start at one time in two transactions never both take the issue's
// branch: the second waits for the first, then takes its own slug's branch. Without the wait both
// read the issue's branch as free, and the second fails on the first's name.
func TestTwoTasksThatStartAtOnceNeverShareTheIssuesBranch(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	inTx(t, st, func(tx pgx.Tx) { putTasks(t, tx, planIssue, 1, 1, "a", "b") })
	first, err := st.Pool().BeginTx(ctx, pgx.TxOptions{})
	must(t, err)
	defer func() { _ = first.Rollback(ctx) }() // a no-op once the transaction commits
	if a := assign(t, first, planIssue, 1, "a"); a != "legion/LEGION-208" {
		t.Fatalf("task a took %q, want the issue's branch", a)
	}
	type answer struct {
		branch string
		err    error
	}
	second := make(chan answer, 1)
	go func() {
		var got answer
		got.err = store.Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
			var err error
			got.branch, err = NewStore().AssignTaskBranch(ctx, tx, planIssue, 1, "b")
			return err
		})
		second <- got
	}()
	select {
	case got := <-second:
		t.Fatalf("task b's start answered %q, %v while task a's was open, want it to wait", got.branch, got.err)
	case <-time.After(500 * time.Millisecond):
	}
	must(t, first.Commit(ctx))
	got := <-second
	if got.err != nil || got.branch != "legion/LEGION-208.b" {
		t.Fatalf("task b's start = %q, %v; want legion/LEGION-208.b", got.branch, got.err)
	}
}
