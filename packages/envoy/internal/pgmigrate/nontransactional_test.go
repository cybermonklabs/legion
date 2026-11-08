package pgmigrate

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/testpg"
)

func TestLoadNonTransactionalMigration(t *testing.T) {
	for name, sql := range map[string]string{
		"one index":         "create index concurrently if not exists things_kind on things (kind);",
		"literal semicolon": "create index concurrently if not exists things_kind on things ((kind || ';')); -- ;\n",
	} {
		t.Run(name, func(t *testing.T) {
			migrations, err := Load(fstest.MapFS{"migrations/0001_index.up.sql": {Data: []byte("-- pgmigrate:non-transactional\n" + sql)}}, "migrations")
			if err != nil {
				t.Fatal(err)
			}
			if !migrations[0].NonTransactional {
				t.Fatal("header did not select execution outside a transaction")
			}
		})
	}
}

func TestLoadDoesNotReadAMarkerInsideSQLAsAnExecutionMode(t *testing.T) {
	for _, sql := range []string{
		"-- ordinary header\n-- pgmigrate:non-transactional\nselect 1; select 2;",
		"select $$\n-- pgmigrate:non-transactional\n$$;",
	} {
		migrations, err := Load(fstest.MapFS{"migrations/0001_normal.up.sql": {Data: []byte(sql)}}, "migrations")
		if err != nil || migrations[0].NonTransactional {
			t.Fatalf("ordinary migration = %+v, %v", migrations, err)
		}
	}
}

func TestLoadRefusesUnsafeNonTransactionalMigrations(t *testing.T) {
	for name, sql := range map[string]string{
		"two statements":      "create index concurrently if not exists i on things (kind); select 1;",
		"not idempotent":      "create index concurrently i on things (kind);",
		"not concurrent":      "create index if not exists i on things (kind);",
		"transaction control": "begin;",
		"no statement":        "-- nothing\n",
		"invalid SQL":         "create index concurrently if not exists",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := Load(fstest.MapFS{"migrations/0001_index.up.sql": {Data: []byte("-- pgmigrate:non-transactional\n" + sql)}}, "migrations")
			if err == nil || len(got) != 0 || !strings.Contains(err.Error(), "0001_index.up.sql") {
				t.Fatalf("Load = %v, %v; want whole-set refusal naming the file", got, err)
			}
		})
	}
}

func TestLoadRefusesUniqueConcurrentIndex(t *testing.T) {
	set := fstest.MapFS{
		"migrations/0001_first.up.sql":  {Data: []byte("select 1")},
		"migrations/0002_unique.up.sql": {Data: []byte("-- pgmigrate:non-transactional\ncreate unique index concurrently if not exists things_kind on things (kind)")},
	}
	migrations, err := Load(set, "migrations")
	if err == nil || len(migrations) != 0 || !strings.Contains(err.Error(), "0002_unique.up.sql") {
		t.Fatalf("Load = %v, %v; want whole-set refusal of the unique concurrent index", migrations, err)
	}
}

func TestExecNonTransactionalBuildsAnIndexAndRestoresTimeout(t *testing.T) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, testpg.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	if _, err := conn.Exec(ctx, `create table things (kind text);
		create function check_build_settings() returns event_trigger language plpgsql as $$
		begin
			if current_setting('lock_timeout') <> '5s' or current_setting('standard_conforming_strings') <> 'on' then
				raise exception 'build ran without its session settings';
			end if;
		end $$;
		create event trigger check_build_settings on ddl_command_start when tag in ('CREATE INDEX') execute function check_build_settings();
		set lock_timeout = '123ms'; set standard_conforming_strings = off`); err != nil {
		t.Fatal(err)
	}
	migration := Migration{Name: "0001_index.up.sql", SQL: "create index concurrently if not exists things_kind on things (kind)", NonTransactional: true}
	if err := ExecNonTransactional(ctx, conn, migration, nil); err != nil {
		t.Fatal(err)
	}
	if err := ExecNonTransactional(ctx, conn, migration, nil); err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	var valid bool
	if err := conn.QueryRow(ctx, "select indisvalid from pg_index where indexrelid = 'things_kind'::regclass").Scan(&valid); err != nil || !valid {
		t.Fatalf("index valid = %v, %v", valid, err)
	}
	var timeout string
	if err := conn.QueryRow(ctx, "show lock_timeout").Scan(&timeout); err != nil || timeout != "123ms" {
		t.Fatalf("lock_timeout = %q, %v", timeout, err)
	}
	var stringsSetting string
	if err := conn.QueryRow(ctx, "show standard_conforming_strings").Scan(&stringsSetting); err != nil || stringsSetting != "off" {
		t.Fatalf("standard_conforming_strings = %q, %v", stringsSetting, err)
	}
}

