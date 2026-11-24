package embed

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAnEmbeddingAnswerNamesItsStatusWithNoPhraseItWrote: Azure's embeddings endpoint, or
// whatever answers in its place, refuses a batch under a reason phrase that quotes a credential's
// URL, with an error code (azure-coded) and with none (azure-uncoded), and so does Entra's token
// endpoint, with an error (entra-error) and with none (entra-bare). Each error names the status by
// its code and the standard phrase (redact.StatusLine), and none of the phrase the endpoint
// wrote. Captures embed.status-line/<case>.
func TestAnEmbeddingAnswerNamesItsStatusWithNoPhraseItWrote(t *testing.T) {
	azure := func(t *testing.T, base string) error {
		client, err := NewAzure(AzureConfig{Endpoint: base + "/models", Deployment: "embed-v-4-0", TokensPerMinute: 300_000, APIKey: testAzureKey})
		if err != nil {
			t.Fatalf("NewAzure: %v", err)
		}
		_, err = client.Embed(context.Background(), []string{"x"}, InputDocument)
		return err
	}
	entra := func(t *testing.T, base string) error {
		tokenFile := filepath.Join(t.TempDir(), "token")
		if err := os.WriteFile(tokenFile, []byte("federated-assertion"), 0o600); err != nil {
			t.Fatalf("write token file: %v", err)
		}
		source, err := NewEntraTokenSource(EntraConfig{TenantID: "test-tenant", ClientID: "test-client", TokenFile: tokenFile, Scope: DefaultEntraScope, Authority: base + "/"})
		if err != nil {
			t.Fatalf("NewEntraTokenSource: %v", err)
		}
		_, err = source.Token(context.Background())
		return err
	}
	for _, c := range []struct {
		name, header, body, want string
		status                   int
		call                     func(*testing.T, string) error
	}{
		{"azure-coded", "Content-Type: application/json\r\n", `{"error":{"code":"RateLimitReached"}}`, "HTTP 429 Too Many Requests, error code", http.StatusTooManyRequests, azure},
		{"azure-uncoded", "", "", "HTTP 502 Bad Gateway", http.StatusBadGateway, azure},
		{"entra-error", "Content-Type: application/json\r\n", `{"error":"invalid_client"}`, "HTTP 401 Unauthorized, error", http.StatusUnauthorized, entra},
		{"entra-bare", "", "", "HTTP 500 Internal Server Error", http.StatusInternalServerError, entra},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := c.call(t, redacttest.StatusLineServer(t, c.status, c.header, c.body))
			if err == nil {
				t.Fatal("the call answered no error")
			}
			redacttest.RequireNone(t, "embed.status-line/"+c.name, err.Error())
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the error is %q, want %q in it", err, c.want)
			}
		})
	}
}
