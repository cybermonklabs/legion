package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// TestTheMachineLoginKindIsTheBrokers: the kind ownMachineLogin holds a restricted member's
// decision to is the one the broker writes on a machine login's record, which Dispatch names
// rather than importing (machineLoginKind).
func TestTheMachineLoginKindIsTheBrokers(t *testing.T) {
	if machineLoginKind != record.KindLauncherCredential {
		t.Fatalf("machineLoginKind = %q, want the broker's record.KindLauncherCredential %q", machineLoginKind, record.KindLauncherCredential)
	}
}

// The machine-lookup answers TestARestrictedMemberNeverDecidesACredentialRequest's fake broker
// gives for the code erin types: her own pending machine login; a pending machine login of
// alice's; a service's machine login, which anyone signed in may deny; a shared secret's request,
// which anyone signed in may approve on the broker's page; and two records naming erin herself
// that are still not her machine's login, a service's machine login and a request for a secret
// she owns, each the one record a clause of the rule refuses alone.
const (
	erinsOwnLogin       = `{"record_id":"rec-erin","kind":"launcher_credential","state":"pending","approver":"erin@other.example","service":null}`
	alicesLogin         = `{"record_id":"rec-alice","kind":"launcher_credential","state":"pending","approver":"alice","service":null}`
	aServicesLogin      = `{"record_id":"rec-service","kind":"launcher_credential","state":"pending","approver":"anyone","service":"legion-daemon"}`
	aSharedSecretAsk    = `{"record_id":"rec-shared","kind":"agent_secret","state":"pending","approver":"anyone","service":null}`
	aServiceNamingErin  = `{"record_id":"rec-service-erin","kind":"launcher_credential","state":"pending","approver":"erin@other.example","service":"legion-daemon"}`
	aSecretErinApproves = `{"record_id":"rec-secret-erin","kind":"agent_secret","state":"pending","approver":"erin@other.example","service":null}`
)

// decisions is how many approves and denies the fake broker has been sent: the calls that decide
// something, apart from the lookups that only read a record.
func (rig *fakeBrokerRig) decisions() int {
	return len(slices.DeleteFunc(slices.Clone(rig.routes), func(route string) bool {
		return !strings.HasSuffix(route, "/approve") && !strings.HasSuffix(route, "/deny")
	}))
}

// revokes is the "METHOD path" of each revoke, a machine login's or a grant's, the fake broker has
// been sent, in order.
func (rig *fakeBrokerRig) revokes() []string {
	return slices.DeleteFunc(slices.Clone(rig.routes), func(route string) bool { return !strings.HasSuffix(route, "/revoke-by-approver") })
}

// tokenRefusal is the code a restricted member's token is refused with on the row method pattern
// of restrictedOwnLoginRoutes, read from the scopes Dispatch gives the token rather than assumed:
// 403 OAUTH_SCOPE_REQUIRED when the token lacks a scope the row needs (routeScopes), which the
// scope gate answers before any other check, and 403 RESTRICTED_MEMBER otherwise. held is the
// token's scopes as GET /api/v1/whoami answers them (heldScopes), so a row holds whatever scopes a
// broker token acting for its person is given.
func tokenRefusal(method, pattern string, held []string) string {
	for _, scope := range routeScopes[method+" "+pattern] {
		if !slices.Contains(held, scope) {
			return codeScopeRequired
		}
	}
	return codeRestrictedMember
}

// heldScopes is the scopes Dispatch gives a credential, read from whoami, its answer to
// GET /api/v1/whoami.
func heldScopes(t *testing.T, whoami *httptest.ResponseRecorder) []string {
	t.Helper()
	var who struct {
		Scopes []string `json:"scopes"`
	}
	if whoami.Code != http.StatusOK || json.Unmarshal(whoami.Body.Bytes(), &who) != nil {
		t.Fatalf("GET /api/v1/whoami = %d %s, want 200 and the credential's scopes", whoami.Code, whoami.Body.String())
	}
	return who.Scopes
}

