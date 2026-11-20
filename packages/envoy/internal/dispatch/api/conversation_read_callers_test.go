package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/brokertoken"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// expectSameCallersAsConversationRead holds route, a read that answers what one agent session's
// conversation holds, to the callers GET /api/v1/agents/{session_id}/messages admits: every kind
// of caller that read tells apart - no credential, a person's browser, a person's own token with
// and without the person scope, the deployment's shared token, and the broker's tokens for a
// person, for one project, for a service with no person and for a machine login - is answered by
// route exactly as it is answered by session's conversation. Both read `session` as every person
// route does, so the broker's token for a person, which proved a session, naming another is
// refused both, 403 ACTOR_SESSION_MISMATCH.
//
// Every such route calls this, so a kind of caller added to Dispatch is covered for all of them
// at once rather than written out again for each and forgotten for one. what names route in the
// failures ("the file list"), and the caller a request names is written as route's own path takes
// one, `&session=` where it already carries a query and `?session=` where it carries none.
// The rig's store is the caller's to seed before the call: the comparison is of who is answered,
// not of what the answer holds.
func expectSameCallersAsConversationRead(t *testing.T, rig *brokerTokenRig, what, session, route string) {
	t.Helper()
	caller := newBrokerCaller(t)

	broker := func(name string, says brokertoken.Introspection) string {
		token := "a-broker-token-" + name
		rig.broker.Says(token, says)
		return token
	}
	personScoped := liveNamedToken(caller.jkt)
	personScoped.ProjectScope, personScoped.Projects = brokertoken.ProjectScopePerson, nil
	brokerTokens := map[string]string{
		"a broker token for a person":        broker("person", personScoped),
		"a broker token for one project":     broker("project", liveNamedToken(caller.jkt)),
		"a broker token for a service":       broker("service", serviceOnlyToken(caller.jkt)),
		"a broker token for a machine login": broker("machine", machineLoginToken(caller.jkt)),
	}
	bearers := map[string]string{
		"the deployment's shared token":             "agent-token",
		"a person's token holding the person scope": personalTokenHolding(t, rig.database, model.ScopePerson),
		"a person's token holding no scope":         personalTokenHolding(t, rig.database),
	}

	conversation := "/api/v1/agents/" + session + "/messages"
	type answer struct {
		status int
		code   string
	}
	answerOf := func(response *httptest.ResponseRecorder) answer {
		if response.Code == http.StatusOK {
			return answer{status: http.StatusOK}
		}
		return answer{status: response.Code, code: decodeBody[refusalAnswer](t, response).Code}
	}
	plain := func(path string, set func(*http.Request)) answer {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		set(request)
		return answerOf(rig.serve(request))
	}

	answers := map[string][2]answer{}
	answers["no credential"] = [2]answer{
		plain(conversation, func(*http.Request) {}), plain(route, func(*http.Request) {}),
	}
	browser := func(request *http.Request) { request.Header.Set("X-Dispatch-User", "alice") }
	answers["a person's browser"] = [2]answer{plain(conversation, browser), plain(route, browser)}
	for name, token := range bearers {
		bearer := func(request *http.Request) { request.Header.Set("Authorization", "Bearer "+token) }
		answers[name] = [2]answer{plain(conversation, bearer), plain(route, bearer)}
	}
	for name, token := range brokerTokens {
		answers[name] = [2]answer{
			answerOf(rig.serve(caller.presenting(t, http.MethodGet, conversation, token, nil))),
			answerOf(rig.serve(caller.presenting(t, http.MethodGet, route, token, nil))),
		}
	}
	another := "?session=s-another"
	if strings.Contains(route, "?") {
		another = "&session=s-another"
	}
	mismatched := "a broker token for a person, naming another session"
	answers[mismatched] = [2]answer{
		answerOf(rig.serve(caller.presenting(t, http.MethodGet, conversation+"?session=s-another",
			brokerTokens["a broker token for a person"], nil))),
		answerOf(rig.serve(caller.presenting(t, http.MethodGet, route+another,
			brokerTokens["a broker token for a person"], nil))),
	}
	if refused := (answer{status: http.StatusForbidden, code: "ACTOR_SESSION_MISMATCH"}); answers[mismatched][1] != refused {
		t.Errorf("%s: %s answers %+v, want %+v", mismatched, what, answers[mismatched][1], refused)
	}

	admitted := 0
	for name, pair := range answers {
		if pair[0] != pair[1] {
			t.Errorf("%s: the conversation read answers %+v and %s %+v, want the same", name, pair[0], what, pair[1])
		}
		if pair[0].status == http.StatusOK {
			admitted++
		}
	}
	// The comparison proves nothing if every caller is refused both reads, or admitted to both.
	if admitted == 0 || admitted == len(answers) {
		t.Fatalf("the conversation read admits %d of %d callers, want some admitted and some refused: %+v",
			admitted, len(answers), answers)
	}
}
