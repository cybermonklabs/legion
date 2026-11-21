// read_only_token_test.go holds Dispatch to its read class: a read-only token — one the secrets
// broker minted from a read entry, or narrowed to reading because its trade asked for read, or a
// personal token a person minted holding read — reaches the rows readClasses classes as reads and
// nothing else. Every other row of the table and GET /ws/live answer it 403 READ_ONLY_TOKEN before
// the route's handler, before the row's scopes and the sign-in check and before its
// Idempotency-Key is read. A token that carries a cut is answered as of its cut on the rows of kind
// answers and named by GET /api/v1/whoami, and every other read and GET /ws/live answer it 403
// AS_OF_TOKEN_NOT_SERVED (refuseReadOnlyToken, as_of_token_test.go). register refuses a table
// whose GET rows readClasses does not class, and a personal token the gate resolves before the
// route keeps the answer it had before the gate.
//
// A token that names a service and no person is already refused every write, SIGN_IN_REQUIRED or
// OAUTH_SCOPE_REQUIRED, so each refusal here is read by its code: a bare 403 would prove nothing.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// readOnlyProbePath is routeProbePath with every wildcard it leaves filled too - a test hook's
// {point} - so every row of the table with its test hooks in is reached at a path its own
// pattern matches rather than at a literal brace.
func readOnlyProbePath(pattern string) string {
	path := routeProbePath(pattern)
	for _, name := range patternWildcards(pattern) {
		path = strings.ReplaceAll(path, "{"+name+"}", "x")
		path = strings.ReplaceAll(path, "{"+name+"...}", "x")
	}
	return path
}

// readOnlyProbeKey is the Idempotency-Key every probe below carries: a well-formed one, so a row
// that reached its keyed layer would claim it rather than refuse it.
const readOnlyProbeKey = "read-only-probe"

// presenting is the request token's holder sends Dispatch for method and path: a personal token
// as a bearer, as its agent presents one, and any other token under the DPoP scheme with a fresh
// proof of caller's key for that method and target.
func (token readOnlyWalkToken) presenting(t *testing.T, caller *brokerCaller, method, path string, body any) *http.Request {
	t.Helper()
	if token.personal {
		return brokerRequest(t, method, path, body, "Bearer "+token.token, "")
	}
	return caller.presenting(t, method, path, token.token, body)
}

// probe is the request token's holder sends route when it walks the table: the probe path and
// body (readOnlyProbePath, routeProbeBody) and a valid Idempotency-Key, presented as the token is
// (presenting).
func (token readOnlyWalkToken) probe(t *testing.T, caller *brokerCaller, route apiRoute) *http.Request {
	t.Helper()
	request := token.presenting(t, caller, route.Method, readOnlyProbePath(route.Pattern), json.RawMessage(routeProbeBody(route)))
	request.Header.Set(contracts.IdempotencyKeyHeader, readOnlyProbeKey)
	return request
}

// theReadOnlyRefusal is the message of every 403 READ_ONLY_TOKEN: one fixed text that names no
// route, written out here rather than read from the server, so a refusal that drifted from it
// fails.
const theReadOnlyRefusal = "this request's credential is a read-only token, which may call only the GET routes Dispatch classes as reads: nothing ran"

// whoamiRow is the one read that describes the caller now: it answers a read-only token that
// carries a cut by naming the cut.
const whoamiRow = "GET /api/v1/whoami"

// gateRefusalOf is the code and message response carries, and empty for an answer that is no
// refusal: a walk reads every row's answer, a list or a text among them, so it never fails on the
// shape.
func gateRefusalOf(response *httptest.ResponseRecorder) refusalAnswer {
	var refusal refusalAnswer
	_ = json.Unmarshal(response.Body.Bytes(), &refusal)
	return refusal
}

// notTheGatesRefusal says how response differs from the read-only gate's refusal coded code, and
// is empty when it is that refusal: 403 with that code and no WWW-Authenticate challenge, since no
// scope makes the route the token's to call. READ_ONLY_TOKEN carries the one fixed message, and
// AS_OF_TOKEN_NOT_SERVED names cut, the cut the token carries.
func notTheGatesRefusal(response *httptest.ResponseRecorder, code, cut string) string {
	refusal := gateRefusalOf(response)
	switch {
	case response.Code != http.StatusForbidden || refusal.Code != code:
		return "answered " + http.StatusText(response.Code) + " " + strings.TrimSpace(response.Body.String())
	case response.Header().Get("WWW-Authenticate") != "":
		return "carries the challenge " + response.Header().Get("WWW-Authenticate")
	case code == contracts.DispatchReadOnlyToken && refusal.Error != theReadOnlyRefusal:
		return fmt.Sprintf("refused with the message %q, want %q", refusal.Error, theReadOnlyRefusal)
	case code == contracts.DispatchAsOfTokenNotServed && !strings.Contains(refusal.Error, cut):
		return fmt.Sprintf("refused with the message %q, which does not name the cut %s", refusal.Error, cut)
	}
	return ""
}

