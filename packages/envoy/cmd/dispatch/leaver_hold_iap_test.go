package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/dispatch/identity/directorytest"
)

// The leaver rig under Identity-Aware Proxy (leaver_hold_rig_test.go, withIAP; R9): Dispatch keeps
// no refresh token there, so the Workspace Directory's answer about a person's account holds and
// releases them, and the broker serves Dispatch's answer that a person is not held only until the
// deadline Dispatch puts on it (not_held_until), 24 hours after the last active answer.

// The words an IAP deployment's broker refuses a held ada in (hold.Gate.HeldWords under
// BROKER_HOLD_REQUIRES_DEADLINE), and refuses her in once Dispatch's answer that she is not held has
// passed its deadline (the 503 HOLD_UNCHECKABLE of hold.Reason's deadline words).
const (
	iapBrokerHeldWords     = "Google's Workspace Directory no longer confirms " + adaEmail + "; their credentials work again once it does"
	iapBrokerDeadlineWords = "whether Dispatch holds " + adaEmail + " cannot be read: Dispatch's answer that they are not held has passed its deadline, or Dispatch has never confirmed them"
)

// iapReleaseBound is how long R4 gives a released person's credentials to serve again once
// Dispatch answers the broker: the broker's 30-second cache of Dispatch's answer, then up to 5
// seconds of Dispatch's cache of the broker's introspection answer and 5 seconds of one in flight.
const iapReleaseBound = 40 * time.Second

// iapLapseBound is how long R9 gives Dispatch to answer a broker token unavailable once its
// person's deadline has passed: the same 5 seconds of cache and 5 seconds in flight.
const iapLapseBound = 10 * time.Second

