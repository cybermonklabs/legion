package store

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// clockGuards are the two functions migration 20261118200651 replaces, each run by a trigger that
// holds a stamp to the database's clock, and whose search_path 20261119021826 sets.
var clockGuards = []string{"guard_people_binding_columns", "guard_broker_revocation_columns"}

// tablesSchema is the schema s's tables are in: the current schema of its connections, under which
// the runner created them.
func tablesSchema(t *testing.T, s *Store) string {
	t.Helper()
	var schema string
	if err := s.Pool.QueryRow(context.Background(), `select current_schema()`).Scan(&schema); err != nil {
		t.Fatalf("read the current schema: %v", err)
	}
	return schema
}

// guardSettings is the settings function guard of schema runs with (pg_proc.proconfig), none for
// an empty slice.
func guardSettings(t *testing.T, s *Store, schema, guard string) []string {
	t.Helper()
	var config []string
	if err := s.Pool.QueryRow(context.Background(), `select coalesce(proconfig, '{}') from pg_proc where proname = $1 and pronamespace = $2::regnamespace`,
		guard, schema).Scan(&config); err != nil {
		t.Fatalf("read %s's settings: %v", guard, err)
	}
	return config
}

// TestTheClockGuardsRunUnderTheirOwnSearchPath: both clock guards run with search_path set to
// pg_catalog, the schema the runner made the tables in, and pg_temp (20261119021826). So a session
// whose search_path lists a schema with a clock_timestamp() of its own before pg_catalog does not
// change the guard's clock: under a session clock a day ahead, a stamp an hour later than now() is
// still refused with SQLSTATE 23514.
//
// Control, in the test, per guard: with the guard's own setting reset (as 20261118200651 left it),
// the same stamp passes under the session's clock.
func TestTheClockGuardsRunUnderTheirOwnSearchPath(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	schema := tablesSchema(t, database)
	for _, guard := range clockGuards {
		if got, want := guardSettings(t, database, schema, guard), "search_path=pg_catalog, "+schema+", pg_temp"; !slices.Equal(got, []string{want}) {
			t.Errorf("%s runs with the settings %q, want %s alone", guard, got, want)
		}
	}
	const email = "ahead@example.com"
	signInAll(t, brokerPeople(database), email, "member-1", "member-2")
	if _, err := database.Pool.Exec(ctx, `create schema ahead_clock;
		create function ahead_clock.clock_timestamp() returns timestamptz language sql volatile
			as $$ select pg_catalog.clock_timestamp() + interval '1 day' $$`); err != nil {
		t.Fatalf("make the session's clock: %v", err)
	}
	// underAheadClock runs statement for email in a transaction whose session search_path puts
	// ahead_clock first, after the setup statements, rolled back, and answers its error.
	underAheadClock := func(t *testing.T, statement string, setup ...string) error {
		t.Helper()
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		for _, step := range append([]string{`select set_config('search_path', 'ahead_clock, pg_catalog, ' || current_schema(), true)`}, setup...) {
			if _, err := tx.Exec(ctx, step); err != nil {
				t.Fatalf("%s: %v", step, err)
			}
		}
		var ahead bool
		if err := tx.QueryRow(ctx, `select clock_timestamp() > now() + interval '1 hour'`).Scan(&ahead); err != nil || !ahead {
			t.Fatalf("the session's clock_timestamp() is a day ahead: %t, %v; want true, or the test shows nothing", ahead, err)
		}
		_, err = tx.Exec(ctx, statement, email)
		return err
	}
	for _, write := range []struct{ guard, statement string }{
		{"guard_people_binding_columns", `update people set confirmed_at = now() + interval '1 hour' where email = $1`},
		{"guard_broker_revocation_columns", `update broker_revocations set tried_at = now() + interval '1 hour' where email = $1`},
	} {
		t.Run(write.guard, func(t *testing.T) {
			if err := underAheadClock(t, write.statement); !isSQLState(err, "23514") {
				t.Errorf("a stamp an hour ahead under a session clock a day ahead answered %v; want it refused with SQLSTATE 23514: the guard reads pg_catalog's clock", err)
			}
			if err := underAheadClock(t, write.statement, `alter function `+write.guard+`() reset search_path`); err != nil {
				t.Errorf("control: with %s's setting reset, the stamp answered %v; want it to pass under the session's clock", write.guard, err)
			}
		})
	}
}

