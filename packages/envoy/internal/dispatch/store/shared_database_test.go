package store

import (
	"context"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	brokerstore "github.com/sjawhar/envoy/internal/cipher/store"
	"github.com/sjawhar/envoy/internal/testpg/testdb"
)

// adminDatabaseURL is the test server's maintenance database, on which a test makes databases of
// its own.
func adminDatabaseURL(t *testing.T) string {
	t.Helper()
	admin, err := url.Parse(testDatabaseURL(t))
	if err != nil {
		t.Fatalf("parse test database URL: %v", err)
	}
	admin.Path = "/postgres"
	return admin.String()
}

// TestTheBrokerAndDispatchSchemasShareOneDatabase: CI runs the broker's and Dispatch's tests
// against one database (ci-job-envoy-go.yaml sets BROKER_TEST_DATABASE_URL and
// DISPATCH_TEST_DATABASE_URL to the same one), and some tests of each migrate that database
// itself rather than a clone, in whichever order the packages run. So the two migration sets
// must create no schema-global name in common: a function, a table, a type, a sequence or an
// index that both create fails the second set's migration (two functions named
// refuse_recorded_account_change did, in broker migration 0042 and Dispatch migration
// 20261117160116). Each order is migrated into a fresh database of its own, twice, the second
// pass applying nothing.
//
// Control: migration 20261117160116's function named refuse_recorded_account_change again, and
// 20261118041916's drop of it named so too (both orders fail, SQLSTATE 42723).
func TestTheBrokerAndDispatchSchemasShareOneDatabase(t *testing.T) {
	ctx := context.Background()
	admin := adminDatabaseURL(t)
	for _, order := range []string{"broker first", "Dispatch first"} {
		t.Run(order, func(t *testing.T) {
			dsn := testdb.DatabaseOn(t, admin, "dispatch_shared")
			dispatch, err := Open(ctx, dsn)
			if err != nil {
				t.Fatalf("open the shared database as Dispatch: %v", err)
			}
			t.Cleanup(dispatch.Pool.Close)
			broker, err := brokerstore.Open(ctx, dsn)
			if err != nil {
				t.Fatalf("open the shared database as the broker: %v", err)
			}
			t.Cleanup(broker.Close)
			steps := []struct {
				name    string
				migrate func(context.Context) error
			}{{"the broker's migrations", broker.Migrate}, {"Dispatch's migrations", dispatch.Migrate}}
			if order == "Dispatch first" {
				steps[0], steps[1] = steps[1], steps[0]
			}
			for pass := 1; pass <= 2; pass++ {
				for _, step := range steps {
					if err := step.migrate(ctx); err != nil {
						t.Fatalf("pass %d, %s on a database the other set shares: %v", pass, step.name, err)
					}
				}
			}
		})
	}
}

// TestTheBrokerAndDispatchMigrationsCreateNoNameInCommon: the shared database above fails only on
// a statement that refuses an existing name, and both sets use `create ... if not exists` and
// `create or replace function` widely, which pass silently over a name the other set made, the
// second set then reading or replacing the first's object. So each set is migrated alone into a
// fresh database of its own, and the names each creates in its schema are compared: every
// relation (table, index, sequence, view), function and type, leaving out an extension's own
// objects, which both sets may share by design. None may be in both.
//
// Control, in the test: a function named as one of the broker's is added to Dispatch's database,
// and the comparison names it.
func TestTheBrokerAndDispatchMigrationsCreateNoNameInCommon(t *testing.T) {
	ctx := context.Background()
	admin := adminDatabaseURL(t)
	dispatch, err := Open(ctx, testdb.DatabaseOn(t, admin, "dispatch_names"))
	if err != nil {
		t.Fatalf("open Dispatch's database: %v", err)
	}
	t.Cleanup(dispatch.Pool.Close)
	if err := dispatch.Migrate(ctx); err != nil {
		t.Fatalf("migrate Dispatch alone: %v", err)
	}
	broker, err := brokerstore.Open(ctx, testdb.DatabaseOn(t, admin, "broker_names"))
	if err != nil {
		t.Fatalf("open the broker's database: %v", err)
	}
	t.Cleanup(broker.Close)
	if err := broker.Migrate(ctx); err != nil {
		t.Fatalf("migrate the broker alone: %v", err)
	}
	brokerNames := schemaNames(t, broker.Pool)
	if len(brokerNames) == 0 {
		t.Fatal("the broker's migrations created no name: the comparison would see nothing")
	}
	dispatchNames := schemaNames(t, dispatch.Pool)
	if len(dispatchNames) == 0 {
		t.Fatal("Dispatch's migrations created no name: the comparison would see nothing")
	}
	if common := commonNames(dispatchNames, brokerNames); len(common) > 0 {
		t.Errorf("both migration sets create %v: on one database the second set's if-not-exists or or-replace statement reuses or replaces the first's object", common)
	}

	var brokerFunction string
	for _, name := range brokerNames {
		if function, ok := strings.CutPrefix(name, "function "); ok {
			brokerFunction = function
			break
		}
	}
	if brokerFunction == "" {
		t.Fatalf("the broker's migrations create no function, so the control has none to copy: %v", brokerNames)
	}
	if _, err := dispatch.Pool.Exec(ctx, `create function `+pgx.Identifier{brokerFunction}.Sanitize()+`() returns int language sql as 'select 1'`); err != nil {
		t.Fatalf("control: add a function named as the broker's %s: %v", brokerFunction, err)
	}
	if common := commonNames(schemaNames(t, dispatch.Pool), brokerNames); !slices.Contains(common, "function "+brokerFunction) {
		t.Errorf("control: with Dispatch's database holding a function named %s, the comparison found %v, so it would not see a name both sets create", brokerFunction, common)
	}
}

// schemaNames is every relation, function and type the database's public schema holds that no
// extension owns, each as "<kind> <name>", sorted as Go sorts strings.
func schemaNames(t *testing.T, database interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}) []string {
	t.Helper()
	rows, err := database.Query(context.Background(), `
		select 'relation ' || c.relname from pg_class c
		where c.relnamespace = 'public'::regnamespace
		  and not exists (select 1 from pg_depend d where d.classid = 'pg_class'::regclass and d.objid = c.oid and d.deptype = 'e')
		union
		select 'function ' || p.proname from pg_proc p
		where p.pronamespace = 'public'::regnamespace
		  and not exists (select 1 from pg_depend d where d.classid = 'pg_proc'::regclass and d.objid = p.oid and d.deptype = 'e')
		union
		select 'type ' || ty.typname from pg_type ty
		where ty.typnamespace = 'public'::regnamespace
		  and not exists (select 1 from pg_depend d where d.classid = 'pg_type'::regclass and d.objid = ty.oid and d.deptype = 'e')`)
	if err != nil {
		t.Fatalf("read the schema's names: %v", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the schema's names: %v", err)
	}
	slices.Sort(names)
	return names
}

// commonNames is every name in both sorted lists.
func commonNames(a, b []string) []string {
	var common []string
	for _, name := range a {
		if _, found := slices.BinarySearch(b, name); found {
			common = append(common, name)
		}
	}
	return common
}