// readOnlyTable is the table register mounts with its test hooks in: every row a read-only token
// could be sent to on a server that serves the hooks, which is the most rows any server mounts.
func readOnlyTable() []apiRoute {
	return (&server{deps: Deps{TestHooksEnabled: true}}).routes()
}

// readOnlyWalkToken is one read-only token the walks below send: what a failure calls it, the
// token, the cut it carries (empty for none), the credential Dispatch makes of it, and whether it
// is a personal token, which its agent presents as a bearer, rather than one the stub broker
// answers for, which its holder presents under DPoP (presenting).
type readOnlyWalkToken struct {
	name       string
	token      string
	cut        string
	credential *model.Credential
	personal   bool
}

// readOnlyWalkTokens is every kind of read-only token Dispatch serves. The broker's, each said to
// the rig's stub broker for caller's key: the read entry's token (the project scope none beside
// the scope read), a token narrowed from each kind of trade the broker narrows, and among them the
// machine login's narrowed token with a cut, as the golden file the broker's own test holds its
// answers to (ciphertokentest.NarrowedAnswers). And a person's, minted on the rig's own store: a
// read-only personal token of each set such a token may hold (readOnlyPersonalTokens). Each is
// held to be a credential Dispatch reads as read-only, carrying the very cut its answer names,
// and at least one to carry a cut, so a walk over them cannot pass for want of either.
func readOnlyWalkTokens(t *testing.T, rig *brokerTokenRig, caller *brokerCaller) []readOnlyWalkToken {
	t.Helper()
	answers := map[string]ciphertoken.Introspection{"the read entry's token": readOnlyToken(caller.jkt)}
	for kind, answer := range ciphertokentest.NarrowedAnswers(t, caller.jkt, brokerTestSession, time.Now().Add(time.Hour).Unix()) {
		answers["the token narrowed from a "+kind] = answer
	}
	var tokens []readOnlyWalkToken
	cuts := 0
	for index, name := range slices.Sorted(maps.Keys(answers)) {
		token := fmt.Sprintf("a-read-only-token-walking-the-table-%d", index)
		rig.broker.Says(token, answers[name])
		actor, _, err := rig.served.brokerTokenActor(caller.presenting(t, http.MethodGet, "/api/v1/whoami", token, nil))
		if err != nil || actor.Credential == nil || !actor.Credential.ReadOnly {
			t.Fatalf("%s: Dispatch's actor is %+v (%v), want a read-only credential", name, actor, err)
		}
		cut := answers[name].AsOf
		var want time.Time
		if cut != "" {
			parsed, err := time.Parse(time.RFC3339Nano, cut)
			if err != nil {
				t.Fatalf("%s: the broker answered the cut %q, which is no RFC 3339 time: %v", name, cut, err)
			}
			want = parsed
		}
		if !actor.Credential.Cut.Equal(want) {
			t.Fatalf("%s: the broker answered the cut %q and the credential carries %v", name, cut, actor.Credential.Cut)
		}
		if cut != "" {
			cuts++
		}
		tokens = append(tokens, readOnlyWalkToken{name: name, token: token, cut: cut, credential: actor.Credential})
	}
	if cuts == 0 {
		t.Fatal("no token the walk sends carries a cut, and one must")
	}
	return append(tokens, readOnlyPersonalTokens(t, rig)...)
}

// readOnlyPersonalTokenSets is every set of scopes a read-only personal token may hold, read alone
// and read beside person (agent_read_tokens' own check), each with what a failure calls its token
// and the scopes whoami names for it.
var readOnlyPersonalTokenSets = []struct {
	name           string
	scopes, whoami []string
}{
	{"the read-only personal token holding read", []string{model.ScopeRead}, []string{model.ScopeRead}},
	{"the read-only personal token holding read and person", []string{model.ScopeRead, model.ScopePerson}, []string{model.ScopePerson, model.ScopeRead}},
}

