package daemon

// A task_branch row decides which needs to join on its read of the plan, joins them on GitHub
// holding no transaction, and then writes what the join found onto the task. Each test here opens
// that window inside the row's own GitHub call, the merge of the need c, and makes there the store
// writes of a fact that can land in it, under the lock every fact applies under
// (intake.SerializeFacts), as that fact makes them. Each plan here is the state the start of tasks
// reaches with a plan of several tasks that register_plan took.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// newWindowRig is newTaskBranchRig whose fake GitHub runs during once, outside any transaction of
// the executor, when it is asked to merge task c's branch into another: between the row's read of
// the plan and its write of the join's conflicts. A nil during runs nothing.
func newWindowRig(t *testing.T, during func(rig *taskBranchRig), tasks ...plannedTask) *taskBranchRig {
	t.Helper()
	rig := newTaskBranchRig(t, tasks...)
	c := rig.branch(t, "c")
	var once sync.Once
	rig.runner.githubAPI = githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widgets/merges" && during != nil {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read the merge request: %v", err)
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			var merge struct {
				Head string `json:"head"`
			}
			if err := json.Unmarshal(body, &merge); err != nil {
				t.Errorf("decode the merge request: %v", err)
			}
			if merge.Head == c {
				once.Do(func() { during(rig) })
			}
		}
		if !rig.github.Handler(w, r) {
			githubtest.Unexpected(t, w, r)
		}
	}})
	return rig
}

// asAFact runs write in a transaction of its own that first takes the lock every fact applies
// under (intake.SerializeFacts), as each fact whose store writes it makes does. It runs on the
// fake GitHub's goroutine, so a failure is reported, not fatal.
func asAFact(t *testing.T, rig *taskBranchRig, what string, write func(ctx context.Context, tx pgx.Tx) error) {
	t.Helper()
	ctx := context.Background()
	if err := store.Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
		if err := intake.SerializeFacts(ctx, tx); err != nil {
			return err
		}
		return write(ctx, tx)
	}); err != nil {
		t.Errorf("%s: %v", what, err)
	}
}

// laterVersionDropsC is the store writes of the owner's stop of task d (taskStopped's StopTask)
// and of a later version that changes d's needs to a alone (planGiven: givenRows through PutPlan,
// then startAtFirstStep's move of the changed stopped task to its first step).
func laterVersionDropsC(ctx context.Context, rig *taskBranchRig, tx pgx.Tx) error {
	issue := taskBranchIssue
	v2 := record.PlanVersion{Issue: issue.Key, Generation: issue.Generation, Version: 2, ArtifactID: "plan-artifact", Source: record.PlanSourceOwner, GivenAt: time.Now()}
	var rows []record.PlanTask
	var steps []record.PlanStep
	for _, task := range []plannedTask{{id: "a"}, {id: "c"}, {id: "d", needs: []string{"a"}}} {
		rows = append(rows, record.PlanTask{Issue: issue.Key, Generation: issue.Generation, Task: task.id, PlanVersion: 2, BlockText: "Task " + task.id + ".",
			Needs: task.needs, State: record.TaskWaiting})
		steps = append(steps, record.PlanStep{Issue: issue.Key, Generation: issue.Generation, Task: task.id, Position: 1, Role: "implementer", Type: "implementation"})
	}
	if err := rig.records.StopTask(ctx, tx, issue.Key, issue.Generation, "d", "task d of LEGION-208 was stopped by its owner"); err != nil {
		return err
	}
	if err := rig.records.PutPlan(ctx, tx, v2, rows, steps); err != nil {
		return err
	}
	return rig.records.MoveTask(ctx, tx, issue.Key, issue.Generation, "d", record.TaskPlace{Kind: record.PlaceStep, Position: 1}, record.TaskRunning)
}

// readmitted is the store writes of admission's re-admission of the issue as an orphan root of
// its own (admit's readmit): the record at the next generation, then the plan carried there, with
// its run (carried) or from its start.
func readmitted(carried bool) func(ctx context.Context, rig *taskBranchRig, tx pgx.Tx) error {
	return func(ctx context.Context, rig *taskBranchRig, tx pgx.Tx) error {
		next := taskBranchIssue
		next.Generation++
		if err := rig.records.PutIssue(ctx, tx, next); err != nil {
			return err
		}
		return rig.records.CarryPlan(ctx, tx, next.Key, taskBranchIssue.Generation, next.Generation, carried)
	}
}

// taskAt is task id of the plan at generation, as its row holds it.
func (rig *taskBranchRig) taskAt(t *testing.T, generation uint64, id string) record.PlanTask {
	t.Helper()
	var stored *record.StoredPlan
	if err := store.Transact(context.Background(), rig.pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		stored, err = rig.records.Plan(context.Background(), tx, taskBranchIssue.Key, generation)
		return err
	}); err != nil {
		t.Fatalf("read the plan of %s at generation %d: %v", taskBranchIssue.Key, generation, err)
	}
	task, ok := stored.TaskOf(id)
	if !ok {
		t.Fatalf("the plan of %s at generation %d holds no task %s", taskBranchIssue.Key, generation, id)
	}
	return task
}

