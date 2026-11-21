package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// widgetAccepted and widgetApproved are arms of the gate's made-up state union beside
// widgetResolved: accepting needs person, approving person and approve.
type widgetAccepted struct {
	State string `json:"state"`
}

type widgetApproved struct {
	State string `json:"state"`
}

// widgetInboxQuery is the gate's made-up list read's query.
type widgetInboxQuery struct {
	WaitingOn *string  `query:"waiting_on"`
	Include   []string `query:"include,comma" enum:"thread,mark"`
}

// widgetAgentMark and widgetIssueMark are arms of a made-up mark union keyed by the path's {kind}:
// an agent mark's {id} is a session, and an issue mark's is none.
type widgetAgentMark struct {
	Pinned *bool `json:"pinned"`
}

func (widgetAgentMark) sessionsAddressed(r *http.Request) []string {
	return []string{r.PathValue("id")}
}

type widgetIssueMark struct {
	Pinned *bool `json:"pinned"`
}

// mountMadeUpRows serves rows through served's own chain (routeHandler), as register mounts a
// group's rows, each at every version it names, and counts the handler's runs.
func mountMadeUpRows(served *server, runs *atomic.Int32, rows ...routeRow) http.Handler {
	mux := http.NewServeMux()
	for _, row := range rows {
		row.Handler = func(w http.ResponseWriter, _ *http.Request) {
			WriteJSON(w, http.StatusOK, map[string]int32{"run": runs.Add(1)})
		}
		for _, route := range mountRow(row) {
			mux.HandleFunc(route.Method+" "+route.Pattern, served.routeHandler(route))
		}
	}
	return mux
}

// tokenRequest sends body as JSON under token, and under key where one is given.
func tokenRequest(t *testing.T, handler http.Handler, method, target string, body any, token, key string) *httptest.ResponseRecorder {
	t.Helper()
	var data []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		data = encoded
	}
	request := httptest.NewRequest(method, target, bytes.NewReader(data))
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		request.Header.Set(contracts.IdempotencyKeyHeader, key)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// assertScopeRefused fails unless response is 403 OAUTH_SCOPE_REQUIRED naming scope, under the
// challenge refuseMissingScope gives it.
func assertScopeRefused(t *testing.T, what string, response *httptest.ResponseRecorder, scope string) {
	t.Helper()
	code, text := refusalOf(t, response)
	if response.Code != http.StatusForbidden || code != "OAUTH_SCOPE_REQUIRED" || !strings.Contains(text, scope) ||
		response.Header().Get("WWW-Authenticate") != insufficientScopeChallenge(scope) {
		t.Errorf("%s: answered %d %s (WWW-Authenticate %q), want 403 OAUTH_SCOPE_REQUIRED naming %s",
			what, response.Code, response.Body.String(), response.Header().Get("WWW-Authenticate"), scope)
	}
}

// assertGateRefused fails unless response is assertScopeRefused's refusal written by the route's
// wrapper (gateV2Authorities) for the declared rule, whose text names that rule ("which state
// accepted requires"): a handler's own scope check names no rule, so a gate that skipped the rule
// fails this even where the handler refuses the request too.
func assertGateRefused(t *testing.T, what string, response *httptest.ResponseRecorder, scope, rule string) {
	t.Helper()
	assertScopeRefused(t, what, response, scope)
	if _, text := refusalOf(t, response); !strings.Contains(text, "which "+rule+" requires") {
		t.Errorf("%s: the refusal %q names no rule %q: the route's wrapper did not refuse it", what, text, rule)
	}
}

