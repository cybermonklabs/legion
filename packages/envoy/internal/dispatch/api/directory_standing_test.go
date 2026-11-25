// directory_standing_test.go holds the API to IAP identity's Directory standing (R9, K11): a
// personal token whose owner Dispatch confirms through the Workspace Directory asks Standing at
// every use, and the person route carries the deadline of a not-held answer.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/dispatch/cipher"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestAnIAPDirectoryHoldRefusesBrokerCallersInTheDirectorysWords: under IAP identity
// (Standing.ChecksDirectory) a hold the Workspace Directory's answer recorded (RecordDirectoryHeld)
// refuses a broker token whose introspection answers its person not held, at its admission
// (brokerPersonAdmitted), a read and an approve alike, each 401 MEMBERSHIP_HELD in the Directory's
// words, since a sign-in releases nothing there. In cookie identity the same recorded hold keeps
// the sign-in's words. With no hold the approve is 403 OAUTH_SCOPE_REQUIRED, since no
// broker-issued token holds approve (brokerPersonScopes). Neither approve reaches the broker.
//
// Control: the admission answering the recorded hold in the sign-in's words whatever the identity,
// as it did before this row; its row fails naming the words.
func TestAnIAPDirectoryHoldRefusesBrokerCallersInTheDirectorysWords(t *testing.T) {
	t.Parallel()
	for _, mode := range []struct {
		name      string
		directory bool
		text      string
	}{
		{"under IAP identity", true, membershipHeldDirectoryText},
		{"in cookie identity", false, membershipHeldText},
	} {
		t.Run("a broker token, "+mode.name, func(t *testing.T) {
			rig := newBrokerTokenRig(t)
			seedPeople(t, rig.database, accessDave)
			caller := newBrokerCaller(t)
			const token = "daves-token-the-broker-answers-not-held"
			rig.broker.Says(token, heldPersonToken(caller.jkt, accessDave, false))
			served := rig.deps
			served.Standing = &standingFake{directory: mode.directory}
			handler := servedWith(t, served)
			whoami := func() *httptest.ResponseRecorder {
				t.Helper()
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, caller.presenting(t, http.MethodGet, "/api/v1/whoami", token, nil))
				return response
			}
			if response := whoami(); response.Code != http.StatusOK {
				t.Fatalf("dave's broker token before any hold, %s = %d %s, want 200", mode.name, response.Code, response.Body.String())
			}
			if err := peopleOf(rig.database).RecordDirectoryHeld(context.Background(), accessDave); err != nil {
				t.Fatal(err)
			}
			wantHeldRefusal(t, whoami(), mode.text, "dave's broker token with a Directory hold recorded, "+mode.name)
		})
		t.Run("an approver's broker token, "+mode.name, func(t *testing.T) {
			relay := newFakeBrokerRig(t)
			introspection := ciphertokentest.NewStubBroker(t)
			_, database, deps := newTestServer(t, testServerOptions{brokerTokens: stubBrokerTokens(t, introspection)})
			deps.AgentSecrets = cipher.New(relay.server.URL, accountTokenFile(t, credentialTestAccountToken))
			deps.Standing = &standingFake{directory: mode.directory}
			handler := servedWith(t, deps)
			seedPeople(t, database, accessDave)
			caller := newBrokerCaller(t)
			const token = "daves-machine-broker-token"
			introspection.Says(token, heldPersonToken(caller.jkt, accessDave, false))
			const path = "/api/v1/credential-requests/rec1/approve"
			request := func(body io.Reader) *http.Request {
				signed := caller.caller.Signed(t, http.MethodPost, testServerURL+path, token)
				request := httptest.NewRequest(http.MethodPost, path, body)
				request.RemoteAddr = brokerTestAddress
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Authorization", signed.Header.Get("Authorization"))
				request.Header.Set("DPoP", signed.Header.Get("DPoP"))
				return request
			}
			const decision = `{"code":"ABCD-1234"}`
			before := relay.calls
			// A broker token neither approves nor mints: no broker-issued token holds the approve
			// scope, whatever its person's standing, so with no hold the route refuses it 403
			// OAUTH_SCOPE_REQUIRED before it reads the body or calls the broker. The decision's own
			// re-read of a broker token's hold went with that rule, so this row no longer holds the
			// body back: the hold is read at the approve's admission, which answers before the scope.
			unheld := httptest.NewRecorder()
			handler.ServeHTTP(unheld, request(strings.NewReader(decision)))
			if refusal := decodeBody[refusalAnswer](t, unheld); unheld.Code != http.StatusForbidden || refusal.Code != codeScopeRequired {
				t.Fatalf("dave's approve by his broker token with no hold, %s = %d %s, want 403 %s", mode.name, unheld.Code, unheld.Body.String(), codeScopeRequired)
			}
			if err := peopleOf(database).RecordDirectoryHeld(context.Background(), accessDave); err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request(strings.NewReader(decision)))
			wantHeldRefusal(t, response, mode.text, "dave's approve by his broker token with a Directory hold recorded, "+mode.name)
			if relay.calls != before {
				t.Fatalf("dave's approves by his broker token, refused, reached the broker %d times, want none", relay.calls-before)
			}
		})
	}
}