// indexState reads one relation of the given name: its oid, whether it is an index, whether that
// index is valid and the table it is on.
func indexState(t *testing.T, ctx context.Context, conn *pgx.Conn, name string) (oid uint32, isIndex bool, valid bool, table string) {
	t.Helper()
	var kind string
	var indisvalid *bool
	var onTable *string
	err := conn.QueryRow(ctx, `select c.oid, c.relkind, i.indisvalid, t.relname from pg_class c
		left join pg_index i on i.indexrelid = c.oid
		left join pg_class t on t.oid = i.indrelid
		where c.relname = $1 and c.relnamespace = 'public'::regnamespace`, name).Scan(&oid, &kind, &indisvalid, &onTable)
	if err != nil {
		t.Fatalf("read relation %s: %v", name, err)
	}
	if indisvalid != nil {
		valid = *indisvalid
	}
	if onTable != nil {
		table = *onTable
	}
	return oid, kind == "i", valid, table
}

func relationMissing(t *testing.T, ctx context.Context, conn *pgx.Conn, name string) bool {
	t.Helper()
	var n int
	if err := conn.QueryRow(ctx, "select count(*) from pg_class where relname = $1 and relnamespace = 'public'::regnamespace", name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 0
}

func indexMigration() Migration {
	return Migration{Name: "0001_index.up.sql", SQL: "create index concurrently if not exists things_kind on things (kind)", NonTransactional: true}
}

// leaveInvalidIndex leaves a real INVALID index of the given name on the given table, the way a
// failed concurrent build does: a UNIQUE concurrent build over duplicate rows fails at once.
func leaveInvalidIndex(t *testing.T, ctx context.Context, conn *pgx.Conn, name, table, column string) {
	t.Helper()
	if _, err := conn.Exec(ctx, "create unique index concurrently "+name+" on "+table+" ("+column+")"); err == nil {
		t.Fatalf("unique index %s accepted duplicate rows", name)
	}
	oid, isIndex, valid, on := indexState(t, ctx, conn, name)
	if !isIndex || valid || on != table {
		t.Fatalf("fixture %s: oid=%d index=%v valid=%v on=%s, want an INVALID index on %s", name, oid, isIndex, valid, on, table)
	}
	t.Logf("fixture: pg_index %s oid=%d indisvalid=%v on=%s", name, oid, valid, on)
}

// A build the runner itself started and lost to lock_timeout leaves an INVALID index of the
// migration's own name on the migration's own table. The next attempt must drop it and build again.
func TestExecNonTransactionalRebuildsAfterATimedOutBuild(t *testing.T) {
	ctx := context.Background()
	url := testpg.URL(t)
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	if _, err := conn.Exec(ctx, "create table things (kind text); insert into things values ('a'), ('b')"); err != nil {
		t.Fatal(err)
	}
	holder, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { holder.Close(context.Background()) })
	tx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// A write holds a snapshot the concurrent build must wait for, without conflicting with the
	// build's own SHARE UPDATE EXCLUSIVE: the build waits and is cancelled at lock_timeout.
	if _, err := tx.Exec(ctx, "insert into things values ('x')"); err != nil {
		t.Fatal(err)
	}
	migration := indexMigration()
	var attempts int
	var leftover uint32
	runErr := RetryLockTimeouts(ctx, migration.Name, func(ctx context.Context) error {
		attempts++
		err := ExecNonTransactional(ctx, conn, migration, nil)
		if attempts == 1 {
			oid, isIndex, valid, on := indexState(t, ctx, conn, "things_kind")
			if !isIndex || valid || on != "things" {
				t.Fatalf("first attempt left oid=%d index=%v valid=%v on=%s, want an INVALID index on things", oid, isIndex, valid, on)
			}
			t.Logf("after attempt 1: pg_index things_kind oid=%d indisvalid=%v on=%s; error %v", oid, valid, on, err)
			leftover = oid
			if rollback := tx.Rollback(ctx); rollback != nil {
				t.Fatal(rollback)
			}
		}
		return err
	})
	if runErr != nil {
		t.Fatalf("RetryLockTimeouts = %v, want nil after the leftover is dropped and the index built again", runErr)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	oid, isIndex, valid, on := indexState(t, ctx, conn, "things_kind")
	if !isIndex || !valid || on != "things" {
		t.Fatalf("things_kind: oid=%d index=%v valid=%v on=%s, want a valid index on things", oid, isIndex, valid, on)
	}
	if oid == leftover {
		t.Fatalf("things_kind oid %d is the leftover's: it was revalidated, not rebuilt", oid)
	}
}

// A build killed outside the runner leaves the same INVALID index of the migration's own name;
// one ExecNonTransactional drops it and builds again.
func TestExecNonTransactionalDropsTheLeftoverOfAKilledBuild(t *testing.T) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, testpg.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	if _, err := conn.Exec(ctx, "create table things (kind text); insert into things values ('same'), ('same')"); err != nil {
		t.Fatal(err)
	}
	leaveInvalidIndex(t, ctx, conn, "things_kind", "things", "kind")
	leftover, _, _, _ := indexState(t, ctx, conn, "things_kind")
	if err := ExecNonTransactional(ctx, conn, indexMigration(), nil); err != nil {
		t.Fatalf("ExecNonTransactional = %v, want nil after dropping its own leftover", err)
	}
	oid, isIndex, valid, on := indexState(t, ctx, conn, "things_kind")
	if !isIndex || !valid || on != "things" {
		t.Fatalf("things_kind: oid=%d index=%v valid=%v on=%s, want a valid index on things", oid, isIndex, valid, on)
	}
	if oid == leftover {
		t.Fatalf("things_kind oid %d is the leftover's: it was revalidated, not rebuilt", oid)
	}
}

