package workspace

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/clock"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// TestACacheAskGoesToThePathJoinedOntoTheCacheURL: --cache-url is what the pod is told, and it can
// carry a path, a query or a fragment. Each ask of the cache goes to the URL's path with the ask's
// path joined onto it, the query kept and the fragment dropped: http://host/cache?sig=... is asked
// /cache/healthz?sig=... (query), and so is http://host/cache/?sig=... (query-trailing-slash), where
// /healthz appended to the URL's text sat inside the query (/cache?sig=.../healthz, the cache asked
// /cache with the query's value changed); http://host/cache#... is asked /cache/healthz (fragment),
// where /healthz sat inside the fragment and the cache was asked /cache. A URL with neither is
// asked /cache/healthz, as it was (no-query). A stand-in cache records the path and the query each
// ask reaches it with.
func TestACacheAskGoesToThePathJoinedOntoTheCacheURL(t *testing.T) {
	repo := ghrepo.MustParse("acme/widgets")
	for _, c := range []struct{ name, suffix, path, query string }{
		{"query", "/cache?sig=s1g-7Qz4k", "/cache/healthz", "sig=s1g-7Qz4k"},
		{"query-trailing-slash", "/cache/?sig=s1g-7Qz4k", "/cache/healthz", "sig=s1g-7Qz4k"},
		{"fragment", "/cache#pane", "/cache/healthz", ""},
		{"no-query", "/cache", "/cache/healthz", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			asked := make(chan url.URL, 1)
			cache := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				asked <- *request.URL
				w.Header().Set(CacheRepositoryHeader, repo.String())
				w.WriteHeader(http.StatusOK)
			}))
			defer cache.Close()
			response, err := askCache(context.Background(), FetchRequest{Repo: repo, CacheURL: cache.URL + c.suffix, CacheToken: "cache-token", Clock: clock.Real()}, "/healthz", false)
			if response != nil {
				_ = response.Body.Close()
			}
			select {
			case got := <-asked:
				if got.Path != c.path || got.RawQuery != c.query {
					t.Errorf("a cache URL ending %q was asked path %q, query %q; want path %q, query %q", c.suffix, got.Path, got.RawQuery, c.path, c.query)
				}
			default:
				t.Fatalf("no ask reached the cache; askCache answered %v", err)
			}
			if err != nil {
				t.Errorf("askCache = %v; want the cache's 200 taken", err)
			}
		})
	}
}

// TestACacheURLThatDoesNotParsePrintsNoneOfIt: a cache URL url.Parse refuses (an invalid escape in
// its path), with made-up credentials in its user information and its query, fails the ask in
// askCache's own words, which hold none of the URL: in unpackCache's error (unparsed-tar) and in
// cacheNotReady's reason the wait logs and fails in (unparsed-health). http.NewRequest's error,
// which askCache returned, quotes the URL whole (twin). A cache URL that parses and whose joined
// URL, as url.URL.String writes it, url.Parse refuses (an IPv6 zone holding a byte that is not
// ASCII, which String escapes as %FD) is refused the same way (reparsed); the twin shows
// http.NewRequest's error for that joined URL holds the credentials. Captures
// workspace.cache-url/<case>.
func TestACacheURLThatDoesNotParsePrintsNoneOfIt(t *testing.T) {
	const (
		user     = "not-a-secret-user"
		password = "not-a-secret-password"
		token    = "c8q2v-not-a-secret-token-4h7m"
	)
	repo := ghrepo.MustParse("acme/widgets")
	unparsed := "http://" + user + ":" + password + "@127.0.0.1:1/cache%zz?sig=" + token
	reparsed := "http://" + user + ":" + password + "@[::%25\xfd]/cache?sig=" + token
	request := func(cacheURL string) FetchRequest {
		return FetchRequest{Repo: repo, CacheURL: cacheURL, CacheToken: "cache-token", Clock: clock.Real()}
	}
	holds := func(t *testing.T, text string) {
		t.Helper()
		for _, credential := range []string{user, password, token} {
			if !strings.Contains(text, credential) {
				t.Fatalf("%q holds no %q, so the row cannot see a leak", text, credential)
			}
		}
	}
	t.Run("twin", func(t *testing.T) {
		_, err := http.NewRequest(http.MethodGet, unparsed+"/tar", nil)
		if err == nil {
			t.Fatalf("http.NewRequest took %q; the row needs a URL that does not parse", unparsed)
		}
		holds(t, err.Error())
	})
	t.Run("unparsed-tar", func(t *testing.T) {
		said := unpackedError(t, request(unparsed))
		oauth2calltest.RequireNoPiece(t, "workspace.cache-url/unparsed-tar", said, token)
		if want := "ask the repository cache: its URL does not parse (the URL is not printed)"; said != want {
			t.Errorf("unpackCache's error is %q, want %q", said, want)
		}
	})
	t.Run("unparsed-health", func(t *testing.T) {
		why, err := cacheNotReady(context.Background(), request(unparsed), time.Minute)
		if err != nil {
			t.Fatalf("cacheNotReady refused %v; want the ask's failure as its reason", err)
		}
		oauth2calltest.RequireNoPiece(t, "workspace.cache-url/unparsed-health", why, token)
		if want := "ask the repository cache: its URL does not parse (the URL is not printed)"; why != want {
			t.Errorf("cacheNotReady's reason is %q, want %q", why, want)
		}
	})
	t.Run("reparsed-twin", func(t *testing.T) {
		parsed, err := url.Parse(reparsed)
		if err != nil {
			t.Fatalf("url.Parse refused %q (%v); the row needs a cache URL it takes", reparsed, err)
		}
		_, err = http.NewRequest(http.MethodGet, parsed.JoinPath("/tar").String(), nil)
		if err == nil {
			t.Fatal("http.NewRequest took the joined URL; the row needs one it refuses")
		}
		holds(t, err.Error())
	})
	t.Run("reparsed", func(t *testing.T) {
		said := unpackedError(t, request(reparsed))
		oauth2calltest.RequireNoPiece(t, "workspace.cache-url/reparsed", said, token)
		if want := "ask the repository cache: the URL of its /tar does not parse (the URL is not printed)"; said != want {
			t.Errorf("unpackCache's error is %q, want %q", said, want)
		}
	})
}