// TestIAPLeaverHoldThroughTheRealBroker (c-iap-deadline; R9, R2 and R4): the leaver rig under IAP,
// the real Dispatch router and the real broker on Postgres, the broker an IAP deployment's
// (BROKER_HOLD_REQUIRES_DEADLINE). Ada and bob hold every credential of the measurement table, ada
// also a sign-in on a machine and a pending machine login, and the fake Directory answers both
// accounts active.
//
// The membership pass then finds ada's account suspended. Each use of her credentials that R2
// refuses is refused, in an IAP deployment's words: her personal token 401 MEMBERSHIP_HELD, a new
// session under her machine login or her sign-in, her session's asks and its grant reads 403
// MEMBERSHIP_HELD, a broker token mint and a trade of her sign-in 400 membership_held, her broker
// token at Dispatch and her approval of the pending login 401 MEMBERSHIP_HELD, and the broker's
// introspection answers her broker token held. Her machine login's refresh grant and her session's
// lease renewal stay served, and each of bob's credentials serves.
//
// A later pass finds her account active. Within 40 seconds every use refused before serves again,
// with no sign-in and no request from her browser, though the broker still kept its held answer
// for her; the launcher credentials, enrollments, grants, sign-ins and personal tokens are the
// rows they were before the hold, so nothing was issued again; and a new session under her machine
// login or her sign-in, and her approval of the pending login, serve.
//
// Then her last active answer and the deadline it fixed are dated 25 hours back in Postgres, and
// the Directory fails each check of her account. Her broker token at Dispatch answers 503
// BROKER_UNAVAILABLE within 10 seconds; her personal token and her approval of a new pending login
// 503 IDENTITY_UNAVAILABLE; the broker's routes 503 HOLD_UNCHECKABLE, saying her deadline has
// passed, and its token endpoint server_error; its introspection of her token answers her active
// and not held with a not_held_until already past, which every reader refuses as a deadline passed;
// and each of bob's credentials still serves.
//
// Controls, each failing the test at the step it names: the refresh grant asking the gate as the
// device code grant does (oauth.Provider.Token), or the lease renewal asking it as the other
// session routes do (the broker's authRenewProof authentication), refuses ada's upkeep while she is
// held; the launcher routes asking no gate serve her new sessions while she is held; an Active
// outcome that releases no recorded hold (store.PgPeopleStore.RecordDirectoryActive) leaves her
// refused after the 40 seconds; the gate serving a not-held answer past its deadline
// (hold.Gate's notHeld) serves her at the broker past it; and withIAP without
// ciphertest.WithHoldRequiresDeadline refuses her held sessions in the words of a broker that
// tells her to sign in.
func TestIAPLeaverHoldThroughTheRealBroker(t *testing.T) {
	t.Parallel()
	r := newLeaverRig(t, withBrokerGate(), withPersonSignIns(), withIAP())
	r.directoryAccount(adaEmail, false)
	r.directoryAccount(bobEmail, false)
	ada, bob := r.person(t, adaEmail), r.person(t, bobEmail)
	r.personSignIn(t, ada)
	recordID, code := r.pendingMachineLogin(t, ada)
	before := snapshotLeaverCredentials(t, r)

	r.directoryAccount(adaEmail, true)
	r.directoryPass(t, adaEmail)
	// The broker asked Dispatch about ada as the rig gave her her credentials, and keeps each
	// not-held answer 30 seconds. It must lapse, or her rows below read the answer from before the
	// Directory found her account suspended rather than ask Dispatch, which now holds her.
	r.pastBrokerCache(t)

	token := bearerRequest(r.handler, http.MethodGet, "/api/v1/whoami", "", ada.dsp.plaintext)
	expectRefusal(t, "ada@example.com's personal token once the Directory holds her", token.Code, token.Body.Bytes(), http.StatusUnauthorized, "MEMBERSHIP_HELD", iapHeldText)
	status, body := r.enrolment(t, ada, ada.machineKey, ada.credential)
	expectRefusal(t, "ada@example.com's new enrollment under her machine login once the Directory holds her", status, body, http.StatusForbidden, "MEMBERSHIP_HELD", iapBrokerHeldWords)
	for _, row := range []struct {
		what   string
		status int
		code   string
		call   func() (int, string)
	}{
		{"ada@example.com's refresh grant while the Directory holds her", http.StatusOK, "", func() (int, string) { return r.refreshGrant(t, ada) }},
		{"ada@example.com's lease renewal while the Directory holds her", http.StatusOK, "", func() (int, string) { return r.renewLease(t, ada) }},
		{"ada@example.com's new enrollment under her sign-in while the Directory holds her", http.StatusForbidden, "MEMBERSHIP_HELD", func() (int, string) { return r.enrolUnderSignIn(t, ada) }},
		{"ada@example.com's sign-in trade while the Directory holds her", http.StatusBadRequest, oauthwire.MembershipHeld, func() (int, string) { return r.tradeSignIn(t, ada) }},
		{"ada@example.com's broker token mint while the Directory holds her", http.StatusBadRequest, oauthwire.MembershipHeld, func() (int, string) { return r.mintBrokerToken(t, ada) }},
		{"ada@example.com's existing broker token at Dispatch while the Directory holds her", http.StatusUnauthorized, "MEMBERSHIP_HELD", func() (int, string) { return r.brokerTokenAtDispatch(t, ada) }},
		{"ada@example.com's broker introspection while the Directory holds her", http.StatusOK, "held", func() (int, string) { return r.introspectionAnswer(t, ada) }},
		{"ada@example.com's agent-tier secret request while the Directory holds her", http.StatusForbidden, "MEMBERSHIP_HELD", func() (int, string) { return r.askAgentSecret(t, ada) }},
		{"ada@example.com's agent-tier grant read while the Directory holds her", http.StatusForbidden, "MEMBERSHIP_HELD", func() (int, string) { return r.readAgentGrant(t, ada) }},
		{"ada@example.com's human-tier secret request while the Directory holds her", http.StatusForbidden, "MEMBERSHIP_HELD", func() (int, string) { return r.askHumanSecret(t, ada) }},
		{"ada@example.com's human-tier grant read while the Directory holds her", http.StatusForbidden, "MEMBERSHIP_HELD", func() (int, string) { return r.readHumanGrant(t, ada) }},
		{"ada@example.com's approval of her pending machine login while the Directory holds her", http.StatusUnauthorized, "MEMBERSHIP_HELD", func() (int, string) { return r.approveOnToken(t, ada, recordID, code) }},
	} {
		expectAnswer(t, row.what, row.status, row.code).got(row.call())
	}
	if unserved := r.iapUnserved(t, bob); len(unserved) != 0 {
		t.Errorf("bob@example.com's credentials while the Directory holds ada: %s, want each served", strings.Join(unserved, "; "))
	}
	if t.Failed() {
		return
	}

	r.directoryAccount(adaEmail, false)
	released := time.Now()
	r.directoryPass(t, adaEmail)
	r.waitForIAPRelease(t, ada, released)
	expectLeaverSnapshot(t, before, snapshotLeaverCredentials(t, r))
	expectAnswer(t, "ada@example.com's new enrollment under her machine login after the release", http.StatusCreated, "").got(r.enrol(t, ada))
	expectAnswer(t, "ada@example.com's new enrollment under her sign-in after the release", http.StatusCreated, "").got(r.enrolUnderSignIn(t, ada))
	expectAnswer(t, "ada@example.com's approval of her pending machine login after the release", http.StatusOK, "").got(r.approveOnToken(t, ada, recordID, code))
	if t.Failed() {
		return
	}

	recordID, code = r.pendingMachineLogin(t, ada)
	r.directory.AnswerUser(adaEmail, directorytest.Answer{Status: http.StatusInternalServerError, Body: `{"error":{"code":500}}`})
	r.ageDirectory(t, adaEmail, 25*time.Hour)
	// The broker asked Dispatch about ada while it released her, and keeps that answer, and the
	// deadline it carries, 30 seconds: it must lapse for the broker to read the deadline aged here.
	r.pastBrokerCache(t)
	lapsed := time.Now()
	r.waitForLapsedBrokerToken(t, ada, lapsed)
	token = bearerRequest(r.handler, http.MethodGet, "/api/v1/whoami", "", ada.dsp.plaintext)
	expectRefusal(t, "ada@example.com's personal token past her deadline", token.Code, token.Body.Bytes(), http.StatusServiceUnavailable, "IDENTITY_UNAVAILABLE", iapUnavailableText)
	status, body = r.enrolment(t, ada, ada.machineKey, ada.credential)
	expectRefusal(t, "ada@example.com's new enrollment under her machine login past her deadline", status, body, http.StatusServiceUnavailable, "HOLD_UNCHECKABLE", iapBrokerDeadlineWords)
	for _, row := range []struct {
		what   string
		status int
		code   string
		call   func() (int, string)
	}{
		{"ada@example.com's new enrollment under her sign-in past her deadline", http.StatusServiceUnavailable, "HOLD_UNCHECKABLE", func() (int, string) { return r.enrolUnderSignIn(t, ada) }},
		{"ada@example.com's sign-in trade past her deadline", http.StatusInternalServerError, "server_error", func() (int, string) { return r.tradeSignIn(t, ada) }},
		{"ada@example.com's broker token mint past her deadline", http.StatusInternalServerError, "server_error", func() (int, string) { return r.mintBrokerToken(t, ada) }},
		{"ada@example.com's agent-tier secret request past her deadline", http.StatusServiceUnavailable, "HOLD_UNCHECKABLE", func() (int, string) { return r.askAgentSecret(t, ada) }},
		{"ada@example.com's agent-tier grant read past her deadline", http.StatusServiceUnavailable, "HOLD_UNCHECKABLE", func() (int, string) { return r.readAgentGrant(t, ada) }},
		{"ada@example.com's human-tier secret request past her deadline", http.StatusServiceUnavailable, "HOLD_UNCHECKABLE", func() (int, string) { return r.askHumanSecret(t, ada) }},
		{"ada@example.com's human-tier grant read past her deadline", http.StatusServiceUnavailable, "HOLD_UNCHECKABLE", func() (int, string) { return r.readHumanGrant(t, ada) }},
		{"ada@example.com's approval of a new pending machine login past her deadline", http.StatusServiceUnavailable, "IDENTITY_UNAVAILABLE", func() (int, string) { return r.approveOnToken(t, ada, recordID, code) }},
		{"bob@example.com's new enrollment past ada's deadline", http.StatusCreated, "", func() (int, string) { return r.enrol(t, bob) }},
	} {
		expectAnswer(t, row.what, row.status, row.code).got(row.call())
	}
	if unserved := r.iapUnserved(t, bob); len(unserved) != 0 {
		t.Errorf("bob@example.com's credentials past ada's deadline: %s, want each served", strings.Join(unserved, "; "))
	}
	if status, held, until := r.introspectedNotHeldUntil(t, ada); status != http.StatusOK || held || until <= 0 || until > time.Now().Unix() {
		t.Errorf("ada@example.com's broker introspection past her deadline = %d, held %t, not_held_until %d; want 200, not held, and a not_held_until already past", status, held, until)
	}
}

