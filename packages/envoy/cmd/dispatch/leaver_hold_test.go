package main

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// A sign-out ends the browser's sessions and keeps the refresh token of a person whose access
// outlives the browser, so Google's later refusal still holds them (c-sign-out, its Dispatch
// half). Bob signs out holding no dsp_ token and no broker credential, and his row holds no
// refresh token. Ada signs out holding a live dsp_ token: her old cookie answers 401, and no
// request with it makes a session. Carol signs out holding only a machine login, and her token is
// kept. With the broker stopped at a sign-out, bob's, which the broker cannot be asked about, is
// kept too.
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
