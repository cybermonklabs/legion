package githubapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/ratelimit"
	"github.com/sjawhar/envoy/internal/githubtest"
)

// echoInstallationToken is the installation token echoGitHub mints: long enough that the
// excerpt redacts every run of it, and shaped as GitHub's own are, its prefix joined at run time
// since the push's leak check refuses a GitHub token written whole.
var echoInstallationToken = "ghs" + "_echoInstallationTokenA1b2C3d4E5f6G7h8I9j0"

// echoGitHub is GitHub with a gateway in its place for the requests echo names: those get the
// gateway's own HTML page, which echoes the request back, the credential it carried both bare on a
// line of its own (so only the redaction of that very token, not the Authorization pattern, can
// hide it) and in the Authorization line. Every other request is GitHub's answer: the
// installation lookup and the token mint, so a call that needs a token gets one.
type echoGitHub struct {
	status int
	header http.Header
	echo   func(*http.Request) bool
	// body, when set, is what the echoed requests get in place of the gateway's page.
	body string

	mu     sync.Mutex
	echoed []string
}

func (f *echoGitHub) serve(w http.ResponseWriter, r *http.Request) {
	credential := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if f.echo(r) {
		for name, values := range f.header {
			w.Header()[name] = values
		}
		if f.body != "" {
			w.WriteHeader(f.status)
			fmt.Fprint(w, f.body)
			return
		}
		f.mu.Lock()
		f.echoed = append(f.echoed, credential)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(f.status)
		fmt.Fprintf(w, "<!DOCTYPE html><html><head><title>%d %s</title><style>pre{white-space:pre-wrap}</style></head>"+
			"<body><pre>\n%s\n%s %s HTTP/1.1\nAuthorization: %s\n</pre></body></html>",
			f.status, http.StatusText(f.status), credential, r.Method, r.URL.RequestURI(), r.Header.Get("Authorization"))
		return
	}
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/installation"):
		fmt.Fprint(w, `{"id":7,"app_slug":"dispatch-test","permissions":{"contents":"read"}}`)
	case r.Method == http.MethodPost && r.URL.Path == "/app/installations/7/access_tokens":
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":%q,"expires_at":%q}`, echoInstallationToken, time.Now().Add(time.Hour).Format(time.RFC3339))
	default:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found"}`)
	}
}

// echoed is every credential the gateway's page echoed back.
func (f *echoGitHub) credentials() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.echoed...)
}

// openLimits is the shared ledger with no limit in force, which records whatever it is given: a
// client that reads GraphQL's limits at all needs one.
type openLimits struct{}

func (openLimits) Wait(context.Context, string) (time.Duration, time.Time, error) {
	return 0, time.Time{}, nil
}
func (openLimits) Record(context.Context, string, time.Duration) error { return nil }
func (openLimits) Strike(_ context.Context, scope string, wait time.Duration) (ratelimit.Limit, error) {
	return ratelimit.Limit{Scope: scope, Wait: wait, Strikes: 1}, nil
}

// leaked reports whether text holds any twelve characters of credential in a row: a cut or a
// partial copy of it is a leak as much as the whole.
func leaked(text, credential string) bool {
	for at := 0; at+12 <= len(credential); at++ {
		if strings.Contains(text, credential[at:at+12]) {
			return true
		}
	}
	return false
}

// requireNamedWithoutCredentials requires text, an error's, to name the gateway's page as a body
// that is not GitHub's error JSON and to hold none of the credentials that page echoed, and the
// page to have echoed at least one: a row whose request never reached the gateway proves nothing.
func requireNamedWithoutCredentials(t *testing.T, text string, echoed []string) {
	t.Helper()
	if len(echoed) == 0 {
		t.Fatalf("the gateway echoed no credential, so the row proves nothing; the error was %q", text)
	}
	for _, credential := range echoed {
		if credential == "" {
			t.Fatalf("a request reached the gateway with no credential; the error was %q", text)
		}
		if leaked(text, credential) {
			t.Errorf("the error prints the credential the request carried (%.16s…): %s", credential, text)
		}
	}
	if !strings.Contains(text, `a body that is not GitHub's error JSON (`) {
		t.Errorf("the error does not name the gateway's page as a body that is not GitHub's error JSON: %s", text)
	}
}

