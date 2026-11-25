package legion

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAStateReadGoesToTheRouteJoinedOntoTheDaemonURL: a project's daemon URL is a setting (a
// legion_settings row), and it can carry a path and a query. The state read goes to the URL's
// path with StatePath joined onto it and the URL's query kept: http://host:port/daemon?sig=...
// reads /daemon/legion/v1/state?sig=... (query), where the route appended to the URL's text sat
// inside the query (/daemon?sig=.../legion/v1/state, the daemon asked /daemon with the query's
// value changed). A URL with neither reads where it did (no-query). A stand-in daemon records
// the path and the query the read reaches it with, and answers a state at any path.
func TestAStateReadGoesToTheRouteJoinedOntoTheDaemonURL(t *testing.T) {
	for _, c := range []struct{ name, suffix, query string }{
		{"query", "/daemon?sig=s1g-7Qz4k", "sig=s1g-7Qz4k"},
		{"no-query", "/daemon", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			var mu sync.Mutex
			var asked []url.URL
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				asked = append(asked, *r.URL)
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"issues":{}}`))
			}))
			t.Cleanup(server.Close)
			if _, _, err := New(server.URL + c.suffix).State(t.Context()); err != nil {
				t.Errorf("State: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(asked) != 1 || asked[0].Path != "/daemon"+StatePath || asked[0].RawQuery != c.query {
				t.Errorf("a daemon URL ending %q was asked %v; want one read of path %q, query %q", c.suffix, asked, "/daemon"+StatePath, c.query)
			}
		})
	}
}

// TestADaemonURLThatDoesNotParsePrintsNoneOfIt: a daemon URL url.Parse refuses (an invalid escape
// in its path), with redacttest's credentials in its user information and its query, fails the
// read in the client's own words, the route named and the failure's class, with none of the URL
// (unparsed); http.NewRequest's error, which the read printed and the board's header showed,
// quotes it whole (twin). A daemon URL that parses, whose joined URL, as url.URL.String writes
// it, url.Parse refuses (an IPv6 zone holding a byte that is not ASCII, which String escapes as
// %FD), is refused the same way (reparsed); its twin shows http.NewRequest's error for the joined
// URL holds the credentials. Captures legion.daemon-url/<case>.
func TestADaemonURLThatDoesNotParsePrintsNoneOfIt(t *testing.T) {
	param := "access" + "_token"
	userinfo := redacttest.User + ":" + redacttest.Password + "@"
	unparsed := "http://" + userinfo + "127.0.0.1:1/daemon%zz?" + param + "=" + redacttest.Token
	reparsed := "http://" + userinfo + "[::%25\xfd]/daemon?" + param + "=" + redacttest.Token
	const wantSaid = "the Legion daemon could not be read: build GET /legion/v1/state: not a valid URL"
	read := func(t *testing.T, base string) string {
		t.Helper()
		_, _, err := New(base).State(t.Context())
		if err == nil {
			t.Fatal("State read a daemon URL that does not parse")
		}
		return err.Error()
	}
	t.Run("twin", func(t *testing.T) {
		if _, err := http.NewRequest(http.MethodGet, unparsed+StatePath, nil); err == nil || len(redacttest.Held(err.Error())) != len(redacttest.Secrets) {
			t.Errorf("http.NewRequest = %v; want an error that quotes the credentials, so the row can see them", err)
		}
	})
	t.Run("unparsed", func(t *testing.T) {
		said := read(t, unparsed)
		redacttest.RequireNoPiece(t, "legion.daemon-url/unparsed", said)
		if said != wantSaid {
			t.Errorf("State = %q; want %q", said, wantSaid)
		}
	})
	t.Run("reparsed-twin", func(t *testing.T) {
		parsed, err := url.Parse(reparsed)
		if err != nil {
			t.Fatalf("url.Parse refused the daemon URL (%v); the row needs one it takes", err)
		}
		if _, err := http.NewRequest(http.MethodGet, parsed.JoinPath(StatePath).String(), nil); err == nil || len(redacttest.Held(err.Error())) != len(redacttest.Secrets) {
			t.Errorf("http.NewRequest of the joined URL = %v; want an error that quotes the credentials, so the row can see them", err)
		}
	})
	t.Run("reparsed", func(t *testing.T) {
		said := read(t, reparsed)
		redacttest.RequireNoPiece(t, "legion.daemon-url/reparsed", said)
		if said != wantSaid {
			t.Errorf("State = %q; want %q", said, wantSaid)
		}
	})
}
