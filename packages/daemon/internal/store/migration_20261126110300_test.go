package store

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/store/migrations"
)

// checkStatesVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const checkStatesVersion = 20261126110300

// checkStatesWayBack is the transaction the header of checkStatesVersion gives an operator to run
// through psql: its lines from `begin;` through `commit;`, each without the header's marker. A
// header that carries no such block stops the test, since a block this cannot find is one it would
// pass while running nothing.
func checkStatesWayBack(t *testing.T) string {
	t.Helper()
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	for _, migration := range all {
		if migration.Version != checkStatesVersion {
			continue
		}
		var block []string
		for _, line := range strings.Split(migration.SQL, "\n") {
			statement, marked := strings.CutPrefix(line, "--   ")
			switch {
			case marked && statement == "begin;":
				block = []string{statement}
			case block != nil && marked:
				block = append(block, statement)
				if statement == "commit;" {
					return strings.Join(block, "\n")
				}
			case block != nil:
				t.Fatalf("the way back in the header of %s ends before its commit;", migration.Name)
			}
		}
		t.Fatalf("the header of %s carries no transaction from begin; through commit;", migration.Name)
	}
	t.Fatalf("no embedded migration is %d", checkStatesVersion)
	return ""
}

// 20261126110300 creates the three tables of the check states, empty, and gives every issue the
// release before recorded a catch-up cursor of 0, so its first catch-up reads its event log from
// the start. The release before names none of them, and its insert of an issue still lands, at
// that cursor.
//
// The way back in the header, run as an operator runs it through psql (one exec, the simple
// protocol), drops the three tables and the column and deletes the migration's own row, keeping
// every issue; the roll-forward's Migrate then applies the migration again, and only it.
func TestTheCheckStatesMigrationAndItsWayBack(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, checkStatesVersion-1)
	exec := func(statement string) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, statement, pgx.QueryExecModeSimpleProtocol); err != nil {
			t.Fatalf("run %q: %v", statement, err)
		}
	}
	// The release before's insert names none of the new columns.
	const releaseBeforesInsert = `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq) values `
	exec(releaseBeforesInsert + `('ACME-1', 'ACME-1', 'ACME', 'root', 'testing', 1, 'in_progress', 'A', 3)`)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the check states: %v", err)
	}
	exec(releaseBeforesInsert + `('ACME-2', 'ACME-2', 'ACME', 'root', 'testing', 1, 'in_progress', 'B', 4)`)
	const cursors = `select row(key, check_states_seq)::text from issues order by key`
	if got, want := rowsAsText(t, store, cursors), []string{"(ACME-1,0)", "(ACME-2,0)"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the catch-up cursors after the migration = %v, want %v", got, want)
	}
	const tables = `select coalesce(to_regclass(name)::text, '') from unnest(array['check_states', 'check_state_sets', 'check_state_serials'])
		with ordinality as listed(name, place) order by place`
	if got, want := rowsAsText(t, store, tables), []string{"check_states", "check_state_sets", "check_state_serials"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the tables after the migration = %v, want %v", got, want)
	}
	exec(`insert into check_states (issue, repo, head, check_id, state, dispatch_seq) values ('ACME-1', 'acme/widgets', 'h', 'k1', 'yes', 5)`)
	exec(`insert into check_state_sets (issue, repo, head, checks, dispatch_seq) values ('ACME-1', 'acme/widgets', 'h', '{k1}', 5)`)
	exec(`insert into check_state_serials (issue, serial) values ('ACME-1', 1)`)

	exec(checkStatesWayBack(t))
	const left = `select row((select count(*) from issues), (select count(*) from schema_version where version = 20261126110300),
		(select count(*) from information_schema.columns where table_schema = current_schema() and table_name = 'issues' and column_name = 'check_states_seq'))::text`
	if got, want := rowsAsText(t, store, tables), []string{"", "", ""}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the tables after the way back = %v, want none of the three", got)
	}
	if got, want := rowsAsText(t, store, left), []string{"(2,0,0)"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after the way back (issues, the migration's row, the cursor column) = %v, want %v: both issues kept, the row and the column gone", got, want)
	}

	applied, err := store.Migrate(ctx)
	if err != nil || applied != 1 {
		t.Fatalf("the roll-forward's migrate applied %d, %v; want the check states' migration alone", applied, err)
	}
	if got, want := rowsAsText(t, store, cursors), []string{"(ACME-1,0)", "(ACME-2,0)"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the catch-up cursors after the roll-forward = %v, want %v", got, want)
	}
	if got, want := rowsAsText(t, store, `select count(*)::text from check_states`), []string{"0"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("check_states after the roll-forward holds %v rows, want none", got)
	}
}
