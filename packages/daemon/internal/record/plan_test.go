package record

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

const (
	planOwner   claim.Token = "legion-legion-legion-208-architect"
	otherOwner  claim.Token = "legion-legion-legion-300-architect"
	planIssue               = "LEGION-208"
	unrecorded              = "LEGION-999"
	planAuthor              = "artifact-plan-208"
	planStepOne             = "implementer"
)

var planGivenAt = time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC)

// ownersPlan is a plan of one task, a, with two steps, the implementer and the reviewer, as an
// owner gives it at version: every field of each of its rows is written, so a round trip that
// drops a column is a round trip this test can fail for.
func ownersPlan(issue string, generation uint64, version int, givenBy claim.Token, at time.Time) (PlanVersion, []PlanTask, []PlanStep) {
	v := PlanVersion{Issue: issue, Generation: generation, Version: version, ArtifactID: planAuthor, Source: PlanSourceOwner, GivenBy: givenBy, GivenAt: at}
	tasks := []PlanTask{{
		Issue: issue, Generation: generation, Task: "a", PlanVersion: version, BlockText: "Build the widget.",
		Needs: []string{"b"}, PartOf: "widgets", OwnerRole: "architect", Merges: true, StepsByDefault: true,
		State: TaskWaiting,
	}}
	steps := []PlanStep{
		{Issue: issue, Generation: generation, Task: "a", Position: 1, Role: planStepOne, Type: "implementation", AgentKey: "fast", Agent: "implementer"},
		{Issue: issue, Generation: generation, Task: "a", Position: 2, Role: "reviewer", Type: "review", Agent: "reviewer"},
	}
	return v, tasks, steps
}

// storedOf is what Plan reads for the rows ownersPlan writes.
func storedOf(v PlanVersion, tasks []PlanTask, steps []PlanStep) *StoredPlan {
	plan := &StoredPlan{Version: v, Tasks: tasks, Steps: map[string][]PlanStep{}}
	for _, step := range steps {
		plan.Steps[step.Task] = append(plan.Steps[step.Task], step)
	}
	return plan
}

func putPlan(t *testing.T, tx pgx.Tx, v PlanVersion, tasks []PlanTask, steps []PlanStep) {
	t.Helper()
	if err := NewStore().PutPlan(context.Background(), tx, v, tasks, steps); err != nil {
		t.Fatalf("put version %d of the plan of %s at generation %d: %v", v.Version, v.Issue, v.Generation, err)
	}
}

func readPlan(t *testing.T, tx pgx.Tx, issue string, generation uint64) *StoredPlan {
	t.Helper()
	plan, err := NewStore().Plan(context.Background(), tx, issue, generation)
	if err != nil {
		t.Fatalf("read the plan of %s at generation %d: %v", issue, generation, err)
	}
	return plan
}

// The plan of an issue is three tables keyed by the issue and its generation: one task and its two
// steps read back as they were written, every column of each, and the generation beside it reads
// no plan at all. A plan given before the daemon records its issue is written at generation 0, and
// no row of the issues table is needed for it: the plan names an issue the daemon has not recorded.
func TestPlanRowsRoundTrip(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	issue := issueFixture(planIssue)
	v, tasks, steps := ownersPlan(planIssue, issue.Generation, 4, planOwner, planGivenAt)
	tasks[0].State, tasks[0].StoppedReason = TaskStopped, "task a of LEGION-208 was stopped by its owner"
	tasks[0].Place = TaskPlace{Kind: PlaceStep, Position: 2}
	tasks[0].Counts = ReturnCounts{Pass: 3, Rounds: 2, ConflictRounds: 1, BackwardMoves: map[string]int{"reviewing-implementing": 2}, CapTold: 3}
	early, earlyTasks, earlySteps := ownersPlan(unrecorded, 0, 1, otherOwner, planGivenAt.Add(time.Hour))

	inTx(t, st, func(tx pgx.Tx) {
		must(t, NewStore().PutIssue(ctx, tx, issue))
		putPlan(t, tx, v, tasks, steps)
		putPlan(t, tx, early, earlyTasks, earlySteps)
	})
	inTx(t, st, func(tx pgx.Tx) {
		if got, want := readPlan(t, tx, planIssue, issue.Generation), storedOf(v, tasks, steps); !reflect.DeepEqual(got, want) {
			t.Fatalf("the plan of %s = %#v, want %#v", planIssue, got, want)
		}
		if got := readPlan(t, tx, planIssue, issue.Generation+1); got != nil {
			t.Fatalf("the plan of %s at the next generation = %#v, want none: no plan was given there", planIssue, got)
		}
		if got, want := readPlan(t, tx, unrecorded, 0), storedOf(early, earlyTasks, earlySteps); !reflect.DeepEqual(got, want) {
			t.Fatalf("the plan of unrecorded %s at generation 0 = %#v, want %#v", unrecorded, got, want)
		}
		recorded, err := NewStore().Issue(ctx, tx, unrecorded)
		must(t, err)
		if recorded != nil {
			t.Fatalf("%s is recorded as %#v, want no record: the fixture must hold a plan of an issue the daemon has not recorded", unrecorded, recorded)
		}
	})
}

