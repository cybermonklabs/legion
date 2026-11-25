package store

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// planStepAgentVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const planStepAgentVersion = 20261117213545

// 20261117213545 adds the name of each step's agent (record.PlanStep.Agent). A step the release
// before wrote has no name and reads as its role, which named its agent then, so the agent of every
// plan the upgrade finds keeps the claim it had. A name written after it reads back as it was
// written, through a store reopened as a daemon restart reopens it, and the column refuses null,
// which the plan read cannot scan.
func TestThePlanStepAgentMigrationLeavesEveryStepNamedForItsRole(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, planStepAgentVersion-1)
	for _, seed := range []string{
		`insert into issue_plans (issue, generation, version, source, artifact_id, given_by, given_at)
			values ('LEGION-208', 1, 1, 'owner', 'artifact-208', 'legion-legion-legion-208-architect', now())`,
		`insert into plan_tasks (issue, generation, task, plan_version, block_text, state)
			values ('LEGION-208', 1, 'a', 1, 'Build the widget.', 'waiting')`,
		`insert into plan_steps (issue, generation, task, position, role, type, agent_key)
			values ('LEGION-208', 1, 'a', 1, 'implementer', 'implementation', ''),
			('LEGION-208', 1, 'a', 2, 'acceptance', 'review', 'owner')`,
	} {
		if _, err := store.Pool().Exec(ctx, seed); err != nil {
			t.Fatalf("record before the migration: %v", err)
		}
	}
	if _, err := store.Pool().Exec(ctx, "update plan_steps set agent = 'x'"); err == nil || !strings.Contains(err.Error(), "agent") {
		t.Fatalf("writing agent before the migration = %v, want the column to be unknown: the fixture is not the old shape", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the step's agent: %v", err)
	}
	readSteps := func(st *Store) []record.PlanStep {
		t.Helper()
		var plan *record.StoredPlan
		if err := Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) (err error) {
			plan, err = record.NewStore().Plan(ctx, tx, "LEGION-208", 1)
			return err
		}); err != nil {
			t.Fatalf("read the plan: %v", err)
		}
		if plan == nil {
			t.Fatal("the plan of LEGION-208 at generation 1 is gone after the migration")
		}
		return plan.Steps["a"]
	}
	steps := readSteps(store)
	if len(steps) != 2 || steps[0].Agent != "" || steps[1].Agent != "" {
		t.Fatalf("the steps after the migration = %+v, want both with no name: the release before kept none", steps)
	}
	if steps[0].AgentName() != "implementer" || steps[1].AgentName() != "" {
		t.Fatalf("the steps after the migration are named %q and %q, want implementer, the role, and none for the owner", steps[0].AgentName(), steps[1].AgentName())
	}

	if _, err := store.Pool().Exec(ctx, "update plan_steps set agent = 'implementer-2' where position = 1"); err != nil {
		t.Fatalf("name the first step's agent: %v", err)
	}
	for _, st := range []*Store{store, reopenStore(t, store)} {
		if got := readSteps(st)[0].Agent; got != "implementer-2" {
			t.Fatalf("the first step's agent reads %q, want implementer-2 as it was written", got)
		}
	}
	if _, err := store.Pool().Exec(ctx, "update plan_steps set agent = null"); err == nil || !strings.Contains(err.Error(), "null value") {
		t.Fatalf("setting agent to null = %v, want the not-null constraint to refuse it", err)
	}
}
