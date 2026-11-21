package workspace

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// initOnly is a Runner whose every command succeeds and prints nothing: unpackCache's git init,
// which the rows below need only to get to the ask of the cache.
type initOnly struct{}

func (initOnly) Timeout() time.Duration                       { return time.Minute }
func (initOnly) Run(context.Context, Command) (Result, error) { return Result{}, nil }

// TestACacheAnswerNamesItsStatusAndRepositoryAsTheRuleReadsThem: the repository cache, or
// whatever answers in its place, answers a pod's asks under a reason phrase that quotes a
// credential's URL: its health not ready (not-ready), the token refused (token-refused), the tar
// refused (tar-refused), and an answer whose repository header itself quotes that URL
// (repository). Each line names the status by its code and the standard phrase
// (oauth2call.StatusLine) and the header as the rule reads it (oauth2call.Header), and none of what
// the cache's stand-in wrote there. Captures workspace.status-line/<case>.
func TestACacheAnswerNamesItsStatusAndRepositoryAsTheRuleReadsThem(t *testing.T) {
	repo := ghrepo.MustParse("acme/widgets")
	named := CacheRepositoryHeader + ": " + repo.String() + "\r\n"
	request := func(base string) FetchRequest {
		return FetchRequest{Repo: repo, CacheURL: base, CacheToken: "cache-token", Clock: WallClock{}}
	}
	for _, c := range []struct {
		name, header, want string
		status             int
		call               func(*testing.T, FetchRequest) string
	}{
		{"not-ready", named, "/healthz answered 503 Service Unavailable", http.StatusServiceUnavailable, func(t *testing.T, r FetchRequest) string {
			why, err := cacheNotReady(context.Background(), r, time.Minute)
			if err != nil {
				t.Fatalf("cacheNotReady: %v", err)
			}
			return why
		}},
		{"token-refused", named, "answered 401 Unauthorized", http.StatusUnauthorized, func(t *testing.T, r FetchRequest) string {
			return askedError(t, r, true)
		}},
		{"repository", CacheRepositoryHeader + ": " + oauth2calltest.Reason + "\r\n", "is the repository cache of see https://issuer.example/code?access_token=, not of", http.StatusOK, func(t *testing.T, r FetchRequest) string {
			return askedError(t, r, false)
		}},
		{"tar-refused", named, "/tar answered 500 Internal Server Error", http.StatusInternalServerError, func(t *testing.T, r FetchRequest) string {
			_, err := unpackCache(context.Background(), initOnly{}, r, filepath.Join(t.TempDir(), "staging"))
			if err == nil {
				t.Fatal("unpackCache answered no error")
			}
			return err.Error()
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			said := c.call(t, request(oauth2calltest.StatusLineServer(t, c.status, c.header, "")))
			oauth2calltest.RequireNone(t, "workspace.status-line/"+c.name, said)
			if !strings.Contains(said, c.want) {
				t.Errorf("the line is %q, want %q in it", said, c.want)
			}
		})
	}
}

// askedError is askCache's error for /healthz, asked with the token or without it.
func askedError(t *testing.T, request FetchRequest, withToken bool) string {
	t.Helper()
	response, err := askCache(context.Background(), request, "/healthz", withToken)
	if response != nil {
		_ = response.Body.Close()
	}
	if err == nil {
		t.Fatal("askCache answered no error")
	}
	return err.Error()
}
