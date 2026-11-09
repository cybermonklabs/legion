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

// connect opens one connection to url and closes it when t ends. The URL is the caller's, since
// testpg.URL makes a database of its own at each call and a test's two connections share one.
func connect(t *testing.T, ctx context.Context, url string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

func TestExecNonTransactionalBuildsAnIndexAndRestoresTimeout(t *testing.T) {
	ctx := context.Background()
	conn := connect(t, ctx, testpg.URL(t))
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

// relation is one relation of pg_class as indexState read it: its oid, whether it is an index,
// whether that index is valid and the table it is on. The zero relation is no relation of that
// name at all, since pg_class never gives a relation oid 0.
type relation struct {
	OID   uint32
	Index bool
	Valid bool
	Table string
}

// indexState reads the one relation of the given name in the public schema, the zero relation
// when there is none.
func indexState(t *testing.T, ctx context.Context, conn *pgx.Conn, name string) relation {
	t.Helper()
	var got relation
	var kind string
	var indisvalid *bool
	var onTable *string
	err := conn.QueryRow(ctx, `select c.oid, c.relkind, i.indisvalid, t.relname from pg_class c
		left join pg_index i on i.indexrelid = c.oid
		left join pg_class t on t.oid = i.indrelid
		where c.relname = $1 and c.relnamespace = 'public'::regnamespace`, name).Scan(&got.OID, &kind, &indisvalid, &onTable)
	if errors.Is(err, pgx.ErrNoRows) {
		return relation{}
	}
	if err != nil {
		t.Fatalf("read relation %s: %v", name, err)
	}
	got.Index = kind == "i"
	if indisvalid != nil {
		got.Valid = *indisvalid
	}
	if onTable != nil {
		got.Table = *onTable
	}
	return got
}

func indexMigration() Migration {
	return Migration{Name: "0001_index.up.sql", SQL: "create index concurrently if not exists things_kind on things (kind)", NonTransactional: true}
}

// leaveInvalidIndex leaves a real INVALID index of the given name on the given table, the way a
// failed concurrent build does: a UNIQUE concurrent build over duplicate rows fails at once.
func leaveInvalidIndex(t *testing.T, ctx context.Context, conn *pgx.Conn, name, table, column string) relation {
	t.Helper()
	if _, err := conn.Exec(ctx, "create unique index concurrently "+name+" on "+table+" ("+column+")"); err == nil {
		t.Fatalf("unique index %s accepted duplicate rows", name)
	}
	got := indexState(t, ctx, conn, name)
	if want := (relation{OID: got.OID, Index: true, Table: table}); got.OID == 0 || got != want {
		t.Fatalf("fixture %s = %+v, want an INVALID index on %s", name, got, table)
	}
	t.Logf("fixture: pg_index %s = %+v", name, got)
	return got
}

// A build the runner itself started and lost to lock_timeout leaves an INVALID index of the
// migration's own name on the migration's own table. The next attempt must drop it and build again.
func TestExecNonTransactionalRebuildsAfterATimedOutBuild(t *testing.T) {
	ctx := context.Background()
	url := testpg.URL(t)
	conn := connect(t, ctx, url)
	if _, err := conn.Exec(ctx, "create table things (kind text); insert into things values ('a'), ('b')"); err != nil {
		t.Fatal(err)
	}
	holder := connect(t, ctx, url)
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
	runErr := DefaultLocking.RetryLockTimeouts(ctx, migration.Name, func(ctx context.Context) error {
		attempts++
		err := ExecNonTransactional(ctx, conn, migration, nil)
		if attempts == 1 {
			got := indexState(t, ctx, conn, "things_kind")
			// The oid is the leftover's, read here and compared after the rebuild.
			if want := (relation{OID: got.OID, Index: true, Table: "things"}); got != want {
				t.Fatalf("after attempt 1: things_kind = %+v, want %+v", got, want)
			}
			t.Logf("after attempt 1: things_kind = %+v; error %v", got, err)
			leftover = got.OID
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
	got := indexState(t, ctx, conn, "things_kind")
	if want := (relation{OID: got.OID, Index: true, Valid: true, Table: "things"}); got != want {
		t.Fatalf("things_kind = %+v, want %+v", got, want)
	}
	if got.OID == leftover {
		t.Fatalf("things_kind oid %d is the leftover's: it was revalidated, not rebuilt", got.OID)
	}
}

// A build killed outside the runner leaves the same INVALID index of the migration's own name;
// one ExecNonTransactional drops it and builds again.
func TestExecNonTransactionalDropsTheLeftoverOfAKilledBuild(t *testing.T) {
	ctx := context.Background()
	conn := connect(t, ctx, testpg.URL(t))
	if _, err := conn.Exec(ctx, "create table things (kind text); insert into things values ('same'), ('same')"); err != nil {
		t.Fatal(err)
	}
	leftover := leaveInvalidIndex(t, ctx, conn, "things_kind", "things", "kind")
	if err := ExecNonTransactional(ctx, conn, indexMigration(), nil); err != nil {
		t.Fatalf("ExecNonTransactional = %v, want nil after dropping its own leftover", err)
	}
	got := indexState(t, ctx, conn, "things_kind")
	if want := (relation{OID: got.OID, Index: true, Valid: true, Table: "things"}); got != want {
		t.Fatalf("things_kind = %+v, want %+v", got, want)
	}
	if got.OID == leftover.OID {
		t.Fatalf("things_kind oid %d is the leftover's: it was revalidated, not rebuilt", got.OID)
	}
}

// The runner drops only an index that is its own: its name, on its own table. Anything else of
// that name is a relation it did not build, and it refuses, naming the relation and the way out
// that relation leaves: another table's index can be dropped concurrently, a table cannot.
func TestExecNonTransactionalRefusesAnIndexItDidNotBuild(t *testing.T) {
	// Every refusal names the relation; the remedy is each case's own, so the subtest brings it.
	refusal := func(t *testing.T, conn *pgx.Conn, remedy ...string) {
		t.Helper()
		err := ExecNonTransactional(context.Background(), conn, indexMigration(), nil)
		if err == nil || !strings.Contains(err.Error(), "things_kind") {
			t.Fatalf("ExecNonTransactional = %v, want a refusal naming things_kind", err)
		}
		for _, want := range remedy {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("refusal %q does not carry %q", err, want)
			}
		}
	}
	t.Run("an INVALID index of the migration's name on another table", func(t *testing.T) {
		ctx := context.Background()
		conn := connect(t, ctx, testpg.URL(t))
		if _, err := conn.Exec(ctx, `create table things (kind text); insert into things values ('a');
			create table others (kind text); insert into others values ('same'), ('same')`); err != nil {
			t.Fatal(err)
		}
		before := leaveInvalidIndex(t, ctx, conn, "things_kind", "others", "kind")
		refusal(t, conn, "is an INVALID index on another table", `DROP INDEX CONCURRENTLY "public"."things_kind"`)
		if got := indexState(t, ctx, conn, "things_kind"); got != before {
			t.Fatalf("things_kind = %+v, want the INVALID index on others untouched (%+v)", got, before)
		}
	})
	t.Run("a valid index of the migration's name on another table", func(t *testing.T) {
		ctx := context.Background()
		conn := connect(t, ctx, testpg.URL(t))
		if _, err := conn.Exec(ctx, `create table things (kind text);
			create table others (kind text); create index things_kind on others (kind)`); err != nil {
			t.Fatal(err)
		}
		before := indexState(t, ctx, conn, "things_kind")
		if want := (relation{OID: before.OID, Index: true, Valid: true, Table: "others"}); before.OID == 0 || before != want {
			t.Fatalf("fixture things_kind = %+v, want a valid index on others", before)
		}
		refusal(t, conn, "is a VALID index on another table", `DROP INDEX CONCURRENTLY "public"."things_kind"`)
		if got := indexState(t, ctx, conn, "things_kind"); got != before {
			t.Fatalf("things_kind = %+v, want the valid index on others untouched (%+v)", got, before)
		}
	})
	t.Run("a non-index relation of the migration's name", func(t *testing.T) {
		ctx := context.Background()
		conn := connect(t, ctx, testpg.URL(t))
		if _, err := conn.Exec(ctx, "create table things (kind text); create table things_kind (x int)"); err != nil {
			t.Fatal(err)
		}
		before := indexState(t, ctx, conn, "things_kind")
		if want := (relation{OID: before.OID}); before.OID == 0 || before != want {
			t.Fatalf("fixture things_kind = %+v, want a plain table of its own oid", before)
		}
		refusal(t, conn, `relkind "r"`, "Rename or remove")
		if got := indexState(t, ctx, conn, "things_kind"); got != before {
			t.Fatalf("things_kind = %+v, want the table things_kind still there (%+v)", got, before)
		}
	})
}

// An INVALID index of another name is another migration's or another operator's business: the
// runner neither drops it nor refuses over it, and its own build goes through.
func TestExecNonTransactionalLeavesAnIndexOfAnotherNameAlone(t *testing.T) {
	ctx := context.Background()
	conn := connect(t, ctx, testpg.URL(t))
	if _, err := conn.Exec(ctx, "create table things (kind text, other text); insert into things values ('a', 'same'), ('b', 'same')"); err != nil {
		t.Fatal(err)
	}
	other := leaveInvalidIndex(t, ctx, conn, "things_other", "things", "other")
	if err := ExecNonTransactional(ctx, conn, indexMigration(), nil); err != nil {
		t.Fatalf("ExecNonTransactional = %v, want nil: the INVALID index is not the migration's", err)
	}
	got := indexState(t, ctx, conn, "things_kind")
	if want := (relation{OID: got.OID, Index: true, Valid: true, Table: "things"}); got != want {
		t.Fatalf("things_kind = %+v, want %+v", got, want)
	}
	if left := indexState(t, ctx, conn, "things_other"); left != other {
		t.Fatalf("things_other = %+v, want the INVALID index untouched (%+v)", left, other)
	}
}

// The DROP of the runner's own leftover takes locks too. One that outlasts lock_timeout must come
// back as a *LockTimeoutError, which is what RetryLockTimeouts finds with errors.As and retries,
// and it must be the drop's own: the error carries the wrapping only the drop adds.
func TestExecNonTransactionalReportsADropTimeoutAsALockTimeoutError(t *testing.T) {
	ctx := context.Background()
	url := testpg.URL(t)
	conn := connect(t, ctx, url)
	if _, err := conn.Exec(ctx, "create table things (kind text); insert into things values ('same'), ('same')"); err != nil {
		t.Fatal(err)
	}
	leaveInvalidIndex(t, ctx, conn, "things_kind", "things", "kind")
	holder := connect(t, ctx, url)
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
	if !strings.HasPrefix(err.Error(), "drop the INVALID index") {
		t.Fatalf("error %q is not the one the DROP of the leftover wraps; it came from elsewhere", err)
	}
}
