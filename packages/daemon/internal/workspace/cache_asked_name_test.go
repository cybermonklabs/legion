package workspace

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/clock"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/redact"
)

// TestACacheLineNamesTheURLItAsked: the cache URL carries a path, made-up user information and a
// query whose value is a made-up credential. A line about an ask the cache answered names the URL
// askCache joined as Unanswered names the URL a request asked (askedCache: its scheme, host and
// path, the route included, and the authority of a URL with user information, as this one has, as
// the mark of a URL with credentials), where it wrote the cache URL's text with the route after
// it: the route sat inside the query, so the line held the credential (twin), and the URL rule,
// which removes a query, removed the route with it (twin-rule). The health answers 503
// (health-status), a body that is not its health (health-body), not ready (health-not-ready) and
// ready without defaultBranchOnly, as a cache of an earlier release that serves every branch does
// (health-wide); the tar answers 500 (tar-status). The control: each line still names the answer
// and the route. Captures workspace.cache-asked/<case>.
func TestACacheLineNamesTheURLItAsked(t *testing.T) {
	const (
		user     = "not-a-secret-user"
		password = "not-a-secret-password"
		token    = "c8q2v-not-a-secret-token-4h7m"
	)
	repo := ghrepo.MustParse("acme/widgets")
	var health func(w http.ResponseWriter)
	cache := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(CacheRepositoryHeader, repo.String())
		switch r.URL.Path {
		case "/cache/healthz":
			health(w)
		case "/cache/tar":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer cache.Close()
	host := strings.TrimPrefix(cache.URL, "http://")
	request := CacheStream{
		Repo:  repo,
		URL:   "http://" + user + ":" + password + "@" + host + "/cache?sig=" + token,
		Token: "cache-token",
		Clock: clock.Real(),
	}
	asText := strings.TrimSuffix(request.URL, "/") + "/healthz"
	t.Run("twin", func(t *testing.T) {
		if !strings.Contains(asText, token) {
			t.Fatalf("the cache URL's text with the route after it is %q; want the credential in it, so the row can see it printed", asText)
		}
	})
	t.Run("twin-rule", func(t *testing.T) {
		if read := redact.URLCredentials("GET " + asText + " answered"); strings.Contains(read, "/healthz") {
			t.Fatalf("the URL rule reads that text as %q; want the route removed with the query, so the row can see it named", read)
		}
	})
	notReady := func(t *testing.T, answer func(w http.ResponseWriter)) string {
		t.Helper()
		health = answer
		why, err := cacheNotReady(context.Background(), request, time.Minute)
		if err != nil || why == "" {
			t.Fatalf("cacheNotReady = %q, %v; want the health's answer as the reason it waits", why, err)
		}
		return why
	}
	const asked = "GET http://[a URL with credentials]/cache"
	for _, c := range []struct {
		name, want string
		said       func(t *testing.T) string
	}{
		{"health-status", asked + "/healthz answered 503 Service Unavailable", func(t *testing.T) string {
			return notReady(t, func(w http.ResponseWriter) { w.WriteHeader(http.StatusServiceUnavailable) })
		}},
		{"health-body", asked + "/healthz answered a body that is not the cache's health", func(t *testing.T) string {
			return notReady(t, func(w http.ResponseWriter) { _, _ = w.Write([]byte("not the health")) })
		}},
		{"health-not-ready", asked + "/healthz answered not ready", func(t *testing.T) string {
			return notReady(t, func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"ready":false}`)) })
		}},
		{"health-wide", asked + "/healthz answered ready without defaultBranchOnly", func(t *testing.T) string {
			return notReady(t, func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"ready":true}`)) })
		}},
		{"tar-status", asked + "/tar answered 500 Internal Server Error", func(t *testing.T) string {
			return unpackedError(t, request)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			said := c.said(t)
			oauth2calltest.RequireNoPiece(t, "workspace.cache-asked/"+c.name, said, token)
			if !strings.Contains(said, c.want) {
				t.Errorf("the line is %q, want %q in it", said, c.want)
			}
		})
	}
}
