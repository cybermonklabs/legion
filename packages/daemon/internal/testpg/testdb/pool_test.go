package testdb

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// A database a test leaves rows, a moved sequence and an altered table in is reset and read back
// before another test takes it, so the next taker sees none of them. The planting is what a test
// does to its own database: an ordinary write, and two changes the reset cannot undo, which the
// reading is for.
//
// The second half is the control: with the reading switched off, the altered table reaches the
// next taker. It plants the same change, gives the database back through a give that skips
// verify, and reads the next taker's database — which is that same one, since the free list is a
// stack.
func TestADatabaseAnotherTestContaminatedIsNeverHandedOn(t *testing.T) {
	dsn := os.Getenv(dsnName)
	if dsn == "" {
		t.Skip(SkipMessage)
	}
	ctx := context.Background()
	admin, err := adminPool(dsn)
	if err != nil {
		t.Fatal(err)
	}
	s := shape{prefix: "legion_pool_contamination_test"}

	first, giveBack, err := take(dsn, s)
	if err != nil {
		t.Fatal(err)
	}
	firstName := databaseOf(t, first)
	plant(t, first)
	if err := giveBack(); err != nil {
		t.Fatalf("give the planted database back: %v", err)
	}

	second, giveSecond, err := take(dsn, s)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = giveSecond() })
	if rows := countRows(t, second, "planted"); rows != 0 {
		t.Errorf("the next taker's database holds %d planted rows, want none", rows)
	}
	if columns := columnsOf(t, second, "planted"); columns != "" {
		t.Errorf("the next taker's database holds the planted table (%s), want none", columns)
	}
	if databaseOf(t, second) == firstName {
		t.Errorf("the next taker was handed %s, the database the planting altered: a database the reading refuses is dropped", firstName)
	}

	// The control: the same planting, given back with the reading switched off, reaches the next
	// taker.
	third, _, err := take(dsn, s)
	if err != nil {
		t.Fatal(err)
	}
	thirdName := databaseOf(t, third)
	plant(t, third)
	if err := reset(admin, thirdName); err != nil {
		t.Fatalf("reset %s: %v", thirdName, err)
	}
	pool.mu.Lock()
	pool.free[s] = append(pool.free[s], thirdName)
	pool.mu.Unlock()

	fourth, giveFourth, err := take(dsn, s)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = giveFourth() })
	if databaseOf(t, fourth) != thirdName {
		t.Fatalf("the control took %s, not the database it put back (%s): the free list hands back the last database given", databaseOf(t, fourth), thirdName)
	}
	if columns := columnsOf(t, fourth, "planted"); columns == "" {
		t.Error("with the reading switched off the planted table did not reach the next taker, so this control proves nothing about the reading")
	}
	if rows := countRows(t, fourth, "planted"); rows != 0 {
		t.Errorf("the control's database holds %d planted rows: the reset empties tables, and the reading is what catches the table itself", rows)
	}
	// The control's database carries the planting, so it never returns to the pool.
	pool.mu.Lock()
	pool.free[s] = nil
	pool.mu.Unlock()
	_ = drop(admin, thirdName, nil)
	if _, err := admin.Exec(ctx, "select 1"); err != nil {
		t.Fatalf("the admin pool after the control: %v", err)
	}
}

// plant writes what a test leaves behind: a table of its own with a row in it, and a sequence
// moved on. The table is what the reading catches; the row is what the reset empties.
func plant(t *testing.T, at string) {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), at)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, statement := range []string{
		`create table planted (id bigserial primary key, left_behind text not null)`,
		`insert into planted (left_behind) values ('a row of the test before')`,
		`select setval(pg_get_serial_sequence('planted', 'id'), 5000)`,
	} {
		if _, err := pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("plant %q: %v", statement, err)
		}
	}
}

// countRows is the rows of table in the database at, and 0 where the table is not there.
func countRows(t *testing.T, at, table string) int64 {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), at)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var rows int64
	if err := pool.QueryRow(context.Background(),
		`select coalesce((select count(*) from planted), 0) where to_regclass($1) is not null union all select 0 limit 1`, table).Scan(&rows); err != nil {
		t.Fatalf("count the rows of %s: %v", table, err)
	}
	return rows
}

// columnsOf is the columns of table in the database at, and "" where the table is not there.
func columnsOf(t *testing.T, at, table string) string {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), at)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var columns string
	if err := pool.QueryRow(context.Background(), `select coalesce(string_agg(attname, ',' order by attnum), '')
		from pg_attribute where attrelid = to_regclass($1) and attnum > 0 and not attisdropped`, table).Scan(&columns); err != nil {
		t.Fatalf("read the columns of %s: %v", table, err)
	}
	return columns
}

// databaseOf is the database a URL names.
func databaseOf(t *testing.T, at string) string {
	t.Helper()
	config, err := pgxpool.ParseConfig(at)
	if err != nil {
		t.Fatal(err)
	}
	if config.ConnConfig.Database == "" {
		t.Fatalf("the URL %s names no database", strings.SplitN(at, "@", 2)[0])
	}
	return config.ConnConfig.Database
}