// attempt executes the task_branch row of task id at the issue's first generation once, as a lane
// does, and answers what the executor answered.
func (rig *taskBranchRig) attempt(id string) error {
	row, err := record.NewOutboxRow(taskBranchIssue.Key, record.TaskBranch{Generation: taskBranchIssue.Generation, Task: id}, time.Now())
	if err != nil {
		return err
	}
	return rig.runner.execute(context.Background(), row)
}

// conflictingNeeds gives tasks a and c work that conflicts, so the join of c into d's branch, which
// starts at a's head, is refused as a conflict.
func conflictingNeeds(t *testing.T, rig *taskBranchRig) string {
	t.Helper()
	a := rig.github.Commit(t, rig.branch(t, "a"), map[string]string{"x.txt": "from a\n"}, "a")
	rig.github.Commit(t, rig.branch(t, "c"), map[string]string{"x.txt": "from c\n"}, "c")
	return a
}

// holdsOnlyItsNeeds fails where task d's row records as a conflict the join of a need d does not
// hold.
func holdsOnlyItsNeeds(t *testing.T, rig *taskBranchRig) {
	t.Helper()
	d := rig.task(t, "d")
	for _, id := range d.JoinConflicts {
		if !slices.Contains(d.Needs, id) {
			t.Errorf("task d (version %d) needs %v and its row records the join of %s as a conflict: a need it does not hold", d.PlanVersion, d.Needs, id)
		}
	}
}

// The join's conflicts are written onto the task only where its step waits for that join: a later
// version that changes the task's needs while the row joins them on GitHub starts the task again,
// whose start queues a join of its own, so the end of the row's join changes nothing, and the next
// row joins the needs the task holds then. The controls: a conflict of a need the task holds is
// recorded, and a later version taken before the row runs is joined as it stands.
func TestTheJoinsConflictsNameOnlyNeedsTheTaskHolds(t *testing.T) {
	t.Parallel()
	t.Run("control: a conflict of a need d holds is recorded", func(t *testing.T) {
		rig := newWindowRig(t, nil, twoNeeds...)
		conflictingNeeds(t, rig)
		rig.run(t, "d")
		if got := rig.task(t, "d").JoinConflicts; !slices.Equal(got, []string{"c"}) {
			t.Fatalf("task d's join conflicts = %v, want [c]", got)
		}
		holdsOnlyItsNeeds(t, rig)
	})
	t.Run("control: the later version before the row", func(t *testing.T) {
		rig := newWindowRig(t, nil, twoNeeds...)
		conflictingNeeds(t, rig)
		asAFact(t, rig, "give the later version that drops c from d's needs", func(ctx context.Context, tx pgx.Tx) error {
			return laterVersionDropsC(ctx, rig, tx)
		})
		rig.run(t, "d")
		if d := rig.task(t, "d"); d.PlanVersion != 2 || !slices.Equal(d.Needs, []string{"a"}) || len(d.JoinConflicts) != 0 {
			t.Fatalf("task d = version %d, needs %v, join conflicts %v; want version 2, needs [a], none", d.PlanVersion, d.Needs, d.JoinConflicts)
		}
	})
	t.Run("the later version inside the row's window", func(t *testing.T) {
		rig := newWindowRig(t, func(rig *taskBranchRig) {
			asAFact(t, rig, "give the later version that drops c from d's needs", func(ctx context.Context, tx pgx.Tx) error {
				return laterVersionDropsC(ctx, rig, tx)
			})
		}, twoNeeds...)
		a := conflictingNeeds(t, rig)

		if err := rig.attempt("d"); err != nil {
			t.Fatalf("the row whose task changed while it joined answered %v, want it run: the end of its join changes nothing", err)
		}

		holdsOnlyItsNeeds(t, rig)
		if d := rig.task(t, "d"); d.StepWait != record.StepWaitJoin {
			t.Fatalf("task d after the row whose join it no longer waits for waits for %q, want its own join", d.StepWait)
		}
		rig.run(t, "d")
		d := rig.task(t, "d")
		if d.PlanVersion != 2 || !slices.Equal(d.Needs, []string{"a"}) || len(d.JoinConflicts) != 0 {
			t.Fatalf("task d after the row's next run = version %d, needs %v, join conflicts %v; want version 2, needs [a], none", d.PlanVersion, d.Needs, d.JoinConflicts)
		}
		if head := rig.github.Head(t, d.BranchOf(taskBranchIssue.Key)); head != a {
			t.Fatalf("task d's branch stands at %q, want task a's head %s", head, a)
		}
	})
}