// TestATemporaryTableNamedPeopleDoesNotShadowTheGuardsRead: a session's temporary table named
// people, holding another account for the address, does not reach guard_broker_revocation_columns,
// whose search_path lists pg_temp last: a recording of a revocation that names the account the
// address records as its holder, its statement naming the tables with their schema, passes.
//
// Control, in the test: with the guard's setting reset (as 20261118200651 left it), Postgres
// searches the temporary schema first, the guard reads the temporary table's account, and the same
// recording is refused with SQLSTATE 23514.
func TestATemporaryTableNamedPeopleDoesNotShadowTheGuardsRead(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	schema := tablesSchema(t, database)
	const email = "shadowed@example.com"
	signInAll(t, brokerPeople(database), email, "member-1", "member-2")
	recordAgain := `update ` + schema + `.broker_revocations set generation = generation + 1,
		holder_subject = (select sub from ` + schema + `.people p where p.email = $1),
		session_generation = (select generation from ` + schema + `.user_sessions u where u.login = $1),
		done_at = null, recorded_at = clock_timestamp() where email = $1 and subject = 'member-1'`
	// withTemporaryPeople runs the recording for email in a transaction holding a temporary people
	// table that names another account for the address, after the setup statements, rolled back.
	withTemporaryPeople := func(t *testing.T, setup ...string) error {
		t.Helper()
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		for _, step := range append([]string{
			`create temporary table people (email text, sub text) on commit drop`,
			`insert into pg_temp.people values ('` + email + `', 'stranger')`,
		}, setup...) {
			if _, err := tx.Exec(ctx, step); err != nil {
				t.Fatalf("%s: %v", step, err)
			}
		}
		_, err = tx.Exec(ctx, recordAgain, email)
		return err
	}
	if err := withTemporaryPeople(t); err != nil {
		t.Errorf("the recording beside a temporary people table answered %v; want it to pass: the guard reads %s.people", err, schema)
	}
	if err := withTemporaryPeople(t, `alter function guard_broker_revocation_columns() reset search_path`); !isSQLState(err, "23514") {
		t.Errorf("control: with the guard's setting reset, the recording answered %v; want it refused with SQLSTATE 23514, the guard reading the temporary table", err)
	}
}

