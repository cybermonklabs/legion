package workflow

import (
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// todaysStoredSteps is the steps of a project that names no default_steps, as the walk stores
// them: today's line up to the owner's acceptance, each with the type of its role.
var todaysStoredSteps = []string{"planner/planning", "implementer/implementation", "tester/testing", "reviewer/review", "acceptance:owner/review"}

// engineRunningByDefault is returnEngine for a project whose default_steps are words
// (config.Project.RunsByDefault, which the daemon wires as Config.DefaultSteps).
func engineRunningByDefault(t *testing.T, words ...string) *Engine {
	t.Helper()
	steps, err := plan.ParseSteps(strings.Join(words, " "))
	if err != nil {
		t.Fatal(err)
	}
	engine := returnEngine()
	engine.cfg.DefaultSteps = steps
	return engine
}

// assertDefaultPlan fails unless key's run at generation holds the plan of the default steps
// alone: version 0 of the source default, with no document and no giver, and one task,
// default, whose block text says so and whose steps are steps, run by default.
func assertDefaultPlan(t *testing.T, stored *record.StoredPlan, key string, generation uint64, steps []string) {
	t.Helper()
	if stored == nil {
		t.Fatalf("%s at generation %d holds no plan rows, want the default steps", key, generation)
	}
	if v := stored.Version; v.Issue != key || v.Generation != generation || v.Version != 0 || v.Source != record.PlanSourceDefault ||
		v.ArtifactID != "" || v.GivenBy != "" {
		t.Fatalf("the version in force of %s = %+v, want version 0 of the source default with no document and no giver", key, v)
	}
	if len(stored.Tasks) != 1 {
		t.Fatalf("the tasks of %s = %+v, want the one task default", key, stored.Tasks)
	}
	if task := stored.Tasks[0]; task.Task != plan.DefaultTask || task.PlanVersion != 0 || task.BlockText != "The project's default steps." ||
		!task.StepsByDefault {
		t.Fatalf("the one task of %s = %+v, want default of version 0, its text the project's default steps, run by default", key, task)
	}
	if got := stepsOfTask(stored, plan.DefaultTask); !reflect.DeepEqual(got, steps) {
		t.Fatalf("the steps of default = %v, want %v", got, steps)
	}
}

// An issue whose owner gives no plan stores the project's default steps at the start of its work
// (StartFirstPhase's gateOpened): one version 0 of the source default, one task default, the
// project's default_steps, and the first of them running, which is planning for a project that
// names none and implementing for `default_steps: [implementer, reviewer]`, whose run starts no
// planner. An owner's plan whose one block names no steps stores the project's default steps under
// the block's id, and the first step writes no default plan beside it.
func TestAnIssueWithNoPlanStoresTheDefaultSteps(t *testing.T) {
	t.Parallel()
	t.Run("a project that names no default_steps stores today's line and starts planning", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)

		firstStep(t, pool, engine, root.Key)

		now := planStandingOf(t, pool, engine, root.Key, root.Generation)
		assertDefaultPlan(t, now.plan, root.Key, root.Generation, todaysStoredSteps)
		want := walkStanding{phase.Planning, record.TaskPlace{Kind: record.PlaceStep, Position: 1}, record.TaskRunning, []string{"0 default"}, todaysStoredSteps}
		if got := walkStandingOf(t, pool, engine, root.Key, root.Generation); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s after its first step stands at %+v, want %+v", root.Key, got, want)
		}
		if planners := roleStartsOf(t, pool, root.Key, claim.RolePlanner); len(planners) != 1 {
			t.Fatalf("the planner's starts = %+v, want one", planners)
		}
	})

	t.Run("default_steps implementer reviewer starts implementing, and no planner", func(t *testing.T) {
		pool := migratedPool(t)
		engine := engineRunningByDefault(t, "implementer", "reviewer")
		root := stepRoot(t, pool, phase.Admitted)

		firstStep(t, pool, engine, root.Key)

		steps := []string{"implementer/implementation", "reviewer/review"}
		assertDefaultPlan(t, planStandingOf(t, pool, engine, root.Key, root.Generation).plan, root.Key, root.Generation, steps)
		want := walkStanding{phase.Implementing, record.TaskPlace{Kind: record.PlaceStep, Position: 1}, record.TaskRunning, []string{"0 default"}, steps}
		if got := walkStandingOf(t, pool, engine, root.Key, root.Generation); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s after its first step stands at %+v, want %+v", root.Key, got, want)
		}
		if planners := roleStartsOf(t, pool, root.Key, claim.RolePlanner); len(planners) != 0 {
			t.Fatalf("the planner's starts = %+v, want none: the project's default steps name no planner", planners)
		}
		if implementers := roleStartsOf(t, pool, root.Key, claim.RoleImplementer); len(implementers) != 1 || implementers[0].Phase != phase.Implementing {
			t.Fatalf("the implementer's starts = %+v, want one, of implementing", implementers)
		}
	})

	t.Run("an owner's block that names no steps stores the project's default steps under its id", func(t *testing.T) {
		pool := migratedPool(t)
		engine := engineRunningByDefault(t, "implementer", "reviewer")
		root := stepRoot(t, pool, phase.Admitted)
		mustTake(t, "version 1", applyFacts(t, pool, engine)(planID(root.Key, root.Generation, 1),
			ownersPlan(t, root.Key, root.Generation, 1, givenTask(t, "Build the widget."))))

		firstStep(t, pool, engine, root.Key)

		now := planStandingOf(t, pool, engine, root.Key, root.Generation)
		if task := now.task(t); task.Task != "a" || !task.StepsByDefault {
			t.Fatalf("the one task = %+v, want a, whose block named no steps", task)
		}
		want := walkStanding{phase.Implementing, record.TaskPlace{Kind: record.PlaceStep, Position: 1}, record.TaskRunning, []string{"1 owner"},
			[]string{"implementer/implementation", "reviewer/review"}}
		if got := walkStandingOf(t, pool, engine, root.Key, root.Generation); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s after its first step stands at %+v, want %+v: the owner's task on the project's default steps, and no default plan", root.Key, got, want)
		}
	})
}

