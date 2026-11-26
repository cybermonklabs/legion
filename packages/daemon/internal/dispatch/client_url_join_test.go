package dispatch

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
)

// TestADispatchCallGoesToTheRouteJoinedOntoTheDispatchURL: the Dispatch URL can carry a path and a
// query. Each call goes to the URL's path with its route joined onto it, the URL's query kept
// ahead of the route's own: http://host/dispatch?sig=... is asked
// /dispatch/api/v1/issues/LEGION-1?sig=... for the issue and /dispatch/api/v1/issues?sig=...&project=legion
// for the list (query), and so is http://host/dispatch/?sig=... (query-trailing-slash), where the
// route appended to the URL's text sat inside the query (/dispatch?sig=.../api/v1/issues/LEGION-1)
// and Dispatch was asked /dispatch. A URL with no query is asked as it was (no-query). A stand-in
// Dispatch records the path and the query each call reaches it with.
func TestADispatchCallGoesToTheRouteJoinedOntoTheDispatchURL(t *testing.T) {
	for _, c := range []struct{ name, suffix, query string }{
		{"query", "/dispatch?sig=s1g-7Qz4k", "sig=s1g-7Qz4k"},
		{"query-trailing-slash", "/dispatch/?sig=s1g-7Qz4k", "sig=s1g-7Qz4k"},
		{"no-query", "/dispatch", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			asked := make(chan url.URL, 2)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				asked <- *r.URL
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "/issues") {
					_, _ = io.WriteString(w, `[]`)
					return
				}
				_, _ = io.WriteString(w, `{"key":"LEGION-1","status":"todo"}`)
			}))
			defer server.Close()
			client := New(server.URL+c.suffix, BearerTransport("dispatch-token", nil))
			ask := func(call string, path, query string, err error) {
				t.Helper()
				select {
				case got := <-asked:
					if got.Path != path || got.RawQuery != query {
						t.Errorf("%s at a Dispatch URL ending %q asked path %q, query %q; want path %q, query %q", call, c.suffix, got.Path, got.RawQuery, path, query)
					}
				default:
					t.Fatalf("%s reached no Dispatch; it answered %v", call, err)
				}
				if err != nil {
					t.Errorf("%s = %v; want Dispatch's 200 taken", call, err)
				}
			}
			_, err := client.GetIssue(context.Background(), "LEGION-1")
			ask("GetIssue", "/dispatch/api/v1/issues/LEGION-1", c.query, err)
			_, err = client.ListIssues(context.Background(), "legion", nil)
			ask("ListIssues", "/dispatch/api/v1/issues", strings.TrimPrefix(c.query+"&project=legion", "&"), err)
		})
	}
}

// TestADispatchURLThatDoesNotParsePrintsNoneOfIt: a Dispatch URL url.Parse refuses (an invalid
// escape in its path), with made-up credentials in its user information and its query, fails the
// call in the client's own words, which hold none of the URL (unparsed); http.NewRequest's error
// for that URL quotes it whole (twin). A URL url.Parse takes whose joined URL, as url.URL.String
// writes it, url.Parse refuses (an IPv6 zone holding a byte that is not ASCII) fails through
// oauth2call.Unanswered, which names a URL it cannot read by no part of it (reparsed). Captures
// dispatch.dispatch-url/<case>.
func TestADispatchURLThatDoesNotParsePrintsNoneOfIt(t *testing.T) {
	const (
		user     = "not-a-secret-user"
		password = "not-a-secret-password"
	)
	unparsed := "http://" + user + ":" + password + "@127.0.0.1:1/dispatch%zz?sig=" + oauth2calltest.Token
	reparsed := "http://" + user + ":" + password + "@[::%25\xfd]/dispatch?sig=" + oauth2calltest.Token
	holds := func(t *testing.T, text string) {
		t.Helper()
		for _, credential := range []string{user, password, oauth2calltest.Token} {
			if !strings.Contains(text, credential) {
				t.Fatalf("%q holds no %q, so the row cannot see a leak", text, credential)
			}
		}
	}
	t.Run("twin", func(t *testing.T) {
		_, err := http.NewRequest(http.MethodGet, unparsed+"/api/v1/issues/LEGION-1", nil)
		if err == nil {
			t.Fatalf("http.NewRequest took %q; the row needs a URL that does not parse", unparsed)
		}
		holds(t, err.Error())
	})
	t.Run("unparsed", func(t *testing.T) {
		_, err := New(unparsed, BearerTransport("dispatch-token", nil)).GetIssue(context.Background(), "LEGION-1")
		if err == nil {
			t.Fatal("GetIssue took a Dispatch URL that does not parse")
		}
		oauth2calltest.RequireNoPiece(t, "dispatch.dispatch-url/unparsed", err.Error(), oauth2calltest.Token, user, password)
		if want := "create Dispatch GET /api/v1/issues/LEGION-1: the Dispatch URL is not an absolute URL with a host (it is not printed)"; err.Error() != want {
			t.Errorf("GetIssue's error is %q, want %q", err, want)
		}
	})
	t.Run("reparsed-twin", func(t *testing.T) {
		parsed, err := url.Parse(reparsed)
		if err != nil {
			t.Fatalf("url.Parse refused %q (%v); the row needs a Dispatch URL it takes", reparsed, err)
		}
		_, err = http.NewRequest(http.MethodGet, parsed.JoinPath("/api/v1/issues/LEGION-1").String(), nil)
		if err == nil {
			t.Fatal("http.NewRequest took the joined URL; the row needs one it refuses")
		}
		holds(t, err.Error())
	})
	t.Run("reparsed", func(t *testing.T) {
		_, err := New(reparsed, BearerTransport("dispatch-token", nil)).GetIssue(context.Background(), "LEGION-1")
		if err == nil {
			t.Fatal("GetIssue took a Dispatch URL whose joined URL does not parse")
		}
		oauth2calltest.RequireNoPiece(t, "dispatch.dispatch-url/reparsed", err.Error(), oauth2calltest.Token, user, password)
	})
}
