package workflow

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// versionKind is one of the three versions the sub-owner of f gives of its parts over the plan of
// placeTasks: one that adds the part f2, one that changes f1's text, and one that removes f1.
type versionKind string

const (
	addsAPart    versionKind = "adds a part"
	changesAPart versionKind = "changes a part"
	removesAPart versionKind = "removes a part"
)

// versionKinds is each versionKind, in the order the rows of a place run them.
var versionKinds = []versionKind{addsAPart, changesAPart, removesAPart}

// placeTasks is the plan the rows of TestASubOwnersVersionDependsOnWhereItsTaskStands start from: a
// (one implementer's step, running at it), f (owner=architect) needing fNeeds, and its parts f0 (no
// needs) and f1 (needing f0), with f, f0 and f1 waiting at no act.
func placeTasks(fNeeds ...string) []ownedTask {
	f1 := part("f1", "f")
	f1.needs = []string{"f0"}
	return []ownedTask{stepped("a", "implementer").at(record.TaskRunning, atStep(1)), subOwned("f", fNeeds...), part("f0", "f"), f1}
}

// donePlaceTasks is placeTasks with f's parts f0 and f1 done, as each part is wherever its task
// stands at its report, its retro or its merge.
func donePlaceTasks() []ownedTask {
	tasks := placeTasks()
	tasks[2] = tasks[2].at(record.TaskDone, atStep(1))
	tasks[3] = tasks[3].at(record.TaskDone, atStep(1))
	return tasks
}

// partsVersion is the sub-owner of f's version 2 of kind over placeTasks(fNeeds...).
func partsVersion(t *testing.T, key string, generation uint64, kind versionKind, fNeeds ...string) intake.PlanGiven {
	t.Helper()
	f1 := partNeeding("f1", "f", "f0")
	tasks := []plan.Task{block("a", ""), ownedBlock("f", fNeeds...), partBlock("f0", "f", "implementer")}
	switch kind {
	case addsAPart:
		tasks = append(tasks, f1, partBlock("f2", "f", "implementer"))
	case changesAPart:
		f1.Text = "Do f1. Again."
		tasks = append(tasks, f1)
	}
	return subOwnersVersion(t, key, generation, "f", 2, tasks...)
}

// startsAtItsParts starts f of placeTasks on key, as a real start does: the start of each task that
// waits (startReady) puts f at its parts, waiting for the join that makes its branch, since a runs
// on the issue's own branch (record.Store.AssignTaskBranch); the end of that join (joinEnds) lets f
// wait for nothing, and f0 starts, while f1 waits for f0.
func startsAtItsParts(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string) {
	t.Helper()
	onStored(t, pool, engine, key, func(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
		return engine.startReadyNow(ctx, tx, issue.Key)
	})
	assertWaits(t, tasksOf(t, pool, engine, key), "f", atParts, record.StepWaitJoin)
	joinEnds(t, pool, engine, key, "f")
	tasks := tasksOf(t, pool, engine, key)
	assertWaits(t, tasks, "f", atParts, record.StepWaitNone)
	assertTask(t, tasks, "f0", record.TaskRunning, atStep(1))
	assertTask(t, tasks, "f1", record.TaskWaiting, record.TaskPlace{})
}

// subOwnersClaimFails applies the failure of the claim of f's sub-owner on key (intake.ClaimFailed),
// which holds f at its parts (claimFailed), and fails the test where the fact is not taken.
func subOwnersClaimFails(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string) {
	t.Helper()
	mustSpend(t, "the failed claim of f's sub-owner", applyFacts(t, pool, engine)("sub-owner-f-failed",
		intake.ClaimFailed{Issue: key, Role: claim.RoleSubOwner, Claim: agentClaim(t, key, plan.SubOwnerAgent("f"))}))
}

// atAct is where onPlace writes f of donePlaceTasks for a row of its report, its retro or its merge:
// through record.Store.MoveTask, running there, and held there through record.Store.HoldTask. Those
// rows claim the version rule alone, never that a report or a join made the place.
func atAct(t *testing.T, pool *pgxpool.Pool, key string, generation uint64, place record.TaskPlace, held bool) {
	t.Helper()
	seedRecord(t, pool, func(tx pgx.Tx) error {
		store := record.NewStore()
		if err := store.MoveTask(t.Context(), tx, key, generation, "f", place, record.TaskRunning); err != nil {
			return err
		}
		if !held {
			return nil
		}
		return store.HoldTask(t.Context(), tx, key, generation, "f", "")
	})
}

