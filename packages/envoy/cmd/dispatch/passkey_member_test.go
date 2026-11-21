package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/wire"
	"github.com/sjawhar/envoy/internal/brokertoken/brokertokentest"
	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/oidc"
)

// These rows hold a restricted member who signed in with a passkey, the only way one signs in, to
// the rules a restricted member's Google sign-in was held to before no Google sign-in admitted one:
// why their sign-in ended, what a sign-out tells them, when their personal token stops, that a
// group never admits them, that an address on a Workspace domain stops them, and that a Workspace
// account taking their address revokes them at the secrets broker.

// mintPasskeyToken mints a personal token at the outside origin on cookie, a passkey member's, as
// Settings mints one, and returns its plaintext.
func mintPasskeyToken(t *testing.T, r *passkeyRig, cookie *http.Cookie) string {
	t.Helper()
	minted := call(r.handler, passkeyOrigin, http.MethodPost, "/api/v1/me/agent-tokens", map[string]string{"name": "an agent of mine"}, cookie)
	var token struct {
		Token string `json:"token"`
	}
	if minted.Code != http.StatusCreated || json.Unmarshal(minted.Body.Bytes(), &token) != nil || token.Token == "" {
		t.Fatalf("POST /api/v1/me/agent-tokens at the outside origin answered %d %s, want 201 with a token", minted.Code, minted.Body.String())
	}
	return token.Token
}

// delist takes email off PRIV's list by a write that is no removal, so nothing ends their sign-in
// or deletes their passkeys at once and only the rule, asked again, can stop them.
func (r *passkeyRig) delist(t *testing.T, email string) {
	t.Helper()
	if _, err := r.database.Pool.Exec(context.Background(), `delete from project_access where project_key = 'PRIV' and principal = $1`, email); err != nil {
		t.Fatalf("take %s off PRIV's list: %v", email, err)
	}
}

// TestAPasskeySignInSaysWhyWhicheverRequestEndedIt: the dashboard asks several things at once, and
// whichever of them finds the hour lapsed runs the check. Once no list names the passkey member,
// the API read that ends the sign-in says why (401 NO_IDENTITY, NOT_LISTED, naming them), and the
// whoami after it, which finds a sign-in another request ended, says the same.
//
// Control: tell an ended passkey member no reason (OutsideRule.SignedOutReason answering ""), and
// the API read's 401 names none and the row fails.
func TestAPasskeySignInSaysWhyWhicheverRequestEndedIt(t *testing.T) {
	r := newPasskeyRig(t, nil)
	cookie := r.enrolled(t, newVirtualAuthenticator(t, passkeyRPID, passkeyOrigin), r.invite(t, outsiderEmail), outsiderEmail)
	r.delist(t, outsiderEmail)
	r.agePasskeyConfirmation(t, outsiderEmail)
	want := signedOutAnswer{Code: "NO_IDENTITY", Reason: "NOT_LISTED", Login: outsiderEmail}
	if got := signedOutOf(t, "GET /api/v1/projects", call(r.handler, passkeyOrigin, http.MethodGet, "/api/v1/projects", nil, cookie)); got != want {
		t.Fatalf("the API read that ran the hourly check: %+v, want %+v", got, want)
	}
	if got := signedOutOf(t, "whoami", whoamiOutside(r.handler, cookie)); got != want {
		t.Fatalf("the whoami after it: %+v, want %+v", got, want)
	}
	if n := r.passkeysOf(t, outsiderEmail); n != 0 {
		t.Fatalf("the ended passkey member holds %d passkey(s), want every one deleted", n)
	}
}

