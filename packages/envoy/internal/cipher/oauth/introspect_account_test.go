package oauth_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/oidc"
)

// The Google account the credential behind a token naming a person records is part of what an
// introspection answers (oauthwire.Introspection's AccountIssuer and AccountSubject), read from the
// row the broker keeps for that credential, so a resource server can tell which account the token
// is. The cases below run the trades the exchange answers on the fixture's own database and read
// each token back.

// approverSubject is the `sub` of the account a machine login's approval names in these cases.
const approverSubject = "109ada-approver"

// approvedCredential inserts the launcher credential a machine login's approval for ada mints,
// recording the approving account issuer and subject, and answers its id. Both empty is an
// approval that named no account, as one an older Dispatch relayed: both columns are left null.
func (f *fixture) approvedCredential(t *testing.T, issuer, subject string) string {
	t.Helper()
	id := uuid.NewString()
	var approverIssuer, approverSubject *string
	if issuer != "" {
		approverIssuer, approverSubject = &issuer, &subject
	}
	if _, err := f.pool.Exec(context.Background(), `insert into launcher_credentials
		(id, operator, host, key_thumbprint, public_jwk, expires_at, approver_issuer, approver_subject)
		values ($1, $2, 'example-host', $3, '{}'::jsonb, now() + interval '30 days', $4, $5)`,
		id, person, "thumbprint-"+id, approverIssuer, approverSubject); err != nil {
		t.Fatalf("insert the approved launcher credential: %v", err)
	}
	return id
}

// TestIntrospectionAnswersTheAccountTheTokensCredentialRecords: a token a person's sign-in on a
// machine minted answers the account of the person token that made the trade, and a dev box
// session's token, exchanged from its machine login's own, the account that approved the login,
// each as the trade wrote it onto the token's own session. A machine's token naming its service
// names no person and so no account, and neither does a dev box token whose machine login records
// none. A token minted before the broker recorded the account on the token records none, and
// answers none even when its sign-in's row records an account: introspection reads no row but the
// token's, so a row another account's trade wrote never names the token. Each token is first
// required live and naming whom it names, so an answer that names no account because it names
// nothing at all cannot pass.
//
// Control: answer the account from the sign-in's row, as the previous head did (the token from
// before the broker recorded it answers the row's account).
func TestIntrospectionAnswersTheAccountTheTokensCredentialRecords(t *testing.T) {
	f := newSignInFixture(t)
	signIn := func(t *testing.T, session string) (token, jkt string) {
		t.Helper()
		key := newKey(t)
		token = f.exchanged(t, signInForm(
			f.personToken(t, person, deviceAudience, true, signInDomain),
			f.machineToken(t, signInMachine, machineAudience, true),
			session), key)
		return token, thumbprint(t, key)
	}
	devBox := func(t *testing.T, credential, session string) string {
		t.Helper()
		key := newKey(t)
		return f.exchanged(t, exchangeForm(f.devBoxTokenOf(t, key, person, credential), oauthwire.SubjectTokenTypeAccessToken, session), key)
	}
	for _, c := range []struct {
		name            string
		token           func(t *testing.T) string
		person, service string
		issuer, subject string
		proves          string
	}{
		{
			name: "a person's sign-in on a machine",
			token: func(t *testing.T) string {
				token, jkt := signIn(t, "ada-box-1")
				recordSignIn(t, f, person, jkt, signInMachine)
				return token
			},
			person: person, issuer: f.googleIssuer.URL(), subject: "109" + person,
			proves: "the account of the person token that made the sign-in",
		},
		{
			name: "a dev box session's token, exchanged from its machine login's",
			token: func(t *testing.T) string {
				return devBox(t, f.approvedCredential(t, oidc.GoogleIssuer, approverSubject), "ada-devbox")
			},
			person: person, issuer: oidc.GoogleIssuer, subject: approverSubject,
			proves: "the account that approved the machine login",
		},
		{
			name:    "a machine's token naming its service",
			token:   func(t *testing.T) string { return f.exchanged(t, machineForm(t, f, "fleet-session"), newKey(t)) },
			service: signInFleet,
			proves:  "no account: the token names no person",
		},
		{
			name: "a token minted before the broker recorded its account, whose sign-in row another account's trade wrote",
			token: func(t *testing.T) string {
				token, jkt := signIn(t, "ada-box-2")
				writeWithoutTheAccountGuard(t, f.pool, `update oauth_access_tokens
					set request = request #- '{session,extra,account_issuer}' #- '{session,extra,account_subject}'
					where request #>> '{session,extra,person}' = $1 and request #>> '{session,jwk_thumbprint}' = $2`, person, jkt)
				if _, err := f.pool.Exec(context.Background(), `insert into person_signins (person, thumbprint, machine_account, issuer, subject)
					values ($1, $2, $3, $4, '109another-account')`, person, jkt, signInMachine, f.googleIssuer.URL()); err != nil {
					t.Fatalf("insert the sign-in row another account's trade wrote: %v", err)
				}
				return token
			},
			person: person,
			proves: "no account: the token records none, and no row but its own is read",
		},
		{
			name:   "a dev box session's token whose machine login records no approving account",
			token:  func(t *testing.T) string { return devBox(t, f.approvedCredential(t, "", ""), "ada-devbox-older") },
			person: person,
			proves: "no account: the machine login's row records none",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			answer, err := f.provider.Introspect(context.Background(), c.token(t))
			if err != nil {
				t.Fatalf("Introspect: %v", err)
			}
			if !answer.Active || answer.Person != c.person || answer.Service != c.service {
				t.Fatalf("the token introspects active=%t person=%q service=%q, want a live token naming person %q and service %q",
					answer.Active, answer.Person, answer.Service, c.person, c.service)
			}
			if answer.AccountIssuer != c.issuer || answer.AccountSubject != c.subject {
				t.Errorf("introspection answered account_issuer %q and account_subject %q, want %q and %q: it must name %s",
					answer.AccountIssuer, answer.AccountSubject, c.issuer, c.subject, c.proves)
			}
		})
	}
}

