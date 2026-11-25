package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// TestTheBootLinesNameNoCredentialTheirURLSettingsCarry drives the REAL compiled broker with each
// URL setting a boot line names holding the probe's credentials. BROKER_EDGE_MACHINE_LOGIN_URL,
// BROKER_MACHINE_LOGIN_URL and BROKER_JUDGE_DISPATCH_URL are held to a scheme and a host alone at
// load, so each carries the probe's user and password in its user information and its token in
// its query; BROKER_NATS_URLS is refused at load with user information, so its entry carries the
// token in its query alone. Each line still says what it says - the edge's own URL, the device
// grant on, the judge on with its model, the hub's server by its host - and none prints a run of
// the user, the password or the token. The twin shows each setting as written holds its markers.
// Captures cipherd.boot-url/<case>.
func TestTheBootLinesNameNoCredentialTheirURLSettingsCarry(t *testing.T) {
	const query = "?access" + "_token=" + redacttest.Token
	userinfo := redacttest.User + ":" + redacttest.Password + "@"
	model := newCountingServer(t, `{}`)
	dispatch := newCountingServer(t, `{}`)
	keys := testnats.NewCalloutKeys(t)
	hub := testnats.StartWithCallout(t, keys)
	edgeMachineLogin := "https://" + userinfo + "dispatch.edge.example/credentials/machine" + query
	machineLogin := "https://" + userinfo + "dispatch.example/credentials/machine" + query
	judgeDispatch := "http://" + userinfo + strings.TrimPrefix(dispatch.URL, "http://") + "/" + query
	hubEntry := hub + query
	// shows is what the line's field holds at least: the hub's server by its host for the NATS
	// line, and for each other line any text at all, since the rule withholds the whole URL.
	cases := []struct {
		name, message, field, configured, shows string
		markers                                 []string
		names                                   map[string]any
	}{
		{"edge", "broker edge listening", "verification_uri", edgeMachineLogin, "", redacttest.Secrets, map[string]any{"url": testEdgeURL}},
		{"machine-login", "machine login device grant", "verification_uri", machineLogin, "", redacttest.Secrets, map[string]any{"state": "on"}},
		{"judge", "model judge", "dispatch", judgeDispatch, "", redacttest.Secrets, map[string]any{"state": "on", "model": testJudgeModel}},
		{"nats", "broker: the NATS auth service is answering", "servers", hubEntry, strings.TrimPrefix(hub, "nats://"), []string{redacttest.Token}, nil},
	}
	t.Run("twin", func(t *testing.T) {
		for _, c := range cases {
			for _, marker := range c.markers {
				if !strings.Contains(c.configured, marker) {
					t.Errorf("%s: the setting as written, %q, holds no %q, so the row cannot see it printed", c.name, c.configured, marker)
				}
			}
		}
	})
	seeds := t.TempDir()
	seedFile := func(name, seed string) string {
		path := filepath.Join(seeds, name)
		if err := os.WriteFile(path, []byte(seed+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	broker := startBroker(t, brokerOptions{Env: []string{
		"BROKER_DATABASE_URL=" + storetest.URL(t),
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_FAKE_SECRETS_FILE=" + emptySecretsFile(t),
		"BROKER_MACHINE_LOGIN_URL=" + machineLogin,
		"BROKER_EDGE_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_EDGE_URL=" + testEdgeURL,
		"BROKER_EDGE_MACHINE_LOGIN_URL=" + edgeMachineLogin,
		"BROKER_SIGNIN_WORKSPACE_DOMAINS=" + ciphertest.EdgeWorkspaceDomain,
		"BROKER_JUDGE_MODEL_URL=" + model.URL,
		"BROKER_JUDGE_MODEL=" + testJudgeModel,
		"BROKER_JUDGE_MODEL_AUDIENCE=" + testJudgeAudience,
		"BROKER_JUDGE_DISPATCH_URL=" + judgeDispatch,
		"BROKER_JUDGE_DISPATCH_TOKEN_FILE=" + judgeTokenFile(t),
		"BROKER_NATS_URLS=" + hubEntry,
		"BROKER_NATS_SEED_FILE=" + seedFile("service.seed", keys.ServiceSeed),
		"BROKER_NATS_ISSUER_SEED_FILE=" + seedFile("issuer.seed", keys.IssuerSeed),
		"BROKER_NATS_XKEY_SEED_FILE=" + seedFile("xkey.seed", keys.XKeySeed),
	}})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			line := bootLine(t, broker, c.message)
			redacttest.RequireNoPiece(t, "cipherd.boot-url/"+c.name, line, redacttest.User, redacttest.Password, redacttest.Token)
			record, _ := logRecord(line)
			if !carries(record, c.names) {
				t.Errorf("the %q line %s does not carry %v", c.message, line, c.names)
			}
			if shown, _ := record[c.field].(string); shown == "" || !strings.Contains(shown, c.shows) {
				t.Errorf("the %q line %s names no %s holding %q", c.message, line, c.field, c.shows)
			}
		})
	}
}

// bootLine waits for the broker's line whose message is message and answers it as the broker
// wrote it, the text a reader of its log reads.
func bootLine(t *testing.T, broker *brokerProcess, message string) string {
	t.Helper()
	broker.AwaitRecord(t, message, nil)
	for line := range strings.Lines(broker.Log()) {
		if record, ok := logRecord(line); ok && record["message"] == message {
			return line
		}
	}
	t.Fatalf("the broker logged a %q line and no line of its log reads as one", message)
	return ""
}