// TestAPasskeySignOutIsAPlainNoIdentity: a passkey member who signs out is told no reason. From the
// browser that signed out, which holds no cookie, and from another browser of theirs, whose session
// the sign-out ended, whoami is a 401 NO_IDENTITY naming no reason and no login, and an API read
// from the other browser is the 401 Dispatch has always answered, byte for byte.
//
// Control: end only the signing-out browser's session (POST /auth/logout advancing no session
// generation), and the other browser's whoami answers 200 and the row fails.
func TestAPasskeySignOutIsAPlainNoIdentity(t *testing.T) {
	r := newPasskeyRig(t, nil)
	device := newVirtualAuthenticator(t, passkeyRPID, passkeyOrigin)
	enrolled := r.enrolled(t, device, r.invite(t, outsiderEmail), outsiderEmail)
	other := sessionCookieOf(t, "the passkey sign-in at another browser", r.signIn(t, r.handler, device))
	if logout := call(r.handler, passkeyOrigin, http.MethodPost, "/auth/logout", nil, enrolled); logout.Code != http.StatusOK {
		t.Fatalf("POST /auth/logout at the outside origin answered %d %s, want 200", logout.Code, logout.Body.String())
	}
	plain := signedOutAnswer{Code: "NO_IDENTITY"}
	if got := signedOutOf(t, "whoami from the browser that signed out", whoamiOutside(r.handler, nil)); got != plain {
		t.Fatalf("whoami from the browser that signed out: %+v, want %+v", got, plain)
	}
	if got := signedOutOf(t, "whoami from another browser holding the ended session", whoamiOutside(r.handler, other)); got != plain {
		t.Fatalf("whoami from another browser holding the ended session: %+v, want %+v", got, plain)
	}
	requirePlainNoIdentity(t, "an API read from another browser holding the ended session",
		call(r.handler, passkeyOrigin, http.MethodGet, "/api/v1/projects", nil, other))
}

// TestAPasskeyMembersTokenStopsOnceNoListNamesThem: a passkey member's personal token acts while a
// project names them directly; once none does it is refused 401 TOKEN_OWNER_NOT_ADMITTED at its
// next use, with no hour to wait, and it acts again once a project names them again: the token
// itself was never revoked.
//
// Control: hold a restricted member's token to nothing (api.restrictedOwnerAdmitted answering nil),
// and the token acts with no list naming its owner and the row fails.
func TestAPasskeyMembersTokenStopsOnceNoListNamesThem(t *testing.T) {
	r := newPasskeyRig(t, nil)
	cookie := r.enrolled(t, newVirtualAuthenticator(t, passkeyRPID, passkeyOrigin), r.invite(t, outsiderEmail), outsiderEmail)
	token := mintPasskeyToken(t, r, cookie)
	requireTokenActs(t, "GET /api/v1/whoami with the passkey member's token", bearerRequest(r.handler, http.MethodGet, "/api/v1/whoami", "", token))

	r.delist(t, outsiderEmail)
	requireTokenRefused(t, "GET /api/v1/whoami with the token once no list names its owner", "no project names them any more",
		bearerRequest(r.handler, http.MethodGet, "/api/v1/whoami", "", token))

	r.listOn(t, "PRIV", access.PrincipalPerson, outsiderEmail)
	requireTokenActs(t, "GET /api/v1/whoami with the token once PRIV's list names its owner again",
		bearerRequest(r.handler, http.MethodGet, "/api/v1/whoami", "", token))
}

