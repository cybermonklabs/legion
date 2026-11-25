package main

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// A hold, at Dispatch, on the leaver rig (leaver_hold_rig_test.go): when Google refuses a person's
// refresh, Dispatch holds that person's credentials until they sign in again, and revokes nothing.
// Each hold here is made by the person's own token requests: their confirmation is dated two
// hours back, the fake Google refuses their refresh, and the next request with their dsp_ token
// makes Dispatch refresh. No browser request comes first and no membership pass runs.

// confirmationAge is how far back a test dates a confirmation: past the hour after which a request
// makes Dispatch refresh, and inside the 24 hours after which Dispatch holds a person it cannot
// confirm.
const confirmationAge = 2 * time.Hour

// The measurement table's Dispatch rows (c-leaver-end-to-end): ada, whom Google refuses, and bob,
// whom it confirms, each hold every credential of the table, and both confirmations are dated two
// hours back. Then ada's dsp_ token answers 401 MEMBERSHIP_HELD and her cookie 401 NO_IDENTITY,
// and bob's answer as they did before the refusal.
func TestLeaverHoldDispatchTokens(t *testing.T) {
	r := newLeaverRig(t)
	ada, bob := r.person(t, adaEmail), r.person(t, bobEmail)
	for _, p := range []*leaverPerson{bob, ada} {
		expectAnswer(t, p.email+"'s cookie (GET /api/v1/whoami), before Google refuses anyone", http.StatusOK, "").got(r.cookie(t, p))
		expectAnswer(t, p.email+"'s dsp_ token (GET /api/v1/whoami), before Google refuses anyone", http.StatusOK, "").got(r.personalToken(t, p.dsp))
	}
	if t.Failed() {
		t.FailNow()
	}

	r.refuse(adaEmail)
	r.ageConfirmation(t, adaEmail, confirmationAge)
	r.ageConfirmation(t, bobEmail, confirmationAge)
	expectAnswer(t, "bob@example.com's dsp_ token (GET /api/v1/whoami), his confirmation two hours old and Google confirming him", http.StatusOK, "").got(r.personalToken(t, bob.dsp))
	expectAnswer(t, "bob@example.com's cookie (GET /api/v1/whoami), Google confirming him", http.StatusOK, "").got(r.cookie(t, bob))
	expectAnswer(t, "ada@example.com's dsp_ token (GET /api/v1/whoami), the first request since Google refused her", http.StatusUnauthorized, "MEMBERSHIP_HELD").got(r.personalToken(t, ada.dsp))
	expectAnswer(t, "ada@example.com's cookie (GET /api/v1/whoami), Google refusing her", http.StatusUnauthorized, "NO_IDENTITY").got(r.cookie(t, ada))
}