// readOnlyPersonalTokens mints one read-only personal token of brokerTestPerson's for each set of
// readOnlyPersonalTokenSets, in that order, through the route a person mints one on, and holds
// each to be a credential Dispatch reads as read-only (personalTokenActor): no cut, the person
// scope exactly where the set holds it, and never read among its scopes, since read marks the
// credential and is no scope a route can require.
func readOnlyPersonalTokens(t *testing.T, rig *brokerTokenRig) []readOnlyWalkToken {
	t.Helper()
	tokens := make([]readOnlyWalkToken, 0, len(readOnlyPersonalTokenSets))
	for _, set := range readOnlyPersonalTokenSets {
		minted := rig.asPerson(t, http.MethodPost, "/api/v1/me/agent-tokens", map[string]any{"name": set.name, "scopes": set.scopes})
		if minted.Code != http.StatusCreated {
			t.Fatalf("mint %s: status=%d body=%s", set.name, minted.Code, minted.Body.String())
		}
		token := decodeBody[agentTokenResponse](t, minted).Token
		actor, _, err := rig.served.optionalActor(brokerRequest(t, http.MethodGet, "/api/v1/whoami", nil, "Bearer "+token, ""))
		if credential := actor.Credential; err != nil || credential == nil || !credential.ReadOnly || !credential.Cut.IsZero() ||
			slices.Contains(credential.Scopes, model.ScopeRead) ||
			credential.Holds(model.ScopePerson) != slices.Contains(set.scopes, model.ScopePerson) {
			t.Fatalf("%s: Dispatch's credential is %+v (%v), want a read-only one with no cut, holding person exactly where the token does and never read",
				set.name, credential, err)
		}
		tokens = append(tokens, readOnlyWalkToken{name: set.name, token: token, credential: actor.Credential, personal: true})
	}
	return tokens
}

// readOnlyAnswer is the refusal the gate, or the row's own scopes, answer token on route, and
// empty where the row's handler runs: READ_ONLY_TOKEN on every row that is not a read, every
// write among them; AS_OF_TOKEN_NOT_SERVED on every read but whoami and the rows of kind answers
// for a token that carries a cut; OAUTH_SCOPE_REQUIRED on a read whose scopes the token's
// credential does not hold. The class and the kind are read from readClasses and not from the row
// (apiRoute.Read), so a table routes() stamped wrong fails the walks rather than agreeing with
// the gate.
func readOnlyAnswer(route apiRoute, token readOnlyWalkToken) string {
	name := route.Method + " " + route.Pattern
	switch {
	case route.Method != http.MethodGet || readClasses[name].Writes != readWritesNothing:
		return contracts.DispatchReadOnlyToken
	case token.cut != "" && readClasses[name].AsOf != asOfAnswers && name != whoamiRow:
		return contracts.DispatchAsOfTokenNotServed
	}
	for _, scope := range route.OAuthScopes {
		if !token.credential.Holds(scope) {
			return codeScopeRequired
		}
	}
	return ""
}

// liveSocketAnswer is the refusal GET /ws/live answers token: AS_OF_TOKEN_NOT_SERVED for a token
// that carries a cut, since the cut is read first, and READ_ONLY_TOKEN for every other.
func liveSocketAnswer(token readOnlyWalkToken) string {
	if token.cut != "" {
		return contracts.DispatchAsOfTokenNotServed
	}
	return contracts.DispatchReadOnlyToken
}

// readOnlyTally counts what one token's walk met, for its log line.
type readOnlyTally struct {
	walked, readOnly, cut, scopes, served int
}

// count adds one row whose expected answer was want, where "" is a row that served the token.
func (tally *readOnlyTally) count(want string) {
	tally.walked++
	switch want {
	case contracts.DispatchReadOnlyToken:
		tally.readOnly++
	case contracts.DispatchAsOfTokenNotServed:
		tally.cut++
	case codeScopeRequired:
		tally.scopes++
	default:
		tally.served++
	}
}

