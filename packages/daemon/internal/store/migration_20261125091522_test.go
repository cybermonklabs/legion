package store

import (
	"context"
	"strings"
	"testing"
)

func TestAWorkspaceDeclinesRecipientSurvivesMigrationAndRollsBack(t *testing.T) {
	const version = 20261125091522
	ctx := context.Background()
	st := emptyStore(t)
	migrateThrough(t, st, version-1)
	if _, err := st.Pool().Exec(ctx, `insert into idle_release_declines
		(project, issue, declined_at, checked_at, role, reason, head, commits)
		values ('legion', 'LEGION-208', now(), now(), 'implementer', 'unpushed_work', 'kept-head', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool().Exec(ctx, `update idle_release_declines set claim = ''`); err == nil || !strings.Contains(err.Error(), "claim") {
		t.Fatalf("writing the recipient before its migration = %v, want an unknown column", err)
	}
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var recipient, head string
	var commits int
	read := func() {
		t.Helper()
		if err := st.Pool().QueryRow(ctx, `select claim, head, commits from idle_release_declines
			where project = 'legion' and issue = 'LEGION-208'`).Scan(&recipient, &head, &commits); err != nil {
			t.Fatal(err)
		}
		if recipient != "" || head != "kept-head" || commits != 1 {
			t.Fatalf("migrated decline = recipient %q, head %q, commits %d; want an unknown recipient and unchanged work", recipient, head, commits)
		}
	}
	read()
	if _, err := st.Pool().Exec(ctx, `update idle_release_declines set claim = 'legion-legion-legion-208-tester'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool().Exec(ctx, wayBackOf(t, version, "the workspace decline recipient")); err != nil {
		t.Fatal(err)
	}
	var column, recorded bool
	if err := st.Pool().QueryRow(ctx, `select
		exists (select 1 from information_schema.columns where table_schema = current_schema()
			and table_name = 'idle_release_declines' and column_name = 'claim'),
		exists (select 1 from schema_version where version = $1)`, version).Scan(&column, &recorded); err != nil {
		t.Fatal(err)
	}
	if column || recorded {
		t.Fatalf("after rollback recipient column = %t and version = %t, want neither", column, recorded)
	}
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	read()
}
