package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/logging"
)

// renderedListenerEnv is the environment of the container named listener in the pod spec golden
// name, which envoy-infra renders under packages/envoy/infra/gcp/testdata: each variable with a
// plain value, as the container receives it. A variable drawn from a Secret or a field has no
// value there and is left out; a golden holding no such container fails the test.
func renderedListenerEnv(t *testing.T, name string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "infra", "gcp", "testdata", name))
	if err != nil {
		t.Fatalf("read the rendered pod %s: %v", name, err)
	}
	var pod struct {
		Spec struct {
			Containers []struct {
				Name string `json:"name"`
				Env  []struct {
					Name  string  `json:"name"`
					Value *string `json:"value"`
				} `json:"env"`
			} `json:"containers"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(raw, &pod); err != nil {
		t.Fatalf("decode the rendered pod %s: %v", name, err)
	}
	for _, container := range pod.Spec.Containers {
		if container.Name != "listener" {
			continue
		}
		env := make(map[string]string, len(container.Env))
		for _, variable := range container.Env {
			if variable.Value != nil {
				env[variable.Name] = *variable.Value
			}
		}
		if len(env) == 0 {
			t.Fatalf("the listener container of %s renders no environment", name)
		}
		return env
	}
	t.Fatalf("%s holds no container named listener", name)
	return nil
}

// TestTheRenderedListenerEnvResolves holds the listener envoy-infra renders to the listener's own
// rules: the environment of each golden a listener of a hub that may make a project private is
// rendered as is fed to resolveListenerBrokerConfig, which accepts it, in the token-file form,
// with the origin it renders, and to resolveListenerV1OpenWithTokenChecks, which keeps /v1 open on
// the tailnet-only listener and closed on the hub's own, which requires a token of every caller;
// the open one renders the token check (ENVOY_OIDC_*) that validateListenerAPIAuth needs beside
// the setting. A render the listener refused would be a listener that does not start (main makes
// the refusal fatal), so this is the start's own check of what the component renders.
//
// Control: the private environment without ENVOY_PUBLIC_URL; the resolver refuses it, and the
// test fails naming the variable.
func TestTheRenderedListenerEnvResolves(t *testing.T) {
	for golden, open := range map[string]bool{
		"clustered-hub-listener-pod-private.golden":         false,
		"clustered-hub-tailnet-listener-pod-private.golden": true,
	} {
		t.Run(golden, func(t *testing.T) {
			env := renderedListenerEnv(t, golden)
			getenv := func(name string) string { return env[name] }
			config, err := resolveListenerBrokerConfig(getenv)
			if err != nil {
				t.Fatalf("the listener refuses the environment %s renders: %v", golden, err)
			}
			if !config.configured() || config.AccountTokenFile == "" || config.GoogleAudience != "" || config.Origin != env[EnvPublicURL] {
				t.Fatalf("%s resolves to %+v, want the broker check in its token-file form with the origin it renders", golden, config)
			}
			got, err := resolveListenerV1OpenWithTokenChecks(getenv)
			if err != nil || got != open {
				t.Fatalf("%s: %s resolves to %v, %v; want %v", golden, EnvV1OpenWithTokenChecks, got, err, open)
			}
			if issuer, audience, err := resolveListenerOIDCConfig(getenv); err != nil || issuer == "" || audience == "" {
				t.Fatalf("%s: the token check resolves to %q, %q, %v; want the pair a private listener renders", golden, issuer, audience, err)
			}

			delete(env, EnvPublicURL)
			if _, err := resolveListenerBrokerConfig(getenv); err == nil || !strings.Contains(err.Error(), EnvPublicURL) {
				t.Fatalf("%s without %s resolves (err = %v), want a refusal naming %s", golden, EnvPublicURL, err, EnvPublicURL)
			}
		})
	}
}

// tailnetSessionOrigin is the origin a tailnet session reaches the clustered hub's session listener
// at in the goldens envoy-infra renders: the Tailscale Service's name and the listener's port.
const tailnetSessionOrigin = "http://envoy-hub.example-tailnet.ts.net:9020"

// TestTheRenderedPrivateListenerTakesTheTailnetsProofs: the listener checks every DPoP proof
// against one origin, ENVOY_PUBLIC_URL, and the callers that present a broker token to a private
// hub's session listener are the tailnet's sessions, which sign their proofs for the Tailscale
// Service's name. So a listener built from the private golden's environment admits a session's
// POST /v1/messages/publish signed for that name.
//
// Control: the same environment with ENVOY_PUBLIC_URL set to the listener's cluster Service, the
// origin this branch rendered before; the tailnet session's proof is refused 401.
func TestTheRenderedPrivateListenerTakesTheTailnetsProofs(t *testing.T) {
	const sessionToken = "access-token-of-a-tailnet-session"
	env := renderedListenerEnv(t, "clustered-hub-listener-pod-private.golden")
	if env[EnvPublicURL] != tailnetSessionOrigin {
		t.Fatalf("the private golden renders %s=%q, want the tailnet's origin %q", EnvPublicURL, env[EnvPublicURL], tailnetSessionOrigin)
	}
	broker := ciphertokentest.NewStubBroker(t)
	session := ciphertokentest.NewCaller(t)
	broker.Says(sessionToken, sessionAnswer("ses_tailnet", "ada@example.com", session.JKT()))
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) })
	publish := func(origin string) int {
		t.Helper()
		tokens := newBrokerTokens(listenerBrokerConfig{BrokerURL: broker.Server.URL, AccountTokenFile: env[EnvBrokerAccountTokenFile], Origin: origin})
		verifier, err := ciphertoken.New(ciphertoken.Options{
			BrokerURL: broker.Server.URL, Account: listenerBrokerAccount("listener-account-token"), Origin: origin,
			Replay: &spentProofsInMemory{}, Address: listenerSourceAddress,
		})
		if err != nil {
			t.Fatalf("build the broker-token verifier: %v", err)
		}
		tokens.verifier.Store(verifier)
		signed := session.Signed(t, http.MethodPost, tailnetSessionOrigin+"/v1/messages/publish", sessionToken)
		request := httptest.NewRequest(http.MethodPost, "/v1/messages/publish", strings.NewReader(`{}`))
		request.Host = "envoy-hub.example-tailnet.ts.net:9020"
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", signed.Header.Get("Authorization"))
		request.Header.Set("DPoP", signed.Header.Get("DPoP"))
		recorder := httptest.NewRecorder()
		apiAuth(listenerAPIAuth{token: "a-bearer-so-the-listener-is-closed", brokers: tokens, logger: logging.NewWithWriter("test-machine", io.Discard)}, next).ServeHTTP(recorder, request)
		return recorder.Code
	}
	if status := publish(env[EnvPublicURL]); status != http.StatusAccepted {
		t.Fatalf("a tailnet session's proof at the golden's origin %s: status = %d, want 202", env[EnvPublicURL], status)
	}
	if status := publish("http://envoy-nats-listener.envoy.svc.cluster.local:9020"); status != http.StatusUnauthorized {
		t.Fatalf("the control, a tailnet session's proof at the cluster Service's origin: status = %d, want 401", status)
	}
}
