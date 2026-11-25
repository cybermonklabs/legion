package enroll

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/store"
)

// TestATradeUnderASignInRecordingNoAccountEndsItsCredentialAndSessions: a sign-in from before
// migration 0045 records no account, so the sessions enrolled under its launcher credential might
// be any account's. A trade under its key ends that sign-in (store.RecordPersonSignIn) and, in
// the same transaction, the credential it held and every session enrolled under it, the grant
// revoked and the pending request cancelled, as revoking the sign-in does, each naming
// NoAccountSignInActor; the trade's own sign-in records its account. Left live, those sessions
// would go on reading on that box after a revoke of the account that enrolled them, which no
// longer finds a row that recorded it or none.
//
// Control: adopt the row in place, as the previous head did (the credential and the session stay
// live).
func TestATradeUnderASignInRecordingNoAccountEndsItsCredentialAndSessions(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	const person = "ada@example.com"
	cred := mintCredential(t, svc, str(person), str("example-fleet"), "signin-box")
	insertSignIn(t, svc, cred, person)
	enr, err := svc.Create(ctx, cred, Enrollment{Kind: "session", RuntimeID: "before-the-account",
		Thumbprint: "tp-before-the-account", SessionID: str("before-the-account")})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	insertLiveGrant(t, svc, enr.ID)
	pending := insertPendingRequest(t, svc, enr.ID)
	var thumbprint string
	if err := svc.Store.Pool.QueryRow(ctx, `select key_thumbprint from launcher_credentials where id = $1`, cred.ID).Scan(&thumbprint); err != nil {
		t.Fatalf("read the credential's key: %v", err)
	}
	// The subject a sign-in's own trades mint its credential under (oauth.Provider.personCredential).
	if _, err := svc.Store.Pool.Exec(ctx, `update launcher_credentials set subject = $1 where id = $2`,
		exchange.PersonSubject(person, "fleet@example.iam.gserviceaccount.com", thumbprint), cred.ID); err != nil {
		t.Fatalf("key the credential by the sign-in's subject: %v", err)
	}
	account := store.IssuerSubject{Issuer: raceIssuer, Subject: "109" + person}
	tx, err := svc.Store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := RecordPersonSignIn(ctx, tx, store.SignInTrade{
		Person: person, Thumbprint: thumbprint, MachineAccount: "fleet@example.iam.gserviceaccount.com", Account: account,
	}); err != nil {
		t.Fatalf("the trade under the sign-in: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	var credentialRevoked bool
	var subject string
	if err := svc.Store.Pool.QueryRow(ctx, `select
		(select revoked_at is not null from launcher_credentials where id = $1),
		coalesce((select subject from person_signins where person = $2 and thumbprint = $3 and revoked_at is null), '')`,
		cred.ID, person, thumbprint).Scan(&credentialRevoked, &subject); err != nil {
		t.Fatalf("read the credential and the sign-in: %v", err)
	}
	if !credentialRevoked {
		t.Error("the launcher credential of the sign-in that recorded no account is live after the trade ended that sign-in")
	}
	if subject != account.Subject {
		t.Errorf("the key's live sign-in records %q, want the trade's account %s", subject, account.Subject)
	}
	if got := readCarryState(t, svc, enr.ID); !got.Ended || got.Dormant || got.LiveGrants != 0 || got.Pending != 0 {
		t.Errorf("the session enrolled under that credential is %+v after the trade, want it ended, never dormant, holding nothing", got)
	}
	if state, by, _, _ := requestState(t, svc, pending); state != "cancelled" || by != NoAccountSignInActor {
		t.Errorf("its pending request is %s by %s, want cancelled by %s", state, by, NoAccountSignInActor)
	}
}

// TestATradeUnderASignInRecordingNoAccountLeavesAnotherCredentialOfItsKey: the credential a trade
// ends with a sign-in recording no account is that sign-in's own, the one its trades minted under
// the subject exchange.PersonSubject names, found by key and subject. A live credential holding
// the same key under another subject (a machine login that came to hold the key once the
// sign-in's own credential ended, since the database lets one live credential hold a key) is not
// the sign-in's, and the trade leaves it and its sessions alone.
//
// Control: the credential found by key alone, as round 11 did; the machine login is revoked.
func TestATradeUnderASignInRecordingNoAccountLeavesAnotherCredentialOfItsKey(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	const person = "ada@example.com"
	other := mintCredential(t, svc, str(person), str("example-fleet"), "machine-login-box")
	insertSignIn(t, svc, other, person)
	if _, err := svc.Store.Pool.Exec(ctx, `update launcher_credentials set subject = 'machine-login:ada-box' where id = $1`, other.ID); err != nil {
		t.Fatalf("give the machine login its own subject: %v", err)
	}
	enr, err := svc.Create(ctx, other, Enrollment{Kind: "session", RuntimeID: "the-machine-logins-session",
		Thumbprint: "tp-the-machine-logins-session", SessionID: str("the-machine-logins-session")})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	var thumbprint string
	if err := svc.Store.Pool.QueryRow(ctx, `select key_thumbprint from launcher_credentials where id = $1`, other.ID).Scan(&thumbprint); err != nil {
		t.Fatalf("read the credential's key: %v", err)
	}
	tx, err := svc.Store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := RecordPersonSignIn(ctx, tx, store.SignInTrade{
		Person: person, Thumbprint: thumbprint, MachineAccount: "fleet@example.iam.gserviceaccount.com",
		Account: store.IssuerSubject{Issuer: raceIssuer, Subject: "109" + person},
	}); err != nil {
		t.Fatalf("the trade under the sign-in: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	var revoked bool
	if err := svc.Store.Pool.QueryRow(ctx, `select revoked_at is not null from launcher_credentials where id = $1`, other.ID).Scan(&revoked); err != nil {
		t.Fatalf("read the machine login: %v", err)
	}
	if revoked {
		t.Error("the trade under a sign-in recording no account revoked a machine login of another subject that holds the key, want it left alone")
	}
	if got := readCarryState(t, svc, enr.ID); got.Ended {
		t.Errorf("the machine login's session is %+v after the trade, want it left as it was", got)
	}
}