// A revocation wins over a hold, and #4199's admission rule answers before it (c-refusal-order,
// all but erin's case, which needs the membership pass). While ada is held, her dsp_ token revoked
// before the hold answers 401 TOKEN_REVOKED. Another Google account then signs in to ada's
// address: her other dsp_ token answers 401 TOKEN_REVOKED, and her machine's broker token, whose
// credential records her replaced account, 401 BROKER_TOKEN_NOT_ACTIVE before and after #4199's
// revocation of that account settles. No restricted member signs in with Google here: Google
// signs in only the Workspace domain, and a person from outside signs in with a passkey, which
// Google never refuses.
func TestLeaverHoldRefusalOrder(t *testing.T) {
	ctx := context.Background()
	r := newLeaverRig(t)
	ada := r.person(t, adaEmail)
	revoked := r.mintToken(t, ada, "revoked before the hold")
	expectAnswer(t, "ada@example.com's second dsp_ token, before its revocation", http.StatusOK, "").got(r.personalToken(t, revoked))
	expectAnswer(t, "ada@example.com's broker token at Dispatch (GET /api/v1/whoami), before Google refuses her", http.StatusOK, "").got(r.brokerTokenAtDispatch(t, ada))
	r.revokeToken(t, ada, revoked)
	if t.Failed() {
		t.FailNow()
	}

	r.refuse(adaEmail)
	r.ageConfirmation(t, adaEmail, confirmationAge)
	expectAnswer(t, "ada@example.com's dsp_ token, the first request since Google refused her", http.StatusUnauthorized, "MEMBERSHIP_HELD").got(r.personalToken(t, ada.dsp))
	expectAnswer(t, "ada@example.com's revoked dsp_ token, while she is held", http.StatusUnauthorized, "TOKEN_REVOKED").got(r.personalToken(t, revoked))

	r.signInAs(t, adaEmail, "another-google-account")
	expectAnswer(t, "ada@example.com's dsp_ token, once another Google account signed in to her address", http.StatusUnauthorized, "TOKEN_REVOKED").got(r.personalToken(t, ada.dsp))
	expectAnswer(t, "ada@example.com's machine's broker token at Dispatch, before the revocation of her replaced account settles", http.StatusUnauthorized, "BROKER_TOKEN_NOT_ACTIVE").got(r.brokerTokenAtDispatch(t, ada))
	if recorded, done := r.outside.brokerRevocation(t, adaEmail); !recorded || done {
		t.Fatalf("the other account's sign-in to %s recorded a broker revocation %t, settled %t; want one owed", adaEmail, recorded, done)
	}
	r.background.RevokeBrokerCredentials(ctx)
	if recorded, done := r.outside.brokerRevocation(t, adaEmail); !recorded || !done {
		t.Fatalf("after the revocation pass, the broker revocation of %s reads recorded %t, settled %t; want it settled", adaEmail, recorded, done)
	}
	expectAnswer(t, "ada@example.com's machine's broker token at Dispatch, after the revocation of her replaced account settled", http.StatusUnauthorized, "BROKER_TOKEN_NOT_ACTIVE").got(r.brokerTokenAtDispatch(t, ada))
}

// A sign-out ends the browser's sessions and keeps the refresh token of a person whose access
// outlives the browser, so Google's later refusal still holds them (c-sign-out, its Dispatch
// half). Bob signs out holding no dsp_ token and no broker credential, and his row holds no
// refresh token. Ada signs out holding a live dsp_ token: her old cookie answers 401, and no
// request with it makes a session; Google then refuses her, and her dsp_ token answers 401
// MEMBERSHIP_HELD. Carol signs out holding only a machine login, and her token is kept. With the
// broker stopped at a sign-out, bob's, which the broker cannot be asked about, is kept too.
func TestLeaverHoldSignOut(t *testing.T) {
	r := newLeaverRig(t)
	ada := r.signIn(t, adaEmail)
	ada.dsp = r.mintToken(t, ada, "ada's agent")
	bob := r.signIn(t, bobEmail)
	carol := r.signIn(t, carolEmail)
	r.machineLogin(t, carol)
	expectAnswer(t, "ada@example.com's dsp_ token, before she signs out", http.StatusOK, "").got(r.personalToken(t, ada.dsp))
	if t.Failed() {
		t.FailNow()
	}

	r.signOut(t, bob)
	expectRefreshToken(t, r, "bob@example.com signed out holding no dsp_ token and no broker credential", bobEmail, false)

	r.signOut(t, ada)
	for _, path := range []string{"/api/v1/whoami", "/auth/whoami"} {
		old := cookieRequest(r.handler, http.MethodGet, path, "", ada.signedIn)
		expectAnswer(t, "ada@example.com's old cookie (GET "+path+"), after she signed out", http.StatusUnauthorized, "NO_IDENTITY").got(dispatchAnswer(old))
		if sessionCookieSet(old) {
			t.Errorf("ada@example.com's old cookie (GET %s), after she signed out, was answered a new session cookie, want none", path)
		} else {
			t.Logf("ada@example.com's old cookie (GET %s), after she signed out, was answered no session cookie, as wanted", path)
		}
	}
	r.refuse(adaEmail)
	r.ageConfirmation(t, adaEmail, confirmationAge)
	expectAnswer(t, "ada@example.com's dsp_ token, signed out and Google refusing her", http.StatusUnauthorized, "MEMBERSHIP_HELD").got(r.personalToken(t, ada.dsp))

	r.signOut(t, carol)
	expectRefreshToken(t, r, "carol@example.com signed out holding only a machine login", carolEmail, true)

	r.signInAgain(t, bobEmail)
	r.stopBroker()
	r.signOut(t, bob)
	r.startBroker()
	expectRefreshToken(t, r, fmt.Sprintf("bob@example.com signed out while the broker was stopped (it cut %d of Dispatch's calls)", r.cutCalls.Load()), bobEmail, true)
}