// TestEveryRowAnswersEachReadOnlyTokenByItsClass builds every row's own chain as register does
// (routeHandler), test hooks in, with the row's handler replaced by a counter, and sends each a
// body and a valid Idempotency-Key under every kind of read-only token (readOnlyWalkTokens). A
// row that is not a read answers 403 READ_ONLY_TOKEN with no challenge and never runs its
// handler. A read runs its handler once for a token with no cut, unless the row's own scopes
// refuse that token first (OAUTH_SCOPE_REQUIRED, which the gate never answers). The token with a
// cut runs the handler of whoami and of each row of kind answers in the same way. Every other read
// answers that token 403 AS_OF_TOKEN_NOT_SERVED naming the cut and runs nothing. GET /ws/live is
// walked the same way through its own chain (liveSocketHandler), with the socket replaced, and
// answers the token with a cut 403 AS_OF_TOKEN_NOT_SERVED and every other token 403
// READ_ONLY_TOKEN. A row is counted once its assertions have run, and each token's count is held
// to the rows the registered server mounts (its route index, one entry a row), so a walk that
// skipped a row, or walked none, fails.
func TestEveryRowAnswersEachReadOnlyTokenByItsClass(t *testing.T) {
	rig := newBrokerTokenRigOn(t, testServerOptions{testHooks: true})
	caller := newBrokerCaller(t)
	tokens := readOnlyWalkTokens(t, rig, caller)
	table := readOnlyTable()
	replays := rig.served.keyedReplays()
	mounted := len(rig.served.routeIndex)

	for _, token := range tokens {
		var tally readOnlyTally
		for _, route := range table {
			name := route.Method + " " + route.Pattern
			ran := 0
			route.Handler = func(w http.ResponseWriter, _ *http.Request) {
				ran++
				w.WriteHeader(http.StatusNoContent)
			}
			response := httptest.NewRecorder()
			rig.served.routeHandler(route, replays[name]).ServeHTTP(response, token.probe(t, caller, route))
			want := readOnlyAnswer(route, token)
			switch want {
			case "":
				if ran != 1 || response.Code != http.StatusNoContent {
					t.Errorf("%s: %s is a read, and its handler ran %d times (status=%d body=%s), want once",
						token.name, name, ran, response.Code, response.Body.String())
				}
			case codeScopeRequired:
				if refusal := gateRefusalOf(response); ran != 0 || response.Code != http.StatusForbidden || refusal.Code != codeScopeRequired {
					t.Errorf("%s: %s ran its handler %d times and answered %d %s, want its scopes' 403 %s and no handler",
						token.name, name, ran, response.Code, response.Body.String(), codeScopeRequired)
				}
			default:
				if ran != 0 {
					t.Errorf("%s: %s ran its handler %d times, want none", token.name, name, ran)
				}
				if why := notTheGatesRefusal(response, want, token.cut); why != "" {
					t.Errorf("%s: %s %s, want 403 %s", token.name, name, why, want)
				}
			}
			tally.count(want)
		}

		socketOpened := 0
		live := rig.served.liveSocketHandler(func(http.ResponseWriter, *http.Request) { socketOpened++ })
		response := httptest.NewRecorder()
		live.ServeHTTP(response, token.presenting(t, caller, http.MethodGet, liveSocketPattern, nil))
		if socketOpened != 0 {
			t.Errorf("%s: GET %s opened the socket %d times, want none", token.name, liveSocketPattern, socketOpened)
		}
		liveWant := liveSocketAnswer(token)
		if why := notTheGatesRefusal(response, liveWant, token.cut); why != "" {
			t.Errorf("%s: GET %s %s, want 403 %s", token.name, liveSocketPattern, why, liveWant)
		}

		if tally.walked == 0 || tally.walked != mounted {
			t.Fatalf("%s: walked %d rows, want every one of the %d the server mounts", token.name, tally.walked, mounted)
		}
		t.Logf("%s: walked %d of the table's %d rows (test hooks in): %d refused %s, %d refused %s, %d refused by their scopes, %d ran their handler; GET %s refused %s",
			token.name, tally.walked, mounted, tally.readOnly, contracts.DispatchReadOnlyToken,
			tally.cut, contracts.DispatchAsOfTokenNotServed, tally.scopes, tally.served, liveSocketPattern, liveWant)
	}
	t.Logf("walked the table's %d rows and GET %s with each of %d read-only tokens", mounted, liveSocketPattern, len(tokens))
}

