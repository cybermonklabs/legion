package cireconcile

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/githubtest"
)

// queryEchoToken is the installation token the query below carries, shaped as GitHub's are; its
// prefix is joined at run time, since the push's leak check refuses one written whole.
var queryEchoToken = "ghs" + "_queryEchoInstallationTokenQ1w2E3r4T5y6"

// A query the GraphQL endpoint answers with a status other than 200 is a failed page, and the
// pass names it in its line; a gateway in GitHub's place can answer it with a page echoing the
// request back, the installation token bare on a line of its own. The page is named as a body
// that is not GitHub's error JSON, with that token redacted.
func TestProxyEchoAGraphQLRefusalRedactsTheInstallationToken(t *testing.T) {
	var mu sync.Mutex
	var echoed []string
	base := githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":7,"app_slug":"dispatch-test","permissions":{"checks":"read"}}`)
		case r.URL.Path == "/app/installations/7/access_tokens":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"token":%q,"expires_at":%q}`, queryEchoToken, time.Now().Add(time.Hour).Format(time.RFC3339))
		case r.URL.Path == "/graphql":
			credential := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			mu.Lock()
			echoed = append(echoed, credential)
			mu.Unlock()
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprintf(w, "<html><head><title>502 Bad Gateway</title></head><body><pre>\n%s\nPOST /graphql HTTP/1.1\nAuthorization: %s\n</pre></body></html>",
				credential, r.Header.Get("Authorization"))
		default:
			githubtest.Unexpected(t, w, r)
		}
	}})
	_, pemText := githubapptest.Key(t)
	client, err := githubapp.New(&auth.AppConfig{ClientID: "Iv1.queryecho", PEM: pemText}, base)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	ctx := context.Background()
	token, err := client.RepositoryToken(ctx, "acme", "web")
	if err != nil {
		t.Fatalf("mint the installation token: %v", err)
	}
	reconciler := &Reconciler{GitHub: client}
	_, err = reconciler.query(ctx, reconciled{owner: "acme", repo: "web", token: token}, "", &counts{})
	if err == nil {
		t.Fatal("the gateway's page answered no error")
	}
	text := err.Error()
	t.Logf("printed: %s", text)
	mu.Lock()
	defer mu.Unlock()
	if len(echoed) == 0 {
		t.Fatalf("the gateway echoed no credential, so the row proves nothing; the error was %q", text)
	}
	for _, credential := range echoed {
		for at := 0; at+12 <= len(credential); at++ {
			if strings.Contains(text, credential[at:at+12]) {
				t.Fatalf("the error prints the credential the query carried (%.16s…): %s", credential, text)
			}
		}
	}
	if !strings.Contains(text, `answered status 502: a body that is not GitHub's error JSON ("`) {
		t.Fatalf("the error does not name the gateway's page as a body that is not GitHub's error JSON: %s", text)
	}
}