// PlanStepsOf is every plan_steps row of an issue, of every generation, each with the name of its
// agent, ordered by generation, task and place: what the naming of a version's agents reads before
// the version is written, so a key keeps the name an earlier generation gave it. Another issue's
// rows are not among them.
func TestPlanStepsOfReadsEveryGenerationsStepsWithTheirNames(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	issue := issueFixture(planIssue)
	issue.Generation = 2
	first, firstTasks, firstSteps := ownersPlan(planIssue, 1, 1, planOwner, planGivenAt)
	second, secondTasks, secondSteps := ownersPlan(planIssue, 2, 1, planOwner, planGivenAt.Add(time.Hour))
	secondTasks[0].Task = "b"
	for i := range secondSteps {
		secondSteps[i].Task = "b"
	}
	secondSteps[0].Agent = "implementer-of-b"
	other, otherTasks, otherSteps := ownersPlan(unrecorded, 2, 1, otherOwner, planGivenAt)
	inTx(t, st, func(tx pgx.Tx) {
		must(t, NewStore().PutIssue(ctx, tx, issue))
		putPlan(t, tx, second, secondTasks, secondSteps)
		putPlan(t, tx, first, firstTasks, firstSteps)
		putPlan(t, tx, other, otherTasks, otherSteps)
	})
	inTx(t, st, func(tx pgx.Tx) {
		got, err := NewStore().PlanStepsOf(ctx, tx, planIssue)
		must(t, err)
		if want := append(append([]PlanStep{}, firstSteps...), secondSteps...); !reflect.DeepEqual(got, want) {
			t.Fatalf("the steps of %s = %#v, want every generation's, in order: %#v", planIssue, got, want)
		}
	})
}

// A later version is the plan in force: Plan reads the highest version, the tasks it gives are
// written over the tasks of the same id, and their steps are the steps it gives, so a step the
// later version leaves out is gone. The same version given twice is refused, and so is a step of a
// task the version does not give, whose steps the write would otherwise leave beside the task.
func TestALaterVersionIsThePlanInForceAndItsTasksAreItsOwn(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	v1, tasks1, steps1 := ownersPlan(planIssue, 3, 1, planOwner, planGivenAt)
	v2, tasks2, steps2 := ownersPlan(planIssue, 3, 2, planOwner, planGivenAt.Add(time.Hour))
	tasks2[0].BlockText = "Build the widget, then its gadget."
	steps2 = steps2[:1]

	inTx(t, st, func(tx pgx.Tx) {
		putPlan(t, tx, v1, tasks1, steps1)
		putPlan(t, tx, v2, tasks2, steps2)
		if got, want := readPlan(t, tx, planIssue, 3), storedOf(v2, tasks2, steps2); !reflect.DeepEqual(got, want) {
			t.Fatalf("the plan after version 2 = %#v, want version 2's task and its one step: %#v", got, want)
		}
	})
	inTx(t, st, func(tx pgx.Tx) {
		if err := NewStore().PutPlan(ctx, tx, v2, tasks2, steps2); err == nil {
			t.Fatalf("version 2 given a second time was stored, want it refused")
		}
	})
	v3, tasks3, steps3 := ownersPlan(planIssue, 3, 3, planOwner, planGivenAt.Add(2*time.Hour))
	steps3[1].Task = "b"
	inTx(t, st, func(tx pgx.Tx) {
		if err := NewStore().PutPlan(ctx, tx, v3, tasks3, steps3); err == nil || !strings.Contains(err.Error(), "task b") {
			t.Fatalf("a step of a task version 3 does not give = %v, want it refused naming the task", err)
		}
	})
}

// A phases row carries the step its role runs, the task and the step's place in it, which enter
// writes for an issue with plan rows (record.PhaseRow.StepTask and StepPlace). A row written with
// neither, which is the row of every issue with no plan rows, reads no task and place 0.
func TestAPhaseRowCarriesItsStep(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	reviewer := PhaseRow{Issue: planIssue, Role: claim.RoleReviewer, Claim: "legion-legion-legion-208-reviewer", Entry: 2, StepTask: "a", StepPlace: 2}
	implementer := PhaseRow{Issue: planIssue, Role: claim.RoleImplementer, Claim: "legion-legion-legion-208-implementer", Entry: 1}
	inTx(t, st, func(tx pgx.Tx) {
		must(t, NewStore().PutIssue(ctx, tx, issueFixture(planIssue)))
		must(t, NewStore().PutPhase(ctx, tx, reviewer))
		must(t, NewStore().PutPhase(ctx, tx, implementer))
	})
	inTx(t, st, func(tx pgx.Tx) {
		rows, err := NewStore().Phases(ctx, tx, planIssue)
		must(t, err)
		if got := PhaseRowOf(rows, claim.RoleReviewer); got.StepTask != "a" || got.StepPlace != 2 {
			t.Fatalf("the reviewer's row runs step %d of task %q, want step 2 of task a as it was written", got.StepPlace, got.StepTask)
		}
		if got := PhaseRowOf(rows, claim.RoleImplementer); got.StepTask != "" || got.StepPlace != 0 {
			t.Fatalf("the implementer's row runs step %d of task %q, want no step: it was written with none", got.StepPlace, got.StepTask)
		}
	})
}

// The step a phases row runs is a step of one generation's plan, whose task ids the next
// generation's plan need not share, so a new generation clears it with the entry's task: the row
// reads no step until the new generation's own start of the role writes one. Its entry stays, as
// ClearGeneration keeps each role's highest entry, into which it folds the role's other rows. The
// row reads its step before the clear, which is the control: a store that never kept the step
// would read none after it too.
func TestANewGenerationClearsTheStepOfAPhaseRow(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	issue := issueFixture(planIssue)
	row := PhaseRow{Issue: planIssue, Role: claim.RoleReviewer, Claim: "legion-legion-legion-208-reviewer", Entry: 2, EntryTask: "Review it.", StepTask: "a", StepPlace: 2}
	inTx(t, st, func(tx pgx.Tx) {
		must(t, NewStore().PutIssue(ctx, tx, issue))
		must(t, NewStore().PutPhase(ctx, tx, row))
		rows, err := NewStore().Phases(ctx, tx, planIssue)
		must(t, err)
		if got := PhaseRowOf(rows, claim.RoleReviewer); got.StepTask != "a" || got.StepPlace != 2 {
			t.Fatalf("the reviewer's row before the new generation runs step %d of task %q, want step 2 of task a as it was written", got.StepPlace, got.StepTask)
		}
		must(t, NewStore().ClearGeneration(ctx, tx, planIssue, planGivenAt))
	})
	inTx(t, st, func(tx pgx.Tx) {
		rows, err := NewStore().Phases(ctx, tx, planIssue)
		must(t, err)
		got := PhaseRowOf(rows, claim.RoleReviewer)
		if got.Entry != 2 {
			t.Fatalf("the reviewer's entry after a new generation = %d, want 2 kept", got.Entry)
		}
		if got.StepTask != "" || got.StepPlace != 0 {
			t.Fatalf("the reviewer's row after a new generation runs step %d of task %q, want none", got.StepPlace, got.StepTask)
		}
	})
}

