package pgmigrate

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/testpg"
)

// catalogOnlyFixtureSQL is the database the census tests of this file judge their rows against,
// <rows> rows in each of its three large tables. big is the table the forms change, big2 the
// table they reference, and big3, which references big2, the table DROP TABLE drops. Autovacuum is
// off on all three, so no vacuum holds a lock or reads a block while a row is measured. The types
// and the functions are the ones the rows resolve: an enum, a composite type made by CREATE TYPE …
// AS, a range, a domain with a constraint and one without, a function of another schema, a
// volatile cast to vol_target, and an enum named serial4, a name PostgreSQL does not look up for a
// column. vol_target_of is plpgsql because Postgres inlines a volatile sql function in its place
// and then does not rewrite the table, so the cast would show nothing.
const catalogOnlyFixtureSQL = `
create table schema_migrations (version bigint primary key);
insert into schema_migrations (version) values (1);
create type mood as enum ('calm', 'busy');
create type pair as (a int, b text);
create type floatrange as range (subtype = float8);
create domain positive as int check (value > 0);
create domain plain_int as int;
create type vol_target as (a int);
create function vol_target_of(int) returns vol_target volatile language plpgsql
  as $$ begin return row($1)::vol_target; end $$;
create cast (int as vol_target) with function vol_target_of(int) as assignment;
create type serial4 as enum ('a');
create schema other;
create function other.one() returns int immutable language sql as 'select 1';
create function spare_fn() returns int language sql as 'select 1';
create table big2 (id int primary key, pad text not null) with (autovacuum_enabled = off);
create table big (
  id int primary key, kind text not null, pad text not null, ref int, token uuid,
  flag boolean default false, spare int, doubled int generated always as (id * 2) stored,
  constraint big_kind_check check (kind <> '')
) with (autovacuum_enabled = off);
create table big3 (id int primary key, big2_id int references big2 (id), pad text not null)
  with (autovacuum_enabled = off);
insert into big2 select g, repeat('x', 200) from generate_series(1, <rows>) g;
insert into big (id, kind, pad, ref) select g, 'k', repeat('y', 200), g from generate_series(1, <rows>) g;
insert into big3 select g, g, repeat('z', 200) from generate_series(1, <rows>) g;
create index big_pad on big (pad);
create function big_noop() returns trigger language plpgsql as $$ begin return null; end $$;
create trigger big_noop after insert on big for each row execute function big_noop();
create view big_view as select id, kind from big;
`

// catalogOnlyFixture is catalogOnlyFixtureSQL with rows rows in each large table. The census
// tests load it with one row, since a table with a row and a primary key is above a one-byte
// limit, and the premise test with 40,000, which fill more than 1,000 heap pages a table.
func catalogOnlyFixture(rows int) string {
	return strings.ReplaceAll(catalogOnlyFixtureSQL, "<rows>", strconv.Itoa(rows))
}

// catalogOnlyDatabase connects to a database of the test's own, loaded with
// catalogOnlyFixture(rows), on the server DISPATCH_TEST_DATABASE_URL names, and skips the test
// when that variable is unset, as the store's tests do. It does not call testpg.URL, which starts
// a container when the variable is unset. The database is dropped when the test ends.
func catalogOnlyDatabase(t *testing.T, ctx context.Context, rows int) *pgx.Conn {
	t.Helper()
	raw := os.Getenv("DISPATCH_TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("DISPATCH_TEST_DATABASE_URL must be set to run the census's catalog tests")
	}
	adminURL, err := url.Parse(raw)
	if err != nil {
		// url.Parse's own error quotes the URL, which carries a credential.
		t.Fatal("DISPATCH_TEST_DATABASE_URL is not a URL")
	}
	adminURL.Path = "/postgres"
	conn := connect(t, ctx, testpg.DatabaseOn(t, adminURL.String(), "pgmigrate_catalog"))
	if _, err := conn.Exec(ctx, catalogOnlyFixture(rows)); err != nil {
		t.Fatalf("load the fixture: %v", err)
	}
	return conn
}

// addColumn is the admitted row "bigint column". A row whose form touches no table the census
// sizes (a view, a LIKE source, a function, a materialized view) pairs the form with it in one
// migration, so that the census sizes big for that migration, and the second migration of a row
// of two is this statement.
const addColumn = "alter table big add column c bigint;"

// paired puts addColumn after the statements of sql, in the same migration.
func paired(sql string) string {
	return sql + " " + addColumn
}

// aboveTheLimit is what the census's refusal of a table above the one-byte limit says.
const aboveTheLimit = "above the 1 bytes a table a migration touches may be"

// admittedRow is one census of TestCensusAdmitsEveryCatalogOnlyForm: the SQL of each pending
// migration, in order, and the kind words the last one must be exempt as.
type admittedRow struct {
	name       string
	migrations []string
	kind       string
}