// TestAPasskeySignInListedOnlyThroughAGroupIsRefused: a group on an access list never admits a
// person from outside, only a direct listing does. A passkey member enrolled while PRIV named them
// directly; once PRIV names them only through the group crew, their passkey sign-in is refused 403
// NOT_LISTED with no cookie, which ends their sign-in and deletes their passkeys, and the cookie
// their enrollment set is told NOT_LISTED. The group's route takes no restricted member
// (GROUP_MEMBER_RESTRICTED), so the membership is written as a group made before the person was
// one holds it.
//
// Control: count a group's members as listed (access.Listed reading groups), and the passkey signs
// in and the row fails.
func TestAPasskeySignInListedOnlyThroughAGroupIsRefused(t *testing.T) {
	r := newPasskeyRig(t, nil)
	device := newVirtualAuthenticator(t, passkeyRPID, passkeyOrigin)
	cookie := r.enrolled(t, device, r.invite(t, outsiderEmail), outsiderEmail)
	if _, err := r.database.Pool.Exec(context.Background(), `insert into access_groups (name, owner) values ('crew', $1)`, privOwner); err != nil {
		t.Fatalf("seed the group crew: %v", err)
	}
	if _, err := r.database.Pool.Exec(context.Background(), `insert into access_group_members (group_name, person) values ('crew', $1)`, outsiderEmail); err != nil {
		t.Fatalf("put %s in the group crew: %v", outsiderEmail, err)
	}
	r.listOn(t, "PRIV", access.PrincipalGroup, "crew")
	r.delist(t, outsiderEmail)

	refused := r.signIn(t, r.handler, device)
	wantCode(t, "the passkey sign-in of a person PRIV names only through the group crew", refused, http.StatusForbidden, "NOT_LISTED")
	if sessionCookieSet(refused) {
		t.Fatal("the refused passkey sign-in set a session cookie")
	}
	if n := r.passkeysOf(t, outsiderEmail); n != 0 {
		t.Fatalf("after the refused sign-in %s holds %d passkey(s), want every one deleted", outsiderEmail, n)
	}
	if got := signedOutOf(t, "whoami with the enrollment's cookie", whoamiOutside(r.handler, cookie)); got.Code != "NO_IDENTITY" || got.Reason != "NOT_LISTED" {
		t.Fatalf("whoami with the enrollment's cookie after the refusal: %+v, want NO_IDENTITY NOT_LISTED", got)
	}
}

// TestAPasskeyMembersTokenStopsOnceTheirAddressIsOnAWorkspaceDomain: a restricted member's token is
// admitted by exactly the rule that admits their passkey sign-in, not by a second copy of it.
// bob@second.example, whom PRIV's list names, enrolls a passkey while
// DISPATCH_SIGNIN_WORKSPACE_DOMAINS lists nothing, and mints a token, which acts. The same
// deployment with second.example among the Workspace's domains refuses that token at once and while
// the list still names bob, 401 TOKEN_OWNER_NOT_ADMITTED saying the address is on a domain the
// Workspace owns, on a read and on a write, and refuses his passkey sign-in, 403 WORKSPACE_ADDRESS.
//
// Control: read the Workspace's domains for the sign-in alone (the personal token's check skipping
// AdmitsRestricted's Workspace address), and the token acts and the row fails.
func TestAPasskeyMembersTokenStopsOnceTheirAddressIsOnAWorkspaceDomain(t *testing.T) {
	const bob = "bob@second.example"
	r := newPasskeyRig(t, nil)
	r.listOn(t, "PRIV", access.PrincipalPerson, bob)
	device := newVirtualAuthenticator(t, passkeyRPID, passkeyOrigin)
	cookie := r.enrolled(t, device, r.invite(t, bob), bob)
	if kind := r.memberKind(t, bob); kind != string(model.MemberRestricted) {
		t.Fatalf("people.member_kind of %s = %q, want restricted", bob, kind)
	}
	token := mintPasskeyToken(t, r, cookie)
	requireTokenActs(t, "GET /api/v1/projects with bob's token, no Workspace domain listed",
		bearerRequest(r.handler, http.MethodGet, "/api/v1/projects", "", token))

	listed, _ := r.serverWith(t, map[string]string{"DISPATCH_SIGNIN_WORKSPACE_DOMAINS": "second.example"})
	const saying = "on a domain the Google Workspace owns"
	requireTokenRefused(t, "GET /api/v1/projects with bob's token, second.example listed", saying,
		bearerRequest(listed, http.MethodGet, "/api/v1/projects", "", token))
	requireTokenRefused(t, "POST /api/v1/issues with bob's token, second.example listed", saying,
		bearerRequest(listed, http.MethodPost, "/api/v1/issues", `{"project":"PRIV","title":"Written once bob's domain is the Workspace's","actor":{"kind":"session","id":"bob-agent"}}`, token))
	refused := r.signIn(t, listed, device)
	wantCode(t, "bob's passkey sign-in, second.example listed", refused, http.StatusForbidden, "WORKSPACE_ADDRESS")
	if sessionCookieSet(refused) {
		t.Fatal("bob's refused passkey sign-in set a session cookie")
	}
}

