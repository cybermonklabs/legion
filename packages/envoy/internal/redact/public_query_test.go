package redact

import (
	"net/http"
	"net/url"
	"slices"
	"testing"
)

// TestAQueryValueIsACredentialUnlessItsNameIsPublicOnTheRoute: QueryTokens counts each value it is
// given under a name no contract declares public on the request's route, a new name included, and
// no value under a name one does: Azure's api-version on the chat completions routes its REST API
// reference names, and on no other route.
func TestAQueryValueIsACredentialUnlessItsNameIsPublicOnTheRoute(t *testing.T) {
	query := url.Values{"api-version": {"2024-10-21"}, "key": {"query-RDX-value"}, "a_new_parameter": {"unlisted-RDX-value"}}
	for _, c := range []struct {
		name, method, target string
		want                 []string
	}{
		{"azure v1 chat completions", http.MethodPost, "https://resource.example/openai/v1/chat/completions", []string{"query-RDX-value", "unlisted-RDX-value"}},
		{"azure deployment chat completions", http.MethodPost, "https://resource.example/openai/deployments/chat-deployment/chat/completions", []string{"query-RDX-value", "unlisted-RDX-value"}},
		{"another route", http.MethodPost, "https://resource.example/v1/messages", []string{"2024-10-21", "query-RDX-value", "unlisted-RDX-value"}},
		{"a read of the chat completions path", http.MethodGet, "https://resource.example/openai/v1/chat/completions", []string{"2024-10-21", "query-RDX-value", "unlisted-RDX-value"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, err := http.NewRequest(c.method, c.target, nil)
			if err != nil {
				t.Fatal(err)
			}
			got := QueryTokens(r, query)
			slices.Sort(got)
			if !slices.Equal(got, c.want) {
				t.Errorf("QueryTokens on %s %s = %q, want %q", c.method, c.target, got, c.want)
			}
		})
	}
}