// A run of a child again, a person's todo on it and a re-admission of a root start the owner's plan
// from its start (CarryPlan with carried false): the new generation holds the same versions and
// the same steps, each task waiting at no act with every count 0; the old generation's rows stay
// as they were. The count of starts of each place is not the plan's: it lives in step_starts alone
// (record.StepPlace), which the copy does not touch.
func TestARerunCopiesTheOwnersPlanFresh(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	issue := issueFixture(planIssue)
	v, tasks, steps := ownersPlan(planIssue, 1, 2, planOwner, planGivenAt)
	tasks[0].Task, steps[0].Task, steps[1].Task = "c", "c", "c"
	tasks[0].State, tasks[0].Place = TaskRunning, TaskPlace{Kind: PlaceStep, Position: 2}
	tasks[0].Counts = ReturnCounts{Pass: 2, Rounds: 1, BackwardMoves: map[string]int{"reviewing-implementing": 1}, CapTold: 1}
	inTx(t, st, func(tx pgx.Tx) {
		must(t, NewStore().PutIssue(ctx, tx, issue))
		putPlan(t, tx, v, tasks, steps)
		must(t, NewStore().CarryPlan(ctx, tx, planIssue, 1, 2, false))
	})

	fresh := v
	fresh.Generation = 2
	freshTask := tasks[0]
	freshTask.Generation, freshTask.State, freshTask.Place = 2, TaskWaiting, TaskPlace{}
	freshTask.Counts = ReturnCounts{}
	var freshSteps []PlanStep
	for _, step := range steps {
		step.Generation = 2
		freshSteps = append(freshSteps, step)
	}
	inTx(t, st, func(tx pgx.Tx) {
		if got, want := readPlan(t, tx, planIssue, 2), storedOf(fresh, []PlanTask{freshTask}, freshSteps); !reflect.DeepEqual(got, want) {
			t.Fatalf("the plan of the new generation = %#v, want the owner's plan from its start: %#v", got, want)
		}
		if got, want := readPlan(t, tx, planIssue, 1), storedOf(v, tasks, steps); !reflect.DeepEqual(got, want) {
			t.Fatalf("the plan of the old generation after the copy = %#v, want it as it was: %#v", got, want)
		}
	})
}

// A hand-over that carries its orphan's run (CarryPlan with carried true) carries where each task
// stands: its state, its place and its counts, so the orphan goes on at the step it stood at. An
// issue that ran before the release has no plan rows, and the carry writes none for it: it goes on
// as today.
func TestACarriedPlanKeepsWhereEachTaskStands(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	v, tasks, steps := ownersPlan(planIssue, 1, 2, planOwner, planGivenAt)
	tasks[0].State, tasks[0].Place = TaskRunning, TaskPlace{Kind: PlaceStep, Position: 2}
	tasks[0].Counts = ReturnCounts{Pass: 2, Rounds: 1, ConflictRounds: 1, BackwardMoves: map[string]int{"reviewing-implementing": 1}, CapTold: 1}
	inTx(t, st, func(tx pgx.Tx) {
		putPlan(t, tx, v, tasks, steps)
		must(t, NewStore().CarryPlan(ctx, tx, planIssue, 1, 2, true))
		must(t, NewStore().CarryPlan(ctx, tx, "LEGION-OLD", 1, 2, true))
	})
	carried := v
	carried.Generation = 2
	carriedTask := tasks[0]
	carriedTask.Generation = 2
	var carriedSteps []PlanStep
	for _, step := range steps {
		step.Generation = 2
		carriedSteps = append(carriedSteps, step)
	}
	inTx(t, st, func(tx pgx.Tx) {
		if got, want := readPlan(t, tx, planIssue, 2), storedOf(carried, []PlanTask{carriedTask}, carriedSteps); !reflect.DeepEqual(got, want) {
			t.Fatalf("the carried plan = %#v, want every task where it stood: %#v", got, want)
		}
		if got := readPlan(t, tx, "LEGION-OLD", 2); got != nil {
			t.Fatalf("the carry of an issue with no plan rows wrote %#v, want nothing", got)
		}
	})
}