// introspectedNotHeldUntil is the broker's introspection of p's broker token under the listener
// account: its status, and for a 200 its held and not_held_until (0 for none).
func (r *leaverRig) introspectedNotHeldUntil(t *testing.T, p *leaverPerson) (status int, held bool, notHeldUntil int64) {
	t.Helper()
	form := url.Values{"token": {p.brokerToken}}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, r.broker.URL+"/v1/oauth/introspect", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+r.broker.ListenerToken)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("introspect %s's broker token: %v", p.email, err)
	}
	defer response.Body.Close()
	var answer struct {
		Held         bool  `json:"held"`
		NotHeldUntil int64 `json:"not_held_until"`
	}
	if response.StatusCode == http.StatusOK {
		if err := json.NewDecoder(response.Body).Decode(&answer); err != nil {
			t.Fatalf("decode %s's introspection answer: %v", p.email, err)
		}
	}
	return response.StatusCode, answer.Held, answer.NotHeldUntil
}

// iapBrowserDecisionHeldText and iapBrowserDecisionUnavailableText are Dispatch's words for a
// machine-login decision at a browser under IAP whose person the Workspace Directory holds, and
// whose standing cannot be read (approverStanding, browserDeciderHeld).
const (
	iapBrowserDecisionHeldText        = "Google's Workspace Directory no longer confirms this caller; their decisions work again once it does"
	iapBrowserDecisionUnavailableText = "whether this caller may still decide could not be read; try again in a minute"
)

