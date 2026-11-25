package appauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// askedCalls keeps the path and the query of every call a Manager made.
type askedCalls struct {
	mu    sync.Mutex
	paths []string
	query []string
}

func (a *askedCalls) record(r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.paths = append(a.paths, r.URL.Path)
	a.query = append(a.query, r.URL.RawQuery)
}

// first is the path and the query of the first call, and whether there was one.
func (a *askedCalls) first() (string, string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.paths) == 0 {
		return "", "", false
	}
	return a.paths[0], a.query[0], true
}

// TestACallGoesToThePathJoinedOntoTheConfiguredBaseURL: the GitHub API base URL a deployment
// configures (Options.BaseURL) may carry a path and a query of its own - a gateway's route key,
// say. Every call the Manager makes joins its own path onto that URL's path and keeps the
// configured query ahead of its own (newRequest, urljoin.Join), so GitHub is asked
// <base path>/<call's path> with that query. The call's path appended to the base's text would
// sit inside the query's last value, and GitHub would be asked the base's own path instead.
//
// A row per call the Manager makes: the App identity read, the bot user read, the installation
// discovery (whose own page query follows the configured one), the token exchange, the
// installation read of a repository, the grant read, token mode's GraphQL viewer read, its App
// read and its listing of the installation's repositories. The twin of each is the same call
// under a base with no query, which reaches the same path either way.
func TestACallGoesToThePathJoinedOntoTheConfiguredBaseURL(t *testing.T) {
	for _, c := range []struct {
		name string
		// tokenMode makes the App a token_command's, whose calls are the second half below.
		tokenMode bool
		// call is the one call of the Manager the row is of.
		call func(ctx context.Context, m *Manager) error
		// path and query are what GitHub must be asked, under the base's own path.
		path, query string
	}{
		{
			name: "app-slug",
			call: func(ctx context.Context, m *Manager) error { _, err := m.AppLogin(ctx, Review); return err },
			path: "/app",
		},
		{
			name: "installed-on",
			call: func(ctx context.Context, m *Manager) error {
				_, err := m.InstalledOn(ctx, Review, ghrepo.MustParse("acme/widgets"))
				return err
			},
			path: "/repos/acme/widgets/installation",
		},
		{
			name: "installations",
			call: func(ctx context.Context, m *Manager) error { _, err := m.InstalledOwners(ctx, Review); return err },
			path: "/app/installations", query: "page=1&per_page=100",
		},
		{
			name: "mint",
			call: func(ctx context.Context, m *Manager) error { _, err := m.Token(ctx, Review, "acme"); return err },
			path: "/app/installations", query: "page=1&per_page=100",
		},
		{
			name: "viewer", tokenMode: true,
			call: func(ctx context.Context, m *Manager) error { _, err := m.AppLogin(ctx, Review); return err },
			path: "/graphql",
		},
	} {
		for _, base := range []struct {
			name, path, query string
		}{
			{name: "query", path: "/api/v3", query: "key=a-route-key"},
			{name: "twin", path: "/api/v3", query: ""},
		} {
			t.Run(c.name+"/"+base.name, func(t *testing.T) {
				asked := &askedCalls{}
				server := joinServer(t, asked, base.path)
				defer server.Close()
				endpoint := server.URL + base.path
				if base.query != "" {
					endpoint += "?" + base.query
				}
				// Every call here is let fail or pass as the stand-in answers it: the row reads
				// the path GitHub was asked, never the call's own answer.
				_ = c.call(context.Background(), joinManager(t, c.tokenMode, endpoint, server))
				path, query, made := asked.first()
				if !made {
					t.Fatal("GitHub was asked nothing at all")
				}
				wantPath, wantQuery := base.path+c.path, c.query
				if base.query != "" {
					wantQuery = strings.TrimSuffix(base.query+"&"+c.query, "&")
				}
				if path != wantPath || query != wantQuery {
					t.Errorf("the call reached %q with the query %q; want %q with %q: the call's path joined onto the base URL's, the configured query kept",
						path, query, wantPath, wantQuery)
				}
			})
		}
	}
}

