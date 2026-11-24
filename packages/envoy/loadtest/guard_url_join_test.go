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

// TestTheGuardChecksGoToTheirRoutesJoinedOntoTheEndpointURLs: the endpoints checkStack checks
// (-nats-mon, -url and -listener under -allow-foreign) can carry a path and a query. Each check
// goes to that path with its route joined onto it, the URL's query kept: http://host/prefix?key=...
// reads /prefix/varz?key=..., /prefix/api/v1/projects?key=... and /prefix/v1/sessions?key=...
// (query), and so does http://host/prefix/?key=... (query-trailing-slash), where the route
// appended to the URL's text sat inside the query and each endpoint was asked /prefix. A URL with
// no query reads where it did (no-query). A stand-in for all three records the path and the query
// each check reaches it with and answers as the stack's own would, so the guard passes.
func TestTheGuardChecksGoToTheirRoutesJoinedOntoTheEndpointURLs(t *testing.T) {
	const name, agent, listener = "loadtest-join", "join-agent-token", "join-listener-token"
	for _, c := range joinURLCases {
		t.Run(c.name, func(t *testing.T) {
			var mu sync.Mutex
			var asked []url.URL
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				asked = append(asked, *r.URL)
				mu.Unlock()
				switch {
				case strings.HasSuffix(r.URL.Path, "/varz"):
					_, _ = w.Write([]byte(`{"server_name":"` + name + `"}`))
				case strings.HasSuffix(r.URL.Path, "/api/v1/projects") && r.Header.Get("Authorization") == "Bearer "+agent:
					_, _ = w.Write([]byte(`[]`))
				case strings.HasSuffix(r.URL.Path, "/v1/sessions") && r.Header.Get("Authorization") == "Bearer "+listener:
					_, _ = w.Write([]byte(`[]`))
				case strings.HasSuffix(r.URL.Path, "/v1/sessions"):
					w.WriteHeader(http.StatusUnauthorized)
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(srv.Close)
			endpoint := srv.URL + c.suffix
			err := checkStack(context.Background(), endpoints{natsMon: endpoint, natsServerName: name, dispatch: endpoint, token: agent, listener: endpoint, listenerToken: listener})
			mu.Lock()
			defer mu.Unlock()
			want := []string{"/prefix/varz", "/prefix/api/v1/projects", "/prefix/v1/sessions", "/prefix/v1/sessions"}
			for i, u := range asked {
				if i >= len(want) || u.Path != want[i] || u.RawQuery != c.query {
					t.Errorf("check %d of an endpoint URL ending %q went to path %q with query %q; want the paths %q, each with query %q", i+1, c.suffix, u.Path, u.RawQuery, want, c.query)
				}
			}
			if len(asked) != len(want) {
				t.Errorf("the endpoints were asked %d times; want %d", len(asked), len(want))
			}
			if err != nil {
				t.Errorf("checkStack = %v; want the stand-in taken for the stack's own", err)
			}
		})
	}
}

// TestAGuardEndpointURLThatDoesNotJoinPrintsNoneOfIt: a Dispatch URL or a NATS monitor URL that
// url.Parse refuses (an invalid escape in its path), with redacttest's credentials in its user
// information and its query, fails the guard's check in fixed words, with none of the URL
// (<check>-unparsed); http.NewRequest's error, which the check printed before, quotes it whole
// (twin). An endpoint URL that parses, whose joined URL, as url.URL.String writes it, url.Parse
// refuses (an IPv6 zone holding a byte that is not ASCII, which String escapes as %FD), is refused
// the same way (<check>-reparsed); its twin shows http.NewRequest's error for the joined URL holds
// the credentials. Captures loadtest.guard-url/<check>-<case>.
func TestAGuardEndpointURLThatDoesNotJoinPrintsNoneOfIt(t *testing.T) {
	param := "access" + "_token"
	userinfo := redacttest.User + ":" + redacttest.Password + "@"
	unparsed := "http://" + userinfo + "127.0.0.1:1/endpoint%zz?" + param + "=" + redacttest.Token
	reparsed := "http://" + userinfo + "[::%25\xfd]/endpoint?" + param + "=" + redacttest.Token
	t.Run("twin", func(t *testing.T) {
		if _, err := http.NewRequest(http.MethodGet, unparsed+"/varz", nil); err == nil || len(redacttest.Held(err.Error())) != len(redacttest.Secrets) {
			t.Errorf("http.NewRequest = %v; want an error that quotes the credentials, so the row can see them", err)
		}
	})
	t.Run("reparsed-twin", func(t *testing.T) {
		parsed, err := url.Parse(reparsed)
		if err != nil {
			t.Fatalf("url.Parse refused the endpoint URL (%v); the row needs one it takes", err)
		}
		if _, err := http.NewRequest(http.MethodGet, parsed.JoinPath("varz").String(), nil); err == nil || len(redacttest.Held(err.Error())) != len(redacttest.Secrets) {
			t.Errorf("http.NewRequest of the joined URL = %v; want an error that quotes the credentials, so the row can see them", err)
		}
	})
	checks := []struct {
		name      string
		endpoints func(endpoint string) endpoints
		unparsed  string
	}{
		{"dispatch", func(endpoint string) endpoints { return endpoints{dispatch: endpoint, token: "join-agent-token"} },
			"guard: dispatch check failed: the Dispatch URL is not an absolute URL with a host (it is not printed)"},
		{"nats", func(endpoint string) endpoints { return endpoints{natsMon: endpoint, natsServerName: "loadtest-join"} },
			"guard: nats check failed: the NATS monitor URL is not an absolute URL with a host (it is not printed)"},
	}
	for _, check := range checks {
		for _, c := range []struct{ name, url, want string }{
			{"unparsed", unparsed, check.unparsed},
			{"reparsed", reparsed, "guard: " + check.name + " check failed: the endpoint's URL joined with the route does not parse (it is not printed)"},
		} {
			t.Run(check.name+"-"+c.name, func(t *testing.T) {
				err := checkStack(context.Background(), check.endpoints(c.url))
				if err == nil {
					t.Fatal("checkStack took an endpoint URL that does not join")
				}
				redacttest.RequireNoPiece(t, "loadtest.guard-url/"+check.name+"-"+c.name, err.Error())
				if err.Error() != c.want {
					t.Errorf("checkStack = %q; want %q", err, c.want)
				}
			})
		}
	}
}