// Where a task stands decides whether its sub-owner gives a version of its parts. At its report,
// and at its retro and its merge, running or held, the task moves forward only, so a part given then
// would never start: the version that adds a part is refused, naming the task and where it stands,
// and nothing is written. Those rows are the place rule's proof. There each part is done, so a
// version that changes or removes f1 is refused TASK_DONE naming the done part, by the refusal of a
// changed task, and the place rule makes no second refusal. No writer holds a task at its report,
// so no row holds f there. While f waits at no act, stands at its parts, running or held, or is
// stopped, each version is taken: a part added while f waits starts once f starts at its parts and
// its join ends; one added at the parts running starts at once; one added at the parts held waits
// for the release (TestAReleasedTaskStartsItsWaitingPart). The parts running come from a real start,
// and the hold at the parts from a real failed claim of f's sub-owner, whose held notice names f's
// parts; the owner's version that changes f while it is held there is refused HeldTaskChanged,
// naming its parts. A done task and one that left the workflow refuse each version TASK_DONE, by
// the sub-owner's own act, which sends the missing work to the owner of the issue. Once f stands at
// its parts again, as the merger's return puts it, the version refused at its merge is taken.
//
// The faults it catches are a part given where it would never start, and a task held at its parts
// that is told of a step it does not have (its controls: partsPlaceRefusal that tests only the
// report, which takes the added part at the retro; taskPlaceWords as at the base, which says `step
// 0 ()`; heldTaskSummary as at the base, which says `task f, step 0`).
func TestASubOwnersVersionDependsOnWhereItsTaskStands(t *testing.T) {
	t.Parallel()
	for _, place := range []struct {
		name  string
		at    record.TaskPlace
		held  bool
		words string
	}{
		{"reported, running", atReport, false, "report"},
		{"its retro, running", atRetro, false, "retro"},
		{"its retro, held", atRetro, true, "retro"},
		{"its merge, running", atMerge, false, "merge"},
		{"its merge, held", atMerge, true, "merge"},
	} {
		for _, kind := range versionKinds {
			t.Run(place.name+", "+string(kind), func(t *testing.T) {
				pool := migratedPool(t)
				engine := returnEngine()
				root := stepRoot(t, pool, phase.Implementing)
				seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
				seedOwnedVersion(t, pool, root.Key, root.Generation, donePlaceTasks()...)
				atAct(t, pool, root.Key, root.Generation, place.at, place.held)
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
	for _, kind := range versionKinds {
		t.Run("waiting at no act, "+string(kind), func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			root := stepRoot(t, pool, phase.Implementing)
			seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
			seedOwnedVersion(t, pool, root.Key, root.Generation, placeTasks("a")...)
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
				return engine.startReadyNow(ctx, tx, issue.Key)
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
			seedOwnedVersion(t, pool, root.Key, root.Generation, placeTasks()...)
			startsAtItsParts(t, pool, engine, root.Key)
			mustTake(t, "the sub-owner's version 2", applyFacts(t, pool, engine)("sub-owner-version-2", partsVersion(t, root.Key, root.Generation, kind)))
			tasks := tasksOf(t, pool, engine, root.Key)
			assertTask(t, tasks, "f", record.TaskRunning, atParts)
			switch kind {
			case addsAPart:
				assertTask(t, tasks, "f2", record.TaskRunning, atStep(1))
			case changesAPart:
				assertTask(t, tasks, "f1", record.TaskWaiting, record.TaskPlace{})
			case removesAPart:
				if _, ok := tasks["f1"]; ok {
					t.Fatalf("the plan after the version that removes f1 holds %+v, want no f1", tasks["f1"])
				}
			}
		})
		t.Run("at its parts, held, "+string(kind), func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			root := stepRoot(t, pool, phase.Implementing)
			seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
			seedOwnedVersion(t, pool, root.Key, root.Generation, placeTasks()...)
			startsAtItsParts(t, pool, engine, root.Key)
			subOwnersClaimFails(t, pool, engine, root.Key)
			assertTask(t, tasksOf(t, pool, engine, root.Key), "f", record.TaskHeld, atParts)
			mustTake(t, "the sub-owner's version 2", applyFacts(t, pool, engine)("sub-owner-version-2", partsVersion(t, root.Key, root.Generation, kind)))
			tasks := tasksOf(t, pool, engine, root.Key)
			assertTask(t, tasks, "f", record.TaskHeld, atParts)
			if kind == addsAPart {
				assertTask(t, tasks, "f2", record.TaskWaiting, record.TaskPlace{})
			}
		})
		t.Run("stopped, "+string(kind), func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			root := stepRoot(t, pool, phase.Implementing)
			seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
			seedOwnedVersion(t, pool, root.Key, root.Generation, placeTasks()...)
			startsAtItsParts(t, pool, engine, root.Key)
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
				tasks := placeTasks()
				tasks[1] = tasks[1].at(state, atMerge)
				tasks[2] = tasks[2].at(state, atStep(1))
				tasks[3] = tasks[3].at(state, atStep(1))
				seedOwnedVersion(t, pool, root.Key, root.Generation, tasks...)
				before := planStandingOf(t, pool, engine, root.Key, root.Generation)
				assertPlanRefusal(t, pool, engine, root.Key, root.Generation, "sub-owner-version-2",
					applyFacts(t, pool, engine)("sub-owner-version-2", partsVersion(t, root.Key, root.Generation, kind)),
					intake.FinishedTask(root.Key, "f", state, intake.ChangeOfParts), before)
			})
		}
	}
	t.Run("held at its parts by a failed claim, told so, and the owner's change of it", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Implementing)
		seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
		seedOwnedVersion(t, pool, root.Key, root.Generation, placeTasks()...)
		startsAtItsParts(t, pool, engine, root.Key)
		subOwnersClaimFails(t, pool, engine, root.Key)
		held := noticesOfKind(t, pool, root.Key, "held")
		if !slices.ContainsFunc(held, func(n record.Notice) bool { return n.Role == claim.RoleSubOwner && n.Summary == "task f, its parts" }) {
			t.Fatalf("the held notices of %s = %+v, want the sub-owner's, naming task f, its parts", root.Key, held)
		}
		before := planStandingOf(t, pool, engine, root.Key, root.Generation)
		given := laterVersion(t, root.Key, root.Generation, 2, block("a", ""), ownedBlock("f", "a"), partBlock("f0", "f", "implementer"), partNeeding("f1", "f", "f0"))
		assertPlanRefusal(t, pool, engine, root.Key, root.Generation, "owner-version-2", applyFacts(t, pool, engine)("owner-version-2", given),
			intake.PlanRefused([]plan.Refusal{intake.HeldTaskChanged("f", "its parts")}), before)
	})
	t.Run("at its parts again after the merger's return, adds a part", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Implementing)
		seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
		tasks := donePlaceTasks()
		tasks[1] = tasks[1].at(record.TaskRunning, atMerge)
		seedOwnedVersion(t, pool, root.Key, root.Generation, tasks...)
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

