package testpg

import (
	"context"
	"maps"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Querier is what TableRows reads through: the store's pool (store.Pool, a CloneStore's) and a pgx
// pool (ClonePool's) both answer it.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// TableRows is every base table of the database's current schema with its rows: each table's as
// the text of each of its rows, sorted, so two reads differ for a table where a row was written,
// deleted or changed in place. A test that says something wrote nothing compares a read before it
// with a read after it (ChangedTables).
//
// A row's text is the row's own (`x.*::text`), which each column's type writes. It is neither
// converted to JSON, which would make two values one wherever jsonb normalises them (a json
// column's spacing, key order and repeated keys), nor hashed, which would only hide the rows that
// differ. The `.*` makes x the row, even in a table that has a column named x.
//
// It fails the test when the schema holds no table. Two reads of no table are equal, so each "no
// table changed" a test asserts would pass whatever was written. A migrated clone holds every
// table of the schema, so a read that finds none reads the wrong database or schema.
func TableRows(t testing.TB, db Querier) map[string]string {
	t.Helper()
	ctx := context.Background()
	rows, err := db.Query(ctx, `select table_name from information_schema.tables
		where table_schema = current_schema() and table_type = 'BASE TABLE' order by table_name`)
	if err != nil {
		t.Fatalf("list the tables: %v", err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("list the tables: %v", err)
	}
	if len(tables) == 0 {
		t.Fatalf("list the tables: the current schema holds none, want the tables a write could reach")
	}
	read := make(map[string]string, len(tables))
	for _, table := range tables {
		var text string
		if err := db.QueryRow(ctx, `select coalesce(array_agg(x.*::text order by x.*::text), '{}')::text from `+
			pgx.Identifier{table}.Sanitize()+` x`).Scan(&text); err != nil {
			t.Fatalf("read %s: %v", table, err)
		}
		read[table] = text
	}
	return read
}

// ChangedTables is each table whose rows differ between two reads of TableRows, in name order. A
// table that one read holds and the other does not counts as changed: a table made or dropped
// between the reads.
func ChangedTables(before, after map[string]string) []string {
	tables := slices.Concat(slices.Collect(maps.Keys(before)), slices.Collect(maps.Keys(after)))
	slices.Sort(tables)
	var changed []string
	for _, table := range slices.Compact(tables) {
		was, inBefore := before[table]
		is, inAfter := after[table]
		if inBefore != inAfter || was != is {
			changed = append(changed, table)
		}
	}
	return changed
}
