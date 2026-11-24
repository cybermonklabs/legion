package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// joinAskedStand serves, until t ends, a stand-in that records the URL of every request it is
// asked and answers each with body, and answers its base URL and the URLs it was asked so far.
func joinAskedStand(t *testing.T, body string) (string, func() []url.URL) {
	t.Helper()
	asked := make(chan url.URL, 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked <- *r.URL
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() []url.URL {
		var got []url.URL
		for {
			select {
			case u := <-asked:
				got = append(got, u)
			default:
				return got
			}
		}
	}
}

// joinURLCases are the configured URLs a URL-join row gives a client, each as the suffix of its
// stand-in's base URL, with the query the client must keep: a path and a query (query), the same
// with the path's trailing slash (query-trailing-slash), and a path alone (no-query), which reads
// where it always did.
var joinURLCases = []struct{ name, suffix, query string }{
	{"query", "/prefix?key=k3y-7Qz4k", "key=k3y-7Qz4k"},
	{"query-trailing-slash", "/prefix/?key=k3y-7Qz4k", "key=k3y-7Qz4k"},
	{"no-query", "/prefix", ""},
}

// joinWantQuery is the query a request joined onto a configured URL whose query is configured
// reaches the server with: the configured query first, then the request's own.
func joinWantQuery(configured, own string) string {
	switch {
	case configured == "":
		return own
	case own == "":
		return configured
	}
	return configured + "&" + own
}

// TestADispatchCallGoesToItsPathJoinedOntoTheDispatchURL: the Dispatch URL (-url, the stack's, or
// replay-fetch's DISPATCH_URL) can carry a path and a query. A call goes to that path with its own
// joined onto it, the URL's query kept ahead of the call's own: http://host/prefix?key=... calls
// /prefix/api/v1/issues?key=...&project=LGA (query), and so does http://host/prefix/?key=...
// (query-trailing-slash), where the path appended to the URL's text sat inside the query and
// Dispatch was asked /prefix. A URL with no query calls where it did (no-query). A stand-in
// records the path and the query the call reaches it with.
func TestADispatchCallGoesToItsPathJoinedOntoTheDispatchURL(t *testing.T) {
	for _, c := range joinURLCases {
		t.Run(c.name, func(t *testing.T) {
			base, asked := joinAskedStand(t, `{}`)
			res := newAPI(base+c.suffix, "join-bearer", 1).call(context.Background(), http.MethodGet, "/api/v1/issues?project=LGA", who{}, nil)
			if res.err != nil || res.status != http.StatusOK {
				t.Fatalf("the call answered %d, %v; want the stand-in's 200", res.status, res.err)
			}
			got := asked()
			if len(got) != 1 {
				t.Fatalf("the stand-in was asked %d times; want once", len(got))
			}
			if want := "/prefix/api/v1/issues"; got[0].Path != want {
				t.Errorf("a Dispatch URL ending %q was called at path %q (query %q); want %q", c.suffix, got[0].Path, got[0].RawQuery, want)
			}
			if want := joinWantQuery(c.query, "project=LGA"); got[0].RawQuery != want {
				t.Errorf("a Dispatch URL ending %q was called with query %q; want %q", c.suffix, got[0].RawQuery, want)
			}
		})
	}
}

// TestADispatchURLThatDoesNotJoinPrintsNoneOfIt: a Dispatch URL url.Parse refuses (an invalid
// escape in its path), with redacttest's credentials in its user information and its query, fails
// the call in fixed words, with none of the URL (unparsed); http.NewRequest's error, which the call
// answered before, quotes it whole (twin). A Dispatch URL that parses, whose joined URL, as
// url.URL.String writes it, url.Parse refuses (an IPv6 zone holding a byte that is not ASCII,
// which String escapes as %FD), is refused the same way (reparsed); its twin shows
// http.NewRequest's error for the joined URL holds the credentials. Captures
// loadtest.api-url/<case>.
func TestADispatchURLThatDoesNotJoinPrintsNoneOfIt(t *testing.T) {
	param := "access" + "_token"
	userinfo := redacttest.User + ":" + redacttest.Password + "@"
	unparsed := "http://" + userinfo + "127.0.0.1:1/dispatch%zz?" + param + "=" + redacttest.Token
	reparsed := "http://" + userinfo + "[::%25\xfd]/dispatch?" + param + "=" + redacttest.Token
	const path = "/api/v1/projects"
	call := func(t *testing.T, dispatch string) string {
		t.Helper()
		res := newAPI(dispatch, "join-bearer", 1).call(context.Background(), http.MethodGet, path, who{}, nil)
		if res.err == nil {
			t.Fatalf("a call to a Dispatch URL that does not join answered %d and no error", res.status)
		}
		return res.err.Error()
	}
	t.Run("twin", func(t *testing.T) {
		if _, err := http.NewRequest(http.MethodGet, unparsed+path, nil); err == nil || len(redacttest.Held(err.Error())) != len(redacttest.Secrets) {
			t.Errorf("http.NewRequest = %v; want an error that quotes the credentials, so the row can see them", err)
		}
	})
	t.Run("unparsed", func(t *testing.T) {
		said := call(t, unparsed)
		redacttest.RequireNoPiece(t, "loadtest.api-url/unparsed", said)
		if want := "the Dispatch URL and the call's path do not join into a URL (neither is printed)"; said != want {
			t.Errorf("the call's error = %q; want %q", said, want)
		}
	})
	t.Run("reparsed-twin", func(t *testing.T) {
		parsed, err := url.Parse(reparsed)
		if err != nil {
			t.Fatalf("url.Parse refused the Dispatch URL (%v); the row needs one it takes", err)
		}
		if _, err := http.NewRequest(http.MethodGet, parsed.JoinPath(path).String(), nil); err == nil || len(redacttest.Held(err.Error())) != len(redacttest.Secrets) {
			t.Errorf("http.NewRequest of the joined URL = %v; want an error that quotes the credentials, so the row can see them", err)
		}
	})
	t.Run("reparsed", func(t *testing.T) {
		said := call(t, reparsed)
		redacttest.RequireNoPiece(t, "loadtest.api-url/reparsed", said)
		if want := "the Dispatch URL joined with the call's path does not parse (it is not printed)"; said != want {
			t.Errorf("the call's error = %q; want %q", said, want)
		}
	})
}