// TestARestrictedMemberNeverDecidesACredentialRequest holds the gate a broker token rests on. The
// secrets broker names a machine login's token after the approver Dispatch sends on its approve
// route, on Dispatch's word (TestDecisionsNameTheCallerNeverTheBrowsersApprover; the broker's
// exchange.Set.Machine takes Person: approver). So the one way a broker token could name an outside
// address without an operator's exchange entry is a restricted member deciding a credential
// request through Dispatch.
//
// A restricted member decides exactly one kind of record, and only at a browser: their own pending
// machine login, one whose approver is that member and which names no service. Dispatch looks it up
// by the code the member typed and relays the approve or the deny of that record alone; that is how
// an outside person approves the device login of their own agents, whose tokens the broker then
// holds to one project. Every other decision of theirs is refused and reaches the broker as no
// decision: another person's machine login, a shared record (approver anyone: a service's machine
// login, a shared secret's request), a record other than the one their code names, an approve or
// a deny that carries no code, the lookup of any of those, and a dismissal, each 403
// RESTRICTED_MEMBER; and every decision through their personal token or a broker token, their own
// login's included, 403 OAUTH_SCOPE_REQUIRED, since neither holds a scope (personalTokenActor,
// brokerTokenActor), read from what whoami says each holds (tokenRefusal), and 403
// RESTRICTED_MEMBER from any credential of theirs that held the route's scopes, which
// TestTheOwnLoginRefusalHoldsARestrictedCredentialHoldingTheScopes holds. A full member's decision
// reaches the broker as it always has, so the rig can see a call.
//
// Controls, each run once: drop every clause but the record id's, and the rows of alice's, the
// service's and the shared records fail; drop the approver comparison alone, and alice's row fails;
// drop the service clause alone, and the row of the service's login naming erin fails; drop the
// kind clause alone, and the row of her own secret's request fails; give a restricted member's
// personal token the caller's scopes at its mint and keep them at its use (tokenScopes answering
// callerScopes for them, personalTokenActor keeping the row's scopes; either alone still leaves
// the token none), and the row fails naming the scopes her personal token holds; drop the refusal
// of a decision that carries no code, and the code-less rows fail.
func TestARestrictedMemberNeverDecidesACredentialRequest(t *testing.T) {
	rig := newFakeBrokerRig(t)
	stub := ciphertokentest.NewStubBroker(t)
	served, _ := newAccessServerWith(t, testServerOptions{
		admitsRestricted: true, brokerTokens: stubBrokerTokens(t, stub),
		agentSecretsURL: rig.server.URL,
	})
	if listed := dispatchRequest(t, served, http.MethodPut, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice"); listed.Code != http.StatusOK {
		t.Fatalf("alice names erin on ACC's list: status=%d body=%s", listed.Code, listed.Body.String())
	}
	const lookup = "POST /v1/machine-logins/lookup"
	code := map[string]any{"code": "ABCD-1234"}
	refusedWith := func(response *httptest.ResponseRecorder, call string, decisionsBefore int, want string) {
		t.Helper()
		body := response.Body.String()
		if response.Code != http.StatusForbidden || !strings.Contains(body, `"code":"`+want+`"`) || rig.decisions() != decisionsBefore {
			t.Errorf("%s as %s answered %d %s and %d decisions reached the broker, want 403 %s and none",
				call, accessErin, response.Code, body, rig.decisions()-decisionsBefore, want)
		}
		if strings.Contains(body, "record_id") {
			t.Errorf("%s as %s answered %s, which carries the record's facts", call, accessErin, body)
		}
	}
	refused := func(response *httptest.ResponseRecorder, call string, decisionsBefore int) {
		t.Helper()
		refusedWith(response, call, decisionsBefore, codeRestrictedMember)
	}

	for _, record := range []struct {
		name, lookedUp, id string
		lookupRefused      bool
	}{
		{"alice's machine login", alicesLogin, "rec-alice", true},
		{"a service's machine login, which anyone signed in may deny", aServicesLogin, "rec-service", true},
		{"a shared secret's request, which anyone signed in may approve", aSharedSecretAsk, "rec-shared", true},
		{"a service's machine login naming erin as its approver", aServiceNamingErin, "rec-service-erin", true},
		{"a request for a secret erin owns", aSecretErinApproves, "rec-secret-erin", true},
		{"alice's record, by the code of erin's own login", erinsOwnLogin, "rec-alice", false},
	} {
		rig.bodyByRoute[lookup] = record.lookedUp
		for _, action := range []string{"approve", "deny"} {
			before := rig.decisions()
			refused(dispatchRequest(t, served, http.MethodPost, "/api/v1/credential-requests/"+record.id+"/"+action, code, accessErin),
				action+" of "+record.name, before)
		}
		if record.lookupRefused {
			before := rig.decisions()
			refused(dispatchRequest(t, served, http.MethodPost, "/api/v1/credential-requests/machine-lookup", code, accessErin),
				"the lookup of "+record.name, before)
		}
	}

	rig.bodyByRoute[lookup] = erinsOwnLogin
	before := rig.decisions()
	refused(dispatchRequest(t, served, http.MethodPost, "/api/v1/credential-requests/rec-erin/dismiss", nil, accessErin), "the dismissal of her own login", before)
	// An approve or a deny of her own login that carries no code is refused before anything is looked
	// up: the code is what names a login as hers.
	for _, action := range []string{"approve", "deny"} {
		before := rig.decisions()
		refused(dispatchRequest(t, served, http.MethodPost, "/api/v1/credential-requests/rec-erin/"+action, map[string]any{}, accessErin),
			action+" of her own login with no code", before)
	}
	minted := dispatchRequest(t, served, http.MethodPost, "/api/v1/me/agent-tokens", map[string]string{"name": "erin's agent"}, accessErin)
	if minted.Code != http.StatusCreated {
		t.Fatalf("erin mints a personal token at her browser: status=%d body=%s", minted.Code, minted.Body.String())
	}
	personal := decodeBody[agentTokenResponse](t, minted).Token
	caller := newBrokerCaller(t)
	const erinsBrokerToken = "erins-person-scoped-broker-token"
	answer := liveNamedToken(caller.jkt)
	// The person scope with one project: a restricted member's broker token listing other than one
	// acts for nobody (restrictedBrokerTokenList).
	answer.Person, answer.ProjectScope, answer.Projects = accessErin, ciphertoken.ProjectScopePerson, []string{"ACC"}
	stub.Says(erinsBrokerToken, answer)
	whoami := httptest.NewRecorder()
	served.ServeHTTP(whoami, caller.presenting(t, http.MethodGet, "/api/v1/whoami", erinsBrokerToken, nil))
	held := heldScopes(t, whoami)
	personalHeld := heldScopes(t, agentRequest(t, served, http.MethodGet, "/api/v1/whoami", nil, personal))
	if len(personalHeld) != 0 {
		t.Errorf("erin's personal token holds %v, want no scope: a restricted member's token is their agent's", personalHeld)
	}
	for _, action := range []string{"approve", "deny"} {
		path := "/api/v1/credential-requests/rec-erin/" + action
		before := rig.decisions()
		byToken := agentRequest(t, served, http.MethodPost, path, code, personal)
		refusedWith(byToken, action+" of her own login with her personal token", before,
			tokenRefusal(http.MethodPost, "/api/v1/credential-requests/{id}/"+action, personalHeld))
		byBroker := httptest.NewRecorder()
		served.ServeHTTP(byBroker, caller.presenting(t, http.MethodPost, path, erinsBrokerToken, code))
		refusedWith(byBroker, action+" of her own login with a broker token acting as her", before,
			tokenRefusal(http.MethodPost, "/api/v1/credential-requests/{id}/"+action, held))
	}

	for _, action := range []string{"approve", "deny"} {
		before := rig.decisions()
		response := dispatchRequest(t, served, http.MethodPost, "/api/v1/credential-requests/rec-erin/"+action, code, accessErin)
		if response.Code != http.StatusOK || rig.decisions() != before+1 || !strings.Contains(rig.lastBody, `"approver":"erin@other.example","code":"ABCD-1234"`) {
			t.Errorf("erin's browser %s of her own pending machine login answered %d %s with %d decisions reaching the broker (last body %s), want 200 and one naming her",
				action, response.Code, response.Body.String(), rig.decisions()-before, rig.lastBody)
		}
	}
	looked := dispatchRequest(t, served, http.MethodPost, "/api/v1/credential-requests/machine-lookup", code, accessErin)
	if looked.Code != http.StatusOK || looked.Body.String() != erinsOwnLogin {
		t.Errorf("erin's lookup of her own pending machine login answered %d %s, want 200 and the broker's record verbatim", looked.Code, looked.Body.String())
	}

	before = rig.decisions()
	response := dispatchRequest(t, served, http.MethodPost, "/api/v1/credential-requests/rec1/approve", code, "alice")
	if response.Code != http.StatusOK || rig.decisions() != before+1 || rig.lastBody != `{"approver":"alice","code":"ABCD-1234","approver_session_generation":0}` {
		t.Errorf("the full member's approve answered %d with %d decisions reaching the broker and body %q, want 200, one naming alice", response.Code, rig.decisions()-before, rig.lastBody)
	}
}

