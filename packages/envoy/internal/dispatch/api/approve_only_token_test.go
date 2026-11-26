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
	"maps"
	"net/http"
	"slices"
	"strings"
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

// approveOnlyVersions is the path prefix of each version the approve-only token's tests drive: the
// approve list names /api/v1 routes, and the token may call the /api/v2 route each has as its
// successor (approveV2Routes), so every proof of what the token does runs on both.
var approveOnlyVersions = []string{"/api/v1", "/api/v2"}

// TestAnApproveOnlyTokenApprovesAsItsPersonAndNothingElse: Dispatch's registered server answers
// the approve-only token's review of a document 201 and records the review as its person's, while
// the person-scoped token of the same session - the box's own - is refused 403
// OAUTH_SCOPE_REQUIRED naming approve on the same route. The approve-only token's comment on the
// issue, and its mint and change of a personal token, are refused 403 APPROVE_ONLY_TOKEN, and none
// of them is recorded. Each holds on /api/v1 and on /api/v2.
func TestAnApproveOnlyTokenApprovesAsItsPersonAndNothingElse(t *testing.T) {
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const approveOnly, person = "an-approve-only-token", "the-boxs-person-scoped-token"
	rig.broker.Says(approveOnly, approveOnlyAnswer(caller.jkt))
	rig.broker.Says(person, personScopedTokenAnswer(caller.jkt))
	for index, prefix := range approveOnlyVersions {
		t.Run(prefix, func(t *testing.T) {
			issue := createInteractionIssue(t, rig.handler, []string{"APR", "APS"}[index], "Approve through the proxy", "A document to approve.\n")
			reviews := prefix + "/artifacts/" + issue.PrimaryArtifactID + "/reviews"

			refused := rig.serve(caller.presenting(t, http.MethodPost, reviews, person, map[string]any{"state": "approved"}))
			if refusal := decodeBody[refusalAnswer](t, refused); refused.Code != http.StatusForbidden || refusal.Code != apicode.OAuthScopeRequired.V1Name() {
				t.Fatalf("the person-scoped token's review = %d %s; want 403 %s", refused.Code, refused.Body.String(), apicode.OAuthScopeRequired.V1Name())
			}

			approved := rig.serve(caller.presenting(t, http.MethodPost, reviews, approveOnly, map[string]any{"state": "approved"}))
			if approved.Code != http.StatusCreated {
				t.Fatalf("the approve-only token's review = %d %s; want 201", approved.Code, approved.Body.String())
			}
			var review struct {
				State string `json:"state"`
				Actor struct {
					Kind string `json:"kind"`
					ID   string `json:"id"`
				} `json:"actor"`
			}
			if err := json.Unmarshal(approved.Body.Bytes(), &review); err != nil {
				t.Fatalf("decode the review: %v", err)
			}
			if review.Actor.Kind != "user" || review.Actor.ID != brokerTestPerson || review.State != "approved" {
				t.Fatalf("the review = %+v; want an approval by %s", review, brokerTestPerson)
			}

			comment, tokenName := "an approve-only token's comment on "+prefix, "minted through the proxy on "+prefix
			for _, write := range []struct {
				method, path string
				body         any
			}{
				{http.MethodPost, prefix + "/issues/" + issue.Key + "/comments", map[string]any{"body": comment}},
				{http.MethodPost, prefix + "/me/agent-tokens", map[string]any{"name": tokenName, "scopes": []string{model.ScopePerson, model.ScopeApprove}}},
				{http.MethodPatch, prefix + "/me/agent-tokens/00000000-0000-4000-8000-000000000001", map[string]any{"scopes": []string{model.ScopePerson, model.ScopeApprove}}},
			} {
				response := rig.serve(caller.presenting(t, write.method, write.path, approveOnly, write.body))
				if refusal := decodeBody[refusalAnswer](t, response); response.Code != http.StatusForbidden || refusal.Code != contracts.DispatchApproveOnlyToken {
					t.Errorf("%s %s under the approve-only token = %d %s; want 403 %s", write.method, write.path, response.Code, response.Body.String(), contracts.DispatchApproveOnlyToken)
				}
			}
			var comments, tokens int
			if err := rig.database.Pool.QueryRow(t.Context(), `select count(*) from comments where body = $1`, comment).Scan(&comments); err != nil {
				t.Fatal(err)
			}
			if err := rig.database.Pool.QueryRow(t.Context(), `select count(*) from agent_tokens where name = $1`, tokenName).Scan(&tokens); err != nil {
				t.Fatal(err)
			}
			if comments != 0 || tokens != 0 {
				t.Fatalf("the refused writes recorded %d comments and %d tokens; want none", comments, tokens)
			}
		})
	}
}

// TestTheApproveListIsTheApproveRowsLessTheTokenMintsAndTheAnswer: the routes an approve-only token
// may call are, on each version, every row of the route table that needs approve, less the mint and
// the change of a personal token - a token they make or widen would be an approve credential on the
// box - and with the answer of an ask, which checks approve in its handler for an approval request.
// The list is read against the table, so a row added later that needs approve is on it unless it is
// named here. It is written once, for /api/v1 (approveScopeRoutes); the /api/v2 routes are the
// successors the table gives them (approveV2Routes), and this test names them from the rows that
// need approve, so the two lists drifting apart fails it. The three /api/v2 routes that take
// several operations by state are listed with the states the token may send them: the others
// (a credential request's dismissal, an ask's resolve and decline) stay closed to it as they are on
// /api/v1, held by the authority gate (gateV2Authorities).
func TestTheApproveListIsTheApproveRowsLessTheTokenMintsAndTheAnswer(t *testing.T) {
	rig := newBrokerTokenRig(t)
	var want []string
	for _, route := range approveRows(rig.served) {
		name := route.Method + " " + route.Pattern
		if name == "POST /api/v1/me/agent-tokens" || name == "PATCH /api/v1/me/agent-tokens/{id}" ||
			name == "POST /api/v2/me/agent-tokens" || name == "PATCH /api/v2/me/agent-tokens/{id}" {
			continue
		}
		want = append(want, name)
	}
	want = append(want, "POST /api/v1/asks/{id}/answer", "PUT /api/v2/asks/{id}/state")
	var got []string
	gotStates := map[string][]string{}
	for _, route := range rig.served.routes() {
		name := route.Method + " " + route.Pattern
		if route.ApproveOnly {
			got = append(got, name)
		}
		if route.ApproveStates != nil {
			gotStates[name] = route.ApproveStates
		}
	}
	slices.Sort(want)
	slices.Sort(got)
	if len(want) < 15 || !slices.Equal(got, want) {
		t.Fatalf("the rows an approve-only token may call = %v; want %v", got, want)
	}
	wantStates := map[string][]string{
		"PUT /api/v2/asks/{id}/state":                         {"answered"},
		"PUT /api/v2/credential-requests/{id}/state":          {"approved", "denied"},
		"PUT /api/v2/issues/{key}/decisions/{block_id}/state": {"kept"},
	}
	if !maps.EqualFunc(gotStates, wantStates, slices.Equal) {
		t.Fatalf("the states an approve-only token may send a state route = %v; want %v", gotStates, wantStates)
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
		case route.ApproveStates != nil:
			// A row the list names by state lets the token past the door and holds it to those states
			// once its body is read, so the probe's arm is refused or not by its own state; the
			// refusal it may answer is the state's (approve_only_state_routes_test.go says which states).
			if refusedAsApproveOnly && !strings.Contains(refusal.Error, "may send "+name) {
				t.Errorf("%s is on the approve list by state and refused the approve-only token as a route off the list: %s", name, response.Body.String())
			}
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
