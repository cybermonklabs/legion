package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"

	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/dispatch/cipher"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A broker token names the Google account its credential records at the broker (the
// introspection's account_issuer and account_subject), so Dispatch holds the token to that
// account: it acts for its person only while that account is the one people records for the
// address, and its approve names that account, never whichever account holds the address.

// approveRelayed is the call the broker is told of for an approve of rec1 by reassignedEmail,
// naming the account subject under s's sign-in issuer, and no account at all for "", and carrying
// the address's session generation as it stands now (sessionGeneration): called right after the
// approve, with nothing between to move it, that is the generation the approve's check read.
func (s *outsideSignIn) approveRelayed(t *testing.T, subject string) string {
	t.Helper()
	code, generation := "ABCD-1234", s.sessionGeneration(t, reassignedEmail)
	decision := cipher.Decision{Approver: reassignedEmail, Code: &code, ApproverSessionGeneration: &generation}
	if subject != "" {
		decision.ApproverIssuer, decision.ApproverSubject = s.pool.URL(), subject
	}
	body, err := json.Marshal(decision)
	if err != nil {
		t.Fatal(err)
	}
	return "POST /v1/credential-requests/rec1/approve " + string(body)
}

// requireBrokerAccountNotSignedIn fails the test unless recorder is the refusal of a broker token
// whose account Dispatch does not record for its address while no revocation names that account:
// 401 BROKER_ACCOUNT_NOT_SIGNED_IN, saying to sign in to Dispatch, a code a client does not take for
// a stale token.
func requireBrokerAccountNotSignedIn(t *testing.T, what string, recorder *httptest.ResponseRecorder) {
	t.Helper()
	if recorder.Code != http.StatusUnauthorized || errorCode(recorder) != "BROKER_ACCOUNT_NOT_SIGNED_IN" ||
		!strings.Contains(recorder.Body.String(), "sign in to Dispatch") {
		t.Errorf("%s answered %d %s, want 401 BROKER_ACCOUNT_NOT_SIGNED_IN saying to sign in to Dispatch", what, recorder.Code, recorder.Body.String())
	}
}

// Google gives alice@example.com to a second account while Dispatch still records the first: the
// second account signs in at the broker, and its broker token, which the broker answers as the
// second account's, approves a machine login. Dispatch refuses it 401 BROKER_ACCOUNT_NOT_SIGNED_IN,
// saying to sign in to Dispatch, at its admission, and tells the broker nothing: relayed, the
// approve would name the first account. Once the second account signs in to Dispatch, and the
// refusal by address that sign-in starts is over, the same token acts, and approves nothing still:
// no broker token holds approve (403 OAUTH_SCOPE_REQUIRED).
func TestOutsideSignInABrokerTokenOfAnAccountDispatchDoesNotRecordApprovesNothing(t *testing.T) {
	ctx := context.Background()
	s := newOutsideSignIn(t, nil)
	broker := newFakeBroker(t)
	handler, background := s.brokerRouter(t, broker)
	s.workspaceSignIn(t, handler, reassignedEmail, firstHolder)
	box := ciphertokentest.NewCaller(t)
	const token = "token-of-the-second-holders-sign-in"
	broker.holds(&fakeCredential{id: "signin-second", person: reassignedEmail, subject: secondHolder, signIn: true, tokens: []string{token}}, box)

	requireBrokerAccountNotSignedIn(t, "the approve by the second account's broker token while Dispatch records the first",
		brokerTokenRequest(t, handler, box, http.MethodPost, approvePath, approveBody, token))
	requireCalls(t, broker, "after the approve by the second account's broker token while Dispatch records the first")
	requireBrokerAccountNotSignedIn(t, "GET /api/v1/whoami with the second account's broker token while Dispatch records the first",
		brokerTokenRequest(t, handler, box, http.MethodGet, "/api/v1/whoami", "", token))

	s.workspaceSignIn(t, handler, reassignedEmail, secondHolder)
	background.RevokeBrokerCredentials(ctx)
	revoke := s.subjectRevoke(t, reassignedEmail, firstHolder, secondHolder)
	requireCalls(t, broker, "after the revocation pass", revoke)
	s.settledAgo(t, reassignedEmail, 31*time.Second)
	requireTokenActs(t, "GET /api/v1/whoami with the second account's broker token, once it signed in to Dispatch",
		brokerTokenRequest(t, handler, box, http.MethodGet, "/api/v1/whoami", "", token))
	requireNoApproveScope(t, "the approve by the second account's broker token, once it signed in to Dispatch",
		brokerTokenRequest(t, handler, box, http.MethodPost, approvePath, approveBody, token))
	requireCalls(t, broker, "after the approve by the second account's broker token, once it signed in to Dispatch", revoke)
}