// TestARestrictedMembersPersonScopeBrokerTokenHoldsNoScope: a broker token the broker scoped to a
// restricted member's person holds no scope at Dispatch, as their named token holds none. A broker
// whose project check is off, an older broker during a rollout, or a token minted before the broker
// held restricted members to one project can still answer such a token; Dispatch read the member's
// kind for it in the same request (brokerPersonAdmitted), so it gives the token no scope: whoami
// answers none, and GET /api/v1/inbox and POST /api/v1/me/agent-tokens are 403
// OAUTH_SCOPE_REQUIRED. So an outside person's agent mints no token and decides nothing, whatever
// the broker answered. erin's token lists ACC alone, the one project such a token may list
// (restrictedBrokerTokenList). A full member's person-scope token naming no project, the control,
// holds the person scope and reads its inbox.
//
// Control: brokerTokenActor giving a person-scope token the person's scopes whatever their member
// kind, and erin's token holds person and approve and reads her inbox, named.
func TestARestrictedMembersPersonScopeBrokerTokenHoldsNoScope(t *testing.T) {
	stub := ciphertokentest.NewStubBroker(t)
	served, _ := newAccessServerWith(t, testServerOptions{admitsRestricted: true, brokerTokens: stubBrokerTokens(t, stub)})
	if listed := dispatchRequest(t, served, http.MethodPut, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice"); listed.Code != http.StatusOK {
		t.Fatalf("alice names erin on ACC's list: status=%d body=%s", listed.Code, listed.Body.String())
	}
	caller := newBrokerCaller(t)
	for _, c := range []struct {
		person, token string
		projects      []string
		restricted    bool
	}{
		{accessErin, "erins-person-scoped-broker-token", []string{"ACC"}, true},
		{accessCarol, "carols-person-scoped-broker-token", nil, false},
	} {
		answer := liveNamedToken(caller.jkt)
		answer.Person, answer.ProjectScope, answer.Projects = c.person, ciphertoken.ProjectScopePerson, c.projects
		stub.Says(c.token, answer)
		whoami := httptest.NewRecorder()
		served.ServeHTTP(whoami, caller.presenting(t, http.MethodGet, "/api/v1/whoami", c.token, nil))
		held := heldScopes(t, whoami)
		inbox := httptest.NewRecorder()
		served.ServeHTTP(inbox, caller.presenting(t, http.MethodGet, "/api/v1/inbox", c.token, nil))
		if !c.restricted {
			if !slices.Contains(held, model.ScopePerson) || inbox.Code != http.StatusOK {
				t.Errorf("%s's person-scope broker token holds %v and reads the inbox %d %s, want the person scope and 200", c.person, held, inbox.Code, inbox.Body.String())
			}
			continue
		}
		if len(held) != 0 {
			t.Errorf("%s's person-scope broker token holds %v, want no scope: %s is a restricted member", c.person, held, c.person)
		}
		minted := httptest.NewRecorder()
		served.ServeHTTP(minted, caller.presenting(t, http.MethodPost, "/api/v1/me/agent-tokens", c.token, map[string]string{"name": "erin's agent's token"}))
		for call, response := range map[string]*httptest.ResponseRecorder{"GET /api/v1/inbox": inbox, "POST /api/v1/me/agent-tokens": minted} {
			if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), `"code":"`+codeScopeRequired+`"`) {
				t.Errorf("%s with %s's person-scope broker token = %d %s, want 403 %s", call, c.person, response.Code, response.Body.String(), codeScopeRequired)
			}
		}
	}
}

