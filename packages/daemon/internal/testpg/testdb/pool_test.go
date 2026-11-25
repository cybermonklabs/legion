package testdb

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// A database a test leaves a table, a row, a moved sequence and a changed database setting in is
// reset and read back before another test takes it, so the next taker sees none of them.
//
// The shape is a copy of a migrated template, as internal/testpg's clones are: its reset truncates
// tables and keeps them, so a table a test created survives the reset, and the reading is what
// stops it. A database setting (`alter database … set`) survives every reset of either shape,
// since it belongs to no schema. The planting is what a test can do to its own database.
//
// The second half is the control: the same planting, given back with the reading switched off,
// reaches the next taker. Without it this test could pass on a reading that sees nothing.
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
	template := migratedLikeTemplate(t, admin, dsn)
	s := shape{template: template, prefix: "legion_pool_contamination_test"}

	// With the reading: the planted database is dropped, and the next taker sees nothing of it.
	first, giveBack, err := take(dsn, s)
	if err != nil {
		t.Fatal(err)
	}
	firstName := databaseOf(t, first)
	plant(t, first, firstName)
	if err := giveBack(); err != nil {
		t.Fatalf("give the planted database back: %v", err)
	}
	second, giveSecond, err := take(dsn, s)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = giveSecond() })
	if databaseOf(t, second) == firstName {
		t.Errorf("the next taker was handed %s, the database the planting changed", firstName)
	}
	if columns := columnsOf(t, second, "planted"); columns != "" {
		t.Errorf("the next taker's database holds the planted table (%s)", columns)
	}
	if setting := settingOf(t, second); setting != "" {
		t.Errorf("the next taker's database carries the planted setting %q", setting)
	}
	if rows := keptRows(t, second); rows != 0 {
		t.Errorf("the next taker's database holds %d rows in the kept table, want none", rows)
	}

	// The control: the same planting, reset but not read back, reaches the next taker.
	third, _, err := take(dsn, s)
	if err != nil {
		t.Fatal(err)
	}
	thirdName := databaseOf(t, third)
	plant(t, third, thirdName)
	if err := reset(admin, s, thirdName); err != nil {
		t.Fatalf("reset %s: %v", thirdName, err)
	}
	pool.mu.Lock()
	pool.free[s] = append(pool.free[s], thirdName)
	pool.mu.Unlock()
	fourth, _, err := take(dsn, s)
	if err != nil {
		t.Fatal(err)
	}
	if databaseOf(t, fourth) != thirdName {
		t.Fatalf("the control took %s, not the database it put back (%s)", databaseOf(t, fourth), thirdName)
	}
	if columns := columnsOf(t, fourth, "planted"); columns == "" {
		t.Error("reset without the reading removed the planted table, so this control proves nothing about the reading")
	}
	if setting := settingOf(t, fourth); setting == "" {
		t.Error("reset without the reading removed the planted setting, so this control proves nothing about the reading")
	}
	if rows := keptRows(t, fourth); rows != 0 {
		t.Errorf("the control's database holds %d rows in the kept table: the reset empties tables, and the reading catches what reset cannot", rows)
	}
	// The control's database carries the planting, so it never returns to the pool.
	if err := drop(admin, thirdName, nil); err != nil {
		t.Fatalf("drop the control's database: %v", err)
	}
	if _, err := admin.Exec(ctx, "select 1"); err != nil {
		t.Fatalf("the admin pool after the control: %v", err)
	}
}

// migratedLikeTemplate is a template shaped as a migrated one: a table of its own with an identity
// and a schema_version holding one row, which a reset keeps. It is dropped when the test ends.
func migratedLikeTemplate(t *testing.T, admin *pgxpool.Pool, dsn string) string {
	t.Helper()
	ctx := context.Background()
	name, err := RandomName("legion_pool_template_test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "create database "+name+" strategy file_copy"); err != nil {
		t.Fatalf("create the template: %v", err)
	}
	t.Cleanup(func() { _ = Drop(context.Background(), admin, name) })
	at, err := DatabaseURL(dsn, name)
	if err != nil {
		t.Fatal(err)
	}
	template, err := pgxpool.New(ctx, at)
	if err != nil {
		t.Fatal(err)
	}
	defer template.Close()
	for _, statement := range []string{
		`create table schema_version (version bigint primary key)`,
		`insert into schema_version (version) values (20990101000001)`,
		`create table kept (id bigserial primary key, note text not null default '')`,
	} {
		if _, err := template.Exec(ctx, statement); err != nil {
			t.Fatalf("build the template, %q: %v", statement, err)
		}
	}
	return name
}

// plant does to the database at what a test can do to its own: a row in a table the template
// holds, a sequence moved on, a table of its own, and a database setting.
func plant(t *testing.T, at, name string) {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), at)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, statement := range []string{
		`insert into kept (note) values ('a row of the test before')`,
		`select setval(pg_get_serial_sequence('kept', 'id'), 5000)`,
		`create table planted (id bigserial primary key, left_behind text not null)`,
		`alter database ` + name + ` set work_mem = '7MB'`,
	} {
		if _, err := pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("plant %q: %v", statement, err)
		}
	}
}

// keptRows is the rows of the kept table in the database at.
func keptRows(t *testing.T, at string) int64 {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), at)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var rows int64
	if err := pool.QueryRow(context.Background(), `select count(*) from kept`).Scan(&rows); err != nil {
		t.Fatalf("count the kept rows: %v", err)
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

// settingOf is the settings the database at carries of its own, "" where it carries none.
func settingOf(t *testing.T, at string) string {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), at)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var settings string
	if err := pool.QueryRow(context.Background(), `select coalesce(string_agg(setting, ','), '')
		from pg_db_role_setting d, unnest(d.setconfig) setting
		where d.setdatabase = (select oid from pg_database where datname = current_database())`).Scan(&settings); err != nil {
		t.Fatalf("read the settings: %v", err)
	}
	return settings
}

// databaseOf is the database a URL names.
func databaseOf(t *testing.T, at string) string {
	t.Helper()
	config, err := pgxpool.ParseConfig(at)
	if err != nil {
		t.Fatal(err)
	}
	return config.ConnConfig.Database
}
