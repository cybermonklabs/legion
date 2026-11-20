package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/testpg/testdb"
)

// eventIDCatalogOffences reads the schema the migrations build, in no spelling at all, for what
// the safe horizon's proof needs of it (events/horizon.go, "Safe horizon"): only events.id's own
// default depends on events_id_seq, events has no child table and no partition, events is a
// plain table, and every function named currval, nextval, setval, lastval or
// pg_get_serial_sequence, or calling the C function of one, is Postgres's own. A name resolves by
// the search path and by argument type, so a currval(text) of public is called before
// pg_catalog.currval(regclass) for a text argument, and one of those names in a schema placed
// before pg_catalog is called before pg_catalog's; either can draw from the sequence in its body,
// which names nothing a reading of the text could see. A superuser can also add one to pg_catalog
// (pg_catalog.currval(text)), replace Postgres's own in place, give one of Postgres's own another's
// name (nextval renamed currval: Postgres calls a function of its own by its oid, so the name
// currval then draws), or make a function of another name in the internal language that calls
// nextval's C function (as 'nextval_oid'). So a function counts as Postgres's own only as the oid
// initdb gives its name (pg_proc.dat: nextval 1574, currval 1575, setval 1576 and 1765, lastval
// 2559, pg_get_serial_sequence 1665), in pg_catalog and still in the fixed pg_language.dat oid
// 12 (internal); every other function of one of those names or oids, or in language oid 12
// (internal) or 13 (c) with one of their C functions as its source, is named. It answers one row
// per offence, and a row also when events.id has no nextval('events_id_seq') default, since the
// read would then see nothing.
const eventIDCatalogOffences = `
	with own as (
		select ad.oid from pg_attrdef ad join pg_attribute a on a.attrelid = ad.adrelid and a.attnum = ad.adnum
		where ad.adrelid = 'events'::regclass and a.attname = 'id'
			and pg_get_expr(ad.adbin, ad.adrelid) = 'nextval(''events_id_seq''::regclass)'
	)
	select pg_describe_object(d.classid, d.objid, d.objsubid) || ' depends on events_id_seq'
	from pg_depend d
	where d.refclassid = 'pg_class'::regclass and d.refobjid = 'events_id_seq'::regclass
		and not (d.classid = 'pg_attrdef'::regclass and d.objid in (select oid from own))
	union all
	select inhrelid::regclass::text || ' is a child or a partition of events'
	from pg_inherits where inhparent = 'events'::regclass
	union all
	select 'events is not a plain table (relkind ' || relkind::text || ')'
	from pg_class where oid = 'events'::regclass and relkind <> 'r'
	union all
	select n.nspname || '.' || p.proname || '(' || pg_get_function_identity_arguments(p.oid) || ')'
		|| ' is a function of a sequence function''s name or code that is not Postgres''s own'
	from pg_proc p join pg_namespace n on n.oid = p.pronamespace
	where (p.proname in ('currval', 'nextval', 'setval', 'lastval', 'pg_get_serial_sequence')
			or p.oid in (1574, 1575, 1576, 1765, 2559, 1665)
			or (p.prolang in (12, 13)
				and p.prosrc in ('nextval_oid', 'currval_oid', 'setval_oid', 'setval3_oid', 'lastval', 'pg_get_serial_sequence')))
		and not (n.nspname = 'pg_catalog' and p.prolang = 12
			and (p.proname, p.oid) in (values ('nextval'::name, 1574::oid), ('currval', 1575), ('setval', 1576),
				('setval', 1765), ('lastval', 2559), ('pg_get_serial_sequence', 1665)))
	union all
	select 'events.id has no nextval(''events_id_seq'') default: this check reads nothing'
	where not exists (select 1 from own)`

type catalogQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func readEventIDCatalogOffences(ctx context.Context, t *testing.T, db catalogQuerier) []string {
	t.Helper()
	rows, err := db.Query(ctx, eventIDCatalogOffences)
	if err != nil {
		t.Fatal(err)
	}
	offences, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return offences
}

