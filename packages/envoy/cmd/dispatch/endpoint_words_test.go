package main

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAModelEndpointTheBootRefusesPrintsNoneOfIt: a chat or embeddings endpoint that is no absolute
// https URL with a host and no query or fragment, carrying redacttest's token in its query
// (chat-query, azure-chat-query, embed-query) or its user name and password in a plain-http URL
// (chat-user-info), is refused naming its setting, and the refusal holds no run of a credential.
// The refusal quoted the endpoint as written. The twin shows each endpoint as written holds a
// credential and is still refused naming its setting, so the row can see one printed. Captures
// dispatch.model-endpoint/<case>.
func TestAModelEndpointTheBootRefusesPrintsNoneOfIt(t *testing.T) {
	const query = "?api-key=" + redacttest.Token
	cases := []struct {
		name, setting string
		env           map[string]string
	}{
		{"chat-query", "DISPATCH_CHAT_ENDPOINT", endpointChatEnvironment(map[string]string{
			"DISPATCH_CHAT_ENDPOINT": "https://chat.example/v1" + query})},
		{"chat-user-info", "DISPATCH_CHAT_ENDPOINT", endpointChatEnvironment(map[string]string{
			"DISPATCH_CHAT_ENDPOINT": "http://" + redacttest.User + ":" + redacttest.Password + "@chat.example/v1"})},
		{"azure-chat-query", "DISPATCH_AZURE_CHAT_ENDPOINT", azureChatEnvironment(map[string]string{
			"DISPATCH_AZURE_CHAT_ENDPOINT": "https://chat.example/models" + query})},
		{"embed-query", "DISPATCH_AZURE_EMBED_ENDPOINT", azureEmbedEnvironment(map[string]string{
			"DISPATCH_AZURE_EMBED_ENDPOINT": "https://embed.example/models" + query})},
	}
	t.Run("twin", func(t *testing.T) {
		for _, tc := range cases {
			if held := redacttest.Held(tc.env[tc.setting]); len(held) == 0 {
				t.Errorf("%s: the endpoint %q holds no credential, so the row could see none printed", tc.name, tc.env[tc.setting])
			}
			if _, err := resolveBootConfig(envGetter(tc.env)); err == nil || !strings.Contains(err.Error(), tc.setting) {
				t.Errorf("%s: resolveBootConfig = %v; want a refusal naming %s", tc.name, err, tc.setting)
			}
		}
	})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveBootConfig(envGetter(tc.env))
			if err == nil {
				t.Fatalf("resolveBootConfig took %s=%q; want it refused", tc.setting, tc.env[tc.setting])
			}
			if !strings.Contains(err.Error(), tc.setting) {
				t.Errorf("resolveBootConfig = %v; want a refusal naming %s", err, tc.setting)
			}
			redacttest.RequireNoPiece(t, "dispatch.model-endpoint/"+tc.name, err.Error(), redacttest.User, redacttest.Password, redacttest.Token)
		})
	}
}