// TestAnIAPPersonalTokenAsksStandingFirst: under IAP identity (Standing.ChecksDirectory) a personal
// token asks Standing at every use, before the fast paths over the refresh token and the sign-in
// confirmation its own statement reads, since IAP keeps neither. Dave's row is an IAP member's, with
// no refresh token: a held answer is 401 MEMBERSHIP_HELD in the Directory's words, an unavailable
// one 503 IDENTITY_UNAVAILABLE, a not-held one with a deadline after its read 200, and a Standing
// that fails 503; a hold recorded on his row is still answered through Standing. Each use asks
// Standing about dave once. In cookie identity carol's held answer keeps the sign-in's words.
//
// Control: ChecksDirectory answering false (personalTokenHeld ignoring it): dave's token takes the
// fast path for an owner with no refresh token, asks Standing nothing and is served, and the test
// fails naming the row.
func TestAnIAPPersonalTokenAsksStandingFirst(t *testing.T) {
	t.Parallel()
	_, database, deps := newTestServer(t, testServerOptions{})
	seedPeople(t, database, accessDave)
	daveToken := personalTokenOwnedBy(t, database, accessDave, model.ScopePerson)
	signInFor(t, database, accessCarol, "carol-account", "carol-refresh", model.MemberFull, 2*time.Hour)
	carolToken := personalTokenOwnedBy(t, database, accessCarol, model.ScopePerson)
	directory := &standingFake{directory: true}
	deps.Standing = directory
	iap := servedWith(t, deps)
	cookie := &standingFake{}
	cookieDeps := deps
	cookieDeps.Standing = cookie
	signedIn := servedWith(t, cookieDeps)

	at := time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC)
	const unavailableText = "whether this token's owner may still act could not be read; try again in a minute"
	rows := []struct {
		name     string
		before   func()
		standing identity.Standing
		err      error
		status   int
		code     string
		text     string
	}{
		{name: "held by the Directory", standing: identity.Standing{Held: true, AsOf: at},
			status: http.StatusUnauthorized, code: codeMembershipHeld, text: membershipHeldDirectoryText},
		{name: "unavailable", standing: identity.Standing{AsOf: at, NotHeldUntil: at},
			status: http.StatusServiceUnavailable, code: "IDENTITY_UNAVAILABLE", text: unavailableText},
		{name: "not held before its deadline", standing: identity.Standing{AsOf: at, NotHeldUntil: at.Add(time.Second)},
			status: http.StatusOK},
		{name: "a Standing that fails", err: identity.ErrMembershipUnreadable,
			status: http.StatusServiceUnavailable, code: "IDENTITY_UNAVAILABLE", text: unavailableText},
		{name: "a hold recorded on the row", standing: identity.Standing{Held: true, AsOf: at},
			before: func() {
				if err := peopleOf(database).RecordDirectoryHeld(context.Background(), accessDave); err != nil {
					t.Fatal(err)
				}
			},
			status: http.StatusUnauthorized, code: codeMembershipHeld, text: membershipHeldDirectoryText},
	}
	for _, row := range rows {
		if row.before != nil {
			row.before()
		}
		directory.answers(row.standing, row.err)
		before := len(directory.askedSoFar())
		response := agentRequest(t, iap, http.MethodGet, "/api/v1/whoami", nil, daveToken)
		var refusal refusalAnswer
		_ = json.Unmarshal(response.Body.Bytes(), &refusal)
		if response.Code != row.status || refusal.Code != row.code || refusal.Error != row.text {
			t.Errorf("dave's token under IAP, Standing %s = %d %s, want %d %s %q", row.name, response.Code, strings.TrimSpace(response.Body.String()), row.status, row.code, row.text)
		}
		if asked := directory.askedSoFar()[before:]; len(asked) != 1 || asked[0] != accessDave {
			t.Errorf("dave's token under IAP, Standing %s, asked Standing about %v; want %s once: IAP keeps no refresh token, so only Standing answers", row.name, asked, accessDave)
		}
	}

	cookie.answers(identity.Standing{Held: true, AsOf: at}, nil)
	wantHeldRefusal(t, agentRequest(t, signedIn, http.MethodGet, "/api/v1/whoami", nil, carolToken), membershipHeldText,
		"carol's token in cookie identity, her confirmation two hours old and Standing answering held")
}

