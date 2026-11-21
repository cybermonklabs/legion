package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/agentsecrets"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// browserSessions is the rig's header identity with each caller a browser's session cookie of the
// address's current session generation, as a person signed in at their browser is to the reads
// that bind a credential to its account (mintCookie, approverStanding): a passkey's sign-in is a
// cookie, and only a cookie's caller is bound to the account people records.
type browserSessions struct {
	identity.HeaderIdentity
	sessions *store.PgSessionStore
}

func (b browserSessions) SessionCookie(r *http.Request) (string, int64, bool) {
	login, err := b.Login(r)
	if err != nil {
		return "", 0, false
	}
	generation, _, err := b.sessions.CurrentSessionGeneration(r.Context(), login)
	return login, generation, err == nil
}

// passkeyOutsideOrigin is the outside origin of these rows: the issuer Dispatch records a passkey's
// subject under (people.sub_issuer), and the passkey store's.
const passkeyOutsideOrigin = "https://outside.example.com"

// withPasskeys turns passkey sign-in on at passkeyOutsideOrigin, beside the restricted rig's
// Google sign-in, whose issuer the deployment's sign-in records accounts of.
func withPasskeys(options *testServerOptions) {
	options.passkeyIssuer = passkeyOutsideOrigin
	options.signInIssuer = "https://accounts.google.com"
}

// recordPasskeyAccount writes erin's people row as her first passkey enrollment leaves it: the
// subject the enrollment made, under the outside origin, and her sign-in standing.
func recordPasskeyAccount(t *testing.T, database *store.Store, email, subject string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `update people set sub = $2, sub_issuer = $3, confirmed_at = now(), signed_in_at = now()
		where email = $1`, email, subject, passkeyOutsideOrigin); err != nil {
		t.Fatalf("record %s's passkey account: %v", email, err)
	}
}

// TestPasskeyApprovalRelaysTheOutsideOriginAsTheApproversIssuer (H7, Dispatch's half): a restricted
// member whose account is a passkey's, its subject under the outside origin, approves their own
// machine login at their browser, and Dispatch relays the approving account to the secrets broker
// as that pair: the broker's minted login records the outside origin and the passkey's subject,
// never Google's issuer. The broker's half, that it compares the pair it recorded, is the broker's
// TestPasskeyApproverIssuerIsRecordedAndComparedAtTheBroker.
//
// Control: relay every account under the sign-in issuer (approve ignoring
// credentialAccount.issuer), and the login records Google's issuer and the row fails naming it.
func TestPasskeyApprovalRelaysTheOutsideOriginAsTheApproversIssuer(t *testing.T) {
	r := newRestrictedTokenRig(t, withPasskeys)
	r.server.deps.Identity = browserSessions{HeaderIdentity: r.server.deps.Identity.(identity.HeaderIdentity), sessions: store.NewPgSessionStore(r.database.Pool)}
	const subject = "cGFzc2tleS1zdWJqZWN0IQ"
	recordPasskeyAccount(t, r.database, accessErin, subject)
	_, _, credential := r.approvedLogin(t, accessErin, "erin-laptop")
	var issuer, approver string
	if err := r.broker.Store.Pool.QueryRow(t.Context(), `select coalesce(approver_issuer, ''), coalesce(approver_subject, '')
		from launcher_credentials where id = $1::uuid`, credential).Scan(&issuer, &approver); err != nil {
		t.Fatalf("read the approving account of erin's login: %v", err)
	}
	if issuer != passkeyOutsideOrigin || approver != subject {
		t.Fatalf("erin's machine login records the approving account (%q, %q), want her passkey's (%q, %q)", issuer, approver, passkeyOutsideOrigin, subject)
	}
}