// The owners' newest plan is the newest plan in force that an owner of the issue gave, in any
// generation, the generation 0 of a plan given before the daemon recorded the issue among them.
// An owner is an architect of the issue's tree now (ArchitectOfTree): a plan another tree's
// architect gave is not an owner's, however new, until that architect's issue is a member of the
// tree, and neither is a plan of the default steps, which nobody gave. An issue of a tree whose
// architects gave none has none.
func TestOwnersNewestPlanReadsOnlyAnOwnersGift(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	issue := issueFixture(planIssue)
	given, givenTasks, givenSteps := ownersPlan(planIssue, 0, 3, planOwner, planGivenAt)
	another, anotherTasks, anotherSteps := ownersPlan(planIssue, 1, 5, otherOwner, planGivenAt.Add(time.Hour))
	defaults := PlanVersion{Issue: planIssue, Generation: 2, Version: 0, Source: PlanSourceDefault, GivenAt: planGivenAt.Add(2 * time.Hour)}
	defaultTasks := []PlanTask{{Issue: planIssue, Generation: 2, Task: "default", BlockText: "The project's default steps.", State: TaskWaiting}}
	defaultSteps := []PlanStep{{Issue: planIssue, Generation: 2, Task: "default", Position: 1, Role: planStepOne, Type: "implementation"}}
	inTx(t, st, func(tx pgx.Tx) {
		putPlan(t, tx, given, givenTasks, givenSteps)
		putPlan(t, tx, another, anotherTasks, anotherSteps)
		putPlan(t, tx, defaults, defaultTasks, defaultSteps)
	})
	newest := func(t *testing.T, issue Issue) *StoredPlan {
		t.Helper()
		var got *StoredPlan
		inTx(t, st, func(tx pgx.Tx) {
			var err error
			got, err = NewStore().OwnersNewestPlan(ctx, tx, issue)
			must(t, err)
		})
		return got
	}

	if got, want := newest(t, issue), storedOf(given, givenTasks, givenSteps); !reflect.DeepEqual(got, want) {
		t.Fatalf("the owners' newest plan = %#v, want the one the root's architect gave at generation 0: %#v", got, want)
	}
	member := issueFixture("LEGION-300")
	member.Tree = planIssue
	inTx(t, st, func(tx pgx.Tx) { must(t, NewStore().PutIssue(ctx, tx, member)) })
	if got, want := newest(t, issue), storedOf(another, anotherTasks, anotherSteps); !reflect.DeepEqual(got, want) {
		t.Fatalf("the owners' newest plan with LEGION-300 a member = %#v, want the one its sub-architect gave at generation 1: %#v", got, want)
	}
	elsewhere := issue
	elsewhere.Tree = "LEGION-400"
	if got := newest(t, elsewhere); got != nil {
		t.Fatalf("the owners' newest plan of %s in tree LEGION-400 = %#v, want none: no architect of that tree gave one", planIssue, got)
	}
}

// An architect of an issue's tree now is the architect of the tree's root, or of a member the
// record holds in that tree, a sub-architect: the one rule of who owns an issue, which an
// architect's call on a recorded issue and the giver of a plan at the issue's first step are both
// held to. The architect of another tree is not, nor the architect of an issue that stood in the
// tree and now stands in a tree of its own, nor a claim that is no architect's.
func TestArchitectOfTreeIsTheRootsOrAMembersArchitect(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	member, moved, issue := issueFixture("LEGION-209"), issueFixture("LEGION-210"), issueFixture("LEGION-211")
	member.Tree, issue.Tree = planIssue, planIssue
	inTx(t, st, func(tx pgx.Tx) {
		for _, recorded := range []Issue{issueFixture(planIssue), member, moved, issue} {
			must(t, NewStore().PutIssue(ctx, tx, recorded))
		}
		for architect, want := range map[claim.Token]bool{
			planOwner:                              true,
			"legion-legion-legion-209-architect":   true,
			"legion-legion-legion-210-architect":   false,
			otherOwner:                             false,
			"legion-legion-legion-208-implementer": false,
			"":                                     false,
		} {
			got, err := ArchitectOfTree(ctx, NewStore(), tx, architect, issue)
			must(t, err)
			if got != want {
				t.Errorf("ArchitectOfTree(%q, LEGION-211 of the tree %s) = %t, want %t", architect, planIssue, got, want)
			}
		}
	})
}

// The plans of earlier owners a first step passes over are each plan of the issue in force whose
// giver is no architect of the issue's tree now (ArchitectOfTree), with its version and its giver,
// in the order they were given: a plan of the tree root's architect is not among them, nor a plan
// of a sub-architect, nor a plan of the default steps, which nobody gave. Once the issue stands in
// a tree of its own, each of those owners' plans is an earlier owner's. (A first step in a group
// that takes no plan passes its owner's plans over too, which workflow's passOverOwners reads.)
func TestPlansOfEarlierOwnersLeaveOutEachArchitectOfTheTree(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	const key = "LEGION-210"
	member, issue := issueFixture("LEGION-209"), issueFixture(key)
	member.Tree, issue.Tree = planIssue, planIssue
	earlier, earlierTasks, earlierSteps := ownersPlan(key, 0, 2, otherOwner, planGivenAt)
	bySub, bySubTasks, bySubSteps := ownersPlan(key, 1, 3, "legion-legion-legion-209-architect", planGivenAt.Add(time.Hour))
	own, ownTasks, ownSteps := ownersPlan(key, 2, 4, planOwner, planGivenAt.Add(2*time.Hour))
	defaults := PlanVersion{Issue: key, Generation: 3, Version: 0, Source: PlanSourceDefault, GivenAt: planGivenAt.Add(3 * time.Hour)}
	defaultTasks := []PlanTask{{Issue: key, Generation: 3, Task: "default", BlockText: "The project's default steps.", State: TaskWaiting}}
	inTx(t, st, func(tx pgx.Tx) {
		for _, recorded := range []Issue{issueFixture(planIssue), member, issue} {
			must(t, NewStore().PutIssue(ctx, tx, recorded))
		}
		putPlan(t, tx, earlier, earlierTasks, earlierSteps)
		putPlan(t, tx, bySub, bySubTasks, bySubSteps)
		putPlan(t, tx, own, ownTasks, ownSteps)
		putPlan(t, tx, defaults, defaultTasks, nil)
	})
	inTx(t, st, func(tx pgx.Tx) {
		got, err := NewStore().PlansOfEarlierOwners(ctx, tx, issue)
		must(t, err)
		if want := []PlanVersion{earlier}; !reflect.DeepEqual(got, want) {
			t.Fatalf("the plans of earlier owners of %s in the tree %s = %#v, want the other tree's alone: %#v", key, planIssue, got, want)
		}
		alone := issue
		alone.Tree = key
		got, err = NewStore().PlansOfEarlierOwners(ctx, tx, alone)
		must(t, err)
		if want := []PlanVersion{earlier, bySub, own}; !reflect.DeepEqual(got, want) {
			t.Fatalf("the plans of earlier owners of %s in a tree of its own = %#v, want every owner's: %#v", key, got, want)
		}
	})
}

