package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A machine login's approve mints a credential at the secrets broker in the approver's name, so
// Dispatch relays it only while its caller still holds the address, read again once the request's
// body has arrived, and names the Google account that approves, so the broker can end the login
// with that account. A deny is held to the same re-read, and names no account.

// approvePath is the approve the tests relay: a machine login's, rec1.
const approvePath = "/api/v1/credential-requests/rec1/approve"

// denyPath is the deny the tests relay: the same machine login's.
const denyPath = "/api/v1/credential-requests/rec1/deny"

// approveBody is the body of an approve or a deny, the code the person typed.
const approveBody = `{"code":"ABCD-1234"}`

// approveCredentials are the two credentials a person approves with: their cookie and a personal
// token they minted on it, each answered as the caller's next request is once the caller no longer
// holds the address. A broker token approves, denies and mints nothing: none holds the approve
// scope (TestOutsideSignInABrokerTokenApprovesDeniesAndMintsNothing).
var approveCredentials = []struct{ name, refusal string }{
	{"cookie", "NO_IDENTITY"},
	{"personal token", "TOKEN_REVOKED"},
}

// requireNoApproveScope fails the test unless recorder is the refusal of a credential that holds
// no approve scope, as every broker token is refused at an approve, a deny and a mint: 403
// OAUTH_SCOPE_REQUIRED.
func requireNoApproveScope(t *testing.T, what string, recorder *httptest.ResponseRecorder) {
	t.Helper()
	if recorder.Code != http.StatusForbidden || errorCode(recorder) != "OAUTH_SCOPE_REQUIRED" {
		t.Errorf("%s answered %d %s, want 403 OAUTH_SCOPE_REQUIRED", what, recorder.Code, recorder.Body.String())
	}
}

// approver is the first holder of reassignedEmail signed in through handler, holding credential:
// it answers a request of the decision at path (approvePath or denyPath) carrying body, which the
// caller sends as that credential. Each of beforeCredential runs after the sign-in and before the
// credential is made. A broker token's machine login records the account the address's row
// records when it is made, the one Dispatch's approve of that login named, none for none, and is
// made now by the broker's clock.
func (s *outsideSignIn) approver(t *testing.T, handler http.Handler, broker *fakeBroker, credential string, beforeCredential ...func()) func(path string, body io.Reader) *http.Request {
	t.Helper()
	cookie := s.workspaceSignIn(t, handler, reassignedEmail, firstHolder)
	for _, step := range beforeCredential {
		step()
	}
	var token string
	var machine *ciphertokentest.Caller
	switch credential {
	case "personal token":
		token = mintTokenOn(t, handler, cookie)
	case "broker token":
		var approvedBy string
		if err := s.database.Pool.QueryRow(context.Background(), `select coalesce(sub, '') from people where email = $1`, reassignedEmail).Scan(&approvedBy); err != nil {
			t.Fatal(err)
		}
		machine, token = ciphertokentest.NewCaller(t), "token-of-the-first-holders-machine"
		broker.holds(&fakeCredential{id: "login-first", person: reassignedEmail, subject: approvedBy, tokens: []string{token}, createdAt: time.Now()}, machine)
	}
	return func(path string, body io.Reader) *http.Request {
		request := httptest.NewRequest(http.MethodPost, brokerOrigin+path, body)
		request.ContentLength = int64(len(approveBody))
		request.Header.Set("Content-Type", "application/json")
		switch credential {
		case "cookie":
			request.Header.Set("Origin", brokerOrigin)
			for _, c := range cookie.Result().Cookies() {
				if c.Name == "dsession" && c.Value != "" {
					request.AddCookie(c)
				}
			}
		case "personal token":
			request.Header.Set("Authorization", "Bearer "+token)
		case "broker token":
			signed := machine.Signed(t, http.MethodPost, brokerOrigin+path, token)
			request.Header.Set("Authorization", signed.Header.Get("Authorization"))
			request.Header.Set("DPoP", signed.Header.Get("DPoP"))
		}
		return request
	}
}

// heldBody is a request body whose first read waits, after saying it began, until the test lets
// it go: a client that sent its headers and holds its body back.
type heldBody struct {
	body             io.Reader
	reading, release chan struct{}
	once             sync.Once
}

