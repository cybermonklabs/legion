// project_repositories_test.go drives GET /v1/project-repositories on the REAL compiled broker,
// over HTTP, on a real Postgres, with the edge listener's entry read from BROKER_NATS_SERVICES and
// Dispatch's answer read through BROKER_PROJECT_ACCESS_URL and its token file, as a deployment
// sets them: what cmd/cipherd wires, and not only what the API package does with what it is given.
package main

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nats-io/nkeys"

	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
	"github.com/sjawhar/envoy/internal/testport"
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
	rig := newEdgeRig(t)
	nats := natsCalloutEnv(t, "edge-listener/edge-a/PRIV="+edgeListenerAccount+" listener/host-a/*="+listenerAccount)
	apiAccounts := []string{"BROKER_API_ACCOUNTS=listener=" + listenerAccount}
	live := newPlainBroker(startBroker(t, brokerOptions{Env: rig.env(nats, apiAccounts, rig.access)}).Addr)
	edge := accountToken(t, rig.cluster, rig.key, edgeListenerAccount)
	listener := accountToken(t, rig.cluster, rig.key, listenerAccount)

	status, answer := live.callAs(t, http.MethodGet, "/v1/project-repositories?project=PRIV", edge, "", "")
	requirePRIVsRepositories(t, "the edge listener of PRIV", status, answer)
	status, answer = live.callAs(t, http.MethodGet, "/v1/project-repositories?project=PRIV", listener, "", "")
	if status != http.StatusForbidden || answer["code"] != "NOT_AN_EDGE_LISTENER" {
		t.Fatalf("the Envoy listener was answered %d %v, want 403 NOT_AN_EDGE_LISTENER", status, answer)
	}
	rig.dispatch.SetDown(true)
	status, answer = live.callAs(t, http.MethodGet, "/v1/project-repositories?project=PRIV", edge, "", "")
	if status != http.StatusServiceUnavailable || answer["code"] != "REPOSITORIES_UNKNOWN" {
		t.Fatalf("with Dispatch down the edge listener was answered %d %v, want 503 REPOSITORIES_UNKNOWN", status, answer)
	}

	unwired := newPlainBroker(startBroker(t, brokerOptions{Env: rig.env(nats, apiAccounts)}).Addr)
	status, answer = unwired.callAs(t, http.MethodGet, "/v1/project-repositories?project=PRIV", edge, "", "")
	if status != http.StatusServiceUnavailable || answer["code"] != "REPOSITORIES_UNKNOWN" {
		t.Fatalf("a broker with no BROKER_PROJECT_ACCESS_URL answered the edge listener %d %v, want 503 REPOSITORIES_UNKNOWN", status, answer)
	}
}

// TestTheBrokerBinaryAnswersAnEdgeListenerWithNoAuthService: a broker that runs no NATS auth
// service (no BROKER_NATS_URLS, no seeds), as the first deployment's does, records the service
// off with the one edge listener it read from BROKER_NATS_SERVICES, and answers that edge
// listener of PRIV its list, asking Dispatch once.
func TestTheBrokerBinaryAnswersAnEdgeListenerWithNoAuthService(t *testing.T) {
	rig := newEdgeRig(t)
	broker := startBroker(t, brokerOptions{Env: rig.env(rig.access, []string{"BROKER_NATS_SERVICES=edge-listener/edge-a/PRIV=" + edgeListenerAccount})})
	record := broker.AwaitRecord(t, "NATS auth service", nil)
	if record["state"] != "off" || record["edge_listeners"] != float64(1) {
		t.Fatalf("the boot recorded the NATS auth service as %v, want state off with 1 edge listener", record)
	}
	live := newPlainBroker(broker.Addr)
	status, answer := live.callAs(t, http.MethodGet, "/v1/project-repositories?project=PRIV", accountToken(t, rig.cluster, rig.key, edgeListenerAccount), "", "")
	requirePRIVsRepositories(t, "a broker with no NATS auth service answered the edge listener of PRIV", status, answer)
	if calls := rig.dispatch.Calls(); calls != 1 {
		t.Fatalf("Dispatch was asked %d times, want 1", calls)
	}
}

