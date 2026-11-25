package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/wire"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
)

// machineLoginsAnswered is the broker's answer to GET /v1/launcher-credentials in
// TestARestrictedMemberListsAndRevokesOnlyTheirOwnMachineLogins: erin's own machine login, one of
// alice's, a service's login erin approved, and a service's login alice approved.
const machineLoginsAnswered = `{"credentials":[
	{"credential_id":"cred-erin","host":"erin-laptop","service":null,"subject":null,"approved_by":"Erin@Other.example","issued_at":"2026-11-20T00:00:00Z","expires_at":"2026-12-20T00:00:00Z","expired":false},
	{"credential_id":"cred-alice","host":"alice-laptop","service":null,"subject":null,"approved_by":"alice","issued_at":"2026-11-20T00:00:00Z","expires_at":"2026-12-20T00:00:00Z","expired":false},
	{"credential_id":"cred-service-erin","host":"cluster.example","service":"legion-daemon","subject":null,"approved_by":"erin@other.example","issued_at":"2026-11-20T00:00:00Z","expires_at":"2026-12-20T00:00:00Z","expired":false},
	{"credential_id":"cred-service","host":"cluster.example","service":"legion-daemon","subject":null,"approved_by":"alice","issued_at":"2026-11-20T00:00:00Z","expires_at":"2026-12-20T00:00:00Z","expired":false}]}`

// credentialIDs is the credential ids a GET /api/v1/machine-logins answer lists, in its order.
func credentialIDs(t *testing.T, response *httptest.ResponseRecorder) []string {
	t.Helper()
	var listed wire.LauncherCredentialsResponse
	if err := json.Unmarshal(response.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode GET /api/v1/machine-logins %s: %v", response.Body.String(), err)
	}
	ids := []string{}
	for _, login := range listed.Credentials {
		ids = append(ids, login.CredentialID)
	}
	return ids
}

// TestARestrictedMemberListsAndRevokesOnlyTheirOwnMachineLogins: a restricted member at a browser
// lists, and revokes, only their own machine logins: a machine login whose approver is them and
// which names no service. erin's GET /api/v1/machine-logins answers her own login alone of the
// four the broker lists, and her revoke of it reaches the broker naming her as its approver. Her
// revoke of alice's login, of a service's login she approved and of one alice approved is each 403
// RESTRICTED_MEMBER, and no revoke reaches the broker. Every other caller is answered as before:
// alice, a full member, lists all four and revokes a service's login.
//
// Control: answer a restricted member as every other caller (the filter removed), and erin lists
// all four and her revoke of a service's login reaches the broker, each named.
func TestARestrictedMemberListsAndRevokesOnlyTheirOwnMachineLogins(t *testing.T) {
	t.Parallel()
	rig := newFakeBrokerRig(t)
	served, _ := newAccessServerWith(t, testServerOptions{
		admitsRestricted: true,
		agentSecretsURL:  rig.server.URL,
	})
	if listed := dispatchRequest(t, served, http.MethodPut, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice"); listed.Code != http.StatusOK {
		t.Fatalf("alice names erin on ACC's list: status=%d body=%s", listed.Code, listed.Body.String())
	}
	rig.bodyByRoute["GET /v1/launcher-credentials"] = machineLoginsAnswered

	listed := dispatchRequest(t, served, http.MethodGet, "/api/v1/machine-logins", nil, accessErin)
	if listed.Code != http.StatusOK || !slices.Equal(credentialIDs(t, listed), []string{"cred-erin"}) {
		t.Errorf("erin's browser on GET /api/v1/machine-logins = %d %s, want 200 listing cred-erin alone", listed.Code, listed.Body.String())
	}
	if !strings.Contains(rig.lastQuery, "approver=erin%40other.example") {
		t.Errorf("the list asked the broker with %q, want erin as the approver", rig.lastQuery)
	}

	for _, other := range []struct{ name, id string }{
		{"alice's machine login", "cred-alice"},
		{"a service's login erin approved", "cred-service-erin"},
		{"a service's login alice approved", "cred-service"},
		{"an id the broker does not list", "cred-unknown"},
	} {
		rig.routes = nil
		refused := dispatchRequest(t, served, http.MethodPost, "/api/v1/machine-logins/"+other.id+"/revoke", nil, accessErin)
		if refused.Code != http.StatusForbidden || !strings.Contains(refused.Body.String(), `"code":"RESTRICTED_MEMBER"`) || len(rig.revokes()) != 0 {
			t.Errorf("erin's revoke of %s = %d %s with %v sent to the broker, want 403 RESTRICTED_MEMBER and no revoke", other.name, refused.Code, refused.Body.String(), rig.revokes())
		}
	}

	rig.routes = nil
	revoked := dispatchRequest(t, served, http.MethodPost, "/api/v1/machine-logins/cred-erin/revoke", nil, accessErin)
	if revoked.Code != http.StatusOK || !slices.Equal(rig.revokes(), []string{"POST /v1/launcher-credentials/cred-erin/revoke-by-approver"}) ||
		!strings.Contains(rig.lastBody, `"approver":"erin@other.example"`) {
		t.Errorf("erin's revoke of her own login = %d %s, sent %v with body %s; want 200 and one revoke naming her", revoked.Code, revoked.Body.String(), rig.revokes(), rig.lastBody)
	}

	// Every other caller, as before.
	everyone := dispatchRequest(t, served, http.MethodGet, "/api/v1/machine-logins", nil, "alice")
	if everyone.Code != http.StatusOK || !slices.Equal(credentialIDs(t, everyone), []string{"cred-erin", "cred-alice", "cred-service-erin", "cred-service"}) {
		t.Errorf("alice's GET /api/v1/machine-logins = %d %s, want the broker's four", everyone.Code, everyone.Body.String())
	}
	rig.routes = nil
	if service := dispatchRequest(t, served, http.MethodPost, "/api/v1/machine-logins/cred-service/revoke", nil, "alice"); service.Code != http.StatusOK || len(rig.revokes()) != 1 {
		t.Errorf("alice's revoke of a service's login = %d %s with %v sent, want 200 and one revoke", service.Code, service.Body.String(), rig.revokes())
	}
}

