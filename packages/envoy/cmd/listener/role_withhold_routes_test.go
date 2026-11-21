package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/logging"
)

// daemonAnswer is the broker's answer about a Legion daemon's own access token: the exchange
// account row of the daemon's account fixes the session legion-daemon:<project> and names that
// one project under the named scope, for the project's operator.
func daemonAnswer(project, jkt string) ciphertoken.Introspection {
	return ciphertoken.Introspection{
		Active:       true,
		Session:      "legion-daemon:" + project,
		Person:       "operator@example.com",
		ProjectScope: ciphertoken.ProjectScopeNamed,
		Projects:     []string{project},
		ClientID:     ciphertoken.ExchangeClientID,
		Thumbprint:   jkt,
	}
}

// The role withhold routes on a real bus, behind the listener's real credential check. A Legion
// daemon's own broker token withholds its project's role from a session and lifts the withhold;
// every other caller is refused 403 and changes nothing: the withheld session's own broker token,
// another session's, another project's daemon, the shared bearer, a projected service-account
// token, and a request with no credential on an open listener. While the role is withheld, the
// withheld session's claim is answered 409 role_withheld, and the role lookup answers withheld:
// true to that session alone, on the 404 of the unclaimed role and on the 200 once another
// session holds it, and names the withheld session to nobody else.
//
// Controls, each run on its own: admit a broker token that acts on its own session, as DELETE
// /v1/interests/<session_id> does, and the withheld session lifts its own withhold, which fails
// the test naming it; drop the acting-session check from the read, and another session reads
// withheld: true, which fails the test naming that session.
func TestTheWithholdRouteAnswersOnlyTheDaemonAndNamesNoOtherSession(t *testing.T) {
	rig := newBrokerRig(t)
	const (
		role          = "legion-infra-infra-12-implementer"
		withheld      = "ses_rig_withheld"
		other         = "ses_rig_other_session"
		daemonToken   = "access-token-of-the-infra-daemon"
		foreignToken  = "access-token-of-the-bet-daemon"
		withheldToken = "access-token-of-the-withheld-session"
		otherToken    = "access-token-of-another-session"
		shared        = "the-shared-bearer"
	)
	rig.broker.Says(daemonToken, daemonAnswer("INFRA", rig.caller.JKT()))
	rig.broker.Says(foreignToken, daemonAnswer("BET", rig.caller.JKT()))
	rig.broker.Says(withheldToken, sessionAnswer(withheld, "ada@example.com", rig.caller.JKT()))
	rig.broker.Says(otherToken, sessionAnswer(other, "grace@example.com", rig.caller.JKT()))
	rig.register(t, withheld)
	rig.register(t, other)
	cluster := newClusterIssuer(t)
	closed := apiAuth(listenerAPIAuth{token: shared, verifier: cluster.verifier, logger: logging.NewWithWriter("test-machine", &bytes.Buffer{})}, rig.routes)
	projected := cluster.token(t, "system:serviceaccount:legion:legion-worker")
	body := `{"role":"` + role + `","session":"` + withheld + `"}`

	asBearer := func(handler http.Handler, method, authorize, path, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Host = "127.0.0.1:9020"
		request.Header.Set("Content-Type", "application/json")
		if authorize != "" {
			request.Header.Set("Authorization", authorize)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}
	// refused is every caller but the daemon's own token, as one call of method on the route.
	refused := map[string]func(method string) *httptest.ResponseRecorder{
		"the withheld session's own broker token": func(m string) *httptest.ResponseRecorder {
			return rig.call(t, rig.handler, m, withheldToken, "/v1/role-withholds", body)
		},
		"another session's broker token": func(m string) *httptest.ResponseRecorder {
			return rig.call(t, rig.handler, m, otherToken, "/v1/role-withholds", body)
		},
		"another project's daemon": func(m string) *httptest.ResponseRecorder {
			return rig.call(t, rig.handler, m, foreignToken, "/v1/role-withholds", body)
		},
		"the shared bearer": func(m string) *httptest.ResponseRecorder {
			return asBearer(closed, m, "Bearer "+shared, "/v1/role-withholds", body)
		},
		"a projected service-account token": func(m string) *httptest.ResponseRecorder {
			return asBearer(closed, m, "Bearer "+projected, "/v1/role-withholds", body)
		},
		"no credential on an open listener": func(m string) *httptest.ResponseRecorder {
			return asBearer(rig.open, m, "", "/v1/role-withholds", body)
		},
	}
	claim := func(token, session string) *httptest.ResponseRecorder {
		return rig.send(t, token, "/v1/roles/set", `{"session_id":"`+session+`","role":"`+role+`"}`)
	}
	lookup := func(token string) *httptest.ResponseRecorder {
		return rig.call(t, rig.handler, http.MethodGet, token, "/v1/roles/"+role, "")
	}
	withheldIn := func(t *testing.T, who string, answer *httptest.ResponseRecorder) bool {
		t.Helper()
		var read struct {
			Withheld *bool `json:"withheld"`
		}
		if err := json.Unmarshal(answer.Body.Bytes(), &read); err != nil {
			t.Fatalf("decode the role lookup %s read: %v; body %s", who, err, answer.Body.String())
		}
		if read.Withheld != nil && !*read.Withheld {
			t.Fatalf("the role lookup %s read carries withheld: false, want the key left out; body %s", who, answer.Body.String())
		}
		return read.Withheld != nil
	}
	// readsAsOthers holds the lookup to answering every session but the withheld one with no
	// withhold and no word of the withheld session, under its own broker token and under the
	// open listener's stated session alike.
	readsAsOthers := func(t *testing.T, status int) {
		t.Helper()
		for who, answer := range map[string]*httptest.ResponseRecorder{
			other + " under its own token":        lookup(otherToken),
			other + " stated on an open listener": asBearer(rig.open, http.MethodGet, "", "/v1/roles/"+role+"?session_id="+other, ""),
			"a caller that states no session":     asBearer(rig.open, http.MethodGet, "", "/v1/roles/"+role, ""),
		} {
			if answer.Code != status || withheldIn(t, who, answer) || strings.Contains(answer.Body.String(), withheld) {
				t.Fatalf("the role lookup %s read: status %d %s; want %d, no withhold and no mention of %s", who, answer.Code, answer.Body.String(), status, withheld)
			}
		}
		for who, answer := range map[string]*httptest.ResponseRecorder{
			withheld + " under its own token":        lookup(withheldToken),
			withheld + " stated on an open listener": asBearer(rig.open, http.MethodGet, "", "/v1/roles/"+role+"?session_id="+withheld, ""),
		} {
			if answer.Code != status || !withheldIn(t, who, answer) {
				t.Fatalf("the role lookup %s read: status %d %s; want %d with withheld: true", who, answer.Code, answer.Body.String(), status)
			}
		}
		if answer := rig.call(t, rig.handler, http.MethodGet, otherToken, "/v1/roles/"+role+"?session_id="+withheld, ""); answer.Code != http.StatusForbidden {
			t.Fatalf("the role lookup under %s's token stating %s: status %d %s, want 403", other, withheld, answer.Code, answer.Body.String())
		}
	}

	if answer := claim(withheldToken, withheld); answer.Code != http.StatusOK {
		t.Fatalf("%s claims %s: status %d %s", withheld, role, answer.Code, answer.Body.String())
	}
	for name, call := range refused {
		if answer := call(http.MethodPut); answer.Code != http.StatusForbidden {
			t.Fatalf("PUT /v1/role-withholds under %s: status %d %s, want 403", name, answer.Code, answer.Body.String())
		}
	}
	if answer := lookup(withheldToken); answer.Code != http.StatusOK || withheldIn(t, withheld, answer) {
		t.Fatalf("the role lookup after every refused withhold: status %d %s, want %s still holding it", answer.Code, answer.Body.String(), withheld)
	}

	if answer := rig.call(t, rig.handler, http.MethodPut, daemonToken, "/v1/role-withholds", body); answer.Code != http.StatusNoContent {
		t.Fatalf("the INFRA daemon withholds %s from %s: status %d %s, want 204", role, withheld, answer.Code, answer.Body.String())
	}
	answer := claim(withheldToken, withheld)
	if answer.Code != http.StatusConflict || !strings.Contains(answer.Body.String(), `"error":"role_withheld"`) || !strings.Contains(answer.Body.String(), `"withheld":true`) {
		t.Fatalf("%s claims %s while it is withheld: status %d %s, want 409 role_withheld", withheld, role, answer.Code, answer.Body.String())
	}
	readsAsOthers(t, http.StatusNotFound)

	if answer := claim(otherToken, other); answer.Code != http.StatusOK {
		t.Fatalf("%s claims %s, withheld from another session only: status %d %s", other, role, answer.Code, answer.Body.String())
	}
	readsAsOthers(t, http.StatusOK)

	for name, call := range refused {
		if answer := call(http.MethodDelete); answer.Code != http.StatusForbidden {
			t.Fatalf("DELETE /v1/role-withholds under %s: status %d %s, want 403", name, answer.Code, answer.Body.String())
		}
		if answer := claim(withheldToken, withheld); answer.Code != http.StatusConflict {
			t.Fatalf("%s claims %s after a lift under %s was refused: status %d %s, want 409: the withhold stays", withheld, role, name, answer.Code, answer.Body.String())
		}
	}

	if answer := rig.call(t, rig.handler, http.MethodDelete, daemonToken, "/v1/role-withholds", body); answer.Code != http.StatusNoContent {
		t.Fatalf("the INFRA daemon lifts %s's withhold from %s: status %d %s, want 204", role, withheld, answer.Code, answer.Body.String())
	}
	if answer := lookup(withheldToken); answer.Code != http.StatusOK || withheldIn(t, withheld, answer) {
		t.Fatalf("the role lookup %s reads after the lift: status %d %s, want no withhold", withheld, answer.Code, answer.Body.String())
	}
	if answer := claim(withheldToken, withheld); answer.Code != http.StatusOK {
		t.Fatalf("%s claims %s after the lift: status %d %s, want 200", withheld, role, answer.Code, answer.Body.String())
	}
}
