// access_read_token_test.go holds Dispatch's access routes, the read class accessAccount, to the
// one credential the secrets broker asks them with: a read token the broker issued to itself,
// whose service DISPATCH_ACCESS_ACCOUNTS lists, bound to the broker's DPoP key. Such a token calls
// the routes of that class and no other route; another read service's token calls none of them;
// a token without its valid proof is refused; and with no DISPATCH_OIDC_* setting nothing else is
// admitted there.
package api

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/brokertoken"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/readclass"
)

const (
	// brokerAccessService is the service the secrets broker's own read entry names: the broker's
	// Kubernetes account, the one DISPATCH_ACCESS_ACCOUNTS lists in production.
	brokerAccessService = "system:serviceaccount:dispatch:envoy-broker"
	// otherReadService is another read entry's service, one DISPATCH_ACCESS_ACCOUNTS does not
	// list: an evaluation site that reads issues.
	otherReadService = "system:serviceaccount:evals:eval-site"
)

// readTokenOf is the broker's answer for a read-only token bound to jkt whose read entry names
// service.
func readTokenOf(jkt, service string) brokertoken.Introspection {
	answer := readOnlyToken(jkt)
	answer.Service = service
	return answer
}

// accessRoutePaths are the three access routes as the broker asks them, each at a path its
// handler answers 200 for a caller it admits.
var accessRoutePaths = []string{
	"/api/v1/access/person?person=alice",
	"/api/v1/access/projects/ACC?person=alice",
	"/api/v1/access/private-projects",
}

// newAccessRouteRig is the broker-token rig with the broker's own service listed in
// DISPATCH_ACCESS_ACCOUNTS and no DISPATCH_OIDC_* setting (testServerOptions.oidc nil), holding the
// project ACC that accessRoutePaths names.
func newAccessRouteRig(t *testing.T) *brokerTokenRig {
	t.Helper()
	rig := newBrokerTokenRigOn(t, testServerOptions{accessAccounts: []string{brokerAccessService}})
	if created := rig.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{"key": "ACC", "name": "Access"}); created.Code != http.StatusCreated {
		t.Fatalf("create ACC: status=%d body=%s", created.Code, created.Body.String())
	}
	return rig
}

// wantAnswered fails t unless response is 200, naming who called and where.
func wantAnswered(t *testing.T, response *httptest.ResponseRecorder, who, path string) {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("%s: GET %s = %d %s, want 200", who, path, response.Code, response.Body.String())
	}
}

// TestBrokerAccessReadTokenOnlyCallsTheAccessRoutes walks every row of the table, test hooks in,
// through the chain register builds (routeHandler) with each row's handler replaced by a recorder,
// presenting the read token of the service DISPATCH_ACCESS_ACCOUNTS lists with a fresh proof each
// time. Every row of the access-account class, as the table stamps it (apiRoute.Reads), runs its
// handler once; every other row, the read list's among them, answers 403 READ_ONLY_TOKEN before
// its handler, and so does GET /ws/live. The class is held to the three access routes by name, so
// a route dropped from the class or added to it fails too.
//
// Controls: refuseReadOnlyToken reading the read list alone (route.ReadScope) for every token, as
// before: each access route refuses the token. refuseReadOnlyToken admitting a listed service on
// every route the read list or the class names: the read list's three run their handlers.
func TestBrokerAccessReadTokenOnlyCallsTheAccessRoutes(t *testing.T) {
	rig := newBrokerTokenRigOn(t, testServerOptions{testHooks: true, accessAccounts: []string{brokerAccessService}})
	caller := newBrokerCaller(t)
	const token = "the-brokers-own-read-token-walking-every-chain"
	rig.broker.Says(token, readTokenOf(caller.jkt, brokerAccessService))

	replays := rig.served.keyedReplays()
	walked := 0
	var admitted []string
	for _, route := range readOnlyTable() {
		name := route.Method + " " + route.Pattern
		ran := 0
		route.Handler = func(w http.ResponseWriter, _ *http.Request) {
			ran++
			w.WriteHeader(http.StatusNoContent)
		}
		response := httptest.NewRecorder()
		rig.served.routeHandler(route, replays[name]).ServeHTTP(response, caller.presentingProbe(t, route, token))
		if slices.Contains(route.Reads, readclass.AccessAccount) {
			admitted = append(admitted, name)
			if ran != 1 || response.Code != http.StatusNoContent {
				t.Errorf("%s is of the access-account class, and the broker's read token ran its handler %d times (status=%d body=%s), want once",
					name, ran, response.Code, response.Body.String())
			}
		} else {
			if ran != 0 {
				t.Errorf("%s is not of the access-account class, and the broker's read token ran its handler %d times, want none", name, ran)
			}
			if why := notTheReadOnlyRefusal(response); why != "" {
				t.Errorf("%s %s, want 403 %s", name, why, contracts.DispatchReadOnlyToken)
			}
		}
		walked++
	}

	socketOpened := 0
	live := rig.served.liveSocketHandler(func(http.ResponseWriter, *http.Request) { socketOpened++ })
	response := httptest.NewRecorder()
	live.ServeHTTP(response, caller.presenting(t, http.MethodGet, liveSocketPattern, token, nil))
	if socketOpened != 0 {
		t.Errorf("GET %s opened the socket %d times for the broker's read token, want none", liveSocketPattern, socketOpened)
	}
	if why := notTheReadOnlyRefusal(response); why != "" {
		t.Errorf("GET %s %s, want 403 %s", liveSocketPattern, why, contracts.DispatchReadOnlyToken)
	}

	if mounted := len(rig.served.routeIndex); walked == 0 || walked != mounted {
		t.Fatalf("walked %d rows, want every one of the %d the server mounts", walked, mounted)
	}
	slices.Sort(admitted)
	if want := []string{"GET /api/v1/access/person", "GET /api/v1/access/private-projects", "GET /api/v1/access/projects/{key}"}; !slices.Equal(admitted, want) {
		t.Fatalf("the access-account class holds %q, want the three access routes %q", admitted, want)
	}
	t.Logf("walked %d rows of the table (test hooks in) and GET %s: %d of the access-account class admitted, %d refused %s",
		walked, liveSocketPattern, len(admitted), walked-len(admitted)+1, contracts.DispatchReadOnlyToken)
}

