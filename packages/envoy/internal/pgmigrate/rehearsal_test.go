package pgmigrate

import (
	"strings"
	"testing"
	"testing/fstest"
)

// RehearseOwn decides each shape of migration that its doc comment names. It reads a migration
// with Postgres's own parser in process (parseStatements). It judges a literal with no catalog.
// So the test needs no database. Each row loads its text as Load loads a file, so the
// non-transactional marker of the concurrent index build sets Migration.NonTransactional. tables
// are the tables that the rehearsed change reads and writes. A refusal names its first statement
// of another kind, so each refusing row gives the words of its reason that name that statement.
func TestRehearseOwnDecidesEachShapeOfAMigration(t *testing.T) {
	tables := []string{"moved_nodes", "moved_tuples"}
	const (
		createTable = "create table fresh (id bigint primary key, kind text not null);\n"
		createIndex = "create index fresh_kind on fresh (kind);\n"
		insert      = "insert into moved_tuples (id, kind) values (1, 'a'), (2, 'b');\n"
		replaceView = "create or replace view moved_view as select id, kind from moved_nodes;\n"
	)
	for _, row := range []struct {
		name string
		sql  string
		want RehearsalStep
		// why is the words a refusal's reason must hold, empty for a step that is no refusal.
		why string
	}{
		{"a create table", createTable, RehearsalApplies, ""},
		{"a create index on a table the migration creates", createTable + createIndex, RehearsalApplies, ""},
		// IF NOT EXISTS on the create index changes nothing: the table is still the migration's
		// new table, and its rows are none.
		{"a create index with if not exists on a table the migration creates",
			createTable + "create index if not exists fresh_kind on fresh (kind);\n", RehearsalApplies, ""},
		{"a bounded insert of literal values", insert, RehearsalApplies, ""},
		{"a create table, a create index on it and a bounded insert", createTable + createIndex + insert, RehearsalApplies, ""},

		{"a concurrent index build, loaded as non-transactional",
			"-- pgmigrate:non-transactional\ncreate index concurrently if not exists moved_nodes_kind on moved_nodes (kind);\n", RehearsalSkips, ""},
		{"a view replacement beside a write of a table not in tables",
			replaceView + "update elsewhere set note = 'x' where id = 1;\n", RehearsalSkips, ""},

		{"a create trigger after a create table",
			createTable + "create trigger fresh_owed after insert on fresh for each row execute function owe();\n", RehearsalRefuses, "it holds create trigger,"},
		{"a create table beside a view replacement", createTable + replaceView, RehearsalRefuses,
			"it holds both statements the rehearsal runs and view replacements it leaves out"},
		{"an insert of a value that is a cast", "insert into moved_tuples (id, kind) values (1, 'a'::text);\n", RehearsalRefuses,
			"it holds an insert of moved_tuples that is no bounded insert,"},
		{"an insert of a value that is a call", "insert into moved_tuples (id, kind) values (1, now());\n", RehearsalRefuses,
			"it holds an insert of moved_tuples that is no bounded insert,"},
		{"a create index on a table the migration does not create", "create index moved_nodes_kind on moved_nodes (kind);\n", RehearsalRefuses,
			`it holds a create index on "moved_nodes", which the migration does not create,`},
		// IF NOT EXISTS is the create table's (createsTable): the table can already exist and hold
		// rows, so the create index is on a table the migration does not create.
		{"a create index on a table that a create table with if not exists names",
			"create table if not exists fresh (id bigint primary key, kind text not null);\n" + createIndex, RehearsalRefuses,
			`it holds a create index on "fresh", which the migration does not create,`},
		{"a temporary table", "create temporary table scratch (id bigint);\n", RehearsalRefuses, "it holds a create of a temporary table,"},
		{"an alter table", "alter table moved_nodes add column note text;\n", RehearsalRefuses, "it holds alter table,"},
		{"a comment and no statement", "-- create table fresh (id bigint);\n", RehearsalRefuses, "Postgres's parser reads no statement of it"},
		{"a statement Postgres refuses", "create table fresh (;\n", RehearsalRefuses, "Postgres's parser reads no statement of it"},
		{"a view replacement beside a write of a table in tables",
			replaceView + "update moved_tuples set kind = 'x' where id = 1;\n", RehearsalRefuses,
			"it holds an update of moved_tuples that is no bounded insert,"},
	} {
		t.Run(row.name, func(t *testing.T) {
			migrations, err := Load(fstest.MapFS{"migrations/0001_row.up.sql": {Data: []byte(row.sql)}}, "migrations")
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			step, why := RehearseOwn(migrations[0], tables)
			if step != row.want || !strings.Contains(why, row.why) {
				t.Errorf("RehearseOwn = %s, %q; want %s with a reason that holds %q", step, why, row.want, row.why)
			}
		})
	}
}