// TestABrowserMintAndScopeChangeAskStandingUnderIAPAlone (browserDeciderHeld): under IAP identity
// (Standing.ChecksDirectory) a person at their browser, who presents no credential (here the
// trusted header, as IAP's assertion is there), mints a personal token and changes a token's
// scopes only while Standing serves them, asking it about them once: a held answer is 401
// MEMBERSHIP_HELD in the Directory's words, an unavailable one and a Standing that fails 503
// IDENTITY_UNAVAILABLE, and a not-held one with a deadline after its read serves. A personal
// token's mint there asks Standing once, its own check's (personalTokenHeld), and nothing more. In
// cookie identity a held answer refuses neither, and neither asks Standing.
//
// Controls: the hold asked for every caller with no credential whatever the identity fails the
// cookie identity's rows; asked for every caller under IAP, it fails the personal token's count.
func TestABrowserMintAndScopeChangeAskStandingUnderIAPAlone(t *testing.T) {
	t.Parallel()
	_, database, deps := newTestServer(t, testServerOptions{})
	seedPeople(t, database, accessDave)
	directory := &standingFake{directory: true}
	deps.Standing = directory
	iap := servedWith(t, deps)
	cookie := &standingFake{}
	cookieDeps := deps
	cookieDeps.Standing = cookie
	signedIn := servedWith(t, cookieDeps)
	mint := func(handler http.Handler) *httptest.ResponseRecorder {
		t.Helper()
		return dispatchRequest(t, handler, http.MethodPost, "/api/v1/me/agent-tokens", map[string]any{"name": "dave's token", "scopes": []string{model.ScopePerson}}, accessDave)
	}
	minted := mint(iap)
	var token struct {
		ID string `json:"id"`
	}
	if minted.Code != http.StatusCreated || json.Unmarshal(minted.Body.Bytes(), &token) != nil || token.ID == "" {
		t.Fatalf("dave's mint at his browser under IAP, Standing answering not held = %d %s, want 201 with a token", minted.Code, minted.Body.String())
	}
	widen := func(handler http.Handler) *httptest.ResponseRecorder {
		t.Helper()
		return dispatchRequest(t, handler, http.MethodPatch, "/api/v1/me/agent-tokens/"+token.ID, map[string]any{"scopes": []string{model.ScopePerson, model.ScopeApprove}}, accessDave)
	}

	at := time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC)
	const unreadableText = "whether this caller may still decide could not be read; try again in a minute"
	for _, row := range []struct {
		name     string
		standing identity.Standing
		err      error
		served   bool
		status   int
		code     string
		text     string
	}{
		{name: "held by the Directory", standing: identity.Standing{Held: true, AsOf: at},
			status: http.StatusUnauthorized, code: codeMembershipHeld, text: membershipHeldDeciderDirectoryText},
		{name: "unavailable", standing: identity.Standing{AsOf: at, NotHeldUntil: at},
			status: http.StatusServiceUnavailable, code: "IDENTITY_UNAVAILABLE", text: unreadableText},
		{name: "a Standing that fails", err: identity.ErrMembershipUnreadable,
			status: http.StatusServiceUnavailable, code: "IDENTITY_UNAVAILABLE", text: unreadableText},
		{name: "not held before its deadline", standing: identity.Standing{AsOf: at, NotHeldUntil: at.Add(time.Second)}, served: true},
	} {
		directory.answers(row.standing, row.err)
		for _, use := range []struct {
			name   string
			call   func(http.Handler) *httptest.ResponseRecorder
			served int
		}{
			{"mint", mint, http.StatusCreated},
			{"widening", widen, http.StatusOK},
		} {
			before := len(directory.askedSoFar())
			response := use.call(iap)
			var refusal refusalAnswer
			_ = json.Unmarshal(response.Body.Bytes(), &refusal)
			status, code, text := row.status, row.code, row.text
			if row.served {
				status = use.served
			}
			if response.Code != status || refusal.Code != code || refusal.Error != text {
				t.Errorf("dave's %s at his browser under IAP, Standing %s = %d %s, want %d %s %q", use.name, row.name, response.Code, strings.TrimSpace(response.Body.String()), status, code, text)
			}
			if asked := directory.askedSoFar()[before:]; len(asked) != 1 || asked[0] != accessDave {
				t.Errorf("dave's %s at his browser under IAP, Standing %s, asked Standing about %v; want %s once", use.name, row.name, asked, accessDave)
			}
		}
	}

	daveToken := personalTokenOwnedBy(t, database, accessDave, model.ScopePerson, model.ScopeApprove)
	before := len(directory.askedSoFar())
	if response := agentRequest(t, iap, http.MethodPost, "/api/v1/me/agent-tokens", map[string]any{"name": "minted by dave's token", "scopes": []string{model.ScopePerson}}, daveToken); response.Code != http.StatusCreated {
		t.Errorf("a mint by dave's token under IAP, Standing answering not held = %d %s, want 201", response.Code, response.Body.String())
	}
	if asked := directory.askedSoFar()[before:]; len(asked) != 1 || asked[0] != accessDave {
		t.Errorf("a mint by dave's token under IAP asked Standing about %v; want %s once: the token's own check asks, and the browser's hold is not the token's", asked, accessDave)
	}

	cookie.answers(identity.Standing{Held: true, AsOf: at}, nil)
	if response := mint(signedIn); response.Code != http.StatusCreated {
		t.Errorf("dave's mint at his browser in cookie identity, Standing answering held = %d %s, want 201: a hold there ends a cookie's session", response.Code, response.Body.String())
	}
	if response := widen(signedIn); response.Code != http.StatusOK {
		t.Errorf("dave's widening at his browser in cookie identity, Standing answering held = %d %s, want 200: a hold there ends a cookie's session", response.Code, response.Body.String())
	}
	if asked := cookie.askedSoFar(); len(asked) != 0 {
		t.Errorf("dave's mint and widening at his browser in cookie identity asked Standing about %v; want nothing", asked)
	}
}

