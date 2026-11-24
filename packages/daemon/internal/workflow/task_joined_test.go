package workflow

import (
	"fmt"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// joinWaits writes the step of task of the plan of key at generation, running at its first step,
// waiting for the end of its join since the engine's clock, as the start of a task writes it.
func joinWaits(t *testing.T, pool *pgxpool.Pool, key string, generation uint64, task string) {
	t.Helper()
	since := stepClock
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return record.NewStore().SetStepWait(t.Context(), tx, key, generation, task, record.StepWaitJoin, &since)
	})
}

// joinedFact is the end of the join of task d of key at generation onto d's branch from its need
// c, in which GitHub refused c as a conflict.
func joinedFact(key string, generation uint64) intake.TaskJoined {
	return intake.TaskJoined{Issue: key, Generation: generation, Task: "d", Branch: "legion/" + key + ".d", Needs: []string{"c"}, Conflicts: []string{"c"}}
}

// The end of a join is a fact that starts the step that waits for it: task d, `implementer`, needs
// c, which is done, and runs at its first step, which waits for its join. The end of the join of c
// into d's branch, which GitHub refused as a conflict, writes c as d's join conflict, ends d's wait
// and starts d's step in the same transaction: its start is counted and queued once, for d's agent.
// A fact of an earlier generation, and one of a join onto another branch or from other needs (a row
// a stop and a later version of d left behind, which the start again replaced with a row of its
// own), change nothing; so do the same fact again and the end of another row of the same join, since
// d's step waits for nothing then.
//
// The fault it catches is a join that ends and starts nothing, so its task waits for ever (its
// control: the handler without its start: no start is queued, and the test fails).
func TestTheEndOfAJoinStartsTheStepThatWaits(t *testing.T) {
	pool := migratedPool(t)
	engine, _ := tickingEngine()
	root := stepRoot(t, pool, phase.Implementing)
	seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "c", steps: []string{"tester"}, agents: []claim.Agent{"tester"}, state: record.TaskDone, place: atStep(1)},
		planTask{id: "d", steps: []string{"implementer"}, agents: []claim.Agent{"implementer"}, needs: []string{"c"}, state: record.TaskRunning, place: atStep(1)})
	seedTaskBranch(t, pool, root.Key, "d", "legion/"+root.Key+".d")
	joinWaits(t, pool, root.Key, root.Generation, "d")
	apply := applyFacts(t, pool, engine)
	unchanged := func(what string) {
		t.Helper()
		tasks := tasksOf(t, pool, engine, root.Key)
		assertWaits(t, tasks, "d", atStep(1), record.StepWaitJoin)
		if conflicts := tasks["d"].JoinConflicts; len(conflicts) != 0 {
			t.Fatalf("after %s task d holds the join conflicts %v, want none", what, conflicts)
		}
		if starts := roleStartsOf(t, pool, root.Key, claim.RoleImplementer); len(starts) != 0 {
			t.Fatalf("after %s the implementer's starts of %s = %+v, want none: d's step waits for its join", what, root.Key, starts)
		}
		assertNothingStarted(t, pool, root.Key, "d", atStep(1))
	}

	earlier := joinedFact(root.Key, root.Generation-1)
	mustSpend(t, "the end of a join of an earlier generation", apply("task-joined:earlier", earlier))
	unchanged("the end of a join of an earlier generation")
	otherBranch := joinedFact(root.Key, root.Generation)
	otherBranch.Branch = "legion/" + root.Key
	mustSpend(t, "the end of a join onto another branch", apply("task-joined:other-branch", otherBranch))
	unchanged("the end of a join onto another branch")
	otherNeeds := joinedFact(root.Key, root.Generation)
	otherNeeds.Needs = []string{"b", "c"}
	mustSpend(t, "the end of a join from other needs", apply("task-joined:other-needs", otherNeeds))
	unchanged("the end of a join from other needs")

	mustSpend(t, "the end of d's join", apply("task-joined:1", joinedFact(root.Key, root.Generation)))

	tasks := tasksOf(t, pool, engine, root.Key)
	assertWaits(t, tasks, "d", atStep(1), record.StepWaitNone)
	if conflicts := tasks["d"].JoinConflicts; !slices.Equal(conflicts, []string{"c"}) {
		t.Fatalf("task d's join conflicts = %v, want [c]", conflicts)
	}
	starts := roleStartsOf(t, pool, root.Key, claim.RoleImplementer)
	if len(starts) != 1 {
		t.Fatalf("the implementer's starts of %s = %+v, want d's one", root.Key, starts)
	}
	assertStartOf(t, starts[0], root.Key, "d", "implementer")
	counted := countedStarts(t, pool, root.Key, "d", atStep(1))

	for _, id := range []string{"task-joined:1", "task-joined:2"} {
		apply(id, joinedFact(root.Key, root.Generation))
		if starts := roleStartsOf(t, pool, root.Key, claim.RoleImplementer); len(starts) != 1 {
			t.Fatalf("after the end of d's join applied again as %s the implementer's starts = %+v, want d's one", id, starts)
		}
		if again := countedStarts(t, pool, root.Key, "d", atStep(1)); again != counted {
			t.Fatalf("after the end of d's join applied again as %s d's step counts the starts %q, want %q", id, again, counted)
		}
	}
}

