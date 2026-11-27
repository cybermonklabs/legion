package workspace

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/envoy/redirects/redirectstest"
	"github.com/sjawhar/legion/daemon/internal/clock"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// TestACacheLocationThatDoesNotParseNamesNoRunOfTheCacheToken: whatever answers at the repository
// cache's address answers the pod's ask of the tar, which carries the cache token, with a redirect
// whose Location quotes that token and does not parse. Go parses a Location before a client's
// CheckRedirect is asked, so net/http's error for the ask quotes the whole Location whatever the
// client's redirect policy; unpackCacheInto's error names the ask's failure by its class and holds
// no run of the token. The twin shows the stand-in's echo reaches net/http's own error; plain
// shows an answer naming the cache's repository is still taken. Captures workspace.redirect/<case>.
func TestACacheLocationThatDoesNotParseNamesNoRunOfTheCacheToken(t *testing.T) {
	const token = "kq3vz-not-a-secret-RDX-5e1c"
	repo := ghrepo.MustParse("acme/widgets")
	t.Run("twin", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodGet, redirectstest.EchoBearer(t, redirectstest.Unparsed)+"/tar", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		redirectstest.RequireEcho(t, request, token)
	})
	t.Run("tar-unparsed", func(t *testing.T) {
		request := CacheStream{Repo: repo, URL: redirectstest.EchoBearer(t, redirectstest.Unparsed), Token: token, Clock: clock.Real()}
		_, err := unpackCacheInto(context.Background(), nil, request, filepath.Join(t.TempDir(), "staging"))
		if err == nil {
			t.Fatal("unpackCacheInto answered no error")
		}
		oauth2calltest.RequireNoPiece(t, "workspace.redirect/tar-unparsed", err.Error(), token)
		if strings.Contains(err.Error(), "moved") {
			t.Errorf("unpackCacheInto's error names the Location: %q", err)
		}
	})
	t.Run("plain", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set(CacheRepositoryHeader, repo.String())
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()
		response, err := askCache(context.Background(), CacheStream{Repo: repo, URL: server.URL, Token: token, Clock: clock.Real()}, "/healthz", true)
		if response != nil {
			_ = response.Body.Close()
		}
		if err != nil {
			t.Fatalf("a plain 200 naming the cache's repository = %v, want it taken", err)
		}
	})
}
