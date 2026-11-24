package githubapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/githubtest"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/internal/urljoin"
)

// askedURL is one request the API stand-in answered, as it arrived.
type askedURL struct{ path, query string }

// apiBaseStandIn is a client whose API base is the stand-in's URL with suffix after it, and a
// reader of every request the stand-in has been asked. The stand-in answers the four calls below
// at whatever path the base puts them under, and 404 at any other path, which is where a base
// with a path and a query sends them when the call's path is appended to the base's text.
func apiBaseStandIn(t *testing.T, suffix string) (*Client, func() []askedURL) {
	t.Helper()
	var mu sync.Mutex
	var asked []askedURL
	server := githubtest.Serve(t, githubtest.Spec{
		Record: func(r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			asked = append(asked, askedURL{path: r.URL.Path, query: r.URL.RawQuery})
		},
		Handle: func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/installation"):
				fmt.Fprint(w, `{"id":7,"app_slug":"dispatch","permissions":{"contents":"read"}}`)
			case strings.HasSuffix(r.URL.Path, "/access_tokens"):
				w.WriteHeader(http.StatusCreated)
				fmt.Fprintf(w, `{"token":"ghs_test","expires_at":%q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
			case strings.HasSuffix(r.URL.Path, "/pulls/7"):
				fmt.Fprint(w, `{"number":7}`)
			case strings.HasSuffix(r.URL.Path, "/app/hook/deliveries"):
				fmt.Fprint(w, `[]`)
			default:
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"message":"Not Found"}`)
			}
		},
	})
	_, pemText := testKeyPEM(t)
	client, err := New(&auth.AppConfig{ClientID: "Iv1.testclient", PEM: pemText}, server.URL+suffix)
	if err != nil {
		t.Fatalf("new client at a base ending %q: %v", suffix, err)
	}
	return client, func() []askedURL {
		mu.Lock()
		defer mu.Unlock()
		return append([]askedURL(nil), asked...)
	}
}

// TestAGitHubCallGoesToThePathJoinedOntoTheAPIBase: the API base is the deployment's
// (DISPATCH_GITHUB_API_BASE), and it can carry a path, a query or a fragment. Every call of this
// client asks the base's path with the call's own joined onto it, the base's query kept ahead of
// the call's own and the fragment, which no request sends, dropped: with the base
// http://host/api?sig=..., the installation lookup asks /api/repos/acme/web/installation?sig=...
// (query), and so does the base http://host/api/?sig=... (query-trailing-slash), where the call's
// path appended to the base's text sat inside the query (/api?sig=.../repos/... — GitHub was
// asked /api, with the query's last value changed); http://host/api#pane asks /api/repos/... with
// no fragment (fragment), where the path sat inside the fragment. A base with neither is asked
// where it was (no-query). The stand-in records the path and the query of every call.
func TestAGitHubCallGoesToThePathJoinedOntoTheAPIBase(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct{ name, suffix, query string }{
		{"query", "/api?sig=s1g-7Qz4k", "sig=s1g-7Qz4k"},
		{"query-trailing-slash", "/api/?sig=s1g-7Qz4k", "sig=s1g-7Qz4k"},
		{"fragment", "/api#pane", ""},
		{"no-query", "/api", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			client, asked := apiBaseStandIn(t, c.suffix)
			for _, call := range []struct {
				name, path, query string
				make              func() error
			}{{
				name: "the installation lookup",
				path: "/api/repos/acme/web/installation",
				make: func() error { _, err := client.Installation(ctx, "acme", "web"); return err },
			}, {
				name: "the token mint",
				path: "/api/app/installations/7/access_tokens",
				make: func() error { _, err := client.Token(ctx, 7); return err },
			}, {
				name:  "a read of a pull request",
				path:  "/api/repos/acme/web/pulls/7",
				query: "per_page=1",
				make: func() error {
					_, status, _, err := client.Read(ctx, "ghs_test", "/repos/acme/web/pulls/7?per_page=1")
					if err == nil && status != http.StatusOK {
						return fmt.Errorf("the read answered status %d", status)
					}
					return err
				},
			}, {
				name:  "the deliveries listing",
				path:  "/api/app/hook/deliveries",
				query: "per_page=100",
				make:  func() error { _, err := client.Deliveries(ctx, time.Time{}, nil); return err },
			}} {
				want := askedURL{path: call.path, query: c.query}
				if call.query != "" {
					want.query = strings.TrimPrefix(c.query+"&"+call.query, "&")
				}
				before := len(asked())
				err := call.make()
				made := asked()[before:]
				if len(made) != 1 {
					t.Errorf("%s at a base ending %q made %d requests, want 1; it answered %v", call.name, c.suffix, len(made), err)
					continue
				}
				if made[0] != want {
					t.Errorf("%s at a base ending %q asked path %q, query %q; want path %q, query %q", call.name, c.suffix, made[0].path, made[0].query, want.path, want.query)
				}
				if err != nil {
					t.Errorf("%s at a base ending %q answered %v; want the call answered", call.name, c.suffix, err)
				}
			}
		})
	}
}