func newHeldBody(body string) *heldBody {
	return &heldBody{body: strings.NewReader(body), reading: make(chan struct{}), release: make(chan struct{})}
}

func (b *heldBody) Read(p []byte) (int, error) {
	b.once.Do(func() {
		close(b.reading)
		<-b.release
	})
	return b.body.Read(p)
}

// serveHeld serves request through handler while the client holds body back: once Dispatch waits on
// the body it runs whileHeld, then lets the body go and answers the response.
func serveHeld(t *testing.T, handler http.Handler, request *http.Request, body *heldBody, whileHeld func()) *httptest.ResponseRecorder {
	t.Helper()
	recorder, answered := httptest.NewRecorder(), make(chan struct{})
	go func() {
		defer close(answered)
		handler.ServeHTTP(recorder, request)
	}()
	var release sync.Once
	defer release.Do(func() { close(body.release) })
	select {
	case <-body.reading:
	case <-answered:
		t.Fatalf("%s %s answered %d %s before reading its body", request.Method, request.URL.Path, recorder.Code, recorder.Body.String())
	case <-time.After(30 * time.Second):
		t.Fatalf("%s %s did not begin reading its body within 30 s", request.Method, request.URL.Path)
	}
	whileHeld()
	release.Do(func() { close(body.release) })
	<-answered
	return recorder
}

// An approve whose caller resolved before its body arrived, and lost the address while the client
// held the body back, never reaches the broker: the first holder sends an approve's headers by
// each credential, unkeyed and under an Idempotency-Key (whose layer resolves the caller before it
// spools the body), and while Dispatch waits on the body Google gives the address to another
// Workspace account, which signs in. Once the body arrives the approve is refused 401 as the
// caller's next request would be, and the broker is told nothing.
func TestOutsideSignInApproveRefusesACallerWhoLostTheAddressWhileItsBodyArrived(t *testing.T) {
	for _, credential := range approveCredentials {
		for _, keyed := range []bool{false, true} {
			t.Run(credential.name+" keyed "+strconv.FormatBool(keyed), func(t *testing.T) {
				s := newOutsideSignIn(t, nil)
				broker := newFakeBroker(t)
				handler, _ := s.brokerRouter(t, broker)
				body := newHeldBody(approveBody)
				request := s.approver(t, handler, broker, credential.name)(approvePath, body)
				if keyed {
					request.Header.Set("Idempotency-Key", "approve-while-the-address-passes-on")
				}
				recorder := serveHeld(t, handler, request, body, func() {
					s.workspaceSignIn(t, handler, reassignedEmail, secondHolder)
				})

				if recorder.Code != http.StatusUnauthorized || errorCode(recorder) != credential.refusal {
					t.Errorf("the approve by the first holder's %s, whose body arrived after the address passed on, answered %d %s, want 401 %s",
						credential.name, recorder.Code, recorder.Body.String(), credential.refusal)
				}
				for _, call := range broker.calls() {
					if strings.Contains(call, "/approve") {
						t.Errorf("the approve by the first holder's %s, whose body arrived after the address passed on, was relayed to the broker: %s", credential.name, call)
					}
				}
				if reads := broker.settingsRead(); reads != 0 {
					t.Errorf("the refused approve by the first holder's %s read the broker's settings %d times, want none: a refused caller asks the broker nothing", credential.name, reads)
				}
			})
		}
	}
}