// TestOtherReadServiceCannotCallTheAccessRoutes: on each access route, the read token of the
// service DISPATCH_ACCESS_ACCOUNTS lists is answered first, so the verifier and the routes are
// shown to admit what they should; then the read token of another read entry's service, an
// evaluation site's, is refused 403 READ_ONLY_TOKEN, as are a read token of the listed service
// presented by a caller of another key (401) and the listed service's token of a shape that is not
// read-only (403 ACCESS_ACCOUNT_REQUIRED).
//
// Control: requireAccessAccount's comparison with DISPATCH_ACCESS_ACCOUNTS removed for a read
// token (any read-only service admitted): the evaluation site's token is answered 200.
func TestOtherReadServiceCannotCallTheAccessRoutes(t *testing.T) {
	rig := newAccessRouteRig(t)
	caller := newBrokerCaller(t)
	const listed, other, writing = "the-brokers-own-read-token", "an-eval-sites-read-token", "the-brokers-service-token-that-is-not-read-only"
	rig.broker.Says(listed, readTokenOf(caller.jkt, brokerAccessService))
	rig.broker.Says(other, readTokenOf(caller.jkt, otherReadService))
	notReadOnly := serviceOnlyToken(caller.jkt)
	notReadOnly.Service = brokerAccessService
	rig.broker.Says(writing, notReadOnly)
	for _, path := range accessRoutePaths {
		wantAnswered(t, rig.serve(caller.presenting(t, http.MethodGet, path, listed, nil)), "the broker's read token", path)
		wantRefusal(t, rig.serve(caller.presenting(t, http.MethodGet, path, other, nil)),
			http.StatusForbidden, contracts.DispatchReadOnlyToken, otherReadService+"'s read token", "GET "+path)
		wantRefusal(t, rig.serve(caller.presenting(t, http.MethodGet, path, writing, nil)),
			http.StatusForbidden, "ACCESS_ACCOUNT_REQUIRED", "a token of the listed service that is not read-only", "GET "+path)
	}
}

// TestBrokerAccessReadTokenNeedsItsProof: the read token of the listed service is admitted on the
// person route with a sound proof, which is the control that the verifier admits at all; then the
// same token is refused with no proof, under the Bearer scheme, with a proof of another key, for
// another origin, for another method, naming another token (ath), with a proof already spent, and
// once the broker no longer holds it live. A token with no proof is refused on each of the three
// access routes.
//
// Control: requireAccessAccount reading the token's service off the Authorization header without
// the verified caller (any DPoP presentation naming the listed service admitted): the refusals
// answer 200.
func TestBrokerAccessReadTokenNeedsItsProof(t *testing.T) {
	rig := newAccessRouteRig(t)
	caller, other := newBrokerCaller(t), newBrokerCaller(t)
	const token, lapsed = "the-brokers-own-read-token", "a-read-token-the-broker-no-longer-holds"
	rig.broker.Says(token, readTokenOf(caller.jkt, brokerAccessService))
	expired := readTokenOf(caller.jkt, brokerAccessService)
	expired.Active, expired.ExpiresAt = false, time.Now().Add(-time.Minute).Unix()
	rig.broker.Says(lapsed, expired)
	const person = "/api/v1/access/person?person=alice"
	wantAnswered(t, rig.serve(caller.presenting(t, http.MethodGet, person, token, nil)), "the broker's read token with a sound proof", person)

	sound := caller.caller.Signed(t, http.MethodGet, testServerURL+person, token)
	if spent := rig.serve(brokerRequest(t, http.MethodGet, person, nil, sound.Header.Get("Authorization"), sound.Header.Get("DPoP"))); spent.Code != http.StatusOK {
		t.Fatalf("the proof's first presentation: %d %s, want 200", spent.Code, spent.Body.String())
	}
	presented := func(method, target, signedToken string) *http.Request {
		signed := caller.caller.Signed(t, method, target, signedToken)
		return brokerRequest(t, http.MethodGet, person, nil, "DPoP "+token, signed.Header.Get("DPoP"))
	}
	for _, c := range []struct {
		name    string
		request *http.Request
		code    string
	}{
		{"no proof", brokerRequest(t, http.MethodGet, person, nil, "DPoP "+token, ""), "BROKER_TOKEN_PROOF_INVALID"},
		{"the Bearer scheme", brokerRequest(t, http.MethodGet, person, nil, "Bearer "+token, ""), "UNAUTHORIZED"},
		{"a proof of another key", other.presenting(t, http.MethodGet, person, token, nil), "BROKER_TOKEN_KEY_MISMATCH"},
		{"a proof for another origin", presented(http.MethodGet, "http://127.0.0.1:8080"+person, token), "BROKER_TOKEN_PROOF_INVALID"},
		{"a proof for another method", presented(http.MethodPost, testServerURL+person, token), "BROKER_TOKEN_PROOF_INVALID"},
		{"a proof naming another token", presented(http.MethodGet, testServerURL+person, "another-token"), "BROKER_TOKEN_PROOF_INVALID"},
		{"a spent proof", brokerRequest(t, http.MethodGet, person, nil, sound.Header.Get("Authorization"), sound.Header.Get("DPoP")), "BROKER_TOKEN_PROOF_INVALID"},
		{"a token the broker no longer holds live", caller.presenting(t, http.MethodGet, person, lapsed, nil), "BROKER_TOKEN_NOT_ACTIVE"},
	} {
		wantRefusal(t, rig.serve(c.request), http.StatusUnauthorized, c.code, "the broker's read token with "+c.name, "GET "+person)
	}
	for _, path := range accessRoutePaths {
		wantRefusal(t, rig.serve(brokerRequest(t, http.MethodGet, path, nil, "DPoP "+token, "")),
			http.StatusUnauthorized, "BROKER_TOKEN_PROOF_INVALID", "the broker's read token with no proof", "GET "+path)
	}
}

