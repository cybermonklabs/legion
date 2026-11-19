package going_test

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/going"
)

type madeUpToken struct{}

func (madeUpToken) Token(context.Context) (string, error) { return "made-up-bearer", nil }

// The chat endpoint (DISPATCH_CHAT_ENDPOINT, or the Azure chat endpoint) may carry a user and a
// password, and net/http's transport error quotes the request's URL with the user kept and a
// token written as the whole user part whole. A chat whose service cannot be reached fails naming
// no part of it, with the failure's class. The control is the same endpoint with no user part,
// whose error is net/http's own, the request's URL and its api-version query with it, since
// redact.URL prints that endpoint whole. The user part's values are made for this test.
func TestAChatEndpointWithAUserPartIsNamedWithNoneOfIt(t *testing.T) {
	const (
		user     = "made-up-user-4b2e"
		password = "not-a-secret-c71d09"
		token    = "not-a-secret-tok5a1"
	)
	for _, userPart := range []string{user + ":" + password + "@", token + "@", ""} {
		client, err := going.NewHTTPClient(going.HTTPConfig{
			Place:      "endpoint",
			Endpoint:   "http://" + userPart + "127.0.0.1:1/v1",
			Model:      "made-up-model",
			APIVersion: "made-up-version",
			Tokens:     madeUpToken{},
		})
		if err != nil {
			t.Fatalf("user part %q: NewHTTPClient: %v", userPart, err)
		}
		_, err = client.Chat(context.Background(), going.ChatRequest{Messages: []going.Message{{Role: "user", Content: "hello"}}}, nil)
		if err == nil || !strings.Contains(err.Error(), "connection refused") {
			t.Fatalf("user part %q: err = %v, want the refused connection's class", userPart, err)
		}
		if userPart == "" && !strings.Contains(err.Error(), `Post "http://127.0.0.1:1/v1/chat/completions?api-version=made-up-version"`) {
			t.Errorf("the control: err = %v, want net/http's own error naming the request's URL", err)
		}
		for _, part := range []string{user, password, token} {
			if strings.Contains(err.Error(), part) {
				t.Errorf("user part %q: the error holds a part of the user part: %q", userPart, err)
			}
		}
	}
}