// admittedRows are every row of the Verification document's list "The rows of c-admit", and the
// plan's own rows for its interpretations and departures. Each statement is on big unless it
// names another table. TestEveryAdmittedFormReadsAndRewritesNoRow runs each row of one migration.
var admittedRows = []admittedRow{
	{"text column", []string{"alter table big add column c text"}, "alter table"},
	{"text[] column", []string{"alter table big add column c text[]"}, "alter table"},
	{"bigint column", []string{addColumn}, "alter table"},
	{"integer column", []string{"alter table big add column c integer"}, "alter table"},
	{"timestamptz column", []string{"alter table big add column c timestamptz"}, "alter table"},
	{"jsonb column", []string{"alter table big add column c jsonb"}, "alter table"},
	{"uuid column", []string{"alter table big add column c uuid"}, "alter table"},
	{"boolean column", []string{"alter table big add column c boolean"}, "alter table"},
	{"enum column", []string{"alter table big add column c mood"}, "alter table"},
	// pair is a composite type made by CREATE TYPE … AS before the census runs, which departure
	// D3 keeps admitted: only the row type of a relation is judged on size.
	{"composite column", []string{"alter table big add column c pair"}, "alter table"},
	{"range column", []string{"alter table big add column c floatrange"}, "alter table"},
	{"references a second table above the limit", []string{"alter table big add column c int references big2 (id)"}, "alter table"},
	{"bigint not null default 0", []string{"alter table big add column c bigint not null default 0"}, "alter table"},
	{"boolean not null default false", []string{"alter table big add column c boolean not null default false"}, "alter table"},
	{"text[] not null default '{}'", []string{"alter table big add column c text[] not null default '{}'"}, "alter table"},
	{"text[] not null default array", []string{"alter table big add column c text[] not null default array['person','approve']"}, "alter table"},
	{"bigint default 0", []string{"alter table big add column c bigint default 0"}, "alter table"},
	{"boolean default false", []string{"alter table big add column c boolean default false"}, "alter table"},
	{"text[] default '{}'", []string{"alter table big add column c text[] default '{}'"}, "alter table"},
	{"text[] default array", []string{"alter table big add column c text[] default array['person','approve']"}, "alter table"},
	{"text default a cast constant", []string{"alter table big add column c text default 'x'::text"}, "alter table"},
	{"timestamptz not null default now()", []string{"alter table big add column c timestamptz not null default now()"}, "alter table"},
	{"timestamptz default current_timestamp", []string{"alter table big add column c timestamptz default current_timestamp"}, "alter table"},
	{"drop column", []string{"alter table big drop column spare"}, "alter table"},
	// A column's default applies only to the rows inserted later, so even a volatile one set on an
	// existing column reads and rewrites no row.
	{"set default gen_random_uuid()", []string{"alter table big alter column token set default gen_random_uuid()"}, "alter table"},
	{"drop default", []string{"alter table big alter column flag drop default"}, "alter table"},
	{"drop not null", []string{"alter table big alter column kind drop not null"}, "alter table"},
	{"drop expression", []string{"alter table big alter column doubled drop expression"}, "alter table"},
	{"foreign key not valid", []string{"alter table big add constraint big_ref_fkey foreign key (ref) references big2 (id) not valid"}, "alter table"},
	// The two lock modes the shipped migrations take.
	{"lock in access exclusive mode", []string{"lock table big in access exclusive mode"}, "lock table"},
	{"lock in exclusive mode", []string{"lock table big in exclusive mode"}, "lock table"},
	{"rename column", []string{"alter table big rename column spare to spare_renamed"}, "rename column"},
	{"create table and an index on it", []string{"create table fresh (id int primary key, big_id int references big (id)); create index fresh_big_id on fresh (big_id);"}, "create table and create index"},
	{"create table like", []string{paired("create table fresh_like (like big including all);")}, "create table and alter table"},
	{"create view", []string{paired("create view fresh_view as select id, kind from big;")}, "create view and alter table"},
	{"create or replace view", []string{paired("create or replace view big_view as select id, kind, pad from big;")}, "create view and alter table"},
	// big3 references big2: the census sizes big3 and reaches big2 through big3's key.
	{"drop table", []string{"drop table big3;"}, "drop table"},
	{"several subcommands", []string{"alter table big add column a bigint not null default 0, add column r int references big2 (id), drop column spare, alter column flag drop default, alter column kind drop not null;"}, "alter table"},

	// The forms the census admitted before it read the catalog.
	{"drop constraint", []string{"alter table big drop constraint big_kind_check"}, "alter table"},
	{"check not valid", []string{"alter table big add constraint big_pad_check check (pad <> '') not valid"}, "alter table"},
	{"plpgsql function and trigger", []string{"create function big_touched() returns trigger language plpgsql as $$ begin return null; end $$; create trigger t1 after update on big for each row execute function big_touched();"}, "create function and create trigger"},
	// Creating an sql function analyzes its query and runs none of it, so a body that counts big
	// reads no block of big.
	{"sql function", []string{paired("create function big_count() returns bigint language sql stable as 'select count(*) from big';")}, "create function and alter table"},
	{"drop trigger", []string{"drop trigger big_noop on big"}, "drop trigger"},
	{"drop function", []string{paired("drop function spare_fn();")}, "drop function and alter table"},
	{"drop index", []string{"drop index big_pad"}, "drop index"},
	{"set local", []string{paired("set local lock_timeout = '1s';")}, "set and alter table"},

	// The plan's own rows. COLLATE, STORAGE and COMPRESSION are no constraints (I4).
	{"storage, compression and collate", []string{`alter table big add column c text storage external compression pglz collate "C"`}, "alter table"},
	{"varchar default a cast constant", []string{"alter table big add column c varchar(5) default 'x'::varchar(5)"}, "alter table"},
	{"bigint[] default array", []string{"alter table big add column c bigint[] default array[1,2]"}, "alter table"},
	{"bigint default a negative constant", []string{"alter table big add column c bigint default -1"}, "alter table"},
	{"text not null default an empty string", []string{"alter table big add column c text not null default ''"}, "alter table"},
	// Without a default no cast runs, so the fixture's volatile cast to vol_target does not count.
	{"vol_target column without a default", []string{"alter table big add column c vol_target"}, "alter table"},
	{"deferrable references", []string{"alter table big add column c int references big2 (id) deferrable initially deferred"}, "alter table"},
	{"qualified type", []string{"alter table big add column c pg_catalog.int8"}, "alter table"},
	{"quoted type", []string{`alter table big add column c "timestamptz"`}, "alter table"},
	// A DO block is code the census does not read, so it leaves the catalog reads on (I6).
	{"after a DO block", []string{"do $$ begin end $$", addColumn}, "alter table"},
	// A table's own row type is no type change (I1).
	{"after a create table", []string{"create table later_tbl (id int)", addColumn}, "alter table"},
	// SET LOCAL ends with its migration's transaction (D1).
	{"after a set local search_path", []string{"set local search_path = public", addColumn}, "alter table"},
	// spare_fn is no cast's function, so Catalog read 5 leaves the reads on, for a replace and for
	// an ALTER FUNCTION alike.
	{"after a replaced function no cast calls", []string{"create or replace function spare_fn() returns int language sql as 'select 2'", addColumn}, "alter table"},
	{"after an altered function no cast calls", []string{"alter function spare_fn() volatile", addColumn}, "alter table"},
}

