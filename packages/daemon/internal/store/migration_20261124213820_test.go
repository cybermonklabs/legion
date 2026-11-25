package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// planStepAgentVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const planStepAgentVersion = 20261124213820

// planStepAgents is each plan_steps row of the store, as `<issue> <generation>/<task>/<position>
// <agent>`, ordered by those four: what the migration names, and what the way back drops.
func planStepAgents(t *testing.T, store *Store) []string {
	t.Helper()
	return rowsAsText(t, store, `select issue || ' ' || generation || '/' || task || '/' || position || ' ' || agent
		from plan_steps order by issue, generation, task, position`)
}

// keyNamesOf is each plan_key_names row of the store, as `<issue> <name> <key> <generation>`,
// ordered by issue and name: the lasting record of the names keys held, which the migration fills
// from the steps it names and the way back drops.
func keyNamesOf(t *testing.T, store *Store) []string {
	t.Helper()
	rows, err := store.Pool().Query(context.Background(), `select issue || ' ' || name || ' ' || agent_key || ' ' || generation from plan_key_names order by issue, name`)
	if err != nil {
		t.Fatalf("read plan_key_names: %v", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read plan_key_names: %v", err)
	}
	return names
}

// seedPlanSteps writes the plan of issue at generation, one task `a`, with steps, each a role and
// its agent key: the rows a plan_steps read finds, without the name of any agent.
func seedPlanSteps(t *testing.T, store *Store, issue string, generation int, steps ...[2]string) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.Pool().Exec(ctx, `insert into issue_plans (issue, generation, version, source, artifact_id, given_by, given_at)
		values ($1, $2, 1, 'owner', 'artifact', 'legion-legion-legion-208-architect', now())`, issue, generation); err != nil {
		t.Fatalf("seed the plan of %s at generation %d: %v", issue, generation, err)
	}
	if _, err := store.Pool().Exec(ctx, `insert into plan_tasks (issue, generation, task, plan_version, block_text, state)
		values ($1, $2, 'a', 1, 'Build the widget.', 'waiting')`, issue, generation); err != nil {
		t.Fatalf("seed the task of %s at generation %d: %v", issue, generation, err)
	}
	for i, step := range steps {
		if _, err := store.Pool().Exec(ctx, `insert into plan_steps (issue, generation, task, position, role, type, agent_key)
			values ($1, $2, 'a', $3, $4, 'implementation', $5)`, issue, generation, i+1, step[0], step[1]); err != nil {
			t.Fatalf("seed step %d of %s at generation %d: %v", i+1, issue, generation, err)
		}
	}
}

