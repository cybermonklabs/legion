// approve_only_token_test.go holds Dispatch's side of a person's approvals through the secrets
// broker's proxy: the broker mints a token of the one scope approve for a person's approve grant,
// binds it to its own pod's key and spends it for the session, never releasing it. Dispatch serves
// that token, an approve-only one, as its person with the scopes person and approve on the routes
// its approve list names, and refuses it on every other route before the route runs. Every test
// here runs Dispatch's real registered server (newBrokerTokenRigOn), its real verifier and its real
// route table.
package api

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
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

// approveOnlyVersion is one version the approve-only token's tests drive: its path prefix, and the
// key of the project that version's issue is created under, so the two versions of a test never
// share an issue. The approve list names /api/v1 routes, and the token may call the /api/v2 route
// each has as its successor, so every proof of what the token does runs on both.
type approveOnlyVersion struct {
	prefix, project string
}

var (
	approveV1 = approveOnlyVersion{prefix: "/api/v1", project: "APO"}
	approveV2 = approveOnlyVersion{prefix: "/api/v2", project: "APT"}
)

// approveOnlyVersions is both versions, in the order a test that runs on each runs them.
var approveOnlyVersions = []approveOnlyVersion{approveV1, approveV2}

// approveOnlyRig is the registered server of a broker-token test, the one key every token it
// presents is bound to, and the two tokens the approve-only tests send: the approve-only token the
// secrets broker's proxy spends, and the person-scoped token of the box's own session, which holds
// the person scope and not approve. Both act as the same person.
type approveOnlyRig struct {
	*brokerTokenRig
	caller      *brokerCaller
	approveOnly string
	person      string
}

// newApproveOnlyRig is a broker-token rig on a server built from serving, with the broker saying
// what each of the two tokens is.
func newApproveOnlyRig(t *testing.T, serving testServerOptions) *approveOnlyRig {
	t.Helper()
	rig := &approveOnlyRig{
		brokerTokenRig: newBrokerTokenRigOn(t, serving),
		caller:         newBrokerCaller(t),
		approveOnly:    "an-approve-only-token",
		person:         "the-boxs-person-scoped-token",
	}
	rig.broker.Says(rig.approveOnly, approveOnlyAnswer(rig.caller.jkt))
	rig.broker.Says(rig.person, personScopedTokenAnswer(rig.caller.jkt))
	return rig
}

// send carries one request of token through the server, signed for method and path, with body.
func (rig *approveOnlyRig) send(t *testing.T, token, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return rig.serve(rig.caller.presenting(t, method, path, token, body))
}

// refusedAsApproveOnly reports whether response is the 403 APPROVE_ONLY_TOKEN that every
// approve-only check answers with. It reads the status and the code and never the message, which
// says nothing of who refused or why that a code does not, and it reads any answer, a list or an
// empty one among them, without failing on its shape.
func refusedAsApproveOnly(response *httptest.ResponseRecorder) bool {
	return response.Code == http.StatusForbidden && gateRefusalOf(response).Code == contracts.DispatchApproveOnlyToken
}