// refusedRow is one census of TestCensusStillRefusesEveryFormThatReadsOrRewritesRows: the SQL of
// each pending migration, in order, and which of them must be refused on size.
type refusedRow struct {
	name       string
	migrations []string
	// first says the first migration is the one refused, not the last.
	first bool
}

// refusedRows are every row of the Verification document's list "The rows of c-refuse", and the
// plan's own rows for its departures and for R4's list. Each statement is on big unless it names
// another table. TestEveryAdmittedFormReadsAndRewritesNoRow runs each row of one migration.
var refusedRows = []refusedRow{
	{name: "default gen_random_uuid()", migrations: []string{"alter table big add column c uuid default gen_random_uuid()"}},
	{name: "default clock_timestamp()", migrations: []string{"alter table big add column c timestamptz default clock_timestamp()"}},
	// The census refuses the six serial names by name, before Catalog read 1. serial and bigserial
	// resolve to no type, so the read would refuse them too.
	{name: "serial column", migrations: []string{"alter table big add column c serial"}},
	{name: "bigserial column", migrations: []string{"alter table big add column c bigserial"}},
	// The fixture's enum named serial4 resolves in the catalog, and PostgreSQL still makes the
	// column a serial, with a sequence and a nextval default that rewrites the table.
	{name: "serial4 column beside a type named serial4", migrations: []string{"alter table big add column c serial4"}},
	// other.one and abs are immutable: the schema refuses the one, the argument the other.
	{name: "default calls a function of another schema", migrations: []string{"alter table big add column c int default other.one()"}},
	{name: "default calls a pg_catalog function with an argument", migrations: []string{"alter table big add column c int default abs(-1)"}},
	{name: "domain with a constraint", migrations: []string{"alter table big add column c positive"}},
	{name: "domain without a constraint", migrations: []string{"alter table big add column c plain_int"}},
	// The census reads the catalog as it stands before any pending migration applies, so a type
	// or a function an earlier pending migration creates does not resolve. A table's row type is
	// no type change, so the first of these rows leaves the reads on and the type is not found.
	{name: "type of a table a pending migration creates", migrations: []string{"create table later_tbl (id int)", "alter table big add column c later_tbl"}},
	{name: "type a pending migration creates", migrations: []string{"create type later_mood as enum ('a')", "alter table big add column c later_mood"}},
	{name: "default function a pending migration creates", migrations: []string{"create function later_one() returns int immutable language sql as 'select 1'", "alter table big add column c int default later_one()"}},
	{name: "after set search_path", migrations: []string{"set search_path = public; " + addColumn}},
	{name: "after set role", migrations: []string{"set role postgres; " + addColumn}},
	{name: "after set session authorization", migrations: []string{"set session authorization postgres; " + addColumn}},
	{name: "after reset role", migrations: []string{"reset role; " + addColumn}},
	{name: "after reset all", migrations: []string{"reset all; " + addColumn}},
	// Postgres finds a setting by its name without regard to case, and the parser keeps the case
	// of a quoted name.
	{name: "after set of a quoted Search_Path", migrations: []string{`set "Search_Path" = public; ` + addColumn}},
	{name: "after reset of a quoted ROLE", migrations: []string{`reset "ROLE"; ` + addColumn}},
	{name: "enum replaced by a domain", migrations: []string{"drop type mood; create domain mood as int check (value > 0)", "alter table big add column c mood"}},
	{name: "check column", migrations: []string{"alter table big add column c int check (c > 0)"}},
	{name: "unique column", migrations: []string{"alter table big add column c int unique"}},
	{name: "primary key column", migrations: []string{"alter table big add column c int primary key"}},
	// With any DEFAULT clause, DEFAULT NULL included, Postgres checks the key against every row.
	{name: "default 1 references", migrations: []string{"alter table big add column c int default 1 references big2 (id)"}},
	{name: "default null references", migrations: []string{"alter table big add column c int default null references big2 (id)"}},
	{name: "default null::int references", migrations: []string{"alter table big add column c int default null::int references big2 (id)"}},
	{name: "identity column", migrations: []string{"alter table big add column c int generated always as identity"}},
	{name: "stored generated column", migrations: []string{"alter table big add column c int generated always as (id * 2) stored"}},
	{name: "not null without a default", migrations: []string{"alter table big add column c int not null"}},
	{name: "not null default null", migrations: []string{"alter table big add column c int not null default null"}},
	{name: "not null default null::int", migrations: []string{"alter table big add column c int not null default null::int"}},
	// ref is nullable and holds no null, so SET NOT NULL scans it. On pad, which is already NOT
	// NULL, PostgreSQL 18 scans nothing, so pad would prove nothing.
	{name: "set not null", migrations: []string{"alter table big alter column ref set not null"}},
	{name: "alter column type", migrations: []string{"alter table big alter column spare type bigint"}},
	{name: "validated check", migrations: []string{"alter table big add constraint big_pad_check check (pad <> '')"}},
	{name: "validate constraint", migrations: []string{"alter table big validate constraint big_kind_check"}},
	{name: "drop column cascade", migrations: []string{"alter table big drop column spare cascade"}},
	{name: "drop table cascade", migrations: []string{"drop table big3 cascade"}},
	{name: "rename table", migrations: []string{"alter table big rename to big_renamed"}},
	{name: "rename index", migrations: []string{"alter index big_pad rename to big_pad2"}},
	{name: "rename primary key", migrations: []string{"alter table big rename constraint big_pkey to big_pkey2"}},
	{name: "rename a view's column", migrations: []string{paired("alter view big_view rename column kind to kind2;")}},
	// The census finds no big_renamed, so it sizes no table for the index: the rename is what
	// must refuse.
	{name: "index on a renamed table", migrations: []string{"alter table big rename to big_renamed", "create index on big_renamed (pad)"}, first: true},
	// CREATE TABLE IF NOT EXISTS records no table, since where t_filled exists the index is built
	// on its rows.
	{name: "index after create table if not exists", migrations: []string{
		"create table t_filled (id int); insert into t_filled select id from big;",
		"alter table big add column c bigint; create table if not exists t_filled (id int); create index on t_filled (id);",
	}},
	{name: "index on an existing table", migrations: []string{"create index big_kind on big (kind)"}},
	{name: "create table as", migrations: []string{paired("create table t_as as select id from big;")}},
	{name: "select into", migrations: []string{paired("select id into t_into from big;")}},
	{name: "partition of", migrations: []string{paired("create table big_part partition of big for values in (1);")}},
	{name: "materialized view", migrations: []string{paired("create materialized view mv as select id from big;")}},
	{name: "update beside an admitted column", migrations: []string{"alter table big add column c bigint; update big set spare = 1;"}},

	// The plan's own rows. Under a search_path the migration sets, even text can resolve to a type
	// of another schema, so without the catalog a text column is judged on size (D4).
	{name: "text column after its own search_path", migrations: []string{"set search_path = other, pg_catalog, public; alter table big add column c text;"}},
	{name: "inherits", migrations: []string{paired("create table t_inh () inherits (big);")}},
	// Postgres casts 1 to vol_target through the fixture's volatile cast function, and rewrites
	// the table to give each row a value of its own.
	{name: "default through a volatile cast", migrations: []string{"alter table big add column c vol_target default 1"}},
	// A pending DROP TABLE big2 could uncover a domain of that name later in the search_path (D3).
	{name: "row type of a table", migrations: []string{"alter table big add column c big2"}},
	// LOCK TABLE needs no catalog read and sizes big, so the refusal comes from ALTER TYPE alone,
	// which the census admitted as alter table before it read objtype (D5).
	{name: "alter type add attribute", migrations: []string{"alter type pair add attribute c text; lock table big in access exclusive mode;"}},
	// The pooled connection keeps a session setting for the next migration it runs (D1).
	{name: "after a session set search_path", migrations: []string{"set search_path = public", addColumn}},
	{name: "after a session set of a quoted Search_Path", migrations: []string{`set "Search_Path" = public`, addColumn}},
	// The census cannot see what a migration it cannot parse changes (D2).
	{name: "after a migration that does not parse", migrations: []string{"create tabel broken (id int)", addColumn}},

	// R4's list: each first migration changes what a name resolves to, so the second reads no
	// catalog and its added column is judged on size. The census only parses these statements and
	// never runs them, so the role nobody_1792 need not exist. vol_target_of is the fixture's cast
	// function, so an ALTER FUNCTION or a replace of it alters the cast.
	{name: "after create cast", migrations: []string{"create cast (text as mood) with inout", addColumn}},
	{name: "after drop cast", migrations: []string{"drop cast (int as vol_target)", addColumn}},
	{name: "after create schema", migrations: []string{"create schema s2", addColumn}},
	{name: "after drop schema", migrations: []string{"drop schema other", addColumn}},
	{name: "after rename type", migrations: []string{"alter type mood rename to mood2", addColumn}},
	{name: "after add enum value", migrations: []string{"alter type mood add value 'idle'", addColumn}},
	{name: "after set schema of a table", migrations: []string{"alter table big2 set schema other", addColumn}},
	{name: "after create extension", migrations: []string{"create extension if not exists pgcrypto", addColumn}},
	{name: "after create domain", migrations: []string{"create domain d2 as int", addColumn}},
	{name: "after reset search_path", migrations: []string{"reset search_path", addColumn}},
	{name: "after set schema", migrations: []string{"set schema 'public'", addColumn}},
	{name: "after type owner", migrations: []string{"alter type mood owner to current_user", addColumn}},
	{name: "after domain owner", migrations: []string{"alter domain positive owner to current_user", addColumn}},
	{name: "after schema owner", migrations: []string{"alter schema other owner to current_user", addColumn}},
	{name: "after drop owned", migrations: []string{"drop owned by nobody_1792", addColumn}},
	{name: "after reassign owned", migrations: []string{"reassign owned by nobody_1792 to current_user", addColumn}},
	{name: "after grant on schema", migrations: []string{"grant usage on schema other to public", addColumn}},
	{name: "after revoke on schema", migrations: []string{"revoke usage on schema other from public", addColumn}},
	{name: "after grant on type", migrations: []string{"grant usage on type mood to public", addColumn}},
	{name: "after alter of a cast's function", migrations: []string{"alter function vol_target_of(int) stable", addColumn}},
	{name: "after drop cascade", migrations: []string{"drop function spare_fn() cascade", addColumn}},
	{name: "after replace of a cast's function", migrations: []string{"create or replace function vol_target_of(int) returns vol_target stable language plpgsql as $$ begin return row($1)::vol_target; end $$", addColumn}},
	// A role holds the USAGE on a schema that each role it inherits from holds, so a change of a
	// role's memberships can change which schemas its lookup finds names in, and a rename of a role
	// changes which schema "$user" names. The roles other_owner_1792 and r_1792 need not exist
	// either.
	{name: "after grant of a role", migrations: []string{"grant other_owner_1792 to current_user", addColumn}},
	{name: "after revoke of a role", migrations: []string{"revoke other_owner_1792 from current_user", addColumn}},
	{name: "after create role in role", migrations: []string{"create role r_1792 in role other_owner_1792", addColumn}},
	{name: "after alter role noinherit", migrations: []string{"alter role nobody_1792 noinherit", addColumn}},
	{name: "after drop role", migrations: []string{"drop role nobody_1792", addColumn}},
	{name: "after rename role", migrations: []string{"alter role nobody_1792 rename to nobody_1792b", addColumn}},
	// A migration's own replace of a cast's function turns its own catalog reads off.
	{name: "beside replace of a cast's function", migrations: []string{"create or replace function vol_target_of(int) returns vol_target stable language plpgsql as $$ begin return row($1)::vol_target; end $$; " + addColumn}},
}

