package githubrest

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/urljoin"
)

// TestAGraphQLQueryJoinsGraphqlOntoTheAPIURL: GitHub's API root with a path and a query (path and
// query): a query posts to /graphql joined onto that path with the query kept (GraphQLEndpoint,
// urljoin.Join). Where GraphQLEndpoint wrote /graphql after the root's text, /graphql went inside
// the query and the stand-in was asked the root's own path. A root with a trailing slash (trailing
// slash) and a plain origin (origin) are asked /graphql under them, as before.
func TestAGraphQLQueryJoinsGraphqlOntoTheAPIURL(t *testing.T) {
	for _, c := range []struct{ name, suffix, asked string }{
		{"path and query", "/api?key=k3y-made-up", "/api/graphql?key=k3y-made-up"},
		{"trailing slash", "/api/", "/api/graphql"},
		{"origin", "", "/graphql"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var mu sync.Mutex
			var asked []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				asked = append(asked, r.Method+" "+r.URL.RequestURI())
				mu.Unlock()
				_, _ = w.Write([]byte(`{"data":{"viewer":{"login":"octocat"}}}`))
			}))
			t.Cleanup(server.Close)
			var data struct {
				Viewer struct {
					Login string `json:"login"`
				} `json:"viewer"`
			}
			err := Client{Token: "token", API: GraphQLEndpoint(server.URL + c.suffix)}.GraphQL(context.Background(), "query { viewer { login } }", nil, &data)
			mu.Lock()
			defer mu.Unlock()
			if want := []string{"POST " + c.asked}; !slices.Equal(asked, want) {
				t.Errorf("GraphQL under %s asked %q; want %q", server.URL+c.suffix, asked, want)
			}
			if err != nil || data.Viewer.Login != "octocat" {
				t.Errorf("GraphQL under %s = %v and read %q; want the stand-in's data read", server.URL+c.suffix, err, data.Viewer.Login)
			}
		})
	}
}

// TestAGraphQLEndpointThatIsNoURLIsRefusedQuotingNoneOfIt: an API root that is no absolute URL
// with a host has no GraphQL endpoint (""), and a query under it is refused as urljoin.ErrBase
// before any request, in words that quote none of it, since the root can carry a credential.
func TestAGraphQLEndpointThatIsNoURLIsRefusedQuotingNoneOfIt(t *testing.T) {
	endpoint := GraphQLEndpoint("127.0.0.1:9?key=k3y-made-up")
	if endpoint != "" {
		t.Fatalf("GraphQLEndpoint of a root with no scheme = %q; want none", endpoint)
	}
	err := Client{Token: "token", API: endpoint}.GraphQL(context.Background(), "query { viewer { login } }", nil, &struct{}{})
	if !errors.Is(err, urljoin.ErrBase) || strings.Contains(err.Error(), "k3y-made-up") || strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("GraphQL under no endpoint = %v; want urljoin.ErrBase, quoting none of the root", err)
	}
}