// A part that its task's sub-owner gave while the task was held at its parts starts at the release
// of the hold: f runs at its parts from a real start, with f1 running and f2 done; the real failed
// claim of f's sub-owner holds f, and its version then adds f3, which waits. f1 passes its step and
// is done, which starts nothing while f is held. The owner's retry_or_escalate releases f, and f3
// starts at its first step in that fact's transaction. The retry starts no sub-owner, as no start
// of f at its parts does (Walk.RoleAt): it queues no start of the role.
//
// The faults it catches are a part that waits for good after its task's release, and a retry that
// fails for a start of the sub-owner its claim cannot name (its controls: the release of
// retry_or_escalate with no start of the tasks that may start, as at the base, which leaves f3
// waiting; restart that starts the sub-owner, which fails the retry: the branch of its step is
// unknown).
func TestAReleasedTaskStartsItsWaitingPart(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Implementing)
	seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
	seedOwnedVersion(t, pool, root.Key, root.Generation, subOwned("f"), part("f1", "f"), part("f2", "f").at(record.TaskDone, atStep(1)))
	onStored(t, pool, engine, root.Key, func(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
		return engine.startReadyNow(ctx, tx, issue.Key)
	})
	joinEnds(t, pool, engine, root.Key, "f")
	assertTask(t, tasksOf(t, pool, engine, root.Key), "f1", record.TaskRunning, atStep(1))
	joinEnds(t, pool, engine, root.Key, "f1")
	subOwnersClaimFails(t, pool, engine, root.Key)
	assertTask(t, tasksOf(t, pool, engine, root.Key), "f", record.TaskHeld, atParts)
	apply := applyFacts(t, pool, engine)
	mustTake(t, "the sub-owner's version that adds f3", apply("sub-owner-version-2", subOwnersVersion(t, root.Key, root.Generation, "f", 2,
		ownedBlock("f"), partBlock("f1", "f", "implementer"), partBlock("f2", "f", "implementer"), partBlock("f3", "f", "implementer"))))
	assertTask(t, tasksOf(t, pool, engine, root.Key), "f3", record.TaskWaiting, record.TaskPlace{})

	seedTaskPRs(t, pool, root.Key, "f1")
	mustSpend(t, "f1's completion", apply("f1-implemented", completedBy(t, root.Key, claim.RoleImplementer, "implementer-f1", "f1", "")))
	tasks := tasksOf(t, pool, engine, root.Key)
	assertTask(t, tasks, "f1", record.TaskDone, atStep(1))
	assertTask(t, tasks, "f3", record.TaskWaiting, record.TaskPlace{})

	mustSpend(t, "the owner's retry of f", apply("retry-f", intake.RetryOrEscalate{Issue: root.Key, Decision: intake.RetryDecision, Task: "f", Reason: "go on"}))
	tasks = tasksOf(t, pool, engine, root.Key)
	assertTask(t, tasks, "f", record.TaskRunning, atParts)
	assertTask(t, tasks, "f3", record.TaskRunning, atStep(1))
	if starts := roleStartsOf(t, pool, root.Key, claim.RoleSubOwner); len(starts) != 0 {
		t.Fatalf("the starts of the sub-owner of %s after the retry = %+v, want none", root.Key, starts)
	}
}