// censusOf runs one census, with the table limit at one byte, of sqls as pending migrations
// numbered from 2, after the version 1 the fixture records, and answers each one's census.
func censusOf(t *testing.T, ctx context.Context, conn *pgx.Conn, sqls []string) []MigrationCensus {
	t.Helper()
	migrations := make([]Migration, len(sqls))
	for i, sql := range sqls {
		version := i + 2
		migrations[i] = Migration{Version: version, Name: fmt.Sprintf("%04d_row.up.sql", version), SQL: sql}
	}
	report, err := Census(ctx, conn, migrations, "schema_migrations", CensusOptions{TableLimit: 1})
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	if len(report.Pending) != len(migrations) {
		t.Fatalf("the census read %d pending migrations, want %d: %+v", len(report.Pending), len(migrations), report.Pending)
	}
	return report.Pending
}

// Each form this repository's migrations write that PostgreSQL runs in the catalog alone is
// exempt from the table limit, whatever the column type: an added column of any type that
// resolves and is not a domain or a relation's row type, with a constant, a cast constant, a SQL
// value function or a call of a stable pg_catalog function with no argument as its default, and
// the other subcommands, renames, creates, drops and locks that read no existing row. The census
// runs with the limit at one byte, so every table of the fixture is above it, and each row's
// migration touches such a table: a form that touches no table the census sizes shares its
// migration with an added bigint column of big, which is admitted alone. Each row asserts that the
// census still sized a table above the limit for the migration, so a pass means the exemption kept
// the refusal away, not a table the census did not find. A row of two migrations shows that what
// the first one does (a DO block, a CREATE TABLE, a SET LOCAL, a function no cast calls) leaves
// the catalog reads of the second one on. The census is read-only and its catalog state lives
// inside one call, so the rows share one database and no row reaches the next.
func TestCensusAdmitsEveryCatalogOnlyForm(t *testing.T) {
	ctx := context.Background()
	conn := catalogOnlyDatabase(t, ctx, 1)
	for _, row := range admittedRows {
		t.Run(row.name, func(t *testing.T) {
			pending := censusOf(t, ctx, conn, row.migrations)
			entry := pending[len(pending)-1]
			if entry.TableSizeExemption != row.kind || len(entry.Refusals) != 0 {
				t.Fatalf("%s: exemption %q, refusals %q; want exempt as %q and no refusal", entry.Migration.Name, entry.TableSizeExemption, entry.Refusals, row.kind)
			}
			if !sizesATableAboveTheLimit(entry) {
				t.Fatalf("%s sized no table above the limit, so the exemption proved nothing: %+v", entry.Migration.Name, entry.Tables)
			}
		})
	}
}