// TestATemporaryTableNamedLikeASessionTableDoesNotTakeTheGenerationsWrite: the two functions a
// replacing sign-in runs after its people write name user_sessions and session_generation_floors
// without a schema — end_people_sessions_on_new_account (20261118041916), which moves the
// address's session generation, and start_session_generation_at_floor (20261119060414), which
// starts a new row at the generation the secrets broker recorded. Postgres searches a session's
// temporary schema before every schema of a path that does not list pg_temp, so a session holding
// a temporary table of either name could take the generation's move, leaving the real row where it
// was and every cookie of the replaced account matching, or decide where a row made now starts.
// Both run with search_path set to pg_catalog, the schema the tables are in, pg_temp
// (20261119021826 and 20261119060414), so the real tables take both writes.
//
// Control, in the test, per function: with that function's setting reset, the temporary table
// takes the write.
func TestATemporaryTableNamedLikeASessionTableDoesNotTakeTheGenerationsWrite(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	schema := tablesSchema(t, database)
	for _, function := range []string{"end_people_sessions_on_new_account", "start_session_generation_at_floor"} {
		if got, want := guardSettings(t, database, schema, function), "search_path=pg_catalog, "+schema+", pg_temp"; !slices.Equal(got, []string{want}) {
			t.Errorf("%s runs with the settings %q, want %s alone", function, got, want)
		}
	}
	const replaced, fresh = "shadowed-sessions@example.com", "shadowed-first@example.com"
	signInAll(t, brokerPeople(database), replaced, "member-1")
	if _, err := database.Pool.Exec(ctx, `insert into people (email) values ($1)`, fresh); err != nil {
		t.Fatalf("the address with no session row yet: %v", err)
	}
	// recordBeside records account for email in a transaction holding a temporary table of each
	// name, the temporary floors naming 4242 for both addresses, after the setup statements, and
	// answers the address's session generation in the real table and in the temporary one (-1 for
	// no row of it). The transaction is rolled back, so no row and neither setting outlives it.
	recordBeside := func(t *testing.T, email, account string, setup ...string) (real, temporary int64) {
		t.Helper()
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		for _, step := range append([]string{
			`create temporary table user_sessions (login text primary key, generation bigint not null) on commit drop`,
			`create temporary table session_generation_floors (login text primary key, generation bigint not null) on commit drop`,
			`insert into pg_temp.session_generation_floors values ('` + replaced + `', 4242), ('` + fresh + `', 4242)`,
		}, setup...) {
			if _, err := tx.Exec(ctx, step); err != nil {
				t.Fatalf("%s: %v", step, err)
			}
		}
		if _, err := tx.Exec(ctx, `update `+schema+`.people set sub = $2 where email = $1`, email, account); err != nil {
			t.Fatalf("the sign-in's people write for %s: %v", email, err)
		}
		read := func(table string) int64 {
			t.Helper()
			generation := int64(-1)
			if err := tx.QueryRow(ctx, `select coalesce((select generation from `+table+` where login = $1), -1)`, email).Scan(&generation); err != nil {
				t.Fatalf("read %s's generation in %s: %v", email, table, err)
			}
			return generation
		}
		return read(schema + ".user_sessions"), read("pg_temp.user_sessions")
	}
	held := sessionGenerationOf(t, database.Pool, replaced)
	if real, temporary := recordBeside(t, replaced, "member-2"); real != held+1 || temporary != -1 {
		t.Errorf("beside temporary tables of both names, the replacing sign-in left %s at generation %d in %s.user_sessions and %d in pg_temp's, want %d and none",
			replaced, real, schema, temporary, held+1)
	}
	if real, temporary := recordBeside(t, replaced, "member-3", `alter function end_people_sessions_on_new_account() reset search_path`); real != held || temporary != 1 {
		t.Errorf("control: with end_people_sessions_on_new_account's setting reset, the sign-in left %s at generation %d in %s.user_sessions and %d in pg_temp's, want %d and 1: the temporary table takes the move",
			replaced, real, schema, temporary, held)
	}
	// The floor is read by a trigger of its own, on the insert of a session row: a statement that
	// makes the row names the real table, as every writer of Dispatch does, and the question is
	// which session_generation_floors that trigger reads while a temporary one of that name
	// stands. (The sign-in's own insert cannot show it: its function is pinned, and a function
	// called under a pinned one inherits that path.)
	makeSessionRow := func(t *testing.T, setup ...string) int64 {
		t.Helper()
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		for _, step := range append([]string{
			`create temporary table session_generation_floors (login text primary key, generation bigint not null) on commit drop`,
			`insert into pg_temp.session_generation_floors values ('` + fresh + `', 4242)`,
		}, setup...) {
			if _, err := tx.Exec(ctx, step); err != nil {
				t.Fatalf("%s: %v", step, err)
			}
		}
		var generation int64
		if err := tx.QueryRow(ctx, `insert into `+schema+`.user_sessions (login) values ($1) returning generation`, fresh).Scan(&generation); err != nil {
			t.Fatalf("make %s's session row: %v", fresh, err)
		}
		return generation
	}
	if generation := makeSessionRow(t); generation != 0 {
		t.Errorf("beside a temporary session_generation_floors naming 4242, %s's new session row starts at generation %d, want 0: the real floors, which hold none for it, decide where it starts", fresh, generation)
	}
	if generation := makeSessionRow(t, `alter function start_session_generation_at_floor() reset search_path`); generation != 4242 {
		t.Errorf("control: with start_session_generation_at_floor's setting reset, %s's new session row starts at generation %d, want 4242: the temporary floor decides where it starts", fresh, generation)
	}
}