// TestIAPHeldPersonDecidesNothingAtHerBrowser (R2's "a held person cannot approve", R9;
// approverStanding): under IAP a person decides a pending machine login at their browser with the
// assertion IAP signs for that request, which carries no personal token, no broker token and no
// session that a hold ends. Once the membership pass finds ada's account suspended, her approval
// and her deny of her pending login at her browser are 401 MEMBERSHIP_HELD in the Directory's
// words, though IAP still admits her browser (GET /auth/whoami 200), and the login stays pending.
// Once a later pass finds her account active, her approval of a second pending login at her browser
// serves. Once her last active answer and its deadline are 25 hours old and the Directory fails each
// check of her account, her approval of the first at her browser is 503 IDENTITY_UNAVAILABLE. Bob,
// whom the Directory answers active, approves his own pending login at his browser throughout.
//
// Control: read no standing for a decision at a browser (approverStanding as it was); ada's
// approval while the Directory holds her is relayed and answered 200, and the test fails there.
func TestIAPHeldPersonDecidesNothingAtHerBrowser(t *testing.T) {
	r := newLeaverRig(t, withBrokerGate(), withIAP())
	r.directoryAccount(adaEmail, false)
	r.directoryAccount(bobEmail, false)
	ada, bob := r.signIn(t, adaEmail), r.signIn(t, bobEmail)
	// IAP's sign-in records each person and checks no Directory: the first pass answers both
	// accounts active, as the hourly pass does after a person's first request.
	if err := r.membershipPass(nil).Run(context.Background()); err != nil {
		t.Fatalf("the first membership pass: %v", err)
	}
	first, firstCode := r.pendingMachineLogin(t, ada)
	second, secondCode := r.pendingMachineLogin(t, ada)
	bobsFirst, bobsFirstCode := r.pendingMachineLogin(t, bob)
	bobsSecond, bobsSecondCode := r.pendingMachineLogin(t, bob)

	r.directoryAccount(adaEmail, true)
	r.directoryPass(t, adaEmail)
	approve := r.decideAtBrowser(t, ada, "approve", first, firstCode)
	expectRefusal(t, "ada@example.com's approval at her browser while the Directory holds her", approve.Code, approve.Body.Bytes(), http.StatusUnauthorized, "MEMBERSHIP_HELD", iapBrowserDecisionHeldText)
	deny := r.decideAtBrowser(t, ada, "deny", first, firstCode)
	expectRefusal(t, "ada@example.com's deny at her browser while the Directory holds her", deny.Code, deny.Body.Bytes(), http.StatusUnauthorized, "MEMBERSHIP_HELD", iapBrowserDecisionHeldText)
	if whoami := r.browser(t, ada, http.MethodGet, "/auth/whoami", ""); whoami.Code != http.StatusOK {
		t.Errorf("GET /auth/whoami with ada@example.com's valid assertion while the Directory holds her answered %d %s, want 200: IAP admits her browser", whoami.Code, whoami.Body.String())
	}
	expectAnswer(t, "bob@example.com's approval at his browser while the Directory holds ada", http.StatusOK, "").got(dispatchAnswer(r.decideAtBrowser(t, bob, "approve", bobsFirst, bobsFirstCode)))
	if t.Failed() {
		return
	}

	r.directoryAccount(adaEmail, false)
	r.directoryPass(t, adaEmail)
	expectAnswer(t, "ada@example.com's approval at her browser after the release", http.StatusOK, "").got(dispatchAnswer(r.decideAtBrowser(t, ada, "approve", second, secondCode)))
	if t.Failed() {
		return
	}

	r.directory.AnswerUser(adaEmail, directorytest.Answer{Status: http.StatusInternalServerError, Body: `{"error":{"code":500}}`})
	r.ageDirectory(t, adaEmail, 25*time.Hour)
	approve = r.decideAtBrowser(t, ada, "approve", first, firstCode)
	expectRefusal(t, "ada@example.com's approval at her browser past her deadline", approve.Code, approve.Body.Bytes(), http.StatusServiceUnavailable, "IDENTITY_UNAVAILABLE", iapBrowserDecisionUnavailableText)
	expectAnswer(t, "bob@example.com's approval at his browser past ada's deadline", http.StatusOK, "").got(dispatchAnswer(r.decideAtBrowser(t, bob, "approve", bobsSecond, bobsSecondCode)))
}

// decideAtBrowser decides (approve or deny) the pending machine login recordID, whose
// confirmation code is code, on Dispatch's decision route from p's browser: under withIAP, the
// assertion IAP signs for that request and no other credential.
func (r *leaverRig) decideAtBrowser(t *testing.T, p *leaverPerson, decision, recordID, code string) *httptest.ResponseRecorder {
	t.Helper()
	return r.browser(t, p, http.MethodPost, "/api/v1/credential-requests/"+recordID+"/"+decision, leaverBody(t, map[string]string{"code": code}))
}

