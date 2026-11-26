package store

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// planEarlierGiversVersion is the version of the migration this file tests, so the test can
// migrate through everything before it whatever lands beside it.
const planEarlierGiversVersion = 20261124033010

// 20261124033010 adds issue_plans.earlier_givers, each owner that gave a version before the one it
// names last (given_by), and back-fills nothing: a version written before it names its one giver
// in given_by and reads no earlier giver. A release before it names its columns in every insert,
// so its own write of a version, the seven-column insert of its record.Store.PutPlan, still
// succeeds afterwards, and its row reads no earlier giver.
//
// The negative control is the read of the column before the migration, which the database
// refuses as a column it does not know.
func TestMigrationPlanEarlierGivers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, planEarlierGiversVersion-1)

	// A version as a release before this migration writes it: its seven columns, named.
	const before = `insert into issue_plans (issue, generation, version, source, artifact_id, given_by, given_at)
		values ($1, 1, 1, 'owner', '6a1f0c3e-2b7d-4e5a-9c81-3d4f5e6a7b80', 'legion-legion-legion-208-architect', now())`
	if _, err := store.Pool().Exec(ctx, before, "LEGION-501"); err != nil {
		t.Fatalf("record a version before the migration: %v", err)
	}
	var earlier []string
	err := store.Pool().QueryRow(ctx, `select earlier_givers from issue_plans where issue = 'LEGION-501'`).Scan(&earlier)
	if err == nil || !strings.Contains(err.Error(), "earlier_givers") {
		t.Fatalf("reading earlier_givers before the migration = %v, want the column to be unknown", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the earlier givers of a plan version: %v", err)
	}

	read := func(issue string) []string {
		t.Helper()
		var givers []string
		if err := store.Pool().QueryRow(ctx, `select earlier_givers from issue_plans where issue = $1`, issue).Scan(&givers); err != nil {
			t.Fatalf("read the earlier givers of the version of %s: %v", issue, err)
		}
		return givers
	}
	if got := read("LEGION-501"); len(got) != 0 {
		t.Errorf("the version written before the migration reads earlier givers %q, want none: the migration back-fills nothing", got)
	}
	// The write of a release before this migration still succeeds over the new column.
	if _, err := store.Pool().Exec(ctx, before, "LEGION-502"); err != nil {
		t.Fatalf("the earlier release's write of a version after the migration: %v", err)
	}
	if got := read("LEGION-502"); len(got) != 0 {
		t.Errorf("the earlier release's version reads earlier givers %q, want none", got)
	}
	if _, err := store.Pool().Exec(ctx, `update issue_plans set earlier_givers = earlier_givers || 'legion-legion-legion-211-architect'::text
		where issue = 'LEGION-502'`); err != nil {
		t.Fatalf("add an earlier giver: %v", err)
	}
	if got := read("LEGION-502"); !slices.Equal(got, []string{"legion-legion-legion-211-architect"}) {
		t.Errorf("the version reads earlier givers %q, want the one added", got)
	}
}

// docs/kubernetes.md rolls back the plan tables in one psql session for each project whose daemon
// rolls back on a shared database, a session that stops at its first error (ON_ERROR_STOP), and
// runs the way back of the givers of a plan version in that session with the plan tables' block.
// Here both projects of one database roll back, LEGION and then OTHER, each running the givers'
// block and then the plan tables' block, and each stopping at its first error, as its session
// would. LEGION's run drops the column and the tables; OTHER's finds no table, and must still run
// every statement, or its own rows of the plan tables' block stand, which the release before fails
// at every attempt. After both, no plan table, no column and neither migration's version row
// stands.
//
// The fault it catches is a statement of the givers' way back that errors when it runs again once
// the table is gone (its control: `alter table issue_plans drop column if exists earlier_givers;`,
// the block of the release before this change, under which OTHER's run stops with relation
// "issue_plans" does not exist).
func TestThePlanWayBacksRunOnceForEachProjectOfADatabase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := emptyStore(t)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	givers := wayBackOf(t, planEarlierGiversVersion, "the givers of a plan version")
	plans := ownerStepsWayBack(t)
	for _, project := range []string{"LEGION", "OTHER"} {
		for _, block := range []struct{ name, sql string }{
			{"the way back of the givers of a plan version", givers},
			{"the way back of the plan tables", strings.ReplaceAll(plans, "<PROJECT>", project)},
		} {
			if _, err := store.Pool().Exec(ctx, block.sql, pgx.QueryExecModeSimpleProtocol); err != nil {
				t.Fatalf("the rollback of %s stops at %s: %v", project, block.name, err)
			}
		}
	}
	var tables, column, versions bool
	if err := store.Pool().QueryRow(ctx, `select to_regclass('issue_plans') is not null or to_regclass('plan_tasks') is not null
		or to_regclass('plan_steps') is not null,
		exists (select 1 from information_schema.columns where table_name = 'issue_plans' and column_name = 'earlier_givers'),
		exists (select 1 from schema_version where version in (20261117034837, 20261124033010))`).Scan(&tables, &column, &versions); err != nil {
		t.Fatalf("read the schema after both rollbacks: %v", err)
	}
	if tables || column || versions {
		t.Errorf("after both rollbacks a plan table stands (%v), the givers' column does (%v) or a version row does (%v), want none", tables, column, versions)
	}
}
