// approve_only_token_test.go holds Dispatch's side of a person's approvals through the secrets
// broker's proxy: the broker mints a token of the one scope approve for a person's approve grant,
// binds it to its own pod's key and spends it for the session, never releasing it. Dispatch serves
// that token, an approve-only one, as its person with both of the person's scopes on the routes
// its approve list names, and refuses it on every other route before the route runs. Every test
// here runs Dispatch's real registered server (newBrokerTokenRigOn), its real verifier and its real
// route table.
package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// approveOnlyAnswer is the broker's answer for the token its proxy spends for a person's approve
// grant: live, bound to jkt (the broker pod's own key in production), acting as its person, naming
// no project, and holding the one scope approve.
func approveOnlyAnswer(jkt string) ciphertoken.Introspection {
	answer := personScopedTokenAnswer(jkt)
	answer.Scope = ciphertoken.ScopeApprove
	return answer
}

// TestAnApproveOnlyTokenApprovesAsItsPersonAndNothingElse: Dispatch's registered server answers
// the approve-only token's review of a document 201 and records the review as its person's, while
// the person-scoped token of the same session - the box's own - is refused 403
// OAUTH_SCOPE_REQUIRED naming approve on the same route. The approve-only token's comment on the
// issue, and its mint and change of a personal token, are refused 403 APPROVE_ONLY_TOKEN, and none
// of them is recorded.
func TestAnApproveOnlyTokenApprovesAsItsPersonAndNothingElse(t *testing.T) {
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const approveOnly, person = "an-approve-only-token", "the-boxs-person-scoped-token"
	rig.broker.Says(approveOnly, approveOnlyAnswer(caller.jkt))
	rig.broker.Says(person, personScopedTokenAnswer(caller.jkt))
	issue := createInteractionIssue(t, rig.handler, "APR", "Approve through the proxy", "A document to approve.\n")
	reviews := "/api/v1/artifacts/" + issue.PrimaryArtifactID + "/reviews"

	refused := rig.serve(caller.presenting(t, http.MethodPost, reviews, person, map[string]any{"state": "approved"}))
	if refusal := decodeBody[refusalAnswer](t, refused); refused.Code != http.StatusForbidden || refusal.Code != apicode.OAuthScopeRequired.V1Name() {
		t.Fatalf("the person-scoped token's review = %d %s; want 403 %s", refused.Code, refused.Body.String(), apicode.OAuthScopeRequired.V1Name())
	}

	approved := rig.serve(caller.presenting(t, http.MethodPost, reviews, approveOnly, map[string]any{"state": "approved"}))
	if approved.Code != http.StatusCreated {
		t.Fatalf("the approve-only token's review = %d %s; want 201", approved.Code, approved.Body.String())
	}
	var review model.ArtifactReview
	if err := json.Unmarshal(approved.Body.Bytes(), &review); err != nil {
		t.Fatalf("decode the review: %v", err)
	}
	if review.Actor.Kind != "user" || review.Actor.ID != brokerTestPerson || review.State != "approved" {
		t.Fatalf("the review = %+v; want an approval by %s", review, brokerTestPerson)
	}

	for _, write := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/api/v1/issues/" + issue.Key + "/comments", map[string]any{"body": "an approve-only token's comment"}},
		{http.MethodPost, "/api/v1/me/agent-tokens", map[string]any{"name": "minted through the proxy", "scopes": []string{model.ScopePerson, model.ScopeApprove}}},
		{http.MethodPatch, "/api/v1/me/agent-tokens/00000000-0000-4000-8000-000000000001", map[string]any{"scopes": []string{model.ScopePerson, model.ScopeApprove}}},
	} {
		response := rig.serve(caller.presenting(t, write.method, write.path, approveOnly, write.body))
		if refusal := decodeBody[refusalAnswer](t, response); response.Code != http.StatusForbidden || refusal.Code != contracts.DispatchApproveOnlyToken {
			t.Errorf("%s %s under the approve-only token = %d %s; want 403 %s", write.method, write.path, response.Code, response.Body.String(), contracts.DispatchApproveOnlyToken)
		}
	}
	var comments, tokens int
	if err := rig.database.Pool.QueryRow(t.Context(), `select count(*) from comments where body = 'an approve-only token''s comment'`).Scan(&comments); err != nil {
		t.Fatal(err)
	}
	if err := rig.database.Pool.QueryRow(t.Context(), `select count(*) from agent_tokens where name = 'minted through the proxy'`).Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	if comments != 0 || tokens != 0 {
		t.Fatalf("the refused writes recorded %d comments and %d tokens; want none", comments, tokens)
	}
}

