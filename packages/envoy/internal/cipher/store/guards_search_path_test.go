package store_test

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cipher/store"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
)

// recordedAccountTables are the tables whose guards hold the broker's record of which Google
// account holds an address: every trigger of theirs runs a function migration 0044 pins.
var recordedAccountTables = []string{
	"replaced_person_subjects", "person_replacement_generations",
	"person_signins", "launcher_credentials", "browser_sessions", "oauth_access_tokens", "oauth_refresh_tokens",
}

// tablesSchema is the schema the broker's tables are in: the current schema of st's connections,
// under which the runner created them.
func tablesSchema(t *testing.T, st *store.Store) string {
	t.Helper()
	var schema string
	if err := st.Pool.QueryRow(context.Background(), `select current_schema()`).Scan(&schema); err != nil {
		t.Fatalf("read the current schema: %v", err)
	}
	return schema
}

// TestEveryRecordedAccountTriggerRunsUnderItsOwnSearchPath reads every trigger of those tables and
// holds the function it runs to search_path = pg_catalog, the schema the tables are in, pg_temp
// (migration 0044). A trigger function with no setting of its own resolves its tables, functions
// and operators under the caller's search_path, where a temporary table or a schema listed before
// pg_catalog is the caller's to make, so a guard added later fails this test until it is pinned.
//
// Control, in the test: with one function's setting reset, the read names it.
func TestEveryRecordedAccountTriggerRunsUnderItsOwnSearchPath(t *testing.T) {
	ctx := context.Background()
	st := storetest.Open(t)
	schema := tablesSchema(t, st)
	want := "search_path=pg_catalog, " + schema + ", pg_temp"
	read := func(t *testing.T) []string {
		t.Helper()
		rows, err := st.Pool.Query(ctx, `select distinct p.proname from pg_trigger t
			join pg_proc p on p.oid = t.tgfoid
			where not t.tgisinternal and t.tgrelid = any($1::regclass[])
				and coalesce(array_to_string(p.proconfig, ','), '') is distinct from $2
			order by p.proname`, recordedAccountTables, want)
		if err != nil {
			t.Fatalf("read the guards of the broker's record of accounts: %v", err)
		}
		names, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("read the guards of the broker's record of accounts: %v", err)
		}
		return names
	}
	if names := read(t); len(names) != 0 {
		t.Errorf("the trigger functions %q of %q run under the caller's search_path, want every one set to %q", names, recordedAccountTables, want)
	}
	if _, err := st.Pool.Exec(ctx, `alter function guard_person_replacement_generations() reset search_path`); err != nil {
		t.Fatal(err)
	}
	if names := read(t); !slices.Equal(names, []string{"guard_person_replacement_generations"}) {
		t.Errorf("control: with guard_person_replacement_generations' setting reset, the read names %q, want it alone: the test can see an unpinned guard", names)
	}
}

// TestASessionOperatorDoesNotLowerTheAppliedGeneration: guard_person_replacement_generations
// refuses new.session_generation < old.session_generation, the rule that keeps the greatest
// replacement the broker applied at an address from moving back — which, as 0042's own comment
// says, "lets a late revoke apply again", ending what the account that holds the address now made.
// `<` is an operator the caller's search_path chooses, so a session that makes a schema of its own
// holding a `<` on bigint that answers false, and lists it before pg_catalog, would have the guard
// admit the lowering. The function is pinned (0044), so pg_catalog's operator takes the comparison
// and the lowering is refused with SQLSTATE 23514.
//
// Control, in the test: with the function's setting reset, the session's operator answers and the
// same lowering passes.
func TestASessionOperatorDoesNotLowerTheAppliedGeneration(t *testing.T) {
	ctx := context.Background()
	st := storetest.Open(t)
	schema := tablesSchema(t, st)
	const person = "session-operator@example.com"
	if _, err := st.Pool.Exec(ctx, `insert into person_replacement_generations (person, session_generation) values ($1, 7)`, person); err != nil {
		t.Fatalf("seed the applied generation: %v", err)
	}
	lower := func(t *testing.T, setup ...string) error {
		t.Helper()
		tx, err := st.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		for _, step := range append([]string{
			`create schema session_ops`,
			`create function session_ops.never_less(bigint, bigint) returns boolean language sql immutable as 'select false'`,
			`create operator session_ops.< (leftarg = bigint, rightarg = bigint, function = session_ops.never_less)`,
		}, setup...) {
			if _, err := tx.Exec(ctx, step); err != nil {
				t.Fatalf("%s: %v", step, err)
			}
		}
		if _, err := tx.Exec(ctx, `select set_config('search_path', 'session_ops, pg_catalog, `+schema+`', true)`); err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(ctx, `update `+schema+`.person_replacement_generations set session_generation = 0 where person = $1`, person)
		return err
	}
	if err := lower(t); !isCheckViolation(err) {
		t.Errorf("under a session operator `<` that answers false, lowering the applied generation answered %v; want it refused with SQLSTATE 23514", err)
	}
	if err := lower(t, `alter function `+schema+`.guard_person_replacement_generations() reset search_path`); err != nil {
		t.Errorf("control: with the guard's setting reset, the lowering answered %v; want it to pass under the session's operator", err)
	}
}