// sizesATableAboveTheLimit says whether the census sized, for entry, a table the migration names
// that exists and is above the one-byte limit.
func sizesATableAboveTheLimit(entry MigrationCensus) bool {
	for _, table := range entry.Tables {
		if table.Exists && table.Through == "" && table.Bytes > 1 {
			return true
		}
	}
	return false
}

// Every form that reads or rewrites rows is still held to the table limit: a volatile default, a
// default through a volatile cast, a column the catalog does not resolve or resolves to a domain
// or a relation's row type, a constraint the column's rows are checked against, and every other
// subcommand, rename and statement kind the census does not admit. So is an added column whose
// reading the census cannot trust: after the migration's own SET of search_path, role or
// session_authorization, and after an earlier pending migration that changes a type, a domain, a
// cast, a schema, an extension or a role, sets one of those settings for the session, or does not
// parse, because the census reads the catalog as it stands before any pending migration applies.
// The census runs with the limit at one byte, so each refused migration is refused on the size of
// a table it touches; a form that touches no table the census sizes shares its migration with an
// added bigint column of big, which is admitted alone, so the refusal shows that the form cost the
// migration its exemption.
func TestCensusStillRefusesEveryFormThatReadsOrRewritesRows(t *testing.T) {
	ctx := context.Background()
	conn := catalogOnlyDatabase(t, ctx, 1)
	for _, row := range refusedRows {
		t.Run(row.name, func(t *testing.T) {
			pending := censusOf(t, ctx, conn, row.migrations)
			entry := pending[len(pending)-1]
			if row.first {
				entry = pending[0]
			}
			if entry.TableSizeExemption != "" || !refusalSays(entry, aboveTheLimit) {
				t.Fatalf("%s: exemption %q, refusals %q; want no exemption and a refusal on size", entry.Migration.Name, entry.TableSizeExemption, entry.Refusals)
			}
		})
	}
}