// A deny is read again as an approve is: the broker decides a machine login's denial whatever
// account asks, so a deny relayed for a caller who lost the address would let the replaced account
// deny the new holder's pending machine login. The first holder sends a deny's headers by each
// credential, unkeyed and under an Idempotency-Key, and while Dispatch waits on the body Google
// gives the address to another Workspace account, which signs in. Once the body arrives the deny is
// refused 401 as the caller's next request would be, and the broker is told no deny.
func TestOutsideSignInDenyRefusesACallerWhoLostTheAddressWhileItsBodyArrived(t *testing.T) {
	for _, credential := range approveCredentials {
		for _, keyed := range []bool{false, true} {
			t.Run(credential.name+" keyed "+strconv.FormatBool(keyed), func(t *testing.T) {
				s := newOutsideSignIn(t, nil)
				broker := newFakeBroker(t)
				handler, _ := s.brokerRouter(t, broker)
				body := newHeldBody(approveBody)
				request := s.approver(t, handler, broker, credential.name)(denyPath, body)
				if keyed {
					request.Header.Set("Idempotency-Key", "deny-while-the-address-passes-on")
				}
				recorder := serveHeld(t, handler, request, body, func() {
					s.workspaceSignIn(t, handler, reassignedEmail, secondHolder)
				})

				if recorder.Code != http.StatusUnauthorized || errorCode(recorder) != credential.refusal {
					t.Errorf("the deny by the first holder's %s, whose body arrived after the address passed on, answered %d %s, want 401 %s",
						credential.name, recorder.Code, recorder.Body.String(), credential.refusal)
				}
				for _, call := range broker.calls() {
					if strings.Contains(call, "/deny") {
						t.Errorf("the deny by the first holder's %s, whose body arrived after the address passed on, was relayed to the broker: %s", credential.name, call)
					}
				}
			})
		}
	}
}

// A deny by a caller who still holds the address is relayed, whichever credential it acts with,
// and names the approver and the code alone: a denial mints nothing, so it names no Google account
// (no approver_issuer, no approver_subject), though the re-read that let it through read one.
func TestOutsideSignInDenyByTheHolderNamesNoAccount(t *testing.T) {
	for _, credential := range approveCredentials {
		t.Run(credential.name, func(t *testing.T) {
			s := newOutsideSignIn(t, nil)
			broker := newFakeBroker(t)
			handler, _ := s.brokerRouter(t, broker)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, s.approver(t, handler, broker, credential.name)(denyPath, strings.NewReader(approveBody)))
			if recorder.Code != http.StatusOK {
				t.Fatalf("the deny by the holder's %s answered %d %s, want 200", credential.name, recorder.Code, recorder.Body.String())
			}
			requireCalls(t, broker, "after the deny by the holder's "+credential.name,
				`POST /v1/credential-requests/rec1/deny {"approver":"`+reassignedEmail+`","code":"ABCD-1234"}`)
		})
	}
}

// An approve held back past the whole revocation is still refused, whichever credential it was sent
// with: the first holder sends an approve's headers by each credential, and while Dispatch waits on
// the body Google gives the address to another Workspace account, which signs in; the revocation
// pass tells the broker to revoke the first holder's account and settles, and an hour passes. An
// approve relayed then would mint a machine login in the address's name for a caller that lost it.
// Each approve is refused 401 as the caller's next request would be, and the broker is told no
// approve.
func TestOutsideSignInAnApproveHeldPastTheRevocationIsNotRelayed(t *testing.T) {
	for _, credential := range approveCredentials {
		t.Run(credential.name, func(t *testing.T) {
			ctx := context.Background()
			s := newOutsideSignIn(t, nil)
			broker := newFakeBroker(t)
			handler, background := s.brokerRouter(t, broker)
			body := newHeldBody(approveBody)
			request := s.approver(t, handler, broker, credential.name)(approvePath, body)
			recorder := serveHeld(t, handler, request, body, func() {
				s.workspaceSignIn(t, handler, reassignedEmail, secondHolder)
				background.RevokeBrokerCredentials(ctx)
				if recorded, done := s.brokerRevocation(t, reassignedEmail); !recorded || !done {
					t.Fatalf("the revocation of %s after the pass reads recorded %t, done %t; want it settled", reassignedEmail, recorded, done)
				}
				s.settledAgo(t, reassignedEmail, time.Hour)
			})

			if recorder.Code != http.StatusUnauthorized || errorCode(recorder) != credential.refusal {
				t.Errorf("the first holder's %s approve, whose body arrived an hour after the revocation settled, answered %d %s, want 401 %s",
					credential.name, recorder.Code, recorder.Body.String(), credential.refusal)
			}
			for _, call := range broker.calls() {
				if strings.Contains(call, "/approve") {
					t.Errorf("the first holder's %s approve, whose body arrived an hour after the revocation settled, was relayed to the broker: %s", credential.name, call)
				}
			}
			if reads := broker.settingsRead(); reads != 0 {
				t.Errorf("the refused approve by the first holder's %s read the broker's settings %d times, want none: a refused caller asks the broker nothing", credential.name, reads)
			}
		})
	}
}