// The authority a value of a request needs is held before the handler runs (R1.8): a state, a
// body member's presence and a query value, each refused 403 OAUTH_SCOPE_REQUIRED naming the scope
// for a credential that lacks it, and admitted for one that holds it, and a value that needs
// nothing admitted for either. A refused write claims no idempotency key, so the person who holds
// the scope is answered by a run under the same key, not by the stored refusal.
func TestTheV2AuthorityGateHoldsEachDeclaredValue(t *testing.T) {
	_, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	person, approve := routeScopeSet{model.ScopePerson}, routeScopeSet{model.ScopePerson, model.ScopeApprove}

	transition := madeUpRow()
	transition.Versions, transition.Method, transition.Pattern, transition.Kind = onlyV2, http.MethodPut, "/widgets/{id}/state", kindState
	transition.Scope = scopeDeployment
	transition.Body = requestBody{Kind: bodyStateUnion, Arms: map[string]any{
		"resolved": widgetResolved{}, "accepted": widgetAccepted{}, "approved": widgetApproved{},
	}}
	transition.Authorities = []authorityRule{
		{Where: authorityWhere{In: partBody, Name: "state"}, Value: "accepted", Scopes: person},
		{Where: authorityWhere{In: partBody, Name: "state"}, Value: "approved", Scopes: approve},
	}
	change := madeUpRow()
	change.Versions, change.Method, change.Pattern, change.Kind = onlyV2, http.MethodPatch, "/widgets/{id}", kindItem
	change.Scope = scopeDeployment
	change.Body = requestBody{Kind: bodyJSON, Type: widgetBody{}}
	change.Authorities = []authorityRule{{Where: authorityWhere{In: partBody, Name: "owner"}, Scopes: person}}
	inbox := madeUpRow()
	inbox.Versions, inbox.Query, inbox.List = onlyV2, widgetInboxQuery{}, listBounds{}
	inbox.Authorities = []authorityRule{
		{Where: authorityWhere{In: partQuery, Name: "waiting_on"}, Value: "me", Scopes: person},
		{Where: authorityWhere{In: partQuery, Name: "include"}, Value: "mark", Scopes: person},
	}
	var runs atomic.Int32
	handler := mountMadeUpRows(served, &runs, transition, change, inbox)
	if faults := undecodableRoutes(append(append(mountRow(transition), mountRow(change)...), mountRow(inbox)...)); len(faults) > 0 {
		t.Fatalf("the made-up rows are refused: %q", faults)
	}

	scopeless := personalTokenOwnedBy(t, database, "alice")
	personOnly := personalTokenOwnedBy(t, database, "alice", model.ScopePerson)
	for _, refused := range []struct {
		what   string
		method string
		target string
		body   any
		token  string
		scope  string
	}{
		{"a state that needs person", http.MethodPut, "/api/v2/widgets/w1/state", map[string]any{"state": "accepted"}, scopeless, model.ScopePerson},
		{"a state that needs approve", http.MethodPut, "/api/v2/widgets/w1/state", map[string]any{"state": "approved"}, personOnly, model.ScopeApprove},
		{"a member that needs person", http.MethodPatch, "/api/v2/widgets/w1", map[string]any{"title": "Renamed", "owner": "bob"}, scopeless, model.ScopePerson},
		{"that member set to null", http.MethodPatch, "/api/v2/widgets/w1", map[string]any{"owner": nil}, scopeless, model.ScopePerson},
		{"that member spelled in capitals", http.MethodPatch, "/api/v2/widgets/w1", map[string]any{"OWNER": "bob"}, scopeless, model.ScopePerson},
		{"a query value that needs person", http.MethodGet, "/api/v2/widgets?waiting_on=me", nil, scopeless, model.ScopePerson},
		{"a comma-list member that needs person", http.MethodGet, "/api/v2/widgets?include=thread,mark", nil, scopeless, model.ScopePerson},
	} {
		before := runs.Load()
		assertScopeRefused(t, refused.what, tokenRequest(t, handler, refused.method, refused.target, refused.body, refused.token, ""), refused.scope)
		if runs.Load() != before {
			t.Errorf("%s: the handler ran behind the refusal", refused.what)
		}
	}
	for _, admitted := range []struct {
		what   string
		method string
		target string
		body   any
		token  string
	}{
		{"a state that needs nothing", http.MethodPut, "/api/v2/widgets/w1/state", map[string]any{"state": "resolved"}, scopeless},
		{"a state that needs person, with it", http.MethodPut, "/api/v2/widgets/w1/state", map[string]any{"state": "accepted"}, personOnly},
		{"a member that needs nothing", http.MethodPatch, "/api/v2/widgets/w1", map[string]any{"title": "Renamed"}, scopeless},
		{"a member that needs person, with it", http.MethodPatch, "/api/v2/widgets/w1", map[string]any{"owner": "bob"}, personOnly},
		{"a query value that needs nothing", http.MethodGet, "/api/v2/widgets?waiting_on=bob&include=thread", nil, scopeless},
		{"a query value that needs person, with it", http.MethodGet, "/api/v2/widgets?waiting_on=me&include=mark", nil, personOnly},
	} {
		if response := tokenRequest(t, handler, admitted.method, admitted.target, admitted.body, admitted.token, ""); response.Code != http.StatusOK {
			t.Errorf("%s: answered %d %s, want the handler's 200", admitted.what, response.Code, response.Body.String())
		}
	}
	if browser := dispatchRequest(t, handler, http.MethodPut, "/api/v2/widgets/w1/state", map[string]any{"state": "approved"}, "alice"); browser.Code != http.StatusOK {
		t.Errorf("a person at a browser approving: answered %d %s, want 200", browser.Code, browser.Body.String())
	}
	if anonymous := tokenRequest(t, handler, http.MethodGet, "/api/v2/widgets?waiting_on=me", nil, "", ""); anonymous.Code != http.StatusUnauthorized {
		t.Errorf("a caller with no credential asking for waiting_on=me: answered %d %s, want 401", anonymous.Code, anonymous.Body.String())
	}

	// alice's scopeless token and alice at a browser are one login, so one key is one key for
	// both: the refusal under it is no answer the key keeps.
	accept := map[string]any{"state": "accepted"}
	assertScopeRefused(t, "an accept under a key", tokenRequest(t, handler, http.MethodPut, "/api/v2/widgets/w2/state", accept, scopeless, "gate-key-1"), model.ScopePerson)
	if run := keyedRequest(t, handler, http.MethodPut, "/api/v2/widgets/w2/state", accept, "alice", "gate-key-1"); run.Code != http.StatusOK || replayed(run) {
		t.Fatalf("the person's accept under the refused request's key: answered %d %s (replayed %v), want a run of its own",
			run.Code, run.Body.String(), replayed(run))
	}
}