// refusalSays says whether a refusal of entry holds every one of texts.
func refusalSays(entry MigrationCensus, texts ...string) bool {
	for _, refusal := range entry.Refusals {
		holds := true
		for _, text := range texts {
			holds = holds && strings.Contains(refusal, text)
		}
		if holds {
			return true
		}
	}
	return false
}

// An enabled event trigger runs code of its own when a command of its tag runs, and that code can
// read any table, so one on the tag of any kind the census admits takes the exemption away from
// every migration, and the refusal on size names it. Each subtest enables an event trigger on one
// tag alone, runs the census on a migration that holds a statement of that kind and an added
// column of big, which makes the census size a table above the limit (a CREATE VIEW alone sizes
// none), and drops the trigger again. The census reads the tags CREATE TABLE, CREATE VIEW and
// DROP TABLE because it admits those kinds: a census that did not read them would keep the
// exemption, and those three subtests would fail. CREATE EVENT TRIGGER needs a superuser, so the
// test fails under any other login rather than skip, since a skipped run reports ok.
func TestCensusEventTriggerOnAnAdmittedKindCancelsItsExemption(t *testing.T) {
	ctx := context.Background()
	conn := catalogOnlyDatabase(t, ctx, 1)
	if _, err := conn.Exec(ctx, "create function noop_event() returns event_trigger language plpgsql as $$ begin end $$"); err != nil {
		t.Fatal(err)
	}
	for i, row := range []struct{ tag, sql string }{
		{"ALTER TABLE", addColumn},
		{"CREATE TABLE", "create table et_new (id int); " + addColumn},
		{"CREATE INDEX", "create table et_idx (id int); create index on et_idx (id); " + addColumn},
		{"CREATE VIEW", "create view et_view as select id from big; " + addColumn},
		{"DROP TABLE", "drop table big3; " + addColumn},
	} {
		trigger := fmt.Sprintf("et_%d", i+1)
		t.Run(row.tag, func(t *testing.T) {
			if _, err := conn.Exec(ctx, "create event trigger "+trigger+" on ddl_command_end when tag in ('"+row.tag+"') execute function noop_event()"); err != nil {
				t.Fatalf("create event trigger %s, which needs a superuser: %v", trigger, err)
			}
			t.Cleanup(func() {
				if _, err := conn.Exec(context.Background(), "drop event trigger "+trigger); err != nil {
					t.Errorf("drop event trigger %s: %v", trigger, err)
				}
			})
			entry := censusOf(t, ctx, conn, []string{row.sql})[0]
			if entry.TableSizeExemption != "" || !refusalSays(entry, aboveTheLimit, "the enabled event triggers "+trigger+" run code of their own on them") {
				t.Fatalf("exemption %q, refusals %q; want a refusal on size that names %s", entry.TableSizeExemption, entry.Refusals, trigger)
			}
		})
	}
}