// TestTheGuardsAcceptAWritersOwnStampAfterTheClockStepsBack: after the database's clock steps
// backwards, a transaction that began before the step holds a now() later than clock_timestamp().
// The guards on people.confirmed_at and broker_revocations.tried_at compare a stamp with
// greatest(now(), clock_timestamp()) (migration 20261118200651), so the writers' own stamps, now(),
// pass, while a stamp later than both, which no writer makes, is refused with SQLSTATE 23514.
//
// The test cannot step the database's clock. It sets clock_timestamp() back a minute instead, for
// one transaction alone: a function of that name in a schema of its own, which the transaction puts
// before pg_catalog on each guard's own search_path (alter function, rolled back with the
// transaction), so the guards' unqualified clock_timestamp() resolves to it. What this shows is the
// guards' comparison against a clock behind now(); it does not show the server's own clock
// stepping, which the test has no way to make.
//
// Control: put clock_timestamp() back in place of greatest(now(), clock_timestamp()) in either
// guard of 20261118200651, and that guard's own-stamp write is refused.
func TestTheGuardsAcceptAWritersOwnStampAfterTheClockStepsBack(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	const email = "clock@example.com"
	signInAll(t, brokerPeople(database), email, "member-1", "member-2")
	// The database is the test's own (migratedEmptyStore), dropped at its end, so the schema needs
	// no unique name and no cleanup of its own.
	const schema = "stepped_clock"
	if _, err := database.Pool.Exec(ctx, `create schema `+schema+`;
		create function `+schema+`.clock_timestamp() returns timestamptz language sql volatile
			as $$ select pg_catalog.clock_timestamp() - interval '1 minute' $$`); err != nil {
		t.Fatalf("make the stepped clock: %v", err)
	}
	// steppedBack runs statement, for email, in a transaction whose clock_timestamp() is a minute
	// behind its now(), rolled back, and answers its error.
	steppedBack := func(t *testing.T, statement string) error {
		t.Helper()
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		for _, guard := range clockGuards {
			if _, err := tx.Exec(ctx, `alter function `+guard+`() set search_path = `+schema+`, pg_catalog, public, pg_temp`); err != nil {
				t.Fatalf("put the stepped clock first for %s: %v", guard, err)
			}
		}
		var behind bool
		if err := tx.QueryRow(ctx, `select `+schema+`.clock_timestamp() < now()`).Scan(&behind); err != nil || !behind {
			t.Fatalf("the stepped clock is behind the transaction's now(): %t, %v; want true, or the test shows nothing", behind, err)
		}
		_, err = tx.Exec(ctx, statement, email)
		return err
	}
	for _, write := range []struct{ column, own, later string }{
		{"people.confirmed_at",
			`update people set confirmed_at = now() where email = $1`,
			`update people set confirmed_at = now() + interval '1 day' where email = $1`},
		{"broker_revocations.tried_at",
			`update broker_revocations set tried_at = now() where email = $1`,
			`update broker_revocations set tried_at = now() + interval '1 day' where email = $1`},
	} {
		t.Run(write.column, func(t *testing.T) {
			if err := steppedBack(t, write.own); err != nil {
				t.Errorf("the writer's own stamp of %s, now(), with the clock a minute behind it, answered %v; want it accepted", write.column, err)
			}
			if err := steppedBack(t, write.later); !isSQLState(err, "23514") {
				t.Errorf("a stamp of %s a day later than now() answered %v; want it refused with SQLSTATE 23514", write.column, err)
			}
		})
	}
}

// bindingTables are the tables the account binding's guards stand on, with the reset's own record
// of its start and end: every trigger of theirs runs a function this schema must pin
// (TestEveryBindingTriggerRunsUnderItsOwnSearchPath).
var bindingTables = []string{"people", "user_sessions", "agent_tokens", "broker_revocations", "session_generation_floors", "deployment_resets"}

// TestEveryBindingTriggerRunsUnderItsOwnSearchPath reads every trigger of the binding's tables and
// holds the function it runs to search_path = pg_catalog, the schema the tables are in, pg_temp
// (20261119021826, 20261119060414, 20261119065002 and 20261120144819). A function with none
// resolves its tables, functions and operators under the caller's path, where a temporary table or
// a schema listed before pg_catalog is the caller's to make; so a guard added later fails this test
// until it is pinned as well.
//
// Control, in the test: with one function's setting reset, the test's own read names it.
func TestEveryBindingTriggerRunsUnderItsOwnSearchPath(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	schema := tablesSchema(t, database)
	want := "search_path=pg_catalog, " + schema + ", pg_temp"
	read := func(t *testing.T) []string {
		t.Helper()
		rows, err := database.Pool.Query(ctx, `select distinct p.proname from pg_trigger t
			join pg_proc p on p.oid = t.tgfoid
			where not t.tgisinternal and t.tgrelid = any($1::regclass[])
				and coalesce(array_to_string(p.proconfig, ','), '') is distinct from $2
			order by p.proname`, bindingTables, want)
		if err != nil {
			t.Fatalf("read the binding's triggers: %v", err)
		}
		names, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("read the binding's triggers: %v", err)
		}
		return names
	}
	if names := read(t); len(names) != 0 {
		t.Errorf("the trigger functions %q of %q run under the caller's search_path, want every one set to %q", names, bindingTables, want)
	}
	if _, err := database.Pool.Exec(ctx, `alter function guard_user_sessions_binding_columns() reset search_path`); err != nil {
		t.Fatal(err)
	}
	if names := read(t); !slices.Equal(names, []string{"guard_user_sessions_binding_columns"}) {
		t.Errorf("control: with guard_user_sessions_binding_columns' setting reset, the read names %q, want it alone: the test can see an unpinned guard", names)
	}
}