// TestIAPHeldPersonDecidesNothingWhoseBodyArrivesAfterTheHold (approverStanding,
// browserDeciderHeld): a decision at a person's browser under IAP reads her standing once its body
// has arrived, as a personal token's decision does (the api package's
// TestAHeldApproverRelaysNoApproval), since IAP admitted the request and Dispatch resolved its
// caller before the body, which a browser may hold back as long as it likes. Ada sends the headers
// of an approve of her pending machine login at her browser; while the body is held back the
// membership pass finds her account suspended, and once the body arrives the approve is 401
// MEMBERSHIP_HELD in the Directory's words. A later pass finds her account active, and a deny of
// the same login, its body held back across a pass that finds her account suspended again, is
// refused the same way. Once a pass finds her account active again her approval of that login
// serves and approves it: neither refused decision reached the broker, which would have approved
// or denied it.
//
// Control: ask Standing before the decision reads its body (browserDeciderHeld ahead of
// decisionFor): the read sees no hold yet, ada's approve is relayed and answered 200, and the test
// fails there.
func TestIAPHeldPersonDecidesNothingWhoseBodyArrivesAfterTheHold(t *testing.T) {
	r := newLeaverRig(t, withBrokerGate(), withIAP())
	r.directoryAccount(adaEmail, false)
	ada := r.signIn(t, adaEmail)
	if err := r.membershipPass(nil).Run(context.Background()); err != nil {
		t.Fatalf("the first membership pass: %v", err)
	}
	recordID, code := r.pendingMachineLogin(t, ada)
	directoryFinds := func(suspended bool) func() {
		return func() {
			r.directoryAccount(adaEmail, suspended)
			r.directoryPass(t, adaEmail)
		}
	}

	for _, decision := range []string{"approve", "deny"} {
		decided := r.decideAtBrowserHeld(t, ada, decision, recordID, code, directoryFinds(true))
		expectRefusal(t, "ada@example.com's "+decision+" at her browser whose body arrived after the Directory held her", decided.Code, decided.Body.Bytes(), http.StatusUnauthorized, "MEMBERSHIP_HELD", iapBrowserDecisionHeldText)
		if t.Failed() {
			return
		}
		directoryFinds(false)()
	}

	approved := r.decideAtBrowser(t, ada, "approve", recordID, code)
	var decided struct {
		State string `json:"state"`
	}
	if approved.Code != http.StatusOK || json.Unmarshal(approved.Body.Bytes(), &decided) != nil || decided.State != "approved" {
		t.Errorf("ada@example.com's approval at her browser after the release answered %d %s, want 200 approved: a refused decision reached the broker", approved.Code, approved.Body.String())
	}
}

// decideAtBrowserHeld is decideAtBrowser with the body held back (heldBody, serveHeld): once
// Dispatch waits on the body it runs whileHeld, then lets the body go and answers the response.
func (r *leaverRig) decideAtBrowserHeld(t *testing.T, p *leaverPerson, decision, recordID, code string, whileHeld func()) *httptest.ResponseRecorder {
	t.Helper()
	decided := leaverBody(t, map[string]string{"code": code})
	body := newHeldBody(decided)
	request := r.iapRequest(t, p, http.MethodPost, "/api/v1/credential-requests/"+recordID+"/"+decision, body)
	request.ContentLength = int64(len(decided))
	return serveHeld(t, r.handler, request, body, whileHeld)
}

