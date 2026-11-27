package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// TestTheBootLinesNameNoCredentialTheirURLSettingsCarry drives the REAL compiled broker with each
// URL setting a boot line names holding the probe's credentials. BROKER_EDGE_MACHINE_LOGIN_URL,
// BROKER_MACHINE_LOGIN_URL and BROKER_JUDGE_DISPATCH_URL are held to a scheme and a host alone at
// load, so each carries the probe's user and password in its user information and its token in
// its query, and the two machine-login pages the token in their path too; BROKER_NATS_URLS is
// refused at load with user information, a query or a fragment (hubURLRefusal), so its entry
// carries the token in its path alone, which nats.go does not send to a nats:// server. A
// second broker's two machine-login pages carry the token in their path alone, which the URL rule
// prints as written (the twin shows it would): a page's path is where a credential would be, so a
// line names the page by its setting. Each line still says what it says - the edge's own URL and
// the setting that names its page, the device grant on and the setting that names its page, the
// judge on with its model, the hub's server by its host - and none prints a run of the user, the
// password or the token. The twin shows each setting as written holds its markers. Captures
// cipherd.boot-url/<case>.
func TestTheBootLinesNameNoCredentialTheirURLSettingsCarry(t *testing.T) {
	const query = "?access" + "_token=" + redacttest.Token
	userinfo := redacttest.User + ":" + redacttest.Password + "@"
	model := newCountingServer(t, `{}`)
	dispatch := newCountingServer(t, `{}`)
	keys := testnats.NewCalloutKeys(t)
	hub := testnats.StartWithCallout(t, keys)
	edgeMachineLogin := "https://" + userinfo + "dispatch.edge.example/credentials/machine/" + redacttest.Token + query
	machineLogin := "https://" + userinfo + "dispatch.example/credentials/machine/" + redacttest.Token + query
	judgeDispatch := "http://" + userinfo + strings.TrimPrefix(dispatch.URL, "http://") + "/" + query
	hubEntry := hub + "/" + redacttest.Token
	edgePathOnly := "https://dispatch.edge.example/credentials/machine/" + redacttest.Token
	machinePathOnly := "https://dispatch.example/credentials/machine/" + redacttest.Token
	// broker is which of the two brokers logs the case's line; shows is what the line's field holds
	// at least: the setting that names the page, the hub's server by its host, or for the judge any
	// text at all, since the rule withholds the whole URL; ruled says the URL rule would print the
	// setting whole, so its path's token is the row's to see.
	cases := []struct {
		name, message, field, configured, shows string
		broker                                  int
		ruled                                   bool
		markers                                 []string
		names                                   map[string]any
	}{
		{"edge", "broker edge listening", "verification_uri", edgeMachineLogin, "BROKER_EDGE_MACHINE_LOGIN_URL", 0, false, redacttest.Secrets, map[string]any{"url": testEdgeURL}},
		{"machine-login", "machine login device grant", "verification_uri", machineLogin, "BROKER_MACHINE_LOGIN_URL", 0, false, redacttest.Secrets, map[string]any{"state": "on"}},
		{"judge", "model judge", "dispatch", judgeDispatch, "", 0, false, redacttest.Secrets, map[string]any{"state": "on", "model": testJudgeModel}},
		{"nats", "broker: the NATS auth service is answering", "servers", hubEntry, strings.TrimPrefix(hub, "nats://"), 0, false, []string{redacttest.Token}, nil},
		{"edge-path", "broker edge listening", "verification_uri", edgePathOnly, "BROKER_EDGE_MACHINE_LOGIN_URL", 1, true, []string{redacttest.Token}, map[string]any{"url": testEdgeURL}},
		{"machine-login-path", "machine login device grant", "verification_uri", machinePathOnly, "BROKER_MACHINE_LOGIN_URL", 1, true, []string{redacttest.Token}, map[string]any{"state": "on"}},
	}
	t.Run("twin", func(t *testing.T) {
		for _, c := range cases {
			if held := redacttest.Held(c.configured); !slices.Equal(held, c.markers) {
				t.Errorf("%s: the setting as written, %q, holds %q, want %q, so the row can see each printed", c.name, c.configured, held, c.markers)
			}
			if shown := redact.URL(c.configured); c.ruled && !strings.Contains(shown, redacttest.Token) {
				t.Errorf("%s: redact.URL(%q) = %q; want the token in it, so the row can see the rule print it", c.name, c.configured, shown)
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
	edgeEnv := func(machineLogin, edgeMachineLogin string) []string {
		return []string{
			"BROKER_DATABASE_URL=" + storetest.URL(t),
			"BROKER_LISTEN_ADDR=127.0.0.1:0",
			"BROKER_PUBLIC_URL=http://127.0.0.1:0",
			"BROKER_FAKE_SECRETS_FILE=" + emptySecretsFile(t),
			"BROKER_MACHINE_LOGIN_URL=" + machineLogin,
			"BROKER_EDGE_LISTEN_ADDR=127.0.0.1:0",
			"BROKER_EDGE_URL=" + testEdgeURL,
			"BROKER_EDGE_MACHINE_LOGIN_URL=" + edgeMachineLogin,
			"BROKER_SIGNIN_WORKSPACE_DOMAINS=" + ciphertest.EdgeWorkspaceDomain,
		}
	}
	brokers := []*brokerProcess{
		startBroker(t, brokerOptions{Env: append(edgeEnv(machineLogin, edgeMachineLogin),
			"BROKER_JUDGE_MODEL_URL="+model.URL,
			"BROKER_JUDGE_MODEL="+testJudgeModel,
			"BROKER_JUDGE_MODEL_AUDIENCE="+testJudgeAudience,
			"BROKER_JUDGE_DISPATCH_URL="+judgeDispatch,
			"BROKER_JUDGE_DISPATCH_TOKEN_FILE="+judgeTokenFile(t),
			"BROKER_NATS_URLS="+hubEntry,
			"BROKER_NATS_SEED_FILE="+seedFile("service.seed", keys.ServiceSeed),
			"BROKER_NATS_ISSUER_SEED_FILE="+seedFile("issuer.seed", keys.IssuerSeed),
			"BROKER_NATS_XKEY_SEED_FILE="+seedFile("xkey.seed", keys.XKeySeed),
		)}),
		startBroker(t, brokerOptions{Env: edgeEnv(machinePathOnly, edgePathOnly)}),
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			line := bootLine(t, brokers[c.broker], c.message)
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
