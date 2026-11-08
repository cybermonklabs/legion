package pgmigrate

import (
	"context"
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

func TestExecNonTransactionalRefusesAnInvalidIndex(t *testing.T) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, testpg.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	if _, err := conn.Exec(ctx, "create table things (kind text); insert into things values ('same'), ('same')"); err != nil {
		t.Fatal(err)
	}
	// A failed UNIQUE build supplies a real INVALID index fixture, but UNIQUE builds themselves
	// are deliberately outside the marked migration mode.
	if _, err := conn.Exec(ctx, "create unique index concurrently things_kind on things (kind)"); err == nil {
		t.Fatal("unique index accepted duplicate rows")
	}
	migration := Migration{Name: "0001_index.up.sql", SQL: "create index concurrently if not exists things_kind on things (kind)", NonTransactional: true}
	var valid bool
	if err := conn.QueryRow(ctx, "select indisvalid from pg_index where indexrelid = 'things_kind'::regclass").Scan(&valid); err != nil || valid {
		t.Fatalf("failed concurrent build did not leave an invalid index: %v, %v", valid, err)
	}
	if err := ExecNonTransactional(ctx, conn, migration, nil); err == nil || !strings.Contains(err.Error(), "INVALID") || !strings.Contains(err.Error(), "DROP INDEX CONCURRENTLY") {
		t.Fatalf("retry = %v, want actionable INVALID-index refusal", err)
	}
}
