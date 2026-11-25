package leakguard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
)

// TestTheTarballReadGoesToThePathJoinedOntoTheAPIAddress: the GitHub API address a deployment
// configures for the check's repository (Source.API) may carry a path and a query of its own - a
// gateway's route key, say. The tarball read joins its own path onto that address's path and
// keeps the configured query (urljoin.Join over githubrest.RepositoryAPI), so GitHub is asked
// <api path>/repos/<owner>/<name>/tarball/<tag> with that query. The read's path appended to the
// address's text would sit inside the query's last value, and GitHub would be asked the
// address's own path instead. The twin is the same read under an address with no query, which
// reaches the same path either way.
func TestTheTarballReadGoesToThePathJoinedOntoTheAPIAddress(t *testing.T) {
	for _, base := range []struct {
		name, path, query string
	}{
		{name: "query", path: "/api/v3", query: "key=a-route-key"},
		{name: "twin", path: "/api/v3", query: ""},
	} {
		t.Run(base.name, func(t *testing.T) {
			var mu sync.Mutex
			var path, query string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				path, query = r.URL.Path, r.URL.RawQuery
				mu.Unlock()
				// The body is no tarball, so the read is refused: the row reads the path GitHub
				// was asked, never what it answered.
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"Not Found"}`))
			}))
			defer server.Close()
			api := server.URL + base.path
			if base.query != "" {
				api += "?" + base.query
			}
			source := &Source{Repository: checkRepository, API: api, Token: func(context.Context) (string, error) {
				return sourceToken, nil
			}}
			_, _ = source.Get(bound(t), theTag)
			mu.Lock()
			defer mu.Unlock()
			wantPath := base.path + "/repos/" + checkRepository.String() + "/tarball/" + theTag
			if path != wantPath || query != base.query {
				t.Errorf("the read reached %q with the query %q; want %q with %q: the read's path joined onto the address's, the configured query kept",
					path, query, wantPath, base.query)
			}
		})
	}
}

// TestATarballReadUnderAnAPIAddressThatIsNoURLRefusesWithoutPrintingIt: an API address a
// deployment configured that is no absolute URL with a host cannot be joined with the tarball's
// path, and the read is refused in the join's own fixed words (urljoin.ErrBase), which print
// neither the address nor url.Parse's error - that error quotes the URL whole, the user
// information a configured address can carry and all. Capture leakguard.api-address/tarball.
func TestATarballReadUnderAnAPIAddressThatIsNoURLRefusesWithoutPrintingIt(t *testing.T) {
	// A host with a space in it: url.Parse refuses it, and its error quotes the whole URL.
	const api = "https://not-a-secret-user:not-a-secret-password@ho st/api?key=not-a-secret-token"
	source := &Source{Repository: checkRepository, API: api, Token: func(context.Context) (string, error) {
		return sourceToken, nil
	}}
	_, err := source.Get(bound(t), theTag)
	if err == nil {
		t.Fatal("the read was made, want it refused: the configured address is no URL")
	}
	said := err.Error()
	for _, secret := range []string{"not-a-secret-user", "not-a-secret-password", "not-a-secret-token"} {
		oauth2calltest.RequireNoPiece(t, "leakguard.api-address/tarball", said, secret)
	}
	if !strings.Contains(said, "is not an absolute URL with a host") {
		t.Errorf("the refusal is %q, want it to name the address as no absolute URL with a host", said)
	}
}