// TestASessionOperatorDoesNotMoveAGenerationBack: guard_user_sessions_binding_columns refuses
// new.generation < old.generation, an operator its caller's search_path could choose. A session
// that makes a schema of its own, holding a `<` on bigint that answers false, and lists it before
// pg_catalog, would have the guard admit a generation moved back, where every cookie of the
// replaced account matches again. The function is pinned (20261119065002), so pg_catalog's
// operator takes the comparison and the lowering is refused with SQLSTATE 23514.
//
// Control, in the test: with the function's setting reset, the session's operator answers and the
// same lowering passes.
func TestASessionOperatorDoesNotMoveAGenerationBack(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	schema := tablesSchema(t, database)
	const email = "session-operator@example.com"
	signInAll(t, brokerPeople(database), email, "account-a", "account-b")
	// lower runs the lowering in a transaction holding a session operator `<` that answers false,
	// after the setup statements, rolled back, and answers its error.
	lower := func(t *testing.T, setup ...string) error {
		t.Helper()
		tx, err := database.Pool.Begin(ctx)
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
		_, err = tx.Exec(ctx, `update `+schema+`.user_sessions set generation = 0 where login = $1`, email)
		return err
	}
	if err := lower(t); !isSQLState(err, "23514") {
		t.Errorf("under a session operator `<` that answers false, moving %s's generation back answered %v; want it refused with SQLSTATE 23514", email, err)
	}
	if err := lower(t, `alter function `+schema+`.guard_user_sessions_binding_columns() reset search_path`); err != nil {
		t.Errorf("control: with the guard's setting reset, the lowering answered %v; want it to pass under the session's operator", err)
	}
}

// TestAClaimMadeBeforeAClockStepBackLetsTheNextRecordingThrough: a pass's claim stamps tried_at
// with now(). After the database's clock steps back further than the time since that claim, the
// row's stamp is later than the clock, and the guard's tried_at arm refused every later write of
// the row while it was — a settlement, the operator's clear, and the two writes
// PgPeopleStore.SignInSubject makes last, which rolled the sign-in back with SQLSTATE 23514: the
// account Google gives the address could not sign in for the length of the step, and the account
// it replaces kept its sessions and tokens for that time. The arm now refuses only a stamp the
// write itself sets or moves (20261119065002), so the sign-in goes through. The test cannot step
// the database's clock: a claim stamped a minute ahead, written with the guard off, stands for a
// claim made just before a step back of more than a minute.
//
// Control, in the test: the same sign-in over a claim stamped a minute behind, which the arm never
// refused.
func TestAClaimMadeBeforeAClockStepBackLetsTheNextRecordingThrough(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := brokerPeople(database)
	stamp := func(t *testing.T, email, at string) {
		t.Helper()
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		for _, statement := range []string{
			`alter table broker_revocations disable trigger broker_revocations_binding_columns`,
			`update broker_revocations set tried_at = now() ` + at + ` where email = '` + email + `'`,
			`alter table broker_revocations enable trigger broker_revocations_binding_columns`,
		} {
			if _, err := tx.Exec(ctx, statement); err != nil {
				t.Fatalf("%s: %v", statement, err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range []struct{ name, email, at string }{
		{"a claim stamped before a step back", "claim-ahead@example.com", "+ interval '1 minute'"},
		{"control, a claim the clock has passed", "claim-behind@example.com", "- interval '1 minute'"},
	} {
		t.Run(row.name, func(t *testing.T) {
			// account-a holds the address and account-b replaces it, so account-a's revocation is
			// owed; the claim is stamped; then account-a takes the address back, which settles its
			// own revocation and records account-b's, both writes of the claimed row.
			signInAll(t, people, row.email, "account-a", "account-b")
			stamp(t, row.email, row.at)
			if err := people.SignInSubject(ctx, row.email, "account-a", "refresh-a2", model.MemberFull); err != nil {
				t.Errorf("the sign-in taking %s back over a claim stamped now() %s answered %v; want it recorded", row.email, row.at, err)
			}
			var sub string
			if err := database.Pool.QueryRow(ctx, `select sub from people where email = $1`, row.email).Scan(&sub); err != nil || sub != "account-a" {
				t.Errorf("after the sign-in, %s records %q, %v; want account-a", row.email, sub, err)
			}
		})
	}
}
