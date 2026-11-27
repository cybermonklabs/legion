package api

import (
	"net/http"
	"reflect"
	"testing"
)

func TestAnUnboundPersonalDeployNamesItsProvedAccount(t *testing.T) {
	t.Parallel()
	rig := newBrokerTokenRig(t)
	minted := rig.asPerson(t, http.MethodPost, "/api/v1/me/agent-tokens", map[string]any{
		"name": "unbound-deployer", "scopes": []string{},
	})
	if minted.Code != http.StatusCreated {
		t.Fatalf("mint the personal token: %d %s", minted.Code, minted.Body)
	}
	token := decodeBody[struct {
		Token string `json:"token"`
	}](t, minted).Token
	before := rig.serve(brokerRequest(t, http.MethodGet, "/api/v1/whoami", nil, "Bearer "+token, ""))
	written := rig.serve(brokerRequest(t, http.MethodPost, "/api/v1/deploys", deployBody("worker", afterMergeC1), "Bearer "+token, ""))
	if written.Code != http.StatusCreated {
		t.Fatalf("the unbound personal token records a deploy: %d %s", written.Code, written.Body)
	}
	listed := rig.serve(brokerRequest(t, http.MethodGet, "/api/v1/deploys?repo=acme/widgets", nil, "Bearer "+token, ""))
	if listed.Code != http.StatusOK {
		t.Fatalf("read the deploys: %d %s", listed.Code, listed.Body)
	}
	deploys := decodeBody[deploysResponse](t, listed).Deploys
	if len(deploys) != 1 {
		t.Fatalf("deploys = %+v, want one recorded build", deploys)
	}
	writer := deploys[0].RecordedBy
	if writer.Kind != "user" || writer.ID != brokerTestPerson || !writer.Proven || writer.Act == nil {
		t.Fatalf("deploys must name the token's proved account and its act, got %+v", writer)
	}
	if writer.Act.TokenID == "" || writer.Act.TokenName != "unbound-deployer" || len(writer.Act.Scopes) != 0 {
		t.Fatalf("the personal token's audit provenance = %+v", writer.Act)
	}
	after := rig.serve(brokerRequest(t, http.MethodGet, "/api/v1/whoami", nil, "Bearer "+token, ""))
	if before.Code != http.StatusOK || after.Code != before.Code || !reflect.DeepEqual(decodeBody[map[string]any](t, before), decodeBody[map[string]any](t, after)) {
		t.Fatalf("the same token's identity and scopes changed: before %d %s, after %d %s", before.Code, before.Body, after.Code, after.Body)
	}
}

func TestADeployKeepsTheCredentialProvedSession(t *testing.T) {
	t.Parallel()
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "the-deployers-session-bound-token"
	rig.broker.Says(token, personScopedTokenAnswer(caller.jkt))
	written := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/deploys", token, deployBody("worker", afterMergeC1)))
	if written.Code != http.StatusCreated {
		t.Fatalf("the session-bound token records a deploy: %d %s", written.Code, written.Body)
	}
	writer := decodeBody[deployResponse](t, written).Deploy.RecordedBy
	if writer.Kind != "session" || writer.ID != brokerTestSession || writer.Owner == nil || *writer.Owner != brokerTestPerson || !writer.Proven {
		t.Fatalf("the credential-proved session was rewritten as another writer: %+v", writer)
	}
}

func TestDeployAttributionDoesNotChangeAReadOnlyTokensScope(t *testing.T) {
	t.Parallel()
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "the-read-only-deployers-token"
	rig.broker.Says(token, readOnlyToken(caller.jkt))
	for range 2 {
		refused := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/deploys", token, deployBody("worker", afterMergeC1)))
		if refused.Code != http.StatusForbidden || decodeBody[refusalAnswer](t, refused).Code != "READ_ONLY_TOKEN" {
			t.Fatalf("the unchanged read-only token must remain refused: %d %s", refused.Code, refused.Body)
		}
	}
}
