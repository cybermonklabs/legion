package testpg

import (
	"context"
	"maps"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
)

// testdb.TableRows reads every table of a migrated clone: each table the server's own catalog
// (pg_tables) lists outside the system schemas stands in the current schema, which TableRows lists,
// and TableRows reads each of them. A table it left out is one whose writes no test that compares
// two reads can see. The row helpers and their other tests are testdb's, since they need no schema;
// this one needs the daemon's, so it stays here.
//
// The fault it catches is a listing that leaves a table out (its control: TableRows' listing
// without issue_plans, which this test names as the one table missing).
func TestTableRowsReadsEveryTableOfTheDatabase(t *testing.T) {
	pool := ClonePool(t, "legion_testpg_rows_test")
	rows, err := pool.Query(context.Background(), `select schemaname || '.' || tablename from pg_tables
		where schemaname not in ('pg_catalog', 'information_schema')`)
	if err != nil {
		t.Fatalf("list the catalog's tables: %v", err)
	}
	catalog, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("list the catalog's tables: %v", err)
	}
	slices.Sort(catalog)
	var schema string
	if err := pool.QueryRow(context.Background(), `select current_schema()`).Scan(&schema); err != nil {
		t.Fatalf("read the current schema: %v", err)
	}
	var read []string
	for _, table := range slices.Sorted(maps.Keys(testdb.TableRows(t, pool))) {
		read = append(read, schema+"."+table)
	}
	if !slices.Equal(read, catalog) {
		var missing []string
		for _, table := range catalog {
			if !slices.Contains(read, table) {
				missing = append(missing, table)
			}
		}
		t.Fatalf("TableRows read %d tables and the catalog lists %d; it left out %q", len(read), len(catalog), missing)
	}
}
