package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/approvetimingtest"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestARestrictedMemberNeverDecidesACredentialRequest holds the gate a broker token rests on. The
// secrets broker names a machine login's token after the approver Dispatch sends on its approve
// route, on Dispatch's word (TestDecisionsNameTheCallerNeverTheBrowsersApprover; the broker's
// exchange.Set.Machine takes Person: approver). So the one way a broker token could name an outside
// address without an operator's exchange entry is a restricted member deciding a credential
// request through Dispatch. This holds that they never reach the broker there, and that a full
// member does, so the rig can see a call.
func TestARestrictedMemberNeverDecidesACredentialRequest(t *testing.T) {
	rig := newFakeBrokerRig(t)
	database := storetest.Open(t)
	const outsider = "erin@other.example"
	seedPeople(t, database, "alice", outsider)
	if _, err := database.Pool.Exec(context.Background(), `update people set member_kind = 'restricted' where email = $1`, outsider); err != nil {
		t.Fatal(err)
	}
	deps, err := NewDeps(DepsInput{
		Store:                 database,
		Identity:              headerIdentity(database),
		AgentTokens:           sharedAgentTokens(t, "agent-token"),
		ServerURL:             "https://dispatch.example",
		Events:                events.NewBroker(),
		AgentSecretsURL:       rig.server.URL,
		AgentSecretsTokenFile: accountTokenFile(t, credentialTestAccountToken),
	})
	if err != nil {
		t.Fatalf("new API dependencies: %v", err)
	}
	mux := http.NewServeMux()
	t.Cleanup(Register(mux, t.Context(), deps).Wait)
	served := approvetimingtest.Approves(rig.server.URL, mux)

	for _, path := range []string{"/api/v1/credential-requests/rec1/approve", "/api/v1/credential-requests/rec1/deny"} {
		response := dispatchRequest(t, served, http.MethodPost, path, map[string]any{"code": "ABCD-1234"}, outsider)
		t.Logf("POST %s as the restricted member: %d %s; broker calls so far %d", path, response.Code, strings.TrimSpace(response.Body.String()), rig.calls)
		if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "RESTRICTED_MEMBER") || rig.calls != 0 {
			t.Errorf("POST %s as %s answered %d %s and the broker was called %d times (last body %q), want 403 RESTRICTED_MEMBER and no call",
				path, outsider, response.Code, response.Body.String(), rig.calls, rig.lastBody)
		}
	}
	response := dispatchRequest(t, served, http.MethodPost, "/api/v1/credential-requests/rec1/approve", map[string]any{"code": "ABCD-1234"}, "alice")
	t.Logf("POST approve as the full member: %d; broker calls %d, last body %s", response.Code, rig.calls, rig.lastBody)
	if response.Code != http.StatusOK || rig.calls != 1 || rig.lastBody != `{"approver":"alice","code":"ABCD-1234","approver_session_generation":0}` {
		t.Errorf("the full member's approve answered %d with %d broker calls and body %q, want 200, one call naming alice", response.Code, rig.calls, rig.lastBody)
	}
}