// An owner's plan starts its first step: `implementer reviewer` given while the root stands
// at admitted starts implementing at its first step, and no planner claim starts. A plan the owner
// gave at generation 0 for a child the daemon had not recorded is carried to the child's
// generation when its first step starts (StartFirstPhase), each task from its start, and the child
// walks it; the rows at generation 0 stay. Where the first step passes over an earlier owner's
// plan of the child's own generation and carries the owner's plan of generation 0, the log line
// and the notice name the plan it runs: the carried one. The twin: a child whose owner gave no
// plan runs the default steps, from planning.
func TestAnOwnersPlanStartsItsFirstStep(t *testing.T) {
	t.Parallel()
	const notTaken = "workflow: a plan of an earlier owner is not taken"

	t.Run("a plan given at admitted starts its first step, and no planner", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)
		mustTake(t, "version 1", applyFacts(t, pool, engine)(planID(root.Key, root.Generation, 1),
			ownersPlan(t, root.Key, root.Generation, 1, givenTask(t, "Build the widget.", "implementer", "reviewer"))))

		firstStep(t, pool, engine, root.Key)

		assertWalksAt(t, pool, engine, root.Key, phase.Implementing, record.TaskPlace{Kind: record.PlaceStep, Position: 1})
		if got := planStandingOf(t, pool, engine, root.Key, root.Generation).versions; !reflect.DeepEqual(got, []string{"1 owner"}) {
			t.Fatalf("the versions of %s = %v, want the owner's alone", root.Key, got)
		}
		if planners := roleStartsOf(t, pool, root.Key, claim.RolePlanner); len(planners) != 0 {
			t.Fatalf("the planner's starts = %+v, want none", planners)
		}
		if implementers := roleStartsOf(t, pool, root.Key, claim.RoleImplementer); len(implementers) != 1 || implementers[0].Phase != phase.Implementing {
			t.Fatalf("the implementer's starts = %+v, want one, of implementing", implementers)
		}
	})

	t.Run("a plan given at generation 0 for a child is carried to its generation", func(t *testing.T) {
		pool := migratedPool(t)
		engine, logged := loggedEngine()
		recordedAt(t, pool, "LEGION-208", "LEGION-208")
		mustTake(t, "version 1 at generation 0", applyFacts(t, pool, engine)(planID("LEGION-209", 0, 1),
			ownersPlan(t, "LEGION-209", 0, 1, givenTask(t, "Build the widget.", "implementer", "reviewer"))))
		recordedAt(t, pool, "LEGION-209", "LEGION-208")
		logged.Reset()

		firstStep(t, pool, engine, "LEGION-209")

		want := walkStanding{phase.Implementing, record.TaskPlace{Kind: record.PlaceStep, Position: 1}, record.TaskRunning, []string{"1 owner"},
			[]string{"implementer/implementation", "reviewer/review"}}
		if got := walkStandingOf(t, pool, engine, "LEGION-209", 1); !reflect.DeepEqual(got, want) {
			t.Fatalf("LEGION-209 after its first step stands at %+v, want %+v, the owner's plan carried from generation 0", got, want)
		}
		if got := planRowsOf(t, pool, "LEGION-209"); got != [3]int{2, 2, 4} {
			t.Fatalf("the plan rows of LEGION-209 = %v, want the plan at generation 0 and its copy at generation 1", got)
		}
		if planners := roleStartsOf(t, pool, "LEGION-209", claim.RolePlanner); len(planners) != 0 {
			t.Fatalf("the planner's starts = %+v, want none", planners)
		}
		if strings.Contains(logged.String(), notTaken) || len(noticesOf(t, pool, "LEGION-209", "plan-not-taken")) != 0 {
			t.Fatalf("the carry of the owner's plan logged or told a plan not taken:\n%s", logged.String())
		}
		// The child's architect is told the phase the child starts, its plan's first step.
		started := []record.Notice{{Kind: "child-status", Role: claim.RoleArchitect,
			Reason: "LEGION-209 starts implementing under LEGION-208: it reached the front of the waiting line and took an admission slot" + childStartedOwnership}}
		if got := noticesOf(t, pool, "LEGION-209", "child-status"); !reflect.DeepEqual(got, started) {
			t.Fatalf("the child-status notices = %+v, want %+v", got, started)
		}
	})

	t.Run("a plan passed over at the child's generation names the carried plan it runs", func(t *testing.T) {
		pool := migratedPool(t)
		engine, logged := loggedEngine()
		earlier, owner := architectOf(t), claim.Token(treeRoleToken(t, "LEGION-209", claim.RoleArchitect))
		recordedAt(t, pool, "LEGION-209", "LEGION-208")
		mustTake(t, "the earlier owner's version 1", applyFacts(t, pool, engine)(planID("LEGION-209", 1, 1),
			ownersPlan(t, "LEGION-209", 1, 1, givenTask(t, "Build the widget.", "implementer", "tester"))))
		recordedAt(t, pool, "LEGION-209", "LEGION-209")
		given := ownersPlan(t, "LEGION-209", 0, 2, givenTask(t, "Build the widget.", "implementer", "reviewer"))
		given.GivenBy = owner
		mustTake(t, "the owner's version 2 at generation 0", applyFacts(t, pool, engine)(planID("LEGION-209", 0, 2), given))
		logged.Reset()

		firstStep(t, pool, engine, "LEGION-209")

		assertWalksAt(t, pool, engine, "LEGION-209", phase.Implementing, record.TaskPlace{Kind: record.PlaceStep, Position: 1})
		if got := stepsOfTask(planStandingOf(t, pool, engine, "LEGION-209", 1).plan, "a"); !reflect.DeepEqual(got, []string{"implementer/implementation", "reviewer/review"}) {
			t.Fatalf("the steps LEGION-209 walks = %v, want the owner's, carried", got)
		}
		runs := "version 2 of document " + planArtifact + ", given by " + string(owner)
		if line := `msg="` + notTaken + `" issue=LEGION-209 generation=1 version=1 document=` + planArtifact + " giver=" + string(earlier) + ` runs="` + runs + `"`; strings.Count(logged.String(), notTaken) != 1 || !strings.Contains(logged.String(), line) {
			t.Fatalf("the log =\n%s\nwant the one line %s", logged.String(), line)
		}
		want := []record.Notice{{Kind: "plan-not-taken", Role: claim.RoleArchitect, Reason: "The first step of LEGION-209 took no plan of an earlier owner: version 1 of document " +
			planArtifact + ", given by " + string(earlier) + ". It runs " + runs + ". To run a plan it did not take, give it again with register_plan as your own."}}
		if got := noticesOf(t, pool, "LEGION-209", "plan-not-taken"); !reflect.DeepEqual(got, want) {
			t.Fatalf("the plan-not-taken notices = %+v, want %+v", got, want)
		}
	})

	t.Run("the twin: a child whose owner gave no plan runs the default steps from planning", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		recordedAt(t, pool, "LEGION-208", "LEGION-208")
		recordedAt(t, pool, "LEGION-209", "LEGION-208")

		firstStep(t, pool, engine, "LEGION-209")

		want := walkStanding{phase.Planning, record.TaskPlace{Kind: record.PlaceStep, Position: 1}, record.TaskRunning, []string{"0 default"}, todaysStoredSteps}
		if got := walkStandingOf(t, pool, engine, "LEGION-209", 1); !reflect.DeepEqual(got, want) {
			t.Fatalf("LEGION-209 after its first step stands at %+v, want %+v", got, want)
		}
		started := []record.Notice{{Kind: "child-status", Role: claim.RoleArchitect,
			Reason: "LEGION-209 starts planning under LEGION-208: it reached the front of the waiting line and took an admission slot" + childStartedOwnership}}
		if got := noticesOf(t, pool, "LEGION-209", "child-status"); !reflect.DeepEqual(got, started) {
			t.Fatalf("the child-status notices = %+v, want %+v", got, started)
		}
	})
}

