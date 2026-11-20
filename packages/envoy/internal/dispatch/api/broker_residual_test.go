package api

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/brokertoken"
	"github.com/sjawhar/envoy/internal/brokertoken/brokertokentest"
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
// holds to one project. Every other decision of theirs is 403 RESTRICTED_MEMBER and reaches the
// broker as no decision: another person's machine login, a shared record (approver anyone: a
// service's machine login, a shared secret's request), a record other than the one their code
// names, the lookup of any of those, every decision through their personal token or a broker token,
// their own login's included, and a dismissal. A full member's decision reaches the broker as it
// always has, so the rig can see a call.
//
// Controls, each run once: drop every clause but the record id's, and the rows of alice's, the
// service's and the shared records fail; drop the approver comparison alone, and alice's row fails;
// drop the service clause alone, and the row of the service's login naming erin fails; drop the
// kind clause alone, and the row of her own secret's request fails; let any credential of a
// restricted member past withinProjectScope (atBrowser answering true), and the rows of her
// personal token and her broker token fail.
func TestARestrictedMemberNeverDecidesACredentialRequest(t *testing.T) {
	rig := newFakeBrokerRig(t)
	stub := brokertokentest.NewStubBroker(t)
	served, _ := newAccessServerWith(t, testServerOptions{
		admitsRestricted: true, brokerTokens: stubBrokerTokens(t, stub),
		agentSecretsURL: rig.server.URL, agentSecretsTokenFile: accountTokenFile(t, credentialTestAccountToken),
	})
	if listed := dispatchRequest(t, served, http.MethodPut, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice"); listed.Code != http.StatusOK {
		t.Fatalf("alice names erin on ACC's list: status=%d body=%s", listed.Code, listed.Body.String())
	}
	const lookup = "POST /v1/machine-logins/lookup"
	code := map[string]any{"code": "ABCD-1234"}
	refused := func(response *httptest.ResponseRecorder, call string, decisionsBefore int) {
		t.Helper()
		body := response.Body.String()
		if response.Code != http.StatusForbidden || !strings.Contains(body, `"code":"RESTRICTED_MEMBER"`) || rig.decisions() != decisionsBefore {
			t.Errorf("%s as %s answered %d %s and %d decisions reached the broker, want 403 RESTRICTED_MEMBER and none",
				call, accessErin, response.Code, body, rig.decisions()-decisionsBefore)
		}
		if strings.Contains(body, "record_id") {
			t.Errorf("%s as %s answered %s, which carries the record's facts", call, accessErin, body)
		}
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
	minted := dispatchRequest(t, served, http.MethodPost, "/api/v1/me/agent-tokens", map[string]string{"name": "erin's agent"}, accessErin)
	if minted.Code != http.StatusCreated {
		t.Fatalf("erin mints a personal token at her browser: status=%d body=%s", minted.Code, minted.Body.String())
	}
	personal := decodeBody[agentTokenResponse](t, minted).Token
	caller := newBrokerCaller(t)
	const erinsBrokerToken = "erins-person-scoped-broker-token"
	answer := liveNamedToken(caller.jkt)
	answer.Person, answer.ProjectScope, answer.Projects = accessErin, brokertoken.ProjectScopePerson, nil
	stub.Says(erinsBrokerToken, answer)
	for _, action := range []string{"approve", "deny"} {
		path := "/api/v1/credential-requests/rec-erin/" + action
		before := rig.decisions()
		refused(agentRequest(t, served, http.MethodPost, path, code, personal), action+" of her own login with her personal token", before)
		byBroker := httptest.NewRecorder()
		served.ServeHTTP(byBroker, caller.presenting(t, http.MethodPost, path, erinsBrokerToken, code))
		refused(byBroker, action+" of her own login with a broker token acting as her", before)
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
		agentSecretsURL:  rig.server.URL, agentSecretsTokenFile: accountTokenFile(t, credentialTestAccountToken),
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
	revokes := func() []string {
		return slices.DeleteFunc(slices.Clone(rig.routes), func(route string) bool { return !strings.HasSuffix(route, "/revoke-by-approver") })
	}
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
	if owed, _, err := revocations.Owing(ctx, accessErin); err != nil || !owed || len(revokes()) != 0 {
		t.Fatalf("after a pass with the broker down the revocation is owed %t (%v) and the broker was sent %v, want it owed and no revoke", owed, err, revokes())
	}

	// The broker answers again; the claim's retry window is moved past, as its 30 s would.
	delete(rig.statusByRoute, list)
	rig.bodyByRoute[list] = `{"credentials":[
		{"credential_id":"cred-erin","host":"erin-laptop","service":null,"subject":null,"approved_by":"erin@other.example","issued_at":"2026-11-20T00:00:00Z","expires_at":"2026-12-20T00:00:00Z","expired":false},
		{"credential_id":"cred-daemon","host":"cluster.example","service":"legion-daemon","subject":null,"approved_by":"erin@other.example","issued_at":"2026-11-20T00:00:00Z","expires_at":"2026-12-20T00:00:00Z","expired":false}]}`
	if _, err := database.Pool.Exec(ctx, `update machine_login_revocations set tried_at = now() - interval '1 minute'`); err != nil {
		t.Fatal(err)
	}
	rig.routes = nil
	registered.revokeBrokerDebts(ctx)
	if !slices.Equal(revokes(), []string{"POST /v1/launcher-credentials/cred-erin/revoke-by-approver"}) {
		t.Errorf("the pass with the broker back revoked %v, want erin's own login alone", revokes())
	}
	if owed, _, err := revocations.Owing(ctx, accessErin); err != nil || owed {
		t.Errorf("the revocation of erin's machine logins is owed %t (%v) after the broker confirmed it, want it settled", owed, err)
	}
}