// DeletePlan deletes one plan, every row of its generation in each of the three tables, and leaves
// the plan of each other generation as it was.
func TestDeletePlanRemovesTheThreeTables(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	gone, goneTasks, goneSteps := ownersPlan(planIssue, 0, 2, otherOwner, planGivenAt)
	later, laterTasks, laterSteps := ownersPlan(planIssue, 0, 3, otherOwner, planGivenAt.Add(time.Minute))
	kept, keptTasks, keptSteps := ownersPlan(planIssue, 1, 3, planOwner, planGivenAt.Add(time.Hour))
	inTx(t, st, func(tx pgx.Tx) {
		putPlan(t, tx, gone, goneTasks, goneSteps)
		putPlan(t, tx, later, laterTasks, laterSteps)
		putPlan(t, tx, kept, keptTasks, keptSteps)
		must(t, NewStore().DeletePlan(ctx, tx, planIssue, 0))
	})
	inTx(t, st, func(tx pgx.Tx) {
		for _, table := range []string{"issue_plans", "plan_tasks", "plan_steps"} {
			var left int
			must(t, tx.QueryRow(ctx, "select count(*) from "+table+" where issue = $1 and generation = 0", planIssue).Scan(&left))
			if left != 0 {
				t.Fatalf("%s holds %d rows of the deleted plan, want none", table, left)
			}
		}
		if got, want := readPlan(t, tx, planIssue, 1), storedOf(kept, keptTasks, keptSteps); !reflect.DeepEqual(got, want) {
			t.Fatalf("the plan of generation 1 after the delete of generation 0's = %#v, want it as it was: %#v", got, want)
		}
	})
}

// DeleteTasks deletes each task it names with its steps, the act of a later version that removes a
// waiting or a stopped task, and leaves the plan's version and every other task as they were.
func TestDeleteTasksRemovesTheirStepsAndLeavesTheRest(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	v, tasks, steps := ownersPlan(planIssue, 1, 2, planOwner, planGivenAt)
	b := tasks[0]
	b.Task, b.Needs, b.PartOf, b.OwnerRole, b.Merges = "b", nil, "", "", false
	bStep := PlanStep{Issue: planIssue, Generation: 1, Task: "b", Position: 1, Role: planStepOne, Type: "implementation"}
	inTx(t, st, func(tx pgx.Tx) {
		putPlan(t, tx, v, append(tasks, b), append(steps, bStep))
		must(t, NewStore().DeleteTasks(ctx, tx, planIssue, 1, []string{"a"}))
	})
	inTx(t, st, func(tx pgx.Tx) {
		if got, want := readPlan(t, tx, planIssue, 1), storedOf(v, []PlanTask{b}, []PlanStep{bStep}); !reflect.DeepEqual(got, want) {
			t.Fatalf("the plan after task a was deleted = %#v, want task b and its step alone: %#v", got, want)
		}
	})
}

