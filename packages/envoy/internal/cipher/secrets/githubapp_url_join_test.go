package secrets_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/githubtest"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// gitHubAtAPath answers the listing and the token request wherever under its address they come,
// and records the path and the query of each call: the stand-in GitHub of a deployment whose
// BROKER_AIRLOCK_TEST_GITHUB_API_BASE carries a path.
func gitHubAtAPath(t *testing.T, now time.Time) (base string, asked *[]url.URL) {
	t.Helper()
	calls := new([]url.URL)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls = append(*calls, *r.URL)
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/app/installations"):
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 1, "account": map[string]string{"login": "ada"}, "permissions": listedHolding},
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens"):
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(readToken(githubtest.InstallationToken("1"), now.Add(time.Hour).Format(time.RFC3339)))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL, calls
}

// TestAGitHubAppMintGoesToThePathsJoinedOntoTheAPIBase: the GitHub API base is the deployment's
// (BROKER_AIRLOCK_TEST_GITHUB_API_BASE, which a local stack points at a stand-in GitHub), and it
// can carry a path, a query or a fragment. The listing and the token request each ask that URL's
// path with their own joined onto it, the base's query kept ahead of the listing's own page
// parameters: http://host/github?sig=... lists /github/app/installations with sig=... first and
// per_page=100&page=1 after (query), and so does http://host/github/?sig=...
// (query-trailing-slash), where the path appended to the URL's text sat inside the query
// (/github?sig=.../app/installations?per_page=100&page=1, GitHub asked /github with the query's
// value changed and a second "?" in it); http://host/github#pane asks /github/app/installations
// with no fragment, which no request sends (fragment). A base with neither is asked where it was
// (no-query).
func TestAGitHubAppMintGoesToThePathsJoinedOntoTheAPIBase(t *testing.T) {
	key, err := testAppKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct{ name, suffix, listQuery, tokenQuery string }{
		{"query", "/github?sig=s1g-7Qz4k", "sig=s1g-7Qz4k&per_page=100&page=1", "sig=s1g-7Qz4k"},
		{"query-trailing-slash", "/github/?sig=s1g-7Qz4k", "sig=s1g-7Qz4k&per_page=100&page=1", "sig=s1g-7Qz4k"},
		{"fragment", "/github#pane", "per_page=100&page=1", ""},
		{"no-query", "/github", "per_page=100&page=1", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			base, asked := gitHubAtAPath(t, now)
			app := secrets.GitHubApp{Base: base + c.suffix, HTTP: http.DefaultClient, Now: func() time.Time { return now }}
			if _, err := app.Mint(t.Context(), mintFor(githubAppValue(t, "123456", key), "ada")); err != nil {
				t.Fatalf("Mint: %v", err)
			}
			if len(*asked) != 2 {
				t.Fatalf("%d calls reached GitHub, want the listing and the token request", len(*asked))
			}
			list, token := (*asked)[0], (*asked)[1]
			if list.Path != "/github/app/installations" || list.RawQuery != c.listQuery {
				t.Errorf("a base ending %q listed path %q, query %q; want %q, %q", c.suffix, list.Path, list.RawQuery, "/github/app/installations", c.listQuery)
			}
			if want := "/github/app/installations/1/access_tokens"; token.Path != want || token.RawQuery != c.tokenQuery {
				t.Errorf("a base ending %q asked for a token at path %q, query %q; want %q, %q", c.suffix, token.Path, token.RawQuery, want, c.tokenQuery)
			}
		})
	}
}

// TestAGitHubAPIBaseThatDoesNotParsePrintsNoneOfIt: a BROKER_AIRLOCK_TEST_GITHUB_API_BASE with
// redacttest's credentials in its user information and its query that url.Parse refuses (an
// invalid escape in its path) fails the mint in fixed words naming the setting, with none of the
// URL (unparsed); one url.Parse takes whose joined URL url.URL.String writes as text url.Parse
// refuses again (an IPv6 zone escape) fails it in fixed words too (reparsed). The twins show
// net/http's own error for each quotes the credentials, so the rows can see one printed. Captures
// secrets.github-api-base/<case>.
func TestAGitHubAPIBaseThatDoesNotParsePrintsNoneOfIt(t *testing.T) {
	key, err := testAppKey()
	if err != nil {
		t.Fatal(err)
	}
	const route = "/app/installations"
	userinfo := redacttest.User + ":" + redacttest.Password + "@"
	param := "access" + "_token"
	unparsed := "http://" + userinfo + "127.0.0.1:1/github%zz?" + param + "=" + redacttest.Token
	reparsed := "http://" + userinfo + "[::%25\xfd]/github?" + param + "=" + redacttest.Token
	mint := func(t *testing.T, base string) string {
		t.Helper()
		retry, _, _ := testRetry(atTheBottom)
		app := secrets.GitHubApp{Base: base, HTTP: http.DefaultClient, Retry: retry}
		minted, err := app.Mint(t.Context(), mintFor(githubAppValue(t, "123456", key), "ada"))
		if err == nil {
			t.Fatalf("Mint released %q at a GitHub API base that does not parse", minted.Value)
		}
		return err.Error()
	}
	t.Run("twin", func(t *testing.T) {
		if _, err := http.NewRequest(http.MethodGet, unparsed+route, nil); err == nil || len(redacttest.Held(err.Error())) != len(redacttest.Secrets) {
			t.Errorf("http.NewRequest = %v; want an error that quotes the credentials, so the row can see them", err)
		}
	})
	t.Run("unparsed", func(t *testing.T) {
		said := mint(t, unparsed)
		redacttest.RequireNoPiece(t, "secrets.github-api-base/unparsed", said)
		if !strings.Contains(said, "BROKER_AIRLOCK_TEST_GITHUB_API_BASE is not an absolute URL with a host") {
			t.Errorf("Mint = %q; want it to name the setting", said)
		}
	})
	t.Run("reparsed-twin", func(t *testing.T) {
		parsed, err := url.Parse(reparsed)
		if err != nil {
			t.Fatalf("url.Parse refused the GitHub API base (%v); the row needs one it takes", err)
		}
		if _, err := http.NewRequest(http.MethodGet, parsed.JoinPath(route).String(), nil); err == nil || len(redacttest.Held(err.Error())) != len(redacttest.Secrets) {
			t.Errorf("http.NewRequest of the joined URL = %v; want an error that quotes the credentials, so the row can see them", err)
		}
	})
	t.Run("reparsed", func(t *testing.T) {
		said := mint(t, reparsed)
		redacttest.RequireNoPiece(t, "secrets.github-api-base/reparsed", said)
		if !strings.Contains(said, "the URL joined from BROKER_AIRLOCK_TEST_GITHUB_API_BASE does not parse") {
			t.Errorf("Mint = %q; want it to name the setting", said)
		}
	})
}
