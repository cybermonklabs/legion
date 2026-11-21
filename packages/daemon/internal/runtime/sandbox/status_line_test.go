package sandbox

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// TestACacheHealthNamesItsStatusAndRepositoryAsTheRuleReadsThem: a repository cache's pod, or
// whatever answers at its address, answers the daemon's health read under a reason phrase that
// quotes a credential's URL: with a repository header that itself quotes it (repository), and
// with a body that is not the cache's health (body). The error names the status by its code and
// the standard phrase (oauth2call.StatusLine) and the header as the rule reads it
// (oauth2call.Header), and none of what the cache's stand-in wrote there. Captures
// sandbox.status-line/<case>.
func TestACacheHealthNamesItsStatusAndRepositoryAsTheRuleReadsThem(t *testing.T) {
	repo := ghrepo.MustParse("acme/widgets")
	for _, c := range []struct {
		name, header, body, want string
	}{
		{"repository", workspace.CacheRepositoryHeader + ": " + oauth2calltest.Reason + "\r\n", "{}", "answered 200 OK naming the repository \"see https://issuer.example/code?access_token=\""},
		{"body", workspace.CacheRepositoryHeader + ": " + repo.String() + "\r\n", "not the health", "answered 200 OK with a body that is not the cache's health"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := cacheHealthOf(context.Background(), &http.Client{}, oauth2calltest.StatusLineServer(t, http.StatusOK, c.header, c.body), repo)
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
