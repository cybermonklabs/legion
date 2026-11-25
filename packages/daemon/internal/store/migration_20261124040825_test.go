package store

import (
	"context"
	"strings"
	"testing"
)

// admissionCapVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const admissionCapVersion = 20261124040825

// 20261124040825 adds project_admission_cap_changes, every change of the cap made while the daemon
// runs. Its checks refuse a cap of 0 (a clear names no cap), a negative cap before or after, a
// ceiling of 0, an actor of neither kind, a controller with no session, an operator with one, a
// blank reason and a project that is no Dispatch key; the rows they admit, a set and a clear, read
// back as written.
//
// The negative control is the read of the table before the migration, which the database refuses
// as a relation it does not know. Each refused insert is the control of its own check: the
// migration without that check writes the row, and this test fails naming it.
func TestMigrationAdmissionCap(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, admissionCapVersion-1)

	var rows int
	err := store.Pool().QueryRow(ctx, `select count(*) from project_admission_cap_changes`).Scan(&rows)
	if err == nil || !strings.Contains(err.Error(), "project_admission_cap_changes") {
		t.Fatalf("reading project_admission_cap_changes before the migration = %v, want the relation to be unknown", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the admission cap: %v", err)
	}

	const columns = `insert into project_admission_cap_changes
		(project, cap, cap_before, cap_after, ceiling, by_kind, by_session, reason, changed_at) values `
	for _, refused := range []struct{ name, values string }{
		{"a cap of 0", `('LEGION', 0, 4, 0, 4, 'operator', '', 'why', now())`},
		{"a negative cap before", `('LEGION', 2, -1, 2, 4, 'operator', '', 'why', now())`},
		{"a negative cap after", `('LEGION', 2, 4, -1, 4, 'operator', '', 'why', now())`},
		{"a ceiling of 0", `('LEGION', 2, 4, 2, 0, 'operator', '', 'why', now())`},
		{"an actor of neither kind", `('LEGION', 2, 4, 2, 4, 'reviewer', '', 'why', now())`},
		{"a controller with no session", `('LEGION', 2, 4, 2, 4, 'controller', '', 'why', now())`},
		{"an operator with a session", `('LEGION', 2, 4, 2, 4, 'operator', 'ses_1', 'why', now())`},
		{"a blank reason", `('LEGION', 2, 4, 2, 4, 'operator', '', ' ', now())`},
		{"a project that is no Dispatch key", `('legion', 2, 4, 2, 4, 'operator', '', 'why', now())`},
	} {
		if _, err := store.Pool().Exec(ctx, columns+refused.values); err == nil {
			t.Errorf("%s was written, want the table to refuse it", refused.name)
		}
	}

	if _, err := store.Pool().Exec(ctx, columns+`('LEGION', 6, 8, 6, 8, 'controller', 'ses_controller', 'the merge queue waits', now()),
		('LEGION', null, 6, 8, 8, 'operator', '', 'the signals are normal', now())`); err != nil {
		t.Fatalf("write a set and a clear: %v", err)
	}
	var sets, clears int
	if err := store.Pool().QueryRow(ctx, `select count(cap), count(*) - count(cap) from project_admission_cap_changes
		where project = 'LEGION'`).Scan(&sets, &clears); err != nil {
		t.Fatalf("read the changes back: %v", err)
	}
	if sets != 1 || clears != 1 {
		t.Errorf("the table holds %d sets and %d clears, want one of each", sets, clears)
	}
}