// 20261124213820 adds the name of each step's agent (record.PlanStep.Agent) and names every row
// it finds with the name record.NameAgents gave it. A step with no key is named for its role, which
// named its agent in the release before. A step of the key owner has no name, since its agent is
// the tree's architect. A step of any other key takes the role of the key's first row of its
// issue, by generation, task and place. That is the name NameAgents gave the key, though it keeps a
// key's newest earlier name: each generation's version took the name the generation before it
// held, so every generation holds the first one's. Key k's first step is the implementer's in
// generation 1 and the tester's in generation 2, and the names are held to what NameAgents gives
// the two versions in turn, the implementer in both. A row the migration finds keyed is a row this
// release wrote before a rollback, whose name the way back dropped. A key is an issue's own: the
// same key on another issue names another agent. A name written after the migration reads back as
// it was written, through a store reopened as a daemon restart reopens it, and the column refuses
// null, which the plan read cannot scan.
func TestThePlanStepAgentMigrationNamesEveryStepItFinds(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, planStepAgentVersion-1)
	seedPlanSteps(t, store, "LEGION-208", 1,
		[2]string{"implementer", "k"}, [2]string{"tester", "k"}, [2]string{"reviewer", ""}, [2]string{"acceptance", "owner"})
	seedPlanSteps(t, store, "LEGION-208", 2, [2]string{"tester", "k"}, [2]string{"reviewer", ""})
	seedPlanSteps(t, store, "LEGION-209", 1, [2]string{"reviewer", "k"}, [2]string{"implementer", ""})
	if _, err := store.Pool().Exec(ctx, "update plan_steps set agent = 'x'"); err == nil || !strings.Contains(err.Error(), "agent") {
		t.Fatalf("writing agent before the migration = %v, want the column to be unknown: the fixture is not the old shape", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the step's agent: %v", err)
	}
	first := record.NameAgents(nil, nil, nil, []record.PlanStep{
		{Generation: 1, Task: "a", Position: 1, Role: "implementer", AgentKey: "k"}, {Generation: 1, Task: "a", Position: 2, Role: "tester", AgentKey: "k"},
		{Generation: 1, Task: "a", Position: 3, Role: "reviewer"}, {Generation: 1, Task: "a", Position: 4, Role: "acceptance", AgentKey: "owner"}})
	second := record.NameAgents(first, nil, nil, []record.PlanStep{
		{Generation: 2, Task: "a", Position: 1, Role: "tester", AgentKey: "k"}, {Generation: 2, Task: "a", Position: 2, Role: "reviewer"}})
	if second[0].Agent != "implementer" {
		t.Fatalf("NameAgents named key k %q in generation 2, want implementer, its generation 1 name: the case is not the one this test holds", second[0].Agent)
	}
	var want []string
	for _, step := range append(first, second...) {
		want = append(want, fmt.Sprintf("LEGION-208 %d/%s/%d %s", step.Generation, step.Task, step.Position, step.Agent))
	}
	want = append(want, "LEGION-209 1/a/1 reviewer", "LEGION-209 1/a/2 implementer")
	if got := planStepAgents(t, store); !slices.Equal(got, want) {
		t.Fatalf("the steps after the migration = %q, want %q: each key named for the role of its first row of the issue, each step with no key for its role, and the owner's step unnamed", got, want)
	}
	// The lasting record holds each name a key's rows hold, by issue, with the latest generation
	// that gave it, and no name of a step with no key or of the owner's step: no issue row stands
	// for either issue, as none need stand for a plan given before the daemon records its issue.
	if got, want := keyNamesOf(t, store), []string{"LEGION-208 implementer k 2", "LEGION-209 reviewer k 1"}; !slices.Equal(got, want) {
		t.Fatalf("plan_key_names after the migration = %q, want %q: each name a key's rows hold, of each issue, at its latest generation", got, want)
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
	if steps := readSteps(store); len(steps) != 4 || steps[0].AgentName() != "implementer" || steps[3].AgentName() != "" {
		t.Fatalf("the steps of generation 1 read after the migration = %+v, want the key's first named implementer and the owner's unnamed", steps)
	}

	if _, err := store.Pool().Exec(ctx, "update plan_steps set agent = 'implementer-2' where issue = 'LEGION-208' and generation = 1 and position = 3"); err != nil {
		t.Fatalf("name the third step's agent: %v", err)
	}
	for _, st := range []*Store{store, reopenStore(t, store)} {
		if got := readSteps(st)[2].Agent; got != "implementer-2" {
			t.Fatalf("the third step's agent reads %q, want implementer-2 as it was written", got)
		}
	}
	if _, err := store.Pool().Exec(ctx, "update plan_steps set agent = null"); err == nil || !strings.Contains(err.Error(), "null value") {
		t.Fatalf("setting agent to null = %v, want the not-null constraint to refuse it", err)
	}
}

// The way back the header gives puts each claim back in its token's role, the last part of the
// token, which is the role the release before finds a claim's step by and grants it from: a claim
// this release moved to the role of the step it works (supervise.Machine.Rerole writes
// claims.role) would otherwise stay there, and the release before would read it as that role's
// worker beside the claim it builds for that role's token. A claim of a role the deployment
// defines, whose name holds dashes, comes back to it the same way; a claim in its token's role and
// the controller's, whose token names no issue, keep theirs. The roll forward then names the
// steps of every plan again, so a key whose name the drop took is named as it was.
func TestThePlanStepAgentWayBackPutsEachClaimInItsTokensRole(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	claims := []struct {
		token        claim.Token
		stored, want claim.Role
	}{
		// The implementer's claim, moved to the tester's step its key works.
		{"legion-legion-legion-208-implementer", claim.RoleTester, claim.RoleImplementer},
		// The claim of a role the deployment defines, moved to the implementer's step.
		{"legion-legion-legion-208-security-reviewer", claim.RoleImplementer, "security-reviewer"},
		{"legion-legion-legion-208-reviewer", claim.RoleReviewer, claim.RoleReviewer},
		{claim.ControllerToken("legion"), claim.RoleController, claim.RoleController},
	}
	for _, c := range claims {
		seeded := supervise.Claim{Token: c.token, Project: "legion", Tree: "LEGION-208", Issue: "LEGION-208",
			Role: c.stored, TreeEpoch: 1, Generation: 1, State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByOperator}
		if c.stored == claim.RoleController {
			seeded.Tree, seeded.Issue, seeded.TreeEpoch = "", "", 0
		}
		if err := store.PutClaim(ctx, seeded); err != nil {
			t.Fatalf("seed the claim %s: %v", c.token, err)
		}
	}
	seedPlanSteps(t, store, "LEGION-208", 1, [2]string{"implementer", "k"}, [2]string{"tester", "k"}, [2]string{"acceptance", "owner"})
	if _, err := store.Pool().Exec(ctx, `update plan_steps set agent = 'implementer' where agent_key = 'k'`); err != nil {
		t.Fatalf("name the key's agent: %v", err)
	}
	// A name the key held in a plan whose rows went: the record keeps it, and the way back drops it
	// with the table.
	if _, err := store.Pool().Exec(ctx, `insert into plan_key_names (issue, name, agent_key, generation) values ('LEGION-208', 'tester', 'j', 1)`); err != nil {
		t.Fatalf("record key j's name: %v", err)
	}

	if _, err := store.Pool().Exec(ctx, headerWayBackOf(t, planStepAgentMigrationFile), pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatalf("run the way back as one transaction: %v", err)
	}
	if tableExists(t, store, "plan_key_names") {
		t.Fatal("plan_key_names stands after the way back, want it dropped: the release before knows no such table")
	}
	var added int
	if err := store.Pool().QueryRow(ctx, `select count(*) from information_schema.columns where table_schema = 'public' and table_name = 'plan_steps'
		and column_name in ('agent', 'leave_session')`).Scan(&added); err != nil || added != 0 {
		t.Fatalf("plan_steps keeps %d of agent and leave_session after the way back (%v), want neither", added, err)
	}
	for _, c := range claims {
		var role string
		if err := store.Pool().QueryRow(ctx, `select role from claims where token = $1`, string(c.token)).Scan(&role); err != nil {
			t.Fatalf("read the role of %s after the way back: %v", c.token, err)
		}
		if claim.Role(role) != c.want {
			t.Fatalf("the claim %s stored as %s is %s after the way back, want %s, the role its token names", c.token, c.stored, role, c.want)
		}
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("roll forward after the way back: %v", err)
	}
	want := []string{"LEGION-208 1/a/1 implementer", "LEGION-208 1/a/2 implementer", "LEGION-208 1/a/3 "}
	if got := planStepAgents(t, store); !slices.Equal(got, want) {
		t.Fatalf("the steps after the roll forward = %q, want %q: the key named again as it was", got, want)
	}
	// The roll forward fills the record again from the steps it names; a name only the dropped
	// record kept is gone, which the way back's cost is.
	if got, want := keyNamesOf(t, store), []string{"LEGION-208 implementer k 1"}; !slices.Equal(got, want) {
		t.Fatalf("plan_key_names after the roll forward = %q, want %q: filled again from the named steps", got, want)
	}
}

// A database that ran 20261124213820 in its first form holds every row that form found at the empty
// string: that form added the column and named no row. An upgrade over it does not run
// 20261124213820 again, since that version is already recorded, so every such row still holds the
// empty string after it. A step that is not the owner's reads as the name its row holds
// (plan.StoredAgentName), so such a row reads as no name. The names record.NameAgents gives the
// steps of a version written over such rows are held by record's
// TestAStepWithNoKeyTakesANameWhereItsPlacesRowHoldsNone.
func TestTheStepAgentMigrationsFirstFormLeavesItsRowsUnnamedAfterTheUpgrade(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, planStepAgentVersion-1)
	seedPlanSteps(t, store, "LEGION-208", 1, [2]string{"implementer", ""}, [2]string{"reviewer", ""}, [2]string{"acceptance", "owner"})
	for _, statement := range []string{
		// The whole of the first form's statement.
		`alter table plan_steps add column agent text not null default ''`,
		fmt.Sprintf("insert into schema_version (version) values (%d)", planStepAgentVersion),
	} {
		if _, err := store.Pool().Exec(ctx, statement); err != nil {
			t.Fatalf("run the first form of the step agent migration, %s: %v", statement, err)
		}
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate over the first form: %v", err)
	}
	if got, want := planStepAgents(t, store), []string{"LEGION-208 1/a/1 ", "LEGION-208 1/a/2 ", "LEGION-208 1/a/3 "}; !slices.Equal(got, want) {
		t.Fatalf("the steps after the upgrade = %q, want %q: every row the first form found unnamed", got, want)
	}
}