// TestThePersonAnswerCarriesNotHeldUntil (K11): the person route carries Standing's NotHeldUntil as
// not_held_until on a not-held answer under IAP identity; an unavailable answer is held false with
// not_held_until equal to as_of; and a held answer, and every answer outside IAP, leave it out.
func TestThePersonAnswerCarriesNotHeldUntil(t *testing.T) {
	t.Parallel()
	issuer := newServiceTokenIssuer(t)
	_, database, deps := newTestServer(t, testServerOptions{oidc: issuer.verifier(t), accessAccounts: []string{listedServiceAccount}})
	bearer := issuer.Mint(t, issuer.signing, issuer.Claims(listedServiceAccount, serviceTokenAudience))
	seedPeople(t, database, accessDave)
	at := time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC)
	deadline := at.Add(23 * time.Hour)
	for _, row := range []struct {
		name     string
		standing identity.Standing
		held     bool
		until    *time.Time
	}{
		{"not held under IAP", identity.Standing{AsOf: at, NotHeldUntil: deadline}, false, &deadline},
		{"unavailable under IAP", identity.Standing{AsOf: at, NotHeldUntil: at}, false, &at},
		{"held", identity.Standing{Held: true, AsOf: at}, true, nil},
		{"not held outside IAP", identity.Standing{AsOf: at}, false, nil},
	} {
		fake := &standingFake{directory: row.until != nil}
		fake.answers(row.standing, nil)
		served := deps
		served.Standing = fake
		code, fields, answer := personAnswerOf(t, servedWith(t, served), bearer, accessDave)
		_, carries := fields["not_held_until"]
		untilOK := (row.until == nil && !carries) || (row.until != nil && carries && answer.NotHeldUntil != nil && answer.NotHeldUntil.Equal(*row.until))
		if code != http.StatusOK || answer.Held == nil || *answer.Held != row.held || !answer.AsOf.Equal(at) || !untilOK {
			t.Errorf("%s: the person answer of %s = %d %v; want held %t, as_of %s and not_held_until %s",
				row.name, accessDave, code, fields, row.held, at.Format(time.RFC3339), describeUntil(row.until))
		}
	}
	failing := &standingFake{directory: true}
	failing.answers(identity.Standing{}, errors.New("read membership: the database did not answer"))
	served := deps
	served.Standing = failing
	if _, fields, _ := personAnswerOf(t, servedWith(t, served), bearer, accessDave); fields["held"] != nil || fields["not_held_until"] != nil {
		t.Errorf("with Standing failing under IAP the person answer = %v; want held and not_held_until left out", fields)
	}
}

// describeUntil is a wanted not_held_until as a failure names it.
func describeUntil(until *time.Time) string {
	if until == nil {
		return "left out"
	}
	return until.Format(time.RFC3339)
}