// machineLoginsWithAPod is the broker's answer to GET /v1/launcher-credentials in
// TestARestrictedMemberListsAndRevokesThePodLoginsTheBrokerMapsToThem and
// TestARemovalRevokesTheMembersOwnLoginAndNotTheirPodsLogin: erin's own machine login, the login
// of a pod the broker's BROKER_POD_OPERATORS maps to erin (its person field names her, and no
// person approved it, as the broker lists a person's pod login), and a service's login.
const machineLoginsWithAPod = `{"credentials":[
	{"credential_id":"cred-erin","host":"erin-laptop","service":null,"subject":null,"approved_by":"Erin@Other.example","issued_at":"2026-11-20T00:00:00Z","expires_at":"2026-12-20T00:00:00Z","expired":false},
	{"credential_id":"cred-pod-erin","host":"agents/erin-pod","service":"erin-agent","subject":"system:serviceaccount:agents:erin-pod","person":"Erin@Other.example","approved_by":"","issued_at":"2026-11-20T00:00:00Z","expires_at":"2026-12-20T00:00:00Z","expired":false},
	{"credential_id":"cred-service","host":"cluster.example","service":"legion-daemon","subject":null,"approved_by":"alice","issued_at":"2026-11-20T00:00:00Z","expires_at":"2026-12-20T00:00:00Z","expired":false}]}`

// TestARestrictedMemberListsAndRevokesThePodLoginsTheBrokerMapsToThem: Dispatch holds a restricted
// member to the broker's own set for such a member (memberMachineLogins). erin's browser lists her
// own machine's login and the login of the pod the broker maps to her, and her revoke of the pod's
// login reaches the broker naming her; her revoke of a service's login is 403 RESTRICTED_MEMBER,
// and no revoke reaches the broker, so the broker's own 404 for it is never reached.
//
// Control: under the filter before this rule (a login naming her as its approver and no service,
// ownMachineLogins, in place of memberMachineLogins in listMachineLogins and revokeMachineLogin),
// the pod's login is neither listed nor revocable, and this test fails on both.
func TestARestrictedMemberListsAndRevokesThePodLoginsTheBrokerMapsToThem(t *testing.T) {
	t.Parallel()
	rig := newFakeBrokerRig(t)
	served, _ := newAccessServerWith(t, testServerOptions{
		admitsRestricted: true,
		agentSecretsURL:  rig.server.URL,
	})
	if listed := dispatchRequest(t, served, http.MethodPut, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice"); listed.Code != http.StatusOK {
		t.Fatalf("alice names erin on ACC's list: status=%d body=%s", listed.Code, listed.Body.String())
	}
	rig.bodyByRoute["GET /v1/launcher-credentials"] = machineLoginsWithAPod

	listed := dispatchRequest(t, served, http.MethodGet, "/api/v1/machine-logins", nil, accessErin)
	if listed.Code != http.StatusOK || !slices.Equal(credentialIDs(t, listed), []string{"cred-erin", "cred-pod-erin"}) {
		t.Errorf("erin's browser on GET /api/v1/machine-logins = %d %s, want 200 listing cred-erin and cred-pod-erin", listed.Code, listed.Body.String())
	}

	rig.routes = nil
	refused := dispatchRequest(t, served, http.MethodPost, "/api/v1/machine-logins/cred-service/revoke", nil, accessErin)
	if refused.Code != http.StatusForbidden || !strings.Contains(refused.Body.String(), `"code":"RESTRICTED_MEMBER"`) || len(rig.revokes()) != 0 {
		t.Errorf("erin's revoke of a service's login = %d %s with %v sent to the broker, want 403 RESTRICTED_MEMBER and no revoke", refused.Code, refused.Body.String(), rig.revokes())
	}

	rig.routes = nil
	revoked := dispatchRequest(t, served, http.MethodPost, "/api/v1/machine-logins/cred-pod-erin/revoke", nil, accessErin)
	if revoked.Code != http.StatusOK || !slices.Equal(rig.revokes(), []string{"POST /v1/launcher-credentials/cred-pod-erin/revoke-by-approver"}) ||
		!strings.Contains(rig.lastBody, `"approver":"erin@other.example"`) {
		t.Errorf("erin's revoke of her pod's login = %d %s, sent %v with body %s; want 200 and one revoke naming her", revoked.Code, revoked.Body.String(), rig.revokes(), rig.lastBody)
	}
}

