package workflow

import (
	"reflect"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
)

// checkerAndCriticDeployment is Legion's own roles of a step and two roles the deployment defines:
// checker of the type testing and critic of the type review.
func checkerAndCriticDeployment() plan.Deployment {
	roles := plan.BuiltinRoles()
	roles["checker"] = plan.RoleDef{Name: "checker", Type: plan.TypeTesting}
	roles["critic"] = plan.RoleDef{Name: "critic", Type: plan.TypeReview}
	return plan.Deployment{Roles: roles}
}

// A stored step takes the type plan.RolesOf gives its role: the type it has in the engine's
// deployment (Config.Deployment), or, for a role the plan defines, the plan's, which the step
// record names and every reader of a step's type reads: the project's default steps and an
// owner's plan each store a role the deployment defines with its own type, as they store Legion's
// own roles. An engine whose configuration names no deployment stores Legion's own roles with
// theirs.
func TestAStoredStepTakesTheTypeOfItsRoleInTheDeployment(t *testing.T) {
	t.Run("the project's default steps", func(t *testing.T) {
		pool := migratedPool(t)
		engine := engineRunningByDefault(t, "implementer", "checker")
		engine.cfg.Deployment = checkerAndCriticDeployment()
		root := stepRoot(t, pool, phase.Admitted)

		firstStep(t, pool, engine, root.Key)

		want := []string{"implementer/implementation", "checker/testing"}
		if got := stepsOfTask(planStandingOf(t, pool, engine, root.Key, root.Generation).plan, plan.DefaultTask); !reflect.DeepEqual(got, want) {
			t.Fatalf("the stored steps of the default steps = %v, want %v", got, want)
		}
	})

	t.Run("an owner's plan", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		engine.cfg.Deployment = checkerAndCriticDeployment()
		root := stepRoot(t, pool, phase.Admitted)
		mustTake(t, "version 1", applyFacts(t, pool, engine)(planID(root.Key, root.Generation, 1),
			ownersPlan(t, root.Key, root.Generation, 1, givenTask(t, "Build the widget.", "implementer", "critic"))))

		firstStep(t, pool, engine, root.Key)

		want := []string{"implementer/implementation", "critic/review"}
		if got := stepsOfTask(planStandingOf(t, pool, engine, root.Key, root.Generation).plan, "a"); !reflect.DeepEqual(got, want) {
			t.Fatalf("the stored steps of the owner's task = %v, want %v", got, want)
		}
	})

	t.Run("an engine whose configuration names no deployment", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)

		firstStep(t, pool, engine, root.Key)

		if got := stepsOfTask(planStandingOf(t, pool, engine, root.Key, root.Generation).plan, plan.DefaultTask); !reflect.DeepEqual(got, todaysStoredSteps) {
			t.Fatalf("the stored default steps = %v, want Legion's own roles with theirs, %v", got, todaysStoredSteps)
		}
	})
}