// The runner drops only an index that is its own: its name, on its own table. Anything else of
// that name is a relation it did not build, and it refuses, naming DROP INDEX CONCURRENTLY.
func TestExecNonTransactionalRefusesAnIndexItDidNotBuild(t *testing.T) {
	refusal := func(t *testing.T, conn *pgx.Conn) {
		t.Helper()
		err := ExecNonTransactional(context.Background(), conn, indexMigration(), nil)
		if err == nil || !strings.Contains(err.Error(), "things_kind") || !strings.Contains(err.Error(), "DROP INDEX CONCURRENTLY") {
			t.Fatalf("ExecNonTransactional = %v, want a refusal naming things_kind and DROP INDEX CONCURRENTLY", err)
		}
	}
	t.Run("an INVALID index of the migration's name on another table", func(t *testing.T) {
		ctx := context.Background()
		conn, err := pgx.Connect(ctx, testpg.URL(t))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close(context.Background()) })
		if _, err := conn.Exec(ctx, `create table things (kind text); insert into things values ('a');
			create table others (kind text); insert into others values ('same'), ('same')`); err != nil {
			t.Fatal(err)
		}
		leaveInvalidIndex(t, ctx, conn, "things_kind", "others", "kind")
		refusal(t, conn)
		oid, isIndex, valid, on := indexState(t, ctx, conn, "things_kind")
		if !isIndex || valid || on != "others" {
			t.Fatalf("things_kind: oid=%d index=%v valid=%v on=%s, want the INVALID index on others untouched", oid, isIndex, valid, on)
		}
	})
	t.Run("a valid index of the migration's name on another table", func(t *testing.T) {
		ctx := context.Background()
		conn, err := pgx.Connect(ctx, testpg.URL(t))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close(context.Background()) })
		if _, err := conn.Exec(ctx, `create table things (kind text);
			create table others (kind text); create index things_kind on others (kind)`); err != nil {
			t.Fatal(err)
		}
		oid, isIndex, valid, on := indexState(t, ctx, conn, "things_kind")
		t.Logf("fixture: pg_index things_kind oid=%d index=%v indisvalid=%v on=%s", oid, isIndex, valid, on)
		refusal(t, conn)
		oid, isIndex, valid, on = indexState(t, ctx, conn, "things_kind")
		if !isIndex || !valid || on != "others" {
			t.Fatalf("things_kind: oid=%d index=%v valid=%v on=%s, want the valid index on others untouched", oid, isIndex, valid, on)
		}
	})
	t.Run("a non-index relation of the migration's name", func(t *testing.T) {
		ctx := context.Background()
		conn, err := pgx.Connect(ctx, testpg.URL(t))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close(context.Background()) })
		if _, err := conn.Exec(ctx, "create table things (kind text); create table things_kind (x int)"); err != nil {
			t.Fatal(err)
		}
		oid, isIndex, valid, on := indexState(t, ctx, conn, "things_kind")
		t.Logf("fixture: relation things_kind oid=%d index=%v indisvalid=%v on=%q", oid, isIndex, valid, on)
		refusal(t, conn)
		if relationMissing(t, ctx, conn, "things_kind") {
			t.Fatal("the refusal removed the table things_kind")
		}
	})
}