// A personal token records the Google account of the credential that minted it, and acts only while
// Dispatch records that account for its address. The first holder mints a token on its cookie and
// a second token with that token: each records the first holder's account; its machine's broker
// token mints nothing (403 OAUTH_SCOPE_REQUIRED: no broker token holds approve). Then the
// address's row comes to record no account (as a pool or header sign-in leaves it), and the second
// holder signs in, which records the address's first account in a while: that sign-in revokes the
// address's tokens that recorded another account, both of the first holder's
// (store.PgPeopleStore.SignInSubject). The first holder's token is refused 401 TOKEN_REVOKED at its
// approve, and the broker is told no approve; a token the second holder mints records the second's
// account.
func TestOutsideSignInAnApproveNamesTheAccountOfTheCredentialThatMadeIt(t *testing.T) {
	ctx := context.Background()
	s := newOutsideSignIn(t, nil)
	broker := newFakeBroker(t)
	handler, _ := s.brokerRouter(t, broker)
	cookie := s.workspaceSignIn(t, handler, reassignedEmail, firstHolder)
	token := mintTokenOn(t, handler, cookie)
	if minted := bearerRequest(handler, http.MethodPost, "/api/v1/me/agent-tokens", `{"name":"minted by a token"}`, token); minted.Code != http.StatusCreated {
		t.Fatalf("a mint by the first holder's token answered %d %s, want 201", minted.Code, minted.Body.String())
	}
	machine := ciphertokentest.NewCaller(t)
	broker.holds(&fakeCredential{id: "login-first", person: reassignedEmail, subject: firstHolder, tokens: []string{"token-of-the-first-holders-machine"}}, machine)
	requireNoApproveScope(t, "a mint by the first holder's broker token",
		brokerTokenRequest(t, handler, machine, http.MethodPost, "/api/v1/me/agent-tokens", `{"name":"minted by a broker token"}`, "token-of-the-first-holders-machine"))
	subjects := func() []string {
		t.Helper()
		rows, err := s.database.Pool.Query(ctx, `select coalesce(subject, '') from agent_tokens order by created_at, id`)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for rows.Next() {
			var subject string
			if err := rows.Scan(&subject); err != nil {
				t.Fatal(err)
			}
			got = append(got, subject)
		}
		return got
	}
	if got := subjects(); strings.Join(got, ",") != firstHolder+","+firstHolder {
		t.Fatalf("the tokens minted by the first holder's cookie and token recorded the accounts %q, want the first holder's on each, and none minted by the broker token", got)
	}

	storetest.TestOnlyClearAccount(t, s.database, reassignedEmail)
	second := s.workspaceSignIn(t, handler, reassignedEmail, secondHolder)
	var live int
	if err := s.database.Pool.QueryRow(ctx, `select count(*) from agent_tokens where subject = $1 and revoked_at is null`, firstHolder).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != 0 {
		t.Fatalf("%d tokens recording the first holder's account are live after the second holder's sign-in recorded itself for the address, want both revoked", live)
	}
	approve := httptest.NewRequest(http.MethodPost, brokerOrigin+approvePath, strings.NewReader(approveBody))
	approve.Header.Set("Content-Type", "application/json")
	approve.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, approve)
	if recorder.Code != http.StatusUnauthorized || errorCode(recorder) != "TOKEN_REVOKED" {
		t.Fatalf("the approve by the first holder's token, once the second holder's sign-in recorded itself, answered %d %s, want 401 TOKEN_REVOKED", recorder.Code, recorder.Body.String())
	}
	for _, call := range broker.calls() {
		if strings.Contains(call, "/approve") {
			t.Fatalf("the refused approve by the first holder's token was relayed to the broker: %s", call)
		}
	}
	mintTokenOn(t, handler, second)
	if got := subjects(); len(got) != 3 || got[2] != secondHolder {
		t.Fatalf("the token the second holder minted recorded %q, want the second holder's account", got)
	}
}

