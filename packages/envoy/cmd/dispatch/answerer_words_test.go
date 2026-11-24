package main

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestTheBootNamesNoCredentialOfAChatEndpointItRefuses: Dispatch's boot refuses a chat endpoint
// that is not an absolute https URL with a host and no query - one net/url cannot parse, for its
// port (unparsed), and one that parses and carries a query (query) - while the endpoint holds the
// probe's user name, password and access token, for either place that reads one. The refusal,
// which main logs as its boot error, names the setting and the shape it wants and the endpoint
// only as redact.URL reads it. Captures dispatch.answerer-endpoint/<place>-<case>.
func TestTheBootNamesNoCredentialOfAChatEndpointItRefuses(t *testing.T) {
	const credentials = redacttest.User + ":" + redacttest.Password + "@"
	const query = "?access" + "_token=" + redacttest.Token
	for _, c := range []struct {
		name, setting, endpoint string
		env                     func(map[string]string) map[string]string
	}{
		{"endpoint-unparsed", "DISPATCH_CHAT_ENDPOINT", "https://" + credentials + "chat.example:port/v1" + query, endpointChatEnvironment},
		{"endpoint-query", "DISPATCH_CHAT_ENDPOINT", "https://" + credentials + "chat.example/v1" + query, endpointChatEnvironment},
		{"azure-unparsed", "DISPATCH_AZURE_CHAT_ENDPOINT", "https://" + credentials + "chat.example:port/models" + query, azureChatEnvironment},
		{"azure-query", "DISPATCH_AZURE_CHAT_ENDPOINT", "https://" + credentials + "chat.example/models" + query, azureChatEnvironment},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := resolveBootConfig(envGetter(c.env(map[string]string{c.setting: c.endpoint})))
			if err == nil {
				t.Fatalf("the boot took %s=%s", c.setting, c.endpoint)
			}
			redacttest.RequireNone(t, "dispatch.answerer-endpoint/"+c.name, err.Error())
			if !strings.Contains(err.Error(), c.setting+"=") || !strings.Contains(err.Error(), "(expected an absolute https URL") {
				t.Errorf("the boot's refusal is %q, want it to name %s and the shape it wants", err, c.setting)
			}
		})
	}
}
