package embed_test

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/embed"
)

// The embeddings endpoint (DISPATCH_AZURE_EMBED_ENDPOINT) may carry a user and a password, and
// net/http's transport error quotes the request's URL with the user kept and a token written as
// the whole user part whole. An embedding whose service cannot be reached fails naming no part of
// it, with the failure's class; the search route logs that error. The control is the same endpoint
// with no user part, whose error is net/http's own, the request's URL and its api-version query
// with it, since redact.URL prints that endpoint whole. The user part's values are made for this
// test.
func TestAnEmbeddingsEndpointWithAUserPartIsNamedWithNoneOfIt(t *testing.T) {
	const (
		user     = "made-up-user-9e1a"
		password = "not-a-secret-2f6b80"
		token    = "not-a-secret-tok7c3"
	)
	for _, userPart := range []string{user + ":" + password + "@", token + "@", ""} {
		client, err := embed.NewAzure(embed.AzureConfig{
			Endpoint:        "http://" + userPart + "127.0.0.1:1/models",
			Deployment:      "made-up-deployment",
			TokensPerMinute: 1000,
			APIKey:          "not-a-secret-api-key",
		})
		if err != nil {
			t.Fatalf("user part %q: NewAzure: %v", userPart, err)
		}
		_, err = client.Embed(context.Background(), []string{"hello"}, embed.InputDocument)
		if err == nil || !strings.Contains(err.Error(), "connection refused") {
			t.Fatalf("user part %q: err = %v, want the refused connection's class", userPart, err)
		}
		if userPart == "" && !strings.Contains(err.Error(), `Post "http://127.0.0.1:1/models/embeddings?api-version=`) {
			t.Errorf("the control: err = %v, want net/http's own error naming the request's URL", err)
		}
		for _, part := range []string{user, password, token} {
			if strings.Contains(err.Error(), part) {
				t.Errorf("user part %q: the error holds a part of the user part: %q", userPart, err)
			}
		}
	}
}