// Where the agent of the step whose join ends runs a step of another task, the step waits for its
// agent instead: task m runs its step of agent k, and d's step, of agent k too, waits for its join.
// The end of d's join writes its conflicts and moves its wait to its agent: no start of d's step is
// counted or queued.
func TestTheEndOfAJoinWhoseAgentIsBusyWaitsForTheAgent(t *testing.T) {
	pool := migratedPool(t)
	engine, _ := tickingEngine()
	root := stepRoot(t, pool, phase.Implementing)
	seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "c", steps: []string{"reviewer"}, agents: []claim.Agent{"reviewer"}, state: record.TaskDone, place: atStep(1)},
		planTask{id: "d", steps: []string{"tester:k"}, agents: []claim.Agent{"k"}, needs: []string{"c"}, state: record.TaskRunning, place: atStep(1)},
		planTask{id: "m", steps: []string{"implementer:k"}, agents: []claim.Agent{"k"}, state: record.TaskRunning, place: atStep(1)})
	seedTaskBranch(t, pool, root.Key, "d", "legion/"+root.Key+".d")
	joinWaits(t, pool, root.Key, root.Generation, "d")

	mustSpend(t, "the end of d's join", applyFacts(t, pool, engine)("task-joined:1", joinedFact(root.Key, root.Generation)))

	tasks := tasksOf(t, pool, engine, root.Key)
	assertWaits(t, tasks, "d", atStep(1), record.StepWaitAgent)
	if conflicts := tasks["d"].JoinConflicts; !slices.Equal(conflicts, []string{"c"}) {
		t.Fatalf("task d's join conflicts = %v, want [c]", conflicts)
	}
	if starts := roleStartsOf(t, pool, root.Key, claim.RoleTester); len(starts) != 0 {
		t.Fatalf("the tester's starts of %s = %+v, want none: k runs m's step", root.Key, starts)
	}
	assertNothingStarted(t, pool, root.Key, "d", atStep(1))
}

// countedStarts is the count of starts of the step of task of key at place, and its run, as the
// store holds them, which fails the test where the step counts none.
func countedStarts(t *testing.T, pool *pgxpool.Pool, key, task string, place record.TaskPlace) string {
	t.Helper()
	starts, err := startRows(t.Context(), pool)
	if err != nil {
		t.Fatalf("read the counts of starts: %v", err)
	}
	count, ok := starts[fmt.Sprintf("%s %s %s %d", key, task, place.Kind, place.Position)]
	if !ok {
		t.Fatalf("the step of task %s at %+v counts no start, want its start counted", task, place)
	}
	return count
}
