package cipher

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestABrokerCallGoesToTheRouteJoinedOntoTheBrokerURL: the broker URL is a setting
// (DISPATCH_AGENT_SECRETS_URL), and it can carry a path and a query. Each call goes to the URL's
// path with its route joined onto it and the URL's query kept ahead of the call's own:
// http://host:port/broker?sig=... lists /broker/v1/pending?sig=...&approver=... (query), where the
// route appended to the URL's text sat inside the query (/broker?sig=.../v1/pending?approver=...,
// the broker asked /broker with the query's value changed). A URL with neither calls where it did
// (no-query). Pending and Grants carry a query of their own, Approve and MachineLookup none; a
// stand-in broker records the path and the query each call reaches it with.
func TestABrokerCallGoesToTheRouteJoinedOntoTheBrokerURL(t *testing.T) {
	for _, c := range []struct{ name, suffix, query string }{
		{"query", "/broker?sig=s1g-7Qz4k", "sig=s1g-7Qz4k"},
		{"no-query", "/broker", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			var mu sync.Mutex
			var asked []url.URL
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				asked = append(asked, *r.URL)
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{}`))
			}))
			t.Cleanup(server.Close)
			client := New(server.URL+c.suffix, tokenFile(t, "account-token"))
			ctx := context.Background()
			for name, call := range map[string]func() (json.RawMessage, error){
				"Pending":       func() (json.RawMessage, error) { return client.Pending(ctx, "sjawhar") },
				"Approve":       func() (json.RawMessage, error) { return client.Approve(ctx, "req-1", Decision{Approver: "sjawhar"}) },
				"Grants":        func() (json.RawMessage, error) { return client.Grants(ctx, "sjawhar") },
				"MachineLookup": func() (json.RawMessage, error) { return client.MachineLookup(ctx, json.RawMessage(`{}`)) },
			} {
				if _, err := call(); err != nil {
					t.Errorf("%s: %v", name, err)
				}
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
			want := map[url.URL]bool{
				{Path: "/broker/v1/pending", RawQuery: joined("approver=sjawhar&state=pending&state=expired")}: true,
				{Path: "/broker/v1/credential-requests/req-1/approve", RawQuery: joined("")}:                   true,
				{Path: "/broker/v1/grants", RawQuery: joined("approver=sjawhar")}:                              true,
				{Path: "/broker/v1/machine-logins/lookup", RawQuery: joined("")}:                               true,
			}
			mu.Lock()
			defer mu.Unlock()
			if len(asked) != len(want) {
				t.Fatalf("a broker URL ending %q was asked %v; want %d calls", c.suffix, asked, len(want))
			}
			for _, got := range asked {
				if !want[url.URL{Path: got.Path, RawQuery: got.RawQuery}] {
					t.Errorf("a broker URL ending %q: a call went to path %q, query %q; want one of %v", c.suffix, got.Path, got.RawQuery, want)
				}
			}
		})
	}
}

// TestABrokerURLThatDoesNotParsePrintsNoneOfIt: a broker URL url.Parse refuses (an invalid escape
// in its path), with redacttest's credentials in its user information and its query, fails each
// call in the client's own words, the call named and the failure's class, with none of the URL
// (unparsed); http.NewRequest's error, which the call's construction returned and which
// ServesDeviceGrant logs, quotes it whole (twin). A broker URL that parses, whose joined URL, as
// url.URL.String writes it, url.Parse refuses (an IPv6 zone holding a byte that is not ASCII,
// which String escapes as %FD), is refused the same way (reparsed); its twin shows
// http.NewRequest's error for the joined URL holds the credentials. Captures
// dispatch-agentsecrets.broker-url/<case>.
func TestABrokerURLThatDoesNotParsePrintsNoneOfIt(t *testing.T) {
	param := "access" + "_token"
	userinfo := redacttest.User + ":" + redacttest.Password + "@"
	unparsed := "http://" + userinfo + "127.0.0.1:1/broker%zz?" + param + "=" + redacttest.Token
	reparsed := "http://" + userinfo + "[::%25\xfd]/broker?" + param + "=" + redacttest.Token
	const wantSaid = "agent-secrets: build GET /v1/pending?approver=sjawhar&state=pending&state=expired request: not a valid URL"
	call := func(t *testing.T, base string) string {
		t.Helper()
		_, err := New(base, tokenFile(t, "account-token")).Pending(context.Background(), "sjawhar")
		if err == nil {
			t.Fatal("Pending called a broker URL that does not parse")
		}
		return err.Error()
	}
	t.Run("twin", func(t *testing.T) {
		if _, err := http.NewRequest(http.MethodGet, unparsed+"/v1/pending", nil); err == nil || len(redacttest.Held(err.Error())) != len(redacttest.Secrets) {
			t.Errorf("http.NewRequest = %v; want an error that quotes the credentials, so the row can see them", err)
		}
	})
	t.Run("unparsed", func(t *testing.T) {
		said := call(t, unparsed)
		redacttest.RequireNoPiece(t, "dispatch-agentsecrets.broker-url/unparsed", said)
		if said != wantSaid {
			t.Errorf("Pending = %q; want %q", said, wantSaid)
		}
	})
	t.Run("reparsed-twin", func(t *testing.T) {
		parsed, err := url.Parse(reparsed)
		if err != nil {
			t.Fatalf("url.Parse refused the broker URL (%v); the row needs one it takes", err)
		}
		if _, err := http.NewRequest(http.MethodGet, parsed.JoinPath("v1/pending").String(), nil); err == nil || len(redacttest.Held(err.Error())) != len(redacttest.Secrets) {
			t.Errorf("http.NewRequest of the joined URL = %v; want an error that quotes the credentials, so the row can see them", err)
		}
	})
	t.Run("reparsed", func(t *testing.T) {
		said := call(t, reparsed)
		redacttest.RequireNoPiece(t, "dispatch-agentsecrets.broker-url/reparsed", said)
		if said != wantSaid {
			t.Errorf("Pending = %q; want %q", said, wantSaid)
		}
	})
}
