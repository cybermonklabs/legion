package envoy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAListenerCallGoesToTheRouteJoinedOntoTheListenerURL: the listener URL is a setting
// (ENVOY_URL), and it can carry a path and a query. Each call goes to the URL's path with its
// route joined onto it and the URL's query kept ahead of the call's own:
// http://host:port/listener?sig=... lists /listener/v1/sessions?sig=...&include=unresponsive
// (query), where the route appended to the URL's text sat inside the query
// (/listener?sig=.../v1/sessions?include=..., the listener asked /listener with the query's value
// changed). A URL with neither calls where it did (no-query). The reads (get) carry a selector of
// their own or none, and a list route ends in a slash; the writes (Unsubscribe, Send) carry none.
// A stand-in listener records the path and the query each call reaches it with.
func TestAListenerCallGoesToTheRouteJoinedOntoTheListenerURL(t *testing.T) {
	for _, c := range []struct{ name, suffix, query string }{
		{"query", "/listener?sig=s1g-7Qz4k", "sig=s1g-7Qz4k"},
		{"no-query", "/listener", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			var mu sync.Mutex
			var asked []url.URL
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				asked = append(asked, *r.URL)
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/v1/legion/agents"):
					_, _ = w.Write([]byte(`{"entries":[]}`))
				case strings.HasSuffix(r.URL.Path, "/v1/interests/unsubscribe"):
					_, _ = w.Write([]byte(`{"removed":[]}`))
				case strings.HasSuffix(r.URL.Path, "/v1/messages/send"):
					_, _ = w.Write([]byte(`{"event_id":"e1","recipient":"s1","state":"connected"}`))
				default:
					_, _ = w.Write([]byte(`[]`))
				}
			}))
			t.Cleanup(server.Close)
			client := New(server.URL+c.suffix, WithToken("listener-token"))
			ctx := context.Background()
			if _, err := client.Sessions(ctx); err != nil {
				t.Errorf("Sessions: %v", err)
			}
			if _, err := client.ListInterests(ctx); err != nil {
				t.Errorf("ListInterests: %v", err)
			}
			if _, err := client.IssueAgents(ctx, "an-issue"); err != nil {
				t.Errorf("IssueAgents: %v", err)
			}
			if _, err := client.Unsubscribe(ctx, "s1", []string{"agent.s1"}); err != nil {
				t.Errorf("Unsubscribe: %v", err)
			}
			if _, err := client.Send(ctx, SendInput{TargetSession: "s1", Message: "m", IdempotencyKey: "k"}); err != nil {
				t.Errorf("Send: %v", err)
			}
			joined := func(own string) string {
				switch {
				case c.query == "":
					return own
				case own == "":
					return c.query
				}
				return c.query + "&" + own
			}
			want := []url.URL{
				{Path: "/listener/v1/sessions", RawQuery: joined("include=unresponsive")},
				{Path: "/listener/v1/interests/", RawQuery: joined("")},
				{Path: "/listener/v1/legion/agents", RawQuery: joined("issue=an-issue")},
				{Path: "/listener/v1/interests/unsubscribe", RawQuery: joined("")},
				{Path: "/listener/v1/messages/send", RawQuery: joined("")},
			}
			mu.Lock()
			defer mu.Unlock()
			if len(asked) != len(want) {
				t.Fatalf("a listener URL ending %q was asked %v; want %v", c.suffix, asked, want)
			}
			for i := range want {
				if asked[i].Path != want[i].Path || asked[i].RawQuery != want[i].RawQuery {
					t.Errorf("a listener URL ending %q: call %d went to path %q, query %q; want path %q, query %q",
						c.suffix, i, asked[i].Path, asked[i].RawQuery, want[i].Path, want[i].RawQuery)
				}
			}
		})
	}
}

// TestARequestWhoseJoinedURLDoesNotParseNamesNoCredential: a listener URL that parses, with the
// probe's user information and a token in its query, whose joined URL, as url.URL.String writes
// it, url.Parse refuses (an IPv6 zone holding a byte that is not ASCII, which String escapes as
// %FD), fails each call before it is sent, and http.NewRequest's error for that URL quotes it
// whole (twin); each call's error names its route and the failure's class, and none of the
// credentials. A listener URL url.Parse refuses outright is
// TestARequestTheClientCannotBuildNamesNoCredentialTheConfiguredURLCarries. Captures
// envoy.build-reparsed-<call>.
func TestARequestWhoseJoinedURLDoesNotParseNamesNoCredential(t *testing.T) {
	param := "access" + "_token"
	reparsed := "http://" + redacttest.User + ":" + redacttest.Password + "@[::%25\xfd]/listener?" + param + "=" + redacttest.Token
	t.Run("twin", func(t *testing.T) {
		parsed, err := url.Parse(reparsed)
		if err != nil {
			t.Fatalf("url.Parse refused the listener URL (%v); the row needs one it takes", err)
		}
		if _, err := http.NewRequest(http.MethodGet, parsed.JoinPath("v1/sessions").String(), nil); err == nil || len(redacttest.Held(err.Error())) != len(redacttest.Secrets) {
			t.Errorf("http.NewRequest of the joined URL = %v; want an error that quotes the credentials, so the rows can see them", err)
		}
	})
	client := New(reparsed)
	_, sessionsErr := client.Sessions(context.Background())
	_, unsubscribeErr := client.Unsubscribe(context.Background(), "session-1", []string{"agent.session-1"})
	_, sendErr := client.Send(context.Background(), SendInput{TargetSession: "session-1", Message: "hello"})
	for _, call := range []struct {
		name, want string
		err        error
	}{
		{"get", "envoy listener unavailable: build GET /v1/sessions request: not a valid URL", sessionsErr},
		{"unsubscribe", "envoy listener unavailable: build POST /v1/interests/unsubscribe request: not a valid URL", unsubscribeErr},
		{"send", "envoy listener unavailable: build POST /v1/messages/send request: not a valid URL", sendErr},
	} {
		t.Run(call.name, func(t *testing.T) {
			if call.err == nil || !errors.Is(call.err, ErrUnavailable) {
				t.Fatalf("the call = %v, want an ErrUnavailable failure", call.err)
			}
			redacttest.RequireNoPiece(t, "envoy.build-reparsed-"+call.name, call.err.Error())
			if call.err.Error() != call.want {
				t.Errorf("the call's error is %q, want %q", call.err, call.want)
			}
		})
	}
}