// premiseTables are the large tables the premise test reads for every row: the census sizes only
// the tables a migration names, but a statement that read a view's base table or a LIKE source
// would hold the migration's locks for the read as well.
var premiseTables = []string{"big", "big2", "big3"}

// tableReading is what the premise test reads of one large table by the OID it took before the
// statement: its relfilenode, nil where the table has no pg_class row any more, and the heap
// blocks the transaction has fetched from it so far.
type tableReading struct {
	relfilenode *uint32
	blocks      int64
}

// String prints a reading for a failure message.
func (r tableReading) String() string {
	if r.relfilenode == nil {
		return fmt.Sprintf("no pg_class row, %d blocks fetched", r.blocks)
	}
	return fmt.Sprintf("relfilenode %d, %d blocks fetched", *r.relfilenode, r.blocks)
}

// premiseRun is one statement as the premise test runs it: the classifier's answer, asked before
// the statement ran, the statement's own error, and each large table's reading before and after
// the statement.
type premiseRun struct {
	exemption     string
	err           error
	before, after map[string]tableReading
}

// runPremise asks the classifier about sql, then runs sql in a savepoint of a transaction that it
// rolls back, reading each large table by its OID before and after. The classifier is asked first,
// in a read-only repeatable-read transaction of its own, as the census asks it, because once a
// CREATE TABLE has run, Catalog read 4 would find the table. pg_stat_get_xact_blocks_fetched
// already holds the blocks of the transaction's earlier reads, and of earlier transactions whose
// counts are not flushed yet, so a reading is the difference between before and after inside one
// transaction.
func runPremise(t *testing.T, ctx context.Context, conn *pgx.Conn, sql string) premiseRun {
	t.Helper()
	var run premiseRun
	classify, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatalf("begin the classifier's transaction: %v", err)
	}
	defer func() { _ = classify.Rollback(ctx) }()
	if run.exemption, _, err = tableSizeExemption(ctx, classify, sql); err != nil {
		t.Fatalf("classify: %v", err)
	}
	if err := classify.Rollback(ctx); err != nil {
		t.Fatalf("roll the classifier's transaction back: %v", err)
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	oids := make(map[string]uint32, len(premiseTables))
	for _, table := range premiseTables {
		var oid uint32
		if err := tx.QueryRow(ctx, "select $1::regclass::oid", table).Scan(&oid); err != nil {
			t.Fatalf("take the oid of %s: %v", table, err)
		}
		oids[table] = oid
	}
	run.before = readTables(t, ctx, tx, oids)
	statement, err := tx.Begin(ctx)
	if err != nil {
		t.Fatalf("savepoint: %v", err)
	}
	if _, run.err = statement.Exec(ctx, sql); run.err != nil {
		err = statement.Rollback(ctx)
	} else {
		err = statement.Commit(ctx)
	}
	if err != nil {
		t.Fatalf("end the savepoint: %v", err)
	}
	run.after = readTables(t, ctx, tx, oids)
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("roll back: %v", err)
	}
	return run
}

// readTables reads each table of oids by its OID: its relfilenode and the heap blocks the
// transaction has fetched from it.
func readTables(t *testing.T, ctx context.Context, tx pgx.Tx, oids map[string]uint32) map[string]tableReading {
	t.Helper()
	readings := make(map[string]tableReading, len(oids))
	for table, oid := range oids {
		var reading tableReading
		if err := tx.QueryRow(ctx, "select (select relfilenode from pg_class where oid = $1::oid), pg_stat_get_xact_blocks_fetched($1::oid)", oid).Scan(&reading.relfilenode, &reading.blocks); err != nil {
			t.Fatalf("read %s by its oid: %v", table, err)
		}
		readings[table] = reading
	}
	return readings
}

// rewrote says whether the statement of run changed the relfilenode of table, which still exists.
func (run premiseRun) rewrote(table string) bool {
	before, after := run.before[table].relfilenode, run.after[table].relfilenode
	return before != nil && after != nil && *before != *after
}

// fetched is how many heap blocks of table the statement of run read.
func (run premiseRun) fetched(table string) int64 {
	return run.after[table].blocks - run.before[table].blocks
}

