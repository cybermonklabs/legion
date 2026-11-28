package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestV2DeclaredAuthority is c-authority of dispatch://INFRA-4071: the authority each state, field
// and query value of the /api/v2 routes needs holds at run time, declared on the routes' rows and
// held by their wrapper (gateV2Authorities) before the handler runs. A session token that holds no
// person scope - alice's personal token holding no scope, acting for tourSession - resolves a
// comment and an ask, and changes a project's gated steps, but is refused 403 OAUTH_SCOPE_REQUIRED
// naming person for a comment's accept, an ask's answer, an ask's asked_of, waiting_on=me,
// pinned=true, a claim's force and a project's default assignee; a person's token without approve
// is refused an approval naming approve; a broker token scoped to one project is refused an agent
// mark naming person, whichever session it marks, and one held to no project, which holds person,
// marks its own session's agent conversation and is refused 403 SESSION_SCOPE for another
// session's, on PATCH /api/v2/me/marks/{kind}/{id} and in an agent item of PATCH
// /api/v2/me/marks alike. A case whose route /api/v2 does not serve fails (requireMounted).
//
// Each refusal of a declared value must be the wrapper's own, whose text names the rule
// (assertGateRefused): the comment's accept and the ask's answer are refused by their handlers
// too, so a code alone would pass a wrapper that skipped them. Its negative control: a scratch
// build whose gateV2Authorities skips the state rules answers those two with the handlers' own
// refusal, which names no rule, and this test fails on both.
func TestV2DeclaredAuthority(t *testing.T) {
	tour := newRouteTour(t)
	scopeless := tourBearer(tour.scopelessToken)
	asSession := url.Values{"session": {tourSession}}

	t.Run("a comment's states", func(t *testing.T) {
		route := tour.requireMounted(t, http.MethodPut, "/comments/{id}/state")
		issue := tour.issue(t)
		// A bearer names its session in actor, as the resolve's own rule for a bearer writes it
		// (resolveCommentStateInput), beside the route map's {state: resolved}.
		resolved := tour.send(t, route, tourRequest{
			Path: map[string]string{"id": tour.comment(t, issue)}, Query: asSession,
			Body: map[string]any{"state": "resolved", "actor": map[string]any{"kind": "session", "id": tourSession}}, As: scopeless,
		}, "")
		if resolved.Code != http.StatusOK {
			t.Errorf("the session token resolving a comment: %d %s, want 200", resolved.Code, resolved.Body.String())
		}
		assertGateRefused(t, "the session token accepting a comment", tour.send(t, route, tourRequest{
			Path: map[string]string{"id": tour.comment(t, issue)}, Query: asSession,
			Body: map[string]any{"state": "accepted"}, As: scopeless,
		}, ""), model.ScopePerson, "state accepted")
	})

	t.Run("an ask's states", func(t *testing.T) {
		route := tour.requireMounted(t, http.MethodPut, "/asks/{id}/state")
		issue := tour.issue(t)
		// A resolve takes its reason, and a bearer names its session in actor, as the route map's
		// {state, reason} body and the resolve's own rule for a bearer write them.
		resolved := tour.send(t, route, tourRequest{
			Path: map[string]string{"id": tour.ask(t, issue)}, Query: asSession,
			Body: map[string]any{
				"state": "resolved", "reason": "Settled on the issue.",
				"actor": map[string]any{"kind": "session", "id": tourSession},
			}, As: scopeless,
		}, "")
		if resolved.Code != http.StatusOK {
			t.Errorf("the session token resolving an ask: %d %s, want 200", resolved.Code, resolved.Body.String())
		}
		assertGateRefused(t, "the session token answering an ask", tour.send(t, route, tourRequest{
			Path: map[string]string{"id": tour.ask(t, issue)}, Query: asSession,
			Body: map[string]any{"state": "answered", "text": "Yes.", "expected_edited_at": nil}, As: scopeless,
		}, ""), model.ScopePerson, "state answered")
	})

	t.Run("an ask's asked_of", func(t *testing.T) {
		route := tour.requireMounted(t, http.MethodPatch, "/asks/{id}")
		assertGateRefused(t, "the session token handing an ask to bob", tour.send(t, route, tourRequest{
			Path: map[string]string{"id": tour.ask(t, tour.issue(t))}, Query: asSession,
			Body: map[string]any{"asked_of": "bob"}, As: scopeless,
		}, ""), model.ScopePerson, "the member asked_of")
	})

	t.Run("the asks waiting on me", func(t *testing.T) {
		route := tour.requireMounted(t, http.MethodGet, "/asks")
		assertGateRefused(t, "the session token reading waiting_on=me", tour.send(t, route, tourRequest{
			Query: url.Values{"waiting_on": {"me"}, "session": {tourSession}}, As: scopeless,
		}, ""), model.ScopePerson, "waiting_on=me")
	})

	t.Run("the issues pinned by me", func(t *testing.T) {
		route := tour.requireMounted(t, http.MethodGet, "/issues")
		assertGateRefused(t, "the session token reading pinned=true", tour.send(t, route, tourRequest{
			Query: url.Values{"pinned": {"true"}, "session": {tourSession}}, As: scopeless,
		}, ""), model.ScopePerson, "pinned=true")
	})

	t.Run("a claim taken by force", func(t *testing.T) {
		// The holder's force is a check on the data the handler makes, not a declared value, so
		// the handler answers the scope refusal the gate would.
		route := tour.requireMounted(t, http.MethodPut, "/issues/{key}/claim")
		assertScopeRefused(t, "the session token forcing a claim", tour.send(t, route, tourRequest{
			Path: map[string]string{"key": tour.issue(t).Key}, Query: asSession,
			Body: map[string]any{"force": true, "actor": map[string]any{"kind": "session", "id": tourSession}}, As: scopeless,
		}, ""), model.ScopePerson)
	})

	t.Run("a project's fields", func(t *testing.T) {
		route := tour.requireMounted(t, http.MethodPatch, "/projects/{key}")
		project := tour.project(t)
		// A bearer names its session in actor, as the gated steps' own rule for a bearer writes it
		// (patchProjectV2Input, as projectGatedStepsInput), beside the route map's {gated_steps}.
		gated := tour.send(t, route, tourRequest{
			Path: map[string]string{"key": project}, Query: asSession,
			Body: map[string]any{"gated_steps": []string{"spec"}, "actor": map[string]any{"kind": "session", "id": tourSession}}, As: scopeless,
		}, "")
		if gated.Code != http.StatusOK {
			t.Errorf("the session token publishing gated steps: %d %s, want 200", gated.Code, gated.Body.String())
		}
		assertGateRefused(t, "the session token setting the default assignee", tour.send(t, route, tourRequest{
			Path: map[string]string{"key": project}, Query: asSession,
			Body: map[string]any{"default_assignee": "bob"}, As: scopeless,
		}, ""), model.ScopePerson, "the member default_assignee")
	})

	t.Run("an approval without approve, and a dismissal with person alone", func(t *testing.T) {
		route := tour.requireMounted(t, http.MethodPut, "/credential-requests/{id}/state")
		record := uuid.NewString()
		// A decision needs approve and a dismissal the person scope alone, so the route's own
		// scopes (its row's OAuthScopes) are person and the state's rule refuses the approval,
		// naming that rule.
		assertGateRefused(t, "a person's token without approve approving", tour.send(t, route, tourRequest{
			Path: map[string]string{"id": record}, Body: map[string]any{"state": "approved", "code": "ABCD-1234"},
			As: tourBearer(tour.personToken),
		}, ""), model.ScopeApprove, "state approved")
		if relayed := tour.secrets.callsTo(http.MethodPost, "/v1/credential-requests/"+record+"/approve"); relayed != 0 {
			t.Errorf("the refused approval reached the broker %d times, want never", relayed)
		}
		tour.secrets.answer(http.MethodPost, "/v1/credential-requests/"+record+"/dismiss", http.StatusOK, map[string]any{
			"record_id": record, "dismissed_by": "alice", "dismissed_at": "2026-11-19T00:00:00Z",
		})
		dismissed := tour.send(t, route, tourRequest{
			Path: map[string]string{"id": record}, Body: map[string]any{"state": "dismissed"},
			As: tourBearer(tour.personToken),
		}, "")
		if dismissed.Code != http.StatusOK {
			t.Errorf("a person's token without approve dismissing: %d %s, want 200", dismissed.Code, dismissed.Body.String())
		}
		if relayed := tour.secrets.callsTo(http.MethodPost, "/v1/credential-requests/"+record+"/dismiss"); relayed != 1 {
			t.Errorf("the dismissal reached the broker %d times, want once", relayed)
		}
	})

	t.Run("an agent mark by a broker token", func(t *testing.T) {
		// The rows need the person scope, which a session's token scoped to one project does not
		// hold, so the project-scoped token is refused naming person whichever session it marks,
		// its own among them. A token held to no one project holds person, and is held to the
		// session it proved though the rows declare OAuth scopes (refusesSessionOtherThanProved):
		// it marks its own session's agent conversation and is refused another session's.
		caller := newBrokerCaller(t)
		const scoped, unscoped = "a-broker-token-for-one-project", "a-broker-token-for-no-one-project"
		tour.brokerTokens.Says(scoped, liveNamedToken(caller.jkt))
		person := liveNamedToken(caller.jkt)
		person.ProjectScope, person.Projects = ciphertoken.ProjectScopePerson, nil
		tour.brokerTokens.Says(unscoped, person)
		for _, write := range []struct {
			name    string
			pattern string
			target  func(session string) (string, any)
		}{
			{"PATCH /api/v2/me/marks/agent/{id}", "/me/marks/{kind}/{id}", func(session string) (string, any) {
				return "/api/v2/me/marks/agent/" + session, map[string]any{"pinned": true}
			}},
			{"an agent item of PATCH /api/v2/me/marks", "/me/marks", func(session string) (string, any) {
				return "/api/v2/me/marks", map[string]any{"items": []map[string]any{{"kind": "agent", "id": session, "pinned": true}}}
			}},
		} {
			route := tour.requireMounted(t, http.MethodPatch, write.pattern)
			mark := func(token, session string) *httptest.ResponseRecorder {
				target, body := write.target(session)
				response := httptest.NewRecorder()
				tour.handler.ServeHTTP(response, caller.presenting(t, route.Method, target, token, body))
				return response
			}
			for _, session := range []string{"session-someone-else", brokerTestSession} {
				assertScopeRefused(t, write.name+" by a broker token scoped to one project marking "+session, mark(scoped, session), model.ScopePerson)
			}
			if own := mark(unscoped, brokerTestSession); own.Code != http.StatusOK {
				t.Errorf("%s by a broker token for %s held to no one project, marking its own session: %d %s, want 200",
					write.name, brokerTestSession, own.Code, own.Body.String())
			}
			refused := mark(unscoped, "session-someone-else")
			if code, text := refusalOf(t, refused); refused.Code != http.StatusForbidden || code != "SESSION_SCOPE" ||
				!strings.Contains(text, brokerTestSession) || !strings.Contains(text, "session-someone-else") {
				t.Errorf("%s by a broker token for %s held to no one project, marking another session: %d %s, "+
					"want 403 SESSION_SCOPE naming both sessions", write.name, brokerTestSession, refused.Code, refused.Body.String())
			}
		}
	})
}
