// packages/envoy/cmd/broker/judge_boot_test.go
// The model judge as the real binary boots it: which of the two brokers a deployment's settings
// make, read off the one line each logs, and the fact that neither the model nor Dispatch is
// called before a request is ever judged.
package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/config"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
)

// testJudgeModel is the model id every broker here is told the judge asks, as the endpoint at
// its model URL names it, and testJudgeAudience the audience of the Google identity token the
// judge would present to that endpoint. Nothing mints one here: a boot asks the model nothing.
const (
	testJudgeModel    = "claude-opus-4-6"
	testJudgeAudience = "https://model.example"
)

// countingServer is an httptest server that records how many requests reached it, so a test can
// say "and nothing called this" about a boot rather than only about a log line.
type countingServer struct {
	*httptest.Server
	calls atomic.Int64
}

func newCountingServer(t *testing.T, body string) *countingServer {
	t.Helper()
	s := &countingServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s
}

// judgeTokenFile writes the projected ServiceAccount token the judge would present to Dispatch,
// the file BROKER_JUDGE_DISPATCH_TOKEN_FILE names. It is read when the judge calls Dispatch and
// never at boot, so what is in it does not matter here; that it exists is what a deployment has
// to get right.
func judgeTokenFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dispatch-token")
	if err := os.WriteFile(path, []byte("dispatch-service-account-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestTheBinaryLogsWhichJudgeItBooted drives the real binary both ways round, off one build: with
// the five judge settings pointed at two stand-in servers, and with none of them. Each boot says
// in one line which of the two brokers it is, and the configured one asks neither stand-in
// anything while booting — the model endpoint is reached on a judged request and nowhere else.
func TestTheBinaryLogsWhichJudgeItBooted(t *testing.T) {
	binary := buildBroker(t)

	t.Run("configured", func(t *testing.T) {
		model := newCountingServer(t, `{}`)
		dispatch := newCountingServer(t, `{"key":"INFRA-1353","title":"","status":"doing"}`)
		broker := startBroker(t, brokerOptions{Binary: binary, Env: []string{
			"BROKER_DATABASE_URL=" + storetest.URL(t),
			"BROKER_LISTEN_ADDR=127.0.0.1:0",
			"BROKER_PUBLIC_URL=http://127.0.0.1:0",
			"BROKER_FAKE_SECRETS_FILE=" + emptySecretsFile(t),
			"BROKER_JUDGE_MODEL_URL=" + model.URL,
			"BROKER_JUDGE_MODEL=" + testJudgeModel,
			"BROKER_JUDGE_MODEL_AUDIENCE=" + testJudgeAudience,
			"BROKER_JUDGE_DISPATCH_URL=" + dispatch.URL,
			"BROKER_JUDGE_DISPATCH_TOKEN_FILE=" + judgeTokenFile(t),
		}})
		log := broker.Await(t, "model judge")
		for _, want := range []string{
			"model judge state=on",
			"model=" + testJudgeModel,
			"protocol=anthropic-messages",
			"dispatch=" + dispatch.URL,
		} {
			if !strings.Contains(log, want) {
				t.Fatalf("the boot line does not carry %q:\n%s", want, log)
			}
		}
		if calls := model.calls.Load() + dispatch.calls.Load(); calls != 0 {
			t.Fatalf("booting the judge made %d calls to the model and Dispatch; want none until a request is judged", calls)
		}
	})

	// The broker every deployment runs until one is configured. It names the settings that
	// would turn the judge on, so an operator reads which of the two this process is off the
	// boot rather than out of a request that waited for a person.
	t.Run("not configured", func(t *testing.T) {
		broker := startBroker(t, brokerOptions{Binary: binary, Env: []string{
			"BROKER_DATABASE_URL=" + storetest.URL(t),
			"BROKER_LISTEN_ADDR=127.0.0.1:0",
			"BROKER_PUBLIC_URL=http://127.0.0.1:0",
			"BROKER_FAKE_SECRETS_FILE=" + emptySecretsFile(t),
		}})
		log := broker.Await(t, "model judge")
		if !strings.Contains(log, "model judge state=off") {
			t.Fatalf("the boot log does not report the judge off:\n%s", log)
		}
		for _, setting := range config.JudgeSettings {
			if !strings.Contains(log, setting) {
				t.Fatalf("the off line does not name %s, one of the settings that turn the judge on:\n%s", setting, log)
			}
		}
	})
}