// TestARestrictedMembersTokensListAndRevokeNoMachineLogin: the list and the revoke of a restricted
// member's own machine logins are their browser's alone. erin's personal token, and a broker token
// acting for her, are each refused GET /api/v1/machine-logins and the revoke of her own login, and
// no call reaches the broker; her browser is answered, so the refusal is the tokens'. Each refusal
// is the one the token's own scopes give (tokenRefusal): 403 OAUTH_SCOPE_REQUIRED, since neither
// token holds a scope (personalTokenActor, brokerTokenActor), and 403 RESTRICTED_MEMBER from a
// credential of hers that held the row's scopes, which
// TestTheOwnLoginRefusalHoldsARestrictedCredentialHoldingTheScopes holds.
//
// Control: give a restricted member's personal token the caller's scopes at its mint and keep them
// at its use (tokenScopes answering callerScopes for them, personalTokenActor keeping the row's
// scopes) and let any credential of a restricted member through (restrictedOwnLoginAtBrowser
// answering allowed true), and her personal token is answered, named.
func TestARestrictedMembersTokensListAndRevokeNoMachineLogin(t *testing.T) {
	t.Parallel()
	rig := newFakeBrokerRig(t)
	stub := ciphertokentest.NewStubBroker(t)
	served, _ := newAccessServerWith(t, testServerOptions{
		admitsRestricted: true, brokerTokens: stubBrokerTokens(t, stub),
		agentSecretsURL: rig.server.URL,
	})
	if listed := dispatchRequest(t, served, http.MethodPut, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice"); listed.Code != http.StatusOK {
		t.Fatalf("alice names erin on ACC's list: status=%d body=%s", listed.Code, listed.Body.String())
	}
	rig.bodyByRoute["GET /v1/launcher-credentials"] = machineLoginsAnswered
	minted := dispatchRequest(t, served, http.MethodPost, "/api/v1/me/agent-tokens", map[string]string{"name": "erin's agent"}, accessErin)
	if minted.Code != http.StatusCreated {
		t.Fatalf("erin mints a personal token at her browser: status=%d body=%s", minted.Code, minted.Body.String())
	}
	personal := decodeBody[agentTokenResponse](t, minted).Token
	caller := newBrokerCaller(t)
	const erinsBrokerToken = "erins-person-scoped-broker-token"
	answer := liveNamedToken(caller.jkt)
	answer.Person, answer.ProjectScope, answer.Projects = accessErin, ciphertoken.ProjectScopePerson, []string{"ACC"}
	stub.Says(erinsBrokerToken, answer)
	brokerWhoami := httptest.NewRecorder()
	served.ServeHTTP(brokerWhoami, caller.presenting(t, http.MethodGet, "/api/v1/whoami", erinsBrokerToken, nil))
	held := map[string][]string{
		"her personal token":            heldScopes(t, agentRequest(t, served, http.MethodGet, "/api/v1/whoami", nil, personal)),
		"a broker token acting for her": heldScopes(t, brokerWhoami),
	}

	for _, call := range []struct{ method, path, pattern string }{
		{http.MethodGet, "/api/v1/machine-logins", "/api/v1/machine-logins"},
		{http.MethodPost, "/api/v1/machine-logins/cred-erin/revoke", "/api/v1/machine-logins/{id}/revoke"},
	} {
		rig.routes = nil
		byToken := agentRequest(t, served, call.method, call.path, nil, personal)
		byBroker := httptest.NewRecorder()
		served.ServeHTTP(byBroker, caller.presenting(t, call.method, call.path, erinsBrokerToken, nil))
		for credential, response := range map[string]*httptest.ResponseRecorder{"her personal token": byToken, "a broker token acting for her": byBroker} {
			want := tokenRefusal(call.method, call.pattern, held[credential])
			if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), `"code":"`+want+`"`) {
				t.Errorf("%s %s with %s = %d %s, want 403 %s", call.method, call.path, credential, response.Code, response.Body.String(), want)
			}
		}
		if len(rig.routes) != 0 {
			t.Errorf("%s %s with erin's tokens reached the broker: %v", call.method, call.path, rig.routes)
		}
	}
	if browser := dispatchRequest(t, served, http.MethodGet, "/api/v1/machine-logins", nil, accessErin); browser.Code != http.StatusOK {
		t.Errorf("erin's browser on GET /api/v1/machine-logins = %d %s, want 200", browser.Code, browser.Body.String())
	}
}
