package ciphertoken_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAnIntrospectionGoesToTheRouteJoinedOntoTheBrokerURL: the broker URL a verifier is built on
// can carry a path and a query. An introspection asks IntrospectPath joined onto the URL's path,
// the query kept: http://host:port/broker?key=... is asked /broker/v1/oauth/introspect?key=...
// (query), where the route appended to the URL's text sat inside the query
// (/broker?key=.../v1/oauth/introspect: the broker was asked /broker with the query's value
// changed). A URL with no query is asked where it was (no-query). A stand-in records the path and
// the query the introspection reaches it with, and answers the token live, so the verification
// decides its caller.
func TestAnIntrospectionGoesToTheRouteJoinedOntoTheBrokerURL(t *testing.T) {
	const route = "/broker" + ciphertoken.IntrospectPath
	for _, c := range []struct{ name, suffix, query string }{
		{"query", "/broker?key=k3y-7Qz4k", "key=k3y-7Qz4k"},
		{"no-query", "/broker", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			caller := newSigner(t)
			asked := make(chan url.URL, 1)
			broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				asked <- *r.URL
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(liveToken(caller.jkt, time.Hour))
			}))
			t.Cleanup(broker.Close)
			rig := newRig(t, func(o *ciphertoken.Options) { o.BrokerURL = broker.URL + c.suffix })
			decided, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, "a-token-asked-about-at-a-joined-route"))
			select {
			case got := <-asked:
				if got.Path != route || got.RawQuery != c.query {
					t.Errorf("a broker URL ending %q was asked at path %q, query %q; want path %q, query %q", c.suffix, got.Path, got.RawQuery, route, c.query)
				}
			default:
				t.Fatalf("no introspection reached the broker; Verify answered %v", err)
			}
			if err != nil || decided.Session != liveToken(caller.jkt, time.Hour).Session {
				t.Errorf("Verify = %+v, %v; want the caller the stand-in's live answer names", decided, err)
			}
		})
	}
}

// TestAnIntrospectionAtABrokerURLThatDoesNotParsePrintsNoneOfIt: a broker URL url.Parse refuses (an
// invalid escape in its path), with redacttest's credentials in its user information and its
// query. The verification is ErrBrokerUnreachable, and its error, which Dispatch logs, names the
// URL by its class, never url.Parse's error, which quotes it whole (unparsed); the twin shows
// url.Parse's error holds the credentials, so the row can see them printed. Captures
// brokertoken.introspect-url/unparsed.
func TestAnIntrospectionAtABrokerURLThatDoesNotParsePrintsNoneOfIt(t *testing.T) {
	param := "access" + "_token"
	unparsed := "http://" + redacttest.User + ":" + redacttest.Password + "@127.0.0.1:1/broker%zz?" + param + "=" + redacttest.Token
	t.Run("unparsed-twin", func(t *testing.T) {
		if _, err := url.Parse(unparsed); err == nil || len(redacttest.Held(err.Error())) != len(redacttest.Secrets) {
			t.Errorf("url.Parse(%q) = %v; want an error that quotes the credentials, so the row can see them", unparsed, err)
		}
	})
	t.Run("unparsed", func(t *testing.T) {
		rig := newRig(t, func(o *ciphertoken.Options) { o.BrokerURL = unparsed })
		_, err := rig.verifier.Verify(t.Context(), newSigner(t).at(resourcePath, "a-token-asked-about-at-an-unparsed-url"))
		if !errors.Is(err, ciphertoken.ErrBrokerUnreachable) {
			t.Fatalf("an introspection at a broker URL that does not parse answered %v, want ErrBrokerUnreachable", err)
		}
		redacttest.RequireNoPiece(t, "brokertoken.introspect-url/unparsed", err.Error())
		if !strings.HasSuffix(err.Error(), ": not a valid URL") {
			t.Errorf("Verify = %q; want the broker URL refused by its class", err)
		}
	})
}