// TestAResetRevokesTheMembersDeviceLogins (R5, R16): the owner's Reset of a restricted member ends
// their sign-in, deletes their passkeys, and records in the same transaction that Dispatch owes the
// secrets broker the revocation of every machine login they approved, as a removal from the list
// does; the broker revocation loop's next pass pays it, so the device login a hijacker approved
// with the person's passkey ends too: its agent's token introspects not active, and its refresh is
// refused. The person stays on ACC's list.
//
// Control: owe nothing on a Reset (resetProjectAccessPerson not calling
// recordMachineLoginRevocation), and the row fails naming the revocation not owed.
func TestAResetRevokesTheMembersDeviceLogins(t *testing.T) {
	r := newRestrictedTokenRig(t, withPasskeys)
	ctx := t.Context()
	recordPasskeyAccount(t, r.database, accessErin, "cmVzZXQtc3ViamVjdC0xIQ")
	erins, erinsDevice, _ := r.approvedLogin(t, accessErin, "erin-laptop")
	erinsToken := r.sessionToken(t, erins, erinsDevice, "erin-agent", "ACC")

	reset := dispatchRequest(t, r.dispatch, http.MethodPost, "/api/v1/projects/ACC/access/people/"+accessErin+"/reset", nil, "alice")
	if reset.Code != http.StatusOK {
		t.Fatalf("alice's Reset of erin: %d %s, want 200", reset.Code, reset.Body.String())
	}
	if people := decodeBody[struct {
		People []string `json:"people"`
	}](t, reset).People; len(people) != 1 || people[0] != accessErin {
		t.Fatalf("ACC's list after the Reset is %v, want erin still on it", people)
	}
	if owed, _, err := store.NewMachineLoginRevocations(r.database.Pool).Owing(ctx, accessErin); err != nil || !owed {
		t.Fatalf("the revocation of erin's machine logins is owed %t (%v) after the Reset, want owed", owed, err)
	}
	var confirmed bool
	if err := r.database.Pool.QueryRow(ctx, `select confirmed_at is not null from people where email = $1`, accessErin).Scan(&confirmed); err != nil || confirmed {
		t.Fatalf("erin's sign-in stands %t (%v) after the Reset, want it ended", confirmed, err)
	}
	r.server.revokeBrokerDebts(ctx)
	if r.introspect(t, erinsToken).Active {
		t.Errorf("erin's agent's token introspects active after the Reset, want it ended")
	}
	if refreshed, err := r.client.Refresh(t, erins.key, erinsDevice); err == nil {
		t.Errorf("erin's device login refreshed after the Reset, answering a token expiring %s; want it refused", refreshed.Expiry)
	}
}

// TestAnApprovalInFlightAcrossAResetMintsNoLogin (R5, R16): an approve whose standing Dispatch
// read before the owner's Reset of the approver (approveCredentialRecord: the account and the
// session generation it relays), and which reaches the secrets broker only after the Reset
// committed and the broker revocation loop's pass ran, mints nothing. The Reset gave the address
// a new passkey account and owes the broker the revocation of the old one at the generation the
// Reset left, so the broker refuses an approve that names the old account, or an older
// generation, and erin's machine never gets a token for that login.
//
// Control: the Reset keeps the address's account (store.PgPasskeys.ResetTx recording no new
// subject), and the late approve mints the login and the row fails naming its credential.
func TestAnApprovalInFlightAcrossAResetMintsNoLogin(t *testing.T) {
	r := newRestrictedTokenRig(t, withPasskeys)
	ctx := t.Context()
	const subject = "aW4tZmxpZ2h0LXN1YmplY3Qh"
	recordPasskeyAccount(t, r.database, accessErin, subject)
	machine := r.startLogin(t, accessErin, "erin-laptop")
	recordID, _ := r.recordOf(t, accessErin, machine)
	var generation int64
	if err := r.database.Pool.QueryRow(ctx, `select coalesce((select generation from user_sessions where login = $1), 0)`, accessErin).Scan(&generation); err != nil {
		t.Fatalf("read erin's session generation: %v", err)
	}
	inFlight := agentsecrets.Decision{
		Approver: accessErin, Code: &machine.device.UserCode,
		ApproverIssuer: passkeyOutsideOrigin, ApproverSubject: subject, ApproverSessionGeneration: &generation,
	}

	if reset := dispatchRequest(t, r.dispatch, http.MethodPost, "/api/v1/projects/ACC/access/people/"+accessErin+"/reset", nil, "alice"); reset.Code != http.StatusOK {
		t.Fatalf("alice's Reset of erin: %d %s, want 200", reset.Code, reset.Body.String())
	}
	r.server.revokeBrokerDebts(ctx)
	if minted, err := r.server.deps.AgentSecrets.Approve(ctx, recordID, inFlight); err == nil {
		t.Fatalf("the approve Dispatch read before the Reset, relayed after the revocation pass, minted erin's login: %s", minted)
	}
	if token, err := r.client.DeviceToken(t, machine.key, machine.device); err == nil {
		t.Fatalf("erin's machine got a token for the login approved across the Reset, expiring %s", token.Expiry)
	}
}