// TestTheApproveListIsTheApproveRowsLessTheTokenMintsAndTheAnswer: the routes an approve-only token
// may call are every row of the route table that needs approve, less the mint and the change of a
// personal token - a token they make or widen would be an approve credential on the box - and
// with the answer of an ask, which checks approve in its handler for an approval request. The
// list is read against the table, so a row added later that needs approve is on it unless it is
// named here. The list names /api/v1 routes alone: an /api/v2 route needs approve too, but the
// token's one client (DispatchClient.reviewArtifact) calls /api/v1, and a route stays closed to
// the token until the list names it (refuseApproveOnlyToken), so every /api/v2 route refuses it
// 403 APPROVE_ONLY_TOKEN. The /api/v2 routes that take a value by state (PUT /api/v2/asks/{id}/state,
// PUT /api/v2/credential-requests/{id}/state) need that gate to hold the token to the approving
// values before the list can name them; the walk below holds every one of them refused until then.
func TestTheApproveListIsTheApproveRowsLessTheTokenMintsAndTheAnswer(t *testing.T) {
	rig := newBrokerTokenRig(t)
	var want []string
	for _, route := range approveRows(rig.served) {
		if route.Version == apiV2 {
			continue
		}
		name := route.Method + " " + route.Pattern
		if name == "POST /api/v1/me/agent-tokens" || name == "PATCH /api/v1/me/agent-tokens/{id}" {
			continue
		}
		want = append(want, name)
	}
	want = append(want, "POST /api/v1/asks/{id}/answer")
	var got []string
	for _, route := range rig.served.routes() {
		if route.ApproveOnly {
			got = append(got, route.Method+" "+route.Pattern)
		}
	}
	slices.Sort(want)
	slices.Sort(got)
	if len(want) < 10 || !slices.Equal(got, want) {
		t.Fatalf("the rows an approve-only token may call = %v; want %v", got, want)
	}
}

// TestEveryRowTheApproveListDoesNotNameRefusesAnApproveOnlyTokenBeforeItsHandler walks every row of
// the registered table, its test hooks in: each row the approve list does not name, and GET
// /ws/live, answers the approve-only token 403 APPROVE_ONLY_TOKEN, and each row it names answers
// anything but that refusal. A walk that walked nothing fails.
func TestEveryRowTheApproveListDoesNotNameRefusesAnApproveOnlyTokenBeforeItsHandler(t *testing.T) {
	rig := newBrokerTokenRigOn(t, testServerOptions{testHooks: true})
	caller := newBrokerCaller(t)
	const token = "an-approve-only-token-walking-every-row"
	rig.broker.Says(token, approveOnlyAnswer(caller.jkt))
	walked, listed := 0, 0
	walking := readOnlyWalkToken{name: "the approve-only token", token: token}
	for _, route := range rig.served.routes() {
		name := route.Method + " " + route.Pattern
		response := rig.serve(walking.probe(t, caller, route))
		refusal := decodeBody[refusalAnswer](t, response)
		refusedAsApproveOnly := response.Code == http.StatusForbidden && refusal.Code == contracts.DispatchApproveOnlyToken
		switch {
		case route.ApproveOnly && refusedAsApproveOnly:
			t.Errorf("%s is on the approve list and refused the approve-only token %s", name, response.Body.String())
		case !route.ApproveOnly && !refusedAsApproveOnly:
			t.Errorf("%s is not on the approve list and answered the approve-only token %d %s; want 403 %s", name, response.Code, response.Body.String(), contracts.DispatchApproveOnlyToken)
		}
		if route.ApproveOnly {
			listed++
		}
		walked++
	}
	live := rig.serve(caller.presenting(t, http.MethodGet, liveSocketPattern, token, nil))
	if refusal := decodeBody[refusalAnswer](t, live); live.Code != http.StatusForbidden || refusal.Code != contracts.DispatchApproveOnlyToken {
		t.Errorf("GET %s under the approve-only token = %d %s; want 403 %s", liveSocketPattern, live.Code, live.Body.String(), contracts.DispatchApproveOnlyToken)
	}
	if walked == 0 || listed == 0 {
		t.Fatalf("walked %d rows, %d of them on the approve list; want both above zero", walked, listed)
	}
	t.Logf("walked %d rows of the table (test hooks in) and GET %s: %d on the approve list, %d refused", walked, liveSocketPattern, listed, walked-listed+1)
}