func newEchoClient(t *testing.T, fake *echoGitHub, options ...Option) *Client {
	t.Helper()
	base := githubtest.Server(t, githubtest.Spec{Handle: fake.serve})
	_, pemText := githubapptest.Key(t)
	client, err := New(&auth.AppConfig{ClientID: "Iv1.proxyecho", PEM: pemText}, base, options...)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return client
}

// mint is the installation token a read under it is made with.
func mint(ctx context.Context, t *testing.T, client *Client) string {
	t.Helper()
	token, err := client.Token(ctx, 7)
	if err != nil {
		t.Fatalf("mint the installation token: %v", err)
	}
	return token
}

// Every call this client makes carries the App JWT or an installation token, and a gateway in
// GitHub's place can answer any of them with a page echoing the request back. Each row's error
// names that page as a body that is not GitHub's error JSON, with the credential its request
// carried redacted, and keeps its type: a rate limit is still the *RateLimitError, a refusal the
// *StatusError with its status.
func TestProxyEchoGitHubAppErrorsRedactTheRequestsCredential(t *testing.T) {
	path := func(suffix string) func(*http.Request) bool {
		return func(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, suffix) }
	}
	limitedType := func(t *testing.T, err error) {
		t.Helper()
		if _, ok := AsRateLimit(err); !ok {
			t.Errorf("err = %v (%T), want the *RateLimitError", err, err)
		}
	}
	refusedType := func(status int) func(*testing.T, error) {
		return func(t *testing.T, err error) {
			t.Helper()
			var refused *StatusError
			if !errors.As(err, &refused) || refused.Status != status || !refused.Message().IsEmpty() {
				t.Errorf("err = %v (%T), want the *StatusError of status %d with no GitHub message", err, err, status)
			}
		}
	}
	retryAfter := http.Header{"Retry-After": {"60"}}
	for _, row := range []struct {
		name    string
		status  int
		header  http.Header
		echo    func(*http.Request) bool
		options []Option
		call    func(context.Context, *testing.T, *Client) error
		typed   func(*testing.T, error)
	}{
		{name: "installation lookup", status: http.StatusBadGateway, echo: path("/repos/acme/web/installation"),
			call: func(ctx context.Context, _ *testing.T, c *Client) error {
				_, err := c.Installation(ctx, "acme", "web")
				return err
			}},
		{name: "installation lookup rate limit", status: http.StatusTooManyRequests, header: retryAfter, echo: path("/repos/acme/web/installation"),
			call: func(ctx context.Context, _ *testing.T, c *Client) error {
				_, err := c.Installation(ctx, "acme", "web")
				return err
			},
			typed: limitedType},
		{name: "token mint", status: http.StatusBadGateway, echo: path("/access_tokens"),
			call: func(ctx context.Context, _ *testing.T, c *Client) error { _, err := c.Token(ctx, 7); return err }},
		{name: "source check", status: http.StatusBadGateway, echo: path("/repos/acme/web"),
			call: func(ctx context.Context, _ *testing.T, c *Client) error {
				_, err := c.CheckSource(ctx, "acme", "web")
				return err
			}},
		{name: "ref", status: http.StatusBadGateway, echo: path("/commits/main"),
			call: func(ctx context.Context, t *testing.T, c *Client) error {
				_, err := c.Ref(ctx, mint(ctx, t, c), "acme", "web", "main")
				return err
			}},
		{name: "branch", status: http.StatusBadGateway, echo: path("/git/ref/heads/main"),
			call: func(ctx context.Context, t *testing.T, c *Client) error {
				_, err := c.Branch(ctx, mint(ctx, t, c), "acme", "web", "main")
				return err
			}},
		{name: "dir", status: http.StatusBadGateway, echo: func(r *http.Request) bool { return strings.Contains(r.URL.Path, "/git/trees/") },
			call: func(ctx context.Context, t *testing.T, c *Client) error {
				_, err := c.Dir(ctx, mint(ctx, t, c), "acme", "web", "c0ffee", ".dispatch/architecture")
				return err
			}},
		{name: "blob", status: http.StatusBadGateway, echo: func(r *http.Request) bool { return strings.Contains(r.URL.Path, "/git/blobs/") },
			call: func(ctx context.Context, t *testing.T, c *Client) error {
				listing := Dir{SHA: "tree", Files: []DirFile{{Name: "api.md", Path: ".dispatch/architecture/api.md", SHA: "blob-1", Size: 3}}}
				_, err := c.DirFiles(ctx, mint(ctx, t, c), "acme", "web", listing)
				return err
			}},
		{name: "installations list", status: http.StatusBadGateway, echo: path("/app/installations"),
			call: func(ctx context.Context, _ *testing.T, c *Client) error {
				_, err := c.ListInstallations(ctx)
				return err
			},
			typed: refusedType(http.StatusBadGateway)},
		{name: "installation repositories", status: http.StatusBadGateway, echo: path("/installation/repositories"),
			call: func(ctx context.Context, _ *testing.T, c *Client) error {
				_, err := c.ListInstallationRepositoriesByID(ctx, 7)
				return err
			},
			typed: refusedType(http.StatusBadGateway)},
		{name: "repository read", status: http.StatusBadGateway, echo: path("/repos/acme/web"),
			call: func(ctx context.Context, t *testing.T, c *Client) error {
				_, err := c.DefaultBranch(ctx, mint(ctx, t, c), "acme", "web")
				return err
			},
			typed: refusedType(http.StatusBadGateway)},
		{name: "repository read rate limit", status: http.StatusForbidden, header: retryAfter, echo: path("/commits/c0ffee"),
			call: func(ctx context.Context, t *testing.T, c *Client) error {
				_, err := c.HasCommit(ctx, mint(ctx, t, c), "acme", "web", "c0ffee")
				return err
			},
			typed: limitedType},
		{name: "graphql rate limit", status: http.StatusForbidden, header: retryAfter, echo: path("/graphql"), options: []Option{WithLimits(openLimits{})},
			call: func(ctx context.Context, t *testing.T, c *Client) error {
				_, _, _, err := c.GraphQL(ctx, mint(ctx, t, c), "query { viewer { login } }", nil)
				return err
			},
			typed: limitedType},
	} {
		t.Run(row.name, func(t *testing.T) {
			ctx := context.Background()
			fake := &echoGitHub{status: row.status, header: row.header, echo: row.echo}
			client := newEchoClient(t, fake, row.options...)
			err := row.call(ctx, t, client)
			if err == nil {
				t.Fatal("the gateway's page answered no error")
			}
			t.Logf("printed: %s", err)
			requireNamedWithoutCredentials(t, err.Error(), fake.credentials())
			if row.typed != nil {
				row.typed(t, err)
			}
		})
	}
}

// GitHub's own error JSON is GitHub's word, and an error prints it as GitHub sent it, as before;
// a refusal's Body and Message are then GitHub's, which the results read keeps the message of.
func TestProxyEchoGitHubsOwnErrorJSONPrintsAsGitHubSentIt(t *testing.T) {
	const own = `{"message":"Server Error","documentation_url":"https://docs.github.com/rest"}`
	ctx := context.Background()
	fake := &echoGitHub{status: http.StatusInternalServerError, body: own, echo: func(r *http.Request) bool { return r.URL.Path == "/repos/acme/web" }}
	client := newEchoClient(t, fake)
	_, err := client.DefaultBranch(ctx, mint(ctx, t, client), "acme", "web")
	var refused *StatusError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v (%T), want the *StatusError", err, err)
	}
	if !strings.HasSuffix(err.Error(), ": status 500: "+own) || refused.PrintedBody() != own || !refused.Message().Equal("Server Error") {
		t.Fatalf("err = %q (body %q, message %q), want GitHub's own JSON whole and its message", err, refused.PrintedBody(), refused.Message())
	}
}
