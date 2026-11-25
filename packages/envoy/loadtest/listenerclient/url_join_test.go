package listenerclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/listenerwire"
)

// TestACallGoesToItsRouteJoinedOntoTheListenerURL: a listener's URL (-listener, the stack's, or a
// -loop-listeners endpoint) can carry a path and a query. Every call goes to that path with its
// route joined onto it, the URL's query kept: http://host/prefix?key=... reads
// /prefix/v1/sessions?key=... (query), and so does http://host/prefix/?key=...
// (query-trailing-slash), where the route appended to the URL's text sat inside the query and the
// listener was asked /prefix. A URL with no query calls where it did (no-query). A stand-in
// records the path and the query each call reaches it with.
func TestACallGoesToItsRouteJoinedOntoTheListenerURL(t *testing.T) {
	routes := []string{"/v1/interests/subscribe", "/v1/messages/publish", "/v1/interests/unsubscribe", SessionsRoute}
	for _, c := range []struct{ name, suffix, query string }{
		{"query", "/prefix?key=k3y-7Qz4k", "key=k3y-7Qz4k"},
		{"query-trailing-slash", "/prefix/?key=k3y-7Qz4k", "key=k3y-7Qz4k"},
		{"no-query", "/prefix", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			var mu sync.Mutex
			var asked []url.URL
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				asked = append(asked, *r.URL)
				mu.Unlock()
				_, _ = w.Write([]byte(`{}`))
			}))
			t.Cleanup(srv.Close)
			client := &Client{URL: srv.URL + c.suffix, Token: "join-bearer", HTTP: srv.Client()}
			ctx := context.Background()
			if _, err := client.Subscribe(ctx, listenerwire.SubscribeRequest{SessionID: "ses_a"}); err != nil {
				t.Errorf("subscribe: %v", err)
			}
			if err := client.Publish(ctx, listenerwire.PublishRequest{Topic: "notifications.team.a"}); err != nil {
				t.Errorf("publish: %v", err)
			}
			if _, err := client.Unsubscribe(ctx, listenerwire.UnsubscribeRequest{SessionID: "ses_a"}); err != nil {
				t.Errorf("unsubscribe: %v", err)
			}
			if _, _, err := client.Sessions(ctx); err != nil {
				t.Errorf("sessions: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(asked) != len(routes) {
				t.Fatalf("the stand-in was asked %d times; want %d", len(asked), len(routes))
			}
			for i, route := range routes {
				if want := "/prefix" + route; asked[i].Path != want || asked[i].RawQuery != c.query {
					t.Errorf("a listener URL ending %q was called at path %q with query %q; want path %q with query %q", c.suffix, asked[i].Path, asked[i].RawQuery, want, c.query)
				}
			}
		})
	}
}

// TestAListenerURLThatDoesNotJoinPrintsNoneOfIt: a listener URL url.Parse refuses (an invalid
// escape in its path), with redacttest's credentials in its user information and its query, fails
// the session-list read (sessions) and a publish (publish) in fixed words, with none of the URL
// (<call>-unparsed); http.NewRequest's error, which each answered before, quotes it whole (twin).
// A listener URL that parses, whose joined URL, as url.URL.String writes it, url.Parse refuses (an
// IPv6 zone holding a byte that is not ASCII, which String escapes as %FD), is refused the same way
// (<call>-reparsed); its twin shows http.NewRequest's error for the joined URL holds the
// credentials. Captures listenerclient.url/<call>-<case>.
func TestAListenerURLThatDoesNotJoinPrintsNoneOfIt(t *testing.T) {
	param := "access" + "_token"
	userinfo := redacttest.User + ":" + redacttest.Password + "@"
	unparsed := "http://" + userinfo + "127.0.0.1:1/listener%zz?" + param + "=" + redacttest.Token
	reparsed := "http://" + userinfo + "[::%25\xfd]/listener?" + param + "=" + redacttest.Token
	t.Run("twin", func(t *testing.T) {
		if _, err := http.NewRequest(http.MethodGet, unparsed+SessionsRoute, nil); err == nil || len(redacttest.Held(err.Error())) != len(redacttest.Secrets) {
			t.Errorf("http.NewRequest = %v; want an error that quotes the credentials, so the row can see them", err)
		}
	})
	t.Run("reparsed-twin", func(t *testing.T) {
		parsed, err := url.Parse(reparsed)
		if err != nil {
			t.Fatalf("url.Parse refused the listener URL (%v); the row needs one it takes", err)
		}
		if _, err := http.NewRequest(http.MethodGet, parsed.JoinPath(SessionsRoute).String(), nil); err == nil || len(redacttest.Held(err.Error())) != len(redacttest.Secrets) {
			t.Errorf("http.NewRequest of the joined URL = %v; want an error that quotes the credentials, so the row can see them", err)
		}
	})
	calls := []struct {
		name string
		call func(*Client) error
	}{
		{"sessions", func(c *Client) error {
			_, _, err := c.Sessions(context.Background())
			return err
		}},
		{"publish", func(c *Client) error {
			return c.Publish(context.Background(), listenerwire.PublishRequest{Topic: "notifications.team.a"})
		}},
	}
	for _, call := range calls {
		for _, c := range []struct{ name, url, want string }{
			{"unparsed", unparsed, "the listener URL is not an absolute URL with a host (it is not printed)"},
			{"reparsed", reparsed, "the listener URL joined with the route does not parse (it is not printed)"},
		} {
			t.Run(call.name+"-"+c.name, func(t *testing.T) {
				err := call.call(&Client{URL: c.url, Token: "join-bearer", HTTP: &http.Client{}})
				if err == nil {
					t.Fatal("a call to a listener URL that does not join answered no error")
				}
				redacttest.RequireNoPiece(t, "listenerclient.url/"+call.name+"-"+c.name, err.Error())
				if err.Error() != c.want {
					t.Errorf("the call's error = %q; want %q", err, c.want)
				}
			})
		}
	}
}
