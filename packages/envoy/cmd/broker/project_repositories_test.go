// project_repositories_test.go drives GET /v1/project-repositories on the REAL compiled broker,
// over HTTP, on a real Postgres, with the edge listener's entry read from BROKER_NATS_SERVICES and
// Dispatch's answer read through BROKER_PROJECT_ACCESS_URL and its token file, as a deployment
// sets them: what cmd/broker wires, and not only what the API package does with what it is given.
package main

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/nats-io/nkeys"

	"github.com/sjawhar/envoy/internal/broker/projectrepos/projectrepostest"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// edgeListenerAccount is the ServiceAccount BROKER_NATS_SERVICES lists as the edge listener of
// PRIV.
const edgeListenerAccount = "system:serviceaccount:example-ns:example-edge-listener"

// TestTheBrokerBinaryRelaysAnEdgeListenersRepositories: the edge listener BROKER_NATS_SERVICES
// lists for PRIV is answered PRIV's list as the Dispatch at BROKER_PROJECT_ACCESS_URL answers it,
// presenting the bearer of BROKER_PROJECT_ACCESS_TOKEN_FILE (the stand-in refuses any other, which
// the broker answers 503); the Envoy listener, listed in both BROKER_NATS_SERVICES and
// BROKER_API_ACCOUNTS, is refused; a Dispatch that is down is 503 REPOSITORIES_UNKNOWN; and a
// broker with no BROKER_PROJECT_ACCESS_URL answers the same edge listener 503
// REPOSITORIES_UNKNOWN, so the answers come from the wiring and not from the route alone.
func TestTheBrokerBinaryRelaysAnEdgeListenersRepositories(t *testing.T) {
	databaseURL := storetest.URL(t)
	cluster := oidctest.New(t)
	clusterKey := cluster.PublishKey(t, "signing-key")
	dispatch := projectrepostest.NewServer(t, map[string][]string{"PRIV": {"acme/pub", "acme/two"}})
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "dispatch-access-token")
	fakeSecretsFile := filepath.Join(dir, "fake-secrets.json")
	for path, body := range map[string]string{tokenFile: dispatch.Token + "\n", fakeSecretsFile: `{"secrets": []}`} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	env := []string{
		"BROKER_DATABASE_URL=" + databaseURL,
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_FAKE_SECRETS_FILE=" + fakeSecretsFile,
		"BROKER_K8S_OIDC_ISSUER=" + cluster.URL(),
		"BROKER_K8S_OIDC_AUDIENCE=" + enrolmentAudience,
		"BROKER_API_ACCOUNTS=listener=" + listenerAccount,
	}
	env = append(env, natsCalloutEnv(t, "edge-listener/edge-a/PRIV="+edgeListenerAccount+" listener/host-a/*="+listenerAccount)...)
	withDispatch := append(append([]string{}, env...),
		"BROKER_PROJECT_ACCESS_URL="+dispatch.URL, "BROKER_PROJECT_ACCESS_TOKEN_FILE="+tokenFile)
	live := newPlainBroker(startBroker(t, brokerOptions{Env: withDispatch}).Addr)
	edge := accountToken(t, cluster, clusterKey, edgeListenerAccount)
	listener := accountToken(t, cluster, clusterKey, listenerAccount)

	status, answer := live.callAs(t, http.MethodGet, "/v1/project-repositories?project=PRIV", edge, "", "")
	repositories, _ := answer["repositories"].([]any)
	if status != http.StatusOK || answer["project"] != "PRIV" || len(repositories) != 2 || repositories[0] != "acme/pub" || repositories[1] != "acme/two" {
		t.Fatalf("the edge listener of PRIV was answered %d %v, want 200 with acme/pub and acme/two", status, answer)
	}
	status, answer = live.callAs(t, http.MethodGet, "/v1/project-repositories?project=PRIV", listener, "", "")
	if status != http.StatusForbidden || answer["code"] != "NOT_AN_EDGE_LISTENER" {
		t.Fatalf("the Envoy listener was answered %d %v, want 403 NOT_AN_EDGE_LISTENER", status, answer)
	}
	dispatch.SetDown(true)
	status, answer = live.callAs(t, http.MethodGet, "/v1/project-repositories?project=PRIV", edge, "", "")
	if status != http.StatusServiceUnavailable || answer["code"] != "REPOSITORIES_UNKNOWN" {
		t.Fatalf("with Dispatch down the edge listener was answered %d %v, want 503 REPOSITORIES_UNKNOWN", status, answer)
	}

	unwired := newPlainBroker(startBroker(t, brokerOptions{Env: env}).Addr)
	status, answer = unwired.callAs(t, http.MethodGet, "/v1/project-repositories?project=PRIV", edge, "", "")
	if status != http.StatusServiceUnavailable || answer["code"] != "REPOSITORIES_UNKNOWN" {
		t.Fatalf("a broker with no BROKER_PROJECT_ACCESS_URL answered the edge listener %d %v, want 503 REPOSITORIES_UNKNOWN", status, answer)
	}
}

// natsCalloutEnv is the NATS auth service's settings for a broker whose hub is not up: a URL
// nothing listens on (the service connects in the background and retries), the three seeds, and
// services as BROKER_NATS_SERVICES, for the cluster audience a service's NATS token carries.
func natsCalloutEnv(t *testing.T, services string) []string {
	t.Helper()
	dir := t.TempDir()
	write := func(name string, create func() (nkeys.KeyPair, error)) string {
		pair, err := create()
		if err != nil {
			t.Fatal(err)
		}
		seed, err := pair.Seed()
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, append(seed, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	// A port this test held and let go, so nothing answers there.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hub := "nats://" + listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return []string{
		"BROKER_NATS_URLS=" + hub,
		"BROKER_NATS_SEED_FILE=" + write("user.seed", nkeys.CreateUser),
		"BROKER_NATS_ISSUER_SEED_FILE=" + write("issuer.seed", nkeys.CreateAccount),
		"BROKER_NATS_XKEY_SEED_FILE=" + write("xkey.seed", nkeys.CreateCurveKeys),
		"BROKER_NATS_SERVICES=" + services,
		"BROKER_NATS_K8S_AUDIENCE=example-envoy-nats",
	}
}
