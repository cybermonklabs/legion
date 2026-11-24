package workflow

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// partNeeding is the block of part id of task of, with one implementer's step keyed id, needing
// needs, as putOwnedVersion stores it.
func partNeeding(id, of string, needs ...string) plan.Task {
	return plan.Task{ID: id, Text: "Do " + id + ".", PartOf: of, Needs: needs, Steps: []plan.Step{{Role: "implementer", Key: id}}}
}

// A sub-owner acts inside the parts of its own task, and which tasks merge is the owner's to
// decide: a sub-owner's version that changes whether a task it does not write merges is refused,
// naming that task, and nothing is written. Merging reads every task's needs, a part's among them.
// In the plan a (running), f (owner=architect, at its parts), g (running, needed by none, so it
// merges) and h (needs a and f), the sub-owner of f gives a version that adds f1 needing g: g would
// no longer merge, and the version is refused. Over the plan that holds f1 needing g, its version
// that removes f1 would make g merge, and is refused too. Its version that adds f1 needing a, which
// h needs already, changes no task's merging, and is taken. The owner's own version that adds k
// needing g, the same change of g's merging, is taken, as g stands at a step.
//
// The fault it catches is a part's need that takes the retro and the merge from a task of the
// owner's (its control: mergeChangeRefusals with no rule of the giver, as at the base, takes the
// sub-owner's two versions).
func TestASubOwnerLeavesWhetherTheOwnersTasksMerge(t *testing.T) {
	a := stepped("a", "implementer").at(record.TaskRunning, atStep(1))
	f := subOwned("f").at(record.TaskRunning, atParts)
	g := stepped("g", "implementer").at(record.TaskRunning, atStep(1))
	h := stepped("h", "implementer", "a", "f")
	blocks := []plan.Task{block("a", ""), ownedBlock("f"), block("g", ""), block("h", "", "a", "f")}
	for _, tc := range []struct {
		name    string
		seeded  []ownedTask
		version func(t *testing.T, key string, generation uint64) intake.PlanGiven
		want    *intake.Refusal
	}{
		{name: "a part's need that takes g's merge is refused", seeded: []ownedTask{a, f, g, h},
			version: func(t *testing.T, key string, generation uint64) intake.PlanGiven {
				return subOwnersVersion(t, key, generation, "f", 2, append(blocks, partNeeding("f1", "f", "g"))...)
			},
			want: intake.PlanRefused([]plan.Refusal{intake.MergeOfOwnersTask("LEGION-208", "g", "f", true)})},
		{name: "a removed part whose need kept g from merging is refused",
			seeded: []ownedTask{a, f, g, h, {id: "f1", role: "implementer", partOf: "f", needs: []string{"g"}, state: record.TaskWaiting}},
			version: func(t *testing.T, key string, generation uint64) intake.PlanGiven {
				return subOwnersVersion(t, key, generation, "f", 2, blocks...)
			},
			want: intake.PlanRefused([]plan.Refusal{intake.MergeOfOwnersTask("LEGION-208", "g", "f", false)})},
		{name: "a part's need that changes no task's merging is taken", seeded: []ownedTask{a, f, g, h},
			version: func(t *testing.T, key string, generation uint64) intake.PlanGiven {
				return subOwnersVersion(t, key, generation, "f", 2, append(blocks, partNeeding("f1", "f", "a"))...)
			}},
		{name: "the owner's version that takes g's merge is taken", seeded: []ownedTask{a, f, g, h},
			version: func(t *testing.T, key string, generation uint64) intake.PlanGiven {
				return laterVersion(t, key, generation, 2, append(blocks, block("k", "", "g"))...)
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			root := stepRoot(t, pool, phase.Implementing)
			seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
			seedOwnedVersion(t, pool, root.Key, root.Generation, tc.seeded...)
			before := planStandingOf(t, pool, engine, root.Key, root.Generation)
			result := applyFacts(t, pool, engine)("version-2", tc.version(t, root.Key, root.Generation))
			if tc.want != nil {
				assertPlanRefusal(t, pool, engine, root.Key, root.Generation, "version-2", result, tc.want, before)
				return
			}
			mustTake(t, "version 2", result)
		})
	}
}

