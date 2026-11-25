package testpg

import (
	"context"
	"fmt"
	"maps"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
)

// TableRows sees a row written, a row deleted and a row changed in place, each in the one table it
// was in, and an update that writes a row's own values again changes nothing it reads. The table
// has a json column, whose key order an update changes, and a column named x, the alias TableRows
// reads each row as: a read that turned the rows into jsonb, or read the column x in place of the
// row, would see no change there.
//
// The faults it catches: a row's text that leaves out its values (its control: TableRows reading
// each table's row count, under which the change in place and the reordered keys are not seen), a
// row read as jsonb (its control: TableRows reading `to_jsonb(x.*)`, the encoding workflow's copy
// had, under which the reordered keys are not seen), the column x read in place of the row (its
// control: `x::text` without `.*`, under which the change in place and the reordered keys are not
// seen), and a comparison that names no table (its control: ChangedTables answering nil).
func TestTableRowsSeesARowWrittenDeletedOrChangedInPlace(t *testing.T) {
	pool := ClonePool(t, "legion_testpg_rows_test")
	exec := func(statement string) {
		t.Helper()
		if _, err := pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	exec(`create table rows_probe (id int primary key, x int not null, body text not null, doc json not null)`)
	exec(`insert into rows_probe values (1, 7, 'first', '{"a": 1, "b": 2}')`)
	for _, step := range []struct {
		what, statement string
		changed         []string
	}{
		{"a row written", `insert into rows_probe values (2, 7, 'second', '{}')`, []string{"rows_probe"}},
		{"a row changed in place", `update rows_probe set body = 'changed' where id = 1`, []string{"rows_probe"}},
		{"a json value's keys reordered in place", `update rows_probe set doc = '{"b": 2, "a": 1}' where id = 1`, []string{"rows_probe"}},
		{"a row deleted", `delete from rows_probe where id = 2`, []string{"rows_probe"}},
		{"a row's own values written again", `update rows_probe set body = body, x = x, doc = doc`, nil},
	} {
		before := TableRows(t, pool)
		exec(step.statement)
		if changed := ChangedTables(before, TableRows(t, pool)); !slices.Equal(changed, step.changed) {
			t.Errorf("%s changed the rows of %q, want %q", step.what, changed, step.changed)
		}
	}
}

// TableRows reads every table of the database, whichever schema holds it: what it lists is what
// the server's own catalog (pg_tables) lists outside the system schemas. A table it left out is
// one whose writes no test that compares two reads can see.
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
	for _, table := range slices.Sorted(maps.Keys(TableRows(t, pool))) {
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

// TableRows fails the test on a schema that holds no table, since two reads of no table are equal
// and every "no table changed" would pass. The database is an empty one, which holds no table.
//
// The fault it catches is a read that answers an empty map (its control: TableRows without its
// check of the listing, under which the read answers and this test fails).
func TestTableRowsFailsOnASchemaWithNoTable(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), testdb.Database(t, "legion_testpg_rows_test"))
	if err != nil {
		t.Fatalf("open a pool on the empty database: %v", err)
	}
	t.Cleanup(pool.Close)
	recorder := &fatalRecorder{TB: t}
	var read map[string]string
	done := make(chan struct{})
	go func() {
		defer close(done)
		read = TableRows(recorder, pool)
	}()
	<-done
	if want := "the current schema holds none"; !strings.Contains(recorder.message, want) {
		t.Fatalf("TableRows of an empty database answered %q and failed the test with %q, want it failed saying %q", read, recorder.message, want)
	}
}

// ChangedTables names a table that one read holds and the other does not, as it names one whose
// rows differ, and names nothing for two equal reads.
//
// The fault it catches is a comparison that ranges over one read's tables (its control:
// ChangedTables ranging over after's tables alone, as api's and workflow's copies did, under which
// the dropped table goes unnamed).
func TestChangedTablesNamesATableOnlyOneReadHolds(t *testing.T) {
	before := map[string]string{"kept": "{}", "dropped": "{}", "written": "{}"}
	after := map[string]string{"kept": "{}", "made": "{}", "written": `{"(1)"}`}
	if changed, want := ChangedTables(before, after), []string{"dropped", "made", "written"}; !slices.Equal(changed, want) {
		t.Errorf("ChangedTables = %q, want %q", changed, want)
	}
	if changed := ChangedTables(before, maps.Clone(before)); len(changed) != 0 {
		t.Errorf("ChangedTables of two equal reads = %q, want none", changed)
	}
}

// fatalRecorder is t with Fatalf recorded and the goroutine ended, as t.Fatalf does.
type fatalRecorder struct {
	testing.TB
	message string
}

func (f *fatalRecorder) Fatalf(format string, args ...any) {
	f.message = fmt.Sprintf(format, args...)
	runtime.Goexit()
}