// An issue admitted in this release that its owner gave no plan holds the rows of the task
// default from its first step, so a plan its owner gives later is judged as a later version over
// them: while default runs it is refused with the running task's text, never with the text
// of an issue that started its work before Legion took plans; once the owner stops default, the
// same version is taken and its task starts. The twin: an issue that started its work before
// the release, with no plan rows, is refused with that earlier release's text.
func TestAnIssueOfThisReleaseJudgesALaterPlanAsALaterVersion(t *testing.T) {
	t.Parallel()
	t.Run("the default task running refuses it, and stopped takes it", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		apply := applyFacts(t, pool, engine)
		root := stepRoot(t, pool, phase.Admitted)
		key, gen := root.Key, root.Generation
		firstStep(t, pool, engine, key)
		id, version1 := planID(key, gen, 1), ownersPlan(t, key, gen, 1, givenTask(t, "Build the widget.", "implementer", "reviewer"))

		before := planStandingOf(t, pool, engine, key, gen)
		reason := plan.RunningTaskChanged(plan.DefaultTask).Reason
		assertPlanRefusal(t, pool, engine, key, gen, id, apply(id, version1), &intake.Refusal{Status: 409, Code: "PLAN_REFUSED",
			Message: "Legion refused this plan and recorded nothing: " + reason, Reasons: []plan.Refusal{{Task: plan.DefaultTask, Reason: reason}}}, before)

		if result := apply("stop", ownersStop(t, root, plan.DefaultTask)); result.Refusal != nil {
			t.Fatalf("the owner's stop of default was refused: %+v", result.Refusal)
		}
		mustTake(t, "version 1 given again after the stop", apply(id, version1))

		want := walkStanding{phase.Implementing, record.TaskPlace{Kind: record.PlaceStep, Position: 1}, record.TaskRunning, []string{"0 default", "1 owner"},
			[]string{"implementer/implementation", "reviewer/review"}}
		if got := walkStandingOf(t, pool, engine, key, gen); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s after version 1 stands at %+v, want %+v", key, got, want)
		}
	})

	t.Run("the twin: an issue that started before the release is refused with that release's text", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Implementing)
		before := planStandingOf(t, pool, engine, root.Key, root.Generation)
		id := planID(root.Key, root.Generation, 1)
		assertPlanRefusal(t, pool, engine, root.Key, root.Generation, id,
			applyFacts(t, pool, engine)(id, ownersPlan(t, root.Key, root.Generation, 1, givenTask(t, "Build the widget.", "implementer", "reviewer"))),
			intake.PlanRefused([]plan.Refusal{intake.StartedBeforePlans(root.Key)}), before)
	})
}