// TestTheMountedServerAnswersEachReadOnlyTokenByItsClass walks the server register mounted, test
// hooks in, with every kind of read-only token: every row that is not a read answers 403
// READ_ONLY_TOKEN; every read that is not whoami and not of kind answers answers the token with a
// cut 403 AS_OF_TOKEN_NOT_SERVED naming the cut; and every other read answers its handler's own
// answer of its probe path, which is neither refusal nor a 401, since the token resolved (or its
// scopes' OAUTH_SCOPE_REQUIRED, for a token that does not hold them). GET /ws/live answers the
// token with a cut 403 AS_OF_TOKEN_NOT_SERVED naming the cut and every other token 403
// READ_ONLY_TOKEN. GET /api/v1/whoami answers every token 200 with the cut it carries, null for
// none, and GET /ws/doc/{room}, which reads no broker token at all, answers 401. A row is counted
// once its assertions have run, and each token's count is held to the rows the registered server
// mounts, as the walk above is.
func TestTheMountedServerAnswersEachReadOnlyTokenByItsClass(t *testing.T) {
	rig := newBrokerTokenRigOn(t, testServerOptions{testHooks: true})
	caller := newBrokerCaller(t)
	tokens := readOnlyWalkTokens(t, rig, caller)
	table := readOnlyTable()
	mounted := len(rig.served.routeIndex)

	for _, token := range tokens {
		var tally readOnlyTally
		for _, route := range table {
			name := route.Method + " " + route.Pattern
			response := rig.serve(token.probe(t, caller, route))
			want := readOnlyAnswer(route, token)
			switch want {
			case "":
				if refusal := gateRefusalOf(response); response.Code == http.StatusUnauthorized ||
					refusal.Code == contracts.DispatchReadOnlyToken || refusal.Code == contracts.DispatchAsOfTokenNotServed {
					t.Errorf("%s: %s is a read and answered %d %s, want its handler's own answer", token.name, name, response.Code, response.Body.String())
				}
			case codeScopeRequired:
				if refusal := gateRefusalOf(response); response.Code != http.StatusForbidden || refusal.Code != codeScopeRequired {
					t.Errorf("%s: %s answered %d %s, want its scopes' 403 %s", token.name, name, response.Code, response.Body.String(), codeScopeRequired)
				}
			default:
				if why := notTheGatesRefusal(response, want, token.cut); why != "" {
					t.Errorf("%s: %s %s, want 403 %s", token.name, name, why, want)
				}
			}
			if name == whoamiRow {
				wantCut := "null"
				if token.cut != "" {
					wantCut = `"` + token.cut + `"`
				}
				var answer struct {
					Cut json.RawMessage `json:"cut"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil || response.Code != http.StatusOK || string(answer.Cut) != wantCut {
					t.Errorf("%s: %s answered %d %s, want 200 with the cut %s", token.name, name, response.Code, response.Body.String(), wantCut)
				}
			}
			tally.count(want)
		}

		live := rig.serve(token.presenting(t, caller, http.MethodGet, liveSocketPattern, nil))
		liveWant := liveSocketAnswer(token)
		if why := notTheGatesRefusal(live, liveWant, token.cut); why != "" {
			t.Errorf("%s: GET %s %s, want 403 %s", token.name, liveSocketPattern, why, liveWant)
		}
		room := "/ws/doc/00000000-0000-0000-0000-000000000001"
		if document := rig.serve(token.presenting(t, caller, http.MethodGet, room, nil)); document.Code != http.StatusUnauthorized {
			t.Errorf("%s: GET %s answered %d %s, want 401: the document socket takes a cookie alone", token.name, room, document.Code, document.Body.String())
		}

		if tally.walked == 0 || tally.walked != mounted {
			t.Fatalf("%s: walked %d rows, want every one of the %d the server mounts", token.name, tally.walked, mounted)
		}
		t.Logf("%s: walked %d of the mounted table's %d rows (test hooks in): %d refused %s, %d refused %s, %d refused by their scopes, %d answered by their handler; GET %s refused %s, GET /ws/doc/{room} 401",
			token.name, tally.walked, mounted, tally.readOnly, contracts.DispatchReadOnlyToken,
			tally.cut, contracts.DispatchAsOfTokenNotServed, tally.scopes, tally.served, liveSocketPattern, liveWant)
	}
	t.Logf("walked the mounted table's %d rows, GET %s and GET /ws/doc/{room} with each of %d read-only tokens", mounted, liveSocketPattern, len(tokens))
}

// TestTheLiveSocketRefusesAReadOnlyPersonalToken: GET /ws/live under a read-only personal token,
// holding read alone or beside person, is 403 READ_ONLY_TOKEN and no socket opens, since a socket
// delivers whatever its views carry and so is no read. Each token is sent through the socket's own
// chain (liveSocketHandler) with the socket replaced by a counter, which shows the socket never
// opens, and through the mounted server, which shows the refusal is what a client is answered.
// The control is a personal token that writes, holding the person scope too: the same chain opens
// the socket for it, so the refusals are the read-only mark's and not the chain's.
func TestTheLiveSocketRefusesAReadOnlyPersonalToken(t *testing.T) {
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	opened := 0
	live := rig.served.liveSocketHandler(func(w http.ResponseWriter, _ *http.Request) {
		opened++
		w.WriteHeader(http.StatusNoContent)
	})
	for _, token := range readOnlyPersonalTokens(t, rig) {
		response := httptest.NewRecorder()
		live.ServeHTTP(response, token.presenting(t, caller, http.MethodGet, liveSocketPattern, nil))
		if opened != 0 {
			t.Fatalf("%s: GET %s opened the socket %d times, want none", token.name, liveSocketPattern, opened)
		}
		if why := notTheGatesRefusal(response, contracts.DispatchReadOnlyToken, ""); why != "" {
			t.Errorf("%s: GET %s %s, want 403 %s", token.name, liveSocketPattern, why, contracts.DispatchReadOnlyToken)
		}
		if why := notTheGatesRefusal(rig.serve(token.presenting(t, caller, http.MethodGet, liveSocketPattern, nil)), contracts.DispatchReadOnlyToken, ""); why != "" {
			t.Errorf("%s: GET %s on the mounted server %s, want 403 %s", token.name, liveSocketPattern, why, contracts.DispatchReadOnlyToken)
		}
	}

	minted := rig.asPerson(t, http.MethodPost, "/api/v1/me/agent-tokens", map[string]any{"name": "a writer", "scopes": []string{model.ScopePerson}})
	if minted.Code != http.StatusCreated {
		t.Fatalf("mint a personal token that writes: status=%d body=%s", minted.Code, minted.Body.String())
	}
	response := httptest.NewRecorder()
	live.ServeHTTP(response, brokerRequest(t, http.MethodGet, liveSocketPattern, nil, "Bearer "+decodeBody[agentTokenResponse](t, minted).Token, ""))
	if opened != 1 {
		t.Fatalf("GET %s under a personal token that writes opened the socket %d times (status=%d body=%s), want once: the chain admits a token that writes",
			liveSocketPattern, opened, response.Code, response.Body.String())
	}
}

// registerRefusal is the refusal Register panics with on the table as it stands, and empty when
// it mounts the table.
func registerRefusal(t *testing.T) (refusal string) {
	t.Helper()
	defer func() {
		if recovered := recover(); recovered != nil {
			refusal, _ = recovered.(string)
			if refusal == "" {
				t.Fatalf("api.Register panicked with %v, want a refusal in words", recovered)
			}
		}
	}()
	Register(http.NewServeMux(), t.Context(), Deps{AgentTokens: sharedAgentTokens(t, "agent-token")})
	return ""
}

// TestRegisterRefusesAGETRowWithNoReadClass: a GET row with no class is a row nobody has read for
// what it writes, and an entry that names no GET row reads as though it classed a route that is
// not there, so Register refuses a table whose read class has either, naming the row: a GET row
// with no entry, an entry whose Writes is unset, an entry naming a POST row, an entry naming no
// row at all, and a GET that writes whose entry has no WriteNote. With all five at once, the one
// refusal names every one of them. It replaces the package's readClasses for its length, so it is
// not parallel.
func TestRegisterRefusesAGETRowWithNoReadClass(t *testing.T) {
	kept := readClasses
	t.Cleanup(func() { readClasses = kept })
	if refusal := registerRefusal(t); refusal != "" {
		t.Fatalf("api.Register refused the read class as it stands: %s", refusal)
	}
	const (
		unclassed = whoamiRow
		unset     = "GET /api/v1/limits"
		post      = "POST /api/v1/issues/{key}/comments"
		nowhere   = "GET /api/v1/no-such-route"
		unnoted   = "GET /api/v1/delivery/timeline"
	)
	changes := []struct {
		why, row string
		change   func(classes map[string]readEntry)
	}{
		{"a GET row with no entry", unclassed, func(classes map[string]readEntry) { delete(classes, unclassed) }},
		{"an entry whose Writes is unset", unset, func(classes map[string]readEntry) { classes[unset] = readEntry{} }},
		{"an entry naming a POST row", post, func(classes map[string]readEntry) { classes[post] = readEntry{Writes: readWritesNothing} }},
		{"an entry naming no row", nowhere, func(classes map[string]readEntry) { classes[nowhere] = readEntry{Writes: readWritesNothing} }},
		{"a GET that writes with no WriteNote", unnoted, func(classes map[string]readEntry) { classes[unnoted] = readEntry{Writes: readWritesState} }},
	}
	refused := func(t *testing.T, refusal, row string) {
		t.Helper()
		if !strings.Contains(refusal, "read class") || !strings.Contains(refusal, row+":") {
			t.Fatalf("api.Register refused %q, want a refusal of the read class naming %s", refusal, row)
		}
	}
	for _, change := range changes {
		t.Run(change.why, func(t *testing.T) {
			readClasses = maps.Clone(kept)
			change.change(readClasses)
			refused(t, registerRefusal(t), change.row)
		})
	}
	t.Run("all five at once", func(t *testing.T) {
		readClasses = maps.Clone(kept)
		for _, change := range changes {
			change.change(readClasses)
		}
		refusal := registerRefusal(t)
		for _, change := range changes {
			refused(t, refusal, change.row)
		}
	})
}

// TestAReadOnlyTokenIsRefusedBeforeTheIdempotencyKey: a POST to a keyed write under every kind of
// read-only token (readOnlyWalkTokens, the read-only personal tokens among them) is refused
// READ_ONLY_TOKEN under a well-formed Idempotency-Key and claims no key, and under a malformed one
// is still refused READ_ONLY_TOKEN rather than the key layer's 400, so the key layer never read
// the header. A named-project token's POST under the same key then runs its write. That second
// write is no proof on its own - a key belongs to its caller (requestKeyLogin), so the named
// token's key would be free whatever the first requests did - which is why the key table is read
// as well.
func TestAReadOnlyTokenIsRefusedBeforeTheIdempotencyKey(t *testing.T) {
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const named = "a-named-token-that-writes"
	tokens := readOnlyWalkTokens(t, rig, caller)
	rig.broker.Says(named, liveNamedToken(caller.jkt))
	own := seedProject(t, rig, brokerTestProject, "Broker")
	if held := countRequestKeys(t, rig.database); held != 0 {
		t.Fatalf("the seed left %d request keys, want none before the test's own requests", held)
	}

	path := "/api/v1/issues/" + own.issue + "/comments"
	const key, body = "read-only-comment-press-1", "Written under the key a read-only token was refused under."
	comment := map[string]any{"body": body}
	post := func(request *http.Request, key string) *httptest.ResponseRecorder {
		t.Helper()
		request.Header.Set(contracts.IdempotencyKeyHeader, key)
		return rig.serve(request)
	}

	for _, token := range tokens {
		if why := notTheGatesRefusal(post(token.presenting(t, caller, http.MethodPost, path, comment), key), contracts.DispatchReadOnlyToken, token.cut); why != "" {
			t.Fatalf("%s: a comment under a key %s, want 403 %s", token.name, why, contracts.DispatchReadOnlyToken)
		}
		if held := countRequestKeys(t, rig.database); held != 0 {
			t.Fatalf("%s: the refused request left %d request keys, want none", token.name, held)
		}
		if why := notTheGatesRefusal(post(token.presenting(t, caller, http.MethodPost, path, comment), "not a key"), contracts.DispatchReadOnlyToken, token.cut); why != "" {
			t.Fatalf("%s: a comment under a malformed key %s, want 403 %s and not the key layer's %s",
				token.name, why, contracts.DispatchReadOnlyToken, contracts.IdempotencyKeyInvalid)
		}
		if held := countRequestKeys(t, rig.database); held != 0 {
			t.Fatalf("%s: the refused request left %d request keys, want none", token.name, held)
		}
	}

	written := post(caller.presenting(t, http.MethodPost, path, named, comment), key)
	if written.Code != http.StatusCreated {
		t.Fatalf("the named token's comment under the same key: status=%d body=%s, want 201", written.Code, written.Body.String())
	}
	var comments int
	if err := rig.database.Pool.QueryRow(context.Background(),
		`select count(*) from comments where issue_key = $1 and body = $2`, own.issue, body).Scan(&comments); err != nil {
		t.Fatalf("count the comment: %v", err)
	}
	if comments != 1 {
		t.Fatalf("the issue holds %d of the comment, want the named token's one", comments)
	}
	if held := countRequestKeys(t, rig.database); held != 1 {
		t.Fatalf("the named token's write left %d request keys, want its own one: the key layer ran for it", held)
	}
	t.Logf("%d read-only tokens were refused a keyed comment under a sound key and a malformed one, and claimed no key", len(tokens))
}

// agentTokenLookup is personalTokenActor's one read of a bearer's row: one statement that looks
// the hash up in agent_tokens and agent_read_tokens alike, so a lookup split into one statement a
// table counts twice.
var agentTokenLookup = regexp.MustCompile(`(?is)\bfrom\s+agent_(read_)?tokens\s+where\s+token_hash\b`)

// TestAnUnresolvedBearerKeepsTheAnswerItHadBeforeTheGate: the read-only gate resolves a personal
// token before the route does, and a token that does not resolve is answered as it was before the
// gate, by the layer that answered it then. An unknown dsp_ bearer, a revoked one and a revoked
// read-only one are each sent to the two rows that take no credential (GET /api/v1 and GET
// /api/v1/schema/blocks), which answer 200, and to GET /api/v1/limits, GET /api/v1/issues/{key}
// and a keyed POST /api/v1/issues, which answer the refusal optionalActor gives each: 401
// UNAUTHORIZED "invalid bearer token" for the unknown one, 401 TOKEN_REVOKED for the revoked ones.
// Every statement each request ran, its handler's and the work it detached, is read through a
// tracer on the server's pool: none writes, and exactly one looks the token up, so the gate
// resolved it once and every later layer read the refusal it carried.
func TestAnUnresolvedBearerKeepsTheAnswerItHadBeforeTheGate(t *testing.T) {
	marked := &markedStatements{}
	handler, _, _ := newTestServer(t, testServerOptions{tracer: marked})

	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "An issue an unresolved bearer reads",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	issue := decodeBody[struct {
		Key string `json:"key"`
	}](t, created).Key
	minted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/me/agent-tokens", map[string]string{"name": "revoked"}, "alice")
	if minted.Code != http.StatusCreated {
		t.Fatalf("mint agent token: status=%d body=%s", minted.Code, minted.Body.String())
	}
	revoked := decodeBody[agentTokenResponse](t, minted)
	if response := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/me/agent-tokens/"+revoked.ID, nil, "alice"); response.Code != http.StatusNoContent {
		t.Fatalf("revoke agent token: status=%d body=%s", response.Code, response.Body.String())
	}
	revokedReader := mintPersonalToken(t, handler, "alice", "revoked-reader", []string{model.ScopeRead, model.ScopePerson})
	if response := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/me/agent-tokens/"+revokedReader.ID, nil, "alice"); response.Code != http.StatusNoContent {
		t.Fatalf("revoke the read-only agent token: status=%d body=%s", response.Code, response.Body.String())
	}
	unknown, _, err := newAgentToken()
	if err != nil {
		t.Fatalf("make an unknown token: %v", err)
	}

	rows := []struct {
		method, path string
		body         string
		public       bool
	}{
		{http.MethodGet, "/api/v1", "", true},
		{http.MethodGet, "/api/v1/schema/blocks", "", true},
		{http.MethodGet, "/api/v1/limits", "", false},
		{http.MethodGet, "/api/v1/issues/" + issue, "", false},
		{http.MethodPost, "/api/v1/issues", `{"project":"TEST","title":"Never created"}`, false},
	}
	bearers := []struct {
		name, token, code, message string
	}{
		{"an unknown dsp_ bearer", unknown, "UNAUTHORIZED", "invalid bearer token"},
		{"a revoked dsp_ bearer", revoked.Token, "TOKEN_REVOKED", "agent token has been revoked"},
		{"a revoked read-only dsp_ bearer", revokedReader.Token, "TOKEN_REVOKED", "agent token has been revoked"},
	}
	for _, bearer := range bearers {
		for _, row := range rows {
			marked.take()
			request := httptest.NewRequestWithContext(context.WithValue(t.Context(), countedRead{}, true),
				row.method, row.path, strings.NewReader(row.body))
			request.Header.Set("Authorization", "Bearer "+bearer.token)
			if row.body != "" {
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set(contracts.IdempotencyKeyHeader, "an-unresolved-bearer-creates-nothing")
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			name := bearer.name + " on " + row.method + " " + row.path
			if row.public {
				if response.Code != http.StatusOK {
					t.Errorf("%s: status=%d body=%s, want 200: the row takes no credential", name, response.Code, response.Body.String())
				}
			} else if refusal := gateRefusalOf(response); response.Code != http.StatusUnauthorized ||
				refusal.Code != bearer.code || refusal.Error != bearer.message {
				t.Errorf("%s: status=%d body=%s, want 401 %s %q", name, response.Code, response.Body.String(), bearer.code, bearer.message)
			}
			lookups := 0
			for _, statement := range marked.take() {
				if writes, readable := writesOf(statement); len(writes) > 0 || !readable {
					t.Errorf("%s: the request wrote %v: %s", name, writes, statement)
				}
				if agentTokenLookup.MatchString(statement) {
					lookups++
				}
			}
			if lookups != 1 {
				t.Errorf("%s: the request looked the token up %d times, want once", name, lookups)
			}
		}
	}
}

// markedStatements is sentStatements (comment_window_tick_test.go) with a copy recorded too, as
// the insert it is: the SQL of every statement a traced pool runs on a context the test marked
// (countedRead, issue_read_batch_test.go), each statement of a batch on its own. That is the one
// request the test sends and the work it detaches from that request's context, and nothing the
// server does on a context of its own. The two calls below are pgx.CopyFromTracer's, which
// sentStatements does not answer: without them a copy would be a write no tracer call names.
type markedStatements struct{ sentStatements }

func (m *markedStatements) TraceCopyFromStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceCopyFromStartData) context.Context {
	m.record(ctx, "insert into "+data.TableName.Sanitize()+" (copy from)")
	return ctx
}

func (m *markedStatements) TraceCopyFromEnd(context.Context, *pgx.Conn, pgx.TraceCopyFromEndData) {}

var (
	// tableWrite is one insert, update or delete and the table it writes.
	tableWrite = regexp.MustCompile(`(?i)\b(insert\s+into|update|delete\s+from)\s+"?([a-z_][a-z0-9_."]*)`)
	// writeWord is every word that makes a statement a write, so a write tableWrite cannot read
	// shows as a word left over.
	writeWord = regexp.MustCompile(`(?i)\b(insert|update|delete)\b`)
	// upsertUpdate is an insert's own conflict clause, whose table is the insert's.
	upsertUpdate = regexp.MustCompile(`(?i)\bdo\s+update\b`)
)

// writesOf answers each write statement makes, as "<verb> <table>", and false when it holds a
// write word no table can be read for: such a statement fails the test rather than passing it.
func writesOf(statement string) ([]string, bool) {
	read := upsertUpdate.ReplaceAllString(statement, "do upsert")
	matches := tableWrite.FindAllStringSubmatch(read, -1)
	writes := make([]string, 0, len(matches))
	for _, match := range matches {
		verb := strings.ToLower(strings.Fields(match[1])[0])
		table := strings.ToLower(strings.ReplaceAll(match[2], `"`, ""))
		if dot := strings.LastIndexByte(table, '.'); dot >= 0 {
			table = table[dot+1:]
		}
		writes = append(writes, verb+" "+table)
	}
	return writes, len(writeWord.FindAllString(read, -1)) == len(matches)
}