// TestAnApproveOnlyTokenApprovesAsItsPersonAndNothingElse: Dispatch's registered server answers
// the approve-only token's review of a document 201 and records the review as its person's, while
// the person-scoped token of the same session - the box's own - is refused 403
// OAUTH_SCOPE_REQUIRED naming approve on the same route. The approve-only token's comment on the
// issue, and its mint and change of a personal token, are refused 403 APPROVE_ONLY_TOKEN, and none
// of them is recorded. Each holds on /api/v1 and on /api/v2.
func TestAnApproveOnlyTokenApprovesAsItsPersonAndNothingElse(t *testing.T) {
	t.Parallel()
	rig := newApproveOnlyRig(t, testServerOptions{})
	for _, version := range approveOnlyVersions {
		t.Run(version.prefix, func(t *testing.T) {
			issue := createInteractionIssue(t, rig.handler, version.project, "Approve through the proxy", "A document to approve.\n")
			reviews := version.prefix + "/artifacts/" + issue.PrimaryArtifactID + "/reviews"

			refused := rig.send(t, rig.person, http.MethodPost, reviews, map[string]any{"state": "approved"})
			if refusal := decodeBody[refusalAnswer](t, refused); refused.Code != http.StatusForbidden || refusal.Code != apicode.OAuthScopeRequired.V1Name() {
				t.Fatalf("the person-scoped token's review = %d %s; want 403 %s", refused.Code, refused.Body.String(), apicode.OAuthScopeRequired.V1Name())
			}

			approved := rig.send(t, rig.approveOnly, http.MethodPost, reviews, map[string]any{"state": "approved"})
			if approved.Code != http.StatusCreated {
				t.Fatalf("the approve-only token's review = %d %s; want 201", approved.Code, approved.Body.String())
			}
			var review struct {
				State string `json:"state"`
				Actor struct {
					Kind string          `json:"kind"`
					ID   string          `json:"id"`
					Act  *model.ActorAct `json:"act"`
				} `json:"actor"`
			}
			if err := json.Unmarshal(approved.Body.Bytes(), &review); err != nil {
				t.Fatalf("decode the review: %v", err)
			}
			if review.Actor.Kind != "user" || review.Actor.ID != brokerTestPerson || review.State != "approved" {
				t.Fatalf("the review = %+v; want an approval by %s", review, brokerTestPerson)
			}
			// The act records the credential's scopes (actScopes): person and approve, never p0, which no
			// broker-issued token holds.
			if review.Actor.Act == nil || !slices.Equal(review.Actor.Act.Scopes, []string{model.ScopePerson, model.ScopeApprove}) {
				t.Fatalf("the review's act = %+v; want the scopes person and approve and no other", review.Actor.Act)
			}

			comment, tokenName := "an approve-only token's comment on "+version.prefix, "minted through the proxy on "+version.prefix
			for _, write := range []struct {
				method, path string
				body         any
			}{
				{http.MethodPost, version.prefix + "/issues/" + issue.Key + "/comments", map[string]any{"body": comment}},
				{http.MethodPost, version.prefix + "/me/agent-tokens", map[string]any{"name": tokenName, "scopes": []string{model.ScopePerson, model.ScopeApprove}}},
				{http.MethodPatch, version.prefix + "/me/agent-tokens/00000000-0000-4000-8000-000000000001", map[string]any{"scopes": []string{model.ScopePerson, model.ScopeApprove}}},
			} {
				response := rig.send(t, rig.approveOnly, write.method, write.path, write.body)
				if !refusedAsApproveOnly(response) {
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
// named here. It is written once, for /api/v1 (approveScopeRoutes), with the /api/v2 state of each
// entry beside it; the /api/v2 routes are the successors the table gives them, which the table's
// build stamps on its rows once (applyApprovePolicy), and this test names them from the rows that
// need approve, so the two lists drifting apart fails it. The list holds 11 /api/v1 routes and 9
// /api/v2 routes. The three /api/v2 routes that take several operations by state are listed with
// the states the token may send them: the others (a credential request's dismissal, an ask's
// resolve and decline) stay closed to it as they are on /api/v1, held by the state check
// (gateApproveOnlyStates).
func TestTheApproveListIsTheApproveRowsLessTheTokenMintsAndTheAnswer(t *testing.T) {
	t.Parallel()
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
	var onV1, onV2 int
	for _, route := range rig.served.routes() {
		name := route.Method + " " + route.Pattern
		if route.ApproveOnly {
			got = append(got, name)
			if route.Version == apiV1 {
				onV1++
			} else {
				onV2++
			}
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
	if onV1 != 11 || onV2 != 9 {
		t.Fatalf("the approve-only token may call %d /api/v1 rows and %d /api/v2 rows; want 11 and 9", onV1, onV2)
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
// /ws/live, answers the approve-only token 403 APPROVE_ONLY_TOKEN, and each row it names whole
// answers anything but that refusal. A row it names by state answers by the state sent, which
// TestAnApproveOnlyTokenIsHeldToTheStatesOfAByStateRoute walks. A walk that walked nothing fails.
func TestEveryRowTheApproveListDoesNotNameRefusesAnApproveOnlyTokenBeforeItsHandler(t *testing.T) {
	t.Parallel()
	rig := newApproveOnlyRig(t, testServerOptions{testHooks: true})
	walked, listed := 0, 0
	walking := readOnlyWalkToken{name: "the approve-only token", token: rig.approveOnly}
	for _, route := range rig.served.routes() {
		name := route.Method + " " + route.Pattern
		response := rig.serve(walking.probe(t, rig.caller, route))
		refused := refusedAsApproveOnly(response)
		switch {
		case route.ApproveStates != nil:
			// The state sent decides: the probe sends one arm of the row's union.
		case route.ApproveOnly && refused:
			t.Errorf("%s is on the approve list and refused the approve-only token %s", name, response.Body.String())
		case !route.ApproveOnly && !refused:
			t.Errorf("%s is not on the approve list and answered the approve-only token %d %s; want 403 %s", name, response.Code, response.Body.String(), contracts.DispatchApproveOnlyToken)
		}
		if route.ApproveOnly {
			listed++
		}
		walked++
	}
	live := rig.send(t, rig.approveOnly, http.MethodGet, liveSocketPattern, nil)
	if !refusedAsApproveOnly(live) {
		t.Errorf("GET %s under the approve-only token = %d %s; want 403 %s", liveSocketPattern, live.Code, live.Body.String(), contracts.DispatchApproveOnlyToken)
	}
	if walked == 0 || listed == 0 {
		t.Fatalf("walked %d rows, %d of them on the approve list; want both above zero", walked, listed)
	}
	t.Logf("walked %d rows of the table (test hooks in) and GET %s: %d on the approve list, %d refused", walked, liveSocketPattern, listed, walked-listed+1)
}

// TestAnApproveOnlyTokenIsHeldToTheStatesOfAByStateRoute walks every /api/v2 row the approve list
// names by state, and sends each state the row declares under the approve-only token: it is refused
// 403 APPROVE_ONLY_TOKEN exactly when the row's states for the token leave it out, so a state the
// list does not give the token cannot be reached by naming it. The states are read from each row's
// union, never listed here. A state the token may not send carries an Idempotency-Key, and none of
// those keys is claimed: the check runs before the keyed layer. A walk that sent no state the token
// may send, or none it may not, fails.
func TestAnApproveOnlyTokenIsHeldToTheStatesOfAByStateRoute(t *testing.T) {
	t.Parallel()
	rig := newApproveOnlyRig(t, testServerOptions{})
	const keys = "approve-only-state-"
	sent, left := 0, 0
	for _, route := range rig.served.routes() {
		if route.ApproveStates == nil {
			continue
		}
		path := readOnlyProbePath(route.Pattern)
		for _, state := range slices.Sorted(maps.Keys(route.Body.Arms)) {
			allowed := slices.Contains(route.ApproveStates, state)
			request := rig.caller.presenting(t, route.Method, path, rig.approveOnly, unionArmBody(t, route, state))
			if !allowed {
				request.Header.Set(contracts.IdempotencyKeyHeader, keys+strconv.Itoa(left))
			}
			response := rig.serve(request)
			if refusedAsApproveOnly(response) == allowed {
				t.Errorf("%s %s with state %s under the approve-only token = %d %s; refused as approve-only = %v, want %v",
					route.Method, route.Pattern, state, response.Code, response.Body.String(), !allowed, allowed)
			}
			if allowed {
				sent++
			} else {
				left++
			}
		}
	}
	if sent == 0 || left == 0 {
		t.Fatalf("the walk sent %d states the token may send and %d it may not; want both above zero", sent, left)
	}
	var claimed int
	if err := rig.database.Pool.QueryRow(t.Context(), `select count(*) from request_keys where idempotency_key like $1`, keys+"%").Scan(&claimed); err != nil {
		t.Fatal(err)
	}
	if claimed != 0 {
		t.Fatalf("the refused states claimed %d idempotency keys; want none", claimed)
	}
}