// The Kubernetes service accounts the broker's BROKER_POD_OPERATORS maps in
// TestLeaverHoldKeepsTheTokenOfAPersonAPodAccountIsMappedTo, each with the login service
// BROKER_SERVICE_LOGINS gives it: one to carol and one to gail. No pod of either logs in.
const (
	carolsPodAccount = "system:serviceaccount:people:carol"
	gailsPodAccount  = "system:serviceaccount:people:gail"
)

// A person whom BROKER_POD_OPERATORS maps a pod account to has access that outlives the browser
// whether or not a pod of the account holds a live login: a pod of it logs in as them at any time
// with its own token, with no step of theirs. On the real broker, which maps one account to carol
// and one to gail, no pod of either logged in:
//
//   - carol, who holds nothing else, signs out, and her refresh token is kept; bob, whom no mapping
//     names, signs out holding nothing, and his is forgotten;
//   - a membership pass asks again of gail's signed-out row, which holds nothing else, and keeps
//     her token, and of dave's, which holds nothing at all, and forgets his;
//   - once the operator drops gail's mapping, the pass an hour later forgets her token, and keeps
//     carol's, whose mapping stands.
//
// Control: the broker's person-access route, or Dispatch's OutlivingAccess.Holds, counting no
// mapping, each as it was: carol's token is forgotten at her sign-out and gail's by the first pass,
// and the test fails at both.
func TestLeaverHoldKeepsTheTokenOfAPersonAPodAccountIsMappedTo(t *testing.T) {
	const (
		daveEmail = "dave@example.com"
		gailEmail = "gail@example.com"
	)
	r := newLeaverRig(t, withPersonPod(carolsPodAccount, "person-carol", carolEmail), withPersonPod(gailsPodAccount, "person-gail", gailEmail))
	carol, bob := r.signIn(t, carolEmail), r.signIn(t, bobEmail)
	r.signOut(t, carol)
	expectRefreshToken(t, r, "carol@example.com signed out holding nothing but an account BROKER_POD_OPERATORS maps to her, no pod of it logged in", carolEmail, true)
	r.signOut(t, bob)
	expectRefreshToken(t, r, "bob@example.com signed out holding nothing, the broker mapping its accounts to carol@example.com and gail@example.com", bobEmail, false)

	// Gail and dave sign in, and their rows read signed out as a sign-out that kept their token
	// leaves them, without asking anything at the sign-out: the pass alone decides what they keep.
	r.signIn(t, gailEmail)
	r.signIn(t, daveEmail)
	if _, err := r.outside.database.Pool.Exec(context.Background(), `update people set signed_out_at = clock_timestamp() where email = any($1)`, []string{gailEmail, daveEmail}); err != nil {
		t.Fatal(err)
	}
	pass := r.membershipPass(nil)
	runPass(t, pass)
	expectRefreshToken(t, r, "a membership pass over gail@example.com's signed-out row, which holds nothing but an account BROKER_POD_OPERATORS maps to her", gailEmail, true)
	expectRefreshToken(t, r, "a membership pass over dave@example.com's signed-out row, which holds nothing", daveEmail, false)
	if t.Failed() {
		t.FailNow()
	}

	r.broker.SetPodOperator(gailsPodAccount, "")
	newPassClock(r.outside.database.Pool).advance(t, time.Hour+time.Minute)
	runPass(t, pass)
	expectRefreshToken(t, r, "the membership pass an hour after the operator dropped gail@example.com's mapping", gailEmail, false)
	expectRefreshToken(t, r, "the same pass over carol@example.com's row, her mapping standing", carolEmail, true)
}

// expectRefreshToken checks whether email's people row holds a refresh token after what: kept
// when keep, else forgotten. Another answer fails the test, naming it, and the one wanted is
// logged.
func expectRefreshToken(t *testing.T, r *leaverRig, what, email string, keep bool) {
	t.Helper()
	if stored := r.storesRefreshToken(t, email); stored != keep {
		t.Errorf("%s, and %s's people row holds a refresh token %t, want %t", what, email, stored, keep)
		return
	}
	t.Logf("%s, and %s's people row holds a refresh token %t, as wanted", what, email, keep)
}