// versionKind is one of the three versions a sub-owner gives of the parts of its task f over the
// plan a (running at step 1), f and its part f1: one that adds the part f2, one that changes f1's
// text, and one that removes f1.
type versionKind string

const (
	addsAPart    versionKind = "adds a part"
	changesAPart versionKind = "changes a part"
	removesAPart versionKind = "removes a part"
)

// partsVersion is the sub-owner of f's version 2 of kind over a, f (needing fNeeds) and f1.
func partsVersion(t *testing.T, key string, generation uint64, kind versionKind, fNeeds ...string) intake.PlanGiven {
	t.Helper()
	tasks := []plan.Task{block("a", ""), ownedBlock("f", fNeeds...)}
	switch kind {
	case addsAPart:
		tasks = append(tasks, partBlock("f1", "f", "implementer"), partBlock("f2", "f", "implementer"))
	case changesAPart:
		tasks = append(tasks, changedPart("f1", "f"))
	}
	return subOwnersVersion(t, key, generation, "f", 2, tasks...)
}

// Where a task stands decides whether its sub-owner gives a version of its parts. At its report,
// its retro and its merge, running or held, the task moves forward only, so a part given then would
// never start: the version that adds a part is refused, naming the task and where it stands, and
// nothing is written. Those rows are the place rule's proof. There each part is done (a report is
// taken only when each part is done), so a version that changes or removes f1 is refused TASK_DONE
// naming the done part, by the refusal of a changed task, and the place rule makes no second
// refusal of it. While the task waits at no act, stands at its parts, running or held, or is
// stopped, each version is taken: a part added while the task waits starts once the task starts at
// its parts and its join ends; one added at the parts running starts at once; one added at the
// parts held, with every other part done, runs once retry_or_escalate releases the task. A done task
// and one that left the workflow refuse each version TASK_DONE, by the sub-owner's own act. After
// the merger's return puts the task at its parts again, the version refused at the merge is taken.
//
// The faults it catches are a part given where it would never start, and a part given while its
// task is held that waits for good after the release (its controls: a place rule that tests only
// the report, which takes the added part at the retro; retryPhase's doors with no startReadyNow, as
// at the base, which leave the held row's part waiting).
func TestASubOwnersVersionDependsOnWhereItsTaskStands(t *testing.T) {
	a := stepped("a", "implementer").at(record.TaskRunning, atStep(1))
	f1Done := part("f1", "f").at(record.TaskDone, atStep(1))
	f1Waiting := part("f1", "f")
	for _, place := range []struct {
		name  string
		f     ownedTask
		words string
	}{
		{"reported, running", subOwned("f").at(record.TaskRunning, atReport), "report"},
		{"reported, held", subOwned("f").at(record.TaskHeld, atReport), "report"},
		{"its retro, running", subOwned("f").at(record.TaskRunning, atRetro), "retro"},
		{"its retro, held", subOwned("f").at(record.TaskHeld, atRetro), "retro"},
		{"its merge, running", subOwned("f").at(record.TaskRunning, atMerge), "merge"},
		{"its merge, held", subOwned("f").at(record.TaskHeld, atMerge), "merge"},
	} {
		for _, kind := range []versionKind{addsAPart, changesAPart, removesAPart} {
			t.Run(place.name+", "+string(kind), func(t *testing.T) {
				pool := migratedPool(t)
				engine := returnEngine()
				root := stepRoot(t, pool, phase.Implementing)
				seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
				seedOwnedVersion(t, pool, root.Key, root.Generation, a, place.f, f1Done)
				want := intake.FinishedTask(root.Key, "f1", record.TaskDone, intake.ChangeOfTask)
				if kind == addsAPart {
					reason := intake.PartsOfReportedTask(root.Key, "f")
					if place.words != "report" {
						reason = intake.PartsOfTaskAtAct(root.Key, "f", place.words)
					}
					want = intake.PlanRefused([]plan.Refusal{reason})
				}
				before := planStandingOf(t, pool, engine, root.Key, root.Generation)
				assertPlanRefusal(t, pool, engine, root.Key, root.Generation, "sub-owner-version-2",
					applyFacts(t, pool, engine)("sub-owner-version-2", partsVersion(t, root.Key, root.Generation, kind)), want, before)
			})
		}
	}
	for _, kind := range []versionKind{addsAPart, changesAPart, removesAPart} {
		t.Run("waiting at no act, "+string(kind), func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			root := stepRoot(t, pool, phase.Implementing)
			seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
			seedOwnedVersion(t, pool, root.Key, root.Generation, a, subOwned("f", "a"), f1Waiting)
			mustTake(t, "the sub-owner's version 2", applyFacts(t, pool, engine)("sub-owner-version-2", partsVersion(t, root.Key, root.Generation, kind, "a")))
			tasks := tasksOf(t, pool, engine, root.Key)
			assertTask(t, tasks, "f", record.TaskWaiting, record.TaskPlace{})
			if kind != addsAPart {
				return
			}
			assertTask(t, tasks, "f2", record.TaskWaiting, record.TaskPlace{})
			onStored(t, pool, engine, root.Key, func(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
				if err := engine.store.MoveTask(ctx, tx, issue.Key, issue.Generation, "a", atStep(1), record.TaskDone); err != nil {
					return err
				}
				_, err := engine.startReady(ctx, tx, issue)
				return err
			})
			assertWaits(t, tasksOf(t, pool, engine, root.Key), "f", atParts, record.StepWaitJoin)
			joinEnds(t, pool, engine, root.Key, "f")
			assertTask(t, tasksOf(t, pool, engine, root.Key), "f2", record.TaskRunning, atStep(1))
		})
		t.Run("at its parts, running, "+string(kind), func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			root := stepRoot(t, pool, phase.Implementing)
			seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
			seedOwnedVersion(t, pool, root.Key, root.Generation, a, subOwned("f").at(record.TaskRunning, atParts), f1Waiting)
			mustTake(t, "the sub-owner's version 2", applyFacts(t, pool, engine)("sub-owner-version-2", partsVersion(t, root.Key, root.Generation, kind)))
			if kind == addsAPart {
				assertTask(t, tasksOf(t, pool, engine, root.Key), "f2", record.TaskRunning, atStep(1))
			}
		})
		t.Run("at its parts, held, "+string(kind), func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			root := stepRoot(t, pool, phase.Implementing)
			seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
			f1 := f1Waiting
			if kind == addsAPart {
				f1 = f1Done
			}
			seedOwnedVersion(t, pool, root.Key, root.Generation, a, subOwned("f").at(record.TaskHeld, atParts), f1)
			apply := applyFacts(t, pool, engine)
			mustTake(t, "the sub-owner's version 2", apply("sub-owner-version-2", partsVersion(t, root.Key, root.Generation, kind)))
			if kind != addsAPart {
				return
			}
			assertTask(t, tasksOf(t, pool, engine, root.Key), "f2", record.TaskWaiting, record.TaskPlace{})
			mustSpend(t, "the retry of f", apply("retry-f", intake.RetryOrEscalate{Issue: root.Key, Decision: intake.RetryDecision, Task: "f", Reason: "go on"}))
			tasks := tasksOf(t, pool, engine, root.Key)
			assertTask(t, tasks, "f", record.TaskRunning, atParts)
			assertTask(t, tasks, "f2", record.TaskRunning, atStep(1))
		})
		t.Run("stopped, "+string(kind), func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			root := stepRoot(t, pool, phase.Implementing)
			seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
			seedOwnedVersion(t, pool, root.Key, root.Generation, a, subOwned("f").at(record.TaskRunning, atParts), f1Waiting)
			stopTasks(t, pool, root.Key, root.Generation, "f")
			mustTake(t, "the sub-owner's version 2", applyFacts(t, pool, engine)("sub-owner-version-2", partsVersion(t, root.Key, root.Generation, kind)))
			assertTask(t, tasksOf(t, pool, engine, root.Key), "f", record.TaskStopped, atParts)
		})
		for _, state := range []record.TaskState{record.TaskDone, record.TaskLeft} {
			t.Run(string(state)+", "+string(kind), func(t *testing.T) {
				pool := migratedPool(t)
				engine := returnEngine()
				root := stepRoot(t, pool, phase.Implementing)
				seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
				seedOwnedVersion(t, pool, root.Key, root.Generation, a, subOwned("f").at(state, atMerge), part("f1", "f").at(state, atStep(1)))
				before := planStandingOf(t, pool, engine, root.Key, root.Generation)
				assertPlanRefusal(t, pool, engine, root.Key, root.Generation, "sub-owner-version-2",
					applyFacts(t, pool, engine)("sub-owner-version-2", partsVersion(t, root.Key, root.Generation, kind)),
					intake.FinishedTask(root.Key, "f", state, intake.ChangeOfParts), before)
			})
		}
	}
	t.Run("at its parts again after the merger's return, adds a part", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Implementing)
		seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
		seedOwnedVersion(t, pool, root.Key, root.Generation, a, subOwned("f").at(record.TaskRunning, atMerge), f1Done)
		apply := applyFacts(t, pool, engine)
		before := planStandingOf(t, pool, engine, root.Key, root.Generation)
		assertPlanRefusal(t, pool, engine, root.Key, root.Generation, "sub-owner-version-2",
			apply("sub-owner-version-2", partsVersion(t, root.Key, root.Generation, addsAPart)),
			intake.PlanRefused([]plan.Refusal{intake.PartsOfTaskAtAct(root.Key, "f", "merge")}), before)
		seedRecord(t, pool, func(tx pgx.Tx) error {
			return record.NewStore().MoveTask(t.Context(), tx, root.Key, root.Generation, "f", atParts, record.TaskRunning)
		})
		mustTake(t, "the sub-owner's version 2 at the parts", apply("sub-owner-version-2", partsVersion(t, root.Key, root.Generation, addsAPart)))
		assertTask(t, tasksOf(t, pool, engine, root.Key), "f2", record.TaskRunning, atStep(1))
	})
}

