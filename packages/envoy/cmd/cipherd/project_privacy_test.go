// project_privacy_test.go drives GET /v1/project-privacy through the REAL compiled broker: the
// accounts it answers are the ones BROKER_NATS_SERVICES binds to a listener, and its read of
// Dispatch is the client BROKER_PROJECT_ACCESS_URL configures, so this is how cmd/cipherd wires
// both into the route, with every request crossing a socket.
package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/nats-io/nkeys"

	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// privacyMeasureAccount is the account plan C's privacy measurement Job runs as.
const privacyMeasureAccount = "system:serviceaccount:example-ns:example-privacy-measure"

// TestTheBrokerAnswersItsListenersProjectPrivacy: a broker running the NATS auth service, whose
// BROKER_NATS_SERVICES binds listenerAccount to a listener, apiAccount to Dispatch's app and
// privacyMeasureAccount to the privacy measurement, answers the listener's and the measurement's
// GET /v1/project-privacy with the private rows of the Dispatch its BROKER_PROJECT_ACCESS_URL
// names, and refuses Dispatch's app account 403 not_listener. The same broker without
// BROKER_PROJECT_ACCESS_URL answers the listener 502 dispatch_unavailable. The hub the auth service
// dials is down throughout: it connects in the background, and the route reads the setting, not
// the hub.
func TestTheBrokerAnswersItsListenersProjectPrivacy(t *testing.T) {
	databaseURL := storetest.URL(t)
	cluster := oidctest.New(t)
	clusterKey := cluster.PublishKey(t, "signing-key")
	dir := t.TempDir()
	fakeSecretsFile := filepath.Join(dir, "fake-secrets.json")
	if err := os.WriteFile(fakeSecretsFile, []byte(`{"secrets": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	seed := func(name string, create func() (nkeys.KeyPair, error)) string {
		t.Helper()
		pair, err := create()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := pair.Seed()
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	// A port nothing listens on, so the auth service's hub is down.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hub := "nats://" + listener.Addr().String()
	listener.Close()
	env := []string{
		"BROKER_DATABASE_URL=" + databaseURL,
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_FAKE_SECRETS_FILE=" + fakeSecretsFile,
		"BROKER_K8S_OIDC_ISSUER=" + cluster.URL(),
		"BROKER_K8S_OIDC_AUDIENCE=" + enrolmentAudience,
		"BROKER_NATS_URLS=" + hub,
		"BROKER_NATS_SEED_FILE=" + seed("user.seed", nkeys.CreateUser),
		"BROKER_NATS_ISSUER_SEED_FILE=" + seed("issuer.seed", nkeys.CreateAccount),
		"BROKER_NATS_XKEY_SEED_FILE=" + seed("xkey.seed", nkeys.CreateCurveKeys),
		"BROKER_NATS_SERVICES=listener/example-hub/*=" + listenerAccount + " dispatch/example-dispatch/*=" + apiAccount +
			" privacy-measure/project-privacy-measure/*=" + privacyMeasureAccount,
		"BROKER_NATS_K8S_AUDIENCE=envoy-nats",
		"BROKER_DRAIN_SECONDS=0",
		// Unset here whatever the test process holds; the first subtest sets both after these.
		"BROKER_PROJECT_ACCESS_URL=",
		"BROKER_PROJECT_ACCESS_TOKEN_FILE=",
	}
	listenerToken := accountToken(t, cluster, clusterKey, listenerAccount)
	privacy := func(t *testing.T, addr, token string) (int, []byte) {
		t.Helper()
		request, err := http.NewRequest(http.MethodGet, "http://"+addr+"/v1/project-privacy", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("GET /v1/project-privacy: %v", err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, body
	}
	code := func(body []byte) string {
		var refusal struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(body, &refusal)
		return refusal.Code
	}

	t.Run("with Dispatch's project access", func(t *testing.T) {
		changed := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
		dispatch := projectaccesstest.NewServer(t, map[string]projectaccesstest.Row{
			"PRIV": {Visibility: "private", Owner: "ada", SlackChannels: []string{"C0PRIV"}, SlackChannelsChangedAt: changed},
			"PUB":  {Visibility: "public", Owner: "ada"},
		}, nil)
		tokenFile := filepath.Join(t.TempDir(), "dispatch-access-token")
		if err := os.WriteFile(tokenFile, []byte(dispatch.Token+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		broker := startBroker(t, brokerOptions{Env: append(env,
			"BROKER_PROJECT_ACCESS_URL="+dispatch.URL, "BROKER_PROJECT_ACCESS_TOKEN_FILE="+tokenFile)})
		requireHealthy(t, broker.Addr)

		status, body := privacy(t, broker.Addr, listenerToken)
		var got, want any
		if err := json.Unmarshal(body, &got); err != nil || status != http.StatusOK {
			t.Fatalf("the listener's GET /v1/project-privacy = %d %s, want 200 and Dispatch's private rows", status, body)
		}
		if err := json.Unmarshal([]byte(`{"projects":[{"project":"PRIV","slack_channels":["C0PRIV"],"slack_channels_changed_at":"2026-11-19T10:00:00Z"}]}`), &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("the listener was answered %s, want PRIV alone, with its channels and when they changed", body)
		}
		// The privacy measurement's entry (kind privacy-measure) parsed at boot, and its account
		// reads the route as a listener's does.
		status, body = privacy(t, broker.Addr, accountToken(t, cluster, clusterKey, privacyMeasureAccount))
		var measured any
		if err := json.Unmarshal(body, &measured); err != nil || status != http.StatusOK || !reflect.DeepEqual(measured, want) {
			t.Fatalf("the privacy measurement's GET /v1/project-privacy = %d %s, want 200 and the same rows", status, body)
		}
		asked := dispatch.Calls()
		if status, body := privacy(t, broker.Addr, accountToken(t, cluster, clusterKey, apiAccount)); status != http.StatusForbidden || code(body) != "not_listener" {
			t.Fatalf("Dispatch's app account's GET /v1/project-privacy = %d %s, want 403 not_listener", status, body)
		}
		if dispatch.Calls() != asked {
			t.Fatalf("the refused call asked Dispatch %d times, want none", dispatch.Calls()-asked)
		}
	})

	t.Run("without Dispatch's project access", func(t *testing.T) {
		broker := startBroker(t, brokerOptions{Env: env})
		requireHealthy(t, broker.Addr)
		if status, body := privacy(t, broker.Addr, listenerToken); status != http.StatusBadGateway || code(body) != "dispatch_unavailable" {
			t.Fatalf("with no BROKER_PROJECT_ACCESS_URL, the listener's GET /v1/project-privacy = %d %s, want 502 dispatch_unavailable", status, body)
		}
	})
}
