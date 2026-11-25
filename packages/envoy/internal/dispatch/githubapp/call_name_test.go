package githubapp

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/internal/urljoin"
)

// TestACallIsNamedByItsRouteAfterTheOriginsName: an error names a call by the API origin as the URL
// rule names a URL a request asked (redact.AskedURL) with the route Dispatch built after it
// (Client.target), not by the rule's name of the whole URL asked (whole), which leaves out the
// route's query and is redact.NotPrinted for a route the rule refuses: a tree's <sha>:<dir> and a
// branch with a '/' still name the repository and the branch, the route's own query prints while
// the base's does not, an origin whose path ends in a slash is named as the request asked it, and
// an origin the rule cannot print still leaves the route printed. Each read is answered 500, or
// past the reader's limit.
func TestACallIsNamedByItsRouteAfterTheOriginsName(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name string
		// path is the API base's path, its query or none after it.
		path string
		// route is the route Dispatch builds for the call.
		route string
		// read makes the call and answers its error.
		read func(*Client) error
		// want is the error's text and whole the rule's name of the whole URL asked, {origin}
		// standing for the origin as AskedURL names it, without a trailing slash.
		want, whole string
	}{{
		name:  "tree",
		path:  "/api/",
		route: "/repos/acme/web/git/trees/c0ffee:docs%2Fnotes",
		read: func(client *Client) error {
			_, err := client.Dir(ctx, "ghs_test", "acme", "web", "c0ffee", "docs/notes")
			return err
		},
		want:  "GET {origin}/repos/acme/web/git/trees/c0ffee:docs%2Fnotes: status 500",
		whole: redact.NotPrinted,
	}, {
		name:  "branch-with-a-slash",
		path:  "/api?" + accessTokenParam + "=" + redacttest.Token,
		route: "/repos/acme/web/commits/feature%2Fx",
		read: func(client *Client) error {
			_, err := client.Ref(ctx, "ghs_test", "acme", "web", "feature/x")
			return err
		},
		want:  "GET {origin}/repos/acme/web/commits/feature%2Fx: status 500",
		whole: redact.NotPrinted,
	}, {
		name:  "route-query",
		path:  "/api?" + accessTokenParam + "=" + redacttest.Token,
		route: "/repos/acme/web/pulls?state=open&per_page=100",
		read: func(client *Client) error {
			_, _, _, err := client.ReadWithin(ctx, "ghs_test", "/repos/acme/web/pulls?state=open&per_page=100", 4)
			return err
		},
		want:  "GET {origin}/repos/acme/web/pulls?state=open&per_page=100: GitHub response body exceeds the 4-byte limit",
		whole: "{origin}/repos/acme/web/pulls",
	}, {
		name:  "origin-not-printed",
		path:  "/api%20v3",
		route: "/repos/acme/web/commits/main",
		read: func(client *Client) error {
			_, err := client.Ref(ctx, "ghs_test", "acme", "web", "main")
			return err
		},
		want:  "GET " + redact.NotPrinted + "/repos/acme/web/commits/main: status 500",
		whole: redact.NotPrinted,
	}} {
		t.Run(c.name, func(t *testing.T) {
			host := serving(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls") {
					fmt.Fprint(w, `[{"number":7}]`)
					return
				}
				w.WriteHeader(http.StatusInternalServerError)
			})
			base := "http://" + host + c.path
			origin, err := urljoin.Join(base, "")
			if err != nil {
				t.Fatalf("join the base: %v", err)
			}
			asked, err := urljoin.JoinURL(origin, c.route)
			if err != nil {
				t.Fatalf("join the route: %v", err)
			}
			named := strings.TrimSuffix(redact.AskedURL(origin), "/")
			if whole := strings.ReplaceAll(c.whole, "{origin}", named); redact.AskedURL(asked) != whole {
				t.Errorf("the rule names the whole URL asked %q; want %q, what a name of it alone would lose", redact.AskedURL(asked), whole)
			}
			got := fmt.Sprint(c.read(credentialClient(t, base)))
			if want := strings.ReplaceAll(c.want, "{origin}", named); got != want {
				t.Errorf("the call's error is %q; want %q", got, want)
			}
		})
	}
}
