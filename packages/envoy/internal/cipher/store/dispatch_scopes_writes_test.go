package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
)

// TestTheDatabaseRefusesAnyChangeOfACredentialsDispatchScopes: migration 0053's trigger refuses
// every update that changes the Dispatch scopes a launcher credential was minted with, with
// SQLSTATE 23514: a machine login's null made a list, a Dispatch sign-in's list made null, made
// another list, or its empty list given a scope. A write that leaves them as they were (a
// refresh's expiry, a revoke, the column restated) passes. And a record's dispatch_scopes, what a
// machine-login record is, is never updated at all (credential_requests_no_update, 0005).
//
// Control, in the test: with the trigger dropped, every one of those changes succeeds, so it is
// the trigger that refuses them.
func TestTheDatabaseRefusesAnyChangeOfACredentialsDispatchScopes(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	if _, err := st.Pool.Exec(ctx, `
		insert into launcher_credentials (id, operator, host, key_thumbprint, public_jwk, expires_at, dispatch_scopes) values
			('6f1f9d5e-3c2a-4b7e-9d1a-0b5e2a9c7d11', 'ada@example.com', 'example-host', 'thumbprint-machine', '{}', now() + interval '1 day', null),
			('6f1f9d5e-3c2a-4b7e-9d1a-0b5e2a9c7d12', 'ada@example.com', 'example-host', 'thumbprint-person', '{}', now() + interval '1 day', '{person}'),
			('6f1f9d5e-3c2a-4b7e-9d1a-0b5e2a9c7d13', 'ada@example.com', 'example-host', 'thumbprint-none', '{}', now() + interval '1 day', '{}');
		insert into credential_requests (id, body, kind, approver, code, expires_at, dispatch_scopes)
			values ('a-sign-in-record', 'body', 'launcher_credential', 'ada@example.com', 'BCDF-GHJK', now() + interval '15 minutes', '{person}');
	`); err != nil {
		t.Fatalf("seed a machine login, two Dispatch sign-ins and a sign-in's record: %v", err)
	}
	changes := []struct{ name, statement string }{
		{"a machine login given scopes", `update launcher_credentials set dispatch_scopes = '{person}' where key_thumbprint = 'thumbprint-machine'`},
		{"a machine login given the empty list", `update launcher_credentials set dispatch_scopes = '{}' where key_thumbprint = 'thumbprint-machine'`},
		{"a sign-in made a machine login", `update launcher_credentials set dispatch_scopes = null where key_thumbprint = 'thumbprint-person'`},
		{"a sign-in given another scope", `update launcher_credentials set dispatch_scopes = '{approve}' where key_thumbprint = 'thumbprint-person'`},
		{"a sign-in of no scope given one", `update launcher_credentials set dispatch_scopes = array_append(dispatch_scopes, 'person') where key_thumbprint = 'thumbprint-none'`},
		{"every row at once", `update launcher_credentials set dispatch_scopes = '{person}'`},
	}
	keeps := []struct{ name, statement string }{
		{"a refresh's expiry", `update launcher_credentials set expires_at = now() + interval '2 days' where key_thumbprint = 'thumbprint-person'`},
		{"a revoke", `update launcher_credentials set revoked_at = clock_timestamp() where key_thumbprint = 'thumbprint-none'`},
		{"the scopes restated", `update launcher_credentials set dispatch_scopes = dispatch_scopes, expires_at = expires_at`},
	}
	// attempt runs statement in a transaction of its own and rolls it back, answering its error.
	attempt := func(statement string) error {
		tx, err := st.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx)
		_, err = tx.Exec(ctx, statement)
		return err
	}
	for _, change := range changes {
		var pgError *pgconn.PgError
		if err := attempt(change.statement); !errors.As(err, &pgError) || pgError.Code != "23514" {
			t.Errorf("%s answered %v, want it refused with SQLSTATE 23514: a credential's Dispatch scopes never change", change.name, err)
		}
	}
	for _, keep := range keeps {
		if err := attempt(keep.statement); err != nil {
			t.Errorf("%s, which leaves the Dispatch scopes as they were, answered %v, want it to pass", keep.name, err)
		}
	}
	if err := attempt(`update credential_requests set dispatch_scopes = null where id = 'a-sign-in-record'`); err == nil {
		t.Error("a sign-in's record made a machine login's passed, want it refused: a record is never updated")
	}

	if _, err := st.Pool.Exec(ctx, `drop trigger launcher_credentials_dispatch_scopes_unchanged on launcher_credentials`); err != nil {
		t.Fatalf("drop the trigger for the control: %v", err)
	}
	for _, change := range changes {
		if err := attempt(change.statement); err != nil {
			t.Errorf("control: with the trigger dropped, %s answered %v, want it to pass, so the test would not see what refuses it", change.name, err)
		}
	}
}
