package store

import (
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
)

// roleHolderMarksVersion adds projects.proven_sessions and project_role_holder_marks, and marks
// each project that already has a role holder (INFRA-4764).
const roleHolderMarksVersion = 20261120204136

// TestTheRoleHolderMarksMigrationMarksEachProjectThatHasARoleHolder runs the shipped files over
// rows written as the release before this one wrote them: HELD, whose daemon published role
// holders on two of its issues, three rows in all, and BARE, whose issue holds none. After the
// migration HELD alone is marked, once, and both projects read the setting as off.
func TestTheRoleHolderMarksMigrationMarksEachProjectThatHasARoleHolder(t *testing.T) {
	ctx := migrationContext(t)
	store := openEmptyTestStore(t)
	if err := store.migrate(ctx, migrationsThrough(t, roleHolderMarksVersion-1)); err != nil {
		t.Fatalf("migrate through %d: %v", roleHolderMarksVersion-1, err)
	}
	if _, err := store.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('HELD', 'Held', 'alice'), ('BARE', 'Bare', 'alice');
		insert into issues (key, project_key, number, title, created_by, rank) values
			('HELD-1', 'HELD', 1, 'One', '{"kind":"user","id":"alice"}', 'U'),
			('HELD-2', 'HELD', 2, 'Two', '{"kind":"user","id":"alice"}', 'V'),
			('BARE-1', 'BARE', 1, 'One', '{"kind":"user","id":"alice"}', 'U');
		insert into issue_role_holders (issue_key, role, session) values
			('HELD-1', 'architect', 'held-architect'),
			('HELD-1', 'implementer', 'held-implementer'),
			('HELD-2', 'tester', 'held-tester');
	`); err != nil {
		t.Fatalf("seed as the release before %d wrote: %v", roleHolderMarksVersion, err)
	}

	// The negative control of the checks below, inside this one run: at the schema the release
	// before this one carried, neither the table nor the column exists, so a mark and a setting
	// read back afterwards are the migration's doing.
	var markTable, settingColumn bool
	if err := store.Pool.QueryRow(ctx, `
		select to_regclass('project_role_holder_marks') is not null,
		       exists (select 1 from information_schema.columns
		               where table_name = 'projects' and column_name = 'proven_sessions')
	`).Scan(&markTable, &settingColumn); err != nil {
		t.Fatalf("read the catalog at %d: %v", roleHolderMarksVersion-1, err)
	}
	if markTable || settingColumn {
		t.Fatalf("at %d project_role_holder_marks exists = %t and projects.proven_sessions exists = %t, want neither before %d runs",
			roleHolderMarksVersion-1, markTable, settingColumn, roleHolderMarksVersion)
	}

	if err := store.migrate(ctx, migrationsThrough(t, roleHolderMarksVersion)); err != nil {
		t.Fatalf("migrate to %d: %v", roleHolderMarksVersion, err)
	}

	rows, err := store.Pool.Query(ctx, `select project_key from project_role_holder_marks order by project_key`)
	if err != nil {
		t.Fatalf("read the marks after %d: %v", roleHolderMarksVersion, err)
	}
	marked, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the marks after %d: %v", roleHolderMarksVersion, err)
	}
	if !slices.Equal(marked, []string{"HELD"}) {
		t.Errorf("projects marked after %d = %v, want [HELD]: the one project with a role holder, once", roleHolderMarksVersion, marked)
	}

	for _, project := range []string{"HELD", "BARE"} {
		var proven bool
		if err := store.Pool.QueryRow(ctx, `select proven_sessions from projects where key = $1`, project).Scan(&proven); err != nil {
			t.Fatalf("read %s's proven_sessions after %d: %v", project, roleHolderMarksVersion, err)
		}
		if proven {
			t.Errorf("%s's proven_sessions after %d = true, want false, the default", project, roleHolderMarksVersion)
		}
	}
}
