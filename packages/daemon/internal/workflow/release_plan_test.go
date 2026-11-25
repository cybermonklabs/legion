package workflow

import (
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// seedPlanOfTheReleaseBefore writes the plan of key at generation as the release before this one
// wrote it, whose statements on plan_tasks name each column but parent_ref (its record's PutPlan):
// one version, given by source (an owner's version 1 of planArtifact by the architect of
// LEGION-208, or the default version 0 with no document and no giver), and one task, waiting at
// no act, with steps, each with the type of its role. The block of the task names its steps where
// stepsByDefault is false, and none where it is true.
func seedPlanOfTheReleaseBefore(t *testing.T, pool *pgxpool.Pool, key string, generation uint64, source, task, text string,
	stepsByDefault bool, steps []plan.Step) {
	t.Helper()
	version, artifact, givenBy := 1, planArtifact, treeRoleToken(t, "LEGION-208", claim.RoleArchitect)
	if source == record.PlanSourceDefault {
		version, artifact, givenBy = 0, "", ""
	}
	seedRecord(t, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), `insert into issue_plans (issue, generation, version, source, artifact_id, given_by, given_at)
			values ($1, $2, $3, $4, $5, $6, $7)`, key, int64(generation), version, source, artifact, givenBy, stepClock); err != nil {
			return err
		}
		if _, err := tx.Exec(t.Context(), `insert into plan_tasks (issue, generation, task, plan_version, block_text, needs, part_of, owner_role,
			merges, steps_by_default, state, stopped_reason, running_kind, position, pass, rounds, conflict_rounds, backward_moves, cap_told)
			values ($1, $2, $3, $4, $5, '{}', '', '', false, $6, 'waiting', '', '', 0, 0, 0, 0, '{}', 0)`,
			key, int64(generation), task, version, text, stepsByDefault); err != nil {
			return err
		}
		for i, step := range steps {
			if _, err := tx.Exec(t.Context(), `insert into plan_steps (issue, generation, task, position, role, type, agent_key)
				values ($1, $2, $3, $4, $5, $6, $7)`, key, int64(generation), task, i+1, step.Role, record.BuiltinDeployment.Roles[step.Role].Type, step.Key); err != nil {
				return err
			}
		}
		return nil
	})
}

