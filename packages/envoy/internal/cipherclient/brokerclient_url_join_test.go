package cipherclient

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// joinedTrade is what a stand-in at the broker's address recorded of the trade posted to it: the
// path and the query it was posted at, the user name its Basic credentials carried, and the htu
// its DPoP proof names.
type joinedTrade struct {
	path, query, user, htu string
}

// TestATradeIsPostedToTheTokenPathJoinedOntoTheBrokerAddress: the broker's address can carry a user
// name and a password, a path, a query or a fragment. A trade is posted at the address's path with
// TokenPath joined onto it, the query kept, the user information sent and the fragment dropped,
// and its DPoP proof names that path (htu): http://user:password@host:port/broker?key=... is
// posted at /broker/v1/oauth/token?key=... (query), and so is http://host:port/broker/?key=...
// (query-trailing-slash), where the path appended to the address's text sat inside the query
// (/broker?key=.../v1/oauth/token: the broker was asked /broker with the query's value changed);
// http://host:port/broker#... is posted at /broker/v1/oauth/token (fragment), where the path sat
// inside the fragment. An address with neither is posted where it was (no-query). A stand-in
// records the path, the query, the user name and the proof's htu the trade reaches it with.
func TestATradeIsPostedToTheTokenPathJoinedOntoTheBrokerAddress(t *testing.T) {
	const tokenPath = "/broker" + TokenPath
	for _, c := range []struct{ name, userinfo, suffix, query string }{
		{"query", "demouser7q:demopass9z-not-a-secret@", "/broker?key=k3y-7Qz4k", "key=k3y-7Qz4k"},
		{"query-trailing-slash", "", "/broker/?key=k3y-7Qz4k", "key=k3y-7Qz4k"},
		{"fragment", "", "/broker#pane", ""},
		{"no-query", "", "/broker", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			asked := make(chan joinedTrade, 1)
			base := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
				_, claims, _ := decodeProof(r.Header.Get("DPoP"))
				htu, _ := claims["htu"].(string)
				user, _, _ := r.BasicAuth()
				asked <- joinedTrade{path: r.URL.Path, query: r.URL.RawQuery, user: user, htu: htu}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"access_token":"demo-access-not-a-secret","token_type":"DPoP","expires_in":3600}`))
			})
			address := strings.Replace(base, "http://", "http://"+c.userinfo, 1) + c.suffix
			client, err := New(Config{BrokerURL: cipheraddr.New(address), Key: testKey(t), Subject: projectedStub()})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			token, err := client.Token(context.Background())
			select {
			case got := <-asked:
				if got.path != tokenPath || got.query != c.query {
					t.Errorf("an address ending %q was posted at path %q, query %q; want path %q, query %q", c.suffix, got.path, got.query, tokenPath, c.query)
				}
				if want, _, _ := strings.Cut(c.userinfo, ":"); got.user != want {
					t.Errorf("the trade carried the user name %q; want %q, the address's own", got.user, want)
				}
				if htu, err := url.Parse(got.htu); err != nil || htu.Path != tokenPath || htu.RawQuery != "" || htu.Fragment != "" {
					t.Errorf("the trade's proof names htu %q; want the path it was posted at, %q, with no query or fragment", got.htu, tokenPath)
				}
			default:
				t.Fatalf("no trade reached the broker; Token answered %v", err)
			}
			if err != nil {
				t.Errorf("Token: %v; want the token the stand-in answered", err)
			} else if token.AccessToken != "demo-access-not-a-secret" {
				t.Errorf("Token = %q; want the token the stand-in answered", token.AccessToken)
			}
		})
	}
}

// TestABrokerAddressThatDoesNotParsePrintsNoneOfIt: an address url.Parse refuses (an invalid escape
// in its path), with redacttest's credentials in its user information and its query. The trade is
// refused by its class, "not a valid URL", and neither its error nor the renewal loop's log line
// about it holds any of the credentials (unparsed); the twin shows url.Parse's error for that
// address quotes them, so the row can see them printed. Captures brokerclient.url-join/unparsed.
func TestABrokerAddressThatDoesNotParsePrintsNoneOfIt(t *testing.T) {
	param := "access" + "_token"
	unparsed := "http://" + redacttest.User + ":" + redacttest.Password + "@127.0.0.1:1/broker%zz?" + param + "=" + redacttest.Token
	t.Run("unparsed-twin", func(t *testing.T) {
		if _, err := url.Parse(unparsed); err == nil || len(redacttest.Held(err.Error())) != len(redacttest.Secrets) {
			t.Errorf("url.Parse(%q) = %v; want an error that quotes the credentials, so the row can see them", unparsed, err)
		}
	})
	t.Run("unparsed", func(t *testing.T) {
		logged, tokenErr := tradeOnce(t, unparsed, HTTPClient())
		if tokenErr == nil {
			t.Fatal("Token traded at an address that does not parse")
		}
		redacttest.RequireNoPiece(t, "brokerclient.url-join/unparsed", tokenErr.Error()+"\n"+logged)
		if !strings.HasSuffix(tokenErr.Error(), ": not a valid URL") {
			t.Errorf("Token = %q; want the address refused by its class", tokenErr)
		}
	})
}