// TestIAPHeldPersonMintsAndWidensNothingAtHerBrowser (R2, R9; createAgentToken,
// updateAgentTokenScopes, browserDeciderHeld): under IAP a person mints a personal token, and
// changes a token's scopes, at her browser with only the assertion IAP signs for that request. Ada
// and bob each mint at their browser a token holding the person scope alone. Once the membership
// pass finds ada's account suspended, her mint of another token and her change of her token's
// scopes to person and approve are 401 MEMBERSHIP_HELD in the Directory's words, though IAP still
// admits her browser (GET /auth/whoami 200): no token is written, and hers keeps the person scope
// alone. Bob, whom the Directory answers active, mints and widens at his browser throughout. Once a
// later pass finds her account active, her mint and her widening serve. Once her last active
// answer and its deadline are 25 hours old and the Directory fails each check of her account, her
// mint and her widening are 503 IDENTITY_UNAVAILABLE.
//
// Control: ask no standing for a mint or a scope change at a browser (createAgentToken and
// updateAgentTokenScopes as they were): ada's mint while the Directory holds her answers 201 and her
// widening 200, and the test fails there.
func TestIAPHeldPersonMintsAndWidensNothingAtHerBrowser(t *testing.T) {
	r := newLeaverRig(t, withBrokerGate(), withIAP())
	r.directoryAccount(adaEmail, false)
	r.directoryAccount(bobEmail, false)
	ada, bob := r.signIn(t, adaEmail), r.signIn(t, bobEmail)
	// IAP's sign-in records each person and checks no Directory: the first pass answers both
	// accounts active, as the hourly pass does after a person's first request.
	if err := r.membershipPass(nil).Run(context.Background()); err != nil {
		t.Fatalf("the first membership pass: %v", err)
	}
	adasToken := r.mintedAtBrowser(t, ada, "ada's narrow token")
	bobsToken := r.mintedAtBrowser(t, bob, "bob's narrow token")

	r.directoryAccount(adaEmail, true)
	r.directoryPass(t, adaEmail)
	mint := r.mintAtBrowser(t, ada, "minted while held")
	expectRefusal(t, "ada@example.com's mint at her browser while the Directory holds her", mint.Code, mint.Body.Bytes(), http.StatusUnauthorized, "MEMBERSHIP_HELD", iapBrowserDecisionHeldText)
	widen := r.widenAtBrowser(t, ada, adasToken)
	expectRefusal(t, "ada@example.com's widening of her token at her browser while the Directory holds her", widen.Code, widen.Body.Bytes(), http.StatusUnauthorized, "MEMBERSHIP_HELD", iapBrowserDecisionHeldText)
	if minted := r.tokensNamed(t, adaEmail, "minted while held"); minted != 0 {
		t.Errorf("ada@example.com's refused mint wrote %d tokens, want none", minted)
	}
	if scopes := r.tokenScopes(t, adasToken); !slices.Equal(scopes, []string{"person"}) {
		t.Errorf("ada@example.com's token after her refused widening holds %v, want [person]", scopes)
	}
	if whoami := r.browser(t, ada, http.MethodGet, "/auth/whoami", ""); whoami.Code != http.StatusOK {
		t.Errorf("GET /auth/whoami with ada@example.com's valid assertion while the Directory holds her answered %d %s, want 200: IAP admits her browser", whoami.Code, whoami.Body.String())
	}
	expectAnswer(t, "bob@example.com's mint at his browser while the Directory holds ada", http.StatusCreated, "").got(dispatchAnswer(r.mintAtBrowser(t, bob, "bob's token while ada is held")))
	expectAnswer(t, "bob@example.com's widening at his browser while the Directory holds ada", http.StatusOK, "").got(dispatchAnswer(r.widenAtBrowser(t, bob, bobsToken)))
	if t.Failed() {
		return
	}

	r.directoryAccount(adaEmail, false)
	r.directoryPass(t, adaEmail)
	released := r.mintedAtBrowser(t, ada, "minted after the release")
	expectAnswer(t, "ada@example.com's widening at her browser after the release", http.StatusOK, "").got(dispatchAnswer(r.widenAtBrowser(t, ada, adasToken)))
	if t.Failed() {
		return
	}

	r.directory.AnswerUser(adaEmail, directorytest.Answer{Status: http.StatusInternalServerError, Body: `{"error":{"code":500}}`})
	r.ageDirectory(t, adaEmail, 25*time.Hour)
	mint = r.mintAtBrowser(t, ada, "minted past the deadline")
	expectRefusal(t, "ada@example.com's mint at her browser past her deadline", mint.Code, mint.Body.Bytes(), http.StatusServiceUnavailable, "IDENTITY_UNAVAILABLE", iapBrowserDecisionUnavailableText)
	widen = r.widenAtBrowser(t, ada, released)
	expectRefusal(t, "ada@example.com's widening of her token at her browser past her deadline", widen.Code, widen.Body.Bytes(), http.StatusServiceUnavailable, "IDENTITY_UNAVAILABLE", iapBrowserDecisionUnavailableText)
	expectAnswer(t, "bob@example.com's mint at his browser past ada's deadline", http.StatusCreated, "").got(dispatchAnswer(r.mintAtBrowser(t, bob, "bob's token past ada's deadline")))
}

// mintAtBrowser is POST /api/v1/me/agent-tokens from p's browser, as the agent tokens page sends
// it, for a token named name holding the person scope alone.
func (r *leaverRig) mintAtBrowser(t *testing.T, p *leaverPerson, name string) *httptest.ResponseRecorder {
	t.Helper()
	return r.browser(t, p, http.MethodPost, "/api/v1/me/agent-tokens", leaverBody(t, map[string]any{"name": name, "scopes": []string{"person"}}))
}

// mintedAtBrowser is mintAtBrowser's token id, failing t unless the mint answers 201 with one.
func (r *leaverRig) mintedAtBrowser(t *testing.T, p *leaverPerson, name string) string {
	t.Helper()
	minted := r.mintAtBrowser(t, p, name)
	var token struct {
		ID string `json:"id"`
	}
	if minted.Code != http.StatusCreated || json.Unmarshal(minted.Body.Bytes(), &token) != nil || token.ID == "" {
		t.Fatalf("POST /api/v1/me/agent-tokens for %q on %s's browser answered %d %s, want 201 with a token", name, p.email, minted.Code, minted.Body.String())
	}
	return token.ID
}

// widenAtBrowser is PATCH /api/v1/me/agent-tokens/{id} from p's browser, as the agent tokens page
// sends it, giving the token id the person and approve scopes.
func (r *leaverRig) widenAtBrowser(t *testing.T, p *leaverPerson, id string) *httptest.ResponseRecorder {
	t.Helper()
	return r.browser(t, p, http.MethodPatch, "/api/v1/me/agent-tokens/"+id, leaverBody(t, map[string]any{"scopes": []string{"person", "approve"}}))
}