// TestOnlyTheEventsIDDefaultDrawsFromItsSequence holds the migrated schema to
// eventIDCatalogOffences. A table made `like events including defaults` (or `including all`),
// `inherits (events)` or `partition of events` takes events' id default without its migration
// naming the sequence, and a write to it takes RowExclusiveLock on that table, not on events,
// which is the lock the horizon dates a writer by. A child's or a partition's rows are rows of
// events too, so the horizon would pass an id a write to one still holds open. A view or a
// SQL-body function that names the sequence depends on it here as well; a plpgsql body records no
// dependency, which TestNoMigrationDrawsAnEventIDOutsideTheInsertDefault reads the text for. And a
// function named like one of Postgres's sequence functions that is not Postgres's own is refused
// by its name alone: its body need not name the sequence to draw from it (currval(text) calling
// nextval($1::regclass)), and a call elsewhere resolves to it, bare or, for one a superuser made in
// pg_catalog or put in place of Postgres's own, as pg_catalog's (store.LatestEventID names
// pg_catalog's).
func TestOnlyTheEventsIDDefaultDrawsFromItsSequence(t *testing.T) {
	store := openEmptyTestStore(t)
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	t.Run("the read names each shape that draws from the sequence or writes rows of events", func(t *testing.T) {
		tx, err := store.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		for _, statement := range []string{
			`create table rt_child () inherits (events)`,
			`create table rt_like (like events including defaults)`,
			`create view rt_view as select nextval('events_id_seq') as id`,
			`create function public.currval(text) returns bigint language sql as $$ select nextval($1::regclass) $$`,
			`create schema rt_util`,
			`create function rt_util.pg_get_serial_sequence(text, text) returns text language sql as $$ select 'events_id_seq' $$`,
		} {
			if _, err := tx.Exec(ctx, statement); err != nil {
				t.Fatalf("%s: %v", statement, err)
			}
		}
		got := strings.Join(readEventIDCatalogOffences(ctx, t, tx), "\n")
		for _, want := range []string{
			"rt_child is a child or a partition of events",
			"default value for column id of table rt_child depends on events_id_seq",
			"default value for column id of table rt_like depends on events_id_seq",
			"rule _RETURN on view rt_view depends on events_id_seq",
			"public.currval(text) is a function of a sequence function's name or code that is not Postgres's own",
			"rt_util.pg_get_serial_sequence(text, text) is a function of a sequence function's name or code that is not Postgres's own",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("the read did not name %q; it read:\n%s", want, got)
			}
		}
	})

	// Only a superuser can change pg_catalog or make a function in the internal language, and a
	// pod's test server logs in as none (packages/daemon/docker/legion-testpg). Each shape is made
	// alone, so each is shown to be named by itself: a function added to pg_catalog in the internal
	// language (Postgres's own textlen under currval's name) has an oid initdb did not give currval;
	// Postgres's own currval(regclass) replaced in place keeps its oid and is no longer in the
	// internal language; Postgres's own nextval renamed currval keeps nextval's oid, which Postgres
	// calls nextval's C function by; and a function of another name in the internal language calls
	// nextval's C function by its source.
	t.Run("the read names a sequence function a superuser made or replaced in pg_catalog", func(t *testing.T) {
		for _, plant := range []struct{ statement, want string }{
			{`create function pg_catalog.currval(text) returns integer language internal immutable strict as 'textlen'`,
				"pg_catalog.currval(text) is a function of a sequence function's name or code that is not Postgres's own"},
			{`create or replace function pg_catalog.currval(regclass) returns bigint language sql as $$ select pg_catalog.nextval($1) $$`,
				"pg_catalog.currval(regclass) is a function of a sequence function's name or code that is not Postgres's own"},
			{`alter function pg_catalog.currval(regclass) rename to rt_currval; alter function pg_catalog.nextval(regclass) rename to currval`,
				"pg_catalog.currval(regclass) is a function of a sequence function's name or code that is not Postgres's own"},
			{`create function public.rt_draw(regclass) returns bigint language internal strict volatile as 'nextval_oid'`,
				"public.rt_draw(regclass) is a function of a sequence function's name or code that is not Postgres's own"},
			{`create function public.rt_draw_after_language_rename(regclass) returns bigint language internal strict volatile as 'nextval_oid'; alter language internal rename to rt_internal`,
				"public.rt_draw_after_language_rename(regclass) is a function of a sequence function's name or code that is not Postgres's own"},
		} {
			func() {
				tx, err := store.Pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = tx.Rollback(ctx) }()
				_, err = tx.Exec(ctx, plant.statement)
				var pgErr *pgconn.PgError
				if errors.As(err, &pgErr) && pgErr.Code == "42501" {
					testdb.SkipOutsideCI(t, "the test server's login cannot change pg_catalog (it is no superuser), so the read is not shown to name a function made there: %v", err)
				}
				if err != nil {
					t.Fatalf("%s: %v", plant.statement, err)
				}
				if got := strings.Join(readEventIDCatalogOffences(ctx, t, tx), "\n"); !strings.Contains(got, plant.want) {
					t.Errorf("the read did not name %q; it read:\n%s", plant.want, got)
				}
			}()
		}
	})

	if offences := readEventIDCatalogOffences(ctx, t, store.Pool); len(offences) > 0 {
		t.Fatalf("the migrated schema lets an event id be drawn, or an event row be written, outside an "+
			"insert into events:\n\t%s\nLet only events.id's default draw from events_id_seq, keep events a "+
			"plain table with no child or partition, and define no function of a sequence function's name "+
			"but Postgres's own: the safe horizon dates a writer by the RowExclusiveLock its insert takes on "+
			"events (events/horizon.go).", strings.Join(offences, "\n\t"))
	}
}