// The replaced account's broker token is refused from the commit of the new account's sign-in to
// Dispatch, whether or not a revocation is recorded: the first holder signs in, and its broker
// token, which the broker answers as the first holder's, reads and holds a live socket open.
// Then the address's row comes to record no account (as a pool or header sign-in leaves it), so
// the second holder's sign-in replaces no recorded account and records no broker revocation at
// all. From that commit the first holder's token is refused at a request, 401
// BROKER_ACCOUNT_NOT_SIGNED_IN, since no revocation names its account and Dispatch cannot tell it
// from an account that has not signed in yet (either acts again only once its account signs in),
// and the socket it opened closes (1008, the caller no longer resolving) at its next heartbeat;
// the second holder's own broker token acts at once.
func TestOutsideSignInTheReplacedAccountsBrokerTokenEndsWithTheNewAccountsSignIn(t *testing.T) {
	s := newOutsideSignIn(t, nil)
	broker := newFakeBroker(t)
	handler, _ := s.brokerRouter(t, broker)
	s.workspaceSignIn(t, handler, reassignedEmail, firstHolder)
	machine, box := ciphertokentest.NewCaller(t), ciphertokentest.NewCaller(t)
	const replacedToken, holderToken = "token-of-the-first-holders-machine", "token-of-the-second-holders-sign-in"
	broker.holds(&fakeCredential{id: "login-first", person: reassignedEmail, subject: firstHolder, tokens: []string{replacedToken}}, machine)
	broker.holds(&fakeCredential{id: "signin-second", person: reassignedEmail, subject: secondHolder, signIn: true, tokens: []string{holderToken}}, box)
	requireTokenActs(t, "GET /api/v1/whoami with the first holder's broker token, before the address passes on",
		brokerTokenRequest(t, handler, machine, http.MethodGet, "/api/v1/whoami", "", replacedToken))

	server := httptest.NewServer(handler)
	defer server.Close()
	signed := machine.Signed(t, http.MethodGet, brokerOrigin+"/ws/live", replacedToken)
	socket, upgraded, err := gws.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/ws/live", http.Header{
		"Authorization": {signed.Header.Get("Authorization")}, "DPoP": {signed.Header.Get("DPoP")},
	})
	if err != nil {
		t.Fatalf("open /ws/live with the first holder's broker token: %v (%v)", err, upgraded)
	}
	defer socket.Close()
	closed := make(chan error, 1)
	go func() {
		for {
			if _, _, err := socket.ReadMessage(); err != nil {
				closed <- err
				return
			}
		}
	}()

	storetest.TestOnlyClearAccount(t, s.database, reassignedEmail)
	s.workspaceSignIn(t, handler, reassignedEmail, secondHolder)
	if recorded, _ := s.brokerRevocation(t, reassignedEmail); recorded {
		t.Fatalf("the second holder's sign-in, replacing no recorded account, recorded a broker revocation of %s", reassignedEmail)
	}
	requireBrokerAccountNotSignedIn(t, "GET /api/v1/whoami with the first holder's broker token, once the second holder signed in",
		brokerTokenRequest(t, handler, machine, http.MethodGet, "/api/v1/whoami", "", replacedToken))
	requireTokenActs(t, "GET /api/v1/whoami with the second holder's broker token, once it signed in",
		brokerTokenRequest(t, handler, box, http.MethodGet, "/api/v1/whoami", "", holderToken))

	// The heartbeat is StreamHeartbeat's fifteen seconds; the bound is one beat and margin.
	select {
	case err := <-closed:
		var closure *gws.CloseError
		if !errors.As(err, &closure) || closure.Code != gws.ClosePolicyViolation {
			t.Fatalf("the socket the first holder's broker token opened ended with %v, want the 1008 close of a caller that no longer resolves", err)
		}
	case <-time.After(25 * time.Second):
		t.Fatal("the socket the first holder's broker token opened was still open 25 s after the second holder signed in, past its next heartbeat")
	}
}