// tokensNamed is how many personal tokens email owns named name.
func (r *leaverRig) tokensNamed(t *testing.T, email, name string) int {
	t.Helper()
	var count int
	if err := r.outside.database.Pool.QueryRow(context.Background(), `select count(*) from agent_tokens where canonical_login(owner) = $1 and name = $2`, email, name).Scan(&count); err != nil {
		t.Fatalf("count %s's tokens named %q: %v", email, name, err)
	}
	return count
}

// tokenScopes is the scopes the personal token id holds.
func (r *leaverRig) tokenScopes(t *testing.T, id string) []string {
	t.Helper()
	var scopes []string
	if err := r.outside.database.Pool.QueryRow(context.Background(), `select scopes from agent_tokens where id = $1`, id).Scan(&scopes); err != nil {
		t.Fatalf("read the scopes of token %s: %v", id, err)
	}
	return scopes
}

// directoryAccount makes the fake Directory answer email's own Workspace account, suspended as
// suspended says: users.get 200 with the account's id, "directory-<email>", and email as its
// primary address.
func (r *leaverRig) directoryAccount(email string, suspended bool) {
	r.directory.SetUser(email, directorytest.User{ID: "directory-" + email, PrimaryEmail: email, Suspended: suspended})
}

// directoryPass makes email's row due a Directory check (ageDirectory, two hours back) and runs
// one membership pass over the router's Membership, as the maintenance sweep runs it: its check
// of email's account records the outcome the fake Directory answers.
func (r *leaverRig) directoryPass(t *testing.T, email string) {
	t.Helper()
	r.ageDirectory(t, email, 2*time.Hour)
	if err := r.membershipPass(nil).Run(context.Background()); err != nil {
		t.Fatalf("the membership pass: %v", err)
	}
}

// ageDirectory dates email's Directory record d back by the database's clock: its last active
// answer (directory_confirmed_at), the deadline that answer fixed (not_held_until), its last check
// (directory_checked_at) and a recorded hold (held_since), so their order stands. It fails t when
// no row of email's holds an active answer to date.
func (r *leaverRig) ageDirectory(t *testing.T, email string, d time.Duration) {
	t.Helper()
	aged, err := r.outside.database.Pool.Exec(context.Background(), `
		update people set directory_confirmed_at = directory_confirmed_at - make_interval(secs => $2),
			not_held_until = not_held_until - make_interval(secs => $2),
			directory_checked_at = directory_checked_at - make_interval(secs => $2),
			held_since = held_since - make_interval(secs => $2)
		where email = $1 and directory_confirmed_at is not null
	`, email, d.Seconds())
	if err != nil {
		t.Fatalf("date %s's Directory record %s back: %v", email, d, err)
	}
	if aged.RowsAffected() != 1 {
		t.Fatalf("dating %s's Directory record %s back changed %d rows, want 1: the Directory never answered their account active", email, d, aged.RowsAffected())
	}
}

// tradeSignIn trades p's sign-in on the fixture's metadata machine again (signInTrade), as the
// machine's agent-secrets renews it, answering as a credential call answers.
func (r *leaverRig) tradeSignIn(t *testing.T, p *leaverPerson) (int, string) {
	t.Helper()
	status, refusal, _ := r.signInTrade(t, p)
	return status, refusal
}

// introspectionAnswer is the broker's introspection of p's broker token (introspection) as a
// credential call answers it, with the code "held" for a 200 that answers the token's person held.
func (r *leaverRig) introspectionAnswer(t *testing.T, p *leaverPerson) (int, string) {
	t.Helper()
	status, held, code := r.introspection(t, p)
	if held {
		return status, "held"
	}
	return status, code
}

// approveOnToken approves the pending machine login recordID, whose confirmation code is code,
// on Dispatch's decision route with p's personal token, as an agent of p's approves one.
func (r *leaverRig) approveOnToken(t *testing.T, p *leaverPerson, recordID, code string) (int, string) {
	t.Helper()
	return dispatchAnswer(bearerRequest(r.handler, http.MethodPost, "/api/v1/credential-requests/"+recordID+"/approve",
		leaverBody(t, map[string]string{"code": code}), p.dsp.plaintext))
}

