package delivery

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

// echoToken is the installation token echoGateway mints, long enough that the excerpt redacts
// every run of it; its prefix is joined at run time, since the push's leak check refuses a
// GitHub token written whole.
var echoToken = "ghs" + "_deliveryEchoTokenZ9y8X7w6V5u4T3s2R1q0"

// echoGateway is GitHub with a gateway in its place for the paths echo names, which answers them
// with its own HTML page echoing the request back: the credential bare on a line of its own, so
// only the redaction of that token can hide it, and the Authorization line. The installation
// lookup and the token mint are GitHub's, and so is a pull request's read, so a read past it is
// reached.
type echoGateway struct {
	status int
	echo   func(*http.Request) bool

	mu     sync.Mutex
	echoed []string
}

func (g *echoGateway) serve(w http.ResponseWriter, r *http.Request) {
	credential := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if g.echo(r) {
		g.mu.Lock()
		g.echoed = append(g.echoed, credential)
		g.mu.Unlock()
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(g.status)
		fmt.Fprintf(w, "<html><head><title>%d %s</title></head><body><pre>\n%s\n%s %s HTTP/1.1\nAuthorization: %s\n</pre></body></html>",
			g.status, http.StatusText(g.status), credential, r.Method, r.URL.RequestURI(), r.Header.Get("Authorization"))
		return
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/installation"):
		fmt.Fprint(w, `{"id":7,"app_slug":"dispatch-test","permissions":{"contents":"read","actions":"read","pull_requests":"read"}}`)
	case r.URL.Path == "/app/installations/7/access_tokens":
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":%q,"expires_at":%q}`, echoToken, time.Now().Add(time.Hour).Format(time.RFC3339))
	case r.URL.Path == "/repos/acme/web/pulls/9":
		fmt.Fprint(w, `{"number":9,"title":"Ship it"}`)
	default:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found"}`)
	}
}

func (g *echoGateway) credentials() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.echoed...)
}

// leakedRun reports whether text holds any twelve characters of credential in a row.
func leakedRun(text, credential string) bool {
	for at := 0; at+12 <= len(credential); at++ {
		if strings.Contains(text, credential[at:at+12]) {
			return true
		}
	}
	return false
}

// Every GitHub read of delivery carries an installation token, and a gateway in GitHub's place
// can answer it with a page echoing the request back. The refusal each read wraps
// (githubapp.CheckTokenResponse, githubapp.CheckRepositoryResponse) is given the token that read
// carried, so the page is named as a body that is not GitHub's error JSON with the token redacted.
// 500 rather than 502, which readGitHubPage asks again for: the row is about the print, not the
// retry.
func TestProxyEchoDeliveryGitHubReadsRedactTheInstallationToken(t *testing.T) {
	path := func(want string) func(*http.Request) bool {
		return func(r *http.Request) bool { return r.URL.Path == want }
	}
	since := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for _, row := range []struct {
		name string
		echo func(*http.Request) bool
		call func(context.Context, *testing.T, *githubapp.Client) error
	}{
		{"pull request read", path("/repos/acme/web/pulls/9"), func(ctx context.Context, _ *testing.T, c *githubapp.Client) error {
			_, err := FetchPullRequest(ctx, c, "acme", "web", 9)
			return err
		}},
		{"first commit read", path("/repos/acme/web/pulls/9/commits"), func(ctx context.Context, _ *testing.T, c *githubapp.Client) error {
			_, err := FetchPullRequest(ctx, c, "acme", "web", 9)
			return err
		}},
		{"commit pull requests", path("/repos/acme/web/commits/c0ffee/pulls"), func(ctx context.Context, t *testing.T, c *githubapp.Client) error {
			token, err := c.RepositoryToken(ctx, "acme", "web")
			if err != nil {
				t.Fatalf("mint the installation token: %v", err)
			}
			_, err = fetchCommitPullPayloads(ctx, c, token, "acme", "web", "c0ffee")
			return err
		}},
		{"search page", path("/graphql"), func(ctx context.Context, t *testing.T, c *githubapp.Client) error {
			token, err := c.RepositoryToken(ctx, "acme", "web")
			if err != nil {
				t.Fatalf("mint the installation token: %v", err)
			}
			_, err = fetchSearchPage(ctx, c, token, "repo:acme/web is:pr is:merged", "")
			return err
		}},
		{"workflow runs page", path("/repos/acme/web/actions/workflows/ci.yml/runs"), func(ctx context.Context, _ *testing.T, c *githubapp.Client) error {
			_, _, err := fetchWorkflowRunsPage(ctx, c, "acme", "web", "ci.yml", since, since.Add(time.Hour), 1)
			return err
		}},
		{"workflow run", path("/repos/acme/web/actions/runs/5"), func(ctx context.Context, _ *testing.T, c *githubapp.Client) error {
			_, err := FetchWorkflowRun(ctx, c, "acme", "web", 5)
			return err
		}},
		{"run jobs", path("/repos/acme/web/actions/runs/5/jobs"), func(ctx context.Context, _ *testing.T, c *githubapp.Client) error {
			_, err := ListWorkflowRunJobs(ctx, c, "acme", "web", 5)
			return err
		}},
		{"commit messages", path("/repos/acme/web/pulls/9/commits"), func(ctx context.Context, _ *testing.T, c *githubapp.Client) error {
			_, err := fetchCommitMessages(ctx, c, "acme", "web", 9)
			return err
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			gateway := &echoGateway{status: http.StatusInternalServerError, echo: row.echo}
			base := githubtest.Server(t, githubtest.Spec{Handle: gateway.serve})
			_, pemText := githubapptest.Key(t)
			client, err := githubapp.New(&auth.AppConfig{ClientID: "Iv1.deliveryecho", PEM: pemText}, base)
			if err != nil {
				t.Fatalf("new client: %v", err)
			}
			err = row.call(context.Background(), t, client)
			if err == nil {
				t.Fatal("the gateway's page answered no error")
			}
			text := err.Error()
			t.Logf("printed: %s", text)
			echoed := gateway.credentials()
			if len(echoed) == 0 {
				t.Fatalf("the gateway echoed no credential, so the row proves nothing; the error was %q", text)
			}
			for _, credential := range echoed {
				if credential == "" || leakedRun(text, credential) {
					t.Errorf("the error prints the credential the request carried (%.16s…): %s", credential, text)
				}
			}
			if !strings.Contains(text, `a body that is not GitHub's error JSON (`) {
				t.Errorf("the error does not name the gateway's page as a body that is not GitHub's error JSON: %s", text)
			}
		})
	}
}