// TestTwoRemovalsFromTheLastTwoListsEndThePasskeySignIn (R5): two owners take a restricted member
// off the last two lists that name them at the same moment, on a deployment with no broker, and
// the removal that commits second ends the sign-in and deletes the passkeys: each removal reads
// whether a list still names the person only under the address's lock
// (endUnlistedPasskeySignIn), so the second reads the first's committed removal.
//
// Control: read the lists before taking the address's lock (endUnlistedPasskeySignIn without
// store.LockAddressTx), and each removal reads the other's row as still there, both skip the end,
// and the row fails naming the passkey still stored.
func TestTwoRemovalsFromTheLastTwoListsEndThePasskeySignIn(t *testing.T) {
	served, database, s := newRegisteredAccessServer(t, testServerOptions{passkeyIssuer: passkeyOutsideOrigin, signInIssuer: "https://accounts.google.com"})
	ctx := t.Context()
	if created := dispatchRequest(t, served, http.MethodPost, "/api/v1/projects", map[string]string{"key": "OTHER", "name": "Other project"}, "alice"); created.Code != http.StatusCreated {
		t.Fatalf("create OTHER: %d %s", created.Code, created.Body.String())
	}
	for _, key := range []string{"ACC", "OTHER"} {
		if listed := dispatchRequest(t, served, http.MethodPut, "/api/v1/projects/"+key+"/access/people/"+accessErin, nil, "alice"); listed.Code != http.StatusOK {
			t.Fatalf("alice names erin on %s: %d %s", key, listed.Code, listed.Body.String())
		}
	}
	const subject = "dHdvLXJlbW92YWxzLWVyaW4h"
	recordPasskeyAccount(t, database, accessErin, subject)
	if _, err := database.Pool.Exec(ctx, `insert into passkeys (credential_id, email, user_handle, public_key, aaguid, flags)
		values ('\x01'::bytea, $1, convert_to($2, 'UTF8'), '\x02'::bytea, ''::bytea, 0)`, accessErin, subject); err != nil {
		t.Fatalf("store erin's passkey: %v", err)
	}

	begin := func() pgx.Tx {
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
		return tx
	}
	first, second := begin(), begin()
	for tx, key := range map[pgx.Tx]string{first: "ACC", second: "OTHER"} {
		if _, err := tx.Exec(ctx, `delete from project_access where project_key = $1 and principal_kind = 'person' and principal = $2`, key, accessErin); err != nil {
			t.Fatalf("take erin off %s: %v", key, err)
		}
	}
	if err := s.endUnlistedPasskeySignIn(ctx, first, "ACC", accessErin); err != nil {
		t.Fatalf("the removal from ACC: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- s.endUnlistedPasskeySignIn(ctx, second, "OTHER", accessErin) }()
	// The removal from OTHER either reads the lists before ACC's commits (no lock) or waits for
	// ACC's address lock; ACC's commits once one of the two is seen.
	var secondErr error
	finished := false
	for deadline := time.Now().Add(time.Minute); !finished; {
		select {
		case secondErr = <-done:
			finished = true
			continue
		case <-time.After(20 * time.Millisecond):
		}
		var waiting int
		if err := database.Pool.QueryRow(ctx, `select count(*) from pg_stat_activity
			where datname = current_database() and wait_event_type = 'Lock' and wait_event = 'advisory'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the removal from OTHER neither finished nor waited on the address's lock within a minute")
		}
	}
	if err := first.Commit(ctx); err != nil {
		t.Fatalf("commit the removal from ACC: %v", err)
	}
	if !finished {
		secondErr = <-done
	}
	if secondErr != nil {
		t.Fatalf("the removal from OTHER: %v", secondErr)
	}
	if err := second.Commit(ctx); err != nil {
		t.Fatalf("commit the removal from OTHER: %v", err)
	}
	if n := countRows(t, database, `select count(*) from passkeys where email = '`+accessErin+`'`); n != 0 {
		t.Fatalf("after both removals erin holds %d passkey(s), want every one deleted", n)
	}
}