// TestATokenRecordsWhenItsCredentialWasMade: a token naming a person records, when it is minted,
// when the credential behind it was made at the broker, from the broker database's clock, and
// introspection answers it (credential_created_at): a sign-in's token, the sign-in row's
// created_at, which here is three days older than the token, since a later trade under one sign-in
// mints tokens long after it was made; a dev box token, its machine login's created_at. A
// machine's token naming its service records none.
//
// Control: personSignIn not recording it (its recordCredentialCreatedAt call removed); the
// sign-in's token answers 0.
func TestATokenRecordsWhenItsCredentialWasMade(t *testing.T) {
	f := newSignInFixture(t)
	ctx := context.Background()
	key := newKey(t)
	var signInMade int64
	if err := f.pool.QueryRow(ctx, `insert into person_signins (person, thumbprint, machine_account, issuer, subject, created_at, last_used_at)
		values ($1, $2, $3, $4, $5, now() - interval '3 days', now() - interval '3 days') returning floor(extract(epoch from created_at))::bigint`,
		person, thumbprint(t, key), signInMachine, oidc.CanonicalIssuer(f.googleIssuer.URL()), "109"+person).Scan(&signInMade); err != nil {
		t.Fatalf("the sign-in this box key made three days ago: %v", err)
	}
	signInToken := f.exchanged(t, signInForm(
		f.personToken(t, person, deviceAudience, true, signInDomain),
		f.machineToken(t, signInMachine, machineAudience, true),
		"ada-box-made"), key)
	credential := f.approvedCredential(t, oidc.GoogleIssuer, approverSubject)
	var loginMade int64
	if err := f.pool.QueryRow(ctx, `select floor(extract(epoch from created_at))::bigint from launcher_credentials where id = $1::uuid`, credential).Scan(&loginMade); err != nil {
		t.Fatalf("read the machine login's creation time: %v", err)
	}
	devKey := newKey(t)
	devToken := f.exchanged(t, exchangeForm(f.devBoxTokenOf(t, devKey, person, credential), oauthwire.SubjectTokenTypeAccessToken, "ada-devbox-made"), devKey)
	for _, c := range []struct {
		name, token string
		want        int64
	}{
		{"a sign-in's token", signInToken, signInMade},
		{"a dev box token", devToken, loginMade},
		{"a machine's token naming its service", f.exchanged(t, machineForm(t, f, "fleet-made"), newKey(t)), 0},
	} {
		answer, err := f.provider.Introspect(ctx, c.token)
		if err != nil || !answer.Active {
			t.Fatalf("%s introspects active=%t, %v; want live", c.name, answer.Active, err)
		}
		if answer.CredentialCreatedAt != c.want {
			t.Errorf("%s answers credential_created_at %d, want %d", c.name, answer.CredentialCreatedAt, c.want)
		}
	}
}

// writeWithoutTheAccountGuard runs statement with migration 0048's trigger on oauth_access_tokens
// turned off for its own transaction alone: it stands in for a token row written before the
// broker recorded the account on its tokens, whose shape the trigger, which refuses only a later
// change of a recorded account, never lets any statement make now.
func writeWithoutTheAccountGuard(t *testing.T, pool *pgxpool.Pool, statement string, args ...any) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	for _, step := range []struct {
		sql  string
		args []any
	}{
		{`alter table oauth_access_tokens disable trigger oauth_access_tokens_account_unchanged`, nil},
		{statement, args},
		{`alter table oauth_access_tokens enable trigger oauth_access_tokens_account_unchanged`, nil},
	} {
		if _, err := tx.Exec(ctx, step.sql, step.args...); err != nil {
			t.Fatalf("%s: %v", step.sql, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestAnExchangedTokenRecordsOnlyTheAccountItsOwnTradeNames: a token exchanged from a machine
// login's own is minted on a session the library reads back out of that subject token's row, so
// whatever account claims that row carried would ride into the new token unless the exchange
// drops them (decide) before it writes the account its own trade names (approverAccount). The
// subject token here carries an account no trade wrote, and the login it follows records no
// approving account: the exchanged token answers no account, never the one carried over.
//
// Control: decide keeps the claims it read back (the delete removed); the token answers the
// carried account.
func TestAnExchangedTokenRecordsOnlyTheAccountItsOwnTradeNames(t *testing.T) {
	f := newSignInFixture(t)
	key := newKey(t)
	credential := f.approvedCredential(t, "", "")
	subjectToken := f.devBoxTokenOf(t, key, person, credential)
	writeWithoutTheAccountGuard(t, f.pool, `update oauth_access_tokens
		set request = jsonb_set(jsonb_set(request, '{session,extra,account_issuer}', '"https://carried.example"'),
			'{session,extra,account_subject}', '"109carried"')
		where request #>> '{session,extra,credential_id}' = $1`, credential)
	exchanged := f.exchanged(t, exchangeForm(subjectToken, oauthwire.SubjectTokenTypeAccessToken, "ada-devbox-carried"), key)
	answer, err := f.provider.Introspect(context.Background(), exchanged)
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	if !answer.Active || answer.Person != person {
		t.Fatalf("the exchanged token introspects active=%t person=%q, want a live token naming %s", answer.Active, answer.Person, person)
	}
	if answer.AccountIssuer != "" || answer.AccountSubject != "" {
		t.Errorf("the exchanged token answers account_issuer %q and account_subject %q, which its subject token carried and its own trade does not name; want none",
			answer.AccountIssuer, answer.AccountSubject)
	}
}