// A part starts where its task's join ends, at each door that ends it, a carried one among them: f
// (owner=architect) needs a, which is done, so a real start puts f at its parts waiting for its join,
// with its task_branch row queued at generation 1, and its part f1 waits. The plan is carried to
// generation 2 with f waiting for its join (record.Store.CarryPlan with carried true, called
// directly: this exercises the carried record, not a re-admission), and the issue moves there. The
// fact's settlement queues the join again at generation 2 (joinAgain), and f1 still waits. The end
// of that join (intake.TaskJoined) starts f1 in its transaction.
//
// The fault it catches is a part that waits for good after its task's join ended (its control: the
// start after a wait with no start of the tasks that may start, and taskJoined with none of its own,
// which leave f1 waiting).
func TestAPartStartsWhereItsTasksJoinEndsAtAnyEntry(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Implementing)
	seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
	seedOwnedVersion(t, pool, root.Key, root.Generation, stepped("a", "implementer").at(record.TaskDone, atStep(1)), subOwned("f", "a"), part("f1", "f"))
	seedTaskBranch(t, pool, root.Key, "a", "legion/"+root.Key)
	onStored(t, pool, engine, root.Key, func(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
		return engine.startReadyNow(ctx, tx, issue.Key)
	})
	assertWaits(t, tasksOf(t, pool, engine, root.Key), "f", atParts, record.StepWaitJoin)
	if rows := taskBranchRowsOf(t, pool, root.Key); !slices.Equal(rows, []string{"f"}) {
		t.Fatalf("the task_branch rows of %s name %v, want f's one", root.Key, rows)
	}

	onStored(t, pool, engine, root.Key, func(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
		if err := engine.store.CarryPlan(ctx, tx, issue.Key, issue.Generation, issue.Generation+1, true); err != nil {
			return err
		}
		moved := issue
		moved.Generation = issue.Generation + 1
		return engine.store.PutIssue(ctx, tx, moved)
	})
	if issue := issueOf(t, pool, engine, root.Key); issue.Generation != root.Generation+1 {
		t.Fatalf("%s stands at generation %d after the carry, want %d", root.Key, issue.Generation, root.Generation+1)
	}
	if rows := taskBranchRowsOf(t, pool, root.Key); !slices.Equal(rows, []string{"f", "f"}) {
		t.Fatalf("the task_branch rows of %s after the carry name %v, want f's row of each generation: the settlement queues its join again", root.Key, rows)
	}
	tasks := tasksOf(t, pool, engine, root.Key)
	assertWaits(t, tasks, "f", atParts, record.StepWaitJoin)
	assertTask(t, tasks, "f1", record.TaskWaiting, record.TaskPlace{})

	joinEnds(t, pool, engine, root.Key, "f")

	tasks = tasksOf(t, pool, engine, root.Key)
	assertWaits(t, tasks, "f", atParts, record.StepWaitNone)
	assertTask(t, tasks, "f1", record.TaskRunning, atStep(1))
}
