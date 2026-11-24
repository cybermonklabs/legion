package githubrest_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/redirects/redirectstest"
)

// TestARedirectIsGitHubsAnswerAndNoLocationReachesTheError is the round-7 red team's probe of the
// client as rows: whatever answers in GitHub's place answers a read with a redirect whose Location
// quotes the installation token the read carried. Go parses a Location before a client's
// CheckRedirect is asked, so one it cannot parse (unparsed, unparsed-unnamed) is net/http's error
// whatever the client's redirect policy; one that parses was followed, to an address nothing
// listens at (unanswered, whose error net/http names by the Location) or to a stand-in that
// answers (followed), each on another host than the read was sent to. No read's error holds a run
// of the token or a word of the Location, and the stand-in a Location names is asked nothing: the
// client follows a redirect only on the origin the read was sent to, so another is GitHub's
// answer, refused by its status. The twin shows the stand-in's echo reaches net/http's own error,
// so a row can see a leak; plain shows a 200 still reads, and renamed that a 301 on the read's own
// origin, GitHub's answer for a renamed repository, is still followed and read. Captures
// githubrest.redirect/<case>.
func TestARedirectIsGitHubsAnswerAndNoLocationReachesTheError(t *testing.T) {
	token := githubtest.InstallationToken(t.Name()) + "-RDX-"
	target, asked := redirectstest.Target(t)
	dead := redirectstest.Dead(t)
	t.Run("twin", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodGet, redirectstest.Echo(t, "Authorization", redirectstest.Unparsed)+"/pulls/7/reviews", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			_ = response.Body.Close()
			t.Fatal("net/http answered the unparsed Location with no error: the rows cannot see one")
		}
		if !strings.Contains(err.Error(), token) {
			t.Fatalf("net/http's own error holds no echo of the token, so the rows cannot see a leak: %q", err)
		}
	})
	bare := func(credential string) string { return strings.TrimPrefix(credential, "Bearer ") }
	for _, c := range []struct {
		name, shown string
		location    func(credential string) string
	}{
		{"unparsed", "/pulls/7/reviews", redirectstest.Unparsed},
		{"unparsed-unnamed", "", redirectstest.Unparsed},
		{"unanswered", "", func(credential string) string { return redirectstest.To(dead, bare(credential)) }},
		{"followed", "", func(credential string) string { return redirectstest.To(target, bare(credential)) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			client := githubrest.Client{Token: token, API: redirectstest.Echo(t, "Authorization", c.location) + "/repos/acme/widgets"}
			var into []map[string]string
			err := client.Get(context.Background(), "/pulls/7/reviews", c.shown, &into)
			if err == nil {
				t.Fatal("the read answered no error")
			}
			oauth2calltest.RequireNoPiece(t, "githubrest.redirect/"+c.name, err.Error(), token)
			if strings.Contains(err.Error(), "moved") {
				t.Errorf("the read's error names the Location: %q", err)
			}
			if n := asked(); n != 0 {
				t.Errorf("the redirect's target was asked %d times, want none", n)
			}
		})
	}
	t.Run("plain", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `[{"state":"APPROVED"}]`)
		}))
		defer server.Close()
		var into []map[string]string
		if err := (githubrest.Client{Token: token, API: server.URL + "/repos/acme/widgets"}).Get(context.Background(), "/pulls/7/reviews", "", &into); err != nil || len(into) != 1 || into[0]["state"] != "APPROVED" {
			t.Fatalf("a plain 200 = %v, %v; want its one review read", into, err)
		}
	})
	t.Run("renamed", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/repos/acme/widgets/") {
				http.Redirect(w, r, "/repositories/7"+strings.TrimPrefix(r.URL.Path, "/repos/acme/widgets"), http.StatusMovedPermanently)
				return
			}
			if r.Header.Get("Authorization") != "Bearer "+token {
				t.Errorf("the followed read carried %q, want the installation token", r.Header.Get("Authorization"))
			}
			fmt.Fprint(w, `[{"state":"APPROVED"}]`)
		}))
		defer server.Close()
		var into []map[string]string
		if err := (githubrest.Client{Token: token, API: server.URL + "/repos/acme/widgets"}).Get(context.Background(), "/pulls/7/reviews", "", &into); err != nil || len(into) != 1 || into[0]["state"] != "APPROVED" {
			t.Fatalf("a 301 on the read's own origin = %v, %v; want it followed and its one review read", into, err)
		}
	})
}
