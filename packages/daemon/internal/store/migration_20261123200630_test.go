package store

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// planAddedVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const planAddedVersion = 20261123200630

// 20261123200630 adds issue_plans.added: each version the upgrade finds reads none, since the
// release before recorded nothing of what a version added, and a version written after it keeps
// the tasks its write names. The way back the header gives drops the column and deletes its
// version row, so the roll-forward runs the migration again, and every version then reads none.
func TestMigrationPlanAddedReadsNoneForEachVersionAndItsWayBackDropsTheColumn(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	exec := func(what, statement string) {
		t.Helper()
		if _, err := store.pool.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	versions := func() []string {
		t.Helper()
		return rowsAsText(t, store, `select version || ' ' || added::text from issue_plans where issue = 'LEGION-208' order by version`)
	}
	migrateThrough(t, store, planAddedVersion-1)
	if _, err := store.pool.Exec(ctx, `update issue_plans set added = '{}'`); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("update issue_plans set added before the migration = %v, want the column unknown: the fixture is not the old shape", err)
	}
	exec("write version 1 as the release before writes it", `insert into issue_plans (issue, generation, version, source, artifact_id, given_by, given_at)
		values ('LEGION-208', 1, 1, 'owner', 'artifact-208', 'legion-legion-legion-208-architect', now())`)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the tasks a version added: %v", err)
	}
	if got := versions(); !slices.Equal(got, []string{"1 {}"}) {
		t.Fatalf("the versions after the migration = %q, want version 1 adding none: the release before recorded nothing of it", got)
	}
	exec("write version 2 adding c", `insert into issue_plans (issue, generation, version, source, artifact_id, given_by, given_at, added)
		values ('LEGION-208', 1, 2, 'owner', 'artifact-208', 'legion-legion-legion-208-architect', now(), '{c}')`)
	if got := versions(); !slices.Equal(got, []string{"1 {}", "2 {c}"}) {
		t.Fatalf("the versions = %q, want version 2 keeping the task it added", got)
	}

	exec("run the way back", wayBackOf(t, planAddedVersion, "the tasks a version added"))
	if _, err := store.pool.Exec(ctx, `update issue_plans set added = '{}'`); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("update issue_plans set added after the way back = %v, want the column gone", err)
	}
	if got := rowsAsText(t, store, `select count(*)::text from schema_version where version = 20261123200630`); got[0] != "0" {
		t.Fatalf("schema_version rows of %d after the way back = %s, want none", planAddedVersion, got[0])
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("roll forward after the way back: %v", err)
	}
	if got := versions(); !slices.Equal(got, []string{"1 {}", "2 {}"}) {
		t.Fatalf("the versions after the roll-forward = %q, want both kept, each adding none", got)
	}
}
