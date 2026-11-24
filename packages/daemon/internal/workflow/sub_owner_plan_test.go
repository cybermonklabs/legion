package workflow

import (
	"maps"
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// subOwnersVersion is register_plan's fact for key at generation from the sub-owner of task of:
// version of the document the plan seedOwnedVersion writes ("plan-artifact"), holding tasks, given
// by the claim of the agent plan.SubOwnerAgent names, as the route records a sub-owner's grant.
func subOwnersVersion(t *testing.T, key string, generation uint64, of string, version int, tasks ...plan.Task) intake.PlanGiven {
	t.Helper()
	given := laterVersion(t, key, generation, version, tasks...)
	given.GivenBy = agentClaim(t, key, plan.SubOwnerAgent(of))
	return given
}

// ownedBlock is the block of task id with a sub-owner of the role architect, needing needs, as
// putOwnedVersion stores subOwned(id, needs...).
func ownedBlock(id string, needs ...string) plan.Task {
	return plan.Task{ID: id, Text: "Do " + id + ".", Owner: "architect", Needs: needs}
}

// partBlock is the block of task id, a part of task of, with one step of role keyed id, as
// putOwnedVersion stores part(id, of) for the role implementer.
func partBlock(id, of, role string) plan.Task {
	return plan.Task{ID: id, Text: "Do " + id + ".", PartOf: of, Steps: []plan.Step{{Role: role, Key: id}}}
}

// Each task is its writer's to change: the owner of the issue writes the tasks of the top level,
// and the sub-owner of a task writes its parts. With the plan in force f (owner=architect), running
// at its parts, and h (needs f), the sub-owner of f gives a version that adds the parts f1 and f2:
// it is taken, the version in force is given by the sub-owner's claim, and f1 and f2 start. Its
// version that changes h's steps is refused with one reason, and so is a version of the agent
// sub-owner-g, whom no task of the plan names; neither writes anything. The owner's version that
// lacks f1, which is stopped so that only the owner's rule refuses its removal, is refused with the
// reason that says what the owner can do instead. Beside a, which runs, a version of the owner that
// removes the stopped f with its parts is refused while the part f1 is done, with the reason of a
// done task, and is taken once f1 is stopped too: the three rows go.
//
// The fault it catches is an owner that changes the parts of a task, or a sub-owner that changes a
// task outside its own (its control: the rule skipped for the owner, so the owner's version that
// lacks f1 is taken and f1 is deleted, and the third case fails).
func TestASubOwnerChangesOnlyItsParts(t *testing.T) {
	t.Run("the sub-owner of f changes the parts of f alone", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		apply := applyFacts(t, pool, engine)
		root := stepRoot(t, pool, phase.Admitted)
		seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
		seedOwnedVersion(t, pool, root.Key, root.Generation, subOwned("f"), stepped("h", "implementer", "f"))
		firstStepOf(t, pool, engine, root.Key)
		assertTask(t, tasksOf(t, pool, engine, root.Key), "f", record.TaskRunning, atParts)

		f, h := ownedBlock("f"), block("h", "", "f")
		f1, f2 := partBlock("f1", "f", "implementer"), partBlock("f2", "f", "planner")
		mustTake(t, "the sub-owner's version 2", apply("sub-owner-version-2", subOwnersVersion(t, root.Key, root.Generation, "f", 2, f, h, f1, f2)))

		standing := planStandingOf(t, pool, engine, root.Key, root.Generation)
		if subOwner := agentClaim(t, root.Key, plan.SubOwnerAgent("f")); standing.plan.Version.Version != 2 || standing.plan.Version.GivenBy != subOwner {
			t.Fatalf("the version in force of %s is %d given by %s, want 2 given by f's sub-owner %s", root.Key, standing.plan.Version.Version, standing.plan.Version.GivenBy, subOwner)
		}
		tasks := tasksOf(t, pool, engine, root.Key)
		assertTask(t, tasks, "f", record.TaskRunning, atParts)
		assertTask(t, tasks, "f1", record.TaskRunning, atStep(1))
		assertTask(t, tasks, "f2", record.TaskRunning, atStep(1))
		assertTask(t, tasks, "h", record.TaskWaiting, record.TaskPlace{})

		hTested := plan.Task{ID: "h", Text: "Do h.", Needs: []string{"f"}, Steps: []plan.Step{{Role: "tester", Key: "h"}}}
		before := planStandingOf(t, pool, engine, root.Key, root.Generation)
		assertPlanRefusal(t, pool, engine, root.Key, root.Generation, "sub-owner-version-3",
			apply("sub-owner-version-3", subOwnersVersion(t, root.Key, root.Generation, "f", 3, f, hTested, f1, f2)),
			intake.PlanRefused([]plan.Refusal{{Task: "h", Reason: "task h is not a part of task f, and the sub-owner of task f changes only the parts of f."}}), before)

		assertPlanRefusal(t, pool, engine, root.Key, root.Generation, "sub-owner-g-version-3",
			apply("sub-owner-g-version-3", subOwnersVersion(t, root.Key, root.Generation, "g", 3, f, h, f1, f2)),
			intake.PlanRefused([]plan.Refusal{{Reason: "sub-owner-g is the sub-owner of no task of the plan in force of LEGION-208."}}), before)

		stopTasks(t, pool, root.Key, root.Generation, "f1")
		before = planStandingOf(t, pool, engine, root.Key, root.Generation)
		assertPlanRefusal(t, pool, engine, root.Key, root.Generation, "owner-version-3",
			apply("owner-version-3", laterVersion(t, root.Key, root.Generation, 3, f, h, f2)),
			intake.PlanRefused([]plan.Refusal{{Task: "f1", Reason: "task f1 is a part of task f, and only the sub-owner of task f changes the parts of f. " +
				"Leave the parts of f to its sub-owner, which writes them, or write this work as tasks of your own, with needs, outside task f."}}), before)
	})
	t.Run("the owner's version that removes a stopped task takes its parts with it", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			f1   ownedTask
		}{
			{"f1 done", part("f1", "f").at(record.TaskDone, atStep(1))},
			{"f1 stopped", part("f1", "f").at(record.TaskRunning, atStep(1))},
		} {
			t.Run(tc.name, func(t *testing.T) {
				pool := migratedPool(t)
				engine := returnEngine()
				root := stepRoot(t, pool, phase.Implementing)
				seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
				a := stepped("a", "implementer").at(record.TaskRunning, atStep(1))
				seedOwnedVersion(t, pool, root.Key, root.Generation, a, subOwned("f").at(record.TaskRunning, atParts), stepped("h", "implementer", "a", "f"),
					tc.f1, part("f2", "f").at(record.TaskRunning, atStep(1)))
				stopped := []string{"f", "f2"}
				if tc.f1.state == record.TaskRunning {
					stopped = append(stopped, "f1")
				}
				stopTasks(t, pool, root.Key, root.Generation, stopped...)

				before := planStandingOf(t, pool, engine, root.Key, root.Generation)
				result := applyFacts(t, pool, engine)("owner-version-2", laterVersion(t, root.Key, root.Generation, 2, block("a", ""), block("h", "", "a")))

				if tc.f1.state == record.TaskDone {
					assertPlanRefusal(t, pool, engine, root.Key, root.Generation, "owner-version-2", result,
						intake.FinishedTask(root.Key, "f1", record.TaskDone, intake.ChangeOfTask), before)
					return
				}
				mustTake(t, "the owner's version 2", result)
				if ids := slices.Sorted(maps.Keys(tasksOf(t, pool, engine, root.Key))); !slices.Equal(ids, []string{"a", "h"}) {
					t.Fatalf("the tasks of %s after the owner removed f = %v, want a and h: f, f1 and f2 deleted", root.Key, ids)
				}
			})
		}
	})
}