// MoveTask writes where one task stands and its state, the place one function of the walk keeps
// (workflow's moveTask): a task at a step, at its retro or at its merge, or at no act. A move takes
// a stopped task's reason away, since the task is no longer stopped: the one move of a stopped
// task that keeps it in the workflow is its start again at step 1 (workflow's restartTask). A stop
// is StopTask's, which keeps the place and records why. A task the plan does not hold, a place no
// plan has and a stop without its reason are refused.
func TestMoveTaskAndStopTaskWriteWhereATaskStands(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	v, tasks, steps := ownersPlan(planIssue, 1, 2, planOwner, planGivenAt)
	records := NewStore()
	inTx(t, st, func(tx pgx.Tx) {
		putPlan(t, tx, v, tasks, steps)
	})
	stands := func(tx pgx.Tx) PlanTask {
		t.Helper()
		plan := readPlan(t, tx, planIssue, 1)
		if plan == nil || len(plan.Tasks) != 1 {
			t.Fatalf("the plan = %#v, want its one task", plan)
		}
		return plan.Tasks[0]
	}
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.MoveTask(ctx, tx, planIssue, 1, "a", TaskPlace{Kind: PlaceStep, Position: 2}, TaskRunning))
		if got := stands(tx); got.State != TaskRunning || got.Place != (TaskPlace{Kind: PlaceStep, Position: 2}) {
			t.Fatalf("task a after its move = %s at %+v, want running at step 2", got.State, got.Place)
		}
		const why = "task a of LEGION-208 was stopped by its owner"
		must(t, records.StopTask(ctx, tx, planIssue, 1, "a", why))
		if got := stands(tx); got.State != TaskStopped || got.StoppedReason != why || got.Place != (TaskPlace{Kind: PlaceStep, Position: 2}) {
			t.Fatalf("task a after its stop = %s (%q) at %+v, want stopped at step 2 with its reason", got.State, got.StoppedReason, got.Place)
		}
		must(t, records.MoveTask(ctx, tx, planIssue, 1, "a", TaskPlace{Kind: PlaceStep, Position: 1}, TaskRunning))
		if got := stands(tx); got.State != TaskRunning || got.StoppedReason != "" || got.Place != (TaskPlace{Kind: PlaceStep, Position: 1}) {
			t.Fatalf("task a after its start again at step 1 = %s (%q) at %+v, want running at step 1 with no reason", got.State, got.StoppedReason, got.Place)
		}
		must(t, records.MoveTask(ctx, tx, planIssue, 1, "a", TaskPlace{Kind: PlaceStep, Position: 2}, TaskRunning))
		must(t, records.MoveTask(ctx, tx, planIssue, 1, "a", TaskPlace{Kind: PlaceRetro}, TaskRunning))
		if got := stands(tx); got.State != TaskRunning || got.Place != (TaskPlace{Kind: PlaceRetro}) {
			t.Fatalf("task a after its move to its retro = %s at %+v, want running at its retro", got.State, got.Place)
		}
	})
	for _, tc := range []struct {
		name string
		act  func(tx pgx.Tx) error
	}{
		{"a task the plan does not hold", func(tx pgx.Tx) error {
			return records.MoveTask(ctx, tx, planIssue, 1, "z", TaskPlace{Kind: PlaceStep, Position: 1}, TaskRunning)
		}},
		{"a step at place 0", func(tx pgx.Tx) error {
			return records.MoveTask(ctx, tx, planIssue, 1, "a", TaskPlace{Kind: PlaceStep}, TaskRunning)
		}},
		{"a retro at a place", func(tx pgx.Tx) error {
			return records.MoveTask(ctx, tx, planIssue, 1, "a", TaskPlace{Kind: PlaceRetro, Position: 1}, TaskRunning)
		}},
		{"a stop by a move", func(tx pgx.Tx) error {
			return records.MoveTask(ctx, tx, planIssue, 1, "a", TaskPlace{Kind: PlaceStep, Position: 1}, TaskStopped)
		}},
		{"a stop without its reason", func(tx pgx.Tx) error {
			return records.StopTask(ctx, tx, planIssue, 1, "a", "")
		}},
		{"a stop of a task the plan does not hold", func(tx pgx.Tx) error {
			return records.StopTask(ctx, tx, planIssue, 1, "z", "stopped")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := st.Pool().BeginTx(ctx, pgx.TxOptions{})
			must(t, err)
			defer func() { _ = tx.Rollback(ctx) }()
			if err := tc.act(tx); err == nil {
				t.Fatalf("%s was written, want it refused", tc.name)
			}
		})
	}
}

// A task whose issue leaves the workflow before the task is done is left where it stands: MoveTask
// writes the state left at its place, and nothing starts it again, neither a stop (StopTask) nor a
// move to any other state, since a todo opens a new run with rows of its own. The same stop and the
// same move of the task while it still runs are written: that is the control, which shows each
// refusal is the state's and not a write the task could not take.
func TestATaskThatLeftTheWorkflowIsNeverStartedAgain(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	v, tasks, steps := ownersPlan(planIssue, 1, 2, planOwner, planGivenAt)
	records := NewStore()
	step2 := TaskPlace{Kind: PlaceStep, Position: 2}
	inTx(t, st, func(tx pgx.Tx) {
		putPlan(t, tx, v, tasks, steps)
		must(t, records.MoveTask(ctx, tx, planIssue, 1, "a", step2, TaskRunning))
	})
	acts := []struct {
		name string
		act  func(tx pgx.Tx) error
	}{
		{"a stop", func(tx pgx.Tx) error { return records.StopTask(ctx, tx, planIssue, 1, "a", "stopped by its owner") }},
		{"a restart at its first step", func(tx pgx.Tx) error {
			return records.MoveTask(ctx, tx, planIssue, 1, "a", TaskPlace{Kind: PlaceStep, Position: 1}, TaskRunning)
		}},
		{"a move to done", func(tx pgx.Tx) error { return records.MoveTask(ctx, tx, planIssue, 1, "a", step2, TaskDone) }},
	}
	try := func(t *testing.T, act func(tx pgx.Tx) error) error {
		t.Helper()
		tx, err := st.Pool().BeginTx(ctx, pgx.TxOptions{})
		must(t, err)
		defer func() { _ = tx.Rollback(ctx) }()
		return act(tx)
	}
	for _, a := range acts {
		if err := try(t, a.act); err != nil {
			t.Fatalf("control: %s of the running task = %v, want it written", a.name, err)
		}
	}
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.MoveTask(ctx, tx, planIssue, 1, "a", step2, TaskLeft))
		if got := readPlan(t, tx, planIssue, 1).Tasks[0]; got.State != TaskLeft || got.Place != step2 {
			t.Fatalf("task a after its issue left = %s at %+v, want left at step 2", got.State, got.Place)
		}
	})
	for _, a := range acts {
		t.Run(a.name, func(t *testing.T) {
			if err := try(t, a.act); err == nil {
				t.Fatalf("%s of a task that left the workflow was written, want it refused", a.name)
			}
		})
	}
}

