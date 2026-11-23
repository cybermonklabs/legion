package sandbox

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// TestACacheHealthNamesItsStatusAndRepositoryAsTheRuleReadsThem: a repository cache's pod, or
// whatever answers at its address, answers the daemon's health read, through the client the
// daemon reads it with (cacheHealthClient), under a reason phrase that quotes a credential's URL:
// with a repository header that itself quotes it (repository), with a body that is not the
// cache's health (body), and with a redirect whose Location carries made-up credentials in its
// user information and its query (movedTo): naming the cache's repository and carrying a healthy
// cache's health, to a stand-in that answers one too (redirected), and naming none, to an address
// nothing listens at (redirect-unanswered), whose transport failure net/http names by the
// Location. The error names the status by its code and the standard phrase
// (oauth2call.StatusLine) and the header as the rule reads it (oauth2call.Header), and none of
// what the cache's stand-in wrote there; a redirect is the read's failure, whatever it carries,
// and the stand-in it names is asked nothing. Captures sandbox.status-line/<case>.
func TestACacheHealthNamesItsStatusAndRepositoryAsTheRuleReadsThem(t *testing.T) {
	repo := ghrepo.MustParse("acme/widgets")
	named := workspace.CacheRepositoryHeader + ": " + repo.String() + "\r\n"
	const healthy = `{"ready":true,"repo":"acme/widgets","bytes":1}`
	// moved is where a redirect below sends the read: it answers as a healthy cache of repo would,
	// and counts what it is asked. unanswered is an address nothing listens at.
	var movedAsked atomic.Int64
	moved := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		movedAsked.Add(1)
		w.Header().Set(workspace.CacheRepositoryHeader, repo.String())
		_, _ = w.Write([]byte(healthy))
	}))
	defer moved.Close()
	unanswered := httptest.NewServer(http.NotFoundHandler())
	unanswered.Close()
	client := (&Runtime{cacheDial: (&net.Dialer{}).DialContext}).cacheHealthClient()
	for _, c := range []struct {
		name, header, body, want string
		status                   int
	}{
		{"repository", workspace.CacheRepositoryHeader + ": " + oauth2calltest.Reason + "\r\n", "{}", "answered 200 OK naming the repository \"see https://issuer.example/code?access_token=\"", http.StatusOK},
		{"body", named, "not the health", "answered 200 OK with a body that is not the cache's health", http.StatusOK},
		{"redirected", named + movedTo(moved.Listener.Addr().String()), healthy, "/healthz answered 302 Found", http.StatusFound},
		{"redirect-unanswered", movedTo(unanswered.Listener.Addr().String()), "", "/healthz answered 302 Found", http.StatusFound},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := cacheHealthOf(context.Background(), client, oauth2calltest.StatusLineServer(t, c.status, c.header, c.body), repo)
			if asked := movedAsked.Swap(0); asked != 0 {
				t.Errorf("the redirect's target was asked %d times, want none", asked)
			}
			if err == nil {
				t.Fatal("cacheHealthOf answered no error")
			}
			oauth2calltest.RequireNone(t, "sandbox.status-line/"+c.name, err.Error())
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the error is %q, want %q in it", err, c.want)
			}
		})
	}
}

// movedTo is a Location header line sending the read to host, carrying the made-up credentials
// of oauth2calltest's probe in its user information and its query.
func movedTo(host string) string {
	return "Location: http://not-a-secret-user:not-a-secret-password@" + host + "/moved?access_token=not-a-secret-token\r\n"
}