// A task with a sub-owner held at its parts or at its report is told where it is held: an owner's
// version that changes f's needs while f is held is refused HeldTaskChanged, naming its parts or
// its report.
//
// The fault it catches is a refusal that names a step a task with a sub-owner does not have (its
// control: taskPlaceWords as at the base, which says `step 0 ()` for each).
func TestAHeldTaskWithASubOwnerIsToldWhereItIsHeld(t *testing.T) {
	for _, tc := range []struct {
		place record.TaskPlace
		words string
	}{{atParts, "its parts"}, {atReport, "its report"}} {
		t.Run(tc.words, func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			root := stepRoot(t, pool, phase.Implementing)
			seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
			seedOwnedVersion(t, pool, root.Key, root.Generation, stepped("a", "implementer").at(record.TaskRunning, atStep(1)),
				subOwned("f").at(record.TaskHeld, tc.place), stepped("h", "implementer", "f"))
			before := planStandingOf(t, pool, engine, root.Key, root.Generation)
			given := laterVersion(t, root.Key, root.Generation, 2, block("a", ""), ownedBlock("f", "a"), block("h", "", "f"))
			assertPlanRefusal(t, pool, engine, root.Key, root.Generation, "owner-version-2", applyFacts(t, pool, engine)("owner-version-2", given),
				intake.PlanRefused([]plan.Refusal{intake.HeldTaskChanged("f", tc.words)}), before)
		})
	}
}

