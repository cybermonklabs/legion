package workspace

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/envoy/redirects/redirectstest"
	"github.com/sjawhar/legion/daemon/internal/clock"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// TestACacheAskThatGotNoAnswerNamesTheCacheByItsSchemeHostAndPath: --cache-url is what the pod is
// told, and it can carry a fragment, which net/http's own error of the URL's text quotes. The tar's
// ask of a cache that never answers (its address is one nothing listens at) fails in unpackCache's
// error with the failure named through oauth2call.Unanswered, by the URL the ask asked: its scheme,
// host and path alone (the cache's path with /tar joined onto it, the fragment dropped), none of the
// fragment. The twin shows net/http's own error of the URL's text with /tar appended holds the
// credential. Captures workspace.unanswered-name/fragment.
func TestACacheAskThatGotNoAnswerNamesTheCacheByItsSchemeHostAndPath(t *testing.T) {
	const token = "w3kd8-not-a-secret-token-5b0f"
	dead := redirectstest.Dead(t)
	cacheURL := "http://" + dead + "#" + token
	t.Run("fragment", func(t *testing.T) {
		response, err := http.Get(cacheURL + "/tar")
		if err == nil {
			_ = response.Body.Close()
			t.Fatalf("%s answered %d: the row needs a cache that never answers", dead, response.StatusCode)
		}
		if !strings.Contains(err.Error(), token) {
			t.Fatalf("net/http's own error holds no %q, so the row cannot see a leak: %q", token, err)
		}
		request := FetchRequest{Repo: ghrepo.MustParse("acme/widgets"), CacheURL: cacheURL, CacheToken: "cache-token", Clock: clock.Real()}
		_, err = unpackCache(context.Background(), initOnly{}, request, filepath.Join(t.TempDir(), "staging"))
		if err == nil {
			t.Fatal("unpackCache answered no error")
		}
		oauth2calltest.RequireNoPiece(t, "workspace.unanswered-name/fragment", err.Error(), token)
		if want := `Get "http://` + dead + `/tar": dial tcp ` + dead + ": "; !strings.HasPrefix(err.Error(), want) {
			t.Errorf("unpackCache's error is %q, want it to begin %q", err, want)
		}
	})
}