// TestAWorkspaceAccountTakingAPasskeyMembersAddressRevokesIt: a passkey member holds
// alice@second.example, a domain the Workspace does not list, which PRIV's list names, and at the
// secrets broker a sign-in on a machine recording their account, issued by the outside origin. Then a
// Workspace account signs in to the address with Google. One revocation pass makes one call to the
// broker, naming the address, the outside origin as the replaced account's issuer, the passkey
// member's subject, the Workspace account as the holder and the session generation that sign-in
// left; the broker ends the passkey member's sign-in with its tokens and refuses that account at the
// address. The passkey member's cookie is refused from the Workspace account's sign-in on.
//
// Control: record the replaced account under the sign-in issuer (the revocation naming Google's
// issuer for a passkey's subject), and the call names another issuer and the row fails.
func TestAWorkspaceAccountTakingAPasskeyMembersAddressRevokesIt(t *testing.T) {
	const email = "alice@second.example"
	ctx := context.Background()
	r := newPasskeyRig(t, nil)
	broker := newFakeBroker(t)
	handler, background := r.brokerRouter(t, broker)
	r.listOn(t, "PRIV", access.PrincipalPerson, email)
	finished := r.enroll(t, handler, newVirtualAuthenticator(t, passkeyRPID, passkeyOrigin), r.invite(t, email))
	cookie := sessionCookieOf(t, email+"'s enrollment", finished)
	var passkeySubject string
	if err := r.database.Pool.QueryRow(ctx, `select sub from people where email = $1`, email).Scan(&passkeySubject); err != nil || passkeySubject == "" {
		t.Fatalf("the passkey subject people records for %s: %q, %v; want one", email, passkeySubject, err)
	}
	outsideIssuer := oidc.CanonicalIssuer(passkeyOrigin)
	box := brokertokentest.NewCaller(t)
	broker.holds(&fakeCredential{id: "signin-passkey", person: email, subject: passkeySubject, issuer: outsideIssuer, signIn: true, tokens: []string{"token-of-the-passkey-members-sign-in"}}, box)

	r.workspaceSignIn(t, handler, email, firstHolder)
	if whoami := call(handler, passkeyOrigin, http.MethodGet, "/auth/whoami", nil, cookie); whoami.Code != http.StatusUnauthorized {
		t.Fatalf("the passkey member's whoami after the Workspace account took the address answered %d %s, want 401", whoami.Code, whoami.Body.String())
	}
	background.RevokeBrokerCredentials(ctx)
	var generation *int64
	if err := r.database.Pool.QueryRow(ctx, `select session_generation from broker_revocations where email = $1 and subject = $2`, email, passkeySubject).Scan(&generation); err != nil || generation == nil {
		t.Fatalf("the session generation the revocation of the passkey member records: %v, %v; want one", generation, err)
	}
	body, err := json.Marshal(wire.RevokePersonSubjectRequest{Person: email, Issuer: outsideIssuer, Subject: passkeySubject, HolderSubject: firstHolder, SessionGeneration: generation})
	if err != nil {
		t.Fatal(err)
	}
	requireCalls(t, broker, "after the pass that follows the Workspace account's sign-in", "POST /v1/person-subjects/revoke "+string(body))
	if broker.live("signin-passkey") {
		t.Error("the passkey member's sign-in at the broker outlived the Workspace account's sign-in to the address")
	}
	if !broker.refuses(email, passkeySubject) {
		t.Errorf("the broker does not refuse the passkey member's account at %s, which it no longer holds", email)
	}
	if recorded, done := r.brokerRevocation(t, email); !recorded || !done {
		t.Fatalf("after the pass the revocations of %s read recorded %t, done %t; want both settled", email, recorded, done)
	}
}
