// packages/envoy/cmd/agent-secrets/machine_login_split_linux_test.go
//go:build linux

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestMachineLoginPrintsNoCredentialASplitRefusalQuotes drives the real `agent-secrets machine
// login` through a real agent-secrets-helper whose broker refuses the device authorization as
// invalid_request, described as the broker describes a refusal of its own ("<CODE>: <message>"),
// but with words that quote a URL carrying a credential across the ": ". The rule cannot read the
// whole description as clean, so the helper keeps it whole and the command's stderr names it as
// not readable: neither half is read on its own. Its designated assertion is
// agent-secrets.machine-login-split/<row>.
func TestMachineLoginPrintsNoCredentialASplitRefusalQuotes(t *testing.T) {
	binary := buildAgentSecrets(t)
	for name, description := range map[string]string{
		"user-info": "see https://" + redacttest.User + ": " + redacttest.Password + "@issuer.example/p",
		"query":     "https://issuer.example/cb?token=: " + redacttest.Token,
	} {
		t.Run(name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("POST /v1/oauth/device_authorization", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				writeJSON(w, map[string]any{"error": "invalid_request", "error_description": description})
			})
			broker := httptest.NewServer(mux)
			defer broker.Close()
			_, sock := serveRealHelper(t, broker.URL, "ada@example.com")
			_, stderr, exit := runAgentSecrets(t, binary, broker.URL, t.TempDir(), []string{"AGENT_SECRETS_HELPER_SOCK=" + sock},
				"machine", "login")
			if exit == 0 {
				t.Fatalf("machine login against a refusing broker exited 0 (stderr %q)", stderr)
			}
			redacttest.RequireNone(t, "agent-secrets.machine-login-split/"+name, stderr)
		})
	}
}
