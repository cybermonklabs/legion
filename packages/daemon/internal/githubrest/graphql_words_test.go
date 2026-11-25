package githubrest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
)

// TestGraphQLErrorsPrintGitHubsWordsAsTheRuleReadsThem: GitHub's 200 to a query, whose errors
// echo the query's installation token in their type, their message and a step of their path, is
// GraphQLErrors that print none of it, and the path's index step is kept as an index. The twin is
// the body as the stand-in wrote it, which holds the token: the echo is real.
func TestGraphQLErrorsPrintGitHubsWordsAsTheRuleReadsThem(t *testing.T) {
	token := githubtest.InstallationToken(t.Name())
	var mu sync.Mutex
	var written string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		echo := r.Header.Get("Authorization")
		body := fmt.Sprintf(`{"data":{"viewer":null},"errors":[{"type":%q,"message":%q,"path":["viewer",%q,0]}]}`, echo, "refused "+echo, echo)
		mu.Lock()
		written = body
		mu.Unlock()
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	var data struct {
		Viewer *struct {
			Login string `json:"login"`
		} `json:"viewer"`
	}
	err := Client{Token: token, API: GraphQLEndpoint(server.URL)}.GraphQL(context.Background(), "query { viewer { login } }", nil, &data)
	var errs GraphQLErrors
	if !errors.As(err, &errs) || len(errs) != 1 || len(errs[0].Path) != 3 || errs[0].Path[2] != 0 {
		t.Fatalf("GraphQL = %v (%#v), want one error with a path of three steps, the last the index 0", err, errs)
	}
	mu.Lock()
	twin := written
	mu.Unlock()
	if !strings.Contains(twin, token) {
		t.Fatalf("the stand-in wrote %q, want the token it was sent, so the row can see a leak", twin)
	}
	for name, printed := range map[string]string{"the error": err.Error(), "the entry": fmt.Sprintf("%+v", errs[0])} {
		if piece := oauth2calltest.Piece(printed, token); piece != "" {
			t.Errorf("%s prints %q, a run of the token the query carried: %s", name, piece, printed)
		}
	}
}