// TestWithNoOIDCNothingButTheListedReadTokenReadsTheAccessRoutes: on a deployment with no
// DISPATCH_OIDC_* setting, each access route answers the listed service's read token with its
// valid proof, and refuses every other credential a caller could present there: none at all, the
// listed account's projected token as a bearer, the shared token, a person's cookie, a personal
// token, a broker token naming a person, and another service's read token.
//
// Control: accessAccountOf admitting every broker-issued token whose service is listed, read-only
// or not, as an OIDC-less admission that read the service alone would: the person's broker token
// is still refused, and the listed service's other token (TestOtherReadServiceCannotCallTheAccessRoutes)
// is answered 200.
func TestWithNoOIDCNothingButTheListedReadTokenReadsTheAccessRoutes(t *testing.T) {
	rig := newAccessRouteRig(t)
	caller := newBrokerCaller(t)
	const listed, other, personToken = "the-brokers-own-read-token", "an-eval-sites-read-token", "a-persons-broker-token"
	rig.broker.Says(listed, readTokenOf(caller.jkt, brokerAccessService))
	rig.broker.Says(other, readTokenOf(caller.jkt, otherReadService))
	rig.broker.Says(personToken, liveNamedToken(caller.jkt))
	personal := mintPersonalToken(t, rig.handler, "alice", "access-route-probe", nil).Token
	issuer := newServiceTokenIssuer(t)
	projected := issuer.Mint(t, issuer.signing, issuer.Claims(brokerAccessService, serviceTokenAudience))
	for _, path := range accessRoutePaths {
		wantAnswered(t, rig.serve(caller.presenting(t, http.MethodGet, path, listed, nil)), "the broker's read token", path)
		for _, c := range []struct {
			who     string
			request *http.Request
			status  int
			code    string
		}{
			{"a caller with no credential", brokerRequest(t, http.MethodGet, path, nil, "", ""), http.StatusUnauthorized, "NO_IDENTITY"},
			{"the listed account's projected token", brokerRequest(t, http.MethodGet, path, nil, "Bearer "+projected, ""), http.StatusUnauthorized, "UNAUTHORIZED"},
			{"the shared token", brokerRequest(t, http.MethodGet, path, nil, "Bearer agent-token", ""), http.StatusForbidden, "ACCESS_ACCOUNT_REQUIRED"},
			{"alice's cookie", withPerson(brokerRequest(t, http.MethodGet, path, nil, "", ""), "alice"), http.StatusForbidden, "ACCESS_ACCOUNT_REQUIRED"},
			{"alice's personal token", brokerRequest(t, http.MethodGet, path, nil, "Bearer "+personal, ""), http.StatusForbidden, "ACCESS_ACCOUNT_REQUIRED"},
			{"a broker token naming a person", caller.presenting(t, http.MethodGet, path, personToken, nil), http.StatusForbidden, "ACCESS_ACCOUNT_REQUIRED"},
			{otherReadService + "'s read token", caller.presenting(t, http.MethodGet, path, other, nil), http.StatusForbidden, contracts.DispatchReadOnlyToken},
		} {
			wantRefusal(t, rig.serve(c.request), c.status, c.code, c.who, "GET "+path)
		}
	}
}
