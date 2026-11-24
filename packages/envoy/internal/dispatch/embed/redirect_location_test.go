package embed

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAnEmbeddingRedirectPrintsNoLocationOfIt: whatever answers in Azure's place answers the
// embeddings call, which carries the api-key, and whatever answers in Entra's place answers the
// token exchange, which carries the federated assertion in its form, each with a redirect whose
// Location quotes that credential and does not parse. Go parses a Location before a client's
// CheckRedirect is asked, so net/http's error quotes it whatever the client's redirect policy;
// the call's error, which the embedding queue logs, holds no run of the credential and no word of
// the Location. Captures embed.redirect/<case>.
func TestAnEmbeddingRedirectPrintsNoLocationOfIt(t *testing.T) {
	const apiKey = "hc8pv-not-a-secret-RDX-3e7a"
	const assertion = "nb5ys-not-a-secret-RDX-9f2c"
	echo := func(credential func(r *http.Request) string) string {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "/moved%zz"+credential(r))
			w.WriteHeader(http.StatusFound)
		}))
		t.Cleanup(server.Close)
		return server.URL
	}
	for _, c := range []struct {
		name, credential string
		call             func(t *testing.T, ctx context.Context) error
	}{
		{"azure", apiKey, func(t *testing.T, ctx context.Context) error {
			base := echo(func(r *http.Request) string { return r.Header.Get("api-key") })
			client, err := NewAzure(AzureConfig{Endpoint: base + "/models", Deployment: "embed-v-4-0", TokensPerMinute: 300_000, APIKey: apiKey})
			if err != nil {
				t.Fatalf("NewAzure: %v", err)
			}
			_, err = client.Embed(ctx, []string{"x"}, InputDocument)
			return err
		}},
		{"entra", assertion, func(t *testing.T, ctx context.Context) error {
			base := echo(func(r *http.Request) string { _ = r.ParseForm(); return r.PostForm.Get("client_assertion") })
			tokenFile := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(tokenFile, []byte(assertion), 0o600); err != nil {
				t.Fatalf("write token file: %v", err)
			}
			source, err := NewEntraTokenSource(EntraConfig{TenantID: "test-tenant", ClientID: "test-client", TokenFile: tokenFile, Scope: DefaultEntraScope, Authority: base + "/"})
			if err != nil {
				t.Fatalf("NewEntraTokenSource: %v", err)
			}
			_, err = source.Token(ctx)
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			err := c.call(t, ctx)
			if err == nil {
				t.Fatal("the call answered no error")
			}
			redacttest.RequireNoPiece(t, "embed.redirect/"+c.name, err.Error(), c.credential)
			if strings.Contains(err.Error(), "moved") {
				t.Errorf("the call's error names the Location: %q", err)
			}
		})
	}
}
