// packages/envoy/cmd/cipherd/judge_boot_test.go
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

	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
)

// testJudgeModel is the model id every broker here is told the judge asks, as the endpoint at
// its model URL names it, testJudgeAudience the audience of the Google identity token the
// judge would present to that endpoint, and testJudgeAccount the service account a judge with
// BROKER_JUDGE_MODEL_ACCOUNT would mint that token as. Nothing mints one here: a boot asks the
// model nothing.
const (
	testJudgeModel    = "claude-opus-4-6"
	testJudgeAudience = "https://model.example"
	testJudgeAccount  = "judge-model@example-project.iam.gserviceaccount.com"
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

// judgeBootEnv is what a broker with a judge is started with here: a database and an address of
// its own, no secrets, and the five judge settings, its model endpoint and its Dispatch pointed
// at model and dispatch.
func judgeBootEnv(t *testing.T, model, dispatch *countingServer) []string {
	t.Helper()
	return []string{
		"BROKER_DATABASE_URL=" + storetest.URL(t),
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_SECRETS_STORE=memory",
		"BROKER_MEMORY_SECRETS_FILE=" + emptyMemoryFile(t),
		"BROKER_JUDGE_MODEL_URL=" + model.URL,
		"BROKER_JUDGE_MODEL=" + testJudgeModel,
		"BROKER_JUDGE_MODEL_AUDIENCE=" + testJudgeAudience,
		"BROKER_JUDGE_DISPATCH_URL=" + dispatch.URL,
		"BROKER_JUDGE_DISPATCH_TOKEN_FILE=" + judgeTokenFile(t),
	}
}

// TestTheBinaryLogsWhichJudgeItBooted drives the real binary three ways round, off one build:
// with the five judge settings pointed at two stand-in servers, with those five and an account
// whose identity token the judge mints, and with none of them. Each boot says in one line which
// of the two brokers it is, and a configured one says which identity its judge presents and asks
// no stand-in anything while booting — the model endpoint is reached on a judged request and
// nowhere else.
func TestTheBinaryLogsWhichJudgeItBooted(t *testing.T) {
	binary := buildBroker(t)

	t.Run("configured", func(t *testing.T) {
		model := newCountingServer(t, `{}`)
		dispatch := newCountingServer(t, `{"key":"INFRA-1353","title":"","status":"doing"}`)
		broker := startBroker(t, brokerOptions{Binary: binary, Env: judgeBootEnv(t, model, dispatch)})
		record := broker.AwaitRecord(t, "model judge", nil)
		if want := (map[string]any{"state": "on", "model": testJudgeModel, "protocol": "anthropic-messages", "dispatch": dispatch.URL, "identity": "metadata"}); !carries(record, want) {
			t.Fatalf("the boot line %v does not carry %v", record, want)
		}
		if calls := model.calls.Load() + dispatch.calls.Load(); calls != 0 {
			t.Fatalf("booting the judge made %d calls to the model and Dispatch; want none until a request is judged", calls)
		}
	})

	// The judge of a pod with no identity token of its own, which mints its account's at
	// generateIdToken. Its line names that account, and its boot reads no access token from
	// the metadata server: a mint, and the read that comes before it, wait for a judged
	// request, so a metadata server that saw nothing proves IAM saw nothing either.
	t.Run("configured with an account", func(t *testing.T) {
		model := newCountingServer(t, `{}`)
		dispatch := newCountingServer(t, `{"key":"INFRA-1353","title":"","status":"doing"}`)
		metadata := newCountingServer(t, `{}`)
		broker := startBroker(t, brokerOptions{Binary: binary, Env: append(judgeBootEnv(t, model, dispatch),
			"BROKER_JUDGE_MODEL_ACCOUNT="+testJudgeAccount,
			"GCE_METADATA_HOST="+strings.TrimPrefix(metadata.URL, "http://"),
		)})
		record := broker.AwaitRecord(t, "model judge", nil)
		if want := (map[string]any{"state": "on", "model": testJudgeModel, "dispatch": dispatch.URL, "identity": testJudgeAccount}); !carries(record, want) {
			t.Fatalf("the boot line %v does not carry %v", record, want)
		}
		if calls := model.calls.Load() + dispatch.calls.Load() + metadata.calls.Load(); calls != 0 {
			t.Fatalf("booting the judge made %d calls to the model, Dispatch and the metadata server; want none until a request is judged", calls)
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
			"BROKER_SECRETS_STORE=memory",
			"BROKER_MEMORY_SECRETS_FILE=" + emptyMemoryFile(t),
		}})
		record := broker.AwaitRecord(t, "model judge", nil)
		if record["state"] != "off" {
			t.Fatalf("the boot line %v does not report the judge off", record)
		}
		detail, _ := record["detail"].(string)
		for _, setting := range config.JudgeSettings {
			if !strings.Contains(detail, setting) {
				t.Fatalf("the off line's detail does not name %s, one of the settings that turn the judge on: %v", setting, record)
			}
		}
	})
}

// TestTheBinaryRefusesAJudgeAccountWithNoJudge drives the real binary with
// BROKER_JUDGE_MODEL_ACCOUNT and none of the five: an account whose identity token no configured
// judge would present names a judge that was never configured, so the broker refuses to start
// naming the setting, as config.Load refuses it, before it opens the database it is given, which
// nothing can reach.
func TestTheBinaryRefusesAJudgeAccountWithNoJudge(t *testing.T) {
	out := runRefusedBroker(t, buildBroker(t),
		"BROKER_DATABASE_URL=postgres://nonexistent-host-this-test-must-never-reach/db",
		"BROKER_PUBLIC_URL=https://secrets.internal.example",
		"BROKER_JUDGE_MODEL_ACCOUNT="+testJudgeAccount,
	)
	if !strings.Contains(out, "BROKER_JUDGE_MODEL_ACCOUNT") || !strings.Contains(out, testJudgeAccount) {
		t.Fatalf("the broker refused, but its output does not name BROKER_JUDGE_MODEL_ACCOUNT and its value:\n%s", out)
	}
}
