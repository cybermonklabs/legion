package main

import (
	"context"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestADispatchCallGoesToItsPathJoinedOntoTheDispatchURL: the Dispatch URL (-url, the stack's, or
// replay-fetch's DISPATCH_URL) can carry a path and a query. A call goes to that path with its own
// joined onto it, the URL's query kept ahead of the call's own: http://host/prefix?key=... calls
// /prefix/api/v1/issues?key=...&project=P (query), and so does http://host/prefix/?key=...
// (query-trailing-slash), where the path appended to the URL's text sat inside the query and
// Dispatch was asked /prefix. A URL with no query calls where it did (no-query). A stand-in
// records the path and the query the call reaches it with.
func TestADispatchCallGoesToItsPathJoinedOntoTheDispatchURL(t *testing.T) {
	for _, c := range joinURLCases {
		t.Run(c.name, func(t *testing.T) {
			base, asked := joinAskedStand(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) })
			res := newAPI(base+c.suffix, "join-bearer", 1).call(context.Background(), http.MethodGet, "/api/v1/issues?project=P", who{}, nil)
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
			if want := joinWantQuery(c.query, "project=P"); got[0].RawQuery != want {
				t.Errorf("a Dispatch URL ending %q was called with query %q; want %q", c.suffix, got[0].RawQuery, want)
			}
		})
	}
}

// TestADispatchURLThatDoesNotJoinPrintsNoneOfIt: a Dispatch URL url.Parse refuses (an invalid
// escape in its path), with redacttest's credentials in its user information and its query, fails
// the call in fixed words, with none of the URL (unparsed); http.NewRequest's error, which the call
// answered before, quotes it whole (twin). A Dispatch URL that parses, whose joined URL, as
// url.URL.String writes it, url.Parse refuses (an IPv6 zone escape), is refused the same way
// (reparsed); its twin shows http.NewRequest's error for the joined URL holds the credentials.
// Captures loadtest.api-url/<case>.
func TestADispatchURLThatDoesNotJoinPrintsNoneOfIt(t *testing.T) {
	const path = "/api/v1/projects"
	unparsed, reparsed := joinRefusedURLs("/dispatch")
	joinRequireTwins(t, unparsed, reparsed, path)
	for _, c := range []struct{ name, url, want string }{
		{"unparsed", unparsed, "the Dispatch URL and the call's path do not join into a URL (neither is printed)"},
		{"reparsed", reparsed, "the Dispatch URL joined with the call's path does not parse (it is not printed)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := newAPI(c.url, "join-bearer", 1).call(context.Background(), http.MethodGet, path, who{}, nil)
			if res.err == nil {
				t.Fatalf("a call to a Dispatch URL that does not join answered %d and no error", res.status)
			}
			said := res.err.Error()
			redacttest.RequireNoPiece(t, "loadtest.api-url/"+c.name, said)
			if said != c.want {
				t.Errorf("the call's error = %q; want %q", said, c.want)
			}
		})
	}
}