// A request a broker token sent before another account signed in to its address, whose body
// arrived after, acts on nothing: the first holder signs in, the address's row comes to record no
// account (so no revocation is ever recorded for what follows), and the first holder's broker
// token, admitted while the row records none, sends the headers of a machine login's approve and
// of a personal token's mint. While Dispatch waits on each body the second holder signs in. Each
// is refused 403 OAUTH_SCOPE_REQUIRED, since no broker token holds approve, the broker is told no
// approve and the mint writes no token; the token's next request is refused 401
// BROKER_ACCOUNT_NOT_SIGNED_IN, since no revocation names its account and nothing of it was
// revoked, never BROKER_TOKEN_NOT_ACTIVE, which a client takes for a stale token and trades again
// for.
func TestOutsideSignInABrokerTokensHeldRequestActsOnNothingOnceAnotherAccountSignsIn(t *testing.T) {
	for _, c := range []struct{ name, path, body string }{
		{"approve", approvePath, approveBody},
		{"mint", "/api/v1/me/agent-tokens", `{"name":"minted while its body was held"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newOutsideSignIn(t, nil)
			broker := newFakeBroker(t)
			handler, _ := s.brokerRouter(t, broker)
			s.workspaceSignIn(t, handler, reassignedEmail, firstHolder)
			storetest.TestOnlyClearAccount(t, s.database, reassignedEmail)
			machine := ciphertokentest.NewCaller(t)
			const token = "token-of-the-first-holders-machine"
			broker.holds(&fakeCredential{id: "login-first", person: reassignedEmail, subject: firstHolder, tokens: []string{token}}, machine)

			body := newHeldBody(c.body)
			request := httptest.NewRequest(http.MethodPost, brokerOrigin+c.path, body)
			request.ContentLength = int64(len(c.body))
			request.Header.Set("Content-Type", "application/json")
			signed := machine.Signed(t, http.MethodPost, brokerOrigin+c.path, token)
			request.Header.Set("Authorization", signed.Header.Get("Authorization"))
			request.Header.Set("DPoP", signed.Header.Get("DPoP"))
			recorder := serveHeld(t, handler, request, body, func() {
				s.workspaceSignIn(t, handler, reassignedEmail, secondHolder)
			})
			if recorded, _ := s.brokerRevocation(t, reassignedEmail); recorded {
				t.Fatalf("the second holder's sign-in, replacing no recorded account, recorded a broker revocation of %s", reassignedEmail)
			}
			requireNoApproveScope(t, "the "+c.name+" by the first holder's broker token, whose body arrived after the second holder signed in", recorder)
			requireBrokerAccountNotSignedIn(t, "GET /api/v1/whoami with the first holder's broker token, after the second holder signed in",
				brokerTokenRequest(t, handler, machine, http.MethodGet, "/api/v1/whoami", "", token))
			requireCalls(t, broker, "after the "+c.name+" by the first holder's broker token, whose body arrived after the second holder signed in")
			if rows := s.tokenRowsNamed(t, "minted while its body was held"); rows != 0 {
				t.Errorf("the refused mint wrote %d token rows, want none", rows)
			}
		})
	}
}

// A broker token whose credential records no account is held to when the broker made that
// credential, against when Dispatch recorded the address's account (people.sub_recorded_at). With
// the first holder's account recorded for the address:
//
//   - made before the recording, or at a time the broker did not record, it is refused 401
//     BROKER_TOKEN_RECORDS_NO_ACCOUNT, at a read and at an approve, and relays no approve;
//   - made ten minutes after the recording, past brokerCredentialMargin, it acts at a read;
//   - with no recording time (a row from before the column), it acts at a read.
//
// Whatever its credential, it approves nothing once admitted: no broker token holds approve (403
// OAUTH_SCOPE_REQUIRED), so the broker is told no approve in any case.
//
// A broker token whose account is of an issuer other than Dispatch's sign-in issuer names an
// account Dispatch cannot compare with the one it records, so once the address records an account
// it is refused the same way whenever it was made, ten minutes after the recording and with no
// recording time included, and relays no approve (api.brokerForeignAccount).
func TestOutsideSignInABrokerTokenRecordingNoAccountDispatchNamesIsHeldToWhenItWasMade(t *testing.T) {
	for _, c := range []struct {
		name             string
		subject, issuer  string
		credentialsWhose string
		refusedWhenever  bool
	}{
		{name: "recording no account", credentialsWhose: "records no account"},
		{name: "recording an account of another issuer", subject: secondHolder, issuer: "https://another-issuer.example",
			credentialsWhose: "records an account of another issuer", refusedWhenever: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			s := newOutsideSignIn(t, nil)
			broker := newFakeBroker(t)
			handler, _ := s.brokerRouter(t, broker)
			s.workspaceSignIn(t, handler, reassignedEmail, firstHolder)
			var recordedAt time.Time
			if err := s.database.Pool.QueryRow(ctx, `select sub_recorded_at from people where email = $1`, reassignedEmail).Scan(&recordedAt); err != nil {
				t.Fatalf("read when the first holder's sign-in recorded its account: %v", err)
			}
			credential := func(id string, createdAt time.Time) (*ciphertokentest.Caller, string) {
				machine := ciphertokentest.NewCaller(t)
				token := "token-of-" + id
				broker.holds(&fakeCredential{id: id, person: reassignedEmail, subject: c.subject, issuer: c.issuer, tokens: []string{token}, createdAt: createdAt}, machine)
				return machine, token
			}
			refuse := func(when string, machine *ciphertokentest.Caller, token string) {
				t.Helper()
				requireBrokerTokenRecordsNoAccount(t, "GET /api/v1/whoami with a broker token whose credential "+c.credentialsWhose+", "+when,
					brokerTokenRequest(t, handler, machine, http.MethodGet, "/api/v1/whoami", "", token))
				requireBrokerTokenRecordsNoAccount(t, "the approve by a broker token whose credential "+c.credentialsWhose+", "+when,
					brokerTokenRequest(t, handler, machine, http.MethodPost, approvePath, approveBody, token))
			}
			for _, refused := range []struct {
				when    string
				created time.Time
			}{
				{"made an hour before the recording", recordedAt.Add(-time.Hour)},
				{"made at a time the broker did not record", time.Time{}},
			} {
				machine, token := credential("login-"+strings.ReplaceAll(refused.when, " ", "-"), refused.created)
				refuse(refused.when, machine, token)
			}
			requireCalls(t, broker, "after the refused approves by broker tokens whose credentials "+c.credentialsWhose)

			later, laterToken := credential("login-made-after-the-recording", recordedAt.Add(10*time.Minute))
			if c.refusedWhenever {
				refuse("made ten minutes after the recording", later, laterToken)
			} else {
				requireTokenActs(t, "GET /api/v1/whoami with a broker token whose credential "+c.credentialsWhose+", made ten minutes after the recording",
					brokerTokenRequest(t, handler, later, http.MethodGet, "/api/v1/whoami", "", laterToken))
				requireNoApproveScope(t, "the approve by a broker token whose credential "+c.credentialsWhose+", made after the recording",
					brokerTokenRequest(t, handler, later, http.MethodPost, approvePath, approveBody, laterToken))
			}
			requireCalls(t, broker, "after the approve by a broker token whose credential "+c.credentialsWhose+", made after the recording")

			storetest.SetRecordingTime(t, s.database, reassignedEmail, nil)
			older, olderToken := credential("login-from-before-the-column", recordedAt.Add(-time.Hour))
			if c.refusedWhenever {
				refuse("the address's row holding no recording time", older, olderToken)
			} else {
				requireTokenActs(t, "GET /api/v1/whoami with a broker token whose credential "+c.credentialsWhose+", the address's row holding no recording time",
					brokerTokenRequest(t, handler, older, http.MethodGet, "/api/v1/whoami", "", olderToken))
				requireNoApproveScope(t, "the approve by a broker token whose credential "+c.credentialsWhose+", no recording time",
					brokerTokenRequest(t, handler, older, http.MethodPost, approvePath, approveBody, olderToken))
			}
			requireCalls(t, broker, "after the approve by a broker token whose credential "+c.credentialsWhose+", no recording time")
		})
	}
}