// A stopped task whose issue then leaves the workflow is left where it stands and keeps the
// reason it was stopped for: the state left says the run is over before the task was done, and
// the reason is the only record of why it was not done. The control is the same leave of a task
// that still runs, which had no reason and is left with none.
func TestAStoppedTaskThatLeavesTheWorkflowKeepsWhyItWasStopped(t *testing.T) {
	const why = "task a of LEGION-208 was stopped by its owner"
	step2 := TaskPlace{Kind: PlaceStep, Position: 2}
	for _, tc := range []struct {
		name    string
		stopped bool
		want    string
	}{
		{name: "a stopped task", stopped: true, want: why},
		{name: "a running task, the control", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := migratedStore(t)
			ctx := context.Background()
			v, tasks, steps := ownersPlan(planIssue, 1, 2, planOwner, planGivenAt)
			records := NewStore()
			inTx(t, st, func(tx pgx.Tx) {
				putPlan(t, tx, v, tasks, steps)
				must(t, records.MoveTask(ctx, tx, planIssue, 1, "a", step2, TaskRunning))
				if tc.stopped {
					must(t, records.StopTask(ctx, tx, planIssue, 1, "a", why))
				}
				must(t, records.MoveTask(ctx, tx, planIssue, 1, "a", step2, TaskLeft))
				if got := readPlan(t, tx, planIssue, 1).Tasks[0]; got.State != TaskLeft || got.StoppedReason != tc.want || got.Place != step2 {
					t.Fatalf("task a after its issue left = %s (%q) at %+v, want left at step 2 with the reason %q", got.State, got.StoppedReason, got.Place, tc.want)
				}
			})
		})
	}
}

// queryCountingTx counts each statement its caller sends, so a test can hold a read to the one
// query it promises.
type queryCountingTx struct {
	pgx.Tx
	statements int
}

func (tx *queryCountingTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	tx.statements++
	return tx.Tx.Query(ctx, sql, args...)
}

func (tx *queryCountingTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	tx.statements++
	return tx.Tx.QueryRow(ctx, sql, args...)
}

func (tx *queryCountingTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	tx.statements++
	return tx.Tx.Exec(ctx, sql, args...)
}

// PlanTasksOf is the task of each issue of a project at the issue's own generation, in one query
// whatever the number of issues, which the state's projection reads beside every issue it serves.
// An issue whose plan rows are of an older generation gives no row: those rows are a run that is
// over. An issue of another project gives none either. A project with no plan rows at all gives an
// empty list, in that one query too. The control is the issue whose rows are of its own
// generation, which is read whole.
func TestPlanTasksOfReadsEachIssuesTaskAtItsGenerationInOneQuery(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	records := NewStore()
	current := issueFixture(planIssue)
	older := issueFixture("LEGION-209")
	elsewhere := issueFixture("OTHER-1")
	elsewhere.Project = "OTHER"
	v, tasks, steps := ownersPlan(current.Key, current.Generation, 2, planOwner, planGivenAt)
	tasks[0].State, tasks[0].Place = TaskRunning, TaskPlace{Kind: PlaceStep, Position: 2}
	tasks[0].Counts = ReturnCounts{Pass: 5, Rounds: 3, BackwardMoves: map[string]int{"reviewing-implementing": 3, "testing-implementing": 2}, CapTold: 3}
	inTx(t, st, func(tx pgx.Tx) {
		for _, issue := range []Issue{current, older, elsewhere} {
			must(t, records.PutIssue(ctx, tx, issue))
		}
		putPlan(t, tx, v, tasks, steps)
		must(t, records.MoveTask(ctx, tx, current.Key, current.Generation, "a", tasks[0].Place, TaskRunning))
		must(t, records.PutReturnCounts(ctx, tx, current.Key, current.Generation, "a", tasks[0].Counts))
		olderV, olderTasks, olderSteps := ownersPlan(older.Key, older.Generation-1, 2, planOwner, planGivenAt)
		putPlan(t, tx, olderV, olderTasks, olderSteps)
		otherV, otherTasks, otherSteps := ownersPlan(elsewhere.Key, elsewhere.Generation, 2, planOwner, planGivenAt)
		putPlan(t, tx, otherV, otherTasks, otherSteps)
	})
	inTx(t, st, func(tx pgx.Tx) {
		counting := &queryCountingTx{Tx: tx}
		got, err := records.PlanTasksOf(ctx, counting, "LEGION")
		must(t, err)
		if !reflect.DeepEqual(got, tasks) || counting.statements != 1 {
			t.Fatalf("the plan tasks of LEGION = %+v in %d statements, want only %s's task at its generation, %+v, in one", got, counting.statements, current.Key, tasks)
		}
		counting = &queryCountingTx{Tx: tx}
		none, err := records.PlanTasksOf(ctx, counting, "NOPLANS")
		must(t, err)
		if len(none) != 0 || counting.statements != 1 {
			t.Fatalf("the plan tasks of a project with no plan rows = %+v in %d statements, want none in one", none, counting.statements)
		}
	})
}

// Where a task stands names the place a step runs at, for the count of its starts and for its
// record (record.StepPlace, the key of step_starts): the task, the kind and the place, in the words
// the count of starts already writes (workflow's placeOnWalk), so the start the walk counts for a task at
// its place is the row of step_starts that the end of that run reads, and the plan keeps no count of
// its own.
func TestATaskPlaceNamesThePlaceItsStartsAreCountedAt(t *testing.T) {
	for _, tc := range []struct {
		place TaskPlace
		want  StepPlace
	}{
		{TaskPlace{Kind: PlaceStep, Position: 2}, StepPlace{Task: "a", Kind: "step", Place: 2}},
		{TaskPlace{Kind: PlaceRetro}, StepPlace{Task: "a", Kind: "retro", Place: 0}},
		{TaskPlace{Kind: PlaceMerge}, StepPlace{Task: "a", Kind: "merge", Place: 0}},
	} {
		if got := tc.place.StepPlace("a"); got != tc.want {
			t.Errorf("task a at %+v counts its starts at %+v, want %+v", tc.place, got, tc.want)
		}
	}
}