// iapUnserved is each of p's credential uses that a hold refuses, and the upkeep it serves, that
// does not serve now, by name and answer: none when each serves, the broker's introspection
// answering p's broker token not held. It sends nothing from p's browser, so a release it sees
// came from no IAP assertion of p's, and it adds no row the release snapshot reads, as a new
// session and an approval would. A trade of p's sign-in is one of them only for a person the rig
// gave a sign-in (personSignIn).
func (r *leaverRig) iapUnserved(t *testing.T, p *leaverPerson) []string {
	t.Helper()
	type use struct {
		name string
		call func() (int, string)
	}
	uses := []use{
		{"personal token", func() (int, string) { return r.personalToken(t, p.dsp) }},
		{"refresh grant", func() (int, string) { return r.refreshGrant(t, p) }},
		{"lease renewal", func() (int, string) { return r.renewLease(t, p) }},
		{"broker token mint", func() (int, string) { return r.mintBrokerToken(t, p) }},
		{"broker token at Dispatch", func() (int, string) { return r.brokerTokenAtDispatch(t, p) }},
		{"broker introspection", func() (int, string) { return r.introspectionAnswer(t, p) }},
		{"agent-tier secret request", func() (int, string) { return r.askAgentSecret(t, p) }},
		{"agent-tier grant read", func() (int, string) { return r.readAgentGrant(t, p) }},
		{"human-tier secret request", func() (int, string) { return r.askHumanSecret(t, p) }},
		{"human-tier grant read", func() (int, string) { return r.readHumanGrant(t, p) }},
	}
	if p.signInCredential != "" {
		uses = append(uses, use{"sign-in trade", func() (int, string) { return r.tradeSignIn(t, p) }})
	}
	var unserved []string
	for _, use := range uses {
		if status, code := use.call(); status != http.StatusOK || code != "" {
			unserved = append(unserved, fmt.Sprintf("%s answered %d %q", use.name, status, code))
		}
	}
	return unserved
}

// waitForIAPRelease requires each use iapUnserved reads of p's to serve within iapReleaseBound of
// since, the moment before the pass whose Active outcome released p, polling each second by the
// wall clock: the broker's 30-second cache of its held answer for p lapses on its own, inside the
// bound, as it does in a deployment.
func (r *leaverRig) waitForIAPRelease(t *testing.T, p *leaverPerson, since time.Time) {
	t.Helper()
	for {
		unserved := r.iapUnserved(t, p)
		elapsed := time.Since(since)
		switch {
		case len(unserved) == 0 && elapsed <= iapReleaseBound:
			t.Logf("%s's credentials served again %s after the pass that found their account active", p.email, elapsed.Round(100*time.Millisecond))
			return
		case len(unserved) == 0:
			t.Errorf("%s's credentials served again only %s after the pass that found their account active, want within %s", p.email, elapsed.Round(100*time.Millisecond), iapReleaseBound)
			return
		case elapsed > iapReleaseBound:
			t.Errorf("%s's credentials %s after the pass that found their account active: %s; want each served within %s", p.email, elapsed.Round(100*time.Millisecond), strings.Join(unserved, "; "), iapReleaseBound)
			return
		}
		time.Sleep(time.Second)
	}
}

// waitForLapsedBrokerToken requires p's broker token at Dispatch to answer 503 BROKER_UNAVAILABLE
// within iapLapseBound of since, the moment p's deadline was passed, polling every quarter second
// by the wall clock: until then it serves, from Dispatch's cache of the broker's last
// introspection answer, and it answers nothing else.
func (r *leaverRig) waitForLapsedBrokerToken(t *testing.T, p *leaverPerson, since time.Time) {
	t.Helper()
	for {
		status, code := r.brokerTokenAtDispatch(t, p)
		elapsed := time.Since(since)
		switch {
		case status == http.StatusServiceUnavailable && code == "BROKER_UNAVAILABLE" && elapsed <= iapLapseBound:
			t.Logf("%s's broker token at Dispatch answered 503 BROKER_UNAVAILABLE %s past their deadline", p.email, elapsed.Round(100*time.Millisecond))
			return
		case status == http.StatusServiceUnavailable && code == "BROKER_UNAVAILABLE", elapsed > iapLapseBound:
			t.Errorf("%s's broker token at Dispatch answered %d %q %s past their deadline, want 503 BROKER_UNAVAILABLE within %s", p.email, status, code, elapsed.Round(100*time.Millisecond), iapLapseBound)
			return
		case status != http.StatusOK || code != "":
			t.Errorf("%s's broker token at Dispatch answered %d %q %s past their deadline, want 200 until Dispatch's cache of the introspection lapses, then 503 BROKER_UNAVAILABLE", p.email, status, code, elapsed.Round(100*time.Millisecond))
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// expectRefusal fails t unless a JSON answer of Dispatch or the broker, its status and body, is
// wantStatus with code in words, each of which writes {"code", "error"}: what was asked, named as
// expectAnswer names it. The one wanted is logged, so a run shows each row it read.
func expectRefusal(t *testing.T, what string, status int, body []byte, wantStatus int, code, words string) {
	t.Helper()
	var refusal struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &refusal)
	if status != wantStatus || refusal.Code != code || refusal.Error != words {
		t.Errorf("%s answered %d %s, want %d %s %q", what, status, strings.TrimSpace(string(body)), wantStatus, code, words)
		return
	}
	t.Logf("%s answered %d %q in the words wanted", what, status, code)
}
