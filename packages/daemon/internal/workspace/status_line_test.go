package workspace

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/envoy/redirects/redirectstest"
	"github.com/sjawhar/legion/daemon/internal/clock"
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
// refused (tar-refused), an answer whose repository header itself quotes that URL (repository),
// and a redirect whose Location carries made-up credentials in its user information and its
// query (redirectstest.MovedTo): of the tar, naming no repository, to a stand-in that answers 500
// (tar-redirected, the reviewer's probe); of the health, naming the cache's repository, to that
// stand-in (health-redirected); and of the health to an address nothing listens at
// (health-redirect-unanswered), whose transport failure net/http names by the Location. Each
// line names the status by its code and the standard phrase (oauth2call.StatusLine) and the
// header as the rule reads it (oauth2call.Header), and none of what the cache's stand-in wrote
// there; a redirect is refused by its status, and the stand-in it names is asked nothing.
// Captures workspace.status-line/<case>.
func TestACacheAnswerNamesItsStatusAndRepositoryAsTheRuleReadsThem(t *testing.T) {
	repo := ghrepo.MustParse("acme/widgets")
	named := CacheRepositoryHeader + ": " + repo.String() + "\r\n"
	request := func(base string) FetchRequest {
		return FetchRequest{Repo: repo, CacheURL: base, CacheToken: "cache-token", Clock: clock.Real()}
	}
	// moved is where a redirect below sends the pod: it answers 500 naming the cache's repository,
	// and counts what it is asked, which unfollowed reads. unanswered is an address nothing listens at.
	var movedAsked atomic.Int64
	moved := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		movedAsked.Add(1)
		w.Header().Set(CacheRepositoryHeader, repo.String())
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer moved.Close()
	unanswered := httptest.NewServer(http.NotFoundHandler())
	unanswered.Close()
	unfollowed := func(t *testing.T, said string) string {
		t.Helper()
		if asked := movedAsked.Swap(0); asked != 0 {
			t.Errorf("the redirect's target was asked %d times, want none", asked)
		}
		return said
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
		{"tar-refused", named, "/tar answered 500 Internal Server Error", http.StatusInternalServerError, unpackedError},
		{"tar-redirected", redirectstest.MovedTo(moved.Listener.Addr().String()), "/tar answered 302 Found", http.StatusFound, func(t *testing.T, r FetchRequest) string {
			return unfollowed(t, unpackedError(t, r))
		}},
		{"health-redirected", named + redirectstest.MovedTo(moved.Listener.Addr().String()), "/healthz answered 302 Found", http.StatusFound, func(t *testing.T, r FetchRequest) string {
			return unfollowed(t, healthRefusal(t, r))
		}},
		{"health-redirect-unanswered", redirectstest.MovedTo(unanswered.Listener.Addr().String()), "/healthz answered 302 Found", http.StatusFound, healthRefusal},
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

// unpackedError is unpackCache's error for the tar.
func unpackedError(t *testing.T, request FetchRequest) string {
	t.Helper()
	_, err := unpackCache(context.Background(), initOnly{}, request, filepath.Join(t.TempDir(), "staging"))
	if err == nil {
		t.Fatal("unpackCache answered no error")
	}
	return err.Error()
}

// healthRefusal is cacheNotReady's refusal of the health, which fails the wait at once; an answer
// it reads as not ready instead fails t, and its reason is the line.
func healthRefusal(t *testing.T, request FetchRequest) string {
	t.Helper()
	why, err := cacheNotReady(context.Background(), request, time.Minute)
	if err == nil {
		t.Errorf("cacheNotReady refused nothing and waits: %s", why)
		return why
	}
	return err.Error()
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
