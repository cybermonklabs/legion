package main

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestADispatchAgentSecretsURLTheBootRefusesPrintsNoneOfIt: a DISPATCH_AGENT_SECRETS_URL with
// redacttest's credentials in its user information (user-info), the token in its path (path), in
// its query (query) or in its fragment (fragment) is refused naming the setting, and the refusal
// holds no run of a credential. The URL is held to BROKER_PUBLIC_URL's form, which the broker holds
// that URL to. The refusal quoted the URL as the rule reads it, which prints a plain path as
// written, and the boot took the other three. The twin shows that the URL with the token in its
// path is still refused naming DISPATCH_AGENT_SECRETS_URL, and that the rule's reading of it holds
// the token, so the row can see one printed. Captures dispatch.agent-secrets-url/<case>.
func TestADispatchAgentSecretsURLTheBootRefusesPrintsNoneOfIt(t *testing.T) {
	boot := func(brokerURL string) settingValues {
		return envGetter(map[string]string{
			"DATABASE_URL":                              "postgres://dispatch",
			"DISPATCH_AGENT_TOKEN":                      "agent-token",
			"DISPATCH_IDENTITY":                         "header:X-Dispatch-User",
			"DISPATCH_IDENTITY_HEADER_TRUSTED":          "1",
			"DISPATCH_AGENT_SECRETS_URL":                brokerURL,
			"DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE": "/var/run/secrets/broker/token",
		})
	}
	const host = "broker.example.com"
	inPath := "https://" + host + "/" + redacttest.Token
	t.Run("twin", func(t *testing.T) {
		if _, err := resolveBootConfig(boot(inPath)); err == nil || !strings.Contains(err.Error(), "DISPATCH_AGENT_SECRETS_URL") {
			t.Errorf("resolveBootConfig(DISPATCH_AGENT_SECRETS_URL=%q) = %v; want a refusal naming the setting", inPath, err)
		}
		if shown := redact.URL(inPath); !strings.Contains(shown, redacttest.Token) {
			t.Errorf("redact.URL(%q) = %q; want the token in it, so the row can see one printed", inPath, shown)
		}
	})
	for _, tc := range []struct{ name, brokerURL string }{
		{"user-info", "https://" + redacttest.User + ":" + redacttest.Password + "@" + host},
		{"path", inPath},
		{"query", "https://" + host + "?access" + "_token=" + redacttest.Token},
		{"fragment", "https://" + host + "#access" + "_token=" + redacttest.Token},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveBootConfig(boot(tc.brokerURL))
			if err == nil {
				t.Fatalf("resolveBootConfig took DISPATCH_AGENT_SECRETS_URL=%q; want it refused", tc.brokerURL)
			}
			if !strings.Contains(err.Error(), "DISPATCH_AGENT_SECRETS_URL") {
				t.Errorf("resolveBootConfig = %v; want a refusal naming DISPATCH_AGENT_SECRETS_URL", err)
			}
			redacttest.RequireNoPiece(t, "dispatch.agent-secrets-url/"+tc.name, err.Error(), redacttest.User, redacttest.Password, redacttest.Token)
		})
	}
}