// An approve names the Google account that approves, so the broker records it on the machine login
// it mints: Dispatch's sign-in issuer and the `sub` people records for the approver, beside the
// approver, the code and the session generation its check read, whichever credential the approver
// acts with.
func TestOutsideSignInApproveNamesTheApprovingAccount(t *testing.T) {
	for _, credential := range approveCredentials {
		t.Run(credential.name, func(t *testing.T) {
			s := newOutsideSignIn(t, nil)
			broker := newFakeBroker(t)
			handler, _ := s.brokerRouter(t, broker)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, s.approver(t, handler, broker, credential.name)(approvePath, strings.NewReader(approveBody)))
			if recorder.Code != http.StatusOK {
				t.Fatalf("the approve by the holder's %s answered %d %s, want 200", credential.name, recorder.Code, recorder.Body.String())
			}
			requireCalls(t, broker, "after the approve by the holder's "+credential.name, s.approveRelayed(t, firstHolder))
			// The control of the refused approves' settings counts: the approve that is relayed reads
			// the broker's settings once, on a router whose client has read none.
			if reads := broker.settingsRead(); reads != 1 {
				t.Errorf("the approve by the holder's %s read the broker's settings %d times, want once", credential.name, reads)
			}
		})
	}
}

// An approver whose credential is bound to no Google account (made while the address's row recorded
// none, as a row from before Dispatch recorded one is) sends neither approver_issuer nor
// approver_subject: the broker takes both or neither and refuses one alone 400
// APPROVER_ACCOUNT_INPUT, so Dispatch never sends the issuer by itself.
func TestOutsideSignInApproveOfARowWithNoAccountSendsNeitherAccountField(t *testing.T) {
	for _, credential := range approveCredentials {
		t.Run(credential.name, func(t *testing.T) {
			s := newOutsideSignIn(t, nil)
			broker := newFakeBroker(t)
			handler, _ := s.brokerRouter(t, broker)
			request := s.approver(t, handler, broker, credential.name, func() {
				storetest.TestOnlyClearAccount(t, s.database, reassignedEmail)
			})
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request(approvePath, strings.NewReader(approveBody)))
			if recorder.Code != http.StatusOK {
				t.Fatalf("the approve by the %s of a row with no account answered %d %s, want 200", credential.name, recorder.Code, recorder.Body.String())
			}
			requireCalls(t, broker, "after the approve by the "+credential.name+" of a row with no account", s.approveRelayed(t, ""))
		})
	}
}

// A broker token approves, denies and mints nothing, whoever holds the address: no broker token
// holds the approve scope (brokerPersonScopes), so the holder's own broker token, which records the
// account Dispatch records for the address and acts at a read, is refused 403
// OAUTH_SCOPE_REQUIRED at a machine login's approve, at its deny and at a personal token's mint.
// The broker is told no decision and no token is minted.
//
// Control: brokerPersonScopes holding approve beside person (each answers 200 or 201, the
// decisions are relayed and the token is written).
func TestOutsideSignInABrokerTokenApprovesDeniesAndMintsNothing(t *testing.T) {
	s := newOutsideSignIn(t, nil)
	broker := newFakeBroker(t)
	handler, _ := s.brokerRouter(t, broker)
	s.workspaceSignIn(t, handler, reassignedEmail, firstHolder)
	machine := ciphertokentest.NewCaller(t)
	const token = "token-of-the-holders-machine"
	broker.holds(&fakeCredential{id: "login-holder", person: reassignedEmail, subject: firstHolder, tokens: []string{token}, createdAt: time.Now()}, machine)
	requireTokenActs(t, "GET /api/v1/whoami with the holder's broker token",
		brokerTokenRequest(t, handler, machine, http.MethodGet, "/api/v1/whoami", "", token))
	const name = "minted by the holder's broker token"
	for _, c := range []struct{ what, path, body string }{
		{"the approve", approvePath, approveBody},
		{"the deny", denyPath, approveBody},
		{"the mint", "/api/v1/me/agent-tokens", `{"name":"` + name + `"}`},
	} {
		requireNoApproveScope(t, c.what+" by the holder's broker token", brokerTokenRequest(t, handler, machine, http.MethodPost, c.path, c.body, token))
	}
	requireCalls(t, broker, "after the approve, the deny and the mint by the holder's broker token")
	if rows := s.tokenRowsNamed(t, name); rows != 0 {
		t.Errorf("the refused mint by the holder's broker token wrote %d token rows, want none", rows)
	}
}