// The premise of every exemption: each statement the classifier admits reads no heap block of an
// existing table and rewrites none, so the time it holds its lock does not grow with the table.
// It runs on PostgreSQL 18, which production runs, on tables of at least 1,000 heap pages, every
// row of one migration of TestCensusAdmitsEveryCatalogOnlyForm and
// TestCensusStillRefusesEveryFormThatReadsOrRewritesRows, from the same Go values, so the tests
// cannot drift apart. For each row it asks the classifier, then runs the row in a transaction it
// rolls back, and reads each of the three large tables by the OID it took before: its
// relfilenode, which a rewrite changes, and the heap blocks the transaction fetched from it.
// Parallel workers are off, because the counter misses their reads. A statement the classifier
// admits must run, read no block of any of the three tables, and keep the relfilenode of each one
// that still exists, and every admitted row of the census test must be admitted here too, so the
// test cannot pass by refusing everything. A statement the classifier refuses may fail or move a
// reading. The controls show that the reading works: each is refused, and each must move its
// reading, or the test fails.
func TestEveryAdmittedFormReadsAndRewritesNoRow(t *testing.T) {
	ctx := context.Background()
	conn := catalogOnlyDatabase(t, ctx, 40000)
	var version string
	if err := conn.QueryRow(ctx, "show server_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	t.Logf("server_version %s", version)
	if !strings.HasPrefix(version, "18.") {
		t.Fatalf("server_version %s, want PostgreSQL 18, which production runs", version)
	}
	for _, table := range premiseTables {
		var pages int64
		if err := conn.QueryRow(ctx, "select pg_relation_size($1::regclass) / current_setting('block_size')::int", table).Scan(&pages); err != nil {
			t.Fatal(err)
		}
		t.Logf("%s holds %d heap pages", table, pages)
		if pages < 1000 {
			t.Fatalf("%s holds %d heap pages, want at least 1,000", table, pages)
		}
	}
	if _, err := conn.Exec(ctx, "set max_parallel_workers_per_gather = 0; set max_parallel_maintenance_workers = 0"); err != nil {
		t.Fatal(err)
	}
	// checkAdmitted holds a statement the classifier admits to the premise.
	checkAdmitted := func(t *testing.T, run premiseRun) {
		t.Helper()
		if run.err != nil {
			t.Fatalf("admitted as %q, the statement failed on the populated tables: %v", run.exemption, run.err)
		}
		for _, table := range premiseTables {
			if blocks := run.fetched(table); blocks != 0 {
				t.Errorf("admitted as %q, the statement read %d heap blocks of %s", run.exemption, blocks, table)
			}
			if run.rewrote(table) {
				t.Errorf("admitted as %q, the statement rewrote %s: its relfilenode changed", run.exemption, table)
			}
		}
	}
	t.Run("admitted", func(t *testing.T) {
		for _, row := range admittedRows {
			if len(row.migrations) != 1 {
				continue
			}
			t.Run(row.name, func(t *testing.T) {
				run := runPremise(t, ctx, conn, row.migrations[0])
				if run.exemption != row.kind {
					t.Fatalf("the classifier answered %q, want %q as the census test admits it", run.exemption, row.kind)
				}
				checkAdmitted(t, run)
			})
		}
	})
	// A refused row may fail or move a reading. One the classifier admits is held to the premise
	// like any admitted row, so a classifier that admits a refused form fails here too.
	t.Run("refused", func(t *testing.T) {
		for _, row := range refusedRows {
			if len(row.migrations) != 1 {
				continue
			}
			t.Run(row.name, func(t *testing.T) {
				if run := runPremise(t, ctx, conn, row.migrations[0]); run.exemption != "" {
					checkAdmitted(t, run)
				}
			})
		}
	})
	t.Run("controls", func(t *testing.T) {
		for _, control := range []struct {
			name, sql string
			moved     func(premiseRun) bool
		}{
			{"a volatile default rewrites big", "alter table big add column u uuid default gen_random_uuid()", func(run premiseRun) bool { return run.rewrote("big") }},
			{"a default through a volatile cast rewrites big", "alter table big add column v vol_target default 1", func(run premiseRun) bool { return run.rewrote("big") }},
			{"a check reads big", "alter table big add column c int check (c > 0)", func(run premiseRun) bool { return run.fetched("big") > 0 }},
			// ref is nullable and holds no null, so Postgres scans big for one.
			{"set not null reads big", "alter table big alter column ref set not null", func(run premiseRun) bool { return run.fetched("big") > 0 }},
			// The count reads big3 before the drop removes its pg_class row, and the counter is read
			// by the OID taken before both.
			{"a count before drop table reads big3", "select count(*) from big3; drop table big3;", func(run premiseRun) bool { return run.fetched("big3") > 0 }},
		} {
			t.Run(control.name, func(t *testing.T) {
				run := runPremise(t, ctx, conn, control.sql)
				if run.exemption != "" {
					t.Errorf("the classifier admitted the control as %q", run.exemption)
				}
				if run.err != nil {
					t.Fatalf("the control failed: %v", run.err)
				}
				if !control.moved(run) {
					t.Fatalf("the control did not move its reading: before %+v, after %+v", run.before, run.after)
				}
			})
		}
	})
}
