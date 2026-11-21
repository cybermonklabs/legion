package store

import (
	"context"
	"testing"
)

// TestTheSearchPathMigrationsQuoteARequiredSchema runs the four migrations whose DO blocks name
// current_schema() in a schema called Dispatch. Main's owner lock keeps a full Dispatch database in
// public, but these branch migrations still have to quote the schema they are handed: their table
// and function names are made with %I, so a schema needing quotes is not folded to dispatch.
func TestTheSearchPathMigrationsQuoteARequiredSchema(t *testing.T) {
	ctx := context.Background()
	database := openEmptyTestStore(t)
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for _, statement := range []string{
		`create schema "Dispatch"`,
		`set local search_path = "Dispatch"`,
		`create table people (email text primary key, sub text)`,
		`create table user_sessions (login text primary key, generation bigint not null)`,
		`create table agent_tokens (id text primary key, subject text)`,
		`create table broker_revocations (email text, issuer text, subject text, holder_subject text, generation bigint, session_generation bigint, recorded_at timestamptz, tried_at timestamptz, done_at timestamptz)`,
		`create function guard_people_binding_columns() returns trigger language plpgsql as $$ begin return new; end $$`,
		`create function guard_broker_revocation_columns() returns trigger language plpgsql as $$ begin return new; end $$`,
		`create function end_people_sessions_on_new_account() returns trigger language plpgsql as $$ begin return new; end $$`,
		`create function guard_user_sessions_binding_columns() returns trigger language plpgsql as $$ begin return new; end $$`,
		`create function refuse_agent_token_binding_column_change() returns trigger language plpgsql as $$ begin return new; end $$`,
		`create function keep_people_recorded_account() returns trigger language plpgsql as $$ begin return new; end $$`,
		`create function broker_revocations_notify() returns trigger language plpgsql as $$ begin return new; end $$`,
	} {
		if _, err := tx.Exec(ctx, statement); err != nil {
			t.Fatalf("make the quoted-schema migration fixture, %q: %v", statement, err)
		}
	}
	for _, file := range []string{
		"20261119021826_clock_guards_search_path.up.sql",
		"20261119060414_session_generation_floors.up.sql",
		"20261119065002_binding_guards_search_path.up.sql",
		"20261120144819_deployment_resets.up.sql",
	} {
		sql, err := migrationFiles.ReadFile("migrations/" + file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("run %s in schema Dispatch: %v", file, err)
		}
	}
	var schema string
	if err := tx.QueryRow(ctx, `select current_schema()`).Scan(&schema); err != nil || schema != "Dispatch" {
		t.Fatalf("the migration transaction's current schema is %q (%v), want Dispatch", schema, err)
	}
	wantNames := []string{
		"broker_revocations_notify",
		"end_people_sessions_on_new_account",
		"guard_broker_revocation_columns",
		"guard_deployment_resets",
		"guard_people_binding_columns",
		"guard_session_generation_floors",
		"guard_user_sessions_binding_columns",
		"keep_people_recorded_account",
		"refuse_agent_token_binding_column_change",
		"start_session_generation_at_floor",
	}
	rows, err := tx.Query(ctx, `select proname, coalesce(array_to_string(proconfig, ','), '') from pg_proc where pronamespace = format('%I', current_schema())::regnamespace and proname = any($1) order by proname`, wantNames)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for rows.Next() {
		var name, config string
		if err := rows.Scan(&name, &config); err != nil {
			t.Fatal(err)
		}
		got[name] = config
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(wantNames) {
		t.Fatalf("the quoted-schema migrations pinned %v, want %v", got, wantNames)
	}
	for _, name := range wantNames {
		if config := got[name]; config != `search_path=pg_catalog, "Dispatch", pg_temp` {
			t.Errorf("%s in schema Dispatch runs with %q, want quoted schema Dispatch in its search_path", name, config)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}
