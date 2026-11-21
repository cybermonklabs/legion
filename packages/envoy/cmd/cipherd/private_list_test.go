// private_list_test.go drives GET /v1/private-projects through the REAL compiled broker, pointed
// at a Dispatch stand-in with BROKER_PROJECT_ACCESS_URL. cmd/cipherd wires no list to the route
// yet: the broker's list is the union of the projects its own settings declare private and the
// ones Dispatch answers private, held through an outage in one place, and that union is not in
// this broker. So the watch reads 503, never Dispatch's list alone, which would lack every declared
// key Dispatch does not also call private.
package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// TestTheBinaryWiresNoPrivateListYet: with Dispatch holding PRIV private and the broker able to ask
// it, the session-watch caller still reads 503 PRIVATE_LIST_UNKNOWN saying the list is not wired.
func TestTheBinaryWiresNoPrivateListYet(t *testing.T) {
	databaseURL := storetest.URL(t)
	cluster := oidctest.New(t)
	clusterKey := cluster.PublishKey(t, "signing-key")
	fakeSecretsFile := writeTempFile(t, "fake-secrets.json", []byte(`{"secrets": []}`))
	dispatch := projectAccessStandIn(t)
	tokenFile := writeTempFile(t, "dispatch-access-token", []byte(dispatch.Token+"\n"))
	broker := startBroker(t, brokerOptions{Env: []string{
		"BROKER_DATABASE_URL=" + databaseURL,
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_FAKE_SECRETS_FILE=" + fakeSecretsFile,
		"BROKER_K8S_OIDC_ISSUER=" + cluster.URL(),
		"BROKER_K8S_OIDC_AUDIENCE=" + enrolmentAudience,
		"BROKER_API_ACCOUNTS=session-watch=" + watchAccount,
		"BROKER_PROJECT_ACCESS_URL=" + dispatch.URL,
		"BROKER_PROJECT_ACCESS_TOKEN_FILE=" + tokenFile,
	}})
	live := newPlainBroker(broker.Addr)
	status, body := live.callAs(t, http.MethodGet, privateListRoute, accountToken(t, cluster, clusterKey, watchAccount), "", "")
	refusal, _ := body["error"].(string)
	if status != http.StatusServiceUnavailable || body["code"] != "PRIVATE_LIST_UNKNOWN" || !strings.Contains(refusal, "not wired") {
		t.Fatalf("GET %s as session-watch = %d %v, want 503 PRIVATE_LIST_UNKNOWN saying the list is not wired: this broker holds no union of its declared and Dispatch's private projects, so Dispatch's list alone would be short",
			privateListRoute, status, body)
	}
}