// TestTheBrokerBinaryAnswersAGoogleEdgeListener: an edge listener on a VM, whose entry names a
// Google account, is the broker's only Google caller: no BROKER_API_ACCOUNTS entry and no VM
// service name one. The broker still starts with BROKER_GOOGLE_OIDC_AUDIENCE, builds the Google
// verifier through its own boot, and answers that edge listener's identity token PRIV's list; a
// Google account no entry names is 403 NOT_AN_EDGE_LISTENER.
func TestTheBrokerBinaryAnswersAGoogleEdgeListener(t *testing.T) {
	const edgeGoogleAccount = "example-edge@example-project.iam.gserviceaccount.com"
	google := oidctest.New(t)
	googleKey := google.PublishKey(t, "google-key")
	rig := newEdgeRig(t)
	live := newPlainBroker(startBroker(t, brokerOptions{Env: rig.env(rig.access, []string{
		"BROKER_NATS_SERVICES=edge-listener/edge-g/PRIV=google:" + edgeGoogleAccount,
		"BROKER_GOOGLE_OIDC_ISSUER=" + google.URL(), "BROKER_GOOGLE_OIDC_AUDIENCE=" + machineAudience,
	})}).Addr)
	identity := func(account string) string {
		t.Helper()
		claims := google.Claims(account, machineAudience)
		claims["email"] = account
		claims["email_verified"] = true
		claims["iat"] = time.Now().Add(-5 * time.Second).Unix()
		return google.Mint(t, googleKey, claims)
	}
	status, answer := live.callAs(t, http.MethodGet, "/v1/project-repositories?project=PRIV", identity(edgeGoogleAccount), "", "")
	requirePRIVsRepositories(t, "the Google edge listener of PRIV", status, answer)
	status, answer = live.callAs(t, http.MethodGet, "/v1/project-repositories?project=PRIV", identity("example-stranger@example-project.iam.gserviceaccount.com"), "", "")
	if status != http.StatusForbidden || answer["code"] != "NOT_AN_EDGE_LISTENER" {
		t.Fatalf("a Google account no entry names was answered %d %v, want 403 NOT_AN_EDGE_LISTENER", status, answer)
	}
	if calls := rig.dispatch.Calls(); calls != 1 {
		t.Fatalf("Dispatch was asked %d times, want 1", calls)
	}
}

// requirePRIVsRepositories fails unless the answer is 200 about PRIV with its list, acme/pub and
// acme/two in that order; who says whose call it answered.
func requirePRIVsRepositories(t *testing.T, who string, status int, answer map[string]any) {
	t.Helper()
	repositories, _ := answer["repositories"].([]any)
	if status != http.StatusOK || answer["project"] != "PRIV" || len(repositories) != 2 || repositories[0] != "acme/pub" || repositories[1] != "acme/two" {
		t.Fatalf("%s: answered %d %v, want 200 about PRIV with acme/pub and acme/two", who, status, answer)
	}
}

// edgeRig is what the brokers of these tests read: a stand-in of Dispatch's access routes where
// PRIV's issues resolve to acme/pub and acme/two, and the cluster issuer whose tokens they take,
// with its signing key.
type edgeRig struct {
	dispatch *projectaccesstest.Server
	cluster  *oidctest.Issuer
	key      *oidctest.Key
	// base is a broker's settings but its BROKER_NATS_* and BROKER_PROJECT_ACCESS_* ones, and
	// access is BROKER_PROJECT_ACCESS_URL and BROKER_PROJECT_ACCESS_TOKEN_FILE, naming dispatch and
	// a file holding the one bearer it takes.
	base, access []string
}

func newEdgeRig(t *testing.T) edgeRig {
	t.Helper()
	cluster := oidctest.New(t)
	rig := edgeRig{
		dispatch: projectaccesstest.NewServer(t, map[string]projectaccesstest.Row{
			// The membership answer's own list is not part of the repository answer: a broker
			// that relayed it would answer acme/member.
			"PRIV": {Visibility: "private", Repositories: []string{"acme/member"}, Resolved: []string{"acme/pub", "acme/two"}},
		}, nil),
		cluster: cluster,
		key:     cluster.PublishKey(t, "signing-key"),
	}
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "dispatch-access-token")
	memorySecretsFile := filepath.Join(dir, "memory-secrets.json")
	for path, body := range map[string]string{tokenFile: rig.dispatch.Token + "\n", memorySecretsFile: `{"secrets": []}`} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rig.base = []string{
		"BROKER_DATABASE_URL=" + storetest.URL(t),
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_SECRETS_STORE=memory",
		"BROKER_MEMORY_SECRETS_FILE=" + memorySecretsFile,
		"BROKER_K8S_OIDC_ISSUER=" + cluster.URL(),
		"BROKER_K8S_OIDC_AUDIENCE=" + enrolmentAudience,
	}
	rig.access = []string{"BROKER_PROJECT_ACCESS_URL=" + rig.dispatch.URL, "BROKER_PROJECT_ACCESS_TOKEN_FILE=" + tokenFile}
	return rig
}

// env is a broker's environment: the rig's base settings, then each of more in turn.
func (r edgeRig) env(more ...[]string) []string {
	env := append([]string{}, r.base...)
	for _, settings := range more {
		env = append(env, settings...)
	}
	return env
}

// natsCalloutEnv is the NATS auth service's settings for a broker whose hub is not up: an address
// that refuses every dial (the service connects in the background and retries), the three seeds,
// and services as BROKER_NATS_SERVICES, for the cluster audience a service's NATS token carries.
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
	// A port the test holds and nothing listens on, so nothing answers there: a closed listener's
	// port is any process's to take.
	hub := "nats://" + testport.Refused(t)
	return []string{
		"BROKER_NATS_URLS=" + hub,
		"BROKER_NATS_SEED_FILE=" + write("user.seed", nkeys.CreateUser),
		"BROKER_NATS_ISSUER_SEED_FILE=" + write("issuer.seed", nkeys.CreateAccount),
		"BROKER_NATS_XKEY_SEED_FILE=" + write("xkey.seed", nkeys.CreateCurveKeys),
		"BROKER_NATS_SERVICES=" + services,
		"BROKER_NATS_K8S_AUDIENCE=example-envoy-nats",
	}
}
