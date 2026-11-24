package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// joinRoomsPG is a Postgres URL nothing listens at, so a rooms read's connection fails at once.
const joinRoomsPG = "postgres://join@127.0.0.1:1/dispatch?connect_timeout=1"

// TestTheRoomsReadsGoToTheirRoutesJoinedOntoTheEndpointURLs: the proxy's wire counts and
// Dispatch's /debug/vars (-pprof) are read at an endpoint URL that can carry a path and a query.
// Each read goes to that path with its route joined onto it, the URL's query kept:
// http://host/prefix?key=... reads /prefix/stats?key=... and /prefix/debug/vars?key=... (query),
// and so does http://host/prefix/?key=... (query-trailing-slash), where the route appended to the
// URL's text sat inside the query, the endpoint was asked /prefix and the window had no reading. A
// URL with no query reads where it did (no-query). A stand-in for both records the path and the
// query each read reaches it with.
func TestTheRoomsReadsGoToTheirRoutesJoinedOntoTheEndpointURLs(t *testing.T) {
	for _, c := range joinURLCases {
		t.Run(c.name, func(t *testing.T) {
			var mu sync.Mutex
			var asked []url.URL
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				asked = append(asked, *r.URL)
				mu.Unlock()
				switch {
				case strings.HasSuffix(r.URL.Path, "/stats"):
					_, _ = w.Write([]byte(`{}`))
				case strings.HasSuffix(r.URL.Path, "/debug/vars"):
					_, _ = w.Write([]byte(`{"dispatch_rooms":{"appended_updates":1}}`))
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(srv.Close)
			endpoint := srv.URL + c.suffix
			r := readWindow(context.Background(), endpoints{proxyStats: endpoint, pprof: endpoint, pg: joinRoomsPG})
			mu.Lock()
			defer mu.Unlock()
			want := []string{"/prefix/stats", "/prefix/debug/vars"}
			for i, u := range asked {
				if i >= len(want) || u.Path != want[i] || u.RawQuery != c.query {
					t.Errorf("read %d of an endpoint URL ending %q went to path %q with query %q; want the paths %q, each with query %q", i+1, c.suffix, u.Path, u.RawQuery, want, c.query)
				}
			}
			if len(asked) != len(want) {
				t.Errorf("the endpoints were asked %d times; want %d", len(asked), len(want))
			}
			if r.proxy == nil || r.rooms == nil {
				t.Errorf("readWindow read proxy %v (%s) and rooms %v (%s); want both", r.proxy, r.proxyErr, r.rooms, r.roomsErr)
			}
		})
	}
}

// TestARoomsEndpointURLThatDoesNotJoinPrintsNoneOfIt: a proxy-stats or a -pprof URL url.Parse
// refuses (an invalid escape in its path), with redacttest's credentials in its user information
// and its query, gives the window's reading a reason in fixed words, with none of the URL
// (<read>-unparsed), where the reason held http.NewRequest's error, which quotes it whole (twin).
// Captures loadtest.rooms-url/<read>-unparsed.
func TestARoomsEndpointURLThatDoesNotJoinPrintsNoneOfIt(t *testing.T) {
	param := "access" + "_token"
	unparsed := "http://" + redacttest.User + ":" + redacttest.Password + "@127.0.0.1:1/endpoint%zz?" + param + "=" + redacttest.Token
	t.Run("twin", func(t *testing.T) {
		if _, err := http.NewRequest(http.MethodGet, unparsed+"/stats", nil); err == nil || len(redacttest.Held(err.Error())) != len(redacttest.Secrets) {
			t.Errorf("http.NewRequest = %v; want an error that quotes the credentials, so the row can see them", err)
		}
	})
	r := readWindow(context.Background(), endpoints{proxyStats: unparsed, pprof: unparsed, pg: joinRoomsPG})
	for _, c := range []struct{ name, said, want string }{
		{"proxy", r.proxyErr, "the proxy serves no wire counts: GET /stats: the URL it is joined onto is not an absolute URL with a host (it is not printed)"},
		{"pprof", r.roomsErr, notServed + " (GET /debug/vars: the URL it is joined onto is not an absolute URL with a host (it is not printed))"},
	} {
		t.Run(c.name+"-unparsed", func(t *testing.T) {
			redacttest.RequireNoPiece(t, "loadtest.rooms-url/"+c.name+"-unparsed", c.said)
			if c.said != c.want {
				t.Errorf("readWindow's reason = %q; want %q", c.said, c.want)
			}
		})
	}
}