// startReadyRuns is a record.Store that counts the runs of startReady (runs): each run reads the
// plan once at the top of its loop, and reads it again only after it started a task at its parts.
type startReadyRuns struct {
	record.Store
	runs *int
}

func (s startReadyRuns) Plan(ctx context.Context, tx pgx.Tx, issue string, generation uint64) (*record.StoredPlan, error) {
	if pc, _, _, ok := runtime.Caller(1); ok && strings.HasSuffix(runtime.FuncForPC(pc).Name(), ".(*Engine).startReady") {
		*s.runs++
	}
	return s.Store.Plan(ctx, tx, issue, generation)
}

// The start of each task that waits runs once for a fact, at its door: a version that starts a
// stopped task with a sub-owner again at its parts runs startReady once (afterSeveral), and not once
// more inside the start again. With a running, f (owner=architect) stopped at its parts, its part
// f1 waiting and h needing f, the owner's version that changes f reads the plan from startReady
// once, since no task startReady starts stands at its parts.
//
// The fault it catches is the rule of what starts after a change written at two places, which run
// it twice (its control: startAtFirstStep that runs startReadyNow at the parts, as at the base,
// which counts two runs).
func TestAVersionThatStartsATaskWithASubOwnerAgainStartsTasksOnce(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	runs := 0
	engine.store = startReadyRuns{Store: record.NewStore(), runs: &runs}
	root := stepRoot(t, pool, phase.Implementing)
	seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
	seedOwnedVersion(t, pool, root.Key, root.Generation, stepped("a", "implementer").at(record.TaskRunning, atStep(1)),
		subOwned("f").at(record.TaskRunning, atParts), part("f1", "f"), stepped("h", "implementer", "f"))
	stopTasks(t, pool, root.Key, root.Generation, "f")

	f := ownedBlock("f")
	f.Text = "Do f. Again."
	given := laterVersion(t, root.Key, root.Generation, 2, block("a", ""), f, partBlock("f1", "f", "implementer"), block("h", "", "f"))
	mustTake(t, "the owner's version 2", applyFacts(t, pool, engine)("owner-version-2", given))
	if runs != 1 {
		t.Fatalf("the version that starts f again ran startReady %d times, want once, at its door", runs)
	}
}