// The join's conflicts reach the generation the run goes on at: admission's re-admission of the
// issue as an orphan root, which carries the run to a new generation in the same branches, lands
// while the row joins, so the end of the row's join, of the generation the issue left, changes
// nothing, and the row of the join the settle of the re-admission queues at the new generation
// (workflow's joinAgain) writes what its join found onto the carried task. The controls: a
// re-admission after the row carries the conflicts the row wrote, and a re-admission that starts
// the plan from its start gets none, since its task's first start there joins its needs itself.
func TestTheJoinsConflictsReachTheGenerationTheRunGoesOnAt(t *testing.T) {
	t.Parallel()
	next := taskBranchIssue.Generation + 1
	t.Run("control: the re-admission after the row", func(t *testing.T) {
		rig := newWindowRig(t, nil, twoNeeds...)
		conflictingNeeds(t, rig)
		rig.run(t, "d")
		asAFact(t, rig, "re-admit the orphan with its run", func(ctx context.Context, tx pgx.Tx) error {
			return readmitted(true)(ctx, rig, tx)
		})
		if got := rig.taskAt(t, next, "d").JoinConflicts; !slices.Equal(got, []string{"c"}) {
			t.Fatalf("task d at generation %d holds join conflicts %v, want [c]", next, got)
		}
	})
	t.Run("the re-admission that carries the run inside the row's window", func(t *testing.T) {
		rig := newWindowRig(t, func(rig *taskBranchRig) {
			asAFact(t, rig, "re-admit the orphan with its run", func(ctx context.Context, tx pgx.Tx) error {
				return readmitted(true)(ctx, rig, tx)
			})
		}, twoNeeds...)
		conflictingNeeds(t, rig)
		rig.run(t, "d")
		if got := rig.taskAt(t, next, "d").JoinConflicts; len(got) != 0 {
			t.Fatalf("task d at generation %d after the row of generation %d holds join conflicts %v, want none: that join ended at a generation the issue left",
				next, taskBranchIssue.Generation, got)
		}
		row := mustOutboxRow(t, taskBranchIssue.Key, record.TaskBranch{Generation: next, Task: "d"}, time.Now())
		if err := rig.runner.execute(context.Background(), row); err != nil {
			t.Fatalf("execute the branch row of task d at generation %d: %v", next, err)
		}
		if got := rig.taskAt(t, next, "d").JoinConflicts; !slices.Equal(got, []string{"c"}) {
			t.Fatalf("task d at generation %d, the run carried on, holds join conflicts %v after its own row; its branch lacks c's work, want [c]", next, got)
		}
	})
	t.Run("control: the re-admission that starts the plan from its start inside the row's window", func(t *testing.T) {
		rig := newWindowRig(t, func(rig *taskBranchRig) {
			asAFact(t, rig, "re-admit the orphan without its run", func(ctx context.Context, tx pgx.Tx) error {
				return readmitted(false)(ctx, rig, tx)
			})
		}, twoNeeds...)
		conflictingNeeds(t, rig)
		rig.run(t, "d")
		if d := rig.taskAt(t, next, "d"); d.State != record.TaskWaiting || d.Branch != "" || len(d.JoinConflicts) != 0 {
			t.Fatalf("task d at generation %d = %s on branch %q with join conflicts %v, want waiting with no branch and none: its first start joins its needs",
				next, d.State, d.Branch, d.JoinConflicts)
		}
	})
}

// The end of the join applies under the lock every fact applies under, not merely again: a later
// version whose fact holds that lock when the row comes to apply it is the version the end of the
// join reads, so the row waits for the fact's commit, and its end changes nothing, since the task
// no longer waits for that join. An end read beside the lock would read the version the fact has
// not committed yet, and write c's conflict onto a task that no longer needs c.
func TestTheJoinsConflictsAreJudgedUnderTheFactLock(t *testing.T) {
	t.Parallel()
	held := make(chan pgx.Tx, 1)
	rig := newWindowRig(t, func(rig *taskBranchRig) {
		ctx := context.Background()
		tx, err := rig.pool.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			t.Errorf("begin the later version's fact: %v", err)
			return
		}
		if err := intake.SerializeFacts(ctx, tx); err != nil {
			t.Errorf("take the fact lock: %v", err)
		}
		if err := laterVersionDropsC(ctx, rig, tx); err != nil {
			t.Errorf("give the later version that drops c from d's needs: %v", err)
		}
		held <- tx
	}, twoNeeds...)
	conflictingNeeds(t, rig)
	done := make(chan error, 1)
	go func() { done <- rig.attempt("d") }()
	var fact pgx.Tx
	select {
	case fact = <-held:
	case err := <-done:
		t.Fatalf("the row answered %v before the later version's fact held the lock", err)
	}
	testwait.Eventually(t, "the row's write to wait for the fact that holds the lock", func() bool {
		var waiting int
		if err := rig.pool.QueryRow(context.Background(), `select count(*) from pg_stat_activity
			where datname = current_database() and wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			return false
		}
		return waiting == 1
	})
	if err := fact.Commit(context.Background()); err != nil {
		t.Fatalf("commit the later version's fact: %v", err)
	}

	err := <-done

	holdsOnlyItsNeeds(t, rig)
	if err != nil {
		t.Errorf("the row whose task a fact changed while it waited to apply its end answered %v, want it run: its end changes nothing", err)
	}
}