// A plan the release before this one stored has no parent_ref of its own, and each of its tasks
// reads the empty parent, a block that names none. So it compares as its owner gave it.
//
// Task a of an owner's plan runs at its first step: a version that changes only a's text is taken,
// and a keeps its place and its counts. A version that gives a a parent is refused, naming a, as
// any change of a running task beyond its text is.
//
// The task default, stored as the default steps the issue runs where its owner gave no plan, runs
// its planner: an owner's version whose one task block is default, with three blocks of the
// planner under it, holds the one task default and three elements of it, and it is taken as a
// change of the text of default, which goes on at its step.
func TestAPlanStoredByTheReleaseBeforeComparesAsItWasGiven(t *testing.T) {
	t.Run("an owner's task takes a text alone and refuses a parent", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		apply := applyFacts(t, pool, engine)
		root := stepRoot(t, pool, phase.Admitted)
		key, gen := root.Key, root.Generation
		steps := givenTask(t, "", "implementer", "tester", "reviewer").Steps
		seedPlanOfTheReleaseBefore(t, pool, key, gen, record.PlanSourceOwner, "a", "Build the widget.", false, steps)
		move(t, pool, engine, key, TriggerGateOpened, "", record.PhaseRow{})
		before := planStandingOf(t, pool, engine, key, gen)
		if task := before.task(t); task.State != record.TaskRunning || task.Place != (record.TaskPlace{Kind: record.PlaceStep, Position: 1}) || task.ParentRef != "" {
			t.Fatalf("task a of the release before after its gate opened = %+v, want it running at step 1 with no parent", task)
		}

		retexted := givenTask(t, "Build the widget, and its manual.", "implementer", "tester", "reviewer")
		mustTake(t, "version 2 with a's text alone changed", apply(planID(key, gen, 2), ownersPlan(t, key, gen, 2, retexted)))
		kept := planStandingOf(t, pool, engine, key, gen)
		if task, was := kept.task(t), before.task(t); task.State != record.TaskRunning || task.Place != was.Place ||
			!sameReturnCounts(task.Counts, was.Counts) || task.BlockText != retexted.Text || !reflect.DeepEqual(kept.plan.Steps, before.plan.Steps) {
			t.Fatalf("task a after version 2 = %+v, want it running at %+v on its steps with version 2's text and its counts", task, was.Place)
		}

		moved := givenTask(t, retexted.Text, "implementer", "tester", "reviewer")
		moved.ParentRef = requirementOne
		id := planID(key, gen, 3)
		reason := plan.RunningTaskChanged("a").Reason
		assertPlanRefusal(t, pool, engine, key, gen, id, apply(id, ownersPlan(t, key, gen, 3, moved)), &intake.Refusal{Status: 409, Code: "PLAN_REFUSED",
			Message: "Legion refused this plan and recorded nothing: " + reason, Reasons: []plan.Refusal{{Task: "a", Reason: reason}}}, kept)
	})

	t.Run("the default task takes a version that holds the planner's blocks under it", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		apply := applyFacts(t, pool, engine)
		root := stepRoot(t, pool, phase.Admitted)
		key, gen := root.Key, root.Generation
		seedPlanOfTheReleaseBefore(t, pool, key, gen, record.PlanSourceDefault, plan.DefaultTask, "The project's default steps.", true,
			config.Project{}.RunsByDefault())
		move(t, pool, engine, key, TriggerGateOpened, "", record.PhaseRow{})
		before := planStandingOf(t, pool, engine, key, gen)
		if task := before.task(t); task.Task != plan.DefaultTask || task.State != record.TaskRunning || before.issue.Phase != phase.Planning {
			t.Fatalf("the default task of the release before after its gate opened = %+v in %s, want default running in planning", task, before.issue.Phase)
		}

		const document = "dispatch://ACME-1/artifact/plan-md"
		blocks := []plan.Block{{ID: plan.DefaultTask, Type: plan.TaskBlockType, Text: "Plan the widget, then build it."}}
		for _, id := range []string{"t1", "t2", "t3"} {
			blocks = append(blocks, plan.Block{ID: id, Type: plan.TaskBlockType, Text: "The planner's " + id + ".",
				Attributes: map[string]string{"parent": document + "#b-default", "serves": requirementOne}})
		}
		given, err := plan.FromBlocks(planArtifact, document, 1, blocks)
		if err != nil {
			t.Fatalf("read version 1 from its blocks: %v", err)
		}
		if len(given.Tasks) != 1 || given.Tasks[0].ID != plan.DefaultTask || len(given.Elements) != 3 {
			t.Fatalf("version 1 holds the tasks %+v and the elements %+v, want default alone and its three elements", given.Tasks, given.Elements)
		}
		fact := ownersPlan(t, key, gen, 1)
		fact.Plan = given
		mustTake(t, "version 1, default with the planner's blocks under it", apply(planID(key, gen, 1), fact))

		after := planStandingOf(t, pool, engine, key, gen)
		task, was := after.task(t), before.task(t)
		if task.Task != plan.DefaultTask || task.State != record.TaskRunning || task.Place != was.Place || task.BlockText != given.Tasks[0].Text ||
			!sameReturnCounts(task.Counts, was.Counts) || !reflect.DeepEqual(after.plan.Steps, before.plan.Steps) {
			t.Fatalf("the task after version 1 = %+v, want default running at %+v on its steps with version 1's text", task, was.Place)
		}
		if !reflect.DeepEqual(after.versions, []string{"0 default", "1 owner"}) || after.issue.Phase != phase.Planning {
			t.Fatalf("after version 1 the versions are %v and %s stands in %s, want the default and version 1, still in planning", after.versions, key, after.issue.Phase)
		}
	})
}