// A re-admission carries a running task, its wait among it, to the next generation, and the join
// its start queued at the generation the issue left finishes without acting, so the join is queued
// again at the new one (joinAgain). For a task at its parts that holds the issue's own branch and
// needs none, that join ends at once, and the task's parts that wait start (startAfterWait): f
// (owner=architect) runs at its parts waiting for the join its task_branch row of generation 1
// makes, f1 waits, and the plan is carried to generation 2 as the issue moves there; once joinAgain
// joined f there, f waits for nothing and f1 runs at its first step.
//
// The fault it catches is a task at its parts with no wait whose parts wait for good (its control:
// joinAgain as at the base, whose start after the wait runs no startReadyNow, which leaves f1
// waiting).
func TestAJoinQueuedAgainAfterAReAdmissionStartsThePartsThatWait(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Implementing)
	seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
	seedOwnedVersion(t, pool, root.Key, root.Generation, subOwned("f"), part("f1", "f"))
	readmitted := root
	readmitted.Generation = root.Generation + 1
	seedRecord(t, pool, func(tx pgx.Tx) error {
		store := record.NewStore()
		if err := store.MoveTask(t.Context(), tx, root.Key, root.Generation, "f", atParts, record.TaskRunning); err != nil {
			return err
		}
		row, err := record.NewOutboxRow(root.Key, record.TaskBranch{Generation: root.Generation, Task: "f"}, stepClock)
		if err != nil {
			return err
		}
		if err := store.Enqueue(t.Context(), tx, row); err != nil {
			return err
		}
		if err := store.CarryPlan(t.Context(), tx, root.Key, root.Generation, readmitted.Generation, true); err != nil {
			return err
		}
		return store.PutIssue(t.Context(), tx, readmitted)
	})
	assertWaits(t, tasksOf(t, pool, engine, root.Key), "f", atParts, record.StepWaitJoin)

	onStored(t, pool, engine, root.Key, func(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
		return engine.joinAgain(ctx, tx, []string{issue.Key})
	})

	tasks := tasksOf(t, pool, engine, root.Key)
	assertWaits(t, tasks, "f", atParts, record.StepWaitNone)
	assertTask(t, tasks, "f1", record.TaskRunning, atStep(1))
}