// The gate resolves the caller once a request, at the first of the route's rules that holds, and
// carries it on the request (resolvedCallerKey) as the route's scope gate does: a second rule that
// holds, every layer behind the gate and the handler read that caller rather than look a personal
// token up again.
func TestTheV2AuthorityGateCarriesTheCallerItResolved(t *testing.T) {
	_, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	person := routeScopeSet{model.ScopePerson}
	inbox := madeUpRow()
	inbox.Versions, inbox.Query, inbox.List = onlyV2, widgetInboxQuery{}, listBounds{}
	inbox.Authorities = []authorityRule{
		{Where: authorityWhere{In: partQuery, Name: "waiting_on"}, Value: "me", Scopes: person},
		{Where: authorityWhere{In: partQuery, Name: "include"}, Value: "mark", Scopes: person},
	}
	var carried atomic.Bool
	inbox.Handler = func(w http.ResponseWriter, r *http.Request) {
		caller, resolved := r.Context().Value(resolvedCallerKey{}).(resolvedCaller)
		carried.Store(resolved && caller.err == nil && caller.actor.Owner != nil && *caller.actor.Owner == "alice")
		WriteJSON(w, http.StatusOK, map[string]bool{"ran": true})
	}
	mux := http.NewServeMux()
	for _, route := range mountRow(inbox) {
		mux.HandleFunc(route.Method+" "+route.Pattern, served.routeHandler(route))
	}
	token := personalTokenOwnedBy(t, database, "alice", model.ScopePerson)
	if response := tokenRequest(t, mux, http.MethodGet, "/api/v2/widgets?waiting_on=me&include=mark", nil, token, ""); response.Code != http.StatusOK {
		t.Fatalf("a read both rules hold for, by a token holding their scope: answered %d %s, want 200", response.Code, response.Body.String())
	}
	if !carried.Load() {
		t.Fatal("the handler behind the gate found no caller on the request: the gate resolved one and kept it to itself")
	}
}

// A body that names the sessions its write goes into (sessionAddressing) is held to the session
// the caller's credential proved (refusesSessionOtherThanProved): a credential that proved one
// session writes into no other session's rows, and an arm whose id is no session is not held to
// it. The made-up row, which declares no OAuth scopes, stands for the agent marks of PATCH
// /api/v2/me/marks/{kind}/{id} and PATCH /api/v2/me/marks, which declare the person scope and
// which TestV2DeclaredAuthority holds to the same rule.
func TestTheV2AuthorityGateHoldsANamedSessionToTheCallersOwn(t *testing.T) {
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-token-for-one-session"
	rig.broker.Says(token, liveNamedToken(caller.jkt))

	mark := madeUpRow()
	mark.Versions, mark.Method, mark.Pattern, mark.Kind = onlyV2, http.MethodPatch, "/widget-marks/{kind}/{id}", kindItem
	mark.Scope = scopeSession
	mark.Body = requestBody{Kind: bodyKindUnion, Arms: map[string]any{"agent": widgetAgentMark{}, "issue": widgetIssueMark{}}}
	var runs atomic.Int32
	handler := mountMadeUpRows(rig.served, &runs, mark)
	pin := map[string]any{"pinned": true}
	send := func(path string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, caller.presenting(t, http.MethodPatch, path, token, pin))
		return response
	}

	if refused := send("/api/v2/widget-marks/agent/session-someone-else"); refused.Code != http.StatusForbidden {
		t.Errorf("an agent mark of another session: answered %d %s, want 403 SESSION_SCOPE", refused.Code, refused.Body.String())
	} else if code, _ := refusalOf(t, refused); code != "SESSION_SCOPE" {
		t.Errorf("an agent mark of another session: refused %s, want SESSION_SCOPE", code)
	}
	if own := send("/api/v2/widget-marks/agent/" + brokerTestSession); own.Code != http.StatusOK {
		t.Errorf("an agent mark of the caller's own session: answered %d %s, want 200", own.Code, own.Body.String())
	}
	if issue := send("/api/v2/widget-marks/issue/session-someone-else"); issue.Code != http.StatusOK {
		t.Errorf("an issue mark, whose id is no session: answered %d %s, want 200", issue.Code, issue.Body.String())
	}
	if runs.Load() != 2 {
		t.Errorf("the handler ran %d times, want twice", runs.Load())
	}
}