// TestARemovalWhileTheBrokerIsDownIsPaidOnceItIsBack: taking a restricted member off an access
// list never waits on the secrets broker. While the broker does not answer, alice's removal of erin
// from ACC answers 200 and erin is off the list; from the commit on, erin's browser sees nothing of
// ACC and her personal token is refused, by Dispatch's own check of the lists; and the revocation
// of her machine logins is owed (recordMachineLoginRevocation). A pass of the broker revocation
// loop that meets the broker
// down revokes nothing and leaves it owed. Once the broker answers, the next pass revokes erin's own
// login and no service's, whoever approved the service's, and settles the debt.
//
// Control: refuse the removal while the broker is down, as the head before this row did, and the
// first row fails naming the 503.
func TestARemovalWhileTheBrokerIsDownIsPaidOnceItIsBack(t *testing.T) {
	rig := newFakeBrokerRig(t)
	served, database, registered := newRegisteredAccessServer(t, testServerOptions{
		admitsRestricted: true,
		agentSecretsURL:  rig.server.URL,
	})
	ctx := t.Context()
	if listed := dispatchRequest(t, served, http.MethodPut, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice"); listed.Code != http.StatusOK {
		t.Fatalf("alice names erin on ACC's list: status=%d body=%s", listed.Code, listed.Body.String())
	}
	minted := dispatchRequest(t, served, http.MethodPost, "/api/v1/me/agent-tokens", map[string]string{"name": "erin's agent"}, accessErin)
	if minted.Code != http.StatusCreated {
		t.Fatalf("erin mints a personal token at her browser: status=%d body=%s", minted.Code, minted.Body.String())
	}
	personal := decodeBody[agentTokenResponse](t, minted).Token
	const list = "GET /v1/launcher-credentials"
	revocations := store.NewMachineLoginRevocations(database.Pool)

	seesACC := func() bool {
		t.Helper()
		projects := dispatchRequest(t, served, http.MethodGet, "/api/v1/projects", nil, accessErin)
		if projects.Code != http.StatusOK {
			t.Fatalf("erin's browser on GET /api/v1/projects = %d %s, want 200", projects.Code, projects.Body.String())
		}
		return strings.Contains(projects.Body.String(), `"key":"ACC"`)
	}
	if !seesACC() {
		t.Fatalf("erin's browser does not see ACC while ACC lists her")
	}
	rig.statusByRoute[list] = http.StatusServiceUnavailable
	removed := dispatchRequest(t, served, http.MethodDelete, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice")
	if removed.Code != http.StatusOK || strings.Contains(removed.Body.String(), accessErin) {
		t.Fatalf("erin's removal while the broker does not answer = %d %s; want 200 and erin off the list", removed.Code, removed.Body.String())
	}
	if seesACC() {
		t.Errorf("erin's browser sees ACC after her removal, want it gone from the commit on")
	}
	if token := agentRequest(t, served, http.MethodGet, "/api/v1/whoami", nil, personal); token.Code != http.StatusUnauthorized ||
		!strings.Contains(token.Body.String(), codeTokenOwnerNotAdmitted) {
		t.Errorf("erin's personal token on GET /api/v1/whoami after her removal = %d %s, want 401 %s", token.Code, token.Body.String(), codeTokenOwnerNotAdmitted)
	}
	if owed, _, err := revocations.Owing(ctx, accessErin); err != nil || !owed {
		t.Fatalf("the revocation of erin's machine logins is owed %t (%v) after her removal, want owed", owed, err)
	}
	registered.revokeBrokerDebts(ctx)
	if owed, _, err := revocations.Owing(ctx, accessErin); err != nil || !owed || len(rig.revokes()) != 0 {
		t.Fatalf("after a pass with the broker down the revocation is owed %t (%v) and the broker was sent %v, want it owed and no revoke", owed, err, rig.revokes())
	}

	// The broker answers again a minute on: past the claim's retry window and the broker's bound
	// on an approve it took before the removal (machineLoginSettleAfter), as that minute would.
	delete(rig.statusByRoute, list)
	rig.bodyByRoute[list] = `{"credentials":[
		{"credential_id":"cred-erin","host":"erin-laptop","service":null,"subject":null,"approved_by":"erin@other.example","issued_at":"2026-11-20T00:00:00Z","expires_at":"2026-12-20T00:00:00Z","expired":false},
		{"credential_id":"cred-daemon","host":"cluster.example","service":"legion-daemon","subject":null,"approved_by":"erin@other.example","issued_at":"2026-11-20T00:00:00Z","expires_at":"2026-12-20T00:00:00Z","expired":false}]}`
	removalAged(t, database, machineLoginSettleAfter)
	rig.routes = nil
	registered.revokeBrokerDebts(ctx)
	if !slices.Equal(rig.revokes(), []string{"POST /v1/launcher-credentials/cred-erin/revoke-by-approver"}) {
		t.Errorf("the pass with the broker back revoked %v, want erin's own login alone", rig.revokes())
	}
	if owed, _, err := revocations.Owing(ctx, accessErin); err != nil || owed {
		t.Errorf("the revocation of erin's machine logins is owed %t (%v) after the broker confirmed it, want it settled", owed, err)
	}
}
