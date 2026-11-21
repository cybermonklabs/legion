package store

import (
	"context"
	"strings"
	"testing"
)

// earliestHeadBrokerRevocations is 20261116232217 as the heads of cybermonklabs/legion#4199 from
// cf83238fe9 (2026-11-16T23:38Z) to fe1d01bd2e (2026-11-17T01:37Z) wrote it: one row per address,
// with no issuer, subject or holder_subject, which aa6bb03e14 added at 04:05Z
// (git show cf83238fe9:packages/envoy/internal/dispatch/store/migrations/20261116232217_broker_revocations.up.sql).
const earliestHeadBrokerRevocations = `
create table broker_revocations (
  email text primary key check (email <> '' and email = lower(btrim(email))),
  generation bigint not null default 1,
  recorded_at timestamptz not null default now(),
  tried_at timestamptz,
  done_at timestamptz
);
create function broker_revocations_notify() returns trigger language plpgsql as $$
begin
  perform pg_notify('dispatch_broker_revocations', '');
  return null;
end $$;
create trigger broker_revocations_notify after insert or update of generation on broker_revocations
  for each row execute function broker_revocations_notify();
`

// TestADatabaseOfTheEarliestHeadsShapeIsRefusedAtMigrate: a database one of those heads migrated
// records 20261116232217 as run and holds the table above. Every later migration used to apply
// over it with no error — 20261118152422 adds session_generation `if not exists` and creates a
// guard whose body names issuer, subject and holder_subject, which plpgsql does not resolve until
// the trigger runs — and then every sign-in that replaced an account failed with 42703 and rolled
// back, so Dispatch kept recording the replaced account and the broker was never told. Migrate now
// refuses such a database (20261119064905), naming the window and the reset, as every head before
// 8db739b89a already does at 20261118041916's drop of refuse_agent_token_account_change.
//
// Control, in the test: a database this head migrated from empty takes the same two sign-ins and
// records the replacing account.
func TestADatabaseOfTheEarliestHeadsShapeIsRefusedAtMigrate(t *testing.T) {
	ctx := context.Background()
	const email = "earliest-head@example.com"

	control := migratedEmptyStore(t)
	people := brokerPeople(control)
	signInAll(t, people, email, "account-a", "account-b")
	var sub string
	if err := control.Pool.QueryRow(ctx, `select sub from people where email = $1`, email).Scan(&sub); err != nil || sub != "account-b" {
		t.Fatalf("control: on a database this head migrated, %s records %q, %v; want account-b", email, sub, err)
	}

	database := openEmptyTestStore(t)
	migrateThrough(t, database, 20261116232216)
	if _, err := database.Pool.Exec(ctx, earliestHeadBrokerRevocations); err != nil {
		t.Fatalf("make the earliest head's broker_revocations: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `insert into schema_migrations (version) values (20261116232217)`); err != nil {
		t.Fatalf("record 20261116232217 as run: %v", err)
	}
	err := database.Migrate(ctx)
	if err == nil {
		t.Fatalf("Migrate over the earliest head's broker_revocations answered no error; want it refused, naming the reset")
	}
	for _, want := range []string{"broker_revocations holds no issuer column", "Reset this database and the secrets broker's database together"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Migrate refused with %v; want a message holding %q", err, want)
		}
	}
	// The refusal stands for the whole of Migrate: the sign-in that used to fail with 42703 never
	// runs, because a server whose Migrate refused does not start.

	// The migration alone, in a schema whose name needs quotes. Pasted together, the name folds
	// Dispatch to dispatch: Postgres then answers 3F000 (schema "dispatch" does not exist) at this
	// migration, on the earliest shape and on a correct one alike. The migration builds the name
	// with format %I. A whole Migrate cannot run in such a schema, since Dispatch runs in the schema
	// public (20261115200014_owner_lock_legacy_gate, and store.ownersLock, name public), so this runs
	// 20261119064905's own statements in a transaction whose search_path is "Dispatch" alone, over a
	// broker_revocations made there.
	refusal, err := migrationFiles.ReadFile("migrations/20261119064905_earliest_head_broker_revocations_refused.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	quoted := openEmptyTestStore(t)
	inQuotedSchema := func(t *testing.T, table string) error {
		t.Helper()
		tx, err := quoted.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		for _, statement := range []string{`create schema "Dispatch"`, `set local search_path = "Dispatch"`, table} {
			if _, err := tx.Exec(ctx, statement); err != nil {
				t.Fatalf("make broker_revocations in the schema Dispatch (%.60s): %v", statement, err)
			}
		}
		var schema string
		if err := tx.QueryRow(ctx, `select current_schema()`).Scan(&schema); err != nil || schema != "Dispatch" {
			t.Fatalf("the transaction's current schema is %q (%v), want Dispatch", schema, err)
		}
		_, err = tx.Exec(ctx, string(refusal))
		return err
	}
	if err := inQuotedSchema(t, earliestHeadBrokerRevocations); err == nil || !strings.Contains(err.Error(), "broker_revocations holds no issuer column") {
		t.Errorf("20261119064905 over the earliest head's broker_revocations in the schema Dispatch answered %v; want the same refusal as in public", err)
	}
	if err := inQuotedSchema(t, `create table broker_revocations (email text not null, issuer text not null, subject text not null)`); err != nil {
		t.Errorf("20261119064905 over a broker_revocations with an issuer column in the schema Dispatch answered %v; want it to pass", err)
	}
}
