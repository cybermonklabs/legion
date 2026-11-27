package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
)

// TestTheDatabaseRefusesAnyChangeOfARecordedAccount: migration 0048's triggers refuse every update
// that changes the account a credential recorded when it was made, whatever statement sends it,
// with SQLSTATE 23514. The writes below are the shapes a scan of source text missed or could miss:
// a plain update, an aliased one, `update only`, one built from two strings, a row-constructor
// assignment, a migration-style backfill of a row that recorded none, an insert's conflict update,
// a machine login's approver, an approval-page session's account, and a jsonb_set of the account a
// token's trade recorded in its stored request, of an access token and of a refresh token. A write
// that leaves the account as it was (a sign-in's revoke, a machine login's expiry, a token's revoke
// and a change of another part of its request) passes.
//
// Control, in the test: with the triggers dropped, every one of those writes succeeds, so it is the
// triggers that refuse them.
func TestTheDatabaseRefusesAnyChangeOfARecordedAccount(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	if _, err := st.Pool.Exec(ctx, `
		insert into person_signins (person, thumbprint, machine_account, issuer, subject) values
			('ada@example.com', 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA', 'fleet@example.iam.gserviceaccount.com', 'https://accounts.google.com', '109ada'),
			('ada@example.com', 'BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB', 'fleet@example.iam.gserviceaccount.com', null, null);
		insert into launcher_credentials (id, operator, host, key_thumbprint, public_jwk, expires_at, approver_issuer, approver_subject)
			values ('6f1f9d5e-3c2a-4b7e-9d1a-0b5e2a9c7d11', 'ada@example.com', 'example-host', 'thumbprint-approved', '{}', now() + interval '1 day',
				'https://accounts.google.com', '109ada');
		insert into browser_sessions (secret_hash, login, issuer, subject, expires_at)
			values (sha256('a session'), 'ada@example.com', 'https://accounts.google.com', '109ada', now() + interval '1 day');
		insert into oauth_access_tokens (signature, request_id, request, expires_at) values
			('access-1', 'r1', '{"client_id":"c","session":{"extra":{"person":"ada@example.com","account_issuer":"https://accounts.google.com","account_subject":"109ada"}}}', now() + interval '1 hour');
		insert into oauth_refresh_tokens (signature, request_id, request, expires_at) values
			('refresh-1', 'r1', '{"client_id":"c","session":{"extra":{"person":"ada@example.com","account_issuer":"https://accounts.google.com","account_subject":"109ada","credential_created_at":1794900000}}}', now() + interval '1 day');
	`); err != nil {
		t.Fatalf("seed one credential of each kind: %v", err)
	}
	changes := []struct{ name, statement string }{
		{"a plain update", `update person_signins set subject = '209other' where thumbprint = 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'`},
		{"an aliased update", `update person_signins as p set subject = '209other' where p.thumbprint = 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'`},
		{"update only", `update only person_signins set subject = '209other' where thumbprint = 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'`},
		{"one built from two strings", "update person_signins set last_used_at = now(), " + "subject = '209other' where thumbprint = 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'"},
		{"a row-constructor assignment", `update person_signins set (issuer, subject) = ('https://issuer.example', '109ada') where thumbprint = 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'`},
		{"a backfill of a row recording none", `update person_signins set issuer = 'https://accounts.google.com', subject = 'someone' where subject is null`},
		{"an insert's conflict update", `insert into person_signins (person, thumbprint, machine_account, issuer, subject)
			values ('ada@example.com', 'BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB', 'fleet@example.iam.gserviceaccount.com', 'https://accounts.google.com', '209other')
			on conflict (person, thumbprint) do update set issuer = excluded.issuer, subject = excluded.subject`},
		{"a machine login's approver", `update launcher_credentials set approver_subject = '209other' where key_thumbprint = 'thumbprint-approved'`},
		{"an approval-page session's account", `update browser_sessions set subject = '209other' where login = 'ada@example.com'`},
		{"an access token's recorded account", `update oauth_access_tokens set request = jsonb_set(request, '{session,extra,account_subject}', '"209other"') where signature = 'access-1'`},
		{"an access token's account taken off", `update oauth_access_tokens set request = request #- '{session,extra,account_issuer}' #- '{session,extra,account_subject}' where signature = 'access-1'`},
		{"a refresh token's recorded account", `update oauth_refresh_tokens set request = jsonb_set(request, '{session,extra,account_subject}', '"209other"') where signature = 'refresh-1'`},
		{"a sign-in's creation time", `update person_signins set created_at = now() + interval '1 day' where thumbprint = 'BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB'`},
		{"a machine login's creation time", `update launcher_credentials set created_at = now() + interval '1 day' where key_thumbprint = 'thumbprint-approved'`},
		{"an approval-page session's creation time", `update browser_sessions set created_at = now() + interval '1 day' where login = 'ada@example.com'`},
		{"an access token's recorded creation time", `update oauth_access_tokens set request = jsonb_set(request, '{session,extra,credential_created_at}', '9999999999') where signature = 'access-1'`},
		{"a refresh token's recorded creation time taken off", `update oauth_refresh_tokens set request = request #- '{session,extra,credential_created_at}' where signature = 'refresh-1'`},
	}
	keeps := []struct{ name, statement string }{
		{"a sign-in's revoke", `update person_signins set revoked_at = clock_timestamp() where thumbprint = 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'`},
		{"a sign-in's last use, its account restated", `update person_signins set last_used_at = now(), subject = subject, issuer = issuer`},
		{"a machine login's expiry", `update launcher_credentials set expires_at = now() + interval '2 days' where key_thumbprint = 'thumbprint-approved'`},
		{"a token's revoke", `update oauth_access_tokens set active = false where signature = 'access-1'`},
		{"another part of a token's request", `update oauth_access_tokens set request = jsonb_set(request, '{client_id}', '"another-client"') where signature = 'access-1'`},
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
			t.Errorf("%s answered %v, want it refused with SQLSTATE 23514: a recorded account never changes", change.name, err)
		}
	}
	for _, keep := range keeps {
		if err := attempt(keep.statement); err != nil {
			t.Errorf("%s, which leaves the recorded account as it was, answered %v, want it to pass", keep.name, err)
		}
	}

	if _, err := st.Pool.Exec(ctx, `
		drop trigger person_signins_account_unchanged on person_signins;
		drop trigger launcher_credentials_approver_unchanged on launcher_credentials;
		drop trigger browser_sessions_account_unchanged on browser_sessions;
		drop trigger oauth_access_tokens_account_unchanged on oauth_access_tokens;
		drop trigger oauth_refresh_tokens_account_unchanged on oauth_refresh_tokens;
	`); err != nil {
		t.Fatalf("drop the triggers for the control: %v", err)
	}
	for _, change := range changes {
		if err := attempt(change.statement); err != nil {
			t.Errorf("control: with the triggers dropped, %s answered %v, want it to pass, so the test would not see what refuses it", change.name, err)
		}
	}
}