// The counts of returns live on the task for an issue with plan rows, and on the implementer's
// phases row for an issue with none, where they lived before plans (record.PhaseRow's Pass, Rounds,
// ConflictRounds, BackwardMoves and CapTold): ReturnCounts and PutReturnCounts read and write the
// same place, and the write leaves every other column of that place as it was. A task the plan of
// an issue with plan rows does not hold is refused.
func TestReturnCountsLiveOnTheTaskOrOnTheImplementersRow(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	records := NewStore()
	v, tasks, steps := ownersPlan(planIssue, 1, 2, planOwner, planGivenAt)
	const noPlan = "LEGION-209"
	implementer := PhaseRow{Issue: noPlan, Role: claim.RoleImplementer, Claim: "legion-legion-legion-209-implementer", HandoffCommit: "abc123", Entry: 3, Pass: 1}
	counts := ReturnCounts{Pass: 4, Rounds: 3, ConflictRounds: 1, BackwardMoves: map[string]int{"testing-implementing": 1, "reviewing-implementing": 2}, CapTold: 3}
	inTx(t, st, func(tx pgx.Tx) {
		putPlan(t, tx, v, tasks, steps)
		must(t, records.PutIssue(ctx, tx, issueFixture(noPlan)))
		must(t, records.PutPhase(ctx, tx, implementer))
		must(t, records.PutReturnCounts(ctx, tx, planIssue, 1, "a", counts))
		must(t, records.PutReturnCounts(ctx, tx, noPlan, 1, "default", counts))
	})
	inTx(t, st, func(tx pgx.Tx) {
		for _, issue := range []struct{ key, task string }{{planIssue, "a"}, {noPlan, "default"}} {
			got, err := records.ReturnCounts(ctx, tx, issue.key, 1, issue.task)
			if err != nil || !reflect.DeepEqual(got, counts) {
				t.Fatalf("the counts of %s = %+v, %v, want %+v", issue.key, got, err, counts)
			}
		}
		if task := readPlan(t, tx, planIssue, 1).Tasks[0]; !reflect.DeepEqual(task.Counts, counts) || task.State != TaskWaiting {
			t.Fatalf("task a after its counts were written = %+v, want the counts on it and its state kept", task)
		}
		rows, err := records.Phases(ctx, tx, planIssue)
		must(t, err)
		if len(rows) != 0 {
			t.Fatalf("the phases rows of %s, which has plan rows = %#v, want none written", planIssue, rows)
		}
		rows, err = records.Phases(ctx, tx, noPlan)
		must(t, err)
		want := implementer
		want.Pass, want.Rounds, want.ConflictRounds, want.BackwardMoves, want.CapTold = counts.Pass, counts.Rounds, counts.ConflictRounds, counts.BackwardMoves, counts.CapTold
		if got := PhaseRowOf(rows, claim.RoleImplementer); !reflect.DeepEqual(got, want) {
			t.Fatalf("the implementer's row of %s = %#v, want the counts on it and every other column kept: %#v", noPlan, got, want)
		}
	})
	tx, err := st.Pool().BeginTx(ctx, pgx.TxOptions{})
	must(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := records.ReturnCounts(ctx, tx, planIssue, 1, "z"); err == nil {
		t.Fatalf("the counts of a task the plan does not hold were read, want them refused")
	}
}

// The task a run with plan rows is on is its plan's one task, whatever that task's state: this
// release's plans hold one (plan.Supported). A plan that holds none, or more than one, names no
// task, and says how many it holds.
func TestThePlansTaskIsItsOneTask(t *testing.T) {
	v := PlanVersion{Issue: "LEGION-212", Generation: 1}
	stopped := PlanTask{Issue: "LEGION-212", Generation: 1, Task: "a", State: TaskStopped}
	if got, err := (&StoredPlan{Version: v, Tasks: []PlanTask{stopped}}).Task(); err != nil || !reflect.DeepEqual(got, stopped) {
		t.Fatalf("the task of a plan of one task = %+v, %v; want %+v", got, err, stopped)
	}
	for _, tc := range []struct {
		tasks []PlanTask
		want  string
	}{
		{nil, "the plan of LEGION-212 at generation 1 holds 0 tasks, and this release walks one"},
		{[]PlanTask{{Task: "a", State: TaskWaiting}, {Task: "b", State: TaskRunning}},
			"the plan of LEGION-212 at generation 1 holds 2 tasks, and this release walks one"},
	} {
		if got, err := (&StoredPlan{Version: v, Tasks: tc.tasks}).Task(); err == nil || err.Error() != tc.want || !reflect.DeepEqual(got, PlanTask{}) {
			t.Errorf("the task of a plan of %d tasks = %+v, %v; want no task and %q", len(tc.tasks), got, err, tc.want)
		}
	}
}

// An owner's plan holds an issue for two more reasons: its owner stopped its task, and it waits
// before its retro for the issues beneath it. Each is written and read back as any reason of a
// hold is.
func TestAHoldKeepsEachNewReason(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	for _, reason := range []HoldReason{HoldTaskStopped, HoldForChildren} {
		issue := issueFixture("LEGION-" + string(reason))
		issue.Phase, issue.Hold = phase.Held, &Hold{From: phase.Retro, Reason: reason}
		inTx(t, st, func(tx pgx.Tx) {
			must(t, NewStore().PutIssue(ctx, tx, issue))
			got, err := NewStore().Issue(ctx, tx, issue.Key)
			must(t, err)
			if got == nil || got.Hold == nil || *got.Hold != *issue.Hold {
				t.Fatalf("the hold of %s = %+v, want %+v", issue.Key, got, issue.Hold)
			}
		})
	}
	if HoldTaskStopped != "task-stopped" || HoldForChildren != "children" {
		t.Fatalf("the reasons are %q and %q, want task-stopped and children, the values the state serves", HoldTaskStopped, HoldForChildren)
	}
}
