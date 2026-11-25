package githubrest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// asked is what one call reached the API address with: the path of the request and its query, as
// the far side read them.
type asked struct {
	mu      sync.Mutex
	path    string
	rawQuit string
}

// record keeps the first request a server was sent.
func (a *asked) record(r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.path == "" {
		a.path, a.rawQuit = r.URL.Path, r.URL.RawQuery
	}
}

// read is the path and query the call reached.
func (a *asked) read() (string, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.path, a.rawQuit
}

// TestAReadGoesToThePathJoinedOntoTheConfiguredAPI: a deployment's GitHub API address is a
// configured URL, and it may carry a path and a query of its own - a gateway's route key, say.
// Every read this client makes joins its own path onto that URL's path and keeps the configured
// query ahead of its own (urljoin.Join, RepositoryAPI and Client.callURL), so GitHub is asked
// <api path>/repos/<owner>/<name>/<read's path> with the configured query. The read's path
// appended to the address's text would sit inside that query's last value, and GitHub would be
// asked the address's own path instead.
//
// The three ways a path reaches a request are read: Get, Post (through send) and GetPages, whose
// own per_page query follows the configured one. The twin of each is the same read under an
// address with no query, which reaches the same path either way.
func TestAReadGoesToThePathJoinedOntoTheConfiguredAPI(t *testing.T) {
	widgets := ghrepo.MustParse("acme/widgets")
	for _, c := range []struct {
		name string
		// body is what the far side answers, in the shape the call's own destination reads.
		body string
		// call is one read of the client, the path it is given below.
		call func(ctx context.Context, client Client) error
		// path and query are what the far side must be asked, the base's path before path.
		path, query string
	}{
		{
			name: "get", body: `{}`,
			call: func(ctx context.Context, client Client) error {
				var into map[string]string
				return client.Get(ctx, "/pulls/7", "", &into)
			},
			path: "/repos/acme/widgets/pulls/7",
		},
		{
			name: "post", body: `{}`,
			call: func(ctx context.Context, client Client) error {
				return client.Post(ctx, "/issues/7/comments", "", map[string]string{"body": "a word"}, nil)
			},
			path: "/repos/acme/widgets/issues/7/comments",
		},
		{
			name: "pages", body: `[]`,
			call: func(ctx context.Context, client Client) error {
				_, err := GetPages[map[string]string](ctx, client, "/pulls", "")
				return err
			},
			path: "/repos/acme/widgets/pulls", query: "per_page=100",
		},
		{
			name: "pages with the read's own query", body: `[]`,
			call: func(ctx context.Context, client Client) error {
				_, err := GetPages[map[string]string](ctx, client, "/pulls?state=open", "")
				return err
			},
			path: "/repos/acme/widgets/pulls", query: "state=open&per_page=100",
		},
	} {
		for _, base := range []struct {
			name, path, query string
		}{
			{name: "query", path: "/api/v3", query: "key=a-route-key"},
			{name: "twin", path: "/api/v3", query: ""},
		} {
			t.Run(c.name+"/"+base.name, func(t *testing.T) {
				reached := &asked{}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					reached.record(r)
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(c.body))
				}))
				defer server.Close()
				endpoint := server.URL + base.path
				if base.query != "" {
					endpoint += "?" + base.query
				}
				client := Client{Token: "not-a-secret-token", API: RepositoryAPI(endpoint, widgets)}
				if err := c.call(context.Background(), client); err != nil {
					t.Fatalf("the read failed: %v", err)
				}
				path, query := reached.read()
				wantPath := base.path + c.path
				wantQuery := c.query
				if base.query != "" {
					wantQuery = strings.TrimSuffix(base.query+"&"+c.query, "&")
				}
				if path != wantPath || query != wantQuery {
					t.Errorf("the read reached %q with the query %q; want %q with %q: the read's path joined onto the address's, the configured query kept",
						path, query, wantPath, wantQuery)
				}
			})
		}
	}
}

// TestAReadUnderAnAPIAddressThatIsNoURLRefusesWithoutPrintingIt: an API address a deployment
// configured that is no absolute URL with a host cannot be joined with a read's path, and every
// read under it is refused in the join's own fixed words (urljoin.ErrBase), which print neither
// the address nor url.Parse's error - that error quotes the URL whole, the user information a
// configured address can carry and all. Capture githubrest.api-address/<case>.
func TestAReadUnderAnAPIAddressThatIsNoURLRefusesWithoutPrintingIt(t *testing.T) {
	// A host with a space in it: url.Parse refuses it, and its error quotes the whole URL.
	const endpoint = "https://not-a-secret-user:not-a-secret-password@ho st/api?key=not-a-secret-token"
	widgets := ghrepo.MustParse("acme/widgets")
	t.Run("no-base", func(t *testing.T) {
		if api := RepositoryAPI(endpoint, widgets); api != "" {
			t.Errorf("RepositoryAPI of an address that is no URL = %q, want no REST base at all", api)
		}
	})
	for _, c := range []struct {
		name string
		call func(ctx context.Context, client Client) error
	}{
		{"get", func(ctx context.Context, client Client) error {
			var into map[string]string
			return client.Get(ctx, "/pulls/7", "", &into)
		}},
		{"post", func(ctx context.Context, client Client) error {
			return client.Post(ctx, "/issues/7/comments", "", map[string]string{"body": "a word"}, nil)
		}},
		{"pages", func(ctx context.Context, client Client) error {
			_, err := GetPages[map[string]string](ctx, client, "/pulls", "")
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			// The client is built as every caller builds it, from the address the deployment
			// configured (RepositoryAPI), and the read is refused before any request is made.
			err := c.call(context.Background(), Client{Token: "token", API: RepositoryAPI(endpoint, widgets)})
			if err == nil {
				t.Fatal("the read was made, want it refused: the configured address is no URL")
			}
			said := err.Error()
			for _, secret := range []string{"not-a-secret-user", "not-a-secret-password", "not-a-secret-token"} {
				oauth2calltest.RequireNoPiece(t, "githubrest.api-address/"+c.name, said, secret)
			}
			if !strings.Contains(said, "is not an absolute URL with a host") {
				t.Errorf("the refusal is %q, want it to name the address as no absolute URL with a host", said)
			}
		})
	}
}