// An INVALID index of another name is another migration's or another operator's business: the
// runner neither drops it nor refuses over it, and its own build goes through.
func TestExecNonTransactionalLeavesAnIndexOfAnotherNameAlone(t *testing.T) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, testpg.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	if _, err := conn.Exec(ctx, "create table things (kind text, other text); insert into things values ('a', 'same'), ('b', 'same')"); err != nil {
		t.Fatal(err)
	}
	leaveInvalidIndex(t, ctx, conn, "things_other", "things", "other")
	other, _, _, _ := indexState(t, ctx, conn, "things_other")
	if err := ExecNonTransactional(ctx, conn, indexMigration(), nil); err != nil {
		t.Fatalf("ExecNonTransactional = %v, want nil: the INVALID index is not the migration's", err)
	}
	oid, isIndex, valid, on := indexState(t, ctx, conn, "things_kind")
	if !isIndex || !valid || on != "things" {
		t.Fatalf("things_kind: oid=%d index=%v valid=%v on=%s, want a valid index on things", oid, isIndex, valid, on)
	}
	leftOid, leftIsIndex, leftValid, leftOn := indexState(t, ctx, conn, "things_other")
	if leftOid != other || !leftIsIndex || leftValid || leftOn != "things" {
		t.Fatalf("things_other: oid=%d index=%v valid=%v on=%s, want the INVALID index (oid %d) untouched", leftOid, leftIsIndex, leftValid, leftOn, other)
	}
}

// The DROP of the runner's own leftover takes locks too. One that outlasts lock_timeout must come
// back as a *LockTimeoutError, so RetryLockTimeouts runs the whole attempt again.
func TestExecNonTransactionalRetriesADropThatTimesOut(t *testing.T) {
	ctx := context.Background()
	url := testpg.URL(t)
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	if _, err := conn.Exec(ctx, "create table things (kind text); insert into things values ('same'), ('same')"); err != nil {
		t.Fatal(err)
	}
	leaveInvalidIndex(t, ctx, conn, "things_kind", "things", "kind")
	holder, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { holder.Close(context.Background()) })
	tx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Rollback(context.Background()) })
	// DROP INDEX CONCURRENTLY needs SHARE UPDATE EXCLUSIVE on the table, which this holder has.
	if _, err := tx.Exec(ctx, "lock table things in share update exclusive mode"); err != nil {
		t.Fatal(err)
	}
	err = ExecNonTransactional(ctx, conn, indexMigration(), nil)
	var timedOut *LockTimeoutError
	if !errors.As(err, &timedOut) {
		t.Fatalf("ExecNonTransactional = %v, want a *LockTimeoutError from the DROP of the leftover", err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "drop") {
		t.Fatalf("error %q does not say the DROP of the leftover timed out", err)
	}
}