// TestAnAPIBaseThatDoesNotParsePrintsNoneOfIt: a base URL url.Parse refuses (an invalid escape in
// its path), with redacttest's credentials in its user information and its query, is refused at
// New in fixed words that hold none of it (unparsed); the error of a request built at that base,
// which the client made before the base was parsed at New, quotes the base whole (twin). No
// client is built and nothing reaches the wire. Captures githubapp.api-base/unparsed.
func TestAnAPIBaseThatDoesNotParsePrintsNoneOfIt(t *testing.T) {
	unparsed := "http://" + redacttest.User + ":" + redacttest.Password + "@127.0.0.1:1/api%zz?access" + "_token=" + redacttest.Token
	t.Run("twin", func(t *testing.T) {
		_, err := http.NewRequest(http.MethodGet, unparsed+"/repos/acme/web/installation", nil)
		if err == nil || len(redacttest.Held(err.Error())) != len(redacttest.Secrets) {
			t.Errorf("http.NewRequest at that base = %v; want an error that quotes the credentials, so the row can see them printed", err)
		}
	})
	t.Run("unparsed", func(t *testing.T) {
		_, pemText := testKeyPEM(t)
		client, err := New(&auth.AppConfig{ClientID: "Iv1.testclient", PEM: pemText}, unparsed)
		text := fmt.Sprint(err)
		if err == nil {
			// Before the base was parsed at New, the first call printed it: the target it built
			// is the base's text with the call's path after it, which the client shows whole.
			_, called := client.Installation(context.Background(), "acme", "web")
			text = fmt.Sprint(called)
		}
		redacttest.RequireNoPiece(t, "githubapp.api-base/unparsed", text, redacttest.Token, redacttest.Password)
		switch {
		case err == nil:
			t.Fatalf("New took a base URL that does not parse; the first call answered %s", text)
		case !errors.Is(err, urljoin.ErrBase):
			t.Errorf("New = %v; want urljoin.ErrBase, which prints none of the base", err)
		case client != nil:
			t.Errorf("New answered a client at a base that does not parse: %#v", client)
		}
	})
}

// TestAReadOfAPathThatIsNoRelativeReferenceIsRefused: Read's path is its caller's, and it is
// joined onto the API origin. A path that names a host, or carries a fragment, is no relative
// reference: the read is refused by urljoin.ErrRef, nothing is sent, and the refusal quotes
// neither the path nor the origin, either of which can carry a credential.
func TestAReadOfAPathThatIsNoRelativeReferenceIsRefused(t *testing.T) {
	for _, c := range []struct{ name, path string }{
		{"host", "//" + redacttest.User + ".example/repos/acme/web"},
		{"fragment", "/repos/acme/web?access" + "_token=" + redacttest.Token + "#" + redacttest.Password},
	} {
		t.Run(c.name, func(t *testing.T) {
			client, asked := apiBaseStandIn(t, "/api")
			_, _, _, err := client.Read(context.Background(), "ghs_test", c.path)
			if !errors.Is(err, urljoin.ErrRef) {
				t.Fatalf("Read of %q = %v; want urljoin.ErrRef", c.path, err)
			}
			for _, held := range redacttest.Held(err.Error()) {
				t.Errorf("the refusal %q holds %q, which the path carried", err, held)
			}
			if n := len(asked()); n != 0 {
				t.Errorf("%d requests reached the wire for a path that is no relative reference, want none", n)
			}
		})
	}
}