// TestACallUnderABaseURLThatIsNoURLRefusesWithoutPrintingIt: a base URL a deployment configured
// that is no absolute URL with a host cannot be joined with a call's path, and every call is
// refused in the join's own fixed words (urljoin.ErrBase), which print neither the base nor
// url.Parse's error - that error quotes the URL whole, the user information a configured base can
// carry and all. Capture appauth.base-url/<case>.
func TestACallUnderABaseURLThatIsNoURLRefusesWithoutPrintingIt(t *testing.T) {
	// A host with a space in it: url.Parse refuses it, and its error quotes the whole URL.
	const endpoint = "https://not-a-secret-user:not-a-secret-password@ho st/api?key=not-a-secret-token"
	for _, c := range []struct {
		name      string
		tokenMode bool
		call      func(ctx context.Context, m *Manager) error
	}{
		{"app-slug", false, func(ctx context.Context, m *Manager) error { _, err := m.AppLogin(ctx, Review); return err }},
		{"installed-on", false, func(ctx context.Context, m *Manager) error {
			_, err := m.InstalledOn(ctx, Review, ghrepo.MustParse("acme/widgets"))
			return err
		}},
		{"installations", false, func(ctx context.Context, m *Manager) error {
			_, err := m.InstalledOwners(ctx, Review)
			return err
		}},
		{"viewer", true, func(ctx context.Context, m *Manager) error { _, err := m.AppLogin(ctx, Review); return err }},
	} {
		t.Run(c.name, func(t *testing.T) {
			asked := &askedCalls{}
			server := joinServer(t, asked, "")
			defer server.Close()
			err := c.call(context.Background(), joinManager(t, c.tokenMode, endpoint, server))
			if err == nil {
				t.Fatal("the call was made, want it refused: the configured base URL is no URL")
			}
			said := err.Error()
			for _, secret := range []string{"not-a-secret-user", "not-a-secret-password", "not-a-secret-token"} {
				oauth2calltest.RequireNoPiece(t, "appauth.base-url/"+c.name, said, secret)
			}
			if !strings.Contains(said, "is not an absolute URL with a host") {
				t.Errorf("the refusal is %q, want it to name the base URL as no absolute URL with a host", said)
			}
		})
	}
}

// joinNow is the moment the Managers of this file's rows read, so their App JWTs are the same
// from run to run.
var joinNow = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

// joinServer is a stand-in GitHub that records the path and the query of every call under prefix,
// the path the configured base URL carries, and answers each of the Manager's reads enough for
// the call to go on. A call to any other path is recorded too, which is what a row that read the
// wrong path sees.
func joinServer(t *testing.T, asked *askedCalls, prefix string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.record(r)
		w.Header().Set("Content-Type", "application/json")
		switch path := strings.TrimPrefix(r.URL.Path, prefix); {
		case path == "/app":
			_, _ = w.Write([]byte(`{"slug":"legion-reviewer"}`))
		case path == "/graphql":
			_, _ = w.Write([]byte(`{"data":{"viewer":{"login":"legion-reviewer[bot]"}}}`))
		case path == "/apps/legion-reviewer":
			_, _ = w.Write([]byte(`{"id":67890,"slug":"legion-reviewer"}`))
		case path == "/app/installations":
			_, _ = w.Write([]byte(`[{"id":77,"account":{"login":"acme"}}]`))
		case path == "/installation/repositories":
			_, _ = w.Write([]byte(`{"total_count":1,"repositories":[{"full_name":"acme/widgets"}]}`))
		case strings.HasSuffix(path, "/installation"):
			_, _ = w.Write([]byte(`{"id":77}`))
		case strings.HasPrefix(path, "/users/"):
			_, _ = w.Write([]byte(`{"id":271566631}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"no such route here"}`))
		}
	}))
}

// joinManager is a Manager reading GitHub at endpoint through server's client, in key mode or,
// where tokenMode says so, with a token_command releasing one token for every account.
func joinManager(t *testing.T, tokenMode bool, endpoint string, server *httptest.Server) *Manager {
	t.Helper()
	_, privatePEM := testAppKey(t)
	review := config.GitHubApp{AppID: "67890", PrivateKey: privatePEM}
	if tokenMode {
		review = config.GitHubApp{AppID: "67890", TokenCommand: `printf '{"installations":{"acme":{"installation_id":77,"token":"ghs_not-a-secret-token-row","expires_at":"2026-10-16T13:00:00Z"}}}'`}
	}
	return New(config.GitHubApps{
		Implement: config.GitHubApp{AppID: "12345", PrivateKey: privatePEM},
		Review:    review,
	}, Options{BaseURL: endpoint, HTTPClient: server.Client(), Now: func() time.Time { return joinNow }})
}
